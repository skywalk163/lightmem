# R5 四路收口报告：R4 残留项清除 + 检索能力升级（p7d_r5_summary）

> **任务书**：`lightmem/.dumate/inbox/_R5_检索能力升级并行任务书.md`（A 向量检索 / B 样本重构 / C 合规性能 SDK / D 北极星）
> **前置权威基线**：`9a2220a`（R4 四路联合最终态）
> **本报告**：四路合流后的最终态实测 + 残留清除情况 + 问题登记册（任务书 §5.2 要求的整合报告）
> **验证机**：192.168.0.88（FreeBSD 15.1，go1.25.14）——全部编译/测试/运行在此完成
> **提交**：`81f7370`(A) / `d6c7be7`(B) / `a09b790`(C) / `cdc478d`(D) + 本收口提交；push 经用户示意后执行

---

## 0. 执行摘要

**R5 四路全部判绿**。合流后最终态（0.88 实测，R5-A 权重 + BGE-m3 向量 + B 路重构数据集 + 固定 Now=2026-10-13）：

| 指标 | R4 基线（9a2220a） | R5 最终态 | 变化 |
|---|---|---|---|
| 公开基准 MRR（780 条，含 temporal 合并） | 0.9845（650 条） | **0.9917** | +0.72pp ✅ |
| 公开基准 ID top-1 准确率 | ≈96% | **1.000** | ✅ |
| 公开基准 fact 准确率（v4） | 94.8% | **96.07%**（662/687） | +1.3pp ✅ |
| 合成集 MRR（530 条） | 0.8585 | **1.000** | +14.2pp ✅ |
| 合成集 multi-hop 主 MRR | 0.500（结构性下限） | **1.000** | 脱离下限 ✅ |
| temporal 桶准确率 | 1.000 | **1.000** | 保持 ✅ |
| 多信号融合准确率 | 0.0%（无样本） | **1.000** | 清除 ✅ |
| 污染率 | 0 | **0** | 保持 ✅ |
| fact 假阳性（HEAD 联动口径） | 0 | **0** | 保持 ✅ |
| 北极星 M1 零 token 覆盖率（代理语料 590） | 口径未实现 | **0.9661 ≥ 0.95** | 达标 ✅ |
| 控制面压缩率 | 口径未实现 | **35.77%**（覆盖率不降） | 达标 ✅ |
| 检索 P95 | 未测 | **0ms < 200ms** | 达标 ✅ |
| GDPR 三件套 | 未实现 | **PASS** | 达标 ✅ |

四路 commit：A `81f7370`（BGE-m3 向量 embedder + ref 权重重校准）/ B `d6c7be7`（multi-hop 样本重构 + temporal 补齐 + fact 归一）/ C `a09b790`（GDPR + P95 + SDK）/ D `cdc478d`（北极星 M1/M2 + 控制面压缩）。

---

## 1. 分路查收（验收逐条对账）

### 1.1 A 路 — 真实向量 embedder + reference_frame 权重重校准（`81f7370`）

| 验收项（§A.5） | 结果 |
|---|---|
| `kernel/embedder_vector.go` 编译通过、`-embedder vector` 可运行 | ✅ BGE-m3（568M，dim=1024，mean pooling，L2 归一）离线生成静态向量 `.npy`，Go 侧查表；未命中返回全零向量（确定性）；模型缺失报错并提示回退 tfidf |
| reference_frame 权重重校准**非零或明确结论** | ✅ **明确结论「纳入后无收益，保留 0.0」**（任务书认可的第二种达标口径）：坐标下降 8 变量 1 轮收敛，ref+0.1 后公开 MRR 不升（0.9865）、60 条校准集传导率 0.0% 不变；根因=信号分正确但被其他 7 路信号稀释，单抬不足以翻转融合序 |
| 公开基准 MRR ≥ 0.9845 | ✅ 0.9901（A 路交付权重单测）/ 0.9917（合流最终态，§0 表） |
| 合成集 MRR ≥ 0.8585 | ✅ 0.8585（A 路当时口径）/ **1.000**（B 路重构数据集合流后，§0 表） |
| LoCoMo 4 条 ≥2 条修复 | ✅ 4 条 rank 1/9/4/10 → **1/5/4/4**，全部进 top-5（原 2 条未进 top-5），按「进 top-5」口径 4/4 修复 |
| L3 上下文增强重评估结论 | ✅ depth=2 负收益（0.9900→0.9842），**保持关闭**（结论记录于 `rank_weights_en_r5.json` 注释） |

交付权重 `eval/weights/rank_weights_en_r5.json`：semantic=12.0（单维扫描 2→50 选定的双达标最优点），其余与 R4 一致。

### 1.2 B 路 — 合成集 multi-hop 样本重构 + temporal 补齐 + fact 归一（`d6c7be7`）

| 验收项（§B.5） | 结果 |
|---|---|
| 合成集 multi-hop 主 MRR ≥ 0.85 | ✅ **1.000**（原 0.500 结构性下限；根因=双元素 Expected 把跳板放 [0] 位，重构为单元素 hop-2） |
| temporal 桶准确率 ≥ 85% | ✅ **1.000**（temporal 样本 130 条：20 真实 + 110 模板，as_of/since/before/between 四形态） |
| fact 假阳性 ≤3 条（C 独立口径） | ✅ 5→**3**（边界归一化 + 边界对齐护栏剔除 2 条碎片 FP；剩余 3 条登记 P4） |
| 公开 MRR ≥ 0.9845 / 合成 ≥ 0.8585 | ✅ 0.9852（R4 权重下 temporal 合并）/ 合流后 0.9917 + 1.000（§0 表） |
| 重构前后数字同时展示 | ✅ `all_multihop_dual.jsonl` 历史双元素口径保留；`r5b_synth_dual` vs `r5b_synth` 对照（0.500→1.000） |
| gofmt/vet/test | ✅ 0 文件 / VET_OK / 10 个 fact 单测 PASS |

### 1.3 C 路 — GDPR + 检索 P95 + 开放 API/SDK（`a09b790`）

| 验收项（§C.5） | 结果 |
|---|---|
| GDPR 三件套 PASS | ✅ `CascadeDelete`（tenant+user 级联 100%）/ `ExportAll`（JSON 可携）/ `RightToBeForgotten`（remove+匿名化，`deleted=true, trace_kept=false`）；5 个单测全绿；经可选 `ComplianceBackend` 类型断言接入，**零改 StorageBackend 契约** |
| 检索 P95 < 200ms | ✅ 1000 次基准 **P95=0ms**（mean ~9.3µs/op；热点缓存命中 0ms）；HotCache 为只读旁挂，不改 Score/排序 |
| SDK README ≥6 端点 + Python/JS CRUD 跑通 | ✅ ≥9 端点；Python/JS 示例端到端跑通 save→search→recall→update→export→forget→search-after-forget |
| 现有检索 MRR 不回归 | ✅ kernel 全量测试通过（rank_fusion/multihop/tools/recall） |
| gofmt/vet/test | ✅ 全绿 |

⚠️ C 路含**构建修复**（报告 §三）：A/B 遗留 kernel 编译回归（`backend_abstract.go` 重复声明+语法错误、`clamp01` 重复、`lib/pq` 死导入、`Upsert` 两值调用、`goal.go`/`package.go` 导入问题）已最小化修复——登记 P2（已关闭）。

### 1.4 D 路 — 北极星 M1/M2 + 控制面压缩（`cdc478d`）

| 验收项（§D.5） | 结果 |
|---|---|
| M1/M2 口径实现 + JSON 报告 | ✅ `eval/polaris/` 独立 subpackage；4 份 JSON 报告 |
| 零 token 覆盖率 ≥0.95 | ✅ **0.9661**（570/590，代理语料）；hit@1 0.9559；分桶 sh/mh/od/temporal 全 1.0，reference_frame 0.667（登记 P3） |
| 压缩率 ≥20% 且覆盖率不降 | ✅ **35.77%**（29050→18660 token）；压缩后 M1 **0.9661 逐位不降**（hit@1 0.9559→0.9644 微升） |
| 公开/合成 MRR 不回归 | ✅ run_kpi 同配置交叉对拍全桶 1.0；北极星零检索逻辑改动 |
| gofmt/vet/test | ✅ 0 文件 / VET_OK / 10 个单测 PASS |

M2 控制面 token 占比 0.4375（压缩后 0.3447）——控制面开销小于数据面，「语言层表意密度」可量化面成立（当前由 Go 规则压缩占位兑现，见问题登记册「光明 light 语言问题」节）。

---

## 2. R4 残留项清除情况（任务书 §0.1 八项逐条）

| # | R4 残留项 | 路由 | R5 结局 | 状态 |
|---|---|---|---|---|
| 1 | reference_frame 权重仍 0.0（信号 100% 区分度但被关闭） | A | 纳入 8 变量坐标下降，1 轮收敛无收益，**明确结论保留 0.0** | ✅ 结论关闭 |
| 2 | char n-gram embedder 对合成集拖累 −1.5pp | A | BGE-m3 向量接入；合成集合流后 MRR 1.000 | ✅ 清除 |
| 3 | 公开假阳性 5 条（C 独立口径） | B | 边界归一化+护栏 → **3 条**（≤3 达标）；剩余登记 P4 | ✅ 达标（残余登记） |
| 4 | multi-hop 4 条 LoCoMo 未修复 | A | 向量匹配后 rank 1/5/4/4，**4/4 进 top-5** | ✅ 清除 |
| 5 | L3 上下文增强默认关闭 | A | depth=2 重评估负收益（0.9900→0.9842），**保持关闭** | ✅ 结论关闭 |
| 6 | 合成集 multi-hop 主 MRR 0.500（结构性） | B | 单元素 Expected 重构 → **1.000** | ✅ 清除 |
| 7 | 时序桶准确率 0.0%（无 temporal 样本） | B | temporal 样本补齐 → **1.000** | ✅ 清除 |
| 8 | 多信号融合准确率 0.0%（待人工复核） | B | 重构后该桶有样本且 **1.000** | ✅ 清除 |

**8/8 清除或结论关闭。**

> **收口追加（2026-10-13 19:20，P1 处置）**：R5-B 重构数据集已提交入库（用户拍板）。本提交纳入 `dataset/all.jsonl`、`single-hop.jsonl`、`multi-hop.jsonl`、`open-domain.jsonl`、`temporal.jsonl`、`all_multihop_dual.jsonl`——由**已提交的生成器** `eval/eval_generate.go`（seed=42，确定性）产出。入库前核验：① `injection.jsonl`/`pollution.jsonl`/`reference_frame.jsonl` 重新生成后与 committed 版**逐字节一致**（生成器确定性证据）；② multi-hop 150 条全部单元素 Expected + `expected_memory_ids_primary`；③ single-hop 50 个 (实体,主题) 组 × 周一/周二/周三 三条互斥事实，query 括号为**消歧锚**（生成器 line 73 注释的设计意图，非答案泄漏构造）；④ temporal 130 条全部带 as_of + second_version。**P8 以此定论**：committed 生成器实际产出 130 条 temporal（B 路报告「50 条」为 `-count` 参数描述，以文件与生成器为准）。合流最终态指标（§0 表）即在此数据集上测得。剩余待办 P3/P4/P7 转入下一轮（R6）任务书。

## 3. roadmap v4 未启动泳道完成情况（任务书 §0.2）

| 泳道 | 任务 | 结果 |
|---|---|---|
| P7-A GDPR 合规 | `kernel/gdpr.go` 级联删除/导出/被遗忘权 | ✅ PASS（C 路） |
| P7-B 检索性能 P95<200ms | `kernel/perf.go` | ✅ P95=0ms（C 路） |
| P7-C 开放 API/SDK | `sdk/` | ✅ ≥9 端点 + Python/JS 跑通（C 路） |
| 北极星 M1/M2 + ≥0.95 | `eval/polaris/` | ✅ 口径实现 + 代理语料 0.9661 达标；**真实语料到位后正式判定**（D 路） |

---

## 4. 问题登记册（本轮发现，含光明 light 语言问题单独标记）

### 4.1 光明 light（light-merge 语言项目）问题：**本轮（R5）无**

R5 四路全部落在 `lightmem/`（Go 实现），未触碰 `light-merge/` 语言项目（C 路报告红线清单明确留痕）；四份路报告全文检索无语言层缺陷记录。**无需向 light 缺陷账（`docs/功能对标/语言缺陷账.md`）追加编号。**

唯一关联事项（产品路线依赖，**非语言缺陷**）：M2「控制面光明语言表意密度」主张目前由 `compress_light.go` 的 Go 规则压缩**占位兑现**；待控制面（编排/策略/配置）真正以光明语言表达时，M2 才能兑现定位文档 §7「独有指标」的完整主张。记为产品后续项，不记语言缺陷。

### 4.2 lightmem 侧问题

| # | 问题 | 级别 | 状态 | 说明 / 处置 |
|---|---|---|---|---|
| P1 | **R5-B 重构数据集未入库**：`d6c7be7` 不含 `dataset/all.jsonl` 等，重构版只在 0.88 远端工作区；D 路整树同步时被覆盖回 R138-B 旧版，致 multi-hop/temporal 假 0.0（isolate 证明 run_kpi 同语料同样 0.0，非检索回归） | 高 | **待处置** | 已在 0.88 用 B 路生成器（seed=42，确定性）复现数据集并验证（合流后全桶 1.0）。**建议：将 regenerated `dataset/all.jsonl` 提交入库，或明确「数据集由生成器现生成」的基线口径**——否则后续任何本地→远端同步都会重演 |
| P2 | A/B 遗留 kernel 编译回归（`backend_abstract.go` 重复声明+语法错误等 6 处） | 高 | ✅ 已关闭 | C 路最小化修复（`a09b790` 报告 §三）；建议后续轮次开工前先跑一次编译门禁 |
| P3 | reference_frame 桶覆盖率 0.667（北极星代理语料口径）：信号 100% 区分度但权重 0.0，单抬权重被其他 7 路信号稀释 | 中 | 未关闭 | A 路已给明确结论（纳入无收益）；突破需新机制（ref 信号门控/分层融合/权重联合搜索），留后续轮次立项 |
| P4 | fact 假阳性残留 3 条（C 独立口径）：`lme-031748ae_abs`×2（否定性回答语义冲突）、`lc-conv-49-D1_16`（碎片化） | 低 | 未关闭 | 护栏无法覆盖；需人工复核或语义级过滤（超 R5 不调 LLM 红线） |
| P5 | 远端工作区同步是破坏性操作：本地仓状态 ≠ 远端工作区时，整树 tar 同步覆盖远端未提交产物 | 中 | ✅ 已规范 | 已入项目记忆铁律（同步前远端 `git status` 核对） |
| P6 | `eval/polaris` 触发根 go.mod 需 `go mod tidy`（首个非 ignore 的 import kernel 的包） | 低 | ✅ 已关闭 | 补齐 8 条 kernel 既有间接依赖 + 新建 `go.sum`（`cdc478d`） |
| P7 | 真实语料 holdout 未到位（用户确认 5 天内）：北极星 0.9661 为**合成代理语料**口径，正式达标判定待真实语料 | 高 | 未关闭 | 接入协议已交付（`eval/polaris/README.md` §3）；语料落 `eval/dataset/holdout.jsonl` 后一条命令复跑 |
| P8 | 文档口径不一致：B 路报告称 `temporal.jsonl` 重生成 50 条，实际入库文件 130 条（A 路向量语料 2756 以 130 条为准） | 低 | 未关闭 | 以文件为准（130 条为 R5  canonical 态）；B 路报告描述待校 |

---

## 5. 验证矩阵（0.88，合流后最终态）

| 门禁 / 测量 | 命令 | 结果 |
|---|---|---|
| gofmt | `gofmt -l eval/polaris/`（四路改动文件均已格式化） | 0 文件 |
| go vet（root 模块） | `go vet ./...` | RC=0 |
| go test（root 模块） | `go test ./...` | RC=0（polaris 10 单测 + fact 10 单测） |
| go vet/test（kernel 模块） | `cd kernel && go vet ./... && go test ./...` | RC=0 / RC=0 |
| 公开基准最终态 | `run_kpi.go -dataset benchmarks/dataset/all.jsonl -temporal-samples dataset/temporal.jsonl -weights rank_weights_en_r5.json -embedder vector -now-fixed 2026-10-13` | MRR 0.9917 / ID 1.0 / fact v4 0.9607 / temporal 1.0 / pollution 0 / FP 0 |
| 合成集最终态 | 同配置 `-dataset dataset/all.jsonl` | MRR 1.000 / 全桶 1.0 |
| 北极星 | `polaris_eval -holdout eval/dataset/holdout_proxy.jsonl` | M1 0.9661 / M2 0.4375 |
| 压缩对照 | `-mode compress` + `-compress` 复评 | 35.77% / 覆盖率 0.9661 不降 |

> 注：`-now-fixed 2026-10-13` 为北极星/交叉对拍的确定性基准（杜绝 time.Now() 进 freshness 信号）；A/B/C 路历史报告用各自启动时刻，MRR 级指标不受影响（freshness 权重 0.1）。

## 6. 提交与推送

| 路 | commit | 内容 |
|---|---|---|
| A | `81f7370` | embedder_vector.go + gen_vectors.py + run_kpi/tune_v2 改造 + r5 权重 + A 报告 |
| B | `d6c7be7` | eval_generate 重构 + generate_temporal + run_kpi 归一/护栏 + fact 单测 + B 报告 |
| C | `a09b790` | gdpr/perf/router/sdk + kernel 构建修复 + C 报告 |
| D | `cdc478d` | eval/polaris 全套 + holdout_proxy + D 报告 + 3 份 JSON + go.mod/go.sum |
| 收口 | 本提交 | 本整合报告 + roadmap 收口留痕 + 最终态补测 JSON |

**push**：经用户 2026-10-13 示意执行，目标 `origin`（gitea `http://192.168.1.5:3000/skywalk/lightmem.git`），推后 `git ls-remote` 复核。

## 7. 遗留与下一步

1. **P1 数据集入库**（高优先）：regenerated `dataset/all.jsonl` 是否随收口提交，请用户拍板；若不入库，README/报告须明示「数据集由 `eval_generate.go -seed 42` 现生成」。
2. **P7 真实 holdout**：到位后落 `eval/dataset/holdout.jsonl`，跑 `go run ./eval/polaris/ -holdout eval/dataset/holdout.jsonl -report polaris_real`，出正式北极星数。
3. **P3 ref 桶**：0.667 是北极星唯一弱桶；下轮可立「reference_frame 信号门控/分层融合」小项。
4. **P4 残留 3 条 FP**：人工复核清单已定位到样本 ID。
5. **控制面光明语言**：产品侧把编排/策略迁到光明语言表达时，M2 口径可直接复用（polaris_token 的指令模板常量对即替换点）。
