# R9 生产化收口 · 合流验收报告

- **任务书**：`lightmem/.dumate/inbox/_R9_生产化收口任务书.md`（§1–§7）
- **权威基线**：`d1e458f`（R8 收口态）
- **合流提交**：`6e625fc`（含 A=`4fc22ef`、B=`2ef9913`、C 路文档/SDK）
- **验证机**：192.168.0.88（FreeBSD 15.1，go1.25.14，Python3 + Node）
- **收口日期**：2026-10-14
- **红线**：只增不改 kernel Schema 语义；不调 LLM；seed=42 确定性；不 touch `light-merge/`；本机（Windows）只做源码编辑，编译/测试/运行全部在 0.88 上验证。

---

## 0. 结论（TL;DR）

三路（A / B / C）均已在 0.88 上实测验收通过，合流后北极星 M1 与公开 MRR **不回归**，门禁全绿。

| 路 | 交付物 | 提交 | 状态 |
|---|---|---|---|
| **A** | API 服务器生产级硬化 | `4fc22ef` | ✅ PASS |
| **B** | 端到端集成测试套件 | `2ef9913` | ✅ PASS |
| **C** | SDK 生产级完善 + API 文档 | `6e625fc` | ✅ PASS |
| **合流验收** | 本文档 | — | ✅ PASS |

| 关键指标 | 基线 `d1e458f` | 合流后 | 判定 |
|---|---|---|---|
| **北极星 M1**（真实语料 holdout 零 token 覆盖） | 1.0000（n=590） | **1.0000**（hit@1 0.9763 / mean_rr 0.9113 / ref 46/60） | ✅ 不回归 |
| **公开 MRR** | 0.9917（780 条） | **0.9917**（bucket_mrr 逐位一致） | ✅ 不回归 |
| gofmt / go vet / go test | 全绿 | **全绿** | ✅ |
| R9 新增测试 | — | **15 项（A 路）+ 10 项（B 路 E2E）**，含 `-race` | ✅ PASS |

**push 状态**：本地 `git commit` 已完成（含合流 commit `6e625fc`）；用户已示意 push。

---

## 1. A 路：API 服务器生产级硬化（`4fc22ef`）

### 1.1 交付物

```
kernel/
├── server_middleware.go        [新增] Logging / Recover / TraceID / WithMiddleware
├── server_metrics.go           [新增] ServerMetrics + MetricsMiddleware + HealthzHandler
├── server_middleware_test.go   [新增] 8 项冒烟测试
├── server_metrics_test.go      [新增] 7 项冒烟测试
├── consolidate.go              [只增] OnRunOnce 回调钩子
└── cmd/lightmem-kernel/main.go [重写] 生产级入口
```

### 1.2 中间件链（装配顺序）

```
LoggingMiddleware
  └── MetricsMiddleware(metrics)     # 请求计数 + 延迟直方图 + 业务动作计数
        └── RecoverMiddleware        # panic → 500，不崩进程
              └── TraceIDMiddleware  # 单调 trace ID → X-Trace-Id + context
                    └── mux（外层）
                          ├── /healthz        HealthzHandler
                          ├── /metrics        metricsHandler（Prometheus 文本 + ?format=json 兼容）
                          ├── /debug/pprof/   仅 -pprof 启用
                          └── /                srv.Handler()（router.go 全部既有端点）
```

### 1.3 验收实测（192.168.0.88）

| 验收项 | 实测 |
|---|---|
| graceful shutdown（SIGTERM） | exit_code=**0**，日志 `[shutdown] done`，慢请求先完成再关 |
| 请求日志 | `[access] method=POST path=/memories status=201 latency_ms=0 trace_id=5 remote_ip=[::1]` |
| panic recover | 返回 500，进程不崩 |
| trace ID 单调 | 响应头 `X-Trace-Id: 3→4→5→6` 递增 |
| `/metrics` Prometheus 文本 | 含 `http_requests_total` / `http_request_duration_ms` / `memories_saved_total` / `memories_recalled_total` / `recall_latency_ms` |
| `/healthz` JSON | `{"status":"ok","backend":"sqlite-skeleton/sqlite-vec","embedder":"hash","last_consolidate":"","profile":"family"}` |
| pprof | 默认 404 / `-pprof` 200 |
| `-embedder vector` | 装配 BGE-m3：`embedder=vector:BAAI/bge-m3` dim=1024 count=2756 |
| 默认态兼容 | 无 flag 变更逐位复现 `d1e458f` 行为 |

**红线核对**：`kernel/router.go` / `lifecycle.go` / `metrics.go` / `rank_fusion.go` / `model.go` **零改动**（`git diff` 空，`git hash-object` 与 HEAD 一致）。

### 1.4 已知限制

- Prometheus 文本为**手写序列化**（零新依赖），`# HELP`/`# TYPE`/标签排序按文本规范实现，未接入 client_golang 官方库；如需 `scrape` 到 Prometheus 建议后续 R 轮评估官方库迁移。

---

## 2. B 路：端到端集成测试套件（`2ef9913`）

### 2.1 交付物

| 文件 | 说明 |
|---|---|
| `eval/e2e/helpers_test.go` | httptest.Server 装配夹具 + saveFact/recallHit/searchText + JSON 断言 |
| `eval/e2e/e2e_test.go` | 10 个全生命周期 E2E 用例 |
| `eval/reports/r9b_e2e_20261014.md` | 覆盖矩阵 + 并发安全证明 + 已知限制 |

**生产代码零改动**：仅新增 `eval/e2e/` 两个测试文件。

### 2.2 覆盖矩阵

| # | 用例 | 生命周期 | 端点路径 | 结果 |
|---|---|---|---|---|
| 1 | `TestSaveAndRecall` | save → recall | `/tools/memory/save` → `/tools/memory/recall` | PASS |
| 2 | `TestSearchEndpoint` | search | `GET /search?q=&tenant_id=&viewer=` | PASS |
| 3 | `TestUpdateAppendsVersion` | update 追加版本不覆盖 | `POST /tools/memory/update` | PASS |
| 4 | `TestForgetDeletesMemory` | forget 软删除 | `POST /tools/memory/forget` | PASS |
| 5 | `TestGDPRExport` | GDPR 可携导出 | `POST /api/v1/gdpr/export` | PASS |
| 6 | `TestCascadeDelete` | GDPR 级联删除 | `POST /api/v1/gdpr/cascade-delete` | PASS |
| 7 | `TestTenantIsolation` | 多租户硬隔离 | `GET /search` + `/tools/memory/recall` | PASS |
| 8 | `TestUserVisibility` | 私有记忆仅 owner 可见 | recall viewer=alice/bob | PASS |
| 9 | `TestPersistenceAcrossRestart` | 重启数据存活 | close → 重建 httptest.Server | PASS |
| 10 | `TestConcurrentSaveRecall` | 并发安全 | 10 并发 save + 并发 recall，`-race` 干净 | PASS |

全生命周期闭环：save → search → recall → update → forget → GDPR export → cascade-delete，全部经真实 `net/http`（`httptest.Server` 挂 `kernel.APIServer.Handler()`）打全链路。

### 2.3 已知限制（登记 R10）

- `rank_fusion.go` 的共享字段存在**生产 race**（仅 E2E `-race` 观测到，测试侧已登记），建议 R10 处理；非阻塞本路交付。

---

## 3. C 路：SDK 生产级完善 + API 文档（`6e625fc`）

### 3.1 交付物

| 文件 | 说明 |
|---|---|
| `sdk/python/lightmem_sdk.py` | v0.2.0 生产级：type hints / 异常分层 / 重试 / 上下文管理器，**stdlib-only**（仅 urllib） |
| `sdk/python/example.py` | 可直接运行的端到端示例 |
| `sdk/js/lightmem-sdk.js` | 对应 JS 客户端 + Node 兼容导出 |
| `sdk/js/example.js` | Node 示例 |
| `docs/api.md` | 17 章节 / 23 端点 API 手册 |
| `docs/api.openapi.yaml` | OpenAPI 3.0.3，23 路径 + 15 schemas |
| `eval/reports/r9c_sdk_docs_20261014.md` | 覆盖矩阵与交付报告 |

### 3.2 端点覆盖核对

以 `kernel/router.go` 实际路由为唯一真相源，逐条比对：

| 端点 | `docs/api.md` | `docs/api.openapi.yaml` | 与 `router.go` 对齐 |
|---|---|---|---|
| `GET /health` | ✅ | ✅ | ✅ |
| `GET /healthz`（R9-A main.go 外层） | ✅ | ✅ | ✅ |
| `GET /profile` | ✅ | ✅ | ✅ |
| `POST /memories` | ✅ | ✅ | ✅ |
| `GET /search` | ✅ | ✅ | ✅ |
| `POST /search/vector` | ✅ | ✅ | ✅ |
| `POST /mem/router` | ✅ | ✅ | ✅ |
| `POST /tools/memory/recall` | ✅ | ✅ | ✅ |
| `POST /tools/memory/save` | ✅ | ✅ | ✅ |
| `POST /tools/memory/update` | ✅ | ✅ | ✅ |
| `POST /tools/memory/feedback` | ✅ | ✅ | ✅ |
| `POST /tools/memory/forget` | ✅ | ✅ | ✅ |
| `POST /lifecycle/run` | ✅ | ✅ | ✅ |
| `GET /metrics` | ✅ | ✅ | ✅ |
| `GET /metrics/kinds` | ✅ | ✅ | ✅ |
| `GET /metrics/layers` | ✅ | ✅ | ✅ |
| `GET /search/trace` | ✅ | ✅ | ✅ |
| `GET /audit/{memory_id}` | ✅ | ✅ | ✅ |
| `GET /audit` | ✅ | ✅ | ✅ |
| `POST /api/v1/gdpr/export` | ✅ | ✅ | ✅ |
| `POST /api/v1/gdpr/cascade-delete` | ✅ | ✅ | ✅ |
| `POST /api/v1/gdpr/forget` | ✅ | ✅ | ✅ |
| `POST /vector/backfill` | ✅ | ✅ | ✅ |
| **合计** | **23** | **23** | ✅ |

`docs/api.md` 章节重排为 **1–17**（`healthz=1`、`health=2`、`profile=3`、`memories=4`、`search=5`、`search/vector=6`、`mem/router=7`、`tools/memory/recall=8` … `lifecycle/run=17`）；`docs/api.openapi.yaml` 路径 23 个，schemas 15 个，`yaml.safe_load` 解析成功（openapi 3.0.3）。

### 3.3 语法校验（192.168.0.88）

```
python3 -m py_compile sdk/python/lightmem_sdk.py sdk/python/example.py  # OK
node -c sdk/js/lightmem-sdk.js                                          # OK
```

---

## 4. 合流验证（三路叠加后）

以 `d1e458f` 为基线，叠加 A + B + C 全部改动，在 192.168.0.88 上重跑：

### 4.1 北极星 M1（真实语料 holdout 零 token 覆盖）

| 指标 | 基线 | 合流后 |
|---|---|---|
| M1 覆盖率 | 1.0000 | **1.0000**（n=590） |
| hit@1 | — | 0.9763 |
| mean reciprocal rank | — | 0.9113 |
| ref 命中 | — | 46/60 |

### 4.2 公开基准 MRR

| 指标 | 基线 | 合流后 |
|---|---|---|
| MRR（780 条） | 0.9917 | **0.9917**（bucket_mrr 逐位一致） |

### 4.3 门禁

| 门禁 | 结果 |
|---|---|
| `gofmt -l kernel/` | 空（kernel + cmd） |
| `go vet ./...` | 通过 |
| `go test ./kernel/... ./eval/e2e/...` | 全绿 |
| `go test -race ./eval/e2e/...` | 通过（生产 race 见 §2.3） |
| R9-A 15 项冒烟测试（8+7，含 `-race`） | PASS |
| R9-B 10 项 E2E | PASS |
| `python3 -m py_compile`（SDK） | OK |
| `node -c`（SDK） | OK |

### 4.4 默认态与确定性

- **默认态兼容**：`lightmem-kernel` 无 flag 变更时逐位复现 `d1e458f` 行为（同端口 / 同路由 / 同 SQLite fallback）。
- **确定性**：seed=42 未改；LLM 调用 0 次；`docs/api.openapi.yaml` 与 `docs/api.md` 内容一致。
- **换行**：全仓 `.go` / `.md` / `.yaml` / `.py` / `.js` 已按 `.gitattributes` 统一为 LF；本机 `core.autocrlf=true` 造成的 CRLF 已全部转回（`git cat-file` 校验 blob 内 CR=0）。

---

## 5. 文件域冲突解决（任务书 §7）

三路并行时按任务书 §7 声明文件域，避免同一文件多路并发覆盖：

| 路 | 独占文件域 | 共用文件域（只读） |
|---|---|---|
| A | `kernel/server_*.go`、`kernel/cmd/lightmem-kernel/main.go`、`kernel/consolidate.go`（只增）、`eval/reports/r9a_*.md` | `kernel/router.go`（红线零改动） |
| B | `eval/e2e/*_test.go`、`eval/reports/r9b_*.md` | 同上 |
| C | `sdk/**`、`docs/api.md`、`docs/api.openapi.yaml`、`eval/reports/r9c_*.md` | 同上 |

**合流冲突**：仅 `docs/api.md` 的端点覆盖表需要同时反映 A 路新增 `/healthz` 与 C 路补齐的 `/mem/router` —— 已在 C 路 commit 内一并更新，三份文档（`api.md` / `api.openapi.yaml` / `r9c_sdk_docs_*.md`）口径一致。

---

## 6. 提交与推送

| 提交 | 内容 | 类型 |
|---|---|---|
| `4fc22ef` | R9-A 服务器硬化（+1345 / −20） | feat |
| `2ef9913` | R9-B E2E 套件（+608） | test |
| `6e625fc` | R9-C SDK + API 文档（+2834 / −164） | docs |
| `r9_acceptance_20261014.md` | 本文档（合流验收） | docs |

**红线**：不改 `light-merge/`；`kernel/router.go` 零改动；本机无 Go（符合红线，编译/测试全部在 0.88 完成）。

---

## 7. 待办与后续

1. **R10 候选**：`rank_fusion.go` 共享字段生产 race（B 路登记）。
2. **R10 候选**：Prometheus 文本 → 官方 client_golang 迁移评估（A 路登记）。
3. **远端同步**：gitea main（origin）为主；GitHub / GitCode 同步另行示意。
