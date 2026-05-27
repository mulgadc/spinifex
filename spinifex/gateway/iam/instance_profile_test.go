package gateway_iam

import (
	"errors"
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
		name      string
		input     *iam.DeleteInstanceProfileInput
		countLive LiveAssociationCounter
		wantErr   string
	}{
		{"nil InstanceProfileName", &iam.DeleteInstanceProfileInput{}, nil, awserrors.ErrorMissingParameter},
		{"empty InstanceProfileName", &iam.DeleteInstanceProfileInput{InstanceProfileName: aws.String("")}, nil, awserrors.ErrorMissingParameter},
		{"valid no counter", &iam.DeleteInstanceProfileInput{InstanceProfileName: aws.String("p")}, nil, ""},
		{
			"valid with zero live associations",
			&iam.DeleteInstanceProfileInput{InstanceProfileName: aws.String("p")},
			func(string) (int, error) { return 0, nil },
			"",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DeleteInstanceProfile(testAccountID, tc.input, svc, tc.countLive)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestDeleteInstanceProfile_LiveAssociationsRefused(t *testing.T) {
	svc := &stubIAMService{
		getInstanceProfile: func(_ string, in *iam.GetInstanceProfileInput) (*iam.GetInstanceProfileOutput, error) {
			return &iam.GetInstanceProfileOutput{
				InstanceProfile: &iam.InstanceProfile{
					InstanceProfileName: in.InstanceProfileName,
					Arn:                 aws.String("arn:aws:iam::000000000000:instance-profile/p"),
				},
			}, nil
		},
	}
	gotARN := ""
	countLive := func(arn string) (int, error) {
		gotARN = arn
		return 2, nil
	}
	_, err := DeleteInstanceProfile(testAccountID, &iam.DeleteInstanceProfileInput{
		InstanceProfileName: aws.String("p"),
	}, svc, countLive)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorIAMDeleteConflict, err.Error())
	assert.Equal(t, "arn:aws:iam::000000000000:instance-profile/p", gotARN)
}

func TestDeleteInstanceProfile_CounterError(t *testing.T) {
	svc := &stubIAMService{
		getInstanceProfile: func(_ string, in *iam.GetInstanceProfileInput) (*iam.GetInstanceProfileOutput, error) {
			return &iam.GetInstanceProfileOutput{
				InstanceProfile: &iam.InstanceProfile{
					InstanceProfileName: in.InstanceProfileName,
					Arn:                 aws.String("arn:aws:iam::000000000000:instance-profile/p"),
				},
			}, nil
		},
	}
	wantErr := errors.New("nats unavailable")
	_, err := DeleteInstanceProfile(testAccountID, &iam.DeleteInstanceProfileInput{
		InstanceProfileName: aws.String("p"),
	}, svc, func(string) (int, error) { return 0, wantErr })
	require.ErrorIs(t, err, wantErr)
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
