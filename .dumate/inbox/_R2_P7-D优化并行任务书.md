# R2 独立任务书：P7-D 四路优化并行推进（RankFusion 英文重校准 / temporal 时态样本 / fact 口径 / multi-hop 分桶）

> **版本**：v1 ｜ 日期：2026-10-11 ｜ **任务编号**：R2（沿用 lightmem 独立编号，R1 为外部基准数据集）
> **依据**：`lightmem/roadmap.md` / `lightmem/roadmap-v4.html` P7-D 延伸条目；`eval/reports/benchmarks_summary_p7d.md` §6「后续建议」五项中的四项
> **前置**：R1 已交付公开基准数据集 650 条（`eval/benchmarks/dataset/all.jsonl`，LME 500 + LoCoMo 50 + BEAM 100）；`d1c4707` 已完成 P7-D 公开基准对标，产出 `eval/reports/benchmarks_summary_p7d.md`（公开基准 MRR 0.546 vs 合成 0.821）
> **任务书归档位**：`lightmem/.dumate/inbox/_R2_P7-D优化并行任务书.md`
> **分发方式**：本任务书由用户**分发**给 4 条并行子任务执行者；4 路互不依赖文件，可同时开工
> **红线（沿用 R1，全程适用）**：
> 1. **只增不改** kernel 模块 Schema 语义（`lightmem.md` §4.1 单一真源 + 只增原则）。
> 2. **所有编译/测试/运行必须在 192.168.0.88 上验证**，本机仅做源码编辑与 SSH 执行/文件传输。
> 3. **推送前须用户示意**，禁止自行 push。
> 4. **不调大模型**生成/改写 query 或 expected 答案；不调 LLM 参与任何权重搜索、事实推断。
> 5. **固定 seed=42 确定性**：所有 shuffle / 采样沿用 `rand.New(rand.NewSource(42))`；禁止引入 `time.Now()`、map 迭代序等不确定源。
> 6. **不触碰 `light-merge/`**（light 语言项目）；落点仅在 `lightmem/eval/` 与 `lightmem/kernel/`（A 路涉及 kernel 权重加载则属允许范围，见 A 路 §A.0）。

---

## 0. 任务来源与四路总览

### 0.1 来自 P7-D 报告 §6 的后续建议（用户 2026-10-11 指定四项）

| 报告 §6 条目 | 原文 | 本轮任务 | 路由 |
|---|---|---|---|
| 2 | RankFusion 权重针对英文长文本重校准，重点提升 top-1 排序 | 权重参数搜索 + 回归评测 | **A** |
| 3 | 为 LME/BEAM temporal 样本补 `as_of` 与双版本记忆（`SecondVersion`），使 as_of 过滤能真实验证 | 时态样本字段补全 | **B** |
| 4 | fact 评测口径改为「got 包含 expected 文本」判定（非精确相等） | 评测器 fact 判定改造 | **C** |
| 1（衍生） | 以 MRR 作为对标核心指标；multi-hop 分桶结构性约束需单独评估 | 分桶 MRR 单独评估 + 评测器分桶统计修正 | **D** |

### 0.2 双源基线（R2 出发口径，权威来源 `d1c4707`）

| 指标 | 合成 KPI (`eval/dataset/all.jsonl`, 530 条) | 公开基准 (`eval/benchmarks/dataset/all.jsonl`, 650 条) |
|---|---|---|
| 总体 MRR | 0.821 | 0.546 |
| 准确率 (memory_id, top-3) | 100.0% | 100.0% （饱和） |
| 准确率 (fact 文本, 精确相等) | 93.0% | 0.0% **（C 路要修的假阴性）** |
| temporal 桶 MRR | 0.988 | 0.552 |
| open-domain 桶 MRR | 0.980 | 0.527 |
| multi-hop 桶 MRR | 0.500 **（D 路要拆的疑似假低）** | 0.558 |

### 0.3 四路并行总览

| 路 | 内容 | 类型 | 涉及文件 | 改动量 | 依赖 |
|---|---|---|---|---|---|
| **A** | RankFusion 英文权重重校准（参数化加载 + 网格/局部搜索 + 双源回归） | Go + 脚本 | 新建 `eval/tune_rank_weights.go`、`eval/weights/rank_weights_en.json`（新增）；`eval/run_kpi.go`（加 `-weights` flag） | 中 | 需先用 C 路修正 fact 口径避免假阴性干扰；与 B 独立 |
| **B** | temporal 样本补 `as_of` + `SecondVersion`（LME/BEAM） | Go | 修改 `eval/benchmarks/bench/longmemeval.go`、`beam.go`、`helpers.go`、`sample.go`；重跑 `all.jsonl` | 中 | 独立 |
| **C** | fact 评测口径改「got 包含 expected」+ 归一化 | Go | 修改 `eval/run_kpi.go`（新增 fact 匹配辅助函数） | 小 | 独立，**A/D 最好等它先落地** |
| **D** | multi-hop 分桶结构性约束单独评估（+ 防御性加固 `!isAdversarial`） | Go | 修改 `eval/run_kpi.go`（multi-hop/temporal 桶判定加 `!isAdversarial`）+ 报告 §multi-hop 表格 | 小 | 独立 |

**波次建议**：**C → A → D → B**（C 消除 fact 假阴性使 A 的回归可信；D 单独评估 multi-hop 结构性约束并做防御性加固，避免结论被样本结构误导；B 纯数据增强独立并行，可与 A 同时开工）。

**判绿口径**：
- A 路：公开基准 top-1 命中率提升 > 5pp，MRR 提升 > 0.05，且合成集不回归（MRR 下降 < 0.01）。
- B 路：temporal 桶 as_of 有效样本 ≥ 90%，且 temporal MRR 提升 > 0.05。
- C 路：公开基准 fact 准确率从 0.0% 提升到 ≥ 50%，合成集不回归（≥ 90%）。
- D 路：multi-hop 桶统计加固（`!isAdversarial`）前后指标**完全一致**（防御性加固无副作用），报告明确合成集 MRR 0.500 的结构性成因与双源不可直接比较。

---

## 1. A 路 — RankFusion 英文权重重校准，主攻 top-1

### A.0 范围与红线澄清

- **允许**修改 `lightmem/kernel/rank_fusion.go` 以支持**从 JSON 文件加载权重**（该文件已有 `LoadWeightsFromFile` 方法，见 `kernel/rank_fusion.go:159`，可能已具备）；**若已具备则不改 kernel，仅在 `eval/` 层新增调用**。
- **禁止**改动 `Memory` Schema、`RouterContext`、召回/写入链路语义。**禁止**用 LLM 做权重搜索，只能用确定性脚本。

### A.1 背景（根因）

P7-D 报告 §4 关键观察 1：
> LME/BEAM 英文长文本，RankFusion 默认权重（针对合成集中文短文本）在英文上匹配弱。

`kernel/rank_fusion.go:106-115` `defaultWeightSpecs`：
```go
var defaultWeightSpecs = []DefaultWeightSpec{
	{Name: "semantic",       Weight: 1.0, Type: "calibrated", Note: "语义相似度（向量召回分近似）"},
	{Name: "keyword",        Weight: 0.8, Type: "calibrated", Note: "BM25 全文关键词"},
	{Name: "entity",         Weight: 0.7, Type: "calibrated", Note: "实体匹配"},
	{Name: "value",          Weight: 0.6, Type: "fixed",      Note: "记忆固有价值（§5.3）"},
	{Name: "freshness",      Weight: 0.5, Type: "fixed",      Note: "时效新鲜度（指数衰减）"},
	{Name: "source_trust",   Weight: 0.4, Type: "fixed",      Note: "来源可信度 人工>系统>用户"},
	{Name: "reference_frame",Weight: 0.6, Type: "calibrated", Note: "参考系匹配度"},
	{Name: "feedback",       Weight: 0.5, Type: "fixed",      Note: "反馈历史（采纳/弃用）"},
}
```

`RunKPI` 目前硬编码使用 `kernel.DefaultRankFusion()`（`eval/run_kpi.go:185`）。

### A.1.1 ⚠️ 关键前提：评测器当前未设 embedder

`kernel/rank_fusion.go` 的 `signalSemantic`（`kernel/rank_fusion.go:384`）实现：
```go
func (rf *RankFusion) signalSemantic(_ context.Context, query string, cands []RecalledCandidate) map[string]float64 {
	out := make(map[string]float64, len(cands))
	if rf.embedder != nil && query != "" {
		// ... 真实向量余弦
	}
	// embedder 为 nil 时，退化为召回分近似
	for _, c := range cands {
		if c.Memory == nil { continue }
		s := c.RecallScore
		if s < 0 { s = 0 }
		out[c.Memory.MemoryID] = s
	}
	return out
}
```

**实测 `eval/run_kpi.go` 全文件无 `SetEmbedder` 调用**（grep 确认），因此评测器里 `semantic` 信号**实际等于召回分 `RecallScore`**。

⚠️ **这意味着**：
1. 调 `semantic` 权重时，实际上是在放大/缩小**召回分本身**，而非真实向量相似度。
2. 若 A 路想让 semantic 信号发挥真实作用，**必须先为评测器提供 embedder**（可选，若本机/0.88 有可用的确定性向量化实现）。若**不提供 embedder**，则 A 路的有效信号是 `keyword / entity / reference_frame / value / freshness / source_trust / feedback`，**semantic 可跳过或仅微调**（因为调它 = 调 RecallScore，与召回层耦合）。

**A 路执行者开工前必须先决策**（二选一，记录在报告里）：
- **方案 α**（推荐，改动最小）：不引入 embedder，**跳过 semantic 权重搜索**，只调 `keyword / entity / reference_frame` 三个 `calibrated` 信号。理由：这三个信号对英文长文本最敏感（BM25 分词、实体抽取、参考系匹配）。
- **方案 β**：为评测器注入 embedder（需找到不调 LLM 的确定性向量化，如 char n-gram TF-IDF），使 semantic 信号真实生效后再调权重。⚠️ **若用 LLM 做 embedding 则违反红线 4**，禁止。

### A.2 目标

- 为公开基准（英文长对话）找到一组权重 `W_en`，使公开基准 **top-1 命中率提升 ≥ 5pp、MRR 提升 ≥ 0.05**。
- 合成集（中文短文本）**不得回归**：MRR 下降 < 0.01。

### A.3 交付

1. **`eval/run_kpi.go`**：新增 `-weights <path>` flag（默认空 = 用 `DefaultRankFusion()`；非空则调用 `kernel.LoadWeightsFromFile`）。参考已有 flag 写法（`eval/run_kpi.go:163-165`）：
   ```go
   dsFlag  := flag.String("dataset", dsDir+"/all.jsonl", "评测集 JSONL 路径")
   repFlag := flag.String("report", "kpi_baseline", "报告文件名前缀")
   wFlag   := flag.String("weights", "", "RankFusion 权重 JSON 路径；空则用 DefaultRankFusion()")
   flag.Parse() // 必须先 Parse 再取值
   dsPath, repPrefix, wPath := *dsFlag, *repFlag, *wFlag
   ```
   加载逻辑（`eval/run_kpi.go` 中，构建 `rf` 处替换 `rf := kernel.DefaultRankFusion()`）：
   ```go
   var rf *kernel.RankFusion
   if wPath != "" {
       rf = kernel.DefaultRankFusion()
       if err := rf.LoadWeightsFromFile(wPath); err != nil {
           log.Fatalf("load weights %s: %v", wPath, err)
       }
   } else {
       rf = kernel.DefaultRankFusion()
   }
   srv.SetRankFusion(rf)
   ```

2. **`eval/weights/rank_weights_en.json`**：JSON 格式对齐 `kernel/rank_fusion.go` 的 `LoadWeightsFromFile` 解析结构（`{ "k": <int>, "weights": [ {Name, Weight, Type, Note}, ... ] }`）。文件名 `rank_weights_en.json` 表示"英文基准调优版"。**必须在报告里标注这是 `A8` 后的调优产物，不是占位先验**。

3. **`eval/tune_rank_weights.go`**（`//go:build ignore`，与 `eval/benchmarks/sample.go` 同样式）：确定性权重搜索脚本。
   - **开工前必须选定 A.1.1 方案 α 或 β**，并在报告里记录选择理由。
   - **禁止**网格全枚举 8 维（组合爆炸）。建议：
     - 先固定 `fixed` 型信号权重（value/freshness/source_trust/feedback）不变。
     - **若选方案 α**：只对 `keyword / entity / reference_frame` 三个 `calibrated` 信号做**局部坐标下降**（每次只动一个权重，步长 ±0.1，接受使公开基准 MRR 上升的改动），最多迭代 20 轮；**跳过 `semantic`**（因为评测器无 embedder，semantic = RecallScore，调它等于调召回层）。
     - **若选方案 β**：先注入确定性 embedder，再对 `semantic / keyword / entity / reference_frame` 四个信号搜索。
     - 每轮在两个数据集上各跑一次 `RunKPI`（复用 `-dataset`/`-report` flag）。
   - **seed 确定性**：搜索顺序固定（不 shuffle 数据集），权重初始值 = 默认值，迭代顺序固定（α 为 `[keyword, entity, reference_frame]`，β 为 `[semantic, keyword, entity, reference_frame]`）。
   - 输出：每轮权重快照 + 公开基准 MRR + 合成 MRR + top-1 命中率，最后把最优权重写入 `eval/weights/rank_weights_en.json`。

4. **回归报告**：`eval/reports/tune_rank_weights_<ts>.md`（含默认 vs W_en 双源对比表）。

### A.4 命令（0.88 上执行）

```sh
# 1) 生成搜索脚本
cd /tmp/duan-light-merge/lightmem/eval
go run ./tune_rank_weights.go -dataset-bench ./benchmarks/dataset/all.jsonl \
  -dataset-synth ./dataset/all.jsonl -out ./weights/rank_weights_en.json

# 2) 用 W_en 跑公开基准
go run ./run_kpi.go -dataset ./benchmarks/dataset/all.jsonl \
  -report benchmarks_en -weights ./weights/rank_weights_en.json

# 3) 用 W_en 跑合成集（验证不回归）
go run ./run_kpi.go -dataset ./dataset/all.jsonl \
  -report kpi_baseline_en -weights ./weights/rank_weights_en.json

# 4) 对照默认
go run ./run_kpi.go -dataset ./benchmarks/dataset/all.jsonl -report benchmarks_default
```

### A.5 验收

- 公开基准 MRR ≥ 0.596（即 0.546 + 0.05），top-1 命中率提升 ≥ 5pp。
- 合成集 MRR ≥ 0.811（即 0.821 − 0.01）。
- `go vet ./...` + `go test ./...` 全绿（0.88）。

---

## 2. B 路 — temporal 样本补 `as_of` + `SecondVersion`

### B.1 背景（根因）

P7-D 报告 §2：
> **temporal / open-domain 桶 MRR 差距最大（-0.44 / -0.45）**：时间推理需 as_of 回溯历史版本，当前**顶层 `as_of` 与 `second_version` 全部为空**（0/166），排序时 as_of 过滤与双版本验证**完全没有被触发**，freshness 信号无法区分历史版本。

### B.1.1 ⚠️ 实测口径（R2 出发前实测，纠正 P7-D 报告的一处错误）

**P7-D 报告 §2 写「`recorded_at` 仅 550/650 有效（BEAM 无时间字段）」是错误口径**，实测（0.88 上 `python` 统计 `eval/benchmarks/dataset/all.jsonl`）如下：

| 字段 | LME (500) | LoCoMo (50) | BEAM (100) | 全量 (650) |
|---|---|---|---|---|
| `memory.recorded_at` 非空 | 500 (100%) | 50 (100%) | 100 (100%) | **650 (100%)** ✅ |
| `memory.occurred_at` 非空 | 500 (100%) | 50 (100%) | **0 (0%)** ❌ | 550 (84.6%) |
| **顶层 `as_of` 非空** | 0 | 0 | 0 | **0 (0%)** ❌ |
| **顶层 `second_version` 非空** | 0 | 0 | 0 | **0 (0%)** ❌ |

**修正后的根因**：`recorded_at` 实际 100% 有值（BEAM 由 `bench/helpers.go:newMemory` 填充），**BEAM 真正缺的是 `occurred_at`**；而真正阻塞 as_of 过滤的是**顶层 `as_of` 与 `second_version` 双字段全空**（`run_kpi.go:280` 只在 `item.AsOf != ""` 时设置 `rc.AsOf`，否则 as_of 过滤不生效；`SecondVersion` 为空则双版本验证不触发）。

- `kernel/backend_sqlite.go:104` 与 `backend_pg.go:250` 已支持 `q.AsOf` 过滤；`kernel/model.go:130-131` 已有 `ValidFrom/ValidUntil` date-only 字段 → **kernel 侧无需改动**，纯数据增强任务。
- 分桶统计：temporal = 166（LME 133 + LoCoMo 8 + BEAM 25），与 R1 一致。

### B.2 目标

- 让 LME 与 BEAM 的 temporal 样本**全部**具备非空 `AsOf` 与 `SecondVersion`（双版本），使 as_of 过滤能真实验证。
- temporal 桶 MRR 提升 ≥ 0.05（从 0.552 → ≥ 0.602）。
- LoCoMo temporal 样本（8 条）若已有时间字段则保持；若空则按同样规则补。

### B.3 交付

1. **`eval/benchmarks/bench/helpers.go`**：新增一个确定性辅助函数，用于从原始数据推断 temporal 样本的 as_of 与双版本。
   - **禁止**用 `time.Now()`；只从原始对话/问题里的日期字段推断。
   - 建议：给定一个问题及其期望召回的 memory（当前版本），构造 SecondVersion = 该 memory 在 as_of 之前的上一版本，其中 `ValidUntil` = as_of 前一日，`ValidFrom` = 上一版本日期；`OccurredAt` / `RecordedAt` 相应调整。
   - 具体规则：**在 longmemeval.go / beam.go / locomo.go 内根据各源字段实现，helpers.go 只提供公共工具**（如 `makeSecondVersion(m kernel.Memory, asOf time.Time) kernel.Memory`）。

2. **`eval/benchmarks/bench/longmemeval.go`**：
   - LME 原始 `question_date` 字段 → 作为 `as_of`。
   - 若 temporal 样本只有单一版本 memory，**构造一个 SecondVersion**（内容为同一 fact 的旧表述或旧值，`ValidUntil` < as_of）。
   - 具体构造规则**不调 LLM**，用原始对话里早于 `question_date` 的 turn 文本作为 SecondVersion 的 fact。

3. **`eval/benchmarks/bench/beam.go`**：
   - BEAM 的 `recorded_at` 已有（`newMemory` 填充），但 `occurred_at` 为空（0/100）→ **从样本内对话轮次顺序推断**一个确定性时间序列（起始 `2024-01-01T00:00:00Z`，逐轮 +24h，**不用 `time.Now()`**），用于构造 `occurred_at`、顶层 `as_of` 与 `SecondVersion`。
   - 若 raw 里能解析到时间戳，优先用 raw 时间；否则用顺序推断。
   - 若仍无法推断，则跳过该样本的 as_of 补全，并在报告里记录**覆盖率**。

4. **`eval/benchmarks/sample.go`**：确保 `toItems`（`sample.go:105`）正确把 `AsOf` 与 `SecondVersion` 透传到输出 `EvalItem`（`eval/run_kpi.go:49-56` 已定义 `AsOf` / `SecondVersion` 字段，需确认透传）。

5. **重跑 `eval/benchmarks/dataset/*.jsonl`**：`go run ./sample.go`（沿用 R1 流程）。

6. **报告**：`eval/reports/temporal_enhance_<ts>.md`，含 as_of 覆盖率（补全前/后）、temporal 桶 MRR 对比。

### B.4 命令（0.88 上执行）

```sh
cd /tmp/duan-light-merge/lightmem/eval/benchmarks
go run ./sample.go
go run ./verify_benchmarks.go
cd .. && go run ./run_kpi.go -dataset ./benchmarks/dataset/all.jsonl -report benchmarks_temporal
```

### B.5 验收

- `eval/benchmarks/dataset/all.jsonl` 中 temporal 桶样本 `AsOf` 非空比例 ≥ 90%，`SecondVersion` 非空比例 ≥ 90%。
- temporal MRR ≥ 0.602（提升 ≥ 0.05）。
- `verify_benchmarks.go` ALL PASS；`go vet` + `go test ./...` 全绿。

---

## 3. C 路 — fact 评测口径改「got 包含 expected」

### C.1 背景（根因）

P7-D 报告 §1 脚注：
> fact 文本 0% 是**数据形态差异，非检索能力**：公开基准 memory.fact 为 `question => answer`（LME/BEAM）或证据 turn 原文（LoCoMo），而 expected_fact 是纯答案，评测器按 `gotFact == expectedFact` 精确比较必然 0%。实际答案已被召回（见 MRR），仅拼接格式不同。

当前实现（`eval/run_kpi.go:343`）：
```go
factHit := item.ExpectedFact != "" && gotFact == item.ExpectedFact
```

`res.Fused[0].Fact` 取的是 top-1 记忆的完整 fact 文本（含 `question => answer` 前缀或 turn 前缀），与纯答案 `expected_fact` 精确相等自然 0%。

### C.2 目标

- 消除 0% 假阴性，让公开基准 fact 准确率 ≥ 50%（预期 70-90%）。
- 合成集 fact 准确率 **不得回归**（≥ 90%，当前 93.0%）。

### C.3 交付

1. **`eval/run_kpi.go`**：把 `factHit := item.ExpectedFact != "" && gotFact == item.ExpectedFact` 替换为对 `matchFact(gotFact, item.ExpectedFact)` 的调用。

2. **新增辅助函数**（放在 `eval/run_kpi.go` 内，纯文本处理，不调 LLM）：
   ```go
   // matchFact 判定 gotFact 是否包含 expectedFact（忽略大小写与空白）。
   // 处理公开基准 fact 形态差异：LME/BEAM 形如 "question => answer"，LoCoMo 形如 "speaker: turn"，
   // 而 expected_fact 是纯答案。策略：
   //   1) 归一化后精确相等 → true
   //   2) 归一化后 gotFact 包含 expectedFact → true
   //   3) 归一化后 expectedFact 包含 gotFact（gotFact 更短且被 expected 包含）→ true
   //   4) 都空 → false
   func matchFact(gotFact, expectedFact string) bool {
       if expectedFact == "" {
           return false
       }
       a := normalizeFact(gotFact)
       b := normalizeFact(expectedFact)
       if a == "" || b == "" {
           return false
       }
       if a == b {
           return true
       }
       return strings.Contains(a, b) || strings.Contains(b, a)
   }

   // normalizeFact 归一化：小写、去除首尾空白、压缩连续空白。
   func normalizeFact(s string) string {
       s = strings.ToLower(strings.TrimSpace(s))
       return strings.Join(strings.Fields(s), " ")
   }
   ```
   > ⚠️ 注意：`strings.Contains` 双向包含会让非常短的 expected（如数字/单词）容易被误命中。**验收时须检查假阳性**：若公开基准 fact 准确率 > 95%，说明判定过松，需收窄为「归一化后 gotFact 包含 expectedFact 且长度比 ≥ 0.3」之类约束。

3. **报告**：`eval/reports/fact_criteria_<ts>.md`，含修正前后 fact 准确率对比、假阳性抽查样本 10 条。

### C.4 命令（0.88 上执行）

```sh
cd /tmp/duan-light-merge/lightmem/eval
# 修正前（基线，用于对比）
go run ./run_kpi.go -dataset ./benchmarks/dataset/all.jsonl -report benchmarks_before_factfix
# 修正后（公开基准）
go run ./run_kpi.go -dataset ./benchmarks/dataset/all.jsonl -report benchmarks_after_factfix
# 修正后（合成集，验证不回归）
go run ./run_kpi.go -dataset ./dataset/all.jsonl -report kpi_baseline_after_factfix
```

### C.5 验收

- 公开基准 fact 准确率 ≥ 50%，且 ≤ 95%（避免假阳性过松）。
- 合成集 fact 准确率 ≥ 90%。
- `go vet` + `go test ./...` 全绿。

---

## 4. D 路 — multi-hop 分桶单独评估（+ 防御性加固）

### D.1 背景（**实测口径纠正 P7-D 报告**）

`eval/run_kpi.go:369-374` 的当前统计：
```go
if item.Bucket == "multi-hop" {
    multiSignalTotal++
    if hit {
        multiSignalHits++
    }
}
```

**⚠️ 实测（0.88 上 `python` 统计，纠正本任务书初稿的判断）**：

| 检查项 | 合成集 (`eval/dataset/all.jsonl`) | 公开基准 (`eval/benchmarks/dataset/all.jsonl`) |
|---|---|---|
| bucket == multi-hop 的条目数 | 150 | 165 (LME 133 + LoCoMo 12 + BEAM 20) |
| 其中 `has_pollution=true` | **0** | **0** |
| 其中 `has_injection=true` | **0** | **0** |
| 全量对抗样本（pollution/injection 任一为 true） | 40（bucket 独立为 pollution/injection，与 multi-hop 互斥） | **0** |

**结论**：当前 `multiSignal` 分母**实际上没有被对抗样本污染**——合成集 multi-hop 与 pollution/injection 是互斥 bucket，公开基准压根没有对抗样本。所以本任务书初稿说的"修复污染分母"**属于夸大**，D 路的**真正价值在结构性约束的单独评估**，`!isAdversarial` 仅是**防御性加固**（防止未来数据源把对抗样本标进 multi-hop 桶）。

### D.2 真正的根因：合成集 multi-hop MRR 0.500 的结构性约束

合成集 multi-hop 150 条按 R1 设计为「hop-1 / hop-2 分离」：`ExpectedMemoryID` = `[hop2ID, hop1ID]`（`eval/benchmarks/sample.go:105 toItems` 把首条作为 `Memory`、第二条挂 `Hop1Memory`）。而评测器 `eval/run_kpi.go:320` 对 top-1 命中的判定是**在 `ExpectedMemoryID` 列表里找命中位置**，`rank = j + 1`，**j=0 即 hop-2 命中得 rank=1，j=1 即 hop-1 命中得 rank=2**。

因此，只要 top-1 召回的是 hop-1（而非目标 hop-2），MRR 就只计 1/2 = 0.5。**这不是检索能力缺陷，而是评测口径与样本设计共同作用的结构性约束**——合成集 0.500 恰对应"一半样本 top-1 命中 hop-2、一半命中 hop-1"。

**D 路的两个子任务**：
1. **防御性加固**：`eval/run_kpi.go` 中 multiSignal 统计加 `!isAdversarial` 条件（口径一致性，**对当前结果无影响**，需在报告里说明）。
2. **单独评估（主任务）**：报告单独列 multi-hop 桶表格，区分合成集 / 公开基准，明确：
   - 合成集 multi-hop MRR 0.500 的结构性成因（hop-1/hop-2 分离 + rank 取 `ExpectedMemoryID` 位置）。
   - 公开基准 multi-hop MRR 0.558（`ExpectedMemoryID` 通常单元素），两源不可直接比较。

### D.3 目标

- 报告明确 multi-hop 桶双源 MRR **不可直接比较**的原因（样本结构差异）。
- 给出合成集 multi-hop 的**替代评估口径**建议（见 D.4 第 3 项），供后续 P8 决策。
- `!isAdversarial` 加固不改变当前任何数字（需验证）。

### D.4 交付

1. **`eval/run_kpi.go`**（防御性加固，**不改现有输出**）：
   ```go
   // 加固：multi-hop 桶统计只对非对抗样本累计（当前数据集该条件恒真，仅防御）
   if item.Bucket == "multi-hop" && !isAdversarial {
       multiSignalTotal++
       if hit {
           multiSignalHits++
       }
   }
   ```
   > 同时检查 temporal 桶统计（`eval/run_kpi.go:363-368`）是否需同样加固，**建议一并加**，口径一致。

2. **报告**：`eval/reports/multihop_bucket_<ts>.md`，含：
   - 合成集 multi-hop 桶 MRR（加固前后应**完全一致**，验证加固无副作用）
   - 公开基准 multi-hop 桶 MRR
   - 结构性约束分析：`ExpectedMemoryID` 布局差异 → 双源 MRR 不可直接比较
   - 防御性加固说明：当前数据集下该条件恒真，纯防御

3. **替代评估口径建议（只写建议，不实现）**：
   - **口径 A（推荐）**：multi-hop 只要求 `top-1 MemoryID ∈ ExpectedMemoryID` 即算 hit（等价于 `rank=1`），不区分 hop 顺序 → 可测"检索到目标事实集合"的能力。
   - **口径 B**：`hit@2`（top-2 内命中任一 expected），单独报告。
   - **口径 C**：保持现状但明确"合成集 MRR 0.500 结构性下限 = 0.5 × P(top1=hop2)"，与公开基准分开看。
   - **不实现**，仅写入报告供用户决策。

### D.5 命令（0.88 上执行）

```sh
cd /tmp/duan-light-merge/lightmem/eval
# 加固前（基线）
go run ./run_kpi.go -dataset ./dataset/all.jsonl -report kpi_baseline_mh_before
go run ./run_kpi.go -dataset ./benchmarks/dataset/all.jsonl -report benchmarks_mh_before
# 加固后（应完全一致）
go run ./run_kpi.go -dataset ./dataset/all.jsonl -report kpi_baseline_mh_after
go run ./run_kpi.go -dataset ./benchmarks/dataset/all.jsonl -report benchmarks_mh_after
```

### D.6 验收

- 合成集 multi-hop 桶统计分母 = 150，公开基准 = 165（加固前后一致）。
- 加固前后所有指标**完全一致**（diff 为空），证明防御性加固无副作用。
- 报告含结构性约束分析与三种替代口径建议。
- `go vet` + `go test ./...` 全绿。

---

## 5. 四路收口与交付

### 5.1 四路最终交付清单

| 路 | 源码 | 报告 | 数据集/权重 |
|---|---|---|---|
| A | `eval/tune_rank_weights.go`、`eval/run_kpi.go`（+`-weights`） | `eval/reports/tune_rank_weights_<ts>.md` | `eval/weights/rank_weights_en.json`（新增） |
| B | `eval/benchmarks/bench/helpers.go`、`longmemeval.go`、`beam.go`、`locomo.go`、`sample.go` | `eval/reports/temporal_enhance_<ts>.md` | `eval/benchmarks/dataset/*.jsonl`（重跑） |
| C | `eval/run_kpi.go`（+`matchFact`/`normalizeFact`） | `eval/reports/fact_criteria_<ts>.md` | — |
| D | `eval/run_kpi.go`（multiSignal/temporal 加 `!isAdversarial`，防御性） | `eval/reports/multihop_bucket_<ts>.md` | — |

### 5.2 收口验证（**合并后统一在 0.88 跑**）

1. `gofmt -l $(git ls-files '*.go')` 无输出
2. `go vet ./...` 全绿
3. `go test ./...` 全绿
4. 四项验收指标各自达标（见 §A.5 / §B.5 / §C.5 / §D.5）
5. 最终整合报告：`eval/reports/p7d_r2_summary.md`，把四路结果合并为一张"优化前后双源 MRR/fact 表"

### 5.3 提交与推送

- **禁止自行 push**。执行者把所有改动 `git add` 并提交（不 push），然后向用户汇报 commit hash 与四路指标。
- 用户示意后才 `git push origin main`（gitea 192.168.1.5:3000/skywalk/lightmem.git）。

### 5.4 风险与降级

| 风险 | 影响 | 应对 |
|---|---|---|
| A 路权重搜索不收敛 | 公开基准 MRR 不升 | 接受局部最优，记录搜索轨迹；若 MRR 提升 < 0.05 则报告"未达目标"而非造假 |
| B 路 BEAM 无 `occurred_at`（0/100） | 时间信号无法用于 freshness/as_of | 从对话轮次顺序确定性推断时间（不用 `time.Now()`）；仍不行则报告 as_of 覆盖率 < 90% 并降级 |
| C 路 fact 判定过松导致假阳性 > 5% | 准确率虚高 | 收窄判定为「包含且长度比 ≥ 0.3」 |
| D 路 multi-hop 结构约束无法突破 | MRR 仍 0.500 | 单独报告说明原因，接受该值，不强行调优 |

---

## 6. 附：四路执行者快速对照表（分发用）

| 路 | 一句话任务 | 主要文件 | 完成判定 |
|---|---|---|---|
| **A** | 英文权重搜索 + `run_kpi.go` 加 `-weights` | `kernel/rank_fusion.go`（只读参考）、`eval/run_kpi.go`、`eval/tune_rank_weights.go` | 公开 MRR ≥ 0.596 且合成 ≥ 0.811 |
| **B** | temporal 样本补 as_of + SecondVersion | `eval/benchmarks/bench/{helpers,longmemeval,beam,locomo}.go`、`sample.go` | as_of/Version 覆盖率 ≥ 90%，temporal MRR ≥ 0.602 |
| **C** | fact 口径改包含判定 | `eval/run_kpi.go` | 公开 fact ≥ 50%，合成 ≥ 90%，无假阳性 > 5% |
| **D** | multi-hop 分母加 `!isAdversarial` + 单独评估 | `eval/run_kpi.go` | 报告含双源 multi-hop 表 + 结构约束说明 |

**通用要求**：
- 全程不调 LLM。
- 全程 seed=42 确定性。
- 所有验证在 192.168.0.88 上完成。
- 完成后 `git commit`（不 push），向用户汇报 commit hash + 指标。
- 报告统一放 `eval/reports/`，命名 `<路名>_<ts>.md`。

---

**（任务书结束）**