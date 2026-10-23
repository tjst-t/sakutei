#!/bin/bash
# 採番（ファイル版 AllocationAuthority・pools・HA 最小形）を手で試す手順を、1 回で通して流すスクリプト
# （docs/trial/allocation/README.md の手順 1〜8）。
# 使い方: docs/trial/allocation/run.sh [作業ディレクトリ]   （既定 /tmp/sakutei-allocation）
# 作業ディレクトリは毎回作り直す。リポジトリのファイルは書き換えない。
# 127.0.0.1 の 2 つのポート（既定 17071・17072、PORT_A・PORT_B で変えられる）で allocator を動かす。curl が要る。
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../../.." && pwd)
W=${1:-/tmp/sakutei-allocation}
PORT_A=${PORT_A:-17071}
PORT_B=${PORT_B:-17072}

step() { printf '\n==== %s ====\n' "$*"; }
note() { printf -- '→ %s\n' "$*"; }

# wait_for <説明> <コマンド…>: コマンドが成り立つまで最大 20 秒待つ。
wait_for() {
	what=$1; shift
	for _ in $(seq 200); do
		if "$@" >/dev/null 2>&1; then return 0; fi
		sleep 0.1
	done
	echo "!! 待ちきれなかった: $what" >&2
	return 1
}
# zone_has <ゾーン> <正規表現>
zone_has() { grep -Eq "$2" "$1"; }
# allocs <採番記録>: 「owner → 値」と cooldown 中の値を並べる。
allocs() {
	awk -F'"' '
		/"allocations": \{/ { sect = "alloc" }
		/"cooldown": \{/    { sect = "cool" }
		sect == "alloc" && /#[a-z_]+": \{/ { owner = $2 }
		sect == "alloc" && /"value":/      { v = $3; gsub(/[: ,]/, "", v); printf "  %s → %s\n", owner, v }
		sect == "cool" && /^ +"[0-9]+": "/ { printf "  cooldown: %s（%s まで）\n", $2, $4 }
		/"generation":/ { g = $3; gsub(/[: ,]/, "", g); printf "  generation: %s\n", g }
	' "$1"
}
# drop_res <intent> <文字列>: その文字列を含むリソース（"  - provider:" から次の要素まで）を消す。
drop_res() {
	awk -v pat="$2" '
		function flush() { if (buf != "" && index(buf, pat) == 0) printf "%s", buf; buf = ""; inres = 0 }
		/^  - provider:/ { flush(); buf = $0 "\n"; inres = 1; next }
		inres && /^    / { buf = buf $0 "\n"; next }
		{ flush(); print }
		END { flush() }
	' "$1" > "$1.tmp" && mv "$1.tmp" "$1"
}
# rc <コマンド…>: 終了コードを表示する（set -e で止めない）。
rc() { set +e; "$@"; c=$?; set -e; echo "（終了コード $c）"; }

PIDS=""
cleanup() { for p in $PIDS; do kill "$p" 2>/dev/null || true; done; wait 2>/dev/null || true; }
trap cleanup EXIT

rm -rf "$W"
mkdir -p "$W"
cp "$HERE/initial/example.com.zone" "$W/"
cp -r "$HERE/intent" "$W/intent"

step "0. ビルド"
(cd "$REPO" && go build -o "$W/sakutei" ./cmd/sakutei)
cd "$W"
S=$W/sakutei
ONE="--intent intent --zone example.com.zone --alloc-store alloc.json"

step "1. plan（読むだけ。払い出さず、採番記録も作らない）"
$S plan $ONE
test ! -e alloc.json && note "plan のあとも採番記録（alloc.json）は無い"

step "2. reconcile（プールを宣言し、小さい順に払い出して TXT に入れる）"
$S reconcile $ONE
grep '^_port' example.com.zone
allocs alloc.json

step "3. 起こし直しても同じ値（1 回収束を繰り返す・デーモンを止めて起こし直す・ずれの修復）"
cp alloc.json alloc.before
$S reconcile $ONE | tail -1
$S --intent intent --zone example.com.zone --alloc-store alloc.json --interval 1s > daemon1.log 2>&1 &
D=$!; PIDS="$PIDS $D"
wait_for "デーモンの 2 周目" grep -q 'cycle 2:' daemon1.log
sed -i 's/IN TXT 8000$/IN TXT 9999/' example.com.zone
note "ゾーンの _port.app1 を手で 9999 に書き換えた"
wait_for "8000 に戻る" zone_has example.com.zone '^_port.app1.example.com. 300 IN TXT 8000$'
grep '^_port.app1' example.com.zone
kill "$D"; wait "$D" || true
$S --intent intent --zone example.com.zone --alloc-store alloc.json --interval 1s > daemon2.log 2>&1 &
D=$!; PIDS="$PIDS $D"
wait_for "起こし直したデーモンの 3 周目" grep -q 'cycle 3:' daemon2.log
kill "$D"; wait "$D" || true
echo "--- 起こし直したデーモンのログ"; cat daemon2.log
cmp -s alloc.json alloc.before && note "採番記録は 1 バイトも変わっていない（同じ owner に同じ値）"

step "4. 宣言から外す → 解放しない（§19.3「Release を呼ぶ経路はまだ作っていない」・§19.5）"
cp intent/main.yaml main.yaml.bak
sed '/# app2-begin/,/# app2-end/d' main.yaml.bak > intent/main.yaml
$S reconcile $ONE | tail -1
note "intent から app2 を外した。既定（ExplicitAbsentOnly）なので現実のレコードも残る"
grep '^_port.app2' example.com.zone
sed -i '/^_port.app2/d' example.com.zone
$S reconcile $ONE | tail -1
note "現実からも手で消した（List に現れない）。それでも採番記録の app2 はそのまま"
allocs alloc.json
cp main.yaml.bak intent/main.yaml
$S reconcile $ONE | grep -E '^\+|value|Reconcile'
note "宣言し直すと同じ 8001 が戻る"

step "5. プールが尽きる → そのリソースだけ waiting-allocation、ほかは収束する"
cat >> intent/main.yaml <<'EOF'
  - provider: dnszone
    id: _port.app3.example.com./TXT
    fields:
      name: _port.app3.example.com.
      type: TXT
      value:
        from_pool: svc-ports
  - provider: dnszone
    id: _port.app4.example.com./TXT
    fields:
      name: _port.app4.example.com.
      type: TXT
      value:
        from_pool: svc-ports
  - provider: dnszone
    id: api.example.com./A
    fields:
      name: api.example.com.
      type: A
      value: 192.0.2.20
EOF
rc $S reconcile $ONE
grep -E '^(_port|api)' example.com.zone
echo "--- plan / status での見え方"
$S plan $ONE 2>&1 | head -1
rc $S status $ONE 2>&1 | grep -E 'pending|DRIFTED|要約'
echo "--- 範囲を 8000-8003 に広げる"
sed -i 's/"8000-8002"/"8000-8003"/' intent/main.yaml
$S reconcile $ONE | grep -E '^\+|value|Reconcile'
echo "--- 使用中の 8003 を外す縮小（8000-8002）は拒否され、その周は何も進まない"
sed -i 's/"8000-8003"/"8000-8002"/' intent/main.yaml
rc $S reconcile $ONE
sed -i 's/"8000-8002"/"8000-8003"/' intent/main.yaml

step "6. 整数を A レコードに入れる（v1 のプールは整数だけ・§19.8）"
cat >> intent/main.yaml <<'EOF'
  - provider: dnszone
    id: app5.example.com./A
    fields:
      name: app5.example.com.
      type: A
      value:
        from_pool: svc-ports
EOF
sed -i 's/"8000-8003"/"8000-8004"/' intent/main.yaml
rc $S reconcile $ONE
grep '^app5' example.com.zone || note "app5 の A は書かれていない（ゾーンはほかのレコードごと読み込める形のまま）"

step "7. 同じホストの 4 つのプロセスが同時に払い出しても重ならない（flock）"
mkdir -p par
for i in 1 2 3 4; do
	mkdir -p "par/p$i/intent"
	cp "$HERE/initial/example.com.zone" "par/p$i/zone"
	{
		echo 'version: "1"'
		echo 'pools:'
		echo '  - name: bulk'
		echo '    range: "100-199"'
		echo 'resources:'
		for j in $(seq -w 1 25); do
			cat <<EOF
  - provider: dnszone
    id: _id.p$i-r$j.example.com./TXT
    fields:
      name: _id.p$i-r$j.example.com.
      type: TXT
      value:
        from_pool: bulk
EOF
		done
	} > "par/p$i/intent/main.yaml"
done
for i in 1 2 3 4; do
	$S reconcile --intent "par/p$i/intent" --zone "par/p$i/zone" --alloc-store par/alloc.json > "par/p$i.log" 2>&1 &
done
wait
for i in 1 2 3 4; do printf 'p%s: ' "$i"; tail -1 "par/p$i.log"; done
total=$(grep -c '"value":' par/alloc.json)
dups=$(grep -o '"value": [0-9]*' par/alloc.json | sort | uniq -d | wc -l)
inzone=$(cat par/p*/zone | grep -c 'IN TXT')
note "採番記録 $total 件・重複 $dups 件・4 つのゾーンの TXT $inzone 件（100 の範囲を 100 件で使い切る）"
echo "--- プロセスごとの値（交互に払い出している = 同時に走った）"
for i in 1 2 3 4; do
	printf 'p%s: %s\n' "$i" "$(grep -A1 "p$i-r" par/alloc.json | grep -o '"value": [0-9]*' | awk '{print $2}' | sort -n | tr '\n' ' ')"
done
for i in 1 2 3 4; do
	$S reconcile --intent "par/p$i/intent" --zone "par/p$i/zone" --alloc-store par/alloc.json > "par/p$i.again.log" 2>&1 &
done
wait
for i in 1 2 3 4; do printf 'もう一度 p%s: ' "$i"; tail -1 "par/p$i.again.log"; done
mkdir -p par/p5/intent; cp "$HERE/initial/example.com.zone" par/p5/zone
sed -n '1,12p' par/p1/intent/main.yaml | sed 's/p1-r01/p5-r01/g' > par/p5/intent/main.yaml
rc $S reconcile --intent par/p5/intent --zone par/p5/zone --alloc-store par/alloc.json

step "8. HA 最小形（active/standby + fencing・§19.8）"
mkdir -p ha/intent
cp "$HERE/initial/example.com.zone" ha/zone
# ha_intent <名前…>: ha/intent/main.yaml を、名前ごとに _port.<名前> の TXT を 1 つずつ持つ形に書き直す。
HA_APPS=""
ha_intent() {
	HA_APPS="$*"
	{
		echo 'version: "1"'
		echo 'pools:'
		echo '  - name: ha-ports'
		echo '    range: "9000-9009"'
		echo 'resources:'
		for n in "$@"; do
			cat <<EOF
  - provider: dnszone
    id: _port.$n.example.com./TXT
    fields:
      name: _port.$n.example.com.
      type: TXT
      value:
        from_pool: ha-ports
EOF
		done
	} > ha/intent/main.yaml
}
add_ha() { ha_intent $HA_APPS "$1"; }
ha_intent app1
A=http://127.0.0.1:$PORT_A; B=http://127.0.0.1:$PORT_B
health() { printf '  A: %s  B: %s\n' "$(curl -s $A/v1/health || echo down)" "$(curl -s $B/v1/health || echo down)"; }
$S allocator --store ha/alloc.json --listen 127.0.0.1:$PORT_A > ha/a.log 2>&1 &
PA=$!; PIDS="$PIDS $PA"
$S allocator --store ha/alloc.json --listen 127.0.0.1:$PORT_B --standby > ha/b.log 2>&1 &
PB=$!; PIDS="$PIDS $PB"
wait_for "A と B が答える" sh -c "curl -sf $A/v1/health && curl -sf $B/v1/health"
cat ha/a.log ha/b.log
health
$S --intent ha/intent --zone ha/zone --alloc-url "$A,$B" --interval 1s > ha/daemon.log 2>&1 &
D=$!; PIDS="$PIDS $D"
wait_for "app1 = 9000" zone_has ha/zone '^_port.app1.example.com. 300 IN TXT 9000$'
grep '^_port' ha/zone

echo "--- 8a. active の A を SIGKILL。既存のずれは直り続け、新しい要求だけ waiting-allocation"
kill -9 "$PA"; wait "$PA" 2>/dev/null || true
sed -i 's/IN TXT 9000$/IN TXT 1/' ha/zone
wait_for "app1 が 9000 に戻る" zone_has ha/zone '^_port.app1.example.com. 300 IN TXT 9000$'
note "allocator が落ちていても app1 のずれは Lookup キャッシュで直った"
add_ha app2
wait_for "app2 が waiting-allocation" grep -q 'app2.*waiting-allocation\|waiting-allocation.*app2' ha/daemon.log
grep -m1 'app2' ha/daemon.log
health

echo "--- 8b. standby の B を昇格（POST /v1/promote）→ app2 が払い出される"
curl -s -X POST $B/v1/promote; echo
wait_for "app2 = 9001" zone_has ha/zone '^_port.app2.example.com. 300 IN TXT 9001$'
grep '^_port' ha/zone

echo "--- 8c. 落ちた A を --standby なしで起こし直す → A が世代を上げて active、B は次の要求で fence されて降りる"
$S allocator --store ha/alloc.json --listen 127.0.0.1:$PORT_A > ha/a2.log 2>&1 &
PA=$!; PIDS="$PIDS $PA"
wait_for "A が答える" curl -sf $A/v1/health
cat ha/a2.log
health
add_ha app3
wait_for "app3 = 9002" zone_has ha/zone '^_port.app3.example.com. 300 IN TXT 9002$'
note "クライアントは B（fence された）から A へフェイルオーバーして払い出した"
health

echo "--- 8d. 使用中のポートで 3 つめを --standby なしで起動（打ち間違いの想定）"
rc $S allocator --store ha/alloc.json --listen 127.0.0.1:$PORT_A
health
add_ha app4
sleep 3
grep '^_port.app4' ha/zone && note "app4 も払い出された（起動に失敗したインスタンスは昇格していない）" || {
	note "app4 が払い出されない"; grep 'app4' ha/daemon.log | tail -1; }

echo "--- 8e. 採番ファイルを直に開く（--alloc-store）と、HA の世代に fence されて書けない（1 プール 1 権威）"
mkdir -p ha/direct; sed 's/app1/direct1/g' ha/intent/main.yaml | sed -n '1,12p' > ha/direct/main.yaml
cp "$HERE/initial/example.com.zone" ha/direct.zone
rc $S reconcile --intent ha/direct --zone ha/direct.zone --alloc-store ha/alloc.json

echo "--- 8f. 手で解放する（v1 で解放する唯一の手段: active への POST /v1/release）"
ha_intent app1 app2 app4
sleep 2
sed -i '/^_port.app3/d' ha/zone
curl -s -X POST -d '{"pool":"ha-ports","owner":"dnszone/_port.app3.example.com./TXT#value"}' $A/v1/release; echo
allocs ha/alloc.json
add_ha app5
wait_for "app5 が払い出される" zone_has ha/zone '^_port.app5.example.com. '
grep '^_port.app5' ha/zone
note "解放した 9002 は cooldown 中なので使われない"
echo "--- 8g. 解放した app3 を同じデーモンの下で宣言し直す"
add_ha app3
wait_for "app3 が戻る" zone_has ha/zone '^_port.app3.example.com. '
grep '^_port.app3' ha/zone
allocs ha/alloc.json
kill "$D" "$PA" "$PB"; wait "$D" "$PA" "$PB" || true

step "9. 採番記録を失う（§19.7 の「現実から逆引きして再構築する手順」は v1 に無い）"
cd "$W"
grep -E '^_port' example.com.zone
rm alloc.json
drop_res intent/main.yaml _port.app1.
note "採番記録を消し、同時に intent から app1 を外した（現実の app1 = 8000 は残る）"
$S reconcile $ONE 2>&1 | grep -E '^~|^\+|value|Reconcile|apply'
grep -E '^_port' example.com.zone
note "app2 が 8000 に付け替わり、現実に 8000 が 2 つある"

step "10. プール分割 by claim（2 つのシャードが別々の採番記録から disjoint なレンジで払い出す・§19.8）"
mkdir -p claim/intent
cp "$HERE/initial/example.com.zone" claim/zone
cat > claim/intent/main.yaml <<'EOF'
version: "1"
pools:
  - name: site-a-ports
    range: "8100-8199"
    claim:
      site: site-a
  - name: site-b-ports
    range: "8200-8299"
    claim:
      site: site-b
resources:
  - provider: dnszone
    id: _port.a1.example.com./TXT
    scope:
      site: site-a
    fields:
      name: _port.a1.example.com.
      type: TXT
      value:
        from_pool: site-a-ports
  - provider: dnszone
    id: _port.b1.example.com./TXT
    scope:
      site: site-b
    fields:
      name: _port.b1.example.com.
      type: TXT
      value:
        from_pool: site-b-ports
EOF
$S reconcile --intent claim/intent --zone claim/zone --alloc-store claim/site-a.json --claim site=site-a > claim/a.log 2>&1 &
CA=$!
$S reconcile --intent claim/intent --zone claim/zone --alloc-store claim/site-b.json --claim site=site-b > claim/b.log 2>&1 &
CB=$!
wait "$CA" "$CB"
grep -h -E 'claim|Reconcile' claim/a.log claim/b.log
grep '^_port' claim/zone
echo "--- site-a の採番記録"; allocs claim/site-a.json; grep -q site-b claim/site-a.json || note "site-a の記録に site-b のプールは無い"
sed -i 's/"8200-8299"/"8150-8299"/' claim/intent/main.yaml
rc $S plan --intent claim/intent --zone claim/zone --alloc-store claim/site-a.json

step "おわり（作業ディレクトリ: $W）"
