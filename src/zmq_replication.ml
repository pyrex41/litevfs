(** ZMQ-based WAL Replication Manager - Pure OCaml Implementation **)

open Zmq
open Zmq_transport

(** WAL diff streaming and replication **)
module ReplicationManager = struct
  open Messages

  type replication_stats = {
    mutable diffs_published: int64;
    mutable diffs_received: int64;
    mutable batches_published: int64;
    mutable batches_received: int64;
    mutable bytes_replicated: int64;
    mutable last_sequence: int64;
  }

  type subscription = {
    db_path: string;
    start_sequence: int64;
    handler: wal_diff -> unit;
  }

  type t = {
    transport: Transport.t;
    mutable sequence_counter: int64;
    stats: replication_stats;
    mutable subscriptions: subscription list;
    mutable running: bool;
  }

  let create transport = {
    transport;
    sequence_counter = 1L;
    stats = {
      diffs_published = 0L;
      diffs_received = 0L;
      batches_published = 0L;
      batches_received = 0L;
      bytes_replicated = 0L;
      last_sequence = 0L;
    };
    subscriptions = [];
    running = false;
  }

  (** Create WAL diff from VFS write event **)
  let create_wal_diff manager db_path page_num data operation =
    let sequence = manager.sequence_counter in
    manager.sequence_counter <- Int64.add manager.sequence_counter 1L;
    
    {
      db_path;
      sequence;
      page_num;
      data;
      operation;
      timestamp = Unix.time () |> Int64.of_float;
    }

  (** Publish individual WAL diff **)
  let publish_wal_diff manager diff =
    match manager.transport.sockets.diff_pub with
    | None -> failwith "No diff publisher socket available"
    | Some socket ->
        let topic = diff.db_path in
        let serialized = serialize_wal_diff diff in
        
        (* Send topic frame *)
        Socket.send ~block:false ~more:true socket topic;
        (* Send data frame *)
        Socket.send ~block:false socket (Bytes.to_string serialized);
        
        manager.stats.diffs_published <- Int64.add manager.stats.diffs_published 1L;
        manager.stats.bytes_replicated <- Int64.add manager.stats.bytes_replicated (Int64.of_int (Bytes.length data));
        manager.stats.last_sequence <- diff.sequence;
        
        Printf.printf "[Replication] Published diff for %s (seq=%Ld, page=%ld)\n%!"
          diff.db_path diff.sequence diff.page_num

  (** Publish batch of WAL diffs **)
  let publish_wal_batch manager batch =
    match manager.transport.sockets.diff_pub with
    | None -> failwith "No diff publisher socket available"
    | Some socket ->
        let topic = batch.db_path ^ ":batch" in
        let serialized = serialize_wal_batch batch in
        
        (* Send topic frame *)
        Socket.send ~block:false ~more:true socket topic;
        (* Send data frame *)
        Socket.send ~block:false socket (Bytes.to_string serialized);
        
        manager.stats.batches_published <- Int64.add manager.stats.batches_published 1L;
        let batch_bytes = List.fold_left (fun acc diff -> 
          Int64.add acc (Int64.of_int (Bytes.length diff.data))
        ) 0L batch.diffs in
        manager.stats.bytes_replicated <- Int64.add manager.stats.bytes_replicated batch_bytes;
        
        Printf.printf "[Replication] Published batch for %s (%d diffs)\n%!"
          batch.db_path (List.length batch.diffs)

  (** Subscribe to database changes **)
  let subscribe_database manager db_path start_sequence handler =
    let subscription = { db_path; start_sequence; handler } in
    manager.subscriptions <- subscription :: manager.subscriptions;
    
    (* Set subscription filter on socket *)
    (match manager.transport.sockets.diff_sub with
     | None -> failwith "No diff subscriber socket available"
     | Some socket ->
         Socket.subscribe socket db_path;
         Socket.subscribe socket (db_path ^ ":batch");
         Printf.printf "[Replication] Subscribed to %s (start_seq=%Ld)\n%!" 
           db_path start_sequence
    )

  (** Process received WAL diff **)
  let process_received_diff manager topic data =
    try
      if String.contains topic ':' then
        (* Batch message *)
        let batch = deserialize_wal_batch (Bytes.of_string data) in
        manager.stats.batches_received <- Int64.add manager.stats.batches_received 1L;
        
        Printf.printf "[Replication] Received batch for %s (%d diffs)\n%!"
          batch.db_path (List.length batch.diffs);
        
        (* Process each diff in batch *)
        List.iter (fun diff ->
          List.iter (fun sub ->
            if sub.db_path = batch.db_path && diff.sequence >= sub.start_sequence then
              sub.handler diff
          ) manager.subscriptions;
          manager.stats.diffs_received <- Int64.add manager.stats.diffs_received 1L
        ) batch.diffs
      else
        (* Individual diff *)
        let diff = deserialize_wal_diff (Bytes.of_string data) in
        manager.stats.diffs_received <- Int64.add manager.stats.diffs_received 1L;
        manager.stats.bytes_replicated <- Int64.add manager.stats.bytes_replicated (Int64.of_int (Bytes.length diff.data));
        
        Printf.printf "[Replication] Received diff for %s (seq=%Ld, page=%ld)\n%!"
          diff.db_path diff.sequence diff.page_num;
        
        (* Call relevant handlers *)
        List.iter (fun sub ->
          if sub.db_path = diff.db_path && diff.sequence >= sub.start_sequence then
            sub.handler diff
        ) manager.subscriptions
    with
    | exn ->
        Printf.printf "[Replication] Error processing diff: %s\n%!" (Printexc.to_string exn)

  (** Run subscriber service **)
  let run_subscriber_service manager =
    match manager.transport.sockets.diff_sub with
    | None -> failwith "No diff subscriber socket available"
    | Some socket ->
        Printf.printf "[Replication] Starting subscriber service\n%!";
        manager.running <- true;
        
        while manager.running do
          try
            (* Receive topic *)
            let topic = Socket.recv ~block:false socket in
            (* Receive data *)
            let data = Socket.recv ~block:false socket in
            
            process_received_diff manager topic data
            
          with
          | Zmq.ZMQ_exception (EAGAIN, _) ->
              (* No message available *)
              Unix.sleepf 0.001 (* 1ms *)
          | exn ->
              Printf.printf "[Replication] Error in subscriber: %s\n%!" (Printexc.to_string exn);
              Unix.sleepf 0.01 (* 10ms *)
        done

  (** Stop replication services **)
  let stop_replication manager =
    manager.running <- false;
    Printf.printf "[Replication] Stopped\n%!"

  (** Get replication statistics **)
  let get_stats manager = manager.stats
end

(** Integration with VFS write events **)
module VFSReplication = struct
  open Messages

  type write_strategy = 
    | Incremental (* Individual WAL diffs *)
    | Bulk of int (* Batched WAL diffs with count *)
    | Rsync of int64 (* Large sync with total bytes *)
    | None

  type vfs_write_event = {
    path: string;
    offset: int64;
    data: bytes;
    is_wal: bool;
    fence_token: int64;
    page_num: int64;
    timestamp: float;
    size: int;
  }

  type write_batch = vfs_write_event list

  type t = {
    repl_manager: ReplicationManager.t;
    mutable pending_writes: (string, write_batch) Hashtbl.t; (* db_path -> events *)
    mutable batch_timers: (string, float) Hashtbl.t; (* db_path -> last_batch_time *)
    batch_size: int;
    batch_timeout: float;
    mutable processing_enabled: bool;
  }

  let create repl_manager ?(batch_size=100) ?(batch_timeout=0.05) () = {
    repl_manager;
    pending_writes = Hashtbl.create 50;
    batch_timers = Hashtbl.create 50;
    batch_size;
    batch_timeout;
    processing_enabled = true;
  }

  (** Compute optimal replication strategy **)
  let compute_replication_strategy batch =
    let event_count = List.length batch in
    let total_bytes = List.fold_left (fun acc event -> acc + event.size) 0 batch in
    let wal_events = List.filter (fun e -> e.is_wal) batch |> List.length in
    
    (* Count unique pages *)
    let pages = Hashtbl.create event_count in
    List.iter (fun event -> Hashtbl.replace pages event.page_num ()) batch;
    let page_spread = Hashtbl.length pages in
    
    (* Decision logic matching Go implementation *)
    if total_bytes < 64 * 1024 then
      Incremental
    else if total_bytes > 10 * 1024 * 1024 then
      Rsync (Int64.of_int total_bytes)
    else if page_spread > 100 && (float_of_int wal_events) /. (float_of_int event_count) > 0.8 then
      Bulk event_count
    else if event_count > 50 then
      Bulk event_count
    else
      Incremental

  (** Execute replication strategy **)
  let execute_replication_strategy vfs_repl strategy batch =
    match strategy with
    | Incremental ->
        (* Send individual WAL diffs *)
        List.iter (fun event ->
          let operation = if event.is_wal then "wal_write" else "page_write" in
          let diff = ReplicationManager.create_wal_diff 
            vfs_repl.repl_manager 
            event.path 
            (Int32.of_int64 event.page_num)
            event.data 
            operation in
          ReplicationManager.publish_wal_diff vfs_repl.repl_manager diff
        ) batch;
        Printf.printf "[VFS Replication] Executed incremental strategy (%d events)\n%!" 
          (List.length batch)
        
    | Bulk count ->
        (* Group by database path and send batches *)
        let path_groups = Hashtbl.create 10 in
        List.iter (fun event ->
          let existing = Hashtbl.find_opt path_groups event.path |> Option.value ~default:[] in
          Hashtbl.replace path_groups event.path (event :: existing)
        ) batch;
        
        Hashtbl.iter (fun db_path events ->
          let diffs = List.map (fun event ->
            let operation = if event.is_wal then "wal_write" else "page_write" in
            ReplicationManager.create_wal_diff 
              vfs_repl.repl_manager 
              event.path 
              (Int32.of_int64 event.page_num)
              event.data 
              operation
          ) events in
          
          let batch = {
            db_path;
            diffs;
            batch_id = Printf.sprintf "batch_%f_%d" (Unix.time ()) (List.length diffs);
            timestamp = Unix.time () |> Int64.of_float;
          } in
          
          ReplicationManager.publish_wal_batch vfs_repl.repl_manager batch
        ) path_groups;
        
        Printf.printf "[VFS Replication] Executed bulk strategy (%d events)\n%!" count
        
    | Rsync total_bytes ->
        (* Log rsync trigger - actual rsync would be handled separately *)
        Printf.printf "[VFS Replication] Rsync strategy triggered (%Ld bytes)\n%!" total_bytes
        
    | None ->
        (* No replication needed *)
        ()

  (** Handle VFS write event **)
  let handle_vfs_write vfs_repl path offset data is_wal fence_token =
    if not vfs_repl.processing_enabled then () else
    
    let event = {
      path;
      offset;
      data;
      is_wal;
      fence_token;
      page_num = Int64.div offset 4096L; (* Assuming 4KB pages *)
      timestamp = Unix.time ();
      size = Bytes.length data;
    } in
    
    (* Add to pending batch *)
    let existing = Hashtbl.find_opt vfs_repl.pending_writes path |> Option.value ~default:[] in
    let updated = event :: existing in
    Hashtbl.replace vfs_repl.pending_writes path updated;
    Hashtbl.replace vfs_repl.batch_timers path (Unix.time ());
    
    (* Check if batch should be processed *)
    let should_process = 
      List.length updated >= vfs_repl.batch_size ||
      (match Hashtbl.find_opt vfs_repl.batch_timers path with
       | Some last_time -> (Unix.time () -. last_time) > vfs_repl.batch_timeout
       | None -> false)
    in
    
    if should_process then begin
      (* Remove from pending *)
      Hashtbl.remove vfs_repl.pending_writes path;
      Hashtbl.remove vfs_repl.batch_timers path;
      
      (* Process batch *)
      let strategy = compute_replication_strategy updated in
      execute_replication_strategy vfs_repl strategy updated
    end

  (** Process pending batches (called periodically) **)
  let process_pending_batches vfs_repl =
    let current_time = Unix.time () in
    let to_process = ref [] in
    
    (* Find batches that have timed out *)
    Hashtbl.iter (fun path last_time ->
      if current_time -. last_time > vfs_repl.batch_timeout then
        match Hashtbl.find_opt vfs_repl.pending_writes path with
        | Some batch when List.length batch > 0 ->
            to_process := (path, batch) :: !to_process
        | _ -> ()
    ) vfs_repl.batch_timers;
    
    (* Process timed out batches *)
    List.iter (fun (path, batch) ->
      Hashtbl.remove vfs_repl.pending_writes path;
      Hashtbl.remove vfs_repl.batch_timers path;
      
      let strategy = compute_replication_strategy batch in
      execute_replication_strategy vfs_repl strategy batch
    ) !to_process

  (** Enable/disable processing **)
  let set_processing_enabled vfs_repl enabled =
    vfs_repl.processing_enabled <- enabled;
    Printf.printf "[VFS Replication] Processing %s\n%!" 
      (if enabled then "enabled" else "disabled")

  (** Get pending batch count **)
  let get_pending_count vfs_repl =
    Hashtbl.fold (fun _path batch acc -> acc + List.length batch) vfs_repl.pending_writes 0
end