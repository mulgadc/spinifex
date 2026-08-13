package handlers_rds

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// MariaDB 11.8 is the current LTS series and the only one in the pinned Alpine
// base. MajorVersion is the series rather than an integer major — MariaDB's
// version axis is two — so the family reads mariadb11.8, as AWS RDS's does.
var engineMariaDB = Engine{
	Name:         "mariadb",
	MajorVersion: "11.8",
	DefaultPort:  3306,
	// root is the superuser mariadb-install-db creates and rds-init keeps,
	// mariadb.sys owns the sys schema views, mysql is created under unix_socket
	// auth, rdsadmin is AWS's management role, PUBLIC the server refuses outright.
	reservedUsernames:        []string{"root", "mariadb.sys", "mysql", "rdsadmin", "public"},
	reservedUsernamePrefixes: []string{"mysql.", "mariadb."},
	// The engine's own limit rather than AWS's documented 16. A client written
	// against real AWS stays inside 16 either way, so this accepts a strict
	// superset; 16 would refuse a 20-character name the server handles fine.
	maxUsernameLen: 80,
	// MariaDB maps a database name onto a directory name and its client has no
	// identifier interpolation, so rds-init builds CREATE DATABASE by shell
	// interpolation and this rule is the barrier rather than defence in depth.
	validateDBName:          dbNameRule(64),
	catalog:                 mariadbParameterCatalog,
	validateCombinations:    validateMariaDBParameterCombinations,
	tlsEnforcementParameter: "require_secure_transport",
	// Only InnoDB keeps a redo log. Aria and MyISAM have none, so a datadir torn
	// mid-write can leave one of their tables inconsistent with no way back.
	crashRecoveryNote: "InnoDB tables will recover from the redo log when it is restored; " +
		"non-transactional tables such as Aria and MyISAM may be left inconsistent.",
	uncleanStopNote: "InnoDB tables will recover from the redo log on the next start; " +
		"non-transactional tables such as Aria and MyISAM may be left inconsistent.",
}

// The curated MariaDB 11.8 table: a validated subset, because a static parameter
// the server refuses at startup is a boot loop with the bad config already on
// the data volume.
//
// Platform-owned settings are absent, except the ones AWS exposes as modifiable
// and this platform pins: those are present and unmodifiable, so a refusal reads
// as policy rather than as a missing feature.
var mariadbParameterCatalog = withMariaDBBooleanSpellings(buildParameterCatalog(
	// Connections and threads. max_connections is what a size-derived default
	// matters most for: RDS's own formula is {DBInstanceClassMemory/12582880}.
	ParameterSpec{
		Name: "max_connections", DataType: ParamTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 10, Max: 100000,
		DefaultFor:  mariadbMaxConnectionsFor,
		MaxFor:      mariadbMaxConnectionsCeilingFor,
		Description: "Maximum number of concurrent client connections.",
	},
	ParameterSpec{
		Name: "max_connect_errors", DataType: ParamTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 1, Max: 4294967295, Default: "100",
		Description: "Successive interrupted connections from a host before it is blocked.",
	},
	ParameterSpec{
		Name: "max_allowed_packet", DataType: ParamTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 1024, Max: 1073741824, Default: "16777216", Unit: "B",
		Description: "Maximum size of one packet or generated string, in bytes.",
	},
	ParameterSpec{
		Name: "thread_cache_size", DataType: ParamTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 0, Max: 16384, Default: "256",
		Description: "Threads the server keeps cached for reuse by new connections.",
	},
	ParameterSpec{
		Name: "wait_timeout", DataType: ParamTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 1, Max: 31536000, Default: "28800", Unit: "s",
		Description: "Seconds the server waits for activity on a non-interactive connection before closing it.",
	},
	ParameterSpec{
		Name: "interactive_timeout", DataType: ParamTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 1, Max: 31536000, Default: "28800", Unit: "s",
		Description: "Seconds the server waits for activity on an interactive connection before closing it.",
	},
	ParameterSpec{
		Name: "connect_timeout", DataType: ParamTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 2, Max: 31536000, Default: "10", Unit: "s",
		Description: "Seconds the server waits for a connection packet before responding with a bad handshake.",
	},
	ParameterSpec{
		Name: "net_read_timeout", DataType: ParamTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 1, Max: 31536000, Default: "30", Unit: "s",
		Description: "Seconds to wait for more data from a connection before aborting the read.",
	},
	ParameterSpec{
		Name: "net_write_timeout", DataType: ParamTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 1, Max: 31536000, Default: "60", Unit: "s",
		Description: "Seconds to wait for a block to be written to a connection before aborting the write.",
	},

	// Locks and statement timeouts. lock_wait_timeout also bounds how long a
	// snapshot quiesce can hold the server, so its default is the engine's.
	ParameterSpec{
		Name: "lock_wait_timeout", DataType: ParamTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 1, Max: 31536000, Default: "86400", Unit: "s",
		Description: "Seconds to wait for a metadata lock before failing the statement.",
	},
	ParameterSpec{
		Name: "innodb_lock_wait_timeout", DataType: ParamTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 1, Max: 1073741824, Default: "50", Unit: "s",
		Description: "Seconds an InnoDB transaction waits for a row lock before it is rolled back.",
	},
	ParameterSpec{
		Name: "max_statement_time", DataType: ParamTypeReal, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 0, Max: 31536000, Default: "0", Unit: "s",
		Description: "Seconds a statement may run before it is aborted; 0 disables.",
	},
	ParameterSpec{
		Name: "idle_transaction_timeout", DataType: ParamTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 0, Max: 31536000, Default: "0", Unit: "s",
		Description: "Seconds an idle open transaction may live before its connection is killed; 0 disables.",
	},

	// InnoDB. The buffer pool is three quarters of class memory, which is RDS's
	// own default and the setting whose being wrong makes the guest unbootable.
	ParameterSpec{
		Name: "innodb_buffer_pool_size", DataType: ParamTypeInteger, ApplyType: ApplyTypeStatic,
		IsModifiable: true, Min: 5242880, Max: 1099511627776, Unit: "B",
		DefaultFor:  innodbBufferPoolSizeFor,
		MaxFor:      innodbBufferPoolSizeCeilingFor,
		Description: "Memory InnoDB caches table and index data in, in bytes.",
	},
	ParameterSpec{
		Name: "innodb_log_file_size", DataType: ParamTypeInteger, ApplyType: ApplyTypeStatic,
		IsModifiable: true, Min: 4194304, Max: 549755813888, Default: "100663296", Unit: "B",
		Description: "Size of the InnoDB redo log, in bytes.",
	},
	ParameterSpec{
		Name: "innodb_flush_method", DataType: ParamTypeEnum, ApplyType: ApplyTypeStatic,
		IsModifiable: true,
		Enum:         []string{"fsync", "o_dsync", "littlesync", "nosync", "o_direct", "o_direct_no_fsync"},
		Default:      "o_direct",
		Description:  "How InnoDB flushes data and log files to the data volume.",
	},
	ParameterSpec{
		Name: "innodb_flush_log_at_trx_commit", DataType: ParamTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 0, Max: 2, Default: "1",
		Description: "When the redo log is written and flushed; 1 flushes at every commit.",
	},
	ParameterSpec{
		Name: "innodb_read_io_threads", DataType: ParamTypeInteger, ApplyType: ApplyTypeStatic,
		IsModifiable: true, Min: 1, Max: 64, Default: "4",
		Description: "Threads InnoDB uses for read prefetch.",
	},
	ParameterSpec{
		Name: "innodb_write_io_threads", DataType: ParamTypeInteger, ApplyType: ApplyTypeStatic,
		IsModifiable: true, Min: 1, Max: 64, Default: "4",
		Description: "Threads InnoDB uses to write from the buffer pool.",
	},
	ParameterSpec{
		Name: "innodb_purge_threads", DataType: ParamTypeInteger, ApplyType: ApplyTypeStatic,
		IsModifiable: true, Min: 1, Max: 32, Default: "4",
		Description: "Threads InnoDB uses to purge the undo history.",
	},
	ParameterSpec{
		Name: "innodb_autoinc_lock_mode", DataType: ParamTypeInteger, ApplyType: ApplyTypeStatic,
		IsModifiable: true, Min: 0, Max: 2, Default: "1",
		Description: "Locking mode for AUTO_INCREMENT allocation; 1 is consecutive.",
	},
	ParameterSpec{
		Name: "innodb_io_capacity", DataType: ParamTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 100, Max: 4294967295, Default: "200",
		Description: "I/O operations per second InnoDB assumes it may use for background work.",
	},
	ParameterSpec{
		Name: "innodb_io_capacity_max", DataType: ParamTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 100, Max: 4294967295, Default: "2000",
		Description: "I/O operations per second InnoDB may use when it is falling behind.",
	},
	ParameterSpec{
		Name: "innodb_max_dirty_pages_pct", DataType: ParamTypeReal, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 0, Max: 99.999, Default: "90",
		Description: "Percentage of dirty pages in the buffer pool InnoDB flushes to stay under.",
	},
	ParameterSpec{
		Name: "innodb_flush_neighbors", DataType: ParamTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 0, Max: 2, Default: "1",
		Description: "Whether flushing a page also flushes its neighbours in the same extent.",
	},
	ParameterSpec{
		Name: "innodb_file_per_table", DataType: ParamTypeBoolean, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Default: "on",
		Description: "Whether each InnoDB table gets its own tablespace file.",
	},
	ParameterSpec{
		Name: "innodb_stats_persistent", DataType: ParamTypeBoolean, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Default: "on",
		Description: "Whether InnoDB optimizer statistics survive a restart.",
	},
	ParameterSpec{
		Name: "innodb_strict_mode", DataType: ParamTypeBoolean, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Default: "on",
		Description: "Whether InnoDB raises errors rather than warnings for invalid table options.",
	},
	ParameterSpec{
		Name: "innodb_adaptive_hash_index", DataType: ParamTypeBoolean, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Default: "off",
		Description: "Whether InnoDB maintains an in-memory hash index over frequently read pages.",
	},

	// Server memory outside InnoDB. Every per-session buffer here is allocated
	// per connection, so a literal tuned for the large end multiplies by
	// max_connections on the small one.
	ParameterSpec{
		Name: "sort_buffer_size", DataType: ParamTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 1024, Max: 4294967295, Default: "2097152", Unit: "B",
		Description: "Per-session buffer for a sort that cannot use an index, in bytes.",
	},
	ParameterSpec{
		Name: "join_buffer_size", DataType: ParamTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 128, Max: 4294967295, Default: "262144", Unit: "B",
		Description: "Per-session buffer for a join that cannot use an index, in bytes.",
	},
	ParameterSpec{
		Name: "read_buffer_size", DataType: ParamTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 8192, Max: 2147479552, Default: "131072", Unit: "B",
		Description: "Per-session buffer for a sequential table scan, in bytes.",
	},
	ParameterSpec{
		Name: "read_rnd_buffer_size", DataType: ParamTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 8192, Max: 2147483647, Default: "262144", Unit: "B",
		Description: "Per-session buffer for reading rows in sorted order, in bytes.",
	},
	ParameterSpec{
		Name: "tmp_table_size", DataType: ParamTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 1024, Max: 4294967295, Default: "16777216", Unit: "B",
		Description: "Size an internal in-memory temporary table may reach before it is written to disk, in bytes.",
	},
	ParameterSpec{
		Name: "max_heap_table_size", DataType: ParamTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 16384, Max: 4294967295, Default: "16777216", Unit: "B",
		Description: "Maximum size of a user-created MEMORY table, in bytes.",
	},
	ParameterSpec{
		Name: "key_buffer_size", DataType: ParamTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 0, Max: 4294967295, Default: "134217728", Unit: "B",
		Description: "Buffer for MyISAM and Aria index blocks, in bytes.",
	},
	ParameterSpec{
		Name: "table_open_cache", DataType: ParamTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 1, Max: 1048576, Default: "2000",
		Description: "Open table handles the server caches across all connections.",
	},
	ParameterSpec{
		Name: "table_definition_cache", DataType: ParamTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 400, Max: 2097152, Default: "400",
		Description: "Table definitions the server caches.",
	},
	ParameterSpec{
		Name: "max_prepared_stmt_count", DataType: ParamTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 0, Max: 1048576, Default: "16382",
		Description: "Prepared statements the server allows across all connections.",
	},
	ParameterSpec{
		Name: "group_concat_max_len", DataType: ParamTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 4, Max: 4294967295, Default: "1048576", Unit: "B",
		Description: "Maximum length of a GROUP_CONCAT result, in bytes.",
	},
	// Off by default as MariaDB itself ships it: the instrumentation costs memory
	// a small class does not have, and it cannot be turned on without a restart.
	ParameterSpec{
		Name: "performance_schema", DataType: ParamTypeBoolean, ApplyType: ApplyTypeStatic,
		IsModifiable: true, Default: "off",
		Description: "Whether the Performance Schema instrumentation is enabled.",
	},

	// Logging. The destination and the log files are platform-owned; what is
	// logged is the customer's.
	ParameterSpec{
		Name: "slow_query_log", DataType: ParamTypeBoolean, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Default: "off",
		Description: "Whether statements slower than long_query_time are logged.",
	},
	ParameterSpec{
		Name: "long_query_time", DataType: ParamTypeReal, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 0, Max: 31536000, Default: "10", Unit: "s",
		Description: "Seconds a statement must run to reach the slow query log.",
	},
	ParameterSpec{
		Name: "log_queries_not_using_indexes", DataType: ParamTypeBoolean, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Default: "off",
		Description: "Whether a statement that examines no index reaches the slow query log.",
	},
	ParameterSpec{
		Name: "general_log", DataType: ParamTypeBoolean, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Default: "off",
		Description: "Whether every connection and statement is logged.",
	},
	ParameterSpec{
		Name: "log_output", DataType: ParamTypeEnum, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Enum: []string{"file", "table", "none"}, Default: "file",
		Description: "Where the general and slow query logs are written.",
	},
	ParameterSpec{
		Name: "log_warnings", DataType: ParamTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 0, Max: 4294967295, Default: "2",
		Description: "Verbosity of the error log; 0 logs errors only.",
	},

	// SQL behaviour.
	// The default omits NO_AUTO_CREATE_USER, which MariaDB is retiring: the
	// platform default must parse on every supported patch release.
	ParameterSpec{
		Name: "sql_mode", DataType: ParamTypeString, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Validate: validateMariaDBSQLMode,
		Default:     "STRICT_TRANS_TABLES,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION",
		Description: "Comma-separated SQL modes the server enforces.",
	},
	ParameterSpec{
		Name: "character_set_server", DataType: ParamTypeEnum, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Enum: []string{"latin1", "utf8mb3", "utf8mb4"}, Default: "utf8mb4",
		Description: "Default character set for new databases.",
	},
	ParameterSpec{
		Name: "collation_server", DataType: ParamTypeEnum, ApplyType: ApplyTypeDynamic,
		IsModifiable: true,
		Enum: []string{
			"latin1_bin", "latin1_swedish_ci",
			"utf8mb3_bin", "utf8mb3_general_ci",
			"utf8mb4_bin", "utf8mb4_general_ci", "utf8mb4_unicode_ci",
		},
		Default:     "utf8mb4_general_ci",
		Description: "Default collation for new databases, which must belong to character_set_server.",
	},
	// The image loads no time zone tables, so only SYSTEM and a fixed offset
	// apply. MariaDB has no time_zone startup option, so an option file naming it
	// aborts the server; default_time_zone is the spelling startup accepts.
	ParameterSpec{
		Name: "time_zone", DataType: ParamTypeString, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Default: "SYSTEM", Validate: validateMariaDBTimeZone,
		optionFileName: "default_time_zone",
		Description:    "Server time zone, as SYSTEM or a fixed offset such as +10:00.",
	},
	ParameterSpec{
		Name: "transaction_isolation", DataType: ParamTypeEnum, ApplyType: ApplyTypeDynamic,
		IsModifiable: true,
		Enum:         []string{"read-uncommitted", "read-committed", "repeatable-read", "serializable"},
		Default:      "repeatable-read",
		Description:  "Default transaction isolation level for new connections.",
	},
	ParameterSpec{
		Name: "autocommit", DataType: ParamTypeBoolean, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Default: "on",
		Description: "Whether each statement outside an explicit transaction commits on its own.",
	},
	ParameterSpec{
		Name: "event_scheduler", DataType: ParamTypeEnum, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Enum: []string{"on", "off"}, Default: "off",
		Description: "Whether the scheduled event executor thread runs.",
	},

	// TLS. Pinned to the same floor as the PostgreSQL engine and static because
	// mariadbd reads tls_version only at startup. ssl_cert and ssl_key stay absent:
	// they name files rds-init mints, so a customer setting them breaks the endpoint.
	ParameterSpec{
		Name: "tls_version", DataType: ParamTypeString, ApplyType: ApplyTypeStatic,
		IsModifiable: false, Default: "TLSv1.3",
		Description: "TLS protocol versions the server accepts, as a comma-separated list.",
	},
	// A real global system variable, unlike PostgreSQL's placeholder: the value in
	// the file is the enforcement, and SET GLOBAL applies it live. A unix socket
	// counts as a secure transport, so neither rds-init nor rds-agent is affected.
	ParameterSpec{
		Name: "require_secure_transport", DataType: ParamTypeBoolean, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Default: "0",
		Description: "Whether the server requires TLS of client connections over TCP.",
	},
))

// MariaDB parses 0, 1, ON, OFF, TRUE and FALSE for a boolean system variable and
// refuses yes and no, which PostgreSQL accepts. Narrowing the whole catalog in
// one place keeps every boolean spec free of the distinction.
var mariadbBooleanSpellings = []string{"on", "off", "true", "false", "1", "0"}

func withMariaDBBooleanSpellings(catalog map[string]ParameterSpec) map[string]ParameterSpec {
	for name, spec := range catalog {
		if spec.DataType == ParamTypeBoolean {
			spec.Enum = mariadbBooleanSpellings
			catalog[name] = spec
		}
	}
	return catalog
}

// The InnoDB buffer pool ceiling, above the three-quarters default but below the
// whole guest: the pool is a single allocation the kernel will not overcommit
// its way out of, so a value at class memory is a server that never starts.
const bufferPoolCeilingNumerator, bufferPoolCeilingDenominator = 7, 8

// innodb_buffer_pool_size = {DBInstanceClassMemory*3/4}, in bytes, which is
// RDS's own default for this engine family.
func innodbBufferPoolSizeFor(memoryMiB int64) string {
	return strconv.FormatInt(clampInt64(memoryMiB*mibToBytes/4*3, minBufferPoolBytes, maxBufferPoolBytes), 10)
}

func innodbBufferPoolSizeCeilingFor(memoryMiB int64) int64 {
	ceiling := memoryMiB * mibToBytes / bufferPoolCeilingDenominator * bufferPoolCeilingNumerator
	return clampInt64(ceiling, minBufferPoolBytes, maxBufferPoolBytes)
}

const (
	minBufferPoolBytes = 5242880
	maxBufferPoolBytes = 1099511627776
)

// max_connections = {DBInstanceClassMemory/12582880}. The floor keeps the
// smallest class above what the platform's own health probing, parameter apply
// and password rotation sessions need before a customer connects at all.
func mariadbMaxConnectionsFor(memoryMiB int64) string {
	return strconv.FormatInt(mariadbMaxConnections(memoryMiB), 10)
}

func mariadbMaxConnectionsCeilingFor(memoryMiB int64) int64 {
	return clampInt64(mariadbMaxConnections(memoryMiB)*4, 20, 100000)
}

func mariadbMaxConnections(memoryMiB int64) int64 {
	return clampInt64(memoryMiB*mibToBytes/12582880, 20, 100000)
}

// The modes MariaDB 11.8 parses, including the combination modes that expand to
// several. A mode the server does not know is a startup failure, so the list is
// closed rather than a syntax check.
var mariadbSQLModes = []string{
	"allow_invalid_dates", "ansi", "ansi_quotes", "db2", "empty_string_is_null",
	"error_for_division_by_zero", "high_not_precedence", "ignore_bad_table_options",
	"ignore_space", "maxdb", "mssql", "mysql323", "mysql40", "no_auto_value_on_zero",
	"no_backslash_escapes", "no_dir_in_create", "no_engine_substitution",
	"no_unsigned_subtraction", "no_zero_date", "no_zero_in_date", "only_full_group_by",
	"oracle", "pad_char_to_full_length", "pipes_as_concat", "postgresql", "real_as_float",
	"simultaneous_assignment", "strict_all_tables", "strict_trans_tables",
	"time_round_fractional", "traditional",
}

func validateMariaDBSQLMode(value string) error {
	for mode := range strings.SplitSeq(value, ",") {
		mode = strings.ToLower(strings.TrimSpace(mode))
		if mode == "" || !slices.Contains(mariadbSQLModes, mode) {
			return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
				"parameter sql_mode does not accept %q; it takes a comma-separated list of MariaDB SQL mode names", value)
		}
	}
	return nil
}

// SYSTEM, or a signed offset between -14:00 and +14:00. A named zone is refused
// because the image loads no time zone tables for the server to resolve it from.
func validateMariaDBTimeZone(value string) error {
	if strings.EqualFold(value, "SYSTEM") {
		return nil
	}
	if offset, err := parseTimeZoneOffset(value); err == nil {
		const maxOffsetMinutes = 14 * 60
		if offset >= -maxOffsetMinutes && offset <= maxOffsetMinutes {
			return nil
		}
	}
	return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
		"parameter time_zone does not accept %q; use SYSTEM or a fixed offset between -14:00 and +14:00", value)
}

func parseTimeZoneOffset(value string) (int, error) {
	parsed, err := time.Parse("-07:00", value)
	if err != nil {
		return 0, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"offset must use signed HH:MM syntax: %v", err)
	}
	_, offsetSeconds := parsed.Zone()
	return offsetSeconds / int(time.Minute/time.Second), nil
}

func validateMariaDBParameterCombinations(params []Parameter) error {
	values := resolvedValues(params)

	// A collation outside the server's character set is one of the few settings
	// mysqld refuses to start under rather than adjusting.
	charset, err := resolvedString(values, "character_set_server")
	if err != nil {
		return err
	}
	collation, err := resolvedString(values, "collation_server")
	if err != nil {
		return err
	}
	if !strings.HasPrefix(collation, charset+"_") {
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"collation_server %q does not belong to character_set_server %q", collation, charset)
	}

	// Left inconsistent, the server silently raises the maximum to the steady
	// rate, so the value a customer set is not the one the engine runs.
	ioCapacity, err := resolvedInteger(values, "innodb_io_capacity")
	if err != nil {
		return err
	}
	ioCapacityMax, err := resolvedInteger(values, "innodb_io_capacity_max")
	if err != nil {
		return err
	}
	if ioCapacityMax < ioCapacity {
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"innodb_io_capacity_max must be at least innodb_io_capacity")
	}
	return nil
}
