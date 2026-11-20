# 光明记忆（LIMS）Linux 部署手册（Ubuntu / Debian / Rocky）

**文档版本：** v1.0
**对应架构文档：** `lightmem/lightmem.md` v2.0（§2.6 可缩放模式、§5.4 存储后端、§5.7 安全）
**适用平台：** Ubuntu 24.04 LTS / Debian 12 (bookworm) / Rocky Linux 9（x86_64 与 arm64）
**对应档位：** `profiles/lims-linux-solo.profile.yaml`（SQLite）、`profiles/lims-linux-family.profile.yaml`（PG + pgvector）
**姊妹文档：**
- `manuals/p1-freebsd-zfs-setup.md`（FreeBSD + ZFS + jail 版，本文的对照组）
- `checklists/linux-security-checklist.md`（本文配套的安全加固清单）
- `deploy/lightmem.service`、`deploy/lightmem-mcp.service`（systemd 单元真源）
- `deploy/init-roles.sql`（应用运行角色 `lightmem_app` bootstrap）

**目标读者：** 在 Linux 上自建记忆服务的个人 / 家庭实验室 / 小团队运维。

---

## 0. 概述与证据边界（先读这一节）

### 0.1 本文解决什么问题

Go 内核（`kernel/`）本身跨平台，缺的只是「Linux 档位 + 部署文档」：

| 项目 | FreeBSD（已有） | Linux（本文） |
|---|---|---|
| 档位文件 | `lims-solo/family/smb/enterprise` | `lims-linux-solo`、`lims-linux-family` |
| 进程隔离 | jail + vnet | systemd 沙箱（无 jail） |
| 数据底座 | ZFS dataset + encryption | ext4 / xfs（+ LUKS/fscrypt 按需） |
| 防火墙 | pf | ufw / firewalld / nftables |
| 服务管理 | rc.d | systemd |
| 端口口径 | **18443 / 18444** | **7443 / 7444** |

> **端口口径不得混用。** 7443/7444 是 systemd 与 quickstart 的口径（见 `deploy/lightmem.service` 注释），18443/18444 是 FreeBSD rc.d 的口径。混用会导致「照着 FreeBSD 手册在 Linux 上 curl 18443 得到 connection refused」这类假故障。

### 0.2 哪些是实测的，哪些是照上游文档写的

本手册区分两种证据等级，请按需核对：

- **【实测】** 标出的结论，均在 **Ubuntu 24.04 userland + Linux/amd64 静态二进制**下真跑过（2026-11-20，本次 T-0414 交付附带记录，详见附录 C）。宿主为 FreeBSD 15.1 的 Linux 兼容层，**libc、FHS 路径、`/opt` 布局、syscall 行为与真机 Ubuntu 24.04 一致**，但 systemd 单元、apt/dnf 安装这两类**只能在真机验证**，因此标为【未实测】。
- **【未实测】** 标出的步骤（apt/dnf 装包、systemd 启停、防火墙、SELinux 打标）按发行版官方与 PGDG 官方安装方式书写，命令本身来自上游文档，本环境无 Linux 真机可跑。首次在生产上执行时请按 §13 排障逐条核对。

> 铁律（沿用本仓惯例）：**未实测的不写成既成事实**。凡是本文写「预期 …」的地方，都是「应该如此」，跑出来不一致请先查 §13，再来改文档。

### 0.3 两条部署路线

| 路线 | 档位 | 外部依赖 | 适合 |
|---|---|---|---|
| A. 极简单机 | `lims-linux-solo` | 无（SQLite 纯 Go 驱动，无 cgo、无系统库） | 个人工作站 / 单台 VPS / 先跑通再升档 |
| B. 单机共享 | `lims-linux-family` | PostgreSQL 18 + pgvector | 家庭实验室 / 小团队，1–5 人，十万级记忆 |

两条路线**二进制完全相同**，只差 profile 与环境变量。建议先走 A 跑通 `/healthz`，再按 §5 装 PG 升到 B。

---

## 1. 目标拓扑

### 1.1 路线 A：Solo（单进程 + SQLite）

```
┌─────────────────────────────── Linux 主机 ───────────────────────────────┐
│                                                                          │
│  systemd: lightmem.service ──► /opt/lightmem/lightmem-kernel             │
│             User=lightmem         ├─ profile: lims-linux-solo            │
│             :7443 (127.0.0.1)     ├─ SQLite: /opt/lightmem/data/*.sqlite │
│                                   └─ 归档:  /opt/lightmem/data/archive/  │
│                                                                          │
│  systemd: lightmem-mcp.service ──► /opt/lightmem/lightmem-mcp :7444      │
│                                     └─ -rest-base http://127.0.0.1:7443  │
└──────────────────────────────────────────────────────────────────────────┘
```

### 1.2 路线 B：Family（PG 与内核同机）

```
┌─────────────────────────────── Linux 主机 ───────────────────────────────┐
│  postgresql-18.service :5432 (127.0.0.1)                                 │
│      └─ db lims, 角色 lightmem_app（NOSUPERUSER）, extension vector       │
│                                                                          │
│  systemd: lightmem.service ──► lightmem-kernel :7443                     │
│             profile: lims-linux-family                                   │
│             └─ storage_backend=pgvector → 127.0.0.1:5432/lims            │
│                                                                          │
│  systemd: lightmem-mcp.service ──► lightmem-mcp :7444                    │
└──────────────────────────────────────────────────────────────────────────┘
```

与 FreeBSD Family 的拓扑差异：**没有 jail、没有 vnet 内网段、没有 MinIO/Redis/RabbitMQ**。PG 与内核同机、只走回环；缺的隔离由 systemd 沙箱 + 专用用户 + 文件权限补，缺的加密由 LUKS/fscrypt 补（清单第 4 节）。

---

## 2. 前置条件

```sh
# 发行版与架构
cat /etc/os-release | head -3
uname -m          # 预期 x86_64 或 aarch64
```

| 项 | 最低 | 建议 |
|---|---|---|
| CPU | 1 核 | 2 核（PG + 内核同机时） |
| 内存 | 1 GB（Solo） | 4 GB（Family，PG 共享缓冲 1 GB） |
| 磁盘 | 2 GB | 20 GB（记忆 + WAL + 归档） |
| 权限 | root 或 sudo | — |
| 网络 | 仅本机访问即可 | 对外发布需反向代理 + 证书 |

其他前置：

- **时区与时间同步**（审计时间戳一致性）：
  ```sh
  timedatectl set-timezone Asia/Shanghai
  timedatectl set-ntp true
  timedatectl status | grep -E 'Time zone|System clock synchronized'
  # 预期：Asia/Shanghai (CST, +0800) / synchronized: yes
  ```
- **SELinux（Rocky 9 默认 enforcing）**：先记住状态，§9 起服务后若被拦再按 §13.7 处置。
  ```sh
  getenforce        # 预期 Enforcing（Rocky）/ 无此命令（Ubuntu、Debian）
  aa-status --enabled && echo apparmor_on   # Ubuntu/Debian 的对应物
  ```

---

## 3. 安装 Go 1.25

**为什么不用 `apt install golang` / `dnf install golang`：** 发行版自带的 Go 通常落后（Ubuntu 24.04 仓库是 1.22、Rocky 9 是 1.21 一线），而 `go.mod` 要求 `go 1.25.0`，编译器过旧会直接拒绝构建。用官方 tarball。

```sh
# 1) 取版本号（写此手册时本仓实测用的是 go1.25.14）
GO_VER=1.25.14
ARCH=$(uname -m); case "$ARCH" in x86_64) GOARCH=amd64 ;; aarch64) GOARCH=arm64 ;; esac

# 2) 下载并校验（sha256 从官方 json 清单取，不写死在本文里）
cd /tmp
curl -fsSLO "https://go.dev/dl/go${GO_VER}.linux-${GOARCH}.tar.gz"
curl -fsSL "https://go.dev/dl/?mode=json&include=all" \
  | grep -A6 "\"go${GO_VER}.linux-${GOARCH}.tar.gz\"" | grep -m1 sha256
sha256sum "go${GO_VER}.linux-${GOARCH}.tar.gz"
# 预期：两行 sha256 完全一致

# 3) 解压到 /usr/local（覆盖旧版前先 rm -rf /usr/local/go）
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf "go${GO_VER}.linux-${GOARCH}.tar.gz"

# 4) PATH 与验证
sudo tee /etc/profile.d/go.sh >/dev/null <<'EOF'
export PATH=$PATH:/usr/local/go/bin
EOF
. /etc/profile.d/go.sh
go version
# 预期：go version go1.25.14 linux/amd64
```

> **只有构建需要 Go。** 运行期不需要 Go 工具链；生产机可以「构建机编译 → 拷贝静态二进制」，见 §7.2。

---

## 4. 创建运行用户与目录布局

```sh
# 1) 专用系统用户：无登录 shell、无家目录
sudo useradd --system --no-create-home --shell /usr/sbin/nologin lightmem
id lightmem
# 预期：uid=... (lightmem) gid=... (lightmem) groups=...,...(lightmem)

# 2) 目录布局（与 deploy/lightmem.service 的 WorkingDirectory/ReadWritePaths 严格一致）
sudo install -d -o root      -g lightmem -m 0750 /opt/lightmem
sudo install -d -o lightmem  -g lightmem -m 0750 /opt/lightmem/data
sudo install -d -o lightmem  -g lightmem -m 0750 /opt/lightmem/logs
sudo install -d -o root      -g lightmem -m 0750 /opt/lightmem/profiles
ls -ld /opt/lightmem /opt/lightmem/{data,logs,profiles}
```

| 路径 | 属主 | 权限 | 用途 | 为什么是这个值 |
|---|---|---|---|---|
| `/opt/lightmem` | root:lightmem | 0750 | 安装前缀 | 二进制不归运行用户所有，改不了自己 |
| `/opt/lightmem/data` | lightmem:lightmem | 0750 | SQLite / 归档 | unit 里 `ReadWritePaths` 白名单之一 |
| `/opt/lightmem/logs` | lightmem:lightmem | 0750 | 落盘日志 | 同上；`ProtectSystem=strict` 下只有这两个可写 |
| `/opt/lightmem/profiles` | root:lightmem | 0750 | profile YAML | 配置只读，防服务进程自改 |
| `/opt/lightmem/lightmem.env` | root:lightmem | **0600** | 凭据 | 内含 token / PG 口令 |

> **踩坑位（本卡实测踩到）**：unit 里 `ProtectSystem=strict` + `ReadWritePaths=/opt/lightmem/data /opt/lightmem/logs`，意味着**除这两处以外全盘只读**。如果 profile 里把 SQLite 路径写成 `/var/lib/lightmem/lims.sqlite` 或相对路径 `./data/lims.sqlite`，服务会以
> `[boot] sqlite mkdir <dir>: mkdir /xxx: permission denied` 起不来 —— 这不是权限配错，是沙箱按设计拦下来的。要么改路径到白名单内，要么在 unit 里补 `ReadWritePaths`（不推荐，白名单越短越安全）。

---

## 5. 安装 PostgreSQL 18 + pgvector（仅路线 B）

FreeBSD 侧用 `pkg install postgresql18-server postgresql18-contrib pgvector…`；Linux 侧**必须走 PGDG 官方源**，因为发行版自带源里的 PG 与 pgvector 都偏旧（Ubuntu 24.04 自带 PG 16 + pgvector 0.6，缺 0.8.2 之后的一批索引/缓冲修复）。

### 5.1 Ubuntu 24.04 / Debian 12（apt）

```sh
# 1) 装 postgresql-common 并添加 PGDG 源（官方脚本自己写 key 与 sources.list）
sudo apt update
sudo apt install -y postgresql-common
sudo /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh

# 2) 装 PG 18 与 pgvector
sudo apt update
sudo apt install -y postgresql-18 postgresql-18-pgvector

# 3) 验证版本（Debian 系装包即自动 initdb 并创建 main 集群）
psql --version
# 预期：psql (PostgreSQL) 18.x
sudo -u postgres psql -tAc "select version()"
```

### 5.2 Rocky Linux 9（dnf）

Rocky 9 的 AppStream 里有 `postgresql` 模块，会**遮蔽** PGDG 的包，必须先 disable，否则 `dnf install postgresql18-server` 报 `No match for argument`。

```sh
# 1) 加 PGDG 仓库（EL-9；EL-10 把 URL 里的 EL-9 换成 EL-10）
sudo dnf install -y https://download.postgresql.org/pub/repos/yum/reporpms/EL-9-x86_64/pgdg-redhat-repo-latest.noarch.rpm

# 2) 屏蔽发行版模块，避免版本遮蔽
sudo dnf -qy module disable postgresql

# 3) 装服务端与 pgvector（扩展包按 PG 大版本后缀命名：pgvector_18）
sudo dnf install -y postgresql18-server pgvector_18

# 4) 初始化集群并开机自启（Rocky 不会自动 initdb）
sudo /usr/pgsql-18/bin/postgresql-18-setup initdb
sudo systemctl enable --now postgresql-18
systemctl is-active postgresql-18
# 预期：active
```

> Rocky 的 PGDATA 在 `/var/lib/pgsql/18/data`，配置文件在 `/var/lib/pgsql/18/data/postgresql.conf`；Debian 系在 `/etc/postgresql/18/main/`。§5.4 调优时别写错路径。

### 5.3 建库、建扩展、建应用角色

```sh
# 1) 建库
sudo -u postgres psql -c "CREATE DATABASE lims;"

# 2) 启用 pgvector（必须装完 postgresql-18-pgvector / pgvector_18 才有这个 extension）
sudo -u postgres psql -d lims -c "CREATE EXTENSION IF NOT EXISTS vector;"
sudo -u postgres psql -d lims -tAc "select extversion from pg_extension where extname='vector'"
# 预期：0.8.x（本仓另在 FreeBSD 实测为 0.8.2）

# 3) 建**非超级用户**运行角色（口令从 secrets 注入，不要写进命令行历史）
#    脚本在仓库里：deploy/init-roles.sql（幂等，可重复执行）
export LIGHTMEM_APP_PASSWORD="$(openssl rand -base64 24)"
sudo -u postgres psql -d lims -v app_password="$LIGHTMEM_APP_PASSWORD" -f /opt/lightmem/deploy/init-roles.sql
# 预期末尾：
#    rolname      | can_login | is_superuser
#    lightmem_app | t         | f
```

**为什么必须是 `lightmem_app` 而不是 `postgres`：** 内核连上后会做启动自检（`kernel/pg_superuser_check.go`）：

- `current_user` 是超级用户 → **默认 fail-fast，拒绝启动**（否则 RLS、`pg_read_all_data` 等库层约束形同虚设，多租户隔离退化为「只靠应用层 SQL 谓词」）；
- 非超级用户 → 日志打印 `[pg-superuser-check] policy=fail: current_user 非 superuser — 通过`（本卡实测到这一行）。

### 5.4 PG 参数（单机 4 GB 内存的量级）

```ini
# /etc/postgresql/18/main/conf.d/lightmem.conf（Debian 系）
# /var/lib/pgsql/18/data/postgresql.conf（Rocky）
listen_addresses = '127.0.0.1'      # 与内核同机，绝不出网卡
shared_buffers = 1GB
effective_cache_size = 3GB
maintenance_work_mem = 256MB
work_mem = 16MB
max_connections = 50                # pool_size=10，留足 psql 排障余量
wal_compression = on
# HNSW 建索引吃内存，首次建库时临时调大
maintenance_work_mem = 512MB
```

```sh
# Debian 系
sudo systemctl restart postgresql
# Rocky
sudo systemctl restart postgresql-18
pg_isready -h 127.0.0.1 -p 5432
# 预期：127.0.0.1:5432 - accepting connections
```

> **不要**照抄 FreeBSD 手册里与 ZFS 协同的 `full_page_writes` / `sync=always` 组合 —— Linux 上没有 ZFS dataset 的 `sync` 属性可配合，关掉 `full_page_writes` 只会换来「断电后可能坏页」。Linux 保持默认 `on`。

---

## 6. 数据目录与文件系统（ext4 / xfs，不用 ZFS）

Linux 档位**显式** `storage.zfs.enabled: false`。数据落在普通文件系统上，按下面的规划放：

| 数据 | 默认路径 | 说明 |
|---|---|---|
| SQLite 库（Solo） | `/opt/lightmem/data/lims.sqlite` | profile 里写死的绝对路径，`-wal`/`-shm` 同目录 |
| 归档（Solo/Family） | `/opt/lightmem/data/archive/` | `storage.archive.backend: local` |
| PG 数据（Family） | `/var/lib/postgresql/18/main`（Debian）/ `/var/lib/pgsql/18/data`（Rocky） | PG 自己管，**不要**塞进 `/opt` |
| 日志 | journald（+ 可选 `/opt/lightmem/logs`） | 见 §9.4 |

```sh
# 1) 看文件系统类型与挂载选项
findmnt -no FSTYPE,OPTIONS /            # 预期 ext4 或 xfs
# 2) 关 atime（减少元数据写入；ext4/xfs 都支持）
#    /etc/fstab 里给对应分区加 noatime，然后
sudo mount -o remount /
findmnt -no OPTIONS / | tr ',' '\n' | grep noatime
```

**静态加密（替代 ZFS encryption）**，二选一：

```sh
# 方案 1：块层 LUKS（整盘/整分区，最省心；需在装机时或离线做）
sudo cryptsetup luksFormat /dev/sdXN
sudo cryptsetup open /dev/sdXN lims_crypt
sudo mkfs.ext4 /dev/mapper/lims_crypt
# /etc/crypttab 里登记，密钥可用 keyfile（0600, root）或交互口令

# 方案 2：目录层 fscrypt（只加密 /opt/lightmem/data，ext4/xfs 需支持）
sudo apt install -y fscrypt        # Rocky: sudo dnf install -y fscrypt
sudo fscrypt setup
sudo fscrypt setup /opt
sudo fscrypt encrypt /opt/lightmem/data --user=lightmem
```

> 加密方式变了，`security.zfs_encryption` 仍保持 `false`（它表达的是「ZFS 加密是否启用」，不是「有没有加密」）；是否加密由你在 OS 层决定，清单第 4 节负责验收。

---

## 7. 编译与安装

### 7.1 在目标机编译

```sh
# 1) 取代码
sudo install -d -o "$USER" -m 0755 /usr/local/src/lightmem
cd /usr/local/src && git clone <仓库地址> lightmem && cd lightmem

# 2) 编译（注意：内核是**独立子模块**，必须进 kernel/ 目录构建；
#    在仓库根目录跑 go build ./kernel/... 会报 "main module (lightmem) does not contain package"）
cd kernel
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /tmp/lightmem-kernel ./cmd/lightmem-kernel
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /tmp/lightmem-mcp    ./cmd/lightmem-mcp
file /tmp/lightmem-kernel
# 预期：ELF 64-bit LSB executable, x86-64, statically linked

# 3) 安装二进制与配置
sudo install -o root -g root -m 0755 /tmp/lightmem-kernel /opt/lightmem/lightmem-kernel
sudo install -o root -g root -m 0755 /tmp/lightmem-mcp    /opt/lightmem/lightmem-mcp
sudo install -o root -g lightmem -m 0640 profiles/lims-linux-family.profile.yaml /opt/lightmem/profiles/
sudo install -o root -g lightmem -m 0640 profiles/lims-linux-solo.profile.yaml   /opt/lightmem/profiles/
sudo install -o root -g lightmem -m 0640 deploy/init-roles.sql /opt/lightmem/deploy/
```

`CGO_ENABLED=0` 不是可选项：SQLite 后端用纯 Go 驱动 `modernc.org/sqlite`，关掉 cgo 才能得到**静态、无 libc/glibc 版本依赖**的二进制，跨发行版、跨 amd64/arm64 拷来就能跑（本卡实测用 `GOOS=linux GOARCH=amd64` 交叉编译出的静态 ELF，在 Ubuntu 24.04 userland 直接启动成功）。

### 7.2 构建机交叉编译（可选，生产机不装 Go）

```sh
# 在任意装了 go1.25 的机器上
cd kernel
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o lightmem-kernel-linux-amd64 ./cmd/lightmem-kernel
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o lightmem-kernel-linux-arm64 ./cmd/lightmem-kernel
sha256sum lightmem-kernel-linux-*
# 拷到目标机后校验 sha256，再 install 到 /opt/lightmem
```

---

## 8. 配置 profile 与凭据

### 8.1 选档位

```sh
# 路线 A
sudo cp /opt/lightmem/profiles/lims-linux-solo.profile.yaml   /opt/lightmem/lims.profile.yaml
# 路线 B
sudo cp /opt/lightmem/profiles/lims-linux-family.profile.yaml /opt/lightmem/lims.profile.yaml
sudo chown root:lightmem /opt/lightmem/lims.profile.yaml && sudo chmod 0640 /opt/lightmem/lims.profile.yaml
```

两档的差异只在 `storage_backend`（sqlite / pgvector）与 `security.jail: false` 外的少量开关，逐字段对照见 §15。

> **profile 里没有 `platform: linux` 字段，是有意的。** `profiles/lims.profile.schema.json` 是 draft-07 且顶层 `additionalProperties: false`，其 `properties` 中没有 `platform`；内核 `kernel/profile.go` 的 `yaml.Unmarshal` 也不解析它。硬加只会有两种结果：严格校验器报 `additionalProperties` 错误，或内核静默忽略 —— 都是误导。平台信息由文件名 `lims-linux-*`、注释块与 `jail: false` 表达。
>
> **注意 schema 目前不是加载闸门**：用 `jsonschema` 严格跑一遍，六份 profile 全都报 `auth` 不在 schema 里（R21-B 加了 `auth:` 段但没回填 schema），两份 family 还多报一处 `storage.pg.user`。`LoadProfile` 本身只做 YAML 解析 + 少量语义校验（`profile` 枚举、`storage_backend`、pg 档必须有 `host`、三个不可裁剪特性为 true），并不读 schema。Linux 两份档位的漂移点与各自 FreeBSD 同名档位**完全一致**，没有新增漂移类别。

### 8.2 凭据文件（0600，唯一入口）

```sh
# 1) 建空文件并锁权限
sudo install -m 600 -o root -g lightmem /dev/null /opt/lightmem/lightmem.env

# 2) 签发 agent token（零依赖脚本，产出整表）
sudo -u lightmem python3 /usr/local/src/lightmem/integrations/agent-onboarding/issue_token.py \
     --tenant myteam --user "$USER" --agents dsh,trae,workbuddy \
     --out /opt/lightmem/agent-tokens

# 3) 把整表塞进 env（JSON 单行；用 python 压缩避免手写引号出错）
sudo -u lightmem python3 - <<'PY' | sudo tee -a /opt/lightmem/lightmem.env >/dev/null
import json
d = json.load(open('/opt/lightmem/agent-tokens/agent-tokens.json', encoding='utf-8'))
print('LIMS_AGENT_TOKENS=' + json.dumps(d, separators=(',', ':'), ensure_ascii=False))
PY

# 4) 其余变量
sudo tee -a /opt/lightmem/lightmem.env >/dev/null <<'EOF'
# 强制鉴权：未带 token 一律 401（生产必须开）
LIMS_REQUIRE_AUTH=1
# 路线 B 才需要：PG 口令（与 init-roles.sql 的 app_password 一致）
#LIMS_PG_PASSWORD=<改我>
#LIMS_PG_HOST=127.0.0.1
#LIMS_PG_USER=lightmem_app
#LIMS_PG_DB=lims
EOF

# 5) 复核：只有 root/lightmem 可读
sudo chown root:lightmem /opt/lightmem/lightmem.env && sudo chmod 600 /opt/lightmem/lightmem.env
sudo ls -l /opt/lightmem/lightmem.env
# 预期：-rw------- 1 root lightmem ... lightmem.env
```

> **别把 token 写进 profile。** profile 是 `0640` 的配置文件、会进代码仓；`LIMS_AGENT_TOKENS` 走 env，env 是 `0600` 且被 `.gitignore` 覆盖。`kernel/auth.go` 的优先级是 **env > profile.auth.agent_tokens**，两处都写时以 env 为准。

---

## 9. systemd 双服务

### 9.1 安装单元

```sh
sudo install -o root -g root -m 0644 deploy/lightmem.service     /etc/systemd/system/lightmem.service
sudo install -o root -g root -m 0644 deploy/lightmem-mcp.service /etc/systemd/system/lightmem-mcp.service
sudo systemctl daemon-reload

# 只检查语法、不启动（改了 unit 之后先跑这个）
systemd-analyze verify /etc/systemd/system/lightmem.service /etc/systemd/system/lightmem-mcp.service
```

### 9.2 启停与状态

```sh
sudo systemctl enable --now lightmem.service
sudo systemctl enable --now lightmem-mcp.service

systemctl status lightmem.service --no-pager
# 预期：Active: active (running)，User=lightmem

systemctl show lightmem.service -p User -p Group -p ExecStart
# 预期：User=lightmem / Group=lightmem / ExecStart=... /opt/lightmem/lightmem-kernel -profile ...:7443
```

两个 unit 的关系：`lightmem-mcp.service` 里 `After=`/`Wants=` 依赖 kernel。**停的顺序反过来**：先 MCP 再 kernel（kernel 先死会让 MCP 反复 `on-failure` 重试）：

```sh
sudo systemctl stop lightmem-mcp.service lightmem.service    # 正序停 MCP→kernel
sudo systemctl restart lightmem.service                      # 只重启内核，MCP 会等它回来
```

### 9.3 沙箱项（这是 Linux 档的「jail 替代品」）

| unit 指令 | 作用 | 别乱关 |
|---|---|---|
| `NoNewPrivileges=true` | setuid/setgid 一律失效，拿不到提权 | — |
| `ProtectSystem=strict` | `/usr` `/boot` `/etc` 只读，可写面只剩 `ReadWritePaths` | 关掉等于放弃「被写坏系统」的最后一道闸 |
| `ProtectHome=true` | 家目录不可见 | 数据别放家目录 |
| `PrivateTmp=true` | 独立 `/tmp`，防与宿主互投毒 | 跨服务共享 `/tmp` 的临时方案会失效 |
| `PrivateDevices=true` | 隐藏真实设备节点 | 需要读 GPU/串口时才关 |
| `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6` | 只允许这几种 socket 族 | 加 AF_NETLINK 之前想清楚为什么 |
| `UMask=0027` | 新建文件 0640、目录 0750 | — |

> 对应 FreeBSD 侧的 `jail + vnet`：systemd 沙箱**不是**安全边界等价物（共用内核、共用一个用户命名空间），它拦的是「被利用后能做什么」。要更强的隔离，用 `systemd-run --property=RootDirectory=` 或容器 / 轻量 VM，但那已超出本文范围。

### 9.4 日志

```sh
# 默认走 journald，按 unit 过滤
journalctl -u lightmem.service -n 50 --no-pager
journalctl -u lightmem-mcp.service -f

# 让日志落到磁盘（重启后仍在；默认可能只在内存）
sudo mkdir -p /var/log/journal
sudo sed -i 's/^#\?Storage=.*/Storage=persistent/' /etc/systemd/journald.conf
sudo systemctl restart systemd-journald
journalctl --disk-usage
```

> 若坚持落 `/opt/lightmem/logs`（例如要接 logrotate 或采集器），记得该目录已在 `ReadWritePaths` 白名单内，且 `chmod 0750 lightmem:lightmem`；不要让日志里出现 token。

---

## 10. 端口与防火墙

### 10.1 默认口径：只听回环，不开任何端口

Linux 档位 `network.bind: 127.0.0.1`、`api_port: 7443`，MCP 是 7444。**这种情况下不需要动防火墙** —— 外部根本连不到回环地址。

```sh
ss -lntp | grep -E ':(7443|7444)'
# 预期（两条，均带 127.0.0.1）：
# LISTEN 0 4096 127.0.0.1:7443 0.0.0.0:* users:(("lightmem-kernel",pid=...))
# LISTEN 0 4096 127.0.0.1:7444 0.0.0.0:* users:(("lightmem-mcp",pid=...))
```

### 10.2 要让局域网其他机器直连（不推荐，仅内网调试）

先把 `network.bind` 改成网卡 IP（如 `192.168.1.20`），再放行。**同时必须配 token**（见 §8.2 的 `LIMS_REQUIRE_AUTH=1`），否则内核会拒绝在「非回环 + 无 token」下启动（`-allow-insecure-open` 是显式逃生阀，别用）。

```sh
# ufw（Ubuntu / Debian）
sudo ufw default deny incoming
sudo ufw default allow outgoing
sudo ufw allow 22/tcp
sudo ufw allow from 192.168.1.0/24 to any port 7443 proto tcp
sudo ufw allow from 192.168.1.0/24 to any port 7444 proto tcp
sudo ufw enable && sudo ufw status numbered

# firewalld（Rocky）
sudo firewall-cmd --permanent --add-service=ssh
sudo firewall-cmd --permanent --add-rich-rule='rule family=ipv4 source address=192.168.1.0/24 port port=7443 protocol=tcp accept'
sudo firewall-cmd --permanent --add-rich-rule='rule family=ipv4 source address=192.168.1.0/24 port port=7444 protocol=tcp accept'
sudo firewall-cmd --reload && sudo firewall-cmd --list-all

# nftables（不想装前端时）
sudo nft add table inet lims
sudo nft add chain inet lims input '{ type filter hook input priority 0; policy drop; }'
sudo nft add rule inet lims input iif lo accept
sudo nft add rule inet lims input tcp dport 22 accept
sudo nft add rule inet lims input ip saddr 192.168.1.0/24 tcp dport { 7443, 7444 } accept
```

### 10.3 推荐做法：只开 443，反代到 7443

见 §11。这样 7443/7444 永远只在回环，防火墙只需放 443（和 22）。

---

## 11. TLS 反向代理（对外发布时）

内核自带的 `security.tls: self_signed` 是**声明**，不是内置 TLS 服务器 —— 实际终结放在 nginx / caddy。这点与 FreeBSD 手册第 4.5 节同构（那边也是 nginx 终结）。

### 11.1 自签证书

```sh
sudo install -d -m 0750 /etc/lightmem/tls
sudo openssl req -x509 -newkey rsa:3072 -nodes -days 825 \
  -keyout /etc/lightmem/tls/lightmem.key \
  -out    /etc/lightmem/tls/lightmem.crt \
  -subj "/CN=lims.lan" \
  -addext "subjectAltName=DNS:lims.lan,IP:192.168.1.20"
sudo chmod 0600 /etc/lightmem/tls/lightmem.key
```

### 11.2 nginx

```nginx
# /etc/nginx/conf.d/lightmem.conf
server {
    listen 443 ssl;
    server_name lims.lan;

    ssl_certificate     /etc/lightmem/tls/lightmem.crt;
    ssl_certificate_key /etc/lightmem/tls/lightmem.key;
    ssl_protocols       TLSv1.2 TLSv1.3;
    add_header Strict-Transport-Security "max-age=31536000" always;

    client_max_body_size 8m;

    location / {
        proxy_pass         http://127.0.0.1:7443;
        proxy_http_version 1.1;
        proxy_set_header   Host              $host;
        proxy_set_header   X-Real-IP         $remote_addr;
        proxy_set_header   X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header   X-Forwarded-Proto https;
        # 长连接：MCP 走 SSE/流式时需要
        proxy_read_timeout 3600s;
        proxy_buffering    off;
    }
}
```

```sh
sudo nginx -t && sudo systemctl reload nginx
curl -sk -o /dev/null -w '%{http_code}\n' https://127.0.0.1/healthz
# 预期：200
```

### 11.3 caddy（更省事，自动证书）

```caddyfile
lims.lan {
    reverse_proxy 127.0.0.1:7443
}
```

---

## 12. 冒烟验证

按顺序跑，每步都有「预期」，不一致就跳 §13。

```sh
# 1) 服务在跑
systemctl is-active lightmem.service lightmem-mcp.service
# 预期：active / active

# 2) 端口在听，且只在回环
ss -lntp | grep -E ':(7443|7444)'

# 3) 三个探针（免鉴权，见 kernel/auth.go 的 isOperationalEndpoint）
curl -s -w '\n%{http_code}\n' http://127.0.0.1:7443/healthz
# 预期：{"backend":"sqlite","embedder":"hash","last_consolidate":"","profile":"solo","started_at":"...","status":"ok"}  + 200
curl -s -w '\n%{http_code}\n' http://127.0.0.1:7443/livez
# 预期：200（不碰任何依赖，进程活着就 200）
curl -s -w '\n%{http_code}\n' http://127.0.0.1:7443/readyz
# 预期：{"check":"","status":"ok"} + 200；503 + not_ready 说明依赖（PG）没就绪

# 4) 写一条再召回（证明后端真的落了盘 / 落了库）
TOKEN=$(python3 -c "import json;print(list(json.load(open('/opt/lightmem/agent-tokens/agent-tokens.json'))['tokens'].values())[0]['token'])" 2>/dev/null || echo "")
curl -s -o /dev/null -w 'create=%{http_code}\n' -X POST http://127.0.0.1:7443/memories \
  -H "Content-Type: application/json" ${TOKEN:+-H "Authorization: Bearer $TOKEN"} \
  -d '{"tenant_id":"smoke","user_id":"smoke","memory_kind":"fact","fact":"linux 部署冒烟","visibility":"private","confidence":0.9,"permission":{"owner":"smoke"}}'
# 预期：create=201（401 说明 token 没配对，或不带 token 但开了 REQUIRE_AUTH）

curl -s -X POST http://127.0.0.1:7443/tools/memory/recall \
  -H "Content-Type: application/json" ${TOKEN:+-H "Authorization: Bearer $TOKEN"} \
  -d '{"text":"linux 部署冒烟","tenant_id":"smoke","user_id":"smoke","top_k":3}' | head -c 300
# 预期：recalled[...] 里能看到刚写的那条 fact

# 5) 数据确实在磁盘上
sudo ls -l /opt/lightmem/data/            # 路线 A：lims.sqlite + -wal + -shm
sudo -u postgres psql -d lims -c "select count(*) from memories;"   # 路线 B
```

**失败判据（避免把假失败当产物问题）：**

- `curl: (7) Failed to connect` → 服务没起或端口不对，**先看 `journalctl -u lightmem`**，别怀疑 profile；
- `curl` 返回 `000` → 连接被拒或进程刚崩；
- 在 Windows/WSL 上跑 `grep`/`head` 判据出现 `exit 255` → 是 POSIX 工具缺失导致的假失败，换 Linux 主机执行同一条命令。

---

## 13. 排障

### 13.1 服务起不来：先看这一条命令

```sh
journalctl -u lightmem.service -n 40 --no-pager
```

按日志首行对号入座：

| 日志 | 原因 | 处置 |
|---|---|---|
| `[boot] sqlite mkdir /opt/lightmem/data: mkdir /opt: permission denied` | 沙箱拦了白名单外路径，或目录不存在/属主错 | 按 §4 建目录；确认 `ReadWritePaths` 覆盖该路径 |
| `[boot] validate profile ...: missing storage_backend` | profile 被截断/复制错文件 | `diff` 与 `profiles/` 原件 |
| `[boot] validate profile ...: features.poison_guard must be true` | 有人裁了不可裁剪特性 | 三个 `true` 不许改（§2.6 红线） |
| `[boot] storage_backend=pgvector requires storage.pg.host` | 删了 `storage.pg.host` | 补 `127.0.0.1` |
| `[pg-superuser-check] policy=fail: current_user is superuser` | 用了 `postgres` 连库 | 按 §5.3 建 `lightmem_app` 并改 `LIMS_PG_USER` |
| `[boot] listen tcp 127.0.0.1:7443: bind: address already in use` | 端口被占（常见：上一次没停干净） | `ss -lntp \| grep 7443` 找 PID，`systemctl stop` 后重试 |

### 13.2 `readyz` 503

```sh
curl -s http://127.0.0.1:7443/readyz
pg_isready -h 127.0.0.1 -p 5432
sudo -u postgres psql -d lims -tAc "select extversion from pg_extension where extname='vector'"
```
逐项对：PG 在不在 → 库在不在 → `vector` 扩展装没装（`0A000 extension "vector" is not available` 就是没装 `postgresql-18-pgvector` / `pgvector_18`）。

### 13.3 401 全挂

```sh
sudo grep -c LIMS_AGENT_TOKENS /opt/lightmem/lightmem.env   # 0 说明没写进去
journalctl -u lightmem.service | grep -i auth | tail -5
# 预期看到： [auth] LIMS_AGENT_TOKENS ... 已加载 N 个 token（或 未配置，透传）
```
JSON 里多一个逗号就会整表解析失败并退化为「未配置」——用 §8.2 的 python 压行法，别手写。

### 13.4 时间不对 / 审计时间戳跳变

```sh
timedatectl status
journalctl --since "10 min ago" | head -3   # 看时间戳是否合理
sudo systemctl status systemd-timesyncd     # Ubuntu/Debian
sudo systemctl status chronyd               # Rocky
```

### 13.5 二进制跑不起来（`Exec format error` / `not found`）

```sh
file /opt/lightmem/lightmem-kernel       # 必须是 ELF ... x86-64 或 aarch64，与 uname -m 一致
ldd /opt/lightmem/lightmem-kernel        # 预期：not a dynamic executable（静态）
```

### 13.6 磁盘满 / WAL 涨

```sh
df -h /opt /var
sudo du -sh /opt/lightmem/data/*          # Solo：-wal 文件异常大可手动 checkpoint
sudo -u postgres psql -d lims -c "select pg_size_pretty(pg_database_size('lims'));"
```

### 13.7 Rocky 9：SELinux 拦截

```sh
sudo ausearch -m avc -ts recent | tail -20        # 有 avc denied 才是 SELinux
sudo semanage fcontext -a -t var_lib_t "/opt/lightmem/data(/.*)?"
sudo restorecon -Rv /opt/lightmem/data
```
> `var_lib_t` 是「服务自有可变数据」的通用标签，适合 `/opt/*/data`。**不要**图省事 `setenforce 0` —— 那等于在清单上勾了「SELinux 已启用」却什么也没保护。

### 13.8 systemd 报 `status=226/NAMESPACE`

多半是 `ReadWritePaths` 指向的目录不存在。`ProtectSystem=strict` 下 systemd 会在起服务前先建命名空间，路径缺失直接 226。

```sh
sudo systemd-analyze verify /etc/systemd/system/lightmem.service
ls -ld /opt/lightmem/data /opt/lightmem/logs
```

---

## 14. 升级、回滚、卸载

### 14.1 升级二进制（配置与数据不动）

```sh
sudo systemctl stop lightmem-mcp.service lightmem.service
sudo cp /opt/lightmem/lightmem-kernel /opt/lightmem/lightmem-kernel.bak.$(date +%F)
sudo install -o root -g root -m 0755 /tmp/lightmem-kernel /opt/lightmem/lightmem-kernel
sudo systemctl start lightmem.service lightmem-mcp.service
systemctl is-active lightmem.service && curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:7443/healthz
```

回滚就是 `install` 回 `.bak` 那个文件，再 `systemctl restart`。

### 14.2 Solo → Family（升档）

1. 按 §5 装好 PG 18 + pgvector，建库、建 `lightmem_app`、`CREATE EXTENSION vector`；
2. `POST /api/v1/gdpr/export` 从 Solo 导出（元数据先落库，`vector_status=pending`，向量可异步补全）；
3. 把 `lims.profile.yaml` 换成 `lims-linux-family.profile.yaml`，按 §8 补 `LIMS_PG_*`；
4. `systemctl restart lightmem lightmem-mcp`，用 §12 复验；
5. 旧 SQLite 文件保留一周再删。

### 14.3 卸载

```sh
sudo systemctl disable --now lightmem-mcp.service lightmem.service
sudo rm -f /etc/systemd/system/lightmem{,-mcp}.service
sudo systemctl daemon-reload && sudo systemctl reset-failed

# ⚠️ 下面这一步会删数据，先备份
sudo tar -C /opt -czf /root/lightmem-data-$(date +%F).tar.gz lightmem/data
sudo rm -rf /opt/lightmem
sudo userdel lightmem
# 需要的话再 drop 库：sudo -u postgres psql -c "DROP DATABASE lims;"
```

> 也可以直接 `sh integrations/quickstart/install.sh --uninstall-systemd` 卸双 unit（它只删 unit、不碰 `/opt/lightmem`）。

---

## 15. 与 FreeBSD 版逐项对照

| 维度 | FreeBSD | Linux | 本文位置 |
|---|---|---|---|
| 档位文件 | `lims-solo/family/...` | `lims-linux-solo/family` | `profiles/` |
| 进程隔离 | `security.jail: true` + iocage + vnet | `security.jail: false` + systemd 沙箱 | §9.3、清单 §3 |
| 数据底座 | ZFS dataset（recordsize/compression） | ext4 / xfs（`noatime`） | §6 |
| 静态加密 | `zfs_encryption: true` + `keylocation` | LUKS（块层）/ fscrypt（目录层） | §6、清单 §4 |
| 强制访问控制 | `mac_*` 内核模块 | SELinux / AppArmor | 清单 §5 |
| 文件 ACL | `setfacl`（UFS/ZFS ACL 语义） | POSIX ACL（`setfacl`/`getfacl`） | 清单 §6 |
| 防火墙 | `pf` + `/etc/pf.conf` | ufw / firewalld / nftables | §10 |
| 服务管理 | rc.d（`service lightmem start`） | systemd（`systemctl`） | §9 |
| 端口 | **18443 / 18444** | **7443 / 7444** | §1、§10 |
| 漏洞审计 | `pkg audit -F` | `apt list --upgradable` / `dnf updateinfo`；unattended-upgrades / dnf-automatic | 清单 §8 |
| 系统审计 | `accounting_enable` + `lastcomm` | auditd + journald | 清单 §8 |
| 最小权限 | `sysctl security.bsd.*` | `sysctl kernel.*`（dmesg_restrict / kptr_restrict / yama） | 清单 §9 |
| 安装路径 | `/usr/local/lightmem` | `/opt/lightmem`（`deploy/platform.sh` 的 `lm_prefix` 已固化该约定） | §4 |

---

## 16. 未覆盖与已知限制

1. **本文的 apt/dnf/systemctl 步骤未在 Linux 真机实测**（见 §0.2）。profile 加载、内核启动、`/healthz`、写入与召回是实测的；装包与 systemd 启停只在文档层核对过官方命令。
2. **无容器化方案**。`docker run` 与 systemd 沙箱二选一，本文只写 systemd（与仓库既有 `deploy/*.service` 保持一致）。
3. **无高可用**。两条 Linux 档位都是 `scaling.mode: standalone`；要主备/集群请按 `lightmem.md` §5.5 与 `lims-smb/enterprise` 另立方案。
4. **arm64 未实测**。交叉编译命令与 `CGO_ENABLED=0` 的静态性在架构上成立，但没在 arm64 机器上跑过。
5. **Redis / RabbitMQ 未接入 Linux 档位**。`lims-linux-family` 明确 `cache: none` / `mq: none`（能力矩阵里 Family 的 Redis 标注为「可选」）。需要时按 §2.6 显式改并自行补装与加固。
6. **`platform` 字段未加入 schema**。理由见 §8.1；若后续要机器可读的平台标识，应先升 `lims.profile.schema.json` 与 `kernel/profile.go`（属内核 profile 解析代码，超出本卡红线）。

---

## 附录 A：一页纸速查

```sh
# 目录           /opt/lightmem/{lightmem-kernel,lightmem-mcp,data,logs,profiles,lightmem.env}
# 端口           7443 REST / 7444 MCP（systemd 口径；勿与 18443/18444 混用）
# 探针           /healthz /livez /readyz（免鉴权）
# 服务           systemctl {status,restart,stop} lightmem{,-mcp}
# 日志           journalctl -u lightmem -f
# 排障三板斧     journalctl -u lightmem -n 50  |  ss -lntp | grep 744  |  curl -s 127.0.0.1:7443/readyz
# PG 体检        pg_isready -h 127.0.0.1 && psql -d lims -tAc "select extversion from pg_extension where extname='vector'"
```

## 附录 B：与架构文档的映射

| 本文 | lightmem.md |
|---|---|
| §0–§1 拓扑与档位 | §2.6 可缩放模式 |
| §3 构建 | §5.4 存储后端可插拔（纯 Go 驱动，跨平台） |
| §4 目录与用户 | §5.7 最小权限 |
| §5 PG + pgvector | §4.2 一致性骨架、§5.4 |
| §6 文件系统与加密 | §5.6（ZFS 部分的 Linux 替代）、§5.7 磁盘加密 |
| §9 systemd 沙箱 | §5.7 进程隔离 / 最小权限 |
| §10 防火墙 | §5.7 网络隔离 |
| §11 TLS 反代 | §5.7 TLS 终结 |
| 全部 | §7 安全、隐私与数据主权（业务层约束，OS 层无法替代） |

## 附录 C：本次交付的实测记录（2026-11-20）

> 证据等级：【实测】。宿主 FreeBSD 15.1 + go1.25.14；**二进制用 `GOOS=linux GOARCH=amd64` 交叉编译为静态 ELF，在 Ubuntu 24.04 userland（`/compat/ubuntu24` chroot）中运行**。被测对象是本卡交付的两份 profile 原文，未做任何路径改写。

**C.1 两份 profile 通过 YAML 解析（仓库根目录）**

```console
$ python3 -c "import yaml,glob; [yaml.safe_load(open(f,encoding='utf-8')) for f in glob.glob('profiles/*.yaml')]"
$ echo $?
0
```

**C.2 `jail: false` 计数（验收命令原文）**

```console
$ grep -c 'jail: false' profiles/lims-linux-solo.profile.yaml profiles/lims-linux-family.profile.yaml
profiles/lims-linux-solo.profile.yaml:1
profiles/lims-linux-family.profile.yaml:1
```

**C.3 Solo profile 在 Ubuntu 24.04 userland 启动**

```console
$ /tmp/lightmem-kernel -profile /opt/lightmem/profiles/lims-linux-solo.profile.yaml -listen 127.0.0.1:17445
2026/11/20 14:14:40 [boot] profile=solo backend=sqlite tier=solo
2026/11/20 14:14:41 [boot] listening on 127.0.0.1:17445

$ curl -s -w '\n%{http_code}\n' http://127.0.0.1:17445/healthz
{"backend":"sqlite","embedder":"hash","last_consolidate":"","profile":"solo","started_at":"2026-11-20T14:14:40Z","status":"ok"}
200

$ ls -l /opt/lightmem/data/
lims.sqlite  lims.sqlite-shm  lims.sqlite-wal        # profile 里的 path 原样生效
```

**C.4 Family profile 在 Ubuntu 24.04 userland 启动（真 PG 18.4 + pgvector 0.8.2）**

```console
$ LIMS_PG_PASSWORD=*** /tmp/lightmem-kernel -profile /opt/lightmem/profiles/lims-linux-family.profile.yaml -listen 127.0.0.1:17446
2026/11/20 14:14:53 [boot] profile=family backend=pgvector tier=family
2026/11/20 14:14:53 [pg-superuser-check] policy=fail: current_user 非 superuser — 通过
2026/11/20 14:14:53 [boot] listening on 127.0.0.1:17446

$ curl -s -w '\n%{http_code}\n' http://127.0.0.1:17446/healthz
{"backend":"pgvector","embedder":"hash","last_consolidate":"","profile":"family","started_at":"2026-11-20T14:14:53Z","status":"ok"}
200

$ curl -s -o /dev/null -w 'create=%{http_code}\n' -X POST http://127.0.0.1:17446/memories -d '{...}'
create=201
```

**C.5 角色 bootstrap（`deploy/init-roles.sql`）实测输出**

```console
$ sudo -u postgres psql -d lims -v app_password=*** -f deploy/init-roles.sql
...
   rolname      | can_login | is_superuser
----------------+-----------+--------------
 lightmem_app   | t         | f
```

**C.6 未实测项**：apt / dnf 装包与 `systemctl` 启停（本环境无 Linux 真机，见 §0.2）。
