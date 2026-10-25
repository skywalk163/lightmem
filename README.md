# 光明记忆（LIMS）v2.4.1（R16 收口）

[![Go Version](https://img.shields.io/badge/Go-1.25-blue)](https://golang.org/)
[![License](https://img.shields.io/badge/License-Apache%202.0-green)](LICENSE)

> **让 AI Agent 像人一样拥有长期记忆**
> 多租户 · 多用户 · 多Agent · 可插拔后端 · 四层温度分层

---

## ⭐ 新用户从这开始：5 分钟让所有 AI 共用一个大脑

你的 DeepSeek Harness / Trae / CodeArts / WorkBuddy / 小浣熊……各自失忆。
lightmem 让它们**连到同一个记忆服务**，一个 AI 沉淀的经验，另一个直接复用。

```bash
sh integrations/quickstart/install.sh      # 一条命令：起服务 + 签 token + 生成接入包
```

然后把生成的 `_lightmem_run/clients/agent-prompt.md` 粘给你的 AI，它会自己装好。

- **人看**：[`GETTING_STARTED.md`](GETTING_STARTED.md)（5 分钟上手 + 宣传 + 排障）
- **AI 看**：[`AGENTS.md`](AGENTS.md)（工具清单 + 接入事实 + 开发红线）

> 个人档默认 **SQLite，零外部依赖**（不用装 PostgreSQL / Docker）。
>
> ⚠️ **能力边界（R25-D 如实化）**：
> - ✅ 零外部依赖：Solo 档仅需 SQLite 文件，无需 PG/Docker/Qdrant。
> - ✅ 向量检索：R25-B 后纯 Go 余弦可用（BLOB 存储），仍零依赖。
> - ❌ 无 jail / MAC / IPsec / KMS：Solo 档安全组件为极简形态。
> - ❌ 无多租户 / 跨 Agent 共享：Solo 档为单用户形态。
> - 完整档位能力矩阵见 [`docs/档位能力矩阵.md`](docs/档位能力矩阵.md)。
> - **哪些由本套件负责、哪些由部署方负责**（守护、备份、主备、负载均衡）见 [`docs/核心与外围边界_v1.0.md`](docs/核心与外围边界_v1.0.md)。

---

## 🚀 快速开始

### 环境要求

- Go 1.25+
- 验证机：192.168.0.88（可选）

### 安装

```bash
git clone http://192.168.1.5:3000/skywalk/lightmem.git
cd lightmem
```

### 运行测试

```bash
# 编译内核
go build github.com/lightmem/kernel

# 运行测试
go test github.com/lightmem/kernel/... -v

# 生成评测集
cd eval && go run ./eval_generate.go

# 运行 KPI 评测
go run ./run_kpi.go
```

### API 端点

```bash
# 健康检查
curl http://localhost:8080/health

# 写入记忆（visibility 与 permission.owner 都**必填**，见下方「写入必填字段」）
curl -X POST http://localhost:8080/memories \
  -H "Content-Type: application/json" \
  -d '{"memory_id": "m001", "fact": "客户A偏好周一开会", "tenant_id": "t1", "user_id": "u1", "visibility": "private", "permission": {"owner": "u1"}}'

# 检索记忆（文本参数用 q；text 为历史别名，q 优先）
curl "http://localhost:8080/search?q=客户A偏好&tenant_id=t1&viewer=u1"
```

#### 写入必填字段（R27+ 写入策略，2026-10-26 订正）

| 字段 | 缺失后果 |
|---|---|
| `permission.owner` | **HTTP 422** `{"error":"writepolicy permission: permission.owner required"}`（`kernel/writepolicy.go` 的 `PermissionPolicy`） |
| `visibility` | **201 但检索不到**：`Memory.canView` 对零值 `Visibility` **没有任何匹配分支**，直接 `return false` ⇒ 记忆写进去了、`/search` 永远返回 `[]`。必须显式给 `private` / `tenant` / `public` 之一 |

> 这两条都是「写错不报错、只是不生效」类坑：`permission.owner` 至少会 422 拦你，
> `visibility` 是静默不可见。R165-T5 真机验收就踩过（201 成功 + search 恒空）。

#### `/search` 参数口径（R19-A 统一）

| 参数 | 必填 | 说明 |
|---|---|---|
| `q` | ✅ | 查询文本（**权威参数**，四个消费者统一用它） |
| `text` | ❌ | `q` 的历史别名；仅当 `q` 为空时生效，保留以兼容旧调用方 |
| `tenant_id` | ✅ | 租户 ID；缺失返回 `400` |
| `viewer` | ❌ | 可见性过滤的观察者 ID |
| `top_k` | ❌ | 返回条数，默认 10 |
| `kind` | ❌ | 记忆类型过滤（逗号分隔，如 `fact,preference`） |

- `q` 与 `text` 同时出现时 **`q` 优先**，两个参数名都不得移除。
- **空查询返回 `400 {"error":"q or text required"}`**：后端对空文本不做过滤（等于列举作用域全部），
  故不提供「空查询 = 全量列表」的隐式行为；要导出请走 `POST /api/v1/gdpr/export`。
- 无命中返回空数组 `[]`（不是 `null`）。
- 消费者一致性：`sdk/python/lightmem_sdk.py`、`sdk/js/lightmem-sdk.js`、
  `integrations/dsh-mcp-bridge/lightmem_mcp_bridge.py` 均发送 `q`（桥同时附带 `text`）。

---

## 📖 文档

| 文档 | 路径 | 说明 |
|---|---|---|
| **接入手册（新用户首选）** | `GETTING_STARTED.md` | 5 分钟上手：一键部署 → 接 AI → 使用与排障（含宣传） |
| **AI Agent 入口** | `AGENTS.md` | 给 AI 看：13 个工具清单、各家配置事实、开发红线 |
| **一键部署** | `integrations/quickstart/install.sh` | 起服务 + 签 token + 生成接入包（零外部依赖） |
| **token 签发** | `integrations/agent-onboarding/issue_token.py` | 为每个 AI 签发独立 token（防冒充） |
| **配置生成** | `integrations/agent-onboarding/gen_configs.py` | 生成 8 家可直接粘贴的 MCP 配置片段 |
| 架构设计文档 | `lightmem.md` | 完整架构设计（v2.4.1，含 v2.4 Monty 建议与 v2.4.1 收口） |
| **档位能力矩阵** | `docs/档位能力矩阵.md` | 四档能力边界如实标注（Solo/Family/SMB/Enterprise），每格附 file:line 或实测命令支撑 |
| 收口报告 | `.dumate/inbox/_R138_B线收口报告.md` | R138-B 任务交付报告 v5 |
| 极简开发计划 | `.dumate/inbox/_R138_B线极简开发计划_4小时版.md` | 4 小时权重校准计划 |
| 宣传文档 | `docs/lightmem_promo_v2.3.md` | 产品宣传文档 |
| 评测集说明 | `eval/README.md` | 评测集生成与使用说明 |
| 门禁脚本 | `scripts/gate.sh` | R16 统一门禁（版本/gofmt/vet/build/test/race/覆盖/红线白名单/PG 真机；[6] 已纳入 R16 S1–S3 骨架文件） |
| 最新收口报告 | `.dumate/inbox/_R16_收口报告.md` | R16 收口（门禁 PASS 12/0/1，北极星三语料补评，ZFS 容灾） |
| 运行手册 | `manuals/p1-freebsd-zfs-setup.md` | FreeBSD + ZFS + jail + pf + PG 18 迁入 + ZFS 快照容灾 |

---

## 🧠 核心特性

### 四层温度分层

| 层级 | 名称 | 类比 | 生命周期 |
|---|---|---|---|
| L0 | 工作记忆 | 短期工作记忆 | 分钟~小时 |
| L1 | 动态记忆 | 情景记忆 | 天~月 |
| L2 | 静态记忆 | 语义记忆 | 长期版本化 |
| L3 | 归档记忆 | 深层档案 | 合规保留 |

### 多信号融合

8 路信号独立打分 + 名次融合：

- 语义相似度
- BM25 全文关键词
- 实体匹配
- 固有价值
- 时效新鲜度
- 来源可信度
- 参考系匹配度
- 反馈历史

### 安全合规

- ✅ 投毒防护（`provenance=prompt_injection_suspect`，`kernel/poison_detect.go`，R15 可评测化）
- ✅ 记忆污染隔离（confidence 机制 + 合并度激活，R15 合流）
- ✅ 隐私保护（租户/用户/项目/可见范围硬过滤 + 权限三元组）
- ✅ 级联删除（可审计，`kernel/gdpr.go` GDPR 三件套）

---

## 📊 当前成果（R16 收口，2026-10-16）

### R16 新增交付（S1–S8）

| 泳道 | 交付物 | 状态 |
|---|---|---|
| **S1 Qdrant 后端** | `kernel/backend_qdrant.go`（20 方法，stdlib-only HTTP）+ `profile.go` 接线 | ✅ 骨架交付（未配置明确 error，不静默降级） |
| **S2 节点角色与拓扑** | `kernel/node_role.go` + `kernel/topology.go`（四角色 + BFS + 撤销 TTL） | ✅ 骨架交付（默认不进主线） |
| **S3 P5 应用连接器** | `kernel/connector_app.go`（git 元数据扫描 + 幂等） | ✅ 骨架交付 |
| **S4 门禁与口径** | `scripts/gate.sh` [1b] 硬翻转 + `corpus_note` 动态化 | ✅ 已交付 |
| **S5 FreeBSD jail vnet** | `/etc/jail.conf` 修复（0.88 实机）+ 手册 §11A | ✅ 已交付（host↔jail 0% loss） |
| **S6 北极星 holdout_v3 补评** | M1=1.0000（840/840）、M2=0.2214、数据面 −7.13% | ✅ 已交付 |
| **S7 ZFS 快照容灾** | `scripts/zfs_backup.sh`（298 行）+ 手册 §10 | ✅ 已交付（0.88 全流程 ALL_DONE） |
| **S8 报告拆分** | 3 份北极星报告各拆为 summary（入库）+ detail.jsonl（不入库） | ✅ 已交付 |

### 北极星评测（真实语料 holdout）

| 指标 | 目标 | 实际 | 状态 |
|---|---|---|---|
| **M1 零 token 覆盖率** | ≥ 0.95 | **1.0000**（800/800，holdout_v2） | ✅ 达标 |
| M1 hit@1 | — | **1.0000**（800/800） | ✅ |
| M2 控制面 token 占比 | 低 token | **0.2498**（43126/172663） | ✅ |
| 数据面压缩率 | — | **−0.353** | ✅ |

> 语料：`eval/holdout/holdout_v2.jsonl`（800 条，LoCoMo-v2 + beam 混合）｜ runner：`eval/polaris/`
> 报告：`eval/reports/r16_polaris_holdout_v2_corpusnote_20261016-015834_summary.json`

### R16 门禁矩阵（0.88 实机，基线 R15 收口 `2b75731`）

| 门禁 | 结果 |
|---|---|
| [0]–[5] go 版本 / gofmt / vet / build / test / race | PASS |
| [6] 新增文件覆盖（含 R16 S1–S3 骨架文件） | PASS |
| [7]–[10] 红线抽查 / 投毒检测 / 合并度激活 / 接线默认关 | PASS |
| [11] PG 真机往返 | SKIP（`LIGHTMEM_PG_HOST` 未配置，非假 PASS）｜**R26-A 起（2026-10-19）**：0.88 已固化 env（`/var/db/lightmem/gate.env`，不入 git），真机往返 13 项全 PASS，收口态 **14/0/0**（首次 SKIP=0）；未配 env 时仍显式 SKIP，可移植性不破 |
| **合计** | **PASS 12 / FAIL 0 / SKIP 1** |

> 验证环境：192.168.0.88（FreeBSD 15.1，go1.25.14，PG 18.6 + pgvector 0.8.2）
> 门禁复现命令：`sh scripts/gate.sh`

### R15 合流门禁

| 门禁 | 结果 |
|---|---|
| gofmt / vet / build / test -count=1 / -race | PASS 12 / FAIL 0 / SKIP 0 |
| 红线白名单（只增不改） | PASS（12 文件逐文件 diff 一致） |
| 北极星 R14→R15 回归 | PASS（832178 bytes 逐字节一致） |

> 验证环境：192.168.0.88（FreeBSD 15.1，go1.25.14，PG 18.6 + pgvector 0.8.2）
> 门禁复现命令：`sh scripts/gate.sh`

### KPI 基线（R138-B，SQLite 骨架口径）

| 指标 | 目标 | 实际 | 状态 |
|---|---|---|---|
| 检索准确率（总体） | > 90% | **93.0%** | ✅ |
| single-hop | 分桶 | 80.0% | ✅ |
| multi-hop | 分桶 | **100%** | ✅ |
| open-domain | 分桶 | **96%** | ✅ |
| 时序桶 | > 85% | **97.7%** | ✅ |
| 多信号融合 | > 85% | **100%** | ✅ |
| 污染率 | 持续下降 | **0%** | ✅ |

> 可复现性：3 次连跑一致 93.0%

### 4 小时权重校准（历史）

| 轮次 | 总体 | 变化 |
|---|---|---|
| R1 基线 | 6.4% | — |
| R2 相关优先 | 18.7% | +192% |
| R3 关键词强化 | 26.0% | +306% |
| R3 + multi-hop | 69.1% | +979% |
| **R3 + temporal** | **93.0%** | **+1353%** |

---

## 🏗️ 项目结构

```
lightmem/
├── kernel/                    # 核心内核
│   ├── backend_sqlite.go      # SQLite 存储后端（Solo 档）
│   ├── backend_pg.go          # PostgreSQL + pgvector 后端（Family/SMB/Enterprise）
│   ├── backend_qdrant.go      # Qdrant REST 后端（Enterprise，R16 S1，stdlib-only）
│   ├── node_role.go           # 节点角色（Coordinator/Worker/Archive/Gateway，R16 S2）
│   ├── topology.go            # 拓扑 BFS + 撤销 TTL 存储（R16 S2）
│   ├── connector_app.go       # P5 应用连接器（git 元数据扫描，R16 S3）
│   ├── router_mem.go          # MemoryRouter 意图识别 + 跨层召回 + RetrievalTrace
│   ├── rank_fusion.go         # 多信号 rank 融合引擎（8 路信号 + VoteFusion + ConflictDetector）
│   ├── writepolicy.go         # WritePolicy 插件注册表
│   ├── lifecycle.go           # BudgetManager / ForgettingEngine / ArchiveEngine / CoreMemoryManager
│   ├── consolidate.go         # 后台整合 worker
│   ├── flush.go               # L0→L1 落库边界 FlushPlan
│   ├── forgetting_drift.go    # 参考系漂移衰减（旁路算子）
│   ├── displacement_edge.go   # 记忆位移边（千脑）
│   ├── edge_persist.go        # 位移边 JSON 落库（原子替换）
│   ├── entity_align.go        # 跨 Agent 参考系对齐
│   ├── poison_detect.go       # 投毒防护
│   ├── gdpr.go                # GDPR 三件套（级联删除/导出/被遗忘权）
│   ├── perf.go                # 检索 P95 与热点缓存
│   ├── goal.go                # GoalMemory + 权限三元组 + 撤销
│   ├── connector.go           # DB/Doc 外部连接器
│   ├── package.go             # 应用包生命周期（8 包）
│   ├── package_memory.go      # 包记忆抽取
│   ├── package_order.go       # 安装拓扑排序
│   ├── router_tools.go        # Agent 记忆工具接口（含 FeedbackLoop）
│   ├── model.go               # 统一 Schema
│   ├── profile.go             # Profile 加载与校验（R16 S1：Qdrant 后端已接线）
│   └── storage.go             # StorageBackend 抽象
├── eval/                      # 评测集与 KPI
│   ├── eval_generate.go       # 评测集生成器
│   ├── run_kpi.go             # KPI 基线评测器
│   ├── polaris/               # 北极星评测 runner（21 文件）
│   ├── benchmarks/            # 公开基准适配（LongMemEval/LoCoMo/BEAM）
│   ├── dataset/               # 评测集数据（12 jsonl，all=570 条）
│   ├── holdout/               # 真实语料 holdout（v1 450 / v2 800 / v3 840）
│   └── reports/               # 评测报告（147 份，含 R15 合流）
├── sdk/                       # SDK（Python + JS，stdlib-only）
├── profiles/                  # 四档 Profile 配置 + JSON Schema
├── checklists/                # FreeBSD 安全加固清单
├── manuals/                   # FreeBSD+ZFS 部署手册 + pf 规则
├── scripts/                   # gate.sh 统一门禁 + 验证脚本
├── docs/                      # 文档
│   └── lightmem_promo_v2.3.md # 宣传文档
├── .dumate/                   # 工作文件
│   └── inbox/                 # 任务书、收口报告、开发计划
├── lightmem.md                # 架构设计文档（主文档）
├── go.mod
└── README.md
```

---

## 🔧 配置

### Profile 配置

```yaml
# profiles/lims-solo.profile.yaml
profile: solo
storage_backend: sqlite
```

| 档位 | 存储后端 | 适用场景 |
|---|---|---|
| Solo | SQLite | 个人、开发测试 |
| Family | PostgreSQL + pgvector | 家庭、小团队 |
| SMB | PostgreSQL + pgvector + Redis | 中小型企业 |
| Enterprise | PostgreSQL + pgvector + Qdrant + Redis + MQ | 企业集合体 |

### 融合权重

> 说明：README 此前的内联 JSON 是占位示意值。**权威权重以 `eval/weights/` 下的校准文件为准**（R6-A 已激活 reference_frame 信号）。

```json
{
  "semantic": 1.0,
  "keyword": 0.8,
  "entity": 0.7,
  "value": 0.6,
  "freshness": 0.5,
  "source_trust": 0.4,
  "reference_frame": 0.6,
  "feedback": 0.5
}
```

> 当前校准文件：`eval/weights/rank_weights_r6_ref.json`（ref 桶 M1 0.667→1.000，其余四桶零回归）

---

## 🧪 评测集

| 分桶 | 条数 | 说明 |
|---|---|---|
| single-hop | 150 | 单条记忆即可回答 |
| multi-hop | 150 | 需 2+ 条记忆链式推理 |
| open-domain | 100 | 无明确 schema，需开放推理 |
| temporal | 130 | 双时态变更，需 as_of 回溯 |
| reference_frame | 60 | 参考系变化（R4-B 新增） |
| pollution | 20 | 低置信度污染样本 |
| injection | 20 | 指令性投毒样本 |
| injection_v2 | 342 | R15 对抗样本扩容 |
| benign_near | 300 | 语义近邻 |
| **合计** | **1272** | 主评测集 `eval/dataset/all.jsonl`=570 条（非对抗 530 + 对抗 40） |

> 另有 holdout 语料：`eval/holdout/`（v1 450 / v2 800 / v3 840 / locomo_v2 200）
> 公开基准：`eval/benchmarks/dataset/`（LongMemEval 500 / beam 100 / locomo 50，合计 650）

---

## 📝 开发计划

| 阶段 | 内容 | 状态 |
|---|---|---|
| P1 核心骨架 | 统一 Schema、ReferenceFrame、四层存储 | ✅ 已交付 |
| P2 智能路由与融合 | MemoryRouter、WritePolicy、8 路多信号检索 | ✅ 已交付 |
| P2.5 评测集建设 | 四类分桶评测集、KPI 基线 | ✅ 已交付 |
| P3 融合反馈与评测 | VoteFusion、FeedbackLoop、retrieval_trace、冲突检测 | ✅ 已交付 |
| P4 生命周期 | BudgetManager、ForgettingEngine、ArchiveEngine、CoreMemory、后台整合、漂移衰减、Flush | ✅ 已交付 |
| P5 外部连接器 | DB/Doc 连接器 + App 连接器（`kernel/connector_app.go`，R16 S3 骨架） | ✅ 已交付（App 连接器骨架） |
| P6 目标与共享 | GoalMemory、权限三元组、单机撤销（跨节点撤销传播已交付 `topology.go` + `node_role.go`，默认不进主线） | ✅ 已交付（单机 + 骨架） |
| P7 合规与优化 | GDPR、P95 性能、SDK、公开基准对标、i18n、OpenAPI | ✅ 已交付 |
| P8 集合体与多后端 | Qdrant 真机后端（`backend_qdrant.go`，R17 真机联调 4/4 PASS）、节点角色（`node_role.go`）、拓扑与撤销广播（`topology.go`）、跨节点 gRPC 真实现（`node_rpc.go` + `proto/`，默认关，本地回环通过） | ✅ 已交付（R16 骨架 + R17 真机落地；PG 18.6 迁入 lims-pg jail） |
| P9 应用包生态 | 8 包生命周期、拓扑安装、包记忆抽取、e2e 测试 | ✅ 已交付 |

> 北极星里程碑（roadmap 泳道三）：**已达标**，M1 零 token 覆盖率 = **1.0000**（800/800，holdout_v2 真实语料）；R16 holdout_v3 补评同样 **M1=1.0000**（840/840）

---

## 🤝 贡献

1. Fork 本仓库
2. 创建特性分支 (`git checkout -b feature/my-feature`)
3. 提交改动 (`git commit -m 'Add my feature'`)
4. 推送到分支 (`git push origin feature/my-feature`)
5. 创建 Pull Request

### 开发规范

- 所有编译/测试/运行在 0.88 执行，本机仅编辑 + SFTP
- 不调 LLM 生成评测集（模板 + 固定 seed）
- 权重不写死为"调优好"（以 `eval/weights/` 校准文件为准，不再内联写死在文档）
- 只增不改 Schema 语义（R14/R15/R16 已建立"只增不改白名单"与统一门禁 `scripts/gate.sh`）
- 运行平台优先级：**FreeBSD > Linux > Windows**（用户拍板）；Windows 仅文档编辑与传输，不装 Go
- 门禁 [11] PG 真机往返：未配置 → 显式 SKIP（绝不静默当 PASS，R15 口径）
- R16 门禁 [6] 新增文件覆盖清单含 `backend_qdrant:4` / `node_role:4` / `topology:3` / `connector_app:4`（骨架契约单测阈值，向后兼容默认 12）

---

## 📤 三远端推送（R26-C）

> 三远端（origin/gitea、gitcode、github）推送走 `scripts/push_all_remotes.sh` 命令序列：脚本封装三远端推送、打印 `git ls-remote` 复核结果，并在 gitcode token 失效 / github git 协议被代理拦截时给出明确排错指引。按红线⑧脚本不自动 push，执行前需业主示意。

---

## 📄 许可证

Apache License 2.0 - 详见 [LICENSE](LICENSE) 文件

---

## 👥 团队

| 角色 | 人数 | 职责 |
|---|---|---|
| 架构师 | 1 | 整体架构、技术选型 |
| 后端开发 | 2~3 | 核心模块、API、一致性、权限与撤销 |
| AI/算法工程师 | 1~2 | 记忆提取、embedding、实体链接、rank 融合权重调优 |
| 前端开发 | 1 | 管理后台、trace 调试工具 |
| 测试工程师 | 1 | 功能/性能/数据质量、评测集分桶维护 |
| 产品经理 | 1 | 需求、场景、合规 |

---

## 📮 联系方式

- 仓库：[http://192.168.1.5:3000/skywalk/lightmem](http://192.168.1.5:3000/skywalk/lightmem)
- 验证机：192.168.0.88

---

> **光明记忆 — 让 AI Agent 拥有"记住"的能力，让知识资产沉淀为组织记忆。**
