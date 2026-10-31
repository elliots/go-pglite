module github.com/elliots/go-pglite

go 1.27.0

require (
	github.com/jackc/pgx/v5 v5.11.0
	github.com/wago-org/wago v0.1.0-beta.13
	github.com/wago-org/wasi v0.3.1
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/sys v0.30.0 // indirect
	golang.org/x/text v0.29.0 // indirect
)

// The fork carries the fixes from wago-org/wago#1043 until a wago release
// includes them.
replace github.com/wago-org/wago => github.com/elliots/wago v0.1.0-beta.13.0.20261031190326-525587b6fa86
