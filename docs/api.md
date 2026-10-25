# LightMem API 文档

> 版本：v0.2.1 ｜ 更新日期：2026-10-17 ｜ 对应 kernel 版本：R19-A 收口态
> 本文档与 `kernel/router.go` 路由表逐位对齐，不臆造端点。
> R19-A 变更：`GET /search` 参数口径统一（`q` 权威、`text` 兼容别名、空查询 400、无命中 `[]`）。

## 目录

- [概述](#概述)
- [认证与权限模型](#认证与权限模型)
- [端点总表](#端点总表)
- [详细端点说明](#详细端点说明)
- [错误码](#错误码)
- [SDK 示例](#sdk-示例)

---

## 概述

LightMem（光明记忆，LIMS）提供 RESTful HTTP API，覆盖记忆的**创建、检索、召回、更新、删除**全生命周期，以及 **GDPR 合规**端点。

### 基础信息

| 项目 | 值 |
|---|---|
| 协议 | HTTP / HTTPS |
| 数据格式 | JSON |
| 字符编码 | UTF-8 |
| 默认端口 | 8080 |
| 默认基地址 | `http://localhost:8080` |

### 启动服务

```sh
# 骨架模式（内存 SQLite，跑通链路用）
lightmem-kernel -backend-override sqlite -listen :8080

# 生产模式（pgvector 后端，需配置 profile）
lightmem-kernel -profile ../profiles/lims-family.profile.yaml
```

---

## 认证与权限模型

LightMem 采用**租户隔离 + 用户可见性**双层权限模型，不依赖外部认证框架。

### 核心字段

| 字段 | 类型 | 说明 |
|---|---|---|
| `tenant_id` | string | 租户 ID，硬隔离边界。不同租户的记忆完全不可见。 |
| `user_id` | string | 用户 ID，用于归属与 GDPR 操作。 |
| `viewer` | string | 可见性过滤的观察者 ID。仅返回该 viewer 有权限看到的记忆。 |
| `visibility` | string | 可见性级别（合法值见 `kernel/model.go`）：`private`（仅 owner，且须同租户）/ `shared_user`（owner + `readers`）/ `tenant`（同租户任意 viewer）/ `cross_tenant`（`shared_with_tenants` 内）/ `public`（任意带身份 viewer）。**零值不匹配任何一级 ⇒ 不可见**。 |
| `permission` | object | 细粒度权限对象，如 `{"owner": "u1", "readers": ["u2"]}`；**`owner` 必填**（缺失 422）。 |

### 权限规则

1. **租户硬隔离**：`tenant_id` 是第一道过滤，跨租户请求返回空结果。
2. **可见性过滤**：`viewer` 参数决定返回哪些记忆，基于 `visibility` 和 `permission` 计算。
3. **GDPR 操作**：`export_all` / `cascade_delete` 需同时提供 `tenant_id` 和 `user_id`。

---

## 端点总表

| 方法 | 路径 | 说明 | 对应章节 |
|---|---|---|---|
| `GET` | `/health` | 服务探活（router.go 骨架） | — |
| `GET` | `/healthz` | 生产级健康检查（R9-A，main.go 外层挂载） | §5.3 |
| `GET` | `/profile` | 获取当前 profile 配置 | — |
| `POST` | `/memories` | 创建 / 幂等写入一条记忆 | §4.4 |
| `GET` | `/search` | 文本检索（`q` 权威 / `text` 别名；空查询 400） | §4.4 |
| `POST` | `/search/vector` | 向量检索（需 embedder） | §4.2 |
| `POST` | `/mem/router` | 记忆路由 + 多信号融合（Agent 直连） | §3.1 / §3.4 |
| `POST` | `/tools/memory/recall` | 跨层召回 + 多信号融合 | §3.1 / §3.4 |
| `POST` | `/tools/memory/save` | Agent 工具接口：保存记忆 | §4.4 |
| `POST` | `/tools/memory/update` | Agent 工具接口：编辑（追加新版本） | §4.1 |
| `POST` | `/tools/memory/feedback` | Agent 工具接口：用户反馈 | — |
| `POST` | `/tools/memory/forget` | Agent 工具接口：单条遗忘 | §7.5 |
| `POST` | `/lifecycle/run` | 手动触发分层生命周期扫描 | §3.10 |
| `GET` | `/metrics` | Prometheus / JSON 指标 | §5.3 |
| `GET` | `/metrics/kinds` | 各类型记忆计数 | — |
| `GET` | `/metrics/layers` | 各层记忆计数 | — |
| `GET` | `/search/trace` | 检索 trace（P3） | §3.4 |
| `GET` | `/audit/{memory_id}` | 单条记忆审计日志 | §5.3 |
| `GET` | `/audit` | 审计日志列表 | §5.3 |
| `POST` | `/api/v1/gdpr/export` | 数据可携导出 | §7.4 |
| `POST` | `/api/v1/gdpr/cascade-delete` | 级联删除 | §7.3 |
| `POST` | `/api/v1/gdpr/forget` | 被遗忘权 | §7.5 |
| `POST` | `/vector/backfill` | 手动触发向量补全 | §4.2 |

---

## 详细端点说明

### 1. GET /healthz

生产级健康检查（R9-A 新增，`main.go` 外层 mux 挂载，非 `router.go` 骨架端点）。

- 检查后端连通性（`Ping`）；失败返回 503。
- 返回 embedder 名、最近一次后台整合（consolidate）时间戳。

**响应 200 OK**

```json
{
  "status": "ok",
  "profile": "family",
  "backend": "sqlite-skeleton/sqlite-vec",
  "embedder": "hash",
  "last_consolidate": "2026-10-14T04:54:16Z"
}
```

**状态码**

| 状态码 | 含义 |
|---|---|
| 200 | 服务正常 |
| 503 | 后端不可达（`status: degraded`） |

> 说明：`/health`（router.go）与 `/healthz`（main.go）并存；`/health` 为骨架期探活，`/healthz` 为生产级健康检查。

---

### 2. GET /health

服务探活，不检查后端连通性。

**响应**

```json
{
  "status": "ok",
  "profile": "lims-family",
  "backend": "sqlite"
}
```

**状态码**

| 状态码 | 含义 |
|---|---|
| 200 | 服务正常 |
| 503 | 后端不可达（`degraded`） |

---

### 3. GET /profile

返回当前加载的 profile 配置（JSON 格式）。

**响应**

```json
{
  "profile": "lims-family",
  "storage_backend": "pgvector",
  "tier": "family",
  "storage": {
    "embedding": { "dimensions": 1024, "model": "bge-m3" }
  }
}
```

---

### 4. POST /memories

创建 / 幂等写入一条记忆。

**请求体**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `tenant_id` | string | ✅ | 租户 ID |
| `user_id` | string | ✅ | 用户 ID |
| `memory_id` | string | ❌ | 记忆 ID（不填则自动生成） |
| `memory_kind` | string | ❌ | 类型：`event` / `fact` / `preference` / `procedure` / `goal`，默认 `fact` |
| `fact` | string | ✅ | 事实文本 |
| `visibility` | string | ❌ | 可见性，合法值：`private`（仅 owner 且同租户）/ `shared_user`（owner + `permission.readers`）/ `tenant`（同租户）/ `cross_tenant`（`shared_with_tenants` 内）/ `public`。**缺省 = 不可见**（`Memory.canView` 对零值没有匹配分支，直接 `return false`），生产调用必须显式给值；R27+ 起不再有隐式默认 |
| `confidence` | float | ❌ | 置信度 [0, 1]，默认 0.0 |
| `permission` | object | ❌ | 细粒度权限；其中 **`owner` 必填**（缺失返回 `422` `permission.owner required`），`readers` / `writers` 可选 |
| `layer` | string | ❌ | 目标层：`L0` / `L1` / `L2` / `L3`，默认自动 |
| `tags` | string[] | ❌ | 标签列表 |
| `metadata` | object | ❌ | 扩展元数据 |

**请求示例**

```http
POST /memories
Content-Type: application/json

{
  "tenant_id": "demo-tenant",
  "user_id": "demo-user",
  "memory_kind": "fact",
  "fact": "用户 skywalk 偏好用中文交流",
  "visibility": "private",
  "confidence": 0.9,
  "permission": { "owner": "demo-user" }
}
```

**响应 201 Created**

```json
{
  "status": "created",
  "memory": {
    "memory_id": "abc123...",
    "fact": "用户 skywalk 偏好用中文交流",
    "layer": "L1",
    "version": 1
  }
}
```

**响应 200 OK（幂等重复）**

```json
{
  "status": "idempotent_noop",
  "memory": { "memory_id": "abc123...", ... }
}
```

**状态码**

| 状态码 | 含义 |
|---|---|
| 201 | 创建成功 |
| 200 | 幂等重复（已存在） |
| 400 | 请求体 JSON 格式错误 |
| 422 | 业务校验失败（如缺少必填字段） |

---

### 5. GET /search

文本检索。

**查询参数**

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `q` | string | ✅ | 查询文本（权威参数） |
| `text` | string | ❌ | `q` 的历史别名；仅当 `q` 为空时生效（R19-A 兼容期，未定移除时间） |
| `tenant_id` | string | ✅ | 租户 ID |
| `viewer` | string | ❌ | 可见性过滤的观察者 ID |
| `top_k` | int | ❌ | 返回条数，默认 10 |
| `kind` | string | ❌ | 记忆类型过滤（逗号分隔，如 `fact,preference`） |

> **参数优先级**：`q` 优先，`q` 为空（含仅空白）时回落到 `text`；两者同时出现时 `text` 被忽略。
> **空查询语义**：`q` 与 `text` 均缺失或仅含空白 → `400 {"error":"q or text required"}`。
> 后端对空文本不做过滤（等价于列举作用域全部），故不提供隐式全量列表；导出请用 `POST /api/v1/gdpr/export`。

**请求示例**

```http
GET /search?q=中文交流&tenant_id=demo-tenant&viewer=demo-user&top_k=10
```

**响应 200 OK**

```json
[
  {
    "memory_id": "abc123...",
    "fact": "用户 skywalk 偏好用中文交流",
    "score": 0.92,
    "layer": "L1",
    "memory_kind": "fact"
  }
]
```

> 无命中返回空数组 `[]`（不是 `null`）。

**状态码**

| 状态码 | 含义 |
|---|---|
| 200 | 检索成功（含无命中的 `[]`） |
| 400 | 缺少 `tenant_id`，或 `q`/`text` 均为空 |
| 500 | 后端错误 |

---

### 6. POST /search/vector

向量检索（需配置 embedder）。

**请求体**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `query` | string | ❌* | 查询文本（与 `vector` 二选一） |
| `vector` | float[] | ❌* | 直接传入向量（与 `query` 二选一） |
| `tenant_id` | string | ✅ | 租户 ID |
| `viewer` | string | ❌ | 可见性过滤 |
| `top_k` | int | ❌ | 返回条数，默认 10 |

**响应 200 OK**

同 `/search`。

**响应 503 Service Unavailable**

```json
{
  "error": "embedding failed: ...",
  "downgrade": "use GET /search?q=..."
}
```

> 向量检索失败时返回 503 并提示降级到文本检索。

---

### 7. POST /mem/router

记忆路由（P2 Agent 直连）：对一次调用执行完整记忆路由链路——层选择 → 多信号打分 → 多信号融合（rank fusion），返回融合候选与 trace。适合 LLM Agent 直接把用户 query 交给内核取回最相关记忆。

**请求体**（字段对齐 `kernel.RouterContext`）

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `text` | string | ✅ | 查询文本 |
| `tenant_id` | string | ✅ | 租户 ID |
| `user_id` | string | ❌ | 用户 ID |
| `project_id` | string | ❌ | 项目坐标（参与参考系信号） |
| `agent_id` | string | ❌ | Agent 标识 |
| `scene` | string | ❌ | 场景坐标 |
| `task` | string | ❌ | 任务坐标 |
| `entity` | string | ❌ | 实体坐标 |
| `include_archived` | bool | ❌ | 是否包含归档记忆，默认 false |
| `top_k` | int | ❌ | 返回条数，默认 10 |
| `as_of` | string | ❌ | 回溯时间（`YYYY-MM-DD`） |
| `memory_kinds` | string[] | ❌ | 限定记忆类型 |
| `visibility` | string[] | ❌ | 限定可见性 |
| `compose_expand` | bool | ❌ | 组合记忆子事实展开（R6-B），默认 false |

**请求示例**

```http
POST /mem/router
Content-Type: application/json

{
  "text": "客户A偏好哪一天开会",
  "tenant_id": "demo-tenant",
  "user_id": "demo-user",
  "top_k": 5
}
```

**响应 200 OK**

```json
{
  "intent": { "kind": "recall", "priority": "normal" },
  "recalled": [
    {
      "layer": "L1",
      "candidates": [ { "memory_id": "abc123", "fact": "客户A偏好周一开会", "score": 0.92 } ],
      "count": 3
    }
  ],
  "fusion_scores": null,
  "fused": [
    {
      "memory_id": "abc123",
      "fact": "客户A偏好周一开会",
      "score": 0.95,
      "layer": "L1",
      "signals": { "text": 0.92, "temporal": 0.88, "graph": 0.0 }
    }
  ],
  "mode": "default",
  "trace": { "query": "客户A偏好哪一天开会", "ranked": [] }
}
```

**状态码**

| 状态码 | 含义 |
|---|---|
| 200 | 路由成功 |
| 400 | `tenant_id` 缺失 / JSON 解析失败 |
| 503 | 检索后端不可用 |

---

### 8. POST /tools/memory/recall

跨层召回 + 多信号融合（Agent 工具接口）。

**请求体**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `text` | string | ✅ | 查询文本 |
| `tenant_id` | string | ✅ | 租户 ID |
| `user_id` | string | ❌ | 用户 ID |
| `viewer` | string | ❌ | 可见性过滤 |
| `top_k` | int | ❌ | 返回条数，默认 10 |

**响应 200 OK**

```json
{
  "fused": [
    {
      "memory_id": "abc123...",
      "fact": "用户 skywalk 偏好用中文交流",
      "score": 0.95,
      "layer": "L1",
      "signals": { "text": 0.92, "temporal": 0.88, "graph": 0.0 }
    }
  ],
  "trace": { ... }
}
```

---

### 9. POST /tools/memory/update

编辑（追加新版本，不覆盖原 fact）。

**请求体**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `memory_id` | string | ✅ | 要更新的记忆 ID |
| `fact` | string | ✅ | 新事实文本 |
| `tenant_id` | string | ✅ | 租户 ID |
| `viewer` | string | ❌ | 可见性过滤 |
| `user_id` | string | ❌ | 用户 ID |
| `confidence` | float | ❌ | 置信度 |

**响应 200 OK**

```json
{
  "memory_id": "abc123...",
  "version": 2,
  "fact": "用户 skywalk 偏好用中文与英文混合交流"
}
```

> 更新会追加新版本，不覆盖旧版本。旧版本仍可通过审计日志查询。

---

### 10. POST /api/v1/gdpr/export

数据可携导出（§7.4）。返回该 `(tenant, user)` 作用域全部记忆。

**请求体**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `tenant_id` | string | ✅ | 租户 ID |
| `user_id` | string | ✅ | 用户 ID |

**响应 200 OK**

```json
{
  "tenant_id": "demo-tenant",
  "user_id": "demo-user",
  "exported": 5,
  "memories": [
    { "memory_id": "abc123...", "fact": "...", ... }
  ]
}
```

---

### 11. POST /api/v1/gdpr/cascade-delete

级联删除（§7.3 删除权）。删除该 `(tenant, user)` 作用域全部记忆。

**请求体**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `tenant_id` | string | ✅ | 租户 ID |
| `user_id` | string | ✅ | 用户 ID |
| `actor` | string | ❌ | 操作者标识，默认 `sdk` |

**响应 200 OK**

```json
{
  "tenant_id": "demo-tenant",
  "user_id": "demo-user",
  "deleted_memory_ids": ["abc123...", "def456..."],
  "deleted_count": 2
}
```

---

### 12. POST /api/v1/gdpr/forget

被遗忘权（§7.5）。彻底移除单条记忆 + 匿名化（不保留 trace）。

**请求体**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `memory_id` | string | ✅ | 要删除的记忆 ID |
| `actor` | string | ❌ | 操作者标识，默认 `sdk` |

**响应 200 OK**

```json
{
  "memory_id": "abc123...",
  "deleted": true,
  "trace_kept": false
}
```

---

### 13. GET /metrics

Prometheus / JSON 指标。

**查询参数**

| 参数 | 类型 | 说明 |
|---|---|---|
| `format` | string | `prometheus` 返回 Prometheus 文本格式，默认 JSON |

**JSON 响应**

```json
{
  "lims_recall_latency_ms": 12.5,
  "lims_hit_rate": 0.976,
  "lims_pollution_rate": 0.02,
  "lims_layer_counts": { "L0": 200, "L1": 10000, "L2": 500, "L3": 50 },
  "lims_total_memories": 10750,
  "lims_total_queries": 12345
}
```

**Prometheus 文本响应**

```
# HELP http_requests_total Total HTTP requests
# TYPE http_requests_total counter
http_requests_total{method="GET",path="/search",status="200"} 12345
# HELP http_request_duration_ms HTTP request duration
# TYPE http_request_duration_ms histogram
http_request_duration_ms_bucket{le="10"} 10000
http_request_duration_ms_bucket{le="100"} 12000
```

---

### 14. GET /search/trace

返回最近一次检索的 trace（P3 retrieval_trace）。

**查询参数**

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `text` | string | ✅ | 查询文本 |
| `tenant_id` | string | ❌ | 租户 ID |
| `user_id` | string | ❌ | 用户 ID |

**响应 200 OK**

```json
{
  "trace": {
    "query": "中文交流",
    "stages": [
      { "stage": "text_search", "duration_ms": 5, "candidates": 10 },
      { "stage": "rank_fusion", "duration_ms": 2, "ranked": 10 }
    ]
  }
}
```

---

### 15. GET /audit/{memory_id}

返回指定记忆的操作历史。

**路径参数**

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `memory_id` | string | ✅ | 记忆 ID |

**响应 200 OK**

```json
{
  "memory_id": "abc123...",
  "rejected": false,
  "audit_log": [
    { "action": "create", "ts": "2026-10-14T10:00:00Z", "actor": "sdk" },
    { "action": "update", "ts": "2026-10-14T10:05:00Z", "actor": "sdk" }
  ],
  "audit_count": 2
}
```

---

### 16. GET /audit

审计日志列表。

**查询参数**

| 参数 | 类型 | 说明 |
|---|---|---|
| `tenant_id` | string | 按租户过滤（不填则返回全部） |
| `limit` | int | 返回条数，默认 100，最大 1000 |

---

### 17. POST /lifecycle/run

手动触发一次分层生命周期扫描。

**请求体**

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `tenant_id` | string | ✅ | 租户 ID |
| `max` | int | ❌ | 最大处理条数，默认不限 |

**响应 200 OK**

```json
{
  "scanned": 100,
  "promoted": 10,
  "demoted": 5,
  "expired": 0
}
```

---

## 错误码

| 状态码 | 含义 | 场景 |
|---|---|---|
| 400 | Bad Request | JSON 格式错误、缺少必填参数 |
| 404 | Not Found | 记忆 ID 不存在 |
| 422 | Unprocessable Entity | 业务校验失败（如 tenant_id 格式错误） |
| 500 | Internal Server Error | 后端内部错误 |
| 503 | Service Unavailable | 后端不可达、embedder 未配置、向量检索失败 |

**错误响应格式**

```json
{
  "error": "错误描述信息"
}
```

---

## SDK 示例

### Python

```python
from lightmem_sdk import LightMemClient

with LightMemClient() as c:
    code, resp = c.save({
        "tenant_id": "t1", "user_id": "u1",
        "fact": "用户偏好中文交流"
    })
    print(code, resp)
```

### JavaScript

```javascript
import { LightMemClient } from "./lightmem-sdk.js";

const c = new LightMemClient();
const { status, body } = await c.save({
  tenant_id: "t1", user_id: "u1", fact: "用户偏好中文交流"
});
console.log(status, body);
```

完整示例见：
- `sdk/python/example.py`
- `sdk/js/example.js`
