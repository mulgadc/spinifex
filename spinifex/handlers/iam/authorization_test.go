package handlers_iam

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/arn"
	"github.com/stretchr/testify/require"
)

func TestCanonicalResourceARNPreservesStoredPaths(t *testing.T) {
	svc := setupTestIAMService(t)
	trust := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`
	policy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*"}]}`

	_, err := svc.CreateUser(testAccountID, &iam.CreateUserInput{UserName: aws.String("alice"), Path: aws.String("/teams/")})
	require.NoError(t, err)
	_, err = svc.CreateRole(testAccountID, &iam.CreateRoleInput{RoleName: aws.String("app"), Path: aws.String("/service-roles/"), AssumeRolePolicyDocument: aws.String(trust)})
	require.NoError(t, err)
	_, err = svc.CreateGroup(testAccountID, &iam.CreateGroupInput{GroupName: aws.String("ops"), Path: aws.String("/teams/")})
	require.NoError(t, err)
	_, err = svc.CreatePolicy(testAccountID, &iam.CreatePolicyInput{PolicyName: aws.String("app"), Path: aws.String("/managed/"), PolicyDocument: aws.String(policy)})
	require.NoError(t, err)
	_, err = svc.CreateInstanceProfile(testAccountID, &iam.CreateInstanceProfileInput{InstanceProfileName: aws.String("nodes"), Path: aws.String("/eks/")})
	require.NoError(t, err)

	tests := []struct {
		kind arn.IAMResourceType
		name string
		want string
	}{
		{arn.IAMUser, "alice", "arn:aws:iam::000000000000:user/teams/alice"},
		{arn.IAMRole, "app", "arn:aws:iam::000000000000:role/service-roles/app"},
		{arn.IAMGroup, "ops", "arn:aws:iam::000000000000:group/teams/ops"},
		{arn.IAMPolicy, "app", "arn:aws:iam::000000000000:policy/managed/app"},
		{arn.IAMInstanceProfile, "nodes", "arn:aws:iam::000000000000:instance-profile/eks/nodes"},
	}
	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			got, err := svc.CanonicalResourceARN(testAccountID, tt.kind, tt.name)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}
