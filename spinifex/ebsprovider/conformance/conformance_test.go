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
		return ebsprovider.NewMemoryProvider(conformance.Capabilities)
	})
}

// TestNATSProviderConformance runs the suite against a NATSProvider backed
// by natsserve.Serve, the production-shaped neutral server, rather than a
// test-only twin: this exercises the transport this package's implementors actually run in production, not a hand-written stand-in for it.
func TestNATSProviderConformance(t *testing.T) {
	conformance.RunSuite(t, func(t *testing.T) ebsprovider.EBSProvider {
		t.Helper()
		_, conn := testutil.StartTestNATS(t)
		stop, err := natsserve.Serve(t.Context(), conn, ebsprovider.NewMemoryProvider(conformance.Capabilities), natsserve.Options{})
		require.NoError(t, err)
		t.Cleanup(stop)
		return ebsprovider.NewNATSProvider(conn, 5*time.Second)
	})
}
