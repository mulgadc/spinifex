package kvutil

import "sync/atomic"

// defaultKVReplicas is the replica count applied to lazily-created KV buckets.
// The daemon sets it once at bootstrap from the configured cluster size; an
// unset value is one replica for single-node operation and tests.
var defaultKVReplicas atomic.Int64

// SetDefaultKVReplicas sets the replica count for buckets subsequently created
// through this package. Values below one are clamped to one.
func SetDefaultKVReplicas(n int) {
	defaultKVReplicas.Store(int64(max(n, 1)))
}

// DefaultKVReplicas returns the cluster default for newly created buckets.
func DefaultKVReplicas() int {
	return max(int(defaultKVReplicas.Load()), 1)
}

// VersionKey is the well-known KV key used to store a bucket's schema version.
const VersionKey = "_version"
