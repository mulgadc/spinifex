//go:build e2e

package multinode

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// runPreflight is the Go port of pre-flight validation
// (run-multinode-e2e.sh:394-423). Two checks:
//   - /dev/kvm exists and is writable on the local node (we run on node1).
//   - SSH to every peer node succeeds with `hostname`.
//
// Failing this exposes a misconfigured VM image or broken peer networking
// before any AWS API churn.
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
	// Bash runs this loop from node1 and skips itself; mirror that — the
	// runner host doesn't necessarily accept SSH back into its own loopback
	// and a local hostname call would test nothing about peer reachability.
	for _, n := range fix.Cluster.Nodes[1:] {
		if _, err := ssh.Run(ctx, n.Addr, "hostname"); err != nil {
			t.Fatalf("peer_ssh %s (%s): %v", n.Name, n.Addr, err)
		}
		harness.Detail(t, "node", n.Name, "addr", n.Addr, "ssh", "ok")
	}
}
