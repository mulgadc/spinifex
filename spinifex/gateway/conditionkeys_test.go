//test:in-package — requestConditionKeys, checkPolicyResources, principalContext
//and ctxAccountID are all unexported, and the file reuses the in-package
//policyMockIAMService and withTestIdentity doubles.

package gateway

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestConditionKeys_PopulatesAvailableKeys(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.TLS = &tls.ConnectionState{}
	r.RemoteAddr = "10.4.1.9:52344"
	principal := principalContext{
		identity:      "alice",
		accountID:     "000000000001",
		principalType: principalTypeUser,
		userID:        "AIDAALICE",
	}

	keys := requestConditionKeys(r, principal)

	assert.Equal(t, iampolicy.ConditionKeys{
		iampolicy.KeySourceIP:         "10.4.1.9",
		iampolicy.KeySecureTransport:  "true",
		iampolicy.KeyUsername:         "alice",
		iampolicy.KeyUserID:           "AIDAALICE",
		iampolicy.KeyPrincipalAccount: "000000000001",
	}, keys)
}

// A role session's aws:userid is the ID STS minted for it. Both halves come from
// the resolved role, so unlike aws:username it can carry a decision.
func TestRequestConditionKeys_UserIDForAssumedRole(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)

	keys := requestConditionKeys(r, principalContext{
		identity:      "session",
		accountID:     "000000000001",
		principalType: principalTypeAssumedRole,
		assumedRoleID: "AROASHAREDOPS:session",
		userID:        "AROASHAREDOPS:session",
	})

	assert.Equal(t, "AROASHAREDOPS:session", keys[iampolicy.KeyUserID])
	assert.NotContains(t, keys, iampolicy.KeyUsername)
}

// An ID the door could not resolve is omitted, not set empty: a policy naming it
// then selects nothing on an Allow rather than selecting the empty-string path.
func TestRequestConditionKeys_OmitsUnresolvableUserID(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)

	keys := requestConditionKeys(r, principalContext{
		identity: "alice", accountID: "000000000001", principalType: principalTypeUser,
	})
	assert.NotContains(t, keys, iampolicy.KeyUserID)
}

// principalUserID resolves the ID once per request, at the point the principal
// is resolved. A user with no record and a session minted before the ID was
// recorded both yield an omitted key, not a failed request.
func TestPrincipalUserID_ResolvesAndOmits(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, IAMService: &mockIAMService{}}

	userID, err := gw.principalUserID(principalContext{
		identity: "alice", accountID: "000000000001", principalType: principalTypeUser,
	})
	require.NoError(t, err)
	assert.Equal(t, "AIDAALICE", userID)

	userID, err = gw.principalUserID(principalContext{
		identity: "session", accountID: "000000000001", principalType: principalTypeAssumedRole,
		assumedRoleID: "AROASHAREDOPS:session",
	})
	require.NoError(t, err)
	assert.Equal(t, "AROASHAREDOPS:session", userID)

	// Root's aws:userid is the account ID, matching what GetCallerIdentity
	// reports for the same principal.
	userID, err = gw.principalUserID(principalContext{
		identity: "root", accountID: "000000000000", principalType: principalTypeUser,
	})
	require.NoError(t, err)
	assert.Equal(t, "000000000000", userID)

	missing := &GatewayConfig{DisableLogging: true, IAMService: &mockIAMService{
		getUserFn: func(string, *iam.GetUserInput) (*iam.GetUserOutput, error) {
			return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
		},
	}}
	userID, err = missing.principalUserID(principalContext{
		identity: "alice", accountID: "000000000001", principalType: principalTypeUser,
	})
	require.NoError(t, err, "a deleted user omits the key rather than failing the request")
	assert.Empty(t, userID)

	// A record predating the field resolves to no ID, which omits the key.
	legacy := &GatewayConfig{DisableLogging: true, IAMService: &mockIAMService{
		getUserFn: func(string, *iam.GetUserInput) (*iam.GetUserOutput, error) {
			return &iam.GetUserOutput{User: &iam.User{UserName: aws.String("alice")}}, nil
		},
	}}
	userID, err = legacy.principalUserID(principalContext{
		identity: "alice", accountID: "000000000001", principalType: principalTypeUser,
	})
	require.NoError(t, err)
	assert.Empty(t, userID)

	// A session minted before assumed_role_id was recorded takes the same arm.
	userID, err = gw.principalUserID(principalContext{
		identity: "session", accountID: "000000000001", principalType: principalTypeAssumedRole,
	})
	require.NoError(t, err)
	assert.Empty(t, userID)
}

// A dependency fault must not read as "the principal has no ID": the key would
// be omitted, silently narrowing an Allow and widening a Deny to everything.
func TestPrincipalUserID_DependencyFaultFailsClosed(t *testing.T) {
	faulty := &GatewayConfig{DisableLogging: true, IAMService: &mockIAMService{
		getUserFn: func(string, *iam.GetUserInput) (*iam.GetUserOutput, error) {
			return nil, errors.New("nats: no responders available for request")
		},
	}}

	userID, err := faulty.principalUserID(principalContext{
		identity: "alice", accountID: "000000000001", principalType: principalTypeUser,
	})

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
	assert.Empty(t, userID)
}

// RoleSessionName is chosen by the caller of AssumeRole, so aws:username must
// stay absent for a role session — otherwise anyone permitted to assume the
// role satisfies an aws:username condition just by naming their session.
func TestRequestConditionKeys_OmitsUsernameForAssumedRole(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	principal := principalContext{
		identity:       "alice",
		accountID:      "000000000001",
		principalType:  principalTypeAssumedRole,
		assumedRoleARN: "arn:aws:sts::000000000001:assumed-role/SharedOps/alice",
	}

	keys := requestConditionKeys(r, principal)

	assert.NotContains(t, keys, iampolicy.KeyUsername)
	assert.Equal(t, "000000000001", keys[iampolicy.KeyPrincipalAccount])
}

// s3:prefix has no meaning on the AWS API path, so a policy conditioned on it
// must not fire here even though the same document works at predastore's door.
func TestRequestConditionKeys_OmitsS3Prefix(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/?prefix=home/", nil)
	keys := requestConditionKeys(r, principalContext{
		identity:      "alice",
		principalType: principalTypeUser,
	})

	assert.NotContains(t, keys, iampolicy.KeyS3Prefix)
	assert.Equal(t, "false", keys[iampolicy.KeySecureTransport])
}

// An empty value would compare as a real value that matches nothing, which on a
// Deny widens access instead of narrowing it. Absent is the only safe reading.
func TestRequestConditionKeys_OmitsEmptyValues(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.RemoteAddr = ""
	keys := requestConditionKeys(r, principalContext{principalType: principalTypeUser})

	assert.NotContains(t, keys, iampolicy.KeySourceIP)
	assert.NotContains(t, keys, iampolicy.KeyUsername)
	assert.NotContains(t, keys, iampolicy.KeyPrincipalAccount)
}

// The OCI registry chain never runs SigV4AuthMiddleware, so the source address
// must come from the request itself or every aws:SourceIp condition is inert
// on that door while working on the AWS API door.
func TestRequestConditionKeys_SourceIPWithoutAuthMiddleware(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v2/app/blobs/uploads/", nil)
	r.RemoteAddr = "10.4.1.9:52344"

	keys := requestConditionKeys(r, principalContext{
		identity:      "alice",
		principalType: principalTypeUser,
	})

	assert.Equal(t, "10.4.1.9", keys[iampolicy.KeySourceIP])
}

// conditionedPolicyIAMService serves one statement carrying cond, so a test can
// drive a real condition through checkPolicyResources.
func conditionedPolicyIAMService(effect string, cond map[string]map[string]handlers_iam.ConditionValue) *policyMockIAMService {
	docs := []handlers_iam.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []handlers_iam.Statement{
			{Effect: "Allow", Action: handlers_iam.StringOrArr{"*"}, Resource: handlers_iam.StringOrArr{"*"}},
			{
				Effect:    effect,
				Action:    handlers_iam.StringOrArr{"ec2:*"},
				Resource:  handlers_iam.StringOrArr{"*"},
				Condition: cond,
			},
		},
	}}
	if effect == "Allow" {
		docs[0].Statement = docs[0].Statement[1:]
	}
	return &policyMockIAMService{
		getUserPoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) { return docs, nil },
		getRolePoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) { return docs, nil },
	}
}

// Proves the keys built from the request actually reach the evaluator. Passing
// nil keys from checkPolicyResources would leave every other test green while
// silently killing every conditional Allow on this path.
func TestCheckPolicyResources_ConditionalAllowHonoursSourceIP(t *testing.T) {
	gw := &GatewayConfig{
		DisableLogging: true,
		IAMService: conditionedPolicyIAMService("Allow", map[string]map[string]handlers_iam.ConditionValue{
			iampolicy.OpIPAddress: {iampolicy.KeySourceIP: {"10.0.0.0/8"}},
		}),
	}

	tests := []struct {
		name    string
		remote  string
		wantErr string
	}{
		{"in range", "10.4.1.9:52344", ""},
		{"out of range", "192.168.1.1:52344", awserrors.ErrorAccessDenied},
		{"no source address", "", awserrors.ErrorAccessDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.RemoteAddr = tt.remote
			req = withTestIdentity(req)
			req = req.WithContext(context.WithValue(req.Context(), ctxAccountID, "000000000001"))

			err := gw.checkPolicyResources(req, "ec2", "DescribeInstances", []string{"*"})
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			code, _ := awserrors.ResolveErrorCode(err)
			assert.Equal(t, tt.wantErr, code)
		})
	}
}

// A Deny conditioned on a key this door cannot supply does not fire, which is
// the one direction where an absent key widens access. Pin it so the omission
// is a decision rather than an accident.
func TestCheckPolicyResources_ConditionalDenyHonoursSourceIP(t *testing.T) {
	gw := &GatewayConfig{
		DisableLogging: true,
		IAMService: conditionedPolicyIAMService("Deny", map[string]map[string]handlers_iam.ConditionValue{
			iampolicy.OpIPAddress: {iampolicy.KeySourceIP: {"203.0.113.0/24"}},
		}),
	}

	for _, tt := range []struct {
		name    string
		remote  string
		wantErr string
	}{
		{"denied range", "203.0.113.5:52344", awserrors.ErrorAccessDenied},
		{"other range", "10.4.1.9:52344", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.RemoteAddr = tt.remote
			req = withTestIdentity(req)
			req = req.WithContext(context.WithValue(req.Context(), ctxAccountID, "000000000001"))

			err := gw.checkPolicyResources(req, "ec2", "DescribeInstances", []string{"*"})
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			code, _ := awserrors.ResolveErrorCode(err)
			assert.Equal(t, tt.wantErr, code)
		})
	}
}
