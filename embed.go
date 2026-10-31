package pglite

import _ "embed"

// pglite-wasi-17.5.tar.gz is built by build/build.sh; its provenance is in
// pglite-wasi-17.5.tar.gz.txt. pglite-wasi-17.tar.gz is the previous binary
// (pglite-oxide's build without working setjmp/longjmp), kept for reference.
//
//go:embed pglite-wasi-17.5.tar.gz
var embeddedWASI []byte

func init() {
	WASIBinary = embeddedWASI
}
