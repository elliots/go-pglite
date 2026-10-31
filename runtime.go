package pglite

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/wago-org/wago"
)

// nativeStackBytes is the native stack wago gives each instance. PostgreSQL
// limits recursion by its shadow stack (max_stack_depth, see
// check_stack_depth); the native frames of the same recursion must fit here,
// or wago traps before PostgreSQL reports the error.
const nativeStackBytes = 8 << 20

// runtime is what every instance in the process shares: the compiled module
// and the extracted installation (binaries, share files and library
// placeholders). There is one per cache directory.
type runtime struct {
	compiled *wago.Compiled
	install  string // directory holding pglite/{bin,share,lib}
}

var runtimes struct {
	sync.Mutex
	byDir map[string]*runtime
}

// loadRuntime returns the runtime for cacheDir, creating it on first use.
func loadRuntime(cacheDir string) (*runtime, error) {
	runtimes.Lock()
	defer runtimes.Unlock()
	if rt, ok := runtimes.byDir[cacheDir]; ok {
		return rt, nil
	}
	rt, err := newRuntime(cacheDir)
	if err != nil {
		return nil, err
	}
	if runtimes.byDir == nil {
		runtimes.byDir = map[string]*runtime{}
	}
	runtimes.byDir[cacheDir] = rt
	return rt, nil
}

func newRuntime(cacheDir string) (*runtime, error) {
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("pglite: cache directory: %w", err)
	}
	sum := sha256.Sum256(embeddedArchive)
	install, err := installArchive(filepath.Join(cacheDir, "pglite-"+hex.EncodeToString(sum[:8])), embeddedArchive)
	if err != nil {
		return nil, err
	}

	wasm, err := os.ReadFile(filepath.Join(install, "pglite", "bin", "pglite.wasi"))
	if err != nil {
		return nil, fmt.Errorf("pglite: %w", err)
	}
	// The module implements setjmp/longjmp with WebAssembly exception
	// handling (exnref encoding). Compiling takes about half a second, so
	// nothing is cached between processes.
	compiled, err := wago.CompileWithConfig(wago.NewRuntimeConfig().WithNativeStackBytes(nativeStackBytes), wasm)
	if err != nil {
		return nil, fmt.Errorf("pglite: compiling module: %w", err)
	}
	return &runtime{compiled: compiled, install: install}, nil
}

// installArchive extracts the embedded archive into dir unless it is already
// there. The extraction goes into a sibling directory that is renamed into
// place, so dir is either absent or complete; a concurrent extraction that
// loses the rename keeps the other's result.
func installArchive(dir string, archive []byte) (string, error) {
	if _, err := os.Stat(filepath.Join(dir, "pglite", "bin", "pglite.wasi")); err == nil {
		return dir, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("pglite: installation: %w", err)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), filepath.Base(dir)+".tmp-")
	if err != nil {
		return "", fmt.Errorf("pglite: installation: %w", err)
	}
	if err := extractTarGz(tmp, archive); err != nil {
		return "", errors.Join(fmt.Errorf("pglite: extracting installation: %w", err), os.RemoveAll(tmp))
	}
	if err := os.Rename(tmp, dir); err != nil {
		if rmErr := os.RemoveAll(tmp); rmErr != nil {
			return "", errors.Join(err, rmErr)
		}
		if _, statErr := os.Stat(filepath.Join(dir, "pglite", "bin", "pglite.wasi")); statErr != nil {
			return "", fmt.Errorf("pglite: installation: %w", err)
		}
	}
	return dir, nil
}

// extractTarGz extracts the archive's regular files, directories and
// symlinks under destDir, dropping the archive's leading "tmp/" (the module
// sees destDir as /tmp).
func extractTarGz(destDir string, data []byte) error {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(hdr.Name, "tmp/")
		if name == "" || name == "tmp" {
			continue
		}
		target := filepath.Join(destDir, name)
		if rel, err := filepath.Rel(destDir, target); err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			return fmt.Errorf("archive entry %q escapes the destination", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fs.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if err := errors.Join(err, f.Close()); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		default:
			return fmt.Errorf("archive entry %q has unsupported type %q", hdr.Name, hdr.Typeflag)
		}
	}
}

// DefaultCacheDir returns the directory New uses for Config.CacheDir when it
// is empty: go-pglite under the user's cache directory.
func DefaultCacheDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("pglite: no cache directory: %w (set Config.CacheDir)", err)
	}
	return filepath.Join(dir, "go-pglite"), nil
}
