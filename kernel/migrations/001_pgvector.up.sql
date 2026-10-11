-- 光明记忆 P1 · PostgreSQL 18 + pgvector 迁移脚本
-- 真源：lightmem.md §4.1 统一 Schema / §4.2 一致性骨架 / §5.4 存储后端
-- 严格只增不改，字段名与 kernel/model.go 对齐。

-- ============================================================
-- 1) 扩展
-- ============================================================
CREATE EXTENSION IF NOT EXISTS vector;

-- ============================================================
-- 2) 主表 memories（统一 Schema 单真源）
--    字段名严格对齐 kernel/model.go 的 Memory struct
-- ============================================================
CREATE TABLE IF NOT EXISTS memories (
    -- 标识
    memory_id          text PRIMARY KEY,                -- ULID
    idempotency_key    text NOT NULL,

    -- 归属
    tenant_id          text NOT NULL,
    user_id            text NOT NULL,
    agent_id           text,
    project_id         text,

    -- 可见性 / 权限（§3.2、§7.1）
    visibility         text NOT NULL DEFAULT 'private',
    shared_with_tenants text[] NOT NULL DEFAULT '{}',
    permission         jsonb NOT NULL DEFAULT '{}',      -- Permission{owner,writers,readers}
    permission_level   text,                             -- public/internal/机密（返回前密级过滤）

    -- 内容（§4.1）
    memory_kind        text NOT NULL,
    fact               text NOT NULL,
    reference_frame    jsonb NOT NULL DEFAULT '{}',      -- ReferenceFrame

    -- 双时态（§4.1）
    occurred_at        timestamptz,
    recorded_at        timestamptz NOT NULL DEFAULT now(),
    valid_from         date,                             -- date-only
    valid_until        date,                             -- date-only
    superseded_by      text,

    -- 质量/来源
    confidence         real NOT NULL DEFAULT 0,
    source             text NOT NULL DEFAULT '',
    version            int  NOT NULL DEFAULT 1,
    importance         real NOT NULL DEFAULT 0,
    recent_access_score real NOT NULL DEFAULT 0,
    access_count       bigint NOT NULL DEFAULT 0,
    expire_at          timestamptz,

    -- 溯源与投毒防护（§7.5）
    provenance         text NOT NULL DEFAULT 'normal',   -- normal / prompt_injection_suspect
    source_evidence    jsonb,

    -- 向量状态（§4.2：元数据为事实源，向量异步补全）
    vector_status      text NOT NULL DEFAULT 'pending',  -- pending / vectorized
    embedding          vector(1024),                     -- 仅 vector_status='vectorized' 时有值
    is_historical      boolean NOT NULL DEFAULT false,

    -- 扩展
    goal_detail        jsonb,                            -- goal 类特有语义
    external_ref       text
);

-- ============================================================
-- 3) 索引
-- ============================================================

-- 幂等键唯一（§4.1）：同 tenant+user+idempotency_key 只允许一个
CREATE UNIQUE INDEX IF NOT EXISTS memories_idem_idx
    ON memories (tenant_id, user_id, idempotency_key);

-- 元数据检索（§4.4 /search）：参考系 + visibility 硬过滤
CREATE INDEX IF NOT EXISTS memories_ref_idx
    ON memories (tenant_id, memory_kind, recorded_at DESC);

-- 可见性硬过滤辅助索引（§3.2）
CREATE INDEX IF NOT EXISTS memories_vis_idx
    ON memories (visibility, tenant_id);

-- 向量 HNSW 索引（§4.2 / profile 参数 m=16 ef_construction=128）
-- 部分索引：只在 vector_status='vectorized' 时建，避免 pending 行污染 HNSW 图
-- 注意：WITH 必须在 WHERE 之前（PostgreSQL 语法约束）
CREATE INDEX IF NOT EXISTS memories_embed_hnsw_idx
    ON memories USING hnsw (embedding vector_cosine_ops)
    WITH (m = 16, ef_construction = 128)
    WHERE vector_status = 'vectorized';

-- ============================================================
-- 4) 向量补全待办队列（§4.2 后台 worker 扫描用）
-- ============================================================
CREATE INDEX IF NOT EXISTS memories_pending_idx
    ON memories (recorded_at)
    WHERE vector_status = 'pending';

-- ============================================================
-- 5) 温度分层（§2.2，P2 阶段启用）
--    只增字段，L0/L1/L2/L3 由 LayeredStore 维护
--    骨架阶段先不建分层专用表，layer 记录在 reference_frame 或新列
-- ============================================================
ALTER TABLE memories ADD COLUMN IF NOT EXISTS layer text DEFAULT NULL;

-- ============================================================
-- 6) 反馈闭环 + 版本历史（§3.11 / §5.3，P2 阶段启用，只增字段）
--    feedback_records：FeedbackRecord[]；version_history：int[]
-- ============================================================
ALTER TABLE memories ADD COLUMN IF NOT EXISTS feedback_records jsonb;
ALTER TABLE memories ADD COLUMN IF NOT EXISTS version_history jsonb;