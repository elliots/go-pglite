package pglite_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	pglite "github.com/elliots/go-pglite"
	"github.com/jackc/pgx/v5"
)

// TestLatencyReport measures what a client sees: a trivial query and a
// 1000-statement workload over pgx's extended protocol. It logs the numbers
// and fails only on gross regressions: a bridge that polls or sleeps adds
// milliseconds to every round trip, while the real cost is about 100µs
// (several times that under the race detector).
func TestLatencyReport(t *testing.T) {
	ctx := t.Context()
	conn := connect(t, newTestPG(t))
	exec(t, conn, "CREATE TABLE lat (id int PRIMARY KEY, name text, created timestamptz DEFAULT now())")

	const samples = 200
	var n int
	durations := make([]time.Duration, 0, samples)
	for range samples {
		start := time.Now()
		if err := conn.QueryRow(ctx, "SELECT $1::int", 1).Scan(&n); err != nil {
			t.Fatal(err)
		}
		durations = append(durations, time.Since(start))
	}
	slices.Sort(durations)
	median, p99 := durations[samples/2], durations[samples*99/100]
	t.Logf("trivial query: median %v, p99 %v", median, p99)
	if median > 10*time.Millisecond {
		t.Errorf("trivial query median %v, want well under 10ms", median)
	}

	start := time.Now()
	for i := range 1000 {
		if _, err := conn.Exec(ctx, "INSERT INTO lat (id, name) VALUES ($1, $2)", i, fmt.Sprint("name", i)); err != nil {
			t.Fatal(err)
		}
	}
	autocommit := time.Since(start)

	start = time.Now()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 1000 {
		if _, err := tx.Exec(ctx, "UPDATE lat SET name = $2 WHERE id = $1", i, "renamed"); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	inTx := time.Since(start)

	start = time.Now()
	batch := &pgx.Batch{}
	for i := range 1000 {
		batch.Queue("SELECT name FROM lat WHERE id = $1", i)
	}
	if err := conn.SendBatch(ctx, batch).Close(); err != nil {
		t.Fatal(err)
	}
	batched := time.Since(start)

	// An autocommit statement waits for its WAL to reach disk; with
	// synchronous_commit=off it does not.
	exec(t, conn, "SET synchronous_commit = off")
	start = time.Now()
	for i := range 1000 {
		if _, err := conn.Exec(ctx, "INSERT INTO lat (id, name) VALUES ($1, $2)", 1000+i, "async"); err != nil {
			t.Fatal(err)
		}
	}
	async := time.Since(start)

	t.Logf("1000 statements: autocommit inserts %v (synchronous_commit=off: %v), updates in one transaction %v, batched selects %v", autocommit, async, inTx, batched)
	if async > 5*time.Second {
		t.Errorf("1000 inserts with synchronous_commit=off took %v", async)
	}
}

func TestConfigSettingsApplyToEverySession(t *testing.T) {
	ctx := t.Context()
	pg, err := pglite.New(pglite.Config{DataDir: t.TempDir(), Template: pglite.TestTemplate, Settings: map[string]string{"synchronous_commit": "off", "application_name": "cfg"}})
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	for range 2 {
		conn := connect(t, pg)
		var sc, app string
		if err := conn.QueryRow(ctx, "SELECT current_setting('synchronous_commit'), current_setting('application_name')").Scan(&sc, &app); err != nil {
			t.Fatal(err)
		}
		if sc != "off" || app != "cfg" {
			t.Errorf("synchronous_commit %q, application_name %q", sc, app)
		}
		conn.Close(ctx)
	}
	// The client's own startup parameter wins.
	conn := connect(t, pg, "application_name=client")
	var app string
	if err := conn.QueryRow(ctx, "SHOW application_name").Scan(&app); err != nil || app != "client" {
		t.Errorf("application_name %q, %v", app, err)
	}
	conn.Close(ctx)

	bad, err := pglite.New(pglite.Config{DataDir: t.TempDir(), Template: pglite.TestTemplate, Settings: map[string]string{"no_such": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Close()
	if _, err := pgx.Connect(ctx, bad.ConnString()); pgErrCode(err) != "42704" {
		t.Errorf("connection with an invalid setting: %v, want undefined_object", err)
	}
}

func BenchmarkTrivialQuery(b *testing.B) {
	pg := newBenchPG(b)
	conn, err := pgx.Connect(b.Context(), pg.ConnString())
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close(b.Context())
	var n int
	for b.Loop() {
		if err := conn.QueryRow(b.Context(), "SELECT $1::int", 1).Scan(&n); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkInsertAutocommit(b *testing.B) {
	pg := newBenchPG(b)
	conn, err := pgx.Connect(b.Context(), pg.ConnString())
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close(b.Context())
	if _, err := conn.Exec(b.Context(), "CREATE TABLE bench (id serial, name text)"); err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if _, err := conn.Exec(b.Context(), "INSERT INTO bench (name) VALUES ($1)", "x"); err != nil {
			b.Fatal(err)
		}
	}
}
