package pglite

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestTemplate is a template cluster the tests clone, so that each gets a
// fresh cluster in a fraction of a second instead of running initdb.
var TestTemplate string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "go-pglite-template-*")
	if err != nil {
		panic(err)
	}
	TestTemplate = filepath.Join(dir, "template")
	start := time.Now()
	err = CreateTemplate(context.Background(), TestTemplate, Config{}, func(context.Context, *PGlite) error { return nil })
	if err != nil {
		panic(err)
	}
	fmt.Fprintf(os.Stderr, "test template created in %v\n", time.Since(start).Round(time.Millisecond))
	code := m.Run()
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
