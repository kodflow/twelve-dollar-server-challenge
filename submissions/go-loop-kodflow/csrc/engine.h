/*
 * Batch engine: one call runs a whole batch of requests against SQLite inside
 * one transaction and writes every response body into an arena.
 *
 * Calls into this file never overlap, except eng_checkpoint: the Go side holds
 * one mutex around each other call (loop threads ending a batch, the
 * checkpoint goroutine). eng_checkpoint runs on a second connection of its
 * own, which is why SQLite is built SQLITE_THREADSAFE=2 (one connection per
 * thread at a time).
 */
#ifndef ENGINE_H
#define ENGINE_H

#include <stdint.h>

enum {
    ENG_HEALTH = 1,
    ENG_FEED = 2,
    ENG_GET_POST = 3,
    ENG_CREATE_POST = 4,
    ENG_LIKE_POST = 5
};

/* One request of a batch. Go fills kind, args and the input offsets; the
 * engine fills status and the body's place in the output arena. */
typedef struct {
    uint8_t kind;
    uint8_t pad0;
    uint16_t status;
    int32_t text_off; /* create: trimmed body, in the input buffer */
    int32_t text_len;
    int32_t user_off; /* create: token username, in the input buffer */
    int32_t user_len;
    int32_t pad1;
    int64_t arg1; /* get/like: post id; create: user id */
    int64_t arg2; /* like: user id */
    int64_t out_off;
    int64_t out_len;
} eng_op;

typedef struct {
    int64_t wal_frames;
    int64_t arena_cap;
} eng_stats;

/* What Go reads after eng_exec without another cgo call: the output arena
 * base and the WAL's size in frames after the last commit (as the WAL hook
 * saw it; it drops when a writer restarts the WAL). Written by eng_exec only,
 * valid until the next eng_exec. */
typedef struct {
    const char *out;
    int64_t wal_frames;
} eng_view;

typedef struct eng eng;

/* eng_open opens path, applies and verifies the pragmas and prepares every
 * statement. On failure it returns NULL and writes a message into err. */
eng *eng_open(const char *path, int max_ops, char *err, int errcap);

/* eng_ops returns the engine-owned array of max_ops ops Go fills. */
eng_op *eng_ops(eng *e);

/* eng_exec runs ops[0..n) in one transaction. in holds the text the ops point
 * into (may be NULL when no op carries text). Every op gets a status and a
 * body once it returns; nothing may be sent before it returns. It returns 0
 * when the batch committed, 1 when every response was rewritten as an error
 * because the transaction failed. */
int eng_exec(eng *e, int n, const char *in, int64_t uptime_s);

/* eng_view_of returns the engine's view, at an address fixed until eng_close. */
const eng_view *eng_view_of(eng *e);

/* eng_out returns the output arena base, valid until the next eng_exec. */
const char *eng_out(eng *e);

/* eng_wal_frames returns eng_view's wal_frames. */
int eng_wal_frames(eng *e);

/* eng_checkpoint runs a PASSIVE checkpoint on the checkpointer's own
 * connection and returns the SQLite code (0 is SQLITE_OK), with the WAL's
 * frames and those now in the database in nlog and nckpt. It is the one
 * call that may overlap the others: it touches nothing but that connection.
 * Two eng_checkpoint calls never overlap each other. */
int eng_checkpoint(eng *e, int *nlog, int *nckpt);

void eng_get_stats(eng *e, eng_stats *out);

/* eng_pragma_text returns the text value of a read-only pragma query, for the
 * startup line; the pointer is valid until the next call. */
const char *eng_pragma_text(eng *e, const char *sql);

const char *eng_sqlite_version(void);

void eng_close(eng *e);

#endif
