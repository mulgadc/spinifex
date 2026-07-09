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

// flexMockIAMService is a configurable mock with per-method overrides. It
// embeds the interface so it satisfies the full contract; only the fn-field
// methods below are wired, and any other method nil-panics if a test reaches
// an unmocked path.
type flexMockIAMService struct {
	handlers_iam.IAMService

	createUserFn      func(string, *iam.CreateUserInput) (*iam.CreateUserOutput, error)
	getUserFn         func(string, *iam.GetUserInput) (*iam.GetUserOutput, error)
	listUsersFn       func(string, *iam.ListUsersInput) (*iam.ListUsersOutput, error)
	deleteUserFn      func(string, *iam.DeleteUserInput) (*iam.DeleteUserOutput, error)
	createAccessKeyFn func(string, *iam.CreateAccessKeyInput) (*iam.CreateAccessKeyOutput, error)
	listAccessKeysFn  func(string, *iam.ListAccessKeysInput) (*iam.ListAccessKeysOutput, error)
	deleteAccessKeyFn func(string, *iam.DeleteAccessKeyInput) (*iam.DeleteAccessKeyOutput, error)
	updateAccessKeyFn func(string, *iam.UpdateAccessKeyInput) (*iam.UpdateAccessKeyOutput, error)

	getAccountSummaryFn func(string, *iam.GetAccountSummaryInput) (*iam.GetAccountSummaryOutput, error)
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

func (m *flexMockIAMService) GetAccountSummary(accountID string, input *iam.GetAccountSummaryInput) (*iam.GetAccountSummaryOutput, error) {
	if m.getAccountSummaryFn != nil {
		return m.getAccountSummaryFn(accountID, input)
	}
	return &iam.GetAccountSummaryOutput{}, nil
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

// Locks in the xmlutil map behaviour: SummaryMap (map[string]*int64) must
// marshal to <SummaryMap><entry><key>..</key><value>..</value></entry>. The Go
// stdlib encoding/xml path cannot marshal a map, so this guards the xmlutil
// builder wiring end to end.
func TestIAMRequest_GetAccountSummary_Success(t *testing.T) {
	svc := &flexMockIAMService{
		getAccountSummaryFn: func(_ string, _ *iam.GetAccountSummaryInput) (*iam.GetAccountSummaryOutput, error) {
			return &iam.GetAccountSummaryOutput{
				SummaryMap: map[string]*int64{"Users": aws.Int64(2)},
			}, nil
		},
	}
	handler := setupIAMRequestHandler(svc)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Action=GetAccountSummary"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp := doRequest(handler, req)
	assert.Equal(t, 200, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	xmlStr := string(body)
	assert.Contains(t, xmlStr, "GetAccountSummaryResult")
	assert.Contains(t, xmlStr, "SummaryMap")
	assert.Contains(t, xmlStr, "<entry>")
	assert.Contains(t, xmlStr, "<key>Users</key>")
	assert.Contains(t, xmlStr, "<value>2</value>")
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
