package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/stretchr/testify/assert"
)

// flexMockIAMService is a configurable mock with per-method overrides.
type flexMockIAMService struct {
	createUserFn      func(string, *iam.CreateUserInput) (*iam.CreateUserOutput, error)
	getUserFn         func(string, *iam.GetUserInput) (*iam.GetUserOutput, error)
	listUsersFn       func(string, *iam.ListUsersInput) (*iam.ListUsersOutput, error)
	deleteUserFn      func(string, *iam.DeleteUserInput) (*iam.DeleteUserOutput, error)
	createAccessKeyFn func(string, *iam.CreateAccessKeyInput) (*iam.CreateAccessKeyOutput, error)
	listAccessKeysFn  func(string, *iam.ListAccessKeysInput) (*iam.ListAccessKeysOutput, error)
	deleteAccessKeyFn func(string, *iam.DeleteAccessKeyInput) (*iam.DeleteAccessKeyOutput, error)
	updateAccessKeyFn func(string, *iam.UpdateAccessKeyInput) (*iam.UpdateAccessKeyOutput, error)
}

func (m *flexMockIAMService) CreateUser(accountID string, input *iam.CreateUserInput) (*iam.CreateUserOutput, error) {
	if m.createUserFn != nil {
		return m.createUserFn(accountID, input)
	}
	return &iam.CreateUserOutput{}, nil
}

func (m *flexMockIAMService) GetUser(accountID string, input *iam.GetUserInput) (*iam.GetUserOutput, error) {
	if m.getUserFn != nil {
		return m.getUserFn(accountID, input)
	}
	return &iam.GetUserOutput{}, nil
}

func (m *flexMockIAMService) ListUsers(accountID string, input *iam.ListUsersInput) (*iam.ListUsersOutput, error) {
	if m.listUsersFn != nil {
		return m.listUsersFn(accountID, input)
	}
	return &iam.ListUsersOutput{}, nil
}

func (m *flexMockIAMService) DeleteUser(accountID string, input *iam.DeleteUserInput) (*iam.DeleteUserOutput, error) {
	if m.deleteUserFn != nil {
		return m.deleteUserFn(accountID, input)
	}
	return &iam.DeleteUserOutput{}, nil
}

func (m *flexMockIAMService) CreateAccessKey(accountID string, input *iam.CreateAccessKeyInput) (*iam.CreateAccessKeyOutput, error) {
	if m.createAccessKeyFn != nil {
		return m.createAccessKeyFn(accountID, input)
	}
	return &iam.CreateAccessKeyOutput{}, nil
}

func (m *flexMockIAMService) ListAccessKeys(accountID string, input *iam.ListAccessKeysInput) (*iam.ListAccessKeysOutput, error) {
	if m.listAccessKeysFn != nil {
		return m.listAccessKeysFn(accountID, input)
	}
	return &iam.ListAccessKeysOutput{}, nil
}

func (m *flexMockIAMService) DeleteAccessKey(accountID string, input *iam.DeleteAccessKeyInput) (*iam.DeleteAccessKeyOutput, error) {
	if m.deleteAccessKeyFn != nil {
		return m.deleteAccessKeyFn(accountID, input)
	}
	return &iam.DeleteAccessKeyOutput{}, nil
}

func (m *flexMockIAMService) UpdateAccessKey(accountID string, input *iam.UpdateAccessKeyInput) (*iam.UpdateAccessKeyOutput, error) {
	if m.updateAccessKeyFn != nil {
		return m.updateAccessKeyFn(accountID, input)
	}
	return &iam.UpdateAccessKeyOutput{}, nil
}

func (m *flexMockIAMService) CreatePolicy(_ string, _ *iam.CreatePolicyInput) (*iam.CreatePolicyOutput, error) {
	return &iam.CreatePolicyOutput{}, nil
}

func (m *flexMockIAMService) GetPolicy(_ string, _ *iam.GetPolicyInput) (*iam.GetPolicyOutput, error) {
	return &iam.GetPolicyOutput{}, nil
}

func (m *flexMockIAMService) GetPolicyVersion(_ string, _ *iam.GetPolicyVersionInput) (*iam.GetPolicyVersionOutput, error) {
	return &iam.GetPolicyVersionOutput{}, nil
}

func (m *flexMockIAMService) ListPolicyVersions(_ string, _ *iam.ListPolicyVersionsInput) (*iam.ListPolicyVersionsOutput, error) {
	return &iam.ListPolicyVersionsOutput{}, nil
}

func (m *flexMockIAMService) ListPolicies(_ string, _ *iam.ListPoliciesInput) (*iam.ListPoliciesOutput, error) {
	return &iam.ListPoliciesOutput{}, nil
}

func (m *flexMockIAMService) DeletePolicy(_ string, _ *iam.DeletePolicyInput) (*iam.DeletePolicyOutput, error) {
	return &iam.DeletePolicyOutput{}, nil
}

func (m *flexMockIAMService) AttachUserPolicy(_ string, _ *iam.AttachUserPolicyInput) (*iam.AttachUserPolicyOutput, error) {
	return &iam.AttachUserPolicyOutput{}, nil
}

func (m *flexMockIAMService) DetachUserPolicy(_ string, _ *iam.DetachUserPolicyInput) (*iam.DetachUserPolicyOutput, error) {
	return &iam.DetachUserPolicyOutput{}, nil
}

func (m *flexMockIAMService) ListAttachedUserPolicies(_ string, _ *iam.ListAttachedUserPoliciesInput) (*iam.ListAttachedUserPoliciesOutput, error) {
	return &iam.ListAttachedUserPoliciesOutput{}, nil
}

func (m *flexMockIAMService) GetUserPolicies(_, _ string) ([]handlers_iam.PolicyDocument, error) {
	return nil, nil
}

func (m *flexMockIAMService) GetRolePolicies(_, _ string) ([]handlers_iam.PolicyDocument, error) {
	return nil, nil
}

func (m *flexMockIAMService) LookupAccessKey(_ string) (*handlers_iam.AccessKey, error) {
	return nil, errors.New("not implemented")
}

func (m *flexMockIAMService) DecryptSecret(_ string) (string, error)            { return "", nil }
func (m *flexMockIAMService) SeedBootstrap(_ *handlers_iam.BootstrapData) error { return nil }
func (m *flexMockIAMService) IsEmpty() (bool, error)                            { return true, nil }

func (m *flexMockIAMService) CreateAccount(_ string) (*handlers_iam.Account, error) {
	return nil, nil
}
func (m *flexMockIAMService) GetAccount(_ string) (*handlers_iam.Account, error) { return nil, nil }
func (m *flexMockIAMService) ListAccounts() ([]*handlers_iam.Account, error)     { return nil, nil }

func (m *flexMockIAMService) CreateRole(_ string, _ *iam.CreateRoleInput) (*iam.CreateRoleOutput, error) {
	return &iam.CreateRoleOutput{}, nil
}
func (m *flexMockIAMService) GetRole(_ string, _ *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
	return &iam.GetRoleOutput{}, nil
}
func (m *flexMockIAMService) ListRoles(_ string, _ *iam.ListRolesInput) (*iam.ListRolesOutput, error) {
	return &iam.ListRolesOutput{}, nil
}
func (m *flexMockIAMService) DeleteRole(_ string, _ *iam.DeleteRoleInput) (*iam.DeleteRoleOutput, error) {
	return &iam.DeleteRoleOutput{}, nil
}
func (m *flexMockIAMService) UpdateRole(_ string, _ *iam.UpdateRoleInput) (*iam.UpdateRoleOutput, error) {
	return &iam.UpdateRoleOutput{}, nil
}
func (m *flexMockIAMService) UpdateAssumeRolePolicy(_ string, _ *iam.UpdateAssumeRolePolicyInput) (*iam.UpdateAssumeRolePolicyOutput, error) {
	return &iam.UpdateAssumeRolePolicyOutput{}, nil
}
func (m *flexMockIAMService) AttachRolePolicy(_ string, _ *iam.AttachRolePolicyInput) (*iam.AttachRolePolicyOutput, error) {
	return &iam.AttachRolePolicyOutput{}, nil
}
func (m *flexMockIAMService) DetachRolePolicy(_ string, _ *iam.DetachRolePolicyInput) (*iam.DetachRolePolicyOutput, error) {
	return &iam.DetachRolePolicyOutput{}, nil
}
func (m *flexMockIAMService) ListAttachedRolePolicies(_ string, _ *iam.ListAttachedRolePoliciesInput) (*iam.ListAttachedRolePoliciesOutput, error) {
	return &iam.ListAttachedRolePoliciesOutput{}, nil
}
func (m *flexMockIAMService) PutRolePolicy(_ string, _ *iam.PutRolePolicyInput) (*iam.PutRolePolicyOutput, error) {
	return &iam.PutRolePolicyOutput{}, nil
}
func (m *flexMockIAMService) GetRolePolicy(_ string, _ *iam.GetRolePolicyInput) (*iam.GetRolePolicyOutput, error) {
	return &iam.GetRolePolicyOutput{}, nil
}
func (m *flexMockIAMService) DeleteRolePolicy(_ string, _ *iam.DeleteRolePolicyInput) (*iam.DeleteRolePolicyOutput, error) {
	return &iam.DeleteRolePolicyOutput{}, nil
}
func (m *flexMockIAMService) ListRolePolicies(_ string, _ *iam.ListRolePoliciesInput) (*iam.ListRolePoliciesOutput, error) {
	return &iam.ListRolePoliciesOutput{}, nil
}
func (m *flexMockIAMService) CreateInstanceProfile(_ string, _ *iam.CreateInstanceProfileInput) (*iam.CreateInstanceProfileOutput, error) {
	return &iam.CreateInstanceProfileOutput{}, nil
}
func (m *flexMockIAMService) GetInstanceProfile(_ string, _ *iam.GetInstanceProfileInput) (*iam.GetInstanceProfileOutput, error) {
	return &iam.GetInstanceProfileOutput{}, nil
}
func (m *flexMockIAMService) ListInstanceProfiles(_ string, _ *iam.ListInstanceProfilesInput) (*iam.ListInstanceProfilesOutput, error) {
	return &iam.ListInstanceProfilesOutput{}, nil
}
func (m *flexMockIAMService) DeleteInstanceProfile(_ string, _ *iam.DeleteInstanceProfileInput) (*iam.DeleteInstanceProfileOutput, error) {
	return &iam.DeleteInstanceProfileOutput{}, nil
}
func (m *flexMockIAMService) ListInstanceProfilesForRole(_ string, _ *iam.ListInstanceProfilesForRoleInput) (*iam.ListInstanceProfilesForRoleOutput, error) {
	return &iam.ListInstanceProfilesForRoleOutput{}, nil
}
func (m *flexMockIAMService) AddRoleToInstanceProfile(_ string, _ *iam.AddRoleToInstanceProfileInput) (*iam.AddRoleToInstanceProfileOutput, error) {
	return &iam.AddRoleToInstanceProfileOutput{}, nil
}
func (m *flexMockIAMService) RemoveRoleFromInstanceProfile(_ string, _ *iam.RemoveRoleFromInstanceProfileInput) (*iam.RemoveRoleFromInstanceProfileOutput, error) {
	return &iam.RemoveRoleFromInstanceProfileOutput{}, nil
}
func (m *flexMockIAMService) ResolveInstanceProfile(_, _ string) (*handlers_iam.InstanceProfile, error) {
	return nil, nil
}

// setupIAMRequestHandler wires an http.Handler for IAM_Request with injected SigV4 context values.
func setupIAMRequestHandler(svc handlers_iam.IAMService) http.Handler {
	gw := &GatewayConfig{
		DisableLogging: true,
		IAMService:     svc,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		ctx = context.WithValue(ctx, ctxService, "iam")
		ctx = context.WithValue(ctx, ctxAccountID, "000000000000")
		r = r.WithContext(ctx)
		if err := gw.IAM_Request(w, r); err != nil {
			gw.ErrorHandler(w, r, err)
		}
	})
}

func TestIAMRequest_CreateUser_Success(t *testing.T) {
	svc := &flexMockIAMService{
		createUserFn: func(_ string, input *iam.CreateUserInput) (*iam.CreateUserOutput, error) {
			return &iam.CreateUserOutput{
				User: &iam.User{
					UserName: input.UserName,
					UserId:   aws.String("AIDAEXAMPLE123"),
					Arn:      aws.String("arn:aws:iam::000000000000:user/alice"),
					Path:     aws.String("/"),
				},
			}, nil
		},
	}
	handler := setupIAMRequestHandler(svc)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Action=CreateUser&UserName=alice"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp := doRequest(handler, req)
	assert.Equal(t, 200, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	xmlStr := string(body)
	assert.Contains(t, xmlStr, "CreateUserResult")
	assert.Contains(t, xmlStr, "alice")
}

func TestIAMRequest_ListUsers_Success(t *testing.T) {
	svc := &flexMockIAMService{
		listUsersFn: func(_ string, _ *iam.ListUsersInput) (*iam.ListUsersOutput, error) {
			return &iam.ListUsersOutput{
				Users: []*iam.User{
					{UserName: aws.String("alice"), UserId: aws.String("AID1")},
					{UserName: aws.String("bob"), UserId: aws.String("AID2")},
				},
				IsTruncated: aws.Bool(false),
			}, nil
		},
	}
	handler := setupIAMRequestHandler(svc)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Action=ListUsers"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp := doRequest(handler, req)
	assert.Equal(t, 200, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	xmlStr := string(body)
	assert.Contains(t, xmlStr, "ListUsersResult")
	assert.Contains(t, xmlStr, "alice")
	assert.Contains(t, xmlStr, "bob")
}

func TestIAMRequest_UnknownAction(t *testing.T) {
	handler := setupIAMRequestHandler(&flexMockIAMService{})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Action=DoesNotExist"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp := doRequest(handler, req)
	assert.Equal(t, 400, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "InvalidAction")
}

func TestIAMRequest_EmptyAction(t *testing.T) {
	handler := setupIAMRequestHandler(&flexMockIAMService{})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("UserName=alice"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp := doRequest(handler, req)
	assert.Equal(t, 400, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "MissingAction")
}

func TestIAMRequest_NilService(t *testing.T) {
	gw := &GatewayConfig{
		DisableLogging: true,
		IAMService:     nil,
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		ctx = context.WithValue(ctx, ctxService, "iam")
		ctx = context.WithValue(ctx, ctxAccountID, "000000000000")
		r = r.WithContext(ctx)
		if err := gw.IAM_Request(w, r); err != nil {
			gw.ErrorHandler(w, r, err)
		}
	})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Action=CreateUser&UserName=alice"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp := doRequest(handler, req)
	assert.Equal(t, 500, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "InternalError")
}

func TestIAMRequest_ServiceError(t *testing.T) {
	svc := &flexMockIAMService{
		createUserFn: func(_ string, _ *iam.CreateUserInput) (*iam.CreateUserOutput, error) {
			return nil, errors.New(awserrors.ErrorIAMEntityAlreadyExists)
		},
	}
	handler := setupIAMRequestHandler(svc)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Action=CreateUser&UserName=alice"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp := doRequest(handler, req)
	assert.Equal(t, 409, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	xmlStr := string(body)
	assert.Contains(t, xmlStr, "EntityAlreadyExists")
	assert.Contains(t, xmlStr, "<ErrorResponse>")
}

func TestIAMRequest_MissingAccountID(t *testing.T) {
	gw := &GatewayConfig{
		DisableLogging: true,
		IAMService:     &flexMockIAMService{},
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), ctxService, "iam")
		r = r.WithContext(ctx)
		if err := gw.IAM_Request(w, r); err != nil {
			gw.ErrorHandler(w, r, err)
		}
	})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Action=CreateUser&UserName=alice"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp := doRequest(handler, req)
	assert.Equal(t, 500, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "InternalError")
}

func TestIAMRequest_ValidationError(t *testing.T) {
	handler := setupIAMRequestHandler(&flexMockIAMService{})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Action=CreateUser"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp := doRequest(handler, req)
	assert.Equal(t, 400, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "MissingParameter")
}
