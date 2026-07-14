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
		RegistryHost:  "123456789012.dkr.ecr.ap-southeast-2.spinifex.internal",
		GatewayIP:     "10.15.8.1",
	}

	got := buildAgentUserData(in)

	wantContains := []string{
		// registries.yaml: every host a pod ref may carry (port-less parity, port-ful
		// parity, gateway IP:port) is a mirror keyed to the gateway IP actually dialed,
		// whose tls config keys the same host:port.
		"/etc/rancher/k3s/registries.yaml",
		"\"123456789012.dkr.ecr.ap-southeast-2.spinifex.internal\":",
		"\"123456789012.dkr.ecr.ap-southeast-2.spinifex.internal:9999\":",
		"\"https://10.15.8.1:9999\"",
		"\"10.15.8.1:9999\":",
		"ca_file: /etc/spinifex-eks/gateway-ca.pem",
		// config.yaml.d drop-in.
		"/etc/rancher/k3s/config.yaml.d/20-ecr-credential-provider.yaml",
		"image-credential-provider-config=/etc/spinifex-eks/credential-provider-config.yaml",
		"image-credential-provider-bin-dir=/usr/local/bin",
		// credential-provider config: matchImages covers the gateway IP:port too, so
		// a port-ful pull from the API-returned URI authenticates.
		"/etc/spinifex-eks/credential-provider-config.yaml",
		"name: ecr-credential-provider",
		"- \"10.15.8.1:9999\"",
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
		RegistryHost:  "123456789012.dkr.ecr.ap-southeast-2.spinifex.internal",
		GatewayIP:     "",
	}
	got := buildAgentUserData(in)
	// With no gateway IP, the endpoint falls back to the registry hostname (DNS
	// must resolve it) and the tls config keys that hostname:port.
	if !strings.Contains(got, "\"https://123456789012.dkr.ecr.ap-southeast-2.spinifex.internal:9999\"") {
		t.Errorf("expected hostname endpoint when GatewayIP is empty\n%s", got)
	}
	if !strings.Contains(got, "\"123456789012.dkr.ecr.ap-southeast-2.spinifex.internal:9999\":") {
		t.Errorf("expected hostname:port tls config key when GatewayIP is empty\n%s", got)
	}
}

func baseTestInput() agentUserDataInput {
	return agentUserDataInput{
		ClusterName:   "alpha",
		NodegroupName: "ng-1",
		ServerURL:     "https://10.0.0.1:443",
		JoinToken:     "jointoken",
		NodeName:      "alpha-ng-1-abcd1234",
		Region:        "ap-southeast-2",
		AccountID:     "123456789012",
		RegistryHost:  "123456789012.dkr.ecr.ap-southeast-2.spinifex.internal",
	}
}

func TestBuildAgentUserData_GPUEnabledSignalsNodegroupAndGPU(t *testing.T) {
	in := baseTestInput()
	in.GPUEnabled = true
	in.GPUVendor = "nvidia"

	got := buildAgentUserData(in)

	// buildAgentUserData writes NO node-label/node-taint config drop-in:
	// mulga-eks-provider-id.sh is the single node-label writer (k3s replaces,
	// not merges, node-label across config.yaml.d files). The worker signals its
	// nodegroup + GPU role via agent.env; the AMI script folds the labels + taint
	// into the one node-label list it already writes for the topology labels.
	if strings.Contains(got, "config.yaml.d/10-nodegroup.yaml") {
		t.Errorf("must not write a nodegroup node-label drop-in, got:\n%s", got)
	}
	if strings.Contains(got, "node-label:") || strings.Contains(got, "node-taint:") {
		t.Errorf("user-data must not carry any node-label/node-taint drop-in, got:\n%s", got)
	}
	if !strings.Contains(got, "SPINIFEX_NODEGROUP=ng-1") {
		t.Errorf("expected SPINIFEX_NODEGROUP in agent.env, got:\n%s", got)
	}
	if !strings.Contains(got, "SPINIFEX_GPU_NODE=true") {
		t.Errorf("expected SPINIFEX_GPU_NODE=true in agent.env for a GPU nodegroup, got:\n%s", got)
	}
	if strings.Contains(got, "K3S_NODE_LABEL") {
		t.Errorf("label must not ride the no-op K3S_NODE_LABEL env, got:\n%s", got)
	}
}

func TestBuildAgentUserData_NonGPUSignalsNodegroupOnly(t *testing.T) {
	in := baseTestInput()

	got := buildAgentUserData(in)

	if strings.Contains(got, "nvidia.com/gpu") {
		t.Errorf("non-GPU nodegroup user-data must not reference nvidia.com/gpu, got:\n%s", got)
	}
	if strings.Contains(got, "SPINIFEX_GPU_NODE") {
		t.Errorf("non-GPU nodegroup must not set SPINIFEX_GPU_NODE, got:\n%s", got)
	}
	if strings.Contains(got, "node-label:") || strings.Contains(got, "node-taint:") {
		t.Errorf("user-data must not carry any node-label/node-taint drop-in, got:\n%s", got)
	}
	// The worker signals its nodegroup via agent.env; mulga-eks-provider-id.sh
	// turns it into the eks.amazonaws.com/nodegroup label that gates the
	// nodegroup reaching ACTIVE (waitWorkersReady buckets Ready nodes by it).
	if !strings.Contains(got, "SPINIFEX_NODEGROUP=ng-1") {
		t.Errorf("expected SPINIFEX_NODEGROUP in agent.env, got:\n%s", got)
	}
	if strings.Contains(got, "K3S_NODE_LABEL") {
		t.Errorf("nodegroup label must not ride the no-op K3S_NODE_LABEL env, got:\n%s", got)
	}
}
