(** Type-safe lease state machine for distributed coordination *)

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

(** Type-safe state transitions with exhaustive pattern matching *)
let transition state event =
  match state, event with
  
  (* Follower state transitions *)
  | Follower { leader = _; fence = _ }, Tick _current_time ->
      (* Stay in follower state, no actions needed *)
      state, []
      
  | Follower { leader; fence }, LeaderHeartbeat { from; fence = new_fence } ->
      if new_fence > fence then
        (* New leader with higher fence token *)
        Follower { leader = from; fence = new_fence }, []
      else if String.equal from leader && new_fence = fence then
        (* Heartbeat from current leader *)
        state, []
      else
        (* Ignore stale heartbeat *)
        state, []
        
  | Follower _, LeaseExpired ->
      (* Leader lease expired, start election *)
      Candidate { election_timeout = Unix.time () +. 5.0 }, [StartElection]
      
  | Follower _, NetworkPartition ->
      (* Network partition detected, become candidate *)
      Candidate { election_timeout = Unix.time () +. 10.0 }, [StartElection]
      
  | Follower _, LeaseAcquired { fence } ->
      (* We acquired the lease, become leader *)
      Leader { fence; expires_at = Unix.time () +. 30.0 }, [NotifyFollowers]
  
  (* Candidate state transitions *)
  | Candidate { election_timeout }, Tick current_time ->
      if current_time >= election_timeout then
        (* Election timeout, become expired *)
        Expired, []
      else
        (* Still in election period *)
        state, []
        
  | Candidate _, LeaseAcquired { fence } ->
      (* Won the election *)
      Leader { fence; expires_at = Unix.time () +. 30.0 }, [NotifyFollowers]
      
  | Candidate _, LeaderHeartbeat { from; fence } ->
      (* Another node became leader *)
      Follower { leader = from; fence }, [BecomeFollower from]
      
  | Candidate _, NetworkPartition ->
      (* Network partition during election, extend timeout *)
      Candidate { election_timeout = Unix.time () +. 15.0 }, []
      
  | Candidate _, LeaseExpired ->
      (* Keep trying to acquire lease *)
      state, [StartElection]
  
  (* Leader state transitions *)
  | Leader { fence = _; expires_at }, Tick current_time ->
      if current_time >= expires_at -. 10.0 then
        (* Lease expiring soon, renew it *)
        state, [RenewLease]
      else
        (* Still valid leader *)
        state, []
        
  | Leader { fence = _; expires_at = _ }, LeaseAcquired { fence = new_fence } ->
      (* Successfully renewed lease *)
      Leader { fence = new_fence; expires_at = Unix.time () +. 30.0 }, []
      
  | Leader _, LeaseExpired ->
      (* Lost leadership *)
      Expired, []
      
  | Leader { fence; _ }, LeaderHeartbeat { from; fence = new_fence } ->
      if new_fence > fence then
        (* Another node has higher fence token, step down *)
        Follower { leader = from; fence = new_fence }, [BecomeFollower from]
      else
        (* Ignore heartbeat from lower fence token *)
        state, []
        
  | Leader _, NetworkPartition ->
      (* Network partition as leader, step down to be safe *)
      Expired, []
  
  (* Expired state transitions *)
  | Expired, Tick _ ->
      (* Try to become candidate after some delay *)
      let random_delay = Random.float 5.0 +. 2.0 in
      Candidate { election_timeout = Unix.time () +. random_delay }, [StartElection]
      
  | Expired, LeaderHeartbeat { from; fence } ->
      (* A leader emerged, become follower *)
      Follower { leader = from; fence }, [BecomeFollower from]
      
  | Expired, LeaseAcquired { fence } ->
      (* We acquired the lease *)
      Leader { fence; expires_at = Unix.time () +. 30.0 }, [NotifyFollowers]
      
  | Expired, NetworkPartition ->
      (* Stay expired during partition *)
      state, []
      
  | Expired, LeaseExpired ->
      (* Already expired, no change *)
      state, []

(** Get current state type as string for debugging *)
let state_name = function
  | Follower _ -> "Follower"
  | Candidate _ -> "Candidate"
  | Leader _ -> "Leader"
  | Expired -> "Expired"

(** Check if state is in a leadership role *)
let is_leader = function
  | Leader _ -> true
  | _ -> false

(** Check if state can accept writes *)
let can_write = function
  | Leader { expires_at; _ } -> Unix.time () < expires_at
  | _ -> false

(** Get fence token from current state *)
let get_fence_token = function
  | Follower { fence; _ } -> Some fence
  | Leader { fence; _ } -> Some fence
  | Candidate _ -> None
  | Expired -> None

(** Get leader identity from current state *)
let get_leader = function
  | Follower { leader; _ } -> Some leader
  | Leader _ -> Some "self"
  | Candidate _ -> None
  | Expired -> None

(** State machine runner for continuous operation *)
module Runner = struct
  type t = {
    mutable state: state;
    mutable last_tick: float;
    action_callback: action -> unit;
  }
  
  let create initial_state action_callback =
    {
      state = initial_state;
      last_tick = Unix.time ();
      action_callback;
    }
  
  let process_event runner event =
    let new_state, actions = transition runner.state event in
    runner.state <- new_state;
    List.iter runner.action_callback actions
  
  let tick runner =
    let current_time = Unix.time () in
    runner.last_tick <- current_time;
    process_event runner (Tick current_time)
  
  let get_state runner = runner.state
  
  (** Auto-tick runner for background operation *)
  let run_background runner interval =
    let rec loop () =
      tick runner;
      Thread.delay interval;
      loop ()
    in
    Thread.create loop ()
end

(** Validation functions *)
let validate_transition old_state new_state =
  match old_state, new_state with
  (* Valid transitions *)
  | Follower _, Candidate _ -> true
  | Follower _, Leader _ -> true
  | Candidate _, Follower _ -> true
  | Candidate _, Leader _ -> true
  | Candidate _, Expired -> true
  | Leader _, Follower _ -> true
  | Leader _, Expired -> true
  | Expired, Candidate _ -> true
  | Expired, Follower _ -> true
  | Expired, Leader _ -> true
  (* Same state is always valid *)
  | s1, s2 when s1 = s2 -> true
  (* Invalid transitions *)
  | _ -> false

(** Metrics and observability *)
module Metrics = struct
  type transition_stats = {
    mutable follower_to_candidate: int;
    mutable candidate_to_leader: int;
    mutable leader_to_follower: int;
    mutable leader_to_expired: int;
    mutable expired_to_candidate: int;
    mutable total_transitions: int;
  }
  
  let create_stats () = {
    follower_to_candidate = 0;
    candidate_to_leader = 0;
    leader_to_follower = 0;
    leader_to_expired = 0;
    expired_to_candidate = 0;
    total_transitions = 0;
  }
  
  let record_transition stats old_state new_state =
    if old_state <> new_state then begin
      stats.total_transitions <- stats.total_transitions + 1;
      match old_state, new_state with
      | Follower _, Candidate _ -> 
          stats.follower_to_candidate <- stats.follower_to_candidate + 1
      | Candidate _, Leader _ ->
          stats.candidate_to_leader <- stats.candidate_to_leader + 1
      | Leader _, Follower _ ->
          stats.leader_to_follower <- stats.leader_to_follower + 1
      | Leader _, Expired ->
          stats.leader_to_expired <- stats.leader_to_expired + 1
      | Expired, Candidate _ ->
          stats.expired_to_candidate <- stats.expired_to_candidate + 1
      | _ -> ()
    end
  
  let get_stats stats = [
    ("follower_to_candidate", stats.follower_to_candidate);
    ("candidate_to_leader", stats.candidate_to_leader);
    ("leader_to_follower", stats.leader_to_follower);
    ("leader_to_expired", stats.leader_to_expired);
    ("expired_to_candidate", stats.expired_to_candidate);
    ("total_transitions", stats.total_transitions);
  ]
end