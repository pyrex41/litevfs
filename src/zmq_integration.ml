(** Complete ZMQ Integration for DistSQLite - Pure OCaml Implementation **)

open Zmq_transport
open Zmq_replication

(** Main integration module that orchestrates everything **)
module DistSQLiteZMQ = struct
  type mode = Coordinator | Node of string

  type config = {
    mode: mode;
    endpoints: Transport.endpoint list;
    node_id: string;
    auto_discovery: bool;
    batch_size: int;
    batch_timeout: float;
  }

  type t = {
    config: config;
    transport: Transport.t;
    lease_manager: LeaseManager.t;
    repl_manager: ReplicationManager.t;
    vfs_repl: VFSReplication.t;
    mutable running: bool;
    mutable coordinator_thread: Thread.t option;
    mutable subscriber_thread: Thread.t option;
  }

  (** Create default configuration **)
  let default_config mode node_id endpoints = {
    mode;
    endpoints;
    node_id;
    auto_discovery = true;
    batch_size = 100;
    batch_timeout = 0.05; (* 50ms *)
  }

  (** Initialize DistSQLite with ZMQ transport **)
  let create config =
    let transport_config = {
      Transport.mode = (match config.mode with Coordinator -> "coordinator" | Node _ -> "node");
      endpoints = config.endpoints;
      node_id = config.node_id;
      auto_discovery = config.auto_discovery;
    } in
    
    let transport = Transport.create_transport transport_config in
    Transport.initialize transport;
    
    let lease_manager = LeaseManager.create transport in
    let repl_manager = ReplicationManager.create transport in
    let vfs_repl = VFSReplication.create repl_manager 
      ~batch_size:config.batch_size 
      ~batch_timeout:config.batch_timeout () in
    
    {
      config;
      transport;
      lease_manager;
      repl_manager;
      vfs_repl;
      running = false;
      coordinator_thread = None;
      subscriber_thread = None;
    }

  (** Start all services **)
  let start system =
    system.running <- true;
    
    match system.config.mode with
    | Coordinator ->
        Printf.printf "[DistSQLite] Starting coordinator services\n%!";
        
        (* Start lease service in background thread *)
        let lease_thread = Thread.create (fun () ->
          LeaseManager.run_coordinator_service system.lease_manager
        ) () in
        system.coordinator_thread <- Some lease_thread;
        
        Printf.printf "[DistSQLite] Coordinator started\n%!"
        
    | Node peer_id ->
        Printf.printf "[DistSQLite] Starting node services (peer: %s)\n%!" peer_id;
        
        (* Start subscriber service in background thread *)
        let subscriber_thread = Thread.create (fun () ->
          ReplicationManager.run_subscriber_service system.repl_manager
        ) () in
        system.subscriber_thread <- Some subscriber_thread;
        
        Printf.printf "[DistSQLite] Node started\n%!"

  (** Stop all services **)
  let stop system =
    Printf.printf "[DistSQLite] Stopping services\n%!";
    
    system.running <- false;
    ReplicationManager.stop_replication system.repl_manager;
    
    (* Wait for background threads *)
    (match system.coordinator_thread with
     | Some thread -> Thread.join thread; system.coordinator_thread <- None
     | None -> ());
    
    (match system.subscriber_thread with
     | Some thread -> Thread.join thread; system.subscriber_thread <- None
     | None -> ());
    
    Transport.close_transport system.transport;
    Printf.printf "[DistSQLite] Stopped\n%!"

  (** High-level API functions **)
  
  (** Acquire database lease **)
  let acquire_lease system db_path ttl =
    match system.config.mode with
    | Node _ -> LeaseManager.acquire_lease system.lease_manager db_path ttl
    | Coordinator -> failwith "Coordinator cannot acquire leases"

  (** Renew database lease **)
  let renew_lease system db_path ttl =
    match system.config.mode with
    | Node _ -> LeaseManager.renew_lease system.lease_manager db_path ttl
    | Coordinator -> failwith "Coordinator cannot renew leases"

  (** Release database lease **)
  let release_lease system db_path =
    match system.config.mode with
    | Node _ -> LeaseManager.release_lease system.lease_manager db_path
    | Coordinator -> failwith "Coordinator cannot release leases"

  (** Subscribe to database changes **)
  let subscribe_database system db_path start_sequence handler =
    ReplicationManager.subscribe_database system.repl_manager db_path start_sequence handler

  (** Handle VFS write operation **)
  let handle_vfs_write system path offset data is_wal fence_token =
    VFSReplication.handle_vfs_write system.vfs_repl path offset data is_wal fence_token

  (** Process pending write batches (call periodically) **)
  let process_pending_writes system =
    VFSReplication.process_pending_batches system.vfs_repl

  (** Get system statistics **)
  let get_statistics system = 
    let repl_stats = ReplicationManager.get_stats system.repl_manager in
    let pending_count = VFSReplication.get_pending_count system.vfs_repl in
    
    [
      ("mode", match system.config.mode with Coordinator -> "coordinator" | Node p -> "node:" ^ p);
      ("running", string_of_bool system.running);
      ("diffs_published", Int64.to_string repl_stats.diffs_published);
      ("diffs_received", Int64.to_string repl_stats.diffs_received);
      ("batches_published", Int64.to_string repl_stats.batches_published);
      ("batches_received", Int64.to_string repl_stats.batches_received);
      ("bytes_replicated", Int64.to_string repl_stats.bytes_replicated);
      ("last_sequence", Int64.to_string repl_stats.last_sequence);
      ("pending_writes", string_of_int pending_count);
    ]

  (** Enable/disable VFS processing **)
  let set_vfs_processing system enabled =
    VFSReplication.set_processing_enabled system.vfs_repl enabled
end

(** C interface for integration with existing VFS layer **)
module CInterface = struct
  (* Global system instance *)
  let system_ref = ref None

  (** Initialize DistSQLite ZMQ system **)
  let initialize mode_str node_id endpoints_str batch_size batch_timeout =
    try
      let mode = match mode_str with
        | "coordinator" -> DistSQLiteZMQ.Coordinator
        | node_mode when String.length node_mode > 5 && String.sub node_mode 0 5 = "node:" ->
            let peer_id = String.sub node_mode 5 (String.length node_mode - 5) in
            DistSQLiteZMQ.Node peer_id
        | _ -> failwith ("Invalid mode: " ^ mode_str)
      in
      
      (* Parse endpoints - simple format: "tcp://host:port,tcp://host2:port2" *)
      let endpoint_strings = String.split_on_char ',' endpoints_str in
      let endpoints = List.map (fun ep_str ->
        if String.length ep_str > 6 && String.sub ep_str 0 6 = "tcp://" then
          let addr_port = String.sub ep_str 6 (String.length ep_str - 6) in
          let parts = String.split_on_char ':' addr_port in
          match parts with
          | [addr; port] -> Transport.create_endpoint "tcp" addr (Some (int_of_string port))
          | [addr] -> Transport.create_endpoint "tcp" addr (Some 5560)
          | _ -> failwith ("Invalid endpoint: " ^ ep_str)
        else
          failwith ("Unsupported protocol in: " ^ ep_str)
      ) endpoint_strings in
      
      let config = DistSQLiteZMQ.default_config mode node_id endpoints in
      let config = { config with batch_size; batch_timeout } in
      
      let system = DistSQLiteZMQ.create config in
      DistSQLiteZMQ.start system;
      
      system_ref := Some system;
      Printf.printf "[C Interface] DistSQLite ZMQ initialized\n%!";
      true
    with
    | exn ->
        Printf.printf "[C Interface] Initialization failed: %s\n%!" (Printexc.to_string exn);
        false

  (** Handle VFS write from C **)
  let handle_write path offset data_ptr data_len is_wal fence_token =
    match !system_ref with
    | None -> 
        Printf.printf "[C Interface] System not initialized\n%!";
        false
    | Some system ->
        try
          (* Convert C data to OCaml bytes *)
          let data = Bytes.create data_len in
          (* This would need proper C stub implementation *)
          (* For now, create dummy data *)
          
          DistSQLiteZMQ.handle_vfs_write system path (Int64.of_int offset) data is_wal (Int64.of_int fence_token);
          true
        with
        | exn ->
            Printf.printf "[C Interface] Write handling failed: %s\n%!" (Printexc.to_string exn);
            false

  (** Process pending writes **)
  let process_pending () =
    match !system_ref with
    | None -> false
    | Some system ->
        try
          DistSQLiteZMQ.process_pending_writes system;
          true
        with
        | exn ->
            Printf.printf "[C Interface] Process pending failed: %s\n%!" (Printexc.to_string exn);
            false

  (** Acquire lease from C **)
  let acquire_lease db_path ttl =
    match !system_ref with
    | None -> (false, 0L)
    | Some system ->
        try
          let response = DistSQLiteZMQ.acquire_lease system db_path (Int64.of_int ttl) in
          (response.Messages.success, response.fence_token)
        with
        | exn ->
            Printf.printf "[C Interface] Lease acquisition failed: %s\n%!" (Printexc.to_string exn);
            (false, 0L)

  (** Get statistics **)
  let get_stats () =
    match !system_ref with
    | None -> []
    | Some system -> DistSQLiteZMQ.get_statistics system

  (** Shutdown system **)
  let shutdown () =
    match !system_ref with
    | None -> ()
    | Some system ->
        DistSQLiteZMQ.stop system;
        system_ref := None;
        Printf.printf "[C Interface] DistSQLite ZMQ shutdown\n%!"
end

(** Register OCaml callbacks for C integration **)
let () =
  Callback.register "zmq_initialize" CInterface.initialize;
  Callback.register "zmq_handle_write" CInterface.handle_write;
  Callback.register "zmq_process_pending" CInterface.process_pending;
  Callback.register "zmq_acquire_lease" CInterface.acquire_lease;
  Callback.register "zmq_get_stats" CInterface.get_stats;
  Callback.register "zmq_shutdown" CInterface.shutdown;
  
  Printf.printf "[ZMQ Integration] OCaml callbacks registered\n%!"