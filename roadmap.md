# 光明记忆（LIMS）· 5 天加速开发路线图

> **版本**：v5（真实进度修订）｜ 日期：2026-10-16 ｜ 对应 HTML：`lightmem/roadmap-v5.html`（已同步本轮修订；roadmap-v1~v4.html 为历史静态快照，保留备查）
> **时间轴**：`2026-10-12 ~ 2026-10-17`（约 5 天，由原 3 个月窗口按 **AI Agent 加持 20 倍**压缩）
> **基线**：lightmem commit `2b75731`（R15 收口，2026-10-16），北极星 M1=1.0000，门禁 PASS 12/0/0
> **交付位置**：`lightmem/` 项目目录（**独立于** `light-merge/` 根目录的 light 语言项目任务）

---

## 〇、真实进度速览（2026-10-16 核验）

> 本节为 v5 新增，逐项以代码/报告/git log 实核，标注文档声称与真实状态的差异。

### 〇.1 阶段状态矩阵（修订前 README 声称 ↔ 真实进度 ↔ 修订后状态）

| 阶段 | 修订前 README | 真实状态 | 核验证据 |
|---|---|---|---|
| P1 核心骨架 | ✅ | ✅ 已交付 | `kernel/model.go`、`embedder*.go`、`backend_{sqlite,pg}.go`（README 原先写的 MemoryExtractor 未实现，已更正） |
| P2 智能路由与融合 | ✅ | ✅ 已交付 | `kernel/router_mem.go`、`writepolicy.go`、`rank_fusion.go:146-154`（8 路信号全定义） |
| P2.5 评测集建设 | ✅ | ✅ 已交付 | `eval/eval_generate.go`、`run_kpi.go`、`dataset/all.jsonl`（570 条） |
| P3 融合反馈与评测 | ✅ | ✅ 已交付 | `rank_fusion.go:509/556`、`router.go:97/294`、`router_mem.go:154/236`、`router_tools.go:250` |
| P4 生命周期 | 🔜 | ✅ **已交付** | `lifecycle.go:252/286/316/347`、`consolidate.go`、`flush.go`、`forgetting_drift.go` + 5 测试文件 |
| P5 外部连接器 | 🔜 | ⚠️ **部分→骨架补全（R16 S3）** | `kernel/connector.go:18/53/160`（DB+Doc）；**R16 S3 新增 `kernel/connector_app.go`**（AppConnector + git 元数据扫描，7 项单测） |
| P6 目标与共享 | 🔜 | ✅ 已交付（单机） | `kernel/goal.go:15/149/181/205`、`model.go:64`；跨节点撤销传播骨架（R16 S2 `topology.go` RevocationStore） |
| P7 合规与优化 | 🔜 | ✅ **已交付** | `kernel/gdpr.go`、`perf.go`、`i18n/`、`metrics.go`、`sdk/{python,js}`、`eval/benchmarks/`、`docs/api.md` + `api.openapi.yaml` |
| P8 集合体与多后端 | 🔜 | ✅ **已交付（R16 骨架 + R17 真机落地）** | **R16** `kernel/backend_qdrant.go`（REST 后端 + profile 接线）+ `node_role.go`/`topology.go`（四角色 + 拓扑 + 撤销广播）；**R17 S1** PG 18.6 迁入 lims-pg jail（10.0.10.10，手册 §11A.5）；**R17 S2** Qdrant 1.19.1 真机部署（lims-qdrant 10.0.10.60）+ 真机联调 4/4 PASS + 幂等部署脚本（§11A.6）；**R17 S3** 跨节点 gRPC 真实现（`node_rpc.go` + `proto/`，默认关，本地回环 + vet 通过，§11A.7） |
| P9 应用包生态 | 🔜 | ✅ **已交付** | `kernel/package*.go`、`eval/packages/manifests/`（8 包齐）、`eval/e2e/packages/package_e2e_test.go` |

> ✅ **本轮已同步**：`README.md` 开发计划表已按上表真实状态更新（P4/P5/P6/P7/P9 由 🔜 更正，P1 移除未实现的 MemoryExtractor）。

> ✅ **R21 新增（多 Agent 接入）**：内核**原生 MCP**（`kernel/mcp.go` + `mcp_transport.go` + `cmd/lightmem-mcp`，**双传输** streamable-http 主 / stdio 兜底，纯 stdlib），
> 13 个 MCP 工具（7 既有 + 6 工作流：`goal_create`/`goal_progress`/`goal_state`/`plan_list`/`step_add`/`summary_add`）；
> **agent token 鉴权 + 写方身份强制**（`kernel/auth.go`，默认透传零回归）；各 Agent 接入模板 8 家（`integrations/agent-onboarding/`）。
> 同轮关闭 **G8（PG private 越权读，P0 安全）**、**G9（PG `ON CONFLICT` 丢字段致更新静默失效，P0 功能）**、B-§6.2、D-2、MCP→REST 身份接缝。
> 详见 `.dumate/inbox/_R21_收口报告.md`；门禁 PASS 14 / FAIL 0 / SKIP 0。

### 〇.2 四泳道收口状态

| 泳道 | roadmap v4 计划 | 真实收口 |
|---|---|---|
| **P7 合规与优化**（GDPR/P95/SDK/对标/i18n） | 10-12~10-17 | ✅ **全交付**：R5-C + R8-A + R8-B 完成；i18n 已于 10-14 前交付 |
| **FreeBSD + ZFS 真机部署** | ZFS→pf→Family→快照→集合体 | ⚠️ **部分**：ZFS dataset 8 个 ✅、jail 5 个启动 ✅、Family 档 PG+pgvector 启动 ✅（r11c_family_real）；⚠️ jail vnet/epair 未通、PG 暂在宿主机；快照容灾、集合体未做 |
| **AI 协同与北极星评测** | 语料→M1/M2→压缩→≥0.95 | ✅ **超标完成**：北极星 M1=**1.0000**（n=800 holdout_v2），M2 控制面占比 0.2498，数据面压缩 −0.353 |
| **应用包生态真机验证** | 8 包装配→包记忆+拓扑 | ✅ **已交付**：8 包 manifest 齐、InstallOrder/ExtractPackageMemory + e2e 全通 |

### 〇.3 北极星真实判定（替代 R5 留痕的 0.9661）

| 指标 | R5 留痕 | R15 最新 | 变化 |
|---|---|---|---|
| M1 零 token 覆盖率 | 0.9661（合成代理语料） | **1.0000**（800/800 真实 holdout_v2） | ↑ 达标超标 |
| hit@1 | — | **1.0000**（800/800） | ✅ |
| M2 控制面 token 占比 | — | **0.2498**（43126/172663） | ✅ |
| 数据面压缩率 | 35.77% | **−0.353**（200295→129537） | ✅ |

> **语料口径提示**：R13/R14/R15 报告的 `config.holdout` 指向 `eval/holdout/holdout_v2.jsonl`（800 条），但 `corpus_note` 仍写"holdout.jsonl（正式判定）"。**实际评测对象是 holdout_v2**，450 条纯 LoCoMo 的 `holdout.jsonl` 未被直接评测。该 corpus_note 为留痕笔误，建议在 R16 北极星产出时修正。

---

## 一、执行原则

| 原则 | 说明 |
|---|---|
| 红线 1 | 只增不改 Schema 语义 |
| 红线 2 | 本机仅编辑 + SSH/传输，**禁止本机实跑**（编译/测试/运行在 0.88） |
| 红线 3 | **不调 LLM 生成评测集**，固定 seed 确定性生成 |
| 北极星 | 真实语料 holdout 零 token 覆盖率 **≥ 0.95** |
| 外部基准数据集 | 需**单独任务**获取/制作（LongMemEval / LoCoMo / BEAM 子集） |

---

## 二、四泳道总览

| 泳道 | 说明 | 关键里程碑 | 状态 |
|---|---|---|---|
| **P7 合规与优化** | GDPR / 性能 P95 / SDK / 公开基准对标 / 多语言 | GDPR → 性能 → SDK → 基准数据集 → 对标 → 多语言 | ✅ 已交付（R5-C + R8-A + R8-B） |
| **FreeBSD + ZFS 真机部署** | ZFS+jail、pf 加固、Family 档启动、快照容灾、集合体 | ZFS+jail → pf → Family 验收 → 快照 → 集合体 | ⚠️ 部分（Family 启动 ✅，快照/集合体未做） |
| **AI 协同与北极星评测** | 真实语料、M1/M2 口径、控制面压缩、≥0.95 达标 | 语料接入 → M1/M2 → 压缩优化 → ≥0.95 达标 | ✅ **超标**（M1=1.0000 ≥ 0.95） |
| **应用包生态真机验证** | 首批 8 包装配、包记忆抽取、拓扑排序 | 8 包装配 → 包记忆+拓扑排序 | ✅ 已交付 |

> 4 项原 tentative（外部基准数据集、集合体 2 台机器、真实语料 5 天内到位、P7-E 多语言）已由用户确认 → 全部转 **confirmed**。

---

## 二·五、用户确认记录（2026-10-12）

> **本小节为对 §二 四泳道全部转 confirmed 的留痕**。原始 4 项待确认已由用户逐项拍板，结论如下：

| # | 待确认项 | 用户结论 | 对路线图的影响 | 依据 |
|---|---|---|---|---|
| 1 | P7-D 公开基准对标依赖外部数据集 | **5 天内可用**，但需**单独开一个任务**去获取/制作 LongMemEval / LoCoMo / BEAM 子集 | §三 3.1 新增独立条目"外部基准数据集获取/制作"（10-12 ~ 10-14），作为 P7-D（10-14 起）的**唯一前置**；P7-D 转 confirmed | 用户 2026-10-12 口头确认 |
| 2 | 集合体 Cluster 需要 ≥2 台机器 | **有 2 台机器**做 Coordinator / Worker | §三 3.2"集合体 Cluster 节点验证"转 confirmed，注明 2 机分工 | 用户 2026-10-12 口头确认 |
| 3 | 北极星真实语料 holdout 何时就绪 | **5 天内到位** | §三 3.3"真实语料 holdout 接入"（10-13 ~ 10-14）转 confirmed，可支撑后续 M1/M2 与 ≥0.95 达标 | 用户 2026-10-12 口头确认 |
| 4 | P7-E 多语言是否本轮就做 | **本轮就做**，不顺延 | §三 3.1"P7-E 多语言国际化"（10-16 ~ 10-17）转 confirmed | 用户 2026-10-12 口头确认 |

**版本用途说明**：本路线图为 5 天冲刺（2026-10-12 ~ 2026-10-17）的**讨论 / 执行稿**——时间维度按 AI Agent 加持将原 95 天压缩 20 倍，**不代表对外发布的 95 天路线**。对外发布版以 `lightmem/roadmap-v1.html`（原始 3 个月窗口）为参考基线。

---

## 三、时间排布表（2026-10-12 ~ 10-17）

### 3.1 P7 合规与优化

| 任务 | 起始 | 结束 | 类型 | 交付物 | 验收标准 |
|---|---|---|---|---|---|
| **P7-A GDPR 合规** | 10-12 | 10-13 | bar | `kernel/gdpr.go` | 级联删除完成率 100%、数据导出、被遗忘权 |
| **P7-B 检索性能 P95<200ms** | 10-13 | 10-14 | bar | `kernel/perf.go` | 内核记忆检索链路 P95 < 200ms、热点缓存 |
| **P7-C 开放 API/SDK** | 10-13 | 10-14 | bar | `sdk/` | REST API 文档、Python SDK、JS SDK |
| **外部基准数据集获取/制作**（独立任务） | 10-12 | 10-14 | bar | `eval/benchmarks/` 子集 | 固定 seed 不调 LLM，10-14 前就绪 |
| **P7-D 公开基准对标** | 10-14 | 10-17 | bar | `eval/benchmarks/` 报告 | LongMemEval / LoCoMo / BEAM 子集适配复跑，记录结果 |
| **P7-E 多语言国际化** | 10-16 | 10-17 | bar | `kernel/i18n/` | 国际化与本地化本轮完成 |

### 3.2 FreeBSD + ZFS 真机部署（验证机 192.168.0.88）

| 任务 | 起始 | 结束 | 类型 | 交付物 | 验收标准 |
|---|---|---|---|---|---|
| **ZFS dataset + jail 划分** | 10-12 | 10-13 | bar | ZFS dataset + lims-pg/lims-router jail | `zfs list` 列出全部 dataset；`jail -l` 列出 jail |
| **pf 规则 + 安全加固** | 10-13 | 10-14 | bar | pf 规则 + checklist | `pfctl -sr` 仅暴露 443/8443；`checklists/freebsd-security-checklist.md` |
| **Family 档真机启动验收** | 10-13 | 10-14 | milestone | `lims-family.profile.yaml` 实机 | 不靠 override 启动，`/memories`、`/search` 走真 PG+pgvector+jail |
| **ZFS 快照 + 容灾 + 备份包** | 10-14 | 10-15 | bar | ZFS snapshot / zfs send / backup 包 | 快照可恢复，`zfs send` 容灾链路可用 |
| **集合体 Cluster 节点验证** | 10-15 | 10-17 | bar | Coordinator/Worker/Archive/Gateway 节点 | 跨节点 pre-filter 与 revocation 广播，**2 台机器分工** |

### 3.3 AI 协同与北极星评测

| 任务 | 起始 | 结束 | 类型 | 交付物 | 验收标准 |
|---|---|---|---|---|---|
| **真实语料 holdout 接入** | 10-13 | 10-14 | bar | holdout 语料落库 | 固定 seed 不调 LLM；用户确认 5 天内到位 |
| **零 token 覆盖率口径 M1/M2** | 10-14 | 10-15 | bar | eval M1/M2 口径 | M1 覆盖率 + M2 控制面 token 占比口径实现，复跑评测 |
| **控制面光明语言压缩优化** | 10-15 | 10-17 | bar | 数据面+控制面双重压缩迭代 | 低 token 长期运行主特色落地 |
| **北极星 ≥0.95 达标验证** | 10-16 | 10-17 | milestone | 北极星评测报告 | 真实语料 holdout 零 token 覆盖率 ≥ 0.95 达成 |

### 3.4 应用包生态真机验证

| 任务 | 起始 | 结束 | 类型 | 交付物 | 验收标准 |
|---|---|---|---|---|---|
| **首批 8 包 Family 档装配** | 10-13 | 10-14 | bar | gitea/git-connector/registry/backup/search/llm-gateway/mesh-sync/webui | 安装、配置、升级、卸载全链路 |
| **包记忆抽取 + 拓扑排序真机验证** | 10-14 | 10-15 | bar | ExtractPackageMemory / InstallOrder | 包成为被记忆的一等实体（装包/配包/用包/沉淀包经验四环节） |

---

## 三·五、R5 检索能力升级收口留痕（2026-10-13）

> R5 四路（A 向量检索 / B 样本重构 / C 合规性能 SDK / D 北极星）全部判绿，合流最终态实测（0.88）：**公开基准 MRR 0.9917**（R4 0.9845）、**合成集 MRR 1.000**（R4 0.8585，multi-hop 主口径脱离 0.500 结构性下限）、fact v4 96.07%、temporal/多信号融合 1.000、污染率 0；**北极星 M1 零 token 覆盖率 0.9661 ≥ 0.95**（合成代理语料口径，真实 holdout 到位后正式判定）、控制面压缩率 35.77% 且覆盖率不降；GDPR 三件套 PASS、检索 P95=0ms、SDK Python/JS 跑通。R4 残留 8 项全部清除或结论关闭。commit：A `81f7370` / B `d6c7be7` / C `a09b790` / D `cdc478d`。整合报告：`eval/reports/p7d_r5_summary.md`（含问题登记册 P1–P8；**光明 light 语言项目问题：本轮无**）。

**待办**：① regenerated `dataset/all.jsonl` 入库拍板（P1，否则数据集口径以生成器 seed=42 现生产物为准）；② 真实语料 holdout 到位后跑正式北极星（P7）；③ ~~reference_frame 桶 0.667 突破机制下轮立项（P3）~~ **已闭环（R6-A）**：`eval/weights/rank_weights_r6_ref.json` 激活 ref 信号后，ref 桶 M1 0.667 → 1.000，公开 MRR 0.992（基线 0.9917 持平），其余四桶零回归；P3 融合反馈与评测已随 R138-B 交付（VoteFusion / FeedbackLoop / retrieval_trace / 8 路信号，总体 KPI 93.0% 无退化，见 `.dumate/inbox/_R138_B线收口报告.md` v5）。

---

## 四、泳道间依赖

| 依赖 | 前置 | 后置 |
|---|---|---|
| 外部基准数据集 | **独立任务**（10-12~10-14） | P7-D 公开基准对标（10-14 起） |
| ZFS+jail 划分 | 无 | pf 加固 → Family 档启动 |
| Family 档启动 | pf 加固完成 | 首批 8 包装配、快照容灾 |
| 真实语料 holdout | 用户提供（5 天内到位） | M1/M2 口径 → 压缩优化 → ≥0.95 达标 |
| 集合体 Cluster | ZFS 快照完成 | —（独立里程碑） |

---

## 五、交付物清单（本轮 5 天 + 已收口）

| 阶段 | 新增文件 | 说明 | 真实状态 |
|---|---|---|---|
| P7-A | `kernel/gdpr.go` | 合规三件套 | ✅ 已交付（R5-C） |
| P7-B | `kernel/perf.go` | 性能与缓存 | ✅ 已交付（R5-C） |
| P7-C | `sdk/` | REST 文档 + Python/JS SDK | ✅ 已交付（R5-C + R9-C 收口） |
| 基准数据集 | `eval/benchmarks/*` | LongMemEval/LoCoMo/BEAM 子集 | ✅ 已交付（650 条，R8-B） |
| P7-E | `kernel/i18n/` | 国际化本地化 | ✅ 已交付（R8-A） |
| 部署 | `manuals/p1-freebsd-zfs-setup.md` 执行记录 | ZFS+jail+pf 实机 | ✅ **已交付（R16 jail vnet 实测 + R17 PG/Qdrant 迁 jail + 快照与回滚就绪）** |
| 北极星 | `eval/reports/polaris_*.json` | M1/M2 与 ≥0.95 达标 | ✅ **超标**（M1=1.0000，n=800） |
| 应用包生态 | `kernel/package*.go` + `eval/packages/` | 8 包生命周期 + 包记忆 + 拓扑排序 | ✅ 已交付（R11-B） |
| 门禁 | `scripts/gate.sh` | 统一门禁（R14 引入，R15 强化） | ✅ 已交付（PASS 12/0/0） |
| **P8 集合体** | `kernel/backend_qdrant.go` + `kernel/node_rpc.go` + `kernel/proto/` + `scripts/deploy_qdrant.sh` | Qdrant 真机后端 / 节点角色 / 跨节点 gRPC 查询 | ✅ **已交付（R17 收口：Qdrant 真机联调 + PG 迁 jail + gRPC 默认关）** |

---

## 六、风险与应对

| 风险 | 影响 | 应对 |
|---|---|---|
| 外部基准数据集 5 天内获取受限 | 高 | 独立任务优先调度；先落种子子集，对标随取随跑 |
| 2 台机器分工协调 | 中 | 明确 Coordinator/Worker 角色分配，跨节点 pre-filter 先行 |
| 5 天窗口任务密集 | 中 | 泳道并行，依赖前置项提前锁定；里程碑单日验收 |
| 北极星语料延迟到位 | 高 | 语料到位即接入；M1/M2 口径可先用合成样本预跑 |

---

> 本路线图为 **v5 真实进度修订版**，**HTML 已同步**为 `lightmem/roadmap-v5.html`（新增 P8 泳道、更新四泳道真实进度、画布高度扩展至 1208px）。roadmap-v1~v4.html 是历史静态快照，保留备查，不作为权威。所有改动须遵守红线 1–3；本机仅编辑，实跑在 0.88。

---

## 三·六、R24 收口留痕（2026-10-19）

> 轮次：R24「索引-内容解耦 + 静态记忆快车道（3FS / Engram 架构映射）」，五条线 T0/A/B/C/D/E。
> 基线 `c908091`（R23.3）；北极星 **M1=1.0000**（七个数字与 R15 收口值逐位相同）；门禁 **PASS 12 / FAIL 1 / SKIP 1**（FAIL 项 `[1b]` 11 个文件**全为历史遗留**，与干净基线逐行相同 ⇒ 新增 FAIL = 0）。

| 线 | 交付 | 关键判据 |
|---|---|---|
| **T0** | 0.88 对齐并重装到 `c9080917a6de...`；L-225 热修；接缝预埋零回归对拍 | PG 真机 public 跨租户可读 + 四值零回归；三端点 12 条记录 sha256 全等 |
| **A** | `kernel/lookup.go`：L2 确定性查找表 + 快车道（Engram 映射） | 34 条查询跨树对拍 sha256 全等；快慢一致率 30/30；bench ≈ **1/33.8** |
| **B** | `kernel/reindex.go`：`Reconcile`/`RebuildIndex` + 可选接口 `VectorBackfiller`/`VectorIndexAuditor`；L-226/L-227 | Qdrant 重建后 `zero=0 pending=0`；`Reconcile` 幂等且只读；三后端 superseded 一致 |
| **C** | `kernel/access.go`：`AccessTracker` + 访问模式分类 + 死引擎接线（L-223/L-224） | `access_count` 真实递增；衰减误差 <1e-9；`-race` 绿；budget 引擎 moved 4 |
| **D** | `kernel/refstore.go`：L3 引用式存储（RefStore） | 21 Test（含中文/1MB/100 随机条目往返）；篡改被拒；`PurgeRefsForMemories` GDPR 级联 |
| **E** | 向量产物重建 + 北极星复跑 + gate.sh 登记 + 收口报告 | **M1=1.0000**；`NEW_FILES` 追加 `lookup:12 reindex:12 access:12 refstore:12` |

### ⚠️ 北极星向量产物：只在 0.88，复跑必须去 0.88

`eval/vectors/{vectors.npy, vectors_meta.json, vectors_index.jsonl}`（4595 条，18.8MB）**从未入 git**，
只存在于 **0.88** 的 `/home/workbuddy/lightmem/eval/vectors/`；`.gitignore:31-35` 已显式排除。
本地仓库只有生成/合流脚本（`gen_vectors.py`、`gen_vectors_inc.py`、`merge_r12_final.py`、`merge_vectors_inc.py`）。

**R24-E 重建过程**（此前 A/B/C/D 四条线的判据⑥因此全部"未闭环"）：A 路 3597 条（`/home/workbuddy/r11a`）
+ B 路 600 条（`/tmp/r12b`）合流 → 4197 条 → M1=0.9962 → 定位缺 locomo_v2 文本 → 增量 398 条嵌入 → **4595 条** → M1=1.0000。
完整命令、md5 与工具链说明见 `lightmem.md` 附录 **D.3.1**。

### 对后续轮次的输入

1. `gofmt -l kernel/` 剩 `admin.go`、`authn_ory.go`（+ frontend 9 个），均基线遗留 → 独立 PR。
2. C 线线上 `FastHits` 恒 0，`deterministic` 分类生产不生效 → 需 A 线改 `router_mem.go` 一行。
3. 0.88 安装脚本 `kill` 后只 `sleep 1` 就启动，升级时可能 `bind: address already in use` 双停 → 改为轮询等端口释放。
4. B/C 线管理端点已按开关接线但**默认关**（`LIMS_ADMIN_REINDEX=1` / `LIMS_ADMIN_LIFECYCLE=1`）→ 运维按需开启。
5. Qdrant 1.19 的 point upsert **强制要求合法 vector 字段**（省略或写 null 均 400）——后续动 qdrant 写入路径前必读 `lightmem.md` D.11 L-227。


> **推送状态**：已于 2026-10-19 推送。commit `7cb0f83`（34 files，+7235/−38）；origin/gitea 与 gitcode 的 main 均为 `7cb0f83`（`git ls-remote` 复核一致）；github 因历史为「内容等价但 SHA 不同」的重放版，采用**非破坏性 cherry-pick** 同步，其 main 为 `f5e172b`（tree `ba38966...` 与 `7cb0f83` 的 tree 逐字节相同，已核对）。

## 三·七、R25 收口留痕（2026-10-19）

> 轮次：R25「收尾债清零 + solo 档向量补全 + 北极星换代」，六条线 T0/A/B/C/D/E。
> 基线 `6cd26e0`（R24 收口）；提交态实测基线 **PASS 11 / FAIL 2 / SKIP 1**（T0 勘误 E2，注意不是任务书初版的 12/1/1）；
> 收口门禁 **PASS 13 / FAIL 0 / SKIP 1**（**R15 门禁矩阵有记录以来首次 FAIL=0**；新增 FAIL=0、存量 FAIL 清零）。
> **北极星换代**：M1 连续四轮饱和 → 主判据改 **M2 控制面占比 ≤ 0.2498**；R25 实测 **M2=0.2498（持平达标）**，
> M1=1.0000 / hit@1=1.0000 / 压缩 35.33% / vetoed=0 全保持；P95 基线首建 **/search=45.56ms**（§9 红线 200ms 的 23%）。
> 详见 `lightmem.md` §D.3.1（换代口径 + R25 五数）与 §D.12（L-229~L-233 关闭记录）。

| 线 | 交付 | 关键判据 |
|---|---|---|
| **T0** | 0.88 对齐重装到 `6cd26e0…`；L-231 双脚本等端口释放（主+mcp 两处，勘误 E3）；基线勘误 E1/E2 | 连续两轮升级 18443/18444 均 200 且日志零 `bind: address already in use`；提交态基线 11/2/1 如实记录（判据 4 不达标部分转 A 线，非 T0 回归） |
| **A** | gofmt 历史债 12 文件清零（含 T0 勘误出的 `router_seam_dump_test.go`） | `git diff` +30/−30 完全对称、纯 tag 列宽重排零逻辑；`[1]`/`[1b]` 双转 PASS；commit `1a4a04e` |
| **B** | `kernel/sqlite_vector.go`（新）：`SQLiteVectorStore` BLOB + Go 侧暴力余弦；`VectorSearch` 可用化；`BackfillVectors` | §5.2 八判据全过（往返一致 N=120、HTTP 集成 200、降级路径在、默认关零回归、`go.mod`/`go.sum` 零变化、Backfill 幂等、11 Test、`-race` 绿） |
| **C** | `router_mem.go:236` 快车道接线 `MarkFast`（L-230，只改一处）+ PG 真机闭环 | FastHits=1 / 4快+1慢→deterministic；PG 独立库 `access_count=5`、衰减分 10.0 逐位；`-race` 绿 |
| **D** | `docs/档位能力矩阵.md`（新，四档逐格附 file:line/命令）+ README 边界说明（只增不删） | 与 `profiles/*.yaml` 逐一核对；B 线结论回填 solo 向量格；三运维开关落表 |
| **E** | 北极星换代复跑 + P95 基线 + gate 登记 + D.12/roadmap 留痕 + 收口报告 | 五数见上；`NEW_FILES += sqlite_vector:11`、`NEW_GO += R25 改动 17 个 .go`；`eval/scripts/bench_p95.py`（只读，标准库零依赖） |

### R25 收口实测锚点

- 北极星：`eval/reports/r25_final_holdout_20261019-032803.json`（832191 B，md5 `e98c7839dd0060ad7d83d7d131b272f9`，与 R24 报告同字节数、五数逐位相同）。
- P95 基线：`eval/reports/r25_p95_baseline.md`（/search P95=45.56ms，n=300 全 200）与 `r25_p95_baseline_vector.md`（/search/vector P95=17.91ms，285×200+15×503 多租户降级观察）。
- 命令：北极星复跑见 `lightmem.md` §D.3.1；P95 复现见 `eval/scripts/bench_p95.py` 文件头（solo/SQLite 独立临时库，勿碰生产 `lims_dsh`）。

### 对后续轮次的输入

1. ~~R25 全部改动未 commit / 未 push~~ **已推送**（2026-10-19 业主示意后）：origin/gitea = gitcode = `82d31e0`（`git ls-remote` 复核一致）；github = `5157d53ba0`（git 协议被代理 502 拦 → inline API 通道内容等价，tree `0a693558b9…` 与 `82d31e0` 逐字节相同，复跑「无改动，跳过」）。
2. **多租户向量预筛候选**：solo 向量检索全局 top-k 后才过滤可见性，双租户语料下 5% 查询降级（D.12 附·观察 2）；solo 单租户设计下不影响生产，引入多租户时再议。
3. sqlite-vec 备选路线（需 cgo，破零依赖承诺）仍待业主拍板。
4. 门禁 `[11]` PG 真机往返默认 SKIP（env 未配）；C 线已单独真机闭环，需全量 PG 门禁时 export 环境变量。
5. `.env` 的 `GITCODE_TOKEN` 已失效（API 401）——本次 gitcode 推送靠 GCM 缓存凭据；**下次 gitcode 推送前需更新该 token**。

## 三·八、R26 收口留痕（2026-10-19）

> 轮次：R26「可信度收口 · PG 门禁消 SKIP + solo 向量两实测缺陷修平 + 三远端推送固化」，六条线 T0/A/B/C/D/E。
> 基线 `82d31e0`（R25 收口，已推三远端）；T0 提交态 clean tree 基线门禁 **PASS 13 / FAIL 0 / SKIP 1**（唯一 SKIP=`[11]`）。
> **收口门禁（E 线实测，0.88）：PASS 14 / FAIL 0 / SKIP 0**——`[11]` 真机往返 13 项全 PASS，`[4]` 根 module（含 `eval/e2e/pg` 真跑）2/2 通过，**R15 门禁矩阵引入以来首次 SKIP=0**（日志 `_r26_088_gate.log`，md5 `da65f88e…`）。历程：E 线首次测量时 PG 主机 `10.0.10.10` 整机不可达（0.88 当日重启后），按文档化可移植性形态测得 13/0/1（不冒充 PASS）；业主修复 0.88（healthz 双 200）后 PG 恢复，重置测试库复跑达成 14/0/0。
> **北极星**：M1=1.0000 / hit@1=1.0000 / **M2=0.2498（主判据，持平达标）** / 压缩 35.33% / vetoed=0——报告与 R25/R24 **逐字节相同**（832191 B，md5 `e98c7839…`）。
> **P95**：`/search`=35.63ms（红线 50.12ms 内）；`/search/vector`=25.91ms 且 **300×200（503 清零）**——503 清零达成，但超 19.70ms 字面红线，根因 **L-239**（`visibleMemoryIDs` 每查询全量 `loadAll`，同机基线对拍 9.29ms→24.58ms）。
> 详见 `lightmem.md` §D.3.1（R26 五数 + P95）与 §D.13（L-234~L-237 关闭记录 + L-238/L-239 新登记）。

| 线 | 交付 | 关键判据 |
|---|---|---|
| **T0** | 0.88 对齐 `82d31e0` + clean tree 基线 13/0/1 + PG 独立库 `lightmem_r26_gate` | 基线在提交态实测；healthz 200/200 |
| **A** | `/var/db/lightmem/gate.env`（600、不入 git、无密码）+ `gate.sh` 头部可选 hook（+19/−0，`[11]` 判定段逐字未改） | `[11]` SKIP→PASS（真机往返 13 项）；干净库矩阵 **14/0/0**（首次 SKIP=0）；env 不在时与基线逐字节相同；新登记 **L-238**（脏库间歇 FAIL，待裁定） |
| **B** | `SearchAllowed` 余弦前可见租户预筛（B1）+ `PutBatch` 单事务批量 upsert（B2） | 双租户不再 503（E 线 bench 300×200 实测）；800 条 backfill 单次 409.7ms（基线 11700ms，~28×）；`go.mod`/`go.sum` 零变化；`-race` 全绿；测试 11→15 |
| **C** | `scripts/push_all_remotes.sh`（三远端命令序列 + token 排错 + inline 回退，不自动 push） | §6.2 五判据全过；README 指针（D 线） |
| **D** | 矩阵回填 B 线结论 + sqlite-vec「默认不做」决策 + PG env 运维段 + README `[11]` 行更新 | §7.2 四判据全过；与 A/B 实测逐项对齐 |
| **E** | 北极星复跑 + P95 对拍 + gate 登记（`sqlite_vector:11→15`）+ D.13/roadmap 留痕 + 收口报告 | 五数逐字节持平；`/search` 35.63ms；`/search/vector` 503 清零但 25.91ms 超红线（L-239）；门禁 **14/0/0**（首次 SKIP=0；PG 主机当日曾不可达，修复后复跑达成） |

### R26 收口实测锚点

- 北极星：`eval/reports/r26_final_holdout_20261019-145806.json`（832191 B，md5 `e98c7839dd0060ad7d83d7d131b272f9`，与 R25/R24 逐字节相同）。
- P95：`eval/reports/r26_p95_search.md`（35.63ms，300×200）、`eval/reports/r26_p95_vector.md`（25.91ms，300×200，503 清零）、`eval/reports/r25cmp_p95_vector.md`（`82d31e0` 同机对拍：285×200+15×503，11.47ms 混合分布）。
- 门禁：**14/0/0**（`_r26_088_gate.log`，md5 `da65f88e…`；首次 SKIP=0；PG 主机当日曾不可达，业主修复后复跑达成）。
- 命令：北极星复跑见 `lightmem.md` §D.3.1；P95 复现见 `eval/scripts/bench_p95.py` 文件头。

### 对后续轮次的输入

1. ~~**PG 主机 `10.0.10.10` 待排查**~~ **已关闭**：业主修复 0.88（重启后服务恢复）后 PG 同步恢复，14/0/0 已复跑达成（`_r26_088_gate.log`）。
2. **L-239（P2，B 线域）**：`visibleMemoryIDs` 改定向 `SELECT memory_id, user_id, tenant_id, visibility`（不取正文），恢复 `/search/vector` 延迟至基线量级；建议下一轮处理。
3. **L-238（P2，待业主裁定）**：`eval/e2e/pg` 夹具隔离（固定 `user` 字面量）+ `private` 可见性不校验 tenant 的语义问题（跨租户同名 user 可读）；推荐修夹具 + 门禁前置重置测试库。
4. sqlite-vec（cgo）路线：R26 已决策**默认不做**（破零依赖承诺），备选待业主拍板。
5. `.env` 的 `GITCODE_TOKEN` 仍失效（401）——下次 gitcode 推送前需更新（`scripts/push_all_remotes.sh` 有排错指引）。
6. R26 全部改动**未 commit / 未 push**（红线⑧），等业主示意。
