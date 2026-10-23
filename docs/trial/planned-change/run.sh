#!/bin/sh
# planned_change（§13.3）を手で試す手順を、1 回で通して流すスクリプト（docs/trial/planned-change/README.md）。
# 使い方: docs/trial/planned-change/run.sh [作業ディレクトリ]   （既定 /tmp/sakutei-trial-pc）
# 作業ディレクトリは毎回作り直す。リポジトリのファイルは書き換えない。
# 宣言の window は「いま」を挟むように（または外すように）その場で書く。
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../../.." && pwd)
W=${1:-/tmp/sakutei-trial-pc}

step() { printf '\n==== %s ====\n' "$*"; }
# win <開始のずれ> <終了のずれ>：date -d に渡す相対時刻（例 "-1 hour" "+1 hour"）で窓を作る（UTC）。
win() { echo "$(date -u -d "$1" '+%Y-%m-%d %H:%M') .. $(date -u -d "$2" '+%Y-%m-%d %H:%M')"; }
# 予算の判定・レーン・宣言の Event の行だけを残す（plan の差分の本文は落とす）。
show() { grep -E 'PlannedChange|BUDGET|budget:|prune|Reconcile:|note:|cycle [0-9]+:|HALT' || true; }

rm -rf "$W"
mkdir -p "$W"
step "0. ビルド"
(cd "$REPO" && go build -o "$W/sakutei" ./cmd/sakutei)
cd "$W"
S=$W/sakutei

# ---------------------------------------------------------------------------
# A. dnszone・nftables：kind: update のレーン
# ---------------------------------------------------------------------------
mkdir -p A/intent
cd A
cp "$HERE/initial/example.com.zone" "$HERE/initial/firewall.rules" .
# 3 件の A レコード（scope zone=example）を書き換え、2 本の chain（scope fw=edge）に 443 を足す。
cp "$HERE/intent/resources.yaml" resources.yaml
ONE="--intent intent --zone example.com.zone --ruleset firewall.rules --max-changes-per-cycle 1"
reset() { cp "$HERE/initial/example.com.zone" "$HERE/initial/firewall.rules" .; rm -f audit.jsonl; }
# decl <ファイル>：resources.yaml に planned_change の YAML（標準入力）を足して intent にする。
decl() { { cat resources.yaml; cat; } > intent/main.yaml; }

step "A1. 宣言なし：5 件の UPDATE が通常予算（1 周 1 件）で止まる"
decl </dev/null
$S reconcile $ONE 2>&1 | show
grep -c '192.0.2.' example.com.zone | sed 's/^/変わっていない A レコード: /'

step "A2. kind: update・scope zone=example・max_count 3 を宣言。--audit-log なしの one-shot はレーンを開かない"
decl <<EOF
planned_change:
  scope:
    zone: example
  kind: update
  max_count: 3
  window: "$(win '-1 hour' '+1 hour')"
  applicant: carol
  approval:
    pr: 7
    approvers:
      - alice
EOF
$S reconcile $ONE 2>&1 | show

step "A3. --audit-log を付ける：zone=example の 3 件はレーンで通り、宣言外の fw=edge の 2 件は通常予算で止まる"
$S reconcile $ONE --audit-log audit.jsonl 2>&1 | show
grep ' A ' example.com.zone
grep 'sakutei_' firewall.rules
echo '--- S4（audit.jsonl）の kind'; sed 's/.*"kind":"\([^"]*\)".*/\1/' audit.jsonl

step "A4. 同じ宣言のまま zone をもう一度ずらす：max_count 3 を使い切っているので通常予算に戻る"
sed -i 's/198.51.100.\([123]\)/192.0.2.\1/' example.com.zone
$S reconcile $ONE --audit-log audit.jsonl 2>&1 | show

step "A5. 承認が足りない：承認者なし（team は 1 人要る）・enterprise で申請者本人を数える・役割が合わない、のどれも開かない"
reset
decl <<EOF
planned_change:
  scope:
    zone: example
  kind: update
  max_count: 3
  window: "$(win '-1 hour' '+1 hour')"
  applicant: carol
EOF
$S reconcile $ONE --audit-log audit.jsonl 2>&1 | show
reset
decl <<EOF
profile: enterprise
planned_change:
  scope:
    zone: example
  kind: update
  max_count: 3
  window: "$(win '-1 hour' '+1 hour')"
  applicant: carol
  approval:
    pr: 7
    approvers:
      - carol
      - alice
EOF
echo "--- enterprise（2-of-2・申請者≠承認者）に carol（申請者）と alice"
$S reconcile $ONE --audit-log audit.jsonl 2>&1 | show
reset
decl <<EOF
planned_change:
  scope:
    zone: example
  kind: update
  max_count: 3
  window: "$(win '-1 hour' '+1 hour')"
  applicant: carol
  approval:
    pr: 7
    approvers:
      - alice
  requirements:
    roles:
      - dns-owner
EOF
echo "--- 宣言の requirements が役割 dns-owner を求める。alice は役割を名乗っていない"
$S reconcile $ONE --audit-log audit.jsonl 2>&1 | show

step "A6. 期限の外：窓がまだ来ていない宣言・過ぎた宣言は効かない"
reset
decl <<EOF
planned_change:
  - scope:
      zone: example
    kind: update
    max_count: 3
    window: "$(win '+1 hour' '+2 hour')"
    applicant: carol
    approval:
      pr: 7
      approvers:
        - alice
  - scope:
      zone: example
    kind: update
    max_count: 3
    window: "$(win '-2 hour' '-1 hour')"
    applicant: carol
    approval:
      pr: 8
      approvers:
        - alice
EOF
$S reconcile $ONE --audit-log audit.jsonl 2>&1 | show

step "A7. 範囲の外：scope zone=other の宣言は zone=example の変更に効かない"
reset
decl <<EOF
planned_change:
  scope:
    zone: other
  kind: update
  max_count: 3
  window: "$(win '-1 hour' '+1 hour')"
  applicant: carol
  approval:
    pr: 7
    approvers:
      - alice
EOF
$S reconcile $ONE --audit-log audit.jsonl 2>&1 | show

step "A8. dnszone の削除は planned_change で宣言できない（見つけたこと 1）"
reset
cat > intent/main.yaml <<EOF
resources:
  - provider: dnszone
    id: www.example.com./A
    scope:
      zone: example
    deletion_policy: PruneWithinScope
    fields:
      name: www.example.com.
      type: A
      value: 192.0.2.10
      ttl: "300"
EOF
echo "--- scope zone=example の PruneWithinScope：www の CREATE だけで、ゾーンにある intent 外の a1〜a3 を消す計画が出ない"
$S plan --intent intent --zone example.com.zone 2>&1 | grep -E "^[-+~] |Converged" || true
sed -i '/scope:/,/zone: example/d' intent/main.yaml
echo "--- scope を外す（root scope）と a1〜a3 が DELETE になる"
$S plan --intent intent --zone example.com.zone 2>&1 | grep -E "^[-+~] |Converged" || true
{ cat intent/main.yaml; cat <<EOF
planned_change:
  kind: delete
  max_count: 3
  window: "$(win '-1 hour' '+1 hour')"
EOF
} > main.yaml.tmp && mv main.yaml.tmp intent/main.yaml
echo "--- それを覆う planned_change は scope を空にできない"
$S plan --intent intent --zone example.com.zone 2>&1 | tail -2 || true
cd "$W"

# ---------------------------------------------------------------------------
# B. fake provider：scope の付いた現実で delete・reimage・kind 不一致・急減判定
#    （dnszone・nftables の List は root scope しか返さないので、削除のレーンは fake で試す。見つけたこと 1）
# ---------------------------------------------------------------------------
# seed <state> <id>=<k>:<v>[,<k>:<v>]... ：fake の現実（state ファイル）を書く。List はこの scope を付けて返す。
seed() {
	python3 - "$@" <<'PY'
import json, sys
path, items = sys.argv[1], sys.argv[2:]
res, scopes = {}, {}
for it in items:
    rid, labels = it.split("=", 1)
    res[rid] = {"fields": {"name": rid, "size": "1"}}
    scopes[rid] = dict(kv.split(":", 1) for kv in labels.split(","))
json.dump({"resources": res, "scopes": scopes, "pruneable_capability": True, "provider_version": "trial-1"}, open(path, "w"), indent=2)
PY
}
# ids <state>：現実にある ID を 1 行に並べる。
ids() { python3 -c 'import json,sys; print(" ".join(sorted(json.load(open(sys.argv[1]))["resources"])))' "$1"; }
# want <id> <k>:<v>[,...]：PruneWithinScope の desired 1 件（YAML）。
want() {
	echo "  - provider: fake"
	echo "    id: $1"
	echo "    deletion_policy: PruneWithinScope"
	echo "    scope:"
	echo "$2" | tr ',' '\n' | sed 's/^\([^:]*\):\(.*\)$/      \1: \2/'
	echo "    fields:"
	echo "      name: $1"
	echo "      size: \"1\""
}
# pc <kind> <max_count> <k>:<v>：承認者 alice つき・いまを挟む窓の planned_change（YAML）。
pc() {
	echo "planned_change:"
	echo "  scope:"
	echo "$3" | tr ',' '\n' | sed 's/^\([^:]*\):\(.*\)$/    \1: \2/'
	echo "  kind: $1"
	echo "  max_count: $2"
	echo "  window: \"$(win '-1 hour' '+1 hour')\""
	echo "  applicant: carol"
	echo "  approval:"
	echo "    pr: 42"
	echo "    approvers:"
	echo "      - alice"
}

mkdir -p "$W/B/intent"
cd "$W/B"
FAKE="--intent intent --state state.json --max-destructive-per-cycle 1"
seedB() {
	rm -f audit.jsonl
	seed state.json keep-decom=pool:decom decom-1=pool:decom decom-2=pool:decom decom-3=pool:decom \
		decom-4=pool:decom decom-5=pool:decom keep-other=pool:other stray-1=pool:other stray-2=pool:other
}
desiredB() { echo 'version: "1"'; echo 'resources:'; want keep-decom pool:decom; want keep-other pool:other; }

step "B1. 宣言なし：decom-1〜5 と stray-1〜2 の 7 件の DELETE が通常予算（1 周 1 件）で止まる"
seedB
desiredB > intent/main.yaml
$S reconcile $FAKE --audit-log audit.jsonl 2>&1 | show
echo "現実: $(ids state.json)"

step "B2. kind: delete・scope pool=decom・max_count 5：decom の 5 件はレーンで消え、宣言外の stray-1〜2 は止まる"
{ desiredB; pc delete 5 pool:decom; } > intent/main.yaml
$S reconcile $FAKE --audit-log audit.jsonl 2>&1 | show
echo "現実: $(ids state.json)"

step "B3. kind の不一致：scope pool=decom に kind: update を宣言しても DELETE はレーンに入らない"
seedB
{ desiredB; pc update 5 pool:decom; } > intent/main.yaml
$S reconcile $FAKE --audit-log audit.jsonl 2>&1 | show
echo "現実: $(ids state.json)"

step "B4. kind: reimage：同じ slot の DELETE+CREATE の対はレーンで通り（破壊予算に delete と数えない）、対の無い DELETE は kind 不一致"
rm -f audit.jsonl
seed state.json hpc-head=pool:hpc node-1-v1=pool:hpc,slot:1 node-2-v1=pool:hpc,slot:2 node-3-v1=pool:hpc,slot:3 \
	node-4-v1=pool:hpc,slot:4 node-5-v1=pool:hpc,slot:5
desiredHPC() {
	echo 'version: "1"'; echo 'resources:'
	want hpc-head pool:hpc
	for i in 1 2 3; do want node-$i-v2 pool:hpc,slot:$i; done
}
desiredHPC > intent/main.yaml
# 1 周 2 件・窓 2 件の破壊予算。reimage の対を delete と数えれば 5 件で超える。
HPC="--intent intent --state state.json --max-destructive-per-cycle 2 --max-destructive-per-window 2 --audit-log audit.jsonl"
echo "--- 宣言なし：DELETE 5・CREATE 3 が予算（2 件）を超えて止まる"
$S reconcile $HPC 2>&1 | show
echo "現実: $(ids state.json)"
{ desiredHPC; pc reimage 3 pool:hpc; } > intent/main.yaml
echo "--- kind: reimage・scope pool=hpc・max_count 3：slot 1〜3 の対はレーン、対の無い node-4・node-5 の 2 件だけが通常予算（2 件に収まる）"
$S reconcile $HPC 2>&1 | show
echo "現実: $(ids state.json)"
echo "--- 同じ宣言で、対の無い DELETE が予算を超えると止まる（1 周 1 件にする）"
seed state.json hpc-head=pool:hpc node-1-v2=pool:hpc,slot:1 node-2-v2=pool:hpc,slot:2 node-3-v2=pool:hpc,slot:3 \
	node-4-v1=pool:hpc,slot:4 node-5-v1=pool:hpc,slot:5
$S reconcile $FAKE --audit-log audit.jsonl 2>&1 | show
echo "現実: $(ids state.json)"

step "B5. 急減判定：常駐ループでレーンが消した分は List 急減から差し引き、宣言外の減少は halt する"
rm -f audit.jsonl
seed state.json keep-decom=pool:decom decom-1=pool:decom decom-2=pool:decom decom-3=pool:decom decom-4=pool:decom \
	decom-5=pool:decom decom-6=pool:decom decom-7=pool:decom decom-8=pool:decom \
	ext-1=pool:ext ext-2=pool:ext ext-3=pool:ext ext-4=pool:ext
{ echo 'version: "1"'; echo 'resources:'; want keep-decom pool:decom; pc delete 8 pool:decom; } > intent/main.yaml
$S --intent intent --state state.json --max-destructive-per-cycle 1 --interval 1s --audit-log audit.jsonl > daemon.log 2>&1 &
PID=$!
sleep 5
echo "--- 13 件 → $(ids state.json | wc -w) 件（decom-1〜8 をレーンで消した。61% の減少）"
echo "現実: $(ids state.json)"
echo "--- ext-1〜3 を手で消す（5 件 → 2 件。60% の減少。宣言の外）"
seed state.json keep-decom=pool:decom ext-4=pool:ext
sleep 4
kill $PID; wait $PID 2>/dev/null || true
show < daemon.log | grep -v 'converged (no changes)' || true
echo "（converged (no changes) の周: $(grep -c 'converged (no changes)' daemon.log) 周）"

step "B6. 常駐ループと窓：開始前は通常予算で止まり、窓に入った周にレーンで通り、終わった周に失効する"
rm -f audit.jsonl
seed state.json keep-decom=pool:decom decom-1=pool:decom decom-2=pool:decom decom-3=pool:decom
T0=$(date -u -d '+4 sec' '+%Y-%m-%dT%H:%M:%SZ'); T1=$(date -u -d '+8 sec' '+%Y-%m-%dT%H:%M:%SZ')
{ echo 'version: "1"'; echo 'resources:'; want keep-decom pool:decom; pc delete 3 pool:decom | sed "s/^  window: .*/  window: \"$T0 .. $T1\"/"; } > intent/main.yaml
grep window intent/main.yaml
$S --intent intent --state state.json --max-destructive-per-cycle 1 --interval 1s > daemon6.log 2>&1 &
PID=$!
sleep 11
kill $PID; wait $PID 2>/dev/null || true
grep -E 'PlannedChange|BUDGET HALTED|pruned=|planned_change lane' daemon6.log | sed 's/(used .* before this cycle)/(…)/' || true
echo "現実: $(ids state.json)"

cd "$W"
step "おわり（作業ディレクトリ: $W）"
