# 光明记忆（LIMS）规模档位 Profile 配置

**文档版本：** v1.0
**对应架构文档：** `lightmem/lightmem.md` §2.6「可缩放模式」（v2.0）
**对应配套文档：**
- `lightmem/checklists/freebsd-security-checklist.md`（安全加固清单）
- `lightmem/manuals/p1-freebsd-zfs-setup.md`（P1 环境搭建手册）
**用途：** 部署时直接复制对应档位的 YAML 为 `lims.profile.yaml`，改少量字段即可启动光明记忆。

---

## 0. 档位一览

| 档位 | 文件 | 目标用户 | 用户量级 | 记忆量级 | 部署形态 | 存储后端 |
|---|---|---|---|---|---|---|
| Solo | `lims-solo.profile.yaml` | 个人开发者 | 1 人 | 万级 | 单机自包含 | SQLite + sqlite-vec |
| Family | `lims-family.profile.yaml` | 家庭实验室 | 1–5 人 | 十万级 | 单机 FreeBSD + jail | PG + pgvector |
| SMB | `lims-smb.profile.yaml` | 中小团队 | 10–500 人 | 百万级 | 单/双节点 | PG + pgvector + Qdrant |
| Enterprise | `lims-enterprise.profile.yaml` | 多租户企业 | >500 人 | 千万级 | 集合体 Cluster | PG 分片 + Qdrant + TiDB |

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

---

## 3. 与配套文档联动

| 本目录 | 配套文档 |
|---|---|
| 每份 YAML 的 `security` 字段 | `checklists/freebsd-security-checklist.md`：`true` 的项需按 checklist 验证 |
| `storage.zfs.datasets` | `manuals/p1-freebsd-zfs-setup.md` 第 3 节：ZFS dataset 规划 |
| `network.pf_rules` | `manuals/p1-freebsd-zfs-setup.md` 第 6 节：pf 防火墙 |
| `storage_backend` | `lightmem.md` §5.4：存储后端可插拔 |
| `scaling` | `lightmem.md` §5.5：单节点/集合体形态 |
| 全部 | `lightmem.md` §2.6：可缩放模式 |

---

## 4. 版本与兼容

- 本目录 `v1.0` 对应主文档 `v2.0`。
- 字段变更时，先升主文档版本，再同步本目录。
- 每份 YAML 顶部标注对应主文档版本，便于追溯。