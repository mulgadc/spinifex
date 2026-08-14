//test:in-package — the file exists to expose unexported identifiers to the
// external cmd_test package, which it can only do from inside cmd.

package cmd

import "strconv"

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
