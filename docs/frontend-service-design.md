# R23 前台服务设计文档（frontend-service-design.md）

> 版本 v1 草案 ｜ 2026-10-18
> 前置：R22（可插拔认证 + DB token + 管理 API）已交付并 19/19 验收通过。
> 定位：本设计只解决"人怎么用 lightmem"——登录、工作台、社区检索前端。
> **后台 Schema 与 SQL 零改动**（红线），前台凭据独立存放。

---

## 1. 一句话与目标

**R23 前台服务**是一个**独立部署**的 Web 服务，让人用浏览器登录 lightmem、
读写自己的记忆、浏览公开记忆。它不改造后台，只通过**两条既定通道**对接：

- **① REST API**：登录成功后，前台**代为调用后台管理 API** 签发该用户的
  `auth_tokens`，拿到 session token 去调业务端点（`/tools/memory/*`、`/search`）。
- **② Authenticator**：用户可配 OIDC/SSO，前台对接 IdP 后把已认证身份交给后台
  （复用 R22.4 的 `ssoAuthenticator` / `oryKratosAuthenticator`）。

**目标**：让"AI 记忆大脑"不止服务于 Agent，也**服务于人**——登录、录入、检索、
浏览社区，一套浏览器即用。

---

## 2. 边界

```
┌───────────────────────────────────────────────────────────────┐
│  ★ 本服务：lightmem-frontend（R23，独立部署）                   │
│  · 登录页 / SSO 回调                                           │
│  · 个人记忆工作台（录入 / 检索 / 管理）                        │
│  · 社区/公开记忆浏览（知乎/豆瓣式，先最简）                     │
│  · 持有【凭据表】+【会话表】                                   │
├───────────────┬───────────────────────────────────────────────┤
│ ① 调 REST API  │ ② 对接 IdP 后交给后台                           │
│    ↓          │    ↓                                          │
│  ★ 后台：lightmem-kernel（R22，不动）                           │
│  · users / agent_tokens / auth_tokens / auth_audit            │
│  · REST API：/tools/memory/*、/search、/api/v1/gdpr/*         │
│  · 管理 API：/admin/*（签 token）                              │
└───────────────────────────────────────────────────────────────┘
```

**前台明确不做**：
- 不存储记忆正文（那是后台的事，前台只调 API）
- 不重写鉴权（复用后台 `Authenticator`）
- 不修改后台 Schema / SQL（红线）

**前台必须提供**：
- 注册 / 登录 UI（用户名密码；OIDC/SSO 后续）
- 会话 cookie 管理（安全、可吊销）
- 个人工作台页面 + 社区浏览页面

---

## 3. 技术栈与形态

**Go 单二进制 + `html/template` + 内嵌静态资源（`embed`）**，**零第三方依赖**（红线⑤）。

| 项 | 选择 | 理由 |
|---|---|---|
| 语言/框架 | Go + `net/http` + `html/template` | stdlib 全够；与 kernel 同技术栈 |
| 前端资源 | `embed` 内嵌 CSS/JS | 单二进制部署，无 node 构建链 |
| 密码哈希 | `golang.org/x/crypto/bcrypt` | **go.mod 已含 x/crypto**，非新增 |
| 会话 | 服务端 session + 签名 cookie | stdlib 实现，可吊销 |
| 后台调用 | `net/http` 客户端 | 调 kernel REST API |

> 不引前端框架（React/Vue），避免 node 构建与新增依赖。交互用轻量 vanilla JS。
> 后续若需富 UI，再单独评估（非本阶段范围）。

---

## 4. 登录与会话

### 4.1 凭据（红线关键：不改后台 Schema）

后台 `users` 表**无** `password_hash` 列，红线也不允许改 Schema。
因此前台**自持凭据表**（自己的 SQLite/内存库），与后台 `users` 只按 `username` 对齐：

```sql
-- 前台自有库（lightmem-frontend.db），不是后台库
CREATE TABLE IF NOT EXISTS credentials (
    username      TEXT PRIMARY KEY,
    password_hash TEXT NOT NULL,          -- bcrypt
    tenant_id     TEXT NOT NULL,
    display_name  TEXT,
    created_at    TEXT NOT NULL,
    disabled_at   TEXT
);
CREATE TABLE IF NOT EXISTS sessions (
    id           TEXT PRIMARY KEY,
    username     TEXT NOT NULL,
    session_hash TEXT UNIQUE NOT NULL,    -- cookie 值的哈希
    expires_at   TEXT NOT NULL,
    revoked_at   TEXT,
    created_at   TEXT NOT NULL
);
```

**分工**：前台管"能不能登录"（凭据），后台管"登录后能干什么"（token / scope / RBAC）。
两者职责清晰，后台零改动。

### 4.2 登录流

1. 用户在 `/login` 提交用户名 + 密码
2. 前台查 `credentials`，`bcrypt.CompareHashAndPassword` 校验
3. 通过 → 前台调后台 `POST /admin/tokens/session`
   （`body: {user_id: <username>, tenant_id, roles, scope}`，用**前台自身的 service token** 鉴权）
4. 后台返回 `auth_tokens` 明文 → 前台把**其哈希**存入 `sessions`，cookie 只放 session id
5. 后续每次请求：前台解 cookie → 查 `sessions` → 用存的 session token 调后台业务端点

### 4.3 会话安全

- cookie：`HttpOnly`、`SameSite=Lax`、`Secure`（生产）、签名不可伪造
- 数据库只存 session **哈希**（同 R22 token 原则，泄漏 DB 也不泄凭据）
- 登出 / 改密 / 封禁 → `revoked_at` + 后台 token 吊销

---

## 5. 个人记忆工作台

登录后的首页，对齐 R22 已有的细粒度 scope 词汇表：

| 板块 | 后台端点 | 说明 |
|---|---|---|
| 录入 | `POST /tools/memory/save` | 记一条记忆（fact/event） |
| 召回 | `POST /tools/memory/recall` | 多信号融合检索（最常用） |
| 更新/更正 | `POST /tools/memory/update` | 追加新版本 |
| 工作计划 | `POST /tools/memory/goal` 等 | goal 建/推进度/改状态 |
| 步骤/总结 | `POST /tools/memory/step` `summary` | 记步骤、写总结 |
| 计划列表 | `GET /tools/memory/plan` | 看进行中计划 |

页面：`/workspace`（录入 + 最近记忆）、`/recall`（检索）、`/goals`（计划）。
全部走 vanilla JS 调前台 /api/* 代理 → 后台。

---

## 6. 社区 / 公开记忆（知乎/豆瓣式，先最简）

后台 `Visibility` 现有 4 值（private / shared_user / tenant / cross_tenant），
**无 `public`**。公开浏览需新增 `public`，这属"只增不改 Schema"的可接受边界，
但需负责人确认（见 §11 D5）。

- **若确认加 `public`**：后台 `model.go` 加一个 `Visibility = "public"` 常量 + 查询时
  允许 public 跨租户读；前台 `/discover` 页浏览公开记忆。
- **若暂不加**：R23 第一版社区部分用 `tenant` 浏览本租户记忆（团队内部论坛），
  公开互联网属性留到后续。

> 红线提醒：加 `Visibility` 常量是"只增"（新增值，不改既有 4 值语义），
> 但仍需负责人确认，避免越界。

---

## 7. 与后台对接

### 7.1 后台调用封装

前台持有一个 `backendClient`，封装 kernel REST API 调用，注入用户的 session token：

```go
type backendClient struct {
    baseURL string            // 后台地址，如 http://127.0.0.1:18444
    admin   string            // 后台管理 API 地址
    svc     string            // 前台自身的 service token（调 /admin/tokens/session 用）
    http    *http.Client
}
func (c *backendClient) call(ctx, method, path, sessionToken string, body any) (any, error)
func (c *backendClient) issueSession(ctx, userID, tenant, roles string) (token string, err error)
```

### 7.2 身份传递

- 前台调后台**业务端点**：`Authorization: Bearer <用户的 session token>`
- 前台调后台**管理 API**：`Authorization: Bearer <前台 service token>`
- 前台不伪造身份；用户身份完全由后台签发的 token 决定

---

## 8. 安全

| 风险 | 对策 |
|---|---|
| 密码泄露 | bcrypt 哈希；凭据表独立，不与后台 token 混存 |
| 会话劫持 | HttpOnly + SameSite=Lax + Secure；session 只存哈希 |
| CSRF | 所有写操作校验 CSRF token（cookie + 表单双提交） |
| 越权 | 前台只透传后台签发的 token；无后台 token 一律拒 |
| 后台暴露 | 后台管理 API 仍限回环（R22）；前台 service token 走内部 |
| 输入注入 | 前台不做 SQL（不用后台库）；模板用 html/template 自动转义 |

---

## 9. 收缩性

与后台 4 档 profile 对齐（**后台能力开关不变**，前台只调 API）：

| 档位 | 前台能力 |
|---|---|
| solo | 登录 + 个人工作台（无社区） |
| smb | + 团队检索（tenant） |
| family | + 家庭共享、公开记忆浏览 |
| enterprise | + OIDC/SSO、审计面板 |

前台自身也读 profile（或 `-frontend-tier` flag），据此隐藏未开放板块。

---

## 10. 迁移路径

| 阶段 | 交付 |
|---|---|
| **R23.1** | 设计定稿（本文档）+ 决策确认；`lightmem-frontend` 骨架（独立进程 + 登录 + 会话 + 个人工作台） |
| **R23.2** | 社区/公开浏览（依 D5 决策）+ 计划工作台完整 |
| **R23.3** | OIDC/SSO 登录（对接 R22.4）+ 安全加固 + 端到端验收 |

每阶段独立提交、跑 gate.sh、推三远端。

---

## 11. 待确认决策（R23 定稿依据）

| # | 决策项 | 选项 | 推荐 |
|---|---|---|---|
| D1 | 技术栈 | A) Go + html/template（零依赖） B) SPA 前端框架 | **A**：符合红线，单二进制 |
| D2 | 部署形态 | A) 独立进程 `lightmem-frontend` B) 挂进 kernel 进程 | **A**：设计文档既定"独立部署" |
| D3 | 登录方式 | A) 用户名密码先行 B) OIDC/SSO 先行 | **A**：最简可用，OIDC 走 R23.3 |
| D4 | 凭据归属 | A) 前台自持凭据表 B) 改后台 users 加 password_hash | **A**：后台零改动（红线） |
| D5 | 社区公开 | A) 加 `Visibility=public`（只增） B) 首版用 tenant 浏览 | **B**：先不动后台 Schema；public 后续确认 |
| D6 | 首版范围 | A) 登录+个人工作台 B) 直接含社区 | **A**：先做核心，社区 R23.2 |

> 确认后按此定稿实现。默认推荐项已标注；如无异议按推荐执行。

---

## 12. 决策定稿（2026-10-18 负责人确认）

| # | 决策项 | 定稿 |
|---|---|---|
| D1 | 技术栈 | Go + `html/template`（零依赖） |
| D2 | 部署形态 | 独立进程 `lightmem-frontend` |
| D3 | 登录方式 | 用户名密码先行；OIDC/SSO 走 R23.3 |
| D4 | 凭据归属 | 前台自持凭据表，后台零改动 |
| **D5** | **社区公开** | **加 `Visibility = "public"`（只增不改，已确认越红线边界）；后台放行跨租户读 public** |
| D6 | 首版范围 | 登录 + 个人工作台；社区/public 在 R23.2 |

### 12.1 D5 落地边界（红线复核）

加 `public` 属**只增不改**：`model.go` 新增一个 `Visibility` 常量值 + 读取时
放行 public，**不改动** private / shared_user / tenant / cross_tenant 的任何既有语义。

安全语义：public = **任何人可读**（跨租户），**只有 owner 可写/改/删**。
检索/详情接口需在"租户可见性过滤"外**额外放行 `visibility='public'`** 的记录，
但 `EnforcePrivateOwner`（写/改/删）**仍要求 owner**，不允许他人借 public 越权改删。

> 这是 R22 已交付代码的唯一后台增量；其余前台功能均不改后台。
> 落地时单独提交、跑 gate.sh，确保既有 4 值零回归。

---

## 13. R23.1 验收记录（2026-10-18，0.88）

端到端脚本 `scripts/accept_r23.py`（在 0.88 起真实 kernel + frontend，
模拟浏览器走注册 → 登录 → 工作台 → API → 登出全流程）。

**结果：16/16 PASS**，覆盖：

| 类别 | 项 |
|---|---|
| 启动 | kernel `/healthz`、frontend `/health` |
| 页面 | 首页 200；已登录 `/workspace` 200 含标题 |
| 鉴权 | 未登录 `/workspace` → `/login`；错密码登录 200 + 「用户名或密码错误」 |
| 注册 | `POST /register` → `/workspace`；登出 → `/`；登出后 `/workspace` → `/login` |
| 业务 | `save` 201 + `memory_id`；`recall` 200；`goals` 200 |
| 计划 | `goal` create 201 + id；`progress` 200；`state` 200 |
| 社区 | `GET /api/discover` 200（`/search?q=` 透传） |

### 13.1 前台代理的关键适配（不改后台）

后台 REST 端点在鉴权后仍要求若干显式字段，前台按登录用户补齐、不暴露给 UI：

| 后台要求 | 前台补齐 |
|---|---|
| `tenant_id`（save/recall/plan 校验存在性） | cookie 记 tenant，代理时自动注入 |
| `permission.owner`（writepolicy 强制） | 登录用户名 |
| `handleGoalCreate` 用 body 的 `user_id` 当 owner | `user_id` = 登录用户名 |
| `goal_detail.constraints` 非空、`kpi.target` 为 `int64` | UI 传非空 constraints + 数字 target |

### 13.2 红线复核

- 后台 `router*.go` / `model.go` / `storage.go` **零改动**（D5 public 留 R23.2）。
- `kernel/go.mod` **零变化**（PBKDF2 手写 stdlib，未引入 `x/crypto`）。
- `gate.sh`：PASS 13 / FAIL 0 / SKIP 1（PG 真机，环境未配）。
- `go vet` / `gofmt` / `-race` 全绿。
- 前台 `go test ./frontend/...` 全绿（PBKDF2 向量 + 凭据库 + 服务集成测试）。
