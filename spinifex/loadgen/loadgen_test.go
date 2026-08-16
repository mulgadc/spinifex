package loadgen_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/mulgadc/spinifex/spinifex/loadgen"
)

// fixedOp answers after a fixed delay, so a stage's percentiles are known
// before it runs.
func fixedOp(name string, kind loadgen.Kind, latency time.Duration, err error) loadgen.Op {
	return loadgen.Op{Name: name, Kind: kind, Call: func(ctx context.Context, _ *loadgen.Target) error {
		select {
		case <-time.After(latency):
		case <-ctx.Done():
		}
		return err
	}}
}

func targets(n int) []*loadgen.Target {
	out := make([]*loadgen.Target, 0, n)
	for i := range n {
		out = append(out, &loadgen.Target{Account: string(rune('a' + i))})
	}
	return out
}

func TestClosedLoopHoldsTheRequestedConcurrency(t *testing.T) {
	t.Parallel()

	var inFlight, peak int64
	op := loadgen.Op{Name: "Probe", Kind: loadgen.KindRead,
		Call: func(ctx context.Context, _ *loadgen.Target) error {
			current := atomic.AddInt64(&inFlight, 1)
			defer atomic.AddInt64(&inFlight, -1)
			for {
				observed := atomic.LoadInt64(&peak)
				if current <= observed || atomic.CompareAndSwapInt64(&peak, observed, current) {
					break
				}
			}
			select {
			case <-time.After(5 * time.Millisecond):
			case <-ctx.Done():
			}
			return nil
		}}

	stages := []loadgen.Stage{{Mode: loadgen.ModeClosed, Concurrency: 4, Duration: 150 * time.Millisecond}}
	report, err := loadgen.Run(context.Background(), targets(2), []loadgen.Op{op}, stages, loadgen.SLO{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := atomic.LoadInt64(&peak); got != 4 {
		t.Errorf("peak concurrency = %d, want exactly the 4 requested", got)
	}
	if count := report.Stages[0].Ops["Probe"].Count; count < 4 {
		t.Errorf("count = %d, want at least one request per worker", count)
	}
}

func TestOpenLoopIssuesRoughlyTheRequestedRate(t *testing.T) {
	t.Parallel()

	stages := []loadgen.Stage{{Mode: loadgen.ModeOpen, RPS: 100, Duration: 300 * time.Millisecond}}
	ops := []loadgen.Op{fixedOp("Probe", loadgen.KindRead, time.Millisecond, nil)}

	report, err := loadgen.Run(context.Background(), targets(1), ops, stages, loadgen.SLO{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 100/s for 300ms is 30 requests. Timer granularity makes the exact count
	// unstable, so this asserts the order of magnitude, which is what a wrong
	// pacing implementation would get wrong.
	count := report.Stages[0].Ops["Probe"].Count
	if count < 15 || count > 45 {
		t.Errorf("count = %d, want roughly 30 for 100 rps over 300ms", count)
	}
}

// A stalled server must not let the generator allocate without bound: requests
// it cannot issue are recorded as shed, because a rate that was not sustained
// is the finding, not something to hide.
func TestOpenLoopShedsRatherThanQueueingWithoutBound(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	op := loadgen.Op{Name: "Stalled", Kind: loadgen.KindRead,
		Call: func(ctx context.Context, _ *loadgen.Target) error {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil
		}}

	stages := []loadgen.Stage{{
		Mode: loadgen.ModeOpen, RPS: 200, Duration: 300 * time.Millisecond, MaxInFlight: 2,
	}}
	report, err := loadgen.Run(context.Background(), targets(1), []loadgen.Op{op}, stages, loadgen.SLO{})
	close(release)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	stats := report.Stages[0].Ops["Stalled"]
	if stats.ErrorCodes["loadgen.Shed"] == 0 {
		t.Errorf("no shed requests recorded against a server that never answered: %+v", stats.ErrorCodes)
	}
}

// A shed request was never issued, so folding a zero latency into the
// distribution would make the stage that failed to keep up look like the
// fastest one in the run.
func TestShedRequestsAreExcludedFromLatency(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	op := loadgen.Op{Name: "Slow", Kind: loadgen.KindRead,
		Call: func(ctx context.Context, _ *loadgen.Target) error {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil
		}}

	stages := []loadgen.Stage{{Mode: loadgen.ModeOpen, RPS: 200, Duration: 250 * time.Millisecond}}
	report, err := loadgen.Run(context.Background(), targets(1), []loadgen.Op{op}, stages, loadgen.SLO{})
	close(release)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	stats := report.Stages[0].Ops["Slow"]
	if stats.ErrorCodes["loadgen.Shed"] == 0 {
		t.Skip("no requests were shed on this machine; nothing to assert")
	}
	if stats.P50MS <= 0 {
		t.Errorf("p50 = %v, want the issued requests' real latency, not a zero from a shed one", stats.P50MS)
	}
}

// Every worker has a request in flight when a stage's time is up. Counting
// those would put a fixed error per worker into every stage and a truncated
// latency into every distribution, which reads as a cluster fault and is ours.
func TestRequestsCutShortByTheStageDeadlineAreNotFailures(t *testing.T) {
	t.Parallel()

	op := loadgen.Op{Name: "Blocked", Kind: loadgen.KindRead,
		Call: func(ctx context.Context, _ *loadgen.Target) error {
			<-ctx.Done()
			return ctx.Err()
		}}

	stages := []loadgen.Stage{{Mode: loadgen.ModeClosed, Concurrency: 3, Duration: 100 * time.Millisecond}}
	report, err := loadgen.Run(context.Background(), targets(1), []loadgen.Op{op},
		stages, loadgen.SLO{ReadP99: time.Millisecond})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if stats, ok := report.Stages[0].Ops["Blocked"]; ok {
		t.Errorf("cancelled requests were recorded: %+v", stats)
	}
	if len(report.Stages[0].Breached) != 0 {
		t.Errorf("our own deadline breached the SLO: %v", report.Stages[0].Breached)
	}
}

func TestPercentilesComeFromRealSamples(t *testing.T) {
	t.Parallel()

	stages := []loadgen.Stage{{Mode: loadgen.ModeClosed, Concurrency: 1, Duration: 200 * time.Millisecond}}
	ops := []loadgen.Op{fixedOp("Fixed", loadgen.KindRead, 20*time.Millisecond, nil)}

	report, err := loadgen.Run(context.Background(), targets(1), ops, stages, loadgen.SLO{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	stats := report.Stages[0].Ops["Fixed"]
	if stats.P50MS < 15 || stats.P50MS > 60 {
		t.Errorf("p50 = %vms, want near the 20ms the operation actually takes", stats.P50MS)
	}
	if stats.P99MS < stats.P50MS {
		t.Errorf("p99 %v is below p50 %v", stats.P99MS, stats.P50MS)
	}
	if stats.MaxMS < stats.P99MS {
		t.Errorf("max %v is below p99 %v", stats.MaxMS, stats.P99MS)
	}
	if stats.AchievedRPS <= 0 {
		t.Error("achieved rps was not reported")
	}
}

func TestFirstBreachNamesTheStageThatCrossedTheSLO(t *testing.T) {
	t.Parallel()

	// Two stages: the second is slow enough to miss a 30ms read SLO.
	fast := fixedOp("Op", loadgen.KindRead, 2*time.Millisecond, nil)
	slow := fixedOp("Op", loadgen.KindRead, 60*time.Millisecond, nil)

	slo := loadgen.SLO{ReadP99: 30 * time.Millisecond}
	first, err := loadgen.Run(context.Background(), targets(1), []loadgen.Op{fast},
		[]loadgen.Stage{{Mode: loadgen.ModeClosed, Concurrency: 1, Duration: 100 * time.Millisecond}}, slo)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(first.Stages[0].Breached) != 0 {
		t.Fatalf("fast stage reported a breach: %v", first.Stages[0].Breached)
	}

	stages := []loadgen.Stage{
		{Mode: loadgen.ModeClosed, Concurrency: 1, Duration: 100 * time.Millisecond},
		{Mode: loadgen.ModeClosed, Concurrency: 2, Duration: 200 * time.Millisecond},
	}
	report, err := loadgen.Run(context.Background(), targets(1), []loadgen.Op{slow}, stages, slo)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	breach := report.FirstBreach[loadgen.ModeClosed]
	if breach == nil {
		t.Fatal("no breach recorded for a stage well past the SLO")
	}
	if breach.StageIndex != 0 || breach.Concurrency != 1 {
		t.Errorf("breach = stage %d concurrency %d, want the first stage that crossed",
			breach.StageIndex, breach.Concurrency)
	}
}

// A quota rejection means the cluster is working. Counting it as a failure
// would fail a correct run, so declared codes are excluded from the verdict
// while still being reported.
func TestExpectedErrorCodesDoNotBreach(t *testing.T) {
	t.Parallel()

	quota := awserr.New("RequestLimitExceeded", "quota", nil)
	ops := []loadgen.Op{fixedOp("Op", loadgen.KindRead, time.Millisecond, quota)}
	stages := []loadgen.Stage{{Mode: loadgen.ModeClosed, Concurrency: 1, Duration: 80 * time.Millisecond}}

	strict, err := loadgen.Run(context.Background(), targets(1), ops, stages, loadgen.SLO{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(strict.Stages[0].Breached) == 0 {
		t.Error("an undeclared error code did not breach")
	}

	tolerant, err := loadgen.Run(context.Background(), targets(1), ops, stages,
		loadgen.SLO{ExpectedErrorCodes: []string{"RequestLimitExceeded"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(tolerant.Stages[0].Breached) != 0 {
		t.Errorf("a declared error code breached: %v", tolerant.Stages[0].Breached)
	}
	if got := tolerant.Stages[0].Ops["Op"].ErrorCodes["RequestLimitExceeded"]; got == 0 {
		t.Error("a declared error code was not reported at all; it must still be counted")
	}
}

func TestErrorCodeKeepsTheServiceCode(t *testing.T) {
	t.Parallel()

	if got := loadgen.ErrorCode(awserr.New("InternalError", "boom", nil)); got != "InternalError" {
		t.Errorf("ErrorCode = %q, want the service code", got)
	}
	if got := loadgen.ErrorCode(nil); got != "" {
		t.Errorf("ErrorCode(nil) = %q, want empty", got)
	}
	if got := loadgen.ErrorCode(errors.New("dial tcp: refused")); got == "" {
		t.Error("a non-AWS error was classified as no error at all")
	}
}

func TestRunRejectsAnEmptyPlan(t *testing.T) {
	t.Parallel()

	if _, err := loadgen.Run(context.Background(), nil, []loadgen.Op{fixedOp("Op", loadgen.KindRead, 0, nil)}, nil, loadgen.SLO{}); err == nil {
		t.Error("no targets was accepted")
	}
	if _, err := loadgen.Run(context.Background(), targets(1), nil, nil, loadgen.SLO{}); err == nil {
		t.Error("no operations was accepted")
	}
}

func TestResolveOpsRejectsAnUnknownOperation(t *testing.T) {
	t.Parallel()

	if _, err := loadgen.ResolveOps([]string{"DescribeInstances"}); err != nil {
		t.Fatalf("ResolveOps: %v", err)
	}
	_, err := loadgen.ResolveOps([]string{"DescribeInstances", "DeleteEverything"})
	if err == nil {
		t.Fatal("an unknown operation was accepted, so the run would silently be shorter than asked for")
	}
	if !strings.Contains(err.Error(), "DeleteEverything") {
		t.Errorf("error %q does not name the unknown operation", err)
	}
}

func TestSummaryReportsTheCapacityNumber(t *testing.T) {
	t.Parallel()

	slo := loadgen.SLO{ReadP99: 5 * time.Millisecond}
	stages := []loadgen.Stage{{Mode: loadgen.ModeClosed, Concurrency: 3, Duration: 80 * time.Millisecond}}
	ops := []loadgen.Op{fixedOp("Op", loadgen.KindRead, 40*time.Millisecond, nil)}

	report, err := loadgen.Run(context.Background(), targets(1), ops, stages, slo)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	report.Endpoint = "https://api.example.invalid"

	summary := loadgen.Summary(report)
	for _, want := range []string{"https://api.example.invalid", "closed", "concurrency 3", "Op"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary does not mention %q:\n%s", want, summary)
		}
	}
	if !strings.Contains(summary, "first breach at concurrency 3") {
		t.Errorf("summary does not state where the SLO broke:\n%s", summary)
	}
}
