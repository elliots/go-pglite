#!/usr/bin/env bash
#
# Builds pglite.wasi inside the toolchain image (build/Dockerfile) and
# writes the archive go-pglite embeds. Run by build/build.sh; not meant to
# be run on the host.
#
# Inputs (bind mounts):
#   /src     PGlite's PostgreSQL source tree, patches already applied
#   /compat  build/compat: headers and stubs for what wasi-libc lacks
#   /out     where the archive and its checksum are written
# Environment:
#   LABEL            archive name without extension, e.g. pglite-wasi-17.5
#   EXTRA_CONFIGURE  extra configure options for this PostgreSQL version
#
# The PGlite port hardcodes /tmp/pglite as its prefix (wasi.h includes
# /tmp/pglite/include/wasm_common.h, and the module expects its files under
# /tmp/pglite at run time), so that path is fixed here too.
set -euo pipefail

PGSRC=${PGSRC:-/src}
COMPAT=${COMPAT:-/compat}
OUT=${OUT:-/out}
BUILD=${BUILD:-/build}
PGROOT=/tmp/pglite
LABEL=${LABEL:?LABEL is required}
EXTRA_CONFIGURE=${EXTRA_CONFIGURE:-}

# Size of the shared I/O zone at the bottom of linear memory: PGlite's wire
# protocol transport reads queries from and writes replies to [0, CMA_MB MB).
CMA_MB=12
# Shadow stack. PostgreSQL's max_stack_depth defaults to 2MB and must stay
# at least STACK_DEPTH_SLOP (512kB) below the real limit; 8MB leaves room.
STACK_SIZE=$((8 * 1024 * 1024))
NCPU=$(nproc)

log() { printf '\n==> %s\n' "$*" >&2; }

rm -rf "$BUILD" "$PGROOT"
mkdir -p "$BUILD/bin" "$PGROOT/include/port" "$OUT"

# ---------------------------------------------------------------------------
# PGlite's port headers at the paths the port hardcodes.
cp "$PGSRC/src/include/port/wasm_common.h" "$PGROOT/include/"
cp "$PGSRC"/wasm-build/sdk_port-wasi/*.c "$PGROOT/include/"
# Debug output off; the role --single runs as.
cat > "$PGROOT/include/port/pg_debug.h" <<'EOF'
#ifndef I_PGDEBUG
#define I_PGDEBUG
#define WASM_USERNAME "postgres"
#define PDEBUG(string)
#define JSDEBUG(string)
#define ADEBUG(string)
#define PGDEBUG 0
#endif
EOF
cp "$PGROOT/include/port/pg_debug.h" "$PGROOT/include/pg_debug.h"

# ---------------------------------------------------------------------------
# Toolchain.
export CC="clang --target=wasm32-wasip1"
export AR=llvm-ar
export RANLIB=llvm-ranlib
# Time zone data is compiled with the host's zic.
export ZIC=/usr/sbin/zic

# setjmp/longjmp through WebAssembly exception handling, in the exnref
# encoding (try_table) that wasmtime implements. The legacy encoding is
# LLVM's default and wasmtime rejects it.
SJLJ="-mllvm -wasm-enable-sjlj -mllvm -wasm-use-legacy-eh=false"
EMULATED="-D_WASI_EMULATED_SIGNAL -D_WASI_EMULATED_PROCESS_CLOCKS -D_WASI_EMULATED_GETPID -D_WASI_EMULATED_MMAN"
EMULATED_LIBS="-lwasi-emulated-signal -lwasi-emulated-process-clocks -lwasi-emulated-getpid -lwasi-emulated-mman"
# PYDK selects the extern (not static) variants in sdk_port.h; SDK_PORT makes
# src/port/pqsignal.c include sdk_port-wasi.c; PG_PREFIX and CMA_MB are read
# by wasm_common.h and the entry point.
PGLITE_DEFS="-DPYDK=1 -DSDK_PORT=1 -DPG_PREFIX=$PGROOT -DCMA_MB=$CMA_MB -I$PGROOT/include"
WARNINGS="-Wno-declaration-after-statement -Wno-macro-redefined -Wno-unused-function -Wno-missing-prototypes -Wno-incompatible-pointer-types -Wno-unused-but-set-variable -Wno-unknown-pragmas -Wno-unused-command-line-argument -Wno-unused-variable"

export CFLAGS="-O2 $SJLJ $EMULATED $PGLITE_DEFS -D_GNU_SOURCE -isystem $COMPAT/include -include $COMPAT/include/wasi_compat.h $WARNINGS"

# The compat definitions are linked into everything, test programs included.
log "compiling compat stubs"
$CC $CFLAGS -c "$COMPAT/wasi_compat.c" -o "$BUILD/wasi_compat.o"
export LDFLAGS="$BUILD/wasi_compat.o $EMULATED_LIBS -lsetjmp"

# Makefile.shlib calls this to link shared libraries. PL/pgSQL and the
# snowball dictionary are linked into pglite.wasi and dlopen() is resolved
# from a table (wasm-build/sdk_port-wasi/sdk_port-wasi-dlfcn.c); the .so
# files only have to exist for PostgreSQL's file check, so link empty ones.
cat > "$BUILD/bin/wasi-shared" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
out=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out=$2; shift 2 ;;
    *) shift ;;
  esac
done
[ -n "$out" ] || { echo "wasi-shared: no -o" >&2; exit 1; }
echo | clang --target=wasm32-wasip1 -fPIC -x c -c - -o "$out.o"
clang --target=wasm32-wasip1 -shared -fPIC -nostdlib -Wl,--no-entry -o "$out" "$out.o"
rm -f "$out.o"
EOF
chmod +x "$BUILD/bin/wasi-shared"
export PATH="$BUILD/bin:$PATH"

# ---------------------------------------------------------------------------
log "configure"
cat > "$BUILD/config.site" <<'EOF'
ac_cv_exeext=.wasi
ac_cv_search_sem_open=no
EOF
cd "$BUILD"
CONFIG_SITE="$BUILD/config.site" "$PGSRC/configure" \
  --host=wasm32-wasi --build="$(uname -m)-unknown-linux-gnu" \
  --prefix="$PGROOT" --with-template=wasi \
  --without-llvm --without-pam --without-readline --without-icu \
  --without-zlib --without-libxml --with-openssl=no --disable-largefile \
  $EXTRA_CONFIGURE

MAKE="make -j$NCPU PORTNAME=wasi wasi=1"

# Generated headers first, serially: the parallel backend build otherwise
# races ahead of utils/errcodes.h.
log "generate headers"
make PORTNAME=wasi wasi=1 -C src/backend generated-headers

# The top-level Makefile builds these before the backend; the patched
# backend link rule needs them to exist when make reads it.
log "build port and common libraries"
$MAKE -C src/port
$MAKE -C src/common

log "build backend"
$MAKE -C src/backend
log "build snowball and plpgsql"
$MAKE -C src/backend/snowball
$MAKE -C src/pl/plpgsql
log "install"
$MAKE -C src/backend install
# postgres.bki and system_constraints.sql are generated and installed here;
# the rule also installs headers and expects their directory to exist.
mkdir -p "$PGROOT/include/postgresql/server/catalog"
$MAKE -C src/include/catalog install
$MAKE -C src/backend/snowball install
$MAKE -C src/pl/plpgsql install
$MAKE -C src/timezone install

# ---------------------------------------------------------------------------
log "link pglite.wasi"
INCLUDES="-I$BUILD/src/include -I$PGSRC/src/include -I$PGSRC/src/interfaces/libpq -I$PGSRC/src"
$CC $CFLAGS $INCLUDES -Wno-incompatible-pointer-types-discards-qualifiers \
  -c "$PGSRC/pglite-wasm/pg_main.c" -o "$BUILD/pglite.o"
$CC $CFLAGS $INCLUDES \
  -c "$PGSRC/wasm-build/sdk_port-wasi/sdk_port-wasi-dlfcn.c" -o "$BUILD/dlfcn.o"
# libpgcore.a is a relocatable link of the whole backend and already carries
# crt1's _start, hence -nostartfiles.
$CC $CFLAGS -nostartfiles \
  -Wl,--global-base=$((CMA_MB * 1024 * 1024)) -Wl,-z,stack-size=$STACK_SIZE \
  -o "$BUILD/pglite.wasi" \
  "$BUILD/pglite.o" "$BUILD/dlfcn.o" \
  "$BUILD/libpgcore.a" \
  "$BUILD/src/backend/snowball/libdict_snowball.a" \
  "$BUILD/src/pl/plpgsql/src/libplpgsql.a" \
  $LDFLAGS

# ---------------------------------------------------------------------------
log "assemble archive"
STAGE="$OUT/stage"
rm -rf "$STAGE"
mkdir -p "$STAGE/tmp/pglite/bin" "$STAGE/tmp/pglite/lib/postgresql"
cp "$BUILD/pglite.wasi" "$STAGE/tmp/pglite/bin/pglite.wasi"
ln -s pglite.wasi "$STAGE/tmp/pglite/bin/postgres"
ln -s pglite.wasi "$STAGE/tmp/pglite/bin/initdb"
cp -R "$PGROOT/share" "$STAGE/tmp/pglite/share"
cp "$PGROOT"/lib/postgresql/*.so "$STAGE/tmp/pglite/lib/postgresql/"
echo password > "$STAGE/tmp/pglite/password"

# Deterministic archive: sorted entries, fixed timestamps and ownership,
# gzip without a name or time stamp.
tar --sort=name --mtime='1970-01-01 00:00:00Z' --owner=0 --group=0 --numeric-owner \
  -C "$STAGE" -cf - tmp | gzip -n -9 > "$OUT/$LABEL.tar.gz"
rm -rf "$STAGE"
(cd "$OUT" && sha256sum "$LABEL.tar.gz" > "$LABEL.tar.gz.sha256")

{
  echo "wasi-sdk: $(cat /opt/wasi-sdk/VERSION 2>/dev/null | head -1)"
  clang --version | head -1
  echo "CFLAGS: $CFLAGS"
  echo "LDFLAGS: $LDFLAGS"
  echo "link: --global-base=$((CMA_MB * 1024 * 1024)) -z stack-size=$STACK_SIZE"
} > "$OUT/toolchain.txt"

log "done: $OUT/$LABEL.tar.gz"
cat "$OUT/$LABEL.tar.gz.sha256"
