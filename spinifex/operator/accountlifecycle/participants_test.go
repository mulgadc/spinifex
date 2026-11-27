package accountlifecycle

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	accountlifecyclev1 "github.com/mulgadc/spinifex/contracts/accountlifecycle/v1"
	migrationv1 "github.com/mulgadc/spinifex/contracts/migration/v1"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/migration/accounttermination"
	"github.com/mulgadc/spinifex/spinifex/domains/migration/executor"
	migrationstore "github.com/mulgadc/spinifex/spinifex/domains/migration/store"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func classes(status string) []accountlifecyclev1.ClassReport {
	var out []accountlifecyclev1.ClassReport
	for _, c := range migrationv1.Declaration().Classes {
		out = append(out, accountlifecyclev1.ClassReport{Class: c, Status: status})
	}
	return out
}

// fakeParticipant answers with the migration declaration. cleanAfter is the
// command pass from which the inventory reads clean; zero never does.
type fakeParticipant struct {
	cleanAfter  int
	commands    int
	inventories int
	commandErr  error
	mutate      func(*accountlifecyclev1.InventoryReply)
	order       *[]string
}

func (f *fakeParticipant) Inventory(_ context.Context, req accountlifecyclev1.InventoryRequest) (*accountlifecyclev1.InventoryReply, error) {
	f.inventories++
	status := accountlifecyclev1.StatusPresent
	if f.cleanAfter > 0 && f.commands >= f.cleanAfter {
		status = accountlifecyclev1.StatusClean
	}
	reply := &accountlifecyclev1.InventoryReply{
		AccountID: req.AccountID, Participant: migrationv1.ParticipantName,
		DeclarationVersion: migrationv1.DeclarationVersion, ContractVersion: accountlifecyclev1.ContractVersion,
		Classes: classes(status),
	}
	if f.mutate != nil {
		f.mutate(reply)
	}
	return reply, nil
}

func (f *fakeParticipant) Terminate(_ context.Context, req accountlifecyclev1.TerminateRequest) (*accountlifecyclev1.TerminateReply, error) {
	f.commands++
	if f.order != nil {
		*f.order = append(*f.order, "participant")
	}
	if f.commandErr != nil {
		return nil, f.commandErr
	}
	return &accountlifecyclev1.TerminateReply{
		AccountID: req.AccountID, Participant: migrationv1.ParticipantName,
		DeclarationVersion: migrationv1.DeclarationVersion, ContractVersion: accountlifecyclev1.ContractVersion,
		Classes: classes(accountlifecyclev1.StatusPresent),
	}, nil
}

// fakeTime is the engine clock; each wait advances it instead of sleeping.
type fakeTime struct {
	now   time.Time
	waits int
}

func (c *fakeTime) install(e *Engine) {
	e.now = func() time.Time { return c.now }
	e.wait = func(_ context.Context, d time.Duration) error { c.waits++; c.now = c.now.Add(d); return nil }
}

func participantEngine(t *testing.T, accounts AccountStore, p ParticipantClient, reapers ...Reaper) (*Engine, *fakeTime) {
	t.Helper()
	e := quietEngine(accounts, reapers...)
	e.Timeouts.DrainPoll = 10 * time.Second
	e.Participants = []Participant{{Declaration: migrationv1.Declaration(), Client: p}}
	clock := &fakeTime{now: time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)}
	clock.install(e)
	return e, clock
}

func TestParticipantRunsAfterTerminatingAndBeforeEveryLegacyStage(t *testing.T) {
	accounts := newFakeAccounts()
	var order []string
	p := &fakeParticipant{cleanAfter: 1, order: &order}
	reaper := &fakeReaper{kind: "vpc", stage: StageNetwork, resources: []Resource{{Kind: "vpc", ID: "vpc-1"}}, order: &order}
	e, _ := participantEngine(t, accounts, p, reaper)
	e.Timeouts.DrainPoll = 5 * time.Millisecond

	result, err := e.Teardown(t.Context(), request())
	require.NoError(t, err)
	assert.True(t, result.AccountDeleted)
	assert.Equal(t, []string{"participant", "vpc/vpc-1"}, order)
	require.Len(t, result.Participants, 1)
	assert.True(t, result.Participants[0].Clean)
	assert.Equal(t, []string{AccountStatusTerminating}, accounts.statuses)
}

// The engine repeats the command until the participant reads clean, so an
// owner that stops after a few passes still lets the account go.
func TestParticipantIsRepeatedUntilClean(t *testing.T) {
	accounts := newFakeAccounts()
	p := &fakeParticipant{cleanAfter: 3}
	e, clock := participantEngine(t, accounts, p)

	result, err := e.Teardown(t.Context(), request())
	require.NoError(t, err)
	assert.True(t, accounts.deleted)
	assert.Equal(t, 3, p.commands)
	assert.Equal(t, 2, clock.waits)
	assert.Equal(t, 3, result.Participants[0].Passes)
}

// Verifies: ADR-0003:S7. A participant still not clean at the end of its
// drain budget keeps the account TERMINATING and runs no later stage.
func TestS7_BlockedParticipantKeepsTheAccount(t *testing.T) {
	accounts := newFakeAccounts()
	p := &fakeParticipant{}
	reaper := &fakeReaper{kind: "vpc", stage: StageNetwork, resources: []Resource{{Kind: "vpc", ID: "vpc-1"}}}
	e, clock := participantEngine(t, accounts, p, reaper)
	start := clock.now

	result, err := e.Teardown(t.Context(), request())
	require.ErrorIs(t, err, ErrParticipantBlocked)
	assert.False(t, accounts.deleted)
	assert.False(t, result.AccountDeleted)
	assert.Zero(t, reaper.deletesAttempted, "no legacy stage runs after a blocked participant")
	assert.False(t, clock.now.Before(start.Add(migrationv1.DrainBudget)), "the budget is spent before blocking")
	assert.Equal(t, "drain-budget-exhausted", result.Participants[0].Reason)
}

func TestParticipantWithoutResponderBlocks(t *testing.T) {
	accounts := newFakeAccounts()
	e, _ := participantEngine(t, accounts, &fakeParticipant{commandErr: nats.ErrNoResponders})
	result, err := e.Teardown(t.Context(), request())
	require.ErrorIs(t, err, ErrParticipantBlocked)
	assert.False(t, accounts.deleted)
	assert.Equal(t, "command-unanswered", result.Participants[0].Reason)
}

func TestParticipantReplyThatBreaksTheContractIsNeverClean(t *testing.T) {
	for name, mutate := range map[string]func(*accountlifecyclev1.InventoryReply){
		"wrong account":      func(r *accountlifecyclev1.InventoryReply) { r.AccountID = "999999999999" },
		"wrong participant":  func(r *accountlifecyclev1.InventoryReply) { r.Participant = "ec2" },
		"older declaration":  func(r *accountlifecyclev1.InventoryReply) { r.DeclarationVersion = 0 },
		"missing class":      func(r *accountlifecyclev1.InventoryReply) { r.Classes = r.Classes[1:] },
		"duplicated class":   func(r *accountlifecyclev1.InventoryReply) { r.Classes[1] = r.Classes[0] },
		"undeclared class":   func(r *accountlifecyclev1.InventoryReply) { r.Classes[0].Class = "bundle" },
		"unknown status":     func(r *accountlifecyclev1.InventoryReply) { r.Classes[0].Status = "gone" },
		"other contract ver": func(r *accountlifecyclev1.InventoryReply) { r.ContractVersion = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			accounts := newFakeAccounts()
			e, _ := participantEngine(t, accounts, &fakeParticipant{cleanAfter: 1, mutate: mutate})
			_, err := e.Teardown(t.Context(), request())
			require.ErrorIs(t, err, ErrParticipantBlocked)
			assert.False(t, accounts.deleted)
		})
	}
}

// flipParticipant is clean during the phase and not clean on the final read,
// as when a record appears after the phase ends.
type flipParticipant struct {
	fakeParticipant

	reads int
}

func (f *flipParticipant) Inventory(ctx context.Context, req accountlifecyclev1.InventoryRequest) (*accountlifecyclev1.InventoryReply, error) {
	f.reads++
	reply, err := f.fakeParticipant.Inventory(ctx, req)
	if f.reads > 1 {
		reply.Classes = classes(accountlifecyclev1.StatusPresent)
	}
	return reply, err
}

func TestFinalInventoryReadKeepsTheAccount(t *testing.T) {
	accounts := newFakeAccounts()
	e, _ := participantEngine(t, accounts, &flipParticipant{fakeParticipant: fakeParticipant{cleanAfter: 1}})
	_, err := e.Teardown(t.Context(), request())
	require.ErrorIs(t, err, ErrParticipantBlocked)
	assert.False(t, accounts.deleted)
}

func TestInventoryReportsParticipantsWithoutACommand(t *testing.T) {
	p := &fakeParticipant{}
	e, _ := participantEngine(t, newFakeAccounts(), p)
	result, err := e.Inventory(t.Context(), testAccountID)
	require.NoError(t, err)
	require.Len(t, result.Participants, 1)
	assert.False(t, result.Participants[0].Clean)
	assert.Zero(t, p.commands)
}

func TestComposeRefusesADeclarationItCannotServe(t *testing.T) {
	good := migrationv1.Declaration()
	for name, mutate := range map[string]func(*accountlifecyclev1.Declaration){
		"subject wildcard": func(d *accountlifecyclev1.Declaration) { d.Name = "mig.*" },
		"no classes":       func(d *accountlifecyclev1.Declaration) { d.Classes = nil },
		"contract version": func(d *accountlifecyclev1.Declaration) { d.CommandContractVersion = 2 },
		"no drain budget":  func(d *accountlifecyclev1.Declaration) { d.DrainBudget = 0 },
	} {
		d := good
		mutate(&d)
		_, err := composeParticipants(nil, []accountlifecyclev1.Declaration{d})
		require.Error(t, err, name)
	}
	ps, err := composeParticipants(nil, participantDeclarations())
	require.NoError(t, err)
	require.Len(t, ps, 1)
	assert.Equal(t, migrationv1.ParticipantName, ps[0].Declaration.Name)
}

type terminatingAccount struct{}

func (terminatingAccount) AccountTerminating(context.Context, string) (bool, error) { return true, nil }

// serveMigration answers the participant routes from one node, as the daemon
// does in the worker queue group.
func serveMigration(t *testing.T, nc *nats.Conn, p *accounttermination.Participant) {
	t.Helper()
	serve := func(subject string, f func(context.Context, []byte) (any, error)) {
		sub, err := nc.QueueSubscribe(subject, "spinifex-workers", func(m *nats.Msg) {
			out, err := f(context.Background(), m.Data)
			require.NoError(t, err)
			data, err := json.Marshal(out)
			require.NoError(t, err)
			require.NoError(t, m.Respond(data))
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}
	serve(accountlifecyclev1.InventorySubject(migrationv1.ParticipantName), func(ctx context.Context, b []byte) (any, error) {
		var req accountlifecyclev1.InventoryRequest
		if err := json.Unmarshal(b, &req); err != nil {
			return nil, err
		}
		return p.Inventory(ctx, &req, "")
	})
	serve(accountlifecyclev1.TerminateSubject(migrationv1.ParticipantName), func(ctx context.Context, b []byte) (any, error) {
		var req accountlifecyclev1.TerminateRequest
		if err := json.Unmarshal(b, &req); err != nil {
			return nil, err
		}
		return p.Terminate(ctx, &req, "")
	})
}

// A termination started mid rolling upgrade reaches the participant through
// whichever node already runs it, drains every record, and deletes the account
// once a job owner on another upgraded node records that it stopped.
func TestTerminationMidRollingUpgradeStillDrains(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	s, err := migrationstore.OpenOrCreate(t.Context(), js, testAccountID)
	require.NoError(t, err)
	for _, r := range []migrationstore.Record{{Kind: "connection", ID: "c-1"}, {Kind: "copy-task", ID: "ct-1"}} {
		r.AccountID = testAccountID
		_, err := s.Create(t.Context(), r)
		require.NoError(t, err)
	}
	_, err = s.Claim(t.Context(), "copy-task", "ct-1", "n3")
	require.NoError(t, err)

	upgraded, err := executor.New(js, "n2", executor.NoObjects{}, nil)
	require.NoError(t, err)
	part, err := accounttermination.New(js, terminatingAccount{}, upgraded, executor.NoObjects{}, nil, nil)
	require.NoError(t, err)
	serveMigration(t, nc, part)

	ps, err := composeParticipants(nc, participantDeclarations())
	require.NoError(t, err)
	accounts := newFakeAccounts()
	e := quietEngine(accounts)
	e.Timeouts.DrainPoll = 10 * time.Second
	e.Participants = ps
	clock := &fakeTime{now: time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)}
	e.now = func() time.Time { return clock.now }
	e.wait = func(_ context.Context, d time.Duration) error {
		clock.waits++
		clock.now = clock.now.Add(d)
		if clock.waits == 2 {
			_, err := s.RecordStopped(context.Background(), "copy-task", "ct-1", "n3")
			return err
		}
		return nil
	}

	result, err := e.Teardown(t.Context(), request())
	require.NoError(t, err)
	assert.True(t, accounts.deleted)
	assert.Equal(t, 3, result.Participants[0].Passes)
	_, err = migrationstore.Open(t.Context(), js, testAccountID)
	require.ErrorIs(t, err, migrationstore.ErrNotFound)
}

// With no node yet serving the participant, the run blocks and keeps the
// account instead of deleting it past records nobody drained.
func TestTerminationWithNoUpgradedNodeKeepsTheAccount(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	ps, err := composeParticipants(nc, participantDeclarations())
	require.NoError(t, err)
	accounts := newFakeAccounts()
	e := quietEngine(accounts)
	e.Timeouts.DrainPoll = 10 * time.Second
	e.Participants = ps
	(&fakeTime{now: time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)}).install(e)

	_, err = e.Teardown(t.Context(), request())
	require.ErrorIs(t, err, ErrParticipantBlocked)
	assert.False(t, accounts.deleted)
	assert.NotErrorIs(t, err, ErrResourcesStuck)
}
