#!/bin/bash
# DR モード（DR plan のプリステージと発動・書込 break-glass の判定と署名・事後合流）を、
# 実バイナリと本物の git（ローカルの bare リポジトリを中央に、そのミラーを大阪の拠点に）で
# 通して流すスクリプト（docs/trial/dr-mode/README.md）。
# 使い方: docs/trial/dr-mode/run.sh [作業ディレクトリ]   （既定 /tmp/sakutei-trial-dr）
# 作業ディレクトリは毎回作り直す。リポジトリのファイルは書き換えない。外のサービスには繋がない。
# 確かめた観点ごとに OK／NG を出し、NG が 1 つでもあれば最後に exit 1 で終わる。
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../../.." && pwd)
W=${1:-/tmp/sakutei-trial-dr}

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
# 現実（fake provider の state）
field() { python3 -c "import json,sys;r=json.load(open('$W/reality.json'))['resources'].get(sys.argv[1]);print(r['fields'][sys.argv[2]] if r else '-')" "$1" "$2"; }
set_field() {
	python3 - "$W/reality.json" "$1" "$2" "$3" <<'EOF'
import json, os, sys
p, rid, k, v = sys.argv[1:]
d = json.load(open(p)); d["resources"][rid]["fields"][k] = v
open(p + ".tmp", "w").write(json.dumps(d, indent=2)); os.rename(p + ".tmp", p)
EOF
}
wait_field() { # wait_field <秒> <id> <field> <値>
	local i=0
	while [ $i -lt $(($1 * 10)) ]; do
		[ "$(field "$2" "$3")" = "$4" ] && return 0
		sleep 0.1; i=$((i + 1))
	done
	return 1
}
stays_field() { # stays_field <秒> <id> <field> <値>：その間ずっと値のまま
	local i=0
	while [ $i -lt $(($1 * 10)) ]; do
		[ "$(field "$2" "$3")" = "$4" ] || return 1
		sleep 0.1; i=$((i + 1))
	done
}
# ログの抜粋（毎周の converged 行は省く）
excerpt() { grep -vE 'converged \(no changes\)' "$1" | sed 's/^/    /'; }

PIDS=""
cleanup() {
	for p in $PIDS; do kill "$p" 2>/dev/null || true; done
	[ -n "${PID_daemon:-}" ] && kill -CONT "$PID_daemon" 2>/dev/null || true
	wait 2>/dev/null || true
	chmod -R u+rwX "$W" 2>/dev/null || true
}
trap cleanup EXIT

rm -rf "$W"
mkdir -p "$W/logs" "$W/tokyo" "$W/osaka" "$W/offline" "$W/s4"
cp "$HERE/initial/reality.json" "$W/"

step "0. ビルドと、中央 git（東京の bare）・大阪のミラーの用意"
(cd "$REPO" && go build -o "$W/sakutei" ./cmd/sakutei)
cd "$W"
S=$W/sakutei
export GIT_AUTHOR_NAME=trial GIT_AUTHOR_EMAIL=trial@example.com GIT_COMMITTER_NAME=trial GIT_COMMITTER_EMAIL=trial@example.com
CENTRAL=$W/tokyo/central.git                  # 中央 git は東京にある（被災する）
MIRROR=$W/osaka/mirror.git                    # 大阪のローカルミラー（§25.2）
S4=$W/s4/audit.jsonl                          # S4 監査（拠点の外。append-only）
git init -q --bare -b main "$CENTRAL"
git clone -q "$CENTRAL" work 2>/dev/null      # 平時の人の手元（ここでコミットして push = PR のマージ）
cp -r "$HERE/repo/." work/
git -C work add -A && git -C work commit -qm "initial intent: tokyo, osaka and the prestaged DR plan"
git -C work push -q origin HEAD:main
merge() { # merge <メッセージ>：手元の変更をコミットして中央へ push する（ミラーは常駐で追う）
	git -C "$W/work" add -A && git -C "$W/work" commit -qm "$1" && git -C "$W/work" push -q origin HEAD:main
	echo "merged: $1 ($(git -C "$W/work" rev-parse --short HEAD))"
}
mirror_main() { git -C "$MIRROR" rev-parse refs/heads/main; }
central_main() { git -C "$CENTRAL" rev-parse refs/heads/main; }
# wait_mirror <秒>：ミラーが中央に追いつくまで待つ
wait_mirror() {
	local i=0
	while [ $i -lt $(($1 * 10)) ]; do
		[ "$(mirror_main)" = "$(central_main)" ] && return 0
		sleep 0.1; i=$((i + 1))
	done
	return 1
}
$S mirror --upstream "$CENTRAL" --dir "$MIRROR" --once
$S mirror --upstream "$CENTRAL" --dir "$MIRROR" --interval 1s >>logs/mirror.log 2>&1 & PID_mirror=$!; PIDS="$PIDS $!"

# 拠点のデーモン。現実は 1 つの state（fake provider）、fencing の世代は共有のストア（§15.2。拠点の外）。
start() { # start <名前> <pull 元> <追加の引数...>
	local name=$1 src=$2; shift 2
	"$S" --intent-repo "$src" --workdir "$W/hosts/$name" --shard "$name" --shard-id "$name" \
		--git-poll 1s --interval 1s --state "$W/reality.json" --fencing-store "$W/fencing.json" "$@" >>"$W/logs/$name.log" 2>&1 &
	eval "PID_$(echo "$name" | tr -c 'a-zA-Z0-9\n' _)=$!"
	PIDS="$PIDS $!"
}
pid_of() { eval "echo \$PID_$(echo "$1" | tr -c 'a-zA-Z0-9\n' _)"; }
stop() { local p; p=$(pid_of "$1"); kill "$p" 2>/dev/null || true; wait "$p" 2>/dev/null || true; }
holder() { $S fence --store "$W/fencing.json" | awk -v s="$1" '$1 == s { print $3 }'; }
wait_holder() { # wait_holder <秒> <scope> <シャード>
	local i=0
	while [ $i -lt $(($1 * 10)) ]; do
		[ "$(holder "$2")" = "$3" ] && return 0
		sleep 0.2; i=$((i + 2))
	done
	return 1
}

step "1. 平時：東京・大阪・大阪の DR シャード（idle）"
start tokyo "file://$CENTRAL"                 # 東京のデーモンは中央を直接読む
start osaka "file://$MIRROR"
start osaka-dr "file://$MIRROR"
check "tk-web・tk-db・os-web が入る" bash -c "$(declare -f wait_field field); W=$W; wait_field 15 tk-web size 2 && wait_field 10 tk-db size 4 && wait_field 10 os-web size 2"
check "DR シャードは平時 idle" wait_for 10 logs/osaka-dr.log 'shard claims nothing.*idle'
check "site=tokyo の holder は tokyo" wait_holder 10 site=tokyo tokyo
$S fence --store fencing.json | sed 's/^/    /'

step "2. 宣言の矛盾は収束させない（§25.1「v1.5 の実装」）"
plan=work/shards/dr/tokyo-to-osaka.yaml
cp "$plan" plan.orig
echo "--- 2a. enabled を書き忘れた plan"
sed -i '/enabled:/d' "$plan"
merge "DR plan without enabled"
check "大阪（plan に関わらないシャード）も周を飛ばす" wait_for 15 logs/osaka.log 'cycle skipped.*enabled must be true or false'
set_field os-web size 9
check "飛ばしている間は os-web のずれを直さない" stays_field 3 os-web size 9
cp plan.orig "$plan"; merge "restore the DR plan"
check "戻すと os-web を直す" wait_field 15 os-web size 2
echo "--- 2b. to が平時 idle でない（大阪の通常シャードを to にして発動）"
sed -i 's/shard: osaka-dr/shard: osaka/; s/enabled: false/enabled: true/' "$plan"
merge "DR plan to a non-idle shard (enabled)"
check "大阪は周を飛ばす" wait_for 15 logs/osaka.log 'cycle skipped.*standby shard "osaka" already claims'
check "東京も周を飛ばす（片側だけ plan に従って idle にならない）" wait_for 10 logs/tokyo.log 'cycle skipped.*standby shard "osaka" already claims'
check "東京は claim を手放さない" bash -c "! grep -q 'claim released' logs/tokyo.log"
check "site=tokyo の holder は tokyo のまま" wait_holder 3 site=tokyo tokyo
cp plan.orig "$plan"; merge "restore the DR plan"
set_field tk-web size 9
check "戻すと東京は周を再開する（tk-web のずれを直す）" wait_field 15 tk-web size 2

step "3. DR 訓練（中央は健在）：enabled: true の 1 行だけで大阪が東京を引き継ぐ（§25.1 (a)・§25.4）"
stop tokyo                                    # 東京のデーモンが死ぬ
sed -i 's/enabled: false/enabled: true/' "$plan"
merge "DR drill: activate tokyo-to-osaka"
echo "    差分: $(git -C work diff --numstat HEAD~1 HEAD)"
check "発動は 1 行の変更" test "$(git -C work diff --numstat HEAD~1 HEAD)" = "$(printf '1\t1\tshards/dr/tokyo-to-osaka.yaml')"
check "DR シャードが DRPlanActive を出す" wait_for 15 logs/osaka-dr.log 'EVENT DRPlanActive: DR plan tokyo-to-osaka'
check "DR シャードが世代を上げて site=tokyo を取る" wait_for 15 logs/osaka-dr.log 'site=tokyo taken over from "tokyo"'
set_field tk-web size 7
check "DR シャードが tk-web のずれを直す" wait_field 10 tk-web size 2
start tokyo "file://$CENTRAL"                 # 東京が戻ってくる
check "戻った東京は idle に留まる" wait_for 15 logs/tokyo.log 'shard claims nothing.*idle'
check "戻った東京も DRPlanActive を出す" wait_for 5 logs/tokyo.log 'EVENT DRPlanActive'
check "site=tokyo の holder は osaka-dr のまま" wait_holder 3 site=tokyo osaka-dr
echo "--- 訓練の終わり：enabled: false に戻すと東京が取り戻す"
sed -i 's/enabled: true/enabled: false/' "$plan"
merge "DR drill: deactivate tokyo-to-osaka"
check "東京が世代を上げて site=tokyo を取り戻す" wait_for 15 logs/tokyo.log 'site=tokyo taken over from "osaka-dr"'
check "DR シャードは idle に戻る" wait_for 15 logs/osaka-dr.log 'claim released'
check "引き継ぎで何も prune しない" bash -c "! grep -h 'pruned=[1-9]' logs/tokyo.log logs/osaka-dr.log"
$S fence --store fencing.json | sed 's/^/    /'

step "4. 被災：東京（中央 git ごと）が落ちる。大阪は break-glass で DR plan を発動する（§25.1 (b)・§29.1）"
$S breakglass keygen --out offline/bg1.key    # 平時に作ってオフライン保管しておく鍵
git clone -q "$MIRROR" op                     # 大阪の運用者の手元（ローカルミラーの clone）
sed -i 's/enabled: false/enabled: true/' op/shards/dr/tokyo-to-osaka.yaml
BG="--repo op --key offline/bg1.key --actor osaka-oncall --audit-log $S4"
REASON="DR: tokyo is down, activate tokyo-to-osaka"
echo "--- 4a. 中央に届くうちは書けない（既定の判定 10s×3）"
check "届くと拒否（1 回目で届いた）" bash -c "! $S breakglass commit $BG --reason '$REASON' >logs/bg-reachable.log 2>&1 && grep -q 'reachable (probe 1/3 answered)' logs/bg-reachable.log"
sed 's/^/    /' logs/bg-reachable.log
stop tokyo
mv tokyo tokyo.down                           # 東京ごと落ちる
echo "--- 4b. 判定の途中で中央が戻る（2 回目で届く）と書けない"
( sleep 3; mv "$W/tokyo.down" "$W/tokyo" ) & PID_flap=$!
check "2 回目で届いて拒否" bash -c "! $S breakglass commit $BG --reason '$REASON' >logs/bg-flap.log 2>&1 && grep -q 'reachable (probe 2/3 answered)' logs/bg-flap.log"
sed 's/^/    /' logs/bg-flap.log
wait $PID_flap
mv tokyo tokyo.down
echo "--- 4c. 必須の指定・S4・判定の回数が欠けると書けない"
for c in "no actor|--repo op --key offline/bg1.key --audit-log $S4 --reason x" \
	"no reason|--repo op --key offline/bg1.key --actor a --audit-log $S4" \
	"no S4|--repo op --key offline/bg1.key --actor a --reason x" \
	"S4 unwritable|--repo op --key offline/bg1.key --actor a --reason x --audit-log /proc/nope/audit.jsonl --probe-interval 10ms" \
	"too few probes|$BG --reason x --probe-attempts 2"; do
	what=${c%%|*}; args=${c#*|}
	if $S breakglass commit $args >logs/bg-refused.log 2>&1; then ng "$what: 書けてしまった"; else ok "$what: 拒否（$(tail -1 logs/bg-refused.log | cut -c1-110)）"; fi
done
check "拒否した書込はミラーに届いていない" test "$(mirror_main)" = "$(git -C op rev-parse HEAD)"
check "拒否した書込は S4 に残らない" test ! -s "$S4"
echo "--- 4d. 中央に届かない（既定の判定 10s×3）→ 署名付きで書く"
t0=$(date +%s)
check "break-glass で書ける" bash -c "$S breakglass commit $BG --reason '$REASON' >logs/bg1.log 2>&1"
t1=$(date +%s)
sed 's/^/    /' logs/bg1.log
check "判定に 20 秒以上かけた（$((t1 - t0)) 秒）" test $((t1 - t0)) -ge 20
BG1=$(mirror_main)
SESSION=$(python3 -c "import json;print([json.loads(l) for l in open('$S4')][0]['session'])")
check "ミラーの先端が break-glass コミット" test "$BG1" = "$(git -C op rev-parse HEAD)"
check "S4 に breakglass.write（コミット・署名・鍵）" python3 -c "
import json;r=[json.loads(l) for l in open('$S4')]
assert len(r)==1 and r[0]['kind']=='breakglass.write' and r[0]['commit']=='$BG1' and r[0]['signature'] and r[0]['key'] and r[0]['actor']=='osaka-oncall', r"
echo "--- break-glass コミット"
git -C "$MIRROR" cat-file commit "$BG1" | sed 's/^/    /'
check "署名を確かめられる" bash -c "$S breakglass verify --repo $MIRROR --pubkey offline/bg1.key.pub $BG1 | tee logs/verify.log | grep -q '^ok '"
$S breakglass keygen --out offline/other.key >/dev/null
check "別の鍵では通らない" bash -c "! $S breakglass verify --repo $MIRROR --pubkey offline/other.key.pub $BG1 >>logs/verify.log"
git clone -q "$MIRROR" forge                  # 改竄：署名をそのまま写して tree を変えたコミット
sed -i 's/size: "4"/size: "1"/' forge/intent/site=tokyo/vms.yaml
git -C forge add -A
FORGED=$(git -C forge cat-file commit "$BG1" | sed '1,/^$/d' | git -C forge commit-tree "$(git -C forge write-tree)" -p "$BG1~1")
check "改竄したコミットは通らない" bash -c "! $S breakglass verify --repo forge --pubkey offline/bg1.key.pub $FORGED >>logs/verify.log"
sed 's/^/    /' logs/verify.log

echo "--- 4e. 拠点の常駐ループ：break-glass コミットを検出し、DR シャードが東京を引き継ぐ"
check "DR シャードが BreakGlassWriteUsed を出す" wait_for 15 logs/osaka-dr.log "EVENT BreakGlassWriteUsed severity=critical: break-glass commit ${BG1:0:12}"
check "大阪（通常シャード）も BreakGlassWriteUsed を出す" wait_for 5 logs/osaka.log "EVENT BreakGlassWriteUsed severity=critical: break-glass commit ${BG1:0:12}"
check "DR シャードが site=tokyo を取る" wait_for 15 logs/osaka-dr.log 'site=tokyo taken over from "tokyo"'
set_field tk-db size 1
check "DR シャードが tk-db のずれを直す" wait_field 10 tk-db size 4
check "ミラーは session を保持する（中央に届かない）" wait_for 10 logs/mirror.log "holding break-glass session $SESSION: central git still unreachable"
check "ミラーの先端は break-glass のまま" test "$(mirror_main)" = "$BG1"
echo "--- 4f. 同じ session でもう 1 つ書く（tk-web を 2 → 3）"
check "手元が古いと書けない" bash -c "git clone -q $MIRROR op-old && git -C op-old reset -q --hard HEAD~1 && sed -i 's/size: \"2\"/size: \"3\"/' op-old/intent/site=tokyo/vms.yaml && ! $S breakglass commit --repo op-old --key offline/bg1.key --actor osaka-oncall --audit-log $S4 --reason x --probe-interval 1s >logs/bg-old.log 2>&1 && grep -q 'pull it first' logs/bg-old.log"
python3 - op/intent/site=tokyo/vms.yaml <<'EOF'
import sys
p = sys.argv[1]; s = open(p).read()
open(p, "w").write(s.replace('name: tk-web\n      size: "2"', 'name: tk-web\n      size: "3"'))
EOF
check "2 つ目も書ける（--probe-interval 1s）" bash -c "$S breakglass commit $BG --reason 'DR: grow tk-web for the osaka load' --probe-interval 1s >logs/bg2.log 2>&1"
BG2=$(mirror_main)
check "DR シャードが tk-web を 3 にする" wait_field 15 tk-web size 3
$S breakglass status --mirror "$MIRROR" | sed 's/^/    /'
check "session に 2 コミット" bash -c "$S breakglass status --mirror $MIRROR | grep -q '2 commit(s)'"
check "--once の同期は失敗し、先端を保つ" bash -c "! $S mirror --upstream $CENTRAL --dir $MIRROR --once >/dev/null 2>&1 && test \$(git -C $MIRROR rev-parse refs/heads/main) = $BG2"

step "5. 中央が戻る：事後合流（breakglass/<session> の push・PR の下書き）と、ミラーの保持"
mv tokyo.down tokyo
# 戻った直後、平時の手元から（break-glass を知らずに）中央へ変更が入る：両側で同じ行が変わる
python3 - work/intent/site=tokyo/vms.yaml <<'EOF'
import sys
p = sys.argv[1]; s = open(p).read()
s = s.replace('name: tk-web\n      size: "2"', 'name: tk-web\n      size: "6"')
open(p, "w").write(s.replace('name: tk-db\n      size: "4"', 'name: tk-db\n      size: "8"'))
EOF
echo "# 復旧直後に中央で入った変更（break-glass と別のパス）" >work/intent/site=osaka/notes.yaml
merge "right after recovery: tk-web to 6, tk-db to 8, osaka notes (unaware of the break-glass)"
CENTRAL_AFTER=$(central_main)
DRAFT=$MIRROR/sakutei-breakglass-$SESSION-pr.md
check "ミラーが合流用ブランチを中央へ push する" wait_for 15 logs/mirror.log "merge-back branch breakglass/$SESSION pushed"
check "中央の breakglass/<session> は break-glass の先端（rebase・squash なし）" test "$(git -C tokyo/central.git rev-parse refs/heads/breakglass/$SESSION)" = "$BG2"
check "PR の下書きがある" test -s "$DRAFT"
echo "--- PR の下書き（$DRAFT）"
sed 's/^/    /' "$DRAFT"
check "下書きに両側で変わったパス（intent/site=tokyo/vms.yaml）だけが並ぶ" bash -c "grep -q '^- \`intent/site=tokyo/vms.yaml\`' $DRAFT && ! grep -q '^- \`intent/site=osaka/notes.yaml\`' $DRAFT"
check "下書きに 2 つのコミット・実行者・鍵" bash -c "grep -q ${BG1:0:12} $DRAFT && grep -q ${BG2:0:12} $DRAFT && grep -q osaka-oncall $DRAFT && grep -q \"\$(python3 -c \"import json;print(json.loads(open('$S4').readline())['key'])\")\" $DRAFT"
check "S4 に breakglass.merge_back.opened" grep -q '"breakglass.merge_back.opened"' "$S4"
check "取り込まれるまでミラーは中央を追わない" bash -c "sleep 3; test \$(git -C $MIRROR rev-parse refs/heads/main) = $BG2"
check "大阪の現実は緊急書込のまま（tk-db は 4、tk-web は 3）" bash -c "$(declare -f stays_field field); W=$W; stays_field 3 tk-db size 4 && stays_field 1 tk-web size 3"

echo "--- 5b. 合流前に東京のデーモンが戻る（中央を直接読む。中央はまだ enabled: false）"
start tokyo "file://$CENTRAL"
sleep 6
echo "    site=tokyo の holder: $(holder site=tokyo)、tk-web=$(field tk-web size)、tk-db=$(field tk-db size)"
if [ "$(holder site=tokyo)" = tokyo ]; then
	echo "見つけたこと：合流前に東京が戻ると、中央（enabled: false）に従って site=tokyo を取り返し、"
	echo "  緊急書込（tk-web=3）を中央の値で巻き戻す。DR シャードは FENCED になる（README の「見つけたこと」1）"
else
	ok "合流前に戻った東京は site=tokyo を取り返さない"
fi

echo "--- 5c. 事後レビュー：人が競合を解いて merge commit で取り込む"
git -C work pull -q --no-rebase origin main 2>/dev/null || true
git -C work fetch -q origin "breakglass/$SESSION"
if git -C work merge --no-ff -q -m "merge breakglass/$SESSION (post-hoc review)" FETCH_HEAD >logs/merge.log 2>&1; then
	ng "競合するはずが自動で merge できた"
else
	ok "中央と break-glass が同じファイルで競合する（自動では解かない）"
fi
git -C work checkout -q --theirs intent/site=tokyo/vms.yaml   # break-glass 側（tk-web=3）を取り…
python3 - work/intent/site=tokyo/vms.yaml <<'EOF'
import sys
p = sys.argv[1]; s = open(p).read()                             # …中央の tk-db=8 を載せ直す
open(p, "w").write(s.replace('name: tk-db\n      size: "4"', 'name: tk-db\n      size: "8"'))
EOF
git -C work add -A && git -C work commit -qm "merge breakglass/$SESSION (post-hoc review)" && git -C work push -q origin HEAD:main
MERGED=$(central_main)
check "取り込みは merge commit（親 2 つ）" test "$(git -C work log -1 --format=%P | wc -w)" = 2
check "ミラーが session を閉じて中央に追従する" wait_mirror 15
check "session の印が消える" bash -c "sleep 1; test ! -e $MIRROR/sakutei-breakglass.json"
check "S4 に breakglass.merged と breakglass.key.retired" bash -c "grep -q '\"breakglass.merged\"' $S4 && grep -q '\"breakglass.key.retired\"' $S4"
check "合流で中央も enabled: true になり、東京は claim を手放す" wait_for 15 logs/tokyo.log 'claim released'
if wait_holder 8 site=tokyo osaka-dr; then
	ok "DR シャードが site=tokyo を取り戻す"
else
	echo "見つけたこと：一度 FENCED になった DR シャードは、東京が claim を手放しても取り戻さない（holder=$(holder site=tokyo)）。"
	echo "  誰も site=tokyo を収束させない。DR シャードを起こし直すと取り戻す（README の「見つけたこと」2）"
	stop osaka-dr
	start osaka-dr "file://$MIRROR"
	check "起こし直した DR シャードが site=tokyo を取り戻す" wait_holder 20 site=tokyo osaka-dr
fi
stop tokyo
check "合流後の現実（tk-web=3、tk-db=8）" bash -c "$(declare -f wait_field field); W=$W; wait_field 15 tk-web size 3 && wait_field 10 tk-db size 8"
check "中央に入った break-glass コミットを全部確かめられる" bash -c "$S breakglass verify --repo $CENTRAL --pubkey offline/bg1.key.pub --range ${BG1}~1..$MERGED | tee logs/verify-central.log | grep -c '^ok ' | grep -qx 2"
sed 's/^/    /' logs/verify-central.log
echo "--- S4（$S4）"
python3 -c "
import json
for l in open('$S4'):
    r=json.loads(l); print('   ', r['kind'], r.get('session',''), (r.get('commit') or r.get('key') or '')[:40])"

echo "--- 5d. 使った鍵は失効：次の被災でも同じ鍵では書けない。新しい鍵へ rotate する"
mv tokyo tokyo.down
git -C op pull -q --ff-only
sed -i 's/size: "3"/size: "5"/' op/intent/site=tokyo/vms.yaml
check "失効した鍵は判定の前に拒否" bash -c "! $S breakglass commit $BG --reason x >logs/bg-retired.log 2>&1 && grep -q 'retired' logs/bg-retired.log"
sed 's/^/    /' logs/bg-retired.log
$S breakglass keygen --out offline/bg2.key >/dev/null
check "rotate した鍵なら書ける" bash -c "$S breakglass commit --repo op --key offline/bg2.key --actor osaka-oncall --audit-log $S4 --reason 'DR again' --probe-interval 1s >logs/bg3.log 2>&1"
mv tokyo.down tokyo
SESSION2=$(python3 -c "import json;print([json.loads(l) for l in open('$S4') if json.loads(l)['kind']=='breakglass.write'][-1]['session'])")
check "2 つ目の session の合流ブランチ" wait_for 15 logs/mirror.log "merge-back branch breakglass/$SESSION2 pushed"

step "6. 既知の限界：squash merge では session が閉じない（§25.1「既知の限界」）"
git -C work pull -q --ff-only origin main
git -C work fetch -q origin "breakglass/$SESSION2"
git -C work merge --squash -q FETCH_HEAD >/dev/null && git -C work commit -qm "squash breakglass/$SESSION2 (wrong way)" && git -C work push -q origin HEAD:main
sleep 4
check "squash で取り込んでも session は開いたまま（ミラーは中央を追わない）" bash -c "test -e $MIRROR/sakutei-breakglass.json && test \$(git -C $MIRROR rev-parse refs/heads/main) != \$(git -C $CENTRAL rev-parse refs/heads/main)"
tail -1 logs/mirror.log | sed 's/^/    /'
git -C work fetch -q origin "breakglass/$SESSION2"
git -C work merge --no-ff -q -s ours -m "merge breakglass/$SESSION2 (record the history; content came with the squash)" FETCH_HEAD
git -C work push -q origin HEAD:main
check "merge commit で履歴を入れ直すと閉じる" wait_mirror 15
stop osaka; stop osaka-dr

step "7. 判定の変種：応答しない中央（TCP はつながるが返事が無い）・権限で弾かれる中央（§29.1）"
git daemon --reuseaddr --base-path="$W/tokyo" --export-all --listen=127.0.0.1 --port=19419 "$W/tokyo" >logs/git-daemon.log 2>&1 &
PIDS="$PIDS $!"
sleep 1
PID_daemon=$(pgrep -P $! | head -1 || true); [ -n "$PID_daemon" ] || PID_daemon=$!
GURL=git://127.0.0.1:19419/central.git
$S mirror --upstream "$GURL" --dir osaka/mirror-g.git --once
$S breakglass keygen --out offline/bg3.key >/dev/null   # bg2 は手順 6 で失効した
git clone -q osaka/mirror-g.git op-g             # 変更しない手元：判定の後の「nothing to commit」で止まる
kill -STOP "$PID_daemon"
t0=$(date +%s)
$S breakglass commit --repo op-g --key offline/bg3.key --actor probe --audit-log "$S4" --reason x >logs/bg-silent.log 2>&1 || true
t1=$(date +%s)
sed 's/^/    /' logs/bg-silent.log
check "応答しない中央は 1 回 10s で切って 3 回とも届かないに数える（$((t1 - t0)) 秒）" bash -c "test \$(grep -c 'no answer from .* within 10s' logs/bg-silent.log) = 3 && test $((t1 - t0)) -ge 50 && test $((t1 - t0)) -lt 60"
t0=$(date +%s)
$S mirror --upstream "$GURL" --dir osaka/mirror-g.git --once --timeout 5s >logs/mirror-silent.log 2>&1 || true
t1=$(date +%s)
sed 's/^/    /' logs/mirror-silent.log
check "ミラーの同期も応答しない中央で止まったままにならない（$((t1 - t0)) 秒）" test $((t1 - t0)) -lt 15
kill -CONT "$PID_daemon"
chmod 000 tokyo/central.git                      # 権限で弾かれる（認証の失敗の代わり）
$S breakglass commit --repo op --key offline/bg3.key --actor probe --audit-log "$S4" --reason x --probe-interval 1s >logs/bg-perm.log 2>&1 || true
chmod 755 tokyo/central.git
sed 's/^/    /' logs/bg-perm.log
check "権限で弾かれる中央も届かないに数える" bash -c "test \$(grep -c 'unreachable:' logs/bg-perm.log) = 3"

step "ログの抜粋"
for f in tokyo osaka osaka-dr mirror; do
	echo "--- logs/$f.log"; excerpt "logs/$f.log"
done

step "おわり（作業ディレクトリ: $W、NG=$NG）"
[ "$NG" -eq 0 ]
