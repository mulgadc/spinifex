package gateway_iam

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	testTags    = []*iam.Tag{{Key: aws.String("env"), Value: aws.String("dev")}}
	testTagKeys = []*string{aws.String("env")}
)

func checkTagValidator(t *testing.T, wantErr string, err error) {
	t.Helper()
	if wantErr != "" {
		require.Error(t, err)
		assert.Equal(t, wantErr, err.Error())
	} else {
		require.NoError(t, err)
	}
}

func TestTagUser(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.TagUserInput
		wantErr string
	}{
		{"nil UserName", &iam.TagUserInput{Tags: testTags}, awserrors.ErrorMissingParameter},
		{"empty UserName", &iam.TagUserInput{UserName: aws.String(""), Tags: testTags}, awserrors.ErrorMissingParameter},
		{"missing Tags", &iam.TagUserInput{UserName: aws.String("alice")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.TagUserInput{UserName: aws.String("alice"), Tags: testTags}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := TagUser(testAccountID, tc.input, svc)
			checkTagValidator(t, tc.wantErr, err)
		})
	}
}

func TestUntagUser(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.UntagUserInput
		wantErr string
	}{
		{"nil UserName", &iam.UntagUserInput{TagKeys: testTagKeys}, awserrors.ErrorMissingParameter},
		{"empty UserName", &iam.UntagUserInput{UserName: aws.String(""), TagKeys: testTagKeys}, awserrors.ErrorMissingParameter},
		{"missing TagKeys", &iam.UntagUserInput{UserName: aws.String("alice")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.UntagUserInput{UserName: aws.String("alice"), TagKeys: testTagKeys}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := UntagUser(testAccountID, tc.input, svc)
			checkTagValidator(t, tc.wantErr, err)
		})
	}
}

func TestListUserTags(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.ListUserTagsInput
		wantErr string
	}{
		{"nil UserName", &iam.ListUserTagsInput{}, awserrors.ErrorMissingParameter},
		{"empty UserName", &iam.ListUserTagsInput{UserName: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.ListUserTagsInput{UserName: aws.String("alice")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ListUserTags(testAccountID, tc.input, svc)
			checkTagValidator(t, tc.wantErr, err)
		})
	}
}

func TestTagRole(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.TagRoleInput
		wantErr string
	}{
		{"nil RoleName", &iam.TagRoleInput{Tags: testTags}, awserrors.ErrorMissingParameter},
		{"empty RoleName", &iam.TagRoleInput{RoleName: aws.String(""), Tags: testTags}, awserrors.ErrorMissingParameter},
		{"missing Tags", &iam.TagRoleInput{RoleName: aws.String("myrole")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.TagRoleInput{RoleName: aws.String("myrole"), Tags: testTags}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := TagRole(testAccountID, tc.input, svc)
			checkTagValidator(t, tc.wantErr, err)
		})
	}
}

func TestUntagRole(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.UntagRoleInput
		wantErr string
	}{
		{"nil RoleName", &iam.UntagRoleInput{TagKeys: testTagKeys}, awserrors.ErrorMissingParameter},
		{"empty RoleName", &iam.UntagRoleInput{RoleName: aws.String(""), TagKeys: testTagKeys}, awserrors.ErrorMissingParameter},
		{"missing TagKeys", &iam.UntagRoleInput{RoleName: aws.String("myrole")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.UntagRoleInput{RoleName: aws.String("myrole"), TagKeys: testTagKeys}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := UntagRole(testAccountID, tc.input, svc)
			checkTagValidator(t, tc.wantErr, err)
		})
	}
}

func TestListRoleTags(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.ListRoleTagsInput
		wantErr string
	}{
		{"nil RoleName", &iam.ListRoleTagsInput{}, awserrors.ErrorMissingParameter},
		{"empty RoleName", &iam.ListRoleTagsInput{RoleName: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.ListRoleTagsInput{RoleName: aws.String("myrole")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ListRoleTags(testAccountID, tc.input, svc)
			checkTagValidator(t, tc.wantErr, err)
		})
	}
}

func TestTagPolicy(t *testing.T) {
	svc := &stubIAMService{}
	arn := "arn:aws:iam::000000000000:policy/mypolicy"
	tests := []struct {
		name    string
		input   *iam.TagPolicyInput
		wantErr string
	}{
		{"nil PolicyArn", &iam.TagPolicyInput{Tags: testTags}, awserrors.ErrorMissingParameter},
		{"empty PolicyArn", &iam.TagPolicyInput{PolicyArn: aws.String(""), Tags: testTags}, awserrors.ErrorMissingParameter},
		{"missing Tags", &iam.TagPolicyInput{PolicyArn: aws.String(arn)}, awserrors.ErrorMissingParameter},
		{"valid", &iam.TagPolicyInput{PolicyArn: aws.String(arn), Tags: testTags}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := TagPolicy(testAccountID, tc.input, svc)
			checkTagValidator(t, tc.wantErr, err)
		})
	}
}

func TestUntagPolicy(t *testing.T) {
	svc := &stubIAMService{}
	arn := "arn:aws:iam::000000000000:policy/mypolicy"
	tests := []struct {
		name    string
		input   *iam.UntagPolicyInput
		wantErr string
	}{
		{"nil PolicyArn", &iam.UntagPolicyInput{TagKeys: testTagKeys}, awserrors.ErrorMissingParameter},
		{"empty PolicyArn", &iam.UntagPolicyInput{PolicyArn: aws.String(""), TagKeys: testTagKeys}, awserrors.ErrorMissingParameter},
		{"missing TagKeys", &iam.UntagPolicyInput{PolicyArn: aws.String(arn)}, awserrors.ErrorMissingParameter},
		{"valid", &iam.UntagPolicyInput{PolicyArn: aws.String(arn), TagKeys: testTagKeys}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := UntagPolicy(testAccountID, tc.input, svc)
			checkTagValidator(t, tc.wantErr, err)
		})
	}
}

func TestListPolicyTags(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.ListPolicyTagsInput
		wantErr string
	}{
		{"nil PolicyArn", &iam.ListPolicyTagsInput{}, awserrors.ErrorMissingParameter},
		{"empty PolicyArn", &iam.ListPolicyTagsInput{PolicyArn: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.ListPolicyTagsInput{PolicyArn: aws.String("arn:aws:iam::000000000000:policy/mypolicy")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ListPolicyTags(testAccountID, tc.input, svc)
			checkTagValidator(t, tc.wantErr, err)
		})
	}
}

func TestTagInstanceProfile(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.TagInstanceProfileInput
		wantErr string
	}{
		{"nil InstanceProfileName", &iam.TagInstanceProfileInput{Tags: testTags}, awserrors.ErrorMissingParameter},
		{"empty InstanceProfileName", &iam.TagInstanceProfileInput{InstanceProfileName: aws.String(""), Tags: testTags}, awserrors.ErrorMissingParameter},
		{"missing Tags", &iam.TagInstanceProfileInput{InstanceProfileName: aws.String("myprofile")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.TagInstanceProfileInput{InstanceProfileName: aws.String("myprofile"), Tags: testTags}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := TagInstanceProfile(testAccountID, tc.input, svc)
			checkTagValidator(t, tc.wantErr, err)
		})
	}
}

func TestUntagInstanceProfile(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.UntagInstanceProfileInput
		wantErr string
	}{
		{"nil InstanceProfileName", &iam.UntagInstanceProfileInput{TagKeys: testTagKeys}, awserrors.ErrorMissingParameter},
		{"empty InstanceProfileName", &iam.UntagInstanceProfileInput{InstanceProfileName: aws.String(""), TagKeys: testTagKeys}, awserrors.ErrorMissingParameter},
		{"missing TagKeys", &iam.UntagInstanceProfileInput{InstanceProfileName: aws.String("myprofile")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.UntagInstanceProfileInput{InstanceProfileName: aws.String("myprofile"), TagKeys: testTagKeys}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := UntagInstanceProfile(testAccountID, tc.input, svc)
			checkTagValidator(t, tc.wantErr, err)
		})
	}
}

func TestListInstanceProfileTags(t *testing.T) {
	svc := &stubIAMService{}
	tests := []struct {
		name    string
		input   *iam.ListInstanceProfileTagsInput
		wantErr string
	}{
		{"nil InstanceProfileName", &iam.ListInstanceProfileTagsInput{}, awserrors.ErrorMissingParameter},
		{"empty InstanceProfileName", &iam.ListInstanceProfileTagsInput{InstanceProfileName: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.ListInstanceProfileTagsInput{InstanceProfileName: aws.String("myprofile")}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ListInstanceProfileTags(testAccountID, tc.input, svc)
			checkTagValidator(t, tc.wantErr, err)
		})
	}
}

func TestTagOpenIDConnectProvider(t *testing.T) {
	svc := &stubIAMService{}
	arn := "arn:aws:iam::000000000000:oidc-provider/oidc.example.com"
	tests := []struct {
		name    string
		input   *iam.TagOpenIDConnectProviderInput
		wantErr string
	}{
		{"nil Arn", &iam.TagOpenIDConnectProviderInput{Tags: testTags}, awserrors.ErrorMissingParameter},
		{"empty Arn", &iam.TagOpenIDConnectProviderInput{OpenIDConnectProviderArn: aws.String(""), Tags: testTags}, awserrors.ErrorMissingParameter},
		{"missing Tags", &iam.TagOpenIDConnectProviderInput{OpenIDConnectProviderArn: aws.String(arn)}, awserrors.ErrorMissingParameter},
		{"valid", &iam.TagOpenIDConnectProviderInput{OpenIDConnectProviderArn: aws.String(arn), Tags: testTags}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := TagOpenIDConnectProvider(testAccountID, tc.input, svc)
			checkTagValidator(t, tc.wantErr, err)
		})
	}
}

func TestUntagOpenIDConnectProvider(t *testing.T) {
	svc := &stubIAMService{}
	arn := "arn:aws:iam::000000000000:oidc-provider/oidc.example.com"
	tests := []struct {
		name    string
		input   *iam.UntagOpenIDConnectProviderInput
		wantErr string
	}{
		{"nil Arn", &iam.UntagOpenIDConnectProviderInput{TagKeys: testTagKeys}, awserrors.ErrorMissingParameter},
		{"empty Arn", &iam.UntagOpenIDConnectProviderInput{OpenIDConnectProviderArn: aws.String(""), TagKeys: testTagKeys}, awserrors.ErrorMissingParameter},
		{"missing TagKeys", &iam.UntagOpenIDConnectProviderInput{OpenIDConnectProviderArn: aws.String(arn)}, awserrors.ErrorMissingParameter},
		{"valid", &iam.UntagOpenIDConnectProviderInput{OpenIDConnectProviderArn: aws.String(arn), TagKeys: testTagKeys}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := UntagOpenIDConnectProvider(testAccountID, tc.input, svc)
			checkTagValidator(t, tc.wantErr, err)
		})
	}
}

func TestListOpenIDConnectProviderTags(t *testing.T) {
	svc := &stubIAMService{}
	arn := "arn:aws:iam::000000000000:oidc-provider/oidc.example.com"
	tests := []struct {
		name    string
		input   *iam.ListOpenIDConnectProviderTagsInput
		wantErr string
	}{
		{"nil Arn", &iam.ListOpenIDConnectProviderTagsInput{}, awserrors.ErrorMissingParameter},
		{"empty Arn", &iam.ListOpenIDConnectProviderTagsInput{OpenIDConnectProviderArn: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid", &iam.ListOpenIDConnectProviderTagsInput{OpenIDConnectProviderArn: aws.String(arn)}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ListOpenIDConnectProviderTags(testAccountID, tc.input, svc)
			checkTagValidator(t, tc.wantErr, err)
		})
	}
}
