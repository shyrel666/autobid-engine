package ingest

// 可插拔 OCR：扫描图片与无文本层的 PDF 页面经 OCRProvider 识别为带坐标的文本行。
// 默认实现按环境依次选择：AUTOBID_OCR_URL（HTTP 服务）→ PATH 中的 tesseract 命令 → 不可用（明确报错，不伪造结果）。

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ErrOCRUnavailable 表示未配置可用的 OCR 引擎。
var ErrOCRUnavailable = errors.New("ingest: OCR is not available (set AUTOBID_OCR_URL or install tesseract with chi_sim)")

// OCRProvider 识别一张图片（一页）。BBox 为像素坐标 [x0, y0, x1, y1]（原点左上）。
type OCRProvider interface {
	Name() string
	Recognize(ctx context.Context, image []byte, mime string) (*OCRPage, error)
}

type OCRPage struct {
	Width  float64   `json:"width"`
	Height float64   `json:"height"`
	Lines  []OCRLine `json:"lines"`
}

type OCRLine struct {
	Text       string     `json:"text"`
	BBox       [4]float64 `json:"bbox"`
	Confidence float64    `json:"confidence"`
}

// DefaultOCR 按环境变量与本机命令选择 OCR 引擎：
// AUTOBID_OCR_URL 非空时使用 HTTPOCR；否则 PATH 中存在 tesseract（或 AUTOBID_TESSERACT 指定路径）时使用 TesseractOCR
// （语言 AUTOBID_OCR_LANG，默认 chi_sim+eng）；都不可用时返回 UnavailableOCR。
func DefaultOCR() OCRProvider {
	if url := strings.TrimSpace(os.Getenv("AUTOBID_OCR_URL")); url != "" {
		return &HTTPOCR{URL: url, Token: os.Getenv("AUTOBID_OCR_TOKEN")}
	}
	bin := strings.TrimSpace(os.Getenv("AUTOBID_TESSERACT"))
	if bin == "" {
		bin = "tesseract"
	}
	if path, err := exec.LookPath(bin); err == nil {
		lang := strings.TrimSpace(os.Getenv("AUTOBID_OCR_LANG"))
		if lang == "" {
			lang = "chi_sim+eng"
		}
		return &TesseractOCR{Path: path, Lang: lang}
	}
	return UnavailableOCR{}
}

// UnavailableOCR 是兜底实现：总是返回 ErrOCRUnavailable。
type UnavailableOCR struct{}

func (UnavailableOCR) Name() string { return "unavailable" }
func (UnavailableOCR) Recognize(context.Context, []byte, string) (*OCRPage, error) {
	return nil, ErrOCRUnavailable
}

// FallbackOCR 依次尝试多个引擎，返回第一个成功的结果。
type FallbackOCR []OCRProvider

func (f FallbackOCR) Name() string {
	names := make([]string, 0, len(f))
	for _, p := range f {
		names = append(names, p.Name())
	}
	return strings.Join(names, ",")
}

func (f FallbackOCR) Recognize(ctx context.Context, image []byte, mime string) (*OCRPage, error) {
	errs := []error{}
	for _, p := range f {
		page, err := p.Recognize(ctx, image, mime)
		if err == nil {
			return page, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", p.Name(), err))
	}
	if len(errs) == 0 {
		return nil, ErrOCRUnavailable
	}
	return nil, errors.Join(errs...)
}

// HTTPOCR 把图片以原始字节 POST 到 OCR 服务（Content-Type 为图片 MIME），响应体为 OCRPage JSON。
type HTTPOCR struct {
	URL    string
	Token  string
	Client *http.Client
}

func (h *HTTPOCR) Name() string { return "http" }

func (h *HTTPOCR) Recognize(ctx context.Context, image []byte, mime string) (*OCRPage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(image))
	if err != nil {
		return nil, err
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	req.Header.Set("Content-Type", mime)
	if h.Token != "" {
		req.Header.Set("Authorization", "Bearer "+h.Token)
	}
	c := h.Client
	if c == nil {
		c = &http.Client{Timeout: 2 * time.Minute}
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ocr request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("ocr service: HTTP %d: %s", resp.StatusCode, truncateRunes(string(body), 200))
	}
	var page OCRPage
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("ocr service: decode response: %w", err)
	}
	return &page, nil
}

// TesseractOCR 调用 tesseract 命令行（stdin 输入、TSV 输出），按 block/par/line 聚合单词为行。
type TesseractOCR struct {
	Path string
	Lang string
}

func (t *TesseractOCR) Name() string { return "tesseract" }

func (t *TesseractOCR) Recognize(ctx context.Context, image []byte, _ string) (*OCRPage, error) {
	cmd := exec.CommandContext(ctx, t.Path, "stdin", "stdout", "-l", t.Lang, "--psm", "3", "tsv")
	cmd.Stdin = bytes.NewReader(image)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("tesseract: %w: %s", err, truncateRunes(stderr.String(), 200))
	}
	return ParseTesseractTSV(stdout.Bytes())
}

// ParseTesseractTSV 解析 tesseract TSV 输出（level 5 为单词）。
func ParseTesseractTSV(tsv []byte) (*OCRPage, error) {
	r := csv.NewReader(bytes.NewReader(tsv))
	r.Comma, r.FieldsPerRecord, r.LazyQuotes = '\t', -1, true
	rows, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("tesseract tsv: %w", err)
	}
	type lineAcc struct {
		key   [3]int
		words []string
		box   [4]float64
		conf  float64
		n     int
	}
	page := &OCRPage{}
	acc := map[[3]int]*lineAcc{}
	var order [][3]int
	for i, row := range rows {
		if i == 0 || len(row) < 12 {
			continue
		}
		num := func(k int) float64 { v, _ := strconv.ParseFloat(strings.TrimSpace(row[k]), 64); return v }
		level := int(num(0))
		left, top, w, h := num(6), num(7), num(8), num(9)
		if level == 1 {
			page.Width, page.Height = w, h
			continue
		}
		text := strings.TrimSpace(row[11])
		if level != 5 || text == "" {
			continue
		}
		key := [3]int{int(num(2)), int(num(3)), int(num(4))}
		a, ok := acc[key]
		if !ok {
			a = &lineAcc{key: key, box: [4]float64{left, top, left + w, top + h}}
			acc[key] = a
			order = append(order, key)
		}
		a.words = append(a.words, text)
		a.box = [4]float64{min(a.box[0], left), min(a.box[1], top), max(a.box[2], left+w), max(a.box[3], top+h)}
		if c := num(10); c >= 0 {
			a.conf += c
			a.n++
		}
	}
	// tesseract 按阅读顺序输出单词，行顺序即首个单词出现的顺序。
	for _, k := range order {
		a := acc[k]
		l := OCRLine{Text: joinWords(a.words), BBox: a.box}
		if a.n > 0 {
			l.Confidence = a.conf / float64(a.n) / 100
		}
		page.Lines = append(page.Lines, l)
	}
	return page, nil
}

// joinWords 拼接 OCR 单词：两侧均为 CJK 时不加空格。
func joinWords(words []string) string {
	var b strings.Builder
	for i, w := range words {
		if i > 0 {
			prev, _ := utf8.DecodeLastRuneInString(words[i-1])
			next, _ := utf8.DecodeRuneInString(w)
			if !(isCJK(prev) || isCJK(next)) {
				b.WriteByte(' ')
			}
		}
		b.WriteString(w)
	}
	return b.String()
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.In(r, unicode.Hiragana, unicode.Katakana, unicode.Hangul) ||
		(r >= 0x3000 && r <= 0x303F) || (r >= 0xFF00 && r <= 0xFFEF)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
