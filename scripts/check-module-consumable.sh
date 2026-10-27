#!/bin/sh
# check-module-consumable.sh <version> — 外のモジュールから Sakutei を Go のモジュール
# として取り込めることを確かめる（v2.0.0 で module のパスに /v2 を付け忘れ、
# go get で取れなかったことの再発防止）。
#
# examples/external-provider を一時ディレクトリに写し、replace を外して
# `go get github.com/tjst-t/sakutei/v2@<version>` で本体を取り、build する。
# <version> はタグ（v2.0.1）か commit（擬似版になる）。公開されたものを取りに
# 行くので、ネットワークが要る（check・test には入れない。CI とリリースの前の点検で回す）。
set -eu
version=${1:?usage: check-module-consumable.sh <tag-or-commit>}
module=github.com/tjst-t/sakutei/v2
root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cp -R "$root/examples/external-provider/." "$work/"
cd "$work"
go mod edit -dropreplace="$module" -droprequire="$module"
# 公開されたばかりの commit はプロキシにまだ無いことがあるので、直接取りに行く。
GOPRIVATE=$module GOFLAGS=-mod=mod go get "$module@$version"
go build ./...
echo "ok: $module@$version can be fetched and examples/external-provider builds against it"
