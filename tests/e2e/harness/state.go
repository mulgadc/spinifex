//go:build e2e

package harness

import (
	"context"
	"fmt"
	"testing"
)

// LocalStateRevision reads the daemon's local state revision via /local/status.
// AssertMonotonic uses it to verify a fault didn't rewind persisted state.
func LocalStateRevision(ctx context.Context, d *DaemonClient, node Node) (uint64, error) {
	s, err := d.Status(ctx, node)
	if err != nil {
		return 0, fmt.Errorf("e2e harness: local state revision %s: %w", node.Name, err)
	}
	return s.Revision, nil
}

// AssertMonotonic fails t if after is strictly less than before. Equal
// revisions are permitted because a scenario that exercises a read-only
// fault (e.g. NATS kill with no instance mutations) should see the revision
// unchanged.
func AssertMonotonic(t *testing.T, before, after uint64) {
	t.Helper()
	if after < before {
		t.Fatalf("e2e harness: local state revision regressed: before=%d after=%d", before, after)
	}
}
