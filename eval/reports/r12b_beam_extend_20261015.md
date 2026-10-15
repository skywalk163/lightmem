# R12-B 交付报告：BEAM 扩展子集

> **版本**：v1 ｜ **日期**：2026-10-15 ｜ **基线**：`a0ca14f`（R11 收口态）
> **项目**：lightmem ｜ **验证机**：192.168.0.88（FreeBSD 15.1-RELEASE-p1，go1.25.14）
> **工作树**：`/tmp/r12b/lightmem`（从 gitea origin `a0ca14f` clone）

---

## 一、交付物清单

| # | 文件 | 说明 |
|---|---|---|
| B.1 | `eval/holdout/beam_extend/build_beam_extend.py` | 从 raw/beam.json 提取剩余未使用 300 条，适配成 BEAM 格式 holdout；确定性 seed=42 |
| B.2 | `eval/holdout/beam_extend/holdout_beam_extend.jsonl` | 300 条，字段与现有 BEAM holdout 逐字段一致 |
| B.3 | `eval/reports/r12b_beam_extend_20261015-005319.json` | 单独判定报告（vector 口径） |
| 辅助 | `eval/holdout/beam_extend/corpus_beam_extend.txt` | 向量语料（600 条去重文本：300 query + 300 fact） |
| 辅助 | `eval/vectors/vectors*.{npy,json,jsonl}` | BGE-m3 静态向量（n=600, dim=1024），验证机生成产物 |

---

## 二、构造口径

### 2.1 取数逻辑

BEAM raw 共 700 条，去重后 memory_id 唯一。按「已被占用即排除」原则切分：

| 用途 | 条数 | 来源文件 |
|---|---|---|
| 公开 MRR 已用 | 100 | `eval/benchmarks/dataset/beam.jsonl` |
| R11 真实语料 holdout 已用 | 300 | `eval/holdout/holdout.jsonl`（`beam-*` 前缀） |
| **本路剩余未使用** | **300** | `eval/benchmarks/raw/beam.json` |

- 严格排除上面 400 条 ID（`100 + 300 = 400`），`700 - 400 = 300`，与任务书预期一致；
- 与 MRR 集 **零重叠**，与 R11 holdout **零重叠**（脚本内 assert 自检）。

### 2.2 格式对齐

`adapt_beam_item` 与 R11 `build_holdout.py` 逐字段一致：

- `tenant=t-beam` / `user=u-beam` / `reference_frame.scene=beam-holdout`
- `memory.fact = "{question} => {expected_fact}"`；`expected_fact` 取 `rubric` 以 `" | "` 合并（无 rubric 回落 `ground_truth_answer`）
- `expected_memory_ids=[memory_id]`、`is_temporal = (bucket=="temporal")`
- bucket 映射与 R11 同表（`abstention/summarization→open-domain` 等）

本地比对校验（`_r12b_scratch/check_align.py`）结论：

```
holdout beam: 300  extend: 300
top-level key mismatch: 0
memory key mismatch: 0
fact == query + ' => ' + expected_fact: True
overlap extend&holdout: 0   extend&dataset: 0
```

### 2.3 分桶分布

| Bucket | n |
|---|---|
| multi-hop | 125 |
| single-hop | 60 |
| temporal | 59 |
| open-domain | 56 |
| **合计** | **300** |

---

## 三、单独判定结果（B.3，vector 口径）

命令：

```bash
cd /tmp/r12b/lightmem
go run eval/polaris/polaris_holdout.go \
  eval/polaris/polaris_fact.go eval/polaris/polaris_m1.go \
  eval/polaris/polaris_m2.go eval/polaris/polaris_token.go \
  eval/polaris/compress_light.go eval/polaris/polaris_fp.go \
  -holdout ./eval/holdout/beam_extend/holdout_beam_extend.jsonl \
  -embedder vector -report r12b_beam_extend -now-fixed 2026-10-15
```

### 3.1 M1 零 token 覆盖率

| 口径 | n | hits | coverage | hit@1 |
|---|---|---|---|---|
| vector（BGE-m3） | 300 | 300 | **1.0000** ✅ | 1.0000 |

**验收判定：M1 = 1.0000 ≥ 0.95，达标。**

### 3.2 分桶明细

| Bucket | n | hits | coverage | hit@1 |
|---|---|---|---|---|
| multi-hop | 125 | 125 | 1.0000 | 1.0000 |
| single-hop | 60 | 60 | 1.0000 | 1.0000 |
| open-domain | 56 | 56 | 1.0000 | 1.0000 |
| temporal | 59 | 59 | 1.0000 | 1.0000 |

### 3.3 控制面 / 数据面 / FP

| 指标 | 数值 |
|---|---|
| M2（控制面 token 占比） | control=17,728，data=62,443，M2=0.2211 |
| 数据面压缩 | 93,083 → 62,443，saved=30,640，reduction=32.92% |
| FP 审计 | v4_hits=300，vetoed=0，残留 FP=0 |

---

## 四、过程发现：向量语料的行式换行缺陷（已修复并复现）

首次判定（修复前）结果为 **M1 = 0.9967**（299/300），唯一 miss：

```
beam-1M_5_q16_summarization  (open-domain)  m1_hit=false
```

**根因**：该条 `memory.fact` 的 rubric 内含**内嵌换行符**。`corpus_*.txt` 按行读取，
内嵌换行会把 1 条文本拆成 2 行，导致 `gen_vectors.py` 写出的是两段半截文本的向量，
而 Go 侧 `VectorEmbedder.Embed` 查表用的是**完整 fact 的归一化键** → 该记忆拿到全零向量
（`kernel/embedder_vector.go`：未命中返回全零向量），语义信号丢失 → 召回排序下降 → miss。

**修复**：`build_beam_extend.py` 写语料时把 `\r`/`\n` 替换为空格。
这与 Go 侧 `normalizeVectorKey`（小写 + 空白折叠）语义等价，
保证写入键与查表键逐字符相同；脚本重跑后 `jsonl` 逐字节不变（`cmp` 通过）。

**复现验证**：

| 轮次 | 语料行数 | 索引覆盖 | M1 |
|---|---|---|---|
| 修复前 | 601（1 条被拆行） | fact miss=1 | 0.9967 |
| **修复后** | 600 | query miss=0 / fact miss=0 | **1.0000** |

> 附：该缺陷在 R11 的 `eval/holdout/build_corpus.py` 中同样存在（同一行式写法规避）。
> 本路按红线 6 不修改既有文件，仅在 B 路自建脚本中规避；建议合流阶段一并复核。

---

## 五、门禁与红线自查

| 项 | 状态 |
|---|---|
| B.1 脚本交付 | ✅ `eval/holdout/beam_extend/build_beam_extend.py` |
| B.2 数据集交付（300 条，与 BEAM holdout 格式一致） | ✅ |
| B.3 M1 ≥ 0.95 | ✅ 1.0000 |
| 与 MRR 100 条零重叠 | ✅ |
| 与 R11 holdout 300 条零重叠 | ✅ |
| 确定性（seed=42，同输入逐位一致） | ✅ jsonl 重跑逐字节相同 |
| 只增不改 Schema / 融合算法 / 既有 SQL / 判定口径（matchFactV4 未动） | ✅ |
| 未修改 `eval/holdout/holdout.jsonl`（只读引用） | ✅ |
| 不调 LLM 生成评测集 | ✅（BGE 嵌入属确定性嵌入例外条款） |
| 本机仅编辑 + SSH/传输，编译/测试/运行在 0.88 | ✅ |
| 未 push（等待用户示意） | ✅ |

---

## 六、对整体北极星（合流）的贡献

按任务书 §三·合流预期：

```
新 holdout = 现有 BEAM 300（R11）+ BEAM 扩展 300（本路）+ LoCoMo v2 200（A 路）= 800
```

本路 BEAM 扩展 300 条单独 M1 = **1.0000**，高于任务书预估的 0.96，
为合流后的整体 M1 ≥ 0.95 提供正向拉动。

---

## 七、复现步骤（0.88）

```bash
# 1) 取基线
git clone --depth 1 http://192.168.1.5:3000/skywalk/lightmem.git /tmp/r12b/lightmem

# 2) 生成扩展子集与向量语料
cd /tmp/r12b/lightmem
python3 eval/holdout/beam_extend/build_beam_extend.py

# 3) 生成 BGE-m3 静态向量（n=600）
python3 eval/vectors/gen_vectors.py \
  -in  eval/holdout/beam_extend/corpus_beam_extend.txt \
  -out eval/vectors/vectors.npy \
  -meta eval/vectors/vectors_meta.json \
  -index eval/vectors/vectors_index.jsonl \
  -model BAAI/bge-m3

# 4) 单独判定
go run eval/polaris/polaris_holdout.go \
  eval/polaris/polaris_fact.go eval/polaris/polaris_m1.go \
  eval/polaris/polaris_m2.go eval/polaris/polaris_token.go \
  eval/polaris/compress_light.go eval/polaris/polaris_fp.go \
  -holdout ./eval/holdout/beam_extend/holdout_beam_extend.jsonl \
  -embedder vector -report r12b_beam_extend -now-fixed 2026-10-15
```
