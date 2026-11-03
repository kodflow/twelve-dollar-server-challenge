# go-loop-kodflow

A Go server for the $12 Server Challenge built on the [kitsunium SDK](https://github.com/kitsunium/sdk):
the SDK's loop engine accepts and serves the connections (io_uring, else epoll, on Linux), its `http1`
profile frames every request, its verifier checks every JWT and its lifecycle runs the process; the
database requests each loop gathers during one batch run through SQLite in a single cgo call and a
single transaction, and their answers leave once it committed.

The first version of this submission (the baseline measured in `bench/BENCH.md`) ran on the SDK's
goroutine-per-connection `net/server` engine with its own HTTP framing and a batcher goroutine. It now
runs on `server.StreamGroup.HandleLoop` and `net/http1`; the SQLite engine (`csrc/engine.c`) is the
same.

| | |
|---|---|
| Language | Go 1.27.3 (cgo for SQLite only) |
| Framework | kitsunium SDK: `pkg/v1/net` v0.5.30 (`net/server`, `net/http1`), `pkg/v1/app` v0.16.50 (`app/lifecycle`), `pkg/v1/security` v0.4.7 (`token`), `pkg/v1/crypto` v0.3.3, `pkg/v1/errs` v0.2.4, `pkg/v1/proc` v0.2.10; `internal` v0.26.50 |
| SQLite | 3.54.1 amalgamation, compiled into the binary with the batch engine (`csrc/engine.c`) |
| Serving | **Direct** on `HOST:PORT` (`0.0.0.0:80` on the box), no Nginx |
| Toolchain flags | `GOTOOLCHAIN=local`, `GOFLAGS=-mod=readonly`, `GOAMD64=v3`, C at `-O2 -march=x86-64-v3` with GCC's PGO (`csrc/train.c`) |

## Measured

On a 1 vCPU / 2 GB KVM box with no swap, the submission and two batch-2 anchors were built and
started as batch 2 does (`bench/campaign.sh`, `bench/vm-target.sh`), under `bench/load.js`'s user
model. Full figures in `bench/BENCH.md`.

**On the challenge's system**, Ubuntu 24.04 with its 6.8 kernel, shipped build (S27, S30, S31):

| what | go-loop-kodflow | go-epoll #72 | c-iouring #112 |
|---|---|---|---|
| CPU per request at 10,000 users (cgroup, median of 2, interleaved; S27) | **37.1 µs** | 73.8 µs | 55.8 µs |
| p99 at 10,000 users (S27) | 6.7 ms | 1.0 ms | 6.5 ms |
| 5-minute holds, users | 150,000 2/2, 165,000 2/2, 172,500 2/2, **180,000 2/2**, 187,500 1/2 | 165,000 0/1 | — |
| memory at 180,000 users, peak | 473 MB | — | — |

A hold passes with p95 < 500 ms, p99 < 1 s and errors < 1 % over the whole run, k6's thresholds.
On 6.8 the shipped build holds 180,000 users in two holds out of two, with no error, a p99 of 243
and 288 ms over the run and 61 and 104 ms over the hold. At 187,500 one hold passed and the other
fell off the softirq cliff described below. That is more than #72's batch-2 score of 165,000; on
this bench #72 itself collapses at 165,000 (63 % errors): the load comes from one 6-vCPU generator,
not four 16-core k6 boxes, and the bench is harder than batch 2's at that size.

Two things differ on 6.8. The kernel refuses io_uring's incremental provided buffers, so the server
runs there with whole buffers (the start line reads `uring-pbuf-inc absent`). And before 6.13 the
SDK sweeps its rings after mass closes, cancelling and re-arming the recv of every open connection
(a closed descriptor's file is otherwise put only once older requests end). With `CQBatch` 64, as
first shipped, completions queued behind the requests still served when the users left, most
likely those bursts (not profiled): 27–40 %
of the 30 s ramp-down's requests took over 1 s at 150,000 and 165,000, and no step held (S27, S28).
`CQBatch` is now 512, the SDK's default, before Linux 6.13 and 64 from it, chosen from the kernel
release at start (`cq_batch_source` on the start line; `LOOP_CQ_BATCH` overrides it). With it the
ramp-down's slow requests fell to 0.1–0.9 % at 150,000 and 165,000 and 0.7–5.4 % at 172,500 and
180,000 (S30, S31). The 10,000-user figures above were measured with `CQBatch` 64; at that load the
two cost the same (S24), and the hold cost at 150,000–180,000 users with 512 is 28.6–30.0 µs a
request.

**On other kernels**, the same box shape:

| kernel | CPU per request at 10,000 users (#72, #112) | 5-minute holds, users |
|---|---|---|
| Debian 13, Linux 6.12 (S14, S16, S17) | **35.0 µs** (75.3, 55.7) | 172,500 2/2, 180,000 1/2; #72 fails 150,000, #112 fails 150,000 |
| Linux 7.3.1, a recent kernel and not the challenge's (S19, S24, S25) | **37.6 µs** with the S14 build (78.1, 57.3); 35.2 µs with the shipped build | with `CQBatch` 64 (its value from 6.13): 180,000 4/4, **187,500 2/2**, 195,000 1/2 |

The p99 at 10,000 users, 6.6–6.7 ms on every kernel, is #112's level (#72: 0.9–1.0 ms): the 4 ms
grouped wait costs the latency it buys. Past the holds above, the one vCPU is the limit: the
kernel's network softirq takes it from the loop once responses pass about 40 ms (S17, S25, S31).

## How a request is served

1. The SDK's loop engine accepts the connection on an IPv4 listener (`tcp4` for an IPv4 `HOST`;
   Go's `tcp` would take a dual-stack socket and accept TCPv6 sockets, 192 bytes of kernel memory more
   each), `MaxConns` 196,000 (three io_uring rings of direct descriptors), idle timeout 120 s. With
   `LOOP_BACKEND` unset it takes the best backend its probe reaches: io_uring, then epoll on Linux, the
   portable netpoller elsewhere; the startup line names it. `GOMAXPROCS` is one more than the CPUs and
   the SDK's `LoopsAuto` runs one loop per P but one, so one loop per CPU (one on the 1-vCPU box), each
   a goroutine locked to its OS thread. With that P to spare the SDK yields each loop to the Go
   scheduler every 5 ms, and on io_uring a loaded loop groups its waits (4 ms for 32 completions,
   where the SDK's default is 250 µs: `bench/BENCH.md`, S13); the startup line's `loop_config`
   reports both.
2. The SDK's `http1` profile frames the requests (`http1.HandleLoop`, head ≤ 16 KiB, body ≤ 64 KiB):
   Content-Length bodies only, pipelining supported. Anything it refuses (any Transfer-Encoding,
   Expect, a conflicting or non-numeric Content-Length, obs-fold, space before a colon, a bare CR or LF,
   a duplicate Authorization or Host, a version other than HTTP/1.1, CONNECT) is answered by the codec
   with its status and `Connection: close`, before the handler sees it.
3. Routing and validation (`internal/api`) answer 404, 400 and 401 at once, without touching the
   database. On the two authenticated routes the token is checked before the id, as the SPEC orders.
   The JWT is verified with the loop's own SDK scratch (`token.VerifierInto`) into the loop's own
   `ClaimsBuf`: no lock, no allocation.
4. Every other request is deferred (`Response.Defer`): its job goes into the loop's batch, with a
   create's text and username copied into the batch's input. When the loop has handled every
   completion of the iteration, the adapter calls the handler's `EndBatch`, which runs the batch in
   **one** call to `eng_exec` (512 jobs per call at most; a larger batch runs as several, each
   committed before any answer leaves):
   - `BEGIN IMMEDIATE`;
   - each write (create, like) inside `SAVEPOINT r`, then `RELEASE r`, or `ROLLBACK TO r; RELEASE r`
     and a 500 for that request alone if it failed. Reads take no savepoint, on purpose: they change
     nothing, so there is nothing to undo, and a failed read is still answered 500 on its own;
   - after any failure, `sqlite3_txn_state`: if the transaction is gone, every response of the batch
     becomes an error;
   - `COMMIT`; if it fails, every response of the batch becomes an error.
   The bodies are copied out of the engine's arena into the loop's buffer, the deferred responses are
   filled, and only then does the adapter write the batch's answers, in request order on each
   connection: no response leaves before its batch committed.
5. After a batch, when the WAL grew by 1,024 frames since the last checkpoint, the checkpoint
   goroutine wakes. It runs PASSIVE checkpoints on a second SQLite connection of its own, beside the
   loop, which keeps reading and committing meanwhile, until a pass ends with every frame copied and no
   commit since (checked under the engine mutex), so the next write restarts the WAL from frame zero.
   After eight passes that each saw a commit land, the last pass runs under the mutex.

Every other call into the engine holds one mutex, whichever loop thread ends a batch. SQLite is built
`SQLITE_THREADSAFE=2`: each connection is used by one thread at a time.

The SQL (`csrc/engine.c`):

| Request | Statements |
|---|---|
| `GET /feed` | the 20 newest ids from `posts_created_at_id_idx`; one range scan of `likes_post_id_idx` over `[min, max]` counted per post; one rowid range scan of `posts JOIN users`. Emitted in index order. If the ids span 256 or more, or a post drops out of the JOIN, the SPEC's reference query runs instead |
| `GET /posts/:id` | the SPEC's post query with `WHERE p.id = ?` |
| `POST /posts` | `INSERT INTO posts (user_id, body)`, then `SELECT created_at` by the new rowid |
| `POST /posts/:id/like` | `INSERT INTO likes … SELECT … WHERE EXISTS (post) ON CONFLICT DO NOTHING`; one row changed is 201; otherwise a post lookup tells 200 from 404 |
| `GET /health` | `SELECT 1` |

## Rules, point by point

1. **Any language and framework.** Go with the kitsunium SDK; SQLite through cgo. One process, two
   SQLite connections (the loops' and the checkpointer's), one SDK loop per CPU.
2. **SQLite only, in process.** The file at `$SQLITE_PATH`, opened read-write by the embedded
   amalgamation. No other database, cache or service.
3. **Schema and data fixed.** No DDL is ever run. `foreign_keys` stays at SQLite's default (OFF), and
   the like route checks the post exists itself (404). Consequence, stated plainly: a validly signed
   token whose `sub` names a user absent from `users` can create posts and likes for that id (the SPEC
   uses `sub` as-is, without a lookup); the seed's tokens all name existing users. `ignore_check_constraints` is never set; the 500-char
   limit is validated before the insert, and the schema's CHECK still applies.
4. **test/test.sh at 100 %.** 42/42 on macOS (portable backend) and in a Debian 12 container on a
   Linux 7.0 kernel with each of `LOOP_BACKEND=auto`, `io_uring` and `epoll`; CI runs it on Ubuntu 24.04.
5. **No caching across requests.** Every database request runs its statements inside its own batch,
   while it is being served; no response or result is kept. Every token's HMAC is computed again by the
   SDK verifier (`token.NewHS256Verifier`, through `VerifyInto`), and nothing about a verification is
   remembered: the loop's `ClaimsBuf` is overwritten by each verification and reset on each refusal,
   and its `VerifyScratch` holds the keyed MAC state only. What is reused: prepared statements
   (`SQLITE_PREPARE_PERSISTENT`), SQLite's page cache (8 MiB) and its mmap (1 GiB).
6. **Durable before the response.** `journal_mode=WAL`, `synchronous=NORMAL`, both verified at startup
   with `locking_mode=NORMAL`; the process refuses to start otherwise. Both connections open through
   SQLite's `unix-excl` VFS, which holds one exclusive POSIX lock on the file for the process, so no
   other process can open it, as with `locking_mode=EXCLUSIVE`; the engine's `eng-wal` VFS over it
   writes a commit's WAL frames in one call, before `COMMIT` returns and before the wal-index shows
   any of them. Group commit: a response is
   filled only after the `COMMIT` of its batch returned `SQLITE_OK`, in `EndBatch`, which the SDK
   adapter runs before it writes any answer of the batch. `wal_autocheckpoint=0` with PASSIVE
   checkpoints on the checkpointer's connection past 1,024 new frames, `journal_size_limit` 64 MiB,
   `busy_timeout` 5 s; never a forced RESTART.
7. **Nothing benchmark-specific.** No route, response or behaviour depends on the load test; the same
   code answers curl, the test suite and k6.
8. **1 vCPU, 2 GB, no swap.** The Go soft memory limit comes from the cgroup cap when there is one
   (`memlimit.Apply`), else 1.5 GiB. An idle connection holds no goroutine and no buffer of this
   program: only the SDK loop's per-connection slot, and a spill buffer while a request is incomplete
   (at most 80 KiB, `SpillCap`).
9. **Direct, no Nginx.** The server listens on `HOST:PORT` itself under `LimitNOFILE=65535`. On io_uring
   the SDK accepts each connection as a direct descriptor, which takes no process descriptor, across
   ⌈`MaxConns` / 65,535⌉ = 3 rings: it accepts up to 196,000 connections (`MaxConns`, which the loop
   engine honours) and closes any beyond that. Since SDK #786 (ADR 0203) the rings fill in order, a
   ring taking connections only once those before it are full, so below 65,535 connections the other
   two cost nothing; each ring's provided buffers (2 KiB) are handed out as it needs them, up to as
   many as its table holds connections (128 MiB a ring at most), never taken back. On epoll, which uses process descriptors, the
   65,535 limit binds first. Idle keep-alive connections stay open 120 s (`IdleTimeout`, honoured by the loop engine).
10. **Built from source on the box.** `install.sh` (root) installs build-essential, curl, openssl, unzip
    and the Go 1.27.3 tarball, checked against its sha256. `build.sh` (user) downloads the SQLite 3.54.1
    amalgamation, checks its sha256 **and** SHA3-256, builds the C engine and then the Go binary from
    modules pinned by `go.sum`. No prebuilt binary is shipped. The service answers `/health` within a
    second of `start.sh`, which `exec`s the binary (no restart loop).
11. **No tuning outside the process.** No sysctl, no CPU pinning. In-process only: the memory limit
    above, `GOAMD64=v3`, SQLite compile options and pragmas allowed by rule 6, and the thread counts:
    `GOMAXPROCS` one more than the CPUs (2 on the 1-vCPU box) and one loop per P but one, unless the
    `GOMAXPROCS` or `LOOPS` variable says otherwise, which the bench harness uses for its variants.
    `LOOP_BACKEND` (auto, io_uring, epoll, portable) only picks among the SDK's backends,
    `LOOP_CAPS` replaces the SDK io_uring capabilities the server opts into (`none`, or a list) and
    `LOOP_BATCH_WAIT` (`off`, or a duration) the SDK's grouped wait, all for the harness's variants.

Also from the batch-2 review: tokens without `exp` are refused (the SDK's default policy); reads never
leave before their batch's `COMMIT`; `start.sh` uses `exec`; every download is checksummed.

## Auth details

- `Authorization` must start with `Bearer ` (case-sensitive), else 401 `missing bearer token`.
- The SDK takes 32-byte keys only. HMAC zero-pads any key shorter than its 64-byte block (RFC 2104 §2),
  so the 23-byte `JWT_SECRET` padded with zeros to 32 bytes gives the same MAC; a test checks it
  against `seed/tokens.json`. A secret over 32 bytes is refused at startup.
- HS256 only, `exp` required, no leeway: any SDK refusal is 401 `invalid or expired token`.
- A signed token whose `sub` is not a positive integer string (including a JSON number or null, which
  the SDK reports as a malformed registered claim after the MAC verified), or whose `username` is
  missing or not a string, is 401 `invalid token payload`. `username: ""` is a string and is accepted.

## Optimizations, and why

- **One cgo call and one transaction per batch.** The crossing and the BEGIN/COMMIT are paid once for
  every database request one loop iteration gathered (512 per call at most): the SDK loop reaps up to
  512 completions per iteration, so under load the batch is whatever arrived meanwhile.
- **The amalgamation in the engine's translation unit** with `THREADSAFE=2`, `DEFAULT_MEMSTATUS=0`,
  `OMIT_DEPRECATED`, `OMIT_SHARED_CACHE` and the other options SQLite recommends: no connection
  mutexes, no memory accounting, SQLite's code inlined into the engine.
- **The 3-scan feed**: three index/rowid range reads instead of 20 correlated `count(*)` subqueries.
- **Profile-guided SQLite**: `build.sh` compiles the engine instrumented, runs `csrc/train.c` once on a
  throwaway database built from the challenge's `schema.sql` and seeded with synthetic rows (the load
  test's mix of feeds, reads, likes and creates), and compiles it again with that profile. On the
  bench box: feed 26.0 → 18.6 µs, post read 4.9 → 3.9 µs, the load test's mix 16.3 → 13.1 µs of CPU
  per request (`bench/microbench`). Without GCC or `schema.sql` the build is the plain `-O2` one.
- **`GOMAXPROCS` one above the loops.** A loop parked in `io_uring_enter`, or in a cgo call, leaves an
  idle P behind, so the Go scheduler's sysmon does not retake the loop's P and hand it off on every
  wait: 90.3 → 83.9 µs/req on the bench box (S9 A/B, `bench/BENCH.md`).
- **The grouped io_uring wait bounded at 4 ms** (`defaultBatchWait`, the SDK's default is 250 µs): a
  loaded loop waits for 32 completions or 4 ms, whichever comes first, so a batch holds 4.1 requests
  instead of 1.3 at 10,000 users: 63.5 → 42.1 µs/req, p95 0.57 → 4.65 ms and p99 0.88 → 6.7 ms
  (S13 A/B), the p99 at #112's level and far under the 1 s bound.
- **Rings filled in order** (SDK #786): three rings cost what one does below 65,535 connections,
  42.1 → 35.0 µs/req at 10,000 users (S13 → S14).
- **A yield every 5 ms**, now the SDK's own (ADR 0199) while a P is left over. The loop goroutine is
  locked to its thread and never otherwise goes through the scheduler; without the yield, every 10 ms
  sysmon preempted it, retook its P in the middle of an `io_uring_enter` wait, handed it off and went
  back to polling every 20 µs: 75.2 → 63.4 µs/req (S9, when `EndBatch` yielded).
- **io_uring's `DEFER_TASKRUN` with `SINGLE_ISSUER`, `NO_SQARRAY` and registered ring descriptors**,
  the SDK's opt-in capabilities: 63.4 → 60.3 µs/req.
- **`unix-excl`**: the process's one exclusive POSIX lock on the file, and the WAL index in heap memory
  shared by the two connections, so no lock reaches the kernel, as with `locking_mode=EXCLUSIVE`.
- **A commit's WAL frames in one write**: SQLite writes each frame as two calls, its 24-byte header
  then its page; the `eng-wal` VFS gathers a transaction's frames and writes them with one `pwrite`
  when the commit frame's page arrives. In the engine microbench's `load` mode a write transaction
  made 11.4 `pwrite` calls and now makes one; CPU per like 24.8 → 21.5 µs, per create 18.8 → 14.9 µs
  (local arm64 container, not the bench box).
- **The checkpoint beside the loop**, on its own connection: the loop waits for no copy and no fsync,
  except for a last pass when commits kept landing during eight passes (S7: 38 ms each on average, up to 59 ms).
- **`BEGIN DEFERRED` for a batch without a write**, no transaction at all for a batch of one health
  check or one post read (one statement, one snapshot by itself), and the JSON string escape 16 bytes at a time
  (SSE2) while no byte needs escaping.
- **The SDK loop engine and http1 profile**: no goroutine per connection, framing and answers written
  without allocating once a loop's buffers have grown, every answer of a batch written in one call per
  connection.
- **Allocation-free JWT verification**: one `VerifyScratch` and one `ClaimsBuf` per loop.

## Known limits

- The "invalid token payload" answer for a non-string `sub` reads the SDK's error detail text
  ("registered claim sub …"); if a later SDK rewords it, such tokens fall back to "invalid or expired
  token". `TestAuthCheck` ("sub number") catches that on an upgrade.
- A client that stops reading: its answers queue in the loop's output up to `OutputCap` (the SDK's
  default), past which the SDK closes the connection.
- The `http1` profile refuses some requests the old framing accepted: HTTP/1.0 and `Expect` (400 and
  417, closed), and its refusals carry an empty body rather than `{"error":"bad request"}`. curl, k6 and
  the test suite send neither; the SPEC asks for HTTP/1.1.
- A token whose `sub` or `username` is longer than 256 bytes (`token.ClaimsBufTextCap`) is refused as
  "invalid or expired token": the SDK's `ClaimsBuf` refuses rather than truncates a claim. The seed's
  usernames are far shorter.
- A loop thread is held for the length of its batch's `eng_exec`, and for a checkpoint's last pass under
  the mutex when one is needed: no read or accept on that loop meanwhile.
- Nothing sheds load before `MaxConns`; the loop engine's `SoftConns` and `EvictIdle` are not set.
  Past 196,000 open connections new ones are closed at accept. On the bench box 172,500 users held
  every hold and 180,000 one in two: past about 175,000 the kernel's network softirq can take the one
  vCPU from the loop, the sockets' receive memory passes `tcp_mem` and the hold fails on p99
  (`bench/BENCH.md`, S17). Steps before S16 measured the load generator past 120,000 users (S15).
- `SIGTERM` does not drain: the SDK's drain closes each connection with a shutdown and a linked
  cancel run on io_uring's workers, which took 249 s at 60,000 open connections, the process
  unkillable and the port held meanwhile (`bench/BENCH.md`, S11). After the State line the server
  waits for the batch in flight, closes SQLite and exits; the kernel closes the connections. An
  answer already sent was committed; a request still unanswered gets no answer.
- The feed renders post objects into the arena before copying them in feed order; the scratch copies
  stay in the arena until the batch ends (memory and one copy, not a correctness issue).
- A failed PASSIVE checkpoint is logged once (`checkpoint_error`) and counted in the State line; the
  WAL keeps growing until one succeeds.

## Observability

- On start, one JSON line on stderr: backend, GOMAXPROCS, the loop pacing the SDK resolved (`loop_config`:
  loops and their source, `batch_wait`, `batch_wait_min`, `yield`), memory limit and its source,
  RLIMIT_NOFILE, SQLite version and the pragmas read back.
- On `SIGUSR1`, and on `SIGTERM` before the exit, one JSON line with the SDK server's `State()`
  (connections, the loop group's backend, rings and per-ring counters), the engine counters (batches,
  ops, max batch, p99 batch time, checkpoints, their passes and p99, and how many made the loop wait
  for a last pass, and how long) and the Go runtime's memory view.

## Files

| | |
|---|---|
| `main.go` | environment, SDK loop group and lifecycle (`sqlite` → `http` → `state`), startup and State lines |
| `db.go`, `histogram.go` | the cgo engine, its mutex, the checkpoint goroutine and its connection, the counters |
| `csrc/engine.c`, `csrc/engine.h` | the batch engine; `build.sh` compiles it with `sqlite3.c` |
| `csrc/train.c` | the PGO training run `build.sh` links against the instrumented engine; not part of the server |
| `internal/api/` | the `http1` handler and its per-loop batch, routing, validation, JWT checks (no cgo) |
| `bench/` | the fixed-load harness used to compare this submission with two batch-2 anchors |

## Build and run

```bash
sudo bash install.sh
bash build.sh
SQLITE_PATH=/path/to/feed.db JWT_SECRET=twelve-dollar-challenge HOST=0.0.0.0 PORT=80 bash start.sh
```
