package pglite_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pglite "github.com/elliots/go-pglite"
)

// The zero Config initialises a cluster in a temporary directory, which
// Close removes together with the socket directory. This is the one test
// that runs initdb without a template.
func TestZeroConfigUsesTemporaryDirectoryRemovedByClose(t *testing.T) {
	pg, err := pglite.New(pglite.Config{})
	if err != nil {
		t.Fatal(err)
	}
	dir := pg.DataDir()
	if rel, err := filepath.Rel(os.TempDir(), dir); err != nil || strings.HasPrefix(rel, "..") {
		t.Errorf("DataDir %q is not under the temporary directory %q", dir, os.TempDir())
	}
	if _, err := os.Stat(filepath.Join(dir, "pglite", "base", "PG_VERSION")); err != nil {
		t.Errorf("no cluster in DataDir: %v", err)
	}
	socketDir := strings.TrimPrefix(strings.Fields(pg.ConnString())[0], "host=")
	if _, err := os.Stat(filepath.Join(socketDir, ".s.PGSQL.5432")); err != nil {
		t.Errorf("no socket at %s: %v", socketDir, err)
	}

	var version string
	if err := pg.DB().QueryRow("SELECT version()").Scan(&version); err != nil || !strings.HasPrefix(version, "PostgreSQL 17.5 ") {
		t.Errorf("version() = %q, %v", version, err)
	}
	if err := pg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for _, d := range []string{dir, socketDir} {
		if _, err := os.Stat(d); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s after Close: %v, want removed", d, err)
		}
	}
}
