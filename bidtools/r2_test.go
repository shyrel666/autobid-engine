package bidtools

import (
	"regexp"
	"testing"
)

// R2 性质 10：Planner / Worker 不继承任何人的角色，也没有任何审批类工具——批准、会签、确认、采用 / 拒绝候选、
// 启用索引、成员与质量策略、账号都只能由登录账号在页面或接口上完成。新增工具若落入这些动作须先改设计。
func TestR2NoApprovalToolsForAgents(t *testing.T) {
	k := New(nil, 1, nil, nil)
	approval := regexp.MustCompile(`approve|approval|sign_?off|confirm|adopt|reject|activate|rollback|member|role|policy|user|password|login|resolve|decide|submission|veto|bid_scope|bid_decision`)
	seen := 0
	for _, tl := range append(append(k.ResidentTools(), k.DeferredTools()...), k.SectionWriterTools()...) {
		seen++
		if approval.MatchString(tl.Name()) {
			t.Errorf("agent tool %q looks like an approval or identity action", tl.Name())
		}
	}
	if seen < 30 {
		t.Fatalf("expected the full tool set, saw %d", seen)
	}
}
