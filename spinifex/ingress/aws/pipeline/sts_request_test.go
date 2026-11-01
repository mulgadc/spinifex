package pipeline

import (
	"errors"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/reqctx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
		principalType: gwfix.PrincipalTypeUser,
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
	gw := withRealAuth(gwfix.WithDeps(&GatewayConfig{DisableLogging: true}, gwfix.Deps{STS: svc, IAM: nil}))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := reqctx.WithService(r.Context(), "sts")
		ctx = withPrincipalField(ctx, fieldAccountID, awsidentifiers.GlobalAccountID)
		ctx = withPrincipalField(ctx, fieldIdentity, "alice")
		ctx = withPrincipalField(ctx, fieldPrincipalType, gwfix.PrincipalTypeUser)
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
		principalType: gwfix.PrincipalTypeUser,
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
		principalType: gwfix.PrincipalTypeUser,
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
		principalType:  gwfix.PrincipalTypeAssumedRole,
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

// A user at a non-root path is reported with its stored ARN, as AWS does,
// resolved by the SigV4 middleware rather than reformatted at "/".
func TestSTSRequest_GetCallerIdentity_PathedUserThroughMiddleware(t *testing.T) {
	const pathed = "arn:aws:iam::123456789012:user/eng/alice"
	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	require.NoError(t, err)
	gw := withRealAuth(gwfix.WithDeps(&GatewayConfig{
		DisableLogging: true,
		Region:         gwfix.Region,
	}, gwfix.Deps{STS: &flexMockSTSService{}, IAM: &gwfix.IAM{
		MasterKey: gwfix.MasterKey,
		AccessKeys: map[string]*iamdomain.AccessKey{gwfix.AccessKey: {
			AccessKeyID: gwfix.AccessKey, SecretAccessKey: encryptedSecret,
			UserName: "alice", AccountID: "123456789012", Status: "Active",
		}},
		GetUserFn: func(string, *iam.GetUserInput) (*iam.GetUserOutput, error) {
			return &iam.GetUserOutput{User: &iam.User{
				UserName: aws.String("alice"), UserId: aws.String("AIDAALICE"), Arn: aws.String(pathed),
			}}, nil
		},
	}}))
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
	gwfix.SignRequest(t, req, body, gwfix.AccessKey, gwfix.SecretKey)

	resp := doRequest(handler, req)
	b, _ := io.ReadAll(resp.Body)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(b))
	assert.Contains(t, string(b), "<Arn>"+pathed+"</Arn>")
}

func TestSTSRequest_GetCallerIdentity_RootShortcircuitsIAMLookup(t *testing.T) {
	// Root short-circuits IAM lookup — flexMockIAMService.GetUser would cause InternalError otherwise.
	handler := setupSTSRequestHandler(stsRequestParams{
		accountID:     awsidentifiers.GlobalAccountID,
		identity:      "root",
		principalType: gwfix.PrincipalTypeUser,
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
		principalType: gwfix.PrincipalTypeUser,
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
		principalType: gwfix.PrincipalTypeUser,
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
	assert.Equal(t, gwfix.PrincipalTypeUser, got.principalType)
	assert.Equal(t, "AKIAEXAMPLE", got.accessKeyID)
	assert.Equal(t, int64(3600), got.duration)
}

func TestSTSRequest_MissingAction(t *testing.T) {
	handler := setupSTSRequestHandler(stsRequestParams{
		accountID:     awsidentifiers.GlobalAccountID,
		identity:      "root",
		principalType: gwfix.PrincipalTypeUser,
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

func TestSTSRequest_NilService_InternalError(t *testing.T) {
	gw := withRealAuth(gwfix.WithDeps(&GatewayConfig{DisableLogging: true}, gwfix.Deps{STS: nil, IAM: &flexMockIAMService{}}))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := reqctx.WithService(r.Context(), "sts")
		ctx = withPrincipalField(ctx, fieldAccountID, awsidentifiers.GlobalAccountID)
		ctx = withPrincipalField(ctx, fieldIdentity, "alice")
		ctx = withPrincipalField(ctx, fieldPrincipalType, gwfix.PrincipalTypeUser)
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

func TestSTSRequest_UnknownAction(t *testing.T) {
	handler := setupSTSRequestHandler(stsRequestParams{
		accountID:     awsidentifiers.GlobalAccountID,
		identity:      "root",
		principalType: gwfix.PrincipalTypeUser,
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
