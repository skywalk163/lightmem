# R8-B 验收收口报告（P7-D 公开基准正式对标收口 + Q5 gofmt）

- **任务书**：`lightmem/.dumate/inbox/_R8_多语言国际化与公开基准对标任务书.md` §2（B 路）
- **代码提交**：`a9112e6`（feat(R8-B): 公开基准复现 + 加法式适配器指标 + P7-D 报告）——**未 push**
- **权威基线**：`3841420`（R7 收口态，已 push gitea main）
- **收口日期**：2026-10-14
- **运行环境**：192.168.0.88（FreeBSD 15.1-RELEASE-p1，go1.25.14），干净工作树 `/tmp/r8b/lightmem`（`3841420` + 向量/权重/时间样本）

---

## 一、验收判定表（任务书 §B.5）

| 验收项 | 目标 | 实测 | 判定 |
|---|---|---|---|
| 正式 P7-D 对标报告 | `p7d_benchmarks_final_20261014.md` 产出 | 已产出（238 行，含逐桶明细/历史轨迹/合成对照/形态说明） | **PASS** |
| 权威基线 | 记载 MRR **0.9917**（非过期 0.546） | **0.9917**（780 条） | **PASS** |
| 复跑可复现 | `3841420` 上重跑公开 MRR ≥ 0.99 | **0.9917**（逐位一致） | **PASS** |
| 适配口径 | fact acc（适配口径）**≥ 90%**（原 0% 为形态假阴性） | **94.76%**（651/687） | **PASS** |
| 北极星口径不变 | `matchFactV4` / `JudgeM1` 无改动 | 本轮**零改动** | **PASS** |
| Q5 gofmt 收口 | `gofmt -l kernel/lifecycle.go kernel/metrics.go` 无输出；零语义改动 | 已由 `873c41e` 关闭，复验无输出、`gofmt -w` 幂等空操作 | **PASS** |
| gofmt / go vet / go test（全仓） | 全绿 | 全绿（见 §六） | **PASS** |
| **北极星 M1 不回归** | M1 = 1.0000 | **1.0000**（hit@1 0.9763 / mean_rr 0.9113，ref 46/60） | **PASS** |
| **公开 MRR 不回归** | ≥ 0.9917 | **0.9917** | **PASS** |

**判定：B 路核心指标（正式对标报告用 0.9917 收口 + 适配口径 94.76% ≥ 90% + 北极星口径零改动）全部达标，判绿。** 无降级登记。

---

## 二、复跑可复现（任务书 §B.3.4）

### 2.1 复跑命令（0.88，R5 最终态配置）

```sh
cd /tmp/r8b/lightmem/eval
go run ./run_kpi.go -dataset ./benchmarks/dataset/all.jsonl \
  -temporal-samples dataset/temporal.jsonl -weights weights/rank_weights_en_r5.json \
  -embedder vector -now-fixed 2026-10-13 -report r8b_bench_final
```

> 注：任务书 §B.4 写作 `-weights rank_weights_en_r5.json`，实测需 `weights/` 前缀（权重位于 `eval/weights/`，相对 `eval/` 运行目录），已修正。

### 2.2 可复现结果

| 指标 | R5 最终态（`r5_final_public_20261013-104900.json`） | 本次复跑（`r8b_bench_final_20261013-231113.json`） | 一致性 |
|---|---|---|---|
| 评测集总数 | 780 | 780 | ✅ |
| 总体 MRR | **0.9917** | **0.9917** | ✅ 逐位一致 |
| memory_id top-1 | 1.000 | 1.000 | ✅ |
| fact 准确率（v4） | 0.9607（660/687） | 0.9607（660/687） | ✅ |
| 分桶 MRR | single 0.9943 / multi 0.9800 / open 0.9788 / temporal 0.9983 | 同左 | ✅ |
| 污染率 / FP | 0% / 0 | 0% / 0 | ✅ |

**结论**：在权威基线 `3841420` 上以 R5 最终态配置复跑，公开基准 MRR = **0.9917**（780 条），与 R5 最终态**逐位一致**，可复现性达标（≥ 0.99）。三条独立跑（final / native / adapter）MRR 均为 0.9917。

---

## 三、适配口径设计与三级对照（任务书 §B.3.2 / §B.5）

### 3.1 根因：fact 形态假阴性

公开基准 `memory.fact` 为 `question => answer`（LME/BEAM）或证据 turn 原文（LoCoMo），而 `expected_fact` 是纯答案片段；用「精确相等」比对必然趋近 **0.0%**。这是**数据形态假阴性**，与检索能力无关（真实答案早在 top-1 召回，MRR 0.9917）。

### 3.2 新增适配口径（`eval/benchmarks/match_fact_benchmarks.go`，⚠️ 非北极星）

- 独立包 `benchmarks`，**不引用、不修改**北极星判定；
- 判定规则：归一 / 边界归一后 `expected ⊆ got`，或 ⊆ `got` 的 `=>` 答案段即命中（**纯确定性包含，不含时间推理/模糊**）；
- `NormalizeBenchFact` / `AnswerSegment` 拆分，配 `match_fact_benchmarks_test.go`（3 个用例含确定性断言）。

### 3.3 三级口径对照（同一 780 条、同一 687 条非模板样本）

| 口径 | 归属 | 判定规则 | fact 准确率 |
|---|---|---|---|
| 精确相等 | （历史，已废弃） | `normalize(got) == normalize(expected)` | ≈0.0%（形态假阴性） |
| **适配口径** | ⚠️ **非北极星**（R8-B 新增） | 归一后 `expected ⊆ got`（含 `=>` 答案段） | **0.9476**（651/687） |
| **北极星 v4** | 北极星（`matchFactV4`，**零改动**） | 适配 + 数值/日期归一 + 时间推理 + 短词元包含 | **0.9607**（660/687） |

**实测关系**：适配口径命中集合 **⊆** v4 命中集合（651 条全部被 v4 覆盖）；v4 另有 **9 条**命中为适配口径未覆盖（7 条 temporal 走时间推理、2 条 open-domain 走模糊/短词元）。即适配口径是 v4 的**严格确定性下界**，满足§B.2「新增适配口径显式标注『非北极星』、与北极星口径并存不混用」的约束。

---

## 四、只增不改的实测证据（任务书 §B.2 / 红线 1、7）

### 4.1 改动清单（`git show a9112e6 --stat`：11 files, 24703 insertions, **0 deletions**）

| 文件 | 性质 | 语义影响 |
|---|---|---|
| `eval/benchmarks/match_fact_benchmarks.go` | **新增**（独立包） | 无（非北极星） |
| `eval/benchmarks/match_fact_benchmarks_test.go` | **新增** | 无 |
| `eval/run_kpi.go` | **只增 +45 行**：`-fact-adapter` flag（默认 `false`）+ `factAdapterEnabled`/`factAdapterHits` + 报告字段 `fact_accuracy_adapter`/`fact_adapter_hits`/`fact_adapter_enabled` + 摘要 1 行 | 无（默认关闭） |
| `matchFactV4` / `JudgeM1` | **零改动** | 无 |
| `kernel/lifecycle.go` / `metrics.go` | **零改动** | 无 |

### 4.2 逐位复现证明

- 默认态跑（`r8b_bench_native_20261013-231549.json`，不加 `-fact-adapter`）与基线跑（`r8b_bench_final_*.json`）**除 `generated_at` 与墙钟延迟外逐字段一致**；
- **780 条 `details` 逐条逐字段完全相同**（脚本比对 `native details == baseline details: True`）；
- 开 `-fact-adapter` 的跑（`r8b_bench_adapter_*.json`）与基线差异**仅**为新增的 3 个适配字段（`fact_accuracy_adapter` 由缺省变为 0.9476），MRR / memory_id / v4 fact 全部不变。

**结论**：默认态逐位复现权威基线；适配口径为**纯增量**，北极星判定链路零改动。

---

## 五、Q5 gofmt 收口（任务书 §B.3.3）

任务书 §2 B 假设 `kernel/lifecycle.go` / `kernel/metrics.go` 存在 R138-B 遗留 gofmt 违规。**经核实，Q5 已在权威基线内关闭**：

- 修复提交 **`873c41e`（style(kernel): gofmt 修复 lifecycle.go / metrics.go 字段对齐）**：`git merge-base --is-ancestor 873c41e 3841420` 为真，且已存在于 `origin/main`；提交说明为「纯格式修复（`gofmt -w` 产物），`git diff -w` 为空，零语义变化」。
- 复验（0.88）：`gofmt -l kernel/lifecycle.go kernel/metrics.go` **无输出**；`gofmt -w` 为**幂等空操作**（再写 `diff` 为空、`git diff --stat` 为空）。

**结论**：Q5 早已关闭，本次**无需新增格式化提交**（再改只会制造空 diff）。以 `gofmt -l` 全绿 + `873c41e` 留痕作为收口证据，判定 **PASS**。

---

## 六、门禁检查（任务书 §3 / §B.4）

在干净工作树 `/tmp/r8b/lightmem`（`3841420` + 本路源码）执行，全部绿：

| 检查项 | 命令 | 结果 |
|---|---|---|
| gofmt（全仓） | `gofmt -l .` | ✅ 无输出 |
| go vet（全仓） | `go vet ./...` | ✅ OK |
| go test（全仓） | `go test ./...` | ✅ `eval/benchmarks` 0.006s ok、`eval/polaris` 0.014s ok |
| go vet（kernel） | `go vet ./...`（kernel） | ✅ OK |
| go test（kernel） | `go test ./...`（kernel） | ✅ `github.com/lightmem/kernel` 0.034s ok |
| **北极星 M1 不回归** | `go run eval/polaris/polaris_voting.go … -ref-activate=true -weights eval/weights/rank_weights_r6_ref.json -now-fixed 2026-10-13 -topk 5` | ✅ **M1=1.0000**（hit@1 0.9763 / mean_rr 0.9113，ref 46/60） |

### 6.1 北极星主口径逐位复现（`polaris_voting.go`）

评测命令（同任务书 §A.4 / R7-A §二）：

```sh
go run eval/polaris/polaris_voting.go eval/polaris/compress_light.go \
  eval/polaris/polaris_m1.go eval/polaris/polaris_token.go \
  eval/polaris/polaris_fact.go eval/polaris/polaris_fp.go \
  -fusion rrf -ref-activate=true -weights eval/weights/rank_weights_r6_ref.json \
  -now-fixed 2026-10-13 -topk 5
```

| 指标 | 本次复跑 | R7-A 默认态 / R6 收口 | 一致性 |
|---|---|---|---|
| n | 590 | 590 | ✅ |
| **M1（主口径）** | **1.0000** | 1.0000 | ✅ 逐位一致 |
| hit@1 | 0.9763 | 0.9763 | ✅ |
| mean_rr | 0.9113 | 0.9113 | ✅ |
| ref 桶 | 46/60 | 46/60 | ✅ |
| single/multi/open/temporal 桶 coverage | 1.0000 全桶 | 1.0000 | ✅ |

> **口径澄清**：北极星 M1 主口径 = `polaris_voting.go` 的 **coverage（hits/n=590/590=1.0000）**。任务书 §0 表中「1.0000（hit@1 1.0000 / mean_rr 0.9199）」的 hit@1/mean_rr 对应 R7-A **多路径开启态**（redundant=true）行；本路默认态复跑应与 R7-A §二 **默认态**行（hit@1 0.9763 / mean_rr 0.9113 / ref 46/60）对齐，实测**逐位一致**。

---

## 七、文件清单（任务书 §B.6）

| 文件 | 类型 | 状态 |
|---|---|---|
| `eval/reports/p7d_benchmarks_final_20261014.md` | 报告（正式 P7-D 对标收口 0.9917） | ✅ 已入库（`a9112e6`） |
| `eval/benchmarks/match_fact_benchmarks.go` | 新增（适配判定口径，非北极星） | ✅ 已入库（`a9112e6`） |
| `eval/benchmarks/match_fact_benchmarks_test.go` | 新增（适配口径单测） | ✅ 已入库（`a9112e6`） |
| `eval/run_kpi.go` | 修改（只增 `-fact-adapter` flag + 3 报告字段） | ✅ 已入库（`a9112e6`） |
| `eval/reports/r8b_bench_final_20261013-231113.json` / `_summary.md` | 复跑公开基准明细（默认口径） | ✅ 已入库（`a9112e6`） |
| `eval/reports/r8b_bench_native_20261013-231549.json` / `_summary.md` | 默认态复跑（逐位复现基线） | ✅ 已入库（`a9112e6`） |
| `eval/reports/r8b_bench_adapter_20261013-231534.json` / `_summary.md` | 适配口径对照跑（含 `fact_accuracy_adapter`） | ✅ 已入库（`a9112e6`） |
| `eval/reports/benchmarks_summary_p7d.md` | 标注 **SUPERSEDED**（旧 MRR 0.546 → 0.9917） | ✅ 已入库（`a9112e6`） |
| `eval/reports/r8b_acceptance_20261014.md` | 本验收收口报告 | ✅ 本次入库 |
| `kernel/lifecycle.go` / `metrics.go` | Q5 收口 | ✅ 已在 `873c41e` 关闭 |

**未触碰**：`light-merge/`、kernel Schema 既有字段语义（`Memory`/`RouterContext`/`RecalledCandidate`）、北极星判定 `matchFactV4`/`JudgeM1`、`run_kpi.go` 的 MRR 判定、`kernel/i18n/`（A 路文件域）。本路与 A 路文件域不重叠（任务书 §6），`kernel/lifecycle.go`/`metrics.go` 本路零改动（无冲突风险）。

---

## 八、结论

**R8-B（P7-D 公开基准正式对标收口 + Q5 gofmt）验收通过，收口入库（`a9112e6`，未 push）。**

- 公开基准 MRR **0.9917**（780 条）在权威基线 `3841420` 上**逐位复现**，正式 P7-D 对标报告以 0.9917 收口（取代过期 0.546 旧报告）；
- 新增确定性适配口径（`benchmarks.MatchFactBenchmarks`，非北极星），fact acc **94.76%**（651/687）≥ 90%，解决 `question => answer` 形态假阴性；为 v4 口径的严格下界，**并存不混用**；
- **只增不改**：`run_kpi.go` 默认态逐位复现基线（780 条 `details` 全等），北极星判定 `matchFactV4`/`JudgeM1` 零改动；
- Q5 gofmt 已由 `873c41e` 在基线内关闭，复验无输出且 `gofmt -w` 幂等；
- 门禁 gofmt/vet/test 全绿；**北极星 M1=1.0000 不回归**（hit@1 0.9763 / mean_rr 0.9113，ref 46/60，与 R7-A 默认态逐位一致），**公开 MRR 0.9917 不回归**；
- 未触碰 `light-merge/`、kernel Schema 字段语义；未 push。
