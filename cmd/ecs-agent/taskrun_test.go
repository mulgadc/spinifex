package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	ctrruntime "github.com/mulgadc/spinifex/cmd/ecs-agent/runtime"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
)

// historyPub records every published message in order (capturePub keeps only the
// last per subject, which loses the RUNNING→STOPPED transition on one subject).
type historyPub struct {
	mu   sync.Mutex
	msgs [][]byte
}

func (h *historyPub) Publish(_ string, data []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	h.msgs = append(h.msgs, cp)
	return nil
}

func (h *historyPub) states() []bus.TaskState {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]bus.TaskState, 0, len(h.msgs))
	for _, raw := range h.msgs {
		var ts bus.TaskState
		if err := json.Unmarshal(raw, &ts); err == nil {
			out = append(out, ts)
		}
	}
	return out
}

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

func newRunAgent(pub publisher, rt *ctrruntime.FakePuller) *Agent {
	return newAgent(config{}, testIdentity(), pub, rt, rt, nil)
}

// runTask with no runtime reports the task STOPPED instead of crashing.
func TestRunTask_NoRuntimeReportsStopped(t *testing.T) {
	pub := &historyPub{}
	a := newAgent(config{}, testIdentity(), pub, nil, nil, nil)
	a.runTask(context.Background(), testAssign())

	st := pub.states()
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
	pub := &historyPub{}
	rt := &ctrruntime.FakePuller{WaitErr: errors.New("blocked")}
	a := newRunAgent(pub, rt)
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

	st := pub.states()
	if len(st) == 0 || st[len(st)-1].LastStatus != bus.TaskStatusRunning {
		t.Fatalf("want RUNNING final state, got %+v", st)
	}
	if len(st[len(st)-1].Containers) != 1 || st[len(st)-1].Containers[0].ContainerID != "t-001-web" {
		t.Errorf("RUNNING state container wrong: %+v", st[len(st)-1].Containers)
	}
}

// A pull failure reports STOPPED and never starts the container.
func TestRunTask_PullFailureReportsStopped(t *testing.T) {
	pub := &historyPub{}
	rt := &ctrruntime.FakePuller{Err: errors.New("pull denied")}
	a := newRunAgent(pub, rt)
	a.runTask(context.Background(), testAssign())

	if len(rt.Runs) != 0 {
		t.Errorf("container should not run after pull failure, got %+v", rt.Runs)
	}
	st := pub.states()
	if len(st) != 1 || st[0].LastStatus != bus.TaskStatusStopped {
		t.Fatalf("want STOPPED, got %+v", st)
	}
}

// A run failure reports STOPPED.
func TestRunTask_RunFailureReportsStopped(t *testing.T) {
	pub := &historyPub{}
	rt := &ctrruntime.FakePuller{RunErr: errors.New("start boom")}
	a := newRunAgent(pub, rt)
	a.runTask(context.Background(), testAssign())

	st := pub.states()
	if len(st) != 1 || st[0].LastStatus != bus.TaskStatusStopped {
		t.Fatalf("want STOPPED, got %+v", st)
	}
}

// On container exit, waitContainer reports STOPPED with the exit code.
func TestRunTask_ExitReportsStoppedWithCode(t *testing.T) {
	pub := &historyPub{}
	rt := &ctrruntime.FakePuller{WaitCode: 7}
	a := newRunAgent(pub, rt)
	a.runTask(context.Background(), testAssign())

	deadline := time.After(time.Second)
	for {
		st := pub.states()
		if len(st) > 0 && st[len(st)-1].LastStatus == bus.TaskStatusStopped {
			last := st[len(st)-1]
			if len(last.Containers) != 1 || last.Containers[0].ExitCode == nil || *last.Containers[0].ExitCode != 7 {
				t.Fatalf("want exit code 7, got %+v", last.Containers)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("no STOPPED state after exit; got %+v", pub.states())
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// awsvpc: an assign carrying an ENI MAC builds the task netns and passes its
// path to the container RunSpec.
func TestRunTask_AwsvpcBuildsNetns(t *testing.T) {
	pub := &historyPub{}
	rt := &ctrruntime.FakePuller{WaitErr: errors.New("blocked")}
	a := newRunAgent(pub, rt)
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
	pub := &historyPub{}
	rt := &ctrruntime.FakePuller{WaitErr: errors.New("blocked")}
	a := newRunAgent(pub, rt)
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
