package pglite_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	pglite "github.com/elliots/go-pglite"
	"github.com/jackc/pgx/v5"
)

func TestTemplateCloneStartsWithSetupData(t *testing.T) {
	ctx := t.Context()
	tmpl := filepath.Join(t.TempDir(), "tmpl")
	start := time.Now()
	err := pglite.CreateTemplate(ctx, tmpl, pglite.Config{}, func(ctx context.Context, pg *pglite.PGlite) error {
		conn, err := pgx.Connect(ctx, pg.ConnString())
		if err != nil {
			return err
		}
		defer conn.Close(ctx)
		_, err = conn.Exec(ctx, "CREATE TABLE seeded (id int); INSERT INTO seeded VALUES (1), (2)")
		return err
	})
	if err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	t.Logf("fresh cluster with setup: %v", time.Since(start).Round(time.Millisecond))

	// Only the cluster is kept.
	entries, err := os.ReadDir(tmpl)
	if err != nil || len(entries) != 1 || entries[0].Name() != "pglite" {
		t.Errorf("template holds %v, %v", entries, err)
	}

	start = time.Now()
	a, err := pglite.New(pglite.Config{DataDir: t.TempDir(), Template: tmpl})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	t.Logf("clone start: %v", time.Since(start).Round(time.Millisecond))
	defer a.Close()
	b, err := pglite.New(pglite.Config{DataDir: t.TempDir(), Template: tmpl})
	if err != nil {
		t.Fatalf("second clone: %v", err)
	}
	defer b.Close()

	ca, cb := connect(t, a), connect(t, b)
	exec(t, ca, "INSERT INTO seeded VALUES (3)")
	var na, nb int
	if err := ca.QueryRow(ctx, "SELECT count(*) FROM seeded").Scan(&na); err != nil {
		t.Fatal(err)
	}
	if err := cb.QueryRow(ctx, "SELECT count(*) FROM seeded").Scan(&nb); err != nil {
		t.Fatal(err)
	}
	if na != 3 || nb != 2 {
		t.Errorf("clones are not independent: a has %d rows, b has %d", na, nb)
	}

	// A template cannot be copied over an existing cluster, and a template
	// directory is never overwritten.
	if _, err := pglite.New(pglite.Config{DataDir: a.DataDir(), Template: tmpl}); err == nil {
		t.Error("template copied over an existing cluster")
	}
	err = pglite.CreateTemplate(ctx, tmpl, pglite.Config{}, func(context.Context, *pglite.PGlite) error { return nil })
	if err == nil {
		t.Error("CreateTemplate overwrote an existing template")
	}
	if err := pglite.CreateTemplate(ctx, filepath.Join(t.TempDir(), "x"), pglite.Config{DataDir: "somewhere"}, nil); err == nil {
		t.Error("CreateTemplate accepted a DataDir")
	}
}

func TestCreateTemplateLeavesNothingOnSetupFailure(t *testing.T) {
	tmpl := filepath.Join(t.TempDir(), "tmpl")
	wantErr := fmt.Errorf("setup refused")
	err := pglite.CreateTemplate(t.Context(), tmpl, pglite.Config{Template: ""}, func(context.Context, *pglite.PGlite) error { return wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("CreateTemplate: %v, want the setup error", err)
	}
	if _, err := os.Stat(tmpl); !os.IsNotExist(err) {
		t.Errorf("template directory exists after failure: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(tmpl)); len(entries) != 0 {
		t.Errorf("leftovers: %v", entries)
	}
}

func TestParallelInstancesFromOneTemplate(t *testing.T) {
	const workers = 4
	ctx := t.Context()
	var wg sync.WaitGroup
	starts := make([]time.Duration, workers)
	errs := make([]error, workers)
	for i := range workers {
		wg.Go(func() {
			errs[i] = func() error {
				start := time.Now()
				pg, err := pglite.New(pglite.Config{DataDir: t.TempDir(), Template: pglite.TestTemplate})
				if err != nil {
					return err
				}
				starts[i] = time.Since(start)
				defer pg.Close()
				conn, err := pgx.Connect(ctx, pg.ConnString())
				if err != nil {
					return err
				}
				defer conn.Close(ctx)
				if _, err := conn.Exec(ctx, "CREATE TABLE w (id int); INSERT INTO w SELECT generate_series(1, 1000)"); err != nil {
					return err
				}
				var n int
				if err := conn.QueryRow(ctx, "SELECT count(*) FROM w WHERE id > $1", i*100).Scan(&n); err != nil {
					return err
				}
				if n != 1000-i*100 {
					return fmt.Errorf("worker %d counted %d", i, n)
				}
				return nil
			}()
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("worker %d: %v", i, err)
		}
	}
	t.Logf("%d clones started in parallel: %v", workers, starts)
}

func TestExistingDataDirIsReopened(t *testing.T) {
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
	if _, err := conn.Exec(ctx, "CREATE TABLE kept (id int); INSERT INTO kept VALUES (7)"); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)
	if err := pg.Close(); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	pg, err = pglite.New(pglite.Config{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("restart of an existing cluster: %v", time.Since(start).Round(time.Millisecond))
	defer pg.Close()
	var n int
	if err := connect(t, pg).QueryRow(ctx, "SELECT id FROM kept").Scan(&n); err != nil || n != 7 {
		t.Errorf("after restart: %d, %v", n, err)
	}
}
