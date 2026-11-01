package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/autobid/autobid-engine/ingest"
)

// TestFixturesDeterministic 验证同一 fixture_version 的招标/更正/原件字节完全一致。
func TestFixturesDeterministic(t *testing.T) {
	dir := t.TempDir()
	a, err := generateFixtures(dir+"/a", dir+"/company-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := generateFixtures(dir+"/b", dir+"/company-b")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Tender, b.Tender) || !bytes.Equal(a.Correction, b.Correction) {
		t.Fatal("tender/correction bytes differ between generations")
	}
	for key, data := range a.Originals {
		if !bytes.Equal(data, b.Originals[key]) {
			t.Fatalf("original %s differs", key)
		}
	}
	t.Logf("tender=%s correction=%s originals=%d", sha256Hex(a.Tender), sha256Hex(a.Correction), len(a.Originals))
	for _, key := range keysOf(a.Hashes) {
		t.Logf("%s=%s", key, a.Hashes[key])
	}
}

func TestTenderChunks(t *testing.T) {
	p, err := ingest.Parse(context.Background(), ingest.File{Name: "tender.docx", MIME: demoDOCXMIME, Data: buildTenderDOCX()}, ingest.ParseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	chunks := ingest.Split(p.Text)
	for _, c := range chunks {
		t.Logf("code=%q title=%q content=%q", c.Code, c.Title, truncate(c.Content, 40))
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
