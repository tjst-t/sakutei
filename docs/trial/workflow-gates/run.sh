#!/bin/sh
# Workflow の承認ゲートを手で試す手順を、1 回で通して流すスクリプト（docs/trial/workflow-gates/README.md）。
# 使い方: docs/trial/workflow-gates/run.sh [作業ディレクトリ]   （既定 /tmp/sakutei-wfg）
# 作業ディレクトリは毎回作り直す。リポジトリのファイルは書き換えない。
# 要るもの: go・curl・python3（fake の現実＝state.json を手で書き換えるのに使う）。
# 承認 API は 127.0.0.1:${PORT:-18470} で待ち受ける。
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../../.." && pwd)
W=${1:-/tmp/sakutei-wfg}
PORT=${PORT:-18470}
API=http://127.0.0.1:$PORT

rm -rf "$W"
mkdir -p "$W"
(cd "$REPO" && go build -o "$W/sakutei" ./cmd/sakutei)
S="$W/sakutei"

step() { printf '\n==== %s ====\n' "$*"; }

# setup <名前>: シナリオごとの作業ディレクトリ（intent・現実・S2・S4 を別にする）
setup() {
	D="$W/$1"
	mkdir -p "$D"
	cp -r "$HERE/intent" "$D/intent"
	cp "$HERE/initial/state.json" "$D/state.json"
	cd "$D"
}

# start / stop: 常駐の sakutei（--workflow-store・--audit-log・--approval-listen）
start() {
	LOG=$1
	"$S" --intent intent --state state.json --workflow-store wf.json --audit-log audit.jsonl \
		--approval-listen "127.0.0.1:$PORT" --interval 1s >"$LOG" 2>&1 &
	PID=$!
}
stop() {
	kill -INT "$PID"
	wait "$PID" || true
}

# until_log <パターン> [秒]: デーモンのログにパターンが出るまで待つ
until_log() {
	i=0
	while ! grep -q -- "$1" "$LOG"; do
		i=$((i + 1))
		if [ "$i" -gt "$((${2:-15} * 10))" ]; then
			echo "!! timed out waiting for: $1"
			tail -20 "$LOG"
			exit 1
		fi
		sleep 0.1
	done
}

# events: デーモンのログから Workflow の出来事だけを出す（skip・要約の行は省く）
events() { grep -E 'Workflow|Stage|Gate|Invariant|Drift' "$LOG" | cut -c1-220 || true; }

# post <gate> <endpoint> <JSON>: 承認 API を叩いて HTTP の状態と error を出す
post() {
	code=$(curl -s -o "$D/body" -w '%{http_code}' -X POST -d "$3" "$API/workflows/rolling/gates/$1/$2")
	err=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("error",""))' "$D/body")
	echo "POST $1/$2 $3 -> $code $err"
}

approve() { "$S" approve --store wf.json --audit-log audit.jsonl "$@" || echo "(exit $?)"; }

# setf <id> <field> <値>: 外からの変更（人の手・ほかの仕組み）
setf() {
	python3 - "$1" "$2" "$3" <<'EOF'
import json, sys
d = json.load(open("state.json"))
d["resources"][sys.argv[1]]["fields"][sys.argv[2]] = sys.argv[3]
json.dump(d, open("state.json", "w"), indent=2)
EOF
}

fields() {
	python3 -c 'import json; d=json.load(open("state.json"))["resources"]; print("  ".join(k+":"+",".join(f+"="+str(v) for f,v in sorted(d[k]["fields"].items()) if f!="name") for k in sorted(d)))'
}

audit() { cut -c1-260 audit.jsonl; }

# ---------------------------------------------------------------------------
step "A0. plan（Workflow の対象は収束ループが触らない）"
setup a
"$S" plan --intent intent --state state.json --workflow-store wf.json | sed -n 2,5p

step "A1. 起動 → s1 を終えてゲート s1 で止まる"
start d1.log
until_log "awaiting approval at gate s1 until"
events
fields

step "A2. デーモンを起こし直しても待ちに留まる"
stop
"$S" workflow list --store wf.json
start d2.log
until_log "cycle 2: WorkflowWaiting"
events

step "A3. CLI（sakutei approve）: 違うゲートは拒否、s1 は単一承認で通る"
approve --approver bob --signature sig-b0 rolling s2
approve --approver bob --signature sig-b1 rolling s1
until_log "awaiting approval at gate s2 until"
events | grep -E 'GateApproved|s2'
fields

step "A4. 承認 API: 申請者本人・役割違い・役割なし・署名なしは拒否"
post s2 approve '{"approver":"alice","roles":["platform-admin"],"signature":"sig-a"}'
post s2 approve '{"approver":"dave","roles":["developer"],"signature":"sig-d"}'
post s2 approve '{"approver":"erin","signature":"sig-e"}'
post s2 approve '{"approver":"bob","roles":["platform-admin"]}'

step "A5. N-of-M: bob の 2 回の承認は 1 と数える"
post s2 approve '{"approver":"bob","roles":["platform-admin"],"signature":"sig-b2"}'
post s2 approve '{"approver":"bob","roles":["platform-admin"],"signature":"sig-b3"}'
until_log "1 of 2 approval(s); roles"
grep "1 of 2 approval(s); roles" "$LOG" | tail -1 | cut -c1-200

step "A6. 1 of 2 のまま起こし直しても 1 of 2 で待つ"
stop
start d3.log
until_log "cycle 1: WorkflowWaiting"
events

step "A7. carol（2 人目）で通り、s3（ゲートなし）を経て done"
post s2 approve '{"approver":"carol","roles":["auditor","platform-admin"],"signature":"sig-c"}'
until_log "WorkflowCompleted"
events
post s2 approve '{"approver":"frank","roles":["platform-admin"],"signature":"sig-f"}'
fields

step "A8. done のあとは収束に戻る: 手でずらした n2 を直す"
until_log "converged (no changes)"
setf n2 version 1
until_log "RESOLVED Drift fake//n2"
events | grep Drift
fields
stop

step "A9. S4 監査ログ（承認した全員・役割・署名。拒否した承認は残らない）"
audit

# ---------------------------------------------------------------------------
step "B1. 却下（API の reject）で halt。対象は収束に返らない"
setup b
start d1.log
until_log "awaiting approval at gate s1 until"
post s1 reject '{"approver":"mallory","signature":"sig-m"}'
until_log "WorkflowHalted"
setf n1 version 9
sleep 3
events | grep -E 'Halted' | tail -1
echo "n1 を手で version=9 にしたまま（halt した Workflow の対象は収束ループが直さない）:"
fields
post s1 approve '{"approver":"bob","signature":"sig-b"}'

step "B2. sakutei workflow cancel → 次の周に最初のステージからやり直す"
"$S" workflow cancel --store wf.json --audit-log audit.jsonl rolling
i=0; while [ "$(grep -c StageAwaitingApproval "$LOG")" -lt 2 ] && [ $i -lt 100 ]; do i=$((i + 1)); sleep 0.1; done
events | tail -6
fields

step "B3. CLI の sakutei reject でも halt"
"$S" reject --store wf.json --audit-log audit.jsonl --approver mallory --signature sig-m2 rolling s1
sleep 2
events | grep Halted | tail -1
stop
"$S" workflow list --store wf.json
audit

# ---------------------------------------------------------------------------
step "C1. ゲートの timeout（s1 を 6s に縮める）: 承認が無ければ halt"
setup c
sed -i '0,/timeout: 24h/s//timeout: 6s/' intent/rolling.yaml
start d1.log
until_log "awaiting approval at gate s1 until"
until_log "timed out" 15
events | grep -E 'Awaiting|timed out' | head -3
post s1 approve '{"approver":"bob","signature":"sig-b"}'
stop
audit

step "C2. デーモンが止まっている間に期限が過ぎても、承認は受け付けず、起こすと halt"
"$S" workflow cancel --store wf.json --audit-log audit.jsonl rolling
setf n1 version 1
start d2.log
until_log "awaiting approval at gate s1 until"
stop
sleep 7
approve --approver bob --signature sig-b rolling s1
start d3.log
until_log "timed out"
events
stop

# ---------------------------------------------------------------------------
step "D1. 不変条件 min_active: 2 — n2 が外で inactive だと n1 の drain に進まない"
setup d
setf n2 active false
start d1.log
until_log "InvariantBlocked"
sleep 2
events | head -3
fields

step "D2. 現実が戻れば進む（level-triggered）"
setf n2 active true
until_log "awaiting approval at gate s1 until"
events | tail -6
fields
stop

echo
echo "完了: $W"
