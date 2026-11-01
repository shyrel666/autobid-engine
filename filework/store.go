// Package filework 实现任务原件的内容寻址存储。物理对象按租户共享，登记行按任务隔离；
// relative_path 只由服务端生成，文件读取时重新计算 SHA-256 并与登记值比对。
package filework

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/db"
)

// DefaultDir 是 AUTOBID_FILEWORK_DIR 的缺省值（已加入 .gitignore）。
const DefaultDir = "data/filework"

// MaxUploadBytes 与 server/knowledge.go 的上传上限一致；可在 Store 上按测试覆盖。
const MaxUploadBytes int64 = 64 << 20

var (
	ErrTooLarge     = errors.New("filework: file exceeds size limit")
	ErrHashMismatch = errors.New("filework: content hash mismatch")
	ErrUnsafePath   = errors.New("filework: unsafe path")
)

var (
	safeTenant = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	drivePath  = regexp.MustCompile(`^[A-Za-z]:`)
)

// MetaStore 是 filework 需要的最小持久化接口；*db.DB 实现它，测试可用内存替身。
type MetaStore interface {
	InsertTaskFileObject(ctx context.Context, o *db.TaskFileObject) (*db.TaskFileObject, bool, error)
	GetTaskFileObject(ctx context.Context, tenant string, taskID, id int64) (*db.TaskFileObject, bool, error)
	ReferencedFilePaths(ctx context.Context, tenant string) (map[string]bool, error)
}

// Store 管理一个根目录下的对象。不同租户目录互相隔离。
type Store struct {
	meta     MetaStore
	root     string
	maxBytes int64
}

// NewStore 创建并准备好根目录。
func NewStore(meta MetaStore, root string) (*Store, error) {
	if meta == nil {
		return nil, errors.New("filework: meta store is required")
	}
	root = strings.TrimSpace(root)
	if root == "" {
		root = DefaultDir
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	return &Store{meta: meta, root: abs, maxBytes: MaxUploadBytes}, nil
}

// SetMaxBytes 覆盖单文件上限（测试用）；非正值恢复默认。
func (s *Store) SetMaxBytes(n int64) {
	if n <= 0 {
		n = MaxUploadBytes
	}
	s.maxBytes = n
}

// Root 返回绝对根目录。
func (s *Store) Root() string { return s.root }

// PutOptions 描述一次对象登记；RelativePath 不接受调用方输入。
type PutOptions struct {
	TenantID      string
	TaskID        int64
	Kind          string
	OriginalName  string
	MediaType     string
	ArchivePath   string
	Format        string
	ProcessStatus string
	SourceJobID   *int64
	ParentFileID  *int64
	CreatedBy     string
}

// Put 流式写入、边写边计算 SHA-256、原子移动到内容寻址路径，再登记元数据。
func (s *Store) Put(ctx context.Context, opts PutOptions, r io.Reader) (*db.TaskFileObject, error) {
	if r == nil {
		return nil, errors.New("filework: nil reader")
	}
	if opts.TaskID <= 0 {
		return nil, errors.New("filework: task_id is required")
	}
	switch opts.Kind {
	case db.FileKindOriginal, db.FileKindScript, db.FileKindDerived, db.FileKindTextSnapshot:
	default:
		return nil, fmt.Errorf("filework: invalid kind %q", opts.Kind)
	}
	tenant, err := tenantDir(opts.TenantID)
	if err != nil {
		return nil, err
	}
	archivePath, err := cleanArchivePath(opts.ArchivePath)
	if err != nil {
		return nil, err
	}
	tmpDir := filepath.Join(s.root, tenant, "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(tmpDir, "put-*")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		_ = os.Remove(tmpName)
	}()
	h := sha256.New()
	limited := io.LimitReader(r, s.maxBytes+1)
	n, err := io.Copy(io.MultiWriter(tmp, h), limited)
	if err != nil {
		return nil, err
	}
	if n > s.maxBytes {
		return nil, fmt.Errorf("%w: %d > %d", ErrTooLarge, n, s.maxBytes)
	}
	if err := tmp.Sync(); err != nil {
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	relative := path.Join(tenant, "objects", sum[:2], sum)
	target := filepath.Join(s.root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return nil, err
	}
	if st, err := os.Stat(target); err == nil {
		if st.Size() != n {
			return nil, fmt.Errorf("%w: size mismatch for %s", ErrHashMismatch, relative)
		}
		_ = os.Remove(tmpName)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	} else if err := os.Rename(tmpName, target); err != nil {
		return nil, err
	} else {
		_ = os.Remove(tmpName) // 已改名，defer 的 Remove 是空操作
	}
	media := strings.TrimSpace(opts.MediaType)
	if media == "" {
		media = "application/octet-stream"
	}
	name := filepath.Base(strings.TrimSpace(opts.OriginalName))
	if name == "." || name == string(filepath.Separator) {
		name = ""
	}
	obj, _, err := s.meta.InsertTaskFileObject(ctx, &db.TaskFileObject{
		TenantID: tenant, TaskID: opts.TaskID, Kind: opts.Kind, SHA256: sum, ByteSize: n,
		RelativePath: relative, OriginalName: name, MediaType: media, ArchivePath: archivePath,
		Format: opts.Format, ProcessStatus: opts.ProcessStatus,
		SourceJobID: opts.SourceJobID, ParentFileID: opts.ParentFileID, Availability: "available",
		CreatedBy: opts.CreatedBy,
	})
	if err != nil {
		// 物理对象保持原样：同租户其他任务可能已引用同一内容；孤立对象由 Cleanup 回收。
		return nil, err
	}
	return obj, nil
}

// PutBytes 是 Put 的字节便捷入口。
func (s *Store) PutBytes(ctx context.Context, opts PutOptions, data []byte) (*db.TaskFileObject, error) {
	return s.Put(ctx, opts, bytes.NewReader(data))
}

// Read 读取对象并按登记哈希校验；跨任务 ID 返回 db.ErrNotFound。
func (s *Store) Read(ctx context.Context, tenant string, taskID, fileID int64) ([]byte, *db.TaskFileObject, error) {
	obj, _, err := s.meta.GetTaskFileObject(ctx, tenant, taskID, fileID)
	if err != nil {
		return nil, nil, err
	}
	if obj.Availability != "available" {
		return nil, obj, fmt.Errorf("filework: object %d is %s", fileID, obj.Availability)
	}
	target, err := s.resolve(obj.RelativePath)
	if err != nil {
		return nil, obj, err
	}
	f, err := os.Open(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, obj, fmt.Errorf("filework: object %d missing at %s", fileID, obj.RelativePath)
		}
		return nil, obj, err
	}
	defer f.Close()
	limited := io.LimitReader(f, s.maxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, obj, err
	}
	if int64(len(data)) > s.maxBytes {
		return nil, obj, fmt.Errorf("%w: object %d", ErrTooLarge, fileID)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != obj.SHA256 || int64(len(data)) != obj.ByteSize {
		return nil, obj, fmt.Errorf("%w: object %d", ErrHashMismatch, fileID)
	}
	return data, obj, nil
}

func (s *Store) resolve(relative string) (string, error) {
	rel := filepath.Clean(filepath.FromSlash(relative))
	if rel == "." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return "", fmt.Errorf("%w: %s", ErrUnsafePath, relative)
	}
	target := filepath.Join(s.root, rel)
	check, err := filepath.Rel(s.root, target)
	if err != nil || check == ".." || strings.HasPrefix(check, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s", ErrUnsafePath, relative)
	}
	return target, nil
}

// Cleanup 删除租户 objects 目录下不在登记表中的孤立普通文件；只处理早于 olderThan 的文件。
// 不跟随符号链接，不删除目录本身（空目录留给后续清理）。
func (s *Store) Cleanup(ctx context.Context, tenant string, olderThan time.Time) (int, error) {
	t, err := tenantDir(tenant)
	if err != nil {
		return 0, err
	}
	refs, err := s.meta.ReferencedFilePaths(ctx, t)
	if err != nil {
		return 0, err
	}
	base := filepath.Join(s.root, t, "objects")
	removed := 0
	err = filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(olderThan) {
			return nil
		}
		rel, err := filepath.Rel(s.root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if refs[rel] {
			return nil
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		removed++
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return removed, nil
	}
	return removed, err
}

func tenantDir(tenant string) (string, error) {
	tenant = strings.TrimSpace(tenant)
	if tenant == "" {
		tenant = db.DefaultTenant
	}
	if !safeTenant.MatchString(tenant) || tenant == "." || tenant == ".." {
		return "", fmt.Errorf("%w: tenant %q", ErrUnsafePath, tenant)
	}
	return tenant, nil
}

func cleanArchivePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", nil
	}
	if strings.ContainsAny(p, "\\\x00") || strings.HasPrefix(p, "/") || drivePath.MatchString(p) {
		return "", fmt.Errorf("%w: archive path %q", ErrUnsafePath, p)
	}
	clean := path.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%w: archive path %q", ErrUnsafePath, p)
	}
	return clean, nil
}
