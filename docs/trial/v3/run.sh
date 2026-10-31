#!/bin/sh
# 新しいコア（v3）を v2 の試用と同じ手順（DNS と FW）で通して流すスクリプト
# （docs/trial/README.md の「v3 で試す」）。同じ手順を providers/cmd/sakutei の
# TestTrial が回す。
# 使い方: docs/trial/v3/run.sh [作業ディレクトリ]   （既定 /tmp/sakutei-trial-v3）
# 作業ディレクトリは毎回作り直す。リポジトリのファイルは書き換えない。
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../../.." && pwd)
W=${1:-/tmp/sakutei-trial-v3}

step() { printf '\n==== %s ====\n' "$*"; }

rm -rf "$W"
mkdir -p "$W"
cp "$HERE/initial/example.com.zone" "$HERE/initial/firewall.rules" "$HERE/config.yaml" "$W/"
cp -r "$HERE/intent" "$W/intent"

step "0. ビルド"
(cd "$REPO/v3/providers" && go build -o "$W/sakutei" ./cmd/sakutei)
cd "$W"
S=./sakutei
ONE="--intent intent --config config.yaml --record record"

step "1. plan（何が変わるかを見るだけ。ファイルは書き換えない）"
cp example.com.zone zone.before
$S plan --intent intent --config config.yaml || true
cmp -s example.com.zone zone.before && echo "→ plan はゾーンファイルを書き換えていない"

step "2. reconcile（1 周だけ収束させる）"
$S reconcile $ONE
echo "--- ゾーンファイル"; cat example.com.zone
echo "--- ruleset"; cat firewall.rules
$S plan --intent intent --config config.yaml | head -1

step "3. 手でずらす → plan で見える → reconcile で戻る"
sed -i 's/192.0.2.30/198.51.100.99/' example.com.zone
$S plan --intent intent --config config.yaml || true
$S reconcile $ONE
grep 'api' example.com.zone

step "4. freeze（知らせるだけで直さない）→ unfreeze で直る"
sed -i 's/192.0.2.20/203.0.113.1/' example.com.zone
$S freeze --record record --reason "試用: 知らせるだけ" --until 1h --name trial --as trial-user
$S reconcile $ONE || echo "→ 終了コード $?（ずれが残る）"
grep 'www' example.com.zone
$S unfreeze trial --record record --reason "試用おわり" --as trial-user
$S reconcile $ONE
grep 'www' example.com.zone

step "5. intent から消しても、既定（prune: false）では現実から消さない"
cp intent/main.yaml main.yaml.bak
sed '/# api-begin/,/# api-end/d' main.yaml.bak > intent/main.yaml
$S reconcile $ONE
grep 'api' example.com.zone && echo "→ api は残っている（宣言外として知らせる）"
cp main.yaml.bak intent/main.yaml

step "おわり（作業ディレクトリ: $W）"
