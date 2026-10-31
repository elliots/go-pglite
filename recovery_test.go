package pglite_test

import (
	"database/sql"
	"strings"
	"testing"

	pglite "github.com/elliots/go-pglite"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// openPgx connects with pgx through database/sql. The recursion
// tests use it because their error reports carry one CONTEXT line per
// level, and lib/pq rejects an ErrorResponse longer than 30000 bytes (a
// libpq startup-only check that lib/pq applies to every message) and then
// loses protocol synchronisation.
func openPgx(t *testing.T, pg *pglite.PGlite) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", pg.ConnString()+"")
	if err != nil {
		t.Fatalf("sql.Open pgx: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

// These tests exercise PostgreSQL's own error recovery (ereport longjmps to
// the enclosing PG_TRY or to the main loop), which needs working
// setjmp/longjmp in the WASI module. Without it the first ERROR inside a
// nested PG_TRY traps the module.

func TestErrorThenNextQueryWorks(t *testing.T) {
	db := newTestPG(t).DB()

	var n int
	err := db.QueryRow("SELECT 1/0").Scan(&n)
	if err == nil || !strings.Contains(err.Error(), "division by zero") {
		t.Fatalf("SELECT 1/0: got %v, want division by zero", err)
	}
	if err := db.QueryRow("SELECT 42").Scan(&n); err != nil {
		t.Fatalf("query after error: %v", err)
	}
	if n != 42 {
		t.Errorf("got %d, want 42", n)
	}
}

func TestSavepointRollbackRecovers(t *testing.T) {
	db := newTestPG(t).DB()
	mustExec(t, db, "CREATE TABLE sp (id int PRIMARY KEY)")

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	if _, err := tx.Exec("INSERT INTO sp VALUES (1)"); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if _, err := tx.Exec("SAVEPOINT s"); err != nil {
		t.Fatalf("SAVEPOINT: %v", err)
	}
	_, err = tx.Exec("INSERT INTO sp VALUES (1)")
	if err == nil || !strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("duplicate insert: got %v, want duplicate key error", err)
	}
	if _, err := tx.Exec("ROLLBACK TO SAVEPOINT s"); err != nil {
		t.Fatalf("ROLLBACK TO SAVEPOINT: %v", err)
	}
	if _, err := tx.Exec("INSERT INTO sp VALUES (2)"); err != nil {
		t.Fatalf("insert after rollback to savepoint: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("COMMIT: %v", err)
	}

	var count int
	if err := db.QueryRow("SELECT count(*) FROM sp").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
}

func TestPlpgsqlExceptionBlockRecovers(t *testing.T) {
	db := newTestPG(t).DB()
	mustExec(t, db, `CREATE FUNCTION catch_div() RETURNS text LANGUAGE plpgsql AS $$
		BEGIN
			PERFORM 1/0;
			RETURN 'not reached';
		EXCEPTION WHEN division_by_zero THEN
			RETURN 'caught ' || SQLERRM;
		END $$`)

	var got string
	if err := db.QueryRow("SELECT catch_div()").Scan(&got); err != nil {
		t.Fatalf("SELECT catch_div(): %v", err)
	}
	if got != "caught division by zero" {
		t.Errorf("got %q, want %q", got, "caught division by zero")
	}

	// An exception block inside a transaction with data changes: the
	// subtransaction rolls back, the outer transaction continues.
	mustExec(t, db, "CREATE TABLE ex (id int PRIMARY KEY)")
	mustExec(t, db, `DO $$
		BEGIN
			INSERT INTO ex VALUES (1);
			BEGIN
				INSERT INTO ex VALUES (1);
			EXCEPTION WHEN unique_violation THEN
				INSERT INTO ex VALUES (2);
			END;
		END $$`)
	var ids string
	if err := db.QueryRow("SELECT string_agg(id::text, ',' ORDER BY id) FROM ex").Scan(&ids); err != nil {
		t.Fatalf("select: %v", err)
	}
	if ids != "1,2" {
		t.Errorf("ids = %q, want 1,2", ids)
	}
}

// A query longer than one socket read must reach PostgreSQL whole.
func TestLongQueryCrossesBridge(t *testing.T) {
	db := newTestPG(t).DB()

	var n int
	if err := db.QueryRow("SELECT length('" + strings.Repeat("x", 300000) + "')").Scan(&n); err != nil {
		t.Fatalf("300kB query: %v", err)
	}
	if n != 300000 {
		t.Errorf("length = %d, want 300000", n)
	}
}

// SQL-function recursion uses little shadow stack per level and so is the
// case most likely to exhaust the native stack before PostgreSQL's
// check fires; the module's max_stack_depth default keeps it ahead.
func TestSQLFunctionRecursionReportsStackDepth(t *testing.T) {
	db := openPgx(t, newTestPG(t))
	mustExec(t, db, `CREATE FUNCTION forever_sql(n int) RETURNS int LANGUAGE sql AS 'SELECT forever_sql($1 + 1)'`)

	var n int
	err := db.QueryRow("SELECT forever_sql(0)").Scan(&n)
	if err == nil || !strings.Contains(err.Error(), "stack depth limit exceeded") {
		t.Fatalf("SELECT forever_sql(0): got %v, want stack depth limit exceeded", err)
	}
	if err := db.QueryRow("SELECT 8").Scan(&n); err != nil {
		t.Fatalf("query after stack depth error: %v", err)
	}
}

func TestDeepRecursionReportsStackDepth(t *testing.T) {
	db := openPgx(t, newTestPG(t))
	mustExec(t, db, `CREATE FUNCTION forever(n int) RETURNS int LANGUAGE plpgsql AS $$
		BEGIN RETURN forever(n + 1); END $$`)

	var n int
	err := db.QueryRow("SELECT forever(0)").Scan(&n)
	if err == nil || !strings.Contains(err.Error(), "stack depth limit exceeded") {
		t.Fatalf("SELECT forever(0): got %v, want stack depth limit exceeded", err)
	}
	if err := db.QueryRow("SELECT 7").Scan(&n); err != nil {
		t.Fatalf("query after stack depth error: %v", err)
	}
	if n != 7 {
		t.Errorf("got %d, want 7", n)
	}
}
