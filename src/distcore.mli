(** DistSQLite Core - High-performance OCaml components *)

(** High-performance VFS operations *)
module VFS : sig
  type t
  
  type write_event = {
    path: string;
    offset: int64;
    data: (char, Bigarray.int8_unsigned_elt, Bigarray.c_layout) Bigarray.Array1.t;
    is_wal: bool;
    fence_token: int64;
  }
  
  val init : unit -> (t, string) result
  val register : t -> unit
  
  (** Lock-free write queue *)
  module WriteQueue : sig
    type t
    val create : capacity:int -> t
    val enqueue : t -> write_event -> bool  (* false if full *)
    val dequeue_batch : t -> max:int -> write_event array
    val pending_count : t -> int
  end
end

(** Binary diff engine for efficient replication *)
module BinaryDiff : sig
  type page_hash = string
  type page_num = int
  
  type diff_chunk = {
    offset: int;
    old_data: (char, Bigarray.int8_unsigned_elt, Bigarray.c_layout) Bigarray.Array1.t;
    new_data: (char, Bigarray.int8_unsigned_elt, Bigarray.c_layout) Bigarray.Array1.t;
  }
  
  (** Compute page-level diff between two SQLite databases *)
  val compute_diff : 
    old_db:(char, Bigarray.int8_unsigned_elt, Bigarray.c_layout) Bigarray.Array1.t -> 
    new_db:(char, Bigarray.int8_unsigned_elt, Bigarray.c_layout) Bigarray.Array1.t -> 
    page_size:int -> 
    (page_num * diff_chunk list) list
  
  (** Rolling hash for rsync-like algorithm *)
  val rolling_hash : 
    window_size:int -> 
    (char, Bigarray.int8_unsigned_elt, Bigarray.c_layout) Bigarray.Array1.t -> 
    int32 array
end

(** High-performance replication protocol *)
module Replication : sig
  type strategy = 
    | TxLog        (* Transaction log shipping *)
    | BulkSync     (* Full rsync-style sync *)
    | Hybrid       (* Intelligent mix *)
    | PageDiff     (* Page-level binary diff *)
  
  type write_batch = VFS.write_event array
  
  (** Compute optimal replication strategy based on write patterns *)
  val compute_strategy : write_batch -> strategy
  
  (** Encode writes into efficient binary protocol *)
  val encode_write_batch : 
    write_batch -> 
    (char, Bigarray.int8_unsigned_elt, Bigarray.c_layout) Bigarray.Array1.t
  
  (** Implement sqlite3_rsync protocol natively *)
  module RsyncProtocol : sig
    type message =
      | OriginBegin of { protocol: int; page_size: int }
      | ReplicaHash of { page_num: int; hash: BinaryDiff.page_hash }
      | OriginPage of { 
          page_num: int; 
          data: (char, Bigarray.int8_unsigned_elt, Bigarray.c_layout) Bigarray.Array1.t 
        }
      | OriginTxn of { generation: int64 }
      
    val encode : 
      message -> 
      (char, Bigarray.int8_unsigned_elt, Bigarray.c_layout) Bigarray.Array1.t
      
    val decode : 
      (char, Bigarray.int8_unsigned_elt, Bigarray.c_layout) Bigarray.Array1.t -> 
      (message, string) result
  end
end

(** Lease state machine with exhaustive type safety *)
module LeaseStateMachine : sig
  type state =
    | Follower of { leader: string; fence: int64 }
    | Candidate of { election_timeout: float }
    | Leader of { fence: int64; expires_at: float }
    | Expired
  
  type event =
    | Tick of float
    | LeaseAcquired of { fence: int64 }
    | LeaseExpired
    | NetworkPartition
    | LeaderHeartbeat of { from: string; fence: int64 }
  
  type action =
    | StartElection
    | RenewLease
    | NotifyFollowers
    | BecomeFollower of string
  
  (** Type-safe state transitions *)
  val transition : state -> event -> state * action list
end