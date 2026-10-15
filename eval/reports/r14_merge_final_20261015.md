# R14 收口报告：三路合流门禁 + 最终北极星

> **任务书**：`eval/reports/r14_taskbook_20261015.md`
> **日期**：2026-10-15 ｜ **基线**：R13 收口态 `b361e80`（验证机工作树 `/tmp/r13/lightmem`）
> **验证机**：192.168.0.88（FreeBSD 15.1-RELEASE-p1，go1.25.14）｜ 合流工作树 `/tmp/r14/lightmem`
> **版本口径**：主标题保持 **v2.4**，本条目为 **v2.4.1 修订号**（按你的口径，不升 v2.5）
> **结论**：**A/B/C 三路全上、保守缺省；门禁 8/8 PASS；北极星 M1 = 1.0000 红线未动；与 R13 报告逐字段一致。**

---

## 一、一句话结论

R14 在 R13 收口态上**只加 6 个新文件**（3 个实现 + 3 个测试）+ 1 个门禁脚本，
落地《千脑智能》Monty 建议 3（参考系漂移衰减）、建议 4（L0→L1 flush 决策）
与 R13 遗留的位移边持久化，并补上统一门禁。

**默认检索 / 融合 / 写入路径与既有 Schema 语义零改动**：任务书 G8 的 10 个
受保护文件 + A/C 路自加的 2 个文件（共 12 个）对基线**逐文件 `diff -q` 一致**；
`eval/holdout/holdout_v2.jsonl` **完全相同**；`eval/vectors/` 向量数据
**无差异**（R14 树仅多 4 个生成脚本）。

北极星 holdout 官方判定：**M1 = 1.0000（800/800）、hit@1 = 1.0000、
mean_rr = 1.0000、M2 = 0.2498、压缩 35.33%、fp vetoed = 0**，
与 R13 报告**逐字段相同**（含 800 条 details）。

---

## 二、门禁矩阵（`scripts/gate.sh` 一次跑完）

```text
$ cd /tmp/r14/lightmem && R14_BASELINE=/tmp/r13/lightmem sh scripts/gate.sh
[0] go 版本 (要求 >= 1.25)                       PASS  (go1.25.14)
[1] gofmt -l 本轮新增文件                      PASS  (0 文件待格式化)
[1b] gofmt -l 全仓（既有噪音，仅报告）   INFO  (4 文件待格式化)
       ./eval/packages/cmd/package-family/main.go
       ./eval/e2e/pg/pg_e2e_test.go
       ./eval/e2e/pg/pg_bench_test.go
       ./eval/e2e/packages/package_e2e_test.go
[2] go vet ./... (逐 module)                        PASS  (2/2 通过)
[3] go build ./... (逐 module)                      PASS  (2/2 通过)
[4] go test -count=1 ./... (逐 module)              PASS  (2/2 通过 (-count=1))
[5] go test -count=1 -race (kernel)                  PASS  (无竞态)
[6] 新增文件测试覆盖 (阈值 12)             PASS  (forgetting_drift,flush,edge_persist)
[7] 红线抽查 (只增不改白名单)             PASS  (基线 /tmp/r13/lightmem，白名单逐文件一致)

========== R14 门禁矩阵 ==========
PASS: 8     FAIL: 0     SKIP: 0    模块数: 2
===================================
```

**汇总：PASS 8 / FAIL 0 / SKIP 0 / 模块数 2。**

[6] 的每文件测试函数数：`forgetting_drift_test.go` 19、`flush_test.go` 24、
`edge_persist_test.go` 14，**合计 57 项**，每文件均过 12 项阈值。

---

## 三、最终北极星（holdout 官方判定，R14 收口态）

报告文件：`eval/reports/r14_final_20261015.json`
（原始时间戳副本 `eval/reports/r14_final_holdout_20261015-105651.json` 一并入库）

| 指标 | R14 收口态 | 任务书 G6 要求 | 判定 |
|---|---|---|---|
| M1 零 token 覆盖率 | **1.0000**（800/800） | 1.0000（800/800） | ✅ 逐位一致 |
| hit@1 率 | 1.0000 | 1.0 | ✅ |
| mean RR | 1.0000 | 1.0 | ✅ |
| id_hit_rate | 1.0000 | — | ✅ |
| 四桶覆盖 | single-hop 149 / temporal 165 / open-domain 224 / multi-hop 262，全 1.0 | 1.0 | ✅ |
| M2 控制面占比 | **0.2498**（control=43126 / data=129537 / total=172663） | 0.2498 | ✅ 逐位一致 |
| 数据面压缩 | **35.33%**（200295 → 129537，省 70758 token） | 35.33% | ✅ |
| fp vetoed | **0**（v4_hits=800，residual_fp=800） | 0 | ✅ |
| 语料判定 | `official_corpus=true`，`mode=holdout-official` | 正式语料 | ✅ |

**配置**：`embedder=vector:BAAI/bge-m3`、`topk=5`、`weights=rank_weights_en_r5.json`、
`now_fixed=2026-10-14`（固定基准日，勿用 `time.Now`）。

### 3.1 与 R13 报告的逐项对照（实测，非声明）

把 R13 收口态的 `r13_polaris_final_20261015-070013.json` 与 R14 的
`r14_final_20261015.json` 做深度比较：

| 字段 | 结果 |
|---|---|
| `m1` | **identical** |
| `m2` | **identical** |
| `data_plane_compression` | **identical** |
| `fp_audit` | **identical** |
| `details`（800 条明细，规范化后逐条比较） | **identical** |
| `config` | 仅 `holdout` 路径写法差异（`./eval/...` vs `eval/...`，同一文件的相对路径前缀） |

即：**R14 的三项新增能力没有改变北极星判定的任何一条输出**——
漂移衰减是旁路算子、flush 是纯决策、位移边落盘在存储层，
三者在北极星链路上都**没有被调用**，因此输出逐位相同是预期结果，不是巧合。

---

## 四、一条口径更正（如实登记：本附录初版写错，已更正）

R14 过程中曾出现 M2 = **0.1772** 的数字，一度被归因为"评测向量集快照变更
（4595 条）"。**该归因错误**，现更正如下（同机同环境实测）：

| 入口 | M2 | control | data |
|---|---|---|---|
| `polaris_holdout.go`（`-mode holdout`，**正式入口**） | **0.2498** | 43126 | **129537**（压缩后） |
| `polaris_eval.go -mode eval`（eval 口径） | 0.1772 | 43126 | **200295**（未压缩） |

差异原因：**入口口径不同**——
`polaris_holdout.go` 在数据面 token 计数前应用了压缩
（`data_plane_compression`：200295 → 129537），
`polaris_eval -mode eval` 用未压缩数据面计数。
`control_tokens` 两边都是 43126，**逐位相同**；
差值 70758 **正好等于压缩节省量**。

排除"向量快照变更"假设的实测证据：

1. `diff -rq /tmp/r13/lightmem/eval/vectors /tmp/r14/lightmem/eval/vectors`
   → **仅 R14 树多 4 个生成脚本**（`corpus.txt`、`gen_vectors.py`、
   `gen_vectors_inc.py`、`merge_vectors_inc.py`），**向量数据文件零差异**。
2. 在 **R13 基线工作树**上用 `polaris_eval -mode eval` 复跑：
   同样得到 M1 = 1.0000 / M2 = 0.1772 / data = 200295（
   `eval/reports/r13_recheck_r14env_20261015.json`）。
3. 该 recheck 报告的 `details` 规范化哈希与 R14 的
   `r14_polaris_final_20261015.json` **完全相同**——
   即 R14 代码改动对 eval 口径输出**零影响**。

**结论**：北极星横向对比**必须固定 `polaris_holdout.go -mode holdout` 这一入口**；
`polaris_eval -mode eval` 只作控制面/压缩的辅助验证，不参与北极星横向对比。

---

## 五、只增不改的红线证据

```text
$ diff -q /tmp/r13/.../holdout_v2.jsonl /tmp/r14/.../holdout_v2.jsonl
HOLDOUT_SAME                                  # 语料完全相同

$ for f in multihop rank_fusion lifecycle voting model goal embedder \
           storage backend_sqlite backend_pg displacement_edge router; do
    cmp -s /tmp/r13/lightmem/kernel/$f.go /tmp/r14/lightmem/kernel/$f.go \
      && echo "SAME $f" || echo "DIFF $f"; done
SAME multihop / rank_fusion / lifecycle / voting / model / goal /
SAME embedder / storage / backend_sqlite / backend_pg /
SAME displacement_edge / router                 # 12/12 全部 SAME
```

本轮**新增文件**（共 7 个，仓库无任何既有文件被改动）：

| 文件 | 行数 | 归属 |
|---|---|---|
| `kernel/forgetting_drift.go` | 371 | A 路 |
| `kernel/forgetting_drift_test.go` | 383 | A 路 |
| `kernel/flush.go` | 305 | B 路 |
| `kernel/flush_test.go` | 440 | B 路 |
| `kernel/edge_persist.go` | 254 | C1 |
| `kernel/edge_persist_test.go` | 430 | C1 |
| `scripts/gate.sh` | 340 | C2 |

另有 `lightmem.md` 的新增章节与版本条目（§3.5.1 / §5.3.1 / §5.4.1 + 附录 D，
附录 B 版本历史补 v2.4.1 行）——均为**追加**，不改动既有结论。

---

## 六、交付清单与状态

| 交付物 | 状态 |
|---|---|
| A 路：漂移衰减 + 测试 + §5.3.1 | ✅ `eval/reports/r14a_delivery_20261015.md` |
| B 路：flush 决策 + 测试 + §3.5.1 | ✅ `eval/reports/r14b_delivery_20261015.md` |
| C1：位移边持久化 + 测试 + §5.4.1 | ✅ `eval/reports/r14c_delivery_20261015.md` |
| C2：统一门禁 `scripts/gate.sh`（8 项矩阵） | ✅ 同上 C 路报告 |
| 文档 v2.4.1（三节 + 版本历史 + 附录 D 门禁与北极星复现） | ✅ `lightmem/lightmem.md` |
| 合流门禁矩阵 + 最终北极星报告 | ✅ `eval/reports/r14_final_20261015.json` + 本报告 |
| 三路交付报告 | ✅ 本报告 + A/B/C 三份 |
| 本地 commit | ✅（**未 push**，push 须你示意） |

### 自验命令（验证机）

```text
cd /tmp/r14/lightmem
tr -d '\r' < scripts/gate.sh > /tmp/gate.sh && R14_BASELINE=/tmp/r13/lightmem sh /tmp/gate.sh
cd kernel && go test -count=1 -race ./...
```

---

## 七、留 R15（本轮不做的 5 项，均已登记）

1. **漂移衰减未接默认检索链路**：保持旁路，等真实语料出现"新但不相关"的
   污染证据再接；接入前必须先补齐三轴标注，否则多数记忆三轴为空、衰减不触发。
2. **flush 决策无人调用**：接入点涉及 L0→L1 的语义合并（`recorded_at`、
   `memory_kind`、是否再压缩），风险高于本轮收益；建议放在 `SessionCompressor`
   之前。
3. **§5.4.1 的 PostgreSQL 真机往返**：本仓库无真实 `database/sql` 驱动，
   本轮只交付 JSON 落盘的真实往返；DDL 文本两套都已就位。
4. **Monty 建议 6（候选池 → 查询级假设集）**：需先有 §7.5 投毒防护的
   澄清交互契约。
5. **合并度指标基线为 0**：`holdout_v2` 全 L1 且无实体/时间标注，分母为 0；
   需外部真实 L2 语料 + 实体标注。

**门禁侧的 2 项遗留**：[1b] 的 4 个既有文件待 gofmt（既有噪音，刻意不动）；
门禁尚未覆盖 pgvector / Qdrant / 集合体多节点等需要真实环境的部分。
