package pglite_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func pgErrCode(err error) string {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		return pgErr.Code
	}
	return ""
}

func TestSecondConcurrentConnectionIsRefused(t *testing.T) {
	ctx := t.Context()
	pg := newTestPG(t)
	first := connect(t, pg)

	start := time.Now()
	_, err := pgx.Connect(ctx, pg.ConnString())
	if code := pgErrCode(err); code != "53300" {
		t.Fatalf("second connection: %v, want SQLSTATE 53300 too_many_connections", err)
	}
	if !strings.Contains(err.Error(), "one session at a time") {
		t.Errorf("error does not explain the limit: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("refusal took %v", took)
	}

	// The first session is unaffected.
	var n int
	if err := first.QueryRow(ctx, "SELECT 1").Scan(&n); err != nil || n != 1 {
		t.Errorf("first connection after refusal: %d, %v", n, err)
	}
	// Once it ends, the next connection is served.
	first.Close(ctx)
	second := connect(t, pg)
	if err := second.QueryRow(ctx, "SELECT 2").Scan(&n); err != nil || n != 2 {
		t.Errorf("connection after close: %d, %v", n, err)
	}
}

func TestNewConnectionGetsCleanSession(t *testing.T) {
	ctx := t.Context()
	pg := newTestPG(t)
	setup := connect(t, pg)
	exec(t, setup, "CREATE TABLE committed (id int)")
	setup.Close(ctx)

	first := connect(t, pg)
	exec(t, first, "SET application_name = 'dirty'")
	exec(t, first, "SET search_path = pg_catalog")
	exec(t, first, "SET TimeZone = 'Asia/Tokyo'")
	exec(t, first, "PREPARE sqlstmt AS SELECT 1")
	if _, err := first.Prepare(ctx, "pgxstmt", "SELECT 2"); err != nil {
		t.Fatal(err)
	}
	exec(t, first, "CREATE TEMP TABLE scratch (id int)")
	exec(t, first, "SELECT pg_advisory_lock(42)")
	exec(t, first, "LISTEN channel")
	exec(t, first, "CREATE SEQUENCE seq")
	exec(t, first, "SELECT nextval('seq')")
	// An open transaction with uncommitted data.
	exec(t, first, "BEGIN")
	exec(t, first, "INSERT INTO public.committed VALUES (1)")
	first.Close(ctx)

	second := connect(t, pg)
	checks := []struct {
		sql, want string
	}{
		{"SHOW application_name", ""},
		{"SHOW search_path", `"$user", public`},
		{"SHOW TimeZone", "UTC"},
		// pgx's own statement cache adds to pg_prepared_statements, so only
		// the first session's names are counted.
		{"SELECT count(*)::text FROM pg_prepared_statements WHERE name IN ('sqlstmt', 'pgxstmt')", "0"},
		{"SELECT (to_regclass('scratch') IS NULL)::text", "true"},
		{"SELECT count(*)::text FROM pg_locks WHERE locktype = 'advisory'", "0"},
		{"SELECT count(*)::text FROM pg_listening_channels()", "0"},
		{"SELECT count(*)::text FROM committed", "0"},
		{"SELECT pg_typeof(1)::text", "integer"}, // the transaction state is idle
	}
	for _, c := range checks {
		var got string
		if err := second.QueryRow(ctx, c.sql).Scan(&got); err != nil {
			t.Errorf("%s: %v", c.sql, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s = %q, want %q", c.sql, got, c.want)
		}
	}
	// currval would be an error in a fresh session.
	_, err := second.Exec(ctx, "SELECT currval('seq')")
	if code := pgErrCode(err); code != "55000" {
		t.Errorf("currval in new session: %v, want object_not_in_prerequisite_state", err)
	}
	if s := second.PgConn().TxStatus(); s != 'I' {
		t.Errorf("transaction status %q, want idle", s)
	}
}

// A client that vanishes without Terminate, in the middle of a pipeline
// (no Sync) and inside an explicit transaction, leaves nothing behind.
func TestAbruptDisconnectLeavesCleanSession(t *testing.T) {
	ctx := t.Context()
	pg := newTestPG(t)
	setup := connect(t, pg)
	exec(t, setup, "CREATE TABLE abrupt (id int)")
	setup.Close(ctx)

	cases := map[string]func(t *testing.T, c *pgconn.PgConn){
		"implicit transaction without Sync": func(t *testing.T, c *pgconn.PgConn) {
			p := c.StartPipeline(ctx)
			p.SendQueryParams("INSERT INTO abrupt VALUES (1)", nil, nil, nil, nil)
			p.SendFlushRequest()
			if err := p.Flush(); err != nil {
				t.Fatal(err)
			}
			if _, err := p.GetResults(); err != nil { // the insert's result
				t.Fatal(err)
			}
		},
		"error without Sync": func(t *testing.T, c *pgconn.PgConn) {
			p := c.StartPipeline(ctx)
			p.SendQueryParams("SELEC 1", nil, nil, nil, nil)
			p.SendFlushRequest()
			if err := p.Flush(); err != nil {
				t.Fatal(err)
			}
			if _, err := p.GetResults(); pgErrCode(err) != "42601" {
				t.Fatalf("got %v, want syntax error", err)
			}
		},
		"open transaction block": func(t *testing.T, c *pgconn.PgConn) {
			if err := c.Exec(ctx, "BEGIN; INSERT INTO abrupt VALUES (2); SET application_name = 'gone'").Close(); err != nil {
				t.Fatal(err)
			}
		},
		"aborted transaction block": func(t *testing.T, c *pgconn.PgConn) {
			if err := c.Exec(ctx, "BEGIN; INSERT INTO abrupt VALUES (3)").Close(); err != nil {
				t.Fatal(err)
			}
			if err := c.Exec(ctx, "SELECT 1/0").Close(); pgErrCode(err) != "22012" {
				t.Fatalf("got %v, want division by zero", err)
			}
		},
	}
	for name, dirty := range cases {
		t.Run(name, func(t *testing.T) {
			c, err := pgconn.Connect(ctx, pg.ConnString())
			if err != nil {
				t.Fatal(err)
			}
			dirty(t, c)
			if err := c.Conn().Close(); err != nil { // the socket, without Terminate
				t.Fatal(err)
			}

			next := connect(t, pg)
			var n int
			var name string
			if err := next.QueryRow(ctx, "SELECT count(*), current_setting('application_name') FROM abrupt").Scan(&n, &name); err != nil {
				t.Fatalf("next session: %v", err)
			}
			if n != 0 || name != "" {
				t.Errorf("next session sees %d rows and application_name %q", n, name)
			}
			next.Close(ctx)
		})
	}
	if err := pg.Err(); err != nil {
		t.Errorf("instance failed: %v", err)
	}
}

func TestPoolReopensItsConnection(t *testing.T) {
	ctx := t.Context()
	pg := newTestPG(t)
	pool, err := pg.Pool(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, "CREATE TABLE p (id int)"); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		// Reset closes the connection; the next use opens a new one while
		// the server may still be resetting the old session.
		pool.Reset()
		if _, err := pool.Exec(ctx, "INSERT INTO p VALUES ($1)", i); err != nil {
			t.Fatalf("after reset %d: %v", i, err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM p").Scan(&n); err != nil || n != 5 {
		t.Errorf("count = %d, %v", n, err)
	}
	if pool.Config().MaxConns != 1 {
		t.Errorf("MaxConns = %d", pool.Config().MaxConns)
	}
}

func TestStartupParametersAreApplied(t *testing.T) {
	ctx := t.Context()
	pg := newTestPG(t)
	conn := connect(t, pg, "application_name=myapp search_path=pg_catalog options='-c\\ work_mem=8MB'")

	var app, path, mem string
	if err := conn.QueryRow(ctx, "SELECT current_setting('application_name'), current_setting('search_path'), current_setting('work_mem')").Scan(&app, &path, &mem); err != nil {
		t.Fatal(err)
	}
	if app != "myapp" || path != "pg_catalog" || mem != "8MB" {
		t.Errorf("application_name %q, search_path %q, work_mem %q", app, path, mem)
	}
	// Reported parameters arrive as ParameterStatus during startup.
	if got := conn.PgConn().ParameterStatus("application_name"); got != "myapp" {
		t.Errorf("ParameterStatus(application_name) = %q", got)
	}
	if got := conn.PgConn().ParameterStatus("server_version"); !strings.HasPrefix(got, "17.5") {
		t.Errorf("ParameterStatus(server_version) = %q", got)
	}
	if got := conn.PgConn().ParameterStatus("TimeZone"); got != "UTC" {
		t.Errorf("ParameterStatus(TimeZone) = %q", got)
	}
	conn.Close(ctx)

	// A parameter PostgreSQL rejects fails the connection, as a server would.
	_, err := pgx.Connect(ctx, pg.ConnString()+" no_such_setting=1")
	if code := pgErrCode(err); code != "42704" {
		t.Errorf("unknown startup parameter: %v, want undefined_object", err)
	}
	_, err = pgx.Connect(ctx, pg.ConnString()+" replication=database")
	if code := pgErrCode(err); code != "0A000" {
		t.Errorf("replication: %v, want feature_not_supported", err)
	}
}

func TestOtherUsersAndDatabasesAreRefused(t *testing.T) {
	ctx := t.Context()
	pg := newTestPG(t)
	base := strings.Replace(strings.Replace(pg.ConnString(), "user=postgres", "", 1), "dbname=postgres", "", 1)

	_, err := pgx.Connect(ctx, base+" user=bob dbname=postgres")
	if code := pgErrCode(err); code != "28000" {
		t.Errorf("user bob: %v, want invalid_authorization_specification", err)
	}
	_, err = pgx.Connect(ctx, base+" user=postgres dbname=other")
	if code := pgErrCode(err); code != "3D000" {
		t.Errorf("database other: %v, want invalid_catalog_name", err)
	}
	// After the refusals the instance serves the right credentials.
	conn := connect(t, pg)
	var n int
	if err := conn.QueryRow(ctx, "SELECT 1").Scan(&n); err != nil {
		t.Error(err)
	}
}

// Signals cannot be sent inside the module, so pg_terminate_backend and
// pg_cancel_backend report failure and do nothing; the session goes on.
func TestBackendSignalsHaveNoEffect(t *testing.T) {
	ctx := t.Context()
	pg := newTestPG(t)
	conn := connect(t, pg)
	var ok bool
	if err := conn.QueryRow(ctx, "SELECT pg_terminate_backend(pg_backend_pid())").Scan(&ok); err != nil || ok {
		t.Fatalf("terminate: %v, %v; want false without error", ok, err)
	}
	var n int
	if err := conn.QueryRow(ctx, "SELECT 1").Scan(&n); err != nil || n != 1 {
		t.Errorf("after terminate: %d, %v", n, err)
	}
	if err := pg.Err(); err != nil {
		t.Errorf("instance failed: %v", err)
	}
}

func TestCancelRequestIsIgnored(t *testing.T) {
	ctx := t.Context()
	pg := newTestPG(t)
	conn := connect(t, pg)
	cancelCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	// pgx sends a CancelRequest on a new connection and closes the
	// query's connection; the statement itself runs to completion.
	_, err := conn.Exec(cancelCtx, "SELECT pg_sleep(1)")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled query: %v", err)
	}
	next := connect(t, pg)
	var n int
	if err := next.QueryRow(ctx, "SELECT 3").Scan(&n); err != nil || n != 3 {
		t.Errorf("after cancel: %d, %v", n, err)
	}
	if err := pg.Err(); err != nil {
		t.Errorf("instance failed: %v", err)
	}
}
