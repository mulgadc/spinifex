//go:build e2e

package scenarios

import "testing"

// TestScenarioF_RaftUnderSATCOM — apply the SATCOM netem profile
// cluster-wide and verify Raft leader elections remain ≤1 over a
// 5-minute window (baselined against LAN, where elections thrash). Scenario F.
func TestScenarioF_RaftUnderSATCOM(t *testing.T) {
	scenarioSkip(t, "F", "predastore-ddil-hardening §1")
}
