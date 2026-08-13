package handlers_rds

import (
	"fmt"
	"strings"
)

// Where the AMI's rds-agent looks for its settings and the gateway CA it pins
// TLS against. Both paths are baked into the image, so they are protocol.
const (
	agentEnvPath       = "/etc/spinifex-rds/agent.env"
	agentGatewayCAPath = "/etc/spinifex-rds/gateway-ca.pem"
)

// No credentials: the agent draws auto-rotating instance-role credentials from
// IMDS, so nothing secret is written to a guest-readable data source. The master
// password arrives only over GetDBBootstrapConfig.
type agentUserDataInput struct {
	GatewayURL           string
	GatewayCACert        string
	Region               string
	DBInstanceIdentifier string
	// The engine this VM is launched as. The agent checks it against the engine
	// its own image bakes and refuses to bootstrap when the two disagree.
	Engine        string
	EngineVersion string
	EnginePort    int64
}

type userDataFile struct {
	path  string
	perms string
	body  string
}

func buildAgentUserData(in agentUserDataInput) string {
	agentBody := strings.Join([]string{
		"RDS_GATEWAY_URL=" + in.GatewayURL,
		"RDS_GATEWAY_CA=" + agentGatewayCAPath,
		"RDS_REGION=" + in.Region,
		"RDS_DB_INSTANCE_IDENTIFIER=" + in.DBInstanceIdentifier,
		"RDS_ENGINE=" + in.Engine,
		"RDS_ENGINE_VERSION=" + in.EngineVersion,
		fmt.Sprintf("RDS_ENGINE_PORT=%d", in.EnginePort),
	}, "\n")

	files := []userDataFile{{path: agentEnvPath, perms: "0600", body: agentBody}}
	// A deployment with no gateway CA leaves the file out rather than writing an
	// empty one, which the agent would load as an empty trust store.
	if ca := strings.TrimRight(in.GatewayCACert, "\n"); ca != "" {
		files = append(files, userDataFile{path: agentGatewayCAPath, perms: "0644", body: ca})
	}

	var buf strings.Builder
	buf.WriteString("#cloud-config\n")
	buf.WriteString("write_files:\n")
	for _, f := range files {
		fmt.Fprintf(&buf, "  - path: %s\n", f.path)
		fmt.Fprintf(&buf, "    permissions: '%s'\n", f.perms)
		buf.WriteString("    content: |\n")
		for line := range strings.SplitSeq(f.body, "\n") {
			buf.WriteString("      ")
			buf.WriteString(line)
			buf.WriteString("\n")
		}
	}
	return buf.String()
}
