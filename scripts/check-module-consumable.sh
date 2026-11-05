#!/bin/sh
# check-module-consumable.sh <version> — 外のモジュールから Sakutei の 2 つのモジュール
# （コアの github.com/tjst-t/sakutei/v3 と、provider などの github.com/tjst-t/sakutei/v3/providers）
# を Go のモジュールとして取り込めることを確かめる。
#
# 一時ディレクトリに使う側のモジュールを作り、v3/providers/cmd/sakutei と同じ main
# （cli.Main(builtin.Options()...)）を置いて、`go get <module>@<version>` で 2 つを取り、
# build する。<version> はタグか commit（擬似版になる）。公開されたものを取りに行くので、
# ネットワークが要る（check・test には入れない。CI とリリースの前の点検で回す）。
set -eu
version=${1:?usage: check-module-consumable.sh <tag-or-commit>}
core=github.com/tjst-t/sakutei/v3
providers=github.com/tjst-t/sakutei/v3/providers
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cd "$work"
go mod init example.com/sakutei-consumer >/dev/null 2>&1
cat >main.go <<EOF
package main

import (
	"$core/cli"
	"$providers/builtin"
)

func main() { cli.Main(builtin.Options()...) }
EOF
# 公開されたばかりの commit はプロキシにまだ無いことがあるので、直接取りに行く。
GOPRIVATE=github.com/tjst-t/sakutei GOFLAGS=-mod=mod go get "$core@$version" "$providers@$version"
GOFLAGS=-mod=mod go build -o /dev/null .
echo "ok: $core@$version and $providers@$version can be fetched and a binary builds against them"
