package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func indexJobFixture(t *testing.T) (*DB, KnowledgeIndexRequest) {
	t.Helper()
	if os.Getenv("AUTOBID_TEST_DATABASE_URL") == "" {
		t.Fatal("index job tests require a real PostgreSQL database")
	}
	d := openTestDB(t)
	tenant := uniq("index-job")
	id, _, err := d.InsertKnowledgeDocument(context.Background(), &KnowledgeDocument{TenantID: tenant, Category: KnowledgeProductSpec, Title: "索引测试", RawText: "索引测试正文"}, []KnowledgeChunk{{Seq: 0, Content: "恢复时间：30秒"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = d.DeleteKnowledgeDocument(context.Background(), tenant, id)
		for _, table := range []string{"knowledge_index_events", "knowledge_indexes", "knowledge_catalogs"} {
			_, _ = d.ExecContext(context.Background(), "DELETE FROM "+table+" WHERE tenant_id=$1", tenant)
		}
	})
	return d, KnowledgeIndexRequest{TenantID: tenant, Model: "test@1", Preprocessing: "test-prep", Actor: "test-actor", Dimension: 2, RequestKey: "request", RequestHash: "hash", Deadline: time.Now().Add(time.Hour)}
}

func TestKnowledgeIndexGuard(t *testing.T) {
	d, req := indexJobFixture(t)
	ctx := context.Background()
	x, _, err := d.SubmitKnowledgeIndexBuild(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	for _, assignment := range []string{
		"model='foreign@2'", "dimension=3", "preprocessing='other'", "tenant_id='other'", "request_key='new-key'",
		"request_hash='new-hash'", "deadline_at=deadline_at+interval '1 second'", "max_attempts=4", "created_by='other'",
	} {
		t.Run(assignment, func(t *testing.T) {
			if _, err := d.ExecContext(ctx, `UPDATE knowledge_indexes SET `+assignment+` WHERE id=$1`, x.ID); err == nil {
				t.Fatalf("guard accepted %s", assignment)
			}
		})
	}
	if _, err = d.ExecContext(ctx, `UPDATE knowledge_indexes SET active=true WHERE id=$1`, x.ID); err == nil {
		t.Fatal("non-ready index became active")
	}
	if err = d.CancelKnowledgeIndexBuild(ctx, req.TenantID, x.ID, "cancel-actor"); err != nil {
		t.Fatal(err)
	}
	for _, terminal := range []string{"cancelled", "ready", "failed"} {
		t.Run("terminal-"+terminal, func(t *testing.T) {
			var current KnowledgeIndex
			if terminal == "cancelled" {
				current = x
			} else {
				r := req
				r.RequestKey = terminal
				current, _, err = d.SubmitKnowledgeIndexBuild(ctx, r)
				if err != nil {
					t.Fatal(err)
				}
				claimed, err := d.ClaimKnowledgeIndexBuildByID(ctx, req.TenantID, current.ID, "worker", time.Minute)
				if err != nil || claimed == nil {
					t.Fatal(err)
				}
				var buildErr error
				if terminal == "failed" {
					buildErr = errors.New("failure")
				}
				if err = d.FinishKnowledgeIndexBuild(ctx, *claimed, buildErr, ""); err != nil {
					t.Fatal(err)
				}
			}
			for _, next := range []string{"queued", "building", "ready", "failed", "cancelled"} {
				if next == terminal {
					continue
				}
				if _, err = d.ExecContext(ctx, `UPDATE knowledge_indexes SET status=$2 WHERE id=$1`, current.ID, next); err == nil {
					t.Fatalf("%s -> %s accepted", terminal, next)
				}
			}
		})
	}
	if _, err = d.ExecContext(ctx, `UPDATE knowledge_indexes SET updated_at='2000-01-01' WHERE id=$1`, x.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := d.GetKnowledgeIndex(ctx, req.TenantID, x.ID)
	if err != nil || stored.UpdatedAt.Before(time.Now().Add(-time.Minute)) {
		t.Fatal("updated_at not maintained by trigger", err)
	}
}

func TestConcurrentKnowledgeIndexSubmit(t *testing.T) {
	d, req := indexJobFixture(t)
	ctx := context.Background()
	for _, sameKey := range []bool{false, true} {
		t.Run(fmt.Sprint("same-key-", sameKey), func(t *testing.T) {
			var wg sync.WaitGroup
			var mu sync.Mutex
			created, replayed, active := 0, 0, 0
			var winner int64
			for i := 0; i < 12; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					r := req
					if !sameKey {
						r.RequestKey = fmt.Sprintf("key-%d", i)
					} else {
						r.RequestKey = "shared"
					}
					x, replay, err := d.SubmitKnowledgeIndexBuild(ctx, r)
					mu.Lock()
					defer mu.Unlock()
					switch {
					case err == nil:
						if replay {
							replayed++
						} else {
							created++
							winner = x.ID
						}
					case errors.Is(err, ErrIndexBuildActive):
						active++
					default:
						t.Errorf("unexpected submit error: %v", err)
					}
				}(i)
			}
			wg.Wait()
			if created != 1 || sameKey && replayed != 11 || !sameKey && active != 11 {
				t.Fatalf("created=%d replayed=%d active=%d", created, replayed, active)
			}
			var n int
			if err := d.QueryRowContext(ctx, `SELECT count(*) FROM knowledge_indexes WHERE tenant_id=$1 AND status IN ('queued','building')`, req.TenantID).Scan(&n); err != nil || n != 1 {
				t.Fatalf("active rows=%d %v", n, err)
			}
			if err := d.CancelKnowledgeIndexBuild(ctx, req.TenantID, winner, "test"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestKnowledgeIndexLeaseAndEpochReset(t *testing.T) {
	d, req := indexJobFixture(t)
	ctx := context.Background()
	x, _, err := d.SubmitKnowledgeIndexBuild(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := d.ClaimKnowledgeIndexBuildByID(ctx, req.TenantID, x.ID, "first", time.Minute)
	if err != nil || claimed == nil {
		t.Fatal(err)
	}
	chunks, err := d.IndexChunks(ctx, req.TenantID, 0, 32)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.PutIndexBatch(ctx, *claimed, chunks, [][]float64{{1, 1}}, 1, 0); err != nil {
		t.Fatal(err)
	}
	if _, err = d.ExecContext(ctx, `UPDATE knowledge_chunks SET content='changed' WHERE id=$1`, chunks[0].Chunk.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = d.ExecContext(ctx, `UPDATE knowledge_indexes SET lease_until=now()-interval '1 second' WHERE id=$1`, x.ID); err != nil {
		t.Fatal(err)
	}
	resumed, err := d.ClaimKnowledgeIndexBuildByID(ctx, req.TenantID, x.ID, "second", time.Minute)
	if err != nil || resumed == nil {
		t.Fatal(err)
	}
	if resumed.CursorChunkID != 0 || resumed.EmbeddedChunks != 0 || resumed.ReusedChunks != 0 || resumed.Epoch == x.Epoch || resumed.Attempts != 2 || !resumed.DeadlineAt.Equal(*x.DeadlineAt) {
		t.Fatalf("epoch reset: %+v", resumed)
	}
	var count int
	if err = d.QueryRowContext(ctx, `SELECT count(*) FROM knowledge_embeddings WHERE index_id=$1`, x.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("old snapshot vectors survived reset", err)
	}
	if err = d.PutIndexBatch(ctx, *claimed, chunks, [][]float64{{1, 1}}, 1, 0); !errors.Is(err, ErrLeaseLost) {
		t.Fatal("expired writer accepted", err)
	}
	if err = d.ReleaseKnowledgeIndexBuild(ctx, *claimed, time.Now(), "late"); !errors.Is(err, ErrLeaseLost) {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(resumed)
	for _, private := range []string{"lease_token", "lease_owner", "lease_until", "request_hash", "cursor_chunk_id"} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("private field %s in JSON", private)
		}
	}
}

func TestKnowledgeIndexAuditRetentionAndConfiguredDedup(t *testing.T) {
	d, req := indexJobFixture(t)
	ctx := context.Background()
	for _, detail := range []map[string]any{{"embedding": "one@1", "embedding_dimension": 2, "reranker": "rank@1"}, {"reranker": "rank@1", "embedding_dimension": 2, "embedding": "one@1"}, {"embedding": "two@2", "embedding_dimension": 2, "reranker": "rank@1"}} {
		if err := d.NoteConfiguredModels(ctx, req.TenantID, detail, "server"); err != nil {
			t.Fatal(err)
		}
	}
	events, err := d.KnowledgeIndexEvents(ctx, req.TenantID, 50)
	if err != nil || len(events) != 2 || events[0].IndexID != 0 {
		t.Fatalf("dedup: %+v %v", events, err)
	}
	x, _, err := d.SubmitKnowledgeIndexBuild(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.DeleteKnowledgeIndex(ctx, req.TenantID, x.ID, "test"); !errors.Is(err, ErrRetrievalConflict) {
		t.Fatal("deleted active build", err)
	}
	if err = d.CancelKnowledgeIndexBuild(ctx, req.TenantID, x.ID, "cancel-actor"); err != nil {
		t.Fatal(err)
	}
	if err = d.DeleteKnowledgeIndex(ctx, req.TenantID, x.ID, "delete-actor"); err != nil {
		t.Fatal(err)
	}
	events, err = d.KnowledgeIndexEvents(ctx, req.TenantID, 200)
	if err != nil || len(events) != 5 || events[0].Event != "deleted" || events[1].Event != "build_cancelled" || events[2].Event != "build_requested" || events[0].CreatedBy != "delete-actor" {
		t.Fatalf("audit lost: %+v %v", events, err)
	}
	if _, err = d.GetKnowledgeIndex(ctx, "foreign-tenant", x.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("tenant isolation", err)
	}
}

func TestKnowledgeIndexBuildTransactionRollback(t *testing.T) {
	d, req := indexJobFixture(t)
	ctx := context.Background()
	rollback := errors.New("rollback index build")
	var id int64
	err := d.Transaction(ctx, func(tx *DB) error {
		x, _, err := tx.SubmitKnowledgeIndexBuild(ctx, req)
		if err != nil {
			return err
		}
		id = x.ID
		claimed, err := tx.ClaimKnowledgeIndexBuildByID(ctx, req.TenantID, id, "scoped", time.Minute)
		if err != nil {
			return err
		}
		if claimed == nil {
			return errors.New("scoped claim missing")
		}
		chunks, err := tx.IndexChunks(ctx, req.TenantID, 0, 32)
		if err != nil {
			return err
		}
		if err = tx.PutIndexBatch(ctx, *claimed, chunks, [][]float64{{1, 1}}, 1, 0); err != nil {
			return err
		}
		if err = tx.FinishKnowledgeIndexBuild(ctx, *claimed, nil, ""); err != nil {
			return err
		}
		if err = tx.ActivateKnowledgeIndex(ctx, req.TenantID, id, "scoped"); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	if _, err = d.GetKnowledgeIndex(ctx, req.TenantID, id); !errors.Is(err, ErrNotFound) {
		t.Fatal("index escaped outer transaction", err)
	}
	events, err := d.KnowledgeIndexEvents(ctx, req.TenantID, 200)
	if err != nil || len(events) != 0 {
		t.Fatalf("audit escaped rollback: %+v %v", events, err)
	}
}

func TestPutIndexBatchWaitsForConcurrentChunkDeletion(t *testing.T) {
	d, req := indexJobFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	x, err := d.StartKnowledgeIndex(ctx, req.TenantID, req.Model, req.Preprocessing, req.Actor, req.Dimension)
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := d.IndexChunks(ctx, req.TenantID, 0, 32)
	if err != nil || len(chunks) != 1 {
		t.Fatalf("chunks: %v %v", chunks, err)
	}
	commitDelete := make(chan struct{})
	releaseDelete := sync.OnceFunc(func() { close(commitDelete) })
	var wg sync.WaitGroup
	defer func() {
		releaseDelete()
		cancel()
		wg.Wait()
	}()
	deletePID, writePID := make(chan int, 1), make(chan int, 1)
	deleteDone, writeDone := make(chan error, 1), make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		deleteDone <- d.Transaction(ctx, func(tx *DB) error {
			var pid int
			if err := tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM knowledge_chunks WHERE tenant_id=$1 AND id=$2`, req.TenantID, chunks[0].Chunk.ID); err != nil {
				return err
			}
			deletePID <- pid
			select {
			case <-commitDelete:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	var deleter int
	select {
	case deleter = <-deletePID:
	case err := <-deleteDone:
		t.Fatalf("delete did not reach its commit barrier: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		writeDone <- d.Transaction(ctx, func(tx *DB) error {
			var pid int
			if err := tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				return err
			}
			writePID <- pid
			return tx.PutIndexBatch(ctx, x, chunks, [][]float64{{1, 1}}, 1, 0)
		})
	}()
	var writer int
	select {
	case writer = <-writePID:
	case err := <-writeDone:
		t.Fatalf("batch did not start: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		var blocked bool
		if err := d.QueryRowContext(ctx, `SELECT $1::int=ANY(pg_blocking_pids($2::int))`, deleter, writer).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err := <-writeDone:
			t.Fatalf("batch returned before deletion committed: %v", err)
		case <-ctx.Done():
			t.Fatal("batch never waited on the deleting transaction")
		case <-tick.C:
		}
	}
	releaseDelete()
	if err = <-deleteDone; err != nil {
		t.Fatal(err)
	}
	if err = <-writeDone; err != nil {
		t.Fatalf("deleted source chunk failed batch: %v", err)
	}
	if err = d.FinishKnowledgeIndexBuild(ctx, x, nil, ""); err != nil {
		t.Fatal(err)
	}
	stored, err := d.GetKnowledgeIndex(ctx, req.TenantID, x.ID)
	if err != nil || stored.Status != "ready" || stored.CursorChunkID != chunks[0].Chunk.ID || stored.EmbeddedChunks != 1 {
		t.Fatalf("stale snapshot did not finish: %+v %v", stored, err)
	}
	var count int
	if err = d.QueryRowContext(ctx, `SELECT count(*) FROM knowledge_embeddings WHERE tenant_id=$1 AND index_id=$2`, req.TenantID, x.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("deleted chunk retained an embedding: %d %v", count, err)
	}
	var activation *IndexActivationError
	if err = d.ActivateKnowledgeIndex(ctx, req.TenantID, x.ID, "test"); !errors.As(err, &activation) || activation.Code != "documents_changed" {
		t.Fatalf("stale index activation: %v", err)
	}
}

func TestReusableEmbeddingsSelectsLatestMatchingFingerprint(t *testing.T) {
	d, req := indexJobFixture(t)
	ctx := context.Background()
	chunks, err := d.IndexChunks(ctx, req.TenantID, 0, 32)
	if err != nil || len(chunks) != 1 {
		t.Fatalf("chunks: %v %v", chunks, err)
	}
	chunkID := chunks[0].Chunk.ID
	fingerprint := ChunkFingerprint(chunks[0].Document, chunks[0].Chunk)
	build := func(status, model, prep string, vector []float64) KnowledgeIndex {
		t.Helper()
		x, err := d.StartKnowledgeIndex(ctx, req.TenantID, model, prep, "test", len(vector))
		if err != nil {
			t.Fatal(err)
		}
		current, err := d.IndexChunks(ctx, req.TenantID, 0, 32)
		if err != nil {
			t.Fatal(err)
		}
		if err = d.PutIndexBatch(ctx, x, current, [][]float64{vector}, 1, 0); err != nil {
			t.Fatal(err)
		}
		switch status {
		case "ready":
			err = d.FinishKnowledgeIndexBuild(ctx, x, nil, "")
		case "failed":
			err = d.FinishKnowledgeIndexBuild(ctx, x, errors.New("partial failure"), "MODEL_ERROR")
		case "cancelled":
			err = d.CancelKnowledgeIndexBuild(ctx, req.TenantID, x.ID, "test")
		}
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	// The lookup does not depend on the audit pointer or the target's status.
	target := KnowledgeIndex{TenantID: req.TenantID, Model: req.Model, Dimension: req.Dimension, Preprocessing: req.Preprocessing}
	assertVector := func(want float64) {
		t.Helper()
		found, err := d.ReusableEmbeddings(ctx, target, map[int64]string{chunkID: fingerprint})
		if err != nil || len(found) != 1 || found[chunkID].ContentHash != fingerprint || len(found[chunkID].Vector) != 2 || found[chunkID].Vector[0] != want {
			t.Fatalf("reusable = %+v, want vector %v: %v", found, want, err)
		}
	}
	build("ready", req.Model, req.Preprocessing, []float64{1, 1})
	assertVector(1)
	build("failed", req.Model, req.Preprocessing, []float64{2, 2})
	assertVector(2)
	build("cancelled", req.Model, req.Preprocessing, []float64{3, 3})
	assertVector(3)
	if _, err = d.ExecContext(ctx, `UPDATE knowledge_chunks SET content='changed fingerprint' WHERE tenant_id=$1 AND id=$2`, req.TenantID, chunkID); err != nil {
		t.Fatal(err)
	}
	build("ready", req.Model, req.Preprocessing, []float64{4, 4})
	if _, err = d.ExecContext(ctx, `UPDATE knowledge_chunks SET content=$3 WHERE tenant_id=$1 AND id=$2`, req.TenantID, chunkID, chunks[0].Chunk.Content); err != nil {
		t.Fatal(err)
	}
	build("ready", "different-model@2", req.Preprocessing, []float64{5, 5})
	build("ready", req.Model, req.Preprocessing, []float64{6, 6, 6})
	build("ready", req.Model, "different-preprocessing", []float64{7, 7})
	current := build("ready", req.Model, req.Preprocessing, []float64{8, 8})
	target.ID = current.ID
	assertVector(3)
	building := build("building", req.Model, req.Preprocessing, []float64{9, 9})
	assertVector(3)
	if err = d.ReleaseKnowledgeIndexBuild(ctx, building, time.Now().Add(time.Hour), "retry later"); err != nil {
		t.Fatal(err)
	}
	assertVector(3)
	if found, err := d.ReusableEmbeddings(ctx, target, map[int64]string{chunkID: "different-fingerprint"}); err != nil || len(found) != 0 {
		t.Fatalf("mismatched fingerprint reused: %+v %v", found, err)
	}
	target.TenantID = "foreign-" + req.TenantID
	if found, err := d.ReusableEmbeddings(ctx, target, map[int64]string{chunkID: fingerprint}); err != nil || len(found) != 0 {
		t.Fatalf("foreign tenant reused: %+v %v", found, err)
	}
}
