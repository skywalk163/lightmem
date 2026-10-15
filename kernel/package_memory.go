// 包记忆抽取（结构化：元数据 / 配置 / 变更记录）
// 真源：lightmem.md §6.2 应用包生态；R11-B 任务书 B.2
// 光明语言：把声明式包 manifest 转成可被检索/沉淀的结构化记忆，包成为"被记忆的一等实体"。
//
// 设计要点：
//   - 每个包抽取 3 条记忆：metadata（包是什么/用途）、config（配置/权限/入口）、changelog（变更记录）。
//     三条 idempotency_key 各自稳定，重复抽取幂等不重复写入。
//   - Fact 文本含包名 + 类型 + 说明 + 信任级 + 权限 + 版本，保证「这个包是干什么的」类查询可命中。
//   - 补齐统一 Schema 的必要默认值（Visibility/Provenance/VectorStatus/Layer/ReferenceFrame），
//     与 kernel 其他写入路径一致；L2 静态层，长期可查。
//   - 兼容既有 API：ExtractPackageMemory(ctx, backend, pkgName) 签名不变，行为由 Manifest 驱动增强。
package kernel

import (
	"context"
	"fmt"
	"log"
	"time"
)

// ExtractPackageMemory 从包抽取结构化记忆并写入记忆库。
//   - pkgName 须命中 FirstPartyPackages（首批 8 包）或通过 manifest 注册；
//   - 返回成功写入（含幂等命中）的记忆条数。
//
// 兼容性：签名与 package.go 初版一致；行为升级为三视图（元数据/配置/变更记录）。
func ExtractPackageMemory(ctx context.Context, backend StorageBackend, pkgName string) (int, error) {
	mf, ok := FirstPartyPackages[pkgName]
	if !ok {
		return 0, fmt.Errorf("unknown package: %s", pkgName)
	}
	return ExtractPackageMemoryFromManifest(ctx, backend, &mf)
}

// ExtractPackageMemoryFromManifest 从任意 PackageManifest 抽取包记忆。
// 适用于家族档 8 包及后续社区包。返回写入/幂等命中的记忆条数。
//
// R13-A 修复（R12 遗留缺陷）：幂等依赖 (tenant,user,idempotency_key) 三元组。
// 原实现每次调用都生成新 MemoryID，SQLite 的 Upsert 幂等分支（同 MemoryID 且
// 同 idempotency_key 才复用）永不命中，重复抽取会不断新增行。此处按代码库
// 既有先例（§4.3 写策略 / router_tools 写入路径）先用 IdempotencyLookup 探测，
// 命中则复用既有 MemoryID，使 Upsert 走 version 递增分支而不新增行。
func ExtractPackageMemoryFromManifest(ctx context.Context, backend StorageBackend, mf *PackageManifest) (int, error) {
	if mf == nil {
		return 0, fmt.Errorf("nil package manifest")
	}
	if mf.Name == "" {
		return 0, fmt.Errorf("package name required")
	}
	if err := ValidateManifest(mf); err != nil {
		return 0, fmt.Errorf("invalid manifest %s: %w", mf.Name, err)
	}

	now := time.Now().UTC()
	total := 0
	lk, hasLookup := backend.(IdempotencyLookup)

	memories := []*Memory{
		// 元数据：包是什么、用途、类型、信任级
		pkgMemory(mf, "metadata", KindFact,
			fmt.Sprintf("包 %s 是 %s 类型的%s包（信任级 %s）：%s。可产出记忆类型：%s。",
				mf.Name, mf.Type, trustCN(mf.Trust), trustCN(mf.Trust), mf.Description, joinOr(mf.MemoryTypes, "全部")),
			0.95, now),
		// 配置：入口 + 权限
		pkgMemory(mf, "config", KindFact,
			fmt.Sprintf("包 %s 配置：入口 %s；权限 %s。",
				mf.Name, mf.Entrypoint, joinOr(mf.Permissions, "无")),
			0.9, now),
		// 变更记录：版本 + 作者 + 依赖
		pkgMemory(mf, "changelog", KindEvent,
			fmt.Sprintf("包 %s 变更：已装配 v%s（作者 %s），依赖 %s。",
				mf.Name, mf.Version, mf.Author, joinOr(mf.Dependencies, "无")),
			0.85, now),
	}

	for _, m := range memories {
		// 幂等：命中既有 (tenant,user,idem_key) 则复用其 MemoryID，
		// 使 Upsert 走「同 ID + 同幂等键 → version 递增」分支，不新增行。
		if hasLookup {
			if ex, err := lk.FindByIdempotencyKey(ctx, m.TenantID, m.UserID, m.IdempotencyKey); err == nil && ex != nil {
				m.MemoryID = ex.MemoryID
			}
		}
		if err := backend.Upsert(ctx, m); err != nil {
			// 幂等命中不计为失败：返回已成功条数
			return total, fmt.Errorf("extract %s: %w", m.IdempotencyKey, err)
		}
		total++
	}

	log.Printf("[package] extracted %d memories from %s v%s", total, mf.Name, mf.Version)
	return total, nil
}

// pkgMemory 构造一条包记忆，补齐统一 Schema 默认值。
func pkgMemory(mf *PackageManifest, view string, kind MemoryKind, fact string, confidence float64, now time.Time) *Memory {
	ref := ReferenceFrame{
		Category: "package",
		Entity:   mf.Name,
		Project:  "lightmem-family",
		Agent:    "package-lifecycle",
	}
	return &Memory{
		MemoryID:        newMemoryID(),
		IdempotencyKey:  fmt.Sprintf("pkg:%s:%s", mf.Name, view),
		TenantID:        "system",
		UserID:          "system",
		AgentID:         "package-lifecycle",
		Visibility:      VisTenant,
		PermissionLevel: "internal",
		MemoryKind:      kind,
		Fact:            fact,
		ReferenceFrame:  ref,
		RecordedAt:      now,
		Confidence:      confidence,
		Source:          "package:" + mf.Name,
		Version:         1,
		Provenance:      ProvNormal,
		VectorStatus:    VectorPending,
		Layer:           LayerL2,
	}
}

// trustCN 信任级中文描述（用于可读事实文本）。
func trustCN(t PackageTrust) string {
	switch t {
	case TrustOfficial:
		return "官方"
	case TrustVerified:
		return "已核验"
	case TrustCommunity:
		return "社区"
	default:
		return string(t)
	}
}

// joinOr 切片转中文顿号串，空时返回 fallback。
func joinOr(items []string, fallback string) string {
	if len(items) == 0 {
		return fallback
	}
	s := ""
	for i, it := range items {
		if i > 0 {
			s += "、"
		}
		s += it
	}
	return s
}
