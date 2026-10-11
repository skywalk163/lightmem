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

## 10. 与后续阶段的关系

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
| 全部 | §4.2 一致性骨架（写入链路）|