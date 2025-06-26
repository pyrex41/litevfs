(** Test Program for OCaml ZMQ Integration **)

open Zmq_integration

(** Test utilities **)
module TestUtils = struct
  let create_test_data size =
    let data = Bytes.create size in
    for i = 0 to size - 1 do
      Bytes.set_uint8 data i (Random.int 256)
    done;
    data

  let measure_time f =
    let start = Unix.gettimeofday () in
    let result = f () in
    let duration = Unix.gettimeofday () -. start in
    (result, duration)

  let print_test_result name success duration =
    let status = if success then "✓ PASS" else "✗ FAIL" in
    Printf.printf "[Test] %s %s (%.3fs)\n%!" status name duration
end

(** Test lease coordination **)
let test_lease_coordination () =
  Printf.printf "\n=== Testing Lease Coordination ===\n%!";
  
  (* Create coordinator *)
  let coord_endpoints = [Transport.create_endpoint "tcp" "localhost" (Some 5560)] in
  let coord_config = DistSQLiteZMQ.default_config DistSQLiteZMQ.Coordinator "coordinator" coord_endpoints in
  let coordinator = DistSQLiteZMQ.create coord_config in
  
  (* Create node *)
  let node_endpoints = [Transport.create_endpoint "tcp" "localhost" (Some 5560)] in
  let node_config = DistSQLiteZMQ.default_config (DistSQLiteZMQ.Node "peer1") "node1" node_endpoints in
  let node = DistSQLiteZMQ.create node_config in
  
  try
    (* Start services *)
    DistSQLiteZMQ.start coordinator;
    Unix.sleepf 0.1; (* Let coordinator start *)
    
    DistSQLiteZMQ.start node;
    Unix.sleepf 0.1; (* Let node connect *)
    
    (* Test lease acquisition *)
    Printf.printf "[Test] Acquiring lease for /test/db.sqlite\n%!";
    let (acquire_result, acquire_time) = TestUtils.measure_time (fun () ->
      DistSQLiteZMQ.acquire_lease node "/test/db.sqlite" 30L
    ) in
    
    let acquire_success = acquire_result.Messages.success in
    TestUtils.print_test_result "Lease Acquisition" acquire_success acquire_time;
    
    if acquire_success then begin
      Printf.printf "[Test] Fence token: %Ld\n%!" acquire_result.fence_token;
      
      (* Test lease renewal *)
      let (renew_result, renew_time) = TestUtils.measure_time (fun () ->
        DistSQLiteZMQ.renew_lease node "/test/db.sqlite" 60L
      ) in
      
      let renew_success = renew_result.Messages.success in
      TestUtils.print_test_result "Lease Renewal" renew_success renew_time;
      
      (* Test lease release *)
      let (release_result, release_time) = TestUtils.measure_time (fun () ->
        DistSQLiteZMQ.release_lease node "/test/db.sqlite"
      ) in
      
      let release_success = release_result.Messages.success in
      TestUtils.print_test_result "Lease Release" release_success release_time
    end;
    
    (* Cleanup *)
    DistSQLiteZMQ.stop node;
    DistSQLiteZMQ.stop coordinator;
    
    acquire_success
  with
  | exn ->
      Printf.printf "[Test] Lease coordination failed: %s\n%!" (Printexc.to_string exn);
      DistSQLiteZMQ.stop node;
      DistSQLiteZMQ.stop coordinator;
      false

(** Test WAL replication **)
let test_wal_replication () =
  Printf.printf "\n=== Testing WAL Replication ===\n%!";
  
  (* Create coordinator/publisher *)
  let pub_endpoints = [Transport.create_endpoint "tcp" "localhost" (Some 5570)] in
  let pub_config = DistSQLiteZMQ.default_config DistSQLiteZMQ.Coordinator "publisher" pub_endpoints in
  let publisher = DistSQLiteZMQ.create pub_config in
  
  (* Create subscriber node *)
  let sub_endpoints = [Transport.create_endpoint "tcp" "localhost" (Some 5570)] in
  let sub_config = DistSQLiteZMQ.default_config (DistSQLiteZMQ.Node "sub1") "subscriber" sub_endpoints in
  let subscriber = DistSQLiteZMQ.create sub_config in
  
  try
    (* Start services *)
    DistSQLiteZMQ.start publisher;
    Unix.sleepf 0.1;
    
    DistSQLiteZMQ.start subscriber;
    Unix.sleepf 0.1;
    
    (* Set up subscription *)
    let received_diffs = ref [] in
    let diff_handler diff =
      received_diffs := diff :: !received_diffs;
      Printf.printf "[Test] Received diff: seq=%Ld, page=%ld, size=%d\n%!" 
        diff.Messages.sequence diff.page_num (Bytes.length diff.data)
    in
    
    DistSQLiteZMQ.subscribe_database subscriber "/test/repl.sqlite" 0L diff_handler;
    Unix.sleepf 0.1; (* Let subscription establish *)
    
    (* Test individual writes *)
    let test_writes = [
      (1024, false);   (* Page write *)
      (512, true);     (* WAL write *)
      (4096, false);   (* Large page write *)
      (256, true);     (* Small WAL write *)
    ] in
    
    let (write_result, write_time) = TestUtils.measure_time (fun () ->
      List.iteri (fun i (size, is_wal) ->
        let data = TestUtils.create_test_data size in
        let offset = Int64.of_int (i * size) in
        DistSQLiteZMQ.handle_vfs_write publisher "/test/repl.sqlite" offset data is_wal 1L;
        Unix.sleepf 0.01 (* Small delay between writes *)
      ) test_writes;
      
      (* Process any pending writes *)
      DistSQLiteZMQ.process_pending_writes publisher;
      Unix.sleepf 0.2; (* Wait for replication *)
      
      List.length !received_diffs = List.length test_writes
    ) in
    
    TestUtils.print_test_result "WAL Replication" write_result write_time;
    Printf.printf "[Test] Sent: %d, Received: %d diffs\n%!" 
      (List.length test_writes) (List.length !received_diffs);
    
    (* Cleanup *)
    DistSQLiteZMQ.stop subscriber;
    DistSQLiteZMQ.stop publisher;
    
    write_result
  with
  | exn ->
      Printf.printf "[Test] WAL replication failed: %s\n%!" (Printexc.to_string exn);
      DistSQLiteZMQ.stop subscriber;
      DistSQLiteZMQ.stop publisher;
      false

(** Test bulk operations **)
let test_bulk_operations () =
  Printf.printf "\n=== Testing Bulk Operations ===\n%!";
  
  let endpoints = [Transport.create_endpoint "tcp" "localhost" (Some 5580)] in
  let config = DistSQLiteZMQ.default_config DistSQLiteZMQ.Coordinator "bulk_test" endpoints in
  let system = DistSQLiteZMQ.create config in
  
  try
    DistSQLiteZMQ.start system;
    Unix.sleepf 0.1;
    
    (* Generate bulk writes that should trigger different strategies *)
    let (bulk_result, bulk_time) = TestUtils.measure_time (fun () ->
      (* Small writes (should trigger incremental) *)
      for i = 0 to 4 do
        let data = TestUtils.create_test_data 1024 in
        DistSQLiteZMQ.handle_vfs_write system "/test/small.sqlite" (Int64.of_int (i * 1024)) data true 1L
      done;
      
      (* Medium batch (should trigger bulk) *)
      for i = 0 to 59 do
        let data = TestUtils.create_test_data 2048 in
        DistSQLiteZMQ.handle_vfs_write system "/test/medium.sqlite" (Int64.of_int (i * 2048)) data true 1L
      done;
      
      (* Large writes (should trigger rsync) *)
      for i = 0 to 2 do
        let data = TestUtils.create_test_data (5 * 1024 * 1024) in
        DistSQLiteZMQ.handle_vfs_write system "/test/large.sqlite" (Int64.of_int (i * 5 * 1024 * 1024)) data false 1L
      done;
      
      (* Process all pending writes *)
      DistSQLiteZMQ.process_pending_writes system;
      Unix.sleepf 0.3; (* Wait for processing *)
      
      true
    ) in
    
    TestUtils.print_test_result "Bulk Operations" bulk_result bulk_time;
    
    (* Print statistics *)
    let stats = DistSQLiteZMQ.get_statistics system in
    Printf.printf "[Test] Statistics:\n%!";
    List.iter (fun (key, value) ->
      Printf.printf "  %s: %s\n%!" key value
    ) stats;
    
    DistSQLiteZMQ.stop system;
    bulk_result
  with
  | exn ->
      Printf.printf "[Test] Bulk operations failed: %s\n%!" (Printexc.to_string exn);
      DistSQLiteZMQ.stop system;
      false

(** Test performance **)
let test_performance () =
  Printf.printf "\n=== Testing Performance ===\n%!";
  
  let endpoints = [Transport.create_endpoint "tcp" "localhost" (Some 5590)] in
  let config = DistSQLiteZMQ.default_config DistSQLiteZMQ.Coordinator "perf_test" endpoints in
  let system = DistSQLiteZMQ.create config in
  
  try
    DistSQLiteZMQ.start system;
    Unix.sleepf 0.1;
    
    let num_writes = 1000 in
    let write_size = 4096 in
    
    Printf.printf "[Test] Performance test: %d writes of %d bytes\n%!" num_writes write_size;
    
    let (perf_result, perf_time) = TestUtils.measure_time (fun () ->
      for i = 0 to num_writes - 1 do
        let data = TestUtils.create_test_data write_size in
        DistSQLiteZMQ.handle_vfs_write system "/test/perf.sqlite" 
          (Int64.of_int (i * write_size)) data (i mod 5 = 0) 1L
      done;
      
      DistSQLiteZMQ.process_pending_writes system;
      true
    ) in
    
    let writes_per_sec = float_of_int num_writes /. perf_time in
    let mb_per_sec = (float_of_int (num_writes * write_size)) /. (perf_time *. 1024.0 *. 1024.0) in
    
    TestUtils.print_test_result "Performance Test" perf_result perf_time;
    Printf.printf "[Test] Performance: %.2f writes/sec, %.2f MB/sec\n%!" writes_per_sec mb_per_sec;
    
    DistSQLiteZMQ.stop system;
    perf_result
  with
  | exn ->
      Printf.printf "[Test] Performance test failed: %s\n%!" (Printexc.to_string exn);
      DistSQLiteZMQ.stop system;
      false

(** Main test runner **)
let run_all_tests () =
  Random.self_init ();
  Printf.printf "DistSQLite ZMQ Integration Tests\n%!";
  Printf.printf "================================\n%!";
  
  let tests = [
    ("Lease Coordination", test_lease_coordination);
    ("WAL Replication", test_wal_replication);
    ("Bulk Operations", test_bulk_operations);
    ("Performance", test_performance);
  ] in
  
  let results = List.map (fun (name, test_fn) ->
    Printf.printf "\nRunning test: %s\n%!" name;
    let (result, time) = TestUtils.measure_time test_fn in
    (name, result, time)
  ) tests in
  
  Printf.printf "\n=== Test Summary ===\n%!";
  let passed = List.fold_left (fun acc (name, result, time) ->
    let status = if result then "PASS" else "FAIL" in
    Printf.printf "%s: %s (%.3fs)\n%!" name status time;
    if result then acc + 1 else acc
  ) 0 results in
  
  Printf.printf "\nPassed: %d/%d tests\n%!" passed (List.length tests);
  
  if passed = List.length tests then begin
    Printf.printf "🎉 All tests passed!\n%!";
    exit 0
  end else begin
    Printf.printf "❌ Some tests failed\n%!";
    exit 1
  end

(** Entry point **)
let () = run_all_tests ()