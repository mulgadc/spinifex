package agent

import (
	"errors"
	"path/filepath"
	"testing"

	ctrruntime "github.com/mulgadc/spinifex/spinifex/agents/ecs/runtime"
)

// The build version is injected into the binary's main package and handed in
// through Config; New must carry it into the identity the register message sends.
func TestNewReportsConfiguredAgentVersion(t *testing.T) {
	orig := dialContainerd
	t.Cleanup(func() { dialContainerd = orig })
	var dialed []string
	dialContainerd = func(socket string) (ctrruntime.Runtime, error) {
		dialed = append(dialed, socket)
		return nil, errors.New("containerd absent")
	}

	socket := filepath.Join(t.TempDir(), "absent.sock")
	srv := metadataStub(t, true)
	a, err := New(Config{
		IMDSBase:         srv.URL + "/latest",
		GatewayURL:       "https://127.0.0.1:1",
		Region:           "us-east-1",
		ClusterName:      "default",
		ContainerdSocket: socket,
		AgentVersion:     "1.2.3-test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.id.AgentVersion != "1.2.3-test" {
		t.Errorf("identity AgentVersion = %q, want the configured build version", a.id.AgentVersion)
	}
	if _, err := a.dialRuntime(); err == nil {
		t.Error("dialRuntime succeeded against an absent containerd")
	}
	if len(dialed) != 2 || dialed[0] != socket || dialed[1] != socket {
		t.Errorf("containerd dials = %q, want the configured socket at boot and on redial", dialed)
	}
}
