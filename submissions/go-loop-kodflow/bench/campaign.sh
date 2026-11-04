#!/usr/bin/env bash
# Fixed-load benchmark campaign for the submission and its two anchors, on the
# bench VM, driven from this machine. Every knob is an environment variable:
#
#   GEN_SSH     ssh command line to the generator          (required)
#   TARGET_SSH  ssh command line to the box under test     (required)
#   HOST_SSH    ssh command line to the hypervisor         (required)
#   TARGET_URL  what k6 hits                               (required)
#   N           virtual users                              (10000)
#   RAMP        ramp-up seconds                            (60)
#   HOLD        hold seconds                               (300)
#   RUNS        runs per target, each on a fresh start     (3)
#   LABEL       campaign label; "smoke" marks a dry run    (campaign)
#   TARGET_ENV  extra K=V added to every go-loop-kodflow* unit          (empty)
#
# The four required ones name the bench machines and have no default. With
# JUMP_HOST the hypervisor that fronts the VMs, LOADGEN_HOST the generator VM
# and TARGET_HOST the box under test, both on the hypervisor's private network:
#
#   GEN_SSH="ssh -J root@JUMP_HOST user@LOADGEN_HOST"
#   TARGET_SSH="ssh -J root@JUMP_HOST user@TARGET_HOST"
#   HOST_SSH="ssh root@JUMP_HOST"
#   TARGET_URL="http://TARGET_HOST:80"
#
# campaign.txt, and so the report, records the placeholders TARGET_HOST and
# LOADGEN_HOST, never the addresses: TARGET_URL is written with its host
# replaced.
#
# The binary sets GOMAXPROCS one above the CPUs unless the variable is
# set: targets.tsv runs the same build as go-loop-kodflow (as shipped) and
# go-loop-kodflow-gmp2 (GOMAXPROCS=2 named, the same on one vCPU).
#
#   bench/campaign.sh prepare [target...]   ship sources, install.sh + build.sh on the box
#   bench/campaign.sh run [target...]       RUNS interleaved runs per target, then the report
#   bench/campaign.sh smoke [target...]     run with N=200, RAMP=10, HOLD=30, RUNS=1, LABEL=smoke
#   bench/campaign.sh report <results-dir>  rebuild BENCH.md from fetched results
#   bench/campaign.sh rerun <results-dir> <reason> <target:run>...
#                                           set runs aside with a reason, measure them again
#
# Targets come from bench/targets.tsv; the default is all of them.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SUB="$(cd "$HERE/.." && pwd)"
REPO="$(cd "$SUB/../.." && pwd)"

: "${GEN_SSH:=}"
: "${TARGET_SSH:=}"
: "${HOST_SSH:=}"
: "${TARGET_URL:=}"
: "${N:=10000}"
: "${RAMP:=60}"
: "${HOLD:=300}"
: "${RUNS:=3}"
: "${LABEL:=campaign}"
: "${TARGET_ENV:=}"

BASE=/var/lib/bench
# k6's ramp-down stage (30 s) plus gracefulRampDown (10 s) plus slack: the
# unit is stopped only after k6 is done with it.
TAIL=55
# Generator CPU at or above this makes a run invalid (same bar as report.sh).
GEN_CPU_MAX=80

# need_hosts: every command that reaches a machine stops here first when one
# of the required variables is unset. Each ssh command line gets
# -o BatchMode=yes after the program name, so a missing key fails instead of
# prompting.
need_hosts() {
  local v
  for v in GEN_SSH TARGET_SSH HOST_SSH TARGET_URL; do
    [ -n "${!v}" ] || { echo "$v is not set (see the header of $0)" >&2; exit 2; }
  done
  read -ra GEN <<<"$GEN_SSH"
  read -ra TGT <<<"$TARGET_SSH"
  read -ra HYP <<<"$HOST_SSH"
  GEN=("${GEN[0]}" -o BatchMode=yes "${GEN[@]:1}")
  TGT=("${TGT[0]}" -o BatchMode=yes "${TGT[@]:1}")
  HYP=("${HYP[0]}" -o BatchMode=yes "${HYP[@]:1}")
}

# The URL as recorded: TARGET_URL with its host replaced by TARGET_HOST.
TARGET_URL_REC="$(sed -E 's#^([a-z]+://)?(\[[^]]*\]|[^/:]+)#\1TARGET_HOST#' <<<"$TARGET_URL")"

gen() { "${GEN[@]}" "$@"; }
tgt() { "${TGT[@]}" "$@"; }
hyp() { "${HYP[@]}" "$@"; }

now() { perl -MTime::HiRes=time -e 'printf "%.3f\n", time'; }
sleep_until() { perl -MTime::HiRes=time,sleep -e '$d = $ARGV[0] - time; sleep($d) if $d > 0' "$1"; }
log() { echo "[$(date -u +%H:%M:%S)] $*" >&2; }

# targets [names...]: the matching rows of targets.tsv, tab separated.
targets() {
  local all
  all="$(grep -v '^#' "$HERE/targets.tsv")"
  if [ $# -eq 0 ]; then
    echo "$all"
    return
  fi
  local name
  for name in "$@"; do
    awk -F'\t' -v n="$name" '$1 == n' <<<"$all" | grep . || { echo "unknown target $name" >&2; exit 2; }
  done
}

resolve_commit() {
  if [ "$1" = HEAD ]; then git -C "$REPO" rev-parse HEAD; else git -C "$REPO" rev-parse "$1^{commit}"; fi
}

ship_harness() {
  log "harness -> target box"
  tar -C "$HERE" -cf - vm-target.sh patches |
    tgt "sudo -n mkdir -p $BASE/harness $BASE/src $BASE/results && sudo -n tar -xf - -C $BASE/harness"
  log "harness -> generator"
  local sum_repo
  sum_repo="$(shasum -a 256 "$REPO/bench/load.js" | cut -d' ' -f1)"
  [ "$sum_repo" = "$(git -C "$REPO" show HEAD:bench/load.js | shasum -a 256 | cut -d' ' -f1)" ] ||
    { echo "bench/load.js differs from the committed one" >&2; exit 1; }
  [ -f "$REPO/seed/tokens.json" ] || { echo "no seed/tokens.json: bash seed/make-seed.sh" >&2; exit 1; }
  tar -C "$HERE" -cf - gen-k6.sh | gen 'mkdir -p ~/bench/runs && tar -xf - -C ~/bench'
  gen 'cat > ~/bench/load.js' <"$REPO/bench/load.js"
  gen 'cat > ~/bench/tokens.json' <"$REPO/seed/tokens.json"
}

cmd_prepare() {
  ship_harness
  local name commit dir rest sha
  # The rows come in on fd 3: ssh would swallow stdin.
  while IFS=$'\t' read -r -u 3 name commit dir _ _ _ _ tree _; do
    # A variant row reuses the build of the row it names as its tree.
    [ "$tree" = "$name" ] || { log "$name reuses the $tree build"; continue; }
    sha="$(resolve_commit "$commit")"
    log "prepare $name at $sha"
    git -C "$REPO" archive --format=tar "$sha" | tgt "sudo -n tee $BASE/src/$name.tar >/dev/null"
    tgt "sudo -n bash $BASE/harness/vm-target.sh prepare $name $dir"
    echo "$sha" | tgt "sudo -n tee $BASE/src/$name.sha >/dev/null"
  done 3< <(targets "$@")
}

# run_one <campaign-dir> <name> <dir> <state_signal> <index> <tree> <env>
run_one() {
  local out="$1" name="$2" dir="$3" signal="$4" i="$5"
  local id rdir="$1/$2/run-$5"
  id="$(basename "$out")-$name-$i"
  mkdir -p "$rdir/vm" "$rdir/gen"
  local tree="$6" extra=""
  [ "$7" != - ] && extra="$7"
  case "$name" in go-loop-kodflow*) extra="$extra $TARGET_ENV" ;; esac
  log "$name run $i: start"
  tgt "sudo -n bash $BASE/harness/vm-target.sh start $name $tree $dir $extra" >"$rdir/start.txt" ||
    { cat "$rdir/start.txt" >&2; return 1; }
  cat "$rdir/start.txt" >&2
  tgt "sudo -n mkdir -p $BASE/results/$id && sudo -n bash $BASE/harness/vm-target.sh invocation $name $BASE/results/$id" ||
    return 1
  tgt "cat $BASE/src/$tree.sha" >"$rdir/commit.txt" || return 1
  echo "${extra:-none}" >"$rdir/env.txt"
  gen "bash ~/bench/gen-k6.sh start $id $N $RAMP $HOLD $TARGET_URL" >&2 || return 1
  local t0=""
  for _ in $(seq 240); do
    t0="$(gen "cat ~/bench/runs/$id/t0.txt 2>/dev/null" || true)"
    [ -n "$t0" ] && break
    sleep 0.5
  done
  [ -n "$t0" ] || { echo "k6 never reported running" >&2; return 1; }
  local hs he stop
  hs="$(awk -v t="$t0" -v r="$RAMP" 'BEGIN { printf "%.3f", t + r }')"
  he="$(awk -v t="$hs" -v h="$HOLD" 'BEGIN { printf "%.3f", t + h }')"
  stop="$(awk -v t="$he" -v x="$TAIL" 'BEGIN { printf "%.3f", t + x }')"
  tgt "sudo -n bash $BASE/harness/vm-target.sh collect $name $BASE/results/$id $hs $he $stop $signal" >&2 ||
    return 1
  log "$name run $i: t0=$t0 hold $hs..$he"
  sleep_until "$hs"
  hyp uptime >"$rdir/host_start.txt" || true
  sleep_until "$he"
  hyp uptime >"$rdir/host_end.txt" || true
  sleep_until "$stop"
  for _ in $(seq 120); do
    gen "test -f ~/bench/runs/$id/done" && tgt "test -f $BASE/results/$id/done" && break
    sleep 2
  done
  gen "bash ~/bench/gen-k6.sh fetch $id" | tar -xf - -C "$rdir/gen"
  tgt "sudo -n bash $BASE/harness/vm-target.sh fetch $BASE/results/$id" | tar -xf - -C "$rdir/vm"
  local rc gencpu
  rc="$(cat "$rdir/gen/k6_exit.txt" 2>/dev/null || echo '?')"
  gencpu="$(gen_cpu "$rdir/gen")"
  log "$name run $i: done (k6 exit $rc, generator CPU ${gencpu:-?} %)"
  if awk -v g="${gencpu:-100}" -v m="$GEN_CPU_MAX" 'BEGIN { exit !(g >= m) }'; then
    log "$name run $i: generator saturated, run invalid"
    return 3
  fi
  # A failed hold is reported at once, never only in the final table.
  if [ "$rc" != 0 ] || grep -qs 'oom_kill [1-9]' "$rdir/vm/memory_events.txt"; then
    log "ALERT $name run $i: hold failed (k6 exit $rc; $(grep -s oom_kill "$rdir/vm/memory_events.txt" | tr '\n' ' '))"
    grep -E 'http_req_duration|http_req_failed|dropped' "$rdir/gen/k6.log" | head -4 >&2 || true
  fi
}

# gen_cpu <gen-dir>: the generator's busy % over the hold, from /proc/stat.
gen_cpu() {
  [ -f "$1/hold_start.txt" ] && [ -f "$1/hold_end.txt" ] || return 0
  awk '$1 == "cpu" { n++; for (k = 2; k <= 9; k++) v[n, k] = $k }
    END { t = 0; for (k = 2; k <= 9; k++) t += v[2, k] - v[1, k]
          idle = (v[2, 5] - v[1, 5]) + (v[2, 6] - v[1, 6]); if (t > 0) printf "%.1f", 100 * (1 - idle / t) }' \
    "$1/hold_start.txt" "$1/hold_end.txt"
}

cmd_run() {
  local stamp out
  stamp="$(date -u +%Y%m%dT%H%M%SZ)"
  out="$HERE/results/$stamp-$LABEL"
  mkdir -p "$out"
  ship_harness
  {
    echo "label=$LABEL"
    echo "date=$stamp"
    echo "n=$N ramp_s=$RAMP hold_s=$HOLD runs=$RUNS"
    echo "target_url=$TARGET_URL_REC gen_host=LOADGEN_HOST"
    echo "target_env=$TARGET_ENV"
    echo "harness_commit=$(git -C "$REPO" rev-parse HEAD)"
  } >"$out/campaign.txt"
  targets "$@" >"$out/targets.tsv"
  local i name commit dir signal
  for i in $(seq "$RUNS"); do
    while IFS=$'\t' read -r -u 3 name commit dir _ _ signal _ tree env; do
      # A run the harness could not complete, or whose generator saturated,
      # is set aside and run again, at most twice.
      local attempt
      for attempt in 1 2 3; do
        run_one "$out" "$name" "$dir" "$signal" "$i" "$tree" "$env" && break
        log "$name run $i attempt $attempt discarded"
        mv "$out/$name/run-$i" "$out/$name/discarded-run-$i-$attempt"
      done
    done 3<"$out/targets.tsv"
  done
  tgt "sudo -n bash $BASE/harness/vm-target.sh stop-all" || true
  bash "$HERE/report.sh" "$out"
}

# cmd_rerun <results-dir> <reason> <target:run>...: sets each run aside with
# its reason and measures it again in the same campaign, then the report.
cmd_rerun() {
  local out="$1" reason="$2" spec name run row dir signal tree env k
  shift 2
  ship_harness
  for spec in "$@"; do
    name="${spec%:*}" run="${spec#*:}"
    row="$(awk -F'\t' -v n="$name" '$1 == n' "$out/targets.tsv")"
    [ -n "$row" ] || { echo "unknown target $name" >&2; exit 2; }
    IFS=$'\t' read -r _ _ dir _ _ signal _ tree env <<<"$row"
    if [ -d "$out/$name/run-$run" ]; then
      k=1
      while [ -e "$out/$name/discarded-run-$run-$k" ]; do k=$((k + 1)); done
      mv "$out/$name/run-$run" "$out/$name/discarded-run-$run-$k"
      echo "$reason" >"$out/$name/discarded-run-$run-$k/rejected.txt"
    fi
    run_one "$out" "$name" "$dir" "$signal" "$run" "$tree" "$env" || log "$name run $run failed again"
  done
  tgt "sudo -n bash $BASE/harness/vm-target.sh stop-all" || true
  bash "$HERE/report.sh" "$out"
}

case "${1:-}" in
rerun) shift; need_hosts; cmd_rerun "$@" ;;
prepare) shift; need_hosts; cmd_prepare "$@" ;;
run) shift; need_hosts; cmd_run "$@" ;;
smoke)
  shift
  need_hosts
  N=200 RAMP=10 HOLD=30 RUNS=1 LABEL=smoke
  cmd_run "$@"
  ;;
report) bash "$HERE/report.sh" "${2:?results dir}" ;;
*)
  sed -n '2,40p' "$0"
  exit 2
  ;;
esac
