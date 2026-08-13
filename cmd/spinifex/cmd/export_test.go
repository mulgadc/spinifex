//test:in-package — the file exists to expose unexported identifiers to the
// external cmd_test package, which it can only do from inside cmd.

package cmd

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
