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
		for _, want := range []string{"--direct=1", "--filename=/dev/vdc", "--offset_increment=", "--output-format=json"} {
			if !strings.Contains(cmd, want) {
				t.Errorf("job %q command is missing %q:\n%s", j.Name, want, cmd)
			}
		}
		if j.workingSetGiB() != j.SizeGiB*j.NumJobs {
			t.Errorf("job %q working set %d does not match %d jobs x %d GiB", j.Name, j.workingSetGiB(), j.NumJobs, j.SizeGiB)
		}
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
