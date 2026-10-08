package handlers_rds

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	rdsengine "github.com/mulgadc/spinifex/spinifex/domains/rds/engine"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func requireFamilyMismatch(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterCombination, awserrors.ValidErrorCodeFromError(err),
		"the code has to survive resolution or the client sees a 500")
}

// A group carries the settings of exactly one engine, so attaching one of
// another engine would boot the database on a configuration file it cannot
// parse. Proven with the two shipped engines rather than a fake third one.
func TestCreateDBInstance_RefusesAGroupOfAnotherEnginesFamily(t *testing.T) {
	h := newCreateHarness(t, testBaseDomain)
	_, err := h.svc.CreateDBParameterGroup(t.Context(), mariadbParameterGroupInput(testMariaDBParameterGroup), testAccountID)
	require.NoError(t, err)
	_, err = h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput(testParameterGroup), testAccountID)
	require.NoError(t, err)

	input := validCreateInput()
	input.DBParameterGroupName = aws.String(testMariaDBParameterGroup)
	_, err = h.svc.CreateDBInstance(t.Context(), input, testAccountID)
	requireFamilyMismatch(t, err)
	assert.Contains(t, err.Error(), enginePostgres.ParameterGroupFamily(), "the error names the family the instance needs")
	assert.False(t, h.recordExists(t, testDBInstanceID), "a rejected create must reserve nothing")

	// The other direction: neither engine may borrow the other's group.
	input = validCreateInput()
	input.Engine = aws.String("mariadb")
	input.DBParameterGroupName = aws.String(testParameterGroup)
	_, err = h.svc.CreateDBInstance(t.Context(), input, testAccountID)
	requireFamilyMismatch(t, err)
	assert.Contains(t, err.Error(), engineMariaDB.ParameterGroupFamily())
	assert.False(t, h.recordExists(t, testDBInstanceID))
}

func TestModifyDBInstance_RefusesAGroupOfAnotherEnginesFamily(t *testing.T) {
	h := newModifyHarness(t)
	_, err := h.svc.CreateDBParameterGroup(t.Context(), mariadbParameterGroupInput(testMariaDBParameterGroup), testAccountID)
	require.NoError(t, err)
	seedInstance(t, h.svc, modifiableRecord())

	input := modifyInput()
	input.DBParameterGroupName = aws.String(testMariaDBParameterGroup)
	input.ApplyImmediately = aws.Bool(true)
	_, err = h.svc.ModifyDBInstance(t.Context(), input, testAccountID)
	requireFamilyMismatch(t, err)
	assert.Equal(t, testDefaultGroup, h.record(t).DBParameterGroupName, "a rejected modify must change nothing")
	assert.Empty(t, h.agent.received(), "nothing may reach the engine")
}

// The deferred half of a modify re-resolves at apply time, so the check has to
// hold there too rather than only at the request that recorded it.
func TestApplyPendingModifications_RefusesAGroupOfAnotherEnginesFamily(t *testing.T) {
	h := newModifyHarness(t)
	h.agent.replyWith("")
	_, err := h.svc.CreateDBParameterGroup(t.Context(), mariadbParameterGroupInput(testMariaDBParameterGroup), testAccountID)
	require.NoError(t, err)

	rec := modifyingRecord(&PendingModifiedValues{
		DBParameterGroupName: testMariaDBParameterGroup,
		RequestedAt:          time.Now().UTC(),
	})
	seedInstance(t, h.svc, rec)

	err = h.svc.applyPendingModifications(t.Context(), h.kv(t), testAccountID, &rec)
	requireFamilyMismatch(t, err)
	assert.Empty(t, h.agent.received(), "nothing may reach the engine")
	// A set that never resolves is as unapplied as one the guest refused, and
	// reports the same way.
	assert.True(t, h.record(t).ParameterApplyFailed)
}

// A restore takes its engine from the snapshot rather than the request, so the
// group named alongside it is checked against what the restored data is.
func TestRestoreDBInstanceFromDBSnapshot_RefusesAGroupOfAnotherEnginesFamily(t *testing.T) {
	h := newSnapshotHarness(t, false)
	h.seedSnapshot(t)
	_, err := h.svc.CreateDBParameterGroup(t.Context(), mariadbParameterGroupInput(testMariaDBParameterGroup), testAccountID)
	require.NoError(t, err)

	input := restoreInput()
	input.DBParameterGroupName = aws.String(testMariaDBParameterGroup)
	_, err = h.svc.RestoreDBInstanceFromDBSnapshot(t.Context(), input, testAccountID)
	requireFamilyMismatch(t, err)
	assert.False(t, h.instanceExists(t, testRestoredID), "a rejected restore must reserve nothing")
}

// Values are validated against the engine the target group's family names. The
// alternative stores one engine's setting into another's group and defers the
// failure to whichever instance next attaches it.
func TestModifyDBParameterGroup_ValidatesAgainstTheGroupsOwnEngine(t *testing.T) {
	h := newCreateHarness(t, testBaseDomain)
	_, err := h.svc.CreateDBParameterGroup(t.Context(), mariadbParameterGroupInput(testMariaDBParameterGroup), testAccountID)
	require.NoError(t, err)
	_, err = h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput(testParameterGroup), testAccountID)
	require.NoError(t, err)

	// work_mem is PostgreSQL-only: a MariaDB group does not carry it.
	_, err = h.svc.ModifyDBParameterGroup(t.Context(),
		modifyParameters(testMariaDBParameterGroup, parameter("work_mem", "16384", "")), testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err))

	// max_connect_errors is MariaDB-only: a PostgreSQL group does not carry it.
	_, err = h.svc.ModifyDBParameterGroup(t.Context(),
		modifyParameters(testParameterGroup, parameter("max_connect_errors", "50", "")), testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err))

	_, err = h.svc.ModifyDBParameterGroup(t.Context(),
		modifyParameters(testMariaDBParameterGroup, parameter("max_connect_errors", "50", "")), testAccountID)
	require.NoError(t, err, "a group takes its own engine's parameters")
	assert.Equal(t, "50", aws.StringValue(describedParameters(t, h, testMariaDBParameterGroup)["max_connect_errors"].ParameterValue))
}

func TestDescribeDBParameters_ListsTheGroupsOwnEngineCatalog(t *testing.T) {
	h := newCreateHarness(t, testBaseDomain)
	_, err := h.svc.CreateDBParameterGroup(t.Context(), mariadbParameterGroupInput(testMariaDBParameterGroup), testAccountID)
	require.NoError(t, err)

	params := describedParameters(t, h, testMariaDBParameterGroup)
	require.Len(t, params, len(engineMariaDB.CatalogParameterNames()))
	assert.NotContains(t, params, "work_mem", "a group must not report another engine's settings")
	assert.Equal(t, "100", aws.StringValue(params["max_connect_errors"].ParameterValue))
	assert.Equal(t, ParameterSourceEngineDefault, aws.StringValue(params["max_connect_errors"].Source))
}

// The rejection has to name what the client can use instead. Exercised again
// here (alongside TestCreateDBParameterGroup_RejectsAnUnofferedFamily) because
// this assertion additionally pins the exact, sorted family list.
func TestCreateDBParameterGroup_RejectionNamesEverySupportedFamily(t *testing.T) {
	h := newCreateHarness(t, testBaseDomain)

	input := parameterGroupInput(testParameterGroup)
	input.DBParameterGroupFamily = aws.String("mysql8")
	_, err := h.svc.CreateDBParameterGroup(t.Context(), input, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err))
	for _, family := range rdsengine.SupportedParameterGroupFamilies() {
		assert.Contains(t, err.Error(), family)
	}
	assert.Equal(t, []string{
		engineMariaDB.ParameterGroupFamily(),
		enginePostgres.ParameterGroupFamily(),
	}, rdsengine.SupportedParameterGroupFamilies())
}
