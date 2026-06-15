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

	agentBody := strings.Join([]string{
		"K3S_URL=" + k3sURL,
		"K3S_TOKEN=" + in.JoinToken,
		"K3S_NODE_NAME=" + in.NodeName,
		"K3S_NODE_LABEL=" + nodeLabel,
	}, "\n")

	files := []userDataFile{
		{Path: k3sFirstBootEnvPath, Perms: "0600", Body: envBody},
		{Path: agentEnvPath, Perms: "0600", Body: agentBody},
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

	buf.WriteString("runcmd:\n")
	buf.WriteString("  - [ rc-update, add, local, default ]\n")
	buf.WriteString("  - [ rc-service, local, start ]\n")

	return buf.String()
}
