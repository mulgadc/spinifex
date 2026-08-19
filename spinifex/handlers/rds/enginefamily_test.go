package handlers_rds

//test:in-package — builds Engine values from unexported fields (catalog,
// maxUsernameLen, validateDBName) and swaps the unexported engines and
// enginesByFamily registries, none of which are reachable from outside.

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A second engine that exists solely inside the test binary, so the cross-engine
// refusal is proven against the seam rather than against whichever engines the
// build happens to offer. Nothing about its catalog matters beyond being
// distinguishable from the shipped ones.
const (
	testEngineName  = "testdb"
	testEngineGroup = "tuned-testdb"
)

var engineUnderTest = Engine{
	Name:                     testEngineName,
	MajorVersion:             "1",
	DefaultPort:              3306,
	description:              "Test Engine",
	licenseModel:             "test-license",
	reservedUsernames:        []string{"root"},
	reservedUsernamePrefixes: []string{"testdb_"},
	maxUsernameLen:           80,
	validateDBName:           dbNameRule(64),
	catalog: map[string]ParameterSpec{
		"buffer_size": {
			Name: "buffer_size", DataType: ParamTypeInteger, ApplyType: ApplyTypeStatic,
			IsModifiable: true, Min: 1, Max: 1024, Default: "64", Unit: "MB",
			Description: "Buffer the server allocates at startup, in MB.",
		},
		"connection_limit": {
			Name: "connection_limit", DataType: ParamTypeInteger, ApplyType: ApplyTypeDynamic,
			IsModifiable: true, Min: 1, Max: 1000, Default: "100",
			Description: "Maximum concurrent connections to the server.",
		},
	},
	validateCombinations: func([]Parameter) error { return nil },
	crashRecoveryNote:    "It will recover when it is restored.",
	uncleanStopNote:      "It will recover on the next start.",
}

// Registers the second engine for one test. Both indexes are rebuilt, because a
// family-knowing caller reads the one keyed by family.
func registerTestEngine(t *testing.T) {
	t.Helper()
	require.NotContains(t, engines, engineUnderTest.Name)
	engines[engineUnderTest.Name] = engineUnderTest
	indexed, err := indexEnginesByFamily(engines)
	require.NoError(t, err)
	enginesByFamily = indexed
	t.Cleanup(func() {
		delete(engines, engineUnderTest.Name)
		indexed, err := indexEnginesByFamily(engines)
		require.NoError(t, err)
		enginesByFamily = indexed
	})
}

func TestValidateEngineRegistry(t *testing.T) {
	require.NoError(t, ValidateEngineRegistry())
}

func TestIndexEnginesByFamily_RejectsInvalidMetadata(t *testing.T) {
	noDBNameRule := engineUnderTest
	noDBNameRule.validateDBName = nil
	noCrashRecoveryNote := engineUnderTest
	noCrashRecoveryNote.crashRecoveryNote = ""
	noUncleanStopNote := engineUnderTest
	noUncleanStopNote.uncleanStopNote = ""
	invalidTLSParameter := engineUnderTest
	invalidTLSParameter.tlsEnforcementParameter = "not-a-boolean-parameter"

	tests := []struct {
		name     string
		registry map[string]Engine
		wantErr  string
	}{
		{
			name:     "missing DB name rule",
			registry: map[string]Engine{"test": noDBNameRule},
			wantErr:  "registers no DBName rule",
		},
		{
			name:     "missing crash recovery note",
			registry: map[string]Engine{"test": noCrashRecoveryNote},
			wantErr:  "registers no crash-recovery note",
		},
		{
			name:     "missing unclean stop note",
			registry: map[string]Engine{"test": noUncleanStopNote},
			wantErr:  "registers no unclean-stop note",
		},
		{
			name:     "invalid TLS enforcement parameter",
			registry: map[string]Engine{"test": invalidTLSParameter},
			wantErr:  "is not a boolean parameter it exposes",
		},
		{
			name: "duplicate parameter group family",
			registry: map[string]Engine{
				"first":  engineUnderTest,
				"second": engineUnderTest,
			},
			wantErr: "is claimed by two engines",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			indexed, err := indexEnginesByFamily(tt.registry)
			require.ErrorContains(t, err, tt.wantErr)
			assert.Nil(t, indexed)
		})
	}
}

func testEngineGroupInput(name string) *rds.CreateDBParameterGroupInput {
	return &rds.CreateDBParameterGroupInput{
		DBParameterGroupName:   aws.String(name),
		DBParameterGroupFamily: aws.String(engineUnderTest.ParameterGroupFamily()),
		Description:            aws.String("Tuned for the second engine"),
	}
}

func requireFamilyMismatch(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterCombination, awserrors.ValidErrorCodeFromError(err),
		"the code has to survive resolution or the client sees a 500")
}

// A group carries the settings of exactly one engine, so attaching one of
// another engine would boot the database on a configuration file it cannot
// parse.
func TestCreateDBInstance_RefusesAGroupOfAnotherEnginesFamily(t *testing.T) {
	registerTestEngine(t)
	h := newCreateHarness(t, testBaseDomain)
	_, err := h.svc.CreateDBParameterGroup(t.Context(), testEngineGroupInput(testEngineGroup), testAccountID)
	require.NoError(t, err)
	_, err = h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput(testParameterGroup), testAccountID)
	require.NoError(t, err)

	input := validCreateInput()
	input.DBParameterGroupName = aws.String(testEngineGroup)
	_, err = h.svc.CreateDBInstance(t.Context(), input, testAccountID)
	requireFamilyMismatch(t, err)
	assert.Contains(t, err.Error(), enginePostgres.ParameterGroupFamily(), "the error names the family the instance needs")
	assert.False(t, h.recordExists(t, testDBInstanceID), "a rejected create must reserve nothing")

	// The other direction: neither engine may borrow the other's group.
	input = validCreateInput()
	input.Engine = aws.String(testEngineName)
	input.DBParameterGroupName = aws.String(testParameterGroup)
	_, err = h.svc.CreateDBInstance(t.Context(), input, testAccountID)
	requireFamilyMismatch(t, err)
	assert.Contains(t, err.Error(), engineUnderTest.ParameterGroupFamily())
	assert.False(t, h.recordExists(t, testDBInstanceID))
}

func TestModifyDBInstance_RefusesAGroupOfAnotherEnginesFamily(t *testing.T) {
	registerTestEngine(t)
	h := newModifyHarness(t)
	_, err := h.svc.CreateDBParameterGroup(t.Context(), testEngineGroupInput(testEngineGroup), testAccountID)
	require.NoError(t, err)
	seedInstance(t, h.svc, modifiableRecord())

	input := modifyInput()
	input.DBParameterGroupName = aws.String(testEngineGroup)
	input.ApplyImmediately = aws.Bool(true)
	_, err = h.svc.ModifyDBInstance(t.Context(), input, testAccountID)
	requireFamilyMismatch(t, err)
	assert.Equal(t, testDefaultGroup, h.record(t).DBParameterGroupName, "a rejected modify must change nothing")
	assert.Empty(t, h.agent.received(), "nothing may reach the engine")
}

// The deferred half of a modify re-resolves at apply time, so the check has to
// hold there too rather than only at the request that recorded it.
func TestApplyPendingModifications_RefusesAGroupOfAnotherEnginesFamily(t *testing.T) {
	registerTestEngine(t)
	h := newModifyHarness(t)
	h.agent.replyWith("")
	_, err := h.svc.CreateDBParameterGroup(t.Context(), testEngineGroupInput(testEngineGroup), testAccountID)
	require.NoError(t, err)

	rec := modifyingRecord(&PendingModifiedValues{
		DBParameterGroupName: testEngineGroup,
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
	registerTestEngine(t)
	h := newSnapshotHarness(t, false)
	h.seedSnapshot(t)
	_, err := h.svc.CreateDBParameterGroup(t.Context(), testEngineGroupInput(testEngineGroup), testAccountID)
	require.NoError(t, err)

	input := restoreInput()
	input.DBParameterGroupName = aws.String(testEngineGroup)
	_, err = h.svc.RestoreDBInstanceFromDBSnapshot(t.Context(), input, testAccountID)
	requireFamilyMismatch(t, err)
	assert.False(t, h.instanceExists(t, testRestoredID), "a rejected restore must reserve nothing")
}

// Values are validated against the engine the target group's family names. The
// alternative stores one engine's setting into another's group and defers the
// failure to whichever instance next attaches it.
func TestModifyDBParameterGroup_ValidatesAgainstTheGroupsOwnEngine(t *testing.T) {
	registerTestEngine(t)
	h := newCreateHarness(t, testBaseDomain)
	_, err := h.svc.CreateDBParameterGroup(t.Context(), testEngineGroupInput(testEngineGroup), testAccountID)
	require.NoError(t, err)
	_, err = h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput(testParameterGroup), testAccountID)
	require.NoError(t, err)

	_, err = h.svc.ModifyDBParameterGroup(t.Context(),
		modifyParameters(testEngineGroup, parameter("work_mem", "16384", "")), testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err))

	_, err = h.svc.ModifyDBParameterGroup(t.Context(),
		modifyParameters(testParameterGroup, parameter("connection_limit", "50", "")), testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err))

	_, err = h.svc.ModifyDBParameterGroup(t.Context(),
		modifyParameters(testEngineGroup, parameter("connection_limit", "50", "")), testAccountID)
	require.NoError(t, err, "a group takes its own engine's parameters")
	assert.Equal(t, "50", aws.StringValue(describedParameters(t, h, testEngineGroup)["connection_limit"].ParameterValue))
}

func TestDescribeDBParameters_ListsTheGroupsOwnEngineCatalog(t *testing.T) {
	registerTestEngine(t)
	h := newCreateHarness(t, testBaseDomain)
	_, err := h.svc.CreateDBParameterGroup(t.Context(), testEngineGroupInput(testEngineGroup), testAccountID)
	require.NoError(t, err)

	params := describedParameters(t, h, testEngineGroup)
	require.Len(t, params, len(engineUnderTest.CatalogParameterNames()))
	assert.NotContains(t, params, "work_mem", "a group must not report another engine's settings")
	assert.Equal(t, "100", aws.StringValue(params["connection_limit"].ParameterValue))
	assert.Equal(t, ParameterSourceEngineDefault, aws.StringValue(params["connection_limit"].Source))
}

// The rejection has to name what the client can use instead, which stops being
// one family as soon as a second engine registers.
func TestCreateDBParameterGroup_RejectionNamesEverySupportedFamily(t *testing.T) {
	registerTestEngine(t)
	h := newCreateHarness(t, testBaseDomain)

	input := parameterGroupInput(testParameterGroup)
	input.DBParameterGroupFamily = aws.String("mysql8")
	_, err := h.svc.CreateDBParameterGroup(t.Context(), input, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err))
	for _, family := range SupportedParameterGroupFamilies() {
		assert.Contains(t, err.Error(), family)
	}
	assert.Equal(t, []string{
		engineMariaDB.ParameterGroupFamily(),
		enginePostgres.ParameterGroupFamily(),
		engineUnderTest.ParameterGroupFamily(),
	}, SupportedParameterGroupFamilies())
}
