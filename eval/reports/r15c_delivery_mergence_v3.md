# R15-C 路交付报告：把死指标激活（合并度）

> 交付日期：2026-10-15 ｜ 工作树：`/tmp/r15c/lightmem`（由 `/tmp/r14/lightmem` 整树复制）
> 验证机：192.168.0.88（go1.25.14 freebsd/amd64）｜ 本机 Windows 只编辑

## 一、目标与结果

任务书 C 路：`mean_edges_per_memory` 与 `mean_memories_per_entity` 在 holdout_v2 上恒为 0（全 L1、无 entity/task），是死指标。本轮**不动 v2、不动北极星**，新增独立语料 `holdout_v3.jsonl` 把两个指标激活，并给 runner 只增两个输出字段。

**结果：两个指标从恒 0 变为可测、可对照。**

| 指标 | v2（反向断言，必须仍为 0） | v3（激活后） |
|---|---|---|
| `total_memories` | 800 | 840 |
| `l2_memories` | 0 | **144** |
| `memories_with_entity` | 0 | **840** |
| `entity_groups` | 0 | **24** |
| `edges_alive` | 0 | **1458** |
| `mean_edges_per_memory` | 0.0000 | **10.1250** |
| `mean_memories_per_entity` | 0.0000 | **35.0000** |
| `entity_coverage`（新） | 0 | 1.0000 |
| `edges_per_relation`（新） | `{}` | `entity-alias:360 / task-flow:360 / temporal:312 / causal:426` |

## 二、交付物清单（文件域按任务书 §八）

| 文件 | 动作 | 说明 |
|---|---|---|
| `eval/holdout/holdout_v3.jsonl` | 新增 | 840 行，UTF-8 LF；v2 未动 |
| `eval/polaris/check_holdout_v3.go` | 新增 | `//go:build ignore` + `package main` + 仅标准库；断言写死常量 |
| `eval/polaris/polaris_mergence.go` | **受控修改（纯新增）** | 只加 `edges_per_relation` / `entity_coverage` 两字段及统计；见第四节 |
| `lightmem.md` §3.2.3 | 新增小节 | 插在 §3.2.2 之后、§3.3 之前（节序 3.2.1→3.2.2→3.2.3） |
| `eval/reports/r15c_mergence_v3_*.json` / `r15c_mergence_v2_*.json` | 新增报告 | 双基线 |

未触碰：`holdout_v2.jsonl`、`kernel/displacement_edge.go`（白名单）、12 个白名单文件、`eval/vectors/`。

## 三、语料设计（对照 `kernel/displacement_edge.go` 实测规则）

设计依据是 `BuildEdgesHeuristic` 的真实建边条件，不是拍脑袋：

- **只对 L2 建边**：故 840 行中 144 条 `layer=L2`（≥120 红线），其余 696 条 L1。
- **按 tenant+user 分组后组内两两配对**：全部 `tenant=t-r15c / user=u-r15c`，144 条 L2 同组配对。
- **entity-alias（0.8）**：24 个实体各 6 条 L2、同实体归一化字符串一致 → C(6,2)×24=360 条。
- **task-flow（0.4）**：每组独立 `task=r15c-task-XX` → 同 360 条。
- **temporal（0.3）**：组内 6 条 `recorded_at` 连续 6 天（≤7 天窗）且同 task → 360 候选；出边 16 条上限软删最低权重后存活 312。
- **causal（0.5）**：kernel 规则是"后条 fact 出现指代词（它/该/这个/it/its/this/that）即连边"。**踩过两个坑**：
  1. 初稿非指代词 fact 里写"与**该**实体"——"该"本身就在指代词表里，导致全部 144 条 L2 误触发 causal；
  2. 实体 slug `orbit/zenith/krait` 含子串 "it"，小写后被 `factMentionsPronoun` 误判。
  最终：24 个 slug 全部不含 it/its/this/that/它/该/这/那；仅最后一组（e23）后 3 条 fact 含"这个"，causal 存活 426 条。
- **三轴非空率**：840/840 = 100%（红线 ≥90%）；`recorded_at` 非零 840/840 = 100%。
- **四桶比例对齐 v2**：single-hop 156 / multi-hop 275 / temporal 173 / open-domain 236（v2 的 149/262/165/224 等比放大到 840）。

## 四、`polaris_mergence.go` 受控修改审计（供 D 路 [7b] 存档）

改动仅两处，**纯新增、既有字段名与计算口径逐位不变**：

1. `emergenceReport` 结构体尾部新增两个 JSON 字段：
   - `edges_per_relation map[string]int`（经导出方法 `store.OutEdges(id)` 遍历全部记忆作起点统计存活边，每条边恰好计一次；不 import 新符号、不改 kernel）；
   - `entity_coverage float64` = `memories_with_entity / total_memories`。
2. main() 在既有统计块之后新增上述两个变量的计算并填入 report。

**v2 反向断言**（证明口径未被改坏）：v2 报告 `l2_memories=0 / edges_alive=0 / mean_edges_per_memory=0 / entity_groups=0 / mean_memories_per_entity=0`，新字段为 `edges_per_relation={}`、`entity_coverage=0`。

## 五、自验命令与结果（均在 0.88 `/tmp/r15c/lightmem` 执行）

```bash
# 1. 语料自检（8 条断言全 PASS）
go run eval/polaris/check_holdout_v3.go -holdout eval/holdout/holdout_v3.jsonl
#   rows=840 l2=144 entity_groups=24 min_per_group=35
#   axis rates: entity=1.0 scene=1.0 task=1.0 recorded_nonzero=1.0
#   ALL PASS

# 2. v3 激活
go run eval/polaris/polaris_mergence.go -holdout eval/holdout/holdout_v3.jsonl \
  -report r15c_mergence_v3 -reports-dir eval/reports -now-fixed 2026-10-15
#   mean_edges_per_memory=10.1250 mean_memories_per_entity=35.0000 edges_alive=1458

# 3. v2 反向断言
go run eval/polaris/polaris_mergence.go -holdout eval/holdout/holdout_v2.jsonl \
  -report r15c_mergence_v2 -reports-dir eval/reports -now-fixed 2026-10-15
#   mean_edges_per_memory=0.0000 edges_alive=0（与 R13-B 基线逐位一致）
```

gofmt：两个改动/新增 .go 文件 `gofmt -l` 为空（已 `gofmt -w` 规范化为 LF）。
`go build ./...` 通过。两个 runner 均 `//go:build ignore`，不进入 `go test ./...`。

## 六、对门禁 [9] 的四条断言口径

| gate[9] 断言 | v3 实测 | 判定 |
|---|---|---|
| v3 `l2_memories > 0` | 144 | PASS |
| v3 `mean_edges_per_memory > 0` | 10.125 | PASS |
| v3 `entity_groups ≥ 20` | 24 | PASS |
| v2 仍为 0（反向） | l2=0 / edges=0 / groups=0 | PASS |

## 七、未做 / 留 R16

- v3 **不替代** v2 作为北极星基准；何时升级为正式评测集需单独口径评审（任务书 §十一.5，涉及红线 9 放宽）。
- 实体/任务的规则化自动抽取器未实现——v3 的 entity/task 是**语料侧人工标注**（生成器写死），与 A 路检测器同构的规则抽取留 R16。
- 生成器 `r15c_gen_v3.py` 在仓库外（`G:\dswork\duan-light-merge\` 旁），不入库；语料即真源，可复跑。
