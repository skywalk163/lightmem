# 日志轮转：Linux vs FreeBSD（T-0413 / T-0426 / R29 L-257）

lightmem 的两份日志 `/var/log/lightmem.log`、`/var/log/lightmem-mcp.log` 按日轮转、保留 7 份（≈ 一周）、
旧档 gzip。两个平台的机制不同，配置各放一份、互不覆盖：

- Linux：logrotate，配置在 `deploy/logrotate.d/lightmem` → 安装到 `/etc/logrotate.d/lightmem`
- FreeBSD：newsyslog，配置在 `deploy/newsyslog.d/lightmem.conf` → 安装到 `/usr/local/etc/newsyslog.conf.d/lightmem.conf`

## 先回答三个问题（T-0426 定夺：方案甲，显式落文件）

| 问题 | 答案 |
|---|---|
| 日志去哪 | Linux + systemd：`/var/log/lightmem.log`、`/var/log/lightmem-mcp.log`（unit 里 `StandardOutput=append:` / `StandardError=append:`）；journalctl 里只剩 systemd 的生命周期消息，不再有 `[access]` 等业务行。FreeBSD：rc.d 落同名文件。 |
| 谁回收 | 这两个文件由 **logrotate**（Linux）/**newsyslog**（FreeBSD）按日轮转、保留 7 份、旧档 gzip；journald 只管自己的元数据，按 `SystemMaxUse` 回收。 |
| 怎么量 | `ls -l /var/log/lightmem*.log*`、`du -sh /var/log/lightmem*.log*`、`sudo logrotate -d /etc/logrotate.d/lightmem`（只读 dry-run）；journald 侧 `journalctl --disk-usage`。 |

**「30 天后还在不在、占多大、被谁删」**：只剩最近 7 份（第 8 天起最旧一份被 logrotate 删除），
按天压缩后的体积约为 `7 × 单日日志量`；删除者是 logrotate（不是 journald）。

> 背景：R29 L-257 登记「日志无轮转 → 撑盘」。T-0413 补了 Linux 侧配置，但 T-0417 真机验收发现
> unit 没配 `StandardOutput=`，日志全进 journald，`/var/log/lightmem*.log` 根本不存在，
> 本文件只能靠 `missingok` 空转。T-0426 定夺为**方案甲**：unit 显式 append 到文件，
> 本文件从此轮转的是真实日志；journald 只保留兜底口径（见 `deploy/README.md`）。

## 对照表

| 项 | Linux（logrotate） | FreeBSD（newsyslog） |
|---|---|---|
| 配置文件 | `deploy/logrotate.d/lightmem` | `deploy/newsyslog.d/lightmem.conf` |
| 安装位置 | `/etc/logrotate.d/lightmem`（644，root:root） | `/usr/local/etc/newsyslog.conf.d/lightmem.conf`（644） |
| 触发者 | `logrotate.timer`（多数发行版每日一次）或 cron | `newsyslog` 由 cron 每分钟检查、到点才转 |
| 周期 | `daily` | `when=@T00`（每日 00:00） |
| 保留份数 | `rotate 7` | `count=7` |
| 压缩 | `compress` + `delaycompress` | `flags=Z`（轮转后 gzip） |
| 文件不存在 | `missingok`（不报错） | 无对应项，不存在即不处理 |
| 空文件 | `notifempty`（跳过） | 无对应项，size=* 时按时间照转 |
| 进程继续写 | `copytruncate`（原地截断，不依赖 reopen） | 省略 pidfile/signal，由 daemon 自身配合 |
| 自检 | `logrotate -d /etc/logrotate.d/lightmem` | `newsyslog -nv`（或 `-v` 手工跑一次） |

口径一致的部分：**按日 + 保留 7 份 + 压缩 + 只轮转这两个文件**。差异只在机制与两个保守选项
（Linux 侧用 `missingok`/`notifempty`/`copytruncate`，理由见下）。

## 为什么是 copytruncate（与 unit 的 append 怎么配合）

`lightmem-kernel` 与 `lightmem-mcp` 目前都没有实现「收到信号后重开日志文件」，所以不能用
`create` + `postrotate` + `kill -HUP` 那套；`copytruncate` 先拷贝再原地截断，inode 不变，
进程继续写同一个 inode，不需要服务配合，代价只是轮转瞬间可能丢几行。

与 unit 的 `StandardOutput=append:` 并不冲突（T-0426 实测）：

- `append:` 以 `O_APPEND` 打开，每次 write 都自动定位到**当前**文件末尾；
- `copytruncate` 不换 inode，只把长度截为 0，fd 依然有效；
- 轮转后下一次写入落在新末尾，文件继续增长（实测 `du == size`，无 NUL 空洞）。

反过来，如果改成 `create`（rename + 新建），`append:` 持有的 fd 会继续指向旧 inode：
日志被写进 `.1`，新文件不再增长。**所以本配置必须保留 `copytruncate`。**

`missingok`：T-0426 之后正常部署里文件一定存在（systemd 启动时就创建），保留它是为了兼容
两种过渡场景：升级前装的老 unit（仍走 journald）、以及服务首次启动前就跑的 logrotate。
没有它，这两种场景每晚都会刷 `error: stat ... No such file or directory`。

`su root root`（T-0417 补）：Ubuntu/Debian 的 `/var/log` 是 `root:syslog 775`，属组可写，
logrotate 会判定「parent directory has insecure permissions」并整条跳过。`StandardOutput=`
创建的文件是 `root:root 0644`，用 `su root root` 轮转不受服务用户影响。

## 配置内容

```conf
/var/log/lightmem.log /var/log/lightmem-mcp.log {
    su root root
    daily
    rotate 7
    compress
    delaycompress
    missingok
    notifempty
    copytruncate
}
```

日志路径口径由 unit 与服务端决定，本文件只轮转、不改路径（`kernel/` 侧与 rc.d/systemd 的路径不动）。

## 安装

推荐走已冻结的 `deploy/platform.sh` 接口（会按 OS 自动选目标，`LM_DRY_RUN=1` 可先看计划）：

```sh
. deploy/platform.sh
lm_logrotate_install deploy/logrotate.d/lightmem     # Linux → /etc/logrotate.d/lightmem
```

> **必须带 `src` 参数**：`lm_logrotate_install` 无参数时的内置兜底规则没有 `su root root`，
> 在 Debian/Ubuntu 上会被父目录权限检查整条跳过（`logrotate -f` rc=1）。这是 platform.sh
> 侧的遗留项，不在 T-0426 的改动范围；照上面带参数调用即可。

手工等价命令：

```sh
# Linux
sudo install -m 644 deploy/logrotate.d/lightmem /etc/logrotate.d/lightmem
# FreeBSD（另一份文件，两平台并存互不影响）
sudo install -m 644 deploy/newsyslog.d/lightmem.conf /usr/local/etc/newsyslog.conf.d/lightmem.conf
```

## 自检

```sh
# 1) 语法与计划：dry-run，只看不转（有错会以非 0 退出并打印原因）
sudo logrotate -d /etc/logrotate.d/lightmem

# 2) 看当天是否该转、轮转后旧档在哪
sudo logrotate -v /etc/logrotate.d/lightmem 2>&1 | tail -20
ls -l /var/log/lightmem.log*

# 3) 手工强制转一次（真转，慎用）
sudo logrotate -f /etc/logrotate.d/lightmem
```

预期：dry-run 输出里能看到 `rotating pattern: /var/log/lightmem.log /var/log/lightmem-mcp.log after 1 days (7 rotations)`，
且没有 `error:` 行。文件不存在时只打印 `does not exist -- skipping`（`missingok` 的正常表现）；
文件存在但为空时 `notifempty` 会跳过 —— **不要用 `touch` 空文件去「验证」轮转**，那只会把空转藏得更深。

## 实测记录

`logrotate 3.21.0`（Ubuntu 24.04，systemd 255）：

- T-0413/T-0417：`-d` 无 `error:`，识别 `after 1 days (7 rotations)`；`-f` 后 `copytruncate`
  生成 `*.log.1`，第二轮 `*.log.1` → `*.log.2.gz`（`delaycompress`），`gzip -t` 通过；
  空文件被 `notifempty` 跳过。
- T-0426（append + copytruncate 联测）：用 `User=workbuddy` + `ProtectSystem=strict` +
  `StandardOutput=append:/var/log/lmtest.log` 的服务实测 —— 文件由 systemd 以 `root:root 0644`
  创建；`logrotate -f` 后 inode 不变（78777）、活动文件从 1214B 截到 24B、`.1` 内容完整；
  3 秒后文件继续增长到 384B，`du -b` 与 `stat` 均为 384（无空洞）；第二次 `-f` 得到
  `.2.gz`。完整记录见任务 T-0426 的 summary。

> AI生成
