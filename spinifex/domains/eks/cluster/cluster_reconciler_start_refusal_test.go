package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// refusingCPControl reports every member stopped and refuses every start with
// err, which is what a restart sees while the host that last ran the member is
// at capacity or still holds its volume.
type refusingCPControl struct {
	err        error
	startCalls int
}

func (c *refusingCPControl) InstanceState(context.Context, string) (string, error) {
	return "stopped", nil
}

func (c *refusingCPControl) StartInstance(context.Context, string) error {
	c.startCalls++
	return c.err
}

func (c *refusingCPControl) StopInstance(context.Context, string) error { return nil }

// What the start path returns after crossing NATS: the code and the message the
// owning node sent, with no sentinel left to match on.
var (
	errStartCapacity = errors.New(awserrors.ErrorInsufficientInstanceCapacity)
	errStartHeld     = awserrors.Errorf(awserrors.ErrorIncorrectState, "%s",
		"volume is leased by another owner: held by node-a since 2026-10-30T01:00:00Z")
)

// A refusal that clears by itself must not use up the restart budget, or the
// reconciler gives up on a control plane that would have started a minute later.
// Replacing the member instead would discard the etcd disk it is waiting to reuse.
func TestMaybeRecoverCP_RetryableStartRefusalIsRetriedNotReplaced(t *testing.T) {
	t.Parallel()
	for name, refusal := range map[string]error{
		"insufficient capacity": errStartCapacity,
		"volume held elsewhere": errStartHeld,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			nodes := threeMemberCP()
			meta := haMeta(nodes)
			cp := &refusingCPControl{err: refusal}
			prov := &fakeCPProvisioner{node: ControlPlaneNode{NodeID: "node-c", InstanceID: "i-cp-new", ENIIP: "10.0.0.20"}}
			r, _ := newReplaceReconciler(t, cp, prov, meta)
			r.degradedSince = time.Now().Add(-10 * time.Minute)

			passes := r.maxRestartAttempts + 2
			for range passes {
				r.lastRestartAt = time.Time{} // the backoff has elapsed
				r.maybeRecoverControlPlane(t.Context(), meta, "stale")
			}

			assert.Equal(t, passes*len(nodes), cp.startCalls, "every pass still tries every member, past the attempt cap")
			assert.Zero(t, r.restartAttempts, "a refusal that clears on its own is not a failed attempt")

			r.replacingSince = time.Now().Add(-10 * time.Minute)
			r.maybeReplaceControlPlaneMember(t.Context(), meta, "")
			assert.Zero(t, prov.calls, "a member that cannot start yet is not a lost member")
		})
	}
}

// The cap still bounds the faults nothing says will clear.
func TestMaybeRecoverCP_UnknownStartFailureStillCountsAgainstTheBudget(t *testing.T) {
	t.Parallel()
	cp := &refusingCPControl{err: errors.New(awserrors.ErrorServerInternal)}
	r := newRecoveryReconciler(t, cp)
	r.degradedSince = time.Now().Add(-10 * time.Minute)

	for range r.maxRestartAttempts + 2 {
		r.lastRestartAt = time.Time{}
		r.maybeRecoverControlPlane(t.Context(), metaWithCP("i-cp"), "stale")
	}

	require.Equal(t, r.maxRestartAttempts, cp.startCalls, "stops at the attempt cap")
	assert.Equal(t, r.maxRestartAttempts, r.restartAttempts)
}
