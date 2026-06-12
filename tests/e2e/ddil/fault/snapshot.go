//go:build e2e

package fault

import (
	"context"
	"fmt"
	"testing"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// InstanceSnapshot captures a point-in-time view of a daemon's locally known instances.
// Take one before and after a fault/heal cycle, then call AssertPreserved to prove no
// instances were lost or regressed to a terminal state.
type InstanceSnapshot []harness.LocalInstance

// TakeSnapshot reads /local/instances on node and returns the result as an InstanceSnapshot.
// DaemonClient is passed explicitly so each scenario controls its own connection reuse.
func TakeSnapshot(ctx context.Context, d *harness.DaemonClient, node harness.Node) (InstanceSnapshot, error) {
	xs, err := d.Instances(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("ddil fault: snapshot %s: %w", node.Name, err)
	}
	return InstanceSnapshot(xs), nil
}

// AssertPreserved fails t if any instance present in pre is absent from post or has
// regressed from a live state (pending/running) to a terminal state without explicit scenario intent.
func (pre InstanceSnapshot) AssertPreserved(t *testing.T, post InstanceSnapshot) {
	t.Helper()

	postByID := make(map[string]harness.LocalInstance, len(post))
	for _, p := range post {
		postByID[p.InstanceID] = p
	}

	var missing []string
	var regressed []string
	for _, p := range pre {
		q, ok := postByID[p.InstanceID]
		if !ok {
			missing = append(missing, p.InstanceID)
			continue
		}
		if stateRegressed(p.State, q.State) {
			regressed = append(regressed, fmt.Sprintf("%s: %s → %s", p.InstanceID, p.State, q.State))
		}
	}

	if len(missing) == 0 && len(regressed) == 0 {
		return
	}
	if len(missing) > 0 {
		t.Errorf("ddil fault: instances disappeared from snapshot: %v", missing)
	}
	if len(regressed) > 0 {
		t.Errorf("ddil fault: instances regressed to a terminal state: %v", regressed)
	}
	t.FailNow()
}

// stateOrdinal maps EC2 instance states to lifecycle position; -1 for unknown.
func stateOrdinal(s string) int {
	switch s {
	case "pending":
		return 0
	case "running":
		return 1
	case "stopping":
		return 2
	case "stopped":
		return 3
	case "shutting-down":
		return 4
	case "terminated":
		return 5
	default:
		return -1
	}
}

// stateRegressed reports whether an instance moved from a live state (pending/running)
// to a terminal state. Unknown states are treated as non-regressing to tolerate schema drift.
func stateRegressed(pre, post string) bool {
	preOrd := stateOrdinal(pre)
	postOrd := stateOrdinal(post)
	if preOrd < 0 || postOrd < 0 {
		return false
	}
	return preOrd <= 1 && postOrd >= 2
}
