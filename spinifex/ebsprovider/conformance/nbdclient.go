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
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// defaultNBDVolumeBytes is small enough for a full nbdcopy round trip to be
// quick and large enough to span many allocation units.
const defaultNBDVolumeBytes int64 = 64 << 20

const defaultNBDNodeID = "node-a"

const defaultNBDVolumePrefix = "vol-nbd"

// reconnectTimeout bounds how long an export may refuse a reconnecting
// client while it finishes tearing the previous connection down.
const reconnectTimeout = 2 * time.Minute

// NBDClientConfig adapts the suite to where it runs. An in-process provider
// needs none of it; a live cluster needs its own node name, and unique
// volume IDs so a rerun does not collide with a previous run's leftovers.
type NBDClientConfig struct {
	NodeID       string
	VolumePrefix string
	VolumeBytes  int64
}

func (c NBDClientConfig) withDefaults() NBDClientConfig {
	if c.NodeID == "" {
		c.NodeID = defaultNBDNodeID
	}
	if c.VolumePrefix == "" {
		c.VolumePrefix = defaultNBDVolumePrefix
	}
	if c.VolumeBytes == 0 {
		c.VolumeBytes = defaultNBDVolumeBytes
	}
	return c
}

// nbdExport is the subset of `nbdinfo --json` this suite asserts on.
type nbdExport struct {
	Name               string   `json:"export-name"`
	Size               int64    `json:"export-size"`
	ReadOnly           bool     `json:"is_read_only"`
	BlockSizeMinimum   int64    `json:"block_size_minimum"`
	BlockSizePreferred int64    `json:"block_size_preferred"`
	CanFlush           bool     `json:"can_flush"`
	Contexts           []string `json:"contexts"`
}

type nbdInfoOutput struct {
	Exports []nbdExport `json:"exports"`
}

// nbdExtent is one entry of `nbdinfo --map --json`. Description is libnbd's
// rendering of the allocation flags, e.g. "data" or "hole,zero".
type nbdExtent struct {
	Offset      int64  `json:"offset"`
	Length      int64  `json:"length"`
	Description string `json:"description"`
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

// dialURI normalises whatever form a provider published into one libnbd will
// dial, so a provider emitting the legacy syntax still gets its export
// tested rather than every subtest failing on the same string.
func dialURI(t *testing.T, nbdURI string) string {
	t.Helper()
	serverType, socketPath, host, port, err := utils.ParseNBDURI(nbdURI)
	require.NoErrorf(t, err, "parse published NBD URI %q", nbdURI)

	if serverType == "unix" {
		return utils.FormatNBDSocketURI(socketPath)
	}
	return utils.FormatNBDTCPURI(host, port)
}

// RequireNBDTools skips when libnbd's client tools are absent, so a host
// that cannot run an external NBD client does not report a false pass.
func RequireNBDTools(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"nbdinfo", "nbdcopy"} {
		if _, err := exec.LookPath(bin); err != nil {
			// CI sets this so a runner image that loses libnbd fails rather
			// than dropping the only external witness to the export.
			if os.Getenv("SPINIFEX_REQUIRE_CONFORMANCE_TOOLS") != "" {
				t.Fatalf("%s not installed (libnbd-bin), but SPINIFEX_REQUIRE_CONFORMANCE_TOOLS is set", bin)
			}
			t.Skipf("%s not installed (libnbd-bin)", bin)
		}
	}
}

// nbdInfo runs nbdinfo against uri and returns its single export.
func nbdInfo(t *testing.T, uri string) nbdExport {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), "nbdinfo", "--json", uri).CombinedOutput()
	require.NoErrorf(t, err, "nbdinfo %s: %s", uri, out)
	t.Logf("nbdinfo %s:\n%s", uri, out)

	var parsed nbdInfoOutput
	require.NoError(t, json.Unmarshal(out, &parsed))
	require.Lenf(t, parsed.Exports, 1, "expected exactly one export from %s", uri)
	return parsed.Exports[0]
}

// nbdMap runs nbdinfo --map against uri and returns the allocation map the
// export reports.
func nbdMap(t *testing.T, uri string) []nbdExtent {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), "nbdinfo", "--map", "--json", uri).CombinedOutput()
	require.NoErrorf(t, err, "nbdinfo --map %s: %s", uri, out)
	t.Logf("nbdinfo --map %s:\n%s", uri, out)

	var extents []nbdExtent
	require.NoError(t, json.Unmarshal(out, &extents))
	return extents
}

// nbdInfoRetry retries nbdinfo until the export accepts it, reporting how
// many attempts and how long it took. Each attempt is a real connection, so
// polling with a cheaper probe would only re-trigger whatever refused it.
func nbdInfoRetry(t *testing.T, uri string) (nbdExport, int, time.Duration) {
	t.Helper()
	start := time.Now()
	deadline := start.Add(reconnectTimeout)

	var lastOut []byte
	for attempt := 1; ; attempt++ {
		out, err := exec.CommandContext(context.Background(), "nbdinfo", "--json", uri).CombinedOutput()
		if err == nil {
			var parsed nbdInfoOutput
			require.NoError(t, json.Unmarshal(out, &parsed))
			require.Lenf(t, parsed.Exports, 1, "expected exactly one export from %s", uri)
			return parsed.Exports[0], attempt, time.Since(start)
		}
		lastOut = out
		if time.Now().After(deadline) {
			require.FailNowf(t, "export never accepted a reconnection",
				"after %s and %d attempts: %s", reconnectTimeout, attempt, lastOut)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// nbdReachable reports whether nbdinfo can still connect, for asserting that
// an export has gone away.
func nbdReachable(uri string) bool {
	return exec.CommandContext(context.Background(), "nbdinfo", "--size", uri).Run() == nil
}

// publishForNBD creates and publishes a volume, registering cleanup so a run
// against a real cluster does not leave volumes behind.
func publishForNBD(t *testing.T, provider ebsprovider.EBSProvider, cfg NBDClientConfig, volumeID string, readOnly bool) *ebsprovider.PublishedVolume {
	t.Helper()
	ctx := context.Background()
	_, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{
		Versioned:     ebsprovider.NewVersioned(),
		VolumeID:      volumeID,
		CapacityRange: ebsprovider.CapacityRange{RequiredBytes: cfg.VolumeBytes},
	})
	require.NoError(t, err)
	t.Cleanup(func() { cleanupVolume(t, provider, cfg, volumeID) })

	pub, err := provider.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{
		Versioned: ebsprovider.NewVersioned(),
		VolumeID:  volumeID,
		NodeID:    cfg.NodeID,
		ReadOnly:  readOnly,
	})
	require.NoError(t, err)
	require.NotNil(t, pub)
	return pub
}

// cleanupVolume unpublishes and deletes best-effort: cleanup runs after the
// assertions and must never turn a passing test red.
func cleanupVolume(t *testing.T, provider ebsprovider.EBSProvider, cfg NBDClientConfig, volumeID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := provider.UnpublishVolume(ctx, ebsprovider.UnpublishVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, NodeID: cfg.NodeID,
	}); err != nil {
		t.Logf("cleanup: unpublish %s: %v", volumeID, err)
	}
	if err := provider.DeleteVolume(ctx, ebsprovider.DeleteVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID,
	}); err != nil {
		t.Logf("cleanup: delete %s: %v", volumeID, err)
	}
}

// RunNBDClientSuite drives a provider's published export with libnbd's
// client tools. newProvider must return a provider that serves real NBD;
// the in-memory reference implementation does not qualify.
func RunNBDClientSuite(t *testing.T, newProvider func(t *testing.T) ebsprovider.EBSProvider) {
	RunNBDClientSuiteWithConfig(t, newProvider, NBDClientConfig{})
}

// RunNBDClientSuiteWithConfig is RunNBDClientSuite against a named node with
// caller-chosen volume IDs, for driving a real daemon over a live cluster.
func RunNBDClientSuiteWithConfig(t *testing.T, newProvider func(t *testing.T) ebsprovider.EBSProvider, cfg NBDClientConfig) {
	RequireNBDTools(t)
	cfg = cfg.withDefaults()

	t.Run("published URI is a standard NBD URI", func(t *testing.T) {
		provider := newProvider(t)
		pub := publishForNBD(t, provider, cfg, cfg.VolumePrefix+"-uri", false)
		assertNBDURI(t, pub.NBDURI)
	})

	t.Run("export is connectable by a standard NBD client", func(t *testing.T) {
		provider := newProvider(t)
		pub := publishForNBD(t, provider, cfg, cfg.VolumePrefix+"-connect", false)
		export := nbdInfo(t, dialURI(t, pub.NBDURI))
		assert.Equal(t, cfg.VolumeBytes, export.Size)
	})

	t.Run("export advertises usable block size constraints", func(t *testing.T) {
		provider := newProvider(t)
		pub := publishForNBD(t, provider, cfg, cfg.VolumePrefix+"-blocksize", false)
		export := nbdInfo(t, dialURI(t, pub.NBDURI))

		require.Positivef(t, export.BlockSizeMinimum,
			"export advertises no minimum block size, so a client cannot know how to align its requests")
		assert.GreaterOrEqual(t, export.BlockSizePreferred, export.BlockSizeMinimum)
		assert.Zero(t, cfg.VolumeBytes%export.BlockSizeMinimum,
			"export size must be a whole number of minimum blocks")
	})

	// A guest that reboots, or any client that drops, reconnects to the same
	// export. Publication is what holds the volume, not any one connection.
	t.Run("export accepts a client reconnecting after the first disconnects", func(t *testing.T) {
		provider := newProvider(t)
		pub := publishForNBD(t, provider, cfg, cfg.VolumePrefix+"-reconnect", false)
		uri := dialURI(t, pub.NBDURI)

		first := nbdInfo(t, uri)

		second, attempts, elapsed := nbdInfoRetry(t, uri)
		t.Logf("reconnect accepted on attempt %d after %s", attempts, elapsed.Round(10*time.Millisecond))
		assert.Equalf(t, 1, attempts,
			"reconnect was refused for %s after the first client disconnected; a client that does not retry sees a hard failure", elapsed.Round(10*time.Millisecond))
		assert.Equal(t, first.Size, second.Size)
	})

	t.Run("data written by an external client reads back byte for byte", func(t *testing.T) {
		provider := newProvider(t)
		pub := publishForNBD(t, provider, cfg, cfg.VolumePrefix+"-roundtrip", false)

		dir := t.TempDir()
		in := filepath.Join(dir, "in.bin")
		out := filepath.Join(dir, "out.bin")
		want := patternBytes(int(cfg.VolumeBytes))
		require.NoError(t, os.WriteFile(in, want, 0o600))

		uri := dialURI(t, pub.NBDURI)
		runNBDCopy(t, in, uri)
		runNBDCopy(t, uri, out)

		got, err := os.ReadFile(out)
		require.NoError(t, err)
		require.Len(t, got, len(want))
		assert.True(t, bytes.Equal(got, want), "data read back over NBD differs from what was written")
	})

	// SparseExtentReporting is only observable at the export. base:allocation
	// being offered proves nothing on its own — nbdkit offers it whether or not
	// the plugin can compute extents, answering "all allocated" when it cannot.
	// What separates the two is the answer: an untouched volume is entirely
	// holes to a server that really tracks allocation.
	t.Run("sparse extent reporting matches the advertised capability", func(t *testing.T) {
		provider := newProvider(t)
		capabilities := capabilitiesOf(t, provider)
		pub := publishForNBD(t, provider, cfg, cfg.VolumePrefix+"-extents", false)

		holes := false
		for _, extent := range nbdMap(t, dialURI(t, pub.NBDURI)) {
			if strings.Contains(extent.Description, "hole") {
				holes = true
				break
			}
		}
		assert.Equalf(t, capabilities.SparseExtentReporting, holes,
			"SparseExtentReporting=%v but a volume with nothing written to it reports holes=%v",
			capabilities.SparseExtentReporting, holes)
	})

	t.Run("read-only publish matches the advertised capability", func(t *testing.T) {
		provider := newProvider(t)
		capabilities := capabilitiesOf(t, provider)

		if !capabilities.ReadOnlyPublish {
			// Refusing is the contract for a provider that cannot honour it;
			// handing back a writable export would be the real failure.
			_, err := provider.CreateVolume(context.Background(), ebsprovider.CreateVolumeRequest{
				Versioned:     ebsprovider.NewVersioned(),
				VolumeID:      cfg.VolumePrefix + "-readonly",
				CapacityRange: ebsprovider.CapacityRange{RequiredBytes: cfg.VolumeBytes},
			})
			require.NoError(t, err)
			t.Cleanup(func() { cleanupVolume(t, provider, cfg, cfg.VolumePrefix+"-readonly") })

			_, err = provider.PublishVolume(context.Background(), ebsprovider.PublishVolumeRequest{
				Versioned: ebsprovider.NewVersioned(), VolumeID: cfg.VolumePrefix + "-readonly",
				NodeID: cfg.NodeID, ReadOnly: true,
			})
			require.ErrorIs(t, err, ebsprovider.ErrUnsupportedCapability)
			return
		}

		pub := publishForNBD(t, provider, cfg, cfg.VolumePrefix+"-readonly", true)
		export := nbdInfo(t, dialURI(t, pub.NBDURI))
		assert.True(t, export.ReadOnly, "a volume published ReadOnly must set the NBD read-only transmission flag")
	})

	t.Run("unpublish tears the export down", func(t *testing.T) {
		provider := newProvider(t)
		volumeID := cfg.VolumePrefix + "-unpublish"
		pub := publishForNBD(t, provider, cfg, volumeID, false)
		uri := dialURI(t, pub.NBDURI)
		require.True(t, nbdReachable(uri), "export should be reachable before unpublish")

		require.NoError(t, provider.UnpublishVolume(context.Background(), ebsprovider.UnpublishVolumeRequest{
			Versioned: ebsprovider.NewVersioned(),
			VolumeID:  volumeID,
			NodeID:    cfg.NodeID,
		}))

		assert.Eventually(t, func() bool { return !nbdReachable(uri) }, 30*time.Second, 250*time.Millisecond,
			"export still answers after UnpublishVolume")
	})
}

// runNBDCopy copies between a local file and an NBD URI in either direction.
func runNBDCopy(t *testing.T, src, dst string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nbdcopy", src, dst).CombinedOutput()
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
