package handlers_eks

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubHTTPDoer is a fake HTTPDoer that returns a configurable status code +
// optional error, and tracks the number of calls for assertion.
type stubHTTPDoer struct {
	mu     sync.Mutex
	calls  atomic.Int32
	status int
	err    error
}

func (s *stubHTTPDoer) Do(_ *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	s.mu.Lock()
	err := s.err
	status := s.status
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(""))}, nil
}

func (s *stubHTTPDoer) setResponse(status int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
	s.err = err
}

func newReconcilerHarness(t *testing.T, healthURL string, opts ...ReconcilerOption) (*ClusterReconciler, nats.KeyValue, nats.KeyValue) {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	leaderKV, err := InitLeaderBucket(js)
	require.NoError(t, err)
	acctKV, err := GetOrCreateAccountBucket(js, testAccountID)
	require.NoError(t, err)
	require.NoError(t, PutClusterMeta(acctKV, sampleClusterMeta("alpha")))

	r, err := NewClusterReconciler(leaderKV, acctKV, testAccountID, "alpha", "holder-1", healthURL, opts...)
	require.NoError(t, err)
	return r, leaderKV, acctKV
}

func TestNewClusterReconciler_EmptyInputsRejected(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	leaderKV, err := InitLeaderBucket(js)
	require.NoError(t, err)
	acctKV, err := GetOrCreateAccountBucket(js, testAccountID)
	require.NoError(t, err)

	_, err = NewClusterReconciler(nil, acctKV, testAccountID, "alpha", "h", "")
	require.Error(t, err)
	_, err = NewClusterReconciler(leaderKV, nil, testAccountID, "alpha", "h", "")
	require.Error(t, err)
	_, err = NewClusterReconciler(leaderKV, acctKV, "", "alpha", "h", "")
	require.Error(t, err)
	_, err = NewClusterReconciler(leaderKV, acctKV, testAccountID, "", "h", "")
	require.Error(t, err)
	_, err = NewClusterReconciler(leaderKV, acctKV, testAccountID, "alpha", "", "")
	require.Error(t, err)
}

func TestClusterReconciler_AcquireLeaseFirstHolderWins(t *testing.T) {
	r, _, _ := newReconcilerHarness(t, "")

	release, ok := r.AcquireLease()
	require.True(t, ok)
	require.NotNil(t, release)
	defer release()

	// Second AcquireLease from same holder must fail (Create returns KeyExists).
	release2, ok2 := r.AcquireLease()
	assert.False(t, ok2)
	assert.Nil(t, release2)
}

func TestClusterReconciler_AcquireLeaseSecondHolderLoses(t *testing.T) {
	r1, leaderKV, acctKV := newReconcilerHarness(t, "")

	release, ok := r1.AcquireLease()
	require.True(t, ok)
	defer release()

	r2, err := NewClusterReconciler(leaderKV, acctKV, testAccountID, "alpha", "holder-2", "")
	require.NoError(t, err)
	release2, ok2 := r2.AcquireLease()
	assert.False(t, ok2)
	assert.Nil(t, release2)
}

func TestClusterReconciler_RefreshLeaseFailsAfterStolen(t *testing.T) {
	r, leaderKV, _ := newReconcilerHarness(t, "")

	release, ok := r.AcquireLease()
	require.True(t, ok)
	defer release()

	assert.True(t, r.RefreshLease(), "refresh should succeed while we own the key")

	// Steal the key by overwriting its value via Put.
	_, err := leaderKV.Put(reconcilerLeaderKey(testAccountID, "alpha"), []byte("holder-2"))
	require.NoError(t, err)

	assert.False(t, r.RefreshLease(), "refresh should fail after another holder stole the key")
}

func TestClusterReconciler_RefreshLeaseFailsWhenKeyDeleted(t *testing.T) {
	r, leaderKV, _ := newReconcilerHarness(t, "")
	release, ok := r.AcquireLease()
	require.True(t, ok)
	defer release()

	require.NoError(t, leaderKV.Delete(reconcilerLeaderKey(testAccountID, "alpha")))
	assert.False(t, r.RefreshLease())
}

func seedBootstrapState(t *testing.T, kv nats.KeyValue) {
	t.Helper()
	_, err := kv.Put(NodeTokenKey("alpha"), []byte("enc-token"))
	require.NoError(t, err)
	_, err = kv.Put(AdminKubeconfigKey("alpha"), []byte("enc-kc"))
	require.NoError(t, err)
	_, err = kv.Put(OIDCJWKSKey("alpha"), []byte(`{"keys":[]}`))
	require.NoError(t, err)
	require.NoError(t, SetClusterCertificateAuthority(kv, "alpha", "ca-blob-b64"))
}

func TestClusterReconciler_CreatingTransitionsToActiveOnReadyAndHealthz(t *testing.T) {
	stub := &stubHTTPDoer{status: http.StatusOK}
	r, _, acctKV := newReconcilerHarness(t,
		"https://nlb.example/healthz",
		WithHTTPClient(stub),
		WithReconcileInterval(10*time.Millisecond),
		WithLeaseRefresh(10*time.Second),
	)
	release, ok := r.AcquireLease()
	require.True(t, ok)
	defer release()

	seedBootstrapState(t, acctKV)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	runErr := make(chan error, 1)
	go func() { runErr <- r.Run(ctx) }()

	require.Eventually(t, func() bool {
		meta, err := GetClusterMeta(acctKV, "alpha")
		if err != nil {
			return false
		}
		return meta.Status == ClusterStatusActive
	}, 1500*time.Millisecond, 10*time.Millisecond, "CREATING should transition to ACTIVE")

	cancel()
	select {
	case err := <-runErr:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	assert.Positive(t, stub.calls.Load(), "/healthz should have been probed at least once")
}

func TestClusterReconciler_CreatingStaysWhenBootstrapMissing(t *testing.T) {
	stub := &stubHTTPDoer{status: http.StatusOK}
	r, _, acctKV := newReconcilerHarness(t,
		"https://nlb.example/healthz",
		WithHTTPClient(stub),
		WithReconcileInterval(10*time.Millisecond),
		WithLeaseRefresh(10*time.Second),
	)
	release, ok := r.AcquireLease()
	require.True(t, ok)
	defer release()

	// Only seed two of the three KV keys (no CA, no JWKS).
	_, err := acctKV.Put(NodeTokenKey("alpha"), []byte("enc-token"))
	require.NoError(t, err)
	_, err = acctKV.Put(AdminKubeconfigKey("alpha"), []byte("enc-kc"))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = r.Run(ctx)

	meta, err := GetClusterMeta(acctKV, "alpha")
	require.NoError(t, err)
	assert.Equal(t, ClusterStatusCreating, meta.Status, "must remain CREATING when bootstrap incomplete")
}

func TestClusterReconciler_CreatingStaysWhenHealthzFails(t *testing.T) {
	stub := &stubHTTPDoer{err: errors.New("connection refused")}
	r, _, acctKV := newReconcilerHarness(t,
		"https://nlb.example/healthz",
		WithHTTPClient(stub),
		WithReconcileInterval(10*time.Millisecond),
		WithLeaseRefresh(10*time.Second),
	)
	release, ok := r.AcquireLease()
	require.True(t, ok)
	defer release()

	seedBootstrapState(t, acctKV)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = r.Run(ctx)

	meta, err := GetClusterMeta(acctKV, "alpha")
	require.NoError(t, err)
	assert.Equal(t, ClusterStatusCreating, meta.Status, "must remain CREATING when /healthz fails")
}

func TestClusterReconciler_DeletingExitsLoop(t *testing.T) {
	stub := &stubHTTPDoer{status: http.StatusOK}
	r, _, acctKV := newReconcilerHarness(t,
		"https://nlb.example/healthz",
		WithHTTPClient(stub),
		WithReconcileInterval(10*time.Millisecond),
		WithLeaseRefresh(10*time.Second),
	)
	release, ok := r.AcquireLease()
	require.True(t, ok)
	defer release()

	require.NoError(t, SetClusterStatus(acctKV, "alpha", ClusterStatusDeleting))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := r.Run(ctx)
	assert.ErrorIs(t, err, ErrReconcilerClusterDeleting)
}

func TestClusterReconciler_LostLeaseExitsLoop(t *testing.T) {
	stub := &stubHTTPDoer{status: http.StatusOK}
	r, leaderKV, acctKV := newReconcilerHarness(t,
		"https://nlb.example/healthz",
		WithHTTPClient(stub),
		WithReconcileInterval(10*time.Second),
		WithLeaseRefresh(20*time.Millisecond),
	)
	release, ok := r.AcquireLease()
	require.True(t, ok)
	defer release()

	seedBootstrapState(t, acctKV)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	runErr := make(chan error, 1)
	go func() { runErr <- r.Run(ctx) }()

	// Steal the lease in another holder's name. The next refresh tick should
	// notice and return ErrReconcilerLeaseLost.
	time.Sleep(40 * time.Millisecond)
	_, err := leaderKV.Put(reconcilerLeaderKey(testAccountID, "alpha"), []byte("holder-2"))
	require.NoError(t, err)

	select {
	case err := <-runErr:
		assert.ErrorIs(t, err, ErrReconcilerLeaseLost)
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after lease loss")
	}
}

func TestClusterReconciler_ActiveProbesAndWarnsOnHealthzFail(t *testing.T) {
	stub := &stubHTTPDoer{status: http.StatusOK}
	r, _, acctKV := newReconcilerHarness(t,
		"https://nlb.example/healthz",
		WithHTTPClient(stub),
		WithReconcileInterval(10*time.Millisecond),
		WithLeaseRefresh(10*time.Second),
	)
	release, ok := r.AcquireLease()
	require.True(t, ok)
	defer release()

	require.NoError(t, SetClusterStatus(acctKV, "alpha", ClusterStatusActive))
	stub.setResponse(http.StatusServiceUnavailable, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = r.Run(ctx)

	meta, err := GetClusterMeta(acctKV, "alpha")
	require.NoError(t, err)
	assert.Equal(t, ClusterStatusActive, meta.Status, "ACTIVE must not flip on healthz fail in this PR")
	assert.Positive(t, stub.calls.Load())
}

func TestClusterReconciler_ProbeHealthzEmptyURLNoop(t *testing.T) {
	stub := &stubHTTPDoer{}
	r, _, _ := newReconcilerHarness(t,
		"",
		WithHTTPClient(stub),
		WithReconcileInterval(10*time.Millisecond),
		WithLeaseRefresh(10*time.Second),
	)

	require.NoError(t, r.probeHealthz(context.Background()))
	assert.EqualValues(t, 0, stub.calls.Load(), "no HTTP call when URL empty")
}
