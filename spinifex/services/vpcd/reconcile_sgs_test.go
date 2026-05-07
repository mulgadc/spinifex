package vpcd

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/services/vpcd/nbdb"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sgReconcileAccountID = "000000000001"

// seedVPCBucket puts the given VPC records into the spinifex-vpcs bucket
// using the standard "<accountID>.<vpcID>" key shape.
func seedVPCBucket(t *testing.T, nc *nats.Conn, vpcs []handlers_ec2_vpc.VPCRecord) nats.KeyValue {
	t.Helper()
	js, err := nc.JetStream()
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: handlers_ec2_vpc.KVBucketVPCs, History: 1})
	require.NoError(t, err)
	for _, v := range vpcs {
		data, _ := json.Marshal(v)
		_, err := kv.Put(utils.AccountKey(sgReconcileAccountID, v.VpcId), data)
		require.NoError(t, err)
	}
	return kv
}

func seedSGBucket(t *testing.T, nc *nats.Conn, sgs []handlers_ec2_vpc.SecurityGroupRecord) nats.KeyValue {
	t.Helper()
	js, err := nc.JetStream()
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: handlers_ec2_vpc.KVBucketSecurityGroups, History: 1})
	require.NoError(t, err)
	for _, sg := range sgs {
		data, _ := json.Marshal(sg)
		_, err := kv.Put(utils.AccountKey(sgReconcileAccountID, sg.GroupId), data)
		require.NoError(t, err)
	}
	return kv
}

func seedENIBucket(t *testing.T, nc *nats.Conn, enis []handlers_ec2_vpc.ENIRecord) nats.KeyValue {
	t.Helper()
	js, err := nc.JetStream()
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: handlers_ec2_vpc.KVBucketENIs, History: 1})
	require.NoError(t, err)
	for _, e := range enis {
		data, _ := json.Marshal(e)
		_, err := kv.Put(utils.AccountKey(sgReconcileAccountID, e.NetworkInterfaceId), data)
		require.NoError(t, err)
	}
	return kv
}

// readVPC fetches the current VPC record straight from KV — used by tests to
// assert the reconciler flipped state.
func readVPC(t *testing.T, kv nats.KeyValue, vpcID string) handlers_ec2_vpc.VPCRecord {
	t.Helper()
	entry, err := kv.Get(utils.AccountKey(sgReconcileAccountID, vpcID))
	require.NoError(t, err)
	var rec handlers_ec2_vpc.VPCRecord
	require.NoError(t, json.Unmarshal(entry.Value(), &rec))
	return rec
}

// TestReconcileSGsOnce_PendingVPCGetsDefaultSGAndAvailable covers scan 1's
// happy path: a VPC stuck in pending with no default SG gets one, then is
// flipped to available.
func TestReconcileSGsOnce_PendingVPCGetsDefaultSGAndAvailable(t *testing.T) {
	_, nc := startTestJetStreamNATS(t)
	ovn := NewMockOVNClient()
	require.NoError(t, ovn.Connect(context.Background()))
	topo := NewTopologyHandler(ovn)

	vpcKV := seedVPCBucket(t, nc, []handlers_ec2_vpc.VPCRecord{{
		VpcId: "vpc-pending1", CidrBlock: "10.0.0.0/16", State: "pending",
		CreatedAt: time.Now(),
	}})
	sgKV := seedSGBucket(t, nc, nil)
	seedENIBucket(t, nc, nil)

	// Subscribe vpcd's SG handlers — CreateDefaultSecurityGroupKV publishes
	// vpc.create-sg, and we need the port group to exist for scan 2 to no-op.
	subs, err := topo.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	result := ReconcileSGsOnce(context.Background(), nc, topo)

	assert.Equal(t, 1, result.DefaultSGsCreated, "scan 1 should provision the missing default SG")
	assert.Equal(t, 1, result.VPCsTransitioned, "scan 1 should flip the VPC to available")
	assert.Equal(t, "available", readVPC(t, vpcKV, "vpc-pending1").State)

	// Verify the default SG record landed in the bucket.
	sgID, err := handlers_ec2_vpc.FindDefaultSGForVPCKV(sgKV, sgReconcileAccountID, "vpc-pending1")
	require.NoError(t, err)
	require.NotEmpty(t, sgID, "default SG record must exist after scan 1")
}

// TestReconcileSGsOnce_PendingVPCWithExistingDefaultSG covers the partial-
// failure recovery path: default SG record was written but the prior CreateVpc
// crashed before flipping state. Reconciler should not create a duplicate SG;
// just flip state.
func TestReconcileSGsOnce_PendingVPCWithExistingDefaultSG(t *testing.T) {
	_, nc := startTestJetStreamNATS(t)
	ovn := NewMockOVNClient()
	require.NoError(t, ovn.Connect(context.Background()))
	topo := NewTopologyHandler(ovn)

	vpcKV := seedVPCBucket(t, nc, []handlers_ec2_vpc.VPCRecord{{
		VpcId: "vpc-half1", CidrBlock: "10.0.0.0/16", State: "pending",
		CreatedAt: time.Now(),
	}})
	seedSGBucket(t, nc, []handlers_ec2_vpc.SecurityGroupRecord{{
		GroupId: "sg-existing00000000", GroupName: "default", VpcId: "vpc-half1", IsDefault: true,
		CreatedAt: time.Now(),
	}})
	seedENIBucket(t, nc, nil)

	subs, err := topo.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	result := ReconcileSGsOnce(context.Background(), nc, topo)

	assert.Equal(t, 0, result.DefaultSGsCreated, "must not create a duplicate default SG")
	assert.Equal(t, 1, result.VPCsTransitioned)
	assert.Equal(t, "available", readVPC(t, vpcKV, "vpc-half1").State)
}

// TestReconcileSGsOnce_AvailableVPCSkipped guards against the reconciler
// touching VPCs that are already available — the most common state.
func TestReconcileSGsOnce_AvailableVPCSkipped(t *testing.T) {
	_, nc := startTestJetStreamNATS(t)
	ovn := NewMockOVNClient()
	require.NoError(t, ovn.Connect(context.Background()))
	topo := NewTopologyHandler(ovn)

	seedVPCBucket(t, nc, []handlers_ec2_vpc.VPCRecord{{
		VpcId: "vpc-avail1", State: "available", CreatedAt: time.Now(),
	}})
	seedSGBucket(t, nc, nil)
	seedENIBucket(t, nc, nil)

	result := ReconcileSGsOnce(context.Background(), nc, topo)

	assert.Equal(t, 0, result.VPCsTransitioned)
	assert.Equal(t, 0, result.DefaultSGsCreated)
}

// TestReconcileSGsOnce_RecreatesMissingPortGroup covers scan 2: an SG record
// exists in KV but the corresponding OVN port group is missing (e.g. OVN NB
// was wiped and restored without bouncing vpcd).
func TestReconcileSGsOnce_RecreatesMissingPortGroup(t *testing.T) {
	_, nc := startTestJetStreamNATS(t)
	ovn := NewMockOVNClient()
	require.NoError(t, ovn.Connect(context.Background()))
	topo := NewTopologyHandler(ovn)

	seedVPCBucket(t, nc, nil)
	seedSGBucket(t, nc, []handlers_ec2_vpc.SecurityGroupRecord{{
		GroupId:   "sg-missing0000000",
		GroupName: "web",
		VpcId:     "vpc-x",
		IngressRules: []handlers_ec2_vpc.SGRule{
			{IpProtocol: "tcp", FromPort: 80, ToPort: 80, CidrIp: "0.0.0.0/0"},
		},
		EgressRules: []handlers_ec2_vpc.SGRule{
			{IpProtocol: "-1", FromPort: 0, ToPort: 0, CidrIp: "0.0.0.0/0"},
		},
		CreatedAt: time.Now(),
	}})
	seedENIBucket(t, nc, nil)

	result := ReconcileSGsOnce(context.Background(), nc, topo)

	assert.Equal(t, 1, result.PortGroupsRecreated)
	pg, err := ovn.GetPortGroup(context.Background(), portGroupName("sg-missing0000000"))
	require.NoError(t, err)
	// 2 deny ACLs + 1 ingress allow + 1 egress allow.
	assert.Len(t, pg.ACLs, 4, "deny-ingress + deny-egress + 1 allow ingress + 1 allow egress")
}

// TestReconcileSGsOnce_RecreatesPortGroupIdempotent: a second pass on a fresh
// SG must not double-recreate when the port group already exists.
func TestReconcileSGsOnce_RecreatesPortGroupIdempotent(t *testing.T) {
	_, nc := startTestJetStreamNATS(t)
	ovn := NewMockOVNClient()
	require.NoError(t, ovn.Connect(context.Background()))
	topo := NewTopologyHandler(ovn)

	seedVPCBucket(t, nc, nil)
	seedSGBucket(t, nc, []handlers_ec2_vpc.SecurityGroupRecord{{
		GroupId: "sg-idempotent0000", VpcId: "vpc-x", CreatedAt: time.Now(),
	}})
	seedENIBucket(t, nc, nil)

	r1 := ReconcileSGsOnce(context.Background(), nc, topo)
	assert.Equal(t, 1, r1.PortGroupsRecreated)
	r2 := ReconcileSGsOnce(context.Background(), nc, topo)
	assert.Equal(t, 0, r2.PortGroupsRecreated, "second pass must be a no-op")
}

// TestReconcileSGsOnce_PrunesOrphanPortGroup covers scan 4: an OVN port group
// exists with no matching KV record (e.g. KV was wiped, or DeleteSecurityGroup
// raced ahead of vpcd's handleDeleteSG before it applied).
func TestReconcileSGsOnce_PrunesOrphanPortGroup(t *testing.T) {
	_, nc := startTestJetStreamNATS(t)
	ovn := NewMockOVNClient()
	require.NoError(t, ovn.Connect(context.Background()))
	topo := NewTopologyHandler(ovn)

	// Pre-create an orphan port group named per the SG-prefix convention.
	require.NoError(t, ovn.CreatePortGroup(context.Background(), "sg_orphan000000000", nil))
	require.NoError(t, ovn.CreateAddressSet(context.Background(), "sg_orphan000000000_ip4", nil))

	seedVPCBucket(t, nc, nil)
	seedSGBucket(t, nc, nil)
	seedENIBucket(t, nc, nil)

	result := ReconcileSGsOnce(context.Background(), nc, topo)

	assert.Equal(t, 1, result.OrphanPortGroupsPruned)
	_, err := ovn.GetPortGroup(context.Background(), "sg_orphan000000000")
	assert.Error(t, err, "orphan port group must be deleted")
}

// TestReconcileSGsOnce_PreservesNonSGPortGroups defends scan 4 from collateral
// damage on port groups that don't follow the SG naming convention.
func TestReconcileSGsOnce_PreservesNonSGPortGroups(t *testing.T) {
	_, nc := startTestJetStreamNATS(t)
	ovn := NewMockOVNClient()
	require.NoError(t, ovn.Connect(context.Background()))
	topo := NewTopologyHandler(ovn)

	require.NoError(t, ovn.CreatePortGroup(context.Background(), "system_admins", nil))

	seedVPCBucket(t, nc, nil)
	seedSGBucket(t, nc, nil)
	seedENIBucket(t, nc, nil)

	result := ReconcileSGsOnce(context.Background(), nc, topo)

	assert.Equal(t, 0, result.OrphanPortGroupsPruned)
	_, err := ovn.GetPortGroup(context.Background(), "system_admins")
	assert.NoError(t, err, "non-SG port group must not be touched")
}

// TestReconcileSGsOnce_SyncsENIMembership covers scan 3: an ENI's KV record
// lists SGs but the LSP is in the wrong port groups (or none).
func TestReconcileSGsOnce_SyncsENIMembership(t *testing.T) {
	_, nc := startTestJetStreamNATS(t)
	ovn := NewMockOVNClient()
	require.NoError(t, ovn.Connect(context.Background()))
	topo := NewTopologyHandler(ovn)
	ctx := context.Background()

	// Pre-create the switch the LSP attaches to, then bring up the SG via
	// the normal handler path so the port group + address set get the right
	// names.
	require.NoError(t, ovn.CreateLogicalSwitch(ctx, &nbdb.LogicalSwitch{Name: "subnet-x"}))
	subs, err := topo.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	sgEvt := SGEvent{GroupId: "sg-eni0000000000000", VpcId: "vpc-x"}
	sgData, _ := json.Marshal(sgEvt)
	resp, err := nc.Request(TopicCreateSG, sgData, 2*time.Second)
	require.NoError(t, err)
	assertSuccess(t, resp, "create SG")

	// Create the LSP without joining any port group — that's the drift the
	// reconciler should heal.
	require.NoError(t, ovn.CreateLogicalSwitchPortInGroups(ctx, "subnet-x",
		&nbdb.LogicalSwitchPort{Name: "port-eni-sync1", Addresses: []string{"02:00:00:aa:bb:01 10.0.0.5"}},
		nil, ""))

	seedVPCBucket(t, nc, nil)
	seedSGBucket(t, nc, []handlers_ec2_vpc.SecurityGroupRecord{{
		GroupId: "sg-eni0000000000000", VpcId: "vpc-x", CreatedAt: time.Now(),
	}})
	seedENIBucket(t, nc, []handlers_ec2_vpc.ENIRecord{{
		NetworkInterfaceId: "eni-sync1",
		PrivateIpAddress:   "10.0.0.5",
		MacAddress:         "02:00:00:aa:bb:01",
		SecurityGroupIds:   []string{"sg-eni0000000000000"},
		CreatedAt:          time.Now(),
	}})

	result := ReconcileSGsOnce(ctx, nc, topo)

	assert.Equal(t, 1, result.PortMembershipsSynced)
	groups, err := ovn.ListPortGroupsForPort(ctx, "port-eni-sync1")
	require.NoError(t, err)
	assert.Contains(t, groups, portGroupName("sg-eni0000000000000"),
		"LSP must end up in the SG's port group after reconcile")
}

// TestReconcileSGsOnce_AllScansClean: a fully-converged cluster sees zero work.
func TestReconcileSGsOnce_AllScansClean(t *testing.T) {
	_, nc := startTestJetStreamNATS(t)
	ovn := NewMockOVNClient()
	require.NoError(t, ovn.Connect(context.Background()))
	topo := NewTopologyHandler(ovn)

	seedVPCBucket(t, nc, []handlers_ec2_vpc.VPCRecord{{
		VpcId: "vpc-clean", State: "available", CreatedAt: time.Now(),
	}})
	seedSGBucket(t, nc, nil)
	seedENIBucket(t, nc, nil)

	result := ReconcileSGsOnce(context.Background(), nc, topo)

	assert.Equal(t, SGReconcileResult{}, result, "no drift means no work")
}

// TestReconcileSGsOnce_NoBucketsHandledGracefully exercises the first-boot path
// where no KV buckets exist yet — must not panic and must not error.
func TestReconcileSGsOnce_NoBucketsHandledGracefully(t *testing.T) {
	_, nc := startTestJetStreamNATS(t)
	ovn := NewMockOVNClient()
	require.NoError(t, ovn.Connect(context.Background()))
	topo := NewTopologyHandler(ovn)

	result := ReconcileSGsOnce(context.Background(), nc, topo)
	assert.Equal(t, SGReconcileResult{}, result)
}

// TestReconcileSGsLoop_RunsAtLeastOnceUnderCancellation drives the goroutine
// loop and verifies it converges drift on its first tick then exits cleanly
// when the context is cancelled. Avoids waiting the full 30s by re-using the
// loop's "acquire-on-each-tick" shape.
func TestReconcileSGsLoop_RunsAtLeastOnceUnderCancellation(t *testing.T) {
	_, nc := startTestJetStreamNATS(t)
	ovn := NewMockOVNClient()
	require.NoError(t, ovn.Connect(context.Background()))
	topo := NewTopologyHandler(ovn)

	subs, err := topo.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	vpcKV := seedVPCBucket(t, nc, []handlers_ec2_vpc.VPCRecord{{
		VpcId: "vpc-loop1", State: "pending", CreatedAt: time.Now(),
	}})
	seedSGBucket(t, nc, nil)
	seedENIBucket(t, nc, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		ReconcileSGsLoop(ctx, nc, topo)
		close(done)
	}()

	// The loop runs one pass immediately on entry (before its first ticker
	// wait), so polling for the converged state covers it without sleeping
	// the full interval.
	require.Eventually(t, func() bool {
		return readVPC(t, vpcKV, "vpc-loop1").State == "available"
	}, 5*time.Second, 25*time.Millisecond, "loop should flip pending VPC on first pass")

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ReconcileSGsLoop did not exit within 2s of context cancellation")
	}
}

// TestSGReconcileResult_Changed covers the small accumulator predicate so
// future field additions don't silently regress the loop's "no log spam when
// nothing happened" behavior.
func TestSGReconcileResult_Changed(t *testing.T) {
	assert.False(t, SGReconcileResult{}.changed())
	assert.True(t, (SGReconcileResult{VPCsTransitioned: 1}).changed())
	assert.True(t, (SGReconcileResult{DefaultSGsCreated: 1}).changed())
	assert.True(t, (SGReconcileResult{PortGroupsRecreated: 1}).changed())
	assert.True(t, (SGReconcileResult{PortMembershipsSynced: 1}).changed())
	assert.True(t, (SGReconcileResult{OrphanPortGroupsPruned: 1}).changed())
}

// TestReconcileSGsOnce_VersionAndBadJSONSkipped guards the defensive parsing
// in scanPendingVPCs and listSGRecords — _version keys and malformed entries
// must not break the pass.
func TestReconcileSGsOnce_VersionAndBadJSONSkipped(t *testing.T) {
	_, nc := startTestJetStreamNATS(t)
	ovn := NewMockOVNClient()
	require.NoError(t, ovn.Connect(context.Background()))
	topo := NewTopologyHandler(ovn)

	subs, err := topo.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	js, err := nc.JetStream()
	require.NoError(t, err)

	vpcKV, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: handlers_ec2_vpc.KVBucketVPCs, History: 1})
	require.NoError(t, err)
	_, err = vpcKV.PutString(utils.VersionKey, "1")
	require.NoError(t, err)
	_, err = vpcKV.Put("garbage-vpc", []byte("not-json"))
	require.NoError(t, err)
	good, _ := json.Marshal(handlers_ec2_vpc.VPCRecord{
		VpcId: "vpc-good", State: "pending", CreatedAt: time.Now(),
	})
	_, err = vpcKV.Put(utils.AccountKey(sgReconcileAccountID, "vpc-good"), good)
	require.NoError(t, err)

	sgKV, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: handlers_ec2_vpc.KVBucketSecurityGroups, History: 1})
	require.NoError(t, err)
	_, err = sgKV.PutString(utils.VersionKey, "1")
	require.NoError(t, err)
	_, err = sgKV.Put("garbage-sg", []byte("{invalid"))
	require.NoError(t, err)

	seedENIBucket(t, nc, nil)

	result := ReconcileSGsOnce(context.Background(), nc, topo)
	assert.Equal(t, 1, result.DefaultSGsCreated, "good VPC should still get a default SG")
	assert.Equal(t, 1, result.VPCsTransitioned)
	assert.Equal(t, "available", readVPC(t, vpcKV, "vpc-good").State)
}

// TestReconcileSGsOnce_ENIWithoutLSPLogs covers the warn-and-continue branch
// in scanENIPortMembership: an ENI's KV record references SGs but no OVN LSP
// exists yet (e.g. ReconcileFromKV hasn't caught up). The scan should not
// count this ENI as synced and must not break the pass.
func TestReconcileSGsOnce_ENIWithoutLSPLogs(t *testing.T) {
	_, nc := startTestJetStreamNATS(t)
	ovn := NewMockOVNClient()
	require.NoError(t, ovn.Connect(context.Background()))
	topo := NewTopologyHandler(ovn)

	seedVPCBucket(t, nc, nil)
	seedSGBucket(t, nc, nil)
	seedENIBucket(t, nc, []handlers_ec2_vpc.ENIRecord{{
		NetworkInterfaceId: "eni-no-lsp",
		PrivateIpAddress:   "10.0.0.1",
		SecurityGroupIds:   []string{"sg-something00000"},
		CreatedAt:          time.Now(),
	}})

	result := ReconcileSGsOnce(context.Background(), nc, topo)
	assert.Equal(t, 0, result.PortMembershipsSynced, "ENI with no LSP must not count as synced")
}

// TestAccountIDFromKey covers the small key-parsing helper directly so a
// future shape change (account ID with dots, etc.) is caught here.
func TestAccountIDFromKey(t *testing.T) {
	cases := []struct {
		key, resID, wantAcc string
		wantOK              bool
	}{
		{"000000000001.vpc-abc", "vpc-abc", "000000000001", true},
		{"vpc-abc", "vpc-abc", "", false},
		{".vpc-abc", "vpc-abc", "", false},
		{"acct.vpc-abc", "vpc-other", "", false},
	}
	for _, c := range cases {
		got, ok := accountIDFromKey(c.key, c.resID)
		assert.Equal(t, c.wantOK, ok, "key=%q resID=%q", c.key, c.resID)
		assert.Equal(t, c.wantAcc, got, "key=%q resID=%q", c.key, c.resID)
	}
}
