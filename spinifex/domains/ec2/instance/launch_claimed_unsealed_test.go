// Drives launchClaimedInstance down its capacity refusal, which is where a node
// decides whether the start may fall back to another one.
//
//test:in-package — launchClaimedInstance and the service's dependencies are unexported.
package instance

import (
	"context"
	"errors"
	"testing"

	viperblocklegacyv1 "github.com/mulgadc/spinifex/contracts/viperblockd/legacy/v1"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeUnsealedChecker struct {
	held   []string
	err    error
	asked  []string
	nodeID string
}

func (f *fakeUnsealedChecker) HoldsUnsealedWrites(_ context.Context, nodeID string, volumeIDs []string) ([]string, error) {
	f.nodeID = nodeID
	f.asked = volumeIDs
	return f.held, f.err
}

func refusedLaunch(t *testing.T, checker *fakeUnsealedChecker) error {
	t.Helper()
	svc, _ := claimedRelaunchService(t, func(*vm.VM) error { return nil })
	svc.SetUnsealedWritesChecker(checker, "node-a")

	// An instance type this node does not offer is the simplest capacity refusal.
	instance := &vm.VM{ID: "i-held", AccountID: "acc", InstanceType: "no.such.type"}
	instance.EBSRequests.Requests = []viperblocklegacyv1.EBSRequest{{Name: "vol-root", Boot: true}, {Name: "vol-data"}}
	return svc.launchClaimedInstance(t.Context(), instance, func() {})
}

func TestLaunchClaimedInstance_CapacityRefusalWhileHoldingUnsealedWrites(t *testing.T) {
	checker := &fakeUnsealedChecker{held: []string{"vol-root"}}
	err := refusedLaunch(t, checker)

	typed, ok := errors.AsType[*UnsealedWritesError](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, []string{"vol-root"}, typed.Volumes)
	assert.Equal(t, awserrors.ErrorInsufficientInstanceCapacity, awserrors.ValidErrorCodeFromError(err))
	assert.Equal(t, "node-a", checker.nodeID)
	assert.Equal(t, []string{"vol-root", "vol-data"}, checker.asked)
}

func TestLaunchClaimedInstance_CapacityRefusalWithNothingUnsealedIsPlain(t *testing.T) {
	err := refusedLaunch(t, &fakeUnsealedChecker{})

	_, typed := errors.AsType[*UnsealedWritesError](err)
	assert.False(t, typed)
	assert.Equal(t, awserrors.ErrorInsufficientInstanceCapacity, awserrors.ValidErrorCodeFromError(err))
}

func TestLaunchClaimedInstance_CapacityRefusalFailsSafeWhenTheCheckFails(t *testing.T) {
	err := refusedLaunch(t, &fakeUnsealedChecker{err: errors.New("nats: timeout")})

	typed, ok := errors.AsType[*UnsealedWritesError](err)
	require.True(t, ok, "an unanswered check must refuse the fallback, got %v", err)
	require.Error(t, typed.CheckErr)
	assert.Equal(t, awserrors.ErrorInsufficientInstanceCapacity, awserrors.ValidErrorCodeFromError(err))
}
