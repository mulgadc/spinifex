package gateway_iam_test

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/arn"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_iam "github.com/mulgadc/spinifex/spinifex/gateway/iam"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAccount = "123456789012"

type resolverService struct {
	handlers_iam.IAMService

	arns      map[arn.IAMResourceType]map[string]string
	lookupErr error
	key       *handlers_iam.AccessKey
	gotKind   arn.IAMResourceType
	gotName   string
}

func (s *resolverService) CanonicalResourceARN(_ string, kind arn.IAMResourceType, name string) (string, error) {
	s.gotKind, s.gotName = kind, name
	if byName := s.arns[kind]; byName != nil {
		if resource := byName[name]; resource != "" {
			return resource, nil
		}
	}
	return "", errors.New(awserrors.ErrorIAMNoSuchEntity)
}

func (s *resolverService) LookupAccessKey(_ string) (*handlers_iam.AccessKey, error) {
	if s.lookupErr != nil {
		return nil, s.lookupErr
	}
	if s.key == nil {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return s.key, nil
}

func TestResourceARNs_Fidelity(t *testing.T) {
	svc := &resolverService{arns: map[arn.IAMResourceType]map[string]string{
		arn.IAMUser:            {"alice": "arn:aws:iam::123456789012:user/teams/alice"},
		arn.IAMRole:            {"app": "arn:aws:iam::123456789012:role/service-roles/app"},
		arn.IAMGroup:           {"ops": "arn:aws:iam::123456789012:group/teams/ops"},
		arn.IAMInstanceProfile: {"nodes": "arn:aws:iam::123456789012:instance-profile/eks/nodes"},
	}}

	tests := []struct {
		name   string
		action string
		input  any
		want   string
	}{
		{"create user default path", "CreateUser", &iam.CreateUserInput{UserName: aws.String("alice")}, "arn:aws:iam::123456789012:user/alice"},
		{"create nested role", "CreateRole", &iam.CreateRoleInput{RoleName: aws.String("app"), Path: aws.String("/service-roles/")}, "arn:aws:iam::123456789012:role/service-roles/app"},
		{"existing user canonical path", "DeleteUser", &iam.DeleteUserInput{UserName: aws.String("alice")}, "arn:aws:iam::123456789012:user/teams/alice"},
		{"existing role canonical path", "DeleteRole", &iam.DeleteRoleInput{RoleName: aws.String("app")}, "arn:aws:iam::123456789012:role/service-roles/app"},
		{"existing group canonical path", "DeleteGroup", &iam.DeleteGroupInput{GroupName: aws.String("ops")}, "arn:aws:iam::123456789012:group/teams/ops"},
		{"direct managed policy ARN", "DeletePolicy", &iam.DeletePolicyInput{PolicyArn: aws.String("arn:aws:iam::123456789012:policy/team/app")}, "arn:aws:iam::123456789012:policy/team/app"},
		{"existing instance profile", "DeleteInstanceProfile", &iam.DeleteInstanceProfileInput{InstanceProfileName: aws.String("nodes")}, "arn:aws:iam::123456789012:instance-profile/eks/nodes"},
		{"OIDC create", "CreateOpenIDConnectProvider", &iam.CreateOpenIDConnectProviderInput{Url: aws.String("https://issuer.example/id/cluster")}, "arn:aws:iam::123456789012:oidc-provider/issuer.example/id/cluster"},
		{"OIDC caller account normalization", "DeleteOpenIDConnectProvider", &iam.DeleteOpenIDConnectProviderInput{OpenIDConnectProviderArn: aws.String("arn:aws:iam::999999999999:oidc-provider/issuer.example/id/cluster")}, "arn:aws:iam::123456789012:oidc-provider/issuer.example/id/cluster"},
		{"account wide", "ListUsers", &iam.ListUsersInput{}, "*"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := gateway_iam.ResourceARNs(tt.action, testAccount, tt.input, svc)
			require.NoError(t, err)
			assert.Equal(t, []string{tt.want}, got)
		})
	}
}

func TestResourceARNs_SecondaryOperandsAreNotResources(t *testing.T) {
	svc := &resolverService{arns: map[arn.IAMResourceType]map[string]string{
		arn.IAMUser:            {"alice": "arn:aws:iam::123456789012:user/alice"},
		arn.IAMRole:            {"app": "arn:aws:iam::123456789012:role/app"},
		arn.IAMGroup:           {"ops": "arn:aws:iam::123456789012:group/ops"},
		arn.IAMInstanceProfile: {"nodes": "arn:aws:iam::123456789012:instance-profile/nodes"},
	}}
	tests := []struct {
		action string
		input  any
		want   string
	}{
		{"AttachUserPolicy", &iam.AttachUserPolicyInput{UserName: aws.String("alice"), PolicyArn: aws.String("arn:aws:iam::123456789012:policy/p")}, "arn:aws:iam::123456789012:user/alice"},
		{"AttachRolePolicy", &iam.AttachRolePolicyInput{RoleName: aws.String("app"), PolicyArn: aws.String("arn:aws:iam::123456789012:policy/p")}, "arn:aws:iam::123456789012:role/app"},
		{"AttachGroupPolicy", &iam.AttachGroupPolicyInput{GroupName: aws.String("ops"), PolicyArn: aws.String("arn:aws:iam::123456789012:policy/p")}, "arn:aws:iam::123456789012:group/ops"},
		{"AddUserToGroup", &iam.AddUserToGroupInput{GroupName: aws.String("ops"), UserName: aws.String("alice")}, "arn:aws:iam::123456789012:group/ops"},
		{"AddRoleToInstanceProfile", &iam.AddRoleToInstanceProfileInput{InstanceProfileName: aws.String("nodes"), RoleName: aws.String("app")}, "arn:aws:iam::123456789012:instance-profile/nodes"},
	}
	for _, tt := range tests {
		t.Run(tt.action, func(t *testing.T) {
			got, err := gateway_iam.ResourceARNs(tt.action, testAccount, tt.input, svc)
			require.NoError(t, err)
			assert.Equal(t, []string{tt.want}, got)
		})
	}
}

func TestResourceARNs_UpdateAccessKeyUsesRecordedOwner(t *testing.T) {
	svc := &resolverService{
		arns: map[arn.IAMResourceType]map[string]string{
			arn.IAMUser: {"owner": "arn:aws:iam::123456789012:user/team/owner"},
		},
		key: &handlers_iam.AccessKey{AccountID: testAccount, UserName: "owner"},
	}
	input := &iam.UpdateAccessKeyInput{
		AccessKeyId: aws.String("AKIAEXAMPLE"),
		UserName:    aws.String("attacker-selected"),
	}
	got, err := gateway_iam.ResourceARNs("UpdateAccessKey", testAccount, input, svc)
	require.NoError(t, err)
	assert.Equal(t, []string{"arn:aws:iam::123456789012:user/team/owner"}, got)
	assert.Equal(t, "owner", svc.gotName)

	svc.key.AccountID = "999999999999"
	got, err = gateway_iam.ResourceARNs("UpdateAccessKey", testAccount, input, svc)
	require.NoError(t, err)
	assert.Equal(t, []string{"*"}, got)
}

func TestResourceARNs_MissingAndLookupErrors(t *testing.T) {
	svc := &resolverService{}

	got, err := gateway_iam.ResourceARNs("DeleteRole", testAccount, &iam.DeleteRoleInput{}, svc)
	require.NoError(t, err)
	assert.Equal(t, []string{"*"}, got)

	got, err = gateway_iam.ResourceARNs("DeleteRole", testAccount, &iam.DeleteRoleInput{RoleName: aws.String("missing")}, svc)
	require.NoError(t, err)
	assert.Equal(t, []string{"*"}, got)

	svc.arns = map[arn.IAMResourceType]map[string]string{}
	svc.lookupErr = errors.New("storage unavailable")
	_, err = gateway_iam.ResourceARNs("UpdateAccessKey", testAccount,
		&iam.UpdateAccessKeyInput{AccessKeyId: aws.String("AKIAEXAMPLE")}, svc)
	require.EqualError(t, err, awserrors.ErrorInternalError)

	_, err = gateway_iam.ResourceARNs("NotAnAction", testAccount, &iam.ListUsersInput{}, svc)
	require.EqualError(t, err, awserrors.ErrorInvalidAction)
}
