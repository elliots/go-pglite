// Package pglite provides an embedded PostgreSQL instance running as a WASI module
// via wasmtime. It wraps the PGlite WASI build of PostgreSQL, allowing Go applications
// to use a real PostgreSQL database without any external dependencies.
//
// Usage:
//
//	pg, err := pglite.New(pglite.Config{})
//	if err != nil { ... }
//	defer pg.Close()
//
//	db := pg.DB() // *sql.DB — use like any other database
//	db.QueryRow("SELECT 1")
package pglite

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bytecodealliance/wasmtime-go/v50"
	_ "github.com/lib/pq"
)

// maxWasmStack is the native stack wasmtime lets WebAssembly frames use
// (default 512kB). PostgreSQL limits recursion by its shadow stack depth
// (max_stack_depth, see check_stack_depth); the native frames of the same
// recursion must fit here, or wasmtime traps before PostgreSQL reports the
// error. wasmtime caps it at its async stack size, 2MB.
const maxWasmStack = 2 << 20

// PGlite is an embedded PostgreSQL instance running in a WASI sandbox.
type PGlite struct {
	wasmMu   sync.Mutex // protects WASM function calls
	initMu   sync.Mutex // protects lazy initialization of db
	cfg      Config
	ctx      context.Context
	cancel   context.CancelFunc
	engine   *wasmtime.Engine
	store    *wasmtime.Store
	instance *wasmtime.Instance
	dataDir  string
	tempDir  bool

	// Socket bridge.
	socketDir  string
	socketPath string
	listener   net.Listener
	wg         sync.WaitGroup

	// Internal sql.DB (lazily initialized).
	db *sql.DB

	// fatal is the first trap of the module, after which the backend's
	// state is unknown and no further connections are served.
	fatalMu sync.Mutex
	fatal   error

	// Cached exported functions.
	fnInteractiveOne   *wasmtime.Func
	fnInteractiveWrite *wasmtime.Func
	fnInteractiveRead  *wasmtime.Func
	fnUseWire          *wasmtime.Func
	fnClearError       *wasmtime.Func

	// CMA (shared memory) transport.
	memory        *wasmtime.Memory
	cmaChannel    int32
	cmaBufferAddr int
	cmaBufferSize int
}

// New creates and initializes a new PGlite instance.
func New(cfg Config) (*PGlite, error) {
	return NewContext(context.Background(), cfg)
}

// NewContext creates and initializes a new PGlite instance with the given context.
func NewContext(ctx context.Context, cfg Config) (*PGlite, error) {
	cfg = cfg.withDefaults()

	ctx, cancel := context.WithCancel(ctx)

	pg := &PGlite{
		cfg:    cfg,
		ctx:    ctx,
		cancel: cancel,
	}

	// Set up data directory.
	if cfg.DataDir == "" {
		tmp, err := os.MkdirTemp("", "go-pglite-*")
		if err != nil {
			cancel()
			return nil, fmt.Errorf("creating temp dir: %w", err)
		}
		pg.dataDir = tmp
		pg.tempDir = true
	} else {
		pg.dataDir = cfg.DataDir
		if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
			cancel()
			return nil, fmt.Errorf("creating data dir: %w", err)
		}
	}

	// Extract WASI binary and set up filesystem.
	wasmBinary, err := setupEnvironment(pg.dataDir)
	if err != nil {
		pg.cleanup()
		cancel()
		return nil, fmt.Errorf("setup environment: %w", err)
	}

	// Create wasmtime engine and store. The module implements
	// setjmp/longjmp with WebAssembly exception handling (exnref encoding),
	// which wasmtime only runs when the proposal is enabled.
	engineCfg := wasmtime.NewConfig()
	engineCfg.SetWasmExceptions(true)
	engineCfg.SetMaxWasmStack(maxWasmStack)
	pg.engine = wasmtime.NewEngineWithConfig(engineCfg)
	pg.store = wasmtime.NewStore(pg.engine)

	// Configure WASI — mirrors pglite-oxide's standard_wasi_builder.
	wasiCfg := wasmtime.NewWasiConfig()
	wasiCfg.SetArgv([]string{"/tmp/pglite/bin/postgres", "--single", cfg.Database})
	wasiCfg.SetEnv(
		[]string{"ENVIRONMENT", "PREFIX", "PGDATA", "PGSYSCONFDIR", "PGUSER", "PGDATABASE", "MODE", "REPL", "TZ", "PGTZ", "PATH"},
		[]string{"wasm32_wasi_preview1", "/tmp/pglite", "/tmp/pglite/base", "/tmp/pglite", cfg.User, cfg.Database, "REACT", "N", "UTC", "UTC", "/tmp/pglite/bin"},
	)

	pgdataDir := filepath.Join(pg.dataDir, "pglite", "base")
	devDir := filepath.Join(pg.dataDir, "dev")
	if err := os.MkdirAll(pgdataDir, 0o755); err != nil {
		pg.cleanup()
		cancel()
		return nil, fmt.Errorf("creating pgdata dir: %w", err)
	}

	// Three pre-opened directories matching pglite-oxide, all writable.
	if err := wasiCfg.PreopenDir(pg.dataDir, "/tmp", true); err != nil {
		pg.cleanup()
		cancel()
		return nil, fmt.Errorf("preopen /tmp: %w", err)
	}
	if err := wasiCfg.PreopenDir(pgdataDir, "/tmp/pglite/base", true); err != nil {
		pg.cleanup()
		cancel()
		return nil, fmt.Errorf("preopen /tmp/pglite/base: %w", err)
	}
	if err := wasiCfg.PreopenDir(devDir, "/dev", true); err != nil {
		pg.cleanup()
		cancel()
		return nil, fmt.Errorf("preopen /dev: %w", err)
	}

	// Stdout/stderr: redirect to files or inherit.
	if cfg.StdoutFile != "" {
		if err := wasiCfg.SetStdoutFile(cfg.StdoutFile); err != nil {
			pg.cleanup()
			cancel()
			return nil, fmt.Errorf("set stdout file: %w", err)
		}
	} else {
		wasiCfg.InheritStdout()
	}
	if cfg.StderrFile != "" {
		if err := wasiCfg.SetStderrFile(cfg.StderrFile); err != nil {
			pg.cleanup()
			cancel()
			return nil, fmt.Errorf("set stderr file: %w", err)
		}
	} else {
		wasiCfg.InheritStderr()
	}

	pg.store.SetWasi(wasiCfg)

	// Create linker with WASI imports.
	linker := wasmtime.NewLinker(pg.engine)
	if err := linker.DefineWasi(); err != nil {
		pg.cleanup()
		cancel()
		return nil, fmt.Errorf("defining WASI: %w", err)
	}

	// Compile the module.
	module, err := wasmtime.NewModule(pg.engine, wasmBinary)
	if err != nil {
		pg.cleanup()
		cancel()
		return nil, fmt.Errorf("compiling WASM module: %w", err)
	}

	// Instantiate the module.
	instance, err := linker.Instantiate(pg.store, module)
	if err != nil {
		pg.cleanup()
		cancel()
		return nil, fmt.Errorf("instantiating WASM module: %w", err)
	}
	pg.instance = instance

	// Initialize following pglite-oxide's sequence: _start → pgl_initdb → pgl_backend.
	if fn := instance.GetFunc(pg.store, "_start"); fn != nil {
		_, err := fn.Call(pg.store)
		if err != nil && !isWasiExit(err) {
			pg.cleanup()
			cancel()
			return nil, fmt.Errorf("_start: %w", err)
		}
	}

	if fn := instance.GetFunc(pg.store, "pgl_initdb"); fn != nil {
		if _, err := fn.Call(pg.store); err != nil {
			pg.cleanup()
			cancel()
			return nil, fmt.Errorf("pgl_initdb: %w", err)
		}
	}

	if fn := instance.GetFunc(pg.store, "pgl_backend"); fn != nil {
		if _, err := fn.Call(pg.store); err != nil {
			pg.cleanup()
			cancel()
			return nil, fmt.Errorf("pgl_backend: %w", err)
		}
	}

	// Cache exported functions.
	pg.fnInteractiveOne = instance.GetFunc(pg.store, "interactive_one")
	pg.fnInteractiveWrite = instance.GetFunc(pg.store, "interactive_write")
	pg.fnInteractiveRead = instance.GetFunc(pg.store, "interactive_read")
	pg.fnUseWire = instance.GetFunc(pg.store, "use_wire")
	pg.fnClearError = instance.GetFunc(pg.store, "clear_error")

	if pg.fnInteractiveOne == nil {
		pg.cleanup()
		cancel()
		return nil, fmt.Errorf("module missing 'interactive_one' export")
	}

	// Always grab WASM memory — needed for portal cleanup on trap recovery.
	if exp := instance.GetExport(pg.store, "memory"); exp != nil {
		pg.memory = exp.Memory()
	}

	// Detect CMA transport (shared memory) — same logic as pglite-oxide.
	if fn := instance.GetFunc(pg.store, "get_channel"); fn != nil {
		if result, err := fn.Call(pg.store); err == nil {
			channel := result.(int32)
			fmt.Fprintf(os.Stderr, "# pglite: get_channel() = %d\n", channel)
			if channel >= 0 {
				pg.cmaChannel = channel
				if fnAddr := instance.GetFunc(pg.store, "get_buffer_addr"); fnAddr != nil {
					if addr, err := fnAddr.Call(pg.store, channel); err == nil {
						pg.cmaBufferAddr = int(addr.(int32))
					}
				}
				if fnSize := instance.GetFunc(pg.store, "get_buffer_size"); fnSize != nil {
					if size, err := fnSize.Call(pg.store, channel); err == nil {
						pg.cmaBufferSize = int(size.(int32))
					}
				}
				fmt.Fprintf(os.Stderr, "# pglite: CMA transport: channel=%d addr=%d size=%d\n",
					pg.cmaChannel, pg.cmaBufferAddr, pg.cmaBufferSize)
			}
		}
	}

	// Start the socket bridge.
	if err := pg.startBridge(); err != nil {
		pg.cleanup()
		cancel()
		return nil, fmt.Errorf("starting socket bridge: %w", err)
	}

	return pg, nil
}

// DB returns a *sql.DB connected to the embedded PostgreSQL instance.
// Uses lib/pq which sends simple query protocol — no portals, no extended
// query issues with pglite's WASI binary.
func (pg *PGlite) DB() *sql.DB {
	pg.initMu.Lock()
	defer pg.initMu.Unlock()
	if pg.db == nil {
		db, err := sql.Open("postgres", pg.connString())
		if err == nil {
			db.SetMaxOpenConns(1)
			db.SetMaxIdleConns(1)
			db.SetConnMaxLifetime(0)
			pg.db = db
		}
	}
	return pg.db
}

// Close shuts down the PGlite instance and cleans up resources.
func (pg *PGlite) Close() error {
	if pg.db != nil {
		pg.db.Close()
	}
	pg.cancel()
	if pg.listener != nil {
		pg.listener.Close()
	}
	pg.wg.Wait()
	pg.cleanup()
	if pg.socketDir != "" {
		os.RemoveAll(pg.socketDir)
	}
	return nil
}

// Err returns the trap that stopped the module, or nil while it is running.
// PostgreSQL recovers from its own errors inside the module; a trap means
// the module crashed, so after it no connection is served.
func (pg *PGlite) Err() error {
	pg.fatalMu.Lock()
	defer pg.fatalMu.Unlock()
	return pg.fatal
}

func (pg *PGlite) fail(err error) {
	pg.fatalMu.Lock()
	defer pg.fatalMu.Unlock()
	if pg.fatal == nil {
		pg.fatal = err
		fmt.Fprintf(os.Stderr, "pglite: module crashed, refusing further connections: %v\n", err)
	}
}

// ConnString returns a libpq-style connection string for the embedded
// server, for drivers other than the lib/pq one DB uses. The server is a
// single-user backend: use one connection at a time and the simple query
// protocol.
func (pg *PGlite) ConnString() string {
	return pg.connString()
}

func (pg *PGlite) connString() string {
	return fmt.Sprintf("host=%s port=5432 dbname=%s user=%s sslmode=disable",
		pg.socketDir, pg.cfg.Database, pg.cfg.User)
}

func (pg *PGlite) cleanup() {
	if pg.tempDir && pg.dataDir != "" {
		os.RemoveAll(pg.dataDir)
	}
}

// isWasiExit checks if an error is a WASI proc_exit (any code).
func isWasiExit(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "exit status")
}

// startBridge creates a Unix socket and starts the bridge goroutine.
func (pg *PGlite) startBridge() error {
	sockDir, err := os.MkdirTemp("", "pglite-sock-*")
	if err != nil {
		return fmt.Errorf("creating socket dir: %w", err)
	}
	pg.socketDir = sockDir
	pg.socketPath = filepath.Join(sockDir, ".s.PGSQL.5432")

	ln, err := net.Listen("unix", pg.socketPath)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", pg.socketPath, err)
	}
	pg.listener = ln

	ioBase := filepath.Join(pg.dataDir, "pglite", "base", ".s.PGSQL.5432")

	// Clean up stale files.
	for _, suffix := range []string{".in", ".out", ".lock.in", ".lock.out"} {
		os.Remove(ioBase + suffix)
	}

	pg.wg.Add(1)
	go func() {
		defer pg.wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			pg.handleConn(conn, ioBase)
		}
	}()

	return nil
}

// handleConn bridges a single client connection to PGlite via file-based I/O.
// Mirrors pglite-oxide's handle_client loop.
func (pg *PGlite) handleConn(conn net.Conn, ioBase string) {
	defer conn.Close()

	// After a trap the backend's state is unknown: serve nothing.
	if pg.Err() != nil {
		return
	}

	inFile := ioBase + ".in"
	lockIn := ioBase + ".lock.in"
	outFile := ioBase + ".out"

	// interactive_one expects whole protocol messages (one or several), so
	// bytes are held back until a message is complete. A socket read returns
	// an arbitrary prefix of what the client wrote; a query longer than one
	// read used to reach PostgreSQL as a fragment and abort the backend.
	var pending []byte
	startup := true
	buf := make([]byte, 65536)

	for {
		select {
		case <-pg.ctx.Done():
			return
		default:
		}

		// Check for pending replies (mirrors pglite-oxide's drain_wire).
		pg.wasmMu.Lock()
		if data, err := os.ReadFile(outFile); err == nil && len(data) > 0 {
			os.Remove(outFile)
			pg.wasmMu.Unlock()
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, werr := conn.Write(data); werr != nil {
				return
			}
			continue
		}
		pg.wasmMu.Unlock()

		conn.SetReadDeadline(time.Now().Add(16 * time.Millisecond))
		n, readErr := conn.Read(buf)

		if n > 0 {
			pending = append(pending, buf[:n]...)
			complete, rest, stillStartup, err := splitMessages(pending, startup)
			if err != nil {
				fmt.Fprintf(os.Stderr, "pglite: closing connection: %v\n", err)
				return
			}
			pending = rest
			startup = stillStartup
			if len(complete) == 0 {
				continue
			}

			// Write to lock file then rename (atomic delivery).
			if err := os.WriteFile(lockIn, complete, 0o644); err != nil {
				return
			}
			if err := os.Rename(lockIn, inFile); err != nil {
				return
			}

			// forward_wire: send payload through WASM and collect all replies.
			// Mirrors pglite-oxide: if forward_wire errors (trap), close the
			// connection. database/sql will reconnect automatically.
			pg.wasmMu.Lock()
			replies, trapErr := pg.forwardWire(outFile)
			pg.wasmMu.Unlock()

			// Send whatever PG wrote (may include the error response).
			if !pg.sendReplies(conn, replies) {
				return
			}
			if trapErr != nil {
				pg.fail(trapErr)
				return
			}
		}

		if readErr != nil {
			if netErr, ok := readErr.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return
		}
	}
}

// splitMessages splits client bytes into the longest prefix made of complete
// protocol messages and the remainder. Before the session is established
// (startup true) a message is an int32 length followed by its body; SSL and
// GSS requests (8 bytes with a magic code) are answered and followed by
// another such message, so startup stays true after them. Afterwards a
// message is a type byte, an int32 length (including itself) and the body.
// A length that cannot be valid is a protocol error.
func splitMessages(data []byte, startup bool) (complete, rest []byte, stillStartup bool, err error) {
	const maxMessage = 1 << 30 // PostgreSQL's own limit
	end := 0
	for {
		var size, body int
		if startup {
			if len(data)-end < 4 {
				break
			}
			body = int(binary.BigEndian.Uint32(data[end:]))
			size = body
			if body < 8 || body > maxMessage {
				return nil, nil, startup, fmt.Errorf("invalid startup message length %d", body)
			}
		} else {
			if len(data)-end < 5 {
				break
			}
			body = int(binary.BigEndian.Uint32(data[end+1:]))
			size = 1 + body
			if body < 4 || body > maxMessage {
				return nil, nil, startup, fmt.Errorf("invalid message length %d for type %q", body, data[end])
			}
		}
		if len(data)-end < size {
			break
		}
		if startup {
			code := binary.BigEndian.Uint32(data[end+4:])
			const sslRequest, gssEncRequest = 80877103, 80877104
			startup = size == 8 && (code == sslRequest || code == gssEncRequest)
			// Startup messages are delivered one at a time: the reply decides
			// how the following bytes are framed.
			end += size
			break
		}
		end += size
	}
	return data[:end], data[end:], startup, nil
}

// forwardWire mirrors pglite-oxide's forward_wire: enables wire mode, then loops
// interactive_one + collectReplies up to maxTicks times, breaking when no data
// is produced. Returns an error if interactive_one traps. Must be called with
// wasmMu held.
func (pg *PGlite) forwardWire(outFile string) ([][]byte, error) {
	const maxTicks = 256

	if pg.fnUseWire != nil {
		pg.fnUseWire.Call(pg.store, int32(1)) //nolint:errcheck
	}

	var replies [][]byte
	for range maxTicks {
		producedBefore := pg.collectReply(outFile, &replies)

		_, err := pg.fnInteractiveOne.Call(pg.store)
		if err != nil {
			// PostgreSQL recovers from its own errors inside interactive_one
			// (ereport longjmps to the handler in pgl_sjlj.c), so a trap here
			// is a genuine crash of the module. Hand back what it wrote and
			// report it; the caller closes the connection.
			pg.collectReply(outFile, &replies)
			os.Remove(strings.TrimSuffix(outFile, ".out") + ".in")
			return replies, fmt.Errorf("interactive_one trapped: %w", err)
		}

		producedAfter := pg.collectReply(outFile, &replies)
		if !producedBefore && !producedAfter {
			break
		}
	}

	return replies, nil
}

// collectReply reads the .out file and appends non-empty data to replies.
// Returns true if any data was produced. Must be called with wasmMu held.
func (pg *PGlite) collectReply(outFile string, replies *[][]byte) bool {
	data, err := os.ReadFile(outFile)
	if err != nil || len(data) == 0 {
		return false
	}
	os.Remove(outFile)
	*replies = append(*replies, data)
	return true
}

// sendReplies writes all reply chunks to the connection. Returns false on write error.
func (pg *PGlite) sendReplies(conn net.Conn, replies [][]byte) bool {
	for _, data := range replies {
		if len(data) == 0 {
			continue
		}
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := conn.Write(data); err != nil {
			return false
		}
	}
	return true
}
