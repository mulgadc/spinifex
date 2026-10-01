// Package clustersize holds one state-tier policy fact and the one number derived from it: how
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
	"fmt"
	"sync/atomic"
	"testing"
)

// MaxReplicas is JetStream's own ceiling on stream replicas, so a cluster
// larger than this replicates to five nodes and no further. Asking for more is
// refused by the server, which is why the count is clamped rather than passed
// through.
const MaxReplicas = 5

// MinHANodes is the smallest cluster in which losing a node is survivable. Below
// it no layer of the stack has a majority to keep: OVN runs standalone, the
// storage metadata quorum has nothing to lose, and a KV bucket has no replica
// count that both survives a loss and accepts writes.
const MinHANodes = 3

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
//
// The first declaration wins. Repeating the same count is a no-op, because two
// call sites legitimately read it from the same config; a different count is an
// error rather than an overwrite, since a process that disagrees with itself
// about how many nodes exist would silently pick whichever ran last.
func Declare(n int) error {
	n = max(n, 0)
	if nodes.CompareAndSwap(0, int64(n)) {
		return nil
	}
	if have := Nodes(); have != n {
		return fmt.Errorf("cluster size was already declared as %d nodes and cannot be redeclared as %d", have, n)
	}
	return nil
}

// ResetForTest clears the declaration so a test can declare a different size.
// It panics outside a test binary: production has exactly one cluster size for
// the life of the process, and a reset available there would undo that.
func ResetForTest() {
	if !testing.Testing() {
		//nolint:forbidigo // Returning an error would let a caller ignore it and carry on with the count cleared.
		panic("clustersize.ResetForTest called outside a test binary")
	}
	nodes.Store(0)
}

// DeclareForTest declares n for the rest of the test binary, tolerating a fixture
// that already declared the same count.
//
// It is for fixtures rather than for tests. Standing up a one-node embedded server
// says something about the binary, not about one test, and several tests running
// in parallel share that one server — so restoring the previous count when any one
// of them finishes would leave its siblings undeclared mid-run.
func DeclareForTest(tb testing.TB, n int) {
	tb.Helper()
	if err := Declare(n); err != nil {
		tb.Fatalf("declare cluster size %d: %v", n, err)
	}
}

// RedeclareForTest declares n for the duration of one test, whatever was declared
// before it, and puts the previous count back afterwards.
//
// Declaring is write-once, so a test needing a different size than its fixture set
// up cannot simply call Declare again. It must not run in parallel with anything
// else that reads the count, because there is one count per process and this
// changes it.
func RedeclareForTest(tb testing.TB, n int) {
	tb.Helper()
	was := Nodes()
	ResetForTest()
	if err := Declare(n); err != nil {
		tb.Fatalf("declare cluster size %d: %v", n, err)
	}
	tb.Cleanup(func() {
		ResetForTest()
		if was > 0 {
			_ = Declare(was)
		}
	})
}

// Nodes returns the declared node count, or 0 when it was never declared.
func Nodes() int {
	return int(nodes.Load())
}

// Replicas is the replica count every stream is created with: the largest odd
// number of nodes this cluster can place a replica on, capped at JetStream's
// ceiling of five.
//
// Odd, because a majority is what a raft group needs and an even count buys
// none. Four replicas store a fourth full copy of every write and still survive
// only one loss, exactly like three; two survive nothing, because losing either
// member leaves no majority to accept a write. So four nodes replicate three
// ways and five replicate five ways, which is also the most any cluster does
// however large it grows.
//
// There is no argument and no override. A replica count is a property of the
// cluster, not of the stream or of the caller, and every stream here is read or
// written by a node other than the one that created it.
func Replicas() (int, error) {
	n := Nodes()
	if n < 1 {
		return 0, ErrUndeclared
	}
	return ReplicasFor(n), nil
}

// ReplicasFor is Replicas for a node count the caller already holds, so code that
// knows the cluster's size without having declared it — a test harness reading a
// cluster it did not build — gets its answer from the same rule rather than a
// second copy of it.
func ReplicasFor(nodes int) int {
	r := min(max(nodes, 1), MaxReplicas)
	if r%2 == 0 {
		r--
	}
	return r
}

// Permanent reports whether err is this package's own refusal rather than a
// transient cluster condition.
//
// The retry loops that wait for JetStream quorum on a cold start must stop on
// one: no amount of waiting declares a cluster size, so a loop that cannot tell
// the two apart spends its whole budget on a misconfiguration and then reports
// a quorum problem that was never the cause.
func Permanent(err error) bool {
	return errors.Is(err, ErrUndeclared)
}
