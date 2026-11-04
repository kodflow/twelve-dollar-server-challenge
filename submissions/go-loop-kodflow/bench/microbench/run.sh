#!/usr/bin/env bash
# Runs engine_bench on the bench box against this submission's engine and
# go-epoll-MegaPanchamZ's (9df7449), both as prepared by bench/campaign.sh
# prepare, on fresh copies of the seed. Run it on TARGET_HOST with sudo rights,
# when no campaign is running:
#
#   bash run.sh [perf]
#
# Variants:
#   ours        .deps/libengine.a as build.sh made it (-O2 -march=x86-64-v3)
#   ours-o3     csrc/engine.c again at -O3 -march=native (the anchor's flags)
#   a72-pgo     the anchor's .deps/libsqlite3.a as its build.sh made it (-O3 -march=native, PGO)
#   a72-nopgo   the anchor's pgo/engine.c at -O3 -march=native, no PGO
# With "perf", also records perf profiles of ours on feed and get.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
WORK="${WORK:-$HOME/microbench}"
OURS=/home/deploy/bench/go-loop-kodflow/submissions/go-loop-kodflow
A72=/home/deploy/bench/anchor-72/submissions/go-epoll-MegaPanchamZ
SEED=/var/lib/bench/seed/feed.db

mkdir -p "$WORK"
rm -rf "$WORK/ours" "$WORK/a72"
sudo cp -r "$OURS" "$WORK/ours"
sudo cp -r "$A72" "$WORK/a72"
sudo chown -R "$(id -u):$(id -g)" "$WORK/ours" "$WORK/a72"

DEFS="-DSQLITE_THREADSAFE=0 -DSQLITE_DQS=0 -DSQLITE_DEFAULT_MEMSTATUS=0 \
 -DSQLITE_DEFAULT_WAL_SYNCHRONOUS=1 -DSQLITE_LIKE_DOESNT_MATCH_BLOBS \
 -DSQLITE_MAX_EXPR_DEPTH=0 -DSQLITE_OMIT_DECLTYPE -DSQLITE_OMIT_DEPRECATED \
 -DSQLITE_OMIT_PROGRESS_CALLBACK -DSQLITE_OMIT_SHARED_CACHE -DSQLITE_OMIT_AUTOINIT \
 -DSQLITE_OMIT_LOAD_EXTENSION -DSQLITE_USE_ALLOCA -DHAVE_FDATASYNC=1"
A72FLAGS="-O3 -march=native -fno-semantic-interposition -fno-plt -fomit-frame-pointer -DHAVE_SQLITE_AMALGAMATION $DEFS"

echo "== build"
cc -O2 -g -I"$WORK/ours/csrc" "$HERE/engine_bench.c" "$WORK/ours/.deps/libengine.a" -lm -o "$WORK/bench-ours"
cc -O3 -march=native -g $DEFS -I"$WORK/ours/.deps/sqlite" -I"$WORK/ours/csrc" -c "$WORK/ours/csrc/engine.c" -o "$WORK/engine-o3.o"
cc -O2 -g -I"$WORK/ours/csrc" "$HERE/engine_bench.c" "$WORK/engine-o3.o" -lm -o "$WORK/bench-ours-o3"
cc -O2 -g -DA72 -I"$WORK/a72" -I"$WORK/a72/.deps" "$HERE/engine_bench.c" "$WORK/a72/.deps/libsqlite3.a" -lm -o "$WORK/bench-a72-pgo"
cc $A72FLAGS -g -I"$WORK/a72/.deps" -c "$WORK/a72/pgo/engine.c" -o "$WORK/a72-engine.o"
cc -O2 -g -DA72 -I"$WORK/a72" -I"$WORK/a72/.deps" "$HERE/engine_bench.c" "$WORK/a72-engine.o" -lm -o "$WORK/bench-a72-nopgo"
grep -m1 '#define SQLITE_VERSION ' "$WORK/ours/.deps/sqlite/sqlite3.c" | sed 's/^/ours sqlite: /'
grep -m1 '#define SQLITE_VERSION ' "$WORK/a72/.deps/sqlite3.c" | sed 's/^/a72 sqlite:  /'

# one <binary> <mode> <iterations> <batch>: a fresh seed copy per measurement.
one() {
  cp "$SEED" "$WORK/feed.db"
  rm -f "$WORK/feed.db-wal" "$WORK/feed.db-shm"
  "$1" "$WORK/feed.db" "$2" "$3" "$4"
}

echo "== measure"
for v in ours ours-o3 a72-pgo a72-nopgo; do
  b="$WORK/bench-$v"
  echo "-- $v"
  one "$b" empty 50000 1
  one "$b" feed 20000 1
  one "$b" get 50000 1
  one "$b" like 20000 1
  one "$b" create 5000 1
  one "$b" mix 20000 1
  one "$b" mix 20000 32
  one "$b" get 50000 32
done

if [ "${1:-}" = perf ]; then
  command -v perf >/dev/null || sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq linux-perf >/dev/null
  for m in feed get mix; do
    cp "$SEED" "$WORK/feed.db"
    rm -f "$WORK/feed.db-wal" "$WORK/feed.db-shm"
    sudo perf record -q -g -o "$WORK/perf-$m.data" "$WORK/bench-ours" "$WORK/feed.db" "$m" 20000 1 >/dev/null
    echo "== perf ours $m (self %, top 25)"
    sudo perf report -i "$WORK/perf-$m.data" --no-children --sort symbol --stdio 2>/dev/null |
      grep -E '^ +[0-9]' | head -25
  done
fi
