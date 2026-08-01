package handlers_eks

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/require"
)

// fakeCSIVolumeReclaimer hands back a fixed DescribeVolumes result so
// reclaimCSIVolumes is exercised without a live EC2 volume service.
type fakeCSIVolumeReclaimer struct {
	out *ec2.DescribeVolumesOutput
	err error
}

var _ csiVolumeReclaimer = (*fakeCSIVolumeReclaimer)(nil)

func (f *fakeCSIVolumeReclaimer) DescribeVolumes(_ context.Context, _ *ec2.DescribeVolumesInput, _ string) (*ec2.DescribeVolumesOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.out, nil
}

func tagVol(id string, tags map[string]string) *ec2.Volume {
	v := &ec2.Volume{VolumeId: aws.String(id), State: aws.String("available")}
	for k, val := range tags {
		v.Tags = append(v.Tags, &ec2.Tag{Key: aws.String(k), Value: aws.String(val)})
	}
	return v
}

func TestReclaimCSIVolumes_ReportsOwnedVolumes(t *testing.T) {
	fake := &fakeCSIVolumeReclaimer{out: &ec2.DescribeVolumesOutput{Volumes: []*ec2.Volume{
		tagVol("vol-owned1", map[string]string{
			"kubernetes.io/cluster/test-cluster":      "owned",
			"kubernetes.io/created-for/pvc/namespace": "default",
			"kubernetes.io/created-for/pvc/name":      "data-pvc",
		}),
	}}}
	svc := &EKSServiceImpl{deps: EKSServiceDeps{Volume: fake}}

	n, err := svc.reclaimCSIVolumes(context.Background(), "acct1", "test-cluster")
	require.NoError(t, err)
	require.Equal(t, 1, n, "the cluster-owner-tagged volume must be reported")
}

func TestReclaimCSIVolumes_IgnoresUnrelatedVolumes(t *testing.T) {
	fake := &fakeCSIVolumeReclaimer{out: &ec2.DescribeVolumesOutput{Volumes: []*ec2.Volume{
		tagVol("vol-other-cluster", map[string]string{
			"kubernetes.io/cluster/some-other-cluster": "owned",
		}),
		tagVol("vol-no-csi-tag", map[string]string{
			"Name": "unrelated-volume",
		}),
	}}}
	svc := &EKSServiceImpl{deps: EKSServiceDeps{Volume: fake}}

	n, err := svc.reclaimCSIVolumes(context.Background(), "acct1", "test-cluster")
	require.NoError(t, err)
	require.Equal(t, 0, n, "volumes owned by a different cluster or untagged must not be reported")
}

func TestReclaimCSIVolumes_NilDepsIsNoop(t *testing.T) {
	svc := &EKSServiceImpl{deps: EKSServiceDeps{}}

	n, err := svc.reclaimCSIVolumes(context.Background(), "acct1", "test-cluster")
	require.NoError(t, err)
	require.Equal(t, 0, n, "a nil Volume dep must disable the reclaim step, not panic or error")
}

func TestReclaimCSIVolumes_DescribeErrorSurfaces(t *testing.T) {
	fake := &fakeCSIVolumeReclaimer{err: errors.New("describe volumes: boom")}
	svc := &EKSServiceImpl{deps: EKSServiceDeps{Volume: fake}}

	_, err := svc.reclaimCSIVolumes(context.Background(), "acct1", "test-cluster")
	require.Error(t, err, "a DescribeVolumes failure must surface, not be swallowed as zero volumes")
}
