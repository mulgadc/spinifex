package gateway_iam

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validTrustDoc = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`

func TestCreateRole(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.CreateRoleInput
		wantErr string
	}{
		{"nil RoleName", &iam.CreateRoleInput{AssumeRolePolicyDocument: aws.String(validTrustDoc)}, awserrors.ErrorMissingParameter},
		{"empty RoleName", &iam.CreateRoleInput{RoleName: aws.String(""), AssumeRolePolicyDocument: aws.String(validTrustDoc)}, awserrors.ErrorMissingParameter},
		{"nil TrustPolicy", &iam.CreateRoleInput{RoleName: aws.String("r")}, awserrors.ErrorMissingParameter},
		{"empty TrustPolicy", &iam.CreateRoleInput{RoleName: aws.String("r"), AssumeRolePolicyDocument: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.CreateRoleInput{RoleName: aws.String("r"), AssumeRolePolicyDocument: aws.String(validTrustDoc)}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CreateRole(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestGetRole(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.GetRoleInput
		wantErr string
	}{
		{"nil RoleName", &iam.GetRoleInput{}, awserrors.ErrorMissingParameter},
		{"empty RoleName", &iam.GetRoleInput{RoleName: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.GetRoleInput{RoleName: aws.String("r")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := GetRole(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestListRoles(t *testing.T) {
	svc := &stubIAMService{}
	_, err := ListRoles(testAccountID, &iam.ListRolesInput{}, svc)
	require.NoError(t, err)
}

func TestDeleteRole(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.DeleteRoleInput
		wantErr string
	}{
		{"nil RoleName", &iam.DeleteRoleInput{}, awserrors.ErrorMissingParameter},
		{"empty RoleName", &iam.DeleteRoleInput{RoleName: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.DeleteRoleInput{RoleName: aws.String("r")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DeleteRole(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestUpdateRole(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.UpdateRoleInput
		wantErr string
	}{
		{"nil RoleName", &iam.UpdateRoleInput{Description: aws.String("d")}, awserrors.ErrorMissingParameter},
		{"empty RoleName", &iam.UpdateRoleInput{RoleName: aws.String(""), Description: aws.String("d")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.UpdateRoleInput{RoleName: aws.String("r"), Description: aws.String("d")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := UpdateRole(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestUpdateAssumeRolePolicy(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.UpdateAssumeRolePolicyInput
		wantErr string
	}{
		{"nil RoleName", &iam.UpdateAssumeRolePolicyInput{PolicyDocument: aws.String(validTrustDoc)}, awserrors.ErrorMissingParameter},
		{"empty RoleName", &iam.UpdateAssumeRolePolicyInput{RoleName: aws.String(""), PolicyDocument: aws.String(validTrustDoc)}, awserrors.ErrorMissingParameter},
		{"nil PolicyDocument", &iam.UpdateAssumeRolePolicyInput{RoleName: aws.String("r")}, awserrors.ErrorMissingParameter},
		{"empty PolicyDocument", &iam.UpdateAssumeRolePolicyInput{RoleName: aws.String("r"), PolicyDocument: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.UpdateAssumeRolePolicyInput{RoleName: aws.String("r"), PolicyDocument: aws.String(validTrustDoc)}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := UpdateAssumeRolePolicy(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestAttachRolePolicy(t *testing.T) {
	svc := &stubIAMService{}
	arn := "arn:aws:iam::000000000000:policy/mypolicy"
	tests := []struct {
		name    string
		input   *iam.AttachRolePolicyInput
		wantErr string
	}{
		{"nil RoleName", &iam.AttachRolePolicyInput{PolicyArn: aws.String(arn)}, awserrors.ErrorMissingParameter},
		{"empty RoleName", &iam.AttachRolePolicyInput{RoleName: aws.String(""), PolicyArn: aws.String(arn)}, awserrors.ErrorMissingParameter},
		{"nil PolicyArn", &iam.AttachRolePolicyInput{RoleName: aws.String("r")}, awserrors.ErrorMissingParameter},
		{"empty PolicyArn", &iam.AttachRolePolicyInput{RoleName: aws.String("r"), PolicyArn: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.AttachRolePolicyInput{RoleName: aws.String("r"), PolicyArn: aws.String(arn)}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := AttachRolePolicy(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestDetachRolePolicy(t *testing.T) {
	svc := &stubIAMService{}
	arn := "arn:aws:iam::000000000000:policy/mypolicy"
	tests := []struct {
		name    string
		input   *iam.DetachRolePolicyInput
		wantErr string
	}{
		{"nil RoleName", &iam.DetachRolePolicyInput{PolicyArn: aws.String(arn)}, awserrors.ErrorMissingParameter},
		{"empty RoleName", &iam.DetachRolePolicyInput{RoleName: aws.String(""), PolicyArn: aws.String(arn)}, awserrors.ErrorMissingParameter},
		{"nil PolicyArn", &iam.DetachRolePolicyInput{RoleName: aws.String("r")}, awserrors.ErrorMissingParameter},
		{"empty PolicyArn", &iam.DetachRolePolicyInput{RoleName: aws.String("r"), PolicyArn: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.DetachRolePolicyInput{RoleName: aws.String("r"), PolicyArn: aws.String(arn)}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DetachRolePolicy(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestListAttachedRolePolicies(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.ListAttachedRolePoliciesInput
		wantErr string
	}{
		{"nil RoleName", &iam.ListAttachedRolePoliciesInput{}, awserrors.ErrorMissingParameter},
		{"empty RoleName", &iam.ListAttachedRolePoliciesInput{RoleName: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.ListAttachedRolePoliciesInput{RoleName: aws.String("r")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ListAttachedRolePolicies(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}
