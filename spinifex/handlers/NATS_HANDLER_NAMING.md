# NATS Handler Naming Convention

This document defines the naming convention for NATS message handlers in the Spinifex daemon.

## Pattern

**Format**: `handleEC2<AWSAction>` → NATS topic `ec2.<AWSAction>`

Where `<AWSAction>` matches the AWS API action name exactly (PascalCase).

### Benefits
1. **AWS API Alignment** - Handler names directly correlate with AWS documentation
2. **Self-Documenting** - Method name clearly indicates which AWS action it handles
3. **Scalable** - Pattern extends cleanly to all AWS services
4. **Consistent** - Same pattern across all handlers

## Current Handlers

### EC2 Instance Operations
```go
handleEC2RunInstances       → ec2.RunInstances         // Launch instances
handleEC2DescribeInstances  → ec2.DescribeInstances    // Query instance status
handleEC2StartInstances     → ec2.StartInstances       // Start stopped instances
handleEC2StopInstances      → ec2.StopInstances        // Stop running instances
handleEC2TerminateInstances → ec2.TerminateInstances   // Terminate instances
handleEC2RebootInstances    → ec2.RebootInstances      // Reboot instances
```

### EC2 Images (AMI)
```go
handleEC2DescribeImages     → ec2.DescribeImages       // List available AMIs
handleEC2CreateImage        → ec2.CreateImage          // Create AMI from instance
handleEC2RegisterImage      → ec2.RegisterImage        // Register external AMI
handleEC2DeregisterImage    → ec2.DeregisterImage      // Remove AMI
handleEC2CopyImage          → ec2.CopyImage            // Copy AMI across regions
```

### EC2 Key Pairs
```go
handleEC2CreateKeyPair      → ec2.CreateKeyPair        // Generate new key pair
handleEC2DeleteKeyPair      → ec2.DeleteKeyPair        // Remove key pair
handleEC2DescribeKeyPairs   → ec2.DescribeKeyPairs     // List key pairs
handleEC2ImportKeyPair      → ec2.ImportKeyPair        // Import existing public key
```

### EBS Volumes
```go
handleEC2CreateVolume       → ec2.CreateVolume         // Create EBS volume
handleEC2AttachVolume       → ec2.AttachVolume         // Attach volume to instance
handleEC2DetachVolume       → ec2.DetachVolume         // Detach volume
handleEC2DeleteVolume       → ec2.DeleteVolume         // Remove volume
handleEC2DescribeVolumes    → ec2.DescribeVolumes      // List volumes
handleEC2CreateSnapshot     → ec2.CreateSnapshot       // Create volume snapshot
handleEC2DeleteSnapshot     → ec2.DeleteSnapshot       // Remove snapshot
```

### VPC Networking
```go
handleEC2CreateVpc          → ec2.CreateVpc            // Create VPC
handleEC2DeleteVpc          → ec2.DeleteVpc            // Remove VPC
handleEC2DescribeVpcs       → ec2.DescribeVpcs         // List VPCs
handleEC2CreateSubnet       → ec2.CreateSubnet         // Create subnet
handleEC2DeleteSubnet       → ec2.DeleteSubnet         // Remove subnet
handleEC2DescribeSubnets    → ec2.DescribeSubnets      // List subnets
```

### Security Groups
```go
handleEC2CreateSecurityGroup    → ec2.CreateSecurityGroup     // Create security group
handleEC2DeleteSecurityGroup    → ec2.DeleteSecurityGroup     // Remove security group
handleEC2DescribeSecurityGroups → ec2.DescribeSecurityGroups  // List security groups
handleEC2AuthorizeSecurityGroupIngress  → ec2.AuthorizeSecurityGroupIngress   // Add inbound rule
handleEC2RevokeSecurityGroupIngress     → ec2.RevokeSecurityGroupIngress      // Remove inbound rule
```

## Implementation Example

### Daemon Handler Method
```go
// handleEC2RunInstances processes incoming EC2 RunInstances requests
func (d *Daemon) handleEC2RunInstances(msg *nats.Msg) {
    // Parse request
    runInstancesInput := &ec2.RunInstancesInput{}
    errResp := utils.UnmarshalJsonPayload(runInstancesInput, msg.Data)

    // Validate inputs
    err := gateway_ec2_instance.ValidateRunInstancesInput(runInstancesInput)

    // Process and respond...
}
```

### NATS Subscription
```go
// Subscribe to EC2 RunInstances with queue group
d.natsSubscriptions["ec2.RunInstances"], err = d.natsConn.QueueSubscribe(
    "ec2.RunInstances",           // NATS topic
    "spinifex-workers",               // Queue group for load balancing
    d.handleEC2RunInstances,      // Handler method
)
```

### Gateway Client Request
```go
// Gateway sends RunInstances request via NATS
msg, err := nc.Request("ec2.RunInstances", jsonData, 30*time.Second)
```

## Migration Notes

### Legacy Topics
For backward compatibility, some handlers may subscribe to both old and new topic formats:

```go
// Legacy topic (deprecated, for backward compatibility)
d.natsSubscriptions["ec2.launch"], err = d.natsConn.QueueSubscribe(
    "ec2.launch", "spinifex-workers", d.handleEC2RunInstances)

// New topic (recommended)
d.natsSubscriptions["ec2.RunInstances"], err = d.natsConn.QueueSubscribe(
    "ec2.RunInstances", "spinifex-workers", d.handleEC2RunInstances)
```

**Recommendation**: New code should use the AWS Action name format (`ec2.RunInstances`).

## Queue Groups

All handlers use the `"spinifex-workers"` queue group for:
- **Load Balancing** - NATS distributes requests across available daemon instances
- **High Availability** - If one daemon fails, others continue processing
- **Scalability** - Add more daemon instances to handle increased load

## Testing Pattern

Test function names follow the same convention:

```go
func TestHandleEC2RunInstances_MessageParsing(t *testing.T) { ... }
func TestHandleEC2RunInstances_ResourceManagement(t *testing.T) { ... }
func TestHandleEC2DescribeInstances_FilterByID(t *testing.T) { ... }
```

## Future Extensions

### S3 Operations
```go
handleS3CreateBucket    → s3.CreateBucket
handleS3DeleteBucket    → s3.DeleteBucket
handleS3PutObject       → s3.PutObject
handleS3GetObject       → s3.GetObject
```

### IAM Operations
```go
handleIAMCreateUser     → iam.CreateUser
handleIAMDeleteUser     → iam.DeleteUser
handleIAMCreateRole     → iam.CreateRole
```

This pattern scales consistently across all AWS services.

## EKS (Elastic Kubernetes Service)

EKS uses two layers of NATS subjects.

### Layer 1 — AWS API surface

The gateway translates EKS REST-JSON requests into `eks.<AWSAction>` NATS
requests, using the existing `spinifex-workers` queue group. The handler
name follows the same `handleEKS<AWSAction>` convention.

```
eks.CreateCluster, eks.DescribeCluster, eks.ListClusters,
eks.UpdateClusterConfig, eks.UpdateClusterVersion, eks.DeleteCluster
eks.CreateNodegroup, eks.DescribeNodegroup, eks.ListNodegroups,
eks.UpdateNodegroupConfig, eks.UpdateNodegroupVersion, eks.DeleteNodegroup
eks.CreateAccessEntry, eks.DescribeAccessEntry, eks.ListAccessEntries,
eks.UpdateAccessEntry, eks.DeleteAccessEntry,
eks.AssociateAccessPolicy, eks.DisassociateAccessPolicy,
eks.ListAssociatedAccessPolicies, eks.ListAccessPolicies
eks.ListAddons, eks.DescribeAddonVersions,
eks.CreateAddon, eks.DeleteAddon, eks.DescribeAddon, eks.UpdateAddon
eks.AssociateIdentityProviderConfig, eks.DescribeIdentityProviderConfig,
eks.ListIdentityProviderConfigs, eks.DisassociateIdentityProviderConfig
eks.TagResource, eks.UntagResource, eks.ListTagsForResource
```

### Layer 2 — internal reconciler bus

The cluster + nodegroup reconcilers communicate with K3s VMs and each
other on `eks.bus.<accountID>.<clusterName>.*`. This layer lands with the
reconciler bodies; only Layer 1 is registered today. See
`docs/development/feature/eks-v1.md` Q11 for the full subject list.

### Cross-service calls

EKS reconcilers also publish on existing namespaces when interacting with
other services (no `eks.` prefix): `ec2.RunInstances`,
`ec2.CreateNetworkInterface`, `elbv2.CreateLoadBalancer`,
`elbv2.RegisterTargets`, `route53.ChangeResourceRecordSets`, etc.
