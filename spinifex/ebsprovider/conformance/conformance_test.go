package conformance_test

import (
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider/conformance"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider/natsserve"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/require"
)

func TestMemoryProviderConformance(t *testing.T) {
	conformance.RunSuite(t, func(t *testing.T) ebsprovider.EBSProvider {
		t.Helper()
		return ebsprovider.NewMemoryProvider(conformance.ReferenceCapabilities)
	})
}

// TestMemoryProviderMinimalCapabilitiesConformance is what makes the suite
// capability-driven rather than capability-asserting: the same suite, a
// provider advertising none of the optional behaviour, and it still passes.
func TestMemoryProviderMinimalCapabilitiesConformance(t *testing.T) {
	conformance.RunSuite(t, func(t *testing.T) ebsprovider.EBSProvider {
		t.Helper()
		return ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{})
	})
}

// TestMemoryProviderOnlineExpansionConformance covers the branch no provider
// we ship exercises: expanding a published volume must succeed when the
// provider advertises that it can.
func TestMemoryProviderOnlineExpansionConformance(t *testing.T) {
	conformance.RunSuite(t, func(t *testing.T) ebsprovider.EBSProvider {
		t.Helper()
		capabilities := conformance.ReferenceCapabilities
		capabilities.OnlineExpansion = true
		return ebsprovider.NewMemoryProvider(capabilities)
	})
}

// TestNATSProviderConformance runs the suite against a NATSProvider backed
// by natsserve.Serve, the production-shaped neutral server, rather than a
// test-only twin: this exercises the transport this package's implementors actually run in production, not a hand-written stand-in for it.
func TestNATSProviderConformance(t *testing.T) {
	conformance.RunSuite(t, func(t *testing.T) ebsprovider.EBSProvider {
		t.Helper()
		_, conn := testutil.StartTestNATS(t)
		stop, err := natsserve.Serve(t.Context(), conn, ebsprovider.NewMemoryProvider(conformance.ReferenceCapabilities), natsserve.Options{})
		require.NoError(t, err)
		t.Cleanup(stop)
		return ebsprovider.NewNATSProvider(conn, 5*time.Second)
	})
}
