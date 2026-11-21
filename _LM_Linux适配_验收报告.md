# _LM_Linux适配_验收报告（T-0417 · 收口卡）

> 卡号：T-0417（lightmem Linux 适配 G：Linux 真机全链验收收口）
> 验收机：**192.168.0.86**（`ub86`），Ubuntu 24.04.4 LTS，非 FreeBSD 0.88 现网
> 代码版本：`56df3c5`（origin/main，含 A–F 全部合入 + T-0413 logrotate）
> 执行日期：2026-11-20（UTC 16:24–16:40）
> 原则：**本报告每条 PASS 都对应一条真实命令 + 真实退出码**，未实测的一律标 FAIL/BLOCKED，不臆造。

---

## 0. 结论速览

| 项 | 结果 |
|---|---|
| 全链（安装→systemd→探针→save/recall→看门狗→logrotate→CI 门禁） | **11 个环节实测通过**，3 处接缝问题在本卡小修或登记 |
| 真机首次 `install.sh --systemd` 能否开箱即用 | **不能**（命中 2 个 A/T0 接缝；见 §5），修复后 PASS |
| save→recall 闭环 | **PASS**，`memory_id=001a1f9c91d8fa6432dc356539eb6` |
| E 线 CI 门禁在真 Linux | **PASS**（27 PASS / 0 FAIL / 1 SKIP，exit 0；修复前 1 条误报 FAIL） |
| 对 FreeBSD 0.88 现网 | **零改动**（本卡只在 0.86 验收机上操作） |

---

## 1. 环境实测

命令与输出（日志：`~/acc/01_env.log`）：

```
$ uname -a
Linux ub86 6.8.0-147-generic #147-Ubuntu SMP PREEMPT_DYNAMIC ... x86_64 GNU/Linux

$ grep -E 'PRETTY_NAME|VERSION_ID|VERSION_CODENAME' /etc/os-release
PRETTY_NAME="Ubuntu 24.04.4 LTS"
VERSION_ID="24.04"
VERSION_CODENAME=noble

$ systemctl --version | head -1
systemd 255 (255.4-1ubuntu8.18)

$ go version
go version go1.27.4 linux/amd64

$ python3 --version
Python 3.12.3

$ id
uid=1003(workbuddy) gid=1003(workbuddy) groups=1003(workbuddy),27(sudo),100(users)

$ git rev-parse HEAD
56df3c5bf0606d187c36617de243591e314a1fa2
```

补充：`/` 8.7G、可用 4.4G；内存 3.9G。`workbuddy` 在 `sudo` 组，`sudo` 口令可用（脚本内 `sudo -S`）。

**工具链来源（本机原本没有 Go）**：apt 只有 1.22（低于 `go.mod` 要求的 1.25.0），`go.dev` 直连超时；
改从 `golang.google.cn/dl` 取镜像包，sha256 对齐官方 dl JSON：

```
$ sha256sum -c   →  go1.27.4.linux-amd64.tar.gz: OK      (exit 0)
$ gzip -t        →  GZIP_OK                              (exit 0)
$ GOPROXY=https://goproxy.cn,direct go get gopkg.in/yaml.v3@v3.0.1
   → go: added gopkg.in/yaml.v3 v3.0.1                   (exit 0)
```

---

## 2. 全链步骤：命令 + 退出码 + 输出摘要

### 2.1 非 root quickstart 安装（`--profile solo`）

```
$ sh integrations/quickstart/install.sh --profile solo
phase1_exit=0
    ✓ Go: go1.27.4
    ✓ Python: Python 3.12.3
    ✓ lightmem-kernel -> _lightmem_run/lightmem-kernel (version=56df3c5...)
    ✓ lightmem-mcp -> _lightmem_run/lightmem-mcp
    ✓ 已写 _lightmem_run/lims-quickstart.profile.yaml
    ✓ token 表 -> _lightmem_run/agent-tokens.json
    ✓ REST 就绪：{"backend":"sqlite","embedder":"hash","profile":"solo","status":"ok",...}
    ✓ MCP 就绪：{"service":"lightmem-mcp","status":"ok","upstream_status":"ok"}
    ✓ 接入包 -> _lightmem_run/clients/
```

**PASS**（非 root、零外部依赖、SQLite solo 档一次过）。

### 2.2 REST / MCP 探针

```
$ curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:7443/healthz   → 200  curl_exit=0
$ curl ... /livez                                                        → 200  curl_exit=0
$ curl ... /readyz                                                       → 200  curl_exit=0
$ curl ... http://127.0.0.1:7444/healthz                                 → 200  curl_exit=0
$ （MCP tools/list，Bearer token）
  http_code=200  curl_exit=0  tools_count=13
  tools=lightmem_save,lightmem_recall,lightmem_search,lightmem_update,lightmem_feedback,
        lightmem_forget,lightmem_health,lightmem_goal_create,lightmem_goal_progress,
        lightmem_goal_state,lightmem_plan_list,lightmem_step_add,lightmem_summary_add
```

**PASS**：三探针全 200，MCP 13 个工具可用。

### 2.3 MCP `lightmem_health`

```
$ tools/call lightmem_health {}
  http_code=200 / curl_exit=0
  lightmem 状态：{"backend":"sqlite","profile":"solo","status":"ok","version":"56df3c5..."}
```

**PASS**（走原生 MCP 入口，聊天 Om 的 `mcp__lightmem__lightmem_health` 等价路径）。

### 2.4 save → recall 闭环（memory_id）

```
$ tools/call lightmem_save {"fact":"T-0417 Linux 全链验收样本：...","memory_kind":"event"}
  http_code=200 / curl_exit=0
  已保存记忆。memory_id=001a1f9c91d8fa6432dc356539eb6 layer=L1

$ tools/call lightmem_recall {"text":"Ubuntu 24.04 全链验收 save recall 闭环","top_k":5}
  http_code=200 / curl_exit=0
  查询 "..." 命中 1 条（mode=unique）：
  1. [score=0.9946236559139783 layer=L1] T-0417 Linux 全链验收样本...
     memory_id=001a1f9c91d8fa6432dc356539eb6
  HIT_MID=YES
```

**PASS**：save 产出 `memory_id`，recall 按融合得分（0.9946）命中同一条。

> 自纠：首轮复核脚本用 JSON 正则抓 `"memory_id":"..."`，而工具返回是纯文本 `memory_id=<hex>`，
> 导致脚本误报 `HIT_MID=NO`；**这是脚本正则的问题，不是产物问题**，已改为按纯文本格式匹配后
> `HIT_MID=YES`（`~/acc/05_recall_verify.log`）。

### 2.5 systemd 双服务（`--systemd`）

首装（**FAIL，命中接缝**，见 §5.1）：

```
$ sudo sh integrations/quickstart/install.sh --profile solo --systemd --service-user workbuddy
  ✓ 已引入 deploy/platform.sh，用 lm_service_install 装 unit
  ✓ 凭据写入 _lightmem_run/lightmem.env（600）
  [!] systemctl 调用失败...
[x] systemctl enable --now 失败
phase2_exit=1
$ ls /etc/systemd/system/ | grep lightmem
  lightmem-mcp.service.service
  lightmem.service.service          ← 双后缀
```

修复（`deploy/platform.sh` 后缀归一）后重跑：

```
$ phase2_retry_exit=0
  ✓ systemd 已启用：lightmem.service lightmem-mcp.service（开机自启 + 立即启动）
$ systemctl is-enabled lightmem.service lightmem-mcp.service   → enabled / enabled
$ systemctl is-active  ...                                     → active  / active
$ ss -ltn | grep -E '7443|7444'
  LISTEN 127.0.0.1:7443
  LISTEN 127.0.0.1:7444
```

但 kernel unit 首启 **FAIL**（命中接缝 §5.2）：

```
Nov 20 16:30:30 lightmem-kernel[10521]: [boot] profile=solo backend=sqlite tier=solo
Nov 20 16:30:30 lightmem-kernel[10521]: [boot] auth config: LIMS_AGENT_TOKENS:
                                        parse agent tokens json: unexpected end of JSON input
Nov 20 16:30:30 systemd[1]: lightmem.service: Main process exited, code=exited, status=1/FAILURE
...(重试 5 次) → Failed to start lightmem.service
```

按 §5.2 的压平步骤处理后：

```
$ sudo sh scripts/lm-env-compact.sh /home/workbuddy/lightmem/_lightmem_run
  wrote .../lightmem.env (891 bytes, 1 line)
  OK: .../lightmem.env 单行、600                     (exit 0)
$ sudo systemctl restart lightmem.service lightmem-mcp.service
$ systemctl is-active lightmem.service lightmem-mcp.service  → active / active
$ curl /healthz → rest=200
$ curl :7444/healthz → {"service":"lightmem-mcp","status":"ok","upstream_status":"ok"}
```

unit 渲染结果（`grep` 自 `/etc/systemd/system/lightmem.service`）：

```
User=workbuddy
Group=workbuddy
EnvironmentFile=-/home/workbuddy/lightmem/_lightmem_run/lightmem.env
ExecStart=/home/workbuddy/lightmem/_lightmem_run/lightmem-kernel -profile .../lims-quickstart.profile.yaml -listen 127.0.0.1:7443
ReadWritePaths=/home/workbuddy/lightmem/_lightmem_run
```

**PASS**（修复后）：双 unit `enabled + active`，端口 7443/7444 与 A 线口径一致（**未改端口口径**）。

### 2.6 systemd 下的 save→recall（含跨重启持久性）

```
$ tools/call lightmem_save {"fact":"T-0417 systemd 口径复测：双 unit 重启后仍可 save→recall"}
  已保存记忆。memory_id=001a1f9d09052ca2091016a1c8d5e layer=L1      (curl_exit=0)
$ tools/call lightmem_recall {"text":"systemd 口径 双 unit 重启 save recall"}
  命中 2 条（mode=fused）
  1. [score=0.9865] ... memory_id=001a1f9d09052ca2091016a1c8d5e
  2. [score=0.9846] ... memory_id=001a1f9c91d8fa6432dc356539eb6
  HIT_MID=YES                                                     (curl_exit=0)
$ recall("Ubuntu 24.04 全链验收 save recall 闭环")
  PHASE1_MID_STILL_THERE=YES
$ ls -l _lightmem_run/data/  → lims.sqlite / -shm / -wal（owner workbuddy）
```

**PASS**：systemd 口径可写可读；phase1（非 root nohup 阶段）写入的记忆在多次安装/重启后仍在。

### 2.7 看门狗（B 线，systemd timer 口径）

```
$ sudo install -m 555 deploy/lightmem-watchdog.sh /usr/local/bin/lightmem-watchdog.sh   → rc 0
$ sudo mkdir -p /usr/local/libexec/lightmem                                            → rc 0
$ sudo install -m 644 deploy/platform.sh /usr/local/libexec/lightmem/platform.sh        → rc 0
$ sudo install -m 644 deploy/lightmem-watchdog.{service,timer} /etc/systemd/system/     → rc 0
$ sudo systemctl enable --now lightmem-watchdog.timer                                   → rc 0
$ systemctl is-enabled/is-active lightmem-watchdog.timer  → enabled / active
$ systemctl list-timers lightmem-watchdog.timer → NEXT 1min 17s

# dry-run（不重启、不写冷却/日志）
$ LM_DRY_RUN=1 ... sh deploy/lightmem-watchdog.sh
  [dry-run] 探活 ... -> rc=1 code=000
  [dry-run] readyz 未就绪（code=000），重启 lightmem
  [dry-run] systemctl restart lightmem          (dryrun_linux_rc=0)
$ LM_FAKE_OS=freebsd ... → [dry-run] service lightmem restart   (dryrun_freebsd_rc=0)
```

**真实故障注入**（停 kernel → 看门狗拉起）：

```
$ sudo systemctl stop lightmem.service ; systemctl is-active → inactive
$ curl /readyz → readyz_while_down=000  (curl: (7) Failed to connect)
$ sudo env LIGHTMEM_READYZ_URL=...:7443/readyz LIGHTMEM_WATCHDOG_SERVICE=lightmem \
       /usr/local/bin/lightmem-watchdog.sh                       → watchdog_rc=0
$ systemctl is-active lightmem.service → active
$ curl /readyz → readyz=200
$ /var/log/lightmem-watchdog.log:
  2026-11-20T16:33:24Z readyz 未就绪（code=000），重启 lightmem
  2026-11-20T16:33:24Z 已重启 lightmem
$ ls -l /run/lightmem/watchdog.cooldown                          → 存在（root, 11B）
```

**冷却防抖**：立刻再跑一次 → 退出码 0、日志**无新增条目**（被 120s 冷却挡下，未发生第二次重启）。

**经 systemd oneshot 单元跑**：

```
$ sudo systemctl start lightmem-watchdog.service      → unit_start_rc=0
$ systemctl show lightmem-watchdog.service -p Result -p ExecMainStatus
  Result=success  ExecMainStatus=0
```

**PASS**（三个层次：脚本 dry-run / 手工真实注入 / systemd oneshot 单元）。

### 2.8 logrotate（C 线）

首版配置在 Ubuntu 真机上 **FAIL**（见 §5.3）：

```
$ sudo logrotate -d /etc/logrotate.d/lightmem
  error: skipping "/var/log/lightmem.log" because parent directory has insecure permissions
         (It's world writable or writable by group which is not "root")  Set "su" directive ...
  logrotate_d_rc=1
$ （造 50 行文件后）sudo logrotate -f /etc/logrotate.d/lightmem → logrotate_f_rc=1，未生成 .1.gz
```

补 `su root root` 后：

```
$ sudo logrotate -d /etc/logrotate.d/lightmem
  rotating pattern: /var/log/lightmem.log /var/log/lightmem-mcp.log  after 1 days (7 rotations)
  considering log /var/log/lightmem.log
  considering log /var/log/lightmem-mcp.log              → logrotate_d_rc=0

$ sudo logrotate -f /etc/logrotate.d/lightmem              → logrotate_f_rc=0
  -rw-r--r-- 0    /var/log/lightmem.log          （copytruncate 后截断为 0 行）
  -rw-r--r-- 391  /var/log/lightmem.log.1
  -rw-r--r-- 131  /var/log/lightmem-mcp.log.1

$ （再写 30 行后）sudo logrotate -f ...                    → logrotate_f2_rc=0
  lightmem.log     0
  lightmem.log.1   291     ← 当拍不压（delaycompress）
  lightmem.log.2.gz 127    ← 上一拍才压
```

**PASS**：`-d` 干跑、强制轮转、`copytruncate` 截断、`delaycompress` 两拍语义均实测成立。

### 2.9 E 线 CI 门禁（Linux 真机）

```
$ sh scripts/ci-linux-smoke.sh
  首跑：PASS=26 FAIL=1 SKIP=1  →  ci_smoke_exit=1
  FAIL  sh -n scripts/p0_verify.sh
        | scripts/p0_verify.sh: 103: Syntax error: "(" unexpected
$ （修复：按 shebang 分派 bash -n / sh -n）重跑
  PASS=27 FAIL=0 SKIP=1  →  ci_smoke_exit=0
  RESULT: PASS exit=0
  SKIP  systemd-analyze verify：unit 语法通过，但 ExecStart 目标未安装（未部署环境）
```

**PASS**（27 PASS / 0 FAIL；唯一的 SKIP 是"未部署环境"的 ExecStart 目标检查，属预期）。

---

## 3. PASS / FAIL 判定表

| # | 环节 | 判据（命令） | 退出码 / 观测 | 结论 |
|---|---|---|---|---|
| 1 | 环境采样 | `uname -a` / `cat /etc/os-release` / `systemctl --version` | 全部 rc 0 | PASS |
| 2 | 工具链 | `go version`、`python3 --version`、`sha256sum -c` | 1.27.4 / 3.12.3 / OK (rc 0) | PASS |
| 3 | 非 root 安装 | `sh install.sh --profile solo` | exit 0 | PASS |
| 4 | REST 探针 | `curl /healthz /livez /readyz` | 200/200/200，curl_exit 0 | PASS |
| 5 | MCP 探针 | `curl :7444/healthz` + `tools/list` | 200，13 tools | PASS |
| 6 | MCP health | `tools/call lightmem_health` | 200，status ok | PASS |
| 7 | save→recall | `lightmem_save` + `lightmem_recall` | memory_id=001a1f9c…，HIT_MID=YES | PASS |
| 8 | systemd unit 落点 | `ls /etc/systemd/system/ \| grep lightmem` | 首装出现 `lightmem.service.service`，exit 1 | **FAIL（已修复）** |
| 9 | systemd 启用 | `systemctl is-enabled / is-active` | enabled/active ×2 | PASS |
| 10 | EnvironmentFile | `journalctl -u lightmem.service` | 原样写多行 JSON → parse 失败 | **FAIL（已修复）** |
| 11 | 端口口径 | `ss -ltn \| grep 7443/7444` | 7443、7444 监听 | PASS |
| 12 | 看门狗安装 | `systemctl list-timers lightmem-watchdog.timer` | enabled/active，NEXT 1min | PASS |
| 13 | 看门狗 dry-run | `LM_DRY_RUN=1 sh deploy/lightmem-watchdog.sh` | rc 0（linux/freebsd 分支均打印计划） | PASS |
| 14 | 看门狗真实拉起 | 停服务 → 跑脚本 → 查 active/readyz | active，readyz=200，日志 2 行 | PASS |
| 15 | 冷却防抖 | 立刻二次执行 | rc 0，日志无新增 | PASS |
| 16 | 看门狗 oneshot | `systemctl start lightmem-watchdog.service` | Result=success | PASS |
| 17 | logrotate 干跑 | `logrotate -d /etc/logrotate.d/lightmem` | 首版 rc 1（insecure perms） | **FAIL（已修复）** |
| 18 | logrotate 轮转 | `logrotate -f` | rc 0，.1 生成、活动文件截断 | PASS |
| 19 | delaycompress | 二次 `-f` | .1 未压、.2.gz 生成 | PASS |
| 20 | CI 门禁 | `sh scripts/ci-linux-smoke.sh` | 首跑 FAIL=1 → 修复后 27/0/1，exit 0 | **PASS（修复后）** |
| 21 | 记忆持久性 | 重启后 recall phase1 记忆 | PHASE1_MID_STILL_THERE=YES | PASS |
| 22 | FreeBSD 0.88 影响 | 本卡命令清单 | 未向 0.88 发起任何写操作 | PASS（零触碰） |

---

## 4. 与 FreeBSD 0.88 的口径差异登记

| 维度 | Linux（本卡实测） | FreeBSD 0.88（现网） | 是否可混用 |
|---|---|---|---|
| 端口 | 7443 / 7444（systemd + quickstart） | 18443 / 18444（rc.d） | **不可**，本卡未改任何端口 |
| 服务管理 | systemd unit（`lightmem.service` + `lightmem-mcp.service`） | rc.d（`lightmem` / `lightmem_mcp`） | 不可 |
| 日志轮转 | logrotate（`deploy/logrotate.d/lightmem`，**需 `su root root`**） | newsyslog（`deploy/newsyslog.d/lightmem.conf`） | 配置各放一份、互不覆盖 |
| 看门狗定时 | systemd timer（2 分钟） | `/etc/cron.d/lightmem-watchdog` | 二选一，不可同机双装 |
| 安装前缀 | 本卡用 quickstart 口径 `_lightmem_run`；`lm_prefix` 返回 `/opt/lightmem` | `/usr/local/lightmem` | 视部署方式 |
| 服务日志去向 | **journald**（unit 未配 `StandardOutput=`，实测日志在 `journalctl`） | 文件 `/var/log/lightmem*.log` | 见 §5.4 |
| 冷却文件目录 | `/run/lightmem`（不可写则 `/var/lib/lightmem`） | `/var/run/lightmem` | 各自默认，无需覆盖 |

---

## 5. 接缝问题（本卡修 / 本卡登记）

### 5.1 【已在本卡修】`lm_service_install` 双后缀 → unit 装不进去（A ↔ T0 接缝）

- 现象：`install.sh --systemd` 报 `[x] systemctl enable --now 失败`，`phase2_exit=1`；`/etc/systemd/system/` 里出现 `lightmem.service.service`、`lightmem-mcp.service.service`。
- 根因：`install.sh` 传入的 `<name>` 已含后缀（`lightmem.service`），`deploy/platform.sh` 的 `lm_service_install` 又无条件补 `.service`。
- 修法（`deploy/platform.sh`）：按后缀归一（`*.service` 原样、否则补一次），**接口签名不变**；`deploy/platform_test.sh` 增补 2 条回归用例（带后缀 → 目标正确、且不产生 `.service.service`）。
- 验证：`sh deploy/platform_test.sh` → **37 passed, 0 failed**（exit 0）；重装后双 unit `enabled/active`。

### 5.2 【T-0417 给规避件 → T-0424 已根修】EnvironmentFile 写了多行 JSON

- 现象：kernel 启动即 `[boot] auth config: LIMS_AGENT_TOKENS: parse agent tokens json: unexpected end of JSON input` → `status=1/FAILURE`，重试 5 次后 `Start request repeated too quickly`。
- 根因：`issue_token.py` 产出的 `agent-tokens.json` 是约 42 行**美化 JSON**；`install.sh` 用 `printf 'LIMS_AGENT_TOKENS=%s\n' "$(cat ...)"` 原样写入 `EnvironmentFile`（按行解析，只保留首行 `{`）。
- 修法（T-0417）：新增 `scripts/lm-env-compact.sh`（`json.dumps(separators=...)` 压成单行 + `chmod 600` + 自检行数），实测 `891 bytes, 1 line`（exit 0）。
- **根修（T-0424，2026-11-21）**：`install.sh` 写 `$RUN_DIR/lightmem.env` 前先压平，规避件不再是必经步骤：
  - 仓库完整 → 调用 `scripts/lm-env-compact.sh`（同一份口径，避免两处实现漂移）；只有 `integrations/` 子集 → 内联 `python3 json.dumps(ensure_ascii=False, separators=(',',':'))`；
  - `python3` 缺失 → 明确 `exit 1`（不再写出一份 systemd 只认首行、kernel 必崩的 env）；
  - 写完自检：**恰好 1 行** + 前缀 `LIMS_AGENT_TOKENS=` + 解析回来与源 JSON 等价 + 600，任一不满足即失败；
  - `agent-tokens.json` 一个字节未改（`--systemd` 分支只改「怎么塞进 EnvironmentFile」）。
  - 回归：`bash integrations/quickstart/envfile_test.sh`（主路径 / 退化路径 / 旧写法反跑变红 / python3 缺失反跑），验证记录见 §5.2.1。

#### 5.2.1 根修验证记录（T-0424，FreeBSD 15.1 真机）

- 环境：`fb98`（FreeBSD 15.1-RELEASE-p1，Python 3.12.13，bash 5.3.15），仓 `task/T-0424` 干净 clone 后跑。
- 判据（三条全 rc=0）：

```
bash -n integrations/quickstart/install.sh                                      rc=0
python3 -c "import sys; s=open('integrations/quickstart/install.sh',encoding='utf-8').read(); sys.exit(0 if 'json.dumps' in s or 'lm-env-compact' in s else 1)"    rc=0
bash integrations/quickstart/envfile_test.sh                                    rc=0（12 项判据全绿）
```

- `envfile_test.sh` 实际输出（节选）：输入 34 行美化 JSON → `lightmem.env` `wc -l = 1`；前缀/`json.loads`/与源等价/600 全过；`agent-tokens.json` sha256 前后一致（未改一个字节）；退化路径（删掉 `scripts/` 后只留 `integrations/`）同样 1 行；反跑 A（旧写法 34 行 env）**变红**，报 `env 行数=34（期望恰好 1 行 + 结尾换行）`；反跑 B（PATH 里没有 python3）`install.sh` 非 0 退出且 `lightmem.env` 未落盘、提示里点了 python3。
- 真机产出（非测试夹具）：`sh install.sh --write-token-env /tmp/t0424_real_run` → `OK: ... 单行 150 bytes, json 等价, 600`，`wc -l = 1`，`-rw-------`。
- `--dry-run --systemd` 打印新路径：`[dry-run] 压平 .../agent-tokens.json → .../lightmem.env（单行 JSON，600）`。

**Linux systemd 真机复测（Ubuntu 24.04.4 `ub86` / 192.168.0.86，2026-11-21 02:2x UTC）**

- 用 `task/T-0424`（head `b825adb`）的干净 clone 跑 `install.sh --profile solo --systemd --service-user workbuddy`，`install_rc=0`；产出 `lightmem.env` = **891 bytes / `wc -l` = 1**（源 `agent-tokens.json` 仍是 **42 行**美化 JSON），`600 root`；`systemctl show -p EnvironmentFiles` 指向该 env。
- 先前崩溃点消失：journal 只有 `[auth] token 表已装配（source=env, tokens=8）` + `[auth] agent token 鉴权已启用（tokens=8）`（kernel 与 MCP 各 8 个 token），**没有任何** `parse agent tokens json: unexpected end of JSON input`。
- 服务真起来：`lightmem.service` `active (running)`，`ExecStart` 指向本次 clone 的 RUN_DIR，`/healthz` 返回 `version=b825adb…`（即本卡新二进制，不是旧部署的 `56df3c5`），MCP `/healthz` 也 `upstream_status=ok`。
- 同轮暴露的**另一个坑（非本卡范围）**：root 跑 install 会把 RUN_DIR 建成 `root:root 755`，systemd 以 `--service-user` 起 kernel 时 SQLite 报 `attempt to write a readonly database (1544)` → 服务 `status=1/FAILURE` 连重启 5 次（即 §7 TODO#2）。验收时临时 `chown -R workbuddy` 该 RUN_DIR 后服务即正常；**本卡未改这条语义**，仅登记证据。
- 复测后已收尾：卸本卡 unit、还原 0.86 原部署（`lightmem.service`/`lightmem-mcp.service` + `lightmem-watchdog.timer` 全部 `active`，旧 `/healthz` 恢复 `version=56df3c5`）。




### 5.3 【已在本卡修】logrotate 在 Ubuntu 上被"父目录权限不安全"整条跳过（C 线配置缺口）

- 现象：`logrotate -d/-f` 均 `rc=1`，报 `parent directory has insecure permissions ... Set "su" directive`。
- 根因：Ubuntu/Debian 的 `/var/log` 是 `root:syslog 775`（属组可写），logrotate 无 `su` 时拒绝操作该目录。
- 修法（`deploy/logrotate.d/lightmem`）：块内加 `su root root`，并在文件头写明原因；轮转语义（daily/rotate 7/compress/delaycompress/missingok/notifempty/copytruncate）一条未改。
- 验证：`-d` rc 0；`-f` rc 0，`.1` 391B、活动文件截断 0 行；二次 `-f` rc 0，`.1` 291B 未压、`.2.gz` 127B。

### 5.4 【登记，未改】systemd 部署下服务日志进 journald，logrotate 对真实日志是空转

- 证据：`grep -E 'StandardOutput|StandardError|SyslogIdentifier' /etc/systemd/system/lightmem*.service` → 均未配置（走 journald 默认）；`journalctl -u lightmem.service` 有真实 access 日志；`/var/log/lightmem*.log` 只在本卡**造样本**时才存在。
- 影响：C 线的 logrotate 配置在 systemd 路径下"永远 missingok 跳过"，真实日志由 journald 自行回收（实测 `journalctl --disk-usage` = 8.0M，`SystemMaxUse` 为注释默认）。
- 处置：本卡**不改 unit 语义**（红线）。建议 A/C 线择一：给 unit 加 `StandardOutput=append:/var/log/lightmem.log`，或把轮转口径改登记为"journald 侧用 `SystemMaxUse`/`MaxRetentionSec`"。

### 5.5 【已在本卡修】E 线门禁把 bash 脚本用 dash 做 `sh -n` → 真 Linux 上误报 FAIL

- 现象：`sh scripts/ci-linux-smoke.sh` → `FAIL sh -n scripts/p0_verify.sh`（`Syntax error: "(" unexpected`），exit 1。
- 根因：`p0_verify.sh` 是 bash（`RESULTS=()` 数组），门禁第 1 节对 `scripts/*.sh` 一律 `sh -n`。
- 修法（`scripts/ci-linux-smoke.sh`）：按 shebang 分派 `bash -n` / `sh -n`（不改检查强度，只改解释器）。
- 验证：27 PASS / 0 FAIL / 1 SKIP，exit 0。

### 5.6 【仅文档】平台抽象层落点目录不存在 → 看门狗退"内置兜底"

- 现象：按 README 原步骤 `install -m 644 deploy/platform.sh /usr/local/libexec/lightmem/platform.sh` 直接失败
  （`install: cannot create regular file ...: No such file or directory`），看门狗随后打印
  `未找到 platform.sh（可用 LIGHTMEM_PLATFORM_SH 指定），启用内置兜底`——`lm_service_restart` 的 Linux/FreeBSD 分支语义随之丢失。
- 修法（`deploy/README.md`）：FreeBSD / Linux 两段安装步骤都补 `mkdir -p /usr/local/libexec/lightmem`，并加一段说明。
- 验证：补装后 `sh -c '. /usr/local/libexec/lightmem/platform.sh; lm_detect_os'` → `linux`（rc 0）；真实注入走 `lm_service_restart` 成功。

---

## 6. 本卡改动清单（均在允许路径内）

| 文件 | 性质 | 说明 |
|---|---|---|
| `_LM_Linux适配_验收报告.md` | 新增 | 本报告 |
| `deploy/platform.sh` | 小修 | `lm_service_install` Linux 分支后缀归一（§5.1） |
| `deploy/platform_test.sh` | 小修 | +2 条回归用例与 `expect_not_has` helper（§5.1） |
| `deploy/logrotate.d/lightmem` | 小修 | 加 `su root root`（§5.3） |
| `deploy/README.md` | 文档 | 安装步骤补 `mkdir -p`（§5.6）+ 已知坑登记（§6.1） |
| `scripts/lm-env-compact.sh` | 新增 | EnvironmentFile 压平规避件（§5.2） |
| `scripts/ci-linux-smoke.sh` | 小修 | shebang 分派解释器（§5.5） |
| `docs/lightmem-Linux适配-并行任务书_v1.md` | 新增 | 总任务书 + A–G 卡号/状态回填 |

未改动：`kernel/`（核心语义）、任何端口口径（7443/7444 与 18443/18444 各归各）、FreeBSD 0.88 现网。

### 6.1 deploy/README.md 已知坑登记（本轮新增）

1. `lm_service_install` 传入带 `.service` 后缀的 name（A 线 install.sh 的写法）→ T0 归一，否则装成 `.service.service`；
2. `EnvironmentFile` 必须单行 JSON —— `install.sh` 已在写入前自行压平（T-0424 根修），`scripts/lm-env-compact.sh` 保留作事后修复；
3. Ubuntu/Debian `logrotate` 需 `su root root`，否则 `/var/log` 属组可写会被整条跳过；
4. 装 `platform.sh` 前先 `mkdir -p /usr/local/libexec/lightmem`。

---

## 7. 遗留 TODO（交回对应线）

| # | 事项 | 归属 | 优先级 |
|---|---|---|---|
| 1 | ~~`install.sh --systemd` 写 env 前压平 JSON（去掉对 `scripts/lm-env-compact.sh` 的人工依赖）~~ **已根修（T-0424，2026-11-21）** | A（T-0411 → T-0424） | P1 |
| 2 | quickstart + systemd 混跑时 `RUN_DIR` 由 root 重建，二进制定为 root 属主；建议装完把 `_lightmem_run` chown 给 `--service-user`（T-0424 已在 0.86 复现硬证据：root 属主下 kernel 报 `attempt to write a readonly database (1544)`，chown 后正常，见 §5.2.1） | A（T-0411） | P2 |
| 3 | systemd 路径下日志去向（journald vs 文件）与 logrotate 口径二选一 | A/C | P2 |
| 4 | `.gitattributes` 给 `deploy/logrotate.d/*`、`deploy/*.sh` 加 `eol=lf`，防 Windows 侧 CRLF 回流（本轮自测时本地写入一度带 CRLF，导致 logrotate 解析失败，已本地纠正、未入库） | 仓库级 | P3 |
| 5 | `hive doctor` 核对：本轮 T-0410/T-0411/T-0413/T-0415 的新增留言与状态 | 收口人 | P2 |

---

## 8. 复现路径（一条命令序列）

```sh
# 0) 验收机
ssh workbuddy@192.168.0.86        # Ubuntu 24.04.4，Go 1.27.4 已装于 /usr/local/go
git clone http://192.168.1.5:3000/skywalk/lightmem.git ~/lightmem
cd ~/lightmem && git checkout 56df3c5

# 1) 非 root 安装 + 探针
export GOPROXY=https://goproxy.cn,direct
sh integrations/quickstart/install.sh --profile solo
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:7443/healthz   # 200

# 2) systemd（root）
sudo env PATH=/usr/local/bin:/usr/bin:/bin GOPROXY=https://goproxy.cn,direct \
     sh integrations/quickstart/install.sh --profile solo --systemd --service-user workbuddy
sudo sh scripts/lm-env-compact.sh "$PWD/_lightmem_run"     # §5.2 规避
sudo systemctl restart lightmem.service lightmem-mcp.service

# 3) 看门狗
sudo mkdir -p /usr/local/libexec/lightmem
sudo install -m 555 deploy/lightmem-watchdog.sh /usr/local/bin/
sudo install -m 644 deploy/platform.sh /usr/local/libexec/lightmem/
sudo install -m 644 deploy/lightmem-watchdog.{service,timer} /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now lightmem-watchdog.timer

# 4) logrotate
sudo install -m 644 deploy/logrotate.d/lightmem /etc/logrotate.d/lightmem
sudo logrotate -d /etc/logrotate.d/lightmem

# 5) E 线门禁
sh scripts/ci-linux-smoke.sh
```
