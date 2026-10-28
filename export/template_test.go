package export

import (
	"bytes"
	"os"
	"testing"
)

// TestDefaultTemplateUpToDate 保证仓库中的 templates/default.docx 与生成代码一致。
func TestDefaultTemplateUpToDate(t *testing.T) {
	want, err := buildDefaultTemplate()
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("AUTOBID_UPDATE_TEMPLATE") == "1" {
		if err := os.WriteFile("templates/default.docx", want, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if !bytes.Equal(want, defaultTemplateDocx) {
		t.Fatal("templates/default.docx is out of date; run: AUTOBID_UPDATE_TEMPLATE=1 go test ./export -run TestDefaultTemplateUpToDate")
	}
	tpl, err := DefaultTemplate()
	if err != nil {
		t.Fatal(err)
	}
	if g := tpl.Geometry; g.PageW != 11906 || g.TextWidth() != 9070 {
		t.Fatalf("geometry %+v", g)
	}
	if m := tpl.Meta(); len(m.MissingStyles) != 0 || !m.HasContentTag {
		t.Fatalf("default template meta %+v", m)
	}
}
