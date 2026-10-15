// R14-A 路测试：参考系漂移衰减（§5.3.1）。
//
// 覆盖：全等/单轴漂移/双轴漂移/全轴漂移/缺失中性/空参考系/归一化等价/
// 部分匹配/乘性单调/排序确定性/入参不改/地板与配置回落/默认路径零改动。
package kernel

import (
	"context"
	"math"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// driftCand 构造一个参考系完整的候选。
func driftCand(id, entity, scene, task string, score float64) RecalledCandidate {
	return RecalledCandidate{
		Memory: &Memory{
			MemoryID: id,
			Fact:     id,
			ReferenceFrame: ReferenceFrame{
				Entity: entity,
				Scene:  scene,
				Task:   task,
			},
			Confidence: 0.9,
		},
		Layer:       LayerL2,
		RecallScore: score,
	}
}

func TestDriftDefaultConfigValues(t *testing.T) {
	cfg := DefaultDriftConfig()
	if cfg.Floor != 0.15 || cfg.Partial != 0.60 || cfg.Neutral != 1.00 {
		t.Fatalf("默认配置非保守缺省：floor=%v partial=%v neutral=%v", cfg.Floor, cfg.Partial, cfg.Neutral)
	}
	axes := cfg.Axes
	if len(axes) != 3 || !reflect.DeepEqual(axes, DefaultDriftAxes()) {
		t.Fatalf("默认轴应为三轴全开，实际 %v", axes)
	}
}

func TestDriftAllAxesEqualFactorOne(t *testing.T) {
	q := &ReferenceFrame{Entity: "客户A", Scene: "门店", Task: "续约"}
	m := &Memory{ReferenceFrame: ReferenceFrame{Entity: "客户A", Scene: "门店", Task: "续约"}}
	d := DriftOf(q, m, DefaultDriftConfig())
	if d.Factor != 1.0 {
		t.Fatalf("三轴全等因子应为 1.0，实际 %v", d.Factor)
	}
	if len(d.Matched) != 3 || len(d.Drifted) != 0 || len(d.Missing) != 0 {
		t.Fatalf("全等应记 3 个 Matched，实际 matched=%v drifted=%v missing=%v", d.Matched, d.Drifted, d.Missing)
	}
}

func TestDriftSingleAxisDriftHitsFloor(t *testing.T) {
	q := &ReferenceFrame{Entity: "客户A", Scene: "门店", Task: "续约"}
	m := &Memory{ReferenceFrame: ReferenceFrame{Entity: "客户A", Scene: "门店", Task: "开票"}}
	d := DriftOf(q, m, DefaultDriftConfig())
	if math.Abs(d.Factor-0.15) > 1e-12 {
		t.Fatalf("单轴漂移因子应为 0.15，实际 %v", d.Factor)
	}
	if !reflect.DeepEqual(d.Drifted, []DriftAxis{AxisTask}) {
		t.Fatalf("漂移轴应仅为 Task，实际 %v", d.Drifted)
	}
	if len(d.Matched) != 2 {
		t.Fatalf("其余两轴应相等，实际 matched=%v", d.Matched)
	}
}

func TestDriftTwoAxisDriftStaysAtFloor(t *testing.T) {
	q := &ReferenceFrame{Entity: "客户A", Scene: "门店", Task: "续约"}
	m := &Memory{ReferenceFrame: ReferenceFrame{Entity: "客户B", Scene: "门店", Task: "开票"}}
	d := DriftOf(q, m, DefaultDriftConfig())
	if math.Abs(d.Factor-0.15) > 1e-12 {
		t.Fatalf("双轴漂移应被地板托住为 0.15，实际 %v", d.Factor)
	}
	if !reflect.DeepEqual(d.Drifted, []DriftAxis{AxisEntity, AxisTask}) {
		t.Fatalf("漂移轴应为 entity+task 且按轴序输出，实际 %v", d.Drifted)
	}
}

func TestDriftAllAxesDriftFloorNotZero(t *testing.T) {
	q := &ReferenceFrame{Entity: "客户A", Scene: "门店", Task: "续约"}
	m := &Memory{ReferenceFrame: ReferenceFrame{Entity: "客户B", Scene: "工厂", Task: "开票"}}
	d := DriftOf(q, m, DefaultDriftConfig())
	if math.Abs(d.Factor-0.15) > 1e-12 {
		t.Fatalf("全轴漂移仍应为地板 0.15（绝不归零），实际 %v", d.Factor)
	}
	if len(d.Drifted) != 3 {
		t.Fatalf("全轴漂移应记 3 个 Drifted，实际 %v", d.Drifted)
	}
}

func TestDriftMissingAxesNeutral(t *testing.T) {
	q := &ReferenceFrame{Entity: "客户A", Scene: "门店", Task: "续约"}
	// 记忆完全无参考系：三轴缺失 → 中性，不衰减。
	m := &Memory{}
	d := DriftOf(q, m, DefaultDriftConfig())
	if d.Factor != 1.0 {
		t.Fatalf("缺失应中性不衰减，实际 %v", d.Factor)
	}
	if !reflect.DeepEqual(d.Missing, []DriftAxis{AxisEntity, AxisScene, AxisTask}) {
		t.Fatalf("应全记为 Missing，实际 %v", d.Missing)
	}
	// 查询侧缺失同理。
	if d2 := DriftOf(nil, &Memory{ReferenceFrame: ReferenceFrame{Entity: "x"}}, DefaultDriftConfig()); d2.Factor != 1.0 {
		t.Fatalf("查询侧 nil 应中性，实际 %v", d2.Factor)
	}
}

func TestDriftEmptyFramesFullyNeutral(t *testing.T) {
	d := DriftOf(&ReferenceFrame{}, &Memory{}, DefaultDriftConfig())
	if d.Factor != 1.0 || len(d.Missing) != 3 {
		t.Fatalf("两侧空参考系应全中性，实际 factor=%v missing=%v", d.Factor, d.Missing)
	}
	for _, l := range [][]DriftAxis{d.Matched, d.Partial, d.Drifted} {
		if len(l) != 0 {
			t.Fatalf("空参考系不应产生 matched/partial/drifted，实际 %v", l)
		}
	}
}

func TestDriftNormalizationEquivalence(t *testing.T) {
	q := &ReferenceFrame{Entity: "客户 A", Scene: " 门店 ", Task: "续约单"}
	m := &Memory{ReferenceFrame: ReferenceFrame{Entity: "客户a", Scene: "门店", Task: "续约单"}}
	d := DriftOf(q, m, DefaultDriftConfig())
	if d.Factor != 1.0 {
		t.Fatalf("归一化等价应判全等，实际 %v", d.Factor)
	}
	if len(d.Matched) != 3 {
		t.Fatalf("归一化后应三轴全等，实际 matched=%v", d.Matched)
	}
}

func TestDriftPartialMatch(t *testing.T) {
	q := &ReferenceFrame{Entity: "客户A", Scene: "门店", Task: "续约"}
	m := &Memory{ReferenceFrame: ReferenceFrame{Entity: "客户A-华东", Scene: "门店", Task: "续约"}}
	d := DriftOf(q, m, DefaultDriftConfig())
	if math.Abs(d.Factor-0.60) > 1e-12 {
		t.Fatalf("单轴部分匹配应为 0.60，实际 %v", d.Factor)
	}
	if !reflect.DeepEqual(d.Partial, []DriftAxis{AxisEntity}) {
		t.Fatalf("应记 1 个 Partial(entity)，实际 %v", d.Partial)
	}
}

func TestDriftMonotonicDecay(t *testing.T) {
	base := ReferenceFrame{Entity: "客户A", Scene: "门店", Task: "续约"}
	mk := func(entity string) *Memory {
		return &Memory{ReferenceFrame: ReferenceFrame{Entity: entity, Scene: "门店", Task: "续约"}}
	}
	// 逐级降低匹配度：全等 > 三轴 partial > 两轴 partial > 单轴 partial > 单轴漂移。
	cases := []struct {
		mem    *Memory
		expect float64
	}{
		{mk("客户A"), 1.0},
		{mk("客户A-华东"), 0.60},
		{&Memory{ReferenceFrame: ReferenceFrame{Entity: "客户A-华东", Scene: "门店店", Task: "续约"}}, 0.36},
		{&Memory{ReferenceFrame: ReferenceFrame{Entity: "客户A-华东", Scene: "门店店", Task: "续约单"}}, 0.216},
		{mk("客户B"), 0.15},
	}
	var prev float64
	for i, c := range cases {
		got := DriftOf(&base, c.mem, DefaultDriftConfig()).Factor
		if math.Abs(got-c.expect) > 1e-9 {
			t.Fatalf("case %d 期望 %v，实际 %v", i, c.expect, got)
		}
		if prev > 0 && got > prev+1e-12 {
			t.Fatalf("单调性破坏：case %d factor %v > 前一个 %v", i, got, prev)
		}
		prev = got
	}
}

func TestDriftAwareRankDeterministicOrder(t *testing.T) {
	qf := &ReferenceFrame{Entity: "客户A", Scene: "门店", Task: "续约"}
	mk := func() []RecalledCandidate {
		return []RecalledCandidate{
			driftCand("m-c", "客户B", "门店", "续约", 0.9),
			driftCand("m-a", "客户A", "门店", "续约", 0.8),
			driftCand("m-b", "客户A-华东", "门店", "续约", 0.7),
		}
	}
	first := DriftAwareRank(qf, mk(), DefaultDriftConfig())
	second := DriftAwareRank(qf, mk(), DefaultDriftConfig())
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("同输入应逐位一致：\n%v\n%v", first, second)
	}
	// 打乱输入顺序，输出序列（按调整后分排序）应一致。
	shuffled := mk()
	shuffled[0], shuffled[2] = shuffled[2], shuffled[0]
	third := DriftAwareRank(qf, shuffled, DefaultDriftConfig())
	if len(third) != 3 {
		t.Fatalf("应为 3 条结果，实际 %d", len(third))
	}
	if third[0].MemoryID != "m-a" {
		t.Fatalf("全等候选应排首，实际 %s", third[0].MemoryID)
	}
}

func TestDriftAwareRankSkipsNilCandidate(t *testing.T) {
	out := DriftAwareRank(nil, []RecalledCandidate{
		{Memory: nil, RecallScore: 0.9},
		driftCand("m-1", "客户A", "门店", "续约", 0.8),
	}, DefaultDriftConfig())
	if len(out) != 1 || out[0].MemoryID != "m-1" {
		t.Fatalf("应跳过 nil 候选，实际 %+v", out)
	}
	empty := DriftAwareRank(nil, nil, DefaultDriftConfig())
	if empty == nil || len(empty) != 0 {
		t.Fatalf("空输入应返回非 nil 空切片，实际 %#v", empty)
	}
}

func TestDriftAwareRankDoesNotMutateInput(t *testing.T) {
	in := driftCand("m-1", "客户A", "门店", "续约", 0.8)
	in.Memory.Version = 7
	cp := in
	_ = DriftAwareRank(&ReferenceFrame{Entity: "客户A", Scene: "门店", Task: "续约"}, []RecalledCandidate{in}, DefaultDriftConfig())
	if !reflect.DeepEqual(in, cp) {
		t.Fatalf("输入被修改：\nbefore %+v\nafter  %+v", cp, in)
	}
}

func TestDriftAwareRankNeverExceedsRecallScore(t *testing.T) {
	qf := &ReferenceFrame{Entity: "客户A", Scene: "门店", Task: "续约"}
	cands := []RecalledCandidate{
		driftCand("m-1", "客户A", "门店", "续约", 0.8),
		driftCand("m-2", "客户B", "工厂", "开票", 0.6),
		driftCand("m-3", "客户A-华东", "门店", "续约", 0.5),
	}
	out := DriftAwareRank(qf, cands, DefaultDriftConfig())
	if len(out) != 3 {
		t.Fatalf("应输出 3 条，实际 %d", len(out))
	}
	for _, r := range out {
		var ref float64
		for _, c := range cands {
			if c.Memory.MemoryID == r.MemoryID {
				ref = c.RecallScore
			}
		}
		if r.Score > ref+1e-12 {
			t.Fatalf("只衰减不加分破坏：%s score=%v > recall=%v", r.MemoryID, r.Score, ref)
		}
		if strings.TrimSpace(r.Reason) == "" {
			t.Fatalf("%s 缺少审计文本", r.MemoryID)
		}
	}
	// 分数应为 (0.8, 0.09, 0.3) 降序。
	want := []float64{0.8, 0.3, 0.09}
	for i, w := range want {
		if math.Abs(out[i].Score-w) > 1e-9 {
			t.Fatalf("rank %d 期望 %v，实际 %v", i, w, out[i].Score)
		}
	}
}

func TestDriftConfigInvalidFallbacks(t *testing.T) {
	cfg := DriftConfig{}
	got := effectiveDriftCfg(cfg)
	if got.Floor != 0.15 || got.Partial != 0.60 || got.Neutral != 1.00 {
		t.Fatalf("零值配置应回落到保守缺省，实际 %+v", got)
	}
	if !reflect.DeepEqual(got.Axes, DefaultDriftAxes()) {
		t.Fatalf("空轴应回落到三轴，实际 %v", got.Axes)
	}
	bad := DriftConfig{Axes: []DriftAxis{DriftAxis(99)}, Floor: 0, Partial: -1, Neutral: 5}
	if e := effectiveDriftCfg(bad); !reflect.DeepEqual(e.Axes, DefaultDriftAxes()) || e.Floor != 0.15 || e.Partial != 0.60 || e.Neutral != 1.0 {
		t.Fatalf("非法配置未回落：%+v", e)
	}
	// 入参不得被改写。
	if bad.Floor != 0 || bad.Partial != -1 || bad.Neutral != 5 {
		t.Fatalf("入参被改写：%+v", bad)
	}
}

func TestDriftConfigCustomAxesAndClamp(t *testing.T) {
	q := &ReferenceFrame{Entity: "客户A", Scene: "门店", Task: "续约"}
	m := &Memory{ReferenceFrame: ReferenceFrame{Entity: "客户B", Scene: "门店", Task: "续约"}}
	// 只判任务轴：实体漂移不参与。
	cfg := DriftConfig{Axes: []DriftAxis{AxisTask}, Floor: 0.15}
	if d := DriftOf(q, m, cfg); d.Factor != 1.0 || len(d.PerAxis) != 1 {
		t.Fatalf("单轴配置应只判 Task，实际 factor=%v axes=%v", d.Factor, d.PerAxis)
	}
	// Floor=1 时完全不衰减。
	cfg2 := DriftConfig{Floor: 1, Partial: 1, Neutral: 1}
	if d := DriftOf(q, m, cfg2); d.Factor != 1.0 {
		t.Fatalf("Floor=1 应完全不衰减，实际 %v", d.Factor)
	}
	// Neutral=0 视为「未设置」→ 回落缺省 1.0，缺失保持中性不惩罚记忆。
	// Go 零值无法区分「未设置」与「显式 0」，契约取保守：参考系缺失
	// 不得衰减（否则历史无参考系记忆会被集体降权，直接威胁北极星）。
	d3 := DriftOf(q, &Memory{}, DriftConfig{Neutral: 0})
	if d3.Factor != 1.0 {
		t.Fatalf("Neutral 零值应回落 1.0（缺失中性），实际 %v", d3.Factor)
	}
	// clamp 上限：Partial=2 应被夹到 1，单轴部分匹配不再衰减。
	d4 := DriftOf(q, &Memory{ReferenceFrame: ReferenceFrame{Entity: "客户A-华东", Scene: "门店", Task: "续约"}}, DriftConfig{Partial: 2})
	if math.Abs(d4.Factor-1.0) > 1e-12 {
		t.Fatalf("Partial=2 应 clamp 到 1，实际 %v", d4.Factor)
	}
	// 重复轴去重。
	cfg4 := effectiveDriftCfg(DriftConfig{Axes: []DriftAxis{AxisEntity, AxisEntity, AxisScene}})
	if len(cfg4.Axes) != 2 {
		t.Fatalf("重复轴应去重，实际 %v", cfg4.Axes)
	}
}

func TestDriftReportFormat(t *testing.T) {
	q := &ReferenceFrame{Entity: "客户A", Scene: "门店", Task: "续约"}
	m := &Memory{ReferenceFrame: ReferenceFrame{Entity: "客户B", Scene: "", Task: "续约"}}
	rep := DriftReport(DriftOf(q, m, DefaultDriftConfig()))
	if !strings.HasPrefix(rep, "drift=") || !strings.Contains(rep, "entity=drift") ||
		!strings.Contains(rep, "scene=missing") || !strings.Contains(rep, "task=equal") {
		t.Fatalf("审计文本格式不符：%s", rep)
	}
	// 逐位确定：同输入两次调用一致。
	if rep2 := DriftReport(DriftOf(q, m, DefaultDriftConfig())); rep2 != rep {
		t.Fatalf("审计文本不稳定：%s vs %s", rep, rep2)
	}
	// 轴序固定：entity 在 scene 前，scene 在 task 前。
	if i := strings.Index(rep, "entity="); i < 0 || strings.Index(rep, "scene=") <= i || strings.Index(rep, "task=") <= i+7 {
		t.Fatalf("轴序不稳定：%s", rep)
	}
}

func TestDriftOfNilArgsSafe(t *testing.T) {
	d := DriftOf(nil, nil, DriftConfig{})
	if d.Factor != 1.0 || d.PerAxis == nil || d.Verdicts == nil {
		t.Fatalf("nil 入参应安全且返回非 nil map，实际 %+v", d)
	}
	for _, l := range [][]DriftAxis{d.Matched, d.Partial, d.Missing, d.Drifted} {
		if l == nil {
			t.Fatalf("列表字段应为非 nil 空切片：%+v", d)
			return
		}
	}
}

func TestDriftDefaultPathUntouched(t *testing.T) {
	rf := DefaultRankFusion()
	// 默认融合模式仍为 RRF（空串等价 rrf）。
	if rf.FusionMode != "" {
		t.Fatalf("默认 FusionMode 不应被改动：%q", rf.FusionMode)
	}
	// 默认信号中不得出现漂移信号（旁路算子不得接入默认路径）。
	for _, s := range rf.Signals {
		if strings.Contains(strings.ToLower(s.Name), "drift") {
			t.Fatalf("默认路径出现漂移信号：%s", s.Name)
		}
	}
	if rf.AbsoluteFloor != 0.6 {
		t.Fatalf("绝对分下限应保持 0.6，实际 %v", rf.AbsoluteFloor)
	}
	if rf.ProximityRatio != 0.20 {
		t.Fatalf("接近融合比值应保持 0.20，实际 %v", rf.ProximityRatio)
	}
	// 排序稳定性：同输入多次调用逐位一致。
	cands := []RecalledCandidate{driftCand("m-1", "a", "b", "c", 0.5)}
	a := rf.Fuse(context.Background(), "客户A", cands)
	b := rf.Fuse(context.Background(), "客户A", cands)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("默认融合输出不稳定：%v vs %v", a, b)
	}
	// 全序检查：同分按字典序。
	out := DriftAwareRank(nil, []RecalledCandidate{
		driftCand("z", "", "", "", 0.5),
		driftCand("a", "", "", "", 0.5),
		driftCand("m", "", "", "", 0.5),
	}, DefaultDriftConfig())
	if !sort.SliceIsSorted(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].MemoryID < out[j].MemoryID
	}) {
		t.Fatalf("排序未满足 (score 降序, memory_id 字典序) 全序：%+v", out)
	}
}
