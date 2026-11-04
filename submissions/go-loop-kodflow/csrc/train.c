/*
 * PGO training run for the batch engine. build.sh links it against the
 * instrumented engine, runs it once on a throwaway database built from the
 * challenge's schema, and compiles the engine again with the profile it
 * left. It is never part of the server.
 *
 *   train <db-path> <schema.sql>
 *
 * The database is seeded with synthetic users, posts and likes (no file of
 * the challenge's seed is needed), then the engine runs batches in the load
 * test's proportions: per user loop one feed, one post read, a like 15 % of
 * the time and a create 2 % of the time, with a health check now and then.
 */
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "engine.h"
#include "sqlite3.h"

#define USERS 2000
#define POSTS 20000
#define LIKES 60000
#define LOOPS 12000

static uint64_t rng = 0x9e3779b97f4a7c15ull;

static uint64_t next(void) {
    rng ^= rng << 13;
    rng ^= rng >> 7;
    rng ^= rng << 17;
    return rng;
}

static char *slurp(const char *path) {
    FILE *f = fopen(path, "rb");
    if (!f) return NULL;
    fseek(f, 0, SEEK_END);
    long n = ftell(f);
    fseek(f, 0, SEEK_SET);
    char *buf = malloc((size_t)n + 1);
    if (buf && fread(buf, 1, (size_t)n, f) != (size_t)n) {
        free(buf);
        buf = NULL;
    }
    if (buf) buf[n] = 0;
    fclose(f);
    return buf;
}

/* body writes a post body of 30 to 160 bytes; one in twenty carries a byte
 * the JSON writer must escape, one in ten some UTF-8. */
static int body(char *out) {
    static const char words[] = "lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor ";
    int n = 30 + (int)(next() % 130);
    for (int i = 0; i < n; i++) out[i] = words[(i + (int)(next() % 7)) % (sizeof(words) - 1)];
    uint64_t r = next() % 20;
    if (r == 0) out[n / 2] = '"';
    if (r == 1) out[n / 3] = '\n';
    if (r == 2 || r == 3) memcpy(out + n - 3, "\xc3\xa9!", 3);
    return n;
}

static int seed(const char *path, const char *schema) {
    sqlite3 *db;
    if (sqlite3_open(path, &db) != SQLITE_OK) return 1;
    char *err = NULL;
    if (sqlite3_exec(db, schema, NULL, NULL, &err) != SQLITE_OK ||
        sqlite3_exec(db, "PRAGMA journal_mode=WAL; BEGIN", NULL, NULL, &err) != SQLITE_OK) {
        fprintf(stderr, "train: schema: %s\n", err ? err : "?");
        return 1;
    }
    sqlite3_stmt *u, *p, *l;
    sqlite3_prepare_v2(db, "INSERT INTO users (id, username) VALUES (?1, ?2)", -1, &u, NULL);
    sqlite3_prepare_v2(db, "INSERT INTO posts (id, user_id, body, created_at) VALUES (?1, ?2, ?3, ?4)", -1, &p, NULL);
    sqlite3_prepare_v2(db, "INSERT OR IGNORE INTO likes (user_id, post_id) VALUES (?1, ?2)", -1, &l, NULL);
    char text[200];
    for (int i = 1; i <= USERS; i++) {
        snprintf(text, sizeof(text), "user_%d", i);
        sqlite3_bind_int(u, 1, i);
        sqlite3_bind_text(u, 2, text, -1, SQLITE_TRANSIENT);
        sqlite3_step(u);
        sqlite3_reset(u);
    }
    for (int i = 1; i <= POSTS; i++) {
        char at[32];
        snprintf(at, sizeof(at), "2026-01-%02dT%02d:%02d:%02d.%03dZ", 1 + i / 86400 % 28, i / 3600 % 24, i / 60 % 60,
                 i % 60, (int)(next() % 1000));
        sqlite3_bind_int(p, 1, i);
        sqlite3_bind_int(p, 2, 1 + (int)(next() % USERS));
        sqlite3_bind_text(p, 3, text, body(text), SQLITE_TRANSIENT);
        sqlite3_bind_text(p, 4, at, -1, SQLITE_TRANSIENT);
        sqlite3_step(p);
        sqlite3_reset(p);
    }
    for (int i = 0; i < LIKES; i++) {
        /* Likes lean on the newest posts, as the feed's readers do. */
        int post = next() % 4 ? POSTS - (int)(next() % 200) : 1 + (int)(next() % POSTS);
        sqlite3_bind_int(l, 1, 1 + (int)(next() % USERS));
        sqlite3_bind_int(l, 2, post);
        sqlite3_step(l);
        sqlite3_reset(l);
    }
    sqlite3_finalize(u);
    sqlite3_finalize(p);
    sqlite3_finalize(l);
    if (sqlite3_exec(db, "COMMIT", NULL, NULL, &err) != SQLITE_OK) {
        fprintf(stderr, "train: seed: %s\n", err ? err : "?");
        return 1;
    }
    sqlite3_close(db);
    return 0;
}

int main(int argc, char **argv) {
    if (argc != 3) {
        fprintf(stderr, "usage: %s <db-path> <schema.sql>\n", argv[0]);
        return 2;
    }
    char *schema = slurp(argv[2]);
    if (!schema) {
        fprintf(stderr, "train: cannot read %s\n", argv[2]);
        return 1;
    }
    sqlite3_initialize();
    if (seed(argv[1], schema)) return 1;

    char err[256];
    eng *e = eng_open(argv[1], 64, err, sizeof(err));
    if (!e) {
        fprintf(stderr, "train: eng_open: %s\n", err);
        return 1;
    }
    static char in[1 << 16];
    int mark = 0, posts = POSTS;
    long ops = 0;
    for (int it = 0; it < LOOPS; it++) {
        /* A batch holds what a few user loops asked for at once. */
        int n = 0, pos = 0, users = 1 + (int)(next() % 4);
        eng_op *op = eng_ops(e);
        for (int k = 0; k < users && n < 60; k++) {
            memset(&op[n], 0, sizeof(op[n]));
            op[n++].kind = next() % 50 ? ENG_FEED : ENG_HEALTH;
            memset(&op[n], 0, sizeof(op[n]));
            op[n].kind = ENG_GET_POST;
            op[n++].arg1 = next() % 100 ? posts - (int64_t)(next() % 20) : 1 + (int64_t)(next() % (2 * posts));
            if (next() % 100 < 15) {
                memset(&op[n], 0, sizeof(op[n]));
                op[n].kind = ENG_LIKE_POST;
                op[n].arg1 = posts - (int64_t)(next() % 20);
                op[n++].arg2 = 1 + (int64_t)(next() % USERS);
            }
            if (next() % 100 < 2) {
                memset(&op[n], 0, sizeof(op[n]));
                op[n].kind = ENG_CREATE_POST;
                op[n].arg1 = 1 + (int64_t)(next() % USERS);
                op[n].text_off = pos;
                op[n].text_len = body(in + pos);
                pos += op[n].text_len;
                op[n].user_off = pos;
                op[n].user_len = snprintf(in + pos, 32, "user_%d", (int)op[n].arg1);
                pos += op[n].user_len;
                n++;
                posts++;
            }
        }
        eng_exec(e, n, in, it);
        ops += n;
        int frames = eng_wal_frames(e);
        if (frames < mark) mark = 0;
        if (frames - mark >= 4096) {
            int nlog, nckpt;
            if (eng_checkpoint(e, &nlog, &nckpt) == 0) mark = nckpt;
        }
    }
    eng_close(e);
    printf("train: %ld ops in %d batches\n", ops, LOOPS);
    return 0;
}
