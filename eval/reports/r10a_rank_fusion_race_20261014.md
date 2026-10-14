# R10-A RankFusion 并发安全加固收口报告

- **任务书**：`lightmem/.dumate/inbox/_R10_生产可靠性闭环并行任务书.md` §1（A 路）
- **权威基线**：`c9ca837`（R9 收口态，已 push gitea main）
- **运行环境**：192.168.0.88（FreeBSD 15.1-RELEASE-p1，go1.25.14），干净工作树 `/tmp/r10a/lightmem`（本地 `c9ca837` 源码经 SFTP 同步，含 BGE-m3 向量文件 `eval/vectors/vectors.npy` + `vectors_index.jsonl` + `vectors_meta.json`）
- **收口日期**：2026-10-14
- **并行声明**：本路仅改动 `kernel/rank_fusion.go`、`kernel/router_mem.go`、`kernel/router.go`、`kernel/router_tools.go` + 新增两个测试文件，文件域与 B 路（`eval/e2e/pg/` + `eval/scripts/`）、C 路（`kernel/server_metrics.go` + `kernel/server_middleware.go`）不重叠。

---

## 一、交付物

| 文件 | 类型 | 说明 |
|---|---|---|
| `kernel/rank_fusion.go` | 只增 | 新增 `ForRequest` 请求级实例构造（配置继承、请求状态独立、信号闭包重绑定） |
| `kernel/router_mem.go` | 只增/局部改 | `RecallAndFuse` 不再并发写共享 `rf`，改走 `ForRequest`；rejectedIDs 读改走 `isRejectedID` |
| `kernel/router.go` | 只增 | `APIServer` 新增 `sync.RWMutex`（`rejectedIDsMu`）+ 3 个辅助方法 |
| `kernel/router_tools.go` | 只增 | `s.rejectedIDs = append(...)` 改走 `s.addRejectedID(...)` |
| `kernel/rank_fusion_race_test.go` | 新增 | 3 个并发回归用例（共享实例 / 多租户 as_of / rejectedIDs） |
| `kernel/rank_fusion_bench_test.go` | 新增 | `BenchmarkRecallAndFuse` + `BenchmarkForRequest` + `BenchmarkRecallAndFuse_FixedCandidates` |
| `eval/reports/r10a_rank_fusion_race_20261014.md` | 本报告 | 根因 + 方案 + 不回归证明 + 性能对比 |

**红线遵守**：未改动 `Fuse` / `rrfRank` / `consensusVoteRank` 融合算法；未改动 `AbsoluteFloor` / `ProximityRatio` / `HalfLife` / `RRFK` 语义；未改动 `Memory` / `RouterContext` / `RecalledCandidate` 既有字段；未改动 `rank_weights_r6_ref.json` / `rank_weights_en_r5.json` 权重值；未引入新第三方依赖（`sync` 为标准库）。

---

## 二、根因复现（修复前 `-race` 报告）

### 2.1 根因定位

R9-B E2E 在 `-race` 下观测到生产 race，复核于 `c9ca837`，精确位置为 `kernel/router_mem.go` `RecallAndFuse` 内（L249-260 附近）：

```go
rf := s.fusion
if rf == nil {
    rf = DefaultRankFusion()
}
if rf.Now.IsZero() {
    rf.Now = time.Now().UTC()   // 写（共享实例）
}
rf.AsOf = rc.AsOf               // 写（共享实例）
rf.QueryFrame = rc.frame()      // 写（共享实例）
fused := rf.Fuse(ctx, rc.Text, flat)
```

**触发条件**：生产 `main.go` 调 `srv.SetRankFusion(DefaultRankFusion())` 后，多个并发请求**共享同一个** `*RankFusion` 实例，每个请求直接写 `Now` / `AsOf` / `QueryFrame` 字段，无 mutex。

**旁证**：`DefaultRankFusion()` 内部以方法值形式把信号闭包绑定到 `rf` 指针（`rf.signalFreshness` / `rf.signalReferenceFrame` / `rf.signalFeedbackHistory`），这些信号函数直接读 `rf.Now` / `rf.QueryFrame`，故写入方与信号读取方也存在跨 goroutine 竞争。

**同类隐患**：`APIServer.rejectedIDs`（`router.go` L23）是 `[]string` 切片，`router_tools.go` 中 append，无锁。

### 2.2 复现证据（修复前实测）

在 0.88 `/tmp/r10a/lightmem` 上，将 `router_mem.go` 回退到修复前版本，运行新增并发回归测试：

```sh
go test -race -run 'TestConcurrent' -count=1 ./kernel/
```

结果：**59 处 `WARNING: DATA RACE` 报告，5 处 FAIL**（`/tmp/r10a_race_report.txt`，2471 行）。关键竞争点：

| # | 位置 | 操作 | 说明 |
|---|---|---|---|
| 1 | `router_mem.go:251` | Write `rf.Now` | 并发请求同时写共享实例 `Now` |
| 2 | `router_mem.go:252` | Write `rf.AsOf` | 并发请求同时写共享实例 `AsOf` |
| 3 | `router_mem.go:253` | Write `rf.QueryFrame` | 并发请求同时写共享实例 `QueryFrame` |
| 4 | `rank_fusion.go:896` | Read `rf.QueryFrame` | 信号闭包 `signalReferenceFrame` 读共享实例 |

前三处为「写-写」竞争，第四处为「写-读」竞争（信号闭包读取被并发写入的字段），完整印证任务书 §1.1 的根因描述。

---

## 三、修复方案选型：请求级实例（`ForRequest`）

### 3.1 选型结论

任务书 §1.5 判定说明：「若最终方案无法同时满足『无锁且输出逐位一致』，则选加锁方案；两者皆可行时优先请求级实例（无锁更清晰，避免信号闭包读锁的开销）」。

**选型：请求级实例（无锁）**，理由：
1. 消除数据竞争同时**不引入任何锁**，避免信号闭包在热路径读锁的开销；
2. 输出逐位不回归：`ForRequest` 只复制配置 + 重绑定信号闭包到副本，`Fuse` / `rrfRank` / `consensusVoteRank` 算法原样沿用；
3. 配置（权重/常量/embedder）天然只读，可安全共享；只有 `Now` / `AsOf` / `QueryFrame` 三个请求级字段需要独立。

### 3.2 `ForRequest` 实现要点

```go
func (rf *RankFusion) ForRequest(now time.Time, asOf *string, qf *ReferenceFrame) *RankFusion {
    cp := &RankFusion{
        Weights: rf.Weights,           // 只读共享
        AbsoluteFloor: rf.AbsoluteFloor,
        ProximityRatio: rf.ProximityRatio,
        HalfLife: rf.HalfLife,
        RRFK: rf.RRFK,
        Now: now, AsOf: asOf, QueryFrame: qf,   // 请求级独立
        FusionMode: rf.FusionMode,
        VoteTopK: rf.VoteTopK,
        SemanticRedundant: rf.SemanticRedundant,
        SemanticAblatePath: rf.SemanticAblatePath,
        embedder: rf.embedder,         // 只读共享
    }
    // 关键：重绑定信号闭包到副本 cp
    for i, s := range rf.Signals {
        cp.Signals[i] = NamedSignal{Name: s.Name, Weight: s.Weight, Type: s.Type}
        switch s.Name {
        case "semantic":        cp.Signals[i].Fn = cp.signalSemantic
        case "freshness":       cp.Signals[i].Fn = cp.signalFreshness
        case "reference_frame": cp.Signals[i].Fn = cp.signalReferenceFrame
        case "feedback":        cp.Signals[i].Fn = cp.signalFeedbackHistory
        default:                cp.Signals[i].Fn = s.Fn  // 包级函数，无接收者
        }
    }
    return cp
}
```

**关键设计点**：`Signals` 里 `semantic` / `freshness` / `reference_frame` / `feedback` 是**方法值闭包**，在 `DefaultRankFusion` 创建时就捕获了模板的 `rf` 指针。若只做结构体浅拷贝，副本的 `Signals` 仍指向模板字段，导致副本的 `Now` / `QueryFrame` 不生效（写入副本、读取模板，结果错误且仍是竞争）。因此 `ForRequest` 必须把这些信号重建为绑定到**副本 `rf`** 的闭包。`keyword` / `entity` / `value` / `source_trust` 是包级函数（无接收者），可原样复用。

### 3.3 `RecallAndFuse` 改造

```go
rf := s.fusion
if rf == nil {
    rf = DefaultRankFusion()
}
// 确定性：仅当注入方未显式设置 Now 时才用当前时刻。
if rf.Now.IsZero() {
    nowVal = time.Now().UTC()
} else {
    nowVal = rf.Now
}
rf = rf.ForRequest(nowVal, rc.AsOf, rc.frame())   // 写入落在副本
fused := rf.Fuse(ctx, rc.Text, flat)
```

注入的 `*RankFusion` 只作**模板**（拷贝到请求级），保留权重/信号配置，不共享写。`RecallAndFuseMultiHop` 委托 `RecallAndFuse`，故单点修复覆盖两条路径。

### 3.4 `rejectedIDs` 并发保护

`APIServer` 新增 `sync.RWMutex`（`rejectedIDsMu`）+ 3 个辅助方法，替换 3 处裸访问：

| 原位置 | 原访问 | 改造后 |
|---|---|---|
| `router_tools.go:251` | `s.rejectedIDs = append(s.rejectedIDs, m.MemoryID)` | `s.addRejectedID(m.MemoryID)` |
| `router_mem.go:237` | `for _, rid := range s.rejectedIDs { ... }` | `s.isRejectedID(c.Memory.MemoryID)` |
| `router.go:392` | `for _, rid := range s.rejectedIDs { ... }` | `s.isRejectedID(memoryID)` |

读写均受同一把 `RWMutex` 保护（读 `RLock`，写 `Lock`），既消除竞争又保持读并发。

---

## 四、不回归证明（0.88 实测）

### 4.1 北极星 M1（vector 口径）

命令：
```sh
go run eval/polaris/polaris_voting.go eval/polaris/compress_light.go \
  eval/polaris/polaris_m1.go eval/polaris/polaris_token.go \
  eval/polaris/polaris_fact.go eval/polaris/polaris_fp.go \
  -fusion rrf -ref-activate=true -embedder vector \
  -weights eval/weights/rank_weights_r6_ref.json -now-fixed 2026-10-13 -topk 5
```

结果：

| 指标 | `c9ca837` 基线 | R10-A 修复后 | 判定 |
|---|---|---|---|
| M1（coverage） | 1.0000 | **1.0000**（590/590） | ✅ |
| hit@1 | 0.9763 | **0.9763** | ✅ |
| mean_rr | 0.9113 | **0.9113** | ✅ |
| ref 桶 hit@1 | 46/60 | **46/60** | ✅ |

分桶明细：single-hop 150/150、multi-hop 150/150、open-domain 100/100、temporal 130/130、reference_frame 46/60，全部与基线逐位一致。

### 4.2 公开基准 MRR

命令：
```sh
cd eval && go run ./run_kpi.go -dataset ./benchmarks/dataset/all.jsonl \
  -temporal-samples dataset/temporal.jsonl -weights weights/rank_weights_en_r5.json \
  -embedder vector -now-fixed 2026-10-13 -report r10a_noreg
```

结果：

| 指标 | `c9ca837` 基线 | R10-A 修复后 | 判定 |
|---|---|---|---|
| 总体 MRR | 0.9917 | **0.9917**（精确 0.9917094） | ✅ |
| single-hop MRR | 0.9943 | 0.994 | ✅ |
| multi-hop MRR | 0.9800 | 0.980 | ✅ |
| open-domain MRR | 0.9788 | 0.979 | ✅ |
| temporal MRR | 0.9983 | 0.998 | ✅ |
| fact acc v4 | 0.9607（660/687） | **0.9607**（660/687） | ✅ |
| 记忆污染率 | 0.0% | 0.0% | ✅ |

### 4.3 融合算法逐位一致性

`Fuse` / `rrfRank` / `consensusVoteRank` 算法未改动，`AbsoluteFloor` / `ProximityRatio` / `HalfLife` / `RRFK` 语义未变。北极星 M1 + 公开 MRR 的逐位复现即为最强证据。

### 4.4 串行基线输出一致

`TestConcurrentRecallAndFuse_SharedInstance` 内先取串行基线 `baseIDs`，16 个 goroutine × 20 次迭代并发调用后逐位比对，全部一致（无失败样例）。`TestConcurrentRecallAndFuse_DifferentTenant` 进一步验证并发后共享实例未被污染（基线可复现）。

---

## 五、`-race` 前后对比

| 阶段 | 命令 | 结果 |
|---|---|---|
| **修复前** | `go test -race -run 'TestConcurrent' -count=1 ./kernel/` | ❌ **59 处 `DATA RACE` + 5 FAIL**（`router_mem.go:251/252/253` 写、`rank_fusion.go:896` 读） |
| **修复后** | `go test -race -run 'TestConcurrent' -count=1 ./kernel/` | ✅ **ok 1.208s，无数据竞争** |
| **修复后全量** | `go test -race ./kernel/... ./eval/e2e/...` | ✅ kernel ok 1.266s、i18n ok 1.022s、eval/e2e ok 1.283s，全绿 |

---

## 六、性能基准前后对比（`BenchmarkRecallAndFuse`）

命令：
```sh
go test -bench 'BenchmarkRecallAndFuse$' -benchmem -run xxx -count=3 ./kernel/
```

| 阶段 | run1 (ns/op) | run2 (ns/op) | run3 (ns/op) | 平均 (ns/op) | allocs/op |
|---|---|---|---|---|---|
| **修复前** | 558224 | 581442 | 571679 | **570448** | 2888 |
| **修复后** | 553917 | 570496 | 566096 | **563503** | 2894 |

**相对回退**：`(563503 - 570448) / 570448 = -1.22%`（实际**略快**，属基准噪声范围），**远低于 5% 红线**，判定 **PASS**。

单独测 `ForRequest` 构造开销：`BenchmarkForRequest` 平均 **~368 ns/op** / 6 allocs，相对 `BenchmarkRecallAndFuse` 的 563 µs/op 仅占 **0.065%**，可忽略。

---

## 七、门禁结果汇总（0.88 实测）

| 检查项 | 命令 | 结果 |
|---|---|---|
| gofmt | `gofmt -l kernel/ eval/` | ✅ 无输出 |
| go vet（root module） | `go vet ./...` | ✅ OK |
| go vet（kernel 嵌套模块） | `go vet ./kernel/` | ✅ OK |
| go test（root module） | `go test ./...` | ✅ benchmarks / e2e / polaris 全绿 |
| go test（kernel 嵌套模块） | `go test ./kernel/` | ✅ ok 0.089s |
| **`-race`（kernel + e2e）** | `go test -race ./kernel/... ./eval/e2e/...` | ✅ 全绿 |
| **北极星 M1 不回归** | polaris vector 口径 | ✅ 1.0000 |
| **公开 MRR 不回归** | run_kpi vector 口径 | ✅ 0.9917 |
| **性能不回归** | `BenchmarkRecallAndFuse` | ✅ -1.22% |
| 新依赖 | 无（仅标准库 `sync`） | ✅ 不适用 |

> **模块结构说明**：`kernel/` 是嵌套 Go 模块（`go.mod: module github.com/lightmem/kernel`），故 kernel 测试需从 `kernel/` 目录内跑；root module 仅含 `eval/`。这与 R9 基线一致（`r9_final` 下 `go test ./kernel/` 同样报 `does not contain package`，非本次引入）。

---

## 八、改动 diff 清单

```diff
# kernel/rank_fusion.go（只增）
+ func (rf *RankFusion) ForRequest(now time.Time, asOf *string, qf *ReferenceFrame) *RankFusion
+   // 配置继承 + 请求状态独立 + 信号闭包重绑定到副本

# kernel/router_mem.go（局部改）
- rf.Now = time.Now().UTC()      // 写共享实例（race）
- rf.AsOf = rc.AsOf              // 写共享实例（race）
- rf.QueryFrame = rc.frame()     // 写共享实例（race）
+ nowVal := ...
+ rf = rf.ForRequest(nowVal, rc.AsOf, rc.frame())   // 写请求级副本
- for _, rid := range s.rejectedIDs { ... }          // 裸读（race）
+ if !s.isRejectedID(c.Memory.MemoryID) { ... }      // 加锁读

# kernel/router.go（只增）
+ import "sync"
+ rejectedIDsMu sync.RWMutex
+ func (s *APIServer) addRejectedID(id string)
+ func (s *APIServer) isRejectedID(id string) bool
- for _, rid := range s.rejectedIDs { ... }          // 裸读（race）
+ rejected := s.isRejectedID(memoryID)               // 加锁读

# kernel/router_tools.go（只增）
- s.rejectedIDs = append(s.rejectedIDs, m.MemoryID)  // 裸写（race）
+ s.addRejectedID(m.MemoryID)                        // 加锁写

# kernel/rank_fusion_race_test.go（新增）
+ TestConcurrentRecallAndFuse_SharedInstance
+ TestConcurrentRecallAndFuse_DifferentTenant
+ TestConcurrentRejectedIDs

# kernel/rank_fusion_bench_test.go（新增）
+ BenchmarkRecallAndFuse
+ BenchmarkRecallAndFuse_FixedCandidates
+ BenchmarkForRequest
```

---

## 九、已知限制与登记

1. **`kernel/rank_fusion_race_test.go` 依赖同包 `fusedIDs` helper**（定义于 `kernel/composition_test.go`，入参 `[]FusedResult`）。若未来删除/改名该 helper 需同步更新，已在本文件头注释登记。
2. **BGE-m3 向量文件为外部产物**（`eval/vectors/vectors.npy` + `vectors_index.jsonl` + `vectors_meta.json`），不在 git 内；本路从已验证工作树 `/tmp/r9_final/lightmem/eval/vectors/` 复制。若工作树未同步，`embedder auto` 会降级为 tfidf 口径（北极星 M1 主判定仍 =1.0000，但 hit@1 / mean_rr 数值不同，需在报告中标注）。
3. **`ForRequest` 的 `Weights` 为 map 只读共享**：若未来有代码在请求级修改 `Weights`，会污染共享 map。当前无此用法（权重只在 `LoadWeightsFromFile` / `SetWeights` 写入，均在构造阶段），已在方法注释中标注为「只读共享」约定。
4. **性能基准受 CPU 调度噪声影响**：-1.22% 的「略快」属噪声范围，结论为「未超 5% 红线」而非「确实更快」。

---

## 十、结论

R10-A 已完成：**消除了生产 `RankFusion` 共享实例的并发数据竞争**（`-race` 从 59 处报告降至 0），**北极星 M1=1.0000 / 公开 MRR=0.9917 逐位不回归**，**性能回退 -1.22%（<5% 红线）**，门禁（gofmt / vet / test / `-race`）全绿，未引入新依赖，未改动融合算法与 Schema 语义。

待用户示意后 push 到 gitea main。