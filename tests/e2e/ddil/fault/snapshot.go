//go:build e2e

package fault

import (
	"context"
	"fmt"
	"testing"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// InstanceSnapshot captures a point-in-time view of a single daemon's locally
// known instances. Scenarios take a snapshot before injecting a fault and a
// second snapshot after the cluster heals, then call AssertPreserved to prove
// the daemon did not lose or regress any instance it had been tracking.
//
// Reuses the LocalInstance type from harness/daemon_client.go. Once
// daemon-local-autonomy §1a lands, that placeholder will be replaced by the
// daemon's authoritative struct and this alias will pick up the real schema
// without scenario changes.
type InstanceSnapshot []harness.LocalInstance

// TakeSnapshot reads /local/instances on the given node and returns the
// response as an InstanceSnapshot.
//
// The DaemonClient is passed explicitly (rather than through a package-level
// singleton) so each scenario owns its connection reuse and so dry-run
// scenarios can stub with a fake client once the endpoint exists.
func TakeSnapshot(ctx context.Context, d *harness.DaemonClient, node harness.Node) (InstanceSnapshot, error) {
	xs, err := d.Instances(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("ddil fault: snapshot %s: %w", node.Name, err)
	}
	return InstanceSnapshot(xs), nil
}

// AssertPreserved fails t if any instance present in the pre-snapshot is
// absent from post, or if any instance regressed from a live state (pending,
// running) to a terminal state (shutting-down, stopped, terminated) without
// explicit scenario intent.
//
// EC2-style state ordering is used rather than the daemon's internal enum
// because /local/instances reports the same `state` string the gateway
// exposes over `ec2.DescribeInstances`. When daemon-local-autonomy §1a
// replaces LocalInstance with the daemon type, the stateOrdinal map below
// should be pointed at the authoritative constants.
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

// stateOrdinal maps EC2 instance states to a monotonic lifecycle position.
// Unknown states return -1 so we can distinguish them from "pending" (0) and
// refuse to flag a regression we can't reason about.
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

// stateRegressed reports whether a live instance (pending/running) moved to
// a terminal state (stopping/stopped/shutting-down/terminated). Moves within
// the live band or within the terminal band are not flagged, and unknown
// states are treated as non-regressing so the assertion does not fail on
// schema drift.
func stateRegressed(pre, post string) bool {
	preOrd := stateOrdinal(pre)
	postOrd := stateOrdinal(post)
	if preOrd < 0 || postOrd < 0 {
		return false
	}
	return preOrd <= 1 && postOrd >= 2
}
