package image

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	ec2snapshot "github.com/mulgadc/spinifex/spinifex/domains/ec2/snapshot"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// assertAWSError checks err carries code and message as AWS words them.
func assertAWSError(t *testing.T, err error, code, message string) {
	t.Helper()
	gotCode, gotMessage, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok, "unregistered error: %v", err)
	assert.Equal(t, code, gotCode)
	assert.Equal(t, message, gotMessage)
}

// renderedProtection reads the state DescribeImages and DescribeImageAttribute
// report, requiring they agree.
func renderedProtection(t *testing.T, svc *ImageServiceImpl, imageID string) string {
	t.Helper()
	ctx := context.Background()
	images, err := svc.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []*string{aws.String(imageID)}}, testAccountID)
	require.NoError(t, err)
	require.Len(t, images.Images, 1)
	attr, err := svc.DescribeImageAttribute(ctx, &ec2.DescribeImageAttributeInput{
		ImageId: aws.String(imageID), Attribute: aws.String(ec2.ImageAttributeNameDeregistrationProtection),
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, attr.DeregistrationProtection)
	require.Equal(t, aws.StringValue(images.Images[0].DeregistrationProtection), aws.StringValue(attr.DeregistrationProtection.Value))
	return aws.StringValue(attr.DeregistrationProtection.Value)
}

func TestImageDeregistrationProtection_Lifecycle(t *testing.T) {
	svc, store := setupTestImageService(t)
	ctx := context.Background()
	const imageID = "ami-0685a57e117077285"
	createTestAMIConfigWithOwner(t, store, imageID, "protected", testAccountID)

	assert.Equal(t, "disabled", renderedProtection(t, svc, imageID), "a new image is unprotected")

	for range 2 {
		out, err := svc.EnableImageDeregistrationProtection(ctx, &ec2.EnableImageDeregistrationProtectionInput{ImageId: aws.String(imageID)}, testAccountID)
		require.NoError(t, err)
		assert.Equal(t, "enabled-without-cooldown", aws.StringValue(out.Return))
		assert.Equal(t, "enabled-without-cooldown", renderedProtection(t, svc, imageID))
	}

	_, err := svc.DeregisterImage(ctx, &ec2.DeregisterImageInput{ImageId: aws.String(imageID)}, testAccountID)
	assertAWSError(t, err, awserrors.ErrorOperationNotPermitted,
		"ami-0685a57e117077285 can't be deregistered because deregistration protection is enabled-without-cooldown. Please disable protection and try again.")
	_, err = svc.GetAMIConfig(ctx, imageID)
	require.NoError(t, err, "a refused deregistration leaves the image")

	for range 2 {
		out, err := svc.DisableImageDeregistrationProtection(ctx, &ec2.DisableImageDeregistrationProtectionInput{ImageId: aws.String(imageID)}, testAccountID)
		require.NoError(t, err)
		assert.Equal(t, "disabled", aws.StringValue(out.Return))
		assert.Equal(t, "disabled", renderedProtection(t, svc, imageID))
	}

	_, err = svc.DeregisterImage(ctx, &ec2.DeregisterImageInput{ImageId: aws.String(imageID)}, testAccountID)
	require.NoError(t, err)
}

// A missing image, a system image and another account's image are all
// NotFound, never UnauthorizedOperation.
func TestImageDeregistrationProtection_ImageNotOwnedIsNotFound(t *testing.T) {
	svc, store := setupTestImageService(t)
	ctx := context.Background()
	createTestAMIConfigWithOwner(t, store, "ami-00000000000000001", "system", "spinifex")
	createTestAMIConfigWithOwner(t, store, "ami-00000000000000002", "other", "000000000002")

	for _, imageID := range []string{"ami-0123456789abcdef0", "ami-00000000000000001", "ami-00000000000000002"} {
		_, err := svc.EnableImageDeregistrationProtection(ctx, &ec2.EnableImageDeregistrationProtectionInput{ImageId: aws.String(imageID)}, testAccountID)
		assertAWSError(t, err, awserrors.ErrorInvalidAMIIDNotFound, "Invalid ImageId: "+imageID)
		_, err = svc.DisableImageDeregistrationProtection(ctx, &ec2.DisableImageDeregistrationProtectionInput{ImageId: aws.String(imageID)}, testAccountID)
		assertAWSError(t, err, awserrors.ErrorInvalidAMIIDNotFound, "Invalid ImageId: "+imageID)
	}

	other, err := svc.GetAMIConfig(ctx, "ami-00000000000000002")
	require.NoError(t, err)
	assert.Empty(t, other.DeregistrationProtection, "another account's image is not written")
}

// An ID no AMI has answers NotFound before it can reach the claim, whose key
// refuses characters such as '*'.
func TestDeregisterImage_OddIDIsNotFoundWithClaimsWired(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	svc, store := setupTestImageService(t)
	snaps := ec2snapshot.NewSnapshotServiceImplWithStore(&config.Config{Predastore: config.PredastoreConfig{Bucket: testBucket}}, store, nil)
	snaps.SetClaims(ec2snapshot.NewClaims(ec2snapshot.NewClaimGroup(kvstore.OpenSyncBucket(js), "node-a"), "node-a", nil))
	svc.SetImageClaims(snaps)

	_, err := svc.DeregisterImage(context.Background(), &ec2.DeregisterImageInput{ImageId: aws.String("ami-a*b")}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())
}
