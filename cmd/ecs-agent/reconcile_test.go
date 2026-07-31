package main

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	ctrruntime "github.com/mulgadc/spinifex/cmd/ecs-agent/runtime"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
)

const testTaskRole = "arn:aws:iam::111122223333:role/task"

// adoptedContainer builds a runtime.Container as the reconciler would discover it,
// stamping only the mulga.ecs.* labels that are non-empty.
func adoptedContainer(taskID, name, cluster, credID, role, mac string, running bool) ctrruntime.Container {
	labels := map[string]string{
		labelTaskID:        taskID,
		labelContainerName: name,
		labelClusterName:   cluster,
	}
	if credID != "" {
		labels[labelCredID] = credID
	}
	if role != "" {
		labels[labelTaskRoleARN] = role
	}
	if mac != "" {
		labels[labelENIMac] = mac
	}
	return ctrruntime.Container{ID: containerID(taskID, name), Labels: labels, Running: running}
}

func waitedFor(t *testing.T, rt *ctrruntime.FakePuller, id string) {
	t.Helper()
	deadline := time.After(time.Second)
	for !slices.Contains(rt.Waits(), id) {
		select {
		case <-deadline:
			t.Fatalf("exit-wait never re-attached for %s; waited=%v", id, rt.Waits())
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// A running, correctly-labeled container is re-adopted: its credentials are
// re-registered, its RUNNING state refreshed, and its exit-wait re-attached.
func TestReconcile_AdoptsRunningLabeledContainer(t *testing.T) {
	cp := &fakeCP{}
	rt := &ctrruntime.FakePuller{
		WaitErr: errors.New("blocked"),
		Containers: []ctrruntime.Container{
			adoptedContainer("t-001", "web", "default", "t-001", testTaskRole, "52:54:00:de:ad:01", true),
		},
	}
	a := newAgent(config{}, testIdentity(), cp, rt, rt, nil)
	a.cred = newCredEndpoint(nil, "us-east-1", "https://gw", "", "127.0.0.1", 0, nil)

	adopted := a.reconcile(context.Background())

	if !adopted["t-001"] {
		t.Fatalf("t-001 not adopted; got %v", adopted)
	}
	if got := a.cred.roles["t-001"]; got != testTaskRole {
		t.Errorf("cred not re-registered: roles[t-001] = %q, want %q", got, testTaskRole)
	}
	if n := countStatus(cp.taskStates(), "t-001", bus.TaskStatusRunning); n != 1 {
		t.Errorf("RUNNING reports for t-001 = %d, want 1", n)
	}
	waitedFor(t, rt, containerID("t-001", "web"))
}

// A task whose container died while the agent was down must be reported STOPPED,
// not silently skipped. The exit-wait is cancelled on agent shutdown, so nothing
// else ever reports that exit: skipping it leaves the gateway holding the task
// RUNNING forever, its service never places a replacement, and the load
// balancer target stays unhealthy with no path back.
func TestReconcile_ReportsTaskWhoseContainerDied(t *testing.T) {
	cp := &fakeCP{}
	rt := &ctrruntime.FakePuller{
		Containers: []ctrruntime.Container{
			adoptedContainer("t-dead", "web", "default", "t-dead", testTaskRole, "52:54:00:de:ad:02", false),
		},
	}
	a := newAgent(config{}, testIdentity(), cp, rt, rt, nil)
	a.cred = newCredEndpoint(nil, "us-east-1", "https://gw", "", "127.0.0.1", 0, nil)

	adopted := a.reconcile(context.Background())

	if n := countStatus(cp.taskStates(), "t-dead", bus.TaskStatusStopped); n != 1 {
		t.Errorf("STOPPED reports for t-dead = %d, want 1", n)
	}
	if n := countStatus(cp.taskStates(), "t-dead", bus.TaskStatusRunning); n != 0 {
		t.Errorf("t-dead must not be reported RUNNING, got %d such reports", n)
	}
	// Seeded so a re-delivered assignment for the dead task is acked, not re-run.
	if !adopted["t-dead"] {
		t.Errorf("t-dead not seeded into the adopted set; got %v", adopted)
	}
	if want := containerID("t-dead", "web"); !slices.Contains(rt.Removed, want) {
		t.Errorf("leftover container %s not removed; removed=%v", want, rt.Removed)
	}
}

// A task with one live and one dead container is still running: the live half is
// re-adopted and the re-attached exit-wait reports the dead one when it is
// reaped, so reconcile must not declare the whole task STOPPED.
func TestReconcile_PartiallyDeadTaskStaysRunning(t *testing.T) {
	cp := &fakeCP{}
	rt := &ctrruntime.FakePuller{
		WaitErr: errors.New("blocked"),
		Containers: []ctrruntime.Container{
			adoptedContainer("t-mixed", "web", "default", "t-mixed", testTaskRole, "52:54:00:de:ad:03", true),
			adoptedContainer("t-mixed", "sidecar", "default", "t-mixed", testTaskRole, "52:54:00:de:ad:03", false),
		},
	}
	a := newAgent(config{}, testIdentity(), cp, rt, rt, nil)
	a.cred = newCredEndpoint(nil, "us-east-1", "https://gw", "", "127.0.0.1", 0, nil)

	a.reconcile(context.Background())

	if n := countStatus(cp.taskStates(), "t-mixed", bus.TaskStatusRunning); n != 1 {
		t.Errorf("RUNNING reports for t-mixed = %d, want 1", n)
	}
	if n := countStatus(cp.taskStates(), "t-mixed", bus.TaskStatusStopped); n != 0 {
		t.Errorf("t-mixed must not be reported STOPPED while a container still runs, got %d", n)
	}
}

// pollAssignments seeded with an adopted task acks its re-delivered assignment
// but does not re-run it, while a genuinely new task still dispatches.
func TestPollAssignments_SeededTaskNotRerun(t *testing.T) {
	mk := func(id string) bus.Assign {
		return bus.Assign{
			AccountID: "123456789012", ClusterName: "default", InstanceID: "i-abc123", TaskID: id,
			Containers: []bus.AssignContainer{{Name: "web", Image: "registry/web:1", Command: []string{"/bin/true"}}},
		}
	}
	adopted, fresh := mk("t-001"), mk("t-002")
	cp := &fakeCP{pollReplies: [][]bus.Assign{
		{adopted, fresh},
		{adopted, fresh},
		nil,
	}}
	rt := &ctrruntime.FakePuller{WaitErr: errors.New("blocked")}
	a := newAgent(config{PollInterval: 5 * time.Millisecond}, testIdentity(), cp, rt, rt, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go a.pollAssignments(ctx, map[string]bool{"t-001": true})

	deadline := time.After(time.Second)
	for countStatus(cp.taskStates(), "t-002", bus.TaskStatusRunning) < 1 {
		select {
		case <-deadline:
			t.Fatal("fresh task t-002 was never dispatched")
		case <-time.After(2 * time.Millisecond):
		}
	}
	time.Sleep(20 * time.Millisecond)
	cancel()

	if n := countStatus(cp.taskStates(), "t-001", bus.TaskStatusRunning); n != 0 {
		t.Errorf("adopted task t-001 was re-run %d times, want 0", n)
	}
	if n := countStatus(cp.taskStates(), "t-002", bus.TaskStatusRunning); n != 1 {
		t.Errorf("fresh task t-002 dispatched %d times, want 1", n)
	}
	var t1Acked bool
	for _, ack := range cp.acks() {
		for _, id := range ack {
			if id == "t-001" {
				t1Acked = true
			}
		}
	}
	if !t1Acked {
		t.Errorf("adopted task t-001 was never acked; acks=%v", cp.acks())
	}
}

// Containers belonging to another cluster are ignored entirely: this agent
// speaks for its own cluster only, and must neither adopt nor report them.
//
// A stopped container of this cluster IS handled — see
// TestReconcile_ReportsTaskWhoseContainerDied. It lands in the adopted set so
// its re-delivered assignment is acked rather than re-run, after reconcile has
// reported the task STOPPED.
func TestReconcile_IgnoresOtherClusterContainers(t *testing.T) {
	cp := &fakeCP{}
	rt := &ctrruntime.FakePuller{
		WaitErr: errors.New("blocked"),
		Containers: []ctrruntime.Container{
			adoptedContainer("t-other", "web", "other", "", "", "", true),
			adoptedContainer("t-other-dead", "web", "other", "", "", "", false),
			adoptedContainer("t-ok", "web", "default", "", "", "", true),
		},
	}
	a := newAgent(config{}, testIdentity(), cp, rt, rt, nil)

	adopted := a.reconcile(context.Background())

	if len(adopted) != 1 || !adopted["t-ok"] {
		t.Fatalf("adopted = %v, want only t-ok", adopted)
	}
	for _, id := range []string{"t-other", "t-other-dead"} {
		if n := countStatus(cp.taskStates(), id, bus.TaskStatusStopped); n != 0 {
			t.Errorf("reported %d STOPPED states for another cluster's task %s, want 0", n, id)
		}
	}
}

// A nil runner or a List error degrades to an empty set without panicking.
func TestReconcile_DegradesGracefully(t *testing.T) {
	noRunner := newAgent(config{}, testIdentity(), &fakeCP{}, nil, nil, nil)
	if got := noRunner.reconcile(context.Background()); len(got) != 0 {
		t.Errorf("nil runner: adopted = %v, want empty", got)
	}

	rt := &ctrruntime.FakePuller{ListErr: errors.New("boom")}
	listErr := newAgent(config{}, testIdentity(), &fakeCP{}, rt, rt, nil)
	if got := listErr.reconcile(context.Background()); len(got) != 0 {
		t.Errorf("list error: adopted = %v, want empty", got)
	}
}

// taskLabels emits the optional cred/role/MAC labels only when set.
func TestTaskLabels_OmitsEmptyOptionalLabels(t *testing.T) {
	full := taskLabels(&bus.Assign{
		TaskID: "t1", ClusterName: "c", CredID: "x", TaskRoleARN: testTaskRole, ENIMacAddress: "mac",
	}, "web")
	for k, want := range map[string]string{
		labelCredID: "x", labelTaskRoleARN: testTaskRole, labelENIMac: "mac",
	} {
		if full[k] != want {
			t.Errorf("full labels[%q] = %q, want %q", k, full[k], want)
		}
	}

	bare := taskLabels(&bus.Assign{TaskID: "t1", ClusterName: "c"}, "web")
	if len(bare) != 3 {
		t.Errorf("bare labels = %v, want only taskID/containerName/clusterName", bare)
	}
	for _, k := range []string{labelCredID, labelTaskRoleARN, labelENIMac} {
		if _, ok := bare[k]; ok {
			t.Errorf("bare labels unexpectedly carry %q", k)
		}
	}
}
