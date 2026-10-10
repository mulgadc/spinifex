package identifiers

import (
	"fmt"
	"net"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHashMAC_FirstOctetPinnedTo02(t *testing.T) {
	// First octet must be 0x02 (not just any LAA value) to stay out of vendor OUI space.
	for i := range 1000 {
		hw, err := net.ParseMAC(HashMAC(fmt.Sprintf("id-%d", i)))
		require.NoError(t, err)
		assert.Equal(t, byte(0x02), hw[0], "first octet must be 0x02, got %#x", hw[0])
	}
}

func TestHashMAC_Determinism(t *testing.T) {
	// Same id must produce the same MAC across calls and across goroutines.
	// Reconcilers across nodes rely on this — non-determinism would
	// split-brain DHCP server_mac vs LRP MAC.
	const id = "i-abc123"
	want := HashMAC(id)

	for range 100 {
		assert.Equal(t, want, HashMAC(id))
	}

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			for range 100 {
				assert.Equal(t, want, HashMAC(id))
			}
		})
	}
	wg.Wait()
}

func TestHashMAC_IDSeparates(t *testing.T) {
	// Different ids must yield different MACs.
	a := HashMAC("subnet-aaaaaaaaaaaaaaaaa")
	b := HashMAC("subnet-bbbbbbbbbbbbbbbbb")
	assert.NotEqual(t, a, b)
}
