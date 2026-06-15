//go:build e2e

package harness

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// DumpAllNodeLogs fans out `journalctl -u 'spinifex-*'` per node into the
// artifact directory. Used by phase 8/9 on failure and by the package
// TestMain cleanup as a last-resort post-mortem. Tolerates SSH failure on
// any node — the log dump is best-effort, not a test gate.
//
// File naming: artifactsDir/journal-<nodeName>.log. Existing files are
// overwritten so repeat runs don't accumulate.
func DumpAllNodeLogs(t *testing.T, c *Cluster, artifactsDir string) {
	t.Helper()
	ssh := NewPeerSSH()
	for _, n := range c.Nodes {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		out, err := ssh.Run(ctx, n.Addr, "journalctl -u 'spinifex-*' -n 500 --no-pager")
		cancel()
		if err != nil {
			t.Logf("DumpAllNodeLogs: %s journalctl: %v (best-effort, skipping)", n.Name, err)
			continue
		}
		path := filepath.Join(artifactsDir, "journal-"+n.Name+".log")
		DumpFile(t, filepath.Dir(path), filepath.Base(path), out)
	}
}

// SpxGetNodesAcrossCluster runs `spx get nodes` best-effort and returns trimmed
// output. CLI ↔ NATS dial can race the cluster join after a node restart and
// return "no servers available" even when the data path is healthy; errors are
// swallowed intentionally.
func SpxGetNodesAcrossCluster(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(SpxRunBestEffort(t, "get", "nodes", "--timeout", "5s"))
}
