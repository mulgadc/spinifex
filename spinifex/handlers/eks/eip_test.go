package handlers_eks

import (
	"context"
	"github.com/aws/aws-sdk-go/aws"
	ec2 "github.com/aws/aws-sdk-go/service/ec2"
)

// fakeEIPProvisioner records allocate/release calls and hands back a
// deterministic pool address so CreateCluster/DeleteCluster egress wiring is
// exercised without a live EIP service.
type fakeEIPProvisioner struct {
	allocateCalls []*ec2.AllocateAddressInput
	releaseCalls  []*ec2.ReleaseAddressInput

	publicIP     string
	allocationID string
	allocateErr  error
	releaseErr   error
}

var _ eipProvisioner = (*fakeEIPProvisioner)(nil)

func newFakeEIPProvisioner() *fakeEIPProvisioner {
	return &fakeEIPProvisioner{publicIP: "203.0.113.50", allocationID: "eipalloc-fake01"}
}

func (f *fakeEIPProvisioner) AllocateAddress(_ context.Context, input *ec2.AllocateAddressInput, _ string) (*ec2.AllocateAddressOutput, error) {
	f.allocateCalls = append(f.allocateCalls, input)
	if f.allocateErr != nil {
		return nil, f.allocateErr
	}
	return &ec2.AllocateAddressOutput{
		PublicIp:     aws.String(f.publicIP),
		AllocationId: aws.String(f.allocationID),
	}, nil
}

func (f *fakeEIPProvisioner) ReleaseAddress(_ context.Context, input *ec2.ReleaseAddressInput, _ string) (*ec2.ReleaseAddressOutput, error) {
	f.releaseCalls = append(f.releaseCalls, input)
	if f.releaseErr != nil {
		return nil, f.releaseErr
	}
	return &ec2.ReleaseAddressOutput{}, nil
}
