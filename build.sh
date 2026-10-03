#!/usr/bin/env bash
# 构建 cfddns。
#
#   ./build.sh            本机架构 -> dist/cfddns
#   ./build.sh all        全平台产物（与 CI 命名一致）
#   ./build.sh arm64      指定架构
#   ./build.sh windows    交叉编译 Windows
#
# 产物命名: cfddns-<goos>-<goarch>[.exe]
set -euo pipefail

cd "$(dirname "$0")"
ROOT=$(pwd)
OUT=${OUT:-$ROOT/dist}
mkdir -p "$OUT"

VERSION=${VERSION:-1.0.0}
LDFLAGS="-s -w"

cd "$ROOT/src"

echo "==> go vet"
go vet ./...

echo "==> go test"
go test ./...

build() {
  local goos=$1 goarch=$2 suffix=$3
  local name="cfddns-${goos}-${goarch}${suffix}"
  printf '==> %s/%s -> %s\n' "$goos" "$goarch" "$name"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -ldflags "$LDFLAGS" -o "$OUT/$name" .
}

case "${1:-local}" in
  all)
    build linux amd64 ""
    build linux arm64 ""
    build windows amd64 .exe
    ;;
  arm64)
    build linux arm64 ""
    ;;
  windows)
    build windows amd64 .exe
    ;;
  local)
    build "$(go env GOOS)" "$(go env GOARCH)" ""
    ;;
  *)
    echo "未知参数: $1（可用: local / all / arm64 / windows）" >&2
    exit 2
    ;;
esac

# 校验工具只在类 Unix 上有意义
if CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o "$OUT/dnsverify" ./cmd/dnsverify 2>/dev/null; then
  echo "==> 已构建 dnsverify"
fi

echo
echo "完成。产物:"
ls -l "$OUT"
echo
echo "安装: install -m 0755 $OUT/cfddns-\$(uname -s | tr 'A-Z' 'a-z')-\$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/') /usr/local/bin/cfddns"
echo "版本: $VERSION"
