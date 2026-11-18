package projection

import (
	"context"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	ec2routetable "github.com/mulgadc/spinifex/spinifex/domains/ec2/routetable"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	ec2vpcendpoint "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpcendpoint"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const acct = "111122223333"

func endpointRecord(id, vpc, state string, tables ...string) ec2vpcendpoint.EndpointRecord {
	return ec2vpcendpoint.EndpointRecord{
		ID: id, AccountID: acct, VpcID: vpc, ServiceName: "com.amazonaws.ap-southeast-2.s3",
		Type: ec2vpcendpoint.TypeGateway, RouteTableIds: tables, State: state, Generation: 3,
	}
}

func claimFor(t *testing.T, vpce string) []byte {
	return mustJSON(t, map[string]any{"vpce_id": vpce, "account_id": acct, "claimed_at": time.Now()})
}

func table(id, vpc string, main bool, subnets ...string) ec2routetable.RouteTableRecord {
	rt := ec2routetable.RouteTableRecord{RouteTableId: id, VpcId: vpc, AccountID: acct, IsMain: main}
	for _, s := range subnets {
		rt.Associations = append(rt.Associations, ec2routetable.AssociationRecord{AssociationId: "rtbassoc-" + s, SubnetId: s})
	}
	return rt
}

func subnet(id, vpc string) ec2vpc.SubnetRecord { return ec2vpc.SubnetRecord{SubnetId: id, VpcId: vpc} }

// With no endpoint bucket no subnet routes to an endpoint, and no error.
func TestLoadGatewayEndpoints_NoBucket(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	got, err := LoadGatewayEndpoints(context.Background(), js)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// A subnet routes to an endpoint through its explicit table, or through the
// main table when it has none; every other condition of an effective
// association must hold, or the subnet routes nowhere.
func TestLoadGatewayEndpoints_MapsEffectiveAssociations(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	testutil.SeedKV(t, js, ec2vpcendpoint.KVBucketVpcEndpoints, map[string][]byte{
		acct + ".vpce-main":    mustJSON(t, endpointRecord("vpce-main", "vpc-a", ec2vpcendpoint.StateAvailable, "rtb-main")),
		acct + ".vpce-exp":     mustJSON(t, endpointRecord("vpce-exp", "vpc-a", ec2vpcendpoint.StateAvailable, "rtb-exp")),
		acct + ".vpce-pending": mustJSON(t, endpointRecord("vpce-pending", "vpc-a", ec2vpcendpoint.StatePending, "rtb-pend")),
		acct + ".vpce-unlist":  mustJSON(t, endpointRecord("vpce-unlist", "vpc-a", ec2vpcendpoint.StateAvailable)),
		acct + ".vpce-other":   mustJSON(t, endpointRecord("vpce-other", "vpc-b", ec2vpcendpoint.StateAvailable, "rtb-foreign")),
		acct + ".vpce-gone":    mustJSON(t, endpointRecord("vpce-gone", "vpc-a", ec2vpcendpoint.StateAvailable, "rtb-deleted")),
		"rtb.rtb-main.s3":      claimFor(t, "vpce-main"),
		"rtb.rtb-exp.s3":       claimFor(t, "vpce-exp"),
		"rtb.rtb-pend.s3":      claimFor(t, "vpce-pending"),
		"rtb.rtb-unlist.s3":    claimFor(t, "vpce-unlist"),
		"rtb.rtb-foreign.s3":   claimFor(t, "vpce-other"),
		"rtb.rtb-deleted.s3":   claimFor(t, "vpce-gone"),
		"rtb.rtb-orphan.s3":    claimFor(t, "vpce-absent"),
	})
	testutil.SeedKV(t, js, ec2routetable.KVBucketRouteTables, map[string][]byte{
		acct + ".rtb-main":    mustJSON(t, table("rtb-main", "vpc-a", true)),
		acct + ".rtb-exp":     mustJSON(t, table("rtb-exp", "vpc-a", false, "subnet-exp")),
		acct + ".rtb-pend":    mustJSON(t, table("rtb-pend", "vpc-a", false, "subnet-pend")),
		acct + ".rtb-unlist":  mustJSON(t, table("rtb-unlist", "vpc-a", false, "subnet-unlist")),
		acct + ".rtb-foreign": mustJSON(t, table("rtb-foreign", "vpc-a", false, "subnet-foreign")),
		acct + ".rtb-orphan":  mustJSON(t, table("rtb-orphan", "vpc-a", false, "subnet-orphan")),
	})
	testutil.SeedKV(t, js, ec2vpc.KVBucketSubnets, map[string][]byte{
		acct + ".subnet-main":    mustJSON(t, subnet("subnet-main", "vpc-a")),
		acct + ".subnet-exp":     mustJSON(t, subnet("subnet-exp", "vpc-a")),
		acct + ".subnet-pend":    mustJSON(t, subnet("subnet-pend", "vpc-a")),
		acct + ".subnet-unlist":  mustJSON(t, subnet("subnet-unlist", "vpc-a")),
		acct + ".subnet-foreign": mustJSON(t, subnet("subnet-foreign", "vpc-a")),
		acct + ".subnet-orphan":  mustJSON(t, subnet("subnet-orphan", "vpc-a")),
		acct + ".subnet-vpcb":    mustJSON(t, subnet("subnet-vpcb", "vpc-b")),
	})

	got, err := LoadGatewayEndpoints(context.Background(), js)
	require.NoError(t, err)
	want := func(id string) GatewayEndpoint {
		return GatewayEndpoint{ID: id, AccountID: acct, VpcID: "vpc-a", ServiceName: "com.amazonaws.ap-southeast-2.s3", Generation: 3}
	}
	assert.Equal(t, map[string]GatewayEndpoint{
		"subnet-main": want("vpce-main"),
		"subnet-exp":  want("vpce-exp"),
	}, got, "pending, unlisted, foreign-VPC, deleted-table and orphan-claim associations route nowhere")
}
