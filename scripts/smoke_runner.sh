#!/bin/sh
cd /tmp/duan-light-merge/lightmem
export LIMS_PG_HOST=127.0.0.1 LIMS_PG_PORT=5432 LIMS_PG_USER=postgres LIMS_PG_DB=lims
export LIMS_CONSOLIDATE_INTERVAL=10s
./kernel/lightmem-kernel -profile profiles/lims-family.profile.yaml -listen :18080 2>&1 &
SERVER_PID=$!
sleep 3

echo "[smoke] /health"
curl -sf http://127.0.0.1:18080/health | grep -q '"status":"ok"' && echo "  OK" || { echo "  FAIL"; kill $SERVER_PID; exit 1; }

echo "[smoke] POST /mem/router"
curl -sf -X POST http://127.0.0.1:18080/mem/router -H 'Content-Type: application/json' -d @/tmp/req.json | grep -q '"intent"' && echo "  OK" || { echo "  FAIL"; kill $SERVER_PID; exit 1; }

echo "[smoke] POST /tools/memory/save"
# 用唯一 idempotency_key 避免幂等冲突
SAVE_ID="smoke-$(date +%s)"
curl -sf -X POST http://127.0.0.1:18080/tools/memory/save -H 'Content-Type: application/json' -d "{\"memory_id\":\"$SAVE_ID\",\"idempotency_key\":\"$SAVE_ID\",\"tenant_id\":\"family-1\",\"user_id\":\"alice\",\"memory_kind\":\"fact\",\"fact\":\"客户A偏好周二开会\",\"confidence\":0.9,\"visibility\":\"tenant\",\"permission\":{\"owner\":\"alice\"},\"source\":\"chat\"}" | grep -q '"status":"created"' && echo "  OK" || { echo "  FAIL"; kill $SERVER_PID; exit 1; }

echo "[smoke] POST /tools/memory/recall"
# 注意：recall 返回 mode 取决于融合结果（unique/fused/low_confidence/conflict）
curl -sf -X POST http://127.0.0.1:18080/tools/memory/recall -H 'Content-Type: application/json' -d '{"text":"客户A 偏好","tenant_id":"family-1","user_id":"alice"}' | grep -q '"mode"' && echo "  OK" || { echo "  FAIL"; kill $SERVER_PID; exit 1; }

echo "[smoke] POST /tools/memory/feedback"
curl -sf -X POST http://127.0.0.1:18080/tools/memory/feedback -H 'Content-Type: application/json' -d "{\"memory_id\":\"$SAVE_ID\",\"status\":\"adopted\",\"operator\":\"alice\",\"viewer\":\"alice\",\"user_id\":\"alice\",\"tenant_id\":\"family-1\"}" | grep -q '"status":"recorded"' && echo "  OK" || { echo "  FAIL"; kill $SERVER_PID; exit 1; }

echo "[smoke] POST /tools/memory/update"
curl -sf -X POST http://127.0.0.1:18080/tools/memory/update -H 'Content-Type: application/json' -d "{\"memory_id\":\"$SAVE_ID\",\"fact\":\"客户A偏好周三开会\",\"viewer\":\"alice\",\"user_id\":\"alice\",\"tenant_id\":\"family-1\"}" | grep -q '"status":"updated"' && echo "  OK" || { echo "  FAIL"; kill $SERVER_PID; exit 1; }

echo "[smoke] POST /tools/memory/forget"
curl -sf -X POST http://127.0.0.1:18080/tools/memory/forget -H 'Content-Type: application/json' -d "{\"memory_id\":\"$SAVE_ID\",\"reason\":\"测试删除\",\"viewer\":\"alice\",\"user_id\":\"alice\",\"tenant_id\":\"family-1\"}" | grep -q '"status":"forgotten"' && echo "  OK" || { echo "  FAIL"; kill $SERVER_PID; exit 1; }

echo "[smoke] GET /metrics/layers"
curl -sf http://127.0.0.1:18080/metrics/layers?tenant_id=family-1 | grep -q '"L0"' && echo "  OK" || { echo "  FAIL"; kill $SERVER_PID; exit 1; }

echo "[smoke] POST /lifecycle/run"
curl -sf -X POST http://127.0.0.1:18080/lifecycle/run -H 'Content-Type: application/json' -d '{"tenant_id":"family-1","max":100}' | grep -q '"scanned"' && echo "  OK" || { echo "  FAIL"; kill $SERVER_PID; exit 1; }

echo ""
echo "[smoke] ALL PASS"
kill $SERVER_PID
