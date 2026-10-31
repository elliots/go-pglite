# Third-party notices

`pglite-wasi-17.5.tar.gz`, embedded in this package, is a build of other
people's software. The licence texts are in `third_party/`.

| Component | What it is | Licence |
| --- | --- | --- |
| PostgreSQL 17.5 | The database server, the `share/postgresql` files and the PL/pgSQL and Snowball dictionary modules linked into the module (`src/`, `COPYRIGHT` of the source tree) | PostgreSQL License (`third_party/PostgreSQL.txt`) |
| PGlite's PostgreSQL port | The WebAssembly port of PostgreSQL the module is built from: electric-sql/postgres-pglite, branch `REL_17_5_WASM-pglite` (the commit is in `pglite-wasi-17.5.tar.gz.txt`), with the `pglite-wasm/` glue, the `wasm-build/` port layer and the changes under `src/`. The repository's README points to its `COPYRIGHT` file, the PostgreSQL License; the PGlite project (electric-sql/pglite), which this port is part of, is published under the Apache License 2.0 | PostgreSQL License (`third_party/PostgreSQL.txt`); Apache License 2.0 (`third_party/Apache-2.0.txt`), © ElectricSQL |
| Snowball stemmer | `src/backend/snowball/libstemmer`, linked in as `dict_snowball` | BSD 3-Clause (`third_party/Snowball-BSD-3-Clause.txt`) |
| IANA Time Zone Database | `share/postgresql/timezone`, compiled from `src/timezone/data` | Public domain |
| wasi-libc (wasi-sdk 34.0, wasi-libc commit 2e6fb9d8ee0c) | The C library linked into the module, with `libsetjmp` and the `wasi-emulated-*` libraries. wasi-libc as a whole is multi-licensed; it includes code from musl, cloudlibc and dlmalloc under their own licences | Apache License 2.0 with LLVM exceptions, Apache License 2.0, or MIT (`third_party/wasi-libc-LICENSE.txt`, `third_party/Apache-2.0.txt`, `third_party/LLVM-exception.txt`, `third_party/wasi-libc-MIT.txt`); musl: MIT (`third_party/musl-COPYRIGHT.txt`); cloudlibc: BSD 2-Clause (`third_party/cloudlibc-BSD-2-Clause.txt`); dlmalloc: CC0 |
| LLVM compiler-rt builtins (wasi-sdk 34.0, LLVM 23.1.0) | Runtime support routines the compiler links into the module | Apache License 2.0 with LLVM exceptions (`third_party/Apache-2.0.txt`, `third_party/LLVM-exception.txt`) |

The patches in `build/patches/` and the shims in `build/compat/` are part of
this repository.

Go dependencies of the package, with their own licences in the module
cache: [wasmtime-go](https://github.com/bytecodealliance/wasmtime-go)
(Apache-2.0 with LLVM exceptions, bundling wasmtime, Apache-2.0 with LLVM
exceptions) and [pgx](https://github.com/jackc/pgx) (MIT).

This repository itself has no licence file; its owner has not chosen one.
