package handlers_ec2_image_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	handlers_ec2_image "github.com/mulgadc/spinifex/spinifex/handlers/ec2/image"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/testutil/recordingstore"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	byIDBucket  = "test-bucket"
	byIDAccount = "000000000001"
	byIDOther   = "000000000002"
	byIDImage   = "ami-target"
)

func putByIDAMI(t *testing.T, store objectstore.ObjectStore, ami ebsmetadata.AMI) {
	t.Helper()
	require.NoError(t, ebsmetadata.NewStore(store, byIDBucket).PutAMI(context.Background(), ami))
}

// byIDAMIDoc is a complete, readable AMI document owned by ownerAlias, which is
// an account ID for a private image and a name like "amazon" for a system one.
func byIDAMIDoc(imageID, ownerAlias string) ebsmetadata.AMI {
	return ebsmetadata.AMI{
		ImageID: imageID, Name: "debian-13", Description: "Debian 13",
		Architecture: "x86_64", PlatformDetails: "Linux/UNIX", CreationDate: time.Now().UTC(),
		RootDeviceType: "ebs", Virtualization: "hvm", ImageOwnerAlias: ownerAlias,
		VolumeSizeGiB: 8, State: "available", Tags: map[string]string{"Name": "base"},
	}
}

// seedByIDCatalog grows the AMI prefix with another account's images. The prefix
// is unpartitioned, so before the fast path these were fetched and decoded on
// every by-ID describe only to be discarded.
func seedByIDCatalog(t *testing.T, store objectstore.ObjectStore, n int) {
	t.Helper()
	for i := range n {
		putByIDAMI(t, store, byIDAMIDoc("ami-bulk"+strconv.Itoa(i), byIDOther))
	}
}

func amiKey(t *testing.T, imageID string) string {
	t.Helper()
	key, err := ebsmetadata.AMIKey(imageID)
	require.NoError(t, err)
	return key
}

// A named image costs one document read and no listing, however many images the
// cluster holds. The whole cluster's count was the old cost, so the operation
// count is the assertion, not the answer.
func TestDescribeImagesByID_ReadsOnlyTheNamedDocument(t *testing.T) {
	for _, catalog := range []int{1, 500} {
		t.Run("catalog"+strconv.Itoa(catalog), func(t *testing.T) {
			store := recordingstore.New()
			svc := handlers_ec2_image.NewImageServiceImplWithStore(store, byIDBucket)
			putByIDAMI(t, store, byIDAMIDoc(byIDImage, byIDAccount))
			seedByIDCatalog(t, store, catalog)

			out, err := svc.DescribeImages(t.Context(), &ec2.DescribeImagesInput{
				ImageIds: aws.StringSlice([]string{byIDImage}),
			}, byIDAccount)

			require.NoError(t, err)
			require.Len(t, out.Images, 1)
			assert.Equal(t, byIDImage, aws.StringValue(out.Images[0].ImageId))
			assert.Empty(t, store.ListPrefixes(), "a named image must not list the AMI prefix")
			assert.Equal(t, []string{amiKey(t, byIDImage)}, store.Gets(),
				"a named image must read its own document and nothing else")
		})
	}
}

// Naming the same image twice is one read and one result: the enumerating path
// matched each document once however often the request named it.
func TestDescribeImagesByID_MultipleAndDuplicateIDs(t *testing.T) {
	store := recordingstore.New()
	svc := handlers_ec2_image.NewImageServiceImplWithStore(store, byIDBucket)
	putByIDAMI(t, store, byIDAMIDoc("ami-a", byIDAccount))
	putByIDAMI(t, store, byIDAMIDoc("ami-b", byIDAccount))

	out, err := svc.DescribeImages(t.Context(), &ec2.DescribeImagesInput{
		ImageIds: aws.StringSlice([]string{"ami-a", "ami-b", "ami-a"}),
	}, byIDAccount)

	require.NoError(t, err)
	require.Len(t, out.Images, 2)
	assert.Equal(t, "ami-a", aws.StringValue(out.Images[0].ImageId))
	assert.Equal(t, "ami-b", aws.StringValue(out.Images[1].ImageId))
	assert.Len(t, store.Gets(), 2, "a duplicated ID must not be read twice")
	assert.Empty(t, store.ListPrefixes())
}

// The AMI key carries no tenant scoping, so the fast path reads a document it
// must not return. Reporting anything other than not-found here discloses
// another account's private image to a caller that named its ID.
func TestDescribeImagesByID_HiddenAndMissingAreIndistinguishable(t *testing.T) {
	tests := []struct {
		name       string
		seedOwner  string
		seedImage  bool
		requestID  string
		wantHidden string
	}{
		{name: "another account's private image", seedOwner: byIDOther, seedImage: true, requestID: byIDImage},
		{name: "an empty owner alias", seedOwner: "", seedImage: true, requestID: byIDImage},
		{name: "an image that does not exist", seedImage: false, requestID: "ami-never-registered"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := recordingstore.New()
			svc := handlers_ec2_image.NewImageServiceImplWithStore(store, byIDBucket)
			if tt.seedImage {
				putByIDAMI(t, store, byIDAMIDoc(byIDImage, tt.seedOwner))
			}

			out, err := svc.DescribeImages(t.Context(), &ec2.DescribeImagesInput{
				ImageIds: aws.StringSlice([]string{tt.requestID}),
			}, byIDAccount)

			require.Error(t, err)
			assert.Nil(t, out)
			assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error(),
				"an image the caller cannot read must answer exactly as a nonexistent one")
		})
	}
}

// A system AMI's alias is not an account ID, so it resolves to the global
// account before any owner comparison and stays visible to every tenant.
func TestDescribeImagesByID_SystemAMIIsVisibleToEveryAccount(t *testing.T) {
	store := recordingstore.New()
	svc := handlers_ec2_image.NewImageServiceImplWithStore(store, byIDBucket)
	putByIDAMI(t, store, byIDAMIDoc(byIDImage, "amazon"))

	out, err := svc.DescribeImages(t.Context(), &ec2.DescribeImagesInput{
		ImageIds: aws.StringSlice([]string{byIDImage}),
		Owners:   aws.StringSlice([]string{"amazon"}),
	}, byIDAccount)

	require.NoError(t, err)
	require.Len(t, out.Images, 1)
	assert.Equal(t, utils.GlobalAccountID, aws.StringValue(out.Images[0].OwnerId))
	assert.Equal(t, "amazon", aws.StringValue(out.Images[0].ImageOwnerAlias))
}

// The Owners parameter is matched against the resolved owner, and a named image
// that no owner selects is not-found rather than an empty success.
func TestDescribeImagesByID_OwnerFilters(t *testing.T) {
	tests := []struct {
		name       string
		ownerAlias string
		owners     []string
		wantErr    string
	}{
		{name: "self against an own image", ownerAlias: byIDAccount, owners: []string{"self"}},
		{name: "own numeric account", ownerAlias: byIDAccount, owners: []string{byIDAccount}},
		{name: "system aliases against a system image", ownerAlias: "amazon",
			owners: []string{"system", "aws-marketplace"}},
		{name: "another account against an own image", ownerAlias: byIDAccount, owners: []string{byIDOther},
			wantErr: awserrors.ErrorInvalidAMIIDNotFound},
		{name: "self against a system image", ownerAlias: "amazon", owners: []string{"self"},
			wantErr: awserrors.ErrorInvalidAMIIDNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := recordingstore.New()
			svc := handlers_ec2_image.NewImageServiceImplWithStore(store, byIDBucket)
			putByIDAMI(t, store, byIDAMIDoc(byIDImage, tt.ownerAlias))

			out, err := svc.DescribeImages(t.Context(), &ec2.DescribeImagesInput{
				ImageIds: aws.StringSlice([]string{byIDImage}),
				Owners:   aws.StringSlice(tt.owners),
			}, byIDAccount)

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tt.wantErr, err.Error())
				return
			}
			require.NoError(t, err)
			assert.Len(t, out.Images, 1)
		})
	}
}

// A filter applies to a directly-fetched document exactly as it did to a listed
// one, tags included, and a named image a filter excludes is not-found.
func TestDescribeImagesByID_AttributeFilters(t *testing.T) {
	tests := []struct {
		name        string
		filterName  string
		filterValue string
		wantErr     string
	}{
		{name: "matching name", filterName: "name", filterValue: "debian-13"},
		{name: "matching state", filterName: "state", filterValue: "available"},
		{name: "matching tag", filterName: "tag:Name", filterValue: "base"},
		{name: "non-matching name", filterName: "name", filterValue: "ubuntu-24",
			wantErr: awserrors.ErrorInvalidAMIIDNotFound},
		{name: "non-matching architecture", filterName: "architecture", filterValue: "arm64",
			wantErr: awserrors.ErrorInvalidAMIIDNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := recordingstore.New()
			svc := handlers_ec2_image.NewImageServiceImplWithStore(store, byIDBucket)
			putByIDAMI(t, store, byIDAMIDoc(byIDImage, byIDAccount))

			out, err := svc.DescribeImages(t.Context(), &ec2.DescribeImagesInput{
				ImageIds: aws.StringSlice([]string{byIDImage}),
				Filters: []*ec2.Filter{{
					Name:   aws.String(tt.filterName),
					Values: aws.StringSlice([]string{tt.filterValue}),
				}},
			}, byIDAccount)

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tt.wantErr, err.Error())
				return
			}
			require.NoError(t, err)
			assert.Len(t, out.Images, 1)
		})
	}
}

// The enumerating path skips a document it cannot decode so one bad AMI cannot
// hide every other image. A named image is never one of those others, so its
// unreadable document fails the call instead of reporting it as absent.
func TestDescribeImagesByID_CorruptDocumentFailsTheCall(t *testing.T) {
	store := recordingstore.New()
	svc := handlers_ec2_image.NewImageServiceImplWithStore(store, byIDBucket)
	_, err := store.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String(byIDBucket),
		Key:    aws.String(amiKey(t, byIDImage)),
		Body:   strings.NewReader("not-json"),
	})
	require.NoError(t, err)

	_, err = svc.DescribeImages(t.Context(), &ec2.DescribeImagesInput{
		ImageIds: aws.StringSlice([]string{byIDImage}),
	}, byIDAccount)

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error(),
		"a named image that cannot be decoded must not report as not-found")
}

// getFailingStore fails the read of one key with an error that is not a missing
// key, standing in for a store that is reachable but unwell.
type getFailingStore struct {
	*objectstore.MemoryObjectStore

	failKey string
}

func (s *getFailingStore) GetObject(ctx context.Context, input *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	if aws.StringValue(input.Key) == s.failKey {
		return nil, errors.New("connection reset by peer")
	}
	return s.MemoryObjectStore.GetObject(ctx, input)
}

// A store failure is not absence: reporting not-found here would tell a caller
// its image is deregistered on the strength of a broken connection.
func TestDescribeImagesByID_StoreErrorIsInternal(t *testing.T) {
	memory := objectstore.NewMemoryObjectStore()
	store := &getFailingStore{MemoryObjectStore: memory, failKey: amiKey(t, byIDImage)}
	svc := handlers_ec2_image.NewImageServiceImplWithStore(store, byIDBucket)
	putByIDAMI(t, memory, byIDAMIDoc(byIDImage, byIDAccount))

	_, err := svc.DescribeImages(t.Context(), &ec2.DescribeImagesInput{
		ImageIds: aws.StringSlice([]string{byIDImage}),
	}, byIDAccount)

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

// A store failure anywhere in the fan-out outranks a missing ID, whatever order
// the request named them in. Answering not-found here would tell a caller its
// image was deregistered on the strength of a broken connection.
func TestDescribeImagesByID_StoreErrorOutranksAMissingID(t *testing.T) {
	memory := objectstore.NewMemoryObjectStore()
	store := &getFailingStore{MemoryObjectStore: memory, failKey: amiKey(t, byIDImage)}
	svc := handlers_ec2_image.NewImageServiceImplWithStore(store, byIDBucket)
	putByIDAMI(t, memory, byIDAMIDoc(byIDImage, byIDAccount))

	_, err := svc.DescribeImages(t.Context(), &ec2.DescribeImagesInput{
		ImageIds: aws.StringSlice([]string{"ami-never-registered", byIDImage}),
	}, byIDAccount)

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

// A caller that has already given up gets an error, never an empty result that
// reads as "this image does not exist".
func TestDescribeImagesByID_CancelledContextFails(t *testing.T) {
	store := recordingstore.New()
	svc := handlers_ec2_image.NewImageServiceImplWithStore(store, byIDBucket)
	putByIDAMI(t, store, byIDAMIDoc(byIDImage, byIDAccount))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	out, err := svc.DescribeImages(ctx, &ec2.DescribeImagesInput{
		ImageIds: aws.StringSlice([]string{byIDImage}),
	}, byIDAccount)

	require.Error(t, err)
	assert.Nil(t, out)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

// A request whose only IDs are nil names nothing, which is an empty result
// rather than a missing image, and reads nothing to find that out.
func TestDescribeImagesByID_OnlyNilIDs(t *testing.T) {
	store := recordingstore.New()
	svc := handlers_ec2_image.NewImageServiceImplWithStore(store, byIDBucket)
	putByIDAMI(t, store, byIDAMIDoc(byIDImage, byIDAccount))

	out, err := svc.DescribeImages(t.Context(), &ec2.DescribeImagesInput{ImageIds: []*string{nil}}, byIDAccount)

	require.NoError(t, err)
	assert.Empty(t, out.Images)
	assert.Empty(t, store.Gets())
	assert.Empty(t, store.ListPrefixes())
}
