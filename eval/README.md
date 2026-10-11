# 评测集（P2.5）

## 分桶定义

| 桶 | 条数 | 定义 | 与 §9 KPI 映射 |
|---|---|---|---|
| single-hop | 150 | 单条记忆即可回答 | 检索准确率（分桶） |
| multi-hop | 150 | 需 2+ 条记忆链式推理 | 多信号融合正确率 |
| open-domain | 100 | 无明确 schema，需开放推理 | 检索准确率（分桶） |
| temporal | 100 | 双时态变更，需判断生效区间 | 时序桶查询正确率 |

## 附加样本

| 类型 | 条数 | 说明 |
|---|---|---|
| 双时态变更 | 30 | valid_from/valid_until 变更，需 as_of 回溯 |
| 污染 | 20 | 低置信度错误记忆，应被降权 |
| 投毒 | 20 | prompt_injection_suspect，应被归档 |

## 加载方法

```go
import "github.com/lightmem/kernel"

// 逐条写入 kernel（走 WritePolicy）。
for _, item := range items {
    _, _ = srv.SaveMemory(ctx, item.Memory, item.User)
}

// 逐条查询（走 A1+A2）。
res, _ := srv.RecallAndFuse(ctx, kernel.RouterContext{
    Text: item.Query, TenantID: item.Tenant, UserID: item.User, AsOf: &item.AsOf,
})
// 判定：res.Fused[0].MemoryID ∈ item.ExpectedMemoryID
```

## 与 §9 KPI 映射

- **检索准确率（总体）**：top-1 命中 `expected_memory_ids` 的比例。
- **分桶准确率**：按 bucket 分别计算。
- **时序桶准确率**：`as_of` 回溯返回正确历史版本。
- **多信号融合正确率**：冲突样本上人工评估融合结果（本轮先自动跑，标注"需人工复核"）。
- **记忆污染率**：被纠正记忆占总召回次数比。

## 生成

```bash
cd eval
go run ./eval_generate.go   # 生成 dataset/*.jsonl
go run ./run_kpi.go         # 跑 KPI，输出 reports/kpi_baseline_*.json
```

## 红线

- 不调 LLM 生成答案（避免污染指标）。
- 默认权重为占位值，待 A8 KPI 出数后按评测结论校准。
