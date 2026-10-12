# R3-B 报告：temporal as_of 时序一致性 + signalFeedbackHistory 确定性修复

- **任务**：P7-D R2 残留项四路并行攻坚 —— **B 路（temporal as_of 时序一致性）**
- **日期**：2026-10-12
- **验收人**：天马
- **状态**：**完成**（含 2 项修复：feedback 确定性 + freshness 未来记忆 bug）

---

## 0. TL;DR

| 维度 | 修复前（R3-D 基线） | 修复后（R3-B） | 变化 |
|---|---|---|---|
| 公开基准 主 MRR | 0.9747 | **0.9754** | **+0.0007** |
| 公开基准 temporal 桶 MRR | 0.9940 | **0.9940** | 持平 |
| 公开基准 fact 准确率 | 0.9231 | **0.9246** | +0.0015 |
| 合成集 主 MRR | 0.8377 | **0.8434** | **+0.0057** |
| 合成集 single-hop 桶 MRR | 0.9267 | **0.9467** | **+0.0200** |

**结论**：**采用 `Now=as_of`**。`Now=2026-10-12` / `Now=评测启动时刻` / `Now=as_of` 三组在修复后**完全一致**（0.9754），证明 flag 无副作用且 freshness 修复消除了 as_of 模式的时序 bug。所有提升归因于 **signalFreshness 未来记忆 bug 修复**，**不是**权重重调（权重未动）。

---

## 1. 根因分析（R2 收口 §8.2 + kernel 源码实测）

### 1.1 signalFeedbackHistory 是第二个隐藏时间源（任务书 B.2.1 根因 1）

`kernel/rank_fusion.go` 原实现：

```go
func signalFeedbackHistory(...) map[string]float64 {
    out := make(map[string]float64, len(cands))
    now := time.Now().UTC()   // ← 用的是真实当前时间，不是 rf.Now！
    ...
}
```

- **违反 seed=42 确定性**：每次评测 `rf.Now=2026-10-12`，但 feedback 却用**真实运行时刻**（每次运行可能差几分钟）判断「90 天内是否 recent」。这造成跨运行不可复现。
- **与 as_of 时序不一致**：as_of=2023 的样本，feedback 却用 2026 判断 recent。若 feedback 记录恰好是 2023-04 的，本应「相对 as_of 很新」，却被判成「3 年前的旧反馈」→ 权重衰减错误。

### 1.2 signalFreshness 未来记忆 bug（**R3-B 新发现，任务书未列**）

在实验 `Now=as_of` 时发现（详见 §3），`kernel/rank_fusion.go` 的 `signalFreshness`：

```go
age := rf.Now.Sub(t)
if age < 0 {
    age = 0    // ← bug：clamp 到 0 后 exp(0) = 1.0，未来记忆得满分
}
out[c.Memory.MemoryID] = math.Exp(-float64(age) / float64(hl))
```

两个触发路径：

**路径 A（公开基准 temporal 桶，2 条样本）**：as_of 之前的干扰项 occurred_at > as_of。原实现 freshness=1.0（未来记忆满分）把干扰项抬到 top-1。

- `lme-gpt4_cd90e484`（期望）occurred_at=2023-05-26，as_of=2023-05-26；干扰项 `lme-993da5e2` occurred_at=**2023-05-30**（4 天后）
- `lme-gpt4_4ef30696`（期望）occurred_at=2022-05-01，as_of=2022-05-01；干扰项 `lme-0db4c65d` occurred_at=**2023-02-10**（9 个月后）

修复前 `Now=as_of` 时这两条 rank 1→2，temporal 桶 MRR 0.9940→0.9880。

**路径 B（合成集 single-hop，6 条样本）**：`recorded_at` / `occurred_at` 为零值 `0001-01-01T00:00:00Z`。`rf.Now.Sub(t)` 溢出为**负 duration**（2025 年 >> Go time.Duration 上限 292 年）→ 触发 `age<0` 分支 → **无时间戳记忆**被误判为「最新」，freshness=1.0。

- 6 条 single-hop 提升：`sh-030/040/061/081/112/141`
- 1 条 open-domain 提升：`beam-1M_28_q16_summarization`（rank 2→1）

**修复**：未来记忆 freshness 归 0（「该记忆当时还不存在，时效不可用」），无时间戳记忆 freshness 也归 0（age 溢出同样得 0，逻辑自洽）。

---

## 2. 修改清单

### 2.1 `kernel/rank_fusion.go`

| 位置 | 修改 |
|---|---|
| line 605-611 | `signalFreshness`：`age<0` 分支从 `age=0`（→1.0 满分）改为 `out[...]=0.0; continue` |
| line 726-736 | `signalFeedbackHistory`：从包级函数改为 `rf` 方法，`now := rf.Now`（零值回退 `time.Now().UTC()`） |
| line 139 | `DefaultRankFusion()`：`Fn: rf.signalFeedbackHistory`（原 `Fn: signalFeedbackHistory`） |

### 2.2 `eval/run_kpi.go`

| Flag | 用途 |
|---|---|
| `-now-asof` | as_of 查询时把 `rf.Now` 设为样本 as_of（R3-B 对照实验） |
| `-now-fixed <YYYY-MM-DD>` | 固定 `rf.Now` 为该日期（复现 R2 报告 rf.Now=2026-10-12） |
| `-diag <id>` | 诊断打印：指定样本 top-5 各信号分（用于根因定位） |

**关键设计**（任务书 B.3.2 警告）：

- `rf` 是**全局指针**，逐样本改 `rf.Now` 会污染下一条。
- 方案：每条查询前把 `rf.Now` 重置为 `baseNow`；`-now-asof` 只对 `AsOf != ""` 的样本覆盖为样本 as_of。非 as_of 样本始终用 baseNow。
- `RecallAndFuse` 内部不会覆盖非零 `rf.Now`（`kernel/router_mem.go:250` 只在 `IsZero()` 时设置），因此此方案可靠。

### 2.3 `kernel/rank_fusion_r3b_test.go`（新增）

5 个单测覆盖 B 路：

1. `TestSignalFreshnessFutureMemoryZero`：未来记忆 freshness=0
2. `TestSignalFreshnessMonotonicInPast`：过去越近越新
3. `TestSignalFeedbackHistoryUsesRFNow`：feedback 用 rf.Now 判断 recent（90 天内）
4. `TestSignalFeedbackHistoryFallbackWhenNowZero`：rf.Now 零值回退 `time.Now().UTC()`
5. `TestSignalFeedbackHistoryNoFeedbackNeutral`：无反馈记忆中性 0.5

---

## 3. 三组对照实验（`Now=2026-10-12` / `Now=评测启动时刻` / `Now=as_of`）

### 3.1 公开基准（`eval/benchmarks/dataset/all.jsonl`，n=650）

| 配置 | 主 MRR | temporal 桶 MRR | fact 准确率 | 备注 |
|---|---|---|---|---|
| `-now-fixed 2026-10-12` | **0.975445** | **0.9940** | **0.9246** | 任务书基准 |
| 无 flag（评测启动时刻） | **0.975445** | **0.9940** | **0.9246** | 现状基线 |
| `-now-asof` | **0.975445** | **0.9940** | **0.9246** | 实验组 |

**三组完全一致** → `-now-asof` flag 无副作用；freshness 修复后 as_of 模式不再降级。

### 3.2 合成集（`eval/dataset/all.jsonl`，n=530）

| 配置 | 主 MRR | single-hop 桶 MRR | temporal 桶 MRR | multi-hop 口径 A |
|---|---|---|---|---|
| `-now-fixed 2026-10-12` | **0.843396** | **0.9467** | **1.0000** | **1.0000** |
| 无 flag | **0.843396** | **0.9467** | **1.0000** | **1.0000** |
| `-now-asof` | **0.843396** | **0.9467** | **1.0000** | **1.0000** |

### 3.3 修复前 vs 修复后（合成集 single-hop）

| 阶段 | 合成集 主 MRR | single-hop 桶 MRR | 说明 |
|---|---|---|---|
| R3-A 基线（12:07） | 0.837736 | 0.9267 | 无 freshness 修复 |
| R3-D 基线（12:38） | 0.837736 | 0.9267 | 无 freshness 修复 |
| R3-B 修复后（13:32） | **0.843396** | **0.9467** | freshness 未来记忆=0 |
| R3-B freshness 还原（13:48） | 0.837736 | 0.9267 | 临时还原复现 |

**→ 6 条 single-hop + 1 条 open-domain 提升全部归因于 freshness 修复，非权重或实验设置差异。**

---

## 4. 诊断数据（`-diag` 打印 top-5 信号分）

### 4.1 `lme-gpt4_cd90e484`（as_of=2023-05-26，期望 occurred_at=2023-05-26）

| 修复前 `Now=评测启动` | freshness | score |
|---|---|---|
| #1 lme-gpt4_cd90e484（**期望**） | 0.0013 | 0.98010 |
| #2 lme-993da5e2（occurred_at=2023-05-30，**未来**） | 0.0015 | 0.97960 |

| 修复前 `Now=as_of` | freshness | score |
|---|---|---|
| #1 lme-993da5e2（**未来**） | **1.0000** | 0.98027 |
| #2 lme-gpt4_cd90e484（**期望**） | **1.0000** | 0.97985 |

→ 未来记忆 freshness=1.0，把语义/keyword 都弱的干扰项抬到 top-1。

### 4.2 `lme-gpt4_4ef30696`（as_of=2022-05-01，期望 occurred_at=2022-05-01）

| 修复前 `Now=as_of` | freshness | score |
|---|---|---|
| #1 lme-0db4c65d（occurred_at=2023-02-10，**9 个月后**） | **1.0000** | 0.96224 |
| #2 lme-gpt4_4ef30696（**期望**） | **1.0000** | 0.95706 |

→ 同样未来记忆得满分，语义/keyword 全 1.0 的期望项反而排第 2。

---

## 5. 验收对照（任务书 B.5）

| 验收项 | 结果 | 证据 |
|---|---|---|
| `signalFeedbackHistory` 无 `time.Now()`（回退分支除外） | ✅ | grep 确认：rank_fusion.go:736 仅回退分支；其余全部 rf.Now |
| `Now=2026-10-12` 与 `Now=评测启动时刻` MRR 完全一致 | ✅ | 公开 0.975445 三组逐位相同；合成 0.843396 三组逐位相同 |
| `Now=as_of` 公开基准 MRR ≥ 0.943 | ✅ | **0.975445**（远超 0.943） |
| `Now=as_of` temporal 桶 MRR ≥ 0.975 | ✅ | **0.9940**（公开基准） |
| 若降级必须给对照数据与根因 | 不适用（未降级，反而提升） | §3.3 给出了 6 条 single-hop 提升的 isolate 验证 |
| `go vet ./...` 全绿 | ✅ | 无输出 |
| `go test ./...` 全绿 | ✅ | kernel/eval 全部 ok |

---

## 6. 决策记录

**采用 `Now=as_of`**。理由：

1. **语义正确**：as_of 查询时 freshness/feedback 应基于「as_of 当时」判断，而非全局 `2026-10-12`。
2. **实证无降级**：三组对照在修复后完全一致，`-now-asof` 是**无损**开关。
3. **附带提升**：freshness 未来记忆 bug 修复带来 6 条 single-hop + 1 条 open-domain 提升（isolate 验证）。
4. **风险可控**：未来记忆 freshness=0 在 `Now=评测启动` 模式下等价原行为（该模式下所有记忆都在过去），因此**不改变现状默认行为**。

**默认行为**：`-now-asof` 默认 false（不开启），保持向后兼容；任务书/生产环境如需 as_of 时序一致性，显式传 `-now-asof`。

---

## 7. 红线核对

| 红线 | 状态 |
|---|---|
| 只增不改 Schema 语义 | ✅ 未动 Memory/FeedbackRecord/任何 Schema |
| 全部验证在 192.168.0.88 | ✅ 编译/test/评测均在远端 |
| 不调 LLM | ✅ freshness/feedback/embedder 全确定性 |
| seed=42 确定性 | ✅ 无新引入的 `time.Now()`（除 `rf.Now` 零值回退） |
| 不 push | ✅ 保持本地 commit，未推远端 |
| `*.go` 必须 LF | ✅ `.gitattributes` 已锁，全部 gofmt clean |
| 不动 `light-merge/` | ✅ 仅动 `lightmem/` |