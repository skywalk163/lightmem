# R15 四路合流报告（A 投毒检测 / B opt-in 接线 / C 合并度激活 / D 工程债收口）

> **合流日期**：2026-10-15 ｜ **基线**：`5b4e03e`（R14 收口）
> **合流树**：`/tmp/r15/lightmem`（由 `/tmp/r14/lightmem` 复制后叠加四路）
> **验证机**：192.168.0.88（FreeBSD 15.1-RELEASE-p1，go1.25.14）
> **结论**：门禁 **PASS 12 / FAIL 0 / SKIP 0**；北极星与 R14 **逐字节一致**（832178 bytes）；四路交付齐，红线零破坏。

---

## 一、合流范围与文件域

四路按任务书 §八 文件域叠加，**零跨路引用**（A 不引 B/C，B 不引 A/C，C 语料与 A 语料不混用，D 只读前三路文件）：

| 路 | 新增/修改文件 |
|---|---|
| **A** 投毒检测可评测化 | `kernel/poison_detect.go`、`kernel/poison_detect_test.go`、`eval/dataset/injection_v2.jsonl`、`eval/dataset/benign_near.jsonl`、`eval/polaris/polaris_injection.go` |
| **B** opt-in 接线 | `kernel/drift_pipeline.go`、`kernel/drift_pipeline_test.go`、`kernel/flush_pipeline.go`、`kernel/flush_pipeline_test.go` |
| **C** 合并度激活 | `eval/holdout/holdout_v3.jsonl`、`eval/polaris/check_holdout_v3.go`、`eval/polaris/polaris_mergence.go`（纯新增两字段） |
| **D** 工程债收口 | `kernel/edge_persist_pg_test.go`、`eval/e2e/pg/setup_edge_persist.sh`、`scripts/gate.sh`、`.gitignore`、`eval/reports/r15d_report_split_convention.md` |
| **共享文档** | `lightmem.md`（四路各增指定小节 + 附录 D + 版本表 v2.4.2）、`eval/polaris/README.md`（§7） |

**合流顺序（按章节号）**：§3.2.3（C）→ §3.5.1.1（B）→ §5.3.1.1（B）→ §7.5.1（A）→ 附录 D（D）→ 附录 B 版本历史 v2.4.2（主 Agent 统一追加）。

---

## 二、门禁矩阵（`/tmp/r15/lightmem`，一次运行的实际输出）

```bash
cd /tmp/r15/lightmem
R15_BASELINE=/tmp/r14/lightmem sh scripts/gate.sh
```

| # | 门禁项 | 结果 | 备注 |
|---|---|---|---|
| [0] | Go 版本 ≥ 1.25 | PASS | go1.25.14 |
| [1] | `gofmt -l` 本轮新增文件 | PASS | 0 文件待格式化 |
| [1b] | `gofmt -l` 全仓 | INFO | **0 文件待格式化**（D3 已收掉 R14 遗留噪音） |
| [2] | `go vet ./...`（逐 module） | PASS | 2/2 通过 |
| [3] | `go build ./...`（逐 module） | PASS | 2/2 通过 |
| [4] | `go test -count=1 ./...`（逐 module） | PASS | 2/2 通过 |
| [5] | `go test -count=1 -race`（kernel） | PASS | 无竞态 |
| [6] | 新增文件测试覆盖 ≥12/文件 | PASS | 7 项（含纯测试条目 `edge_persist_pg`） |
| [7] | 红线抽查（12 文件白名单） | PASS | 基线 `/tmp/r14/lightmem`，逐文件一致 |
| [7b] | 受控修改审计 | PASS + INFO | `writepolicy.go`/`metrics.go` diff **空**；`polaris_mergence.go` 45 行纯新增（已存档） |
| [8] | 投毒检测 | PASS | `poison_detect_test.go` 全 PASS |
| [9] | 合并度激活 | PASS | v3 `l2=144` / `edges=10.125` / `groups=24` |
| [9] | 合并度激活（v2 反向） | PASS | v2 三字段仍为 0 |
| [10] | 接线默认关 | PASS | `drift_pipeline`/`flush_pipeline` 全 PASS |

**汇总：PASS 12 / FAIL 0 / SKIP 0 / module 2。**

---

## 三、北极星复现（主 1）

```bash
cd /tmp/r15/lightmem
go run ./eval/polaris/polaris_holdout.go \
  ./eval/polaris/polaris_fact.go ./eval/polaris/polaris_m1.go \
  ./eval/polaris/polaris_m2.go ./eval/polaris/polaris_token.go \
  ./eval/polaris/compress_light.go ./eval/polaris/polaris_fp.go \
  -holdout eval/holdout/holdout_v2.jsonl \
  -weights eval/weights/rank_weights_en_r5.json -embedder vector -topk 5 \
  -now-fixed 2026-10-14 -report r15_final_holdout -reports-dir eval/reports
```

| 指标 | R15 实测 | R14 基线 | 判定 |
|---|---|---|---|
| M1 零 token 覆盖 | **1.0000** | 1.0000 | ✅ |
| hits / hit@1 / mean_rr / id_hit_rate | 800 / 1.0000 / 1.0000 / 1.0000 | 同 | ✅ |
| 四桶覆盖 | single-hop 149 / multi-hop 262 / temporal 165 / open-domain 224，全 1.0 | 同 | ✅ |
| M2 控制面占比 | 0.2498（control=43126 / data=129537 / total=172663） | 同 | ✅ |
| 数据面压缩 | 35.33%（200295 → 129537，saved=70758） | 同 | ✅ |
| FP 审计 | vetoed=0（v4_hits=800） | 同 | ✅ |
| mode / official_corpus | `holdout-official` / `true` | 同 | ✅ |

**逐位对比（实测）**：`mode`、`m1`、`m2`、`data_plane_compression`、`fp_audit`、`config`、800 条 `details` **全部 identical**；整份 JSON **深度相等，且逐字节相同（832178 bytes）**。

> 逐字节相同是**预期结果**，不是巧合：A 路检测器 opt-in 未进默认写路径、B 路接线默认关、C 路只新增独立语料与输出字段、D 路只动测试与门禁脚本——四路都没有触碰北极星判分输入。

报告：`eval/reports/r15_final_20261015.json`（原始时间戳副本 `r15_final_holdout_20261015-152736.json`）。

---

## 四、四路能力实测汇总

| 路 | 指标 | 结果 |
|---|---|---|
| A | 投毒检测 recall / precision / fpr | **1.0000**（342/342） / **1.0000** / **0.0000**（0/300）；17 条规则全部有命中 |
| B | 接线默认关 | `DefaultDriftPipelineConfig().Mode == "default"`；26 项测试全 PASS；默认态与 R14 逐位一致 |
| C | 合并度指标（v3 激活 / v2 反向） | v3：`l2=144`、`mean_edges_per_memory=10.125`、`entity_groups=24`、`entity_coverage=1.0`；v2：三字段仍为 0 |
| D | PG 真机往返 | **13 PASS / 0 SKIP**（PG 18 @ 127.0.0.1:5432）；无环境变量时 13 项全 SKIP 且 `ok` |

---

## 五、合流过程中发现并修复的真实缺陷

R15 四路**交付态本身带缺陷**——A/B/C 三路的 Go 文件从未过 `gofmt`，D 路的 PG 测试与 setup 脚本**从未编译/执行过**。合流首次真跑全部暴露并修复（详见 `lightmem.md` 附录 D.5 与 `r15d_delivery_20261015.md` §五）：

| 类别 | 数量 | 举例 |
|---|---|---|
| 编译级（模块无法通过 `go vet`/`go test`） | 6 处 | `edge_persist_pg_test.go` 缺 `reflect`/`strings` import、`declared and not used` |
| 脚本级（dash 语法错误） | 1 处 | `setup_edge_persist.sh` 引号未闭合 → `Syntax error: "(" unexpected` |
| 语义级（真机断言失败） | 5 处 | `TIMESTAMPTZ` 微秒精度丢纳秒；并发 Save 撞唯一索引；DDL 断言把注释当语句 |
| 门禁自身缺陷 | 2 处 | [6] 把纯测试条目判为「缺实现」；[9] 扫 stdout 而 JSON 在报告文件 |
| 格式级 | 3 个文件 | `poison_detect*.go`、`polaris_injection.go` 未 `gofmt` |

**这批缺陷若不真跑就交付，等于「门禁报绿但能力从未生效」**——与 R14 抓出 `os.Replace`（kernel 整个模块编译不过）是同一类问题。

---

## 六、红线核验（主 3）

| 红线 | 状态 | 证据 |
|---|---|---|
| 1 北极星不降级 | PASS | 逐字节一致（832178 bytes） |
| 2 只增不改 | PASS | `[7]` 12 文件白名单逐文件 `diff -q` 一致 |
| 3 既有 SQL 不动 | PASS | DDL 只含 `CREATE`；`[7b]` 存档 |
| 4 holdout_v2 / injection.jsonl / vectors 只读 | PASS | 未触碰 |
| 5 不调 LLM，确定性 | PASS | 固定 `-now-fixed`、无随机源 |
| 6 本机只编辑 | PASS | 编译/测试/格式化全在 0.88 |
| 7 `-count=1` | PASS | `[4]`/`[5]`/`[8]`/`[10]` 全带 |
| 8 四路文件域不重叠 | PASS | 见 §一；`lightmem.md` 按节号顺序追加 |
| 9 对抗样本不进 holdout | PASS | `injection_v2.jsonl` 独立入口、独立报告 |
| 10 语料只增文件 | PASS | 新增 3 个 jsonl，既有条目零改动 |
| 11 接线默认关 | PASS | `[10]` + 北极星逐字节一致 |
| 12 SKIP 不冒充 PASS | PASS | PG 无环境时 13 项 SKIP 且不计 PASS |

---

## 七、文档（主 2）

`lightmem.md` 已含：**§3.2.3**（C）｜**§3.5.1.1**（B）｜**§5.3.1.1**（B）｜**§7.5.1**（A）｜**附录 D 更新**（D，含 D.7 更正记录：§二.3 对 R14 任务书 §九.3 的 `recorded_at` 更正）｜**附录 B 版本历史 v2.4.2**（主 Agent 追加）。`monty.md` 未动。

---

## 八、交付物索引

| 文件 | 说明 |
|---|---|
| `eval/reports/r15_taskbook_20261015.md` | R15 四路任务书 |
| `eval/reports/r15a_delivery_2026-10-15.md` | A 路交付报告 |
| `eval/reports/r15b_delivery_20261015.md` | B 路交付报告 |
| `eval/reports/r15c_delivery_mergence_v3.md` | C 路交付报告 |
| `eval/reports/r15d_delivery_20261015.md` | D 路交付报告 |
| `eval/reports/r15d_report_split_convention.md` | D4 报告拆分约定 |
| `eval/reports/r15_merge_final_20261015.md` | **本报告**（合流） |
| `eval/reports/r15_final_20261015.json` | 最终北极星报告（摘要） |
| `eval/polaris/r15_gate_diff/*.diff` | [7b] 受控修改审计存档 |

---

## 九、未闭环（留 R16）

1. **PG 用例未进入 `gate.sh`**：仍需手动设环境变量触发。建议 R16 加 `[11] PG 真机往返`，无环境时记 SKIP。
2. **D4 约定未执行**：下一次北极星产出时执行明细拆分，并同步更新 `gate.sh` 与交付报告模板。
3. **`[1b]` 全仓 gofmt 仍是 INFO**：语义为「只报告不计 FAIL」，即使全仓归零也只打印 INFO 行。若希望它成为硬门禁，需改判定口径。
4. **真实攻击面未测**（A 路）：语料为合成对抗样本，真实投毒样本检出率未知。
5. **检测器性能未基准**（A 路）：全量扫描为线性成本，100KB 以上正文耗时未实测。
6. **`holdout_v3` 不替代 `holdout_v2`**（C 路）：何时升级为正式评测集需单独口径评审。
7. **实体/任务规则化抽取器未实现**（C 路）：v3 的 entity/task 是语料侧人工标注。
