//test:in-package — requestConditionKeys, checkPolicyResources, credential.Principal
//and ctxAccountID are all unexported, and the file reuses the in-package
//gwfix.PolicyIAM and withTestIdentity doubles.

package gateway

import (
	"crypto/tls"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/policyauth"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestConditionKeys_PopulatesAvailableKeys(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.TLS = &tls.ConnectionState{}
	r.RemoteAddr = "10.4.1.9:52344"
	principal := credential.Principal{
		Identity:      "alice",
		AccountID:     "000000000001",
		PrincipalType: principalTypeUser,
		UserID:        "AIDAALICE",
	}

	before := time.Now().Truncate(time.Second)
	keys := policyauth.RequestConditionKeys(r, principal)
	after := time.Now()

	// The request time is the server clock at the call, in both spellings.
	for _, key := range []string{iampolicy.KeyCurrentTime, iampolicy.KeyEpochTime} {
		at, err := iampolicy.ParseDate(keys[key])
		require.NoError(t, err, key)
		assert.False(t, at.Before(before) || at.After(after), "%s is %s, outside the call", key, at)
		delete(keys, key)
	}
	assert.Equal(t, iampolicy.ConditionKeys{
		iampolicy.KeySourceIP:         "10.4.1.9",
		iampolicy.KeySecureTransport:  "true",
		iampolicy.KeyUsername:         "alice",
		iampolicy.KeyUserID:           "AIDAALICE",
		iampolicy.KeyPrincipalAccount: "000000000001",
		iampolicy.KeyPrincipalType:    iampolicy.PrincipalTypeUser,
	}, keys)
}

// Every principal type this door recognizes maps to the canonical AWS spelling,
// and an unrecognized one omits the key rather than supplying an empty string.
func TestRequestConditionKeys_PrincipalType(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)

	tests := []struct {
		principalType string
		want          string
		omitted       bool
	}{
		{principalType: principalTypeUser, want: iampolicy.PrincipalTypeUser},
		{principalType: principalTypeAssumedRole, want: iampolicy.PrincipalTypeAssumedRole},
		{principalType: principalTypeRoot, want: iampolicy.PrincipalTypeAccount},
		{principalType: "", omitted: true},
	}
	for _, tt := range tests {
		keys := policyauth.RequestConditionKeys(r, credential.Principal{
			Identity: "alice", AccountID: "000000000001", PrincipalType: tt.principalType,
		})
		if tt.omitted {
			assert.NotContains(t, keys, iampolicy.KeyPrincipalType, "principalType %q", tt.principalType)
			continue
		}
		assert.Equal(t, tt.want, keys[iampolicy.KeyPrincipalType], "principalType %q", tt.principalType)
	}
}

// A role session's aws:userid is the ID STS minted for it. Both halves come from
// the resolved role, so unlike aws:username it can carry a decision.
func TestRequestConditionKeys_UserIDForAssumedRole(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)

	keys := policyauth.RequestConditionKeys(r, credential.Principal{
		Identity:      "session",
		AccountID:     "000000000001",
		PrincipalType: principalTypeAssumedRole,
		AssumedRoleID: "AROASHAREDOPS:session",
		UserID:        "AROASHAREDOPS:session",
	})

	assert.Equal(t, "AROASHAREDOPS:session", keys[iampolicy.KeyUserID])
	assert.NotContains(t, keys, iampolicy.KeyUsername)
}

// An ID the door could not resolve is omitted, not set empty: a policy naming it
// then selects nothing on an Allow rather than selecting the empty-string path.
func TestRequestConditionKeys_OmitsUnresolvableUserID(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)

	keys := policyauth.RequestConditionKeys(r, credential.Principal{
		Identity: "alice", AccountID: "000000000001", PrincipalType: principalTypeUser,
	})
	assert.NotContains(t, keys, iampolicy.KeyUserID)
}

// RoleSessionName is chosen by the caller of AssumeRole, so aws:username must
// stay absent for a role session — otherwise anyone permitted to assume the
// role satisfies an aws:username condition just by naming their session.
func TestRequestConditionKeys_OmitsUsernameForAssumedRole(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	principal := credential.Principal{
		Identity:       "alice",
		AccountID:      "000000000001",
		PrincipalType:  principalTypeAssumedRole,
		AssumedRoleARN: "arn:aws:sts::000000000001:assumed-role/SharedOps/alice",
	}

	keys := policyauth.RequestConditionKeys(r, principal)

	assert.NotContains(t, keys, iampolicy.KeyUsername)
	assert.Equal(t, "000000000001", keys[iampolicy.KeyPrincipalAccount])
}

// s3:prefix has no meaning on the AWS API path, so a policy conditioned on it
// must not fire here even though the same document works at predastore's door.
func TestRequestConditionKeys_OmitsS3Prefix(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/?prefix=home/", nil)
	keys := policyauth.RequestConditionKeys(r, credential.Principal{
		Identity:      "alice",
		PrincipalType: principalTypeUser,
	})

	assert.NotContains(t, keys, iampolicy.KeyS3Prefix)
	assert.Equal(t, "false", keys[iampolicy.KeySecureTransport])
}

// An empty value would compare as a real value that matches nothing, which on a
// Deny widens access instead of narrowing it. Absent is the only safe reading.
func TestRequestConditionKeys_OmitsEmptyValues(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.RemoteAddr = ""
	keys := policyauth.RequestConditionKeys(r, credential.Principal{PrincipalType: principalTypeUser})

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

	keys := policyauth.RequestConditionKeys(r, credential.Principal{
		Identity:      "alice",
		PrincipalType: principalTypeUser,
	})

	assert.Equal(t, "10.4.1.9", keys[iampolicy.KeySourceIP])
}

// conditionedPolicyIAMService serves one statement carrying cond, so a test can
// drive a real condition through checkPolicyResources.
func conditionedPolicyIAMService(effect string, cond map[string]map[string]iamdomain.ConditionValue) *gwfix.PolicyIAM {
	docs := []iamdomain.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []iamdomain.Statement{
			{Effect: "Allow", Action: iamdomain.StringOrArr{"*"}, Resource: iamdomain.StringOrArr{"*"}},
			{
				Effect:    effect,
				Action:    iamdomain.StringOrArr{"ec2:*"},
				Resource:  iamdomain.StringOrArr{"*"},
				Condition: cond,
			},
		},
	}}
	if effect == "Allow" {
		docs[0].Statement = docs[0].Statement[1:]
	}
	return &gwfix.PolicyIAM{
		GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) { return docs, nil },
		GetRolePoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) { return docs, nil },
	}
}

// Proves the keys built from the request actually reach the evaluator. Passing
// nil keys from checkPolicyResources would leave every other test green while
// silently killing every conditional Allow on this path.
func TestCheckPolicyResources_ConditionalAllowHonoursSourceIP(t *testing.T) {
	gw := withVerifier(&GatewayConfig{
		DisableLogging: true,
		IAMService: conditionedPolicyIAMService("Allow", map[string]map[string]iamdomain.ConditionValue{
			iampolicy.OpIPAddress: {iampolicy.KeySourceIP: {"10.0.0.0/8"}},
		}),
	})

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
			req = req.WithContext(withPrincipalField(req.Context(), fieldAccountID, "000000000001"))

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
	gw := withVerifier(&GatewayConfig{
		DisableLogging: true,
		IAMService: conditionedPolicyIAMService("Deny", map[string]map[string]iamdomain.ConditionValue{
			iampolicy.OpIPAddress: {iampolicy.KeySourceIP: {"203.0.113.0/24"}},
		}),
	})

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
			req = req.WithContext(withPrincipalField(req.Context(), fieldAccountID, "000000000001"))

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
