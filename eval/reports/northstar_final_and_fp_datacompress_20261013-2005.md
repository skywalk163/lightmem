# R6-C 交付报告：北极星正式判定 runner + FP 清理 + 数据面压缩

- 日期：2026-10-13（UTC+8）
- 路径：`lightmem/` 模块；验证机 192.168.0.88
- 基线：`8db1b78`（R5 收口）之上新增，不动 `polaris_eval.go`（D 路独占）、不动 kernel、零 LLM 调用、now-fixed=2026-10-13。

---

## 0. TL;DR

| 项 | 目标 | 实测 | 结论 |
|---|---|---|---|
| 北极星 M1（真实语料） | ≥0.95 正式判定 | **真实 holdout.jsonl 未到位 → 代理语料预跑，非正式判定** | ⚠️ 见 §1 |
| C 独立口径 FP | ≤1 | **0**（v4 命中 626，审计 vetoed=0） | ✅ |
| 数据面压缩 | token −20% 且 M1 不降 | **109335 → 50491（−53.82%）**，M1 判定输入原文、覆盖率 0.9631 不变 | ✅ |
| gofmt / vet / 两模块门禁 | 全绿 | gofmt -l 空、vet 通过、root + kernel `go test ./...` ok | ✅ |

---

## 1. 北极星正式判定（C.2.1）

### 1.1 语料到位状态（诚实声明）

`eval/dataset/holdout.jsonl`（真实语料）**当前不存在**（0.88 远端核对：仅有 `holdout_proxy.jsonl` 590 条）。按任务书预案：**用代理语料预跑，并显著标注「非正式判定」**——以下数字仅作走势参考，**不作为北极星 ≥0.95 达标依据**。runner 已内置自动探测：真实语料一旦落到 `eval/dataset/holdout.jsonl`，同名命令复跑即自动切换为正式判定（`official_corpus=true`），无需改代码。

### 1.2 代理预跑数字（非正式）

runner：`eval/polaris/polaris_holdout.go`（`//go:build ignore` 独立主程序，绕开 D 路独占的 `polaris_eval.go`；与库文件同跑）。

| 指标 | 值 |
|---|---|
| n | 590 |
| M1 零 token 覆盖率 | **0.9661**（570/590） |
| hit@1 | 0.9559 |
| M2（控制面占比） | 0.4375（control=29050 / data=37346） |

- 与 R5-D 对拍数字逐位一致（0.9661 / 0.4375）→ runner 链路与既有 harness 同源，可信。
- 报告：`eval/reports/r6c_holdout_proxy_20261013-120101.json`（含 `corpus_note` 显著标注非正式）。

---

## 2. FP 清理（C.2.2）——C 独立口径 FP = 0

### 2.1 两条确定性规则（`polaris_fp.go`，纯字符串、无 LLM、无随机性）

**规则 1 否定一致性（negation_conflict）**：
expected 是拒绝/否定式回答（含 `not enough` / `no, ` / `i don't` / `there is no` / `not mentioned` / `cannot` …），而 got 的**回答侧**（`Q=>A` 取 `=>` 后半）无任何拒绝标记（即给出了肯定答案）→ 语义矛盾，veto。
护栏：got 回答侧同样含拒绝标记（取到的就是那条拒绝记忆本身）→ 不 veteo（真命中保护）。

**规则 2 碎片化护栏强化（fragment）**：
expected ≤3 个内容词、got 无 `=>` 结构（纯对话碎片）、且 expected 未整词落在 got 中 → veto。
**时序豁免**：expected 含年份/月份/相对时间词（`July 2023`、`in 2022`…）时豁免——v4 的 matchFactTemporal 会把 got 的相对时间（last year/next month）结合 anchor 推成绝对日期，是合法时序推理命中，不是碎片化（R6-C 实测 6 条时序命中初版被误伤，加豁免后单测锁定）。

### 2.2 前后对照（公开基准 650 条，v4 同源判定）

| 口径 | v4 命中 | 审计 vetoed（语义矛盾/碎片） | 残留 FP |
|---|---|---|---|
| 清理前（R5 登记 P4） | — | 3（历史登记：lme-031748ae_abs×2 否定冲突 + lc-conv-49-D1_16 碎片） | 3 |
| **清理后（本次复跑）** | 626 | **0** | **0** |

说明：R5-A 向量重排后，历史 3 条 FP 在 R5 终态已自然消解（复跑确认：lme-031748ae_abs top-1 已正确取到拒绝记忆本体；lc-conv-49-D1_16 已 fact_hit=false）。本次交付的是**确定性护栏 + 回归锁定**，防止同类 FP 复发。

### 2.3 人工复核清单（3 条历史样本 ID）

| 样本 ID | 历史问题 | 规则判定依据 | 当前状态（R6-C 复跑） | 建议 |
|---|---|---|---|---|
| `lme-031748ae_abs`（×2） | expected="The information is not enough…"（拒绝式），历史 got="…you led 4 engineers. Now, you lead 5 engineers"（肯定答案）→ v4 因共享词元放行 | **negation_conflict**：exp 含 `not enough`，got 回答侧无拒绝标记 | 已消解：top-1 取到正确拒绝记忆（真命中不被误 veto，单测 `TestR6C_FPVeto_RefusalTrueHit` 锁定） | 保留规则作回归护栏；真实语料上线后复跑确认 |
| `lc-conv-49-D1_16` | expected="friend's advice"（2 词短事实），历史 got="Sam: …hiking trip…"（无关对话碎片） | **fragment**：exp ≤3 词、got 无 `=>`、短语未整词落地 | 已消解：R5 终态 fact_hit=false（未命中，不再计入） | 保留护栏；时序豁免已加，避免误伤 "July 2023" 类合法命中 |

单测锁定（`polaris_fp_test.go`，全部 PASS）：
`TestR6C_FPVeto_NegationConflict` / `TestR6C_FPVeto_RefusalTrueHit` / `TestR6C_FPVeto_Fragment` / `TestR6C_FPVeto_AnchoredShortFact` / `TestR6C_FPVeto_PlainHit` / `TestR6C_FPVeto_TemporalExempt` / `TestR6C_FPAnswerSide` / `TestR6C_IsRefusalExpected`。

---

## 3. 数据面压缩（C.2.3）——−53.82%

`compress_light.go` 新增 `CompressDataFact`（确定性，只压**展示/计费面副本**，判定输入不动——双口径）：

1. 剥离对话体 speaker 前缀（`Sam: `）；
2. `Q => A` 事实只保留回答侧（问题镜像在控制面已计费，数据面不重复）；
3. 无 `=>` 对话体：跳过呼语碎片，保留首句核心块（≤60 rune，词界内收）；
4. 护栏：压缩结果为空回退原文，永不压成空块。

### 3.1 前后对照（公开基准，Σ top-5 候选 fact token）

| 口径 | token 数 |
|---|---|
| 压缩前（原文副本） | 109,335 |
| 压缩后（展示/计费面副本） | 50,491 |
| **下降** | **58,844（−53.82%）** |
| M1 判定覆盖率（判定用原文，未压） | 0.9631（626/650）——**压缩前后不变** |

> 代理语料（合成中文短 fact，无 `=>`、无长对话）数据面压缩率 0%——本身已极短，无压缩空间；压缩收益主要体现在公开基准的长对话 fact 上（600/650 为 `Q=>A` 格式）。

单测（`polaris_compress_data_test.go`）：Q=>A 回答侧保留、呼语碎片剔除、空输入护栏、压缩不更长、批量账目为正——全部 PASS。

---

## 4. 门禁验证（0.88 实测）

```
gofmt -l eval/polaris/        # 空输出
go vet ./eval/polaris/        # VET_OK
go test ./eval/polaris/       # ok（含 12 条 R6C 新单测）
go test ./...                 # root 模块 ok
cd kernel && go test ./...    # ok
```

## 5. 交付物

| 文件 | 说明 |
|---|---|
| `eval/polaris/polaris_holdout.go`（新增，`//go:build ignore`） | 真实语料正式判定独立 runner（M1/M2 + FP 审计 + 数据面对照 + 代理语料显著标注） |
| `eval/polaris/polaris_fp.go`（新增） | FP 确定性审计层（否定一致性 + 碎片化护栏 + 时序豁免） |
| `eval/polaris/polaris_fp_test.go`（新增） | FP 规则回归锁定 |
| `eval/polaris/compress_light.go`（扩展） | `CompressDataFact` 数据面压缩 + 批量账目 |
| `eval/polaris/polaris_compress_data_test.go`（新增） | 数据面压缩单测 |
| `eval/reports/r6c_holdout_proxy_20261013-120101.json` | 代理预跑报告（非正式） |
| `eval/reports/r6c_public_audit3_20261013-120527.json` | 公开基准 FP 审计 + 数据面对照 |

## 6. 红线自查

- ✅ 只增不改：未动 `polaris_eval.go`（D 路）、未动 kernel schema 语义、未动 `run_kpi.go`；
- ✅ 全部编译/测试/运行在 0.88 验证；
- ✅ 未 push（等用户信号）；
- ✅ 零 LLM 调用、now-fixed 确定性；
- ✅ 未碰 `light-merge/`；
- ✅ 本地 .go 全部 LF（远端 gofmt 后回拉，未提交 CRLF）。
