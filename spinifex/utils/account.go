package utils

import (
	"sync/atomic"
)

// defaultKVReplicas is the replica count applied to buckets created via
// GetOrCreateKVBucket. The daemon sets it once at boot to the cluster size so
// lazily-created buckets are born quorate on multi-node; 0 means unset and is
// treated as 1 (single-node / tests).
var defaultKVReplicas atomic.Int64

// SetDefaultKVReplicas sets the replica count for buckets subsequently created
// via GetOrCreateKVBucket. Clamped to a minimum of 1. Called once at daemon
// boot with the cluster size, before any handler creates a bucket.
func SetDefaultKVReplicas(n int) {
	defaultKVReplicas.Store(int64(max(n, 1)))
}

// DefaultKVReplicas returns the current default replica count (minimum 1).
func DefaultKVReplicas() int {
	return max(int(defaultKVReplicas.Load()), 1)
}

// VersionKey is the well-known KV key used to store a bucket's schema version.
// The schema-version helpers that read and write it live in package kvutil,
// alongside the other context-aware KV helpers.
const VersionKey = "_version"

// GlobalAccountID is the root/system account ID.
const GlobalAccountID = "000000000000"

// AccountKey returns a KV key scoped to an account: "{accountID}.{resourceID}".
func AccountKey(accountID, resourceID string) string {
	return accountID + "." + resourceID
}

// IsAccountID checks if a string is a valid 12-digit AWS account ID.
func IsAccountID(s string) bool {
	if len(s) != 12 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
