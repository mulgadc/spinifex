package cluster

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

	// RegistryHosts lists every gateway's registry host[:port] as it appears in a
	// repositoryUri, local node first. Each is trusted and pulled from directly, with
	// the others as fallbacks. Empty keeps the single GatewayIP endpoint.
	RegistryHosts []string

	// GPUEnabled/GPUVendor gate the GPU node label + default scheduling taint;
	// set from the owning NodegroupRecord (see gpuFieldsForInstanceTypes).
	GPUEnabled bool
	GPUVendor  string
}

// registryGateways returns the gateway host[:port] endpoints a worker may dial, in
// preference order: every advertised gateway, then GatewayIP:9999 when it is not
// already listed. With neither, the parity hostname at :9999 is the only endpoint.
func registryGateways(in agentUserDataInput) []string {
	var gws []string
	seen := map[string]bool{}
	add := func(h string) {
		if h != "" && !seen[h] {
			seen[h] = true
			gws = append(gws, h)
		}
	}
	for _, h := range in.RegistryHosts {
		add(h)
	}
	if in.GatewayIP != "" {
		add(in.GatewayIP + ":9999")
	}
	if len(gws) == 0 {
		add(in.RegistryHost + ":9999")
	}
	return gws
}

// registryMirrors maps every host a pod image ref may carry to the endpoints
// containerd tries for it, and matches the credential provider's matchImages
// verbatim against the same keys. The parity names try the gateways in order; a
// gateway key tries itself first so a down gateway still falls back to the rest.
func registryMirrors(registryHost string, gateways []string) (keys []string, endpoints map[string][]string) {
	endpoints = map[string][]string{}
	for _, k := range []string{registryHost, registryHost + ":9999"} {
		keys = append(keys, k)
		endpoints[k] = gateways
	}
	for _, g := range gateways {
		if _, ok := endpoints[g]; ok {
			continue
		}
		keys = append(keys, g)
		ordered := []string{g}
		for _, o := range gateways {
			if o != g {
				ordered = append(ordered, o)
			}
		}
		endpoints[g] = ordered
	}
	return keys, endpoints
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
	// settings live alongside the K3s join vars here. SPINIFEX_NODEGROUP (and
	// SPINIFEX_GPU_NODE for GPU workers) are consumed by mulga-eks-provider-id.sh,
	// the single writer of the k3s node-label list: k3s replaces (not merges) the
	// node-label list across config.yaml.d files, so all node-labels must come
	// from one drop-in or the last-sorted file silently wipes the rest.
	agentLines := []string{
		"K3S_URL=" + k3sURL,
		"K3S_TOKEN=" + in.JoinToken,
		"K3S_NODE_NAME=" + in.NodeName,
		"EKS_GATEWAY_URL=" + in.GatewayURL,
		"EKS_GATEWAY_CA=" + k3sGatewayCAPath,
		"EKS_REGION=" + in.Region,
		"EKS_ACCOUNT_ID=" + in.AccountID,
		"SPINIFEX_NODEGROUP=" + in.NodegroupName,
	}
	if in.GPUEnabled {
		agentLines = append(agentLines, "SPINIFEX_GPU_NODE=true")
	}
	agentBody := strings.Join(agentLines, "\n")

	// Gateways are dialed by IP: the gateway cert carries IP SANs so TLS validates
	// without DNS, and there is no internal resolver yet (cloud-init
	// manage_etc_hosts rewrites /etc/hosts each boot). Every endpoint is trusted via
	// the configs tls block below.
	gateways := registryGateways(in)
	mirrorKeys, mirrorEndpoints := registryMirrors(in.RegistryHost, gateways)
	registryLines := []string{"mirrors:"}
	for _, key := range mirrorKeys {
		registryLines = append(registryLines, "  \""+key+"\":", "    endpoint:")
		for _, ep := range mirrorEndpoints[key] {
			registryLines = append(registryLines, "      - \"https://"+ep+"\"")
		}
	}
	registryLines = append(registryLines, "configs:")
	for _, g := range gateways {
		registryLines = append(registryLines,
			"  \""+g+"\":",
			"    tls:",
			"      ca_file: "+k3sGatewayCAPath,
		)
	}
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
	for _, host := range mirrorKeys {
		credLines = append(credLines, "      - \""+host+"\"")
	}
	credLines = append(credLines,
		"    defaultCacheDuration: \"10h\"",
		"    apiVersion: credentialprovider.kubelet.k8s.io/v1",
		"    args: []",
	)
	credProviderConfig := strings.Join(credLines, "\n")

	// buildAgentUserData writes NO config.yaml.d node-label/node-taint drop-in:
	// mulga-eks-provider-id.sh (AMI-baked) is the sole node-label writer. It reads
	// SPINIFEX_NODEGROUP/SPINIFEX_GPU_NODE from agent.env and folds the nodegroup
	// label (plus nvidia.com/gpu.present + the GPU NoSchedule taint) into the same
	// 10-provider-id.yaml node-label list it writes for the EBS CSI topology labels.
	files := []userDataFile{
		{Path: k3sFirstBootEnvPath, Perms: "0600", Body: envBody},
		{Path: agentEnvPath, Perms: "0600", Body: agentBody},
		{Path: k3sGatewayCAPath, Perms: "0644", Body: strings.TrimRight(in.GatewayCACert, "\n")},
		{Path: "/etc/rancher/k3s/registries.yaml", Perms: "0644", Body: registriesYAML},
		{Path: "/etc/rancher/k3s/config.yaml.d/20-ecr-credential-provider.yaml", Perms: "0644", Body: credProviderDropin},
		{Path: "/etc/spinifex-eks/credential-provider-config.yaml", Perms: "0644", Body: credProviderConfig},
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
