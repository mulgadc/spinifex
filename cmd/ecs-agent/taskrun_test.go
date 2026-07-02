package main

import (
	"context"
	"errors"
	"testing"
	"time"

	ctrruntime "github.com/mulgadc/spinifex/cmd/ecs-agent/runtime"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
)

func testAssign() *bus.Assign {
	return &bus.Assign{
		AccountID:   "123456789012",
		ClusterName: "default",
		InstanceID:  "i-abc123",
		TaskID:      "t-001",
		Containers: []bus.AssignContainer{
			{Name: "web", Image: "registry/web:1", Command: []string{"/bin/true"},
				Environment: map[string]string{"FOO": "bar"}},
		},
	}
}

func newRunAgent(cp controlPlane, rt *ctrruntime.FakePuller) *Agent {
	return newAgent(config{}, testIdentity(), cp, rt, rt, nil)
}

// runTask with no runtime reports the task STOPPED instead of crashing.
func TestRunTask_NoRuntimeReportsStopped(t *testing.T) {
	cp := &fakeCP{}
	a := newAgent(config{}, testIdentity(), cp, nil, nil, nil)
	a.runTask(context.Background(), testAssign())

	st := cp.taskStates()
	if len(st) != 1 || st[0].LastStatus != bus.TaskStatusStopped {
		t.Fatalf("want one STOPPED state, got %+v", st)
	}
	if st[0].Reason == "" {
		t.Error("STOPPED state missing reason")
	}
}

// Happy path: pull + run reports RUNNING with the container ID; Wait blocking
// (forced error) means no STOPPED overwrite, so we can assert the RUNNING frame.
func TestRunTask_PullRunReportsRunning(t *testing.T) {
	cp := &fakeCP{}
	rt := &ctrruntime.FakePuller{WaitErr: errors.New("blocked")}
	a := newRunAgent(cp, rt)
	as := testAssign()
	a.runTask(context.Background(), as)

	if len(rt.Pulls) != 1 || rt.Pulls[0].Ref != "registry/web:1" {
		t.Fatalf("expected one pull of registry/web:1, got %+v", rt.Pulls)
	}
	if len(rt.Runs) != 1 || rt.Runs[0].Image != "registry/web:1" {
		t.Fatalf("expected one run, got %+v", rt.Runs)
	}
	if rt.Runs[0].Labels["mulga.ecs.taskID"] != "t-001" {
		t.Errorf("missing task label: %+v", rt.Runs[0].Labels)
	}

	st := cp.taskStates()
	if len(st) == 0 || st[len(st)-1].LastStatus != bus.TaskStatusRunning {
		t.Fatalf("want RUNNING final state, got %+v", st)
	}
	if len(st[len(st)-1].Containers) != 1 || st[len(st)-1].Containers[0].ContainerID != "t-001-web" {
		t.Errorf("RUNNING state container wrong: %+v", st[len(st)-1].Containers)
	}
}

// A pull failure reports STOPPED and never starts the container.
func TestRunTask_PullFailureReportsStopped(t *testing.T) {
	cp := &fakeCP{}
	rt := &ctrruntime.FakePuller{Err: errors.New("pull denied")}
	a := newRunAgent(cp, rt)
	a.runTask(context.Background(), testAssign())

	if len(rt.Runs) != 0 {
		t.Errorf("container should not run after pull failure, got %+v", rt.Runs)
	}
	st := cp.taskStates()
	if len(st) != 1 || st[0].LastStatus != bus.TaskStatusStopped {
		t.Fatalf("want STOPPED, got %+v", st)
	}
}

// A run failure reports STOPPED.
func TestRunTask_RunFailureReportsStopped(t *testing.T) {
	cp := &fakeCP{}
	rt := &ctrruntime.FakePuller{RunErr: errors.New("start boom")}
	a := newRunAgent(cp, rt)
	a.runTask(context.Background(), testAssign())

	st := cp.taskStates()
	if len(st) != 1 || st[0].LastStatus != bus.TaskStatusStopped {
		t.Fatalf("want STOPPED, got %+v", st)
	}
}

// On container exit, waitContainer reports STOPPED with the exit code.
func TestRunTask_ExitReportsStoppedWithCode(t *testing.T) {
	cp := &fakeCP{}
	rt := &ctrruntime.FakePuller{WaitCode: 7}
	a := newRunAgent(cp, rt)
	a.runTask(context.Background(), testAssign())

	deadline := time.After(time.Second)
	for {
		st := cp.taskStates()
		if len(st) > 0 && st[len(st)-1].LastStatus == bus.TaskStatusStopped {
			last := st[len(st)-1]
			if len(last.Containers) != 1 || last.Containers[0].ExitCode == nil || *last.Containers[0].ExitCode != 7 {
				t.Fatalf("want exit code 7, got %+v", last.Containers)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("no STOPPED state after exit; got %+v", cp.taskStates())
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// pollAssignments dispatches each assign once and acks it on the next poll.
func TestPollAssignments_DispatchesOnceAndAcks(t *testing.T) {
	as := testAssign()
	cp := &fakeCP{pollReplies: [][]bus.Assign{
		{*as}, // poll 1: one pending assign
		{*as}, // poll 2: still present (not yet acked when poll 2 was issued)
		nil,   // poll 3: gateway dropped it after the ack
	}}
	rt := &ctrruntime.FakePuller{WaitErr: errors.New("blocked")}
	a := newAgent(config{PollInterval: 5 * time.Millisecond}, testIdentity(), cp, rt, rt, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go a.pollAssignments(ctx, map[string]bool{})

	// runTask reports RUNNING through the (mutex-guarded) control plane. With the
	// assign returned on two consecutive polls, dedup means exactly one dispatch →
	// exactly one RUNNING report.
	deadline := time.After(time.Second)
	for {
		if running := countStatus(cp.taskStates(), as.TaskID, bus.TaskStatusRunning); running >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("assign was never dispatched (no RUNNING report)")
		case <-time.After(2 * time.Millisecond):
		}
	}
	time.Sleep(20 * time.Millisecond)
	cancel()

	if running := countStatus(cp.taskStates(), as.TaskID, bus.TaskStatusRunning); running != 1 {
		t.Errorf("task dispatched %d times, want exactly 1 (dedup on taskID)", running)
	}
	// The taskID must appear in a later poll's ack list.
	acked := false
	for _, ack := range cp.acks() {
		for _, id := range ack {
			if id == as.TaskID {
				acked = true
			}
		}
	}
	if !acked {
		t.Errorf("task %s was never acked; acks=%v", as.TaskID, cp.acks())
	}
}

// stopTask reaps the task's labeled containers and reports STOPPED with the
// directive reason and container status.
func TestStopTask_ReapsLabeledContainersAndReportsStopped(t *testing.T) {
	cp := &fakeCP{}
	rt := &ctrruntime.FakePuller{Containers: []ctrruntime.Container{
		{ID: "t-001-web", Running: true, Labels: map[string]string{
			"mulga.ecs.taskID": "t-001", "mulga.ecs.containerName": "web",
		}},
		{ID: "t-999-other", Running: true, Labels: map[string]string{
			"mulga.ecs.taskID": "t-999", "mulga.ecs.containerName": "web",
		}},
	}}
	a := newRunAgent(cp, rt)

	a.stopTask(context.Background(), bus.StopDirective{TaskID: "t-001", Reason: "bye"})

	if len(rt.Removed) != 1 || rt.Removed[0] != "t-001-web" {
		t.Fatalf("want only t-001-web removed, got %+v", rt.Removed)
	}
	st := cp.taskStates()
	if len(st) != 1 || st[0].LastStatus != bus.TaskStatusStopped {
		t.Fatalf("want one STOPPED report, got %+v", st)
	}
	if st[0].Reason != "bye" {
		t.Errorf("want reason %q, got %q", "bye", st[0].Reason)
	}
	if len(st[0].Containers) != 1 || st[0].Containers[0].Status != bus.TaskStatusStopped {
		t.Errorf("want one STOPPED container, got %+v", st[0].Containers)
	}
}

// A stop for a task with no live containers still reports STOPPED so the
// scheduler releases its capacity.
func TestStopTask_NoContainersStillReportsStopped(t *testing.T) {
	cp := &fakeCP{}
	rt := &ctrruntime.FakePuller{}
	a := newRunAgent(cp, rt)

	a.stopTask(context.Background(), bus.StopDirective{TaskID: "t-001", Reason: "gone"})

	if len(rt.Removed) != 0 {
		t.Errorf("nothing to remove, got %+v", rt.Removed)
	}
	st := cp.taskStates()
	if len(st) != 1 || st[0].LastStatus != bus.TaskStatusStopped {
		t.Fatalf("want one STOPPED report, got %+v", st)
	}
}

// A stop delivered by poll is dispatched once, acked in the stop-ack list, and
// suppresses a same-poll assign for the same task.
func TestPollAssignments_StopReapsAndSuppressesAssign(t *testing.T) {
	as := testAssign()
	cp := &fakeCP{
		pollReplies: [][]bus.Assign{{*as}, {*as}, nil},
		stopReplies: [][]bus.StopDirective{{{TaskID: as.TaskID, Reason: "bye"}}},
	}
	rt := &ctrruntime.FakePuller{Containers: []ctrruntime.Container{
		{ID: "t-001-web", Running: true, Labels: map[string]string{"mulga.ecs.taskID": "t-001"}},
	}}
	a := newAgent(config{PollInterval: 5 * time.Millisecond}, testIdentity(), cp, rt, rt, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go a.pollAssignments(ctx, map[string]bool{})

	deadline := time.After(time.Second)
	for {
		if stopped := countStatus(cp.taskStates(), as.TaskID, bus.TaskStatusStopped); stopped >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("stop never reaped; states=%+v", cp.taskStates())
		case <-time.After(2 * time.Millisecond):
		}
	}
	time.Sleep(20 * time.Millisecond)
	cancel()

	// The assign for the stopping task must never have started a container.
	if len(rt.Runs) != 0 {
		t.Errorf("assign for a stopping task should not run, got %+v", rt.Runs)
	}
	if running := countStatus(cp.taskStates(), as.TaskID, bus.TaskStatusRunning); running != 0 {
		t.Errorf("stopping task should never report RUNNING, got %d", running)
	}
	acked := false
	for _, ack := range cp.stopAcks() {
		for _, id := range ack {
			if id == as.TaskID {
				acked = true
			}
		}
	}
	if !acked {
		t.Errorf("stop %s was never acked; stopAcks=%v", as.TaskID, cp.stopAcks())
	}
}

// stopTask with no runtime is a no-op: nothing to reap, no state reported.
func TestStopTask_NilRunnerNoOp(t *testing.T) {
	cp := &fakeCP{}
	a := newAgent(config{}, testIdentity(), cp, nil, nil, nil)
	a.stopTask(context.Background(), bus.StopDirective{TaskID: "t-001", Reason: "x"})
	if len(cp.taskStates()) != 0 {
		t.Fatalf("nil runner should report no state, got %+v", cp.taskStates())
	}
}

// A List failure aborts the reap without reporting a (false) STOPPED state.
func TestStopTask_ListErrorNoReport(t *testing.T) {
	cp := &fakeCP{}
	rt := &ctrruntime.FakePuller{ListErr: errors.New("list boom")}
	a := newRunAgent(cp, rt)
	a.stopTask(context.Background(), bus.StopDirective{TaskID: "t-001", Reason: "x"})
	if len(cp.taskStates()) != 0 {
		t.Fatalf("list error should report no state, got %+v", cp.taskStates())
	}
}

// A directive with no reason reaps the matching container and reports STOPPED with
// the default reason.
func TestStopTask_EmptyReasonReapsWithDefault(t *testing.T) {
	cp := &fakeCP{}
	rt := &ctrruntime.FakePuller{Containers: []ctrruntime.Container{{
		ID: "t-001-web",
		Labels: map[string]string{
			"mulga.ecs.taskID": "t-001", "mulga.ecs.containerName": "web",
		},
	}}}
	a := newRunAgent(cp, rt)
	a.stopTask(context.Background(), bus.StopDirective{TaskID: "t-001"})

	if len(rt.Removed) != 1 || rt.Removed[0] != "t-001-web" {
		t.Fatalf("want container t-001-web reaped, got %+v", rt.Removed)
	}
	st := cp.taskStates()
	if len(st) != 1 || st[0].LastStatus != bus.TaskStatusStopped {
		t.Fatalf("want one STOPPED report, got %+v", st)
	}
	if st[0].Reason != "Task stopped" {
		t.Errorf("want default reason %q, got %q", "Task stopped", st[0].Reason)
	}
}

func countStatus(states []bus.TaskState, taskID, status string) int {
	n := 0
	for _, st := range states {
		if st.TaskID == taskID && st.LastStatus == status {
			n++
		}
	}
	return n
}

// awsvpc: an assign carrying an ENI MAC builds the task netns and passes its
// path to the container RunSpec.
func TestRunTask_AwsvpcBuildsNetns(t *testing.T) {
	cp := &fakeCP{}
	rt := &ctrruntime.FakePuller{WaitErr: errors.New("blocked")}
	a := newRunAgent(cp, rt)
	f := &fakeNetRunner{linkOut: linkWithENI}
	a.netns = newTestNetns(f)

	as := testAssign()
	as.ENIMacAddress = "52:54:00:de:ad:01"
	a.runTask(context.Background(), as)

	if !f.sawAny("ip netns add ecs-t-001") {
		t.Fatalf("expected task netns build, got %v", f.joined())
	}
	if len(rt.Runs) != 1 || rt.Runs[0].NetnsPath != netnsPathFor("t-001") {
		t.Fatalf("want NetnsPath %q on RunSpec, got %+v", netnsPathFor("t-001"), rt.Runs)
	}
}

// bridge/host: no ENI MAC means no netns is built and the container runs in the
// host (VM) netns (empty NetnsPath).
func TestRunTask_NoMacSkipsNetns(t *testing.T) {
	cp := &fakeCP{}
	rt := &ctrruntime.FakePuller{WaitErr: errors.New("blocked")}
	a := newRunAgent(cp, rt)
	f := &fakeNetRunner{linkOut: linkWithENI}
	a.netns = newTestNetns(f)

	a.runTask(context.Background(), testAssign())

	if len(f.joined()) != 0 {
		t.Fatalf("expected no netns commands, got %v", f.joined())
	}
	if len(rt.Runs) != 1 || rt.Runs[0].NetnsPath != "" {
		t.Fatalf("want empty NetnsPath, got %+v", rt.Runs)
	}
}

func TestContainerID(t *testing.T) {
	if got := containerID("t-1", "web"); got != "t-1-web" {
		t.Errorf("containerID = %q, want t-1-web", got)
	}
}
