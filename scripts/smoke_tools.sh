#!/usr/bin/env bash
# R138-B 端到端冒烟测试（任务书 §4.4 / §10）。
# 在 0.88 上运行：起 kernel + 走 A1/A2/A5 端点。
# 用法：bash scripts/smoke_tools.sh
set -euo pipefail

cd "$(dirname "$0")/.."
KERNEL="kernel/lightmem-kernel"
PORT=18080
BASE="http://127.0.0.1:${PORT}"

# 1) 编译（0.88 上）
echo "[smoke] building..."
cd kernel
go build -o "$KERNEL" ./cmd/lightmem-kernel
cd ..

# 2) 起服务（后台）
export LIMS_PG_HOST=127.0.0.1 LIMS_PG_PORT=5432 LIMS_PG_USER=postgres LIMS_PG_DB=lims
export LIMS_CONSOLIDATE_INTERVAL=10s
./"$KERNEL" -profile profiles/lims-family.profile.yaml -listen ":${PORT}" &
PID=$!
sleep 3

cleanup() { kill $PID 2>/dev/null || true; }
trap cleanup EXIT

# 3) 健康检查
echo "[smoke] /health"
curl -sf "${BASE}/health" | grep -q '"status":"ok"' && echo "  OK" || { echo "  FAIL"; exit 1; }

# 4) A1 /mem/router 意图识别 + 跨层召回
echo "[smoke] POST /mem/router"
curl -sf -X POST "${BASE}/mem/router" \
  -H 'Content-Type: application/json' \
  -d '{"text":"客户A偏好是什么","tenant_id":"family-1","user_id":"alice"}' | grep -q '"intent":"query_knowledge"' && echo "  OK" || { echo "  FAIL"; exit 1; }

# 5) A5 /tools/memory/save 写一条记忆
echo "[smoke] POST /tools/memory/save"
curl -sf -X POST "${BASE}/tools/memory/save" \
  -H 'Content-Type: application/json' \
  -d '{"memory_id":"smoke-1","idempotency_key":"smoke-k1","tenant_id":"family-1","user_id":"alice","memory_kind":"fact","fact":"客户A偏好周二开会","confidence":0.9,"visibility":"tenant","permission":{"owner":"alice"},"source":"chat"}' | grep -q '"status":"created"' && echo "  OK" || { echo "  FAIL"; exit 1; }

# 6) A5 /tools/memory/recall 跨层召回 + 融合
echo "[smoke] POST /tools/memory/recall"
curl -sf -X POST "${BASE}/tools/memory/recall" \
  -H 'Content-Type: application/json' \
  -d '{"text":"客户A 偏好","tenant_id":"family-1","user_id":"alice"}' | grep -q '"mode":"unique"' && echo "  OK" || { echo "  FAIL"; exit 1; }

# 7) A5 /tools/memory/feedback
echo "[smoke] POST /tools/memory/feedback"
curl -sf -X POST "${BASE}/tools/memory/feedback" \
  -H 'Content-Type: application/json' \
  -d '{"memory_id":"smoke-1","status":"adopted","operator":"alice","viewer":"alice","user_id":"alice","tenant_id":"family-1"}' | grep -q '"status":"recorded"' && echo "  OK" || { echo "  FAIL"; exit 1; }

# 8) A5 /tools/memory/update
echo "[smoke] POST /tools/memory/update"
curl -sf -X POST "${BASE}/tools/memory/update" \
  -H 'Content-Type: application/json' \
  -d '{"memory_id":"smoke-1","fact":"客户A偏好周三开会","viewer":"alice","user_id":"alice","tenant_id":"family-1"}' | grep -q '"status":"updated"' && echo "  OK" || { echo "  FAIL"; exit 1; }

# 9) A5 /tools/memory/forget
echo "[smoke] POST /tools/memory/forget"
curl -sf -X POST "${BASE}/tools/memory/forget" \
  -H 'Content-Type: application/json' \
  -d '{"memory_id":"smoke-1","reason":"测试删除","viewer":"alice","user_id":"alice","tenant_id":"family-1"}' | grep -q '"status":"forgotten"' && echo "  OK" || { echo "  FAIL"; exit 1; }

# 10) /metrics/layers 四层计数
echo "[smoke] GET /metrics/layers"
curl -sf "${BASE}/metrics/layers?tenant_id=family-1" | grep -q '"L0"' && echo "  OK" || { echo "  FAIL"; exit 1; }

# 11) /lifecycle/run 生命周期扫描
echo "[smoke] POST /lifecycle/run"
curl -sf -X POST "${BASE}/lifecycle/run" \
  -H 'Content-Type: application/json' \
  -d '{"tenant_id":"family-1","max":100}' | grep -q '"scanned"' && echo "  OK" || { echo "  FAIL"; exit 1; }

echo ""
echo "[smoke] ALL PASS"
