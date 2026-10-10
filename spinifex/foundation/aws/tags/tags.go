// Package tags defines Spinifex system-tag vocabulary and generic AWS EC2 tag
// transformations. The UI filters system-managed resources out of
// customer-facing listings; operators append ?system=1 to surface them.
package tags

import (
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
)

const (
	// ManagedByKey marks a resource as managed by a Spinifex
	// platform component. The value identifies the component.
	ManagedByKey = "spinifex:managed-by"

	// ManagedByELBv2 identifies ELBv2/ALB-owned resources
	// (HAProxy VMs, their ENIs, the LB AMI).
	ManagedByELBv2 = "elbv2"

	// ManagedByEKS identifies EKS-owned resources (K3s control-plane VMs,
	// their ENIs, the unified eks-node AMI, cluster + nodegroup SGs).
	ManagedByEKS = "eks"

	// ManagedByECS identifies the spinifex-ecs-node AMI for resolution.
	// Container instances launched from it stay customer-owned and untagged,
	// so this value is not added to IsSystemManaged.
	ManagedByECS = "ecs"

	// ManagedByRDS identifies RDS-owned resources (DB engine VMs, the
	// rds-postgres AMI, the shared RDS system VPC and its ENIs). Unlike
	// ManagedByECS the VMs themselves carry it, marking them system-owned to the
	// UI's listings and the platform's own sweeps.
	ManagedByRDS = "rds"

	// ManagedByBedrock identifies self-hosted vLLM serving VMs, their ENIs, the
	// vllm-serving AMI, and the shared Bedrock system VPC.
	ManagedByBedrock = "bedrock"

	// LBARNKey stores the parent LB ARN on ELBv2-managed ENIs.
	LBARNKey = "spinifex:lb-arn"

	// GPUVendorKey marks a system AMI (eks/ecs node) with the GPU vendor its
	// drivers target. Absent on non-GPU AMIs.
	GPUVendorKey = "gpu-vendor"

	// GPUVendorNVIDIA identifies an NVIDIA-driver GPU node AMI.
	GPUVendorNVIDIA = "nvidia"

	// GPUVendorAMD identifies an AMD-driver GPU node AMI.
	GPUVendorAMD = "amd"

	// DHCPDisabledKey marks an ENI whose logical switch port must not get OVN
	// DHCP options — used for statically-addressed customer ENIs whose lease
	// would otherwise be flushed by the guest's dhcpcd at expiry. Internal:
	// stripped from the persisted user-visible tag set at CreateNetworkInterface.
	DHCPDisabledKey = "spinifex:dhcp"

	// DHCPDisabledValue is the DHCPDisabledKey value that suppresses DHCP.
	DHCPDisabledValue = "disabled"
)

// IsSystemManaged reports whether a ManagedBy value denotes a Spinifex
// platform-owned system VM (an empty value is a customer instance). System
// VMs bind a system.TerminateInstance.{id} subject so a cluster-wide teardown
// invoked on any node can route a terminate to the owning node.
func IsSystemManaged(managedBy string) bool {
	return managedBy == ManagedByELBv2 || managedBy == ManagedByEKS || managedBy == ManagedByRDS || managedBy == ManagedByBedrock
}

// MapToEC2 converts a tag map to a slice of EC2 Tag pointers. It returns nil
// when the input map is empty.
func MapToEC2(m map[string]string) []*ec2.Tag {
	if len(m) == 0 {
		return nil
	}
	tags := make([]*ec2.Tag, 0, len(m))
	for k, v := range m {
		tags = append(tags, &ec2.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return tags
}

// Extract returns tags from the TagSpecification matching resourceType. If no
// specification matches, the returned map is empty rather than nil.
func Extract(tagSpecs []*ec2.TagSpecification, resourceType string) map[string]string {
	tags := make(map[string]string)
	for _, tagSpec := range tagSpecs {
		if tagSpec.ResourceType != nil && *tagSpec.ResourceType == resourceType {
			for _, tag := range tagSpec.Tags {
				if tag.Key != nil && tag.Value != nil {
					tags[*tag.Key] = *tag.Value
				}
			}
		}
	}
	return tags
}
