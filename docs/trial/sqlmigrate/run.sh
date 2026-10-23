#!/bin/bash
# sqlmigrate（SQLite のマイグレーション）で単純 Task（§6.2、strategy: task）を、本物の sqlite3 と
# 常駐の sakutei（--task-store）で試す手順を、1 回で通して流すスクリプト（docs/trial/sqlmigrate/README.md の手順 1〜6）。
# 使い方: docs/trial/sqlmigrate/run.sh [作業ディレクトリ]   （既定 /tmp/sakutei-sqlmigrate）
# 作業ディレクトリは毎回作り直す。リポジトリのファイルは書き換えない。sqlite3 と Go が要る。通しで 2 分ほど。
# 0002 はわざと遅い（数秒）。手順 1 と 5 は、0002 を流している最中にデーモンが周を跨ぐのを使う。
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../../.." && pwd)
W=${1:-/tmp/sakutei-sqlmigrate}

step() { printf '\n==== %s ====\n' "$*"; }
note() { printf -- '→ %s\n' "$*"; }
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
# count <ファイル> <正規表現>
count() { grep -Ec "$2" "$1" || true; }
# cycles <ログ>: 何周回ったか。
cycles() { count "$1" 'cycle [0-9]+: observed'; }
# after_cycles <ログ> <n>: いまから n 周回るまで待つ。
after_cycles() { local n; n=$(cycles "$1"); wait_for 30 "$2 周" "test \$(cycles '$1') -ge $((n + $2))"; }

# ver <ディレクトリ>: DB の版（schema_migrations の最大の版。DB が無ければ none）。
ver() {
	if [ ! -f "$1/data/app.db" ]; then echo none; return; fi
	sqlite3 "$1/data/app.db" 'SELECT coalesce(max(version), 0) FROM schema_migrations;' 2>/dev/null || echo "?"
}
# rows <ディレクトリ>: schema_migrations の中身と、表の一覧。
rows() {
	echo "  \$ sqlite3 data/app.db 'select * from schema_migrations'"
	sqlite3 "$1/data/app.db" 'select * from schema_migrations' | sed 's/^/  /'
	echo "  （表: $(sqlite3 "$1/data/app.db" "select group_concat(name, ' ') from (select name from sqlite_master where type = 'table' order by name)")）"
	echo "  （users の列: $(sqlite3 "$1/data/app.db" "select group_concat(name, ' ') from pragma_table_info('users')")）"
}
# tasks <ディレクトリ>: sakutei task list。
tasks() { echo "  \$ sakutei task list --store tasks.json"; (cd "$1" && "$S" task list --store tasks.json) | sed 's/^/  /'; }
# launched <ログ>: Task を起動した回数。
launched() { count "$1" 'RequiresTaskEvent: .* launched \(strategy: task'; }

PIDS=""
cleanup() { for p in $PIDS; do kill -9 "$p" 2>/dev/null || true; done; wait 2>/dev/null || true; }
trap cleanup EXIT
# start <ディレクトリ> <ログ> [引数…]: そのディレクトリで常駐の sakutei を起動する（DPID に pid）。
# 自分のプロセスグループで起動する（手順 5 でグループごと殺すため）。
start() {
	d=$1 log=$2; shift 2
	(cd "$d" && exec setsid "$S" --intent intent --sqlmigrate-config databases.yaml --task-store tasks.json --interval 1s "$@") >"$d/$log" 2>&1 &
	DPID=$!
	PIDS="$PIDS $DPID"
	wait_for 10 "最初の周" "test \$(cycles '$d/$log') -ge 1"
}
# stop: 最後に起動したデーモンを止める（Ctrl-C と同じ SIGINT。走っている Task は待つ）。
stop() { kill -INT "$DPID"; wait "$DPID" 2>/dev/null || true; }

# mk <名前>: 設定・マイグレーション・空の intent/ と data/ を持つディレクトリを作る。
mk() {
	rm -rf "$W/$1"; mkdir -p "$W/$1/intent" "$W/$1/data"
	cp "$HERE/databases.yaml" "$W/$1/"; cp -r "$HERE/migrations" "$W/$1/"
	echo "$W/$1"
}
# intent <ディレクトリ> <版> [window] [rollback_on_fail]: app の版を宣言する intent を書く。
# 書き換えは rename で入れ替える（デーモンが書きかけを読まないように）。
intent() {
	{
		echo 'version: "1"'
		echo 'resources:'
		echo '  - provider: sqlmigrate'
		echo '    id: app'
		echo '    update_policy:'
		echo '      strategy: task'
		if [ -n "${3:-}" ]; then echo "      window: \"$3\""; fi
		if [ -n "${4:-}" ]; then echo "      rollback_on_fail: $4"; fi
		echo '    fields:'
		echo "      version: \"$2\""
	} >"$1/intent/.main.yaml.new"
	mv "$1/intent/.main.yaml.new" "$1/intent/main.yaml"
}
# show <ログ> <正規表現>: ログの行のうち、知りたい行だけを出す（長い Task の ID は短くする）。
show() { grep -E "$2" "$1" | sed -E 's/task [^ ]+-([0-9]+-[0-9]+)( |$)/task …-\1\2/g; s/^/  /'; }
EV='launched|completed|RequiresTaskEvent|Drift|RESOLVED|awaiting|converged|outstanding|outside'

rm -rf "$W"
mkdir -p "$W"

step "0. ビルド"
(cd "$REPO" && go build -o "$W/sakutei" ./cmd/sakutei)
S=$W/sakutei
sqlite3 --version
echo "今: $(date -u '+%a %H:%M:%S') UTC"

step "1. 版 0 → 3：Task は 1 回だけ起動し（0002 を流している間に周を跨いでも二重に起動しない）、完了印を観測して記録が片づく"
d=$(mk 1-converge)
intent "$d" 3
(cd "$d" && "$S" plan --intent intent --sqlmigrate-config databases.yaml) | sed 's/^/  /'
start "$d" daemon.log
wait_for 10 "0001 が通る" "test \$(ver '$d') = 1"
echo "--- 0002 を流している最中（版 $(ver "$d")）"
tasks "$d"
wait_for 60 "版 3" "test \$(ver '$d') = 3"
wait_for 10 "記録が片づく" "(cd '$d' && '$S' task list --store tasks.json) | grep -q 'no outstanding task'"
after_cycles "$d/daemon.log" 3
stop
echo "--- デーモンの出力（抜粋）"
show "$d/daemon.log" "$EV|cycle [0-9]+: observed"
note "Task の起動: $(launched "$d/daemon.log") 回、Task の実行中に回った周（awaiting）: $(count "$d/daemon.log" 'awaiting their Task') 周"
tasks "$d"
rows "$d"

step "2. メンテ窓：今を含まない窓では起動しない。今を含む窓に書き換えると起動する"
d=$(mk 2-window)
OUT="$(date -u -d '+2 hour' +%H:%M)-$(date -u -d '+3 hour' +%H:%M) UTC"
IN="$(date -u -d '-30 min' +%H:%M)-$(date -u -d '+30 min' +%H:%M) UTC"
intent "$d" 1 "$OUT"
echo "--- window: \"$OUT\"（今 $(date -u +%H:%M) UTC を含まない）"
(cd "$d" && "$S" plan --intent intent --sqlmigrate-config databases.yaml) | grep -E 'task:' | sed 's/^/  /'
start "$d" daemon.log
after_cycles "$d/daemon.log" 4
show "$d/daemon.log" "$EV"
note "版 $(ver "$d")（DB を作りもしない）、Task の起動 $(launched "$d/daemon.log") 回"
tasks "$d"
echo "--- window: \"$IN\"（今を含む）に書き換える（デーモンはそのまま）"
intent "$d" 1 "$IN"
n=$(wc -l <"$d/daemon.log")
wait_for 20 "版 1" "test \$(ver '$d') = 1"
wait_for 10 "記録が片づく" "grep -q 'completed (completion mark' '$d/daemon.log'"
stop
tail -n +$((n + 1)) "$d/daemon.log" | grep -E "$EV" | sed -E 's/task [^ ]+-([0-9]+-[0-9]+)( |$)/task …-\1\2/g; s/^/  /'
note "版 $(ver "$d")、Task の起動 $(launched "$d/daemon.log") 回"

step "3a. 壊れた 0003 で失敗 → halt（rollback_on_fail なし）。0001・0002 は残り、再起動しない"
d=$(mk 3-fail)
cp "$HERE/broken/0003_create_orders.up.sql" "$d/migrations/app/"
intent "$d" 3
start "$d" daemon.log
wait_for 60 "失敗" "(cd '$d' && '$S' task list --store tasks.json) | grep -q failed"
after_cycles "$d/daemon.log" 5
show "$d/daemon.log" "$EV|cycle [0-9]+: observed"
note "Task の起動 $(launched "$d/daemon.log") 回（失敗のあと $(count "$d/daemon.log" 'cycle [0-9]+: observed') 周目まで、起動し直していない）"
tasks "$d"
rows "$d"
FAILD=$d FAILPID=$DPID  # 手順 4・6 はこのデーモンのまま続ける

step "3b. rollback_on_fail: true。版 1 から 3 を宣言し、0003 で失敗 → 起動したときの版 1 まで down で戻る"
d=$(mk 3b-rollback)
cp "$HERE/broken/0003_create_orders.up.sql" "$d/migrations/app/"
intent "$d" 1
start "$d" daemon.log
wait_for 20 "版 1 に収束" "grep -q 'completed (completion mark' '$d/daemon.log'"
echo "--- 版 $(ver "$d") に収束した。rollback_on_fail: true で 3 を宣言する"
intent "$d" 3 "" true
wait_for 60 "失敗" "(cd '$d' && '$S' task list --store tasks.json) | grep -q failed"
after_cycles "$d/daemon.log" 3
stop
show "$d/daemon.log" "$EV"
note "Task の起動 $(launched "$d/daemon.log") 回（版 1 への 1 回と、3 への 1 回）"
tasks "$d"
rows "$d"

step "4. 0003 を直して sakutei task cancel → 差分が残っているので、デーモンが改めて Task を起動して版 3 に収束"
d=$FAILD DPID=$FAILPID
cp "$HERE/migrations/app/0003_create_orders.up.sql" "$d/migrations/app/"
note "0003_create_orders.up.sql を直した（まだ cancel していない）"
after_cycles "$d/daemon.log" 2
note "直しただけでは起動しない: 版 $(ver "$d")、Task の起動 $(launched "$d/daemon.log") 回"
n=$(wc -l <"$d/daemon.log")
echo "  \$ sakutei task cancel --store tasks.json sqlmigrate//app"
(cd "$d" && "$S" task cancel --store tasks.json sqlmigrate//app) | sed -E 's/task [^ ]+-([0-9]+-[0-9]+) /task …-\1 /; s/^/  /'
wait_for 20 "版 3" "test \$(ver '$d') = 3"
wait_for 10 "記録が片づく" "(cd '$d' && '$S' task list --store tasks.json) | grep -q 'no outstanding task'"
after_cycles "$d/daemon.log" 1
tail -n +$((n + 1)) "$d/daemon.log" | grep -E "$EV" | sed -E 's/task [^ ]+-([0-9]+-[0-9]+)( |$)/task …-\1\2/g; s/^/  /'
note "Task の起動 $(launched "$d/daemon.log") 回（失敗した 1 回と、cancel のあとの 1 回）"
rows "$d"

step "6. 版を下げる（3 → 1）：down を逆順（0003 → 0002）に流す"
n=$(wc -l <"$d/daemon.log")
intent "$d" 1
(cd "$d" && "$S" plan --intent intent --sqlmigrate-config databases.yaml) | grep -E 'UPDATE|version|task:' | sed 's/^/  /'
wait_for 30 "版 1" "test \$(ver '$d') = 1"
wait_for 10 "記録が片づく" "(cd '$d' && '$S' task list --store tasks.json) | grep -q 'no outstanding task'"
after_cycles "$d/daemon.log" 1
stop
tail -n +$((n + 1)) "$d/daemon.log" | grep -E "$EV" | sed -E 's/task [^ ]+-([0-9]+-[0-9]+)( |$)/task …-\1\2/g; s/^/  /'
rows "$d"

# kill5 <名前> <殺し方の説明> <kill の引数の頭>: 0002 を流している最中にデーモンを SIGKILL し、起こし直す。
kill5() {
	d=$(mk "$1")
	intent "$d" 3
	start "$d" daemon.log
	wait_for 10 "0001 が通る" "test \$(ver '$d') = 1"
	sleep 1
	echo "--- 0002 を流している最中（版 $(ver "$d")）に $2"
	echo "  sqlite3 のプロセス（殺す前）: $(pgrep -f "^sqlite3 -batch -bail .*\./data/app\.db$" -d ' ' || echo なし)"
	kill -9 "$3$DPID"
	wait "$DPID" 2>/dev/null || true
	echo "  sqlite3 のプロセス（殺した直後）: $(pgrep -f "^sqlite3 -batch -bail .*\./data/app\.db$" -d ' ' || echo なし)"
	note "殺した直後の版 $(ver "$d")"
	sleep 6  # 残った sqlite3 があれば、それが終わるのを待つ
	note "6 秒後の版 $(ver "$d")"
	tasks "$d"
	rows "$d"
	echo "  integrity_check: $(sqlite3 "$d/data/app.db" 'pragma integrity_check')"
	echo "--- デーモンを起こし直す"
	start "$d" daemon2.log
	after_cycles "$d/daemon2.log" 5
	show "$d/daemon2.log" "$EV|cycle [0-9]+: observed"
	note "起こし直したデーモンの Task の起動 $(launched "$d/daemon2.log") 回、版 $(ver "$d")"
	echo "--- 人が確かめて cancel する"
	(cd "$d" && "$S" task cancel --store tasks.json sqlmigrate//app) | sed -E 's/task [^ ]+-([0-9]+-[0-9]+) /task …-\1 /; s/^/  /'
	wait_for 30 "版 3" "test \$(ver '$d') = 3"
	wait_for 10 "記録が片づく" "(cd '$d' && '$S' task list --store tasks.json) | grep -q 'no outstanding task'"
	stop
	note "cancel のあと Task の起動 $(launched "$d/daemon2.log") 回、版 $(ver "$d")"
}

step "5a. 0002 の実行中にデーモンだけを SIGKILL → 起こし直したデーモンは自動で再実行しない"
kill5 5a-kill-daemon "デーモンだけを kill -9" ""

step "5b. 0002 の実行中にデーモンをプロセスグループごと SIGKILL（子の sqlite3 も殺す。電源断に近い）"
kill5 5b-kill-group "プロセスグループごと kill -9" "-"

step "おわり"
echo "作業ディレクトリ: $W（各手順のディレクトリに daemon.log・tasks.json・data/app.db が残る）"
