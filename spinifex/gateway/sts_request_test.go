package gateway

import (
	"errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/reqctx"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// flexMockSTSService is a configurable STSService mock with per-method
// overrides. It embeds the interface so it satisfies the full contract; only
// the fn-field methods below are wired, and any other method nil-panics if a
// test reaches an unmocked path.
type flexMockSTSService struct {
	stsdomain.STSService

	assumeRoleFn        func(string, string, string, *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error)
	getCallerIdentityFn func(string, string, string, *sts.GetCallerIdentityInput) (*sts.GetCallerIdentityOutput, error)
	getSessionTokenFn   func(string, string, string, string, *sts.GetSessionTokenInput) (*sts.GetSessionTokenOutput, error)
	lookupSessionFn     func(string) (*stsdomain.SessionCredential, error)
	assumeWebIdentityFn func(*sts.AssumeRoleWithWebIdentityInput) (*sts.AssumeRoleWithWebIdentityOutput, error)
}

var _ stsdomain.STSService = (*flexMockSTSService)(nil)

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

func (m *flexMockSTSService) GetSessionToken(callerAccountID, callerUserName, callerPrincipalType, callerAccessKeyID string, input *sts.GetSessionTokenInput) (*sts.GetSessionTokenOutput, error) {
	if m.getSessionTokenFn != nil {
		return m.getSessionTokenFn(callerAccountID, callerUserName, callerPrincipalType, callerAccessKeyID, input)
	}
	return &sts.GetSessionTokenOutput{}, nil
}

func (m *flexMockSTSService) LookupSessionCredential(akid string) (*stsdomain.SessionCredential, error) {
	if m.lookupSessionFn != nil {
		return m.lookupSessionFn(akid)
	}
	return nil, nil
}

func (m *flexMockSTSService) AssumeRoleWithWebIdentity(input *sts.AssumeRoleWithWebIdentityInput) (*sts.AssumeRoleWithWebIdentityOutput, error) {
	if m.assumeWebIdentityFn != nil {
		return m.assumeWebIdentityFn(input)
	}
	return nil, errors.New(awserrors.ErrorNotImplemented)
}

// stsRequestParams holds identity values the STS dispatcher reads from SigV4 context.
type stsRequestParams struct {
	accountID      string
	identity       string
	principalType  string
	accessKey      string
	assumedRoleARN string
	assumedRoleID  string
	// underlyingRoleARN is what the policy gate resolves an assumed-role
	// caller's policies by; SessionName never participates.
	underlyingRoleARN string
	stsSvc            stsdomain.STSService
	iamSvc            iamdomain.IAMService
}

func setupSTSRequestHandler(p stsRequestParams) http.Handler {
	if p.iamSvc == nil {
		p.iamSvc = &flexMockIAMService{}
	}
	gw := withVerifier(&GatewayConfig{
		DisableLogging: true,
		STSService:     p.stsSvc,
		IAMService:     p.iamSvc,
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		ctx = reqctx.WithService(ctx, "sts")
		ctx = withPrincipalField(ctx, fieldAccountID, p.accountID)
		ctx = withPrincipalField(ctx, fieldIdentity, p.identity)
		ctx = withPrincipalField(ctx, fieldPrincipalType, p.principalType)
		ctx = withPrincipalField(ctx, fieldAccessKey, p.accessKey)
		if p.assumedRoleARN != "" {
			ctx = withPrincipalField(ctx, fieldAssumedRoleARN, p.assumedRoleARN)
		}
		if p.assumedRoleID != "" {
			ctx = withPrincipalField(ctx, fieldAssumedRoleID, p.assumedRoleID)
			// The door seals aws:userid as the assumed-role ID for a role session.
			ctx = withPrincipalField(ctx, fieldUserID, p.assumedRoleID)
		}
		if p.underlyingRoleARN != "" {
			ctx = withPrincipalField(ctx, fieldUnderlyingRoleARN, p.underlyingRoleARN)
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
		accountID:     awsidentifiers.GlobalAccountID,
		identity:      "alice",
		principalType: principalTypeUser,
		accessKey:     "AKIAEXAMPLE",
		stsSvc:        svc,
		iamSvc:        stsIdentityPolicy(stsStatement("Allow", "sts:AssumeRole", "*")),
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
	// AssumedRoleID comes from the resolved SessionCredential; the dispatcher must not re-lookup it.
	svc := &flexMockSTSService{
		lookupSessionFn: func(string) (*stsdomain.SessionCredential, error) {
			t.Fatal("assumed-role GetCallerIdentity must not re-lookup the session credential")
			return nil, nil
		},
	}
	handler := setupSTSRequestHandler(stsRequestParams{
		accountID:      awsidentifiers.GlobalAccountID,
		identity:       "s1",
		principalType:  principalTypeAssumedRole,
		accessKey:      "ASIAEXAMPLE",
		assumedRoleARN: "arn:aws:sts::000000000000:assumed-role/app/s1",
		assumedRoleID:  "AROAEXAMPLE:s1",
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
	// Root short-circuits IAM lookup — flexMockIAMService.GetUser would cause InternalError otherwise.
	handler := setupSTSRequestHandler(stsRequestParams{
		accountID:     awsidentifiers.GlobalAccountID,
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

func TestSTSRequest_GetSessionToken_Success(t *testing.T) {
	var got struct {
		accountID     string
		userName      string
		principalType string
		accessKeyID   string
		duration      int64
	}
	svc := &flexMockSTSService{
		getSessionTokenFn: func(accountID, userName, principalType, accessKeyID string, input *sts.GetSessionTokenInput) (*sts.GetSessionTokenOutput, error) {
			got.accountID = accountID
			got.userName = userName
			got.principalType = principalType
			got.accessKeyID = accessKeyID
			got.duration = aws.Int64Value(input.DurationSeconds)
			return &sts.GetSessionTokenOutput{
				Credentials: &sts.Credentials{
					AccessKeyId:     aws.String("ASIAEXAMPLE123"),
					SecretAccessKey: aws.String("secret"),
					SessionToken:    aws.String("token"),
				},
			}, nil
		},
	}
	handler := setupSTSRequestHandler(stsRequestParams{
		accountID:     awsidentifiers.GlobalAccountID,
		identity:      "alice",
		principalType: principalTypeUser,
		accessKey:     "AKIAEXAMPLE",
		stsSvc:        svc,
	})

	body := "Action=GetSessionToken&DurationSeconds=3600"
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp := doRequest(handler, req)
	require.Equal(t, 200, resp.StatusCode)

	b, _ := io.ReadAll(resp.Body)
	xmlStr := string(b)
	assert.Contains(t, xmlStr, "GetSessionTokenResult")
	assert.Contains(t, xmlStr, "ASIAEXAMPLE123")

	// Verify forwarded identity fields and that checkPolicy did not block (STS_Request runs no checkPolicy pass).
	assert.Equal(t, awsidentifiers.GlobalAccountID, got.accountID)
	assert.Equal(t, "alice", got.userName)
	assert.Equal(t, principalTypeUser, got.principalType)
	assert.Equal(t, "AKIAEXAMPLE", got.accessKeyID)
	assert.Equal(t, int64(3600), got.duration)
}

func TestSTSRequest_UnknownAction(t *testing.T) {
	handler := setupSTSRequestHandler(stsRequestParams{
		accountID:     awsidentifiers.GlobalAccountID,
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
		accountID:     awsidentifiers.GlobalAccountID,
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
		accountID:     awsidentifiers.GlobalAccountID,
		identity:      "alice",
		principalType: principalTypeUser,
		accessKey:     "AKIAEXAMPLE",
		stsSvc:        svc,
		// An empty policy: a request missing RoleArn is malformed whatever the
		// caller holds, so the gate must not answer it with a denial.
		iamSvc: stsIdentityPolicy(),
	})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Action=AssumeRole&RoleSessionName=s1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp := doRequest(handler, req)
	assert.Equal(t, 400, resp.StatusCode)

	b, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(b), "MissingParameter")
}

func TestSTSRequest_NilService_InternalError(t *testing.T) {
	gw := withVerifier(&GatewayConfig{DisableLogging: true, STSService: nil, IAMService: &flexMockIAMService{}})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := reqctx.WithService(r.Context(), "sts")
		ctx = withPrincipalField(ctx, fieldAccountID, awsidentifiers.GlobalAccountID)
		ctx = withPrincipalField(ctx, fieldIdentity, "alice")
		ctx = withPrincipalField(ctx, fieldPrincipalType, principalTypeUser)
		ctx = withPrincipalField(ctx, fieldAccessKey, "AKIAEXAMPLE")
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

// TestSTSRequest_AssumeRole_NilIAMService_InternalError pins the gate to the
// same fail-closed answer checkPolicyResources gives: the target-role lookup
// runs ahead of it, so an absent IAM service must not reach a nil interface.
func TestSTSRequest_AssumeRole_NilIAMService_InternalError(t *testing.T) {
	svc := &flexMockSTSService{
		assumeRoleFn: func(string, string, string, *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error) {
			t.Fatal("handler reached: an unavailable IAM service must fail closed")
			return nil, nil
		},
	}
	gw := withVerifier(&GatewayConfig{DisableLogging: true, STSService: svc, IAMService: nil})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := reqctx.WithService(r.Context(), "sts")
		ctx = withPrincipalField(ctx, fieldAccountID, awsidentifiers.GlobalAccountID)
		ctx = withPrincipalField(ctx, fieldIdentity, "alice")
		ctx = withPrincipalField(ctx, fieldPrincipalType, principalTypeUser)
		ctx = withPrincipalField(ctx, fieldAccessKey, "AKIAEXAMPLE")
		r = r.WithContext(ctx)
		if err := gw.STS_Request(w, r); err != nil {
			gw.ErrorHandler(w, r, err)
		}
	})

	req := httptest.NewRequest(http.MethodPost, "/",
		strings.NewReader("Action=AssumeRole&RoleArn="+stsTestRoleARN+"&RoleSessionName=s1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp := doRequest(handler, req)
	assert.Equal(t, 500, resp.StatusCode)
	b, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(b), "InternalError")
}

func TestSTSRequest_AssumeRole_ServiceError_PropagatesAccessDenied(t *testing.T) {
	// AccessDenied from the handler (trust-policy denial) must reach the wire as IAM ErrorResponse + 403.
	svc := &flexMockSTSService{
		assumeRoleFn: func(string, string, string, *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error) {
			return nil, errors.New(awserrors.ErrorAccessDenied)
		},
	}
	handler := setupSTSRequestHandler(stsRequestParams{
		accountID:     awsidentifiers.GlobalAccountID,
		identity:      "alice",
		principalType: principalTypeUser,
		accessKey:     "AKIAEXAMPLE",
		stsSvc:        svc,
		iamSvc:        stsIdentityPolicy(stsStatement("Allow", "sts:AssumeRole", "*")),
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
	// User principal triggers IAMService.GetUser to resolve UserId.
	iamSvc := &flexMockIAMService{
		getUserFn: func(_ string, _ *iam.GetUserInput) (*iam.GetUserOutput, error) {
			return &iam.GetUserOutput{User: &iam.User{UserId: aws.String("AIDAALICE000")}}, nil
		},
	}
	handler := setupSTSRequestHandler(stsRequestParams{
		accountID:     awsidentifiers.GlobalAccountID,
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

// A user at a non-root path is reported with its stored ARN, as AWS does,
// resolved by the SigV4 middleware rather than reformatted at "/".
func TestSTSRequest_GetCallerIdentity_PathedUserThroughMiddleware(t *testing.T) {
	const pathed = "arn:aws:iam::123456789012:user/eng/alice"
	encryptedSecret, err := iamdomain.EncryptSecret(testSecretKey, testMasterKey)
	require.NoError(t, err)
	gw := withVerifier(&GatewayConfig{
		DisableLogging: true,
		Region:         testRegion,
		STSService:     &flexMockSTSService{},
		IAMService: &mockIAMService{
			masterKey: testMasterKey,
			accessKeys: map[string]*iamdomain.AccessKey{testAccessKey: {
				AccessKeyID: testAccessKey, SecretAccessKey: encryptedSecret,
				UserName: "alice", AccountID: "123456789012", Status: "Active",
			}},
			getUserFn: func(string, *iam.GetUserInput) (*iam.GetUserOutput, error) {
				return &iam.GetUserOutput{User: &iam.User{
					UserName: aws.String("alice"), UserId: aws.String("AIDAALICE"), Arn: aws.String(pathed),
				}}, nil
			},
		},
	})
	withServices(gw, stsRegistration(gw), ec2Registration(gw))
	handler := gw.SigV4AuthMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := gw.STS_Request(w, r); err != nil {
			gw.ErrorHandler(w, r, err)
		}
	}))

	body := []byte("Action=GetCallerIdentity")
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	req.Host = "localhost:9999"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	signTestRequest(t, req, body, testAccessKey, testSecretKey)

	resp := doRequest(handler, req)
	b, _ := io.ReadAll(resp.Body)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(b))
	assert.Contains(t, string(b), "<Arn>"+pathed+"</Arn>")
}

func TestBuildCallerARN(t *testing.T) {
	const acct = "123456789012"
	cases := []struct {
		name, accountID, identity, principalType, assumedRoleARN, userARN, want string
	}{
		{"pathed user takes its stored ARN", acct, "alice", principalTypeUser, "",
			"arn:aws:iam::" + acct + ":user/eng/alice", "arn:aws:iam::" + acct + ":user/eng/alice"},
		{"user with no record formats at the root path", acct, "alice", principalTypeUser, "", "",
			"arn:aws:iam::" + acct + ":user/alice"},
		{"global root ignores a user ARN", awsidentifiers.GlobalAccountID, "root", principalTypeUser, "",
			"arn:aws:iam::" + awsidentifiers.GlobalAccountID + ":user/root", "arn:aws:iam::" + awsidentifiers.GlobalAccountID + ":root"},
		{"root principal", acct, "", principalTypeRoot, "", "", "arn:aws:iam::" + acct + ":root"},
		{"assumed role keeps its session ARN", acct, "s", principalTypeAssumedRole,
			"arn:aws:sts::" + acct + ":assumed-role/Ops/s", "", "arn:aws:sts::" + acct + ":assumed-role/Ops/s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildCallerARN(tc.accountID, tc.identity, tc.principalType, tc.assumedRoleARN, tc.userARN)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
