package handlers_ec2_vpc_test

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newNodeServices starts one JetStream and returns n VPC services, each on its
// own connection, standing in for the daemons of an n-node cluster.
func newNodeServices(t *testing.T, n int) ([]*handlers_ec2_vpc.VPCServiceImpl, *nats.Conn, jetstream.JetStream) {
	t.Helper()
	ns, nc, js := testutil.StartTestJetStream(t)
	testutil.StubVpcdSGResponder(t, nc)

	svcs := make([]*handlers_ec2_vpc.VPCServiceImpl, n)
	for i := range n {
		conn, err := nats.Connect(ns.ClientURL())
		require.NoError(t, err)
		t.Cleanup(conn.Close)
		svcs[i], err = handlers_ec2_vpc.NewVPCServiceImplWithNATS(t.Context(), nil, conn)
		require.NoError(t, err)
	}
	return svcs, nc, js
}

// accountRecords decodes every record the account holds in bucket.
func accountRecords[T any](t *testing.T, js jetstream.JetStream, bucket, accountID string) []T {
	t.Helper()
	kv, err := js.KeyValue(t.Context(), bucket)
	require.NoError(t, err)
	keys, err := kv.Keys(t.Context())
	if errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil
	}
	require.NoError(t, err)

	var out []T
	for _, key := range keys {
		if !strings.HasPrefix(key, accountID+".") {
			continue
		}
		entry, err := kv.Get(t.Context(), key)
		require.NoError(t, err)
		var record T
		require.NoError(t, json.Unmarshal(entry.Value(), &record))
		out = append(out, record)
	}
	return out
}

func putRecord(t *testing.T, js jetstream.JetStream, bucket, key string, value any) {
	t.Helper()
	kv, err := js.KeyValue(t.Context(), bucket)
	require.NoError(t, err)
	data, err := json.Marshal(value)
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), key, data)
	require.NoError(t, err)
}

type mainRouteTable struct {
	RouteTableId string `json:"route_table_id"`
	VpcId        string `json:"vpc_id"`
	IsMain       bool   `json:"is_main"`
}

func TestEnsureDefaultVPC_RacingNodesBuildOneSetPerAccount(t *testing.T) {
	t.Parallel()
	svcs, nc, js := newNodeServices(t, 4)

	var vpcCreates atomic.Int32
	sub, err := nc.Subscribe("vpc.create", func(*nats.Msg) { vpcCreates.Add(1) })
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, nc.Flush())

	accounts := []string{"000000000041", "000000000042", "000000000043", "000000000044", "000000000045", "000000000046"}
	infos := make(map[string][]*handlers_ec2_vpc.DefaultVPCInfo)
	var mu sync.Mutex
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, accountID := range accounts {
		for _, svc := range svcs {
			wg.Go(func() {
				<-start
				info, err := svc.EnsureDefaultVPC(accountID)
				assert.NoError(t, err)
				mu.Lock()
				infos[accountID] = append(infos[accountID], info)
				mu.Unlock()
			})
		}
	}
	close(start)
	wg.Wait()

	vnis := map[int64]string{}
	for _, accountID := range accounts {
		got := infos[accountID]
		require.Len(t, got, len(svcs))
		require.NotNil(t, got[0])
		for _, info := range got[1:] {
			assert.Equal(t, got[0], info, "every node must report the same default resources")
		}

		vpcs := accountRecords[handlers_ec2_vpc.VPCRecord](t, js, handlers_ec2_vpc.KVBucketVPCs, accountID)
		require.Len(t, vpcs, 1, "account %s", accountID)
		assert.Equal(t, got[0].VpcId, vpcs[0].VpcId)
		_, dup := vnis[vpcs[0].VNI]
		assert.False(t, dup, "VNI %d allocated to two default VPCs", vpcs[0].VNI)
		vnis[vpcs[0].VNI] = accountID

		subnets := accountRecords[handlers_ec2_vpc.SubnetRecord](t, js, handlers_ec2_vpc.KVBucketSubnets, accountID)
		require.Len(t, subnets, 1, "account %s", accountID)
		assert.Equal(t, got[0].SubnetId, subnets[0].SubnetId)

		sgs := accountRecords[handlers_ec2_vpc.SecurityGroupRecord](t, js, handlers_ec2_vpc.KVBucketSecurityGroups, accountID)
		assert.Len(t, sgs, 1, "account %s", accountID)

		rtbs := accountRecords[mainRouteTable](t, js, "spinifex-vpc-route-tables", accountID)
		assert.Len(t, rtbs, 1, "account %s", accountID)
	}

	require.Eventually(t, func() bool { return vpcCreates.Load() >= int32(len(accounts)) }, 5*time.Second, 10*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, int32(len(accounts)), vpcCreates.Load(), "vpcd must be told about each default VPC once")
}

func TestEnsureDefaultVPC_AdoptsOldestExistingDefault(t *testing.T) {
	t.Parallel()
	svcs, _, js := newNodeServices(t, 1)
	svc := svcs[0]

	const accountID = "000000000051"
	created := time.Date(2026, 9, 11, 14, 13, 40, 0, time.UTC)
	for i, id := range []string{"vpc-0000000000000newer", "vpc-0000000000000older"} {
		putRecord(t, js, handlers_ec2_vpc.KVBucketVPCs, utils.AccountKey(accountID, id), handlers_ec2_vpc.VPCRecord{
			VpcId: id, CidrBlock: handlers_ec2_vpc.DefaultVPCCidr, IsDefault: true, VNI: int64(200 + i),
			CreatedAt: created.Add(time.Duration(1-i) * time.Minute),
		})
	}
	putRecord(t, js, handlers_ec2_vpc.KVBucketSubnets, utils.AccountKey(accountID, "subnet-00000000000older"), handlers_ec2_vpc.SubnetRecord{
		SubnetId: "subnet-00000000000older", VpcId: "vpc-0000000000000older", CidrBlock: handlers_ec2_vpc.DefaultSubnetCidr,
		IsDefault: true, CreatedAt: created,
	})

	for range 2 {
		info, err := svc.EnsureDefaultVPC(accountID)
		require.NoError(t, err)
		assert.Equal(t, "vpc-0000000000000older", info.VpcId)
		assert.Equal(t, "subnet-00000000000older", info.SubnetId)
		assert.NotEmpty(t, info.InternetGatewayId)
	}

	assert.Len(t, accountRecords[handlers_ec2_vpc.VPCRecord](t, js, handlers_ec2_vpc.KVBucketVPCs, accountID), 2,
		"adopting an existing default VPC must not build another")
}

func TestEnsureDefaultVPC_FinishesHalfBuiltClaim(t *testing.T) {
	t.Parallel()
	svcs, _, js := newNodeServices(t, 1)
	svc := svcs[0]

	// A node wrote the claim and the VPC record, then died.
	const accountID = "000000000052"
	putRecord(t, js, handlers_ec2_vpc.KVBucketDefaultVPCs, accountID, map[string]any{
		"vpc_id": "vpc-00000000000000half", "subnet_id": "subnet-0000000000half", "group_id": "sg-000000000000half",
		"route_table_id": "rtb-00000000000half", "internet_gateway_id": "igw-00000000000half",
		"cidr": handlers_ec2_vpc.DefaultVPCCidr, "subnet_cidr": handlers_ec2_vpc.DefaultSubnetCidr, "vni": 4242,
	})
	putRecord(t, js, handlers_ec2_vpc.KVBucketVPCs, utils.AccountKey(accountID, "vpc-00000000000000half"), handlers_ec2_vpc.VPCRecord{
		VpcId: "vpc-00000000000000half", CidrBlock: handlers_ec2_vpc.DefaultVPCCidr, IsDefault: true, VNI: 4242,
	})

	pending, err := svc.DefaultVPC(t.Context(), accountID)
	require.NoError(t, err)
	assert.Nil(t, pending, "an unfinished claim must not be reported as the default VPC")

	info, err := svc.EnsureDefaultVPC(accountID)
	require.NoError(t, err)
	assert.Equal(t, &handlers_ec2_vpc.DefaultVPCInfo{
		VpcId: "vpc-00000000000000half", SubnetId: "subnet-0000000000half", Cidr: handlers_ec2_vpc.DefaultVPCCidr,
		SubnetCidr: handlers_ec2_vpc.DefaultSubnetCidr, InternetGatewayId: "igw-00000000000half",
	}, info)

	subnets := accountRecords[handlers_ec2_vpc.SubnetRecord](t, js, handlers_ec2_vpc.KVBucketSubnets, accountID)
	require.Len(t, subnets, 1)
	assert.Equal(t, "subnet-0000000000half", subnets[0].SubnetId)
	sgs := accountRecords[handlers_ec2_vpc.SecurityGroupRecord](t, js, handlers_ec2_vpc.KVBucketSecurityGroups, accountID)
	require.Len(t, sgs, 1)
	assert.Equal(t, "sg-000000000000half", sgs[0].GroupId)
	rtbs := accountRecords[mainRouteTable](t, js, "spinifex-vpc-route-tables", accountID)
	require.Len(t, rtbs, 1)
	assert.Equal(t, "rtb-00000000000half", rtbs[0].RouteTableId)

	done, err := svc.DefaultVPC(t.Context(), accountID)
	require.NoError(t, err)
	assert.Equal(t, info, done)
}

func TestEnsureDefaultVPC_RebuildsDeletedDefaultVPC(t *testing.T) {
	t.Parallel()
	svcs, _, js := newNodeServices(t, 1)
	svc := svcs[0]

	const accountID = "000000000053"
	first, err := svc.EnsureDefaultVPC(accountID)
	require.NoError(t, err)
	_, err = svc.DeleteSubnet(t.Context(), &ec2.DeleteSubnetInput{SubnetId: aws.String(first.SubnetId)}, accountID)
	require.NoError(t, err)
	_, err = svc.DeleteVpc(t.Context(), &ec2.DeleteVpcInput{VpcId: aws.String(first.VpcId)}, accountID)
	require.NoError(t, err)

	gone, err := svc.DefaultVPC(t.Context(), accountID)
	require.NoError(t, err)
	assert.Nil(t, gone, "deleting the default VPC must release its claim")

	second, err := svc.EnsureDefaultVPC(accountID)
	require.NoError(t, err)
	assert.NotEqual(t, first.VpcId, second.VpcId)
	vpcs := accountRecords[handlers_ec2_vpc.VPCRecord](t, js, handlers_ec2_vpc.KVBucketVPCs, accountID)
	require.Len(t, vpcs, 1)
	assert.Equal(t, second.VpcId, vpcs[0].VpcId)
}

func TestEnsureDefaultVPC_RetiresClaimWhoseVPCIsGone(t *testing.T) {
	t.Parallel()
	svcs, _, js := newNodeServices(t, 1)
	svc := svcs[0]

	const accountID = "000000000054"
	putRecord(t, js, handlers_ec2_vpc.KVBucketDefaultVPCs, accountID, map[string]any{
		"vpc_id": "vpc-00000000000000gone", "internet_gateway_id": "igw-00000000000gone",
		"cidr": handlers_ec2_vpc.DefaultVPCCidr, "complete": true,
	})

	info, err := svc.EnsureDefaultVPC(accountID)
	require.NoError(t, err)
	assert.NotEqual(t, "vpc-00000000000000gone", info.VpcId)
	vpcs := accountRecords[handlers_ec2_vpc.VPCRecord](t, js, handlers_ec2_vpc.KVBucketVPCs, accountID)
	require.Len(t, vpcs, 1)
	assert.Equal(t, info.VpcId, vpcs[0].VpcId)
}

func TestEnsureDefaultVPC_UsesBootstrapIDs(t *testing.T) {
	t.Parallel()
	svcs, _, _ := newNodeServices(t, 2)

	ids := handlers_ec2_vpc.BootstrapIDs{VpcId: "vpc-0000000000000boot", SubnetId: "subnet-000000000boot", IgwId: "igw-0000000000000boot"}
	for i, svc := range svcs {
		info, err := svc.EnsureDefaultVPC("000000000001", ids)
		require.NoError(t, err, "node %d", i)
		assert.Equal(t, ids.VpcId, info.VpcId)
		assert.Equal(t, ids.SubnetId, info.SubnetId)
		assert.Equal(t, ids.IgwId, info.InternetGatewayId)
	}
}
