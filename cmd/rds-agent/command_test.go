package main

import (
	"context"
	"errors"
	"testing"
	"time"

	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
)

func TestCommander_DispatchesByNameAndReplies(t *testing.T) {
	var ran handlers_rds.Command
	registry := commandRegistry{
		"reload-parameters": func(_ context.Context, cmd handlers_rds.Command) (string, error) {
			ran = cmd
			return "reloaded", nil
		},
	}

	reply := newCommander(nil, registry, time.Second).execute(context.Background(), handlers_rds.Command{
		CommandID:  "cmd-1",
		Type:       "reload-parameters",
		Parameters: []handlers_rds.Parameter{{Name: "work_mem", Value: "8MB"}},
	})

	if ran.CommandID != "cmd-1" || len(ran.Parameters) != 1 {
		t.Errorf("handler received %+v, want the command with its parameters", ran)
	}
	if reply.Status != handlers_rds.CommandStatusSucceeded || reply.Message != "reloaded" {
		t.Errorf("reply = %+v, want succeeded/reloaded", reply)
	}
	if reply.CommandID != "cmd-1" {
		t.Errorf("reply command ID = %q, want cmd-1", reply.CommandID)
	}
}

func TestCommander_FailedHandlerRepliesWithItsError(t *testing.T) {
	registry := commandRegistry{
		"grow-storage": func(context.Context, handlers_rds.Command) (string, error) {
			return "", errors.New("no space left on device")
		},
	}

	reply := newCommander(nil, registry, time.Second).execute(context.Background(),
		handlers_rds.Command{CommandID: "cmd-2", Type: "grow-storage"})

	if reply.Status != handlers_rds.CommandStatusFailed {
		t.Errorf("reply status = %q, want failed", reply.Status)
	}
	if reply.Message != "no space left on device" {
		t.Errorf("reply message = %q, want the handler's error", reply.Message)
	}
}

// A control plane ahead of the guest issues types this build has no handler
// for. The issuer is blocked on the command ID, so it gets an answer rather
// than a timeout.
func TestCommander_UnknownTypeRepliesFailedNotDropped(t *testing.T) {
	reply := newCommander(nil, newCommandRegistry(&fakeEngine{}, &fakeStorage{}), time.Second).execute(context.Background(),
		handlers_rds.Command{CommandID: "cmd-3", Type: "quiesce"})

	if reply.CommandID != "cmd-3" || reply.Status != handlers_rds.CommandStatusFailed {
		t.Errorf("reply = %+v, want a failed reply for cmd-3", reply)
	}
	if reply.Message == "" {
		t.Error("unsupported-command reply carried no message")
	}
}

// The reply for work the guest actually did rides the next poll. A poll that
// fails must not consume it, or the issuer never learns the outcome.
func TestCommander_RunCarriesRepliesOnTheNextPoll(t *testing.T) {
	cp := newFakeControlPlane()
	cp.pollQueue = [][]handlers_rds.Command{
		{{CommandID: "cmd-1", Type: "reload-parameters"}},
	}
	registry := commandRegistry{
		"reload-parameters": func(context.Context, handlers_rds.Command) (string, error) { return "done", nil },
	}

	c := newCommander(cp, registry, 10*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()

	waitFor(t, func() bool {
		replies := cp.snapshotReplies()
		return len(replies) >= 2 && len(replies[1]) == 1
	}, "the reply to ride the second poll")
	cancel()
	<-done

	replies := cp.snapshotReplies()
	if len(replies[0]) != 0 {
		t.Errorf("first poll carried replies %+v, want none", replies[0])
	}
	if got := replies[1][0]; got.CommandID != "cmd-1" || got.Status != handlers_rds.CommandStatusSucceeded {
		t.Errorf("second poll carried %+v, want the succeeded reply for cmd-1", got)
	}
}

// A poll error must not spin the loop: without a backoff a gateway that is down
// would be re-polled at line rate for as long as it stayed down.
func TestCommander_BacksOffAfterAFailedPoll(t *testing.T) {
	cp := newFakeControlPlane()
	cp.pollErr = errors.New("gateway unreachable")

	c := newCommander(cp, newCommandRegistry(&fakeEngine{}, &fakeStorage{}), 10*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	<-done

	// pollErrorBackoff is 5s, so the window admits the first poll and no more.
	if polls := len(cp.snapshotReplies()); polls > 1 {
		t.Errorf("polled %d times in 200ms against a broken channel, want 1", polls)
	}
}
