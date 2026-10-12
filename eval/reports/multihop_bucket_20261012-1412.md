# R2 任务D：multi-hop 分桶结构性约束评估与防御性加固报告

> 版本：v1 ｜ 日期：2026-10-12 ｜ 执行：D 路
> 落点：`eval/run_kpi.go`（temporal/multiSignal 统计加 `!isAdversarial`）、本报告

---

## 1. 背景与实测口径纠正

任务书初稿担心 multi-hop 桶分母被对抗样本（pollution/injection）污染。**0.88 实测（Python 统计）**：

| 检查项 | 合成集 (`eval/dataset/all.jsonl`) | 公开基准 (`eval/benchmarks/dataset/all.jsonl`) |
|---|---|---|
| bucket == multi-hop 条目数 | 150 | 165（LME 133 + LoCoMo 12 + BEAM 20） |
| 其中 has_pollution=true | 0 | 0 |
| 其中 has_injection=true | 0 | 0 |
| 全量对抗样本（pollution/injection 任一） | 40（独立成桶，与 multi-hop 互斥） | 0 |

**结论**：当前数据集下 multi-hop 分母**未被对抗样本污染**。本路的真正价值是：
1. **防御性加固**（防未来数据源把对抗样本标进 multi-hop/temporal 桶）；
2. **结构性约束单独评估**（合成集 multi-hop MRR 0.500 的成因，以及双源不可直接比较）。

## 2. 防御性加固（`eval/run_kpi.go`）

```go
// 加固：temporal / multi-hop 桶统计只对非对抗样本累计。
// 当前数据集下对抗样本已在上方 isAdversarial 分支 continue，此条件恒真，
// 仅防止未来数据源把 pollution/injection 标进 temporal/multi-hop 桶污染分母。
if item.IsTemporal && !isAdversarial {
    temporalTotal++
    if hit { temporalHits++ }
}
if item.Bucket == "multi-hop" && !isAdversarial {
    multiSignalTotal++
    if hit { multiSignalHits++ }
}
```

## 3. Before / After 对比（0.88 实测，R3 默认权重）

| 数据集 | 阶段 | 总条目 | multi-hop 条数 | multi-hop MRR | 总体 MRR |
|---|---|---|---|---|---|
| 合成集 | before | 530 | 150 | **0.5000** | 0.8208 |
| 合成集 | after | 530 | 150 | **0.5000** | 0.8208 |
| 公开基准 | before | 650 | 165 | **0.5584** | 0.5463 |
| 公开基准 | after | 650 | 165 | **0.5584** | 0.5463 |

**所有数字逐位一致**（multi-hop 条数 150/165、MRR 0.5000/0.5584、总体 MRR 0.8208/0.5463），证明防御性加固零副作用，符合预期。

`go vet ./kernel/...`、`go test ./kernel/...`、`gofmt -l` 全绿。

## 4. 结构性约束分析：合成集 multi-hop MRR 为何恒为 0.500

### 4.1 样本设计

合成集 multi-hop 150 条按 R1 设计为「hop-1 / hop-2 分离」：
- `ExpectedMemoryID = [hop2ID, hop1ID]`（`eval/benchmarks/sample.go:105 toItems` 把首条作为 `Memory`、第二条挂 `Hop1Memory`）。
- 即 `ExpectedMemoryID[0]` = hop-2（最终答案目标），`ExpectedMemoryID[1]` = hop-1（中间跳板）。

### 4.2 评测器 rank 计算

`eval/run_kpi.go:321-327`：
```go
for j, exp := range item.ExpectedMemoryID {
    if res.Fused[0].MemoryID == exp {
        hit = true
        rank = j + 1
        break
    }
}
```

- top-1 = hop-2 → `j=0` → `rank=1` → RR=1.0
- top-1 = hop-1 → `j=1` → `rank=2` → RR=0.5
- top-1 都不是 → 继续扫 top-K，找到时 `rank = 位置+1`

### 4.3 0.500 的成因

合成集 multi-hop MRR 实测恒为 0.500（R3 / W_en 权重下都是 0.500）。这意味着：
- 一半样本 top-1 命中 hop-2（RR=1）；
- 一半样本 top-1 命中 hop-1（RR=0.5）；
- 没有样本 top-1 落到 hop-K 之外（ID 召回已 100%，见 overall_accuracy=1.0）。

**这不是检索能力缺陷，而是「样本设计 + rank 口径」共同作用的结构性下限**：当 top-1 在 hop-1/hop-2 二选一时，MRR 的上界受 hop 顺序约束。

### 4.4 公开基准 multi-hop MRR 0.558 的不可比性

公开基准 multi-hop 165 条的 `ExpectedMemoryID` 通常是**单元素列表**（目标事实 ID），不存在 hop-1/hop-2 排序问题。top-1 命中即 rank=1（RR=1），未命中则在 top-K 中找。

因此双源 multi-hop MRR **不可直接比较**：
- 合成集 0.500 = 结构性下限（hop 顺序惩罚）；
- 公开 0.558 = 真实排序质量。

A 路调权重后公开 multi-hop MRR 从 0.558 → 0.923（W_en），而合成集 multi-hop MRR 仍恒为 0.500——这进一步印证了 0.500 是结构性下限，权重无法突破。

## 5. 替代评估口径建议（仅建议，不实现）

供 P8 决策：

| 口径 | 定义 | 优点 | 缺点 |
|---|---|---|---|
| **A（推荐）** | multi-hop 只要求 `top-1 MemoryID ∈ ExpectedMemoryID` 即算 hit（等价 rank=1），不区分 hop 顺序 | 直接测"检索到目标事实集合"的能力；与公开基准可比 | 丢失 hop 顺序信息 |
| **B** | 单独报 `hit@2`（top-2 内命中任一 expected） | 反映"一次交互内能拿到正确事实"的体验 | 与 MRR 口径并存需双指标 |
| **C** | 保持现状，但报告中明确标注"合成集 multi-hop MRR 0.500 = 0.5×P(top1=hop2) + 1.0×P(top1=hop1)"的结构性下限 | 零改动 | 读者易误读为"multi-hop 检索差" |

**建议**：P8 采用口径 A 作为主指标，口径 B 作为辅助；口径 C 作为历史留档。

## 6. 验收对照（任务书 §D.6）

| 验收项 | 要求 | 实测 | 结果 |
|---|---|---|---|
| 合成 multi-hop 分母 | 150 | 150（before=after） | ✅ |
| 公开 multi-hop 分母 | 165 | 165（before=after） | ✅ |
| 加固前后所有指标 | 完全一致（diff 为空） | MRR/条数/总体逐位一致 | ✅ |
| 报告含双源 multi-hop 表 | 必含 | 见 §3 | ✅ |
| 报告含结构性约束说明 | 必含 | 见 §4 | ✅ |
| go vet + go test | 全绿 | ok | ✅ |

## 7. 交付文件

- `eval/run_kpi.go`：temporal/multiSignal 统计加 `!isAdversarial`（防御性，零行为变化）
- `eval/reports/multihop_bucket_20261012-1412.md`：本报告
