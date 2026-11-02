#!/usr/bin/env bash
#
# 从官方 Qoder 客户端本地存储提取签到所需的设备指纹
# （cosy-machineid / cosy-machinetype / cosy-machinetoken）
#
# 背景（见 issue #4）：
#   国际版（qoder.com）的「每天领 100 Credits」活动**按真实设备身份下发**。
#   若签到请求不带官方客户端登录时的 cosy 设备指纹，服务端不会下发
#   CLAIM_BEFIT 活动，签到时表现为 `无可用签到活动`（no_campaign）。
#   国内版（qoder.com.cn）通常不需要。
#
#   首次在官方客户端领取一次后，服务端转为「懒下发」，
#   之后即使不带指纹也能查到活动。
#
# 用法：
#   ./extract_fingerprint.sh                 # 自动探测国际版 / 国内版
#   ./extract_fingerprint.sh global          # 指定国际版
#   ./extract_fingerprint.sh cn              # 指定国内版
#
# 输出：可直接粘贴到账号 JSON 的 checkin_fingerprint 片段
#
set -euo pipefail

TARGET="${1:-auto}"

# ---- 平台目录 --------------------------------------------------------------
detect_dir() {
  local region="$1"
  case "$region" in
    global) local names=(com.qoder.app.stable Qoder) ;;
    cn)     local names=(com.qodercn.app.stable "Qoder CN") ;;
    *)      local names=(com.qoder.app.stable Qoder com.qodercn.app.stable "Qoder CN") ;;
  esac
  case "$(uname)" in
    Darwin)
      for n in "${names[@]}"; do
        local p="$HOME/Library/Application Support/$n"
        [ -d "$p" ] && { echo "$p"; return 0; }
      done ;;
    Linux)
      for n in "${names[@]}"; do
        for base in "$HOME/.config" "$HOME/.local/share"; do
          local p="$base/$n"
          [ -d "$p" ] && { echo "$p"; return 0; }
        done
      done ;;
    MINGW*|MSYS*|CYGWIN*)
      for n in "${names[@]}"; do
        local p="${APPDATA:-$HOME/AppData/Roaming}/$n"
        [ -d "$p" ] && { echo "$p"; return 0; }
      done ;;
  esac
  return 1
}

DIR="$(detect_dir "$TARGET" || true)"
if [ -z "$DIR" ]; then
  echo "❌ 未找到 Qoder 客户端数据目录（region=$TARGET）" >&2
  echo "   请确认已安装并登录官方客户端，或用参数指定：global / cn" >&2
  exit 1
fi
echo "客户端数据目录: $DIR"

# ---- machine id（明文文件） ------------------------------------------------
MID=""
for f in "auth.machine-id" "machine_id" ".machine-id"; do
  if [ -f "$DIR/$f" ]; then
    MID="$(tr -d ' \t\r\n' < "$DIR/$f")"
    break
  fi
done
if [ -z "$MID" ]; then
  echo "❌ 未找到 machine id 明文文件（auth.machine-id）" >&2
  echo "   请先在官方客户端完成一次登录" >&2
  exit 1
fi
echo "machine id: $MID"

# ---- machine token / type（在加密的 auth.v1.dat 中） -----------------------
echo ""
echo "⚠️  machine token / machine type 存储于加密文件（无法直接读取）。"
echo ""
echo "   获取方式（二选一）："
echo ""
echo "   方式 A · 抓包（推荐，任何平台可用）"
echo "     1) 启动抓包:  mitmdump -q -s scripts/capture_addon.py -p 8080"
echo "     2) 开启代理:  ./scripts/proxy_toggle.sh on"
echo "     3) 打开官方客户端，进入「用量面板 → 礼物图标」触发签到活动加载"
echo "     4) 关闭代理:  ./scripts/proxy_toggle.sh off"
echo "     5) 从 scripts/capture/ 里找 URL 含 /sash/api/v1/me/campaigns 的请求，"
echo "        取请求头中的 cosy-machinetoken / cosy-machinetype / cosy-version"
echo ""
echo "   方式 B · 从本机客户端自动化提取（仅当本机已安装官方客户端）"
echo "        见 scripts/README 中关于解密 auth.v1.dat 的说明"
echo ""
echo "──── 复制下面片段到账号 JSON（填入 machine_token / machine_type） ────"
cat <<EOF
{
  "checkin_fingerprint": {
    "machine_id": "$MID",
    "machine_token": "<从抓包获取 cosy-machinetoken>",
    "machine_type": "<从抓包获取 cosy-machinetype>",
    "machine_code": "<可选，cosy-machinecode>",
    "hostname": "localhost",
    "machine_os": "aarch64_darwin",
    "version": "<从抓包获取 cosy-version，如 0.4.4>"
  }
}
EOF
echo "────────────────────────────────────────────────────────────────"
echo ""
echo "写入位置: <数据目录>/accounts/<账号ID>.json"
echo "或直接在 Web 控制台「账号列表」中查看该账号 JSON 后添加该字段。"
