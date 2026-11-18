package conformance_test

import (
	"context"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs/conformance"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs/natsserve"
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

// TestMemoryProviderSharedInstanceConformance runs the suite twice against one
// provider instance, standing in for a live daemon whose volumes outlive the
// test binary. It passes only if the suite removes everything it creates: with
// no cleanup the second run collides with the first on already_exists.
func TestMemoryProviderSharedInstanceConformance(t *testing.T) {
	shared := ebsprovider.NewMemoryProvider(conformance.ReferenceCapabilities)
	newProvider := func(t *testing.T) ebsprovider.EBSProvider {
		t.Helper()
		return shared
	}

	for _, run := range []string{"first run", "second run"} {
		t.Run(run, func(t *testing.T) {
			conformance.RunSuiteWithConfig(t, newProvider, conformance.SuiteConfig{})
		})
	}
}

// TestMemoryProviderNamePrefixConformance covers the other half of running
// against a live provider: distinct prefixes let concurrent runs share one
// daemon without naming the same volumes.
func TestMemoryProviderNamePrefixConformance(t *testing.T) {
	shared := ebsprovider.NewMemoryProvider(conformance.ReferenceCapabilities)
	newProvider := func(t *testing.T) ebsprovider.EBSProvider {
		t.Helper()
		return shared
	}

	for _, prefix := range []string{"alpha-", "beta-"} {
		t.Run(prefix, func(t *testing.T) {
			conformance.RunSuiteWithConfig(t, newProvider, conformance.SuiteConfig{NamePrefix: prefix})
		})
	}
}

// TestMemoryProviderFenceNoticeConformance runs the suite with the revocation
// case enabled, so the contract's fence notice is held to the same check here
// as on viperblockd.
func TestMemoryProviderFenceNoticeConformance(t *testing.T) {
	_, conn := testutil.StartTestNATS(t)
	shared := ebsprovider.NewMemoryProvider(conformance.ReferenceCapabilities)
	shared.NotifyFencesOn(conn)

	conformance.RunSuiteWithConfig(t, func(t *testing.T) ebsprovider.EBSProvider {
		t.Helper()
		return shared
	}, conformance.SuiteConfig{
		RevokeExport: func(t *testing.T, published *ebsprovider.PublishedVolume) ebsprovider.FenceNotice {
			t.Helper()
			return conformance.CaptureFenceNotice(t, conn, published.NodeID, func() error {
				_, err := shared.RevokePublication(t.Context(), published.VolumeID, "node-elsewhere")
				return err
			})
		},
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

// settlingProvider is a MemoryProvider answering every CreateSnapshot pending.
// It settles a pending snapshot once a filtered read has seen it pending, so
// the suite's wait for completion is exercised and still ends.
type settlingProvider struct {
	*ebsprovider.MemoryProvider
}

var _ ebsprovider.EBSProvider = settlingProvider{}

func newSettlingProvider() settlingProvider {
	provider := ebsprovider.NewMemoryProvider(conformance.ReferenceCapabilities)
	provider.AnswerSnapshotsPending(true)
	return settlingProvider{MemoryProvider: provider}
}

func (p settlingProvider) ListSnapshots(ctx context.Context, req ebsprovider.ListSnapshotsRequest) (*ebsprovider.ListSnapshotsResponse, error) {
	resp, err := p.MemoryProvider.ListSnapshots(ctx, req)
	if err == nil && req.SnapshotID != "" && len(resp.Snapshots) == 1 && resp.Snapshots[0].State == ebsprovider.SnapshotStatePending {
		if err := p.SettleSnapshot(req.SnapshotID, ebsprovider.SnapshotStateCompleted); err != nil {
			return nil, err
		}
	}
	return resp, err
}

// TestMemoryProviderPendingSnapshotConformance runs the suite against a
// provider whose CreateSnapshot answers pending, as one whose backup outlives
// the request does, in process and over NATS.
func TestMemoryProviderPendingSnapshotConformance(t *testing.T) {
	t.Run("in process", func(t *testing.T) {
		conformance.RunSuite(t, func(t *testing.T) ebsprovider.EBSProvider {
			t.Helper()
			return newSettlingProvider()
		})
	})
	t.Run("over NATS", func(t *testing.T) {
		conformance.RunSuite(t, func(t *testing.T) ebsprovider.EBSProvider {
			t.Helper()
			_, conn := testutil.StartTestNATS(t)
			stop, err := natsserve.Serve(t.Context(), conn, newSettlingProvider(), natsserve.Options{})
			require.NoError(t, err)
			t.Cleanup(stop)
			return ebsprovider.NewNATSProvider(conn, 5*time.Second)
		})
	})
}
