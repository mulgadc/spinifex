package accounttermination

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	accountlifecyclev1 "github.com/mulgadc/spinifex/contracts/accountlifecycle/v1"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/migration/admission"
	"github.com/mulgadc/spinifex/spinifex/domains/migration/executor"
	"github.com/mulgadc/spinifex/spinifex/domains/migration/store"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const acct = "111122223333"

var t0 = time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)

type terminating map[string]bool

func (m terminating) AccountTerminating(_ context.Context, id string) (bool, error) {
	return m[id], nil
}

// objects records every prefix deleted; failNext fails that many calls.
type objects struct {
	mu       sync.Mutex
	deleted  []string
	failNext int
}

func (o *objects) DeletePrefix(_ context.Context, prefix string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.failNext > 0 {
		o.failNext--
		return errors.New("object store unavailable")
	}
	o.deleted = append(o.deleted, prefix)
	return nil
}

func (o *objects) prefixes() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.deleted...)
}

type cache struct {
	mu       sync.Mutex
	accounts map[string]bool
	sweeps   int
}

func (c *cache) Sweep() { c.mu.Lock(); c.sweeps++; c.mu.Unlock() }
func (c *cache) Accounts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for a := range c.accounts {
		out = append(out, a)
	}
	return out
}
func (c *cache) DropAccount(a string) { c.mu.Lock(); delete(c.accounts, a); c.mu.Unlock() }
func (c *cache) holds(a string) bool  { c.mu.Lock(); defer c.mu.Unlock(); return c.accounts[a] }

type node struct {
	exec *executor.Executor
	part *Participant
	objs *objects
	sess *cache
}

func newNode(t *testing.T, js jetstream.JetStream, id string) *node {
	t.Helper()
	n := &node{objs: &objects{}, sess: &cache{accounts: map[string]bool{acct: true, "444455556666": true}}}
	var err error
	n.exec, err = executor.New(js, id, n.objs, n.sess)
	require.NoError(t, err)
	n.part, err = New(js, terminating{acct: true}, n.exec, n.objs, n.sess, func() time.Time { return t0 })
	require.NoError(t, err)
	return n
}

func seed(t *testing.T, js jetstream.JetStream, recs ...store.Record) *store.Store {
	t.Helper()
	s, err := store.OpenOrCreate(t.Context(), js, acct)
	require.NoError(t, err)
	for _, r := range recs {
		r.AccountID = acct
		_, err := s.Create(t.Context(), r)
		require.NoError(t, err)
	}
	return s
}

func claimed(t *testing.T, s *store.Store, kind, id, by string) {
	t.Helper()
	_, err := s.Claim(t.Context(), kind, id, by)
	require.NoError(t, err)
}

func terminate(t *testing.T, p *Participant) *accountlifecyclev1.TerminateReply {
	t.Helper()
	reply, err := p.Terminate(t.Context(), &accountlifecyclev1.TerminateRequest{AccountID: acct}, "")
	require.NoError(t, err)
	return reply
}

func inventory(t *testing.T, p *Participant) map[string]accountlifecyclev1.ClassReport {
	t.Helper()
	reply, err := p.Inventory(t.Context(), &accountlifecyclev1.InventoryRequest{AccountID: acct}, "")
	require.NoError(t, err)
	require.Equal(t, acct, reply.AccountID)
	require.Equal(t, "migration", reply.Participant)
	out := map[string]accountlifecyclev1.ClassReport{}
	for _, c := range reply.Classes {
		out[c.Class] = c
	}
	require.Len(t, out, 3)
	return out
}

func allClean(classes []accountlifecyclev1.ClassReport) bool {
	for _, c := range classes {
		if c.Status != accountlifecyclev1.StatusClean {
			return false
		}
	}
	return len(classes) == 3
}

func bucketGone(t *testing.T, js jetstream.JetStream) bool {
	t.Helper()
	_, err := store.Open(t.Context(), js, acct)
	return errors.Is(err, store.ErrNotFound)
}

// Verifies: ADR-0003:S7. Termination deletes only the migration records of
// the terminated account: every record, its objects, then the bucket.
func TestS7_TerminationDrainsEveryRecord(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	n := newNode(t, js, "n1")
	seed(t, js,
		store.Record{Kind: "connection", ID: "c-1"}, store.Record{Kind: "connection", ID: "c-2"},
		store.Record{Kind: "assessment", ID: "a-1"}, store.Record{Kind: "copy-task", ID: "ct-1"})
	other, err := store.OpenOrCreate(t.Context(), js, "444455556666")
	require.NoError(t, err)
	_, err = other.Create(t.Context(), store.Record{Kind: "connection", ID: "c-1", AccountID: "444455556666"})
	require.NoError(t, err)

	reply := terminate(t, n.part)
	assert.True(t, allClean(reply.Classes), "%+v", reply.Classes)
	assert.Empty(t, reply.Blocked)
	assert.True(t, bucketGone(t, js))
	assert.ElementsMatch(t, []string{
		"migration/111122223333/connection/c-1/", "migration/111122223333/connection/c-2/",
		"migration/111122223333/assessment/a-1/", "migration/111122223333/copy-task/ct-1/",
		"migration/111122223333/",
	}, n.objs.prefixes())
	for _, c := range inventory(t, n.part) {
		assert.Equal(t, accountlifecyclev1.StatusClean, c.Status)
	}
	sealed, err := admission.NewSeals(js).Sealed(t.Context(), acct)
	require.NoError(t, err)
	assert.True(t, sealed, "the seal outlives the bucket")
	assert.False(t, n.sess.holds(acct), "the account's cached sessions are dropped")
	assert.True(t, n.sess.holds("444455556666"))

	_, err = other.Get(t.Context(), "connection", "c-1")
	require.NoError(t, err, "another account's record is untouched")
}

// Verifies: ADR-0003:S8. The command marks and returns while an executor
// still owns a job; it never waits for that executor.
func TestS8_TerminationReturnsBeforeTheOwnerDrains(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	a, b := newNode(t, js, "n1"), newNode(t, js, "n2")
	s := seed(t, js, store.Record{Kind: "copy-task", ID: "ct-1"}, store.Record{Kind: "connection", ID: "c-1"})
	claimed(t, s, "copy-task", "ct-1", "n2")

	reply := terminate(t, a.part)
	assert.Equal(t, []accountlifecyclev1.BlockedItem{{Class: "copy-task", Reason: accountlifecyclev1.ReasonOwnerNotStopped, Count: 1}}, reply.Blocked)
	inv := inventory(t, a.part)
	assert.Equal(t, accountlifecyclev1.StatusPresent, inv["copy-task"].Status)
	assert.Equal(t, 1, inv["copy-task"].Remaining)
	assert.Equal(t, accountlifecyclev1.StatusPresent, inv["connection"].Status, "the bucket is not clean until it is gone")
	assert.Zero(t, inv["connection"].Remaining)

	e, err := s.Get(t.Context(), "copy-task", "ct-1")
	require.NoError(t, err)
	assert.True(t, e.Record.DeleteRequested)
	assert.NotContains(t, a.objs.prefixes(), "migration/111122223333/copy-task/ct-1/", "nothing of an owned record is deleted")

	_, err = s.RecordStopped(t.Context(), "copy-task", "ct-1", "n2")
	require.NoError(t, err)
	require.NoError(t, b.exec.Sweep(t.Context()))
	_, err = s.Get(t.Context(), "copy-task", "ct-1")
	require.ErrorIs(t, err, store.ErrNotFound, "the next sweep deletes once the owner stopped")

	assert.True(t, allClean(terminate(t, a.part).Classes))
	assert.True(t, bucketGone(t, js))
}

// Verifies: ADR-0003:S7. A delete that stops part way keeps DeleteRequested,
// and the next sweep on any node resumes it.
func TestS7_InterruptedDeleteResumes(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	a, b := newNode(t, js, "n1"), newNode(t, js, "n2")
	s := seed(t, js, store.Record{Kind: "assessment", ID: "a-1"}, store.Record{Kind: "assessment", ID: "a-2"})
	a.objs.failNext = 1

	reply := terminate(t, a.part)
	assert.False(t, allClean(reply.Classes))
	l, err := s.List(t.Context())
	require.NoError(t, err)
	require.Len(t, l.Entries, 1, "one record lost its object delete and stays")
	assert.True(t, l.Entries[0].Record.DeleteRequested)

	require.NoError(t, b.exec.Sweep(t.Context()))
	l, err = s.List(t.Context())
	require.NoError(t, err)
	assert.Empty(t, l.Entries)
	assert.True(t, allClean(terminate(t, a.part).Classes))
}

// A record marked by an earlier pass whose node then died is resumed by a
// sweep alone, with no new termination command.
func TestS7_SweepResumesAMarkedRecordWithoutTheCommand(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	b := newNode(t, js, "n2")
	s := seed(t, js, store.Record{Kind: "connection", ID: "c-1"}, store.Record{Kind: "connection", ID: "c-2"})
	_, err := s.MarkDeleteRequested(t.Context(), "connection", "c-1", t0)
	require.NoError(t, err)

	require.NoError(t, b.exec.Sweep(t.Context()))
	_, err = s.Get(t.Context(), "connection", "c-1")
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.Get(t.Context(), "connection", "c-2")
	require.NoError(t, err, "a sweep never deletes an unmarked record")
}

// An executor restarted on the owning node holds no job, so its own claim is
// stale: it records it stopped and the drain continues.
func TestDrain_StaleOwnClaimIsStoppedAndDeleted(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	s := seed(t, js, store.Record{Kind: "copy-task", ID: "ct-1"})
	claimed(t, s, "copy-task", "ct-1", "n1")
	_, err := s.MarkDeleteRequested(t.Context(), "copy-task", "ct-1", t0)
	require.NoError(t, err)

	other := newNode(t, js, "n2")
	require.NoError(t, other.exec.Sweep(t.Context()))
	_, err = s.Get(t.Context(), "copy-task", "ct-1")
	require.NoError(t, err, "another node never stops a claim it does not hold")

	restarted := newNode(t, js, "n1")
	require.NoError(t, restarted.exec.Sweep(t.Context()))
	_, err = s.Get(t.Context(), "copy-task", "ct-1")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestTerminate_RefusesAnAccountNotTerminating(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	exec, err := executor.New(js, "n1", &objects{}, nil)
	require.NoError(t, err)
	p, err := New(js, terminating{}, exec, &objects{}, nil, nil)
	require.NoError(t, err)
	s := seed(t, js, store.Record{Kind: "connection", ID: "c-1"})

	_, err = p.Terminate(t.Context(), &accountlifecyclev1.TerminateRequest{AccountID: acct}, "")
	require.Error(t, err)
	e, err := s.Get(t.Context(), "connection", "c-1")
	require.NoError(t, err)
	assert.False(t, e.Record.DeleteRequested, "a refused command marks nothing")
	sealed, err := admission.NewSeals(js).Sealed(t.Context(), acct)
	require.NoError(t, err)
	assert.False(t, sealed)

	for _, bad := range []string{"", "000000000000", "12"} {
		_, err = p.Terminate(t.Context(), &accountlifecyclev1.TerminateRequest{AccountID: bad}, "")
		require.Error(t, err, bad)
		_, err = p.Inventory(t.Context(), &accountlifecyclev1.InventoryRequest{AccountID: bad}, "")
		require.Error(t, err, bad)
	}
}

func TestInventory_NeverReportsCleanForAnUnreadableScan(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	n := newNode(t, js, "n1")
	for _, c := range inventory(t, n.part) {
		assert.Equal(t, accountlifecyclev1.StatusClean, c.Status, "an account that never used migration is clean")
	}

	seed(t, js, store.Record{Kind: "connection", ID: "c-1"})
	kv, err := js.KeyValue(t.Context(), store.BucketName(acct))
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), "assessment.a-1", []byte("{broken"))
	require.NoError(t, err)

	inv := inventory(t, n.part)
	assert.Equal(t, accountlifecyclev1.StatusPresent, inv["connection"].Status)
	assert.Equal(t, 1, inv["connection"].Remaining)
	assert.Equal(t, accountlifecyclev1.StatusIncomplete, inv["assessment"].Status)
	assert.Equal(t, []string{accountlifecyclev1.ReasonUnreadableRecord}, inv["assessment"].Reasons)

	reply := terminate(t, n.part)
	assert.False(t, allClean(reply.Classes), "an unreadable record keeps the bucket")
	assert.False(t, bucketGone(t, js))

	_, err = kv.Put(t.Context(), "bundle.b-1", []byte("{}"))
	require.NoError(t, err)
	for _, c := range inventory(t, n.part) {
		assert.Equal(t, accountlifecyclev1.StatusIncomplete, c.Status, "a key of no declared kind fails every class closed")
	}
}

func TestSweep_ExpiresSessionsAndDropsSealedAccounts(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	n := newNode(t, js, "n1")
	require.NoError(t, n.exec.Sweep(t.Context()))
	assert.Equal(t, 1, n.sess.sweeps)
	assert.True(t, n.sess.holds(acct))

	require.NoError(t, admission.NewSeals(js).Seal(t.Context(), terminating{acct: true}, acct, t0))
	require.NoError(t, n.exec.Sweep(t.Context()))
	assert.False(t, n.sess.holds(acct), "a node that did not serve the command still drops the account")
	assert.True(t, n.sess.holds("444455556666"))
}

func TestRun_SweepsOncePerTickAndStopsWithItsContext(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	n := newNode(t, js, "n1")
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { n.exec.Run(ctx, ticks); close(done) }()

	ticks <- t0
	ticks <- t0
	cancel()
	<-done
	assert.GreaterOrEqual(t, n.sess.sweeps, 1)
	assert.LessOrEqual(t, n.sess.sweeps, 2)
}
