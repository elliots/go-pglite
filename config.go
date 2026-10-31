package pglite

import "log/slog"

// User and Database are the only role and database the backend serves: the
// module runs initdb as this user and starts the single-user backend on
// this database.
const (
	User     = "postgres"
	Database = "postgres"
)

// Config configures an instance. The zero value starts a fresh cluster in a
// temporary directory that Close removes.
type Config struct {
	// DataDir is the instance directory, holding the cluster under
	// pglite/base. It is created if missing; a directory without a cluster
	// is initialised. Empty means a temporary directory removed by Close.
	DataDir string

	// Template is a directory made by CreateTemplate whose cluster is
	// copied into DataDir before the start. DataDir must not hold a cluster
	// yet.
	Template string

	// CacheDir holds the extracted installation every instance shares.
	// Empty means DefaultCacheDir.
	CacheDir string

	// Settings are session settings every connection starts with, applied
	// before the client's own startup parameters. A setting PostgreSQL
	// rejects fails the first connection. For example,
	// synchronous_commit=off makes a commit return before its WAL reaches
	// disk, which on macOS (where every fsync is a full flush) takes
	// several milliseconds; a crash can then lose the last commits, but
	// never corrupt the cluster.
	Settings map[string]string

	// StdoutFile and StderrFile receive the module's standard output and
	// PostgreSQL's log. Empty discards them.
	StdoutFile string
	StderrFile string

	// Logger receives the bridge's own messages, such as a crash of the
	// module. nil means slog.Default.
	Logger *slog.Logger
}

func (c *Config) withDefaults() (Config, error) {
	out := *c
	if out.CacheDir == "" {
		dir, err := DefaultCacheDir()
		if err != nil {
			return out, err
		}
		out.CacheDir = dir
	}
	if out.Logger == nil {
		out.Logger = slog.Default()
	}
	return out, nil
}
