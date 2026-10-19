// PostgreSQL + pgvector 存储后端（Family/SMB/Enterprise 档，§5.4）
// 真源：lightmem.md §4.1 统一 Schema / §4.2 一致性骨架 / §5.4 存储后端可插拔。
//
// 关键设计（严格对齐文档）：
//   - 统一 Schema 单真源：单表 memories，JSONB 存 Permission/ReferenceFrame/GoalDetail
//   - 元数据为事实源，向量异步补全（§4.2）：Upsert 只写元数据 + vector_status='pending'
//   - 双时态（§4.1）：recorded_at 只增不改；occurred_at/valid_from/valid_until/superseded_by 独立列
//   - 可见性硬过滤（§3.2）：SQL WHERE 先过滤，再做 relevance 排序
//   - 向量检索失败降级（§4.2）：返回 err，由调度层 Query 兜底
package kernel

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // pgvector stdlib 驱动
)

// pgBackend 是 pgvector 后端实现。
type pgBackend struct {
	db    *sql.DB
	conf  *PGConfig
	dim   int // 向量维度（从 profile storage.embedding.dimensions 读）
	table string
}

// newPgBackend 从 StorageConfig 装配 pgvector 后端。
func newPgBackend(ctx context.Context, cfg *StorageConfig) (StorageBackend, error) {
	if cfg == nil || cfg.PG == nil {
		return nil, fmt.Errorf("pgvector backend requires storage.pg config")
	}
	dim := 1024
	if cfg.Embedding != nil && cfg.Embedding.Dimensions > 0 {
		dim = cfg.Embedding.Dimensions
	}

	host, port := cfg.PG.Host, cfg.PG.Port
	if port == 0 {
		port = 5432
	}
	user := cfg.PG.User
	if user == "" {
		user = "postgres"
	}
	sslmode := cfg.PG.SSLMode
	if sslmode == "" {
		sslmode = "disable"
	}

	dsn := fmt.Sprintf("host=%s port=%d user=%s dbname=%s sslmode=%s",
		host, port, user, cfg.PG.Database, sslmode)
	if cfg.PG.Password != "" {
		dsn += " password=" + cfg.PG.Password
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("pg open: %w", err)
	}
	// 连接池大小用 Go sql.DB 管理（pgx stdlib 不支持 pool_max_conns DSN 参数）
	db.SetMaxOpenConns(maxInt(cfg.PG.PoolSize, 10))
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("pg ping: %w", err)
	}

	return &pgBackend{db: db, conf: cfg.PG, dim: dim, table: "memories"}, nil
}

func (b *pgBackend) Name() string { return "pgvector" }

func (b *pgBackend) Ping(ctx context.Context) error { return b.db.PingContext(ctx) }

func (b *pgBackend) Close() error { return b.db.Close() }

// Upsert 幂等写入（§4.1）。
func (b *pgBackend) Upsert(ctx context.Context, m *Memory) error {
	if m.MemoryID == "" || m.TenantID == "" {
		return fmt.Errorf("memory_id and tenant_id required")
	}
	if m.RecordedAt.IsZero() {
		m.RecordedAt = time.Now().UTC()
	}

	permJSON, _ := json.Marshal(m.Permission)
	refJSON, _ := json.Marshal(m.ReferenceFrame)
	goalJSON, _ := json.Marshal(m.GoalDetail)
	evJSON, _ := json.Marshal(m.SourceEvidence)
	sharedJSON, _ := json.Marshal(m.SharedWithTenants)
	fbJSON, _ := json.Marshal(m.FeedbackRecords)
	vhJSON, _ := json.Marshal(m.VersionHistory)

	vis := string(m.Visibility)
	if vis == "" {
		vis = "private"
	}
	prov := string(m.Provenance)
	if prov == "" {
		prov = "normal"
	}
	vs := string(m.VectorStatus)
	if vs == "" {
		vs = "pending"
	}
	// §3.10 自动分类落层：Upsert 时若未显式指定 layer，按 memory_kind 决定初始层
	layer := m.Layer
	if layer == "" {
		layer = ClassifyLayer(m)
		m.Layer = layer // 回写，确保 INSERT/响应都带 layer
	}
	// 幂等键：§4.1 唯一约束 (tenant,user,idempotency_key)，未显式指定时回退到 memory_id
	// 避免多条 memory_id 不同但 idem_key 都为空 → 撞唯一索引。
	idem := m.IdempotencyKey
	if idem == "" {
		idem = m.MemoryID
	}

	_, err := b.db.ExecContext(ctx, `
	INSERT INTO memories (
		memory_id, idempotency_key, tenant_id, user_id, agent_id, project_id,
		visibility, shared_with_tenants, permission, permission_level,
		memory_kind, fact, reference_frame,
		occurred_at, recorded_at, valid_from, valid_until, superseded_by,
		confidence, source, version, importance, recent_access_score, access_count, expire_at,
		provenance, source_evidence, vector_status, is_historical,
		goal_detail, external_ref, layer, feedback_records, version_history
	) VALUES (
		$1,$2,$3,$4,$5,$6,
		$7,$8,$9,$10,
		$11,$12,$13,
		$14,$15,$16,$17,$18,
		$19,$20,$21,$22,$23,$24,$25,
		$26,$27,$28,$29,
		$30,$31,$32,$33,$34
	)
	ON CONFLICT (memory_id) DO UPDATE SET
		fact = EXCLUDED.fact,
		confidence = EXCLUDED.confidence,
		version = memories.version + 1,
		superseded_by = EXCLUDED.superseded_by,
		valid_until = EXCLUDED.valid_until,
		reference_frame = EXCLUDED.reference_frame,
		vector_status = EXCLUDED.vector_status,
		-- R21 收口 · G9：补全更新列。此前 ON CONFLICT 只更新上述 7 列，
		-- 导致 UpdateGoalProgress/SetGoalState/Upsert 既有记忆（如 feedback 回写）
		-- 时 **goal_detail / layer / provenance 等列根本不落库**：
		-- 0.88 实测 progress=0.5 返回 200 但 DB 里 goal_detail->>'progress' 为空。
		-- 这些列语义均为「就地更新同一 memory_id」，不违反只增不改（§4.1）。
		goal_detail = EXCLUDED.goal_detail,
		layer = EXCLUDED.layer,
		provenance = EXCLUDED.provenance,
		is_historical = EXCLUDED.is_historical,
		feedback_records = EXCLUDED.feedback_records,
		version_history = EXCLUDED.version_history,
		importance = EXCLUDED.importance,
		recent_access_score = EXCLUDED.recent_access_score,
		access_count = EXCLUDED.access_count,
		expire_at = EXCLUDED.expire_at,
		valid_from = EXCLUDED.valid_from,
		permission_level = EXCLUDED.permission_level,
		permission = EXCLUDED.permission,
		visibility = EXCLUDED.visibility,
		shared_with_tenants = EXCLUDED.shared_with_tenants`,
		m.MemoryID, idem, m.TenantID, m.UserID, optStr(m.AgentID), optStr(m.ProjectID),
		vis, stringArrFromJSON(sharedJSON), permJSON, optStr(m.PermissionLevel),
		string(m.MemoryKind), m.Fact, refJSON,
		optTime(m.OccurredAt), m.RecordedAt, optDate(m.ValidFrom), optDate(m.ValidUntil), derefStrPtr(m.SupersededBy),
		m.Confidence, m.Source, m.Version, m.Importance, m.RecentAccessScore, m.AccessCount, optTime(m.ExpireAt),
		prov, optJSON(evJSON), vs, m.IsHistorical,
		optJSON(goalJSON), derefStrPtr(m.ExternalRef), optLayer(m.Layer),
		optJSON(fbJSON), optJSON(vhJSON),
	)
	if err != nil {
		return fmt.Errorf("pg upsert: %w", err)
	}
	return nil
}

// Get 单条查询，含 visibility 硬过滤（§3.2）。
func (b *pgBackend) Get(ctx context.Context, memoryID, viewer, tenant string) (*Memory, error) {
	var (
		m                                                               Memory
		sharedJSON, permJSON, refJSON, evJSON, goalJSON, fbJSON, vhJSON []byte
		agentNS, projectNS, permLevel, superseded, extRef, layer        sql.NullString
		occurredNS, expireNS                                            sql.NullTime
		validFrom, validUntil                                           sql.NullString
		conf                                                            sql.NullFloat64
	)

	err := b.db.QueryRowContext(ctx, `
	SELECT memory_id, idempotency_key, tenant_id, user_id, agent_id, project_id,
		visibility, shared_with_tenants, permission, permission_level,
		memory_kind, fact, reference_frame,
		occurred_at, recorded_at, valid_from, valid_until, superseded_by,
		confidence, source, version, importance, recent_access_score, access_count, expire_at,
		provenance, source_evidence, vector_status, is_historical,
		goal_detail, external_ref, layer, feedback_records, version_history
	FROM memories WHERE memory_id=$1`, memoryID).Scan(
		&m.MemoryID, &m.IdempotencyKey, &m.TenantID, &m.UserID, &agentNS, &projectNS,
		&m.Visibility, &sharedJSON, &permJSON, &permLevel,
		&m.MemoryKind, &m.Fact, &refJSON,
		&occurredNS, &m.RecordedAt, &validFrom, &validUntil, &superseded,
		&conf, &m.Source, &m.Version, &m.Importance, &m.RecentAccessScore, &m.AccessCount, &expireNS,
		&m.Provenance, &evJSON, &m.VectorStatus, &m.IsHistorical,
		&goalJSON, &extRef, &layer, &fbJSON, &vhJSON,
	)
	if err != nil {
		return nil, fmt.Errorf("pg get: %w", err)
	}

	m.AgentID = agentNS.String
	m.ProjectID = projectNS.String
	m.Confidence = conf.Float64
	m.PermissionLevel = permLevel.String
	m.SupersededBy = optStrPtrNull(superseded)
	m.ExternalRef = optStrPtrNull(extRef)
	m.OccurredAt = optTimePtrNull(occurredNS)
	m.ValidFrom = validFrom.String
	m.ValidUntil = validUntil.String
	m.Layer = MemoryLayer(layer.String)
	m.ExpireAt = optTimePtrNull(expireNS)
	if len(goalJSON) > 0 {
		var gd GoalDetail
		if json.Unmarshal(goalJSON, &gd) == nil {
			m.GoalDetail = &gd
		}
	}
	_ = json.Unmarshal(pgArrayToJSON(string(sharedJSON)), &m.SharedWithTenants)
	_ = json.Unmarshal(permJSON, &m.Permission)
	_ = json.Unmarshal(refJSON, &m.ReferenceFrame)
	_ = json.Unmarshal(fbJSON, &m.FeedbackRecords)
	_ = json.Unmarshal(vhJSON, &m.VersionHistory)

	if !m.canView(viewer, tenant) {
		return nil, fmt.Errorf("memory %s not visible to %s/%s", memoryID, viewer, tenant)
	}
	return &m, nil
}

// pgVisibilityPredicate 生成 PG 后端的可见性硬过滤谓词（R21-B 修 G8，P0 越权读）。
//
// 逐条对齐 backend_sqlite.go 的 canView 规范语义（SQLite/Qdrant 本就正确，仅 PG 分叉；
// 0.88 生产档 family+pgvector 正踩此分叉）：
//
//	private      → viewer 非空 且 user_id = viewer（仅 owner 本人）
//	shared_user  → viewer 非空 且 (user_id = viewer 或 readers 含 viewer)
//	tenant       → tenant 非空 且 tenant_id = tenant
//	cross_tenant → tenant 非空 且 shared_with_tenants 含 tenant
//	public       → viewer 非空（R23.2 新增，L-225 热修补入，见下方 L-225 段）
//
// 修复前的缺陷（G8，E 线 0.88 实测复现）：
//
//	(visibility = $1 OR (visibility='shared_user' AND permission->'readers' ? $2)
//	 OR tenant_id = $3 OR $3 = ANY(shared_with_tenants))
//
// 两个独立错误：① $1 传的是 q.Viewer（用户名）却与 visibility 枚举比较 → 恒假，
// owner 校验形同虚设；② tenant_id = $3 无差别放行 → private 行对同租户所有人
// （含不传 viewer）可见。既有单测跑在 SQLite 上，永远测不到此分叉（绿灯假象）。
//
// 参数名由调用方给（$N 占位），便于 Query 与 VectorSearch 复用同一谓词。
//
// L-225 热修（R24-T0，P1）：原谓词缺 public 分支，导致 R23.2 已定稿的「社区公开」
// 在 PG 生产档完全不可用——public 记忆在 PG 上谁都检索不到（R23.2 验收跑的是
// solo/sqlite，故从未暴露）。本次**只追加一个 OR 分支**，与 backend_sqlite.go
// canView 的 `case VisPublic: return viewer != ""` 逐字对齐；既有四个分支、
// 函数签名、调用点参数序一律未动。
func pgVisibilityPredicate(viewerParam, tenantParam string) string {
	return fmt.Sprintf(`(
       (visibility = 'private'      AND user_id = %[1]s AND %[1]s <> '')
    OR (visibility = 'shared_user' AND %[1]s <> '' AND (user_id = %[1]s OR permission->'readers' ? %[1]s))
    OR (visibility = 'tenant'      AND tenant_id = %[2]s AND %[2]s <> '')
    OR (visibility = 'cross_tenant' AND %[2]s <> '' AND %[2]s = ANY(shared_with_tenants))
    OR (visibility = 'public'      AND %[1]s <> '')
)`, viewerParam, tenantParam)
}

func (b *pgBackend) Query(ctx context.Context, q QueryParams) ([]QueryResult, error) {
	topK := q.TopK
	if topK <= 0 {
		topK = 10
	}
	// R21-B 修 G8：谓词改用 $1=viewer / $2=tenant 两个独立参数（对齐 canView）。
	where := []string{
		pgVisibilityPredicate("$1", "$2"),
	}
	args := []any{q.Viewer, q.TenantID}
	argIdx := 2

	// R24-B · L-226：默认只返回当前生效版本（superseded_by IS NULL），
	// 与 SQLite / Qdrant 两侧及 recall 路径既有语义一致。IncludeSuperseded=true
	// 时不加该条件。注意：这是 Go 侧 SQL 拼接，未改 migrations/*.sql（红线）。
	if !q.IncludeSuperseded {
		where = append(where, "superseded_by IS NULL")
	}
	if q.Text != "" {
		argIdx++
		args = append(args, "%"+strings.ToLower(q.Text)+"%")
		where = append(where, fmt.Sprintf("LOWER(fact) LIKE $%d", argIdx))
	}
	if len(q.MemoryKinds) > 0 {
		kinds := make([]string, len(q.MemoryKinds))
		for i, k := range q.MemoryKinds {
			argIdx++
			args = append(args, string(k))
			kinds[i] = fmt.Sprintf("$%d", argIdx)
		}
		where = append(where, fmt.Sprintf("memory_kind IN (%s)", strings.Join(kinds, ",")))
	}
	if q.AsOf != nil {
		argIdx++
		args = append(args, *q.AsOf)
		where = append(where,
			fmt.Sprintf("(valid_from IS NULL OR valid_from <= $%d::date)", argIdx),
			fmt.Sprintf("(valid_until IS NULL OR valid_until >= $%d::date)", argIdx),
		)
	}

	sqlStr := fmt.Sprintf(`
	SELECT memory_id, fact, confidence, source, occurred_at, recorded_at,
	       valid_from, valid_until, superseded_by, is_historical,
	       COALESCE(confidence, 0) AS score
	FROM %s
	WHERE %s
	ORDER BY confidence DESC, recorded_at DESC
	LIMIT %d`, b.table, strings.Join(where, " AND "), topK)

	rows, err := b.db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("pg query: %w", err)
	}
	defer rows.Close()

	var results []QueryResult
	for rows.Next() {
		var r QueryResult
		var occurred sql.NullTime
		var validFrom, validUntil sql.NullString
		var superseded sql.NullString
		var conf sql.NullFloat64
		if err := rows.Scan(
			&r.MemoryID, &r.Fact, &conf, &r.Source, &occurred, &r.RecordedAt,
			&validFrom, &validUntil, &superseded, &r.IsHistorical, &r.Score,
		); err != nil {
			return nil, fmt.Errorf("pg query scan: %w", err)
		}
		r.Score = float64(conf.Float64)
		r.OccurredAt = optTimePtrNull(occurred)
		r.ValidFrom = validFrom.String
		r.ValidUntil = validUntil.String
		r.SupersededBy = optStrPtrNull(superseded)
		results = append(results, r)
	}
	return results, rows.Err()
}

// VectorSearch 向量检索（§4.4 /search/vector）。
func (b *pgBackend) VectorSearch(ctx context.Context, q QueryParams) ([]QueryResult, error) {
	if len(q.Vector) == 0 {
		return nil, fmt.Errorf("vector search requires embedding vector")
	}
	topK := q.TopK
	if topK <= 0 {
		topK = 10
	}
	vecStr := vectorToString(q.Vector)

	// R21-B 修 G8：与 Query 同一谓词（$2=viewer / $3=tenant，$1 为向量，LIMIT $4）。
	sqlStr := fmt.Sprintf(`
	SELECT memory_id, fact, confidence, source, occurred_at, recorded_at,
	       valid_from, valid_until, superseded_by, is_historical,
	       1 - (embedding <=> $1::vector) AS score
	FROM %s
	WHERE vector_status = 'vectorized'
	AND %s
	ORDER BY embedding <=> $1::vector
	LIMIT $4`, b.table, pgVisibilityPredicate("$2", "$3"))

	rows, err := b.db.QueryContext(ctx, sqlStr, vecStr, q.Viewer, q.TenantID, topK)
	if err != nil {
		return nil, fmt.Errorf("pg vector search: %w", err)
	}
	defer rows.Close()

	var results []QueryResult
	for rows.Next() {
		var r QueryResult
		var occurred sql.NullTime
		var validFrom, validUntil, superseded sql.NullString
		var conf sql.NullFloat64
		if err := rows.Scan(
			&r.MemoryID, &r.Fact, &conf, &r.Source, &occurred, &r.RecordedAt,
			&validFrom, &validUntil, &superseded, &r.IsHistorical, &r.Score,
		); err != nil {
			return nil, fmt.Errorf("pg vector scan: %w", err)
		}
		r.OccurredAt = optTimePtrNull(occurred)
		r.ValidFrom = validFrom.String
		r.ValidUntil = validUntil.String
		r.SupersededBy = optStrPtrNull(superseded)
		results = append(results, r)
	}
	return results, rows.Err()
}

// CountByKind 按 memory_kind 计数（§3.6 指标）。
func (b *pgBackend) CountByKind(ctx context.Context, kind MemoryKind, tenant string) (int64, error) {
	var n int64
	err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memories WHERE tenant_id=$1 AND memory_kind=$2`,
		tenant, string(kind)).Scan(&n)
	return n, err
}

// BackfillVectors 后台 worker：补全 vector_status='pending' 的 embedding（§4.2）。
//
// R24-B · B2：签名由 `error` 改为 `(BackfillReport, error)`，以适配冻结接口
// `VectorBackfiller`（任务书 §5.1 B2 / §9）。**行为逐位不变**：同样的 SQL、
// 同样的 LIMIT 100、同样的失败继续（`continue`）语义；唯一差别是新增计数返回值，
// 失败计数只影响报告不影响流程（原实现对 embed 失败也是 continue 跳过）。
//
// 连带修改：`kernel/router.go` 的 handleBackfill 调用点（签名变更的必要适配，
// 非新增功能，详见 `_R24_B线交付报告.md` §偏离登记）。
func (b *pgBackend) BackfillVectors(ctx context.Context, embed func(string) ([]float32, error)) (BackfillReport, error) {
	var rep BackfillReport
	rows, err := b.db.QueryContext(ctx, `
	SELECT memory_id, fact FROM memories
	WHERE vector_status='pending' ORDER BY recorded_at LIMIT 100`)
	if err != nil {
		return rep, err
	}
	type pending struct {
		id   string
		fact string
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.fact); err != nil {
			rows.Close()
			return rep, err
		}
		batch = append(batch, p)
	}
	rows.Close()

	rep.Scanned = int64(len(batch))
	for _, p := range batch {
		vec, err := embed(p.fact)
		if err != nil {
			rep.Failed++
			continue
		}
		vecStr := vectorToString(vec)
		if _, err := b.db.ExecContext(ctx,
			`UPDATE memories SET embedding=$1::vector, vector_status='vectorized' WHERE memory_id=$2 AND vector_status='pending'`,
			vecStr, p.id); err != nil {
			rep.Failed++
			return rep, err
		}
		rep.Vectorized++
	}
	return rep, nil
}

// AuditVectorIndex 实现 VectorIndexAuditor（R24-B · B3）：PG 是「元数据与向量
// 同库同表」，故不存在真正的孤儿索引；缺失索引 = vector_status<>'vectorized'。
// 零向量统计走 SQL 侧：`embedding` 非空但其 L2 范数为 0（pgvector 的 `<->` 自距离）。
func (b *pgBackend) AuditVectorIndex(ctx context.Context, tenant string) (*IndexAudit, error) {
	audit := &IndexAudit{}
	where := `WHERE ($1 = '' OR tenant_id = $1)`
	args := []any{tenant}

	var n int64
	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memories `+where+` AND vector_status='vectorized'`, args...).Scan(&n); err != nil {
		return nil, err
	}
	audit.Indexed = n

	// 零向量：有 embedding 且与自身的欧氏距离为 0（全零向量）。
	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memories `+where+` AND embedding IS NOT NULL AND embedding <-> embedding = 0`, args...).Scan(&n); err != nil {
		return nil, err
	}
	audit.ZeroVector = n

	rows, err := b.db.QueryContext(ctx,
		`SELECT memory_id FROM memories `+where+` AND vector_status<>'vectorized' ORDER BY memory_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		audit.MissingIDs = append(audit.MissingIDs, id)
	}
	return audit, rows.Err()
}

// CountByLayer 返回指定层（layer 为空 = 全部层）的记忆条数（§3.10 温度指标）。
func (b *pgBackend) CountByLayer(ctx context.Context, layer MemoryLayer, tenant string) (int64, error) {
	var n int64
	if layer == "" {
		err := b.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM memories WHERE tenant_id=$1`, tenant).Scan(&n)
		return n, err
	}
	err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memories WHERE tenant_id=$1 AND layer=$2`,
		tenant, string(layer)).Scan(&n)
	return n, err
}

// ---- LayerScanner (P2 温度分层, §3.10) ----

// ScanLayer 按层返回记忆（layer 为空 = 全部层），限制 max 条，按 recorded_at 升序（最旧的先处理）。
func (b *pgBackend) ScanLayer(ctx context.Context, layer MemoryLayer, tenant string, max int) ([]*Memory, error) {
	if max <= 0 {
		max = 1000
	}
	sqlStr := `
	SELECT memory_id, idempotency_key, tenant_id, user_id, agent_id, project_id,
		visibility, shared_with_tenants, permission, permission_level,
		memory_kind, fact, reference_frame,
		occurred_at, recorded_at, valid_from, valid_until, superseded_by,
		confidence, source, version, importance, recent_access_score, access_count, expire_at,
		provenance, source_evidence, vector_status, is_historical,
		goal_detail, external_ref, layer
	FROM ` + b.table + `
	WHERE tenant_id=$1`
	args := []any{tenant}
	if layer != "" {
		sqlStr += " AND layer=$2"
		args = append(args, string(layer))
	}
	sqlStr += " ORDER BY recorded_at ASC LIMIT " + fmt.Sprintf("%d", max)

	rows, err := b.db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("pg scan layer: %w", err)
	}
	defer rows.Close()

	var out []*Memory
	for rows.Next() {
		var (
			m                                                          Memory
			sharedJSON, permJSON, refJSON, evJSON, goalJSON            []byte
			agentNS, projectNS, permLevel, superseded, extRef, layerNS sql.NullString
			conf                                                       sql.NullFloat64
			occT, expT                                                 sql.NullTime
			validFrom, validUntil                                      sql.NullString
		)
		if err := rows.Scan(
			&m.MemoryID, &m.IdempotencyKey, &m.TenantID, &m.UserID, &agentNS, &projectNS,
			&m.Visibility, &sharedJSON, &permJSON, &permLevel,
			&m.MemoryKind, &m.Fact, &refJSON,
			&occT, &m.RecordedAt, &validFrom, &validUntil, &superseded,
			&conf, &m.Source, &m.Version, &m.Importance, &m.RecentAccessScore, &m.AccessCount, &expT,
			&m.Provenance, &evJSON, &m.VectorStatus, &m.IsHistorical,
			&goalJSON, &extRef, &layerNS,
		); err != nil {
			return nil, fmt.Errorf("pg scan layer scan: %w", err)
		}
		m.AgentID = agentNS.String
		m.ProjectID = projectNS.String
		m.Confidence = conf.Float64
		m.PermissionLevel = permLevel.String
		m.SupersededBy = optStrPtrNull(superseded)
		m.ExternalRef = optStrPtrNull(extRef)
		m.OccurredAt = optTimePtrNull(occT)
		m.ValidFrom = validFrom.String
		m.ValidUntil = validUntil.String
		m.Layer = MemoryLayer(layerNS.String)
		m.ExpireAt = optTimePtrNull(expT)
		if len(goalJSON) > 0 {
			var gd GoalDetail
			if json.Unmarshal(goalJSON, &gd) == nil {
				m.GoalDetail = &gd
			}
		}
		_ = json.Unmarshal(pgArrayToJSON(string(sharedJSON)), &m.SharedWithTenants)
		_ = json.Unmarshal(permJSON, &m.Permission)
		_ = json.Unmarshal(refJSON, &m.ReferenceFrame)
		out = append(out, &m)
	}
	return out, rows.Err()
}

// UpdateLayer 更新单条记忆的 layer 字段（只增不改：不动 recorded_at）。
func (b *pgBackend) UpdateLayer(ctx context.Context, memoryID string, layer MemoryLayer) error {
	res, err := b.db.ExecContext(ctx,
		`UPDATE memories SET layer=$1 WHERE memory_id=$2`, string(layer), memoryID)
	if err != nil {
		return fmt.Errorf("pg update layer: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("pg update layer: memory %s not found", memoryID)
	}
	return nil
}

// ---- MemoryScanner / IdempotencyLookup（P2 A3/A6）----

// ScanAll 跨租户全量扫描（整合 worker 用，§3.13）。按 recorded_at 升序，限制 max 条。
func (b *pgBackend) ScanAll(ctx context.Context, max int) ([]*Memory, error) {
	if max <= 0 {
		max = 5000
	}
	rows, err := b.db.QueryContext(ctx, `
	SELECT memory_id, idempotency_key, tenant_id, user_id, agent_id, project_id,
		visibility, shared_with_tenants, permission, permission_level,
		memory_kind, fact, reference_frame,
		occurred_at, recorded_at, valid_from, valid_until, superseded_by,
		confidence, source, version, importance, recent_access_score, access_count, expire_at,
		provenance, source_evidence, vector_status, is_historical,
		goal_detail, external_ref, layer
	FROM `+b.table+` ORDER BY recorded_at ASC LIMIT `+fmt.Sprintf("%d", max))
	if err != nil {
		return nil, fmt.Errorf("pg scan all: %w", err)
	}
	defer rows.Close()
	return b.scanMemoryRows(rows)
}

// FindByIdempotencyKey 按 (tenant,user,idempotency_key) 查已存在记忆（§4.1）。
// 未命中返回 (nil, nil)；命中返回已存在记录（含 layer 等完整字段）。
func (b *pgBackend) FindByIdempotencyKey(ctx context.Context, tenant, user, key string) (*Memory, error) {
	if key == "" {
		return nil, nil
	}
	rows, err := b.db.QueryContext(ctx, `
	SELECT memory_id, idempotency_key, tenant_id, user_id, agent_id, project_id,
		visibility, shared_with_tenants, permission, permission_level,
		memory_kind, fact, reference_frame,
		occurred_at, recorded_at, valid_from, valid_until, superseded_by,
		confidence, source, version, importance, recent_access_score, access_count, expire_at,
		provenance, source_evidence, vector_status, is_historical,
		goal_detail, external_ref, layer
	FROM `+b.table+`
	WHERE tenant_id=$1 AND user_id=$2 AND idempotency_key=$3
	ORDER BY recorded_at DESC LIMIT 1`, tenant, user, key)
	if err != nil {
		return nil, fmt.Errorf("pg find idempotency: %w", err)
	}
	defer rows.Close()
	ms, err := b.scanMemoryRows(rows)
	if err != nil {
		return nil, err
	}
	if len(ms) == 0 {
		return nil, nil
	}
	return ms[0], nil
}

// scanMemoryRows 把与 ScanLayer 相同列顺序的结果集扫成 []*Memory。
func (b *pgBackend) scanMemoryRows(rows *sql.Rows) ([]*Memory, error) {
	var out []*Memory
	for rows.Next() {
		var (
			m                                                          Memory
			sharedJSON, permJSON, refJSON, evJSON, goalJSON            []byte
			agentNS, projectNS, permLevel, superseded, extRef, layerNS sql.NullString
			conf                                                       sql.NullFloat64
			occT, expT                                                 sql.NullTime
			validFrom, validUntil                                      sql.NullString
		)
		if err := rows.Scan(
			&m.MemoryID, &m.IdempotencyKey, &m.TenantID, &m.UserID, &agentNS, &projectNS,
			&m.Visibility, &sharedJSON, &permJSON, &permLevel,
			&m.MemoryKind, &m.Fact, &refJSON,
			&occT, &m.RecordedAt, &validFrom, &validUntil, &superseded,
			&conf, &m.Source, &m.Version, &m.Importance, &m.RecentAccessScore, &m.AccessCount, &expT,
			&m.Provenance, &evJSON, &m.VectorStatus, &m.IsHistorical,
			&goalJSON, &extRef, &layerNS,
		); err != nil {
			return nil, fmt.Errorf("pg scan memory scan: %w", err)
		}
		m.AgentID = agentNS.String
		m.ProjectID = projectNS.String
		m.Confidence = conf.Float64
		m.PermissionLevel = permLevel.String
		m.SupersededBy = optStrPtrNull(superseded)
		m.ExternalRef = optStrPtrNull(extRef)
		m.OccurredAt = optTimePtrNull(occT)
		m.ValidFrom = validFrom.String
		m.ValidUntil = validUntil.String
		m.Layer = MemoryLayer(layerNS.String)
		m.ExpireAt = optTimePtrNull(expT)
		if len(goalJSON) > 0 {
			var gd GoalDetail
			if json.Unmarshal(goalJSON, &gd) == nil {
				m.GoalDetail = &gd
			}
		}
		_ = json.Unmarshal(pgArrayToJSON(string(sharedJSON)), &m.SharedWithTenants)
		_ = json.Unmarshal(permJSON, &m.Permission)
		_ = json.Unmarshal(refJSON, &m.ReferenceFrame)
		out = append(out, &m)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------------
// ComplianceBackend（P7-A GDPR，§7.3/§7.4/§7.5）
// 只增不改：在 pgvector 后端上补齐删除/导出。导出/返回 ID 均按确定性排序。
// ------------------------------------------------------------------

// ExportByScope 返回 (tenant, userID) 作用域下的全部记忆。
func (b *pgBackend) ExportByScope(ctx context.Context, tenant, userID string) ([]*Memory, error) {
	rows, err := b.db.QueryContext(ctx, `
	SELECT memory_id, idempotency_key, tenant_id, user_id, agent_id, project_id,
		visibility, shared_with_tenants, permission, permission_level,
		memory_kind, fact, reference_frame,
		occurred_at, recorded_at, valid_from, valid_until, superseded_by,
		confidence, source, version, importance, recent_access_score, access_count, expire_at,
		provenance, source_evidence, vector_status, is_historical,
		goal_detail, external_ref, layer
	FROM `+b.table+`
	WHERE tenant_id=$1 AND user_id=$2
	ORDER BY recorded_at ASC, memory_id ASC`, tenant, userID)
	if err != nil {
		return nil, fmt.Errorf("pg export by scope: %w", err)
	}
	defer rows.Close()
	return b.scanMemoryRows(rows)
}

// DeleteByScope 级联删除 (tenant, userID) 作用域下全部记忆，返回被删 ID（升序）。
func (b *pgBackend) DeleteByScope(ctx context.Context, tenant, userID string) ([]string, error) {
	// 注：PostgreSQL 的 DELETE ... RETURNING 不支持 ORDER BY 子句，
	// 故在应用层对返回的 memory_id 做升序排序，保持接口语义不变。
	rows, err := b.db.QueryContext(ctx, `
	DELETE FROM `+b.table+`
	WHERE tenant_id=$1 AND user_id=$2
	RETURNING memory_id`, tenant, userID)
	if err != nil {
		return nil, fmt.Errorf("pg cascade delete: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("pg cascade delete scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(ids)
	return ids, nil
}

// DeleteByID 物理删除单条记忆（被遗忘权）。不存在时视为已遗忘（返回 nil，幂等）。
func (b *pgBackend) DeleteByID(ctx context.Context, memoryID string) error {
	if memoryID == "" {
		return fmt.Errorf("memory_id required")
	}
	res, err := b.db.ExecContext(ctx,
		`DELETE FROM `+b.table+` WHERE memory_id=$1`, memoryID)
	if err != nil {
		return fmt.Errorf("pg delete by id: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 已不存在 → 视为已遗忘（幂等）
		return nil
	}
	return nil
}

// ---- helper ----

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func optStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func optStrPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func derefStrPtr(s *string) any {
	if s == nil || *s == "" {
		return nil
	}
	return *s
}

func optTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return *t
}

func optTimePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func optTimePtrNull(t sql.NullTime) *time.Time {
	if t.Valid && !t.Time.IsZero() {
		v := t.Time
		return &v
	}
	return nil
}

func optStrPtrNull(s sql.NullString) *string {
	if s.Valid && s.String != "" {
		v := s.String
		return &v
	}
	return nil
}

func optDate(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func optJSON(b []byte) any {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	return b
}

func optLayer(l MemoryLayer) any {
	if l == "" {
		return nil
	}
	return string(l)
}

// stringArrFromJSON 把 JSON 数组（如 `["a","b"]`）转成 PG `text[]` 字面量（如 `{"a","b"}`）。
//
// R21 收口修 §6.2（B 线报告遗留，PG Upsert 编码缺陷）：
// 旧实现直接把 JSON 串当 `text[]` 参数传（`return string(b)`），而 `shared_with_tenants`
// 列类型是 `text[]`（见 migrations/001_pgvector.up.sql），PG 会报
// `malformed array literal`（SQLSTATE 22P02）——**任何带 SharedWithTenants 的记忆
// 在 PG 上写不进去**。此处改为输出 PG 数组字面量并对元素做转义。
func stringArrFromJSON(b []byte) any {
	if len(b) == 0 || string(b) == "null" {
		return "{}"
	}
	var arr []string
	if err := json.Unmarshal(b, &arr); err != nil || len(arr) == 0 {
		return "{}"
	}
	var sb strings.Builder
	sb.WriteByte('{')
	for i, s := range arr {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteByte('"')
		// PG 数组字面量内需转义 `\` 与 `"`
		sb.WriteString(strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s))
		sb.WriteByte('"')
	}
	sb.WriteByte('}')
	return sb.String()
}

// pgArrayToJSON 把 PG `text[]` 字面量（如 `{"a","b"}`）还原成 JSON 数组（如 `["a","b"]`），
// 供上层 `json.Unmarshal(&m.SharedWithTenants)` 使用。
// 与 stringArrFromJSON 对称，修 §6.2 的读回路径。
func pgArrayToJSON(literal string) []byte {
	literal = strings.TrimSpace(literal)
	if literal == "" || literal == "{}" || literal == "null" {
		return []byte("[]")
	}
	// 去掉两端花括号
	body := strings.TrimPrefix(literal, "{")
	body = strings.TrimSuffix(body, "}")
	if strings.TrimSpace(body) == "" {
		return []byte("[]")
	}
	var out []string
	var cur strings.Builder
	inQuote := false
	escaped := false
	for _, r := range body {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\' && inQuote:
			escaped = true
		case r == '"':
			if inQuote {
				out = append(out, cur.String())
				cur.Reset()
				inQuote = false
			} else {
				inQuote = true
			}
		case r == ',' && !inQuote:
			// 分隔符，忽略
		case inQuote:
			cur.WriteRune(r)
		}
	}
	j, err := json.Marshal(out)
	if err != nil {
		return []byte("[]")
	}
	return j
}

func vectorToString(v []float32) string {
	if len(v) == 0 {
		return "[]"
	}
	var sb strings.Builder
	sb.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "%g", x)
	}
	sb.WriteByte(']')
	return sb.String()
}
