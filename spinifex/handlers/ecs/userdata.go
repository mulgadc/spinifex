package handlers_ecs

import (
	"fmt"
	"strings"
)

// ecsGatewayCAPath is the on-VM gateway CA PEM path the agent verifies TLS against.
const ecsGatewayCAPath = "/etc/spinifex-ecs/gateway-ca.pem"

// containerInstanceUserDataInput carries the control-plane endpoint a container
// instance needs to register. No credentials: the agent draws auto-rotating
// instance-role creds from IMDS, matching AWS ecsInstanceRole.
type containerInstanceUserDataInput struct {
	GatewayURL    string
	GatewayCACert string
	Region        string
	ClusterName   string
}

// containerInstanceUserDataFile is one cloud-config write_files entry.
type containerInstanceUserDataFile struct {
	Path  string
	Perms string
	Body  string
}

// buildContainerInstanceUserData renders the #cloud-config for an ECS
// container-instance VM. It seeds only the agent's control-plane config + the
// gateway TLS CA; credentials come from IMDS, so no keys are written.
func buildContainerInstanceUserData(in containerInstanceUserDataInput) string {
	agentBody := strings.Join([]string{
		"ECS_GATEWAY_URL=" + in.GatewayURL,
		"ECS_GATEWAY_CA=" + ecsGatewayCAPath,
		"ECS_REGION=" + in.Region,
		"ECS_CLUSTER=" + in.ClusterName,
	}, "\n")

	files := []containerInstanceUserDataFile{
		{Path: "/etc/spinifex-ecs/agent.env", Perms: "0600", Body: agentBody},
		{Path: ecsGatewayCAPath, Perms: "0644", Body: strings.TrimRight(in.GatewayCACert, "\n")},
	}

	var buf strings.Builder
	buf.WriteString("#cloud-config\n")
	buf.WriteString("write_files:\n")
	for _, f := range files {
		fmt.Fprintf(&buf, "  - path: %s\n", f.Path)
		fmt.Fprintf(&buf, "    permissions: '%s'\n", f.Perms)
		buf.WriteString("    content: |\n")
		for line := range strings.SplitSeq(f.Body, "\n") {
			buf.WriteString("      ")
			buf.WriteString(line)
			buf.WriteString("\n")
		}
	}
	return buf.String()
}
