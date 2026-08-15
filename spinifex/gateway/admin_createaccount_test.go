package gateway

//test:in-package — exercises the unexported CreateAccount internals
// (claimClientToken, checkAccountCap, request validation) directly, since the
// idempotency contract cannot be observed from the response body alone.

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testClientToken = "0123456789abcdef0123456789abcdef"

// capMockIAMService reports a fixed account count for the cap check.
type capMockIAMService struct {
	handlers_iam.IAMService

	accounts int
	err      error
}

func (m *capMockIAMService) ListAccounts() ([]*handlers_iam.Account, error) {
	if m.err != nil {
		return nil, m.err
	}
	return make([]*handlers_iam.Account, m.accounts), nil
}

func TestValidateCreateAccountRequest(t *testing.T) {
	tests := []struct {
		name    string
		req     CreateAccountRequest
		wantErr string
	}{
		{
			name: "valid",
			req:  CreateAccountRequest{Name: "ben@example.com", ClientToken: testClientToken},
		},
		{
			name:    "missing name",
			req:     CreateAccountRequest{ClientToken: testClientToken},
			wantErr: awserrors.ErrorMissingParameter,
		},
		{
			name:    "missing client token",
			req:     CreateAccountRequest{Name: "ben@example.com"},
			wantErr: awserrors.ErrorMissingParameter,
		},
		{
			name:    "name is not an email address",
			req:     CreateAccountRequest{Name: "ben", ClientToken: testClientToken},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name:    "name has no dotted domain",
			req:     CreateAccountRequest{Name: "ben@example", ClientToken: testClientToken},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name:    "name has two at signs",
			req:     CreateAccountRequest{Name: "ben@a@example.com", ClientToken: testClientToken},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name:    "name too long",
			req:     CreateAccountRequest{Name: strings.Repeat("a", 250) + "@example.com", ClientToken: testClientToken},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name:    "client token too short",
			req:     CreateAccountRequest{Name: "ben@example.com", ClientToken: "short"},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name:    "client token too long",
			req:     CreateAccountRequest{Name: "ben@example.com", ClientToken: strings.Repeat("a", 129)},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name:    "client token has illegal characters",
			req:     CreateAccountRequest{Name: "ben@example.com", ClientToken: strings.Repeat("a", 31) + "!"},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name:    "source too long",
			req:     CreateAccountRequest{Name: "ben@example.com", ClientToken: testClientToken, Source: strings.Repeat("s", 65)},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req
			err := validateCreateAccountRequest(&req)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Equal(t, tc.wantErr, err.Error())
		})
	}
}

// A validation message names the offending field, never its value: `name` is a
// customer email address.
func TestValidateCreateAccountRequestNeverEchoesTheValue(t *testing.T) {
	req := CreateAccountRequest{Name: "ben@example.com", ClientToken: "short"}
	err := validateCreateAccountRequest(&req)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "short")
}

func TestClientTokenKeyHidesTheToken(t *testing.T) {
	key := clientTokenKey(testClientToken)

	assert.NotContains(t, key, testClientToken)
	assert.Len(t, key, 64)
	assert.NotEqual(t, key, clientTokenKey(testClientToken+"a"))
}

func newIdempotencyBucket(t *testing.T) jetstream.KeyValue {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)

	kv, err := kvutil.GetOrCreateBucketWithTTL(t.Context(), js, KVBucketAdminIdempotency, 1, adminIdempotencyTTL)
	require.NoError(t, err)
	return kv
}

func TestClaimClientToken(t *testing.T) {
	idem := newIdempotencyBucket(t)
	ctx := t.Context()

	// First claim wins and has nothing to replay.
	replay, err := claimClientToken(ctx, idem, testClientToken, "ben@example.com")
	require.NoError(t, err)
	assert.Nil(t, replay)

	// A second call while the first is in flight is retryable, not a duplicate.
	_, err = claimClientToken(ctx, idem, testClientToken, "ben@example.com")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorOperationInProgress, err.Error())

	// Reusing a token for a different name is a client bug, not a retry.
	_, err = claimClientToken(ctx, idem, testClientToken, "someone@example.com")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorIdempotentParameterMismatch, err.Error())
}

func TestClaimClientTokenReplaysCompletedResult(t *testing.T) {
	idem := newIdempotencyBucket(t)
	ctx := t.Context()

	done, err := json.Marshal(adminIdemRecord{
		State: adminIdemDone,
		Name:  "ben@example.com",
		Response: &CreateAccountResponse{
			AccountID:       "000000000042",
			AccountName:     "ben@example.com",
			SecretAccessKey: "ciphertext",
		},
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	})
	require.NoError(t, err)
	_, err = idem.Put(ctx, clientTokenKey(testClientToken), done)
	require.NoError(t, err)

	// The name is normalised on both sides, so case cannot defeat the replay.
	replay, err := claimClientToken(ctx, idem, testClientToken, "BEN@Example.com")
	require.NoError(t, err)
	require.NotNil(t, replay)
	assert.Equal(t, "000000000042", replay.AccountID)
	assert.Equal(t, "ciphertext", replay.SecretAccessKey)
}

func TestCheckAccountCap(t *testing.T) {
	tests := []struct {
		name     string
		max      int
		accounts int
		wantErr  string
	}{
		{name: "under the cap", max: 128, accounts: 127},
		{name: "at the cap", max: 128, accounts: 128, wantErr: awserrors.ErrorIAMLimitExceeded},
		{name: "over the cap", max: 128, accounts: 200, wantErr: awserrors.ErrorIAMLimitExceeded},
		{name: "zero means uncapped", max: 0, accounts: 5000},
		{name: "negative means uncapped", max: -1, accounts: 5000},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gw := &GatewayConfig{
				DisableLogging:    true,
				IAMService:        &capMockIAMService{accounts: tc.accounts},
				SignupMaxAccounts: tc.max,
			}
			err := gw.checkAccountCap()
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Equal(t, tc.wantErr, err.Error())
		})
	}
}

// A cap that cannot be evaluated must not be treated as "not reached".
func TestCheckAccountCapFailsClosedOnListError(t *testing.T) {
	gw := &GatewayConfig{
		DisableLogging:    true,
		IAMService:        &capMockIAMService{err: assert.AnError},
		SignupMaxAccounts: 128,
	}

	err := gw.checkAccountCap()
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
}

// newCreateAccountGateway wires a gateway against a real IAM service, embedded
// JetStream, and a stub daemon answering the default-VPC request.
func newCreateAccountGateway(t *testing.T) *GatewayConfig {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)

	masterKey, err := handlers_iam.GenerateMasterKey()
	require.NoError(t, err)
	svc, err := handlers_iam.NewIAMServiceImpl(t.Context(), nc, masterKey, 1)
	require.NoError(t, err)

	sub, err := nc.Subscribe(utils.SubjectEnsureDefaultVpc, func(msg *nats.Msg) {
		_ = msg.Respond([]byte(`{"vpc_id":"vpc-0a1b2c3d"}`))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	return &GatewayConfig{
		DisableLogging:    true,
		IAMService:        svc,
		NATSConn:          nc,
		SignupMaxAccounts: 128,
	}
}

func createAccountBody(t *testing.T, name, clientToken string) []byte {
	t.Helper()
	body, err := json.Marshal(CreateAccountRequest{Name: name, ClientToken: clientToken, Source: "test"})
	require.NoError(t, err)
	return body
}

func TestAdminCreateAccountProvisionsAndReplays(t *testing.T) {
	gw := newCreateAccountGateway(t)
	ctx := t.Context()
	body := createAccountBody(t, "ben@example.com", testClientToken)

	first, err := gw.adminCreateAccount(ctx, body)
	require.NoError(t, err)
	created := first.(*CreateAccountResponse)

	assert.NotEmpty(t, created.AccountID)
	assert.Equal(t, "ben@example.com", created.AccountName)
	assert.Equal(t, handlers_iam.AdminUserName, created.AdminUser)
	assert.NotEmpty(t, created.AccessKeyID)
	assert.NotEmpty(t, created.SecretAccessKey)
	assert.Equal(t, "vpc-0a1b2c3d", created.DefaultVpcID)

	// The replay is the only way to re-obtain the secret, so it must match.
	second, err := gw.adminCreateAccount(ctx, body)
	require.NoError(t, err)
	assert.Equal(t, created, second.(*CreateAccountResponse))

	accounts, err := gw.IAMService.ListAccounts()
	require.NoError(t, err)
	assert.Len(t, accounts, 1, "replay must not provision a second account")
}

func TestAdminCreateAccountRejectsDuplicateName(t *testing.T) {
	gw := newCreateAccountGateway(t)
	ctx := t.Context()

	_, err := gw.adminCreateAccount(ctx, createAccountBody(t, "ben@example.com", testClientToken))
	require.NoError(t, err)

	// Different token, same name: a second person cannot take an existing name,
	// and the error discloses nothing about the account that holds it.
	_, err = gw.adminCreateAccount(ctx, createAccountBody(t, "BEN@example.com", strings.Repeat("b", 32)))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccountAlreadyExists, err.Error())
}

func TestAdminCreateAccountEnforcesCap(t *testing.T) {
	gw := newCreateAccountGateway(t)
	gw.SignupMaxAccounts = 1
	ctx := t.Context()

	_, err := gw.adminCreateAccount(ctx, createAccountBody(t, "ben@example.com", testClientToken))
	require.NoError(t, err)

	_, err = gw.adminCreateAccount(ctx, createAccountBody(t, "second@example.com", strings.Repeat("c", 32)))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorIAMLimitExceeded, err.Error())
}

// A rejected request must release its token claim, or the caller is told
// OperationInProgress until the TTL expires.
func TestAdminCreateAccountReleasesTokenAfterFailure(t *testing.T) {
	gw := newCreateAccountGateway(t)
	gw.SignupMaxAccounts = 1
	ctx := t.Context()

	_, err := gw.adminCreateAccount(ctx, createAccountBody(t, "ben@example.com", testClientToken))
	require.NoError(t, err)

	capped := createAccountBody(t, "second@example.com", strings.Repeat("c", 32))
	_, err = gw.adminCreateAccount(ctx, capped)
	require.Error(t, err)

	gw.SignupMaxAccounts = 128
	_, err = gw.adminCreateAccount(ctx, capped)
	assert.NoError(t, err)
}

func TestAdminCreateAccountMalformedBody(t *testing.T) {
	gw := newCreateAccountGateway(t)

	_, err := gw.adminCreateAccount(t.Context(), []byte("not json"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidRequest, err.Error())
}

// The secret access key is the one value in this flow that must never be
// recoverable from a log, at any level.
func TestAdminCreateAccountNeverLogsTheSecret(t *testing.T) {
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	gw := newCreateAccountGateway(t)
	out, err := gw.adminCreateAccount(t.Context(), createAccountBody(t, "ben@example.com", testClientToken))
	require.NoError(t, err)

	created := out.(*CreateAccountResponse)
	require.NotEmpty(t, created.SecretAccessKey)
	assert.NotContains(t, logged.String(), created.SecretAccessKey)
	assert.NotContains(t, logged.String(), testClientToken)
}
