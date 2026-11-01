package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/filework"
	"github.com/autobid/autobid-engine/ingest"
)

func TestFileProcessingBrowser(t *testing.T) {
	if os.Getenv("PLAYWRIGHT_MODULE") == "" {
		t.Skip("set PLAYWRIGHT_MODULE and build web to run browser acceptance")
	}
	c := newFileAPIWithDB(t, isolatedModelDB(t))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	runner := &filework.Runner{Processor: &filework.Processor{DB: c.pg, Store: c.store}, Owner: "browser", Poll: 50 * time.Millisecond}
	done := make(chan struct{})
	go func() { defer close(done); runner.Run(ctx) }()
	defer func() { cancel(); <-done }()
	task := c.createTask(t)
	// Seed a formal document whose old parser missed text. Bytes stay unchanged.
	status, raw := c.upload(t, task, "file", "original.csv", []byte("名称,数量\n服务器,12\n"), map[string]string{"tender_no": "BROWSER-REV"})
	if status != 201 {
		t.Fatalf("seed: %d %s", status, raw)
	}
	var up struct {
		Result ingest.Result `json:"result"`
	}
	json.Unmarshal(raw, &up)
	if _, err := c.pg.ExecContext(ctx, `UPDATE document_chunks SET content='旧解析：遗漏设备数量' WHERE doc_id=$1`, up.Result.Document.ID); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte(testToken), 0600); err != nil {
		t.Fatal(err)
	}
	fixturePath, _ := filepath.Abs("../eval/file-processing/fixtures/safe.zip")
	cmd := exec.CommandContext(ctx, "node", "../web/e2e/file-processing.mjs")
	cmd.Env = append(os.Environ(), "E2E_BASE="+c.url, "E2E_TOKEN_FILE="+token, fmt.Sprintf("E2E_REV_TASK=%d", task), fmt.Sprintf("E2E_REV_DOC=%d", up.Result.Document.ID), "E2E_ZIP="+fixturePath)
	output, err := cmd.CombinedOutput()
	t.Log(string(output))
	if err != nil {
		t.Fatal(err)
	}
}
