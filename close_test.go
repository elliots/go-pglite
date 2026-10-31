package pglite_test

import (
	"testing"

	pglite "github.com/elliots/go-pglite"
	"github.com/jackc/pgx/v5"
)

// Close while a connection is open, even inside an aborted transaction,
// rolls the transaction back and shuts the cluster down cleanly; the
// uncommitted work is gone when the cluster is opened again.
func TestCloseWithOpenTransactionShutsDownCleanly(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	pg, err := pglite.New(pglite.Config{DataDir: dir, Template: pglite.TestTemplate})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, pg.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{"CREATE TABLE kept (id int)", "BEGIN", "INSERT INTO kept VALUES (1)"} {
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	if _, err := conn.Exec(ctx, "SELECT 1/0"); pgErrCode(err) != "22012" {
		t.Fatalf("SELECT 1/0: %v", err)
	}
	if err := pg.Close(); err != nil {
		t.Fatalf("Close with an aborted transaction open: %v", err)
	}
	if _, err := conn.Exec(ctx, "SELECT 1"); err == nil {
		t.Error("connection survived Close")
	}

	pg, err = pglite.New(pglite.Config{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	var n int
	if err := connect(t, pg).QueryRow(ctx, "SELECT count(*) FROM kept").Scan(&n); err != nil || n != 0 {
		t.Errorf("rows after reopening: %d, %v", n, err)
	}
}
