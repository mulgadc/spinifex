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

	// LBARNKey stores the parent LB ARN on ELBv2-managed ENIs.
	LBARNKey = "spinifex:lb-arn"
)
