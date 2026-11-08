# 安全策略（lightmem）

## 报告漏洞

**不要开 public issue。** 请在 Gitea 主仓提 **private issue**，或按 `git log`
里的维护者联系方式私下联系。

> TODO：维护者安全联系邮箱尚未在仓库中公开，本文件不编造地址；
> 待业主给出后补上。在此之前以 Gitea private issue 为首选通道。

## 支持范围

本仓当前版本 tag 仅 `v0.1.0`（`git tag` 另有 `r13-premerge-safety` 等非版本标签），
不提供多版本安全维护承诺；安全修复只跟随主分支发布。
不承诺自动响应时限，跟进在 Gitea private issue 中由业主处理。

## 已知的设计边界（不是漏洞）

以下为有意为之，报告前请先确认：

- **Solo 档没有 jail / MAC / IPsec / KMS**，也没有多租户 / 跨 Agent 共享
  （README「⚠️ 能力边界（R25-D 如实化）」已声明；完整档位见 `docs/档位能力矩阵.md`）。
- 默认监听回环；**非回环监听且 token 表为空时拒绝启动**（启动期 fail-closed 护栏，
  见 `kernel/auth.go` 的 `IsLoopbackAddr` 与该文件注释、commit `30ff8e4`）。
  ⚠️ 注意：该护栏**当前只在 gitea 侧，GitHub 镜像缺失**，详见
  [`docs/远端分叉核验_20261106.md`](docs/远端分叉核验_20261106.md)（T-0264 只读核验）。
- **agent token 默认透传**：未配置 `LIMS_AGENT_TOKENS` 时 `AuthMiddleware` 直接放行
  并打一行 warn 日志（`kernel/auth.go`，红线④零回归设计）；已配置时缺/错 token 返回 401。
  这是向后兼容的默认值，不是鉴权缺失。

## 凭据

`.gitignore` 已覆盖 `agent-tokens.json`、`tokens.csv`、`clients/*-token.json`、
`*-token.txt`、`.lightmem-secrets/`、`_lightmem_run/`、`data/`。
**提交前请 `git status` 确认没有这些文件**；也不要在 issue / PR / 文档里贴
token、密钥或真实内网地址（文档统一写 `<门禁机>`）。
