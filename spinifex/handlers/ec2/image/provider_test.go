package handlers_ec2_image

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	awss3 "github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupProviderImageService creates an image service with the EBS provider
// injected, so GetAMIConfig/putAMIConfig/DescribeImages take the provider
// branch instead of the legacy embedded path.
func setupProviderImageService(t *testing.T) (*ImageServiceImpl, *objectstore.MemoryObjectStore) {
	svc, store := setupTestImageService(t)
	svc.SetEBSProvider(ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}))
	return svc, store
}

// TestGetAMIConfig_Provider_ReturnsDocument locks that under the provider,
// GetAMIConfig reads the ebsmetadata document rather than legacy config.json.
func TestGetAMIConfig_Provider_ReturnsDocument(t *testing.T) {
	svc, _ := setupProviderImageService(t)

	ami := ebsmetadata.AMI{
		ImageID: "ami-doc001", Name: "provider-ami", Architecture: "x86_64",
		PlatformDetails: "Linux/UNIX", Virtualization: "hvm", RootDeviceType: "ebs",
		VolumeSizeGiB: 12, ImageOwnerAlias: testAccountID,
	}
	require.NoError(t, svc.MetadataStore().PutAMI(context.Background(), ami))

	got, err := svc.GetAMIConfig(context.Background(), "ami-doc001")
	require.NoError(t, err)
	assert.Equal(t, ami.Name, got.Name)
	assert.Equal(t, ami.VolumeSizeGiB, got.VolumeSizeGiB)
	assert.Equal(t, ami.ImageOwnerAlias, got.ImageOwnerAlias)
}

// TestGetAMIConfig_Embedded_ReturnsConvertedLegacyValue locks that the
// embedded path still reads legacy config.json, converted into the same
// ebsmetadata.AMI shape the provider path returns.
func TestGetAMIConfig_Embedded_ReturnsConvertedLegacyValue(t *testing.T) {
	svc, store := setupTestImageService(t)
	createTestAMIConfigWithName(t, store, "ami-legacy001", "legacy-ami")

	got, err := svc.GetAMIConfig(context.Background(), "ami-legacy001")
	require.NoError(t, err)
	assert.Equal(t, "legacy-ami", got.Name)
	assert.Equal(t, testAccountID, got.ImageOwnerAlias)
	assert.Equal(t, uint64(8), got.VolumeSizeGiB)
}

// TestPutAMIConfig_Provider_WritesDocumentNotConfigJSON locks the same
// single-source-of-truth contract as
// TestUpdateVolumeState_WritesStateJSONNotConfig in the volume package: under
// the provider, AMI metadata lives only in the ebsmetadata document, never in
// {imageID}/config.json.
func TestPutAMIConfig_Provider_WritesDocumentNotConfigJSON(t *testing.T) {
	svc, store := setupProviderImageService(t)

	meta := ebsmetadata.AMI{
		ImageID: "ami-nocfg001", Name: "provider-write", Architecture: "x86_64",
		PlatformDetails: "Linux/UNIX", Virtualization: "hvm", RootDeviceType: "ebs",
		VolumeSizeGiB: 10, ImageOwnerAlias: testAccountID,
	}
	require.NoError(t, svc.putAMIConfig(context.Background(), meta.ImageID, meta))

	_, err := store.GetObject(context.Background(), &awss3.GetObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(meta.ImageID + "/config.json"),
	})
	require.Error(t, err)
	assert.True(t, objectstore.IsNoSuchKeyError(err), "putAMIConfig must not write config.json under the provider")

	got, err := svc.GetAMIConfig(context.Background(), meta.ImageID)
	require.NoError(t, err)
	assert.Equal(t, meta.Name, got.Name)
}

// TestDescribeImages_Provider_ListsViaListAMIs_DocumentOnlyAMI locks that
// DescribeImages under the provider enumerates via Store.ListAMIs and
// surfaces an AMI that exists only as an ebsmetadata document.
func TestDescribeImages_Provider_ListsViaListAMIs_DocumentOnlyAMI(t *testing.T) {
	svc, _ := setupProviderImageService(t)

	ami := ebsmetadata.AMI{
		ImageID: "ami-listonly001", Name: "list-only", Architecture: "x86_64",
		PlatformDetails: "Linux/UNIX", Virtualization: "hvm", RootDeviceType: "ebs",
		VolumeSizeGiB: 8, ImageOwnerAlias: testAccountID, CreationDate: time.Now(), State: "available",
	}
	require.NoError(t, svc.MetadataStore().PutAMI(context.Background(), ami))

	out, err := svc.DescribeImages(context.Background(), &ec2.DescribeImagesInput{}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Images, 1, "an AMI that exists only as an ebsmetadata document must be visible under the provider")
	assert.Equal(t, "ami-listonly001", aws.StringValue(out.Images[0].ImageId))
	assert.Equal(t, "list-only", aws.StringValue(out.Images[0].Name))
}
