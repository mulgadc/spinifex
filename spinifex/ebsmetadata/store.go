package ebsmetadata

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
)

// Store persists Spinifex-owned metadata documents in the existing object
// store. It intentionally knows nothing about provider state or VBState.
type Store struct {
	objects objectstore.ObjectStore
	bucket  string
}

func NewStore(objects objectstore.ObjectStore, bucket string) *Store {
	return &Store{objects: objects, bucket: bucket}
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

// GetVolume returns the volume's ebsmetadata document. A volume with no
// document does not exist as far as the control plane is concerned.
func (s *Store) GetVolume(ctx context.Context, volumeID string) (Volume, error) {
	key, err := VolumeKey(volumeID)
	if err != nil {
		return Volume{}, err
	}
	data, err := s.get(ctx, key)
	if err != nil {
		return Volume{}, err
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

// ListVolumes returns every ebsmetadata volume document, skipping any it
// cannot read or decode. One unreadable document must not make every volume in
// the cluster invisible, so the tolerance is deliberate and each skip is logged.
func (s *Store) ListVolumes(ctx context.Context) ([]Volume, error) {
	if s == nil || s.objects == nil {
		return nil, errors.New("metadata store is not configured")
	}
	return s.listVolumeDocuments(ctx, true)
}

// ListVolumesStrict is ListVolumes without the tolerance. It is for callers
// whose answer would be wrong rather than merely partial: a volume that failed
// to read is not evidence that no volume holds the resource being checked.
func (s *Store) ListVolumesStrict(ctx context.Context) ([]Volume, error) {
	if s == nil || s.objects == nil {
		return nil, errors.New("metadata store is not configured")
	}
	return s.listVolumeDocuments(ctx, false)
}

func (s *Store) listVolumeDocuments(ctx context.Context, skipCorrupt bool) ([]Volume, error) {
	return listDocuments(ctx, s, volumePrefix, "volume", UnmarshalVolume, skipCorrupt)
}

// ListAMIs returns every AMI document, skipping any it cannot decode. One
// corrupt document must not make every image in the cluster invisible, so the
// tolerance is deliberate — the skip is logged rather than silent.
func (s *Store) ListAMIs(ctx context.Context) ([]AMI, error) {
	if s == nil || s.objects == nil {
		return nil, errors.New("metadata store is not configured")
	}
	return s.listAMIDocuments(ctx, true)
}

// ListAMIsStrict is ListAMIs without the tolerance. It is for callers whose
// answer would be wrong rather than merely partial: a name-uniqueness check
// cannot rule out a name held by an AMI it failed to read.
func (s *Store) ListAMIsStrict(ctx context.Context) ([]AMI, error) {
	if s == nil || s.objects == nil {
		return nil, errors.New("metadata store is not configured")
	}
	return s.listAMIDocuments(ctx, false)
}

func (s *Store) listAMIDocuments(ctx context.Context, skipCorrupt bool) ([]AMI, error) {
	return listDocuments(ctx, s, amiPrefix, "AMI", UnmarshalAMI, skipCorrupt)
}

// Prefixes the metadata documents live under, one per document kind.
const (
	volumePrefix = "spinifex/ebsmetadata/v1/volumes/"
	amiPrefix    = "spinifex/ebsmetadata/v1/amis/"
)

// listDocuments reads and decodes every document under prefix.
//
// skipCorrupt tolerates a document that cannot be fetched as well as one that
// cannot be decoded: an object whose shards no longer join is exactly as
// unusable as one whose bytes will not parse, and either kind read wholesale
// as a failure turns a single bad document into a cluster-wide outage.
func listDocuments[T any](
	ctx context.Context,
	s *Store,
	prefix, kind string,
	unmarshal func([]byte) (T, error),
	skipCorrupt bool,
) ([]T, error) {
	result, err := s.objects.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket), Prefix: aws.String(prefix),
	})
	if err != nil {
		return nil, err
	}

	documents := make([]T, 0, len(result.Contents))
	for _, object := range result.Contents {
		if object.Key == nil {
			continue
		}
		data, err := s.get(ctx, *object.Key)
		if err != nil {
			if skipCorrupt {
				slog.WarnContext(ctx, "skipping unreadable "+kind+" document", "key", *object.Key, "err", err)
				continue
			}
			return nil, err
		}
		document, err := unmarshal(data)
		if err != nil {
			if skipCorrupt && errors.Is(err, ErrCorruptDocument) {
				slog.WarnContext(ctx, "skipping undecodable "+kind+" document", "key", *object.Key, "err", err)
				continue
			}
			return nil, err
		}
		documents = append(documents, document)
	}
	return documents, nil
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

// GetAMI returns the AMI's ebsmetadata document. An AMI with no document does
// not exist as far as the control plane is concerned.
func (s *Store) GetAMI(ctx context.Context, imageID string) (AMI, error) {
	key, err := AMIKey(imageID)
	if err != nil {
		return AMI{}, err
	}
	data, err := s.get(ctx, key)
	if err != nil {
		return AMI{}, err
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
