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

// SpxGetNodesAcrossCluster runs `spx get nodes` and returns the raw output
// stripped of trailing whitespace. Same as harness.SpxGetNodes but tagged
// for multinode call sites — kept as a thin shim so the bash phase 9
// "spx get nodes after recovery" assertion has a single source of truth.
func SpxGetNodesAcrossCluster(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(SpxGetNodes(t))
}
