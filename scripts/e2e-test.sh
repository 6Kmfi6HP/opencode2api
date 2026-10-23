#!/bin/bash
# E2E 测试脚本 - 并发优化验证

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

# 配置
MOCK_PORT=19998
GATEWAY_PORT=19999
CONCURRENCY=${CONCURRENCY:-100}
REQUESTS=${REQUESTS:-500}
MOCK_MIN_DELAY=${MOCK_MIN_DELAY:-50}
MOCK_MAX_DELAY=${MOCK_MAX_DELAY:-200}
MOCK_ERROR_RATE=${MOCK_ERROR_RATE:-0.01}
MOCK_SLOW_RATE=${MOCK_SLOW_RATE:-0.05}
MOCK_SLOW_DELAY=${MOCK_SLOW_DELAY:-3000}

# 颜色
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

cleanup() {
    log_info "清理进程..."
    [ -n "$MOCK_PID" ] && kill $MOCK_PID 2>/dev/null || true
    [ -n "$GATEWAY_PID" ] && kill $GATEWAY_PID 2>/dev/null || true
    rm -f /tmp/e2e-*.log /tmp/e2e-*.json
}

trap cleanup EXIT

echo "════════════════════════════════════════════════════════════════"
echo "            E2E 测试 - 并发优化验证"
echo "════════════════════════════════════════════════════════════════"
echo ""

# 1. 编译 mock 服务器
log_info "1. 编译 mock 服务器..."
cd "$SCRIPT_DIR"
go build -o /tmp/mock-upstream mock-upstream.go
if [ $? -ne 0 ]; then
    log_error "mock 服务器编译失败"
    exit 1
fi
log_info "✓ mock 服务器编译成功"
echo ""

# 2. 编译网关
log_info "2. 编译网关..."
cd "$PROJECT_DIR"
make build
if [ $? -ne 0 ]; then
    log_error "网关编译失败"
    exit 1
fi
log_info "✓ 网关编译成功"
echo ""

# 3. 启动 mock 上游
log_info "3. 启动 mock 上游服务器..."
/tmp/mock-upstream \
    -port=$MOCK_PORT \
    -min-delay=$MOCK_MIN_DELAY \
    -max-delay=$MOCK_MAX_DELAY \
    -error-rate=$MOCK_ERROR_RATE \
    -slow-rate=$MOCK_SLOW_RATE \
    -slow-delay=$MOCK_SLOW_DELAY \
    > /tmp/e2e-mock.log 2>&1 &
MOCK_PID=$!

sleep 2
if ! kill -0 $MOCK_PID 2>/dev/null; then
    log_error "mock 服务器启动失败"
    cat /tmp/e2e-mock.log
    exit 1
fi
log_info "✓ mock 服务器已启动 (PID: $MOCK_PID, Port: $MOCK_PORT)"
echo ""

# 4. 创建测试配置
log_info "4. 创建测试配置..."
cat > /tmp/e2e-config.json << EOF
{
  "port": "$GATEWAY_PORT",
  "log_file": "/tmp/e2e-gateway.log",
  "log_level": "info",
  "stats_file": "/tmp/e2e-stats.json",
  "upstream_base_urls": ["http://localhost:$MOCK_PORT"],
  "models": [
    {
      "id": "gpt-4o-mini",
      "upstream_id": "gpt-4o-mini"
    }
  ]
}
EOF
log_info "✓ 配置文件已创建"
echo ""

# 5. 启动网关
log_info "5. 启动网关..."
cd "$PROJECT_DIR"
./bin/opencode2api -config /tmp/e2e-config.json > /tmp/e2e-gateway-stdout.log 2>&1 &
GATEWAY_PID=$!

sleep 3
if ! kill -0 $GATEWAY_PID 2>/dev/null; then
    log_error "网关启动失败"
    cat /tmp/e2e-gateway-stdout.log
    exit 1
fi

# 健康检查
log_info "等待网关就绪..."
for i in {1..10}; do
    if curl -s http://localhost:$GATEWAY_PORT/health > /dev/null 2>&1; then
        log_info "✓ 网关已就绪 (PID: $GATEWAY_PID, Port: $GATEWAY_PORT)"
        break
    fi
    if [ $i -eq 10 ]; then
        log_error "网关健康检查失败"
        exit 1
    fi
    sleep 1
done
echo ""

# 6. 预热请求
log_info "6. 发送预热请求..."
for i in {1..5}; do
    curl -s -X POST http://localhost:$GATEWAY_PORT/v1/chat/completions \
        -H "Content-Type: application/json" \
        -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"warmup"}],"max_tokens":10}' \
        > /dev/null
done
log_info "✓ 预热完成"
echo ""

# 7. 并发测试
log_info "7. 运行并发测试..."
log_info "配置: 并发=${CONCURRENCY}, 总请求=${REQUESTS}"
log_info "Mock: 延迟=${MOCK_MIN_DELAY}-${MOCK_MAX_DELAY}ms, 错误率=${MOCK_ERROR_RATE}, 慢请求率=${MOCK_SLOW_RATE}"
echo ""

cat > /tmp/e2e-request.json << 'EOF'
{
  "model": "gpt-4o-mini",
  "messages": [{"role": "user", "content": "test"}],
  "max_tokens": 50
}
EOF

# 使用 ab 压测
ab -n $REQUESTS -c $CONCURRENCY \
    -T application/json \
    -p /tmp/e2e-request.json \
    -g /tmp/e2e-ab-plot.tsv \
    http://localhost:$GATEWAY_PORT/v1/chat/completions \
    > /tmp/e2e-ab-result.txt 2>&1

echo ""
log_info "✓ 压测完成"
echo ""

# 8. 分析结果
log_info "8. 分析测试结果..."
echo ""

echo "════════════════════════════════════════════════════════════════"
echo "                        压测结果"
echo "════════════════════════════════════════════════════════════════"
grep -E "Requests per second|Time per request|Transfer rate|failed|Percentage" /tmp/e2e-ab-result.txt || true
echo ""

echo "════════════════════════════════════════════════════════════════"
echo "                      延迟分布"
echo "════════════════════════════════════════════════════════════════"
awk '$1 ~ /^[0-9]+%$/ {printf "%-10s %s\n", $1, $2}' /tmp/e2e-ab-result.txt || true
echo ""

# 9. 验证指标
log_info "9. 验证关键指标..."
echo ""

# 提取指标
RPS=$(grep "Requests per second" /tmp/e2e-ab-result.txt | awk '{print $4}')
MEAN_TIME=$(grep "Time per request" /tmp/e2e-ab-result.txt | head -1 | awk '{print $4}')
FAILED=$(grep "Failed requests" /tmp/e2e-ab-result.txt | awk '{print $3}')
P50=$(grep "50%" /tmp/e2e-ab-result.txt | awk '{print $2}')
P95=$(grep "95%" /tmp/e2e-ab-result.txt | awk '{print $2}')
P99=$(grep "99%" /tmp/e2e-ab-result.txt | awk '{print $2}')

echo "关键指标:"
echo "  RPS (请求/秒):        $RPS"
echo "  平均响应时间:         ${MEAN_TIME}ms"
echo "  失败请求数:           $FAILED"
echo "  P50 延迟:             ${P50}ms"
echo "  P95 延迟:             ${P95}ms"
echo "  P99 延迟:             ${P99}ms"
echo ""

# 指标验证
PASS=true

# 验证失败率 < 1%
FAIL_RATE=$(echo "scale=4; $FAILED / $REQUESTS" | bc)
if [ $(echo "$FAIL_RATE > 0.01" | bc) -eq 1 ]; then
    log_error "✗ 失败率过高: ${FAIL_RATE} (期望 < 0.01)"
    PASS=false
else
    log_info "✓ 失败率正常: ${FAIL_RATE}"
fi

# 验证 P99 < 3s (考虑 mock slow rate)
if [ $(echo "$P99 > 3000" | bc) -eq 1 ]; then
    log_warn "⚠ P99 延迟较高: ${P99}ms (期望 < 3000ms，但考虑 mock slow rate)"
else
    log_info "✓ P99 延迟正常: ${P99}ms"
fi

echo ""

# 10. 网关日志摘要
log_info "10. 网关日志摘要..."
echo ""
echo "请求总数:"
grep -c "request_done" /tmp/e2e-gateway.log || echo "0"
echo ""
echo "错误日志:"
grep -i error /tmp/e2e-gateway.log | head -5 || echo "无错误"
echo ""

# 11. Mock 服务器统计
log_info "11. Mock 服务器统计..."
echo ""
echo "Mock 日志摘要:"
echo "  正常响应: $(grep -c '\[OK\]' /tmp/e2e-mock.log || echo 0)"
echo "  错误响应: $(grep -c '\[ERROR\]' /tmp/e2e-mock.log || echo 0)"
echo "  慢响应:   $(grep -c '\[SLOW\]' /tmp/e2e-mock.log || echo 0)"
echo ""

# 12. 生成报告
log_info "12. 生成测试报告..."
cat > /tmp/e2e-report.txt << EOF
════════════════════════════════════════════════════════════════
                     E2E 测试报告
════════════════════════════════════════════════════════════════

测试时间: $(date)
并发数:   $CONCURRENCY
总请求:   $REQUESTS

Mock 配置:
  延迟:     ${MOCK_MIN_DELAY}-${MOCK_MAX_DELAY}ms
  错误率:   ${MOCK_ERROR_RATE}
  慢请求率: ${MOCK_SLOW_RATE} (${MOCK_SLOW_DELAY}ms)

关键指标:
  RPS:              $RPS 请求/秒
  平均响应时间:     ${MEAN_TIME}ms
  失败请求:         $FAILED / $REQUESTS
  失败率:           ${FAIL_RATE}

延迟分布:
  P50:              ${P50}ms
  P95:              ${P95}ms
  P99:              ${P99}ms

验证结果: $([ "$PASS" = true ] && echo "✓ 通过" || echo "✗ 失败")

详细日志:
  压测结果:   /tmp/e2e-ab-result.txt
  网关日志:   /tmp/e2e-gateway.log
  Mock日志:   /tmp/e2e-mock.log

════════════════════════════════════════════════════════════════
EOF

cat /tmp/e2e-report.txt

log_info "报告已保存至: /tmp/e2e-report.txt"
echo ""

if [ "$PASS" = true ]; then
    echo -e "${GREEN}════════════════════════════════════════════════════════════════${NC}"
    echo -e "${GREEN}                    ✓ E2E 测试通过${NC}"
    echo -e "${GREEN}════════════════════════════════════════════════════════════════${NC}"
    exit 0
else
    echo -e "${RED}════════════════════════════════════════════════════════════════${NC}"
    echo -e "${RED}                    ✗ E2E 测试失败${NC}"
    echo -e "${RED}════════════════════════════════════════════════════════════════${NC}"
    exit 1
fi
