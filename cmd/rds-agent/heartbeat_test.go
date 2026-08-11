package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
)

// The first healthy probe is the first proof that the installed include can
// start PostgreSQL, so it must seed recovery without an apply command.
func TestHeartbeater_FirstServingProbeSeedsLastKnownGood(t *testing.T) {
	runner := &recordingRunner{}
	engine := newTestEngine(t, runner.run)
	content := []byte("work_mem = '4096'\n")
	if err := os.WriteFile(engine.parametersPath(), content, 0o600); err != nil {
		t.Fatalf("write installed parameters: %v", err)
	}

	cp := newFakeControlPlane()
	h := newHeartbeater(cp, engine.probe, engine, 0)
	h.beat(context.Background())

	lastGood, err := os.ReadFile(engine.lastGoodPath())
	if err != nil {
		t.Fatalf("read last known good: %v", err)
	}
	if string(lastGood) != string(content) {
		t.Errorf("last known good = %q, want %q", lastGood, content)
	}
	states := cp.snapshotStates()
	if len(states) != 1 || states[0].health != handlers_rds.EngineHealthHealthy {
		t.Errorf("states = %+v, want one healthy heartbeat", states)
	}
}

func TestHeartbeater_BoundsBootstrapFailure(t *testing.T) {
	h := newHeartbeater(newFakeControlPlane(), newEngineProbe(testProbeConfig(), staticProbe(2)), nil, 0)
	h.setBootstrapFailure("bootstrap fetch", errors.New(strings.Repeat("界", 1000)))

	failure := h.bootstrapFailure.Load()
	if failure == nil {
		t.Fatal("bootstrap failure was not stored")
	}
	if len(*failure) > maxBootstrapFailureBytes {
		t.Errorf("bootstrap failure is %d bytes, want at most %d", len(*failure), maxBootstrapFailureBytes)
	}
	if !utf8.ValidString(*failure) {
		t.Error("bootstrap failure was truncated inside a UTF-8 encoding")
	}
	if !strings.HasSuffix(*failure, "...") {
		t.Errorf("bootstrap failure %q does not show that it was truncated", *failure)
	}
}

func TestHeartbeater_ChecksServingParametersOnEveryHealthyProbe(t *testing.T) {
	code := 0
	cfg := testProbeConfig()
	probe := newEngineProbe(cfg, func(context.Context, string, ...string) (int, error) {
		return code, nil
	})
	recorder := &countingServingRecorder{}
	h := newHeartbeater(newFakeControlPlane(), probe, recorder, 0)

	h.beat(context.Background())
	h.beat(context.Background())
	code = 2
	h.beat(context.Background())
	code = 0
	h.beat(context.Background())

	if recorder.calls != 3 {
		t.Errorf("record calls = %d, want one per healthy probe", recorder.calls)
	}
}

type countingServingRecorder struct {
	calls int
}

var _ servingParameterRecorder = (*countingServingRecorder)(nil)

func (r *countingServingRecorder) RecordServingParameters(context.Context) error {
	r.calls++
	return nil
}
