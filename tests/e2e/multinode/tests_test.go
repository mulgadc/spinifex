//go:build e2e

package multinode

import "testing"

// Top-level Test* wrappers — one per bash phase. Each delegates to a
// phaseN_X function in the matching <phase>_test.go file. Names follow
// the single-node convention (TestX, no numeric prefix) so isolated
// runs via `go test -run TestMultinodeClusterHealth` are stable.

// TestMultinodePreflight maps to phase 1 of run-multinode-e2e.sh.
func TestMultinodePreflight(t *testing.T) {
	phase1_Preflight(t, requireMultiNodeFixture(t))
}

// TestMultinodeClusterHealth maps to phase 2.
func TestMultinodeClusterHealth(t *testing.T) {
	phase2_ClusterHealth(t, requireMultiNodeFixture(t))
}
