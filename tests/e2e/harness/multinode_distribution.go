//go:build e2e

package harness

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// InstanceHostingNode returns the cluster node running the named instance's
// qemu process (ps auxw | grep qemu-system). Returns nil if no node owns it.
func InstanceHostingNode(t *testing.T, c *Cluster, instanceID string) *Node {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ssh := NewPeerSSH()
	for i := range c.Nodes {
		n := c.Nodes[i]
		out, err := runPSGrep(ctx, ssh, n, instanceID)
		if err != nil {
			t.Fatalf("discover instance %s on %s: %v", instanceID, n.Name, err)
		}
		if strings.Contains(out, instanceID) {
			return &c.Nodes[i]
		}
	}
	return nil
}

// CountInstancesPerNode fans out a ps grep per node and returns a count by
// node name. Used by phase 3 distribution checks.
func CountInstancesPerNode(t *testing.T, c *Cluster, instanceIDs []string) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, id := range instanceIDs {
		if n := InstanceHostingNode(t, c, id); n != nil {
			counts[n.Name]++
		}
	}
	return counts
}

// AssertSpreadAcrossNodes fails the test if the given instances don't span
// at least minNodes distinct cluster members. Bash phase 3 logs but doesn't
// fail when distribution is sub-optimal — Go matches by accepting minNodes=1
// (logging only) at the caller's discretion.
func AssertSpreadAcrossNodes(t *testing.T, c *Cluster, instanceIDs []string, minNodes int) map[string]int {
	t.Helper()
	counts := CountInstancesPerNode(t, c, instanceIDs)
	if len(counts) < minNodes {
		t.Fatalf("AssertSpreadAcrossNodes: %d instances on %d node(s) (%v), want >= %d nodes",
			len(instanceIDs), len(counts), counts, minNodes)
	}
	return counts
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
