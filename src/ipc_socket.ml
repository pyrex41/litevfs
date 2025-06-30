(** Socket-based IPC Module
 * 
 * This module provides safe, portable inter-process communication
 * using Unix domain sockets, replacing the unsafe shared memory approach.
 *)

open Unix

(** Message types for IPC *)
type ipc_message = 
  | WriteEvent of {
      path: string;
      offset: int64;
      data: bytes;
      is_wal: bool;
      fence_token: int64;
    }
  | BatchComplete of {
      path: string;
      count: int;
    }
  | SyncEvent of {
      path: string;
      flags: int;
    }
  | TruncateEvent of {
      path: string;
      size: int64;
    }
  | StatusRequest
  | StatusResponse of {
      open_files: int;
      pending_writes: int;
      total_writes: int64;
    }
  | Shutdown

(** Socket server configuration *)
type server_config = {
  socket_path: string;
  max_connections: int;
  buffer_size: int;
  timeout_ms: int;
}

(** Client connection state *)
type client_state = {
  fd: file_descr;
  addr: sockaddr;
  mutable buffer: bytes;
  mutable buffer_pos: int;
  mutable last_activity: float;
}

(** Server state *)
type server_state = {
  config: server_config;
  listen_fd: file_descr;
  clients: (file_descr, client_state) Hashtbl.t;
  mutable running: bool;
  mutable message_handlers: (ipc_message -> unit) list;
}

(** Client state *)
type client_connection = {
  fd: file_descr;
  mutable connected: bool;
  socket_path: string;
}

(** Error types *)
type ipc_error =
  | SocketError of string
  | SerializationError of string
  | ConnectionClosed
  | Timeout
  | InvalidMessage of string

exception IPC_Error of ipc_error

let string_of_error = function
  | SocketError msg -> Printf.sprintf "Socket error: %s" msg
  | SerializationError msg -> Printf.sprintf "Serialization error: %s" msg
  | ConnectionClosed -> "Connection closed"
  | Timeout -> "Operation timed out"
  | InvalidMessage msg -> Printf.sprintf "Invalid message: %s" msg

(** Message serialization using Marshal *)
let serialize_message msg =
  try
    let data = Marshal.to_bytes msg [Marshal.Closures] in
    let len = Bytes.length data in
    let header = Bytes.create 4 in
    Bytes.set_int32_ne header 0 (Int32.of_int len);
    Bytes.cat header data
  with
  | e -> raise (IPC_Error (SerializationError (Printexc.to_string e)))

let deserialize_message data offset len =
  try
    if len < 4 then
      raise (IPC_Error (InvalidMessage "Message too short"));
    
    let msg_len = Int32.to_int (Bytes.get_int32_ne data offset) in
    if len < 4 + msg_len then
      raise (IPC_Error (InvalidMessage "Incomplete message"));
    
    let msg_data = Bytes.sub data (offset + 4) msg_len in
    let msg = Marshal.from_bytes msg_data 0 in
    (msg, 4 + msg_len)
  with
  | e -> raise (IPC_Error (SerializationError (Printexc.to_string e)))

(** Socket utilities *)
let create_unix_socket path =
  try
    (* Remove existing socket file *)
    (try Unix.unlink path with Unix_error _ -> ());
    
    let fd = Unix.socket PF_UNIX SOCK_STREAM 0 in
    Unix.set_nonblock fd;
    Unix.setsockopt fd SO_REUSEADDR true;
    (fd, ADDR_UNIX path)
  with
  | Unix_error (err, op, _) ->
    raise (IPC_Error (SocketError (Printf.sprintf "%s: %s" op (error_message err))))
  | e ->
    raise (IPC_Error (SocketError (Printexc.to_string e)))

let safe_close fd =
  try Unix.close fd with _ -> ()

let current_time () = Unix.gettimeofday ()

(** Server implementation *)
module Server = struct
  let default_config = {
    socket_path = "/tmp/distsqlite_ipc.sock";
    max_connections = 10;
    buffer_size = 64 * 1024; (* 64KB *)
    timeout_ms = 5000; (* 5 seconds *)
  }

  let create_server config =
    let (listen_fd, addr) = create_unix_socket config.socket_path in
    
    try
      Unix.bind listen_fd addr;
      Unix.listen listen_fd config.max_connections;
      
      Printf.printf "[IPC] Server listening on %s\n%!" config.socket_path;
      
      {
        config;
        listen_fd;
        clients = Hashtbl.create config.max_connections;
        running = false;
        message_handlers = [];
      }
    with
    | e ->
      safe_close listen_fd;
      raise e

  let add_message_handler server handler =
    server.message_handlers <- handler :: server.message_handlers

  let handle_message server msg =
    List.iter (fun handler ->
      try handler msg
      with e ->
        Printf.eprintf "[IPC] Message handler error: %s\n" (Printexc.to_string e)
    ) server.message_handlers

  let accept_client server =
    try
      let (client_fd, client_addr) = Unix.accept server.listen_fd in
      Unix.set_nonblock client_fd;
      
      let client = {
        fd = client_fd;
        addr = client_addr;
        buffer = Bytes.create server.config.buffer_size;
        buffer_pos = 0;
        last_activity = current_time ();
      } in
      
      Hashtbl.add server.clients client_fd client;
      Printf.printf "[IPC] Client connected: %s\n%!" 
        (match client_addr with ADDR_UNIX _ -> "unix_socket" | _ -> "unknown");
      
      Some client
    with
    | Unix_error (EAGAIN, _, _) | Unix_error (EWOULDBLOCK, _, _) -> None
    | Unix_error (err, op, _) ->
      Printf.eprintf "[IPC] Accept error: %s in %s\n" (error_message err) op;
      None
    | e ->
      Printf.eprintf "[IPC] Accept error: %s\n" (Printexc.to_string e);
      None

  let remove_client server client_fd =
    (match Hashtbl.find_opt server.clients client_fd with
    | Some _ -> 
      Hashtbl.remove server.clients client_fd;
      safe_close client_fd;
      Printf.printf "[IPC] Client disconnected\n%!"
    | None -> ())

  let read_from_client server client =
    try
      let available = Bytes.length client.buffer - client.buffer_pos in
      if available <= 0 then begin
        Printf.eprintf "[IPC] Client buffer full, disconnecting\n";
        remove_client server client.fd;
        false
      end else begin
        let bytes_read = Unix.recv client.fd client.buffer client.buffer_pos available [] in
        if bytes_read = 0 then begin
          remove_client server client.fd;
          false
        end else begin
          client.buffer_pos <- client.buffer_pos + bytes_read;
          client.last_activity <- current_time ();
          
          (* Process complete messages *)
          let rec process_messages offset remaining =
            if remaining >= 4 then begin
              let msg_len = Int32.to_int (Bytes.get_int32_ne client.buffer offset) in
              let total_len = 4 + msg_len in
              
              if remaining >= total_len then begin
                let (msg, consumed) = deserialize_message client.buffer offset remaining in
                handle_message server msg;
                process_messages (offset + consumed) (remaining - consumed)
              end
            end else if remaining > 0 then begin
              (* Move incomplete message to start of buffer *)
              Bytes.blit client.buffer offset client.buffer 0 remaining;
              client.buffer_pos <- remaining
            end else begin
              client.buffer_pos <- 0
            end
          in
          
          process_messages 0 client.buffer_pos;
          true
        end
      end
    with
    | Unix_error (EAGAIN, _, _) | Unix_error (EWOULDBLOCK, _, _) -> true
    | Unix_error (err, op, _) ->
      Printf.eprintf "[IPC] Read error: %s in %s\n" (error_message err) op;
      remove_client server client.fd;
      false
    | IPC_Error err ->
      Printf.eprintf "[IPC] Protocol error: %s\n" (string_of_error err);
      remove_client server client.fd;
      false
    | e ->
      Printf.eprintf "[IPC] Read error: %s\n" (Printexc.to_string e);
      remove_client server client.fd;
      false

  let cleanup_inactive_clients server =
    let now = current_time () in
    let timeout = float_of_int server.config.timeout_ms /. 1000.0 in
    
    let to_remove = ref [] in
    Hashtbl.iter (fun fd client ->
      if now -. client.last_activity > timeout then
        to_remove := fd :: !to_remove
    ) server.clients;
    
    List.iter (fun fd ->
      Printf.printf "[IPC] Removing inactive client\n%!";
      remove_client server fd
    ) !to_remove

  let run_server server =
    server.running <- true;
    Printf.printf "[IPC] Server started\n%!";
    
    try
      while server.running do
        (* Accept new clients *)
        (match accept_client server with
        | Some _ -> ()
        | None -> ());
        
        (* Read from existing clients *)
        let client_fds = Hashtbl.fold (fun fd _ acc -> fd :: acc) server.clients [] in
        List.iter (fun fd ->
          match Hashtbl.find_opt server.clients fd with
          | Some client -> ignore (read_from_client server client)
          | None -> ()
        ) client_fds;
        
        (* Clean up inactive clients *)
        cleanup_inactive_clients server;
        
        (* Small sleep to prevent busy waiting *)
        Thread.delay 0.001; (* 1ms *)
      done
    with
    | e ->
      Printf.eprintf "[IPC] Server error: %s\n" (Printexc.to_string e);
      server.running <- false

  let stop_server server =
    server.running <- false;
    
    (* Close all client connections *)
    Hashtbl.iter (fun fd _ -> safe_close fd) server.clients;
    Hashtbl.clear server.clients;
    
    (* Close listening socket *)
    safe_close server.listen_fd;
    
    (* Remove socket file *)
    (try Unix.unlink server.config.socket_path with _ -> ());
    
    Printf.printf "[IPC] Server stopped\n%!"

  let broadcast_message server msg =
    let data = serialize_message msg in
    let client_fds = Hashtbl.fold (fun fd _ acc -> fd :: acc) server.clients [] in
    
    List.iter (fun fd ->
      try
        let _ = Unix.send fd data 0 (Bytes.length data) [] in
        ()
      with
      | Unix_error (err, op, _) ->
        Printf.eprintf "[IPC] Send error to client: %s in %s\n" (error_message err) op;
        remove_client server fd
      | e ->
        Printf.eprintf "[IPC] Send error to client: %s\n" (Printexc.to_string e);
        remove_client server fd
    ) client_fds
end

(** Client implementation *)
module Client = struct
  let connect socket_path =
    let (fd, addr) = create_unix_socket socket_path in
    
    try
      Unix.connect fd addr;
      Printf.printf "[IPC] Connected to server at %s\n%!" socket_path;
      
      {
        fd;
        connected = true;
        socket_path;
      }
    with
    | Unix_error (err, op, _) ->
      safe_close fd;
      raise (IPC_Error (SocketError (Printf.sprintf "%s: %s" op (error_message err))))
    | e ->
      safe_close fd;
      raise (IPC_Error (SocketError (Printexc.to_string e)))

  let disconnect client =
    if client.connected then begin
      safe_close client.fd;
      client.connected <- false;
      Printf.printf "[IPC] Disconnected from server\n%!"
    end

  let send_message client msg =
    if not client.connected then
      raise (IPC_Error ConnectionClosed);
    
    try
      let data = serialize_message msg in
      let bytes_sent = Unix.send client.fd data 0 (Bytes.length data) [] in
      if bytes_sent <> Bytes.length data then
        raise (IPC_Error (SocketError "Incomplete send"));
    with
    | Unix_error (err, op, _) ->
      client.connected <- false;
      raise (IPC_Error (SocketError (Printf.sprintf "%s: %s" op (error_message err))))
    | IPC_Error _ as e -> raise e
    | e ->
      client.connected <- false;
      raise (IPC_Error (SocketError (Printexc.to_string e)))

  let receive_message client timeout_ms =
    if not client.connected then
      raise (IPC_Error ConnectionClosed);
    
    try
      (* Read header first *)
      let header = Bytes.create 4 in
      let _ = Unix.recv client.fd header 0 4 [] in
      let msg_len = Int32.to_int (Bytes.get_int32_ne header 0) in
      
      (* Read message body *)
      let body = Bytes.create msg_len in
      let _ = Unix.recv client.fd body 0 msg_len [] in
      
      Marshal.from_bytes body 0
    with
    | Unix_error (err, op, _) ->
      client.connected <- false;
      raise (IPC_Error (SocketError (Printf.sprintf "%s: %s" op (error_message err))))
    | e ->
      client.connected <- false;
      raise (IPC_Error (SerializationError (Printexc.to_string e)))
end

(** High-level API *)
module API = struct
  type server = server_state
  type client = client_connection
  
  let create_server = Server.create_server
  let run_server = Server.run_server
  let stop_server = Server.stop_server
  let add_message_handler = Server.add_message_handler
  let broadcast_message = Server.broadcast_message
  
  let connect_client = Client.connect
  let disconnect_client = Client.disconnect
  let send_message = Client.send_message
  let receive_message = Client.receive_message
  
  let default_server_config = Server.default_config
end

(** Integration with VFS module *)
let setup_vfs_integration server =
  (* Register VFS callbacks to send IPC messages *)
  Vfs_intercept.register_write_callback (fun events ->
    List.iter (fun event ->
      let msg = WriteEvent {
        path = event.path;
        offset = event.offset;
        data = Bytes.create (Bigarray.Array1.dim event.data); (* Convert bigarray to bytes *)
        is_wal = event.is_wal;
        fence_token = event.fence_token;
      } in
      
      (* Copy bigarray data to bytes *)
      let bytes_msg = match msg with
        | WriteEvent record ->
          for i = 0 to Bigarray.Array1.dim event.data - 1 do
            Bytes.set record.data i (Bigarray.Array1.get event.data i)
          done;
          msg
        | _ -> msg
      in
      
      Server.broadcast_message server bytes_msg
    ) events
  );
  
  Vfs_intercept.register_batch_callback (fun path count ->
    let msg = BatchComplete { path; count } in
    Server.broadcast_message server msg
  );
  
  Vfs_intercept.register_sync_callback (fun path flags ->
    let msg = SyncEvent { path; flags } in
    Server.broadcast_message server msg
  )

(** Callback registration for C interop *)
let () =
  Callback.register "ipc_create_server" (fun socket_path ->
    let config = { Server.default_config with socket_path } in
    Server.create_server config
  );
  
  Callback.register "ipc_connect_client" Client.connect;
  
  Callback.register "ipc_send_write_event" (fun client path offset data is_wal token ->
    let msg = WriteEvent {
      path;
      offset;
      data; (* Assuming data is already bytes *)
      is_wal;
      fence_token = token;
    } in
    Client.send_message client msg
  )