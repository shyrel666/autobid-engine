package db

// 知识库检索文本处理：PostgreSQL 内置分词器不切分中文（且本环境不依赖 zhparser / pg_jieba 扩展），
// 因此入库前由 Go 层把文本展开为可检索的词元串，再以 'simple' 配置生成 tsvector：
//   - CJK 连续片段：单字 + 相邻双字（bigram）；
//   - 拉丁字母/数字片段：按字母数字切词并小写。
// 查询时 CJK 词按 bigram 逐个 AND（等价于“包含该词”的近似），拉丁词做前缀匹配，词与词之间 OR，由 ts_rank_cd 排序。

import (
	"context"
	"hash/fnv"
	"math"
	"strconv"
	"strings"
	"unicode"
)

type tokenRun struct {
	cjk  bool
	text []rune
}

func isCJKRune(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.In(r, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
}

// runs 把文本切成 CJK 片段与字母数字片段，其他字符作为分隔符。
func runs(s string) []tokenRun {
	var (
		out []tokenRun
		cur tokenRun
	)
	flush := func() {
		if len(cur.text) > 0 {
			out = append(out, cur)
		}
		cur = tokenRun{}
	}
	for _, r := range strings.ToLower(s) {
		switch {
		case isCJKRune(r):
			if !cur.cjk {
				flush()
				cur.cjk = true
			}
			cur.text = append(cur.text, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if cur.cjk {
				flush()
			}
			cur.text = append(cur.text, r)
		default:
			flush()
		}
	}
	flush()
	return out
}

// KnowledgeTokens 返回用于建立全文索引的词元（CJK 单字 + 双字、拉丁/数字词）。
func KnowledgeTokens(s string) []string {
	var out []string
	for _, r := range runs(s) {
		if !r.cjk {
			out = append(out, string(r.text))
			continue
		}
		for i := range r.text {
			out = append(out, string(r.text[i]))
			if i+1 < len(r.text) {
				out = append(out, string(r.text[i:i+2]))
			}
		}
	}
	return out
}

// KnowledgeSearchText 是写入 search_title / search_body 的词元串。
func KnowledgeSearchText(parts ...string) string {
	return strings.Join(KnowledgeTokens(strings.Join(parts, " ")), " ")
}

// QueryTerms 把检索词按空白与常见中文标点切分并去重（保留原始大小写用于关键词命中计算）。
func QueryTerms(q string) []string {
	f := func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(",，;；、|/。:：()（）[]【】\"'“”", r)
	}
	seen := map[string]bool{}
	var out []string
	for _, t := range strings.FieldsFunc(q, f) {
		k := strings.ToLower(t)
		if !seen[k] && len(runs(t)) > 0 {
			seen[k] = true
			out = append(out, t)
		}
	}
	return out
}

func quoteLexeme(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// termQuery 把一个检索词编译为 tsquery 片段：CJK 片段取双字逐个 AND（单字片段取单字），拉丁片段前缀匹配。
func termQuery(term string) string {
	var parts []string
	for _, r := range runs(term) {
		switch {
		case !r.cjk:
			parts = append(parts, quoteLexeme(string(r.text))+":*")
		case len(r.text) == 1:
			parts = append(parts, quoteLexeme(string(r.text)))
		default:
			for i := 0; i+1 < len(r.text); i++ {
				parts = append(parts, quoteLexeme(string(r.text[i:i+2])))
			}
		}
	}
	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return "(" + strings.Join(parts, " & ") + ")"
}

// KnowledgeTSQuery 把检索词编译为 to_tsquery('simple', ...) 的输入；没有可检索词元时返回 ""。
func KnowledgeTSQuery(terms []string) string {
	var qs []string
	for _, t := range terms {
		if q := termQuery(t); q != "" {
			qs = append(qs, q)
		}
	}
	return strings.Join(qs, " | ")
}

// ---------------------------------------------------------------------------
// 向量
// ---------------------------------------------------------------------------

// Embedder 生成文本向量；Model 标识向量空间，只有同一 Model 的向量才会被比较。
// 可替换为外部 Embedding 服务实现；向量写入 knowledge_chunks.embedding（float8[]），由 kb_cosine 计算相似度。
type Embedder interface {
	Model() string
	Embed(ctx context.Context, texts []string) ([][]float64, error)
}

// QueryEmbedder 由非对称模型实现：检索查询与被检索文档使用不同编码（如百炼 text_type=query/document），二者仍在同一向量空间。
type QueryEmbedder interface {
	EmbedQuery(ctx context.Context, texts []string) ([][]float64, error)
}

// EmbedQuery 为检索查询生成向量：模型提供查询编码时使用它，否则与文档编码相同。
func EmbedQuery(ctx context.Context, e Embedder, texts []string) ([][]float64, error) {
	if q, ok := e.(QueryEmbedder); ok {
		return q.EmbedQuery(ctx, texts)
	}
	return e.Embed(ctx, texts)
}

// HashEmbedder 是无需外部服务的本地词袋哈希向量（hashing trick）：CJK 取双字、拉丁取词，
// 按 FNV 哈希映射到 Dim 维并带符号累加（1+log 词频），再做 L2 归一化。
// 它能在关键词不完全一致时（如“视频监控平台”与“监控视频平台”）提供部分重叠相似度，作为全文检索的补充召回。
type HashEmbedder struct {
	Dim int
}

// DefaultEmbedder 是知识库默认向量器。
func DefaultEmbedder() Embedder { return HashEmbedder{Dim: 256} }

func (h HashEmbedder) dim() int {
	if h.Dim <= 0 {
		return 256
	}
	return h.Dim
}

func (h HashEmbedder) Model() string { return "hash-bigram-" + strconv.Itoa(h.dim()) }

func (h HashEmbedder) Embed(_ context.Context, texts []string) ([][]float64, error) {
	out := make([][]float64, len(texts))
	for i, t := range texts {
		out[i] = h.vector(t)
	}
	return out, nil
}

func (h HashEmbedder) vector(text string) []float64 {
	n := h.dim()
	tf := map[string]int{}
	for _, r := range runs(text) {
		if !r.cjk {
			tf[string(r.text)]++
			continue
		}
		if len(r.text) == 1 {
			tf[string(r.text)]++
		}
		for i := 0; i+1 < len(r.text); i++ {
			tf[string(r.text[i:i+2])]++
		}
	}
	v := make([]float64, n)
	for tok, c := range tf {
		hs := fnv.New64a()
		_, _ = hs.Write([]byte(tok))
		x := hs.Sum64()
		sign := 1.0
		if x&(1<<63) != 0 {
			sign = -1
		}
		v[x%uint64(n)] += sign * (1 + math.Log(float64(c)))
	}
	var norm float64
	for _, x := range v {
		norm += x * x
	}
	if norm == 0 {
		return v
	}
	norm = math.Sqrt(norm)
	for i := range v {
		v[i] = math.Round(v[i]/norm*1e6) / 1e6
	}
	return v
}
