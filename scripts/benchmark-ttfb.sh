#!/bin/bash
# TTFB (Time To First Byte) 压测脚本

set -e

PORT=${PORT:-8080}
HOST=${HOST:-localhost}
CONCURRENCY=${CONCURRENCY:-100}
REQUESTS=${REQUESTS:-1000}
AUTH_KEY=${AUTH_KEY:-}

echo "=== TTFB 压测开始 ==="
echo "目标: http://${HOST}:${PORT}/v1/chat/completions"
echo "并发数: ${CONCURRENCY}"
echo "总请求数: ${REQUESTS}"
echo ""

# 创建测试请求体
cat > /tmp/bench-request.json << 'EOF'
{
  "model": "gpt-4o-mini",
  "messages": [
    {"role": "user", "content": "Say hello"}
  ],
  "max_tokens": 50,
  "stream": false
}
EOF

# 创建 curl 格式化输出模板
cat > /tmp/curl-format.txt << 'EOF'
    time_namelookup:  %{time_namelookup}s\n
       time_connect:  %{time_connect}s\n
    time_appconnect:  %{time_appconnect}s\n
   time_pretransfer:  %{time_pretransfer}s\n
      time_redirect:  %{time_redirect}s\n
 time_starttransfer:  %{time_starttransfer}s (TTFB)\n
                    ----------\n
         time_total:  %{time_total}s\n
EOF

echo "1. 单次请求延迟测试"
echo "===================="

if [ -n "$AUTH_KEY" ]; then
  AUTH_HEADER="-H \"Authorization: Bearer $AUTH_KEY\""
else
  AUTH_HEADER=""
fi

for i in {1..3}; do
  echo "第 $i 次:"
  curl -w "@/tmp/curl-format.txt" -o /dev/null -s \
    -H "Content-Type: application/json" \
    ${AUTH_HEADER:+-H "$AUTH_HEADER"} \
    -d @/tmp/bench-request.json \
    "http://${HOST}:${PORT}/v1/chat/completions" || true
  echo ""
done

echo ""
echo "2. 并发压测 (Apache Bench)"
echo "=========================="

AB_CMD="ab -n ${REQUESTS} -c ${CONCURRENCY} -T application/json -p /tmp/bench-request.json"
if [ -n "$AUTH_KEY" ]; then
  AB_CMD="$AB_CMD -H \"Authorization: Bearer $AUTH_KEY\""
fi
AB_CMD="$AB_CMD http://${HOST}:${PORT}/v1/chat/completions"

eval $AB_CMD 2>&1 | tee /tmp/bench-result.txt

echo ""
echo "3. 关键指标摘要"
echo "==============="
grep -E "Time per request|Requests per second|Transfer rate|Percentage of the requests|failed" /tmp/bench-result.txt || true

echo ""
echo "=== 压测完成 ==="
echo ""
echo "完整结果已保存至: /tmp/bench-result.txt"
echo "TTFB < 1s 为优秀"
echo "TTFB < 2s 为良好"
echo "TTFB > 5s 需要优化"
