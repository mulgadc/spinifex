package issuer

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

type fakeIssuer struct {
	clock *fakeClock
	calls int
	inner *Issuer
}

func (f *fakeIssuer) Issue(a, c string) (*ClientCertificate, error) {
	f.calls++
	return f.inner.Issue(a, c)
}

type fakeCreator struct {
	clock *fakeClock
	calls []SessionRequest
	certs []*ClientCertificate
	err   error
}

func (f *fakeCreator) CreateSession(_ context.Context, cert *ClientCertificate, r SessionRequest) (Session, error) {
	f.calls = append(f.calls, r)
	f.certs = append(f.certs, cert)
	if f.err != nil {
		return Session{}, f.err
	}
	return Session{AccessKeyID: "AK", SecretAccessKey: "S", SessionToken: "T", Expiration: f.clock.now().Add(SessionDurationSeconds * time.Second)}, nil
}

type fakeState struct {
	checks atomic.Int32
	err    error
}

func (f *fakeState) CheckActive(context.Context, string) error {
	f.checks.Add(1)
	return f.err
}

func newCacheFixture(t *testing.T) (*Cache, *fakeClock, *fakeIssuer, *fakeCreator, *fakeState) {
	t.Helper()
	clock := &fakeClock{t: testNow}
	ca, key := newTestCA(t, testNow.Add(-24*time.Hour), testNow.Add(24*time.Hour), true)
	inner, err := New(ca, key, "c1", clock.now)
	require.NoError(t, err)
	iss := &fakeIssuer{clock: clock, inner: inner}
	cr := &fakeCreator{clock: clock}
	st := &fakeState{}
	return NewCache(iss, cr, st, clock.now), clock, iss, cr, st
}

var testConn = Connection{AccountID: "123456789012", ConnectionID: "conn-1", TrustAnchorARN: testTrustAnchor, ProfileARN: testProfile}

const (
	discoveryRole = "arn:aws:iam::111122223333:role/Discovery"
	dataRole      = "arn:aws:iam::111122223333:role/Data"
)

func TestCache_ReusesSessionUntilRenewWindow(t *testing.T) {
	c, clock, _, cr, _ := newCacheFixture(t)
	ctx := context.Background()
	_, err := c.Credentials(ctx, testConn, discoveryRole)
	require.NoError(t, err)

	clock.advance(9*time.Minute + 59*time.Second) // 5m01s left
	_, err = c.Credentials(ctx, testConn, discoveryRole)
	require.NoError(t, err)
	assert.Len(t, cr.calls, 1)

	clock.advance(2 * time.Second) // 4m59s left
	s, err := c.Credentials(ctx, testConn, discoveryRole)
	require.NoError(t, err)
	assert.Len(t, cr.calls, 2)
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

	require.Len(t, cr.calls, 2)
	assert.Equal(t, discoveryRole, cr.calls[0].RoleARN)
	assert.Equal(t, dataRole, cr.calls[1].RoleARN)
	assert.Equal(t, testTrustAnchor, cr.calls[1].TrustAnchorARN)
	assert.Equal(t, 1, iss.calls, "both roles share one certificate")
}

func TestCache_RenewsCertificateBeforeItExpires(t *testing.T) {
	c, clock, iss, cr, _ := newCacheFixture(t)
	ctx := context.Background()
	_, err := c.Credentials(ctx, testConn, discoveryRole)
	require.NoError(t, err)

	// Sessions renew every ~10 minutes; the 1 hour certificate is replaced
	// once it has 5 minutes left, never used inside that window.
	for range 7 {
		clock.advance(10 * time.Minute)
		_, err = c.Credentials(ctx, testConn, discoveryRole)
		require.NoError(t, err)
	}
	assert.Equal(t, 2, iss.calls)
	for _, cert := range cr.certs {
		assert.NotNil(t, cert)
	}
	last := cr.certs[len(cr.certs)-1]
	assert.Greater(t, last.Certificate.NotAfter.Sub(clock.now()), RenewBefore)
	assert.NotEqual(t, cr.certs[0].Certificate.SerialNumber, last.Certificate.SerialNumber)
}

func TestCache_ChecksStateBeforeEveryUse(t *testing.T) {
	c, _, _, _, st := newCacheFixture(t)
	ctx := context.Background()
	_, err := c.Credentials(ctx, testConn, discoveryRole)
	require.NoError(t, err)
	assert.EqualValues(t, 2, st.checks.Load(), "use check plus refresh check")
	_, err = c.Credentials(ctx, testConn, discoveryRole)
	require.NoError(t, err)
	assert.EqualValues(t, 3, st.checks.Load(), "a cache hit still checks once")
}

func TestCache_StopsAndForgetsWhenConnectionNotActive(t *testing.T) {
	c, _, iss, cr, st := newCacheFixture(t)
	ctx := context.Background()
	_, err := c.Credentials(ctx, testConn, discoveryRole)
	require.NoError(t, err)

	st.err = ErrConnectionNotActive
	_, err = c.Credentials(ctx, testConn, discoveryRole)
	require.ErrorIs(t, err, ErrConnectionNotActive)
	assert.Len(t, cr.calls, 1, "no call to AWS after the state turns")

	st.err = nil
	_, err = c.Credentials(ctx, testConn, discoveryRole)
	require.NoError(t, err)
	assert.Len(t, cr.calls, 2, "the cached session was dropped")
	assert.Equal(t, 2, iss.calls, "the cached certificate was dropped")
}

func TestCache_StateLookupFailureFailsClosedWithoutDropping(t *testing.T) {
	c, _, _, cr, st := newCacheFixture(t)
	ctx := context.Background()
	_, err := c.Credentials(ctx, testConn, discoveryRole)
	require.NoError(t, err)

	st.err = errors.New("kv unavailable")
	_, err = c.Credentials(ctx, testConn, discoveryRole)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrConnectionNotActive)

	st.err = nil
	_, err = c.Credentials(ctx, testConn, discoveryRole)
	require.NoError(t, err)
	assert.Len(t, cr.calls, 1)
}

func TestCache_StateTurnsDuringRefresh(t *testing.T) {
	c, clock, _, cr, st := newCacheFixture(t)
	ctx := context.Background()
	_, err := c.Credentials(ctx, testConn, discoveryRole)
	require.NoError(t, err)
	clock.advance(11 * time.Minute)

	// First check passes, second (before refresh) fails.
	st.err = nil
	flip := &flipState{n: 1}
	c.state = flip
	_, err = c.Credentials(ctx, testConn, discoveryRole)
	require.ErrorIs(t, err, ErrConnectionNotActive)
	assert.Len(t, cr.calls, 1, "no refresh after the state turned")
}

type flipState struct{ n int }

func (f *flipState) CheckActive(context.Context, string) error {
	if f.n > 0 {
		f.n--
		return nil
	}
	return ErrConnectionNotActive
}

func TestCache_RefusedRefreshReturnsErrorAndNoStaleSession(t *testing.T) {
	c, clock, _, cr, _ := newCacheFixture(t)
	ctx := context.Background()
	_, err := c.Credentials(ctx, testConn, discoveryRole)
	require.NoError(t, err)
	clock.advance(11 * time.Minute)

	cr.err = &APIError{StatusCode: 403, Code: "AccessDeniedException"}
	_, err = c.Credentials(ctx, testConn, discoveryRole)
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.True(t, apiErr.Refused())

	cr.err = nil
	s, err := c.Credentials(ctx, testConn, discoveryRole)
	require.NoError(t, err)
	assert.True(t, s.Expiration.After(clock.now()))
}

func TestCache_DropForgetsConnection(t *testing.T) {
	c, _, _, cr, _ := newCacheFixture(t)
	ctx := context.Background()
	_, err := c.Credentials(ctx, testConn, discoveryRole)
	require.NoError(t, err)
	c.Drop(testConn.ConnectionID)
	_, err = c.Credentials(ctx, testConn, discoveryRole)
	require.NoError(t, err)
	assert.Len(t, cr.calls, 2)
	c.Drop("unknown")
}

var _ StateChecker = (*fakeState)(nil)
