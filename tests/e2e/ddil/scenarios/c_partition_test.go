//go:build e2e

package scenarios

import "testing"

// TestScenarioC_CleanPartition partitions node3 from node1+node2 via iptables DROP, verifies
// the majority keeps serving and the isolated node enters standalone, then asserts heal
// converges without duplicate or orphaned VMs. Quarantined until predastore write-quorum lands
// (partition also severs witness disk I/O, causing BLOCK_IO_ERROR → instance termination).
func TestScenarioC_CleanPartition(t *testing.T) {
	scenarioSkip(t, "C", "predastore-ddil-hardening §2")
}
