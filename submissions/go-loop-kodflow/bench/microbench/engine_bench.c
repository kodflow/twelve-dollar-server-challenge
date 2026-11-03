/*
 * Engine micro-benchmark, outside HTTP: calls the batch engine directly on a
 * copy of the seed, one op kind at a time, and reports CPU µs per op
 * (getrusage, user + system) and wall µs per op.
 *
 * Built twice by run.sh: against this submission's engine (csrc/engine.c),
 * and with -DA72 against go-epoll-MegaPanchamZ's engine at 9df7449
 * (pgo/engine.c + shim.h), so both run the same op sequence on the same data.
 *
 *   engine_bench <db> <mode> <iterations> <batch>
 *   mode: empty | feed | get | like | create | mix | load
 *
 * "empty" runs a batch with nothing in it: for this engine that is
 * BEGIN IMMEDIATE + COMMIT; the A72 engine opens no transaction for it.
 * "mix" follows bench/load.js per user loop: feed, get, 15 % like, 2 % create.
 * "load" is the same loop on the posts load.js picks: its get and like take one
 * of the 20 newest posts, the feed's, where "mix" takes any of the seed's.
 */
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/resource.h>
#include <time.h>

#ifdef A72
#include "shim.h"
#else
#include "engine.h"
#endif

static uint64_t rng = 88172645463325252ull;
static uint64_t next(void) {
    rng ^= rng << 13;
    rng ^= rng >> 7;
    rng ^= rng << 17;
    return rng;
}

static double now_us(void) {
    struct timespec t;
    clock_gettime(CLOCK_MONOTONIC, &t);
    return t.tv_sec * 1e6 + t.tv_nsec / 1e3;
}

static double cpu_us(void) {
    struct rusage r;
    getrusage(RUSAGE_SELF, &r);
    return (r.ru_utime.tv_sec + r.ru_stime.tv_sec) * 1e6 + r.ru_utime.tv_usec + r.ru_stime.tv_usec;
}

enum { K_EMPTY, K_FEED, K_GET, K_LIKE, K_CREATE };

/* feedpick makes post() pick from the feed: the seed's newest id is 500,000
 * and every create in a run takes the next one. */
static int feedpick;
static int64_t newest = 500000;

static int64_t post(void) {
    return feedpick ? newest - (int64_t)(next() % 20) : 1 + (int64_t)(next() % 500000);
}

static const char BODY[] = "user_0042 says hi at 2026-10-23T22:31:00.000Z (VU 4242, iter 3)";
static const char USER[] = "golden_ember_42";

#ifdef A72
static db_ctx *ctx;
static int pos;

static void open_db(const char *path) {
    const char *pragmas =
        "PRAGMA locking_mode = EXCLUSIVE; PRAGMA journal_mode = WAL; PRAGMA synchronous = NORMAL;"
        " PRAGMA foreign_keys = ON; PRAGMA mmap_size = 1073741824; PRAGMA cache_size = -2000;"
        " PRAGMA temp_store = MEMORY;";
    char *err = NULL;
    ctx = db_init(path, pragmas, 2048, 2 << 20, 16 << 20, &err);
    if (!ctx) {
        fprintf(stderr, "db_init: %s\n", err ? err : "?");
        exit(1);
    }
}

static void put(int i, int kind) {
    db_op *op = &ctx->ops[i];
    memset(op, 0, sizeof(*op));
    switch (kind) {
    case K_FEED: op->kind = OP_FEED; break;
    case K_GET: op->kind = OP_GET_POST; op->arg1 = post(); break;
    case K_LIKE:
        op->kind = OP_LIKE_POST;
        op->arg1 = 1 + (int64_t)(next() % 20000);  /* user */
        op->arg2 = post(); /* post */
        break;
    case K_CREATE:
        op->kind = OP_CREATE_POST;
        newest++;
        op->arg1 = 1 + (int64_t)(next() % 20000);
        memcpy(ctx->req_buf + pos, BODY, sizeof(BODY) - 1);
        op->text_off = pos, op->text_len = sizeof(BODY) - 1, pos += sizeof(BODY) - 1;
        memcpy(ctx->req_buf + pos, USER, sizeof(USER) - 1);
        op->user_off = pos, op->user_len = sizeof(USER) - 1, pos += sizeof(USER) - 1;
        break;
    }
}

static void run(int n) {
    db_exec_batch(ctx, n, 1);
    db_checkpoint_if_due(ctx);
    pos = 0;
}
#else
static eng *e;
static char in[1 << 16];
static int pos;

static void open_db(const char *path) {
    char err[256];
    e = eng_open(path, 256, err, sizeof(err));
    if (!e) {
        fprintf(stderr, "eng_open: %s\n", err);
        exit(1);
    }
}

static void put(int i, int kind) {
    eng_op *op = &eng_ops(e)[i];
    memset(op, 0, sizeof(*op));
    switch (kind) {
    case K_FEED: op->kind = ENG_FEED; break;
    case K_GET: op->kind = ENG_GET_POST; op->arg1 = post(); break;
    case K_LIKE:
        op->kind = ENG_LIKE_POST;
        op->arg1 = post(); /* post */
        op->arg2 = 1 + (int64_t)(next() % 20000);  /* user */
        break;
    case K_CREATE:
        op->kind = ENG_CREATE_POST;
        newest++;
        op->arg1 = 1 + (int64_t)(next() % 20000);
        memcpy(in + pos, BODY, sizeof(BODY) - 1);
        op->text_off = pos, op->text_len = sizeof(BODY) - 1, pos += sizeof(BODY) - 1;
        memcpy(in + pos, USER, sizeof(USER) - 1);
        op->user_off = pos, op->user_len = sizeof(USER) - 1, pos += sizeof(USER) - 1;
        break;
    }
}

static int mark;

/* run executes one batch, then checkpoints inline as the server's
 * checkpointer would past 1,024 new frames (checkpointFrames in db.go), so
 * its cost is in the op's. */
static void run(int n) {
    eng_exec(e, n, in, 1);
    int frames = eng_wal_frames(e);
    if (frames < mark) mark = 0;
    if (frames - mark >= 1024) {
        int nlog, nckpt;
        if (eng_checkpoint(e, &nlog, &nckpt) == 0) mark = nckpt;
    }
    pos = 0;
}
#endif

int main(int argc, char **argv) {
    if (argc != 5) {
        fprintf(stderr, "usage: %s <db> <empty|feed|get|like|create|mix> <iterations> <batch>\n", argv[0]);
        return 2;
    }
    const char *mode = argv[2];
    long iters = atol(argv[3]);
    int batch = atoi(argv[4]);
    open_db(argv[1]);

    int kind = !strcmp(mode, "empty") ? K_EMPTY : !strcmp(mode, "feed") ? K_FEED
             : !strcmp(mode, "get") ? K_GET : !strcmp(mode, "like") ? K_LIKE
             : !strcmp(mode, "create") ? K_CREATE : -1;
    feedpick = !strcmp(mode, "load");
    int mix = feedpick || !strcmp(mode, "mix");
    if (kind < 0 && !mix) {
        fprintf(stderr, "unknown mode %s\n", mode);
        return 2;
    }
    /* Warm the page cache and the statements before measuring. */
    for (int i = 0; i < 2000; i++) {
        put(0, i % 2 ? K_GET : K_FEED);
        run(1);
    }

    long ops = 0;
    double c0 = cpu_us(), w0 = now_us();
    for (long it = 0; it < iters; it++) {
        int n = 0;
        if (mix) {
            /* One load.js user loop, cut into batches of `batch` ops. */
            int seq[4], m = 0;
            seq[m++] = K_FEED;
            seq[m++] = K_GET;
            if (next() % 100 < 15) seq[m++] = K_LIKE;
            if (next() % 100 < 2) seq[m++] = K_CREATE;
            for (int j = 0; j < m; j++) {
                put(n++, seq[j]);
                if (n == batch) {
                    run(n);
                    ops += n;
                    n = 0;
                }
            }
        } else if (kind != K_EMPTY) {
            for (n = 0; n < batch; n++) put(n, kind);
        }
        if (n > 0 || kind == K_EMPTY) {
            run(n);
            ops += n ? n : 1;
        }
    }
    double c1 = cpu_us(), w1 = now_us();
    printf("%-6s %-6s batch=%-3d ops=%-7ld cpu_us/op=%7.2f wall_us/op=%7.2f\n",
#ifdef A72
           "a72",
#else
           "ours",
#endif
           mode, batch, ops, (c1 - c0) / ops, (w1 - w0) / ops);
    return 0;
}
