//go:build e2e

package diskperf

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// Availability bounds. These are ceilings on a responsiveness probe, not
// latency targets: a healthy probe answers in milliseconds and a starved one
// never answers at all, so the exact value between those two matters little and
// the bound is set to leave the healthy case orders of magnitude of headroom.
const (
	// guestProbeBound covers a fresh SSH handshake plus a stat, which costs a
	// few hundred milliseconds on an idle guest.
	guestProbeBound = 15 * time.Second
	// hostProbeBound covers a TCP connect and an SSH banner read on the host,
	// which is sub-millisecond on the local network.
	hostProbeBound = 5 * time.Second

	// probeInterval is the gap between successive probes. Frequent enough to
	// catch a stall that resolves inside a minute, sparse enough that the
	// probes themselves are not part of the workload.
	probeInterval = 5 * time.Second
)

// probe is one repeated responsiveness check running alongside a workload.
type probe struct {
	// Name identifies the probe in failure output.
	Name string
	// Bound is the ceiling a single attempt must answer within.
	Bound time.Duration
	// Attempt performs one check. It must return promptly on failure rather
	// than block, since exceeding Bound is itself the signal.
	Attempt func() error
}

// probeReport is what one probe observed across a workload.
type probeReport struct {
	Name     string
	Samples  int
	Failures int
	Max      time.Duration
	// FirstFailure is the earliest breach, kept because the first one carries
	// the cause and later ones are usually consequences of it.
	FirstFailure error
	// FirstFailureAt is measured from the start of the watch.
	FirstFailureAt time.Duration
}

// ok reports whether every sample answered inside the bound.
func (r probeReport) ok() bool { return r.Failures == 0 && r.Samples > 0 }

// summary renders the report for test output.
func (r probeReport) summary() string {
	return fmt.Sprintf("%s: %d samples, %d over bound, max %s", r.Name, r.Samples, r.Failures, r.Max.Round(time.Millisecond))
}

// probeRecords renders the reports for the result artifact. FirstFailure is an
// error and does not marshal, so it is flattened to its message.
func probeRecords(reports []probeReport) []map[string]any {
	out := make([]map[string]any, 0, len(reports))
	for _, r := range reports {
		rec := map[string]any{
			"name":       r.Name,
			"samples":    r.Samples,
			"over_bound": r.Failures,
			"max_ms":     r.Max.Milliseconds(),
		}
		if r.FirstFailure != nil {
			rec["first_failure"] = r.FirstFailure.Error()
			rec["first_failure_at_s"] = int(r.FirstFailureAt.Seconds())
		}
		out = append(out, rec)
	}
	return out
}

// watch runs every probe on its own goroutine until stop is closed, and
// returns what each observed. Probes run concurrently with each other and with
// the workload; each holds its own connection, so one blocking does not
// serialise the rest.
func watch(probes []probe, stop <-chan struct{}) []probeReport {
	reports := make([]probeReport, len(probes))
	var wg sync.WaitGroup
	origin := time.Now()

	for i, p := range probes {
		wg.Add(1)
		go func(i int, p probe) {
			defer wg.Done()
			rep := probeReport{Name: p.Name}
			for {
				select {
				case <-stop:
					reports[i] = rep
					return
				default:
				}

				started := time.Now()
				err := p.Attempt()
				elapsed := time.Since(started)
				rep.Samples++
				if elapsed > rep.Max {
					rep.Max = elapsed
				}
				if err != nil || elapsed > p.Bound {
					rep.Failures++
					if rep.FirstFailure == nil {
						rep.FirstFailure = failureCause(err, elapsed, p.Bound)
						rep.FirstFailureAt = started.Sub(origin)
					}
				}

				select {
				case <-stop:
					reports[i] = rep
					return
				case <-time.After(probeInterval):
				}
			}
		}(i, p)
	}
	wg.Wait()
	return reports
}

// failureCause distinguishes a probe that answered too late from one that did
// not answer at all. The two have different causes and the message has to say
// which was seen.
func failureCause(err error, elapsed, bound time.Duration) error {
	if err != nil {
		return fmt.Errorf("failed after %s: %w", elapsed.Round(time.Millisecond), err)
	}
	return fmt.Errorf("answered in %s, over the %s bound", elapsed.Round(time.Millisecond), bound)
}

// guestProbe checks that the guest still services ordinary I/O on its root
// volume while the workload runs on a different volume. Reading the root
// deliberately targets a volume the job never touches, so a breach is evidence
// that one volume's workload degrades another rather than merely that the job
// is slow.
//
// The command is a directory read and a stat of the root filesystem: the
// original symptom was `ls` and tab-completion freezing outright, not a
// measurable slowdown.
func guestProbe(tgt harness.SSHTarget) probe {
	return probe{
		Name:  "guest responsiveness (root volume)",
		Bound: guestProbeBound,
		Attempt: func() error {
			out, err := harness.GuestExecTimeout(tgt, "stat -f / >/dev/null && ls -1 /usr/bin >/dev/null", guestProbeBound)
			if err != nil {
				return fmt.Errorf("%w\n%s", err, out)
			}
			return nil
		},
	}
}

// hostProbe checks that the host's own sshd still accepts a connection and
// speaks first. The original blocker escaped the guest and made SSH to the host
// fail, so this is the assertion that the blast radius stays inside the tenant.
//
// A banner read rather than a full authentication: it needs no credential, and
// receiving the banner already proves the kernel completed a handshake and
// scheduled sshd, which is exactly what a starved host cannot do.
func hostProbe(addr string) probe {
	return probe{
		Name:  "host sshd (" + addr + ")",
		Bound: hostProbeBound,
		Attempt: func() error {
			return readSSHBanner(addr, hostProbeBound)
		},
	}
}

// readSSHBanner dials addr and reads the SSH identification string the server
// sends on connect.
func readSSHBanner(addr string, timeout time.Duration) error {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("set deadline: %w", err)
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		return fmt.Errorf("read banner: %w", err)
	}
	if n < 4 || string(buf[:4]) != "SSH-" {
		return errors.New("peer sent no SSH identification string")
	}
	return nil
}
