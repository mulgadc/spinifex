package main

import (
	"context"
	"testing"
	"time"

	ctrruntime "github.com/mulgadc/spinifex/cmd/ecs-agent/runtime"
)

func TestAgent_RunRegistersThenStopsOnContext(t *testing.T) {
	cp := &fakeCP{}
	cfg := config{Heartbeat: 5 * time.Millisecond, PollInterval: 5 * time.Millisecond}
	puller := &ctrruntime.FakePuller{}
	a := newAgent(cfg, testIdentity(), cp, puller, puller, nil)
	a.closers = append(a.closers, puller.Close)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	// Give it time to register + re-register (heartbeat) a few times.
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}

	// One boot register + at least one heartbeat re-register.
	if cp.registerCount() < 2 {
		t.Errorf("registers = %d, want >= 2 (boot + heartbeat)", cp.registerCount())
	}
	if !puller.Closed {
		t.Error("Stop did not close the puller")
	}
}

func TestDetectCapacity_Positive(t *testing.T) {
	c := detectCapacity()
	if c.CPU <= 0 {
		t.Errorf("CPU = %d, want > 0", c.CPU)
	}
	// MemoryMiB may be 0 on platforms without /proc/meminfo; just assert non-negative.
	if c.MemoryMiB < 0 {
		t.Errorf("MemoryMiB = %d, want >= 0", c.MemoryMiB)
	}
}
