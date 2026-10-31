package pglite

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// CreateTemplate makes a template for Config.Template at dir: it starts a
// fresh instance (cfg.DataDir and cfg.Template must be empty), runs setup on
// it, for example to apply migrations, and shuts it down. The instance is
// built in a temporary sibling directory that is renamed to dir only on
// success, so dir is either absent or a complete template; if dir already
// exists the rename fails and nothing is changed.
func CreateTemplate(ctx context.Context, dir string, cfg Config, setup func(ctx context.Context, pg *PGlite) error) error {
	if cfg.DataDir != "" || cfg.Template != "" {
		return errors.New("pglite: CreateTemplate chooses the data directory; leave Config.DataDir and Config.Template empty")
	}
	parent := filepath.Dir(filepath.Clean(dir))
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("pglite: %w", err)
	}
	tmp, err := os.MkdirTemp(parent, filepath.Base(dir)+".tmp-")
	if err != nil {
		return fmt.Errorf("pglite: %w", err)
	}
	cfg.DataDir = tmp
	err = func() error {
		pg, err := New(cfg)
		if err != nil {
			return err
		}
		if err := setup(ctx, pg); err != nil {
			return errors.Join(fmt.Errorf("pglite: template setup: %w", err), pg.Close())
		}
		return pg.Close()
	}()
	if err == nil {
		// Only the cluster is part of a template.
		err = removeAllExcept(tmp, filepath.Join("pglite", "base"))
	}
	if err == nil {
		err = os.Rename(tmp, dir)
	}
	if err != nil {
		return errors.Join(err, os.RemoveAll(tmp))
	}
	return nil
}

// removeAllExcept removes everything in root except the path keep (relative
// to root) and the directories leading to it.
func removeAllExcept(root, keep string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	first, rest, _ := cutPath(keep)
	for _, e := range entries {
		p := filepath.Join(root, e.Name())
		switch {
		case e.Name() != first:
			if err := os.RemoveAll(p); err != nil {
				return err
			}
		case rest != "":
			if err := removeAllExcept(p, rest); err != nil {
				return err
			}
		}
	}
	return nil
}

func cutPath(p string) (first, rest string, ok bool) {
	for i := 0; i < len(p); i++ {
		if os.IsPathSeparator(p[i]) {
			return p[:i], p[i+1:], true
		}
	}
	return p, "", false
}

// cloneTemplate copies the cluster of the template directory into the
// instance directory dir.
func cloneTemplate(template, dir string) error {
	src := filepath.Join(template, "pglite", "base")
	dst := filepath.Join(dir, "pglite", "base")
	if _, err := os.Stat(filepath.Join(src, "PG_VERSION")); err != nil {
		return fmt.Errorf("pglite: template %s has no cluster: %w", template, err)
	}
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("pglite: %s already exists; a template is copied only into a directory without a cluster", dst)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		return fmt.Errorf("pglite: copying template %s: %w", template, err)
	}
	return nil
}
