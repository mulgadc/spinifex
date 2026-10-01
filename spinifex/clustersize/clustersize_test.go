package clustersize_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/clustersize"
)

func TestDeclareAndNodes(t *testing.T) {
	t.Cleanup(clustersize.ResetForTest)
	clustersize.ResetForTest()

	if got := clustersize.Nodes(); got != 0 {
		t.Fatalf("undeclared cluster size = %d, want 0", got)
	}
	if err := clustersize.Declare(3); err != nil {
		t.Fatalf("Declare(3): %v", err)
	}
	if got := clustersize.Nodes(); got != 3 {
		t.Fatalf("Declare(3) = %d, want 3", got)
	}
}

// TestDeclareIsWriteOnce covers the three ways a second declaration can arrive.
// Repeating the same count is how two call sites reading the same config behave
// and must be accepted; a different count is a process disagreeing with itself
// about how many nodes exist, which must not be resolved by whichever ran last.
func TestDeclareIsWriteOnce(t *testing.T) {
	t.Cleanup(clustersize.ResetForTest)
	clustersize.ResetForTest()

	if err := clustersize.Declare(3); err != nil {
		t.Fatalf("first Declare(3): %v", err)
	}
	if err := clustersize.Declare(3); err != nil {
		t.Fatalf("repeating Declare(3) must be accepted, got %v", err)
	}
	err := clustersize.Declare(5)
	if err == nil {
		t.Fatal("Declare(5) after Declare(3) returned no error")
	}
	if got := clustersize.Nodes(); got != 3 {
		t.Fatalf("a rejected redeclaration changed the count to %d, want 3 kept", got)
	}
	// The message has to name both counts, because the failure it reports is a
	// disagreement and neither number alone identifies it.
	for _, want := range []string{"3", "5"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name the count %s", err, want)
		}
	}
}

// TestDeclareRejectsANegativeCount: a negative node count is not a smaller
// cluster, it is a broken config, and storing it as-is would read back as a
// count rather than as undeclared.
func TestDeclareRejectsANegativeCount(t *testing.T) {
	t.Cleanup(clustersize.ResetForTest)
	clustersize.ResetForTest()

	if err := clustersize.Declare(-2); err != nil {
		t.Fatalf("Declare(-2): %v", err)
	}
	if got := clustersize.Nodes(); got != 0 {
		t.Fatalf("Declare(-2) = %d, want 0 (undeclared)", got)
	}
	if _, err := clustersize.Replicas(); !errors.Is(err, clustersize.ErrUndeclared) {
		t.Fatalf("Replicas() after Declare(-2) = %v, want ErrUndeclared", err)
	}
}

// TestReplicas covers the policy and the fail-closed answer together: a bucket is
// replicated across the largest odd number of nodes the cluster can place, capped
// at five, and a process that never declared a size gets an error rather than a
// guess at one.
//
// The even counts are the point of the table. Four replicas cost a fourth full
// copy of every write and survive exactly one loss, the same as three, and two
// survive nothing at all because losing either member leaves no majority.
func TestReplicas(t *testing.T) {
	t.Cleanup(clustersize.ResetForTest)

	for _, tc := range []struct{ nodes, want int }{
		{1, 1}, {2, 1}, {3, 3}, {4, 3}, {5, 5}, {6, 5}, {10, 5}, {64, 5},
	} {
		clustersize.ResetForTest()
		if err := clustersize.Declare(tc.nodes); err != nil {
			t.Fatalf("Declare(%d): %v", tc.nodes, err)
		}
		got, err := clustersize.Replicas()
		if err != nil {
			t.Fatalf("Replicas() with %d nodes: %v", tc.nodes, err)
		}
		if got != tc.want {
			t.Errorf("Replicas() with %d nodes = %d, want %d", tc.nodes, got, tc.want)
		}
		if got%2 == 0 {
			t.Errorf("Replicas() with %d nodes = %d, which is even", tc.nodes, got)
		}
		if fromCount := clustersize.ReplicasFor(tc.nodes); fromCount != got {
			t.Errorf("ReplicasFor(%d) = %d but Replicas() = %d; the two must be one rule",
				tc.nodes, fromCount, got)
		}
	}

	clustersize.ResetForTest()
	if _, err := clustersize.Replicas(); !errors.Is(err, clustersize.ErrUndeclared) {
		t.Fatalf("Replicas() with an undeclared cluster size = %v, want ErrUndeclared", err)
	}
}

// TestReplicasForNeverReturnsZero: callers that hold a node count use it without
// an error return, so a count below one has to resolve to the single replica a
// lone server can hold rather than to none.
func TestReplicasForNeverReturnsZero(t *testing.T) {
	for _, nodes := range []int{-1, 0} {
		if got := clustersize.ReplicasFor(nodes); got != 1 {
			t.Errorf("ReplicasFor(%d) = %d, want 1", nodes, got)
		}
	}
}

// TestPermanent separates this package's own refusal from a cluster that has yet
// to form. The retry loops waiting for JetStream quorum stop on the first and
// keep waiting through the second, so confusing them spends a whole startup
// budget on a misconfiguration.
func TestPermanent(t *testing.T) {
	if !clustersize.Permanent(clustersize.ErrUndeclared) {
		t.Error("Permanent(ErrUndeclared) = false")
	}
	wrapped := errors.Join(errors.New("create KV bucket x"), clustersize.ErrUndeclared)
	if !clustersize.Permanent(wrapped) {
		t.Error("Permanent() did not see through a wrapped ErrUndeclared")
	}
	if clustersize.Permanent(errors.New("no suitable peers for placement")) {
		t.Error("Permanent() treated a placement failure as permanent")
	}
	if clustersize.Permanent(nil) {
		t.Error("Permanent(nil) = true")
	}
}
