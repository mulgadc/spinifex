package handlers_eks

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const reaperSysAccount = "000000000000"

// cpENI builds a control-plane ENI record carrying the cluster + account tags
// the billable reaper reads to map a running VM back to its cluster meta.
func cpENI(eniID, cluster, account string) *ec2.NetworkInterface {
	return &ec2.NetworkInterface{
		NetworkInterfaceId: aws.String(eniID),
		TagSet: []*ec2.Tag{
			{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByEKS)},
			{Key: aws.String(clusterEKSClusterTagKey), Value: aws.String(cluster)},
			{Key: aws.String(clusterEKSAccountTagKey), Value: aws.String(account)},
			{Key: aws.String(clusterEKSRoleTagKey), Value: aws.String(clusterEKSRoleControlPlane)},
		},
	}
}

func cpVM(id, eniID string) *vm.VM {
	return &vm.VM{ID: id, ManagedBy: tags.ManagedByEKS, ENIId: eniID, AccountID: reaperSysAccount, LastNode: "node-1"}
}

// TestRLC5_EKSBillableReaperTerminatesOrphanCPVM enforces ADR-0006 §5
// meta-independent billable cleanup: a running EKS control-plane VM whose
// cluster meta is DEFINITIVELY GONE is a billable orphan and must be terminated
// by the GC backstop — the real fix for mulga-siv-294 (orphan CP VM surviving a
// daemon restart after DeleteCluster swept the meta).
func TestRLC5_EKSBillableReaperTerminatesOrphanCPVM(t *testing.T) {
	f := newEKSServiceFixture(t)
	f.vpc.describeByENI = map[string]*ec2.NetworkInterface{
		"eni-orphan": cpENI("eni-orphan", "gone-cluster", testAccountID),
	}
	orphan := cpVM("i-orphan", "eni-orphan")

	reaper := f.svc.NewBillableReaper(func() ([]*vm.VM, error) { return []*vm.VM{orphan}, nil })

	reaped, err := reaper.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, reaped, "ADR-0006 §5: a CP VM whose cluster meta is gone must be reaped")
	assert.Contains(t, f.inst.terminateCalls, "i-orphan", "the orphan CP VM must be terminated")
	require.Len(t, f.vpc.deleteCalls, 1, "the orphan CP ENI must be deleted")
	assert.Equal(t, "eni-orphan", aws.StringValue(f.vpc.deleteCalls[0].NetworkInterfaceId))

	// Idempotent: a repeat sweep over the same (still meta-absent) VM is safe —
	// the terminate path tolerates an already-gone instance/ENI.
	_, err = reaper.Sweep(context.Background())
	require.NoError(t, err)
}

// TestRLC5_EKSBillableReaperSpareLiveAndUncertain enforces the reaper's no-false-
// reap guarantee: a CP VM whose cluster meta still exists (live / CREATING /
// DELETING) is left to the cluster's own teardown, and a VM whose ENI is gone or
// untagged is never reaped (orphan-hood cannot be confirmed).
func TestRLC5_EKSBillableReaperSpareLiveAndUncertain(t *testing.T) {
	f := newEKSServiceFixture(t)
	require.NoError(t, PutClusterMeta(f.kv, sampleClusterMeta("alive")))

	f.vpc.describeByENI = map[string]*ec2.NetworkInterface{
		"eni-live": cpENI("eni-live", "alive", testAccountID),
		// eni-untagged present but missing the cluster/account tags.
		"eni-untagged": {NetworkInterfaceId: aws.String("eni-untagged")},
	}

	live := cpVM("i-live", "eni-live")                    // meta present → spare
	untagged := cpVM("i-untagged", "eni-untagged")        // tags missing → spare
	gone := cpVM("i-gone", "eni-missing")                 // ENI absent from describe → spare
	notEKS := &vm.VM{ID: "i-customer", ENIId: "eni-cust"} // not EKS-managed → ignored

	reaper := f.svc.NewBillableReaper(func() ([]*vm.VM, error) {
		return []*vm.VM{live, untagged, gone, notEKS}, nil
	})

	reaped, err := reaper.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, reaped, "ADR-0006 §5: the reaper must never reap on uncertainty or a live cluster")
	assert.Empty(t, f.inst.terminateCalls, "no VM with a live/unknown cluster may be terminated")
}
