package pglite_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// The backend cannot resume a COPY FROM STDIN whose data arrives in a later
// call, so the server passes a COPY on only together with its data (the
// simple protocol, where the client streams the data after the query) and
// refuses the forms where it cannot do that, instead of letting the module
// crash.
func TestCopyFromStdinThroughExtendedProtocolIsRefused(t *testing.T) {
	ctx := t.Context()
	pg := newTestPG(t)
	conn, err := pgconn.Connect(ctx, pg.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if err := conn.Exec(ctx, "CREATE TABLE cp (id int)").Close(); err != nil {
		t.Fatal(err)
	}

	res := conn.ExecParams(ctx, "COPY cp FROM STDIN", nil, nil, nil, nil).Read()
	if code := pgErrCode(res.Err); code != "0A000" || !strings.Contains(res.Err.Error(), "simple query protocol") {
		t.Fatalf("extended COPY FROM STDIN: %v, want feature_not_supported", res.Err)
	}
	if conn.TxStatus() != 'I' {
		t.Errorf("transaction status %q after refusal, want idle", conn.TxStatus())
	}
	// Inside a transaction block the refusal leaves the block as it was.
	if err := conn.Exec(ctx, "BEGIN").Close(); err != nil {
		t.Fatal(err)
	}
	res = conn.ExecParams(ctx, "COPY cp FROM STDIN", nil, nil, nil, nil).Read()
	if code := pgErrCode(res.Err); code != "0A000" {
		t.Fatalf("extended COPY FROM STDIN in a transaction: %v", res.Err)
	}
	if conn.TxStatus() != 'T' {
		t.Errorf("transaction status %q, want in transaction", conn.TxStatus())
	}
	if err := conn.Exec(ctx, "ROLLBACK").Close(); err != nil {
		t.Fatal(err)
	}

	// In a pipeline, the refusal discards the rest of the pipeline up to
	// its Sync; the messages before the refused one have reached the
	// backend and stand.
	p := conn.StartPipeline(ctx)
	p.SendQueryParams("INSERT INTO cp VALUES (1)", nil, nil, nil, nil)
	p.SendQueryParams("COPY cp FROM STDIN", nil, nil, nil, nil)
	p.SendQueryParams("INSERT INTO cp VALUES (2)", nil, nil, nil, nil)
	p.SendPipelineSync()
	if err := p.Flush(); err != nil {
		t.Fatal(err)
	}
	r, err := p.GetResults()
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if rr, ok := r.(*pgconn.ResultReader); !ok || rr.Read().CommandTag.String() != "INSERT 0 1" {
		t.Errorf("first insert result: %T %v", r, r)
	}
	if _, err := p.GetResults(); pgErrCode(err) != "0A000" {
		t.Errorf("COPY in pipeline: %v, want feature_not_supported", err)
	}
	if r, err := p.GetResults(); err != nil {
		t.Errorf("pipeline end: %v", err)
	} else if _, ok := r.(*pgconn.PipelineSync); !ok {
		t.Errorf("pipeline end: %T, want PipelineSync (the third statement is discarded)", r)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	res = conn.ExecParams(ctx, "SELECT count(*) FROM cp", nil, nil, nil, nil).Read()
	if res.Err != nil || string(res.Rows[0][0]) != "1" {
		t.Errorf("rows after pipeline: %v, %v", res.Rows, res.Err)
	}
	if err := pg.Err(); err != nil {
		t.Errorf("instance failed: %v", err)
	}
}

func TestCopyFromStdinInMultiStatementQuery(t *testing.T) {
	ctx := t.Context()
	pg := newTestPG(t)
	conn, err := pgconn.Connect(ctx, pg.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if err := conn.Exec(ctx, "CREATE TABLE cp (id int)").Close(); err != nil {
		t.Fatal(err)
	}

	// One COPY among other statements takes its data as usual.
	tag, err := conn.CopyFrom(ctx, strings.NewReader("1\n2\n3\n"), "BEGIN; COPY cp FROM STDIN; COMMIT")
	if err != nil || tag.String() != "COMMIT" {
		t.Fatalf("BEGIN; COPY; COMMIT: %v, %v", tag, err)
	}
	// If the COPY never runs, the data the client streamed is ignored.
	_, err = conn.CopyFrom(ctx, strings.NewReader("4\n"), "SELECT 1/0; COPY cp FROM STDIN")
	if code := pgErrCode(err); code != "22012" {
		t.Fatalf("failing statement before COPY: %v, want division_by_zero", err)
	}
	// Two COPYs would need the second data stream after the first reply.
	_, err = conn.Exec(ctx, "COPY cp FROM STDIN; COPY cp FROM STDIN").ReadAll()
	if code := pgErrCode(err); code != "0A000" {
		t.Fatalf("two COPY FROM STDIN: %v, want feature_not_supported", err)
	}

	res := conn.ExecParams(ctx, "SELECT count(*) FROM cp", nil, nil, nil, nil).Read()
	if res.Err != nil || string(res.Rows[0][0]) != "3" {
		t.Errorf("rows: %v, %v", res.Rows, res.Err)
	}
	if err := pg.Err(); err != nil {
		t.Errorf("instance failed: %v", err)
	}
}

// A client that vanishes in the middle of its COPY data ends the session
// without the COPY reaching the backend.
func TestCopyFromStdinAbandonedByClient(t *testing.T) {
	ctx := t.Context()
	pg := newTestPG(t)
	conn, err := pgconn.Connect(ctx, pg.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Exec(ctx, "CREATE TABLE cp (id int)").Close(); err != nil {
		t.Fatal(err)
	}
	// pgconn sends CopyFail with the reader's error, which the backend
	// reports back as query_canceled; the session goes on.
	_, err = conn.CopyFrom(ctx, &failingReader{}, "COPY cp FROM STDIN")
	if code := pgErrCode(err); code != "57014" || !strings.Contains(err.Error(), "source failed") {
		t.Fatalf("CopyFrom with a failing reader: %v, want the reader's error from the backend", err)
	}
	res := conn.ExecParams(ctx, "SELECT count(*) FROM cp", nil, nil, nil, nil).Read()
	if res.Err != nil || string(res.Rows[0][0]) != "0" {
		t.Errorf("after CopyFail: %v, %v", res.Rows, res.Err)
	}
	// Cutting the connection during the data leaves the next session clean.
	p, err := pgconn.Connect(ctx, pg.ConnString())
	if err == nil {
		t.Fatalf("second connection was accepted: %v", err)
	}
	conn.Close(ctx)
	p, err = pgconn.Connect(ctx, pg.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Conn().Write(append(queryBytes("COPY cp FROM STDIN"), 'd', 0, 0, 0, 6, '5', '\n')); err != nil {
		t.Fatal(err)
	}
	if err := p.Conn().Close(); err != nil {
		t.Fatal(err)
	}
	next := connect(t, pg)
	var n int
	if err := next.QueryRow(ctx, "SELECT count(*) FROM cp").Scan(&n); err != nil || n != 0 {
		t.Errorf("after abandoned COPY: %d, %v", n, err)
	}
	if err := pg.Err(); err != nil {
		t.Errorf("instance failed: %v", err)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("source failed") }

// queryBytes returns a simple-protocol Query message.
func queryBytes(sql string) []byte {
	n := uint32(4 + len(sql) + 1)
	return append(append([]byte{'Q', byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}, sql...), 0)
}
