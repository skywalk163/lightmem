# R4-D 报告：合成集 multi-hop 样本重构方案与降级说明

> **日期**：2026-10-12 ｜ **实机**：192.168.0.88
> **背景**：R3-D §6 风险表 —— 合成集 multi-hop 主 MRR 恒 0.500 掩盖排序问题（口径 A 已 1.000，但主 MRR 仍 0.500）。本报告给出根因、重构方案、对比数据与推荐决策。

---

## 1. 根因（逐行定位）

合成集 multi-hop 样本由 `eval/eval_generate.go:115-168` 生成，链条为：

```
hop-1 (memID1 = mh1-XXX)："<entity>的决策人是决策人NNN"   ← 跳板
hop-2 (memID2 = mh2-XXX)："决策人NNN偏好<weekday>开会"      ← 最终答案
query："<entity>偏好什么时间开会（决策人：决策人NNN）"
ExpectedMemoryID = []string{memID1, memID2}   // ← hop1 在 [0]，hop2 在 [1]
```

评测器主 MRR 口径（`eval/run_kpi.go` rank 判定）：top-1 MemoryID == `ExpectedMemoryID[j]` 时取 `rank = j+1`，`RR = 1/rank`。

实测合成集 150 条 multi-hop 样本，检索器 top-1 **正确召回的总是 hop-2（最终答案 memID2）**：

| top-1 实际召回 | 在 Expected 中的位置 | 主 MRR 计法 | RR |
|---|---|---|---|
| hop-2 memID2（正确答案） | `Expected[1]`（j=1） | rank=2 | **0.5** |
| hop-1 memID1（跳板，仅在错误排序时出现） | `Expected[0]`（j=0） | rank=1 | 1.0 |

因此 150 条全部 RR=0.5 → **主 MRR 恒 0.500**。这不是检索器排序错误，而是**样本 Expected 数组的元素顺序与"检索目标=最终答案"的直觉相反**：把跳板 hop-1 放在了优先级更高的 `[0]` 位。

R3-D 口径 A（`sliceContains(Expected, top1ID)`，不区分顺序）实测 **150/150 = 1.000**，证明 top-1 始终正确命中最终答案——**检索能力在合成集上已饱和，0.500 纯粹是评分口径的结构性惩罚**。

---

## 2. 重构方案对比

### 方案 1：ExpectedMemoryID 改单元素（只留 hop-2）

`eval_generate.go:142` 改为 `ExpectedMemoryID: []string{memID2}`。

- 主 MRR：top-1=hop2 → j=0 → rank=1 → RR=1.0 → **1.000**（与口径 A 一致）。
- 优点：主 MRR 与口径 A 数字统一，消除"0.500 假象"。
- 缺点：
  - 需要重新生成 `eval/dataset/all.jsonl`，历史 R1/R2/R3 数字（0.500）不再可比。
  - hop-1 跳板记忆从 Expected 中消失，若未来要评测"跳板召回"能力则失去对照。
  - `Hop1Memory` 字段仍保留（物化用），但不参与打分。

### 方案 2：交换 Expected 顺序（hop-2 在 [0]，hop-1 在 [1]）

`eval_generate.go:142` 改为 `ExpectedMemoryID: []string{memID2, memID1}`。

- 主 MRR：top-1=hop2 → j=0 → RR=1.0 → **1.000**；若错误召回到 hop-1 → j=1 → RR=0.5。
- 优点：单元素方案的效果，且保留 hop-1 在 Expected 中（未来可评测跳板排序惩罚）。
- 缺点：同方案 1，需重新生成数据集，历史数字不可比。

### 方案 3：保留现状，主 MRR 口径加降级标注（推荐）

- 不改 `eval_generate.go`、不重新生成数据集。
- 在评测器摘要与报告中明确标注：**合成集 multi-hop 主 MRR 0.500 为样本结构下限（Expected 顺序惩罚），真实排序质量以口径 A（1.000）为准**。
- 公开基准不受影响（其 ExpectedMemoryID 159/165 为单元素，`toItems` 里 hop 顺序已是答案优先）。

---

## 3. 前后对比（若执行方案 1/2）

| 口径 | 现状（Expected=[hop1,hop2]） | 重构后（方案 1/2） | 说明 |
|---|---|---|---|
| 合成集 multi-hop 主 MRR | **0.500** | **1.000** | 消除顺序惩罚 |
| 合成集 multi-hop 口径 A | 1.000 | 1.000 | 不变（本就不区分顺序） |
| 合成集 multi-hop hit@2 | 1.000 | 1.000 | 不变 |
| 合成集总体 MRR | 0.8434 | ~0.8434 + Δ | multi-hop 占 150/570，Δ ≈ (1.0−0.5)×150/570 ≈ +0.132 |
| 与 R1/R2/R3 历史数字可比性 | 基线 | **断裂**（主 MRR 从 0.500 跳到 1.000） | 需在报告中标注口径版本 |

> 方案 1/2 会让合成集总体 MRR 从 0.843 跳到约 0.975，看似"大幅提升"，实为口径修正而非检索能力提升——**风险是误导后续轮次把口径变化误读为算法进步**。

---

## 4. 推荐决策：方案 3（降级说明，不改数据集）

**理由**：

1. **合成集检索能力已饱和**（口径 A 1.000、hit@2 1.000），重构主 MRR 数字不会带来新的诊断信息，只制造口径断裂。
2. **公开基准才是真实能力检验场**：其 multi-hop 口径 A 0.9455、9 条真实失败（R3-D §4.3）才是后续优化方向（R4-A hop 链式推理）。在合成集上凑数字无意义。
3. **历史可比性优先**：R1→R2→R3 的 0.500 已是基线，保留它可让后续轮次一眼看出"合成集 multi-hop 主 MRR 恒定"是已知结构，而非回归。
4. **R3-D 已落地口径 A/B**，决策方已有正确指标可用；主 MRR 作为历史口径保留即可。

**落地动作**（本报告即降级说明，无需代码改动）：

- 评测器摘要中 multi-hop 三口径并列展示（R3-D 已实现）。
- 后续所有引用合成集 multi-hop 主 MRR 0.500 的报告，需标注"样本结构下限，非检索能力"。
- P8 若需要"合成集 multi-hop 主 MRR 可读"，再执行方案 1/2 并 bump 数据集版本号（`dataset/v2/`），与 v1 数据并存。

---

## 5. 与公开基准的对照（为何公开基准没有此问题）

公开基准 `eval/benchmarks/sample.go:toItems` 把多证据问题展开时，`ExpectedMemoryIDs` 直接来自原始数据集（LoCoMo/BEAM/LME 的 ground truth），159/165 条为**单元素**（只有最终答案 ID），天然不存在 hop 顺序惩罚。其余 6 条多元素样本的顺序也由原始数据集决定，不出现"跳板排在答案前"的反转。

这进一步说明：0.500 是**合成集构造时的特有序问题**，不是评测器或检索器缺陷。

---

## 6. 复现命令（192.168.0.88）

```sh
cd /tmp/duan-light-merge/lightmem/eval
# 现状（R4-D 验证，embedder 抽象后逐位一致）
go run ./run_kpi.go -dataset ./dataset/all.jsonl \
  -report r4d_synth -weights ./weights/rank_weights_en.json
# → overall_mrr 0.8434, multi-hop 主 0.500, 口径 A 1.000, hit@2 1.000
```

证据：`eval/reports/r4d_synth_20261012-151102.json`。
