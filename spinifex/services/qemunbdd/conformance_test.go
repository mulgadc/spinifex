package qemunbdd_test

// The point of this provider: the same contract suite that covers
// MemoryProvider, run unmodified against an implementation that shares no
// code with viperblock and stores real qcow2 files.

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider/conformance"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider/natsserve"
	"github.com/mulgadc/spinifex/spinifex/services/qemunbdd"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/require"
)

// requireQEMUTools skips when the host has no qemu, so the suite stays
// runnable on a machine that cannot exercise the real binaries.
func requireQEMUTools(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"qemu-img", "qemu-nbd", "qemu-io"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
}

// shortTempDir is used instead of t.TempDir because the test name is part of
// that path, and AF_UNIX truncates a socket path at 108 bytes. Real base
// directories are short; only the test names are not.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "qnbd") //nolint:usetesting // t.TempDir embeds the test name and overflows the 108-byte socket path limit
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func newQEMUProvider(t *testing.T) ebsprovider.EBSProvider {
	t.Helper()
	provider, err := qemunbdd.NewProvider(shortTempDir(t))
	require.NoError(t, err)
	return provider
}

func TestQEMUNBDProviderConformance(t *testing.T) {
	requireQEMUTools(t)
	conformance.RunSuite(t, newQEMUProvider)
}

// TestQEMUNBDProviderNATSConformance is the swappability claim itself: the
// same wire contract, the same neutral server, a provider with no viperblock
// in it, and a client that cannot tell the difference.
func TestQEMUNBDProviderNATSConformance(t *testing.T) {
	requireQEMUTools(t)
	conformance.RunSuite(t, func(t *testing.T) ebsprovider.EBSProvider {
		t.Helper()
		_, conn := testutil.StartTestNATS(t)
		stop, err := natsserve.Serve(t.Context(), conn, newQEMUProvider(t), natsserve.Options{})
		require.NoError(t, err)
		t.Cleanup(stop)
		return ebsprovider.NewNATSProvider(conn, 15*time.Second)
	})
}
