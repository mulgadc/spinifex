package conformance

import (
	"context"
	"fmt"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
)

// benchVolumeBytes is the size every benchmarked volume is created at. It is
// deliberately small: this suite prices control operations, and a provider
// that allocates lazily would otherwise be credited for doing less work.
const benchVolumeBytes = 1 << 30

// benchProvider is what a benchmark measures. Unlike the conformance suite it
// is built once for the whole run: constructing a provider per iteration would
// price the constructor, which is not a verb.
type benchProvider struct {
	provider ebsprovider.EBSProvider
	cfg      SuiteConfig
}

func (p benchProvider) createVolume(b *testing.B, volumeID string) {
	b.Helper()
	_, err := p.provider.CreateVolume(context.Background(), ebsprovider.CreateVolumeRequest{
		Versioned:     ebsprovider.NewVersioned(),
		VolumeID:      volumeID,
		CapacityRange: ebsprovider.CapacityRange{RequiredBytes: benchVolumeBytes},
	})
	if err != nil {
		b.Fatalf("create %s: %v", volumeID, err)
	}
}

func (p benchProvider) deleteVolume(b *testing.B, volumeID string) {
	b.Helper()
	if err := p.provider.DeleteVolume(context.Background(), ebsprovider.DeleteVolumeRequest{
		Versioned: ebsprovider.NewVersioned(),
		VolumeID:  volumeID,
	}); err != nil {
		b.Fatalf("delete %s: %v", volumeID, err)
	}
}

// RunBenchSuite prices one round trip per verb. Every implementation is driven
// through the same calls in the same order, so two providers' numbers are
// comparable and the only difference is what answers.
//
// It measures control operations. Guest I/O never crosses these verbs, so no
// number here says anything about data-path throughput.
func RunBenchSuite(b *testing.B, provider ebsprovider.EBSProvider, cfg SuiteConfig) {
	p := benchProvider{provider: provider, cfg: cfg}
	capabilities := benchCapabilities(b, provider)

	b.Run("GetCapabilities", func(b *testing.B) {
		for b.Loop() {
			if _, err := provider.GetCapabilities(context.Background(), ebsprovider.GetCapabilitiesRequest{
				Versioned: ebsprovider.NewVersioned(),
			}); err != nil {
				b.Fatal(err)
			}
		}
	})

	// Create and delete are measured as a pair rather than separately: a
	// benchmark that only created would leave b.N volumes behind on a live
	// provider, and one that only deleted would have nothing to delete.
	b.Run("CreateDeleteVolume", func(b *testing.B) {
		i := 0
		for b.Loop() {
			volumeID := cfg.id(fmt.Sprintf("vol-bench%08d", i))
			p.createVolume(b, volumeID)
			p.deleteVolume(b, volumeID)
			i++
		}
	})

	b.Run("GetVolume", func(b *testing.B) {
		volumeID := cfg.id("vol-benchdescribe")
		p.createVolume(b, volumeID)
		b.Cleanup(func() { p.deleteVolume(b, volumeID) })
		b.ResetTimer()
		for b.Loop() {
			if _, err := provider.GetVolume(context.Background(), ebsprovider.GetVolumeRequest{
				Versioned: ebsprovider.NewVersioned(),
				VolumeID:  volumeID,
			}); err != nil {
				b.Fatal(err)
			}
		}
	})

	if capabilities.VolumeEnumeration {
		b.Run("ListVolumes", func(b *testing.B) {
			for b.Loop() {
				if _, err := provider.ListVolumes(context.Background(), ebsprovider.ListVolumesRequest{
					Versioned: ebsprovider.NewVersioned(),
				}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}

	// Publish and unpublish are the attach path, and the pair is what an
	// instance launch actually costs. Measuring publish alone would leave the
	// volume attached and the next iteration would fail on it.
	b.Run("PublishUnpublishVolume", func(b *testing.B) {
		volumeID := cfg.id("vol-benchpublish0")
		p.createVolume(b, volumeID)
		b.Cleanup(func() { p.deleteVolume(b, volumeID) })
		b.ResetTimer()
		for b.Loop() {
			if _, err := provider.PublishVolume(context.Background(), ebsprovider.PublishVolumeRequest{
				Versioned: ebsprovider.NewVersioned(),
				VolumeID:  volumeID,
				NodeID:    cfg.node(),
			}); err != nil {
				b.Fatal(err)
			}
			if err := provider.UnpublishVolume(context.Background(), ebsprovider.UnpublishVolumeRequest{
				Versioned: ebsprovider.NewVersioned(),
				VolumeID:  volumeID,
				NodeID:    cfg.node(),
			}); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("CreateDeleteSnapshot", func(b *testing.B) {
		volumeID := cfg.id("vol-benchsnapshot")
		p.createVolume(b, volumeID)
		b.Cleanup(func() { p.deleteVolume(b, volumeID) })
		b.ResetTimer()
		i := 0
		for b.Loop() {
			snapshotID := cfg.id(fmt.Sprintf("snap-bench%08d", i))
			if _, err := provider.CreateSnapshot(context.Background(), ebsprovider.CreateSnapshotRequest{
				Versioned:  ebsprovider.NewVersioned(),
				VolumeID:   volumeID,
				SnapshotID: snapshotID,
			}); err != nil {
				b.Fatal(err)
			}
			if err := provider.DeleteSnapshot(context.Background(), ebsprovider.DeleteSnapshotRequest{
				Versioned:  ebsprovider.NewVersioned(),
				SnapshotID: snapshotID,
			}); err != nil {
				b.Fatal(err)
			}
			i++
		}
	})
}

func benchCapabilities(b *testing.B, provider ebsprovider.EBSProvider) ebsprovider.Capabilities {
	b.Helper()
	resp, err := provider.GetCapabilities(context.Background(), ebsprovider.GetCapabilitiesRequest{
		Versioned: ebsprovider.NewVersioned(),
	})
	if err != nil {
		b.Fatalf("get capabilities: %v", err)
	}
	return resp.Capabilities
}
