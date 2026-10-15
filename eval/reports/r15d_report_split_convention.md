# R15-D4：报告体积拆分约定（Report Volume Split Convention）

## 背景

R14 北极星明细单份 ~1.05 万行（~832KB），随轮次累积会显著增长仓库体积。
本轮不回改 R13 / R14 已入库的历史报告，只立约定 + 在下一次北极星产出时执行。

## 约定

### 1. 文件拆分规则

| 文件类型 | 命名 | 入库 | 说明 |
|---------|------|------|------|
| 摘要 | `r15_final_*.json` | ✅ 入库 | 含 M1/M2 核心指标、门禁矩阵、关键统计 |
| 明细 | `r15_final_*.detail.jsonl` | ❌ 不入库 | 逐条记忆命中、逐边统计、完整向量数据 |

### 2. 摘要字段（gate.sh 与交付报告依赖）

摘要 JSON 必须包含以下字段：

```json
{
  "report": "r15_final_holdout_20261015-xxxxxx",
  "generated_at": "2026-10-15T00:00:00Z",
  "m1": {
    "coverage": 1.0,
    "hits": 800,
    "hit_at_1_rate": 1.0,
    "mean_rr": 1.0,
    "id_hit_rate": 1.0,
    "buckets": {
      "single-hop": {"coverage": 1.0, "n": 149},
      "multi-hop": {"coverage": 1.0, "n": 262},
      "temporal": {"coverage": 1.0, "n": 165},
      "open-domain": {"coverage": 1.0, "n": 224}
    }
  },
  "m2": {
    "m2_control_share": 0.24976978275600448,
    "control": 43126,
    "data": 129537,
    "total": 172663,
    "data_plane_compression": 200295,
    "reduction_rate": 0.3532689283307122
  },
  "fp_audit": {
    "vetoed": 0
  },
  "gate": {
    "pass": 12,
    "fail": 0,
    "skip": 0
  }
}
```

### 3. 明细文件格式

明细文件为 JSONL（每行一条 JSON），加入 `.gitignore`：

```jsonl
{"memory_id": "mem_001", "fact": "...", "hit": true, "rank": 1, "bucket": "single-hop"}
{"memory_id": "mem_002", "fact": "...", "hit": true, "rank": 2, "bucket": "multi-hop"}
...
```

### 4. 执行时机

- 本轮（R15）：只立约定，不回改历史报告
- 下一轮（R16）：在北极星产出时执行拆分，同时更新 `gate.sh` 与交付报告模板

### 5. .gitignore 规则

```
# 北极星明细（体积大，不入库）
*.detail.jsonl
```

## 红线遵守

- 不回改 R13 / R14 已入库的历史报告
- `gate.sh` 与交付报告只依赖摘要字段，不依赖明细
- 明细文件本地保留，用于问题排查与审计，不进入版本控制
