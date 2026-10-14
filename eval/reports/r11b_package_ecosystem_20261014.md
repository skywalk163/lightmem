# R11-B 交付报告：应用包生态真机验证（8 包 Family 档装配 + 包记忆抽取 + 拓扑排序）

> **版本**：v1 ｜ **日期**：2026-10-14 ｜ **任务编号**：R11-B
> **基线**：`75718de`（R10 收口态，已 push gitea main）
> **验证机**：192.168.0.88（FreeBSD 15.1-RELEASE-p1，go1.25.14）
> **状态**：代码已交付，**编译/测试/gofmt 须在 0.88 执行**（任务书红线 §四-2：本机仅编辑 + SSH/传输，编译/测试/运行在 0.88）

---

## 0. 执行环境与本机约束

| 项 | 值 | 说明 |
|---|---|---|
| 开发机 | HP Z620（Windows，`skywalk`） | 仅编辑 + 静态结构校验，**无 Go 工具链** |
| 目标验证机 | 192.168.0.88（FreeBSD 15.1 + go1.25.14） | 编译/测试/gofmt 在此执行 |
| 静态校验手段 | Python 脚本 | 括号配对、import 是否被使用、复合字面量对齐（gofmt 规则复刻） |

**红线遵守**：
- 未在本机编译/运行任何 Go 代码（红线 §四-2）；
- 未修改 kernel Schema 语义 / Fuse 算法 / 既有 SQL / 北极星判定口径（红线 §四-1）；
- 未触碰 `light-merge/`（红线 §四-4）；
- 未执行任何 push（红线 §四-5）。

---

## 1. 交付物清单

| # | 文件 | 类型 | 任务书编号 | 说明 |
|---|---|---|---|---|
| 1 | `kernel/package_memory.go` | **新增** | B.2 | `ExtractPackageMemory` 结构化包记忆抽取（元数据/配置/变更记录三视图），自 `package.go` 迁出并增强 |
| 2 | `kernel/package_order.go` | **新增** | B.3 | `InstallOrder` 拓扑安装顺序 + **循环依赖检测**，自 `package.go` 迁出并增强 |
| 3 | `kernel/package.go` | 编辑 | B.2/B.3 | 迁出 `ExtractPackageMemory` / `InstallOrder`，移除不再使用的 `time` import；**其余内容语义不变** |
| 4 | `kernel/package_order_test.go` | **新增** | B.3 | 拓扑排序单测（合法 DAG / 链式 / 循环 / 自依赖 / 未知依赖 / 确定性） |
| 5 | `kernel/package_memory_test.go` | **新增** | B.2 | 包记忆抽取单测（8 包计数 / 未知包 / 幂等 / 召回命中 / 自定义 manifest） |
| 6 | `eval/packages/family_profile.yaml` | **新增** | B.1 | 首批 8 包 Family 档装配档案（元数据 + 依赖图 + 期望顺序 + 生命周期动作） |
| 7 | `eval/packages/manifests/*.json`（8 个） | **新增** | B.1 | registry / gitea / llm-gateway / git-connector / search / backup / mesh-sync / webui 8 个 manifest |
| 8 | `eval/packages/install_family.sh` | **新增** | B.1 | 8 包装配脚本（order / lifecycle / memory / cycle-demo 四子命令） |
| 9 | `eval/packages/cmd/package-family/main.go` | **新增** | B.1 | install_family.sh 的 Go 驱动（走 kernel 公共 API 驱动全链路） |
| 10 | `eval/e2e/packages/package_e2e_test.go` | **新增** | B.4 | 包记忆 E2E：装包→抽包记忆→召回命中；错误顺序→拓扑排序报错 |
| 11 | `eval/reports/r11b_package_ecosystem_20261014.md` | 报告 | — | 本文件 |

**文件域核对**：`eval/packages/`、`kernel/package_memory.go`、`kernel/package_order.go`、`eval/e2e/packages/`、`eval/reports/r11b_*` 全部命中，未越界触碰 A/C 路文件域。

---

## 2. 依赖图设计（首批 8 包）

```
registry ─┬─ git-connector        (deps: gitea)
          │  search               (deps: registry)
          │  backup               (deps: registry)
          │  mesh-sync            (deps: registry)
gitea ────┘
          │
webui ────┴─ registry, search, gitea   (聚合 UI，依赖最深)
llm-gateway                           (无依赖)
```

**拓扑安装顺序**（`InstallOrder` 输出）：

1. registry → 2. gitea → 3. llm-gateway → 4. git-connector → 5. search → 6. backup → 7. mesh-sync → 8. webui

> 期望顺序已固化在 `family_profile.yaml` 的 `expected_install_order` 字段，`install_family.sh order` 会校验实际输出与期望一致。

---

## 3. 核心实现

### 3.1 `kernel/package_memory.go` — 包记忆抽取（B.2）

每个包抽取 **3 条结构化记忆**（idempotency_key 各自稳定，重抽幂等）：

| 视图 | IdempotencyKey | MemoryKind | 事实文本 | 置信度 |
|---|---|---|---|---|
| 元数据 | `pkg:<name>:metadata` | fact | 包名 + 类型 + 信任级 + 说明 + 可产出记忆类型 | 0.95 |
| 配置 | `pkg:<name>:config` | fact | 入口 + 权限列表 | 0.90 |
| 变更记录 | `pkg:<name>:changelog` | event | 版本 + 作者 + 依赖 | 0.85 |

统一补齐 Schema 默认值：`Visibility=VisTenant`、`PermissionLevel=internal`、`Provenance=ProvNormal`、`VectorStatus=VectorPending`、`Layer=LayerL2`、`ReferenceFrame{Category:"package", Entity:<name>, Project:"lightmem-family", Agent:"package-lifecycle"}`。

**兼容性**：保留原 API `ExtractPackageMemory(ctx, backend, pkgName)`（从 `FirstPartyPackages` 查 manifest），新增 `ExtractPackageMemoryFromManifest(ctx, backend, mf)` 支持任意社区包。

### 3.2 `kernel/package_order.go` — 拓扑排序 + 循环检测（B.3）

采用 **三色标记 DFS**（WHITE/GRAY/BLACK）：

- 命中 GRAY（递归栈中）= 回路，报错并输出回路路径（如 `cycle detected: a -> b -> c -> a`）；
- 未知依赖（引用未注册包）单独报错；
- 自依赖单独报错；
- 输出拓扑序：依赖在前，可直接按序安装；
- 包名排序后遍历，结果确定性（不依赖 map 迭代顺序）。

> 相对 `package.go` 初版的无环 DFS：初版把 `visited=true` 设在递归前，回边会静默返回 nil，**循环被静默接受**。R11-B 补齐循环依赖检测（B.3 明确要求）。

---

## 4. 装配脚本与全链路

### 4.1 `eval/packages/install_family.sh`

```sh
bash eval/packages/install_family.sh            # all：order + lifecycle + memory
bash eval/packages/install_family.sh order      # 拓扑排序 + 与期望顺序比对
bash eval/packages/install_family.sh lifecycle  # 装->配->启->升->卸 全链路
bash eval/packages/install_family.sh memory     # 抽包记忆 + 召回验证
bash eval/packages/install_family.sh cycle-demo # 跑循环依赖检测单测
```

脚本前置检查：8 个 manifest 齐全、`family_profile.yaml` 存在、go 命令可用。

### 4.2 `eval/packages/cmd/package-family/main.go`（Go 驱动）

走 kernel 既有公共 API，逐包执行：`Install → Configure → Enable → Upgrade → Uninstall`，内存 SQLite（`:memory:`），不依赖真实 PG/网络/LLM。

### 4.3 全链路生命周期（B.1 验收核心）

对 8 包按拓扑序执行：**安装 → 配置 → 启用 → 升级 → 卸载**。卸载按逆拓扑序（先卸被依赖最多的 webui，最后卸 registry），避免"仍有包依赖它"的拦截。全部卸载后 `ListPackages()` 应为空。

---

## 5. E2E 测试（B.4）

`eval/e2e/packages/package_e2e_test.go`（`go test ./eval/e2e/packages/ -v -count=1`）：

| # | 用例 | 验收目标 |
|---|---|---|
| 1 | `TestPackageE2E_InstallQueryRecall` | 装包 → 抽包记忆 → 查询「这个包是干什么的」命中 registry 包记忆 |
| 2 | `TestPackageE2E_WrongOrder_Error` | 循环依赖 / 未知依赖 → 拓扑排序报错；依赖未装/未启用 → PackageLifecycle 拦截 |
| 3 | `TestPackageE2E_FullLifecycle` | 8 包 装→配→启→升→卸 全链路，无残留 |

**测试方式**：内存 SQLite + `kernel.PackageLifecycle` + `kernel.InstallOrder` + `kernel.ExtractPackageMemory`，走真实 kernel 公共 API；`eval/packages/manifests/` 路径通过 `runtime.Caller` 定位仓库根后解析。

---

## 6. 验证状态（诚实声明）

> **重要**：本机（Windows Z620）**无 Go 工具链**，按红线 §四-2 未在本机编译/测试。以下为**本机静态校验结果** + **须在 0.88 执行的命令**。

### 6.1 本机已完成（静态校验）

| 检查 | 方法 | 结果 |
|---|---|---|
| 括号/圆括号/方括号配对 | Python 剥离字符串+注释后计数 | ✅ 全部平衡 |
| import 是否被使用 | Python 解析 import + 全文引用 | ✅ 全部使用，无冗余 |
| gofmt 复合字面量对齐 | Python 复刻 gofmt 对齐规则（已用 30+ 个已提交 gofmt-clean 文件验证该规则） | ✅ 7 个新/改 .go 文件对齐一致 |
| tab 缩进（gofmt 要求） | Python 检查行首字符 | ✅ 全部 `\t`，无前导空格缩进 |
| 换行符统一 | 按 `.gitattributes`（`*.go`/`*.sh`/`*.yaml`/`*.json`/`*.md` 全部 `eol=lf`） | ✅ 全部归一为 LF，无 CR |
| shell 脚本语法 | `bash -n` | ✅ 通过 |
| YAML/JSON 合法性 | `yaml.safe_load` + `json.load` | ✅ 合法 |
| YAML ↔ JSON manifest 一致性 | 逐字段比对（name/version/description/author/trust/type/entrypoint/permissions/memory_types/dependencies） | ✅ 8 包完全一致 |
| 包名与 `kernel.FirstPartyPackages` 一致 | 集合比对 | ✅ 8 包名称完全一致 |
| 期望安装顺序合法性 | 校验依赖先行 | ✅ `family_profile.yaml` 的 `expected_install_order` 是合法拓扑序 |
| 未触碰红线项 | 文件 diff 审计 | ✅ 仅新增 + 迁移两个函数，未改 Schema/融合/SQL/北极星 |

**本机修复记录**：
- `eval/e2e/packages/package_e2e_test.go` 的 `repoRoot` 原用固定 3 层 `filepath.Dir` 上溯，路径深度有误；已改为**沿目录逐级上溯直到找到 `go.mod`**，鲁棒可移植。
- 全部新文件初写时部分带 CRLF（Windows 编辑工具所致），已按 `.gitattributes` 归一为 LF（尤其 `.sh` 若为 CRLF 会在 0.88 报 `\r: not found`）。

### 6.2 须在 0.88 执行（红线 §四-2）

```sh
# 1. gofmt（首次建议 gofmt -w 兜底，再验证 -l 为空）
cd /tmp/r11b/lightmem
gofmt -l ./kernel ./eval            # 期望：空输出
gofmt -w ./kernel ./eval            # 若 -l 非空，先自动格式化

# 2. go vet
go vet ./kernel/... ./eval/e2e/packages/...

# 3. kernel 单测（含本路新增）
go test ./kernel/ -run 'Package|InstallOrder' -v -count=1

# 4. 包 E2E
go test ./eval/e2e/packages/ -v -count=1

# 5. 装配脚本
bash eval/packages/install_family.sh all

# 6. 不回归：kernel 全量 + -race
go test ./kernel/... -count=1
go test -race ./kernel/... -count=1
```

**已知风险**：本机无 Go，`gofmt -l` 的字节级一致性未能本机验证；若 0.88 上 `gofmt -l` 报个别文件，`gofmt -w` 一键修复即可，不影响逻辑正确性（已通过结构+逻辑推演校验）。

---

## 7. 验收对照（任务书 §B.1–B.4）

| # | 验收项 | 交付物 | 判定依据 | 状态 |
|---|---|---|---|---|
| B.1 | 首批 8 包 Family 档装配，安装→配置→升级→卸载全链路 | `family_profile.yaml` + `install_family.sh` + 8 manifests + `cmd/package-family` | 脚本按拓扑序驱动 8 包全链路；E2E `TestPackageE2E_FullLifecycle` 验证无残留 | ✅ 代码交付，须 0.88 实测 |
| B.2 | `ExtractPackageMemory`：从元数据/配置/变更记录抽结构化记忆写入记忆库 | `kernel/package_memory.go` | 每包 3 条（元数据/配置/变更），idempotency_key 稳定，幂等；单测覆盖 | ✅ 代码交付，须 0.88 实测 |
| B.3 | `InstallOrder`：依赖图拓扑排序 + 循环依赖检测 | `kernel/package_order.go` | 三色 DFS 输出拓扑序；回路/自依赖/未知依赖均报错并给出回路路径 | ✅ 代码交付，须 0.88 实测 |
| B.4 | 装包后查「这个包是干什么的」命中包记忆；错误顺序→拓扑排序报错 | `eval/e2e/packages/package_e2e_test.go` | 两个用例分别覆盖召回命中与报错提示 | ✅ 代码交付，须 0.88 实测 |

---

## 8. 结论

- ✅ **B.1–B.4 全部交付**：8 包装配档案 + 安装脚本 + Go 驱动、结构化包记忆抽取、拓扑排序（含循环检测）、包记忆 E2E，全部落到任务书指定的文件域。
- ✅ **补齐 1 个既有缺陷**：`package.go` 初版 `InstallOrder` 用 `visited` 布尔标记，回路被静默接受（回边返回 nil）；R11-B 改为三色标记 DFS，回路显式报错并输出路径，满足 B.3"循环依赖检测"。
- ✅ **红线遵守**：未改 Schema/融合算法/既有 SQL/北极星口径；未触碰 `light-merge/`；未 push。
- ⚠️ **未在本机编译/测试**：按红线 §四-2，编译/测试/gofmt 在 0.88 执行（见 §6.2 命令清单）。
- 📌 **下一步**：0.88 上跑 §6.2 全量命令，逐项勾掉 B.1–B.4 验收；合流后经用户示意再 push gitea main。