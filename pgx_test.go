package pglite_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"

	pglite "github.com/elliots/go-pglite"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// newTestPG starts an instance in a temporary directory. Instances start
// from a shared template (see TestMain), so a test gets a fresh cluster in
// well under a second.
func newTestPG(t *testing.T) *pglite.PGlite {
	t.Helper()
	pg, err := pglite.New(pglite.Config{DataDir: t.TempDir(), Template: pglite.TestTemplate})
	if err != nil {
		t.Fatalf("pglite.New: %v", err)
	}
	t.Cleanup(func() {
		if err := pg.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return pg
}

// connect opens a pgx connection with the default (extended) protocol.
func connect(t *testing.T, pg *pglite.PGlite, extra ...string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), strings.Join(append([]string{pg.ConnString()}, extra...), " "))
	if err != nil {
		t.Fatalf("pgx.Connect: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

func exec(t *testing.T, conn *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(t.Context(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// newUUID returns a random version 4 UUID. pgx encodes and decodes uuid
// values as [16]byte.
func newUUID(t *testing.T) [16]byte {
	t.Helper()
	var u [16]byte
	if _, err := rand.Read(u[:]); err != nil {
		t.Fatal(err)
	}
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	return u
}

func TestPgxNamedAndUnnamedPreparedStatements(t *testing.T) {
	ctx := t.Context()
	conn := connect(t, newTestPG(t))

	// Named: Parse + Describe + Sync, then Bind/Execute by name.
	sd, err := conn.Prepare(ctx, "add", "SELECT $1::int + $2::int")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(sd.ParamOIDs) != 2 || sd.ParamOIDs[0] != pgtype.Int4OID {
		t.Errorf("param OIDs = %v", sd.ParamOIDs)
	}
	var sum int
	if err := conn.QueryRow(ctx, "add", 40, 2).Scan(&sum); err != nil || sum != 42 {
		t.Fatalf("named statement: %d, %v", sum, err)
	}
	var n int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM pg_prepared_statements WHERE name = 'add'").Scan(&n); err != nil || n != 1 {
		t.Errorf("pg_prepared_statements has %d 'add', err %v", n, err)
	}
	if err := conn.Deallocate(ctx, "add"); err != nil {
		t.Fatalf("Deallocate: %v", err)
	}

	// Unnamed statement, described and executed in one round trip.
	modes := []pgx.QueryExecMode{pgx.QueryExecModeCacheStatement, pgx.QueryExecModeCacheDescribe, pgx.QueryExecModeDescribeExec, pgx.QueryExecModeExec}
	for _, mode := range modes {
		var s string
		if err := conn.QueryRow(ctx, "SELECT $1::text || $2::text", mode, "a", "b").Scan(&s); err != nil || s != "ab" {
			t.Errorf("mode %v: %q, %v", mode, s, err)
		}
	}
}

func TestPgxParameterTypesRoundTrip(t *testing.T) {
	ctx := t.Context()
	conn := connect(t, newTestPG(t))
	exec(t, conn, `CREATE TABLE types (
		id uuid PRIMARY KEY, t text, i2 int2, i4 int4, i8 int8, n numeric, b bool,
		ts timestamptz, d date, j jsonb, by bytea, ta text[], ia int[], ua uuid[], tn text)`)

	id := newUUID(t)
	ts := time.Date(2024, 6, 15, 12, 34, 56, 789000000, time.UTC)
	d := time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC)
	num := pgtype.Numeric{Int: big.NewInt(1234567), Exp: -3, Valid: true}
	j := map[string]any{"name": "alice", "tags": []any{"a", "b"}, "n": float64(3)}
	by := []byte{0, 1, 2, 255, 'x'}
	ta := []string{"x", "", "it's"}
	ia := []int32{1, -2, 3}
	ua := [][16]byte{newUUID(t), newUUID(t)}

	exec(t, conn, `INSERT INTO types VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		id, "hello", int16(7), int32(-70000), int64(1)<<40, num, true, ts, d, j, by, ta, ia, ua, nil)

	var (
		gotID  [16]byte
		gotT   string
		gotI2  int16
		gotI4  int32
		gotI8  int64
		gotN   pgtype.Numeric
		gotB   bool
		gotTS  time.Time
		gotD   time.Time
		gotJ   map[string]any
		gotBy  []byte
		gotTA  []string
		gotIA  []int32
		gotUA  [][16]byte
		gotNil *string
	)
	err := conn.QueryRow(ctx, "SELECT * FROM types WHERE id = $1", id).Scan(
		&gotID, &gotT, &gotI2, &gotI4, &gotI8, &gotN, &gotB, &gotTS, &gotD, &gotJ, &gotBy, &gotTA, &gotIA, &gotUA, &gotNil)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if gotID != id || gotT != "hello" || gotI2 != 7 || gotI4 != -70000 || gotI8 != 1<<40 || !gotB {
		t.Errorf("scalars: %v %q %d %d %d %v", gotID, gotT, gotI2, gotI4, gotI8, gotB)
	}
	if f, _ := gotN.Float64Value(); f.Float64 != 1234.567 {
		t.Errorf("numeric = %v", f)
	}
	if !gotTS.Equal(ts) || !gotD.Equal(d) {
		t.Errorf("timestamptz %v (want %v), date %v (want %v)", gotTS, ts, gotD, d)
	}
	wantJ, _ := json.Marshal(j)
	gotJSON, _ := json.Marshal(gotJ)
	if string(gotJSON) != string(wantJ) {
		t.Errorf("jsonb = %s, want %s", gotJSON, wantJ)
	}
	if !bytes.Equal(gotBy, by) {
		t.Errorf("bytea = %v", gotBy)
	}
	if strings.Join(gotTA, "|") != strings.Join(ta, "|") || len(gotIA) != 3 || gotIA[1] != -2 || len(gotUA) != 2 || gotUA[1] != ua[1] {
		t.Errorf("arrays = %v %v %v", gotTA, gotIA, gotUA)
	}
	if gotNil != nil {
		t.Errorf("NULL text = %q", *gotNil)
	}

	// Binary results for the types pgx decodes in binary format.
	rows, err := conn.Query(ctx, "SELECT id, i8, ts, j, by, ia FROM types", pgx.QueryResultFormats{pgx.BinaryFormatCode, pgx.BinaryFormatCode, pgx.BinaryFormatCode, pgx.BinaryFormatCode, pgx.BinaryFormatCode, pgx.BinaryFormatCode})
	if err != nil {
		t.Fatalf("binary query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := rows.Scan(&gotID, &gotI8, &gotTS, &gotJ, &gotBy, &gotIA); err != nil {
			t.Fatalf("binary scan: %v", err)
		}
	}
	if rows.Err() != nil || gotID != id || gotI8 != 1<<40 {
		t.Errorf("binary row: %v %v %v", rows.Err(), gotID, gotI8)
	}
	for i, fd := range rows.FieldDescriptions() {
		if fd.Format != pgx.BinaryFormatCode {
			t.Errorf("column %d (%s) came back in format %d", i, fd.Name, fd.Format)
		}
	}
}

func TestPgxBatchErrorInTheMiddle(t *testing.T) {
	ctx := t.Context()
	pg := newTestPG(t)
	conn := connect(t, pg)
	exec(t, conn, "CREATE TABLE b (id int PRIMARY KEY)")

	// pgx sends the whole batch and one Sync: the error in the second
	// statement makes PostgreSQL skip the third.
	batch := &pgx.Batch{}
	batch.Queue("INSERT INTO b VALUES ($1)", 1)
	batch.Queue("INSERT INTO b VALUES ($1)", 1)
	batch.Queue("INSERT INTO b VALUES ($1)", 2)
	br := conn.SendBatch(ctx, batch)
	if _, err := br.Exec(); err != nil {
		t.Errorf("first insert: %v", err)
	}
	if _, err := br.Exec(); pgErrCode(err) != "23505" {
		t.Errorf("second insert: %v, want unique_violation", err)
	}
	if _, err := br.Exec(); err == nil {
		t.Error("third insert ran after the error")
	}
	if err := br.Close(); err == nil {
		t.Error("Close: want the batch's error")
	}
	var n int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM b").Scan(&n); err != nil || n != 0 {
		t.Errorf("rows after failed batch = %d, %v (the implicit transaction rolls back)", n, err)
	}
	if err := pg.Err(); err != nil {
		t.Errorf("instance failed: %v", err)
	}
}

// Many errors in one pipeline: the client sends every unit before reading
// a reply, so they all reach the server at once. The module aborts after a
// handful of errors in one call; the server passes one Sync-delimited unit
// per call.
func TestPgxPipelineWithManyErrorsSurvives(t *testing.T) {
	ctx := t.Context()
	pg := newTestPG(t)
	conn := connect(t, pg)

	p := conn.PgConn().StartPipeline(ctx)
	const units = 20
	for range units {
		p.SendQueryParams("SELECT 1/0", nil, nil, nil, nil)
		p.SendPipelineSync()
	}
	if err := p.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	errs := 0
	for range units {
		if _, err := p.GetResults(); err != nil {
			// The error for the unit comes with the first GetResults.
			if pgErrCode(err) != "22012" {
				t.Fatalf("unit error: %v", err)
			}
			errs++
		}
		if r, err := p.GetResults(); err != nil {
			t.Fatalf("unit end: %v", err)
		} else if _, ok := r.(*pgconn.PipelineSync); !ok {
			t.Fatalf("unit end: got %T, want PipelineSync", r)
		}
	}
	if err := p.Close(); err != nil {
		t.Fatalf("pipeline Close: %v", err)
	}
	if errs != units {
		t.Errorf("got %d errors, want %d", errs, units)
	}
	var n int
	if err := conn.QueryRow(ctx, "SELECT 5").Scan(&n); err != nil || n != 5 {
		t.Errorf("after pipeline: %d, %v", n, err)
	}
	if err := pg.Err(); err != nil {
		t.Errorf("instance failed: %v", err)
	}
}

func TestPgxStatementCacheAfterDDL(t *testing.T) {
	ctx := t.Context()
	conn := connect(t, newTestPG(t))
	exec(t, conn, "CREATE TABLE c (id int)")
	exec(t, conn, "INSERT INTO c VALUES (1)")

	// The first query caches a prepared statement for the SQL.
	rows, err := conn.Query(ctx, "SELECT * FROM c WHERE id = $1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pgx.CollectRows(rows, pgx.RowToMap); err != nil {
		t.Fatal(err)
	}
	exec(t, conn, "ALTER TABLE c ADD COLUMN name text DEFAULT 'x'")

	// As with a PostgreSQL server, the cached statement's result type is
	// stale: PostgreSQL reports it and pgx drops the cache entry, so the
	// query after that succeeds.
	rows, err = conn.Query(ctx, "SELECT * FROM c WHERE id = $1", 1)
	if err == nil {
		_, err = pgx.CollectRows(rows, pgx.RowToMap)
	}
	if pgErrCode(err) != "0A000" {
		t.Fatalf("after DDL: %v, want 0A000 cached plan must not change result type", err)
	}
	rows, err = conn.Query(ctx, "SELECT * FROM c WHERE id = $1", 1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowToMap)
	if err != nil || len(got) != 1 || got[0]["name"] != "x" {
		t.Errorf("after invalidation: %v, %v", got, err)
	}

	// Named prepared statements see the same error, and work again once
	// re-prepared.
	if _, err := conn.Prepare(ctx, "allc", "SELECT * FROM c"); err != nil {
		t.Fatal(err)
	}
	exec(t, conn, "ALTER TABLE c ADD COLUMN more int")
	rows, err = conn.Query(ctx, "allc")
	if err == nil {
		_, err = pgx.CollectRows(rows, pgx.RowToMap)
	}
	if pgErrCode(err) != "0A000" {
		t.Fatalf("named after DDL: %v, want 0A000", err)
	}
	if err := conn.Deallocate(ctx, "allc"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Prepare(ctx, "allc", "SELECT * FROM c"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM (SELECT * FROM c) s").Scan(&n); err != nil || n != 1 {
		t.Errorf("count = %d, %v", n, err)
	}
}

func TestPgxCopyFromAndCopyTo(t *testing.T) {
	ctx := t.Context()
	conn := connect(t, newTestPG(t))
	exec(t, conn, "CREATE TABLE cp (id int, name text)")

	rows := make([][]any, 1000)
	for i := range rows {
		rows[i] = []any{int32(i), strings.Repeat("n", i%50)}
	}
	n, err := conn.CopyFrom(ctx, pgx.Identifier{"cp"}, []string{"id", "name"}, pgx.CopyFromRows(rows))
	if err != nil || n != 1000 {
		t.Fatalf("CopyFrom: %d, %v", n, err)
	}

	var out bytes.Buffer
	tag, err := conn.PgConn().CopyTo(ctx, &out, "COPY cp TO STDOUT")
	if err != nil || tag.RowsAffected() != 1000 {
		t.Fatalf("CopyTo: %v, %v", tag, err)
	}
	if lines := strings.Count(out.String(), "\n"); lines != 1000 {
		t.Errorf("COPY TO wrote %d lines", lines)
	}

	// A failing COPY: the CopyData after the error is ignored and the
	// session goes on.
	_, err = conn.CopyFrom(ctx, pgx.Identifier{"missing"}, []string{"id"}, pgx.CopyFromRows([][]any{{int32(1)}}))
	if pgErrCode(err) != "42P01" {
		t.Errorf("COPY into missing table: %v, want undefined_table", err)
	}
	var count int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM cp").Scan(&count); err != nil || count != 1000 {
		t.Errorf("count = %d, %v", count, err)
	}
}

// Replies larger than the shared buffer go through the output file; input
// larger than half of it goes through the input file.
func TestLargeRepliesAndQueriesCrossTheBridge(t *testing.T) {
	ctx := t.Context()
	conn := connect(t, newTestPG(t))

	var s string
	if err := conn.QueryRow(ctx, "SELECT repeat('x', 20000000)").Scan(&s); err != nil || len(s) != 20000000 {
		t.Fatalf("20MB reply: len %d, %v", len(s), err)
	}
	big := strings.Repeat("y", 7000000)
	var n int
	if err := conn.QueryRow(ctx, "SELECT length('"+big+"')").Scan(&n); err != nil || n != len(big) {
		t.Fatalf("7MB query: %d, %v", n, err)
	}
	if err := conn.QueryRow(ctx, "SELECT length($1::text)", big).Scan(&n); err != nil || n != len(big) {
		t.Fatalf("7MB parameter: %d, %v", n, err)
	}
	if err := conn.QueryRow(ctx, "SELECT 1").Scan(&n); err != nil || n != 1 {
		t.Fatalf("after large messages: %d, %v", n, err)
	}
}

func TestNoticesReachTheClient(t *testing.T) {
	ctx := t.Context()
	pg := newTestPG(t)
	var notices []string
	cfg, err := pgx.ParseConfig(pg.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	cfg.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) { notices = append(notices, n.Message) }
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	exec(t, conn, "DO $$ BEGIN RAISE NOTICE 'hello from plpgsql'; END $$")
	if len(notices) != 1 || notices[0] != "hello from plpgsql" {
		t.Errorf("notices = %q", notices)
	}
}
