#!/usr/bin/env bash
# R10-B: 隔离 PostgreSQL 18 + pgvector 测试库建表/清理脚本
#
# 红线遵守：
#   - 凭据全部走环境变量（LIGHTMEM_PG_DSN 或 LIGHTMEM_PG_*），不硬编码；
#   - 测试库名带 _r10_test 后缀，测试结束可一键 drop；
#   - DDL 列结构严格对齐 kernel/backend_pg.go 的 INSERT/SELECT 列；
#   - 可在干净 0.88 上重复执行。
#
# 用法：
#   export LIGHTMEM_PG_HOST=localhost LIGHTMEM_PG_PORT=5432 \
#          LIGHTMEM_PG_USER=postgres LIGHTMEM_PG_PASSWORD='' \
#          LIGHTMEM_PG_DB=postgres
#   bash r10_pg_setup.sh create   # 建隔离测试库 + 建表 + 建索引
#   bash r10_pg_setup.sh drop    # 删除测试库
#   bash r10_pg_setup.sh name    # 打印当前测试库名（stdout）
#
# 环境变量优先级：
#   LIGHTMEM_PG_DSN（完整 DSN，如 "host=... port=... user=... dbname=... sslmode=..."）
#   > LIGHTMEM_PG_HOST / LIGHTMEM_PG_PORT / LIGHTMEM_PG_USER / LIGHTMEM_PG_PASSWORD / LIGHTMEM_PG_DB

set -euo pipefail

# ---- 解析连接配置 ----
if [[ -n "${LIGHTMEM_PG_DSN:-}" ]]; then
    # 完整 DSN 模式：直接 psql 用
    PG_CONN="${LIGHTMEM_PG_DSN}"
else
    PG_HOST="${LIGHTMEM_PG_HOST:-localhost}"
    PG_PORT="${LIGHTMEM_PG_PORT:-5432}"
    PG_USER="${LIGHTMEM_PG_USER:-postgres}"
    PG_PASSWORD="${LIGHTMEM_PG_PASSWORD:-}"
    PG_ADMIN_DB="${LIGHTMEM_PG_DB:-postgres}"
    PG_CONN="host=${PG_HOST} port=${PG_PORT} user=${PG_USER} dbname=${PG_ADMIN_DB} sslmode=disable"
    if [[ -n "${PG_PASSWORD}" ]]; then
        PG_CONN="${PG_CONN} password=${PG_PASSWORD}"
    fi
fi

# 测试库名：带 pid 隔离，避免多轮测试互相污染
TEST_DB="lightmem_r10_test_$$"

# ---- 工具函数 ----
psql_exec() {
    # $1 = SQL 字符串；$2 = 目标库名（可选，默认用 PG_CONN 里的库）
    local sql="$1"
    local db="${2:-}"
    local conn="${PG_CONN}"
    if [[ -n "${db}" ]]; then
        # 替换 dbname
        conn=$(echo "${PG_CONN}" | sed -E "s|dbname=[^ ]+|dbname=${db}|")
    fi
    PGPASSWORD="${PG_PASSWORD:-}" psql "${conn}" -v ON_ERROR_STOP=1 -c "${sql}"
}

usage() {
    echo "Usage: $0 {create|drop|name}" >&2
    exit 1
}

# ---- 子命令 ----
cmd_name() {
    echo "${TEST_DB}"
}

cmd_create() {
    echo "[r10-pg-setup] creating isolated test database: ${TEST_DB}"

    # 1) 建库（在 admin 库上执行）
    # 注意：CREATE DATABASE 不能在事务里跑，psql -c 单条语句天然非事务
    # 先确保不存在（幂等）
    psql_exec "DROP DATABASE IF EXISTS \"${TEST_DB}\";" postgres >/dev/null 2>&1 || true
    psql_exec "CREATE DATABASE \"${TEST_DB}\";" postgres

    # 2) 建 pgvector 扩展
    echo "[r10-pg-setup] CREATE EXTENSION vector"
    psql_exec "CREATE EXTENSION IF NOT EXISTS vector;" "${TEST_DB}"

    # 3) 建 memories 表（列结构严格对齐 backend_pg.go）
    echo "[r10-pg-setup] CREATE TABLE memories"
    psql_exec "
CREATE TABLE memories (
    memory_id            TEXT PRIMARY KEY,
    idempotency_key      TEXT NOT NULL,
    tenant_id            TEXT NOT NULL,
    user_id              TEXT NOT NULL,
    agent_id             TEXT DEFAULT '',
    project_id           TEXT DEFAULT '',
    visibility           TEXT NOT NULL DEFAULT 'private',
    shared_with_tenants  TEXT[] DEFAULT '{}',
    permission           JSONB DEFAULT '{}'::jsonb,
    permission_level     TEXT DEFAULT '',
    memory_kind          TEXT NOT NULL,
    fact                 TEXT NOT NULL,
    reference_frame      JSONB DEFAULT '{}'::jsonb,
    occurred_at          TIMESTAMPTZ,
    recorded_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    valid_from           DATE,
    valid_until          DATE,
    superseded_by        TEXT,
    confidence           DOUBLE PRECISION DEFAULT 0,
    source               TEXT DEFAULT '',
    version              INTEGER DEFAULT 1,
    importance           DOUBLE PRECISION DEFAULT 0,
    recent_access_score  DOUBLE PRECISION DEFAULT 0,
    access_count         BIGINT DEFAULT 0,
    expire_at            TIMESTAMPTZ,
    provenance           TEXT DEFAULT 'normal',
    source_evidence      JSONB,
    vector_status        TEXT DEFAULT 'pending',
    is_historical        BOOLEAN DEFAULT FALSE,
    goal_detail          JSONB,
    external_ref         TEXT,
    layer                TEXT DEFAULT '',
    feedback_records     JSONB,
    version_history      JSONB,
    embedding            vector(1024)
);
" "${TEST_DB}"

    # 4) 索引
    echo "[r10-pg-setup] CREATE INDEXES"
    # 幂等键唯一索引（§4.1：(tenant, user, idempotency_key) 唯一）
    psql_exec "CREATE UNIQUE INDEX IF NOT EXISTS idx_memories_idem ON memories (tenant_id, user_id, idempotency_key);" "${TEST_DB}"
    # tenant 查询索引
    psql_exec "CREATE INDEX IF NOT EXISTS idx_memories_tenant ON memories (tenant_id);" "${TEST_DB}"
    # memory_id 已有 PK，这里补一个普通索引（任务书要求 vector_idx on memory_id，实际 PK 已覆盖；保留注释说明）
    # 向量 HNSW 索引（cosine ops，适合 VectorSearch 的 <=> 操作符）
    psql_exec "CREATE INDEX IF NOT EXISTS idx_memories_embedding_hnsw ON memories USING hnsw (embedding vector_cosine_ops) WHERE vector_status = 'vectorized';" "${TEST_DB}"

    echo "[r10-pg-setup] DONE. test database = ${TEST_DB}"
    echo "[r10-pg-setup] Tear down with: $0 drop"
}

cmd_drop() {
    echo "[r10-pg-setup] dropping test database: ${TEST_DB}"
    psql_exec "DROP DATABASE IF EXISTS \"${TEST_DB}\";" postgres >/dev/null
    echo "[r10-pg-setup] DROPPED."
}

# ---- 入口 ----
main() {
    local sub="${1:-}"
    case "${sub}" in
        create) cmd_create ;;
        drop)   cmd_drop ;;
        name)   cmd_name ;;
        *)      usage ;;
    esac
}

main "$@"
