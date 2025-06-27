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