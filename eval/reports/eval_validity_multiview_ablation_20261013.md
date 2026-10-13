# R6-D 评测有效性报告（2026-10-13）

> 任务书：`_R6_千脑智能深化与北极星达标并行任务书.md` §4（D 路）
> 基线：8db1b78（R5 收口态，北极星 M1=0.9661）
> 语料：holdout_proxy.jsonl（590 条，合成代理语料）
> 环境：192.168.0.88（go run ./eval/polaris/）

## 1. 无锚变体（锚红利量化）

| 指标 | 基线（含锚） | 无锚（去括号） | 差值（锚红利） |
|---|---|---|---|
| 总体 M1 | **0.9661** | **0.5458** | **0.4203** |
| hit@1 | 0.9559 | 0.4898 | 0.4661 |
| mean_rr | 0.9063 | 0.4342 | 0.4721 |

### 分桶对照

| 桶 | 基线 | 无锚 | 差值 | 解读 |
|---|---|---|---|---|
| single-hop | 1.000 | 0.347 | **0.653** | 锚是消歧关键，无锚后 65% 命中丢失 |
| multi-hop | 1.000 | 0.000 | **1.000** | 多跳完全依赖锚做时序/实体区分 |
| temporal | 1.000 | 1.000 | 0 | temporal 桶不依赖括号锚 |
| reference_frame | 0.667 | 0.667 | 0 | ref 桶锚红利为 0（结构性问题） |
| open-domain | 1.000 | 1.000 | 0 | open-domain 不依赖括号锚 |

**结论**：锚红利总体 42pp，其中 single-hop 贡献 65pp、multi-hop 贡献 100pp。这证实了 P9 的判断——重构数据集的 query 括号是消歧锚且含答案 token，single-hop 桶 1.0 含「锚红利」。**评测诚实性关键数字：无锚 single-hop M1=0.347，这是真检索难度。**

## 2. 消融鲁棒性（8 信号逐路关闭）

| 信号 | 消融 M1 | 降幅 (pp) | 保留率 | 判定 |
|---|---|---|---|---|
| **semantic** | **0.7458** | **20.68** | **0.725** | **FAIL（单点）** |
| entity | 0.9034 | 4.92 | 0.959 | PASS |
| keyword | 0.9525 | 0 | 1.008 | PASS |
| value | 0.9525 | 0 | 0.999 | PASS |
| freshness | 0.9525 | 0 | 1.000 | PASS |
| source_trust | 0.9525 | 0 | 1.002 | PASS |
| reference_frame | 0.9525 | 0 | 1.000 | PASS |
| feedback | 0.9525 | 0 | 1.000 | PASS |

**结论**：
- semantic 信号是**单点故障**——关闭后 M1 降幅 20.68pp，远超 5pp 红线。这与千脑理论「多模型冗余」的理想状态不符。
- 其余 7 路信号关闭后降幅 ≤5pp，冗余达标。
- **遗留登记**：semantic 单点问题需通过 A 路投票共识（多视图集成）或 B 路组合记忆来缓解。

## 3. 多视图集成（确定性模板视图）

| 视图 | 模板 | M1 | hit@1 |
|---|---|---|---|
| view_0（基线） | 原 query | 0.9525 | 0.9085 |
| view_1 | 原 query + "？" | 0.6559 | 0.6559 |
| view_2 | "请查询" + 原 query | 0.6220 | 0.6220 |
| view_3 | 原 query + "，帮我查一下" | 0.6305 | 0.6305 |
| **集成（得票）** | 4 视图投票 | **0.8339** | **0.6458** |

**结论**：
- 多视图集成 M1=0.8339，**低于**基线 view_0 的 0.9525（delta=-0.1186）。
- 原因：回退模板（加"？"、"请查询"、"帮我查一下"）本身是劣化视图，拉低了集成效果。
- 千脑「思考=运动」的理想——多视图探测应≥单视图——在当前回退模板下未达成。
- **改进方向**：若能生成语义等价的变体（如「X的Y是啥」而非加标点），集成可能提升。但红线禁止 LLM 改写，需确定性模板设计。

## 4. R6-D 验收对照

| 验收项 | 目标 | 实际 | 判定 |
|---|---|---|---|
| 无锚变体报告产出 | 锚红利量化 | 总体 42pp，single-hop 65pp，multi-hop 100pp | ✅ PASS |
| 消融：任一路降幅 ≤5pp | ≤5pp | semantic 20.68pp（FAIL），其余 ≤5pp | ⚠️ 部分 FAIL |
| 多视图集成 M1 ≥ 单视图基线 | 不降 | 0.8339 < 0.9525（降 11.86pp） | ⚠️ FAIL |
| gofmt -l 无输出 | 0 | 0 | ✅ PASS |
| go vet 全绿 | 0 | 0 | ✅ PASS |
| go test 全绿 | 0 | 0 | ✅ PASS |

## 5. 千脑理论映射（D 路）

| 千脑概念 | lightmem 现状 | R6-D 结论 |
|---|---|---|
| 思考=在参考系中运动（多视图探测） | 单发 query 检索 | 回退模板视图劣化，需更优确定性模板 |
| 多模型冗余=优雅降级 | 多信号融合 | semantic 单点故障，冗余未达标 |
| 参考系是知识组织核心 | 单帧 4 坐标 | 无锚变体暴露锚红利，但 ref 桶本身无改善 |

## 6. 文件清单

| 文件 | 类型 | 说明 |
|---|---|---|
| `eval/polaris/polaris_validity.go` | 新增 | 无锚/消融/多视图三大模块 |
| `eval/polaris/polaris_eval.go` | 修改 | 新增 `-validity/-no-anchor/-ablate/-multiview` 开关 |
| `eval/polaris/polaris_test.go` | 新增测试 | stripAnchor/generateMultiviews/allSignalNames 单测 |
| `eval/reports/r6d_validity_noanchor_*.json` | 报告 | 无锚变体对照 |
| `eval/reports/r6d_validity_ablation_*.json` | 报告 | 8 信号消融曲线 |
| `eval/reports/r6d_validity_multiview_*.json` | 报告 | 多视图集成对照 |
| `eval/reports/r6d_validity_anchor_details_*.json` | 报告 | 锚红利逐条明细 |
| `eval/reports/r6d_validity_multiview_details_*.json` | 报告 | 多视图逐条明细 |
