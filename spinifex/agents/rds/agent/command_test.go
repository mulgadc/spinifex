package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	rdsinstance "github.com/mulgadc/spinifex/spinifex/domains/rds/instance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommander_DispatchesByNameAndReplies(t *testing.T) {
	var ran rdsinstance.Command
	registry := commandRegistry{
		"reload-parameters": func(_ context.Context, cmd rdsinstance.Command) (string, error) {
			ran = cmd
			return "reloaded", nil
		},
	}

	reply := newCommander(nil, registry, time.Second).execute(context.Background(), rdsinstance.Command{
		CommandID:  "cmd-1",
		Type:       "reload-parameters",
		Parameters: []rdsinstance.Parameter{{Name: "work_mem", Value: "8MB"}},
	})

	if ran.CommandID != "cmd-1" || len(ran.Parameters) != 1 {
		t.Errorf("handler received %+v, want the command with its parameters", ran)
	}
	if reply.Status != rdsinstance.CommandStatusSucceeded || reply.Message != "reloaded" {
		t.Errorf("reply = %+v, want succeeded/reloaded", reply)
	}
	if reply.CommandID != "cmd-1" {
		t.Errorf("reply command ID = %q, want cmd-1", reply.CommandID)
	}
}

func TestCommander_FailedHandlerRepliesWithItsError(t *testing.T) {
	registry := commandRegistry{
		"grow-storage": func(context.Context, rdsinstance.Command) (string, error) {
			return "", errors.New("no space left on device")
		},
	}

	reply := newCommander(nil, registry, time.Second).execute(context.Background(),
		rdsinstance.Command{CommandID: "cmd-2", Type: "grow-storage"})

	if reply.Status != rdsinstance.CommandStatusFailed {
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
		rdsinstance.Command{CommandID: "cmd-3", Type: "quiesce"})

	if reply.CommandID != "cmd-3" || reply.Status != rdsinstance.CommandStatusFailed {
		t.Errorf("reply = %+v, want a failed reply for cmd-3", reply)
	}
	if reply.Message == "" {
		t.Error("unsupported-command reply carried no message")
	}
}

// The reply for work the guest actually did rides a later poll. A poll that
// fails must not consume it, or the issuer never learns the outcome.
func TestCommander_RunCarriesRepliesOnALaterPoll(t *testing.T) {
	cp := newFakeControlPlane()
	cp.pollQueue = [][]rdsinstance.Command{
		{{CommandID: "cmd-1", Type: "reload-parameters"}},
	}
	registry := commandRegistry{
		"reload-parameters": func(context.Context, rdsinstance.Command) (string, error) { return "done", nil },
	}

	c := newCommander(cp, registry, 10*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()

	waitFor(t, func() bool { return len(repliesFor(cp, "cmd-1")) > 0 }, "the reply to ride a later poll")
	cancel()
	<-done

	if replies := cp.snapshotReplies(); len(replies[0]) != 0 {
		t.Errorf("first poll carried replies %+v, want none", replies[0])
	}
	if got := repliesFor(cp, "cmd-1")[0]; got.Status != rdsinstance.CommandStatusSucceeded {
		t.Errorf("the poll carried %+v, want the succeeded reply for cmd-1", got)
	}
}

func TestCommander_RetainsAndRetriesTheCompletePendingBatch(t *testing.T) {
	cp := newFakeControlPlane()
	pending := []rdsinstance.CommandReply{
		{CommandID: "cmd-1", Status: rdsinstance.CommandStatusSucceeded},
		{CommandID: "cmd-2", Status: rdsinstance.CommandStatusFailed, Message: "disk full"},
	}
	c := newCommander(cp, commandRegistry{
		"reload-parameters": func(context.Context, rdsinstance.Command) (string, error) {
			return "reloaded", nil
		},
	}, 10*time.Millisecond)
	c.pending = append([]rdsinstance.CommandReply(nil), pending...)
	cp.pollErr = errors.New("reply publication failed")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	<-cp.polled
	cancel()
	<-done

	assert.Equal(t, pending, c.pending)
	replies := cp.snapshotReplies()
	require.Len(t, replies, 1)
	assert.Equal(t, pending, replies[0])

	cp.mu.Lock()
	cp.pollErr = nil
	cp.pollQueue = [][]rdsinstance.Command{{{
		CommandID: "cmd-3",
		Type:      "reload-parameters",
	}}}
	cp.mu.Unlock()

	ctx, cancel = context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	waitFor(t, func() bool {
		return len(repliesFor(cp, "cmd-3")) > 0
	}, "the retained batch to succeed and the new reply to reach a later poll")
	cancel()
	<-done

	replies = cp.snapshotReplies()
	assert.Equal(t, pending, replies[1], "the complete failed batch must be retried in order")
	assert.Equal(t, rdsinstance.CommandStatusSucceeded, repliesFor(cp, "cmd-3")[0].Status)
	for _, later := range replies[2:] {
		for _, reply := range later {
			assert.Equal(t, "cmd-3", reply.CommandID, "a batch a poll carried must not be sent again")
		}
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

// A MariaDB statement that commits waits out a held BACKUP STAGE, and the
// release is itself a command. Run one at a time, the release would queue behind
// the statement it is meant to unblock until the hold's own deadline.
func TestCommander_ABlockedCommandDoesNotKeepOutTheRelease(t *testing.T) {
	cp := newFakeControlPlane()
	cp.pollQueue = [][]rdsinstance.Command{
		{{CommandID: "cmd-password", Type: rdsinstance.CommandSetPassword}},
		{{CommandID: "cmd-release", Type: rdsinstance.CommandUnquiesce}},
	}
	released := make(chan struct{})
	registry := commandRegistry{
		rdsinstance.CommandSetPassword: func(ctx context.Context, _ rdsinstance.Command) (string, error) {
			select {
			case <-released:
				return "", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		},
		rdsinstance.CommandUnquiesce: func(context.Context, rdsinstance.Command) (string, error) {
			close(released)
			return "", nil
		},
	}

	c := newCommander(cp, registry, 10*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()

	waitFor(t, func() bool {
		return len(repliesFor(cp, "cmd-password")) > 0 && len(repliesFor(cp, "cmd-release")) > 0
	}, "both the release and the command it unblocked to be answered")
	cancel()
	<-done

	for _, id := range []string{"cmd-password", "cmd-release"} {
		assert.Equal(t, rdsinstance.CommandStatusSucceeded, repliesFor(cp, id)[0].Status, id)
	}
}

// The issuer re-publishes until it has a reply, so a command still running is
// delivered again. Running the copy would rotate a password twice or open a
// second backup alongside the first.
func TestCommander_ARepublishedCommandRunsOnce(t *testing.T) {
	cp := newFakeControlPlane()
	cmd := rdsinstance.Command{CommandID: "cmd-1", Type: "reload-parameters"}
	cp.pollQueue = [][]rdsinstance.Command{{cmd}, {cmd}}
	var mu sync.Mutex
	runs := 0
	secondDelivered := make(chan struct{})
	registry := commandRegistry{
		"reload-parameters": func(ctx context.Context, _ rdsinstance.Command) (string, error) {
			mu.Lock()
			runs++
			mu.Unlock()
			select {
			case <-secondDelivered:
			case <-ctx.Done():
			}
			return "done", nil
		},
	}

	c := newCommander(cp, registry, 10*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()

	// The third poll means both copies have been handed to the commander.
	waitFor(t, func() bool { return len(cp.snapshotReplies()) >= 3 }, "both copies to be delivered")
	close(secondDelivered)
	waitFor(t, func() bool { return len(repliesFor(cp, "cmd-1")) > 0 }, "the reply")
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, runs, "a re-published copy of a running command must not run again")
}

// Every reply to id carried on any poll so far.
func repliesFor(cp *fakeControlPlane, id string) []rdsinstance.CommandReply {
	var out []rdsinstance.CommandReply
	for _, batch := range cp.snapshotReplies() {
		for _, reply := range batch {
			if reply.CommandID == id {
				out = append(out, reply)
			}
		}
	}
	return out
}
