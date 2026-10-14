# R9-A 交付报告：API 服务器生产级硬化

> **版本**：v1 ｜ 日期：2026-10-14 ｜ 任务编号：R9-A（`lightmem/.dumate/inbox/_R9_生产化收口任务书.md` §1）
> **基线**：`d1e458f`（R8 收口态）｜ **验证机**：192.168.0.88（FreeBSD，go1.25.14）
> **红线**：只增不改 kernel Schema 语义；不调 LLM；seed=42 确定性；不 touch `light-merge/`；不 push，等用户示意。

---

## 0. 结论（TL;DR）

| 验收项 | 目标 | 实测 | 判定 |
|---|---|---|---|
| **graceful shutdown** | SIGTERM 退出码 0，在途请求完成，consolidate drain | 实测 exit_code=**0**，日志 `[shutdown] done` | ✅ PASS |
| 请求日志中间件 | 每请求 method/path/status/latency/trace_id | `[access] method=POST path=/memories status=201 latency_ms=0 trace_id=5 remote_ip=[::1]` | ✅ PASS |
| panic recover | panic 返回 500 不崩进程 | `TestRecoverMiddleware_PanicReturns500` / `TestFullChain_Smoke` 通过 | ✅ PASS |
| trace ID | 响应头单调递增 + 注入日志 | `TestTraceIDMonotonic` + 冒烟 `X-Trace-Id: 3→4→5→6` | ✅ PASS |
| `/metrics` | Prometheus 文本，含核心指标 | 含 `http_requests_total` / `http_request_duration_ms` / `memories_saved_total` / `memories_recalled_total` / `recall_latency_ms` | ✅ PASS |
| 健康检查升级 | `/healthz` JSON 含 backend/embedder/consolidate | `{"status":"ok","backend":"sqlite-skeleton/sqlite-vec","embedder":"hash","last_consolidate":"","profile":"family"}` | ✅ PASS |
| pprof | `-pprof` 启用可访问，默认关闭 | 默认 404 / 启用 200 | ✅ PASS |
| 真实 embedder 接线 | `-embedder vector` 装配 BGE-m3 | `embedder=vector:BAAI/bge-m3` dim=1024 count=2756 | ✅ PASS |
| 默认态兼容 | 无 flag 变更逐位兼容骨架 | 同端口/同路由/同 SQLite fallback，`/memories`、`/search` 正常 | ✅ PASS |
| **北极星 M1 不回归** | M1 = 1.0000 | **1.0000**（hit@1 0.9763 / mean_rr 0.9113 / ref 46/60，n=590） | ✅ PASS |
| **公开 MRR 不回归** | ≥ 0.9917 | **0.9917**（780 条，bucket_mrr 逐位一致） | ✅ PASS |
| gofmt / go vet / go test | 全绿 | gofmt kernel 空、vet 通过、kernel+i18n test 通过 | ✅ PASS |

**未 push**：改动仅 `git commit`（不 push），等待用户示意。

---

## 1. 设计

### 1.1 结构

```
kernel/
├── server_middleware.go        [新增] Logging / Recover / TraceID / WithMiddleware
├── server_metrics.go           [新增] ServerMetrics + MetricsMiddleware + HealthzHandler
├── server_middleware_test.go   [新增] 中间件冒烟测试（8 项）
├── server_metrics_test.go      [新增] 指标/健康检查测试（7 项）
├── consolidate.go              [只增] OnRunOnce 回调钩子
└── cmd/lightmem-kernel/main.go [重写] 生产级入口
```

### 1.2 中间件链（装配顺序）

```
LoggingMiddleware
  └── MetricsMiddleware(metrics)     # 请求计数 + 延迟直方图 + 业务动作计数
        └── RecoverMiddleware        # panic → 500，不崩进程
              └── TraceIDMiddleware  # 单调 trace ID → X-Trace-Id 响应头 + context
                    └── mux（外层）
                          ├── /healthz        HealthzHandler
                          ├── /metrics        metricsHandler（Prometheus 文本 + ?format=json 兼容）
                          ├── /debug/pprof/   仅 -pprof 启用
                          └── /                srv.Handler()（router.go 全部既有端点）
```

- 外层 mux 用 `mux.Handle("/", srv.Handler())` 承接业务路由，**不修改 `kernel/router.go` 一行**（§6 文件域红线）。
- `/metrics` 由 main.go 外层接管，避免与 router.go `handleMetrics` 路由冲突，同时保留 `?format=json` 兼容旧消费者。

### 1.3 指标清单（手写 Prometheus 文本，零新依赖）

| 指标 | 类型 | 说明 |
|---|---|---|
| `http_requests_total{method,path,status}` | counter | 请求总数，标签切分 |
| `http_request_duration_ms` | histogram | 请求延迟（桶 1/5/10/25/50/100/250/500/1000/5000 ms） |
| `memories_saved_total` | counter | 成功写入次数（2xx 且 path=/memories 或 /tools/memory/save） |
| `memories_recalled_total` | counter | 成功检索次数（2xx 且 path=/search 或 /tools/memory/recall 或 /search/vector） |
| `recall_latency_ms` | histogram | 检索延迟 |

- `/metrics` 默认输出 **R9-A 指标 + 既有 `kernel.MetricsCollector.PrometheusText()`（`lims_*` 系列）拼接**，二者互补互不覆盖（只增不改）。
- 4xx 请求**不计入**业务动作计数（仅 2xx 计数）。

### 1.4 确定性

- trace ID：进程级 `atomic.Uint64` 单调计数器（从 1 开始），**不用 UUID/随机源/time.Now()**。
- 指标输出：标签按 key **字典序**排序输出，无 map 迭代序。
- 直方图桶边界固定；`ObserveHTTP` 只在 2xx 记录业务动作。
- `HealthzHandler` 中 `last_consolidate` 由 `Consolidator.SetOnRunOnce` 回调注入（`RunOnce` 完成后记录 `rep.RanAt` UTC），无随机源。

---

## 2. flag 扩充（默认态兼容）

| flag | 默认 | 说明 |
|---|---|---|
| `-embedder` | `hash` | `hash`（零依赖占位，维度取 profile `storage.embedding.dimensions`）/ `vector`（BGE-m3 静态向量） |
| `-vector-matrix` | `eval/vectors/vectors.npy` | 向量矩阵路径 |
| `-vector-meta` | `eval/vectors/vectors_meta.json` | 元数据路径 |
| `-vector-index` | `eval/vectors/vectors_index.jsonl` | 文本索引路径 |
| `-pprof` | `false` | 启用 `/debug/pprof/` 端点 |
| `-metrics` | `true` | 挂载 `/metrics` |
| `-shutdown-timeout` | `10s` | 优雅关闭超时 |

**默认态（无 flag 变更）**：`-profile` 默认值、`-listen :8080`、`-backend-override` 行为、SQLite fallback 与原骨架逐位一致。

---

## 3. 冒烟实测（0.88）

### 3.1 启动 + 健康检查

```sh
cd /tmp/r9a/lightmem
/tmp/lightmem-kernel -profile profiles/lims-family.profile.yaml -backend-override sqlite -listen :18080 &
curl -s http://localhost:18080/healthz | python3 -m json.tool
# {"backend":"sqlite-skeleton/sqlite-vec","embedder":"hash","last_consolidate":"","profile":"family","status":"ok"}
```

### 3.2 写入 + 检索（路由兼容）

```sh
curl -s -D /tmp/h1 -X POST http://localhost:18080/memories \
  -H 'Content-Type: application/json' \
  -d '{"memory_id":"m001","fact":"客户A偏好周一开会","tenant_id":"t1","user_id":"u1","visibility":"private","permission":{"owner":"u1"}}'
# HTTP/1.1 201 Created ; X-Trace-Id: 5
# {"memory_id":"m001",...,"fact":"客户A偏好周一开会",...}

curl -s 'http://localhost:18080/search?q=客户A&tenant_id=t1&viewer=u1'
# [{"memory_id":"m001","fact":"客户A偏好周一开会","score":0.7,...}] ; X-Trace-Id: 6
```

### 3.3 优雅关闭

```sh
kill -TERM $PID; wait $PID; echo $?   # exit_code=0
# [shutdown] received signal terminated, graceful draining (timeout=10s)
# [shutdown] cancel consolidate worker
# [shutdown] closing backend sqlite-skeleton/sqlite-vec
# [consolidate] worker stopped: context canceled
# [shutdown] done
```

### 3.4 pprof

```sh
# 默认：/debug/pprof/ → 404
/tmp/lightmem-kernel -profile ... -backend-override sqlite -listen :18082
curl -o /dev/null -w '%{http_code}' http://localhost:18082/debug/pprof/   # 404

# -pprof：→ 200
/tmp/lightmem-kernel -profile ... -backend-override sqlite -listen :18083 -pprof
curl -o /dev/null -w '%{http_code}' http://localhost:18083/debug/pprof/   # 200
```

### 3.5 真实 embedder 接线

```sh
/tmp/lightmem-kernel -profile profiles/lims-family.profile.yaml -backend-override sqlite \
  -listen :18085 -embedder vector
# [boot] vector embedder loaded: vector:BAAI/bge-m3 dim=1024 count=2756
curl -s http://localhost:18085/healthz | python3 -m json.tool
# {"backend":"sqlite-skeleton/sqlite-vec","embedder":"vector:BAAI/bge-m3",...}
```

> 注：`/search/vector` 在 sqlite 骨架后端返回 503 并提示降级（`vector search not available on sqlite-skeleton/sqlite-vec; downgrade to text`），此为**既有后端契约**（router.go §4.2 降级），非 A 路改动。

---

## 4. 不回归证明

### 4.1 北极星 M1（`polaris_voting.go`，默认态逐位复现 `d1e458f`）

```sh
go run eval/polaris/polaris_voting.go eval/polaris/compress_light.go \
  eval/polaris/polaris_m1.go eval/polaris/polaris_token.go \
  eval/polaris/polaris_fact.go eval/polaris/polaris_fp.go \
  -fusion rrf -ref-activate=true -weights eval/weights/rank_weights_r6_ref.json \
  -now-fixed 2026-10-13 -topk 5
```

| 指标 | 本次复跑 | d1e458f 基线 | 一致性 |
|---|---|---|---|
| n | 590 | 590 | ✅ |
| **M1（coverage）** | **1.0000** | 1.0000 | ✅ 逐位一致 |
| hit@1 | 0.9763 | 0.9763 | ✅ |
| mean_rr | 0.9113 | 0.9113 | ✅ |
| ref 桶 | 46/60 | 46/60 | ✅ |
| single/multi/open/temporal 桶 coverage | 1.0000 | 1.0000 | ✅ |

### 4.2 公开基准 MRR（`run_kpi.go`，R5 最终态配置）

```sh
cd eval && go run ./run_kpi.go -dataset ./benchmarks/dataset/all.jsonl \
  -temporal-samples dataset/temporal.jsonl -weights weights/rank_weights_en_r5.json \
  -embedder vector -now-fixed 2026-10-13 -report r9a_bench_final
```

| 指标 | 本次复跑 | 基线 | 一致性 |
|---|---|---|---|
| 评测集总数 | 780 | 780 | ✅ |
| **总体 MRR** | **0.9917** | 0.9917 | ✅ 逐位一致 |
| 分桶 MRR | single 0.9943 / multi 0.9800 / open 0.9788 / temporal 0.9983 | 同左 | ✅ |
| fact acc（v4） | 0.9607（660/687） | 0.9607 | ✅ |
| memory_id top-1 | 1.000 | 1.000 | ✅ |
| 污染率 / FP | 0% / 0 | 0% / 0 | ✅ |

**结论**：A 路只动服务器外壳（中间件 / 指标 / 入口），`rank_fusion` / `router_mem` / `voting` 检索链路零改动，北极星 M1 与公开 MRR 均逐位复现 `d1e458f`。

---

## 5. 门禁

| 检查 | 命令 | 结果 |
|---|---|---|
| gofmt（A 路文件） | `gofmt -l kernel/server_middleware.go kernel/server_metrics.go kernel/server_middleware_test.go kernel/server_metrics_test.go kernel/consolidate.go kernel/cmd/lightmem-kernel/main.go` | 空（clean） |
| gofmt（kernel/） | `gofmt -l kernel/` | 空（clean） |
| go vet | `go vet ./...`（kernel 模块） | ✅ OK |
| go test | `cd kernel && go test ./... -count=1` | kernel OK / i18n OK / cmd no test files |
| go test（R9-A 15 项） | `go test . -run 'R9\|TraceID\|Recover\|Logging\|WithMiddleware\|FullChain\|Middleware_Concurrent\|ServerMetrics\|MetricsMiddleware\|Healthz\|LastConsolidate\|BackendDown\|EmptyOutput\|AfterRequests\|4xx\|DeterministicOutput' -v -count=1` | **15 项全部 PASS** |
| race detector | 同上 + `-race` | ✅ 无数据竞争 |
| 编译 | `cd kernel/cmd/lightmem-kernel && go build -o /tmp/lightmem-kernel .` | ✅ 16.1MB 二进制 |

---

## 6. 文件清单

| 文件 | 类型 | 说明 | diff 摘要 |
|---|---|---|---|
| `kernel/server_middleware.go` | 新增 | Logging / Recover / TraceID / WithMiddleware | 167 行 |
| `kernel/server_metrics.go` | 新增 | ServerMetrics + MetricsMiddleware + HealthzHandler | 311 行 |
| `kernel/server_middleware_test.go` | 新增 | 8 项冒烟测试 | 217 行 |
| `kernel/server_metrics_test.go` | 新增 | 7 项指标/健康检查测试 | 215 行 |
| `kernel/cmd/lightmem-kernel/main.go` | 重写 | 生产级入口（graceful shutdown + flag + 中间件链） | 234 行 |
| `kernel/consolidate.go` | 只增 | `OnRunOnce` 回调钩子（+13 行，纯增量） | +13 −0 |
| `eval/reports/r9a_server_hardening_20261014.md` | 新增 | 本报告 | — |

**零改动**（`git diff HEAD` 为空，hash-object 与 HEAD 一致）：`kernel/lifecycle.go`、`kernel/metrics.go`、`kernel/rank_fusion.go`、`kernel/router.go`、`kernel/embedder.go`、`kernel/embedder_vector.go`、`kernel/model.go`。

**不触**：`light-merge/`、`kernel/router.go`（只读）、`eval/weights/rank_weights_r6_ref.json`（只读）、`eval/` 评测口径（`matchFactV4`/`JudgeM1`/MRR 判定零改动）。

---

## 7. 已知限制与降级登记

| 项 | 状态 | 说明 |
|---|---|---|
| `-embedder vector` 默认关闭 | ✅ 按任务书 §1.A.2 | 保持零依赖启动；显式 `-embedder vector` 才装配 BGE-m3 |
| `/search/vector` 在 sqlite 骨架后端 | 既有契约（503 + 降级提示） | router.go §4.2 降级逻辑，A 路未改 |
| `last_consolidate` 空值 | 首次 consolidate 前为 `""` | 语义：尚未执行后台整合 |
| `docs/api.md` 中 `/healthz` 升级、`/metrics` 新字段 | ⏳ 待 C 路收口后补充 | 任务书 §6 明确「不阻塞 C 路开工」，A 路不触 `docs/` |
| 真实 PG 端到端验证 | ⏳ R10 候选 | 任务书 §9，不在 A 路范围 |
| 真实语料 holdout | ⏳ 外部依赖 | `eval/dataset/holdout.jsonl` 未到位，使用 `holdout_proxy.jsonl`（590 条，R7-A 权威代理语料） |

---

## 8. 收口要求

- ✅ 已完成 `git commit`（不 push），汇报 commit hash + 指标。
- ✅ 北极星 M1 **1.0000**、公开 MRR **0.9917** 均不回归。
- ✅ 门禁 gofmt / vet / test 全绿。
- ⏸ 等待用户示意后才 push 到 gitea main（GitHub / GitCode 远端若需同步另行示意）。
