(** High-performance lock-free queue implementation *)

open Bigarray

type 'a node = {
  mutable item: 'a option;
  mutable next: 'a node option Atomic.t;
}

type 'a t = {
  head: 'a node Atomic.t;
  tail: 'a node Atomic.t;
  capacity: int;
  size: int Atomic.t;
}

let create_node item =
  { 
    item = Some item; 
    next = Atomic.make None;
  }

let create ~capacity =
  let dummy = { item = None; next = Atomic.make None } in
  {
    head = Atomic.make dummy;
    tail = Atomic.make dummy;
    capacity;
    size = Atomic.make 0;
  }

(** Lock-free enqueue using Michael & Scott algorithm *)
let enqueue t item =
  if Atomic.get t.size >= t.capacity then
    false
  else begin
    let new_node = create_node item in
    let rec try_enqueue () =
      let tail = Atomic.get t.tail in
      let next = Atomic.get tail.next in
      (* Check if tail still points to the last node *)
      if tail == Atomic.get t.tail then
        match next with
        | None ->
            (* Try to link new node at the end of the list *)
            if Atomic.compare_and_set tail.next None (Some new_node) then begin
              (* Try to swing tail to the new node *)
              ignore (Atomic.compare_and_set t.tail tail new_node);
              Atomic.incr t.size;
              true
            end else
              try_enqueue ()
        | Some next_node ->
            (* Tail is lagging, try to advance it *)
            ignore (Atomic.compare_and_set t.tail tail next_node);
            try_enqueue ()
      else
        try_enqueue ()
    in
    try_enqueue ()
  end

(** Lock-free dequeue *)
let dequeue t =
  let rec try_dequeue () =
    let head = Atomic.get t.head in
    let tail = Atomic.get t.tail in
    let next = Atomic.get head.next in
    
    (* Check consistency *)
    if head == Atomic.get t.head then
      if head == tail then
        match next with
        | None -> None (* Queue is empty *)
        | Some next_node ->
            (* Tail is lagging, try to advance it *)
            ignore (Atomic.compare_and_set t.tail tail next_node);
            try_dequeue ()
      else
        match next with
        | None -> try_dequeue () (* This shouldn't happen *)
        | Some next_node ->
            (* Try to swing head to the next node *)
            if Atomic.compare_and_set t.head head next_node then begin
              Atomic.decr t.size;
              next_node.item
            end else
              try_dequeue ()
    else
      try_dequeue ()
  in
  try_dequeue ()

(** Dequeue multiple items efficiently *)
let dequeue_batch t ~max =
  let rec collect acc count =
    if count >= max then
      List.rev acc
    else
      match dequeue t with
      | None -> List.rev acc
      | Some item -> collect (item :: acc) (count + 1)
  in
  Array.of_list (collect [] 0)

let pending_count t = 
  Atomic.get t.size

(** Write event specific queue *)
module WriteEventQueue = struct
  type write_event = {
    path: string;
    offset: int64;
    data: (char, int8_unsigned_elt, c_layout) Array1.t;
    is_wal: bool;
    fence_token: int64;
  }
  
  type t = write_event lock_free_queue
  and 'a lock_free_queue = 'a t
  
  let create = create
  let enqueue = enqueue
  let dequeue_batch = dequeue_batch
  let pending_count = pending_count
end