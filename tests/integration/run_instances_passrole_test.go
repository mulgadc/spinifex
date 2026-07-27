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

// TestRunInstances_DeniedWithoutPassRole proves RunInstances enforces
// iam:PassRole (resolveAndAuthorizeInstanceProfile ->
// resolveAndAuthorizeProfile in gateway/ec2/instance/RunInstances.go /
// IamInstanceProfileAssociation.go) against the role inside a supplied
// instance profile: a caller granted ec2:RunInstances but not iam:PassRole on
// the target role is denied, and the identical session is allowed once the
// role grants that specific PassRole. Isolating PassRole as the sole variable
// (ec2:RunInstances is granted throughout) proves the denial is genuinely
// gated on the missing PassRole grant rather than some other authz gap.
func TestRunInstances_DeniedWithoutPassRole(t *testing.T) {
	gw := StartGateway(t)
	iamCli := gw.IAMClient(t)

	targetRoleARN := iamRoleARN(gw.AccountID, prTargetRole)

	// The role backing the instance profile the caller wants to attach.
	_, err := iamCli.CreateRole(&iam.CreateRoleInput{
		RoleName:                 aws.String(prTargetRole),
		AssumeRolePolicyDocument: aws.String(iamTrustPolicyEC2Standard),
	})
	require.NoError(t, err, "create-role (target)")

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
