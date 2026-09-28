package clustersize_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/clustersize"
)

func TestDeclareAndNodes(t *testing.T) {
	t.Cleanup(func() { clustersize.Declare(0) })

	if got := clustersize.Nodes(); got != 0 {
		t.Fatalf("undeclared cluster size = %d, want 0", got)
	}
	clustersize.Declare(3)
	if got := clustersize.Nodes(); got != 3 {
		t.Fatalf("Declare(3) = %d, want 3", got)
	}
	// A negative node count is not a smaller cluster, it is a broken config, and
	// storing it as-is would read back as a count rather than as undeclared.
	clustersize.Declare(-2)
	if got := clustersize.Nodes(); got != 0 {
		t.Fatalf("Declare(-2) = %d, want 0", got)
	}
}

// TestReplicas covers the clamp and the fail-closed answer together: a bucket is
// replicated across every node up to JetStream's ceiling of five, and a process
// that never declared a size gets an error rather than a guess at one.
func TestReplicas(t *testing.T) {
	t.Cleanup(func() { clustersize.Declare(0) })

	for _, tc := range []struct{ nodes, want int }{
		{1, 1}, {3, 3}, {4, 4}, {5, 5}, {10, 5}, {64, 5},
	} {
		clustersize.Declare(tc.nodes)
		got, err := clustersize.Replicas()
		if err != nil {
			t.Fatalf("Replicas() with %d nodes: %v", tc.nodes, err)
		}
		if got != tc.want {
			t.Errorf("Replicas() with %d nodes = %d, want %d", tc.nodes, got, tc.want)
		}
	}

	clustersize.Declare(0)
	if _, err := clustersize.Replicas(); err == nil {
		t.Fatal("Replicas() with an undeclared cluster size returned no error")
	}
}
