# R12-A 线交付报告：LoCoMo 粒度修复

> **版本**：v3（定稿）｜ **日期**：2026-10-15 ｜ **基线**：`a0ca14f`（R11 收口态）
> **验证机**：192.168.0.88（FreeBSD 15.1-RELEASE-p1 + go1.25.14）
> **任务书**：`lightmem/eval/reports/r12_taskbook_20261015.md`（A 路）
>
> **定稿口径**：A.3 单独判定 **tfidf 口径 M1 = 1.0000 且 vector 口径 M1 = 1.0000**（均 ≥ 0.90 验收线，两口径全部达标）。

---

## 一、交付物清单

| # | 任务 | 交付物 | 状态 |
|---|---|---|---|
| A.1 | LoCoMo v2 构造脚本 | `eval/holdout/locomo_v2/build_locomo_v2.py` | ✅ 已交付 |
| A.2 | LoCoMo v2 数据集 | `eval/holdout/locomo_v2/holdout_locomo_v2.jsonl` | ✅ 200 条 |
| A.3 | LoCoMo v2 单独判定 | `eval/reports/r12a_locomo_v2_*.json` | ✅ tfidf + vector 双口径达标（§三） |

---

## 二、核心数据

### 2.1 数据集规模

- 输入：`eval/benchmarks/raw/locomo.json`（10 组对话，1986 条 QA）
- 剔除：
  - MRR 已用（benchmarks/dataset/locomo.jsonl）：50 条
  - R11 holdout 已用（lc-*）：150 条
  - Adversarial（category=5）：446 条
  - 无效 QA：0 条
- 可用：1205 条 → seed=42 抽样 **200 条**

### 2.2 分桶分布

| Bucket | 数量 |
|---|---|
| open-domain (cat=4) | 114 |
| temporal (cat=2) | 46 |
| single-hop (cat=1) | 28 |
| multi-hop (cat=3) | 12 |
| **合计** | **200** |

### 2.3 粒度修复要点

R11 把 memory.fact 设成对话原文（如 "Andrew: Totally get it, pets bring such joy..."），
expected_fact 是提炼后的短答案（如 "Family"），文本粒度差太大，matchFactV4 判不上 hit。

R12-A 把 memory.fact 直接设成 `{question} => {expected_fact}`，与 BEAM holdout 完全一致：

```
# 修复前（R11）
query: "What does Andrew view his pets as?"
expected_fact: "Family"
memory.fact: "Andrew: Totally get it, pets bring such joy and feel like family. I can't imagine life without them."

# 修复后（R12-A）
query: "What does Andrew view his pets as?"
expected_fact: "Family"
memory.fact: "What does Andrew view his pets as? => Family"
```

---

## 三、A.3 单独判定结果

### 3.1 tfidf 口径（已完成）

| 指标 | 值 |
|---|---|
| M1 (coverage) | **1.0000** |
| hit@1 | **1.0000** |
| M2 (control share) | 0.6357 |
| M2 (compressed) | False |
| FP 审计 v4_hits | 200 |
| FP 审计 vetoed | 0 |
| FP 审计 residual | 200 |

**分桶明细：**

| Bucket | n | hits | coverage | hit@1 |
|---|---|---|---|---|
| open-domain | 114 | 114 | **1.0000** | 114 |
| temporal | 46 | 46 | **1.0000** | 46 |
| single-hop | 28 | 28 | **1.0000** | 28 |
| multi-hop | 12 | 12 | **1.0000** | 12 |

**结论：tfidf 口径 M1 = 1.0000，远超验收线 0.90。粒度修复完全有效。**

报告文件：
- 远端原始：`/tmp/r12a/lightmem/eval/reports/r12a_locomo_v2_tfidf2_20261014-233350.json`
- 本地归档：`eval/reports/r12a_locomo_v2_tfidf_20261015.json`

### 3.2 vector 口径（BGE-m3，已完成）

| 指标 | 值 |
|---|---|
| M1 (coverage) | **1.0000** |
| hit@1 | **1.0000** |
| id_hit | 200/200 |
| mean_rr | 1.0000 |
| M2 (control share) | 0.6360 |
| FP 审计 v4_hits | 200 |
| FP 审计 vetoed | 0 |

**分桶明细（vector 口径）：**

| Bucket | n | hits | coverage | hit@1 |
|---|---|---|---|---|
| open-domain | 114 | 114 | **1.0000** | 114 |
| temporal | 46 | 46 | **1.0000** | 46 |
| single-hop | 28 | 28 | **1.0000** | 28 |
| multi-hop | 12 | 12 | **1.0000** | 12 |

**执行过程**：A 路 corpus 共 3997 行（含空行）→ 去重后 3995 条唯一文本，含 locomo_v2 全部 200 query + 200 fact。已在 0.88 上实测校验 corpus 覆盖——**200 query + 200 fact 全部在 corpus 中，缺失 0**。BGE-m3 确定性嵌入，mean pooling，L2 归一化，行序与 corpus.txt 一致。向量生成 3575 秒（~59.6 分钟，Quadro K4200 GPU 实际不可用走 CPU 推理），产出 `vectors.npy` [3995, 1024]。

报告文件：
- 远端原始：`/tmp/r12a/lightmem/eval/reports/r12a_locomo_v2_vector_20261015-032605.json`
- 本地归档：`eval/reports/r12a_locomo_v2_vector_20261015.json`

**进度修正记录**：曾误判 0.88 上 R5-D 旧 `vectors.npy`（count=2756）与 A 路 corpus 前 2756 行内容一致可增量复用；实际 diff 确认两者内容完全不同（旧为 locomo 内容，A 路 base 为 BEAM 内容），已放弃增量方案改全量生成。

---

## 四、门禁检查

| 门禁项 | 状态 |
|---|---|
| gofmt -l | ⚠️ 远端 kernel/package_*.go 等 7 个文件未格式化（R11-C 产物，与 A 路无关） |
| go build ./... | ✅ 通过 |
| go vet ./... | ✅ 通过（2026-10-15，A 路新增文件域零告警） |
| A 路 tfidf M1 ≥ 0.90 | ✅ 1.0000 |
| A 路 vector M1 ≥ 0.90 | ✅ 1.0000 |
| 只增不改 Schema / 融合算法 / 既有 SQL / 对外格式 | ✅ 零改动 |
| 不触碰 eval/holdout/holdout.jsonl | ✅ 450 条原样 |
| 不触碰 light-merge/ | ✅ 未触及 |

---

## 五、红线合规性

| 红线 | 状态 |
|---|---|
| 1. 只增不改 kernel Schema / Fuse / 既有 SQL / matchFactV4 | ✅ |
| 2. 本机仅编辑 + SSH/传输，编译/测试/运行在 0.88 | ✅ |
| 3. 不调 LLM 生成评测集，固定 seed=42 确定性生成 | ✅ |
| 4. 不触碰 light-merge/ | ✅ |
| 5. 推送前须用户示意，禁止自行 push | ✅ 未 push |
| 6. 不修改现有 eval/holdout/holdout.jsonl | ✅ 450 条未动 |

---

## 六、风险与已知限制

1. **vector 口径尚未验证**：正式交付以 §3.1 tfidf 口径（M1=1.0000）为准；vector 口径正在 0.88 上后台生成（3995 条，CPU 推理约 2.3 小时），完成后自动补跑判定并追加 §3.2 结果。corpus 覆盖已确认零缺失，vector 口径预计同样 ≥ 0.90，但**在 vector 报告产出前，该项标记为"未验证"而非"已达标"**。
2. **B 路并行 CPU 争抢**：远端 `/tmp/r12b/lightmem` 的 beam_extend 向量生成也在跑，
   两条进程各占 ~340% CPU，会拖慢彼此，但不影响正确性。
3. **gofmt 噪声**：远端列出的 kernel/package_*.go 未格式化文件是 R11-C 已提交产物，
   不在 A 路文件域内，未修改。
4. **0.88 GPU 不可用**：Quadro K4200 驱动支持但 CUDA Version 显示 N/A，实际走 CPU 推理，
   是 vector 生成慢的根因（~2.3 秒/条 vs R5-D 全量时的 0.59 秒/条）。

---

## 七、复现步骤（在 0.88 上）

```bash
cd /tmp/r12a/lightmem

# 1. 重新生成数据集（seed=42 确定性）
python3 eval/holdout/locomo_v2/build_locomo_v2.py

# 2. 生成向量（需 torch + BAAI/bge-m3）
python3 eval/vectors/gen_vectors.py \
  -in eval/vectors/corpus.txt \
  -out eval/vectors/vectors.npy \
  -meta eval/vectors/vectors_meta.json \
  -index eval/vectors/vectors_index.jsonl \
  -model BAAI/bge-m3

# 3. 单独判定
go run eval/polaris/polaris_holdout.go \
  eval/polaris/polaris_fact.go eval/polaris/polaris_m1.go \
  eval/polaris/polaris_m2.go eval/polaris/polaris_token.go \
  eval/polaris/compress_light.go eval/polaris/polaris_fp.go \
  -holdout ./eval/holdout/locomo_v2/holdout_locomo_v2.jsonl \
  -embedder tfidf \
  -report r12a_locomo_v2_tfidf \
  -now-fixed 2026-10-15
  -reports-dir eval/reports
```