package main

import (
	"context"
	"errors"
	"testing"

	ctrruntime "github.com/mulgadc/spinifex/cmd/ecs-agent/runtime"
)

// TestRuntimeGate_DialsUntilTheSocketAnswers is the boot case the agent used to
// lose: the node starts containerd and the agent together, so the first dials
// fail. Resolving once left the agent runtime-less for the life of the process.
func TestRuntimeGate_DialsUntilTheSocketAnswers(t *testing.T) {
	g := newRuntimeGate(nil)
	if g.available() {
		t.Fatal("empty gate reported a runtime")
	}

	rt := &ctrruntime.FakePuller{}
	dials := 0
	dial := func() (ctrruntime.Runtime, error) {
		dials++
		if dials < 3 {
			return nil, errors.New("dial containerd: no such file or directory")
		}
		return rt, nil
	}

	for range 2 {
		g.step(t.Context(), dial)
		if g.available() {
			t.Fatalf("gate resolved a runtime after %d failed dials", dials)
		}
	}

	g.step(t.Context(), dial)
	if g.get() != rt {
		t.Fatalf("gate did not pick up the runtime once the socket answered (dials=%d)", dials)
	}
}

// TestRuntimeGate_DropsARuntimeThatStopsAnswering covers a containerd restarted
// underneath a live agent: the held runtime stops answering, so the gate clears
// it and the next pass re-dials rather than handing out a dead client forever.
func TestRuntimeGate_DropsARuntimeThatStopsAnswering(t *testing.T) {
	dead := &ctrruntime.FakePuller{ListErr: errors.New("connection refused")}
	fresh := &ctrruntime.FakePuller{}
	g := newRuntimeGate(dead)

	dial := func() (ctrruntime.Runtime, error) { return fresh, nil }

	g.step(t.Context(), dial)
	if g.available() {
		t.Fatal("gate kept a runtime whose probe failed")
	}
	if !dead.Closed {
		t.Error("displaced runtime was not closed")
	}

	g.step(t.Context(), dial)
	if g.get() != fresh {
		t.Fatal("gate did not re-dial after clearing")
	}
}

// TestRuntimeGate_KeepsAHealthyRuntime guards the other direction: a probe that
// succeeds must not churn the runtime, or every pass would drop a working client.
func TestRuntimeGate_KeepsAHealthyRuntime(t *testing.T) {
	rt := &ctrruntime.FakePuller{}
	g := newRuntimeGate(rt)

	dial := func() (ctrruntime.Runtime, error) {
		t.Error("dialled while holding a healthy runtime")
		return nil, errors.New("unexpected dial")
	}
	g.step(t.Context(), dial)

	if g.get() != rt {
		t.Fatal("gate dropped a runtime whose probe succeeded")
	}
	if rt.Closed {
		t.Error("healthy runtime was closed")
	}
}

// TestRuntimeGate_ClearIsIdempotent keeps Stop safe to call on an agent that
// never resolved a runtime.
func TestRuntimeGate_ClearIsIdempotent(t *testing.T) {
	g := newRuntimeGate(nil)
	if g.clear() {
		t.Error("clear reported dropping a runtime the gate never held")
	}

	rt := &ctrruntime.FakePuller{}
	g.set(rt)
	if !g.clear() {
		t.Error("clear did not report dropping the held runtime")
	}
	if !rt.Closed {
		t.Error("cleared runtime was not closed")
	}
	if g.clear() {
		t.Error("second clear reported dropping a runtime")
	}
}

// TestRuntimeGate_WatchStopsWithContext keeps the watch loop from outliving the
// agent it belongs to.
func TestRuntimeGate_WatchStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	g := newRuntimeGate(nil)

	done := make(chan struct{})
	go func() {
		g.watch(ctx, func() (ctrruntime.Runtime, error) { return nil, errors.New("no socket") }, 0)
		close(done)
	}()

	cancel()
	<-done
}
