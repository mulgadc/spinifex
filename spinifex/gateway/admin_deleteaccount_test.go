package gateway

//test:in-package — the request validators, the job staleness rule and the
// engine-error mapping are unexported, and each is a safety rail that would go
// untested if it could only be reached through a live cluster.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/accountteardown"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// listAccountsIAMService answers the one call /admin/ListAccounts makes.
type listAccountsIAMService struct {
	policyMockIAMService

	accounts []*handlers_iam.Account
	err      error
}

func (m *listAccountsIAMService) ListAccounts() ([]*handlers_iam.Account, error) {
	return m.accounts, m.err
}

// A real delete needs the name confirmation and a client token; a dry run
// deletes nothing, so it needs neither.
func TestValidateDeleteAccountRequest(t *testing.T) {
	token := strings.Repeat("a", clientTokenMinLen)

	tests := []struct {
		name    string
		request DeleteAccountRequest
		wantErr string
	}{
		{
			name:    "complete request",
			request: DeleteAccountRequest{AccountID: "000000000042", AccountName: "t@example.com", ClientToken: token},
		},
		{
			name:    "dry run needs only the account id",
			request: DeleteAccountRequest{AccountID: "000000000042", DryRun: true},
		},
		{
			name:    "no account id",
			request: DeleteAccountRequest{AccountName: "t@example.com", ClientToken: token},
			wantErr: awserrors.ErrorMissingParameter,
		},
		{
			name:    "account id is not twelve digits",
			request: DeleteAccountRequest{AccountID: "42", AccountName: "t@example.com", ClientToken: token},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name:    "account id is not numeric",
			request: DeleteAccountRequest{AccountID: "00000000004x", AccountName: "t@example.com", ClientToken: token},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name:    "no name confirmation",
			request: DeleteAccountRequest{AccountID: "000000000042", ClientToken: token},
			wantErr: awserrors.ErrorMissingParameter,
		},
		{
			name:    "no client token",
			request: DeleteAccountRequest{AccountID: "000000000042", AccountName: "t@example.com"},
			wantErr: awserrors.ErrorMissingParameter,
		},
		{
			name:    "client token too short",
			request: DeleteAccountRequest{AccountID: "000000000042", AccountName: "t@example.com", ClientToken: "short"},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "account name too long",
			request: DeleteAccountRequest{
				AccountID:   "000000000042",
				AccountName: strings.Repeat("x", accountNameMaxLen+1),
				ClientToken: token,
			},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDeleteAccountRequest(&tc.request)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Equal(t, tc.wantErr, err.Error())
		})
	}
}

// Whitespace around a confirmation name is a copy-paste artefact, not a
// mismatch — but it must be trimmed before the comparison, not after.
func TestValidateDeleteAccountRequestTrimsInput(t *testing.T) {
	request := DeleteAccountRequest{
		AccountID:   "  000000000042 ",
		AccountName: " t@example.com ",
		ClientToken: " " + strings.Repeat("a", clientTokenMinLen) + " ",
	}

	require.NoError(t, validateDeleteAccountRequest(&request))

	assert.Equal(t, "000000000042", request.AccountID)
	assert.Equal(t, "t@example.com", request.AccountName)
	assert.Equal(t, strings.Repeat("a", clientTokenMinLen), request.ClientToken)
}

// A job is retryable only once its gateway has stopped heartbeating. Getting
// this wrong in either direction is bad: too eager runs two teardowns on one
// account, too slow wedges the account until an operator intervenes.
func TestDeletionJobStale(t *testing.T) {
	now := time.Now().UTC()

	tests := []struct {
		name      string
		updatedAt string
		want      bool
	}{
		{"heartbeat just now", now.Format(time.RFC3339), false},
		{"heartbeat within the window", now.Add(-accountDeletionStale / 2).Format(time.RFC3339), false},
		{"heartbeat stopped", now.Add(-2 * accountDeletionStale).Format(time.RFC3339), true},
		{"no heartbeat recorded", "", true},
		{"unparseable heartbeat", "yesterday", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			job := &accountDeletionJob{UpdatedAt: tc.updatedAt}
			assert.Equal(t, tc.want, deletionJobStale(job, now))
		})
	}
}

// A protected account must answer exactly as an unauthorized caller would: no
// credential grants deleting it, so the refusal describes nothing.
func TestTeardownErrorMapping(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"protected account", accountteardown.ErrProtectedAccount, awserrors.ErrorAccessDenied},
		{
			"wrapped protected account",
			fmt.Errorf("000000000001 is the super admin account: %w", accountteardown.ErrProtectedAccount),
			awserrors.ErrorAccessDenied,
		},
		{"name mismatch", accountteardown.ErrAccountNameMismatch, awserrors.ErrorIdempotentParameterMismatch},
		{"no such account", errors.New("account not found: 000000000042"), awserrors.ErrorIAMNoSuchEntity},
		{"anything else", errors.New("nats: timeout"), awserrors.ErrorInternalError},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := teardownError("DeleteAccount", "000000000042", tc.err)
			require.Error(t, got)
			assert.Equal(t, tc.want, got.Error())
		})
	}
}

// The listing is an operator's index of tenants. It must carry the status —
// that is how a stuck TERMINATING account is noticed — and no key material.
func TestAdminListAccountsReportsStatus(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, IAMService: &listAccountsIAMService{
		accounts: []*handlers_iam.Account{
			{AccountID: "000000000042", AccountName: "t@example.com", Status: handlers_iam.AccountStatusActive},
			nil,
			{AccountID: "000000000043", AccountName: "u@example.com", Status: handlers_iam.AccountStatusTerminating},
		},
	}}

	output, err := gw.adminListAccounts(context.Background(), nil)
	require.NoError(t, err)

	response, ok := output.(*ListAccountsResponse)
	require.True(t, ok)
	assert.Equal(t, 2, response.Count)
	assert.Equal(t, handlers_iam.AccountStatusActive, response.Accounts[0].Status)
	assert.Equal(t, handlers_iam.AccountStatusTerminating, response.Accounts[1].Status)
}

// Failing closed matters here: an empty listing reads as "no accounts", which
// is exactly what an operator would act on.
func TestAdminListAccountsFailsClosed(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, IAMService: &listAccountsIAMService{
		err: errors.New("kv unavailable"),
	}}

	_, err := gw.adminListAccounts(context.Background(), nil)

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
}

// Each admin method is a separate grant. The signup Worker holds
// spinifex:CreateAccount and must not reach deletion with it.
func TestCreateAccountGrantDoesNotAllowDeleteAccount(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, IAMService: createAccountPolicy()}

	for _, method := range []string{"DeleteAccount", "DescribeAccountDeletion", "ListAccounts"} {
		t.Run(method, func(t *testing.T) {
			rec := authorizedAdminRequestForMethod(gw, method, `{}`)

			assert.Equal(t, awserrors.ErrorAccessDenied, decodeAdminError(t, rec).Error.Code)
		})
	}
}

// A missing account id must be refused before anything reads the cluster, so
// DescribeAccountDeletion cannot be used to probe with junk.
func TestDescribeAccountDeletionValidatesTheAccountID(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}

	_, err := gw.adminDescribeAccountDeletion(context.Background(), []byte(`{}`))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())

	_, err = gw.adminDescribeAccountDeletion(context.Background(), []byte(`{"accountId":"nope"}`))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}
