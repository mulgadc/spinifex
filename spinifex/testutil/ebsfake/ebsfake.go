package ebsfake

import (
	"context"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
)

// Provider is ebsprovider.MemoryProvider plus the object-prefix delete
// viperblockd performs on ebs.provider.v1.volume.delete. Without it a deleted
// volume's legacy config.json survives in the test store and the ebsmetadata
// legacy fallback resurrects the volume.
type Provider struct {
	ebsprovider.EBSProvider

	store  objectstore.ObjectStore
	bucket string
}

var _ ebsprovider.EBSProvider = (*Provider)(nil)

// New returns an in-memory EBS provider backed by store.
func New(store objectstore.ObjectStore, bucket string) *Provider {
	return &Provider{
		EBSProvider: ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}),
		store:       store, bucket: bucket,
	}
}

func (p *Provider) DeleteVolume(ctx context.Context, req ebsprovider.DeleteVolumeRequest) error {
	if err := p.EBSProvider.DeleteVolume(ctx, req); err != nil {
		return err
	}
	for _, prefix := range []string{req.VolumeID + "-efi/", req.VolumeID + "/"} {
		listed, err := p.store.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket: aws.String(p.bucket), Prefix: aws.String(prefix),
		})
		if err != nil {
			return err
		}
		for _, obj := range listed.Contents {
			if _, err := p.store.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(p.bucket), Key: obj.Key}); err != nil {
				return err
			}
		}
	}
	return nil
}
