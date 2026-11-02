---
name: Bug 报告
about: 提交 Bug 报告帮助改进 lightmem
title: '[Bug] '
labels: bug
assignees: ''
---

## 描述

请清晰简洁地描述这个 Bug。

## 复现步骤

> 五条必填：**平台 / Go 版本 / 档位 / 复现命令 / 完整报错与日志**。缺任何一条，排查成本都会翻倍。

1. 环境准备（在工作目录里，例如 `~/dswork` 或 `G:\dswork`）：

   ```sh
   git clone <lightmem 仓地址> lightmem
   cd lightmem
   # 默认 solo 档：SQLite 单文件，零外部依赖（无需 PG / Docker / Qdrant）
   sh integrations/quickstart/install.sh
   ```

2. 执行命令（REST 默认 `http://127.0.0.1:7443`，MCP 默认 `http://127.0.0.1:7444/mcp`；
   Windows 把路径分隔符换成 `\`，参数不变）：

   ```sh
   # 粘贴你实际执行的那一条：curl /healthz、go test ./kernel/...、某个 MCP 工具调用……
   ```

3. 看到报错（**完整粘贴**，不要只贴最后一行）：

   ```
   粘贴完整报错：stdout + stderr；带 panic / traceback 的请连首行一起贴
   ```

## 期望行为

请清晰简洁地描述你期望发生什么。

## 实际行为

请描述实际发生了什么。

## 环境信息

- 操作系统： [例如 FreeBSD 15.1 / Ubuntu 24.04 / macOS 14 / Windows 10]
- Go 版本： [运行 `go version` 的输出，例如 go1.25.14]
- lightmem 版本 / 提交： [二进制 `-version` 的输出，或 `git rev-parse --short HEAD`]
- 档位（profile）： [solo / family / smb / enterprise；solo = SQLite 单文件，family = PostgreSQL + pgvector]
- 端口： [REST / MCP 实际监听端口，例如 7443 / 7444]
- 是否配了 `LIMS_AGENT_TOKENS`： [已配（JSON 串 / 指向的文件路径）/ 未配（透传态，启动日志里会有一行 warn）]
- 存储后端： [SQLite（给文件路径）/ PostgreSQL（给 PG 版本 + pgvector 版本 + 连接方式）]
- 部署形态： [install.sh 一键部署 / systemd / FreeBSD rc.d（`integrations/lims-088-rc.d/`）/ 裸二进制]

## 完整日志

- 服务端日志： `_lightmem_run/lightmem.log`（`install.sh` 的运行目录）尾部若干行，**整段粘贴**。
- 健康检查： `curl -sS http://127.0.0.1:<REST 端口>/healthz` 的原样输出。
- MCP 侧： 你接入的那个 AI 工具的 MCP 日志（工具不出现 / 调用报错时必贴）。

## 隐私提醒

> **不要把「记忆正文」贴进 issue**。`fact` 等字段里可能是真实个人或客户信息（含隐私）。
> 需要说明数据形态时，用脱敏占位（如把「客户A偏好周一开会」写成「<某人>偏好<时间>」），
> 或只描述结构与字段名。
> 同理：**不要粘贴 token 明文**（`LIMS_AGENT_TOKENS` 的取值、签发出来的 agent token 都算）。

## 附加上下文

- 涉及的任务卡 / 结果：`tasks/T-xxxx.md`、`results/T-xxxx.json` 的关键字段
- 截图（如果有）
- 该问题是否在特定条件下才出现？（只在 Windows / 只在 family 档 / 只在配了 token 后 / 只在并发写入时……）