# lightmem Linux 适配 · 并行任务书 v1

> 版本：v1（2026-11-20）｜ 收口卡：T-0417 ｜ 验收报告：`_LM_Linux适配_验收报告.md`（本仓根）
> 代码仓：`lightmem` ｜ 基线：`56df3c5`（origin/main，A–F + C 线 logrotate 全部合入）
> 铁律：**先证伪再动手**；未实测的不写交付；不动 FreeBSD rc.d 既有文件；端口口径 7443/7444（systemd/quickstart）与 18443/18444（rc.d）不得混改。

---

## 1. 现状实测（为什么要做这一轮）

FreeBSD 0.88 是现网主力，Linux 侧长期只有「模板 + 文档」而无真机闭环。R29 之后 kernel/MCP 双进程、看门狗、日志轮转三件事在 Linux 上**从未被一条命令串起来验过**，典型症状是：

- `install.sh --systemd` 在真机上**装完即失败**（本轮实测：unit 落点 `lightmem.service.service`，systemctl 找不到）；
- 即使 unit 装对，kernel 仍因 `EnvironmentFile` 写了多行 JSON 而**启动即崩**；
- `logrotate` 配置在 Ubuntu 上被「父目录权限不安全」整条跳过，日志仍在涨；
- E 线 CI 门禁在真 Linux(dash) 上对 bash 脚本误报 FAIL。

以上四条都不是"设计错误"，而是**跨线接缝**：单线自测（dry-run / 假 OS）全绿，拼起来才暴露。

---

## 2. 契约（冻结项，各线只复用不改）

| 项 | 口径 | 归属 |
|---|---|---|
| 平台抽象接口 | `deploy/platform.sh` 六函数（`lm_detect_os` / `lm_prefix` / `lm_service_restart` / `lm_service_install` / `lm_probe_port` / `lm_logrotate_install`） | T-0410 |
| 端口 | systemd + quickstart = **7443/7444**；rc.d = **18443/18444** | 全线 |
| systemd unit | `deploy/lightmem.service` + `deploy/lightmem-mcp.service`（双 unit，MCP `After/Wants` kernel） | T-0411 |
| 看门狗 | `deploy/lightmem-watchdog.sh` + `.service`/`.timer`（Linux）；`deploy/cron.d/lightmem-watchdog`（FreeBSD） | T-0412 |
| 日志轮转 | Linux `deploy/logrotate.d/lightmem`；FreeBSD `deploy/newsyslog.d/lightmem.conf` | T-0413 |
| Linux profile | `profiles/` + 部署手册 + 安全清单 | T-0414 |
| 部署冒烟门禁 | `scripts/ci-linux-smoke.sh`（不 sudo、不写 /etc、不编译） | T-0415 |
| 平台口径文案 | `README.md` / `GETTING_STARTED.md` | T-0416 |

红线（全线共同）：不改 kernel 核心语义；不改端口口径；不碰 FreeBSD 0.88 现网；报告里的 PASS 必须有真实命令与退出码。

---

## 3. 波次与卡号回填

| 波次 | 卡号 | 线 | 标题 | 状态 | 交付物 |
|---|---|---|---|---|---|
| 0 | **T-0410** | T0 | 平台抽象 `deploy/platform.sh` + 接口冻结 | done | `deploy/platform.sh`、`deploy/platform_test.sh` |
| 1 | **T-0411** | A | systemd 双服务单元 + `install.sh --systemd` 健全化 | done | `deploy/lightmem.service`、`deploy/lightmem-mcp.service` |
| 1 | **T-0412** | B | 看门狗跨平台化 + systemd timer | done | `deploy/lightmem-watchdog.sh`、`.service`、`.timer` |
| 1 | **T-0413** | C | logrotate 日志轮转（Linux 侧） | done | `deploy/logrotate.d/lightmem`、`README.md` |
| 1 | **T-0414** | D | Linux profile + 部署手册 + 安全清单 | done | `profiles/`、`deploy/README.md` |
| 1 | **T-0415** | E | CI 增加 Linux 部署冒烟门禁 | done | `scripts/ci-linux-smoke.sh` |
| 1 | **T-0416** | F | README / GETTING_STARTED 平台口径修正 | done | `README.md`、`GETTING_STARTED.md` |
| 2 | **T-0417** | G | Linux 真机全链验收收口 | done | `_LM_Linux适配_验收报告.md`、本任务书 |

依赖关系：`T-0417` 依赖 `T-0411..T-0416` 全部合入；C 线（T-0413）于本轮验收开始后才补齐，故 G 卡首轮曾因前置未满足而挂起，补齐后继续。

---

## 4. 全链验收结论（明细见验收报告）

| 环节 | 结论 | 关键证据 |
|---|---|---|
| 非 root quickstart（solo/SQLite） | PASS | `install.sh --profile solo` exit 0；REST/MCP 双 200 |
| 探针 | PASS | `/healthz` `/livez` `/readyz` 均 200；MCP 13 tools |
| save→recall | PASS | `memory_id=001a1f9c91d8fa6432dc356539eb6`，`HIT_MID=YES` |
| systemd 双服务 | PASS（修复后） | `is-enabled/enabled`、`is-active/active`、7443/7444 监听 |
| 看门狗（timer + 真实注入 + 冷却） | PASS | 停服→自动拉起→`readyz=200`；二次执行被冷却挡下 |
| logrotate（`-d` / `-f` / delaycompress） | PASS（修复后） | rc 0；`.1` 生成、活动文件截断 0 行；`.2.gz` 生成 |
| E 线 CI 门禁 | PASS（修复后） | 27 PASS / 0 FAIL / 1 SKIP，exit 0 |
| 记忆持久性 | PASS | 重启后 phase1 记忆仍在 |

---

## 5. 本轮登记/修复的接缝

| # | 问题 | 处置 | 归属 |
|---|---|---|---|
| S1 | `lm_service_install` 对已带 `.service` 的 name 再补后缀 → 装成 `.service.service` | **本卡修**（`deploy/platform.sh` 后缀归一 + 2 条回归用例） | 修：G；根因：A/T0 |
| S2 | `install.sh` 把多行 JSON 写进 `EnvironmentFile` → kernel 启动崩 | 本卡给 `scripts/lm-env-compact.sh` 规避；**跟修归 A** | A |
| S3 | logrotate 在 Ubuntu（`/var/log` root:syslog 775）被整条跳过 | **本卡修**（加 `su root root`） | C→G 补 |
| S4 | systemd 部署下日志进 journald，logrotate 对真实日志空转 | 仅登记，**不改 unit 语义**；建议 A/C 择一 | A/C |
| S5 | E 线门禁用 dash 检查 bash 脚本 → 真 Linux 误报 FAIL | **本卡修**（按 shebang 分派解释器） | E |
| S6 | `/usr/local/libexec/lightmem` 不存在导致 `platform.sh` 装不上、看门狗退"内置兜底" | **本卡修文档**（`deploy/README.md` 补 `mkdir -p`） | B（文档） |

---

## 6. 遗留与后续

- S2 / S4 / S6 的**根修**落在 A 线（`install.sh`、unit 日志去向），本卡只做规避与登记，不越界改 `integrations/`。
- 建议给 `deploy/logrotate.d/*`、`deploy/*.sh` 加 `.gitattributes` `eol=lf`：Windows 侧编辑带 CRLF 会直接让 `logrotate` 解析失败（本轮自测踩过，已本地纠正、未入库）。
- 下一轮候选：**family 档（PostgreSQL + pgvector）在 Linux 的等保级验收**（本轮只覆盖 solo/SQLite 零依赖路径）；以及 mirror/镜像源不可用时的离线安装包形态。
- 运维侧可观测：`journalctl -u lightmem.service`、`/var/log/lightmem-watchdog.log`、`/run/lightmem/watchdog.cooldown`。

---

## 7. 验收口径（收口人自查）

```sh
grep -c 'PASS' _LM_Linux适配_验收报告.md      # 计数（每条 PASS 均对应命令+退出码）
grep -q 'memory_id' _LM_Linux适配_验收报告.md # save→recall 闭环
sh scripts/ci-linux-smoke.sh                  # 部署门禁（Linux 上应 exit 0）
```
