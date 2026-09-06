#!/usr/bin/env bash
# 一键演示：注入真实 5xx → 提工单 → 等处理 → 一键扩容恢复 → 验证回落
# 用法: ./scripts/demo-5xx.sh   （需服务已在本机 :8090 运行且 kubectl 可连 makejob 集群）
set -e
BASE=${BASE:-http://localhost:8080}
API=${API:-http://localhost:8090}

echo "==> 1/5 注入故障：缩容 makejob-question 到 0 副本"
kubectl scale deployment makejob-question --replicas=0

echo "==> 2/5 持续制造真实流量（约 90s）"
( for i in $(seq 1 180); do curl -s -o /dev/null -m 3 "$BASE/api/v1/questions" || true; sleep 0.5; done ) &
TRAFFIC_PID=$!

echo "==> 等待 5xx 指标积累（40s）后提交工单"
sleep 40
TID=$(printf '{"title":"接口 5xx 突增","content":"gateway 出现大量 5xx，涉及 question 服务，请排查根因并给出处置建议。","reporter":"demo"}' \
  | curl -s -m 10 -X POST "$API/api/v1/tickets" -H "Content-Type: application/json" --data-binary @- \
  | python -c "import sys,json; print(json.load(sys.stdin)['id'])")
echo "工单号: $TID   （跟踪: $API/tickets/$TID）"

echo "==> 3/5 等待多 Agent 处理至终态"
for i in $(seq 1 40); do
  ST=$(curl -s -m 5 "$API/api/v1/tickets/$TID" | python -c "import sys,json; print(json.load(sys.stdin).get('status'))" 2>/dev/null || echo "?")
  echo "  [$i] $ST"
  case "$ST" in resolved|escalated_human|failed) break;; esac
  sleep 10
done

echo "==> 4/5 一键处置：扩容回 1 副本（人工确认语义）"
curl -s -m 30 -X POST "$API/api/v1/tickets/$TID/actions" -H "Content-Type: application/json" \
  -d '{"tool":"k8s.scale","input":{"deployment":"makejob-question","replicas":1}}' | head -c 200; echo
kill $TRAFFIC_PID 2>/dev/null || true

echo "==> 5/5 等待服务恢复"
sleep 30
kubectl scale deployment makejob-question --replicas=1 2>/dev/null || true
CODE=$(curl -s -o /dev/null -w "%{http_code}" -m 5 "$BASE/api/v1/questions" || echo "?")
echo "恢复后接口状态: $CODE"
echo "演示完成：工单 $TID ｜ 审计: $API/api/v1/tickets/$TID/audit"
