package gateway_iam

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validUserInlineDoc = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"ec2:DescribeInstances","Resource":"*"}]}`

func TestPutUserPolicy(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.PutUserPolicyInput
		wantErr string
	}{
		{"nil UserName", &iam.PutUserPolicyInput{PolicyName: aws.String("p"), PolicyDocument: aws.String(validUserInlineDoc)}, awserrors.ErrorMissingParameter},
		{"empty UserName", &iam.PutUserPolicyInput{UserName: aws.String(""), PolicyName: aws.String("p"), PolicyDocument: aws.String(validUserInlineDoc)}, awserrors.ErrorMissingParameter},
		{"nil PolicyName", &iam.PutUserPolicyInput{UserName: aws.String("u"), PolicyDocument: aws.String(validUserInlineDoc)}, awserrors.ErrorMissingParameter},
		{"empty PolicyName", &iam.PutUserPolicyInput{UserName: aws.String("u"), PolicyName: aws.String(""), PolicyDocument: aws.String(validUserInlineDoc)}, awserrors.ErrorMissingParameter},
		{"nil PolicyDocument", &iam.PutUserPolicyInput{UserName: aws.String("u"), PolicyName: aws.String("p")}, awserrors.ErrorMissingParameter},
		{"empty PolicyDocument", &iam.PutUserPolicyInput{UserName: aws.String("u"), PolicyName: aws.String("p"), PolicyDocument: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.PutUserPolicyInput{UserName: aws.String("u"), PolicyName: aws.String("p"), PolicyDocument: aws.String(validUserInlineDoc)}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PutUserPolicy(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestGetUserPolicy(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.GetUserPolicyInput
		wantErr string
	}{
		{"nil UserName", &iam.GetUserPolicyInput{PolicyName: aws.String("p")}, awserrors.ErrorMissingParameter},
		{"empty UserName", &iam.GetUserPolicyInput{UserName: aws.String(""), PolicyName: aws.String("p")}, awserrors.ErrorMissingParameter},
		{"nil PolicyName", &iam.GetUserPolicyInput{UserName: aws.String("u")}, awserrors.ErrorMissingParameter},
		{"empty PolicyName", &iam.GetUserPolicyInput{UserName: aws.String("u"), PolicyName: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.GetUserPolicyInput{UserName: aws.String("u"), PolicyName: aws.String("p")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := GetUserPolicy(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestDeleteUserPolicy(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.DeleteUserPolicyInput
		wantErr string
	}{
		{"nil UserName", &iam.DeleteUserPolicyInput{PolicyName: aws.String("p")}, awserrors.ErrorMissingParameter},
		{"empty UserName", &iam.DeleteUserPolicyInput{UserName: aws.String(""), PolicyName: aws.String("p")}, awserrors.ErrorMissingParameter},
		{"nil PolicyName", &iam.DeleteUserPolicyInput{UserName: aws.String("u")}, awserrors.ErrorMissingParameter},
		{"empty PolicyName", &iam.DeleteUserPolicyInput{UserName: aws.String("u"), PolicyName: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.DeleteUserPolicyInput{UserName: aws.String("u"), PolicyName: aws.String("p")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DeleteUserPolicy(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestListUserPolicies(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.ListUserPoliciesInput
		wantErr string
	}{
		{"nil UserName", &iam.ListUserPoliciesInput{}, awserrors.ErrorMissingParameter},
		{"empty UserName", &iam.ListUserPoliciesInput{UserName: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.ListUserPoliciesInput{UserName: aws.String("u")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ListUserPolicies(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}
