package handlers_iam

import (
	"errors"
	"testing"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	rdsFullAccessARN     = "arn:aws:iam::aws:policy/AmazonRDSFullAccess"
	rdsReadOnlyAccessARN = "arn:aws:iam::aws:policy/AmazonRDSReadOnlyAccess"
)

// The four internal RDS agent actions. Written out rather than imported from
// handlers_rds, which imports this package.
var rdsInternalActions = []string{
	"rds:RegisterDBInstance",
	"rds:SubmitDBStateChange",
	"rds:PollDBCommands",
	"rds:GetDBBootstrapConfig",
}

func TestParseBuiltinManagedPolicies(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		raw     map[string]string
		wantErr string
	}{
		{
			name: "valid document",
			raw: map[string]string{
				"arn:valid": `{"Version":"2012-10-17","Statement":[]}`,
			},
		},
		{
			name: "malformed document",
			raw: map[string]string{
				"arn:malformed": `{`,
			},
			wantErr: "parse managed policy arn:malformed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			parsed, err := parseBuiltinManagedPolicies(tt.raw)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				assert.Nil(t, parsed)
				return
			}
			require.NoError(t, err)
			assert.Len(t, parsed, len(tt.raw))
		})
	}
}

// Serial: mutates the package-level parse error that every constructor reads.
func TestNewIAMServiceImpl_PropagatesBuiltinManagedPolicyParseError(t *testing.T) {
	parseErr := errors.New("malformed builtin policy")
	previousErr := builtinManagedPolicyParseErr
	builtinManagedPolicyParseErr = parseErr
	t.Cleanup(func() { builtinManagedPolicyParseErr = previousErr })

	_, err := NewIAMServiceImpl(t.Context(), nil, make([]byte, 32), 1)
	require.ErrorIs(t, err, parseErr)
	assert.ErrorContains(t, err, "init builtin managed policies")
}

// A role whose permissions come entirely from an attached AWS-managed ARN has to
// resolve to a grant, the way the stock EKS roles already do, rather than fail
// closed on a document Spinifex never stores.
func TestResolveAttachedPolicy_RDSManagedShimsResolve(t *testing.T) {
	t.Parallel()
	s := &IAMServiceImpl{}
	for _, arn := range []string{rdsFullAccessARN, rdsReadOnlyAccessARN} {
		doc, include, err := s.resolveAttachedPolicy(t.Context(), "123456789012", arn)
		require.NoError(t, err, "arn %q", arn)
		require.True(t, include, "arn %q must resolve to a grant", arn)
		assert.NotEmpty(t, doc.Statement, "arn %q must carry statements", arn)
	}
}

// AmazonRDSFullAccess is what a stock admin role carries, and it must not read as
// a path to the master password. The gateway refuses the internal actions by
// principal class regardless, but GetPolicyVersion must not claim otherwise.
func TestRDSManagedShims_GrantNoInternalAction(t *testing.T) {
	t.Parallel()
	for _, arn := range []string{rdsFullAccessARN, rdsReadOnlyAccessARN} {
		doc, ok := builtinManagedPolicyDoc(arn)
		require.True(t, ok, "arn %q", arn)
		for _, action := range rdsInternalActions {
			assert.Equal(t, iampolicy.Deny, iampolicy.EvaluateWithKeys(action, "*", []PolicyDocument{doc}, nil),
				"%s must not grant %s", arn, action)
		}
	}
}

func TestRDSFullAccessShim_GrantsTheCustomerSurface(t *testing.T) {
	t.Parallel()
	doc, ok := builtinManagedPolicyDoc(rdsFullAccessARN)
	require.True(t, ok)

	granted := []string{
		"rds:CreateDBInstance", "rds:DescribeDBInstances", "rds:ModifyDBInstance",
		"rds:DeleteDBInstance", "rds:RebootDBInstance", "rds:StartDBInstance", "rds:StopDBInstance",
		"rds:CreateDBSnapshot", "rds:DeleteDBSnapshot", "rds:RestoreDBInstanceFromDBSnapshot",
		"rds:DescribeDBInstanceAutomatedBackups",
		"rds:CreateDBSubnetGroup", "rds:DeleteDBSubnetGroup",
		"rds:CreateDBParameterGroup", "rds:ModifyDBParameterGroup", "rds:DescribeDBParameters",
		"rds:DeleteDBParameterGroup",
		"rds:AddTagsToResource", "rds:RemoveTagsFromResource", "rds:ListTagsForResource",
		"rds:DescribeEvents",
	}
	for _, action := range granted {
		assert.Equal(t, iampolicy.Allow, iampolicy.EvaluateWithKeys(action, "*", []PolicyDocument{doc}, nil),
			"AmazonRDSFullAccess must grant %s", action)
	}
}

// Read-only means read-only: the shim is the whole grant for a role that carries
// nothing else, so a mutation reaching Allow here would be the only check missed.
func TestRDSReadOnlyAccessShim_GrantsReadsOnly(t *testing.T) {
	t.Parallel()
	doc, ok := builtinManagedPolicyDoc(rdsReadOnlyAccessARN)
	require.True(t, ok)

	for _, action := range []string{"rds:DescribeDBInstances", "rds:DescribeDBSnapshots",
		"rds:DescribeDBParameters", "rds:ListTagsForResource"} {
		assert.Equal(t, iampolicy.Allow, iampolicy.EvaluateWithKeys(action, "*", []PolicyDocument{doc}, nil),
			"AmazonRDSReadOnlyAccess must grant %s", action)
	}
	for _, action := range []string{"rds:CreateDBInstance", "rds:DeleteDBInstance",
		"rds:ModifyDBInstance", "rds:StopDBInstance", "rds:AddTagsToResource",
		"rds:RestoreDBInstanceFromDBSnapshot"} {
		assert.Equal(t, iampolicy.Deny, iampolicy.EvaluateWithKeys(action, "*", []PolicyDocument{doc}, nil),
			"AmazonRDSReadOnlyAccess must not grant %s", action)
	}
}

// An RDS-looking managed ARN Spinifex does not model resolves to no grant rather
// than an error, so a role carrying it is denied that grant and not broken.
func TestResolveAttachedPolicy_UnmodeledRDSManagedARNGrantsNothing(t *testing.T) {
	t.Parallel()
	s := &IAMServiceImpl{}
	for _, arn := range []string{
		"arn:aws:iam::aws:policy/AmazonRDSDataFullAccess",
		"arn:aws:iam::aws:policy/AmazonRDSFullAccess2",
		"arn:aws:iam::aws:policy/service-role/AmazonRDSEnhancedMonitoringRole",
	} {
		doc, include, err := s.resolveAttachedPolicy(t.Context(), "123456789012", arn)
		require.NoError(t, err, "arn %q", arn)
		assert.False(t, include, "arn %q must resolve to no grant", arn)
		assert.Empty(t, doc.Statement)
	}
}
