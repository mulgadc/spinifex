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

// Containers for another cluster or with a stopped task are not adopted.
func TestReconcile_FiltersOtherClusterAndStopped(t *testing.T) {
	rt := &ctrruntime.FakePuller{
		WaitErr: errors.New("blocked"),
		Containers: []ctrruntime.Container{
			adoptedContainer("t-other", "web", "other", "", "", "", true),
			adoptedContainer("t-stop", "web", "default", "", "", "", false),
			adoptedContainer("t-ok", "web", "default", "", "", "", true),
		},
	}
	a := newAgent(config{}, testIdentity(), &fakeCP{}, rt, rt, nil)

	adopted := a.reconcile(context.Background())

	if len(adopted) != 1 || !adopted["t-ok"] {
		t.Fatalf("adopted = %v, want only t-ok", adopted)
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
