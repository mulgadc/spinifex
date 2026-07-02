package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
)

// fakeCP is a test double for the gateway control-plane. It records registers and
// task-state reports, and replays a scripted queue of poll responses.
type fakeCP struct {
	mu sync.Mutex

	registers    int
	states       []bus.TaskState
	pollAcks     [][]string
	pollStopAcks [][]string
	pollReplies  [][]bus.Assign
	stopReplies  [][]bus.StopDirective
	pollCalls    int

	registerErr bool
	submitErr   bool
}

var _ controlPlane = (*fakeCP)(nil)

func (f *fakeCP) Register(_ identity) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.registerErr {
		return errors.New("register boom")
	}
	f.registers++
	return nil
}

func (f *fakeCP) SubmitTaskState(st bus.TaskState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.submitErr {
		return errors.New("submit boom")
	}
	f.states = append(f.states, st)
	return nil
}

func (f *fakeCP) PollAssignments(_, _ string, ackAssigns, ackStops []string) ([]bus.Assign, []bus.StopDirective, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pollAcks = append(f.pollAcks, append([]string(nil), ackAssigns...))
	f.pollStopAcks = append(f.pollStopAcks, append([]string(nil), ackStops...))
	var out []bus.Assign
	if f.pollCalls < len(f.pollReplies) {
		out = f.pollReplies[f.pollCalls]
	}
	var stops []bus.StopDirective
	if f.pollCalls < len(f.stopReplies) {
		stops = f.stopReplies[f.pollCalls]
	}
	f.pollCalls++
	return out, stops, nil
}

func (f *fakeCP) registerCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.registers
}

func (f *fakeCP) taskStates() []bus.TaskState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bus.TaskState(nil), f.states...)
}

func (f *fakeCP) acks() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.pollAcks...)
}

func (f *fakeCP) stopAcks() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.pollStopAcks...)
}

func testIdentity() identity {
	return identity{
		AccountID:    "123456789012",
		ClusterName:  "default",
		InstanceID:   "i-abc123",
		AZ:           "us-east-1a",
		Hostname:     "ecs-host",
		Capacity:     bus.InstanceCapacity{CPU: 2048, MemoryMiB: 4096},
		AgentVersion: "test",
	}
}

func TestRegistrar_Register(t *testing.T) {
	cp := &fakeCP{}
	if err := newRegistrar(cp, testIdentity()).Register(); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if cp.registerCount() != 1 {
		t.Errorf("registers = %d, want 1", cp.registerCount())
	}
}

func TestRegistrar_RegisterError(t *testing.T) {
	cp := &fakeCP{registerErr: true}
	if err := newRegistrar(cp, testIdentity()).Register(); err == nil {
		t.Fatal("expected register error")
	}
}

func TestHeartbeater_Beat(t *testing.T) {
	cp := &fakeCP{}
	if err := newHeartbeater(cp, testIdentity(), time.Second).beat(); err != nil {
		t.Fatalf("beat: %v", err)
	}
	if cp.registerCount() != 1 {
		t.Errorf("beat should re-register once, got %d", cp.registerCount())
	}
}

func TestHeartbeater_DefaultInterval(t *testing.T) {
	h := newHeartbeater(&fakeCP{}, testIdentity(), 0)
	if h.interval != defaultHeartbeat {
		t.Errorf("interval = %v, want default", h.interval)
	}
}

func TestHeartbeater_RunStopsOnContext(t *testing.T) {
	cp := &fakeCP{}
	h := newHeartbeater(cp, testIdentity(), 5*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() { h.Run(ctx); close(done) }()

	time.Sleep(25 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop on context cancel")
	}
	if cp.registerCount() == 0 {
		t.Error("expected at least one re-register before cancel")
	}
}
