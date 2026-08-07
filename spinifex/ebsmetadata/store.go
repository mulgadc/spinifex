package ebsmetadata

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
)

// LegacyVolumeReader converts the pre-migration on-disk state for volumeID
// into a Volume document. found=false with a nil error means no legacy record
// exists either — a genuinely unknown volume, not a decode failure.
//
// It is a function value rather than a method so the decode can live in a
// package that may import viperblock; this one must not.
type LegacyVolumeReader func(ctx context.Context, objects objectstore.ObjectStore, bucket, volumeID string) (Volume, bool, error)

// LegacyAMIReader is LegacyVolumeReader for AMIs.
type LegacyAMIReader func(ctx context.Context, objects objectstore.ObjectStore, bucket, imageID string) (AMI, bool, error)

// Store persists Spinifex-owned metadata documents in the existing object
// store. It intentionally knows nothing about provider state or VBState.
type Store struct {
	objects objectstore.ObjectStore
	bucket  string

	// Back the Get/List calls for resources predating the backfill migration.
	// nil (the default) turns the fallback off, which is how it is retired
	// once the migration has run everywhere.
	legacyVolume LegacyVolumeReader
	legacyAMI    LegacyAMIReader
}

func NewStore(objects objectstore.ObjectStore, bucket string) *Store {
	return &Store{objects: objects, bucket: bucket}
}

// SetLegacyVolumeFallback wires the legacy-state reader used by GetVolume and
// ListVolumes when a volume has no ebsmetadata document yet. Pass nil to turn
// the fallback off.
func (s *Store) SetLegacyVolumeFallback(fn LegacyVolumeReader) {
	s.legacyVolume = fn
}

// SetLegacyAMIFallback is SetLegacyVolumeFallback for GetAMI/ListAMIs.
func (s *Store) SetLegacyAMIFallback(fn LegacyAMIReader) {
	s.legacyAMI = fn
}

func (s *Store) PutVolume(ctx context.Context, volume Volume) error {
	key, err := VolumeKey(volume.VolumeID)
	if err != nil {
		return err
	}
	data, err := MarshalVolume(volume)
	if err != nil {
		return fmt.Errorf("marshal volume metadata: %w", err)
	}
	return s.put(ctx, key, data)
}

// GetVolume returns the volume's ebsmetadata document, or the legacy-state
// equivalent when no document exists yet and a fallback is configured. A read
// that falls back never writes the converted value back — that races the
// backfill migration — so a hot volume without a document is decoded on
// every read until the migration (or a later read after it) catches up.
func (s *Store) GetVolume(ctx context.Context, volumeID string) (Volume, error) {
	key, err := VolumeKey(volumeID)
	if err != nil {
		return Volume{}, err
	}
	data, err := s.get(ctx, key)
	if err != nil {
		if !objectstore.IsNoSuchKeyError(err) || s.legacyVolume == nil {
			return Volume{}, err
		}
		volume, found, legacyErr := s.legacyVolume(ctx, s.objects, s.bucket, volumeID)
		if legacyErr != nil {
			return Volume{}, legacyErr
		}
		if !found {
			return Volume{}, err
		}
		slog.DebugContext(ctx, "ebsmetadata: served volume from legacy fallback", "volumeId", volumeID)
		return volume, nil
	}
	return UnmarshalVolume(data)
}

func (s *Store) DeleteVolume(ctx context.Context, volumeID string) error {
	key, err := VolumeKey(volumeID)
	if err != nil {
		return err
	}
	_, err = s.objects.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	return err
}

// ListVolumes returns every ebsmetadata volume document, unioned with legacy
// volumes that have no document yet (skipped when no fallback is configured).
// The ebsmetadata document wins where a volume has both.
func (s *Store) ListVolumes(ctx context.Context) ([]Volume, error) {
	if s == nil || s.objects == nil {
		return nil, errors.New("metadata store is not configured")
	}
	volumes, err := s.listVolumeDocuments(ctx)
	if err != nil {
		return nil, err
	}
	if s.legacyVolume == nil {
		return volumes, nil
	}

	documented := make(map[string]bool, len(volumes))
	for _, v := range volumes {
		documented[v.VolumeID] = true
	}

	legacyIDs, err := LegacyPrefixIDs(ctx, s.objects, s.bucket, "vol-", "-efi", "-cloudinit")
	if err != nil {
		return nil, err
	}
	for _, id := range legacyIDs {
		if documented[id] {
			continue
		}
		volume, found, err := s.legacyVolume(ctx, s.objects, s.bucket, id)
		if err != nil {
			slog.DebugContext(ctx, "ebsmetadata: skipping unreadable legacy volume", "volumeId", id, "err", err)
			continue
		}
		if !found {
			continue
		}
		volumes = append(volumes, volume)
	}
	return volumes, nil
}

// listVolumeDocuments returns only the ebsmetadata volume documents, with no
// legacy union — the pre-migration behavior, reused by ListVolumes.
func (s *Store) listVolumeDocuments(ctx context.Context) ([]Volume, error) {
	result, err := s.objects.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket), Prefix: aws.String("spinifex/ebsmetadata/v1/volumes/"),
	})
	if err != nil {
		return nil, err
	}
	volumes := make([]Volume, 0, len(result.Contents))
	for _, object := range result.Contents {
		if object.Key == nil {
			continue
		}
		data, err := s.get(ctx, *object.Key)
		if err != nil {
			return nil, err
		}
		volume, err := UnmarshalVolume(data)
		if err != nil {
			return nil, err
		}
		volumes = append(volumes, volume)
	}
	return volumes, nil
}

// LegacyPrefixIDs lists top-level bucket prefixes starting with prefix,
// excluding any ending in one of excludeSuffixes (internal sub-volumes such
// as the EFI partition or cloud-init seed, which are never user resources).
//
// It pages to exhaustion deliberately: a caller that stops at the first page
// would report a partial fleet, and the backfill migration would stamp itself
// complete having converted only the volumes it happened to see.
func LegacyPrefixIDs(ctx context.Context, objects objectstore.ObjectStore, bucket, prefix string, excludeSuffixes ...string) ([]string, error) {
	var ids []string
	var continuationToken *string
	for {
		result, err := objects.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			Delimiter:         aws.String("/"),
			ContinuationToken: continuationToken,
		})
		if err != nil {
			return nil, err
		}
		ids = append(ids, matchingPrefixIDs(result.CommonPrefixes, prefix, excludeSuffixes)...)

		if !aws.BoolValue(result.IsTruncated) {
			return ids, nil
		}
		continuationToken = result.NextContinuationToken
	}
}

// matchingPrefixIDs strips the trailing slash from each common prefix and
// keeps those matching prefix without a suffix in excludeSuffixes.
func matchingPrefixIDs(commonPrefixes []*s3.CommonPrefix, prefix string, excludeSuffixes []string) []string {
	var ids []string
outer:
	for _, cp := range commonPrefixes {
		if cp.Prefix == nil {
			continue
		}
		id := strings.TrimSuffix(*cp.Prefix, "/")
		if !strings.HasPrefix(id, prefix) {
			continue
		}
		for _, suffix := range excludeSuffixes {
			if strings.HasSuffix(id, suffix) {
				continue outer
			}
		}
		ids = append(ids, id)
	}
	return ids
}

// ListAMIs returns every ebsmetadata AMI document, unioned with legacy AMIs
// that have no document yet (skipped when no fallback is configured). The
// ebsmetadata document wins where an AMI has both. An undecodable legacy AMI
// is skipped so one corrupt config cannot fail the whole listing.
func (s *Store) ListAMIs(ctx context.Context) ([]AMI, error) {
	return s.listAMIs(ctx, false)
}

// ListAMIsStrict is ListAMIs with no tolerance for an undecodable legacy AMI.
// A caller that decides something from the absence of an AMI must fail loudly
// rather than act on a listing it knows is short.
func (s *Store) ListAMIsStrict(ctx context.Context) ([]AMI, error) {
	return s.listAMIs(ctx, true)
}

func (s *Store) listAMIs(ctx context.Context, strict bool) ([]AMI, error) {
	if s == nil || s.objects == nil {
		return nil, errors.New("metadata store is not configured")
	}
	amis, err := s.listAMIDocuments(ctx)
	if err != nil {
		return nil, err
	}
	if s.legacyAMI == nil {
		return amis, nil
	}

	documented := make(map[string]bool, len(amis))
	for _, a := range amis {
		documented[a.ImageID] = true
	}

	legacyIDs, err := LegacyPrefixIDs(ctx, s.objects, s.bucket, "ami-")
	if err != nil {
		return nil, err
	}
	for _, id := range legacyIDs {
		if documented[id] {
			continue
		}
		ami, found, err := s.legacyAMI(ctx, s.objects, s.bucket, id)
		if err != nil {
			if strict {
				return nil, fmt.Errorf("read legacy AMI %s: %w", id, err)
			}
			// An ami- prefix whose legacy config exists but cannot be decoded is
			// a fault, not a legitimate skip: the image silently disappears from
			// DescribeImages, so it has to stay visible at the default level.
			slog.WarnContext(ctx, "ebsmetadata: skipping unreadable legacy AMI", "imageId", id, "err", err)
			continue
		}
		if !found {
			continue
		}
		amis = append(amis, ami)
	}
	return amis, nil
}

// listAMIDocuments returns only the ebsmetadata AMI documents, with no legacy
// union — the pre-migration behavior, reused by ListAMIs.
func (s *Store) listAMIDocuments(ctx context.Context) ([]AMI, error) {
	result, err := s.objects.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket), Prefix: aws.String("spinifex/ebsmetadata/v1/amis/"),
	})
	if err != nil {
		return nil, err
	}
	amis := make([]AMI, 0, len(result.Contents))
	for _, object := range result.Contents {
		if object.Key == nil {
			continue
		}
		data, err := s.get(ctx, *object.Key)
		if err != nil {
			return nil, err
		}
		ami, err := UnmarshalAMI(data)
		if err != nil {
			return nil, err
		}
		amis = append(amis, ami)
	}
	return amis, nil
}

func (s *Store) PutAMI(ctx context.Context, ami AMI) error {
	key, err := AMIKey(ami.ImageID)
	if err != nil {
		return err
	}
	data, err := MarshalAMI(ami)
	if err != nil {
		return fmt.Errorf("marshal AMI metadata: %w", err)
	}
	return s.put(ctx, key, data)
}

// GetAMI returns the AMI's ebsmetadata document, or the legacy-state
// equivalent when no document exists yet and a fallback is configured. See
// GetVolume for why a fallback read never writes the converted value back.
func (s *Store) GetAMI(ctx context.Context, imageID string) (AMI, error) {
	key, err := AMIKey(imageID)
	if err != nil {
		return AMI{}, err
	}
	data, err := s.get(ctx, key)
	if err != nil {
		if !objectstore.IsNoSuchKeyError(err) || s.legacyAMI == nil {
			return AMI{}, err
		}
		ami, found, legacyErr := s.legacyAMI(ctx, s.objects, s.bucket, imageID)
		if legacyErr != nil {
			return AMI{}, legacyErr
		}
		if !found {
			return AMI{}, err
		}
		slog.DebugContext(ctx, "ebsmetadata: served AMI from legacy fallback", "imageId", imageID)
		return ami, nil
	}
	return UnmarshalAMI(data)
}

func (s *Store) DeleteAMI(ctx context.Context, imageID string) error {
	key, err := AMIKey(imageID)
	if err != nil {
		return err
	}
	_, err = s.objects.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	return err
}

func (s *Store) put(ctx context.Context, key string, data []byte) error {
	if s == nil || s.objects == nil {
		return errors.New("metadata store is not configured")
	}
	_, err := s.objects.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), Body: bytes.NewReader(data),
		ContentType: aws.String("application/json"),
	})
	return err
}

func (s *Store) get(ctx context.Context, key string) ([]byte, error) {
	if s == nil || s.objects == nil {
		return nil, errors.New("metadata store is not configured")
	}
	result, err := s.objects.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return nil, err
	}
	defer result.Body.Close()
	return io.ReadAll(result.Body)
}
