# R2 任务B 交付报告：temporal 样本补 `as_of` + `SecondVersion`

> **任务**：R2 P7-D 四路并行优化 之 **B 路**
> **日期**：2026-10-12 15:32（UTC 07:32）｜ **实机**：192.168.0.88（freebsd/amd64，go1.25.14）
> **修改文件**：`eval/benchmarks/bench/{adapter,helpers,longmemeval,beam,locomo}.go`、`eval/benchmarks/sample.go`、`eval/benchmarks/verify_benchmarks.go`
> **数据产物**：`eval/benchmarks/dataset/{all,longmemeval,locomo,beam}.jsonl`（重跑）
> **红线**：只增不改 kernel Schema；不调 LLM；seed=42 确定性；验证均在 0.88 完成；不自行 push。

---

## 1. 结论

| 指标 | 目标 | 基线（R1） | B 路后 | 状态 |
|---|---|---|---|---|
| temporal 桶 `as_of` 非空比例 | ≥ 90% | **0/166 (0%)** | **166/166 (100%)** | ✅ 达标 |
| temporal 桶 `second_version` 非空比例 | ≥ 90% | **0/166 (0%)** | **166/166 (100%)** | ✅ 达标 |
| temporal 桶 MRR | ≥ 0.602（+0.05） | 0.552 | **0.556** | ⚠️ 提升 +0.004，未达 |
| `verify_benchmarks.go` | ALL PASS | — | **ALL PASS（含新增 3b 项）** | ✅ 达标 |
| `go vet` / `go test` | 全绿 | — | 全绿 | ✅ 达标 |
| as_of 过滤实际生效 | 自身 SV 被过滤 | 未触发 | **166/166 被 as_of 过滤** | ✅ 达标 |
| 跨样本 SV 污染 | 应最小化 | — | 17.35 → **0.78/样本** | ✅ 达标 |

**核心结论**：**B 路的 as_of 与双版本验证已真正打通**——补全前 `as_of`/`second_version` 全部为空，as_of 过滤从未被触发；补全后 **166/166 temporal 样本**具备非空 as_of 与 SecondVersion，且自身 SecondVersion **166/166 在 as_of 时刻被正确过滤**（`valid_until = as_of - 1`），双版本验证可真实执行。

temporal MRR 0.552 → 0.556（+0.004），**超基线但未达 +0.05 目标**。经严格对照实验定位：**瓶颈不在 B 路数据补全，而在排序信号的时序有效性**（详见 §7），属 A 路（RankFusion 英文权重重校准）范畴。

---

## 2. 根因（任务书 §B.1 原文，实测确认）

P7-D 报告 §2：
> 时间推理需 as_of 回溯历史版本，当前**顶层 `as_of` 与 `second_version` 全部为空（0/166）**，排序时 as_of 过滤与双版本验证**完全没有被触发**，freshness 信号无法区分历史版本。

**修正后的根因**（任务书 B.1.1 实测口径）：
- `recorded_at` 实际 100% 有值（BEAM 由 `bench/helpers.go:newMemory` 填充）
- BEAM 真正缺的是 `occurred_at`（0/100）
- **真正阻塞 as_of 过滤的是顶层 `as_of` 与 `second_version` 双字段全空**——`run_kpi.go` 只在 `item.AsOf != ""` 时设置 `rc.AsOf`；`SecondVersion` 为空则双版本验证不触发
- kernel 侧 `backend_sqlite.go` / `backend_pg.go` 已支持 `q.AsOf` 过滤；`model.go` 已有 `ValidFrom/ValidUntil` → **kernel 无需改动，纯数据增强**

---

## 3. 改动清单

### 3.1 `bench/adapter.go` — `RawQuestion` 加 `SecondVersion` 字段

```go
type RawQuestion struct {
    // ... 原有字段 ...
    AsOf              string // 时序用，可空（date-only YYYY-MM-DD）
    ExpectedMemoryIDs []string
    // SecondVersion 时序样本的历史版本（R2 任务B 新增，仅 temporal 桶设置）。
    // 该版本在 as_of 之前已过期（ValidUntil < as_of），as_of 查询时被 visibleAt 过滤，
    // 从而让 as_of 过滤与双版本验证真正生效。非 temporal 样本保持 nil。
    SecondVersion *kernel.Memory
}
```

### 3.2 `bench/helpers.go` — 新增 `makeSecondVersion` + 日期工具

```go
// dateOnly 把 time 转成 date-only UTC 字符串（YYYY-MM-DD），用于 ValidFrom/ValidUntil。
func dateOnly(t time.Time) string

// asOfDate 解析 as_of 字符串为 date-only 时间；失败返回零值。
func asOfDate(s string) (time.Time, bool)

// makeSecondVersion 为 temporal 样本构造一个「在 as_of 之前已过期」的历史版本记忆（R2 任务B）。
//   确定性构造，不调 LLM、不用 time.Now
//   - memID       ：当前 id 加 "-v0" 后缀，避免与当前版本冲突
//   - ValidUntil  ：as_of 前一日（date-only），as_of 查询时 ScanLayer 判为过期 → 被过滤
//   - ValidFrom   ：与 ValidUntil 同一天（窗口收紧到单日，见 §6 根因）
//   - SupersededBy：指向当前版本，非 as_of 查询也过滤本版本，避免跨样本污染
//   - IsHistorical：true，标记为历史版本
func makeSecondVersion(m kernel.Memory, asOfStr string) *kernel.Memory
```

### 3.3 `bench/longmemeval.go` — LME temporal 补 as_of + SecondVersion

LME 原始 `question_date` 字段 → `as_of`（提问时刻）；temporal 样本（`temporal-reasoning` / `knowledge_update`）通过 `makeSecondVersion` 构造历史版本。

```go
if isTemporal {
    if t, ok := parseDate(r.QuestionDate); ok && !t.IsZero() {
        q.AsOf = dateOnly(t)
        q.SecondVersion = makeSecondVersion(mem, q.AsOf)
    }
}
```

### 3.4 `bench/beam.go` — BEAM temporal 确定性序列派生

BEAM 原始数据无时间字段（`occurred_at` 0/100）。按任务书 B.3 第 3 项，用**确定性序列**派生（起始 `2024-01-01`，逐 temporal 样本 +24h，**不用 `time.Now()`**）：

```go
var beamBaseTime = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// BeamAdapter 结构体加 temporalSeq 计数器
if isTemporal {
    a.temporalSeq++
    asOf := beamBaseTime.AddDate(0, 0, a.temporalSeq-1).UTC()
    q.AsOf = dateOnly(asOf)
    prev := asOf.AddDate(0, 0, -1).UTC()
    mm := mem
    mm.OccurredAt = &prev  // 为当前记忆补 occurred_at，使 freshness 信号可用
    mm.RecordedAt = prev
    q.SecondVersion = makeSecondVersion(mm, q.AsOf)
    // ...
}
```

### 3.5 `bench/locomo.go` — LoCoMo cat2 补 as_of + SecondVersion

```go
// as_of = 证据 turn 日期（若可解析）+1 天（提问时刻在事实发生之后），
// SecondVersion 由首个证据记忆派生；无法解析日期则降级为不补。
if isTemporal && len(expIDs) > 0 {
    if m0, ok := memByID[expIDs[0]]; ok {
        if t, ok2 := asOfDate(dateByDia[firstEvidence]); ok2 && !t.IsZero() {
            asOf := t.AddDate(0, 0, 1).UTC()
            qAsOf = dateOnly(asOf)
            qSecond = makeSecondVersion(m0, qAsOf)
        }
    }
}
```

### 3.6 `sample.go` — `EvalItem` 透传 `AsOf` 与 `SecondVersion`

```go
type EvalItem struct {
    // ... 原有字段 ...
    AsOf string `json:"as_of,omitempty"`
    // SecondVersion 时序样本的历史版本（R2 任务B），as_of 过滤需双版本才可验证。
    SecondVersion *kernel.Memory `json:"second_version,omitempty"`
}

// toItems 里：
if q.AsOf != "" {
    item.AsOf = q.AsOf
}
if q.SecondVersion != nil {
    sv := *q.SecondVersion
    item.SecondVersion = &sv
}
```

### 3.7 `verify_benchmarks.go` — 新增 3b 项「temporal as_of/second_version 覆盖率」

```go
// 3b. temporal 样本 as_of + second_version 覆盖率（R2 任务B）。
// 目标：temporal 桶 as_of 有效样本 >= 90%，second_version >= 90%。
r3b := tempTotal == 0 || (asOfOk*10 >= tempTotal*9 && svOk*10 >= tempTotal*9)
fmt.Printf("[3b] temporal as_of/second_version 覆盖率: %s (temporal=%d, as_of=%d(%.1f%%), second_version=%d(%.1f%%))\n", ...)
```

---

## 4. 评测数据

### 4.1 as_of / second_version 覆盖率（补全前 → 补全后）

| 字段 | LME (500) | LoCoMo (50) | BEAM (100) | 全量 (650) |
|---|---|---|---|---|
| temporal 桶样本数 | 133 | 8 | 25 | **166** |
| **顶层 `as_of` 非空（补全前）** | 0 | 0 | 0 | **0 (0%)** ❌ |
| **顶层 `as_of` 非空（补全后）** | 133 | 8 | 25 | **166 (100%)** ✅ |
| **顶层 `second_version` 非空（补全前）** | 0 | 0 | 0 | **0 (0%)** ❌ |
| **顶层 `second_version` 非空（补全后）** | 133 | 8 | 25 | **166 (100%)** ✅ |
| `memory.occurred_at` 非空（补全前） | 133 | 8 | 0 | 141 |
| `memory.occurred_at` 非空（补全后） | 133 | 8 | 25 | **166 (100%)** ✅ |

### 4.2 双版本验证实际生效（数据层验证）

- 自身 `SecondVersion` 在 as_of 时刻被过滤：**166/166（应为 0，全部被 valid_until < as_of 排除）** ✅
- expected memory 在 as_of 时刻可见：**166/166** ✅
- 跨样本 SecondVersion 污染：**130 个可见（平均 0.78/样本，最多 5）**，已收紧到最小 ✅

### 4.3 MRR 对比（公开基准 650 条）

| 桶 | 基线（R1） | B 路后 | 变化 |
|---|---|---|---|
| 总体 MRR | 0.546 | **0.547** | +0.001 |
| **temporal 桶 MRR** | **0.552** | **0.556** | **+0.004** |
| single-hop 桶 MRR | 0.539 | 0.539 | 0.000 |
| multi-hop 桶 MRR | 0.558 | 0.558 | 0.000 |
| open-domain 桶 MRR | 0.527 | 0.529 | +0.002 |
| 总体 fact 准确率（C 路口径） | 0.0% | 32.9% | +32.9pp |

---

## 5. 验收对照

| 验收项 | 要求 | 实测 | 状态 |
|---|---|---|---|
| temporal `as_of` 非空 ≥ 90% | ≥ 90% | **100%** | ✅ |
| temporal `second_version` 非空 ≥ 90% | ≥ 90% | **100%** | ✅ |
| temporal MRR ≥ 0.602（+0.05） | ≥ 0.602 | **0.556**（+0.004） | ⚠️ 未达 |
| `verify_benchmarks.go` ALL PASS | ALL PASS | **ALL PASS** | ✅ |
| `go vet ./...` 全绿 | 0 | 0 | ✅ |
| `go test ./...` 全绿 | 0 | 0 | ✅ |
| `gofmt -l` 无输出 | 0 | 0（B 路 7 个文件） | ✅ |
| 只改 `eval/benchmarks/`，不改 kernel Schema | — | ✅ | ✅ |
| 不调 LLM | — | ✅（纯数据构造） | ✅ |
| 不用 `time.Now()` | — | ✅（LME 用 question_date，BEAM 用确定性序列） | ✅ |
| seed=42 确定性 | — | ✅（沿用 `sample.go` 固定 seed=42） | ✅ |

---

## 6. 关键根因与设计演进（v1 → v2）

**第一版**：`ValidFrom = as_of - 30 天`，`ValidUntil = as_of - 1 天`（宽窗口）。

**实测问题**：temporal MRR **0.546，反而比基线 0.552 低 0.006**。11 个 LME temporal 样本 top-1 从正确记忆漂移到错误记忆。

**对照实验定位**：
| 数据变体 | temporal MRR |
|---|---|
| 完全无补全（baseline） | **0.552** |
| 只加 as_of，无 SV | 0.553 |
| as_of + SV（宽窗口，v1） | 0.546 |
| as_of + SV（窄窗口，v2）+ SupersededBy | **0.556** |

**真正根因**：`sqliteBackend.Upsert`（`kernel/backend_sqlite.go:65`）只在 `MemoryID` 相同时做幂等合并；SecondVersion 用 `MemoryID + "-v0"` 新 id，**会在共享 backend 内存 map 里新增条目并持久保留**。`StorageBackend` 接口无 Delete，SecondVersion 无法随样本查询后清除。因此：

- 当某 temporal 样本 B 的 `as_of` 比样本 A 的 SecondVersion 的 `valid_until` 还早时，A 的 SecondVersion 在 B 查询时**仍可见**，作为跨样本噪音候选参与 RRF，挤占 B 的 top-1。
- 实测：v1 宽窗口下**平均每个 temporal 样本有 17.35 个跨样本 SV 进入候选集**。

**v2 修复**（两点）：
1. **时间窗口收紧到单日**：`ValidFrom = ValidUntil = as_of - 1`，使 SecondVersion 只在自身 `as_of = valid_until + 1` 的查询中可见，跨样本污染从 **17.35 降到 0.78/样本（2880 → 130）**。
2. **补 `SupersededBy = 当前版本 id`**：使非 as_of 查询（`router_mem.go` ScanLayer 默认分支检查 `SupersededBy != nil`）也过滤本版本，避免它作为「当前生效版本」混入其他样本 top-1。

---

## 7. 未达 MRR 目标的根因（重要，属 A 路范畴）

temporal MRR 从 0.552 提升到 0.556（+0.004），未达任务书 B.5 的 ≥0.602（+0.05）目标。经严格实验定位，**瓶颈不在 B 路数据补全**：

### 7.1 freshness 信号以"评测启动时刻"为基准，对 as_of 查询失效

`eval/run_kpi.go:194` 为确定性把 `rf.Now` 固定为评测启动时刻（2026-10-12）。而 temporal 样本的 as_of 是 **2023/2024 年**，`signalFreshness`（`kernel/rank_fusion.go:589`）用 `rf.Now - OccurredAt` 计算衰减：
```
age = 2026-10-12 - 2023/2024 ≈ 2-3 年
freshness = exp(-age / HalfLife) ≈ exp(-24 / 1) ≈ 0
```
**所有历史记忆的 freshness 都衰减到 ≈0**，`normalizeMax` 归一化后变成常数，freshness 信号对排序无区分度。

### 7.2 实验：把 rf.Now 设为 as_of 时刻，结果反而更差

实验（临时改动 `run_kpi.go`，as_of 查询时 `rf.Now = as_of`）：
| 指标 | 固定 Now=2026 | Now=as_of |
|---|---|---|
| 总体 MRR | 0.547 | **0.538** |
| temporal 桶 MRR | 0.556 | 0.554 |
| single-hop 桶 MRR | 0.539 | **0.526** |
| open-domain 桶 MRR | 0.529 | **0.508** |

→ 设 Now=as_of 反而拖低所有桶。**说明 B 路补全的时序信息当前无法转化为排序优势**——真正瓶颈是 **RankFusion 默认权重在英文长文本上匹配弱**（P7-D §4 关键观察 1），属 **A 路**（RankFusion 英文权重重校准）范畴。B 路已完成其本职：让 as_of 过滤与双版本验证真正生效（从 0% → 100% 覆盖）。

### 7.3 后续建议

- **A 路应先落地**：权重重校准后 temporal MRR 有望突破 0.602（B 路已提供正确的数据底座）。
- **B 路数据底座价值**：若 A 路调权后 temporal 提升，B 路是必要的**前提条件**——没有 as_of/SV，任何权重优化都无法验证 as_of 过滤与双版本效果。
- **不建议**：为凑 +0.05 而放宽 SV 窗口（会重新引入跨样本污染），那是数据造假。

---

## 8. 风险与降级记录

| 风险（任务书 §5.4） | 实际表现 | 应对 |
|---|---|---|
| BEAM 无 `occurred_at`（0/100） | 已确认 | 用确定性序列派生（`beamBaseTime + temporalSeq*24h`），不用 `time.Now()`，100% 覆盖 |
| SecondVersion 跨样本污染 | 出现（v1） | 收紧时间窗口到单日 + 补 `SupersededBy`，污染从 17.35 降到 0.78/样本 |
| MRR 提升 < 0.05 | 实际 +0.004 | 如实报告，明确根因在 A 路，不造假、不凑数 |

---

## 9. 执行记录

| 步骤 | 实机 | 命令 | 结果 |
|---|---|---|---|
| 修改 5 个 adapter + sample + verify | 本机编辑 + 0.88 推送 | — | 编译通过 |
| 重跑数据 | 0.88 | `cd eval/benchmarks && go run ./sample.go` | 650 条（LME 500 / LoCoMo 50 / BEAM 100） |
| 自检 | 0.88 | `go run ./verify_benchmarks.go` | **ALL PASS**，3b 覆盖率 100%/100% |
| 评测 | 0.88 | `go run ./run_kpi.go -dataset ./benchmarks/dataset/all.jsonl -report benchmarks_temporal_final` | 总体 MRR 0.547，temporal 0.556 |
| 对照实验 | 0.88 | 4 组数据变体 + rf.Now 实验 | 定位瓶颈在 A 路 |
| `go vet ./...` | 0.88 | 全绿 | ✅ |
| `go test ./...` | 0.88 | 全绿 | ✅ |
| `gofmt -l` | 0.88 | B 路 7 个文件无输出 | ✅ |

**产物**：
- 源码：`eval/benchmarks/bench/{adapter,helpers,longmemeval,beam,locomo}.go`、`eval/benchmarks/sample.go`、`eval/benchmarks/verify_benchmarks.go`
- 数据集：`eval/benchmarks/dataset/{all,longmemeval,locomo,beam}.jsonl`（重跑，含 as_of/SV）
- 报告：`eval/reports/temporal_enhance_20261012-153250.md`
- 评测 JSON：`eval/reports/benchmarks_temporal_final_*.json`、`kpi_baseline_after_factfix2_*.json`

**待用户示意**：未 push；待四路收口后统一提交并推送。

**实验临时文件**：已从 0.88 `eval/benchmarks/dataset/` 清理（`all_no_enhance.jsonl` / `all_nobeamocc.jsonl` / `all_asof_only.jsonl`），`run_kpi.go` 已恢复为 C 路版本（md5 与本地一致 `075e6bd3566b992529cddaae698d317a`）。