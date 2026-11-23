package instance

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
)

// Unexported hooks and in-package fixtures the external test package needs.
const (
	TestRootAccount = testRootAccount
	TestRootAZ      = testRootAZ
	TestRootBucket  = testRootBucket
)

// NewProviderRootVolumeTestService is the in-package provider fixture over an
// in-memory provider and object store, with the one-AMI loader.
func NewProviderRootVolumeTestService(t *testing.T) *InstanceServiceImpl {
	t.Helper()
	return providerRootVolumeService(t, ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}),
		rootVolumeAMILoader(), objectstore.NewMemoryObjectStore())
}

// NewProviderRootVolumeTestServiceWithStore is NewProviderRootVolumeTestService
// over store, so a real volume service can share the launch's metadata.
func NewProviderRootVolumeTestServiceWithStore(t *testing.T, store objectstore.ObjectStore) *InstanceServiceImpl {
	t.Helper()
	return providerRootVolumeService(t, ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}),
		rootVolumeAMILoader(), store)
}

func (s *InstanceServiceImpl) MetadataStore() *ebsmetadata.Store { return s.metadata }

func (s *InstanceServiceImpl) PrepareDataVolumes(ctx context.Context, input *ec2.RunInstancesInput, instance *vm.VM) ([]VolumeInfo, error) {
	return s.prepareDataVolumes(ctx, input, instance)
}

// DataVolumeParams mirrors dataVolumeParams with its fields exported.
type DataVolumeParams struct {
	DeviceName          string
	SizeGiB             int64
	Iops                int64
	DeleteOnTermination bool
}

func ParseDataVolumeParams(input *ec2.RunInstancesInput) []DataVolumeParams {
	var out []DataVolumeParams
	for _, p := range parseDataVolumeParams(input) {
		out = append(out, DataVolumeParams{
			DeviceName:          p.deviceName,
			SizeGiB:             p.sizeGiB,
			Iops:                p.iops,
			DeleteOnTermination: p.deleteOnTermination,
		})
	}
	return out
}
