package dhcp

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPromiscRefCount verifies the bridge stays in IFF_PROMISC across
// overlapping DORAs: first caller flips on, intermediate callers piggy-back
// without touching the flag, last caller flips off. Catches the regression
// where nightly cell 1 timed out on "no matching response packet received"
// because chaddr-derived 02:xx unicast OFFERs never reach AF_PACKET on a
// non-promisc Linux bridge.
func TestPromiscRefCount(t *testing.T) {
	defer resetPromiscState()
	var (
		mu     sync.Mutex
		states []bool
	)
	setPromiscBackup := setPromiscFn
	t.Cleanup(func() { setPromiscFn = setPromiscBackup })
	setPromiscFn = func(iface string, on bool) error {
		mu.Lock()
		defer mu.Unlock()
		states = append(states, on)
		return nil
	}

	rel1, err := enableBridgePromisc("br-wan")
	require.NoError(t, err)
	rel2, err := enableBridgePromisc("br-wan")
	require.NoError(t, err)
	rel3, err := enableBridgePromisc("br-wan")
	require.NoError(t, err)

	mu.Lock()
	assert.Equal(t, []bool{true}, states, "only first acquire should flip PROMISC on")
	mu.Unlock()

	require.NoError(t, rel1())
	require.NoError(t, rel2())
	mu.Lock()
	assert.Equal(t, []bool{true}, states, "PROMISC must stay on while at least one lease in flight")
	mu.Unlock()

	require.NoError(t, rel3())
	mu.Lock()
	assert.Equal(t, []bool{true, false}, states, "last release flips PROMISC off")
	mu.Unlock()
}

func TestPromiscDifferentBridgesIndependent(t *testing.T) {
	defer resetPromiscState()
	type call struct {
		iface string
		on    bool
	}
	var (
		mu    sync.Mutex
		calls []call
	)
	setPromiscBackup := setPromiscFn
	t.Cleanup(func() { setPromiscFn = setPromiscBackup })
	setPromiscFn = func(iface string, on bool) error {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, call{iface, on})
		return nil
	}

	relA, err := enableBridgePromisc("br-wan")
	require.NoError(t, err)
	relB, err := enableBridgePromisc("br-mgmt")
	require.NoError(t, err)

	require.NoError(t, relA())
	require.NoError(t, relB())

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []call{
		{"br-wan", true},
		{"br-mgmt", true},
		{"br-wan", false},
		{"br-mgmt", false},
	}, calls)
}

func resetPromiscState() {
	promiscMu.Lock()
	defer promiscMu.Unlock()
	for k := range promiscRefs {
		delete(promiscRefs, k)
	}
}
