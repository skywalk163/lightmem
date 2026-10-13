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
		vector_status = EXCLUDED.vector_status`,
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
	_ = json.Unmarshal(sharedJSON, &m.SharedWithTenants)
	_ = json.Unmarshal(permJSON, &m.Permission)
	_ = json.Unmarshal(refJSON, &m.ReferenceFrame)
	_ = json.Unmarshal(fbJSON, &m.FeedbackRecords)
	_ = json.Unmarshal(vhJSON, &m.VersionHistory)

	if !m.canView(viewer, tenant) {
		return nil, fmt.Errorf("memory %s not visible to %s/%s", memoryID, viewer, tenant)
	}
	return &m, nil
}
func (b *pgBackend) Query(ctx context.Context, q QueryParams) ([]QueryResult, error) {
	topK := q.TopK
	if topK <= 0 {
		topK = 10
	}
	where := []string{
		"(visibility = $1 OR (visibility = 'shared_user' AND permission->'readers' ? $2) OR tenant_id = $3 OR $3 = ANY(shared_with_tenants))",
	}
	args := []any{q.Viewer, q.Viewer, q.TenantID}
	argIdx := 3

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

	sqlStr := fmt.Sprintf(`
	SELECT memory_id, fact, confidence, source, occurred_at, recorded_at,
	       valid_from, valid_until, superseded_by, is_historical,
	       1 - (embedding <=> $1::vector) AS score
	FROM %s
	WHERE vector_status = 'vectorized'
	AND (visibility = $2 OR (visibility = 'shared_user' AND permission->'readers' ? $3) OR tenant_id = $4 OR $4 = ANY(shared_with_tenants))
	ORDER BY embedding <=> $1::vector
	LIMIT $5`, b.table)

	rows, err := b.db.QueryContext(ctx, sqlStr, vecStr, q.Viewer, q.Viewer, q.TenantID, topK)
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
func (b *pgBackend) BackfillVectors(ctx context.Context, embed func(string) ([]float32, error)) error {
	rows, err := b.db.QueryContext(ctx, `
	SELECT memory_id, fact FROM memories
	WHERE vector_status='pending' ORDER BY recorded_at LIMIT 100`)
	if err != nil {
		return err
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
			return err
		}
		batch = append(batch, p)
	}
	rows.Close()

	for _, p := range batch {
		vec, err := embed(p.fact)
		if err != nil {
			continue
		}
		vecStr := vectorToString(vec)
		if _, err := b.db.ExecContext(ctx,
			`UPDATE memories SET embedding=$1::vector, vector_status='vectorized' WHERE memory_id=$2 AND vector_status='pending'`,
			vecStr, p.id); err != nil {
			return err
		}
	}
	return nil
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
		_ = json.Unmarshal(sharedJSON, &m.SharedWithTenants)
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
		_ = json.Unmarshal(sharedJSON, &m.SharedWithTenants)
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
	rows, err := b.db.QueryContext(ctx, `
	DELETE FROM `+b.table+`
	WHERE tenant_id=$1 AND user_id=$2
	RETURNING memory_id ORDER BY memory_id ASC`, tenant, userID)
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
	return ids, rows.Err()
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

func stringArrFromJSON(b []byte) any {
	if len(b) == 0 || string(b) == "null" {
		return "{}"
	}
	return string(b)
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
