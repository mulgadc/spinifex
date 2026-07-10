// Package tags defines tag keys for Spinifex system-owned resources.
// The UI filters these out of customer-facing listings; operators append ?system=1 to surface them.
package tags

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

	// LBARNKey stores the parent LB ARN on ELBv2-managed ENIs.
	LBARNKey = "spinifex:lb-arn"

	// GPUVendorKey marks a system AMI (eks/ecs node) with the GPU vendor its
	// drivers target. Absent on non-GPU AMIs.
	GPUVendorKey = "gpu-vendor"

	// GPUVendorNVIDIA identifies an NVIDIA-driver GPU node AMI.
	GPUVendorNVIDIA = "nvidia"

	// GPUVendorAMD identifies an AMD-driver GPU node AMI.
	GPUVendorAMD = "amd"
)

// IsSystemManaged reports whether a ManagedBy value denotes a Spinifex
// platform-owned system VM (an empty value is a customer instance). System
// VMs bind a system.TerminateInstance.{id} subject so a cluster-wide teardown
// invoked on any node can route a terminate to the owning node.
func IsSystemManaged(managedBy string) bool {
	return managedBy == ManagedByELBv2 || managedBy == ManagedByEKS
}
