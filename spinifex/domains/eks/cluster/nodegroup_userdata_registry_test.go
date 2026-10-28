package cluster

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"
)

type renderedRegistries struct {
	Mirrors map[string]struct {
		Endpoint []string `yaml:"endpoint"`
	} `yaml:"mirrors"`
	Configs map[string]struct {
		TLS struct {
			CAFile string `yaml:"ca_file"`
		} `yaml:"tls"`
	} `yaml:"configs"`
}

type renderedCredProvider struct {
	Providers []struct {
		Name        string   `yaml:"name"`
		MatchImages []string `yaml:"matchImages"`
	} `yaml:"providers"`
}

// renderWorkerFiles builds the userdata and returns the parsed registries.yaml and
// credential-provider config, so assertions see the structure containerd and the
// kubelet will, not substrings.
func renderWorkerFiles(t *testing.T, in agentUserDataInput) (renderedRegistries, renderedCredProvider) {
	t.Helper()
	var cloud struct {
		WriteFiles []struct {
			Path    string `yaml:"path"`
			Content string `yaml:"content"`
		} `yaml:"write_files"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(buildAgentUserData(in)), &cloud))

	var reg renderedRegistries
	var cred renderedCredProvider
	var sawReg, sawCred bool
	for _, f := range cloud.WriteFiles {
		switch f.Path {
		case "/etc/rancher/k3s/registries.yaml":
			require.NoError(t, yaml.Unmarshal([]byte(f.Content), &reg))
			sawReg = true
		case "/etc/spinifex-eks/credential-provider-config.yaml":
			require.NoError(t, yaml.Unmarshal([]byte(f.Content), &cred))
			sawCred = true
		}
	}
	require.True(t, sawReg && sawCred, "registries.yaml and credential-provider config must be rendered")
	return reg, cred
}

func registryTestInput() agentUserDataInput {
	in := baseTestInput()
	in.GatewayURL = "https://192.168.1.228:9999"
	in.GatewayCACert = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----"
	return in
}

const parity = "123456789012.dkr.ecr.ap-southeast-2.spinifex.internal"

func TestBuildAgentUserData_RegistryHostsFallbackOrder(t *testing.T) {
	t.Parallel()
	in := registryTestInput()
	in.GatewayIP = "192.168.1.228"
	in.RegistryHosts = []string{"192.168.1.228:9999", "192.168.1.229:9999", "192.168.1.230:9999", "192.168.1.231:9999"}

	reg, cred := renderWorkerFiles(t, in)

	all := []string{
		"https://192.168.1.228:9999", "https://192.168.1.229:9999",
		"https://192.168.1.230:9999", "https://192.168.1.231:9999",
	}
	// The issue's case: a repositoryUri on .231 is keyed, dials .231 first, then the rest.
	assert.Equal(t, []string{
		"https://192.168.1.231:9999", "https://192.168.1.228:9999",
		"https://192.168.1.229:9999", "https://192.168.1.230:9999",
	}, reg.Mirrors["192.168.1.231:9999"].Endpoint)
	assert.Equal(t, all, reg.Mirrors[parity].Endpoint)
	assert.Equal(t, all, reg.Mirrors[parity+":9999"].Endpoint)

	for _, gw := range []string{"192.168.1.228:9999", "192.168.1.229:9999", "192.168.1.230:9999", "192.168.1.231:9999"} {
		assert.Equal(t, "https://"+gw, reg.Mirrors[gw].Endpoint[0], "mirror %s dials itself first", gw)
		assert.Equal(t, "/etc/spinifex-eks/gateway-ca.pem", reg.Configs[gw].TLS.CAFile, "CA for %s", gw)
	}
	assert.Len(t, reg.Mirrors, 6)
	assert.Len(t, reg.Configs, 4)

	require.Len(t, cred.Providers, 1)
	assert.ElementsMatch(t, []string{
		parity, parity + ":9999",
		"192.168.1.228:9999", "192.168.1.229:9999", "192.168.1.230:9999", "192.168.1.231:9999",
	}, cred.Providers[0].MatchImages)
}

func TestBuildAgentUserData_EmptyRegistryHostsKeepsSingleGatewayEndpoint(t *testing.T) {
	t.Parallel()
	in := registryTestInput()
	in.GatewayIP = "10.15.8.1"

	reg, cred := renderWorkerFiles(t, in)

	single := []string{"https://10.15.8.1:9999"}
	assert.Equal(t, single, reg.Mirrors[parity].Endpoint)
	assert.Equal(t, single, reg.Mirrors[parity+":9999"].Endpoint)
	assert.Equal(t, single, reg.Mirrors["10.15.8.1:9999"].Endpoint)
	assert.Len(t, reg.Mirrors, 3)
	require.Len(t, reg.Configs, 1)
	assert.Equal(t, "/etc/spinifex-eks/gateway-ca.pem", reg.Configs["10.15.8.1:9999"].TLS.CAFile)
	assert.Equal(t, []string{parity, parity + ":9999", "10.15.8.1:9999"}, cred.Providers[0].MatchImages)
}

func TestBuildAgentUserData_EmptyRegistryHostsAndNoGatewayIPUsesHostname(t *testing.T) {
	t.Parallel()
	reg, cred := renderWorkerFiles(t, registryTestInput())

	host := []string{"https://" + parity + ":9999"}
	assert.Equal(t, host, reg.Mirrors[parity].Endpoint)
	assert.Equal(t, host, reg.Mirrors[parity+":9999"].Endpoint)
	assert.Len(t, reg.Mirrors, 2)
	require.Len(t, reg.Configs, 1)
	assert.Contains(t, reg.Configs, parity+":9999")
	assert.Equal(t, []string{parity, parity + ":9999"}, cred.Providers[0].MatchImages)
}

func TestBuildAgentUserData_GatewayIPNotInRegistryHostsIsKept(t *testing.T) {
	t.Parallel()
	in := registryTestInput()
	in.GatewayIP = "10.15.8.1"
	in.RegistryHosts = []string{"192.168.1.229:9999", "192.168.1.230:9999"}

	reg, cred := renderWorkerFiles(t, in)

	assert.Equal(t, []string{
		"https://192.168.1.229:9999", "https://192.168.1.230:9999", "https://10.15.8.1:9999",
	}, reg.Mirrors[parity].Endpoint)
	assert.Equal(t, "https://10.15.8.1:9999", reg.Mirrors["10.15.8.1:9999"].Endpoint[0])
	assert.Contains(t, reg.Configs, "10.15.8.1:9999")
	assert.Contains(t, cred.Providers[0].MatchImages, "10.15.8.1:9999")
}

func TestBuildAgentUserData_RegistryHostWithoutPortIsDialedOn443(t *testing.T) {
	t.Parallel()
	in := registryTestInput()
	in.RegistryHosts = []string{"10.0.0.1", "10.0.0.2:9999"}

	reg, _ := renderWorkerFiles(t, in)

	assert.Equal(t, []string{"https://10.0.0.1", "https://10.0.0.2:9999"}, reg.Mirrors["10.0.0.1"].Endpoint)
	assert.Equal(t, "/etc/spinifex-eks/gateway-ca.pem", reg.Configs["10.0.0.1"].TLS.CAFile)
}
