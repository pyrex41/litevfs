(* Build Configuration for DistSQLite *)

(* CPU feature detection flags *)
let has_avx2 = ref None
let has_sse2 = ref None

(* Build flags *)
let enable_simd = ref true
let enable_logging = ref true
let debug_mode = ref false

(* Performance tuning *)
let default_queue_capacity = 1000
let default_batch_size = 100
let max_concurrent_operations = 64

(* Platform detection *)
let is_linux = Sys.os_type = "Unix" && Sys.file_exists "/proc/version"
let is_macos = Sys.os_type = "Unix" && not is_linux
let is_windows = Sys.os_type = "Win32"

(* Feature availability *)
let supports_mmap = is_linux || is_macos
let supports_sendfile = is_linux
let supports_kqueue = is_macos
let supports_epoll = is_linux

(* Build-time configuration *)
let configure () =
  (* Detect CPU features at runtime *)
  has_avx2 := Some (try
    let _ = Sys.command "grep -q avx2 /proc/cpuinfo 2>/dev/null" in
    true
  with _ -> false);
  
  has_sse2 := Some (try
    let _ = Sys.command "grep -q sse2 /proc/cpuinfo 2>/dev/null" in
    true
  with _ -> false);
  
  (* Set SIMD availability based on CPU features *)
  enable_simd := (match !has_avx2, !has_sse2 with
    | Some true, _ -> true
    | _, Some true -> true  (* Fallback to SSE2 *)
    | _ -> false)

(* Get feature availability *)
let get_avx2_support () = 
  match !has_avx2 with
  | Some b -> b
  | None -> configure (); Option.value !has_avx2 ~default:false

let get_sse2_support () = 
  match !has_sse2 with
  | Some b -> b
  | None -> configure (); Option.value !has_sse2 ~default:false

let get_simd_support () = !enable_simd

(* Logging configuration *)
let set_debug_mode enabled = debug_mode := enabled
let set_logging enabled = enable_logging := enabled

let is_debug_mode () = !debug_mode
let is_logging_enabled () = !enable_logging