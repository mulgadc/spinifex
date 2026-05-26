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

// InstanceHostingNode returns the cluster node currently running the named
// instance's qemu process. Mirrors the bash find_instance_node helper:
// SSH each node, `ps auxw | grep <instance_id> | grep qemu-system`. Returns
// nil if no node owns the instance (caller decides whether that's fatal).
//
// Skips fix-the-process for the local node (index 0) because the bash uses a
// direct `ps` instead of SSH for self — but Go runs everywhere as part of
// the same test binary, so we accept the small redundancy and SSH back to
// self too. Avoids a special case that the bash had only because of script
// invocation context.
func InstanceHostingNode(t *testing.T, c *Cluster, instanceID string) *Node {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ssh := NewPeerSSH()
	for i := range c.Nodes {
		n := c.Nodes[i]
		out, err := runPSGrep(ctx, ssh, n, instanceID)
		if err == nil && strings.Contains(out, instanceID) {
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
// Bash uses `ps auxw | grep <id> | grep qemu-system | grep -v grep`; we
// preserve the same pipeline so future schedulers that change the qemu
// binary name (e.g. qemu-system-aarch64) surface a single-place change.
func runPSGrep(ctx context.Context, ssh *PeerSSH, n Node, instanceID string) (string, error) {
	cmd := fmt.Sprintf("ps auxw | grep %q | grep qemu-system | grep -v grep", instanceID)
	out, err := ssh.Run(ctx, n.Addr, cmd)
	if err != nil {
		// `grep` returns exit 1 when no match; treat that as "no match",
		// not a transport error.
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
