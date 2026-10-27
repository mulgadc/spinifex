//go:build integration

package integration

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/route53"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A hosted zone is visible to its own account only: another tenant gets
// NoSuchHostedZone for every call naming it, exactly as for a zone that does
// not exist, and its own listing stays empty.
func TestRoute53_HostedZonesAreAccountScoped(t *testing.T) {
	gw := StartGateway(t)
	StartServiceDaemonLite(t, gw)
	owner := createTenantAccount(t, gw, "route53-owner").Route53
	other := createTenantAccount(t, gw, "route53-other").Route53

	created, err := owner.CreateHostedZone(&route53.CreateHostedZoneInput{
		Name: aws.String("example.com"), CallerReference: aws.String("route53-scoping"),
	})
	require.NoError(t, err)
	zoneID := created.HostedZone.Id
	assert.Equal(t, []string{"ns1.spx3.net"}, aws.StringValueSlice(created.DelegationSet.NameServers))

	_, err = owner.ChangeResourceRecordSets(&route53.ChangeResourceRecordSetsInput{
		HostedZoneId: zoneID,
		ChangeBatch: &route53.ChangeBatch{Changes: []*route53.Change{{
			Action: aws.String("CREATE"),
			ResourceRecordSet: &route53.ResourceRecordSet{
				Name: aws.String("www.example.com"), Type: aws.String("A"), TTL: aws.Int64(300),
				ResourceRecords: []*route53.ResourceRecord{{Value: aws.String("192.0.2.10")}},
			},
		}}},
	})
	require.NoError(t, err)

	_, err = other.GetHostedZone(&route53.GetHostedZoneInput{Id: zoneID})
	assertAWSErrorCode(t, err, "NoSuchHostedZone", "foreign GetHostedZone")
	_, err = other.ListResourceRecordSets(&route53.ListResourceRecordSetsInput{HostedZoneId: zoneID})
	assertAWSErrorCode(t, err, "NoSuchHostedZone", "foreign ListResourceRecordSets")
	_, err = other.DeleteHostedZone(&route53.DeleteHostedZoneInput{Id: zoneID})
	assertAWSErrorCode(t, err, "NoSuchHostedZone", "foreign DeleteHostedZone")
	listed, err := other.ListHostedZones(&route53.ListHostedZonesInput{})
	require.NoError(t, err)
	assert.Empty(t, listed.HostedZones)

	sets, err := owner.ListResourceRecordSets(&route53.ListResourceRecordSetsInput{HostedZoneId: zoneID})
	require.NoError(t, err)
	assert.Len(t, sets.ResourceRecordSets, 3, "NS, SOA and the A record")
}
