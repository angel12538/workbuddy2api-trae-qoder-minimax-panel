#!/bin/sh
# ============================================================
#  Trae2api-cn 授权助手（通用版）
#  中转站:   http://127.0.0.1:8000
#  本机回调: http://127.0.0.1:8765/authorize
#  生成时间: 自动生成
#  运行: sh start_auth.sh  （或 chmod +x 后直接执行）
# ============================================================
set -e

DIR="$(cd "$(dirname "$0")" && pwd)"
RELAY="http://127.0.0.1:8000"
PORT="8765"
HELPER="$DIR/web_login.py"

if [ -d "$HELPER" ]; then
  echo "web_login.py 目标不能是文件夹，请更换授权助手目录后重试。"
  exit 1
fi
echo "[1/2] 正在从中转站刷新授权助手脚本 web_login.py ..."
TMP_HELPER="$HELPER.download.$$"
rm -f "$TMP_HELPER"
if ! curl -fsSL --max-time 20 -o "$TMP_HELPER" "$RELAY/web/login/download?as=py"; then
  rm -f "$TMP_HELPER"
  echo "授权助手脚本下载失败，请确认本机可以访问 $RELAY"
  exit 1
fi
if ! mv "$TMP_HELPER" "$HELPER"; then
  rm -f "$TMP_HELPER"
  echo "无法更新授权助手脚本，请检查本目录写入权限。"
  exit 1
fi

if command -v python3 >/dev/null 2>&1; then
  PYCMD="python3"
elif command -v python >/dev/null 2>&1; then
  PYCMD="python"
else
  echo "未检测到 Python 3，请先安装。"
  exit 1
fi

echo "[2/2] 启动本机授权监听器: http://127.0.0.1:$PORT"
exec "$PYCMD" "$HELPER" --relay "$RELAY" --port "$PORT"
