package qemunbdd_test

// The point of this provider: the same contract suite that covers
// MemoryProvider, run unmodified against an implementation that shares no
// code with viperblock and stores real qcow2 files.

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs/conformance"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs/natsserve"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/nbd"
	hostprocess "github.com/mulgadc/spinifex/spinifex/runtime/host/process"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/qemunbdd"
	"github.com/stretchr/testify/require"
)

// requireQEMUTools skips when the host has no qemu, so the suite stays
// runnable on a machine that cannot exercise the real binaries. CI sets
// SPINIFEX_REQUIRE_CONFORMANCE_TOOLS so a runner image that loses qemu fails
// loudly instead of quietly leaving MemoryProvider as the only implementation
// the contract suite judges.
func requireQEMUTools(t testing.TB) {
	t.Helper()
	for _, bin := range []string{"qemu-img", "qemu-nbd", "qemu-io"} {
		if _, err := exec.LookPath(bin); err != nil {
			if os.Getenv("SPINIFEX_REQUIRE_CONFORMANCE_TOOLS") != "" {
				t.Fatalf("%s not installed, but SPINIFEX_REQUIRE_CONFORMANCE_TOOLS is set", bin)
			}
			t.Skipf("%s not installed", bin)
		}
	}
}

// shortTempDir is used instead of t.TempDir because the test name is part of
// that path, and AF_UNIX truncates a socket path at 108 bytes. Real base
// directories are short; only the test names are not.
func shortTempDir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "qnbd") //nolint:usetesting // t.TempDir embeds the test name and overflows the 108-byte socket path limit
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func newQEMUProvider(t *testing.T) ebsprovider.EBSProvider {
	t.Helper()
	return newQEMUProviderTB(t)
}

func newQEMUProviderTB(t testing.TB) ebsprovider.EBSProvider {
	t.Helper()
	provider, err := qemunbdd.NewProvider(shortTempDir(t))
	require.NoError(t, err)
	return provider
}

// killQEMUExport stops the qemu-nbd serving an export behind the provider's
// back, through the pid file it writes beside the socket, as a crash would.
func killQEMUExport(t *testing.T, published *ebsprovider.PublishedVolume) {
	t.Helper()
	serverType, socketPath, _, _, err := nbd.ParseNBDURI(published.Export.NBD.NBDURI)
	require.NoError(t, err)
	require.Equal(t, "unix", serverType)
	data, err := os.ReadFile(strings.TrimSuffix(socketPath, ".sock") + ".pid")
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, err)
	require.NoError(t, hostprocess.ForceKillProcess(pid, 5*time.Second))
}

func TestQEMUNBDProviderConformance(t *testing.T) {
	requireQEMUTools(t)
	conformance.RunSuiteWithConfig(t, newQEMUProvider, conformance.SuiteConfig{KillExport: killQEMUExport})
}

// TestQEMUNBDExternalNBDClient checks the half of the boundary our own Go
// client can never check: whether the export we publish is usable by an NBD
// client that knows nothing about this codebase.
func TestQEMUNBDExternalNBDClient(t *testing.T) {
	requireQEMUTools(t)
	conformance.RunNBDClientSuite(t, newQEMUProvider)
}

// TestQEMUNBDProviderNATSConformance is the swappability claim itself: the
// same wire contract, the same neutral server, a provider with no viperblock
// in it, and a client that cannot tell the difference.
func TestQEMUNBDProviderNATSConformance(t *testing.T) {
	requireQEMUTools(t)
	conformance.RunSuiteWithConfig(t, func(t *testing.T) ebsprovider.EBSProvider {
		t.Helper()
		_, conn := testutil.StartTestNATS(t)
		stop, err := natsserve.Serve(t.Context(), conn, newQEMUProvider(t), natsserve.Options{})
		require.NoError(t, err)
		t.Cleanup(stop)
		return ebsprovider.NewNATSProvider(conn, 15*time.Second)
	}, conformance.SuiteConfig{KillExport: killQEMUExport})
}
