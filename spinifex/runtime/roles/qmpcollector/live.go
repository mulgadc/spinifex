package qmpcollector

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	telemetryv1 "github.com/mulgadc/spinifex/contracts/telemetry/v1"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/qmp"
	"github.com/nats-io/nats.go"
)

const (
	livePeriod = telemetryv1.LiveSamplePeriodMillis * time.Millisecond
	liveLease  = telemetryv1.LiveWatchLeaseMillis * time.Millisecond
	// vCPU threads live as long as QEMU, so QMP is asked again only this often.
	liveThreadRefresh = 10 * time.Second
)

// liveCounters is one raw snapshot for the live view. CPU comes from
// schedstat's nanoseconds, since 10 ms jiffies would step a 500 ms sample.
type liveCounters struct {
	at        time.Time
	cpuNanos  uint64
	rxBytes   uint64 // tap rx = guest egress
	txBytes   uint64 // tap tx = guest ingress
	rxPackets uint64
	txPackets uint64
}

// liveSampler samples one instance every livePeriod while somebody is
// watching, and only then. Its ring is memory, dropped when the lease lapses.
type liveSampler struct {
	cfg       *Config
	nc        *nats.Conn
	meta      func() telemetryv1.GuestTelemetryMeta
	threadIDs func(telemetryv1.GuestTelemetryMeta) ([]int, error)
	now       func() time.Time

	mu      sync.Mutex
	ctx     context.Context
	until   time.Time
	running bool
	ring    []telemetryv1.LiveSample
}

func newLiveSampler(cfg *Config, nc *nats.Conn, meta func() telemetryv1.GuestTelemetryMeta) *liveSampler {
	return &liveSampler{cfg: cfg, nc: nc, meta: meta, threadIDs: qmpThreadIDs, now: time.Now}
}

// subscribe listens for watches on this instance's account-scoped subject.
// A malformed account or instance ID has no live view rather than a subject
// that could match another one.
func (l *liveSampler) subscribe(ctx context.Context) (*nats.Subscription, error) {
	meta := l.meta()
	if !telemetryv1.ValidLiveTarget(meta.AccountID, meta.InstanceID) {
		return nil, fmt.Errorf("no live view for account %q instance %q", meta.AccountID, meta.InstanceID)
	}
	l.mu.Lock()
	l.ctx = ctx
	l.mu.Unlock()
	return l.nc.Subscribe(telemetryv1.LiveWatchSubject(meta.AccountID, meta.InstanceID), l.handleWatch)
}

func (l *liveSampler) handleWatch(msg *nats.Msg) {
	var watch telemetryv1.LiveWatch
	if len(msg.Data) > 0 {
		if err := json.Unmarshal(msg.Data, &watch); err != nil {
			slog.Debug("qmp-collector: malformed live watch", "subject", msg.Subject, "err", err)
			return
		}
	}

	ring := l.watch(watch.SinceMillis)
	if msg.Reply == "" {
		return
	}
	data, err := json.Marshal(telemetryv1.LiveRing{Samples: ring})
	if err != nil {
		slog.Error("qmp-collector: marshal live ring", "err", err)
		return
	}
	if err := msg.Respond(data); err != nil {
		slog.Debug("qmp-collector: live ring reply failed", "err", err)
	}
}

// watch extends the lease, starts the sampler if it was idle, and returns the
// samples newer than sinceMillis.
func (l *liveSampler) watch(sinceMillis int64) []telemetryv1.LiveSample {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.until = l.now().Add(liveLease)
	if !l.running && l.ctx != nil {
		l.running = true
		go l.run(l.ctx)
	}

	samples := make([]telemetryv1.LiveSample, 0, len(l.ring))
	for _, s := range l.ring {
		if s.TimeMillis > sinceMillis {
			samples = append(samples, s)
		}
	}
	return samples
}

// lapsed reports whether the lease ran out, and if so stops the sampler and
// drops its ring under the same lock a watch takes to restart it.
func (l *liveSampler) lapsed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.now().Before(l.until) {
		return false
	}
	l.running = false
	l.ring = nil
	return true
}

func (l *liveSampler) record(s telemetryv1.LiveSample) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.ring) == telemetryv1.LiveRingSamples {
		l.ring = append(l.ring[:0], l.ring[1:]...)
	}
	l.ring = append(l.ring, s)
}

func (l *liveSampler) stopped() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.running = false
	l.ring = nil
}

func (l *liveSampler) run(ctx context.Context) {
	ticker := time.NewTicker(livePeriod)
	defer ticker.Stop()

	var (
		threads   []int
		threadsAt time.Time
		prev      *liveCounters
	)
	for {
		select {
		case <-ctx.Done():
			l.stopped()
			return
		case <-ticker.C:
		}
		if l.lapsed() {
			return
		}

		meta := l.meta()
		if threads == nil || l.now().Sub(threadsAt) >= liveThreadRefresh {
			ids, err := l.threadIDs(meta)
			if err != nil {
				slog.Debug("qmp-collector: live thread lookup failed", "instanceId", meta.InstanceID, "err", err)
				threads, prev = nil, nil
				continue
			}
			threads, threadsAt = ids, l.now()
		}

		cur, err := readLiveCounters(l.cfg, threads, meta.Taps, l.now())
		if err != nil {
			slog.Debug("qmp-collector: live sample failed", "instanceId", meta.InstanceID, "err", err)
			threads, prev = nil, nil
			continue
		}
		if prev != nil {
			if s, ok := liveRates(meta.VCPUs, prev, cur); ok {
				l.record(s)
				l.publish(meta, s)
			}
		}
		prev = cur
	}
}

func (l *liveSampler) publish(meta telemetryv1.GuestTelemetryMeta, s telemetryv1.LiveSample) {
	data, err := json.Marshal(s)
	if err != nil {
		slog.Error("qmp-collector: marshal live sample", "err", err)
		return
	}
	if err := l.nc.Publish(telemetryv1.LiveSampleSubject(meta.AccountID, meta.InstanceID), data); err != nil {
		slog.Debug("qmp-collector: live publish failed", "instanceId", meta.InstanceID, "err", err)
	}
}

// qmpThreadIDs asks the telemetry socket which host thread runs each vCPU.
func qmpThreadIDs(meta telemetryv1.GuestTelemetryMeta) ([]int, error) {
	client, err := qmp.NewQMPClient(meta.Socket)
	if err != nil {
		return nil, fmt.Errorf("dial telemetry socket: %w", err)
	}
	defer func() { _ = client.Close() }()

	if err := client.Execute(qmp.QMPCommand{Execute: "qmp_capabilities"}, nil); err != nil {
		return nil, fmt.Errorf("qmp_capabilities: %w", err)
	}
	var cpus []qmp.CPUInfoFast
	if err := client.Execute(qmp.QMPCommand{Execute: "query-cpus-fast"}, &cpus); err != nil {
		return nil, fmt.Errorf("query-cpus-fast: %w", err)
	}
	ids := make([]int, 0, len(cpus))
	for _, cpu := range cpus {
		ids = append(ids, cpu.ThreadID)
	}
	return ids, nil
}

// readLiveCounters reads the vCPU threads' run time and the taps' counters. A
// tap that vanished mid-read is skipped, as the periodic tick does.
func readLiveCounters(cfg *Config, threads []int, taps []string, at time.Time) (*liveCounters, error) {
	c := &liveCounters{at: at}
	for _, tid := range threads {
		ns, err := threadRunNanos(cfg.ProcRoot, tid)
		if err != nil {
			return nil, err
		}
		c.cpuNanos += ns
	}
	for _, tap := range taps {
		var values [4]uint64
		ok := true
		for i, name := range []string{"rx_bytes", "tx_bytes", "rx_packets", "tx_packets"} {
			v, err := readSysfsCounter(cfg.SysRoot, tap, name)
			if err != nil {
				ok = false
				break
			}
			values[i] = v
		}
		if !ok {
			continue
		}
		c.rxBytes += values[0]
		c.txBytes += values[1]
		c.rxPackets += values[2]
		c.txPackets += values[3]
	}
	return c, nil
}

// threadRunNanos returns the first field of /proc/<tid>/schedstat: the time
// the thread has spent on a CPU, in nanoseconds.
func threadRunNanos(procRoot string, tid int) (uint64, error) {
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(tid), "schedstat"))
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, fmt.Errorf("empty schedstat for tid %d", tid)
	}
	ns, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse schedstat for tid %d: %w", tid, err)
	}
	return ns, nil
}

// liveRates turns two snapshots into per-second rates over the time that
// actually elapsed. ok is false when a counter went backwards (QEMU restarted,
// a tap was unplugged), and the caller re-primes from the new snapshot.
func liveRates(vcpus int, prev, cur *liveCounters) (telemetryv1.LiveSample, bool) {
	elapsed := cur.at.Sub(prev.at).Seconds()
	if elapsed <= 0 ||
		cur.cpuNanos < prev.cpuNanos ||
		cur.rxBytes < prev.rxBytes || cur.txBytes < prev.txBytes ||
		cur.rxPackets < prev.rxPackets || cur.txPackets < prev.txPackets {
		return telemetryv1.LiveSample{}, false
	}

	cpu := float64(cur.cpuNanos-prev.cpuNanos) / 1e9 / elapsed / float64(max(vcpus, 1)) * 100
	return telemetryv1.LiveSample{
		TimeMillis: cur.at.UnixMilli(),
		CPUPercent: roundTo(min(cpu, 100), 2),
		NetworkIn:  roundTo(float64(cur.txBytes-prev.txBytes)/elapsed, 0),
		NetworkOut: roundTo(float64(cur.rxBytes-prev.rxBytes)/elapsed, 0),
		PacketsIn:  roundTo(float64(cur.txPackets-prev.txPackets)/elapsed, 2),
		PacketsOut: roundTo(float64(cur.rxPackets-prev.rxPackets)/elapsed, 2),
	}, true
}

// roundTo keeps a rate to the precision a chart can show, which also keeps
// each streamed sample short.
func roundTo(v float64, places int) float64 {
	scale := math.Pow10(places)
	return math.Round(v*scale) / scale
}
