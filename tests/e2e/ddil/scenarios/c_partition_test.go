//go:build e2e

package scenarios

import "testing"

// TestScenarioC_CleanPartition — iptables-DROP node3 away from node1 and
// node2, verify the majority keeps serving API, the isolated node
// reports standalone mode, and heal converges state without duplicate
// or orphaned VMs. See
// docs/development/improvements/ddil-e2e-test-harness.md §3 Scenario C.
//
// Quarantined: witness VM block I/O is backed by distributed predastore,
// so the partition that severs node3 ↔ peer traffic also severs the
// witness disk writes (BLOCK_IO_ERROR → daemon terminates the instance
// → AssertProgressed reads a dead sshd → handshake EOF). Needs
// predastore-ddil-hardening §2 (write-quorum with deferred parity
// repair) so writes succeed at data-shard quorum and the witness disk
// survives the split.
func TestScenarioC_CleanPartition(t *testing.T) {
	scenarioSkip(t, "C", "predastore-ddil-hardening §2")
}
