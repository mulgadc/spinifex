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

// TestReconcileSGsOnce_RecreatesMissingPortGroup: an SG record exists in KV
// but the corresponding OVN port group is missing (e.g. OVN NB was wiped).
func TestReconcileSGsOnce_RecreatesMissingPortGroup(t *testing.T) {
	_, nc := startTestJetStreamNATS(t)
	ovn := NewMockOVNClient()
	require.NoError(t, ovn.Connect(context.Background()))
	topo := NewTopologyHandler(ovn)

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
	assert.Len(t, pg.ACLs, 6, "deny-ingress + deny-egress + dhcp-egress + dhcp-ingress + 1 allow ingress + 1 allow egress")
}

// TestReconcileSGsOnce_RecreatesPortGroupIdempotent: a second pass on a fresh
// SG must not double-recreate when the port group already exists.
func TestReconcileSGsOnce_RecreatesPortGroupIdempotent(t *testing.T) {
	_, nc := startTestJetStreamNATS(t)
	ovn := NewMockOVNClient()
	require.NoError(t, ovn.Connect(context.Background()))
	topo := NewTopologyHandler(ovn)

	seedSGBucket(t, nc, []handlers_ec2_vpc.SecurityGroupRecord{{
		GroupId: "sg-idempotent0000", VpcId: "vpc-x", CreatedAt: time.Now(),
	}})
	seedENIBucket(t, nc, nil)

	r1 := ReconcileSGsOnce(context.Background(), nc, topo)
	assert.Equal(t, 1, r1.PortGroupsRecreated)
	r2 := ReconcileSGsOnce(context.Background(), nc, topo)
	assert.Equal(t, 0, r2.PortGroupsRecreated, "second pass must be a no-op")
}

// TestReconcileSGsOnce_SyncsENIMembership: an ENI's KV record lists SGs but
// the LSP is in the wrong port groups (or none).
func TestReconcileSGsOnce_SyncsENIMembership(t *testing.T) {
	_, nc := startTestJetStreamNATS(t)
	ovn := NewMockOVNClient()
	require.NoError(t, ovn.Connect(context.Background()))
	topo := NewTopologyHandler(ovn)
	ctx := context.Background()

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

	// Create the LSP without joining any port group — that's the drift.
	require.NoError(t, ovn.CreateLogicalSwitchPortInGroups(ctx, "subnet-x",
		&nbdb.LogicalSwitchPort{Name: "port-eni-sync1", Addresses: []string{"02:00:00:aa:bb:01 10.0.0.5"}},
		nil))

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

	seedSGBucket(t, nc, nil)
	seedENIBucket(t, nc, nil)

	result := ReconcileSGsOnce(context.Background(), nc, topo)

	assert.Equal(t, SGReconcileResult{}, result, "no drift means no work")
}

// TestReconcileSGsOnce_NoBucketsHandledGracefully: first-boot path where no
// KV buckets exist yet.
func TestReconcileSGsOnce_NoBucketsHandledGracefully(t *testing.T) {
	_, nc := startTestJetStreamNATS(t)
	ovn := NewMockOVNClient()
	require.NoError(t, ovn.Connect(context.Background()))
	topo := NewTopologyHandler(ovn)

	result := ReconcileSGsOnce(context.Background(), nc, topo)
	assert.Equal(t, SGReconcileResult{}, result)
}

// TestReconcileSGsOnce_NoDriftZeroSync: a fully converged ENI (LSP already in
// the right port group, IP already in the address set) must not be counted as
// synced. Otherwise the steady-state cluster logs phantom drift every cycle
// (Plan I.8 — "Constant churn means the reconciler is misclassifying steady
// state as drift").
func TestReconcileSGsOnce_NoDriftZeroSync(t *testing.T) {
	_, nc := startTestJetStreamNATS(t)
	ovn := NewMockOVNClient()
	require.NoError(t, ovn.Connect(context.Background()))
	topo := NewTopologyHandler(ovn)
	ctx := context.Background()

	require.NoError(t, ovn.CreateLogicalSwitch(ctx, &nbdb.LogicalSwitch{Name: "subnet-c"}))
	subs, err := topo.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	sgEvt := SGEvent{GroupId: "sg-converged000000", VpcId: "vpc-x"}
	sgData, _ := json.Marshal(sgEvt)
	resp, err := nc.Request(TopicCreateSG, sgData, 2*time.Second)
	require.NoError(t, err)
	assertSuccess(t, resp, "create SG")

	// LSP already joined the port group — matches what handleCreateLSP /
	// handleUpdatePortSGs leave behind on the happy path. The per-port-group
	// `_ip4` Address_Set is auto-derived by ovn-northd in production.
	pgName := portGroupName("sg-converged000000")
	require.NoError(t, ovn.CreateLogicalSwitchPortInGroups(ctx, "subnet-c",
		&nbdb.LogicalSwitchPort{Name: "port-eni-converged", Addresses: []string{"02:00:00:aa:bb:02 10.0.0.6"}},
		[]string{pgName}))

	seedSGBucket(t, nc, []handlers_ec2_vpc.SecurityGroupRecord{{
		GroupId: "sg-converged000000", VpcId: "vpc-x", CreatedAt: time.Now(),
	}})
	seedENIBucket(t, nc, []handlers_ec2_vpc.ENIRecord{{
		NetworkInterfaceId: "eni-converged",
		PrivateIpAddress:   "10.0.0.6",
		MacAddress:         "02:00:00:aa:bb:02",
		SecurityGroupIds:   []string{"sg-converged000000"},
		CreatedAt:          time.Now(),
	}})

	result := ReconcileSGsOnce(ctx, nc, topo)

	assert.Equal(t, 0, result.PortMembershipsSynced, "fully converged ENI must not count as synced")
	assert.Equal(t, 0, result.PortGroupsRecreated, "existing PG must not be recreated")
	assert.Equal(t, 0, result.OrphanPortGroupsRemoved, "PG with matching SG record must not be torn down")
}

// TestReconcileSGsOnce_DeletesOrphanPortGroup: an `sg_*` port group with no
// matching SG record must be torn down (PG + ACLs) within one pass. Section
// G2 of the manual test plan. The matching `<pg>_ip4` Address_Set is
// auto-derived in SB by ovn-northd and goes away with the port group.
func TestReconcileSGsOnce_DeletesOrphanPortGroup(t *testing.T) {
	_, nc := startTestJetStreamNATS(t)
	ovn := NewMockOVNClient()
	require.NoError(t, ovn.Connect(context.Background()))
	topo := NewTopologyHandler(ovn)
	ctx := context.Background()

	const orphanPG = "sg_orphan0000000000"
	require.NoError(t, ovn.CreatePortGroup(ctx, orphanPG, nil))

	// Pre-existing ACL on the orphan PG to confirm the scan clears it before
	// deleting (matches handleDeleteSG's order).
	require.NoError(t, ovn.AddACLs(ctx, orphanPG, []ACLSpec{{
		Direction: "to-lport", Priority: 900,
		Match: "outport == @" + orphanPG + " && ip4", Action: "drop",
	}}))

	seedSGBucket(t, nc, nil)
	seedENIBucket(t, nc, nil)

	result := ReconcileSGsOnce(ctx, nc, topo)

	assert.Equal(t, 1, result.OrphanPortGroupsRemoved)
	_, err := ovn.GetPortGroup(ctx, orphanPG)
	assert.Error(t, err, "orphan port group must be gone")
	pgs, err := ovn.ListPortGroups(ctx)
	require.NoError(t, err)
	for _, pg := range pgs {
		assert.NotEqual(t, orphanPG, pg.Name)
	}
}

// TestReconcileSGsOnce_LeavesNonSpinifexPortGroupsAlone: only `sg_*` port
// groups are spinifex-managed. A PG named differently (e.g. one a third
// party put in OVN) must not be touched.
func TestReconcileSGsOnce_LeavesNonSpinifexPortGroupsAlone(t *testing.T) {
	_, nc := startTestJetStreamNATS(t)
	ovn := NewMockOVNClient()
	require.NoError(t, ovn.Connect(context.Background()))
	topo := NewTopologyHandler(ovn)
	ctx := context.Background()

	require.NoError(t, ovn.CreatePortGroup(ctx, "third-party-pg", nil))

	seedSGBucket(t, nc, nil)
	seedENIBucket(t, nc, nil)

	result := ReconcileSGsOnce(ctx, nc, topo)
	assert.Equal(t, 0, result.OrphanPortGroupsRemoved)
	_, err := ovn.GetPortGroup(ctx, "third-party-pg")
	assert.NoError(t, err, "non-`sg_*` port group must be left untouched")
}

// TestReconcileSGsOnce_ENIWithoutLSPLogs: an ENI's KV record references SGs
// but no OVN LSP exists yet. The scan should not count this ENI as synced.
func TestReconcileSGsOnce_ENIWithoutLSPLogs(t *testing.T) {
	_, nc := startTestJetStreamNATS(t)
	ovn := NewMockOVNClient()
	require.NoError(t, ovn.Connect(context.Background()))
	topo := NewTopologyHandler(ovn)

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

// TestReconcileSGsOnce_NilTopologyHandler: defensive guard — a partial vpcd
// startup that fails before TopologyHandler.ovn is wired must not panic when
// the leader-elected reconciler tick fires.
func TestReconcileSGsOnce_NilTopologyHandler(t *testing.T) {
	_, nc := startTestJetStreamNATS(t)

	assert.Equal(t, SGReconcileResult{}, ReconcileSGsOnce(context.Background(), nc, nil),
		"nil topo must return zero result, not panic")
	assert.Equal(t, SGReconcileResult{}, ReconcileSGsOnce(context.Background(), nc, NewTopologyHandler(nil)),
		"topo with nil ovn must return zero result, not panic")
}

// TestReconcileSGsOnce_SkipsMalformedSGEntry: a corrupt SG record (non-JSON
// bytes) must be logged-and-skipped, not crash the reconciler. Otherwise a
// single bad write to the SG bucket would freeze convergence cluster-wide.
func TestReconcileSGsOnce_SkipsMalformedSGEntry(t *testing.T) {
	_, nc := startTestJetStreamNATS(t)
	ovn := NewMockOVNClient()
	require.NoError(t, ovn.Connect(context.Background()))
	topo := NewTopologyHandler(ovn)

	// Seed with one valid + one malformed entry. The malformed entry must not
	// abort the scan — the valid SG must still get its port group created.
	js, err := nc.JetStream()
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: handlers_ec2_vpc.KVBucketSecurityGroups, History: 1})
	require.NoError(t, err)
	_, err = kv.Put(utils.AccountKey(sgReconcileAccountID, "sg-malformed00000"), []byte("not valid json"))
	require.NoError(t, err)
	good, _ := json.Marshal(handlers_ec2_vpc.SecurityGroupRecord{
		GroupId: "sg-good0000000000", VpcId: "vpc-x", CreatedAt: time.Now(),
	})
	_, err = kv.Put(utils.AccountKey(sgReconcileAccountID, "sg-good0000000000"), good)
	require.NoError(t, err)

	result := ReconcileSGsOnce(context.Background(), nc, topo)
	assert.Equal(t, 1, result.PortGroupsRecreated, "valid SG must still be processed")
	_, err = ovn.GetPortGroup(context.Background(), portGroupName("sg-good0000000000"))
	assert.NoError(t, err)
	_, err = ovn.GetPortGroup(context.Background(), portGroupName("sg-malformed00000"))
	assert.Error(t, err, "malformed entry must not produce a port group")
}

// TestReconcileSGsLoop_ExitsOnContextCancel: the periodic loop must stop when
// its context is cancelled. Otherwise vpcd shutdown leaks a goroutine that
// keeps holding the reconcile-leader lock until TTL expiry — blocking other
// vpcds from converging during the shutdown window.
func TestReconcileSGsLoop_ExitsOnContextCancel(t *testing.T) {
	_, nc := startTestJetStreamNATS(t)
	ovn := NewMockOVNClient()
	require.NoError(t, ovn.Connect(context.Background()))
	topo := NewTopologyHandler(ovn)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		ReconcileSGsLoop(ctx, nc, topo)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ReconcileSGsLoop did not return within 2s of ctx cancel")
	}
}
