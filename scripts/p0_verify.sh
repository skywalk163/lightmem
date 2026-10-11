#!/usr/bin/env bash
# P0 修复验证脚本（0.88 实测）
# 步骤：gofmt/vet/build/test + 重生成评测集 + 连跑 3 次 KPI 验证可复现
set -euo pipefail

REPO="/tmp/duan-light-merge/lightmem"
cd "$REPO"

echo "=========================================="
echo "Step 1: gofmt 检查"
echo "=========================================="
OUT=$(gofmt -d kernel/ eval/ 2>&1) || true
if [ -n "$OUT" ]; then
  echo "FAIL: gofmt found formatting issues:"
  echo "$OUT"
  exit 1
fi
echo "PASS: gofmt clean"

echo ""
echo "=========================================="
echo "Step 2: go vet 检查"
echo "=========================================="
go vet github.com/lightmem/kernel/...
echo "PASS: go vet clean"

echo ""
echo "=========================================="
echo "Step 3: go build 编译"
echo "=========================================="
go build github.com/lightmem/kernel
echo "PASS: go build success"

echo ""
echo "=========================================="
echo "Step 4: go test 测试"
echo "=========================================="
go test github.com/lightmem/kernel/... -v -count=1
echo "PASS: go test success"

echo ""
echo "=========================================="
echo "Step 5: 重生成评测集"
echo "=========================================="
cd eval
python3 -c "
import sys
sys.path.insert(0, '.')
from eval_generate import *
import json

r = seed()
all_items = []
all_items.extend(genSingleHop(r, 150))
all_items.extend(genMultiHop(r, 150))
all_items.extend(genOpenDomain(r, 100))
all_items.extend(genTemporal(r, 100))
all_items.extend(genTemporalChange(r, 30))
all_items.extend(genPollution(r, 20))
all_items.extend(genInjection(r, 20))

# 写 all.jsonl
with open('dataset/all.jsonl', 'w') as f:
    for item in all_items:
        f.write(json.dumps(item, ensure_ascii=False) + '\n')

# 按 bucket 分桶
buckets = {'single-hop': [], 'multi-hop': [], 'open-domain': [], 'temporal': [], 'pollution': [], 'injection': []}
for item in all_items:
    buckets[item['bucket']].append(item)

for bucket, items in buckets.items():
    with open(f'dataset/{bucket}.jsonl', 'w') as f:
        for item in items:
            f.write(json.dumps(item, ensure_ascii=False) + '\n')
    print(f'  {bucket}: {len(items)} items')

print(f'  total: {len(all_items)} items')

# 验证 query 唯一性
queries = {}
for item in all_items:
    q = item['query']
    if q in queries:
        print(f'  DUPLICATE QUERY: {q}')
        print(f'    existing: {queries[q]}')
        print(f'    new:      {item[\"id\"]}')
    queries[q] = item['id']

print(f'  unique queries: {len(queries)} / {len(all_items)}')
if len(queries) != len(all_items):
    print('  FAIL: duplicate queries found')
    sys.exit(1)
print('  PASS: all queries unique')
"
cd ..
echo "PASS: dataset regenerated"

echo ""
echo "=========================================="
echo "Step 6: 连跑 3 次 KPI 验证可复现"
echo "=========================================="
RESULTS=()
for i in 1 2 3; do
  echo "--- Run $i ---"
  cd eval
  python3 run_kpi.py 2>&1 | tail -20 > /tmp/kpi_run$i.txt
  cd ..
  # 提取 overall accuracy
  ACC=$(grep "检索准确率（总体，按 memory_id）" /tmp/kpi_run$i.txt | grep -oP '[0-9]+\.[0-9]+')
  echo "  Run $i: overall accuracy = $ACC%"
  RESULTS+=("$ACC")
done

echo ""
echo "=========================================="
echo "Step 7: 可复现性验证"
echo "=========================================="
UNIQUE=$(printf '%s\n' "${RESULTS[@]}" | sort -u | wc -l)
echo "  Run 1: ${RESULTS[0]}%"
echo "  Run 2: ${RESULTS[1]}%"
echo "  Run 3: ${RESULTS[2]}%"
echo "  Unique values: $UNIQUE"

if [ "$UNIQUE" -eq 1 ]; then
  echo "  PASS: 3 runs identical, reproducible"
else
  echo "  FAIL: 3 runs differ, not reproducible"
  exit 1
fi

echo ""
echo "=========================================="
echo "ALL PASS"
echo "=========================================="
