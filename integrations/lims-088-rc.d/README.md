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
- **`--tokens-file` 用独立副本**：不要把该参数指向 `$PREFIX/agent-tokens.json` 本身，
  否则 `install` 会报 `are the same file`。用 `/tmp/tokens-src.json` 之类的副本作为源。

### ⚠️ 两条部署纪律（T3 实测踩过）

1. **任何要上机生效的部署脚本，必须先提交再上机。**
   T3 曾出现：rc.d 的 token 注入改动**未提交**就上机，随后为刷新版本记录执行
   `git checkout -f origin/main` → **工作树修改被丢弃** → 重装用了无注入的旧脚本 →
   **18443 鉴权静默失效**（服务正常、健康检查通过、token 表在盘上，但不再被读取）。
2. **部署后必须复验「鉴权矩阵」，不能只看健康检查与 `service status`。**
   最低限度四项：

   ```sh
   curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18443/healthz    # 期望 200（豁免）
   curl -s -o /dev/null -w '%{http_code}\n' \
        'http://127.0.0.1:18443/search?q=x&tenant_id=t'                        # 期望 401
   curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $TOK" \
        'http://127.0.0.1:18443/search?q=x&tenant_id=dshteam'                  # 期望 200
   curl -s -o /dev/null -w '%{http_code}\n' -X POST http://127.0.0.1:18444/mcp \
        -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'  # 期望 401
   ```

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
| `lightmem_pg_host/port/user/db` | `10.0.10.10` / `5432` / `lightmem_app` / `lims_dsh` | PG 连接（经 `LIMS_PG_*` 注入内核） |
| `lightmem_pg_wait` | `60` | 启动前等待 PG 就绪的秒数 |
| `lightmem_start_wait` | `20` | 启动后等待健康检查通过的秒数 |
| `lightmem_restart_delay` | `5` | **R29-A**：崩溃后重启延迟秒数（`daemon -R`，1–31536000） |
| `lightmem_restart_count` | `24` | **R29-A**：最多重启次数（`daemon -C`，上限 128）；用尽后 `daemon` 自行退出 → 服务真下线，**防无限刷屏** |

MCP 服务对应 `lightmem_mcp_restart_delay` / `lightmem_mcp_restart_count`（默认同 5/24）。

## 守护与自愈（R29-A，永续运行 L1）

`service lightmem start` 实际拉起的是：

```sh
/usr/sbin/daemon -f -H -R 5 -C 24 -u workbuddy -P /var/run/lightmem/lightmem.pid \
    -o /var/log/lightmem.log env HOME=… lightmem-kernel -profile … -listen :18443 -embedder hash
```

| 参数 | 语义（0.88 `man daemon` 实测） |
|---|---|
| `-R 5` | 子进程终止后延迟 5s 重启——把崩溃风暴压成「每 5s 最多一次」的有界节奏 |
| `-C 24` | 最多重启 24 次；用尽后 `daemon` 自身退出 → **服务真下线**（等人看日志），而非满速刷屏 |
| `-H` | 收到 SIGHUP 时重开 `-o` 日志文件——配合 `deploy/newsyslog.d/lightmem.conf`（T0 P0-A）做日志轮转，避免长期运行撑满盘（L-257） |
| `-P` | pidfile 记录 **supervisor** pid（停止目标；勿用 `-p` 的子进程 pid） |

三层守护模型：**init**（rc.d 开机自启 + 等 PG）→ **supervisor**（`daemon -R/-C` 自愈 + 防抖）
→ **watchdog**（`deploy/` 三件套，T0 P0-A 交付：每 2 分钟探 `/readyz`，失败带 120s 冷却地
`service lightmem restart`——`-C` 用尽后的最后兜底 + 告警）。

### 重启自证清单（R21；升级 rc.d / 换二进制后必跑）

按顺序执行，每步的期望都写在括号里：

```sh
# ① stop 后不再被拉起（验证新 stop 顺序 + -R/-C 组合：stop = TERM 监督者，不会被重启）
sudo service lightmem stop; sleep 20; sudo service lightmem status   # 期望：NOT running，20s 后仍 NOT running

# ② kill -9 后自动恢复（supervisor -R 5 拉起）
sudo service lightmem status                                        # 先记下 kernel pid
kill -9 $(pgrep -f lightmem-kernel); sleep 15; sudo service lightmem status
                                                                     # 期望：RUNNING + 新 pid；healthz 200

# ③ 端口唯一（无孤儿/重复实例）
sockstat -4l | grep 18443                                          # 期望：恰好 1 行

# ④ 鉴权矩阵四项（见上一节）                                          # 期望：200 / 401 / 200 / 401

# ⑤ 条数不丢（恢复后数据仍在）
curl -s -H "Authorization: Bearer $TOK" 'http://127.0.0.1:18443/search?q=x&tenant_id=dshteam'  # 与 kill 前一致
```

> R29-A 在 0.88 的逐项实测记录见 `_R29_A线交付报告.md`。

## 两个必须知道的坑（已在脚本内注释）

1. **不能把 `/usr/sbin/daemon` 当作 rc.subr 的 `command`。**
   本版 FreeBSD 的 `daemon(8)` 是**前台监管**：它会一直等到子进程退出才返回
   （实测 `daemon -P p -o log /bin/sleep 30` 阻塞 30s）。而 rc.subr 对 `command` 是前台等待，
   于是 `service lightmem start` 会**永久挂住**。
   → 脚本改用自定义 `start_cmd`，显式 `&` 后台化，并等健康检查通过才返回。

2. **自定义 `start_cmd` 不会套用 `${name}_user` 的 `su` 包装。**
   rc.subr 只在 `command` 那条路径上做降权，`*_cmd` 分支不做。
   → 必须显式给 `daemon -u <user>`，否则服务会以 **root** 身份运行。

3. **stop 必须「先 TERM 监督者、再兜底 pkill」（R29-A 修正）。**
   在 `daemon -R/-C` 监督语义下，先 `pkill` 子进程会被监督者**立刻拉起**新子进程，
   与随后的 TERM 竞态，可能留下新实例占用端口（下次 `start` 报 `EADDRINUSE`）。
   daemon(8) 手册：SIGTERM 发给**监督者**（`-P` 记录的就是它）时会转发给子进程，
   子进程退出、移除 pidfile、监督者随后终止——一步到位。
   ⚠️ 同理：**不要**把 `-p`（子进程 pidfile）当停止目标，手册明确警告
   「signal the child → daemon restarts it」。

另外 `daemon -f` 用于先把 stdin/stdout/stderr 指向 `/dev/null` 再按 `-o` 写日志，
避免子进程继承调用方 fd（否则非交互 SSH 会话会因管道被持有而迟迟不返回）。

## 与旧实例的隔离

停止逻辑只用「本服务的二进制路径 + profile 路径」精确匹配，**不会**误杀同机上
`:8443` 的历史实例。安装脚本清理端口时也只针对自己的 `lightmem_listen`。
