// R15-B 路：FlushPlan → L1 落库记录的「映射定义」（只定义，不执行）。
//
// 真源：lightmem.md §3.5.1（R14 的 DecideFlush 纯决策）+ R15 任务书 B.3。
//
// 背景：R14 的 DecideFlush 只产出 FlushPlan（待落库 id、触发理由、留 L0 的 id），
// 全库生产调用数为零、且「Plan 到 L1 记录」这一步没人定义。本文件把这一步显式化：
// 给定 FlushPlan 与 L0 记忆来源，产出「该写什么」的 L1 落库记录描述。
//
// 红线遵守（R15）：
//   - 只增不改：本文件为全新文件，不修改 lifecycle.go / flush.go / 任何既有字段语义；
//   - recorded_at 保留：§4.1 双时态唯一主字段，只增不改，禁止刷成 flush 时刻；
//   - layer 走既有 ClassifyLayer：不在本文件重写分层逻辑；
//   - memory_kind 映射表显式给出（L0 捕获项 → fact / goal / procedure）；
//   - 仍然不写库：本文件只产出结构，执行交给既有写路径，不碰 StorageBackend、无 I/O。
package kernel

import (
	"sort"
	"time"
)

// FlushMemSource 按 ID 取出 L0 记忆的能力（由调用方注入）。
//
// 设计要点：flush_pipeline 不持有、不调用任何 StorageBackend——它只接受一个
// 最小查询接口，使「取数」与「写库」都在调用方侧，本文件保持纯函数、
// 无 I/O、无随机、可单测。测试用接口桩（不实现 StorageBackend）即可证明
// 未触碰存储后端。
type FlushMemSource interface {
	GetL0Memory(id string) (*Memory, bool)
}

// L0KindToL1Kind 显式映射表：L0 捕获项 kind → L1 落库 kind。
//
// 依据 §3.5.1「四类触发」沉淀语义与 §2.3 分层：
//   - event / preference 会话态 / 偏好沉淀为 fact（长期可检索的事实）；
//   - goal 长期保留（§2.3 直接落 L2）；
//   - procedure 流程沉淀为 procedure。
//
// 表穷举全部 MemoryKind；未列明的 kind 回落 UnknownKindFallback（默认 KindFact）。
var L0KindToL1Kind = map[MemoryKind]MemoryKind{
	KindEvent:      KindFact,
	KindFact:       KindFact,
	KindGoal:       KindGoal,
	KindProcedure:  KindProcedure,
	KindPreference: KindFact,
}

// FlushMappingConfig 映射可调参数（预留，当前仅 UnknownKindFallback 必填语义）。
type FlushMappingConfig struct {
	// UnknownKindFallback 未列明 kind 的回落值；零值回落 KindFact。
	UnknownKindFallback MemoryKind
}

// DefaultFlushMappingConfig 返回默认映射配置。
func DefaultFlushMappingConfig() FlushMappingConfig {
	return FlushMappingConfig{UnknownKindFallback: KindFact}
}

// FlushRecord 「该写什么」的 L1 落库记录描述（不写库）。
type FlushRecord struct {
	// MemoryID 与待落库项一致（输出按 MemoryID 字典序，与 plan 输入序无关）。
	MemoryID string
	// Memory 原始 L0 记忆（逐位未改；由调用方决定如何落库）。
	Memory *Memory
	// RecordedAt 原始值，逐位保留（§4.1 双时态：只增不改、唯一主字段）。
	RecordedAt time.Time
	// Layer 由既有 ClassifyLayer(m) 判定，不重写分层逻辑。
	Layer MemoryLayer
	// TargetKind L0KindToL1Kind[m.MemoryKind]，显式映射结果。
	TargetKind MemoryKind
	// Reasons 从 plan 透传（按枚举序稳定输出），回答「为什么此刻落库」。
	Reasons []FlushReason
}

// resolveL1Kind 查表返回 L0 项对应的 L1 kind；未列明回落 cfg.UnknownKindFallback。
func resolveL1Kind(m *Memory, cfg FlushMappingConfig) MemoryKind {
	if m == nil {
		return cfg.UnknownKindFallback
	}
	if k, ok := L0KindToL1Kind[m.MemoryKind]; ok {
		return k
	}
	return cfg.UnknownKindFallback
}

// MapFlushToL1 把 FlushPlan 映射为 L1 落库记录描述（只定义，不执行）。
//
// 规则：
//  1. 只映射 plan.Items（待落库项）；HoldItems 显式排除（留 L0，不入 L1）。
//  2. 逐 ID 查 FlushMemSource；查不到的 ID 跳过（不臆造记录）。
//  3. RecordedAt 逐位保留原始值；Layer 走 ClassifyLayer；TargetKind 走映射表。
//  4. 输出按 MemoryID 字典序（确定性、可 diff）。
//  5. 纯函数：无 I/O、无随机、不触碰 StorageBackend、不改 Memory。
func MapFlushToL1(plan FlushPlan, src FlushMemSource, cfg FlushMappingConfig) []FlushRecord {
	if src == nil {
		return nil
	}
	// 去重（防御性，plan.Items 已字典序但容许调用方直接构造）。
	seen := map[string]bool{}
	ids := make([]string, 0, len(plan.Items))
	for _, id := range plan.Items {
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make([]FlushRecord, 0, len(ids))
	for _, id := range ids {
		m, ok := src.GetL0Memory(id)
		if !ok || m == nil {
			continue
		}
		out = append(out, FlushRecord{
			MemoryID:   id,
			Memory:     m,
			RecordedAt: m.RecordedAt,
			Layer:      ClassifyLayer(m),
			TargetKind: resolveL1Kind(m, cfg),
			Reasons:    plan.Reasons,
		})
	}
	return out
}
