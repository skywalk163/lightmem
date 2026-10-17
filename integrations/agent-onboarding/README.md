# lightmem Agent 接入手册

> 目标：让几乎所有 AI Agent 都能连上 lightmem，把**工作计划 / 步骤 / 总结**存进去，成为大家的共享 AI 记忆大脑。
> 本目录（`integrations/agent-onboarding/`）是 R21 D 线交付：**各家配置模板 + 生成器 + 验证手册**。

> **⚠️ 新用户请先看仓库根的 [`GETTING_STARTED.md`](../../GETTING_STARTED.md)**（5 分钟一键接入）。
> 本文件是**逐家细节与原理**，适合排障或想了解每家差异时读。

---

## 0. 三十秒版

1. **两条通道，按 Agent 能力二选一**（不能反过来）：
   - 能发 HTTP 的 → `http://<你的主机>:<端口>/mcp`（原生 MCP，**主通道**，一个端点服务所有 Agent）
   - 只能吃 stdio 的 → `lightmem-mcp -transport stdio`（原生兜底）；Windows 上没有该二进制，用 Python 桥过渡（见 §8）
2. 从 `configs/` 里挑对应那家的文件，把 `<YOUR_TOKEN>` 换成真 token，粘到 §1 表里的**绝对路径**。
3. 重载该 Agent，看到 `mcp__lightmem__*` 工具即成功；调 `lightmem_health` 应返回 `status=ok`。

不想手工改？**用跨平台的 Python 生成器** `gen_configs.py`（只产出待粘贴文件，**不碰你的配置目录**）：

```bash
cd integrations/agent-onboarding
python3 gen_configs.py --server http://192.168.0.88:18444 --tokens ./agent-tokens.json
# 产物在 ./out/，附 MANIFEST.md（哪个文件搬到哪个路径）
#         与 agent-prompt.md（可直接粘给 AI 的自我安装指令）
```

> **Windows 用户注意**：本目录另有一个 `gen_configs.ps1`，
> **已废弃（DEPRECATED）**，仅 Windows 能跑、且 token 表格式与服务端不一致。
> **请统一用上面的 `gen_configs.py`**（Windows 上同样能跑 `python gen_configs.py`）。

---

## 1. 接入总表（8 家，一页看完）

路径均已在本机（Windows，2026-10-17）逐条核对：**「待创建」= 目录在、文件还没有**，新建即可。

| Agent | 配置文件绝对路径 | 传输 | 判别的关键字段 | 顶层键 | 生效方式 | 模板 |
|---|---|---|---|---|---|---|
| **DSH**（DeepSeek Harness） | `%USERPROFILE%\.dsh\profiles\desktop\cordis.patch.yml`（已有） | streamable-http | `transport: streamable-http` | `- insert:` 补丁条目 | 保存即 HMR 热重载 | `configs/dsh.cordis.patch.yml` |
| **Trae CN** | `%APPDATA%\Trae CN\User\mcp.json`（待创建） | http | `url`（**无 type**） | `mcpServers` | 重启 MCP Server | `configs/trae.mcp.json` |
| **TRAE SOLO CN** | `%APPDATA%\TRAE SOLO CN\User\mcp.json`（待创建） | http | `url`（**无 type**） | `mcpServers` | 重启 MCP Server | `configs/trae-solo.mcp.json` |
| **Trae（工作区级）** | `<工作区根>\.trae\mcp.json`（待创建） | http | `url` | `mcpServers` | 重载窗口 | `configs/trae-workspace.mcp.json` |
| **WorkBuddy** | `%USERPROFILE%\.workbuddy\mcp.json`（已有） | streamableHttp | `type: "streamableHttp"`（**驼峰**） | `mcpServers` | 重启 + `mcp-approvals.json` 审批 | `configs/workbuddy.mcp.json` |
| **小浣熊 / Box-Agent** | `%USERPROFILE%\.box-agent\config\mcp.json`（用户层，已有） | streamable_http | `type: "streamable_http"`（**下划线**） | `mcpServers` | 重启小浣熊 | `configs/raccoon.mcp.json` |
| **OpenCode / DuMate** | `<工作区根>\opencode.json`；DuMate 亦读 `%LOCALAPPDATA%\Programs\DuMate\resources\config\opencode\opencode.json` | remote | `type: "remote"` | **`mcp`** | 重启进程 | `configs/opencode.json` |
| **CodeArts**（华为） | `%USERPROFILE%\.codearts\mcp\mcp_settings.json`（已有，镜像 `.codeartswork\mcp\`） | **仅 local/stdio** | `type: "local"` + `command` **数组** | **`mcp`** | `enabled: true` + 重启 IDE | `configs/codearts.mcp_settings.json`（原生）/ `configs/codearts.bridge.mcp_settings.json`（Windows 可用） |
| **Roo Code**（VSCode 扩展） | `%APPDATA%\Code\User\globalStorage\rooveterinaryinc.roo-cline\settings\mcp_settings.json`（本机无，便携版见下） | stdio（支持 remote） | `type: "stdio"` | `mcpServers` | 重载窗口 | `configs/roo-cline.mcp_settings.json` |

> **Roo Code 便携版**：本机扩展在 `G:\VSCode\admin\extensions\rooveterinaryinc.roo-cline-3.54.0`，
> 该 VSCode 是便携安装且**尚未生成 data 目录**。首次创建 settings 时路径为
> `G:\VSCode\admin\data\user\globalStorage\rooveterinaryinc.roo-cline\settings\mcp_settings.json`。
> 若 Roo 版本支持远程传输，更省事：`{"type":"streamable-http","url":"http://192.168.0.88:18444/mcp","headers":{"Authorization":"Bearer <YOUR_TOKEN>"}}`（Windows 上无需任何二进制）。

---

## 2. 三大命名陷阱（**配错就静默不生效**）

这不是风格问题。字段名写错时，**没有任何报错**：Agent 正常启动，只是 `mcp__lightmem__*` 永远不出现。逐家核对：

| 语义 | DSH | WorkBuddy | 小浣熊 / Box-Agent | Trae | OpenCode / DuMate | CodeArts |
|---|---|---|---|---|---|---|
| **HTTP 类型标记** | `transport: streamable-http`（连字符小写） | `type: "streamableHttp"`（**驼峰**） | `type: "streamable_http"`（**下划线**） | 用 `url`，**不写 type** | `type: "remote"` | **不支持 HTTP** |
| **顶层键** | `- insert:` 补丁条目 | `mcpServers` | `mcpServers` | `mcpServers` | **`mcp`** | **`mcp`** |
| **stdio 命令** | `command` + `args` | `command` + `args` | `command` + `args` | `command` + `args` | `command`（**数组**） | `command`（**数组**） |
| **环境变量键名** | `env`（会被清洗，见④） | `env` | `env` | `env` | `environment` | `environment` |

三个反例（都能让工具消失，且不报错）：

```jsonc
// ❌ WorkBuddy 写成下划线
{ "mcpServers": { "lightmem": { "type": "streamable_http", "url": "..." } } }

// ❌ 小浣熊写成驼峰
{ "mcpServers": { "lightmem": { "type": "streamableHttp", "url": "..." } } }

// ❌ CodeArts / OpenCode 用 mcpServers（它们的顶层键是 mcp）
{ "mcpServers": { "lightmem": { ... } } }
```

### 另外三个坑（同样静默）

**④ DSH 的 stdio 通道会清洗环境变量。** 名字匹配 `KEY|PASSWORD|SECRET|TOKEN` 的环境变量会被丢掉——
所以 `LIMS_AGENT_TOKENS` 这类名字**不能**通过 stdio 传给 DSH。HTTP 通道走 `headers`，不受影响（模板已按此写）。

**⑤ WorkBuddy 需要额外审批。** 写进 `mcp.json` 后还要在 `mcp-approvals.json` 里放行，否则工具列表里看不到。

**⑥ 生成文件的编码。** 所有 `*.json` 必须是 **UTF-8 无 BOM**——带 BOM 的 JSON 会被部分 Agent 的解析器直接拒绝
（`Unexpected token`）。`gen_configs.py` 已强制无 BOM。
（历史注记：已废弃的 `gen_configs.ps1` 也强制无 BOM；而 `*.ps1` 自身相反，
Windows PowerShell 5.1 **必须**有 BOM 才按 UTF-8 解析，否则中文注释会把脚本语法撑坏——也是实测踩到的。）

---

## 3. 目录与模板清单

```
integrations/agent-onboarding/
├── README.md               ← 本文件：总表 + 陷阱 + 失败模式
├── VERIFY.md               ← 逐家手工验证步骤（含 curl 自测）
├── gen_configs.py          ← ✨ **跨平台生成器（推荐）**：Windows/macOS/Linux/FreeBSD
│                              产出 9 个配置片段 + MANIFEST.md + agent-prompt.md
├── issue_token.py          ← ✨ **token 签发**（每 Agent 一份，防冒充）
├── gen_configs.ps1         ← ⚠️ **已废弃 DEPRECATED**（仅 Windows + PS 5.1）
│                              改用上面的 gen_configs.py
├── tokens.example.json     ← token 表样例（全是占位符）
├── .gitignore              ← 忽略 out/ 与本地真实 token 表
└── configs/
    ├── dsh.cordis.patch.yml                 DSH（streamable-http）
    ├── trae.mcp.json                        Trae CN（用户级）
    ├── trae-solo.mcp.json                   TRAE SOLO CN（用户级）
    ├── trae-workspace.mcp.json              Trae（工作区级变体）
    ├── workbuddy.mcp.json                   WorkBuddy（streamableHttp）
    ├── raccoon.mcp.json                     小浣熊 / Box-Agent（streamable_http）
    ├── opencode.json                        OpenCode / DuMate（remote）
    ├── codearts.mcp_settings.json           CodeArts（stdio，原生 lightmem-mcp）
    ├── codearts.bridge.mcp_settings.json    CodeArts（stdio，Python 桥过渡，Windows 可用）
    └── roo-cline.mcp_settings.json          Roo Code（stdio）
```

模板里的占位符（生成器会替换，手工粘贴时自己填）：

| 占位符 | 含义 | 默认/示例 |
|---|---|---|
| `<YOUR_TOKEN>` | 该 Agent 的 token | 由 B 线签发，**每家一份** |
| `<LIGHTMEM_MCP_BIN>` | 原生 MCP 二进制 | `/usr/local/lightmem/lightmem-mcp` |
| `<LIGHTMEM_PROFILE>` | profile 路径 | `/usr/local/lightmem/profiles/lims-family.profile.yaml` |
| `<PYTHON_EXE>` | Python 解释器（桥） | `C:\Python314\python.exe` |
| `<BRIDGE_PY>` | 桥脚本（桥） | `…\integrations\dsh-mcp-bridge\lightmem_mcp_bridge.py` |
| `<TENANT_ID>` / `<USER_ID>` / `<AGENT_ID>` | 未开鉴权时的身份回退（仅 stdio 桥） | `dshteam` / `skywalk` / 各家 agent id |

> 模板中**不含任何真实 token**；`tokens.example.json` 里的值也是 `REPLACE-ME` 占位。

---

## 4. 生成器

### 4.1 `gen_configs.py` —— **推荐（跨平台）**

```bash
# 全量生成（token 由 issue_token.py 签发）
python3 gen_configs.py --server http://192.168.0.88:18444 --tokens ./agent-tokens.json

# 只生成两家
python3 gen_configs.py --server http://10.0.0.9:18444 --tokens ./agent-tokens.json \
  --agents dsh,workbuddy

# stdio 类需要本机二进制路径（默认 /usr/local/lightmem/lightmem-mcp）
python3 gen_configs.py --server ... --tokens ... --mcp-binary /path/to/lightmem-mcp
```

| 参数 | 默认 | 说明 |
|---|---|---|
| `--server` | 必填 | MCP 服务地址（自动补 `/mcp`） |
| `--tokens` | 必填 | `agent-tokens.json`（`issue_token.py` 产出，键 = **token**） |
| `--agents` | 全部 9 个 | 逗号分隔；`trae-workspace` 复用 `trae` 的 token |
| `--mcp-binary` | `/usr/local/lightmem/lightmem-mcp` | stdio 模板用 |
| `--rest-base` | `http://127.0.0.1:18443` | stdio 转发目标 |
| `--out` | `out` | 输出目录（**不写任何 Agent 配置目录**） |

**它做什么**：读 token → **先全量校验再落盘**（不会留残缺产物）→ 产出 9 个配置片段
+ `MANIFEST.md`（粘到哪/怎么粘/要不要重启）+ **`agent-prompt.md`（可直接粘给 AI 的自我安装指令）**。
**它不做什么**：不写 Agent 配置目录、不启动服务、不联网、不碰 git。

> 已实测（0.88 FreeBSD + 本机 Windows）：9 个片段全量生成；三大类型标记各自正确
> （`streamableHttp` 驼峰 / `streamable_http` 下划线 / `remote`）；DSH 用连字符；
> CodeArts `command` 为数组、Roo 为字符串+args。

### 4.2 `gen_configs.ps1` —— ⚠️ **已废弃（DEPRECATED）**

> **新用户请勿使用。** 仅保留给既有 Windows 用户的历史兼容，**不再更新**。

废弃理由：

| # | 问题 |
|---|---|
| 1 | 只能跑在 **Windows + PowerShell 5.1+**；macOS / Linux / FreeBSD 无法使用 |
| 2 | token 表格式（`{agent: token}`）与**服务端实际格式**（`{token: {agent_id…}}`）**不一致**，需手工转换，易配错 |
| 3 | 不产出 `agent-prompt.md`，无法让 AI 自行安装配置 |
| 4 | 需要 `-ExecutionPolicy Bypass` 才能执行（本机执行策略为 `Restricted`） |

**替代命令**（Windows 上一样能跑）：

```powershell
python gen_configs.py --server http://192.168.0.88:18444 --tokens .\agent-tokens.json
```

<details>
<summary>旧用法（仅供历史参考，不再维护）</summary>

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\gen_configs.ps1 -TokenFile .\tokens.example.json
```

| 参数 | 默认 | 说明 |
|---|---|---|
| `-OutDir` | `.\out` | 输出目录。**拒绝**落在任何 Agent 配置目录内 |
| `-McpHost` / `-McpPort` / `-RestPort` | `192.168.0.88` / `18444` / `18443` | 端点替换 |
| `-TokenFile` | — | token 表 JSON（`{agent: token}`） |
| `-DefaultToken` | — | TokenFile 未覆盖的 agent 用这个 |
| `-Agents` | 全部 | 逗号分隔 |
| `-Force` | 关 | 覆盖 OutDir 里已存在的文件 |

> 历史实测：10 个模板全量生成通过，9 个 JSON `json.load` 校验通过。
> 注意 `*.ps1` 在 Windows PowerShell 5.1 下**必须有 BOM** 才按 UTF-8 解析。

</details>

---

## 5. 身份与 token

**每个 Agent 一份 token，服务端按 token 定身份**（B 线 `AuthMiddleware`）；HTTP 通道用请求头
`Authorization: Bearer <token>`。写方身份强制：身份存在时，body 里的 `user_id`/`tenant_id` **被忽略**（防冒充）。

任务书 §3.1 冻结的 token 表覆盖 6 家，本目录覆盖 8 家，**缺两家**——需 B 线/部署侧补齐：

| Agent 键 | agent_id | user_id | tenant_id | 备注 |
|---|---|---|---|---|
| `dsh` | `deepseek-harness` | `dsh-agent` | `dshteam` | 冻结表已有 |
| `trae` | `trae` | `skywalk` | `dshteam` | 冻结表已有 |
| `trae-solo` | `trae-solo` | `skywalk` | `dshteam` | **需新增**（与 Trae 是不同进程，身份应可区分） |
| `workbuddy` | `workbuddy` | `skywalk` | `dshteam` | 冻结表已有 |
| `raccoon` | `raccoon` | `skywalk` | `dshteam` | 冻结表已有 |
| `opencode` | `opencode` | `skywalk` | `dshteam` | 冻结表已有 |
| `codearts` | `codearts` | `skywalk` | `dshteam` | 冻结表已有 |
| `roo-cline` | `roo-cline` | `skywalk` | `dshteam` | **需新增**（stdio 兜底也需要身份） |

两条使用规则：

- `visibility=private` 的 owner 必须是 token 的 `user_id`，否则 403（防止把私有记忆写到别人名下）。
- 共享大脑场景建议默认 `visibility=tenant`（G7）：不显式传也能被同租户其它 Agent 召回。私有内容显式传 `private`。

**stdio 通道的 token 传递方式未在冻结接口中定义**（§3.1 只定义了 HTTP 头）。
本目录模板暂留空 `env`，建议约定为进程环境 `LIMS_AGENT_TOKEN`（单值），由 `lightmem-mcp -transport stdio`
读取后注入身份——**待 B 线在 `auth.go` 落实**；未落实前，stdio 通道以 profile `auth:` 段的默认身份运行，
多 Agent 共用一个 stdio 二进制时无法区分彼此（写入的 `agent_id` 会相同）。这是 D 线发现的接口缺口，已登记到交付报告。

---

## 6. 工具面（13 个）

| 工具 | 用途 | 对应 REST | 轮次 |
|---|---|---|---|
| `lightmem_save` | 写一条记忆（只增不改） | `POST /tools/memory/save` | R160 |
| `lightmem_recall` | 跨层召回 + 多信号融合（**首选检索**） | `POST /tools/memory/recall` | R160 |
| `lightmem_search` | 轻量检索 | `GET /search` | R160 |
| `lightmem_update` | 更正（追加新版本，旧版标记被取代） | `POST /tools/memory/update` | R160 |
| `lightmem_feedback` | adopted / rejected / edited | `POST /tools/memory/feedback` | R160 |
| `lightmem_forget` | 软删除 | `POST /tools/memory/forget` | R160 |
| `lightmem_health` | 健康检查 | `GET /healthz` | R160 |
| `lightmem_goal_create` | **建计划**（需 `goal_detail`） | `POST /tools/memory/goal` | R21-C |
| `lightmem_goal_progress` | **推进度**（0~1） | `POST /tools/memory/goal/progress` | R21-C |
| `lightmem_goal_state` | **改状态**（active/paused/completed/expired） | `POST /tools/memory/goal/state` | R21-C |
| `lightmem_plan_list` | **列计划**（按 status 过滤） | `GET /tools/memory/plan` | R21-C |
| `lightmem_step_add` | **记步骤**（`kind=procedure`） | `POST /tools/memory/step` | R21-C |
| `lightmem_summary_add` | **记总结**（`kind=event`） | `POST /tools/memory/summary` | R21-C |

Agent 侧的典型用法（写进各家的规则/提示词即可）：

```
开工 → lightmem_plan_list 看有没有同类计划；没有就 lightmem_goal_create 建计划
推进 → lightmem_step_add 记关键步骤；阶段结束 lightmem_goal_progress 更新进度
收工 → lightmem_summary_add 写总结；下次 lightmem_recall 先查历史结论
```

> 工具名固定为 `mcp__<serverName>__<tool>`。`serverName` 必须是 `lightmem`，否则前缀不是 `mcp__lightmem__`，
> 各家提示词里写死的名字会全部失效。
>
> `lightmem_goal_state` 的入参名当前有分歧（冻结口径 `status`，C 线实现读 `state`），见 §7 对应行。

---

## 7. 常见失败模式（按现象查）

| 现象 | 根因 | 处置 |
|---|---|---|
| 配好重启后**工具一个都不出现**，无报错 | ① 类型标记写法不对（§2）② 顶层键不对 ③ 文件带 BOM | 逐条对 §2 表；`Get-Content -Encoding Byte -TotalCount 3` 看是否 `EF BB BF` |
| 工具出现但**一调就 401** | token 未填 / 填错 / 服务端未配该 token | 换成真 token；核对服务端 `LIMS_AGENT_TOKENS` 是否含该键 |
| 工具出现但**一调就 404 / `not visible`** | 缺 viewer，或 tenant 不匹配 | HTTP 通道由 token 定身份，检查 token 的 `tenant_id`；桥通道检查 `LIGHTMEM_USER_ID` |
| `lightmem_search` 传了词却**返回全部记忆、分数还一样** | 旧版服务端只读 `q`，客户端传的是 `text`（HEAD 已加 `text` 别名，R19-A） | 先确认服务端在 HEAD；自造客户端时 `q`+`text` 都发最稳 |
| 写中文报 **`surrogates not allowed`** | stdio 编码不是 UTF-8（Python 特有坑） | 原生 `lightmem-mcp` 是 Go，天然 UTF-8；用桥时别删 `reconfigure(encoding="utf-8")` |
| `lightmem_goal_create` 返回 **422 `goal memory requires goal_detail`** | 只传了 `fact`，没传 `goal_detail`（`constraints` 非空 + `kpi.target>0`） | 补 `goal_detail`；这是**策略正确行为**，不是 bug |
| `lightmem_goal_state` 传了 `status` 但**状态没变**（HTTP 200） | 字段名未对齐：任务书冻结 `status`，当前 C 线实现读 `state` | A 线包 MCP tool 前须裁决；实测时先试 `state` |
| 建计划/记步骤返回 **400 `tenant_id required`** | 未开鉴权时 `tenant_id` 必须由 body 提供 | 补 `tenant_id`；B 线鉴权落地后由 token 决定 |
| 连接成功但**调用超时** | `toolCallTimeoutMs` 太小 / 服务端慢查询 | DSH 默认 60s；本目录模板设 30s，慢环境调到 60000 |
| DSH 里**两个 serverName 冲突** | 新旧条目都叫 `lightmem` | 过渡期旧桥用 `serverName: lightmem_legacy` |
| **保存后重启服务记忆全丢** | 跑在 Solo/SQLite 内存档 | 用 pgvector 档（`profiles/lims-family.profile.yaml`） |
| `relation "memories" does not exist` | PG 未迁移 | 跑 `integrations/lims-088-rc.d/install_lims_rc_d.sh`（内建幂等迁移） |
| WorkBuddy 里配置写了却不生效 | 未在 `mcp-approvals.json` 放行 | 补审批后重启 |
| 便携版 VSCode 找不到 Roo 配置 | 该 VSCode 从未生成 `data\user` | 先创建 `G:\VSCode\admin\data\user\globalStorage\rooveterinaryinc.roo-cline\settings\mcp_settings.json` |

---

## 8. 过渡期与回滚（红线④⑩）

R21 上线顺序：**A 线 18444 就绪 → 各家切 HTTP → 观察稳定 → 拆桥**。切换前既有通道必须可用。

| 阶段 | DSH | 其它 Agent |
|---|---|---|
| **切换前（当前）** | `serverName: lightmem` + `transport: stdio` + Python 桥（R160 既有配置，**别动**） | 用 `codearts.bridge.mcp_settings.json` 等 stdio 桥模板 |
| **双通道并存** | 追加 `transport: streamable-http` 条目，`serverName: lightmem`；旧桥改 `serverName: lightmem_legacy` | HTTP 模板直接加；stdio 保持 |
| **切换后** | 删掉 legacy 条目 | Windows 侧只剩「只能 stdio」的 CodeArts / Roo 仍用桥 |

**回滚**（3 步，1 分钟内）：
1. 把 `dsh.cordis.patch.yml` 里新增的 HTTP 条目删掉（或整块注释）；
2. 把 `serverName: lightmem_legacy` 改回 `lightmem`；
3. 保存等 HMR；`lightmem_health` 恢复即回滚完成。

> 桥的定位：**过渡通道**。原生 MCP 上线后，Python 桥只保留给「只能吃 stdio」的 CodeArts / Cline 系；
> DSH 改走 `transport: streamable-http`。桥侧的处置说明由 E 线写入 `integrations/dsh-mcp-bridge/README.md` 顶部。

---

## 9. 安全须知

- **不要把 18443 / 18444 暴露到不可信网络**。G8（PG 后端 private 越权读）与 G4（无鉴权可冒充写）叠加时，
  风险是「任意人可读他人私有记忆 + 可冒充任意 Agent 写入」。B 线修完并开启 `LIMS_REQUIRE_AUTH=1` 前，只在内网使用。
- **token 不进 git**：模板只放 `<YOUR_TOKEN>`；`tokens.example.json` 全是占位符；
  真实 token 表放仓库外（如 `%USERPROFILE%\.lightmem\tokens.json`）。
- **每家一份 token**，不要 8 家共用一个——否则「哪个 Agent 写的」这条信息就没了，共享大脑会退化成一锅粥。
- 本目录不含任何明文凭据，可直接入库。

---

## 10. 本轮交付边界（别把没验证的当已验证）

| 项 | 状态 |
|---|---|
| 8 家配置模板 + 生成器 + 验证手册 | ✅ 已交付，模板 JSON/YAML 结构校验通过，生成器实跑通过 |
| 各 Agent **端到端**连通（工具真出现、真调用） | ⏳ 依赖 A 线 18444 上线；按 `VERIFY.md` 逐家执行 |
| `dsh` 桥通道（stdio + 18443） | ✅ R160 已实测通（7 工具、中文往返、持久化） |
| `codearts.bridge` 通道 | ⚠️ 与 DSH 用**同一桥脚本、同一端点**，但未在 CodeArts 内实测；配置结构已按 CodeArts 的 `mcp`+`command` 数组口径写 |
| stdio 通道的 token 传递 | ⚠️ 冻结接口未定义（见 §5 末），待 B 线确认 |
| 桥支持 `Authorization` 头 | ⚠️ 当前桥不发认证头；若 18443 开启强制鉴权，桥通道会 401（E 线处置） |

逐家验证步骤 → [`VERIFY.md`](./VERIFY.md)
