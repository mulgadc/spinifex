package handlers_rds

//test:in-package — replaces the unexported CA dependency, drives the
// resolveGroupParameters funnel directly and reads the enforcement defaults off
// the unexported engine registry.

import (
	"crypto/rsa"
	"crypto/x509"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testMariaDBParameterGroup = "tuned-mariadb"

// A deployment holding no cluster CA, which is the only state the gate refuses.
// admin init writes one onto every node and the join hard-fails without it, so
// this is a state a formed deployment cannot reach.
func withoutClusterCA(t *testing.T, svc *Service) {
	t.Helper()
	svc.deps.LoadCA = func() (*x509.Certificate, *rsa.PrivateKey, error) { return nil, nil, nil }
}

func mariadbParameterGroupInput(name string) *rds.CreateDBParameterGroupInput {
	return &rds.CreateDBParameterGroupInput{
		DBParameterGroupName:   aws.String(name),
		DBParameterGroupFamily: aws.String(engineMariaDB.ParameterGroupFamily()),
		Description:            aws.String("Tuned for the orders workload"),
	}
}

func requireNoTLSToEnforce(t *testing.T, err error, parameter, group string) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterCombination, awserrors.ValidErrorCodeFromError(err),
		"the code has to survive resolution or the client sees a 500")
	assert.Contains(t, err.Error(), parameter, "the error names the parameter asking for TLS")
	assert.Contains(t, err.Error(), group, "and the group carrying it")
	assert.Contains(t, err.Error(), "cluster CA", "and the configuration that would let it stand")
}

// The three states loadCA distinguishes, because the gate reads the middle one
// as a refusal and must not read the last one as one.
func TestTLSAvailable_ReportsWhetherTheDeploymentCanServeTLS(t *testing.T) {
	configured := NewService(nil, testRegion).WithDeps(Deps{LoadCA: newTestCA(t)})
	available, err := configured.tlsAvailable()
	require.NoError(t, err)
	assert.True(t, available)

	unconfigured := NewService(nil, testRegion).WithDeps(Deps{})
	available, err = unconfigured.tlsAvailable()
	require.NoError(t, err, "no CA at all is an answer rather than a failure")
	assert.False(t, available)

	// Half a CA is a misconfiguration, and reporting it as "cannot serve TLS"
	// would turn enforcement off on a deployment that meant to configure one.
	partial := NewService(nil, testRegion).WithDeps(Deps{CACertPath: "/etc/spinifex/ca.pem"})
	available, err = partial.tlsAvailable()
	require.Error(t, err)
	assert.False(t, available)
}

// The whole of the migration story: an instance created with no parameter group
// of its own requires TLS, on both engines, with no customer action.
func TestCreateDBInstance_EnforcesTLSWithNoParameterGroupOfItsOwn(t *testing.T) {
	tests := []struct {
		engine    Engine
		parameter string
	}{
		{enginePostgres, "rds.force_ssl"},
		{engineMariaDB, "require_secure_transport"},
	}
	for _, tc := range tests {
		t.Run(tc.engine.Name, func(t *testing.T) {
			h := newCreateHarness(t, testBaseDomain)
			input := validCreateInput()
			input.Engine = aws.String(tc.engine.Name)

			_, err := h.svc.CreateDBInstance(t.Context(), input, testAccountID)
			require.NoError(t, err, "a CA-configured deployment binds enforcement without complaint")

			rec := h.record(t, testDBInstanceID)
			assert.Equal(t, tc.engine.DefaultParameterGroupName(), rec.DBParameterGroupName)
			assert.Equal(t, "1", resolvedParameter(t, rec.Bootstrap.ResolvedParameters, tc.parameter))

			params := describedParameters(t, h, tc.engine.DefaultParameterGroupName())
			require.Contains(t, params, tc.parameter)
			assert.Equal(t, "1", aws.StringValue(params[tc.parameter].ParameterValue))
			assert.Equal(t, ParameterSourceEngineDefault, aws.StringValue(params[tc.parameter].Source),
				"nothing was set: the default is what enforces")
		})
	}
}

// Enforcement the deployment cannot serve would launch an instance no client can
// reach, so the binding is refused where the customer can see it.
func TestCreateDBInstance_RefusesEnforcementWithNoClusterCA(t *testing.T) {
	h := newCreateHarness(t, testBaseDomain)
	withoutClusterCA(t, h.svc)

	_, err := h.svc.CreateDBInstance(t.Context(), validCreateInput(), testAccountID)
	requireNoTLSToEnforce(t, err, "rds.force_ssl", enginePostgres.DefaultParameterGroupName())
	assert.False(t, h.recordExists(t, testDBInstanceID), "a rejected create must reserve nothing")
}

// The customer keeps the AWS-visible way out, which is the reason enforcement is
// a modifiable parameter rather than a platform constant.
func TestCreateDBInstance_AcceptsEnforcementTurnedOffWithNoClusterCA(t *testing.T) {
	tests := []struct {
		engine    Engine
		group     string
		input     *rds.CreateDBParameterGroupInput
		parameter string
	}{
		{enginePostgres, testParameterGroup, parameterGroupInput(testParameterGroup), "rds.force_ssl"},
		{engineMariaDB, testMariaDBParameterGroup, mariadbParameterGroupInput(testMariaDBParameterGroup), "require_secure_transport"},
	}
	for _, tc := range tests {
		t.Run(tc.engine.Name, func(t *testing.T) {
			h := newCreateHarness(t, testBaseDomain)
			_, err := h.svc.CreateDBParameterGroup(t.Context(), tc.input, testAccountID)
			require.NoError(t, err)
			_, err = h.svc.ModifyDBParameterGroup(t.Context(),
				modifyParameters(tc.group, parameter(tc.parameter, "off", "")), testAccountID)
			require.NoError(t, err)
			withoutClusterCA(t, h.svc)

			input := validCreateInput()
			input.Engine = aws.String(tc.engine.Name)
			input.DBParameterGroupName = aws.String(tc.group)
			_, err = h.svc.CreateDBInstance(t.Context(), input, testAccountID)
			require.NoError(t, err)

			rec := h.record(t, testDBInstanceID)
			assert.Equal(t, "0", resolvedParameter(t, rec.Bootstrap.ResolvedParameters, tc.parameter),
				"the spelling the customer set reaches the guest canonicalised")
		})
	}
}

func TestModifyDBInstance_RefusesEnforcementWithNoClusterCA(t *testing.T) {
	h := newModifyHarness(t)
	_, err := h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput(testParameterGroup), testAccountID)
	require.NoError(t, err)
	seedInstance(t, h.svc, modifiableRecord())
	withoutClusterCA(t, h.svc)

	input := modifyInput()
	input.DBParameterGroupName = aws.String(testParameterGroup)
	input.ApplyImmediately = aws.Bool(true)
	_, err = h.svc.ModifyDBInstance(t.Context(), input, testAccountID)
	requireNoTLSToEnforce(t, err, "rds.force_ssl", testParameterGroup)
	assert.Equal(t, testDefaultGroup, h.record(t).DBParameterGroupName, "a rejected modify must change nothing")
	assert.Empty(t, h.agent.received(), "nothing may reach the engine")
}

// The deferred half of a modify re-resolves at apply time, so the gate holds
// there too rather than only at the request that recorded it.
func TestApplyPendingModifications_RefusesEnforcementWithNoClusterCA(t *testing.T) {
	h := newModifyHarness(t)
	h.agent.replyWith("")
	_, err := h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput(testParameterGroup), testAccountID)
	require.NoError(t, err)

	rec := modifyingRecord(&PendingModifiedValues{
		DBParameterGroupName: testParameterGroup,
		RequestedAt:          time.Now().UTC(),
	})
	seedInstance(t, h.svc, rec)
	withoutClusterCA(t, h.svc)

	err = h.svc.applyPendingModifications(t.Context(), h.kv(t), testAccountID, &rec)
	requireNoTLSToEnforce(t, err, "rds.force_ssl", testParameterGroup)
	assert.Empty(t, h.agent.received(), "nothing may reach the engine")
	assert.True(t, h.record(t).ParameterApplyFailed)
}

func TestRestoreDBInstanceFromDBSnapshot_RefusesEnforcementWithNoClusterCA(t *testing.T) {
	h := newSnapshotHarness(t, false)
	h.seedSnapshot(t)
	withoutClusterCA(t, h.svc)

	_, err := h.svc.RestoreDBInstanceFromDBSnapshot(t.Context(), restoreInput(), testAccountID)
	requireNoTLSToEnforce(t, err, "rds.force_ssl", enginePostgres.DefaultParameterGroupName())
	assert.False(t, h.instanceExists(t, testRestoredID), "a rejected restore must reserve nothing")
}
