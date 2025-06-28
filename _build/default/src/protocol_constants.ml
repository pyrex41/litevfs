(* Protocol Constants for DistSQLite Replication *)

(* Magic numbers for protocol identification *)
let dist_magic = 0x44495354l  (* "DIST" in little-endian *)
let sqlite_magic = 0x53514C69l  (* "SQLi" in little-endian *)

(* Message type constants *)
let origin_error_type = 0x01
let origin_msg_type = 0x02
let replica_error_type = 0x03
let replica_msg_type = 0x04
let replica_hash_type = 0x05

(* Protocol version *)
let protocol_version = 1

(* Buffer size limits *)
let max_message_size = 1024 * 1024  (* 1MB *)
let max_path_length = 4096
let max_batch_size = 1000

(* Replication strategy constants *)
let strategy_txlog = 0
let strategy_bulk_sync = 1
let strategy_hybrid = 2
let strategy_page_diff = 3

(* Performance thresholds *)
let small_write_threshold = 4096      (* 4KB *)
let large_write_threshold = 1048576   (* 1MB *)
let bulk_sync_threshold = 10485760    (* 10MB *)

(* Timeouts (in milliseconds) *)
let default_timeout = 5000
let heartbeat_interval = 1000
let lease_duration = 30000

(* Hash algorithm constants *)
let hash_seed = 0x9e3779b9l
let page_size_default = 4096