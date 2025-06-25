#!/bin/bash

# 探针脚本：读取文件中的端口号并检查服务健康状态

# 文件路径（根据实际情况修改）
PORT_FILE="/home/aiges/sglangport"

# 健康检查路径（根据实际情况修改）
HEALTH_PATH="/health"

# 超时时间（秒）
TIMEOUT=1

# 检查文件是否存在
if [ ! -f "$PORT_FILE" ]; then
  echo "错误：端口文件 $PORT_FILE 不存在"
  exit 1
fi

# 读取端口号并清理非数字字符
PORT=$(cat "$PORT_FILE" | tr -cd '[:digit:]')

# 验证端口号范围（1-65535）
if ! echo "$PORT" | grep -E '^[0-9]+$' >/dev/null 2>&1; then
  echo "错误：端口号格式无效: [$PORT]"
  exit 1
fi

# 构建URL
URL="http://localhost:$PORT$HEALTH_PATH"

# 使用curl发送请求并检查状态码
echo "正在检查 $URL ..."
HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" --connect-timeout $TIMEOUT $URL)

# 验证HTTP_CODE是否为有效的数字
if ! echo "$HTTP_CODE" | grep -E '^[0-9]{3}$' >/dev/null; then
  echo "错误：无法获取有效的HTTP状态码: $HTTP_CODE"
  exit 1
fi

# 处理响应
if [ "$HTTP_CODE" -eq 200 ]; then
  echo "服务健康 (HTTP $HTTP_CODE)"
  exit 0
else
  echo "服务不健康 (HTTP $HTTP_CODE)"
  exit 1
fi