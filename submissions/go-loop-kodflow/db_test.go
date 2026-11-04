package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kodflow/twelve-dollar-go-loop/internal/api"
)

// freshSeed copies seed/feed.db into a temporary directory, or skips.
func freshSeed(t testing.TB) string {
	t.Helper()
	src, err := os.Open("../../seed/feed.db")
	if err != nil {
		t.Skip("seed/feed.db not built: bash seed/make-seed.sh")
	}
	defer func() { _ = src.Close() }()
	path := filepath.Join(t.TempDir(), "feed.db")
	dst, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// job builds the job for op in b, copying a create's text and user into b.In
// as the handler does.
func job(b *api.Batch, op api.Op) api.Job {
	j := api.Job{Kind: op.Kind, PostID: op.PostID, UserID: op.UserID}
	if op.Kind == api.KindCreatePost {
		j.TextOff, j.TextLen = int32(len(b.In)), int32(len(op.Text))
		b.In = append(b.In, op.Text...)
		j.UserOff, j.UserLen = int32(len(b.In)), int32(len(op.User))
		b.In = append(b.In, op.User...)
	}
	return j
}

// runBatch executes ops as one batch and returns each answer.
func runBatch(db *engine, ops ...api.Op) (status []int, body []string) {
	b := &api.Batch{}
	for _, op := range ops {
		b.Jobs = append(b.Jobs, job(b, op))
	}
	db.ExecBatch(b)
	for _, j := range b.Jobs {
		status, body = append(status, j.Status), append(body, string(j.Body))
	}
	return status, body
}

func TestBatchAgainstSeed(t *testing.T) {
	db, err := openEngine(freshSeed(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.close()
	if db.info.JournalMode != "wal" || db.info.LockingMode != "normal" || db.info.Synchronous != "1" {
		t.Fatalf("pragmas %+v", db.info)
	}

	golden, err := os.ReadFile("../../test/golden/feed.json")
	if err != nil {
		t.Fatal(err)
	}
	status, body := runBatch(db, api.Op{Kind: api.KindFeed})
	if status[0] != 200 || body[0] != string(bytes.TrimRight(golden, "\n")) {
		t.Fatalf("feed %d differs from golden", status[0])
	}

	// One batch, one transaction: each job gets its own answer, and a
	// failing write must not undo its neighbours.
	status, body = runBatch(db,
		api.Op{Kind: api.KindCreatePost, UserID: 1, Text: "hello \"world\"\n", User: []byte("golden_ember_1")},
		api.Op{Kind: api.KindLikePost, PostID: 1, UserID: 1},
		api.Op{Kind: api.KindLikePost, PostID: 999999999, UserID: 1},
		api.Op{Kind: api.KindGetPost, PostID: 999999999},
		api.Op{Kind: api.KindHealth},
	)
	want := []int{201, 201, 404, 404, 200}
	for i := range want {
		if status[i] != want[i] {
			t.Fatalf("op %d: %d %s", i, status[i], body[i])
		}
	}
	var created struct {
		Post struct {
			ID     int64  `json:"id"`
			Body   string `json:"body"`
			Author string `json:"author"`
		} `json:"post"`
	}
	if err := json.Unmarshal([]byte(body[0]), &created); err != nil {
		t.Fatalf("create body %s: %v", body[0], err)
	}
	if created.Post.Body != "hello \"world\"\n" || created.Post.Author != "golden_ember_1" {
		t.Fatalf("created %+v", created)
	}

	status, body = runBatch(db, api.Op{Kind: api.KindLikePost, PostID: 1, UserID: 1})
	if status[0] != 200 || body[0] != `{"liked":true,"already_liked":true,"post_id":1}` {
		t.Fatalf("repeat like %d %s", status[0], body[0])
	}
	status, body = runBatch(db, api.Op{Kind: api.KindFeed})
	var feed struct {
		Posts []struct {
			ID int64 `json:"id"`
		} `json:"posts"`
	}
	if err := json.Unmarshal([]byte(body[0]), &feed); err != nil || status[0] != 200 || len(feed.Posts) != 20 || feed.Posts[0].ID != created.Post.ID {
		t.Fatalf("feed after create: %d %v %s", status[0], err, body[0])
	}
	if snap := db.snapshot(); snap.FailedBatches != 0 || snap.Ops < 8 {
		t.Fatalf("stats %+v", snap)
	}
}

// TestBatchPastMaxBatch runs more jobs than one cgo call holds, and from two
// goroutines at once as two loops would: every job is answered, and its body
// is its own although the engine's arena is rewritten between chunks.
func TestBatchPastMaxBatch(t *testing.T) {
	db, err := openEngine(freshSeed(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.close()
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			ops := make([]api.Op, maxBatch+37)
			for i := range ops {
				ops[i] = api.Op{Kind: api.KindGetPost, PostID: int64(i%50 + 1)}
			}
			status, body := runBatch(db, ops...)
			for i := range ops {
				var got struct {
					Post struct {
						ID int64 `json:"id"`
					} `json:"post"`
				}
				if status[i] != 200 || json.Unmarshal([]byte(body[i]), &got) != nil || got.Post.ID != ops[i].PostID {
					t.Errorf("job %d: %d %s", i, status[i], body[i])
					return
				}
			}
		})
	}
	wg.Wait()
}

// TestCheckpointBesideWrites keeps committing posts while the checkpointer
// runs its passes: every create is answered 201, the WAL is restarted once a
// checkpoint has copied it whole, and every post is there after a reopen.
func TestCheckpointBesideWrites(t *testing.T) {
	path := freshSeed(t)
	db, err := openEngine(path)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	create := func(i int) {
		status, body := runBatch(db,
			api.Op{Kind: api.KindCreatePost, UserID: 1, Text: "checkpoint", User: []byte("golden_ember_1")},
			api.Op{Kind: api.KindLikePost, PostID: int64(i%500 + 1), UserID: int64(i%900 + 2)},
		)
		if status[0] != 201 {
			t.Fatalf("create %d: %d %s", i, status[0], body[0])
		}
		var created struct {
			Post struct {
				ID int64 `json:"id"`
			} `json:"post"`
		}
		if err := json.Unmarshal([]byte(body[0]), &created); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, created.Post.ID)
	}
	i := 0
	for ; db.snapshot().Checkpoints < 3; i++ {
		if i > 200000 {
			t.Fatalf("no third checkpoint after %d creates: %+v", i, db.snapshot())
		}
		create(i)
	}
	snap := db.snapshot()
	if snap.CkptErrors != 0 || snap.FailedBatches != 0 || snap.CkptLastWritten < checkpointFrames {
		t.Fatalf("stats %+v", snap)
	}
	// Three checkpoints of 1,024 frames each: a WAL that was never restarted
	// would hold all of them. Only a write after a whole checkpoint restarts
	// it, and the third may still be running beside the writes that counted
	// it, so keep writing, paced, until the WAL is restarted or time is up.
	deadline := time.Now().Add(10 * time.Second)
	for frames := db.walFrames(); frames >= 2*checkpointFrames; frames = db.walFrames() {
		if time.Now().After(deadline) {
			t.Fatalf("WAL at %d frames after %d checkpoints: never restarted", frames, db.snapshot().Checkpoints)
		}
		create(i)
		i++
		time.Sleep(time.Millisecond)
	}
	db.close()

	db, err = openEngine(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.close()
	for _, id := range []int64{ids[0], ids[len(ids)/2], ids[len(ids)-1]} {
		if status, body := runBatch(db, api.Op{Kind: api.KindGetPost, PostID: id}); status[0] != 200 {
			t.Fatalf("post %d after reopen: %d %s", id, status[0], body[0])
		}
	}
}

// BenchmarkExecBatch runs one-job batches as a loop does at the bench's load
// (1.1-1.4 requests per batch): the cgo glue around the engine is a visible
// share of a health check's or a post read's cost.
func BenchmarkExecBatch(b *testing.B) {
	for _, bc := range []struct {
		name string
		op   api.Op
	}{
		{"health", api.Op{Kind: api.KindHealth}},
		{"get", api.Op{Kind: api.KindGetPost, PostID: 499990}},
		{"feed", api.Op{Kind: api.KindFeed}},
	} {
		b.Run(bc.name, func(b *testing.B) {
			db, err := openEngine(freshSeed(b))
			if err != nil {
				b.Fatal(err)
			}
			defer db.close()
			batch := &api.Batch{}
			for b.Loop() {
				batch.Jobs = append(batch.Jobs[:0], job(batch, bc.op))
				db.ExecBatch(batch)
				if batch.Jobs[0].Status != 200 {
					b.Fatalf("status %d", batch.Jobs[0].Status)
				}
			}
		})
	}
}

// TestBatchWideCommit commits one batch whose WAL frames pass 128 KiB, as a
// full batch of likes on posts far apart does: SQLite's unix VFS writes at
// most 0x1ffff bytes a call, and a gathered run past that came back as
// SQLITE_FULL at COMMIT, failing the whole batch (bench/BENCH.md, S13).
func TestBatchWideCommit(t *testing.T) {
	db, err := openEngine(freshSeed(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.close()
	ops := make([]api.Op, maxBatch)
	for i := range ops {
		ops[i] = api.Op{Kind: api.KindLikePost, PostID: int64(i)*977 + 1, UserID: int64(i%20000 + 1)}
	}
	status, body := runBatch(db, ops...)
	for i := range ops {
		if status[i] != 200 && status[i] != 201 {
			t.Fatalf("job %d: %d %s", i, status[i], body[i])
		}
	}
	if snap := db.snapshot(); snap.FailedBatches != 0 {
		t.Fatalf("failed batches %d", snap.FailedBatches)
	}
}
