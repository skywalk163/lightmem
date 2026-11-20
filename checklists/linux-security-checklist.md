# 光明记忆（LIMS）Linux 安全加固 Checklist

**文档版本：** v1.0
**对应架构文档：** `lightmem/lightmem.md` §5.7（v2.0）
**适用平台：** Ubuntu 24.04 LTS / Debian 12 / Rocky Linux 9
**对应 FreeBSD 版：** `checklists/freebsd-security-checklist.md`（本文逐节给出 Linux 对照）
**目标读者：** 部署实施工程师、安全合规评审人
**用途：** §12 安全合规评审可直接引用；也可作为 Linux 部署后的逐项验收清单

---

## 0. 使用说明

1. 本 checklist 是**命令级可执行**版本：每项给出「操作命令 + 验证命令 + 预期结果」。凡本文写「预期 …」都是「应该如此」，不一致时按 `manuals/linux-setup.md` §13 排障，别先怀疑产品。
2. 执行顺序建议**严格自上而下**：先最小化、再隔离、再加密、再网络、最后审计 —— 与 FreeBSD 版同序，避免中途暴露。
3. 每完成一步勾选「□」并填入实际验证输出摘要（评审要看的是你机器上的输出，不是本文的预期）。
4. **边界声明**：本清单只做 OS 层加固，**不替代** §7.1 权限三元组、§7.5 投毒防护、§7.3 级联删除的应用层实现。
5. **Linux 与 FreeBSD 的根本差异**：FreeBSD 用 `jail + ZFS + pf + MAC` 四件套；Linux 没有等价的一站式方案，对应物是 **systemd 沙箱 / LUKS-fscrypt / ufw-firewalld / SELinux-AppArmor**。少一层就是少一层，不要用「已勾选」掩盖差异（第 3 节末尾有专门说明）。
6. **证据等级**：本文命令按各发行版官方方式书写；本卡交付环境无 Linux 真机，装包与 systemd 类命令属**未实测**，第 3/4 节的「沙箱与加密机制」部分为实机常识性核对。首次执行请逐条看输出。

---

## 1. 前置条件检查

- [ ] **发行版与版本确认**
  ```sh
  cat /etc/os-release | grep -E '^(NAME|VERSION_ID)='
  uname -m
  # 预期：Ubuntu 24.04 / Debian 12 / Rocky 9；x86_64 或 aarch64
  ```
- [ ] **root 权限确认**
  ```sh
  id -u
  # 预期：0（或全程 sudo）
  ```
- [ ] **时区与 NTP**（审计时间戳一致性，出事后对时间线全靠它）
  ```sh
  timedatectl set-timezone Asia/Shanghai
  timedatectl set-ntp true
  timedatectl status | grep -E 'Time zone|synchronized'
  # 预期：Asia/Shanghai (CST, +0800) / System clock synchronized: yes
  ```
- [ ] **文件系统类型确认**（决定第 4 节走 LUKS 还是 fscrypt）
  ```sh
  findmnt -no SOURCE,FSTYPE,OPTIONS /opt
  # 预期：FSTYPE 为 ext4 或 xfs；OPTIONS 含 noatime（没含就按手册 §6 补）
  ```
- [ ] **SELinux / AppArmor 现状**
  ```sh
  getenforce 2>/dev/null || echo "no selinux"     # Rocky 预期 Enforcing
  aa-status --enabled 2>/dev/null && echo apparmor_on || echo "no apparmor"
  ```

---

## 2. 最小安装与基础加固

- [ ] **关掉不需要的服务**（按需删减，原则：不用的服务就是攻击面）
  ```sh
  systemctl list-unit-files --state=enabled --no-pager
  # 常见可关：cups（打印）、bluetooth、avahi-daemon、ModemManager
  sudo systemctl disable --now cups bluetooth avahi-daemon 2>/dev/null
  ```
  验证：
  ```sh
  systemctl list-unit-files --state=enabled --no-pager | grep -E 'cups|bluetooth|avahi'
  # 预期：无输出
  ```
- [ ] **系统更新到最新 patch**
  ```sh
  # Ubuntu / Debian
  sudo apt update && sudo apt -y full-upgrade
  # Rocky
  sudo dnf -y upgrade --refresh
  ```
  验证：
  ```sh
  apt list --upgradable 2>/dev/null | tail -n +2 | wc -l    # 预期：0
  # 或 Rocky：sudo dnf updateinfo summary | head
  ```
- [ ] **自动安全更新**
  ```sh
  # Ubuntu / Debian
  sudo apt install -y unattended-upgrades
  sudo dpkg-reconfigure -plow unattended-upgrades
  # Rocky
  sudo dnf install -y dnf-automatic
  sudo sed -i 's/^apply_updates.*/apply_updates = yes/' /etc/dnf/automatic.conf
  sudo systemctl enable --now dnf-automatic.timer
  ```
  验证：
  ```sh
  systemctl is-active unattended-upgrades    # Ubuntu/Debian，预期 active
  systemctl list-timers dnf-automatic.timer  # Rocky，预期有下次触发时间
  ```
- [ ] **SSH 加固**（如需远程管理）
  ```sh
  # /etc/ssh/sshd_config.d/99-lightmem-hardening.conf
  PermitRootLogin no
  PasswordAuthentication no
  PubkeyAuthentication yes
  X11Forwarding no
  MaxAuthTries 3
  AllowUsers <你的运维账号>
  ```
  验证：
  ```sh
  sudo sshd -t && sudo sshd -T | grep -E '^(permitrootlogin|passwordauthentication|maxauthtries)'
  # 预期：permitrootlogin no / passwordauthentication no / maxauthtries 3
  ```
  > 改完**先别断开当前会话**，另开一个窗口验证能登录，再退出。

---

## 3. 进程隔离（systemd 沙箱替代 jail）

**目标：** FreeBSD 的 `jail + vnet` 在 Linux 上没有等价物。Linux 档位用「专用系统用户 + systemd 沙箱 + 只给白名单可写路径」实现**同等意图的降级版**：拿不到提权、看不到宿主目录、只能写数据与日志目录。

- [ ] **确认专用用户存在且不可登录**
  ```sh
  id lightmem
  getent passwd lightmem | cut -d: -f7
  # 预期：shell 为 /usr/sbin/nologin
  ```
- [ ] **确认沙箱项都在 unit 里**
  ```sh
  systemctl cat lightmem.service | grep -E 'NoNewPrivileges|ProtectSystem|ProtectHome|PrivateTmp|PrivateDevices|RestrictAddressFamilies|RestrictNamespaces|LockPersonality|UMask'
  # 预期：九项全在，且 ProtectSystem=strict
  ```
- [ ] **确认进程确实跑在沙箱里（运行期核对，不是看配置文件）**
  ```sh
  PID=$(systemctl show -p MainPID --value lightmem.service)
  sudo grep -E 'NoNewPrivs|Seccomp' /proc/$PID/status
  # 预期：NoNewPrivs: 1
  sudo ls -l /proc/$PID/root/proc | head -3
  # 预期：能看到沙箱视图（PrivateTmp 下 /tmp 与宿主不同）
  ```
- [ ] **确认服务以非 root 运行**
  ```sh
  ps -o user,pid,cmd -p "$(systemctl show -p MainPID --value lightmem.service)"
  # 预期：lightmem（不是 root）
  ```
- [ ] **可写面只有白名单**
  ```sh
  systemctl show lightmem.service -p ReadWritePaths
  # 预期：/opt/lightmem/data /opt/lightmem/logs
  ```
  验证（反向证明）：
  ```sh
  sudo -u lightmem touch /etc/lightmem-should-fail 2>&1 | head -1
  # 预期：Permission denied（服务进程同理）
  ```

> **边界声明（评审必读）**：systemd 沙箱与 jail **不等价**。共用同一内核、同一用户命名空间，被利用后仍能通过内核漏洞逃逸；它拦的是「被利用后能做什么」（写系统文件、读家目录、提权）。要 jail 级隔离请在 Linux 上用容器/轻量 VM，并在此处把差异写进评审记录 —— **不要勾了本项就宣称「等价于 FreeBSD jail」**。

---

## 4. 数据目录与静态加密（LUKS / fscrypt 替代 ZFS encryption）

**目标：** `permission_level=机密` 的记忆不能以明文躺在盘上。FreeBSD 用 `zfs create -o encryption=on`；Linux 用块层 LUKS 或目录层 fscrypt。

- [ ] **目录与属主**
  ```sh
  sudo install -d -o lightmem -g lightmem -m 0750 /opt/lightmem/data /opt/lightmem/logs
  ls -ld /opt/lightmem/data /opt/lightmem/logs
  # 预期：drwxr-x--- lightmem lightmem
  ```
- [ ] **选一种加密方案（二选一，别叠加）**

  **方案 A：LUKS（块层，整分区/整盘，重启需解锁或 keyfile）**
  ```sh
  sudo cryptsetup luksFormat --type luks2 /dev/sdXN          # 会二次确认，别选错盘
  sudo cryptsetup open /dev/sdXN lims_crypt
  sudo mkfs.ext4 -L limsdata /dev/mapper/lims_crypt
  sudo mkdir -p /opt/lightmem/data && sudo mount /dev/mapper/lims_crypt /opt/lightmem/data
  # /etc/crypttab:  lims_crypt  /dev/sdXN  /etc/lightmem/luks.key  luks,discard
  # /etc/fstab:     /dev/mapper/lims_crypt  /opt/lightmem/data  ext4  defaults,noatime  0 2
  ```
  验证：
  ```sh
  sudo cryptsetup status lims_crypt | grep -E 'type|device'
  findmnt -no FSTYPE,SOURCE /opt/lightmem/data
  # 预期：ext4 /dev/mapper/lims_crypt
  sudo dd if=/dev/sdXN bs=4k count=8 2>/dev/null | strings | grep -c '"fact"' 
  # 预期：0（原盘上看不到明文业务字段；有值说明加密没生效）
  ```

  **方案 B：fscrypt（目录层，只加密 data 目录）**
  ```sh
  sudo apt install -y fscrypt            # Rocky: sudo dnf install -y fscrypt
  sudo fscrypt setup                     # 初始化 /etc/fscrypt.conf 与 /.fscrypt
  sudo fscrypt setup /opt
  sudo fscrypt encrypt /opt/lightmem/data --user=lightmem
  ```
  验证：
  ```sh
  sudo fscrypt status /opt/lightmem/data
  # 预期：Encrypted: Yes，Unlocked: Yes，policy 有 key
  ```
- [ ] **加密密钥权限**
  ```sh
  sudo install -m 0600 -o root -g root /dev/null /etc/lightmem/luks.key   # 用了 keyfile 才有
  sudo ls -l /etc/lightmem/luks.key
  # 预期：-rw------- root root
  ```
- [ ] **备份（加密盘的备份同样要保密）**
  ```sh
  sudo cryptsetup luksHeaderBackup /dev/sdXN --header-backup-file /root/luks-header-$(date +%F).img
  sudo chmod 600 /root/luks-header-*.img
  ```

> 说明：`security.zfs_encryption` 在 Linux 档位恒为 `false` —— 它表达的是「ZFS 加密开关」，不是「有没有加密」。本节的 LUKS/fscrypt 才是 Linux 的加密答案，验收只看本节。

---

## 5. 强制访问控制（SELinux / AppArmor 替代 MAC）

**目标：** 给「服务进程能碰什么」再加一层内核强制。FreeBSD 用 `mac_lomac/mac_bsdextended`；Linux 用发行版自带的 LSM。

- [ ] **Rocky 9：SELinux 保持 enforcing，并给数据目录打标**
  ```sh
  getenforce
  # 预期：Enforcing（若为 Permissive/Disabled，先修 /etc/selinux/config，需重启）
  sudo semanage fcontext -a -t var_lib_t "/opt/lightmem/data(/.*)?"
  sudo restorecon -Rv /opt/lightmem/data
  ```
  验证：
  ```sh
  ls -Z /opt/lightmem/data | head -3
  # 预期：上下文含 :var_lib_t:
  sudo ausearch -m avc -ts recent | tail -5
  # 预期：无新的 avc denied（有就按手册 §13.7 处理）
  ```
- [ ] **Ubuntu / Debian：AppArmor 生效，并确认服务没被「静默 unconfined」**
  ```sh
  sudo aa-status | head -5
  # 预期：apparmor module is loaded，profiles 有 enforcing 计数
  ```
  > lightmem 目前没有自带 AppArmor profile。**不要**伪造一个 `complain` 模式的就勾「已加固」；正确做法是把「无专用 profile」写进评审差异表，靠第 3 节沙箱 + 第 9 节最小权限兜底。
- [ ] **确认没有为了跑通而关掉 LSM**
  ```sh
  grep -E '^SELINUX=' /etc/selinux/config 2>/dev/null
  # 预期：SELINUX=enforcing（不是 disabled/permissive）
  ```

---

## 6. 文件系统 ACL（POSIX ACL）

**目标：** 目录粒度授权，补充「谁能读归档、谁能写库文件」。

- [ ] **确认 ext4/xfs 挂载支持 ACL**（ext4 默认开 `acl`，xfs 天生支持）
  ```sh
  findmnt -no OPTIONS /opt
  # 预期：含 acl（ext4 若用 defaults 即隐含 acl）
  ```
- [ ] **给读取者授只读**
  ```sh
  sudo setfacl -m u:lims_reader:r-x /opt/lightmem/data
  sudo setfacl -m g:lims_reader:r-x /opt/lightmem/data/archive
  getfacl /opt/lightmem/data | grep lims_reader
  # 预期：user:lims_reader:r-x
  ```
- [ ] **给写入者授读写**
  ```sh
  sudo setfacl -m u:lims_writer:rwx /opt/lightmem/data
  ```
- [ ] **默认 ACL（新文件自动继承，替代 FreeBSD 上的继承位）**
  ```sh
  sudo setfacl -d -m u:lims_reader:r-x /opt/lightmem/data/archive
  getfacl /opt/lightmem/data/archive | grep '^default:user:lims_reader'
  # 预期：default:user:lims_reader:r-x
  ```
- [ ] **撤掉不需要的 ACL**
  ```sh
  sudo setfacl -x u:lims_reader /opt/lightmem/data
  sudo setfacl -b /opt/lightmem/data     # 或一次性清空
  ```

---

## 7. 网络防火墙（ufw / firewalld / nftables）

**目标：** 只有「确实要对外」的端口对外。Linux 档位默认 `bind: 127.0.0.1`，此时**不需要任何入站放行**。

- [ ] **确认服务只听回环**
  ```sh
  ss -lntp | grep -E ':(7443|7444)'
  # 预期：两行都是 127.0.0.1:7443 / 127.0.0.1:7444
  # 出现 0.0.0.0 或 :: 说明 profile 的 network.bind 被改了，回手册 §10.2
  ```
- [ ] **启用主机防火墙（默认拒绝入站）**
  ```sh
  # ufw（Ubuntu / Debian）
  sudo ufw default deny incoming && sudo ufw default allow outgoing
  sudo ufw allow 22/tcp
  sudo ufw --force enable && sudo ufw status verbose

  # firewalld（Rocky）
  sudo systemctl enable --now firewalld
  sudo firewall-cmd --set-default-zone=public
  sudo firewall-cmd --permanent --add-service=ssh
  sudo firewall-cmd --reload && sudo firewall-cmd --list-all
  ```
- [ ] **只有确需内网直连时才放 7443/7444（且必须已开鉴权）**
  ```sh
  # ufw
  sudo ufw allow from 192.168.1.0/24 to any port 7443 proto tcp
  sudo ufw allow from 192.168.1.0/24 to any port 7444 proto tcp
  # firewalld
  sudo firewall-cmd --permanent --add-rich-rule='rule family=ipv4 source address=192.168.1.0/24 port port=7443 protocol=tcp accept'
  sudo firewall-cmd --reload
  ```
  验证：
  ```sh
  sudo ufw status numbered | grep -E '744[34]'        # 或 firewall-cmd --list-all
  # 预期：只对指定网段放行，没有 "Anywhere"
  sudo grep -c '^LIMS_REQUIRE_AUTH=1' /opt/lightmem/lightmem.env
  # 预期：1（放行端口就必须强制鉴权）

  # 从另一台机器实测（不是本机 curl，本机永远通）
  nc -vz <服务器IP> 7443
  ```
- [ ] **对外发布走 443 反代，别直接开 7443**
  ```sh
  sudo ufw allow 443/tcp       # 或 firewall-cmd --permanent --add-service=https
  ss -lntp | grep ':443'
  # 预期：nginx/caddy 在听；7443/7444 仍是回环
  ```

---

## 8. 漏洞审计与系统审计

- [ ] **可升级包清单（等价 FreeBSD `pkg audit -F`）**
  ```sh
  # Ubuntu / Debian（安全源）
  sudo apt update && apt list --upgradable 2>/dev/null | tail -n +2
  # Rocky
  sudo dnf updateinfo list security
  ```
  > 判定：无输出 / `0 problem(s)` 视为通过；有则评估修复或记录豁免理由与 CVE 编号。
- [ ] **每日自动跑一次并落日志**
  ```sh
  sudo tee /etc/cron.daily/lims-pkg-audit >/dev/null <<'EOF'
  #!/bin/sh
  { apt list --upgradable 2>/dev/null || dnf updateinfo list security; } > /var/log/lims-pkg-audit.log 2>&1
  EOF
  sudo chmod 0755 /etc/cron.daily/lims-pkg-audit
  ```
- [ ] **系统审计（等价 FreeBSD `accounting_enable` + `lastcomm`）**
  ```sh
  # Ubuntu / Debian
  sudo apt install -y auditd audispd-plugins
  sudo systemctl enable --now auditd
  # Rocky
  sudo dnf install -y audit
  sudo systemctl enable --now auditd
  ```
  验证：
  ```sh
  sudo auditctl -s | grep -E 'enabled|pid'
  # 预期：enabled 1（或 2 = 不可变模式）
  sudo ausearch -m EXECVE -ts today | tail -5
  # 预期：能看到今日执行记录
  ```
- [ ] **命令级留痕（等价 `lastcomm`）**
  ```sh
  # Debian 系：apt install -y acct && systemctl enable --now acct
  # Rocky：  dnf install -y psacct && systemctl enable --now psacct
  sudo lastcomm | head -10
  # 预期：有命令执行记录
  ```
- [ ] **审计日志不被随手清掉**
  ```sh
  sudo grep -E '^(max_log_file|max_log_file_action|space_left_action)' /etc/audit/auditd.conf
  # 预期：max_log_file_action = keep_logs（或 rotate），不是 ignore
  ```

---

## 9. 最小权限与内核加固

- [ ] **sysctl 加固（对应 FreeBSD `security.bsd.*`）**
  ```sh
  sudo tee /etc/sysctl.d/99-lightmem-hardening.conf >/dev/null <<'EOF'
  # 普通用户看不到别人的进程信息/内核日志
  kernel.dmesg_restrict = 1
  kernel.kptr_restrict = 2
  kernel.yama.ptrace_scope = 1
  # 防硬链接/软链接提权
  fs.protected_hardlinks = 1
  fs.protected_symlinks = 1
  fs.suid_dumpable = 0
  # 网络基础加固
  net.ipv4.conf.all.rp_filter = 1
  net.ipv4.tcp_syncookies = 1
  net.ipv4.conf.all.accept_redirects = 0
  net.ipv6.conf.all.accept_redirects = 0
  EOF
  sudo sysctl --system >/dev/null
  ```
  验证：
  ```sh
  sysctl kernel.dmesg_restrict kernel.kptr_restrict kernel.yama.ptrace_scope fs.suid_dumpable
  # 预期：1 / 2 / 1 / 0
  ```
- [ ] **服务账户无 sudo、无附加组**
  ```sh
  sudo -l -U lightmem
  # 预期：is not allowed to run sudo
  id lightmem
  # 预期：只在自己的组里，不含 wheel/sudo/adm
  ```
- [ ] **资源上限（防一条烂查询把机器拖死）**
  ```sh
  # /etc/systemd/system/lightmem.service.d/limits.conf
  [Service]
  LimitNOFILE=65535
  MemoryMax=2G
  TasksMax=512
  ```
  验证：
  ```sh
  sudo systemctl daemon-reload && sudo systemctl restart lightmem.service
  systemctl show lightmem.service -p MemoryMax -p LimitNOFILE
  # 预期：MemoryMax=2147483648 / LimitNOFILE=65535
  ```

---

## 10. 凭据与 token

- [ ] **env 文件 0600，只 root 与 lightmem 可读**
  ```sh
  sudo chown root:lightmem /opt/lightmem/lightmem.env
  sudo chmod 600 /opt/lightmem/lightmem.env
  sudo ls -l /opt/lightmem/lightmem.env
  # 预期：-rw------- 1 root lightmem
  ```
- [ ] **进程环境里看得到，磁盘上不可被同机他人读到**
  ```sh
  sudo -u lightmem cat /opt/lightmem/lightmem.env 2>&1 | head -1
  # 预期：Permission denied
  ```
- [ ] **仓库里没有明文 token**
  ```sh
  cd /usr/local/src/lightmem && git grep -nE 'LIMS_AGENT_TOKENS=.*t-' -- . ':!*.md' | head
  # 预期：无输出（只在 md 文档里出现示例）
  git status --short | grep -E 'lightmem\.env|agent-tokens' 
  # 预期：无输出（这些文件不该被跟踪）
  ```
- [ ] **强制鉴权已开**
  ```sh
  sudo grep -E '^LIMS_REQUIRE_AUTH=1$' /opt/lightmem/lightmem.env
  # 预期：有这一行
  curl -s -o /dev/null -w '%{http_code}\n' -X POST http://127.0.0.1:7443/memories -d '{}' 
  # 预期：401（不带 token 必须被拒）
  ```
- [ ] **PG 口令只在 env，不在 profile / 命令行历史**
  ```sh
  sudo grep -nE 'password' /opt/lightmem/lims.profile.yaml | grep -v '^.*#'
  # 预期：无输出（profile 里不写口令）
  sudo grep -c '^LIMS_PG_PASSWORD=' /opt/lightmem/lightmem.env
  # 预期：1
  ```
- [ ] **token 轮换有路径**
  ```sh
  # 换发：重跑 issue_token.py → 更新 env → 重启；旧 token 从表里删掉即失效
  python3 /usr/local/src/lightmem/integrations/agent-onboarding/issue_token.py --agent trae --quiet | head -c 8
  # 预期：打印新 token 前缀（不要把这个输出贴到工单里）
  ```

---

## 11. TLS 与反向代理

- [ ] **对外只有 443，且证书私钥 0600**
  ```sh
  sudo ls -l /etc/lightmem/tls/
  # 预期：lightmem.key -rw-------，lightmem.crt -rw-r--r--
  ```
- [ ] **协议与套件不退化**
  ```sh
  openssl s_client -connect 127.0.0.1:443 -servername lims.lan </dev/null 2>/dev/null | grep -E 'Protocol|Cipher'
  # 预期：Protocol 为 TLSv1.2 或 TLSv1.3；不出现 SSLv3/TLSv1
  ```
- [ ] **HSTS 与跳转**
  ```sh
  curl -skI https://127.0.0.1/healthz | grep -iE 'strict-transport-security|^HTTP'
  # 预期：HTTP/1.1 200 或 2xx；有 strict-transport-security 头
  ```
- [ ] **反代不泄露版本号**
  ```sh
  curl -skI https://127.0.0.1/ | grep -i '^server'
  # 预期：server_tokens off（显示 nginx 而非 nginx/1.24.0）
  ```

---

## 12. 日志权限

- [ ] **journald 持久化且不被普通用户清**
  ```sh
  grep -E '^Storage=' /etc/systemd/journald.conf
  # 预期：Storage=persistent
  sudo journalctl --disk-usage
  ```
- [ ] **落盘日志目录 0750**
  ```sh
  sudo ls -ld /opt/lightmem/logs
  # 预期：drwxr-x--- lightmem lightmem
  ```
- [ ] **日志里没有明文凭据**
  ```sh
  sudo journalctl -u lightmem.service --since '1 hour ago' | grep -icE 'token|password'
  # 预期：0（若 >0，先查是不是 stdout 打印了 env）
  ```
- [ ] **日志轮转有上限**（避免打满盘导致记忆库写失败）
  ```sh
  sudo tee /etc/logrotate.d/lightmem >/dev/null <<'EOF'
  /opt/lightmem/logs/*.log {
      weekly
      rotate 8
      compress
      missingok
      notifempty
      create 0640 lightmem lightmem
  }
  EOF
  sudo logrotate -d /etc/logrotate.d/lightmem 2>&1 | tail -5
  # 预期：dry-run 无 error
  ```

---

## 13. 收尾验证清单

| 检查项 | 验证命令 | 预期 |
|---|---|---|
| 服务以非 root 运行 | `ps -o user= -p $(systemctl show -p MainPID --value lightmem.service)` | `lightmem` |
| 沙箱生效 | `grep NoNewPrivs /proc/$(systemctl show -p MainPID --value lightmem.service)/status` | `NoNewPrivs: 1` |
| 可写面仅白名单 | `systemctl show lightmem.service -p ReadWritePaths` | `data` + `logs` |
| 数据目录加密 | `sudo fscrypt status /opt/lightmem/data` 或 `cryptsetup status lims_crypt` | `Encrypted: Yes` / `type: LUKS2` |
| SELinux（Rocky） | `getenforce` | `Enforcing` |
| ACL 生效 | `getfacl /opt/lightmem/data` | 有 `lims_reader` 条目 |
| 端口只在回环 | `ss -lntp \| grep -E ':(7443\|7444)'` | 两条 `127.0.0.1` |
| 防火墙默认拒绝 | `sudo ufw status verbose` / `firewall-cmd --list-all` | `default deny incoming` |
| 未鉴权被拒 | `curl -s -o /dev/null -w '%{http_code}' -XPOST 127.0.0.1:7443/memories -d '{}'` | `401` |
| env 权限 | `sudo ls -l /opt/lightmem/lightmem.env` | `-rw------- root lightmem` |
| 无明文口令 | `sudo grep -n password /opt/lightmem/lims.profile.yaml` | 无输出 |
| 审计在跑 | `sudo auditctl -s \| grep enabled` | `enabled 1` |
| 自动更新在跑 | `systemctl is-active unattended-upgrades` / `systemctl list-timers dnf-automatic.timer` | `active` / 有下次触发 |
| 探针正常 | `curl -s -o /dev/null -w '%{http_code}' 127.0.0.1:7443/readyz` | `200` |
| 日志无凭据 | `journalctl -u lightmem \| grep -ic token` | `0` |

---

## 14. 完成判定

本 checklist 视为通过，需**同时**满足：

1. 第 13 节所有验证项均为预期值（贴实际输出，不贴本文预期）；
2. 无 `permission_level=机密` 数据落在未加密的目录/分区上（第 4 节方案已落地）；
3. 服务进程以非 root 运行且 `NoNewPrivs: 1`，可写面只有 `/opt/lightmem/{data,logs}`；
4. 7443/7444 只在回环；如确需内网直连，`LIMS_REQUIRE_AUTH=1` 已开且仅对指定网段放行；
5. 凭据只存在于 `0600` 的 env 文件，profile 与仓库内无明文；
6. 配置已持久化（`/etc/sysctl.d/`、unit drop-in、`/etc/fstab`、`/etc/crypttab`、`/etc/systemd/journald.conf`、防火墙 `--permanent` 或 `ufw` 规则），**重启后仍生效**；
7. 第 3 节的「与 jail 不等价」边界已写入评审差异表（不是勾完就算）。

---

## 15. 附录 A：快速回滚

| 场景 | 回滚命令 |
|---|---|
| 防火墙把 SSH 锁了 | 本地控制台 `sudo ufw disable` / `sudo systemctl stop firewalld` |
| SELinux 标签导致服务读不到数据 | `sudo restorecon -Rv /opt/lightmem`；仍不行则 `sudo setenforce 0` **临时**取证，事后必须回到 Enforcing |
| LUKS 挂不上 | `sudo cryptsetup open /dev/sdXN lims_crypt`（需要 keyfile 或口令）；先确认 `/etc/crypttab` 路径 |
| fscrypt 目录解不开 | `sudo fscrypt unlock /opt/lightmem/data --user=lightmem` |
| systemd 226/NAMESPACE | 检查 `ReadWritePaths` 目录是否存在；`systemd-analyze verify` |
| 加固后服务起不来 | `sudo systemctl edit lightmem.service` 逐项注释可疑沙箱项 → `daemon-reload` → `restart`，定位到具体一项后再决定长期策略 |
| sysctl 影响业务 | `sudo sysctl -w kernel.yama.ptrace_scope=0`（临时），并从 `/etc/sysctl.d/99-lightmem-hardening.conf` 移除该行 |

## 16. 附录 B：与 FreeBSD checklist 逐节对照

| FreeBSD 版 | Linux 版（本文） | 说明 |
|---|---|---|
| §1 前置（`freebsd-version` / `zpool list`） | §1 前置（`os-release` / `findmnt`） | 版本与文件系统底座 |
| §2 最小安装（`sysrc` 关服务） | §2 最小安装（`systemctl disable`） | 同意图 |
| §2 系统更新（`freebsd-update`） | §2 更新（`apt full-upgrade` / `dnf upgrade`）+ 自动更新 | Linux 多一层自动安全更新 |
| §3 jail + vnet | §3 systemd 沙箱 + 专用用户 | **不等价**，见 §3 边界声明 |
| §4 ZFS dataset + encryption | §4 目录规划 + LUKS/fscrypt | Linux 无 dataset 概念 |
| §5 MAC（`mac_lomac`） | §5 SELinux / AppArmor | Rocky 可打标，Ubuntu 无专用 profile |
| §6 ACL（`setfacl`） | §6 POSIX ACL（`setfacl`/`getfacl`） | 命令同名、语义略异（继承用 default ACL） |
| §7 pf + IPsec | §7 ufw / firewalld / nftables | 单机形态无 IPsec 需求 |
| §8 `pkg audit` + `accton` | §8 apt/dnf + auditd + acct/psacct | 同意图 |
| §9 `sysctl security.bsd.*` | §9 `sysctl kernel.*` | 参数体系不同 |
| §10 收尾验证 | §13 收尾验证 | 本文多三项：端口回环、未鉴权 401、日志无凭据 |

## 17. 附录 C：与架构文档的映射

| 本文 | lightmem.md |
|---|---|
| §2 最小安装 | §5.6「为什么 FreeBSD + ZFS」的 Linux 侧对应（ext4/xfs） |
| §3 进程隔离 | §5.7「进程隔离」 |
| §4 目录与加密 | §5.6「dataset 布局」+ §5.7「磁盘加密」 |
| §5 MAC | §5.7「强制访问控制」 |
| §6 ACL | §5.7「文件级访问」 |
| §7 防火墙 | §5.7「网络隔离」 |
| §8 漏洞/系统审计 | §5.7「漏洞审计 / 系统审计」 |
| §9 最小权限 | §5.7「系统加固」 |
| §10 凭据 | §7.1 权限三元组（应用层）+ §5.7 密钥管理 |
| §11 TLS | §5.7「TLS 终结」 |
| 全部 | §7「安全、隐私与数据主权」业务层约束（OS 层无法替代） |
