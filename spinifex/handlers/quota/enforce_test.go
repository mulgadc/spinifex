package handlers_quota_test

import (
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_quota "github.com/mulgadc/spinifex/spinifex/handlers/quota"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// stubDescribe answers one describe subject with a fixed payload, standing in
// for the daemon so the enforcement path can be driven end to end without one.
func stubDescribe(t *testing.T, nc *nats.Conn, subject string, reply any) {
	t.Helper()
	data, err := json.Marshal(reply)
	require.NoError(t, err)

	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	require.NoError(t, nc.Flush())
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

// enforceService builds an enabled Service with per-dimension caps low enough
// that a handful of resources crosses them.
func enforceService(limits handlers_quota.Limits) *handlers_quota.Service {
	return handlers_quota.New(limits, nil)
}

func vpcList(n int) *ec2.DescribeVpcsOutput {
	out := &ec2.DescribeVpcsOutput{}
	for i := range n {
		out.Vpcs = append(out.Vpcs, &ec2.Vpc{VpcId: aws.String("vpc-" + string(rune('a'+i)))})
	}
	return out
}

// The account is one below its cap, so one more is allowed and two are not.
func TestEnforceVPCsAtBoundary(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	stubDescribe(t, nc, "ec2.DescribeVpcs", vpcList(3))
	svc := enforceService(handlers_quota.Limits{Enabled: true, VPCs: 4})

	require.NoError(t, svc.EnforceVPCs(t.Context(), nc, "000000000002", 1))

	err := svc.EnforceVPCs(t.Context(), nc, "000000000002", 2)
	require.Error(t, err)
	require.Equal(t, awserrors.ErrorResourceLimitExceeded, err.Error())
}

// An account already at its cap must be refused outright.
func TestEnforceVPCsAtCap(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	stubDescribe(t, nc, "ec2.DescribeVpcs", vpcList(4))
	svc := enforceService(handlers_quota.Limits{Enabled: true, VPCs: 4})

	err := svc.EnforceVPCs(t.Context(), nc, "000000000002", 1)
	require.Error(t, err)
	require.Equal(t, awserrors.ErrorResourceLimitExceeded, err.Error())
}

func TestEnforceSubnets(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	stubDescribe(t, nc, "ec2.DescribeSubnets", &ec2.DescribeSubnetsOutput{
		Subnets: []*ec2.Subnet{{SubnetId: aws.String("subnet-a")}, {SubnetId: aws.String("subnet-b")}},
	})
	svc := enforceService(handlers_quota.Limits{Enabled: true, Subnets: 2})

	require.Error(t, svc.EnforceSubnets(t.Context(), nc, "000000000002", 1))
}

func TestEnforceEIPs(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	stubDescribe(t, nc, "ec2.DescribeAddresses", &ec2.DescribeAddressesOutput{
		Addresses: []*ec2.Address{{PublicIp: aws.String("203.0.113.1")}},
	})
	svc := enforceService(handlers_quota.Limits{Enabled: true, EIPs: 4})

	require.NoError(t, svc.EnforceEIPs(t.Context(), nc, "000000000002", 3))
	require.Error(t, svc.EnforceEIPs(t.Context(), nc, "000000000002", 4))
}

func TestEnforceLoadBalancers(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	stubDescribe(t, nc, "elbv2.DescribeLoadBalancers", &elbv2.DescribeLoadBalancersOutput{
		LoadBalancers: []*elbv2.LoadBalancer{{LoadBalancerArn: aws.String("arn:a")}, {LoadBalancerArn: aws.String("arn:b")}},
	})
	svc := enforceService(handlers_quota.Limits{Enabled: true, LoadBalancers: 2})

	err := svc.EnforceLoadBalancers(t.Context(), nc, "000000000002", 1)
	require.Error(t, err)
	require.Equal(t, awserrors.ErrorResourceLimitExceeded, err.Error())
}

func TestEnforceRDSInstances(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	stubDescribe(t, nc, "rds.DescribeDBInstances", &rds.DescribeDBInstancesOutput{
		DBInstances: []*rds.DBInstance{{DBInstanceIdentifier: aws.String("db-a")}},
	})
	svc := enforceService(handlers_quota.Limits{Enabled: true, RDSInstances: 2})

	require.NoError(t, svc.EnforceRDSInstances(t.Context(), nc, "000000000002", 1))
	require.Error(t, svc.EnforceRDSInstances(t.Context(), nc, "000000000002", 2))
}

// The volume count and the capacity sum are separate caps, and either alone
// must be able to refuse a create.
func TestEnforceVolumeCreateCountAndCapacity(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	stubDescribe(t, nc, "ec2.DescribeVolumes", &ec2.DescribeVolumesOutput{
		Volumes: []*ec2.Volume{
			{VolumeId: aws.String("vol-a"), Size: aws.Int64(10)},
			{VolumeId: aws.String("vol-b"), Size: aws.Int64(10)},
		},
	})

	countBound := enforceService(handlers_quota.Limits{Enabled: true, Volumes: 2, VolumesGiB: 1000})
	err := countBound.EnforceVolumeCreate(t.Context(), nc, "000000000002", 1)
	require.Error(t, err, "the count cap must refuse even a tiny volume")
	require.Equal(t, awserrors.ErrorResourceLimitExceeded, err.Error())

	capacityBound := enforceService(handlers_quota.Limits{Enabled: true, Volumes: 16, VolumesGiB: 25})
	require.NoError(t, capacityBound.EnforceVolumeCreate(t.Context(), nc, "000000000002", 5))
	require.Error(t, capacityBound.EnforceVolumeCreate(t.Context(), nc, "000000000002", 6))
}

func TestEnforceVolumeModify(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	stubDescribe(t, nc, "ec2.DescribeVolumes", &ec2.DescribeVolumesOutput{
		Volumes: []*ec2.Volume{
			{VolumeId: aws.String("vol-a"), Size: aws.Int64(10)},
			{VolumeId: aws.String("vol-b"), Size: aws.Int64(10)},
		},
	})
	svc := enforceService(handlers_quota.Limits{Enabled: true, Volumes: 16, VolumesGiB: 40})

	require.NoError(t, svc.EnforceVolumeModify(t.Context(), nc, "000000000002", "vol-a", 30))
	require.Error(t, svc.EnforceVolumeModify(t.Context(), nc, "000000000002", "vol-a", 31))
}

// A describe that cannot be answered must fail the check rather than be read as
// "the account holds nothing", which would wave every create past the cap.
func TestEnforceFailsWhenDescribeFails(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	svc := enforceService(handlers_quota.Limits{Enabled: true, VPCs: 4, EIPs: 4, Subnets: 4, LoadBalancers: 2, RDSInstances: 2, Volumes: 4})

	require.Error(t, svc.EnforceVPCs(t.Context(), nc, "000000000002", 1))
	require.Error(t, svc.EnforceSubnets(t.Context(), nc, "000000000002", 1))
	require.Error(t, svc.EnforceEIPs(t.Context(), nc, "000000000002", 1))
	require.Error(t, svc.EnforceLoadBalancers(t.Context(), nc, "000000000002", 1))
	require.Error(t, svc.EnforceRDSInstances(t.Context(), nc, "000000000002", 1))
	require.Error(t, svc.EnforceVolumeCreate(t.Context(), nc, "000000000002", 1))
}
