package handlers_rds

//test:in-package — pins present behaviour of the engine catalogue (engine.go,
// engine_postgres.go, engine_mariadb.go, paramcatalog.go, sizing.go, catalog.go)
// ahead of a package split, including unexported seams no exported API reaches.

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// engineForFamily and normaliseFamily fold case and trim surrounding
// whitespace before the registry lookup, exactly as LookupEngine does for an
// engine name.
func TestEngineForFamily_NormalisesCaseAndWhitespace(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "postgres18", normaliseFamily(" POSTGRES18 "))
	assert.Equal(t, "mariadb11.8", normaliseFamily("MariaDB11.8"))

	byCase, err := engineForFamily(" POSTGRES18 ")
	require.NoError(t, err)
	assert.Equal(t, enginePostgres.Name, byCase.Name)

	byMixedCase, err := engineForFamily("MariaDB11.8")
	require.NoError(t, err)
	assert.Equal(t, engineMariaDB.Name, byMixedCase.Name)
}

func TestEngineForFamily_RejectsUnknownFamily(t *testing.T) {
	t.Parallel()
	_, err := engineForFamily("mysql8")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(err),
		"the code has to survive resolution or the client sees a 500")
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

// ValidateMasterUserPassword's three rejections are built two different ways
// today: the length and character checks go through awserrors.Errorf, whose
// wrapped code resolves through ValidErrorCodeFromError; the empty-password
// check is a bare errors.New with the code only prefixed into the message
// text, which ValidErrorCodeFromError's exact-match lookup does not resolve.
// This pins that divergence rather than papering over it.
func TestValidateMasterUserPassword_ExactCodesAndMessages(t *testing.T) {
	t.Parallel()

	empty := ValidateMasterUserPassword("")
	require.Error(t, empty)
	assert.Equal(t, "InvalidParameterValue: MasterUserPassword is required", empty.Error())
	assert.Equal(t, awserrors.ErrorServerInternal, awserrors.ValidErrorCodeFromError(empty),
		"today this does NOT resolve to InvalidParameterValue: the code is only prefixed into the "+
			"message text, not carried as a wrapped codedError, so the helper's exact-match lookup misses it")

	tooShort := ValidateMasterUserPassword("short1")
	require.Error(t, tooShort)
	assert.Equal(t, "MasterUserPassword must be between 8 and 128 characters: InvalidParameterValue", tooShort.Error())
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(tooShort),
		"this one is built with awserrors.Errorf, so it resolves correctly")

	tooLong := ValidateMasterUserPassword(repeatByte('x', maxMasterPasswordLen+1))
	require.Error(t, tooLong)
	assert.Equal(t, "MasterUserPassword must be between 8 and 128 characters: InvalidParameterValue", tooLong.Error())
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, awserrors.ValidErrorCodeFromError(tooLong))
}

func repeatByte(b byte, n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return string(out)
}

// catalogAgentContract is one row of the agent-contract metadata a catalog
// parameter carries: the name the option file is written under, its data type
// and its apply semantics. Hand-transcribed from engine_postgres.go and
// engine_mariadb.go, not derived from the catalog at test time.
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

// ResolveEffectiveParameters checks validateCombinations before anything else,
// so an engine that registers none fails with ServerInternal rather than
// resolving a parameter set it cannot then validate.
func TestResolveEffectiveParameters_NoCombinationCheckIsServerInternal(t *testing.T) {
	t.Parallel()
	bare := Engine{Name: "bare-engine"}
	_, err := bare.ResolveEffectiveParameters(SmallestInstanceClass(), nil)
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
		validateCombinations: func(params []Parameter) error {
			values := resolvedValues(params)
			_, err := resolvedInteger(values, "bar")
			return err
		},
	}
	_, err := broken.ResolveEffectiveParameters(SmallestInstanceClass(), nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, awserrors.ValidErrorCodeFromError(err))
	assert.Contains(t, err.Error(), "resolved parameter set is missing bar")
}

// The half of each recovery warning that depends on the engine, pinned
// verbatim rather than by substring: a wording change to either note should
// fail here even if it keeps every substring the existing Contains assertions
// check for.
func TestUncleanStopMessage_PinsExactTextPerEngine(t *testing.T) {
	t.Parallel()
	postgres := uncleanStopMessage(t.Context(), "postgres", "stopping the instance")
	assert.Equal(t,
		"The database engine could not be shut down cleanly before stopping the instance. "+
			"It will recover from its write-ahead log on the next start.",
		postgres)

	mariadb := uncleanStopMessage(t.Context(), "mariadb", "rebooting the instance")
	assert.Equal(t,
		"The database engine could not be shut down cleanly before rebooting the instance. "+
			"InnoDB tables will recover from the redo log on the next start; "+
			"non-transactional tables such as Aria and MyISAM may be left inconsistent.",
		mariadb)
}

func TestCrashConsistentSnapshotMessage_PinsExactTextPerEngine(t *testing.T) {
	t.Parallel()
	postgres := crashConsistentSnapshotMessage(t.Context(), "postgres")
	assert.Equal(t,
		"The database engine could not be quiesced before the snapshot; the snapshot is crash consistent. "+
			"It will recover from its write-ahead log when it is restored.",
		postgres)

	mariadb := crashConsistentSnapshotMessage(t.Context(), "mariadb")
	assert.Equal(t,
		"The database engine could not be quiesced before the snapshot; the snapshot is crash consistent. "+
			"InnoDB tables will recover from the redo log when it is restored; "+
			"non-transactional tables such as Aria and MyISAM may be left inconsistent.",
		mariadb)
}

// The literal DescribeDBParameters' size-derived defaults resolve against for
// the smallest class: db.t3.micro's 1 GiB, stated here as the MiB classMemoryMiB
// actually returns rather than only the ordering sizing_test.go already checks.
func TestSmallestInstanceClass_MemoryIsExactlyOneGiB(t *testing.T) {
	t.Parallel()
	memoryMiB, err := classMemoryMiB(SmallestInstanceClass())
	require.NoError(t, err)
	assert.Equal(t, int64(1024), memoryMiB)
}
