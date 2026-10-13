// R5-A：静态向量 Embedder——BGE-m3 确定性嵌入的磁盘回放实现。
//
// 背景（R5-A 任务书 §A.1 / §A.3.1）：
//   - R4-D 已预留 kernel.Embedder 接口（Embed/Dim/Name），但 Go 进程内
//     无 onnxruntime / llama.cpp 可用（FreeBSD 无法安装 onnxruntime wheel）。
//   - 任务书 §5.4「风险与降级」明确允许：**用静态向量文件（.npy + .json 词向量）
//     模拟向量模型**，或明确报告"向量模型未就绪，-embedder vector 降级 tfidf"。
//   - 本实现采用降级路径：远端 0.88 上用 torch+transformers 离线加载 BAAI/bge-m3，
//     对评测集 query/fact 文本生成确定性嵌入，落盘 .npy；Go 侧读取该静态向量文件，
//     实现"确定性嵌入 + 余弦相似度"的 semantic 信号。**不调 LLM**（红线 4 例外条款：
//     bge 嵌入属"确定性嵌入"而非 LLM 语义推理/排序/事实判断）。
//
// 确定性红线（任务书 §0 红线 5）：
//   - 模型文件路径固定（/tmp/bge_m3_weights.pt + vocab/），推理参数固定；
//   - Embed() 输出逐位一致（同一文本必得同一向量），无 time.Now()、无 map 迭代序；
//   - 未在向量表中出现的文本返回全零向量（长度确定 = Dim）。
//
// 降级语义（任务书 §A.3.1）：
//   - 模型文件缺失时报错并提示回退 `-embedder tfidf`；
//   - Name() 返回 "vector:bge-m3"（模型标识）。
//
// Schema 红线（红线 1）：只增不改——本文件不修改 Memory / RouterContext / FusedResult
// 字段语义，仅新增一个 Embedder 实现。
package kernel

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
)

// VectorEmbedderConfig 静态向量 embedder 的加载配置。
type VectorEmbedderConfig struct {
	// MatrixPath .npy 文件路径（float32，形状 [N, Dim]，行序与 MetaIndex 一致）。
	MatrixPath string
	// MetaPath 元数据 JSON 路径（VectorMeta）。
	MetaPath string
	// IndexPath 文本索引 JSONL 路径（可选）：把 text -> 向量行号 映射落盘，
	// 避免每次启动都全量扫描（大库时）。空则仅用 MetaPath 内的 index 映射。
	IndexPath string
}

// VectorMeta 是静态向量文件的元数据（与远端生成脚本约定一致）。
type VectorMeta struct {
	Model       string         `json:"model"`
	Dim         int            `json:"dim"`
	Count       int            `json:"count"`
	Pooling     string         `json:"pooling"`
	Normalized  bool           `json:"normalized"`
	QueryPrefix string         `json:"query_prefix"`
	GeneratedAt string         `json:"generated_at"`
	ElapsedS    float64        `json:"elapsed_s"`
	Texts       []string       `json:"texts,omitempty"`
	TextToIndex map[string]int `json:"text_to_index,omitempty"`
}

// VectorEmbedder 是静态向量 embedder（实现 kernel.Embedder 接口）。
//
// 线程安全：加载完成后所有字段只读；Embed 只做 map 查表 + 向量拷贝，并发安全。
type VectorEmbedder struct {
	dim    int
	model  string
	normal bool
	// matrix 是 [N, Dim] float32 的扁平存储（行主序）。
	matrix []float32
	n      int
	// index 是 text -> 行号 的确定性映射（同文本必映射到同一行）。
	index map[string]int
	// prefix 是 query 前缀（BGE 指令），用于 Embed 前的文本预处理。
	prefix string
}

// NewVectorEmbedder 从磁盘加载静态向量文件并构造 embedder。
// 任一文件缺失 / 解析失败都返回错误（调用方应回退 `-embedder tfidf`）。
func NewVectorEmbedder(cfg VectorEmbedderConfig) (*VectorEmbedder, error) {
	// 1) 元数据
	metaRaw, err := os.ReadFile(cfg.MetaPath)
	if err != nil {
		return nil, fmt.Errorf("vector embedder: read meta %s: %w", cfg.MetaPath, err)
	}
	var meta VectorMeta
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		return nil, fmt.Errorf("vector embedder: parse meta %s: %w", cfg.MetaPath, err)
	}
	if meta.Dim <= 0 || meta.Count <= 0 {
		return nil, fmt.Errorf("vector embedder: meta dim=%d count=%d invalid", meta.Dim, meta.Count)
	}

	// 2) 向量矩阵
	mat, err := readNpyFloat32(cfg.MatrixPath, meta.Count, meta.Dim)
	if err != nil {
		return nil, fmt.Errorf("vector embedder: load matrix %s: %w", cfg.MatrixPath, err)
	}

	// 3) 文本索引（优先用 IndexPath，其次 MetaPath 内的 Texts/TextToIndex）
	idx := make(map[string]int, meta.Count)
	if cfg.IndexPath != "" {
		loaded, lerr := loadTextIndex(cfg.IndexPath, meta.Count)
		if lerr != nil {
			return nil, fmt.Errorf("vector embedder: load index %s: %w", cfg.IndexPath, lerr)
		}
		idx = loaded
	} else if len(meta.Texts) == meta.Count {
		for i, t := range meta.Texts {
			if _, dup := idx[t]; !dup {
				idx[t] = i
			}
		}
	} else if len(meta.TextToIndex) > 0 {
		for t, i := range meta.TextToIndex {
			if i >= 0 && i < meta.Count {
				if _, dup := idx[t]; !dup {
					idx[t] = i
				}
			}
		}
	}

	name := "vector:" + meta.Model
	if meta.Model == "" {
		name = "vector:static"
	}
	return &VectorEmbedder{
		dim:    meta.Dim,
		model:  name,
		normal: meta.Normalized,
		matrix: mat,
		n:      meta.Count,
		index:  idx,
		prefix: meta.QueryPrefix,
	}, nil
}

// Embed 把文本向量化。查表命中返回对应向量；未命中返回全零向量（长度确定 = Dim）。
// 空文本返回全零向量（与 CharNgramVocab.Embed 语义一致）。
// 无随机源、无 time.Now()，同一输入逐位一致（确定性）。
func (v *VectorEmbedder) Embed(text string) ([]float32, error) {
	if v == nil {
		return nil, nil
	}
	vec := make([]float32, v.dim)
	if v.dim <= 0 || text == "" {
		return vec, nil
	}
	key := v.normalizeKey(text)
	row, ok := v.index[key]
	if !ok || row < 0 || row >= v.n {
		return vec, nil
	}
	copy(vec, v.matrix[row*v.dim:(row+1)*v.dim])
	return vec, nil
}

// Dim 返回向量维度。
func (v *VectorEmbedder) Dim() int {
	if v == nil {
		return 0
	}
	return v.dim
}

// Name 返回实现名（"vector:bge-m3"）。
func (v *VectorEmbedder) Name() string {
	if v == nil {
		return "vector:nil"
	}
	return v.model
}

// Count 返回已加载向量数（供诊断日志）。
func (v *VectorEmbedder) Count() int {
	if v == nil {
		return 0
	}
	return v.n
}

// normalizeKey 归一化查表键：小写 + 压缩空白（与远端生成脚本的预处理保持一致）。
// BGE 的 query 前缀不参与归一化（远端生成时对 query 加了前缀；此处查表用原文归一化键）。
func (v *VectorEmbedder) normalizeKey(s string) string {
	return normalizeVectorKey(s)
}

// normalizeVectorKey 是包级键归一化（供 loadTextIndex 与 Embed 共用，
// 保证索引文件与查表键一致——否则 index.jsonl 用原文键而 Embed 用归一化键，永远 miss）。
func normalizeVectorKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.Join(strings.Fields(s), " ")
}

// CosineSimilarity 计算两个向量的余弦相似度（导出供评测器/诊断复用）。
// 与 rank_fusion.go 内部 cosine 函数的公式一致（后者未导出）。
// 纯算术，无随机源，确定性。
func CosineSimilarity(a, b []float32) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := 0; i < n; i++ {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// readNpyFloat32 读取 .npy 文件为 [rows, cols] float32 扁平数组。
// 支持 numpy 1.x/2.x 的 '<f4' 格式（无压缩）；其他格式返回错误。
func readNpyFloat32(path string, rows, cols int) ([]float32, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 128 {
		return nil, fmt.Errorf("npy too small: %d bytes", len(data))
	}
	// NPY 魔数（6 字节）
	if len(data) < 128 {
		return nil, fmt.Errorf("npy too small: %d bytes", len(data))
	}
	if data[0] != 0x93 || data[1] != 'N' || data[2] != 'U' || data[3] != 'M' || data[4] != 'P' || data[5] != 'Y' {
		return nil, fmt.Errorf("npy magic mismatch")
	}
	// 版本号在 magic（6 字节）之后：字节 6=verMajor, 7=verMinor。
	verMaj := data[6]
	_ = data[7] // verMinor
	headerLen := 0
	hdrStart := 0
	switch verMaj {
	case 1:
		// V1：magic(6) + ver(2) + header_len(uint16, LE) → header 起始偏移 10。
		if len(data) < 10 {
			return nil, fmt.Errorf("npy header truncated")
		}
		headerLen = int(binary.LittleEndian.Uint16(data[8:10]))
		hdrStart = 10
	case 2, 3:
		// V2/V3：magic(6) + ver(2) + header_len(uint32, LE) → header 起始偏移 12。
		if len(data) < 12 {
			return nil, fmt.Errorf("npy header truncated")
		}
		headerLen = int(binary.LittleEndian.Uint32(data[8:12]))
		hdrStart = 12
	default:
		return nil, fmt.Errorf("unsupported npy version %d", verMaj)
	}
	if hdrStart == 0 {
		return nil, fmt.Errorf("npy header start not resolved")
	}
	headerEnd := hdrStart + headerLen
	if headerEnd > len(data) {
		return nil, fmt.Errorf("npy header overruns file")
	}
	hdr := string(data[hdrStart:headerEnd])
	if !strings.Contains(hdr, "<f4") && !strings.Contains(hdr, "|f4") {
		return nil, fmt.Errorf("npy dtype not little-endian float32: %s", preview(hdr, 120))
	}
	payload := data[headerEnd:]
	want := rows * cols * 4
	if len(payload) < want {
		return nil, fmt.Errorf("npy payload too small: have %d want %d", len(payload), want)
	}
	out := make([]float32, rows*cols)
	for i := 0; i < rows*cols; i++ {
		bits := binary.LittleEndian.Uint32(payload[i*4 : i*4+4])
		out[i] = math.Float32frombits(bits)
	}
	return out, nil
}

// loadTextIndex 从 JSONL（每行 {"text":..., "index":n}）加载 text -> 行号 映射。
func loadTextIndex(path string, count int) (map[string]int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	idx := make(map[string]int)
	lines := strings.Split(string(raw), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec struct {
			Text  string `json:"text"`
			Index int    `json:"index"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue // 容错：跳过格式异常行（索引完整性由调用方校验）
		}
		if rec.Text == "" || rec.Index < 0 || rec.Index >= count {
			continue
		}
		if _, dup := idx[normalizeVectorKey(rec.Text)]; !dup {
			idx[normalizeVectorKey(rec.Text)] = rec.Index
		}
	}
	return idx, nil
}

// defaultVectorPaths 返回评测器默认的静态向量文件路径（相对 eval/ 目录）。
// 与远端生成脚本的输出路径保持一致。
func defaultVectorPaths() (matrix, meta, index string) {
	dir := "vectors"
	return filepath.Join(dir, "vectors.npy"),
		filepath.Join(dir, "vectors_meta.json"),
		filepath.Join(dir, "vectors_index.jsonl")
}

func preview(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
