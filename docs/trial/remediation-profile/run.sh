#!/bin/bash
# remediation schedule（直してよい時間帯）と運用プロファイル（cooling-off を含む）を手で試す手順を、
# 1 回で通して流すスクリプト（docs/trial/remediation-profile/README.md の手順 1〜6）。
# 使い方: docs/trial/remediation-profile/run.sh [作業ディレクトリ]   （既定 /tmp/sakutei-remediation）
# 作業ディレクトリは毎回作り直す。リポジトリのファイルは書き換えない。
# 窓は今の時刻から作るので、intent はこのスクリプトが書く。手順 2 は本物の時計で窓が開いて閉じるのを待つ
# （2〜3 分）。通しで 4 分ほど。
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../../.." && pwd)
W=${1:-/tmp/sakutei-remediation}

step() { printf '\n==== %s ====\n' "$*"; }
note() { printf -- '→ %s\n' "$*"; }
# rc <コマンド…>: 終了コードを表示する（set -e で止めない）。
rc() { set +e; "$@"; c=$?; set -e; echo "（終了コード $c）"; }
# wait_for <秒> <説明> <条件>: 条件（シェルの式。毎回評価し直す）が成り立つまで待つ。
wait_for() {
	secs=$1 what=$2 cond=$3
	for _ in $(seq $((secs * 10))); do
		if eval "$cond" >/dev/null 2>&1; then return 0; fi
		sleep 0.1
	done
	echo "!! 待ちきれなかった: $what" >&2
	return 1
}
# val <ディレクトリ> <名前>: ゾーンファイルのその名前の A レコードの値。
val() { awk -v n="$2" '$1 == n && $4 == "A" { print $5 }' "$1/example.com.zone"; }
# has <ファイル> <正規表現>
has() { grep -Eq "$2" "$1"; }
# count <ファイル> <正規表現>
count() { grep -Ec "$2" "$1" || true; }

PIDS=""
cleanup() { for p in $PIDS; do kill "$p" 2>/dev/null || true; done; wait 2>/dev/null || true; }
trap cleanup EXIT
# start <ディレクトリ> <ログ> <引数…>: そのディレクトリで常駐の sakutei を起動する（DPID に pid）。
start() {
	d=$1 log=$2; shift 2
	(cd "$d" && exec "$S" --intent intent --zone example.com.zone --interval 2s "$@") >"$d/$log" 2>&1 &
	DPID=$!
	PIDS="$PIDS $DPID"
}
# stop: 最後に起動したデーモンを止める（Ctrl-C と同じ SIGINT。周の切れ目で止まる）。
stop() { kill -INT "$DPID"; wait "$DPID" 2>/dev/null || true; }

# 窓の時刻を作る。曜日・日付またぎを確かめる窓は、今がその土地の昼ごろになる tz（TZW）で書く
# （UTC で 0 時の前後に流しても、今の時刻を中心に ±1 時間の窓が日付をまたがないように）。
H=$(date -u +%-H)
OFF=$((12 - H))
if [ $OFF -gt 0 ]; then TZW="Etc/GMT-$OFF"; else TZW="Etc/GMT+$((-OFF))"; fi
at() { TZ=$TZW date -d "$1" +%H:%M; }  # at '+30 min' → TZW での HH:MM
dow() { TZ=$TZW date -d "$1" +%a; }    # dow 'today' → Fri

# mk <名前>: 作業ディレクトリの中に、初めのゾーンと空の intent/ を持つディレクトリを作る。
mk() { rm -rf "$W/$1"; mkdir -p "$W/$1/intent"; cp "$HERE/initial/example.com.zone" "$W/$1/"; echo "$W/$1"; }
# records [scope の行]: a1〜a3 を 198.51.100.x にする宣言（引数があれば各リソースの scope にする）。
records() {
	for i in 1 2 3; do
		printf '  - provider: dnszone\n    id: a%s.example.com./A\n' $i
		if [ $# -gt 0 ]; then printf '    scope:\n      %s\n' "$1"; fi
		printf '    fields:\n      name: a%s.example.com.\n      type: A\n      value: 198.51.100.%s\n      ttl: "300"\n' $i $i
	done
}
# www: www を 192.0.2.10 に戻す宣言（初めのゾーンでは 192.0.2.99 にずれている）。
www() { printf '  - provider: dnszone\n    id: www.example.com./A\n    fields:\n      name: www.example.com.\n      type: A\n      value: 192.0.2.10\n      ttl: "300"\n'; }
# sched <dir> <remediation: の中身（2 字下げ）>: www だけを持つ intent を書く。
sched() { { echo 'version: "1"'; echo 'remediation:'; printf '%s\n' "$2"; echo 'resources:'; www; } >"$1/intent/main.yaml"; }
# try <名前> <remediation: の中身>: 新しいディレクトリで 1 回収束し、決まった mode と www の値を出す。
try() {
	d=$(mk "1-$1")
	sched "$d" "$2"
	set +e; (cd "$d" && "$S" reconcile --intent intent --zone example.com.zone) >"$d/out" 2>&1; c=$?; set -e
	printf '%-28s %s\n' "[$1]" "$(grep -E '^Remediation mode' "$d/out" || echo "(Remediation mode の行が無い)")"
	printf '%-28s www=%s（終了コード %s）\n' "" "$(val "$d" www)" "$c"
}

rm -rf "$W"
mkdir -p "$W"

step "0. ビルド"
(cd "$REPO" && go build -o "$W/sakutei" ./cmd/sakutei)
S=$W/sakutei
echo "今: $(date -u '+%a %H:%M:%S') UTC / $(TZ=$TZW date '+%a %H:%M') $TZW / $(TZ=Asia/Tokyo date '+%a %H:%M') Asia/Tokyo"

step "1. 時間帯（1 回収束の reconcile）。www は 192.0.2.99 にずれていて、intent は 192.0.2.10"
try "今を含む窓" "  default: alert-only
  tz: $TZW
  schedule:
    - window: \"$(at '-30 min')-$(at '+30 min')\"
      mode: auto"
try "今を含まない窓" "  tz: $TZW
  schedule:
    - window: \"$(at '+1 hour')-$(at '+90 min')\"
      mode: auto"
d=$W/1-今を含まない窓
grep -E 'would be|^Reconcile' "$d/out" | sed 's/^/    /'
try "default: auto・窓の外" "  default: auto
  tz: $TZW
  schedule:
    - window: \"$(at '+1 hour')-$(at '+90 min')\"
      mode: alert-only"
try "先に書いた窓が勝つ" "  tz: $TZW
  schedule:
    - window: \"$(at '-30 min')-$(at '+30 min')\"
      mode: alert-only
    - window: \"*\"
      mode: auto"
try "今日の曜日" "  tz: $TZW
  schedule:
    - window: \"$(dow today) $(at '-30 min')-$(at '+30 min')\"
      mode: auto"
try "明日の曜日" "  tz: $TZW
  schedule:
    - window: \"$(dow tomorrow) $(at '-30 min')-$(at '+30 min')\"
      mode: auto"
# 終わりが始めより前の窓は日付をまたぐ。曜日は「窓が開く日」。
try "昨日開いて今日閉じる窓" "  tz: $TZW
  schedule:
    - window: \"$(dow yesterday) $(at '+31 min')-$(at '+30 min')\"
      mode: auto"
try "今日開いて明日閉じる窓" "  tz: $TZW
  schedule:
    - window: \"$(dow today) $(at '+31 min')-$(at '+30 min')\"
      mode: auto"
try "tz: Asia/Tokyo の窓" "  tz: Asia/Tokyo
  schedule:
    - window: \"$(TZ=Asia/Tokyo date -d '-30 min' +%H:%M)-$(TZ=Asia/Tokyo date -d '+30 min' +%H:%M)\"
      mode: auto"
try "同じ窓を tz 無し（UTC）で" "  schedule:
    - window: \"$(TZ=Asia/Tokyo date -d '-30 min' +%H:%M)-$(TZ=Asia/Tokyo date -d '+30 min' +%H:%M)\"
      mode: auto"

if [ -z "${SKIP2:-}" ]; then  # SKIP2=1 で手順 2（本物の時計で 2〜3 分待つ）を飛ばす
step "2. 常駐：窓の外は知らせるだけ、窓に入ると直し、窓が閉じるとまた直さない（本物の時計で待つ）"
d=$(mk 2-daemon)
# 次の分の頭から 1 分だけの窓。分の終わり近くなら 1 分待ってから作る（窓の前の周を何周か見るため）。
if [ "$(date -u +%-S)" -gt 40 ]; then sleep $((62 - $(date -u +%-S))); fi
OPEN=$(date -u -d '+1 min' +%H:%M)
CLOSE=$(date -u -d '+2 min' +%H:%M)
sched "$d" "  default: alert-only
  schedule:
    - window: \"$OPEN-$CLOSE\"
      mode: auto"
sed -n '/^remediation/,/^resources/p' "$d/intent/main.yaml"
start "$d" daemon.log
wait_for 10 "最初の周" 'has "$d/daemon.log" "cycle 2:"'
echo "--- 窓の前（$(date -u +%H:%M:%S)）"
grep -E 'loop started|remediation mode=|DRIFT EVENT|cycle 1:' "$d/daemon.log" | head -6
note "www=$(val "$d" www)（直していない）"
wait_for 150 "窓が開く" 'has "$d/daemon.log" "remediation mode=auto"'
wait_for 10 "直す" 'test "$(val "$d" www)" = 192.0.2.10'
echo "--- 窓が開いた（$(date -u +%H:%M:%S)）"
grep -E 'remediation mode=auto' "$d/daemon.log"
note "www=$(val "$d" www)（窓が開いた周に直した）"
# 閉じた後の mode の行（2 回目の alert-only）を待つ
wait_for 90 "窓が閉じる" 'test "$(count "$d/daemon.log" "remediation mode=alert-only")" -ge 2'
echo "--- 窓が閉じた（$(date -u +%H:%M:%S)）"
grep -E 'remediation mode=' "$d/daemon.log" | tail -1
sed -i 's/^www\(.*\)192\.0\.2\.10$/www\1192.0.2.77/' "$d/example.com.zone"
note "ゾーンの www を手で 192.0.2.77 に書き換えた"
n=$(count "$d/daemon.log" 'cycle [0-9]+: observed')
wait_for 20 "3 周" 'test "$(count "$d/daemon.log" "cycle [0-9]+: observed")" -ge $((n + 3))'
grep -E 'DRIFT EVENT' "$d/daemon.log" | tail -1
grep -E 'cycle [0-9]+: alert-only' "$d/daemon.log" | tail -1
note "www=$(val "$d" www)（窓の外なので直さない）"
stop
echo "--- mode の切り替わり（全部）"
grep -E 'remediation mode=' "$d/daemon.log"

fi

step "3. --mode・--freeze・scope 明示の alert-only（schedule は今を含む窓で auto）"
AUTO="  tz: $TZW
  schedule:
    - window: \"$(at '-30 min')-$(at '+30 min')\"
      mode: auto"
d=$(mk 3-mode)
sched "$d" "$AUTO"
cd "$d"
for m in "--mode auto" "--mode alert-only" "--freeze" "--mode alert-only --freeze"; do
	set +e; $S reconcile --intent intent --zone example.com.zone $m >out 2>&1; c=$?; set -e
	printf '%-28s %s（終了コード %s、www=%s）\n' "[$m]" "$(grep -E '^Remediation mode|FROZEN' out | head -1)" "$c" "$(val . www)"
	cp "$HERE/initial/example.com.zone" .
done
echo "--- --mode の値の誤り"
rc $S reconcile --intent intent --zone example.com.zone --mode Auto
rc $S reconcile --intent intent --zone example.com.zone --mode freeze
rc $S --intent intent --zone example.com.zone --mode alertonly
echo "--- --mode= （空）"
rc $S reconcile --intent intent --zone example.com.zone --mode=
cp "$HERE/initial/example.com.zone" .
echo "--- plan は --mode を受け取らない（読むだけ）"
rc $S plan --intent intent --zone example.com.zone --mode alert-only 2>&1 | tail -2
echo "--- 常駐で --mode alert-only・--freeze"
for m in "--mode alert-only" "--freeze"; do
	lg=daemon$(echo "$m" | tr ' ' '_').log
	start "$d" "$lg" $m
	wait_for 10 "2 周" 'has "$d/$lg" "cycle 2:"'
	stop
	echo "[$m]"
	grep -E 'loop started|remediation mode=|cycle 1: (alert-only|FROZEN)' "$d/$lg" | sed 's/^/  /'
	note "www=$(val . www)"
done
echo "--- scope 明示の remediation: alert-only（a1〜a3 は scope tier=db、www は scope なし）"
{ echo 'version: "1"'; echo 'remediation:'; printf '%s\n' "$AUTO"
  printf 'scopes:\n  - scope:\n      tier: db\n    remediation: alert-only\n'
  echo 'resources:'; www; records "tier: db"; } >intent/main.yaml
rc $S reconcile --intent intent --zone example.com.zone 2>&1 | grep -E 'Remediation mode|alert-only|UPDATE|^Reconcile|終了コード'
note "www=$(val . www) a1=$(val . a1)（www は直し、tier=db の a1〜a3 は直さない）"
cd "$W"

step "4. remediation: の宣言の誤り（ロードで拒否）"
d=$(mk 4-errors)
cd "$d"
bad() {
	sched . "$1"
	printf '[%s] ' "$2"; rc $S plan --intent intent --zone example.com.zone 2>&1 | tr '\n' ' '; echo
}
bad '  schedule:
    - window: "Sat 25:00-26:00"
      mode: auto' "時刻の範囲外"
bad '  schedule:
    - window: "Sat 02:00-02:00"
      mode: auto' "空の窓"
bad '  schedule:
    - window: "Satu 02:00-04:00"
      mode: auto' "曜日の誤り"
bad '  schedule:
    - window: "02:00-04:00"' "mode が無い"
bad '  schedule:
    - window: "02:00-04:00"
      mode: freeze' "mode の誤り"
bad '  tz: Asia/Tokio
  schedule:
    - window: "02:00-04:00"
      mode: auto' "tz の誤り"
bad '  default: sometimes' "default の誤り"
sched . '  default: auto'
cp intent/main.yaml intent/second.yaml
sed -i '/^resources:/,$d' intent/second.yaml
printf '[2 つのファイルに remediation:] '; rc $S plan --intent intent --zone example.com.zone 2>&1 | tr '\n' ' '; echo
cd "$W"

# 5・6 の intent: a1〜a3（scope pool=web）を 198.51.100.x に変える。変更の上限は 1 周 1 件なので、
# 通常の予算では 3 件とも止まる。planned_change（kind: update・max_count 3）のレーンが開けば 3 件とも通る。
# pc <dir> <トップレベルに足す行> <planned_change に足す行>
WIN="$(date -u -d '-1 hour' '+%Y-%m-%d %H:%M') .. $(date -u -d '+3 hour' '+%Y-%m-%d %H:%M')"
pc() {
	{ echo 'version: "1"'; if [ -n "$2" ]; then printf '%s\n' "$2"; fi
	  echo 'resources:'; records "pool: web"
	  printf 'planned_change:\n  scope:\n    pool: web\n  kind: update\n  max_count: 3\n  window: "%s"\n' "$WIN"
	  if [ -n "$3" ]; then printf '%s\n' "$3"; fi; } >"$1/intent/main.yaml"
}
# once [引数…]: 1 回収束（上限 1 件・S4 あり）。レーンと予算の行と a1 の値だけを出す。
once() {
	set +e; $S reconcile --intent intent --zone example.com.zone --max-changes-per-cycle 1 --audit-log audit.jsonl "$@" >out 2>&1; c=$?; set -e
	grep -E 'PlannedChange|^Reconcile|lane' out | sed 's/planned_change(scope=pool=web kind=update window=[^)]*)/planned_change(pool=web update)/' | sed 's/^/  /'
	echo "  （終了コード $c、a1=$(val . a1)）"
	cp "$HERE/initial/example.com.zone" .
}
APPROVERS() { printf '  approval:\n    pr: 7\n    approvers:\n'; for a in "$@"; do printf '      - %s\n' "$a"; done; }

step "5. 運用プロファイルの既定と上書き（1 回収束・--audit-log）"
d=$(mk 5-profile)
cd "$d"
echo "[team（profile を書かない）・承認なし]"; pc . "" ""; once
echo "[team・承認者 1 人]"; pc . "" "$(APPROVERS alice)"; once
echo "[team・承認者 1 人・--audit-log なし]"
pc . "" "$(APPROVERS alice)"
set +e; $S reconcile --intent intent --zone example.com.zone --max-changes-per-cycle 1 >out 2>&1; set -e
grep -E 'planned_change|^Reconcile' out | sed 's/^/  /'; cp "$HERE/initial/example.com.zone" .
rm -f audit.jsonl
echo "[enterprise・承認者 1 人]"; pc . "profile: enterprise" "  applicant: carol
$(APPROVERS alice)"; once
echo "[enterprise・申請者本人と 1 人]"; pc . "profile: enterprise" "  applicant: carol
$(APPROVERS carol alice)"; once
echo "[enterprise・申請者以外の 2 人]"; pc . "profile: enterprise" "  applicant: carol
$(APPROVERS alice bob)"; once
rm -f audit.jsonl
echo "[enterprise・宣言の requirements（1-of-1）が既定に勝つ]"; pc . "profile: enterprise" "  applicant: carol
  requirements:
    quorum:
      n: 1
      of: 1
$(APPROVERS alice)"; once
rm -f audit.jsonl
echo "[solo（cooling_off は既定の 2h）]"; pc . "profile: solo" ""; once
rm -f audit.jsonl
echo "[solo・solo.cooling_off: 30m]"; pc . "profile: solo
solo:
  cooling_off: 30m" ""; once
rm -f audit.jsonl
echo "[solo・宣言の cooling_off: 0s が既定に勝つ]"; pc . "profile: solo" "  cooling_off: 0s"; once
rm -f audit.jsonl
echo "[solo・宣言の requirements（2-of-3）は solo でも効く・承認者 1 人]"; pc . "profile: solo" "  cooling_off: 0s
  requirements:
    quorum:
      n: 2
      of: 3
$(APPROVERS alice)"; once
rm -f audit.jsonl
echo "--- enterprise の承認ゲート（requirements の無いゲート）と applicant"
wf() {
	cat <<EOF
workflows:
  - workflow: renumber
$1    target:
      provider: dnszone
    stages:
      - name: all
        targets:
          - www.example.com./A
        gate:
          manual_approval: required
    steps:
      - apply:
          value: 192.0.2.10
EOF
}
{ echo 'version: "1"'; echo 'profile: enterprise'; echo 'resources:'; www; wf ""; } >intent/main.yaml
printf '[enterprise・applicant なし] '; rc $S plan --intent intent --zone example.com.zone 2>&1 | tr '\n' ' '; echo
{ echo 'version: "1"'; echo 'profile: enterprise'; echo 'resources:'; www; wf "    applicant: carol
"; } >intent/main.yaml
printf '[enterprise・applicant あり] '; rc $S plan --intent intent --zone example.com.zone 2>&1 | grep -E 'workflow|Plan|Converged|終了コード' | tr '\n' ' '; echo
{ echo 'version: "1"'; echo 'profile: team'; echo 'resources:'; www; wf ""; } >intent/main.yaml
printf '[team・applicant なし] '; rc $S plan --intent intent --zone example.com.zone 2>&1 | grep -E 'workflow|Plan|Converged|終了コード' | tr '\n' ' '; echo
echo "--- 書式の誤り（ロードで拒否）"
for top in "profile: Solo" "profile: team
solo:
  cooling_off: 1h" "profile: solo
solo:
  cooling_off: -1h" "profile: solo
solo:
  delay: 1h"; do
	{ echo 'version: "1"'; printf '%s\n' "$top"; echo 'resources:'; www; } >intent/main.yaml
	printf '[%s] ' "$(echo "$top" | tr '\n' ' ')"; rc $S plan --intent intent --zone example.com.zone 2>&1 | tr '\n' ' '; echo
done
{ echo 'version: "1"'; echo 'profile: solo'; echo 'resources:'; www; } >intent/main.yaml
printf 'version: "1"\nprofile: solo\n' >intent/second.yaml
printf '[2 つのファイルに profile:] '; rc $S plan --intent intent --zone example.com.zone 2>&1 | tr '\n' ' '; echo
rm intent/second.yaml
cd "$W"

step "6. cooling-off の時計は S4：デーモンを起こし直しても延びない・縮まない"
CO=40
d=$(mk 6-cooling-off)
cd "$d"
pc . "profile: solo
solo:
  cooling_off: ${CO}s" ""
T0=$(date -u +%s)
start "$d" daemon1.log --max-changes-per-cycle 1 --audit-log audit.jsonl
wait_for 10 "cooling-off の知らせ" 'has daemon1.log "PlannedChangeCoolingOff"'
grep -E 'PlannedChangeCoolingOff|BUDGET HALTED' daemon1.log | head -2 | sed 's/planned_change(scope=pool=web kind=update window=[^)]*)/planned_change(pool=web update)/'
UNTIL=$(grep -oE 'until [0-9T:Z-]+' daemon1.log | head -1)
sleep 12
stop
note "$(( $(date -u +%s) - T0 )) 秒で止めた。a1=$(val . a1)（レーンは開いていない）"
sleep 3
T1=$(date -u +%s)
start "$d" daemon2.log --max-changes-per-cycle 1 --audit-log audit.jsonl
wait_for 10 "cooling-off の知らせ（起こし直し後）" 'has daemon2.log "PlannedChangeCoolingOff"'
grep -E 'PlannedChangeCoolingOff' daemon2.log | head -1 | sed 's/planned_change(scope=pool=web kind=update window=[^)]*)/planned_change(pool=web update)/'
UNTIL2=$(grep -oE 'until [0-9T:Z-]+' daemon2.log | head -1)
if [ "$UNTIL" = "$UNTIL2" ]; then
	note "起こし直す前と同じ「$UNTIL」（S4 の planned_change.declared から読み直した）"
else
	echo "!! 起こし直す前は「$UNTIL」、後は「$UNTIL2」"
fi
wait_for 60 "レーンが開く" 'test "$(val . a1)" = 198.51.100.1'
T2=$(date -u +%s)
wait_for 10 "3 件" 'test "$(val . a3)" = 198.51.100.3'
grep -E 'PlannedChangeOpened|admitted' daemon2.log | sed 's/planned_change(scope=pool=web kind=update window=[^)]*)/planned_change(pool=web update)/'
note "宣言を初めて見てから $((T2 - T0)) 秒、起こし直してから $((T2 - T1)) 秒で開いた（cooling-off ${CO}s）。a1=$(val . a1) a2=$(val . a2) a3=$(val . a3)"
wait_for 10 "周の終わり" 'test "$(count daemon2.log "planned_change.executed|admitted")" -ge 1'
sleep 3
stop
echo "--- S4（audit.jsonl）"
sed -E 's/"planned_change":"[^"]*",//; s/planned_change\(scope=pool=web kind=update window=[^)]*\)/planned_change(pool=web update)/; s/"time":"([^.]*)\.[0-9]*Z"/"time":"\1Z"/' audit.jsonl

echo "--- cooling-off は S4 が無ければ評価できない（--audit-log なしの常駐）"
d=$(mk 6-no-audit)
cd "$d"
pc . "profile: solo
solo:
  cooling_off: 1s" ""
start "$d" daemon.log --max-changes-per-cycle 1
wait_for 10 "3 周" 'has daemon.log "cycle 3:"'
stop
grep -E 'PlannedChange' daemon.log | head -1 | sed 's/planned_change(scope=pool=web kind=update window=[^)]*)/planned_change(pool=web update)/'
note "a1=$(val . a1)（1 秒経ってもレーンは開かない）"

echo "--- デーモンを止めている間に宣言を消す（取り下げ）→ 書き戻すと cooling-off はやり直し"
d=$(mk 6-withdraw)
cd "$d"
pc . "profile: solo
solo:
  cooling_off: 20s" ""
cp intent/main.yaml decl.yaml
start "$d" daemon1.log --max-changes-per-cycle 1 --audit-log audit.jsonl
wait_for 10 "cooling-off の知らせ" 'has daemon1.log "PlannedChangeCoolingOff"'
stop
sed -i '/^planned_change:/,$d' intent/main.yaml
note "intent から planned_change を消した（PR の revert）"
set +e; $S reconcile --intent intent --zone example.com.zone --max-changes-per-cycle 1 --audit-log audit.jsonl >out 2>&1; set -e
grep -E 'Withdrawn|^Reconcile' out | sed 's/^/  /'
sleep 21
cp decl.yaml intent/main.yaml
note "20 秒以上経ってから同じ宣言を書き戻した"
T3=$(date -u +%s)
start "$d" daemon2.log --max-changes-per-cycle 1 --audit-log audit.jsonl
wait_for 10 "cooling-off の知らせ" 'has daemon2.log "PlannedChangeCoolingOff"'
grep -E 'PlannedChangeCoolingOff' daemon2.log | head -1 | sed 's/planned_change(scope=pool=web kind=update window=[^)]*)/planned_change(pool=web update)/'
wait_for 40 "レーンが開く" 'test "$(val . a1)" = 198.51.100.1'
note "書き戻してから $(( $(date -u +%s) - T3 )) 秒で開いた（やり直した 20s の後）"
sleep 3
stop
echo "--- S4 の記録の並び"
grep -oE '"kind":"[a-z_.]+"' audit.jsonl | tr '\n' ' '; echo

echo "--- 窓の外（alert-only）の回でも宣言を見たことは S4 に残り、cooling-off の時計は動く"
d=$(mk 6-schedule)
cd "$d"
# sched_pc <窓>: solo（cooling_off 5s）+ その窓だけ auto の schedule + planned_change
sched_pc() { pc . "profile: solo
solo:
  cooling_off: 5s
remediation:
  tz: $TZW
  schedule:
    - window: \"$1\"
      mode: auto" ""; }
sched_pc "$(at '+1 hour')-$(at '+90 min')"
set +e; $S reconcile --intent intent --zone example.com.zone --max-changes-per-cycle 1 --audit-log audit.jsonl >out 2>&1; c=$?; set -e
grep -E 'Remediation mode|PlannedChange|would be' out | head -2 | sed 's/^/  /'
note "（終了コード $c、a1=$(val . a1)）。S4: $(grep -oE '"kind":"[a-z_.]+"' audit.jsonl 2>/dev/null | tr '\n' ' ')"
sleep 6
sched_pc "$(at '-30 min')-$(at '+30 min')"
note "6 秒後に、窓を今を含む帯に書き換えた（窓が開いたのと同じ）"
set +e; $S reconcile --intent intent --zone example.com.zone --max-changes-per-cycle 1 --audit-log audit.jsonl >out 2>&1; c=$?; set -e
grep -E 'Remediation mode|PlannedChange|^Reconcile' out | sed 's/planned_change(scope=pool=web kind=update window=[^)]*)/planned_change(pool=web update)/' | sed 's/^/  /'
note "（終了コード $c、a1=$(val . a1)）。窓の外の回から数えた cooling-off が過ぎているので、窓が開いた回にそのまま開く"
grep -oE '"kind":"[a-z_.]+"' audit.jsonl | tr '\n' ' '; echo

step "おわり"
