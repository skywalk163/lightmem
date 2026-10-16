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
| P8 集合体与多后端 | 🔜 | ✅ **骨架已交付（R16 收口）** | **S1** `kernel/backend_qdrant.go`（Qdrant REST 后端全接口实现 + profile 接线）；**S2** `kernel/node_role.go` + `kernel/topology.go`（四角色 + 拓扑 + 撤销广播）；门禁 [1b] 硬翻转；北极星三语料补评；ZFS 快照容灾 |
| P9 应用包生态 | 🔜 | ✅ **已交付** | `kernel/package*.go`、`eval/packages/manifests/`（8 包齐）、`eval/e2e/packages/package_e2e_test.go` |

> ✅ **本轮已同步**：`README.md` 开发计划表已按上表真实状态更新（P4/P5/P6/P7/P9 由 🔜 更正，P1 移除未实现的 MemoryExtractor）。

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

> **语料口径提示**：R13/R14/R15 报告的 `config.holdout` 指向 `eval/holdout/holdout_v2.jsonl`（800 条），但 `corpus_note` 仍写「holdout.jsonl（正式判定）」。**实际评测对象是 holdout_v2**，450 条纯 LoCoMo 的 `holdout.jsonl` 未被直接评测。该 corpus_note 为留痕笔误，建议在 R16 北极星产出时修正。

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
| 1 | P7-D 公开基准对标依赖外部数据集 | **5 天内可用**，但需**单独开一个任务**去获取/制作 LongMemEval / LoCoMo / BEAM 子集 | §三 3.1 新增独立条目「外部基准数据集获取/制作」（10-12 ~ 10-14），作为 P7-D（10-14 起）的**唯一前置**；P7-D 转 confirmed | 用户 2026-10-12 口头确认 |
| 2 | 集合体 Cluster 需要 ≥2 台机器 | **有 2 台机器**做 Coordinator / Worker | §三 3.2「集合体 Cluster 节点验证」转 confirmed，注明 2 机分工 | 用户 2026-10-12 口头确认 |
| 3 | 北极星真实语料 holdout 何时就绪 | **5 天内到位** | §三 3.3「真实语料 holdout 接入」（10-13 ~ 10-14）转 confirmed，可支撑后续 M1/M2 与 ≥0.95 达标 | 用户 2026-10-12 口头确认 |
| 4 | P7-E 多语言是否本轮就做 | **本轮就做**，不顺延 | §三 3.1「P7-E 多语言国际化」（10-16 ~ 10-17）转 confirmed | 用户 2026-10-12 口头确认 |

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
| 部署 | `manuals/p1-freebsd-zfs-setup.md` 执行记录 | ZFS+jail+pf 实机 | ⚠️ 部分（ZFS/jail/Family 启动 ✅，jail vnet 未通、快照未做） |
| 北极星 | `eval/reports/polaris_*.json` | M1/M2 与 ≥0.95 达标 | ✅ **超标**（M1=1.0000，n=800） |
| 应用包生态 | `kernel/package*.go` + `eval/packages/` | 8 包生命周期 + 包记忆 + 拓扑排序 | ✅ 已交付（R11-B） |
| 门禁 | `scripts/gate.sh` | 统一门禁（R14 引入，R15 强化） | ✅ 已交付（PASS 12/0/0） |
| **P8 集合体** | （未生成） | Qdrant 后端 / 节点角色 / 跨节点查询 | 🔜 **未开始**（唯一剩余主交付项） |

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