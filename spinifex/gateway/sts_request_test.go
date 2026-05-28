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
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	handlers_sts "github.com/mulgadc/spinifex/spinifex/handlers/sts"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// flexMockSTSService is a configurable STSService mock with per-method
// overrides. Mirrors flexMockIAMService for STS-side dispatcher tests.
type flexMockSTSService struct {
	assumeRoleFn        func(string, string, string, *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error)
	getCallerIdentityFn func(string, string, string, *sts.GetCallerIdentityInput) (*sts.GetCallerIdentityOutput, error)
	lookupSessionFn     func(string) (*handlers_sts.SessionCredential, error)
}

var _ handlers_sts.STSService = (*flexMockSTSService)(nil)

func (m *flexMockSTSService) AssumeRole(callerAccountID, callerARN, callerIdentity string, input *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error) {
	if m.assumeRoleFn != nil {
		return m.assumeRoleFn(callerAccountID, callerARN, callerIdentity, input)
	}
	return &sts.AssumeRoleOutput{}, nil
}

func (m *flexMockSTSService) GetCallerIdentity(callerAccountID, callerARN, callerUserID string, input *sts.GetCallerIdentityInput) (*sts.GetCallerIdentityOutput, error) {
	if m.getCallerIdentityFn != nil {
		return m.getCallerIdentityFn(callerAccountID, callerARN, callerUserID, input)
	}
	return &sts.GetCallerIdentityOutput{
		Account: aws.String(callerAccountID),
		Arn:     aws.String(callerARN),
		UserId:  aws.String(callerUserID),
	}, nil
}

func (m *flexMockSTSService) LookupSessionCredential(akid string) (*handlers_sts.SessionCredential, error) {
	if m.lookupSessionFn != nil {
		return m.lookupSessionFn(akid)
	}
	return nil, nil
}

func (m *flexMockSTSService) VerifySessionToken(*handlers_sts.SessionCredential, string) bool {
	return true
}

// stsRequestParams is the set of identity values the dispatcher pulls from
// SigV4 context. Defaults work for a user principal in the global account.
type stsRequestParams struct {
	accountID      string
	identity       string
	principalType  string
	accessKey      string
	assumedRoleARN string
	stsSvc         handlers_sts.STSService
	iamSvc         handlers_iam.IAMService
}

func setupSTSRequestHandler(p stsRequestParams) http.Handler {
	if p.iamSvc == nil {
		p.iamSvc = &flexMockIAMService{}
	}
	gw := &GatewayConfig{
		DisableLogging: true,
		STSService:     p.stsSvc,
		IAMService:     p.iamSvc,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		ctx = context.WithValue(ctx, ctxService, "sts")
		ctx = context.WithValue(ctx, ctxAccountID, p.accountID)
		ctx = context.WithValue(ctx, ctxIdentity, p.identity)
		ctx = context.WithValue(ctx, ctxPrincipalType, p.principalType)
		ctx = context.WithValue(ctx, ctxAccessKey, p.accessKey)
		if p.assumedRoleARN != "" {
			ctx = context.WithValue(ctx, ctxAssumedRoleARN, p.assumedRoleARN)
		}
		r = r.WithContext(ctx)
		if err := gw.STS_Request(w, r); err != nil {
			gw.ErrorHandler(w, r, err)
		}
	})
}

func TestSTSRequest_AssumeRole_Success(t *testing.T) {
	var seenCallerARN string
	svc := &flexMockSTSService{
		assumeRoleFn: func(accountID, callerARN, identity string, input *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error) {
			seenCallerARN = callerARN
			return &sts.AssumeRoleOutput{
				Credentials: &sts.Credentials{
					AccessKeyId:     aws.String("ASIAEXAMPLE123"),
					SecretAccessKey: aws.String("secret"),
					SessionToken:    aws.String("token"),
				},
				AssumedRoleUser: &sts.AssumedRoleUser{
					AssumedRoleId: aws.String("AROAEXAMPLE:s1"),
					Arn:           aws.String("arn:aws:sts::000000000000:assumed-role/app/s1"),
				},
			}, nil
		},
	}
	handler := setupSTSRequestHandler(stsRequestParams{
		accountID:     utils.GlobalAccountID,
		identity:      "alice",
		principalType: principalTypeUser,
		accessKey:     "AKIAEXAMPLE",
		stsSvc:        svc,
	})

	body := "Action=AssumeRole&RoleArn=arn:aws:iam::000000000000:role/app&RoleSessionName=s1"
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp := doRequest(handler, req)
	require.Equal(t, 200, resp.StatusCode)

	b, _ := io.ReadAll(resp.Body)
	xmlStr := string(b)
	assert.Contains(t, xmlStr, "AssumeRoleResult")
	assert.Contains(t, xmlStr, "ASIAEXAMPLE123")
	assert.Equal(t, "arn:aws:iam::000000000000:user/alice", seenCallerARN)
}

func TestSTSRequest_GetCallerIdentity_AssumedRole(t *testing.T) {
	svc := &flexMockSTSService{
		lookupSessionFn: func(akid string) (*handlers_sts.SessionCredential, error) {
			return &handlers_sts.SessionCredential{
				AccessKeyID:    akid,
				AssumedRoleID:  "AROAEXAMPLE:s1",
				AssumedRoleARN: "arn:aws:sts::000000000000:assumed-role/app/s1",
				AccountID:      utils.GlobalAccountID,
				SessionName:    "s1",
			}, nil
		},
	}
	handler := setupSTSRequestHandler(stsRequestParams{
		accountID:      utils.GlobalAccountID,
		identity:       "s1",
		principalType:  principalTypeAssumedRole,
		accessKey:      "ASIAEXAMPLE",
		assumedRoleARN: "arn:aws:sts::000000000000:assumed-role/app/s1",
		stsSvc:         svc,
	})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Action=GetCallerIdentity"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp := doRequest(handler, req)
	require.Equal(t, 200, resp.StatusCode)

	b, _ := io.ReadAll(resp.Body)
	xmlStr := string(b)
	assert.Contains(t, xmlStr, "GetCallerIdentityResult")
	assert.Contains(t, xmlStr, "arn:aws:sts::000000000000:assumed-role/app/s1")
	assert.Contains(t, xmlStr, "AROAEXAMPLE:s1")
}

func TestSTSRequest_GetCallerIdentity_RootShortcircuitsIAMLookup(t *testing.T) {
	// Root must produce {Account, Arn, UserId} without touching IAM. The
	// flexMockIAMService's GetUser would return a zero-value User and the
	// resolver would surface InternalError; the root short-circuit skips
	// that path.
	handler := setupSTSRequestHandler(stsRequestParams{
		accountID:     utils.GlobalAccountID,
		identity:      "root",
		principalType: principalTypeUser,
		accessKey:     "AKIAROOT",
		stsSvc:        &flexMockSTSService{},
	})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Action=GetCallerIdentity"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp := doRequest(handler, req)
	require.Equal(t, 200, resp.StatusCode)

	b, _ := io.ReadAll(resp.Body)
	xmlStr := string(b)
	assert.Contains(t, xmlStr, "<Arn>arn:aws:iam::000000000000:root</Arn>")
	assert.Contains(t, xmlStr, "<UserId>000000000000</UserId>")
}

func TestSTSRequest_StubAction_Returns501(t *testing.T) {
	stubs := []string{
		"GetSessionToken",
		"AssumeRoleWithWebIdentity",
		"AssumeRoleWithSAML",
		"GetAccessKeyInfo",
		"GetFederationToken",
		"DecodeAuthorizationMessage",
	}
	for _, action := range stubs {
		t.Run(action, func(t *testing.T) {
			handler := setupSTSRequestHandler(stsRequestParams{
				accountID:     utils.GlobalAccountID,
				identity:      "root",
				principalType: principalTypeUser,
				accessKey:     "AKIAROOT",
				stsSvc:        &flexMockSTSService{},
			})
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Action="+action))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

			resp := doRequest(handler, req)
			assert.Equal(t, 501, resp.StatusCode)

			b, _ := io.ReadAll(resp.Body)
			xmlStr := string(b)
			assert.Contains(t, xmlStr, "NotImplementedException")
			assert.Contains(t, xmlStr, "<ErrorResponse>")
		})
	}
}

func TestSTSRequest_UnknownAction(t *testing.T) {
	handler := setupSTSRequestHandler(stsRequestParams{
		accountID:     utils.GlobalAccountID,
		identity:      "root",
		principalType: principalTypeUser,
		accessKey:     "AKIAROOT",
		stsSvc:        &flexMockSTSService{},
	})
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Action=ThisDoesNotExist"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp := doRequest(handler, req)
	assert.Equal(t, 400, resp.StatusCode)

	b, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(b), "InvalidAction")
}

func TestSTSRequest_MissingAction(t *testing.T) {
	handler := setupSTSRequestHandler(stsRequestParams{
		accountID:     utils.GlobalAccountID,
		identity:      "root",
		principalType: principalTypeUser,
		accessKey:     "AKIAROOT",
		stsSvc:        &flexMockSTSService{},
	})
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Foo=Bar"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp := doRequest(handler, req)
	assert.Equal(t, 400, resp.StatusCode)

	b, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(b), "MissingAction")
}

func TestSTSRequest_AssumeRole_MissingRoleArn(t *testing.T) {
	svc := &flexMockSTSService{
		assumeRoleFn: func(string, string, string, *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error) {
			t.Fatal("AssumeRole should not be called when required params missing")
			return nil, nil
		},
	}
	handler := setupSTSRequestHandler(stsRequestParams{
		accountID:     utils.GlobalAccountID,
		identity:      "alice",
		principalType: principalTypeUser,
		accessKey:     "AKIAEXAMPLE",
		stsSvc:        svc,
	})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Action=AssumeRole&RoleSessionName=s1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp := doRequest(handler, req)
	assert.Equal(t, 400, resp.StatusCode)

	b, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(b), "MissingParameter")
}

func TestSTSRequest_NilService_InternalError(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, STSService: nil, IAMService: &flexMockIAMService{}}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), ctxService, "sts")
		ctx = context.WithValue(ctx, ctxAccountID, utils.GlobalAccountID)
		ctx = context.WithValue(ctx, ctxIdentity, "alice")
		ctx = context.WithValue(ctx, ctxPrincipalType, principalTypeUser)
		ctx = context.WithValue(ctx, ctxAccessKey, "AKIAEXAMPLE")
		r = r.WithContext(ctx)
		if err := gw.STS_Request(w, r); err != nil {
			gw.ErrorHandler(w, r, err)
		}
	})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Action=GetCallerIdentity"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp := doRequest(handler, req)
	assert.Equal(t, 500, resp.StatusCode)
	b, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(b), "InternalError")
}

func TestSTSRequest_AssumeRole_ServiceError_PropagatesAccessDenied(t *testing.T) {
	// Confirms that an AccessDenied returned by the handler (trust-policy
	// denial) reaches the wire with the IAM-style ErrorResponse envelope and
	// HTTP 403.
	svc := &flexMockSTSService{
		assumeRoleFn: func(string, string, string, *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error) {
			return nil, errors.New(awserrors.ErrorAccessDenied)
		},
	}
	handler := setupSTSRequestHandler(stsRequestParams{
		accountID:     utils.GlobalAccountID,
		identity:      "alice",
		principalType: principalTypeUser,
		accessKey:     "AKIAEXAMPLE",
		stsSvc:        svc,
	})

	body := "Action=AssumeRole&RoleArn=arn:aws:iam::000000000000:role/app&RoleSessionName=s1"
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp := doRequest(handler, req)
	assert.Equal(t, 403, resp.StatusCode)
	b, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(b), "AccessDenied")
	assert.Contains(t, string(b), "<ErrorResponse>")
}

func TestSTSRequest_GetCallerIdentity_User_LookupIAM(t *testing.T) {
	// Sanity check: user principal triggers IAMService.GetUser to resolve
	// UserId. Uses flexMockIAMService configured to return a fixed UserId.
	iamSvc := &flexMockIAMService{
		getUserFn: func(_ string, _ *iam.GetUserInput) (*iam.GetUserOutput, error) {
			return &iam.GetUserOutput{User: &iam.User{UserId: aws.String("AIDAALICE000")}}, nil
		},
	}
	handler := setupSTSRequestHandler(stsRequestParams{
		accountID:     utils.GlobalAccountID,
		identity:      "alice",
		principalType: principalTypeUser,
		accessKey:     "AKIAEXAMPLE",
		stsSvc:        &flexMockSTSService{},
		iamSvc:        iamSvc,
	})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Action=GetCallerIdentity"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp := doRequest(handler, req)
	require.Equal(t, 200, resp.StatusCode)

	b, _ := io.ReadAll(resp.Body)
	xmlStr := string(b)
	assert.Contains(t, xmlStr, "<Arn>arn:aws:iam::000000000000:user/alice</Arn>")
	assert.Contains(t, xmlStr, "<UserId>AIDAALICE000</UserId>")
}
