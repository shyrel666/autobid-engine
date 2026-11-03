package db

// 集成测试：需要真实 PostgreSQL。设置 AUTOBID_TEST_DATABASE_URL 后运行，未设置时跳过。
// 每个测试创建独立任务与唯一招标编号，不清库，可在共享测试库上重复执行。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var (
	testDBOnce sync.Once
	testDB     *DB
	testDBErr  error
	seq        atomic.Int64
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	dsn := os.Getenv("AUTOBID_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AUTOBID_TEST_DATABASE_URL not set")
	}
	testDBOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		testDB, testDBErr = Open(ctx, dsn)
		if testDBErr == nil {
			testDBErr = testDB.Migrate(ctx)
		}
	})
	if testDBErr != nil {
		t.Fatalf("open test db: %v", testDBErr)
	}
	return testDB
}

func uniq(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), seq.Add(1))
}

type fixture struct {
	d      *DB
	task   *Task
	es     *ExplorationStore
	as     *AssetStore
	origin int64
}

func newFixture(t *testing.T, scopes ...TaskScope) *fixture {
	t.Helper()
	d := openTestDB(t)
	ctx := context.Background()
	// 通用夹具沿用旧基线；D3 强制门禁由 d3_integration_test 单独覆盖。
	task, err := d.CreateTask(ctx, CreateTaskInput{Name: uniq("task"), Goal: "核验招标文件合规性", Scopes: scopes, LegacyWriting: true})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	es := NewExplorationStore(d, task.ExplorationID)
	origin, err := es.OriginFactID(ctx)
	if err != nil || origin == 0 {
		t.Fatalf("origin fact: id=%d err=%v", origin, err)
	}
	return &fixture{d: d, task: task, es: es, as: NewAssetStore(d, task.ID), origin: origin}
}

func (f *fixture) intent(t *testing.T, parents ...int64) int64 {
	t.Helper()
	id, err := f.es.AddIntentTx(context.Background(), AddIntentInput{Payload: map[string]any{"summary": "核对资质要求"}, Parents: parents})
	if err != nil {
		t.Fatalf("AddIntentTx: %v", err)
	}
	return id
}

func (f *fixture) edges(t *testing.T) map[Edge]bool {
	t.Helper()
	es, err := f.es.Edges(context.Background(), 1000)
	if err != nil {
		t.Fatalf("Edges: %v", err)
	}
	out := map[Edge]bool{}
	for _, e := range es {
		out[e] = true
	}
	return out
}

func (f *fixture) countNodes(t *testing.T, kind string) int {
	t.Helper()
	var n int
	if err := f.d.QueryRow(`SELECT COUNT(*) FROM exploration_nodes WHERE exploration_id = $1 AND kind = $2`,
		f.es.ID(), kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCreateTaskSeedsGraph(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	goal, err := f.es.AddGoalTx(ctx, map[string]any{"summary": "A 标段无废标风险"}, nil)
	if err != nil {
		t.Fatalf("AddGoalTx: %v", err)
	}
	intent := f.intent(t)
	e := f.edges(t)
	if !e[Edge{f.origin, RelSpawns, goal}] || !e[Edge{f.origin, RelDerivedFrom, intent}] {
		t.Fatalf("missing seed edges: %v", e)
	}
	n, err := f.es.GetNode(ctx, intent)
	if err != nil || n.Priority != DefaultIntentPriority || n.State != StateIntentOpen {
		t.Fatalf("intent node = %+v, err=%v", n, err)
	}
}

func TestLeaseClaimRenewFenceAndReap(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.intent(t)

	epoch, ok, err := f.es.ClaimIntentWithLease(ctx, id, "work#1", time.Minute)
	if err != nil || !ok || epoch != 1 {
		t.Fatalf("first claim: epoch=%d ok=%v err=%v", epoch, ok, err)
	}
	if _, ok, _ := f.es.ClaimIntentWithLease(ctx, id, "work#2", time.Minute); ok {
		t.Fatal("second claim on running intent must fail")
	}
	if ok, err := f.es.RenewIntentLease(ctx, id, "work#1", epoch, time.Minute); !ok || err != nil {
		t.Fatalf("renew by owner: ok=%v err=%v", ok, err)
	}
	if ok, _ := f.es.RenewIntentLease(ctx, id, "work#2", epoch, time.Minute); ok {
		t.Fatal("renew by non-owner must fail")
	}

	// 模拟 work#1 失联：把租约改短后回收。
	if _, err := f.d.Exec(`UPDATE exploration_nodes SET lease_until = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if n, err := f.es.ReapExpiredRunningIntents(ctx); err != nil || n != 1 {
		t.Fatalf("reap: n=%d err=%v", n, err)
	}
	epoch2, ok, err := f.es.ClaimIntentWithLease(ctx, id, "work#2", time.Minute)
	if err != nil || !ok || epoch2 != epoch+1 {
		t.Fatalf("reclaim: epoch=%d ok=%v err=%v", epoch2, ok, err)
	}

	// 旧 Worker 迟到的写回与续租必须被 fencing 拒绝。
	if ok, _ := f.es.CompareAndSetIntentStateFenced(ctx, id, StateIntentRunning, StateIntentDone, epoch); ok {
		t.Fatal("stale epoch CAS must fail")
	}
	if ok, _ := f.es.RenewIntentLease(ctx, id, "work#1", epoch, time.Minute); ok {
		t.Fatal("stale owner renew must fail")
	}
	if ok, err := f.es.CompareAndSetIntentStateFenced(ctx, id, StateIntentRunning, StateIntentDone, epoch2); !ok || err != nil {
		t.Fatalf("current epoch CAS: ok=%v err=%v", ok, err)
	}
	n, _ := f.es.GetNode(ctx, id)
	if n.State != StateIntentDone || n.CompletedAt == nil || n.LeaseUntil != nil {
		t.Fatalf("after done: %+v", n)
	}
}

func TestConcurrentClaimHasSingleWinner(t *testing.T) {
	f := newFixture(t)
	id := f.intent(t)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, ok, err := f.es.ClaimIntentWithLease(context.Background(), id, fmt.Sprintf("work#%d", i), time.Minute)
			if err != nil {
				t.Errorf("claim: %v", err)
			}
			if ok {
				wins.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("winners = %d, want 1", wins.Load())
	}
}

func TestGlobalReapReturnsExplorations(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.intent(t)
	if _, ok, _ := f.es.ClaimIntentWithLease(ctx, id, "work#1", time.Millisecond); !ok {
		t.Fatal("claim failed")
	}
	time.Sleep(20 * time.Millisecond)
	ids, err := f.d.ReapExpiredIntents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range ids {
		found = found || x == f.es.ID()
	}
	if !found {
		t.Fatalf("exploration %d not in reaped %v", f.es.ID(), ids)
	}
}

func TestBlockIntentFenced(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.intent(t)
	epoch, _, _ := f.es.ClaimIntentWithLease(ctx, id, "work#1", time.Minute)
	if ok, _ := f.es.BlockIntentFenced(ctx, id, epoch+1, BlockedReasonQuotaExhausted); ok {
		t.Fatal("block with wrong epoch must fail")
	}
	if ok, err := f.es.BlockIntentFenced(ctx, id, epoch, BlockedReasonQuotaExhausted); !ok || err != nil {
		t.Fatalf("block: ok=%v err=%v", ok, err)
	}
	if n, err := f.es.ReopenIntentsByBlockedReason(ctx, BlockedReasonQuotaExhausted); n != 1 || err != nil {
		t.Fatalf("reopen by reason: n=%d err=%v", n, err)
	}
}

func TestRecordFactWithYieldsTxIsAtomic(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	intent := f.intent(t)
	clause, _, err := f.as.UpsertAsset(ctx, &Asset{Type: AssetClause, TenderNo: uniq("ZB"), Code: "3.2.1", Title: "工期要求"}, 0)
	if err != nil {
		t.Fatalf("UpsertAsset: %v", err)
	}

	before := f.countNodes(t, KindFact)
	if _, err := f.es.RecordFactWithYieldsTx(ctx, f.origin, map[string]any{"summary": "x"}, 0, "work#1", nil); err == nil {
		t.Fatal("non-intent parent must be rejected")
	}
	if _, err := f.es.RecordFactWithYieldsTx(ctx, intent, map[string]any{"summary": "x"}, 0, "work#1", []int64{clause, -1, 1 << 60}); err == nil {
		t.Fatal("unknown anchor asset must fail")
	}
	if got := f.countNodes(t, KindFact); got != before {
		t.Fatalf("failed writes left %d orphan facts", got-before)
	}

	fact, err := f.es.RecordFactWithYieldsTx(ctx, intent, map[string]any{"summary": "工期 180 天满足要求"}, 0, "work#1", []int64{clause})
	if err != nil {
		t.Fatalf("RecordFactWithYieldsTx: %v", err)
	}
	if !f.edges(t)[Edge{intent, RelYields, fact}] {
		t.Fatal("missing yields edge")
	}
	n, _ := f.es.GetNode(ctx, fact)
	if len(n.Anchors) != 1 || n.Anchors[0] != clause || n.Owner != "work#1" {
		t.Fatalf("fact node = %+v", n)
	}
}

func TestProveGoalTx(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	g1, _ := f.es.AddGoalTx(ctx, map[string]any{"summary": "g1"}, nil)
	g2, _ := f.es.AddGoalTx(ctx, map[string]any{"summary": "g2"}, nil)
	intent := f.intent(t)
	fact, _ := f.es.RecordFactWithYieldsTx(ctx, intent, map[string]any{"summary": "ev"}, 0, "w", nil)

	if _, _, err := f.es.ProveGoalTx(ctx, g1, intent); !errors.Is(err, ErrInvalidEdge) {
		t.Fatalf("intent as evidence: err=%v, want ErrInvalidEdge", err)
	}
	if n, _ := f.es.GetNode(ctx, g1); n.State != StateGoalOpen {
		t.Fatalf("goal flipped despite rollback: %s", n.State)
	}

	allMet, total, err := f.es.ProveGoalTx(ctx, g1, fact)
	if err != nil || allMet || total != 2 {
		t.Fatalf("prove g1: allMet=%v total=%d err=%v", allMet, total, err)
	}
	if err := f.es.SetNodeState(ctx, g2, StateGoalAbandoned); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.es.ProveGoalTx(ctx, g2, fact); err == nil {
		t.Fatal("abandoned goal must not be provable")
	}
	allMet, total, err = f.es.ProveGoalTx(ctx, g1, fact)
	if err != nil || !allMet || total != 1 {
		t.Fatalf("re-prove with g2 abandoned: allMet=%v total=%d err=%v", allMet, total, err)
	}
	if !f.edges(t)[Edge{fact, RelProves, g1}] {
		t.Fatal("missing proves edge")
	}
}

func TestEdgeKindMatrix(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	goal, _ := f.es.AddGoalTx(ctx, nil, nil)
	intent := f.intent(t)
	for _, c := range []struct {
		src int64
		rel string
		dst int64
	}{
		{intent, RelYields, goal},
		{goal, RelProves, goal},
		{intent, RelDerivedFrom, intent},
		{f.origin, "bogus", goal},
	} {
		if err := f.es.Link(ctx, c.src, c.rel, c.dst); !errors.Is(err, ErrInvalidEdge) {
			t.Errorf("Link(%d -%s-> %d) err=%v, want ErrInvalidEdge", c.src, c.rel, c.dst, err)
		}
	}
}

func TestCrossTaskEvidenceRequiresRelation(t *testing.T) {
	src := newFixture(t)
	ctx := context.Background()
	srcIntent := src.intent(t)
	srcFact, _ := src.es.RecordFactWithYieldsTx(ctx, srcIntent, map[string]any{"summary": "上游事实"}, 0, "w", nil)

	other := newFixture(t)
	if _, err := other.es.AddIntentTx(ctx, AddIntentInput{Parents: []int64{srcFact}}); !errors.Is(err, ErrForeignNode) {
		t.Fatalf("unrelated task used foreign fact: err=%v", err)
	}

	dst := newFixture(t)
	if err := dst.d.AddTaskRelation(ctx, dst.task.ID, src.task.ID); err != nil {
		t.Fatal(err)
	}
	id, err := dst.es.AddIntentTx(ctx, AddIntentInput{Parents: []int64{srcFact}})
	if err != nil {
		t.Fatalf("direct-source fact as parent: %v", err)
	}
	if !dst.edges(t)[Edge{srcFact, RelDerivedFrom, id}] {
		t.Fatal("missing cross-task derived_from edge")
	}
	// yields 的源端必须是本空间节点，来源例外不适用。
	if _, err := dst.es.RecordFactWithYieldsTx(ctx, srcIntent, nil, 0, "w", nil); err == nil {
		t.Fatal("foreign intent must not yield into this exploration")
	}
}

func TestUpsertAssetIdempotentAndMerging(t *testing.T) {
	tender := uniq("ZB")
	f1 := newFixture(t)
	f2 := newFixture(t)
	ctx := context.Background()
	intent := f1.intent(t)
	amount := "1250000.5000"

	id, created, err := f1.as.UpsertAsset(ctx, &Asset{
		Type: AssetCommercialItem, TenderNo: tender, LotNo: "A", Code: "BJ-01",
		Title: "服务器", Amount: &amount, Currency: "CNY", IsMandatory: true,
		SourceRef: []byte(`{"page":12}`),
	}, intent)
	if err != nil || !created {
		t.Fatalf("first upsert: created=%v err=%v", created, err)
	}
	id2, created, err := f2.as.UpsertAsset(ctx, &Asset{
		Type: AssetCommercialItem, TenderNo: tender, LotNo: "A", Code: "BJ-01",
		Content: "2U 机架式", SourceRef: []byte(`{"section":"第五章"}`),
	}, 0)
	if err != nil || created || id2 != id {
		t.Fatalf("second upsert: id=%d/%d created=%v err=%v", id2, id, created, err)
	}

	a, err := f1.as.GetAsset(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if a.Title != "服务器" || a.Content != "2U 机架式" || !a.IsMandatory || a.Amount == nil || *a.Amount != amount {
		t.Fatalf("merged asset = %+v", a)
	}
	if len(a.TaskIDs) != 2 || string(a.SourceRef) != `{"page": 12, "section": "第五章"}` {
		t.Fatalf("task_ids=%v source_ref=%s", a.TaskIDs, a.SourceRef)
	}
	if n, _ := f1.es.GetNode(ctx, intent); len(n.Anchors) != 1 || n.Anchors[0] != id {
		t.Fatalf("source node not anchored: %+v", n.Anchors)
	}
	scoped, err := f1.as.ListScopedAssets(ctx, nil, 100)
	if err != nil || len(scoped) != 1 || scoped[0].ID != id {
		t.Fatalf("auto scope: %v err=%v", scoped, err)
	}
	if _, _, err := f1.as.UpsertAsset(ctx, &Asset{Type: AssetClause, TenderNo: tender}, 0); err == nil {
		t.Fatal("clause without code must violate ck_asset_identity")
	}
}

func TestUpsertEntityEdge(t *testing.T) {
	f1 := newFixture(t)
	f2 := newFixture(t)
	ctx := context.Background()
	tender := uniq("ZB")
	lot, _, _ := f1.as.UpsertAsset(ctx, &Asset{Type: AssetLot, TenderNo: tender, LotNo: "A"}, 0)
	qual, _, _ := f1.as.UpsertAsset(ctx, &Asset{Type: AssetQualification, TenderNo: tender, LotNo: "A", Code: "ZZ-1"}, 0)

	e1, err := f1.as.UpsertEntityEdge(ctx, lot, EntityRelRequires, qual, f1.task.ID, nil, 1, map[string]any{"mandatory": true})
	if err != nil {
		t.Fatalf("UpsertEntityEdge: %v", err)
	}
	e2, err := f2.as.UpsertEntityEdge(ctx, lot, EntityRelRequires, qual, f2.task.ID, nil, 0.5, map[string]any{"note": "x"})
	if err != nil || e2 != e1 {
		t.Fatalf("second upsert: id=%d/%d err=%v", e2, e1, err)
	}
	edges, err := f1.as.EntityEdgesOf(ctx, lot, EntityRelRequires, true, 10)
	if err != nil || len(edges) != 1 {
		t.Fatalf("EntityEdgesOf: %v err=%v", edges, err)
	}
	e := edges[0]
	if len(e.TaskIDs) != 2 || e.Weight != 0.5 || string(e.Props) != `{"note": "x", "mandatory": true}` {
		t.Fatalf("merged edge = %+v props=%s", e, e.Props)
	}
	if _, err := f1.as.UpsertEntityEdge(ctx, lot, EntityRelRequires, lot, 0, nil, 1, nil); err == nil {
		t.Fatal("self edge must be rejected")
	}
}

func TestCoverageWithSources(t *testing.T) {
	tender := uniq("ZB")
	f := newFixture(t, TaskScope{TenderNo: tender, LotNo: "A"})
	ctx := context.Background()
	c1, _, _ := f.as.UpsertAsset(ctx, &Asset{Type: AssetScoringItem, TenderNo: tender, LotNo: "A", Code: "PF-1"}, 0)
	_, _, _ = f.as.UpsertAsset(ctx, &Asset{Type: AssetClause, TenderNo: tender, Code: "1.1"}, 0) // 全标段通用要素
	other := newFixture(t)
	_, _, _ = other.as.UpsertAsset(ctx, &Asset{Type: AssetClause, TenderNo: tender, LotNo: "B", Code: "9.9"}, 0)

	cov, err := f.d.TaskCoverageWithSources(ctx, f.task.ID)
	if err != nil || cov.Target != 2 || cov.Tested != 0 {
		t.Fatalf("initial coverage = %+v err=%v", cov, err)
	}
	intent := f.intent(t)
	if _, err := f.es.RecordFactWithYieldsTx(ctx, intent, map[string]any{"summary": "评分项已核"}, 0, "w", []int64{c1}); err != nil {
		t.Fatal(err)
	}
	cov, _ = f.d.TaskCoverageWithSources(ctx, f.task.ID)
	if cov.Target != 2 || cov.Tested != 1 || cov.Pct == nil || *cov.Pct != 0.5 {
		t.Fatalf("coverage = %+v", cov)
	}
}

func TestRecordFinding(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	intent := f.intent(t)
	fact, _ := f.es.RecordFactWithYieldsTx(ctx, intent, map[string]any{"summary": "缺少 ISO27001 证书"}, 0, "w", nil)

	before := f.countNodes(t, KindFinding)
	if _, err := f.es.RecordFinding(ctx, RecordFindingInput{
		TaskID: f.task.ID, IntentID: intent, Category: FindingCompliance, Title: "资质缺失", Evidence: []int64{intent},
	}); !errors.Is(err, ErrInvalidEdge) {
		t.Fatalf("intent as evidence: err=%v", err)
	}
	if _, err := f.es.RecordFinding(ctx, RecordFindingInput{
		TaskID: f.task.ID, Category: "bogus", Title: "x",
	}); err == nil {
		t.Fatal("invalid category must violate CHECK")
	}
	if got := f.countNodes(t, KindFinding); got != before {
		t.Fatalf("failed findings left %d nodes", got-before)
	}

	rec, err := f.es.RecordFinding(ctx, RecordFindingInput{
		TaskID: f.task.ID, IntentID: intent, Category: FindingCompliance, Severity: SeverityCritical,
		Title: "资质缺失将导致废标", Summary: "★条款 4.2 要求 ISO27001", Evidence: []int64{fact}, Worker: "w",
		Detail: map[string]any{"clause": "4.2"},
	})
	if err != nil {
		t.Fatalf("RecordFinding: %v", err)
	}
	if !f.edges(t)[Edge{intent, RelYields, rec.NodeID}] {
		t.Fatal("missing yields edge to finding")
	}
	var ev int
	_ = f.d.QueryRow(`SELECT COUNT(*) FROM finding_evidence WHERE finding_id = $1 AND node_id = $2`, rec.FindingID, fact).Scan(&ev)
	if ev != 1 {
		t.Fatal("missing finding evidence")
	}
	other := newFixture(t)
	if _, err := f.es.RecordFinding(ctx, RecordFindingInput{TaskID: other.task.ID, Category: FindingRisk, Title: "x"}); err == nil {
		t.Fatal("task/exploration mismatch must be rejected")
	}
}

func TestIntentLifecycleControls(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.intent(t)
	epoch, _, _ := f.es.ClaimIntentWithLease(ctx, id, "w", time.Minute)
	if ok, _ := f.es.CompareAndSetIntentStateFenced(ctx, id, StateIntentRunning, StateIntentPaused, epoch); !ok {
		t.Fatal("running->paused failed")
	}
	epoch2, ok, err := f.es.ResumePausedIntentWithLease(ctx, id, "chat", time.Minute)
	if !ok || err != nil || epoch2 != epoch+1 {
		t.Fatalf("paused->running: epoch=%d ok=%v err=%v", epoch2, ok, err)
	}
	if ok, _ := f.es.CompareAndSetIntentStateFenced(ctx, id, StateIntentRunning, StateIntentExhausted, epoch2); !ok {
		t.Fatal("running->exhausted failed")
	}
	if ok, _ := f.es.ReopenIntent(ctx, id); !ok {
		t.Fatal("exhausted->open failed")
	}
	if ok, err := f.es.SoftDeleteIntent(ctx, id, "superseded"); !ok || err != nil {
		t.Fatalf("soft delete: ok=%v err=%v", ok, err)
	}
	if ok, _ := f.es.ReopenIntent(ctx, id); ok {
		t.Fatal("deleted intent must not reopen")
	}
	n, _ := f.es.GetNode(ctx, id)
	if n.State != StateIntentDeleted || n.DeleteReason != "superseded" {
		t.Fatalf("after delete: %+v", n)
	}
	if ok, _ := f.es.SoftDeleteIntent(ctx, id, "again"); ok {
		t.Fatal("double delete must be a no-op")
	}
}

func TestTaskLLMChainQuotaTransitions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, err := f.d.CreateLLMProfile(ctx, LLMProfile{Name: uniq("prof-a"), Model: "claude-opus-5-5", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := f.d.CreateLLMProfile(ctx, LLMProfile{Name: uniq("prof-b"), Model: "claude-sonnet-5-5", Enabled: true})
	if err := f.d.SetTaskLLMChain(ctx, f.task.ID, []int64{a, b}); err != nil {
		t.Fatal(err)
	}
	st, _ := f.d.TaskLLMState(ctx, f.task.ID)
	if !st.HasChain || st.ActiveProfileID == nil || *st.ActiveProfileID != a {
		t.Fatalf("initial state %+v", st)
	}

	tr, err := f.d.MarkTaskLLMProfileQuotaExhaustedAtRevision(ctx, f.task.ID, a, st.Revision, "credit balance too low")
	if err != nil || !tr.Advanced || tr.Stale || tr.NextProfileID == nil || *tr.NextProfileID != b {
		t.Fatalf("advance: %+v err=%v", tr, err)
	}
	// 携带旧 revision 的并发请求不得覆盖新链。
	stale, err := f.d.MarkTaskLLMProfileQuotaExhaustedAtRevision(ctx, f.task.ID, a, st.Revision, "late")
	if err != nil || !stale.Stale || stale.Advanced || *stale.NextProfileID != b {
		t.Fatalf("stale: %+v err=%v", stale, err)
	}
	st2, _ := f.d.TaskLLMState(ctx, f.task.ID)
	last, err := f.d.MarkTaskLLMProfileQuotaExhaustedAtRevision(ctx, f.task.ID, b, st2.Revision, "billing")
	if err != nil || !last.ChainExhausted || last.NextProfileID != nil {
		t.Fatalf("exhausted: %+v err=%v", last, err)
	}
	st3, _ := f.d.TaskLLMState(ctx, f.task.ID)
	if st3.ActiveProfileID != nil || !st3.HasChain {
		t.Fatalf("exhausted chain must not wrap around: %+v", st3)
	}
	if err := f.d.ResetTaskLLMChain(ctx, f.task.ID); err != nil {
		t.Fatal(err)
	}
	st4, _ := f.d.TaskLLMState(ctx, f.task.ID)
	if st4.ActiveProfileID == nil || *st4.ActiveProfileID != a || st4.Revision <= st3.Revision {
		t.Fatalf("reset: %+v", st4)
	}
}

func TestLLMUsageAndHealthPersistence(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	pid, _ := f.d.CreateLLMProfile(ctx, LLMProfile{Name: uniq("prof"), Model: "claude-opus-5-5", Enabled: true})
	for _, u := range []LLMUsage{
		{TaskID: f.task.ID, Worker: "worker", Model: "claude-opus-5-5", ProfileID: pid, InputTokens: 100, OutputTokens: 20, Status: "ok"},
		{TaskID: f.task.ID, Worker: "planner", Model: "claude-opus-5-5", ProfileID: pid, InputTokens: 50, Status: "error", Error: "boom"},
	} {
		if err := f.d.InsertLLMUsage(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	totals, err := f.d.TaskUsageTotals(ctx, f.task.ID)
	if err != nil || len(totals) != 1 || totals[0].Calls != 2 || totals[0].InputTokens != 150 {
		t.Fatalf("totals=%+v err=%v", totals, err)
	}
	open := time.Now().Add(time.Hour)
	if err := f.d.SaveLLMHealth(ctx, LLMHealth{ProfileID: pid, Trips: 1, OpenUntil: open, LastError: "401"}); err != nil {
		t.Fatal(err)
	}
	hs, _ := f.d.ActiveLLMHealth(ctx)
	found := false
	for _, h := range hs {
		found = found || (h.ProfileID == pid && h.Trips == 1)
	}
	if !found {
		t.Fatal("active circuit breaker not restored")
	}
	_ = f.d.DeleteLLMHealth(ctx, pid)
}
