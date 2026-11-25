package instance

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	ebsmetadata "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	ec2image "github.com/mulgadc/spinifex/spinifex/domains/ec2/image"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
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

// amiLookupFixture is an image service over a store holding lookupTestAMI. With
// failRead set, every read of the AMI's document fails as a predastore 500 does.
func amiLookupFixture(t *testing.T, registered, failRead bool) (*ec2image.ImageServiceImpl, objectstore.ObjectStore) {
	t.Helper()
	memory := objectstore.NewMemoryObjectStore()
	if registered {
		require.NoError(t, ebsmetadata.NewStore(memory, testRootBucket).PutAMI(context.Background(), ebsmetadata.AMI{
			ImageID:         lookupTestAMI,
			ImageOwnerAlias: "spinifex",
			SnapshotID:      "snap-" + lookupTestAMI,
			VolumeSizeGiB:   4,
			State:           ebsmetadata.AMIStateAvailable,
		}))
	}
	var store objectstore.ObjectStore = memory
	if failRead {
		key, err := ebsmetadata.AMIKey(lookupTestAMI)
		require.NoError(t, err)
		store = &amiReadFailingStore{MemoryObjectStore: memory, failKey: key}
	}
	return ec2image.NewImageServiceImplWithStore(store, testRootBucket), store
}

// The launch answers not-found only for an AMI whose document is absent. A
// failed read of an existing AMI is a retryable server error.
func TestPrepareRunInstances_AMILookupErrorClass(t *testing.T) {
	cases := []struct {
		name       string
		registered bool
		failRead   bool
		want       string
	}{
		{name: "absent", registered: false, want: awserrors.ErrorInvalidAMIIDNotFound},
		{name: "read fails", registered: true, failRead: true, want: awserrors.ErrorServerInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loader, _ := amiLookupFixture(t, tc.registered, tc.failRead)
			types, _ := defaultPrepareInstanceTypes()
			svc := &InstanceServiceImpl{instanceTypes: types, amiLoader: loader}

			_, _, _, err := svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{
				InstanceType: aws.String("t3.micro"),
				ImageId:      aws.String(lookupTestAMI),
			}, testRootAccount, "")

			require.Error(t, err)
			assert.Equal(t, tc.want, err.Error())
		})
	}
}

// The node re-reads the AMI to clone its root volume, and answers the same
// classes as the prepare step.
func TestPrepareRootVolume_Provider_AMILookupErrorClass(t *testing.T) {
	defer goleak.VerifyNone(t)

	cases := []struct {
		name       string
		registered bool
		failRead   bool
		want       string
	}{
		{name: "absent", registered: false, want: awserrors.ErrorInvalidAMIIDNotFound},
		{name: "read fails", registered: true, failRead: true, want: awserrors.ErrorServerInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loader, store := amiLookupFixture(t, tc.registered, tc.failRead)
			svc := providerRootVolumeService(t, ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}), loader, store)

			instance := &vm.VM{AccountID: testRootAccount}
			err := svc.prepareRootVolume(context.Background(), &ec2.RunInstancesInput{ImageId: aws.String(lookupTestAMI)},
				"vol-root", ebsmetadata.RootDeviceName, testRootVolumeBytes, 0, instance, true)

			require.Error(t, err)
			assert.Equal(t, tc.want, err.Error())
			assert.Empty(t, instance.EBSRequests.Requests, "a failed launch must not claim a boot volume")
		})
	}
}
