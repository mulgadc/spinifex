//go:build e2e

package scenarios

import "testing"

// TestScenarioE_PredastoreWriteUnderPartition — partition node3, put a
// 10MB S3 object from the majority side, verify PUT succeeds with the
// missing-shard repair journal capturing node3's parity, then heal and
// verify journal drain + cross-node reads. Scenario E.
func TestScenarioE_PredastoreWriteUnderPartition(t *testing.T) {
	scenarioSkip(t, "E", "predastore-ddil-hardening §2")
}
