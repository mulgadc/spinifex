package handlers_rds

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/instancetypes"
)

// The static PostgreSQL 18 parameter catalog, following the EKS add-on catalog
// pattern: a curated in-binary table rather than the engine's full ~350 GUCs.
// A curated subset that is genuinely validated is worth more than the whole set
// passed through unchecked — a static parameter the engine refuses at startup
// is a boot loop, and the bad config is on the persistent data volume.

// When a parameter takes effect. Static settings are stored and reported
// pending-reboot; dynamic ones are adopted by a reload.
const (
	ApplyTypeStatic  = "static"
	ApplyTypeDynamic = "dynamic"
)

// AWS's own ApplyMethod values, echoed back on DescribeDBParameters.
const (
	ApplyMethodImmediate     = "immediate"
	ApplyMethodPendingReboot = "pending-reboot"
)

// Where a reported value came from. AWS distinguishes these and the Terraform
// provider reads them, so a computed default must not be reported as user.
const (
	ParameterSourceUser          = "user"
	ParameterSourceEngineDefault = "engine-default"
)

// The parameter data types the catalog offers. Every one is validated on input,
// so a value the API accepts is one the engine will parse.
const (
	paramTypeInteger = "integer"
	paramTypeReal    = "real"
	paramTypeBoolean = "boolean"
	paramTypeString  = "string"
	paramTypeEnum    = "enum"
)

// One catalog entry. Exactly one of Default and DefaultFor is set: a literal for
// the parameters whose engine default is size-independent, and a formula over
// the instance class's memory for the ones that are not.
type ParameterSpec struct {
	Name        string
	DataType    string
	ApplyType   string
	Description string
	// False for the handful of settings the platform owns — changing them would
	// break the endpoint, the agent's socket access or the serving certificate.
	IsModifiable bool

	Default string
	// Evaluated against the class's memory. The result is a literal, so the
	// customer-facing API never sees a formula.
	DefaultFor func(memoryMiB int64) string

	// Inclusive bounds for integer and real parameters. Both zero means
	// unbounded below and above.
	Min, Max float64
	// The permitted values of an enum parameter, lowest-to-highest where the
	// engine gives them an order.
	Enum []string
	// The engine's own unit suffix, reported in AllowedValues so a customer can
	// see what an integer means. Empty for unitless settings.
	Unit string
}

// The catalog, keyed by parameter name. Every entry here is one a customer has a
// real reason to tune; the platform-owned settings (port, listen_addresses,
// ssl*, unix_socket_directories, data_directory) are deliberately absent rather
// than present-and-unmodifiable, because they are not the customer's to set.
var parameterCatalog = buildParameterCatalog(
	// Connections. max_connections is what a size-derived default matters most
	// for: RDS's own formula is LEAST({DBInstanceClassMemory/9531392}, 5000).
	ParameterSpec{
		Name: "max_connections", DataType: paramTypeInteger, ApplyType: ApplyTypeStatic,
		IsModifiable: true, Min: 6, Max: 5000,
		DefaultFor:  maxConnectionsFor,
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
		Description: "Shared memory buffers the server uses, in 8 kB blocks.",
	},
	ParameterSpec{
		Name: "effective_cache_size", DataType: paramTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 1, Max: 4194304, Unit: "8kB",
		DefaultFor:  effectiveCacheSizeFor,
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
	ParameterSpec{
		Name: "wal_compression", DataType: paramTypeEnum, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Enum: []string{"off", "pglz", "lz4", "zstd", "on"}, Default: "off",
		Description: "Compression method for full-page images written to the WAL.",
	},
	ParameterSpec{
		Name: "max_wal_size", DataType: paramTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 2, Max: 2097151, Default: "1024", Unit: "MB",
		Description: "WAL size that triggers a checkpoint, in MB.",
	},
	ParameterSpec{
		Name: "min_wal_size", DataType: paramTypeInteger, ApplyType: ApplyTypeDynamic,
		IsModifiable: true, Min: 2, Max: 2097151, Default: "80", Unit: "MB",
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
	ParameterSpec{
		Name: "autovacuum_max_workers", DataType: paramTypeInteger, ApplyType: ApplyTypeStatic,
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

func clampInt64(v, lo, hi int64) int64 {
	return min(max(v, lo), hi)
}

// Indexes the specs by name and fails the build-equivalent — process start — on
// a malformed entry, so a catalog typo cannot reach a customer's create.
func buildParameterCatalog(specs ...ParameterSpec) map[string]ParameterSpec {
	out := make(map[string]ParameterSpec, len(specs))
	for _, spec := range specs {
		switch {
		case spec.Name == "":
			panic("rds: parameter catalog holds an unnamed entry")
		case spec.Default == "" && spec.DefaultFor == nil:
			panic("rds: parameter catalog entry " + spec.Name + " has no default")
		case spec.Default != "" && spec.DefaultFor != nil:
			panic("rds: parameter catalog entry " + spec.Name + " has both a literal and a computed default")
		}
		if _, exists := out[spec.Name]; exists {
			panic("rds: parameter catalog entry " + spec.Name + " is duplicated")
		}
		out[spec.Name] = spec
	}
	return out
}

// The catalog entry for a parameter name, or false when the engine has no such
// setting or it is one this platform does not expose.
func LookupParameter(name string) (ParameterSpec, bool) {
	spec, ok := parameterCatalog[strings.ToLower(strings.TrimSpace(name))]
	return spec, ok
}

// Sorted, so a describe returns the same order on every call and Terraform does
// not read a reshuffle as drift.
func CatalogParameterNames() []string {
	return slices.Sorted(maps.Keys(parameterCatalog))
}

// The engine default for one parameter at one instance class: the literal, or
// the formula evaluated against the class's memory.
func (s ParameterSpec) DefaultAt(memoryMiB int64) string {
	if s.DefaultFor != nil {
		return s.DefaultFor(memoryMiB)
	}
	return s.Default
}

// The AllowedValues string AWS reports: a range for numerics, the alternatives
// for an enum or boolean. Empty for a free-form string, as AWS leaves it.
func (s ParameterSpec) AllowedValues() string {
	switch s.DataType {
	case paramTypeInteger, paramTypeReal:
		bounds := fmt.Sprintf("%s-%s", formatBound(s.Min), formatBound(s.Max))
		if s.Unit != "" {
			return bounds + " (" + s.Unit + ")"
		}
		return bounds
	case paramTypeEnum:
		return strings.Join(s.Enum, ",")
	case paramTypeBoolean:
		return "on,off,true,false,yes,no,1,0"
	default:
		return ""
	}
}

// Integral bounds print without a decimal point, so an integer parameter's range
// does not read as a real one's.
func formatBound(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// The memory an instance class's guest has, which every size-derived default is
// computed from. An unknown class is a validation failure upstream, so this is
// the last line rather than the check.
func classMemoryMiB(instanceClass string) (int64, error) {
	instanceType, err := InstanceTypeForClass(instanceClass)
	if err != nil {
		return 0, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"DBInstanceClass %q is not supported; supported classes are %s", instanceClass, strings.Join(SupportedInstanceClasses(), ", "))
	}
	memoryMiB, ok := instancetypes.DefaultMemoryMiB(instanceType)
	if !ok || memoryMiB <= 0 {
		return 0, awserrors.Errorf(awserrors.ErrorServerInternal,
			"no memory footprint is known for instance type %s", instanceType)
	}
	return memoryMiB, nil
}

// AWS accepts formulas like {DBInstanceClassMemory/32768} and references like
// DBInstanceClassMemory from a customer. This platform does not: the catalog
// validates literals, and a formula that reached the engine unvalidated would be
// a startup failure rather than an API error.
func rejectFormulaValue(name, value string) error {
	if !strings.ContainsAny(value, "{}") && !strings.Contains(value, "DBInstanceClass") {
		return nil
	}
	return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
		"the value %q of parameter %s is a formula; only literal values are accepted, "+
			"and the size-derived defaults are computed for you", value, name)
}

// Checks one customer-supplied value against its catalog entry. A parameter the
// catalog does not hold, one the platform owns, and one outside its own range
// are all rejected here — at the API, rather than by an engine that then refuses
// to start with the bad config already on the data volume.
func validateParameterValue(name, value string) (ParameterSpec, error) {
	spec, ok := LookupParameter(name)
	if !ok {
		return ParameterSpec{}, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"%q is not a parameter this engine exposes", name)
	}
	if !spec.IsModifiable {
		return ParameterSpec{}, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"parameter %s is not modifiable", spec.Name)
	}
	if err := rejectFormulaValue(spec.Name, value); err != nil {
		return ParameterSpec{}, err
	}

	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ParameterSpec{}, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"parameter %s was given an empty value", spec.Name)
	}
	switch spec.DataType {
	case paramTypeInteger:
		n, err := strconv.ParseInt(trimmed, 10, 64)
		if err != nil {
			return ParameterSpec{}, typeError(spec, value, "an integer")
		}
		if float64(n) < spec.Min || float64(n) > spec.Max {
			return ParameterSpec{}, rangeError(spec, value)
		}
	case paramTypeReal:
		f, err := strconv.ParseFloat(trimmed, 64)
		if err != nil {
			return ParameterSpec{}, typeError(spec, value, "a number")
		}
		if f < spec.Min || f > spec.Max {
			return ParameterSpec{}, rangeError(spec, value)
		}
	case paramTypeBoolean:
		if !validBoolean(trimmed) {
			return ParameterSpec{}, typeError(spec, value, "a boolean (on, off, true, false, yes, no, 1, 0)")
		}
	case paramTypeEnum:
		if !slices.Contains(spec.Enum, strings.ToLower(trimmed)) {
			return ParameterSpec{}, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
				"parameter %s does not accept %q; allowed values are %s", spec.Name, value, strings.Join(spec.Enum, ", "))
		}
	}
	return spec, nil
}

// PostgreSQL's own boolean spellings, so a config a customer copied from the
// engine's documentation is accepted as the engine would accept it.
func validBoolean(value string) bool {
	switch strings.ToLower(value) {
	case "on", "off", "true", "false", "yes", "no", "1", "0":
		return true
	default:
		return false
	}
}

func typeError(spec ParameterSpec, value, want string) error {
	return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
		"parameter %s takes %s, not %q", spec.Name, want, value)
}

func rangeError(spec ParameterSpec, value string) error {
	return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
		"the value %q of parameter %s is outside its allowed range %s", value, spec.Name, spec.AllowedValues())
}

// The full parameter set an instance runs with: every catalog default evaluated
// at the instance's class, overlaid with the group's stored overrides. The
// result is literals only, sorted by name so a re-resolve that changed nothing
// produces a byte-identical include.
//
// Overrides are re-validated rather than trusted: a catalog whose bounds
// tightened must not keep handing the engine a value it would now reject.
func ResolveEffectiveParameters(instanceClass string, overrides map[string]string) ([]Parameter, error) {
	memoryMiB, err := classMemoryMiB(instanceClass)
	if err != nil {
		return nil, err
	}
	names := CatalogParameterNames()
	resolved := make([]Parameter, 0, len(names))
	for _, name := range names {
		spec := parameterCatalog[name]
		value := spec.DefaultAt(memoryMiB)
		if override, ok := overrides[name]; ok {
			if _, err := validateParameterValue(name, override); err != nil {
				return nil, err
			}
			value = override
		}
		resolved = append(resolved, Parameter{Name: name, Value: value})
	}
	return resolved, nil
}
