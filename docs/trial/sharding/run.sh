#!/bin/bash
# 静的シャーディング（claim の git 配布・S2-free シャード・ハンドオフ）と inventory 検証を、
# 実バイナリと本物の git（ローカルの bare リポジトリ）で通して流すスクリプト（docs/trial/sharding/README.md）。
# 使い方: docs/trial/sharding/run.sh [作業ディレクトリ]   （既定 /tmp/sakutei-trial-sharding）
# 作業ディレクトリは毎回作り直す。リポジトリのファイルは書き換えない。外のサービスには繋がない。
# 確かめた観点ごとに OK／NG を出し、NG が 1 つでもあれば最後に exit 1 で終わる。
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../../.." && pwd)
W=${1:-/tmp/sakutei-trial-sharding}

step() { printf '\n==== %s ====\n' "$*"; }
NG=0
ok() { echo "OK: $*"; }
ng() { echo "NG: $*"; NG=$((NG + 1)); }
check() { # check <説明> <コマンド...>
	local what=$1; shift
	if "$@"; then ok "$what"; else ng "$what"; fi
}

# wait_for <秒> <ファイル> <grep -E の式>：ログに式が出るまで待つ
wait_for() {
	local i=0
	while [ $i -lt $(($1 * 10)) ]; do
		grep -qE "$3" "$2" 2>/dev/null && return 0
		sleep 0.1; i=$((i + 1))
	done
	return 1
}
# zone_has <ゾーンの行の式>
zone_has() { grep -qE "$1" "$W/example.com.zone"; }
wait_zone() { # wait_zone <秒> <式>
	local i=0
	while [ $i -lt $(($1 * 10)) ]; do
		zone_has "$2" && return 0
		sleep 0.1; i=$((i + 1))
	done
	return 1
}
# ログの抜粋（毎周の converged 行は省く）
excerpt() { grep -vE 'converged \(no changes\)' "$1" | sed 's/^/    /'; }

PIDS=""
cleanup() { for p in $PIDS; do kill "$p" 2>/dev/null || true; done; wait 2>/dev/null || true; }
trap cleanup EXIT

rm -rf "$W"
mkdir -p "$W/logs"
cp "$HERE/initial/example.com.zone" "$HERE/initial/fake-reality.json" "$W/"

step "0. ビルドと、中央 git（bare）・ミラーの用意"
(cd "$REPO" && go build -o "$W/sakutei" ./cmd/sakutei)
cd "$W"
S=$W/sakutei
export GIT_AUTHOR_NAME=trial GIT_AUTHOR_EMAIL=trial@example.com GIT_COMMITTER_NAME=trial GIT_COMMITTER_EMAIL=trial@example.com
git init -q --bare -b main central.git        # 中央（PR がマージされる先）
git clone -q central.git work 2>/dev/null      # 人の手元（ここでコミットして push = PR のマージ）
cp -r "$HERE/repo/." work/
git -C work add -A && git -C work commit -qm "initial intent: sites a, b and c (c is provisioned by central)"
git -C work push -q origin HEAD:main
# merge <メッセージ>：手元の変更をコミットして中央へ push し、ミラーを追従させる
merge() {
	git -C "$W/work" add -A && git -C "$W/work" commit -qm "$1" && git -C "$W/work" push -q origin HEAD:main
	$S mirror --upstream "$W/central.git" --dir "$W/mirror.git" --once >/dev/null
	echo "merged: $1 ($(git -C "$W/work" rev-parse --short HEAD))"
}
$S mirror --upstream "$W/central.git" --dir "$W/mirror.git" --once
MIRROR=file://$W/mirror.git
# 拠点のデーモン共通：ミラーから pull、現実は 1 つのゾーンファイル（拠点ごとに自分のレコードを持つ）、
# fencing の世代は全シャードで共有するストア（§15.2）。ログは logs/<名前>.log。
start() { # start <ログ名> <追加の引数...>
	local name=$1; shift
	"$S" --intent-repo "$MIRROR" --git-poll 1s --interval 1s --zone "$W/example.com.zone" \
		--fencing-store "$W/fencing.json" "$@" >>"$W/logs/$name.log" 2>&1 &
	eval "PID_$(echo "$name" | tr -c 'a-zA-Z0-9\n' _)=$!"
	PIDS="$PIDS $!"
}
pid_of() { eval "echo \$PID_$(echo "$1" | tr -c 'a-zA-Z0-9\n' _)"; }
stop() { local p; p=$(pid_of "$1"); kill "$p" 2>/dev/null || true; wait "$p" 2>/dev/null || true; }
holder() { $S fence --store "$W/fencing.json" | awk -v s="$1" '$1 == s { print $3 }'; }

step "1. 2 つの店舗デーモン（S2-free）が shards/<名前>.yaml の claim で担当を分ける（§17.1・§17.4・§17.5）"
start store-a --workdir host-a --shard store-a --shard-id store-a --s2-free
start store-b --workdir host-b --shard store-b --shard-id store-b --s2-free
start central --workdir host-central --shard central --shard-id central
start store-c --workdir host-c --shard store-c --shard-id store-c --s2-free
check "a・b・c のレコードがそれぞれの担当で入る" wait_zone 15 '^c-www'
wait_zone 10 '^a-api' && wait_zone 10 '^b-www' || true
check "store-c は開店前で idle" wait_for 10 logs/store-c.log 'claims nothing.*idle'
sed 's/^/    /' example.com.zone
$S fence --store fencing.json | sed 's/^/    /'
echo "--- host-a の手元写し（sparse checkout。site=b・site=c は無い）"
(cd host-a/repo && find . -path ./.git -prune -o -type f -print | sort | sed 's/^/    /')
check "host-a に intent/site=b が無い" test ! -e host-a/repo/intent/site=b
missing=$(git -C host-a/repo rev-list --objects --missing=print HEAD | grep -c '^?' || true)
check "host-a は他拠点の blob を持っていない（partial clone、欠けた blob $missing 個）" test "$missing" -gt 0

echo "--- 互いの担当のずれは、それぞれの担当だけが直す"
sed -i 's/192.0.2.11/198.51.100.11/; s/192.0.2.21/198.51.100.21/' example.com.zone
check "a-www は store-a が直す" wait_for 10 logs/store-a.log 'Drift dnszone/site=a/a-www.*resolved'
check "b-www は store-b が直す" wait_for 10 logs/store-b.log 'Drift dnszone/site=b/b-www.*resolved'
check "store-a は site=b に触れない" bash -c "! grep -q 'site=b' logs/store-a.log"
check "store-b は site=a に触れない" bash -c "! grep -q 'site=a' logs/store-b.log"

step "2. 二重 claim：同じ shards/store-a.yaml で 2 つ目のデーモンを起こす（§15.2・§17.1）"
start store-a2 --workdir host-a2 --shard store-a --shard-id store-a2 --s2-free
check "新しい方が世代を上げて site=a を取る" wait_for 15 logs/store-a2.log 'site=a taken over from "store-a"'
check "古い方は FENCED で書かない" wait_for 10 logs/store-a.log 'FENCED'
sed -i 's/192.0.2.12/198.51.100.12/' example.com.zone
check "a-api のずれは新しい方だけが直す" wait_for 10 logs/store-a2.log 'Drift dnszone/site=a/a-api.*resolved'
check "古い方は a-api を直していない" bash -c "! grep -q 'a-api' logs/store-a.log"
$S fence --store fencing.json | sed 's/^/    /'
stop store-a2; stop store-a
start store-a --workdir host-a --shard store-a --shard-id store-a --s2-free   # §17.6 の 4：再起動で取り戻す
check "store-a を起こし直すと世代を上げて取り戻す" wait_for 15 logs/store-a.log 'site=a taken over from "store-a2"'

step "3. 開店：claim 変更 PR で site=c を中央から店舗へ（§17.7）"
printf '# 中央のプロビジョニング claim（店舗 c は開店済み）\nclaim:\n' >work/shards/central.yaml
printf '# 店舗 c のデーモン（S2-free）の担当\nclaim:\n  site: c\n' >work/shards/store-c.yaml
merge "open store c: hand site=c over to store-c"
check "中央は claim を手放す" wait_for 15 logs/central.log 'claim released'
check "店舗が世代を上げて site=c を取る" wait_for 15 logs/store-c.log 'site=c taken over from "central"'
check "fencing の holder は store-c" test "$(holder site=c)" = store-c
sed -i 's/192.0.2.31/198.51.100.31/' example.com.zone
check "c-www のずれは店舗が直す" wait_for 10 logs/store-c.log 'Drift dnszone/site=c/c-www.*resolved'
check "中央は c-www を直していない" bash -c "! grep -q 'c-www' logs/central.log"

echo "--- 閉店。ただし店舗はまだ claim 変更を pull していない（--git-poll 1h で起こし直す）"
stop store-c
start store-c-slow --workdir host-c --shard store-c --shard-id store-c --s2-free --git-poll 1h
check "店舗（遅い方）が site=c を取る" wait_for 15 logs/store-c-slow.log 'site=c (acquired|taken over)'
printf '# 中央のプロビジョニング claim。閉店した店舗 c を戻す（§17.7）\nclaim:\n  site: c\n' >work/shards/central.yaml
printf '# 店舗 c のデーモン（閉店。idle）\nclaim:\n' >work/shards/store-c.yaml
merge "close store c: hand site=c back to central"
check "中央が世代を上げて site=c を取る" wait_for 15 logs/central.log 'site=c taken over from "store-c"'
check "claim 変更を知らない店舗は FENCED で書かない" wait_for 10 logs/store-c-slow.log 'FENCED'
sed -i 's/192.0.2.31/198.51.100.32/' example.com.zone
check "c-www のずれは中央が直す" wait_for 10 logs/central.log 'DriftEvent: dnszone/site=c/c-www.*198.51.100.32'
wait_zone 10 '^c-www.* 192\.0\.2\.31$' || true
check "店舗（遅い方）は c-www を直していない" bash -c "! grep -q 'c-www' logs/store-c-slow.log"
stop store-c-slow

step "4. WAN 断：ミラーが消えても、前回 pull した intent で自己修復を続ける（§17.4）"
mv mirror.git mirror.git.down
check "店舗は前回の revision で収束を続けると言う" wait_for 10 logs/store-a.log 'converging on the last pulled revision'
sed -i 's/192.0.2.11/198.51.100.111/' example.com.zone
check "WAN 断の間も a-www のずれを直す" wait_zone 10 '^a-www.* 192\.0\.2\.11$'
rm -rf mirror.git.down
$S mirror --upstream "$W/central.git" --dir "$W/mirror.git" --once   # 状態を持たない中継なので作り直せる

echo "--- 4b. fencing のストアにも届かないとき（実際の多拠点では fencing ストアは拠点の外にある）"
mkdir -p fz/store && cp "$HERE/initial/example.com.zone" fz/example.com.zone
"$S" --intent-repo "$MIRROR" --git-poll 1s --interval 1s --zone "$W/fz/example.com.zone" --fencing-store "$W/fz/store/fencing.json" \
	--workdir fz/host --shard store-b --shard-id store-b-fz --s2-free >>logs/fz.log 2>&1 & PID_fz=$!; PIDS="$PIDS $!"
check "（4b）b-www が入る" wait_for 10 logs/fz.log 'cycle 1: observed'
chmod 000 fz/store                               # fencing ストアに届かない
sed -i 's/192.0.2.21/198.51.100.24/' fz/example.com.zone
check "（4b）周を飛ばす" wait_for 10 logs/fz.log 'fencing store unreachable; cycle skipped'
sleep 3
if grep -q '198.51.100.24' fz/example.com.zone; then
	echo "見つけたこと：fencing ストアに届かない間は、S2-free シャードもずれを直さない（README の「見つけたこと」6）"
else
	ok "（4b）fencing ストアに届かなくても直す"
fi
chmod 755 fz/store
check "（4b）届くようになると直す" wait_for 10 logs/fz.log 'RESOLVED Drift dnszone/site=b/b-www'
kill $PID_fz; wait $PID_fz 2>/dev/null || true

step "5. S2-free シャードのホスト全損 → 再イメージ → pull → 収束（§17.5）"
kill -9 "$(pid_of store-b)"; wait "$(pid_of store-b)" 2>/dev/null || true
rm -rf host-b                                    # 手元写しも render 出力も残らない
sed -i 's/192.0.2.21/198.51.100.22/' example.com.zone   # 止まっている間に現実がずれる
cat >>work/intent/site=b/dns.yaml <<'EOF'
  - provider: dnszone
    id: b-mail.example.com./A
    scope:
      site: b
    fields:
      name: b-mail.example.com.
      type: A
      value: 192.0.2.22
      ttl: "300"
EOF
merge "site b: add b-mail"                       # 止まっている間に intent も進む
start store-b2 --workdir host-b --shard store-b --shard-id store-b --s2-free   # 同じフラグで起こす
check "最新の intent を pull して b-mail を足す" wait_zone 15 '^b-mail'
check "ずれた b-www を戻す" wait_zone 10 '^b-www.* 192\.0\.2\.21$'
echo "--- 復旧後の host-b（pull と収束で作り直せるものだけ）"
(cd host-b && find . -path ./repo/.git -prune -o -type f -print | sort | sed 's/^/    /')

step "6. claim の外は prune しない（§17.1・FIX #6）"
echo "--- 6a. dnszone：site=a の intent から a-api を外し、PruneWithinScope にする"
cat >work/intent/site=a/dns.yaml <<'EOF'
# 拠点 a の DNS レコード（手順 6a：a-api を外し、PruneWithinScope にした）
version: "1"
resources:
  - provider: dnszone
    id: a-www.example.com./A
    scope:
      site: a
    deletion_policy: PruneWithinScope
    fields:
      name: a-www.example.com.
      type: A
      value: 192.0.2.11
      ttl: "300"
EOF
merge "site a: drop a-api, prune within scope"
sleep 4
check "b・c・www のレコードは残る（claim の外）" bash -c "grep -q '^b-www' example.com.zone && grep -q '^c-www' example.com.zone && grep -q '^www' example.com.zone"
if zone_has '^a-api'; then
	echo "見つけたこと：dnszone の List は scope を返さない（root scope）ので、scope 付きの intent の PruneWithinScope は"
	echo "  claim の中のレコード（a-api）も消さない。知らせも出ない（README の「見つけたこと」3）"
else
	ok "a-api は prune された"
fi

echo "--- 6b. fake provider（List が scope を返す）：claim site=a で a-old だけを prune する"
mkdir -p work/intent/site=a
cat >work/intent/site=a/vms.yaml <<'EOF'
# 手順 6b：fake provider の a-vm だけを宣言する。a-old は site=a の中の孤児、b-* と stray は claim の外
version: "1"
resources:
  - provider: fake
    id: a-vm
    scope:
      site: a
    deletion_policy: PruneWithinScope
    fields:
      name: a-vm
      size: "1"
EOF
merge "site a: fake vms (prune within scope)"
stop store-a
start store-a-fake --workdir host-a --shard store-a --shard-id store-a --s2-free --state "$W/fake-reality.json"
check "a-old を prune する" wait_for 15 logs/store-a-fake.log 'pruned fake/site=a/a-old'
sleep 2
left=$(python3 -c "import json;print(' '.join(sorted(json.load(open('fake-reality.json'))['resources'])))")
echo "    現実に残ったもの: $left"
check "b-vm・b-old（claim の外）と stray（scope なし）は残る" test "$left" = "a-vm b-old b-vm stray"
stop store-a-fake; stop store-b2; stop central

step "7. inventory の検証（§9.2 incomplete≠empty）"
mkdir -p inv && cp "$HERE/initial/example.com.zone" inv/example.com.zone && cp "$HERE/inventory/intent.yaml" inv/intent.yaml
mkdir -p inv/intent && mv inv/intent.yaml inv/intent/main.yaml
# inv <ホスト名...>：inventory を書き換える（書き込みは一時ファイル → rename）
inv() {
	local i=0 h
	{
		echo '# CMDB の export の代役（手順 7）'; echo 'version: "1"'; echo 'resources:'
		for h in "$@"; do
			i=$((i + 1))
			printf '  - provider: dnszone\n    id: %s.example.com./A\n    deletion_policy: PruneWithinScope\n    fields:\n      name: %s.example.com.\n      type: A\n      value: 192.0.2.%d\n      ttl: "300"\n' "$h" "$h" $((100 + i))
		done
	} >"$W/inv/inventory.yaml.tmp" && mv "$W/inv/inventory.yaml.tmp" "$W/inv/inventory.yaml"
}
izone() { grep -cE '^h[0-9]' "$W/inv/example.com.zone" || true; }
INV="--intent $W/inv/intent --zone $W/inv/example.com.zone --inventory-file $W/inv/inventory.yaml --interval 1s"
inv h1 h2 h3 h4
$S $INV >>logs/inv.log 2>&1 & PID_inv=$!; PIDS="$PIDS $!"
check "inventory の 4 件が入る" wait_for 10 logs/inv.log 'cycle 2: .*converged'
echo "--- 4 → 1 件（半分未満）"
inv h1
check "InventoryHalted で止まる" wait_for 10 logs/inv.log 'EVENT InventoryHalted'
sleep 2
check "止まっている間は何も消さない（h1〜h4 の 4 件のまま）" test "$(izone)" = 4
echo "--- 4 → 2 件（ちょうど半分は受け入れる）"
inv h1 h2
check "受け入れて 2 件を prune する" wait_for 10 logs/inv.log 'pruned=2'
check "halt が解ける" wait_for 10 logs/inv.log 'RESOLVED InventoryHalted'
check "h1・h2 の 2 件が残る" test "$(izone)" = 2
echo "--- inventory のファイルが消える（不達）"
rm inv/inventory.yaml
check "前回の inventory で続ける" wait_for 10 logs/inv.log 'using the previous inventory'
sleep 2
check "不達でも消さない" test "$(izone)" = 2
kill $PID_inv; wait $PID_inv 2>/dev/null || true
echo "--- 再起動直後（基準は S3 で消えている）に 1 件：--inventory-expected-min 2 で守る"
inv h1
$S $INV --inventory-expected-min 2 >>logs/inv-min.log 2>&1 & PID_inv=$!; PIDS="$PIDS $!"
check "期待基数未満で最初の周から止まる" wait_for 10 logs/inv-min.log 'fewer than the expected 2'
sleep 1
check "消さない" test "$(izone)" = 2
kill $PID_inv; wait $PID_inv 2>/dev/null || true
echo "--- 期待基数なしで起こし直すと、最初の取得はそのまま受け入れる（設計どおり §9.2）"
$S $INV >>logs/inv-nomin.log 2>&1 & PID_inv=$!; PIDS="$PIDS $!"
check "1 件を受け入れて h2 を prune する" wait_for 10 logs/inv-nomin.log 'pruned dnszone//h2'
kill $PID_inv; wait $PID_inv 2>/dev/null || true

step "ログの抜粋"
for f in store-a store-a2 store-b store-b2 central store-c store-c-slow fz store-a-fake inv inv-min inv-nomin; do
	echo "--- logs/$f.log"; excerpt "logs/$f.log"
done

step "おわり（作業ディレクトリ: $W、NG=$NG）"
[ "$NG" -eq 0 ]
