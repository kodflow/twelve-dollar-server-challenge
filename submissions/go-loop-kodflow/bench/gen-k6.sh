#!/usr/bin/env bash
# Generator side of the harness. Runs as the generator's user (LOADGEN_HOST);
# campaign.sh ships it with bench/load.js and seed/tokens.json to ~/bench.
#
#   gen-k6.sh start <run-id> <vus> <ramp-s> <hold-s> <base-url>   detached k6 run
#   gen-k6.sh fetch <run-id>                                      tar of the run on stdout
#
# The runner raises the soft nofile limit to the hard one, starts k6 with its
# REST API on localhost, takes t0 as the first instant k6 reports the test
# running, and samples the k6 request counter and /proc/stat at the hold's
# start (t0 + ramp) and end (t0 + ramp + hold). t0 is written to t0.txt for the
# controller, which hands the same instants to the target-side collector.
set -euo pipefail

HERE="$HOME/bench"
K6="${K6:-$HOME/bin/k6}"
API=localhost:6565
cmd="${1:?command}"
shift

now() { date +%s.%N; }
sleep_until() {
  local d
  d="$(awk -v t="$1" -v n="$(now)" 'BEGIN { d = t - n; print (d > 0 ? d : 0) }')"
  sleep "$d"
}

# counter <metric>: the k6 counter's current count, from the REST API.
counter() {
  curl -s "http://$API/v1/metrics/$1" | sed -n 's/.*"count":\([0-9]*\).*/\1/p'
}

sample() {
  {
    echo "time $(now)"
    echo "http_reqs $(counter http_reqs)"
    echo "iterations $(counter iterations)"
    echo "vus $(curl -s "http://$API/v1/status" | sed -n 's/.*"vus":\([0-9]*\).*/\1/p')"
    grep '^cpu ' /proc/stat
  } >"$1"
}

case "$cmd" in
start)
  run="$HERE/runs/$1"
  rm -rf "$run"
  mkdir -p "$run"
  setsid nohup "$0" fg "$@" >"$run/runner.log" 2>&1 </dev/null &
  echo "runner pid $!"
  ;;

fg)
  id="$1" vus="$2" ramp="$3" hold="$4" url="$5"
  run="$HERE/runs/$id"
  ulimit -n "$(ulimit -Hn)"
  {
    echo "nofile_soft $(ulimit -Sn)"
    echo "nofile_hard $(ulimit -Hn)"
    echo "port_range $(tr '\t' ' ' </proc/sys/net/ipv4/ip_local_port_range)"
    echo "nproc $(nproc)"
    echo "k6 $("$K6" version | head -1)"
    echo "load_js_sha256 $(sha256sum "$HERE/load.js" | cut -d' ' -f1)"
    echo "ntp $(timedatectl show -p NTPSynchronized --value 2>/dev/null || echo unknown)"
  } >"$run/gen.txt"
  "$K6" run --address "$API" --summary-export "$run/summary.json" \
    -e BASE_URL="$url" -e VUS="$vus" -e RAMP_UP="${ramp}s" -e DURATION="${hold}s" \
    -e TOKENS="$HERE/tokens.json" "$HERE/load.js" >"$run/k6.log" 2>&1 &
  k6pid=$!
  for _ in $(seq 1200); do
    curl -s "http://$API/v1/status" 2>/dev/null | grep -q '"running":true' && break
    kill -0 "$k6pid" 2>/dev/null || break
    sleep 0.1
  done
  t0="$(now)"
  echo "$t0" >"$run/t0.txt"
  start="$(awk -v t="$t0" -v r="$ramp" 'BEGIN { printf "%.3f", t + r }')"
  end="$(awk -v t="$t0" -v r="$ramp" -v h="$hold" 'BEGIN { printf "%.3f", t + r + h }')"
  sleep_until "$start"
  sample "$run/hold_start.txt"
  sleep_until "$end"
  sample "$run/hold_end.txt"
  rc=0
  wait "$k6pid" || rc=$?
  echo "$rc" >"$run/k6_exit.txt"
  touch "$run/done"
  ;;

fetch)
  tar -C "$HERE/runs/$1" -cf - .
  ;;

*)
  echo "unknown command $cmd" >&2
  exit 2
  ;;
esac
