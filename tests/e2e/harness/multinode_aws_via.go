//go:build e2e

package harness

import "testing"

// AWSClientForGateway returns an AWSClient scoped to a single node's gateway
// endpoint. Clones the Env so concurrent callers can safely address different nodes.
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
