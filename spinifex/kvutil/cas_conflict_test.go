package kvutil_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const casTestAttempts = 5

func wrongLastSequence(code jetstream.ErrorCode) error {
	apiErr := &jetstream.APIError{
		ErrorCode:   code,
		Code:        400,
		Description: "wrong last sequence: 3",
	}
	return fmt.Errorf("%w: %w", apiErr, jetstream.ErrKeyRevisionMismatch)
}

type casConflictKV struct {
	jetstream.KeyValue

	conflictCode   jetstream.ErrorCode
	updateFailures int
	deleteFailures int
}

func (k *casConflictKV) Update(ctx context.Context, key string, value []byte, last uint64) (uint64, error) {
	if k.updateFailures > 0 {
		k.updateFailures--
		return 0, wrongLastSequence(k.conflictCode)
	}
	return k.KeyValue.Update(ctx, key, value, last)
}

func (k *casConflictKV) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	if k.deleteFailures > 0 {
		k.deleteFailures--
		return wrongLastSequence(k.conflictCode)
	}
	return k.KeyValue.Delete(ctx, key, opts...)
}

// conflictCodes names the two wrong-last-sequence codes a bucket can report, so every CAS test covers a replicated bucket as well as single-replica one.
var conflictCodes = map[string]jetstream.ErrorCode{
	"single replica": jetstream.JSErrCodeStreamWrongLastSequence,
	"replicated":     jetstream.JSErrCodeStreamWrongLastSequenceConstant,
}

func casTestBucket(t *testing.T, name string) jetstream.KeyValue {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)

	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: name, History: 1})
	require.NoError(t, err)
	return kv
}

type casRecord struct {
	Counter int `json:"counter"`
}

func bumpCounter(r *casRecord) (bool, error) {
	r.Counter++
	return true, nil
}

// A lost Update race must be retried on both replica counts. Matching only
// jetstream.ErrKeyExists passes the single-replica case and surfaces a raw
// "wrong last sequence" error on the replicated one.
func TestCASUpdate_RetriesRevisionConflict(t *testing.T) {
	for name, code := range conflictCodes {
		t.Run(name, func(t *testing.T) {
			kv := casTestBucket(t, "cas-update-"+jetstreamCodeSuffix(code))
			_, err := kv.Put(t.Context(), "key", []byte(`{"counter":1}`))
			require.NoError(t, err)

			stub := &casConflictKV{KeyValue: kv, conflictCode: code, updateFailures: casTestAttempts - 1}
			got, err := kvutil.Update(t.Context(), stub, "key", kvutil.CASConfig{Attempts: casTestAttempts}, bumpCounter)
			require.NoError(t, err)
			assert.Equal(t, 2, got.Counter)
			assert.Zero(t, stub.updateFailures, "all injected conflicts must have been consumed")
		})
	}
}

func TestCASUpdate_ExhaustsRetriesOnSustainedConflict(t *testing.T) {
	for name, code := range conflictCodes {
		t.Run(name, func(t *testing.T) {
			kv := casTestBucket(t, "cas-exhaust-"+jetstreamCodeSuffix(code))
			_, err := kv.Put(t.Context(), "key", []byte(`{"counter":1}`))
			require.NoError(t, err)

			stub := &casConflictKV{KeyValue: kv, conflictCode: code, updateFailures: casTestAttempts + 1}
			_, err = kvutil.Update(t.Context(), stub, "key", kvutil.CASConfig{Attempts: casTestAttempts}, bumpCounter)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "CAS update exhausted")
			assert.ErrorIs(t, err, jetstream.ErrKeyRevisionMismatch)
		})
	}
}

func TestCASPut_RetriesRevisionConflict(t *testing.T) {
	for name, code := range conflictCodes {
		t.Run(name, func(t *testing.T) {
			kv := casTestBucket(t, "cas-put-"+jetstreamCodeSuffix(code))
			_, err := kv.Put(t.Context(), "key", []byte(`{"counter":1}`))
			require.NoError(t, err)

			stub := &casConflictKV{KeyValue: kv, conflictCode: code, updateFailures: casTestAttempts - 1}
			require.NoError(t, kvutil.Put(t.Context(), stub, "key", kvutil.CASConfig{Attempts: casTestAttempts}, []byte(`{"counter":9}`)))
			assert.Zero(t, stub.updateFailures, "all injected conflicts must have been consumed")

			entry, err := kv.Get(t.Context(), "key")
			require.NoError(t, err)
			assert.JSONEq(t, `{"counter":9}`, string(entry.Value()))
		})
	}
}

// Claim's write is a revision-guarded Delete, which reports a lost race only
// as ErrKeyRevisionMismatch — ErrKeyExists never matches it on a
// once the conflict carries the replicated code.
func TestCASClaim_RetriesRevisionConflict(t *testing.T) {
	for name, code := range conflictCodes {
		t.Run(name, func(t *testing.T) {
			kv := casTestBucket(t, "cas-claim-"+jetstreamCodeSuffix(code))
			_, err := kv.Put(t.Context(), "key", []byte(`{"counter":7}`))
			require.NoError(t, err)

			stub := &casConflictKV{KeyValue: kv, conflictCode: code, deleteFailures: casTestAttempts - 1}
			got, notFound, err := kvutil.Claim[casRecord](t.Context(), stub, "key", kvutil.CASConfig{Attempts: casTestAttempts})
			require.NoError(t, err)
			require.False(t, notFound)
			require.NotNil(t, got)
			assert.Equal(t, 7, got.Counter)
			assert.Zero(t, stub.deleteFailures, "all injected conflicts must have been consumed")

			_, err = kv.Get(t.Context(), "key")
			assert.ErrorIs(t, err, jetstream.ErrKeyNotFound, "the claim must have removed the key")
		})
	}
}

func jetstreamCodeSuffix(code jetstream.ErrorCode) string {
	return fmt.Sprintf("%d", code)
}

func TestUpdate_AbsentKeyReturnsCallerError(t *testing.T) {
	kv := casTestBucket(t, "cas-notfound")

	sentinel := errors.New("no such entity")
	_, err := kvutil.Update(t.Context(), kv, "missing", kvutil.CASConfig{NotFound: sentinel},
		func(*casRecord) (bool, error) { t.Fatal("mutate ran against an absent key"); return false, nil })

	assert.ErrorIs(t, err, sentinel)
}

func TestUpdate_ExhaustionUsesCallerError(t *testing.T) {
	kv := casTestBucket(t, "cas-exhausted-hook")
	_, err := kv.Put(t.Context(), "key", []byte(`{"counter":1}`))
	require.NoError(t, err)

	stub := &casConflictKV{KeyValue: kv, conflictCode: jetstream.JSErrCodeStreamWrongLastSequenceConstant,
		updateFailures: casTestAttempts + 1}
	sentinel := errors.New("contended")
	_, err = kvutil.Update(t.Context(), stub, "key", kvutil.CASConfig{
		Attempts:  casTestAttempts,
		Exhausted: func(string, int) error { return sentinel },
	}, bumpCounter)

	assert.ErrorIs(t, err, sentinel)
}
