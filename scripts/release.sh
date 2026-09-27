#!/usr/bin/env bash
# 交叉编译发布矩阵 → dist/。全部依赖纯 Go（modernc SQLite、aws-sdk、bcrypt），
# CGO 可关，一套源码出 Windows/Linux/macOS 五个平台产物。
#
#   ./scripts/release.sh            # 默认矩阵
#   VERSION=v1.2.3 ./scripts/release.sh
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${VERSION:-$(date +%Y%m%d)-dev}"
OUT=dist
rm -rf "$OUT" && mkdir -p "$OUT"
LDFLAGS="-s -w -X main.version=${VERSION}"
ASSETS=(README.md LICENSE pageshare.example.json)

pack() { # pack <os-arch> [exe 后缀]——产物在 dist/ 内自含，可重复执行
	local name=$1 ext=${2:-}
	local dir="$OUT/pageshare-$VERSION-$name"
	rm -rf "$dir" && mkdir -p "$dir"
	CGO_ENABLED=0 GOOS="${name%-*}" GOARCH="${name##*-}" \
		go build -trimpath -ldflags "$LDFLAGS" -o "$dir/pageshare$ext" ./cmd/pageshare
	cp "${ASSETS[@]}" "$dir/"
	if [[ "$name" == windows-* ]]; then
		( cd "$OUT" && zip -qr "pageshare-$VERSION-$name.zip" "pageshare-$VERSION-$name" )
	else
		( cd "$OUT" && tar -czf "pageshare-$VERSION-$name.tar.gz" "pageshare-$VERSION-$name" )
	fi
	rm -rf "$dir"
	echo "  ✔ pageshare-$VERSION-$name"
}

echo "pageshare $VERSION 发布构建："
pack linux-amd64
pack linux-arm64
pack darwin-arm64
pack darwin-amd64
pack windows-amd64 .exe

( cd "$OUT" && shasum -a 256 pageshare-* > sha256sums.txt )
echo
ls -lh "$OUT"
