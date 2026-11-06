package cluster

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/tags"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/nats-io/nats.go/jetstream"
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

// ADR-0003:S7. A running control-plane VM whose cluster record cannot be found
// is retained and alarmed, never terminated and never has its NLB or VPC torn
// down: a missing record cannot tell a completed DeleteCluster from a lost
// bucket or an out-of-band delete.
func TestS7_EKSBillableReaperNeverTerminatesAControlPlane(t *testing.T) {
	const lostAccount = "999999999999"

	cases := []struct {
		name      string
		account   string
		prepare   func(t *testing.T, f *eksServiceFixture)
		wantAlarm string
		wantLog   string
	}{
		{
			name:      "account bucket missing",
			account:   lostAccount,
			prepare:   func(*testing.T, *eksServiceFixture) {},
			wantAlarm: "account bucket missing",
		},
		{
			name:      "bucket has no meta key",
			account:   testAccountID,
			prepare:   func(*testing.T, *eksServiceFixture) {},
			wantAlarm: "cluster record missing",
		},
		{
			name:    "meta removed by the teardown revision-checked delete",
			account: testAccountID,
			prepare: func(t *testing.T, f *eksServiceFixture) {
				require.NoError(t, PutClusterMeta(t.Context(), f.kv, sampleClusterMeta("orphan")))
				_, err := kvutil.DeleteIf(t.Context(), f.kv, ClusterMetaKey("orphan"), kvutil.CASConfig{
					Attempts: maxClusterStateCASRetries,
					NotFound: ErrClusterNotFound,
				}, func(*ClusterMeta) error { return nil })
				require.NoError(t, err)
			},
			wantAlarm: "cluster record missing",
		},
		{
			name:    "meta removed out of band",
			account: testAccountID,
			prepare: func(t *testing.T, f *eksServiceFixture) {
				require.NoError(t, PutClusterMeta(t.Context(), f.kv, sampleClusterMeta("orphan")))
				require.NoError(t, f.kv.Delete(t.Context(), ClusterMetaKey("orphan")))
			},
			wantAlarm: "cluster record missing",
		},
		{
			name:    "meta unreadable",
			account: testAccountID,
			prepare: func(t *testing.T, f *eksServiceFixture) {
				_, err := f.kv.Put(t.Context(), ClusterMetaKey("orphan"), []byte("{not json"))
				require.NoError(t, err)
			},
			wantLog: "cluster record read failed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// slog.SetDefault is process-wide, so this test is not parallel.
			var buf bytes.Buffer
			restore := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
			t.Cleanup(func() { slog.SetDefault(restore) })

			f := newEKSServiceFixture(t)
			f.vpc.describeByENI = map[string]*ec2.NetworkInterface{
				"eni-cp": cpENI("eni-cp", "orphan", tc.account),
			}
			nlbName := ClusterNLBName("orphan")
			f.nlb.lbByName[nlbName] = &elbv2.LoadBalancer{
				LoadBalancerArn:  aws.String("arn:lb-orphan"),
				LoadBalancerName: aws.String(nlbName),
			}
			f.ngw.gws = []*fakeCPNatGateway{{
				id:    "nat-orphan",
				tags:  map[string]string{clusterEKSClusterTagKey: "orphan", clusterEKSRoleTagKey: cpVPCRoles.NatGW},
				state: "available",
				addrs: []*ec2.NatGatewayAddress{{AllocationId: aws.String("eipalloc-orphan")}},
			}}
			tc.prepare(t, f)

			reaper := f.svc.NewBillableReaper(func() ([]*vm.VM, error) { return []*vm.VM{cpVM("i-cp", "eni-cp")}, nil })
			for range 2 {
				removed, err := reaper.Sweep(context.Background())
				require.NoError(t, err)
				assert.Zero(t, removed, "ADR-0003:S7: the reaper removes nothing")
			}

			assert.Empty(t, f.inst.terminateCalls, "ADR-0003:S7: a reaper's judgement is never the decision to terminate")
			assert.Empty(t, f.vpc.deleteCalls, "the control-plane ENI must stay")
			assert.Contains(t, f.nlb.lbByName, nlbName, "the NLB must stay")
			assert.Empty(t, f.eip.releaseCalls, "no EIP may be released")

			js := testutil.NewJetStream(t, f.svc.deps.NATSConn)
			if tc.account == lostAccount {
				_, err := OpenAccountBucket(t.Context(), js, lostAccount)
				assert.ErrorIs(t, err, jetstream.ErrBucketNotFound, "a sweep must not create the bucket it found missing")
			}

			out := buf.String()
			if tc.wantAlarm != "" {
				assert.Contains(t, out, "DATA-SAFETY ALARM")
				assert.Contains(t, out, tc.wantAlarm)
				assert.Contains(t, out, "i-cp")
			} else {
				assert.NotContains(t, out, "DATA-SAFETY ALARM", "an unreadable record is not a missing one")
				assert.Contains(t, out, tc.wantLog)
			}
		})
	}
}

// TestRLC5_EKSBillableReaperSpareLiveAndUncertain enforces the reaper's no-false-
// reap guarantee: a CP VM whose cluster meta still exists (live / CREATING /
// DELETING) is left to the cluster's own teardown, and a VM whose ENI is gone or
// untagged is never reaped (orphan-hood cannot be confirmed).
func TestRLC5_EKSBillableReaperSpareLiveAndUncertain(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	require.NoError(t, PutClusterMeta(t.Context(), f.kv, sampleClusterMeta("alive")))

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
	assert.Equal(t, 0, reaped, "ADR-0003:S2 behaviour; historic PROP-LIFECYCLE-006 §5 mechanism: the reaper must never reap on uncertainty or a live cluster")
	assert.Empty(t, f.inst.terminateCalls, "no VM with a live/unknown cluster may be terminated")
	assert.Empty(t, f.eip.releaseCalls, "no reclaim (no EIP release) may run without a reap")
}
