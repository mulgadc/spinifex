package admin

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
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
	documented, err := metadata.ListAllVolumesStrict(ctx)
	if err != nil {
		return nil, fmt.Errorf("orphan scan: list control-plane volumes: %w", err)
	}
	known := make(map[string]bool, len(documented))
	for _, volume := range documented {
		known[volume.VolumeID] = true
	}

	// An AMI's blocks live in a provider volume named after the image, and it
	// is recorded as an AMI document rather than a volume one. Without this
	// every registered image reads as an orphan.
	images, err := metadata.ListAMIsStrict(ctx)
	if err != nil {
		return nil, fmt.Errorf("orphan scan: list control-plane images: %w", err)
	}
	for _, image := range images {
		known[image.ImageID] = true
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

// UEFIStoreRecords is the read side of the UEFI store-record bucket.
type UEFIStoreRecords interface {
	InstanceIDs(ctx context.Context) ([]string, error)
	Get(ctx context.Context, instanceID string) (*uefistore.UEFIStoreRecord, uint64, error)
}

var _ UEFIStoreRecords = (*uefistore.Store)(nil)

// InstanceIndex reports every instance ID the control plane holds a record
// for, live or terminated. A record it cannot decode still counts.
type InstanceIndex interface {
	InstanceIDs(ctx context.Context) (map[string]bool, error)
}

// OrphanUEFIStoreRecord is a store record that is not deleting and whose
// instance has no record, so nothing will ever delete its objects.
type OrphanUEFIStoreRecord struct {
	InstanceID string
	AccountID  string
	State      string
}

// OrphanUEFIStores is what the object store and the store records hold for
// UEFI variable stores that nothing owns.
type OrphanUEFIStores struct {
	// InstancePrefixes are uefi/i-*/ prefixes with no instance record and no
	// store record.
	InstancePrefixes []string
	// ImagePrefixes are uefi/ami-*/ prefixes with no AMI document.
	ImagePrefixes []string
	// OtherPrefixes are prefixes named for neither an instance nor an image,
	// which no owner can ever claim.
	OtherPrefixes []string
	Records       []OrphanUEFIStoreRecord
}

// Empty reports whether the scan found nothing.
func (o *OrphanUEFIStores) Empty() bool {
	return len(o.InstancePrefixes) == 0 && len(o.ImagePrefixes) == 0 && len(o.OtherPrefixes) == 0 && len(o.Records) == 0
}

// FindOrphanUEFIStores reports UEFI variable stores that nothing owns. Like
// FindOrphanVolumes it never deletes: an absent instance record is not proof
// of a terminate, so settling each one is an operator's decision.
//
// Every read is strict. A record or document this cannot read aborts the scan
// rather than reading as absent, which would point an operator at live data.
func FindOrphanUEFIStores(ctx context.Context, objects objectstore.ObjectStore, bucket string,
	metadata *ebsmetadata.Store, stores UEFIStoreRecords, instances InstanceIndex) (*OrphanUEFIStores, error) {
	if objects == nil || bucket == "" || metadata == nil || stores == nil || instances == nil {
		return nil, errors.New("uefi orphan scan: object store, bucket, metadata, store records and instance index are all required")
	}

	known, err := instances.InstanceIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("uefi orphan scan: list instance records: %w", err)
	}
	images, err := metadata.ListAMIsStrict(ctx)
	if err != nil {
		return nil, fmt.Errorf("uefi orphan scan: list control-plane images: %w", err)
	}
	imageIDs := make(map[string]bool, len(images))
	for _, image := range images {
		imageIDs[image.ImageID] = true
	}

	out := &OrphanUEFIStores{}
	ids, err := stores.InstanceIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("uefi orphan scan: list store records: %w", err)
	}
	recorded := make(map[string]bool, len(ids))
	for _, id := range ids {
		recorded[id] = true
		rec, _, err := stores.Get(ctx, id)
		if errors.Is(err, kvstore.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("uefi orphan scan: read store record %s: %w", id, err)
		}
		if rec.State != uefistore.StateDeleting && !known[id] {
			out.Records = append(out.Records, OrphanUEFIStoreRecord{InstanceID: id, AccountID: rec.AccountID, State: rec.State})
		}
	}

	_, prefixes, err := objectstore.ListAll(ctx, objects, &s3.ListObjectsV2Input{
		Bucket:    aws.String(bucket),
		Prefix:    aws.String(uefistore.ObjectRoot),
		Delimiter: aws.String("/"),
	})
	if err != nil {
		return nil, fmt.Errorf("uefi orphan scan: list store objects: %w", err)
	}
	for _, p := range prefixes {
		prefix := aws.StringValue(p.Prefix)
		owner := strings.TrimSuffix(strings.TrimPrefix(prefix, uefistore.ObjectRoot), "/")
		switch {
		case strings.HasPrefix(owner, "i-"):
			if !known[owner] && !recorded[owner] {
				out.InstancePrefixes = append(out.InstancePrefixes, prefix)
			}
		case strings.HasPrefix(owner, "ami-"):
			if !imageIDs[owner] {
				out.ImagePrefixes = append(out.ImagePrefixes, prefix)
			}
		default:
			out.OtherPrefixes = append(out.OtherPrefixes, prefix)
		}
	}
	sort.Strings(out.InstancePrefixes)
	sort.Strings(out.ImagePrefixes)
	sort.Strings(out.OtherPrefixes)
	return out, nil
}
