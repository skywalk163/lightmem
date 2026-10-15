// kernel/rank_fusion_monty.go — R13-A 融合算子：只加不减 + 相对容差（Monty 建议 2）。
//
// 理论依据（lightmem/monty.md §4.3，Thousand Brains Project，MIT 许可）：
//
//	Monty 特征证据 feature_evidence = clip(tolerance - diff, 0, inf) / tolerance——
//	不匹配**记 0，绝不给负分**；搜索半径内**取 max 而非平均**。
//
// 与既有实现的边界（任务书红线 1「只增不改」）：
//   - 本文件为**新增融合模式**，不修改 rank_fusion.go 的 rrfRank /
//     consensusVoteRank / Fuse / VoteFusion，也不改 DefaultRankFusion 的
//     默认装配与权重语义；默认路径（RankFusion.Rank / RankFusion.Fuse）
//     行为逐位不变；
//   - 不新增 / 不改动 Memory 既有 Schema 字段：MontyEvidence / MontyVerdict
//     均为计算期结构，**不落库**，缺省不生效。
//
// 确定性（任务书红线 4）：无随机源；平局一律按 MemoryID 字典序打破，
// 不依赖 map 迭代序与 time.Now。
package kernel

import (
	"context"
	"fmt"
	"sort"
)

// 证据差值（diff）的度量口径。
const (
	// MontyDiffRelative 相对口径（默认）：diff = best - norm，其中 norm ∈ [0,1]
	// 为该信号对本候选的归一化分（该信号最优者为 1）。归一化后 diff 天然有界，
	// 与 §3.4「接近融合相对条件 (top1-top2)/top1 < 20%」同一量纲。
	MontyDiffRelative = "relative"
	// MontyDiffAbsolute 绝对口径：diff = bestRaw - raw（原始分差），
	// 供调用方按原始信号量纲显式指定容差（如 BM25 分数差）。
	MontyDiffAbsolute = "absolute"
)

// 默认参数（§3.4.1）。tolerance 默认 0.2 与 §3.4 既有 20% 相对条件一致。
const (
	// DefaultMontyTolerance 相对容差默认值。
	DefaultMontyTolerance = 0.2
	// DefaultMontyAbsoluteFloor 绝对分下限默认值（保留 lightmem 既有 0.6 硬门槛）。
	DefaultMontyAbsoluteFloor = 0.6
)

// MonotonicConfig 只加不减融合的参数。
type MonotonicConfig struct {
	// Tolerance 相对容差 ∈ (0,1]；<=0 时取 DefaultMontyTolerance。
	// diff ≤ tolerance 时线性得分 (tolerance-diff)/tolerance，diff > tolerance 记 0。
	Tolerance float64
	// AbsoluteFloor 绝对分下限；<=0 时取 DefaultMontyAbsoluteFloor。
	// top1 < AbsoluteFloor → 禁止融合，返回单条并标低置信（保留 lightmem 差异）。
	AbsoluteFloor float64
	// DiffMode 见 MontyDiffRelative / MontyDiffAbsolute；空值或未知值取相对口径。
	DiffMode string
}

// withDefaults 补默认值。绝对下限优先沿用引擎自身的 rf.AbsoluteFloor（保持与
// §3.4 默认路径同一门槛），仅当引擎未设置时才回落 0.6。
func (c MonotonicConfig) withDefaults(engineFloor float64) MonotonicConfig {
	if c.Tolerance <= 0 {
		c.Tolerance = DefaultMontyTolerance
	}
	if c.AbsoluteFloor <= 0 {
		if engineFloor > 0 {
			c.AbsoluteFloor = engineFloor
		} else {
			c.AbsoluteFloor = DefaultMontyAbsoluteFloor
		}
	}
	if c.DiffMode != MontyDiffAbsolute {
		c.DiffMode = MontyDiffRelative
	}
	return c
}

// MontyEvidence 单候选在单信号上的证据明细（计算期结构，不落库）。
type MontyEvidence struct {
	// Signal 信号名。
	Signal string
	// Weight 该信号权重。
	Weight float64
	// Raw 该信号给本候选的原始分（缺失为 0）。
	Raw float64
	// Normalized 相对该信号最优者的归一化分 ∈ [0,1]（缺失为 0）。
	Normalized float64
	// Diff 证据差值（口径由 MonotonicConfig.DiffMode 决定）。
	Diff float64
	// Evidence clip(tolerance-diff, 0, inf)/tolerance 的结果 ∈ [0,1]。
	// 恒 ≥ 0：本模式**不存在负证据**（只加不减）。
	Evidence float64
}

// MontyVerdict 单候选的只加不减融合结论。
type MontyVerdict struct {
	MemoryID string      `json:"memory_id"`
	Fact     string      `json:"fact"`
	Layer    MemoryLayer `json:"layer"`
	// Score 归一化只加不减融合分 ∈ [0,1]：Σ(w×evidence) / Σw（仅计有信息的信号）。
	Score float64 `json:"score"`
	// Evidences 逐信号证据明细（便于审计「为什么记 0」）。
	Evidences []MontyEvidence `json:"evidences,omitempty"`
	// LowConfidence top1 低于绝对下限时为 true。
	LowConfidence bool `json:"low_confidence,omitempty"`
	// FusedWith 与本体并列融合的其他候选 ID（接近融合时互填）。
	FusedWith []string `json:"fused_with,omitempty"`
	// Reason 判决理由（可读）。
	Reason string  `json:"reason,omitempty"`
	Memory *Memory `json:"memory,omitempty"`
}

// MontyFusionResult 只加不减融合的整体结果。
type MontyFusionResult struct {
	// Ranked 判决后返回的候选（唯一最优 = 1 条；接近融合 = 多条）。
	Ranked []MontyVerdict `json:"ranked,omitempty"`
	// All **全部**候选的逐条证据分（降序，含未入选 Ranked 者）。
	// 用于审计「某候选为什么记 0 / 被绝对下限挡住」，也保证判决可逐条核对。
	All []MontyVerdict `json:"all,omitempty"`
	// Tolerance / AbsoluteFloor 本次生效的参数（含默认值填充结果）。
	Tolerance     float64 `json:"tolerance"`
	AbsoluteFloor float64 `json:"absolute_floor"`
	// DiffMode 本次生效的差值口径。
	DiffMode string `json:"diff_mode"`
	// Top1Score 全量候选的最高融合分（判决前，便于上层观察边际）。
	Top1Score float64 `json:"top1_score"`
	// FloorPass top1 是否通过绝对下限。
	FloorPass bool `json:"floor_pass"`
	// Fused 是否发生了接近融合。
	Fused bool `json:"fused"`
	// MissingSignals 本次查询中「无正证据」或「未启用」而被跳过的信号名（字典序）。
	MissingSignals []string `json:"missing_signals,omitempty"`
}

// MontyEvidenceScore 只加不减证据分：clip(tolerance-diff, 0, inf) / tolerance。
//
// 语义（严格对齐 monty.md §4.3 与 lightmem.md §3.4.1）：
//   - diff <= 0          → 1（完全匹配，取最大证据）
//   - 0 < diff < tol     → (tolerance-diff)/tolerance，线性衰减 ∈ (0,1)
//   - diff >= tolerance  → 0（**记 0，绝不记负分**）
//   - tolerance <= 0     → 0（容差非正视为无有效证据；不做除零）
func MontyEvidenceScore(diff, tolerance float64) float64 {
	if tolerance <= 0 {
		return 0
	}
	if diff <= 0 {
		return 1
	}
	if diff >= tolerance {
		return 0
	}
	return (tolerance - diff) / tolerance
}

// ------------------------------------------------------------------
// 核心：FuseMonotonic
// ------------------------------------------------------------------

// FuseMonotonic 以「只加不减 + 相对容差」口径融合候选，**默认路径不调用**。
//
// 与 RankFusion.Fuse 的差异（只增不改，互不影响）：
//   - 证据只加不减：每个信号对本候选只贡献 ∈[0,1] 的正向证据，信号缺失/超容差记 0，
//     任何情况下不产生负分（不会因「某信号强烈反对」而把候选压到 0 以下）；
//   - 相对容差显式化：容差内的差值得线性证据，超容差**记 0 而非扣分**；
//   - 保留 lightmem 的绝对下限：top1 分 < AbsoluteFloor 时禁止融合，返回单条并标低置信。
//
// 判决语义（与 §3.4 三态决策同形，但走独立算子）：
//  1. top1 分 < AbsoluteFloor → 返回单条，LowConfidence=true；
//  2. 否则若 top1 与 top2 的分差比例 < tolerance，返回多条并互填 FusedWith；
//  3. 否则返回单条（唯一最优）。
//
// 依赖 rf.Signals / s.Weight：权重 <=0 视为该信号无信息（跳过但不记 0 惩罚）；
// Signals 为空时返回空结果，不 panic。
func (rf *RankFusion) FuseMonotonic(ctx context.Context, query string, cands []RecalledCandidate, cfg MonotonicConfig) MontyFusionResult {
	cfg = cfg.withDefaults(rf.AbsoluteFloor)
	out := MontyFusionResult{
		Tolerance:     cfg.Tolerance,
		AbsoluteFloor: cfg.AbsoluteFloor,
		DiffMode:      cfg.DiffMode,
	}
	if len(rf.Signals) == 0 || len(cands) == 0 {
		return out
	}

	// 1) 候选去重（保序）+ 各信号独立打分 + 归一化（与 rrfRank 同一套信号函数，保证可比）。
	byID := make(map[string]*Memory, len(cands))
	ids := make([]string, 0, len(cands))
	for _, c := range cands {
		if c.Memory == nil || c.Memory.MemoryID == "" {
			continue
		}
		if _, dup := byID[c.Memory.MemoryID]; !dup {
			ids = append(ids, c.Memory.MemoryID)
		}
		byID[c.Memory.MemoryID] = c.Memory
	}
	if len(ids) == 0 {
		return out
	}
	rawBySig := make(map[string]map[string]float64, len(rf.Signals))
	normBySig := make(map[string]map[string]float64, len(rf.Signals))
	bestRaw := make(map[string]float64, len(rf.Signals))
	for _, s := range rf.Signals {
		raw := s.Fn(ctx, query, cands)
		rawBySig[s.Name] = raw
		normBySig[s.Name] = normalizeMax(raw)
		best := 0.0
		for _, v := range raw {
			if v > best {
				best = v
			}
		}
		bestRaw[s.Name] = best
	}

	// 2) 逐候选聚合只加不减证据。
	missing := map[string]bool{}
	verdicts := make([]MontyVerdict, 0, len(ids))
	for _, id := range ids {
		m := byID[id]
		v := MontyVerdict{MemoryID: id, Fact: m.Fact, Layer: m.Layer, Memory: m}
		var wsum, esum float64
		for _, s := range rf.Signals {
			w := s.Weight
			if w <= 0 {
				// 权重非正：该信号不参与计分（无信息，而非 0 分证据）。
				missing[s.Name] = true
				continue
			}
			rawV := rawBySig[s.Name][id]
			normV := normBySig[s.Name][id]
			if rawV <= 0 && normV <= 0 {
				// 信号缺失或对本候选无命中 → 记 0，不扣分（addition-only）。
				missing[s.Name] = true
				v.Evidences = append(v.Evidences, MontyEvidence{
					Signal: s.Name, Weight: w, Raw: 0, Normalized: 0,
					Diff: cfg.Tolerance, Evidence: 0,
				})
				wsum += w
				continue
			}
			diff := 1.0 - normV // 相对口径默认：与本信号最优者（归一化 1.0）的差
			if cfg.DiffMode == MontyDiffAbsolute {
				diff = bestRaw[s.Name] - rawV
			}
			ev := MontyEvidenceScore(diff, cfg.Tolerance)
			v.Evidences = append(v.Evidences, MontyEvidence{
				Signal: s.Name, Weight: w, Raw: rawV, Normalized: normV,
				Diff: diff, Evidence: ev,
			})
			wsum += w
			esum += w * ev
		}
		if wsum > 0 {
			// 分母只含有信息的信号权重：弱信号缺失不稀释，也不减分。
			v.Score = clampUnit(esum / wsum)
		}
		verdicts = append(verdicts, v)
	}
	for name := range missing {
		out.MissingSignals = append(out.MissingSignals, name)
	}
	sort.Strings(out.MissingSignals)

	// 3) 排序：融合分降序，平局按 MemoryID 字典序（确定性，无随机源）。
	sort.SliceStable(verdicts, func(i, j int) bool {
		if verdicts[i].Score != verdicts[j].Score {
			return verdicts[i].Score > verdicts[j].Score
		}
		return verdicts[i].MemoryID < verdicts[j].MemoryID
	})
	if len(verdicts) == 0 {
		return out
	}
	out.All = verdicts
	out.Top1Score = verdicts[0].Score
	out.FloorPass = verdicts[0].Score >= cfg.AbsoluteFloor

	// 4) 绝对下限：低于下限禁止融合（保留 lightmem 既有硬门槛，Monty 无此约束）。
	if !out.FloorPass {
		verdicts[0].LowConfidence = true
		verdicts[0].Reason = fmt.Sprintf(
			"top1=%.3f < absolute_floor=%.2f: single, low confidence (monotonic)",
			verdicts[0].Score, cfg.AbsoluteFloor)
		out.Ranked = verdicts[:1]
		return out
	}
	if len(verdicts) == 1 {
		verdicts[0].Reason = "single_candidate: unique optimum (monotonic)"
		out.Ranked = verdicts[:1]
		return out
	}

	// 5) 接近融合：分差比例 < tolerance → 并列融合（不覆盖、不静默选一个）。
	top1, top2 := verdicts[0], verdicts[1]
	ratio := 1.0
	if top1.Score > 0 {
		ratio = (top1.Score - top2.Score) / top1.Score
	}
	if ratio < cfg.Tolerance {
		picked := []MontyVerdict{top1, top2}
		for _, r := range verdicts[2:] {
			if top1.Score <= 0 {
				break
			}
			if (top1.Score-r.Score)/top1.Score < cfg.Tolerance {
				picked = append(picked, r)
			}
		}
		pids := make([]string, 0, len(picked))
		for _, p := range picked {
			pids = append(pids, p.MemoryID)
		}
		for i := range picked {
			fw := make([]string, 0, len(pids)-1)
			for _, id := range pids {
				if id != picked[i].MemoryID {
					fw = append(fw, id)
				}
			}
			picked[i].FusedWith = fw
			picked[i].Reason = fmt.Sprintf(
				"monotonic: gap %.1f%% < tolerance %.1f%% and top1=%.3f >= floor %.2f: fused multiple",
				ratio*100, cfg.Tolerance*100, top1.Score, cfg.AbsoluteFloor)
		}
		out.Fused = true
		out.Ranked = picked
		return out
	}

	top1.Reason = fmt.Sprintf(
		"unique optimum (monotonic: gap %.1f%% >= tolerance %.1f%%)",
		ratio*100, cfg.Tolerance*100)
	out.Ranked = []MontyVerdict{top1}
	return out
}

// clampUnit 把 v 夹到 [0,1]。
func clampUnit(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
