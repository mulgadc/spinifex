package gateway_iam

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const groupPolicyArn = "arn:aws:iam::000000000000:policy/mypolicy"

func TestCreateGroup(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.CreateGroupInput
		wantErr string
	}{
		{"nil GroupName", &iam.CreateGroupInput{}, awserrors.ErrorMissingParameter},
		{"empty GroupName", &iam.CreateGroupInput{GroupName: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.CreateGroupInput{GroupName: aws.String("g")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CreateGroup(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestGetGroup(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.GetGroupInput
		wantErr string
	}{
		{"nil GroupName", &iam.GetGroupInput{}, awserrors.ErrorMissingParameter},
		{"empty GroupName", &iam.GetGroupInput{GroupName: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.GetGroupInput{GroupName: aws.String("g")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := GetGroup(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestListGroups(t *testing.T) {
	svc := &stubIAMService{}
	_, err := ListGroups(testAccountID, &iam.ListGroupsInput{}, svc)
	require.NoError(t, err)
}

func TestDeleteGroup(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.DeleteGroupInput
		wantErr string
	}{
		{"nil GroupName", &iam.DeleteGroupInput{}, awserrors.ErrorMissingParameter},
		{"empty GroupName", &iam.DeleteGroupInput{GroupName: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.DeleteGroupInput{GroupName: aws.String("g")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DeleteGroup(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestAddUserToGroup(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.AddUserToGroupInput
		wantErr string
	}{
		{"nil GroupName", &iam.AddUserToGroupInput{UserName: aws.String("u")}, awserrors.ErrorMissingParameter},
		{"empty GroupName", &iam.AddUserToGroupInput{GroupName: aws.String(""), UserName: aws.String("u")}, awserrors.ErrorMissingParameter},
		{"nil UserName", &iam.AddUserToGroupInput{GroupName: aws.String("g")}, awserrors.ErrorMissingParameter},
		{"empty UserName", &iam.AddUserToGroupInput{GroupName: aws.String("g"), UserName: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.AddUserToGroupInput{GroupName: aws.String("g"), UserName: aws.String("u")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := AddUserToGroup(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestRemoveUserFromGroup(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.RemoveUserFromGroupInput
		wantErr string
	}{
		{"nil GroupName", &iam.RemoveUserFromGroupInput{UserName: aws.String("u")}, awserrors.ErrorMissingParameter},
		{"empty GroupName", &iam.RemoveUserFromGroupInput{GroupName: aws.String(""), UserName: aws.String("u")}, awserrors.ErrorMissingParameter},
		{"nil UserName", &iam.RemoveUserFromGroupInput{GroupName: aws.String("g")}, awserrors.ErrorMissingParameter},
		{"empty UserName", &iam.RemoveUserFromGroupInput{GroupName: aws.String("g"), UserName: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.RemoveUserFromGroupInput{GroupName: aws.String("g"), UserName: aws.String("u")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := RemoveUserFromGroup(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestListGroupsForUser(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.ListGroupsForUserInput
		wantErr string
	}{
		{"nil UserName", &iam.ListGroupsForUserInput{}, awserrors.ErrorMissingParameter},
		{"empty UserName", &iam.ListGroupsForUserInput{UserName: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.ListGroupsForUserInput{UserName: aws.String("u")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ListGroupsForUser(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestAttachGroupPolicy(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.AttachGroupPolicyInput
		wantErr string
	}{
		{"nil GroupName", &iam.AttachGroupPolicyInput{PolicyArn: aws.String(groupPolicyArn)}, awserrors.ErrorMissingParameter},
		{"empty GroupName", &iam.AttachGroupPolicyInput{GroupName: aws.String(""), PolicyArn: aws.String(groupPolicyArn)}, awserrors.ErrorMissingParameter},
		{"nil PolicyArn", &iam.AttachGroupPolicyInput{GroupName: aws.String("g")}, awserrors.ErrorMissingParameter},
		{"empty PolicyArn", &iam.AttachGroupPolicyInput{GroupName: aws.String("g"), PolicyArn: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.AttachGroupPolicyInput{GroupName: aws.String("g"), PolicyArn: aws.String(groupPolicyArn)}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := AttachGroupPolicy(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestDetachGroupPolicy(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.DetachGroupPolicyInput
		wantErr string
	}{
		{"nil GroupName", &iam.DetachGroupPolicyInput{PolicyArn: aws.String(groupPolicyArn)}, awserrors.ErrorMissingParameter},
		{"empty GroupName", &iam.DetachGroupPolicyInput{GroupName: aws.String(""), PolicyArn: aws.String(groupPolicyArn)}, awserrors.ErrorMissingParameter},
		{"nil PolicyArn", &iam.DetachGroupPolicyInput{GroupName: aws.String("g")}, awserrors.ErrorMissingParameter},
		{"empty PolicyArn", &iam.DetachGroupPolicyInput{GroupName: aws.String("g"), PolicyArn: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.DetachGroupPolicyInput{GroupName: aws.String("g"), PolicyArn: aws.String(groupPolicyArn)}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DetachGroupPolicy(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestListAttachedGroupPolicies(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.ListAttachedGroupPoliciesInput
		wantErr string
	}{
		{"nil GroupName", &iam.ListAttachedGroupPoliciesInput{}, awserrors.ErrorMissingParameter},
		{"empty GroupName", &iam.ListAttachedGroupPoliciesInput{GroupName: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.ListAttachedGroupPoliciesInput{GroupName: aws.String("g")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ListAttachedGroupPolicies(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

const validGroupInlineDoc = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"ec2:DescribeInstances","Resource":"*"}]}`

func TestPutGroupPolicy(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.PutGroupPolicyInput
		wantErr string
	}{
		{"nil GroupName", &iam.PutGroupPolicyInput{PolicyName: aws.String("p"), PolicyDocument: aws.String(validGroupInlineDoc)}, awserrors.ErrorMissingParameter},
		{"empty GroupName", &iam.PutGroupPolicyInput{GroupName: aws.String(""), PolicyName: aws.String("p"), PolicyDocument: aws.String(validGroupInlineDoc)}, awserrors.ErrorMissingParameter},
		{"nil PolicyName", &iam.PutGroupPolicyInput{GroupName: aws.String("g"), PolicyDocument: aws.String(validGroupInlineDoc)}, awserrors.ErrorMissingParameter},
		{"empty PolicyName", &iam.PutGroupPolicyInput{GroupName: aws.String("g"), PolicyName: aws.String(""), PolicyDocument: aws.String(validGroupInlineDoc)}, awserrors.ErrorMissingParameter},
		{"nil PolicyDocument", &iam.PutGroupPolicyInput{GroupName: aws.String("g"), PolicyName: aws.String("p")}, awserrors.ErrorMissingParameter},
		{"empty PolicyDocument", &iam.PutGroupPolicyInput{GroupName: aws.String("g"), PolicyName: aws.String("p"), PolicyDocument: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.PutGroupPolicyInput{GroupName: aws.String("g"), PolicyName: aws.String("p"), PolicyDocument: aws.String(validGroupInlineDoc)}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PutGroupPolicy(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestGetGroupPolicy(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.GetGroupPolicyInput
		wantErr string
	}{
		{"nil GroupName", &iam.GetGroupPolicyInput{PolicyName: aws.String("p")}, awserrors.ErrorMissingParameter},
		{"empty GroupName", &iam.GetGroupPolicyInput{GroupName: aws.String(""), PolicyName: aws.String("p")}, awserrors.ErrorMissingParameter},
		{"nil PolicyName", &iam.GetGroupPolicyInput{GroupName: aws.String("g")}, awserrors.ErrorMissingParameter},
		{"empty PolicyName", &iam.GetGroupPolicyInput{GroupName: aws.String("g"), PolicyName: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.GetGroupPolicyInput{GroupName: aws.String("g"), PolicyName: aws.String("p")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := GetGroupPolicy(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestDeleteGroupPolicy(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.DeleteGroupPolicyInput
		wantErr string
	}{
		{"nil GroupName", &iam.DeleteGroupPolicyInput{PolicyName: aws.String("p")}, awserrors.ErrorMissingParameter},
		{"empty GroupName", &iam.DeleteGroupPolicyInput{GroupName: aws.String(""), PolicyName: aws.String("p")}, awserrors.ErrorMissingParameter},
		{"nil PolicyName", &iam.DeleteGroupPolicyInput{GroupName: aws.String("g")}, awserrors.ErrorMissingParameter},
		{"empty PolicyName", &iam.DeleteGroupPolicyInput{GroupName: aws.String("g"), PolicyName: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.DeleteGroupPolicyInput{GroupName: aws.String("g"), PolicyName: aws.String("p")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DeleteGroupPolicy(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestListGroupPolicies(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.ListGroupPoliciesInput
		wantErr string
	}{
		{"nil GroupName", &iam.ListGroupPoliciesInput{}, awserrors.ErrorMissingParameter},
		{"empty GroupName", &iam.ListGroupPoliciesInput{GroupName: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.ListGroupPoliciesInput{GroupName: aws.String("g")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ListGroupPolicies(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}
