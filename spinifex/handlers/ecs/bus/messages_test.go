package bus

import (
	"encoding/json"
	"testing"
)

// oldAssignContainer models an ecs-agent built before TFC-0040: the wire
// shape AssignContainer had before this fix added user, readonlyRootFilesystem,
// privileged, pseudoTerminal, interactive, systemControls, capAdd/capDrop,
// startTimeout and stopTimeout. It is the AMI-baked agent's decode side.
type oldAssignContainer struct {
	Name         string            `json:"name"`
	Image        string            `json:"image"`
	CPU          int               `json:"cpu,omitempty"`
	MemoryMiB    int               `json:"memoryMiB,omitempty"`
	GPU          int               `json:"gpu,omitempty"`
	Essential    bool              `json:"essential"`
	Command      []string          `json:"command,omitempty"`
	Environment  map[string]string `json:"environment,omitempty"`
	PortMappings []PortMapping     `json:"portMappings,omitempty"`
	LogDriver    string            `json:"logDriver,omitempty"`
}

// TestAssignContainer_OlderAgentIgnoresUnknownFields verifies the wire
// contract TFC-0040 depends on: an ecs-agent built before this change decodes
// a current AssignContainer without error, silently dropping the fields it
// does not know about, because json.Unmarshal ignores unknown fields by
// default (PollAssignments never sets DisallowUnknownFields). An agent baked
// into the ECS AMI must survive a daemon upgrade that adds fields.
func TestAssignContainer_OlderAgentIgnoresUnknownFields(t *testing.T) {
	ro := true
	priv := true
	start := int64(30)
	current := AssignContainer{
		Name: "app", Image: "registry/app:1", Essential: true,
		User:                   "1000",
		ReadonlyRootFilesystem: &ro,
		Privileged:             &priv,
		SystemControls:         []SystemControl{{Namespace: "net.core.somaxconn", Value: "1024"}},
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
