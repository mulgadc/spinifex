//go:build integration

package integration

import (
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/stretchr/testify/require"
)

const (
	prTargetRole    = "pr-target-role"
	prTargetProfile = "pr-target-profile"
	prCallerRole    = "pr-caller-role"

	// prPolicyRunInstancesOnly grants ec2:RunInstances but no iam:PassRole, so
	// the instance-profile attach must be denied on that missing grant alone.
	prPolicyRunInstancesOnly = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"ec2:RunInstances","Resource":"*"}]}`
)

// prPolicyWithPassRole grants ec2:RunInstances plus iam:PassRole scoped to the
// target role's exact ARN, so attaching the profile is allowed once granted.
func prPolicyWithPassRole(targetRoleARN string) string {
	return fmt.Sprintf(`{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"ec2:RunInstances","Resource":"*"},
		{"Effect":"Allow","Action":"iam:PassRole","Resource":"%s"}
	]}`, targetRoleARN)
}

// TestRunInstances_DeniedWithoutPassRole exercises stored role paths with real IAM
// policy evaluation: missing grants and explicit denies reject the request,
// while an allow naming the stored role ARN permits dispatch.
func TestRunInstances_DeniedWithoutPassRole(t *testing.T) {
	for _, path := range []string{"/", "/team/", "/team/services/"} {
		t.Run(path, func(t *testing.T) {
			testInstanceProfilePassRole(t, path)
		})
	}
}

func testInstanceProfilePassRole(t *testing.T, path string) {
	t.Helper()
	gw := StartGateway(t)
	iamCli := gw.IAMClient(t)

	// The role backing the instance profile the caller wants to attach.
	targetRole, err := iamCli.CreateRole(&iam.CreateRoleInput{
		RoleName:                 aws.String(prTargetRole),
		Path:                     aws.String(path),
		AssumeRolePolicyDocument: aws.String(iamTrustPolicyEC2Standard),
	})
	require.NoError(t, err, "create-role (target)")
	targetRoleARN := aws.StringValue(targetRole.Role.Arn)

	_, err = iamCli.CreateInstanceProfile(&iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String(prTargetProfile),
	})
	require.NoError(t, err, "create-instance-profile")

	_, err = iamCli.AddRoleToInstanceProfile(&iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String(prTargetProfile),
		RoleName:            aws.String(prTargetRole),
	})
	require.NoError(t, err, "add-role-to-instance-profile")

	// The role the caller assumes, freely assumable so policy attachment is
	// the only variable (mirrors TestAssumedRoleControlPlaneEnforcement).
	callerRoleOut, err := iamCli.CreateRole(&iam.CreateRoleInput{
		RoleName:                 aws.String(prCallerRole),
		AssumeRolePolicyDocument: aws.String(assumedRoleTrustPolicy),
	})
	require.NoError(t, err, "create-role (caller)")

	runOnlyPolicy, err := iamCli.CreatePolicy(&iam.CreatePolicyInput{
		PolicyName:     aws.String("pr-run-instances-only"),
		PolicyDocument: aws.String(prPolicyRunInstancesOnly),
	})
	require.NoError(t, err, "create-policy run-instances-only")
	_, err = iamCli.AttachRolePolicy(&iam.AttachRolePolicyInput{
		RoleName:  callerRoleOut.Role.RoleName,
		PolicyArn: runOnlyPolicy.Policy.Arn,
	})
	require.NoError(t, err, "attach run-instances-only policy")

	assumeOut, err := gw.STSClient(t).AssumeRole(&sts.AssumeRoleInput{
		RoleArn:         callerRoleOut.Role.Arn,
		RoleSessionName: aws.String("pr-session"),
	})
	require.NoError(t, err, "assume-role")
	creds := assumeOut.Credentials
	require.NotNil(t, creds)

	sessionCli := gw.ClientsWithSessionCreds(t,
		aws.StringValue(creds.AccessKeyId),
		aws.StringValue(creds.SecretAccessKey),
		aws.StringValue(creds.SessionToken))

	runInput := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-0123456789abcdef0"),
		InstanceType: aws.String("t3.micro"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
		IamInstanceProfile: &ec2.IamInstanceProfileSpecification{
			Name: aws.String(prTargetProfile),
		},
	}

	// No PassRole grant: denied before any NATS dispatch, so no daemon stub
	// is needed for this call to resolve promptly.
	_, err = sessionCli.EC2.RunInstances(runInput)
	requireAWSErrorCode(t, err, "AccessDenied")

	// An explicit path-scoped deny must override an otherwise unrestricted grant.
	denyPolicy, err := iamCli.CreatePolicy(&iam.CreatePolicyInput{
		PolicyName: aws.String("pr-deny-path"),
		PolicyDocument: aws.String(fmt.Sprintf(`{"Version":"2012-10-17","Statement":[
			{"Effect":"Allow","Action":"*","Resource":"*"},
			{"Effect":"Deny","Action":"iam:PassRole","Resource":"arn:aws:iam::%s:role%s*"}
		]}`, gw.AccountID, path)),
	})
	require.NoError(t, err)
	_, err = iamCli.AttachRolePolicy(&iam.AttachRolePolicyInput{
		RoleName:  callerRoleOut.Role.RoleName,
		PolicyArn: denyPolicy.Policy.Arn,
	})
	require.NoError(t, err)
	_, err = sessionCli.EC2.RunInstances(runInput)
	requireAWSErrorCode(t, err, "AccessDenied")
	_, err = sessionCli.EC2.AssociateIamInstanceProfile(&ec2.AssociateIamInstanceProfileInput{
		InstanceId:         aws.String("i-0123456789abcdef0"),
		IamInstanceProfile: runInput.IamInstanceProfile,
	})
	requireAWSErrorCode(t, err, "AccessDenied")
	_, err = iamCli.DetachRolePolicy(&iam.DetachRolePolicyInput{
		RoleName:  callerRoleOut.Role.RoleName,
		PolicyArn: denyPolicy.Policy.Arn,
	})
	require.NoError(t, err)

	// Grant iam:PassRole scoped to the exact target role ARN; the identical
	// session must now be allowed through to a real launch.
	withPassRolePolicy, err := iamCli.CreatePolicy(&iam.CreatePolicyInput{
		PolicyName:     aws.String("pr-run-instances-plus-passrole"),
		PolicyDocument: aws.String(prPolicyWithPassRole(targetRoleARN)),
	})
	require.NoError(t, err, "create-policy with PassRole")
	_, err = iamCli.AttachRolePolicy(&iam.AttachRolePolicyInput{
		RoleName:  callerRoleOut.Role.RoleName,
		PolicyArn: withPassRolePolicy.Policy.Arn,
	})
	require.NoError(t, err, "attach PassRole policy")

	const (
		instanceType = "t3.micro"
		nodeID       = "pr-node"
	)
	gw.StubSubject(t, "spinifex.node.status", mustMarshal(t, &types.NodeStatusResponse{
		Node:          nodeID,
		InstanceTypes: []types.InstanceTypeCap{{Name: instanceType, Available: 2}},
	}))
	nodeCh := captureLaunchTemplateNodeInput(t, gw, instanceType, nodeID)

	out, err := sessionCli.EC2.RunInstances(runInput)
	require.NoError(t, err, "run-instances must succeed once PassRole is granted")
	require.Len(t, out.Instances, 1)
	awaitLaunchTemplateNodeInput(t, nodeCh) // drain: proves the launch actually dispatched to the daemon
}
