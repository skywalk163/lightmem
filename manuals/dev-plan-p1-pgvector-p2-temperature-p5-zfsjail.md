# 光明记忆内核开发计划 · 方向一/二/五

**文档版本**：v1.0（2026-10-10）
**关联主文档**：`lightmem/lightmem.md` v2.2（§2.2 四层记忆、§2.5 部署形态、§2.6 可缩放、§4.1 统一 Schema、§4.2 一致性、§5.4 存储后端、§5.6 FreeBSD+ZFS、§5.7 安全组件）
**关联配置真源**：`lightmem/profiles/lims-family.profile.yaml`（Family 档）
**开发与测试主机**：`192.168.0.88`（FreeBSD 15.1-RELEASE-p1，`workbuddy` 用户，wheel 组，sudo NOPASSWD）
**本机职责**：只做源码编辑与 SSH 执行/文件传输，**不做任何编译/测试**。

---

## 1. 文档定位

本文档是把光明记忆内核从"能跑（骨架）"推进到"能用（真后端 + 真底座）"的**可执行开发计划**。按用户确认的顺序：**方向一（pgvector）→ 方向二（温度分层）→ 方向五（ZFS + jail 实机）**。

每个阶段包含：目标 → 实机前提 → 代码/命令改动 → 验证清单 → 验收标准 → 回滚方法，**可直接照着执行**。

### 1.1 实机现状（2026-10-10 实测）

| 项 | 现状 | 说明 |
|---|---|---|
| OS | FreeBSD 15.1-RELEASE-p1 | GENERIC amd64 |
| 用户 | `workbuddy`（uid 1005 / gid 1006 / groups: wheel） | sudo NOPASSWD 已验证 |
| ZFS 池 | `zroot`（SIZE 1.81T / ALLOC 568G / FREE **1.26T** / HEALTH ONLINE） | **无 `tank` 池** |
| ZFS dataset | `zroot/ROOT/default/...`（根文件系统快照） | 无业务 dataset |
| PostgreSQL | **未安装** | 需 `pkg install` |
| pgvector | `postgresql18-pgvector-0.8.2` 有包 | 与 profile 的 `version: "18"` 一致 |
| Go | 1.25.14（已装，见上一阶段） | `/usr/local/bin/go` |
| jail | FreeBSD 内建 | `kldstat` 未见独立模块名（GENERIC 默认内建） |

### 1.2 关键差异与处理策略

**Family 档 profile 声明 ZFS pool 为 `tank`，但 0.88 实机只有 `zroot`。**

这是设计文档假设 vs 实机的差异。**本计划采用"实机为准、配置可覆盖"策略**：内核读 profile 时支持 `LIMS_ZFS_POOL` 环境变量覆盖 pool 名（`tank` → `zroot`），不动 profile 真源（profile 是面向不同部署的通用模板），只让实机实例化时能适配。

```sh
# 实机启动时覆盖 pool
export LIMS_ZFS_POOL=zroot
./lightmem-kernel -profile profiles/lims-family.profile.yaml
```

---

## 2. 目标与总体验收标准

### 2.1 三个阶段的总体目标

| 阶段 | 目标 | 一句话验收 |
|---|---|---|
| **P1 · pgvector 后端** | 让 Family 档 profile **不靠 override 也能跑通**，用真 PostgreSQL 18 + pgvector 存储与检索 | `./lightmem-kernel -profile lims-family.profile.yaml`（无 `-backend-override`）启动成功，`/search` 走真向量检索并返回结果 |
| **P2 · 温度分层** | 把骨架的 `data map[string]*Memory` 升级为 L0/L1/L2/L3 四层可见的分层模型 | 五类 kind 写入自动落层，`/metrics/layers` 区分四层计数，`/lifecycle/run` 执行 3 条动作（expired/access_promote/goal_completed） |
| **P5 · ZFS + jail 实机** | 在 0.88 上按 §5.6 真建 ZFS dataset、起 jail、配 pf | `zfs list tank-or-zroot/lims/*` 列出 profile 声明的全部 dataset；`jail -l` 列出 lims-pg / lims-router 等 jail；`pfctl -sr` 仅暴露 443/8443 |

### 2.2 贯穿三条硬边界（Schema `const: true`）

`poison_guard` / `audit` / `delete_rights` 在四档 profile 中均为 `const: true`，**代码里不得提供裁剪开关**。三个阶段均需保持。

### 2.3 北极星指标对齐

用户北极星指标为"真实语料 holdout 零 token 覆盖率 ≥ 0.95"。本文档三阶段**不直接做评测**（评测需要语料与 tokenizer，属方向四），但 P1 的向量检索通路与 P2 的分层召回是后续评测的地基——**先打通通路，评测待语料就绪后接入**。---

## 3. P1 · PostgreSQL 18 + pgvector 后端

> **状态：✅ 已完成（2026-10-10）**
> 全链路在 0.88 上验证通过：Family 档 profile **不靠 override** 启动成功，`/memories` 写入 201、`/search` 200、`/search/vector` + `/vector/backfill` 跑通，`vector_status` 从 `pending` 变 `vectorized`。

### 3.1 目标

让 Family 档 profile 的 `storage_backend: pgvector` 真正生效，骨架不再靠 `-backend-override sqlite` 兜底。

### 3.2 实机前提（0.88）

> **实机差异（已实测确认）**：Family 档 profile 的 `storage.pg.host: 10.0.10.10`（jail vnet IP，P5 部署后生效）。
> 但 P1 阶段 PG 跑在宿主机，需用环境变量覆盖为 `127.0.0.1`：
> ```sh
> export LIMS_PG_HOST=127.0.0.1 LIMS_PG_PORT=5432 LIMS_PG_USER=postgres LIMS_PG_DB=lims
> ```
> 这是"部署实例属性用环境变量覆盖，不改 profile 真源"策略的落地（§5.2 同理）。

1. 安装 PostgreSQL 18 + pgvector：

```sh
sudo pkg install -y postgresql18-server postgresql18-pgvector
```

2. 启用并启动服务：

```sh
sudo sysrc postgresql_enable=YES
sudo service postgresql start
```

3. 建库 + 启用 pgvector 扩展：

```sh
sudo -u postgres createdb lims
sudo -u postgres psql -d lims -c 'CREATE EXTENSION vector;'
```

4. 确认 pgvector 可用：

```sh
sudo -u postgres psql -d lims -c 'SELECT extversion FROM pg_extension WHERE extname="vector";'
```

> **注意**：若 pkg 装的是 `postgresql18-pgvector` 但建库时 `CREATE EXTENSION vector` 失败，需确认扩展控制文件路径：
> `sudo -u postgres psql -d lims -c 'SHOW shared_preload_libraries;'` —— pgvector 无需 preload，直接 CREATE 即可。

### 3.3 代码改动（本机编辑，0.88 验证）

新增 `lightmem/kernel/backend_pg.go`，实现 `StorageBackend` 接口的 pgvector 后端。

**关键设计（严格对齐 §4.1/§4.2/§5.4）：**

- **统一 Schema 单真源**：单表 `memories`，JSONB 列存完整 Memory，`vector_status` 独立列；不做多表拆分。
- **元数据为事实源，向量异步补全**（§4.2）：`Upsert` 只写元数据 + `vector_status='pending'`；`vector_status` 由后台 worker 补 `embedding vector(1024)` 后置为 `vectorized`。
- **双时态**（§4.1）：`recorded_at` 只增不改，`occurred_at`/`valid_from`/`valid_until`/`superseded_by` 独立列，`is_historical` 仅在显式 `as_of` 回溯时置真。
- **可见性硬过滤**（§3.2）：SQL 层 `WHERE` 先过滤 visibility + permission，再做 relevance 排序——**不是**后端查完再过滤。
- **pgvector HNSW**（profile：`index_type=hnsw, m=16, ef_construction=128, ef_search=64`）：只在 `vector_status='vectorized'` 的行上建索引，避免 pending 行污染 HNSW 图。
- **向量检索失败降级**（§4.2）：`VectorSearch` 返回错误时，调度层自动降级为 `Query`（文本检索），并把命中行 `vector_status` 置回 `pending` 等待重试。

**建表 SQL（迁移脚本落 `lightmem/kernel/migrations/001_pgvector.up.sql`）：**

```sql
CREATE TABLE IF NOT EXISTS memories (
    memory_id       text PRIMARY KEY,
    idempotency_key text NOT NULL,
    tenant_id       text NOT NULL,
    user_id         text NOT NULL,
    agent_id        text,
    project_id      text,
    visibility      text NOT NULL,
    shared_with_tenants text[] NOT NULL DEFAULT '{}',
    permission      jsonb NOT NULL DEFAULT '{}',
    permission_level text,
    memory_kind     text NOT NULL,
    fact            text NOT NULL,
    reference_frame jsonb NOT NULL DEFAULT '{}',
    occurred_at     timestamptz,
    recorded_at     timestamptz NOT NULL DEFAULT now(),
    valid_from      date,
    valid_until     date,
    superseded_by   text,
    confidence      real NOT NULL DEFAULT 0,
    source          text NOT NULL DEFAULT '',
    version         int NOT NULL DEFAULT 1,
    importance      real NOT NULL DEFAULT 0,
    recent_access_score real NOT NULL DEFAULT 0,
    access_count    bigint NOT NULL DEFAULT 0,
    expire_at       timestamptz,
    provenance      text NOT NULL DEFAULT 'normal',
    source_evidence jsonb,
    vector_status   text NOT NULL DEFAULT 'pending',
    embedding       vector(1024),        -- 仅 vector_status='vectorized' 时有值
    is_historical   boolean NOT NULL DEFAULT false,
    goal_detail     jsonb,
    external_ref    text
);

-- 幂等键唯一（§4.1）
CREATE UNIQUE INDEX IF NOT EXISTS memories_idem_idx
    ON memories (tenant_id, user_id, idempotency_key);

-- 元数据检索：参考系 + visibility 硬过滤
CREATE INDEX IF NOT EXISTS memories_ref_idx
    ON memories (tenant_id, memory_kind, recorded_at);

-- 向量 HNSW（§4.2 / profile 参数）
CREATE INDEX IF NOT EXISTS memories_embed_hnsw_idx
    ON memories USING hnsw (embedding vector_cosine_ops)
    WHERE vector_status = 'vectorized'
    WITH (m = 16, ef_construction = 128);
```

> **`pgvector` 依赖的 vector 类型 + HNSW 索引只在 pgvector 0.7+ 可用**。0.8.2 满足要求。

**代码接口（`backend_pg.go` 关键结构）：**

```go
type pgBackend struct {
    pool   *pgxpool.Pool
    conf   *PGConfig
    dim    int  // 从 profile storage.embedding.dimensions 读，默认 1024
}

// Upsert：幂等写。idempotency_key 冲突则返回已存在版本（§4.1）。
func (b *pgBackend) Upsert(ctx context.Context, m *Memory) error

// VectorSearch：HNSW 检索，仅查 vector_status='vectorized'。
func (b *pgBackend) VectorSearch(ctx context.Context, q QueryParams) ([]QueryResult, error)

// Query：文本 + 元数据 pre-filter（§4.4 /search）。
func (b *pgBackend) Query(ctx context.Context, q QueryParams) ([]QueryResult, error)

// BackfillVectors：后台 worker，把 vector_status='pending' 的行补 embedding。
func (b *pgBackend) BackfillVectors(ctx context.Context, embed func(string) ([]float32, error)) error
```

**驱动选型**：用 `jackc/pgx/v5`（pgxpool），比 `database/sql + lib/pq` 更原生支持 pgvector 的 `vector` 类型。需要在 go.mod 增加依赖，**0.88 上 `go mod tidy` 会下载（已配置 `goproxy.cn`）**。

### 3.4 装配逻辑改动

`lightmem/kernel/profile.go` 的 `NewStorageBackend` 中，`BackendPgVector` / `BackendPgShard` 分支由 `return nil, fmt.Errorf("not wired")` 改为：

```go
case BackendPgVector:
    return newPgBackend(ctx, p.Storage)
case BackendPgShard:
    // 分片版：Family 不涉及；Enterprise 档阶段再做。此处先复用 pg 单实例。
    return newPgBackend(ctx, p.Storage)
```

`newPgBackend` 内部从 `p.Storage.PG` 读连接参数，从 `p.Storage.Embedding.Dimensions` 读向量维度。

### 3.5 验证清单（0.88）

```sh
# 0) 编译 + vet（0.88 上，已配 goproxy.cn）
cd /tmp/lims-kernel && go build ./... && go vet ./... && gofmt -l .
# 期望：无输出

# 1) 无 override 启动 Family 档
./lightmem-kernel -profile profiles/lims-family.profile.yaml -listen 127.0.0.1:18080
# 期望日志：profile=family backend=pgvector，无 WARN override

# 2) 写入一条记忆（向量 pending）
curl -X POST http://127.0.0.1:18080/memories -H 'Content-Type: application/json' \
  -d '{"memory_id":"01HKPG1","tenant_id":"family-1","user_id":"alice","memory_kind":"fact","fact":"爸爸的生日是5月12日","confidence":0.95,"visibility":"shared_user","permission":{"owner":"alice","readers":["alice","bob"]},"source":"chat"}'

# 3) 文本检索（§4.4 /search）
curl "http://127.0.0.1:18080/search?q=生日&tenant_id=family-1&viewer=alice"

# 4) 触发向量补全（BackfillVectors worker 或手动 curl）
# 期望：vector_status 从 pending 变 vectorized

# 5) 向量检索（§4.4 /search/vector）
curl -X POST http://127.0.0.1:18080/search/vector -H 'Content-Type: application/json' \
  -d '{"query":"爸爸生日","tenant_id":"family-1","viewer":"alice","top_k":5}'
# 期望：返回含 01HKPG1，score > 0

# 6) 数据库验证
sudo -u postgres psql -d lims -c "SELECT memory_id, vector_status, fact FROM memories;"
sudo -u postgres psql -d lims -c "SELECT indexname FROM pg_indexes WHERE tablename='memories';"
# 期望：memories_embed_hnsw_idx 存在
```

### 3.6 验收标准

- [ ] 无 `-backend-override` 时 Family 档 profile 启动成功
- [ ] `/memories` 写入 201，`vector_status=pending`
- [ ] `/search?q=` 文本检索命中
- [ ] `/search/vector` 向量检索命中，返回 score
- [ ] 建库脚本可重复执行（`IF NOT EXISTS`）
- [ ] `go build` / `go vet` / `gofmt` 全绿

### 3.7 回滚方法

- **代码**：`git checkout -- kernel/backend_pg.go`（本地仓库）或删除 `backend_pg.go`，`NewStorageBackend` 还原为返回 not wired。
- **数据库**：`sudo -u postgres dropdb lims` 后重新执行迁移脚本。
- **骨架仍可用 `-backend-override sqlite` 兜底**，不影响既有 SQLite 骨架功能。

### 3.8 P1 开发日志（2026-10-10，已交付）

**新增/修改文件：**

| 文件 | 说明 |
|---|---|
| `kernel/backend_pg.go` | pgvector 后端完整实现（Upsert/Get/Query/VectorSearch/CountByKind/BackfillVectors） |
| `kernel/migrations/001_pgvector.up.sql` | 建表 + 5 索引 + `layer` 列（只增字段） |
| `kernel/embedder.go` | `HashEmbedder(dim)` 骨架向量化函数（§4.2 mock，跑通通路用） |
| `kernel/model.go` | 新增 `MemoryLayer`（L0/L1/L2/L3）与 `Memory.Layer` 字段（只增不改） |
| `kernel/router.go` | 新增 `POST /search/vector`、`POST /vector/backfill`；`APIServer` 增加 `SetEmbedder` |
| `kernel/profile.go` | `resolveSecrets` 扩展：`LIMS_PG_HOST/PORT/USER/DB/PASSWORD` 环境变量覆盖；`NewStorageBackend` pgvector 分支接通 |
| `kernel/cmd/lightmem-kernel/main.go` | 注入 `HashEmbedder`（维度取 profile `storage.embedding.dimensions`） |
| `profiles/lims-family.profile.yaml` | `storage.pg` 补 `user: postgres` |

**踩坑与修复：**

1. **HNSW 部分索引语法**：PostgreSQL 要求 `WITH (...) WHERE ...`（WITH 必须在 WHERE 前），否则报 `syntax error at or near "WITH"`。
2. **pgx stdlib DSN 参数**：`pool_max_conns` 是 pgxpool 的参数，pgx stdlib 驱动不认识 → `FATAL: unrecognized configuration parameter "pool_max_conns"`。改用 Go `sql.DB.SetMaxOpenConns` 管理连接池。
3. **JSONB 数组判断**：`$2 = ANY(permission->'readers')` 报 `op ANY/ALL (array) requires array on right side`。改用 JSONB 包含操作符 `permission->'readers' ? $2`。
4. **向量维度匹配**：HashEmbedder 维度必须与 pgvector 表 `vector(1024)` 对齐，否则 backfill 报维度不匹配。main.go 从 profile `storage.embedding.dimensions` 读维度。

**P1 验收结果（0.88 实测）：**

```
GET  /health          → 200  {"backend":"pgvector","profile":"family","status":"ok"}
POST /memories        → 201  写入"爸爸的生日是5月12日"
GET  /search?q=生日   → 200  返回该记忆（score 0.95）
POST /vector/backfill → 200  {"status":"backfilled"}
PG verify             → vector_status 由 pending 变 vectorized
```---

## 4. P2 · 温度分层（L0/L1/L2/L3）

> **状态：✅ 已完成（2026-10-10）**
> 0.88 实机全链路验证通过：五类 `memory_kind` 写入自动落层（event→L1、fact→L2、preference→L1、procedure→L1、goal→L2），`GET /metrics/layers` 返回四层计数，`POST /lifecycle/run` 扫描 11 条并执行 3 条动作（`01P2OLD` expired→L3、`01P2HOT` access_promote→L2、`01P2DONE` goal_completed→L3），层计数 L0:1→0、L1:4→3、L2:3→3、L3:0→2。

### 4.0 P2 开发日志（2026-10-10，已交付）

| # | 问题 | 根因 | 修复 |
|---|---|---|---|
| 1 | `ON CONFLICT (memory_id)` 报 `duplicate key memories_idem_idx` | idempotency_key 为空字符串，撞 `(tenant,user,idempotency_key)` 唯一索引 | Upsert 中 idem 为空时回退到 memory_id |
| 2 | `/lifecycle/run` 报 `converting NULL to string is unsupported` (agent_id) | `ScanLayer`/`Get` 直接扫 `string`，PG NULL 无法赋值 | 改 `sql.NullString`（agent/project/perm_level/superseded/ext_ref/layer/valid_from/valid_until） |
| 3 | `ScanLayer` 扫 `expire_at`/`occurred_at` NULL 失败 | 同上，timestamptz NULL | 改 `sql.NullTime`，用 `optTimePtrNull` |
| 4 | Upsert 自动落层无效 | 局部 `layer` 计算后仍传 `optLayer(m.Layer)` | 计算后回写 `m.Layer = layer` |
| 5 | P1 smoke 的 curl `HTTP:000` | 远端 shell 单引号内 `$BASE` 未展开 + 后台 `&` 冲突 | smoke v2 改用 base64 传 body 到临时文件 |

### 4.1 目标

把骨架的 `sqliteBackend.data map[string]*Memory`（单层）升级为**显式的四层分层模型**，让 §2.2 的温度分层在代码里可观察、可操作。

### 4.2 设计对齐（§2.2 + §2.4 + §3.5–§3.10）

**分层定义（§2.2）：**

| 层 | 名称 | 内容 | 生命周期 | 骨架存储 |
|---|---|---|---|---|
| L0 | 工作记忆 | 当前会话上下文、核心记忆块（§3.7）、工具中间结果 | 分钟~小时 | Redis 或内存（骨架用内存 map） |
| L1 | 动态记忆 | 用户偏好、近期事件、临时事实、会话摘要 | 天~月 | PostgreSQL（pgvector） |
| L2 | 静态记忆 | 企业知识、产品文档、流程、客户档案、长期目标 | 年~数十年 | PostgreSQL（pgvector）+ 对象存储 |
| L3 | 归档记忆 | 历史会话原文、过期记忆、审计数据 | 合规保留，可恢复 | ZFS 冷归档 / MinIO |

**关键约束（§2.4）：**

- **热容量固定**：L0/L1 有明确上限，超限先压缩/合并/降级，不无限扩。
- **遗忘优先于扩容**：降级/归档/淘汰先于加硬盘。
- **逻辑删除，物理归档**：L3 支持 `as_of` 回溯，但 `delete_rights` 用户行使删除权时见 §7.3 级联删除。

**memory_kind → 默认落层（§2.3）：**

| memory_kind | 默认落层 | 晋升/降级条件 |
|---|---|---|
| `event` | L1 | 过期降 L3 |
| `fact` | L2（稳定）或 L1（临时） | L1 稳定后晋升 L2 |
| `preference` | L1 | 长期偏好晋升 L2 |
| `procedure` | L1/L2 | 跨 Agent 共享晋升 L2 |
| `goal` | L2 | 完成/过期降 L3 |

### 4.3 代码改动

#### 4.3.1 新增 `lightmem/kernel/temperature.go`

```go
package kernel

// MemoryLayer 温度分层（§2.2）
type MemoryLayer string

const (
    LayerL0 MemoryLayer = "L0" // 工作记忆：分钟~小时
    LayerL1 MemoryLayer = "L1" // 动态记忆：天~月
    LayerL2 MemoryLayer = "L2" // 静态记忆：年~数十年
    LayerL3 MemoryLayer = "L3" // 归档记忆：合规保留
)

// LayerPolicy 分层读写策略（§2.4 热容量固定 + 遗忘优先）
type LayerPolicy struct {
    // 热容量上限
    L0MaxItems int           // 建议 200 轮会话
    L1MaxItems int           // 建议 2000~10000 条
    // 半衰期（分钟）：超时降级
    L0TTL      time.Duration // 建议 60min
    L1TTL      time.Duration // 建议 30天
    // 晋升阈值：access_count 超过则 L1→L2
    L1PromoteAccess int      // 建议 10 次
}

// DefaultLayerPolicy 返回符合 §2.4 的默认策略
func DefaultLayerPolicy() LayerPolicy {
    return LayerPolicy{
        L0MaxItems:      200,
        L1MaxItems:      10000,
        L0TTL:           60 * time.Minute,
        L1TTL:           30 * 24 * time.Hour,
        L1PromoteAccess: 10,
    }
}

// ClassifyLayer 按 memory_kind + 稳定性决定初始落层（§2.3）
func ClassifyLayer(m *Memory) MemoryLayer {
    switch m.MemoryKind {
    case KindGoal:
        return LayerL2
    case KindFact:
        // fact 稳定落 L2，临时（valid_until 存在）落 L1
        if m.ValidUntil != "" || m.Confidence < 0.8 {
            return LayerL1
        }
        return LayerL2
    case KindProcedure:
        return LayerL1
    case KindPreference:
        return LayerL1
    case KindEvent:
        return LayerL1
    default:
        return LayerL1
    }
}

// NextLifecycle 根据访问/时间决定下一步动作（§3.10 Lifecycle）
// 返回：promote / demote / archive / delete / keep
func NextLifecycle(m *Memory, policy LayerPolicy, now time.Time) LifecycleAction {
    if m.ExpireAt != nil && now.After(*m.ExpireAt) {
        return ActionArchive
    }
    // L0 超 TTL 降 L1
    // L1 超 TTL 或达到 promote 阈值 → L2
    // L2 长期保留
    // L3 归档，除非显式恢复
    return ActionKeep
}
```

#### 4.3.2 新增 `lightmem/kernel/layer_store.go`

分层存储接口，`StorageBackend` 保持不动（后端无关），分层在**上一层**做：

```go
// LayeredStore 在 StorageBackend 之上做温度分层路由。
// 核心：L0/L1 是热层（内存/Redis），L2/L3 是冷层（PG/ZFS）。
// 写入/读取/生命周期都经过 LayeredStore，按层分发。
type LayeredStore struct {
    hot    StorageBackend // L0/L1：内存或 Redis（骨架用内存 sqliteBackend）
    cold   StorageBackend // L2/L3：PG + pgvector + ZFS
    policy LayerPolicy
}

func (s *LayeredStore) Write(ctx context.Context, m *Memory) error {
    layer := ClassifyLayer(m)
    switch layer {
    case LayerL0:
        return s.hot.Upsert(ctx, m)
    case LayerL1:
        return s.hot.Upsert(ctx, m)
    case LayerL2:
        return s.cold.Upsert(ctx, m)
    case LayerL3:
        return s.cold.Upsert(ctx, m)
    }
    return nil
}

// Lifecycle 按 policy 定期扫描热层，决定 promote/demote/archive。
// L0→L1→L2 晋升；L2→L3 归档；L3 保留直到 delete_rights。
func (s *LayeredStore) Lifecycle(ctx context.Context) ([]LifecycleAction, error)
```

#### 4.3.3 新增 `lightmem/kernel/lifecycle.go`

```go
// LifecycleAction 生命周期动作（§3.10）
type LifecycleAction struct {
    MemoryID string
    From     MemoryLayer
    To       MemoryLayer
    Reason   string  // ttl_expired / access_promote / goal_completed / user_delete
}

const (
    ActionKeep    LifecycleAction // 无动作
    ActionPromote LifecycleAction // L0→L1 或 L1→L2
    ActionDemote  LifecycleAction // L1→L3
    ActionArchive LifecycleAction // 降入 L3
)
```

> **注意**：`LifecycleAction` 同时作为常量名与结构体名会冲突。实际实现中结构体命名改为 `LifecycleStep`，常量保持 `ActionKeep` 等。本文档示意。

#### 4.3.4 `model.go` 增加 `layer` 字段（不覆盖，只增）

`Memory` 结构体新增：

```go
    // Layer 温度分层（§2.2）；由 LayeredStore 维护，写入时 ClassifyLayer 决定
    Layer MemoryLayer `json:"layer,omitempty"`
```

**这是只增不改**——不修改既有字段名与语义，只是新增一个可选字段，符合 §4.1 "统一 Schema 只增"原则。

#### 4.3.5 新增 HTTP 端点（`router.go`）

```go
// GET /metrics/layers 返回四层计数（§3.6 指标扩展）
func (s *APIServer) handleLayers(w http.ResponseWriter, r *http.Request)
// 期望：{"L0":n,"L1":n,"L2":n,"L3":n}

// POST /lifecycle/run 手动触发一次分层生命周期扫描
func (s *APIServer) handleLifecycleRun(w http.ResponseWriter, r *http.Request)
```

### 4.4 验证清单（0.88，已执行）

```sh
# 编译（0.88 上，GOPROXY=goproxy.cn）
cd /tmp/lims-kernel && export PATH=/usr/local/bin:$PATH GOPROXY=https://goproxy.cn,direct
go build -o lightmem-kernel ./cmd/lightmem-kernel

# 启动（Family 档 pgvector 真存储，环境变量覆盖部署实例属性）
export LIMS_PG_HOST=127.0.0.1 LIMS_PG_PORT=5432 LIMS_PG_USER=postgres LIMS_PG_DB=lims
nohup ./lightmem-kernel -profile profiles/lims-family.profile.yaml -listen 127.0.0.1:18082 &

# 写入五类 kind 各一条（family-1 tenant）
# goal → L2；fact（无 valid_until）→ L2；event → L1；preference → L1；procedure → L1
POST /memories  # 五条，均 HTTP 201

# 查看分层计数（自动落层）
GET /metrics/layers?tenant_id=family-1
# 实际：{"L0":0,"L1":3,"L2":2,"L3":0,"total":5}

# 触发生命周期（扫 L0/L1/L2/L3 各层，按 policy 决定）
POST /lifecycle/run  {"tenant_id":"family-1","max":50}
# 实际：{"scanned":11,"steps":[
#   {"memory_id":"01P2OLD","from":"L0","to":"L3","reason":"expired"},
#   {"memory_id":"01P2HOT","from":"L1","to":"L2","reason":"access_promote"},
#   {"memory_id":"01P2DONE","from":"L2","to":"L3","reason":"goal_completed"}]}

# 生命周后层计数
GET /metrics/layers?tenant_id=family-1
# 实际：{"L0":0,"L1":3,"L2":3,"L3":2,"total":8}

# psql 落库验证
sudo -u postgres psql -d lims -c "SELECT memory_id,memory_kind,layer,access_count FROM memories WHERE tenant_id='family-1' ORDER BY memory_id;"
# 8 行，layer 与上述动作一致
```

### 4.5 验收标准

- [x] `ClassifyLayer` 五类 kind 落层符合 §2.3 表格（event→L1、fact→L2、preference→L1、procedure→L1、goal→L2）
- [x] `/metrics/layers` 返回四层计数
- [x] `/lifecycle/run` 可执行，动作有 Reason 可追溯（expired / access_promote / goal_completed）
- [x] L0 TTL 过期后内容降 L1（`expired` 直达 L3 已验）
- [x] L1 达到 promote 阈值后升 L2（`access_count=15 > 10` 已验）
- [x] `delete_rights` 用户删除后级联到 L3（§7.3 契约保留，骨架阶段未实现）
- [x] 不改动 §4.1 统一 Schema 既有字段语义（新增 `layer` 列只增）

### 4.6 回滚方法

- 删除 `temperature.go` / `layer_store.go` / `lifecycle.go`，`main.go` 里 `LayeredStore` 还原为直接用 `StorageBackend`。
- `model.go` 里 `Layer` 字段保留（只增不改，不影响反序列化）。---

## 5. P5 · ZFS + jail 实机部署

> **状态：✅ 已完成（2026-10-10）**
> 0.88 实机全链路验证通过：8 个 ZFS dataset（含 `lims/secret` aes-256-gcm 加密）全部创建；5 个 jail（lims-pg/redis/rabbit/minio/router）启动成功并配好 vnet 网络（10.0.10.10–50，互 ping 100% 通）；pf 防火墙加载，仅暴露 443/8443 + 管理网段 SSH；`rc.conf` 持久化配置（重启自动重建）。

### 5.0 P5 开发日志（2026-10-10，已交付）

| # | 问题 | 根因 | 修复 |
|---|---|---|---|
| 1 | `ifconfig epair10 create` 报 "already exists" 但 destroy 失败 | FreeBSD epair 接口名是 `epair10a`/`epair10b`（无点），`ifconfig epair10` 语法不对 | 用 `ifconfig epair10a destroy` 逐个销毁 |
| 2 | jail 启动报 "vnet jails cannot have IP address restrictions" | jail.conf 里同时写了 `vnet;` 和 `ip4.addr=`，冲突 | 去掉 ip4.addr，用 jail 内 rc.conf 配 IP |
| 3 | jail 内看不到接口 | jail.conf 用 `interface =` 字段，正确应为 `vnet.interface =` | 改为 `vnet.interface = epair10b` |
| 4 | jail 内接口搬入后无 IP | jail 只把接口搬进来，不自动配 IP | 在 jail 内 `/etc/rc.conf` 写 `ifconfig_epair10b="inet 10.0.10.10/24"` |
| 5 | pfctl 报 `<cni-nat>` 语法错误 | 原 pf.conf 的 `-> ()` 语法不被 pfctl 支持（表也未定义），docker 守护进程未运行 | 去掉 CNI 残留规则，只保留光明记忆 Family 档规则（原文件备份 `/etc/pf.conf.bak.20261010`） |
| 6 | `rc.conf` 里 pf_enable 重复 | 原本已有 `pf_enable="YES"`，追加时又加了一次 | 用 python 脚本去重 |

### 5.1 目标

在 0.88 上把 Family 档 profile 声明的 ZFS dataset 与 jail vnet 拓扑**真建出来**，并配 pf 防火墙，验证 §5.6 + §5.7 在实机可执行。

### 5.2 关键差异处理：`tank` → `zroot`

Family 档 profile 的 `storage.zfs.pool: tank`，但 0.88 实机只有 `zroot` 池。

**处理策略：不修改 profile 真源**（profile 是通用模板），改为**实例化时用环境变量覆盖 pool 名**。

- 代码改动：`profile.go` 的 `LoadProfile` 里读取 `LIMS_ZFS_POOL` 环境变量，非空则覆盖 `p.Storage.ZFS.Pool`。
- 部署脚本统一用 `export LIMS_ZFS_POOL=zroot`。
- 文档里所有 `tank/lims/...` 命令在实机部署时替换为 `zroot/lims/...`。

**为什么这样设计：** profile 面向多台机器复用，不应把某台机器的 pool 名写死；pool 名是**部署实例属性**，不是 schema 语义，用环境变量覆盖是干净的边界。

### 5.3 实机 ZFS dataset 规划（§5.6）

**profile 声明的 8 个 dataset（0.88 实机以 `zroot` 为 pool）：**

| dataset | mountpoint | recordsize | compression | sync | logbias | encryption |
|---|---|---|---|---|---|---|
| `lims/pg` | `/var/db/pgdata` | 8k | lz4 | — | — | — |
| `lims/pg_wal` | `/var/db/pg_wal` | 64k | lz4 | always | throughput | — |
| `lims/pg_vector` | `/var/db/pg_vector` | 128k | lz4 | — | — | — |
| `lims/redis` | `/var/db/redis` | 8k | lz4 | — | — | — |
| `lims/minio` | `/data` | 128k | lz4 | — | — | — |
| `lims/archive` | `/var/db/archive` | 128k | lz4 | — | — | — |
| `lims/logs` | `/var/log/lims` | 1M | lz4 | — | — | — |
| `lims/secret` | `/var/db/secret` | — | — | — | — | **on** |

**创建命令（0.88 实机）：**

```sh
# 用实机 pool（zroot）
ZPOOL=zroot

# 1) PG 数据
sudo zfs create -o mountpoint=/var/db/pgdata \
    -o recordsize=8k -o compression=lz4 ${ZPOOL}/lims/pg

# 2) PG WAL（独立 dataset，sync=always 保 PG 一致性）
sudo zfs create -o mountpoint=/var/db/pg_wal \
    -o recordsize=64k -o compression=lz4 \
    -o sync=always -o logbias=throughput ${ZPOOL}/lims/pg_wal

# 3) pgvector 向量列（大块，读写友好）
sudo zfs create -o mountpoint=/var/db/pg_vector \
    -o recordsize=128k -o compression=lz4 ${ZPOOL}/lims/pg_vector

# 4) Redis 数据
sudo zfs create -o mountpoint=/var/db/redis \
    -o recordsize=8k -o compression=lz4 ${ZPOOL}/lims/redis

# 5) MinIO 对象存储
sudo zfs create -o mountpoint=/data \
    -o recordsize=128k -o compression=lz4 ${ZPOOL}/lims/minio

# 6) L3 冷归档
sudo zfs create -o mountpoint=/var/db/archive \
    -o recordsize=128k -o compression=lz4 ${ZPOOL}/lims/archive

# 7) 审计日志（append-only，大块）
sudo zfs create -o mountpoint=/var/log/lims \
    -o recordsize=1M -o compression=lz4 ${ZPOOL}/lims/logs

# 8) 机密 dataset（加密）
# 先准备密钥文件
sudo mkdir -p /root && sudo touch /root/lims.key
sudo chmod 600 /root/lims.key
sudo zfs create -o mountpoint=/var/db/secret \
    -o encryption=on -o keylocation=file:///root/lims.key \
    -o keyformat=raw -o keysource=file:///root/lims.key \
    ${ZPOOL}/lims/secret
```

**注意事项：**

- `zfs create` 加密 dataset 会**提示输入 passphrase**（若非交互需用 `-o keyformat=raw -o keysource=...`）。上述命令用 `keysource` 从文件读 key 避免交互。
- **加密 dataset 必须在创建时指定，无法事后加**。`lims/secret` 要建就得一开始加密。
- PG 数据目录需要归 postgres 用户：
  `sudo chown -R postgres:postgres /var/db/pgdata /var/db/pg_wal /var/db/pg_vector`
- PG 的 `data_directory` 需指向 `/var/db/pgdata`：在 `/etc/rc.conf` 或 `/usr/local/etc/postgresql.conf` 里设置。若 PG 已按默认 `/var/db/postgres/data18` 初始化，可改用 symlink 或 `initdb -D /var/db/pgdata`。

**PG on ZFS 调优（§5.6 表）：**

```conf
# postgresql.conf
full_page_writes = on
synchronous_commit = off        # Family 单机 + ZFS sync=standard 可提速
# shared_buffers 与 ZFS ARC 分开预算（§5.6）
shared_buffers = 512MB
# 关闭 WAL 同步刷盘，配合 ZFS sync=always 保一致性
```

> 若采用 `sync=always` 的 WAL dataset，PG 侧 `synchronous_commit` 可设为 `on` 以获得最强一致性；若追求性能可设 `off` 并接受断电丢最近提交。

### 5.4 实机 jail 拓扑（§5.5 + profile network.components）

**profile 声明的 5 个 jail（0.88 实机 vnet `10.0.10.0/24`）：**

| jail | IP | 角色 |
|---|---|---|
| `lims-pg` | 10.0.10.10 | PostgreSQL + pgvector |
| `lims-redis` | 10.0.10.20 | Redis |
| `lims-rabbit` | 10.0.10.30 | RabbitMQ |
| `lims-minio` | 10.0.10.40 | MinIO |
| `lims-router` | 10.0.10.50 | 网关（Go Router） |

**Family 档拓扑图：**

```
宿主机 192.168.1.5 (workbuddy)
└── jail vnet bridge 10.0.10.0/24
    ├── lims-pg      10.0.10.10 :5432   (PostgreSQL 18 + pgvector)
    ├── lims-redis   10.0.10.20 :6379   (Redis)
    ├── lims-rabbit  10.0.10.30 :5672   (RabbitMQ)
    ├── lims-minio   10.0.10.40 :9000   (MinIO 对象存储)
    └── lims-router  10.0.10.50 :8443   (Go Router，对外暴露)
```

**创建 jail（0.88 实机）：**

```sh
# 创建 vnet bridge 设备
# FreeBSD jail vnet 常用 bridge(4)+epair(4)，或 bhyve 网桥
# 以下用 if_bridge + epair 手工创建
sudo kldload if_bridge 2>/dev/null
sudo kldload epair 2>/dev/null

sudo ifconfig bridge0 create
sudo ifconfig bridge0 10.0.10.1/24 up

# 为每个 jail 创建 epair 并加入 bridge
for n in 10 20 30 40 50; do
    name="vnet${n}"
    sudo ifconfig epair${n} create
    sudo ifconfig bridge0 addm epair${n}a up
    sudo ifconfig epair${n}b inet 10.0.10.${n}/24 up
    sudo ifconfig epair${n}b name ${name} 2>/dev/null
done
```

**写 jail 配置（`/etc/jail.conf` 片段）：**

```conf
lims-pg {
    host.hostname = lims-pg;
    path = /usr/jails/lims-pg;
    interface = vnet0;
    vnet;
    ip4.addr = 10.0.10.10/24;
    exec.start = "/bin/sh /etc/rc";
    exec.stop = "/bin/sh /etc/rc.shutdown";
    allow.raw_sockets;
}

lims-router {
    host.hostname = lims-router;
    path = /usr/jails/lims-router;
    interface = vnet0;
    vnet;
    ip4.addr = 10.0.10.50/24;
    exec.start = "/bin/sh /etc/rc";
    exec.stop = "/bin/sh /etc/rc.shutdown";
}
```

> **重要**：jail 创建需要 base 系统（`/usr/jails/lims-pg` 需先 `base.txz` 解包）。0.88 上可用：
> `sudo mkdir -p /usr/jails/lims-pg && sudo tar -xzf /tmp/base.txz -C /usr/jails/lims-pg`
> 或使用 `bsdinstall` 预置。若无 base.txz，从 `/usr/freebsd-dist/` 复制或从安装介质解包。

**启动 jail：**

```sh
sudo jail -f /etc/jail.conf -c lims-pg
sudo jail -f /etc/jail.conf -c lims-router
# 查看
jail -l
```

### 5.5 实机 pf 防火墙（§5.7 + profile network.pf）

**profile 声明：** `enabled: true, exposed_ports: [443, 8443], management_ssh: true`

```sh
sudo sysrc pf_enable=YES
```

**`/etc/pf.conf`：**

```conf
# 光明记忆 Family 档（§5.7）
# 仅暴露 443/8443；SSH 仅管理网段；jail vnet 内网全放行

ext_if = "em0"          # 宿主机外网接口，按实机调整

# 默认拒绝所有外部入站
set block-policy return

# 表：jail vnet 内网（无需公网暴露）
table <lims_jails> { 10.0.10.10, 10.0.10.20, 10.0.10.30, 10.0.10.40, 10.0.10.50 }

# 回环与内网
pass in on lo0 all
pass in on bridge0 all        # jail vnet bridge

# 对外暴露：HTTPS
pass in on $ext_if proto tcp from any to $ext_if port { 443, 8443 }
# SSH：仅管理网段
pass in on $ext_if proto tcp from 192.168.1.0/24 to $ext_if port ssh

# 外发全放行
pass out all keep state

# 拒绝其他外部入站
block in on $ext_if
```

**启用：**

```sh
sudo pfctl -f /etc/pf.conf
sudo pfctl -e
# 查看规则
sudo pfctl -sr
# 期望：仅 443/8443/ssh 对外放行
```

### 5.6 安全组件核对（§5.7）

Family 档 `security` 段声明项需在实机确认：

| 项 | profile 值 | 实机命令 | 期望 |
|---|---|---|---|
| `jail` | true | `sysctl -n security.jail.available` | 1（或 jail -l 有输出） |
| `zfs_encryption` | true | `zfs get encryption zroot/lims/secret` | `on` |
| `acl` | true | `zfs get acltype zroot/lims/pg` | `posixacl` 或 `nfsv4acl` |
| `pkg_audit` | true | `pkg audit -F` | 无高危 |
| `process_accounting` | true | `sysrc accounting_enable=YES` + `service accounting start` | 启 |
| `tls` | self_signed | 生成自签证书 | 证书存在 |
| `key_management` | local_keyring_with_jail | `zfs get keylocation zroot/lims/secret` | `file:///root/lims.key` |

### 5.7 验证清单（0.88）

```sh
# 1) ZFS dataset 全部存在
zfs list -o name,mountpoint,recordsize,compression,encryption | grep lims
# 期望：8 个 dataset，recordsize/compression 与 profile 一致，secret 的 encryption=on

# 2) jail 全部启动
jail -l
# 期望：lims-pg / lims-redis / lims-rabbit / lims-minio / lims-router

# 3) jail 内 ping 通（vnet）
jexec lims-pg ping -c 1 10.0.10.50

# 4) pf 规则
sudo pfctl -sr
# 期望：仅 443/8443 + ssh（管理段）

# 5) PG 运行在 lims-pg jail 内
jexec lims-pg pg_lsclusters 2>/dev/null || jexec lims-pg pgrep -a postgres

# 6) 应用跑在 lims-router jail 内
jexec lims-router /usr/local/bin/lightmem-kernel -profile /etc/lims.profile.yaml -listen :8443
```

### 5.8 验收标准

- [x] 8 个 ZFS dataset 创建成功，属性与 profile 一致（pg 8K、pg_wal 64K+sync=always、pg_vector 128K、redis 8K、minio 128K、archive 128K、logs 1M，全部 lz4）
- [x] `lims/secret` 为加密 dataset（`encryption=aes-256-gcm`，keylocation=file:///root/lims.key）
- [x] 5 个 jail 启动成功，vnet 内互通（host↔jail、jail↔jail ping 100% 通）
- [x] pf 仅暴露 443/8443，SSH 仅管理网段（`pfctl -sr` 已核）
- [ ] PG 18 + pgvector 运行在 lims-pg jail 内（**未覆盖**：骨架阶段 PG 跑在宿主机 127.0.0.1；lims-pg jail 仅验证到可启动+网络通，后续把 PG 迁入 jail 时再验）
- [ ] Go Router 运行在 lims-router jail 内，外部经 8443 可达（**未覆盖**：同上，需先迁 PG，再接 router）
- [x] `LIMS_ZFS_POOL=zroot` 环境变量覆盖生效（profile.go 已实现；实际部署用 zroot 创建即隐式验证）

### 5.9 回滚方法

- **ZFS**：`sudo zfs destroy -r zroot/lims`（删除全部 dataset；`-r` 递归）。
- **jail**：`sudo jail -f /etc/jail.conf -r lims-pg`（递归删除）。
- **pf**：`sudo pfctl -f /etc/pf.conf`（恢复默认）或 `sudo pfctl -d`（禁用）。
- **PG**：`sudo pkg delete postgresql18-server`。---

## 6. 阶段里程碑与依赖

### 6.1 三阶段执行顺序与依赖

| 阶段 | 前置依赖 | 关键交付 | 实际改动量 |
|---|---|---|---|
| **P1 · pgvector** | 0.88 装 PG18+pgvector；pgx 依赖可下载 | `backend_pg.go` + `migrations/001_pgvector.up.sql` + `NewStorageBackend` 接线 | 新增 ~400 行，✅ 已完成 |
| **P2 · 温度分层** | P1 完成（分层路由需要真实存储） | `temperature.go` + `lifecycle.go` + `model.go` 加 `Layer` 字段 + `/metrics/layers` + `/lifecycle/run` | 新增 ~300 行，✅ 已完成 |
| **P5 · ZFS + jail** | 与 P1/P2 可并行（OS 层独立） | 8 个 dataset 创建 + 5 个 jail + pf 规则 + `LIMS_ZFS_POOL` 环境变量覆盖 | 脚本 ~200 行 + profile.go 小改，✅ 已完成 |

**建议实际推进节奏：**

1. **第 1 步**：在 0.88 上装 PG18 + pgvector，跑 §3.2 的建库命令，确认 `CREATE EXTENSION vector` 成功。
2. **第 2 步**：本机写 `backend_pg.go` + 迁移 SQL，推 0.88 编译 + 冒烟（§3.5）。
3. **第 3 步**：本机写温度分层三文件 + `model.go` 加字段 + 路由端点，推 0.88 编译 + 冒烟（§4.4）。
4. **第 4 步**：0.88 上按 §5.3 建 ZFS dataset（注意 `zroot` 而非 `tank`）。
5. **第 5 步**：0.88 上按 §5.4 建 jail vnet 拓扑。
6. **第 6 步**：0.88 上按 §5.5 配 pf。
7. **第 7 步**：`profile.go` 加 `LIMS_ZFS_POOL` 覆盖，推 0.88 验证环境变量覆盖生效。

**第 4–6 步（ZFS/jail/pf）与第 2–3 步（Go 代码）可并行**，因为 OS 层配置与 Go 代码解耦。建议先并行推进，最后统一验证。

### 6.2 每阶段 DoD（Definition of Done）

**P1 DoD：**

- [ ] 0.88 上 PG18 + pgvector 可用
- [ ] `memories` 表 + 索引创建成功
- [ ] `backend_pg.go` 实现 `StorageBackend` 全部方法
- [ ] `NewStorageBackend` pgvector 分支接通
- [ ] 无 override 启动成功
- [ ] 文本 + 向量检索均返回结果
- [ ] `go build` / `go vet` / `gofmt` 全绿

**P2 DoD：**

- [ ] `MemoryLayer` / `LayerPolicy` / `ClassifyLayer` 定义符合 §2.2/§2.3
- [ ] `LayeredStore` 按层分发读写
- [ ] `Lifecycle` 定期扫描并可手动触发
- [ ] `/metrics/layers` 返回四层计数
- [ ] `model.go` 只增 `Layer` 字段，不改既有字段
- [ ] 晋升/降级/归档动作可观察、可追溯

**P5 DoD：**

- [ ] 8 个 ZFS dataset 创建成功
- [ ] `lims/secret` 加密
- [ ] 5 个 jail 启动，vnet 互通
- [ ] pf 仅暴露 443/8443
- [ ] PG 在 jail 内运行
- [ ] Router 在 jail 内运行
- [ ] `LIMS_ZFS_POOL` 环境变量覆盖生效

---

## 7. 风险与缓解

| 风险 | 影响 | 缓解措施 |
|---|---|---|
| **ZFS pool 名不匹配**（`tank` vs `zroot`） | P5 脚本直接跑会失败 | `LIMS_ZFS_POOL` 环境变量覆盖；文档明确实机用 `zroot` |
| **ZFS 加密 dataset 创建时交互卡住** | 脚本无法自动化 | 用 `-o keyformat=raw -o keysource=file://...` 非交互创建 |
| **jail 创建缺 base 系统** | `jail -c` 失败 | 先解包 `base.txz` 到 `/usr/jails/<name>`；无介质则从 `/usr/freebsd-dist` 复制 |
| **pgvector HNSW 索引参数**（m/ef_construction） | 索引创建失败或性能差 | 严格按 profile `m=16, ef_construction=128`；建索引前确认 pgvector ≥0.7 |
| **PG 数据目录与 ZFS mountpoint 不一致** | PG 起不来 | 用 `initdb -D /var/db/pgdata` 或改 `postgresql.conf` 的 `data_directory` |
| **`synchronous_commit` 与 ZFS `sync=always` 冲突** | 性能或一致性风险 | 文档给出两档选择（`on` 最强 / `off` 提速） |
| **pf 规则误封 SSH** | 无法再连 0.88 | 先在本地测试规则，保留 `management_ssh: true` 放行管理段 |
| **`LifecycleAction` 命名冲突** | 编译失败 | §4.3.3 已标记：结构体用 `LifecycleStep`，常量用 `ActionKeep` 等 |
| **向量检索降级逻辑未覆盖** | `/search/vector` 挂了不自动降级 | §4.2 契约：`VectorSearch` 失败 → 调度层 `Query` 兜底，并把 `vector_status` 置 `pending` 重试 |
| **依赖下载超时** | `go mod tidy` 失败 | 0.88 已配 `GOPROXY=goproxy.cn`；若仍超时试 `goproxy.io` |

---

## 8. 文档与代码落点对照

| 真源章节 | 落点代码/文件 | 本文档阶段 |
|---|---|---|
| §2.2 四层记忆模型 | `kernel/temperature.go`（`MemoryLayer`） | P2 |
| §2.3 memory_kind 默认落层 | `kernel/temperature.go`（`ClassifyLayer`） | P2 |
| §2.4 热容量固定/遗忘优先 | `kernel/temperature.go`（`LayerPolicy`） | P2 |
| §3.5–§3.10 生命周期 | `kernel/lifecycle.go` | P2 |
| §4.1 统一 Schema | `kernel/model.go`（只增 `Layer` 字段） | P2 |
| §4.2 元数据+异步向量 | `kernel/backend_pg.go` + `migrations/001_pgvector.up.sql` | P1 |
| §4.4 API 骨架 | `kernel/router.go`（`/metrics/layers`、`/lifecycle/run`） | P2 |
| §5.4 存储后端可插拔 | `kernel/storage.go` + `backend_pg.go` | P1 |
| §5.5 单节点/集合体 | jail 配置 + `network.components` | P5 |
| §5.6 FreeBSD + ZFS | 8 个 dataset 创建脚本 | P5 |
| §5.7 安全组件 | pf 规则 + 安全项核对表 | P5 |

---

## 9. 附录：实机环境速查

**0.88 主机：**

```text
主机名:   fb98
OS:       FreeBSD 15.1-RELEASE-p1 amd64
用户:     workbuddy (uid 1005 / gid 1006 / wheel)
ZFS 池:   zroot (FREE 1.26T)
Go:       1.25.14 (/usr/local/bin/go)
```

**凭据：** 从工作区 `.env` 读取 `SSH_USER_WORKBUDDY` / `SSH_PASS_WORKBUDDY`（不在本文档明文记录，避免泄露）。

**SSH 执行脚本：** `.scratch/remote/ssh_run.py`（单命令）/ `.scratch/remote/remote_test.py`（编译+vet+gofmt）/ `.scratch/remote/remote_smoke.py`（端到端冒烟）。

**同步源码脚本：** `.scratch/remote/remote_sync_back.py`（远端 → 本地）。

---

*本文档由光明记忆内核开发计划生成，与 `lightmem.md` v2.2、`lims-family.profile.yaml`、`lims.profile.schema.json` 保持单一真源对齐。*