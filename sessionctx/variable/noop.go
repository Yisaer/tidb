// Copyright 2020 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package variable

import (
	"math"
)

// The following sysVars are noops.
// Some applications will depend on certain variables to be present or settable,
// for example query_cache_time. These are included for MySQL compatibility,
// but changing them has no effect on behavior.

var noopSysVars = []*SysVar{
	// It is unsafe to pretend that any variation of "read only" is enabled when the server
	// does not support it. It is possible that these features will be supported in future,
	// but until then...
	{Scope: ScopeGlobal | ScopeSession, Name: TxReadOnly, Value: Off, Type: TypeBool, Aliases: []string{TransactionReadOnly}, Validation: func(vars *SessionVars, normalizedValue string, originalValue string, scope ScopeFlag) (string, error) {
		return checkReadOnly(vars, normalizedValue, originalValue, scope, false)
	}},
	{Scope: ScopeGlobal | ScopeSession, Name: TransactionReadOnly, Value: Off, Type: TypeBool, Aliases: []string{TxReadOnly}, Validation: func(vars *SessionVars, normalizedValue string, originalValue string, scope ScopeFlag) (string, error) {
		return checkReadOnly(vars, normalizedValue, originalValue, scope, false)
	}},
	{Scope: ScopeGlobal, Name: OfflineMode, Value: Off, Type: TypeBool, Validation: func(vars *SessionVars, normalizedValue string, originalValue string, scope ScopeFlag) (string, error) {
		return checkReadOnly(vars, normalizedValue, originalValue, scope, true)
	}},
	{Scope: ScopeGlobal, Name: SuperReadOnly, Value: Off, Type: TypeBool, Validation: func(vars *SessionVars, normalizedValue string, originalValue string, scope ScopeFlag) (string, error) {
		return checkReadOnly(vars, normalizedValue, originalValue, scope, false)
	}},
	{Scope: ScopeGlobal, Name: ReadOnly, Value: Off, Type: TypeBool, Validation: func(vars *SessionVars, normalizedValue string, originalValue string, scope ScopeFlag) (string, error) {
		return checkReadOnly(vars, normalizedValue, originalValue, scope, false)
	}},
	{Scope: ScopeGlobal, Name: ConnectTimeout, Value: "10", Type: TypeUnsigned, MinValue: 2, MaxValue: secondsPerYear, AutoConvertOutOfRange: true},
	{Scope: ScopeGlobal | ScopeSession, Name: QueryCacheWlockInvalidate, Value: Off, Type: TypeBool},
	{Scope: ScopeGlobal | ScopeSession, Name: "sql_buffer_result", Value: Off, IsHintUpdatable: true},
	{Scope: ScopeGlobal, Name: MyISAMUseMmap, Value: Off, Type: TypeBool, AutoConvertNegativeBool: true},
	{Scope: ScopeGlobal, Name: "gtid_mode", Value: Off, Type: TypeBool},
	{Scope: ScopeGlobal, Name: FlushTime, Value: "0", Type: TypeUnsigned, MinValue: 0, MaxValue: secondsPerYear, AutoConvertOutOfRange: true},
	{Scope: ScopeNone, Name: "performance_schema_max_mutex_classes", Value: "200"},
	{Scope: ScopeGlobal | ScopeSession, Name: LowPriorityUpdates, Value: Off, Type: TypeBool},
	{Scope: ScopeGlobal | ScopeSession, Name: SessionTrackGtids, Value: Off, Type: TypeEnum, PossibleValues: []string{Off, "OWN_GTID", "ALL_GTIDS"}},
	{Scope: ScopeGlobal | ScopeSession, Name: "ndbinfo_max_rows", Value: ""},
	{Scope: ScopeGlobal | ScopeSession, Name: "ndb_index_stat_option", Value: ""},
	{Scope: ScopeGlobal | ScopeSession, Name: OldPasswords, Value: "0", Type: TypeUnsigned, MinValue: 0, MaxValue: 2, AutoConvertOutOfRange: true},
	{Scope: ScopeNone, Name: "innodb_version", Value: "5.6.25"},
	{Scope: ScopeGlobal | ScopeSession, Name: BigTables, Value: Off, Type: TypeBool},
	{Scope: ScopeNone, Name: "skip_external_locking", Value: "1"},
	{Scope: ScopeNone, Name: "innodb_sync_array_size", Value: "1"},
	{Scope: ScopeSession, Name: "rand_seed2", Value: ""},
	{Scope: ScopeGlobal, Name: ValidatePasswordCheckUserName, Value: Off, Type: TypeBool},
	{Scope: ScopeGlobal, Name: ValidatePasswordNumberCount, Value: "1", Type: TypeUnsigned, MinValue: 0, MaxValue: math.MaxUint64, AutoConvertOutOfRange: true},
	{Scope: ScopeSession, Name: "gtid_next", Value: ""},
	{Scope: ScopeGlobal, Name: "ndb_show_foreign_key_mock_tables", Value: ""},
	{Scope: ScopeNone, Name: "multi_range_count", Value: "256"},
	{Scope: ScopeGlobal | ScopeSession, Name: "binlog_error_action", Value: "IGNORE_ERROR"},
	{Scope: ScopeGlobal | ScopeSession, Name: "default_storage_engine", Value: "InnoDB"},
	{Scope: ScopeNone, Name: "ft_query_expansion_limit", Value: "20"},
	{Scope: ScopeGlobal, Name: MaxConnectErrors, Value: "100", Type: TypeUnsigned, MinValue: 1, MaxValue: math.MaxUint64, AutoConvertOutOfRange: true},
	{Scope: ScopeGlobal, Name: SyncBinlog, Value: "0", Type: TypeUnsigned, MinValue: 0, MaxValue: 4294967295, AutoConvertOutOfRange: true},
	{Scope: ScopeNone, Name: "max_digest_length", Value: "1024"},
	{Scope: ScopeNone, Name: "innodb_force_load_corrupted", Value: "0"},
	{Scope: ScopeNone, Name: "performance_schema_max_table_handles", Value: "4000"},
	{Scope: ScopeGlobal, Name: InnodbFastShutdown, Value: "1", Type: TypeUnsigned, MinValue: 0, MaxValue: 2, AutoConvertOutOfRange: true},
	{Scope: ScopeNone, Name: "ft_max_word_len", Value: "84"},
	{Scope: ScopeGlobal, Name: "log_backward_compatible_user_definitions", Value: ""},
	{Scope: ScopeNone, Name: "lc_messages_dir", Value: "/usr/local/mysql-5.6.25-osx10.8-x86_64/share/"},
	{Scope: ScopeGlobal, Name: "ft_boolean_syntax", Value: "+ -><()~*:\"\"&|"},
	{Scope: ScopeGlobal, Name: TableDefinitionCache, Value: "2000", Type: TypeUnsigned, MinValue: 400, MaxValue: 524288, AutoConvertOutOfRange: true},
	{Scope: ScopeNone, Name: SkipNameResolve, Value: Off, Type: TypeBool},
	{Scope: ScopeNone, Name: "performance_schema_max_file_handles", Value: "32768"},
	{Scope: ScopeSession, Name: "transaction_allow_batching", Value: ""},
	{Scope: ScopeNone, Name: "performance_schema_max_statement_classes", Value: "168"},
	{Scope: ScopeGlobal, Name: "server_id", Value: "0"},
	{Scope: ScopeGlobal, Name: "innodb_flushing_avg_loops", Value: "30"},
	{Scope: ScopeGlobal, Name: "innodb_max_purge_lag", Value: "0"},
	{Scope: ScopeGlobal | ScopeSession, Name: "preload_buffer_size", Value: "32768"},
	{Scope: ScopeGlobal, Name: CheckProxyUsers, Value: Off, Type: TypeBool},
	{Scope: ScopeNone, Name: "have_query_cache", Value: "YES"},
	{Scope: ScopeGlobal, Name: "innodb_flush_log_at_timeout", Value: "1"},
	{Scope: ScopeGlobal, Name: "innodb_max_undo_log_size", Value: ""},
	{Scope: ScopeGlobal | ScopeSession, Name: "range_alloc_block_size", Value: "4096", IsHintUpdatable: true},
	{Scope: ScopeNone, Name: "have_rtree_keys", Value: "YES"},
	{Scope: ScopeGlobal, Name: "innodb_old_blocks_pct", Value: "37"},
	{Scope: ScopeGlobal, Name: "innodb_file_format", Value: "Barracuda", Type: TypeEnum, PossibleValues: []string{"Antelope", "Barracuda"}},
	{Scope: ScopeGlobal, Name: "innodb_default_row_format", Value: "dynamic", Type: TypeEnum, PossibleValues: []string{"redundant", "compact", "dynamic"}},
	{Scope: ScopeGlobal, Name: "innodb_compression_failure_threshold_pct", Value: "5"},
	{Scope: ScopeNone, Name: "performance_schema_events_waits_history_long_size", Value: "10000"},
	{Scope: ScopeGlobal, Name: "innodb_checksum_algorithm", Value: "innodb"},
	{Scope: ScopeNone, Name: "innodb_ft_sort_pll_degree", Value: "2"},
	{Scope: ScopeNone, Name: "thread_stack", Value: "262144"},
	{Scope: ScopeGlobal, Name: "relay_log_info_repository", Value: "FILE"},
	{Scope: ScopeGlobal | ScopeSession, Name: "max_delayed_threads", Value: "20"},
	{Scope: ScopeNone, Name: "protocol_version", Value: "10"},
	{Scope: ScopeGlobal | ScopeSession, Name: "new", Value: Off},
	{Scope: ScopeGlobal | ScopeSession, Name: "myisam_sort_buffer_size", Value: "8388608"},
	{Scope: ScopeGlobal | ScopeSession, Name: "optimizer_trace_offset", Value: "-1"},
	{Scope: ScopeGlobal, Name: InnodbBufferPoolDumpAtShutdown, Value: "0"},
	{Scope: ScopeGlobal | ScopeSession, Name: SQLNotes, Value: "1"},
	{Scope: ScopeGlobal, Name: InnodbCmpPerIndexEnabled, Value: Off, Type: TypeBool, AutoConvertNegativeBool: true},
	{Scope: ScopeGlobal, Name: "innodb_ft_server_stopword_table", Value: ""},
	{Scope: ScopeNone, Name: "performance_schema_max_file_instances", Value: "7693"},
	{Scope: ScopeNone, Name: "log_output", Value: "FILE"},
	{Scope: ScopeGlobal, Name: "binlog_group_commit_sync_delay", Value: ""},
	{Scope: ScopeGlobal, Name: "binlog_group_commit_sync_no_delay_count", Value: ""},
	{Scope: ScopeNone, Name: "have_crypt", Value: "YES"},
	{Scope: ScopeGlobal, Name: "innodb_log_write_ahead_size", Value: ""},
	{Scope: ScopeNone, Name: "innodb_log_group_home_dir", Value: "./"},
	{Scope: ScopeNone, Name: "performance_schema_events_statements_history_size", Value: "10"},
	{Scope: ScopeGlobal, Name: GeneralLog, Value: Off, Type: TypeBool},
	{Scope: ScopeGlobal, Name: "validate_password_dictionary_file", Value: ""},
	{Scope: ScopeGlobal, Name: BinlogOrderCommits, Value: On, Type: TypeBool},
	{Scope: ScopeGlobal, Name: "key_cache_division_limit", Value: "100"},
	{Scope: ScopeGlobal | ScopeSession, Name: "max_insert_delayed_threads", Value: "20"},
	{Scope: ScopeNone, Name: "performance_schema_session_connect_attrs_size", Value: "512"},
	{Scope: ScopeGlobal, Name: "innodb_max_dirty_pages_pct", Value: "75"},
	{Scope: ScopeGlobal, Name: InnodbFilePerTable, Value: On, Type: TypeBool, AutoConvertNegativeBool: true},
	{Scope: ScopeGlobal, Name: InnodbLogCompressedPages, Value: "1"},
	{Scope: ScopeNone, Name: "skip_networking", Value: "0"},
	{Scope: ScopeGlobal, Name: "innodb_monitor_reset", Value: ""},
	{Scope: ScopeNone, Name: "ssl_cipher", Value: ""},
	{Scope: ScopeNone, Name: "tls_version", Value: "TLSv1,TLSv1.1,TLSv1.2"},
	{Scope: ScopeGlobal, Name: InnodbPrintAllDeadlocks, Value: Off, Type: TypeBool, AutoConvertNegativeBool: true},
	{Scope: ScopeNone, Name: "innodb_autoinc_lock_mode", Value: "1"},
	{Scope: ScopeGlobal, Name: "key_buffer_size", Value: "8388608"},
	{Scope: ScopeGlobal, Name: "host_cache_size", Value: "279"},
	{Scope: ScopeGlobal, Name: DelayKeyWrite, Value: On, Type: TypeEnum, PossibleValues: []string{Off, On, "ALL"}},
	{Scope: ScopeNone, Name: "metadata_locks_cache_size", Value: "1024"},
	{Scope: ScopeNone, Name: "innodb_force_recovery", Value: "0"},
	{Scope: ScopeGlobal, Name: "innodb_file_format_max", Value: "Antelope"},
	{Scope: ScopeGlobal | ScopeSession, Name: "debug", Value: ""},
	{Scope: ScopeGlobal, Name: "log_warnings", Value: "1"},
	{Scope: ScopeGlobal | ScopeSession, Name: InnodbStrictMode, Value: On, Type: TypeBool, AutoConvertNegativeBool: true},
	{Scope: ScopeGlobal, Name: "innodb_rollback_segments", Value: "128"},
	{Scope: ScopeGlobal | ScopeSession, Name: "join_buffer_size", Value: "262144", IsHintUpdatable: true},
	{Scope: ScopeNone, Name: "innodb_mirrored_log_groups", Value: "1"},
	{Scope: ScopeGlobal, Name: "max_binlog_size", Value: "1073741824"},
	{Scope: ScopeGlobal, Name: "concurrent_insert", Value: "AUTO"},
	{Scope: ScopeGlobal, Name: InnodbAdaptiveHashIndex, Value: On, Type: TypeBool, AutoConvertNegativeBool: true},
	{Scope: ScopeGlobal, Name: InnodbFtEnableStopword, Value: On, Type: TypeBool, AutoConvertNegativeBool: true},
	{Scope: ScopeGlobal, Name: "general_log_file", Value: "/usr/local/mysql/data/localhost.log"},
	{Scope: ScopeGlobal | ScopeSession, Name: InnodbSupportXA, Value: "1"},
	{Scope: ScopeGlobal, Name: "innodb_compression_level", Value: "6"},
	{Scope: ScopeNone, Name: "innodb_file_format_check", Value: "1"},
	{Scope: ScopeNone, Name: "myisam_mmap_size", Value: "18446744073709551615"},
	{Scope: ScopeNone, Name: "innodb_buffer_pool_instances", Value: "8"},
	{Scope: ScopeGlobal | ScopeSession, Name: "max_length_for_sort_data", Value: "1024", IsHintUpdatable: true},
	{Scope: ScopeNone, Name: CharacterSetSystem, Value: "utf8"},
	{Scope: ScopeGlobal, Name: InnodbOptimizeFullTextOnly, Value: "0"},
	{Scope: ScopeNone, Name: "character_sets_dir", Value: "/usr/local/mysql-5.6.25-osx10.8-x86_64/share/charsets/"},
	{Scope: ScopeGlobal | ScopeSession, Name: QueryCacheType, Value: Off, Type: TypeEnum, PossibleValues: []string{Off, On, "DEMAND"}},
	{Scope: ScopeNone, Name: "innodb_rollback_on_timeout", Value: "0"},
	{Scope: ScopeGlobal | ScopeSession, Name: "query_alloc_block_size", Value: "8192"},
	{Scope: ScopeNone, Name: "have_compress", Value: "YES"},
	{Scope: ScopeNone, Name: "thread_concurrency", Value: "10"},
	{Scope: ScopeGlobal | ScopeSession, Name: "query_prealloc_size", Value: "8192"},
	{Scope: ScopeNone, Name: "relay_log_space_limit", Value: "0"},
	{Scope: ScopeGlobal | ScopeSession, Name: MaxUserConnections, Value: "0", Type: TypeUnsigned, MinValue: 0, MaxValue: 4294967295, AutoConvertOutOfRange: true},
	{Scope: ScopeNone, Name: "performance_schema_max_thread_classes", Value: "50"},
	{Scope: ScopeGlobal, Name: "innodb_api_trx_level", Value: "0"},
	{Scope: ScopeNone, Name: "disconnect_on_expired_password", Value: "1"},
	{Scope: ScopeNone, Name: "performance_schema_max_file_classes", Value: "50"},
	{Scope: ScopeGlobal, Name: "expire_logs_days", Value: "0"},
	{Scope: ScopeGlobal | ScopeSession, Name: BinlogRowQueryLogEvents, Value: Off, Type: TypeBool},
	{Scope: ScopeGlobal, Name: "default_password_lifetime", Value: ""},
	{Scope: ScopeNone, Name: "pid_file", Value: "/usr/local/mysql/data/localhost.pid"},
	{Scope: ScopeNone, Name: "innodb_undo_tablespaces", Value: "0"},
	{Scope: ScopeGlobal, Name: InnodbStatusOutputLocks, Value: Off, Type: TypeBool, AutoConvertNegativeBool: true},
	{Scope: ScopeNone, Name: "performance_schema_accounts_size", Value: "100"},
	{Scope: ScopeGlobal | ScopeSession, Name: "max_error_count", Value: "64", IsHintUpdatable: true},
	{Scope: ScopeGlobal, Name: "max_write_lock_count", Value: "18446744073709551615"},
	{Scope: ScopeNone, Name: "performance_schema_max_socket_instances", Value: "322"},
	{Scope: ScopeNone, Name: "performance_schema_max_table_instances", Value: "12500"},
	{Scope: ScopeGlobal, Name: "innodb_stats_persistent_sample_pages", Value: "20"},
	{Scope: ScopeGlobal, Name: "show_compatibility_56", Value: ""},
	{Scope: ScopeNone, Name: "innodb_open_files", Value: "2000"},
	{Scope: ScopeGlobal, Name: "innodb_spin_wait_delay", Value: "6"},
	{Scope: ScopeGlobal, Name: "thread_cache_size", Value: "9"},
	{Scope: ScopeGlobal, Name: LogSlowAdminStatements, Value: Off, Type: TypeBool},
	{Scope: ScopeNone, Name: "innodb_checksums", Type: TypeBool, Value: On},
	{Scope: ScopeNone, Name: "ft_stopword_file", Value: "(built-in)"},
	{Scope: ScopeGlobal, Name: "innodb_max_dirty_pages_pct_lwm", Value: "0"},
	{Scope: ScopeGlobal, Name: LogQueriesNotUsingIndexes, Value: Off, Type: TypeBool},
	{Scope: ScopeGlobal | ScopeSession, Name: "max_heap_table_size", Value: "16777216", IsHintUpdatable: true},
	{Scope: ScopeGlobal | ScopeSession, Name: "div_precision_increment", Value: "4", IsHintUpdatable: true},
	{Scope: ScopeGlobal, Name: "innodb_lru_scan_depth", Value: "1024"},
	{Scope: ScopeGlobal, Name: "innodb_purge_rseg_truncate_frequency", Value: ""},
	{Scope: ScopeGlobal | ScopeSession, Name: SQLAutoIsNull, Value: Off, Type: TypeBool, IsHintUpdatable: true},
	{Scope: ScopeNone, Name: "innodb_api_enable_binlog", Value: "0"},
	{Scope: ScopeGlobal | ScopeSession, Name: "innodb_ft_user_stopword_table", Value: ""},
	{Scope: ScopeNone, Name: "server_id_bits", Value: "32"},
	{Scope: ScopeGlobal, Name: "innodb_log_checksum_algorithm", Value: ""},
	{Scope: ScopeNone, Name: "innodb_buffer_pool_load_at_startup", Value: "1"},
	{Scope: ScopeGlobal | ScopeSession, Name: "sort_buffer_size", Value: "262144", IsHintUpdatable: true},
	{Scope: ScopeGlobal, Name: "innodb_flush_neighbors", Value: "1"},
	{Scope: ScopeNone, Name: "innodb_use_sys_malloc", Value: "1"},
	{Scope: ScopeNone, Name: "performance_schema_max_socket_classes", Value: "10"},
	{Scope: ScopeNone, Name: "performance_schema_max_stage_classes", Value: "150"},
	{Scope: ScopeGlobal, Name: "innodb_purge_batch_size", Value: "300"},
	{Scope: ScopeNone, Name: "have_profiling", Value: "NO"},
	{Scope: ScopeGlobal, Name: InnodbBufferPoolDumpNow, Value: Off, Type: TypeBool, AutoConvertNegativeBool: true},
	{Scope: ScopeGlobal, Name: RelayLogPurge, Value: On, Type: TypeBool},
	{Scope: ScopeGlobal, Name: "ndb_distribution", Value: ""},
	{Scope: ScopeGlobal, Name: "myisam_data_pointer_size", Value: "6"},
	{Scope: ScopeGlobal, Name: "ndb_optimization_delay", Value: ""},
	{Scope: ScopeGlobal, Name: "innodb_ft_num_word_optimize", Value: "2000"},
	{Scope: ScopeGlobal | ScopeSession, Name: "max_join_size", Value: "18446744073709551615", IsHintUpdatable: true},
	{Scope: ScopeNone, Name: CoreFile, Value: Off, Type: TypeBool},
	{Scope: ScopeGlobal | ScopeSession, Name: "max_seeks_for_key", Value: "18446744073709551615", IsHintUpdatable: true},
	{Scope: ScopeNone, Name: "innodb_log_buffer_size", Value: "8388608"},
	{Scope: ScopeGlobal, Name: "delayed_insert_timeout", Value: "300"},
	{Scope: ScopeGlobal, Name: "max_relay_log_size", Value: "0"},
	{Scope: ScopeGlobal | ScopeSession, Name: MaxSortLength, Value: "1024", Type: TypeUnsigned, MinValue: 4, MaxValue: 8388608, AutoConvertOutOfRange: true, IsHintUpdatable: true},
	{Scope: ScopeNone, Name: "metadata_locks_hash_instances", Value: "8"},
	{Scope: ScopeGlobal, Name: "ndb_eventbuffer_free_percent", Value: ""},
	{Scope: ScopeNone, Name: "large_files_support", Value: "1"},
	{Scope: ScopeGlobal, Name: "binlog_max_flush_queue_time", Value: "0"},
	{Scope: ScopeGlobal, Name: "innodb_fill_factor", Value: ""},
	{Scope: ScopeGlobal, Name: "log_syslog_facility", Value: ""},
	{Scope: ScopeNone, Name: "innodb_ft_min_token_size", Value: "3"},
	{Scope: ScopeGlobal | ScopeSession, Name: "transaction_write_set_extraction", Value: ""},
	{Scope: ScopeGlobal | ScopeSession, Name: "ndb_blob_write_batch_bytes", Value: ""},
	{Scope: ScopeGlobal, Name: "automatic_sp_privileges", Value: "1"},
	{Scope: ScopeGlobal, Name: "innodb_flush_sync", Value: ""},
	{Scope: ScopeNone, Name: "performance_schema_events_statements_history_long_size", Value: "10000"},
	{Scope: ScopeGlobal, Name: "innodb_monitor_disable", Value: ""},
	{Scope: ScopeNone, Name: "innodb_doublewrite", Value: "1"},
	{Scope: ScopeNone, Name: "log_bin_use_v1_row_events", Value: "0"},
	{Scope: ScopeSession, Name: "innodb_optimize_point_storage", Value: ""},
	{Scope: ScopeNone, Name: "innodb_api_disable_rowlock", Value: "0"},
	{Scope: ScopeGlobal, Name: "innodb_adaptive_flushing_lwm", Value: "10"},
	{Scope: ScopeNone, Name: "innodb_log_files_in_group", Value: "2"},
	{Scope: ScopeGlobal, Name: InnodbBufferPoolLoadNow, Value: Off, Type: TypeBool, AutoConvertNegativeBool: true},
	{Scope: ScopeNone, Name: "performance_schema_max_rwlock_classes", Value: "40"},
	{Scope: ScopeNone, Name: "binlog_gtid_simple_recovery", Value: "1"},
	{Scope: ScopeNone, Name: "performance_schema_digests_size", Value: "10000"},
	{Scope: ScopeGlobal | ScopeSession, Name: Profiling, Value: Off, Type: TypeBool},
	{Scope: ScopeSession, Name: "rand_seed1", Value: ""},
	{Scope: ScopeGlobal, Name: "sha256_password_proxy_users", Value: ""},
	{Scope: ScopeGlobal | ScopeSession, Name: SQLQuoteShowCreate, Value: On, Type: TypeBool},
	{Scope: ScopeGlobal | ScopeSession, Name: "binlogging_impossible_mode", Value: "IGNORE_ERROR"},
	{Scope: ScopeGlobal | ScopeSession, Name: QueryCacheSize, Value: "1048576"},
	{Scope: ScopeGlobal, Name: "innodb_stats_transient_sample_pages", Value: "8"},
	{Scope: ScopeGlobal, Name: InnodbStatsOnMetadata, Value: "0"},
	{Scope: ScopeNone, Name: "server_uuid", Value: "00000000-0000-0000-0000-000000000000"},
	{Scope: ScopeNone, Name: "open_files_limit", Value: "5000"},
	{Scope: ScopeGlobal | ScopeSession, Name: "ndb_force_send", Value: ""},
	{Scope: ScopeNone, Name: "skip_show_database", Value: "0"},
	{Scope: ScopeGlobal, Name: "log_timestamps", Value: ""},
	{Scope: ScopeNone, Name: "version_compile_machine", Value: "x86_64"},
	{Scope: ScopeGlobal, Name: "event_scheduler", Value: Off},
	{Scope: ScopeGlobal | ScopeSession, Name: "ndb_deferred_constraints", Value: ""},
	{Scope: ScopeGlobal, Name: "log_syslog_include_pid", Value: ""},
	{Scope: ScopeNone, Name: "innodb_ft_cache_size", Value: "8000000"},
	{Scope: ScopeGlobal, Name: InnodbDisableSortFileCache, Value: "0"},
	{Scope: ScopeGlobal, Name: "log_error_verbosity", Value: ""},
	{Scope: ScopeNone, Name: "performance_schema_hosts_size", Value: "100"},
	{Scope: ScopeGlobal, Name: "innodb_replication_delay", Value: "0"},
	{Scope: ScopeGlobal, Name: SlowQueryLog, Value: "0"},
	{Scope: ScopeSession, Name: "debug_sync", Value: ""},
	{Scope: ScopeGlobal, Name: InnodbStatsAutoRecalc, Value: "1"},
	{Scope: ScopeGlobal | ScopeSession, Name: "lc_messages", Value: "en_US"},
	{Scope: ScopeGlobal | ScopeSession, Name: "bulk_insert_buffer_size", Value: "8388608", IsHintUpdatable: true},
	{Scope: ScopeGlobal | ScopeSession, Name: BinlogDirectNonTransactionalUpdates, Value: Off, Type: TypeBool},
	{Scope: ScopeGlobal, Name: "innodb_change_buffering", Value: "all"},
	{Scope: ScopeGlobal | ScopeSession, Name: SQLBigSelects, Value: On, Type: TypeBool, IsHintUpdatable: true},
	{Scope: ScopeGlobal, Name: "innodb_max_purge_lag_delay", Value: "0"},
	{Scope: ScopeGlobal | ScopeSession, Name: "session_track_schema", Value: ""},
	{Scope: ScopeGlobal, Name: "innodb_io_capacity_max", Value: "2000"},
	{Scope: ScopeGlobal, Name: "innodb_autoextend_increment", Value: "64"},
	{Scope: ScopeGlobal | ScopeSession, Name: "binlog_format", Value: "STATEMENT"},
	{Scope: ScopeGlobal | ScopeSession, Name: "optimizer_trace", Value: "enabled=off,one_line=off"},
	{Scope: ScopeGlobal | ScopeSession, Name: "read_rnd_buffer_size", Value: "262144", IsHintUpdatable: true},
	{Scope: ScopeGlobal | ScopeSession, Name: NetWriteTimeout, Value: "60"},
	{Scope: ScopeGlobal, Name: InnodbBufferPoolLoadAbort, Value: Off, Type: TypeBool, AutoConvertNegativeBool: true},
	{Scope: ScopeGlobal | ScopeSession, Name: "transaction_prealloc_size", Value: "4096"},
	{Scope: ScopeNone, Name: "performance_schema_setup_objects_size", Value: "100"},
	{Scope: ScopeGlobal, Name: "sync_relay_log", Value: "10000"},
	{Scope: ScopeGlobal, Name: "innodb_ft_result_cache_limit", Value: "2000000000"},
	{Scope: ScopeNone, Name: "innodb_sort_buffer_size", Value: "1048576"},
	{Scope: ScopeGlobal, Name: "innodb_ft_enable_diag_print", Type: TypeBool, Value: Off},
	{Scope: ScopeNone, Name: "thread_handling", Value: "one-thread-per-connection"},
	{Scope: ScopeGlobal, Name: "stored_program_cache", Value: "256"},
	{Scope: ScopeNone, Name: "performance_schema_max_mutex_instances", Value: "15906"},
	{Scope: ScopeGlobal, Name: "innodb_adaptive_max_sleep_delay", Value: "150000"},
	{Scope: ScopeNone, Name: "large_pages", Value: Off},
	{Scope: ScopeGlobal | ScopeSession, Name: "session_track_system_variables", Value: ""},
	{Scope: ScopeGlobal, Name: "innodb_change_buffer_max_size", Value: "25"},
	{Scope: ScopeGlobal, Name: LogBinTrustFunctionCreators, Value: Off, Type: TypeBool},
	{Scope: ScopeNone, Name: "innodb_write_io_threads", Value: "4"},
	{Scope: ScopeGlobal, Name: "mysql_native_password_proxy_users", Value: ""},
	{Scope: ScopeNone, Name: "large_page_size", Value: "0"},
	{Scope: ScopeNone, Name: "table_open_cache_instances", Value: "1"},
	{Scope: ScopeGlobal, Name: InnodbStatsPersistent, Value: On, Type: TypeBool, AutoConvertNegativeBool: true},
	{Scope: ScopeGlobal | ScopeSession, Name: "session_track_state_change", Value: ""},
	{Scope: ScopeNone, Name: OptimizerSwitch, Value: "index_merge=on,index_merge_union=on,index_merge_sort_union=on,index_merge_intersection=on,engine_condition_pushdown=on,index_condition_pushdown=on,mrr=on,mrr_cost_based=on,block_nested_loop=on,batched_key_access=off,materialization=on,semijoin=on,loosescan=on,firstmatch=on,subquery_materialization_cost_based=on,use_index_extensions=on", IsHintUpdatable: true},
	{Scope: ScopeGlobal, Name: "delayed_queue_size", Value: "1000"},
	{Scope: ScopeNone, Name: "innodb_read_only", Value: "0"},
	{Scope: ScopeNone, Name: "datetime_format", Value: "%Y-%m-%d %H:%i:%s"},
	{Scope: ScopeGlobal, Name: "log_syslog", Value: ""},
	{Scope: ScopeGlobal | ScopeSession, Name: "transaction_alloc_block_size", Value: "8192"},
	{Scope: ScopeGlobal, Name: "innodb_large_prefix", Type: TypeBool, Value: On},
	{Scope: ScopeNone, Name: "performance_schema_max_cond_classes", Value: "80"},
	{Scope: ScopeGlobal, Name: "innodb_io_capacity", Value: "200"},
	{Scope: ScopeGlobal, Name: "max_binlog_cache_size", Value: "18446744073709547520"},
	{Scope: ScopeGlobal | ScopeSession, Name: "ndb_index_stat_enable", Value: ""},
	{Scope: ScopeGlobal, Name: "executed_gtids_compression_period", Value: ""},
	{Scope: ScopeNone, Name: "time_format", Value: "%H:%i:%s"},
	{Scope: ScopeGlobal | ScopeSession, Name: OldAlterTable, Value: Off, Type: TypeBool},
	{Scope: ScopeGlobal | ScopeSession, Name: "long_query_time", Value: "10.000000"},
	{Scope: ScopeNone, Name: "innodb_use_native_aio", Value: "0"},
	{Scope: ScopeGlobal, Name: "log_throttle_queries_not_using_indexes", Value: "0"},
	{Scope: ScopeNone, Name: "locked_in_memory", Value: "0"},
	{Scope: ScopeNone, Name: "innodb_api_enable_mdl", Value: "0"},
	{Scope: ScopeGlobal, Name: "binlog_cache_size", Value: "32768"},
	{Scope: ScopeGlobal, Name: "innodb_compression_pad_pct_max", Value: "50"},
	{Scope: ScopeGlobal, Name: InnodbCommitConcurrency, Value: "0", Type: TypeUnsigned, MinValue: 0, MaxValue: 1000, AutoConvertOutOfRange: true},
	{Scope: ScopeNone, Name: "ft_min_word_len", Value: "4"},
	{Scope: ScopeGlobal, Name: EnforceGtidConsistency, Value: Off, Type: TypeEnum, PossibleValues: []string{Off, On, "WARN"}},
	{Scope: ScopeGlobal, Name: SecureAuth, Value: On, Type: TypeBool, Validation: func(vars *SessionVars, normalizedValue string, originalValue string, scope ScopeFlag) (string, error) {
		if TiDBOptOn(normalizedValue) {
			return On, nil
		}
		return normalizedValue, ErrWrongValueForVar.GenWithStackByArgs(SecureAuth, originalValue)
	}},
	{Scope: ScopeNone, Name: "max_tmp_tables", Value: "32"},
	{Scope: ScopeGlobal, Name: InnodbRandomReadAhead, Value: Off, Type: TypeBool, AutoConvertNegativeBool: true},
	{Scope: ScopeGlobal | ScopeSession, Name: UniqueChecks, Value: On, Type: TypeBool, IsHintUpdatable: true},
	{Scope: ScopeGlobal, Name: "internal_tmp_disk_storage_engine", Value: ""},
	{Scope: ScopeGlobal | ScopeSession, Name: "myisam_repair_threads", Value: "1"},
	{Scope: ScopeGlobal, Name: "ndb_eventbuffer_max_alloc", Value: ""},
	{Scope: ScopeGlobal, Name: "innodb_read_ahead_threshold", Value: "56"},
	{Scope: ScopeGlobal, Name: "key_cache_block_size", Value: "1024"},
	{Scope: ScopeNone, Name: "ndb_recv_thread_cpu_mask", Value: ""},
	{Scope: ScopeGlobal, Name: "gtid_purged", Value: ""},
	{Scope: ScopeGlobal, Name: "max_binlog_stmt_cache_size", Value: "18446744073709547520"},
	{Scope: ScopeGlobal | ScopeSession, Name: "lock_wait_timeout", Value: "31536000"},
	{Scope: ScopeGlobal | ScopeSession, Name: "read_buffer_size", Value: "131072", IsHintUpdatable: true},
	{Scope: ScopeNone, Name: "innodb_read_io_threads", Value: "4"},
	{Scope: ScopeGlobal | ScopeSession, Name: MaxSpRecursionDepth, Value: "0", Type: TypeUnsigned, MinValue: 0, MaxValue: 255, AutoConvertOutOfRange: true},
	{Scope: ScopeNone, Name: "ignore_builtin_innodb", Value: "0"},
	{Scope: ScopeGlobal, Name: "slow_query_log_file", Value: "/usr/local/mysql/data/localhost-slow.log"},
	{Scope: ScopeGlobal, Name: "innodb_thread_sleep_delay", Value: "10000"},
	{Scope: ScopeGlobal, Name: "innodb_ft_aux_table", Value: ""},
	{Scope: ScopeGlobal | ScopeSession, Name: SQLWarnings, Value: Off, Type: TypeBool},
	{Scope: ScopeGlobal | ScopeSession, Name: KeepFilesOnCreate, Value: Off, Type: TypeBool},
	{Scope: ScopeNone, Name: "innodb_data_file_path", Value: "ibdata1:12M:autoextend"},
	{Scope: ScopeNone, Name: "performance_schema_setup_actors_size", Value: "100"},
	{Scope: ScopeNone, Name: "innodb_additional_mem_pool_size", Value: "8388608"},
	{Scope: ScopeNone, Name: "log_error", Value: "/usr/local/mysql/data/localhost.err"},
	{Scope: ScopeGlobal, Name: "binlog_stmt_cache_size", Value: "32768"},
	{Scope: ScopeNone, Name: "relay_log_info_file", Value: "relay-log.info"},
	{Scope: ScopeNone, Name: "innodb_ft_total_cache_size", Value: "640000000"},
	{Scope: ScopeNone, Name: "performance_schema_max_rwlock_instances", Value: "9102"},
	{Scope: ScopeGlobal, Name: "table_open_cache", Value: "2000"},
	{Scope: ScopeNone, Name: "performance_schema_events_stages_history_long_size", Value: "10000"},
	{Scope: ScopeSession, Name: "insert_id", Value: ""},
	{Scope: ScopeGlobal | ScopeSession, Name: "default_tmp_storage_engine", Value: "InnoDB", IsHintUpdatable: true},
	{Scope: ScopeGlobal | ScopeSession, Name: "optimizer_search_depth", Value: "62", IsHintUpdatable: true},
	{Scope: ScopeGlobal | ScopeSession, Name: "max_points_in_geometry", Value: "65536", IsHintUpdatable: true},
	{Scope: ScopeGlobal, Name: "innodb_stats_sample_pages", Value: "8"},
	{Scope: ScopeGlobal | ScopeSession, Name: "profiling_history_size", Value: "15"},
	{Scope: ScopeNone, Name: "have_symlink", Value: "YES"},
	{Scope: ScopeGlobal | ScopeSession, Name: "storage_engine", Value: "InnoDB"},
	{Scope: ScopeGlobal | ScopeSession, Name: "sql_log_off", Value: "0"},
	// In MySQL, the default value of `explicit_defaults_for_timestamp` is `0`.
	// But In TiDB, it's set to `1` to be consistent with TiDB timestamp behavior.
	// See: https://github.com/pingcap/tidb/pull/6068 for details
	{Scope: ScopeNone, Name: "explicit_defaults_for_timestamp", Value: On, Type: TypeBool},
	{Scope: ScopeNone, Name: "performance_schema_events_waits_history_size", Value: "10"},
	{Scope: ScopeGlobal, Name: "log_syslog_tag", Value: ""},
	{Scope: ScopeGlobal, Name: "innodb_undo_log_truncate", Value: ""},
	{Scope: ScopeSession, Name: "innodb_create_intrinsic", Value: ""},
	{Scope: ScopeGlobal, Name: "gtid_executed_compression_period", Value: ""},
	{Scope: ScopeGlobal, Name: "ndb_log_empty_epochs", Value: ""},
	{Scope: ScopeNone, Name: "have_geometry", Value: "YES"},
	{Scope: ScopeGlobal | ScopeSession, Name: "optimizer_trace_max_mem_size", Value: "16384"},
	{Scope: ScopeGlobal | ScopeSession, Name: "net_retry_count", Value: "10"},
	{Scope: ScopeSession, Name: "ndb_table_no_logging", Value: ""},
	{Scope: ScopeGlobal | ScopeSession, Name: "optimizer_trace_features", Value: "greedy_search=on,range_optimizer=on,dynamic_range=on,repeated_subselect=on"},
	{Scope: ScopeGlobal, Name: "innodb_flush_log_at_trx_commit", Value: "1"},
	{Scope: ScopeGlobal, Name: "rewriter_enabled", Value: ""},
	{Scope: ScopeGlobal, Name: "query_cache_min_res_unit", Value: "4096"},
	{Scope: ScopeGlobal | ScopeSession, Name: "updatable_views_with_limit", Value: "YES", IsHintUpdatable: true},
	{Scope: ScopeGlobal | ScopeSession, Name: "optimizer_prune_level", Value: "1", IsHintUpdatable: true},
	{Scope: ScopeGlobal | ScopeSession, Name: "completion_type", Value: "NO_CHAIN"},
	{Scope: ScopeGlobal, Name: "binlog_checksum", Value: "CRC32"},
	{Scope: ScopeNone, Name: "report_port", Value: "3306"},
	{Scope: ScopeGlobal | ScopeSession, Name: ShowOldTemporals, Value: Off, Type: TypeBool},
	{Scope: ScopeGlobal, Name: "query_cache_limit", Value: "1048576"},
	{Scope: ScopeGlobal, Name: "innodb_buffer_pool_size", Value: "134217728"},
	{Scope: ScopeGlobal, Name: InnodbAdaptiveFlushing, Value: On, Type: TypeBool, AutoConvertNegativeBool: true},
	{Scope: ScopeGlobal, Name: "innodb_monitor_enable", Value: ""},
	{Scope: ScopeNone, Name: "date_format", Value: "%Y-%m-%d"},
	{Scope: ScopeGlobal, Name: "innodb_buffer_pool_filename", Value: "ib_buffer_pool"},
	{Scope: ScopeGlobal, Name: "slow_launch_time", Value: "2"},
	{Scope: ScopeGlobal | ScopeSession, Name: "ndb_use_transactions", Value: ""},
	{Scope: ScopeNone, Name: "innodb_purge_threads", Value: "1"},
	{Scope: ScopeGlobal, Name: "innodb_concurrency_tickets", Value: "5000"},
	{Scope: ScopeGlobal, Name: "innodb_monitor_reset_all", Value: ""},
	{Scope: ScopeNone, Name: "performance_schema_users_size", Value: "100"},
	{Scope: ScopeGlobal, Name: "ndb_log_updated_only", Value: ""},
	{Scope: ScopeNone, Name: "basedir", Value: "/usr/local/mysql"},
	{Scope: ScopeGlobal, Name: "innodb_old_blocks_time", Value: "1000"},
	{Scope: ScopeGlobal, Name: "innodb_stats_method", Value: "nulls_equal"},
	{Scope: ScopeGlobal, Name: LocalInFile, Value: On, Type: TypeBool},
	{Scope: ScopeGlobal | ScopeSession, Name: "myisam_stats_method", Value: "nulls_unequal"},
	{Scope: ScopeNone, Name: "version_compile_os", Value: "osx10.8"},
	{Scope: ScopeNone, Name: "relay_log_recovery", Value: "0"},
	{Scope: ScopeNone, Name: "old", Value: "0"},
	{Scope: ScopeGlobal | ScopeSession, Name: InnodbTableLocks, Value: On, Type: TypeBool, AutoConvertNegativeBool: true},
	{Scope: ScopeNone, Name: PerformanceSchema, Value: Off, Type: TypeBool},
	{Scope: ScopeNone, Name: "myisam_recover_options", Value: Off},
	{Scope: ScopeGlobal | ScopeSession, Name: NetBufferLength, Value: "16384"},
	{Scope: ScopeGlobal | ScopeSession, Name: "binlog_row_image", Value: "FULL"},
	{Scope: ScopeNone, Name: "innodb_locks_unsafe_for_binlog", Value: "0"},
	{Scope: ScopeSession, Name: "rbr_exec_mode", Value: ""},
	{Scope: ScopeGlobal, Name: "myisam_max_sort_file_size", Value: "9223372036853727232"},
	{Scope: ScopeNone, Name: "back_log", Value: "80"},
	{Scope: ScopeSession, Name: "pseudo_thread_id", Value: ""},
	{Scope: ScopeNone, Name: "have_dynamic_loading", Value: "YES"},
	{Scope: ScopeGlobal, Name: "rewriter_verbose", Value: ""},
	{Scope: ScopeGlobal, Name: "innodb_undo_logs", Value: "128"},
	{Scope: ScopeNone, Name: "performance_schema_max_cond_instances", Value: "3504"},
	{Scope: ScopeGlobal, Name: "delayed_insert_limit", Value: "100"},
	{Scope: ScopeGlobal, Name: Flush, Value: Off, Type: TypeBool},
	{Scope: ScopeGlobal | ScopeSession, Name: "eq_range_index_dive_limit", Value: "200", IsHintUpdatable: true},
	{Scope: ScopeNone, Name: "performance_schema_events_stages_history_size", Value: "10"},
	{Scope: ScopeGlobal | ScopeSession, Name: "ndb_join_pushdown", Value: ""},
	{Scope: ScopeGlobal, Name: "validate_password_special_char_count", Value: "1"},
	{Scope: ScopeNone, Name: "performance_schema_max_thread_instances", Value: "402"},
	{Scope: ScopeGlobal | ScopeSession, Name: "ndbinfo_show_hidden", Value: ""},
	{Scope: ScopeGlobal | ScopeSession, Name: "net_read_timeout", Value: "30"},
	{Scope: ScopeNone, Name: "innodb_page_size", Value: "16384"},
	{Scope: ScopeNone, Name: "innodb_log_file_size", Value: "50331648"},
	{Scope: ScopeGlobal, Name: "sync_relay_log_info", Value: "10000"},
	{Scope: ScopeGlobal | ScopeSession, Name: "optimizer_trace_limit", Value: "1"},
	{Scope: ScopeNone, Name: "innodb_ft_max_token_size", Value: "84"},
	{Scope: ScopeGlobal, Name: ValidatePasswordLength, Value: "8", Type: TypeUnsigned, MinValue: 0, MaxValue: math.MaxUint64, AutoConvertOutOfRange: true},
	{Scope: ScopeGlobal, Name: "ndb_log_binlog_index", Value: ""},
	{Scope: ScopeGlobal, Name: "innodb_api_bk_commit_interval", Value: "5"},
	{Scope: ScopeNone, Name: "innodb_undo_directory", Value: "."},
	{Scope: ScopeNone, Name: "bind_address", Value: "*"},
	{Scope: ScopeGlobal, Name: "innodb_sync_spin_loops", Value: "30"},
	{Scope: ScopeGlobal | ScopeSession, Name: SQLSafeUpdates, Value: Off, Type: TypeBool, IsHintUpdatable: true},
	{Scope: ScopeNone, Name: "tmpdir", Value: "/var/tmp/"},
	{Scope: ScopeGlobal, Name: "innodb_thread_concurrency", Value: "0"},
	{Scope: ScopeGlobal, Name: "innodb_buffer_pool_dump_pct", Value: ""},
	{Scope: ScopeGlobal | ScopeSession, Name: "lc_time_names", Value: "en_US"},
	{Scope: ScopeGlobal | ScopeSession, Name: "max_statement_time", Value: ""},
	{Scope: ScopeGlobal | ScopeSession, Name: EndMarkersInJSON, Value: Off, Type: TypeBool, IsHintUpdatable: true},
	{Scope: ScopeGlobal, Name: AvoidTemporalUpgrade, Value: Off, Type: TypeBool},
	{Scope: ScopeGlobal, Name: "key_cache_age_threshold", Value: "300"},
	{Scope: ScopeGlobal, Name: InnodbStatusOutput, Value: Off, Type: TypeBool, AutoConvertNegativeBool: true},
	{Scope: ScopeSession, Name: "identity", Value: ""},
	{Scope: ScopeGlobal | ScopeSession, Name: "min_examined_row_limit", Value: "0"},
	{Scope: ScopeGlobal, Name: "sync_frm", Type: TypeBool, Value: On},
	{Scope: ScopeGlobal, Name: "innodb_online_alter_log_max_size", Value: "134217728"},
	{Scope: ScopeGlobal | ScopeSession, Name: "information_schema_stats_expiry", Value: "86400"},
	{Scope: ScopeGlobal, Name: ThreadPoolSize, Value: "16", Type: TypeUnsigned, MinValue: 1, MaxValue: 64, AutoConvertOutOfRange: true},
	{Scope: ScopeNone, Name: "lower_case_file_system", Value: "1"},
	// for compatibility purpose, we should leave them alone.
	// TODO: Follow the Terminology Updates of MySQL after their changes arrived.
	// https://mysqlhighavailability.com/mysql-terminology-updates/
	{Scope: ScopeSession, Name: PseudoSlaveMode, Value: Off, Type: TypeBool},
	{Scope: ScopeGlobal, Name: "slave_pending_jobs_size_max", Value: "16777216"},
	{Scope: ScopeGlobal, Name: "slave_transaction_retries", Value: "10"},
	{Scope: ScopeGlobal, Name: "slave_checkpoint_period", Value: "300"},
	{Scope: ScopeGlobal, Name: MasterVerifyChecksum, Value: Off, Type: TypeBool},
	{Scope: ScopeGlobal, Name: "rpl_semi_sync_master_trace_level", Value: ""},
	{Scope: ScopeGlobal, Name: "master_info_repository", Value: "FILE"},
	{Scope: ScopeGlobal, Name: "rpl_stop_slave_timeout", Value: "31536000"},
	{Scope: ScopeGlobal, Name: "slave_net_timeout", Value: "3600"},
	{Scope: ScopeGlobal, Name: "sync_master_info", Value: "10000"},
	{Scope: ScopeGlobal, Name: "init_slave", Value: ""},
	{Scope: ScopeGlobal, Name: SlaveCompressedProtocol, Value: Off, Type: TypeBool},
	{Scope: ScopeGlobal, Name: "rpl_semi_sync_slave_trace_level", Value: ""},
	{Scope: ScopeGlobal, Name: LogSlowSlaveStatements, Value: Off, Type: TypeBool},
	{Scope: ScopeGlobal, Name: "slave_checkpoint_group", Value: "512"},
	{Scope: ScopeNone, Name: "slave_load_tmpdir", Value: "/var/tmp/"},
	{Scope: ScopeGlobal, Name: "slave_parallel_type", Value: ""},
	{Scope: ScopeGlobal, Name: "slave_parallel_workers", Value: "0"},
	{Scope: ScopeGlobal, Name: "rpl_semi_sync_master_timeout", Value: "10000", Type: TypeInt, MaxValue: math.MaxInt64},
	{Scope: ScopeNone, Name: "slave_skip_errors", Value: Off},
	{Scope: ScopeGlobal, Name: "sql_slave_skip_counter", Value: "0"},
	{Scope: ScopeGlobal, Name: "rpl_semi_sync_slave_enabled", Value: Off, Type: TypeBool},
	{Scope: ScopeGlobal, Name: "rpl_semi_sync_master_enabled", Value: Off, Type: TypeBool},
	{Scope: ScopeGlobal, Name: "slave_preserve_commit_order", Value: Off, Type: TypeBool},
	{Scope: ScopeGlobal, Name: "slave_exec_mode", Value: "STRICT"},
	{Scope: ScopeNone, Name: "log_slave_updates", Value: Off, Type: TypeBool},
	{Scope: ScopeGlobal, Name: "rpl_semi_sync_master_wait_point", Value: "AFTER_SYNC", Type: TypeEnum, PossibleValues: []string{"AFTER_SYNC", "AFTER_COMMIT"}},
	{Scope: ScopeGlobal, Name: "slave_sql_verify_checksum", Value: On, Type: TypeBool},
	{Scope: ScopeGlobal, Name: "slave_max_allowed_packet", Value: "1073741824"},
	{Scope: ScopeGlobal, Name: "rpl_semi_sync_master_wait_for_slave_count", Value: "1", Type: TypeInt, MinValue: 1, MaxValue: 65535},
	{Scope: ScopeGlobal, Name: "rpl_semi_sync_master_wait_no_slave", Value: On, Type: TypeBool},
	{Scope: ScopeGlobal, Name: "slave_rows_search_algorithms", Value: "TABLE_SCAN,INDEX_SCAN"},
	{Scope: ScopeGlobal, Name: SlaveAllowBatching, Value: Off, Type: TypeBool},
}
