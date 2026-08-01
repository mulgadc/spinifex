package handlers_eks

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
)

// csiClusterOwnerTag is the tag key the aws-ebs-csi-driver controller stamps
// on every volume it provisions, once its --k8s-tag-cluster-id flag is set to
// the cluster name (scripts/images/eks-node/addons/aws-ebs-csi-driver's
// 10-controller.yaml). Without that flag a CSI-provisioned volume carries no
// cluster-scoped tag at all — only PVC/PV identity — so reclaim would have to
// guess across every cluster sharing the account, which risks matching a
// different live cluster's volume.
func csiClusterOwnerTag(name string) string {
	return "kubernetes.io/cluster/" + name
}

// reclaimCSIVolumes finds EBS volumes the in-cluster CSI driver provisioned
// for this cluster's PVCs and reports (never deletes) whichever are still
// around after the CP/nodegroup teardown above. Real EKS has the identical
// gap: the CSI controller's own DeleteVolume call only runs while the cluster
// (and its controller pod) is still alive, so a cluster deleted with PVCs
// still bound leaks their volumes there too — the customer is expected to
// delete workloads before deleting the cluster. This step exists purely to
// make that leak visible here instead of silent, since nothing else looks for
// it: VolumeLeakReaper only catches a volume still attached to a definitively
// gone instance, and an EKS worker's CSI-provisioned volume is detached by
// the time its node is torn down.
//
// It never deletes: a PV's reclaimPolicy (Delete vs Retain) is a
// Kubernetes-side property the CSI driver does not tag onto the volume, so
// there is no safe way to tell from EC2 tags alone whether deleting a given
// volume is what the customer wanted. Deleting a Retain volume would be
// unrecoverable data loss, so — mirroring VolumeLeakReaper's mark-and-alarm
// policy for the identical reason (ADR-0005 §3) — reclamation stays an
// explicit operator action.
func (s *EKSServiceImpl) reclaimCSIVolumes(ctx context.Context, accountID, name string) (int, error) {
	if s.deps.Volume == nil {
		return 0, nil
	}
	out, err := s.deps.Volume.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{}, accountID)
	if err != nil {
		return 0, fmt.Errorf("describe volumes: %w", err)
	}

	ownerTag := csiClusterOwnerTag(name)
	reported := 0
	for _, vol := range out.Volumes {
		if vol == nil {
			continue
		}
		var owned bool
		var pvcNamespace, pvcName string
		for _, tag := range vol.Tags {
			if tag == nil || tag.Key == nil {
				continue
			}
			switch aws.StringValue(tag.Key) {
			case ownerTag:
				owned = true
			case "kubernetes.io/created-for/pvc/namespace":
				pvcNamespace = aws.StringValue(tag.Value)
			case "kubernetes.io/created-for/pvc/name":
				pvcName = aws.StringValue(tag.Value)
			}
		}
		if !owned {
			continue
		}
		reported++
		slog.WarnContext(ctx, "purgeClusterInfra: CSI-provisioned volume outlived cluster teardown, not deleted (reclaimPolicy unknown from tags alone — operator action required)",
			"cluster", name, "account", accountID, "volumeId", aws.StringValue(vol.VolumeId),
			"pvcNamespace", pvcNamespace, "pvcName", pvcName, "state", aws.StringValue(vol.State))
	}
	return reported, nil
}
