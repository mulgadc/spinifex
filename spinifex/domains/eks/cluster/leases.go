package cluster

import (
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvsync"
)

// KeyPrefixCluster is the prefix of every per-cluster lease in the sync bucket.
const KeyPrefixCluster = "eks.cluster."

// clusterLeaseTTL is every per-cluster lease's time to live.
const clusterLeaseTTL = 60 * time.Second

// NewClusterLeases returns the per-cluster lease group for holder. The EKS
// owner builds it once per process: a cluster's reconciler and teardown leases
// are members of it, and a deleted cluster's are forgotten.
func NewClusterLeases(store kvsync.KVStore, holder string) *kvsync.LeaseGroup {
	return kvsync.NewLeaseGroup(store, KeyPrefixCluster, holder, clusterLeaseTTL)
}

// clusterLeaseName names a cluster's reconciler lease in the group.
func clusterLeaseName(accountID, clusterName string) string {
	return accountID + "/" + clusterName
}

// teardownLeaseName names the per-cluster cross-node gate for a DELETING
// teardown. It is distinct from clusterLeaseName so a delete never contends
// with the still-live reconciler, which holds that lease until it reads DELETING.
func teardownLeaseName(accountID, clusterName string) string {
	return clusterLeaseName(accountID, clusterName) + "/teardown"
}
