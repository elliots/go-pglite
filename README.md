# go-pglite

Embedded PostgreSQL 17.5 for Go: [PGlite](https://pglite.dev/)'s WASI port
of PostgreSQL, run with [wasmtime-go](https://github.com/bytecodealliance/wasmtime-go),
with a Unix-socket bridge so any Go SQL driver can connect.

## Usage

```go
package main

import (
    "database/sql"
    "fmt"
    "log"

    pglite "github.com/elliots/go-pglite"
)

func main() {
    pg, err := pglite.New(pglite.Config{})
    if err != nil {
        log.Fatal(err)
    }
    defer pg.Close()

    db := pg.DB()

    var greeting string
    err = db.QueryRow("SELECT 'hello from pglite'").Scan(&greeting)
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println(greeting)
}
```

## Configuration

```go
pglite.Config{
    DataDir:    "/path/to/data",  // empty = temporary directory (removed on close)
    Database:   "mydb",           // default: "postgres"
    User:       "myuser",         // default: "postgres"
    StdoutFile: "/tmp/pg.log",    // empty = /dev/null
    StderrFile: "/tmp/pg.err",    // empty = /dev/null
}
```

## How it works

`pglite-wasi-17.5.tar.gz` (PostgreSQL 17.5 compiled to WASI; its provenance
is in `pglite-wasi-17.5.tar.gz.txt`) is embedded with `//go:embed`. At
runtime it is extracted and executed with wasmtime, with the WebAssembly
exception-handling proposal enabled: the module's setjmp/longjmp, and with
it PostgreSQL's own error recovery, is built on it. A Unix socket bridge
translates between the PostgreSQL wire protocol and PGlite's file-based I/O,
so any Go SQL driver (this package uses [lib/pq](https://github.com/lib/pq))
can connect.

One single-user backend serves every connection, so session state (`SET`,
temporary tables) is shared between them.

The module sets `max_stack_depth` to 256kB (see `build/patches/`): recursion
also uses the native stack of wasmtime, limited to 2MB, and at this setting
PostgreSQL's own "stack depth limit exceeded" error fires before wasmtime
traps. Raising it is unsafe. `Err()` reports a trap, after which the
instance serves no further connections.

## Building the WASI binary

```
build/build.sh
```

needs Docker and git. It fetches PGlite's PostgreSQL tree at a pinned
commit, applies `build/patches/`, compiles it with a pinned wasi-sdk inside
the image from `build/Dockerfile`, and writes the archive and its provenance
file. The archive is reproducible byte for byte.

## Requirements

- Go 1.24+
- macOS or Linux (Unix sockets required)

## Running tests

```
go test -v -timeout 300s
```

## REPL

```
go run ./cmd/pglite-repl
```
