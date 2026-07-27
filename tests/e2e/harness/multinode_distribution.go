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
