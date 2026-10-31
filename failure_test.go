package pglite

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// A trap or a FATAL leaves the backend in an unknown state. Neither can be
// provoked from a client of this build (signals are not delivered, so
// pg_terminate_backend does nothing), so the failure is injected the way
// exec records it, and the instance's reaction is checked.
func TestFailedBackendRefusesConnectionsAndReportsWhy(t *testing.T) {
	ctx := t.Context()
	pg, err := New(Config{DataDir: t.TempDir(), Template: TestTemplate})
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	conn, err := pgx.Connect(ctx, pg.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	injected := errors.New("interactive_one trapped: injected")
	pg.backend.fail(injected)
	if err := pg.Err(); !errors.Is(err, injected) || !strings.Contains(err.Error(), "Config.StderrFile") {
		t.Fatalf("Err() = %v", err)
	}

	// The open session's next statement fails with the reason, and so does
	// every new connection.
	_, err = conn.Exec(ctx, "SELECT 1")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != codeInternalError || !strings.Contains(pgErr.Message, "injected") {
		t.Errorf("statement after failure: %v", err)
	}
	_, err = pgx.Connect(ctx, pg.ConnString())
	if !errors.As(err, &pgErr) || pgErr.Code != codeInternalError || !strings.Contains(pgErr.Message, "injected") {
		t.Errorf("connection after failure: %v", err)
	}
	// Close does not touch the failed backend beyond releasing it.
	if err := pg.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// Ordinary errors are PostgreSQL's business; a FATAL or PANIC in a reply
// means the backend would have exited, so exec marks it failed.
func TestFatalInReplyMarksBackendFailed(t *testing.T) {
	pg, err := New(Config{DataDir: t.TempDir(), Template: TestTemplate})
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	b := pg.backend
	if _, err := b.query("SELECT 1/0"); err == nil {
		t.Fatal("division by zero: want error")
	}
	if err := b.err(); err != nil {
		t.Fatalf("backend failed on an ordinary error: %v", err)
	}

	ordinary := append(errorResponse("ERROR", "22012", "division by zero"), 'Z', 0, 0, 0, 5, 'I')
	if e, err := fatalIn(ordinary); e != nil || err != nil {
		t.Errorf("ordinary error: %v, %v", e, err)
	}
	fatal := append(append([]byte(nil), ordinary...), errorResponse("FATAL", codeAdminShutdown, "terminating connection due to administrator command")...)
	if e, err := fatalIn(fatal); err != nil || e == nil || e.Code != codeAdminShutdown {
		t.Errorf("fatal reply: %v, %v", e, err)
	}
	if _, err := fatalIn(fatal[:len(fatal)-3]); err == nil {
		t.Error("truncated reply: want error")
	}
}
