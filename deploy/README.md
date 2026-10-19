# deploy/ —— 部署初始化脚本（bootstrap）

> **定位**：这里放的是**部署/DBA 初始化**脚本，**不是 schema migration**。
> 与 `migrations/` **物理隔离**、语义分离：
>
> | 目录 | 管什么 | 谁负责 | 红线③（禁改 schema）是否适用 |
> |---|---|---|---|
> | `migrations/` | 表/列/索引/约束等 **schema 变更** | 应用版本演进 | ✅ 适用（受控） |
> | `deploy/` | 角色、库、权限、secrets 注入等**环境初始化** | 部署 / DBA | ❌ 不适用（不改 schema） |
>
> 业主裁定（2026-10-20）：「红线③约束的是 schema 变更，不是 DBA 初始化脚本。」

## init-roles.sql —— 应用运行角色 `lightmem_app`

**为什么需要**：应用若以 superuser（如 `postgres`）连接，将绕过 RLS、`pg_read_all_data`
等数据库层约束，多租户隔离退化为「仅靠应用层 SQL 谓词」。配套的**代码侧启动自检**
见 `kernel/pg_superuser_check.go`（连接建立后执行
`SELECT rolsuper FROM pg_roles WHERE rolname = current_user`，默认 fail-fast）。

### 用法

口令**必须从 secrets 注入**，不得写进仓库或命令行历史：

```sh
export PGPASSWORD="<postgres 超级用户口令>"     # 仅建角色这一步需要
psql -h "$PGHOST" -p 5432 -U postgres -d "$PGDATABASE" \
     -v app_password="$LIGHTMEM_APP_PASSWORD" \
     -f deploy/init-roles.sql
```

- 幂等：可重复执行（角色已存在则跳过创建，不重置口令）。
- 未传 `app_password` 时脚本中止 —— **有意为之**：绝不创建无口令的可登录角色。
- 脚本末尾会打印复核行，`is_superuser` **必须为 `false`**。

### 授权内容

| 对象 | 授权 | 备注 |
|---|---|---|
| DATABASE（当前库） | `CONNECT` | |
| SCHEMA `public` | `USAGE` | |
| TABLE `public.memories` | `SELECT, INSERT, UPDATE, DELETE` | R28 实测：当前仅此一张业务表 |
| 后续新建表（DEFAULT PRIVILEGES） | 同上 | 避免每次建表漏授权 |

实测（2026-10-20）：**无 sequence**——`memory_id` 为 `text`，由应用层生成，
故无需 `GRANT ... ON SEQUENCE`。

### 已知未覆盖（登记为 L-241，本轮不做）

`public.memories` 的 `rowsecurity = false`，`pg_policies` 为空 —— **未启用 RLS**。
启用 RLS 属 schema 变更且需按租户设计 policy 与 `TO` 子句，
已登记为下一轮 migration 候选；本脚本**不**开启 RLS。

---

## 连接串切换（裁定二第 3 件：配置变更，代码零改动）

应用侧连接角色由环境变量决定，切换只需改配置：

| 场景 | 变量 |
|---|---|
| 服务（FreeBSD rc.d） | `/etc/rc.conf`：`lightmem_pg_user="lightmem_app"`（口令经 secrets/env 注入） |
| 门禁 / CI | `LIGHTMEM_PG_USER=lightmem_app`、`LIGHTMEM_PG_PASSWORD=...` |

切换后必须验证（缺 GRANT 会在此暴露）：

```sh
cd kernel && go test -run 'EdgePersistPg' -count=1 -v .
```

### 回退

若切换后出现授权不全或其它失败，**直接改回原配置**即可，代码无需改动；
不要用 `LIMS_PG_SUPERUSER_CHECK=warn` 掩盖问题后宣布完成。

### 过渡开关（仅临时使用）

`LIMS_PG_SUPERUSER_CHECK`：

| 值 | 行为 |
|---|---|
| 未设置 / `fail` | **默认**：superuser 连接 → 阻断启动 |
| `warn` | 只告警，不阻断（过渡期） |
| `off` | 跳过自检（显式关闭，会留日志） |
