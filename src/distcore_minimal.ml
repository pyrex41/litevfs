(** Minimal DistSQLite Core - No stdlib dependencies *)

(* Basic initialization function *)
let distcore_minimal_init () = 
  (* Return 42 to indicate success *)
  42

(* Register minimal callback *)
let () =
  Callback.register "minimal_init" distcore_minimal_init