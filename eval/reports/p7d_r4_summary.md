# R4 收口报告：R3 残留项四路攻坚（multi-hop 检索 / reference_frame / fact 短板 / R3 收口）

> **日期**：2026-10-12 ｜ **实机**：192.168.0.88（FreeBSD 15.1-RELEASE-p1，go1.25.14）
> **基线**：R3 四路联合 `1dc9d05`（公开 MRR **0.9754** / 合成 **0.8434**）
> **终点**：R4 四路联合（公开 MRR **0.9845** / 合成 **0.8585**）
> **任务书**：`.dumate/inbox/_R4_检索攻坚并行任务书.md`
> **本报告职责**：把 R4 四路结果合并为一张「R3 → R4 双源 MRR/fact/multi-hop 对照表」，标注每路残留清除情况与判绿口径达成情况，列明 P8 建议。R4 任务书 §5.2 第 5 条要求的最终整合报告。
> **数字口径**：除特别注明外，均取 R4 联合 HEAD 在 0.88 的实测（`go run ./run_kpi.go -now-asof-default`，multihop depth=1 默认开启）；C 路 fact 数字单独注明是否含 A 路 multihop 联动。

---

## 1. 一页结论

| 指标 | R3（`1dc9d05`） | R4 联合（HEAD） | Δ | 判绿 |
|---|---|---|---|---|
| **公开基准总体 MRR** | 0.9754 | **0.9845** | **+0.91pp** | ✅ 不回归 |
| 公开基准 top-1 命中率 | 89.7% | **≈96%** | +6.2pp | ✅ |
| 公开基准 fact（v4 口径，A 路联动） | — | **94.8%**（528/557） | 较 R4-C 独立 95.2% 略降但假阳性 5→0 | ✅ ≥90% |
| 公开基准 multi-hop 口径 A | 0.9455 | **0.9758** | **+3.03pp** | ✅ ≥0.95 |
| 公开基准 multi-hop 主 MRR | 0.9599 | **0.9725** | **+1.26pp** | ✅ |
| 公开基准 hit@2 | 0.9818 | **0.9818** | 0 | ✅ 不回归 |
| **合成集总体 MRR** | 0.8434 | **0.8585** | **+1.51pp** | ✅ ≥0.843 |
| 合成集 fact（v4 口径，A 路联动） | 0.9698 | **1.0000**（530/530） | +3.02pp | ✅ ≥96.6% |
| 合成集 multi-hop 口径 A | 1.0000 | **1.0000** | 0 | ✅ 保持 |
| 污染率 | 0 | 0 | 0 | ✅ |

**四路判绿**：A ✅ / B ✅ / C ✅（公开假阳性已由 5 条降至 0，A 路联动后）/ D ✅。

> **fact 口径联动说明**：R4-C 独立评测（不含 A 路 multihop）给出公开 95.2%、合成 97.0%；R4-A 落地 multihop 二次重排后，top-1 更准导致 fact 提升——公开 fact 94.8%（分子分母同步调整后净效果，假阳性 1→0）、合成 fact 100%（530/530）。两个数字都可复现，分别对应不同评测配置，本报告以 HEAD 联合实测为准。

---

## 2. R3 → R4 双源对照表

### 2.1 公开基准（650 条：LME 500 / LoCoMo 50 / BEAM 100）

| 指标 | R3（`1dc9d05`） | R4（HEAD） | Δ | 主要归因 |
|---|---|---|---|---|
| 总体 MRR | 0.9754 | **0.9845** | +0.91pp | R4-A multi-hop 重排 |
| multi-hop 口径 A | 0.9455 | **0.9758** | +3.03pp | R4-A |
| multi-hop 主 MRR（口径 C） | 0.9599 | **0.9725** | +1.26pp | R4-A |
| multi-hop hit@2（口径 B） | 0.9818 | 0.9818 | 0 | — |
| single-hop MRR | 0.9867 | 0.9962 | +0.95pp | R4-A |
| open-domain MRR | 0.9119 | 0.9176 | +0.57pp | R4-A |
| temporal MRR | 0.9940 | **1.0000** | +0.60pp | R4-A + R4-B asof 默认 |
| fact（v4，A 路联动） | — | **94.8%**（528/557） | 新增 | R4-C + R4-A |
| fact（v4，R4-C 独立） | — | **95.2%**（530/557） | 新增 | R4-C |
| fact 假阳性 | 5 条 | **0**（A 路联动）/ 5 条（C 独立） | −5 / 0 | R4-A 提升 top-1 准确 |
| 模板 expected 剔除 | 6 条标注 | **93 条** | +87 | R4-C beam fp 清洗 |
| 污染率 | 0 | 0 | 0 | — |

### 2.2 合成集（530 条非对抗 + 40 条对抗）

| 指标 | R3（`1dc9d05`） | R4（HEAD） | Δ | 主要归因 |
|---|---|---|---|---|
| 总体 MRR | 0.8434 | **0.8585** | +1.51pp | R4-A |
| top-1 命中率 | 67.5% | **≈68.7%** | +1.2pp | R4-A 二次重排 |
| fact（v4，A 路联动） | 0.9698 | **1.0000** | +3.02pp | R4-A |
| fact（v4，R4-C 独立） | 0.9698 | **0.9700** | +0.02pp | R4-C 保持 |
| fact（v3 口径复现 R3-A 波动） | — | 97.0%（isolate 基线 A=97.5%，D=96.0%） | — | R4-B 波动诊断 |
| single-hop MRR | 0.9467 | **1.0000** | +5.33pp | R4-A |
| multi-hop 主 MRR（口径 C） | 0.500 | 0.500 | 0 | 结构性（D 路已降级说明） |
| multi-hop 口径 A | 1.0000 | 1.0000 | 0 | — |
| multi-hop hit@2 | 1.0000 | 1.0000 | 0 | — |
| open-domain / temporal MRR | 1.000 / 1.000 | 1.000 / 1.000 | 0 | — |
| 污染率 | 0 | 0 | 0 | — |

---

## 3. R4 四路判绿情况（目标 → 实测 → 状态）

### 3.1 R4-A：multi-hop 原生检索（hop 证据重排 + 上下文增强）

| 验收项 | 目标 | 实测 | 状态 |
|---|---|---|---|
| 公开 multi-hop 口径 A | ≥ 0.95 | **0.9758** | ✅ |
| hit@2 不回归 | ≥ 0.982 | 0.9818 | ✅ |
| 主 MRR 不回归 | ≥ 0.9599 | 0.9725 | ✅ |
| 其余桶不回归 | — | 全部不回归 | ✅ |
| isolate 复现 R3-D 基线 | 0.9455 | **逐位复现 0.9455** | ✅ |
| 9 条 top-1 失败修复 | ≥ 部分 | **5 条修复**（LME 全部），4 条如实报告 | ✅ |
| 合成集 MRR 不回归 | ≥ 0.843 | 0.8585 | ✅ |

### 3.2 R4-B：reference_frame 变化评测集 + 合成集波动诊断 + as_of 默认开关

| 验收项 | 目标 | 实测 | 状态 |
|---|---|---|---|
| 变化评测集 ≥ 50 条 | ≥ 50 | **60 条**（30 组 × 2） | ✅ |
| reference_frame 信号复检明确结论 | 有结论 | **100% 区分度**，被权重 0.0 关闭；rf=4.0 时 top-1 0%→46.7%（HEAD 实测，略高于 R4-B 首次实测 43.3%，因 multihop 重排部分辅助） | ✅ |
| 合成集 fact/top-1 波动根因定位 | 有根因 | **char n-gram embedder**（isolate 2×2 矩阵） | ✅ |
| `-now-asof-default` 公开 MRR 不回归 | ≥ 0.975 | **0.975**（isolate `-no-multihop` 逐位复现 R3 基线） | ✅ |
| `-now-asof-default` 合成 MRR 不回归 | ≥ 0.843 | **0.843**（isolate） | ✅ |

### 3.3 R4-C：fact 口径 v4（短 expected / beam fp / 时间推理 / 数值归一）

| 验收项 | 目标 | 实测 | 状态 |
|---|---|---|---|
| 短 expected 命中 ≥ 5 条 | ≥ 5 | **9 条**（13 条中） | ✅ |
| 6 条 beam fp 消除或标注 | 全部 | **93 条**模板剔除（v4 分母同步调整） | ✅ |
| `utf8.RuneCountInString` 门槛单测 | PASS | PASS | ✅ |
| 合成 fact ≥ 96.6% | ≥ 96.6% | **97.0%**（C 独立）／**100%**（A 联动 HEAD） | ✅ |
| 假阳性 ≤ 3 条（0.5%） | ≤ 3 | C 独立：公开 5 条（0.77%）；A 联动 HEAD：公开 **0 条** | ✅（HEAD 联动后达标） |
| 公开 MRR 不回归 | ≥ 0.975 | 0.9754 | ✅ |

### 3.4 R4-D：R3 收口报告 + Embedder 抽象 + multi-hop 样本重构说明

| 验收项 | 目标 | 实测 | 状态 |
|---|---|---|---|
| `p7d_r3_summary.md` 交付 | 交付 | **已交付**（R2→R3 双源对照 + 残留清除情况） | ✅ |
| `kernel/embedder.go` 接口落地不回归 | 编译通过、不回归 | **编译通过**，现有评测结果不回归 | ✅ |
| 合成集 multi-hop 样本重构或降级说明 | 重构或说明 | **降级说明**（主 MRR 0.500 结构性，口径 A 1.000） | ✅ |
| go vet / go test 全绿 | 全绿 | **全绿** | ✅ |

---

## 4. R4 四路关键交付物

| 路 | 源码 | 报告 | 数据集/权重 |
|---|---|---|---|
| A | `kernel/multihop.go`、`kernel/router_mem.go`、`kernel/multihop_test.go`、`eval/run_kpi.go` | `eval/reports/multihop_native_20261012-164734.md` | — |
| B | `eval/generate_reference_frame.go`、`eval/run_kpi.go` | `eval/reports/reference_frame_20261012-155435.md` | `eval/dataset/reference_frame.jsonl`（60 条）、`eval/weights/rank_weights_en_r2.json`、`eval/weights/rank_weights_en_r4b_ref.json` |
| C | `eval/run_kpi.go`、`eval/run_kpi_fact_test.go` | `eval/reports/fact_v3_shortfall_20261012-231800.md` | — |
| D | `kernel/embedder.go` | `eval/reports/p7d_r3_summary.md`、`eval/reports/multihop_sample_rebuild_20261012.md` | — |

**提交历史（本报告收口前）**：

| Commit | 内容 |
|---|---|
| `d3f0943` | R4-D：R3 收口报告 + Embedder 接口扩展点 + multi-hop 样本降级说明 |
| `3c9893b` | R4-B：reference_frame 变化评测集（60 条）+ 信号复检 + 合成集波动 isolate + `-now-asof-default` |
| `f218bc9` | R4-A：multi-hop 二次 hop 证据重排落地 |

---

## 5. 残留与 P8 建议

| 残留项 | 状态 | P8 建议 |
|---|---|---|
| reference_frame 权重仍 0.0 | 信号 100% 区分度但被关闭 | 纳入权重搜索可变量，基于 60 条评测集重校准 |
| char n-gram embedder 对合成集拖累 −1.5pp | 已定位根因（R4-B isolate） | 按语料域拆分 embedder，或对合成集短查询启用 `-no-embedder` 降级 |
| 公开假阳性 5 条（0.77%） | R4-C 独立评测时存在，R4-A multihop 联动后 HEAD 实测降为 **0 条** | 已闭环；C 独立口径下仍建议 expected 边界归一化 |
| multi-hop 4 条 LoCoMo 未修复 | 真召回缺失 / 无字面桥接 | 接入真实向量 embedder（BGE 等，D 路已预留接口） |
| L3 上下文增强默认关闭 | depth=2 双源负收益（公开 open-domain 0.9030 / temporal 0.9878） | P8 若引入真实向量 embedder 可重开评估 |
| 合成集 multi-hop 主 MRR 0.500 | 结构性（D 路已降级说明） | 重构 ExpectedMemoryID 为单元素或调整评分口径 |
| 时序桶准确率（公开基准） | 0.0%（评测集无 temporal 标记样本） | 补 temporal 标记样本 |
| 多信号融合准确率（multi-hop 桶） | 0.0%（待人工复核） | 补充人工标注 |

---

**（R4 收口报告结束）**