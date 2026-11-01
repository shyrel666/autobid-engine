package ingest

// ZIP 招标包：先列清单再受限展开。拒绝绝对路径、盘符、路径穿越、反斜杠、链接、重复规范化路径、
// 加密条目与嵌套压缩包；条目数、单条字节、累计字节与压缩比在读取过程中检查。

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// ArchiveLimits 是 ZIP 展开限额；零值字段由 DefaultArchiveLimits 补齐。
type ArchiveLimits struct {
	MaxEntries    int
	MaxTotalBytes int64
	MaxEntryBytes int64
	MaxRatio      float64
}

// DefaultArchiveLimits 返回方案 §6.2 的首轮建议值。
func DefaultArchiveLimits() ArchiveLimits {
	return ArchiveLimits{MaxEntries: 200, MaxTotalBytes: 256 << 20, MaxEntryBytes: 64 << 20, MaxRatio: 100}
}

func (l ArchiveLimits) normalized() ArchiveLimits {
	d := DefaultArchiveLimits()
	if l.MaxEntries <= 0 {
		l.MaxEntries = d.MaxEntries
	}
	if l.MaxTotalBytes <= 0 {
		l.MaxTotalBytes = d.MaxTotalBytes
	}
	if l.MaxEntryBytes <= 0 {
		l.MaxEntryBytes = d.MaxEntryBytes
	}
	if l.MaxRatio <= 0 {
		l.MaxRatio = d.MaxRatio
	}
	return l
}

// ArchiveError 是带稳定错误码的 ZIP 边界错误（供 API 与页面显示明确原因）。
type ArchiveError struct {
	Code   string `json:"code"`
	Path   string `json:"path,omitempty"`
	Detail string `json:"detail,omitempty"`
}

func (e *ArchiveError) Error() string {
	if e.Path != "" {
		return e.Code + ": " + e.Path
	}
	if e.Detail != "" {
		return e.Code + ": " + e.Detail
	}
	return e.Code
}

// 稳定错误码（F0 expected.json 与 F1 测试共用）。
const (
	ArchiveInvalid        = "archive_invalid"
	ArchiveEncrypted      = "archive_encrypted"
	ArchivePathTraversal  = "archive_path_traversal"
	ArchiveAbsolutePath   = "archive_absolute_path"
	ArchiveDrivePath      = "archive_drive_path"
	ArchiveBackslash      = "archive_backslash"
	ArchiveSymlink        = "archive_symlink"
	ArchiveSpecialFile    = "archive_special_file"
	ArchiveDuplicatePath  = "archive_duplicate_path"
	ArchiveNested         = "archive_nested"
	ArchiveEntryLimit     = "archive_entry_limit"
	ArchiveTotalSizeLimit = "archive_total_size_limit"
	ArchiveEntrySizeLimit = "archive_entry_size_limit"
	ArchiveRatioLimit     = "archive_ratio_limit"
)

// ArchiveEntry 是一条已列出的普通文件；展开成功后含 SHA-256、格式与媒体类型。
type ArchiveEntry struct {
	Path           string  `json:"path"`
	Size           int64   `json:"size"`
	CompressedSize int64   `json:"compressed_size"`
	Ratio          float64 `json:"ratio,omitempty"`
	SHA256         string  `json:"sha256,omitempty"`
	Format         string  `json:"format,omitempty"`
	MediaType      string  `json:"media_type,omitempty"`
	// FileID 仅在服务端登记子文件后回填；ingest 包本身不依赖数据库。
	FileID int64 `json:"file_id,omitempty"`
}

// Archive 是 ZIP 清单与展开摘要。
type Archive struct {
	Entries    []ArchiveEntry `json:"entries"`
	TotalBytes int64          `json:"total_bytes"`
	Warnings   []string       `json:"warnings,omitempty"`
}

var (
	driveName   = regexp.MustCompile(`^[A-Za-z]:`)
	nestedExts  = map[string]bool{".zip": true, ".rar": true, ".7z": true, ".tar": true, ".gz": true, ".bz2": true, ".xz": true, ".tgz": true}
	ratioMinLen = int64(64 << 10)
)

// ParseArchive 校验并列出 ZIP；不把子文件写入正式文档，仅返回清单、哈希与格式。
func ParseArchive(data []byte, limits ArchiveLimits) (*Archive, error) {
	return WalkArchive(data, limits, nil)
}

// WalkArchive 校验并逐条展开；fn 收到完整条目字节（单条受限）。fn 为 nil 时只列清单并计算哈希。
func WalkArchive(data []byte, limits ArchiveLimits, fn func(*ArchiveEntry, []byte) error) (*Archive, error) {
	limits = limits.normalized()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, &ArchiveError{Code: ArchiveInvalid, Detail: err.Error()}
	}
	files := 0
	for _, f := range zr.File {
		if !strings.HasSuffix(f.Name, "/") {
			files++
		}
	}
	if files > limits.MaxEntries {
		return nil, &ArchiveError{Code: ArchiveEntryLimit, Detail: fmt.Sprintf("%d > %d", files, limits.MaxEntries)}
	}
	out := &Archive{Entries: []ArchiveEntry{}}
	seen := map[string]bool{}
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "/") {
			// 目录不展开，但目录名也必须通过同一套路径校验（拒绝 ../、盘符、绝对路径）。
			if _, err := validateArchivePath(strings.TrimSuffix(f.Name, "/")); err != nil {
				return nil, err
			}
			continue
		}
		name, err := validateArchivePath(f.Name)
		if err != nil {
			return nil, err
		}
		key := strings.ToLower(name)
		if seen[key] {
			return nil, &ArchiveError{Code: ArchiveDuplicatePath, Path: name}
		}
		seen[key] = true
		if f.Flags&0x1 != 0 {
			return nil, &ArchiveError{Code: ArchiveEncrypted, Path: name}
		}
		mode := f.Mode()
		if mode&os.ModeSymlink != 0 {
			return nil, &ArchiveError{Code: ArchiveSymlink, Path: name}
		}
		if mode&os.ModeType != 0 {
			return nil, &ArchiveError{Code: ArchiveSpecialFile, Path: name}
		}
		if nestedExts[strings.ToLower(filepath.Ext(name))] {
			return nil, &ArchiveError{Code: ArchiveNested, Path: name}
		}
		if int64(f.UncompressedSize64) > limits.MaxEntryBytes {
			return nil, &ArchiveError{Code: ArchiveEntrySizeLimit, Path: name,
				Detail: fmt.Sprintf("%d > %d", f.UncompressedSize64, limits.MaxEntryBytes)}
		}
		if f.UncompressedSize64 >= uint64(ratioMinLen) && f.CompressedSize64 > 0 {
			ratio := float64(f.UncompressedSize64) / float64(f.CompressedSize64)
			if ratio > limits.MaxRatio {
				return nil, &ArchiveError{Code: ArchiveRatioLimit, Path: name, Detail: fmt.Sprintf("%.1f > %.1f", ratio, limits.MaxRatio)}
			}
		}
		rc, err := f.Open()
		if err != nil {
			return nil, &ArchiveError{Code: ArchiveInvalid, Path: name, Detail: err.Error()}
		}
		content, err := io.ReadAll(io.LimitReader(rc, limits.MaxEntryBytes+1))
		rc.Close()
		if err != nil {
			return nil, &ArchiveError{Code: ArchiveInvalid, Path: name, Detail: err.Error()}
		}
		if int64(len(content)) > limits.MaxEntryBytes {
			return nil, &ArchiveError{Code: ArchiveEntrySizeLimit, Path: name}
		}
		out.TotalBytes += int64(len(content))
		if out.TotalBytes > limits.MaxTotalBytes {
			return nil, &ArchiveError{Code: ArchiveTotalSizeLimit, Detail: fmt.Sprintf("%d > %d", out.TotalBytes, limits.MaxTotalBytes)}
		}
		sum := sha256.Sum256(content)
		format := DetectFormat(name, "", content)
		if format == FormatZIP {
			return nil, &ArchiveError{Code: ArchiveNested, Path: name}
		}
		if format == "" {
			format = "unsupported"
		}
		entry := ArchiveEntry{Path: name, Size: int64(len(content)), CompressedSize: int64(f.CompressedSize64),
			SHA256: hex.EncodeToString(sum[:]), Format: format, MediaType: mediaTypeFor(format, name)}
		if f.CompressedSize64 > 0 {
			entry.Ratio = float64(f.UncompressedSize64) / float64(f.CompressedSize64)
		}
		if fn != nil {
			if err := fn(&entry, content); err != nil {
				return nil, err
			}
		}
		out.Entries = append(out.Entries, entry)
	}
	return out, nil
}

func validateArchivePath(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", &ArchiveError{Code: ArchiveInvalid, Detail: "empty entry name"}
	}
	if strings.ContainsAny(raw, "\\\x00") {
		return "", &ArchiveError{Code: ArchiveBackslash, Path: raw}
	}
	if strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return "", &ArchiveError{Code: ArchiveAbsolutePath, Path: raw}
	}
	if driveName.MatchString(raw) {
		return "", &ArchiveError{Code: ArchiveDrivePath, Path: raw}
	}
	clean := path.Clean(raw)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return "", &ArchiveError{Code: ArchivePathTraversal, Path: raw}
	}
	return clean, nil
}

func mediaTypeFor(format, name string) string {
	if m, ok := formatMIME[format]; ok {
		return m
	}
	if ext := filepath.Ext(name); ext != "" {
		if m := mime.TypeByExtension(ext); m != "" {
			return m
		}
	}
	return "application/octet-stream"
}

// zipEntryNames 只读取 ZIP 中央目录，用于格式嗅探（不展开内容）。
func zipEntryNames(data []byte) (map[string]bool, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(zr.File))
	for _, f := range zr.File {
		out[strings.ToLower(f.Name)] = true
	}
	return out, nil
}
