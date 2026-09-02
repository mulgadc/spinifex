package gateway_test

import (
	"slices"
	"testing"

	gateway_rds "github.com/mulgadc/spinifex/spinifex/gateway/rds"
	"github.com/stretchr/testify/assert"
)

// RDS carries its scopes on the dispatch table itself, so the two cannot
// disagree the way the other services' can. What can still go wrong is an action
// landing with no scopes and nobody noticing it now evaluates against "*", so
// the gate is the account-level half: every entry here is a written decision
// that the action addresses no single resource.
var rdsAccountLevelActions = map[string]string{
	// Creates: the resource does not exist yet, which is how AWS evaluates them.
	"CreateDBInstance":       "the instance does not exist until this call makes it",
	"CreateDBSubnetGroup":    "the subnet group does not exist until this call makes it",
	"CreateDBParameterGroup": "the parameter group does not exist until this call makes it",

	// Describes that filter rather than address one resource.
	"DescribeDBInstances":                "filters the account's instances",
	"DescribeDBSnapshots":                "filters the account's snapshots",
	"DescribeDBInstanceAutomatedBackups": "filters the account's automated backups",
	"DescribeDBSubnetGroups":             "filters the account's subnet groups",
	"DescribeDBParameterGroups":          "filters the account's parameter groups",
	"DescribeEvents":                     "the event ring is per-account and a filter names no one resource",

	// Catalogs read no per-account state at all.
	"DescribeDBEngineVersions":           "a static engine catalog, not tenant state",
	"DescribeOrderableDBInstanceOptions": "cluster capability, not tenant state",

	// Internal agent actions. AuthorizeCaller refuses these to every customer
	// principal by class before any policy is evaluated, so a resource scope
	// would narrow nothing.
	"RegisterDBInstance":     "agent-only, gated by principal class",
	"SubmitDBStateChange":    "agent-only, gated by principal class",
	"PollDBCommands":         "agent-only, gated by principal class",
	"GetDBBootstrapConfig":   "agent-only, gated by principal class",
	"AcknowledgeDBBootstrap": "agent-only, gated by principal class",
}

// TestRDSScopeTableIsExhaustive is what stops the next RDS action being added
// with a silent account-wide grant. Every served action is either scoped or
// carries a written reason for being account-level, and an action in both sets
// or in neither fails.
func TestRDSScopeTableIsExhaustive(t *testing.T) {
	unsupported := gateway_rds.UnsupportedActionNames()

	for _, action := range gateway_rds.ActionNames() {
		if slices.Contains(unsupported, action) {
			continue
		}
		_, accountLevel := rdsAccountLevelActions[action]
		assert.NotEqual(t, gateway_rds.HasScope(action), accountLevel,
			"rds action %q must either carry a scope in gateway/rds/handler.go or a written reason "+
				"in rdsAccountLevelActions, not both and not neither", action)
	}

	for action := range rdsAccountLevelActions {
		assert.True(t, gateway_rds.HasAction(action),
			"rdsAccountLevelActions names %q, which RDS does not serve: remove it", action)
		assert.NotContains(t, unsupported, action,
			"rdsAccountLevelActions names %q, which is recognised but not offered: remove it", action)
	}
}
