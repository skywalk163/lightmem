# 光明记忆（LIMS）FreeBSD 安全加固 Checklist

**文档版本：** v1.0
**对应架构文档：** `lightmem/lightmem.md` §5.7（v1.9）
**适用平台：** FreeBSD 14.x（根文件系统为 ZFS）
**目标读者：** 部署实施工程师、安全合规评审人
**用途：** 供 §12 安全合规评审直接引用，也可作为部署后的逐项验收清单

---

## 0. 使用说明

1. 本 checklist 是**命令级可执行**版本：每一项给出「操作命令 + 验证命令 + 预期结果」。
2. 执行顺序**建议严格自上而下**：先隔离、再加密、再网络、最后审计，避免中途暴露。
3. 每完成一步，把「□」勾选并填入实际验证输出摘要。
4. 与 §5.7 的关系：§5.7 说明"为什么 + 原理"，本文件回答"怎么做 + 怎么验证"。
5. **边界声明**：本 checklist 只做 OS 层加固，**不替代** §7.1 权限三元组、§7.5 投毒防护、§7.3 级联删除的应用层实现。

---

## 1. 前置条件检查

- [ ] **FreeBSD 版本确认**
  ```sh
  freebsd-version
  # 预期：14.x-RELEASE（如 14.1-RELEASE / 14.2-RELEASE）
  ```
- [ ] **ZFS root 确认**
  ```sh
  zpool list
  # 预期：能看到 root pool（如 zroot）且状态为 ONLINE
  df -h /
  # 预期：文件系统类型为 zfs
  ```
- [ ] **root 权限确认**
  ```sh
  id
  # 预期：uid=0(root)
  ```
- [ ] **时区与 NTP 确认**（影响审计时间戳一致性）
  ```sh
  tzsetup  # 选择 Asia/Shanghai
  sysrc ntpd_enable=YES && service ntpd start
  ```

---

## 2. 最小安装与基础加固

- [ ] **关闭不需要的服务**
  ```sh
  sysrc sshd_enable=YES       # 若需远程管理，仅此一项必开
  sysrc sendmail_enable=NONE  # 关闭 sendmail
  sysrc lpd_enable=NONE
  sysrc rpcbind_enable=NONE
  # 其余按需关闭：dumpdev=NONE, nfsd_enable=NONE
  ```
  验证：
  ```sh
  sysrc -a | grep _enable
  # 预期：除 sshd 外均为 NONE
  ```
- [ ] **系统更新到最新 patch 版本**
  ```sh
  freebsd-update fetch install
  ```
  验证：
  ```sh
  freebsd-version -k -u
  # 预期：内核与 userland 均已更新
  ```
- [ ] **SSH 加固（如启用 SSH）**
  ```sh
  # 编辑 /etc/ssh/sshd_config
  PermitRootLogin no
  PasswordAuthentication no
  PubkeyAuthentication yes
  X11Forwarding no
  MaxAuthTries 3
  ```
  验证：
  ```sh
  ssh -G localhost | grep -E 'PermitRootLogin|PasswordAuthentication'
  # 预期：no / no
  ```

---

## 3. 组件隔离（jail + vnet）

**目标：** 每个存储/服务组件独立 jail，崩溃/越权不跨 jail；vnet 使节点间仅暴露必要端口。

- [ ] **安装 iocage**
  ```sh
  pkg install iocage
  ```
- [ ] **初始化 iocage 与 RELEASE base**
  ```sh
  iocage fetch release=14.1-RELEASE
  iocage create --name lims-pg --release 14.1-RELEASE --pkglist=autostart pkg=boot
  ```
- [ ] **创建各组件 jail（PG / Redis / RabbitMQ / MinIO / Router）**
  ```sh
  iocage create --name lims-pg      --release 14.1-RELEASE pkg=boot
  iocage create --name lims-redis   --release 14.1-RELEASE pkg=boot
  iocage create --name lims-rabbit  --release 14.1-RELEASE pkg=boot
  iocage create --name lims-minio   --release 14.1-RELEASE pkg=boot
  iocage create --name lims-router  --release 14.1-RELEASE pkg=boot
  ```
- [ ] **为每个 jail 启用 vnet + 分配独立 IP 段**
  ```sh
  iocage set interfaces="vnet0:bridge0" lims-pg
  iocage set ip4_addr="vnet0|10.0.10.10/24" lims-pg
  iocage set ip4_addr="vnet0|10.0.10.20/24" lims-redis
  iocage set ip4_addr="vnet0|10.0.10.30/24" lims-rabbit
  iocage set ip4_addr="vnet0|10.0.10.40/24" lims-minio
  iocage set ip4_addr="vnet0|10.0.10.50/24" lims-router
  # 创建宿主桥接
  ifconfig bridge0 create
  ifconfig bridge0 addm em0 up
  ```
- [ ] **挂载各组件所需 ZFS dataset 到 jail**（只读或按需读写，见第 4 节）
  ```sh
  iocage mount lims-pg tank/lims/pg /var/db/pgdata nullfs rw
  iocage mount lims-redis tank/lims/redis /var/db/redis nullfs rw
  iocage mount lims-minio tank/lims/minio /data nullfs rw
  ```
- [ ] **验证隔离**
  ```sh
  iocage list
  # 预期：5 个 jail 全部 running
  jexec lims-pg ps aux
  # 预期：jail 内只看到该 jail 自己的进程
  jls
  # 预期：每个 jail 有独立 jid / path / host
  ```

---

## 4. ZFS dataset 规划与加密

**目标：** 按存储组件独立划分 dataset，机密数据启用静态加密。

- [ ] **创建各组件 dataset（recordsize 按 §5.6 建议）**
  ```sh
  zfs create -o mountpoint=/var/db/pgdata  tank/lims/pg
  zfs create -o recordsize=8k    -o compression=lz4 tank/lims/pg_wal
  zfs create -o recordsize=128k  -o compression=lz4 tank/lims/pg_vector
  zfs create -o recordsize=8k    -o compression=lz4 tank/lims/redis
  zfs create -o recordsize=128k  -o compression=lz4 tank/lims/minio
  zfs create -o recordsize=1M    -o compression=lz4 tank/lims/logs
  zfs create -o recordsize=128k  -o compression=lz4 tank/lims/archive
  ```
  验证：
  ```sh
  zfs list -o name,recordsize,compression,mountpoint | grep tank/lims
  # 预期：所有 lims 相关 dataset 均创建，recordsize/compression 符合预期
  ```
- [ ] **关闭 atime（减少元数据写入）**
  ```sh
  zfs set atime=off tank/lims
  ```
  验证：
  ```sh
  zfs get atime tank/lims/pg
  # 预期：off
  ```
- [ ] **机密 dataset 启用加密**
  ```sh
  # 生成密钥文件（仅 root 可读）
  openssl rand -base64 32 > /root/lims.key
  chmod 600 /root/lims.key
  # 创建加密 dataset
  zfs create -o encryption=on \
             -o keyformat=raw \
             -o keylocation=file:///root/lims.key \
             tank/lims/secret
  ```
  验证：
  ```sh
  zfs get encryption tank/lims/secret
  # 预期：on
  zfs get keystatus tank/lims/secret
  # 预期：available
  ```
- [ ] **测试密钥加载与卸载**
  ```sh
  zfs unmount tank/lims/secret && zfs unload-key tank/lims/secret
  zfs get keystatus tank/lims/secret
  # 预期：unavailable
  zfs load-key tank/lims/secret
  zfs mount tank/lims/secret
  zfs get keystatus tank/lims/secret
  # 预期：available
  ```
- [ ] **配置开机自动加载密钥（可选，需权衡安全性）**
  ```sh
  sysrc zfskeys_enable=YES
  # 密钥文件需在开机时可用，否则需人工 zfs load-key
  ```

---

## 5. 强制访问控制（MAC）

**目标：** `permission_level=机密` 的记忆所在进程/文件受 MAC 约束，与 §7.1 密级形成"应用+OS"双保险。

- [ ] **加载 MAC 内核模块**
  ```sh
  kldload mac_bsdextended
  # 或 mac_lomac / mac_mls
  ```
  **持久化（开机加载）**：
  ```sh
  echo 'mac_bsdextended_load="YES"' >> /boot/loader.conf
  echo 'mac_lomac_load="YES"' >> /boot/loader.conf
  ```
  验证：
  ```sh
  kldstat | grep mac
  # 预期：能看到 mac_bsdextended / mac_lomac
  ```
- [ ] **对机密 dataset 启用 MAC 标签（mac_lomac 示例）**
  ```sh
  sysctl security.mac.lomac.enabled=1
  # 为 lims-router jail 的进程打标签
  jexec lims-router setfmac high:mls tank/lims/secret
  jexec lims-router getfmac tank/lims/secret
  ```
  验证：
  ```sh
  sysctl security.mac.lomac.enabled
  # 预期：1
  ```
  > **注意：** MAC 标签配置错误可能导致服务无法访问数据，**务必在维护窗口操作并保留回滚方案**。

---

## 6. 文件系统 ACL

**目标：** dataset/目录粒度授权，补充 jail 内进程权限。

- [ ] **为读取者授予 ACL**
  ```sh
  setfacl -m u:lims_reader:rx tank/lims/pg
  setfacl -m g:lims_reader:rx tank/lims/archive
  ```
  验证：
  ```sh
  getfacl tank/lims/pg
  # 预期：能看到 lims_reader 的 r-x 权限
  ```
- [ ] **为写入者授予 ACL**
  ```sh
  setfacl -m u:lims_writer:rwx tank/lims/pg
  ```
- [ ] **移除不需要的 ACL**
  ```sh
  setfacl -x u:lims_reader tank/lims/secret
  ```

---

## 7. 网络防火墙（pf）

**目标：** 仅网关暴露对外端口；节点间走 IPsec；内部组件间只放行必要端口。

- [ ] **启用 pf 并配置规则**
  ```sh
  sysrc pf_enable=YES
  service pf start
  ```
- [ ] **编写 /etc/pf.conf（示例：单节点形态）**
  ```text
  # 默认拒绝
  block in all
  block out all

  # 允许 loopback
  set skip on lo0

  # 对外暴露：仅 TLS 端口 443/8443（nginx 网关）
  pass in on em0 proto tcp from any to (em0) port { 443, 8443 } flags S/SA keep state

  # 内部 vnet 网段（组件间）
  pass in on bridge0 from 10.0.10.0/24 to any keep state
  pass out on bridge0 from any to 10.0.10.0/24 keep state

  # SSH 管理（建议限制来源 IP）
  pass in on em0 proto tcp from <management_net> to (em0) port 22 keep state

  # 拒绝其余
  block in on em0 all
  ```
  > **重要：** 在 `pf.conf` 生效前，先测试：
  ```sh
  pfctl -vnf /etc/pf.conf   # 语法校验
  ```
- [ ] **应用规则并持久化**
  ```sh
  pfctl -f /etc/pf.conf
  ```
  验证：
  ```sh
  pfctl -s rules
  # 预期：能看到 block/pass 规则
  pfctl -s states
  # 预期：正常连接可见
  ```
- [ ] **节点间 IPsec（集合体形态）**
  ```sh
  # 安装 strongswan
  pkg install strongswan
  # 编辑 /usr/local/etc/strongswan.conf 配置节点对
  sysrc strongswan_enable=YES
  service strongswan start
  ```

---

## 8. 漏洞审计与系统审计

- [ ] **运行 pkg audit 扫描第三方包 CVE**
  ```sh
  pkg audit -F
  ```
  > **输出解读：** 有 `0 problem(s)` 视为通过；有漏洞需评估修复或记录豁免。
- [ ] **订阅 FreeBSD 安全通告**（人工/脚本）
  ```sh
  # 每日 cron 检查
  echo '0 3 * * * root pkg audit -F > /var/log/pkg-audit.log 2>&1' >> /etc/crontab
  ```
- [ ] **启用 Process Accounting**
  ```sh
  sysrc accounting_enable=YES
  service accounting start
  accton /var/account/acct
  ```
  验证：
  ```sh
  lastcomm | head -20
  # 预期：能看到特权命令执行记录
  ```

---

## 9. 根分区与最小权限加固

- [ ] **根数据集设为只读（可选，需权衡可维护性）**
  ```sh
  zfs set readonly=on zroot
  # 需要更新时：zfs set readonly=off zroot
  ```
- [ ] **sysctl 加固**
  ```sh
  sysctl security.bsd.see_other_uids=0
  sysctl security.bsd.see_other_gids=0
  sysctl security.bsd.unprivileged_proc_debug=0
  sysctl kern.randompid=1
  ```
  持久化：
  ```sh
  echo 'security.bsd.see_other_uids=0' >> /etc/sysctl.conf
  echo 'security.bsd.see_other_gids=0' >> /etc/sysctl.conf
  echo 'security.bsd.unprivileged_proc_debug=0' >> /etc/sysctl.conf
  ```

---

## 10. 收尾验证清单

| 检查项 | 验证命令 | 预期 |
|---|---|---|
| 所有 jail 运行 | `iocage list` | 5 个 jail 全部 running |
| 加密 dataset 生效 | `zfs get encryption tank/lims/secret` | `on`，`keystatus=available` |
| MAC 生效 | `sysctl security.mac.lomac.enabled` | `1` |
| 防火墙规则生效 | `pfctl -s rules` | 只有 443/8443/22 放行 |
| ACL 生效 | `getfacl tank/lims/pg` | 有 lims_reader 权限 |
| pkg audit 通过 | `pkg audit -F` | `0 problem(s)` |
| 系统审计启用 | `lastcomm \| head` | 有命令记录 |
| 无明文密钥 | `ls -la /root/lims.key` | `-rw------- root wheel` |
| 无暴露的未加密数据 | `zfs get encryption -r tank/lims` | 机密 dataset 为 `on` |

---

## 11. 完成判定

本 checklist 视为通过，需同时满足：

1. 第 10 节所有验证项均为预期值；
2. 无遗留的 `permission_level=机密` 数据落在未加密 dataset 上；
3. 无 jail 直接访问宿主 ZFS pool（`iocage list` + `zfs mount` 交叉核对）；
4. 防火墙仅暴露必要端口；
5. 全部配置已持久化到 `/etc/rc.conf`、`/boot/loader.conf`、`/etc/sysctl.conf`、`/etc/pf.conf`（重启后仍生效）。

---

## 附录 A：快速回滚

| 场景 | 回滚命令 |
|---|---|
| 防火墙把 SSH 锁了 | 本地控制台执行 `pfctl -d`（禁用 pf） |
| MAC 标签导致 jail 无法访问数据 | `sysctl security.mac.lomac.enabled=0` + `getfmac` 清标签 |
| 加密 dataset 无法挂载 | `zfs load-key tank/lims/secret`（需密钥文件或口令） |
| jail 起不来 | `iocage stop <name>` → `iocage start <name>` |
| pkg audit 误报 | 记录 CVE ID 与豁免理由，归档至审计日志 |

## 附录 B：与架构文档的映射

| 本节步骤 | 架构文档章节 |
|---|---|
| 第 2 节 最小安装 | §5.6「为什么 FreeBSD + ZFS」 |
| 第 3 节 jail | §5.7「进程隔离」 |
| 第 4 节 ZFS dataset/加密 | §5.6「dataset 布局」+ §5.7「磁盘加密」 |
| 第 5 节 MAC | §5.7「强制访问控制」 |
| 第 6 节 ACL | §5.7「文件级访问」 |
| 第 7 节 pf/IPsec | §5.7「网络隔离」+ §5.5「集合体」 |
| 第 8 节 pkg audit/accton | §5.7「漏洞审计 / 系统审计」 |
| 第 9 节 最小权限 | §5.7「系统加固」 |
| 全部 | §7「安全、隐私与数据主权」业务层约束 |
| 全部 | 配套搭建手册 `manuals/p1-freebsd-zfs-setup.md`（本 checklist 与该手册配合使用：手册负责"搭起来"，本 checklist 负责"验证是否生效"） |