// R5-A：静态向量 embedder 单元测试。
// 覆盖：NPY 解析、元数据加载、Embed 确定性（同文本逐位一致）、未命中降级全零、
//
//	CosineSimilarity 正确性（正交=0、同向=1、反向=-1）。
//
// 不调 LLM；不依赖真实模型文件（用生成的迷你夹具验证读取逻辑）。
package kernel

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// writeMiniNpy 生成一个 [rows, cols] float32 的 NPY 文件（V1 头，<f4）。
// 对齐规则与 numpy 一致：10 + headerLen 为 64 的倍数。
func writeMiniNpy(t *testing.T, path string, rows, cols int, data []float32) {
	t.Helper()
	header := "{'descr': '<f4', 'fortran_order': False, 'shape': (" +
		itoa(rows) + ", " + itoa(cols) + "), }"
	pad := (64 - ((10 + len(header)) % 64)) % 64
	headerLen := len(header) + pad
	buf := make([]byte, 0, 128+headerLen+rows*cols*4)
	// NPY 魔数固定 6 字节 "\x93NUMPY" + 版本 2 字节 + header_len 2 字节 = 10 字节头前缀。
	buf = append(buf, 0x93, 'N', 'U', 'M', 'P', 'Y', 0x01, 0x00) // magic(6) + v1.0
	var hl [2]byte
	binary.LittleEndian.PutUint16(hl[:], uint16(headerLen))
	buf = append(buf, hl[:]...)
	buf = append(buf, []byte(header)...)
	for i := 0; i < pad; i++ {
		buf = append(buf, ' ')
	}
	for _, v := range data {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], math.Float32bits(v))
		buf = append(buf, b[:]...)
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatalf("write npy: %v", err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		return "-" + string(digits)
	}
	return string(digits)
}

func writeMiniMeta(t *testing.T, path string, texts []string) VectorMeta {
	t.Helper()
	idx := map[string]int{}
	for i, s := range texts {
		if _, dup := idx[s]; !dup {
			idx[s] = i
		}
	}
	meta := VectorMeta{
		Model:       "bge-m3",
		Dim:         4,
		Count:       len(texts),
		Normalized:  true,
		QueryPrefix: "Represent this sentence for searching relevant passages: ",
		Texts:       texts,
	}
	raw, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write meta: %v", err)
	}
	return meta
}

func TestVectorEmbedder_LoadAndEmbed(t *testing.T) {
	dir := t.TempDir()
	texts := []string{
		"What does Andrew view his pets as?",
		"Family",
		"客户A的偏好是什么",
		"客户A的偏好是周一",
	}
	// 构造 4 个手工向量（4 维）：
	//   v0 = (1,0,0,0)   query 锚点
	//   v1 = (0.8,0.6,0,0) 与 v0 高相似（预期 top）
	//   v2 = (0,0,1,0)  正交
	//   v3 = (-1,0,0,0) 反向
	data := []float32{1, 0, 0, 0, 0.8, 0.6, 0, 0, 0, 0, 1, 0, -1, 0, 0, 0}
	npyPath := filepath.Join(dir, "vec.npy")
	metaPath := filepath.Join(dir, "meta.json")
	writeMiniNpy(t, npyPath, 4, 4, data)
	writeMiniMeta(t, metaPath, texts)

	ve, err := NewVectorEmbedder(VectorEmbedderConfig{MatrixPath: npyPath, MetaPath: metaPath})
	if err != nil {
		t.Fatalf("NewVectorEmbedder: %v", err)
	}
	if ve.Dim() != 4 {
		t.Fatalf("Dim = %d, want 4", ve.Dim())
	}
	if ve.Count() != 4 {
		t.Fatalf("Count = %d, want 4", ve.Count())
	}
	if ve.Name() != "vector:bge-m3" {
		t.Fatalf("Name = %s, want vector:bge-m3", ve.Name())
	}

	// 确定性：同文本多次 Embed 逐位一致。
	v, err := ve.Embed(texts[0])
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	for i := 0; i < 50; i++ {
		v2, _ := ve.Embed(texts[0])
		for j := range v {
			if v[j] != v2[j] {
				t.Fatalf("Embed not deterministic at run %d dim %d: %v vs %v", i, j, v[j], v2[j])
			}
		}
	}

	// 未命中文本 → 全零向量（长度确定）。
	miss, _ := ve.Embed("not in index text at all")
	for j := range miss {
		if miss[j] != 0 {
			t.Fatalf("miss embed not zero at %d: %v", j, miss[j])
		}
	}
	if len(miss) != 4 {
		t.Fatalf("miss len = %d, want 4", len(miss))
	}

	// 空文本 → 全零。
	empty, _ := ve.Embed("")
	for j := range empty {
		if empty[j] != 0 {
			t.Fatalf("empty embed not zero at %d", j)
		}
	}
}

func TestVectorEmbedder_CosineSimilarity(t *testing.T) {
	cases := []struct {
		a, b []float32
		want float64
	}{
		{[]float32{1, 0, 0}, []float32{1, 0, 0}, 1},
		{[]float32{1, 0, 0}, []float32{0, 1, 0}, 0},
		{[]float32{1, 0, 0}, []float32{-1, 0, 0}, -1},
		{[]float32{1, 1, 0}, []float32{1, 0, 0}, math.Sqrt(0.5)},
		{[]float32{0, 0, 0}, []float32{1, 0, 0}, 0}, // 零向量 → 0
		{nil, []float32{1, 0, 0}, 0},                // 空 → 0
	}
	for i, c := range cases {
		got := CosineSimilarity(c.a, c.b)
		if math.Abs(got-c.want) > 1e-6 {
			t.Errorf("case %d: CosineSimilarity = %.6f, want %.6f", i, got, c.want)
		}
	}
}

func TestVectorEmbedder_MissingFiles(t *testing.T) {
	dir := t.TempDir()
	// 元数据缺失
	if _, err := NewVectorEmbedder(VectorEmbedderConfig{
		MatrixPath: filepath.Join(dir, "x.npy"), MetaPath: filepath.Join(dir, "x.json"),
	}); err == nil {
		t.Fatal("expected error on missing files")
	}
	// 矩阵尺寸不匹配
	writeMiniNpy(t, filepath.Join(dir, "v.npy"), 2, 2, []float32{1, 0, 0, 1})
	writeMiniMeta(t, filepath.Join(dir, "m.json"), []string{"a", "b", "c"})
	if _, err := NewVectorEmbedder(VectorEmbedderConfig{
		MatrixPath: filepath.Join(dir, "v.npy"), MetaPath: filepath.Join(dir, "m.json"),
	}); err == nil {
		t.Fatal("expected error on dim/count mismatch")
	}
}

func TestVectorEmbedder_NilEmbed(t *testing.T) {
	var ve *VectorEmbedder
	v, err := ve.Embed("hello")
	if err != nil {
		t.Fatalf("nil Embed: %v", err)
	}
	if len(v) != 0 {
		t.Fatalf("nil Embed len = %d, want 0", len(v))
	}
	if ve.Dim() != 0 || ve.Count() != 0 {
		t.Fatal("nil Dim/Count should be 0")
	}
	if ve.Name() != "vector:nil" {
		t.Fatalf("nil Name = %s", ve.Name())
	}
}
