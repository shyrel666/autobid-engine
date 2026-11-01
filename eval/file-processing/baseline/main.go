// Command baseline 记录现有固定解析器的导入基线：逐文件解析并输出可重复的 JSON 摘要。
// 只用于 F0/F4 的工程对比，不访问网络，不写数据库。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/autobid/autobid-engine/ingest"
)

type fileResult struct {
	File       string   `json:"file"`
	SHA256     string   `json:"sha256"`
	Bytes      int      `json:"bytes"`
	Format     string   `json:"format,omitempty"`
	Parser     string   `json:"parser,omitempty"`
	Pages      int      `json:"pages,omitempty"`
	Chunks     int      `json:"chunks,omitempty"`
	Tables     int      `json:"tables,omitempty"`
	Mandatory  int      `json:"mandatory_chunks,omitempty"`
	Warnings   []string `json:"warnings,omitempty"`
	Error      string   `json:"error,omitempty"`
	DurationMS int64    `json:"duration_ms"`
}

type summary struct {
	Total   int            `json:"total"`
	OK      int            `json:"ok"`
	Failed  int            `json:"failed"`
	Chunks  int            `json:"chunks"`
	Tables  int            `json:"tables"`
	Pages   int            `json:"pages"`
	Formats map[string]int `json:"formats"`
}

type report struct {
	Dir     string       `json:"dir"`
	Files   []fileResult `json:"files"`
	Summary summary      `json:"summary"`
}

func main() {
	dir := flag.String("dir", "", "待扫描目录")
	flag.Parse()
	if strings.TrimSpace(*dir) == "" {
		fmt.Fprintln(os.Stderr, "-dir is required")
		os.Exit(2)
	}
	rep, err := run(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(dir string) (*report, error) {
	rep := &report{Dir: filepath.Clean(dir), Files: []fileResult{}, Summary: summary{Formats: map[string]int{}}}
	var paths []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".pdf", ".docx", ".txt", ".md", ".png", ".jpg", ".jpeg", ".tif", ".tiff", ".bmp", ".gif", ".webp", ".xlsx", ".csv", ".tsv", ".zip":
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	ocr := ingest.DefaultOCR()
	for _, path := range paths {
		rep.Files = append(rep.Files, parseOne(path, ocr))
	}
	for _, f := range rep.Files {
		rep.Summary.Total++
		rep.Summary.Chunks += f.Chunks
		rep.Summary.Tables += f.Tables
		rep.Summary.Pages += f.Pages
		if f.Error != "" {
			rep.Summary.Failed++
			continue
		}
		rep.Summary.OK++
		rep.Summary.Formats[f.Format]++
	}
	return rep, nil
}

func parseOne(path string, ocr ingest.OCRProvider) fileResult {
	start := time.Now()
	data, err := os.ReadFile(path)
	if err != nil {
		return fileResult{File: filepath.ToSlash(path), Error: err.Error(), DurationMS: time.Since(start).Milliseconds()}
	}
	sum := sha256.Sum256(data)
	res := fileResult{File: filepath.ToSlash(path), SHA256: hex.EncodeToString(sum[:]), Bytes: len(data)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	p, err := ingest.Parse(ctx, ingest.File{Name: filepath.Base(path), Data: data}, ingest.ParseOptions{OCR: ocr})
	res.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Format, res.Parser, res.Pages = p.Format, p.Meta.Parser, p.PageCount
	res.Chunks = len(p.Sections())
	res.Tables = len(p.Tables)
	for _, s := range p.Sections() {
		if s.Mandatory {
			res.Mandatory++
		}
	}
	res.Warnings = p.Meta.Warnings
	return res
}
