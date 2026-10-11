package ecsv1_test

import (
	"encoding/json"
	"testing"

	ecsv1 "github.com/mulgadc/spinifex/contracts/ecs/v1"
)

// oldAssignContainer models an ecs-agent built before the container-runtime
// fields were added: the wire shape AssignContainer had before user,
// readonlyRootFilesystem, privileged and the rest. The AMI-baked decode side.
type oldAssignContainer struct {
	Name         string              `json:"name"`
	Image        string              `json:"image"`
	CPU          int                 `json:"cpu,omitempty"`
	MemoryMiB    int                 `json:"memoryMiB,omitempty"`
	GPU          int                 `json:"gpu,omitempty"`
	Essential    bool                `json:"essential"`
	Command      []string            `json:"command,omitempty"`
	Environment  map[string]string   `json:"environment,omitempty"`
	PortMappings []ecsv1.PortMapping `json:"portMappings,omitempty"`
	LogDriver    string              `json:"logDriver,omitempty"`
}

// An agent baked into the ECS AMI must survive a daemon upgrade that adds
// fields: json.Unmarshal ignores unknown fields by default and PollAssignments
// never sets DisallowUnknownFields.
func TestAssignContainer_OlderAgentIgnoresUnknownFields(t *testing.T) {
	ro := true
	priv := true
	start := int64(30)
	current := ecsv1.AssignContainer{
		Name: "app", Image: "registry/app:1", Essential: true,
		User:                   "1000",
		ReadonlyRootFilesystem: &ro,
		Privileged:             &priv,
		SystemControls:         []ecsv1.SystemControl{{Namespace: "net.core.somaxconn", Value: "1024"}},
		CapAdd:                 []string{"SYS_PTRACE"},
		StartTimeout:           &start,
	}
	body, err := json.Marshal(current)
	if err != nil {
		t.Fatalf("marshal current AssignContainer: %v", err)
	}

	var old oldAssignContainer
	if err := json.Unmarshal(body, &old); err != nil {
		t.Fatalf("older agent failed to decode a newer AssignContainer: %v", err)
	}
	if old.Name != "app" || old.Image != "registry/app:1" || !old.Essential {
		t.Errorf("older agent lost known fields: %+v", old)
	}
}
