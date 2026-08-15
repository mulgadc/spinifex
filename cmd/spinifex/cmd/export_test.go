//test:in-package — the file exists to expose unexported identifiers to the
// external cmd_test package, which it can only do from inside cmd.

package cmd

import (
	"errors"
	"strconv"
	"time"
)

// Test hooks for the external cmd_test package.

// OpenFormationPort exposes openFormationPort for testing.
var OpenFormationPort = openFormationPort

// SetFirewallApplyHelper points the firewall helper at a stub and returns the
// function that restores it.
func SetFirewallApplyHelper(path string) func() {
	orig := firewallApplyHelper
	firewallApplyHelper = path
	return func() { firewallApplyHelper = orig }
}

// InitCLILogger exposes initCLILogger for testing.
var InitCLILogger = initCLILogger

// CLILogLevel exposes the CLI logger's level var for testing.
var CLILogLevel = cliLogLevel

// SetVerboseFlag sets the root --verbose flag and returns the function that
// restores it.
func SetVerboseFlag(v bool) func() {
	flag := rootCmd.PersistentFlags().Lookup("verbose")
	orig := flag.Value.String()
	_ = flag.Value.Set(strconv.FormatBool(v))
	return func() { _ = flag.Value.Set(orig) }
}

// TrustsWholeAccount exposes trustsWholeAccount for testing.
var TrustsWholeAccount = trustsWholeAccount

// LocalGatewayEndpoint exposes localGatewayEndpoint for testing.
var LocalGatewayEndpoint = localGatewayEndpoint

// NewClientToken exposes newClientToken for testing.
var NewClientToken = newClientToken

// AdminHTTPClient exposes adminHTTPClient for testing.
var AdminHTTPClient = adminHTTPClient

// CreateAccountRemote exposes createAccountRemote for testing.
var CreateAccountRemote = createAccountRemote

// AdminTarget builds the unexported target struct for testing.
func AdminTarget(endpoint, region, caBundle string) adminTarget {
	return adminTarget{endpoint: endpoint, region: region, caBundle: caBundle}
}

// DecodeAdminError exposes decodeAdminError for testing.
var DecodeAdminError = decodeAdminError

// RetryableAdminError reports whether the CLI would suggest retrying err.
func RetryableAdminError(err error) bool {
	var adminErr *adminError
	return errors.As(err, &adminErr) && retryableAdminErrors[adminErr.Code]
}

// ConsoleRegion exposes consoleRegion for testing.
var ConsoleRegion = consoleRegion

// DeleteAccountRemote exposes deleteAccountRemote for testing.
var DeleteAccountRemote = deleteAccountRemote

// DescribeAccountDeletionRemote exposes describeAccountDeletionRemote for testing.
var DescribeAccountDeletionRemote = describeAccountDeletionRemote

// FollowAccountDeletion exposes followAccountDeletion for testing.
var FollowAccountDeletion = followAccountDeletion

// SetAccountDeletePollInterval shortens the follow loop's poll and returns the
// function that restores it, so a test does not wait an operator's interval.
func SetAccountDeletePollInterval(d time.Duration) func() {
	orig := accountDeletePollInterval
	accountDeletePollInterval = d
	return func() { accountDeletePollInterval = orig }
}

// PrintTeardownPlan exposes printTeardownPlan for testing.
var PrintTeardownPlan = printTeardownPlan

// PrintTeardownResult exposes printTeardownResult for testing.
var PrintTeardownResult = printTeardownResult

// PromptAccountName exposes promptAccountName for testing.
var PromptAccountName = promptAccountName
