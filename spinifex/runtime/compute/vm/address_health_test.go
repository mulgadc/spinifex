package vm_test

import (
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// managerWithGuests returns a manager holding one VM per argument, each with a
// primary ENI named after it.
func managerWithGuests(t *testing.T, ids ...string) *vm.Manager {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	m := vm.NewManager()
	for _, id := range ids {
		m.Insert(&vm.VM{ID: id, ENIId: "eni-" + id, Status: vm.StateRunning})
	}
	return m
}

// The guest named is the guest marked, and no other. A pass that marked more
// than it found would report healthy instances as unreachable, which is the
// mirror of the fault it exists to catch.
func TestOnlyTheGuestWhoseAddressIsDarkIsMarked(t *testing.T) {
	m := managerWithGuests(t, "i-1", "i-2")

	marked := m.SetUnreachableAddresses(map[string]string{"eni-i-1": "429 too many requests"})
	assert.Equal(t, []string{"i-1"}, marked)

	dark, ok := m.Get("i-1")
	require.True(t, ok)
	assert.False(t, dark.Health.AddressUnreachableSince.IsZero())
	assert.Equal(t, "429 too many requests", dark.Health.AddressUnreachableReason)

	fine, ok := m.Get("i-2")
	require.True(t, ok)
	assert.True(t, fine.Health.AddressUnreachableSince.IsZero())
}

// A load balancer carries more than one ENI, and its public address hangs off
// one of the extras. Reading only the primary would report it healthy.
func TestAnAddressOnASecondENICountsAsTheGuestsOwn(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	m := vm.NewManager()
	m.Insert(&vm.VM{ID: "i-alb", ENIId: "eni-primary", Status: vm.StateRunning,
		ExtraENIs: []vm.ExtraENI{{ENIID: "eni-public"}}})

	marked := m.SetUnreachableAddresses(map[string]string{"eni-public": "denied"})
	assert.Equal(t, []string{"i-alb"}, marked)
}

// The marker has to clear on its own evidence. One that only ever gets set
// would leave a recovered guest reported impaired for the rest of its life.
func TestAnAddressThatArrivesClearsTheMarker(t *testing.T) {
	m := managerWithGuests(t, "i-1")

	require.Len(t, m.SetUnreachableAddresses(map[string]string{"eni-i-1": "429"}), 1)
	assert.Empty(t, m.SetUnreachableAddresses(nil))

	v, ok := m.Get("i-1")
	require.True(t, ok)
	assert.True(t, v.Health.AddressUnreachableSince.IsZero())
	assert.Empty(t, v.Health.AddressUnreachableReason)
}

// ImpairedSince is when the fault started. Re-confirming it every fifteen
// seconds must not keep resetting the clock, or the field reports the age of the
// last observation and a long outage looks brand new.
func TestReconfirmingAFaultDoesNotResetWhenItStarted(t *testing.T) {
	m := managerWithGuests(t, "i-1")

	m.SetUnreachableAddresses(map[string]string{"eni-i-1": "429"})
	v, ok := m.Get("i-1")
	require.True(t, ok)
	first := v.Health.AddressUnreachableSince
	require.False(t, first.IsZero())

	time.Sleep(time.Millisecond)
	m.SetUnreachableAddresses(map[string]string{"eni-i-1": "still 429"})
	assert.Equal(t, first, v.Health.AddressUnreachableSince)
	assert.Equal(t, "still 429", v.Health.AddressUnreachableReason,
		"the reason is the current one even though the timestamp is the original")
}
