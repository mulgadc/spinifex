package image

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const deprecationImageID = "ami-0d3e1f2a3b4c5d6e7"

func enableDeprecation(t *testing.T, svc *ImageServiceImpl, imageID string, at time.Time) error {
	t.Helper()
	out, err := svc.EnableImageDeprecation(context.Background(), &ec2.EnableImageDeprecationInput{
		ImageId: aws.String(imageID), DeprecateAt: aws.Time(at),
	}, testAccountID)
	if err == nil {
		assert.True(t, aws.BoolValue(out.Return))
	}
	return err
}

func describeDeprecationTime(t *testing.T, svc *ImageServiceImpl, imageID string) *string {
	t.Helper()
	out, err := svc.DescribeImages(context.Background(), &ec2.DescribeImagesInput{ImageIds: []*string{aws.String(imageID)}}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Images, 1)
	return out.Images[0].DeprecationTime
}

// AWS stores the time rounded to the nearest minute and renders it with
// milliseconds; enabling again replaces it.
func TestEnableImageDeprecation_RoundsToTheMinuteAndReplaces(t *testing.T) {
	svc, store := setupTestImageService(t)
	createTestAMIConfigWithOwner(t, store, deprecationImageID, "deprecating", testAccountID)
	base := time.Now().UTC().AddDate(1, 0, 0).Truncate(time.Minute)

	require.NoError(t, enableDeprecation(t, svc, deprecationImageID, base.Add(44*time.Second)))
	assert.Equal(t, base.Add(time.Minute).Format("2006-01-02T15:04:05.000Z"), aws.StringValue(describeDeprecationTime(t, svc, deprecationImageID)))

	require.NoError(t, enableDeprecation(t, svc, deprecationImageID, base.Add(time.Hour+29*time.Second+900*time.Millisecond)))
	assert.Equal(t, base.Add(time.Hour).Format("2006-01-02T15:04:05.000Z"), aws.StringValue(describeDeprecationTime(t, svc, deprecationImageID)))
}

func TestEnableImageDeprecation_RefusesTimesOutsideTheWindow(t *testing.T) {
	svc, store := setupTestImageService(t)
	createTestAMIConfigWithOwner(t, store, deprecationImageID, "deprecating", testAccountID)
	now := time.Now()

	tests := []struct {
		name    string
		at      time.Time
		wantMsg string
	}{
		{"past", now.Add(-time.Minute), "EnableImageDeprecation failed. Caused by specified deprecation time in the past"},
		{"beyond ten years", now.AddDate(10, 0, 1), "EnableImageDeprecation failed. Caused by specified deprecation time that is above ten-year limit"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, msg, _ := awserrors.ResolveErrorDetail(enableDeprecation(t, svc, deprecationImageID, tc.at))
			assert.Equal(t, awserrors.ErrorInvalidParameterValue, code)
			assert.Equal(t, tc.wantMsg, msg)
		})
	}
	assert.Nil(t, describeDeprecationTime(t, svc, deprecationImageID))

	require.NoError(t, enableDeprecation(t, svc, deprecationImageID, now.Add(90*time.Second)))
	require.NoError(t, enableDeprecation(t, svc, deprecationImageID, now.AddDate(9, 0, 0)))
}

// Disabling clears the time, and disabling an image with none still succeeds.
func TestDisableImageDeprecation_ClearsAndIsRepeatable(t *testing.T) {
	svc, store := setupTestImageService(t)
	createTestAMIConfigWithOwner(t, store, deprecationImageID, "deprecating", testAccountID)
	require.NoError(t, enableDeprecation(t, svc, deprecationImageID, time.Now().AddDate(1, 0, 0)))

	for range 2 {
		out, err := svc.DisableImageDeprecation(context.Background(), &ec2.DisableImageDeprecationInput{ImageId: aws.String(deprecationImageID)}, testAccountID)
		require.NoError(t, err)
		assert.True(t, aws.BoolValue(out.Return))
		assert.Nil(t, describeDeprecationTime(t, svc, deprecationImageID))
	}
}

// AWS answers NotFound for an image the caller cannot change, including a
// public Amazon image, as for one that does not exist.
func TestImageDeprecation_UnownedImageIsNotFound(t *testing.T) {
	svc, store := setupTestImageService(t)
	other := "ami-0a1b2c3d4e5f60718"
	system := "ami-0f1e2d3c4b5a69788"
	createTestAMIConfigWithOwner(t, store, other, "other", "000000000002")
	createTestAMIConfigWithOwner(t, store, system, "system", SystemOwnerAlias)

	for _, id := range []string{other, system, "ami-0123456789abcdef0"} {
		want := "Invalid ImageId: " + id
		code, msg, _ := awserrors.ResolveErrorDetail(enableDeprecation(t, svc, id, time.Now().AddDate(1, 0, 0)))
		assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, code, id)
		assert.Equal(t, want, msg, id)

		_, err := svc.DisableImageDeprecation(context.Background(), &ec2.DisableImageDeprecationInput{ImageId: aws.String(id)}, testAccountID)
		code, msg, _ = awserrors.ResolveErrorDetail(err)
		assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, code, id)
		assert.Equal(t, want, msg, id)
	}
}

// A document written before the field existed decodes and renders no
// DeprecationTime.
func TestDescribeImages_DocumentWithoutDeprecationTime(t *testing.T) {
	svc, store := setupTestImageService(t)
	putRawAMIDocument(t, store, deprecationImageID, `{"schema_version":2,"image_id":"`+deprecationImageID+
		`","name":"legacy","architecture":"x86_64","platform_details":"Linux/UNIX","root_device_type":"ebs","virtualization":"hvm","image_owner_alias":"`+testAccountID+`","volume_size_gib":8}`)

	assert.Nil(t, describeDeprecationTime(t, svc, deprecationImageID))
}
