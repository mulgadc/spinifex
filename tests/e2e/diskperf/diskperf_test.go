//go:build e2e

package diskperf

import (
	"encoding/json"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

const (
	// diskperfDevice is the guest-visible attach point requested for the volume
	// under test.
	diskperfDevice = "/dev/sdp"

	// volumeHeadroomGiB pads the volume past the working set so fio's last job
	// is not writing into the final block of the device.
	volumeHeadroomGiB = 4

	// volumeReadyTimeout bounds the wait for a hotplugged volume to reach the
	// guest kernel.
	volumeReadyTimeout = 90 * time.Second
)

// TestGuestDiskPerformance is the gate. It drives each pinned fio profile
// against its own freshly created raw volume and asserts two separable classes
// of property: that the guest and the host stayed responsive throughout, hard
// pass/fail; and that throughput and latency have not regressed past the band,
// against a committed baseline.
//
// The two are kept apart deliberately. The availability class is what would
// have caught the original blocker, and it would have caught it on the first
// run, because its thresholds are absolute rather than relative to a number
// somebody measured on a good day. Folding them together would let a run that
// left the host unreachable pass on the strength of a respectable IOPS figure.
func TestGuestDiskPerformance(t *testing.T) {
	fix := requireDiskPerfFixture(t)
	harness.Phase(t, "Guest Disk Performance")

	base, err := loadBaseline()
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	reps := envInt(t, "SPINIFEX_DISKPERF_REPS", 1)

	instanceID, tgt := ensureDiskPerfInstance(t, fix)
	az := harness.DiscoverDefaultAZ(t, fix.Harness)
	harness.Detail(t, "instance", instanceID, "az", az, "reps", reps, "baseline_origin", base.Origin)

	if err := installFio(tgt); err != nil {
		t.Fatalf("%v", err)
	}

	hostAddr := hostSSHAddr(fix.Env)
	if err := readSSHBanner(hostAddr, hostProbeBound); err != nil {
		// Establish the probe works before the workload starts. Skipping the
		// host assertion loudly is honest; asserting on a channel that was
		// already broken would report the harness as a storage regression.
		t.Errorf("host sshd probe is unusable before any load (%s): %v — the host-responsiveness assertion cannot run", hostAddr, err)
		hostAddr = ""
	}

	for _, job := range gateJobs {
		t.Run(job.Name, func(t *testing.T) {
			runJob(t, fix, job, base.Jobs[job.Name], instanceID, tgt, az, hostAddr, reps)
		})
	}
}

// runJob executes one profile reps times, each against a volume created for
// that repetition. The fresh volume is the reset: the extent index grows
// monotonically, so a reused volume makes each repetition scan a larger index
// than the last and drifts the gate's own numbers upward over time.
func runJob(t *testing.T, fix *Fixture, job jobSpec, base BaselineJob, instanceID string, tgt harness.SSHTarget, az, hostAddr string, reps int) {
	t.Helper()
	sizeGiB := int64(job.workingSetGiB() + volumeHeadroomGiB)

	var results []runResult
	var probes []probeReport
	for rep := 1; rep <= reps; rep++ {
		harness.Step(t, "rep %d/%d: %d jobs x %d GiB %s %s at qd%d on a fresh %d GiB volume",
			rep, reps, job.NumJobs, job.SizeGiB, job.BlockSize, job.RW, job.IODepth, sizeGiB)

		volID := createVolume(t, fix, az, sizeGiB)
		before := harness.GuestDiskSet(t, tgt)
		harness.AttachVolumeWait(t, fix.AWS, volID, instanceID, diskperfDevice)
		dev := harness.WaitForNewGuestDisk(t, tgt, before, volumeReadyTimeout)

		res, reports := driveWithProbes(t, job, tgt, dev, hostAddr)
		harness.DetachVolumeWait(t, fix.AWS, volID)

		assertAvailability(t, job, reports)
		results = append(results, res)
		probes = append(probes, reports...)
	}

	report := aggregate(t, job, base, results, probes)
	harness.DumpFile(t, resultsDir(t, fix), job.Name+"-result.json", report)
}

// resultsDir is the run's top-level artifact directory rather than the
// per-test one. The per-test directory deletes itself when the test passes,
// which is right for failure diagnostics and wrong for these: a passing run is
// exactly the run whose numbers a baseline capture needs to read.
func resultsDir(t *testing.T, fix *Fixture) string {
	t.Helper()
	dir := fix.Env.ArtifactDir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("results dir %s: %v", dir, err)
	}
	return dir
}

// driveWithProbes runs the job while the responsiveness probes watch. The
// probes only bracket the availability job: they cost a connection every few
// seconds, which is negligible against the workload but is still load, and it
// has no place in a run whose numbers feed a baseline.
func driveWithProbes(t *testing.T, job jobSpec, tgt harness.SSHTarget, dev, hostAddr string) (runResult, []probeReport) {
	t.Helper()
	if !job.Availability {
		res, err := runFio(tgt, job, dev)
		if err != nil {
			t.Fatalf("%v", err)
		}
		return res, nil
	}

	probes := []probe{guestProbe(tgt)}
	if hostAddr != "" {
		probes = append(probes, hostProbe(hostAddr))
	}

	stop := make(chan struct{})
	done := make(chan []probeReport, 1)
	go func() { done <- watch(probes, stop) }()

	res, err := runFio(tgt, job, dev)
	close(stop)
	reports := <-done

	if err != nil {
		// A job that did not finish inside its budget is the stall itself, so
		// what the probes saw while it ran is the diagnosis and has to be
		// printed alongside the failure rather than dropped with the run.
		for _, r := range reports {
			t.Logf("  %s", r.summary())
			if r.FirstFailure != nil {
				t.Logf("    first breach at +%s: %v", r.FirstFailureAt.Round(time.Second), r.FirstFailure)
			}
		}
		t.Fatalf("%v", err)
	}
	return res, reports
}

// assertAvailability turns the probe reports into hard pass/fail. Zero
// tolerance: one breach means the guest or the host stopped answering while a
// single tenant ran an ordinary workload.
func assertAvailability(t *testing.T, job jobSpec, reports []probeReport) {
	t.Helper()
	if !job.Availability {
		return
	}
	if len(reports) == 0 {
		t.Fatal("availability job ran with no probes — nothing was asserted")
	}
	for _, r := range reports {
		harness.Detail(t, "probe", r.Name, "samples", r.Samples, "over_bound", r.Failures, "max_ms", r.Max.Milliseconds())
		if r.Samples == 0 {
			t.Errorf("%s: no samples taken — the probe never ran, so it proved nothing", r.Name)
			continue
		}
		if r.ok() {
			continue
		}
		t.Errorf("%s: %d of %d samples breached the bound, first at +%s: %v",
			r.Name, r.Failures, r.Samples, r.FirstFailureAt.Round(time.Second), r.FirstFailure)
	}
}

// aggregate compares the run against the baseline and returns the recorded
// result for the artifact bundle. Every metric is printed whatever its verdict,
// so a passing run still leaves the numbers behind for the next baseline
// capture to be judged against.
func aggregate(t *testing.T, job jobSpec, base BaselineJob, results []runResult, probes []probeReport) []byte {
	t.Helper()
	if len(results) == 0 {
		t.Fatalf("job %q produced no results", job.Name)
	}

	agg := medianAggregate(results)
	recorded := map[string]any{
		"job":          job.Name,
		"reps":         len(results),
		"fio_version":  results[0].Report.Version,
		"read_iops":    agg.Read.IOPS,
		"write_iops":   agg.Write.IOPS,
		"read_p99_9":   agg.Read.p999Ms(),
		"write_p99_9":  agg.Write.p999Ms(),
		"read_max_ms":  agg.Read.maxMs(),
		"write_max_ms": agg.Write.maxMs(),
		// The availability evidence belongs in the same record as the numbers.
		// A throughput figure taken from a run that left the host unreachable
		// is not a throughput figure worth keeping.
		"probes": probeRecords(probes),
	}

	for _, m := range metricsFor(job, agg, base) {
		v, msg := m.judge()
		switch v {
		case verdictFail:
			t.Errorf("REGRESSION %s", msg)
		case verdictWarn:
			t.Logf("  warn: %s", msg)
		default:
			t.Logf("  %s", msg)
		}
	}

	blob, err := json.MarshalIndent(recorded, "", "  ")
	if err != nil {
		t.Fatalf("encoding result: %v", err)
	}
	return blob
}

// medianAggregate reduces the repetitions to one aggregate by taking the median
// of each metric independently. Per-metric rather than picking a single
// representative run: the median run by IOPS is not necessarily the median run
// by tail latency, and the tail is the number this gate cares most about.
func medianAggregate(results []runResult) fioJob {
	pick := func(f func(fioJob) float64) float64 {
		xs := make([]float64, 0, len(results))
		for _, r := range results {
			xs = append(xs, f(r.Aggregate))
		}
		return median(xs)
	}

	var out fioJob
	out.Name = results[0].Job
	out.Read.IOPS = pick(func(j fioJob) float64 { return j.Read.IOPS })
	out.Write.IOPS = pick(func(j fioJob) float64 { return j.Write.IOPS })
	out.Read.Clat.Percentile = map[string]int64{p999Key: int64(pick(func(j fioJob) float64 { return float64(j.Read.Clat.Percentile[p999Key]) }))}
	out.Write.Clat.Percentile = map[string]int64{p999Key: int64(pick(func(j fioJob) float64 { return float64(j.Write.Clat.Percentile[p999Key]) }))}
	out.Read.Clat.Max = int64(pick(func(j fioJob) float64 { return float64(j.Read.Clat.Max) }))
	out.Write.Clat.Max = int64(pick(func(j fioJob) float64 { return float64(j.Write.Clat.Max) }))

	// Carry the byte counts so metricsFor can still tell which streams the job
	// actually exercised.
	for _, r := range results {
		out.Read.IOBytes += r.Aggregate.Read.IOBytes
		out.Write.IOBytes += r.Aggregate.Write.IOBytes
	}
	return out
}

// createVolume makes a volume for a single repetition and registers its
// teardown. The harness Ensure* helper memoizes on (az, size) and would hand
// back the same volume every time, which is the opposite of the reset this
// needs.
func createVolume(t *testing.T, fix *Fixture, az string, sizeGiB int64) string {
	t.Helper()
	// e2e:allow-create — a fresh volume per repetition is the reset this gate depends on.
	out, err := fix.AWS.EC2.CreateVolume(&ec2.CreateVolumeInput{
		AvailabilityZone: aws.String(az),
		Size:             aws.Int64(sizeGiB),
	})
	if err != nil {
		t.Fatalf("CreateVolume(%d GiB in %s): %v", sizeGiB, az, err)
	}
	volID := aws.StringValue(out.VolumeId)
	harness.RegisterVolumeTeardown(t, fix.AWS, volID)
	harness.WaitForVolumeState(t, fix.AWS, volID, "available")
	return volID
}

// ensureDiskPerfInstance launches (or returns the memoized) guest and waits for
// SSH.
func ensureDiskPerfInstance(t *testing.T, fix *Fixture) (instanceID string, tgt harness.SSHTarget) {
	t.Helper()
	instType, arch := harness.DiscoverNanoInstanceType(t, fix.Harness)
	ami := harness.DiscoverUbuntuAMI(t, fix.Harness, arch)
	keyName, keyPath := harness.EnsureKeyPair(t, fix.Harness, fix.ArtifactDir(t))
	vpc := harness.EnsureDefaultVPC(t, fix.Harness)
	harness.AuthorizeSSHIngress(t, fix.AWS, vpc.SGID)

	instanceID = harness.EnsureInstance(t, fix.Harness, harness.InstanceSpec{
		AMIID:        ami,
		InstanceType: instType,
		KeyName:      keyName,
		SubnetID:     vpc.SubnetID,
		SGID:         vpc.SGID,
	})
	inst := harness.WaitForInstanceState(t, fix.AWS, instanceID, "running")
	host, port := harness.InstancePublicSSHHost(t, inst)

	harness.Step(t, "waiting for guest SSH on %s:%d (instance_type=%s)", host, port, instType)
	if !harness.TryGuestSSHReady(host, port, "ubuntu", keyPath, 5*time.Minute) {
		t.Fatalf("guest %s SSH %s:%d not ready after 5m", instanceID, host, port)
	}
	return instanceID, harness.SSHTarget{User: "ubuntu", Host: host, Port: port, KeyPath: keyPath}
}

// hostSSHAddr is the address the host-responsiveness probe dials. WANHost is
// the externally reachable node address, which is what an operator locked out
// by the original defect would have been trying to reach.
func hostSSHAddr(env *harness.Env) string {
	return net.JoinHostPort(env.WANHost, "22")
}

// envInt reads a positive integer override, failing outright on a malformed
// value rather than silently falling back to the default.
func envInt(t *testing.T, key string, def int) int {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		t.Fatalf("%s=%q is not a positive integer", key, v)
	}
	return n
}
