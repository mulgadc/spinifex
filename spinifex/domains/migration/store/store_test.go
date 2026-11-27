package store

import (
	"testing"
	"time"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func kvConfig(name string) jetstream.KeyValueConfig {
	return jetstream.KeyValueConfig{Bucket: name, History: 1}
}

const acct = "111122223333"

var t0 = time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)

func newStore(t *testing.T) *Store {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	s, err := OpenOrCreate(t.Context(), js, acct)
	require.NoError(t, err)
	return s
}

func TestOpen_NeverCreatesTheBucket(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	_, err := Open(t.Context(), js, acct)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = js.KeyValue(t.Context(), BucketName(acct))
	require.Error(t, err, "a reader does not bring a bucket into existence")

	_, err = Open(t.Context(), js, "not-an-account")
	require.Error(t, err)
}

func TestCreate_KeysByKindAndIDAndRefusesASecondCreate(t *testing.T) {
	s := newStore(t)
	rev, err := s.Create(t.Context(), Record{Kind: "copy-task", ID: "ct-1", AccountID: acct, Body: []byte(`{"x":1}`)})
	require.NoError(t, err)
	require.NotZero(t, rev)

	raw, err := s.kv.Get(t.Context(), "copy-task.ct-1")
	require.NoError(t, err)
	assert.JSONEq(t, `{"schema_version":1,"kind":"copy-task","id":"ct-1","account_id":"111122223333","body":{"x":1}}`, string(raw.Value()))

	_, err = s.Create(t.Context(), Record{Kind: "copy-task", ID: "ct-1", AccountID: acct})
	require.ErrorIs(t, err, ErrConflict)
}

func TestCreate_RefusesBadKindIDOrAccount(t *testing.T) {
	s := newStore(t)
	for name, rec := range map[string]Record{
		"unknown kind":  {Kind: "bundle", ID: "b-1", AccountID: acct},
		"dotted id":     {Kind: "connection", ID: "a.b", AccountID: acct},
		"empty id":      {Kind: "connection", ID: "", AccountID: acct},
		"other account": {Kind: "connection", ID: "c-1", AccountID: "444455556666"},
	} {
		_, err := s.Create(t.Context(), rec)
		require.Error(t, err, name)
	}
}

func TestMarkDeleteRequested_IsIdempotentAndKeepsTheBody(t *testing.T) {
	s := newStore(t)
	_, err := s.Create(t.Context(), Record{Kind: "connection", ID: "c-1", AccountID: acct, Body: []byte(`{"name":"prod"}`)})
	require.NoError(t, err)

	first, err := s.MarkDeleteRequested(t.Context(), "connection", "c-1", t0)
	require.NoError(t, err)
	again, err := s.MarkDeleteRequested(t.Context(), "connection", "c-1", t0.Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, first.Revision, again.Revision, "a second mark writes nothing")
	assert.Equal(t, t0, *again.Record.DeleteRequestedAt)
	assert.JSONEq(t, `{"name":"prod"}`, string(again.Record.Body))

	_, err = s.MarkDeleteRequested(t.Context(), "connection", "gone", t0)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestClaim_RefusesAMarkedRecordAndAnotherNodesClaim(t *testing.T) {
	s := newStore(t)
	for _, id := range []string{"a-1", "a-2"} {
		_, err := s.Create(t.Context(), Record{Kind: "assessment", ID: id, AccountID: acct})
		require.NoError(t, err)
	}
	_, err := s.Claim(t.Context(), "assessment", "a-1", "n1")
	require.NoError(t, err)
	_, err = s.Claim(t.Context(), "assessment", "a-1", "n2")
	require.ErrorIs(t, err, ErrClaimed)

	_, err = s.MarkDeleteRequested(t.Context(), "assessment", "a-2", t0)
	require.NoError(t, err)
	_, err = s.Claim(t.Context(), "assessment", "a-2", "n1")
	require.ErrorIs(t, err, ErrDeleteRequested, "an executor never claims a record marked for deletion")

	e, err := s.RecordStopped(t.Context(), "assessment", "a-1", "n2")
	require.NoError(t, err)
	assert.False(t, e.Record.Owner.Stopped, "only the owner records that it stopped")
	e, err = s.RecordStopped(t.Context(), "assessment", "a-1", "n1")
	require.NoError(t, err)
	assert.True(t, e.Record.Owner.Stopped)
}

func TestMutate_RefusesARecordFromANewerBuild(t *testing.T) {
	s := newStore(t)
	_, err := s.kv.Create(t.Context(), "connection.c-9", []byte(`{"schema_version":2,"kind":"connection","id":"c-9","account_id":"111122223333","future":true}`))
	require.NoError(t, err)
	_, err = s.MarkDeleteRequested(t.Context(), "connection", "c-9", t0)
	require.ErrorIs(t, err, ErrNewerSchema)
	raw, err := s.kv.Get(t.Context(), "connection.c-9")
	require.NoError(t, err)
	assert.Contains(t, string(raw.Value()), `"future":true`, "this build does not rewrite a newer record")
}

func TestDelete_RefusesARecordThatChangedSinceItWasRead(t *testing.T) {
	s := newStore(t)
	rev, err := s.Create(t.Context(), Record{Kind: "connection", ID: "c-1", AccountID: acct})
	require.NoError(t, err)
	_, err = s.MarkDeleteRequested(t.Context(), "connection", "c-1", t0)
	require.NoError(t, err)

	require.ErrorIs(t, s.Delete(t.Context(), "connection", "c-1", rev), ErrConflict)
	e, err := s.Get(t.Context(), "connection", "c-1")
	require.NoError(t, err)
	require.NoError(t, s.Delete(t.Context(), "connection", "c-1", e.Revision))
	_, err = s.Get(t.Context(), "connection", "c-1")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestList_ReportsUnreadableAndUnknownKeys(t *testing.T) {
	s := newStore(t)
	_, err := s.Create(t.Context(), Record{Kind: "connection", ID: "c-1", AccountID: acct})
	require.NoError(t, err)
	_, err = s.kv.Put(t.Context(), "assessment.a-1", []byte("not json"))
	require.NoError(t, err)
	_, err = s.kv.Put(t.Context(), "bundle.b-1", []byte("{}"))
	require.NoError(t, err)

	l, err := s.List(t.Context())
	require.NoError(t, err)
	require.Len(t, l.Entries, 1)
	assert.Equal(t, 1, l.Unreadable["assessment"])
	assert.Equal(t, 1, l.Unknown)
}

func TestAccountIDs_ListsOnlyMigrationBuckets(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	for _, a := range []string{"111122223333", "444455556666"} {
		_, err := OpenOrCreate(t.Context(), js, a)
		require.NoError(t, err)
	}
	_, err := js.CreateKeyValue(t.Context(), kvConfig("spinifex-migration-seals"))
	require.NoError(t, err)
	_, err = js.CreateKeyValue(t.Context(), kvConfig("spinifex-other"))
	require.NoError(t, err)

	got, err := AccountIDs(t.Context(), js)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"111122223333", "444455556666"}, got)

	require.NoError(t, DeleteBucket(t.Context(), js, "111122223333"))
	require.NoError(t, DeleteBucket(t.Context(), js, "111122223333"), "deleting an absent bucket is success")
	got, err = AccountIDs(t.Context(), js)
	require.NoError(t, err)
	assert.Equal(t, []string{"444455556666"}, got)
}
