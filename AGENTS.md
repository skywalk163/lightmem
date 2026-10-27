# AGENTS.md —— 给 AI Agent 的仓库入口

> 本文件供 **AI Agent 阅读**（也让人类快速上手）。
> 如果你是一个 AI，正被要求"接入 lightmem"或"用 lightmem 记点东西"，从这里开始。

---

## 一句话

**lightmem 是一个独立运行的记忆服务。** 你（AI）通过 **MCP** 连到它，
就能读写长期记忆；多个 AI 连同一个 lightmem，就能**共享记忆**。

---

## 你要做什么（二选一）

### A. 让我"接入" lightmem（配置我的客户端）

**优先读这个文件并执行**：

```
integrations/quickstart/install.sh  生成（若已运行过）
    → _lightmem_run/clients/agent-prompt.md
```

如果该文件已存在，**直接照它执行**（它会告诉你备份、写哪个文件、要不要重启）。
如果还不存在，先在仓库根跑：

```bash
sh integrations/quickstart/install.sh
```

它会起服务、签发 token、并生成 `agent-prompt.md`。

**核心事实**（写配置时必须正确，否则**静默失效**）：

| 客户端 | 类型标记 | 顶层键 |
|---|---|---|
| DeepSeek Harness | `transport: streamable-http`（连字符） | `- insert:` 条目 |
| WorkBuddy | `type: "streamableHttp"`（驼峰） | `mcpServers` |
| 小浣熊 / Box-Agent | `type: "streamable_http"`（下划线） | `mcpServers` |
| Trae / SOLO | 无类型标记，用 `url` | `mcpServers` |
| OpenCode / DuMate | `type: "remote"` | `mcp` |
| CodeArts | `type: "local"`，`command` **数组** | `mcp` |
| Roo Code / Cline | `type: "stdio"`，`command` **字符串**+`args` | `mcpServers` |

- 服务地址形如 `http://<host>:<port>/mcp`（默认 MCP 端口 `7444` 或部署时指定）。
- 身份用 `Authorization: Bearer <token>`；**token 由 issue_token.py 签发**。
- stdio 类（CodeArts / Roo）**每台机各起一个进程、只带自己那一条 token**，
  用 `LIMS_AGENT_TOKENS` 指向单条 JSON。

### B. 我已经接好了，只是要用它

见下方「工具清单」与「推荐习惯」。

---

## 工具清单（13 个，连上后即可调用）

**基础读写**

| 工具 | 用途 |
|---|---|
| `lightmem_save` | 写一条记忆（只增不改） |
| `lightmem_recall` | **首选检索**：跨层召回 + 多信号融合 |
| `lightmem_search` | 轻量检索（关键词/相似度直查） |
| `lightmem_update` | 更正（追加新版本，旧版标记被取代） |
| `lightmem_feedback` | 反馈 adopted / rejected / edited |
| `lightmem_forget` | 软删除（保留审计痕迹） |
| `lightmem_health` | 健康检查（排障用） |

**工作流记忆**（这是它区别于普通笔记的地方）

| 工具 | 用途 |
|---|---|
| `lightmem_goal_create` | **建计划**（`goal_detail` 必填：status/constraints/kpi/deadline/progress） |
| `lightmem_goal_progress` | 推进度（0~1；≥1 自动置 completed） |
| `lightmem_goal_state` | 改状态（active/paused/completed/expired，走状态机） |
| `lightmem_plan_list` | 列计划（可按 status 过滤） |
| `lightmem_step_add` | 记步骤（`procedure`，带 project_id / goal_id） |
| `lightmem_summary_add` | 记总结（`event`，带 project_id / goal_id） |

---

## 推荐的使用习惯（建议写进你的系统提示）

```
1. 动手前先回忆：遇到"这个项目/这个主题"，先用 lightmem_recall 查历史结论与偏好。
2. 做完就沉淀：用 lightmem_save 记关键决策；用 lightmem_step_add 记关键步骤。
3. 共享 vs 私密：给团队看的信息用 visibility=tenant；私人草稿用 visibility=private。
4. 长期任务用计划：多步任务先 lightmem_goal_create，再逐步推进度、记步骤、写总结。
```

---

## 已知边界（别踩）

| 事项 | 说明 |
|---|---|
| **换发 token 必须重启服务** | 服务端只在启动时装配 token 表；新 token 不重启会 `401` |
| **类型标记写错不报错** | 只是工具永远不出现。见上表 |
| **`goal` 必须带 `goal_detail`** | 否则被服务端 `422` 拒绝（这是策略要求，不是 bug） |
| **跨租户访问会被 404/401** | 身份由 token 定；你只能访问自己租户的记忆 |
| **`visibility=private` 他人不可见** | 这是硬过滤，也是设计意图 |
| **solo 档无向量检索** | 自动降级为文本检索，不影响使用；需要语义向量请上 family 档 |
| **别盲抄 `agent-prompt.md` 的端口与 token** | 那份 `clients/` 产物可能是**历史部署**留下的（实测：里面写的 `7450/7451` 端口已停用，其 token 全部 `401`）。当前权威部署是 `/usr/local/lightmem`：REST `:18443`、MCP `:18444`，身份 `deepseek-harness / dsh-agent / dshteam`，生效 token 见 `~/.dsh/profiles/web/cordis.patch.yml`。**先探活再动手**：`curl -s http://127.0.0.1:18443/healthz` 应返回 200，再带 `Bearer` 打 `POST /tools/memory/save` 看是否 401 |

---

## 关键文件

| 路径 | 何时看 |
|---|---|
| `GETTING_STARTED.md` | **人**要部署/接入时（含宣传与 5 分钟上手） |
| `integrations/quickstart/install.sh` | 一键部署服务端 |
| `integrations/agent-onboarding/issue_token.py` | 签发 token |
| `integrations/agent-onboarding/gen_configs.py` | 生成各家配置 |
| `integrations/agent-onboarding/README.md` | 接入原理、逐家细节 |
| `integrations/agent-onboarding/VERIFY.md` | 逐家验证清单 |
| `lightmem.md` | 完整架构设计（Schema、分层、安全模型） |
| `docs/api.md` / `docs/api.openapi.yaml` | REST API 参考 |

---

## 开发约定（如果你要改这个仓库）

- **门禁**：`sh scripts/gate.sh`（版本/gofmt/vet/build/test/race/覆盖/红线白名单/PG 真机）。
- **红线**：
  - 只增不改 Schema 语义（`model.go` / `storage.go` / `lifecycle.go` / `compliance.go`）；
  - 既有 SQL 不动（`kernel/migrations/*.sql`）；
  - **不新增第三方依赖**（MCP 协议是纯 stdlib 手写；`go.mod` 应保持零变化）；
  - 接线默认关（新能力不得改变既有行为）；
  - **推送需用户示意**，不要自动 push。
- **编译/测试在 FreeBSD 0.88 上做**（本机 Windows 只编辑与传输，不跑 go）。
- 新文件必须 `gofmt`（门禁 `[1b]` 是硬门禁，阈值 0）。
- 提交信息用 `feat/fix/docs(R编号): 简述` 格式。
