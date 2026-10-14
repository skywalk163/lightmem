# 光明记忆（LIMS）项目阶段性回顾与 R11 任务书

> **版本**：v1 ｜ **日期**：2026-10-14 ｜ **基线**：`75718de`（R10 收口态，已 push gitea main）
> **项目**：lightmem（光明记忆 LIMS）— 面向 AI Agent 的长期记忆系统
> **验证机**：192.168.0.88（FreeBSD 15.1-RELEASE-p1，go1.25.14，PG 18.6 + pgvector 0.8.2）

---

## 一、项目回顾：从 R1 到 R10

### 1.1 项目定位

**光明记忆（LIMS）** 是一个面向 AI Agent 的长期记忆系统，核心目标是：
- 让 AI Agent 像人一样拥有长期记忆
- 多租户 · 多用户 · 多 Agent · 可插拔后端 · 四层温度分层（L0 工作 / L1 动态 / L2 静态 / L3 归档）
- 北极星指标：真实语料 holdout 零 token 覆盖率 **≥ 0.95**

技术栈：Go 1.25，kernel 模块独立成 `github.com/lightmem/kernel`，根模块 `lightmem`。

### 1.2 十轮迭代演进

| 轮次 | 主题 | 核心成果 | 关键指标 |
|---|---|---|---|
| **R1–R4** | 基础能力建设 | 检索链路搭建、融合算法初版、评测框架 | MRR 从基线爬到 0.9845 |
| **R5** | 检索能力升级收口 | 向量检索接入、样本重构、合规性能 SDK、北极星口径初版 | 公开基准 MRR **0.9917**、合成集 MRR **1.000**、fact acc v4 **96.07%**、北极星 M1（合成代理）**0.9661** ≥ 0.95 |
| **R6** | 千脑智能深化与北极星达标 | 千脑投票共识 + 参考系深化（P3 突破：ref 桶 0.667→1.0）、连续学习 retention、北极星 holdout runner、评测有效性 | 参考系覆盖率突破、控制面压缩率 **35.77%** |
| **R7** | semantic 单点冗余加固 + 确定性多视图 | 三路互补语义路径 + ConsensusVoter 投票、multiview_v2 确定性视图生成器 | semantic 路径冗余消除单点故障 |
| **R8** | 公开基准检索攻坚 + 多语言国际化 | P7-D 公开基准复现 + 加法式适配器指标、P7-E i18n 多语言 | 公开基准对标完成、中英双语支持 |
| **R9** | API 服务器生产级硬化 | 中间件链 + Prometheus 文本指标 + 优雅关闭 + pprof + vector embedder 接线、E2E 测试套件（全生命周期+权限隔离+并发安全）、stdlib-only Python/JS SDK + OpenAPI 3.0.3 | API 服务器生产级可用、SDK 双语言交付 |
| **R10** | 生产可靠性闭环 | RankFusion 并发安全加固（消除共享实例 race）、真实 PG 18 + pgvector 端到端验证（持久化重启通过）、Prometheus 格式合规审计 + Golden 快照 | `race` 干净、真实 PG 全链路跑通、DeleteByScope SQL bug 修复 |

### 1.3 当前核心指标（R10 基线 75718de）

| 指标 | 数值 | 口径 | 状态 |
|---|---|---|---|
| 北极星 M1（零 token 覆盖率） | **1.0000** | tfidf 降级口径 | ✅ 合成代理达标 |
| 北极星 M1（vector 口径） | **0.9661** | 合成代理语料 | ✅ ≥0.95 |
| 公开基准 MRR | **0.9917** | vector 口径（780 条） | ✅ 行业领先 |
| fact accuracy v4 | **96.07%** | 660/687 | ✅ |
| 控制面压缩率 | **35.77%** | — | ✅ 低 token 特色落地 |
| 检索性能 P95 | **< 200ms** | 内核链路 | ✅ |
| PG Upsert 延迟 | **15.0 ms/op** | 真实 PG 18.6 | ✅ |
| PG Query 延迟 | **0.53 ms/op** | 真实 PG 18.6 | ✅ |
| PG VectorSearch 延迟 | **0.45 ms/op** | 真实 PG 18.6 + HNSW | ✅ |

---

## 二、当前工作总结

### 2.1 已完成的能力域

| 域 | 完成度 | 说明 |
|---|---|---|
| **检索内核** | 95% | 向量 + 文本双路召回、RRF + 千脑投票融合、温度分层、参考系、多信号（temporal / freshness / proximity） |
| **合规与隐私** | 90% | GDPR 三件套（Export / DeleteByScope / DeleteByID）、租户硬过滤、可见性控制、级联删除 |
| **存储后端** | 85% | SQLite（内存+文件）、PG + pgvector 真实验证通过；后端可插拔接口完整 |
| **API 服务** | 90% | REST API、中间件链（限流/日志/CORS）、Prometheus 指标、优雅关闭、pprof 调试 |
| **SDK** | 85% | Python SDK（stdlib-only）、JS SDK、OpenAPI 3.0.3 文档 |
| **评测体系** | 90% | 北极星 M1/M2 口径、公开基准对标、合成集 KPI、E2E 集成测试、Golden 快照 |
| **多语言** | 80% | kernel/i18n 中英双语框架 |
| **生产可靠性** | 80% | 并发安全（-race 干净）、真实 PG 持久化验证、指标格式合规 |

### 2.2 遗留缺口（Roadmap v4 未完成项）

| # | 缺口 | 泳道 | 优先级 |
|---|---|---|---|
| 1 | 真实语料 holdout 接入（用户说 5 天内到位，截至 R10 尚未落库） | AI 协同与北极星 | P0 |
| 2 | 北极星 ≥0.95 **正式判定**（真实语料口径，当前是合成代理） | AI 协同与北极星 | P0 |
| 3 | 首批 8 包 Family 档装配（gitea / git-connector / registry / backup / search / llm-gateway / mesh-sync / webui） | 应用包生态 | P1 |
| 4 | 包记忆抽取 + 拓扑排序真机验证 | 应用包生态 | P1 |
| 5 | ZFS dataset + jail 划分（lims-pg / lims-router jail） | FreeBSD + ZFS 真机 | P1 |
| 6 | pf 规则 + 安全加固 checklist | FreeBSD + ZFS 真机 | P1 |
| 7 | Family 档真机启动验收（不靠 override，走真 PG+pgvector+jail） | FreeBSD + ZFS 真机 | P0 |
| 8 | ZFS 快照 + 容灾 + 备份包 | FreeBSD + ZFS 真机 | P2 |
| 9 | 集合体 Cluster 节点验证（2 台机器 Coordinator / Worker） | FreeBSD + ZFS 真机 | P2 |
| 10 | BGE-m3 向量文件同步到 0.88（复跑 vector 口径北极星 + MRR） | AI 协同与北极星 | P1 |
| 11 | Prometheus 官方 client_golang 迁移（proxy 不可达，待 GOPROXY 配置） | 生产可靠性 | P2 |
| 12 | 控制面压缩进一步优化（当前 35.77%） | AI 协同与北极星 | P2 |

---

## 三、远期目标展望

### 3.1 近期目标（R11–R12，1–2 轮）

**主题：从「功能可用」到「生产可交付」**

1. **北极星正式达标**：真实语料 holdout 落库，M1/M2 口径正式判定，零 token 覆盖率 ≥ 0.95 从合成代理升级到真实语料
2. **应用包生态打通**：首批 8 包装配完成，包记忆抽取 + 拓扑排序真机验证通过，包成为被记忆的一等实体
3. **生产部署硬化**：ZFS+jail+pf+Family 档真机验收通过，不靠 override 启动，全链路走真 PG+pgvector+jail

### 3.2 中期目标（R13–R15，3–5 轮）

**主题：从「单机可用」到「分布式可扩展」**

1. **集合体 Cluster**：2 台机器 Coordinator / Worker 分工，跨节点 pre-filter 与 revocation 广播
2. **ZFS 容灾**：快照 + zfs send 容灾链路可用，备份包一键恢复
3. **控制面压缩迭代**：从 35.77% 进一步压到 50%+，低 token 长期运行主特色落地
4. **更多公开基准对标**：LongMemEval / LoCoMo / BEAM 全量子集适配复跑

### 3.3 远期目标（R16+）

**主题：从「记忆系统」到「Agent 基础设施」**

1. **开放平台**：SDK 多语言扩展（Go / Java / Rust），第三方 Agent 可接入
2. **多模态记忆**：文本之外，图像 / 音频 / 结构化数据的记忆表示
3. **记忆联邦**：跨实例记忆同步与隔离，mesh-sync 包真机验证
4. **行业基准**：在 LongMemEval / LoCoMo 等公开榜单进入 Top 3

---

## 四、R11 并行任务书

> **主题**：北极星正式达标 + 应用包生态打通 + 生产部署硬化
> **基线**：`75718de`（R10 收口态）
> **三路并行、文件域不重叠**
> **验证机**：192.168.0.88（FreeBSD 15.1）

### 红线（三路共同遵守）

1. 只增不改 kernel Schema 语义、Fuse 算法、既有 SQL 语义、北极星判定口径
2. 本机仅编辑 + SSH/传输，编译/测试/运行在 0.88
3. 不调 LLM 生成评测集，固定 seed=42 确定性生成
4. 不触碰 `light-merge/`（light 语言项目）
5. 推送前须用户示意，禁止自行 push

---

### A 路 — 北极星正式达标（真实语料 holdout 接入）

**目标**：把北极星 M1 从「合成代理口径」升级到「真实语料口径」，正式判定 ≥ 0.95。

**背景**：R5–R10 的北极星 M1 都是基于合成代理语料（seed=42 生成器）计算的，用户已确认真实语料 holdout 5 天内到位。R11 A 路负责：语料落库 → M1/M2 口径正式判定 → 不回归验证。

**范围**：

| # | 任务 | 交付物 | 验收标准 |
|---|---|---|---|
| A.1 | 真实语料 holdout 接入 | `eval/holdout/` 目录 + 加载器 | 用户提供的语料文件落库，格式对齐既有评测集 schema |
| A.2 | M1/M2 正式口径判定 | `eval/reports/r11a_polaris_real_*.md` | 在真实语料上跑 M1（零 token 覆盖率）+ M2（控制面 token 占比），输出正式数值 |
| A.3 | BGE-m3 向量文件同步 | `eval/vectors/vectors.npy` + index + meta | 同步到 0.88 工作树，复跑 vector 口径北极星 + 公开 MRR，与 R10 tfidf 口径交叉验证 |
| A.4 | 不回归复跑 | 同上报告 | 合成代理口径 M1=1.0000（tfidf）/ 0.9661（vector）逐位复现，无回归 |

**文件域**：`eval/holdout/`、`eval/vectors/`、`eval/reports/r11a_*`、`kernel/polaris.go`（只增不改）

**命令**：
```bash
cd /tmp/r11a/lightmem
# 跑真实语料北极星
go run ./eval polaris --holdout ./eval/holdout/ --now-fixed
# 复跑合成代理口径
go run ./eval polaris --now-fixed
```

---

### B 路 — 应用包生态真机验证

**目标**：首批 8 包 Family 档装配完成，包记忆抽取 + 拓扑排序真机验证通过。

**背景**：Roadmap v4 §3.4 规划了应用包生态：包成为被记忆的一等实体（装包 / 配包 / 用包 / 沉淀包经验四环节）。R11 B 路负责：8 包装配链路打通 + ExtractPackageMemory + InstallOrder 拓扑排序。

**范围**：

| # | 任务 | 交付物 | 验收标准 |
|---|---|---|---|
| B.1 | 首批 8 包 Family 档装配 | `eval/packages/family_profile.yaml` + 安装脚本 | gitea / git-connector / registry / backup / search / llm-gateway / mesh-sync / webui 八包，安装 → 配置 → 升级 → 卸载全链路跑通 |
| B.2 | 包记忆抽取 | `kernel/package_memory.go`（新增） | `ExtractPackageMemory`：从包的元数据 / 配置 / 变更记录中抽取结构化记忆，写入记忆库 |
| B.3 | 拓扑排序（安装顺序） | `kernel/package_order.go`（新增） | `InstallOrder`：根据包依赖图输出拓扑排序，循环依赖检测 |
| B.4 | 包记忆召回验证 | `eval/e2e/packages/package_e2e_test.go`（新增） | 装包后查询「这个包是干什么的」→ 命中抽取的包记忆；错误安装顺序 → 拓扑排序报错提示 |

**文件域**：`eval/packages/`、`kernel/package_memory.go`、`kernel/package_order.go`、`eval/e2e/packages/`、`eval/reports/r11b_*`

**命令**：
```bash
cd /tmp/r11b/lightmem
# 装配 8 包
bash eval/packages/install_family.sh
# 跑包记忆 E2E
go test ./eval/e2e/packages/ -v -count=1
```

---

### C 路 — 生产部署硬化（ZFS + jail + pf + Family 档真机验收）

**目标**：ZFS+jail+pf 真机部署完成，Family 档不靠 override 启动，全链路走真 PG+pgvector+jail。

**背景**：Roadmap v4 §3.2 的 FreeBSD + ZFS 真机部署泳道，截至 R10 尚未启动。R11 C 路负责：ZFS dataset 划分 → jail 隔离 → pf 加固 → Family 档真机验收。

**范围**：

| # | 任务 | 交付物 | 验收标准 |
|---|---|---|---|
| C.1 | ZFS dataset + jail 划分 | `manuals/p1-freebsd-zfs-setup.md`（执行记录） | `zfs list` 列出 lims-pg / lims-router 两个 dataset；`jail -l` 列出 lims-pg / lims-router 两个 jail |
| C.2 | pf 规则 + 安全加固 | `manuals/pf.rules` + `checklists/freebsd-security-checklist.md` | `pfctl -sr` 仅暴露 443 / 8443；checklist 全部打勾 |
| C.3 | Family 档真机启动验收 | `eval/reports/r11c_family_real_*.md` | 不靠 override 启动 Family 档，`/memories`、`/search` 走真 PG+pgvector+jail，端到端查询通过 |
| C.4 | 不回归验证 | 同上报告 | Family 档真机下，北极星 M1（tfidf）= 1.0000 逐位复现，检索延迟 P95 < 200ms |

**文件域**：`manuals/`、`checklists/`、`eval/reports/r11c_*`、部署脚本（`eval/scripts/r11c_*.sh`）

**命令**：
```bash
# 在 0.88 上以 root 执行
zfs create zroot/lims-pg
zfs create zroot/lims-router
# 启动 jail
jail -c lims-pg
jail -c lims-router
# 加载 pf 规则
pfctl -f /etc/pf.rules
# Family 档真机启动
cd /tmp/r11c/lightmem && go run ./cmd/lightmem-kernel --profile family-real.yaml
```

---

### 三路冲突声明

| 路 | 主要文件域 | 交叉点 |
|---|---|---|
| A | `eval/holdout/`、`eval/vectors/`、`kernel/polaris.go` | 无 |
| B | `kernel/package_memory.go`、`kernel/package_order.go`、`eval/packages/`、`eval/e2e/packages/` | 无 |
| C | `manuals/`、`checklists/`、部署脚本、jail 配置 | 无 |

**三路文件域完全不重叠**，可并行开发。

---

### R11 验收门禁（合流时）

| 门禁项 | 目标 |
|---|---|
| gofmt -l | 空 |
| go vet ./... | 无 warning |
| go test ./kernel/... | 全绿 |
| go test -race ./kernel/... | 无 DATA RACE |
| 北极星 M1（合成代理 tfidf） | 1.0000 逐位复现 |
| 公开 MRR（vector） | 0.9917 逐位复现 |
| 三路交付报告齐全 | A/B/C 各一份 |
| 只增不改 Schema / 融合算法 / 既有 SQL / 对外格式 | ✅ |

---

## 五、下一步

本任务书由用户审阅后确认。确认后：
1. 在 0.88 上分别 clone 三个工作树（`/tmp/r11a/`、`/tmp/r11b/`、`/tmp/r11c/`）
2. 三路并行开发，各自完成后 commit（不 push）
3. 三路全部完成后，写合流验收报告
4. 用户示意后 push gitea main

> **你写我发**：本文档为草案，由用户审阅、修改、拍板后正式启动 R11。
