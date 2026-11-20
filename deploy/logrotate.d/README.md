# 日志轮转：Linux vs FreeBSD（T-0413 / R29 L-257）

lightmem 的两份日志 `/var/log/lightmem.log`、`/var/log/lightmem-mcp.log` 按日轮转、保留 7 份（≈ 一周）、
旧档 gzip。两个平台的机制不同，配置各放一份、互不覆盖：

- Linux：logrotate，配置在 `deploy/logrotate.d/lightmem` → 安装到 `/etc/logrotate.d/lightmem`
- FreeBSD：newsyslog，配置在 `deploy/newsyslog.d/lightmem.conf` → 安装到 `/usr/local/etc/newsyslog.conf.d/lightmem.conf`

> 背景：R29 L-257 登记「日志无轮转 → 撑盘」，当时只修了 FreeBSD 侧；Linux 没有 newsyslog，
> `deploy/newsyslog.d/` 在 Linux 上形同虚设，本文件补上 Linux 侧。

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

## 为什么是 copytruncate

`lightmem-kernel` 与 `lightmem-mcp` 目前都没有实现「收到信号后重开日志文件」，所以不能用
`create` + `postrotate` + `kill -HUP` 那套；`copytruncate` 先拷贝再原地截断，进程继续写同一个
inode，不需要服务配合，代价只是轮转瞬间可能丢几行。`delaycompress` 与 `copytruncate` 是官方
推荐的搭配：轮转当拍不压、下一拍再压，避免「进程还在写上一份、这边就把它压掉」。

`missingok`：Linux 上服务实际输出常走 journald（systemd 单元未重定向到文件），
`/var/log/lightmem*.log` 可能根本不存在；没有它 logrotate 每晚都会刷
`error: stat of /var/log/lightmem.log failed: No such file or directory`。

## 配置内容

```conf
/var/log/lightmem.log /var/log/lightmem-mcp.log {
    daily
    rotate 7
    compress
    delaycompress
    missingok
    notifempty
    copytruncate
}
```

日志路径口径由服务端决定，本文件只轮转、不改路径（`kernel/` 侧与 rc.d/systemd 的路径不动）。

## 安装

推荐走已冻结的 `deploy/platform.sh` 接口（会按 OS 自动选目标，`LM_DRY_RUN=1` 可先看计划）：

```sh
. deploy/platform.sh
lm_logrotate_install deploy/logrotate.d/lightmem     # Linux → /etc/logrotate.d/lightmem
```

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
且没有 `error:` 行；日志不存在时只打印 `does not exist -- skipping`，属于 `missingok` 的正常表现。

## 实测记录

在 Linux 真机（Ubuntu，`logrotate 3.21.0`）上验证：

- `logrotate -d`（含 200 行真实内容）：无 `error:`，正确识别 `after 1 days (7 rotations)`；
- `logrotate -f`：主日志被 `copytruncate` 截成 0 字节，生成 `*.log.1`；
- 第二轮：`*.log.1` 压成 `*.log.2.gz`（`delaycompress` 生效），`gzip -t` 校验通过；
- 空文件再转：被 `notifempty` 跳过，不产生新档；
- 轮转保留份数由 `rotate 7` 控制，第 8 份起自动清理。

> AI生成
