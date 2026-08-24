//test:in-package — drives the unexported CAS primitives (casUpdate, casPut and
//the generic casClaim[T]) against maxCASRetries. A generic function cannot be
//re-exported through export_test.go without a wrapper per instantiation.

package daemon

import (
	"context"
	"fmt"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wrongLastSequence builds the error nats.go returns for a lost CAS race on a
// bucket whose stream reports the conflict under code. Single-replica streams
// report 10071, replicated ones 10164. Update and revision-guarded Delete wrap
// both with ErrKeyRevisionMismatch; only 10071 also satisfies ErrKeyExists, so
// a predicate written against ErrKeyExists alone is blind on a real cluster.
func wrongLastSequence(code jetstream.ErrorCode) error {
	apiErr := &jetstream.APIError{
		ErrorCode:   code,
		Code:        400,
		Description: "wrong last sequence: 3",
	}
	return fmt.Errorf("%w: %w", apiErr, jetstream.ErrKeyRevisionMismatch)
}

// casConflictKV forces the next failuresLeft writes of each kind to fail with a
// wrong-last-sequence response, so the CAS primitives can be driven against a
// replicated bucket's error shape without standing up a real cluster.
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

// conflictCodes names the two wrong-last-sequence codes a bucket can report, so
// every CAS test covers a replicated bucket as well as a single-replica one.
var conflictCodes = map[string]jetstream.ErrorCode{
	"single replica": jetstream.JSErrCodeStreamWrongLastSequence,
	"replicated":     jetstream.JSErrCodeStreamWrongLastSequenceConstant,
}

func casTestBucket(t *testing.T, name string) jetstream.KeyValue {
	t.Helper()
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: name, History: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = js.DeleteKeyValue(context.Background(), name) })
	return kv
}

type casRecord struct {
	Counter int `json:"counter"`
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

			stub := &casConflictKV{KeyValue: kv, conflictCode: code, updateFailures: maxCASRetries - 1}
			got, err := casUpdate(t.Context(), stub, "key", func(r *casRecord) { r.Counter++ }, false)
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

			stub := &casConflictKV{KeyValue: kv, conflictCode: code, updateFailures: maxCASRetries + 1}
			_, err = casUpdate(t.Context(), stub, "key", func(r *casRecord) { r.Counter++ }, false)
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

			stub := &casConflictKV{KeyValue: kv, conflictCode: code, updateFailures: maxCASRetries - 1}
			require.NoError(t, casPut(t.Context(), stub, "key", []byte(`{"counter":9}`), false))
			assert.Zero(t, stub.updateFailures, "all injected conflicts must have been consumed")

			entry, err := kv.Get(t.Context(), "key")
			require.NoError(t, err)
			assert.JSONEq(t, `{"counter":9}`, string(entry.Value()))
		})
	}
}

// casClaim's write is a revision-guarded Delete, which reports a lost race only
// as ErrKeyRevisionMismatch — ErrKeyExists never matches it on any replica count
// once the conflict carries the replicated code.
func TestCASClaim_RetriesRevisionConflict(t *testing.T) {
	for name, code := range conflictCodes {
		t.Run(name, func(t *testing.T) {
			kv := casTestBucket(t, "cas-claim-"+jetstreamCodeSuffix(code))
			_, err := kv.Put(t.Context(), "key", []byte(`{"counter":7}`))
			require.NoError(t, err)

			stub := &casConflictKV{KeyValue: kv, conflictCode: code, deleteFailures: maxCASRetries - 1}
			got, notFound, err := casClaim[casRecord](t.Context(), stub, "key")
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
