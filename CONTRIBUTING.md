# 贡献指南（lightmem）

> 面向真人贡献者：clone 之后怎么改、怎么验、怎么提。
> 给 AI / Agent 看的入口是 [`AGENTS.md`](AGENTS.md) 与 [`GETTING_STARTED.md`](GETTING_STARTED.md)，
> 本文件不重复它们的内容。

## 构建

⚠️ **本仓库有「两个 module」：根 module `lightmem` 与 `kernel/`（`github.com/lightmem/kernel`）。**

顶层 `go.mod` 里 `require github.com/lightmem/kernel v0.0.0` 只是占位版本号，
靠 `replace github.com/lightmem/kernel => ./kernel` 指向本地（monorepo 标准做法）。
因此根目录可以直接 `go build ./...` / `go vet ./...` / `go test ./...`，
`eval/` 那批 import kernel 的代码会真正被编译和测试。

完整门禁是 `sh scripts/gate.sh`，它会自动发现仓库里**所有** `go.mod` 并逐个跑
vet/build/test（根 module 与 `kernel/` 各一遍），与 CI 口径一致。内核单独的
构建/测试仍照旧在 `kernel/` 下执行：

CI 也按这个口径配置：`.github/workflows/ci.yml` 的
`defaults.run.working-directory: kernel`（文件头 1–13 行写明了原因，
与 `release.yml` 一致）。

正确的：

```bash
cd kernel && go build ./... && go test ./...
```

需要产出二进制时（README「从源码构建」）：

```bash
cd kernel
go build -o lightmem-kernel ./cmd/lightmem-kernel
go build -o lightmem-frontend ./cmd/lightmem-frontend
go build -o lightmem-mcp ./cmd/lightmem-mcp
```

Go 版本要求 1.25+（README「环境要求」与 CI 的 `GO_VERSION: '1.25'`）。
注意 `go install ...@latest` 暂不可用：kernel 模块尚未独立发布到可解析的模块路径。

> 按仓库红线 2（`roadmap.md`）：本机仅编辑 + SSH/传输，编译 / 测试 / 运行在 `<门禁机>`
> 上执行，完整门禁是 `sh scripts/gate.sh`。Windows 侧只做文档编辑与传输。

## 跑测试

```bash
cd kernel && go test ./...          # 内核单测
sh scripts/smoke.sh                  # 一键冒烟，比单测轻
```

- `scripts/smoke.sh` 头部写清了用法与依赖（`go` / `curl` / `sh`，不需要网络）：
  验 `/healthz`、MCP 端口监听、SQLite 落盘且重启后仍能召回一条测试记忆，
  全程在临时目录跑，不碰真实数据。`KEEP=1` 可留临时目录排查。
- `sh scripts/gate.sh` 是统一门禁（版本 / gofmt / vet / build / test / race /
  覆盖 / 红线白名单 / PG 真机），判据见 README「R16 门禁矩阵」。
  未配置 PG 时 [11] 必须**显式 SKIP**，绝不静默当 PASS。
- **墙内/离线：先配 `GOPROXY` 再跑门禁**。连不上 `proxy.golang.org` 时，go 步骤会
  报 `Get "https://proxy.golang.org/...": i/o timeout` 与 `FAIL ... [setup failed]`，
  看着和真回归一模一样。重试方式：

  ```bash
  GOPROXY=https://goproxy.cn,direct sh scripts/gate.sh
  ```

  看到 `[setup failed]` **先怀疑网络/代理**，别先怀疑这批改动。
  `gate.sh` 自己不改默认 `GOPROXY`（避免 CI 行为漂移），只在命中下载失败特征时
  在汇总前多打一条提示。

> 如实说明：上面两条命令的依据是 `.github/workflows/ci.yml`、README「运行测试」
> 与脚本自身注释，**并未在贡献者本机实跑验证**——本机没有 Go 工具链，
> 且红线 2 要求实跑放在门禁机。请在 `<门禁机>` 上自行跑一遍再据此提 PR。

## 提 PR

- **分支**：走 hive 任务卡时用 `task/T-xxxx`；纯特性可用 `feature/my-feature`
  （README「贡献」段）。
- **commit message**：照 `git log --oneline -20` 的实际风格写 `type(scope): 中文描述`，
  例如 `docs(README): ...`、`feat(auth): ...`、`hive(T-0264): ... (manual submit)`。
  README 里的 `Add my feature` 只是早期英文占位模板，别照抄。
- **不要自动 push 到三个远端**：`scripts/push_all_remotes.sh` 头部红线⑧明写
  「本脚本不自动执行任何 git push，所有 push 命令仅打印，由业主确认后手动执行」；
  README 也写「按红线⑧脚本不自动 push，执行前需业主示意」。推送前需业主示意。
- **三远端现状**：`origin` 指向 gitea 主仓，`gitcode` / `github` 为镜像。
  两侧当前**有分叉**，且启动期安全护栏只存在于 gitea 侧——详见
  [`docs/远端分叉核验_20261106.md`](docs/远端分叉核验_20261106.md)（T-0264，只读核验）。
  请勿自行 rebase / merge / force 合流，收口由另立的卡处理。
- PR 描述里写清：改了哪些文件、在 `<门禁机>` 跑过哪条命令、门禁结果、
  是否触碰「只增不改」白名单。

## 红线（提交前逐条自查）

- **只增不改 Schema 语义**：R14/R15/R16 已建立「只增不改白名单」与统一门禁
  `scripts/gate.sh`（`roadmap.md` 红线 1、README「开发规范」）。
- **不写 token / 密钥 / 凭据**：`.gitignore` 里有一整段凭据类规则——
  `agent-tokens.json`、`tokens.csv`、`clients/*-token.json`、`*-token.txt`、
  `.lightmem-secrets/`、`_lightmem_run/`、`data/`。这是血泪教训：提交前
  `git status` 确认没有这些文件，也不要在 issue / PR / 文档里贴凭据或真实内网地址
  （文档里统一写 `<门禁机>`）。
- **能力边界要如实写**：README「⚠️ 能力边界（R25-D 如实化）」就是范例——
  Solo 档明确写了**没有** jail / MAC / IPsec / KMS、没有多租户 / 跨 Agent 共享。
  新增能力不要顺手写成「已具备」；边界不清标「未验证」，不要标「已支持」。
- 不调 LLM 生成评测集（模板 + 固定 seed）；权重以 `eval/weights/` 校准文件为准，
  不内联写死为「调优好」。
- 运行平台优先级 FreeBSD > Linux > Windows（README「开发规范」）。

## 相关文档

| 用途 | 文件 |
|---|---|
| AI / Agent 入口 | `AGENTS.md`、`GETTING_STARTED.md` |
| 档位能力如实标注 | `docs/档位能力矩阵.md` |
| 接口 | `docs/api.md`、`docs/api.openapi.yaml` |
| 门禁与红线 | `scripts/gate.sh`、`roadmap.md`、`.github/workflows/ci.yml` |
| 安全披露 | `SECURITY.md` |
