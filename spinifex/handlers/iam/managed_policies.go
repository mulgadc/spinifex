package handlers_iam

import (
	"encoding/json"
	"fmt"
)

// awsManagedPolicyDocs maps well-known AWS-managed policy ARNs to the grant
// documents AWS publishes for them. Spinifex does not provision managed
// policies (AttachRolePolicy/AttachUserPolicy round-trip the ARN opaquely),
// but assumed-role and user authorization must honour their grants so stock
// EKS roles — whose permissions come entirely from these policies — work
// without a backing document. Unknown managed ARNs are not modeled and resolve
// to no grant (fail-closed deny), never an error.
//
// Documents mirror the AWS-published policies; Condition blocks are omitted
// because Statement carries no Condition field (a minor, deliberate over-grant
// acceptable for a compat shim).
var awsManagedPolicyDocs = map[string]string{
	"arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy": `{
		"Version": "2012-10-17",
		"Statement": [
			{
				"Effect": "Allow",
				"Action": [
					"ec2:DescribeInstances",
					"ec2:DescribeInstanceTypes",
					"ec2:DescribeRouteTables",
					"ec2:DescribeSecurityGroups",
					"ec2:DescribeSubnets",
					"ec2:DescribeVolumes",
					"ec2:DescribeVolumesModifications",
					"ec2:DescribeVpcs",
					"ec2:DescribeDhcpOptions",
					"ec2:DescribeNetworkInterfaces",
					"ec2:DescribeInstanceTopology"
				],
				"Resource": "*"
			},
			{
				"Sid": "EKSDescribeClusterPolicy",
				"Effect": "Allow",
				"Action": "eks:DescribeCluster",
				"Resource": "*"
			}
		]
	}`,

	"arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy": `{
		"Version": "2012-10-17",
		"Statement": [
			{
				"Effect": "Allow",
				"Action": [
					"ec2:AssignPrivateIpAddresses",
					"ec2:AttachNetworkInterface",
					"ec2:CreateNetworkInterface",
					"ec2:DeleteNetworkInterface",
					"ec2:DescribeInstances",
					"ec2:DescribeTags",
					"ec2:DescribeNetworkInterfaces",
					"ec2:DescribeInstanceTypes",
					"ec2:DescribeSubnets",
					"ec2:DetachNetworkInterface",
					"ec2:ModifyNetworkInterfaceAttribute",
					"ec2:UnassignPrivateIpAddresses"
				],
				"Resource": "*"
			},
			{
				"Sid": "CreateTags",
				"Effect": "Allow",
				"Action": "ec2:CreateTags",
				"Resource": "arn:aws:ec2:*:*:network-interface/*"
			}
		]
	}`,

	"arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly": `{
		"Version": "2012-10-17",
		"Statement": [
			{
				"Effect": "Allow",
				"Action": [
					"ecr:GetAuthorizationToken",
					"ecr:BatchCheckLayerAvailability",
					"ecr:GetDownloadUrlForLayer",
					"ecr:GetRepositoryPolicy",
					"ecr:DescribeRepositories",
					"ecr:ListImages",
					"ecr:DescribeImages",
					"ecr:BatchGetImage",
					"ecr:GetLifecyclePolicy",
					"ecr:GetLifecyclePolicyPreview",
					"ecr:ListTagsForResource",
					"ecr:DescribeImageScanFindings"
				],
				"Resource": "*"
			}
		]
	}`,

	"arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryPullOnly": `{
		"Version": "2012-10-17",
		"Statement": [
			{
				"Effect": "Allow",
				"Action": [
					"ecr:GetAuthorizationToken",
					"ecr:BatchCheckLayerAvailability",
					"ecr:GetDownloadUrlForLayer",
					"ecr:BatchGetImage"
				],
				"Resource": "*"
			}
		]
	}`,

	"arn:aws:iam::aws:policy/AmazonEKSClusterPolicy": `{
		"Version": "2012-10-17",
		"Statement": [
			{
				"Effect": "Allow",
				"Action": [
					"autoscaling:DescribeAutoScalingGroups",
					"autoscaling:UpdateAutoScalingGroup",
					"ec2:AttachVolume",
					"ec2:AuthorizeSecurityGroupIngress",
					"ec2:CreateRoute",
					"ec2:CreateSecurityGroup",
					"ec2:CreateTags",
					"ec2:CreateVolume",
					"ec2:DeleteRoute",
					"ec2:DeleteSecurityGroup",
					"ec2:DeleteVolume",
					"ec2:DescribeInstances",
					"ec2:DescribeRouteTables",
					"ec2:DescribeSecurityGroups",
					"ec2:DescribeSubnets",
					"ec2:DescribeVolumes",
					"ec2:DescribeVolumesModifications",
					"ec2:DescribeVpcs",
					"ec2:DescribeDhcpOptions",
					"ec2:DescribeNetworkInterfaces",
					"ec2:DescribeAvailabilityZones",
					"ec2:DescribeAccountAttributes",
					"ec2:DescribeAddresses",
					"ec2:DescribeInternetGateways",
					"ec2:DetachVolume",
					"ec2:ModifyInstanceAttribute",
					"ec2:ModifyVolume",
					"ec2:RevokeSecurityGroupIngress",
					"elasticloadbalancing:*",
					"kms:DescribeKey"
				],
				"Resource": "*"
			},
			{
				"Sid": "AllowServiceLinkedRole",
				"Effect": "Allow",
				"Action": [
					"iam:CreateServiceLinkedRole",
					"iam:ListAttachedRolePolicies"
				],
				"Resource": "*"
			}
		]
	}`,

	"arn:aws:iam::aws:policy/AmazonEKSServicePolicy": `{
		"Version": "2012-10-17",
		"Statement": [
			{
				"Effect": "Allow",
				"Action": [
					"eks:UpdateClusterVersion",
					"ec2:CreateNetworkInterface",
					"ec2:CreateNetworkInterfacePermission",
					"ec2:DeleteNetworkInterface",
					"ec2:DescribeInstances",
					"ec2:DescribeNetworkInterfaces",
					"ec2:DescribeSecurityGroups",
					"ec2:DescribeSubnets",
					"ec2:DescribeVpcs",
					"ec2:ModifyNetworkInterfaceAttribute",
					"route53:AssociateVPCWithHostedZone",
					"kms:DescribeKey"
				],
				"Resource": "*"
			}
		]
	}`,
}

// builtinManagedPolicyParsed is the parsed form of awsManagedPolicyDocs, built
// once at init. A malformed builtin document is a programming error and panics.
var builtinManagedPolicyParsed = func() map[string]PolicyDocument {
	parsed := make(map[string]PolicyDocument, len(awsManagedPolicyDocs))
	for arn, raw := range awsManagedPolicyDocs {
		var doc PolicyDocument
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			panic(fmt.Sprintf("iam: malformed builtin managed policy %s: %v", arn, err))
		}
		parsed[arn] = doc
	}
	return parsed
}()

// builtinManagedPolicyDoc returns the modeled grant document for an AWS-managed
// policy ARN. ok is false for managed ARNs Spinifex does not model.
func builtinManagedPolicyDoc(arn string) (PolicyDocument, bool) {
	doc, ok := builtinManagedPolicyParsed[arn]
	return doc, ok
}

// resolveAttachedPolicy resolves one attached policy ARN to its grant document
// for authorization. AWS-managed ARNs resolve from the builtin registry, or
// resolve to no grant (include=false) when not modeled — never an error, so a
// role carrying an unmodeled managed policy is denied that grant rather than
// failing the whole request. Customer-managed ARNs are fetched from KV and
// fail closed (error) when unresolvable.
func (s *IAMServiceImpl) resolveAttachedPolicy(accountID, arn string) (doc PolicyDocument, include bool, err error) {
	if isAWSManagedPolicyARN(arn) {
		if d, ok := builtinManagedPolicyDoc(arn); ok {
			return d, true, nil
		}
		return PolicyDocument{}, false, nil
	}
	policy, err := s.getPolicyByARN(accountID, arn)
	if err != nil {
		return PolicyDocument{}, false, fmt.Errorf("resolve policy %s: %w", arn, err)
	}
	if err := json.Unmarshal([]byte(policy.PolicyDocument), &doc); err != nil {
		return PolicyDocument{}, false, fmt.Errorf("parse policy %s: %w", policy.PolicyName, err)
	}
	return doc, true, nil
}
