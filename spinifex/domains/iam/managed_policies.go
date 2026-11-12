package iam

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	iamarn "github.com/mulgadc/bluebottle/pkg/auth"
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

	// Verb prefixes rather than the rds:* AWS publishes: rds:* would also match
	// the internal agent actions the gateway reserves for a DB VM's own role, and
	// a document appearing to grant those would be a lie in GetPolicyVersion.
	"arn:aws:iam::aws:policy/AmazonRDSFullAccess": `{
		"Version": "2012-10-17",
		"Statement": [
			{
				"Effect": "Allow",
				"Action": [
					"rds:Add*",
					"rds:Copy*",
					"rds:Create*",
					"rds:Delete*",
					"rds:Describe*",
					"rds:Download*",
					"rds:Failover*",
					"rds:List*",
					"rds:Modify*",
					"rds:Promote*",
					"rds:Purchase*",
					"rds:Reboot*",
					"rds:Remove*",
					"rds:Reset*",
					"rds:Restore*",
					"rds:Start*",
					"rds:Stop*"
				],
				"Resource": "*"
			},
			{
				"Effect": "Allow",
				"Action": [
					"ec2:DescribeAccountAttributes",
					"ec2:DescribeAvailabilityZones",
					"ec2:DescribeInternetGateways",
					"ec2:DescribeSecurityGroups",
					"ec2:DescribeSubnets",
					"ec2:DescribeVpcAttribute",
					"ec2:DescribeVpcs",
					"cloudwatch:GetMetricStatistics",
					"cloudwatch:ListMetrics",
					"logs:DescribeLogStreams",
					"logs:GetLogEvents",
					"sns:ListSubscriptions",
					"sns:ListTopics",
					"sns:Publish"
				],
				"Resource": "*"
			},
			{
				"Sid": "AllowServiceLinkedRole",
				"Effect": "Allow",
				"Action": "iam:CreateServiceLinkedRole",
				"Resource": "*"
			}
		]
	}`,

	"arn:aws:iam::aws:policy/AmazonRDSReadOnlyAccess": `{
		"Version": "2012-10-17",
		"Statement": [
			{
				"Effect": "Allow",
				"Action": [
					"rds:Describe*",
					"rds:ListTagsForResource",
					"rds:DownloadDBLogFilePortion"
				],
				"Resource": "*"
			},
			{
				"Effect": "Allow",
				"Action": [
					"ec2:DescribeAccountAttributes",
					"ec2:DescribeAvailabilityZones",
					"ec2:DescribeSecurityGroups",
					"ec2:DescribeSubnets",
					"ec2:DescribeVpcs",
					"cloudwatch:GetMetricStatistics",
					"cloudwatch:ListMetrics",
					"logs:DescribeLogStreams",
					"logs:GetLogEvents"
				],
				"Resource": "*"
			}
		]
	}`,

	"arn:aws:iam::aws:policy/AmazonRoute53FullAccess": `{
		"Version": "2012-10-17",
		"Statement": [
			{
				"Effect": "Allow",
				"Action": [
					"route53:*",
					"route53domains:*",
					"cloudfront:ListDistributions",
					"elasticloadbalancing:DescribeLoadBalancers",
					"elasticbeanstalk:DescribeEnvironments",
					"s3:ListBucket",
					"s3:GetBucketLocation",
					"s3:GetBucketWebsite",
					"ec2:DescribeVpcs",
					"ec2:DescribeVpcEndpoints",
					"ec2:DescribeRegions",
					"sns:ListTopics",
					"sns:ListSubscriptionsByTopic",
					"cloudwatch:DescribeAlarms",
					"cloudwatch:GetMetricStatistics"
				],
				"Resource": "*"
			},
			{
				"Effect": "Allow",
				"Action": "apigateway:GET",
				"Resource": "arn:aws:apigateway:*::/domainnames"
			}
		]
	}`,

	"arn:aws:iam::aws:policy/AmazonRoute53ReadOnlyAccess": `{
		"Version": "2012-10-17",
		"Statement": [
			{
				"Effect": "Allow",
				"Action": [
					"route53:Get*",
					"route53:List*",
					"route53:TestDNSAnswer"
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

// managedPolicyVersion is the AWS policy version a shipped document is pinned
// to, with the metadata GetPolicy reports for it. Times are RFC 3339 UTC.
type managedPolicyVersion struct {
	VersionID   string
	Description string
	CreatedAt   string
	UpdatedAt   string
}

// awsManagedPolicyVersions pins every entry of awsManagedPolicyDocs to the
// default version AWS publishes in its managed policy reference guide.
var awsManagedPolicyVersions = map[string]managedPolicyVersion{
	"arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy": {
		VersionID:   "v3",
		Description: "This policy allows Amazon EKS worker nodes to connect to Amazon EKS Clusters.",
		CreatedAt:   "2018-05-27T21:09:01Z",
		UpdatedAt:   "2023-11-27T00:06:13Z",
	},
	"arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy": {
		VersionID:   "v6",
		Description: "This policy provides the Amazon VPC CNI Plugin (amazon-vpc-cni-k8s) the permissions it requires to modify the IP address configuration on your EKS worker nodes. This permission set allows the CNI to list, describe, and modify Elastic Network Interfaces on your behalf. More information on the AWS VPC CNI Plugin is available here: https://github.com/aws/amazon-vpc-cni-k8s",
		CreatedAt:   "2018-05-27T21:07:00Z",
		UpdatedAt:   "2026-03-04T19:12:00Z",
	},
	"arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly": {
		VersionID:   "v3",
		Description: "Provides read-only access to Amazon EC2 Container Registry repositories.",
		CreatedAt:   "2015-12-21T17:04:00Z",
		UpdatedAt:   "2019-12-10T20:56:00Z",
	},
	"arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryPullOnly": {
		VersionID:   "v1",
		Description: "Provides access to pull images from Amazon EC2 Container Registry repositories.",
		CreatedAt:   "2024-10-04T16:58:00Z",
		UpdatedAt:   "2024-10-04T16:58:00Z",
	},
	"arn:aws:iam::aws:policy/AmazonEKSClusterPolicy": {
		VersionID:   "v10",
		Description: "This policy provides Kubernetes the permissions it requires to manage resources on your behalf. Kubernetes requires Ec2:CreateTags permissions to place identifying information on EC2 resources including but not limited to Instances, Security Groups, and Elastic Network Interfaces. ",
		CreatedAt:   "2018-05-27T21:06:14Z",
		UpdatedAt:   "2026-02-12T17:58:15Z",
	},
	"arn:aws:iam::aws:policy/AmazonRDSFullAccess": {
		VersionID:   "v14",
		Description: "Provides full access to Amazon RDS via the AWS Management Console.",
		CreatedAt:   "2015-02-06T18:40:00Z",
		UpdatedAt:   "2023-08-17T23:00:00Z",
	},
	"arn:aws:iam::aws:policy/AmazonRDSReadOnlyAccess": {
		VersionID:   "v7",
		Description: "Provides read only access to Amazon RDS via the AWS Management Console.",
		CreatedAt:   "2015-02-06T18:40:00Z",
		UpdatedAt:   "2023-04-14T12:32:00Z",
	},
	"arn:aws:iam::aws:policy/AmazonRoute53FullAccess": {
		VersionID:   "v8",
		Description: "Provides full access to all Amazon Route 53 via the AWS Management Console.",
		CreatedAt:   "2015-02-06T18:40:00Z",
		UpdatedAt:   "2026-10-08T20:17:00Z",
	},
	"arn:aws:iam::aws:policy/AmazonRoute53ReadOnlyAccess": {
		VersionID:   "v2",
		Description: "Provides read only access to all Amazon Route 53 via the AWS Management Console.",
		CreatedAt:   "2015-02-06T18:40:00Z",
		UpdatedAt:   "2016-11-15T21:15:00Z",
	},
	"arn:aws:iam::aws:policy/AmazonEKSServicePolicy": {
		VersionID:   "v7",
		Description: "This policy allows Amazon Elastic Container Service for Kubernetes to create and manage the necessary resources to operate EKS Clusters.",
		CreatedAt:   "2018-05-27T21:08:00Z",
		UpdatedAt:   "2024-10-14T21:12:00Z",
	},
}

// builtinManagedPolicy returns a shipped AWS-managed policy as a Policy
// record. ok is false for managed ARNs Spinifex does not ship.
func builtinManagedPolicy(policyARN string) (*Policy, bool) {
	raw, ok := awsManagedPolicyDocs[policyARN]
	if !ok {
		return nil, false
	}
	pin := awsManagedPolicyVersions[policyARN]
	sum := sha256.Sum256([]byte(policyARN))
	return &Policy{
		PolicyName:     policyARN[strings.LastIndex(policyARN, "/")+1:],
		PolicyID:       "ANPA" + strings.ToUpper(hex.EncodeToString(sum[:]))[:17],
		ARN:            policyARN,
		Path:           "/",
		Description:    pin.Description,
		PolicyDocument: raw,
		CreatedAt:      pin.CreatedAt,
		UpdatedAt:      pin.UpdatedAt,
		DefaultVersion: pin.VersionID,
	}, true
}

// builtinManagedPolicyParsed is the parsed form of awsManagedPolicyDocs, built
// once at init. NewIAMServiceImpl propagates any parsing failure.
var builtinManagedPolicyParsed, builtinManagedPolicyParseErr = parseBuiltinManagedPolicies(awsManagedPolicyDocs)

func parseBuiltinManagedPolicies(rawPolicies map[string]string) (map[string]PolicyDocument, error) {
	parsed := make(map[string]PolicyDocument, len(rawPolicies))
	for arn, raw := range rawPolicies {
		var doc PolicyDocument
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			return nil, fmt.Errorf("parse managed policy %s: %w", arn, err)
		}
		parsed[arn] = doc
	}
	return parsed, nil
}

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
func (s *IAMServiceImpl) resolveAttachedPolicy(ctx context.Context, accountID, arn string) (doc PolicyDocument, include bool, err error) {
	if iamarn.IsAWSManagedPolicyARN(arn) {
		if d, ok := builtinManagedPolicyDoc(arn); ok {
			return d, true, nil
		}
		return PolicyDocument{}, false, nil
	}
	policy, err := s.getPolicyByARN(ctx, accountID, arn)
	if err != nil {
		return PolicyDocument{}, false, fmt.Errorf("resolve policy %s: %w", arn, err)
	}
	if err := json.Unmarshal([]byte(policy.PolicyDocument), &doc); err != nil {
		return PolicyDocument{}, false, fmt.Errorf("parse policy %s: %w", policy.PolicyName, err)
	}
	return doc, true, nil
}
