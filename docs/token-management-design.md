# 正式版 Token 管理系统设计（R22）

> **范围声明**：本系统是 **lightmem 后台服务的内部鉴权与管理**，**不**承担前台功能
> （人的登录页面、SSO 回调、社区检索 UI）。后者由**未来的前台服务**负责——
> 前台服务通过本系统提供的 **内部接口** 或 **认证器扩展点** 对接。
>
> **定位**：lightmem 不只是「AI 的大脑」，未来也是**给人提供信息录入/检索**的平台
> （类似知乎/豆瓣的论坛属性）。因此 token 管理要同时覆盖 **两类主体**：
> Agent（AI）与人。本文把它们统一在**一个可插拔认证器框架**下。

对应主文档版本：`lightmem.md` v2.x　｜　红线版本：`scripts/gate.sh`　｜　R22 起生效

---

## 1. 设计目标

| # | 目标 | 约束 |
|---|---|---|
| G1 | **收缩性**：从 1 人到 >500 人，一套框架不换架构 | 挂到现有 4 档 profile（solo/smb/family/enterprise） |
| G2 | **可插拔认证器**：内部实现可被 Ory Kratos / SSO 替换 | 认证器走接口，业务层不感知 |
| G3 | **token 管理闭环**：签发/吊销/轮换/过期/scope/审计 | 管理 API，**不重启即生效**（修现有"改 token 必重启"缺陷） |
| G4 | **零回归**：现有 `issue_token.py` / `gen_configs.py` / stdio 单身份行为不变 | 红线④默认透传；新能力默认关 |
| G5 | **不新增第三方依赖** | `go.mod` 本轮零新增（红线⑤）；现有依赖已覆盖所需：`modernc.org/sqlite`（SQLite 存储）、`jackc/pgx`（PG 存储）、`gopkg.in/yaml.v3`（profile 扩展）、`golang.org/x/crypto`（密码哈希，未来前台要用） |
| G6 | **后台服务定位**：不碰登录页面/SSO 回调 | 只提供内部接口 + 扩展点 |

---

## 2. 边界：后台做什么、不做什么

```
┌─────────────────────────────────────────────────────────────┐
│  前台服务（未来，独立部署：R23+）                              │
│  人的登录页面 / SSO 回调 / 社区 UI / 检索前端                 │
└───────────────┬─────────────────────────────────────────────┘
                │  ① 前台服务作为「用户」调用后台管理 API
                │  ② 或前台服务对接外部 IdP（Ory Kratos/SSO），
                │     然后把已认证的用户身份交给后台
                ▼
┌─────────────────────────────────────────────────────────────┐
│  ★ 本系统：lightmem 后台服务的 Token 管理系统（R22）          │
│  · 认证器接口（可插拔：内部 / Ory Kratos / SSO）              │
│  · token 签发 / 吊销 / 轮换 / 过期 / scope                    │
│  · 主体管理（agent / user）与角色                             │
│  · 审计日志                                                   │
└─────────────────────────────────────────────────────────────┘
```

**后台明确不做**：
- 不提供人的注册/登录 UI（那是前台服务）
- 不实现 OAuth2 授权码流 / SAML 协议本体（只留**认证器接口**）
- 不接社交登录

**后台必须提供**（给未来前台服务对接用）：
- 认证器接口 `Authenticator`：前台可以注入自己的实现（例如包装 Ory Kratos 的 session 校验）
- 管理 API：前台服务可代为签发/吊销 token（例如用户登录后，前台调后台签发该用户的 session token）

---

## 3. 收缩性框架（挂到 4 档）

现有 profile 已定义好规模，本系统的**能力开关**挂在其上，随档位自动收敛：

| 档位 | 用户量级 | 认证器默认 | token 存储 | 管理 API | 说明 |
|---|---|---|---|---|---|
| **solo** | 1 人 | 内部（单 token） | SQLite 表（或仍走静态文件） | 可关闭 | 保持"最少指令"——`issue_token.py` 一条命令即用 |
| **smb** | 2–20 人 | 内部 | SQLite 表 | 开（本机回环可用） | 首个需要"吊销/轮换"的档位 |
| **family** | 20–500 人 | 内部 / 可插 Ory | PostgreSQL | 开（需管理员 token） | 团队协作、多人共享记忆 |
| **enterprise** | >500 人 | 可插 Ory Kratos / SSO | PostgreSQL（+ 分片） | 开（RBAC + 审计强制） | 对接企业 IdP、强制审计 |

配置骨架（profile 新增段，**默认缺省= 内部测试版行为**）：

```yaml
auth:
  require_auth: false          # 缺省 false = 零回归（现有默认透传）
  agent_tokens: {}             # 现有静态表（solo 档仍可用）

  # ── R22 新增（全部可缺省；缺省即回退到现有静态文件行为）──
  authenticator: internal       # internal | ory_kratos | sso | custom
  token_store:                  # static | db   （solo 默认 static，其余默认 db）
    backend: db                 # sqlite | pg
  admin_api:
    enabled: true               # 提供管理端点
    bind_loopback_only: true    # 默认仅本机回环可调管理 API
  audit:
    enabled: false              # 默认关（solo 不需要）；family+ 建议开
    retention_days: 365
```

> **红线④（零回归）**：`authenticator` / `admin_api` / `audit` 全部缺省，
> 现有 solo 部署与全部既有测试**行为完全不变**。

---

## 4. 认证器接口（扩展点核心）

这是 R22 的**核心抽象**。所有鉴权走这个接口，业务层（记忆读写、检索）只认 `Identity`。

```go
// kernel/authn.go  （R22 新增；纯 stdlib）

// Authenticator 是唯一鉴权入口。外部实现（Ory Kratos / SSO / 企业 IdP）
// 只需实现它即可无缝替换内部实现，业务层零改动。
type Authenticator interface {
    // Authenticate 用原始凭证换身份。token 或 session 由实现自行解释。
    Authenticate(ctx context.Context, raw string) (Identity, error)

    // AuthenticateRequest 从 HTTP 请求提取凭证（Bearer / Cookie / Header）。
    // 内部实现读 Authorization: Bearer；Ory 实现可读 cookie + 调 Kratos session API。
    AuthenticateRequest(ctx context.Context, r *http.Request) (Identity, error)

    // Name 返回认证器标识，用于日志与审计。
    Name() string
}

// Identity 是鉴权成功后的统一主体。现有 AgentIdentity 演进为它的一个特例。
type Identity struct {
    Kind       IdentityKind // Agent | User
    ID         string       // 主体内部 id（agent_id 或 user_id）
    AgentID    string       // Kind=Agent 时有效
    UserID     string       // Kind=User 时有效
    TenantID   string       // 租户边界
    Roles      []string     // RBAC：admin / user / auditor / agent
    Scope      []string     // 该凭证允许的操作域（如 memories:write）
    Issuer     string       // internal | ory_kratos | sso:<provider>
    TokenID    string       // 命中的 token 标识（脱敏展示）
    ExpiresAt  time.Time    // 零值 = 永不过期
}
```

**为什么这足够**：
- **内部实现**：查 `agent_tokens` / `auth_tokens` 表 → 填 `Identity`。
- **Ory Kratos**：实现读 cookie → 调 Kratos `GET /sessions/whoami` → 把返回的 user 映射成 `Identity{Kind:User}`。业务层完全不变。
- **SSO（OIDC/SAML）**：实现从 `Authorization` 或自定义头取已验签的 token/断言 → 映射 `Identity`。前台服务负责验签，后台只认结果。

> **接线默认关（红线）**：默认用内部实现；Ory/SSO 实现**随代码发布但默认不编译进路由**，
> 通过 `authenticator: ory_kratos` 显式开启。新增外部实现不改变既有行为。

---

## 5. 数据模型

新增 **4 张表**（DDL 只含 `CREATE TABLE/INDEX`，不含 `ALTER` —— 红线）。solo 档落 SQLite，
family/enterprise 落 PostgreSQL，**同一套 DDL 双写**（沿用 `backend_sqlite.go` /
`backend_pg.go` 的双后端模式）。

```sql
-- 5.1 users：人（含未来前台服务注册的用户）
CREATE TABLE IF NOT EXISTS users (
    id           TEXT PRIMARY KEY,           -- 内部 id（uuid）
    username     TEXT UNIQUE NOT NULL,
    email        TEXT,
    tenant_id    TEXT NOT NULL,
    role         TEXT NOT NULL DEFAULT 'user',  -- admin|user|auditor
    external_id  TEXT,                       -- 对接 Ory/SSO 时的外部主体 id（可空）
    idp          TEXT,                       -- internal|ory_kratos|sso:<provider>
    created_at   TIMESTAMP NOT NULL,
    updated_at   TIMESTAMP NOT NULL,
    disabled_at  TIMESTAMP                    -- 非空 = 停用
);
CREATE INDEX IF NOT EXISTS idx_users_tenant ON users (tenant_id);
CREATE INDEX IF NOT EXISTS idx_users_external ON users (idp, external_id);

-- 5.2 agent_tokens：Agent（AI）的身份 token
CREATE TABLE IF NOT EXISTS agent_tokens (
    id           TEXT PRIMARY KEY,           -- 内部 id（uuid）
    token_hash   TEXT UNIQUE NOT NULL,       -- 只存哈希（SHA-256），不存明文
    token_hint   TEXT NOT NULL,              -- 明文前 8 位，用于人识别
    agent_id     TEXT NOT NULL,
    user_id      TEXT,
    tenant_id    TEXT NOT NULL,
    scope        TEXT,                       -- 逗号分隔；空 = 全部
    expires_at   TIMESTAMP,                  -- 可空 = 永不过期
    revoked_at   TIMESTAMP,                  -- 非空 = 已吊销
    created_by   TEXT,                       -- 谁签发的（user_id 或 admin）
    created_at   TIMESTAMP NOT NULL,
    last_used_at TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_at_tenant ON agent_tokens (tenant_id);
CREATE INDEX IF NOT EXISTS idx_at_agent  ON agent_tokens (agent_id);
CREATE INDEX IF NOT EXISTS idx_at_active ON agent_tokens (tenant_id, revoked_at, expires_at);

-- 5.3 auth_tokens：人（前台服务代签）的 session token
CREATE TABLE IF NOT EXISTS auth_tokens (
    id           TEXT PRIMARY KEY,
    token_hash   TEXT UNIQUE NOT NULL,
    token_hint   TEXT NOT NULL,
    user_id      TEXT NOT NULL,
    tenant_id    TEXT NOT NULL,
    roles        TEXT NOT NULL DEFAULT 'user',
    scope        TEXT,
    expires_at   TIMESTAMP,
    revoked_at   TIMESTAMP,
    created_by   TEXT,
    created_at   TIMESTAMP NOT NULL,
    last_used_at TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_au_user ON auth_tokens (user_id);
CREATE INDEX IF NOT EXISTS idx_au_active ON auth_tokens (user_id, revoked_at, expires_at);

-- 5.4 auth_audit：鉴权与 token 管理审计（family+ 建议强制）
CREATE TABLE IF NOT EXISTS auth_audit (
    id           TEXT PRIMARY KEY,
    tenant_id    TEXT,
    actor_id     TEXT,                        -- 谁做的（token 主体）
    action       TEXT NOT NULL,               -- token_issue|token_revoke|token_rotate|login|deny
    target_id    TEXT,                        -- 目标 token / user
    detail       TEXT,
    ip           TEXT,
    created_at   TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_tenant ON auth_audit (tenant_id, created_at);
CREATE INDEX IF NOT EXISTS idx_audit_actor ON auth_audit (actor_id, created_at);
```

**设计要点**：

| 要点 | 说明 |
|---|---|
| **只存哈希** | `token_hash` 存 SHA-256。数据库泄露 ≠ token 泄露（明文只在签发那一刻返回给调用方） |
| **`token_hint`** | 明文前 8 位，让人在 UI/清单里能认出是哪条 token |
| **`expires_at` / `revoked_at`** | 两个正交的失效条件，都非空才判「不可用」 |
| **`scope`** | 细粒度权限，如 `memories:write,goals:write`；空 = 该主体全部权限 |
| **`idp` + `external_id`** | users 表为对接 Ory/SSO 预留：外部主体 id + 身份源 |
| **`roles`** | RBAC：`admin`（可管 token/users）、`user`（正常读写）、`auditor`（只读审计） |

---

## 6. 管理 API（HTTP，不重启即生效）

这是修复现有「改 token 必须重启服务」缺陷的关键。所有端点挂在 `/admin/` 前缀下，
默认**仅本机回环**可调（`admin_api.bind_loopback_only: true`）。

| Method | Path | 作用 | 权限 |
|---|---|---|---|
| POST | `/admin/users` | 建用户 | admin |
| GET | `/admin/users` | 列用户 | admin |
| PATCH | `/admin/users/{id}` | 改角色/停用 | admin |
| POST | `/admin/tokens/agent` | 签发 agent token | admin |
| GET | `/admin/tokens/agent?tenant=&agent=` | 列 agent token | admin |
| POST | `/admin/tokens/agent/{id}/revoke` | 吊销 | admin |
| POST | `/admin/tokens/agent/{id}/rotate` | 轮换（吊销旧 + 发新） | admin |
| POST | `/admin/tokens/session` | 签发人 session token（**前台服务代签**） | admin / 内部调用 |
| POST | `/admin/tokens/session/{id}/revoke` | 吊销人 session | admin / 用户本人 |
| GET | `/admin/audit?tenant=&from=&to=` | 查审计 | admin / auditor |

**请求/响应示例**（签发 agent token）：

```http
POST /admin/tokens/agent
Authorization: Bearer <admin-session-token>
Content-Type: application/json

{
  "agent_id": "trae",
  "tenant_id": "myteam",
  "user_id": "alice",
  "scope": "memories:write,goals:write",
  "expires_in_seconds": 2592000      // 30 天；缺省=永不过期
}

→ 201
{
  "token": "t-trae-3f8a2b...",        // 明文仅此一次返回
  "hint": "t-trae-3f",
  "expires_at": "2026-11-17T00:00:00Z",
  "agent_id": "trae"
}
```

**「不重启即生效」的实现**：token 校验从「启动时装配的静态表」改为**每次查库 + 内存缓存
（短 TTL，如 60s）**。吊销/轮换后最多 60s 全局生效，无需重启服务。
（solo 档可关闭缓存走静态文件，保持零开销。）

---

## 7. 鉴权流（统一走 Authenticator）

```
请求进来
   │
   ▼
AuthMiddleware ──► authenticator.AuthenticateRequest(ctx, r)
                         │
              ┌──────────┴───────────┐
              │ 读 Authorization:    │ 内部实现：查 agent_tokens / auth_tokens 表
              │ Bearer <token>        │   → 哈希比对 → 检查 revoked_at/expires_at
              │                        │
              │ Ory Kratos 实现：      │ 读 cookie → 调 Kratos whoami → 映射 Identity
              │ SSO 实现：            │ 前台已验签的 token/断言 → 映射 Identity
              └──────────┬───────────┘
                         ▼
                  得到 Identity（含 roles/scope/tenant）
                         │
                         ▼
              withIdentity(ctx, id)  →  业务层用 IdentityFromContext 取身份
```

- **现有 `AgentIdentity` / `LoadAgentTokens` / `SingleIdentity`**：保留为内部认证器的
  **fallback**（solo 档仍走静态文件，行为完全不变）。
- **`RequireIdentity` / `ResolveActor` / `EnforcePrivateOwner`**：不改，继续复用——
  它们只认 `IdentityFromContext`，而 `Identity` 只是字段扩展，天然兼容。
- **stdio 单身份约定不变**：CodeArts/Roo 每个进程仍只带一条 token。

---

## 8. 与现有工具的兼容

| 现有工具 | R22 关系 |
|---|---|
| `issue_token.py` | **保留**为 solo 档的"最少指令"入口。新增 `--write-db` 开关，把签发的 token 写入 DB（而非仅写 JSON 文件） |
| `gen_configs.py` | 不变。读 token 表仍兼容（`{token: {agent_id...}}` 格式不变） |
| `LIMS_AGENT_TOKENS` 环境变量 | 保留。solo 档静态文件路径不变；仅当 `token_store.backend=db` 时改为读库 |
| `SingleIdentity()` stdio 安全约定 | **不变**（表多条时仍不猜身份） |
| profile `auth.agent_tokens` | 保留为 fallback |

> **关键**：`token_hash` 是新增字段，但 **`issue_token.py` 产出的明文 token 完全兼容**
> —— 内部认证器对 DB 中的 token 做 SHA-256 后与库中哈希比对；对静态文件中的明文
> 直接常量时间比对。两条路径并存，互不干扰。

---

## 9. 安全与合规

| 项 | 设计 |
|---|---|
| **明文 token 只在签发时返回一次** | DB 只存 SHA-256。`admin/tokens/agent` 的 `token` 字段仅此一次 |
| **管理 API 默认仅回环** | `bind_loopback_only: true`；跨机访问须显式放开 + TLS |
| **审计强制（family+）** | 签发/吊销/轮换/拒绝登录全部入 `auth_audit` |
| **RBAC** | `admin`/`user`/`auditor` 三级；agent token 默认 scope 受限 |
| **脱敏** | 日志与错误里 token 只显示 `token_hint`（前 8 位），复用现有 `redactTokenID` |
| **不新增第三方依赖** | 哈希用 `crypto/sha256`（stdlib），不用 JWT 库；token 仍为 opaque（`t-` 前缀随机串）。`go.mod` 本轮**零新增**（`modernc.org/sqlite`/`jackc/pgx`/`yaml.v3` 已有，直接复用） |
| **红线** | 只增不改 Schema；DDL 无 ALTER；接线默认关；`go.mod` 零新增依赖 |

---

## 10. 迁移路径

| 阶段 | 交付 | 档位 |
|---|---|---|
| **R22.1** | 认证器接口 `Authenticator` + `Identity` 扩展；DB 表 DDL；内部认证器（DB 路径） | solo 零回归 |
| **R22.2** | 管理 API（users / tokens / revoke / rotate / audit）；"不重启生效"缓存 | smb 起 |
| **R22.3** | 审计 + RBAC 完整；`issue_token.py --write-db` | family |
| **R22.4** | Ory Kratos 适配器（默认编译但不启用）；SSO 接口示例 | enterprise |
| **R23+** | **前台服务**：人登录 UI / SSO 回调 / 社区检索前端（独立部署，调 R22 管理 API 或注入 Authenticator） | 全部 |

---

## 11. 已确认决策（R22 定稿）

> 以下 5 项已由负责人确认，作为实现依据。

| # | 决策项 | 结论 |
|---|---|---|
| 1 | **管理 API 鉴权** | 管理员**自己也当 `user`**，登录后拿 `auth_tokens`；管理操作校验 `roles 含 admin`。不引入独立的"管理员 agent token"概念 |
| 2 | **缓存 TTL** | **60s**。吊销/轮换后最多 60s 全局生效（接受"吊销后 60s 内可能仍生效"的窗口） |
| 3 | **`expires_in_seconds` 缺省** | **永不过期**（兼容现有测试版习惯）；`created_at` 仍记入审计以便追溯 |
| 4 | **`scope` 粒度** | **细粒度**（如 `memories:write`、`goals:read`），不采用粗粒度通配 |
| 5 | **Ory Kratos 适配器** | R22 **只定接口 `Authenticator` + 文档**；适配器实现**留到确实需要对接 IdP 时**再写（默认不编译进路由） |

**细粒度 scope 词汇表**（决策 #4 落地）：以 `<域>:<动作>` 表示。

| 域 | 动作 | 含义 |
|---|---|---|
| `memories` | `read` / `write` | 记忆读写 |
| `goals` | `read` / `write` | 工作计划 |
| `procedures` | `read` / `write` | 工作步骤 |
| `events` | `read` / `write` | 工作总结 |
| `admin` | `manage` | 管理 API（仅 admin role 需要） |

> scope 为空 = 该主体全部权限（保留测试版"一条 token 干所有事"的便利）；
> 需要收紧时逐个加 `memories:read` 等条目即可。

---

> **一句话**：本系统是后台的鉴权底座——**统一一个 `Authenticator` 接口，
> 一张 token 表管人 + Agent，一套管理 API 做到不重启即吊销**；
> 外部 IdP（Ory/SSO）通过接口插拔，前台服务通过管理 API 对接。
> 收缩性靠 4 档 profile 上的开关，solo 档完全保持现在的"最少指令"体验。
