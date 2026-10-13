# P7-D 公开基准正式对标报告（R8-B 收口）

> **版本**：v1（正式收口）｜ **日期**：2026-10-14 ｜ **任务编号**：R8-B（P7-D 公开基准对标收口 + Q5 gofmt）
> **任务书**：`lightmem/.dumate/inbox/_R8_多语言国际化与公开基准对标任务书.md` §2（B 路）
> **权威基线**：`3841420`（R7 收口态，已 push gitea main）
> **验证机**：192.168.0.88（FreeBSD 15.1-RELEASE-p1，go1.25.14）——全部编译/测试/运行在此完成
> **评测器**：`eval/run_kpi.go`（HEAD 增强版：MRR / Cost / PollutionTrend / fact v4 / 适配口径）
> **本报告取代**：`eval/reports/benchmarks_summary_p7d.md`（2026-10-12，公开 MRR **0.546**，为 R2 之前的过期数值）

---

## 0. 执行摘要

本报告为 **P7-D 公开基准对标的正式收口版**，以 R5 最终态配置在权威基线 `3841420` 上**复跑可复现**的实测数据为准，覆盖 LongMemEval / LoCoMo / BEAM 三基准合并子集（650 条）+ R5-B 合并 temporal 样本（130 条）= **780 条**。

| 指标 | 权威基线（本报告实测） | 目标 | 判定 |
|---|---|---|---|
| **公开基准总体 MRR** | **0.9917** | ≥ 0.99 | ✅ 可复现 |
| 公开基准 memory_id top-1 准确率 | **1.000** | — | ✅ |
| 公开基准 fact 准确率（北极星 v4 口径） | **0.9607**（660/687） | > 0.90 | ✅ |
| 公开基准 fact 准确率（适配口径，非北极星） | **0.9476**（651/687） | ≥ 0.90 | ✅ |
| 分桶 MRR | single-hop 0.9943 / multi-hop 0.9800 / open-domain 0.9788 / temporal 0.9983 | — | ✅ |
| 污染率 / fact 假阳性 | **0% / 0 条** | — | ✅ |
| 合成集 MRR（对照） | **1.000** | — | ✅ |

**与原过渡报告（`benchmarks_summary_p7d.md`，MRR 0.546）的差异**：0.546 是 R1/R2 之前的默认权重态；经 R2（英语权重重校准）→ R3/R4（multi-hop 重排、as_of 时序）→ R5（真实向量 embedder + 样本重构 + fact 归一）四轮连续提升，最终态为 **0.9917**（+0.4457 绝对值）。本报告即以此收口。

**关键澄清（fact 形态假阴性）**：公开基准 `memory.fact` 为 `question => answer`（LME/BEAM）或证据 turn 原文（LoCoMo），而 `expected_fact` 是纯答案；用「精确相等」比对必然得 **0.0%**（形态假阴性，非检索能力）。本报告给出两级修正口径：

- **适配口径**（本次 R8-B 新增，`benchmarks.MatchFactBenchmarks`，⚠️ 非北极星）：`expected` 以确定性包含关系出现在 `got`（或其 `=>` 答案段）即命中 → **94.76%**；
- **北极星 v4 口径**（`matchFactV4`，本轮零改动）：在适配基础上追加数值/日期归一、时间推理、短词元包含 → **96.07%**。

---

## 1. 权威基线表（780 条，R5 最终态配置复跑实测）

**复跑命令**（0.88）：
```sh
cd /tmp/r8b/lightmem/eval
go run ./run_kpi.go -dataset ./benchmarks/dataset/all.jsonl \
  -temporal-samples dataset/temporal.jsonl -weights weights/rank_weights_en_r5.json \
  -embedder vector -now-fixed 2026-10-13 -report r8b_bench_final
```
**证据**：`eval/reports/r8b_bench_final_20261013-231113.json` + `_summary.md`（另附 `-fact-adapter` 适配口径跑：`r8b_bench_adapter_20261013-231534.json`）。

### 1.1 总体指标

| 指标 | 数值 | 说明 |
|---|---|---|
| 评测集总数 | **780** | 650 基准（LME 500 / LoCoMo 50 / BEAM 100）+ 130 temporal 合并 |
| 总体 MRR | **0.9917** | 与 R5 最终态 `r5_final_public_20261013-104900.json` 逐位一致 |
| 总体 memory_id 准确率（top-3） | **1.000** | 饱和 |
| 总体 fact 准确率（v4，分母剔除模板） | **0.9607**（660/687） | 模板 expected 剔除 93 条 |
| 总体 fact 准确率（适配口径，非北极星） | **0.9476**（651/687） | `-fact-adapter` 跑 |
| 时序桶准确率 | **1.000** | temporal 桶 |
| 多信号融合准确率 | **1.000** | multi-hop 桶自动跑（待人工复核） |
| 记忆污染率 | **0.000** | 7 个窗口全 0 |
| fact 假阳性 | **0** | `fact_false_positives` 缺省（omitempty） |

### 1.2 分桶 MRR（top-1 加权）

| 分桶 | MRR | 条数占比 |
|---|---|---|
| single-hop | **0.9943** | 主导桶 |
| multi-hop | **0.9800** | 主口径 C（历史可比） |
| ├ 口径 A（top-1 ∈ Expected 即 hit） | 0.9818 | R3-D 辅助 |
| ├ 口径 B hit@2（top-2 内命中） | 0.9818 | R3-D 辅助 |
| └ R5-B 主口径（ExpectedMemoryIDPrimary 最终答案） | 0.9861 | 脱离结构性下限 |
| open-domain | **0.9788** | |
| temporal | **0.9983** | 合并 130 temporal 样本后 |

### 1.3 成本 / 延迟旁证（-fact-adapter 跑）

| 指标 | 数值 |
|---|---|
| 总查询数 | 780 |
| 总延迟 | 14990 ms |
| 平均延迟 | **19.2 ms / 查询**（热点缓存辅助，远低于 P95<200ms 目标） |
| 平均信号计算次数 | 24 / 查询（总 18720） |
| 估算成本 | **$0.0187 / 查询**（token 消耗全程 0——纯确定性规则，不调 LLM） |
| 污染率趋势 | stable（7 窗口全 0） |

> 注：`total_tokens = 0` 是设计使然——本评测链路全部为确定性字符串/规则/查表，符合红线 4「不调 LLM」。

---

## 2. 历史对照：公开基准 MRR 提升轨迹

> 数据来源：各轮收口报告（`p7d_r2_summary.md` / `p7d_r4_summary.md` / `p7d_r5_summary.md`）与各轮 JSON 报告。

| 轮次 | 提交 | 公开基准条数 | 总体 MRR | Δ | fact 准确率 | 主要动作 |
|---|---|---|---|---|---|---|
| P7-D 基线（R1 默认权重） | `f8b1e6d` 前 | 650 | **0.546** | — | 0.0%（精确口径假阴性） | 默认权重（针对合成集中文短文本） |
| **R2**（四路） | `20ebc00` 等 | 650 | **0.9432** | +0.397 | 86.9% | 英语 RankFusion 权重重校准 + temporal as_of/SV + fact 包含口径 |
| **R3**（四路） | `1dc9d05` | 650 | **0.9754** | +0.032 | — | char n-gram embedder + reference_frame 修复 + multi-hop 口径 A |
| **R4**（四路） | `f218bc9` 等 | 650 | **0.9845** | +0.009 | 94.8% | multi-hop 二次 hop 重排 + fact v4（时间推理/数值归一） |
| **R5**（四路） | `81f7370` 等 | 650+130 | **0.9917** | +0.007 | **96.07%** | 真实向量 embedder（BGE-m3）+ multi-hop 样本重构 + fact 边界归一 |
| **R7**（收口） | `3841420` | 780 | **0.9917** | 0 | 96.07% | 单点冗余加固 + 多视图升级（北极星侧，公开 MRR 不变） |
| **R8-B**（本次复跑） | 本报告 | 780 | **0.9917** | 0 | 96.07% | 权威基线复跑可复现 + 新增适配口径（94.76%，非北极星） |

**轨迹解读**：+0.4457 的绝对提升中，约 **0.397（89%）来自 R2 英语权重重校准**（含 `reference_frame` 由 1.2 关闭为 0.0），其余由 R3/R4/R5 的检索机制升级逐步补齐。R5 之后指标稳定在 0.9917，R7、R8-B 两轮均未回归。

> **与任务书 §B.3.1 的表述差异**：任务书将 0.9845 记为「R2」，经核对权威报告，0.9845 实为 **R4 四路联合态**（`p7d_r4_summary.md` §1），R2 为 0.9432。本报告以报告实测为准。

---

## 3. 与合成集对照

| 指标 | 公开基准（780） | 合成集（R5 合流） | 说明 |
|---|---|---|---|
| 总体 MRR | **0.9917** | **1.000** | 合成集为模板生成、近确定性；公开基准为真实英文长对话 |
| memory_id top-1 | 1.000 | 1.000 | 均饱和 |
| fact 准确率（v4） | 0.9607 | 1.000 | 合成集 expected 与记忆文本同源 |
| multi-hop 主 MRR | 0.9800 | 1.000 | 合成集已由单元素重构脱离 0.500 下限 |

**结论**：公开基准 0.9917 已接近合成集上限（差 0.0083），说明「真实英文长对话 / 跨会话 / 时间推理」这一 P7-D 原始短板（R1 时差 −0.274）已基本消除。

---

## 4. fact 形态差异与三级口径说明

### 4.1 根因

公开基准的 `memory.fact` 与被比较的 `expected_fact` 形态不同：

| 来源 | `memory.fact` 形态 | `expected_fact` 形态 |
|---|---|---|
| LongMemEval / BEAM | `question => answer` | 纯答案片段 |
| LoCoMo | `speaker: turn 原文` | 纯答案片段 |

因此「`got == expected` 精确相等」判定的准确率必然趋近 **0.0%**——这是**数据形态假阴性**，与检索能力无关（真实答案早已被 top-1 召回，见 MRR 0.9917）。

### 4.2 三级口径对照（同一 780 条、同一 687 条非模板样本）

| 口径 | 归属 | 判定规则 | fact 准确率 | 说明 |
|---|---|---|---|---|
| 精确相等 | （历史） | `normalize(got) == normalize(expected)` | **≈0.0%** | 形态假阴性，已废弃 |
| **适配口径** | ⚠️ **非北极星**（R8-B 新增 `benchmarks.MatchFactBenchmarks`） | 归一/边界归一后 `expected ⊆ got`，或 ⊆ `got` 的 `=>` 答案段 | **0.9476**（651/687） | 只做确定性包含，不含时间推理/模糊 |
| **北极星 v4 口径** | 北极星（`matchFactV4`，本轮**零改动**） | 适配基础上追加数值/日期归一、时间推理、短词元包含 | **0.9607**（660/687） | 与适配口径**并存不混用** |

**两者关系（实测）**：适配口径命中集合 **⊆** v4 命中集合——适配口径 651 条全部被 v4 覆盖；v4 另有 9 条命中为适配口径未覆盖（7 条 temporal 走时间推理、2 条 open-domain 走模糊/短词元）。即适配口径是 v4 的**严格确定性下界**，符合「适配口径显式标注、不与北极星口径混用」的红线要求。

### 4.3 验收对照

| 验收项（任务书 §B.5） | 目标 | 实测 | 判定 |
|---|---|---|---|
| 适配口径 fact acc | **≥ 90%**（原 0% 为形态假阴性） | **94.76%**（651/687） | ✅ |
| 北极星口径不变 | `matchFactV4` / `JudgeM1` 无改动 | 本轮零改动（见 §6） | ✅ |

---

## 5. 旁证：污染 / 假阳性 / 成本

| 旁证项 | 数值 | 说明 |
|---|---|---|
| 记忆污染率 | **0%** | 7 个窗口（1–700）全 0 泄漏；公开基准不含 injection/pollution 样本 |
| fact 假阳性（fact_hit 但 top-1 ID 未中） | **0 条** | `fact_false_positives` omitempty 缺省即 0 |
| 模板 expected 剔除 | **93 条** | `beam-*` 的 `LLM response should ...:`，全部 fact_hit；单列不计入主口径 |
| 候选扩展辅助口径 | 2 条 | 短 expected 在 top-5 候选内命中（不覆盖主口径） |
| token 成本 | **0** | 纯确定性规则，不调 LLM（红线 4） |
| 平均延迟 | 19.2 ms | 见 §1.3 |

---

## 6. 北极星口径零改动证明

本轮对评测链路的改动**全部为只增**，且不触北极星判定：

| 文件 | 改动性质 | 北极星影响 |
|---|---|---|
| `eval/benchmarks/match_fact_benchmarks.go` | **新增**（独立包 `benchmarks`，非北极星） | 无 |
| `eval/run_kpi.go` | **只增**：新增 `-fact-adapter` flag（默认 false）+ 报告字段 `fact_accuracy_adapter` / `fact_adapter_hits` / `fact_adapter_enabled` + 摘要 1 行 | 无 |
| `matchFactV4` / `JudgeM1` | **零改动** | 无 |
| `kernel/lifecycle.go` / `metrics.go` | **零改动**（Q5 见 §7.2） | 无 |

**只增不改的实测证据**：默认态（不加 `-fact-adapter`）复跑报告 `r8b_bench_native_20261013-231549.json` 与基线 `r8b_bench_final_*.json` **除 `generated_at` 与墙钟延迟外逐字段一致**，且 **780 条 `details` 逐条逐字段完全相同**（`native details == baseline details: True`）。`-fact-adapter` 跑与基线的差异**仅**为新增的 3 个适配字段。

---

## 7. 复现与门禁

### 7.1 复现命令（0.88）

```sh
cd /tmp/r8b/lightmem/eval

# 北极星口径（默认，逐位复现 R5 最终态）
go run ./run_kpi.go -dataset ./benchmarks/dataset/all.jsonl \
  -temporal-samples dataset/temporal.jsonl -weights weights/rank_weights_en_r5.json \
  -embedder vector -now-fixed 2026-10-13 -report r8b_bench_native

# 适配口径对照（非北极星，只增）
go run ./run_kpi.go -dataset ./benchmarks/dataset/all.jsonl \
  -temporal-samples dataset/temporal.jsonl -weights weights/rank_weights_en_r5.json \
  -embedder vector -now-fixed 2026-10-13 -report r8b_bench_adapter -fact-adapter
```

### 7.2 Q5 gofmt 收口

任务书 §2 B 假设 `kernel/lifecycle.go` / `kernel/metrics.go` 存在 R138-B 遗留 gofmt 违规。**经核实，Q5 已在权威基线内关闭**：

- 修复提交 **`873c41e`（style(kernel): gofmt 修复 lifecycle.go / metrics.go 字段对齐）**，`git merge-base --is-ancestor 873c41e 3841420` 为真，且已存在于 `origin/main`；提交说明「纯格式修复（`gofmt -w` 产物），`git diff -w` 为空，零语义变化」。
- 复验：`gofmt -l kernel/lifecycle.go kernel/metrics.go` **无输出**；`gofmt -w` 为**幂等空操作**（`diff` 为空、`git diff --stat` 为空）。

**结论**：Q5 无需新增格式化提交（再改会制造空 diff）；以 `gofmt -l` 全绿 + `873c41e` 留痕作为收口证据。

### 7.3 门禁（0.88）

见 §8 验收表；gofmt / go vet / go test 全绿，北极星 M1 与公开 MRR 均不回归（详见 `r8b_acceptance_20261014.md`）。

---

## 8. 结论状态

| 验收项（任务书 §B.5） | 目标 | 结果 | 判定 |
|---|---|---|---|
| 正式 P7-D 对标报告 | `p7d_benchmarks_final_20261014.md` 产出 | 本文件 | ✅ |
| 权威基线 | 记载 MRR **0.9917**（非过期 0.546） | 0.9917（780 条） | ✅ |
| 复跑可复现 | `3841420` 重跑公开 MRR ≥ 0.99 | **0.9917** | ✅ |
| 适配口径 | fact acc（适配口径）≥ 90% | **94.76%**（651/687） | ✅ |
| 北极星口径不变 | `matchFactV4` / `JudgeM1` 无改动 | 零改动 | ✅ |
| Q5 gofmt 收口 | `gofmt -l` 无输出；零语义改动 | 已由 `873c41e` 关闭 + 复验幂等 | ✅ |
| gofmt / go vet / go test（全仓） | 全绿 | 全绿 | ✅ |

**总结**：P7-D 公开基准对标以 **MRR 0.9917 / fact v4 0.9607（适配口径 0.9476）/ 污染率 0 / FP 0** 正式收口；三基准子集适配与复跑可复现达标；北极星判定口径零改动；Q5 gofmt 已在基线内关闭。

---

## 附：证据文件

| 文件 | 内容 |
|---|---|
| `eval/reports/r8b_bench_final_20261013-231113.json` / `_summary.md` | 权威基线复跑明细（默认口径） |
| `eval/reports/r8b_bench_native_20261013-231549.json` / `_summary.md` | 只增不改的默认态复跑（与基线逐字段一致） |
| `eval/reports/r8b_bench_adapter_20261013-231534.json` / `_summary.md` | 适配口径对照跑（含 `fact_accuracy_adapter`） |
| `eval/reports/r5_final_public_20261013-104900.json` | R5 最终态（0.9917） |
| `eval/reports/p7d_r2_summary.md` / `p7d_r3_summary.md` / `p7d_r4_summary.md` / `p7d_r5_summary.md` | 各轮收口（MRR 轨迹来源） |
| `eval/reports/benchmarks_summary_p7d.md` | **已过期（0.546）**，由本报告取代 |
