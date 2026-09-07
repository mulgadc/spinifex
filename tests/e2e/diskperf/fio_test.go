//go:build e2e

package diskperf

import (
	"strings"
	"testing"
)

// sampleFio is a trimmed fio JSON document with the fields the gate reads.
const sampleFio = `{
  "fio version": "fio-3.36",
  "jobs": [
    {
      "jobname": "randwrite-4k",
      "read":  {"io_bytes": 0, "bw_bytes": 0, "iops": 0.0,
                "clat_ns": {"max": 0, "mean": 0, "percentile": {}}},
      "write": {"io_bytes": 17179869184, "bw_bytes": 38000000, "iops": 9280.5,
                "clat_ns": {"max": 342000000, "mean": 3400000, "percentile": {"99.900000": 93000000}}}
    }
  ]
}`

func TestParseFioReadsNamedJob(t *testing.T) {
	res, err := parseFio("randwrite-4k", sampleFio)
	if err != nil {
		t.Fatalf("parseFio: %v", err)
	}
	if got := res.Aggregate.Write.IOPS; got != 9280.5 {
		t.Errorf("write IOPS = %v, want 9280.5", got)
	}
	if got := res.Aggregate.Write.p999Ms(); got != 93 {
		t.Errorf("write p99.9 = %v ms, want 93", got)
	}
	if got := res.Aggregate.Write.maxMs(); got != 342 {
		t.Errorf("write max = %v ms, want 342", got)
	}
	if res.Report.Version != "fio-3.36" {
		t.Errorf("fio version = %q, want fio-3.36", res.Report.Version)
	}
}

// fio prefixes its JSON with progress output on some builds, so the object is
// located by its opening brace rather than assumed to start at byte zero.
func TestParseFioSkipsLeadingNoise(t *testing.T) {
	if _, err := parseFio("randwrite-4k", "Jobs: 4 (f=4)\n"+sampleFio); err != nil {
		t.Fatalf("parseFio with leading noise: %v", err)
	}
}

// A job that ran but moved nothing must not be reported as a result: its
// numbers describe no workload, and comparing them against a baseline would
// manufacture either a pass or a regression from no evidence.
func TestParseFioRejectsEmptyJob(t *testing.T) {
	empty := strings.ReplaceAll(sampleFio, `"io_bytes": 17179869184`, `"io_bytes": 0`)
	empty = strings.ReplaceAll(empty, `"iops": 9280.5`, `"iops": 0`)
	_, err := parseFio("randwrite-4k", empty)
	if err == nil {
		t.Fatal("parseFio accepted a job that moved no bytes")
	}
	if !strings.Contains(err.Error(), "moved no bytes") {
		t.Errorf("error does not name the cause: %v", err)
	}
}

func TestParseFioRejectsMissingJob(t *testing.T) {
	if _, err := parseFio("randread-4k", sampleFio); err == nil {
		t.Fatal("parseFio accepted output with no matching job name")
	}
}

func TestParseFioRejectsNonJSON(t *testing.T) {
	if _, err := parseFio("randwrite-4k", "fio: engine libaio not loadable"); err == nil {
		t.Fatal("parseFio accepted output containing no JSON object")
	}
}

func TestMedian(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []float64
		want float64
	}{
		{"empty", nil, 0},
		{"single", []float64{7}, 7},
		{"odd is the middle value", []float64{9, 1, 5}, 5},
		{"even averages the two central", []float64{1, 2, 3, 4}, 2.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := median(tc.in); got != tc.want {
				t.Errorf("median(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// median must not reorder its input: callers hold the per-repetition slices and
// an in-place sort would silently reassociate metrics across repetitions.
func TestMedianDoesNotMutateInput(t *testing.T) {
	in := []float64{3, 1, 2}
	median(in)
	if in[0] != 3 || in[1] != 1 || in[2] != 2 {
		t.Errorf("median reordered its input: %v", in)
	}
}

// The device has to be large enough for every job's region, since
// offset_increment gives each job a disjoint slice rather than sharing one.
func TestJobCommandCoversTheWholeWorkingSet(t *testing.T) {
	for _, j := range gateJobs {
		cmd := j.command("vdc", "/tmp/out.json")
		for _, want := range []string{"--filename=/dev/vdc", "--offset_increment=", "--output-format=json"} {
			if !strings.Contains(cmd, want) {
				t.Errorf("job %q command is missing %q:\n%s", j.Name, want, cmd)
			}
		}
		if j.SizeMiB > 0 {
			continue
		}
		if !strings.Contains(cmd, "--direct=1") {
			t.Errorf("job %q is a throughput profile and must bypass the page cache:\n%s", j.Name, cmd)
		}
		if j.workingSetGiB() != j.SizeGiB*j.NumJobs {
			t.Errorf("job %q working set %d does not match %d jobs x %d GiB", j.Name, j.workingSetGiB(), j.NumJobs, j.SizeGiB)
		}
	}
}

// A durability job measures the wrong thing under direct=1 or libaio: the point
// is the cost of making a buffered write durable, which is what etcd pays per
// raft entry. Guards the combination rather than the flag.
func TestFdatasyncJobIsBufferedAndSynchronous(t *testing.T) {
	var found int
	for _, j := range gateJobs {
		if j.Fdatasync == 0 {
			cmd := j.command("vdc", "/tmp/out.json")
			if strings.Contains(cmd, "--fdatasync=") {
				t.Errorf("job %q carries fdatasync without asking for it:\n%s", j.Name, cmd)
			}
			continue
		}
		found++
		cmd := j.command("vdc", "/tmp/out.json")
		for _, want := range []string{"--fdatasync=1", "--ioengine=psync", "--lat_percentiles=1"} {
			if !strings.Contains(cmd, want) {
				t.Errorf("job %q is missing %q:\n%s", j.Name, want, cmd)
			}
		}
		if strings.Contains(cmd, "--direct=1") {
			t.Errorf("job %q uses direct=1, which bypasses the cache the fdatasync exists to flush:\n%s", j.Name, cmd)
		}
		if j.IODepth != 1 {
			t.Errorf("job %q runs at qd%d: a queue hides the per-commit latency being measured", j.Name, j.IODepth)
		}
	}
	if found == 0 {
		t.Error("no fdatasync job in the profile set — durability latency is unmeasured")
	}
}

// The sync distribution is reported apart from the write stream, so a job that
// parses only write clat records the time to reach the page cache and calls it
// durability.
func TestParseFioReadsTheSyncStream(t *testing.T) {
	const withSync = `{"fio version":"fio-3.36","jobs":[{"jobname":"syncwrite-4k",
	 "write":{"io_bytes":67108864,"bw_bytes":1000,"iops":250,"clat_ns":{"max":900,"mean":400,"percentile":{"99.900000":800}}},
	 "sync":{"total_ios":16384,"lat_ns":{"max":41000000,"mean":5100000,"percentile":{"99.900000":38000000}}}}]}`

	res, err := parseFio("syncwrite-4k", withSync)
	if err != nil {
		t.Fatalf("parseFio: %v", err)
	}
	if got := res.Aggregate.Sync.TotalIOs; got != 16384 {
		t.Errorf("sync total_ios = %d, want 16384", got)
	}
	if got := res.Aggregate.Sync.p999Ms(); got != 38 {
		t.Errorf("sync p99.9 = %.1fms, want 38.0", got)
	}
	if got := res.Aggregate.Sync.maxMs(); got != 41 {
		t.Errorf("sync max = %.1fms, want 41.0", got)
	}
}

// An fdatasync result must be gated on the sync distribution. Judging it on
// write IOPS would pass a disk that buffers everything and commits slowly,
// which is the exact disk etcd cannot run on.
func TestMetricsForGatesTheSyncDistribution(t *testing.T) {
	agg := fioJob{
		Name:  "syncwrite-4k",
		Write: fioStream{IOBytes: 67108864, IOPS: 250},
		Sync:  fioSync{TotalIOs: 16384, Lat: fioClat{Percentile: map[string]int64{p999Key: 38000000}}},
	}
	ms := metricsFor(jobSpec{Name: "syncwrite-4k", Fdatasync: 1}, agg, BaselineJob{SyncP999Ms: 10})

	var got *metric
	for i := range ms {
		if ms[i].Name == "fdatasync_p99_9_ms" {
			got = &ms[i]
		}
	}
	if got == nil {
		t.Fatalf("no fdatasync metric produced from a job with %d syncs", agg.Sync.TotalIOs)
	}
	if got.HigherIsBetter {
		t.Error("fdatasync latency was scored as higher-is-better")
	}
	if v, _ := got.judge(); v != verdictFail {
		t.Errorf("38ms against a 10ms baseline judged %v, want fail", v)
	}
}

// randrw needs a mix and the single-direction profiles must not carry one,
// since fio ignores rwmixread outside randrw and its presence would suggest a
// mix that is not being run.
func TestJobCommandSetsMixOnlyForRandrw(t *testing.T) {
	for _, j := range gateJobs {
		cmd := j.command("vdc", "/tmp/out.json")
		hasMix := strings.Contains(cmd, "--rwmixread=")
		if want := j.RW == "randrw"; hasMix != want {
			t.Errorf("job %q rw=%s: rwmixread present=%v, want %v", j.Name, j.RW, hasMix, want)
		}
	}
}
