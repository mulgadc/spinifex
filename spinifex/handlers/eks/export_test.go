//test:in-package — an export_test.go is in-package by definition; that is the
//whole of what it does. The tests using these live in handlers_eks_test.

package handlers_eks

// The cluster client-token constructor and param hash, exported for the
// external test package. Both are unexported in production because a caller
// outside the package has no business minting the store or reproducing the
// hash, but the hash is exactly the piece that decides whether a retried
// CreateCluster replays or creates a second cluster.
var (
	NewClusterTokenStore  = newClusterTokenStore
	ClusterTokenParamHash = clusterTokenParamHash
)
