package ebsmetadata

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

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

// ListVolumes returns all Spinifex-owned volume records in the store.
func (s *Store) ListVolumes(ctx context.Context) ([]Volume, error) {
	if s == nil || s.objects == nil {
		return nil, errors.New("metadata store is not configured")
	}
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

// ListAMIs returns all Spinifex-owned AMI records in the store.
func (s *Store) ListAMIs(ctx context.Context) ([]AMI, error) {
	if s == nil || s.objects == nil {
		return nil, errors.New("metadata store is not configured")
	}
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
