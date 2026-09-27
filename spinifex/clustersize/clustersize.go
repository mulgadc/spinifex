// Package clustersize holds one fact and the one number derived from it: how
// many nodes this process believes the cluster has, and therefore how many
// nodes each of its JetStream streams is replicated across.
//
// It is its own package because the fact is declared in one place,
// config.LoadConfig, and read in another, kvutil, and those sit either side of
// packages that would otherwise import each other.
//
// The number matters because a stream on one replica lives on one
// JetStream-chosen server recorded in no config. Losing that node makes the
// stream unreadable and unwritable from every node at once, so a single-node
// event becomes a cluster-wide outage.
package clustersize

import (
	"errors"
	"sync/atomic"
)

// MaxReplicas is JetStream's own ceiling on stream replicas, so a cluster
// larger than this replicates to five nodes and no further. Asking for more is
// refused by the server, which is why the count is clamped rather than passed
// through.
const MaxReplicas = 5

// ErrUndeclared is returned by Replicas in a process that never loaded a
// cluster config. It is an error rather than a fallback to one replica because
// one replica is the failure this package exists to prevent, and a fallback
// would reinstate it in exactly the processes nobody checked.
var ErrUndeclared = errors.New("cluster size was never declared, so the stream replica count is unknown")

// nodes is the declared node count. Zero means undeclared.
var nodes atomic.Int64

// Declare records the cluster's node count for this process. It is called from
// config.LoadConfig, so every process that can reach a cluster's NATS has
// already declared it before any handler creates a bucket.
func Declare(n int) {
	nodes.Store(int64(max(n, 0)))
}

// Nodes returns the declared node count, or 0 when it was never declared.
func Nodes() int {
	return int(nodes.Load())
}

// Replicas is the replica count every stream is created with: the cluster's
// node count, clamped to JetStream's ceiling.
//
// There is no argument and no override. A replica count is a property of the
// cluster, not of the stream or of the caller, and every stream here is read or
// written by a node other than the one that created it — so there is no stream
// for which fewer than the cluster's nodes is the right answer.
func Replicas() (int, error) {
	n := Nodes()
	if n < 1 {
		return 0, ErrUndeclared
	}
	return min(n, MaxReplicas), nil
}
