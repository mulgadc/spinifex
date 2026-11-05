package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	t.Parallel()
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

// A copy can arrive after the handler has returned but before a poll has
// carried its reply. The in-flight set has already let go of it then, so only
// the completed set stops it running a second time.
func TestCommander_ACopyArrivingBeforeTheReplyIsCarriedDoesNotRunAgain(t *testing.T) {
	var mu sync.Mutex
	runs := map[string]int{}
	handler := func(_ context.Context, cmd rdsinstance.Command) (string, error) {
		mu.Lock()
		runs[cmd.CommandID]++
		mu.Unlock()
		return "", nil
	}
	c := newCommander(nil, commandRegistry{"reload-parameters": handler}, time.Second)
	ctx := t.Context()
	go c.serveSerial(ctx)

	first := rdsinstance.Command{CommandID: "cmd-1", Type: "reload-parameters"}
	c.dispatch(ctx, first)
	waitFor(t, func() bool { return len(c.pendingReplies()) == 1 }, "the first run's reply to be pending")

	c.dispatch(ctx, first)
	// The lane is FIFO, so once this one has answered a queued copy would have run.
	c.dispatch(ctx, rdsinstance.Command{CommandID: "cmd-2", Type: "reload-parameters"})
	waitFor(t, func() bool {
		return slices.ContainsFunc(c.pendingReplies(), func(r rdsinstance.CommandReply) bool { return r.CommandID == "cmd-2" })
	}, "the following command's reply")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, runs["cmd-1"], "a copy arriving before the reply was carried must not run")
	assert.Len(t, c.pendingReplies(), 2, "the copy must not be answered a second time either")
}

// A quiesce that waited out its issuer's budget behind another command would
// put the engine in backup mode for a snapshot nobody is taking.
func TestCommander_AStaleQuiesceNeverTakesAHold(t *testing.T) {
	t.Parallel()
	cp := newFakeControlPlane()
	cp.pollQueue = [][]rdsinstance.Command{
		{{CommandID: "cmd-apply", Type: rdsinstance.CommandApplyParams}},
		{{CommandID: "cmd-quiesce", Type: rdsinstance.CommandQuiesce, Parameters: []rdsinstance.Parameter{
			{Name: rdsinstance.CommandParamQuiesceLabel, Value: "orders-db-automated"},
			{Name: rdsinstance.CommandParamQuiesceDeadlineSeconds, Value: "360"},
		}}},
	}
	engine := &fakeEngine{}
	registry := newCommandRegistry(engine, &fakeStorage{})
	applying, release := make(chan struct{}), make(chan struct{})
	registry[rdsinstance.CommandApplyParams] = func(ctx context.Context, _ rdsinstance.Command) (string, error) {
		close(applying)
		<-release
		return "", nil
	}
	c := newCommander(cp, registry, 10*time.Millisecond)
	c.budget = func(commandType string) time.Duration {
		if commandType == rdsinstance.CommandQuiesce {
			return 50 * time.Millisecond
		}
		return controlPlaneBudget(commandType)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()

	<-applying
	waitFor(t, func() bool { return len(cp.snapshotReplies()) >= 3 }, "the quiesce to be delivered behind the apply")
	time.Sleep(100 * time.Millisecond)
	close(release)
	waitFor(t, func() bool { return len(repliesFor(cp, "cmd-quiesce")) > 0 }, "the quiesce to be answered")
	cancel()
	<-done

	reply := repliesFor(cp, "cmd-quiesce")[0]
	assert.Equal(t, rdsinstance.CommandStatusFailed, reply.Status)
	assert.Contains(t, reply.Message, "expired")
	assert.Empty(t, engine.label, "an expired quiesce must never reach the engine")
}

// Engine commands still run one at a time: a parameter apply rewrites the
// option file a graceful stop would otherwise race.
func TestCommander_AParameterApplyNeverRunsAlongsideAnEngineStop(t *testing.T) {
	t.Parallel()
	cp := newFakeControlPlane()
	cp.pollQueue = [][]rdsinstance.Command{
		{{CommandID: "cmd-apply", Type: rdsinstance.CommandApplyParams}},
		{{CommandID: "cmd-stop", Type: rdsinstance.CommandStopEngine}},
	}
	var mu sync.Mutex
	active, most := 0, 0
	release := make(chan struct{})
	// Each holds the lane until released, so two running at once are both seen
	// whichever is scheduled first.
	engineWork := func(context.Context, rdsinstance.Command) (string, error) {
		mu.Lock()
		active++
		most = max(most, active)
		mu.Unlock()
		<-release
		mu.Lock()
		active--
		mu.Unlock()
		return "", nil
	}
	registry := commandRegistry{
		rdsinstance.CommandApplyParams: engineWork,
		rdsinstance.CommandStopEngine:  engineWork,
	}
	c := newCommander(cp, registry, 10*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()

	waitFor(t, func() bool { return len(cp.snapshotReplies()) >= 3 }, "the stop to be delivered behind the apply")
	time.Sleep(50 * time.Millisecond)
	close(release)
	waitFor(t, func() bool { return len(repliesFor(cp, "cmd-stop")) > 0 }, "the stop to be answered")
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, most, "the engine stop ran while the parameter apply was still running")
}

// The agent bounds each command by its issuer's budget, so a type the control
// plane has no budget for would run on a guess.
func TestCommandRegistry_EveryTypeHasAControlPlaneBudget(t *testing.T) {
	for commandType := range newCommandRegistry(&fakeEngine{}, &fakeStorage{}) {
		if _, ok := rdsinstance.CommandBudget(commandType); !ok {
			t.Errorf("%s has no control-plane budget", commandType)
		}
	}
}

func (c *commander) pendingReplies() []rdsinstance.CommandReply {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.pending)
}

// The first copy can reach the agent late, after a gateway or bus outage, with
// its issuer about to give up. A quiesce granted a fresh budget then would take
// a hold nobody is left to release.
func TestCommander_AQuiesceStampedNearlyOutOfBudgetNeverTakesAHold(t *testing.T) {
	t.Parallel()
	cp := newFakeControlPlane()
	cp.pollQueue = [][]rdsinstance.Command{
		{{CommandID: "cmd-apply", Type: rdsinstance.CommandApplyParams}},
		{{CommandID: "cmd-quiesce", Type: rdsinstance.CommandQuiesce, RemainingBudgetMs: 50, Parameters: []rdsinstance.Parameter{
			{Name: rdsinstance.CommandParamQuiesceLabel, Value: "orders-db-automated"},
			{Name: rdsinstance.CommandParamQuiesceDeadlineSeconds, Value: "360"},
		}}},
	}
	engine := &fakeEngine{}
	registry := newCommandRegistry(engine, &fakeStorage{})
	applying, release := make(chan struct{}), make(chan struct{})
	registry[rdsinstance.CommandApplyParams] = func(context.Context, rdsinstance.Command) (string, error) {
		close(applying)
		<-release
		return "", nil
	}
	c := newCommander(cp, registry, 10*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()

	<-applying
	waitFor(t, func() bool { return len(cp.snapshotReplies()) >= 3 }, "the quiesce to be delivered behind the apply")
	// The quiesce was received before the third poll began, so its stamped budget
	// has run out 50ms after this point.
	queuedAt := time.Now()
	waitFor(t, func() bool { return time.Since(queuedAt) > 50*time.Millisecond }, "the stamped budget to pass")
	close(release)
	waitFor(t, func() bool { return len(repliesFor(cp, "cmd-quiesce")) > 0 }, "the quiesce to be answered")
	cancel()
	<-done

	reply := repliesFor(cp, "cmd-quiesce")[0]
	assert.Equal(t, rdsinstance.CommandStatusFailed, reply.Status)
	assert.Contains(t, reply.Message, "expired")
	assert.Empty(t, engine.label, "a quiesce whose issuer had all but given up must never reach the engine")
}

// A full lane refuses at once rather than queueing work its issuer will not
// outlive, and the refusal is the command's answer: a re-published copy must
// not slip in once the lane has room.
func TestCommander_AFullLaneRefusesAtOnceAndNeverRunsTheCommand(t *testing.T) {
	var mu sync.Mutex
	runs := map[string]int{}
	running, release := make(chan struct{}, 1), make(chan struct{})
	handler := func(_ context.Context, cmd rdsinstance.Command) (string, error) {
		mu.Lock()
		runs[cmd.CommandID]++
		mu.Unlock()
		if cmd.CommandID == "cmd-blocker" {
			running <- struct{}{}
			<-release
		}
		return "", nil
	}
	c := newCommander(nil, commandRegistry{"reload-parameters": handler}, time.Second)
	ctx := t.Context()
	go c.serveSerial(ctx)

	c.dispatch(ctx, rdsinstance.Command{CommandID: "cmd-blocker", Type: "reload-parameters"})
	<-running
	for i := range serialQueueDepth {
		c.dispatch(ctx, rdsinstance.Command{CommandID: fmt.Sprintf("cmd-queued-%d", i), Type: "reload-parameters"})
	}

	overflow := rdsinstance.Command{CommandID: "cmd-overflow", Type: "reload-parameters"}
	c.dispatch(ctx, overflow)
	refused := slices.IndexFunc(c.pendingReplies(), func(r rdsinstance.CommandReply) bool { return r.CommandID == overflow.CommandID })
	require.GreaterOrEqual(t, refused, 0, "a full lane must answer at once, before anything ahead of it finishes")
	assert.Equal(t, rdsinstance.CommandStatusFailed, c.pendingReplies()[refused].Status)
	assert.Contains(t, c.pendingReplies()[refused].Message, "busy")

	close(release)
	last := fmt.Sprintf("cmd-queued-%d", serialQueueDepth-1)
	waitFor(t, func() bool {
		return slices.ContainsFunc(c.pendingReplies(), func(r rdsinstance.CommandReply) bool { return r.CommandID == last })
	}, "the queue to drain")
	c.dispatch(ctx, overflow)
	// FIFO again: once this answers, a copy let onto the lane would have run.
	c.dispatch(ctx, rdsinstance.Command{CommandID: "cmd-after", Type: "reload-parameters"})
	waitFor(t, func() bool {
		return slices.ContainsFunc(c.pendingReplies(), func(r rdsinstance.CommandReply) bool { return r.CommandID == "cmd-after" })
	}, "the command after the re-published copy")

	mu.Lock()
	defer mu.Unlock()
	assert.Zero(t, runs[overflow.CommandID], "a refused command must never run")
	answers := 0
	for _, r := range c.pendingReplies() {
		if r.CommandID == overflow.CommandID {
			answers++
		}
	}
	assert.Equal(t, 1, answers, "a re-published copy of a refused command must be ignored, not answered again")
}
