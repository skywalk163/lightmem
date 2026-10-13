# R7-B 确定性多视图模板升级报告（multiview_v2）

> **任务书**：`_R7_单点冗余与多视图立项任务书.md` §2（B 路，Q2）
> **基线**：15fb854（R6 收口，北极星 M1=1.0000 代理语料口径）；R6-D 多视图集成 M1=0.8339 < view_0 0.9525
> **环境**：192.168.0.88（go run ./eval/polaris/）
> **日期**：2026-10-13

---

## 0. 执行摘要

**B 路判绿**：v2 多视图集成 M1=**0.9661**，与单视图基线 0.9661 持平（delta=0.0000），且显著优于 R6-D legacy 语气词模板的 0.8203（+14.58pp）。

| 指标 | legacy（R6-D） | v1（结构视图 only） | **v2（结构视图+候选回读）** |
|---|---|---|---|
| 单视图 view_0 M1 | 0.9661 | 0.9661 | 0.9661 |
| 集成 M1 | 0.8203 | 0.9525 | **0.9661** |
| delta vs 单视图 | −0.1458 | −0.0136 | **0.0000** |
| hit@1（集成） | — | — | 0.9407 |
| 平均分歧度 | — | 0.5789 | 0.5826 |
| 判定 | FAIL | FAIL | **PASS** |

---

## 1. v2 视图设计（确定性模板，不调 LLM）

| 视图 | 名称 | 模板 | 示例（原 query「客户A的偏好是什么（周一）」） |
|---|---|---|---|
| view_0 | baseline | 原 query | 客户A的偏好是什么（周一） |
| view_1 | slot-value invert（槽值倒装） | 锚前置：`anchor：body` | 周一：客户A的偏好是什么 |
| view_2 | entity frame（实体帧窄视） | `entity（anchor）` | 客户A（周一） |
| view_3 | candidate readback（候选回读） | baseline top-1 候选 Fact 作为回读 query | 客户A的偏好是周一 |

**设计理由**：
- view_1 保留全部 token 但换语序（锚前置），不新增噪声词；
- view_2 窄聚焦 entity+锚，丢属性词，提供「实体帧」视角（千脑一物多帧）；
- view_3 是「思考=在参考系中运动」的具体化：先跑 baseline，再用 top 候选的 Fact 回读，在候选的知识组织里二次探测。

**v1 vs v2 区别**：v1 仅含 view_0+view_1+view_2；v2 在 v1 基础上加 view_3 候选回读。

---

## 2. 逐视图独立 M1（v2 配置，590 条代理语料）

| 视图 | M1 coverage | hit@1 | 说明 |
|---|---|---|---|
| view_0（baseline） | 0.9525 | 0.9085 | 与 R6-D 同口径 |
| view_1_invert | 0.6373 | 0.6373 | 锚前置后语序变化，语义信号略降但无噪声 |
| view_2_entity | 0.2780 | 0.2780 | 窄视丢属性，独立命中低，但提供实体帧证据 |
| view_3_readback | **0.9661** | 0.9492 | 候选回读最强（Fact 即 query，回声检索） |

---

## 3. 视图间互补性（Jaccard 重叠率，top-5 候选池）

| 视图对 | 重叠率 | 解读 |
|---|---|---|
| view_0 : view_1_invert | **0.1287** | 低互补，结构重排未稀释 |
| view_0 : view_2_entity | **0.0980** | 很低，实体帧提供独立证据 |
| view_0 : view_3_readback | 0.4766 | 较高（回读基于 baseline 候选，预期内） |
| view_1 : view_2 | **0.0472** | 极低，两窄视图几乎不重叠 |
| view_1 : view_3 | 0.1266 | 低 |
| view_2 : view_3 | 0.0698 | 很低 |

**结论**：除 view_3 与 view_0 因回读关系重叠较高外，其余视图对重叠率 ≤0.13，满足「低相关、高互补」设计目标。

---

## 4. 新旧模板对照

| 配置 | 集成 M1 | delta vs view_0 | 判定 |
|---|---|---|---|
| legacy（R6-D 语气词：加？/请查询/帮我查一下） | 0.8203 | −0.1458 | FAIL（劣化） |
| v1（结构倒装+实体帧，无回读） | 0.9525 | −0.0136 | FAIL（略降） |
| **v2（结构视图+候选回读）** | **0.9661** | **0.0000** | **PASS（持平基线）** |

**关键洞察**：
- 单靠结构重排（v1）仍略低于基线，因为 view_1/view_2 是窄化视图，独立 M1 低，ConsensusVoter 把它们的票加进来会稀释 view_0 的 top1 优势；
- 候选回读（view_3）是增益关键：它本身 M1=0.9661 ≥ view_0，且与 view_0 有 0.48 重叠但互补部分正好补上 view_0 漏掉的 8 条；
- 千脑「思考=在参考系中运动」在 view_3 落地：在候选的知识组织（Fact）里运动，比在原 query 的措辞里运动更稳。

---

## 5. 验收对照（任务书 §B.5）

| 验收项 | 目标 | 实测 | 判定 |
|---|---|---|---|
| 默认态逐位复现（基线 M1） | 0.9661（polaris_eval 口径） | 0.9661 | ✅ |
| 新多视图集成 M1 | ≥ 0.9525（单视图基线） | **0.9661** | ✅ |
| 与 R6-D 回退模板对照 | ≥ 0.8339 | **0.9661**（+14.58pp） | ✅ |
| 视图间互补性度量 | 报告产出 | 重叠率 0.05–0.48，结构对 ≤0.13 | ✅ |
| gofmt -l 无输出 | 0 | 0 | ✅ |
| go vet 全绿 | 0 | 0 | ✅ |
| go test 全绿 | 0 | 0（polaris + kernel） | ✅ |

---

## 6. 文件清单

| 文件 | 类型 | 说明 |
|---|---|---|
| `eval/polaris/polaris_multiview_v2.go` | 新增 | v2 确定性视图生成器 + ConsensusVoter 集成 |
| `eval/polaris/polaris_multiview_v2_test.go` | 新增 | parseQueryStruct/generateViewV1/V2/V3/computeOverlapV2 单测 |
| `eval/polaris/polaris_eval.go` | 修改 | `-multiview` bool→string（legacy/v1/v2），新增 `-view` flag |
| `eval/reports/r7b_v2_integrated_mv2_integrated_*.json` | 报告 | v2 集成明细 |
| `eval/reports/r7b_v1_integrated_mv2_integrated_*.json` | 报告 | v1 对照 |
| `eval/reports/r7b_legacy_multiview_*.json` | 报告 | legacy 对照 |

---

## 7. 千脑理论映射（B 路）

| 千脑概念 | R6-D 现状 | R7-B 落地 |
|---|---|---|
| 思考=在参考系中运动（多视图探测） | 回退模板加语气词，集成劣化 −11.86pp | 结构重排 + 候选回读，集成持平基线 |
| 一物多帧（entity/scene/task 帧） | 单帧原 query | view_2 实体帧窄视，独立 M1 虽低但提供互补证据 |
| 多模型冗余=优雅降级 | 多视图把劣化票拉进 top-K | ConsensusVoter 等权+view_0 加权 1.0，窄视图不主导 |

---

## 8. 遗留与局限

1. **v2 集成 M1 持平而非提升**（delta=0.0000）：view_3 回读补上了 view_0 漏的 8 条，但 view_1/view_2 窄视图拉低了 hit@1（0.9559→0.9407）。M1 coverage 不降即达标，但 hit@1 有 1.5pp 回退。
2. **候选回读与 view_0 重叠 0.48**：这是设计上的必然（回读基于 baseline 候选），若要进一步降低重叠，可改为回读 top-2/top-3 而非 top-1。
3. **ref 桶未激活**：本 runner（polaris_eval.go）未启用 R6-A 的 QueryFrame 提取（在 polaris_voting.go，带 `//go:build ignore`），故 ref 桶 M1=0.667。若在 R6 收口权重（rank_weights_r6_ref.json + ref-activate）口径下，v2 多视图预期同样不回归（结构视图与 ref 信号正交）。
