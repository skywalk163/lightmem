// R14-A 路：参考系漂移衰减（Reference-Frame Drift Decay）。
//
// 真源：lightmem.md §5.3.1；依据 lightmem/monty.md §4.4 建议 3。
//
// 背景（Monty 的缺陷，反向借鉴）：Monty 的 evidence 默认无界累积、不遗忘，
// 其文档自承「换物体时旧证据应失效，有界更合理」。本文件把这条痛点转成
// lightmem 的显式规则：**排序衰减不应只由时间与访问驱动**——当记忆的
// reference_frame 与查询参考系在 entity/scene/task 上显著不同时，即使该记忆
// 「很新」，排序分也应快速下沉。
//
// 与既有机制的分工：
//   - §5.3 固有价值（IntrinsicValue）回答「该记多久」，只增不改；
//   - 本文件回答「此刻该不该沉下去」，只在排序期做乘性衰减；
//   - 检索期已有 signalReferenceFrame 信号，本文件不改其权重、不进默认
//     RRF / consensus 投票路径，由调用方按需前置调用 DriftAwareRank。
//
// 红线遵守：
//   - 只增不改：本文件为全新文件，不修改 kernel/lifecycle.go、
//     kernel/rank_fusion.go 或任何既有字段语义；
//   - 只衰减不加分：DriftAwareRank 在输入分上乘漂移因子，不引入新信号、
//     不改权重、不覆盖默认融合结果；
//   - 缺失即中性：查询或记忆缺某轴字段时该轴记为中性（默认不衰减），
//     避免误伤没有标注参考系的历史数据；
//   - 乘性单调：因子 = 各轴分量乘积并 clamp 到 [Floor, 1.0]，单轴漂移即足以
//     下沉，且永不低于地板、绝不归零；
//   - 确定性：无随机、无时间依赖、无 I/O、不调用 LLM；
//   - 私有函数一律 drift 前缀，避免与包内既有小写标识符重名。
package kernel

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// 保守缺省值（R14 定稿口径，先定后调）。
const (
	// DriftDefaultFloor 衰减地板：单轴漂移最低只把分压到 15%，防归零。
	DriftDefaultFloor = 0.15
	// DriftDefaultPartial 部分匹配系数：包含关系但不相等时按 60% 计。
	DriftDefaultPartial = 0.60
	// DriftDefaultNeutral 缺失中性系数：字段缺失不算漂移，按 100% 计。
	DriftDefaultNeutral = 1.00
)

// DriftAxis 漂移判定的参考系轴（与 §3.2 ReferenceFrame 字段一一对应）。
type DriftAxis int

const (
	// AxisEntity 实体轴（ReferenceFrame.Entity）。
	AxisEntity DriftAxis = iota
	// AxisScene 场景轴（ReferenceFrame.Scene）。
	AxisScene
	// AxisTask 任务轴（ReferenceFrame.Task）。
	AxisTask
)

// axisCount 轴数量，供固定顺序遍历与数组化。
const axisCount = 3

// axisLabels 轴的可读标签（snake_case，日志 / 审计用）。
var axisLabels = []string{"entity", "scene", "task"}

// String 返回轴的可读标签；未知轴返回 unknown。
func (a DriftAxis) String() string {
	if a >= 0 && int(a) < axisCount {
		return axisLabels[a]
	}
	return "unknown"
}

// DefaultDriftAxes 返回默认全开三轴（实体、场景、任务）。
func DefaultDriftAxes() []DriftAxis {
	return []DriftAxis{AxisEntity, AxisScene, AxisTask}
}

// DriftVerdict 单轴判定结果。
type DriftVerdict int

const (
	// verdictEqual 归一化后完全相等。
	verdictEqual DriftVerdict = iota
	// verdictPartial 存在包含关系但不相等（视为部分匹配）。
	verdictPartial
	// verdictMissing 查询侧或记忆侧缺该字段（中性，不衰减）。
	verdictMissing
	// verdictDrift 两侧均非空且无包含关系。
	verdictDrift
)

// String 返回判定标签（用于审计文本）。
func (v DriftVerdict) String() string {
	switch v {
	case verdictEqual:
		return "equal"
	case verdictPartial:
		return "partial"
	case verdictMissing:
		return "missing"
	case verdictDrift:
		return "drift"
	}
	return "unknown"
}

// DriftConfig 漂移衰减配置。全部可配，非法值回落到保守缺省（先定后调）。
type DriftConfig struct {
	// Axes 参与判定的轴；为空或全非法时回落 DefaultDriftAxes()。
	Axes []DriftAxis
	// Floor 衰减地板，默认 0.15；<=0 回落默认，>1 clamp 到 1.0。
	Floor float64
	// Partial 部分匹配系数，默认 0.60；<0 回落默认，>1 clamp 到 1.0。
	Partial float64
	// Neutral 缺失中性系数，默认 1.00；<0 回落默认，>1 clamp 到 1.0。
	Neutral float64
}

// DefaultDriftConfig 返回保守缺省：三轴全开，Floor=0.15，Partial=0.60，
// Neutral=1.00。
func DefaultDriftConfig() DriftConfig {
	return DriftConfig{
		Axes:    DefaultDriftAxes(),
		Floor:   DriftDefaultFloor,
		Partial: DriftDefaultPartial,
		Neutral: DriftDefaultNeutral,
	}
}

// ReferenceDrift 单条记忆相对查询参考系的漂移结论（因子 ∈[0,1] + 逐轴明细）。
type ReferenceDrift struct {
	// Factor 乘性衰减因子，clamp 到 [Floor, 1.0]；全轴中性时为 1.0。
	Factor float64
	// PerAxis 各轴得分（axis -> score），键为判定过的轴。
	PerAxis map[DriftAxis]float64
	// Verdicts 各轴判定（axis -> verdict），供审计文本使用，避免从分值反推。
	Verdicts map[DriftAxis]DriftVerdict
	// Matched 完全相等的轴（固定轴序输出）。
	Matched []DriftAxis
	// Partial 部分匹配的轴（固定轴序输出）。
	Partial []DriftAxis
	// Missing 字段缺失、按中性处理的轴（固定轴序输出）。
	Missing []DriftAxis
	// Drifted 发生漂移的轴（固定轴序输出）。
	Drifted []DriftAxis
}

// driftNormalize 归一化：转小写 + 去标点与空白（保留字母、数字、CJK 等字母字符）。
//
// 口径与 §3.2 参考系归一化一致，保证 "客户 A" 与 "客户a" 判为相等。
func driftNormalize(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// driftHasPartial 判断是否为包含关系（两侧长度至少 2，避免单字误判为部分匹配）。
func driftHasPartial(q, m string) bool {
	if len(q) < 2 || len(m) < 2 || q == m {
		return false
	}
	if len(q) >= len(m) {
		return strings.Contains(q, m)
	}
	return strings.Contains(m, q)
}

// driftVerdictOf 单轴判定。入参必须是已归一化字符串。
func driftVerdictOf(q, m string) DriftVerdict {
	if q == "" || m == "" {
		return verdictMissing
	}
	if q == m {
		return verdictEqual
	}
	if driftHasPartial(q, m) {
		return verdictPartial
	}
	return verdictDrift
}

// driftAxisContains 轴列表去重辅助。
func driftAxisContains(list []DriftAxis, a DriftAxis) bool {
	for _, x := range list {
		if x == a {
			return true
		}
	}
	return false
}

// effectiveDriftCfg 填平非法配置，返回可直接使用的配置副本（不修改入参）。
//
// 零值契约：Floor / Partial / Neutral 的 0 值一律视为「未设置」并回落到
// 保守缺省（0.15 / 0.60 / 1.00）。Go 的零值无法与「显式传 0」区分，
// 取保守侧是因为「缺失中性不衰减」是北极星安全语义——若 0 生效，
// 所有无参考系的存量记忆会被集体打到地板。
func effectiveDriftCfg(cfg DriftConfig) DriftConfig {
	def := DefaultDriftConfig()
	out := cfg
	if out.Floor <= 0 {
		out.Floor = def.Floor
	}
	if out.Floor > 1 {
		out.Floor = 1
	}
	if out.Partial <= 0 {
		out.Partial = def.Partial
	}
	if out.Partial > 1 {
		out.Partial = 1
	}
	if out.Neutral <= 0 {
		out.Neutral = def.Neutral
	}
	if out.Neutral > 1 {
		out.Neutral = 1
	}
	axes := make([]DriftAxis, 0, len(out.Axes))
	for _, a := range out.Axes {
		if a >= 0 && int(a) < axisCount && !driftAxisContains(axes, a) {
			axes = append(axes, a)
		}
	}
	if len(axes) == 0 {
		axes = DefaultDriftAxes()
	}
	out.Axes = axes
	return out
}

// driftScoreOf 单轴判定 → 得分。
func driftScoreOf(v DriftVerdict, cfg DriftConfig) float64 {
	switch v {
	case verdictEqual:
		return 1.0
	case verdictPartial:
		return cfg.Partial
	case verdictMissing:
		return cfg.Neutral
	case verdictDrift:
		return cfg.Floor
	}
	return 1.0
}

// driftAxisValue 取某轴的已归一化值。
func driftAxisValue(a DriftAxis, qf *ReferenceFrame) string {
	if qf == nil {
		return ""
	}
	switch a {
	case AxisEntity:
		return driftNormalize(qf.Entity)
	case AxisScene:
		return driftNormalize(qf.Scene)
	case AxisTask:
		return driftNormalize(qf.Task)
	}
	return ""
}

// DriftOf 计算记忆相对查询参考系的漂移结论。
//
// 规则：逐轴判定（equal / partial / missing / drift）→ 逐轴得分 → 乘积 →
// clamp 到 [Floor, 1.0]。query 或 mem 为 nil 时全轴缺失，Factor=Neutral
// （默认 1.0），即完全不衰减。列表字段恒为非 nil 空切片，便于断言。
func DriftOf(query *ReferenceFrame, mem *Memory, cfg DriftConfig) ReferenceDrift {
	cfg = effectiveDriftCfg(cfg)
	var mf *ReferenceFrame
	if mem != nil {
		f := mem.ReferenceFrame
		mf = &f
	}

	res := ReferenceDrift{
		PerAxis:  make(map[DriftAxis]float64),
		Verdicts: make(map[DriftAxis]DriftVerdict),
		Matched:  []DriftAxis{},
		Partial:  []DriftAxis{},
		Missing:  []DriftAxis{},
		Drifted:  []DriftAxis{},
	}
	product := 1.0
	for _, a := range cfg.Axes {
		v := driftVerdictOf(driftAxisValue(a, query), driftAxisValue(a, mf))
		s := driftScoreOf(v, cfg)
		res.PerAxis[a] = s
		res.Verdicts[a] = v
		product *= s
		switch v {
		case verdictEqual:
			res.Matched = append(res.Matched, a)
		case verdictPartial:
			res.Partial = append(res.Partial, a)
		case verdictMissing:
			res.Missing = append(res.Missing, a)
		case verdictDrift:
			res.Drifted = append(res.Drifted, a)
		}
	}
	// 轴序按 entity < scene < task 收集，无需再排序。
	factor := product
	if factor < cfg.Floor {
		factor = cfg.Floor
	}
	if factor > 1 {
		factor = 1
	}
	res.Factor = factor
	return res
}

// DriftReport 生成单行审计文本（确定性，字段按固定轴序）。
//
// 格式：drift=0.150[entity=drift,scene=missing,task=equal]
func DriftReport(d ReferenceDrift) string {
	parts := make([]string, 0, axisCount)
	for a := DriftAxis(0); a < DriftAxis(axisCount); a++ {
		if _, ok := d.PerAxis[a]; ok {
			parts = append(parts, axisLabels[a]+"="+d.Verdicts[a].String())
		}
	}
	return fmt.Sprintf("drift=%.3f[%s]", d.Factor, strings.Join(parts, ","))
}

// DriftAwareRank 在候选召回分上做**乘性**漂移衰减，产出可审计的排序结果。
//
// 契约：
//   - 只衰减不加分：Score = RecallScore × Factor，永不高于原始召回分；
//   - 不改输入：不修改传入的 RecalledCandidate 或其 Memory；
//   - 确定性：按调整后 Score 降序；同分按 MemoryID 字典序（全序）；
//   - 稳健：Memory 为 nil 的候选直接跳过；空输入返回非 nil 空切片；
//   - 审计：每条结果的 Reason 填 DriftReport 文本（既有字段，语义为原因说明）；
//   - 不进默认路径：本函数是旁路算子，不影响 RankFusion 的 RRF / 投票结果。
func DriftAwareRank(qf *ReferenceFrame, cands []RecalledCandidate, cfg DriftConfig) []FusedResult {
	out := make([]FusedResult, 0, len(cands))
	for _, c := range cands {
		if c.Memory == nil {
			continue
		}
		d := DriftOf(qf, c.Memory, cfg)
		score := c.RecallScore * d.Factor
		out = append(out, FusedResult{
			MemoryID:     c.Memory.MemoryID,
			Fact:         c.Memory.Fact,
			Layer:        c.Layer,
			Score:        score,
			Source:       c.Memory.Source,
			Confidence:   c.Memory.Confidence,
			IsHistorical: c.Memory.IsHistorical,
			ValidFrom:    c.Memory.ValidFrom,
			ValidUntil:   c.Memory.ValidUntil,
			SupersededBy: c.Memory.SupersededBy,
			Memory:       c.Memory,
			Reason:       DriftReport(d),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].MemoryID < out[j].MemoryID
	})
	return out
}
