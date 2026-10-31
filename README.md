# go-pglite

PostgreSQL 17.5 in a Go process: [PGlite](https://pglite.dev/)'s WASI port
of PostgreSQL, run with [wasmtime-go](https://github.com/bytecodealliance/wasmtime-go),
behind a Unix socket that pgx or any other PostgreSQL client in the process
connects to. No external server, no Docker; cgo is required.

## Usage

```go
pg, err := pglite.New(pglite.Config{})
if err != nil {
    log.Fatal(err)
}
defer pg.Close()

pool, err := pg.Pool(ctx)            // *pgxpool.Pool with MaxConns 1
conn, err := pgx.Connect(ctx, pg.ConnString())
db := pg.DB()                        // *sql.DB (driver pgx), one connection
```

`New` with an empty `DataDir` initialises a cluster in a temporary directory
(8 to 15 seconds, mostly initdb's fsyncs) and removes it on `Close`. Tests
that need many instances make one template and clone it:

```go
err := pglite.CreateTemplate(ctx, "/path/to/template", pglite.Config{},
    func(ctx context.Context, pg *pglite.PGlite) error {
        return applyMigrations(ctx, pg.ConnString())
    })

pg, err := pglite.New(pglite.Config{DataDir: t.TempDir(), Template: "/path/to/template"})
```

A clone copies the cluster (about 40MB) and starts in 0.15 to 0.4 seconds,
also when several start at once; reopening an existing cluster takes about
40ms. A trivial query round trip through pgx takes 50 to 120µs (measured on
an Apple M-series laptop; `go test -v -run TestLatencyReport` and
`TestTemplate` print the numbers for a machine).

## Configuration

```go
pglite.Config{
    DataDir:    "",            // instance directory; empty = temporary, removed on Close
    Template:   "",            // template to copy the cluster from (CreateTemplate)
    CacheDir:   "",            // shared installation and compiled code; empty = user cache dir
    Settings:   map[string]string{"synchronous_commit": "off"}, // session settings every connection starts with
    StdoutFile: "",            // the module's stdout; empty = discarded
    StderrFile: "",            // PostgreSQL's log; empty = discarded
    Logger:     nil,           // *slog.Logger for the bridge's own messages; nil = slog.Default
}
```

The role and database are fixed: `postgres` and `postgres` (`pglite.User`,
`pglite.Database`). A connection asking for anything else is refused with
the SQLSTATE a server would use.

`synchronous_commit=off` is worth setting for tests: every commit otherwise
waits for an fsync of the WAL, which on macOS is a full flush and takes
several milliseconds. It cannot corrupt the cluster, only lose the last
commits after a crash.

## Limits

- **One session per instance.** The backend is PostgreSQL in single-user
  mode. A second connection while one is open waits up to a second for it
  to end, then fails with SQLSTATE 53300 (`too_many_connections`). A pool
  with one connection (`Pool`) is the intended client. Instances are
  independent, so run one per parallel worker.
- A new connection gets a clean session: when a connection ends, the
  bridge rolls back whatever was open and runs `DISCARD ALL`, so settings,
  prepared statements, temporary tables, advisory locks, cursors and
  `LISTEN` registrations do not survive. The same happens when `Close`
  cuts a connection, so the shutdown checkpoint never finds an open
  transaction. `RESET` of `search_path` or `TimeZone` returns to the
  module's command-line values (`pg_catalog`, `GMT`) rather than the usual
  defaults the bridge sets at session start.
- Statements cannot be cancelled: a CancelRequest is accepted and ignored,
  the statement runs to completion. Signals do not exist in the module, so
  `pg_cancel_backend` and `pg_terminate_backend` return false and do
  nothing.
- A `SET` during a session does not send a ParameterStatus message (the
  module's message loop skips PostgreSQL's report of changed settings);
  the startup reply carries the correct values for all reported settings.
- `COPY ... FROM STDIN` needs its whole data stream in the call that
  starts the COPY, so the bridge collects the `CopyData` messages up to
  `CopyDone` or `CopyFail` before it passes the query on, and the client
  sees `CopyInResponse` only afterwards. pgx's `CopyFrom` streams its data
  without waiting and works; a client that waits for `CopyInResponse`
  before sending (libpq's `PQputCopyData`) waits forever. A query may
  contain one such COPY; the extended protocol form (Parse of a COPY FROM
  STDIN) is refused with SQLSTATE 0A000, and in a pipeline the refusal
  discards the rest of the pipeline up to its Sync while the messages
  before it stand. Without these checks the backend aborts.
- `LISTEN`/`NOTIFY` within a session works; there is no other session to
  notify.
- A FATAL error (for example `pg_terminate_backend`) or a trap of the
  module ends the instance: `Err()` reports it and every later connection
  is refused with the reason. PostgreSQL's ordinary errors, including
  those in PL/pgSQL exception blocks and subtransactions, are recovered
  inside the module.
- Recursion is limited by `max_stack_depth=256kB`, set in the module (see
  `build/patches/`); deeper recursion reports PostgreSQL's "stack depth
  limit exceeded" before wasmtime's native stack runs out. Do not raise it.
- An instance holds the cluster's files open; copy a template only when no
  instance runs on it.

## How it works

`pglite-wasi-17.5.tar.gz` is embedded with `//go:embed`. On first use in a
process it is extracted into `CacheDir`, where wasmtime also caches the
compiled code (compiling the 11MB module takes a few seconds; later
processes skip it). Each instance is one wasmtime store with the module
instantiated against WASI preview 1: the instance directory is `/tmp`, the
shared installation is `/tmp/pglite` read-only, the cluster is
`/tmp/pglite/base`, and the host's `/dev` provides `/dev/urandom`.

Start-up calls the module's `_start`, `pgl_initdb` and `pgl_backend`. A
freshly initialised cluster is shut down and started again, because initdb
leaves the backend on `template1` with initdb's session settings.

The bridge accepts connections on a Unix socket and runs one session at a
time. It answers the startup handshake (the module processes the startup
packet itself; the client sees trust authentication), applies the
client's startup parameters with `set_config`, then passes the client's
messages to the module: a batch of pipelined messages up to the next
`Query`, `Sync` or `FunctionCall` goes into a buffer in the module's
memory, one call to `interactive_one` processes it and the reply is read
back from the same buffer. Input over 6MB and replies over 12MB go through
files in the cluster directory instead. There is no polling.

Errors raised by PostgreSQL unwind with setjmp/longjmp, which the build
implements with the WebAssembly exception-handling proposal (wasmtime runs
it with `SetWasmExceptions(true)`).

## Building the WASI binary

```
build/build.sh
```

needs Docker and git. It fetches PGlite's PostgreSQL tree
(electric-sql/postgres-pglite, branch `REL_17_5_WASM-pglite`) at a pinned
commit, applies `build/patches/` (setjmp/longjmp through wasi-libc's
`libsetjmp`, the stack base, `max_stack_depth`), compiles it with a pinned
wasi-sdk 34.0 inside the image from `build/Dockerfile`, and writes the
archive and `pglite-wasi-17.5.tar.gz.txt`, which records the commit,
patch checksums, toolchain and flags. The archive is reproducible byte for
byte.

The module's memory starts with the 12MB I/O zone (`--global-base`), then
the static data, the 8MB shadow stack (`--no-stack-first`; wasm-ld's
default would put the stack inside the I/O zone, where large replies
overwrote it) and the heap.

## Licences

go-pglite is licensed under the MIT License; see [LICENSE](LICENSE). The
embedded build is made of PostgreSQL, PGlite's port, wasi-libc and other
components under their own licences; see
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

## Requirements

- Go 1.27+, cgo
- macOS or Linux

## Running tests

```
go test -race -count=1 ./...
go test -bench . -run ^$ .
go run ./cmd/pglite-repl
```

The tests build one template cluster (`TestMain`) and clone it per test.
`TestLatencyReport`, `TestTemplateCloneStartsWithSetupData` and
`TestParallelInstancesFromOneTemplate` log start times and latencies.
