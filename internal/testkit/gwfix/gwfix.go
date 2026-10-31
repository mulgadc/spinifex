// Package gwfix holds runtime-free fixtures for gateway-level tests: SigV4
// signing helpers, identity/session doubles and policy builders. It must not
// import runtime/ or operator/ so gateway tests stay free of them.
package gwfix

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsv1 "github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/require"
)

const (
	AccessKey = "AKIAIOSFODNN7EXAMPLE"                     //nolint:gosec // AWS documentation example key
	SecretKey = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY" //nolint:gosec // AWS documentation example secret
	Region    = "us-east-1"
	Service   = "ec2"

	SessionAKID  = "ASIATESTSESSIONAAAAA"
	SessionToken = "wire-session-token-plain"
	RoleARN      = "arn:aws:iam::123456789012:role/test-role"
	AssumedARN   = "arn:aws:sts::123456789012:assumed-role/test-role/test-session"

	PrincipalTypeUser        = "user"
	PrincipalTypeAssumedRole = "assumed-role"
	PrincipalTypeRoot        = "root"
)

// IAM implements iamdomain.IAMService for auth tests. It embeds the interface
// so it satisfies the full contract; only the methods the SigV4 path drives are
// wired, and any other method nil-panics to surface an unexpected call.
type IAM struct {
	iamdomain.IAMService

	AccessKeys map[string]*iamdomain.AccessKey
	MasterKey  []byte

	// Accounts overrides the status the auth gate reads. An empty map means
	// every account is ACTIVE.
	Accounts map[string]*iamdomain.Account

	// GetUserFn overrides the synthetic user record, for a test that cares what
	// aws:userid resolves to or that it fails to resolve at all.
	GetUserFn func(accountID string, input *iam.GetUserInput) (*iam.GetUserOutput, error)
}

func (m *IAM) LookupAccessKey(accessKeyID string) (*iamdomain.AccessKey, error) {
	ak, ok := m.AccessKeys[accessKeyID]
	if !ok {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return ak, nil
}

func (m *IAM) DecryptSecret(ciphertext string) (string, error) {
	return iamdomain.DecryptSecret(ciphertext, m.MasterKey)
}

func (m *IAM) GetAccount(accountID string) (*iamdomain.Account, error) {
	if account, ok := m.Accounts[accountID]; ok {
		return account, nil
	}
	return &iamdomain.Account{AccountID: accountID, Status: iamdomain.AccountStatusActive}, nil
}

// GetUser answers with a unique ID derived from the name, so aws:userid resolves
// on the authorization path without every test defining a user record.
func (m *IAM) GetUser(accountID string, input *iam.GetUserInput) (*iam.GetUserOutput, error) {
	if m.GetUserFn != nil {
		return m.GetUserFn(accountID, input)
	}
	name := awsv1.StringValue(input.UserName)
	return &iam.GetUserOutput{User: &iam.User{
		UserName: awsv1.String(name),
		UserId:   awsv1.String("AIDA" + strings.ToUpper(name)),
	}}, nil
}

// PolicyIAM adds configurable user and role policy lookups to IAM.
type PolicyIAM struct {
	IAM

	GetUserPoliciesFn func(accountID, userName string) ([]iamdomain.PolicyDocument, error)
	GetRolePoliciesFn func(accountID, roleName string) ([]iamdomain.PolicyDocument, error)
}

func (m *PolicyIAM) GetUserPolicies(accountID, userName string) ([]iamdomain.PolicyDocument, error) {
	if m.GetUserPoliciesFn != nil {
		return m.GetUserPoliciesFn(accountID, userName)
	}
	return nil, nil
}

func (m *PolicyIAM) GetRolePolicies(accountID, roleName string) ([]iamdomain.PolicyDocument, error) {
	if m.GetRolePoliciesFn != nil {
		return m.GetRolePoliciesFn(accountID, roleName)
	}
	return nil, nil
}

// ErrorLookupIAM returns a configurable error from LookupAccessKey.
type ErrorLookupIAM struct {
	IAM

	Err error
}

func (m *ErrorLookupIAM) LookupAccessKey(_ string) (*iamdomain.AccessKey, error) {
	return nil, m.Err
}

// MasterKey is a fixed 32-byte key for deterministic secret encryption in tests.
var MasterKey = bytes.Repeat([]byte{0x5a}, 32)

// STS implements the session-credential half of stsdomain.STSService.
type STS struct {
	stsdomain.STSService

	Sessions  map[string]*stsdomain.SessionCredential
	Tokens    map[string]string // AKID → plaintext wire token for HMAC equivalence
	LookupErr error
	Lookups   atomic.Int32 // counts LookupSessionCredential calls for negative-side-effect assertions

	PrincipalErr    error        // verdict or fault the continuity check returns
	PrincipalChecks atomic.Int32 // counts VerifySessionPrincipal calls, to pin its ordering
}

func (m *STS) LookupSessionCredential(accessKeyID string) (*stsdomain.SessionCredential, error) {
	m.Lookups.Add(1)
	if m.LookupErr != nil {
		return nil, m.LookupErr
	}
	if !strings.HasPrefix(accessKeyID, "ASIA") {
		return nil, nil
	}
	cred, ok := m.Sessions[accessKeyID]
	if !ok {
		return nil, nil
	}
	return cred, nil
}

// VerifySessionToken uses plaintext equality rather than HMAC to keep tests
// simple while preserving the contract shape.
func (m *STS) VerifySessionToken(cred *stsdomain.SessionCredential, wireToken string) bool {
	if cred == nil {
		return false
	}
	want, ok := m.Tokens[cred.AccessKeyID]
	if !ok {
		return false
	}
	return want == wireToken
}

// VerifySessionPrincipal defaults to "still the same principal"; PrincipalErr
// drives the rejection-verdict and dependency-fault branches.
func (m *STS) VerifySessionPrincipal(cred *stsdomain.SessionCredential) (*stsdomain.SessionPrincipal, error) {
	m.PrincipalChecks.Add(1)
	if m.PrincipalErr != nil {
		return nil, m.PrincipalErr
	}
	return StubSessionPrincipal(cred)
}

// StubSessionPrincipal is the shared "principal unchanged" answer for the
// session doubles. It derives RoleName from the underlying role ARN rather than
// the session name, because that is what the real implementation returns: a
// stub echoing SessionName would let a door read the wrong field and stay green.
func StubSessionPrincipal(cred *stsdomain.SessionCredential) (*stsdomain.SessionPrincipal, error) {
	if cred == nil {
		return nil, errors.New("nil session credential")
	}
	roleName := cred.UnderlyingRoleARN
	if idx := strings.LastIndex(roleName, "/"); idx >= 0 {
		roleName = roleName[idx+1:]
	}
	return &stsdomain.SessionPrincipal{
		UserID:   cred.UserID,
		RoleID:   cred.RoleID,
		RoleName: roleName,
		RoleARN:  cred.UnderlyingRoleARN,
	}, nil
}

// SignRequest signs req in place with the aws-sdk-go-v2 v4 signer — the same
// path production callers use via gwsign — so the gateway verifies a real SDK
// signature. body must match what the middleware reads from r.Body so the payload
// hash agrees. An optional signingTime pins the clock for skew tests.
func SignRequest(t *testing.T, req *http.Request, body []byte, accessKey, secret string, signingTime ...time.Time) {
	t.Helper()
	when := time.Now().UTC()
	if len(signingTime) > 0 {
		when = signingTime[0]
	}
	SignRequestAs(t, req, body, accessKey, secret, Service, when)
}

// SignRequestAs signs with an explicit credential-scope service, so a test
// can drive a scope the gateway does not serve.
func SignRequestAs(t *testing.T, req *http.Request, body []byte, accessKey, secret, service string, when time.Time) {
	t.Helper()
	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])
	// gwsign sets this header so the SDK signs it; sigv4 reproduces it when
	// reconstructing the canonical request.
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	require.NoError(t, v4.NewSigner().SignHTTP(context.Background(),
		aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secret},
		req, payloadHash, service, Region, when))
}

// SignSessionRequest sets X-Amz-Security-Token before signing so it's included
// in SignedHeaders, matching what the AWS CLI/SDK do with session credentials.
func SignSessionRequest(t *testing.T, req *http.Request, body []byte, accessKey, secret, sessionToken string) {
	t.Helper()
	req.Header.Set("X-Amz-Security-Token", sessionToken)
	SignRequest(t, req, body, accessKey, secret)
}

// AllowPolicy returns a single-statement Allow policy granting action on "*".
func AllowPolicy(action string) []iamdomain.PolicyDocument {
	return AllowPolicyResource(action, "*")
}

// AllowPolicyResource returns a single-statement Allow policy for action on resource.
func AllowPolicyResource(action, resource string) []iamdomain.PolicyDocument {
	return []iamdomain.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []iamdomain.Statement{{
			Effect:   "Allow",
			Action:   iamdomain.StringOrArr{action},
			Resource: iamdomain.StringOrArr{resource},
		}},
	}}
}

// AssumedRoleSessionCred builds an assumed-role SessionCredential.
// SecretEncrypted is filled in by the gateway builder.
func AssumedRoleSessionCred(sessionName, underlyingRoleARN, accountID string) *stsdomain.SessionCredential {
	return &stsdomain.SessionCredential{
		AccessKeyID:       SessionAKID,
		AccountID:         accountID,
		PrincipalType:     PrincipalTypeAssumedRole,
		AssumedRoleARN:    "arn:aws:sts::" + accountID + ":assumed-role/role/" + sessionName,
		UnderlyingRoleARN: underlyingRoleARN,
		SessionName:       sessionName,
		ExpiresAt:         time.Now().UTC().Add(time.Hour),
		CreatedAt:         time.Now().UTC().Add(-time.Minute),
	}
}
