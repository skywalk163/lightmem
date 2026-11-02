---
name: 功能请求
about: 为 lightmem 提出新功能建议
title: '[Feature] '
labels: enhancement
assignees: ''
---

## 问题描述

请清晰简洁地描述这个功能请求要解决的问题。

例如：当我用 family 档（PostgreSQL + pgvector）跑多个 Agent 共享记忆时，经常需要 [...]，这很不方便。

## 建议方案

请清晰简洁地描述你希望的解决方案。

```sh
# 可选：展示你期望的命令行 / API 用法示例
# 例如：lightmem-kernel --profile family ...
# 或   ：POST /api/v1/<你希望有的端点>
```

## 替代方案

请描述你考虑过的替代方案或变通方法（现有的档位 profile / REST 接口 / MCP 工具面 / SDK 能不能凑出来？）。

## 适用场景

这个功能在什么场景下最有用？

- [ ] 档位：Solo（SQLite，单机零外部依赖）
- [ ] 档位：Family（PostgreSQL + pgvector）
- [ ] 档位：SMB / Enterprise
- [ ] 接入面：REST API
- [ ] 接入面：MCP（stdio 或 HTTP）
- [ ] 接入面：客户端 SDK（Python / JS）/ DSH 桥
- [ ] 记忆模型：fact / preference / goal / event / procedure
- [ ] 检索与可见性（tenant / viewer / permission 三元组）
- [ ] 部署与运维（install.sh / 看门狗 / 日志轮转 / GDPR 导出）
- [ ] 其他：______

## 影响范围

- [ ] kernel 存储后端（SQLite / PostgreSQL / Qdrant）
- [ ] REST 处理器与路由
- [ ] MCP 工具面（`mcp_tools.go` / `mcp_tools_workflow.go`）
- [ ] 鉴权与 token（`LIMS_AGENT_TOKENS` / profile `auth` 段）
- [ ] 档位与 profile（`profiles/*.profile.yaml` 与 schema）
- [ ] 迁移与向后兼容
- [ ] 文档
- [ ] 其他：______

## 隐私提醒

> 提需求时**不要把「记忆正文」贴进来**（可能含真实个人或客户信息）；用脱敏占位，或只描述字段与结构。
> token 明文同样不要贴。

## 附加上下文

请提供任何其他有助于理解该功能请求的上下文、截图或参考资料。