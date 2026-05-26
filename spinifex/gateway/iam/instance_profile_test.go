package gateway_iam

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateInstanceProfile(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.CreateInstanceProfileInput
		wantErr string
	}{
		{"nil InstanceProfileName", &iam.CreateInstanceProfileInput{}, awserrors.ErrorMissingParameter},
		{"empty InstanceProfileName", &iam.CreateInstanceProfileInput{InstanceProfileName: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.CreateInstanceProfileInput{InstanceProfileName: aws.String("p")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CreateInstanceProfile(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestGetInstanceProfile(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.GetInstanceProfileInput
		wantErr string
	}{
		{"nil InstanceProfileName", &iam.GetInstanceProfileInput{}, awserrors.ErrorMissingParameter},
		{"empty InstanceProfileName", &iam.GetInstanceProfileInput{InstanceProfileName: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.GetInstanceProfileInput{InstanceProfileName: aws.String("p")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := GetInstanceProfile(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestListInstanceProfiles(t *testing.T) {
	svc := &stubIAMService{}
	_, err := ListInstanceProfiles(testAccountID, &iam.ListInstanceProfilesInput{}, svc)
	require.NoError(t, err)
}

func TestDeleteInstanceProfile(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.DeleteInstanceProfileInput
		wantErr string
	}{
		{"nil InstanceProfileName", &iam.DeleteInstanceProfileInput{}, awserrors.ErrorMissingParameter},
		{"empty InstanceProfileName", &iam.DeleteInstanceProfileInput{InstanceProfileName: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.DeleteInstanceProfileInput{InstanceProfileName: aws.String("p")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DeleteInstanceProfile(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestListInstanceProfilesForRole(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.ListInstanceProfilesForRoleInput
		wantErr string
	}{
		{"nil RoleName", &iam.ListInstanceProfilesForRoleInput{}, awserrors.ErrorMissingParameter},
		{"empty RoleName", &iam.ListInstanceProfilesForRoleInput{RoleName: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.ListInstanceProfilesForRoleInput{RoleName: aws.String("r")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ListInstanceProfilesForRole(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestAddRoleToInstanceProfile(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.AddRoleToInstanceProfileInput
		wantErr string
	}{
		{"nil InstanceProfileName", &iam.AddRoleToInstanceProfileInput{RoleName: aws.String("r")}, awserrors.ErrorMissingParameter},
		{"empty InstanceProfileName", &iam.AddRoleToInstanceProfileInput{InstanceProfileName: aws.String(""), RoleName: aws.String("r")}, awserrors.ErrorMissingParameter},
		{"nil RoleName", &iam.AddRoleToInstanceProfileInput{InstanceProfileName: aws.String("p")}, awserrors.ErrorMissingParameter},
		{"empty RoleName", &iam.AddRoleToInstanceProfileInput{InstanceProfileName: aws.String("p"), RoleName: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.AddRoleToInstanceProfileInput{InstanceProfileName: aws.String("p"), RoleName: aws.String("r")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := AddRoleToInstanceProfile(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestRemoveRoleFromInstanceProfile(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.RemoveRoleFromInstanceProfileInput
		wantErr string
	}{
		{"nil InstanceProfileName", &iam.RemoveRoleFromInstanceProfileInput{RoleName: aws.String("r")}, awserrors.ErrorMissingParameter},
		{"empty InstanceProfileName", &iam.RemoveRoleFromInstanceProfileInput{InstanceProfileName: aws.String(""), RoleName: aws.String("r")}, awserrors.ErrorMissingParameter},
		{"nil RoleName", &iam.RemoveRoleFromInstanceProfileInput{InstanceProfileName: aws.String("p")}, awserrors.ErrorMissingParameter},
		{"empty RoleName", &iam.RemoveRoleFromInstanceProfileInput{InstanceProfileName: aws.String("p"), RoleName: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.RemoveRoleFromInstanceProfileInput{InstanceProfileName: aws.String("p"), RoleName: aws.String("r")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := RemoveRoleFromInstanceProfile(testAccountID, tc.input, svc)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}
