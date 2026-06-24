package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
)

// capturePub records published messages; ErrOn forces Publish to fail.
type capturePub struct {
	mu    sync.Mutex
	msgs  map[string][]byte
	count int
	errOn bool
}

func newCapturePub() *capturePub { return &capturePub{msgs: map[string][]byte{}} }

func (c *capturePub) Publish(subject string, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.errOn {
		return errors.New("publish boom")
	}
	c.count++
	cp := make([]byte, len(data))
	copy(cp, data)
	c.msgs[subject] = cp
	return nil
}

func (c *capturePub) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
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
	pub := newCapturePub()
	id := testIdentity()
	if err := newRegistrar(pub, id).Register(); err != nil {
		t.Fatalf("Register: %v", err)
	}
	subj := bus.RegisterSubject(id.AccountID, id.ClusterName, id.InstanceID)
	raw, ok := pub.msgs[subj]
	if !ok {
		t.Fatalf("no message on %s; got %v", subj, pub.msgs)
	}
	var got bus.RegisterInstance
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.InstanceID != id.InstanceID || got.Capacity.CPU != 2048 {
		t.Errorf("payload mismatch: %+v", got)
	}
	if got.RegisteredAt.IsZero() {
		t.Error("RegisteredAt not set")
	}
}

func TestRegistrar_PublishError(t *testing.T) {
	pub := newCapturePub()
	pub.errOn = true
	if err := newRegistrar(pub, testIdentity()).Register(); err == nil {
		t.Fatal("expected publish error")
	}
}

func TestHeartbeater_Beat(t *testing.T) {
	pub := newCapturePub()
	id := testIdentity()
	h := newHeartbeater(pub, id, time.Second, func() int { return 3 })
	if err := h.beat(); err != nil {
		t.Fatalf("beat: %v", err)
	}
	subj := bus.HeartbeatSubject(id.AccountID, id.ClusterName, id.InstanceID)
	var got bus.Heartbeat
	if err := json.Unmarshal(pub.msgs[subj], &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Status != bus.StatusActive || got.RunningTasks != 3 {
		t.Errorf("payload mismatch: %+v", got)
	}
}

func TestHeartbeater_NilRunningTasksIsZero(t *testing.T) {
	pub := newCapturePub()
	h := newHeartbeater(pub, testIdentity(), 0, nil)
	if h.interval != defaultHeartbeat {
		t.Errorf("interval = %v, want default", h.interval)
	}
	if err := h.beat(); err != nil {
		t.Fatal(err)
	}
}

func TestHeartbeater_RunStopsOnContext(t *testing.T) {
	pub := newCapturePub()
	h := newHeartbeater(pub, testIdentity(), 5*time.Millisecond, nil)
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
	if pub.calls() == 0 {
		t.Error("expected at least one heartbeat before cancel")
	}
}
