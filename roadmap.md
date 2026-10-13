# 光明记忆（LIMS）· 5 天加速开发路线图

> **版本**：v4（最终版）｜ 日期：2026-10-12 ｜ 对应 HTML：`lightmem/roadmap-v4.html`
> **时间轴**：`2026-10-12 ~ 2026-10-17`（约 5 天，由原 3 个月窗口按 **AI Agent 加持 20 倍**压缩）
> **基线**：lightmem commit `f8b1e6d`，KPI 93.0%；P1–P6 / P8 / P9 已交付，P7 本轮启动
> **交付位置**：`lightmem/` 项目目录（**独立于** `light-merge/` 根目录的 light 语言项目任务）

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
| **P7 合规与优化** | GDPR / 性能 P95 / SDK / 公开基准对标 / 多语言 | GDPR → 性能 → SDK → 基准数据集 → 对标 → 多语言 | 全 confirmed |
| **FreeBSD + ZFS 真机部署** | ZFS+jail、pf 加固、Family 档启动、快照容灾、集合体 | ZFS+jail → pf → Family 验收 → 快照 → 集合体 | 全 confirmed |
| **AI 协同与北极星评测** | 真实语料、M1/M2 口径、控制面压缩、≥0.95 达标 | 语料接入 → M1/M2 → 压缩优化 → ≥0.95 达标 | 全 confirmed |
| **应用包生态真机验证** | 首批 8 包装配、包记忆抽取、拓扑排序 | 8 包装配 → 包记忆+拓扑排序 | 全 confirmed |

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

**待办**：① regenerated `dataset/all.jsonl` 入库拍板（P1，否则数据集口径以生成器 seed=42 现生产物为准）；② 真实语料 holdout 到位后跑正式北极星（P7）；③ reference_frame 桶 0.667 突破机制下轮立项（P3）。

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

## 五、交付物清单（本轮 5 天）

| 阶段 | 新增文件 | 说明 |
|---|---|---|
| P7-A | `kernel/gdpr.go` | 合规三件套 |
| P7-B | `kernel/perf.go` | 性能与缓存 |
| P7-C | `sdk/` | REST 文档 + Python/JS SDK |
| 基准数据集 | `eval/benchmarks/*` | LongMemEval/LoCoMo/BEAM 子集 |
| P7-E | `kernel/i18n/` | 国际化本地化 |
| 部署 | `manuals/p1-freebsd-zfs-setup.md` 执行记录 | ZFS+jail+pf 实机 |
| 北极星 | `eval/reports/polaris_*.json` | M1/M2 与 ≥0.95 达标 |

---

## 六、风险与应对

| 风险 | 影响 | 应对 |
|---|---|---|
| 外部基准数据集 5 天内获取受限 | 高 | 独立任务优先调度；先落种子子集，对标随取随跑 |
| 2 台机器分工协调 | 中 | 明确 Coordinator/Worker 角色分配，跨节点 pre-filter 先行 |
| 5 天窗口任务密集 | 中 | 泳道并行，依赖前置项提前锁定；里程碑单日验收 |
| 北极星语料延迟到位 | 高 | 语料到位即接入；M1/M2 口径可先用合成样本预跑 |

---

> 本路线图为**最终版**（HTML `roadmap-v4.html` 与本文档一致）。所有改动须遵守红线 1–3；本机仅编辑，实跑在 0.88。