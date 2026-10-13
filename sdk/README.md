# LightMem SDK & REST API

光明记忆（LightMem, LIMS）开放 API 与多语言 SDK。对应 kernel 的 HTTP API 骨架（§4.4）与 P7-A GDPR 合规端点（§7.3/§7.4/§7.5）。

> 当前为「骨架」实现：`sqlite` 后端为内存实现（重启即失），`pgvector` 后端为生产路径。所有端点均不调 LLM 参与检索排序/事实判断（R5 红线）。

## 服务启动

```sh
# 骨架：内存 SQLite 后端（跑通链路用）
lightmem-kernel -backend-override sqlite -listen :8080
```

## REST API 端点清单

| 方法 | 路径 | 说明 | 对应 § |
|---|---|---|---|
| POST | `/memories` | 创建 / 幂等写入一条记忆 | §4.4 |
| GET | `/search?q=&tenant_id=&viewer=&top_k=&kind=` | 文本检索 | §4.4 |
| POST | `/tools/memory/recall` | 跨层召回 + 多信号融合（A1+A2） | §3.1 / §3.4 |
| POST | `/tools/memory/update` | 编辑（追加新版本，不覆盖 fact） | §4.1 |
| GET | `/metrics` | Prometheus / JSON 指标 | §5.3 |
| GET | `/health` | 探活 | — |
| POST | `/api/v1/gdpr/export` | **数据可携导出**（§7.4） | P7-A |
| POST | `/api/v1/gdpr/cascade-delete` | **级联删除**（§7.3 删除权） | P7-A |
| POST | `/api/v1/gdpr/forget` | **被遗忘权**（§7.5 彻底移除 + 匿名化） | P7-A |

> 注：当前删除路径统一走 GDPR 端点（`/api/v1/gdpr/forget` 单条 / `cascade-delete` 作用域），而非 `DELETE /memories/:id`；`DELETE /user` 语义等价于 `cascade-delete`。`/export` 为 GDPR 导出端点，非通用 GET 列表。

### 请求 / 响应示例

**保存记忆（Create）**

```http
POST /memories
Content-Type: application/json

{
  "tenant_id": "demo-tenant",
  "user_id": "demo-user",
  "memory_kind": "fact",
  "fact": "用户 skywalk 偏好用中文交流",
  "visibility": "private",
  "confidence": 0.9
}
```

```json
{ "status": "created", "memory": { "memory_id": "000...", "fact": "用户 skywalk 偏好用中文交流", "layer": "L1" } }
```

**检索（Read）**

```http
GET /search?q=中文交流&tenant_id=demo-tenant&viewer=demo-user&top_k=10
```

```json
[ { "memory_id": "000...", "fact": "用户 skywalk 偏好用中文交流", "score": 0.9 } ]
```

**级联删除（§7.3）**

```http
POST /api/v1/gdpr/cascade-delete
{ "tenant_id": "demo-tenant", "user_id": "demo-user", "actor": "admin" }
```

```json
{ "tenant_id": "demo-tenant", "user_id": "demo-user", "deleted_memory_ids": ["000...","001..."], "deleted_count": 2 }
```

**数据可携导出（§7.4）**

```http
POST /api/v1/gdpr/export
{ "tenant_id": "demo-tenant", "user_id": "demo-user" }
```

```json
{ "tenant_id": "demo-tenant", "user_id": "demo-user", "exported": 2, "memories": [ ... ] }
```

**被遗忘权（§7.5）**

```http
POST /api/v1/gdpr/forget
{ "memory_id": "000...", "actor": "user" }
```

```json
{ "memory_id": "000...", "deleted": true, "trace_kept": false }
```

## SDK

| 语言 | 文件 | 依赖 | 运行 |
|---|---|---|---|
| Python | `python/lightmem_sdk.py` | 仅标准库 | `python3 example.py http://localhost:8080` |
| JavaScript | `js/lightmem-sdk.js` | Node 18+ 内置 fetch | `node example.js http://localhost:8080` |

两个 SDK 均提供 `save / search / recall / update / export_all / cascade_delete / forget` 方法，与以上端点一一对应。示例 `example.py` / `example.js` 演示完整 **CRUD + GDPR 闭环**（创建 → 检索 → 更新 → 导出 → 被遗忘）。

## 红线与约束

- 不调 LLM 做检索排序 / 事实判断（R5 红线）。
- `Memory` / `RouterContext` / `RecalledCandidate` 字段语义只增不改；GDPR 删除/导出通过可选的 `ComplianceBackend` 接口执行，不改动 `StorageBackend` 既有方法。
- 检索结果与排名不被热点缓存改写（P7-B `HotCache` 为只读旁挂，原样返回底层结果）。
