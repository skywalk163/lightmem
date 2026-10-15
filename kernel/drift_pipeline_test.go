package kernel

import (
	"context"
	"math"
	"reflect"
	"sort"
	"testing"
	"time"
)

// newDriftRF 构造一个固定时刻的 RankFusion，避免 time.Now 引入的不确定性。
func newDriftRF() *RankFusion {
	rf := DefaultRankFusion()
	rf.Now = time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	rf.QueryFrame = &ReferenceFrame{Tenant: "t1", User: "u1"}
	return rf
}

// mkCand 构造一条带参考系的候选。
func mkDriftCand(id, entity, scene, task string, score float64) RecalledCandidate {
	return RecalledCandidate{
		Memory: &Memory{
			MemoryID:       id,
			Fact:           "fact-" + id,
			ReferenceFrame: ReferenceFrame{Entity: entity, Scene: scene, Task: task},
			Confidence:     0.9,
			RecordedAt:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			Source:         "unit-test",
		},
		Layer:       LayerL1,
		RecallScore: score,
	}
}

func closeEnough(a, b, eps float64) bool {
	return math.Abs(a-b) <= eps
}

// TestDriftPipelineDefaultConfigMode 默认配置必须处于「默认关」态（红线 11）。
func TestDriftPipelineDefaultConfigMode(t *testing.T) {
	if got := DefaultDriftPipelineConfig().Mode; got != RankModeDefault {
		t.Fatalf("DefaultDriftPipelineConfig().Mode=%q, want %q", got, RankModeDefault)
	}
}

// TestDriftPipelineEnabledHelper Enabled() 与 Mode 一致。
func TestDriftPipelineEnabledHelper(t *testing.T) {
	if DefaultDriftPipelineConfig().Enabled() {
		t.Fatal("default config must not be Enabled")
	}
	driftCfg := DriftPipelineConfig{Mode: RankModeDrift}
	if !driftCfg.Enabled() {
		t.Fatal("drift mode must be Enabled")
	}
}

// TestDriftPipelineResolveMode Resolve 的选路规则：默认透传、drift+OnlyIfAxisAnnotated 三轴全缺回落。
func TestDriftPipelineResolveMode(t *testing.T) {
	// 默认态原样透传。
	c0 := DefaultDriftPipelineConfig()
	if got := c0.Resolve(&ReferenceFrame{Entity: "e"}); got != RankModeDefault {
		t.Fatalf("default resolve=%q, want default", got)
	}
	// drift + 关闭 OnlyIfAxisAnnotated：原样 drift。
	c1 := DriftPipelineConfig{Mode: RankModeDrift, OnlyIfAxisAnnotated: false}
	if got := c1.Resolve(&ReferenceFrame{}); got != RankModeDrift {
		t.Fatalf("drift+no-annotate resolve=%q, want drift", got)
	}
	// drift + 开启 OnlyIfAxisAnnotated + nil qf：回落 default。
	c2 := DriftPipelineConfig{Mode: RankModeDrift, OnlyIfAxisAnnotated: true}
	if got := c2.Resolve(nil); got != RankModeDefault {
		t.Fatalf("drift+annotate+nil-qf resolve=%q, want default", got)
	}
	// drift + 开启 OnlyIfAxisAnnotated + 三轴全空：回落 default。
	if got := c2.Resolve(&ReferenceFrame{}); got != RankModeDefault {
		t.Fatalf("drift+annotate+empty-qf resolve=%q, want default", got)
	}
	// drift + 开启 OnlyIfAxisAnnotated + 至少一轴非空：保持 drift。
	if got := c2.Resolve(&ReferenceFrame{Entity: "e"}); got != RankModeDrift {
		t.Fatalf("drift+annotate+has-axis resolve=%q, want drift", got)
	}
}

// TestDriftPipelineDefaultEqualsBaseline 默认态与 R14 基线 rf.Rank 逐位一致。
func TestDriftPipelineDefaultEqualsBaseline(t *testing.T) {
	rf := newDriftRF()
	cands := []RecalledCandidate{
		mkDriftCand("a", "e1", "s1", "t1", 0.9),
		mkDriftCand("b", "e2", "s2", "t2", 0.5),
		mkDriftCand("c", "e3", "s3", "t3", 0.7),
	}
	qf := &ReferenceFrame{Entity: "e1", Scene: "s1", Task: "t1"}
	cfg := DefaultDriftPipelineConfig() // Mode=default
	got, err := RankViaPipeline(context.Background(), rf, qf, "q", cands, cfg)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := rf.Rank(context.Background(), "q", cands)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("default pipeline != rf.Rank\n got=%+v\nwant=%+v", got, want)
	}
}

// TestDriftPipelineDriftEqualsDriftAwareRank 漂移态与直接调 DriftAwareRank 逐位一致（证明只是委托）。
func TestDriftPipelineDriftEqualsDriftAwareRank(t *testing.T) {
	rf := newDriftRF()
	cands := []RecalledCandidate{
		mkDriftCand("a", "e1", "s1", "t1", 0.9),
		mkDriftCand("b", "e2", "s2", "t2", 0.5),
	}
	qf := &ReferenceFrame{Entity: "e1", Scene: "s1", Task: "t1"}
	cfg := DriftPipelineConfig{Mode: RankModeDrift, DriftCfg: DefaultDriftConfig(), OnlyIfAxisAnnotated: false}
	got, err := RankViaPipeline(context.Background(), rf, qf, "q", cands, cfg)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := DriftAwareRank(qf, cands, DefaultDriftConfig())
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("drift pipeline != DriftAwareRank\n got=%+v\nwant=%+v", got, want)
	}
}

// TestDriftPipelineOnlyIfAxisAnnotatedFallback 开启 OnlyIfAxisAnnotated 且三轴全缺时回落默认结果。
func TestDriftPipelineOnlyIfAxisAnnotatedFallback(t *testing.T) {
	rf := newDriftRF()
	cands := []RecalledCandidate{
		mkDriftCand("a", "e1", "s1", "t1", 0.9),
		mkDriftCand("b", "e2", "s2", "t2", 0.5),
	}
	cfg := DriftPipelineConfig{Mode: RankModeDrift, DriftCfg: DefaultDriftConfig(), OnlyIfAxisAnnotated: true}
	qfMissing := &ReferenceFrame{} // 三轴全空
	got, err := RankViaPipeline(context.Background(), rf, qfMissing, "q", cands, cfg)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := rf.Rank(context.Background(), "q", cands)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("annotate fallback != rf.Rank\n got=%+v\nwant=%+v", got, want)
	}
}

// TestDriftPipelineSingleAxisFloor 开态下单轴漂移下沉到地板 0.15（entity 相等、scene 漂移、task 相等）。
func TestDriftPipelineSingleAxisFloor(t *testing.T) {
	rf := newDriftRF()
	c := mkDriftCand("a", "e1", "sX", "t1", 0.8) // scene 与 qf 不同 → 漂移
	cands := []RecalledCandidate{c}
	qf := &ReferenceFrame{Entity: "e1", Scene: "s1", Task: "t1"}
	cfg := DriftPipelineConfig{Mode: RankModeDrift, DriftCfg: DefaultDriftConfig(), OnlyIfAxisAnnotated: false}
	got, err := RankViaPipeline(context.Background(), rf, qf, "q", cands, cfg)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 result, got %d", len(got))
	}
	// entity 相等(1.0) × scene 漂移(0.15) × task 相等(1.0) = 0.15；score = 0.8 × 0.15。
	want := 0.8 * 0.15
	if !closeEnough(got[0].Score, want, 1e-9) {
		t.Fatalf("single-axis floor score=%v, want %v", got[0].Score, want)
	}
	if got[0].Score >= c.RecallScore {
		t.Fatalf("drifted score %v should be below recall %v", got[0].Score, c.RecallScore)
	}
}

// TestDriftPipelineMissingAxisNeutral 缺轴走中性 1.00（qf 有 entity、mem 缺 entity → 中性，score 不变）。
func TestDriftPipelineMissingAxisNeutral(t *testing.T) {
	rf := newDriftRF()
	c := mkDriftCand("a", "", "s1", "t1", 0.8) // mem 缺 entity
	cands := []RecalledCandidate{c}
	qf := &ReferenceFrame{Entity: "e1", Scene: "s1", Task: "t1"}
	cfg := DriftPipelineConfig{Mode: RankModeDrift, DriftCfg: DefaultDriftConfig(), OnlyIfAxisAnnotated: false}
	got, err := RankViaPipeline(context.Background(), rf, qf, "q", cands, cfg)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 result, got %d", len(got))
	}
	// entity 缺失(中性 1.0) × scene 相等(1.0) × task 相等(1.0) = 1.0；score 不变。
	if !closeEnough(got[0].Score, c.RecallScore, 1e-9) {
		t.Fatalf("missing-axis neutral score=%v, want %v", got[0].Score, c.RecallScore)
	}
}

// TestDriftPipelineDeterministicShuffle 打乱候选顺序，漂移态输出逐位一致（证明排序与输入序无关）。
func TestDriftPipelineDeterministicShuffle(t *testing.T) {
	rf := newDriftRF()
	base := []RecalledCandidate{
		mkDriftCand("a", "e1", "s1", "t1", 0.9),
		mkDriftCand("b", "e2", "s2", "t2", 0.5),
		mkDriftCand("c", "e3", "s3", "t3", 0.7),
	}
	cfg := DriftPipelineConfig{Mode: RankModeDrift, DriftCfg: DefaultDriftConfig(), OnlyIfAxisAnnotated: false}
	qf := &ReferenceFrame{Entity: "e1", Scene: "s1", Task: "t1"}

	gotA, _ := RankViaPipeline(context.Background(), rf, qf, "q", base, cfg)

	shuffled := make([]RecalledCandidate, len(base))
	copy(shuffled, base)
	sort.SliceStable(shuffled, func(i, j int) bool { return shuffled[i].Memory.MemoryID > shuffled[j].Memory.MemoryID })
	gotB, _ := RankViaPipeline(context.Background(), rf, qf, "q", shuffled, cfg)

	if !reflect.DeepEqual(gotA, gotB) {
		t.Fatalf("shuffled output differs\n gotA=%+v\n gotB=%+v", gotA, gotB)
	}
}

// TestDriftPipelineNilQF nil qf 安全：全轴缺失 → 中性 1.0，score 不变，无 panic。
func TestDriftPipelineNilQF(t *testing.T) {
	rf := newDriftRF()
	cands := []RecalledCandidate{mkDriftCand("a", "e1", "s1", "t1", 0.8)}
	cfg := DriftPipelineConfig{Mode: RankModeDrift, DriftCfg: DefaultDriftConfig(), OnlyIfAxisAnnotated: false}
	got, err := RankViaPipeline(context.Background(), rf, nil, "q", cands, cfg)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 result, got %d", len(got))
	}
	if !closeEnough(got[0].Score, 0.8, 1e-9) {
		t.Fatalf("nil qf should be neutral, score=%v want 0.8", got[0].Score)
	}
}

// TestDriftPipelineEmptyCands 空候选安全：两种模式都不 panic。
func TestDriftPipelineEmptyCands(t *testing.T) {
	rf := newDriftRF()
	qf := &ReferenceFrame{Entity: "e1"}
	// 默认态空候选 → rf.Rank 返回 nil。
	def, err := RankViaPipeline(context.Background(), rf, qf, "q", nil, DefaultDriftPipelineConfig())
	if err != nil {
		t.Fatalf("default empty err: %v", err)
	}
	if def != nil {
		t.Fatalf("default empty should be nil, got %+v", def)
	}
	// 漂移态空候选 → DriftAwareRank 返回非 nil 空切片。
	drift, err := RankViaPipeline(context.Background(), rf, qf, "q", nil,
		DriftPipelineConfig{Mode: RankModeDrift, DriftCfg: DefaultDriftConfig(), OnlyIfAxisAnnotated: false})
	if err != nil {
		t.Fatalf("drift empty err: %v", err)
	}
	if len(drift) != 0 {
		t.Fatalf("drift empty should be empty, got %d", len(drift))
	}
}

// TestDriftPipelineNilRF rf==nil 返回错误且不 panic。
func TestDriftPipelineNilRF(t *testing.T) {
	cands := []RecalledCandidate{mkDriftCand("a", "e1", "s1", "t1", 0.8)}
	got, err := RankViaPipeline(context.Background(), nil, &ReferenceFrame{Entity: "e1"}, "q", cands, DefaultDriftPipelineConfig())
	if err == nil {
		t.Fatal("expected error when rf is nil")
	}
	if got != nil {
		t.Fatalf("expected nil result, got %+v", got)
	}
}

// TestDriftPipelineNoSecondDecay 改变 DriftConfig.Floor 后漂移输出变化，且仍与 DriftAwareRank 逐位一致
// （证明本文件不含第二套衰减，共用 R14 同一份逻辑）。
func TestDriftPipelineNoSecondDecay(t *testing.T) {
	rf := newDriftRF()
	cands := []RecalledCandidate{
		mkDriftCand("a", "e1", "sX", "t1", 0.8), // scene 漂移
		mkDriftCand("b", "e2", "s2", "t2", 0.5),
	}
	qf := &ReferenceFrame{Entity: "e1", Scene: "s1", Task: "t1"}
	cfgA := DriftPipelineConfig{Mode: RankModeDrift, DriftCfg: DefaultDriftConfig(), OnlyIfAxisAnnotated: false}
	cfgB := DriftPipelineConfig{Mode: RankModeDrift, DriftCfg: DefaultDriftConfig(), OnlyIfAxisAnnotated: false}
	cfgB.DriftCfg.Floor = 0.05 // 改地板

	gotA, _ := RankViaPipeline(context.Background(), rf, qf, "q", cands, cfgA)
	gotB, _ := RankViaPipeline(context.Background(), rf, qf, "q", cands, cfgB)

	if reflect.DeepEqual(gotA, gotB) {
		t.Fatal("changing DriftConfig.Floor should change drift output (proves config is wired through)")
	}
	wantB := DriftAwareRank(qf, cands, cfgB.DriftCfg)
	if !reflect.DeepEqual(gotB, wantB) {
		t.Fatalf("drift pipeline(Floor=0.05) != DriftAwareRank\n got=%+v\nwant=%+v", gotB, wantB)
	}
}
