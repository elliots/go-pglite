#!/usr/bin/env bash
#
# Builds the PGlite WASI binary archive that go-pglite embeds, reproducibly:
#
#   build/build.sh            # writes pglite-wasi-17.5.tar.gz and its .txt
#
# Needs Docker and git. The PostgreSQL source is fetched at a pinned commit
# into build/work/, the patches in build/patches/ are applied, and
# pglite-build.sh runs inside the image from build/Dockerfile. The archive
# is byte-for-byte reproducible for a given toolchain image.
set -euo pipefail

cd "$(dirname "$0")"
HERE=$PWD
REPO_ROOT=$(cd .. && pwd)

PG_REPO=https://github.com/electric-sql/postgres-pglite.git
PG_BRANCH=REL_17_5_WASM-pglite
PG_COMMIT=2194acf356e9b7bf7cff29c4ed4bd6b82b93895d
PG_VERSION=17.5
LABEL=pglite-wasi-17.5
IMAGE=go-pglite-builder:wasi-sdk-34
WORK=${WORK:-$HERE/work}
SRC=$WORK/postgres-pglite
OUT=$WORK/out

log() { printf '\n==> %s\n' "$*" >&2; }

log "toolchain image $IMAGE"
docker build -q -t "$IMAGE" "$HERE" >/dev/null
IMAGE_ID=$(docker image inspect --format '{{.Id}}' "$IMAGE")

log "source $PG_REPO @ $PG_COMMIT"
mkdir -p "$WORK"
if [ ! -d "$SRC/.git" ]; then
  git init -q "$SRC"
  git -C "$SRC" remote add origin "$PG_REPO"
fi
if ! git -C "$SRC" cat-file -e "$PG_COMMIT^{commit}" 2>/dev/null; then
  git -C "$SRC" fetch --depth 1 origin "$PG_COMMIT"
fi
git -C "$SRC" checkout -q --force --detach "$PG_COMMIT"
git -C "$SRC" clean -qfdx
[ "$(git -C "$SRC" rev-parse HEAD)" = "$PG_COMMIT" ]

log "patches"
for p in "$HERE"/patches/*.patch; do
  echo "  $(basename "$p")"
  git -C "$SRC" apply --whitespace=nowarn "$p"
done

log "build"
rm -rf "$OUT"
mkdir -p "$OUT"
docker run --rm \
  -e LABEL="$LABEL" -e EXTRA_CONFIGURE=--disable-spinlocks \
  -v "$SRC:/src" -v "$HERE/compat:/compat" -v "$OUT:/out" \
  -v "$HERE/pglite-build.sh:/pglite-build.sh:ro" \
  "$IMAGE" bash /pglite-build.sh

cp "$OUT/$LABEL.tar.gz" "$REPO_ROOT/$LABEL.tar.gz"
SHA=$(cd "$REPO_ROOT" && shasum -a 256 "$LABEL.tar.gz" | cut -d' ' -f1)

log "provenance"
{
  echo "$LABEL.tar.gz: PostgreSQL $PG_VERSION (PGlite WASI port) built by build/build.sh"
  echo
  echo "sha256 $SHA"
  echo
  echo "Source"
  echo "  $PG_REPO"
  echo "  branch $PG_BRANCH, commit $PG_COMMIT"
  echo
  echo "Patches (build/patches, applied with git apply)"
  for p in "$HERE"/patches/*.patch; do
    echo "  $(basename "$p")  sha256 $(shasum -a 256 "$p" | cut -d' ' -f1)"
  done
  echo
  echo "Compat shims (build/compat, compiled into the module)"
  (cd "$HERE" && find compat -type f | sort | while read -r f; do
    echo "  $f  sha256 $(shasum -a 256 "$f" | cut -d' ' -f1)"
  done)
  echo
  echo "Toolchain (build/Dockerfile)"
  echo "  image $IMAGE id $IMAGE_ID"
  echo "  base debian:bookworm-slim@$(sed -n 's/^FROM debian:bookworm-slim@//p' "$HERE/Dockerfile")"
  echo "  wasi-sdk-34.0 arm64-linux sha256 f7e243dff54d60bcc576e94d6166b69f410f2500ae4a9ceef34315be10e77971"
  echo "  wasi-sdk-34.0 x86_64-linux sha256 b761e3a0721dbae9c09a0059e5fdb2bf917d1b4a8a7b430fb3b5aafb0984b2c4"
  sed 's/^/  /' "$OUT/toolchain.txt"
  echo
  echo "Build command"
  echo "  build/build.sh  (runs build/pglite-build.sh in the image)"
  echo
  echo "Archive layout (tmp/ prefix is stripped on extraction, see setup.go)"
  echo "  tmp/pglite/bin/pglite.wasi      the module; initdb and postgres are symlinks to it"
  echo "  tmp/pglite/share/postgresql     share files including timezone data"
  echo "  tmp/pglite/lib/postgresql       empty .so placeholders; plpgsql and dict_snowball are linked in"
  echo "  tmp/pglite/password"
} > "$REPO_ROOT/$LABEL.tar.gz.txt"

log "done"
ls -l "$REPO_ROOT/$LABEL.tar.gz"
echo "sha256 $SHA"
