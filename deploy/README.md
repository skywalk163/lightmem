# deploy/ —— 部署初始化脚本（bootstrap）

> **定位**：这里放的是**部署/DBA 初始化**脚本，**不是 schema migration**。
> 与 `migrations/` **物理隔离**、语义分离：
>
> | 目录 | 管什么 | 谁负责 | 红线③（禁改 schema）是否适用 |
> |---|---|---|---|
> | `migrations/` | 表/列/索引/约束等 **schema 变更** | 应用版本演进 | ✅ 适用（受控） |
> | `deploy/` | 角色、库、权限、secrets 注入等**环境初始化** | 部署 / DBA | ❌ 不适用（不改 schema） |
>
> 业主裁定（2026-10-20）：「红线③约束的是 schema 变更，不是 DBA 初始化脚本。」

## init-roles.sql —— 应用运行角色 `lightmem_app`

**为什么需要**：应用若以 superuser（如 `postgres`）连接，将绕过 RLS、`pg_read_all_data`
等数据库层约束，多租户隔离退化为「仅靠应用层 SQL 谓词」。配套的**代码侧启动自检**
见 `kernel/pg_superuser_check.go`（连接建立后执行
`SELECT rolsuper FROM pg_roles WHERE rolname = current_user`，默认 fail-fast）。

### 用法

口令**必须从 secrets 注入**，不得写进仓库或命令行历史：

```sh
export PGPASSWORD="<postgres 超级用户口令>"     # 仅建角色这一步需要
psql -h "$PGHOST" -p 5432 -U postgres -d "$PGDATABASE" \
     -v app_password="$LIGHTMEM_APP_PASSWORD" \
     -f deploy/init-roles.sql
```

- 幂等：可重复执行（角色已存在则跳过创建，不重置口令）。
- 未传 `app_password` 时脚本中止 —— **有意为之**：绝不创建无口令的可登录角色。
- 脚本末尾会打印复核行，`is_superuser` **必须为 `false`**。

### 授权内容

| 对象 | 授权 | 备注 |
|---|---|---|
| DATABASE（当前库） | `CONNECT` | |
| SCHEMA `public` | `USAGE` | |
| TABLE `public.memories` | `SELECT, INSERT, UPDATE, DELETE` | R28 实测：当前仅此一张业务表 |
| 后续新建表（DEFAULT PRIVILEGES） | 同上 | 避免每次建表漏授权 |

实测（2026-10-20）：**无 sequence**——`memory_id` 为 `text`，由应用层生成，
故无需 `GRANT ... ON SEQUENCE`。

### 已知未覆盖（登记为 L-241，本轮不做）

`public.memories` 的 `rowsecurity = false`，`pg_policies` 为空 —— **未启用 RLS**。
启用 RLS 属 schema 变更且需按租户设计 policy 与 `TO` 子句，
已登记为下一轮 migration 候选；本脚本**不**开启 RLS。

---

## 连接串切换（裁定二第 3 件：配置变更，代码零改动）

应用侧连接角色由环境变量决定，切换只需改配置：

| 场景 | 变量 |
|---|---|
| 服务（FreeBSD rc.d） | `/etc/rc.conf`：`lightmem_pg_user="lightmem_app"`（口令经 secrets/env 注入） |
| 门禁 / CI | `LIGHTMEM_PG_USER=lightmem_app`、`LIGHTMEM_PG_PASSWORD=...` |

切换后必须验证（缺 GRANT 会在此暴露）：

```sh
cd kernel && go test -run 'EdgePersistPg' -count=1 -v .
```

### 回退

若切换后出现授权不全或其它失败，**直接改回原配置**即可，代码无需改动；
不要用 `LIMS_PG_SUPERUSER_CHECK=warn` 掩盖问题后宣布完成。

---

## 认证收紧（trust → scram-sha-256，R28 追加）

**为什么**：`pg_hba.conf` 原为 `host all all 10.0.10.0/24 trust` —— 网段内任何人都可**免密冒充任意角色**
（包括 superuser），多租户隔离形同虚设。

### 改法（0.88 上 PG 数据目录 `/var/db/pgdata`）

```sh
# 1) 先给可登录角色设强口令（口令以 SCRAM-SHA-256 存储）
APP_PW=$(openssl rand -hex 24); SUPER_PW=$(openssl rand -hex 24)
psql -h "$PGHOST" -U postgres -c "ALTER ROLE lightmem_app PASSWORD '$APP_PW'"
psql -h "$PGHOST" -U postgres -c "ALTER ROLE postgres   PASSWORD '$SUPER_PW'"

# 2) 口令落盘（chmod 600，绝不进 git / rc.conf）
printf 'LIMS_PG_PASSWORD=%s\n' "$APP_PW"   > /var/db/lightmem/pg_app.env
printf 'PGPASSWORD=%s\n'        "$SUPER_PW" > /var/db/lightmem/pg_super.env
chmod 600 /var/db/lightmem/pg_app.env /var/db/lightmem/pg_super.env

# 3) 备份 + 收紧 host 规则（local 保留，见下）
sudo -n cp /var/db/pgdata/pg_hba.conf /var/db/pgdata/pg_hba.conf.bak.r28
sudo -n sed -i '' -E 's/^(host[[:space:]]+.*)[[:space:]]+trust$/\1 scram-sha-256/' \
     /var/db/pgdata/pg_hba.conf

# 4) 生效（SQL 重载，无需知道 service 名）
psql -h "$PGHOST" -U postgres -c 'SELECT pg_reload_conf()'
```

> ⚠️ 收紧后**所有** psql / 应用连接都必须带口令。排障用 `psql -w`（`-w` = 不提示密码、直接失败），
> 否则非 tty 环境会因等待输入而挂住。

### `local` 已收紧为 peer（trust 全清零）

**前提发现**：PG 跑在 jail（10.0.10.10）内，unix socket 也在 jail 内——从 0.88 宿主**走不了 socket**
（`find` 不到、`sudo -u postgres psql` 报 `No such file or directory`），即 `local` 规则**实际无人使用**。
排查确认无 socket 免密依赖：workbuddy 无 crontab、系统 cron 无 psql、仓库脚本的 psql 全走 TCP（`-h`）。

```sh
# local 一并收紧（trust → peer）
sudo -n sed -i '' -E 's/^(local[[:space:]]+.*)[[:space:]]+trust$/\1 peer/' \
     /var/db/pgdata/pg_hba.conf
psql -c 'SELECT pg_reload_conf()'
```

- jail 内管理入口：`sudo -u postgres psql`（OS 用户 postgres 与 PG 角色同名，peer 认证通过）。
- **残余未知**：jail 内部无法 ssh 进入（22 端口拒绝），若 jail 内有 cron/脚本走 socket 免密会受影响
  （风险极低：该库为 lightmem 专用测试/开发库，应用全走 TCP）。
- **回退**：`sudo -n cp /var/db/pgdata/pg_hba.conf.bak.r28b /var/db/pgdata/pg_hba.conf` + reload。

### 口令注入路径（代码零改动）

kernel 的 `resolveSecrets`（`kernel/profile.go:358`）已支持 `LIMS_PG_PASSWORD`：

| 消费方 | 注入方式 |
|---|---|
| 服务 | `integrations/lims-088-rc.d/lightmem` 的 `lightmem_pg_secrets`（默认 `/var/db/lightmem/pg_app.env`）→ `LIMS_PG_PASSWORD` |
| 门禁 / CI | `/var/db/lightmem/gate.env` 追加 `export LIGHTMEM_PG_PASSWORD=...` |
| 人工运维 | `/var/db/lightmem/pg_super.env` |

secrets 文件缺失 → 空口令 → scram 下**连接失败并显式报错**，绝不静默退回免密。

### 过渡开关（仅临时使用）

`LIMS_PG_SUPERUSER_CHECK`：

| 值 | 行为 |
|---|---|
| 未设置 / `fail` | **默认**：superuser 连接 → 阻断启动 |
| `warn` | 只告警，不阻断（过渡期） |
| `off` | 跳过自检（显式关闭，会留日志） |

---

## P0-A 守护件（R29-T0）

> **文件域切分**：本目录（`deploy/`）由 **T0 线独占**，放的是「自愈/轮转/探针」部署件；
> `integrations/lims-088-rc.d/*`（rc.d 脚本 + 其 README）由 **A 线独占**，T0 只读复用。
> 两者分工：rc.d 负责 **进程级自愈**（`daemon -R 5 -C 24 -H`），P0-A 负责
> **应用级探活兜底**（看门狗）+ **日志轮转** + **systemd 模板**。

### 交付件清单

| 件 | 路径 | 职责 |
|---|---|---|
| 看门狗脚本 | `deploy/lightmem-watchdog.sh` | 每 2 分钟探 `/readyz`，未就绪则 `lm_service_restart`（linux `systemctl` / freebsd `service`）重启**本服务**；探活回退链 `curl`→`wget`→`fetch`（仅 freebsd）；冷却 120s 防抖，`logger` 记事件 |
| 定时条目（cron） | `deploy/cron.d/lightmem-watchdog` | FreeBSD 默认；Linux 无 systemd 时的备选（`*/2 * * * * root ...`） |
| systemd 看门狗单元 | `deploy/lightmem-watchdog.service` | Linux oneshot：`ExecStart=/usr/local/bin/lightmem-watchdog.sh`，`RuntimeDirectory=lightmem` |
| systemd 看门狗计时器 | `deploy/lightmem-watchdog.timer` | Linux 每 2 分钟触发（`OnCalendar=*:0/2`、`Persistent=true`、`RandomizedDelaySec=10`） |
| 日志轮转 | `deploy/newsyslog.d/lightmem.conf` | `lightmem.log` / `lightmem-mcp.log` 按日切、保留 7 份（对应 L-257） |
| systemd 模板 | `deploy/lightmem.service` | 非 FreeBSD/容器部署用（FreeBSD 真机走 rc.d）；`Restart=always` 等 + 探针注释 |

> 看门狗 + 定时器是 T0 的**补充件**，不替代 rc.d。FreeBSD 真机用 cron，Linux 默认 systemd timer；
> 每种系统**只装一种**定时方式（timer 与 cron 同时装会双份触发）。
> 冷却文件目录按平台选：Linux `/run/lightmem`（不可写则 `/var/lib/lightmem`），FreeBSD `/var/run/lightmem`。

### 安装（FreeBSD 真机）

```sh
# 1) 看门狗脚本 + 平台抽象层（脚本按 脚本同目录 → /usr/local/libexec/lightmem 的顺序找 platform.sh）
install -m 555 deploy/lightmem-watchdog.sh /usr/local/bin/lightmem-watchdog.sh
mkdir -p /usr/local/libexec/lightmem
install -m 644 deploy/platform.sh /usr/local/libexec/lightmem/platform.sh
# 2) cron 条目（系统 crontab 或 /etc/cron.d/）
install -m 644 deploy/cron.d/lightmem-watchdog /etc/cron.d/lightmem-watchdog
#   或：echo '*/2 * * * * root /usr/local/bin/lightmem-watchdog.sh' >> /etc/crontab
# 3) newsyslog 轮转
install -m 644 deploy/newsyslog.d/lightmem.conf /etc/newsyslog.d/lightmem.conf
```

> `mkdir -p /usr/local/libexec/lightmem` 不可省：FreeBSD 14/15 基座没有这个目录，
> 直接 `install -m 644 ... /usr/local/libexec/lightmem/platform.sh` 会
> `install: cannot create regular file ...: No such file or directory`，
> 看门狗随后退到「内置兜底」分支（不再走 `lm_service_restart`，Linux/FreeBSD 分支语义丢失）。

### 安装（Linux：systemd timer 或 cron 二选一）

```sh
# 0) 脚本 + 平台抽象层
install -m 555 deploy/lightmem-watchdog.sh /usr/local/bin/lightmem-watchdog.sh
mkdir -p /usr/local/libexec/lightmem
install -m 644 deploy/platform.sh /usr/local/libexec/lightmem/platform.sh

# A) systemd timer（推荐）
install -m 644 deploy/lightmem-watchdog.service deploy/lightmem-watchdog.timer /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now lightmem-watchdog.timer
systemctl list-timers lightmem-watchdog.timer

# B) 无 systemd（容器/老旧发行版）改用 cron，与 A 二选一
install -m 644 deploy/cron.d/lightmem-watchdog /etc/cron.d/lightmem-watchdog
```

> **端口口径不混用**：脚本默认探针 URL 按平台选——Linux `http://127.0.0.1:7443/readyz`
> （systemd/quickstart 口径），FreeBSD `http://127.0.0.1:18443/readyz`（rc.d 口径）；
> 只有改成非默认端口时才需要覆盖：systemd 写 `/etc/lightmem-watchdog.env`
> （单元已 `EnvironmentFile=-` 引入），cron 方式则在定时行前加 `LIGHTMEM_READYZ_URL=...`。

### 自测（dry-run：不重启、不写冷却/日志）

```sh
sh -n deploy/lightmem-watchdog.sh
LIGHTMEM_READYZ_URL=http://127.0.0.1:1/readyz LM_DRY_RUN=1 sh deploy/lightmem-watchdog.sh
# 想看 Linux/FreeBSD 分支的实际计划命令（单机上也能验证另一侧语义）：
LM_FAKE_OS=linux   LIGHTMEM_READYZ_URL=http://127.0.0.1:1/readyz LM_DRY_RUN=1 sh deploy/lightmem-watchdog.sh
LM_FAKE_OS=freebsd LIGHTMEM_READYZ_URL=http://127.0.0.1:1/readyz LM_DRY_RUN=1 sh deploy/lightmem-watchdog.sh
```

### 与 rc.d 自愈的关系（重要）

- **rc.d `daemon -R 5 -C 24 -H`**：管「进程崩溃/退出」——内核挂了 5s 拉起，24 次用尽后 daemon 自行退出，看门狗兜底重启。
- **看门狗 `/readyz`**：管「进程在但后端不可达」——例如 PG 短暂抽风、内核活着却 503，daemon 不会重启，看门狗补这个盲区。
- 二者**互补不替代**：看门狗有 120s 冷却防抖，避免重启风暴。

### 探针端点（S0 已落地，B 线硬化）

- `/livez`：存活探针，立即 200，不碰任何依赖（最快）。
- `/readyz`：就绪探针，`Ping` 后端，失败 503（S0 无 TTL 缓存，B 线硬化）。
- 两者均在 `kernel/auth.go` 的 `isOperationalEndpoint` 豁免清单内（免鉴权），否则 rc.d 健康检查与监控探针会 401。

---

## platform.sh 接口契约（T-0410，已冻结）

> 目的：lightmem 现有部署件里 OS 判断散在各处（看门狗用 `service`/`fetch`、quickstart 的
> `install.sh` 按 `ss`/`sockstat` 写提示、rc.d 硬编码 `/usr/local`）。Linux 适配若每条线
> 各写一套 `uname`/路径/服务管理判断必然漂移，故把这类判断收敛到 `deploy/platform.sh`。
>
> **接口名与语义已冻结**：后续线（systemd 单元、systemd timer 看门狗、logrotate、CI 冒烟）
> 只复用，不得改签名或语义；确需扩展时新增函数，不动既有六个。

### 定位与硬约束

- **POSIX sh 片段**，可被 `.` 引入；`sh -n deploy/platform.sh` 必须通过，不得引入 bash 专属语法。
- 被引入时**无副作用**：只定义函数与默认值，不执行任何系统命令。
- 只做平台判断，不承载业务逻辑；不改 rc.d 既有脚本行为，不动 kernel 代码。
- 端口口径不在本文件里编码：`integrations/quickstart` / systemd 用 **7443/7444**，
  FreeBSD rc.d 用 **18443/18444**，两者不得混改（`lm_probe_port` 只接收调用方给的端口）。

### 环境变量

| 变量 | 默认 | 作用 |
|---|---|---|
| `LM_DRY_RUN` | `0` | 置 `1`（或 `true/yes/on`）时只打印将要执行的命令，不改动系统 |
| `LM_FAKE_OS` | 空 | 测试钩子：强制走 `freebsd` / `linux` / `darwin` 分支；非法值返回 2 |
| `LM_DEPLOY_DIR` | `deploy` | `deploy/` 目录位置；FreeBSD 装 newsyslog 规则时按它取源文件 |

### 六个接口（冻结）

| 接口 | 语义 | 返回值 |
|---|---|---|
| `lm_detect_os` | 打印 `freebsd` / `linux` / `darwin`（基于 `uname -s`） | 0 成功；1 未知系统；2 `LM_FAKE_OS` 非法 |
| `lm_prefix` | 打印安装前缀：linux `/opt/lightmem`；freebsd（及 darwin）`/usr/local/lightmem` | 0；OS 判断失败时透传 |
| `lm_service_restart <name>` | linux `systemctl restart <name>`；freebsd `service <name> restart` | 命令退出码；缺参 2；darwin 未支持 3 |
| `lm_service_install <src> <name>` | linux 装 `/etc/systemd/system/<name>.service`（644）并 `daemon-reload`；freebsd 装 `/usr/local/etc/rc.d/<name>`（555） | 0；缺参 2；非 root/源文件缺失 1；darwin 3 |
| `lm_probe_port <port>` | 端口是否在监听：优先 `ss -ltn`，回退 `sockstat -4l`，再回退 `lsof` | 0 在监听（打印命中行）；1 未监听；2 参数错误/无可用工具 |
| `lm_logrotate_install [src]` | linux 装 `/etc/logrotate.d/lightmem`（无 `src` 时写内置 `daily/rotate 7/compress` 规则）；freebsd 装 `/usr/local/etc/newsyslog.conf.d/lightmem.conf`（无 `src` 时取 `${LM_DEPLOY_DIR}/newsyslog.d/lightmem.conf`） | 0；非 root/源文件缺失 1；darwin 3 |

**DRY_RUN 约定**：`lm_service_restart` / `lm_service_install` / `lm_probe_port` /
`lm_logrotate_install` 在 `LM_DRY_RUN=1` 时只打印 `[dry-run] ...` 计划行并返回 0，
不执行 `systemctl`/`service`/`install`/写文件；`lm_probe_port` 的 dry-run 不判定「是否在监听」，
仅用于形状断言。`lm_detect_os` / `lm_prefix` 是纯函数，dry-run 不影响结果。

### 用法

```sh
. "$(dirname "$0")/platform.sh"

os=$(lm_detect_os) || exit 1
prefix=$(lm_prefix)
lm_probe_port 7443 || echo "端口 7443 未监听"
lm_service_install deploy/lightmem.service lightmem   # Linux；FreeBSD 传 rc.d 脚本
lm_logrotate_install
```

### 自测

```sh
sh -n deploy/platform.sh      # POSIX 语法门禁
sh deploy/platform_test.sh    # 逐接口形状断言（无需 root，不写系统）
```

`platform_test.sh` 用 `LM_FAKE_OS` 在单机上把 freebsd/linux 两个分支都跑一遍：
Linux 真机可验 FreeBSD 语义，FreeBSD 真机也可验 Linux 语义，避免「只在本机 OS 上绿灯」的假覆盖。
唯一真实动作是 `lm_probe_port` 的只读探测（探一个必然没人监听的端口）。

在 Linux（dash）与 FreeBSD 15.1（`/bin/sh`）上均为 **35 passed, 0 failed**。
在 Windows 的 Git Bash（`MINGW64_NT-*`）下，「真实系统识别」一条会标 `skip`——契约只认
FreeBSD/Linux/Darwin，MINGW 本来就该返回 1；其余 34 条照测，用于在没有 POSIX 真机的提交机上
做形状回归（`bash deploy/platform_test.sh` 同样 exit 0）。

Windows 提交机若 PATH 里没有 `sh`（Git for Windows 只把 `git.exe` 放进 PATH，`sh.exe` 在
`Git\bin` 下），可以把 `sh` 指向 `C:\Program Files\Git\bin\sh.exe` 再跑同一套判据；
判据本身不变，别为过判据去改脚本。

---

## 已知部署坑（R29-F 真机验收登记，**装之前先看**）

> 来源：`_R29_F线验收报告.md` §5、`_R29_遗留问题存档.md` §2。三条都是**部署侧**问题，与 R29 代码逻辑无关，
> 但会让人「服务起不来」而误判成产品坏了。

### 1. L-258（P0）· solo 档相对 sqlite 路径会解析到 `/`

`integrations/lims-088-rc.d/lightmem` 的 `lightmem_start()` **不会 `cd ${lightmem_chdir}`**，
而 `service(8)` 会把 CWD 置为 `/`；仓库自带的 `profiles/lims-solo.profile.yaml` 用的是**相对路径**
`./data/lims.sqlite` → 实际解析成 `/data/lims.sqlite`，服务用户无权限 → 日志刷
`sqlite wal pragma: unable to open database file`，健康检查永远不过。

**规避（修复前）**：把 profile 的 sqlite 路径改成绝对路径，并让 `rc.conf` 指向该副本：

```sh
sudo cp /usr/local/lightmem/profiles/lims-solo.profile.yaml \
        /usr/local/lightmem/profiles/lims-solo-abs.profile.yaml
sudo sed -i '' 's#path: ./data/lims.sqlite#path: /usr/local/lightmem/data/lims.sqlite#' \
        /usr/local/lightmem/profiles/lims-solo-abs.profile.yaml
sudo sysrc lightmem_profile="/usr/local/lightmem/profiles/lims-solo-abs.profile.yaml"
```

### 2. L-259（P0）· `lightmem_pg_wait=0` 表达不了「不等 PG」

`0` 会让 `while` 循环体不执行、随后仍 `return 1` → 启动直接失败并打印
`PostgreSQL <host>:<port> 在 0s 内不可达`（**即使端口是通的**）。solo/无 PG 的机器尤其容易踩。

**规避（修复前）**：把 `lightmem_pg_wait` 设成 ≥1 的秒数，并保证该端口**可达**
（没有 PG 时可临时放一个占位监听；仅满足 `nc -z` 可达门，solo 档并不会真的连 PG）。

### 3. L-260（P1）· 孤儿 supervisor 会让 `stop` 失效

`lightmem_stop()` 只 `kill -TERM "$(cat ${pidfile})"`。若历史上留下**第二个** supervisor
（异常启动/重复 start），它不在 pidfile 里，仍会按 `-R` 继续拉起子进程 → 表现为「stop 之后服务自己活了」。

**处置**：`sudo pkill -9 -f 'daemon: env'` + `sudo pkill -9 -x lightmem-kernel` + 删 pidfile。
注意 daemon 的 cmdline 是 `daemon: env[pid] (daemon)`，用 `pkill -f 'daemon -f -H -R'` **匹配不到**。

### 4. L-261（P1，口径）· PG 档的启动自检是「默认开」

`storage_backend: pgvector*` 时，启动自检（表/关键列）**不依赖 `-data-dir`**，缺表缺列直接 fail-fast。
这是 B2 的登记行为，但意味着「未设新开关 = 与 R28 逐位一致」只对 sqlite 档成立。
**部署 PG 档前必须先应用 `kernel/migrations/001_pgvector.up.sql`**，否则服务起不来（这是修复意图，不是故障）。

---

## Linux 真机验收新增坑（T-0417，2026-11-20，Ubuntu 24.04.4 实测）

> 来源：`_LM_Linux适配_验收报告.md` §5。这四条都是**跨线接缝**：单线 dry-run 全绿，真机拼起来才炸。
> 前三条已在 T-0417 内小修或给规避件，第四条只登记（不改 unit 语义）。

### 5. unit 装成 `lightmem.service.service`（A ↔ T0 接缝，已修）

`integrations/quickstart/install.sh` 调 `lm_service_install` 时传的 `<name>` **已带 `.service`**
（如 `lightmem.service`），早期实现又无条件补一次后缀，落点变成
`/etc/systemd/system/lightmem.service.service` → `systemctl enable --now lightmem.service` 找不到 unit，
`install.sh --systemd` 以 `exit 1` 收场，而**服务其实没装**（极易误判成"systemd 不可用"）。

- 现状：`deploy/platform.sh` 已按后缀归一（`*.service` 原样、否则补一次），接口签名未变。
- 回归：`deploy/platform_test.sh` 含「带 `.service` 后缀不再重复拼接 / 不产生 `.service.service`」两条用例。
- 自查：`ls /etc/systemd/system/ | grep '.service.service'` 必须为空。

### 6. `EnvironmentFile` 里塞了多行 JSON，kernel 启动即崩（A 线待根修，已给规避件）

`scripts` 侧签发的 `agent-tokens.json` 是约 40 行的**美化 JSON**；`install.sh --systemd` 用
`printf 'LIMS_AGENT_TOKENS=%s\n' "$(cat ...)"` 原样写入 `lightmem.env`。systemd 的 `EnvironmentFile`
**按行解析**，只保留首行 `{`，于是：

```
[boot] auth config: LIMS_AGENT_TOKENS: parse agent tokens json: unexpected end of JSON input
lightmem.service: Main process exited, code=exited, status=1/FAILURE   （重试 5 次后 Start request repeated too quickly）
```

- 规避（可重复执行，实测 `891 bytes, 1 line`）：

```sh
sudo sh scripts/lm-env-compact.sh /opt/lightmem/_lightmem_run    # 或你的 RUN_DIR
sudo systemctl restart lightmem.service lightmem-mcp.service
```

- 自查：`wc -l "$RUN_DIR/lightmem.env"` 必须是 `1`；`sudo systemctl show lightmem.service -p Environment | head -c 200` 应能看到完整 JSON。

### 7. logrotate 在 Ubuntu/Debian 上被「父目录权限不安全」整条跳过（已修）

Ubuntu 的 `/var/log` 是 `root:syslog 775`（属组可写），logrotate 没有 `su` 指令时**拒绝**操作该目录：

```
error: skipping "/var/log/lightmem.log" because parent directory has insecure permissions
       (It's world writable or writable by group which is not "root")  Set "su" directive ...
```

`sudo logrotate -f /etc/logrotate.d/lightmem` 直接 `rc=1`，旧档根本不生成（日志继续涨，看起来像"轮转没配"）。

- 现状：`deploy/logrotate.d/lightmem` 已在块内加 `su root root`（`-d`/`-f` 均 `rc=0`，`delaycompress` 两拍语义实测成立）。
- 自查：`sudo logrotate -d /etc/logrotate.d/lightmem` 末尾不应出现 `skipping ... insecure permissions`。

### 8. 装 `platform.sh` 前必须先建 `/usr/local/libexec/lightmem`（文档已修）

`install -m 644 deploy/platform.sh /usr/local/libexec/lightmem/platform.sh` 在目录不存在时直接失败
（`install: cannot create regular file ...: No such file or directory`）。看门狗脚本随后找不到 `platform.sh`，
打印 `未找到 platform.sh（可用 LIGHTMEM_PLATFORM_SH 指定），启用内置兜底` ——
`lm_service_restart` 的 Linux/FreeBSD 分支语义就此丢失（重启动作退化成内置实现，跨平台差异不再生效）。

- 现状：本文档「安装（FreeBSD 真机）」「安装（Linux）」两段步骤均已补 `mkdir -p /usr/local/libexec/lightmem`。
- 自查：`sh -c '. /usr/local/libexec/lightmem/platform.sh; lm_detect_os'` 应打印 `linux`/`freebsd`（rc 0）。

### 9. 登记（不改）：systemd 部署下服务日志走 journald，logrotate 对真实日志是空转

`deploy/lightmem.service` / `lightmem-mcp.service` 未配 `StandardOutput=`，日志默认进 journald
（`journalctl -u lightmem.service` 可见 `[access]` 行）；`/var/log/lightmem*.log` 在 systemd 路径下**并不存在**，
`deploy/logrotate.d/lightmem` 因 `missingok` 每晚静默跳过，真实日志由 journald 自行回收。

- 两条可选口径（**择一，别都做**）：给 unit 加 `StandardOutput=append:/var/log/lightmem.log`
  （让 logrotate 真正生效），或明确登记"journald 侧用 `SystemMaxUse` / `MaxRetentionSec` 治理"。
- 本卡不改 unit 语义（红线），仅登记，待 A/C 线定夺。

---

## EOL 门禁（T-0427，T-0417 TODO#4）

deploy/ 下的 systemd unit（`lightmem*.service` / `lightmem-watchdog.timer`）、cron 条目
（`cron.d/`）、logrotate / newsyslog 规则，**行尾带 `\r`（CRLF）会让对应解析器直接失败**
且报错隐晦：cron 静默不跑、systemd `ExecStart` 起不来、logrotate stat 报错。
这些文件没有常规扩展名，曾落在 `* text=auto` 兜底里 —— Windows 检出成 CRLF，
`git add` 时极易把 CRLF 提上去（T-0417 实测踩过：本地写入带 CRLF，logrotate 解析失败；
`deploy/logrotate.d/lightmem` 还曾缺末行换行）。`.gitattributes` 已用**路径级**
`text eol=lf` 钉死（见其 deploy 规则段），并要求这些文件末字节为 `0x0a`。

自查（POSIX sh，零依赖，Windows 提交机可用 Git 自带的 `sh` 跑）：

```sh
sh scripts/check-eol-lf.sh
```

新增部署文件时同步两处：`.gitattributes` 的路径级规则 + `scripts/check-eol-lf.sh` 的
`TARGETS` 清单（缺一处门禁会漏检或误报不同步）。

> AI生成
