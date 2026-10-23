#!/bin/bash
# 一键测试脚本

echo "=== 并发优化一键测试 ==="
echo ""

# 1. 编译
echo "📦 1. 编译项目..."
make build
if [ $? -ne 0 ]; then
  echo "❌ 编译失败"
  exit 1
fi
echo "✅ 编译成功"
echo ""

# 2. 后台启动服务
echo "🚀 2. 启动服务（后台）..."
./bin/opencode2api -config config.test.json > /tmp/opencode2api-test.log 2>&1 &
PID=$!
echo "   进程 PID: $PID"
sleep 3
if ! kill -0 $PID 2>/dev/null; then
  echo "❌ 服务启动失败"
  cat /tmp/opencode2api-test.log
  exit 1
fi
echo "✅ 服务已启动"
echo ""

# 3. 健康检查
echo "🔍 3. 健康检查..."
for i in {1..10}; do
  if curl -s http://localhost:8080/health > /dev/null 2>&1; then
    echo "✅ 服务就绪"
    break
  fi
  if [ $i -eq 10 ]; then
    echo "❌ 服务未就绪"
    kill $PID 2>/dev/null
    exit 1
  fi
  sleep 1
done
echo ""

# 4. 压测
echo "📊 4. 运行 TTFB 压测..."
./scripts/benchmark-ttfb.sh
echo ""

# 5. 停止服务
echo "🛑 5. 停止服务..."
kill $PID 2>/dev/null
wait $PID 2>/dev/null
echo "✅ 服务已停止"
echo ""

echo "=== 测试完成 ==="
echo "日志文件: /tmp/opencode2api-test.log"
echo "压测结果: /tmp/bench-result.txt"
