package engine

//test:in-package — reaches unexported fields (catalog, maxUsernameLen,
// validateDBName, crashRecoveryNote, uncleanStopNote, tlsEnforcementParameter)
// and the unexported engines/enginesByFamily registries, none of which this
// package exports.

import (
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLookupEngine(t *testing.T) {
	t.Parallel()
	_, err := LookupEngine("PostgreSQL")
	require.Error(t, err, "the AWS engine identifier is 'postgres', not the product name")

	engine, err := LookupEngine("  POSTGRES ")
	require.NoError(t, err, "the engine name is matched case-insensitively and trimmed")
	assert.Equal(t, "postgres", engine.Name)
	assert.Equal(t, int64(5432), engine.DefaultPort)
	assert.Equal(t, "default.postgres18", engine.DefaultParameterGroupName())

	_, err = LookupEngine("mysql")
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
	assert.Equal(t, []string{"mariadb", "postgres"}, SupportedEngines())
}

// A version other than the pinned one would be served by an image that is
// not the one asked for, so it is rejected rather than quietly substituted.
func TestEngineValidateVersion(t *testing.T) {
	t.Parallel()
	engine, err := LookupEngine("postgres")
	require.NoError(t, err)

	for _, version := range []string{"", "18"} {
		assert.NoError(t, engine.ValidateVersion(version), "version %q", version)
	}
	for _, version := range []string{"17", "16.2", "18.1", "18.4", " 18 ", "19", "latest"} {
		assert.Error(t, engine.ValidateVersion(version), "version %q", version)
	}

	// The pin, not the request, is what the record and the AMI lookup carry.
	assert.Equal(t, "18", engine.EngineVersion())
}

// The AWS engine identifier is "postgres", not the marketing name a customer
// might reasonably try first.
func TestLookupEngine_RejectsProductNameWithExactCode(t *testing.T) {
	t.Parallel()
	_, err := LookupEngine("PostgreSQL")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err))
}

func TestEngineValidateVersion_UnsupportedVersionCarriesExactCode(t *testing.T) {
	t.Parallel()
	engine, err := LookupEngine("postgres")
	require.NoError(t, err)

	err = engine.ValidateVersion("19")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err))
}

func TestEngineValidateMasterUsername(t *testing.T) {
	engine, err := LookupEngine("postgres")
	require.NoError(t, err)

	for _, username := range []string{"appuser", "app_user1", "Orders"} {
		assert.NoError(t, engine.ValidateMasterUsername(username), "username %q", username)
	}

	cases := map[string]string{
		"empty":          "",
		"leading digit":  "1user",
		"hyphen":         "app-user",
		"too long":       strings.Repeat("a", enginePostgres.maxUsernameLen+1),
		"reserved":       "rdsadmin",
		"reserved cased": "RDSAdmin",
		// The cluster superuser and the group role the master's administrative
		// privileges come through. Both exist by the time the master is created,
		// so a collision would surface as a failed bootstrap inside the guest.
		"cluster superuser": "postgres",
		"group role":        "rds_superuser",
		// postgres reserves the pg_ prefix for its own internal roles, so a master
		// role taking one would collide inside the engine, not at the API.
		"reserved prefix": "pg_backup",
	}
	for name, username := range cases {
		t.Run(name, func(t *testing.T) {
			err := engine.ValidateMasterUsername(username)
			require.Error(t, err)
			assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
		})
	}
}

// The name is interpolated into a CREATE DATABASE in the guest, so the rule is
// narrower than the engine's own: it is what makes the interpolation safe.
func TestEngineValidateDBName(t *testing.T) {
	engine, err := LookupEngine("postgres")
	require.NoError(t, err)

	// Empty is not a rejection: AWS creates no initial database when none is named.
	for _, name := range []string{"", "orders", "order_db1", "Orders"} {
		assert.NoError(t, engine.ValidateDBName(name), "DBName %q", name)
	}

	cases := map[string]string{
		"hyphen":        "my-db",
		"leading digit": "1db",
		"space":         "my db",
		"dot":           "my.db",
		// MariaDB maps a database name onto a directory name, so these are
		// structurally dangerous there rather than merely awkward.
		"slash":          "my/db",
		"backslash":      `my\db`,
		"trailing space": "orders ",
		"quote":          "orders'",
		"too long":       strings.Repeat("a", 64),
	}
	for name, dbName := range cases {
		t.Run(name, func(t *testing.T) {
			err := engine.ValidateDBName(dbName)
			require.Error(t, err)
			assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
		})
	}
}

// EngineForFamily and normaliseFamily fold case and trim surrounding
// whitespace before the registry lookup, exactly as LookupEngine does for an
// engine name.
func TestEngineForFamily_NormalisesCaseAndWhitespace(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "postgres18", normaliseFamily(" POSTGRES18 "))
	assert.Equal(t, "mariadb11.8", normaliseFamily("MariaDB11.8"))

	byCase, err := EngineForFamily(" POSTGRES18 ")
	require.NoError(t, err)
	assert.Equal(t, enginePostgres.Name, byCase.Name)

	byMixedCase, err := EngineForFamily("MariaDB11.8")
	require.NoError(t, err)
	assert.Equal(t, engineMariaDB.Name, byMixedCase.Name)
}

func TestEngineForFamily_RejectsUnknownFamily(t *testing.T) {
	t.Parallel()
	_, err := EngineForFamily("mysql8")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err),
		"the code has to survive resolution or the client sees a 500")
}

func TestValidateEngineRegistry(t *testing.T) {
	require.NoError(t, ValidateEngineRegistry())
}

// A second engine that exists solely inside the test binary, so the cross-engine
// registry validation is proven against the seam rather than against whichever
// engines the build happens to offer.
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
	validateCombinations: func([]Setting) error { return nil },
	crashRecoveryNote:    "It will recover when it is restored.",
	uncleanStopNote:      "It will recover on the next start.",
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

// ResolveEffectiveParameters checks validateCombinations before anything else,
// so an engine that registers none fails with ServerInternal rather than
// resolving a parameter set it cannot then validate.
func TestResolveEffectiveParameters_NoCombinationCheckIsServerInternal(t *testing.T) {
	t.Parallel()
	bare := Engine{Name: "bare-engine"}
	_, err := bare.ResolveEffectiveParameters(testSizing, testSizing.SmallestInstanceClass(), nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, awserrors.ValidErrorCodeFromError(err))
	assert.Contains(t, err.Error(), "registers no parameter combination checks")
}

// A combination check that reads a name absent from its own catalog is a
// catalog bug, not anything a customer did, so the resolver's internal reader
// reports ServerInternal naming the missing key.
func TestResolveEffectiveParameters_CombinationCheckMissingKeyIsServerInternal(t *testing.T) {
	t.Parallel()
	broken := Engine{
		Name: "broken-combination-checks",
		catalog: map[string]ParameterSpec{
			"foo": {Name: "foo", DataType: ParamTypeInteger, ApplyType: ApplyTypeStatic,
				IsModifiable: true, Min: 0, Max: 10, Default: "1"},
		},
		validateCombinations: func(settings []Setting) error {
			values := resolvedValues(settings)
			_, err := resolvedInteger(values, "bar")
			return err
		},
	}
	_, err := broken.ResolveEffectiveParameters(testSizing, testSizing.SmallestInstanceClass(), nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, awserrors.ValidErrorCodeFromError(err))
	assert.Contains(t, err.Error(), "resolved parameter set is missing bar")
}
