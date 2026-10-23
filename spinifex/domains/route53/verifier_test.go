package route53_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	r53 "github.com/aws/aws-sdk-go/service/route53"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/domains/route53"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
)

// fakeDNS is the public DNS tree: what each name's parent publishes for it.
type fakeDNS struct {
	parents map[string][]string
	lookups int
	err     error
}

func (f *fakeDNS) lookup(_ context.Context, name string) (route53.Delegation, error) {
	f.lookups++
	if f.err != nil {
		return route53.Delegation{}, f.err
	}
	return route53.Delegation{Source: "the parent's name servers", Servers: f.parents[strings.TrimSuffix(name, ".")]}, nil
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }
func newClock() *clock                   { return &clock{t: time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)} }
func ours() []string                     { return []string{"ns1.spx3.net", "ns2.spx3.net"} }
func zoneID(id string) string            { return strings.TrimPrefix(id, "/hostedzone/") }

func newVerifier(t *testing.T) (*route53.Service, *route53.Verifier, *fakeDNS, *clock) {
	t.Helper()
	svc := newService(t)
	c := newClock()
	svc.Now = c.now
	dns := &fakeDNS{parents: map[string][]string{}}
	v := route53.NewVerifier(svc.Store())
	v.Nameservers = ours
	v.Public = dns.lookup
	v.Now = c.now
	return svc, v, dns, c
}

func zone(t *testing.T, svc *route53.Service, account, id string) *route53.ZoneRecord {
	t.Helper()
	z, err := svc.Store().GetZone(ctx, account, zoneID(id))
	require.NoError(t, err)
	return z
}

func pass(t *testing.T, v *route53.Verifier) {
	t.Helper()
	_, err := v.Reconcile(ctx)
	require.NoError(t, err)
}

func TestVerifierActivatesOnlyAFullDelegationToUs(t *testing.T) {
	svc, v, dns, _ := newVerifier(t)
	cases := map[string]struct {
		parent []string
		state  string
		reason string
	}{
		"all.example":     {[]string{"NS1.spx3.net.", "ns2.spx3.net"}, route53.StateActive, ""},
		"subset.example":  {[]string{"ns2.spx3.net."}, route53.StateActive, ""},
		"mixed.example":   {[]string{"ns1.spx3.net.", "ns-1.awsdns-01.org."}, route53.StatePendingNameservers, "not this cluster's: ns-1.awsdns-01.org"},
		"foreign.example": {[]string{"dana.ns.cloudflare.com."}, route53.StatePendingNameservers, "not this cluster's"},
		"none.example":    {nil, route53.StatePendingNameservers, "does not delegate none.example"},
	}
	ids := map[string]string{}
	for name, tc := range cases {
		dns.parents[name] = tc.parent
		ids[name] = createZone(t, svc, acctA, name)
	}
	pass(t, v)
	for name, tc := range cases {
		z := zone(t, svc, acctA, ids[name])
		assert.Equal(t, tc.state, z.State, name)
		assert.Contains(t, z.StateReason, tc.reason, name)
		assert.Equal(t, tc.parent, z.Delegation, name)
	}
}

func TestVerifierFirstMatchWins(t *testing.T) {
	svc, v, dns, c := newVerifier(t)
	dns.parents["example.com"] = ours()
	first := createZone(t, svc, acctA, "example.com")
	c.advance(time.Second)
	second := createZone(t, svc, acctB, "example.com")
	pass(t, v)

	assert.Equal(t, route53.StateActive, zone(t, svc, acctA, first).State)
	later := zone(t, svc, acctB, second)
	assert.Equal(t, route53.StatePendingNameservers, later.State)
	assert.Equal(t, "the domain is active in another account", later.StateReason)

	// The claim is not released by the loser checking again.
	c.advance(time.Minute)
	pass(t, v)
	assert.Equal(t, route53.StatePendingNameservers, zone(t, svc, acctB, second).State)
}

func TestVerifierScheduleBacksOffAfterTheFirstTwoDays(t *testing.T) {
	svc, v, dns, c := newVerifier(t)
	createZone(t, svc, acctA, "slow.example")
	pass(t, v)
	require.Equal(t, 1, dns.lookups)

	c.advance(time.Minute)
	pass(t, v)
	assert.Equal(t, 2, dns.lookups, "a new zone is checked every pass")

	c.advance(48 * time.Hour)
	pass(t, v)
	c.advance(30 * time.Minute)
	pass(t, v)
	assert.Equal(t, 3, dns.lookups, "an old pending zone is checked hourly")
	c.advance(30 * time.Minute)
	pass(t, v)
	assert.Equal(t, 4, dns.lookups)
}

func TestVerifierLosesAZoneOnlyAfterThreeDaysOfFailure(t *testing.T) {
	svc, v, dns, c := newVerifier(t)
	dns.parents["moved.example"] = ours()
	id := createZone(t, svc, acctA, "moved.example")
	pass(t, v)
	require.Equal(t, route53.StateActive, zone(t, svc, acctA, id).State)

	// The owner moves the domain away; the daily re-check notices.
	dns.parents["moved.example"] = []string{"ns-1.awsdns-01.org."}
	c.advance(24 * time.Hour)
	pass(t, v)
	z := zone(t, svc, acctA, id)
	assert.Equal(t, route53.StateActive, z.State, "one failed check is not a lost delegation")
	assert.Equal(t, c.t, z.FailingSince)

	c.advance(71 * time.Hour)
	pass(t, v)
	assert.Equal(t, route53.StateActive, zone(t, svc, acctA, id).State)
	c.advance(time.Hour)
	pass(t, v)
	assert.Equal(t, route53.StateNameserversLost, zone(t, svc, acctA, id).State)

	// It comes back when the delegation does.
	dns.parents["moved.example"] = ours()
	c.advance(time.Hour)
	pass(t, v)
	z = zone(t, svc, acctA, id)
	assert.Equal(t, route53.StateActive, z.State)
	assert.True(t, z.FailingSince.IsZero())
}

func TestVerifierTreatsAnUnreachableParentAsUnknown(t *testing.T) {
	svc, v, dns, c := newVerifier(t)
	dns.parents["flaky.example"] = ours()
	id := createZone(t, svc, acctA, "flaky.example")
	pass(t, v)

	dns.err = errors.New("i/o timeout")
	for range 5 {
		c.advance(24 * time.Hour)
		pass(t, v)
	}
	z := zone(t, svc, acctA, id)
	assert.Equal(t, route53.StateActive, z.State)
	assert.True(t, z.FailingSince.IsZero())
	assert.Contains(t, z.StateReason, "could not be checked: i/o timeout")
}

// A subdomain of an active zone is decided by that zone's records, never by
// public DNS, so account B cannot take a name under account A's domain.
func TestVerifierReadsAnActiveParentZoneLocally(t *testing.T) {
	svc, v, dns, c := newVerifier(t)
	dns.parents["example.com"] = ours()
	dns.parents["shop.example.com"] = ours() // what a lookup would wrongly accept
	parent := createZone(t, svc, acctA, "example.com")
	pass(t, v)
	require.Equal(t, route53.StateActive, zone(t, svc, acctA, parent).State)

	child := createZone(t, svc, acctB, "shop.example.com")
	c.advance(time.Minute)
	pass(t, v)
	z := zone(t, svc, acctB, child)
	assert.Equal(t, route53.StatePendingNameservers, z.State)
	assert.Equal(t, "hosted zone example.com does not delegate shop.example.com", z.StateReason)

	_, err := apply(svc, acctA, parent, change("CREATE", "shop.example.com", "NS", 300, "ns1.spx3.net.", "ns2.spx3.net."))
	require.NoError(t, err)
	c.advance(time.Minute)
	pass(t, v)
	z = zone(t, svc, acctB, child)
	assert.Equal(t, route53.StateActive, z.State)
	assert.Equal(t, "hosted zone example.com", z.DelegationSource)
}

func TestVerifierReadsAPlatformZoneLocally(t *testing.T) {
	svc, v, dns, _ := newVerifier(t)
	v.Platform = func(_ context.Context, name string) (route53.Delegation, bool, error) {
		if !strings.HasSuffix(name, ".spx3.net.") {
			return route53.Delegation{}, false, nil
		}
		return route53.Delegation{Source: "platform zone spx3.net", Servers: ours()}, true, nil
	}
	id := createZone(t, svc, acctA, "app.spx3.net")
	pass(t, v)
	assert.Equal(t, route53.StateActive, zone(t, svc, acctA, id).State)
	assert.Zero(t, dns.lookups)
}

func TestVerifierLeavesOverriddenZonesAlone(t *testing.T) {
	svc, v, dns, c := newVerifier(t)
	dns.parents["held.example"] = ours()
	id := createZone(t, svc, acctA, "held.example")
	require.NoError(t, svc.Store().MutateZone(ctx, acctA, zoneID(id), func(z *route53.ZoneRecord) (bool, error) {
		z.State, z.Override = route53.StateSuspended, route53.StateSuspended
		return true, nil
	}))
	pass(t, v)
	c.advance(time.Hour)
	_, err := v.Verify(ctx, acctA, zoneID(id))
	require.NoError(t, err)
	assert.Equal(t, route53.StateSuspended, zone(t, svc, acctA, id).State)
	assert.Zero(t, dns.lookups)
}

func TestVerifyChecksImmediately(t *testing.T) {
	svc, v, dns, _ := newVerifier(t)
	id := createZone(t, svc, acctA, "now.example")
	pass(t, v)
	dns.parents["now.example"] = ours()

	z, err := v.Verify(ctx, acctA, zoneID(id))
	require.NoError(t, err)
	assert.Equal(t, route53.StateActive, z.State)

	_, err = v.Verify(ctx, acctB, zoneID(id))
	require.ErrorIs(t, err, kvstore.ErrNotFound, "another account's zone is not found")
}

func TestVerifierNeedsPublishedNameservers(t *testing.T) {
	svc, v, dns, _ := newVerifier(t)
	v.Nameservers = func() []string { return nil }
	dns.parents["any.example"] = ours()
	id := createZone(t, svc, acctA, "any.example")
	pass(t, v)
	z := zone(t, svc, acctA, id)
	assert.Equal(t, route53.StatePendingNameservers, z.State)
	assert.Equal(t, "this cluster publishes no name servers", z.StateReason)
}

func TestActivatedZoneChangesWaitForSync(t *testing.T) {
	svc, v, dns, _ := newVerifier(t)
	dns.parents["sync.example"] = ours()
	id := createZone(t, svc, acctA, "sync.example")
	pass(t, v)
	out, err := svc.ChangeResourceRecordSets(ctx, &r53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(id),
		ChangeBatch:  &r53.ChangeBatch{Changes: []*r53.Change{change("CREATE", "www.sync.example", "A", 60, "192.0.2.1")}},
	}, acctA)
	require.NoError(t, err)
	assert.Equal(t, "PENDING", aws.StringValue(out.ChangeInfo.Status))
}
