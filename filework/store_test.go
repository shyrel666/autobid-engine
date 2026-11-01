package filework

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/autobid/autobid-engine/db"
)

type memMeta struct {
	mu   sync.Mutex
	next int64
	objs map[int64]*db.TaskFileObject
}

func newMemMeta() *memMeta { return &memMeta{objs: map[int64]*db.TaskFileObject{}} }

func (m *memMeta) InsertTaskFileObject(_ context.Context, o *db.TaskFileObject) (*db.TaskFileObject, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, cur := range m.objs {
		if cur.TenantID == o.TenantID && cur.TaskID == o.TaskID && cur.SHA256 == o.SHA256 && cur.ArchivePath == o.ArchivePath {
			return cur, false, nil
		}
	}
	m.next++
	cp := *o
	cp.ID = m.next
	cp.Availability = "available"
	m.objs[cp.ID] = &cp
	return &cp, true, nil
}

func (m *memMeta) GetTaskFileObject(_ context.Context, tenant string, taskID, id int64) (*db.TaskFileObject, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objs[id]
	if !ok || o.TenantID != tenant || o.TaskID != taskID {
		return nil, false, db.ErrNotFound
	}
	cp := *o
	return &cp, true, nil
}

func (m *memMeta) ReferencedFilePaths(_ context.Context, tenant string) (map[string]bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]bool{}
	for _, o := range m.objs {
		if o.TenantID == tenant {
			out[o.RelativePath] = true
		}
	}
	return out, nil
}

func testStore(t *testing.T) (*Store, *memMeta) {
	t.Helper()
	meta := newMemMeta()
	s, err := NewStore(meta, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s, meta
}

func TestPutReadHashAndDedupe(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	data := []byte("招标原件内容")
	obj, err := s.PutBytes(ctx, PutOptions{TenantID: "default", TaskID: 1, Kind: db.FileKindOriginal,
		OriginalName: "../报价.xlsx", MediaType: "application/vnd.ms-excel", CreatedBy: "tester"}, data)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if obj.SHA256 != hex.EncodeToString(sum[:]) || obj.ByteSize != int64(len(data)) {
		t.Fatalf("object = %+v", obj)
	}
	if filepath.Base(obj.OriginalName) != obj.OriginalName || obj.OriginalName != "报价.xlsx" {
		t.Fatalf("original name not sanitized: %q", obj.OriginalName)
	}
	got, readObj, err := s.Read(ctx, "default", 1, obj.ID)
	if err != nil || string(got) != string(data) || readObj.ID != obj.ID {
		t.Fatalf("read = %q %+v %v", got, readObj, err)
	}
	again, err := s.PutBytes(ctx, PutOptions{TenantID: "default", TaskID: 1, Kind: db.FileKindOriginal, OriginalName: "副本.xlsx"}, data)
	if err != nil || again.ID != obj.ID {
		t.Fatalf("dedupe = %+v %v", again, err)
	}
	// 跨任务共享同一物理对象，但登记行分别存在。
	other, err := s.PutBytes(ctx, PutOptions{TenantID: "default", TaskID: 2, Kind: db.FileKindOriginal, OriginalName: "副本.xlsx"}, data)
	if err != nil || other.ID == obj.ID || other.RelativePath != obj.RelativePath {
		t.Fatalf("cross-task = %+v %v", other, err)
	}
}

func TestReadDetectsTamper(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	obj, err := s.PutBytes(ctx, PutOptions{TenantID: "default", TaskID: 1, Kind: db.FileKindOriginal}, []byte("original"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.Root(), filepath.FromSlash(obj.RelativePath))
	if err := os.WriteFile(path, []byte("tampered!"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Read(ctx, "default", 1, obj.ID); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("tampered read error = %v", err)
	}
}

func TestPutLimitsAndUnsafePaths(t *testing.T) {
	s, _ := testStore(t)
	s.SetMaxBytes(4)
	ctx := context.Background()
	if _, err := s.PutBytes(ctx, PutOptions{TenantID: "default", TaskID: 1, Kind: db.FileKindOriginal}, []byte("12345")); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize error = %v", err)
	}
	if _, err := s.PutBytes(ctx, PutOptions{TenantID: "../escape", TaskID: 1, Kind: db.FileKindOriginal}, []byte("x")); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("tenant escape error = %v", err)
	}
	if _, err := s.PutBytes(ctx, PutOptions{TenantID: "default", TaskID: 1, Kind: db.FileKindOriginal, ArchivePath: "../x"}, []byte("x")); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("archive escape error = %v", err)
	}
}

func TestCleanupRemovesOnlyOrphans(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	obj, err := s.PutBytes(ctx, PutOptions{TenantID: "default", TaskID: 1, Kind: db.FileKindOriginal}, []byte("keep"))
	if err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(s.Root(), "default", "objects", "aa", "orphan")
	if err := os.MkdirAll(filepath.Dir(orphan), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphan, []byte("orphan"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	for _, p := range []string{filepath.Join(s.Root(), filepath.FromSlash(obj.RelativePath)), orphan} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.Cleanup(ctx, "default", time.Now().Add(-time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("cleanup = %d %v", n, err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan still exists: %v", err)
	}
	if _, _, err := s.Read(ctx, "default", 1, obj.ID); err != nil {
		t.Fatalf("referenced object removed: %v", err)
	}
}
