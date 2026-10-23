#!/bin/bash
# tier 別の Observe 頻度（observe_tiers）・BulkObserve・status とデーモンの状態（--daemon-status）を、
# 実バイナリの常駐デーモンと dnszone・nftables のファイルで通して流すスクリプト（docs/trial/observe/README.md）。
# 使い方: docs/trial/observe/run.sh [作業ディレクトリ]   （既定 /tmp/sakutei-trial-observe）
# 作業ディレクトリは毎回作り直す。リポジトリのファイルは書き換えない。外のサービスには繋がない。
# 確かめた観点ごとに OK／NG を出し、NG が 1 つでもあれば最後に exit 1 で終わる。
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../../.." && pwd)
W=${1:-/tmp/sakutei-trial-observe}

step() { printf '\n==== %s ====\n' "$*"; }
NG=0
ok() { echo "OK: $*"; }
ng() { echo "NG: $*"; NG=$((NG + 1)); }
check() { # check <説明> <コマンド...>
	local what=$1; shift
	if "$@"; then ok "$what"; else ng "$what"; fi
}
# wait_for <秒> <ファイル> <grep -E の式>：ファイルに式が出るまで待つ
wait_for() {
	local i=0
	while [ $i -lt $(($1 * 10)) ]; do
		grep -qE "$3" "$2" 2>/dev/null && return 0
		sleep 0.1; i=$((i + 1))
	done
	return 1
}
# 周 K の要約行（"cycle K: observed=N ..."）から N を出す。無ければ空。
obs_of() { grep -oE "cycle $2: observed=[0-9]+" "$1" | head -1 | sed 's/.*=//'; }
# 周 K の「間引いた数」。無ければ 0。
notdue_of() { local n; n=$(grep -oE "cycle $2: [0-9]+ resource\(s\) not due" "$1" | head -1 | awk '{print $3}'); echo "${n:-0}"; }
# 最後に要約を出した周の番号
last_cycle() { grep -oE 'cycle [0-9]+: observed=' "$1" | tail -1 | awk '{print $2}' | tr -d :; }
# wait_cycle <ログ> <K>：周 K の要約が出るまで待つ
wait_cycle() { wait_for 30 "$1" "cycle $2: observed="; }
# 式 <re> に合う行の後で最初に要約を出した周（DriftEvent が出た周）
cycle_after() { awk -v re="$2" 'f && /cycle [0-9]+: observed=/ {match($0, /cycle [0-9]+:/); print substr($0, RSTART+6, RLENGTH-7); exit} $0 ~ re {f=1}' "$1"; }
# ログの抜粋（毎周の converged 行と not due 行は省く）
excerpt() { grep -vE 'converged \(no changes\)$|not due for Observe' "$1" | sed 's/^/    /'; }

PIDS=""
cleanup() { for p in $PIDS; do kill "$p" 2>/dev/null || true; done; wait 2>/dev/null || true; }
trap cleanup EXIT

rm -rf "$W"
mkdir -p "$W/logs"
cp "$HERE/initial/example.com.zone" "$HERE/initial/firewall.rules" "$W/"
cp -r "$HERE/intent" "$W/intent"

step "0. ビルド"
(cd "$REPO" && go build -o "$W/sakutei" ./cmd/sakutei)
cd "$W"
S=$W/sakutei
ONE="--intent intent --zone example.com.zone --ruleset firewall.rules"
STATUS="$S status $ONE --daemon-status ds.json"
# start <ログ名> <追加の引数...>：常駐デーモンを 1 秒周期で起こす
start() {
	local name=$1; shift
	"$S" "$@" --interval 1s >>"logs/$name.log" 2>&1 &
	PID=$!; PIDS="$PIDS $PID"
}
stop() { kill "$1" 2>/dev/null || true; wait "$1" 2>/dev/null || true; }
L=logs/daemon.log

step "1. BulkObserve の経路が使われている（§5.7）"
start daemon $ONE --daemon-status ds.json
wait_cycle $L 3
check "dnszone は BulkObserve で観測する（ログに 1 行）" test "$(grep -c 'provider dnszone: Observe via BulkObserve' $L)" = 1
check "nftables も BulkObserve で観測する（ログに 1 行）" test "$(grep -c 'provider nftables: Observe via BulkObserve' $L)" = 1
check "per-resource Read の経路は出ない" bash -c "! grep -q 'Observe via per-resource Read' $L"
grep 'Observe via' $L

step "2. observe_tiers の間引き（§16.5）"
wait_cycle $L 13
echo "周  observed  間引いた数"
for c in $(seq 1 13); do printf '%3d  %8s  %10s\n' "$c" "$(obs_of $L $c)" "$(notdue_of $L $c)"; done
# 初期の現実は intent と一致している：周 1 は全部、そこから stable の 3 つは 4 周に 1 回（周 5・9・13）。
EXPECT="4 1 1 1 4 1 1 1 4 1 1 1 4"
GOT=$(for c in $(seq 1 13); do printf '%s ' "$(obs_of $L $c)"; done | sed 's/ $//')
check "observed は周 1・5・9・13 に 4（全部）、他の周は 1（tier なしの www だけ）: $GOT" test "$GOT" = "$EXPECT"
check "間引いた周は「3 resource(s) not due for Observe」を出す" test "$(notdue_of $L 2)" = 3
check "間引いた周も converged と言う（間引きは unobservable ではない）" grep -q 'cycle 2: observed=1 converged (no changes)' $L

step "3. 間引いている間に手で変えたずれは何周後に拾われるか"
# stable を Observe した周（周 K、observed=4）の直後に api を変える：次に stable を見るのは K+4 周。
K=$(last_cycle $L)
while [ "$(obs_of $L "$K")" != 4 ]; do K=$((K + 1)); wait_cycle $L $K; done
sed -i 's/^api\t300 IN A 192.0.2.30/api\t300 IN A 198.51.100.30/' example.com.zone
echo "周 $K（stable を Observe した周）の後で api を 198.51.100.30 に変えた"
# 3b. その間に status（毎回 Read で全部を観測する）を見る：デーモンの間引きと関係なくずれが見える
sleep 0.3
$STATUS >status-3b.txt 2>&1 || true
check "status は間引きと関係なく、すぐに api のずれを出す" grep -q 'DRIFTED dnszone//api.example.com./A' status-3b.txt
cat status-3b.txt
# status はずれた値を出さないので、値は plan（同じく per-resource Read）で見る
$S plan $ONE >plan-3b.txt 2>&1 || true
cat plan-3b.txt
wait_for 15 $L 'DriftEvent: dnszone//api.example.com./A' || true
wait_cycle $L $((K + 5))
D=$(cycle_after $L 'DriftEvent: dnszone//api.example.com./A')
echo "api のずれを拾った周: $D（変えた後 $((D - K)) 周目）"
check "tier stable（observe_every 4）のずれは 4 周目（周 $((K + 4))）に拾われる" test "$D" = $((K + 4))
check "拾った周に元に戻す（ゾーンが 192.0.2.30 に戻る）" grep -qE '^api\s+300 IN A 192.0.2.30' example.com.zone
check "DriftEvent の値（BulkObserve で見た値）が plan の値（Read で見た値）と同じ 198.51.100.30" \
	bash -c "grep 'DriftEvent: dnszone//api' $L | grep -q 'now \"198.51.100.30\"' && grep -q '~ value  *\"198.51.100.30\" → \"192.0.2.30\"' plan-3b.txt"
wait_cycle $L $((D + 2))
check "直した次の周（$((D + 1))）は api を間引かない（未収束は毎周、observed=2）" test "$(obs_of $L $((D + 1)))" = 2
check "その次の周（$((D + 2))）からまた間引く（observed=1）" test "$(obs_of $L $((D + 2)))" = 1
# 3c. tier なしの www は次の周に拾う
K2=$(last_cycle $L)
sed -i 's/^www\t300 IN A 192.0.2.10/www\t300 IN A 198.51.100.10/' example.com.zone
echo "周 $K2 の後で www（tier なし）を 198.51.100.10 に変えた"
wait_cycle $L $((K2 + 2))
D2=$(cycle_after $L 'DriftEvent: dnszone//www.example.com./A')
echo "www のずれを拾った周: $D2"
check "tier なしのずれは次の周（周 $((K2 + 1))）に拾われる" test "$D2" = $((K2 + 1))
# 3d. nftables（stable の唯一のリソース）
K3=$(last_cycle $L)
while [ "$(obs_of $L "$K3")" = 1 ] || [ -z "$(obs_of $L "$K3")" ]; do K3=$((K3 + 1)); wait_cycle $L $K3; done
sed -i 's/tcp dport 443 accept/tcp dport 4443 accept/' firewall.rules
echo "周 $K3（nftables を Observe した周）の後で ruleset の 443 を 4443 に変えた"
wait_cycle $L $((K3 + 5))
D3=$(cycle_after $L 'DriftEvent: nftables//inet/filter/sakutei_allow')
echo "nftables のずれを拾った周: $D3"
check "nftables のずれも 4 周目（周 $((K3 + 4))）に拾われ、直る" bash -c "test '$D3' = $((K3 + 4)) && grep -q 'tcp dport 443 accept' firewall.rules"

step "4. intent を変えたら間引かない（cause A は次の周）"
K4=$(last_cycle $L)
sed -i 's/value: 192.0.2.40/value: 192.0.2.44/' intent/main.yaml
echo "周 $K4 の後で intent の mail（stable）を 192.0.2.44 に変えた"
wait_for 10 example.com.zone '192.0.2.44' || true
wait_cycle $L $((K4 + 2))
check "mail は間引かれず、変えた次の周（周 $((K4 + 1))）に Apply される" \
	bash -c "grep -qE '^mail\s+300 IN A 192.0.2.44' example.com.zone && grep -qE 'cycle $((K4 + 1)): observed=[0-9]+ applied=0 launched=1' $L"

step "5. status と --daemon-status：デーモンの状態"
$STATUS >status-5.txt 2>&1 || true
cat status-5.txt
check "status は適合（drift なし）を出す" grep -q '適合（drift なし）' status-5.txt
check "status に --daemon-status のデーモンの周と連続スキップ回数が出る" grep -qE 'デーモン（cycle [0-9]+, .*consecutive_unreachable_skips=0（上限 3）' status-5.txt
echo "--- ds.json"; cat ds.json

step "6. BulkObserve の complete-or-error：ゾーンファイルが壊れたら（§5.7）"
cp example.com.zone zone.good
K6=$(last_cycle $L)
echo 'oops this is not a record' >>example.com.zone
cp example.com.zone zone.broken
wait_cycle $L $((K6 + 5))
check "壊れたゾーンは bulk の呼び出し全体のエラー（complete-or-error）と出る" \
	grep -q 'bulk observe (complete-or-error, §5.7): transient: dnszone: BulkObserve: parse zone' $L
check "Read に回して部分的に答えない（dnszone: Read: のエラーは出ない）" bash -c "! grep -q 'dnszone: Read:' $L"
check "その間ゾーンファイルを書き換えない" cmp -s example.com.zone zone.broken
check "nftables は同じ周も収束を続ける（unobservable は dnszone だけ）" bash -c "! grep -q 'observe nftables' $L"
rc=0; $STATUS >status-6.txt 2>&1 || rc=$?; echo "（status の終了コード: $rc）" >>status-6.txt
cat status-6.txt
grep 'cycle '"$((K6 + 2))"':' $L | sed 's/^/    /'
cp zone.good example.com.zone
K6b=$(last_cycle $L)
wait_cycle $L $((K6b + 2))
check "直すと次の周から収束に戻る" grep -qE "cycle $((K6b + 2)): observed=[0-9]+ converged" $L
stop $PID

step "7. BulkObserve と Read は等しい（SDK の BulkEquivalenceTester を実ファイルに）"
(cd "$REPO" && go build -tags trial -o "$W/bulkeq" ./docs/trial/observe/bulkeq)
echo "--- 収束した作業ディレクトリのファイル"
check "収束した現実で BulkObserve ≡ Read" ./bulkeq --zone example.com.zone --ruleset firewall.rules
echo "--- 手で書いた癖のあるファイル（messy/。作業ディレクトリに写して読む：読むときも隣に <file>.lock ができる）"
mkdir -p messy && cp "$HERE/messy/example.com.zone" "$HERE/messy/firewall.rules" messy/
check "癖のあるゾーンと ruleset でも BulkObserve ≡ Read" ./bulkeq --zone messy/example.com.zone --ruleset messy/firewall.rules

step "8. 再起動で間引きの記録は消える（S3）"
L8=logs/restart.log
start restart $ONE --daemon-status ds.json
wait_cycle $L8 3
check "起こし直した周 1 は全部を Observe する（observed=4）、周 2 からまた間引く" \
	bash -c "test '$(obs_of $L8 1)' = 4 && test '$(obs_of $L8 2)' = 1"
step "9. 周がエラーで終わり続けると --daemon-status に出る（#104 で足した）"
cp intent/main.yaml main.yaml.good
printf 'version: "1"\nresources: {broken\n' >intent/main.yaml
wait_for 15 $L8 'cycle [0-9]+ error: load intent' || true
sleep 3
$STATUS --intent "$HERE/intent" >status-9.txt 2>&1 || true
sed -n '/デーモン/,$p' status-9.txt
check "status が周のエラーの連続回数と最後のエラーを警告する" grep -qE '警告: 周がエラーで終わり続けている consecutive_error_cycles=[0-9]+ .*load intent' status-9.txt
cp main.yaml.good intent/main.yaml
K9=$(grep -oE 'cycle [0-9]+ error' $L8 | tail -1 | awk '{print $2}')
wait_cycle $L8 $((K9 + 1))
sleep 0.3
check "直すとエラーの連続回数は 0 に戻る" grep -q '"consecutive_error_cycles": 0' ds.json
step "10. デーモンが止まると、status は古いファイルだと警告する（#104 で足した）"
stop $PID
sleep 4
$STATUS >status-10.txt 2>&1 || true
sed -n '/デーモン/,$p' status-10.txt
check "止まって周期の 3 倍を過ぎると「更新されていない」と警告する" grep -q '数倍のあいだ更新されていない' status-10.txt

step "11. 外部入力が届かず飛ばした周の数（#15、§9.2）"
mkdir -p inv/intent
cp "$HERE/initial/example.com.zone" inv/example.com.zone
cat >inv/intent/main.yaml <<'EOF'
version: "1"
resources:
  - provider: dnszone
    id: www.example.com./A
    fields:
      name: www.example.com.
      type: A
      value: 192.0.2.10
      ttl: "300"
EOF
LI=logs/inventory.log
INV="--intent inv/intent --zone inv/example.com.zone --inventory-file inv/inventory.yaml"
start inventory $INV --daemon-status inv/ds.json
wait_for 15 $LI 'consecutive_unreachable_skips=4' || true
check "inventory が最初から無い周は飛ばし、連続回数を毎周出す" grep -q 'cycle 4: SKIPPED (external input unreachable, no previous value): consecutive_unreachable_skips=4' $LI
check "上限 3 で ExternalInputUnreachableProlonged を 1 回だけ出す" test "$(grep -c 'EVENT ExternalInputUnreachableProlonged' $LI)" = 1
$S status --intent inv/intent --zone inv/example.com.zone --daemon-status inv/ds.json >status-11.txt 2>&1 || true
sed -n '/デーモン/,$p' status-11.txt
check "status に飛ばした周の数と Prolonged の警告が出る" \
	bash -c "grep -qE 'consecutive_unreachable_skips=[4-9]（上限 3）' status-11.txt && grep -q 'ExternalInputUnreachableProlonged' status-11.txt"
check "飛ばした周は Observe も Apply もしない（ゾーンはそのまま）" cmp -s inv/example.com.zone "$HERE/initial/example.com.zone"
cat >inv/inventory.yaml.tmp <<'EOF'
version: "1"
resources:
  - provider: dnszone
    id: h1.example.com./A
    fields:
      name: h1.example.com.
      type: A
      value: 192.0.2.101
      ttl: "300"
EOF
mv inv/inventory.yaml.tmp inv/inventory.yaml
check "inventory が届くと、飛ばした周の数を出して収束を再開する" wait_for 10 $LI 'external input reachable again after [0-9]+ skipped cycle'
check "inventory の h1 が入る" wait_for 10 inv/example.com.zone '^h1.example.com.'
sleep 1.2
$S status --intent inv/intent --zone inv/example.com.zone --daemon-status inv/ds.json >status-11b.txt 2>&1 || true
sed -n '/デーモン/,$p' status-11b.txt
check "status の連続スキップ回数は 0 に戻る" grep -q 'consecutive_unreachable_skips=0（上限 3）' status-11b.txt
stop $PID

step "ログの抜粋"
for f in daemon restart inventory; do
	echo "--- logs/$f.log"; excerpt "logs/$f.log"
done

step "おわり（作業ディレクトリ: $W、NG=$NG）"
[ "$NG" -eq 0 ]
