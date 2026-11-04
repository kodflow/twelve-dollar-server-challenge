/*
 * The SQLite side of the server. build.sh compiles this file together with the
 * amalgamation (one translation unit, so SQLite's own functions inline here)
 * into .deps/libengine.a, which the Go package links through cgo.
 */
#include "sqlite3.c"

#include "engine.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#if defined(__SSE2__)
#include <emmintrin.h>
#endif

/* A feed whose 20 ids span more than this falls back to the reference query:
 * the range scans below would read too many unrelated rows. */
#define FEED_SPAN_MAX 256
#define FEED_LEN 20

typedef struct {
    char *buf;
    size_t len;
    size_t cap;
    int oom;
} arena;

struct eng {
    sqlite3 *db;
    /* ck is the checkpointer's own connection: a checkpoint runs on it, on
     * another thread, while db keeps serving. */
    sqlite3 *ck;
    sqlite3_stmt *s_begin;
    sqlite3_stmt *s_begin_read;
    sqlite3_stmt *s_commit;
    sqlite3_stmt *s_rollback;
    sqlite3_stmt *s_savepoint;
    sqlite3_stmt *s_release;
    sqlite3_stmt *s_rollback_to;
    sqlite3_stmt *s_health;
    sqlite3_stmt *s_feed_ids;
    sqlite3_stmt *s_feed_likes;
    sqlite3_stmt *s_feed_posts;
    sqlite3_stmt *s_feed_ref;
    sqlite3_stmt *s_post;
    sqlite3_stmt *s_insert;
    sqlite3_stmt *s_created;
    sqlite3_stmt *s_like;
    sqlite3_stmt *s_exists;
    eng_op *ops;
    int max_ops;
    arena out;
    eng_view view;
    char pragma_buf[64];
};

static const char ERR_INTERNAL[] = "{\"error\":\"internal server error\"}";
static const char ERR_NOT_FOUND[] = "{\"error\":\"post not found\"}";

/* ---------------------------------------------------------------- arena */

static int ar_reserve(arena *a, size_t n) {
    if (a->len + n <= a->cap) return 1;
    size_t nc = a->cap ? a->cap : 1 << 20;
    while (nc < a->len + n) nc *= 2;
    char *nb = (char *)realloc(a->buf, nc);
    if (!nb) {
        a->oom = 1;
        return 0;
    }
    a->buf = nb;
    a->cap = nc;
    return 1;
}

static void ar_put(arena *a, const char *s, size_t n) {
    if (!ar_reserve(a, n)) return;
    memcpy(a->buf + a->len, s, n);
    a->len += n;
}

#define AR_LIT(a, lit) ar_put((a), (lit), sizeof(lit) - 1)

static void ar_int(arena *a, int64_t v) {
    char tmp[24];
    char *end = tmp + sizeof(tmp);
    char *p = end;
    uint64_t u = v < 0 ? (uint64_t)(-(v + 1)) + 1 : (uint64_t)v;
    do {
        *--p = (char)('0' + (u % 10));
        u /= 10;
    } while (u);
    if (v < 0) *--p = '-';
    ar_put(a, p, (size_t)(end - p));
}

static const char HEX[] = "0123456789abcdef";

/* ar_str appends s as a JSON string. Bytes >= 0x80 pass through: SQLite text
 * is UTF-8 and the SPEC makes escaping non-ASCII optional. */
static void ar_str(arena *a, const unsigned char *s, size_t n) {
    if (!ar_reserve(a, n * 6 + 2)) return;
    char *d = a->buf + a->len;
    *d++ = '"';
    size_t i = 0;
#if defined(__SSE2__)
    /* Sixteen bytes at a time while none needs escaping: a control byte
     * (unsigned <= 0x1f), a quote or a backslash. The first one found falls
     * through to the byte loop, which also takes the tail. */
    const __m128i quote = _mm_set1_epi8('"'), bslash = _mm_set1_epi8('\\'), ctrl = _mm_set1_epi8(0x1f);
    while (i + 16 <= n) {
        __m128i v = _mm_loadu_si128((const __m128i *)(s + i));
        __m128i bad = _mm_or_si128(_mm_or_si128(_mm_cmpeq_epi8(v, quote), _mm_cmpeq_epi8(v, bslash)),
                                   _mm_cmpeq_epi8(_mm_max_epu8(v, ctrl), ctrl));
        int mask = _mm_movemask_epi8(bad);
        if (mask) {
            int run = __builtin_ctz((unsigned)mask);
            memcpy(d, s + i, (size_t)run);
            d += run;
            i += (size_t)run;
            break;
        }
        _mm_storeu_si128((__m128i *)d, v);
        d += 16;
        i += 16;
    }
#endif
    for (; i < n; i++) {
        unsigned char c = s[i];
        if (c >= 0x20 && c != '"' && c != '\\') {
            *d++ = (char)c;
            continue;
        }
        *d++ = '\\';
        switch (c) {
        case '"': *d++ = '"'; break;
        case '\\': *d++ = '\\'; break;
        case '\n': *d++ = 'n'; break;
        case '\r': *d++ = 'r'; break;
        case '\t': *d++ = 't'; break;
        case '\b': *d++ = 'b'; break;
        case '\f': *d++ = 'f'; break;
        default:
            *d++ = 'u';
            *d++ = '0';
            *d++ = '0';
            *d++ = HEX[c >> 4];
            *d++ = HEX[c & 0xf];
        }
    }
    *d++ = '"';
    a->len = (size_t)(d - a->buf);
}

static void ar_col_str(arena *a, sqlite3_stmt *s, int i) {
    const unsigned char *t = sqlite3_column_text(s, i);
    int n = sqlite3_column_bytes(s, i);
    ar_str(a, t ? t : (const unsigned char *)"", t ? (size_t)n : 0);
}

/* ar_post appends one post object in the SPEC's key order. */
static void ar_post(arena *a, int64_t id, sqlite3_stmt *s, int body, int created, int author, int64_t likes) {
    AR_LIT(a, "{\"id\":");
    ar_int(a, id);
    AR_LIT(a, ",\"body\":");
    ar_col_str(a, s, body);
    AR_LIT(a, ",\"created_at\":");
    ar_col_str(a, s, created);
    AR_LIT(a, ",\"author\":");
    ar_col_str(a, s, author);
    AR_LIT(a, ",\"like_count\":");
    ar_int(a, likes);
    ar_put(a, "}", 1);
}

/* ---------------------------------------------------------------- wal vfs */

/* SQLite writes a WAL frame as two write calls, the 24-byte frame header and
 * then the page, so a commit of n pages costs 2n pwrite system calls. The
 * eng-wal VFS is unix-excl with one change: the WAL file's frames are gathered
 * in memory while they arrive back to back and written with one call when
 * the commit frame's page arrives. That is still inside sqlite3WalFrames,
 * before the wal-index publishes a single new frame, so no reader, the
 * checkpointer's connection included, can ask for a frame not yet in the
 * file, and COMMIT returns after the write as before: durability is the same
 * as unix-excl's. Any other call on the file (read, sync, truncate, size,
 * close, file control) and any write that does not continue the gathered run
 * write the run first. The database file and the others are unix-excl's own
 * files, untouched. */

#define WALW_FRAME_HDR 24
/* A transaction that writes more than this is written in pieces of it. The
 * unix VFS's seekAndWriteFd masks every write to 0x1ffff bytes (nBuf &=
 * 0x1ffff): a longer run is cut, then its multiple of 128 KiB is asked for as
 * zero bytes, written as none, and COMMIT answers SQLITE_FULL (S13). */
#define WALW_MAX 0x1ffff

typedef struct {
    sqlite3_file base;
    sqlite3_file *real;
    char *buf;
    size_t len;
    size_t cap;
    sqlite3_int64 off;
    /* commit: the last frame header gathered is a commit frame's. */
    int commit;
} walw_file;

/* real is opened right after the walw_file, aligned for any member it has. */
#define WALW_HEAD ((sizeof(walw_file) + 15) & ~(size_t)15)

static sqlite3_vfs walw_vfs;

static sqlite3_file *walw_real(sqlite3_file *f) { return ((walw_file *)f)->real; }

static int walw_flush(walw_file *w) {
    if (w->len == 0) return SQLITE_OK;
    int rc = w->real->pMethods->xWrite(w->real, w->buf, (int)w->len, w->off);
    w->len = 0;
    w->commit = 0;
    return rc;
}

static int walw_write(sqlite3_file *f, const void *z, int n, sqlite3_int64 off) {
    walw_file *w = (walw_file *)f;
    int rc;
    if (w->len && (off != w->off + (sqlite3_int64)w->len || w->len + (size_t)n > WALW_MAX)) {
        if ((rc = walw_flush(w)) != SQLITE_OK) return rc;
    }
    /* Only a frame header starts a run: the WAL header and the checksum
     * rewrites go straight to the file. */
    if (w->len == 0 && n != WALW_FRAME_HDR) return w->real->pMethods->xWrite(w->real, z, n, off);
    if (w->len + (size_t)n > w->cap) {
        size_t nc = w->cap ? w->cap : 32 << 10;
        while (nc < w->len + (size_t)n) nc *= 2;
        char *nb = (char *)realloc(w->buf, nc);
        if (!nb) {
            if ((rc = walw_flush(w)) != SQLITE_OK) return rc;
            return w->real->pMethods->xWrite(w->real, z, n, off);
        }
        w->buf = nb;
        w->cap = nc;
    }
    if (w->len == 0) w->off = off;
    memcpy(w->buf + w->len, z, (size_t)n);
    w->len += (size_t)n;
    if (n == WALW_FRAME_HDR) {
        /* Bytes 4-7 of a frame header: the database size after a commit,
         * zero on every other frame. */
        const unsigned char *h = (const unsigned char *)z;
        w->commit = (h[4] | h[5] | h[6] | h[7]) != 0;
        return SQLITE_OK;
    }
    return w->commit ? walw_flush(w) : SQLITE_OK;
}

static int walw_close(sqlite3_file *f) {
    walw_file *w = (walw_file *)f;
    int rc = walw_flush(w);
    int rc2 = w->real->pMethods->xClose(w->real);
    free(w->buf);
    w->buf = NULL;
    w->cap = 0;
    return rc != SQLITE_OK ? rc : rc2;
}

/* WALW_FLUSHED defines a method that writes the gathered run, then delegates. */
#define WALW_FLUSHED(name, params, args)                                  \
    static int walw_##name params {                                       \
        int rc = walw_flush((walw_file *)f);                              \
        if (rc != SQLITE_OK) return rc;                                   \
        return walw_real(f)->pMethods->x##name args;                      \
    }
WALW_FLUSHED(Read, (sqlite3_file * f, void *z, int n, sqlite3_int64 off), (walw_real(f), z, n, off))
WALW_FLUSHED(Truncate, (sqlite3_file * f, sqlite3_int64 size), (walw_real(f), size))
WALW_FLUSHED(Sync, (sqlite3_file * f, int flags), (walw_real(f), flags))
WALW_FLUSHED(FileSize, (sqlite3_file * f, sqlite3_int64 *size), (walw_real(f), size))
WALW_FLUSHED(Lock, (sqlite3_file * f, int lock), (walw_real(f), lock))
WALW_FLUSHED(Unlock, (sqlite3_file * f, int lock), (walw_real(f), lock))
WALW_FLUSHED(CheckReservedLock, (sqlite3_file * f, int *out), (walw_real(f), out))
WALW_FLUSHED(FileControl, (sqlite3_file * f, int op, void *arg), (walw_real(f), op, arg))
WALW_FLUSHED(ShmMap, (sqlite3_file * f, int pg, int sz, int ext, void volatile **pp), (walw_real(f), pg, sz, ext, pp))
WALW_FLUSHED(ShmLock, (sqlite3_file * f, int off, int n, int flags), (walw_real(f), off, n, flags))
WALW_FLUSHED(ShmUnmap, (sqlite3_file * f, int del), (walw_real(f), del))
WALW_FLUSHED(Fetch, (sqlite3_file * f, sqlite3_int64 off, int n, void **pp), (walw_real(f), off, n, pp))
WALW_FLUSHED(Unfetch, (sqlite3_file * f, sqlite3_int64 off, void *p), (walw_real(f), off, p))

static int walw_sector_size(sqlite3_file *f) { return walw_real(f)->pMethods->xSectorSize(walw_real(f)); }

static int walw_device(sqlite3_file *f) {
    return walw_real(f)->pMethods->xDeviceCharacteristics(walw_real(f));
}

static void walw_shm_barrier(sqlite3_file *f) { walw_real(f)->pMethods->xShmBarrier(walw_real(f)); }

static const sqlite3_io_methods walw_io = {
    3,
    walw_close,
    walw_Read,
    walw_write,
    walw_Truncate,
    walw_Sync,
    walw_FileSize,
    walw_Lock,
    walw_Unlock,
    walw_CheckReservedLock,
    walw_FileControl,
    walw_sector_size,
    walw_device,
    walw_ShmMap,
    walw_ShmLock,
    walw_shm_barrier,
    walw_ShmUnmap,
    walw_Fetch,
    walw_Unfetch,
};

static int walw_open(sqlite3_vfs *vfs, sqlite3_filename name, sqlite3_file *f, int flags, int *out) {
    sqlite3_vfs *real = (sqlite3_vfs *)vfs->pAppData;
    if (!(flags & SQLITE_OPEN_WAL)) return real->xOpen(real, name, f, flags, out);
    walw_file *w = (walw_file *)f;
    memset(w, 0, sizeof(*w));
    w->real = (sqlite3_file *)((char *)f + WALW_HEAD);
    int rc = real->xOpen(real, name, w->real, flags, out);
    if (rc != SQLITE_OK) return rc;
    if (w->real->pMethods->iVersion < 3) {
        w->real->pMethods->xClose(w->real);
        return SQLITE_CANTOPEN;
    }
    w->base.pMethods = &walw_io;
    return SQLITE_OK;
}

/* eng_vfs registers eng-wal over unix-excl once and returns its name. */
static const char *eng_vfs(void) {
    if (walw_vfs.zName) return walw_vfs.zName;
    sqlite3_vfs *real = sqlite3_vfs_find("unix-excl");
    if (!real) return "unix-excl";
    walw_vfs = *real;
    walw_vfs.pNext = NULL;
    walw_vfs.zName = "eng-wal";
    walw_vfs.pAppData = real;
    walw_vfs.szOsFile = (int)WALW_HEAD + real->szOsFile;
    walw_vfs.xOpen = walw_open;
    if (sqlite3_vfs_register(&walw_vfs, 0) != SQLITE_OK) {
        walw_vfs.zName = NULL;
        return "unix-excl";
    }
    return walw_vfs.zName;
}

/* ---------------------------------------------------------------- open */

static int on_wal(void *arg, sqlite3 *db, const char *name, int frames) {
    (void)db;
    (void)name;
    ((eng *)arg)->view.wal_frames = frames;
    return SQLITE_OK;
}

static int prep(eng *e, const char *sql, sqlite3_stmt **out) {
    return sqlite3_prepare_v3(e->db, sql, -1, SQLITE_PREPARE_PERSISTENT, out, NULL);
}

static void set_err(char *err, int cap, const char *what, const char *detail) {
    snprintf(err, (size_t)cap, "%s: %s", what, detail ? detail : "unknown");
}

const char *eng_pragma_text(eng *e, const char *sql) {
    sqlite3_stmt *s = NULL;
    e->pragma_buf[0] = 0;
    if (sqlite3_prepare_v2(e->db, sql, -1, &s, NULL) != SQLITE_OK) return e->pragma_buf;
    if (sqlite3_step(s) == SQLITE_ROW) {
        const unsigned char *t = sqlite3_column_text(s, 0);
        if (t) snprintf(e->pragma_buf, sizeof(e->pragma_buf), "%s", (const char *)t);
    }
    sqlite3_finalize(s);
    return e->pragma_buf;
}

eng *eng_open(const char *path, int max_ops, char *err, int errcap) {
    eng *e = (eng *)calloc(1, sizeof(eng));
    if (!e) {
        set_err(err, errcap, "open", "out of memory");
        return NULL;
    }
    e->max_ops = max_ops;
    e->ops = (eng_op *)calloc((size_t)max_ops, sizeof(eng_op));
    if (!e->ops || !ar_reserve(&e->out, 1 << 20)) {
        set_err(err, errcap, "open", "out of memory");
        eng_close(e);
        return NULL;
    }
    int rc = sqlite3_initialize();
    /* unix-excl: the process takes one exclusive POSIX lock on the file, the
     * first time it locks it, and keeps it; every later lock, of the file and
     * of the wal-index, is kept in this process's memory, as is the
     * wal-index itself. Other processes are locked out as with
     * locking_mode=EXCLUSIVE, the lock traffic stays out of the kernel, and
     * the checkpointer's connection below still shares the wal-index.
     * eng-wal is unix-excl with the WAL frames of a commit written in one
     * call (see the wal vfs above). */
    if (rc == SQLITE_OK) rc = sqlite3_open_v2(path, &e->db, SQLITE_OPEN_READWRITE, eng_vfs());
    if (rc != SQLITE_OK) {
        set_err(err, errcap, "open", e->db ? sqlite3_errmsg(e->db) : sqlite3_errstr(rc));
        eng_close(e);
        return NULL;
    }
    /* NORMAL locking mode, so the checkpointer's connection can run a
     * checkpoint while this one serves; the unix-excl VFS keeps that from
     * costing a system call per transaction. wal_autocheckpoint=0 installs
     * SQLite's own hook, so ours goes in after it. */
    static const char *pragmas =
        "PRAGMA locking_mode=NORMAL;"
        "PRAGMA journal_mode=WAL;"
        "PRAGMA synchronous=NORMAL;"
        "PRAGMA busy_timeout=5000;"
        "PRAGMA mmap_size=1073741824;"
        "PRAGMA cache_size=-8192;"
        "PRAGMA temp_store=MEMORY;"
        "PRAGMA journal_size_limit=67108864;"
        "PRAGMA wal_autocheckpoint=0;";
    char *msg = NULL;
    if (sqlite3_exec(e->db, pragmas, NULL, NULL, &msg) != SQLITE_OK) {
        set_err(err, errcap, "pragmas", msg);
        sqlite3_free(msg);
        eng_close(e);
        return NULL;
    }
    sqlite3_wal_hook(e->db, on_wal, e);
    /* Read the schema now rather than on the first request. */
    if (sqlite3_exec(e->db, "SELECT count(*) FROM sqlite_schema", NULL, NULL, &msg) != SQLITE_OK) {
        set_err(err, errcap, "schema", msg);
        sqlite3_free(msg);
        eng_close(e);
        return NULL;
    }
    if (strcmp(eng_pragma_text(e, "PRAGMA journal_mode"), "wal") != 0) {
        set_err(err, errcap, "journal_mode is not wal", e->pragma_buf);
        eng_close(e);
        return NULL;
    }
    if (strcmp(eng_pragma_text(e, "PRAGMA locking_mode"), "normal") != 0) {
        set_err(err, errcap, "locking_mode is not normal", e->pragma_buf);
        eng_close(e);
        return NULL;
    }
    if (strcmp(eng_pragma_text(e, "PRAGMA synchronous"), "1") != 0) {
        set_err(err, errcap, "synchronous is not NORMAL", e->pragma_buf);
        eng_close(e);
        return NULL;
    }
    if (prep(e, "BEGIN IMMEDIATE", &e->s_begin) ||
        prep(e, "BEGIN DEFERRED", &e->s_begin_read) ||
        prep(e, "COMMIT", &e->s_commit) ||
        prep(e, "ROLLBACK", &e->s_rollback) ||
        prep(e, "SAVEPOINT r", &e->s_savepoint) ||
        prep(e, "RELEASE r", &e->s_release) ||
        prep(e, "ROLLBACK TO r", &e->s_rollback_to) ||
        prep(e, "SELECT 1", &e->s_health) ||
        prep(e, "SELECT id FROM posts ORDER BY created_at DESC, id DESC LIMIT 20", &e->s_feed_ids) ||
        prep(e, "SELECT post_id FROM likes WHERE post_id BETWEEN ?1 AND ?2", &e->s_feed_likes) ||
        prep(e, "SELECT p.id, p.body, p.created_at, u.username FROM posts p JOIN users u ON u.id = p.user_id"
                " WHERE p.id BETWEEN ?1 AND ?2", &e->s_feed_posts) ||
        prep(e, "SELECT p.id, p.body, p.created_at, u.username,"
                " (SELECT count(*) FROM likes l WHERE l.post_id = p.id)"
                " FROM posts p JOIN users u ON u.id = p.user_id"
                " ORDER BY p.created_at DESC, p.id DESC LIMIT 20", &e->s_feed_ref) ||
        prep(e, "SELECT p.body, p.created_at, u.username,"
                " (SELECT count(*) FROM likes l WHERE l.post_id = p.id)"
                " FROM posts p JOIN users u ON u.id = p.user_id WHERE p.id = ?1", &e->s_post) ||
        prep(e, "INSERT INTO posts (user_id, body) VALUES (?1, ?2)", &e->s_insert) ||
        prep(e, "SELECT created_at FROM posts WHERE id = ?1", &e->s_created) ||
        prep(e, "INSERT INTO likes (user_id, post_id) SELECT ?2, ?1"
                " WHERE EXISTS (SELECT 1 FROM posts WHERE id = ?1)"
                " ON CONFLICT (user_id, post_id) DO NOTHING", &e->s_like) ||
        prep(e, "SELECT 1 FROM posts WHERE id = ?1", &e->s_exists)) {
        set_err(err, errcap, "prepare", sqlite3_errmsg(e->db));
        eng_close(e);
        return NULL;
    }
    /* The checkpointer's connection. It never writes a row: a PASSIVE
     * checkpoint takes no lock a reader or the writer waits on, and never
     * calls a busy handler. It must have read the file once, though, or it
     * does not know the file is in WAL mode and its checkpoints are no-ops
     * (nlog -1). */
    rc = sqlite3_open_v2(path, &e->ck, SQLITE_OPEN_READWRITE, eng_vfs());
    if (rc == SQLITE_OK)
        rc = sqlite3_exec(e->ck,
                          "PRAGMA journal_mode=WAL;"
                          "PRAGMA wal_autocheckpoint=0;"
                          "SELECT count(*) FROM sqlite_schema;",
                          NULL, NULL, &msg);
    if (rc != SQLITE_OK) {
        set_err(err, errcap, "open checkpointer", msg ? msg : (e->ck ? sqlite3_errmsg(e->ck) : sqlite3_errstr(rc)));
        sqlite3_free(msg);
        eng_close(e);
        return NULL;
    }
    return e;
}

eng_op *eng_ops(eng *e) { return e->ops; }

const char *eng_out(eng *e) { return e->out.buf; }

const eng_view *eng_view_of(eng *e) { return &e->view; }

const char *eng_sqlite_version(void) { return sqlite3_libversion(); }

void eng_close(eng *e) {
    if (!e) return;
    if (e->db) {
        sqlite3_stmt *s;
        while ((s = sqlite3_next_stmt(e->db, NULL)) != NULL) sqlite3_finalize(s);
        sqlite3_close(e->db);
    }
    if (e->ck) sqlite3_close(e->ck);
    free(e->ops);
    free(e->out.buf);
    free(e);
}

/* ---------------------------------------------------------------- ops */

/* Return codes of the op runners. */
enum { RUN_OK = 0, RUN_ERR = 1 };

static int step_once(sqlite3_stmt *s) {
    int rc = sqlite3_step(s);
    sqlite3_reset(s);
    return rc;
}

static void finish(eng_op *op, arena *a, size_t start, int status) {
    op->status = (uint16_t)status;
    op->out_off = (int64_t)start;
    op->out_len = (int64_t)(a->len - start);
}

static int run_health(eng *e, eng_op *op, int64_t uptime_s) {
    arena *a = &e->out;
    size_t start = a->len;
    if (step_once(e->s_health) == SQLITE_ROW) {
        AR_LIT(a, "{\"status\":\"ok\",\"db\":\"ok\",\"uptime_s\":");
        ar_int(a, uptime_s < 0 ? 0 : uptime_s);
        ar_put(a, "}", 1);
        finish(op, a, start, 200);
    } else {
        const char *m = sqlite3_errmsg(e->db);
        AR_LIT(a, "{\"status\":\"degraded\",\"db\":\"unreachable\",\"error\":");
        ar_str(a, (const unsigned char *)m, strlen(m));
        ar_put(a, "}", 1);
        finish(op, a, start, 503);
    }
    return a->oom ? RUN_ERR : RUN_OK;
}

/* feed_reference runs the SPEC's query, one count per post. */
static int feed_reference(eng *e, eng_op *op) {
    arena *a = &e->out;
    size_t start = a->len;
    sqlite3_stmt *s = e->s_feed_ref;
    int rc, rows = 0;
    AR_LIT(a, "{\"posts\":[");
    while ((rc = sqlite3_step(s)) == SQLITE_ROW) {
        if (rows++) ar_put(a, ",", 1);
        ar_post(a, sqlite3_column_int64(s, 0), s, 1, 2, 3, sqlite3_column_int64(s, 4));
    }
    sqlite3_reset(s);
    if (rc != SQLITE_DONE) return RUN_ERR;
    AR_LIT(a, "]}");
    finish(op, a, start, 200);
    return a->oom ? RUN_ERR : RUN_OK;
}

/* run_feed reads the 20 newest ids from the index, counts their likes with one
 * range scan of likes_post_id_idx, reads the posts with one rowid range scan,
 * and emits them in the index order. Every row is read inside this request. */
static int run_feed(eng *e, eng_op *op) {
    arena *a = &e->out;
    int64_t ids[FEED_LEN];
    int n = 0, rc;
    sqlite3_stmt *s = e->s_feed_ids;
    while ((rc = sqlite3_step(s)) == SQLITE_ROW && n < FEED_LEN) ids[n++] = sqlite3_column_int64(s, 0);
    sqlite3_reset(s);
    if (rc != SQLITE_DONE && rc != SQLITE_ROW) return RUN_ERR;
    if (n == 0) return feed_reference(e, op);
    int64_t lo = ids[0], hi = ids[0];
    for (int i = 1; i < n; i++) {
        if (ids[i] < lo) lo = ids[i];
        if (ids[i] > hi) hi = ids[i];
    }
    if (hi - lo >= FEED_SPAN_MAX) return feed_reference(e, op);

    int span = (int)(hi - lo + 1);
    uint32_t likes[FEED_SPAN_MAX];
    int8_t wanted[FEED_SPAN_MAX];
    size_t off[FEED_SPAN_MAX], len[FEED_SPAN_MAX];
    memset(likes, 0, sizeof(likes[0]) * (size_t)span);
    memset(wanted, 0, sizeof(wanted[0]) * (size_t)span);
    memset(len, 0, sizeof(len[0]) * (size_t)span);
    for (int i = 0; i < n; i++) wanted[ids[i] - lo] = 1;

    s = e->s_feed_likes;
    sqlite3_bind_int64(s, 1, lo);
    sqlite3_bind_int64(s, 2, hi);
    while ((rc = sqlite3_step(s)) == SQLITE_ROW) {
        int64_t pid = sqlite3_column_int64(s, 0);
        if (pid >= lo && pid <= hi) likes[pid - lo]++;
    }
    sqlite3_reset(s);
    if (rc != SQLITE_DONE) return RUN_ERR;

    /* Post objects are rendered into the arena first, then copied in feed
     * order; the scratch copies stay behind in the arena until the batch ends. */
    s = e->s_feed_posts;
    sqlite3_bind_int64(s, 1, lo);
    sqlite3_bind_int64(s, 2, hi);
    while ((rc = sqlite3_step(s)) == SQLITE_ROW) {
        int64_t id = sqlite3_column_int64(s, 0);
        if (id < lo || id > hi || !wanted[id - lo]) continue;
        size_t at = a->len;
        ar_post(a, id, s, 1, 2, 3, likes[id - lo]);
        off[id - lo] = at;
        len[id - lo] = a->len - at;
    }
    sqlite3_reset(s);
    if (rc != SQLITE_DONE) return RUN_ERR;
    if (a->oom) return RUN_ERR;
    for (int i = 0; i < n; i++) {
        /* A post whose author row is missing drops out of the JOIN; the
         * reference query then picks the right replacement. */
        if (len[ids[i] - lo] == 0) return feed_reference(e, op);
    }

    /* The loop below copies from the arena into the arena. That is only safe
     * because this reservation guarantees no ar_put realloc()s the buffer
     * under its own source pointer: never append to the arena between this
     * reserve and the end of the loop without growing `need` to match. */
    size_t need = 12;
    for (int i = 0; i < n; i++) need += len[ids[i] - lo] + 1;
    if (!ar_reserve(a, need)) return RUN_ERR;
    size_t start = a->len;
    AR_LIT(a, "{\"posts\":[");
    for (int i = 0; i < n; i++) {
        if (i) ar_put(a, ",", 1);
        ar_put(a, a->buf + off[ids[i] - lo], len[ids[i] - lo]);
    }
    AR_LIT(a, "]}");
    finish(op, a, start, 200);
    return a->oom ? RUN_ERR : RUN_OK;
}

static int run_get_post(eng *e, eng_op *op) {
    arena *a = &e->out;
    sqlite3_stmt *s = e->s_post;
    sqlite3_bind_int64(s, 1, op->arg1);
    int rc = sqlite3_step(s);
    size_t start = a->len;
    if (rc == SQLITE_ROW) {
        AR_LIT(a, "{\"post\":");
        ar_post(a, op->arg1, s, 0, 1, 2, sqlite3_column_int64(s, 3));
        ar_put(a, "}", 1);
        sqlite3_reset(s);
        finish(op, a, start, 200);
        return a->oom ? RUN_ERR : RUN_OK;
    }
    sqlite3_reset(s);
    if (rc != SQLITE_DONE) return RUN_ERR;
    AR_LIT(a, ERR_NOT_FOUND);
    finish(op, a, start, 404);
    return a->oom ? RUN_ERR : RUN_OK;
}

static int run_create(eng *e, eng_op *op, const char *in) {
    arena *a = &e->out;
    sqlite3_stmt *s = e->s_insert;
    sqlite3_bind_int64(s, 1, op->arg1);
    sqlite3_bind_text(s, 2, in + op->text_off, op->text_len, SQLITE_STATIC);
    int rc = step_once(s);
    sqlite3_clear_bindings(s);
    if (rc != SQLITE_DONE) return RUN_ERR;
    int64_t id = sqlite3_last_insert_rowid(e->db);

    s = e->s_created;
    sqlite3_bind_int64(s, 1, id);
    if (sqlite3_step(s) != SQLITE_ROW) {
        sqlite3_reset(s);
        return RUN_ERR;
    }
    size_t start = a->len;
    AR_LIT(a, "{\"post\":{\"id\":");
    ar_int(a, id);
    AR_LIT(a, ",\"body\":");
    ar_str(a, (const unsigned char *)in + op->text_off, (size_t)op->text_len);
    AR_LIT(a, ",\"created_at\":");
    ar_col_str(a, s, 0);
    AR_LIT(a, ",\"author\":");
    ar_str(a, (const unsigned char *)in + op->user_off, (size_t)op->user_len);
    AR_LIT(a, ",\"like_count\":0}}");
    sqlite3_reset(s);
    finish(op, a, start, 201);
    return a->oom ? RUN_ERR : RUN_OK;
}

static int run_like(eng *e, eng_op *op) {
    arena *a = &e->out;
    sqlite3_stmt *s = e->s_like;
    sqlite3_bind_int64(s, 1, op->arg1);
    sqlite3_bind_int64(s, 2, op->arg2);
    if (step_once(s) != SQLITE_DONE) return RUN_ERR;
    size_t start = a->len;
    if (sqlite3_changes(e->db) == 1) {
        AR_LIT(a, "{\"liked\":true,\"already_liked\":false,\"post_id\":");
        ar_int(a, op->arg1);
        ar_put(a, "}", 1);
        finish(op, a, start, 201);
        return a->oom ? RUN_ERR : RUN_OK;
    }
    /* Nothing inserted: either the like exists or the post does not. */
    s = e->s_exists;
    sqlite3_bind_int64(s, 1, op->arg1);
    int rc = step_once(s);
    if (rc == SQLITE_ROW) {
        AR_LIT(a, "{\"liked\":true,\"already_liked\":true,\"post_id\":");
        ar_int(a, op->arg1);
        ar_put(a, "}", 1);
        finish(op, a, start, 200);
    } else if (rc == SQLITE_DONE) {
        AR_LIT(a, ERR_NOT_FOUND);
        finish(op, a, start, 404);
    } else {
        return RUN_ERR;
    }
    return a->oom ? RUN_ERR : RUN_OK;
}

static void put_error(eng *e, eng_op *op) {
    arena *a = &e->out;
    size_t start = a->len;
    AR_LIT(a, ERR_INTERNAL);
    finish(op, a, start, 500);
}

static int in_txn(eng *e) { return sqlite3_txn_state(e->db, "main") != SQLITE_TXN_NONE; }

/* ---------------------------------------------------------------- batch */

int eng_exec(eng *e, int n, const char *in, int64_t uptime_s) {
    arena *a = &e->out;
    a->len = 0;
    a->oom = 0;
    /* A batch with no write reads one snapshot and takes no write lock. A
     * batch of one health check or one post read, most batches at the
     * bench's load, is one statement, which reads one snapshot by itself:
     * it runs without BEGIN and COMMIT, two statements less. */
    int writes = 0;
    for (int i = 0; i < n; i++) writes |= e->ops[i].kind == ENG_CREATE_POST || e->ops[i].kind == ENG_LIKE_POST;
    int txn = writes || n > 1 || (n == 1 && e->ops[0].kind == ENG_FEED);
    int failed = txn && step_once(writes ? e->s_begin : e->s_begin_read) != SQLITE_DONE;

    for (int i = 0; i < n && !failed; i++) {
        eng_op *op = &e->ops[i];
        size_t mark = a->len;
        int write = op->kind == ENG_CREATE_POST || op->kind == ENG_LIKE_POST;
        if (write && step_once(e->s_savepoint) != SQLITE_DONE) {
            failed = 1;
            break;
        }
        int rc;
        switch (op->kind) {
        case ENG_HEALTH: rc = run_health(e, op, uptime_s); break;
        case ENG_FEED: rc = run_feed(e, op); break;
        case ENG_GET_POST: rc = run_get_post(e, op); break;
        case ENG_CREATE_POST: rc = run_create(e, op, in); break;
        case ENG_LIKE_POST: rc = run_like(e, op); break;
        default: rc = RUN_ERR;
        }
        if (rc == RUN_OK) {
            if (write && step_once(e->s_release) != SQLITE_DONE) failed = 1;
            continue;
        }
        /* The op failed: undo its writes, answer 500, and keep the batch only
         * if the transaction survived. A response arena that could not grow
         * is reset to the op's start so the 500 fits. */
        if (write) {
            step_once(e->s_rollback_to);
            step_once(e->s_release);
        }
        if (a->oom) {
            a->len = mark;
            a->oom = 0;
        }
        put_error(e, op);
        if (!in_txn(e)) failed = 1;
    }

    if (!failed && txn) {
        if (step_once(e->s_commit) != SQLITE_DONE) failed = 1;
    }
    if (failed) {
        if (in_txn(e)) step_once(e->s_rollback);
        /* Nothing in the batch is known to be committed: every response
         * becomes an error, health included. */
        a->len = 0;
        a->oom = 0;
        for (int i = 0; i < n; i++) put_error(e, &e->ops[i]);
    }
    e->view.out = a->buf;
    return failed;
}

/* ---------------------------------------------------------------- checkpoint */

int eng_wal_frames(eng *e) { return (int)e->view.wal_frames; }

int eng_checkpoint(eng *e, int *nlog, int *nckpt) {
    *nlog = 0;
    *nckpt = 0;
    return sqlite3_wal_checkpoint_v2(e->ck, NULL, SQLITE_CHECKPOINT_PASSIVE, nlog, nckpt);
}

void eng_get_stats(eng *e, eng_stats *out) {
    out->wal_frames = e->view.wal_frames;
    out->arena_cap = (int64_t)e->out.cap;
}
