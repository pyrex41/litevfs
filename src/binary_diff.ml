(** High-performance binary diff engine for database replication *)

open Bigarray

type page_hash = string
type page_num = int

type diff_chunk = {
  offset: int;
  old_data: (char, int8_unsigned_elt, c_layout) Array1.t;
  new_data: (char, int8_unsigned_elt, c_layout) Array1.t;
}

(** Fast hash function for page content *)
let hash_page data offset page_size =
  let h = ref 0x9e3779b9l in
  for i = 0 to page_size - 1 do
    let byte = Char.code (Array1.get data (offset + i)) in
    h := Int32.add (Int32.mul !h 0x9e3779b9l) (Int32.of_int byte);
  done;
  Int32.to_string !h

(** Rolling hash implementation for rsync-like algorithm *)
module RollingHash = struct
  type t = {
    mutable hash: int32;
    mutable a: int32;
    mutable b: int32;
    window_size: int;
    prime: int32;
  }
  
  let create window_size =
    {
      hash = 0l;
      a = 0l;
      b = 0l;
      window_size;
      prime = 65521l; (* Largest prime less than 2^16 *)
    }
  
  let init rh data start_pos =
    rh.a <- 0l;
    rh.b <- 0l;
    for i = 0 to rh.window_size - 1 do
      let byte = Int32.of_int (Char.code (Array1.get data (start_pos + i))) in
      rh.a <- Int32.rem (Int32.add rh.a byte) rh.prime;
      rh.b <- Int32.rem (Int32.add rh.b (Int32.mul byte (Int32.of_int (rh.window_size - i)))) rh.prime;
    done;
    rh.hash <- Int32.logor (Int32.shift_left rh.b 16) rh.a
  
  let update rh old_byte new_byte =
    let old_val = Int32.of_int (Char.code old_byte) in
    let new_val = Int32.of_int (Char.code new_byte) in
    let window_size_l = Int32.of_int rh.window_size in
    
    rh.a <- Int32.rem (Int32.add (Int32.sub rh.a old_val) new_val) rh.prime;
    rh.b <- Int32.rem (Int32.add (Int32.sub rh.b (Int32.mul old_val window_size_l)) rh.a) rh.prime;
    rh.hash <- Int32.logor (Int32.shift_left rh.b 16) rh.a
  
  let get_hash rh = rh.hash
end

(** Compute rolling hash for entire data *)
let rolling_hash ~window_size data =
  let data_len = Array1.dim data in
  if data_len < window_size then
    [||]
  else begin
    let num_hashes = data_len - window_size + 1 in
    let hashes = Array.make num_hashes 0l in
    let rh = RollingHash.create window_size in
    
    (* Initialize first hash *)
    RollingHash.init rh data 0;
    hashes.(0) <- RollingHash.get_hash rh;
    
    (* Roll through the rest *)
    for i = 1 to num_hashes - 1 do
      let old_byte = Array1.get data (i - 1) in
      let new_byte = Array1.get data (i + window_size - 1) in
      RollingHash.update rh old_byte new_byte;
      hashes.(i) <- RollingHash.get_hash rh;
    done;
    
    hashes
  end

(** Find matching blocks between two data sets *)
let find_matching_blocks ~old_data ~new_data ~block_size =
  let old_len = Array1.dim old_data in
  let new_len = Array1.dim new_data in
  
  if old_len < block_size || new_len < block_size then
    []
  else begin
    (* Build hash table of old data blocks *)
    let old_hashes = Hashtbl.create (old_len / block_size) in
    
    for i = 0 to (old_len - block_size) / block_size do
      let offset = i * block_size in
      let hash = hash_page old_data offset block_size in
      Hashtbl.add old_hashes hash offset;
    done;
    
    (* Scan new data for matches *)
    let matches = ref [] in
    for i = 0 to (new_len - block_size) / block_size do
      let offset = i * block_size in
      let hash = hash_page new_data offset block_size in
      
      match Hashtbl.find_opt old_hashes hash with
      | Some old_offset ->
          (* Verify actual content matches (avoid hash collisions) *)
          let match_found = ref true in
          for j = 0 to block_size - 1 do
            if Array1.get old_data (old_offset + j) <> Array1.get new_data (offset + j) then
              match_found := false
          done;
          
          if !match_found then
            matches := (old_offset, offset, block_size) :: !matches
      | None -> ()
    done;
    
    !matches
  end

(** Compute page-level diff for SQLite databases *)
let compute_diff ~old_db ~new_db ~page_size =
  let old_pages = Array1.dim old_db / page_size in
  let new_pages = Array1.dim new_db / page_size in
  let max_pages = max old_pages new_pages in
  
  let diffs = ref [] in
  
  for page_num = 0 to max_pages - 1 do
    let old_offset = page_num * page_size in
    let new_offset = page_num * page_size in
    
    let old_exists = old_offset + page_size <= Array1.dim old_db in
    let new_exists = new_offset + page_size <= Array1.dim new_db in
    
    match old_exists, new_exists with
    | true, true ->
        (* Both pages exist, compare them *)
        let pages_match = ref true in
        for i = 0 to page_size - 1 do
          if Array1.get old_db (old_offset + i) <> Array1.get new_db (new_offset + i) then
            pages_match := false
        done;
        
        if not !pages_match then begin
          (* Find sub-page differences *)
          let chunk_size = 64 in (* 64-byte chunks *)
          let chunks = ref [] in
          
          for chunk_start = 0 to page_size - 1 do
            let chunk_end = min (chunk_start + chunk_size) page_size in
            let chunk_len = chunk_end - chunk_start in
            
            if chunk_len > 0 then begin
              let chunk_differs = ref false in
              for i = chunk_start to chunk_end - 1 do
                if Array1.get old_db (old_offset + i) <> Array1.get new_db (new_offset + i) then
                  chunk_differs := true
              done;
              
              if !chunk_differs then begin
                let old_chunk = Array1.sub old_db old_offset chunk_len in
                let new_chunk = Array1.sub new_db new_offset chunk_len in
                
                let diff_chunk = {
                  offset = chunk_start;
                  old_data = old_chunk;
                  new_data = new_chunk;
                } in
                chunks := diff_chunk :: !chunks
              end
            end
          done;
          
          if !chunks <> [] then
            diffs := (page_num, List.rev !chunks) :: !diffs
        end
        
    | false, true ->
        (* New page added *)
        let new_page = Array1.sub new_db new_offset page_size in
        let empty_page = Array1.create char c_layout 0 in
        
        let diff_chunk = {
          offset = 0;
          old_data = empty_page;
          new_data = new_page;
        } in
        diffs := (page_num, [diff_chunk]) :: !diffs
        
    | true, false ->
        (* Old page deleted *)
        let old_page = Array1.sub old_db old_offset page_size in
        let empty_page = Array1.create char c_layout 0 in
        
        let diff_chunk = {
          offset = 0;
          old_data = old_page;
          new_data = empty_page;
        } in
        diffs := (page_num, [diff_chunk]) :: !diffs
        
    | false, false ->
        (* Neither page exists - skip *)
        ()
  done;
  
  List.rev !diffs

(** Efficient in-place diff using rsync algorithm *)
let rsync_diff ~old_data ~new_data ~block_size =
  let old_len = Array1.dim old_data in
  let new_len = Array1.dim new_data in
  let window_size = min block_size 64 in
  
  (* Build signature of old data *)
  let old_hashes = rolling_hash ~window_size old_data in
  let signature_table = Hashtbl.create (Array.length old_hashes) in
  
  Array.iteri (fun i hash ->
    Hashtbl.add signature_table hash i
  ) old_hashes;
  
  (* Scan new data for matches *)
  let matches = ref [] in
  let new_hashes = rolling_hash ~window_size new_data in
  
  Array.iteri (fun new_pos hash ->
    match Hashtbl.find_opt signature_table hash with
    | Some old_pos ->
        (* Verify match *)
        let match_len = ref 0 in
        let max_match = min (old_len - old_pos) (new_len - new_pos) in
        
        while !match_len < max_match &&
              Array1.get old_data (old_pos + !match_len) = Array1.get new_data (new_pos + !match_len) do
          incr match_len
        done;
        
        if !match_len >= window_size then
          matches := (old_pos, new_pos, !match_len) :: !matches
    | None -> ()
  ) new_hashes;
  
  List.rev !matches