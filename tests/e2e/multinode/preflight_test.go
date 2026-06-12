//go:build e2e

package multinode

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// runPreflight checks /dev/kvm is writable on node1 and SSH `hostname` succeeds on every peer.
// Catches misconfigured images or broken peer networking before any AWS API churn.
func runPreflight(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Multinode — Pre-flight")

	harness.Step(t, "verify /dev/kvm")
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("/dev/kvm not writable: %v", err)
	}
	_ = f.Close()

	harness.Step(t, "peer_ssh hostname on every remote node")
	ssh := harness.NewPeerSSH()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Skip node1 (the runner): SSH back to its own loopback tests nothing about peer reachability.
	for _, n := range fix.Cluster.Nodes[1:] {
		if _, err := ssh.Run(ctx, n.Addr, "hostname"); err != nil {
			t.Fatalf("peer_ssh %s (%s): %v", n.Name, n.Addr, err)
		}
		harness.Detail(t, "node", n.Name, "addr", n.Addr, "ssh", "ok")
	}
}
