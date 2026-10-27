package route53_test

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/domains/route53"
	"github.com/mulgadc/spinifex/spinifex/domains/route53/ext"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

func TestActivationIsReportedAndCheckedOnDemand(t *testing.T) {
	svc, v, dns, _ := newVerifier(t)
	svc.Verifier = v
	id := createZone(t, svc, acctA, "demand.example")

	got, err := svc.GetHostedZoneActivation(ctx, &ext.GetHostedZoneActivationInput{Id: aws.String(id)}, acctA)
	require.NoError(t, err)
	assert.Equal(t, route53.StatePendingNameservers, aws.StringValue(got.Activation.State))
	assert.Nil(t, got.Activation.CheckedAt, "never checked")

	dns.parents["demand.example"] = ours()
	verified, err := svc.VerifyHostedZoneDelegation(ctx, &ext.VerifyHostedZoneDelegationInput{Id: aws.String(id)}, acctA)
	require.NoError(t, err)
	assert.Equal(t, route53.StateActive, aws.StringValue(verified.Activation.State))
	assert.Equal(t, ours(), aws.StringValueSlice(verified.Activation.DelegatedTo))
	assert.NotNil(t, verified.Activation.CheckedAt)

	_, err = svc.GetHostedZoneActivation(ctx, &ext.GetHostedZoneActivationInput{Id: aws.String(id)}, acctB)
	requireCode(t, err, "NoSuchHostedZone")
	_, err = svc.VerifyHostedZoneDelegation(ctx, &ext.VerifyHostedZoneDelegationInput{Id: aws.String(id)}, acctB)
	requireCode(t, err, "NoSuchHostedZone")
}

func TestAdminFindsZonesAcrossAccounts(t *testing.T) {
	svc, _, _, c := newVerifier(t)
	a := createZone(t, svc, acctA, "shared.example")
	c.advance(time.Second)
	createZone(t, svc, acctB, "shared.example")
	createZone(t, svc, acctB, "other.example")

	byName, err := svc.AdminFindZones(ctx, &route53.AdminZoneQuery{Name: "Shared.Example"}, "")
	require.NoError(t, err)
	require.Len(t, byName.Zones, 2)
	assert.Equal(t, acctA, byName.Zones[0].AccountID, "oldest first")

	byID, err := svc.AdminFindZones(ctx, &route53.AdminZoneQuery{ZoneID: a}, "")
	require.NoError(t, err)
	require.Len(t, byID.Zones, 1)
	assert.Equal(t, zoneID(a), byID.Zones[0].ID)
}

func TestAdminOverrideActivatesSuspendsAndReleases(t *testing.T) {
	svc, v, dns, c := newVerifier(t)
	svc.Verifier = v
	held := createZone(t, svc, acctA, "contested.example")
	c.advance(time.Second)
	squat := createZone(t, svc, acctB, "contested.example")

	_, err := svc.AdminOverride(ctx, &route53.AdminOverrideInput{ZoneID: held, Override: route53.StateActive}, "")
	requireCode(t, err, "InvalidInput")

	out, err := svc.AdminOverride(ctx, &route53.AdminOverrideInput{ZoneID: held, Override: route53.StateActive, Reason: "air-gapped root"}, "")
	require.NoError(t, err)
	assert.Equal(t, route53.StateActive, out.Zones[0].State)

	_, err = svc.AdminOverride(ctx, &route53.AdminOverrideInput{ZoneID: squat, Override: route53.StateActive, Reason: "mine"}, "")
	requireCode(t, err, "ConflictingDomainExists")

	// The delegation check never demotes an operator's activation.
	pass(t, v)
	assert.Equal(t, route53.StateActive, zone(t, svc, acctA, held).State)

	_, err = svc.AdminOverride(ctx, &route53.AdminOverrideInput{ZoneID: held, Override: route53.StateSuspended, Reason: "dispute"}, "")
	require.NoError(t, err)
	dns.parents["contested.example"] = ours()
	c.advance(time.Minute)
	pass(t, v)
	assert.Equal(t, route53.StateSuspended, zone(t, svc, acctA, held).State)
	assert.Equal(t, route53.StateActive, zone(t, svc, acctB, squat).State, "suspending a zone frees its name")

	_, err = svc.AdminOverride(ctx, &route53.AdminOverrideInput{ZoneID: held}, "")
	require.NoError(t, err)
	z := zone(t, svc, acctA, held)
	assert.Equal(t, route53.StatePendingNameservers, z.State)
	assert.Empty(t, z.Override)
	pass(t, v)
	assert.Equal(t, "the domain is active in another account", zone(t, svc, acctA, held).StateReason)

	_, err = svc.AdminOverride(ctx, &route53.AdminOverrideInput{ZoneID: "ZNOPE"}, "")
	requireCode(t, err, "NoSuchHostedZone")
	_, err = svc.AdminOverride(ctx, &route53.AdminOverrideInput{ZoneID: held, Override: "BOGUS", Reason: "x"}, "")
	requireCode(t, err, "InvalidInput")
}

func TestAdminVerifyChecksInTheOwningAccount(t *testing.T) {
	svc, v, dns, _ := newVerifier(t)
	id := createZone(t, svc, acctB, "admin.example")
	_, err := svc.AdminVerify(ctx, &route53.AdminZoneQuery{ZoneID: id}, "")
	requireCode(t, err, "ServiceUnavailable")

	svc.Verifier = v
	dns.parents["admin.example"] = ours()
	out, err := svc.AdminVerify(ctx, &route53.AdminZoneQuery{ZoneID: id}, "")
	require.NoError(t, err)
	assert.Equal(t, route53.StateActive, out.Zones[0].State)
	assert.Equal(t, acctB, out.Zones[0].AccountID)
}

// A zone that names a platform zone, such as one created before its name was
// reserved, cannot be activated by the operator either.
func TestAdminOverrideRefusesAReservedName(t *testing.T) {
	svc := newService(t)
	id := createZone(t, svc, acctA, "services.example")
	svc.Reserved = func(name string) bool { return name == "services.example." }

	_, err := svc.AdminOverride(ctx, &route53.AdminOverrideInput{ZoneID: id, Override: route53.StateActive, Reason: "air-gapped root"}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidDomainName)
	z, err := svc.Store().GetZone(ctx, acctA, route53.BareZoneID(id))
	require.NoError(t, err)
	assert.Equal(t, route53.StatePendingNameservers, z.State)
}
