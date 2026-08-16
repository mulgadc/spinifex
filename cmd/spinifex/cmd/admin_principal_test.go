package cmd_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/cmd/spinifex/cmd"
	"github.com/mulgadc/spinifex/spinifex/gateway"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// policyActions returns the actions an inline policy document allows.
func policyActions(t *testing.T, document string) []string {
	t.Helper()

	var doc struct {
		Statement []struct {
			Effect string                   `json:"Effect"`
			Action handlers_iam.StringOrArr `json:"Action"`
		} `json:"Statement"`
	}
	require.NoError(t, json.Unmarshal([]byte(document), &doc))

	var actions []string
	for _, statement := range doc.Statement {
		if statement.Effect != "Allow" {
			continue
		}
		actions = append(actions, statement.Action...)
	}
	return actions
}

// A principal minted without --grant is the operator credential, so it has to
// cover the surface the gateway actually serves rather than a list that can
// fall behind it.
func TestPrincipalGrantsDefaultToEveryAdminMethod(t *testing.T) {
	grants, err := cmd.ResolvePrincipalGrants(nil)
	require.NoError(t, err)

	var want []string
	for _, method := range gateway.AdminMethodNames() {
		want = append(want, "spinifex:"+method)
	}
	assert.ElementsMatch(t, want, grants)
}

// Scoping is the whole point of the flag: a signup credential holds exactly
// CreateAccount, so a leaked key cannot remove a tenant.
func TestPrincipalGrantsScopeToTheRequestedMethods(t *testing.T) {
	grants, err := cmd.ResolvePrincipalGrants([]string{"CreateAccount"})
	require.NoError(t, err)

	assert.Equal(t, []string{"spinifex:CreateAccount"}, grants)
}

// Operators write the method either way, and a duplicate is a typo rather than
// a reason to refuse.
func TestPrincipalGrantsAcceptQualifiedAndRepeatedNames(t *testing.T) {
	grants, err := cmd.ResolvePrincipalGrants([]string{
		"spinifex:ListAccounts", "listaccounts", " DeleteAccount ",
	})
	require.NoError(t, err)

	assert.Equal(t, []string{"spinifex:DeleteAccount", "spinifex:ListAccounts"}, grants)
}

// A misspelt method must fail rather than mint a key that silently authorises
// nothing — the caller would only find out at the first AccessDenied.
func TestPrincipalGrantsRejectAnUnknownMethod(t *testing.T) {
	_, err := cmd.ResolvePrincipalGrants([]string{"DeleteEverything"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "DeleteEverything")
	assert.Contains(t, err.Error(), "CreateAccount")
}

// A wildcard here would authorise whatever is added to the surface next with a
// key minted before it existed.
func TestPrincipalPolicyNamesEveryMethod(t *testing.T) {
	grants, err := cmd.ResolvePrincipalGrants(nil)
	require.NoError(t, err)

	actions := policyActions(t, cmd.PrincipalPolicyDocument(grants))

	assert.ElementsMatch(t, grants, actions)
	for _, action := range actions {
		assert.NotContains(t, action, "*", "an admin grant must name its method")
	}
}

// The listing reads back what the create path wrote, so a principal's grants
// can be audited without decoding the document by hand.
func TestPrincipalPolicyActionsRoundTrip(t *testing.T) {
	grants, err := cmd.ResolvePrincipalGrants([]string{"CreateAccount", "ListAccounts"})
	require.NoError(t, err)

	assert.Equal(t, []string{"CreateAccount", "ListAccounts"},
		cmd.PrincipalPolicyActions(cmd.PrincipalPolicyDocument(grants)))
}

// A document nobody can parse is reported as no grants rather than as a guess.
func TestPrincipalPolicyActionsIgnoreAnUndecodableDocument(t *testing.T) {
	assert.Empty(t, cmd.PrincipalPolicyActions("{not json"))
}

// Rotating the super-admin account's own administrator would revoke the
// bootstrap credential the operator is holding.
func TestPrincipalNameRefusesTheAccountAdministrator(t *testing.T) {
	err := cmd.ValidatePrincipalName(handlers_iam.AdminUserName)

	require.Error(t, err)
	assert.Contains(t, err.Error(), handlers_iam.AdminUserName)
}

func TestPrincipalNameRejectsUnusableNames(t *testing.T) {
	assert.Error(t, cmd.ValidatePrincipalName(""))
	assert.Error(t, cmd.ValidatePrincipalName("has space"))
	assert.Error(t, cmd.ValidatePrincipalName(strings.Repeat("a", 65)))

	assert.NoError(t, cmd.ValidatePrincipalName("signup"))
	assert.NoError(t, cmd.ValidatePrincipalName("ops-team_1@example.com"))
}

// The listing prints one line per principal, including a revoked credential
// that holds no key — an operator has to see it to know it is still there.
func TestPrintPrincipalTableRendersEveryPrincipal(t *testing.T) {
	output := captureOutput(t, func() {
		cmd.PrintPrincipalTable("000000000001", []cmd.PrincipalRowSpec{
			{UserName: "admin", Grants: []string{"AdministratorAccess (attached)"}, Keys: 1},
			{UserName: "signup", Grants: []string{"CreateAccount"}, Keys: 1},
			{UserName: "retired", Keys: 0},
		})
	})

	assert.Contains(t, output, "000000000001")
	assert.Contains(t, output, "AdministratorAccess (attached)")
	assert.Contains(t, output, "signup")
	assert.Contains(t, output, "retired")
	assert.Contains(t, output, "3 principal(s)")
}

// A principal with nothing granted must read as a dash. A blank cell reads as
// a rendering fault rather than as an answer.
func TestPrintPrincipalTableMarksAnEmptyGrant(t *testing.T) {
	output := captureOutput(t, func() {
		cmd.PrintPrincipalTable("000000000001", []cmd.PrincipalRowSpec{{UserName: "retired"}})
	})

	assert.Contains(t, output, "-")
}

// The local and remote listings print through one function, so a tenant that is
// TERMINATING reads the same either way — which is how a stuck teardown is
// noticed.
func TestAccountSummariesCarryStatus(t *testing.T) {
	summaries := cmd.AccountSummaries([]*handlers_iam.Account{
		{AccountID: "000000000042", AccountName: "t@example.com", Status: handlers_iam.AccountStatusActive},
		nil,
		{AccountID: "000000000043", AccountName: "u@example.com", Status: handlers_iam.AccountStatusTerminating},
	})

	require.Len(t, summaries, 2)
	assert.Equal(t, handlers_iam.AccountStatusTerminating, summaries[1].Status)
}

func TestPrintAccountTableRendersEveryAccount(t *testing.T) {
	output := captureOutput(t, func() {
		cmd.PrintAccountTable([]gateway.AccountSummary{
			{
				AccountID:   "000000000042",
				AccountName: "tenant@example.com",
				Status:      handlers_iam.AccountStatusTerminating,
				CreatedAt:   "2026-08-16T07:00:00Z",
			},
		})
	})

	assert.Contains(t, output, "000000000042")
	assert.Contains(t, output, "tenant@example.com")
	assert.Contains(t, output, handlers_iam.AccountStatusTerminating)
	assert.Contains(t, output, "2026-08-16 07:00")
}

// An empty listing must say so. A bare header reads as truncated output.
func TestPrintAccountTableSaysWhenThereAreNone(t *testing.T) {
	output := captureOutput(t, func() { cmd.PrintAccountTable(nil) })

	assert.Contains(t, output, "No accounts found")
}
