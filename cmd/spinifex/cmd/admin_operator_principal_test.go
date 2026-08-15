package cmd_test

import (
	"encoding/json"
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

	// Action is a string in one document and an array in the other, which is
	// what StringOrArr exists for.
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

// Each admin method is a separate grant. A wildcard here would authorise
// whatever is added to the surface next with a key minted before it existed.
func TestOperatorPrincipalPolicyGrantsMethodsByName(t *testing.T) {
	actions := policyActions(t, cmd.OperatorPrincipalPolicyDocument)

	assert.ElementsMatch(t, []string{
		"spinifex:CreateAccount",
		"spinifex:DeleteAccount",
		"spinifex:DescribeAccountDeletion",
		"spinifex:ListAccounts",
	}, actions)

	for _, action := range actions {
		assert.NotContains(t, action, "*", "an admin grant must name its method")
	}
}

// The printed summary is what an operator records alongside the key, so it has
// to be the policy that was actually attached.
func TestOperatorPrincipalActionsMatchThePolicy(t *testing.T) {
	assert.ElementsMatch(t,
		policyActions(t, cmd.OperatorPrincipalPolicyDocument), cmd.OperatorPrincipalActions)
}

// The signup Worker's credential holds exactly one action and must never gain
// deletion: widening it turns a public signup form into a way to remove
// accounts.
func TestSignupPrincipalStillGrantsOnlyCreateAccount(t *testing.T) {
	actions := policyActions(t, cmd.SignupPrincipalPolicyDocument)

	assert.Equal(t, []string{"spinifex:CreateAccount"}, actions)
}

// The two credentials are separate so revoking either costs nothing else.
func TestOperatorAndSignupPrincipalsAreDifferentUsers(t *testing.T) {
	assert.NotEqual(t, cmd.SignupPrincipalUserName, cmd.OperatorPrincipalUserName)
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
