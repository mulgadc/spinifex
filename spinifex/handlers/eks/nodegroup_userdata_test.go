package handlers_eks

import (
	"strings"
	"testing"
)

func TestBuildAgentUserData_ECRWiring(t *testing.T) {
	in := agentUserDataInput{
		ClusterName:   "alpha",
		NodegroupName: "ng-1",
		ServerURL:     "https://10.0.0.1:443",
		JoinToken:     "jointoken",
		NodeName:      "alpha-ng-1-abcd1234",
		GatewayURL:    "https://10.15.8.1:9999",
		GatewayCACert: "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----",
		Region:        "ap-southeast-2",
		AccountID:     "123456789012",
		RegistryHost:  "123456789012.dkr.ecr.ap-southeast-2.mulga.internal",
		GatewayIP:     "10.15.8.1",
	}

	got := buildAgentUserData(in)

	wantContains := []string{
		// registries.yaml: pod ref stays the port-less host, mirror endpoint and
		// tls config key the gateway IP actually dialed.
		"/etc/rancher/k3s/registries.yaml",
		"\"123456789012.dkr.ecr.ap-southeast-2.mulga.internal\":",
		"\"https://10.15.8.1:9999\"",
		"\"10.15.8.1:9999\":",
		"ca_file: /etc/spinifex-eks/gateway-ca.pem",
		// config.yaml.d drop-in.
		"/etc/rancher/k3s/config.yaml.d/20-ecr-credential-provider.yaml",
		"image-credential-provider-config=/etc/spinifex-eks/credential-provider-config.yaml",
		"image-credential-provider-bin-dir=/usr/local/bin",
		// credential-provider config.
		"/etc/spinifex-eks/credential-provider-config.yaml",
		"name: ecr-credential-provider",
		// EKS env in agent.env.
		"EKS_GATEWAY_URL=https://10.15.8.1:9999",
		"EKS_ACCOUNT_ID=123456789012",
		"EKS_REGION=ap-southeast-2",
		"EKS_GATEWAY_CA=/etc/spinifex-eks/gateway-ca.pem",
		// gateway CA write_files.
		"/etc/spinifex-eks/gateway-ca.pem",
	}
	for _, want := range wantContains {
		if !strings.Contains(got, want) {
			t.Errorf("rendered userdata missing %q\n---\n%s", want, got)
		}
	}
	// No /etc/hosts injection: cloud-init manage_etc_hosts rewrites it each boot.
	if strings.Contains(got, "/etc/hosts") {
		t.Errorf("rendered userdata should not touch /etc/hosts\n%s", got)
	}
}

func TestBuildAgentUserData_NoGatewayIPUsesHostnameEndpoint(t *testing.T) {
	in := agentUserDataInput{
		ClusterName:   "alpha",
		NodegroupName: "ng-1",
		ServerURL:     "https://10.0.0.1:443",
		JoinToken:     "jointoken",
		NodeName:      "alpha-ng-1-abcd1234",
		Region:        "ap-southeast-2",
		AccountID:     "123456789012",
		RegistryHost:  "123456789012.dkr.ecr.ap-southeast-2.mulga.internal",
		GatewayIP:     "",
	}
	got := buildAgentUserData(in)
	// With no gateway IP, the endpoint falls back to the registry hostname (DNS
	// must resolve it) and the tls config keys that hostname:port.
	if !strings.Contains(got, "\"https://123456789012.dkr.ecr.ap-southeast-2.mulga.internal:9999\"") {
		t.Errorf("expected hostname endpoint when GatewayIP is empty\n%s", got)
	}
	if !strings.Contains(got, "\"123456789012.dkr.ecr.ap-southeast-2.mulga.internal:9999\":") {
		t.Errorf("expected hostname:port tls config key when GatewayIP is empty\n%s", got)
	}
}
