#!/bin/sh
# R15-D1：位移边 PostgreSQL 建库建表最小命令脚本（setup_edge_persist.sh）。
#
# 用途：在验证机（192.168.0.88）上为 edge_persist PG 真机测试准备数据库与表。
#       本脚本只做 CREATE DATABASE / CREATE TABLE，不执行任何业务数据写入。
#
# 用法：
#   sh eval/e2e/pg/setup_edge_persist.sh              # 用默认库名 lightmem_test
#   LIGHTMEM_PG_DB=mydb sh eval/e2e/pg/setup_edge_persist.sh
#
# 环境变量：
#   LIGHTMEM_PG_HOST     PG 主机（默认 localhost）
#   LIGHTMEM_PG_PORT     PG 端口（默认 5432）
#   LIGHTMEM_PG_USER     PG 用户（默认 postgres）
#   LIGHTMEM_PG_PASSWORD PG 密码（默认空）
#   LIGHTMEM_PG_DB       要创建的库名（默认 lightmem_test）
#
# 红线遵守：
#   - 只增新表：DDL 只含 CREATE TABLE / CREATE INDEX，绝不含 ALTER / DROP / TRUNCATE。
#   - 不硬编码凭据：全部走环境变量。
#   - 幂等：CREATE TABLE IF NOT EXISTS / CREATE INDEX IF NOT EXISTS。
#   - 脚本是 #!/bin/sh（FreeBSD 下走 dash），不能用 BASH_SOURCE。

set -u

PG_HOST="${LIGHTMEM_PG_HOST:-localhost}"
PG_PORT="${LIGHTMEM_PG_PORT:-5432}"
PG_USER="${LIGHTMEM_PG_USER:-postgres}"
PG_PASSWORD="${LIGHTMEM_PG_PASSWORD:-}"
PG_DB="${LIGHTMEM_PG_DB:-lightmem_test}"

# 构建 psql 连接参数（不含 -d，先连 postgres 模板库建库）。
# 注意：整串必须是一个闭合的双引号（此前误写成 "psql -h "$PG_HOST" ... 的多段拼接，
# 引号数为奇数 → 字符串未闭合，dash 在 DDL 处报 `Syntax error: "(" unexpected`）。
PSQL_BASE="psql -h $PG_HOST -p $PG_PORT -U $PG_USER"
if [ -n "$PG_PASSWORD" ]; then
    export PGPASSWORD="$PG_PASSWORD"
fi

# 1) 建库（若不存在）
if ! $PSQL_BASE -lqt | grep -q "^[[:space:]]*"$PG_DB"[[:space:]]"; then
    printf '创建数据库：%s\n' "$PG_DB"
    if ! $PSQL_BASE -c "CREATE DATABASE "$PG_DB";" postgres 2>&1; then
        printf 'FAIL: 建库失败\n' >&2
        exit 1
    fi
else
    printf '数据库已存在：%s（跳过）\n' "$PG_DB"
fi

# 2) 建表 + 索引（IF NOT EXISTS，幂等）
printf '建表 + 索引（幂等）...\n'
DDL="CREATE TABLE IF NOT EXISTS displacement_edges (
  id             BIGSERIAL PRIMARY KEY,
  scope          TEXT    NOT NULL,
  from_memory_id TEXT    NOT NULL,
  to_memory_id   TEXT    NOT NULL,
  relation       TEXT    NOT NULL CHECK (relation IN ('entity-alias','causal','task-flow','temporal')),
  weight         DOUBLE PRECISION NOT NULL CHECK (weight >= 0 AND weight <= 1),
  created_at     TIMESTAMPTZ NOT NULL,
  invalidated_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_disp_edge ON displacement_edges
  (scope, from_memory_id, to_memory_id, relation) WHERE invalidated_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_disp_edge_from ON displacement_edges (scope, from_memory_id);
CREATE INDEX IF NOT EXISTS idx_disp_edge_to ON displacement_edges (scope, to_memory_id);"

if ! $PSQL_BASE -d "$PG_DB" -c "$DDL" 2>&1; then
    printf 'FAIL: 建表失败\n' >&2
    exit 1
fi

printf 'PASS: 数据库 %s 已就绪，表 displacement_edges 已建。\n' "$PG_DB"
printf '测试时请设置环境变量：\n'
printf '  export LIGHTMEM_PG_HOST=%s\n' "$PG_HOST"
printf '  export LIGHTMEM_PG_PORT=%s\n' "$PG_PORT"
printf '  export LIGHTMEM_PG_USER=%s\n' "$PG_USER"
if [ -n "$PG_PASSWORD" ]; then
    printf '  export LIGHTMEM_PG_PASSWORD=***\n'
fi
printf '  export LIGHTMEM_PG_DB=%s\n' "$PG_DB"
