# lightmem 接入验证手册（VERIFY）

> 配套 [`README.md`](./README.md)。本文件只讲**怎么验**，不讲原理。
> 顺序：先做 §0 通用自测（确认服务端没问题），再逐家做 §1（确认客户端没问题），最后做 §2 端到端闭环。

---

## 0. 通用自测（先证明服务端是好的）

> 前提：A 线原生 MCP 已上线 `:18444`。若还没上线，改用过渡通道（`:18443` + Python 桥，见 `README.md` §8），验证逻辑相同。

### 0.1 健康检查

```sh
curl -sS http://192.168.0.88:18443/healthz
# 期望：{"backend":"pgvector","embedder":"hash","profile":"family","status":"ok"}
```

### 0.2 MCP 握手（`initialize`）

```sh
curl -sS -D- -X POST http://192.168.0.88:18444/mcp \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H 'Authorization: Bearer <YOUR_TOKEN>' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"curl","version":"0"}}}'
```

期望：
- HTTP `200`；
- 响应头含 **`Mcp-Session-Id: …`**（后续请求必须带上，否则会话不认）；
- body 里 `result.protocolVersion` **等于你请求的版本**（服务端必须回显，不能硬写死）；
- body 里 `result.serverInfo.name` = `lightmem`。

> 响应可能是 `application/json`，也可能是 `text/event-stream`（SSE，内容在 `data:` 行里）。两种都算正常。

### 0.3 工具清单（`tools/list`）

```sh
SID=<上一步响应头里的 Mcp-Session-Id>
curl -sS -X POST http://192.168.0.88:18444/mcp \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H "Mcp-Session-Id: $SID" \
  -H 'Authorization: Bearer <YOUR_TOKEN>' \
  -d '{"jsonrpc":"2.0","method":"notifications/initialized"}' >/dev/null

curl -sS -X POST http://192.168.0.88:18444/mcp \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H "Mcp-Session-Id: $SID" \
  -H 'Authorization: Bearer <YOUR_TOKEN>' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
  | grep -o '"name":"lightmem_[a-z_]*"' | sort -u
```

期望 **13 个**：`save` / `recall` / `search` / `update` / `feedback` / `forget` / `health` /
`goal_create` / `goal_progress` / `goal_state` / `plan_list` / `step_add` / `summary_add`。

### 0.4 调一个工具（`tools/call` → health）

```sh
curl -sS -X POST http://192.168.0.88:18444/mcp \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -H "Mcp-Session-Id: $SID" -H 'Authorization: Bearer <YOUR_TOKEN>' \
  -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"lightmem_health","arguments":{}}}'
```

期望：`result.isError` 不存在或为 `false`，内容里出现 `"status":"ok"`。

### 0.5 鉴权负向（应被拒）

```sh
# 缺 token
curl -sS -o /dev/null -w '%{http_code}\n' -X POST http://192.168.0.88:18444/mcp \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
# 期望：401（当服务端配置了 LIMS_AGENT_TOKENS 或 LIMS_REQUIRE_AUTH=1）
```

> 未配置任何 token 时（默认透传态），这里会返回 200——那是**向后兼容的预期行为**，不是缺陷。

---

## 1. 逐家验证

统一判据（三家 UI 文案不同，按这个看就行）：

1. 配置放好后重载 → Agent 的 MCP 列表里**出现 `lightmem` 且状态正常**；
2. 工具列表里出现 **`mcp__lightmem__*`**（DSH 前缀写法；其它家可能只显示原始名 `lightmem_health`）；
3. 调用 `lightmem_health` → 返回 `status=ok`；
4. 调用一次 `lightmem_recall`（随便一个词）→ 返回结果数组，**不是 500 / 不是 401**。

### 1.1 DSH（DeepSeek Harness）

1. 把 `configs/dsh.cordis.patch.yml` 的 `- insert:` 块**追加**到
   `%USERPROFILE%\.dsh\profiles\desktop\cordis.patch.yml` 末尾（不要覆盖已有条目）；
2. 保存。**不需要重启**——`dsh-hmr` 会热重载，日志里能看到 `mcp-lightmem` 重新加载；
3. 新开一轮对话，问「你有哪些 lightmem 工具」→ 应列出 13 个 `mcp__lightmem__*`；
4. 让 Agent 调 `mcp__lightmem__lightmem_health` → 输出含 `"status":"ok"`。

**失败定位**：工具不出现 → 99% 是 §2 的命名陷阱（`transport` 写成了驼峰/下划线），或 token 里带了不可见字符。
把 `failOnStartupError` 临时改成 `true`，启动失败会直接报错而不是静默。

### 1.2 Trae CN / TRAE SOLO CN

1. 新建 `%APPDATA%\Trae CN\User\mcp.json`（SOLO 用 `%APPDATA%\TRAE SOLO CN\User\mcp.json`），
   内容 = `configs/trae.mcp.json`（已替换 token）；
2. **完全退出** Trae 再启动（MCP Server 不会热重载）；
3. 打开 MCP 设置页 → 看到 `lightmem`，状态为已连接；工具数应为 13；
4. 在对话里让它「调用 lightmem_health」。

> 工作区级变体：`<工作区根>\.trae\mcp.json`（内容同 `configs/trae-workspace.mcp.json`）。
> 工作区级**优先于**用户级；同一台机器上两份都配会以工作区为准。

### 1.3 WorkBuddy

1. 把 `configs/workbuddy.mcp.json` 的 `mcpServers.lightmem` 块**合并**进
   `%USERPROFILE%\.workbuddy\mcp.json`（该文件已有 `lighthouse-ops`、`agentsync`，别覆盖）；
2. 在 `mcp-approvals.json` 里放行 `lightmem`；
3. 重启 WorkBuddy → 连接器/MCP 页看到 `lightmem` 已连接；
4. 调 `lightmem_health`。

**失败定位**：配置正确但列表里没有 → 检查审批（陷阱⑤）；JSON 报 `Unexpected token` → 文件带 BOM（陷阱⑥）。

### 1.4 小浣熊 / Box-Agent

1. 把 `configs/raccoon.mcp.json` 的 `mcpServers.lightmem` 块合并进
   `%USERPROFILE%\.box-agent\config\mcp.json`（**用户层**；`mcp.system.json` 是托管文件，别改）；
2. 重启小浣熊；
3. 设置 → MCP 里看到 `lightmem`，`alwaysLoad: true` 意味着它进首屏工具集；
4. 调 `lightmem_health`。

**失败定位**：写成了 `type: "streamableHttp"` → 静默不生效（小浣熊要**下划线** `streamable_http`）。

### 1.5 OpenCode / DuMate

1. 在工作区根放 `opencode.json`（内容 = `configs/opencode.json`），与既有配置**合并**（保留 `model`/`provider`）；
2. 重启进程（opencode 不热重载 MCP）；
3. 会话里问「列出 lightmem 工具」或看启动日志里的 MCP 连接行；
4. 调 `lightmem_health`。

> DuMate 的 `resources\config\opencode\opencode.json` 是**随安装包分发**的文件，升级可能被覆盖。
> 优先用工作区级 `opencode.json`，把 App 内置那份只当兜底。

**失败定位**：用了 `mcpServers` → OpenCode 只认顶层 `mcp`，静默忽略。

### 1.6 CodeArts（华为）

两条路，先看这条 IDE 跑在哪：

- **跑在 Linux/FreeBSD（有 `lightmem-mcp` 二进制）** → `configs/codearts.mcp_settings.json`；
- **跑在 Windows（本机现状）** → `configs/codearts.bridge.mcp_settings.json`（Python 桥，**今天就能用**）。

步骤：
1. 合并 `mcp.lightmem` 块进 `%USERPROFILE%\.codearts\mcp\mcp_settings.json`
   （镜像目录 `%USERPROFILE%\.codeartswork\mcp\` 同步一份，两处都被读取）；
2. 确认 `enabled: true`，重启 IDE；
3. 设置 → MCP 看到 `lightmem`，状态正常；
4. 调 `lightmem_health`。

**注意**：CodeArts 的 `command` 是**数组**（不是 `command` + `args` 分开），写成分开形式会被忽略。

### 1.7 Roo Code

1. 新建 `%APPDATA%\Code\User\globalStorage\rooveterinaryinc.roo-cline\settings\mcp_settings.json`
   （便携版 VSCode：`G:\VSCode\admin\data\user\globalStorage\…`，目录可能要先创建）；
2. 内容 = `configs/roo-cline.mcp_settings.json`；
3. 重载窗口 → Roo 的 MCP Servers 面板里 `lightmem` 为 Connected；
4. 调 `lightmem_health`。

> Roo 支持远程传输时，更推荐 `type: "streamable-http"` + `url` + `headers`，Windows 上不需要任何本地二进制。

### 1.8 stdio 原生通道（Linux/FreeBSD 上的 CodeArts / Cline / 自建客户端）

不经过任何 Agent，先证明二进制本身能握手：

```sh
printf '%s\n%s\n%s\n' \
 '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"shell","version":"0"}}}' \
 '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
 '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
 | /usr/local/lightmem/lightmem-mcp -transport stdio -profile /usr/local/lightmem/profiles/lims-family.profile.yaml
```

期望：**每行一条 JSON 响应**（`initialize` 回显版本、`tools/list` 返回 13 个工具），stderr 无 panic。
`notifications/initialized` 不产生响应，属正常。

---

## 2. 端到端闭环（R21 核心验收）

在**任意一家**已连通的 Agent 里依次执行（或直接 curl 对应 REST）：

| # | 动作 | 工具 | 期望 |
|---|---|---|---|
| 1 | 建计划 | `lightmem_goal_create`（`fact` + `goal_detail{constraints,kpi,deadline}` + `tenant_id`） | 201 / 返回 `memory_id` |
| 2 | 记步骤 | `lightmem_step_add`（`tenant_id` + `project_id` + `goal_id` + `seq`） | 成功，`kind=procedure` |
| 3 | 推 50% | `lightmem_goal_progress`（`memory_id` + `progress=0.5` + `tenant_id`） | 成功 |
| 4 | 记总结 | `lightmem_summary_add`（`tenant_id` + `project_id` + `fact`） | 成功，`kind=event` |
| 5 | 列计划 | `lightmem_plan_list`（`tenant_id` + `status=active`） | **能看到第 1 步那条，且 `progress=0.5`** |
| 6 | 召回 | `lightmem_recall`（计划关键词） | 命中计划、步骤、总结三类 |

> **字段名偏差（验收前必须裁决）**：`goal/state` 的入参名，任务书 §3.2 冻结为 `status`，
> 当前 C 线实现读的是 `state`（`kernel/router_tools.go:278`）。两者未对齐时，
> Agent 传 `status` 会**静默无效**（HTTP 200 但状态不变）。A 线包 MCP tool 前必须先统一，
> 否则工具描述与实际行为不符。本文件示例按**冻结口径 `status`** 写；若实测无效，先试 `state`。
>
> **身份未开时 `tenant_id` 必填**：`tenant_id` 目前由 body 提供；B 线鉴权落地后由 token 决定，body 值被忽略。

### 2.1 中文 UTF-8 往返（必做）

```
save:   "R21 验证：中文写入测试 —— 计划、步骤、总结、进度四类内容都要能原样召回。"
recall: "中文写入测试"
```
期望：召回内容与写入**逐字一致**，无乱码、无 `surrogates not allowed`、无 `\uXXXX` 转义残留。

### 2.2 跨 Agent 共享（≥2 个 Agent）

1. Agent A 写一条 `visibility=tenant`：「共享大脑验证：A 写入，B 应能召回」；
2. Agent B `recall` 同一句 → **命中**；
3. Agent A 写一条 `visibility=private`：「私有隔离验证：只有 A 能看」；
4. Agent B `recall` → **召不到**（正确隔离）；
5. Agent A 自己 `recall` → 能召到。

> 这一步同时是 G8 的行为侧验证：**不传 viewer / 异用户 viewer 都不该看到别人的 private**。

### 2.3 身份不冒充

1. 用 `trae` 的 token，body 里故意写 `"user_id":"agent-dsh"`；
2. 写入后查库 / 召回：落库的 `user_id` 应为 **`skywalk`**（token 决定），不是 `agent-dsh`。

---

## 3. 验收记录表（逐家填，交付时附）

| Agent | 工具出现 | health=ok | recall 有结果 | 端到端闭环 | 备注 |
|---|---|---|---|---|---|
| DSH | ☐ | ☐ | ☐ | ☐ | |
| Trae CN | ☐ | ☐ | ☐ | ☐ | |
| TRAE SOLO CN | ☐ | ☐ | ☐ | ☐ | |
| WorkBuddy | ☐ | ☐ | ☐ | ☐ | |
| 小浣熊 / Box-Agent | ☐ | ☐ | ☐ | ☐ | |
| OpenCode / DuMate | ☐ | ☐ | ☐ | ☐ | |
| CodeArts | ☐ | ☐ | ☐ | ☐ | 桥通道 / 原生 |
| Roo Code | ☐ | ☐ | ☐ | ☐ | |
| stdio 原生（§1.8） | ☐ | ☐ | ☐ | ☐ | 在 0.88 执行 |

---

## 4. 一条命令跑完服务端侧（0.88 上执行）

```sh
# 只验「服务端是否就绪」，不涉及任何 Agent
sh -c '
set -e
B=http://127.0.0.1:18444/mcp
H="-H Content-Type:application/json -H Accept:application/json,text/event-stream"
curl -sS $B $H -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-11-25\",\"capabilities\":{},\"clientInfo\":{\"name\":\"t\",\"version\":\"0\"}}}" | head -c 400; echo
'
```

> 若 `18444` 未监听：`sockstat -4l | grep 18444` 确认；启动方式见 E 线 `integrations/mcp-native/`。
