package main

import (
	"context"
	"testing"
	"time"

	ctrruntime "github.com/mulgadc/spinifex/cmd/ecs-agent/runtime"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
)

func TestAgent_RunRegistersThenStopsOnContext(t *testing.T) {
	pub := newCapturePub()
	cfg := config{Heartbeat: 5 * time.Millisecond}
	id := testIdentity()
	puller := &ctrruntime.FakePuller{}
	a := newAgent(cfg, id, pub, puller, puller, nil)
	a.closers = append(a.closers, puller.Close)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	// Give it time to register + emit a heartbeat or two.
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

	regSubj := bus.RegisterSubject(id.AccountID, id.ClusterName, id.InstanceID)
	if _, ok := pub.msgs[regSubj]; !ok {
		t.Errorf("no register message on %s", regSubj)
	}
	hbSubj := bus.HeartbeatSubject(id.AccountID, id.ClusterName, id.InstanceID)
	if _, ok := pub.msgs[hbSubj]; !ok {
		t.Errorf("no heartbeat message on %s", hbSubj)
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
