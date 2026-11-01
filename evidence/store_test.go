package evidence

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPutReadCheckAndPathSafety(t *testing.T) {
	s := New(t.TempDir())
	rel, sum, err := s.Put("d2-test", "docx", []byte("fake original"))
	if err != nil || rel == "" || sum == "" {
		t.Fatalf("put: rel=%q sum=%q err=%v", rel, sum, err)
	}
	if !strings.HasPrefix(rel, "d2-test/originals/") || !strings.HasSuffix(rel, ".docx") {
		t.Fatalf("unexpected layout: %s", rel)
	}
	again, sum2, err := s.Put("d2-test", "docx", []byte("fake original"))
	if err != nil || again != rel || sum2 != sum {
		t.Fatalf("content addressing must be idempotent: %s %s %v", again, sum2, err)
	}
	data, err := s.Read(rel, sum)
	if err != nil || string(data) != "fake original" {
		t.Fatalf("read: %q %v", data, err)
	}
	if _, err := s.Read(rel, strings.Repeat("0", 64)); err == nil {
		t.Fatal("hash mismatch accepted")
	}
	readable, match, err := s.Check(rel, sum)
	if err != nil || !readable || !match {
		t.Fatalf("check: readable=%v match=%v err=%v", readable, match, err)
	}
	if readable, match, err := s.Check("d2-test/originals/"+sum+".docx", strings.Repeat("a", 64)); err != nil || !readable || match {
		t.Fatalf("wrong hash: readable=%v match=%v err=%v", readable, match, err)
	}
	readable, _, err = s.Check("d2-test/originals/missing.docx", sum)
	if err != nil || readable {
		t.Fatalf("missing file: readable=%v err=%v", readable, err)
	}
	for _, bad := range []string{"../etc/passwd", "/abs/path", `C:\x`, "a/../../b"} {
		if _, err := s.Abs(bad); err == nil {
			t.Fatalf("unsafe path accepted: %s", bad)
		}
	}
	if _, err := s.Read("d2-test/originals/no-such.docx", ""); !os.IsNotExist(err) {
		t.Fatalf("expected not exist, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Root)); err != nil {
		t.Fatalf("root missing: %v", err)
	}
}
