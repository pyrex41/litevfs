(** Enhanced VFS with SQLite page interception for sqlite3_rsync compatibility *)

open Bigarray

type page_num = int
type page_data = (char, int8_unsigned_elt, c_layout) Array1.t
type page_hash = string

(** Page tracking state *)
module PageTracker = struct
  type page_state = 
    | Clean of page_hash
    | Dirty of page_data
    | New of page_data
  
  type t = {
    page_size: int;
    pages: (page_num, page_state) Hashtbl.t;
    dirty_pages: page_num Queue.t;
    mutable total_pages: int;
  }
  
  let create page_size = {
    page_size;
    pages = Hashtbl.create 10000;
    dirty_pages = Queue.create ();
    total_pages = 0;
  }
  
  let mark_page_dirty tracker page_num data =
    let old_state = Hashtbl.find_opt tracker.pages page_num in
    match old_state with
    | None | Some (Clean _) ->
        Hashtbl.replace tracker.pages page_num (Dirty data);
        Queue.add page_num tracker.dirty_pages
    | Some (Dirty _) ->
        Hashtbl.replace tracker.pages page_num (Dirty data)
    | Some (New _) ->
        Hashtbl.replace tracker.pages page_num (New data)
  
  let get_page_hash tracker page_num =
    match Hashtbl.find_opt tracker.pages page_num with
    | Some (Clean hash) -> Some hash
    | Some (Dirty data) | Some (New data) ->
        let hash = Binary_diff.hash_page data 0 (Array1.dim data) in
        Some hash
    | None -> None
  
  let get_dirty_pages tracker =
    let result = ref [] in
    try
      while true do
        let page_num = Queue.take tracker.dirty_pages in
        result := page_num :: !result
      done;
      !result
    with Queue.Empty -> !result
end

(** External base VFS functions - implemented in C stubs *)
external vfs_base_read : Nativeint.t -> (char, int8_unsigned_elt, c_layout) Array1.t -> int64 -> int -> int = "caml_vfs_base_read"
external vfs_base_write : Nativeint.t -> (char, int8_unsigned_elt, c_layout) Array1.t -> int64 -> int -> int = "caml_vfs_base_write"
external vfs_base_close : Nativeint.t -> int = "caml_vfs_base_close"

(** VFS file handle with page tracking *)
type vfs_file = {
  base_file: Nativeint.t; (* Opaque pointer to base VFS file *)
  tracker: PageTracker.t;
  mutable intercept_enabled: bool;
  db_path: string;
}

(** SQLite VFS method implementations *)
let vfs_read file buffer offset amount =
  let page_num = Int64.to_int (Int64.div offset (Int64.of_int file.tracker.page_size)) in
  
  if file.intercept_enabled then begin
    (* Check if we have intercepted data *)
    match Hashtbl.find_opt file.tracker.pages page_num with
    | Some (Dirty data) | Some (New data) ->
        (* Use intercepted data *)
        let page_offset = Int64.to_int (Int64.rem offset (Int64.of_int file.tracker.page_size)) in
        let copy_len = min amount (Array1.dim data - page_offset) in
        for i = 0 to copy_len - 1 do
          Array1.set buffer i (Array1.get data (page_offset + i))
        done;
        copy_len
    | _ ->
        (* Fall back to base VFS *)
        vfs_base_read file.base_file buffer offset amount
  end else
    vfs_base_read file.base_file buffer offset amount

let vfs_write file buffer offset amount =
  let page_num = Int64.to_int (Int64.div offset (Int64.of_int file.tracker.page_size)) in
  let page_offset = Int64.to_int (Int64.rem offset (Int64.of_int file.tracker.page_size)) in
  
  if file.intercept_enabled then begin
    (* Get or create page data *)
    let page_data = 
      match Hashtbl.find_opt file.tracker.pages page_num with
      | Some (Dirty data) | Some (New data) -> 
          Array1.create char c_layout (Array1.dim data) |> fun new_data ->
          Array1.blit data new_data; new_data
      | Some (Clean _) | None ->
          (* Create new page, potentially reading existing data first *)
          let new_data = Array1.create char c_layout file.tracker.page_size in
          if page_offset > 0 then begin
            (* Need to preserve existing data before the write *)
            let temp_buffer = Array1.create char c_layout file.tracker.page_size in
            let page_start = Int64.mul (Int64.of_int page_num) (Int64.of_int file.tracker.page_size) in
            let read_amount = vfs_base_read file.base_file temp_buffer page_start file.tracker.page_size in
            if read_amount > 0 then
              Array1.blit temp_buffer new_data
          end;
          new_data
    in
    
    (* Update the page data *)
    for i = 0 to amount - 1 do
      if page_offset + i < Array1.dim page_data then
        Array1.set page_data (page_offset + i) (Array1.get buffer i)
    done;
    
    (* Mark page as dirty *)
    PageTracker.mark_page_dirty file.tracker page_num page_data
  end;
  
  (* Also write to actual file *)
  vfs_base_write file.base_file buffer offset amount

(** High-level sync integration *)
module SyncIntegration = struct
  (** Write event enhanced with page information *)
  type enhanced_write_event = {
    path: string;
    offset: int64;
    data: (char, int8_unsigned_elt, c_layout) Array1.t;
    is_wal: bool;
    fence_token: int64;
    page_num: int;
    page_hash: string;
  }
  
  (** Convert VFS write to enhanced write event *)
  let create_enhanced_event file offset data =
    let page_num = Int64.to_int (Int64.div offset (Int64.of_int file.tracker.page_size)) in
    let page_hash = match PageTracker.get_page_hash file.tracker page_num with
      | Some hash -> hash
      | None -> Binary_diff.hash_page data 0 (Array1.dim data)
    in
    {
      path = file.db_path;
      offset;
      data;
      is_wal = String.contains file.db_path 'w'; (* Simple WAL detection *)
      fence_token = 0L; (* Would be set by caller *)
      page_num;
      page_hash;
    }
  
  (** Get all pages that need syncing *)
  let get_sync_pages file =
    let dirty_pages = PageTracker.get_dirty_pages file.tracker in
    List.map (fun page_num ->
      match Hashtbl.find_opt file.tracker.pages page_num with
      | Some (Dirty data) | Some (New data) ->
          let hash = Binary_diff.hash_page data 0 (Array1.dim data) in
          (page_num, hash, data)
      | _ -> failwith "Page not found"
    ) dirty_pages
  
  (** Compare with replica hashes to determine what to send *)
  let compute_diff_pages local_pages replica_hashes =
    List.filter (fun (page_num, local_hash, _data) ->
      match List.assoc_opt page_num replica_hashes with
      | Some replica_hash -> not (String.equal local_hash replica_hash)
      | None -> true (* New page *)
    ) local_pages
end

(** OCaml callbacks for VFS integration *)
let () =
  Callback.register "vfs_intercept_read" vfs_read;
  Callback.register "vfs_intercept_write" vfs_write;
  Callback.register "create_page_tracker" PageTracker.create;
  Callback.register "get_sync_pages" SyncIntegration.get_sync_pages