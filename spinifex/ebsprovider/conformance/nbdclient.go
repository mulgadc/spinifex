package conformance

// Everything else in this package asserts the contract through our own Go
// client. This file asserts the published export through libnbd's tools,
// which were written with no knowledge of this codebase.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nbdClientVolumeBytes is small enough for a full nbdcopy round trip to be
// quick and large enough to span many qcow2 clusters.
const nbdClientVolumeBytes = 64 << 20

// nbdExport is the subset of `nbdinfo --json` this suite asserts on.
type nbdExport struct {
	Name               string `json:"export-name"`
	Size               int64  `json:"export-size"`
	ReadOnly           bool   `json:"is_read_only"`
	BlockSizeMinimum   int64  `json:"block_size_minimum"`
	BlockSizePreferred int64  `json:"block_size_preferred"`
	CanFlush           bool   `json:"can_flush"`
}

type nbdInfoOutput struct {
	Exports []nbdExport `json:"exports"`
}

// assertNBDURI requires a published URI to be one an NBD client can dial:
// nbd+unix:///?socket=/path or nbd://host:port. QEMU's legacy nbd:unix:
// filename syntax is not a URI and no NBD client but QEMU accepts it.
func assertNBDURI(t *testing.T, nbdURI string) {
	t.Helper()
	require.NotEmpty(t, nbdURI)
	assert.Truef(t, strings.HasPrefix(nbdURI, "nbd+unix://") || strings.HasPrefix(nbdURI, "nbd://"),
		"published NBD URI %q is not an NBD URI", nbdURI)
}

// RequireNBDTools skips when libnbd's client tools are absent, so a host
// that cannot run an external NBD client does not report a false pass.
func RequireNBDTools(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"nbdinfo", "nbdcopy"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed (libnbd-bin)", bin)
		}
	}
}

// nbdInfo runs nbdinfo against uri and returns its single export.
func nbdInfo(t *testing.T, uri string) nbdExport {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), "nbdinfo", "--json", uri).CombinedOutput()
	require.NoErrorf(t, err, "nbdinfo %s: %s", uri, out)

	var parsed nbdInfoOutput
	require.NoError(t, json.Unmarshal(out, &parsed))
	require.Lenf(t, parsed.Exports, 1, "expected exactly one export from %s", uri)
	return parsed.Exports[0]
}

// nbdReachable reports whether nbdinfo can still connect, for asserting that
// an export has gone away.
func nbdReachable(uri string) bool {
	return exec.CommandContext(context.Background(), "nbdinfo", "--size", uri).Run() == nil
}

// publishForNBD creates a volume of nbdClientVolumeBytes and publishes it,
// returning the export the provider says to connect to.
func publishForNBD(t *testing.T, provider ebsprovider.EBSProvider, volumeID string, readOnly bool) *ebsprovider.PublishedVolume {
	t.Helper()
	ctx := context.Background()
	_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{
		Versioned:     ebsprovider.NewVersioned(),
		VolumeID:      volumeID,
		CapacityRange: ebsprovider.CapacityRange{RequiredBytes: nbdClientVolumeBytes},
	})
	require.NoError(t, err)

	pub, err := provider.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{
		Versioned: ebsprovider.NewVersioned(),
		VolumeID:  volumeID,
		NodeID:    nbdClientNodeID,
		ReadOnly:  readOnly,
	})
	require.NoError(t, err)
	require.NotNil(t, pub)
	return pub
}

const nbdClientNodeID = "node-a"

// RunNBDClientSuite drives a provider's published export with libnbd's
// client tools. newProvider must return a provider that serves real NBD;
// the in-memory reference implementation does not qualify.
func RunNBDClientSuite(t *testing.T, newProvider func(t *testing.T) ebsprovider.EBSProvider) {
	RequireNBDTools(t)

	t.Run("published URI is connectable by a standard NBD client", func(t *testing.T) {
		provider := newProvider(t)
		pub := publishForNBD(t, provider, "vol-nbd-connect", false)
		export := nbdInfo(t, pub.NBDURI)
		assert.Equal(t, int64(nbdClientVolumeBytes), export.Size)
	})

	t.Run("export advertises usable block size constraints", func(t *testing.T) {
		provider := newProvider(t)
		pub := publishForNBD(t, provider, "vol-nbd-blocksize", false)
		export := nbdInfo(t, pub.NBDURI)

		assert.Positive(t, export.BlockSizeMinimum)
		assert.GreaterOrEqual(t, export.BlockSizePreferred, export.BlockSizeMinimum)
		assert.Zero(t, nbdClientVolumeBytes%export.BlockSizeMinimum,
			"export size must be a whole number of minimum blocks")
	})

	t.Run("data written by an external client reads back byte for byte", func(t *testing.T) {
		provider := newProvider(t)
		pub := publishForNBD(t, provider, "vol-nbd-roundtrip", false)

		dir := t.TempDir()
		in := filepath.Join(dir, "in.bin")
		out := filepath.Join(dir, "out.bin")
		want := patternBytes(nbdClientVolumeBytes)
		require.NoError(t, os.WriteFile(in, want, 0o600))

		runNBDCopy(t, in, pub.NBDURI)
		runNBDCopy(t, pub.NBDURI, out)

		got, err := os.ReadFile(out)
		require.NoError(t, err)
		require.Len(t, got, len(want))
		assert.True(t, bytes.Equal(got, want), "data read back over NBD differs from what was written")
	})

	t.Run("read-only publish is advertised as read-only", func(t *testing.T) {
		provider := newProvider(t)
		pub := publishForNBD(t, provider, "vol-nbd-readonly", true)
		export := nbdInfo(t, pub.NBDURI)
		assert.True(t, export.ReadOnly, "a volume published ReadOnly must set the NBD read-only transmission flag")
	})

	t.Run("unpublish tears the export down", func(t *testing.T) {
		provider := newProvider(t)
		pub := publishForNBD(t, provider, "vol-nbd-unpublish", false)
		require.True(t, nbdReachable(pub.NBDURI), "export should be reachable before unpublish")

		require.NoError(t, provider.UnpublishVolume(context.Background(), ebsprovider.UnpublishVolumeRequest{
			Versioned: ebsprovider.NewVersioned(),
			VolumeID:  "vol-nbd-unpublish",
			NodeID:    nbdClientNodeID,
		}))

		assert.Eventually(t, func() bool { return !nbdReachable(pub.NBDURI) }, 10*time.Second, 100*time.Millisecond,
			"export still answers after UnpublishVolume")
	})
}

// runNBDCopy copies between a local file and an NBD URI in either direction.
func runNBDCopy(t *testing.T, src, dst string) {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), "nbdcopy", src, dst).CombinedOutput()
	require.NoErrorf(t, err, "nbdcopy %s %s: %s", src, dst, out)
}

// patternBytes builds a position-dependent pattern, so a copy that lands at
// the wrong offset fails as loudly as one that loses data.
func patternBytes(n int) []byte {
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = byte(i*31 + i/4096)
	}
	return buf
}
