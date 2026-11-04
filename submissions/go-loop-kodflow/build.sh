#!/usr/bin/env bash
# Builds bin/server as a normal user: the SQLite amalgamation and the batch
# engine as one C translation unit (.deps/libengine.a), then the Go binary,
# which links it through cgo. Dependencies are pinned by digest (SQLite) and
# by go.sum (Go modules).
set -euo pipefail
cd "$(dirname "$0")"

SQLITE_VERSION=3540100
SQLITE_URL="https://www.sqlite.org/2026/sqlite-amalgamation-${SQLITE_VERSION}.zip"
SQLITE_SHA3=92281c86e2957f9b9305890f9ac04204caa7b677cc868dc326d942689df28e30
SQLITE_SHA256=0a9c8e34352dad46006aa82836d5383bff9dc6bc9ce5a60553c05aa30e6ad1f9

export PATH="/usr/local/go/bin:$PATH"
export GOTOOLCHAIN=local GOFLAGS=-mod=readonly CGO_ENABLED=1

case "$(uname -m)" in
  x86_64 | amd64)
    # x86-64-v3 (AVX2) and not -march=native: the scored box is a Cascade
    # Lake whose AVX-512 has broken other entries' builds.
    MARCH="-march=x86-64-v3"
    export GOAMD64=v3
    ;;
  *) MARCH="" ;;
esac

mkdir -p .deps bin
zip=".deps/sqlite-amalgamation-${SQLITE_VERSION}.zip"
if [ ! -f .deps/sqlite/sqlite3.c ]; then
  if [ ! -f "$zip" ]; then
    # Download beside the target and rename, so an interrupted transfer
    # never leaves a truncated zip that fails every later build.
    curl -fsSL --retry 3 "$SQLITE_URL" -o "$zip.part"
    mv "$zip.part" "$zip"
  fi
  sum256="$( (sha256sum "$zip" 2>/dev/null || shasum -a 256 "$zip") | awk '{print $1}')"
  sum3="$(openssl dgst -sha3-256 "$zip" | awk '{print $NF}')"
  if [ "$sum256" != "$SQLITE_SHA256" ] || [ "$sum3" != "$SQLITE_SHA3" ]; then
    echo "SQLite checksum mismatch (sha256 $sum256, sha3-256 $sum3); removed $zip, run build.sh again" >&2
    rm -f "$zip"
    exit 1
  fi
  rm -rf .deps/sqlite ".deps/sqlite-amalgamation-${SQLITE_VERSION}"
  unzip -q -o "$zip" -d .deps
  mv ".deps/sqlite-amalgamation-${SQLITE_VERSION}" .deps/sqlite
fi

# Compile options from https://sqlite.org/compile.html#rcmd, plus
# THREADSAFE=2 (each connection used by one thread at a time: the loops' one
# under the engine mutex, the checkpointer's on its goroutine) and NORMAL as
# the WAL default. Nothing here changes durability: the pragmas set it explicitly.
SQLITE_DEFS="-DSQLITE_THREADSAFE=2 -DSQLITE_DQS=0 -DSQLITE_DEFAULT_MEMSTATUS=0 \
 -DSQLITE_DEFAULT_WAL_SYNCHRONOUS=1 -DSQLITE_LIKE_DOESNT_MATCH_BLOBS \
 -DSQLITE_MAX_EXPR_DEPTH=0 -DSQLITE_OMIT_DECLTYPE -DSQLITE_OMIT_DEPRECATED \
 -DSQLITE_OMIT_PROGRESS_CALLBACK -DSQLITE_OMIT_SHARED_CACHE -DSQLITE_OMIT_AUTOINIT \
 -DSQLITE_OMIT_LOAD_EXTENSION -DSQLITE_USE_ALLOCA -DHAVE_FDATASYNC=1"
# The engine is compiled with GCC's profile-guided optimisation: once
# instrumented, linked into csrc/train.c, run on a throwaway database built
# from the challenge's schema.sql, then again with the profile it left. The
# object keeps one name across both compiles, since GCC names the profile
# after it. Any failing step, or a compiler other than GCC, falls back to the
# plain build.
CFLAGS_ENGINE="-O2 $MARCH -fno-semantic-interposition -fomit-frame-pointer"
SCHEMA=../../schema.sql
engine_plain() {
  # shellcheck disable=SC2086
  cc $CFLAGS_ENGINE $SQLITE_DEFS -I.deps/sqlite -Icsrc -c csrc/engine.c -o .deps/engine.o
}
engine_pgo() (
  set -e
  cc -v 2>&1 | grep -q '^gcc version' || { echo "build: cc is not GCC, no PGO"; exit 1; }
  [ -f "$SCHEMA" ] || { echo "build: no $SCHEMA, no PGO"; exit 1; }
  prof="$PWD/.deps/pgo"
  rm -rf "$prof"
  # shellcheck disable=SC2086
  cc $CFLAGS_ENGINE $SQLITE_DEFS -fprofile-generate="$prof" -I.deps/sqlite -Icsrc -c csrc/engine.c -o .deps/engine.o
  # shellcheck disable=SC2086
  cc -O2 $MARCH -fprofile-generate="$prof" -I.deps/sqlite -Icsrc csrc/train.c .deps/engine.o -lm -o .deps/train
  scratch="$(mktemp -d)"
  trap 'rm -rf "$scratch"' EXIT
  .deps/train "$scratch/train.db" "$SCHEMA"
  # shellcheck disable=SC2086
  cc $CFLAGS_ENGINE $SQLITE_DEFS -fprofile-use="$prof" -fprofile-correction -Wno-missing-profile \
    -I.deps/sqlite -Icsrc -c csrc/engine.c -o .deps/engine.o
)
if [ ! -f .deps/libengine.a ] || [ csrc/engine.c -nt .deps/libengine.a ] || [ csrc/engine.h -nt .deps/libengine.a ] ||
  [ csrc/train.c -nt .deps/libengine.a ] || [ build.sh -nt .deps/libengine.a ]; then
  echo "build: compiling SQLite $SQLITE_VERSION and the engine"
  if engine_pgo; then
    echo "build: engine built with PGO"
  else
    echo "build: PGO unavailable, plain build"
    engine_plain
  fi
  rm -f .deps/libengine.a
  ar rcs .deps/libengine.a .deps/engine.o
fi

# The engine's digest goes into the binary: the go command does not hash the
# static library cgo links, so without it an engine rebuilt under unchanged
# Go sources would leave bin/server "up to date" with the old engine.
engine_sum="$( (sha256sum .deps/libengine.a 2>/dev/null || shasum -a 256 .deps/libengine.a) | cut -c1-16)"
go build -trimpath -ldflags="-s -w -X main.engineBuild=$engine_sum" -o bin/server .
echo "build: bin/server ($(go version))"
