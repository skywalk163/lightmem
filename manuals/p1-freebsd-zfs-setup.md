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

---

### 11A.7 R17-S3 跨节点 gRPC 通信部署（P8 节点 mesh 落地 runbook）

> **对应任务**：R17 S3 ——「把 P8 跨节点 rank merge 的骨架（`InMemoryNodeRegistry` / `InMemoryBroadcaster` / `NoopPreFilter` + `topology.go`）替换为真实 gRPC 通信，默认关、本地回环」
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