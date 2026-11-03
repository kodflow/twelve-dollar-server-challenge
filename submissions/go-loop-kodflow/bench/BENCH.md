# BENCH: how this submission was measured, and what came out

This is the public summary of the measurements behind `README.md`. Each step below keeps its
label (S9 to S25), so the references in the README still point somewhere. The raw results are
not committed. `bench/campaign.sh` and `bench/userload/step.sh` reproduce them on any pair of
machines.

## The bench

- **Box under test**: a 1 vCPU, 2 GB virtual machine with no swap, as rule 8 requires. Nothing
  on it is tuned: no sysctl, no CPU pinning, the unit's `LimitNOFILE=65535` unchanged. The box
  ran Debian 13 with Linux 6.12 up to S17, and Linux 7.3.1 from S19 on. S18 and S27 ran on a second
  box with the challenge's own system, Ubuntu 24.04 with its GA 6.8 kernel, built the same way
  from the official cloud image each time. Every run refused to start
  while `/proc/swaps` had an entry.
- **Generator**: a separate machine on the same host. Its CPU stayed far from saturation in every
  run kept here (10 % at 10,000 users; 29 % on average past 140,000 users from S16 on).
- **Anchors**: two earlier submissions, built and run by the same harness on the same box.
  - go-epoll #72 (`go-epoll-MegaPanchamZ`) scored 165,000 users in batch 2.
  - c-iouring #112 scored 150,000 users in batch 2.
- **Two measures.**
  - *CPU per request* at 10,000 users. `bench/load.js` runs with a 60 s ramp and a 300 s hold,
    three runs per target, interleaved. The figure is the server cgroup's `cpu.stat` usage over
    the hold, divided by the requests k6 counted in the hold. `bench/report.sh` writes the table.
  - *Users held*, as the challenge scores them. `bench/userload` ramps users for 120 s, holds
    them for 300 s and ramps them down over 30 s. A step passes when k6's thresholds (p95, p99
    under 1 s, errors) hold over the whole run, as in `bench/load.js`.

## Where it ended

The final build is three io_uring rings (`MaxConns` 196,000), `BatchWait` 4 ms, and `CQBatch` 64
from Linux 6.13 on, 512 (the SDK's default) before (S30).
It runs on SDK `internal` v0.26.50, `pkg/v1/net` v0.5.30 and `pkg/v1/app` v0.16.50.

| | go-loop-kodflow | go-epoll #72 | c-iouring #112 |
|---|---|---|---|
| CPU per request, 10,000 users, Linux 6.12 (S14) | **35.0 µs** | 75.3 µs | 55.7 µs |
| CPU per request, 10,000 users, Ubuntu 24.04 / 6.8, shipped code with `CQBatch` 64 (S27; the same cost as 512 at this load, S24) | **37.1 µs** | 73.8 µs | 55.8 µs |
| CPU per request, 10,000 users, Ubuntu 24.04 / 6.8, S14 build (S18) | **37.3 µs** | 74.5 µs | 57.5 µs |
| CPU per request, 10,000 users, Linux 7.3.1 (S19, S25) | **35.2–37.6 µs** | 78.1 µs | 57.3 µs |
| p99 at 10,000 users | 6.6–6.7 ms | 0.9–1.0 ms | 6.5–6.7 ms |
| Users held, Linux 6.12 (S16, S17) | 172,500 2/2, 180,000 1/2 | 140,000 1/1, 150,000 0/1 | 150,000 0/1 |
| Users held, Ubuntu 24.04 / 6.8, shipped build (S30, S31) | 150,000 2/2, 165,000 2/2, 172,500 2/2, **180,000 2/2**, 187,500 1/2 | fails at 165,000 | — |
| Users held, Ubuntu 24.04 / 6.8, first shipped build, `CQBatch` 64 (S27, S28) | 150,000 0/2, 165,000 0/2, 172,500 0/2 | — | — |
| Users held, Ubuntu 24.04 / 6.8, S14 build (S18) | 165,000 1/2, 172,500 0/2 | fails at 165,000 | — |
| Users held, Linux 7.3.1 (S19, S24, S25) | 180,000 4/4, 187,500 2/2, 195,000 1/2 | — | — |

The 4 ms grouped wait costs the latency it buys. The p99 at 10,000 users is #112's level, far
under the 1 s bound.

On the challenge's own kernel the shipped build passes #72 and #112 on CPU per request (S27) and
holds 180,000 users two times in two (S31), more than #72's batch-2 score of 165,000; #72 itself
collapses at 165,000 here. Its first version, with `CQBatch` 64 on every kernel, held none of six
holds from 150,000 to 172,500 on 6.8: they failed in the 30 s ramp-down (S28), where completions
queued behind the requests still served; 512 a round cut that (S29). The likely source, not
profiled, is the SDK's sweep after mass closes on a kernel before 6.13. `CQBatch` now follows
the kernel (S30), and the 6.8 kernel refuses incremental provided buffers, so the server runs there
with whole buffers.

## The ceiling: one vCPU and the network softirq

Past about 180,000 users a hold can fall off a cliff that has nothing to do with memory. The
fall goes like this (S17, S22, S25):

1. At steady state the box is about 65 % busy, and a quarter of that is network softirq.
2. Once responses pass about 40 ms, the clients' delayed ACKs pile up and softirq climbs from
   about 25 % to 75 % in a few seconds.
3. The loop loses the single CPU and completions drop to zero.
4. TCP receive memory jumps past `tcp_mem`.
5. When the loop gets the CPU back, the backlog costs hundreds of MB of provided buffers, and
   that memory stays.

A profile at 187,500 users (S25) puts 42.5 % of the server's CPU in the kernel's network path,
against 22.7 % in SQLite and JSON rendering. The largest single leaf is the established-socket
hash lookup, 7.6 % of the server's samples. The kernel sizes that table at boot from the 2 GB of
memory, and changing it would mean tuning the box, which rule 11 forbids.

No change inside the process removed the cliff:

- smaller loop turns, with `CQBatch` at 32 or 16;
- a time budget per turn (`BatchBudget`);
- receive bundles;
- whole provided buffers.

Each of these bounds the turns that feed the cliff, but none moved the point where it starts.
More rings would not help either: the cliff comes before the 196,000-connection limit.

## Steps

| step | what changed | outcome |
|---|---|---|
| S9 | Profile at 10,000 users. Engine work: `unix-excl` VFS, deferred reads, SSE2 JSON, PGO. | Engine microbench: feed 26.0 → 18.6 µs, load-test mix 16.3 → 13.1 µs per op. The loop's own yield in `EndBatch` brought 75.2 → 63.4 µs/req. No time-window group commit (the SDK cannot hold an answer past its batch), and no JWT cache (rule 5). |
| S10 | SDK grouped wait (32 completions or 250 µs) and yield. | The gap to #112 did not move: at 1,060 req/s the loop wakes for one or two completions, so the grouped wait never compounds. |
| S11 | Users and their caps. | One io_uring ring of direct descriptors caps the server at 65,535 connections; raising the fd limit alone did not hold 140,000. |
| S12 | Three rings (`MaxConns` 196,000). | 120,000 users held, at 6.9 µs/req for the extra rings. Kept, since users are what the challenge scores. |
| S13 | SDK batch-cost fixes, one cgo call per batch, WAL frames in one write, `BatchWait` 250 µs vs 2 ms vs 4 ms, one ring vs three. | A 4 ms wait groups 4.1 requests per batch (250 µs groups 1.3) and cuts 21–22 µs/req at one ring and at three; p99 6.6 ms. Shipped: 4 ms. |
| S13b | Ceiling and #112 on the same steps. | The steps past 120,000 turned out to be limited by the generator (see S15/S16). |
| S14 | SDK: rings fill in order. | Three rings cost what one does below 65,535 connections: 42.1 → 35.0 µs/req (#72 75.3, #112 55.7). |
| S15 | Per-second timelines on both machines. | Every step that failed past 120,000 had the generator's kernel at 90–98 % system time searching for source ports. The earlier steps past 120,000 measured the generator. |
| S16 | Corrected generator: eight source addresses, `-linger0`, 30 s ramp-down, per-second timeline. | 140,000 4/4, 150,000 2/2, 165,000 2/2. #72 holds 140,000, #112 does not hold 150,000. |
| S17 | Ceiling, with #72 on the same steps. | 172,500 2/2, 180,000 1/2. #72 collapses at 150,000 and 165,000 (CPU 96 %, 58–60 % of requests failing). The 180,000 failure is the softirq cliff above. |
| S18 | The challenge's system: Ubuntu 24.04, kernel 6.8, a second box with the same shape, with the S14 build. | 37.3 µs/req (#72 74.5, #112 57.5). 165,000 held once in two holds, 172,500 not held. #72 collapses at 165,000. |
| S19 | Linux 7.3.1, built from kernel.org with the distribution's config, everything else unchanged. | 180,000 held every time (p99 under 62 ms), at 7 % more CPU per request (37.6 µs/req). 7.3.1 caches far more of the mmapped database, so peak cgroup memory there is page cache, not memory per connection. |
| S20 | SDK incremental provided buffers (`IOU_PBUF_RING_INC`), on vs off. | Kept on: 37.4 vs 38.7 µs/req, no receive starved. With whole buffers, every hold starved receives. |
| S21 | SDK receive bundles, on vs off. | Dropped: the same CPU per request, more memory, and the only fall of the four holds. |
| S22–S23 | Diagnosis of the 180,000 cliff (a trace of loop turns, inert unless `LOOP_TRACE` is set). | Delayed-ACK storms start once responses pass about 40 ms. The slow seconds come from turns of 300–1,400 operations whose answers all leave at the end of the turn. |
| S24 | `CQBatch` (per-turn reap) and `BatchBudget` (per-turn time budget). | Both bound the turns, and neither removes the cliff. With `CQBatch` 64: 187,500 2/2, 195,000 1/2. |
| S25 | `CQBatch` 64, 32, 16, and 64 with a 2 ms budget, four holds each at 180,000 users, plus a CPU profile at 187,500. | `CQBatch` 64 holds 4/4 at 35.2 µs/req and stays the default. 32 holds 4/4 with no fewer slow seconds; 16 and the budget each fell once. |
| S27 | The shipped build on the challenge's system (Ubuntu 24.04, kernel 6.8), S18's method; the start line reads `uring-pbuf on`, `uring-pbuf-inc absent` (whole buffers) and `cq_batch` 64. | 37.1 µs/req (#72 73.8, #112 55.8, two interleaved runs each). 165,000 0/2, both on p99 alone: 1.08 and 1.50 s over the run, 0.47 and 1.12 s over the hold, no errors. 172,500 0/2: 1.10 s, then the softirq cliff (p99 14.8 s, 0.06 % errors). #72 collapses at 165,000 (63 % errors). |
| S28 | The shipped build at 150,000 users on the same 6.8 system, two holds. | 0/2: p99 1.77 s over each run, 0.36 and 0.03 s over the hold, no errors, no listen overflow. The requests over 1 s started in the 30 s ramp-down (38–40 % of its requests, up to 11.5 s), when the box went to 80–95 % busy, most of it system time, as 5,000 connections a second closed. The same ramp-down cost 0.3–0.9 % of its requests on 6.12 and with the S14 build on 6.8 (S16–S18), and none on 7.3.1 (S19, S25). |
| S29 | A/B at 150,000 on 6.8, one hold each: `LOOP_CQ_BATCH=512` against 64. | Both passed: p99 352 and 821 ms over the run, 71 and 19 ms over the hold; ramp-down requests over 1 s 7.3 % against 21.8 %. One hold each: the ramp-down's share varies from run to run (64: 21.8 % here, 38–40 % in S28; the S14 build on 6.8: 0.3–0.6 % at 165,000, 14–29 % at 172,500). |
| S30 | `CQBatch` by kernel: 512 before Linux 6.13, where the SDK sweeps, 64 from it; `LOOP_CQ_BATCH` overrides it. On 6.8, no environment. | 42/42; the start line reads `cq_batch` 512, `cq_batch_source` `kernel < 6.13`. 150,000 2/2 (p99 242 and 352 ms over the run), 165,000 2/2 (225 and 190 ms); ramp-down requests over 1 s 0.13–0.90 %. |
| S31 | The same build on 6.8 at 172,500, 180,000 and 187,500 users, two holds each, stopping at the first step not held twice. | 172,500 2/2 (p99 514 and 263 ms), 180,000 2/2 (288 and 243 ms; 104 ms at most over the hold; memory peak 473 MB), 187,500 1/2: the second fell off the softirq cliff (p99 11.2 s, 0.03 % errors). Hold cost 29.1–30.0 µs a request. |

## Reproducing

```bash
# From the repository root, with ssh command lines for your own machines:
export GEN_SSH="ssh user@LOADGEN_HOST" TARGET_SSH="ssh user@TARGET_HOST" \
       HOST_SSH="ssh root@JUMP_HOST" TARGET_URL="http://TARGET_HOST:80"
bash submissions/go-loop-kodflow/bench/campaign.sh prepare   # sources, install.sh, build.sh on the box
bash submissions/go-loop-kodflow/bench/campaign.sh run       # 10,000 users, 3 interleaved runs, report

# On the generator, beside the userload binary: one 5-minute hold.
SOURCES=192.0.2.9,192.0.2.10 TARGET=TARGET_HOST:80 USERS=180000 bash step.sh s25-180k
```

`bench/targets.tsv` lists the submission's variants and the anchors. `bench/vm-target.sh`
prepares a target the way the challenge does: `install.sh` as root, `build.sh` as a normal user,
and `start.sh` under systemd. `bench/microbench` runs the engine alone, one operation per batch.
