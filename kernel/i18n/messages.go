// kernel/i18n/messages.go — R8-A（P7-E）静态三语翻译表（确定性词典）。
//
// 设计目标（任务书 A.1 优先）：
//   - 翻译表是**静态词典**，非 LLM 产物（红线「不调 LLM」）；
//   - 每条消息一个稳定 key，三语翻译；
//   - **默认 locale 中文（任务书 A.1）**：zh-CN 栏 = **真正的中文本地化**，
//     en-US 栏 = 英文，ja-JP 栏 = 日文，三语相互独立、逐语本地化；
//   - **kernel 既有英文硬编码保持不动**（红线「只增不改」）：i18n 化通过
//     **新增**入口（kernel/i18n_hook.go + 接入样板函数）实现，不改既有函数
//     返回值/错误类型/日志格式，因此默认态行为**逐位不变**（不回归）。
//
// 只增不改：本文件不修改 model.go 既有 Schema 字段语义。
package i18n

// MsgKey 消息键（字符串常量，非整数枚举，便于外部引用与扩展）。
type MsgKey string

// 消息键清单（覆盖 kernel 关键错误 / 日志 / 校验提示，共 34 条）。
const (
	// 生命周期（lifecycle.go）
	MsgLayerScannerUnsupported   MsgKey = "layer_scanner_unsupported"    // 后端未实现 LayerScanner
	MsgLayerScannerP2Unsupported MsgKey = "layer_scanner_p2_unsupported" // 后端未实现 LayerScanner（P2 必需）
	MsgLifecycleUpdateFailed     MsgKey = "lifecycle_update_failed"      // 生命周期层迁移失败
	MsgArchiveMoveFailed         MsgKey = "archive_move_failed"          // 归档迁移失败
	MsgBudgetMoveFailed          MsgKey = "budget_move_failed"           // 预算迁移失败
	MsgCoreMarkFailed            MsgKey = "core_mark_failed"             // 核心记忆标记失败
	MsgForgetMoveFailed          MsgKey = "forget_move_failed"           // 遗忘迁移失败
	MsgCompressCreateProcFailed  MsgKey = "compress_create_proc_failed"  // 压缩存储过程创建失败
	MsgScanFailed                MsgKey = "scan_failed"                  // 扫描失败

	// 审计 / 指标（metrics.go）
	MsgAuditAction  MsgKey = "audit_action"  // 审计动作记录
	MsgAuditForget  MsgKey = "audit_forget"  // 审计遗忘记录
	MsgFeedbackCorr MsgKey = "feedback_corr" // 反馈修正候选

	// 排序融合（rank_fusion.go）
	MsgParseWeightsFailed MsgKey = "parse_weights_failed" // 权重解析失败

	// 后端通用（backend_sqlite.go / backend_pg.go）
	MsgDbNotConnected         MsgKey = "db_not_connected"         // 数据库未连接
	MsgIdempotencyUnsupported MsgKey = "idempotency_unsupported"  // 后端不支持幂等查询
	MsgGdprUnsupported        MsgKey = "gdpr_unsupported"         // 后端不支持 GDPR 合规操作
	MsgPgvectorRequiresConfig MsgKey = "pgvector_requires_config" // pgvector 后端需要 storage.pg 配置
	MsgEmptyPort              MsgKey = "empty_port"               // 端口为空
	MsgInvalidPort            MsgKey = "invalid_port"             // 端口非法
	MsgPathNotConfigured      MsgKey = "path_not_configured"      // 路径未配置

	// 记忆 / 权限（model.go / router.go）
	MsgMemoryNotFound        MsgKey = "memory_not_found"        // 记忆未找到
	MsgMemoryNotVisible      MsgKey = "memory_not_visible"      // 记忆对用户不可见
	MsgMemoryTenantRequired  MsgKey = "memory_tenant_required"  // memory_id 与 tenant_id 必填
	MsgCascadeTenantRequired MsgKey = "cascade_tenant_required" // 级联删除需 tenant_id + user_id
	MsgExportTenantRequired  MsgKey = "export_tenant_required"  // 导出需 tenant_id + user_id

	// 校验（profile.go / package.go / temperature.go）
	MsgInvalidName       MsgKey = "invalid_name"       // 名称非法
	MsgInvalidTier       MsgKey = "invalid_tier"       // 档位非法
	MsgInvalidPermLevel  MsgKey = "invalid_perm_level" // 权限级别非法
	MsgInvalidState      MsgKey = "invalid_state"      // 状态迁移非法
	MsgPackageNotFound   MsgKey = "package_not_found"  // 包未找到
	MsgPackageRequired   MsgKey = "package_required"   // 包名必填
	MsgDependencyMissing MsgKey = "dependency_missing" // 依赖未启用

	// 向量嵌入器（embedder_vector.go）
	MsgVectorMetaInvalid MsgKey = "vector_meta_invalid" // 向量 meta 非法
	MsgVectorRequiresVec MsgKey = "vector_requires_vec" // 向量检索需要 embedding 向量
)

// msgEntry 单条消息的静态翻译（三语）。
type msgEntry struct {
	key MsgKey
	zh  string // 简体中文（真本地化，任务书 A.1 默认 locale 中文）
	en  string // 英文
	ja  string // 日文
}

// msgTable 静态翻译表（确定性：slice 线性查找，非 map，避免 map 迭代序污染）。
//
// 注意：占位符（%s / %v / %d / %q / %w 等）必须三语一致，供 fmt.Sprintf 统一插值。
var msgTable = []msgEntry{
	// ---- 生命周期 ----
	{MsgLayerScannerUnsupported, "后端 %s 未实现 LayerScanner", "backend %s does not implement LayerScanner", "バックエンド %s は LayerScanner を実装していません"},
	{MsgLayerScannerP2Unsupported, "后端 %s 未实现 LayerScanner；P2 要求实现", "backend %s does not implement LayerScanner; P2 requires it", "バックエンド %s は LayerScanner を実装していません；P2 で必須です"},
	{MsgLifecycleUpdateFailed, "[生命周期] 更新 %s -> %s 失败：%v", "[lifecycle] update %s -> %s failed: %v", "[ライフサイクル] 更新 %s -> %s 失敗：%v"},
	{MsgArchiveMoveFailed, "[归档] 迁移 %s 到 L3 失败：%v", "[archive] move %s to L3 failed: %v", "[アーカイブ] 移動 %s -> L3 失敗：%v"},
	{MsgBudgetMoveFailed, "[预算] 迁移 %s 到 L3 失败：%v", "[budget] move %s to L3 failed: %v", "[バジェット] 移動 %s -> L3 失敗：%v"},
	{MsgCoreMarkFailed, "[核心] 标记 %s 为核心失败：%v", "[core] mark %s as core failed: %v", "[コア] マーク %s 失敗：%v"},
	{MsgForgetMoveFailed, "[遗忘] 迁移 %s 到 L3 失败：%v", "[forget] move %s to L3 failed: %v", "[忘却] 移動 %s -> L3 失敗：%v"},
	{MsgCompressCreateProcFailed, "[压缩] 创建存储过程失败：%v", "[compress] create procedure failed: %v", "[圧縮] ストアドプロシージャ作成失敗：%v"},
	{MsgScanFailed, "扫描 %s", "scan %s", "スキャン %s"},

	// ---- 审计 / 指标 ----
	{MsgAuditAction, "[审计] action=%s memory_id=%s tenant=%s user=%s actor=%s detail=%s", "[audit] action=%s memory_id=%s tenant=%s user=%s actor=%s detail=%s", "[監査] action=%s memory_id=%s tenant=%s user=%s actor=%s detail=%s"},
	{MsgAuditForget, "[审计] 遗忘 memory_id=%s tenant=%s viewer=%s reason=%s", "[audit] forget memory_id=%s tenant=%s viewer=%s reason=%s", "[監査] 忘却 memory_id=%s tenant=%s viewer=%s reason=%s"},
	{MsgFeedbackCorr, "[反馈] 修正候选 memory_id=%s rejected_reason=%s", "[feedback] correction-candidate memory_id=%s rejected_reason=%s", "[フィードバック] 修正候補 memory_id=%s rejected_reason=%s"},

	// ---- 排序融合 ----
	{MsgParseWeightsFailed, "解析权重 %s", "parse weights %s", "重み解析 %s"},

	// ---- 后端通用 ----
	{MsgDbNotConnected, "数据库未连接", "db not connected", "データベース未接続"},
	{MsgIdempotencyUnsupported, "后端不支持幂等查询", "backend does not support idempotency lookup", "バックエンドは冪等性照会をサポートしていません"},
	{MsgGdprUnsupported, "后端 %s 不支持合规操作（GDPR）", "backend %s does not support compliance operations (GDPR)", "バックエンド %s はコンプライアンス操作（GDPR）をサポートしていません"},
	{MsgPgvectorRequiresConfig, "pgvector 后端需要 storage.pg 配置", "pgvector backend requires storage.pg config", "pgvector バックエンドには storage.pg 設定が必要です"},
	{MsgEmptyPort, "端口为空", "empty port", "ポートが空です"},
	{MsgInvalidPort, "非法端口 %q", "invalid port %q", "無効なポート %q"},
	{MsgPathNotConfigured, "路径未配置", "path not configured", "パスが未設定です"},

	// ---- 记忆 / 权限 ----
	{MsgMemoryNotFound, "记忆 %s 未找到", "memory %s not found", "メモリ %s が見つかりません"},
	{MsgMemoryNotVisible, "记忆 %s 对 %s/%s 不可见", "memory %s not visible to %s/%s", "メモリ %s は %s/%s に不可視です"},
	{MsgMemoryTenantRequired, "memory_id 与 tenant_id 必填", "memory_id and tenant_id required", "memory_id と tenant_id は必須です"},
	{MsgCascadeTenantRequired, "级联删除需要 tenant_id 与 user_id", "tenant_id and user_id required for cascade delete", "カスケード削除には tenant_id と user_id が必要です"},
	{MsgExportTenantRequired, "导出需要 tenant_id 与 user_id", "tenant_id and user_id required for export", "エクスポートには tenant_id と user_id が必要です"},

	// ---- 校验 ----
	{MsgInvalidName, "非法名称：%s（仅小写字母、数字、连字符）", "invalid name: %s (only lowercase, digits, hyphen)", "無効な名前：%s（小文字・数字・ハイフンのみ）"},
	{MsgInvalidTier, "非法档位 %q（可选 solo|family|smb|enterprise）", "invalid tier %q (want solo|family|smb|enterprise)", "無効なティア %q（solo|family|smb|enterprise）"},
	{MsgInvalidPermLevel, "非法权限级别 %q", "permission_level %q invalid", "無効な権限レベル %q"},
	{MsgInvalidState, "非法状态迁移：%s -> %s", "invalid state transition: %s -> %s", "無効な状態遷移：%s -> %s"},
	{MsgPackageNotFound, "包未找到：%s", "package not found: %s", "パッケージが見つかりません：%s"},
	{MsgPackageRequired, "包名必填", "package name required", "パッケージ名は必須です"},
	{MsgDependencyMissing, "依赖未启用：%s", "dependency not enabled: %s", "依存関係が未有効です：%s"},

	// ---- 向量嵌入器 ----
	{MsgVectorMetaInvalid, "向量嵌入器：meta dim=%d count=%d 非法", "vector embedder: meta dim=%d count=%d invalid", "ベクトル埋め込み：meta dim=%d count=%d 無効"},
	{MsgVectorRequiresVec, "向量检索需要 embedding 向量", "vector search requires embedding vector", "ベクトル検索には embedding ベクトルが必要です"},
}

// lookup 线性查找消息键对应的翻译条目（确定性）。
func lookup(key MsgKey) (msgEntry, bool) {
	for _, e := range msgTable {
		if e.key == key {
			return e, true
		}
	}
	return msgEntry{}, false
}

// SupportedKeys 返回全部已定义消息键（确定性排序：按 msgTable 定义序，非 map 序）。
func SupportedKeys() []MsgKey {
	out := make([]MsgKey, 0, len(msgTable))
	for _, e := range msgTable {
		out = append(out, e.key)
	}
	return out
}

// Count 返回翻译表条目数（供报告统计）。
func Count() int { return len(msgTable) }
