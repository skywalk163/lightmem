// R15-B 路：漂移衰减 / 默认融合的「选路 + 委托」分发器。
//
// 真源：lightmem.md §5.3.1（R14 的 DriftAwareRank 旁路算子）+ R15 任务书 B.2。
//
// 背景：R14 把 DriftAwareRank 作为旁路算子交付，全库生产调用数为零——
// 「代码就位 ≠ 能力生效」。本文件把它的接线做成 **opt-in、默认关** 的分发开关：
//   - Mode=Default → 逐位委托既有 (rf *RankFusion).Rank（R14 基线，逐位不变）；
//   - Mode=Drift   → 逐位委托 R14 的 DriftAwareRank（不重写衰减，不第二套实现）。
//
// 红线遵守（R15）：
//   - 只增不改：本文件为全新文件，不修改 rank_fusion.go / 任何既有函数；
//   - 默认关：DefaultDriftPipelineConfig().Mode == RankModeDefault（红线 11）；
//   - 不重写：本文件不实现第二套衰减 / 第二套 RRF / 排序打分，一律调用既有函数；
//   - 确定性：无随机、无时间依赖、无 I/O；
//   - 安全：nil qf / 空候选 / rf==nil 均不 panic，按契约返回或报错。
package kernel

import (
	"context"
	"fmt"
)

// RankMode 排序路径选择。
type RankMode string

const (
	// RankModeDefault 默认路径：委托既有 (rf *RankFusion).Rank（R14 基线，逐位不变）。
	RankModeDefault RankMode = "default"
	// RankModeDrift 显式开启：委托 R14 的 DriftAwareRank（乘性漂移衰减）。
	RankModeDrift RankMode = "drift"
)

// DriftPipelineConfig 漂移衰减接线配置。
type DriftPipelineConfig struct {
	// Mode 排序路径；默认 RankModeDefault（红线 11：接线只许默认关）。
	Mode RankMode
	// DriftCfg 复用 R14 的 Floor=0.15 / Partial=0.60 / Neutral=1.00。
	DriftCfg DriftConfig
	// OnlyIfAxisAnnotated 默认 true：查询侧三轴（entity/scene/task）全缺时，
	// 即使 Mode=Drift 也回落 RankModeDefault，避免对无参考系数据空转衰减
	// （§二.3：holdout_v2 上三轴只有 scene 有标注，开态行为零变化）。
	OnlyIfAxisAnnotated bool
}

// DefaultDriftPipelineConfig 返回保守缺省：默认关 + 三轴全开缺省衰减 + 仅标注才衰减。
func DefaultDriftPipelineConfig() DriftPipelineConfig {
	return DriftPipelineConfig{
		Mode:                RankModeDefault,
		DriftCfg:            DefaultDriftConfig(),
		OnlyIfAxisAnnotated: true,
	}
}

// Enabled 报告当前配置是否处于显式开启（drift）态。
func (c DriftPipelineConfig) Enabled() bool {
	return c.Mode == RankModeDrift
}

// Resolve 解析实际生效的排序路径。
//
// 仅当 Mode==RankModeDrift 且 OnlyIfAxisAnnotated==true 且查询侧三轴全缺时，
// 回落 RankModeDefault；其余情况原样采用 Mode。供调用方与测试显式断言选路结果。
func (c DriftPipelineConfig) Resolve(qf *ReferenceFrame) RankMode {
	if c.Mode != RankModeDrift || !c.OnlyIfAxisAnnotated {
		return c.Mode
	}
	if qf == nil {
		return RankModeDefault
	}
	if qf.Entity == "" && qf.Scene == "" && qf.Task == "" {
		return RankModeDefault
	}
	return RankModeDrift
}

// RankViaPipeline 选路 + 委托：
//   - 默认态逐位委托 rf.Rank；
//   - 漂移态逐位委托 R14 的 DriftAwareRank（不重写衰减逻辑）。
//
// 本文件禁止复制衰减 / 排序 / 打分逻辑——一律调用既有函数。这是「接线只做选路 +
// 委托」的核心约束：RankViaPipeline 不含第二套衰减实现，改变 DriftConfig.Floor
// 后，本函数漂移态输出与直接调用 DriftAwareRank 同步变化（共用同一份逻辑）。
//
// 错误处理：rf==nil 直接报错（不 panic）；其余输入（nil qf / 空候选）均安全委托。
func RankViaPipeline(ctx context.Context, rf *RankFusion, qf *ReferenceFrame,
	query string, cands []RecalledCandidate, cfg DriftPipelineConfig) ([]FusedResult, error) {
	if rf == nil {
		return nil, fmt.Errorf("RankViaPipeline: rf 为 nil，无法委托默认融合")
	}
	switch cfg.Resolve(qf) {
	case RankModeDrift:
		// 漂移态：逐位委托 R14 旁路算子；query / ctx 不参与衰减计算。
		return DriftAwareRank(qf, cands, cfg.DriftCfg), nil
	default:
		// 默认态：逐位委托 R14 基线排序；与 rf.Rank 逐位一致。
		return rf.Rank(ctx, query, cands), nil
	}
}
