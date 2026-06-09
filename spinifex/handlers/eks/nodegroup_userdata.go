package handlers_eks

import (
	"fmt"
	"net"
	"strings"
)

// agentUserDataInput is the input shape for buildAgentUserData. ControlPlaneENIIP
// is the cluster's in-VPC control-plane ENI private IP (the K3s join target);
// JoinToken is the decrypted K3s node token.
type agentUserDataInput struct {
	ClusterName       string
	NodegroupName     string
	ControlPlaneENIIP string
	JoinToken         string
	NodeName          string
}

// buildAgentUserData renders the cloud-config YAML a nodegroup worker consumes
// at first boot. It mirrors buildK3sUserData: a bootcmd resolver fix, a single
// write_files block (first-boot.env + agent.env + the IMDS on-link route
// script), and a runcmd that enables the OpenRC `local` service. Output is
// unencoded YAML; the caller base64-wraps it for the RunInstances UserData
// field. The eks-node-role first-boot selector reads SPINIFEX_K3S_ROLE=agent
// (and the presence of agent.env) to enable the k3s-agent service, which sources
// agent.env for K3S_URL/K3S_TOKEN.
func buildAgentUserData(in agentUserDataInput) string {
	// K3S_URL targets the control-plane ENI's in-VPC private IP on :6443
	// because the cluster NLB DNS endpoint is not resolvable in-VPC yet.
	// Workers share the VPC with the server, so this is directly reachable and
	// TLS-pinned by the join token's CA hash (SAN/hostname irrelevant for the
	// k3s agent join). Switch to the NLB endpoint on :443 once authoritative
	// in-VPC DNS is available.
	k3sURL := "https://" + net.JoinHostPort(in.ControlPlaneENIIP, "6443")
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

	// Resolver fix via bootcmd (not write_files) for the same reason as the
	// server path: /etc/resolv.conf is a dangling symlink on the Alpine AMI, so
	// write_files would follow the dead link and abort the whole block.
	// Containerd needs a working resolver to pull the worker's system images.
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
