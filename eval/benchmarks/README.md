# R1 外部基准数据集（LongMemEval / LoCoMo / BEAM 子集）

> 任务书：`lightmem/.dumate/inbox/_R1_外部基准数据集获取与制作任务书.md`
> 红线：**只增不改** kernel Schema；不调 LLM 生成/改写 query 与答案；固定 seed=42 确定性采样；所有编译/测试在 192.168.0.88 验证。

## 1. 来源与口径

| 基准 | 官方来源 | 原始规模 | 本轮子集 | 字段映射要点 |
|---|---|---|---|---|
| **LongMemEval** | `xiaowu0162/LongMemEval`（oracle 变体，HF） | 500 条 query | **500**（全量主变体） | question→query；answer→expected_fact；question_type→分桶 |
| **LoCoMo** | `snap-research/locomo`（locomo10） | 10 组对话 / 1986 QA | **50**（分层抽样，剔除 category=5 adversarial） | evidence dia_id 指向的 turn 原文→memory.fact；answer→expected_fact |
| **BEAM** | `mem0ai/memory-benchmarks`（BEAM 1M，700 题） | 700 题（10 类） | **100**（剔除 abstention 不可答题，分层抽样） | question→query；ground_truth_answer→expected_fact；question_type→分桶 |

**合计：650 条 query**（≥650 达标），与既有 `eval/dataset/`（570 条合成样本）形成「合成 + 公开」双源评测。

> 原始数据为第三方公开数据。`raw/` 下的三份 JSON 是对官方原始数据的**机械裁剪**（仅删去巨型字段，不改写任何文字）；`sample.go` 仅做字段映射、分桶标注、固定 seed 采样与去重，**全程无 LLM 参与**。

## 2. 分桶映射（四类能力）

| 分桶 | LongMemEval | LoCoMo category | BEAM question_type | 本轮子集条数 |
|---|---|---|---|---|
| single-hop | single-session-preference/assistant/user | 1 | preference_following / information_extraction / instruction_following | 264 |
| multi-hop | multi-session | 3 | multi_session_reasoning / contradiction_resolution | 165 |
| temporal | temporal-reasoning / knowledge_update | 2 | temporal_reasoning / event_ordering / knowledge_update | 166 |
| open-domain | — | 4 | summarization | 55 |

每条记录字段与 `eval/run_kpi.go::EvalItem` 完全对齐，可被既有评测链路直接消费。

## 3. 文件

```
eval/benchmarks/
├── sample.go            #go:build ignore  seed=42 采样 + 落盘（驱动 ./bench 适配器）
├── verify_benchmarks.go #go:build ignore  4 项硬校验
├── README.md
├── bench/              # B1 正式适配器子包（可被 go build/vet 直接编译）
│   ├── adapter.go      #   Adapter 接口 + RawConversation/RawTurn/RawQuestion 类型
│   ├── longmemeval.go  #   LongMemEvalAdapter
│   ├── locomo.go       #   LoCoMoAdapter
│   ├── beam.go         #   BeamAdapter
│   └── helpers.go      #   newMemory / SanitizeID / RawToString / MemKey
├── raw/                 # 机械裁剪后的官方原始输入（不参与评测，仅复现用）
│   ├── longmemeval.json
│   ├── locomo.json
│   └── beam.json
└── dataset/
    ├── longmemeval.jsonl   # 500
    ├── locomo.jsonl        # 50
    ├── beam.jsonl          # 100
    └── all.jsonl           # 650
```

## 4. 复现（在 0.88 上，lightmem/eval/benchmarks/ 下）

```sh
gofmt -l .                     # 空输出 = 格式合规
go build ./bench/              # B1 适配器子包编译
go vet ./bench/                # B1 适配器 vet

go run ./sample.go             # 从 raw/ 重新生成四份 JSONL（seed=42 确定性）
go run ./verify_benchmarks.go  # 4 项硬校验 + memory_id 复用提示（ALL PASS）
```

`sample.go` / `verify_benchmarks.go` 均为 `//go:build ignore`，各自单独 `go run`。

## 5. 下游 P7-D 消费

```sh
cd /tmp/duan-light-merge/lightmem/eval
go run ./run_kpi.go -dataset ./benchmarks/dataset/all.jsonl -report benchmarks_20261012.json
```

预期与既有合成 KPI 93.0% 对比，形成「合成 + 公开」双源结论。

## 6. 已知边界

- `memory_kind` 当前全为 `fact`（`bench/helpers.go::newMemory` 硬编码）；任务书 §3.3 要求的按内容类型推断（preference / procedure / event）尚未实现，属 B1 适配器职责。
- LoCoMo 多跳题共享同一证据 turn 时，eval 条目 id 会追加 ` [dup#n]` 后缀以保持唯一（不改语义、不改 memory_id）。
- BEAM 源数据无时间字段，`recorded_at`/`occurred_at` 留零值。
