(** High-performance replication protocol implementation *)

open Bigarray

type strategy = 
  | TxLog        (* Transaction log shipping *)
  | BulkSync     (* Full rsync-style sync *)
  | Hybrid       (* Intelligent mix *)
  | PageDiff     (* Page-level binary diff *)

type write_event = {
  path: string;
  offset: int64;
  data: (char, int8_unsigned_elt, c_layout) Array1.t;
  is_wal: bool;
  fence_token: int64;
}

type write_batch = write_event array

(** Analyze write patterns to determine optimal replication strategy *)
let compute_strategy batch =
  if Array.length batch = 0 then TxLog
  else begin
    let total_size = Array.fold_left (fun acc event -> 
      acc + Array1.dim event.data
    ) 0 batch in
    
    let wal_count = Array.fold_left (fun acc event ->
      if event.is_wal then acc + 1 else acc
    ) 0 batch in
    
    let sequential_writes = ref 0 in
    if Array.length batch > 1 then begin
      for i = 1 to Array.length batch - 1 do
        let prev = batch.(i-1) in
        let curr = batch.(i) in
        if String.equal prev.path curr.path && 
           Int64.sub curr.offset prev.offset = Int64.of_int (Array1.dim prev.data) then
          incr sequential_writes
      done
    end;
    
    let sequential_ratio = float !sequential_writes /. float (Array.length batch) in
    let wal_ratio = float wal_count /. float (Array.length batch) in
    
    match total_size, sequential_ratio, wal_ratio with
    | size, _, _ when size > 1024 * 1024 -> BulkSync  (* >1MB: use bulk sync *)
    | _, seq, _ when seq > 0.8 -> PageDiff            (* Highly sequential: page diff *)
    | _, _, wal when wal > 0.5 -> TxLog               (* Mostly WAL: transaction log *)
    | _ -> Hybrid                                      (* Mixed: hybrid approach *)
  end

(** Efficient binary encoding of write batch using zero-copy serialization *)
let encode_write_batch batch =
  (* Calculate total size needed *)
  let header_size = 4 + 4 in (* magic + batch_size *)
  let total_size = Array.fold_left (fun acc event ->
    let path_len = String.length event.path in
    let event_size = 4 + path_len + 8 + 4 + 1 + 8 + Array1.dim event.data in
    acc + 4 + event_size (* 4 bytes for event size prefix *)
  ) header_size batch in
  
  let buffer = Array1.create char c_layout total_size in
  let pos = ref 0 in
  
  (* Write header *)
  let magic = Protocol_constants.dist_magic in
  Array1.set buffer (!pos + 0) (Char.chr (Int32.to_int (Int32.shift_right magic 24)));
  Array1.set buffer (!pos + 1) (Char.chr (Int32.to_int (Int32.shift_right magic 16) land 0xFF));
  Array1.set buffer (!pos + 2) (Char.chr (Int32.to_int (Int32.shift_right magic 8) land 0xFF));
  Array1.set buffer (!pos + 3) (Char.chr (Int32.to_int magic land 0xFF));
  pos := !pos + 4;
  
  let batch_size = Array.length batch in
  Array1.set buffer (!pos + 0) (Char.chr (batch_size lsr 24));
  Array1.set buffer (!pos + 1) (Char.chr ((batch_size lsr 16) land 0xFF));
  Array1.set buffer (!pos + 2) (Char.chr ((batch_size lsr 8) land 0xFF));
  Array1.set buffer (!pos + 3) (Char.chr (batch_size land 0xFF));
  pos := !pos + 4;
  
  (* Write events *)
  Array.iter (fun event ->
    let path_len = String.length event.path in
    let data_len = Array1.dim event.data in
    let event_size = 4 + path_len + 8 + 4 + 1 + 8 + data_len in
    
    (* Event size *)
    Array1.set buffer (!pos + 0) (Char.chr (event_size lsr 24));
    Array1.set buffer (!pos + 1) (Char.chr ((event_size lsr 16) land 0xFF));
    Array1.set buffer (!pos + 2) (Char.chr ((event_size lsr 8) land 0xFF));
    Array1.set buffer (!pos + 3) (Char.chr (event_size land 0xFF));
    pos := !pos + 4;
    
    (* Path length *)
    Array1.set buffer (!pos + 0) (Char.chr (path_len lsr 24));
    Array1.set buffer (!pos + 1) (Char.chr ((path_len lsr 16) land 0xFF));
    Array1.set buffer (!pos + 2) (Char.chr ((path_len lsr 8) land 0xFF));
    Array1.set buffer (!pos + 3) (Char.chr (path_len land 0xFF));
    pos := !pos + 4;
    
    (* Path *)
    String.iteri (fun i c ->
      Array1.set buffer (!pos + i) c
    ) event.path;
    pos := !pos + path_len;
    
    (* Offset (8 bytes) *)
    let offset = event.offset in
    for i = 7 downto 0 do
      Array1.set buffer (!pos + i) (Char.chr (Int64.to_int (Int64.shift_right offset (i * 8)) land 0xFF));
    done;
    pos := !pos + 8;
    
    (* Data length *)
    Array1.set buffer (!pos + 0) (Char.chr (data_len lsr 24));
    Array1.set buffer (!pos + 1) (Char.chr ((data_len lsr 16) land 0xFF));
    Array1.set buffer (!pos + 2) (Char.chr ((data_len lsr 8) land 0xFF));
    Array1.set buffer (!pos + 3) (Char.chr (data_len land 0xFF));
    pos := !pos + 4;
    
    (* Flags (is_wal) *)
    Array1.set buffer !pos (if event.is_wal then '\001' else '\000');
    pos := !pos + 1;
    
    (* Fence token (8 bytes) *)
    let fence = event.fence_token in
    for i = 7 downto 0 do
      Array1.set buffer (!pos + i) (Char.chr (Int64.to_int (Int64.shift_right fence (i * 8)) land 0xFF));
    done;
    pos := !pos + 8;
    
    (* Data (zero-copy) *)
    for i = 0 to data_len - 1 do
      Array1.set buffer (!pos + i) (Array1.get event.data i);
    done;
    pos := !pos + data_len;
  ) batch;
  
  buffer

(** sqlite3_rsync protocol implementation *)
module RsyncProtocol = struct
  type message =
    (* Origin (source) messages *)
    | OriginBegin of { protocol: int; page_size: int; page_count: int }
    | OriginEnd
    | OriginError of string
    | OriginPage of { 
        page_num: int; 
        data: (char, int8_unsigned_elt, c_layout) Array1.t 
      }
    | OriginTxn of { page_count: int }
    | OriginMsg of string
    (* Replica (destination) messages *)
    | ReplicaBegin of { protocol: int }
    | ReplicaEnd
    | ReplicaError of string
    | ReplicaHash of { page_num: int; hash: Binary_diff.page_hash }
    | ReplicaReady
    | ReplicaMsg of string
  
  (** Message type constants matching sqlite3_rsync *)
  let origin_begin_type = 0x41
  let origin_end_type = 0x42
  let origin_error_type = Protocol_constants.origin_error_type
  let origin_page_type = 0x44
  let origin_txn_type = 0x45
  let origin_msg_type = Protocol_constants.origin_msg_type
  
  let replica_begin_type = 0x61
  let replica_error_type = Protocol_constants.replica_error_type
  let replica_end_type = 0x63
  let replica_hash_type = Protocol_constants.replica_hash_type
  let replica_ready_type = 0x65
  let replica_msg_type = Protocol_constants.replica_msg_type
  
  (** Utility functions for binary encoding *)
  let write_uint32 buf offset value =
    Array1.set buf (offset + 0) (Char.chr ((value lsr 24) land 0xFF));
    Array1.set buf (offset + 1) (Char.chr ((value lsr 16) land 0xFF));
    Array1.set buf (offset + 2) (Char.chr ((value lsr 8) land 0xFF));
    Array1.set buf (offset + 3) (Char.chr (value land 0xFF))
  
  let write_string buf offset str =
    String.iteri (fun i c -> Array1.set buf (offset + i) c) str
  
  let encode message =
    match message with
    | OriginBegin { protocol; page_size; page_count } ->
        let buffer = Array1.create char c_layout 7 in
        Array1.set buffer 0 (Char.chr origin_begin_type);
        Array1.set buffer 1 (Char.chr protocol);
        (* Page size as power of 2 (e.g., 4096 = 2^12) *)
        let page_size_pow2 = 
          let rec find_pow2 n acc = 
            if n = 1 then acc else find_pow2 (n lsr 1) (acc + 1)
          in find_pow2 page_size 0
        in
        Array1.set buffer 2 (Char.chr page_size_pow2);
        write_uint32 buffer 3 page_count;
        buffer
        
    | OriginEnd ->
        let buffer = Array1.create char c_layout 1 in
        Array1.set buffer 0 (Char.chr origin_end_type);
        buffer
        
    | OriginError msg ->
        let msg_len = String.length msg in
        let buffer = Array1.create char c_layout (5 + msg_len) in
        Array1.set buffer 0 (Char.chr origin_error_type);
        write_uint32 buffer 1 msg_len;
        write_string buffer 5 msg;
        buffer
        
    | OriginMsg msg ->
        let msg_len = String.length msg in
        let buffer = Array1.create char c_layout (5 + msg_len) in
        Array1.set buffer 0 (Char.chr origin_msg_type);
        write_uint32 buffer 1 msg_len;
        write_string buffer 5 msg;
        buffer
        
    | OriginPage { page_num; data } ->
        let data_len = Array1.dim data in
        let buffer = Array1.create char c_layout (5 + data_len) in
        Array1.set buffer 0 (Char.chr origin_page_type);
        write_uint32 buffer 1 page_num;
        for i = 0 to data_len - 1 do
          Array1.set buffer (5 + i) (Array1.get data i);
        done;
        buffer
        
    | OriginTxn { page_count } ->
        let buffer = Array1.create char c_layout 5 in
        Array1.set buffer 0 (Char.chr origin_txn_type);
        write_uint32 buffer 1 page_count;
        buffer
        
    | ReplicaBegin { protocol } ->
        let buffer = Array1.create char c_layout 2 in
        Array1.set buffer 0 (Char.chr replica_begin_type);
        Array1.set buffer 1 (Char.chr protocol);
        buffer
        
    | ReplicaEnd ->
        let buffer = Array1.create char c_layout 1 in
        Array1.set buffer 0 (Char.chr replica_end_type);
        buffer
        
    | ReplicaError msg ->
        let msg_len = String.length msg in
        let buffer = Array1.create char c_layout (5 + msg_len) in
        Array1.set buffer 0 (Char.chr replica_error_type);
        write_uint32 buffer 1 msg_len;
        write_string buffer 5 msg;
        buffer
        
    | ReplicaMsg msg ->
        let msg_len = String.length msg in
        let buffer = Array1.create char c_layout (5 + msg_len) in
        Array1.set buffer 0 (Char.chr replica_msg_type);
        write_uint32 buffer 1 msg_len;
        write_string buffer 5 msg;
        buffer
        
    | ReplicaHash { page_num = _; hash } ->
        let buffer = Array1.create char c_layout 21 in (* 1 + 20 for hash *)
        Array1.set buffer 0 (Char.chr replica_hash_type);
        (* Hash is fixed 20 bytes *)
        write_string buffer 1 hash;
        buffer
        
    | ReplicaReady ->
        let buffer = Array1.create char c_layout 1 in
        Array1.set buffer 0 (Char.chr replica_ready_type);
        buffer
  
  (** Utility functions for binary decoding *)
  let read_uint32 buf offset =
    (Char.code (Array1.get buf (offset + 0)) lsl 24) lor
    (Char.code (Array1.get buf (offset + 1)) lsl 16) lor
    (Char.code (Array1.get buf (offset + 2)) lsl 8) lor
    (Char.code (Array1.get buf (offset + 3)))
  
  let read_string buf offset len =
    String.init len (fun i -> Array1.get buf (offset + i))
  
  let decode buffer =
    let buffer_len = Array1.dim buffer in
    if buffer_len < 1 then
      Error "Buffer too small for message type"
    else begin
      let msg_type = Char.code (Array1.get buffer 0) in
      
      match msg_type with
      | t when t = origin_begin_type -> (* OriginBegin *)
          if buffer_len < 7 then
            Error "Buffer too small for OriginBegin message"
          else begin
            let protocol = Char.code (Array1.get buffer 1) in
            let page_size_pow2 = Char.code (Array1.get buffer 2) in
            let page_size = 1 lsl page_size_pow2 in
            let page_count = read_uint32 buffer 3 in
            Ok (OriginBegin { protocol; page_size; page_count })
          end
          
      | t when t = origin_end_type -> Ok OriginEnd
      
      | t when t = origin_error_type || t = origin_msg_type ->
          if buffer_len < 5 then
            Error "Buffer too small for message with string"
          else begin
            let msg_len = read_uint32 buffer 1 in
            if buffer_len < 5 + msg_len then
              Error "Buffer too small for message string data"
            else begin
                           let msg = read_string buffer 5 msg_len in
             Ok (if t = origin_error_type then OriginError msg else OriginMsg msg)
           end
         end
         
      | t when t = origin_page_type -> (* OriginPage *)
          if buffer_len < 5 then
            Error "Buffer too small for OriginPage header"
          else begin
            let page_num = read_uint32 buffer 1 in
            let data_len = buffer_len - 5 in
            let data = Array1.sub buffer 5 data_len in
            Ok (OriginPage { page_num; data })
          end
          
      | t when t = origin_txn_type -> (* OriginTxn *)
          if buffer_len < 5 then
            Error "Buffer too small for OriginTxn message"
          else begin
            let page_count = read_uint32 buffer 1 in
            Ok (OriginTxn { page_count })
          end
          
      | t when t = replica_begin_type -> (* ReplicaBegin *)
          if buffer_len < 2 then
            Error "Buffer too small for ReplicaBegin message"
          else begin
            let protocol = Char.code (Array1.get buffer 1) in
            Ok (ReplicaBegin { protocol })
          end
          
      | t when t = replica_end_type -> Ok ReplicaEnd
      | t when t = replica_ready_type -> Ok ReplicaReady
      
      | t when t = replica_error_type || t = replica_msg_type ->
          if buffer_len < 5 then
            Error "Buffer too small for replica message with string"
          else begin
            let msg_len = read_uint32 buffer 1 in
            if buffer_len < 5 + msg_len then
              Error "Buffer too small for replica message string data"
            else begin
              let msg = read_string buffer 5 msg_len in
              Ok (if t = replica_error_type then ReplicaError msg else ReplicaMsg msg)
            end
          end
          
      | t when t = replica_hash_type -> (* ReplicaHash *)
          if buffer_len < 21 then
            Error "Buffer too small for ReplicaHash (need 20-byte hash)"
          else begin
            let hash = read_string buffer 1 20 in
            (* Note: Original protocol doesn't include page_num in ReplicaHash *)
            Ok (ReplicaHash { page_num = 0; hash })
          end
          
      | _ ->
          Error ("Unknown message type: " ^ string_of_int msg_type)
    end
end