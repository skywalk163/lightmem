// R3-A：确定性 char n-gram TF-IDF embedder 与 signalReferenceFrame 修复单测。
//
// 覆盖点（任务书 A.5）：
//   - embedder 确定性（同文本同向量）、维度固定、空文本全零向量
//   - 词表顺序确定性（同 corpus 反复构建结果一致）
//   - signalReferenceFrame 在 QueryFrame==nil / 坐标全空时不再返回 1.0 常数
package kernel

import (
	"context"
	"math"
	"testing"
)

func nearlyEqual(a, b float64, eps float64) bool {
	return math.Abs(a-b) < eps
}

// 用例 A-1：embedder 确定性 + 维度固定 + 归一化。
func TestCharNgramEmbedderDeterministic(t *testing.T) {
	corpus := []string{
		"John: We held a toy drive for veterans",
		"community food drive organized in March",
		"Toy drive, Community food drive, veterans, domestic violence",
	}
	emb := NewCharNgramEmbedder(3, corpus)

	v1, err := emb("Toy drive, Community food drive")
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	v2, _ := emb("Toy drive, Community food drive")
	if len(v1) == 0 {
		t.Fatal("embed returned empty vector")
	}
	if !nearlyEqual(float64(v1[0]), float64(v2[0]), 1e-6) {
		t.Fatalf("determinism broken: %v vs %v", v1[0], v2[0])
	}
	// L2 归一化
	var sq float64
	for _, x := range v1 {
		sq += float64(x) * float64(x)
	}
	if !nearlyEqual(math.Sqrt(sq), 1.0, 1e-4) {
		t.Fatalf("not L2-normalized: |v|=%.4f", math.Sqrt(sq))
	}
}

// 用例 A-2：空文本 / 无 n-gram → 全零向量，长度与词表一致。
func TestCharNgramEmbedderEmpty(t *testing.T) {
	v := NewCharNgramVocab(3)
	v.BuildIDF([]string{"alpha beta gamma"})
	if v.Dim() == 0 {
		t.Fatal("vocab empty after build")
	}
	got := v.Embed("")
	for i, x := range got {
		if x != 0 {
			t.Fatalf("empty text not zero at %d", i)
		}
	}
	// 无 n-gram 命中的文本（单字符）
	got2 := v.Embed("x")
	if len(got2) != v.Dim() {
		t.Fatalf("dim mismatch: %d vs %d", len(got2), v.Dim())
	}
	for _, x := range got2 {
		if x != 0 {
			t.Fatalf("no-gram text not zero")
		}
	}
}

// 用例 A-3：同 corpus 反复构建 → 词表顺序与向量一致（seed 无关确定性）。
func TestCharNgramVocabStable(t *testing.T) {
	corpus := []string{"toy drive", "veterans food drive", "domestic violence support"}
	a := NewCharNgramVocab(3)
	a.BuildIDF(corpus)
	b := NewCharNgramVocab(3)
	b.BuildIDF(corpus)
	if a.Dim() != b.Dim() {
		t.Fatalf("dim mismatch: %d vs %d", a.Dim(), b.Dim())
	}
	va := a.Embed("toy drive")
	vb := b.Embed("toy drive")
	for i := range va {
		if !nearlyEqual(float64(va[i]), float64(vb[i]), 1e-9) {
			t.Fatalf("vocab unstable at %d", i)
		}
	}
}

// 用例 A-4：相关文本相似度 > 不相关文本（语义信号确有区分度）。
func TestCharNgramEmbedderDiscriminates(t *testing.T) {
	corpus := []string{
		"John organized a community food drive in March",
		"Sarah attended the veterans support workshop",
		"we collected canned goods for the food bank",
	}
	emb := NewCharNgramEmbedder(3, corpus)
	relatedA, _ := emb("community food drive")
	relatedB, _ := emb("organized a food drive event")
	unrelated, _ := emb("quantum mechanics lecture notes")
	simRel := cosine(relatedA, relatedB)
	simUnrel := cosine(relatedA, unrelated)
	if simRel <= simUnrel {
		t.Fatalf("expected related>unrelated, got %.4f vs %.4f", simRel, simUnrel)
	}
}

// 用例 A-5：signalReferenceFrame 在 QueryFrame==nil 时返回 0.5（不再 1.0）。
func TestSignalReferenceFrameNilQueryFrame(t *testing.T) {
	rf := DefaultRankFusion()
	rf.QueryFrame = nil
	cands := []RecalledCandidate{
		mkCand("m1", "fact one", "ent", KindFact, 1.0),
		mkCand("m2", "fact two", "ent", KindFact, 0.9),
	}
	out := rf.signalReferenceFrame(context.Background(), "q", cands)
	for _, id := range []string{"m1", "m2"} {
		v, ok := out[id]
		if !ok {
			t.Fatalf("missing %s", id)
		}
		if !nearlyEqual(v, 0.5, 1e-9) {
			t.Fatalf("expected neutral 0.5, got %v", v)
		}
	}
}

// 用例 A-6：signalReferenceFrame 在查询端坐标全空时返回 0.5。
func TestSignalReferenceFrameAllCoordsEmpty(t *testing.T) {
	rf := DefaultRankFusion()
	rf.QueryFrame = &ReferenceFrame{} // 所有坐标为空
	cands := []RecalledCandidate{
		mkCand("m1", "fact", "ent", KindFact, 1.0),
	}
	out := rf.signalReferenceFrame(context.Background(), "q", cands)
	if !nearlyEqual(out["m1"], 0.5, 1e-9) {
		t.Fatalf("expected 0.5 neutral, got %v", out["m1"])
	}
}

// 用例 A-7：signalReferenceFrame 在查询端有坐标且候选命中时给出区分度分。
func TestSignalReferenceFrameHitsCoords(t *testing.T) {
	rf := DefaultRankFusion()
	rf.QueryFrame = &ReferenceFrame{Tenant: "t1", User: "u1"}
	hit := mkCand("m1", "fact", "ent", KindFact, 1.0)
	hit.Memory.ReferenceFrame = ReferenceFrame{Tenant: "t1", User: "u1"}
	miss := mkCand("m2", "fact", "ent", KindFact, 1.0)
	miss.Memory.ReferenceFrame = ReferenceFrame{Tenant: "t1", User: "other"}
	cands := []RecalledCandidate{hit, miss}
	out := rf.signalReferenceFrame(context.Background(), "q", cands)
	if !nearlyEqual(out["m1"], 1.0, 1e-9) {
		t.Fatalf("expected hit 1.0, got %v", out["m1"])
	}
	if !nearlyEqual(out["m2"], 0.5, 1e-9) {
		t.Fatalf("expected miss 0.5, got %v", out["m2"])
	}
}

// 用例 A-8：词表 JSON 序列化/反序列化往返（供 tune 脚本与 run_kpi.go 共享 IDF）。
func TestCharNgramVocabRoundTrip(t *testing.T) {
	v := NewCharNgramVocab(3)
	v.BuildIDF([]string{"toy drive for veterans", "community food drive"})
	path := t.TempDir() + "/vocab.json"
	if err := v.SaveJSON(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	v2, err := LoadCharNgramVocabJSON(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if v2.Dim() != v.Dim() {
		t.Fatalf("dim mismatch: %d vs %d", v2.Dim(), v.Dim())
	}
	if v2.N != v.N {
		t.Fatalf("N mismatch: %d vs %d", v2.N, v.N)
	}
	a := v.Embed("toy drive")
	b := v2.Embed("toy drive")
	for i := range a {
		if !nearlyEqual(float64(a[i]), float64(b[i]), 1e-6) {
			t.Fatalf("round-trip mismatch at %d", i)
		}
	}
}
