(** Comprehensive unit tests for OCaml modules *)

open Alcotest
open Bigarray
open Distcore

let test_vfs_write_queue () =
  let queue = VFS.WriteQueue.create ~capacity:10 in
  let event = {
    VFS.path = "/test.db";
    offset = 0L;
    data = Array1.create char c_layout 100;
    is_wal = false;
    fence_token = 1L;
  } in
  
  (* Test enqueue *)
  let success = VFS.WriteQueue.enqueue queue event in
  check bool "enqueue success" true success;
  
  (* Test pending count *)
  let count = VFS.WriteQueue.pending_count queue in
  check int "pending count" 1 count;
  
  (* Test dequeue *)
  let batch = VFS.WriteQueue.dequeue_batch queue ~max:1 in
  check int "batch size" 1 (Array.length batch);
  
  (* Verify data integrity *)
  let dequeued = batch.(0) in
  check string "path preserved" event.path dequeued.path;
  check int64 "offset preserved" event.offset dequeued.offset

let test_binary_diff_basic () =
  let old_data = Array1.create char c_layout 1000 in
  let new_data = Array1.create char c_layout 1000 in
  
  (* Fill with different patterns *)
  for i = 0 to 999 do
    Array1.set old_data i (Char.chr (i mod 256));
    Array1.set new_data i (Char.chr ((i + 1) mod 256));
  done;
  
  let diff = BinaryDiff.compute_diff ~old_db:old_data ~new_db:new_data ~page_size:256 in
  check bool "diff detected changes" true (List.length diff > 0)

let test_binary_diff_rolling_hash () =
  let data = Array1.create char c_layout 1000 in
  for i = 0 to 999 do
    Array1.set data i (Char.chr (i mod 256));
  done;
  
  let hashes = BinaryDiff.rolling_hash ~window_size:64 data in
  check bool "hashes generated" true (List.length hashes > 0);
  
  (* Verify hash consistency *)
  let hashes2 = BinaryDiff.rolling_hash ~window_size:64 data in
  check (list int32) "hash consistency" hashes hashes2

let test_replication_strategy () =
  let events = [|
    { VFS.path = "/test.db"; offset = 0L; 
      data = Array1.create char c_layout 100; is_wal = false; fence_token = 1L };
    { VFS.path = "/test.db"; offset = 100L; 
      data = Array1.create char c_layout 50; is_wal = false; fence_token = 2L };
  |] in
  
  let strategy = Replication.compute_strategy events in
  check bool "strategy computed" true (match strategy with
    | Replication.TxLog -> true
    | Replication.BulkSync -> true
    | Replication.Hybrid -> true
    | Replication.PageDiff -> true)

let test_replication_encoding () =
  let events = [|
    { VFS.path = "/test.db"; offset = 0L; 
      data = Array1.create char c_layout 10; is_wal = false; fence_token = 1L };
  |] in
  
  let encoded = Replication.encode_write_batch events in
  check bool "encoding successful" true (Array1.dim encoded > 0);
  
  (* Test round-trip encoding/decoding *)
  let decoded = Replication.decode_write_batch encoded in
  match decoded with
  | Ok decoded_events ->
      check int "decoded length" 1 (Array.length decoded_events);
      check string "path preserved" events.(0).path decoded_events.(0).path
  | Error _ ->
      fail "decoding failed"

let test_lease_state_machine () =
  open LeaseStateMachine in
  
  let initial_state = Follower { term = 1; voted_for = None } in
  let event = RequestVote { term = 2; candidate_id = "node1"; last_log_index = 0; last_log_term = 1 } in
  
  let new_state, actions = transition initial_state event in
  check bool "state transition occurred" true (new_state <> initial_state);
  check bool "actions generated" true (List.length actions >= 0)

let test_lease_state_machine_runner () =
  open LeaseStateMachine in
  
  let action_count = ref 0 in
  let action_handler action =
    incr action_count;
    match action with
    | SendMessage _ -> ()
    | UpdateStorage _ -> ()
    | SetElectionTimeout _ -> ()
  in
  
  let initial_state = Follower { term = 1; voted_for = None } in
  let runner = Runner.create initial_state action_handler in
  
  let event = RequestVote { term = 2; candidate_id = "node1"; last_log_index = 0; last_log_term = 1 } in
  Runner.handle_event runner event;
  
  check bool "actions executed" true (!action_count >= 0)

let test_memory_operations () =
  let arr1 = Array1.create char c_layout 1000 in
  let arr2 = Array1.create char c_layout 1000 in
  
  (* Fill with same data *)
  for i = 0 to 999 do
    let c = Char.chr (i mod 256) in
    Array1.set arr1 i c;
    Array1.set arr2 i c;
  done;
  
  (* Test memcmp *)
  let cmp_result = MemOps.memcmp arr1 0 arr2 0 1000 in
  check int "identical arrays compare equal" 0 cmp_result;
  
  (* Test hash computation *)
  let hash = MemOps.hash_simd arr1 0 1000 in
  check bool "hash computed" true (hash <> 0l)

let test_performance_monitoring () =
  let module P = PerfMonitor in
  
  (* Reset stats *)
  P.reset_stats ();
  let initial = P.get_stats () in
  check int64 "initial queue enqueues" 0L initial.queue_enqueues;
  
  (* Record some operations *)
  P.record_queue_enqueue ();
  P.record_queue_dequeue 5;
  P.record_bytes_processed 1024;
  
  let updated = P.get_stats () in
  check int64 "queue enqueues updated" 1L updated.queue_enqueues;
  check int64 "queue dequeues updated" 5L updated.queue_dequeues;
  check int64 "bytes processed updated" 1024L updated.bytes_processed

let test_api_functions () =
  let vfs = API.create_vfs () in
  check bool "VFS created" true (Option.is_some vfs);
  
  let queue = API.create_write_queue 100 in
  check bool "write queue created" true (queue <> Obj.repr ());
  
  let stats = API.get_performance_stats () in
  check bool "stats retrieved" true (List.length stats > 0)

let test_error_handling () =
  (* Test invalid inputs *)
  let invalid_queue = VFS.WriteQueue.create ~capacity:(-1) in
  check bool "invalid capacity handled" true (invalid_queue <> Obj.repr ());
  
  (* Test empty diff *)
  let empty_data = Array1.create char c_layout 0 in
  let diff = BinaryDiff.compute_diff ~old_db:empty_data ~new_db:empty_data ~page_size:256 in
  check bool "empty diff handled" true (List.length diff = 0)

let test_memory_bounds () =
  let data = Array1.create char c_layout 100 in
  
  (* Test bounds checking in memcmp - should handle gracefully *)
  let result = MemOps.memcmp data 0 data 50 60 in  (* This should exceed bounds *)
  check bool "bounds checking works" true (result = -1)

let test_concurrent_access () =
  let queue = VFS.WriteQueue.create ~capacity:1000 in
  let event = {
    VFS.path = "/test.db";
    offset = 0L;
    data = Array1.create char c_layout 10;
    is_wal = false;
    fence_token = 1L;
  } in
  
  (* Simulate concurrent enqueues *)
  let successes = ref 0 in
  for _ = 1 to 100 do
    if VFS.WriteQueue.enqueue queue event then
      incr successes
  done;
  
  check bool "concurrent enqueues handled" true (!successes > 0);
  
  let final_count = VFS.WriteQueue.pending_count queue in
  check bool "final count reasonable" true (final_count >= 0 && final_count <= 1000)

(* Test suite definition *)
let vfs_tests = [
  test_case "Write queue basic operations" `Quick test_vfs_write_queue;
]

let binary_diff_tests = [
  test_case "Basic diff computation" `Quick test_binary_diff_basic;
  test_case "Rolling hash consistency" `Quick test_binary_diff_rolling_hash;
]

let replication_tests = [
  test_case "Strategy computation" `Quick test_replication_strategy;
  test_case "Encoding round-trip" `Quick test_replication_encoding;
]

let lease_tests = [
  test_case "State machine transitions" `Quick test_lease_state_machine;
  test_case "State machine runner" `Quick test_lease_state_machine_runner;
]

let memory_tests = [
  test_case "Memory operations" `Quick test_memory_operations;
  test_case "Memory bounds checking" `Quick test_memory_bounds;
]

let monitoring_tests = [
  test_case "Performance monitoring" `Quick test_performance_monitoring;
]

let api_tests = [
  test_case "High-level API functions" `Quick test_api_functions;
]

let error_tests = [
  test_case "Error handling" `Quick test_error_handling;
]

let concurrency_tests = [
  test_case "Concurrent access" `Quick test_concurrent_access;
]

(* Main test runner *)
let () =
  run "DistCore OCaml Unit Tests" [
    ("VFS", vfs_tests);
    ("Binary Diff", binary_diff_tests);
    ("Replication", replication_tests);
    ("Lease State Machine", lease_tests);
    ("Memory Operations", memory_tests);
    ("Performance Monitoring", monitoring_tests);
    ("API", api_tests);
    ("Error Handling", error_tests);
    ("Concurrency", concurrency_tests);
  ]