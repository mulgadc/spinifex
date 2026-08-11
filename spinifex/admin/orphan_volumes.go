package admin

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
)

// efiVolumeSuffix marks a volume derived from another one. An EFI variable
// store is created alongside its root volume and deliberately gets no
// control-plane document, so it is only an orphan when its base is one too.
const efiVolumeSuffix = "-efi"

// OrphanVolume is a volume the provider holds that the control plane has no
// document for. Its blocks are consuming space with no API handle, so it can
// be neither described nor deleted through EC2.
type OrphanVolume struct {
	VolumeID string
	Handle   string

	// Derived marks a volume named after another one, such as an EFI variable
	// store. It is reported only when its base volume is also undocumented,
	// which means the pair was stranded together.
	Derived bool
}

// FindOrphanVolumes reports what storage holds that the control plane cannot
// see. It reads both sides independently: the provider for what exists, the
// metadata store for what is known, and returns the difference.
//
// It never deletes. An undocumented volume is the case where deletion is most
// tempting and most dangerous, because the evidence that it is still wanted is
// exactly the document that went missing.
func FindOrphanVolumes(ctx context.Context, provider ebsprovider.EBSProvider, metadata *ebsmetadata.Store) ([]OrphanVolume, error) {
	if provider == nil || metadata == nil {
		return nil, fmt.Errorf("orphan scan: provider and metadata store are both required")
	}

	capabilities, err := provider.GetCapabilities(ctx, ebsprovider.GetCapabilitiesRequest{Versioned: ebsprovider.NewVersioned()})
	if err != nil {
		return nil, fmt.Errorf("orphan scan: read provider capabilities: %w", err)
	}
	if !capabilities.Capabilities.VolumeEnumeration {
		return nil, fmt.Errorf("orphan scan: %w: provider does not enumerate volumes", ebsprovider.ErrUnsupportedCapability)
	}

	// Strict listing on purpose: a document this cannot decode must abort the
	// scan, never be treated as absent. Reporting a volume as orphaned because
	// its document was unreadable would point an operator at live data.
	documented, err := metadata.ListVolumes(ctx)
	if err != nil {
		return nil, fmt.Errorf("orphan scan: list control-plane volumes: %w", err)
	}
	known := make(map[string]bool, len(documented))
	for _, volume := range documented {
		known[volume.VolumeID] = true
	}

	held, err := listProviderVolumes(ctx, provider)
	if err != nil {
		return nil, err
	}

	var orphans []OrphanVolume
	for _, ref := range held {
		if known[ref.ID] {
			continue
		}
		base, derived := strings.CutSuffix(ref.ID, efiVolumeSuffix)
		if derived && known[base] {
			continue
		}
		orphans = append(orphans, OrphanVolume{VolumeID: ref.ID, Handle: ref.Handle, Derived: derived})
	}
	sort.Slice(orphans, func(i, j int) bool { return orphans[i].VolumeID < orphans[j].VolumeID })
	return orphans, nil
}

// listProviderVolumes drains every page the provider offers. A partial walk
// would under-report orphans, which is the one direction this must not fail
// in: a missed orphan is a volume nobody ever goes looking for again.
func listProviderVolumes(ctx context.Context, provider ebsprovider.EBSProvider) ([]ebsprovider.VolumeRef, error) {
	var refs []ebsprovider.VolumeRef
	var token string
	for {
		resp, err := provider.ListVolumes(ctx, ebsprovider.ListVolumesRequest{
			Versioned:     ebsprovider.NewVersioned(),
			StartingToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("orphan scan: list provider volumes: %w", err)
		}
		refs = append(refs, resp.Volumes...)
		if resp.NextToken == "" {
			return refs, nil
		}
		if resp.NextToken == token {
			return nil, fmt.Errorf("orphan scan: provider returned a non-advancing page token %q", token)
		}
		token = resp.NextToken
	}
}
