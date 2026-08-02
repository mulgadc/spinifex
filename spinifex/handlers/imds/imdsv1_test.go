package handlers_imds

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingResolver wraps fakeResolver to count instance lookups, so the cache
// can be shown to keep the untokened path off the DescribeInstances fan-out.
type countingResolver struct {
	fakeResolver

	calls int
}

func (c *countingResolver) resolveInstance(ctx context.Context, eni *eniFacts) (*instanceFacts, error) {
	c.calls++
	return c.fakeResolver.resolveInstance(ctx, eni)
}

func v1Resolver(httpTokens string) *countingResolver {
	return &countingResolver{fakeResolver: fakeResolver{
		eni:  testENI(),
		inst: &instanceFacts{httpTokens: httpTokens},
	}}
}

// An instance left at the default still 401s an untokened GET, so the IMDSv1
// opt-in cannot leak to instances that never asked for it.
func TestHTTP_TokenlessGETRejectedWhenTokensRequired(t *testing.T) {
	svc, _ := newTestService(v1Resolver(ec2.HttpTokensStateRequired), &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())

	rec := get(t, h, prefixMetaData+"instance-id", "")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Empty(t, rec.Body.String())
}

// HttpTokens=optional is what lets an IMDSv1-only agent such as cloudbase-init
// read metadata without performing the token handshake.
func TestHTTP_TokenlessGETServedWhenTokensOptional(t *testing.T) {
	svc, _ := newTestService(v1Resolver(ec2.HttpTokensStateOptional), &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())

	rec := get(t, h, prefixMetaData+"instance-id", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "i-0123456789", rec.Body.String())
}

// IMDSv2 keeps working unchanged on an instance that also permits v1.
func TestHTTP_TokenedGETStillServedWhenTokensOptional(t *testing.T) {
	svc, _ := newTestService(v1Resolver(ec2.HttpTokensStateOptional), &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())

	rec := get(t, h, prefixMetaData+"instance-id", issueToken(t, h))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "i-0123456789", rec.Body.String())
}

// A valid token short-circuits before the http-tokens lookup, so compliant
// IMDSv2 clients never pay for the opt-in check.
func TestIMDSv1_TokenedRequestSkipsLookup(t *testing.T) {
	res := v1Resolver(ec2.HttpTokensStateRequired)
	svc, _ := newTestService(res, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())

	token := issueToken(t, h)
	rec := get(t, h, prefixMetaData+"local-ipv4", token)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Zero(t, res.calls, "a valid token must not trigger an http-tokens lookup")
}

// The untokened path is reachable without credentials, so repeated requests must
// not each drive a DescribeInstances fan-out.
func TestIMDSv1_LookupIsCached(t *testing.T) {
	res := v1Resolver(ec2.HttpTokensStateOptional)
	svc, _ := newTestService(res, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())

	for range 5 {
		require.Equal(t, http.StatusOK, get(t, h, prefixMetaData+"instance-id", "").Code)
	}
	assert.Equal(t, 1, res.calls, "http-tokens must be resolved once and cached")
}

// A failed lookup denies and is cached too: without caching the failure, a guest
// could replay a failing request to keep the fan-out running.
func TestIMDSv1_LookupFailureDeniesAndCaches(t *testing.T) {
	res := &countingResolver{fakeResolver: fakeResolver{
		eni:     testENI(),
		instErr: errors.New("gather timeout"),
	}}
	svc, _ := newTestService(res, &fakeIAM{}, &fakeAssumer{})
	h := withTapENI(svc.httpHandler(), testENI())

	for range 3 {
		assert.Equal(t, http.StatusUnauthorized, get(t, h, prefixMetaData+"instance-id", "").Code)
	}
	assert.Equal(t, 1, res.calls, "a failing lookup must not be retried per request")
}

// An expired entry is re-resolved, so flipping HttpTokens takes effect without a
// vpcd restart.
func TestIMDSv1_CacheExpires(t *testing.T) {
	c := newV1AllowCache()
	now := time.Unix(1_700_000_000, 0).UTC()

	c.put("i-1", true, now)
	allowed, ok := c.get("i-1", now.Add(v1AllowTTL-time.Second))
	require.True(t, ok)
	assert.True(t, allowed)

	_, ok = c.get("i-1", now.Add(v1AllowTTL+time.Second))
	assert.False(t, ok, "entry must expire after the TTL")
}

// sweep drops expired entries so the map cannot grow without bound across
// instance churn.
func TestIMDSv1_CacheSweep(t *testing.T) {
	c := newV1AllowCache()
	now := time.Unix(1_700_000_000, 0).UTC()

	c.put("i-old", true, now)
	c.put("i-new", true, now.Add(v1AllowTTL))
	c.sweep(now.Add(v1AllowTTL + time.Second))

	_, ok := c.get("i-old", now.Add(v1AllowTTL+time.Second))
	assert.False(t, ok)
	assert.Len(t, c.entries, 1, "only the live entry survives the sweep")
}

// An ENI with no attached instance denies without attempting a lookup.
func TestIMDSv1_UnattachedENIDenies(t *testing.T) {
	res := v1Resolver(ec2.HttpTokensStateOptional)
	svc, _ := newTestService(res, &fakeIAM{}, &fakeAssumer{})

	assert.False(t, svc.imdsV1Allowed(context.Background(), &eniFacts{eniID: "eni-x"}))
	assert.Zero(t, res.calls)
}
