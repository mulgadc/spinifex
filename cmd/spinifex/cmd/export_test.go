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
