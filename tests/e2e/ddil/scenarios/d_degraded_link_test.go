//go:build e2e

package scenarios

import "testing"

// TestScenarioD_DegradedLink — apply the SATCOM netem profile to every
// node, verify fan-out queries return complete results and Raft does not
// enter continuous leader election. Scenario D.
func TestScenarioD_DegradedLink(t *testing.T) {
	scenarioSkip(t, "D", "ddil-concept-demonstrator §1.2 + predastore-ddil-hardening §1")
}
