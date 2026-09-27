package clustersize

import "testing"

func TestDeclareAndNodes(t *testing.T) {
	t.Cleanup(func() { nodes.Store(0) })

	if got := Nodes(); got != 0 {
		t.Fatalf("undeclared cluster size = %d, want 0", got)
	}
	Declare(3)
	if got := Nodes(); got != 3 {
		t.Fatalf("Declare(3) = %d, want 3", got)
	}
	// A negative node count is not a smaller cluster, it is a broken config, and
	// storing it as-is would read back as a count rather than as undeclared.
	Declare(-2)
	if got := Nodes(); got != 0 {
		t.Fatalf("Declare(-2) = %d, want 0", got)
	}
}
