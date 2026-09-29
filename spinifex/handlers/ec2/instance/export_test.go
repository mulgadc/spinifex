package handlers_ec2_instance

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
)

// Unexported hooks and in-package fixtures the external test package needs.
const (
	TestRootAccount = testRootAccount
	TestRootAZ      = testRootAZ
)

// NewProviderRootVolumeTestService is the in-package provider fixture over an
// in-memory provider and object store, with the one-AMI loader.
func NewProviderRootVolumeTestService(t *testing.T) *InstanceServiceImpl {
	t.Helper()
	return providerRootVolumeService(t, ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}),
		rootVolumeAMILoader(), objectstore.NewMemoryObjectStore())
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
