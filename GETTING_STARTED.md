# 5 分钟接入 lightmem —— 让你的所有 AI 共用一个大脑

> **你机器的每个 AI，都能记住你说过的话。**
> 换一个 AI 开工，不用再把背景交代一遍——它自己去记忆里查。

---

## 一、这是什么？为什么你需要它

你电脑上跑着好几个 AI：DeepSeek Harness、Trae、CodeArts、WorkBuddy、小浣熊……
它们**各自失忆**。你在 Trae 里定下的架构决策，换到 DSH 开工，它一无所知，
你得从头讲一遍。

**lightmem 解决的就是这件事**：它是一个**独立运行的记忆服务**，所有 AI 都连到同一个
它。一个 AI 沉淀的经验，另一个 AI 直接复用。

```
你的机器
  ├── DeepSeek Harness ─┐
  ├── Trae ─────────────┤
  ├── CodeArts ─────────┼──▶ lightmem（记忆大脑）
  ├── WorkBuddy ────────┤     原生 MCP：Linux/quickstart 7444 或 FreeBSD rc.d 18444
  ├── 小浣熊 ───────────┤
  └── OpenCode / Roo ───┘
```

它不只是"存聊天记录"，而是**结构化的长期记忆**：

| 能力 | 说明 |
|---|---|
| **四层温度分层** | L0 工作记忆（分钟）→ L1 动态（天~月）→ L2 静态（长期）→ L3 归档（合规） |
| **8 路信号融合检索** | 语义 + 关键词 + 实体 + 时效 + 来源可信度…… 不是简单关键词匹配 |
| **工作计划/步骤/总结** | 一等公民：能建计划、推进度、记步骤、写总结 |
| **跨 Agent 共享** | `tenant` 级记忆全员可见；`private` 只有自己能看 |
| **防冒充** | 每个 AI 一份 token，身份由服务端认定，请求体里谎报也没用 |
| **安全合规** | 投毒防护、租户硬过滤、GDPR 三件套 |

**而且它很轻**：个人档用 SQLite，**零外部依赖**，不需要装 PostgreSQL、不需要 Docker。

---

## 二、开始：一条命令

### 前提

只需要 **Go 1.25+**。没有？去 <https://go.dev/dl/>（macOS：`brew install go`）。
Python3 可选（用于 token 签发与配置生成，系统通常自带）。

### 部署

```bash
git clone <你的仓库地址> lightmem    # 或用 gitea：http://192.168.1.5:3000/skywalk/lightmem.git
cd lightmem
sh integrations/quickstart/install.sh
```

就这样。它会：

1. 检查 Go；
2. 编译两个二进制（REST + MCP 服务）；
3. 生成 profile（**默认 solo 档 = SQLite，零外部依赖**）；
4. **签发 8 份 agent token**；
5. 启动服务并做健康检查；
6. 生成**客户端接入包**（含 `MANIFEST.md` 与给 AI 的 `agent-prompt.md`）。

跑完你会看到：

```
  REST      : http://127.0.0.1:7443
  MCP       : http://127.0.0.1:7444/mcp    ← 给 AI 接的就是这个
```

> 想改端口/租户？`install.sh --port 8443 --tenant myteam --user alice`
> 想先看看它要做什么？`install.sh --dry-run`
> 更多档位与 systemd：`install.sh -h`
>
> **Linux 快速路径（主流发行版，一等平台）**：在 Ubuntu 24.04 / Debian 12 / Rocky 9 上，
> 除上面的一条命令外，加 `--systemd` 即可装成两个 systemd 服务（kernel `:7443` + MCP `:7444`）：
>
> ```bash
> sudo sh integrations/quickstart/install.sh --profile solo --systemd --service-user lightmem
> # 等价的发行版前置（Go 1.25 / PG+pgvector / 用户与目录 / 防火墙 / 排障）见：
> # manuals/linux-setup.md  ← TODO：该文件随 D 线 T-0414 合入；未合入前 Linux profile 以 rc.d/quickstart 默认档为准
> ```
>
> **FreeBSD 路径保持原样**：走 rc.d / jail / ZFS（端口 `:18443/:18444`），
> `install.sh` 在 FreeBSD 上会跳过 systemd、不碰任何 rc.d 文件。
> 两套端口口径（`7443/7444` ↔ `18443/18444`）不得混改。

---

## 三、把 AI 接进来（三种方式，挑一种）

### 方式 ① —— 最省事：把一段 prompt 粘给你的 AI

把这句发给**任何有文件读写权限的 AI**（DSH、Trae、CodeArts 都行）：

```
请阅读 ./_lightmem_run/clients/agent-prompt.md 并照它执行，
把 lightmem 的 MCP 配置装到本机各 AI 工具里。
```

它会自己备份、写入配置、告诉你哪些需要重启。**你什么都不用填。**

### 方式 ② —— 手工：照 MANIFEST 逐家粘贴

打开 `./_lightmem_run/clients/MANIFEST.md`，里面逐家写了：
**粘到哪个文件、合并还是新建、要不要重启**。配置片段已经生成好，复制即可。

### 方式 ③ —— 只给一个 AI 接（比如只给 Trae）

```bash
# 1) 签发这个 AI 的 token
python3 integrations/agent-onboarding/issue_token.py --agent trae --tenant myteam --user alice --out ./mytokens

# 2) 生成它的配置片段
python3 integrations/agent-onboarding/gen_configs.py \
  --server http://127.0.0.1:7444 --tokens ./mytokens/agent-tokens.json \
  --agents trae --out ./myout

# 3) 把 ./myout/trae.mcp.json 的内容粘到 Trae 的配置路径
```

---

## 四、装完后：立刻能用

在任意一个已接入的 AI 里说：

> **调用 lightmem_health，把返回原样贴给我。**

看到 `{"status":"ok"}` 就成了。

然后开始真正用它：

```
用 lightmem_save 记住这件事：本项目用的是 Go 1.25 + pgvector，部署在 192.168.0.88。
```

```
先用 lightmem_recall 查一下我们之前关于「鉴权方案」的结论，再动手改。
```

```
用 lightmem_goal_create 建一个计划：把 lightmem 接入全部 AI Agent，deadline 2026-10-25
```

**最后那句「动手前先回忆」，就是共享记忆大脑的日常用法。**

### 给 AI 的一段"长期习惯"（推荐粘到你的 AI 系统提示里）

```
你在动手前，先用 lightmem_recall 查一下相关历史结论与偏好。
完成后，用 lightmem_save / lightmem_step_add 把这次的关键决策与步骤记下来。
跨 AI 协作时，共享信息用 visibility=tenant，私人草稿用 visibility=private。
```

---

## 五、三家最容易踩的坑（**写错不报错，只是不生效**）

这是本工具存在的最大理由——同一件事，不同 AI 要求不同的字段名：

| AI | HTTP 类型标记 | 顶层键 |
|---|---|---|
| DeepSeek Harness | `transport: streamable-http`（**连字符**） | `- insert:` 补丁条目 |
| WorkBuddy | `type: "streamableHttp"`（**驼峰**） | `mcpServers` |
| 小浣熊 / Box-Agent | `type: "streamable_http"`（**下划线**） | `mcpServers` |
| Trae / SOLO | 无类型标记，用 `url` | `mcpServers` |
| OpenCode / DuMate | `type: "remote"` | `mcp`（**不是** mcpServers） |
| CodeArts | `type: "local"`，`command` 为**数组** | `mcp` |
| Roo Code / Cline | `type: "stdio"`，`command` 为**字符串** + `args` | `mcpServers` |

> **症状**：配置写了、重启了，但工具列表里就是没有 lightmem。
> **99% 是这一栏写错**——它不会报任何错，只是静默失效。
> 用 `gen_configs.py` 生成就不会错，它把写法固化好了。

**另外两个常见现象**：

| 现象 | 说明 |
|---|---|
| Trae 日志里 `MCPOauthService ... No token data found` | Trae 在找 **OAuth** 令牌，与我们用的 `Authorization: Bearer` **无关**，是它的常规探测，不是错误 |
| 首次调用弹授权框 | 正常。Trae / WorkBuddy 首次调 MCP 工具要你点一次"允许" |

---

## 六、如果换发/补签 token：**必须重启服务**

服务端**只在启动时装配 token 表**。你后补签的 token，不重启不会被认——
表现为新 token 报 `401 unauthorized`。

```bash
# 按你的启动方式重启 MCP 服务：
# Linux（systemd；install.sh --systemd 装的双 unit）：
sudo systemctl restart lightmem-mcp lightmem

# FreeBSD（rc.d，原有口径不变）：
sudo service lightmem_mcp restart

# 手工方式（两平台通用）：
kill $(cat ./_lightmem_run/lightmem-mcp.pid)
# 然后重跑 install.sh，或按它末尾给的命令重启
```

---

## 六·五、守护与自愈（FreeBSD 与 Linux 各自口径）

> 这一节讲服务「挂了怎么自己起来」。FreeBSD 面向部署在 `192.168.1.5`（FreeBSD 0.88）的 family 档；
> Linux 面向主流发行版（Ubuntu 24.04 / Debian 12 / Rocky 9）的 systemd 部署，两者均为一等平台，机制按平台分列，不互相替换。

### 两层自愈

| 层 | FreeBSD（rc.d，原义保留） | Linux（systemd） |
|---|---|---|
| 进程级 | rc.d `daemon -R 5 -C 24 -H`：内核崩溃/退出 → 5s 拉起；24 次用尽后 daemon 退出，看门狗兜底 | `deploy/lightmem.service` / `lightmem-mcp.service`（T-0411 已落地）：`Restart=on-failure`、`RestartSec=2`、`StartLimitBurst=5`/60s 防重启风暴，`systemctl` 托管 |
| 应用级 | `deploy/lightmem-watchdog.sh`（cron 每 2 分钟）：进程在但 `/readyz` 503 → `service lightmem restart`，120s 冷却防抖 | 同一看门狗脚本经 `deploy/lightmem-watchdog.timer`（systemd timer）周期触发；TODO：timer 随 B 线 T-0412 合入，未合入前 Linux 侧沿用 cron 形态 |

两者**互补不替代**：进程级只管进程退出，看门狗补「进程在但后端连不上」的盲区。
重启命令按平台分：FreeBSD `sudo service lightmem restart`；Linux `sudo systemctl restart lightmem lightmem-mcp`。

### 探针端点（免鉴权）

- `/livez`：存活探针，立即 200，不碰任何依赖。
- `/readyz`：就绪探针，`Ping` 后端，200=ready / 503=not_ready。

> 这两个端点已加入 `kernel/auth.go` 的运维豁免清单，否则 FreeBSD rc.d 健康检查与 Linux systemd/监控探针会吃到 401、服务永远起不来。

### 安装看门狗 + 日志轮转

FreeBSD（rc.d / cron / newsyslog，原义保留）：

```sh
install -m 555 deploy/lightmem-watchdog.sh /usr/local/bin/lightmem-watchdog.sh
install -m 644 deploy/cron.d/lightmem-watchdog /etc/cron.d/lightmem-watchdog
install -m 644 deploy/newsyslog.d/lightmem.conf /etc/newsyslog.d/lightmem.conf
```

Linux（systemd / logrotate）：日志轮转配置路径 `/etc/logrotate.d/lightmem`（由 `deploy/platform.sh` 的
`lm_logrotate_install` 口径安装；`deploy/logrotate.d/lightmem` 配置文件随 C 线 T-0413 合入，
未合入前 TODO 先手工配 logrotate）。看门狗 timer 随 B 线 T-0412 合入，未合入前用上面的 cron 形态。
安装前缀：Linux `/opt/lightmem`、FreeBSD `/usr/local/lightmem`（`lm_prefix()` 口径，勿混用）。

### 重启自证四步（验证自愈真的生效）

1. `sudo service lightmem status` —— 应 `lightmem is running as pid NNNN`。
2. 模拟崩溃：`sudo kill -9 $(pgrep -f 'lightmem-kernel -profile')`；等 10 秒。
3. `sudo service lightmem status` —— 应换了一个新 pid（**自愈成功**）；`curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:18443/healthz` 应为 200。
4. 模拟不可自愈故障：临时改 `lightmem_listen=":99999"` → `service lightmem restart` 必失败；约 24 次重启后 daemon 自行退出、服务稳定 NOT running（**有界、不刷屏**）；改回 `:18443` 再 `service lightmem start` 即恢复。

> 说明：family 档默认走 pgvector 持久化；solo/SQLite 档当前是内存骨架，重启即丢数据，不可用于真实记忆（详见历史问题记录）。

---

## 七、两种档位

| | **solo**（默认） | **family** |
|---|---|---|
| 存储 | **SQLite**（单文件，零依赖） | PostgreSQL + pgvector |
| 适合 | 个人开发、先跑起来看看 | 团队/多用户、十万级记忆、向量检索 |
| 外部依赖 | **无** | 需要 PG |

```bash
sh integrations/quickstart/install.sh --profile family   # 切到 family
```

> 向量能力：solo 档的向量检索暂降级为文本（调度层自动兜底，不影响使用）。
> 需要语义向量检索请上 family 档。

---

## 八、安全须知（请务必看）

- `agent-tokens.json`、`clients/*.json`、`tokens.csv` **含明文凭据**：
  - 文件权限已设为 `600`；
  - **不要提交进 git**；建议把 `_lightmem_run/` 加进 `.gitignore`；
  - **生产环境请换发正式 token**（`issue_token.py` 现签），别沿用示例/测试值。
- 服务默认只监听 `127.0.0.1`（本机）。**要让局域网其它机器访问**，需显式
  `--bind 0.0.0.0`——届时请务必确认 token 已启用并且网络可信。
- `stdio` 类客户端（CodeArts / Roo）**每台机器各起一个进程、只带自己那一条 token**。
  带整表会被服务端拒绝（多身份无法区分，宁可无身份也不猜测，避免把 A 的身份安到 B 头上）。

---

## 九、排障速查

| 症状 | 排查 |
|---|---|
| `go: command not found` | 装 Go 1.25+：<https://go.dev/dl/> |
| 工具不出现 | 查 §五 的类型标记；再看该 AI 的 MCP 日志 |
| `401 unauthorized` | token 不对，或换发后**没重启服务**（§六） |
| `bind: address already in use` | 端口被占：换 `--port`，或先停旧进程。查占用：Linux `ss -ltnp \| grep 7444`（rc.d 档查 `18444`）；FreeBSD `sockstat -4l \| grep 18444` |
| `400 tenant_id required` | 老版本症状（身份没注入）；升级到本版 |
| 服务起了但 `healthz` 不通 | 看 `_lightmem_run/lightmem.log` 尾部；systemd 装法另看 `journalctl -u lightmem -u lightmem-mcp -n 100 --no-pager` |
| Linux 服务起不来/反复重启 | `systemctl status lightmem lightmem-mcp`、`journalctl -u lightmem-mcp -f`；确认日志轮转 `logrotate -d /etc/logrotate.d/lightmem`（FreeBSD 用 `newsyslog` 口径，见上） |
| FreeBSD 服务起不来 | `sudo service lightmem status`、`sockstat -4l \| grep 18443`；排障口径与 rc.d 保持一致，不套用 Linux 命令 |

> 命令对照（各平台用各自口径，不互换）：
>
> | 用途 | Linux | FreeBSD |
> |---|---|---|
> | 查端口占用 | `ss -ltnp` | `sockstat -4l` |
> | 查服务日志 | `journalctl -u lightmem*` | `cat /var/log/lightmem*.log`（newsyslog 轮转） |
> | 日志轮转 | `logrotate`（`/etc/logrotate.d/lightmem`） | `newsyslog`（`/etc/newsyslog.d/lightmem.conf`） |
> | 服务管理 | `systemctl restart/status lightmem*` | `service lightmem* restart/status` |
> | 安装前缀 | `/opt/lightmem` | `/usr/local/lightmem` |
> | 端口口径 | 7443/7444 | 18443/18444 |

---

## 十、文件索引

| 路径 | 用途 |
|---|---|
| `integrations/quickstart/install.sh` | **一键部署**（这条开始）；Linux 加 `--systemd`（双 unit），FreeBSD 自动跳过 |
| `deploy/platform.sh` | 平台抽象接口（`lm_detect_os` / `lm_prefix` / `lm_probe_port` / `lm_logrotate_install` 等，Linux 与 FreeBSD 各自口径） |
| `manuals/linux-setup.md` | Linux 部署手册（Ubuntu/Debian/Rocky）｜ TODO：随 D 线 T-0414 合入 |
| `manuals/p1-freebsd-zfs-setup.md` | FreeBSD + ZFS + jail 部署手册（原义保留） |
| `integrations/agent-onboarding/issue_token.py` | 签发 agent token |
| `integrations/agent-onboarding/gen_configs.py` | 生成各家 MCP 配置片段 |
| `integrations/agent-onboarding/README.md` | 接入原理与逐家细节 |
| `integrations/agent-onboarding/VERIFY.md` | 逐家验证清单 |
| `integrations/agent-onboarding/configs/` | 配置模板 |
| `_lightmem_run/` | **你跑完 install.sh 后的运行目录**（含 token、数据、日志、接入包） |
| `lightmem.md` | 完整架构设计 |
| `AGENTS.md` | 给 AI Agent 看的入口 |

---

> **一句话**：`sh integrations/quickstart/install.sh`，然后把生成的
> `agent-prompt.md` 粘给你的 AI。剩下的它会自己装好。
