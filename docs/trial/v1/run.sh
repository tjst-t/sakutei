#!/bin/sh
# v1 を手で試す手順を、1 回で通して流すスクリプト（docs/trial/README.md の手順 1〜6）。
# 使い方: docs/trial/v1/run.sh [作業ディレクトリ]   （既定 /tmp/sakutei-trial）
# 作業ディレクトリは毎回作り直す。リポジトリのファイルは書き換えない。
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../../.." && pwd)
W=${1:-/tmp/sakutei-trial}

step() { printf '\n==== %s ====\n' "$*"; }

rm -rf "$W"
mkdir -p "$W"
cp "$HERE/initial/example.com.zone" "$HERE/initial/firewall.rules" "$W/"
cp -r "$HERE/intent" "$W/intent"

step "0. ビルド"
(cd "$REPO" && go build -o "$W/sakutei" ./cmd/sakutei)
cd "$W"
S=./sakutei
# dnszone は --zone、nftables は --ruleset
ONE="--intent intent --zone example.com.zone --ruleset firewall.rules"

step "1. import（いまの現実を intent の形で書き出す。理想ではなくスナップショット）"
$S import --provider dnszone,nftables --zone example.com.zone --ruleset firewall.rules --out imported --samples 1
cat imported/*.yaml

step "2. plan（何が変わるかを見るだけ。ファイルは書き換えない）"
cp example.com.zone zone.before
$S plan $ONE
cmp -s example.com.zone zone.before && echo "→ plan はゾーンファイルを書き換えていない"

step "3. reconcile（1 周だけ収束させる）"
$S reconcile $ONE
echo "--- ゾーンファイル"; cat example.com.zone
echo "--- ruleset"; cat firewall.rules
$S plan $ONE | tail -1

step "4. 手でずらす → status で見える → reconcile で戻る"
sed -i 's/192.0.2.30/198.51.100.99/' example.com.zone
$S status --intent intent --zone example.com.zone --ruleset firewall.rules || true
$S reconcile $ONE
grep '^api' example.com.zone

step "5. alert-only（ずれを知らせるだけで直さない）"
sed -i 's/192.0.2.20/203.0.113.1/' example.com.zone
$S reconcile $ONE --mode alert-only
grep '^www' example.com.zone
$S reconcile $ONE >/dev/null
grep '^www' example.com.zone

step "6. intent から消しても、既定（ExplicitAbsentOnly）では現実から消さない"
cp intent/main.yaml main.yaml.bak
sed '/# api-begin/,/# api-end/d' main.yaml.bak > intent/main.yaml
$S plan $ONE | tail -3
grep '^api' example.com.zone && echo "→ api は残っている"
cp main.yaml.bak intent/main.yaml

step "おわり（作業ディレクトリ: $W）"
