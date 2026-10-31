package gateway

import (
	"errors"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/reqctx"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/credverify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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

func TestIAMRequest_EmptyAction(t *testing.T) {
	handler := setupIAMRequestHandler(&flexMockIAMService{})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("UserName=alice"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp := doRequest(handler, req)
	assert.Equal(t, 400, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "MissingAction")
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

// AWS answers GetRole for a never-used role with an empty <RoleLastUsed/>
// element; a nil-field struct must still marshal to that element, not vanish.
func TestIAMRequest_GetRole_EmptyRoleLastUsed(t *testing.T) {
	svc := &flexMockIAMService{
		getRoleFn: func(_ string, input *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
			return &iam.GetRoleOutput{Role: &iam.Role{
				RoleName:                 input.RoleName,
				AssumeRolePolicyDocument: aws.String("{}"),
				RoleLastUsed:             &iam.RoleLastUsed{},
			}}, nil
		},
	}
	handler := setupIAMRequestHandler(svc)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Action=GetRole&RoleName=app"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp := doRequest(handler, req)
	assert.Equal(t, 200, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "<RoleLastUsed></RoleLastUsed>")
}

// AWS answers an account with no alias with an empty AccountAliases list and
// IsTruncated false, and no Marker.
func TestIAMRequest_ListAccountAliases_Empty(t *testing.T) {
	server := httptest.NewServer(setupIAMRequestHandler(&realAliasesIAMService{}))
	defer server.Close()
	client := iam.New(session.Must(session.NewSession(&aws.Config{
		Credentials: credentials.NewStaticCredentials("test", "test", ""),
		DisableSSL:  aws.Bool(true),
		Endpoint:    aws.String(server.URL),
		MaxRetries:  aws.Int(0),
		Region:      aws.String("us-east-1"),
	})))

	out, err := client.ListAccountAliases(&iam.ListAccountAliasesInput{})
	require.NoError(t, err)
	assert.Empty(t, out.AccountAliases)
	assert.False(t, aws.BoolValue(out.IsTruncated))
	assert.Nil(t, out.Marker)

	_, err = client.ListAccountAliases(&iam.ListAccountAliasesInput{MaxItems: aws.Int64(1001)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorValidationError)
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

func TestIAMRequest_MissingAccountID(t *testing.T) {
	gw := &GatewayConfig{
		DisableLogging: true,
		IAMService:     &flexMockIAMService{},
		Credentials:    credverify.New(&flexMockIAMService{}, nil),
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := reqctx.WithService(r.Context(), "iam")
		r = withTestIdentity(r.WithContext(ctx))
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

func TestIAMRequest_NilService(t *testing.T) {
	gw := &GatewayConfig{
		DisableLogging: true,
		IAMService:     nil,
		Credentials:    credverify.New(nil, nil),
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		ctx = reqctx.WithService(ctx, "iam")
		ctx = withPrincipalField(ctx, fieldAccountID, "000000000000")
		ctx = withPrincipalField(ctx, fieldIdentity, "root")
		ctx = withPrincipalField(ctx, fieldPrincipalType, gwfix.PrincipalTypeUser)
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

func TestIAMRequest_UnknownAction(t *testing.T) {
	handler := setupIAMRequestHandler(&flexMockIAMService{})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Action=DoesNotExist"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp := doRequest(handler, req)
	assert.Equal(t, 400, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "InvalidAction")
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
