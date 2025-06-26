(** ZMQ Transport Layer for DistSQLite - Pure OCaml Implementation **)

open Zmq

(** Transport configuration and socket management **)
module Transport = struct
  type socket_type = 
    | REQ | REP | PUB | SUB | PUSH | PULL | DEALER | ROUTER

  type endpoint = {
    protocol: string; (* "tcp", "ipc", "inproc" *)
    address: string;
    port: int option;
  }

  type transport_config = {
    mode: string; (* "coordinator", "node" *)
    endpoints: endpoint list;
    node_id: string;
    auto_discovery: bool;
  }

  type socket_set = {
    (* Lease coordination *)
    lease_req: [`Req] Socket.t option;
    lease_rep: [`Rep] Socket.t option;
    
    (* WAL diff streaming *)
    diff_pub: [`Pub] Socket.t option;
    diff_sub: [`Sub] Socket.t option;
    
    (* Bulk operations *)
    bulk_push: [`Push] Socket.t option;
    bulk_pull: [`Pull] Socket.t option;
    
    (* Cluster management *)
    cluster_dealer: [`Dealer] Socket.t option;
    cluster_router: [`Router] Socket.t option;
  }

  type t = {
    context: Context.t;
    config: transport_config;
    sockets: socket_set;
    mutable connected: bool;
    mutable coordinator_endpoint: endpoint option;
  }

  let create_endpoint protocol address port =
    { protocol; address; port }

  let endpoint_to_string endpoint =
    match endpoint.port with
    | Some port -> Printf.sprintf "%s://%s:%d" endpoint.protocol endpoint.address port
    | None -> Printf.sprintf "%s://%s" endpoint.protocol endpoint.address

  let create_context () =
    Context.create ()

  let create_socket context socket_type =
    match socket_type with
    | REQ -> Socket.create context Socket.req
    | REP -> Socket.create context Socket.rep
    | PUB -> Socket.create context Socket.pub
    | SUB -> Socket.create context Socket.sub
    | PUSH -> Socket.create context Socket.push
    | PULL -> Socket.create context Socket.pull
    | DEALER -> Socket.create context Socket.dealer
    | ROUTER -> Socket.create context Socket.router

  let bind_socket socket endpoint =
    let addr = endpoint_to_string endpoint in
    Socket.bind socket addr;
    Printf.printf "[ZMQ Transport] Bound socket to %s\n%!" addr

  let connect_socket socket endpoint =
    let addr = endpoint_to_string endpoint in
    Socket.connect socket addr;
    Printf.printf "[ZMQ Transport] Connected socket to %s\n%!" addr

  let create_transport config =
    let context = create_context () in
    let sockets = {
      lease_req = None; lease_rep = None;
      diff_pub = None; diff_sub = None;
      bulk_push = None; bulk_pull = None;
      cluster_dealer = None; cluster_router = None;
    } in
    {
      context;
      config;
      sockets;
      connected = false;
      coordinator_endpoint = None;
    }

  (** Initialize transport based on mode **)
  let initialize transport =
    let { context; config; _ } = transport in
    
    match config.mode with
    | "coordinator" ->
        (* Coordinator binds to all service sockets *)
        let lease_rep = create_socket context REP in
        let diff_pub = create_socket context PUB in
        let bulk_pull = create_socket context PULL in
        let cluster_router = create_socket context ROUTER in
        
        List.iteri (fun i endpoint ->
          match i with
          | 0 -> bind_socket lease_rep endpoint
          | 1 -> bind_socket diff_pub { endpoint with port = Some ((Option.get endpoint.port) + 1) }
          | 2 -> bind_socket bulk_pull { endpoint with port = Some ((Option.get endpoint.port) + 2) }
          | 3 -> bind_socket cluster_router { endpoint with port = Some ((Option.get endpoint.port) + 3) }
          | _ -> ()
        ) config.endpoints;
        
        transport.sockets <- {
          transport.sockets with
          lease_rep = Some lease_rep;
          diff_pub = Some diff_pub;
          bulk_pull = Some bulk_pull;
          cluster_router = Some cluster_router;
        };
        transport.connected <- true;
        Printf.printf "[ZMQ Transport] Coordinator initialized\n%!"
        
    | "node" ->
        (* Node connects to coordinator services *)
        let lease_req = create_socket context REQ in
        let diff_sub = create_socket context SUB in
        let bulk_push = create_socket context PUSH in
        let cluster_dealer = create_socket context DEALER in
        
        (* Set node identity for DEALER socket *)
        Socket.set_identity cluster_dealer config.node_id;
        
        List.iteri (fun i endpoint ->
          match i with
          | 0 -> connect_socket lease_req endpoint
          | 1 -> connect_socket diff_sub { endpoint with port = Some ((Option.get endpoint.port) + 1) }
          | 2 -> connect_socket bulk_push { endpoint with port = Some ((Option.get endpoint.port) + 2) }
          | 3 -> connect_socket cluster_dealer { endpoint with port = Some ((Option.get endpoint.port) + 3) }
          | _ -> ()
        ) config.endpoints;
        
        (* Subscribe to all topics initially *)
        Socket.subscribe diff_sub "";
        
        transport.sockets <- {
          transport.sockets with
          lease_req = Some lease_req;
          diff_sub = Some diff_sub;
          bulk_push = Some bulk_push;
          cluster_dealer = Some cluster_dealer;
        };
        transport.connected <- true;
        Printf.printf "[ZMQ Transport] Node initialized\n%!"
        
    | _ -> failwith ("Unknown transport mode: " ^ config.mode)

  let close_transport transport =
    let close_socket_opt = function
      | None -> ()
      | Some socket -> Socket.close socket
    in
    
    close_socket_opt transport.sockets.lease_req;
    close_socket_opt transport.sockets.lease_rep;
    close_socket_opt transport.sockets.diff_pub;
    close_socket_opt transport.sockets.diff_sub;
    close_socket_opt transport.sockets.bulk_push;
    close_socket_opt transport.sockets.bulk_pull;
    close_socket_opt transport.sockets.cluster_dealer;
    close_socket_opt transport.sockets.cluster_router;
    
    Context.terminate transport.context;
    transport.connected <- false;
    Printf.printf "[ZMQ Transport] Closed\n%!"
end

(** Message serialization using Marshal (can be replaced with msgpack later) **)
module Messages = struct
  type lease_request = {
    node_id: string;
    db_path: string;
    ttl: int64;
    action: string; (* "acquire", "renew", "release" *)
  }

  type lease_response = {
    success: bool;
    fence_token: int64;
    ttl: int64;
    error_msg: string option;
  }

  type wal_diff = {
    db_path: string;
    sequence: int64;
    page_num: int32;
    data: bytes;
    operation: string;
    timestamp: int64;
  }

  type wal_batch = {
    db_path: string;
    diffs: wal_diff list;
    batch_id: string;
    timestamp: int64;
  }

  type cluster_message = {
    sender: string;
    message_type: string;
    data: bytes;
    timestamp: int64;
  }

  let serialize_lease_request req =
    Marshal.to_bytes req []

  let deserialize_lease_request data =
    Marshal.from_bytes data 0

  let serialize_lease_response resp =
    Marshal.to_bytes resp []

  let deserialize_lease_response data =
    Marshal.from_bytes data 0

  let serialize_wal_diff diff =
    Marshal.to_bytes diff []

  let deserialize_wal_diff data =
    Marshal.from_bytes data 0

  let serialize_wal_batch batch =
    Marshal.to_bytes batch []

  let deserialize_wal_batch data =
    Marshal.from_bytes data 0

  let serialize_cluster_message msg =
    Marshal.to_bytes msg []

  let deserialize_cluster_message data =
    Marshal.from_bytes data 0
end

(** Lease management using ZMQ REQ/REP pattern **)
module LeaseManager = struct
  open Messages

  type lease_state = {
    node_id: string;
    fence_token: int64;
    expires_at: int64;
    db_path: string;
  }

  type t = {
    transport: Transport.t;
    mutable leases: (string, lease_state) Hashtbl.t; (* db_path -> lease_state *)
    mutable fence_counter: int64;
  }

  let create transport =
    {
      transport;
      leases = Hashtbl.create 100;
      fence_counter = 1L;
    }

  let get_current_time () =
    Unix.time () |> Int64.of_float

  (** Coordinator-side lease management **)
  let handle_lease_request manager req =
    let current_time = get_current_time () in
    
    match req.action with
    | "acquire" ->
        let existing = Hashtbl.find_opt manager.leases req.db_path in
        (match existing with
         | Some lease when lease.expires_at > current_time ->
             (* Lease still active *)
             {
               success = false;
               fence_token = 0L;
               ttl = 0L;
               error_msg = Some ("Lease held by " ^ lease.node_id);
             }
         | _ ->
             (* Grant new lease *)
             let fence_token = manager.fence_counter in
             manager.fence_counter <- Int64.add manager.fence_counter 1L;
             let expires_at = Int64.add current_time req.ttl in
             
             let new_lease = {
               node_id = req.node_id;
               fence_token;
               expires_at;
               db_path = req.db_path;
             } in
             
             Hashtbl.replace manager.leases req.db_path new_lease;
             
             {
               success = true;
               fence_token;
               ttl = req.ttl;
               error_msg = None;
             }
        )
        
    | "renew" ->
        let existing = Hashtbl.find_opt manager.leases req.db_path in
        (match existing with
         | Some lease when lease.node_id = req.node_id ->
             let expires_at = Int64.add current_time req.ttl in
             let renewed_lease = { lease with expires_at } in
             Hashtbl.replace manager.leases req.db_path renewed_lease;
             
             {
               success = true;
               fence_token = lease.fence_token;
               ttl = req.ttl;
               error_msg = None;
             }
         | _ ->
             {
               success = false;
               fence_token = 0L;
               ttl = 0L;
               error_msg = Some "No valid lease to renew";
             }
        )
        
    | "release" ->
        let existing = Hashtbl.find_opt manager.leases req.db_path in
        (match existing with
         | Some lease when lease.node_id = req.node_id ->
             Hashtbl.remove manager.leases req.db_path;
             {
               success = true;
               fence_token = lease.fence_token;
               ttl = 0L;
               error_msg = None;
             }
         | _ ->
             {
               success = false;
               fence_token = 0L;
               ttl = 0L;
               error_msg = Some "No valid lease to release";
             }
        )
        
    | _ ->
        {
          success = false;
          fence_token = 0L;
          ttl = 0L;
          error_msg = Some ("Unknown action: " ^ req.action);
        }

  (** Node-side lease operations **)
  let send_lease_request transport req =
    match transport.Transport.sockets.lease_req with
    | None -> failwith "No lease request socket available"
    | Some socket ->
        let serialized = serialize_lease_request req in
        Socket.send socket (Bytes.to_string serialized);
        
        (* Receive response *)
        let response_data = Socket.recv socket in
        deserialize_lease_response (Bytes.of_string response_data)

  let acquire_lease manager db_path ttl =
    let req = {
      node_id = manager.transport.config.node_id;
      db_path;
      ttl;
      action = "acquire";
    } in
    send_lease_request manager.transport req

  let renew_lease manager db_path ttl =
    let req = {
      node_id = manager.transport.config.node_id;
      db_path;
      ttl;
      action = "renew";
    } in
    send_lease_request manager.transport req

  let release_lease manager db_path =
    let req = {
      node_id = manager.transport.config.node_id;
      db_path;
      ttl = 0L;
      action = "release";
    } in
    send_lease_request manager.transport req

  (** Run coordinator lease service **)
  let run_coordinator_service manager =
    match manager.transport.sockets.lease_rep with
    | None -> failwith "No lease reply socket available"
    | Some socket ->
        Printf.printf "[Lease Manager] Starting coordinator service\n%!";
        
        while manager.transport.connected do
          try
            (* Non-blocking receive with timeout *)
            let request_data = Socket.recv ~block:false socket in
            let req = deserialize_lease_request (Bytes.of_string request_data) in
            
            Printf.printf "[Lease Manager] Processing %s request for %s from %s\n%!"
              req.action req.db_path req.node_id;
            
            let response = handle_lease_request manager req in
            let serialized_response = serialize_lease_response response in
            Socket.send socket (Bytes.to_string serialized_response);
            
            Printf.printf "[Lease Manager] Response: success=%b, fence=%Ld\n%!"
              response.success response.fence_token
            
          with
          | Zmq.ZMQ_exception (EAGAIN, _) ->
              (* No message available, continue *)
              Unix.sleepf 0.001 (* 1ms *)
          | exn ->
              Printf.printf "[Lease Manager] Error: %s\n%!" (Printexc.to_string exn);
              Unix.sleepf 0.01 (* 10ms *)
        done
end