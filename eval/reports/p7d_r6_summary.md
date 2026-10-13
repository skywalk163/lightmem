# R6 四路收口报告：千脑智能深化 + 北极星达标（p7d_r6_summary）

> **任务书**：`lightmem/.dumate/inbox/_R6_千脑智能深化与北极星达标并行任务书.md`（A 投票共识/参考系 · B 连续学习/组合 · C 北极星判定/FP/压缩 · D 评测有效性）
> **前置权威基线**：`8db1b78`（R5 收口 + 重构数据集入库，公开 MRR 0.9917 / 合成 1.000 / 北极星 M1 0.9661）
> **本报告**：四路合流后最终态实测 + 千脑五块落地 + P3/P4/P7/P9 清除状态 + 新问题登记册（任务书 §6.2 第 4 条要求的整合报告）
> **验证机**：192.168.0.88（FreeBSD 15.1，go1.25.14）——全部编译/测试/运行在此完成
> **合流副本**：`/tmp/r6final`（HEAD = B/C/D 已提交态，叠加 A 路改动后全量验证）
> **提交**：A `3b58b2f` / B `ad6eae4` / C `9fd7b46` / D `7ae070c` + 本收口提交；**push 待用户示意**

---

## 0. 执行摘要

**R6 四路交付完成；A/B 判绿，C 部分达标（受外部语料制约），D 完成测量但暴露 2 项未达标（如实登记）。**

合流后最终态（0.88 实测，R5 交付权重 + BGE-m3 向量 + 固定 Now=2026-10-13）：

| 指标 | R5 基线（8db1b78） | R6 最终态 | 变化 | 归属 |
|---|---|---|---|---|
| 公开基准 MRR（780 条，含 temporal 合并） | 0.9917 | **0.992** | 持平 ✅ | A |
| 合成集 MRR（570 条） | 1.000 | **1.000** | 保持 ✅ | A |
| 北极星 M1（代理语料 590，D 口径默认权重） | 0.9661 | **0.9661** | 逐位一致（默认零回归）✅ | A/C |
| 北极星 ref 桶（A 路激活口径） | 0.667 | **1.000**（hit@1 34→46） | +33.3pp ✅ | **A** |
| 北极星 M1（A 路激活交付权重口径） | — | **1.000**（hit@1 0.9763） | 全桶 1.0 ✅ | A |
| retention(N=1000) | 未测 | **1.0000 ≥0.95** | 达标 ✅ | B |
| C 独立口径 FP | 3 | **0** | ≤1 达标 ✅ | C |
| 数据面压缩率 | 未实现 | **−53.82%**（M1 判定不降） | 达标 ✅ | C |
| 真实语料北极星正式判定 | 未到位 | **未到位 → 代理预跑，非正式** | ⚠️ 待语料 | C |
| 锚红利（无锚变体） | 未量化 | **总体 42pp**（single-hop 65pp / multi-hop 100pp） | 已量化 ✅ | D |
| 信号消融（任一路 ≤5pp） | 未测 | semantic **20.68pp（FAIL）**，其余 ≤5pp | ⚠️ 部分 FAIL | D |
| 多视图集成 M1 | 未测 | 0.8339 < 单视图 0.9525 | ⚠️ FAIL | D |

四路 commit：A `3b58b2f`（投票共识 + 参考系激活，P3 突破）/ B `ad6eae4`（retention + 组合记忆）/ C `9fd7b46`（holdout runner + FP 审计 + 数据面压缩）/ D `7ae070c`（无锚变体 + 消融 + 多视图）。

---

## 1. 分路查收（验收逐条对账）

### 1.1 A 路 — 千脑投票共识 + 参考系深化（`3b58b2f`）✅ 判绿

**P3 突破**：ref 桶 M1 0.667 → **1.000**，其余四桶降幅 **0.0pp**。

| 验收项（§A.4） | 目标 | 实测 | 判定 |
|---|---|---|---|
| ref 桶 M1 覆盖率 | ≥0.85（现 0.667） | **1.0000**（hit@1 34→60 满配 / 46 于交付权重） | ✅ |
| single/multi-hop/open-domain/temporal 降幅 | ≤0.5pp | 0.0pp（四桶全 1.0） | ✅ |
| 投票模式公开/合成 MRR 不回归 | ≤基线−0.002 | 公开 0.992（基线 0.9917）/ 合成 1.000 | ✅ |
| 分歧度与 top-1 错误率正相关 | 可测 | Spearman ρ=**0.2712>0**，分箱 miss_rate 3.98%→12.12% 单调 | ✅ |
| gofmt/vet/test（双模块） | 全绿 | 全绿 | ✅ |

**根因（深度诊断，推翻 R5-A 结论）**：P3 不是「ref 权重被稀释」，而是 **ref 信号在评测链路里根本没被激活**（两层）：
1. `RecallAndFuse` 内 `rf.QueryFrame = rc.frame()`，评测只设 Text/tenant/user → 参考系坐标全空 → `signalReferenceFrame` 恒返 0.5 中性分（常数，不改变排序）；
2. 查询实体正则 `客户[A-Z0-9一-龥]+` 贪婪，把「客户A的偏好是什么」整句当 Entity，与候选「客户A」不匹配 → hit=0。
修复后 refSig 正确记忆 0.333；**R5-A「ref 权重纳入无收益」是对死信号的误校准**。

**关键设计洞察**：纯票排序（丢弃 tuned 权重）在真实语料上**结构性回退**（multi-hop 1.0→0.02~0.31，ref 桶→0.10~0.53）。根因：RRF 用 `1/(K+rank)`（K=60）**平滑**计票，ConsensusVoter 用 `1/rank` **陡峭**计票，后者让 top1 独大、挤掉多跳中间证据。故最终 vote 模式采用**「RRF 主排序 + ConsensusVoter side-channel」**——主排序与 rrf 逐位一致（结构性零回归），共识分/分歧度/得票数作为新增可选字段暴露给上层做「分歧大→低置信」判定。

**交付**：`kernel/voting.go`（新，`Interpretation`+`Weight`/`ConsensusVoter`/`ConsensusResult`/`FrameSet`/`MatchFrames`）、`kernel/voting_test.go`（新，10 用例）、`kernel/rank_fusion.go`（改，+149/−0，纯新增）、`eval/polaris/polaris_voting.go`（新独立 runner）、`eval/weights/rank_weights_r6_ref.json`（ref=3.0 交付权重）。

### 1.2 B 路 — 连续学习 retention + 存储期组合记忆（`ad6eae4`）✅ 判绿

| 验收项（§B.3） | 目标 | 实测 | 判定 |
|---|---|---|---|
| retention(N=1000) | ≥0.95 | **1.0000**（100/300/1000 全 1.0） | ✅ |
| 可塑性（增量集自身 M1） | — | 1.0000 | ✅ |
| 组合记忆 store-expand-retrieve 单测 | 全绿 | 5/5 用例 PASS | ✅ |
| consolidate 既有测试 | 无回归 | 全绿 | ✅ |
| 公开/合成 MRR 不回归 | ≤−0.002 | 结构论证差异=0（`ComposeExpand` 默认 false） | ✅ |

**设计**：存储期组合（`Memory.Components` 新增可选字段 + `Compose`/`Decompose`/`ExpandComposition`，子记忆参考系继承+覆盖）；`expandCompositions` 仅当 `rc.ComposeExpand=true` 且候选含 `Components` 时生效（既有数据 no-op）；consolidate 组合感知（组合不去重拆散 + 父组件同步）。

### 1.3 C 路 — 北极星正式判定 + FP 清理 + 数据面压缩（`9fd7b46`）⚠️ 部分达标

| 验收项（§C.3） | 目标 | 实测 | 判定 |
|---|---|---|---|
| 真实语料北极星 M1 | ≥0.95 正式判定 | **真实 holdout.jsonl 未到位 → 代理预跑 0.9661，非正式** | ⚠️ 待语料 |
| C 独立口径 FP | ≤1 | **0**（v4 命中 626，审计 vetoed=0） | ✅ |
| 数据面压缩 | token −20% 且 M1 不降 | **109335→50491（−53.82%）**，判定输入原文不压 | ✅ |
| gofmt/vet/test | 全绿 | 全绿 | ✅ |

**FP 清理**：两条确定性规则（否定一致性 `negation_conflict` + 碎片化护栏 `fragment` + 时序豁免）；历史 3 条 FP 在 R5 终态已自然消解，本次交付**护栏 + 回归锁定**防复发。**数据面压缩**：`CompressDataFact` 只压展示/计费面副本，判定输入不动（双口径）。

### 1.4 D 路 — 评测有效性 + 多视图 + 消融（`7ae070c`）⚠️ 完成测量，2 项未达标

| 验收项（§D.4） | 目标 | 实测 | 判定 |
|---|---|---|---|
| 无锚变体（锚红利量化） | 报告产出 | 总体 **42pp**（single-hop 65pp / multi-hop 100pp） | ✅ |
| 消融：任一路降幅 ≤5pp | ≤5pp | semantic **20.68pp FAIL**；其余 7 路 ≤5pp | ⚠️ 部分 FAIL |
| 多视图集成 M1 ≥ 单视图 | 不降 | 0.8339 < 0.9525（−11.86pp） | ⚠️ FAIL |
| gofmt/vet/test | 全绿 | 全绿 | ✅ |

**关键诚实性数字**：**无锚 single-hop M1 = 0.347** ——这是重构数据集剥离括号消歧锚后的真检索难度（含锚 1.0 有 65pp 锚红利）。
**未达标如实登记**：① semantic 单点故障（关掉降 20.68pp，千脑「多模型冗余」未达标）；② 多视图集成因回退模板（加标点/加「请查询」）本身劣化而低于单视图。

---

## 2. R5 转入项清除情况（任务书 §0.1 四项逐条）

| # | 转入项 | 路由 | R6 结局 | 状态 |
|---|---|---|---|---|
| P3 | reference_frame 桶 0.667（信号区分度 100% 但权重 0.0） | A | **根因重定位**：信号在评测链路里是死的（非权重稀释）；激活后 ref 桶 **1.000** | ✅ **关闭（突破）** |
| P4 | fact 假阳性残留 3 条 | C | 确定性护栏 + 回归锁定；R6 复跑 FP=**0**（历史 3 条已自然消解） | ✅ 关闭 |
| P7 | 真实语料 holdout 未到位 | C | runner 就绪、自动探测切换；**语料仍未到位 → 代理预跑 0.9661，正式判定待语料** | ⏳ 未关闭（外部依赖） |
| P9 | 评测有效性：single-hop 含「锚红利」，缺无锚难度变体 | D | 无锚变体量化：**锚红利 42pp**（single-hop 0.653 / multi-hop 1.000），暴露真难度 | ✅ 关闭（如实量化） |

**P3 关闭是本轮最大成果**；P7 为唯一未关闭项，纯外部语料依赖（用户确认 5 天内到位）。

---

## 3. 千脑理论五块落地情况（任务书 §0.2）

| # | 千脑概念 | R6 落地 | 状态 |
|---|---|---|---|
| 1 | 柱各自解释 → 柱间**投票**涌现共识 | `ConsensusVoter` 解释级倒数秩计票；vote 融合模式（默认 rrf） | ✅ 落地 |
| 2 | **分歧=不确定**（预测误差驱动注意） | `Divergence=1−Consensus` 经 `FusedResult` side-channel 暴露；ρ=0.2712 正相关可测 | ✅ 落地 |
| 3 | **参考系是知识组织核心**（一物多帧） | `FrameSet`+`MatchFrames` 帧变换匹配；**ref 信号激活（P3 突破）** | ✅ 落地 |
| 4 | **思考=在参考系中运动**（多视图） | 确定性模板视图 + 候选得票集成 | ⚠️ 落地但劣化（回退模板，需更优模板） |
| 5 | **连续学习**（不从零重训练） | retention(N=1000)=1.0 + 可塑性曲线 | ✅ 落地 |
| 6 | **组合物体**（物体=子物体） | 存储期组合 `Components`+`Compose/Decompose` | ✅ 落地 |
| 7 | **多模型冗余=优雅降级** | 8 信号消融测量 | ⚠️ semantic 单点故障（未达标） |
| 8 | 旧脑/新脑分层 | WritePolicy vs 学习记忆（R6 未动） | — 已对齐 |

**五块中 3.5 块落地**（投票共识 / 分歧置信 / 参考系激活 / 连续学习 / 组合物体 = 5 块落地；多视图与冗余测量为「已测未达标」）。

---

## 4. 验证矩阵（0.88，合流后最终态）

| 门禁 / 测量 | 命令 | 结果 |
|---|---|---|
| gofmt（全仓库） | `gofmt -l .` | 空输出 ✓（A 路 4 文件已格式化） |
| go vet（root 模块） | `go vet ./...` | RC=0 |
| go test（root 模块） | `go test ./...` | ok（polaris + fact + fp + compress_data 单测） |
| go vet/test（kernel 模块） | `cd kernel && go vet ./... && go test ./...` | RC=0 / ok（含 voting_test 10 例 + composition_test 5 例） |
| 公开基准 | `run_kpi -dataset benchmarks/dataset/all.jsonl -temporal-samples dataset/temporal.jsonl -weights rank_weights_en_r5.json -embedder vector` | MRR 0.992 / ID 1.0 / fact 96.1% / temporal 1.0 |
| 合成集 | 同配置 `-dataset dataset/all.jsonl` | MRR 1.000 / 全桶 1.0 |
| 北极星（D 口径默认权重） | `go run ./eval/polaris/ -holdout eval/dataset/holdout_proxy.jsonl` | M1 0.9661 / M2 0.4375（逐位同 R5） |
| 北极星（A 路激活交付权重） | `polaris_voting -fusion rrf -ref-activate=true -weights rank_weights_r6_ref.json` | M1 **1.000** / ref 桶 **1.000** |
| 公开/合成（ref=3.0 交付权重） | `run_kpi -weights rank_weights_r6_ref.json` | 公开 0.992 / 合成 1.000 |

> 备注：全仓库 `gofmt -l` 曾在 `kernel/lifecycle.go`、`kernel/metrics.go` 报输出，经核为 **R138-B 提交（`ce38cd5`）遗留**的字段对齐格式问题，非 R6 引入、非本轮改动文件（已在验证副本修复，未纳入任一路 commit diff；建议收口后单独提交该格式化或留待后续）。

---

## 5. 新问题登记册（R6 发现）

### 5.1 光明 light（light-merge 语言项目）：**本轮（R6）无**

R6 四路全部落在 `lightmem/`（Go 实现），未触碰 `light-merge/`。无需向 light 缺陷账追加编号。

### 5.2 lightmem 侧问题

| # | 问题 | 级别 | 状态 | 说明 / 处置 |
|---|---|---|---|---|
| Q1 | semantic 信号单点故障：消融后 M1 降 **20.68pp**（远超 5pp 红线），千脑「多模型冗余」未达标 | 高 | 未关闭 | 缓解方向：投票共识多视图集成（D 路初版因回退模板劣化）/ 组合记忆；需更优确定性模板 |
| Q2 | 多视图集成 M1 0.8339 < 单视图 0.9525（−11.86pp） | 中 | 未关闭 | 根因=回退模板（加标点/加「请查询/帮我查一下」）本身劣化；需语义等价变体（红线禁 LLM，须确定性模板） |
| Q3 | 真实语料 holdout 未到位：北极星 0.9661 为代理语料口径 | 高 | 未关闭 | 同 R5-P7；runner 就绪（`polaris_holdout.go` 自动探测），语料落 `eval/dataset/holdout.jsonl` 即自动切正式判定 |
| Q4 | 无锚真难度暴露：single-hop 无锚 M1=**0.347**（含锚 1.0，锚红利 65pp） | 中 | 登记 | 评测诚实性交付物；产品口径需据此校准「真检索能力」宣称 |
| Q5 | `kernel/lifecycle.go`/`metrics.go` 既有 gofmt 违规（R138-B 遗留） | 低 | 登记 | 非 R6 引入；建议单独格式化提交 |

---

## 6. 提交与推送

| 路 | commit | 内容 |
|---|---|---|
| A | `3b58b2f` | voting.go + voting_test.go + rank_fusion.go（+149） + polaris_voting.go + rank_weights_r6_ref.json + A 报告 |
| B | `ad6eae4` | composition.go + composition_test.go + consolidate.go + model.go + router_mem.go + polaris_learning.go + B 报告 |
| C | `9fd7b46` | polaris_holdout.go + polaris_fp.go(+test) + compress_light.go + polaris_compress_data_test.go + 2 JSON + C 报告 |
| D | `7ae070c` | polaris_eval.go + polaris_validity.go + polaris_test.go + 3 JSON + D 报告 |
| 收口 | 本提交 | 本整合报告 |

**push**：**禁止自行 push（R6 红线）**。四路 commit 已本地就绪，经用户示意后由主 agent 统一 push（gitea `http://192.168.1.5:3000/skywalk/lightmem.git`）并 `git ls-remote` 复核。

---

## 7. 遗留与下一步

1. **P7/Q3 真实 holdout**（高优先，外部依赖）：语料到位后落 `eval/dataset/holdout.jsonl`，`go run ./eval/polaris/ -holdout eval/dataset/holdout.jsonl -report polaris_real` 出正式北极星数。
2. **Q1 semantic 单点 / Q2 多视图**：合并为「确定性多视图 + 冗余降级」立项；核心是设计语义等价视图模板（非加标点式回退）。
3. **交付权重上架建议**：`rank_weights_r6_ref.json`（ref=3.0）作为 ref 激活后的默认调优点；`polaris_voting.go` 已默认引用。
4. **A 路 vote 模式**：作为增量能力（默认 rrf），供上层按「分歧大→需确认」场景选择性启用。
