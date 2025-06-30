(** Test suite for minimal C stubs *)

open Bigarray

let test_stubs_init () =
  Alcotest.(check bool) "stubs initialization" true (Vfs_intercept.stubs_init ())

let test_runtime_info () =
  let (name, version, active) = Vfs_intercept.get_runtime_info () in
  Alcotest.(check string) "runtime name" "OCaml Runtime" name;
  Alcotest.(check bool) "runtime active" true active;
  Printf.printf "OCaml version: %s\n" version

let test_bigarray_creation () =
  let test_data = "Hello, DistSQLite!" in
  let len = String.length test_data in
  
  (* Create a test buffer *)
  let buffer = Bytes.create len in
  Bytes.blit_string test_data 0 buffer 0 len;
  
  (* Create bigarray from pointer (simulated) *)
  let bigarray = Array1.create char c_layout len in
  for i = 0 to len - 1 do
    Array1.set bigarray i (Bytes.get buffer i)
  done;
  
  (* Verify data *)
  let result = Buffer.create len in
  for i = 0 to len - 1 do
    Buffer.add_char result (Array1.get bigarray i)
  done;
  
  Alcotest.(check string) "bigarray data" test_data (Buffer.contents result)

let test_safe_memcmp () =
  let data1 = Array1.create char c_layout 10 in
  let data2 = Array1.create char c_layout 10 in
  
  (* Fill with same data *)
  for i = 0 to 9 do
    Array1.set data1 i (char_of_int (i + 65)); (* 'A', 'B', 'C', ... *)
    Array1.set data2 i (char_of_int (i + 65));
  done;
  
  (* Test comparison *)
  let result1 = Vfs_intercept.safe_memcmp data1 0 data2 0 10 in
  Alcotest.(check int) "identical arrays" 0 result1;
  
  (* Modify one array *)
  Array1.set data2 5 'X';
  let result2 = Vfs_intercept.safe_memcmp data1 0 data2 0 10 in
  Alcotest.(check bool) "different arrays" true (result2 <> 0)

let test_safe_memcpy () =
  let src = Array1.create char c_layout 10 in
  let dst = Array1.create char c_layout 10 in
  
  (* Fill source *)
  for i = 0 to 9 do
    Array1.set src i (char_of_int (i + 97)); (* 'a', 'b', 'c', ... *)
    Array1.set dst i '0'; (* Initialize destination *)
  done;
  
  (* Copy data *)
  Vfs_intercept.safe_memcpy dst 0 src 0 10;
  
  (* Verify copy *)
  for i = 0 to 9 do
    let expected = char_of_int (i + 97) in
    let actual = Array1.get dst i in
    Alcotest.(check char) (Printf.sprintf "copied byte %d" i) expected actual
  done

let test_simple_hash () =
  let data = Array1.create char c_layout 10 in
  
  (* Fill with known data *)
  let test_string = "0123456789" in
  for i = 0 to 9 do
    Array1.set data i test_string.[i]
  done;
  
  (* Compute hash *)
  let hash1 = Vfs_intercept.simple_hash data 0 10 in
  let hash2 = Vfs_intercept.simple_hash data 0 10 in
  
  (* Hash should be consistent *)
  Alcotest.(check int32) "consistent hash" hash1 hash2;
  
  (* Different data should produce different hash *)
  Array1.set data 0 'X';
  let hash3 = Vfs_intercept.simple_hash data 0 10 in
  Alcotest.(check bool) "different hash for different data" true (hash1 <> hash3)

let test_bounds_checking () =
  let data = Array1.create char c_layout 5 in
  
  (* These should fail with bounds checking *)
  try
    ignore (Vfs_intercept.safe_memcmp data 0 data 0 10); (* len > array size *)
    Alcotest.fail "Should have failed bounds check"
  with
  | Failure msg when String.contains msg "bounds" -> 
    Alcotest.(check bool) "bounds check works" true true
  | e -> 
    Alcotest.fail ("Unexpected exception: " ^ Printexc.to_string e);
  
  try
    ignore (Vfs_intercept.safe_memcmp data 3 data 0 5); (* offset + len > size *)
    Alcotest.fail "Should have failed bounds check"
  with
  | Failure msg when String.contains msg "bounds" -> 
    Alcotest.(check bool) "offset bounds check works" true true
  | e -> 
    Alcotest.fail ("Unexpected exception: " ^ Printexc.to_string e)

let test_vfs_integration () =
  (* Test VFS initialization *)
  let success = Vfs_intercept.initialize () in
  Alcotest.(check bool) "VFS initialization" true success;
  
  (* Test configuration *)
  let config = Vfs_intercept.get_config () in
  Alcotest.(check bool) "VFS interception enabled" true config.enable_interception;
  
  (* Test stats *)
  let stats = Vfs_intercept.get_stats () in
  Alcotest.(check bool) "Initial stats valid" true (stats.open_files >= 0);
  
  (* Cleanup *)
  Vfs_intercept.cleanup ()

let test_ipc_integration () =
  let open Ipc_socket in
  
  (* Test server creation *)
  let config = { Server.default_config with socket_path = "/tmp/test_distsqlite.sock" } in
  let server = Server.create_server config in
  
  (* Add a simple message handler *)
  let received_messages = ref [] in
  Server.add_message_handler server (fun msg ->
    received_messages := msg :: !received_messages
  );
  
  (* Test client connection *)
  let client = Client.connect "/tmp/test_distsqlite.sock" in
  
  (* Send a test message *)
  let test_msg = StatusRequest in
  Client.send_message client test_msg;
  
  (* Cleanup *)
  Client.disconnect client;
  Server.stop_server server;
  
  Printf.printf "IPC test completed successfully\n"

(* Exception handling test *)
let test_error_handling () =
  let data = Array1.create char c_layout 5 in
  
  (* Test that exceptions are properly raised and can be caught *)
  (try
    Vfs_intercept.safe_memcmp data (-1) data 0 1; (* Negative offset *)
    Alcotest.fail "Should have raised exception for negative offset"
  with
  | Failure _ -> Alcotest.(check bool) "Exception handling works" true true
  | e -> Alcotest.fail ("Wrong exception: " ^ Printexc.to_string e));
  
  (* Test NULL pointer handling simulation *)
  (try
    ignore (Vfs_intercept.simple_hash data 0 (-1)); (* Negative length *)
    Alcotest.fail "Should have raised exception for negative length"
  with
  | Failure _ -> Alcotest.(check bool) "Negative length check works" true true
  | e -> Alcotest.fail ("Wrong exception: " ^ Printexc.to_string e))

let test_performance () =
  let size = 1024 * 1024 in (* 1MB *)
  let data1 = Array1.create char c_layout size in
  let data2 = Array1.create char c_layout size in
  
  (* Fill with test data *)
  for i = 0 to size - 1 do
    Array1.set data1 i (char_of_int (i mod 256));
    Array1.set data2 i (char_of_int ((i + 1) mod 256));
  done;
  
  let start_time = Unix.gettimeofday () in
  
  (* Perform operations *)
  for _ = 1 to 100 do
    ignore (Vfs_intercept.safe_memcmp data1 0 data2 0 1024);
    ignore (Vfs_intercept.simple_hash data1 0 1024);
  done;
  
  let end_time = Unix.gettimeofday () in
  let duration = end_time -. start_time in
  
  Printf.printf "Performance test: 100 iterations of 1KB ops took %.3f ms\n" (duration *. 1000.0);
  Alcotest.(check bool) "Performance reasonable" true (duration < 1.0) (* Should be well under 1 second *)

(* Test suite definition *)
let tests = [
  ("stubs_init", `Quick, test_stubs_init);
  ("runtime_info", `Quick, test_runtime_info);
  ("bigarray_creation", `Quick, test_bigarray_creation);
  ("safe_memcmp", `Quick, test_safe_memcmp);
  ("safe_memcpy", `Quick, test_safe_memcpy);
  ("simple_hash", `Quick, test_simple_hash);
  ("bounds_checking", `Quick, test_bounds_checking);
  ("vfs_integration", `Quick, test_vfs_integration);
  ("ipc_integration", `Slow, test_ipc_integration);
  ("error_handling", `Quick, test_error_handling);
  ("performance", `Slow, test_performance);
]

let () =
  Printf.printf "Running DistSQLite Minimal Stubs Test Suite\n";
  Printf.printf "==========================================\n";
  Alcotest.run "DistSQLite Minimal Stubs" [
    ("core", tests);
  ]