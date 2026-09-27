//go:build e2e

package harness

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// InstanceHostingNode returns the cluster node running the named instance's
// qemu process (ps auxw | grep qemu-system). Returns nil if no node owns it.
func InstanceHostingNode(t *testing.T, c *Cluster, instanceID string) *Node {
	t.Helper()
	hosting := InstanceHostingNodes(t, c, instanceID, nil)
	if len(hosting) == 0 {
		return nil
	}
	return &hosting[0]
}

// InstanceHostingNodes returns every node running the named instance's qemu
// process, so a caller can assert there is exactly one rather than assume it.
// Nodes in skip are not contacted, for a node deliberately taken down.
//
// A node that cannot be reached is skipped rather than fatal: this is used to
// prove an instance did not come back in two places at once, and a node that
// answers nothing is running nothing that matters to that question.
func InstanceHostingNodes(t *testing.T, c *Cluster, instanceID string, skip []string) []Node {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ssh := NewPeerSSH()

	var hosting []Node
	for i := range c.Nodes {
		n := c.Nodes[i]
		if slices.Contains(skip, n.Name) {
			continue
		}
		out, err := runPSGrep(ctx, ssh, n, instanceID)
		if err != nil {
			t.Logf("InstanceHostingNodes: %s did not answer for %s: %v", n.Name, instanceID, err)
			continue
		}
		if strings.Contains(out, instanceID) {
			hosting = append(hosting, c.Nodes[i])
		}
	}
	return hosting
}

// runPSGrep runs the ps+grep pipeline on a node and returns combined output.
func runPSGrep(ctx context.Context, ssh *PeerSSH, n Node, instanceID string) (string, error) {
	cmd := fmt.Sprintf("ps auxw | grep %q | grep qemu-system | grep -v grep", instanceID)
	out, err := ssh.Run(ctx, n.Addr, cmd)
	if err != nil {
		// exit 1 from grep means no match, not a transport error.
		if exitErr, ok := asExitErr(err); ok && exitErr.ExitCode() == 1 {
			return "", nil
		}
		return string(out), err
	}
	return string(out), nil
}

func asExitErr(err error) (*exec.ExitError, bool) {
	for err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee, true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return nil, false
		}
		err = u.Unwrap()
	}
	return nil, false
}
