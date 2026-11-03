#!/usr/bin/env bash
# Builds <results-dir>/BENCH.md from the runs campaign.sh fetched.
#
#   bench/report.sh bench/results/<stamp>-<label>
#
# Per run: µs/req = Δcpu.stat usage_usec / Δk6 http_reqs over the hold, CPU %,
# cgroup memory.peak, steal on the box, CPU of the generator, k6's verdict at N.
# Per target: medians over the valid runs. Then the extrapolation
# users_ext = c × 10^6 / (µs_req × 0.098), with c calibrated so the anchors
# land on their batch-2 scores, capped by memory and file descriptors, and the
# verdict on the medians.
set -euo pipefail

OUT="${1:?results dir}"
# Memory the app may use on a 2 GB box before the OS has to give way (MB).
MEM_BUDGET_MB=1700
# Generator CPU above which a run is invalid: the load, not the target, is
# what was measured.
GEN_CPU_MAX=80
# CPU used on the box outside the target's cgroup above which a run is
# invalid: something else was running on the machine under test.
FOREIGN_CPU_MAX=5

# kv <file>: "key value" lines and the /proc/stat cpu line as one JSON object.
kv() {
  [ -f "$1" ] || { echo null; return; }
  awk '
    $1 == "cpu" { printf "%s\"cpu\":[%s,%s,%s,%s,%s,%s,%s,%s]", sep, $2, $3, $4, $5, $6, $7, $8, $9; sep = ","; next }
    NF >= 2 && $2 ~ /^-?[0-9.]+$/ { printf "%s\"%s\":%s", sep, $1, $2; sep = "," }
  ' "$1" | sed 's/^/{/; s/$/}/'
}

# first_line <file> <pattern>: the first line matching, else the file's first
# line (the anchors have no JSON startup line).
first_line() {
  [ -f "$1" ] || return 0
  grep -m1 "$2" "$1" || head -1 "$1" | cut -c1-300
}

runs_json() {
  local rdir name i
  for rdir in "$OUT"/*/run-*; do
    [ -d "$rdir" ] || continue
    name="$(basename "$(dirname "$rdir")")"
    i="${rdir##*-}"
    jq -n \
      --arg name "$name" --argjson run "$i" \
      --arg commit "$(cat "$rdir/commit.txt" 2>/dev/null || echo unknown)" \
      --arg env "$(cat "$rdir/env.txt" 2>/dev/null || echo none)" \
      --argjson vs "$(kv "$rdir/vm/hold_start.txt")" --argjson ve "$(kv "$rdir/vm/hold_end.txt")" \
      --argjson gs "$(kv "$rdir/gen/hold_start.txt")" --argjson ge "$(kv "$rdir/gen/hold_end.txt")" \
      --arg peak "$(cat "$rdir/vm/memory_peak.txt" 2>/dev/null || echo 0)" \
      --arg events "$(cat "$rdir/vm/memory_events.txt" 2>/dev/null || true)" \
      --arg k6exit "$(cat "$rdir/gen/k6_exit.txt" 2>/dev/null || echo -1)" \
      --slurpfile summary <(cat "$rdir/gen/summary.json" 2>/dev/null || echo '{}') \
      --arg healthy "$(grep -m1 'healthy after' "$rdir/start.txt" 2>/dev/null || true)" \
      --arg host_start "$(cat "$rdir/host_start.txt" 2>/dev/null || true)" \
      --arg host_end "$(cat "$rdir/host_end.txt" 2>/dev/null || true)" \
      --arg startline "$(first_line "$rdir/vm/journal.log" '"event":"start"')" \
      --arg stateline "$(grep -m1 '"signal":"SIGUSR1"' "$rdir/vm/journal.log" 2>/dev/null || true)" \
      --arg gentxt "$(cat "$rdir/gen/gen.txt" 2>/dev/null || true)" \
      '
      def d(a; b; k): if (a == null or b == null or a[k] == null or b[k] == null) then null else b[k] - a[k] end;
      def cpu(a; b): if (a == null or b == null or a.cpu == null or b.cpu == null) then null else [range(0; 8) | b.cpu[.] - a.cpu[.]] end;
      ($summary[0].metrics // {}) as $m
      | cpu($vs; $ve) as $vc | cpu($gs; $ge) as $gc
      | d($vs; $ve; "time") as $wall
      | d($vs; $ve; "usage_usec") as $usage
      | d($gs; $ge; "http_reqs") as $reqs
      | ($gc | if . == null then null else 100 * (1 - (.[3] + .[4]) / add) end) as $gencpu
      | {
          target: $name, run: $run, commit: $commit, env: $env,
          wall_s: $wall, usage_s: (if $usage then $usage / 1e6 else null end),
          hold_reqs: $reqs,
          rps: (if $reqs and $wall then $reqs / $wall else null end),
          us_req: (if $usage and $reqs and $reqs > 0 then $usage / $reqs else null end),
          cpu_pct: (if $usage and $wall then 100 * $usage / 1e6 / $wall else null end),
          mem_peak_mb: (($peak | tonumber) / 1048576),
          oom_kills: ($events | capture("oom_kill (?<n>[0-9]+)").n // "0" | tonumber),
          steal_pct: ($vc | if . == null then null else 100 * .[7] / add end),
          # CPU spent on the box by anything but the target cgroup, as a share
          # of the hold: user+nice+system+irq+softirq from /proc/stat (USER_HZ
          # = 100) minus the unit usage_usec. The cgroup is charged softirq
          # work done on its behalf, so a quiet box sits at 0.5-2.5 %.
          foreign_cpu_pct: (if $vc == null or $usage == null or $wall == null then null
                            else 100 * (((($vc[0] + $vc[1] + $vc[2] + $vc[5] + $vc[6]) / 100)) - $usage / 1e6) / $wall end),
          iowait_pct: ($vc | if . == null then null else 100 * .[4] / add end),
          gen_cpu_pct: $gencpu,
          k6_exit: ($k6exit | tonumber),
          p95_ms: $m.http_req_duration["p(95)"], p99_ms: $m.http_req_duration["p(99)"],
          err_rate: ($m.http_req_failed.value // $m.http_req_failed.rate),
          reqs_total: $m.http_reqs.count,
          dropped_iterations: ($m.dropped_iterations.count // 0),
          healthy: $healthy, host_load_start: $host_start, host_load_end: $host_end,
          startup_line: $startline, state_line: $stateline, generator: $gentxt
        }
      | .valid = (.k6_exit == 0 and .us_req != null and .oom_kills == 0
                  and .foreign_cpu_pct != null and .foreign_cpu_pct <= '"$FOREIGN_CPU_MAX"'
                  and .gen_cpu_pct != null and .gen_cpu_pct < '"$GEN_CPU_MAX"')
      '
  done | jq -s .
}

runs="$(runs_json)"
echo "$runs" >"$OUT/runs.json"
# The bench machines' addresses never reach the report: a campaign.txt that
# still records them prints TARGET_HOST and LOADGEN_HOST instead.
campaign="$(sed -E 's#^(target_url=([a-z]+://)?)(\[[^]]*\]|[^/: ]+)#\1TARGET_HOST#; s#gen_host=[^ ]*#gen_host=LOADGEN_HOST#' "$OUT/campaign.txt")"
label="$(sed -n 's/^label=//p' "$OUT/campaign.txt")"
n="$(sed -n 's/^n=\([0-9]*\).*/\1/p' "$OUT/campaign.txt")"
runs_wanted="$(sed -n 's/.* runs=\([0-9]*\).*/\1/p' "$OUT/campaign.txt")"

# Targets with their batch-2 scores and caps, as JSON.
targets="$(awk -F'\t' '!/^#/ && NF { printf "%s{\"name\":\"%s\",\"dir\":\"%s\",\"b2_users\":%s,\"b2_us\":%s,\"fd_cap\":%s}", s, $1, $3, ($4 == "-" ? "null" : $4), ($5 == "-" ? "null" : $5), ($7 == "-" ? "null" : $7); s = "," }' "$OUT/targets.tsv" | sed 's/^/[/; s/$/]/')"

summary="$(jq -n --argjson runs "$runs" --argjson targets "$targets" \
  --argjson n "$n" --argjson want "$runs_wanted" --argjson budget "$MEM_BUDGET_MB" '
  def median: sort | if length == 0 then null elif length % 2 == 1 then .[length / 2 | floor] else (.[length / 2 - 1] + .[length / 2]) / 2 end;
  def users(c; us): if us == null or us <= 0 then null else c * 1e6 / (us * 0.098) end;
  [ $targets[] | . as $t
    | [ $runs[] | select(.target == $t.name) ] as $all
    | [ $all[] | select(.valid) ] as $ok
    | $t + {
        runs: ($all | length), valid: ($ok | length),
        us_med: ([ $ok[].us_req ] | median), us_min: ([ $ok[].us_req ] | min), us_max: ([ $ok[].us_req ] | max),
        cpu_med: ([ $ok[].cpu_pct ] | median), rps_med: ([ $ok[].rps ] | median),
        mem_med: ([ $ok[].mem_peak_mb ] | median), p99_med: ([ $ok[].p99_ms ] | median),
        steal_max: ([ $all[].steal_pct | select(. != null) ] | max),
        gen_cpu_max: ([ $all[].gen_cpu_pct | select(. != null) ] | max)
      } ] as $rows
  | [ $rows[] | select(.b2_users != null and .us_med != null) | .b2_users * .us_med * 0.098 / 1e6 ] as $cs
  | (if ($cs | length) > 0 then ($cs | add / length) else null end) as $c
  | [ $rows[] | . + {
        users_ext: (if $c then users($c; .us_med) else null end),
        mem_cap: (if .mem_med and .mem_med > 0 then $n * $budget / .mem_med else null end)
      }
      | . + { users_capped: (if .users_ext == null then null else [ .users_ext, .mem_cap, .fd_cap ] | map(select(. != null)) | min end) }
      | . + { residual_pct: (if .b2_users and .users_ext then 100 * (.users_ext - .b2_users) / .b2_users else null end) }
    ] as $rows
  | ($rows | map({ (.name): . }) | add) as $by
  | {
      c: $c, rows: $rows,
      complete: ([ $rows[] | .valid >= $want ] | all),
      beats_72: ($by["go-loop-kodflow"].users_capped != null and $by["anchor-72"].users_capped != null
                 and $by["go-loop-kodflow"].users_capped > $by["anchor-72"].users_capped),
      cheaper_than_112: ($by["go-loop-kodflow"].us_med != null and $by["anchor-112"].us_med != null
                         and $by["go-loop-kodflow"].us_med <= $by["anchor-112"].us_med)
    }
  | .verdict = (if .complete and .beats_72 and .cheaper_than_112 then "PASS" else "FAIL" end)
  | .variants = [ $rows[] | select(.name | startswith("go-loop-kodflow")) | . as $v
      | { name: .name,
          beats_72: (.users_capped != null and $by["anchor-72"].users_capped != null and .users_capped > $by["anchor-72"].users_capped),
          cheaper_than_112: (.us_med != null and $by["anchor-112"].us_med != null and .us_med <= $by["anchor-112"].us_med) }
      | .bar = (if .beats_72 and .cheaper_than_112 then "PASS" else "FAIL" end) ]
')"
echo "$summary" >"$OUT/summary.json"

fmt() { jq -r "$1" <<<"$summary"; }
{
  if [ "$label" = smoke ]; then
    echo "# BENCH (SMOKE RUN — harness dry run, not a measurement)"
  else
    echo "# BENCH — fixed-load campaign"
  fi
  echo
  echo "verdict: $(fmt .verdict)  (go-loop-kodflow, env as in targets.tsv)"
  echo
  echo '```'
  echo "$campaign"
  echo '```'
  echo
  echo "Verdict: PASS iff every target has its runs valid, the submission's capped users_ext"
  echo "beats go-epoll #72, and its median µs/req is at most c-iouring #112's. The two bars are"
  echo "**correlated**: users_ext is a monotone function of µs/req with one shared c, so beating #112 on"
  echo "µs/req nearly implies beating #72's extrapolation; only the caps separate them."
  echo
  echo "Box: TARGET_HOST with swap turned off for the campaign (swapoff -a, not persistent);"
  echo "every run refused to start unless /proc/swaps had no entry (rule 8: no swap)."
  echo
  echo "Spread: each target shows its median and the min–max over its valid runs."
  echo
  echo "Caps: mem cap = N × ${MEM_BUDGET_MB} MB / memory.peak (linear, base memory included, so"
  echo "conservative and meaningless at small N); fd cap from targets.tsv; capped = min(users_ext, caps)."
  echo "users_ext uses that relation with c fitted on the two anchors (mean of their c); the"
  echo "residual column is each anchor's distance from its batch-2 score under that shared c."
  echo
  echo "complete: $(fmt .complete) · beats #72: $(fmt .beats_72) · µs/req ≤ #112: $(fmt .cheaper_than_112) · c = $(fmt '.c // "n/a"')"
  echo
  echo "Per variant of the submission (same build, GOMAXPROCS from the environment):"
  echo
  fmt '.variants[] | "- \(.name): \(.bar) (beats #72: \(.beats_72), µs/req ≤ #112: \(.cheaper_than_112))"'
  echo
  echo "| target | valid/runs | µs/req median (min–max) | CPU % | req/s | p99 ms | RAM MB | users_ext | mem cap | fd cap | capped | residual vs batch 2 |"
  echo "|---|---|---|---|---|---|---|---|---|---|---|---|"
  fmt '.rows[] | "| \(.name) | \(.valid)/\(.runs) | \(.us_med // "—" | if type == "number" then (. * 10 | round / 10) else . end) (\(.us_min // "—" | if type == "number" then (. * 10 | round / 10) else . end)–\(.us_max // "—" | if type == "number" then (. * 10 | round / 10) else . end)) | \(.cpu_med // "—" | if type == "number" then round else . end) | \(.rps_med // "—" | if type == "number" then round else . end) | \(.p99_med // "—" | if type == "number" then (. * 10 | round / 10) else . end) | \(.mem_med // "—" | if type == "number" then round else . end) | \(.users_ext // "—" | if type == "number" then round else . end) | \(.mem_cap // "—" | if type == "number" then round else . end) | \(.fd_cap // "—") | \(.users_capped // "—" | if type == "number" then round else . end) | \(.residual_pct // "—" | if type == "number" then "\(. * 10 | round / 10) %" else . end) |"'
  echo
  echo "## Runs"
  echo
  echo "| target | run | valid | k6 exit | hold reqs | µs/req | CPU % | p95 ms | p99 ms | err % | RAM MB | steal % | iowait % | foreign_cpu_pct | gen CPU % | dropped it. | host load (start / end) |"
  echo "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|"
  jq -r '.[] | "| \(.target) | \(.run) | \(.valid) | \(.k6_exit) | \(.hold_reqs // "—") | \(.us_req // "—" | if type == "number" then (. * 10 | round / 10) else . end) | \(.cpu_pct // "—" | if type == "number" then (. * 10 | round / 10) else . end) | \(.p95_ms // "—" | if type == "number" then (. * 10 | round / 10) else . end) | \(.p99_ms // "—" | if type == "number" then (. * 10 | round / 10) else . end) | \(.err_rate // "—" | if type == "number" then (. * 10000 | round / 100) else . end) | \(.mem_peak_mb | round) | \(.steal_pct // "—" | if type == "number" then (. * 100 | round / 100) else . end) | \(.iowait_pct // "—" | if type == "number" then (. * 100 | round / 100) else . end) | \(.foreign_cpu_pct // "—" | if type == "number" then (. * 100 | round / 100) else . end) | \(.gen_cpu_pct // "—" | if type == "number" then (. * 10 | round / 10) else . end) | \(.dropped_iterations) | \(.host_load_start | capture("load average: (?<l>.*)").l // "—") / \(.host_load_end | capture("load average: (?<l>.*)").l // "—") |"' <<<"$runs"
  echo
  echo "## Rejected runs"
  echo
  echo "Runs set aside and run again; they count in nothing above."
  echo
  found=0
  for r in "$OUT"/*/discarded-run-*; do
    [ -d "$r" ] || continue
    found=1
    echo "- $(basename "$(dirname "$r")") $(basename "$r"): $(cat "$r/rejected.txt" 2>/dev/null || echo 'harness failure or generator saturated')"
  done
  [ "$found" = 1 ] || echo "- none"
  echo
  echo "## Startup and State lines"
  echo
  jq -r '.[] | "- \(.target) run \(.run) (\(.commit[0:12]), env \(.env)), \(.healthy)\n  - start: `\(.startup_line)`\n  - state at hold end: `\(if .state_line == "" then "n/a (target has no SIGUSR1 handler)" else .state_line end)`"' <<<"$runs"
  echo
  echo "## Generator"
  echo
  echo '```'
  jq -r 'first(.[] | .generator) // ""' <<<"$runs"
  echo '```'
} >"$OUT/BENCH.md"
echo "wrote $OUT/BENCH.md (verdict $(fmt .verdict))"
