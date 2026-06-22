package handlers_eks

import (
	"fmt"
	"strings"
)

// agentUserDataInput is the input shape for buildAgentUserData. ServerURL is the
// cluster's published apiserver endpoint (the NLB front-end, https://<ip>:443),
// the K3s join target reachable from the customer VPC under the Set-B topology;
// JoinToken is the decrypted K3s node token.
type agentUserDataInput struct {
	ClusterName   string
	NodegroupName string
	ServerURL     string
	JoinToken     string
	NodeName      string

	// ECR registry wiring for the kubelet credential provider. GatewayURL is the
	// worker-reachable gateway base URL; GatewayCACert is the inline CA PEM;
	// RegistryHost is the precomputed <acct>.dkr.ecr.<region>.<suffix>; GatewayIP
	// is the IP host parsed from GatewayURL for the /etc/hosts mapping (empty when
	// the gateway is a hostname).
	GatewayURL    string
	GatewayCACert string
	Region        string
	AccountID     string
	RegistryHost  string
	GatewayIP     string
}

// buildAgentUserData renders the cloud-config YAML for a nodegroup worker VM.
// Mirrors buildK3sUserData structure; SPINIFEX_K3S_ROLE=agent enables k3s-agent.
func buildAgentUserData(in agentUserDataInput) string {
	// K3S_URL is the published NLB endpoint (:443→:6443); the in-VPC CP ENI IP
	// lives in the unpeered managed CP VPC and is unreachable from the worker VPC.
	k3sURL := in.ServerURL
	nodeLabel := "eks.amazonaws.com/nodegroup=" + in.NodegroupName

	envBody := strings.Join([]string{
		"SPINIFEX_K3S_ROLE=agent",
	}, "\n")

	// The ECR credential provider reads /etc/spinifex-eks/agent.env, so the EKS_*
	// settings live alongside the K3s join vars here.
	agentBody := strings.Join([]string{
		"K3S_URL=" + k3sURL,
		"K3S_TOKEN=" + in.JoinToken,
		"K3S_NODE_NAME=" + in.NodeName,
		"K3S_NODE_LABEL=" + nodeLabel,
		"EKS_GATEWAY_URL=" + in.GatewayURL,
		"EKS_GATEWAY_CA=" + k3sGatewayCAPath,
		"EKS_REGION=" + in.Region,
		"EKS_ACCOUNT_ID=" + in.AccountID,
	}, "\n")

	registriesYAML := strings.Join([]string{
		"mirrors:",
		"  \"" + in.RegistryHost + "\":",
		"    endpoint:",
		"      - \"https://" + in.RegistryHost + ":9999\"",
		"configs:",
		"  \"" + in.RegistryHost + ":9999\":",
		"    tls:",
		"      ca_file: " + k3sGatewayCAPath,
	}, "\n")

	credProviderDropin := strings.Join([]string{
		"kubelet-arg:",
		"  - \"image-credential-provider-config=/etc/spinifex-eks/credential-provider-config.yaml\"",
		"  - \"image-credential-provider-bin-dir=/usr/local/bin\"",
	}, "\n")

	credProviderConfig := strings.Join([]string{
		"apiVersion: kubelet.config.k8s.io/v1",
		"kind: CredentialProviderConfig",
		"providers:",
		"  - name: ecr-credential-provider",
		"    matchImages:",
		"      - \"" + in.RegistryHost + "\"",
		"      - \"" + in.RegistryHost + ":9999\"",
		"    defaultCacheDuration: \"10h\"",
		"    apiVersion: credentialprovider.kubelet.k8s.io/v1",
		"    args: []",
	}, "\n")

	files := []userDataFile{
		{Path: k3sFirstBootEnvPath, Perms: "0600", Body: envBody},
		{Path: agentEnvPath, Perms: "0600", Body: agentBody},
		{Path: k3sGatewayCAPath, Perms: "0644", Body: strings.TrimRight(in.GatewayCACert, "\n")},
		{Path: "/etc/rancher/k3s/registries.yaml", Perms: "0644", Body: registriesYAML},
		{Path: "/etc/rancher/k3s/config.yaml.d/20-ecr-credential-provider.yaml", Perms: "0644", Body: credProviderDropin},
		{Path: "/etc/spinifex-eks/credential-provider-config.yaml", Perms: "0644", Body: credProviderConfig},
		{
			Path:  "/etc/local.d/imds-onlink-route.start",
			Perms: "0755",
			Body: "#!/bin/sh\n" +
				"dev=$(ip route show default | awk '{print $5; exit}')\n" +
				"[ -n \"$dev\" ] && ip route replace " + imdsServerIP + "/32 dev \"$dev\" scope link",
		},
	}

	var buf strings.Builder
	buf.WriteString("#cloud-config\n")

	// bootcmd (not write_files): /etc/resolv.conf is a dangling symlink on Alpine; see buildK3sUserData.
	buf.WriteString("bootcmd:\n")
	buf.WriteString("  - rm -f " + k3sResolvConfPath + "\n")
	fmt.Fprintf(&buf, "  - printf '%s\\n' > %s\n",
		strings.ReplaceAll(k3sResolvConf, "\n", "\\n"), k3sResolvConfPath)
	// /etc/hosts maps the registry host to the gateway IP before k3s starts; only
	// when the gateway is reachable by IP (else DNS must resolve the host).
	if in.GatewayIP != "" && in.RegistryHost != "" {
		hostsLine := in.GatewayIP + " " + in.RegistryHost + " ecr." + in.Region + "." + suffixFromRegistryHost(in.RegistryHost, in.Region)
		fmt.Fprintf(&buf, "  - grep -q '%s' /etc/hosts || printf '%s\\n' >> /etc/hosts\n", in.RegistryHost, hostsLine)
	}

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

	// Enable OpenRC `local` for reboot persistence, then run the IMDS route script
	// directly. Starting the service here would deadlock: runcmd runs inside
	// cloud-final, but `local` is ordered after it — blocking until OpenRC times out.
	buf.WriteString("runcmd:\n")
	buf.WriteString("  - [ rc-update, add, local, default ]\n")
	buf.WriteString("  - [ /etc/local.d/imds-onlink-route.start ]\n")

	return buf.String()
}

// suffixFromRegistryHost extracts the internal DNS suffix from a registry host of
// the form <acct>.dkr.ecr.<region>.<suffix>, used for the ecr.<region>.<suffix>
// alias in /etc/hosts. Returns "" when the host does not carry the expected
// .ecr.<region>. marker.
func suffixFromRegistryHost(host, region string) string {
	marker := ".ecr." + region + "."
	if _, after, ok := strings.Cut(host, marker); ok {
		return after
	}
	return ""
}
