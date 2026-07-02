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
