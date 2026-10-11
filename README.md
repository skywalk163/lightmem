# 光明记忆（LIMS）v2.3

[![Go Version](https://img.shields.io/badge/Go-1.25-blue)](https://golang.org/)
[![License](https://img.shields.io/badge/License-Apache%202.0-green)](LICENSE)

> **让 AI Agent 像人一样拥有长期记忆**
> 多租户 · 多用户 · 多Agent · 可插拔后端 · 四层温度分层

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

# 写入记忆
curl -X POST http://localhost:8080/memories \
  -H "Content-Type: application/json" \
  -d '{"memory_id": "m001", "fact": "客户A偏好周一开会", "tenant_id": "t1", "user_id": "u1"}'

# 检索记忆
curl "http://localhost:8080/search?text=客户A偏好"
```

---

## 📖 文档

| 文档 | 路径 | 说明 |
|---|---|---|
| 架构设计文档 | `lightmem.md` | 完整架构设计（v2.3） |
| 收口报告 | `.dumate/inbox/_R138_B线收口报告.md` | R138-B 任务交付报告 v4 |
| 极简开发计划 | `.dumate/inbox/_R138_B线极简开发计划_4小时版.md` | 4 小时权重校准计划 |
| 宣传文档 | `docs/lightmem_promo_v2.3.md` | 产品宣传文档 |
| 评测集说明 | `eval/README.md` | 评测集生成与使用说明 |

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

- ✅ 投毒防护（`provenance=prompt_injection_suspect`）
- ✅ 记忆污染隔离（confidence 机制）
- ✅ 隐私保护（租户/用户/项目/可见范围硬过滤）
- ✅ 级联删除（可审计）

---

## 📊 当前成果（R138-B，2026-10-11）

### KPI 基线

| 指标 | 目标 | 实际 | 状态 |
|---|---|---|---|
| 检索准确率（总体） | > 90% | **93.0%** | ✅ |
| single-hop | 分桶 | 80.0% | ✅ |
| multi-hop | 分桶 | **100%** | ✅ |
| open-domain | 分桶 | **96%** | ✅ |
| 时序桶 | > 85% | **97.7%** | ✅ |
| 多信号融合 | > 85% | **100%** | ✅ |
| 污染率 | 持续下降 | **0%** | ✅ |

> 验证环境：192.168.0.88，SQLite 骨架后端
> 可复现性：3 次连跑一致 93.0%

### 4 小时权重校准

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
│   ├── backend_pg.go          # PostgreSQL 后端（Family/SMB/Enterprise）
│   ├── router_mem.go          # MemoryRouter 意图识别 + 跨层召回
│   ├── rank_fusion.go         # 多信号 rank 融合引擎
│   ├── writepolicy.go         # WritePolicy 插件注册表
│   ├── lifecycle.go           # 生命周期扫描器
│   ├── consolidate.go         # 后台整合 worker
│   ├── router_tools.go        # Agent 记忆工具接口
│   ├── model.go               # 统一 Schema
│   ├── profile.go             # Profile 加载与校验
│   └── storage.go             # StorageBackend 抽象
├── eval/                      # 评测集与 KPI
│   ├── eval_generate.go       # 评测集生成器
│   ├── run_kpi.go             # KPI 基线评测器
│   ├── dataset/               # 评测集数据（570 条）
│   └── reports/               # KPI 报告
├── profiles/                  # 四档 Profile 配置
│   ├── lims-solo.profile.yaml
│   ├── lims-family.profile.yaml
│   ├── lims-smb.profile.yaml
│   └── lims-enterprise.profile.yaml
├── scripts/                   # 脚本
│   ├── a9_verify.sh           # A9 验证脚本
│   └── p0_verify.sh           # P0 修复验证脚本
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

```json
{
  "semantic": 1.5,
  "keyword": 1.5,
  "entity": 1.2,
  "value": 0.2,
  "freshness": 0.1,
  "source_trust": 0.1,
  "reference_frame": 1.2,
  "feedback": 0.1
}
```

---

## 🧪 评测集

| 分桶 | 条数 | 说明 |
|---|---|---|
| single-hop | 150 | 单条记忆即可回答 |
| multi-hop | 150 | 需 2+ 条记忆链式推理 |
| open-domain | 100 | 无明确 schema，需开放推理 |
| temporal | 130 | 双时态变更，需 as_of 回溯 |
| pollution | 20 | 低置信度污染样本 |
| injection | 20 | 指令性投毒样本 |
| **合计** | **570** | 非对抗 530 + 对抗 40 |

---

## 📝 开发计划

| 阶段 | 内容 | 状态 |
|---|---|---|
| P1 核心骨架 | 统一 Schema、ReferenceFrame、MemoryExtractor | ✅ |
| P2 智能路由与融合 | MemoryRouter、WritePolicy、多信号检索 | ✅ |
| P2.5 评测集建设 | 四类分桶评测集、KPI 基线 | ✅ |
| P3 融合反馈与评测 | VoteFusion、FeedbackLoop | 🔜 |
| P4 生命周期 | BudgetManager、Forgetting/Archive | 🔜 |
| P5 外部连接器 | 数据库/文档/应用连接器 | 🔜 |
| P6 目标与共享 | GoalMemory、权限三元组 | 🔜 |
| P7 合规与优化 | GDPR、公开基准对标 | 🔜 |
| P8 集合体与多后端 | Qdrant、节点角色、跨节点查询 | 🔜 |
| P9 应用包生态 | manifest.yaml、包生命周期 | 🔜 |

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
- 权重不写死为"调优好"（占位权重标注"待 A8 校准"）
- 只增不改 Schema 语义

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
