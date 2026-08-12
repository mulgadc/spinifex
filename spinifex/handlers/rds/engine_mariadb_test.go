package handlers_rds

import (
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Offered only now that the image and the guest implementation exist: until
// they did, a create would have resolved an AMI nothing builds and left a volume
// and an ENI behind for an instance that could never become available.
func TestMariaDB_IsOffered(t *testing.T) {
	engine, err := LookupEngine("  MariaDB ")
	require.NoError(t, err, "the engine name is matched case-insensitively and trimmed")
	assert.Equal(t, engineMariaDB.Name, engine.Name)
	assert.Contains(t, SupportedEngines(), "mariadb")

	byFamily, err := engineForFamily(engineMariaDB.ParameterGroupFamily())
	require.NoError(t, err)
	assert.Equal(t, engineMariaDB.Name, byFamily.Name)
	assert.Contains(t, SupportedParameterGroupFamilies(), "mariadb11.8")

	// mysql is a distinct AWS engine this platform does not offer, and MariaDB is
	// deliberately not aliased onto it: the alias would report an engine and a
	// version the instance is not running.
	_, err = LookupEngine("mysql")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err))
}

// The whole point of registering the engine: a request naming it has to reach
// the MariaDB AMI carrying MariaDB's own port, family and guest configuration,
// not PostgreSQL's defaults with a different name on them.
func TestCreateDBInstance_LaunchesMariaDB(t *testing.T) {
	h := newCreateHarness(t, testBaseDomain)
	input := validCreateInput()
	input.Engine = aws.String("mariadb")

	out, err := h.svc.CreateDBInstance(t.Context(), input, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out.DBInstance)
	assert.Equal(t, "mariadb", aws.StringValue(out.DBInstance.Engine))
	assert.Equal(t, "11.8", aws.StringValue(out.DBInstance.EngineVersion))
	require.NotNil(t, out.DBInstance.Endpoint)
	assert.Equal(t, int64(3306), aws.Int64Value(out.DBInstance.Endpoint.Port),
		"the engine's default port applies when the request names none")

	rec := h.record(t, testDBInstanceID)
	assert.Equal(t, "mariadb", rec.Engine)
	assert.Equal(t, int64(3306), rec.Port)
	assert.Equal(t, engineMariaDB.DefaultParameterGroupName(), rec.DBParameterGroupName)

	// The AMI is selected by the engine's own tags, so a MariaDB create can never
	// land on the PostgreSQL image however the two are published.
	amiFilters := map[string]string{}
	for _, f := range h.launch.images.filters {
		amiFilters[aws.StringValue(f.Name)] = aws.StringValue(f.Values[0])
	}
	assert.Equal(t, "mariadb", amiFilters["tag:"+engineTagKey])
	assert.Equal(t, "11.8", amiFilters["tag:"+engineVersionTagKey])

	// The guest builds its engine implementation from the image, and refuses to
	// run when cloud-init disagrees with what the image bakes.
	require.NotNil(t, h.launch.launcher.input)
	assert.Contains(t, h.launch.launcher.input.UserData, "RDS_ENGINE=mariadb")
	assert.Contains(t, h.launch.launcher.input.UserData, "RDS_ENGINE_PORT=3306")
}

func TestMariaDB_Identity(t *testing.T) {
	assert.Equal(t, "mariadb", engineMariaDB.Name)
	assert.Equal(t, int64(3306), engineMariaDB.DefaultPort)
	// The version series the API pins to, not an integer major: PostgreSQL's
	// version axis is one integer and MariaDB's is two.
	assert.Equal(t, "11.8", engineMariaDB.EngineVersion())
	assert.Equal(t, "mariadb11.8", engineMariaDB.ParameterGroupFamily())
	assert.Equal(t, "default.mariadb11.8", engineMariaDB.DefaultParameterGroupName())

	for _, version := range []string{"", "11.8"} {
		assert.NoError(t, engineMariaDB.ValidateVersion(version), "version %q", version)
	}
	for _, version := range []string{"11", "11.4", "11.8.8", "10.11", "12", "latest"} {
		assert.Error(t, engineMariaDB.ValidateVersion(version), "version %q", version)
	}
}

// The engine's own limit rather than AWS's documented 16, so a client written
// against real AWS is a strict subset and nothing that works there breaks here.
func TestMariaDB_ValidateMasterUsername(t *testing.T) {
	for _, username := range []string{"appuser", "app_user1", "Orders", strings.Repeat("a", 80)} {
		assert.NoError(t, engineMariaDB.ValidateMasterUsername(username), "username %q", username)
	}

	cases := map[string]string{
		"empty":            "",
		"leading digit":    "1user",
		"hyphen":           "app-user",
		"dot":              "app.user",
		"too long":         strings.Repeat("a", 81),
		"install-db root":  "root",
		"cased root":       "ROOT",
		"sys schema owner": "mariadb.sys",
		"socket account":   "mysql",
		"aws management":   "rdsadmin",
		// The server refuses CREATE USER PUBLIC, which would spend the one-shot
		// bootstrap password on a guaranteed failure.
		"role name": "PUBLIC",
	}
	for name, username := range cases {
		t.Run(name, func(t *testing.T) {
			err := engineMariaDB.ValidateMasterUsername(username)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err))
		})
	}
}

// The guest re-checks the name it is handed rather than trusting the control
// plane, and it reaches the reserved half on its own.
func TestMariaDB_ValidateUsernameNotReserved(t *testing.T) {
	for _, username := range []string{"mysql.session", "MySQL.infoschema", "mariadb.sys", "mariadb.anything"} {
		err := engineMariaDB.ValidateUsernameNotReserved(username)
		require.Error(t, err, "username %q", username)
		assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err))
	}
	assert.NoError(t, engineMariaDB.ValidateUsernameNotReserved("mysqladmin"),
		"the prefix is mysql. with the dot, not the bare word")
}

// MariaDB maps a database onto a directory name and rds-init interpolates the
// name into CREATE DATABASE from the shell, so this rule is the actual barrier.
func TestMariaDB_ValidateDBName(t *testing.T) {
	for _, name := range []string{"", "orders", "Orders_2", strings.Repeat("d", 64)} {
		assert.NoError(t, engineMariaDB.ValidateDBName(name), "name %q", name)
	}
	for _, name := range []string{"my-db", "1db", "my db", "my/db", `my\db`, "my.db", "db ", strings.Repeat("d", 65)} {
		err := engineMariaDB.ValidateDBName(name)
		require.Error(t, err, "name %q", name)
		assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err))
	}

	// Each engine takes its own identifier limit rather than a shared one.
	assert.Error(t, enginePostgres.ValidateDBName(strings.Repeat("d", 64)))
	assert.NoError(t, engineMariaDB.ValidateDBName(strings.Repeat("d", 64)))
}

// The two catalogs are separate tables, not one table with an engine column, so
// neither engine can be handed a setting the other's server would not parse.
func TestMariaDBCatalog_IsSeparateFromPostgres(t *testing.T) {
	for _, name := range []string{"innodb_buffer_pool_size", "sql_mode", "key_buffer_size"} {
		_, ok := enginePostgres.LookupParameter(name)
		assert.False(t, ok, "PostgreSQL must not expose %s", name)
	}
	for _, name := range []string{"work_mem", "shared_buffers", "autovacuum", "wal_level", "datestyle"} {
		_, ok := engineMariaDB.LookupParameter(name)
		assert.False(t, ok, "MariaDB must not expose %s", name)
	}
	// max_connections exists in both, and each takes its own engine's bounds.
	_, err := engineMariaDB.validateParameterValue("max_connections", "20000")
	assert.NoError(t, err)
	_, err = enginePostgres.validateParameterValue("max_connections", "20000")
	assert.Error(t, err, "PostgreSQL's own ceiling is 5000")
}

// Absent rather than present-and-unmodifiable, because they are not the
// customer's to set: the endpoint, the agent's socket access, the serving
// certificate and the snapshot recovery guarantee all depend on them.
func TestMariaDBCatalog_OmitsPlatformOwnedSettings(t *testing.T) {
	owned := []string{
		"port", "datadir", "socket", "bind_address",
		"ssl_ca", "ssl_cert", "ssl_key", "require_secure_transport",
		"secure_file_priv", "skip_symbolic_links", "log_bin",
		"default_storage_engine",
		"innodb_buffer_pool_chunk_size", "innodb_buffer_pool_instances",
	}
	for _, name := range owned {
		_, ok := engineMariaDB.LookupParameter(name)
		assert.False(t, ok, "%s is platform-owned and must not be in the catalog", name)
	}
}

// The size-derived defaults have to move with the class, or the formulas are
// decoration and one end of the supported range is mis-tuned.
func TestMariaDBCatalog_SizeDerivedDefaultsScaleWithClassMemory(t *testing.T) {
	smallest, err := classMemoryMiB("db.t3.micro")
	require.NoError(t, err)
	largest, err := classMemoryMiB("db.m5.xlarge")
	require.NoError(t, err)

	for _, name := range []string{"innodb_buffer_pool_size", "max_connections"} {
		spec, ok := engineMariaDB.LookupParameter(name)
		require.True(t, ok, "%s is expected to carry a computed default", name)
		require.NotNil(t, spec.DefaultFor, "%s should be size-derived", name)
		require.NotNil(t, spec.MaxFor, "%s should carry a class ceiling", name)

		small, err := strconv.ParseInt(spec.DefaultAt(smallest), 10, 64)
		require.NoError(t, err)
		large, err := strconv.ParseInt(spec.DefaultAt(largest), 10, 64)
		require.NoError(t, err)
		assert.Less(t, small, large, "%s does not grow with the class", name)
		assert.LessOrEqual(t, small, spec.MaxFor(smallest), "%s defaults above its own class ceiling", name)
	}
}

// Three quarters of class memory is RDS's own default for this engine family,
// and it is the setting whose being wrong stops the server from starting.
func TestMariaDBCatalog_BufferPoolIsThreeQuartersOfClassMemory(t *testing.T) {
	memoryMiB, err := classMemoryMiB("db.m5.large")
	require.NoError(t, err)

	bytes, err := strconv.ParseInt(innodbBufferPoolSizeFor(memoryMiB), 10, 64)
	require.NoError(t, err)
	assert.Equal(t, memoryMiB*mibToBytes*3/4, bytes)
	assert.Less(t, bytes, innodbBufferPoolSizeCeilingFor(memoryMiB),
		"the ceiling has to leave headroom above the default")
	assert.Less(t, innodbBufferPoolSizeCeilingFor(memoryMiB), memoryMiB*mibToBytes,
		"a pool at class memory is a server that never starts")
}

// max_connections = {DBInstanceClassMemory/12582880}, which is RDS's own formula.
func TestMariaDBCatalog_MaxConnectionsFollowsTheRDSFormula(t *testing.T) {
	memoryMiB, err := classMemoryMiB("db.m5.xlarge")
	require.NoError(t, err)
	assert.Equal(t, strconv.FormatInt(memoryMiB*mibToBytes/12582880, 10), mariadbMaxConnectionsFor(memoryMiB))

	// The floor keeps the smallest class above what the platform's own probe,
	// parameter apply and password rotation sessions need.
	smallest, err := classMemoryMiB(SmallestInstanceClass())
	require.NoError(t, err)
	connections, err := strconv.ParseInt(mariadbMaxConnectionsFor(smallest), 10, 64)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, connections, int64(20))
}

func TestMariaDBCatalog_RejectsBadValues(t *testing.T) {
	cases := []struct {
		name, param, value, want string
	}{
		{"UnknownName", "not_a_setting", "1", "is not a parameter this engine exposes"},
		{"PostgresSetting", "work_mem", "16384", "is not a parameter this engine exposes"},
		{"NotAnInteger", "max_connections", "many", "takes an integer"},
		{"BelowRange", "max_connections", "1", "outside its allowed range"},
		{"BufferPoolBelowMinimum", "innodb_buffer_pool_size", "1048576", "outside its allowed range"},
		{"RedoLogBelowMinimum", "innodb_log_file_size", "1048576", "outside its allowed range"},
		{"NotAReal", "long_query_time", "slow", "takes a number"},
		{"RealOutOfRange", "innodb_max_dirty_pages_pct", "100", "outside its allowed range"},
		{"NotInEnum", "log_output", "syslog", "does not accept"},
		{"NotAnIsolationLevel", "transaction_isolation", "snapshot", "does not accept"},
		{"UnknownSQLMode", "sql_mode", "STRICT_TRANS_TABLES,NOT_A_MODE", "does not accept"},
		{"EmptySQLModeToken", "sql_mode", "STRICT_TRANS_TABLES,,ANSI_QUOTES", "does not accept"},
		{"NamedTimeZone", "time_zone", "Australia/Sydney", "does not accept"},
		{"OffsetOutOfRange", "time_zone", "+15:00", "does not accept"},
		{"UnsignedOffset", "time_zone", "10:00", "does not accept"},
		{"OffsetWithoutMinutes", "time_zone", "+10", "does not accept"},
		{"OffsetWithShortHour", "time_zone", "+1:00", "does not accept"},
		{"OffsetWithLongHour", "time_zone", "+0001:00", "does not accept"},
		{"Empty", "sort_buffer_size", "", "empty value"},
		{"Formula", "innodb_buffer_pool_size", "{DBInstanceClassMemory*3/4}", "is a formula"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := engineMariaDB.validateParameterValue(tc.param, tc.value)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err),
				"the code has to survive resolution or the client sees a 500")
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestMariaDBCatalog_AcceptsEngineSpelledValues(t *testing.T) {
	cases := []struct{ param, value string }{
		{"sql_mode", "STRICT_TRANS_TABLES,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION"},
		{"sql_mode", "ansi_quotes"},
		{"sql_mode", "TRADITIONAL, ONLY_FULL_GROUP_BY"},
		{"time_zone", "SYSTEM"},
		{"time_zone", "system"},
		{"time_zone", "+10:00"},
		{"time_zone", "-05:30"},
		{"time_zone", "+00:00"},
		{"innodb_flush_method", "O_DIRECT"},
		{"transaction_isolation", "READ-COMMITTED"},
		{"log_output", "TABLE"},
	}
	for _, tc := range cases {
		t.Run(tc.param+"/"+tc.value, func(t *testing.T) {
			_, err := engineMariaDB.validateParameterValue(tc.param, tc.value)
			assert.NoError(t, err)
		})
	}
}

// MariaDB refuses yes and no for a boolean system variable where PostgreSQL
// accepts them, so a value the API takes must be one mysqld will parse.
func TestMariaDBCatalog_TakesOnlyTheEnginesBooleanSpellings(t *testing.T) {
	for _, value := range []string{"on", "OFF", "true", "False", "1", "0"} {
		_, err := engineMariaDB.validateParameterValue("slow_query_log", value)
		assert.NoError(t, err, "slow_query_log rejected %q, which the engine accepts", value)
	}
	for _, value := range []string{"yes", "no"} {
		_, err := engineMariaDB.validateParameterValue("slow_query_log", value)
		require.Error(t, err, "slow_query_log accepted %q, which the engine refuses", value)
		assert.Contains(t, err.Error(), "takes a boolean")
	}

	spec, ok := engineMariaDB.LookupParameter("slow_query_log")
	require.True(t, ok)
	assert.Equal(t, "on,off,true,false,1,0", spec.AllowedValues(),
		"a customer has to be able to read which spellings this engine takes")
}

func TestMariaDBCatalog_RejectsFatalCombinations(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]string
		want      string
	}{
		{
			name:      "collation outside the server character set",
			overrides: map[string]string{"character_set_server": "latin1"},
			want:      "does not belong to character_set_server",
		},
		{
			name:      "io capacity maximum below the steady rate",
			overrides: map[string]string{"innodb_io_capacity": "4000"},
			want:      "innodb_io_capacity_max must be at least innodb_io_capacity",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := engineMariaDB.ResolveEffectiveParameters("db.m5.xlarge", tt.overrides)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err))
			assert.Contains(t, err.Error(), tt.want)
		})
	}

	_, err := engineMariaDB.ResolveEffectiveParameters("db.m5.xlarge", map[string]string{
		"character_set_server": "latin1", "collation_server": "latin1_bin",
	})
	assert.NoError(t, err, "a consistent pair is the customer's to choose")
}

// The buffer pool is the one setting a large-class literal makes a smaller guest
// unbootable with, so its class ceiling has to bind.
func TestMariaDBCatalog_BoundsSizeDerivedOverridesToTheClass(t *testing.T) {
	tests := []struct{ name, value string }{
		{name: "innodb_buffer_pool_size", value: "12884901888"},
		{name: "max_connections", value: "2000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := engineMariaDB.ResolveEffectiveParameters("db.t3.micro", map[string]string{tt.name: tt.value})
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err))
			assert.Contains(t, err.Error(), "db.t3.micro")
			assert.Contains(t, err.Error(), "ceiling")

			_, err = engineMariaDB.ResolveEffectiveParameters("db.m5.xlarge", map[string]string{tt.name: tt.value})
			assert.NoError(t, err, "the same literal should fit the larger class")
		})
	}
}

// The apply type is what decides whether a change is a SET GLOBAL or a restart,
// and the guest's pending-restart comparison reads the static keys out of the
// installed file. Getting one wrong is a permanent pending-reboot or a change
// the customer is told took effect and did not.
func TestMariaDBCatalog_ApplyTypesMatchTheEngine(t *testing.T) {
	static := []string{
		"innodb_buffer_pool_size", "innodb_log_file_size", "innodb_flush_method",
		"innodb_read_io_threads", "innodb_write_io_threads", "innodb_purge_threads",
		"innodb_autoinc_lock_mode", "performance_schema",
	}
	for _, name := range static {
		spec, ok := engineMariaDB.LookupParameter(name)
		require.True(t, ok, "%s", name)
		assert.Equal(t, ApplyTypeStatic, spec.ApplyType, "%s", name)
	}

	dynamic := []string{
		"max_connections", "max_allowed_packet", "innodb_flush_log_at_trx_commit",
		"innodb_io_capacity", "key_buffer_size", "table_open_cache", "sql_mode",
		"slow_query_log", "long_query_time", "time_zone", "transaction_isolation",
	}
	for _, name := range dynamic {
		spec, ok := engineMariaDB.LookupParameter(name)
		require.True(t, ok, "%s", name)
		assert.Equal(t, ApplyTypeDynamic, spec.ApplyType, "%s", name)
	}
}

func TestMariaDBCatalog_ResolvesEveryParameterAsALiteral(t *testing.T) {
	resolved, err := engineMariaDB.ResolveEffectiveParameters("db.m5.large", map[string]string{"max_connections": "300"})
	require.NoError(t, err)
	require.Len(t, resolved, len(engineMariaDB.CatalogParameterNames()))

	values := map[string]string{}
	for i, param := range resolved {
		if i > 0 {
			assert.Less(t, resolved[i-1].Name, param.Name, "the resolved set is not sorted by name")
		}
		assert.NotContains(t, param.Value, "{", "%s reached the agent as a formula", param.Name)
		assert.NotContains(t, param.Value, "DBInstanceClass", "%s reached the agent as a reference", param.Name)
		values[param.Name] = param.Value
	}
	assert.Equal(t, "300", values["max_connections"], "the override did not win")

	memoryMiB, err := classMemoryMiB("db.m5.large")
	require.NoError(t, err)
	assert.Equal(t, innodbBufferPoolSizeFor(memoryMiB), values["innodb_buffer_pool_size"],
		"a parameter the group does not set should carry its computed default")
}

// The bug that kept every MariaDB instance from booting: mariadbd has no
// time_zone startup option, so an option file naming it aborts the server before
// it opens the datadir.
func TestMariaDBCatalog_TimeZoneUsesItsStartupSpellingInTheOptionFile(t *testing.T) {
	assert.Equal(t, "default_time_zone", engineMariaDB.OptionFileName("time_zone"))

	// The customer-facing name is what the API reports and what a live SET GLOBAL
	// names, so the catalog must still carry it.
	spec, ok := engineMariaDB.LookupParameter("time_zone")
	require.True(t, ok)
	assert.Equal(t, "time_zone", spec.Name)
	assert.True(t, spec.IsModifiable)
}

func TestMariaDBCatalog_EveryOtherParameterKeepsItsName(t *testing.T) {
	for _, name := range engineMariaDB.CatalogParameterNames() {
		if name == "time_zone" {
			continue
		}
		assert.Equal(t, name, engineMariaDB.OptionFileName(name),
			"%s was given an option-file spelling but the fixture asserts none", name)
	}
}

// PostgreSQL sets every one of its settings under the name the customer knows,
// so a mapping there would be a bug rather than a fix.
func TestPostgresCatalog_NoParameterIsRenamedForTheOptionFile(t *testing.T) {
	for _, name := range enginePostgres.CatalogParameterNames() {
		assert.Equal(t, name, enginePostgres.OptionFileName(name))
	}
}

// A name the catalog does not carry is the customer's own, and the option file
// takes it unchanged rather than silently dropping it.
func TestMariaDBCatalog_OptionFileNamePassesAnUnknownNameThrough(t *testing.T) {
	assert.Equal(t, "not_a_parameter", engineMariaDB.OptionFileName("not_a_parameter"))
}
