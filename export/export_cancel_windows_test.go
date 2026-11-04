//go:build windows

package export

// S4：取消正在真实文档引擎（WPS / Word）中刷新的最终稿导出作业。取消在数据库内立即建立围栏（终态 + 推进令牌）；执行者的心跳
// 随即失去租约并取消本次刷新（结束本次启动的自动化进程），或刷新先完成而写回被拒绝——两种顺序下作业都保持 cancelled、
// 不登记产物、不留下本次尝试的文件，调用前已存在的进程不受影响。心跳间隔缩短到 300ms 以便在刷新中途取消。
// 依赖文档引擎与真实 PostgreSQL。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
	"github.com/autobid/autobid-engine/db/dbtest"
)

func TestExportCancelDuringRealRefresh(t *testing.T) {
	d := dbtest.Open(t)
	o := requireOffice(t)
	eng := comEngines[o.Name()]
	ctx := context.Background()
	p := dbtest.Ready(t, d, dbtest.DefaultSpec(40).WithWideTable("1.1", 13, 18))
	svc := &Service{DB: d, Store: &Store{Root: t.TempDir()}, Office: o}
	sub, err := svc.SubmitExport(ctx, db.DefaultTenant, p.TaskID, "exporter", finalRequest("cancel-real", db.ExportPDF))
	if err != nil {
		t.Fatal(err)
	}
	before := listProcs(t, eng.image)
	start := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := (&Runner{Service: svc, Owner: "s4-cancel", Lease: 5 * time.Minute, Heartbeat: 300 * time.Millisecond, TaskID: p.TaskID}).RunOnce(ctx)
		done <- err
	}()
	automation := func(ps []winProc) []winProc {
		var got []winProc
		for _, p := range ps {
			if (strings.Contains(p.Cmd, "/Automation") || strings.Contains(p.Cmd, "-Embedding")) && !p.Created.Before(start.Add(-2*time.Second)) {
				got = append(got, p)
			}
		}
		return got
	}
	ours := waitFor(t, 60*time.Second, func() []winProc { return automation(listProcs(t, eng.image)) })
	j, cancelled, err := d.CancelExportJob(ctx, db.DefaultTenant, p.TaskID, sub.Jobs[0].ID, "reviewer")
	if err != nil || !cancelled || j.Status != db.ExportCancelled {
		t.Fatalf("cancel = %+v %v %v (automation processes seen: %d)", j, cancelled, err, len(ours))
	}
	cancelledAt := time.Now()
	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("runner did not return after the export was cancelled")
	}
	path := "刷新被心跳取消"
	if errors.Is(runErr, db.ErrLeaseLost) {
		path = "刷新先完成，写回被围栏拒绝"
	}
	after := reloadJob(t, d, p.TaskID, j.ID)
	if after.Status != db.ExportCancelled || after.ArtifactPath != "" || after.ArtifactHash != "" || after.ErrorCode != db.JobErrCancelled {
		t.Fatalf("job after cancel = %+v", after)
	}
	if files := jobDirFiles(t, svc, after); len(files) != 0 {
		t.Fatalf("cancelled export left files: %v", files)
	}
	if left := automation(listProcs(t, eng.image)); len(left) > 0 {
		time.Sleep(3 * time.Second)
		if left = automation(listProcs(t, eng.image)); len(left) > 0 {
			t.Fatalf("%s 残留自动化进程：%+v", eng.label, left)
		}
	}
	for _, bp := range before {
		if !alive(listProcs(t, eng.image), bp.PID) {
			t.Errorf("调用前已存在的 %s 进程 %d 被结束（%s）", eng.label, bp.PID, bp.Cmd)
		}
	}
	if ok, _ := (&Runner{Service: svc, Owner: "idle", TaskID: p.TaskID}).RunOnce(ctx); ok {
		t.Fatal("cancelled export was executed again")
	}
	t.Logf("%s：%d 个自动化进程，取消后 %.1f 秒执行者返回（%v）", path, len(ours), time.Since(cancelledAt).Seconds(), runErr)
}
