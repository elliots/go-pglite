package pglite

import _ "embed"

// pglite-wasi-17.5.tar.gz is PostgreSQL 17.5 compiled to WASI, built by
// build/build.sh; its provenance is in pglite-wasi-17.5.tar.gz.txt.
//
//go:embed pglite-wasi-17.5.tar.gz
var embeddedArchive []byte
