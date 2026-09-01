package handlers_ec2_routetable

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- rtbMatchesFilters ---

func TestRtbMatchesFilters_EmptyFilters(t *testing.T) {
	t.Parallel()
	record := &RouteTableRecord{RouteTableId: "rtb-1", VpcId: "vpc-1"}
	assert.True(t, rtbMatchesFilters(record, map[string][]string{}))
}

func TestRtbMatchesFilters_VpcId(t *testing.T) {
	t.Parallel()
	record := &RouteTableRecord{RouteTableId: "rtb-1", VpcId: "vpc-aaa"}
	assert.True(t, rtbMatchesFilters(record, map[string][]string{"vpc-id": {"vpc-aaa"}}))
	assert.False(t, rtbMatchesFilters(record, map[string][]string{"vpc-id": {"vpc-bbb"}}))
}

func TestRtbMatchesFilters_RouteTableId(t *testing.T) {
	t.Parallel()
	record := &RouteTableRecord{RouteTableId: "rtb-xyz", VpcId: "vpc-1"}
	assert.True(t, rtbMatchesFilters(record, map[string][]string{"route-table-id": {"rtb-xyz"}}))
	assert.False(t, rtbMatchesFilters(record, map[string][]string{"route-table-id": {"rtb-other"}}))
}

func TestRtbMatchesFilters_AssociationMain_True(t *testing.T) {
	t.Parallel()
	record := &RouteTableRecord{
		Associations: []AssociationRecord{
			{AssociationId: "rtbassoc-1", Main: true},
		},
	}
	assert.True(t, rtbMatchesFilters(record, map[string][]string{"association.main": {"true"}}))
	assert.False(t, rtbMatchesFilters(record, map[string][]string{"association.main": {"false"}}))
}

func TestRtbMatchesFilters_AssociationMain_False(t *testing.T) {
	t.Parallel()
	record := &RouteTableRecord{
		Associations: []AssociationRecord{
			{AssociationId: "rtbassoc-1", Main: false},
		},
	}
	assert.False(t, rtbMatchesFilters(record, map[string][]string{"association.main": {"true"}}))
}

func TestRtbMatchesFilters_AssociationId(t *testing.T) {
	t.Parallel()
	record := &RouteTableRecord{
		Associations: []AssociationRecord{
			{AssociationId: "rtbassoc-abc"},
		},
	}
	assert.True(t, rtbMatchesFilters(record, map[string][]string{
		"association.route-table-association-id": {"rtbassoc-abc"},
	}))
	assert.False(t, rtbMatchesFilters(record, map[string][]string{
		"association.route-table-association-id": {"rtbassoc-other"},
	}))
}

func TestRtbMatchesFilters_AssociationSubnetId(t *testing.T) {
	t.Parallel()
	record := &RouteTableRecord{
		Associations: []AssociationRecord{
			{AssociationId: "rtbassoc-1", SubnetId: "subnet-aaa"},
		},
	}
	assert.True(t, rtbMatchesFilters(record, map[string][]string{
		"association.subnet-id": {"subnet-aaa"},
	}))
	assert.False(t, rtbMatchesFilters(record, map[string][]string{
		"association.subnet-id": {"subnet-bbb"},
	}))
}

func TestRtbMatchesFilters_RouteDestinationCidr(t *testing.T) {
	t.Parallel()
	record := &RouteTableRecord{
		Routes: []RouteRecord{
			{DestinationCidrBlock: "10.0.0.0/16", GatewayId: "local", State: "active"},
			{DestinationCidrBlock: "0.0.0.0/0", GatewayId: "igw-123", State: "active"},
		},
	}
	assert.True(t, rtbMatchesFilters(record, map[string][]string{
		"route.destination-cidr-block": {"0.0.0.0/0"},
	}))
	assert.False(t, rtbMatchesFilters(record, map[string][]string{
		"route.destination-cidr-block": {"192.168.0.0/16"},
	}))
}

func TestRtbMatchesFilters_RouteGatewayId(t *testing.T) {
	t.Parallel()
	record := &RouteTableRecord{
		Routes: []RouteRecord{
			{DestinationCidrBlock: "0.0.0.0/0", GatewayId: "igw-abc", State: "active"},
		},
	}
	assert.True(t, rtbMatchesFilters(record, map[string][]string{
		"route.gateway-id": {"igw-abc"},
	}))
	assert.False(t, rtbMatchesFilters(record, map[string][]string{
		"route.gateway-id": {"igw-other"},
	}))
}

func TestRtbMatchesFilters_MultipleFilters(t *testing.T) {
	t.Parallel()
	record := &RouteTableRecord{
		RouteTableId: "rtb-1",
		VpcId:        "vpc-aaa",
		Routes: []RouteRecord{
			{DestinationCidrBlock: "0.0.0.0/0", GatewayId: "igw-123", State: "active"},
		},
	}
	// Both match -> true
	assert.True(t, rtbMatchesFilters(record, map[string][]string{
		"vpc-id":           {"vpc-aaa"},
		"route.gateway-id": {"igw-123"},
	}))
	// One mismatches -> false
	assert.False(t, rtbMatchesFilters(record, map[string][]string{
		"vpc-id":           {"vpc-aaa"},
		"route.gateway-id": {"igw-other"},
	}))
}

func TestRtbMatchesFilters_MultipleValues(t *testing.T) {
	t.Parallel()
	record := &RouteTableRecord{VpcId: "vpc-bbb"}
	assert.True(t, rtbMatchesFilters(record, map[string][]string{
		"vpc-id": {"vpc-aaa", "vpc-bbb", "vpc-ccc"},
	}))
}

func TestRtbMatchesFilters_NoAssociations(t *testing.T) {
	t.Parallel()
	record := &RouteTableRecord{RouteTableId: "rtb-1"}
	assert.False(t, rtbMatchesFilters(record, map[string][]string{
		"association.subnet-id": {"subnet-1"},
	}))
}

func TestRtbMatchesFilters_NoRoutes(t *testing.T) {
	t.Parallel()
	record := &RouteTableRecord{RouteTableId: "rtb-1"}
	assert.False(t, rtbMatchesFilters(record, map[string][]string{
		"route.destination-cidr-block": {"0.0.0.0/0"},
	}))
}

func TestRtbMatchesFilters_UnknownFilter(t *testing.T) {
	t.Parallel()
	record := &RouteTableRecord{RouteTableId: "rtb-1", VpcId: "vpc-1"}
	assert.False(t, rtbMatchesFilters(record, map[string][]string{
		"bogus-filter": {"value"},
	}))
}

func TestRtbMatchesFilters_Wildcard(t *testing.T) {
	t.Parallel()
	record := &RouteTableRecord{RouteTableId: "rtb-1", VpcId: "vpc-aaa"}
	assert.True(t, rtbMatchesFilters(record, map[string][]string{
		"vpc-id": {"vpc-*"},
	}))
}

func TestRtbMatchesFilters_TagFilter(t *testing.T) {
	t.Parallel()
	record := &RouteTableRecord{
		RouteTableId: "rtb-1",
		VpcId:        "vpc-1",
		Tags:         map[string]string{"Name": "my-rt", "env": "prod"},
	}
	assert.True(t, rtbMatchesFilters(record, map[string][]string{
		"tag:Name": {"my-rt"},
	}))
	assert.False(t, rtbMatchesFilters(record, map[string][]string{
		"tag:Name": {"other"},
	}))
	assert.False(t, rtbMatchesFilters(record, map[string][]string{
		"tag:missing": {"value"},
	}))
}

// --- recordToEC2 ---

func TestRecordToEC2_Basic(t *testing.T) {
	t.Parallel()
	record := &RouteTableRecord{
		RouteTableId: "rtb-abc",
		VpcId:        "vpc-123",
		AccountID:    "111222333444",
	}
	rtb := recordToEC2(record)
	assert.Equal(t, "rtb-abc", *rtb.RouteTableId)
	assert.Equal(t, "vpc-123", *rtb.VpcId)
	assert.Equal(t, "111222333444", *rtb.OwnerId)
	assert.Empty(t, rtb.Routes)
	assert.Empty(t, rtb.Associations)
	assert.Empty(t, rtb.Tags)
}

func TestRecordToEC2_Routes(t *testing.T) {
	t.Parallel()
	record := &RouteTableRecord{
		RouteTableId: "rtb-1",
		VpcId:        "vpc-1",
		AccountID:    "acct",
		Routes: []RouteRecord{
			{DestinationCidrBlock: "10.0.0.0/16", GatewayId: "local", State: "active", Origin: "CreateRouteTable"},
			{DestinationCidrBlock: "0.0.0.0/0", GatewayId: "igw-123", State: "active", Origin: "CreateRoute"},
			{DestinationCidrBlock: "192.168.0.0/16", NatGatewayId: "nat-abc", State: "active", Origin: "CreateRoute"},
		},
	}
	rtb := recordToEC2(record)
	require.Len(t, rtb.Routes, 3)

	assert.Equal(t, "10.0.0.0/16", *rtb.Routes[0].DestinationCidrBlock)
	assert.Equal(t, "local", *rtb.Routes[0].GatewayId)
	assert.Nil(t, rtb.Routes[0].NatGatewayId)

	assert.Equal(t, "igw-123", *rtb.Routes[1].GatewayId)

	assert.Equal(t, "nat-abc", *rtb.Routes[2].NatGatewayId)
	assert.Nil(t, rtb.Routes[2].GatewayId)
}

func TestRecordToEC2_Associations(t *testing.T) {
	t.Parallel()
	record := &RouteTableRecord{
		RouteTableId: "rtb-1",
		VpcId:        "vpc-1",
		AccountID:    "acct",
		Associations: []AssociationRecord{
			{AssociationId: "rtbassoc-main", Main: true},
			{AssociationId: "rtbassoc-sub", SubnetId: "subnet-aaa", Main: false},
		},
	}
	rtb := recordToEC2(record)
	require.Len(t, rtb.Associations, 2)

	assert.Equal(t, "rtbassoc-main", *rtb.Associations[0].RouteTableAssociationId)
	assert.True(t, *rtb.Associations[0].Main)
	assert.Nil(t, rtb.Associations[0].SubnetId)
	assert.Equal(t, "associated", *rtb.Associations[0].AssociationState.State)

	assert.Equal(t, "rtbassoc-sub", *rtb.Associations[1].RouteTableAssociationId)
	assert.False(t, *rtb.Associations[1].Main)
	assert.Equal(t, "subnet-aaa", *rtb.Associations[1].SubnetId)
}

func TestRecordToEC2_Tags(t *testing.T) {
	t.Parallel()
	record := &RouteTableRecord{
		RouteTableId: "rtb-1",
		VpcId:        "vpc-1",
		AccountID:    "acct",
		Tags:         map[string]string{"Name": "my-rt", "env": "prod"},
	}
	rtb := recordToEC2(record)
	require.Len(t, rtb.Tags, 2)

	tagMap := make(map[string]string)
	for _, tag := range rtb.Tags {
		tagMap[aws.StringValue(tag.Key)] = aws.StringValue(tag.Value)
	}
	assert.Equal(t, "my-rt", tagMap["Name"])
	assert.Equal(t, "prod", tagMap["env"])
}
