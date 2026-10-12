// R3-A：确定性 char n-gram TF-IDF embedder（不调 LLM）。
//
// 背景（R2 收口报告 §7.1）：评测器从未调用 SetEmbedder，signalSemantic 退化为
// RecallScore，导致 semantic 信号无法参与权重重校准。R3 注入一个确定性、
// 可复现、纯字符串处理的 char n-gram TF-IDF 向量化器，让 semantic 真正生效。
//
// 设计（对齐任务书 A.3.1）：
//   - 文本小写化 + 空白归一，生成字符 n-gram（默认 n=3，trigram）。
//   - 词表由调用方提供的 corpus（两个评测集所有 query + fact）一次性统计 IDF，
//     词表顺序按「首次出现顺序」固定，不使用 map 迭代序，不使用 time.Now()。
//   - 向量化：TF × IDF，L2 归一化；维度 = 词表大小（固定）。
//   - 空文本 / 无任何 n-gram → 返回全零向量（长度确定）。
//
// 红线：不调 LLM；seed 无关（无随机源）；不修改 Memory / RouterContext 语义。
package kernel

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
)

// CharNgramVocab 是 char n-gram TF-IDF 词表（IDF 预计算）。
//
// 线程安全：BuildIDF / Load 完成后词表只读，Embed 并发安全（无共享可变状态）。
type CharNgramVocab struct {
	N     int                // 语料文档数（IDF 分母）
	Ngram int                // n-gram 长度（默认 3）
	idf   map[string]float64 // n-gram -> IDF 权重
	order []string           // 词表顺序（首次出现顺序），保证 Embed 维度稳定
}

// vocabJSON 是 CharNgramVocab 的磁盘序列化格式。
// Order 保留词表顺序（首次出现顺序），保证 Load 后 Embed 维度与索引与 Build 时完全一致。
type vocabJSON struct {
	N     int                `json:"n"`
	Ngram int                `json:"ngram"`
	Order []string           `json:"order"`
	Terms map[string]float64 `json:"idf"`
}

// NewCharNgramVocab 返回一个空词表（n 为 n-gram 长度，<=0 时回退 3）。
// 需调用 BuildIDF（或 LoadCharNgramVocabJSON）后才可用于向量化。
func NewCharNgramVocab(n int) *CharNgramVocab {
	if n <= 0 {
		n = 3
	}
	return &CharNgramVocab{Ngram: n, idf: map[string]float64{}}
}

// NewCharNgramEmbedder 便捷构造：用 corpus 一次性建词表并返回向量化闭包。
// 闭包签名即 RankFusion.SetEmbedder 所接受的 func(string) ([]float32, error)。
func NewCharNgramEmbedder(n int, corpus []string) func(string) ([]float32, error) {
	v := NewCharNgramVocab(n)
	v.BuildIDF(corpus)
	return v.EmbedFn()
}

// BuildIDF 从 corpus 统计 IDF（词表按首次出现顺序固定）。
// 可重复调用：重复调用会重建词表（以最后一次 corpus 为准）。
//
// 确定性说明：词表顺序与 IDF 映射完全由 corpus 的文本顺序决定，
// 不使用 map 迭代序（R3 红线 5：seed=42 确定性），不使用 time.Now()。
func (v *CharNgramVocab) BuildIDF(corpus []string) {
	df := map[string]int{}
	seenOrder := []string{}
	seen := map[string]bool{}
	v.N = 0
	for _, text := range corpus {
		if text == "" {
			continue
		}
		grams := charNGramsOrdered(text, v.Ngram) // 有序：按出现顺序，非 map 迭代序
		if len(grams) == 0 {
			continue
		}
		v.N++
		for _, g := range grams {
			if !seen[g] {
				seen[g] = true
				seenOrder = append(seenOrder, g)
			}
			df[g]++
		}
	}
	// 平滑 IDF：ln(1 + N/df)，保证 df=N 时仍 >0（与 BM25 常用形式一致）。
	// 按 seenOrder 构建，保证词表顺序 order 与 idf 键集稳定。
	idf := make(map[string]float64, len(df))
	for _, g := range seenOrder {
		d, ok := df[g]
		if !ok {
			continue
		}
		idf[g] = math.Log(1 + float64(v.N)/float64(d))
	}
	v.idf = idf
	v.order = seenOrder
}

// Embed 把文本向量化为固定维度 float32 向量（TF-IDF 加权 + L2 归一化）。
// 空文本 / 空词表 / 无任何 n-gram 命中 → 全零向量（长度 = 词表大小，确定）。
func (v *CharNgramVocab) Embed(text string) []float32 {
	vec := make([]float32, len(v.order))
	if text == "" || len(v.order) == 0 || len(v.idf) == 0 {
		return vec
	}
	grams := charNGrams(text, v.Ngram)
	if len(grams) == 0 {
		return vec
	}
	var sq float64
	for i, g := range v.order {
		tf, ok := grams[g]
		if !ok {
			continue
		}
		idf, ok := v.idf[g]
		if !ok || idf <= 0 {
			continue
		}
		w := float64(tf) * idf
		vec[i] = float32(w)
		sq += w * w
	}
	if sq > 0 {
		norm := float32(math.Sqrt(sq))
		for i := range vec {
			vec[i] /= norm
		}
	}
	return vec
}

// EmbedFn 返回符合 SetEmbedder 签名的向量化闭包。
func (v *CharNgramVocab) EmbedFn() func(string) ([]float32, error) {
	return func(text string) ([]float32, error) {
		return v.Embed(text), nil
	}
}

// Dim 返回向量维度（词表大小）。
func (v *CharNgramVocab) Dim() int { return len(v.order) }

// SaveJSON 把词表序列化到路径（确定性：词表顺序写入 order）。
func (v *CharNgramVocab) SaveJSON(path string) error {
	b, err := json.MarshalIndent(vocabJSON{
		N:     v.N,
		Ngram: v.Ngram,
		Order: v.order,
		Terms: v.idf,
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// LoadCharNgramVocabJSON 从磁盘反序列化词表（供 tune 脚本与 run_kpi.go 共享同一 IDF）。
// 词表顺序从磁盘 order 字段恢复；若旧格式无 order 字段，按排序后固定（降级但仍确定性）。
func LoadCharNgramVocabJSON(path string) (*CharNgramVocab, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc vocabJSON
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse vocab %s: %w", path, err)
	}
	n := doc.Ngram
	if n <= 0 {
		n = 3
	}
	v := &CharNgramVocab{N: doc.N, Ngram: n, idf: doc.Terms}
	if len(doc.Order) == len(doc.Terms) {
		v.order = doc.Order
	} else {
		// 降级：旧格式无 order，按排序固定。
		v.order = make([]string, 0, len(doc.Terms))
		for g := range doc.Terms {
			v.order = append(v.order, g)
		}
		sort.Strings(v.order)
	}
	return v, nil
}

// charNGrams 生成文本的 char n-gram 集合（rune 级，含空白）。
// 返回按「首次出现顺序」排列的 n-gram 列表（含重复计数）。
//
// ⚠️ 确定性：不使用 map 迭代序（R3 红线 5）。调用方应依赖返回顺序固定词表。
func charNGramsOrdered(text string, n int) []string {
	text = normalizeEmbedText(text)
	if text == "" || n <= 0 {
		return nil
	}
	rs := []rune(text)
	if len(rs) < n {
		return nil
	}
	out := make([]string, 0, len(rs)-n+1)
	for i := 0; i+n <= len(rs); i++ {
		out = append(out, string(rs[i:i+n]))
	}
	return out
}

// charNGrams 生成文本的 char n-gram 集合（rune 级，含空白）。
// 返回 map[n-gram]tf（同一 n-gram 在文本中的出现次数）。
//
// 兼容保留：仅用于 TF 计数；词表顺序请用 charNGramsOrdered（确定性）。
func charNGrams(text string, n int) map[string]int {
	text = normalizeEmbedText(text)
	if text == "" || n <= 0 {
		return nil
	}
	rs := []rune(text)
	if len(rs) < n {
		// 短文本：不足 n 个 rune 时无 n-gram，返回空（→ 全零向量）。
		return nil
	}
	out := make(map[string]int, len(rs)-n+1)
	for i := 0; i+n <= len(rs); i++ {
		out[string(rs[i:i+n])]++
	}
	return out
}

// normalizeEmbedText 小写化并压缩空白（与 signalKeywordBM25 的 tokenize 预处理一致）。
func normalizeEmbedText(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.Join(strings.Fields(s), " ")
}
