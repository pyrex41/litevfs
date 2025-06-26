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
  let magic = 0x44495354l in (* "DIST" *)
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
    | OriginBegin of { protocol: int; page_size: int }
    | ReplicaHash of { page_num: int; hash: Binary_diff.page_hash }
    | OriginPage of { 
        page_num: int; 
        data: (char, int8_unsigned_elt, c_layout) Array1.t 
      }
    | OriginTxn of { generation: int64 }
  
  let encode message =
    match message with
    | OriginBegin { protocol; page_size } ->
        let buffer = Array1.create char c_layout 12 in
        (* Message type: 1 *)
        Array1.set buffer 0 '\001';
        Array1.set buffer 1 '\000';
        Array1.set buffer 2 '\000';
        Array1.set buffer 3 '\000';
        
        (* Protocol version *)
        Array1.set buffer 4 (Char.chr (protocol lsr 24));
        Array1.set buffer 5 (Char.chr ((protocol lsr 16) land 0xFF));
        Array1.set buffer 6 (Char.chr ((protocol lsr 8) land 0xFF));
        Array1.set buffer 7 (Char.chr (protocol land 0xFF));
        
        (* Page size *)
        Array1.set buffer 8 (Char.chr (page_size lsr 24));
        Array1.set buffer 9 (Char.chr ((page_size lsr 16) land 0xFF));
        Array1.set buffer 10 (Char.chr ((page_size lsr 8) land 0xFF));
        Array1.set buffer 11 (Char.chr (page_size land 0xFF));
        
        buffer
        
    | ReplicaHash { page_num; hash } ->
        let hash_len = String.length hash in
        let buffer = Array1.create char c_layout (12 + hash_len) in
        
        (* Message type: 2 *)
        Array1.set buffer 0 '\002';
        Array1.set buffer 1 '\000';
        Array1.set buffer 2 '\000';
        Array1.set buffer 3 '\000';
        
        (* Page number *)
        Array1.set buffer 4 (Char.chr (page_num lsr 24));
        Array1.set buffer 5 (Char.chr ((page_num lsr 16) land 0xFF));
        Array1.set buffer 6 (Char.chr ((page_num lsr 8) land 0xFF));
        Array1.set buffer 7 (Char.chr (page_num land 0xFF));
        
        (* Hash length *)
        Array1.set buffer 8 (Char.chr (hash_len lsr 24));
        Array1.set buffer 9 (Char.chr ((hash_len lsr 16) land 0xFF));
        Array1.set buffer 10 (Char.chr ((hash_len lsr 8) land 0xFF));
        Array1.set buffer 11 (Char.chr (hash_len land 0xFF));
        
        (* Hash data *)
        String.iteri (fun i c ->
          Array1.set buffer (12 + i) c
        ) hash;
        
        buffer
        
    | OriginPage { page_num; data } ->
        let data_len = Array1.dim data in
        let buffer = Array1.create char c_layout (12 + data_len) in
        
        (* Message type: 3 *)
        Array1.set buffer 0 '\003';
        Array1.set buffer 1 '\000';
        Array1.set buffer 2 '\000';
        Array1.set buffer 3 '\000';
        
        (* Page number *)
        Array1.set buffer 4 (Char.chr (page_num lsr 24));
        Array1.set buffer 5 (Char.chr ((page_num lsr 16) land 0xFF));
        Array1.set buffer 6 (Char.chr ((page_num lsr 8) land 0xFF));
        Array1.set buffer 7 (Char.chr (page_num land 0xFF));
        
        (* Data length *)
        Array1.set buffer 8 (Char.chr (data_len lsr 24));
        Array1.set buffer 9 (Char.chr ((data_len lsr 16) land 0xFF));
        Array1.set buffer 10 (Char.chr ((data_len lsr 8) land 0xFF));
        Array1.set buffer 11 (Char.chr (data_len land 0xFF));
        
        (* Page data (zero-copy) *)
        for i = 0 to data_len - 1 do
          Array1.set buffer (12 + i) (Array1.get data i);
        done;
        
        buffer
        
    | OriginTxn { generation } ->
        let buffer = Array1.create char c_layout 12 in
        
        (* Message type: 4 *)
        Array1.set buffer 0 '\004';
        Array1.set buffer 1 '\000';
        Array1.set buffer 2 '\000';
        Array1.set buffer 3 '\000';
        
        (* Generation (8 bytes) *)
        for i = 7 downto 0 do
          Array1.set buffer (4 + i) (Char.chr (Int64.to_int (Int64.shift_right generation (i * 8)) land 0xFF));
        done;
        
        buffer
  
  let decode buffer =
    let buffer_len = Array1.dim buffer in
    if buffer_len < 4 then
      Error "Buffer too small for message type"
    else begin
      let msg_type = 
        (Char.code (Array1.get buffer 0)) lor
        ((Char.code (Array1.get buffer 1)) lsl 8) lor
        ((Char.code (Array1.get buffer 2)) lsl 16) lor
        ((Char.code (Array1.get buffer 3)) lsl 24) in
      
      match msg_type with
      | 1 -> (* OriginBegin *)
          if buffer_len < 12 then
            Error "Buffer too small for OriginBegin message"
          else begin
            let protocol = 
              (Char.code (Array1.get buffer 4)) lor
              ((Char.code (Array1.get buffer 5)) lsl 8) lor
              ((Char.code (Array1.get buffer 6)) lsl 16) lor
              ((Char.code (Array1.get buffer 7)) lsl 24) in
            
            let page_size = 
              (Char.code (Array1.get buffer 8)) lor
              ((Char.code (Array1.get buffer 9)) lsl 8) lor
              ((Char.code (Array1.get buffer 10)) lsl 16) lor
              ((Char.code (Array1.get buffer 11)) lsl 24) in
            
            Ok (OriginBegin { protocol; page_size })
          end
          
      | 2 -> (* ReplicaHash *)
          if buffer_len < 12 then
            Error "Buffer too small for ReplicaHash header"
          else begin
            let page_num = 
              (Char.code (Array1.get buffer 4)) lor
              ((Char.code (Array1.get buffer 5)) lsl 8) lor
              ((Char.code (Array1.get buffer 6)) lsl 16) lor
              ((Char.code (Array1.get buffer 7)) lsl 24) in
            
            let hash_len = 
              (Char.code (Array1.get buffer 8)) lor
              ((Char.code (Array1.get buffer 9)) lsl 8) lor
              ((Char.code (Array1.get buffer 10)) lsl 16) lor
              ((Char.code (Array1.get buffer 11)) lsl 24) in
            
            if buffer_len < 12 + hash_len then
              Error "Buffer too small for ReplicaHash data"
            else begin
              let hash = String.init hash_len (fun i ->
                Array1.get buffer (12 + i)
              ) in
              Ok (ReplicaHash { page_num; hash })
            end
          end
          
      | 3 -> (* OriginPage *)
          if buffer_len < 12 then
            Error "Buffer too small for OriginPage header"
          else begin
            let page_num = 
              (Char.code (Array1.get buffer 4)) lor
              ((Char.code (Array1.get buffer 5)) lsl 8) lor
              ((Char.code (Array1.get buffer 6)) lsl 16) lor
              ((Char.code (Array1.get buffer 7)) lsl 24) in
            
            let data_len = 
              (Char.code (Array1.get buffer 8)) lor
              ((Char.code (Array1.get buffer 9)) lsl 8) lor
              ((Char.code (Array1.get buffer 10)) lsl 16) lor
              ((Char.code (Array1.get buffer 11)) lsl 24) in
            
            if buffer_len < 12 + data_len then
              Error "Buffer too small for OriginPage data"
            else begin
              let data = Array1.sub buffer 12 data_len in
              Ok (OriginPage { page_num; data })
            end
          end
          
      | 4 -> (* OriginTxn *)
          if buffer_len < 12 then
            Error "Buffer too small for OriginTxn message"
          else begin
            let generation = ref 0L in
            for i = 0 to 7 do
              let byte = Int64.of_int (Char.code (Array1.get buffer (4 + i))) in
              generation := Int64.logor !generation (Int64.shift_left byte (i * 8));
            done;
            Ok (OriginTxn { generation = !generation })
          end
          
      | _ ->
          Error ("Unknown message type: " ^ string_of_int msg_type)
    end
end