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
	// is the IP host parsed from GatewayURL, dialed as the registry mirror endpoint
	// and keyed (with :9999) for both the mirror and matchImages so a port-ful pull
	// resolves without DNS (empty when the gateway is a hostname needing DNS).
	GatewayURL    string
	GatewayCACert string
	Region        string
	AccountID     string
	RegistryHost  string
	GatewayIP     string

	// GPUEnabled/GPUVendor gate the GPU node label + default scheduling taint;
	// set from the owning NodegroupRecord (see gpuFieldsForInstanceTypes).
	GPUEnabled bool
	GPUVendor  string
}

// registryMirrorHosts returns the distinct registry hosts a worker must accept on
// a pod image ref: the port-less parity name, the port-ful parity name (the value
// DescribeRepositories returns), and the gateway IP:port when the gateway
// advertises an IP. containerd matches mirror keys and the credential provider
// matches matchImages verbatim against the image host, so every form must be
// listed; all resolve to the single dialed gateway endpoint.
func registryMirrorHosts(registryHost, endpointHost string) []string {
	hosts := []string{registryHost, registryHost + ":9999"}
	if endpointHost != registryHost {
		hosts = append(hosts, endpointHost+":9999")
	}
	return hosts
}

// buildAgentUserData renders the cloud-config YAML for a nodegroup worker VM.
// Mirrors buildK3sUserData structure; SPINIFEX_K3S_ROLE=agent enables k3s-agent.
func buildAgentUserData(in agentUserDataInput) string {
	// K3S_URL is the published NLB endpoint (:443→:6443); the in-VPC CP ENI IP
	// lives in the unpeered managed CP VPC and is unreachable from the worker VPC.
	k3sURL := in.ServerURL

	envBody := strings.Join([]string{
		"SPINIFEX_K3S_ROLE=agent",
	}, "\n")

	// The ECR credential provider reads /etc/spinifex-eks/agent.env, so the EKS_*
	// settings live alongside the K3s join vars here. The nodegroup label rides a
	// node-label config drop-in below, not K3S_NODE_LABEL (k3s has no such env).
	agentBody := strings.Join([]string{
		"K3S_URL=" + k3sURL,
		"K3S_TOKEN=" + in.JoinToken,
		"K3S_NODE_NAME=" + in.NodeName,
		"EKS_GATEWAY_URL=" + in.GatewayURL,
		"EKS_GATEWAY_CA=" + k3sGatewayCAPath,
		"EKS_REGION=" + in.Region,
		"EKS_ACCOUNT_ID=" + in.AccountID,
	}, "\n")

	// The mirror endpoint is dialed by IP when the gateway IP is known: the gateway
	// cert carries an IP SAN, so TLS validates without DNS, and there is no internal
	// resolver yet (cloud-init manage_etc_hosts rewrites /etc/hosts each boot). A pod
	// image ref may carry any host the API hands out — the port-less parity name, the
	// port-ful parity name (the repositoryUri value), or the gateway IP:port — so each
	// is keyed as a mirror redirecting to the one endpoint host:port actually dialed,
	// whose CA the configs tls block trusts.
	endpointHost := in.RegistryHost
	if in.GatewayIP != "" {
		endpointHost = in.GatewayIP
	}
	endpoint := "https://" + endpointHost + ":9999"
	registryLines := []string{"mirrors:"}
	for _, host := range registryMirrorHosts(in.RegistryHost, endpointHost) {
		registryLines = append(registryLines,
			"  \""+host+"\":",
			"    endpoint:",
			"      - \""+endpoint+"\"",
		)
	}
	registryLines = append(registryLines,
		"configs:",
		"  \""+endpointHost+":9999\":",
		"    tls:",
		"      ca_file: "+k3sGatewayCAPath,
	)
	registriesYAML := strings.Join(registryLines, "\n")

	credProviderDropin := strings.Join([]string{
		"kubelet-arg:",
		"  - \"image-credential-provider-config=/etc/spinifex-eks/credential-provider-config.yaml\"",
		"  - \"image-credential-provider-bin-dir=/usr/local/bin\"",
	}, "\n")

	credLines := []string{
		"apiVersion: kubelet.config.k8s.io/v1",
		"kind: CredentialProviderConfig",
		"providers:",
		"  - name: ecr-credential-provider",
		"    matchImages:",
	}
	for _, host := range registryMirrorHosts(in.RegistryHost, endpointHost) {
		credLines = append(credLines, "      - \""+host+"\"")
	}
	credLines = append(credLines,
		"    defaultCacheDuration: \"10h\"",
		"    apiVersion: credentialprovider.kubelet.k8s.io/v1",
		"    args: []",
	)
	credProviderConfig := strings.Join(credLines, "\n")

	// The eks.amazonaws.com/nodegroup label must ride a k3s node-label config
	// drop-in: k3s has no K3S_NODE_LABEL env, so agent.env cannot set it. The
	// nodegroup readiness gate (waitWorkersReady) buckets Ready nodes by this
	// label, so a worker without it never counts toward its nodegroup's ACTIVE
	// transition. k3s concatenates node-label lists across config.yaml.d drop-ins.
	nodegroupLabelDropin := strings.Join([]string{
		"node-label:",
		"  - \"eks.amazonaws.com/nodegroup=" + in.NodegroupName + "\"",
	}, "\n")

	files := []userDataFile{
		{Path: k3sFirstBootEnvPath, Perms: "0600", Body: envBody},
		{Path: agentEnvPath, Perms: "0600", Body: agentBody},
		{Path: k3sGatewayCAPath, Perms: "0644", Body: strings.TrimRight(in.GatewayCACert, "\n")},
		{Path: "/etc/rancher/k3s/registries.yaml", Perms: "0644", Body: registriesYAML},
		{Path: "/etc/rancher/k3s/config.yaml.d/20-ecr-credential-provider.yaml", Perms: "0644", Body: credProviderDropin},
		{Path: "/etc/rancher/k3s/config.yaml.d/15-nodegroup-label.yaml", Perms: "0644", Body: nodegroupLabelDropin},
		{Path: "/etc/spinifex-eks/credential-provider-config.yaml", Perms: "0644", Body: credProviderConfig},
	}

	// GPU nodes advertise nvidia.com/gpu.present=true (so the device-plugin
	// DaemonSet nodeSelector matches) and default to NoSchedule so ordinary
	// workloads don't consume scarce GPU capacity. Both ride a separate
	// node-label/node-taint drop-in; k3s merges it over the nodegroup-label one.
	if in.GPUEnabled {
		gpuDropin := strings.Join([]string{
			"node-label:",
			"  - \"nvidia.com/gpu.present=true\"",
			"node-taint:",
			"  - \"nvidia.com/gpu=present:NoSchedule\"",
		}, "\n")
		files = append(files, userDataFile{
			Path: "/etc/rancher/k3s/config.yaml.d/20-gpu.yaml", Perms: "0644", Body: gpuDropin,
		})
	}

	var buf strings.Builder
	buf.WriteString("#cloud-config\n")

	// bootcmd (not write_files): /etc/resolv.conf is a dangling symlink on Alpine; see buildK3sUserData.
	buf.WriteString("bootcmd:\n")
	buf.WriteString("  - rm -f " + k3sResolvConfPath + "\n")
	fmt.Fprintf(&buf, "  - printf '%s\\n' > %s\n",
		strings.ReplaceAll(k3sResolvConf, "\n", "\\n"), k3sResolvConfPath)

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
