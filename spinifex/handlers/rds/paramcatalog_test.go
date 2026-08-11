package handlers_rds

import (
	"strconv"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every engine's catalog, whether or not it is registered in engines: a catalog
// has to satisfy these invariants before the engine is offered, not once a
// customer can reach it.
var catalogEngines = []Engine{enginePostgres, engineMariaDB}

// The failure this guards is the one the phase opens by warning about, arriving
// through the default path rather than a customer typo: a catalog change that
// makes the smallest class unbootable has to fail here rather than in a create.
func TestParameterCatalog_DefaultsSatisfyTheirOwnConstraintsAtEveryClass(t *testing.T) {
	for _, engine := range catalogEngines {
		for _, class := range SupportedInstanceClasses() {
			t.Run(engine.Name+"/"+class, func(t *testing.T) {
				memoryMiB, err := classMemoryMiB(class)
				require.NoError(t, err)

				for _, name := range engine.CatalogParameterNames() {
					spec, ok := engine.LookupParameter(name)
					require.True(t, ok)

					value := spec.DefaultAt(memoryMiB)
					assert.NotEmpty(t, value, "%s has an empty default at %s", name, class)
					// Validated by the same function a customer's value goes through, so
					// a default cannot be something the API would reject.
					_, err := engine.validateParameterValue(name, value)
					assert.NoError(t, err, "the default %q of %s is not a value %s accepts", value, name, class)
				}

				// The defaults also have to satisfy the engine's combination checks,
				// which no single parameter's own validation can see.
				_, err = engine.ResolveEffectiveParameters(class, nil)
				assert.NoError(t, err, "the untouched defaults do not resolve at %s", class)
			})
		}
	}
}

// Every entry has to be a shape the resolver and the describe can both render,
// or a client reads a parameter with no type and no apply semantics.
func TestParameterCatalog_EntriesAreWellFormed(t *testing.T) {
	for _, engine := range catalogEngines {
		t.Run(engine.Name, func(t *testing.T) {
			for _, name := range engine.CatalogParameterNames() {
				spec, _ := engine.LookupParameter(name)

				assert.Equal(t, name, spec.Name, "the catalog key and the spec name disagree")
				assert.Contains(t, []string{ApplyTypeStatic, ApplyTypeDynamic}, spec.ApplyType, "%s", name)
				assert.Contains(t, []string{paramTypeInteger, paramTypeReal, paramTypeBoolean, paramTypeString, paramTypeEnum},
					spec.DataType, "%s", name)
				if spec.DataType == paramTypeEnum {
					assert.NotEmpty(t, spec.Enum, "%s is an enum with no allowed values", name)
				}
				if spec.DataType == paramTypeInteger || spec.DataType == paramTypeReal {
					assert.Less(t, spec.Min, spec.Max, "%s has an empty range", name)
				}
			}
		})
	}
}

// The size-derived defaults have to actually move with the class, or the
// formulas are decoration and one end of the supported range is mis-tuned.
func TestParameterCatalog_SizeDerivedDefaultsScaleWithClassMemory(t *testing.T) {
	smallest, err := classMemoryMiB("db.t3.micro")
	require.NoError(t, err)
	largest, err := classMemoryMiB("db.m5.xlarge")
	require.NoError(t, err)
	require.Less(t, smallest, largest)

	for _, name := range []string{"shared_buffers", "effective_cache_size", "max_connections", "maintenance_work_mem"} {
		spec, ok := enginePostgres.LookupParameter(name)
		require.True(t, ok, "%s is expected to carry a computed default", name)
		require.NotNil(t, spec.DefaultFor, "%s should be size-derived", name)

		small, err := strconv.ParseInt(spec.DefaultAt(smallest), 10, 64)
		require.NoError(t, err)
		large, err := strconv.ParseInt(spec.DefaultAt(largest), 10, 64)
		require.NoError(t, err)
		assert.Less(t, small, large, "%s does not grow with the class", name)
	}
}

// shared_buffers is a quarter of class memory on real RDS, and it is the setting
// whose being wrong at the small end stops the engine from starting.
func TestParameterCatalog_SharedBuffersIsAQuarterOfClassMemory(t *testing.T) {
	memoryMiB, err := classMemoryMiB("db.m5.large")
	require.NoError(t, err)

	blocks, err := strconv.ParseInt(sharedBuffersFor(memoryMiB), 10, 64)
	require.NoError(t, err)
	assert.Equal(t, memoryMiB/4, blocks*8/1024, "shared_buffers is not a quarter of the class's memory")
}

func TestValidateParameterValue_RejectsBadInput(t *testing.T) {
	cases := []struct {
		name, param, value, want string
	}{
		{"UnknownName", "not_a_setting", "1", "is not a parameter this engine exposes"},
		{"NotAnInteger", "max_connections", "many", "takes an integer"},
		{"BelowRange", "max_connections", "1", "outside its allowed range"},
		{"AboveRange", "max_connections", "500000", "outside its allowed range"},
		{"WALBelowTwoSegments", "min_wal_size", "16", "outside its allowed range"},
		{"NotAReal", "checkpoint_completion_target", "high", "takes a number"},
		{"RealOutOfRange", "checkpoint_completion_target", "2", "outside its allowed range"},
		{"NotABoolean", "autovacuum", "maybe", "takes a boolean"},
		{"NotInEnum", "log_statement", "everything", "does not accept"},
		{"Empty", "work_mem", "", "empty value"},
		{"StringControlCharacter", "timezone", "UTC\nwork_mem", "control characters"},
		{"StringTrailingBackslash", "datestyle", `ISO\`, "end with a backslash"},
		{"NormalizedTrailingBackslash", "datestyle", "ISO\\   ", "end with a backslash"},
		{"StringTooLong", "datestyle", strings.Repeat("x", maxStringParameterBytes+1), "maximum length"},
		{"UnknownTimezone", "timezone", "Not/A_Real_Zone", "does not accept"},
		{"GoLocalTimezone", "timezone", "Local", "does not accept"},
		{"CaseInsensitiveLocalTimezone", "timezone", "lOcAl", "does not accept"},
		{"UnknownDateStyle", "datestyle", "garbage", "does not accept"},
		{"DuplicateDateStyles", "datestyle", "ISO, SQL", "does not accept"},
		{"DuplicateDateOrders", "datestyle", "MDY, DMY", "does not accept"},
		{"EmptyDateStylePart", "datestyle", "ISO,", "does not accept"},
		{"TooManyDateStyleParts", "datestyle", "ISO, MDY, DMY", "does not accept"},
		// AWS accepts these; passing one through would be a startup failure rather
		// than an API error.
		{"Formula", "shared_buffers", "{DBInstanceClassMemory/32768}", "is a formula"},
		{"Reference", "max_connections", "DBInstanceClassMemory", "is a formula"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := enginePostgres.validateParameterValue(tc.param, tc.value)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err),
				"the code has to survive resolution or the client sees a 500")
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestValidateParameterValue_AcceptsPostgresStringValues(t *testing.T) {
	cases := []struct {
		name, param, value string
	}{
		{"UTC", "timezone", "UTC"},
		{"GMT", "timezone", "GMT"},
		{"IANAZone", "timezone", "Australia/Sydney"},
		{"DateStyleOnly", "datestyle", "ISO"},
		{"DateOrderOnly", "datestyle", "YMD"},
		{"StyleAndOrder", "datestyle", "ISO, MDY"},
		{"CaseInsensitive", "datestyle", "gErMaN, dMy"},
		{"NormalizedWhitespace", "datestyle", "  SQL, DMY  \n"},
		{"NormalizedLength", "datestyle", strings.Repeat(" ", maxStringParameterBytes+1) + "ISO"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := enginePostgres.validateParameterValue(tc.param, tc.value)
			assert.NoError(t, err)
		})
	}
}

func TestValidateParameterValue_AcceptsEveryPostgresSpellingOfABoolean(t *testing.T) {
	for _, value := range []string{"on", "OFF", "true", "False", "yes", "no", "1", "0"} {
		_, err := enginePostgres.validateParameterValue("autovacuum", value)
		assert.NoError(t, err, "autovacuum rejected %q, which the engine accepts", value)
	}
}

// A group's overrides are laid over the whole catalog, not merged into a subset:
// every parameter is present, and one the group does not set carries its default.
func TestResolveEffectiveParameters_OverlaysOverridesOnDefaults(t *testing.T) {
	resolved, err := enginePostgres.ResolveEffectiveParameters("db.t3.medium", map[string]string{"work_mem": "16384"})
	require.NoError(t, err)
	require.Len(t, resolved, len(enginePostgres.CatalogParameterNames()))

	values := map[string]string{}
	for _, param := range resolved {
		values[param.Name] = param.Value
	}
	assert.Equal(t, "16384", values["work_mem"], "the override did not win")

	memoryMiB, err := classMemoryMiB("db.t3.medium")
	require.NoError(t, err)
	assert.Equal(t, sharedBuffersFor(memoryMiB), values["shared_buffers"],
		"a parameter the group does not set should carry its computed default")
}

// Sorted output is what lets a re-resolve that changed nothing produce a
// byte-identical include, so an apply does not restart an engine for no reason.
func TestResolveEffectiveParameters_IsSortedAndCarriesNoFormula(t *testing.T) {
	resolved, err := enginePostgres.ResolveEffectiveParameters("db.m5.xlarge", nil)
	require.NoError(t, err)

	for i := 1; i < len(resolved); i++ {
		assert.Less(t, resolved[i-1].Name, resolved[i].Name, "the resolved set is not sorted by name")
	}
	for _, param := range resolved {
		assert.NotContains(t, param.Value, "{", "%s reached the agent as a formula", param.Name)
		assert.NotContains(t, param.Value, "DBInstanceClass", "%s reached the agent as a reference", param.Name)
	}
}

// A stored value the catalog would now reject must not keep being handed to the
// engine, so the resolve re-validates rather than trusting what was written.
func TestResolveEffectiveParameters_RevalidatesStoredOverrides(t *testing.T) {
	_, err := enginePostgres.ResolveEffectiveParameters("db.t3.medium", map[string]string{"max_connections": "999999"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max_connections")
}

func TestResolveEffectiveParameters_BoundsSizeDerivedOverridesToTheClass(t *testing.T) {
	tests := []struct {
		name, value string
	}{
		{name: "shared_buffers", value: "131072"},
		{name: "effective_cache_size", value: "262144"},
		{name: "max_connections", value: "1000"},
		{name: "maintenance_work_mem", value: "1048576"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := enginePostgres.ResolveEffectiveParameters("db.t3.micro", map[string]string{tt.name: tt.value})
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err))
			assert.Contains(t, err.Error(), "db.t3.micro")
			assert.Contains(t, err.Error(), tt.value)
			assert.Contains(t, err.Error(), "ceiling")

			_, err = enginePostgres.ResolveEffectiveParameters("db.m5.xlarge", map[string]string{tt.name: tt.value})
			assert.NoError(t, err, "the same literal should fit the larger class")
		})
	}
}

func TestResolveEffectiveParameters_RejectsFatalCombinations(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]string
		want      string
	}{
		{
			name:      "minimal WAL with senders",
			overrides: map[string]string{"wal_level": "minimal"},
			want:      "max_wal_senders and max_replication_slots",
		},
		{
			name: "reserved connections consume every slot",
			overrides: map[string]string{
				"max_connections": "20", "superuser_reserved_connections": "20",
			},
			want: "must be less than max_connections",
		},
		{
			name:      "too many server processes",
			overrides: map[string]string{"max_worker_processes": "262143"},
			want:      "too many server processes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := enginePostgres.ResolveEffectiveParameters("db.m5.xlarge", tt.overrides)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err))
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestResolveEffectiveParameters_AcceptsMinimalWALWithoutReplication(t *testing.T) {
	_, err := enginePostgres.ResolveEffectiveParameters("db.t3.micro", map[string]string{
		"wal_level": "minimal", "max_wal_senders": "0", "max_replication_slots": "0",
	})
	require.NoError(t, err)
}

func TestParameterCatalog_Postgres18ApplyTypesAndBuiltCompressionMethods(t *testing.T) {
	autovacuumWorkers, _ := enginePostgres.LookupParameter("autovacuum_max_workers")
	assert.Equal(t, ApplyTypeDynamic, autovacuumWorkers.ApplyType)

	walCompression, _ := enginePostgres.LookupParameter("wal_compression")
	assert.ElementsMatch(t, []string{"off", "pglz", "lz4", "zstd", "on"}, walCompression.Enum)
}

func TestResolveEffectiveParameters_RejectsAnUnknownClass(t *testing.T) {
	_, err := enginePostgres.ResolveEffectiveParameters("db.x99.mega", nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err),
		"the code has to survive resolution or the client sees a 500")
}

// AllowedValues is what tells a customer what an integer means, so a unitful
// parameter has to report its unit rather than a bare pair of numbers.
func TestParameterSpec_AllowedValuesDescribesTheConstraint(t *testing.T) {
	shared, _ := enginePostgres.LookupParameter("shared_buffers")
	assert.Equal(t, "16-4194304 (8kB)", shared.AllowedValues())

	logStatement, _ := enginePostgres.LookupParameter("log_statement")
	assert.Equal(t, "none,ddl,mod,all", logStatement.AllowedValues())

	timezone, _ := enginePostgres.LookupParameter("timezone")
	assert.Empty(t, timezone.AllowedValues(), "a free-form string has no constraint to report")

	target, _ := enginePostgres.LookupParameter("checkpoint_completion_target")
	assert.True(t, strings.HasPrefix(target.AllowedValues(), "0-1"), "got %q", target.AllowedValues())
}
