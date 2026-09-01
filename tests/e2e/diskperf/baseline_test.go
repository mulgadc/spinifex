//go:build e2e

package diskperf

import (
	"strings"
	"testing"
)

// The embedded baseline is the gate's reference. A job renamed without its
// baseline key would silently drop to "no committed baseline" and stop gating,
// which is the failure mode this suite exists to prevent elsewhere.
func TestBaselineCoversEveryGateJob(t *testing.T) {
	base, err := loadBaseline()
	if err != nil {
		t.Fatalf("loadBaseline: %v", err)
	}
	for _, j := range gateJobs {
		if _, ok := base.Jobs[j.Name]; !ok {
			t.Errorf("baseline.json has no entry for job %q", j.Name)
		}
	}
	for name := range base.Jobs {
		if !slicesContainsJob(gateJobs, name) {
			t.Errorf("baseline.json carries %q, which no gate job produces", name)
		}
	}
	if strings.TrimSpace(base.Origin) == "" {
		t.Error("baseline.json has no origin — a reference figure with no stated provenance cannot be judged for whether it still applies")
	}
}

func slicesContainsJob(jobs []jobSpec, name string) bool {
	for _, j := range jobs {
		if j.Name == name {
			return true
		}
	}
	return false
}

func TestMetricJudgeBands(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    metric
		want verdict
	}{
		{"throughput on baseline", metric{Name: "write_iops", Got: 1000, Want: 1000, HigherIsBetter: true}, verdictOK},
		{"throughput improved", metric{Name: "write_iops", Got: 2000, Want: 1000, HigherIsBetter: true}, verdictOK},
		{"throughput down 5%", metric{Name: "write_iops", Got: 950, Want: 1000, HigherIsBetter: true}, verdictOK},
		{"throughput down 15%", metric{Name: "write_iops", Got: 850, Want: 1000, HigherIsBetter: true}, verdictWarn},
		{"throughput halved", metric{Name: "write_iops", Got: 500, Want: 1000, HigherIsBetter: true}, verdictFail},

		{"latency on baseline", metric{Name: "write_p99_9_ms", Got: 100, Want: 100}, verdictOK},
		{"latency improved", metric{Name: "write_p99_9_ms", Got: 50, Want: 100}, verdictOK},
		{"latency up 15%", metric{Name: "write_p99_9_ms", Got: 115, Want: 100}, verdictWarn},
		{"latency doubled", metric{Name: "write_p99_9_ms", Got: 200, Want: 100}, verdictFail},

		{"no committed baseline", metric{Name: "write_iops", Got: 900, Want: 0, HigherIsBetter: true}, verdictUnmeasured},
		{"fio reported nothing", metric{Name: "write_iops", Got: 0, Want: 1000, HigherIsBetter: true}, verdictUnmeasured},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := tc.m.judge()
			if got != tc.want {
				t.Errorf("judge() = %v, want %v (%s)", got, tc.want, msg)
			}
		})
	}
}

// The band edges are the contract, so they are pinned rather than left to the
// nearest test case either side of them.
func TestMetricJudgeBandEdges(t *testing.T) {
	exactWarn := metric{Name: "write_iops", Got: 900, Want: 1000, HigherIsBetter: true}
	if v, msg := exactWarn.judge(); v != verdictOK {
		t.Errorf("exactly %.0f%% regression = %v, want OK (the band is exclusive): %s", warnFraction*100, v, msg)
	}
	exactFail := metric{Name: "write_iops", Got: 750, Want: 1000, HigherIsBetter: true}
	if v, msg := exactFail.judge(); v != verdictWarn {
		t.Errorf("exactly %.0f%% regression = %v, want warn (the band is exclusive): %s", failFraction*100, v, msg)
	}
}

// A write-only job reports zeros for the idle read stream. Comparing those
// against a read baseline would report a total regression on every run.
func TestMetricsForSkipsIdleStreams(t *testing.T) {
	base := BaselineJob{ReadIOPS: 5000, WriteIOPS: 9000, ReadP999Ms: 100, WriteP999Ms: 90}
	writeOnly := fioJob{Name: "randwrite-4k"}
	writeOnly.Write.IOBytes = 1 << 30
	writeOnly.Write.IOPS = 9000

	ms := metricsFor(jobSpec{Name: "randwrite-4k"}, writeOnly, base)
	for _, m := range ms {
		if strings.HasPrefix(m.Name, "read_") {
			t.Errorf("write-only job produced read metric %q", m.Name)
		}
	}
	if len(ms) != 2 {
		t.Errorf("write-only job produced %d metrics, want 2", len(ms))
	}

	mixed := writeOnly
	mixed.Read.IOBytes = 1 << 30
	mixed.Read.IOPS = 5000
	if got := len(metricsFor(jobSpec{Name: "randrw-70-30-4k"}, mixed, base)); got != 4 {
		t.Errorf("mixed job produced %d metrics, want 4", got)
	}
}
