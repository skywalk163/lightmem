# LIMS v1.4 光明记忆系统架构设计文档

**项目名称：** Light Intelligent Memory System（LIMS / lightmem，光明记忆系统，简称"光明记忆"）
**版本：** v1.4
**文档类型：** 架构设计文档

---

## 1. 项目概述

### 1.1 项目定位

光明记忆（LIMS）是一个面向多租户、多用户、多 Agent 的智能记忆管理平台。它让 AI Agent 像人一样拥有长期记忆、短期记忆和程序性记忆，并在不同场景下自动选择最合适的记忆层级进行存取。与"单 Agent 自带的上下文"不同，光明记忆是一个**可被多个 Agent 共享调用的独立记忆服务**：一个 Agent 沉淀下来的经验，可以被同一个用户名下的其他 Agent 直接复用。

### 1.2 核心价值

- **对 AI Agent：** 拥有"记住"的能力，不再每次对话都从零开始；新任务开始时能自动召回相关的历史目标、偏好和经验。
- **对企业：** 沉淀客户、项目、流程等知识资产，形成可复用的组织记忆，且在租户间隔离可控。
- **对用户：** 获得个性化、连续的服务体验；用户对自己的记忆资产有完整的查看、导出、删除入口（数据主权）。

### 1.3 设计哲学

光明记忆融合《千脑智能》的参考系思想、人脑记忆分层模型，以及 Sparse Agent 等长期数字员工实践：

- **参考系组织：** 每条记忆都有坐标（租户、用户、Agent、项目、实体、场景、任务、时间），像大脑用空间坐标组织知识。
- **记忆内容分类：** 不只存"事实"，还区分事件、偏好、程序性经验、长期目标——不同内容的生命周期和召回方式不同。
- **多记忆加权融合：** 冲突记忆不直接覆盖，而是通过固有价值、时效、来源、反馈等多维加权，而非"多个大模型互相投票"。
- **持续学习闭环：** 记忆随交互反馈不断修正；用户纠正后，下一轮工作立即生效。
- **分布式容错：** 同一事实多版本保存，单点损坏不影响整体。
- **渐进式记忆：** 记忆从会话事件流中持续捕获、压缩、召回，按固有价值逐步升级。
- **记忆预算驱动：** 不无限膨胀，有容量上限，低价值记忆自动淡化、压缩、归档。
- **线索优先：** LIMS 内部只存线索和摘要，详细内容交由外部系统（数据库、文档系统、档案系统）保管。
- **稀疏触发：** 记忆检索和晋升都是按需、稀疏触发的——不需要时不加载、不算分，控制成本。

---

## 2. 系统架构

### 2.1 总体架构

```text
应用层（Agents / Apps）
  客服 Agent | 销售 Agent | 项目 Agent | 个人助手 Agent | 数字员工 Agent
        ↓
记忆路由层（MemoryRouter）
  意图识别 → 记忆类型判断 → 层级选择 → 可见范围过滤 → 读写分发
        ↓
内部记忆层（按温度分四层）
  L0 工作记忆 | L1 动态记忆 | L2 静态记忆 | L3 归档记忆
        ↓
外部记忆连接器层
  数据库 | 文档/知识库 | 对象存储 | 应用/API | 网页/搜索
        ↓
基础设施层
  关系数据库(PG + pgvector) | 图数据库 | 缓存(Redis) | 消息队列 | 日志库
```

### 2.2 四层记忆模型（按温度/时间分）

| 层级 | 名称 | 类比 | 存储内容 | 生命周期 | 典型场景 |
|---|---|---|---|---|---|
| L0 | 工作记忆 | 人类短期工作记忆 | 当前对话上下文、临时变量、工具调用中间结果、外部系统查询线索 | 分钟~小时 | 多轮对话、任务执行链 |
| L1 | 动态记忆 | 人类情景记忆 | 用户偏好、近期事件、临时事实、会话摘要、外部系统结论、跨 Agent 共享经验 | 天~月 | 记住用户上次说过的需求 |
| L2 | 静态记忆 | 人类语义记忆 | 企业知识、产品文档、标准流程、客户档案、长期目标/KPI/约束、外部系统元数据索引 | 长期版本化，数年~数十年 | 企业知识问答、标准流程、数字员工长期目标 |
| L3 | 归档记忆 | 深层档案 | 历史会话原文、过期记忆、审计数据、合规保留数据 | 合规保留，可按需恢复 | 历史回溯、审计 |

### 2.3 记忆内容分类（按内容类型分，横切四层）

温度分层回答"记多久、放多快"，内容分类回答"记的是什么、怎么用"。每条记忆必须有一个 `memory_kind`：

| memory_kind | 含义 | 示例 | 默认落层 |
|---|---|---|---|
| `event` | 发生过的事件 | "2026-10-08 客户A投诉物流延迟" | L1，过期降 L3 |
| `fact` | 客观事实 | "产品X标准报价每件100元" | L2（稳定）或 L1（临时） |
| `preference` | 用户/客户偏好 | "客户A偏好周二下午开会" | L1，长期偏好晋升 L2 |
| `procedure` | 程序性经验/方法 | "写产品介绍先讲用途，再列功能" | L1/L2，跨 Agent 共享 |
| `goal` | 长期目标 / KPI / 工作约束 | "未来一年把账号周触达做到 10 万，未经确认的内容先发我审阅" | L2，长期保留 |

> `goal` 类记忆吸收自 Sparse Agent：数字员工要持续负责一件事，就必须长期记住目标、衡量指标和行为边界。新任务开始时优先召回相关 `goal`。

### 2.4 存储分层与容量原则

| 原则 | 含义 |
|---|---|
| 热容量固定 | L0/L1 的活跃记忆有明确上限，保证检索速度和成本可控 |
| 冷数据可长期保留 | L2 和归档层通过对象存储、压缩、分层存储支撑数十年 |
| 遗忘优先于扩容 | 先压缩、合并、降级、归档，而不是一味加硬盘 |
| 逻辑删除，物理归档 | 企业记忆很多需要审计；但用户行使删除权时见 §8.3 级联删除 |
| 预算驱动 | 每个租户/用户/Agent/项目都有记忆预算，超出后触发压缩和淘汰 |
| 线索优先 | LIMS 内部只存线索和摘要，详细内容交由外部系统保管 |

---

## 3. 核心功能模块

### 3.1 记忆路由模块（MemoryRouter）

**职责：** 决定一条记忆该去哪一层、该带什么坐标、以及查询时该从哪一层取。

**功能：**

- **意图识别：** 判断输入是查询知识、记录事实、确认偏好，还是仅在当前对话使用。
- **层级选择：** 临时上下文 → L0；偏好/近期事件 → L1；企业知识/长期目标 → L2；合规保留/历史回溯 → L3。
- **跨层查询：** 多层并行检索，按 §3.4 融合结果。
- **记忆晋升/降级：** 依据 §5.3 的固有价值分数自动流转，不按固定时间硬晋升。
- **外部记忆路由：** 内部记忆不足以回答时，路由到外部连接器取补充信息。
- **跨会话召回：** 新任务开启时，自动召回相关 `goal`、`procedure`、`preference`。
- **预算协同：** 与 MemoryBudgetManager 协同，超预算触发压缩淘汰。

### 3.2 参考系坐标模块（ReferenceFrame）

**职责：** 为每条记忆绑定多维坐标，实现精准定位与隔离。

| 维度 | 说明 | 过滤类型 |
|---|---|---|
| 租户 tenant | 企业/组织隔离 | 硬过滤（安全边界） |
| 用户 user | 个人偏好隔离 | 硬过滤 |
| Agent agent | 哪一个/哪类 Agent 创建或使用 | 软排序 |
| 可见范围 visibility | private / shared_user / tenant / cross_tenant | 硬过滤 |
| 项目 project | 项目维度隔离 | 硬过滤 |
| 实体 entity | 关联的客户、产品等 | 软排序 |
| 场景 scene | 会议、工单、聊天、邮件等 | 软排序 |
| 任务 task | 报价、审批、咨询等 | 软排序 |
| 观察时间 assert_time | 记忆被观察/记录的时间（只增，写入后不改） | 软排序 |
| 生效区间 valid_from/valid_until | 事实声明为真的时间窗口（双时态，旧版本保留） | 软排序 |
| 分类 category | 业务分类 | 软排序 |

**可见范围（visibility）规则（吸收 Sparse Agent 共享记忆思路）：**

- `private`：仅创建者本人或指定 Agent 可见，默认值。
- `shared_user`：同一用户名下所有 Agent 可见——运营 Agent 确认的"先讲用途再讲功能"，写作 Agent 可直接复用。
- `tenant`：同租户内全员/指定角色可见。
- `cross_tenant`：跨租户共享，必须显式授权；实现方式是在记忆上挂 `shared_with_tenants: []`，查询过滤条件为 `tenant_id = current OR tenant_id IN shared_with_tenants`，而不是绕过硬过滤。

**坐标匹配：** 硬过滤维度不匹配直接排除；软排序维度输出 `[0,1]` 匹配度加权。

### 3.3 记忆提取器（MemoryExtractor）

> v1.3 新增。此前版本里它只在数据流图中出现、却没有模块定义，属于孤儿组件，现补齐。

**职责：** 从对话、文档、工具结果、外部系统同步数据中，自动抽取结构化记忆条目。

**功能：**

- **事实抽取：** 用规则 + LLM 从文本中抽出 `event / fact / preference / procedure / goal` 五类候选。
- **坐标自动标注：** 自动绑定 tenant、user、entity、scene、task；标注置信度低时打"待人工确认"标记。
- **只增（ADD-only）策略（v1.4 决策）：** 抽取结果一律**追加为新观察记录**，不做原地更新；相同幂等键的重复提交直接忽略（防重），同一事实的新观察作为新版本保留，冲突裁决推迟到读取路径（见 §3.4 / §4.6 读时融合）。
- **双时态标注：** 每条观察自动记录 `assert_time`（观察时间，只增不改）与 `valid_from`/`valid_until`（生效区间）；事实变更后旧观察保留为历史版本，不做物理覆盖（对照 Zep/Graphiti bi-temporal 实践）。
- **结构化输出：** 输出符合 §4.1 统一 schema 的记忆条目，交给 Router 落层。
- **人工校正入口：** 提供回写接口，人工修正后样本可用于后续提取效果优化。

### 3.4 投票融合模块（VoteFusion）

**职责：** 多条记忆对同一事实/问题有不同描述时，做加权融合而非简单覆盖。

**查询排序分（本次召回时计算，与具体查询相关）：**

| 维度 | 默认权重 | 说明 |
|---|---|---|
| 向量相似度 | 25% | 与查询的相关性 |
| 记忆固有价值 | 20% | 来自 §5.3，与查询无关 |
| 时效新鲜度 | 20% | 指数衰减，默认半衰期 L1=30 天、可配置 |
| 来源可信度 | 15% | 人工确认 > 系统提取 > 用户口述 |
| 参考系匹配度 | 15% | 与当前上下文坐标匹配 |
| 反馈历史 | 5% | 被采纳次数 / 总使用次数 |

**权重机制：** 上表为**先验默认权重**，由 FeedbackLoop 在线以指数移动平均（EMA）缓慢调整，避免冷启动震荡；新租户/新场景先跑默认权重，积累足够反馈后再个性化。

**融合策略：**

- **唯一最优：** 排序分最高者胜出。
- **接近融合：** `(top1 - top2) / top1 < 20%` 时，融合多条事实文本并附加各条来源说明。
- **冲突标记：** 无法融合时保留多条，标记"待人工确认"，不静默选一个。
- **时效裁决（双时态）：** 同一事实的多条观察先按生效区间过滤——生效区间覆盖查询时刻的版本优先；无覆盖时取最接近的已失效版本并标注历史时间，不直接丢弃。

### 3.5 动态记忆存储（DynamicMemoryStore, L1）

**职责：** 管理短期、可变的偏好、事件、共享经验。

**功能：** 添加/更新/删除；冲突检测与自动失效旧记忆；按半衰期衰减；过期清理；反馈记录；按参考系查询；投票融合排序。

**L1 记忆结构示例：**

```json
{
  "memory_id": "mem_01J8...",
  "idempotency_key": "hash(t1+u1+客户A+周二开会)",
  "tenant_id": "t1",
  "user_id": "u1",
  "agent_id": "agent_cs_01",
  "visibility": "shared_user",
  "memory_kind": "preference",
  "fact": "客户A偏好周二下午开会",
  "confidence": 0.85,
  "source": "dialogue_extraction",
  "permission_level": "internal",
  "valid_from": "2026-10-01",
  "valid_until": null,
  "expire_at": "2027-01-01",
  "importance": 0.92,
  "access_count": 15,
  "version": 3,
  "observed_at": "2026-10-08T10:30:00Z",
  "superseded_by": null,  // 被新观察替代时指向新 memory_id（双时态历史链）
  "reference_frame": {
    "entity": "客户A", "scene": "日程安排", "task": "会议预约"
  },
  "feedback_records": [
    {"status": "adopted", "time": "2026-10-05", "operator": "system"}
  ]
}
```

### 3.6 静态记忆存储（StaticMemoryStore, L2）

**职责：** 管理长期、稳定的知识与长期目标。

**功能：** 版本化管理；分类与标签；权限控制（public/internal/机密）；跨租户共享（走 visibility=cross_tenant）；批量导入导出；全文 + 向量检索；参考系绑定；投票融合排序。

**L2 记忆结构示例：**

```json
{
  "memory_id": "mem_01J9...",
  "tenant_id": "t1",
  "visibility": "tenant",
  "memory_kind": "fact",
  "content": "产品X的标准报价为每件100元",
  "source": "产品文档v2.1",
  "category": "产品价格",
  "permission_level": "internal",
  "version": 5,
  "version_history": [],
  "reference_frame": { "entity": "产品X", "category": "产品价格" }
}
```

### 3.7 工作记忆缓存（WorkingMemoryCache, L0）

**职责：** 管理当前会话的临时上下文。

**功能：** 滑动窗口（最近 N 轮）；关键信息提取；TTL 过期；结构化变量存储；会话压缩。

**L0 记忆结构示例（v1.3 补齐）：**

```json
{
  "memory_id": "mem_01JA...",
  "tenant_id": "t1", "user_id": "u1", "session_id": "sess_8842",
  "memory_kind": "event",
  "fact": "当前订单号 ORD-20261009-007，用户确认收货地址为济宁",
  "ttl_seconds": 3600,
  "ref": {
    "type": "external_reference",
    "summary": "刚查了 CRM 工单042，客户A投诉物流",
    "storage_location": "crm://tickets/20261001/042"
  }
}
```

### 3.8 目标记忆（GoalMemory, L2）

> v1.3 新增，吸收 Sparse Agent 的 Goal / KPI / Constraints 思路。

**职责：** 存储和召回驱动数字员工长期行为的目标、衡量指标与工作边界。

**结构示例：**

```json
{
  "memory_id": "mem_01JB...",
  "tenant_id": "t1", "user_id": "u1", "agent_id": "agent_social_01",
  "visibility": "private",
  "memory_kind": "goal",
  "goal": "未来一年持续运营微博与 X，让目标用户更了解产品",
  "kpi": {"metric": "周可核验触达量", "target": 100000, "cadence": "weekly"},
  "constraints": ["只使用确认过的产品事实", "发布前查重", "未经确认的内容先给用户审阅"],
  "status": "active",
  "created_at": "2026-10-01", "expires_at": "2027-10-01"
}
```

**行为：**

- 每次 Agent 启动新一轮工作时，自动召回该 Agent / 该用户名下所有 `status=active` 的 goal。
- KPI 达成、用户暂停、目标到期时，goal 转为 `paused / completed / expired`，降入 L3。
- constraints 作为硬边界参与 MemoryRouter 的写后校验。

### 3.9 会话压缩器（SessionCompressor）

**职责：** 对 L0 长会话自动压缩，提取高价值信息晋升到 L1。**作用域仅限 L0 会话流。**

**功能：** 每 N 轮或 L0 达容量上限时触发；抽取决策、偏好、待办、情绪信号；丢弃重复低价值片段；结构化输出写入 L1。

```text
原始对话流 → 滑动窗口 → SessionCompressor → [高价值片段 → L1] + [低价值片段 → 丢弃]
```

### 3.10 记忆生命周期管理（MemoryLifecycleManager）

> v1.3 将原 3.8 与原 3.10"存储与生命周期管理模块"合并为一个模块，消除职责重叠。

**职责：** 统一管理记忆从创建到删除的全生命周期，以及预算、遗忘、归档、压缩、审计、监控。它是一个**编排者**，下面挂五个执行引擎。

| 子组件 | 职责 |
|---|---|
| MemoryBudgetManager | 每租户/用户/Agent/项目配额，超预算触发下面的引擎 |
| ForgettingEngine | 按固有价值（§5.3）做衰减、合并、降级、逻辑删除 |
| ArchiveEngine | 低价值记忆转入 L3 对象归档/冷归档，可恢复 |
| CompressionEngine | 面向 L1/L2 批量记忆做结构化摘要（与 §3.9 SessionCompressor 的区别：后者面向 L0 实时会话流，本引擎面向 L1/L2 离线批量） |
| AuditLogger + StorageMonitor | 记录创建/修改/归档/删除；监控各层容量、成本、检索延迟 |

**生命周期状态机：**

```text
created → active → (promoted: L0→L1→L2) → (demoted: →L3 archived)
        ↘ superseded（同一事实的新观察替代旧观察：旧观察标记 superseded_by 保留为历史，参与读时融合）
                                      ↘ soft_deleted → (用户行使删除权 → purged，见 §8.3)
```

晋升/降级依据是 §5.3 的固有价值分数连续观测，而非单次阈值抖动。

### 3.11 反馈闭环模块（FeedbackLoop）

**职责：** 记录记忆被使用后的效果，反哺固有价值与投票权重。

**反馈来源：** 用户显式反馈（赞/踩/改）；模型隐式反馈（是否采纳该记忆生成回答）；任务结果反馈（用了这条记忆后任务是否成功）。

```text
记忆被检索 → 用于生成回答 → 记录是否采纳 → 用户反馈
   → 更新 confidence / 固有价值 / EMA 投票权重 → 影响下次融合
```

**可解释要求（吸收 Sparse Agent 实践）：** Agent 回答时应能引用"我依据了 mem_01J8，因为它与本问题向量相似度 0.91、参考系匹配、最近被采纳过 3 次"。因此每次查询都必须落一条 trace（见 §4.3）。

### 3.12 外部记忆连接器层（External Memory Connector）

**定位：** 位于内部记忆层之下、外部系统之上的"记忆桥梁"。内部记忆层（L0–L3）与外部连接器是**上下层关系**，不是平级。

```text
Agent / 应用 → MemoryRouter
                ├── 内部记忆层（L0 / L1 / L2 / L3）
                └── 外部记忆连接器层
                    ├── 数据库连接器
                    ├── 文档/知识库连接器
                    ├── 对象存储连接器
                    ├── 应用/API 连接器
                    └── 网页/搜索连接器
```

**连接器分类：**

| 连接器 | 典型来源 | LIMS 如何"记忆"它 |
|---|---|---|
| 数据库 | MySQL / PostgreSQL / Oracle / MongoDB / ClickHouse | 记表结构、字段含义、常用查询模板，不复制全量业务数据 |
| 文档/知识库 | Confluence / SharePoint / Notion / Wiki / RAGFlow | 记文档索引、摘要、标签，原文留在外部 |
| 对象存储 | S3 / OSS / MinIO / HDFS / FTP | 记文件元数据、版本、位置，需要时再读 |
| 应用/API | CRM / ERP / OA / 工单 / 订单 | 记接口能力、权限范围、调用结果摘要 |
| 网页/搜索 | 官网 / 行业站 / 搜索引擎 | 记来源可信度、抓取时间、关键结论 |

**四条核心原则：**

1. **只存线索，不搬全量。**
2. **外部系统是长期档案柜。** 内部存线索/偏好/摘要，外部存原始记录。
3. **每次外部读取都带权限和来源。** 记录来源系统、访问人权限、是否脱敏、更新时间、可否缓存。
4. **引用式记忆：** 一条记忆可以是一个外部引用：

```json
{
  "memory_type": "external_reference",
  "fact_summary": "客户A上周投诉物流延迟",
  "source_system": "CRM",
  "source_id": "ticket_20261001_042",
  "storage_location": "crm://tickets/20261001/042",
  "confidence": 0.92,
  "permission_level": "internal",
  "last_sync_time": "2026-10-08T15:30:00Z"
}
```

**每个连接器至少提供：** 连接管理、元数据索引、按需检索、结果短缓存、增量同步、来源追溯、安全控制（租户隔离/字段脱敏/访问审计）。

**实现优先级：** ① 文档/知识库连接器 → ② 数据库连接器 → ③ 对象存储连接器 → 后续 API、网页搜索、实时事件总线。

---

## 4. 数据模型与一致性

### 4.1 统一记忆 Schema

所有记忆共用基础字段，各层/各 kind 扩展自身字段：

- 标识：`memory_id`（ULID，按时间有序）、`idempotency_key`
- 隔离：`tenant_id`、`user_id`、`agent_id`、`visibility`、`project_id`、`permission_level`
- 内容：`memory_kind`、`fact/content`、`reference_frame`
- 双时态：`assert_time`（观察时间，只增不改）、`valid_from`、`valid_until`（生效区间；变更后旧观察物理保留）、`superseded_by`（被替代链，v1.4 新增）
- 质量：`confidence`、`source`、`version`、`importance`、`access_count`、`expire_at`
- 关系：`feedback_records[]`、`version_history[]`、`external_ref?`

### 4.2 一致性骨架（v1.4：pgvector 单库收敛）

L1/L2 的元数据与向量**统一落在 PostgreSQL（pgvector 扩展）**，单一事实源 = 单一存储：写入一次本地事务（元数据 + 向量）即完成，**消除"PG vs 向量库"双写不一致窗口**（决策 3：不再引入独立向量数据库）。

```text
写 PG 单库事务（元数据 + pgvector 向量，落 status=vectorized）
   → MQ 异步任务仅负责非事实写入的副作用：衰减调度 / 晋升 / 清理 / 审计
   → 对账任务（每小时）扫向量生成失败的超时项补偿（仅重算向量，事实已在库内）
```

读路径以 PG 为准；向量生成失败不阻塞写入主流程（该条先可被全文/元数据检索，向量重算后补全），不等同于丢失。

### 4.3 幂等、并发与检索 Trace

- **幂等键：** `idempotency_key = sha1(tenant_id + user_id + normalized_fact + reference_frame_business_key)`。同键重复提交直接忽略（防重）；不同观察（不同 assert_time/来源）作为**新版本追加保留**，不覆盖历史，由读时融合裁决（v1.4 只增语义）。
- **并发：** 写更新带 `version` 乐观锁；版本冲突时走 VoteFusion 冲突分支，不静默覆盖。
- **向量过滤：** 租户/用户/项目是**安全硬过滤，必须用 pre-filter**（向量索引内带标量字段过滤），禁止 post-filter，避免越权召回。
- **检索 Trace：** 每次 query 落一条 `retrieval_trace`：query 文本、命中的 memory_id 列表、各维度得分、最终融合策略、是否被采纳。这是排查"记错/记漏"和记忆污染的主要依据。

### 4.4 核心 API 骨架

```text
# 写入（记忆提取器/外部同步调用）
PUT /v1/memories
  body: { memory_kind, fact, reference_frame, visibility, source, ... }
  resp: { memory_id, idempotent_hit: bool, version }

# 读取（Agent 查询）
POST /v1/memories/query
  body: { query, reference_frame_filter, layers: [L0,L1,L2], top_k, as_of? }
  # as_of：可选时间回溯点；缺省为当前时刻（双时态时效裁决）
  resp: { results: [{memory_id, fact, score, source, observed_at, valid_from, valid_until, superseded_by, trace_id}], trace_id }

# 反馈
POST /v1/memories/{id}/feedback
  body: { status: adopted|rejected|edited, operator }

# 数据主权（见 §8.3）
GET  /v1/users/me/memories?export=true   # 导出
DELETE /v1/users/me/memories/{id}        # 行使删除权
```

### 4.5 写入流程

```text
用户输入 / 文档导入 / 工具结果 / 外部同步
   → MemoryExtractor 抽取候选记忆（只增：一律追加新观察，见 §3.3）
   → ReferenceFrame 自动标注坐标（含 assert_time / valid_from / valid_until）
   → Router 决策（L0/L1/L2/L3/外部连接器）
   → 幂等检查（同键重复提交忽略；新观察追加为新版本）
   → 写 PG 单库事务（元数据 + pgvector 向量）→ MQ 异步副作用（衰减/晋升/清理）→ 元数据索引 / 审计
```

### 4.6 读取流程

```text
Agent 查询 → 意图识别 → 参考系硬过滤（租户/用户/可见范围）
   → 多层并行召回：
       L0 向量+时间衰减 | L1 固有价值+融合 | L2 全文+向量
       L3 按需恢复     | 外部连接器按需查询（带超时）
   → 双时态时效裁决（剔除生效区间外的过期观察，保留为历史标记）
   → VoteFusion 读时融合去重排序 → 附带来源、时间与 trace_id
   → 返回 Agent → 记录反馈
```

**异常分支：** 外部连接器超时（默认 300ms 软超时，降级为"仅内部记忆回答"并在 trace 标注 `external_timeout`）、权限不足（静默剔除该来源并审计）、数据冲突（走 §3.4 冲突标记）、写入失败（MQ 重试 + 死信 + 对账）。

---

## 5. 存储预算与容量规划

### 5.1 存储分层总览

| 层级 | 存储位置 | 生命周期 |
|---|---|---|
| L0 | Redis / 内存 | 分钟~小时 |
| L1 | PostgreSQL（pgvector） | 天~月 |
| L2 | PostgreSQL（pgvector）+ 图数据库 + 对象存储 | 年~数十年 |
| L3 | 对象归档 / 冷存储 | 合规保留，可恢复 |
| 审计日志 | 日志库 / 对象存储 | 按合规要求保留 |
| 外部系统 | 由外部系统自管 | — |

### 5.2 容量预算模型

| 层级 | 预算示例 |
|---|---|
| L0 | 每会话 50KB~200KB 或最近 200 轮 |
| L1 | 每用户 2,000~10,000 条 |
| L2 | 每租户 50GB~500GB |
| L3 | 弹性归档，不严格限容 |
| 向量索引 | 每租户 100 万~1,000 万条（pre-filter） |

L1 接近上限时依次执行：压缩合并 → 低价值降级 L3 → 过时/重复逻辑删除。

### 5.3 记忆固有价值（用于晋升/降级/遗忘）

> v1.3 修正：与查询相关的"参考系匹配度"从这里移除，它只属于 §3.4 的查询排序分。否则同一条记忆在不同查询下"价值"漂移，晋升/降级不可复现。

```text
记忆固有价值 V（与查询无关，[0,1] 加权求和）=
    访问频率   × 0.25
  + 反馈质量   × 0.30
  + 来源可信度 × 0.25
  + 时效性     × 0.20        （指数衰减，默认半衰期可配，L1=30 天）
```

- V 持续下降 → L1 降 L3；
- V 持续上升 → L0 升 L1，经人工确认或长期稳定后沉淀 L2；
- `goal`、长期偏好、企业制度：即使访问少也不轻易遗忘（白名单保护）；
- 临时事实（"今天会议改下午"）过期自动失效。

---

## 6. 技术选型建议

| 组件 | 建议方案 | 说明 |
|---|---|---|
| 向量存储 | PostgreSQL 扩展 **pgvector**（HNSW 索引） | v1.4 决策：与元数据同库同事务，消除双写不一致；支持 HNSW/IVFFlat + 标量 pre-filter（租户隔离），不再引入独立向量库 |
| 关系数据库 | PostgreSQL | 元数据、参考系、反馈、审计（事实源） |
| 图数据库 | Neo4j / NebulaGraph | L2 实体关系知识图谱 |
| 缓存 | Redis | L0 工作记忆、热点记忆 |
| 全文检索 | Elasticsearch | L2 静态记忆全文搜索 |
| 消息队列 | RabbitMQ / Kafka | 向量写入、晋升、衰减、清理异步任务 |
| 对象存储 | S3 / OSS / MinIO | L2/L3 长期存储、连接器后端 |
| Embedding 模型 | BGE / text-embedding-3（**租户级可插拔**） | 参考 Sparse Agent：模型选择可配置，密钥加密存储 |
| 分词 | jieba / HanLP | 中文分词 |

---

## 7. 安全、隐私与数据主权

### 7.1 隔离与权限

- 租户/用户/项目硬过滤 + pre-filter，杜绝跨租户召回；
- 记忆级 `permission_level`（public/internal/机密）在返回前做二次过滤；
- 外部连接器读取按调用者权限执行，字段脱敏在连接器层完成。

### 7.2 审计

所有 create/update/archive/soft_delete/purge 落 AuditLogger，含操作人、时间、前后值摘要、trace_id。

### 7.3 删除权与级联清除（解决"逻辑删除"与 GDPR 的矛盾）

用户/合规要求硬删除时，按以下链路级联执行，而非只打个标记：

1. PG 记录 `purge_requested` 并先软删；
2. 异步任务：删除 PG 中该记忆行（含 pgvector 向量）→ 删 Redis 缓存 → 删检索 trace 明细；
3. 对 `external_reference` 记忆：只删 LIMS 侧引用，**不**回删外部业务系统原始记录（那属于外部系统权限），但在审计中记录"该用户在 LIMS 的引用已清除"；
4. L3 冷归档介质：按合规保留期到期后物理擦除（不可提前擦除的，做加密粉碎）；
5. 跨租户共享记忆：删本租户可见副本/授权，不影响其他租户持有的副本；
6. 全程审计可追溯。

### 7.4 数据导出

用户可一键导出自己名下全部记忆（JSON/CSV），对应 Sparse Agent"数据和文件有明确归属、用户始终有入口管理"的原则。

---

## 8. 实施路线图

> 调整：记忆提取器从原第六阶段提前到第一阶段，因为它是写入流程的入口。

| 阶段 | 周期 | 内容 |
|---|---|---|
| P1 核心骨架 | 4~6 周 | 统一 Schema（含双时态）、ReferenceFrame、MemoryExtractor（只增+读时融合）、L0/L1/L2 存储（pgvector 单库）、基础 CRUD、PG 单库一致性骨架 |
| P2 智能路由 | 3~4 周 | MemoryRouter、意图识别、跨层召回、晋升/降级 |
| P3 融合与反馈 | 3~4 周 | VoteFusion、冲突检测、FeedbackLoop、retrieval_trace |
| P4 生命周期 | 3~4 周 | BudgetManager、Forgetting/Archive/Compression 引擎、审计、监控 |
| P5 外部连接器 | 3~4 周 | 文档/库/对象存储连接器、引用式记忆、增量同步 |
| P6 目标与共享 | 3~4 周 | GoalMemory、visibility 共享模型、SessionCompressor、管理后台、批量导入导出 |
| P7 合规与优化 | 持续 | GDPR 级联删除/导出、性能调优、多语言、开放 API/SDK |

---

## 9. 关键指标（KPI）

| 指标 | 目标 | 评测口径 |
|---|---|---|
| 记忆检索准确率 | > 90% | 用人工标注的查询-相关记忆评测集（≥500 条），top-k 命中率；对话内基准对齐 LongMemEval / LoCoMo 口径 |
| 历史/时效查询正确率 | > 85% | 双时态评测：含"事实已变更""回溯历史时点"的查询，在查询时刻返回正确版本（LongMemEval 时间维度口径） |
| 投票融合正确率 | > 85% | 冲突样本集上人工评估融合结果 |
| 检索延迟 P95 | < 200ms | 纯内部多层召回；外部连接器另计软超时 300ms |
| 记忆晋升准确率 | > 80% | 抽样人工评估自动晋升是否合理 |
| 用户满意度 | > 4.5/5 | 记忆相关功能的显式评分 |
| 外部连接器可用性 | > 99% | 连接成功率（超时降级不算失败） |
| 预算合规率 | > 95% | 各层记忆量在预算内的比例 |

---

## 10. 风险与应对

| 风险 | 影响 | 应对 |
|---|---|---|
| 记忆污染（错误记忆写入） | 高 | confidence 机制、低置信度待人工确认、FeedbackLoop 修正、retrieval_trace 可排查 |
| 隐私泄露 | 高 | 租户/用户硬过滤 + 向量 pre-filter、permission_level 二次过滤、脱敏 |
| 检索性能瓶颈 | 中 | pre-filter 分层索引、热点缓存、异步衰减清理 |
| 冲突融合效果差 | 中 | 多版本保留、待确认标记、人工干预入口 |
| 记忆提取准确率不足 | 中 | 规则+模型、人工校正样本回流；v1.4 只增策略下误提取可通过读时融合稀释，并可整链修正 |
| 外部连接器不稳定 | 中 | 缓存、超时重试、降级内部记忆回答 |
| 存储成本失控 | 中 | 预算机制、自动归档、冷存储分层 |
| 向量与元数据不一致 | 低 | pgvector 单库同事务写入，消除双写窗口；向量生成失败可补偿重算（§4.2） |
| 删除权执行不到位 | 高 | §7.3 级联清除链路 + 审计 |

---

## 11. 团队与分工建议

| 角色 | 人数 | 职责 |
|---|---|---|
| 架构师 | 1 | 整体架构、技术选型 |
| 后端开发 | 2~3 | 核心模块、API、一致性 |
| AI/算法工程师 | 1~2 | 记忆提取、embedding、意图识别、权重调优 |
| 前端开发 | 1 | 管理后台、trace 调试工具 |
| 测试工程师 | 1 | 功能/性能/数据质量、评测集维护 |
| 产品经理 | 1 | 需求、场景、合规 |

---

## 12. 下一步行动

1. 团队评审本 v1.4 架构，确认命名（LIMS / lightmem / 光明记忆）与模块边界；
2. 细化 P1 需求，基于 §4.4 输出接口文档；
3. 搭建开发环境（PostgreSQL + pgvector、Redis、MQ、对象存储）；
4. 从统一 Schema + ReferenceFrame + MemoryExtractor 入手编码；
5. 外部连接器调研（Confluence / SharePoint / S3）；
6. 安全合规评审：租户隔离、visibility 共享模型、删除权级联链路。

---

## 附录 A：术语表

| 术语 | 说明 |
|---|---|
| L0/L1/L2/L3 | 工作/动态/静态/归档记忆，按温度分四层 |
| memory_kind | 记忆内容类型：event/fact/preference/procedure/goal |
| visibility | 可见范围：private/shared_user/tenant/cross_tenant |
| 参考系 | 记忆的多维坐标 |
| 固有价值 V | 与查询无关的记忆质量分，用于晋升/遗忘 |
| 查询排序分 | 本次召回时融合向量相似度/固有价值/坐标等的分 |
| 引用式记忆 | 只存外部系统引用和摘要，不搬全文 |
| retrieval_trace | 一次查询命中了哪些记忆、各打多少分的可追溯记录 |
| GoalMemory | 长期目标/KPI/工作约束记忆 |
| 双时态 | 观察时间（assert_time，只增不改）与生效区间（valid_from/until，表达事实何时为真）分离建模；事实变更后旧观察保留为历史版本 |
| 只增提取 | 记忆提取只追加新观察、不原地覆盖旧记录，冲突裁决推迟到查询时（读时融合） |
| 读时融合 | 多版本/冲突记忆在读取路径按双时态 + 固有价值加权裁决，写入路径只做追加 |

## 附录 B：版本历史

| 版本 | 日期 | 说明 |
|---|---|---|
| v1.0 | 2026-09-01 | 初始四层记忆架构 |
| v1.1 | 2026-09-20 | 增加会话压缩器、外部连接器、预算与遗忘引擎 |
| v1.2 | 2026-10-09 | 统一术语、修复表格/JSON/公式、强化隔离与职责边界 |
| v1.3 | 2026-10-09 | 定名 Light Intelligent Memory System（LIMS/光明记忆）；新增 memory_kind 内容分类、visibility 共享模型、MemoryExtractor、GoalMemory、L0 Schema、API 骨架、PG+向量库一致性、pre-filter、retrieval_trace、数据主权与级联删除；拆分"固有价值"与"查询排序分"；合并重复的生命周期模块；吸收 Sparse Agent 跨 Agent 经验共享与目标记忆思路 |
| v1.4 | 2026-10-09 | 采纳架构评审三项决策：① 提取策略改「只增 + 读时融合」（对照 Mem0 ADD-only 算法）；② 引入双时态（观察时间与生效区间分离，旧事实失效保留）；③ 向量存储收敛 pgvector（单库单事务，消除双写不一致）；同步收敛 P1 路线图与 KPI 口径 |

## 附录 C：参考文档
|参考 | 链接 |
|---|---|
| Sparse Agent：用目标、KPI 和约束定义数字员工 | https://zhuhailin.com/zh/blog/national-day-second-sparse-agent |
| Sparse Agent 使用手册：从第一个目标开始 | https://sparse-agent.com/cases/goal-kpi-constraints.html |
| Mem0（github.com/mem0ai/mem0）| 官方博客与论文：Mem0: Building Production-Ready AI Agents with Scalable Long-Term Memory；Introducing The Token-Efficient Memory Algorithm（2026-04）| 
| LangMem（github.com/langchain-ai/langmem）| 官方文档：Core Memory API、Memory Management Tools、Background Memory Manager | 
| 业内综述参考：| Agent Memory 分类与评测基准（CoALA、LoCoMo、LongMemEval、BEAM、DolphinBench）| 