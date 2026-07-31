package handlers_rds

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testAgentUserDataInput() agentUserDataInput {
	return agentUserDataInput{
		GatewayURL:           "https://172.30.0.1:9999",
		GatewayCACert:        "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n",
		Region:               testRegion,
		DBInstanceIdentifier: testDBID,
		EngineVersion:        "18",
		EnginePort:           5432,
	}
}

func TestBuildAgentUserData(t *testing.T) {
	out := buildAgentUserData(testAgentUserDataInput())

	require.True(t, strings.HasPrefix(out, "#cloud-config\n"), "cloud-init only parses a document with the header")
	assert.Contains(t, out, "  - path: "+agentEnvPath)
	assert.Contains(t, out, "  - path: "+agentGatewayCAPath)
	assert.Contains(t, out, "RDS_GATEWAY_URL=https://172.30.0.1:9999")
	assert.Contains(t, out, "RDS_GATEWAY_CA="+agentGatewayCAPath)
	assert.Contains(t, out, "RDS_REGION="+testRegion)
	assert.Contains(t, out, "RDS_DB_INSTANCE_IDENTIFIER="+testDBID)
	assert.Contains(t, out, "RDS_ENGINE_VERSION=18")
	assert.Contains(t, out, "RDS_ENGINE_PORT=5432")

	// Every content line is indented under its block, or cloud-init reads the
	// second line as a new key and drops the rest of the file.
	for line := range strings.SplitSeq(strings.TrimRight(out, "\n"), "\n") {
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "write_files:") ||
			strings.HasPrefix(line, "  - path:") || strings.HasPrefix(line, "    ") {
			continue
		}
		t.Errorf("unindented line in the cloud-config document: %q", line)
	}
}

// D8: the master password reaches the guest only over GetDBBootstrapConfig, and
// the agent's gateway credentials come from IMDS — so nothing secret is written
// to a data source any process on the VM can read.
func TestBuildAgentUserDataCarriesNoCredentials(t *testing.T) {
	in := testAgentUserDataInput()
	out := buildAgentUserData(in)

	for _, secret := range []string{"AKIA", "SecretAccessKey", "aws_secret", "MasterUserPassword", "password"} {
		assert.NotContains(t, strings.ToLower(out), strings.ToLower(secret))
	}
}

// An empty CA file would be loaded by the agent as an empty trust store, which
// fails the pin in a way that looks like a certificate problem rather than a
// missing configuration.
func TestBuildAgentUserDataOmitsAnAbsentCA(t *testing.T) {
	in := testAgentUserDataInput()
	in.GatewayCACert = ""

	out := buildAgentUserData(in)
	assert.NotContains(t, out, "  - path: "+agentGatewayCAPath)
	assert.Contains(t, out, "RDS_GATEWAY_CA="+agentGatewayCAPath,
		"the path stays configured so an image with a baked-in CA still finds it")
}
