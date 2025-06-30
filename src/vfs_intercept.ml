(** VFS Interception Module
 * 
 * This module handles SQLite VFS interception purely in OCaml,
 * moving complex logic out of the fragile C FFI layer.
 *)

open Bigarray

(** VFS file handle *)
type file_handle = {
  path: string;
  fd: Unix.file_descr option;
  page_size: int;
  mutable intercept_enabled: bool;
  mutable write_queue: (string * int64 * (char, int8_unsigned_elt, c_layout) Array1.t * bool * int64) list;
  mutable total_writes: int64;
  mutable last_sync: float;
}

(** Write event for replication *)
type write_event = {
  path: string;
  offset: int64;
  data: (char, int8_unsigned_elt, c_layout) Array1.t;
  is_wal: bool;
  fence_token: int64;
}

(** VFS configuration *)
type config = {
  enable_interception: bool;
  batch_size: int;
  sync_threshold: float; (* seconds *)
  max_queue_size: int;
}

(** Global VFS state *)
module State = struct
  let files = Hashtbl.create 32
  let config = ref {
    enable_interception = true;
    batch_size = 100;
    sync_threshold = 0.1; (* 100ms *)
    max_queue_size = 1000;
  }
  let write_callbacks = ref []
  let batch_callbacks = ref []
  let sync_callbacks = ref []
end

(** Error types *)
type vfs_error = 
  | InvalidPath of string
  | FileNotFound of string
  | IOError of string * string
  | BufferOverflow of string
  | InterceptionDisabled

exception VFS_Error of vfs_error

let string_of_error = function
  | InvalidPath path -> Printf.sprintf "Invalid path: %s" path
  | FileNotFound path -> Printf.sprintf "File not found: %s" path
  | IOError (op, msg) -> Printf.sprintf "IO error in %s: %s" op msg
  | BufferOverflow msg -> Printf.sprintf "Buffer overflow: %s" msg
  | InterceptionDisabled -> "VFS interception is disabled"

(** Utility functions *)
let is_wal_file path =
  String.length path > 4 && 
  String.sub path (String.length path - 4) 4 = "-wal"

let is_shm_file path =
  String.length path > 4 && 
  String.sub path (String.length path - 4) 4 = "-shm"

let current_time_ms () = Unix.gettimeofday () *. 1000.0

let generate_fence_token () =
  Int64.of_float (current_time_ms () *. 1000.0)

(** File operations *)
let create_file_handle path page_size =
  {
    path;
    fd = None;
    page_size;
    intercept_enabled = !State.config.enable_interception;
    write_queue = [];
    total_writes = 0L;
    last_sync = current_time_ms ();
  }

let open_file path flags =
  let page_size = 4096 in (* Default SQLite page size *)
  let handle = create_file_handle path page_size in
  
  (* Try to open the underlying file *)
  (try
    let fd = Unix.openfile path flags 0o644 in
    handle.fd <- Some fd;
    Hashtbl.replace State.files path handle;
    Ok handle
  with
  | Unix.Unix_error (err, op, _) ->
    Error (IOError (op, Unix.error_message err))
  | e ->
    Error (IOError ("open", Printexc.to_string e)))

let close_file handle =
  (match handle.fd with
  | Some fd -> 
    (try Unix.close fd with _ -> ());
    handle.fd <- None
  | None -> ());
  
  (* Process any remaining writes *)
  if handle.write_queue <> [] then
    flush_writes handle;
  
  Hashtbl.remove State.files handle.path

(** Write interception *)
and intercept_write handle offset data is_wal =
  if not handle.intercept_enabled then
    raise (VFS_Error InterceptionDisabled);
  
  let fence_token = generate_fence_token () in
  let data_copy = Array1.create char c_layout (Array1.dim data) in
  Array1.blit data data_copy;
  
  (* Add to write queue *)
  let write_entry = (handle.path, offset, data_copy, is_wal, fence_token) in
  handle.write_queue <- write_entry :: handle.write_queue;
  handle.total_writes <- Int64.succ handle.total_writes;
  
  (* Check if we should flush *)
  let should_flush = 
    List.length handle.write_queue >= !State.config.batch_size ||
    (current_time_ms () -. handle.last_sync) > (!State.config.sync_threshold *. 1000.0) in
  
  if should_flush then
    flush_writes handle;
  
  Ok fence_token

(** Flush queued writes *)
and flush_writes handle =
  if handle.write_queue = [] then
    ()
  else begin
    let writes = List.rev handle.write_queue in
    handle.write_queue <- [];
    handle.last_sync <- current_time_ms ();
    
    (* Convert to write events *)
    let events = List.map (fun (path, offset, data, is_wal, token) ->
      { path; offset; data; is_wal; fence_token = token }
    ) writes in
    
    (* Notify callbacks *)
    List.iter (fun callback ->
      try callback events
      with e -> 
        Printf.eprintf "VFS write callback error: %s\n" (Printexc.to_string e)
    ) !State.write_callbacks;
    
    (* Notify batch completion *)
    List.iter (fun callback ->
      try callback handle.path (List.length events)
      with e ->
        Printf.eprintf "VFS batch callback error: %s\n" (Printexc.to_string e)
    ) !State.batch_callbacks
  end

(** Read operation (passthrough with optional interception) *)
let intercept_read handle offset amount =
  match handle.fd with
  | None -> Error (FileNotFound handle.path)
  | Some fd ->
    try
      let _ = Unix.lseek fd (Int64.to_int offset) Unix.SEEK_SET in
      let buffer = Bytes.create amount in
      let bytes_read = Unix.read fd buffer 0 amount in
      
      (* Create bigarray from read data *)
      let data = Array1.create char c_layout bytes_read in
      for i = 0 to bytes_read - 1 do
        Array1.set data i (Bytes.get buffer i)
      done;
      
      Ok data
    with
    | Unix.Unix_error (err, op, _) -> 
      Error (IOError (op, Unix.error_message err))
    | e -> 
      Error (IOError ("read", Printexc.to_string e))

(** Sync operation *)
let sync_file handle flags =
  (* First flush any pending writes *)
  flush_writes handle;
  
  (* Then sync the underlying file *)
  match handle.fd with
  | None -> Error (FileNotFound handle.path)
  | Some fd ->
    try
      Unix.fsync fd;
      
      (* Notify sync callbacks *)
      List.iter (fun callback ->
        try callback handle.path flags
        with e ->
          Printf.eprintf "VFS sync callback error: %s\n" (Printexc.to_string e)
      ) !State.sync_callbacks;
      
      Ok ()
    with
    | Unix.Unix_error (err, op, _) -> 
      Error (IOError (op, Unix.error_message err))
    | e -> 
      Error (IOError ("sync", Printexc.to_string e))

(** Truncate operation *)
let truncate_file handle size =
  (* Flush writes first *)
  flush_writes handle;
  
  match handle.fd with
  | None -> Error (FileNotFound handle.path)
  | Some fd ->
    try
      Unix.ftruncate fd (Int64.to_int size);
      Ok ()
    with
    | Unix.Unix_error (err, op, _) -> 
      Error (IOError (op, Unix.error_message err))
    | e -> 
      Error (IOError ("truncate", Printexc.to_string e))

(** File size *)
let file_size handle =
  match handle.fd with
  | None -> Error (FileNotFound handle.path)
  | Some fd ->
    try
      let stats = Unix.fstat fd in
      Ok (Int64.of_int stats.st_size)
    with
    | Unix.Unix_error (err, op, _) -> 
      Error (IOError (op, Unix.error_message err))
    | e -> 
      Error (IOError ("stat", Printexc.to_string e))

(** Configuration *)
let set_config new_config =
  State.config := new_config

let get_config () = !State.config

(** Callback registration *)
let register_write_callback callback =
  State.write_callbacks := callback :: !State.write_callbacks

let register_batch_callback callback =
  State.batch_callbacks := callback :: !State.batch_callbacks

let register_sync_callback callback =
  State.sync_callbacks := callback :: !State.sync_callbacks

let clear_callbacks () =
  State.write_callbacks := [];
  State.batch_callbacks := [];
  State.sync_callbacks := []

(** Statistics *)
type stats = {
  open_files: int;
  total_writes: int64;
  pending_writes: int;
  avg_batch_size: float;
}

let get_stats () =
  let files = Hashtbl.fold (fun _ handle acc -> handle :: acc) State.files [] in
  let total_writes = List.fold_left (fun acc h -> Int64.add acc h.total_writes) 0L files in
  let pending_writes = List.fold_left (fun acc h -> acc + List.length h.write_queue) 0 files in
  let open_files = List.length files in
  
  {
    open_files;
    total_writes;
    pending_writes;
    avg_batch_size = if open_files > 0 then float_of_int pending_writes /. float_of_int open_files else 0.0;
  }

(** Initialize VFS interception *)
let initialize () =
  Printf.printf "[VFS] OCaml VFS interception initialized\n%!";
  Hashtbl.clear State.files;
  clear_callbacks ();
  true

(** Cleanup *)
let cleanup () =
  (* Close all open files and flush writes *)
  Hashtbl.iter (fun _ handle ->
    flush_writes handle;
    close_file handle
  ) State.files;
  
  Hashtbl.clear State.files;
  clear_callbacks ();
  Printf.printf "[VFS] OCaml VFS interception cleaned up\n%!"

(** C-callable wrappers for minimal stubs *)
external write_event_of_c : nativeint -> write_event = "distcore_write_event_of_c"
external write_event_to_c : write_event -> nativeint -> unit = "distcore_write_event_to_c"
external create_bigarray_from_ptr : nativeint -> int -> (char, int8_unsigned_elt, c_layout) Array1.t = "distcore_create_bigarray_from_ptr"
external stubs_init : unit -> bool = "distcore_stubs_init"
external get_runtime_info : unit -> (string * string * bool) = "distcore_get_runtime_info"

(** Main API for external use *)
module API = struct
  type t = file_handle
  
  let open_file = open_file
  let close_file = close_file
  let intercept_write = intercept_write
  let intercept_read = intercept_read
  let sync_file = sync_file
  let truncate_file = truncate_file
  let file_size = file_size
  let flush_writes = flush_writes
  
  let set_config = set_config
  let get_config = get_config
  let get_stats = get_stats
  
  let register_write_callback = register_write_callback
  let register_batch_callback = register_batch_callback
  let register_sync_callback = register_sync_callback
  
  let initialize = initialize
  let cleanup = cleanup
  
  let is_wal_file = is_wal_file
  let is_shm_file = is_shm_file
end

(** Callback registration for C interop *)
let () =
  Callback.register "vfs_initialize" initialize;
  Callback.register "vfs_cleanup" cleanup;
  Callback.register "vfs_get_stats" get_stats;
  Callback.register "vfs_open_file" (fun path flags -> 
    match open_file path [Unix.O_RDWR; Unix.O_CREAT] with
    | Ok handle -> handle
    | Error err -> failwith (string_of_error err)
  );
  Callback.register "vfs_close_file" close_file;
  Callback.register "vfs_intercept_write" (fun handle offset data is_wal ->
    match intercept_write handle offset data is_wal with
    | Ok token -> token
    | Error err -> failwith (string_of_error err)
  )