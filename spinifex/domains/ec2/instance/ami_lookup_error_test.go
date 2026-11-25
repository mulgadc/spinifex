package instance

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	ebsmetadata "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	ec2image "github.com/mulgadc/spinifex/spinifex/domains/ec2/image"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const lookupTestAMI = "ami-0123456789abcdef0"

// amiReadFailingStore answers every GET of failKey with the error the S3
// client returns when predastore answers 500, as it did with its blob layer down.
type amiReadFailingStore struct {
	*objectstore.MemoryObjectStore

	failKey string
}

func (s *amiReadFailingStore) GetObject(ctx context.Context, input *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	if aws.StringValue(input.Key) == s.failKey {
		return nil, awserr.NewRequestFailure(
			awserr.New("InternalError", "We encountered an internal error. Please try again.", nil), 500, "req-1")
	}
	return s.MemoryObjectStore.GetObject(ctx, input)
}

// amiDoc is what the store holds at lookupTestAMI's key.
type amiDoc int

const (
	amiAbsent amiDoc = iota
	amiRegistered
	amiCorrupt
)

// amiLookupFixture is an image service over a store holding doc for
// lookupTestAMI. With failRead set, each read of the AMI's document fails as a
// predastore 500 does.
func amiLookupFixture(t *testing.T, doc amiDoc, failRead bool) *ec2image.ImageServiceImpl {
	t.Helper()
	ctx := context.Background()
	memory := objectstore.NewMemoryObjectStore()
	key, err := ebsmetadata.AMIKey(lookupTestAMI)
	require.NoError(t, err)
	switch doc {
	case amiAbsent:
	case amiRegistered:
		require.NoError(t, ebsmetadata.NewStore(memory, testRootBucket).PutAMI(ctx, ebsmetadata.AMI{
			ImageID:         lookupTestAMI,
			ImageOwnerAlias: "spinifex",
			SnapshotID:      "snap-" + lookupTestAMI,
			VolumeSizeGiB:   4,
			State:           ebsmetadata.AMIStateAvailable,
		}))
	case amiCorrupt:
		_, err := memory.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(testRootBucket),
			Key:    aws.String(key),
			Body:   strings.NewReader("{not json"),
		})
		require.NoError(t, err)
	}
	var store objectstore.ObjectStore = memory
	if failRead {
		store = &amiReadFailingStore{MemoryObjectStore: memory, failKey: key}
	}
	return ec2image.NewImageServiceImplWithStore(store, testRootBucket)
}

// The launch answers not-found only for an AMI whose document is absent. A
// failed read of an existing AMI is a retryable server error.
func TestPrepareRunInstances_AMILookupErrorClass(t *testing.T) {
	cases := []struct {
		name     string
		imageID  string
		doc      amiDoc
		failRead bool
		want     string
	}{
		{name: "absent", doc: amiAbsent, want: awserrors.ErrorInvalidAMIIDNotFound},
		{name: "read fails", doc: amiRegistered, failRead: true, want: awserrors.ErrorServerInternal},
		{name: "corrupt", doc: amiCorrupt, want: awserrors.ErrorServerInternal},
		{name: "unkeyable ID", imageID: "ami-a/b", doc: amiRegistered, want: awserrors.ErrorInvalidAMIIDMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			imageID := lookupTestAMI
			if tc.imageID != "" {
				imageID = tc.imageID
			}
			loader := amiLookupFixture(t, tc.doc, tc.failRead)
			types, _ := defaultPrepareInstanceTypes()
			svc := &InstanceServiceImpl{instanceTypes: types, amiLoader: loader}

			_, _, _, err := svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{
				InstanceType: aws.String("t3.micro"),
				ImageId:      aws.String(imageID),
			}, testRootAccount, "")

			require.Error(t, err)
			assert.Equal(t, tc.want, err.Error())
		})
	}
}
