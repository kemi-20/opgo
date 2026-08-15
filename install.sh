#!/usr/bin/env bash
# opgo 一键安装脚本（Ubuntu / Debian amd64）
# 用法: curl -fsSL https://raw.githubusercontent.com/<用户名>/opgo/main/install.sh | sudo bash
set -euo pipefail

REPO="${OPGO_REPO:-kemi-20/opgo}"
OPGO_DIR="/opt/opgo"
BIN="$OPGO_DIR/opgo"
SERVICE="opgo.service"

if [ "$(id -u)" -ne 0 ]; then
  echo "请以 root 运行：sudo bash install.sh" >&2
  exit 1
fi

echo "==> 安装依赖（curl / jq）"
if ! command -v curl >/dev/null 2>&1; then
  apt-get update -y && apt-get install -y curl
fi
if ! command -v jq >/dev/null 2>&1; then
  apt-get update -y && apt-get install -y jq
fi

echo "==> 创建用户与目录"
if ! id -u opgo >/dev/null 2>&1; then
  useradd --system --no-create-home --home-dir "$OPGO_DIR" opgo
fi
mkdir -p "$OPGO_DIR"
chown opgo:opgo "$OPGO_DIR"

echo "==> 下载最新 release"
ASSET_URL="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | jq -r '.assets[] | select(.name=="opgo-linux-amd64") | .browser_download_url')"
if [ -z "$ASSET_URL" ]; then
  echo "未找到 opgo-linux-amd64，请确认仓库 ${REPO} 已发布 v* 版本。" >&2
  exit 1
fi
curl -fsSL -o "${BIN}.tmp" "$ASSET_URL"
chmod +x "${BIN}.tmp"
mv "${BIN}.tmp" "$BIN"
chown opgo:opgo "$BIN"

echo "==> 写入 systemd 服务"
cat > "/etc/systemd/system/${SERVICE}" <<'UNIT'
[Unit]
Description=opgo - Coding Plan sharing gateway
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=opgo
Group=opgo
ExecStart=/opt/opgo/opgo
Restart=on-failure
RestartSec=3
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
CapabilityBoundingSet=
AmbientCapabilities=
RestrictAddressFamilies=AF_INET AF_INET6
RestrictRealtime=true
LockPersonality=true
MemoryDenyWriteExecute=true
ReadWritePaths=/opt/opgo

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable --now "$SERVICE"

echo ""
echo "安装完成。首次启动会自动生成示例配置 /opt/opgo/config.json，"
echo "请编辑该文件（upstream_base / master_key / admin_password / users）后执行："
echo "  sudo systemctl restart opgo"
echo "查看日志："
echo "  journalctl -u opgo -f"
