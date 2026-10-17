# lightmem 在 192.168.0.88 上的 rc.d 自启服务

把 lightmem 从「`/tmp` + `nohup` 开发式部署」升级为**开机自启的系统服务**。

R21 T3 起，本目录提供**两个**服务：

| 服务 | 端口 | 角色 |
|---|---|---|
| `lightmem` | **18443** | 业务 REST（内核） |
| `lightmem_mcp` | **18444** | **原生 MCP**（streamable-http），供各 Agent 客户端接入 |

> `lightmem_mcp` 是 `lightmem` 的**下游消费者**：它把 MCP `tools/call` 转成对
> `127.0.0.1:18443` 的 REST 调用（`REQUIRE: lightmem`）。

## 文件

| 文件 | 说明 |
|---|---|
| `lightmem` | 内核 rc.d 服务脚本（→ `/usr/local/etc/rc.d/lightmem`） |
| `install_lims_rc_d.sh` | 内核安装/升级脚本（幂等，需 root） |
| `lightmem_mcp` | 原生 MCP rc.d 服务脚本（→ `/usr/local/etc/rc.d/lightmem_mcp`） |
| `install_lims_mcp_rc_d.sh` | MCP 安装/升级脚本（幂等，需 root） |

## 安装

```sh
# 1) 先以服务用户编译（**不要用 sudo**：sudo 会重置环境丢掉 Go module cache，本机无外网必然超时）
su -m workbuddy -c 'cd /path/to/lightmem/kernel && go build -o lightmem-kernel ./cmd/lightmem-kernel'
su -m workbuddy -c 'cd /path/to/lightmem/kernel && go build -o lightmem-mcp    ./cmd/lightmem-mcp'

# 2) 安装内核（sudo）
sudo sh install_lims_rc_d.sh --src /path/to/lightmem

# 3) 安装原生 MCP（sudo）；--tokens-file 显式给出才启用鉴权
sudo sh install_lims_mcp_rc_d.sh --src /path/to/lightmem --tokens-file /tmp/agent-tokens.json
```

安装后布局：

```
/usr/local/lightmem/                 安装目录（稳定，非 /tmp）
  ├── lightmem-kernel
  ├── lightmem-mcp
  ├── agent-tokens.json              agent token 表（0600，物主=服务用户）
  ├── profiles/{lims-family,lims-solo}.profile.yaml
  ├── migrations/001_pgvector.up.sql
  ├── VERSION                        来源 commit + 安装时间（内核）
  └── VERSION.mcp                    来源 commit + MCP 端口/上游（MCP）
/usr/local/etc/rc.d/lightmem         rc.d 服务脚本（内核）
/usr/local/etc/rc.d/lightmem_mcp     rc.d 服务脚本（MCP）
/var/log/lightmem.log                内核日志
/var/log/lightmem-mcp.log            MCP 日志
```

`/etc/rc.conf` 会被 `sysrc` 写入（`lightmem_enable="YES"` 等），**开机自动启动**。

## 鉴权与 token（R21 T3）

- **token 表**：`/usr/local/lightmem/agent-tokens.json`（JSON 对象，`0600`）。
  每个 agent 一份 token，形如
  `{"t-dsh-<hex>": {"agent_id":"deepseek-harness","user_id":"dsh-agent","tenant_id":"dshteam"}}`。
- **注入方式**：rc.d 启动时读取该文件并注入 `LIMS_AGENT_TOKENS`。
  **文件不存在 → 不注入 → 透传态（无鉴权）**，便于开发。
- **服务端行为**（`kernel/auth.go`）：
  - 业务端点（`/search`、`/tools/*`、`/audit` …）要求 `Authorization: Bearer <token>`；
  - **运维端点 `/healthz`、`/health`、`/metrics` 豁免鉴权** —— 否则 supervisor 的
    `fetch /healthz` 会 401，**服务将永远无法启动**（T3 实测踩到过，勿删该豁免）；
  - 身份以 token 为准，**忽略请求体里的 `user_id`/`tenant_id`**（防冒充）；
  - MCP 进程经回环调用内核时，用 `X-Lightmem-{User,Tenant,Agent}` 头承载身份，
    **仅信任 `127.0.0.1`/`::1`**。
- **权限**：token 表含凭据，务必保持 `0600`；**不要**提交进 git。

## 日常操作

```sh
sudo service lightmem     start | stop | restart | status
sudo service lightmem_mcp start | stop | restart | status
sysrc -n lightmem_enable       # 看是否开机自启
tail -f /var/log/lightmem.log
```

## 默认参数（可在 `/etc/rc.conf` 覆盖）

| 变量 | 默认 | 说明 |
|---|---|---|
| `lightmem_chdir` | `/usr/local/lightmem` | 安装目录 |
| `lightmem_user` | `workbuddy` | 运行身份 |
| `lightmem_listen` | `:18443` | 监听端口（**避开同机既有 `:8443` 旧实例**） |
| `lightmem_profile` | `<chdir>/profiles/lims-family.profile.yaml` | 档位（默认持久化 pgvector 档） |
| `lightmem_pg_host/port/user/db` | `10.0.10.10` / `5432` / `postgres` / `lims_dsh` | PG 连接（经 `LIMS_PG_*` 注入内核） |
| `lightmem_pg_wait` | `60` | 启动前等待 PG 就绪的秒数 |
| `lightmem_start_wait` | `20` | 启动后等待健康检查通过的秒数 |

## 两个必须知道的坑（已在脚本内注释）

1. **不能把 `/usr/sbin/daemon` 当作 rc.subr 的 `command`。**
   本版 FreeBSD 的 `daemon(8)` 是**前台监管**：它会一直等到子进程退出才返回
   （实测 `daemon -P p -o log /bin/sleep 30` 阻塞 30s）。而 rc.subr 对 `command` 是前台等待，
   于是 `service lightmem start` 会**永久挂住**。
   → 脚本改用自定义 `start_cmd`，显式 `&` 后台化，并等健康检查通过才返回。

2. **自定义 `start_cmd` 不会套用 `${name}_user` 的 `su` 包装。**
   rc.subr 只在 `command` 那条路径上做降权，`*_cmd` 分支不做。
   → 必须显式给 `daemon -u <user>`，否则服务会以 **root** 身份运行。

另外 `daemon -f` 用于先把 stdin/stdout/stderr 指向 `/dev/null` 再按 `-o` 写日志，
避免子进程继承调用方 fd（否则非交互 SSH 会话会因管道被持有而迟迟不返回）。

## 与旧实例的隔离

停止逻辑只用「本服务的二进制路径 + profile 路径」精确匹配，**不会**误杀同机上
`:8443` 的历史实例。安装脚本清理端口时也只针对自己的 `lightmem_listen`。
