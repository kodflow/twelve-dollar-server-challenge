#!/usr/bin/env bash
# One users step from the generator: PROCS userload processes sharing the
# source addresses in SOURCES, then merge.py. Run on the generator, with the
# userload binary, merge.py and tokens.json beside this script.
#
#   SOURCES=192.0.2.9,192.0.2.10,... TARGET=TARGET_HOST:80 USERS=150000 \
#     bash step.sh <tag>
#
#   SOURCES   comma-separated source addresses, already on the interface (required)
#   TARGET    host:port (required)
#   USERS RAMP HOLD RAMPDOWN   users, seconds (150000 120 300 30)
#   PROCS     userload processes (4); each takes every PROCS-th address
#   GOGC      for userload (400: at the default its heap's collections took
#             the generator's cores, BENCH.md S12)
#   OUT       results directory (./runs/<tag>)
#
# (192.0.2.0/24 is a placeholder, RFC 5737.) connect(2) picks a source port
# from ip_local_port_range per address; past about half of it in use, open
# or in TIME_WAIT, the generator's kernel took 90 % of its CPU and the run
# measured the generator (BENCH.md, S15). So the step refuses to start above
# 40 % of the range per address, waits for the previous run's TIME_WAIT to
# clear, widens the range for the run and puts it back after.
set -euo pipefail
tag="${1:?tag}"
HERE="$(cd "$(dirname "$0")" && pwd)"
: "${SOURCES:?comma-separated source addresses}" "${TARGET:?host:port}"
: "${USERS:=150000}" "${RAMP:=120}" "${HOLD:=300}" "${RAMPDOWN:=30}" "${PROCS:=4}" "${GOGC:=400}"
: "${OUT:=$HERE/runs/$tag}"
IFS=, read -ra src <<<"$SOURCES"
lo=1024 hi=65535
per_addr=$(((USERS + ${#src[@]} - 1) / ${#src[@]}))
if [ $((per_addr * 100)) -gt $(((hi - lo + 1) * 40)) ]; then
  echo "step: $per_addr connections per address passes 40 % of $lo-$hi; add SOURCES" >&2
  exit 2
fi
mkdir -p "$OUT"
for _ in $(seq 70); do
  tw=$(ss -Htan state time-wait | wc -l)
  [ "$tw" -lt 1000 ] && break
  sleep 1
done
echo "time_wait_at_start $(ss -Htan state time-wait | wc -l)" >"$OUT/gen.txt"
old_range=$(cat /proc/sys/net/ipv4/ip_local_port_range)
echo "$lo $hi" | sudo -n tee /proc/sys/net/ipv4/ip_local_port_range >/dev/null
trap 'echo "$old_range" | sudo -n tee /proc/sys/net/ipv4/ip_local_port_range >/dev/null' EXIT
pids=()
for ((p = 0; p < PROCS; p++)); do
  mine=()
  for ((i = p; i < ${#src[@]}; i += PROCS)); do mine+=("${src[i]}"); done
  local_list=$(IFS=,; echo "${mine[*]}")
  n=$((USERS / PROCS + (p < USERS % PROCS ? 1 : 0)))
  GOGC=$GOGC "$HERE/userload" -url "$TARGET" -users "$n" -ramp "${RAMP}s" -hold "${HOLD}s" \
    -rampdown "${RAMPDOWN}s" -linger0 -tokens "$HERE/tokens.json" -local "$local_list" \
    -out "$OUT/step-$p.json" -raw "$OUT/raw-$p" -timeline "$OUT/tl-$p.txt" >"$OUT/step-$p.log" 2>&1 &
  pids+=($!)
done
# The generator's CPU and TCP counters, once a second, for the run's length.
(while kill -0 "${pids[0]}" 2>/dev/null; do
  printf '%s ' "$(date +%s)"
  grep '^cpu ' /proc/stat | tr '\n' ' '
  awk '/^Tcp:/ { if (!h) { split($0, k); h = 1 } else for (i = 2; i <= NF; i++) if (k[i] ~ /^(RetransSegs|ActiveOpens|EstabResets)$/) printf "%s %s ", k[i], $i }' /proc/net/snmp
  echo
  sleep 1
done) >"$OUT/genstat.txt" &
rc=0
for pid in "${pids[@]}"; do wait "$pid" || rc=1; done
python3 -I "$HERE/merge.py" $(for ((p = 0; p < PROCS; p++)); do echo "$OUT/raw-$p"; done) >"$OUT/merged.json"
touch "$OUT/done"
exit $rc
