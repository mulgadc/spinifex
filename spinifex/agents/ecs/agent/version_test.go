package agent

import (
	"path/filepath"
	"testing"
)

// The build version is injected into the binary's main package and handed in
// through Config; New must carry it into the identity the register message sends.
func TestNewReportsConfiguredAgentVersion(t *testing.T) {
	srv := metadataStub(t, true)
	a, err := New(Config{
		IMDSBase:         srv.URL + "/latest",
		GatewayURL:       "https://127.0.0.1:1",
		Region:           "us-east-1",
		ClusterName:      "default",
		ContainerdSocket: filepath.Join(t.TempDir(), "absent.sock"),
		AgentVersion:     "1.2.3-test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.id.AgentVersion != "1.2.3-test" {
		t.Errorf("identity AgentVersion = %q, want the configured build version", a.id.AgentVersion)
	}
}
