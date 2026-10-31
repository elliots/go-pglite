package pglite_test

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	pglite "github.com/elliots/go-pglite"
)

// newBenchPG is newTestPG for benchmarks.
func newBenchPG(b *testing.B) *pglite.PGlite {
	b.Helper()
	pg, err := pglite.New(pglite.Config{DataDir: b.TempDir(), Template: pglite.TestTemplate})
	if err != nil {
		b.Fatalf("pglite.New: %v", err)
	}
	b.Cleanup(func() { pg.Close() })
	return pg
}

func mustExec(t testing.TB, db *sql.DB, query string) {
	t.Helper()
	if _, err := db.Exec(query); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func TestVersionIsPostgres17(t *testing.T) {
	var version string
	if err := newTestPG(t).DB().QueryRow("SELECT version()").Scan(&version); err != nil {
		t.Fatalf("SELECT version(): %v", err)
	}
	if !strings.HasPrefix(version, "PostgreSQL 17.5 ") {
		t.Errorf("version() = %q, want PostgreSQL 17.5", version)
	}
}

func TestDatabaseSQLHandle(t *testing.T) {
	db := newTestPG(t).DB()
	mustExec(t, db, "CREATE TABLE test (id int, name text)")
	mustExec(t, db, "INSERT INTO test VALUES (1, 'alice'), (2, 'bob')")

	rows, err := db.Query("SELECT id, name FROM test WHERE id >= $1 ORDER BY id", 1)
	if err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id int
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			t.Fatal(err)
		}
		got = append(got, name)
	}
	if strings.Join(got, ",") != "alice,bob" {
		t.Errorf("rows = %v", got)
	}

	var current string
	if err := db.QueryRow("SELECT current_database() || '/' || current_user").Scan(&current); err != nil || current != "postgres/postgres" {
		t.Errorf("current_database/current_user = %q, %v", current, err)
	}
}

func TestSessionDefaultsMatchPostgreSQL(t *testing.T) {
	db := newTestPG(t).DB()
	checks := map[string]string{
		"SHOW search_path":     `"$user", public`,
		"SHOW TimeZone":        "UTC",
		"SHOW client_encoding": "UTF8",
		"SHOW max_stack_depth": "256kB",
	}
	for q, want := range checks {
		var got string
		if err := db.QueryRow(q).Scan(&got); err != nil {
			t.Errorf("%s: %v", q, err)
		} else if got != want {
			t.Errorf("%s = %q, want %q", q, got, want)
		}
	}
	// Strong random values differ between calls: the module reads the
	// host's /dev/urandom.
	var a, b string
	if err := db.QueryRow("SELECT gen_random_uuid()::text, gen_random_uuid()::text").Scan(&a, &b); err != nil || a == b {
		t.Errorf("gen_random_uuid: %q %q, %v", a, b, err)
	}
	if err := db.QueryRow("SELECT gen_random_uuid()::text").Scan(&b); err != nil || a == b {
		t.Errorf("gen_random_uuid across statements: %q %q, %v", a, b, err)
	}
	var ts time.Time
	if err := db.QueryRow("SELECT '2024-06-15 12:00:00+00'::timestamptz").Scan(&ts); err != nil || !ts.Equal(time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("timestamptz = %v, %v", ts, err)
	}
}

func TestInformationSchemaAndCatalog(t *testing.T) {
	db := newTestPG(t).DB()
	mustExec(t, db, `CREATE TABLE users (
		id serial PRIMARY KEY, name text NOT NULL, email varchar(255) UNIQUE,
		active boolean DEFAULT true, created_at timestamp DEFAULT now())`)
	mustExec(t, db, `CREATE INDEX idx_users_name ON users (name)`)

	var cols int
	if err := db.QueryRow(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'users'`).Scan(&cols); err != nil || cols != 5 {
		t.Errorf("information_schema.columns: %d, %v", cols, err)
	}
	var idx int
	if err := db.QueryRow(`SELECT count(*) FROM pg_indexes WHERE tablename = 'users'`).Scan(&idx); err != nil || idx != 3 {
		t.Errorf("pg_indexes: %d, %v", idx, err)
	}
}

func TestCascadeDelete(t *testing.T) {
	db := newTestPG(t).DB()
	mustExec(t, db, `CREATE TABLE departments (id serial PRIMARY KEY, name text NOT NULL)`)
	mustExec(t, db, `CREATE TABLE employees (id serial PRIMARY KEY, dept_id int REFERENCES departments(id) ON DELETE CASCADE)`)
	mustExec(t, db, `INSERT INTO departments (name) VALUES ('engineering')`)
	mustExec(t, db, `INSERT INTO employees (dept_id) VALUES (1)`)
	mustExec(t, db, `DELETE FROM departments WHERE id = 1`)
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM employees`).Scan(&count); err != nil || count != 0 {
		t.Errorf("employees after cascade: %d, %v", count, err)
	}
}

func TestCloseIsIdempotentAndReportsNoError(t *testing.T) {
	pg, err := pglite.New(pglite.Config{DataDir: t.TempDir(), Template: pglite.TestTemplate})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pg.DB().QueryRow("SELECT 1").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if err := pg.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := pg.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}
