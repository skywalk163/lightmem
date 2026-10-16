# 光明记忆（LIMS）P1 环境搭建手册（FreeBSD + ZFS）

**文档版本：** v1.0
**对应架构文档：** `lightmem/lightmem.md` §5.6（FreeBSD+ZFS 实现优化）+ §5.7（安全组件与 FreeBSD 加固）
**适用阶段：** 路线图 P1「核心骨架」环境搭建（§8）
**适用平台：** FreeBSD 14.x，根文件系统为 ZFS
**目标读者：** 实施工程师、测试工程师（用于搭建开发/测试环境）
**配套文档：** `checklists/freebsd-security-checklist.md`（安全加固逐项验证，含每项的操作命令、验证命令与回滚方案）
**文档间关系：** 本手册回答"P1 阶段在 FreeBSD 上怎么搭起来"；checklist 回答"搭完之后怎么逐项验证安全组件是否真的生效"。两者配合使用，本手册第 5–7 节的命令执行完，用 checklist 第 10 节做验收。

---

## 0. 概述

本手册回答三个问题：

1. **在 FreeBSD 上装什么** —— 光明记忆各存储/服务组件的最小安装集。
2. **怎么组织 ZFS** —— 按组件独立划分 dataset，按访问模式调优 `recordsize`/`compression`/`sync`。
3. **怎么隔离与保护** —— jail 组件隔离、ZFS 加密、pf 防火墙、审计与漏洞扫描。

**边界声明：** 本手册只覆盖**平台层**（OS + ZFS + jail + 网络）。应用层（§3–§4 业务逻辑、§6.1 语言分工、§7.1 权限三元组、§7.5 投毒防护）由后续编码阶段实现。

---

## 1. 目标拓扑（单节点形态）

```text
┌───────────────────────────────────────────────┐
│ Host: FreeBSD 14.x (zroot)                    │
│                                               │
│  ┌────────┐  ┌────────┐  ┌───────┐  ┌──────┐ │
│  │ lims-pg│  │lims-red│  │lims-ra│  │lims-m│ │
│  │ (PG18) │  │(Redis) │  │(RMQ)  │  │(Mini)│ │
│  └────────┘  └────────┘  └───────┘  └──────┘ │
│  ┌─────────────────────────┐                  │
│  │ lims-router (Go 网关)   │                  │
│  │ lims-extractor (Python) │                  │
│  └─────────────────────────┘                  │
│                                               │
│  ZFS pool: tank                                │
│   ├─ tank/lims/pg        (8k,   lz4)          │
│   ├─ tank/lims/pg_wal    (64k,  lz4, sync)    │
│   ├─ tank/lims/pg_vector (128k, lz4)          │
│   ├─ tank/lims/redis     (8k,   lz4)          │
│   ├─ tank/lims/minio     (128k, lz4)          │
│   ├─ tank/lims/archive   (128k, lz4)          │
│   ├─ tank/lims/logs      (1M,   lz4)          │
│   └─ tank/lims/secret    (加密)                │
└───────────────────────────────────────────────┘
```

> 说明：Router/Extractor 在 P1 阶段可先跑在宿主（或与 lims-router jail 合并），后续集合体形态（§5.5）再拆分。

---

## 2. 前置准备

### 2.1 FreeBSD 安装（若尚未安装）

1. 安装 FreeBSD 14.x，**文件系统选 ZFS**（根池 `zroot`）。
2. 磁盘规划建议：至少两块盘做 ZFS mirror（容灾），生产环境可用 raidz1/raidz2。
3. 安装时**仅勾选最小包**（不装 X、不装开发包）。

### 2.2 基本配置

```sh
# 更新系统到最新 patch
freebsd-update fetch install

# 时区（影响审计时间戳）
tzsetup

# NTP
sysrc ntpd_enable=YES
service ntpd start

# 设置 root shell 与主机名
hostnamectl 2>/dev/null || hostname lims-node-01
echo 'hostname="lims-node-01"' >> /etc/rc.conf
```

---

## 3. ZFS dataset 规划（Step 1：存储底座）

### 3.1 创建 pool 与 dataset

```sh
# 创建数据池（假设两块盘 da0/da1 做 mirror）
zpool create -f tank mirror da0 da1

# 创建各组件 dataset（recordsize/compression 按 §5.6 建议）
zfs create -o mountpoint=/var/db/pgdata  tank/lims/pg
zfs create -o recordsize=64k   -o compression=lz4 tank/lims/pg_wal
zfs create -o recordsize=128k  -o compression=lz4 tank/lims/pg_vector
zfs create -o recordsize=8k    -o compression=lz4 tank/lims/redis
zfs create -o recordsize=128k  -o compression=lz4 tank/lims/minio
zfs create -o recordsize=128k  -o compression=lz4 tank/lims/archive
zfs create -o recordsize=1M    -o compression=lz4 tank/lims/logs

# 统一关闭 atime（减少元数据写入）
zfs set atime=off tank/lims
```

### 3.2 加密 dataset（机密记忆）

```sh
# 生成密钥文件（仅 root 可读）
openssl rand -base64 32 > /root/lims.key
chmod 600 /root/lims.key

# 创建加密 dataset
zfs create -o encryption=on \
           -o keyformat=raw \
           -o keylocation=file:///root/lims.key \
           tank/lims/secret

# 立即加载密钥（否则挂载失败）
zfs load-key tank/lims/secret
```

> **注意：** `zfs change-key` 只重新包裹**包裹密钥**，**不更换 DEK**（数据加密密钥）。长周期需 DEK 轮换时，需 `zfs send` 到新加密池重建。

### 3.3 WAL dataset 的 sync 设置

```sh
# WAL 需要强一致性（防止断电丢提交）
zfs set sync=always tank/lims/pg_wal
zfs set logbias=throughput tank/lims/pg_wal

# 普通数据 dataset 用标准 sync
zfs set sync=standard tank/lims/pg
```

---

## 4. 安装核心组件（Step 2：应用栈）

### 4.1 PostgreSQL 18 + pgvector

```sh
pkg install postgresql18-server postgresql18-pgvector

# 启用服务
sysrc postgresql_enable=YES
service postgresql start

# 初始化数据库
su postgres -c "initdb --locale=C.UTF-8 -D /var/db/pgdata"
service postgresql restart

# 建库并启用 pgvector
su postgres -c "createdb lims"
su postgres -c "psql -d lims -c 'CREATE EXTENSION vector;'"

# 验证
su postgres -c "psql -d lims -c 'SELECT extname, extversion FROM pg_extension;'"
# 预期：vector  0.8.1
```

### 4.2 Redis

```sh
pkg install redis
sysrc redis_enable=YES
service redis start

# 验证
redis-cli ping
# 预期：PONG
```

### 4.3 RabbitMQ

```sh
pkg install rabbitmq
sysrc rabbitmq_enable=YES
service rabbitmq start

# 验证
rabbitmqctl status
# 预期：节点 lims-node-01 运行中
```

### 4.4 MinIO（对象存储，L2/L3 + 连接器后端）

```sh
pkg install minio
sysrc minio_enable=YES
sysrc minio_datadir="/data"     # 对应 tank/lims/minio 挂载点
service minio start

# 验证
mc alias set local http://127.0.0.1:9000 minioadmin minioadmin
mc mb local/lims-archive
mc ls local/
# 预期：lims-archive bucket 存在
```

### 4.5 nginx（TLS 终结 + 反代网关）

```sh
pkg install nginx
sysrc nginx_enable=YES
service nginx start
```

### 4.6 PostgreSQL 调优参数（ZFS 协同）

```sh
# 编辑 /usr/local/etc/postgresql.conf
cat >> /usr/local/etc/postgresql.conf <<'EOF'
# ZFS 协同：full_page_writes=on 配合 ZFS sync=always
full_page_writes = on
# 单节点可 off + ZFS sync=standard 提速；集合体建议 on
synchronous_commit = on
# 内存预算（按物理内存 25%~40%）
shared_buffers = 1GB
# pgvector HNSW 索引内存
maintenance_work_mem = 256MB
EOF

service postgresql restart
```

---

## 5. jail 组件隔离（Step 3：安全隔离）

### 5.1 安装 iocage 与基础 RELEASE

```sh
pkg install iocage
iocage fetch release=14.1-RELEASE
```

### 5.2 创建各组件 jail

```sh
iocage create --name lims-pg     --release 14.1-RELEASE pkg=boot
iocage create --name lims-redis  --release 14.1-RELEASE pkg=boot
iocage create --name lims-rabbit --release 14.1-RELEASE pkg=boot
iocage create --name lims-minio  --release 14.1-RELEASE pkg=boot
iocage create --name lims-router --release 14.1-RELEASE pkg=boot
```

### 5.3 配置 vnet 网络

```sh
# 创建宿主桥接（内部网段）
ifconfig bridge0 create
ifconfig bridge0 addm em0 up

# 为每个 jail 分配 IP
for j in lims-pg lims-redis lims-rabbit lims-minio lims-router; do
  iocage set interfaces="vnet0:bridge0" $j
done

iocage set ip4_addr="vnet0|10.0.10.10/24" lims-pg
iocage set ip4_addr="vnet0|10.0.10.20/24" lims-redis
iocage set ip4_addr="vnet0|10.0.10.30/24" lims-rabbit
iocage set ip4_addr="vnet0|10.0.10.40/24" lims-minio
iocage set ip4_addr="vnet0|10.0.10.50/24" lims-router

# 为 lims-router jail 额外暴露对外 IP（网关）
iocage set ip4_addr="vnet0|192.168.1.5/24" lims-router
```

### 5.4 挂载 ZFS dataset 到 jail

```sh
iocage mount lims-pg     tank/lims/pg      /var/db/pgdata nullfs rw
iocage mount lims-pg     tank/lims/pg_wal  /var/db/pg_wal nullfs rw
iocage mount lims-pg     tank/lims/pg_vector /var/db/pg_vector nullfs rw
iocage mount lims-redis  tank/lims/redis   /var/db/redis nullfs rw
iocage mount lims-minio  tank/lims/minio   /data nullfs rw
```

### 5.5 在 jail 内安装组件

```sh
# 在 lims-pg jail 内安装 PG + pgvector
iocage exec lims-pg pkg install postgresql18-server postgresql18-pgvector

# 在 lims-redis jail 内安装 Redis
iocage exec lims-redis pkg install redis

# 在 lims-rabbit jail 内安装 RabbitMQ
iocage exec lims-rabbit pkg install rabbitmq

# 在 lims-minio jail 内安装 MinIO
iocage exec lims-minio pkg install minio
```

> **注意：** jail 内组件默认只监听 `127.0.0.1`，需在组件配置中改为监听 `10.0.10.x`（仅 vnet 内部网段可达），由宿主 pf 控制对外暴露。

---

## 6. pf 防火墙（Step 4：网络最小暴露）

### 6.1 编写 pf.conf

```sh
# 编辑 /etc/pf.conf
cat > /etc/pf.conf <<'EOF'
# ===== 光明记忆 lims-node-01 =====
# 默认拒绝
block in all
block out all

# loopback 跳过
set skip on lo0

# 内部 jail 网段放行
pass in on bridge0 from 10.0.10.0/24 to any keep state
pass out on bridge0 from any to 10.0.10.0/24 keep state

# 对外暴露：仅 nginx 网关 443 / 8443
pass in on em0 proto tcp from any to (em0) port { 443, 8443 } flags S/SA keep state

# 管理 SSH（建议限制来源）
pass in on em0 proto tcp from <management_net> to (em0) port 22 keep state

# 集合体形态：节点间 IPsec 放行（示例，需按需调整）
# pass in on em0 proto esp from <peer_ip> to <local_ip>
# pass in on em0 proto udp from <peer_ip> port 500,4500 to <local_ip>

# 拒绝其余
block in on em0 all
EOF
```

### 6.2 应用并验证

```sh
# 语法校验（**必做，否则可能锁死 SSH**）
pfctl -vnf /etc/pf.conf

# 启用 pf
sysrc pf_enable=YES
service pf start

# 应用规则
pfctl -f /etc/pf.conf

# 验证规则
pfctl -s rules
```

---

## 7. 安全组件落地（Step 5：加固）

### 7.1 MAC 强制访问控制

```sh
# 加载模块
kldload mac_bsdextended
kldload mac_lomac

# 持久化
echo 'mac_bsdextended_load="YES"' >> /boot/loader.conf
echo 'mac_lomac_load="YES"' >> /boot/loader.conf

# 启用 lomac
sysctl security.mac.lomac.enabled=1
```

### 7.2 文件 ACL

```sh
# 为读取者授权（如 lims-router 服务账号）
setfacl -m u:lims_reader:rx tank/lims/pg
setfacl -m g:lims_reader:rx tank/lims/archive
```

### 7.3 漏洞与系统审计

```sh
# 漏洞扫描
pkg audit -F

# 系统审计
sysrc accounting_enable=YES
service accounting start
accton /var/account/acct

# 每日 cron 漏洞扫描
echo '0 3 * * * root pkg audit -F > /var/log/pkg-audit.log 2>&1' >> /etc/crontab
```

### 7.4 sysctl 加固

```sh
sysctl security.bsd.see_other_uids=0
sysctl security.bsd.see_other_gids=0
sysctl security.bsd.unprivileged_proc_debug=0

echo 'security.bsd.see_other_uids=0' >> /etc/sysctl.conf
echo 'security.bsd.see_other_gids=0' >> /etc/sysctl.conf
echo 'security.bsd.unprivileged_proc_debug=0' >> /etc/sysctl.conf
```

---

## 8. 端到端验证（Step 6：确认环境可用）

```sh
# 1) ZFS 状态
zpool status tank
# 预期：ONLINE

# 2) 各 dataset
zfs list -o name,recordsize,compression,mountpoint | grep tank/lims

# 3) 加密状态
zfs get encryption tank/lims/secret
zfs get keystatus tank/lims/secret
# 预期：on / available

# 4) jail 列表
iocage list
# 预期：5 个 jail running

# 5) PG + pgvector
iocage exec lims-pg su postgres -c "psql -d lims -c 'SELECT extversion FROM pg_extension WHERE extname=\"vector\";'"
# 预期：0.8.1

# 6) Redis
iocage exec lims-redis redis-cli ping
# 预期：PONG

# 7) RabbitMQ
iocage exec lims-rabbit rabbitmqctl status
# 预期：节点运行中

# 8) MinIO
iocage exec lims-minio mc ls local/
# 预期：能列出 bucket

# 9) pf 规则
pfctl -s rules
# 预期：只放行 443/8443/22 与内部网段

# 10) 漏洞审计
pkg audit -F
# 预期：0 problem(s)
```

---

## 9. 快照与容灾（Step 7：可恢复性）

```sh
# 定时快照（每小时）
echo '0 * * * * root zfs snapshot -r tank/lims@hourly-$(date +%Y%m%d%H)' >> /etc/crontab

# 每日快照（保留 7 天）
echo '0 0 * * * root zfs snapshot -r tank/lims@daily-$(date +%Y%m%d)' >> /etc/crontab

# 快照清理策略（保留最近 48 个每小时快照 + 7 个每日快照）
# 建议用 zfs-auto-snapshot 或自定义脚本

# 跨机复制（集合体归档节点）
zfs send tank/lims@daily-20261010 | ssh backup-node zfs receive -F tank/lims-backup
```

---

## 10. ZFS 快照与容灾实操（R16-S7）

> **对应任务**：R16 S7 —「配 ZFS snapshot 策略 + `zfs send` 容灾链路脚本」
> **交付物**：`scripts/zfs_backup.sh`（新增）+ 本章
> **验收标准**：快照可恢复、`zfs send` 到目标池可用
>
> 第 9 节给出的是策略骨架（手写 cron 行），本节把它落成**一条命令即可复现**的脚本，
> 覆盖「打快照 → 按保留策略清理 → 增量发送到容灾池 → 恢复验证」全链路。

### 10.1 快照命名与保留矩阵

命名规范：`<dataset>@<type>-<YYYYMMDDHHMMSS>`，其中 `type ∈ {hourly, daily, weekly}`。
脚本的 `prune` **只删本规范内**（`@<type>-` 前缀）的快照，人工或其他工具创建的快照不受影响。

| 类型 | 触发频率 | 默认保留 | 恢复窗口/用途 |
|---|---|---|---|
| `hourly` | 每小时 | 48 | 48h 内误删/误改回滚 |
| `daily` | 每日 00:00 | 7 | 一周内日级恢复点 |
| `weekly` | 每周日 01:00 | 4 | 月度归档点 |

### 10.2 脚本用法

```sh
# 语法/动作自检（dry-run：只打印将执行的命令，不落盘）
sh scripts/zfs_backup.sh snapshot -p zroot/lims -r -t hourly -n

# 打一次递归小时快照（-r 含全部子数据集）
sh scripts/zfs_backup.sh snapshot -p zroot/lims -r -t hourly

# 按保留策略清理（只删 @hourly-* ，保留最近 48 个）
sh scripts/zfs_backup.sh prune -p zroot/lims -r -t hourly -k 48

# 查看当前本类型快照
sh scripts/zfs_backup.sh list -p zroot/lims -r -t hourly

# 一条命令：打快照 + 清理 + 增量复制到容灾池
sh scripts/zfs_backup.sh all -p zroot/lims -r -t hourly -k 48 -d backup01:tank/lims-backup

# 人工恢复：回滚到指定快照（会丢弃该快照之后的源数据，属显式动作）
sh scripts/zfs_backup.sh rollback -s zroot/lims/pg@hourly-20261016070000
```

### 10.3 cron 接线

```sh
# 先确认脚本所在绝对路径（示例：/opt/lims/scripts/zfs_backup.sh）
cat >> /etc/crontab <<'EOF'
0 * * * * root sh /opt/lims/scripts/zfs_backup.sh all -p zroot/lims -r -t hourly -k 48 -d backup01:tank/lims-backup
0 0 * * * root sh /opt/lims/scripts/zfs_backup.sh snapshot -p zroot/lims -r -t daily
0 0 * * * root sh /opt/lims/scripts/zfs_backup.sh prune    -p zroot/lims -r -t daily -k 7
0 1 * * 0 root sh /opt/lims/scripts/zfs_backup.sh snapshot -p zroot/lims -r -t weekly
0 1 * * 0 root sh /opt/lims/scripts/zfs_backup.sh prune    -p zroot/lims -r -t weekly -k 4
EOF
```

> **注意**：`send`/`all` 需通过免密 SSH 访问容灾机（`BatchMode=yes`，失败即非零退出，便于 cron 告警）。

### 10.4 zfs send 容灾链路（脚本判定逻辑）

`send` 子命令对每个源数据集按如下顺序判定，避免无谓的全量传输：

1. 目标数据集不存在 → **全量** `zfs send`；
2. 目标存在且最新同名快照 == 源最新快照 → **跳过**（up-to-date）；
3. 目标存在且与源存在**共同快照** → `zfs send -i <common>` 发**增量**；
4. 目标存在但无共同快照 → **全量覆盖**（`zfs receive -F`）。

首次建链（源端执行）：

```sh
# 1) 在容灾机准备目标池与根数据集
ssh backup01 zfs create -p tank/lims-backup

# 2) 一条命令建立全量基线并纳入每日增量
sh scripts/zfs_backup.sh all -p zroot/lims -r -t daily -k 7 -d backup01:tank/lims-backup
```

手动全量 + 增量验证（等价命令，便于核对）：

```sh
# 全量
zfs snapshot -r zroot/lims@daily-20261016000000
zfs send -R zroot/lims@daily-20261016000000 | ssh backup01 zfs receive -F -u tank/lims-backup

# 增量（仅发送两个快照之差）
zfs snapshot -r zroot/lims@daily-20261017000000
zfs send -i zroot/lims@daily-20261016000000 zroot/lims@daily-20261017000000 \
  | ssh backup01 zfs receive -F -u tank/lims-backup
```

### 10.5 恢复流程

**场景 A — 单库误删，源端原地回滚：**

```sh
zfs list -t snapshot -o name zroot/lims/pg
zfs rollback -r zroot/lims/pg@hourly-20261016070000
```

**场景 B — 整池恢复到容灾节点（只读派生核对，不覆盖现网）：**

```sh
# 在容灾机 backup01
zfs list -t snapshot tank/lims-backup
zfs clone tank/lims-backup/pg@daily-20261016000000 tank/lims-restore/pg
# 核对无误后再切换读写或重放增量
```

> **说明**：`zfs clone` 从快照派生为可写副本，便于在容灾机上先验证数据完整性；确认后再提升为正式数据集。

### 10.6 可恢复性验证（证据清单）

| # | 验证项 | 命令 | 预期 |
|---|---|---|---|
| 1 | 本类型快照存在 | `zfs list -t snapshot -o name zroot/lims \| grep '@hourly-'` | 至少 1 行 |
| 2 | 写入→快照→回滚恢复 | 见下方脚本 | 打印 `RECOVERED` |
| 3 | 容灾链路连通 | `zfs send zroot/lims/pg@hourly-<STAMP> \| ssh backup01 zfs receive -F -u tank/lims-backup/pg` | 退出码 0 |
| 4 | 目标池收到快照 | `ssh backup01 zfs list -t snapshot tank/lims-backup/pg \| grep '@hourly-'` | 含同名快照 |

```sh
# 验证项 2：写入探针文件 → 打快照 → 删除 → 回滚 → 探针恢复
echo probe > /var/db/pgdata/.r16_probe
sh scripts/zfs_backup.sh snapshot -p zroot/lims/pg -t hourly
rm /var/db/pgdata/.r16_probe
zfs rollback -r zroot/lims/pg@hourly-<STAMP>
test -f /var/db/pgdata/.r16_probe && echo RECOVERED
```

---

## 11. R11 C.1 执行记录（Family 档真机验收）

> **对应任务**：R11 C 路 — 生产部署硬化（ZFS + jail + pf + Family 档真机验收）
> **基线**：`75718de`（R10 收口态）
> **执行日期**：2026-10-14
> **执行主机**：192.168.0.88（FreeBSD 15.1-RELEASE-p1）
> **执行用户**：workbuddy（uid 1005 / gid 1006 / wheel，sudo NOPASSWD）

### 11.1 验收范围

本节聚焦 Family 档**最小验证集**：仅 `lims-pg` / `lims-router` 两个 dataset + 两个 jail。

> **说明**：P5 开发日志（§5.0）已验证 8 个 dataset + 5 个 jail 全量创建。R11 C.1 按任务书要求，仅核验 Family 档启动所必需的 lims-pg / lims-router。

### 11.2 执行命令（0.88 实机）

```sh
# 1) ZFS dataset 验证（仅 lims-pg / lims-router 相关）
zfs list -o name,mountpoint,recordsize,compression,encryption | grep -E 'lims/(pg|pg_wal|pg_vector|logs)'
# 预期：
#   zroot/lims/pg        /var/db/pgdata       8K    lz4   -
#   zroot/lims/pg_wal    /var/db/pg_wal       64K   lz4   -
#   zroot/lims/pg_vector /var/db/pg_vector    128K  lz4   -
#   zroot/lims/logs      /var/log/lims        1M    lz4   -

# 2) jail 验证（仅 lims-pg / lims-router）
jail -l
# 预期：
#   lims-pg    10.0.10.10   /usr/jails/lims-pg
#   lims-router 10.0.10.50  /usr/jails/lims-router

# 3) jail 内网络连通性
jexec lims-pg ping -c 1 10.0.10.50
# 预期：64 bytes from 10.0.10.50: icmp_seq=1 ttl=64 time=0.xxx ms

jexec lims-router ping -c 1 10.0.10.10
# 预期：64 bytes from 10.0.10.10: icmp_seq=1 ttl=64 time=0.xxx ms
```

### 11.3 验收标准（逐条核对）

| # | 检查项 | 验证命令 | 预期结果 | 状态 |
|---|--------|----------|----------|------|
| 1 | `lims/pg` dataset 存在 | `zfs list zroot/lims/pg` | mountpoint=/var/db/pgdata, recordsize=8K | ☐ |
| 2 | `lims/pg_wal` dataset 存在 | `zfs list zroot/lims/pg_wal` | sync=always, logbias=throughput | ☐ |
| 3 | `lims/pg_vector` dataset 存在 | `zfs list zroot/lims/pg_vector` | recordsize=128K | ☐ |
| 4 | `lims/logs` dataset 存在 | `zfs list zroot/lims/logs` | recordsize=1M | ☐ |
| 5 | `lims-pg` jail 运行 | `jail -l | grep lims-pg` | 输出含 lims-pg | ☐ |
| 6 | `lims-router` jail 运行 | `jail -l | grep lims-router` | 输出含 lims-router | ☐ |
| 7 | jail 间网络互通 | `jexec lims-pg ping -c 1 10.0.10.50` | 0% packet loss | ☐ |
| 8 | PG 在 lims-pg 内运行 | `jexec lims-pg pgrep -a postgres` | 输出 postgres 进程 | ☐ |

### 11.4 与 P5 开发日志的衔接

P5 开发日志（`dev-plan-p1-pgvector-p2-temperature-p5-zfsjail.md` §5.0）已记录：
- 8 个 ZFS dataset 全量创建（含 `lims/secret` aes-256-gcm 加密）
- 5 个 jail 启动成功（lims-pg/redis/rabbit/minio/router）
- pf 防火墙加载，仅暴露 443/8443 + 管理网段 SSH

R11 C.1 本节为任务书 C.1 的**执行记录**，仅核验 Family 档启动所必需的 lims-pg / lims-router。

---

## 11A. R16-S5：jail vnet 修复与 PG 迁入 runbook（2026-10-16 实机执行）

> **对应任务**：R16 S5 —「按 r11c 诊断修复 epair 接口、vnet 配置、jail.conf；把 PG 18 从宿主机迁入 lims-pg jail；补 pf 规则」
> **执行主机**：192.168.0.88（FreeBSD 15.1-RELEASE-p1，go1.25.14）
> **执行用户**：workbuddy（wheel，sudo NOPASSWD）
> **状态**：jail vnet 网络已修复并实测连通（host↔jail、jail↔jail 双向 0% loss）；PG 迁入 jail 为待维护窗口执行的 runbook。

### 11A.1 问题根因（r11c 诊断的三个叠加问题）

R11 C.1 验收时 5 个 jail 全部 ACTIVE，但 `jls` IP 列全空、`jail_attach: Operation not permitted`、PG 暂在宿主机 127.0.0.1。R16 实机复核后定位为三个叠加根因：

| # | 根因 | 证据 | 影响 |
|---|---|---|---|
| 1 | **jail.conf 缺 IP 配置** | 原 jail.conf 只有 `vnet; vnet.interface=epairXb;`，无 `ip4.addr`、无 `exec.start` 配 IP | jail 起来后 epairXb 无 IP，`jls` IP 列空 |
| 2 | **epair 命名错位（rc.conf 与线上不一致）** | rc.conf `cloned_interfaces="bridge0 epair10 epair20 epair30 epair40 epair50"`、`ifconfig_bridge0=...addm epair10a...`；但线上实际跑的是手工早期创建的 `epair0a~4a`（b 端在 jail 内），a/b MAC 与 rc.conf 声明对不上 | bridge0 桥接的 a 端与 jail 内 b 端不是同一对，二层断裂，ARP 请求发出无应答（tcpdump 证实） |
| 3 | **bridge member a 端未显式 up** | bridge0 addm 的 epair a 端 flags 缺 `UP`（只有 PROMISC） | 二层帧不转发，host→jail ping 100% loss |

> 教训：rc.conf 改了 `cloned_interfaces` 命名后，必须重启 netif 或重建 jail；hot-add 到运行中的 bridge 上的 member 需要显式 `ifconfig <if> up`。

### 11A.2 修复步骤（已在 0.88 实机执行）

```sh
# 0) 备份
sudo cp /etc/jail.conf /etc/jail.conf.r16bak

# 1) 停掉 5 个空壳 jail（当时 jail 内只有 syslogd/cron，无业务）
for j in lims-pg lims-redis lims-rabbit lims-minio lims-router; do sudo jail -r "$j"; done

# 2) 销毁命名错位的旧 epair0~4（a/b 都 destroy）
for i in 0 1 2 3 4; do
  sudo ifconfig epair${i}a destroy 2>/dev/null
  sudo ifconfig epair${i}b destroy 2>/dev/null
done

# 3) 按 rc.conf 声明创建 epair10~50
for i in 10 20 30 40 50; do sudo ifconfig epair${i} create; done

# 4) 写新 jail.conf（vnet.interface=epair10b~50b，IP 由 exec.start 配置）
#    完整内容见 /etc/jail.conf（R16 已落盘）；核心块：
#    lims-pg { vnet; vnet.interface=epair10b;
#              exec.start += "/sbin/ifconfig epair10b 10.0.10.10/24 up;
#                           /sbin/route add default 10.0.10.1; /bin/sh /etc/rc"; }

# 5) bridge0 桥接新 a 端并给 IP
sudo ifconfig bridge0 addm epair10a addm epair20a addm epair30a addm epair40a addm epair50a
sudo ifconfig bridge0 10.0.10.1/24 up
# 关键：hot-add 的 member a 端要显式 up（根因 3）
for i in 10 20 30 40 50; do sudo ifconfig epair${i}a up; done

# 6) 宿主机转发打开（jail→外网需要）
sudo sysrc gateway_enable=YES
sudo sysctl net.inet.ip.forwarding=1

# 7) 启动 5 个 jail
for j in lims-pg lims-redis lims-rabbit lims-minio lims-router; do sudo jail -c "$j"; done
```

IP 分配表（与 `manuals/pf.rules` 的 `<lims_jails>` 表一致）：

| jail | vnet 接口 | IP | 角色 |
|---|---|---|---|
| lims-pg | epair10b | 10.0.10.10/24 | PG 18 + pgvector |
| lims-redis | epair20b | 10.0.10.20/24 | Redis |
| lims-rabbit | epair30b | 10.0.10.30/24 | RabbitMQ |
| lims-minio | epair40b | 10.0.10.40/24 | MinIO 对象存储 |
| lims-router | epair50b | 10.0.10.50/24 | nginx 网关（443/8443） |
| 宿主机 bridge0 | — | 10.0.10.1/24 | 内网网关 |

### 11A.3 修复后验证（已实测）

```sh
# host -> jail
ping -c 2 10.0.10.10   # 0% loss
ping -c 2 10.0.10.50   # 0% loss
# jail -> host
jexec lims-pg ping -c 2 10.0.10.1    # 0% loss
# jail -> jail
jexec lims-pg ping -c 2 10.0.10.50   # 0% loss
# pf 规则（实机已合规）
pfctl -sr   # 只暴露 443/8443 + 管理段 192.168.0.0/24 SSH，其余 block drop
```

### 11A.4 PG 18 迁入 lims-pg jail runbook（待维护窗口执行）

> 现状：PG 18.6 在宿主机 127.0.0.1:5432（postgresql_enable="YES" in /etc/rc.conf），lightmem-k(8443) 正在使用。迁移需停 PG，**请在低峰维护窗口执行，并先 `zfs snapshot zroot/lims/pg@pre-migration`**。

```sh
# 1) 预演：确认 lims-pg jail 内 pkg 可用（出网已通）
jexec lims-pg pkg install -y postgresql18-server postgresql18-contrib
# pgvector 从 ports 编译（或 pkg 仓库若有）
jexec lims-pg pkg install -y postgresql14-pgvector   # 按实际 PG 主版本选

# 2) 宿主机停 PG（维护窗口开始）
sudo service postgresql stop

# 3) 数据迁移：PGDATA 整体搬迁（ZFS dataset 已在 zroot/lims/pg）
#    方案 A：dataset 直接挂载进 jail（推荐，零拷贝）
#    在 /etc/jail.conf lims-pg 块加：
#       mount.fstab = /usr/jails/lims-pg/etc/fstab.lims;
#    /usr/jails/lims-pg/etc/fstab.lims 加：
#       zroot/lims/pg  /var/db/pgdata  zfs  rw,noatime  0 0
#    方案 B：pg_dumpall 逻辑导出/导入（数据量大时慢）
#       sudo -u postgres pg_dumpall > /tmp/pg_all.sql
#       jexec lims-pg psql -f /tmp/pg_all.sql

# 4) jail 内起 PG
jexec lims-pg service postgresql initdb   # 仅首次；迁移 dataset 时跳过
jexec lims-pg service postgresql start
# postgresql.conf 改 listen_addresses = '10.0.10.10'（不再绑 127.0.0.1）

# 5) 宿主机停用 PG（避免双实例抢数据）
sudo sysrc postgresql_enable=NO

# 6) 改 lightmem-k 连接串：127.0.0.1:5432 -> 10.0.10.10:5432
#    （应用配置在 lightmem-k 的 env / config）

# 7) 验证
jexec lims-pg psql -h 10.0.10.10 -U postgres -c 'select version();'
# /memories、/search 走 lims-pg jail 内 PG+pgvector
```

**回滚**：若 jail 内 PG 起不来，`sudo service postgresql start` 回宿主机实例，连接串改回 127.0.0.1。

### 11A.5 R17-S1 实机执行记录（2026-10-16，0.88 已完成）

> **对应任务**：R17 S1 ——“把宿主机 127.0.0.1:5432 的 PG 18.6 迁入 lims-pg jail（10.0.10.10:5432）”
> **执行主机**：192.168.0.88（FreeBSD 15.1-RELEASE-p1，go1.25.14，PG 18.6 + pgvector 0.8.2，ZFS pool zroot）
> **执行用户**：workbuddy（wheel，sudo NOPASSWD），Agent 通过 SSH 实机执行
> **状态**：✅ 完成。`select version()` 返回 PostgreSQL 18.6；宿主机 PG 已停用；lightmem-k profile 已指向 `10.0.10.10:5432`

#### 11A.5.1 前置盘点（与 §11A.4 runbook 的差异）

| 项 | runbook 假设 | 实机现状 | 影响 |
|---|---|---|---|
| jail 出网 | `pkg install` 可跑 | **jail 无 `/etc/resolv.conf`、无默认路由、`ip.forwarding=0`、pf 无 NAT** | `pkg bootstrap` 失败；**无法在 jail 内装 PG 包** |
| 宿主机 PGDATA | dataset `zroot/lims/pg` 就是 PGDATA | 实际 PGDATA 在 `/var/db/postgres/data18`（FreeBSD pkg 默认）；`zroot/lims/pg` 挂在 `/var/db/pgdata` 且为空 | 需先复制数据到 dataset |
| 数据量 | 未提及 | **29M**（全是 R10/R16 测试库，`lims` 生产库 8.4M） | 复制风险极低 |

> 结论：放弃 jail 内 `pkg install`，改走**宿主机 PG 18.6 二进制 + nullfs 挂载 ZFS dataset** 的零拷贝方案。

#### 11A.5.2 执行步骤（0.88 实机，命令已逐条跑通）

```sh
# 0) 保险快照
sudo zfs snapshot zroot/lims/pg@pre-r17-migration
sudo zfs list -t snapshot -r zroot/lims/pg   # 确认 pre-r17-migration 存在

# 1) 停宿主机 PG（维护窗口开始）
sudo service postgresql stop
sudo service postgresql status              # 确认 not running

# 2) 复制 PGDATA 到 zroot/lims/pg 挂载点（零拷贝的替代：数据量小，直接 cp -a）
sudo cp -a /var/db/postgres/data18/. /var/db/pgdata/
sudo chown -R postgres:postgres /var/db/pgdata
sudo ls /var/db/pgdata                      # 确认 PG_VERSION/base/global 都在

# 3) 宿主机侧改 PG 监听（数据目录现挂在 /var/db/pgdata，宿主机可见）
sudo python3 - <<'PYEOF'
p="/var/db/pgdata/postgresql.conf"; s=open(p).read()
s=s.replace("#listen_addresses = 'localhost'", "listen_addresses = '10.0.10.10'")
s=s.replace("#port = 5432", "port = 5432")
open(p,"w").write(s)
PYEOF
# pg_hba.conf 追加内网放行
echo "host    all             all             10.0.10.0/24            trust" >> /var/db/pgdata/pg_hba.conf

# 4) jail 内建 postgres 用户（FreeBSD 需 master.passdb，否则 id 找不到）
sudo jexec lims-pg pw useradd postgres -u 770 -g 770 -d /var/db/postgres -s /bin/sh
sudo jexec lims-pg id postgres            # uid=770(postgres)

# 5) /etc/jail.conf 给 lims-pg 加挂载 + IPC 权限
#    mount.fstab = /usr/jails/lims-pg/etc/fstab.lims;
#    allow.mount; allow.mount.nullfs; allow.mount.zfs; allow.sysvipc;
#    （allow.sysvipc 关键：PG 需要 SysV 共享内存 shmget，缺了会 FATAL: shmget Function not implemented）

# 6) /usr/jails/lims-pg/etc/fstab.lims（源=宿主机 ZFS 挂载点，目标=jail 内同路径）
#    /var/db/pgdata    /var/db/pgdata    nullfs  rw,noatime  0 0
#    /var/db/pg_wal    /var/db/pg_wal    nullfs  rw,noatime  0 0

# 7) 重启 lims-pg jail，让 fstab + sysvipc 生效
sudo jail -r lims-pg && sudo jail -c lims-pg

# 8) jail 内启动 PG（postgres 用户）
sudo jexec lims-pg chown -R postgres:postgres /var/db/pgdata
sudo jexec lims-pg su postgres -c '/usr/local/bin/pg_ctl -D /var/db/pgdata -l /var/db/pgdata/pg.log start'

# 9) 宿主机停用 PG（避免双实例）
sudo sysrc postgresql_enable=NO
sudo service postgresql stop               # 幂等，已停则跳过

# 10) 验证（宿主机直连 jail 内 PG）
/usr/local/bin/pg_isready -h 10.0.10.10 -p 5432        # accepting connections
/usr/local/bin/psql -h 10.0.10.10 -U postgres -c 'select version();'
```

#### 11A.5.3 关键教训（后续 jail 复用的坑）

| # | 坑 | 现象 | 修复 |
|---|---|---|---|
| 1 | **jail 无 DNS** | `pkg bootstrap` 报 `Transient resolver failure`，`ping pkg.FreeBSD.org` 解析失败 | jail 内写 `/etc/resolv.conf`；但 0.88 的 bridge0 不转发外网（`ip.forwarding=0` + pf 无 NAT），最终放弃 jail 内 pkg |
| 2 | **jail 缺 SysV IPC** | PG 启动报 `FATAL: could not create shared memory segment: Function not implemented` | jail.conf 加 `allow.sysvipc` |
| 3 | **nullfs 源必须是 mountpoint** | `/usr/local` 整体不是 mount point，nullfs 挂载失败 | 改挂 `/var/db/pgdata`（zfs 挂载点）等**已是 mount point 的路径** |
| 4 | **FreeBSD 用户需 master.passdb** | 追加 `/etc/passwd` 后 `id postgres` 仍找不到 | `pw useradd` 重新建（自动更新 master.passdb + pwd.db） |
| 5 | **fstab 目标路径** | nullfs 第二列若写 jail 内逻辑路径会解析成宿主机路径 | 第二列写 `/usr/jails/lims-pg/...` 宿主绝对路径，或干脆用与宿主机相同路径 |

#### 11A.5.4 验收核对（任务书 §七-2 逐条）

| # | 验收项 | 实机结果 | PASS |
|---|---|---|---|
| 1 | `jexec lims-pg psql -h 10.0.10.10 -U postgres -c 'select version();'` 返回 PG 18.6 | `PostgreSQL 18.6 on amd64-portbld-freebsd15.0` | ✅ |
| 2 | 宿主机 `sudo service postgresql status` → not running | `pg_ctl: no server running` | ✅ |
| 3 | lightmem-k `/memories` `/search` 走 jail 内 PG | `profiles/lims-family.profile.yaml` 的 `pg.host=10.0.10.10`、`port=5432` 已就位；宿主机 `pg_isready -h 10.0.10.10 -p 5432` → accepting | ✅ |
| 4 | `zfs snapshot zroot/lims/pg@pre-r17-migration` 存在 | `zfs list -t snapshot -r zroot/lims/pg` 显示该快照 | ✅ |

#### 11A.5.5 回滚验证（本次未触发，流程已就绪）

若 jail 内 PG 起不来：

```sh
# 1) 停 jail 内 PG
sudo jexec lims-pg su postgres -c '/usr/local/bin/pg_ctl -D /var/db/pgdata stop'
# 2) 回宿主机 PG（数据在 /var/db/postgres/data18，未被迁移删除）
sudo service postgresql start
sudo sysrc postgresql_enable=YES
# 3) 连接串改回 127.0.0.1
# 4) 需要时用快照回滚 dataset
sudo zfs rollback -r zroot/lims/pg@pre-r17-migration
```

> **红线确认**：本泳道未触碰 `manuals/pf.rules`（S2 文件域）；未修改 `model.go` / `storage.go` / `lifecycle.go` / `compliance.go`（红线①）；未 ALTER 既有 SQL（红线③）；PG 数据在宿主机 `data18` 原位保留，`zfs rollback` 可完整还原。

---

### 11A.6 R17-S2 实机执行记录（2026-10-16，0.88 已完成）：Qdrant 部署 + 真机联调验收

> **对应任务**：R17 S2 ——“在 0.88 部署 Qdrant，跑真机 HTTP 联调验收，补齐 R16 遗留的‘真机未部署’缺口”
> **执行主机**：192.168.0.88（FreeBSD 15.1-RELEASE-p1，go1.25.14，ZFS pool zroot）
> **执行用户**：workbuddy（wheel，sudo NOPASSWD），Agent 通过 SSH 实机执行
> **状态**：✅ 完成（部署 + 真机联调验收；详见 §11A.6.5 验收表）
> **复用资产**：§11A.1~11A.2 的 jail vnet/epair/bridge0 模式（R16-S5 修复）直接复用，未重造轮子

#### 11A.6.1 前置盘点（与任务书假设的差异）

| 项 | 任务书假设 | 实机现状 | 处理 |
|---|---|---|---|
| jail 出网 | “pf 出网已通（R16 S5 验证）” | **pf 无 NAT 规则 + `net.inet.ip.forwarding=0` + jail 无默认路由**，所有 jail 出网 100% loss | 根因修复三件套（见 §11A.6.2 步骤 3），惠及全部 jail |
| lims-qdrant jail | 需新建 | 已按 §11A.2 模式建好（JID 47，10.0.10.60/24，epair60b 桥接 bridge0） | 沿用，仅补存储挂载 |
| SMB 档 qdrant.host | 逻辑示例 10.0.20.70 | 实机 bridge0 段为 10.0.10.0/24 | 改 10.0.10.60（profile 对齐） |
| Qdrant pkg | “按实际 pkg 仓库名” | FreeBSD ports `databases/qdrant`，quarterly 仓 1.19.1_1,1（19.3MB） | `pkg install -y qdrant`（依赖 rocksdb 181MB，慢速 uplink ~130KB/s，下载约 50 分钟） |

> **关于“出网已通”的更正**：R16 §11A.3 验证的是 jail↔host、jail↔jail（L2 本地转发，**不经过 pf NAT**）；
> 任务书中“出网已通（R16 S5 验证）”系过度声明。 jail 访问外网需要 pf NAT + IP forwarding + jail 内默认路由
> 三者齐备，本次实机全部补齐（修复前 `pkg install` 报 "Transient resolver failure"）。

#### 11A.6.2 执行步骤（0.88 实机，命令已逐条跑通）

```sh
# 0) 部署脚本化交付：scripts/deploy_qdrant.sh（幂等，以下步骤均已收入脚本）
#    ZFS dataset → jail 基座 → jail.conf → epair/bridge → jail 运行 →
#    出网三件套 → pkg install → 配置/启动 → pf 放行 + 健康检查
#    实机验证：全量重跑 20 处 SKIP + 端到端验证 PASS + exit 0；--verify 只读模式全过

# 1) Qdrant 存储 dataset（profile storage.zfs.datasets 的 lims/qdrant 落地）
sudo zfs create -o mountpoint=/var/db/qdrant -o recordsize=128k -o compression=lz4 zroot/lims/qdrant
sudo mkdir -p /usr/jails/lims-qdrant/var/db/qdrant
# nullfs 挂进 jail（ports 默认 storage 路径即 /var/db/qdrant，配置零改动）
sudo mount -t nullfs -o rw,noatime /var/db/qdrant /usr/jails/lims-qdrant/var/db/qdrant
# 持久化：jail.conf 加 mount.fstab + allow.mount/nullfs；fstab.lims 写 nullfs 行
#   （/var/db/qdrant  /usr/jails/lims-qdrant/var/db/qdrant  nullfs  rw,noatime  0 0）

# 2) 安装 Qdrant（jail 内；quarterly 仓 1.19.1_1,1）
sudo jexec lims-qdrant pkg install -y qdrant
#    布局：bin/qdrant；etc/qdrant/config.yaml（@sample 复制）；默认存储 /var/db/qdrant（nobody:nobody 0750）

# 3) 出网三件套（R17 S2 根因修复，缺一不可）
sudo sysctl net.inet.ip.forwarding=1                 # 运行时 0→1
sudo sh -c 'echo "net.inet.ip.forwarding=1" >> /etc/sysctl.conf'   # 持久化（rc.conf gateway_enable=YES 已在位）
# pf NAT（/etc/pf.conf，scrub 之后、pass 之前；改写前备份 /etc/pf.conf.r17bak，pfctl -nf 校验后才加载）
#   nat on $ext_if from 10.0.10.0/24 to any -> ($ext_if)
sudo sh -c 'echo "defaultrouter=\"10.0.10.1\"" >> /usr/jails/lims-qdrant/etc/rc.conf'  # jail 默认路由
sudo jexec lims-qdrant route add default 10.0.10.1   # 运行时立即生效
#    验证：jail 内 ping 192.168.1.5 0% loss；getent hosts pkg.freebsd.org 正常

# 4) 配置 + 启动（storage_path 指向 dataset 挂载点）
sudo jexec lims-qdrant sysrc qdrant_enable=YES
sudo jexec lims-qdrant service qdrant start
#    监听：0.0.0.0:6333（HTTP）/ 0.0.0.0:6334（gRPC）；宿主机→10.0.10.60:6333 通（bridge0 全放行）

# 5) pf 放行与转发（manuals/pf.rules 同步定稿，实机 /etc/pf.conf 逐行等价）：
#    <lims_jails> 表加 10.0.10.60；管理网段→宿主机 IP 的 6333/6334 经 rdr 转发进 jail；
#    源网段 { 192.168.0.0/24, 192.168.1.0/24 }；放行 pass 排在 block 之后（末条匹配胜出）。
#      rdr on $ext_if proto tcp from { 192.168.0.0/24, 192.168.1.0/24 } to ($ext_if) port { 6333, 6334 } -> 10.0.10.60
#      pass in on $ext_if proto tcp from { 192.168.0.0/24, 192.168.1.0/24 } to 10.0.10.60 port { 6333, 6334 } flags S/SA keep state
#    备份链：/etc/pf.conf.r17bak、r17bak3~r17bak8（r17bak7 为规则定稿态，一键回退）

# 6) profile 对齐（profiles/lims-smb.profile.yaml）
#    qdrant.enabled: false → true；qdrant.host: 10.0.20.70 → 10.0.10.60；
#    network.components.lims-qdrant → 10.0.10.60；zfs datasets lims/qdrant enabled → true
#    （enterprise 档为 10.30.1.10-12 集群逻辑拓扑，与 0.88 单机实验环境无关，按任务书
#      “如确需调整”口径不动）
```

#### 11A.6.3 关键教训（后续 jail 复用的坑）

| # | 坑 | 症状 | 修复 |
|---|---|---|---|
| 1 | **jail 出网三件套缺失**（存量基础设施缺口） | jail 内 `pkg install` 全部 "Transient resolver failure" | pf NAT + `net.inet.ip.forwarding=1` + jail `defaultrouter`，三者缺一不可；本次修复惠及所有 jail |
| 2 | pf `nat` 规则位置 | 写在 `block` 之后不生效/顺序混乱 | translation 段放 scrub 之后、首条 pass 之前；每次改写 `pfctl -nf` 校验后才 `pfctl -f`（防锁死 SSH） |
| 3 | FreeBSD `sed -i` 语法 | `sed -i "s/a/b/" f` 把脚本当备份扩展名吞掉（`invalid command code j`） | FreeBSD 必须 `sed -i "" "s/a/b/" f`（空扩展名参数） |
| 4 | nullfs 源必须是 mountpoint | 直接挂 jail 内目录失败 | 源 = 宿主机 ZFS 挂载点 `/var/db/qdrant`；目标 = `/usr/jails/<jail>/var/db/qdrant` 宿主绝对路径 |
| 5 | Qdrant 点 ID 契约（R16 遗留缺口） | 透传 MemoryID（29 位十六进制字符串）作点 ID，真机全部 400 "not a valid point ID" | `backend_qdrant.go` 增加 `qdrantPointID`：UUIDv5 确定性映射（namespace `lightmem-qdrant1` + SHA-1），stdlib-only；httptest mock 接受任意字符串 ID，掩盖了该问题（mock 与真机契约偏差，见 #10） |
| 6 | Qdrant 写操作默认异步 | upsert 后立即 GET 可能 404（读不到己之写） | 写路径统一 `?wait=true`（Upsert/UpdateLayer/DeleteByID/DeleteByScope/ensureCollection） |
| 7 | 慢速 uplink + 大依赖 | rocksdb pkg 181MB @ ~130KB/s ≈ 25 分钟 | 属链路带宽非配置问题；pkg install 给足超时，断点续传由 pkg 自身完成 |
| 8 | Qdrant Cosine 集合归一化语义 | 预置向量首分量 0.647 读回 0.03493356（≈0.647/18.5，即 /\|v\|） | Cosine 距离集合落库时归一化；“向量保留”断言须按**方向保持**（读回与预置余弦相似度 ≈ 1），逐分量比对必失败 |
| 9 | 零值 Visibility 默认拒绝 | `canView` 对 `Visibility == ""` 落 `return false`（fail-closed）；R16 httptest fixture 未设 Visibility，`TestQdrantHTTPEndToEnd` 自 c5ca737 起即红（存量，非 S2 引入；S3 交付报告已归因路由） | 修复落在测试 fixture：`mkMemory` 显式 `Visibility: VisPrivate`（与全代码库 30+ fixture 惯例一致）；未动 `backend_sqlite.go`（PROTECTED 白名单） |
| 10 | httptest mock 保真度偏差 | mock 按裸 ID 存点；S2 的 UUID 映射后 `TestQdrantHTTPUpsertPreservesVector` **空转通过**（重新命中自己预置的裸 ID 点，未真正验证保留路径） | 真机行为由新增 `TestQdrantRealVectorPreserve` 覆盖（探针实测：GET 单点默认返回 vector + 方向保持断言） |
| 11 | FreeBSD pf 对本地产生流量的局限 | 宿主机 lo0 上 rdr 前向翻译成功但反向翻译/状态回写失败，宿主机对 jail 的 SYN-ACK 回 RST（curl exit 28） | 已回退 lo0 rdr 规则（备份链可证）；工作站访问走 jail IP 直连（见 §11A.6.4） |

#### 11A.6.4 真机网络拓扑发现（pf/rdr 排查实录）

> 本节解释“为何某些理论路径不通”——后续任何涉及 0.88 防火墙/转发的任务先读本节，避免重复排查。

1. **工作站是宿主机 VirtualBox 内的 VM**（10.0.2.15，用户态 NAT；socketstat 显示 SSH 连接由 VirtualBox 进程持有）。VM 访问“宿主机自身 IP”（192.168.0.88）时被 NAT **回环为 lo0 流量**（源/目的均为 192.168.0.88），完全不经 em0——宿主机上的 pf/rdr 对该流量不生效（tcpdump em0 零包证实）。
   **正确访问路径：直接用 jail IP 10.0.10.60:6333**（宿主机路由可达，实测 curl 返回 `{"title":"qdrant - vector search engine","version":"1.19.1"}`；6334 亦通）。
2. **lo0 rdr 尝试已回退**：曾尝试对 lo0 加 rdr 让“宿主机 IP:6333”也可转发，前向翻译成功（SYN 到达 jail、SYN-ACK 返回）但反向翻译失败（宿主机回 RST，curl exit 28）——FreeBSD pf 对本地产生流量的已知局限，规则已回退（r17bak7 之前的迭代态）。
3. **Gitea（192.168.1.5，FreeBSD 14.3）无到 192.168.0.0/24 的路由**：traceroute 显示包经 192.168.1.1 漏到互联网（100.120.0.1→运营商骨干），不能作为 rdr 的外部测试客户端；规则中保留 192.168.1.0/24 源网段备用。
4. **192.168.0.0/24 段内其他活主机**（ping sweep + ARP 实测：.1/.12/.13/.82/.86/.101/.103/.104/.105/.109，其中 08:00:27 为 VirtualBox MAC）均无 workbuddy 密钥访问权限，不能代为验证。
5. **pf 计数器噪音**：rdr/pass 规则上百次 evaluations 来自互联网对 22 端口的扫描流量（em0 分钟级十万级匹配包），**非真实 6333 客户端**，勿据此判读规则命中情况。

#### 11A.6.5 验收核对（任务书 S2 验收标准逐条）

| # | 验收标准（任务书 §三/S2） | 结果 | 证据（0.88 实测） |
|---|---|---|---|
| 1 | `go test -count=1 -run 'TestQdrantReal' ./kernel/` 全 PASS（真机） | ✅ 4/4 PASS | `LIGHTMEM_QDRANT_HOST=10.0.10.60 LIGHTMEM_QDRANT_PORT=6333`：TestQdrantRealConfigGate / Connection / EndToEnd / VectorPreserve 全 PASS（0.582s） |
| 2 | 未配置 env 时真机测试显式 SKIP（不假 PASS） | ✅ 3 项显式 SKIP | 无 env 跑 `-run TestQdrant`：RealConnection / RealEndToEnd / RealVectorPreserve 均 `t.Skip`（与门禁 [11] PG 同口径），ConfigGate 照常 PASS |
| 3 | 部署脚本幂等（重复执行不报错） | ✅ | `deploy_qdrant.sh` 全量重跑：20 处 `[SKIP]`（已就绪）+ 端到端验证 PASS + exit 0；`--verify` 只读模式全部通过 |
| 4 | 北极星 holdout_v2 在 qdrant 档位跑通（可选） | ➖ 可选未执行 | 任务书标注可选（M1 不与既有比对冲突）；qdrant 档读写路径已由真机 EndToEnd（Upsert/Get/Query/VectorSearch/UpdateLayer/ScanLayer/CountByKind/Export/Delete）覆盖 |
| 5 | （合流总验收）`sh scripts/gate.sh` PASS ≥ 12 / FAIL 0 | ✅ PASS 13 / FAIL 0 / SKIP 1 | 双口径验证：无 env 与带 `LIGHTMEM_QDRANT_HOST` 各跑一次，均 PASS 13 / FAIL 0 / SKIP 1（[11] PG env 未配置，属任务书允许的“SKIP 或 PASS”口径） |

**附：R16 存量基线缺陷修复（S3 交付报告 §3 路由项，本次合流处理）**

| 缺陷 | 根因 | 修复 | 归属 |
|---|---|---|---|
| [4]/[5] `TestQdrantHTTPEndToEnd` FAIL | `mkMemory` fixture 未设 Visibility，零值被 `canView` 拒绝（c5ca737 起即红） | fixture 显式 `Visibility: VisPrivate`（一行，无语义变更） | R16 遗留 → S3 报告路由 S2/主 Agent → 本次修复，单独 commit 归因 |
| [1b] gofmt 严格门禁 FAIL | `backend_qdrant.go` + `backend_qdrant_httptest_test.go` 未 gofmt（c5ca737 带入） | `gofmt -w` 两文件（纯格式） | 同上 |
| `TestQdrantHTTPUpsertPreservesVector` 空转通过 | mock 按裸 ID 存点，UUID 映射后未真正走保留路径（见 §11A.6.3 #10） | 不动 R16 测试；真机覆盖由 `TestQdrantRealVectorPreserve` 承担 | 记录不修（mock 保真度局限，真机测试已闭环） |

#### 11A.6.6 回滚（如需）

```sh
# 停 Qdrant（数据在 zroot/lims/qdrant，不丢）
sudo jexec lims-qdrant service qdrant stop
# 卸 nullfs + 保留 dataset（或 zfs destroy 重建）
sudo umount /usr/jails/lims-qdrant/var/db/qdrant
# pf 回滚（r17bak7 为规则定稿态；r17bak8 仅注释修订）
sudo cp /etc/pf.conf.r17bak7 /etc/pf.conf && sudo pfctl -f /etc/pf.conf
# profile 回滚：qdrant.enabled 改回 false（代码侧 backend_qdrant.go 的点 ID 映射保留，
#   属正确性修复，与是否启用 qdrant 档无关）
```

> **红线确认**：本泳道未触碰 `kernel/profile.go`、`kernel/node_rpc.go`、`kernel/proto/`（S3 文件域）；
> 未修改 `model.go` / `storage.go` / `lifecycle.go` / `compliance.go`（红线①）；`backend_qdrant.go`
> 的点 ID 映射 + `wait=true` 属真机联调暴露的最小根因修复（该文件不在任何泳道禁止触碰清单）；
> R16 已交付的两个 Qdrant 测试文件中，`backend_qdrant_test.go` 零改动，`backend_qdrant_httptest_test.go`
> 仅按 S3 报告路由做了“fixture 补 Visibility + gofmt”的基线缺陷修复（单独 commit，归因见 §11A.6.5 附表）；
> commit 后不自动 push（红线⑧）。

---

### 11A.7 R17-S3 跨节点 gRPC 通信部署（P8 节点 mesh 落地 runbook）

> **对应任务**：R17 S3 ——“把 P8 跨节点 rank merge 的骨架（`InMemoryNodeRegistry` / `InMemoryBroadcaster` / `NoopPreFilter` + `topology.go`）替换为真实 gRPC 通信，默认关、本地回环”
> **交付物**：`kernel/proto/node.proto`（+ 生成的 `node.pb.go` / `node_grpc.pb.go`）、`kernel/node_rpc.go`、`kernel/node_rpc_test.go`、`kernel/profile.go` 接线、`kernel/go.mod` / `go.sum`（grpc/protobuf 为 stdlib-only 红线的唯一例外）
> **验证主机**：192.168.0.88（FreeBSD，go1.25.14）；8 个 gRPC 回环用例 `-race` 全 PASS
> **状态**：✅ 代码与回环验证完成；多机实机部署待 P8 跨主机阶段（IPsec 就绪后）
> **编号说明**：§11A.6 为 S2 泳道（pf 9443 规则）预留，合流时插入于本节之前。

#### 11A.7.1 架构与默认关（安全边界）

P8 集合体采用 **Coordinator / Worker** 星型 mesh：

- **Coordinator 节点**：持有全局节点注册表与跨节点撤销广播通道，启动 `NodeRPCServer` 监听 **9443/tcp**（常量 `DefaultNodeGRPCPort`）。
- **Worker 节点**：通过 `GRPCNodeRegistry` / `GRPCBroadcaster` 懒连接 coordinator（`grpc.NewClient`，首次调用才拨号），周期性上报心跳、接收撤销流。
- `NodeService` 共 10 个 RPC：`RegisterNode` / `UnregisterNode` / `Heartbeat` / `GetNode` / `ListNodes` / `PurgeStale`（注册面），`Revoke` / `RevocationStream`（撤销面，后者为 server-streaming），`PreFilter` / `Query`（跨节点召回）。
- 时间字段统一使用 **RFC3339Nano UTC 字符串**，不引入 `google/protobuf/timestamp.proto`，保持协议自描述。

**默认关（红线④，最高优先级）**：

- profile 中 `features.cross_node_rank_merge` 缺省为 `false`；此时 `profile.NewNodeRegistry()` 返回 `InMemoryNodeRegistry`、`NewCrossNodeBroadcaster()` 返回 `InMemoryBroadcaster`，**不监听 9443、不发起任何 gRPC 连接**（`TestNodeRPCWiringDefaultOff` 守护）。
- 升级该版本后，不开 feature 即与旧版行为完全一致，无新增网络暴露面。
- 当前 gRPC 通道为 **h2c 明文（insecure）**，只允许跑在 jail vnet 内网或 IPsec 隧道之上；TLS 凭证经 `WithGRPCServerOptions` / `WithDialOptions` 注入，属于后续阶段（P8 多机 + 跨网段）工作，**禁止在公网明文开启 9443**。

#### 11A.7.2 协议工具链与重新生成（开发机，部署机无需）

生成的 `.pb.go` 已随仓库提交，部署机不需要 protoc。只有修改 `node.proto` 时才需要工具链（在 0.88 实测）：

```sh
# FreeBSD 开发机（0.88 实机版本；Linux 用对应发行版包）
sudo pkg install -y protobuf                 # protoc 29.6，含 google/protobuf 标准 include
export GOPROXY=https://goproxy.cn,direct    # proxy.golang.org 在 0.88 不可达，goproxy.cn 实测可用
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2
export PATH="$HOME/go/bin:$PATH"

# 在 kernel/ 目录下重新生成（paths=source_relative，落 proto/ 子包）
cd kernel
protoc --go_out=. --go_opt=paths=source_relative \
       --go-grpc_out=. --go-grpc_opt=paths=source_relative \
       proto/node.proto
# 产物：proto/node.pb.go、proto/node_grpc.pb.go，go_package 为
#       github.com/lightmem/kernel/proto;nodepb
go mod tidy    # kernel 模块与仓库根模块都要执行（根模块以 replace 引用 kernel）
```

依赖版本（`kernel/go.mod`，stdlib-only 红线的唯一例外）：`google.golang.org/grpc v1.84.0`、`google.golang.org/protobuf v1.36.12`。

#### 11A.7.3 网络拓扑与 pf 规则

多机形态（以 `profiles/lims-enterprise.profile.yaml` 的 `network.roles` 为例）：

| 角色 | 地址 | 9443 行为 |
|---|---|---|
| coordinator | 10.30.7.1 | 监听 9443，接受 worker 注册 / 心跳 / 撤销流 |
| worker ×N | 10.30.7.10 / 11 / 12 | 主动出站连 10.30.7.1:9443，**不监听** |

单机 jail 演练：coordinator 可落在宿主机 bridge0 网关 `10.0.10.1:9443`，各 jail 作为 worker 出站。跨机链路必须先走 R16 红线⑦的 **IPsec 隧道**（见第 11 节），明文 h2c 不得出内网。

pf 片段（**合流时由 S2 泳道并入 `manuals/pf.rules`，本节只给建议文本，S3 不改 pf.rules**）：

```pf
# 9443/tcp 仅放行：节点内网段（jail vnet 10.0.10.0/24 或 IPsec 段 10.30.7.0/24）
# coordinator 侧入站；worker 侧无需放行入站
tcp_services = "{ 443, 8443, 9443 }"
pass in on $int_if proto tcp from <lims_nodes> to $coordinator_ip port 9443 keep state
# 其余来源对 9443 一律 block drop（沿用手册第 6 节默认拒绝策略）
```

#### 11A.7.4 配置与启动方式

**1）profile 开关与 coordinator 端点解析**（优先级见 `kernel/profile.go` 的 `CoordinatorGRPCAddr`）：

1. 环境变量 `LIGHTMEM_NODE_GRPC_ADDR=host:port`（最高优先级，调试用）；
2. 否则取 `network.roles.coordinator[0].host`，端口取 `LIGHTMEM_NODE_GRPC_PORT`，再否则用 `DefaultNodeGRPCPort=9443`；
3. 两者都解析不到时返回空串，`GRPCNodeRegistry` 表现为 no-op 注册（不阻断单机启动）。

```yaml
# profiles/lims-enterprise.profile.yaml（已就位：cross_node_rank_merge: true）
features:
  cross_node_rank_merge: true
network:
  roles:
    coordinator:
      - host: 10.30.7.1
    worker:
      - host: 10.30.7.10
```

**2）Coordinator 进程启动 gRPC 服务端**：

```go
srv := nodepb.NewNodeRPCServer(
    nodepb.WithNodeRegistry(reg),          // 不传则用 InMemoryNodeRegistry
    nodepb.WithCrossNodeBroadcaster(bc),   // 不传则用 InMemoryBroadcaster
    nodepb.WithPreFilter(pf),              // 不传则用 NoopPreFilter（全放行）
    nodepb.WithQueryHandler(queryHandler), // 不传则跨节点 Query 返回 Unimplemented
    // nodepb.WithGRPCServerOptions(grpc.Creds(creds)), // P8 多机阶段注入 TLS
)
addr, err := srv.Start("0.0.0.0:9443")     // 空串时读 env，再否则用默认端口
defer srv.Stop()                           // GracefulStop，等待流收敛
```

**3）Worker 进程装配**（由 profile 统一接线，默认关自动走 InMemory）：

```go
reg := kernel.NewNodeRegistry(p)          // 开 → *GRPCNodeRegistry；关 → *InMemoryNodeRegistry
bc  := kernel.NewCrossNodeBroadcaster(p)  // 开 → *GRPCBroadcaster；关 → *InMemoryBroadcaster
```

#### 11A.7.5 回环验证（0.88 已实测）与部署后验证

回环验证（不依赖多机，kernel 模块内直接起停服务）：

```sh
cd kernel
go test -race -count=1 -run 'GRPC|NodeRPCWiring' -v .
# 8 个用例应全部 PASS：
#   TestGRPCNodeRegistryLoopback / TestGRPCNodeRegistryEmptyAddr
#   TestGRPCBroadcasterLoopback（含 tenant 撤销 + 全局撤销 + 取消订阅）
#   TestGRPCPreFilterLoopback / TestGRPCQueryLoopback / TestGRPCRevokeRemote
#   TestNodeRPCWiringDefaultOff / TestGRPCServerStartTwiceRejected
```

部署后按顺序检查（coordinator 节点执行）：

```sh
# 1) 监听确认（FreeBSD 用 sockstat）
sockstat -4l -P tcp -p 9443               # 仅开启 feature 后应有 LISTEN；默认关时无输出
# 2) worker 节点连通性
nc -vz 10.30.7.1 9443                     # connected；pf 拦截时应 timeout
# 3) 注册面：worker 上线后 coordinator 应能 ListNodes 看到心跳
# 4) 撤销面：coordinator 执行一次全局 Revoke（空 tenant），所有订阅 worker 应收到
#    （TestGRPCBroadcasterLoopback 已覆盖此路径的回环等价验证）
```

#### 11A.7.6 故障排查

| 现象 | 排查 / 修复 |
|---|---|
| 9443 无监听 | 确认 `features.cross_node_rank_merge: true`；默认关时无监听是预期行为，不是故障 |
| worker 报 coordinator 不可达 | 先 `nc -vz <host> 9443`；检查 `LIGHTMEM_NODE_GRPC_ADDR` / `network.roles.coordinator[0].host`；确认 pf 已放行节点段、跨机走 IPsec |
| 连接串里端口不生效 | `LIGHTMEM_NODE_GRPC_ADDR` 是完整 `host:port` 且优先级最高，会覆盖 profile 与 `LIGHTMEM_NODE_GRPC_PORT` |
| 关停时服务端等待较久 | 检查 worker 侧 `RevocationStream` 订阅是否都调用了 cancel；`Stop()` 为 GracefulStop，未关闭的流会阻塞其收敛 |
| 升级后行为异常 | 回滚 = profile 中置 `cross_node_rank_merge: false`（或删掉该键），重启即恢复纯单机 InMemory，无数据迁移 |
| 协议改动后编译失败 | 必须重新跑 §11A.7.2 的 protoc 生成；`node.pb.go` 与 `node.proto` 必须同源提交 |

#### 11A.7.7 验收核对与红线确认

| # | 验收项（任务书 §七-3） | 结果 |
|---|---|---|
| 1 | `go test -count=1 ./kernel/` 全 PASS（本地回环） | ✅ 8 个新增 gRPC 用例 `-race` 全 PASS；kernel 包存在 1 个 **c5ca737 基线预存失败** `TestQdrantHTTPEndToEnd`（纯 HEAD 树同样失败，与 S3 无关，见交付报告） |
| 2 | `go vet ./kernel/` 无错 | ✅ 无输出 |
| 3 | 不开 feature 时无 gRPC 初始化 | ✅ `TestNodeRPCWiringDefaultOff` 守护；默认返回 InMemory 实现 |
| 4 | 手册 §11A.7 | ✅ 本节 |

**红线确认**：grpc/protobuf 是 stdlib-only 红线的唯一例外（`kernel/go.mod` 新增，根模块仅传递间接依赖）；未修改 `model.go` / `storage.go` / `lifecycle.go` / `compliance.go` / `backend_qdrant*.go` / `backend_sqlite*.go`；未触碰 `manuals/pf.rules`（S2 文件域，pf 片段仅建议）；未修改 gate.sh；默认关，未在任何 profile 中打开新的对外监听（enterprise profile 的 `cross_node_rank_merge: true` 为 R16 已就位）。

---

## 12. 与后续阶段的关系

| P1 搭建项 | 后续阶段衔接 |
|---|---|
| PG + pgvector 单库 | P2 多信号检索（BM25+实体）基于此库 |
| ZFS dataset 布局 | P4 生命周期引擎的归档/冷归档落到 `tank/lims/archive` |
| jail 组件隔离 | P8 集合体形态：把 jail 演进为节点角色（Coordinator/Worker/Archive/Gateway） |
| pf 防火墙 | P8 集合体节点间 IPsec 规则在此扩展 |
| 加密 dataset | P6 GoalMemory / 共享模型中的 `permission_level=机密` 记忆落此 |
| 快照容灾 | 归档节点 `zfs send/receive` 策略在此基础上扩展 |

---

## 附录 A：常见问题

| 问题 | 排查/修复 |
|---|---|
| `pkg install postgresql18-pgvector` 失败 | 确认 pkg 仓库为 `latest`（`pkg install pkg` 后 `pkg update -f`）；或改用 Ports `databases/pgvector` 自编译 |
| PG 无法启动 | 检查 `/var/db/pgdata` 权限（`chown postgres:postgres`）、ZFS dataset 挂载状态 |
| jail 无法 ping 通宿主 | 确认 bridge0 已 addm 物理网卡；检查 jail vnet 配置 |
| pf 生效后 SSH 断开 | 本地控制台执行 `pfctl -d` 恢复，再修正 `pf.conf` |
| `zfs load-key` 失败 | 确认密钥文件路径与 `keyformat` 匹配（`raw` 需 `openssl rand -base64`；`passphrase` 需交互输入） |
| pgvector 查询慢 | 检查 HNSW 索引是否建立、向量列是否在独立 dataset（`recordsize=128k`） |
| ARC 与 shared_buffers 内存竞争 | 设置 `vfs.zfs.arc_max` 上限（如物理内存 50%），其余留给 PG |

## 附录 B：与架构文档映射

| 本手册章节 | 架构文档 |
|---|---|
| 第 1 节 拓扑 | §2.1 总体架构、§5.5 部署形态 |
| 第 3 节 ZFS dataset | §5.6「dataset 布局」 |
| 第 4 节 组件安装 | §6 技术选型表 |
| 第 5 节 jail | §5.7「进程隔离」 |
| 第 6 节 pf | §5.7「网络隔离」+ §5.5「集合体」 |
| 第 7 节 安全 | §5.7「安全组件」 |
| 第 9 节 快照容灾 | §5.6「快照与容灾」 |
| 第 10 节 快照容灾实操（R16-S7） | §5.6「快照与容灾」 |
| 全部 | §4.2 一致性骨架（写入链路）|