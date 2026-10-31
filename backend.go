package pglite

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/wago-org/wago"
	"github.com/wago-org/wasi/p1"
)

// backend is one PostgreSQL single-user backend running in an instance of
// the module. It executes frontend messages and returns the backend's reply.
// Its methods are not safe for concurrent use; the server serialises them.
//
// How the module talks: interactive_one() processes the pending input,
// which is either in the shared buffer at the bottom of linear memory
// ("CMA": the host copies the messages to bufAddr and calls
// interactive_write(len); the reply follows at bufAddr+len+1 and
// interactive_read() returns its length) or, after interactive_write(0),
// in the file /tmp/pglite/base/.s.PGSQL.5432.in. A reply that does not fit
// in the buffer, and every reply to file input, is written to
// .s.PGSQL.5432.out instead. PostgreSQL's errors are handled inside the
// module (ereport longjmps to the handler around the message loop); a
// trap is a crash.
type backend struct {
	inst    *wago.Instance
	outputs []*os.File // files receiving the module's stdout and stderr

	bufAddr int // start of the shared buffer in linear memory
	bufSize int // size of the shared buffer

	inFile, lockInFile, outFile string // host paths of the socket files
	stderr                      string // host path of PostgreSQL's stderr, for error messages
	log                         *slog.Logger

	mu     sync.Mutex
	failed error // set once the backend can no longer be used
	closed bool
}

// Socket file name, relative to the data directory (/tmp/pglite/base).
const socketBase = ".s.PGSQL.5432"

// pgl_initdb status bits (pglite-wasm/pg_main.c).
const (
	idbFailed = 0x01
	idbHasDB  = 0x04
)

// startBackend instantiates the module for the instance directory dir and
// starts PostgreSQL. A directory without a cluster is initialised first
// (initdb); fresh reports that, in which case the backend is connected to
// template1 and its session carries initdb's settings, so the caller
// finishes the initialisation and starts again.
func startBackend(rt *runtime, dir string, cfg *Config) (_ *backend, fresh bool, err error) {
	pgdata := filepath.Join(dir, "pglite", "base")
	if err := os.MkdirAll(pgdata, 0o700); err != nil {
		return nil, false, err
	}
	b := &backend{
		stderr:     cfg.StderrFile,
		log:        cfg.Logger,
		inFile:     filepath.Join(pgdata, socketBase+".in"),
		lockInFile: filepath.Join(pgdata, socketBase+".lock.in"),
		outFile:    filepath.Join(pgdata, socketBase+".out"),
	}
	for _, f := range []string{b.inFile, b.lockInFile, b.outFile, filepath.Join(pgdata, socketBase+".lock.out")} {
		if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, false, err
		}
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, b.close())
		}
	}()

	wasi := p1.Config{
		Args: []string{"/tmp/pglite/bin/postgres", "--single", Database},
		Env: []string{
			"ENVIRONMENT=wasm32_wasi_preview1", "PREFIX=/tmp/pglite",
			"PGDATA=/tmp/pglite/base", "PGSYSCONFDIR=/tmp/pglite",
			"PGUSER=" + User, "PGDATABASE=" + Database, "MODE=REACT",
			"REPL=N", "TZ=UTC", "PGTZ=UTC", "PATH=/tmp/pglite/bin",
		},
		Stdout: io.Discard,
		Stderr: io.Discard,
	}
	// The module expects its installation under /tmp/pglite with the data
	// directory at /tmp/pglite/base. wasi-libc resolves a path against the
	// preopen with the longest matching prefix, so the installation is
	// shared read-only while /tmp (where initdb writes scratch files) and
	// the data directory belong to the instance.
	//
	// /dev is the host's: PostgreSQL is built without OpenSSL and reads
	// /dev/urandom for every strong random value (gen_random_uuid, cancel
	// keys). A regular file would return the same bytes on every open.
	preopens := []struct {
		host, guest string
		writable    bool
	}{
		{dir, "/tmp", true},
		{filepath.Join(rt.install, "pglite"), "/tmp/pglite", false},
		{pgdata, "/tmp/pglite/base", true},
		{"/dev", "/dev", false},
	}
	for _, p := range preopens {
		// WASI skips a directory it cannot open instead of failing, so
		// each one is checked here.
		st, err := os.Stat(p.host)
		if err == nil && !st.IsDir() {
			err = errors.New("not a directory")
		}
		if err != nil {
			return nil, false, fmt.Errorf("pglite: map %s to %s: %w", p.host, p.guest, err)
		}
		wasi.Mounts = append(wasi.Mounts, p1.Preopen{
			GuestPath: p.guest, HostPath: p.host,
			Read: true, Write: p.writable, MutateDirectory: p.writable,
		})
	}
	if cfg.StdoutFile != "" {
		if wasi.Stdout, err = b.createOutput(cfg.StdoutFile); err != nil {
			return nil, false, fmt.Errorf("pglite: stdout file: %w", err)
		}
	}
	if cfg.StderrFile != "" {
		if wasi.Stderr, err = b.createOutput(cfg.StderrFile); err != nil {
			return nil, false, fmt.Errorf("pglite: stderr file: %w", err)
		}
	}

	exports := rt.compiled.ExportedFunctions()
	for _, name := range []string{
		"_start", "pgl_initdb", "pgl_backend", "get_buffer_addr", "get_buffer_size", "use_wire",
		"interactive_one", "interactive_write", "interactive_read", "pgl_shutdown",
	} {
		if !slices.Contains(exports, name) {
			return nil, false, fmt.Errorf("pglite: module has no %s export", name)
		}
	}
	if b.inst, err = wago.Instantiate(rt.compiled, wago.InstantiateOptions{Imports: p1.Imports(wasi)}); err != nil {
		return nil, false, fmt.Errorf("pglite: instantiate module: %w", err)
	}
	if b.inst.Memory() == nil {
		return nil, false, errors.New("pglite: module does not export its memory")
	}

	// _start runs main(), which sets up the C runtime and exits with status
	// 0 without starting PostgreSQL; pgl_initdb creates the cluster if
	// PGDATA has none; pgl_backend starts the single-user backend.
	if _, err := b.call("_start"); err != nil {
		if status, ok := exitStatus(err); !ok || status != 0 {
			return nil, false, err
		}
	}
	status, err := b.call("pgl_initdb")
	if err != nil {
		return nil, false, err
	}
	if status&idbFailed != 0 {
		return nil, false, fmt.Errorf("pglite: initdb failed (status %#x)%s", status, b.stderrHint())
	}
	fresh = status&idbHasDB == 0
	if _, err := b.call("pgl_backend"); err != nil {
		return nil, false, err
	}

	addr, err := b.call("get_buffer_addr", int32(0))
	if err != nil {
		return nil, false, err
	}
	size, err := b.call("get_buffer_size", int32(0))
	if err != nil {
		return nil, false, err
	}
	if addr <= 0 || size <= 0 || int(addr)+int(size) > len(b.memory()) {
		return nil, false, fmt.Errorf("pglite: module reports an invalid shared buffer (address %d, size %d)", addr, size)
	}
	b.bufAddr, b.bufSize = int(addr), int(size)

	// Wire mode: input and output are protocol messages rather than SQL
	// text and console output.
	if _, err := b.call("use_wire", int32(1)); err != nil {
		return nil, false, err
	}
	return b, fresh, nil
}

// createOutput opens a file for the module's output, replacing its
// contents. close closes it.
func (b *backend) createOutput(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	b.outputs = append(b.outputs, f)
	return f, nil
}

// invoke calls an exported function that takes i32 arguments and returns
// nothing (reported as 0) or one i32.
func (b *backend) invoke(name string, args ...int32) (int32, error) {
	raw := make([]uint64, len(args))
	for i, a := range args {
		raw[i] = wago.I32(a)
	}
	r, err := b.inst.Invoke(name, raw...)
	if err != nil {
		return 0, err
	}
	switch len(r) {
	case 0:
		return 0, nil
	case 1:
		return wago.AsI32(r[0]), nil
	default:
		return 0, fmt.Errorf("%s returned %d values, want at most one", name, len(r))
	}
}

// call is invoke for the start-up calls, with errors that name the call.
func (b *backend) call(name string, args ...int32) (int32, error) {
	v, err := b.invoke(name, args...)
	if err != nil {
		return 0, fmt.Errorf("pglite: %s: %w%s", name, err, b.stderrHint())
	}
	return v, nil
}

// memory returns the current view of linear memory. A call into the module
// may grow memory and invalidate the view.
func (b *backend) memory() []byte { return b.inst.Memory().UnsafeBytes() }

// exitStatus returns the WASI exit status carried by err, if any.
func exitStatus(err error) (int32, bool) {
	exit, ok := errors.AsType[*wago.ExitError](err)
	if !ok {
		return 0, false
	}
	return exit.Code, true
}

func (b *backend) stderrHint() string {
	if b.stderr == "" {
		return " (set Config.StderrFile to see PostgreSQL's log)"
	}
	return fmt.Sprintf(" (PostgreSQL's log is in %s)", b.stderr)
}

// exec passes complete frontend messages to PostgreSQL and returns
// everything it replied. SQL errors are part of the reply; a returned error
// means the backend is unusable. A FATAL or PANIC in the reply also makes
// the backend unusable: a real backend exits on them, while this build
// carries on in an undefined state.
func (b *backend) exec(input []byte) ([]byte, error) {
	if err := b.err(); err != nil {
		return nil, err
	}
	reply, err := b.run(input)
	if err != nil {
		return nil, b.fail(err)
	}
	fatal, err := fatalIn(reply)
	if err != nil {
		return nil, b.fail(err)
	}
	if fatal != nil {
		b.fail(fmt.Errorf("PostgreSQL reported %w", fatal))
	}
	return reply, nil
}

// fatalIn returns the first FATAL or PANIC in a reply, or an error if the
// reply is not a sequence of complete messages.
func fatalIn(reply []byte) (*PgError, error) {
	for m, err := range messages(reply) {
		if err != nil {
			return nil, fmt.Errorf("malformed reply: %w", err)
		}
		if m.typ() == 'E' {
			if e := parseError(m.body()); e.Severity == "FATAL" || e.Severity == "PANIC" {
				return e, nil
			}
		}
	}
	return nil, nil
}

// err returns the error that made the backend unusable, if any.
func (b *backend) err() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.failed
}

// fail records the first error that made the backend unusable.
func (b *backend) fail(err error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failed == nil {
		b.failed = fmt.Errorf("pglite: backend failed: %w%s", err, b.stderrHint())
		b.log.Error("pglite: backend failed, refusing further connections", "err", err)
	}
	return b.failed
}

func (b *backend) run(input []byte) ([]byte, error) {
	if len(input) > b.bufSize/2 {
		// Leave the shared buffer room for the reply.
		f, err := b.inputFile()
		if err != nil {
			return nil, err
		}
		_, err = f.Write(input)
		if err := errors.Join(err, f.Close()); err != nil {
			return nil, err
		}
		return b.runFile()
	}
	copy(b.memory()[b.bufAddr:], input)
	if _, err := b.invoke("interactive_write", int32(len(input))); err != nil {
		return nil, fmt.Errorf("interactive_write: %w", err)
	}
	if err := b.interact(); err != nil {
		return nil, err
	}
	n, err := b.invoke("interactive_read")
	if err != nil {
		return nil, fmt.Errorf("interactive_read: %w", err)
	}
	if n := int(n); n > 0 {
		start := b.bufAddr + len(input) + 1
		mem := b.memory() // the call may have grown memory
		if start+n > b.bufAddr+b.bufSize || start+n > len(mem) {
			return nil, fmt.Errorf("reply of %d bytes at %d overruns the shared buffer", n, start)
		}
		return append([]byte(nil), mem[start:start+n]...), nil
	}
	// Nothing in the buffer: either there was no reply, or it did not fit
	// and went to the output file.
	return b.readOutput()
}

// inputFile creates the file whose contents runFile passes to PostgreSQL.
func (b *backend) inputFile() (*os.File, error) {
	return os.OpenFile(b.lockInFile, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
}

// runFile passes the input file to PostgreSQL, which streams it through its
// receive buffer, and returns the reply from the output file.
func (b *backend) runFile() ([]byte, error) {
	// A zero length selects file input.
	if _, err := b.invoke("interactive_write", 0); err != nil {
		return nil, fmt.Errorf("interactive_write: %w", err)
	}
	if err := os.Rename(b.lockInFile, b.inFile); err != nil {
		return nil, err
	}
	if err := b.interact(); err != nil {
		return nil, err
	}
	return b.readOutput()
}

// interact calls interactive_one. The module sets PostgreSQL's stack base
// on entry, so the call must come from the host, never from inside an
// import callback. A trap means the module crashed.
func (b *backend) interact() error {
	if _, err := b.invoke("interactive_one"); err != nil {
		return fmt.Errorf("interactive_one trapped: %w", err)
	}
	return nil
}

// readOutput returns and removes the output file, or nil without one.
func (b *backend) readOutput() ([]byte, error) {
	data, err := os.ReadFile(b.outFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return data, os.Remove(b.outFile)
}

// query runs SQL through the simple query protocol and returns the text of
// every row it returned. An ErrorResponse becomes a *PgError.
func (b *backend) query(sql string) ([][]*string, error) {
	reply, err := b.exec(queryMessage(sql))
	if err != nil {
		return nil, err
	}
	return parseQueryReply(sql, reply)
}

// parseQueryReply extracts the rows from the reply to a simple query.
func parseQueryReply(sql string, reply []byte) ([][]*string, error) {
	var rows [][]*string
	var pgErr *PgError
	ready := false
	for m, err := range messages(reply) {
		if err != nil {
			return nil, err
		}
		switch m.typ() {
		case 'D':
			row, err := parseDataRow(m.body())
			if err != nil {
				return nil, err
			}
			rows = append(rows, row)
		case 'E':
			if pgErr == nil {
				pgErr = parseError(m.body())
			}
		case 'Z':
			ready = true
		}
	}
	if pgErr != nil {
		return nil, pgErr
	}
	if !ready {
		return nil, fmt.Errorf("pglite: query %q: reply has no ReadyForQuery", sql)
	}
	return rows, nil
}

// shutdown stops PostgreSQL and releases the instance. The module's exit
// path writes no shutdown checkpoint, so one is requested first; without
// it the next start would replay the WAL.
func (b *backend) shutdown() error {
	if b.closed {
		return nil
	}
	var err error
	if b.err() == nil {
		if _, err = b.query("CHECKPOINT"); err == nil {
			// pgl_shutdown runs proc_exit(66), which this build turns
			// into a partial shutdown that returns.
			if _, callErr := b.invoke("pgl_shutdown"); callErr != nil {
				if status, ok := exitStatus(callErr); !ok || status != 66 {
					err = fmt.Errorf("pglite: pgl_shutdown: %w%s", callErr, b.stderrHint())
				}
			}
		}
	}
	return errors.Join(err, b.close())
}

// close releases the instance without shutting PostgreSQL down.
func (b *backend) close() error {
	if b.closed {
		return nil
	}
	b.closed = true
	var err error
	if b.inst != nil {
		err = b.inst.Close()
	}
	for _, f := range b.outputs {
		err = errors.Join(err, f.Close())
	}
	return err
}
