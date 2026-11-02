package queue

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	acct   = "111122223333"
	region = "ap-southeast-2"
)

var errLostReply = errors.New("nats: timeout")

// faultKV wraps the real bucket. A hook returning handled=true replaces the
// operation's result; it may still call the real operation first.
type faultKV struct {
	real kvOps
	mu   sync.Mutex

	onGet    func(key string) ([]byte, uint64, error, bool)
	onCreate func(key string, value []byte) (uint64, error, bool)
	onUpdate func(key string, value []byte, rev uint64) (uint64, error, bool)
	onKeys   func(filter string) ([]string, error, bool)
}

func (f *faultKV) get(ctx context.Context, key string) ([]byte, uint64, error) {
	f.mu.Lock()
	hook := f.onGet
	f.mu.Unlock()
	if hook != nil {
		if v, rev, err, ok := hook(key); ok {
			return v, rev, err
		}
	}
	return f.real.get(ctx, key)
}

func (f *faultKV) create(ctx context.Context, key string, value []byte) (uint64, error) {
	f.mu.Lock()
	hook := f.onCreate
	f.mu.Unlock()
	if hook != nil {
		if rev, err, ok := hook(key, value); ok {
			return rev, err
		}
	}
	return f.real.create(ctx, key, value)
}

func (f *faultKV) update(ctx context.Context, key string, value []byte, rev uint64) (uint64, error) {
	f.mu.Lock()
	hook := f.onUpdate
	f.mu.Unlock()
	if hook != nil {
		if r, err, ok := hook(key, value, rev); ok {
			return r, err
		}
	}
	return f.real.update(ctx, key, value, rev)
}

func (f *faultKV) keys(ctx context.Context, filter string) ([]string, error) {
	f.mu.Lock()
	hook := f.onKeys
	f.mu.Unlock()
	if hook != nil {
		if k, err, ok := hook(filter); ok {
			return k, err
		}
	}
	return f.real.keys(ctx, filter)
}

func (f *faultKV) delete(ctx context.Context, key string, rev uint64) error {
	return f.real.delete(ctx, key, rev)
}

type fixture struct {
	store *Store
	fault *faultKV
	kv    jetstream.KeyValue
	now   time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	clustersize.DeclareForTest(t, 1)
	_, _, js := testutil.StartTestJetStream(t)
	ctx := context.Background()
	s, err := Open(ctx, js, 16, 8)
	require.NoError(t, err)
	kv, err := js.KeyValue(ctx, Bucket)
	require.NoError(t, err)
	f := &fixture{store: s, kv: kv, now: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
	f.fault = &faultKV{real: s.kv}
	s.kv = f.fault
	s.Now = func() time.Time { return f.now }
	return f
}

func (f *fixture) keys(t *testing.T, prefix string) []string {
	t.Helper()
	ks, err := f.kv.Keys(context.Background())
	if errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil
	}
	require.NoError(t, err)
	var out []string
	for _, k := range ks {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

func create(f *fixture, name string, attrs map[string]string) (Record, error) {
	return f.store.Create(context.Background(), CreateRequest{Account: acct, Region: region, Name: name, Attributes: attrs})
}

func TestCreateAndResolve(t *testing.T) {
	f := newFixture(t)
	r, err := create(f, "orders", map[string]string{AttrVisibilityTimeout: "60"})
	require.NoError(t, err)

	assert.Equal(t, "arn:aws:sqs:ap-southeast-2:111122223333:orders", r.ARN)
	assert.Equal(t, StateActive, r.State)
	assert.Equal(t, uint64(1), r.Generation)
	assert.Equal(t, "60", r.Attributes[AttrVisibilityTimeout])
	assert.Equal(t, "345600", r.Attributes[AttrMessageRetentionPeriod])

	// The keys exist in a real bucket in the dotted form that lets a later
	// listing watch one account and Region.
	assert.Equal(t, []string{"n.111122223333.ap-southeast-2.orders"}, f.keys(t, "n."))
	assert.Equal(t, []string{"q.111122223333." + r.QueueID}, f.keys(t, "q."))

	got, err := f.store.Resolve(context.Background(), acct, region, "orders")
	require.NoError(t, err)
	assert.Equal(t, r.QueueID, got.QueueID)

	_, err = f.store.Resolve(context.Background(), acct, "us-east-1", "orders")
	assert.ErrorIs(t, err, ErrQueueDoesNotExist)
	_, err = f.store.Resolve(context.Background(), "999988887777", region, "orders")
	assert.ErrorIs(t, err, ErrQueueDoesNotExist)
}

func TestCreateRepeat(t *testing.T) {
	f := newFixture(t)
	first, err := create(f, "orders", map[string]string{AttrVisibilityTimeout: "60"})
	require.NoError(t, err)

	same, err := create(f, "orders", map[string]string{AttrVisibilityTimeout: "60", AttrDelaySeconds: "0"})
	require.NoError(t, err)
	assert.Equal(t, first.QueueID, same.QueueID)

	none, err := create(f, "orders", nil)
	require.NoError(t, err)
	assert.Equal(t, first.QueueID, none.QueueID)

	_, err = create(f, "orders", map[string]string{AttrVisibilityTimeout: "30"})
	assert.ErrorIs(t, err, ErrQueueNameExists)
	assert.Len(t, f.keys(t, "q."), 1)
}

func TestCreateRejectsBadInputBeforeWriting(t *testing.T) {
	f := newFixture(t)
	_, err := create(f, "bad.name", nil)
	assert.ErrorIs(t, err, ErrInvalidQueueName)
	_, err = create(f, "orders", map[string]string{"RedrivePolicy": "{}"})
	assert.ErrorIs(t, err, ErrUnsupportedAttribute)
	_, err = f.store.Create(context.Background(), CreateRequest{Account: "acct", Region: region, Name: "orders"})
	assert.ErrorIs(t, err, ErrInvalidIdentity)
	assert.Empty(t, f.keys(t, ""))
}

// Concurrent creators of one name agree on one queue, and every loser removes
// the record its binding write could not apply to.
func TestConcurrentCreatorsAgree(t *testing.T) {
	f := newFixture(t)
	const n = 8
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			r, err := create(f, "orders", map[string]string{AttrDelaySeconds: "5"})
			ids[i], errs[i] = r.QueueID, err
		})
	}
	wg.Wait()
	for i := range n {
		require.NoError(t, errs[i])
		assert.Equal(t, ids[0], ids[i])
	}
	assert.Equal(t, []string{"q.111122223333." + ids[0]}, f.keys(t, "q."))
}

func TestConcurrentCreatorsWithDifferentAttributes(t *testing.T) {
	f := newFixture(t)
	values := []string{"1", "2", "3", "4"}
	errs := make([]error, len(values))
	var wg sync.WaitGroup
	for i, v := range values {
		wg.Go(func() {
			_, errs[i] = create(f, "orders", map[string]string{AttrDelaySeconds: v})
		})
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		if err == nil {
			won++
			continue
		}
		assert.ErrorIs(t, err, ErrQueueNameExists)
	}
	assert.Equal(t, 1, won)
	assert.Len(t, f.keys(t, "q."), 1)
}

func reserve(t *testing.T, f *fixture, name string, reuseAfter time.Time) {
	t.Helper()
	b := Binding{V: RecordVersion, QueueID: "OLDQUEUE", State: BindingReserved, ReuseAfter: reuseAfter}
	enc, err := b.Encode()
	require.NoError(t, err)
	_, err = f.kv.Create(context.Background(), bindingKey(acct, region, name), enc)
	require.NoError(t, err)
}

func TestReservedNameReusableOnlyAfterReuseAfter(t *testing.T) {
	f := newFixture(t)
	reserve(t, f, "orders", f.now.Add(60*time.Second))

	_, err := create(f, "orders", nil)
	assert.ErrorIs(t, err, ErrQueueDeletedRecently)
	_, err = f.store.Resolve(context.Background(), acct, region, "orders")
	assert.ErrorIs(t, err, ErrQueueDoesNotExist)
	assert.Empty(t, f.keys(t, "q."))

	f.now = f.now.Add(60 * time.Second)
	r, err := create(f, "orders", nil)
	require.NoError(t, err)
	assert.NotEqual(t, "OLDQUEUE", r.QueueID)
	got, err := f.store.Resolve(context.Background(), acct, region, "orders")
	require.NoError(t, err)
	assert.Equal(t, r.QueueID, got.QueueID)
}

func TestNameBoundToDeletingQueue(t *testing.T) {
	f := newFixture(t)
	r, err := create(f, "orders", nil)
	require.NoError(t, err)
	_, err = markDeleting(f, "orders")
	require.NoError(t, err)

	_, err = create(f, "orders", nil)
	assert.ErrorIs(t, err, ErrQueueDeletedRecently)
	_, err = f.store.Resolve(context.Background(), acct, region, "orders")
	assert.ErrorIs(t, err, ErrQueueDoesNotExist)

	// A deleting record without a usable deletion intent never frees its name.
	f.rewrite(t, r.QueueID, func(r *Record) { r.Deletion = nil })
	_, err = create(f, "orders", nil)
	assert.ErrorIs(t, err, ErrUnavailable)
}

// A binding write that applied but whose reply was lost is recognised from
// the leader's binding, and the queue is returned, not discarded.
func TestBindingAppliedReplyLost(t *testing.T) {
	f := newFixture(t)
	f.fault.onCreate = func(key string, value []byte) (uint64, error, bool) {
		if !strings.HasPrefix(key, "n.") {
			return 0, nil, false
		}
		_, err := f.fault.real.create(context.Background(), key, value)
		require.NoError(t, err)
		return 0, errLostReply, true
	}
	r, err := create(f, "orders", nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"q.111122223333." + r.QueueID}, f.keys(t, "q."))

	f.fault.onCreate = nil
	got, err := f.store.Resolve(context.Background(), acct, region, "orders")
	require.NoError(t, err)
	assert.Equal(t, r.QueueID, got.QueueID)
}

// A binding write with no reply that did not apply may still apply, so the
// record is kept and the request fails closed.
func TestBindingUnknownOutcomeFailsClosed(t *testing.T) {
	f := newFixture(t)
	f.fault.onCreate = func(key string, _ []byte) (uint64, error, bool) {
		if strings.HasPrefix(key, "n.") {
			return 0, errLostReply, true
		}
		return 0, nil, false
	}
	_, err := create(f, "orders", nil)
	assert.ErrorIs(t, err, ErrUnavailable)
	assert.Len(t, f.keys(t, "q."), 1, "a possibly bound record must not be deleted")

	f.fault.onCreate = nil
	_, err = f.store.Resolve(context.Background(), acct, region, "orders")
	assert.ErrorIs(t, err, ErrQueueDoesNotExist)

	r, err := create(f, "orders", nil)
	require.NoError(t, err)
	got, err := f.store.Resolve(context.Background(), acct, region, "orders")
	require.NoError(t, err)
	assert.Equal(t, r.QueueID, got.QueueID)
}

func TestAttributesFor(t *testing.T) {
	f := newFixture(t)
	r, err := create(f, "orders", map[string]string{AttrDelaySeconds: "5"})
	require.NoError(t, err)

	every := []string{
		"QueueArn", "CreatedTimestamp", "LastModifiedTimestamp", AttrVisibilityTimeout, AttrMessageRetentionPeriod,
		AttrMaximumMessageSize, AttrDelaySeconds, AttrReceiveMessageWaitTimeSeconds, AttrSqsManagedSseEnabled,
	}
	got, err := r.AttributesFor(every)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"QueueArn":                        r.ARN,
		"CreatedTimestamp":                "1793361600",
		"LastModifiedTimestamp":           "1793361600",
		AttrVisibilityTimeout:             "30",
		AttrMessageRetentionPeriod:        "345600",
		AttrMaximumMessageSize:            "1048576",
		AttrDelaySeconds:                  "5",
		AttrReceiveMessageWaitTimeSeconds: "0",
		AttrSqsManagedSseEnabled:          "false",
	}, got)

	named, err := r.AttributesFor([]string{AttrDelaySeconds, "QueueArn", "Policy"})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{AttrDelaySeconds: "5", "QueueArn": r.ARN}, named)

	_, err = r.AttributesFor([]string{"Bogus"})
	assert.ErrorIs(t, err, ErrInvalidAttributeName)
}

func TestOpenRefusesNonDurableBucket(t *testing.T) {
	clustersize.DeclareForTest(t, 1)
	ctx := context.Background()
	for name, cfg := range map[string]jetstream.KeyValueConfig{
		"expiring": {Bucket: Bucket, History: 1, Storage: jetstream.FileStorage, TTL: time.Hour},
		"memory":   {Bucket: Bucket, History: 1, Storage: jetstream.MemoryStorage},
		"capped":   {Bucket: Bucket, History: 1, Storage: jetstream.FileStorage, MaxBytes: 1 << 20},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, js := testutil.StartTestJetStream(t)
			_, err := js.CreateKeyValue(ctx, cfg)
			require.NoError(t, err)
			_, err = Open(ctx, js, 16, 8)
			assert.Error(t, err, "opened a bucket that can lose records")
		})
	}
}
