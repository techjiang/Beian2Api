#!/usr/bin/env bash
# 一键安装：适用于 Debian/Ubuntu/RHEL 系，自带 systemd。
# 用法：sudo ./scripts/install.sh [安装目录]
set -euo pipefail

DIR="${1:-/opt/beian2api}"
BIN_SRC="$(cd "$(dirname "$0")/.." && pwd)/bin/beian2api"

if [[ $EUID -ne 0 ]]; then
  echo "请用 root 运行：sudo $0 [安装目录]" >&2
  exit 1
fi
if [[ ! -f "$BIN_SRC" ]]; then
  echo "找不到 $BIN_SRC，请先在项目根目录执行 make build" >&2
  exit 1
fi

echo "→ 安装到 $DIR"
id -u beian2api >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin beian2api
install -d -o beian2api -g beian2api "$DIR" "$DIR/data"
install -m 0755 "$BIN_SRC" "$DIR/beian2api"
[[ -f "$DIR/config.yaml" ]] || install -m 0644 -o beian2api -g beian2api \
  "$(dirname "$0")/../config.example.yaml" "$DIR/config.yaml"

install -m 0644 "$(dirname "$0")/../deploy/beian2api.service" /etc/systemd/system/beian2api.service
sed -i "s#/opt/beian2api#$DIR#g" /etc/systemd/system/beian2api.service

systemctl daemon-reload
systemctl enable --now beian2api
sleep 1
echo
echo "✔ 已启动。检查状态："
systemctl --no-pager --lines=8 status beian2api || true
echo
echo "健康检查： curl http://127.0.0.1:8080/healthz"
echo "数据目录： $DIR/data（备份这个目录即可）"
