package engine

import (
	"strconv"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every engine's catalog, whether or not it is registered in engines: a catalog
// has to satisfy these invariants before the engine is offered, not once a
// customer can reach it.
var catalogEngines = []Engine{enginePostgres, engineMariaDB}

func TestBuildParameterCatalog_ReturnsValidationErrors(t *testing.T) {
	computedDefault := func(int64) string { return "1" }
	classCeiling := func(int64) int64 { return 2 }
	tests := []struct {
		name    string
		specs   []ParameterSpec
		wantErr string
	}{
		{
			name:  "valid entry",
			specs: []ParameterSpec{{Name: "valid", Default: "1"}},
		},
		{
			name:    "unnamed entry",
			specs:   []ParameterSpec{{Default: "1"}},
			wantErr: "holds an unnamed entry",
		},
		{
			name:    "missing default",
			specs:   []ParameterSpec{{Name: "missing"}},
			wantErr: "has no default",
		},
		{
			name: "literal and computed defaults",
			specs: []ParameterSpec{{
				Name: "both", Default: "1", DefaultFor: computedDefault, MaxFor: classCeiling,
			}},
			wantErr: "has both a literal and a computed default",
		},
		{
			name: "computed default without ceiling",
			specs: []ParameterSpec{{
				Name: "unpaired", DefaultFor: computedDefault,
			}},
			wantErr: "must pair its computed default and class ceiling",
		},
		{
			name: "duplicate entry",
			specs: []ParameterSpec{
				{Name: "duplicate", Default: "1"},
				{Name: "duplicate", Default: "2"},
			},
			wantErr: "is duplicated",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			catalog, err := buildParameterCatalog(tt.specs...)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				assert.Nil(t, catalog)
				return
			}
			require.NoError(t, err)
			assert.Len(t, catalog, len(tt.specs))
		})
	}
}

// The failure this guards is the one the phase opens by warning about, arriving
// through the default path rather than a customer typo: a catalog change that
// makes the smallest class unbootable has to fail here rather than in a create.
func TestParameterCatalog_DefaultsSatisfyTheirOwnConstraintsAtEveryClass(t *testing.T) {
	for _, engine := range catalogEngines {
		for _, class := range SupportedInstanceClasses() {
			t.Run(engine.Name+"/"+class, func(t *testing.T) {
				memoryMiB, err := testSizing.ClassMemoryMiB(class)
				require.NoError(t, err)

				for _, name := range engine.CatalogParameterNames() {
					spec, ok := engine.LookupParameter(name)
					require.True(t, ok)

					value := spec.DefaultAt(memoryMiB)
					assert.NotEmpty(t, value, "%s has an empty default at %s", name, class)
					// The same checks a customer's value goes through, minus the
					// modifiability gate a pinned entry would stop at: its default still
					// has to be a literal the engine parses.
					assert.NoError(t, spec.validateValue(value),
						"the default %q of %s is not a value %s accepts", value, name, class)
				}

				// The defaults also have to satisfy the engine's combination checks,
				// which no single parameter's own validation can see.
				_, err = engine.ResolveEffectiveParameters(testSizing, class, nil)
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
				assert.Contains(t, []string{ParamTypeInteger, ParamTypeReal, ParamTypeBoolean, ParamTypeString, ParamTypeEnum},
					spec.DataType, "%s", name)
				if spec.DataType == ParamTypeEnum {
					assert.NotEmpty(t, spec.Enum, "%s is an enum with no allowed values", name)
				}
				if spec.DataType == ParamTypeInteger || spec.DataType == ParamTypeReal {
					assert.Less(t, spec.Min, spec.Max, "%s has an empty range", name)
				}
			}
		})
	}
}

// The size-derived defaults have to actually move with the class, or the
// formulas are decoration and one end of the supported range is mis-tuned.
func TestParameterCatalog_SizeDerivedDefaultsScaleWithClassMemory(t *testing.T) {
	t.Parallel()
	smallest, err := testSizing.ClassMemoryMiB("db.t3.micro")
	require.NoError(t, err)
	largest, err := testSizing.ClassMemoryMiB("db.m5.xlarge")
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
	t.Parallel()
	memoryMiB, err := testSizing.ClassMemoryMiB("db.m5.large")
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
			_, err := enginePostgres.ValidateParameterValue(tc.param, tc.value)
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
			_, err := enginePostgres.ValidateParameterValue(tc.param, tc.value)
			assert.NoError(t, err)
		})
	}
}

func TestValidateParameterValue_AcceptsEveryPostgresSpellingOfABoolean(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"on", "OFF", "true", "False", "yes", "no", "1", "0"} {
		_, err := enginePostgres.ValidateParameterValue("autovacuum", value)
		assert.NoError(t, err, "autovacuum rejected %q, which the engine accepts", value)
	}
}

// The value one name carries in a resolved set, exactly as the guest is handed
// it: resolvedValues lowercases, which the combination checks want and an
// assertion about what is written into an option file does not.
func resolvedParameter(t *testing.T, resolved []Setting, name string) string {
	t.Helper()
	for _, setting := range resolved {
		if setting.Name == name {
			return setting.Value
		}
	}
	t.Fatalf("the resolved set carries no %s", name)
	return ""
}

// Every spelling the API accepts is canonicalised before it leaves the control
// plane, so the guest has one literal to compare rather than a vocabulary that
// differs between the two engines.
func TestResolveEffectiveParameters_CanonicalisesEveryBooleanSpelling(t *testing.T) {
	tests := []struct {
		engine    Engine
		name      string
		spellings map[string]string
	}{
		{
			engine: enginePostgres, name: "autovacuum",
			spellings: map[string]string{
				"on": "1", "ON": "1", "true": "1", "True": "1", "yes": "1", "1": "1",
				"off": "0", "OFF": "0", "false": "0", "False": "0", "no": "0", "0": "0",
			},
		},
		// The same eight on MariaDB, whose own parser takes only six: yes and no
		// never reach mysqld, because this is where they stop being either.
		{
			engine: engineMariaDB, name: "autocommit",
			spellings: map[string]string{
				"on": "1", "ON": "1", "true": "1", "True": "1", "yes": "1", "1": "1",
				"off": "0", "OFF": "0", "false": "0", "False": "0", "no": "0", "0": "0",
			},
		},
	}
	for _, tc := range tests {
		for spelling, want := range tc.spellings {
			t.Run(tc.engine.Name+"/"+spelling, func(t *testing.T) {
				resolved, err := tc.engine.ResolveEffectiveParameters(testSizing, testSizing.SmallestInstanceClass(),
					map[string]string{tc.name: spelling})
				require.NoError(t, err)
				assert.Equal(t, want, resolvedParameter(t, resolved, tc.name))
			})
		}
	}
}

// The catalog's own defaults go through it too, so no boolean reaches an option
// file in whichever spelling the catalog entry happens to be written in.
func TestResolveEffectiveParameters_CanonicalisesTheDefaultsToo(t *testing.T) {
	for _, engine := range catalogEngines {
		t.Run(engine.Name, func(t *testing.T) {
			resolved, err := engine.ResolveEffectiveParameters(testSizing, testSizing.SmallestInstanceClass(), nil)
			require.NoError(t, err)

			booleans := 0
			for _, setting := range resolved {
				spec, ok := engine.LookupParameter(setting.Name)
				require.True(t, ok)
				if spec.DataType != ParamTypeBoolean {
					continue
				}
				booleans++
				assert.Contains(t, []string{"1", "0"}, setting.Value,
					"%s reaches the guest as %q", setting.Name, setting.Value)
			}
			assert.NotZero(t, booleans, "no boolean in the set, so this asserts nothing")
		})
	}
}

// The catalog's first present-and-unmodifiable entries. AWS exposes both as
// modifiable, so being absent would read as a platform gap where a refusal
// naming the parameter reads as policy.
func TestParameterCatalog_TLSFloorIsPinnedAndNotModifiable(t *testing.T) {
	tests := []struct {
		engine Engine
		name   string
	}{
		{enginePostgres, "ssl_min_protocol_version"},
		{engineMariaDB, "tls_version"},
	}
	for _, tc := range tests {
		t.Run(tc.engine.Name, func(t *testing.T) {
			spec, ok := tc.engine.LookupParameter(tc.name)
			require.True(t, ok, "%s exposes no %s", tc.engine.Name, tc.name)
			assert.Equal(t, "TLSv1.3", spec.Default)
			assert.False(t, spec.IsModifiable)

			_, err := tc.engine.ValidateParameterValue(tc.name, "TLSv1.2")
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err),
				"the code has to survive resolution or the client sees a 500")
			assert.Contains(t, err.Error(), "parameter "+tc.name+" is not modifiable")

			// Pinned in the catalog and inert unless it is also in the set the guest
			// installs, which is the half a customer's group cannot reach.
			resolved, err := tc.engine.ResolveEffectiveParameters(testSizing, testSizing.SmallestInstanceClass(), nil)
			require.NoError(t, err)
			assert.Equal(t, "TLSv1.3", resolvedParameter(t, resolved, tc.name))
		})
	}
}

// Each engine's enforcement parameter under AWS's own name for it, so a real
// aws_db_parameter_group works verbatim. Dynamic on both: MariaDB takes it as
// SET GLOBAL, and PostgreSQL's is a placeholder GUC that never appears in
// pg_settings.pending_restart, where a static classification would leave it
// reported as applied and enforcing nothing.
func TestParameterCatalog_EachEngineExposesItsTLSEnforcementParameter(t *testing.T) {
	tests := []struct {
		engine Engine
		name   string
		// Every spelling the API accepts, on both engines: a group written for AWS
		// RDS carries whichever one its author chose.
		spellings map[string]string
	}{
		{enginePostgres, "rds.force_ssl", map[string]string{"on": "1", "true": "1", "yes": "1", "off": "0", "false": "0", "no": "0"}},
		{engineMariaDB, "require_secure_transport", map[string]string{"on": "1", "true": "1", "yes": "1", "off": "0", "false": "0", "no": "0"}},
	}
	for _, tc := range tests {
		t.Run(tc.engine.Name, func(t *testing.T) {
			spec, ok := tc.engine.LookupParameter(tc.name)
			require.True(t, ok, "%s exposes no %s", tc.engine.Name, tc.name)
			assert.Equal(t, ParamTypeBoolean, spec.DataType)
			assert.Equal(t, ApplyTypeDynamic, spec.ApplyType)
			assert.True(t, spec.IsModifiable, "a customer can turn enforcement off, exactly as on AWS")
			assert.Equal(t, "1", spec.Default, "an instance with no parameter group of its own enforces TLS")

			// The guest derives enforcement from this name rather than from the engine
			// it happens to be running, so the two have to agree on it.
			assert.Equal(t, tc.name, tc.engine.TLSEnforcementParameter())

			// Whichever spelling the group carries, the guest has one literal to compare.
			for spelling, want := range tc.spellings {
				resolved, err := tc.engine.ResolveEffectiveParameters(testSizing, testSizing.SmallestInstanceClass(),
					map[string]string{tc.name: spelling})
				require.NoError(t, err)
				assert.Equal(t, want, resolvedParameter(t, resolved, tc.name))
			}
		})
	}
}

// A group's overrides are laid over the whole catalog, not merged into a subset:
// every parameter is present, and one the group does not set carries its default.
func TestResolveEffectiveParameters_OverlaysOverridesOnDefaults(t *testing.T) {
	t.Parallel()
	resolved, err := enginePostgres.ResolveEffectiveParameters(testSizing, "db.t3.medium", map[string]string{"work_mem": "16384"})
	require.NoError(t, err)
	require.Len(t, resolved, len(enginePostgres.CatalogParameterNames()))

	values := map[string]string{}
	for _, setting := range resolved {
		values[setting.Name] = setting.Value
	}
	assert.Equal(t, "16384", values["work_mem"], "the override did not win")

	memoryMiB, err := testSizing.ClassMemoryMiB("db.t3.medium")
	require.NoError(t, err)
	assert.Equal(t, sharedBuffersFor(memoryMiB), values["shared_buffers"],
		"a parameter the group does not set should carry its computed default")
}

// Sorted output is what lets a re-resolve that changed nothing produce a
// byte-identical include, so an apply does not restart an engine for no reason.
func TestResolveEffectiveParameters_IsSortedAndCarriesNoFormula(t *testing.T) {
	t.Parallel()
	resolved, err := enginePostgres.ResolveEffectiveParameters(testSizing, "db.m5.xlarge", nil)
	require.NoError(t, err)

	for i := 1; i < len(resolved); i++ {
		assert.Less(t, resolved[i-1].Name, resolved[i].Name, "the resolved set is not sorted by name")
	}
	for _, setting := range resolved {
		assert.NotContains(t, setting.Value, "{", "%s reached the agent as a formula", setting.Name)
		assert.NotContains(t, setting.Value, "DBInstanceClass", "%s reached the agent as a reference", setting.Name)
	}
}

// A stored value the catalog would now reject must not keep being handed to the
// engine, so the resolve re-validates rather than trusting what was written.
func TestResolveEffectiveParameters_RevalidatesStoredOverrides(t *testing.T) {
	t.Parallel()
	_, err := enginePostgres.ResolveEffectiveParameters(testSizing, "db.t3.medium", map[string]string{"max_connections": "999999"})
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
			_, err := enginePostgres.ResolveEffectiveParameters(testSizing, "db.t3.micro", map[string]string{tt.name: tt.value})
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err))
			assert.Contains(t, err.Error(), "db.t3.micro")
			assert.Contains(t, err.Error(), tt.value)
			assert.Contains(t, err.Error(), "ceiling")

			_, err = enginePostgres.ResolveEffectiveParameters(testSizing, "db.m5.xlarge", map[string]string{tt.name: tt.value})
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
			_, err := enginePostgres.ResolveEffectiveParameters(testSizing, "db.m5.xlarge", tt.overrides)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err))
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestResolveEffectiveParameters_AcceptsMinimalWALWithoutReplication(t *testing.T) {
	t.Parallel()
	_, err := enginePostgres.ResolveEffectiveParameters(testSizing, "db.t3.micro", map[string]string{
		"wal_level": "minimal", "max_wal_senders": "0", "max_replication_slots": "0",
	})
	require.NoError(t, err)
}

func TestParameterCatalog_Postgres18ApplyTypesAndBuiltCompressionMethods(t *testing.T) {
	t.Parallel()
	autovacuumWorkers, _ := enginePostgres.LookupParameter("autovacuum_max_workers")
	assert.Equal(t, ApplyTypeDynamic, autovacuumWorkers.ApplyType)

	walCompression, _ := enginePostgres.LookupParameter("wal_compression")
	assert.ElementsMatch(t, []string{"off", "pglz", "lz4", "zstd", "on"}, walCompression.Enum)
}

func TestResolveEffectiveParameters_RejectsAnUnknownClass(t *testing.T) {
	t.Parallel()
	_, err := enginePostgres.ResolveEffectiveParameters(testSizing, "db.x99.mega", nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err),
		"the code has to survive resolution or the client sees a 500")
}

// AllowedValues is what tells a customer what an integer means, so a unitful
// parameter has to report its unit rather than a bare pair of numbers.
func TestParameterSpec_AllowedValuesDescribesTheConstraint(t *testing.T) {
	t.Parallel()
	shared, _ := enginePostgres.LookupParameter("shared_buffers")
	assert.Equal(t, "16-4194304 (8kB)", shared.AllowedValues())

	logStatement, _ := enginePostgres.LookupParameter("log_statement")
	assert.Equal(t, "none,ddl,mod,all", logStatement.AllowedValues())

	timezone, _ := enginePostgres.LookupParameter("timezone")
	assert.Empty(t, timezone.AllowedValues(), "a free-form string has no constraint to report")

	target, _ := enginePostgres.LookupParameter("checkpoint_completion_target")
	assert.True(t, strings.HasPrefix(target.AllowedValues(), "0-1"), "got %q", target.AllowedValues())
}

// catalogAgentContract is one row of the agent-contract metadata a catalog
// parameter carries: the name the option file is written under, its data type
// and its apply semantics. Hand-transcribed from postgres.go and mariadb.go,
// not derived from the catalog at test time.
type catalogAgentContract struct {
	optionFileName string
	dataType       string
	applyType      string
}

var postgresAgentContract = map[string]catalogAgentContract{
	"max_connections":                     {"max_connections", ParamTypeInteger, ApplyTypeStatic},
	"superuser_reserved_connections":      {"superuser_reserved_connections", ParamTypeInteger, ApplyTypeStatic},
	"idle_in_transaction_session_timeout": {"idle_in_transaction_session_timeout", ParamTypeInteger, ApplyTypeDynamic},
	"statement_timeout":                   {"statement_timeout", ParamTypeInteger, ApplyTypeDynamic},
	"lock_timeout":                        {"lock_timeout", ParamTypeInteger, ApplyTypeDynamic},
	"tcp_keepalives_idle":                 {"tcp_keepalives_idle", ParamTypeInteger, ApplyTypeDynamic},
	"shared_buffers":                      {"shared_buffers", ParamTypeInteger, ApplyTypeStatic},
	"effective_cache_size":                {"effective_cache_size", ParamTypeInteger, ApplyTypeDynamic},
	"work_mem":                            {"work_mem", ParamTypeInteger, ApplyTypeDynamic},
	"maintenance_work_mem":                {"maintenance_work_mem", ParamTypeInteger, ApplyTypeDynamic},
	"temp_buffers":                        {"temp_buffers", ParamTypeInteger, ApplyTypeDynamic},
	"max_prepared_transactions":           {"max_prepared_transactions", ParamTypeInteger, ApplyTypeStatic},
	"max_worker_processes":                {"max_worker_processes", ParamTypeInteger, ApplyTypeStatic},
	"max_parallel_workers":                {"max_parallel_workers", ParamTypeInteger, ApplyTypeDynamic},
	"max_parallel_workers_per_gather":     {"max_parallel_workers_per_gather", ParamTypeInteger, ApplyTypeDynamic},
	"wal_level":                           {"wal_level", ParamTypeEnum, ApplyTypeStatic},
	"synchronous_commit":                  {"synchronous_commit", ParamTypeEnum, ApplyTypeDynamic},
	"wal_compression":                     {"wal_compression", ParamTypeEnum, ApplyTypeDynamic},
	"max_wal_size":                        {"max_wal_size", ParamTypeInteger, ApplyTypeDynamic},
	"min_wal_size":                        {"min_wal_size", ParamTypeInteger, ApplyTypeDynamic},
	"checkpoint_timeout":                  {"checkpoint_timeout", ParamTypeInteger, ApplyTypeDynamic},
	"checkpoint_completion_target":        {"checkpoint_completion_target", ParamTypeReal, ApplyTypeDynamic},
	"wal_buffers":                         {"wal_buffers", ParamTypeInteger, ApplyTypeStatic},
	"max_wal_senders":                     {"max_wal_senders", ParamTypeInteger, ApplyTypeStatic},
	"max_replication_slots":               {"max_replication_slots", ParamTypeInteger, ApplyTypeStatic},
	"autovacuum":                          {"autovacuum", ParamTypeBoolean, ApplyTypeDynamic},
	"autovacuum_max_workers":              {"autovacuum_max_workers", ParamTypeInteger, ApplyTypeDynamic},
	"autovacuum_naptime":                  {"autovacuum_naptime", ParamTypeInteger, ApplyTypeDynamic},
	"autovacuum_vacuum_threshold":         {"autovacuum_vacuum_threshold", ParamTypeInteger, ApplyTypeDynamic},
	"autovacuum_vacuum_scale_factor":      {"autovacuum_vacuum_scale_factor", ParamTypeReal, ApplyTypeDynamic},
	"autovacuum_analyze_threshold":        {"autovacuum_analyze_threshold", ParamTypeInteger, ApplyTypeDynamic},
	"autovacuum_analyze_scale_factor":     {"autovacuum_analyze_scale_factor", ParamTypeReal, ApplyTypeDynamic},
	"autovacuum_vacuum_cost_limit":        {"autovacuum_vacuum_cost_limit", ParamTypeInteger, ApplyTypeDynamic},
	"random_page_cost":                    {"random_page_cost", ParamTypeReal, ApplyTypeDynamic},
	"seq_page_cost":                       {"seq_page_cost", ParamTypeReal, ApplyTypeDynamic},
	"effective_io_concurrency":            {"effective_io_concurrency", ParamTypeInteger, ApplyTypeDynamic},
	"default_statistics_target":           {"default_statistics_target", ParamTypeInteger, ApplyTypeDynamic},
	"jit":                                 {"jit", ParamTypeBoolean, ApplyTypeDynamic},
	"log_min_duration_statement":          {"log_min_duration_statement", ParamTypeInteger, ApplyTypeDynamic},
	"log_statement":                       {"log_statement", ParamTypeEnum, ApplyTypeDynamic},
	"log_min_messages":                    {"log_min_messages", ParamTypeEnum, ApplyTypeDynamic},
	"log_connections":                     {"log_connections", ParamTypeBoolean, ApplyTypeDynamic},
	"log_disconnections":                  {"log_disconnections", ParamTypeBoolean, ApplyTypeDynamic},
	"log_lock_waits":                      {"log_lock_waits", ParamTypeBoolean, ApplyTypeDynamic},
	"log_temp_files":                      {"log_temp_files", ParamTypeInteger, ApplyTypeDynamic},
	"log_autovacuum_min_duration":         {"log_autovacuum_min_duration", ParamTypeInteger, ApplyTypeDynamic},
	"deadlock_timeout":                    {"deadlock_timeout", ParamTypeInteger, ApplyTypeDynamic},
	"timezone":                            {"timezone", ParamTypeString, ApplyTypeDynamic},
	"datestyle":                           {"datestyle", ParamTypeString, ApplyTypeDynamic},
	"track_activity_query_size":           {"track_activity_query_size", ParamTypeInteger, ApplyTypeStatic},
	"track_io_timing":                     {"track_io_timing", ParamTypeBoolean, ApplyTypeDynamic},
	"ssl_min_protocol_version":            {"ssl_min_protocol_version", ParamTypeString, ApplyTypeDynamic},
	"rds.force_ssl":                       {"rds.force_ssl", ParamTypeBoolean, ApplyTypeDynamic},
}

var mariadbAgentContract = map[string]catalogAgentContract{
	"max_connections":                {"max_connections", ParamTypeInteger, ApplyTypeDynamic},
	"max_connect_errors":             {"max_connect_errors", ParamTypeInteger, ApplyTypeDynamic},
	"max_allowed_packet":             {"max_allowed_packet", ParamTypeInteger, ApplyTypeDynamic},
	"thread_cache_size":              {"thread_cache_size", ParamTypeInteger, ApplyTypeDynamic},
	"wait_timeout":                   {"wait_timeout", ParamTypeInteger, ApplyTypeDynamic},
	"interactive_timeout":            {"interactive_timeout", ParamTypeInteger, ApplyTypeDynamic},
	"connect_timeout":                {"connect_timeout", ParamTypeInteger, ApplyTypeDynamic},
	"net_read_timeout":               {"net_read_timeout", ParamTypeInteger, ApplyTypeDynamic},
	"net_write_timeout":              {"net_write_timeout", ParamTypeInteger, ApplyTypeDynamic},
	"lock_wait_timeout":              {"lock_wait_timeout", ParamTypeInteger, ApplyTypeDynamic},
	"innodb_lock_wait_timeout":       {"innodb_lock_wait_timeout", ParamTypeInteger, ApplyTypeDynamic},
	"max_statement_time":             {"max_statement_time", ParamTypeReal, ApplyTypeDynamic},
	"idle_transaction_timeout":       {"idle_transaction_timeout", ParamTypeInteger, ApplyTypeDynamic},
	"innodb_buffer_pool_size":        {"innodb_buffer_pool_size", ParamTypeInteger, ApplyTypeStatic},
	"innodb_log_file_size":           {"innodb_log_file_size", ParamTypeInteger, ApplyTypeStatic},
	"innodb_flush_method":            {"innodb_flush_method", ParamTypeEnum, ApplyTypeStatic},
	"innodb_flush_log_at_trx_commit": {"innodb_flush_log_at_trx_commit", ParamTypeInteger, ApplyTypeDynamic},
	"innodb_read_io_threads":         {"innodb_read_io_threads", ParamTypeInteger, ApplyTypeStatic},
	"innodb_write_io_threads":        {"innodb_write_io_threads", ParamTypeInteger, ApplyTypeStatic},
	"innodb_purge_threads":           {"innodb_purge_threads", ParamTypeInteger, ApplyTypeStatic},
	"innodb_autoinc_lock_mode":       {"innodb_autoinc_lock_mode", ParamTypeInteger, ApplyTypeStatic},
	"innodb_io_capacity":             {"innodb_io_capacity", ParamTypeInteger, ApplyTypeDynamic},
	"innodb_io_capacity_max":         {"innodb_io_capacity_max", ParamTypeInteger, ApplyTypeDynamic},
	"innodb_max_dirty_pages_pct":     {"innodb_max_dirty_pages_pct", ParamTypeReal, ApplyTypeDynamic},
	"innodb_flush_neighbors":         {"innodb_flush_neighbors", ParamTypeInteger, ApplyTypeDynamic},
	"innodb_file_per_table":          {"innodb_file_per_table", ParamTypeBoolean, ApplyTypeDynamic},
	"innodb_stats_persistent":        {"innodb_stats_persistent", ParamTypeBoolean, ApplyTypeDynamic},
	"innodb_strict_mode":             {"innodb_strict_mode", ParamTypeBoolean, ApplyTypeDynamic},
	"innodb_adaptive_hash_index":     {"innodb_adaptive_hash_index", ParamTypeBoolean, ApplyTypeDynamic},
	"sort_buffer_size":               {"sort_buffer_size", ParamTypeInteger, ApplyTypeDynamic},
	"join_buffer_size":               {"join_buffer_size", ParamTypeInteger, ApplyTypeDynamic},
	"read_buffer_size":               {"read_buffer_size", ParamTypeInteger, ApplyTypeDynamic},
	"read_rnd_buffer_size":           {"read_rnd_buffer_size", ParamTypeInteger, ApplyTypeDynamic},
	"tmp_table_size":                 {"tmp_table_size", ParamTypeInteger, ApplyTypeDynamic},
	"max_heap_table_size":            {"max_heap_table_size", ParamTypeInteger, ApplyTypeDynamic},
	"key_buffer_size":                {"key_buffer_size", ParamTypeInteger, ApplyTypeDynamic},
	"table_open_cache":               {"table_open_cache", ParamTypeInteger, ApplyTypeDynamic},
	"table_definition_cache":         {"table_definition_cache", ParamTypeInteger, ApplyTypeDynamic},
	"max_prepared_stmt_count":        {"max_prepared_stmt_count", ParamTypeInteger, ApplyTypeDynamic},
	"group_concat_max_len":           {"group_concat_max_len", ParamTypeInteger, ApplyTypeDynamic},
	"performance_schema":             {"performance_schema", ParamTypeBoolean, ApplyTypeStatic},
	"slow_query_log":                 {"slow_query_log", ParamTypeBoolean, ApplyTypeDynamic},
	"long_query_time":                {"long_query_time", ParamTypeReal, ApplyTypeDynamic},
	"log_queries_not_using_indexes":  {"log_queries_not_using_indexes", ParamTypeBoolean, ApplyTypeDynamic},
	"general_log":                    {"general_log", ParamTypeBoolean, ApplyTypeDynamic},
	"log_output":                     {"log_output", ParamTypeEnum, ApplyTypeDynamic},
	"log_warnings":                   {"log_warnings", ParamTypeInteger, ApplyTypeDynamic},
	"sql_mode":                       {"sql_mode", ParamTypeString, ApplyTypeDynamic},
	"character_set_server":           {"character_set_server", ParamTypeEnum, ApplyTypeDynamic},
	"collation_server":               {"collation_server", ParamTypeEnum, ApplyTypeDynamic},
	// The one remapped entry: mariadbd has no time_zone startup option, so the
	// option file carries it under default_time_zone instead.
	"time_zone":                {"default_time_zone", ParamTypeString, ApplyTypeDynamic},
	"transaction_isolation":    {"transaction_isolation", ParamTypeEnum, ApplyTypeDynamic},
	"autocommit":               {"autocommit", ParamTypeBoolean, ApplyTypeDynamic},
	"event_scheduler":          {"event_scheduler", ParamTypeEnum, ApplyTypeDynamic},
	"tls_version":              {"tls_version", ParamTypeString, ApplyTypeStatic},
	"require_secure_transport": {"require_secure_transport", ParamTypeBoolean, ApplyTypeDynamic},
}

// Every catalog parameter of every engine, checked against a golden table
// transcribed from the catalogue source: its option-file spelling, data type
// and apply semantics. A package split that drops or renames one of these
// has to fail here rather than surface as a guest that refuses to start.
func TestCatalogueAgentContractMetadata_MatchesGoldenTable(t *testing.T) {
	tests := []struct {
		engine Engine
		golden map[string]catalogAgentContract
	}{
		{enginePostgres, postgresAgentContract},
		{engineMariaDB, mariadbAgentContract},
	}
	for _, tc := range tests {
		t.Run(tc.engine.Name, func(t *testing.T) {
			names := tc.engine.CatalogParameterNames()
			require.Len(t, names, len(tc.golden), "the catalogue gained or lost a parameter since this table was written")
			for _, name := range names {
				want, ok := tc.golden[name]
				require.True(t, ok, "%s is not in the golden table", name)

				spec, ok := tc.engine.LookupParameter(name)
				require.True(t, ok)
				assert.Equal(t, want.dataType, spec.DataType, "%s DataType", name)
				assert.Equal(t, want.applyType, spec.ApplyType, "%s ApplyType", name)
				assert.Equal(t, want.optionFileName, tc.engine.OptionFileName(name), "%s OptionFileName", name)
			}
		})
	}
}
