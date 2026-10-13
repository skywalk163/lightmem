# P7-D 公开基准对标报告（合成 + 公开双源）

> ⚠️ **已过期（SUPERSEDED）**：本报告记录公开 MRR **0.546**，为 R2 之前的默认权重态。
> 经 R2→R3→R4→R5 四轮升级，公开基准最终态为 **MRR 0.9917 / fact v4 0.9607**（780 条，含 temporal 合并）。
> **正式收口报告见** `eval/reports/p7d_benchmarks_final_20261014.md`。本文件仅作历史留痕，勿再引用其数值。

> 生成时间：2026-10-12 ｜ 任务：P7-D 公开基准对标
> 实机：192.168.0.88（freebsd/amd64，go1.25.14）｜ 评测器：`eval/run_kpi.go`（HEAD 增强版，含 MRR/Cost/PollutionTrend）
> 前置：R1 外部基准数据集（`eval/benchmarks/dataset/`，650 条）已交付；合成 KPI 集 `eval/dataset/all.jsonl`（570 条）

## 1. 结论

**公开基准 MRR 0.546，显著低于合成集的 0.821（差 -0.274），暴露英文长对话 / 跨会话 / 时间推理场景的检索排序短板。**

评测器 HEAD 增强版将 memory_id 命中判定放宽到 top-3，故两端 acc 均饱和到 100%；**MRR 才是排序质量核心指标**（top-1 命中权重最大），最能区分真实检索能力。

| 指标 | 合成 KPI | 公开基准 | 差距 |
|---|---|---|---|
| 评测集总数 | 530 | 650 | +120 |
| **总体 MRR** | **0.821** | **0.546** | **-0.274** |
| 总体准确率（memory_id，top-3） | 100.0% | 100.0% | 饱和 |
| 总体准确率（fact 文本） | 93.0% | 0.0%* | 数据口径差异 |
| 时序桶准确率 | 100.0% | 100.0% | 饱和 |
| 多信号融合 | 100.0% | 100.0% | 饱和 |
| 记忆污染率 | 0.0% | 0.0% | — |
| 每条查询成本（估算） | $0.0137 | $0.0156 | +14% |
| 平均延迟（ms） | 10.9 | 13.1 | +20% |

> *fact 文本 0% 是**数据形态差异，非检索能力**：公开基准 memory.fact 为 `question => answer`（LME/BEAM）或证据 turn 原文（LoCoMo），而 expected_fact 是纯答案，评测器按 `gotFact == expectedFact` 精确比较必然 0%。实际答案已被召回（见 MRR），仅拼接格式不同。

## 2. 分桶 MRR

| 分桶 | 合成 MRR | 基准 MRR | 差距 |
|---|---|---|---|
| single-hop | 0.890 | 0.539 | -0.351 |
| multi-hop | 0.500 | 0.558 | +0.058 |
| temporal | 0.988 | 0.552 | -0.436 |
| open-domain | 0.980 | 0.527 | -0.453 |

**temporal / open-domain 桶 MRR 差距最大（-0.44 / -0.45）**：时间推理需 as_of 回溯历史版本，当前 `recorded_at` 仅 550/650 有效（BEAM 无时间字段），排序时 freshness 信号无法正确区分版本。

**multi-hop 桶合成集反而更低（0.500 vs 0.558）**：合成集多跳样本设计为 hop-1/hop-2 分离，top-1 命中概率天然受限，MRR 反映这一结构性约束。

## 3. 成本与延迟

| 指标 | 合成 | 基准 | 说明 |
|---|---|---|---|
| 每条查询成本（估算） | $0.0137 | $0.0156 | 基准集英文长文本 token 更多 |
| 平均延迟 | 10.9 ms | 13.1 ms | 基准集候选数更大 |

## 4. 关键观察

- **MRR 差 -0.274 是真实排序能力下限**：合成集 0.821 vs 基准 0.546，差距主因：
  1. **英文语义检索**：LME/BEAM 英文长文本，RankFusion 默认权重（针对合成集中文短文本）在英文上匹配弱。
  2. **长对话记忆量**：LME 单会话 500 条记忆 + 多 session 跨时间，`kpiRecallTopK=200`（maxScan=8000）已覆盖全库，但 top-1 排序仍难命中正确答案。
  3. **事实形态差异**：基准 memory.fact 是 `question => answer` 或原文 turn，文本相似度信号与 expected_fact（纯答案）天然错位。
- **污染率 0%**：公开基准不含 injection/pollution 样本，符合预期。
- **memory_kind 全为 fact**：基准集未区分 kind，MRR 未受 kind 信号影响。

## 5. 评测链路说明

- 评测器 `run_kpi.go` 本次新增 `-dataset` / `-report` 参数（P7-D 复用同一链路，不另造评测器）：
  ```sh
  cd /tmp/duan-light-merge/lightmem/eval
  go run ./run_kpi.go -dataset ./benchmarks/dataset/all.jsonl -report benchmarks
  ```
- 报告落盘：`eval/reports/benchmarks_20261012-041127.json`（650 条明细，含 cost/MRR/pollution_trend）+ `benchmarks_summary.md`。
- 合成基线回归（默认参数）产出：acc(mem_id)=100.0%、acc(fact)=93.0%、MRR=0.821、污染率 0.0%，与既有 `f8b1e6d` 报告口径一致，参数化未破坏原行为。
- **注意**：HEAD 增强版将 memory_id 命中放宽到 top-3，故 accuracy 两端饱和到 100%，**不可再作为差异指标**；请以 MRR 为准。

## 6. 后续建议（P7-D 延伸）

1. **以 MRR 作为对标核心指标**：acc 已饱和，MRR（top-1 命中权重最大）才能区分真实检索能力。
2. **RankFusion 权重针对英文长文本重校准**：当前默认权重基于合成集（中文短文本），需针对 LME/BEAM 英文长对话回归调参，重点提升 top-1 排序。
3. **时态桶强化**：为 LME/BEAM temporal 样本补 `as_of` 与双版本记忆（`SecondVersion`），使 as_of 过滤能真实验证，提升 temporal MRR。
4. **fact 评测口径修正**：对公开基准改用「got 包含 expected 文本」判定（非精确相等），以正确度量检索能力（当前 0% 是假阴性）。
5. **memory_kind 区分**：当前全为 `fact`，下游若需按 kind 检索需在适配器加规则推断（纯规则，不调 LLM）。

## 7. 结论状态

- 合成 KPI：acc 100.0%（top-3）/ 93.0%（fact 精确）✅，MRR 0.821
- 公开基准：acc 100.0%（top-3）/ 0.0%（fact 精确，口径假阴性）❌，**MRR 0.546**
- 双源结论：**合成集 MRR 0.821 是「中文短文本 + 精确匹配」的乐观估计；公开基准 MRR 0.546 揭示英文长对话 + 时间推理的真实短板，P7-D 需针对此方向调优**。
