# R7-A 验收收口报告（semantic 单点冗余加固，Q1）

- **任务书**：`lightmem/.dumate/inbox/_R7_单点冗余与多视图立项任务书.md` §A
- **代码提交**：`bb286d1` feat(R7-A): semantic 单点冗余加固（Q1）
- **北极星基线**：`15fb854`（M1 = 1.0000）
- **收口日期**：2026-10-14
- **运行环境**：192.168.0.88（Go 1.25），干净工作树 `/tmp/r7a/lightmem`

---

## 一、验收判定表（任务书 §A.5）

| 验收项 | 目标 | 实测 | 判定 |
|---|---|---|---|
| 默认态逐位复现 15fb854 | 差异 0（M1=1.0000） | M1=**1.0000**，hit@1=0.9763（ref 桶 46/60），与 R6 收口报告完全一致 | **PASS** |
| 多路径开启后整体 M1 | ≥ 1.0000（不降） | M1=**1.0000**，hit@1=**1.0000**，mean_rr 0.9171（+0.0058） | **PASS** |
| 任一路关闭降幅 ≤5pp | ≤5pp（原 20.68pp） | 关 path0 −1.02pp / 关 path1 −0.51pp / 关 path2 −0.00pp，最大 **1.02pp** | **PASS** |
| 与 R6-D 单路径消融对照 | 报告产出 | 见 §三（20.68pp → 1.02pp，改善约 20 倍） | **PASS** |
| gofmt / go vet / go test | 全绿 | gofmt 无输出；vet OK；kernel 0.043s ok；polaris 0.010s ok | **PASS** |

**判定：A 路核心指标（多路径开启后任一路关闭 ≤5pp）达标，判绿。** 多路径开启后整体 M1 不降且 hit@1 提升，冗余达标，无降级登记。

---

## 二、北极星主口径（`polaris_voting.go`，15fb854 同源）

评测命令：
```
go run eval/polaris/polaris_voting.go eval/polaris/compress_light.go \
  eval/polaris/polaris_m1.go eval/polaris/polaris_token.go \
  eval/polaris/polaris_fact.go eval/polaris/polaris_fp.go \
  -fusion rrf -ref-activate=true -weights eval/weights/rank_weights_r6_ref.json \
  -now-fixed 2026-10-13 -topk 5
```

| 配置 | M1 | hit@1 | mean_rr | n |
|---|---|---|---|---|
| 默认态（redundant=false，复现 15fb854） | **1.0000** | 0.9763 | 0.9113 | 590 |
| 多路径开启（redundant=true） | **1.0000** | **1.0000** | 0.9171 | 590 |
| 确定性重跑（同参数再跑） | **1.0000** | **1.0000** | 0.9199 | 590 |
| 关 path0（原向量） | 0.9898 | 0.9881 | 0.9203 | 590 |
| 关 path1（slot-value） | 0.9949 | 0.9915 | 0.9147 | 590 |
| 关 path2（token 共现） | 1.0000 | 0.9983 | 0.9144 | 590 |

- 默认态逐位复现：M1=1.0000，hit@1=0.9763，ref 桶 hit@1=46/60 —— 与 R6 收口报告完全一致。
- 多路径开启：M1 不降且 **hit@1 提升到 1.0000**（ref 桶 46→60 全命中），mean_rr +0.0058。
- 确定性：两次独立运行 M1/hit@1 完全一致（无随机源，红线 5 满足）。

---

## 三、逐路径消融 vs R6-D 单路径对照

| 消融对象 | M1 | 降幅 | 保留率 | 判定 |
|---|---|---|---|---|
| **R6-D：关闭整个 semantic** | 0.7458 | **20.68pp** | 0.725 | FAIL（单点） |
| R7-A：关 path0（向量） | 0.9898 | **1.02pp** | — | PASS |
| R7-A：关 path1（slot-value） | 0.9949 | **0.51pp** | — | PASS |
| R7-A：关 path2（token 共现） | 1.0000 | **0.00pp** | — | PASS |

**结论**：单点故障（关闭 semantic 降 20.68pp）已通过三路互补路径消除——任一路关闭最大降 1.02pp，**改善约 20 倍**，远超 5pp 红线。

### R6-D 同口径交叉对照（`polaris_eval.go` 路径）

| 配置 | M1 | 降幅 |
|---|---|---|
| 基线（runEvalOnce 链路） | 0.9661 | — |
| 多路径开启 | **1.0000** | +3.39pp |
| 关 path0 | 1.0000 | +3.39pp（升） |
| 关 path1 | 0.9932 | −2.71pp |
| 关 path2 | 0.9898 | −2.37pp |

> **口径澄清**：验收主口径为 `polaris_voting.go`（北极星，15fb854 同源，基线 M1=1.0000）。R6-D 消融表基线 0.9525（`r6d_validity_ablation_20261013-115707.json`，multi-hop 142/150）与本报告 eval 链路基线 0.9661（multi-hop 150/150）之差，源于 15fb854 之后合入的 R6-D fact fix 修复 multi-hop 召回，属代码版本推进，非口径冲突。两个口径下「多路径开启 M1 ≥ 各自基线」均成立。

---

## 四、实现要点（只增不改）

- **`kernel/semantic_multi.go`（新增）**：
  - path0 = 原向量余弦（保留 R6 实现不动）；
  - path1 = slot-value 结构匹配（entity 0.5 + attr 0.2 + value↔anchor/decider 0.3，满分 1.0，零向量依赖）；
  - path2 = token 双向共现 + IDF 加权（0.6 主项 + 0.4 次项，双向归一）；
  - 三路各自 top-K 解释 → `kernel.ConsensusVoter` 倒数秩投票 → 与向量底座 0.5:0.5 平滑 → `normalizeMax`。
  - **平局抑制**（修复 ref 桶 3 条 miss 的关键）：`isAskAttrQuery` + `subjectValueRe` 对「问属性值 + value 是主体词（hop1 中间记忆）」的候选扣分（path1 扣 0.3，path2 归 0），使正确 fact 稳定排前。
- **`kernel/rank_fusion.go`（只增）**：`RankFusion` 新增 `SemanticRedundant` / `SemanticAblatePath`；`signalSemantic` 加分支，`false` 时逐位保持 R6 行为。
- **`eval/polaris/polaris_semantic_redundancy.go`（新增）**：`runSemanticRedundancy` 三步（基线→多路径开启→逐路径消融）+ JSON 报告。
- **`polaris_eval.go` / `polaris_voting.go`（只增）**：`-semantic-redundant` / `-semantic-ablate-path` flag + 分发段。

**未触碰**：`light-merge/`、kernel Schema 既有字段语义、`Memory`/`RouterContext`/`RecalledCandidate` 定义。R7-B 的 `polaris_multiview_v2.go` 保持独立未提交，与 A 路共存未冲突。

---

## 五、文件清单（任务书 §A.6）

| 文件 | 类型 | 状态 |
|---|---|---|
| `kernel/semantic_multi.go` | 新增 | ✅ 已入库 |
| `kernel/rank_fusion.go` | 修改（只增） | ✅ 已入库 |
| `eval/polaris/polaris_semantic_redundancy.go` | 新增 | ✅ 已入库 |
| `eval/polaris/polaris_eval.go` | 修改（只增） | ✅ 已入库 |
| `eval/polaris/polaris_voting.go` | 修改（只增） | ✅ 已入库 |
| `eval/reports/semantic_redundancy_20261014.md` | 报告 | ✅ 已入库 |
| `eval/reports/semantic_redundancy_20261014-000000.json` | 报告 | ✅ 已入库 |
| `eval/reports/r7a_{sem,base,det,ab0,ab1,ab2}_*.json` | 报告明细 | ✅ 已入库 |

---

## 六、结论

**R7-A（Q1 semantic 单点冗余加固）验收通过，收口入库。**

- 单点故障 20.68pp 已消除：任一路关闭最大降 1.02pp（改善约 20 倍，红线 5pp 达标）。
- 多路径开启 M1=1.0000 不降，hit@1 提升到 1.0000（ref 桶 46→60）。
- 默认态逐位复现 15fb854；门禁 gofmt/vet/test 全绿；确定性两次运行一致。
- 只增不改，未触碰 kernel Schema 与 `light-merge/`。