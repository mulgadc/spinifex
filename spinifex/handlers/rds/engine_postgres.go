package handlers_rds

import (
	"strconv"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// PostgreSQL 18 is the pinned v1 major, matching the rds-postgres AMI preset.
// A new major is a new AMI plus a bump here, never a runtime upgrade.
var enginePostgres = Engine{
	Name:         "postgres",
	MajorVersion: "18",
	DefaultPort:  5432,
	// rdsadmin is the management role AWS reserves; postgres is the cluster
	// superuser initdb creates, which the master role must not collide with.
	// rds_superuser is the group role the bootstrap grants the master its
	// administrative privileges through.
	reservedUsernames:        []string{"rdsadmin", "postgres", "rds_superuser"},
	reservedUsernamePrefixes: []string{"pg_"},
	// NAMEDATALEN-1, where the engine's own limit and AWS's documented one
	// coincide. A database name is the same identifier limit.
	maxUsernameLen:       63,
	validateDBName:       dbNameRule(63),
	catalog:              postgresParameterCatalog,
	validateCombinations: validatePostgresParameterCombinations,
}

// The curated PostgreSQL 18 table: a genuinely validated subset rather than the
// engine's full ~350 GUCs, because a static parameter the engine refuses at
// startup is a boot loop with the bad config already on the data volume.
//
// The platform-owned settings (port, listen_addresses, ssl*,
// unix_socket_directories, data_directory) are deliberately absent rather than
// present-and-unmodifiable, because they are not the customer's to set.
var postgresParameterCatalog = buildParameterCatalog(
	// Connections. max_connections is what a size-derived default matters most
	// for: RDS's own formula is LEAST({DBInstanceClassMemory/9531392}, 5000).
	ParameterSpec{
		Name: "max_connections", DataType: paramTypeInteger, ApplyType: ApplyTypeStatic,
		IsModifiable: true, Min: 6, Max: 5000,
		DefaultFor:  maxConnectionsFor,
		MaxFor:      maxConnectionsCeilingFor,
		Description: "Maximum number of concurrent connections to the database server.",
	},
	ParameterSpec{
		Name: "superuser_reserved_connections", DataType: paramTypeInteger, ApplyType: ApplyTypeStatic,
		IsModifiable: true, Min: 0, Max: 100, Default: "3",
		Description: "Connection slots reserved for superusers.",
	},
	ParameterSpec{
		Name: "idle_in_transaction_session_timeout", DataType: paramTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 0, Max: 2147483647, Default: "0", Unit: "ms",
		Description: "Milliseconds an idle open transaction may live before it is aborted; 0 disables.",
	},
	ParameterSpec{
		Name: "statement_timeout", DataType: paramTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 0, Max: 2147483647, Default: "0", Unit: "ms",
		Description: "Milliseconds a statement may run before it is aborted; 0 disables.",
	},
	ParameterSpec{
		Name: "lock_timeout", DataType: paramTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 0, Max: 2147483647, Default: "0", Unit: "ms",
		Description: "Milliseconds to wait for a lock before failing the statement; 0 disables.",
	},
	ParameterSpec{
		Name: "tcp_keepalives_idle", DataType: paramTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 0, Max: 3600, Default: "0", Unit: "s",
		Description: "Idle seconds before the server sends a TCP keepalive; 0 takes the system default.",
	},

	// Memory. Every one of these is a fraction of class memory on real RDS, so a
	// literal tuned for the large end makes db.t3.micro fail to start.
	ParameterSpec{
		Name: "shared_buffers", DataType: paramTypeInteger, ApplyType: ApplyTypeStatic,
		IsModifiable: true, Min: 16, Max: 4194304, Unit: "8kB",
		DefaultFor:  sharedBuffersFor,
		MaxFor:      sharedBuffersCeilingFor,
		Description: "Shared memory buffers the server uses, in 8 kB blocks.",
	},
	ParameterSpec{
		Name: "effective_cache_size", DataType: paramTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 1, Max: 4194304, Unit: "8kB",
		DefaultFor:  effectiveCacheSizeFor,
		MaxFor:      effectiveCacheSizeCeilingFor,
		Description: "Planner assumption about the disk cache available to one query, in 8 kB blocks.",
	},
	// Per sort or hash rather than per connection, so RDS leaves it a literal
	// even in the memory group; a size-derived default here would multiply by
	// max_connections and by the parallel workers under each of them.
	ParameterSpec{
		Name: "work_mem", DataType: paramTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 64, Max: 2147483647, Default: "4096", Unit: "kB",
		Description: "Memory one sort or hash operation may use before spilling to disk, in kB.",
	},
	ParameterSpec{
		Name: "maintenance_work_mem", DataType: paramTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 1024, Max: 2147483647, Unit: "kB",
		DefaultFor:  maintenanceWorkMemFor,
		MaxFor:      maintenanceWorkMemCeilingFor,
		Description: "Memory a VACUUM, CREATE INDEX or ALTER TABLE ADD FOREIGN KEY may use, in kB.",
	},
	ParameterSpec{
		Name: "temp_buffers", DataType: paramTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 100, Max: 1073741823, Default: "1024", Unit: "8kB",
		Description: "Per-session buffers for temporary tables, in 8 kB blocks.",
	},
	ParameterSpec{
		Name: "max_prepared_transactions", DataType: paramTypeInteger, ApplyType: ApplyTypeStatic,
		IsModifiable: true, Min: 0, Max: 262143, Default: "0",
		Description: "Maximum simultaneously prepared transactions; 0 disables two-phase commit.",
	},

	// Parallelism. Bounded by the class's vCPU count on a real deployment, but
	// PostgreSQL degrades gracefully when they exceed it, so literals are honest.
	ParameterSpec{
		Name: "max_worker_processes", DataType: paramTypeInteger, ApplyType: ApplyTypeStatic,
		IsModifiable: true, Min: 0, Max: 262143, Default: "8",
		Description: "Maximum background processes the server may start.",
	},
	ParameterSpec{
		Name: "max_parallel_workers", DataType: paramTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 0, Max: 1024, Default: "8",
		Description: "Maximum workers the server may use for parallel operations.",
	},
	ParameterSpec{
		Name: "max_parallel_workers_per_gather", DataType: paramTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 0, Max: 1024, Default: "2",
		Description: "Maximum parallel workers one Gather node may start.",
	},

	// WAL and checkpoints.
	ParameterSpec{
		Name: "wal_level", DataType: paramTypeEnum, ApplyType: ApplyTypeStatic,
		IsModifiable: true, Enum: []string{"minimal", "replica", "logical"}, Default: "replica",
		Description: "How much information is written to the WAL.",
	},
	ParameterSpec{
		Name: "synchronous_commit", DataType: paramTypeEnum, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Enum: []string{"off", "local", "remote_write", "on", "remote_apply"}, Default: "on",
		Description: "How much WAL processing must complete before a commit returns.",
	},
	// Alpine's PostgreSQL 18 package is built with both optional compression
	// libraries, so every engine method is safe to expose.
	ParameterSpec{
		Name: "wal_compression", DataType: paramTypeEnum, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Enum: []string{"off", "pglz", "lz4", "zstd", "on"}, Default: "off",
		Description: "Compression method for full-page images written to the WAL.",
	},
	// The image uses PostgreSQL's 16 MiB WAL segments; both size settings must
	// hold at least two segments or startup rejects the configuration.
	ParameterSpec{
		Name: "max_wal_size", DataType: paramTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 32, Max: 2097151, Default: "1024", Unit: "MB",
		Description: "WAL size that triggers a checkpoint, in MB.",
	},
	ParameterSpec{
		Name: "min_wal_size", DataType: paramTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 32, Max: 2097151, Default: "80", Unit: "MB",
		Description: "WAL size below which old segments are recycled rather than removed, in MB.",
	},
	ParameterSpec{
		Name: "checkpoint_timeout", DataType: paramTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 30, Max: 86400, Default: "300", Unit: "s",
		Description: "Maximum seconds between automatic WAL checkpoints.",
	},
	ParameterSpec{
		Name: "checkpoint_completion_target", DataType: paramTypeReal, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 0, Max: 1, Default: "0.9",
		Description: "Fraction of the checkpoint interval a checkpoint's writes are spread over.",
	},
	ParameterSpec{
		Name: "wal_buffers", DataType: paramTypeInteger, ApplyType: ApplyTypeStatic,
		IsModifiable: true, Min: -1, Max: 262143, Default: "-1", Unit: "8kB",
		Description: "Shared memory used for WAL not yet written, in 8 kB blocks; -1 derives it from shared_buffers.",
	},
	ParameterSpec{
		Name: "max_wal_senders", DataType: paramTypeInteger, ApplyType: ApplyTypeStatic,
		IsModifiable: true, Min: 0, Max: 262143, Default: "10",
		Description: "Maximum simultaneously running WAL sender processes.",
	},
	ParameterSpec{
		Name: "max_replication_slots", DataType: paramTypeInteger, ApplyType: ApplyTypeStatic,
		IsModifiable: true, Min: 0, Max: 262143, Default: "10",
		Description: "Maximum replication slots the server may define.",
	},

	// Autovacuum. Turning it off is the single most reliable way to take a
	// PostgreSQL database down slowly, so it is offered but its default is on.
	ParameterSpec{
		Name: "autovacuum", DataType: paramTypeBoolean, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Default: "on",
		Description: "Whether the autovacuum launcher runs.",
	},
	// PostgreSQL 18 moved this setting to SIGHUP; worker slot allocation is now
	// controlled separately by the postmaster-only autovacuum_worker_slots.
	ParameterSpec{
		Name: "autovacuum_max_workers", DataType: paramTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 1, Max: 262143, Default: "3",
		Description: "Maximum autovacuum worker processes running at once.",
	},
	ParameterSpec{
		Name: "autovacuum_naptime", DataType: paramTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 1, Max: 2147483, Default: "60", Unit: "s",
		Description: "Seconds between autovacuum runs on any one database.",
	},
	ParameterSpec{
		Name: "autovacuum_vacuum_threshold", DataType: paramTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 0, Max: 2147483647, Default: "50",
		Description: "Row updates or deletes before a table is vacuumed.",
	},
	ParameterSpec{
		Name: "autovacuum_vacuum_scale_factor", DataType: paramTypeReal, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 0, Max: 100, Default: "0.2",
		Description: "Fraction of the table size added to autovacuum_vacuum_threshold.",
	},
	ParameterSpec{
		Name: "autovacuum_analyze_threshold", DataType: paramTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 0, Max: 2147483647, Default: "50",
		Description: "Row inserts, updates or deletes before a table is analyzed.",
	},
	ParameterSpec{
		Name: "autovacuum_analyze_scale_factor", DataType: paramTypeReal, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 0, Max: 100, Default: "0.1",
		Description: "Fraction of the table size added to autovacuum_analyze_threshold.",
	},
	ParameterSpec{
		Name: "autovacuum_vacuum_cost_limit", DataType: paramTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: -1, Max: 10000, Default: "-1",
		Description: "Cost budget one autovacuum worker spends before sleeping; -1 takes vacuum_cost_limit.",
	},

	// Planner.
	// The engine's own ceiling on a planner cost is DBL_MAX, which is not a bound
	// worth reporting: a cost past four digits already makes the plan choice
	// insensitive to it, so the range stays one a customer can read.
	ParameterSpec{
		Name: "random_page_cost", DataType: paramTypeReal, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 0, Max: 10000, Default: "1.1",
		Description: "Planner estimate of the cost of a non-sequentially fetched page.",
	},
	ParameterSpec{
		Name: "seq_page_cost", DataType: paramTypeReal, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 0, Max: 10000, Default: "1",
		Description: "Planner estimate of the cost of a sequentially fetched page.",
	},
	ParameterSpec{
		Name: "effective_io_concurrency", DataType: paramTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 0, Max: 1000, Default: "16",
		Description: "Concurrent disk I/O operations the planner assumes are useful.",
	},
	ParameterSpec{
		Name: "default_statistics_target", DataType: paramTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 1, Max: 10000, Default: "100",
		Description: "Default statistics target for table columns without one of their own.",
	},
	ParameterSpec{
		Name: "jit", DataType: paramTypeBoolean, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Default: "on",
		Description: "Whether JIT compilation may be used for qualifying queries.",
	},

	// Logging. The destination and the collector are platform-owned; what is
	// logged is the customer's.
	ParameterSpec{
		Name: "log_min_duration_statement", DataType: paramTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: -1, Max: 2147483647, Default: "-1", Unit: "ms",
		Description: "Milliseconds a statement must run to be logged; 0 logs every statement, -1 disables.",
	},
	ParameterSpec{
		Name: "log_statement", DataType: paramTypeEnum, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Enum: []string{"none", "ddl", "mod", "all"}, Default: "none",
		Description: "Which SQL statements are written to the log.",
	},
	ParameterSpec{
		Name: "log_min_messages", DataType: paramTypeEnum, ApplyType: ApplyTypeDynamic,
		IsModifiable: true,
		Enum: []string{"debug5", "debug4", "debug3", "debug2", "debug1", "info", "notice",
			"warning", "error", "log", "fatal", "panic"},
		Default:     "warning",
		Description: "Lowest message severity written to the server log.",
	},
	ParameterSpec{
		Name: "log_connections", DataType: paramTypeBoolean, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Default: "off",
		Description: "Whether each successful connection attempt is logged.",
	},
	ParameterSpec{
		Name: "log_disconnections", DataType: paramTypeBoolean, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Default: "off",
		Description: "Whether the end of each session is logged, with its duration.",
	},
	ParameterSpec{
		Name: "log_lock_waits", DataType: paramTypeBoolean, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Default: "on",
		Description: "Whether a session waiting longer than deadlock_timeout for a lock is logged.",
	},
	ParameterSpec{
		Name: "log_temp_files", DataType: paramTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: -1, Max: 2147483647, Default: "-1", Unit: "kB",
		Description: "Size in kB above which a temporary file's removal is logged; 0 logs all, -1 disables.",
	},
	ParameterSpec{
		Name: "log_autovacuum_min_duration", DataType: paramTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: -1, Max: 2147483647, Default: "-1", Unit: "ms",
		Description: "Milliseconds an autovacuum action must run to be logged; -1 disables.",
	},

	// Timeouts and locale.
	ParameterSpec{
		Name: "deadlock_timeout", DataType: paramTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 1, Max: 2147483647, Default: "1000", Unit: "ms",
		Description: "Milliseconds to wait on a lock before checking for a deadlock.",
	},
	ParameterSpec{
		Name: "timezone", DataType: paramTypeString, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Default: "UTC",
		Description: "Time zone the server displays and interprets timestamps in.",
	},
	ParameterSpec{
		Name: "datestyle", DataType: paramTypeString, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Default: "ISO, MDY",
		Description: "Display format for date and time values.",
	},
	ParameterSpec{
		Name: "track_activity_query_size", DataType: paramTypeInteger, ApplyType: ApplyTypeStatic,
		IsModifiable: true, Min: 100, Max: 1048576, Default: "1024", Unit: "B",
		Description: "Bytes of each running query pg_stat_activity retains.",
	},
	ParameterSpec{
		Name: "track_io_timing", DataType: paramTypeBoolean, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Default: "on",
		Description: "Whether block read and write times are collected.",
	},
)

// The bytes of class memory an RDS parameter formula divides. Real RDS exposes
// it as {DBInstanceClassMemory}, and the resolver evaluates the formulas here
// rather than passing them to the engine.
const mibToBytes = 1024 * 1024

// shared_buffers = {DBInstanceClassMemory/32768}, in 8 kB blocks — a quarter of
// class memory, which is RDS's own default.
func sharedBuffersFor(memoryMiB int64) string {
	return strconv.FormatInt(clampInt64(memoryMiB*mibToBytes/32768, 16, 4194304), 10)
}

// effective_cache_size = {DBInstanceClassMemory/16384}, in 8 kB blocks — half of
// class memory, the planner's assumption about what the OS is caching.
func effectiveCacheSizeFor(memoryMiB int64) string {
	return strconv.FormatInt(clampInt64(memoryMiB*mibToBytes/16384, 1, 4194304), 10)
}

// max_connections = LEAST({DBInstanceClassMemory/9531392}, 5000). The floor is
// what keeps db.t3.micro above superuser_reserved_connections plus the workers
// autovacuum and replication need, which is where the smallest class would
// otherwise fail to start.
func maxConnectionsFor(memoryMiB int64) string {
	return strconv.FormatInt(clampInt64(memoryMiB*mibToBytes/9531392, 20, 5000), 10)
}

// maintenance_work_mem = {DBInstanceClassMemory/63963136}, in kB. Capped so a
// large class does not let autovacuum_max_workers reserve the whole machine.
func maintenanceWorkMemFor(memoryMiB int64) string {
	return strconv.FormatInt(clampInt64(memoryMiB*mibToBytes/63963136*1024, 16384, 2097152), 10)
}

// Class ceilings deliberately leave substantial headroom above the defaults.
// They prevent obviously impossible large-class literals without prescribing
// production tuning for values the guest can support.
func sharedBuffersCeilingFor(memoryMiB int64) int64 {
	return clampInt64(memoryMiB*1024/8*3/4, 16, 4194304)
}

func effectiveCacheSizeCeilingFor(memoryMiB int64) int64 {
	return clampInt64(memoryMiB*1024/8, 1, 4194304)
}

func maxConnectionsCeilingFor(memoryMiB int64) int64 {
	defaults := clampInt64(memoryMiB*mibToBytes/9531392, 20, 5000)
	return clampInt64(defaults*4, 20, 5000)
}

func maintenanceWorkMemCeilingFor(memoryMiB int64) int64 {
	return clampInt64(memoryMiB*1024/2, 16384, 2147483647)
}

func clampInt64(v, lo, hi int64) int64 {
	return min(max(v, lo), hi)
}

func validatePostgresParameterCombinations(params []Parameter) error {
	values := make(map[string]string, len(params))
	for _, param := range params {
		values[param.Name] = strings.ToLower(strings.TrimSpace(param.Value))
	}

	maxWALSenders, err := resolvedInteger(values, "max_wal_senders")
	if err != nil {
		return err
	}
	maxReplicationSlots, err := resolvedInteger(values, "max_replication_slots")
	if err != nil {
		return err
	}
	if values["wal_level"] == "minimal" && (maxWALSenders != 0 || maxReplicationSlots != 0) {
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"wal_level minimal requires max_wal_senders and max_replication_slots both to be 0")
	}
	maxConnections, err := resolvedInteger(values, "max_connections")
	if err != nil {
		return err
	}
	reservedConnections, err := resolvedInteger(values, "superuser_reserved_connections")
	if err != nil {
		return err
	}
	if reservedConnections >= maxConnections {
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"superuser_reserved_connections must be less than max_connections")
	}
	maxWorkerProcesses, err := resolvedInteger(values, "max_worker_processes")
	if err != nil {
		return err
	}
	const (
		postgres18MaxBackends           = 262143
		postgres18AutovacuumWorkerSlots = 16
		postgres18SpecialWorkerProcs    = 2
	)
	backends := maxConnections + postgres18AutovacuumWorkerSlots + maxWorkerProcesses + maxWALSenders + postgres18SpecialWorkerProcs
	if backends > postgres18MaxBackends {
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"max_connections, max_worker_processes and max_wal_senders reserve too many server processes")
	}
	return nil
}

func resolvedInteger(values map[string]string, name string) (int64, error) {
	value, ok := values[name]
	if !ok {
		return 0, awserrors.Errorf(awserrors.ErrorServerInternal,
			"resolved parameter set is missing %s", name)
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, awserrors.Errorf(awserrors.ErrorServerInternal,
			"resolved parameter %s has invalid integer value %q", name, value)
	}
	return n, nil
}
