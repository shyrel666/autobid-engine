// Package evidence 是 D2 企业证据原件的本地内容寻址存储（与 export.Store 同类，不属于 eval 夹具）。
// 布局：<root>/<tenant>/originals/<sha256>.<ext>；数据库只保存相对路径与 SHA-256。
// 读取时校验哈希并拒绝绝对路径、盘符与 ".."，原件目录不进入 Git。
package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// DefaultDir 是生产默认原件目录。
const DefaultDir = "data/evidence"

// Store 是原件目录。Root 为空时使用 AUTOBID_EVIDENCE_DIR 或 DefaultDir。
type Store struct {
	Root string
}

// New 返回指定根目录的存储。
func New(root string) *Store { return &Store{Root: root} }

// FromEnv 按 AUTOBID_EVIDENCE_DIR（缺省 data/evidence）构造存储。
func FromEnv() *Store {
	root := strings.TrimSpace(os.Getenv("AUTOBID_EVIDENCE_DIR"))
	if root == "" {
		root = DefaultDir
	}
	return &Store{Root: root}
}

var reSafe = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

func safeSegment(s string) string {
	s = reSafe.ReplaceAllString(s, "_")
	s = strings.Trim(s, "._")
	if s == "" {
		return "default"
	}
	return s
}

// Abs 把数据库中的相对路径解析为绝对路径，并拒绝越界路径。
func (s *Store) Abs(rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) ||
		strings.Contains(rel, ":") || strings.Contains(rel, "..") {
		return "", fmt.Errorf("evidence: invalid original path %q", rel)
	}
	root, err := filepath.Abs(s.Root)
	if err != nil {
		return "", err
	}
	p := filepath.Join(root, filepath.FromSlash(rel))
	if !strings.HasPrefix(p, root+string(filepath.Separator)) {
		return "", fmt.Errorf("evidence: original path %q escapes the evidence directory", rel)
	}
	return p, nil
}

// Put 以内容寻址写入原件，返回相对路径与 SHA-256；同一内容重复写入幂等。
func (s *Store) Put(tenant, ext string, data []byte) (rel, sum string, err error) {
	sum = sha256Hex(data)
	ext = strings.TrimPrefix(strings.TrimSpace(ext), ".")
	ext = strings.ToLower(reSafe.ReplaceAllString(ext, ""))
	if ext == "" {
		ext = "bin"
	}
	rel = filepath.ToSlash(filepath.Join(safeSegment(tenant), "originals", sum+"."+ext))
	abs, err := s.Abs(rel)
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", "", err
	}
	if existing, err := os.ReadFile(abs); err == nil && sha256Hex(existing) == sum {
		return rel, sum, nil
	}
	if err := writeAtomic(abs, data); err != nil {
		return "", "", err
	}
	return rel, sum, nil
}

// Read 读取原件并校验 SHA-256（wantHash 为空时不校验）。
func (s *Store) Read(rel, wantHash string) ([]byte, error) {
	abs, err := s.Abs(rel)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	if wantHash != "" && sha256Hex(data) != wantHash {
		return nil, fmt.Errorf("evidence: %s content hash mismatch", rel)
	}
	return data, nil
}

// Check 返回原件是否可读以及哈希是否与登记值一致。路径不可读不等于哈希不符，由规则层区分 manual 与 inadmissible。
func (s *Store) Check(rel, wantHash string) (readable, match bool, err error) {
	_, readable, match, err = s.Inspect(rel, wantHash)
	return readable, match, err
}

// Inspect 区分文件不存在（exists=false）与存在但不可读 / 哈希不符。
func (s *Store) Inspect(rel, wantHash string) (exists, readable, match bool, err error) {
	if rel == "" || wantHash == "" {
		return false, false, false, nil
	}
	abs, err := s.Abs(rel)
	if err != nil {
		return false, false, false, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, false, false, nil
		}
		return true, false, false, nil
	}
	return true, true, sha256Hex(data) == wantHash, nil
}

func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	if err := errors.Join(werr, f.Sync(), f.Close()); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
