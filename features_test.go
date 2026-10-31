package pglite_test

import (
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// These tests exercise the PostgreSQL features applications lean on, through
// pgx's default extended query protocol.

func TestSchemasAndSearchPath(t *testing.T) {
	ctx := t.Context()
	conn := connect(t, newTestPG(t))
	exec(t, conn, "CREATE SCHEMA app")
	exec(t, conn, "CREATE TABLE app.items (id int PRIMARY KEY, name text)")
	exec(t, conn, "CREATE TABLE public.items (id int PRIMARY KEY)")
	exec(t, conn, "INSERT INTO app.items VALUES (1, 'in app')")

	var name string
	if err := conn.QueryRow(ctx, "SELECT name FROM app.items").Scan(&name); err != nil || name != "in app" {
		t.Errorf("qualified select: %q, %v", name, err)
	}
	exec(t, conn, "SET search_path TO app, public")
	if err := conn.QueryRow(ctx, "SELECT name FROM items").Scan(&name); err != nil || name != "in app" {
		t.Errorf("select through search_path: %q, %v", name, err)
	}
	var schemas []string
	rows, err := conn.Query(ctx, "SELECT schema_name FROM information_schema.schemata WHERE schema_name IN ('app', 'public') ORDER BY 1")
	if err != nil {
		t.Fatal(err)
	}
	if schemas, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil || strings.Join(schemas, ",") != "app,public" {
		t.Errorf("schemata: %v, %v", schemas, err)
	}
	var owner string
	if err := conn.QueryRow(ctx, "SELECT nspowner::regrole::text FROM pg_namespace WHERE nspname = 'app'").Scan(&owner); err != nil || owner != "postgres" {
		t.Errorf("pg_namespace: %q, %v", owner, err)
	}
	exec(t, conn, "DROP SCHEMA app CASCADE")
	if _, err := conn.Exec(ctx, "SELECT * FROM app.items"); pgErrCode(err) != "42P01" {
		t.Errorf("after DROP SCHEMA: %v, want undefined_table", err)
	}
}

func TestDDLInTransactionsAndCursors(t *testing.T) {
	ctx := t.Context()
	conn := connect(t, newTestPG(t))

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{"CREATE TABLE t (id int)", "ALTER TABLE t ADD COLUMN name text", "CREATE INDEX ON t (name)", "INSERT INTO t VALUES (1, 'a')"} {
		if _, err := tx.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT to_regclass('t') IS NOT NULL").Scan(&exists); err != nil || exists {
		t.Errorf("table survived the rolled-back transaction: %v, %v", exists, err)
	}

	tx, err = conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{"CREATE TABLE t (id int)", "ALTER TABLE t ADD COLUMN name text", "INSERT INTO t SELECT i, 'n' || i FROM generate_series(1, 10) i"} {
		if _, err := tx.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	// A cursor lives in the transaction.
	if _, err := tx.Exec(ctx, "DECLARE c CURSOR FOR SELECT id FROM t ORDER BY id"); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, "FETCH 3 FROM c")
	if err != nil {
		t.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil || len(ids) != 3 || ids[2] != 3 {
		t.Errorf("FETCH 3: %v, %v", ids, err)
	}
	if _, err := tx.Exec(ctx, "MOVE 5 FROM c"); err != nil {
		t.Fatal(err)
	}
	var id int
	if err := tx.QueryRow(ctx, "FETCH NEXT FROM c").Scan(&id); err != nil || id != 9 {
		t.Errorf("FETCH after MOVE: %d, %v", id, err)
	}
	if _, err := tx.Exec(ctx, "CLOSE c"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var cols int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM information_schema.columns WHERE table_name = 't'").Scan(&cols); err != nil || cols != 2 {
		t.Errorf("columns after commit: %d, %v", cols, err)
	}
	// Transaction-level read-only and isolation settings are honoured.
	tx, err = conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, "INSERT INTO t VALUES (11, 'x')")
	if pgErrCode(err) != "25006" {
		t.Errorf("insert in a read-only transaction: %v, want read_only_sql_transaction", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAdvisoryLocks(t *testing.T) {
	ctx := t.Context()
	conn := connect(t, newTestPG(t))
	var got bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", int64(42)).Scan(&got); err != nil || !got {
		t.Fatalf("pg_try_advisory_lock: %v, %v", got, err)
	}
	var locks int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND objid = 42").Scan(&locks); err != nil || locks != 1 {
		t.Errorf("pg_locks: %d, %v", locks, err)
	}
	// Session locks are reentrant and need as many unlocks.
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(42)").Scan(&got); err != nil || !got {
		t.Errorf("second pg_try_advisory_lock: %v, %v", got, err)
	}
	for range 2 {
		if err := conn.QueryRow(ctx, "SELECT pg_advisory_unlock(42)").Scan(&got); err != nil || !got {
			t.Errorf("pg_advisory_unlock: %v, %v", got, err)
		}
	}
	// Unlocking what is not held fails with a WARNING, not an error.
	if err := conn.QueryRow(ctx, "SELECT pg_advisory_unlock(42)").Scan(&got); err != nil || got {
		t.Errorf("pg_advisory_unlock of a free lock: %v, %v", got, err)
	}
	// Transaction-level locks end with the transaction.
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock(7, 8)").Scan(&got); err != nil || !got {
		t.Errorf("pg_try_advisory_xact_lock: %v, %v", got, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM pg_locks WHERE locktype = 'advisory'").Scan(&locks); err != nil || locks != 0 {
		t.Errorf("advisory locks after commit: %d, %v", locks, err)
	}
}

func TestQueueOperations(t *testing.T) {
	ctx := t.Context()
	conn := connect(t, newTestPG(t))
	exec(t, conn, `CREATE TABLE jobs (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, key text UNIQUE NOT NULL,
		attempts int NOT NULL DEFAULT 0, run_at timestamptz NOT NULL DEFAULT now())`)

	var id int64
	if err := conn.QueryRow(ctx, "INSERT INTO jobs (key) VALUES ($1) RETURNING id", "a").Scan(&id); err != nil || id != 1 {
		t.Fatalf("INSERT RETURNING: %d, %v", id, err)
	}
	// ON CONFLICT DO UPDATE with EXCLUDED and RETURNING.
	var attempts int
	if err := conn.QueryRow(ctx, `INSERT INTO jobs (key) VALUES ($1)
		ON CONFLICT (key) DO UPDATE SET attempts = jobs.attempts + 1 RETURNING id, attempts`, "a").Scan(&id, &attempts); err != nil || id != 1 || attempts != 1 {
		t.Errorf("ON CONFLICT DO UPDATE: %d %d, %v", id, attempts, err)
	}
	tag, err := conn.Exec(ctx, "INSERT INTO jobs (key) VALUES ($1), ($2) ON CONFLICT DO NOTHING", "a", "b")
	if err != nil || tag.RowsAffected() != 1 {
		t.Errorf("ON CONFLICT DO NOTHING: %v, %v", tag, err)
	}
	// Claiming with FOR UPDATE SKIP LOCKED.
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, "SELECT id FROM jobs WHERE run_at <= now() ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1")
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil || len(claimed) != 1 || claimed[0] != 1 {
		t.Errorf("SKIP LOCKED claim: %v, %v", claimed, err)
	}
	var deleted string
	if err := tx.QueryRow(ctx, "DELETE FROM jobs WHERE id = $1 RETURNING key", claimed[0]).Scan(&deleted); err != nil || deleted != "a" {
		t.Errorf("DELETE RETURNING: %q, %v", deleted, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// UPDATE ... RETURNING with a FROM clause and a CTE.
	var updated []string
	rows, err = conn.Query(ctx, `WITH due AS (SELECT id FROM jobs WHERE attempts < 3)
		UPDATE jobs SET attempts = attempts + 1 FROM due WHERE jobs.id = due.id RETURNING key`)
	if err != nil {
		t.Fatal(err)
	}
	if updated, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil || len(updated) != 1 || updated[0] != "b" {
		t.Errorf("UPDATE ... FROM ... RETURNING: %v, %v", updated, err)
	}
}

func TestSequencesAndIdentityColumns(t *testing.T) {
	ctx := t.Context()
	conn := connect(t, newTestPG(t))
	exec(t, conn, "CREATE TABLE ident (id int GENERATED ALWAYS AS IDENTITY, v text)")
	exec(t, conn, "CREATE TABLE ser (id serial PRIMARY KEY, v text)")
	exec(t, conn, "CREATE SEQUENCE counter START 100 INCREMENT 10")

	var a, b int
	if err := conn.QueryRow(ctx, "INSERT INTO ident (v) VALUES ('x') RETURNING id").Scan(&a); err != nil || a != 1 {
		t.Errorf("first identity: %d, %v", a, err)
	}
	_, err := conn.Exec(ctx, "INSERT INTO ident (id, v) VALUES (5, 'x')")
	if pgErrCode(err) != "428C9" {
		t.Errorf("explicit value for GENERATED ALWAYS: %v, want generated_always", err)
	}
	if err := conn.QueryRow(ctx, "INSERT INTO ident (id, v) OVERRIDING SYSTEM VALUE VALUES (5, 'x') RETURNING id").Scan(&b); err != nil || b != 5 {
		t.Errorf("OVERRIDING SYSTEM VALUE: %d, %v", b, err)
	}
	if err := conn.QueryRow(ctx, "INSERT INTO ident (v) VALUES ('y') RETURNING id").Scan(&b); err != nil || b != 2 {
		t.Errorf("identity after override: %d, %v", b, err)
	}
	var seq string
	if err := conn.QueryRow(ctx, "SELECT pg_get_serial_sequence('ser', 'id')").Scan(&seq); err != nil || seq != "public.ser_id_seq" {
		t.Errorf("pg_get_serial_sequence: %q, %v", seq, err)
	}
	var n int64
	if err := conn.QueryRow(ctx, "SELECT nextval('counter')").Scan(&n); err != nil || n != 100 {
		t.Errorf("nextval: %d, %v", n, err)
	}
	if err := conn.QueryRow(ctx, "SELECT nextval('counter')").Scan(&n); err != nil || n != 110 {
		t.Errorf("second nextval: %d, %v", n, err)
	}
	if err := conn.QueryRow(ctx, "SELECT setval('counter', 500), currval('counter')").Scan(&n, &n); err != nil || n != 500 {
		t.Errorf("setval/currval: %d, %v", n, err)
	}
	// A sequence value consumed in a rolled-back transaction is not reused.
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO ser (v) VALUES ('lost')"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, "INSERT INTO ser (v) VALUES ('kept') RETURNING id").Scan(&a); err != nil || a != 2 {
		t.Errorf("serial after rollback: %d, %v", a, err)
	}
}

func TestJSONBArraysNumericsAndTimestamps(t *testing.T) {
	ctx := t.Context()
	conn := connect(t, newTestPG(t))
	exec(t, conn, "CREATE TABLE docs (id int PRIMARY KEY, doc jsonb, tags text[], amount numeric(12,4), at timestamptz)")
	exec(t, conn, `INSERT INTO docs VALUES
		(1, '{"name": "alice", "age": 30, "roles": ["admin", "dev"]}', '{"a","b"}', 10.5, '2024-03-10 12:00:00+00'),
		(2, '{"name": "bob", "age": 25, "roles": ["dev"]}', '{"b","c"}', 0.1, '2024-03-10 23:30:00+00')`)

	// jsonb: operators, containment, path updates and aggregation.
	var name string
	var roles []string
	if err := conn.QueryRow(ctx, "SELECT doc->>'name', ARRAY(SELECT jsonb_array_elements_text(doc->'roles')) FROM docs WHERE doc @> $1", `{"age": 30}`).Scan(&name, &roles); err != nil || name != "alice" || len(roles) != 2 {
		t.Errorf("jsonb operators: %q %v, %v", name, roles, err)
	}
	var doc map[string]any
	if err := conn.QueryRow(ctx, "UPDATE docs SET doc = jsonb_set(doc, '{age}', '31') WHERE id = 1 RETURNING doc").Scan(&doc); err != nil || doc["age"] != float64(31) {
		t.Errorf("jsonb_set: %v, %v", doc, err)
	}
	var agg []map[string]any
	if err := conn.QueryRow(ctx, "SELECT jsonb_agg(doc - 'roles' ORDER BY id) FROM docs").Scan(&agg); err != nil || len(agg) != 2 || agg[1]["name"] != "bob" {
		t.Errorf("jsonb_agg: %v, %v", agg, err)
	}
	var n int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM docs WHERE doc ? 'age' AND (doc->>'age')::int > $1", 26).Scan(&n); err != nil || n != 1 {
		t.Errorf("jsonb ? and cast: %d, %v", n, err)
	}

	// arrays: parameters, ANY, containment, unnest and aggregation.
	var ids []int32
	rows, err := conn.Query(ctx, "SELECT id FROM docs WHERE tags && $1 ORDER BY id", []string{"c", "z"})
	if err != nil {
		t.Fatal(err)
	}
	if ids, err = pgx.CollectRows(rows, pgx.RowTo[int32]); err != nil || len(ids) != 1 || ids[0] != 2 {
		t.Errorf("array overlap: %v, %v", ids, err)
	}
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM docs WHERE id = ANY($1)", []int32{1, 2, 3}).Scan(&n); err != nil || n != 2 {
		t.Errorf("= ANY: %d, %v", n, err)
	}
	var tags []string
	if err := conn.QueryRow(ctx, "SELECT array_agg(DISTINCT t ORDER BY t) FROM docs, unnest(tags) t").Scan(&tags); err != nil || strings.Join(tags, "") != "abc" {
		t.Errorf("unnest/array_agg: %v, %v", tags, err)
	}
	if err := conn.QueryRow(ctx, "SELECT array_length(tags || $1::text[], 1) FROM docs WHERE id = 1", []string{"d"}).Scan(&n); err != nil || n != 3 {
		t.Errorf("array concatenation: %d, %v", n, err)
	}

	// numeric: exact decimal arithmetic.
	var sum pgtype.Numeric
	if err := conn.QueryRow(ctx, "SELECT sum(amount) FROM docs").Scan(&sum); err != nil {
		t.Fatal(err)
	}
	if s, _ := sum.Value(); s != "10.6000" {
		t.Errorf("sum(numeric) = %v, want 10.6000", s)
	}
	var exact pgtype.Numeric
	if err := conn.QueryRow(ctx, "SELECT $1::numeric + $2::numeric", pgtype.Numeric{Int: big.NewInt(1), Exp: -1, Valid: true}, pgtype.Numeric{Int: big.NewInt(2), Exp: -1, Valid: true}).Scan(&exact); err != nil {
		t.Fatal(err)
	}
	if s, _ := exact.Value(); s != "0.3" {
		t.Errorf("0.1 + 0.2 = %v", s)
	}
	var text string
	if err := conn.QueryRow(ctx, "SELECT (12345678901234567890.123456789::numeric * 2)::text").Scan(&text); err != nil || text != "24691357802469135780.246913578" {
		t.Errorf("big numeric: %q, %v", text, err)
	}

	// timestamptz: zone conversion, truncation, intervals and ranges.
	var at time.Time
	if err := conn.QueryRow(ctx, "SELECT at AT TIME ZONE 'Asia/Tokyo' FROM docs WHERE id = 2").Scan(&at); err != nil || at.Hour() != 8 || at.Day() != 11 {
		t.Errorf("AT TIME ZONE: %v, %v", at, err)
	}
	exec(t, conn, "SET TimeZone = 'America/New_York'")
	if err := conn.QueryRow(ctx, "SELECT at FROM docs WHERE id = 1").Scan(&at); err != nil || !at.Equal(time.Date(2024, 3, 10, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("timestamptz in another zone: %v, %v", at, err)
	}
	var day string
	if err := conn.QueryRow(ctx, "SELECT date_trunc('day', at)::date::text FROM docs WHERE id = 2").Scan(&day); err != nil || day != "2024-03-10" {
		t.Errorf("date_trunc in New York: %q, %v", day, err)
	}
	// Month arithmetic happens in the session's time zone.
	exec(t, conn, "SET TimeZone = 'UTC'")
	var later time.Time
	if err := conn.QueryRow(ctx, "SELECT $1::timestamptz + $2::interval", time.Date(2024, 1, 31, 0, 0, 0, 0, time.UTC), "1 month").Scan(&later); err != nil || !later.Equal(time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("interval arithmetic: %v, %v", later, err)
	}
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM docs WHERE at <@ tstzrange($1, $2)", time.Date(2024, 3, 10, 0, 0, 0, 0, time.UTC), time.Date(2024, 3, 10, 13, 0, 0, 0, time.UTC)).Scan(&n); err != nil || n != 1 {
		t.Errorf("tstzrange: %d, %v", n, err)
	}
	// uuid: generation and comparison.
	var u1, u2 [16]byte
	if err := conn.QueryRow(ctx, "SELECT gen_random_uuid(), gen_random_uuid()").Scan(&u1, &u2); err != nil || u1 == u2 {
		t.Errorf("gen_random_uuid: %v %v, %v", u1, u2, err)
	}
	var same bool
	if err := conn.QueryRow(ctx, "SELECT $1::uuid = $2::uuid", u1, u1).Scan(&same); err != nil || !same {
		t.Errorf("uuid comparison: %v, %v", same, err)
	}
}

func TestCatalogAndInformationSchemaReads(t *testing.T) {
	ctx := t.Context()
	conn := connect(t, newTestPG(t))
	exec(t, conn, "CREATE TABLE parent (id int PRIMARY KEY, name text NOT NULL)")
	exec(t, conn, "CREATE TABLE child (id int PRIMARY KEY, parent_id int NOT NULL REFERENCES parent(id) ON DELETE CASCADE, note text)")
	exec(t, conn, "CREATE INDEX child_parent_idx ON child (parent_id)")
	exec(t, conn, "COMMENT ON TABLE child IS 'children'")
	exec(t, conn, "CREATE VIEW child_names AS SELECT c.id, p.name FROM child c JOIN parent p ON p.id = c.parent_id")

	type column struct {
		Name, Type string
		Nullable   bool
	}
	rows, err := conn.Query(ctx, `SELECT a.attname, format_type(a.atttypid, a.atttypmod), NOT a.attnotnull
		FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relname = 'child' AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum`)
	if err != nil {
		t.Fatal(err)
	}
	cols, err := pgx.CollectRows(rows, pgx.RowToStructByPos[column])
	if err != nil || len(cols) != 3 || cols[1] != (column{"parent_id", "integer", false}) || cols[2] != (column{"note", "text", true}) {
		t.Errorf("pg_attribute: %v, %v", cols, err)
	}

	rows, err = conn.Query(ctx, `SELECT tc.constraint_type, kcu.column_name
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu ON kcu.constraint_name = tc.constraint_name
		WHERE tc.table_name = 'child' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	constraints, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Type, Column string }])
	if err != nil || len(constraints) != 2 || constraints[0].Type != "FOREIGN KEY" || constraints[0].Column != "parent_id" || constraints[1].Type != "PRIMARY KEY" {
		t.Errorf("table_constraints: %v, %v", constraints, err)
	}
	var fkTarget string
	if err := conn.QueryRow(ctx, `SELECT confrelid::regclass::text || '.' || a.attname FROM pg_constraint c
		JOIN pg_attribute a ON a.attrelid = c.confrelid AND a.attnum = c.confkey[1]
		WHERE c.conrelid = 'child'::regclass AND c.contype = 'f'`).Scan(&fkTarget); err != nil || fkTarget != "parent.id" {
		t.Errorf("pg_constraint: %q, %v", fkTarget, err)
	}
	rows, err = conn.Query(ctx, "SELECT table_name || ':' || table_type FROM information_schema.tables WHERE table_schema = 'public' ORDER BY 1")
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || strings.Join(tables, " ") != "child:BASE TABLE child_names:VIEW parent:BASE TABLE" {
		t.Errorf("information_schema.tables: %v, %v", tables, err)
	}
	var indexdef, comment, viewdef string
	if err := conn.QueryRow(ctx, "SELECT indexdef FROM pg_indexes WHERE indexname = 'child_parent_idx'").Scan(&indexdef); err != nil || !strings.HasSuffix(indexdef, "USING btree (parent_id)") {
		t.Errorf("pg_indexes: %q, %v", indexdef, err)
	}
	if err := conn.QueryRow(ctx, "SELECT obj_description('child'::regclass, 'pg_class')").Scan(&comment); err != nil || comment != "children" {
		t.Errorf("obj_description: %q, %v", comment, err)
	}
	if err := conn.QueryRow(ctx, "SELECT pg_get_viewdef('child_names', true)").Scan(&viewdef); err != nil || !strings.Contains(viewdef, "JOIN parent p") {
		t.Errorf("pg_get_viewdef: %q, %v", viewdef, err)
	}
	var settings int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM pg_settings WHERE name IN ('max_stack_depth', 'search_path')").Scan(&settings); err != nil || settings != 2 {
		t.Errorf("pg_settings: %d, %v", settings, err)
	}
}

// Describe, binary results and portals through pgconn directly.
func TestExtendedProtocolDescribeAndBinaryResults(t *testing.T) {
	ctx := t.Context()
	pg := newTestPG(t)
	conn, err := pgconn.Connect(ctx, pg.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if err := conn.Exec(ctx, "CREATE TABLE d (id int PRIMARY KEY, n numeric, b bool, at timestamptz); INSERT INTO d VALUES (1, 2.5, true, '2024-01-02 03:04:05+00')").Close(); err != nil {
		t.Fatal(err)
	}

	sd, err := conn.Prepare(ctx, "sel", "SELECT id, n, b, at FROM d WHERE id = $1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(sd.ParamOIDs) != 1 || sd.ParamOIDs[0] != pgtype.Int4OID || len(sd.Fields) != 4 || sd.Fields[1].DataTypeOID != pgtype.NumericOID || sd.Fields[3].DataTypeOID != pgtype.TimestamptzOID {
		t.Errorf("describe: params %v, fields %v", sd.ParamOIDs, sd.Fields)
	}
	// A statement without a result set describes to no fields.
	if sd, err := conn.Prepare(ctx, "ins", "INSERT INTO d (id) VALUES ($1)", nil); err != nil || len(sd.Fields) != 0 {
		t.Errorf("describe INSERT: %v, %v", sd, err)
	}
	// Binary parameter and binary results.
	res := conn.ExecPrepared(ctx, "sel", [][]byte{{0, 0, 0, 1}}, []int16{pgtype.BinaryFormatCode}, []int16{1, 1, 1, 1}).Read()
	if res.Err != nil || len(res.Rows) != 1 {
		t.Fatalf("ExecPrepared: %v, %v", res.Rows, res.Err)
	}
	for i, fd := range res.FieldDescriptions {
		if fd.Format != pgtype.BinaryFormatCode {
			t.Errorf("field %d in format %d", i, fd.Format)
		}
	}
	if string(res.Rows[0][0]) != "\x00\x00\x00\x01" || string(res.Rows[0][2]) != "\x01" || len(res.Rows[0][3]) != 8 {
		t.Errorf("binary row: %q", res.Rows[0])
	}
	// An error in Bind (wrong parameter count) leaves the session usable.
	res = conn.ExecPrepared(ctx, "sel", nil, nil, nil).Read()
	if pgErrCode(res.Err) != "08P01" {
		t.Errorf("bind with no parameters: %v, want protocol_violation", res.Err)
	}
	res = conn.ExecParams(ctx, "SELECT $1::text", [][]byte{[]byte("ok")}, nil, nil, nil).Read()
	if res.Err != nil || string(res.Rows[0][0]) != "ok" {
		t.Errorf("after bind error: %v, %v", res.Rows, res.Err)
	}
}
