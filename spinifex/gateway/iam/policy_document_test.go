package gateway_iam_test

import (
	"net/url"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	gateway_iam "github.com/mulgadc/spinifex/spinifex/gateway/iam"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rawDoc holds the characters a decode-once client would corrupt if the
// document went out unencoded: '+' reads as space, "%41" as "A", a lone '%' fails.
const (
	rawDoc     = `{"Sid":"a+b%41c d/x:y %"}`
	encodedDoc = `%7B%22Sid%22%3A%22a%2Bb%2541c%20d%2Fx%3Ay%20%25%22%7D`
)

// rawDocIAMService answers every document-bearing read with rawDoc, as the
// service layer stores and returns it.
type rawDocIAMService struct {
	handlers_iam.IAMService
}

func rawDocRole() *iam.Role {
	return &iam.Role{RoleName: aws.String("r"), AssumeRolePolicyDocument: aws.String(rawDoc)}
}

func rawDocProfile() *iam.InstanceProfile {
	return &iam.InstanceProfile{Roles: []*iam.Role{rawDocRole()}}
}

func (rawDocIAMService) CreateRole(string, *iam.CreateRoleInput) (*iam.CreateRoleOutput, error) {
	return &iam.CreateRoleOutput{Role: rawDocRole()}, nil
}
func (rawDocIAMService) GetRole(string, *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
	return &iam.GetRoleOutput{Role: rawDocRole()}, nil
}
func (rawDocIAMService) ListRoles(string, *iam.ListRolesInput) (*iam.ListRolesOutput, error) {
	return &iam.ListRolesOutput{Roles: []*iam.Role{rawDocRole(), rawDocRole()}}, nil
}
func (rawDocIAMService) GetRolePolicy(string, *iam.GetRolePolicyInput) (*iam.GetRolePolicyOutput, error) {
	return &iam.GetRolePolicyOutput{PolicyDocument: aws.String(rawDoc)}, nil
}
func (rawDocIAMService) GetUserPolicy(string, *iam.GetUserPolicyInput) (*iam.GetUserPolicyOutput, error) {
	return &iam.GetUserPolicyOutput{PolicyDocument: aws.String(rawDoc)}, nil
}
func (rawDocIAMService) GetGroupPolicy(string, *iam.GetGroupPolicyInput) (*iam.GetGroupPolicyOutput, error) {
	return &iam.GetGroupPolicyOutput{PolicyDocument: aws.String(rawDoc)}, nil
}
func (rawDocIAMService) GetPolicyVersion(string, *iam.GetPolicyVersionInput) (*iam.GetPolicyVersionOutput, error) {
	return &iam.GetPolicyVersionOutput{PolicyVersion: &iam.PolicyVersion{Document: aws.String(rawDoc)}}, nil
}
func (rawDocIAMService) GetInstanceProfile(string, *iam.GetInstanceProfileInput) (*iam.GetInstanceProfileOutput, error) {
	return &iam.GetInstanceProfileOutput{InstanceProfile: rawDocProfile()}, nil
}
func (rawDocIAMService) ListInstanceProfiles(string, *iam.ListInstanceProfilesInput) (*iam.ListInstanceProfilesOutput, error) {
	return &iam.ListInstanceProfilesOutput{InstanceProfiles: []*iam.InstanceProfile{rawDocProfile(), rawDocProfile()}}, nil
}
func (rawDocIAMService) ListInstanceProfilesForRole(string, *iam.ListInstanceProfilesForRoleInput) (*iam.ListInstanceProfilesForRoleOutput, error) {
	return &iam.ListInstanceProfilesForRoleOutput{InstanceProfiles: []*iam.InstanceProfile{rawDocProfile()}}, nil
}

func roleDocs(roles ...*iam.Role) []*string {
	var docs []*string
	for _, r := range roles {
		docs = append(docs, r.AssumeRolePolicyDocument)
	}
	return docs
}

func profileDocs(profiles ...*iam.InstanceProfile) []*string {
	var docs []*string
	for _, p := range profiles {
		docs = append(docs, roleDocs(p.Roles...)...)
	}
	return docs
}

// TestPolicyDocumentsAreURLEncoded pins the percent-encoding AWS applies to
// every document it returns over the Query API.
func TestPolicyDocumentsAreURLEncoded(t *testing.T) {
	svc := rawDocIAMService{}
	name := aws.String("n")
	tests := []struct {
		action string
		call   func() ([]*string, error)
	}{
		{"CreateRole", func() ([]*string, error) {
			out, err := gateway_iam.CreateRole(testAccountID, &iam.CreateRoleInput{RoleName: name, AssumeRolePolicyDocument: aws.String(rawDoc)}, svc)
			if err != nil {
				return nil, err
			}
			return roleDocs(out.Role), nil
		}},
		{"GetRole", func() ([]*string, error) {
			out, err := gateway_iam.GetRole(testAccountID, &iam.GetRoleInput{RoleName: name}, svc)
			if err != nil {
				return nil, err
			}
			return roleDocs(out.Role), nil
		}},
		{"ListRoles", func() ([]*string, error) {
			out, err := gateway_iam.ListRoles(testAccountID, &iam.ListRolesInput{}, svc)
			if err != nil {
				return nil, err
			}
			return roleDocs(out.Roles...), nil
		}},
		{"GetRolePolicy", func() ([]*string, error) {
			out, err := gateway_iam.GetRolePolicy(testAccountID, &iam.GetRolePolicyInput{RoleName: name, PolicyName: name}, svc)
			if err != nil {
				return nil, err
			}
			return []*string{out.PolicyDocument}, nil
		}},
		{"GetUserPolicy", func() ([]*string, error) {
			out, err := gateway_iam.GetUserPolicy(testAccountID, &iam.GetUserPolicyInput{UserName: name, PolicyName: name}, svc)
			if err != nil {
				return nil, err
			}
			return []*string{out.PolicyDocument}, nil
		}},
		{"GetGroupPolicy", func() ([]*string, error) {
			out, err := gateway_iam.GetGroupPolicy(testAccountID, &iam.GetGroupPolicyInput{GroupName: name, PolicyName: name}, svc)
			if err != nil {
				return nil, err
			}
			return []*string{out.PolicyDocument}, nil
		}},
		{"GetPolicyVersion", func() ([]*string, error) {
			out, err := gateway_iam.GetPolicyVersion(testAccountID, &iam.GetPolicyVersionInput{PolicyArn: name, VersionId: aws.String("v1")}, svc)
			if err != nil {
				return nil, err
			}
			return []*string{out.PolicyVersion.Document}, nil
		}},
		{"GetInstanceProfile", func() ([]*string, error) {
			out, err := gateway_iam.GetInstanceProfile(testAccountID, &iam.GetInstanceProfileInput{InstanceProfileName: name}, svc)
			if err != nil {
				return nil, err
			}
			return profileDocs(out.InstanceProfile), nil
		}},
		{"ListInstanceProfiles", func() ([]*string, error) {
			out, err := gateway_iam.ListInstanceProfiles(testAccountID, &iam.ListInstanceProfilesInput{}, svc)
			if err != nil {
				return nil, err
			}
			return profileDocs(out.InstanceProfiles...), nil
		}},
		{"ListInstanceProfilesForRole", func() ([]*string, error) {
			out, err := gateway_iam.ListInstanceProfilesForRole(testAccountID, &iam.ListInstanceProfilesForRoleInput{RoleName: name}, svc)
			if err != nil {
				return nil, err
			}
			return profileDocs(out.InstanceProfiles...), nil
		}},
	}
	for _, tc := range tests {
		t.Run(tc.action, func(t *testing.T) {
			docs, err := tc.call()
			require.NoError(t, err)
			require.NotEmpty(t, docs)
			for _, doc := range docs {
				require.NotNil(t, doc)
				assert.Equal(t, encodedDoc, *doc)
				decoded, err := url.QueryUnescape(*doc)
				require.NoError(t, err)
				assert.Equal(t, rawDoc, decoded, "a client decoding once must recover the stored document")
			}
		})
	}
}
