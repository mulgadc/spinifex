package main

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	handlers_ecs "github.com/mulgadc/spinifex/spinifex/handlers/ecs"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
)

// fakeCP is a test double for the gateway control-plane. It records registers,
// task-state reports and GPU reports, and replays a scripted queue of poll
// responses.
type fakeCP struct {
	mu sync.Mutex

	registers      int
	lastRegistered identity
	states         []bus.TaskState
	gpuReports     []gpuReport
	pollAcks       [][]string
	pollStopAcks   [][]string
	pollReplies    [][]bus.Assign
	stopReplies    [][]bus.StopDirective
	stopPending    []bus.StopDirective
	pollCalls      int

	registerErr bool
	submitErr   bool
}

// gpuReport is one recorded ReportTaskGPU call.
type gpuReport struct {
	cluster    string
	task       string
	containers []handlers_ecs.ContainerGPUReport
}

var _ controlPlane = (*fakeCP)(nil)

func (f *fakeCP) Register(id identity) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.registerErr {
		return errors.New("register boom")
	}
	f.registers++
	f.lastRegistered = id
	return nil
}

// registered returns the identity of the most recent Register call.
func (f *fakeCP) registered() identity {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastRegistered
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
	// stopPending is the durable inbox the gateway keeps: an acked directive is
	// deleted, and everything still in it is redelivered on every poll.
	f.stopPending = slices.DeleteFunc(f.stopPending, func(sd bus.StopDirective) bool {
		return slices.Contains(ackStops, sd.TaskID)
	})
	stops = slices.Concat(stops, f.stopPending)
	f.pollCalls++
	return out, stops, nil
}

func (f *fakeCP) ReportTaskGPU(cluster, task string, containers []handlers_ecs.ContainerGPUReport) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gpuReports = append(f.gpuReports, gpuReport{cluster: cluster, task: task, containers: containers})
	return nil
}

func (f *fakeCP) gpuReportsFor(taskID string) []gpuReport {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []gpuReport
	for _, r := range f.gpuReports {
		if r.task == taskID {
			out = append(out, r)
		}
	}
	return out
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

// testIdentityWithGPUs is testIdentity seeded with discovered GPU UUIDs, so
// newAgent's ledger has real devices to pin from.
func testIdentityWithGPUs(uuids ...string) identity {
	id := testIdentity()
	id.Capacity.GPU = len(uuids)
	id.Capacity.GPUIDs = uuids
	return id
}

func TestRegistrar_Register(t *testing.T) {
	cp := &fakeCP{}
	if err := newRegistrar(cp, testIdentity(), nil).Register(); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if cp.registerCount() != 1 {
		t.Errorf("registers = %d, want 1", cp.registerCount())
	}
}

func TestRegistrar_RegisterError(t *testing.T) {
	cp := &fakeCP{registerErr: true}
	if err := newRegistrar(cp, testIdentity(), nil).Register(); err == nil {
		t.Fatal("expected register error")
	}
}

func TestHeartbeater_Beat(t *testing.T) {
	cp := &fakeCP{}
	if err := newHeartbeater(cp, testIdentity(), time.Second, nil).beat(); err != nil {
		t.Fatalf("beat: %v", err)
	}
	if cp.registerCount() != 1 {
		t.Errorf("beat should re-register once, got %d", cp.registerCount())
	}
}

func TestHeartbeater_DefaultInterval(t *testing.T) {
	h := newHeartbeater(&fakeCP{}, testIdentity(), 0, nil)
	if h.interval != defaultHeartbeat {
		t.Errorf("interval = %v, want default", h.interval)
	}
}

func TestHeartbeater_RunStopsOnContext(t *testing.T) {
	cp := &fakeCP{}
	h := newHeartbeater(cp, testIdentity(), 5*time.Millisecond, nil)
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

// TestRegistrar_NoRuntimeAdvertisesNoCapacity is the placement half of the boot
// bug: an agent with no container runtime kept registering its full CPU and
// memory, so the scheduler placed tasks it could only fail to start. Capacity is
// advertised only while a runtime is there to use it.
func TestRegistrar_NoRuntimeAdvertisesNoCapacity(t *testing.T) {
	cp := &fakeCP{}
	ready := false
	r := newRegistrar(cp, testIdentity(), func() bool { return ready })

	if err := r.Register(); err != nil {
		t.Fatalf("register: %v", err)
	}
	if got := cp.registered().Capacity; got.CPU != 0 || got.MemoryMiB != 0 {
		t.Fatalf("advertised capacity with no runtime: cpu=%d memory=%d", got.CPU, got.MemoryMiB)
	}

	ready = true
	if err := r.Register(); err != nil {
		t.Fatalf("register: %v", err)
	}
	want := testIdentity().Capacity
	if got := cp.registered().Capacity; got.CPU != want.CPU || got.MemoryMiB != want.MemoryMiB {
		t.Fatalf("capacity not restored once the runtime resolved: cpu=%d memory=%d", got.CPU, got.MemoryMiB)
	}
}

// TestHeartbeater_NoRuntimeAdvertisesNoCapacity is the same rule on the beat,
// which is how a runtime lost after boot reaches the control plane.
func TestHeartbeater_NoRuntimeAdvertisesNoCapacity(t *testing.T) {
	cp := &fakeCP{}
	h := newHeartbeater(cp, testIdentity(), time.Second, func() bool { return false })

	if err := h.beat(); err != nil {
		t.Fatalf("beat: %v", err)
	}
	if got := cp.registered().Capacity; got.CPU != 0 || got.MemoryMiB != 0 {
		t.Fatalf("beat advertised capacity with no runtime: cpu=%d memory=%d", got.CPU, got.MemoryMiB)
	}
	// The instance still identifies itself, so it stays visible as drained rather
	// than vanishing from the cluster.
	if cp.registered().InstanceID != testIdentity().InstanceID {
		t.Error("beat dropped the instance identity")
	}
}
