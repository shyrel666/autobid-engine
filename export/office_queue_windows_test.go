//go:build windows

package export

// 文档引擎排队与连续运行（S1，计划 12.3 / 12.5 的“多作业并发与稳定性”）：两个导出执行者同时领取 6 个真实导出作业，
// 同一引擎同一时间只处理一个文档（其余排队，不因等待而丢租约或重试）；连续 6 次真实刷新全部成功、版式自检无问题、
// 同一快照的三份 PDF 页数一致，结束后没有残留自动化进程，调用前已存在的进程不受影响。依赖文档引擎与真实 PostgreSQL。

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/db/dbtest"
)

// trackingOffice 记录同时进入 Refresh 的调用数（含在引擎内排队等待的调用）。
type trackingOffice struct {
	Office
	mu            sync.Mutex
	inflight, top int
	calls         int
}

func (o *trackingOffice) Refresh(ctx context.Context, in, outDocx, outPDF string) (*RefreshResult, error) {
	o.mu.Lock()
	o.inflight++
	o.calls++
	o.top = max(o.top, o.inflight)
	o.mu.Unlock()
	defer func() { o.mu.Lock(); o.inflight--; o.mu.Unlock() }()
	return o.Office.Refresh(ctx, in, outDocx, outPDF)
}

func TestOfficeQueuesConcurrentExportJobs(t *testing.T) {
	d := dbtest.Open(t)
	office := requireOffice(t)
	eng := comEngines[office.Name()]
	before := listProcs(t, eng.image)
	start := time.Now()
	p := dbtest.Ready(t, d, dbtest.DefaultSpec(60).WithWideTable("1.1", 13, 30))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ids []int64
	for i := 0; i < 3; i++ {
		jobs, _, err := d.CreateExportJobs(ctx, db.DefaultTenant, p.TaskID, "exporter", db.CreateExportInput{Formats: []string{db.ExportDOCX, db.ExportPDF}, DefaultTemplateHash: DefaultTemplateHash()})
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range jobs {
			ids = append(ids, j.ID)
		}
	}
	tracked := &trackingOffice{Office: office}
	svc := &Service{DB: d, Store: &Store{Root: t.TempDir()}, Office: tracked}

	// 采样本次启动的自动化进程数：同一引擎串行时，任一时刻最多只有一次会话的进程（WPS 为主进程 + 子进程）。
	sampled := 0
	stopSampler := make(chan struct{})
	samplerDone := make(chan struct{})
	go func() {
		defer close(samplerDone)
		for {
			select {
			case <-stopSampler:
				return
			default:
			}
			n := 0
			for _, pr := range listProcs(t, eng.image) {
				if (strings.Contains(pr.Cmd, "/Automation") || strings.Contains(pr.Cmd, "-Embedding")) && !pr.Created.Before(start.Add(-2*time.Second)) {
					n++
				}
			}
			sampled = max(sampled, n)
			time.Sleep(200 * time.Millisecond)
		}
	}()

	r := &Runner{Service: svc, Owner: "queue", Workers: 2, Lease: OfficeTimeout() + 60*time.Second, Poll: 200 * time.Millisecond, TaskID: p.TaskID}
	began := time.Now()
	r.Start(ctx)
	deadline := time.Now().Add(15 * time.Minute)
	var done []*db.ExportJob
	for time.Now().Before(deadline) {
		done = done[:0]
		for _, id := range ids {
			if j := reloadJob(t, d, p.TaskID, id); j.Status == db.ExportSucceeded || j.Status == db.ExportFailed {
				done = append(done, j)
			}
		}
		if len(done) == len(ids) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	elapsed := time.Since(began)
	cancel()
	close(stopSampler)
	<-samplerDone
	if len(done) != len(ids) {
		t.Fatalf("only %d of %d export jobs finished in time", len(done), len(ids))
	}
	tpl, _ := DefaultTemplate()
	pages := map[int]bool{}
	for _, j := range done {
		if j.Status != db.ExportSucceeded || !j.TOCRefreshed || j.Attempts != 1 {
			t.Fatalf("job %d (%s) = %s attempts=%d toc=%v: %s", j.ID, j.Format, j.Status, j.Attempts, j.TOCRefreshed, j.Error)
		}
		for _, w := range j.Warnings {
			if w.Kind == db.WarnSelfCheck {
				t.Errorf("job %d self-check: %s", j.ID, w.Message)
			}
		}
		if j.Format == db.ExportPDF {
			rep, err := CheckPDF(artifact(t, svc, j), tpl.Geometry)
			if err != nil {
				t.Fatal(err)
			}
			for _, is := range rep.Issues {
				t.Errorf("job %d PDF issue (page %d) %s: %s", j.ID, is.Page, is.Kind, is.Detail)
			}
			pages[rep.Pages] = true
		}
	}
	if len(pages) != 1 {
		t.Errorf("same snapshot exported to different page counts: %v", pages)
	}
	if tracked.calls != len(ids) || tracked.top < 2 {
		t.Errorf("engine calls = %d, max concurrent callers = %d; want %d calls with 2 workers contending", tracked.calls, tracked.top, len(ids))
	}
	if limit := map[string]int{EngineWPS: 2, EngineWord: 1}[eng.name]; sampled > limit {
		t.Errorf("sampled %d %s automation processes at once; the engine must process one document at a time", sampled, eng.label)
	}
	time.Sleep(time.Second)
	for _, pr := range listProcs(t, eng.image) {
		if (strings.Contains(pr.Cmd, "/Automation") || strings.Contains(pr.Cmd, "-Embedding")) && !pr.Created.Before(start.Add(-2*time.Second)) {
			t.Errorf("leftover %s automation process %d", eng.label, pr.PID)
		}
	}
	after := listProcs(t, eng.image)
	for _, pr := range before {
		if !alive(after, pr.PID) {
			t.Errorf("pre-existing %s process %d was ended", eng.label, pr.PID)
		}
	}
	t.Logf("%d jobs on %s in %s (2 workers, engine serial); PDF pages %v; max sampled automation processes %d", len(ids), office.Version(), elapsed.Round(time.Second), pages, sampled)
}
