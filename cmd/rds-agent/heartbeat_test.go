package main

import (
	"context"
	"os"
	"testing"

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

func TestHeartbeater_RecordsEachServingTransition(t *testing.T) {
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

	if recorder.calls != 2 {
		t.Errorf("record calls = %d, want one per serving transition", recorder.calls)
	}
}

type countingServingRecorder struct {
	calls int
}

var _ servingParameterRecorder = (*countingServingRecorder)(nil)

func (r *countingServingRecorder) RecordServingParameters() error {
	r.calls++
	return nil
}
