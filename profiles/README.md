# 光明记忆（LIMS）规模档位 Profile 配置

**文档版本：** v1.1（新增 Linux 档位）
**对应架构文档：** `lightmem/lightmem.md` §2.6「可缩放模式」（v2.0）
**对应配套文档：**
- `lightmem/checklists/freebsd-security-checklist.md`（FreeBSD 安全加固清单）
- `lightmem/checklists/linux-security-checklist.md`（Linux 安全加固清单，v1.0 新增）
- `lightmem/manuals/p1-freebsd-zfs-setup.md`（FreeBSD + ZFS 环境搭建手册）
- `lightmem/manuals/linux-setup.md`（Linux 环境搭建手册，v1.0 新增）
**用途：** 部署时直接复制对应档位的 YAML 为 `lims.profile.yaml`，改少量字段即可启动光明记忆。

---

## 0. 档位一览

### 0.1 FreeBSD 档位（原有四档，语义未改动）

| 档位 | 文件 | 目标用户 | 用户量级 | 记忆量级 | 部署形态 | 存储后端 |
|---|---|---|---|---|---|---|
| Solo | `lims-solo.profile.yaml` | 个人开发者 | 1 人 | 万级 | 单机自包含 | SQLite + sqlite-vec |
| Family | `lims-family.profile.yaml` | 家庭实验室 | 1–5 人 | 十万级 | 单机 FreeBSD + jail | PG + pgvector |
| SMB | `lims-smb.profile.yaml` | 中小团队 | 10–500 人 | 百万级 | 单/双节点 | PG + pgvector + Qdrant |
| Enterprise | `lims-enterprise.profile.yaml` | 多租户企业 | >500 人 | 千万级 | 集合体 Cluster | PG 分片 + Qdrant + TiDB |

### 0.2 Linux 档位（v1.1 新增）

| 档位 | 文件 | 目标用户 | 用户量级 | 记忆量级 | 部署形态 | 存储后端 |
|---|---|---|---|---|---|---|
| Linux Solo | `lims-linux-solo.profile.yaml` | 个人开发者 | 1 人 | 万级 | 单机 Linux 自包含 | SQLite（纯 Go 驱动，无 cgo） |
| Linux Family | `lims-linux-family.profile.yaml` | 家庭实验室 / 小团队 | 1–5 人 | 十万级 | 单机 Linux，PG 与内核同机 | PG 18 + pgvector |

适用发行版：**Ubuntu 24.04 LTS / Debian 12 / Rocky Linux 9**，x86_64 与 arm64。

**Linux 档位与 FreeBSD 档位的差异（只有这四条，其余字段逐一对齐）：**

1. **无 jail**：`security.jail: false`，进程隔离改由 systemd 沙箱承担（`deploy/lightmem.service` 的 `NoNewPrivileges` / `ProtectSystem=strict` / `PrivateTmp` / `RestrictAddressFamilies` / `UMask=0027`）。
2. **无 ZFS**：`storage.zfs.enabled: false`，数据落普通 ext4/xfs 路径；静态加密用 LUKS / fscrypt（见 Linux 安全清单第 4 节），故 `security.zfs_encryption: false`。
3. **路径按 FHS 固定**：`/opt/lightmem/data/*`（配合 `deploy/platform.sh` 的 `lm_prefix` 与 unit 的 `ReadWritePaths` 白名单），不再用相对 `./data/*`。
4. **端口口径不同**：Linux/systemd 用 **7443（REST）/ 7444（MCP）**；FreeBSD rc.d 用 **18443 / 18444**。**两者不得混改**。

Linux Family 另外把 `cache` / `mq` 显式取 `none`：能力矩阵里 Family 的 Redis 标注为「可选」，单机 Linux 版按「零额外中间件」交付；需要时按 §2.6 显式改 `redis` / `rabbitmq` 并自行补装与加固。

### 0.3 关于 `platform` 字段（重要）

**Linux 档位没有 `platform: linux` 字段，这是有意为之，不是漏写。**

- `lims.profile.schema.json` 是 draft-07，**顶层 `additionalProperties: false`**，且 `properties` 中没有 `platform`；
- `kernel/profile.go` 的 `yaml.Unmarshal` 也不解析该字段（未知键被静默忽略）。

硬加只有两种结局：严格校验器报 `additionalProperties` 错误，或内核静默忽略 —— 后者更危险，会让运维以为「平台已被识别」。因此平台信息只用**文件名 `lims-linux-*` + 文件头注释块 + `security.jail` 开关**表达。若将来需要机器可读的平台标识，须先升 schema 与 `kernel/profile.go`（属内核 profile 解析代码，改动要走独立任务卡）。

**顺带说明 schema 与 YAML 的既有漂移**（2026-11-20 实测，用 `jsonschema` 4.26 对着 `lims.profile.schema.json` 跑全部六份 profile）：

| 文件 | 严格校验结果 |
|---|---|
| `lims-solo` / `lims-smb` / `lims-enterprise` / `lims-linux-solo` | 1 处：`auth` 不在 schema 里（R21-B 新增段未回填 schema） |
| `lims-family` / `lims-linux-family` | 2 处：同上 + `storage.pg.user` 不在 schema 里 |

也就是说 **schema 目前不是加载闸门**（`LoadProfile` 只用 `yaml.Unmarshal` + 少量语义校验，不看 schema），既有四份档位本来就都过不了严格校验。Linux 两份档位与各自的 FreeBSD 同名档位**漂移点完全一致**，没有引入新的漂移类别；`platform` 若加上去就会成为第三个漂移点，故不加。要收口 schema 漂移，另开任务卡同时改 schema 与 `kernel/profile.go`。


---

## 1. 使用方式

### 1.1 选择档位

- **不确定该选哪档？** 按 §2.6「规模选择的判定问题」逐条评估，任一为"是"则升一档。
- **想保守起步？** 从 `Family` 起步，数据经 §7.4 导出/导入，`vector_status` 保证向量可重算，后续升档不丢数据。

### 1.2 启用

```sh
# 复制对应档位为运行配置
cp lightmem/profiles/lims-family.profile.yaml ./lims.profile.yaml

# 修改必要字段：端口、密钥路径、域名等
# 启动光明记忆（示例：Go Router 二进制）
./lims-router --config ./lims.profile.yaml
```

Linux 上用 systemd 双服务（REST 7443 / MCP 7444），部署路径 `/opt/lightmem`，完整步骤见 `manuals/linux-setup.md` 第 7–9 节：

```sh
# 只示意「装哪两份、装到哪」，详细命令以手册为准
install -m 0640 profiles/lims-linux-family.profile.yaml /opt/lightmem/profiles/
install -m 0644 deploy/lightmem.service deploy/lightmem-mcp.service /etc/systemd/system/
systemctl enable --now lightmem.service lightmem-mcp.service
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:7443/healthz    # 预期 200
```

### 1.3 升档

```sh
# 备份当前配置
cp ./lims.profile.yaml ./lims.profile.yaml.bak

# 复制目标档位
cp lightmem/profiles/lims-smb.profile.yaml ./lims.profile.yaml

# 按 §2.6「升级兼容性保证」执行数据迁移：
#  - 元数据先落库（vector_status=pending）
#  - 向量异步补全
#  - 期间可被全文/元数据检索
```

---

## 2. 字段说明

所有档位共用以下顶层字段，字段名与 §2.6 一致：

| 字段 | 含义 | 可选值 |
|---|---|---|
| `profile` | 档位标识 | solo / family / smb / enterprise |
| `storage_backend` | 存储后端 | sqlite / pgvector / qdrant / pgvector+shard |
| `storage` | 存储详细参数 | 见各档位 YAML |
| `cache` | 缓存组件 | none / redis / redis-cluster |
| `mq` | 消息队列 | none / redis / rabbitmq / kafka |
| `network` | 网络参数（pf 规则、IPsec） | 见各档位 YAML |
| `security` | 安全组件开关 | 见各档位 YAML |
| `features` | 功能开关 | 见各档位 YAML |
| `scaling` | 扩容参数 | 见各档位 YAML |

**安全语义不可裁剪：** `features` 中的 `poison_guard`、`audit`、`delete_rights`（级联删除）在**所有档位**必须为 `true`；只有 `security` 层的 OS 加固项可按档位开关。

**Linux 档位读这两行就够：**

| 字段 | Linux Solo | Linux Family | 含义变化 |
|---|---|---|---|
| `profile` | `solo` | `family` | **不变**：仍是规模档位，`linux` 是平台维度，不新增 enum |
| `security.jail` | `false` | `false` | Linux 无 jail；隔离由 systemd 沙箱承担（不是「关掉安全」） |
| `security.zfs_encryption` | `false` | `false` | Linux 无 ZFS；加密走 LUKS/fscrypt |
| `storage.zfs.enabled` | 无该段 | `false` | Family 显式写出 `false`，便于与 FreeBSD 版直接 diff |
| `network.pf` | `enabled: false` | `enabled: false` | pf 是 FreeBSD 组件；Linux 用 ufw/firewalld（清单第 7 节） |
| `storage_backend` | `sqlite` | `pgvector` | 与 FreeBSD 同名档位一致 |

---

## 3. 与配套文档联动

| 本目录 | 配套文档 |
|---|---|
| 每份 YAML 的 `security` 字段（FreeBSD 档） | `checklists/freebsd-security-checklist.md`：`true` 的项需按 checklist 验证 |
| 每份 YAML 的 `security` 字段（Linux 档） | `checklists/linux-security-checklist.md`：jail→systemd 沙箱、ZFS 加密→LUKS/fscrypt、pf→ufw/firewalld 的逐项对照 |
| `storage.zfs.datasets` | `manuals/p1-freebsd-zfs-setup.md` 第 3 节：ZFS dataset 规划 |
| `network.pf_rules` | `manuals/p1-freebsd-zfs-setup.md` 第 6 节：pf 防火墙 |
| Linux 档位全部字段 | `manuals/linux-setup.md`：Go 1.25 / PG 18+pgvector / lightmem 用户 / systemd 双服务 / 端口 7443-7444 / ufw-firewalld |
| `storage_backend` | `lightmem.md` §5.4：存储后端可插拔 |
| `scaling` | `lightmem.md` §5.5：单节点/集合体形态 |
| 全部 | `lightmem.md` §2.6：可缩放模式 |

---

## 4. 版本与兼容

- 本目录 `v1.1` 对应主文档 `v2.0`；`v1.0` → `v1.1` 只**新增**两份 Linux 档位与本节说明，**未改动**原有四份 YAML 的任何字段与语义。
- 字段变更时，先升主文档版本，再同步本目录。
- 每份 YAML 顶部标注对应主文档版本，便于追溯。
- Linux 档位的验收口径（与 FreeBSD 档位共用同一套）：
  ```sh
  # 全部 profile 可被 YAML 解析
  python3 -c "import yaml,glob; [yaml.safe_load(open(f,encoding='utf-8')) for f in glob.glob('profiles/*.yaml')]"
  # Linux 档位必须是「无 jail」
  grep -c 'jail: false' profiles/lims-linux-solo.profile.yaml profiles/lims-linux-family.profile.yaml
  # 预期：两份各 1 行
  ```
