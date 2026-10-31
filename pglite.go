// Package pglite runs PostgreSQL 17.5 in-process: PGlite's WASI build of
// PostgreSQL, executed with wasmtime, behind a Unix socket that any
// PostgreSQL client in the process can connect to.
//
// Each instance is one PostgreSQL single-user backend, so it serves one
// session at a time; a second concurrent connection is refused with
// SQLSTATE 53300. When a connection closes, the next one gets a clean
// session. Instances are independent: a test can start one per worker,
// copied from a template made with CreateTemplate.
//
//	pg, err := pglite.New(pglite.Config{})
//	if err != nil { ... }
//	defer pg.Close()
//	pool, err := pg.Pool(ctx) // *pgxpool.Pool with one connection
package pglite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// PGlite is one embedded PostgreSQL instance.
type PGlite struct {
	cfg       Config
	dir       string // instance directory
	tempDir   bool   // dir is removed by Close
	socketDir string
	backend   *backend
	server    *server

	dbMu sync.Mutex
	db   *sql.DB
}

// New starts an instance. A fresh directory is initialised, which takes a
// few seconds; a clone of a template (Config.Template) or an existing
// cluster starts in a fraction of that.
func New(cfg Config) (_ *PGlite, err error) {
	cfg, err = cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	rt, err := loadRuntime(cfg.CacheDir)
	if err != nil {
		return nil, err
	}
	pg := &PGlite{cfg: cfg}
	if cfg.DataDir == "" {
		dir, err := os.MkdirTemp("", "go-pglite-*")
		if err != nil {
			return nil, fmt.Errorf("pglite: %w", err)
		}
		pg.dir, pg.tempDir = dir, true
	} else {
		pg.dir = cfg.DataDir
		if err := os.MkdirAll(pg.dir, 0o755); err != nil {
			return nil, fmt.Errorf("pglite: %w", err)
		}
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, pg.releaseDirs())
		}
	}()
	if cfg.Template != "" {
		if err := cloneTemplate(cfg.Template, pg.dir); err != nil {
			return nil, err
		}
	}

	start := time.Now()
	b, fresh, err := startBackend(rt, pg.dir, &cfg)
	if err != nil {
		return nil, err
	}
	if fresh {
		// initdb leaves the backend connected to template1 with initdb's
		// session settings. Finish the cluster and start again on the
		// configured database.
		initdbDone := time.Now()
		if err := b.shutdown(); err != nil {
			return nil, err
		}
		if b, fresh, err = startBackend(rt, pg.dir, &cfg); err != nil {
			return nil, err
		}
		if fresh {
			return nil, errors.Join(errors.New("pglite: initdb left no cluster in "+pg.dir), b.close())
		}
		cfg.Logger.Debug("pglite: cluster initialised", "dir", pg.dir, "initdb", initdbDone.Sub(start), "restart", time.Since(initdbDone))
	} else {
		cfg.Logger.Debug("pglite: backend started", "dir", pg.dir, "took", time.Since(start))
	}
	pg.backend = b
	defer func() {
		if err != nil {
			err = errors.Join(err, b.close())
		}
	}()

	// Unix socket paths are short-lived and limited to about 100 bytes, so
	// the socket gets its own directory under the system's temporary
	// directory rather than living in the instance directory.
	sockDir, err := os.MkdirTemp("", "pglite-*")
	if err != nil {
		return nil, fmt.Errorf("pglite: %w", err)
	}
	pg.socketDir = sockDir
	ln, err := net.Listen("unix", filepath.Join(sockDir, socketBase))
	if err != nil {
		return nil, fmt.Errorf("pglite: %w", err)
	}
	pg.server = newServer(ln, b, &cfg)
	return pg, nil
}

// ConnString returns a connection string for the instance, for pgx,
// database/sql drivers and other clients in the process. For a pgxpool, add
// pool_max_conns=1 or use Pool.
func (pg *PGlite) ConnString() string {
	return fmt.Sprintf("host=%s port=5432 user=%s dbname=%s sslmode=disable", pg.socketDir, User, Database)
}

// Pool returns a pgxpool with one connection, the most an instance serves.
func (pg *PGlite) Pool(ctx context.Context) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(pg.ConnString())
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 1
	return pgxpool.NewWithConfig(ctx, cfg)
}

// DB returns a database/sql handle on the instance (driver pgx) with one
// connection. It is closed by Close.
func (pg *PGlite) DB() *sql.DB {
	pg.dbMu.Lock()
	defer pg.dbMu.Unlock()
	if pg.db == nil {
		db, err := sql.Open("pgx", pg.ConnString())
		if err != nil {
			panic("pglite: opening database/sql handle: " + err.Error())
		}
		db.SetMaxOpenConns(1)
		pg.db = db
	}
	return pg.db
}

// DataDir returns the instance directory.
func (pg *PGlite) DataDir() string { return pg.dir }

// Err reports why the instance stopped serving connections, or nil while it
// is healthy: a trap of the module or a FATAL error, after which the
// backend's state is unknown. PostgreSQL's ordinary errors are not
// reported here; the backend recovers from them.
func (pg *PGlite) Err() error { return pg.backend.err() }

// Close ends the sessions, stops PostgreSQL and releases the instance; a
// temporary DataDir is removed.
func (pg *PGlite) Close() error {
	var err error
	pg.dbMu.Lock()
	if pg.db != nil {
		err = pg.db.Close()
		pg.db = nil
	}
	pg.dbMu.Unlock()
	err = errors.Join(err, pg.server.close(), pg.backend.shutdown(), pg.releaseDirs())
	return err
}

func (pg *PGlite) releaseDirs() error {
	var err error
	if pg.socketDir != "" {
		err = os.RemoveAll(pg.socketDir)
	}
	if pg.tempDir {
		err = errors.Join(err, os.RemoveAll(pg.dir))
	}
	return err
}
