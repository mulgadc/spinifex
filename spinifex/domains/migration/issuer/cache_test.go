package issuer

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type fakeIssuer struct {
	calls   atomic.Int32
	accts   []string
	inner   *Issuer
	issuedM sync.Mutex
}

func (f *fakeIssuer) Issue(a, c string) (*ClientCertificate, error) {
	f.calls.Add(1)
	f.issuedM.Lock()
	f.accts = append(f.accts, a)
	f.issuedM.Unlock()
	return f.inner.Issue(a, c)
}

type fakeCreator struct {
	clock   *fakeClock
	mu      sync.Mutex
	calls   []SessionRequest
	certs   []*ClientCertificate
	err     error
	entered chan struct{} // when set, signals entry then waits on release
	release chan struct{}
}

func (f *fakeCreator) CreateSession(_ context.Context, cert *ClientCertificate, r SessionRequest) (Session, error) {
	f.mu.Lock()
	f.calls = append(f.calls, r)
	f.certs = append(f.certs, cert)
	err, entered, release := f.err, f.entered, f.release
	f.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
		<-release
	}
	if err != nil {
		return Session{}, err
	}
	return Session{AccessKeyID: "AK", SecretAccessKey: "S", SessionToken: "T", Expiration: f.clock.now().Add(SessionDurationSeconds * time.Second)}, nil
}

func (f *fakeCreator) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type fakeState struct {
	checks   atomic.Int32
	inactive atomic.Bool
	lookup   atomic.Bool
}

func (f *fakeState) CheckActive(context.Context, string, string) error {
	f.checks.Add(1)
	if f.inactive.Load() {
		return ErrConnectionNotActive
	}
	if f.lookup.Load() {
		return errors.New("kv unavailable")
	}
	return nil
}

var _ StateChecker = (*fakeState)(nil)

func newCacheFixture(t *testing.T) (*Cache, *fakeClock, *fakeIssuer, *fakeCreator, *fakeState) {
	t.Helper()
	clock := &fakeClock{t: testNow}
	ca, key := newTestCA(t, testNow.Add(-24*time.Hour), testNow.Add(24*time.Hour), true)
	inner, err := New(ca, key, "c1", clock.now)
	require.NoError(t, err)
	iss := &fakeIssuer{inner: inner}
	cr := &fakeCreator{clock: clock}
	st := &fakeState{}
	return NewCache(iss, cr, st, clock.now), clock, iss, cr, st
}

const (
	discoveryRole = "arn:aws:iam::111122223333:role/Discovery"
	dataRole      = "arn:aws:iam::111122223333:role/Data"
)

var testConn = Connection{
	AccountID: "123456789012", ConnectionID: "conn-1",
	TrustAnchorARN: testTrustAnchor, ProfileARN: testProfile,
	Roles: []string{discoveryRole, dataRole},
}

func TestCache_ReusesSessionUntilRenewWindow(t *testing.T) {
	c, clock, _, cr, _ := newCacheFixture(t)
	ctx := context.Background()
	_, err := c.Credentials(ctx, testConn, discoveryRole)
	require.NoError(t, err)

	clock.advance(9*time.Minute + 59*time.Second) // 5m01s left
	_, err = c.Credentials(ctx, testConn, discoveryRole)
	require.NoError(t, err)
	assert.Equal(t, 1, cr.callCount())

	clock.advance(2 * time.Second) // 4m59s left
	s, err := c.Credentials(ctx, testConn, discoveryRole)
	require.NoError(t, err)
	assert.Equal(t, 2, cr.callCount())
	assert.Equal(t, clock.now().Add(900*time.Second), s.Expiration)
}

func TestCache_SessionsAreKeyedByRole(t *testing.T) {
	c, _, iss, cr, _ := newCacheFixture(t)
	ctx := context.Background()
	_, err := c.Credentials(ctx, testConn, discoveryRole)
	require.NoError(t, err)
	_, err = c.Credentials(ctx, testConn, dataRole)
	require.NoError(t, err)
	_, err = c.Credentials(ctx, testConn, discoveryRole)
	require.NoError(t, err)

	require.Equal(t, 2, cr.callCount())
	assert.Equal(t, discoveryRole, cr.calls[0].RoleARN)
	assert.Equal(t, dataRole, cr.calls[1].RoleARN)
	assert.Equal(t, testTrustAnchor, cr.calls[1].TrustAnchorARN)
	assert.EqualValues(t, 1, iss.calls.Load(), "both roles share one certificate")
}

func TestCache_RefusesRoleOutsideConnection(t *testing.T) {
	c, _, _, cr, st := newCacheFixture(t)
	_, err := c.Credentials(context.Background(), testConn, "arn:aws:iam::111122223333:role/Admin")
	require.ErrorIs(t, err, ErrRoleNotAllowed)
	_, err = c.Credentials(context.Background(), Connection{AccountID: "123456789012", ConnectionID: "conn-1"}, discoveryRole)
	require.ErrorIs(t, err, ErrRoleNotAllowed)
	assert.Zero(t, cr.callCount())
	assert.Zero(t, st.checks.Load())
}

func TestCache_SameConnectionIDInTwoAccountsIsIsolated(t *testing.T) {
	c, _, iss, cr, _ := newCacheFixture(t)
	ctx := context.Background()
	other := testConn
	other.AccountID = "210987654321"

	_, err := c.Credentials(ctx, testConn, discoveryRole)
	require.NoError(t, err)
	_, err = c.Credentials(ctx, other, discoveryRole)
	require.NoError(t, err)

	assert.Equal(t, 2, cr.callCount(), "the second account does not reuse the first session")
	assert.Equal(t, []string{"123456789012", "210987654321"}, iss.accts)
	assert.Equal(t, "123456789012", cr.certs[0].Certificate.Subject.OrganizationalUnit[0])
	assert.Equal(t, "210987654321", cr.certs[1].Certificate.Subject.OrganizationalUnit[0])
}

func TestCache_RenewsCertificateBeforeItExpires(t *testing.T) {
	c, clock, iss, cr, _ := newCacheFixture(t)
	ctx := context.Background()
	_, err := c.Credentials(ctx, testConn, discoveryRole)
	require.NoError(t, err)

	// Sessions renew about every 10 minutes; the one hour certificate is
	// replaced once it has 5 minutes left and is never used inside that window.
	for range 7 {
		clock.advance(10 * time.Minute)
		_, err = c.Credentials(ctx, testConn, discoveryRole)
		require.NoError(t, err)
	}
	assert.EqualValues(t, 2, iss.calls.Load())
	last := cr.certs[len(cr.certs)-1]
	assert.Greater(t, last.Certificate.NotAfter.Sub(clock.now()), RenewBefore)
	assert.NotEqual(t, cr.certs[0].Certificate.SerialNumber, last.Certificate.SerialNumber)
}

func TestCache_ChecksStateOnEveryUse(t *testing.T) {
	c, _, _, _, st := newCacheFixture(t)
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		_, err := c.Credentials(ctx, testConn, discoveryRole)
		require.NoError(t, err)
	}
	// First call: before use and after the refresh. Later hits: once each.
	assert.EqualValues(t, 4, st.checks.Load())
}

func TestCache_StopsAndForgetsWhenConnectionNotActive(t *testing.T) {
	c, _, iss, cr, st := newCacheFixture(t)
	ctx := context.Background()
	_, err := c.Credentials(ctx, testConn, discoveryRole)
	require.NoError(t, err)

	st.inactive.Store(true)
	_, err = c.Credentials(ctx, testConn, discoveryRole)
	require.ErrorIs(t, err, ErrConnectionNotActive)
	assert.Equal(t, 1, cr.callCount(), "no call to AWS after the state turns")

	st.inactive.Store(false)
	_, err = c.Credentials(ctx, testConn, discoveryRole)
	require.NoError(t, err)
	assert.Equal(t, 2, cr.callCount(), "the cached session was dropped")
	assert.EqualValues(t, 2, iss.calls.Load(), "the cached certificate was dropped")
}

func TestCache_StateLookupFailureFailsClosedWithoutDropping(t *testing.T) {
	c, _, _, cr, st := newCacheFixture(t)
	ctx := context.Background()
	_, err := c.Credentials(ctx, testConn, discoveryRole)
	require.NoError(t, err)

	st.lookup.Store(true)
	_, err = c.Credentials(ctx, testConn, discoveryRole)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrConnectionNotActive)

	st.lookup.Store(false)
	_, err = c.Credentials(ctx, testConn, discoveryRole)
	require.NoError(t, err)
	assert.Equal(t, 1, cr.callCount())
}

func TestCache_DisconnectDuringRefreshDiscardsSessionAndStopsQueuedHit(t *testing.T) {
	c, clock, _, cr, st := newCacheFixture(t)
	ctx := context.Background()
	_, err := c.Credentials(ctx, testConn, discoveryRole)
	require.NoError(t, err)
	clock.advance(11 * time.Minute) // the data role has no session; discovery is due

	cr.mu.Lock()
	cr.entered, cr.release = make(chan struct{}), make(chan struct{})
	entered, release := cr.entered, cr.release
	cr.mu.Unlock()

	errs := make(chan error, 2)
	go func() {
		_, err := c.Credentials(ctx, testConn, dataRole)
		errs <- err
	}()
	<-entered // the refresh holds the entry and is inside CreateSession

	go func() {
		_, err := c.Credentials(ctx, testConn, discoveryRole) // a queued caller
		errs <- err
	}()
	st.inactive.Store(true) // disconnect lands while the refresh is in flight
	close(release)

	assert.ErrorIs(t, <-errs, ErrConnectionNotActive)
	assert.ErrorIs(t, <-errs, ErrConnectionNotActive)
	assert.Equal(t, 2, cr.callCount(), "the queued caller made no AWS call")

	st.inactive.Store(false)
	cr.mu.Lock()
	cr.entered = nil
	cr.mu.Unlock()
	_, err = c.Credentials(ctx, testConn, dataRole)
	require.NoError(t, err)
	assert.Equal(t, 3, cr.callCount(), "the refreshed session was not kept")
}

func TestCache_RefusedRefreshReturnsErrorAndNoStaleSession(t *testing.T) {
	c, clock, _, cr, _ := newCacheFixture(t)
	ctx := context.Background()
	_, err := c.Credentials(ctx, testConn, discoveryRole)
	require.NoError(t, err)
	clock.advance(11 * time.Minute)

	cr.mu.Lock()
	cr.err = &APIError{StatusCode: 403, Code: "AccessDeniedException"}
	cr.mu.Unlock()
	_, err = c.Credentials(ctx, testConn, discoveryRole)
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.True(t, apiErr.Refused())

	cr.mu.Lock()
	cr.err = nil
	cr.mu.Unlock()
	s, err := c.Credentials(ctx, testConn, discoveryRole)
	require.NoError(t, err)
	assert.True(t, s.Expiration.After(clock.now()))
}

func TestCache_DropForgetsConnection(t *testing.T) {
	c, _, _, cr, _ := newCacheFixture(t)
	ctx := context.Background()
	_, err := c.Credentials(ctx, testConn, discoveryRole)
	require.NoError(t, err)
	c.Drop(testConn.AccountID, testConn.ConnectionID)
	_, err = c.Credentials(ctx, testConn, discoveryRole)
	require.NoError(t, err)
	assert.Equal(t, 2, cr.callCount())
	c.Drop("000000000000", "unknown")
}

func TestCache_SweepDropsExpiredMaterial(t *testing.T) {
	c, clock, _, cr, _ := newCacheFixture(t)
	ctx := context.Background()
	_, err := c.Credentials(ctx, testConn, discoveryRole)
	require.NoError(t, err)

	clock.advance(14 * time.Minute) // session valid for one more minute
	c.Sweep()
	e := c.conns[connKey{testConn.AccountID, testConn.ConnectionID}]
	assert.Len(t, e.sessions, 1)
	assert.NotNil(t, e.cert)

	clock.advance(2 * time.Minute) // session expired
	c.Sweep()
	assert.Empty(t, e.sessions)
	assert.NotNil(t, e.cert, "the certificate is valid for an hour")

	clock.advance(time.Hour)
	c.Sweep()
	assert.Nil(t, e.cert)
	assert.Equal(t, 1, cr.callCount())
}
