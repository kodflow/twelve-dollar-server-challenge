#!/usr/bin/env bash
# Target side of the harness. Runs as root on the box under test (TARGET_HOST);
# campaign.sh ships it to $BASE/harness and calls it over ssh with sudo.
#
#   vm-target.sh prepare <target> <dir>        install.sh as root, build.sh as deploy
#   vm-target.sh start   <target> <tree> <dir> [K=V]
#                                              fresh seed copy, detached systemd unit running the
#                                              build prepared as <tree>, wait for /health
#   vm-target.sh collect <target> <run-dir> <hold-start> <hold-end> <stop-at> <state-signal>
#                                              detached: samples the hold, stops the unit
#   vm-target.sh fetch   <run-dir>             tar of the run's results on stdout
#   vm-target.sh stop-all                      stops every bench unit
#
# The unit mirrors batch 2: User=deploy, LimitNOFILE=65535,
# AmbientCapabilities=CAP_NET_BIND_SERVICE, direct on 0.0.0.0:80.
set -euo pipefail

BASE=/var/lib/bench
SEED="$BASE/seed/feed.db"
JWT_SECRET=twelve-dollar-challenge
cmd="${1:?command}"
shift

unit_of() { echo "bench-$1"; }
cgroup_of() { echo "/sys/fs/cgroup/system.slice/$(unit_of "$1").service"; }
repo_of() { echo "/home/deploy/bench/$1"; }

# now: epoch seconds with nanoseconds.
now() { date +%s.%N; }

# sleep_until <epoch>: sleeps until the absolute time, never negative.
sleep_until() {
  local d
  d="$(awk -v t="$1" -v n="$(now)" 'BEGIN { d = t - n; print (d > 0 ? d : 0) }')"
  sleep "$d"
}

# sample <file>: one snapshot of the counters the report needs.
sample() {
  local target="$1" out="$2" cg
  cg="$(cgroup_of "$target")"
  {
    echo "time $(now)"
    echo "usage_usec $(awk '$1 == "usage_usec" { print $2 }' "$cg/cpu.stat")"
    echo "user_usec $(awk '$1 == "user_usec" { print $2 }' "$cg/cpu.stat")"
    echo "system_usec $(awk '$1 == "system_usec" { print $2 }' "$cg/cpu.stat")"
    echo "memory_current $(cat "$cg/memory.current")"
    # cpu user nice system idle iowait irq softirq steal guest guest_nice
    grep '^cpu ' /proc/stat
  } >"$out"
}

case "$cmd" in
prepare)
  target="$1" dir="$2"
  repo="$(repo_of "$target")"
  mkdir -p "$BASE/logs" "$BASE/seed"
  id deploy >/dev/null 2>&1 || useradd --create-home --shell /bin/bash deploy
  rm -rf "$repo"
  mkdir -p "$repo"
  tar -xf "$BASE/src/$target.tar" -C "$repo"
  shopt -s nullglob
  for p in "$BASE/harness/patches/$target"/*.patch; do
    echo "patch: $p"
    patch -p1 -d "$repo" <"$p"
  done
  chown -R deploy:deploy /home/deploy/bench
  echo "== $target install.sh (root)"
  (cd "$repo/$dir" && bash install.sh) >"$BASE/logs/$target-install.log" 2>&1 ||
    { tail -40 "$BASE/logs/$target-install.log"; exit 1; }
  echo "== $target build.sh (deploy)"
  start_s="$(date +%s)"
  # shellcheck disable=SC2024 # root writes the log on purpose
  sudo -u deploy -H bash -c "cd '$repo/$dir' && bash build.sh" >"$BASE/logs/$target-build.log" 2>&1 ||
    { tail -40 "$BASE/logs/$target-build.log"; exit 1; }
  echo "build took $(($(date +%s) - start_s)) s"
  tail -3 "$BASE/logs/$target-build.log"
  if [ ! -f "$SEED" ]; then
    echo "== seed (once)"
    command -v node >/dev/null && command -v sqlite3 >/dev/null ||
      DEBIAN_FRONTEND=noninteractive apt-get install -y -qq nodejs sqlite3 >/dev/null
    (cd "$repo" && bash seed/make-seed.sh)
    cp "$repo/seed/feed.db" "$SEED"
  fi
  ;;

start)
  target="$1" tree="$2" dir="$3"
  shift 3
  repo="$(repo_of "$tree")"
  "$0" stop-all
  # Rule 8: no swap on the scored box. Refuse to start a run with swap active.
  if [ "$(awk 'NR > 1' /proc/swaps | wc -l)" -ne 0 ]; then
    echo "swap is active (rule 8 wants none):"
    cat /proc/swaps
    exit 1
  fi
  echo "swap: none (/proc/swaps has no entry)"
  # One database copy per build, not per target: one target runs at a time,
  # and a variant row's copy would add the seed's 240 MB to a 7.4 GB disk
  # whose filling fails every write (BENCH.md, S11).
  run="$BASE/run/$tree"
  rm -rf "$run"
  mkdir -p "$run"
  cp "$SEED" "$run/feed.db"
  chown -R deploy:deploy "$run"
  extra=()
  for kv in "$@"; do extra+=("--setenv=$kv"); done
  systemd-run --unit="$(unit_of "$target")" --collect --quiet \
    -p User=deploy -p Group=deploy \
    -p LimitNOFILE=65535 -p AmbientCapabilities=CAP_NET_BIND_SERVICE \
    -p WorkingDirectory="$repo/$dir" \
    --setenv=SQLITE_PATH="$run/feed.db" --setenv=JWT_SECRET="$JWT_SECRET" \
    --setenv=HOST=0.0.0.0 --setenv=PORT=80 "${extra[@]}" \
    /bin/bash "$repo/$dir/start.sh"
  t0="$(now)"
  for _ in $(seq 600); do
    curl -sf http://127.0.0.1:80/health >/dev/null 2>&1 && break
    sleep 0.1
  done
  curl -sf http://127.0.0.1:80/health >/dev/null || {
    echo "no healthy /health after 60 s"
    journalctl -u "$(unit_of "$target")" -o cat --no-pager | tail -30
    exit 1
  }
  awk -v a="$t0" -v b="$(now)" 'BEGIN { printf "healthy after %.2f s\n", b - a }'
  ;;

collect)
  # Re-exec detached so the samples survive the ssh session that asked.
  target="$1" run_dir="$2"
  mkdir -p "$run_dir"
  setsid nohup "$0" collect-fg "$@" >"$run_dir/collect.log" 2>&1 </dev/null &
  echo "collector pid $!"
  ;;

collect-fg)
  target="$1" run_dir="$2" hold_start="$3" hold_end="$4" stop_at="$5" state_signal="$6"
  unit="$(unit_of "$target")"
  {
    echo "kernel $(uname -r)"
    echo "ntp $(timedatectl show -p NTPSynchronized --value 2>/dev/null || echo unknown)"
    nproc | sed 's/^/nproc /'
    grep -m1 'model name' /proc/cpuinfo
    free -m | awk '/^Mem:/ { print "mem_total_mb " $2 } /^Swap:/ { print "swap_total_mb " $2 }'
    echo "swaps_entries $(awk 'NR > 1' /proc/swaps | wc -l)"
  } >"$run_dir/box.txt"
  sleep_until "$hold_start"
  sample "$target" "$run_dir/hold_start.txt"
  sleep_until "$hold_end"
  sample "$target" "$run_dir/hold_end.txt"
  if [ "$state_signal" = 1 ]; then
    systemctl kill -s USR1 --kill-whom=main "$unit" || true
  fi
  sleep_until "$stop_at"
  cg="$(cgroup_of "$target")"
  cat "$cg/memory.peak" >"$run_dir/memory_peak.txt"
  grep -E '^(oom|oom_kill) ' "$cg/memory.events" >"$run_dir/memory_events.txt" || true
  systemctl stop "$unit" || true
  sleep 1
  inv="$(cat "$run_dir/invocation" 2>/dev/null || true)"
  if [ -n "$inv" ]; then
    journalctl _SYSTEMD_INVOCATION_ID="$inv" -o cat --no-pager >"$run_dir/journal.log" || true
  else
    journalctl -u "$unit" -o cat --no-pager -n 500 >"$run_dir/journal.log" || true
  fi
  touch "$run_dir/done"
  ;;

invocation)
  # invocation <target> <run-dir>: pins the unit run for journalctl.
  systemctl show -p InvocationID --value "$(unit_of "$1")" >"$2/invocation"
  ;;

fetch)
  tar -C "$1" -cf - .
  ;;

stop-all)
  for u in $(systemctl list-units --plain --no-legend 'bench-*.service' | awk '{ print $1 }'); do
    systemctl stop "$u" || true
  done
  ;;

*)
  echo "unknown command $cmd" >&2
  exit 2
  ;;
esac
