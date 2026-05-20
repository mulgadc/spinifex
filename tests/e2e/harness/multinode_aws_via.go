//go:build e2e

package harness

import "testing"

// AWSClientForGateway returns an AWSClient pointed at the named node's
// gateway endpoint instead of env.ServiceIPs[0]. Used by multinode phases
// 6 and 7 (cross-node gateway access + cross-node operations) where the
// test issues describe/stop/start against every node in turn.
//
// Bash equivalent: aws_via in run-multinode-e2e.sh — sets AWS_ENDPOINT_URL
// per call. Here we clone the *Env so the swap is scoped to one call site
// and concurrent callers can each address a different node safely.
func AWSClientForGateway(t *testing.T, env *Env, node Node) *AWSClient {
	t.Helper()
	if env == nil {
		t.Fatalf("AWSClientForGateway: nil env")
	}
	if node.Addr == "" {
		t.Fatalf("AWSClientForGateway: node %q has empty addr", node.Name)
	}
	scoped := *env
	scoped.ServiceIPs = []string{node.Addr}
	return NewAWSClient(t, &scoped)
}
