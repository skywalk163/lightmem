-- deploy/init-roles.sql —— 应用运行角色 bootstrap（R28-A2 / L-240，业主裁定二）
--
-- ⚠️ 定位：**部署初始化（DBA/bootstrap）脚本，不是 schema migration**。
--    业主裁定原文：「CREATE ROLE ... 属于部署初始化，放在 deploy/init-roles.sql
--    或部署脚本里，与 schema migration 目录物理隔离。红线③约束的是 schema 变更，
--    不是 DBA 初始化脚本。」
--    本文件**不进 migrations/**，也不改任何既有表/索引/约束。
--
-- 为什么需要它：应用若以 superuser（如 postgres）连接，将绕过 RLS、
-- pg_read_all_data 等数据库层约束，多租户隔离退化为「仅靠应用层 SQL 谓词」。
-- 配套代码侧自检见 kernel/pg_superuser_check.go（启动期 rolsuper 断言）。
--
-- 用法（口令必须从 secrets 注入，**不要写进仓库**）：
--     psql -h $PGHOST -U postgres -d $PGDATABASE \
--          -v app_password="$LIGHTMEM_APP_PASSWORD" -f deploy/init-roles.sql
--
-- 幂等：可重复执行（角色/授权均已存在判断）。
-- 依赖 psql 元命令（\gset / \if / \gexec），需用 psql 执行，不能走纯 SQL 通道。

\set ON_ERROR_STOP on

-- 1) 角色是否已存在（0/1 → 供 \if 求值）
SELECT count(*) AS role_exists FROM pg_roles WHERE rolname = 'lightmem_app' \gset

\if :role_exists
    \echo 'role lightmem_app already exists — 跳过创建（不重置口令；如需轮换请单独执行 ALTER ROLE）'
\else
    -- NOSUPERUSER 是这条脚本的全部意义所在。
    -- :'app_password' 由 psql 在**顶层语句**中替换为带引号的字面量（dollar-quoted
    -- 块内 psql 不做变量插值，故这里不能用 DO 块）。
    CREATE ROLE lightmem_app LOGIN PASSWORD :'app_password'
        NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT;
    \echo 'created role lightmem_app (NOSUPERUSER)'
\endif

-- 口令经 psql -v app_password=... 注入；未传该变量时 psql 会报错并因
-- ON_ERROR_STOP 中止 —— **有意为之**：绝不创建无口令/空口令的可登录角色。
-- 注：0.88 → 10.0.10.10 当前为 trust 免密，口令为将来收紧 pg_hba 预置。

-- 2) 库/模式级授权（库名取当前连接库，无需额外变量）
SELECT current_database() AS db_name \gset
GRANT CONNECT ON DATABASE :"db_name" TO lightmem_app;
GRANT USAGE ON SCHEMA public TO lightmem_app;

-- CREATE 是**必需**而非权宜：应用启动含自建表 DDL（kernel/authn_store.go：
-- users / agent_tokens / auth_tokens，均为 CREATE TABLE IF NOT EXISTS；
-- 测试夹具的 displacement_edges 同理）。R14 红线限定此类 DDL **只含 CREATE，
-- 绝不含 ALTER/DROP/TRUNCATE**，故授予 CREATE 的风险可控；
-- 且 CREATE 不等于 superuser——它既不绕过 RLS，也不赋予 pg_read_all_data。
-- 实测依据：不授 CREATE 时，非 superuser 连接会在 `CREATE TABLE IF NOT EXISTS`
-- 处直接报 `permission denied for schema public`（SQLSTATE 42501），即使表已存在。
GRANT CREATE ON SCHEMA public TO lightmem_app;

-- 3) 表级授权。
--    R28 实测：生产库当前仅 memories 一张业务表，**无 sequence** ——
--    memory_id 为 text，由应用层生成，故无需 GRANT ... ON SEQUENCE。
GRANT SELECT, INSERT, UPDATE, DELETE ON public.memories TO lightmem_app;

-- 兜底：对 public schema 下**当前存在的所有表**补授权（幂等）。
-- 用途：测试库/多表环境（如 displacement_edges）不必改本脚本；
-- 生产库只有 memories 时与上一行等价。
SELECT format('GRANT SELECT, INSERT, UPDATE, DELETE ON public.%I TO lightmem_app', tablename)
FROM pg_tables
WHERE schemaname = 'public' \gexec

-- 4) 未来由 bootstrap 执行者新建的表，默认授予同名权限（避免每次建表漏授权）。
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO lightmem_app;

-- 5) 应用表 owner 交给 lightmem_app。
--    原因（实测）：PG 对**已存在**的对象执行 `CREATE TABLE/INDEX IF NOT EXISTS`
--    时**仍先做权限检查**，非 owner 会被拒（实测报错：
--    `must be owner of table displacement_edges` SQLSTATE 42501）——
--    所以光有 CREATE 权限不够，表必须由应用角色拥有，否则应用侧 DDL
--    （kernel/authn_store.go 的 users/agent_tokens/auth_tokens，测试夹具的
--    displacement_edges）在第二次启动起就会失败。
--    这是标准部署形态（应用角色拥有应用表）：owner 变更**不改表结构**，
--    且 superuser（postgres）后续执行 migration / ALTER 不受任何影响。
SELECT format('ALTER TABLE public.%I OWNER TO lightmem_app', tablename)
FROM pg_tables
WHERE schemaname = 'public' \gexec

-- 6) 复核输出（人读）：确认 lightmem_app 非 superuser 且可登录
SELECT rolname,
       rolcanlogin AS can_login,
       rolsuper    AS is_superuser   -- 必须为 false
FROM pg_roles
WHERE rolname = 'lightmem_app';

-- ⚠️ 已知未覆盖（不在本轮，登记为 L-241）：
--   当前 public.memories 的 rowsecurity = false（**未启用 RLS**），pg_policies 为空。
--   启用 RLS 属 schema 变更 + 需要按租户设计 policy 与 TO 子句，
--   已登记为下一轮 migration 候选，本脚本**不**开启 RLS。
