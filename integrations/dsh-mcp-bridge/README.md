# lightmem × DeepSeek Harness MCP 桥

把 lightmem 的 REST API 暴露成 **MCP tools**，让 DeepSeek Harness（DSH）能像调用原生工具一样
读写 lightmem 记忆。

```
DSH (dsh-mcp-client, stdio)  ──JSON-RPC over stdio──▶  本桥  ──HTTP/REST──▶  lightmem
```

## 为什么需要这座桥

- **DSH 侧**：与外部记忆系统对接的唯一通道是 MCP（`stdio` 或 `streamable-http`）。
- **lightmem 侧**：只提供 REST，且不实现 MCP。
- 两边协议不通，故需要转换层。

桥刻意做成 **零第三方依赖**（只用 Python 标准库）：

1. DSH 的 stdio transport 会清洗掉名字匹配 `KEY|PASSWORD|SECRET|TOKEN` 的环境变量，
   并继承其余环境；在子进程里再装依赖既脆弱又慢。
2. 无需 `pip install`，任何 Python 3.9+ 直接可跑。

## 文件

| 文件 | 说明 |
|---|---|
| `lightmem_mcp_bridge.py` | 桥本体（MCP server，stdio） |
| `deploy_lims_088.sh` | 在 192.168.0.88 上编译/迁移/启动 lightmem |

## 快速使用

### 1) 在 0.88 上把 lightmem 跑起来

```sh
sh deploy_lims_088.sh start     # 默认持久化档（pgvector）
sh deploy_lims_088.sh status
curl http://192.168.0.88:18443/healthz
```

默认用 **pgvector 档**，因为 Solo/SQLite 后端目前是**内存骨架**（重启即丢数据，
详见 `_R160_DSH_lightmem_联调问题记录.md` I-15）。
如只想快速试跑（数据不持久）：

```sh
LIMS_MODE=solo sh deploy_lims_088.sh start
```

### 2) 让 DSH 接上桥

在 DSH 的 profile 补丁里追加（**profile 级文件，务必追加而非覆盖**）：

- 桌面版：`%USERPROFILE%\.dsh\profiles\desktop\cordis.patch.yml`
- 全局：`%USERPROFILE%\.dsh\cordis.patch.yml`

```yaml
- insert:
    - id: mcp-lightmem
      name: '@deepseek-ai/dsh-mcp-client'
      config:
        serverName: lightmem
        transport: stdio
        command: 'C:\Python314\python.exe'
        args:
          - 'G:\dswork\duan-light-merge\lightmem\integrations\dsh-mcp-bridge\lightmem_mcp_bridge.py'
        cwd: 'G:\dswork\duan-light-merge\lightmem'
        env:
          LIGHTMEM_BASE_URL: 'http://192.168.0.88:18443'
          LIGHTMEM_TENANT_ID: 'dsh'
          LIGHTMEM_USER_ID: 'dsh-agent'
        failOnStartupError: false
```

保存后 DSH 的 HMR 会**热重载**该插件（无需重启 Harness），
随后工具以 `mcp__lightmem__*` 出现。

> 其它 `cordis.patch.yml` 已有内容不要动；该文件同时承载模型、UI 等其它用户配置。

### 3) 验证

```powershell
# 用 DSH 内置的同版本 MCP SDK 独立驱动桥，绕开 GUI 排查问题
node e2e-all.mjs "C:\Python314\python.exe" "...\lightmem_mcp_bridge.py"   # → ALL_PASS
```

或在 DSH 会话里直接调 `mcp__lightmem__lightmem_health`。

## 暴露的工具

| 工具 | 说明 | 对应 REST |
|---|---|---|
| `lightmem_save` | 写一条记忆（只增不改） | `POST /tools/memory/save` |
| `lightmem_recall` | 跨层召回 + 8 路信号融合（**首选检索**） | `POST /tools/memory/recall` |
| `lightmem_search` | 轻量检索（桥同时发 `q` 与 `text`，见下方「参数名坑」） | `GET /search` |
| `lightmem_update` | 更正（追加新版本，旧版标记被取代） | `POST /tools/memory/update` |
| `lightmem_feedback` | adopted / rejected / edited 反馈 | `POST /tools/memory/feedback` |
| `lightmem_forget` | 软删除（置历史 + 标记取代者） | `POST /tools/memory/forget` |
| `lightmem_health` | 健康检查，排障用 | `GET /healthz` |

## 配置项（环境变量）

| 变量 | 默认 | 说明 |
|---|---|---|
| `LIGHTMEM_BASE_URL` | `http://192.168.0.88:18443` | lightmem 地址 |
| `LIGHTMEM_TENANT_ID` | `dsh` | 默认租户 |
| `LIGHTMEM_USER_ID` | `dsh-agent` | 默认用户（同时作为可见性 viewer） |
| `LIGHTMEM_AGENT_ID` | `deepseek-harness` | 默认 Agent 标识 |
| `LIGHTMEM_TIMEOUT` | `15` | 单次 HTTP 超时（秒） |
| `LIGHTMEM_LOG` | 空 | 日志文件路径（追加写；stderr 始终输出） |

> 变量名刻意**不含** `KEY`/`PASSWORD`/`SECRET`/`TOKEN` 字样，
> 否则会被 DSH 的 stdio 环境清洗规则丢弃。

## 排障

| 现象 | 原因与处置 |
|---|---|
| DSH 里看不到 `mcp__lightmem__*` | 桥进程没起来。看 `%TEMP%` 或 `LIGHTMEM_LOG`；确认 `python` 路径与脚本路径正确 |
| **`lightmem_search` 明明传了词却返回全部记忆（分数还都一样）** | **参数名坑（上游 I-12）**：服务端 `kernel/router.go:154` 只读 `q`，而 README 写的是 `text`。桥已同时发 `q`+`text` 规避；若你自建客户端，请用 `q=` |
| 写中文报 `surrogates not allowed` | stdio 编码不是 UTF-8。本桥已强制 `reconfigure(encoding="utf-8")`；若你改造过桥，别删那段 |
| `404 memory ... not visible to /<tenant>` | 缺 viewer。`feedback`/`forget` 必须带 `user_id`（本桥已自动带） |
| `relation "memories" does not exist` | PG 未迁移。跑 `deploy_lims_088.sh`（内建幂等迁移）或手工应用 `kernel/migrations/001_pgvector.up.sql` |
| 重启后记忆全丢 | 你用的是 Solo/SQLite 档（内存骨架）。改用默认的 pgvector 档 |
| 桥日志显示未实现的 method | 正常。DSH 先探测 `server/discover`，桥回 `-32601` 后客户端按规范回退到 legacy era |
| 连接后工具调用全失败 | 确认 lightmem 在跑：`curl $LIGHTMEM_BASE_URL/healthz` |

## 已知限制

- **仅实现 MCP 的 tools 能力**；未实现 prompts / resources / tasks。
- `server/discover`（现代 era 探测）未实现，依赖客户端回退到 legacy `2025-11-25`。
  实测 DSH 会正常回退，但不排除其它客户端要求现代 era。
- 不做连接池与重试；单次调用失败即如实上报（重试策略由 DSH / 模型决定）。
- 桥是无状态转发，**记忆的持久化完全取决于 lightmem 的后端档位**。

## 后续建议

把该桥收编为 lightmem 官方组件（例如 Go 写的 `cmd/lightmem-mcp`，直接复用内核），
让 lightmem 原生支持 MCP，即可去掉这个外部 Python 进程与协议转换层。
