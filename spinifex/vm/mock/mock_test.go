package mock

import (
	"errors"
	"testing"

	handlers_ec2_instance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/instance"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This check lives here rather than in mock.go so mock.go's own import set
// stays minimal (vm only): handlers/ec2/instance imports vm, and pulling it
// into mock.go would make its own internal tests importing this package a
// cycle. Test files of this package are never re-exported to importers, so
// the check is just as effective here.
var _ handlers_ec2_instance.StoppedInstanceStore = (*StateStore)(nil)

func TestSaveAndLoadRunningState_RoundTrips(t *testing.T) {
	s := New()
	require.NoError(t, s.SaveRunningState("node-1", map[string]*vm.VM{"i-1": {ID: "i-1"}}))

	got, err := s.LoadRunningState("node-1")
	require.NoError(t, err)
	require.Contains(t, got, "i-1")

	empty, err := s.LoadRunningState("node-2")
	require.NoError(t, err)
	assert.Empty(t, empty)
}

func TestClaimStoppedInstance_MissingReturnsErrStoppedInstanceClaimed(t *testing.T) {
	s := New()
	_, err := s.ClaimStoppedInstance("i-missing")
	assert.ErrorIs(t, err, vm.ErrStoppedInstanceClaimed)
	assert.Equal(t, 1, s.ClaimAttempts)
}

func TestClaimAfterLoad_RemovesEntryOnLoad(t *testing.T) {
	s := New()
	s.Stopped["i-1"] = &vm.VM{ID: "i-1"}
	s.ClaimAfterLoad = true

	got, err := s.LoadStoppedInstance("i-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, []string{"i-1"}, s.ClaimedStopped)

	_, err = s.ClaimStoppedInstance("i-1")
	assert.ErrorIs(t, err, vm.ErrStoppedInstanceClaimed)
}

func TestDeleteStoppedInstance_FailFirstThenSucceeds(t *testing.T) {
	s := New()
	s.Stopped["i-1"] = &vm.VM{ID: "i-1"}
	s.DeleteFailFirst = true

	require.Error(t, s.DeleteStoppedInstance("i-1"))
	require.NoError(t, s.DeleteStoppedInstance("i-1"))
	assert.Equal(t, 2, s.DeleteAttempts)
	assert.Contains(t, s.DeletedStopped, "i-1")
}

func TestUpdateStoppedInstance_MissingReturnsErrKeyNotFound(t *testing.T) {
	s := New()
	_, err := s.UpdateStoppedInstance("i-missing", func(*vm.VM) {})
	assert.ErrorIs(t, err, jetstream.ErrKeyNotFound)
	assert.Equal(t, 1, s.UpdateStoppedCalls)
}

func TestWriteStoppedInstance_ErrInjectionAndHook(t *testing.T) {
	s := New()
	var hookCalls int
	s.OnWriteStoppedInstance = func(id string, v *vm.VM) { hookCalls++ }

	require.NoError(t, s.WriteStoppedInstance("i-1", &vm.VM{ID: "i-1"}))
	assert.Equal(t, 1, hookCalls)
	assert.NotNil(t, s.WroteStopped["i-1"])
	assert.NotNil(t, s.Stopped["i-1"])

	s.WriteStoppedErr = errors.New("kv down")
	err := s.WriteStoppedInstance("i-2", &vm.VM{ID: "i-2"})
	assert.EqualError(t, err, "kv down")
}

func TestUpdateTerminatedInstance_MissingErrors(t *testing.T) {
	s := New()
	_, err := s.UpdateTerminatedInstance("i-missing", func(*vm.VM) {})
	require.Error(t, err)
}
