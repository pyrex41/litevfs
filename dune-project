(lang dune 3.0)

(name distsqlite-core)
(version 1.0.0)

(package
 (name distsqlite-core)
 (synopsis "High-performance OCaml core for DistSQLite")
 (description "Performance-critical components of DistSQLite rewritten in OCaml for 10x performance improvement")
 (depends 
  ocaml 
  dune
  bigarray-compat
  core_bench
  alcotest))

(executables
 (public_names distcore_bench))

(library
 (public_name distsqlite-core)
 (name distcore)
 (modules distcore lock_free_queue binary_diff replication lease_state_machine)
 (libraries bigarray-compat))

(rule
 (targets libdistcore.so)
 (deps distcore_stubs.c (glob_files *.cmo) (glob_files *.cmx))
 (action
  (run gcc -shared -fPIC -O3 -o libdistcore.so distcore_stubs.c 
       -I %{ocaml:standard_library} -lasmrun_shared -lm -lunix)))