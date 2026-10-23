#!/bin/sh
# import と Sync Mode を手で試す手順を、1 回で通して流すスクリプト（docs/trial/sync-import/README.md）。
# 使い方: docs/trial/sync-import/run.sh [作業ディレクトリ]   （既定 /tmp/sakutei-trial-sync）
# 作業ディレクトリは毎回作り直す。リポジトリのファイルは書き換えない。git はローカルのリポジトリだけを使う。
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../../.." && pwd)
W=${1:-/tmp/sakutei-trial-sync}

step() { printf '\n==== %s ====\n' "$*"; }
# 人のコミットに使う名前（Sync のコミットと見分けるため）
export GIT_AUTHOR_NAME=alice GIT_AUTHOR_EMAIL=alice@example.com
export GIT_COMMITTER_NAME=alice GIT_COMMITTER_EMAIL=alice@example.com
LOG='%h %an <%ae> | %cn <%ce> | sig=%G? | %s'

rm -rf "$W"
mkdir -p "$W"
step "0. ビルド"
(cd "$REPO" && go build -o "$W/sakutei" ./cmd/sakutei)
S="$W/sakutei"

# ---------------------------------------------------------------- import
mkdir "$W/import"
cd "$W/import"
cp "$HERE/initial/example.com.zone" zone

step "1. import（dnszone の現実を intent に書き出す）→ 直後の plan は差分ゼロ"
$S import --provider dnszone --zone zone --out intent --samples 3 --sample-interval 1s
cat intent/imported-dnszone.yaml
cp zone zone.before
$S plan --intent intent --zone zone
cmp -s zone zone.before && echo "→ import も plan もゾーンファイルを書き換えていない"

step "2. 既存の出力は --force なしでは上書きしない"
$S import --provider dnszone --zone zone --out intent --samples 1 || echo "→ 終了コード $?（上書きを拒否）"

step "3. managed 候補の印（観測のあいだに値が変わるレコード）"
# dyn の値を 0.4 秒おきに変える「DHCP のような外の誰か」
( for i in 2 3 4 5 6 7; do sleep 0.4; sed -i "s/^dyn\(.*\) 10\.0\.0\.[0-9]*$/dyn\1 10.0.0.$i/" zone; done ) &
$S import --provider dnszone --zone zone --out intent --samples 3 --sample-interval 1s --force
wait
grep -B1 -A1 'managed 候補: 観測' intent/imported-dnszone.yaml

step "4. dnszone の --scope（dnszone のレコードは root scope なので、ラベルで絞ると何も残らない）"
$S import --provider dnszone --zone zone --out scoped --scope zone=example.com --samples 1

# ---------------------------------------------------------------- Sync Mode
step "5. Sync Mode の準備：bare の origin と、人が intent をコミットしたクローン"
git init -q --bare "$W/origin.git"
git clone -q "$W/origin.git" "$W/repo" 2>/dev/null
cd "$W/repo"
git config commit.gpgsign true        # 人の設定で署名が既定でも、Sync は署名しない（§29.1）
git config user.signingkey no-such-key
mkdir intent
cp "$HERE/intent/scopes.yaml" intent/
git add intent
git -c commit.gpgsign=false commit -q -m "intent: legacy を mode: sync にする"
git push -q origin HEAD 2>/dev/null
cp "$HERE/initial/state.json" "$W/state.json"
STATE="$W/state.json"
cat intent/scopes.yaml

step "6. 1 回目の sync：legacy の現実が generated/ に書かれ、sakutei-sync が署名なしでコミットする"
sum0=$(sha256sum "$STATE" | cut -d' ' -f1)
$S sync --intent intent --provider fake --state "$STATE" --samples 1
git log --format="$LOG"
git show --stat --format='%B' HEAD | sed -n '1,12p'
cat generated/legacy/sync-fake.yaml

step "7. 現実が変わらなければコミットしない"
$S sync --intent intent --provider fake --state "$STATE" --samples 1
echo "コミット数: $(git rev-list --count HEAD)"

step "8. 常駐（--interval 1s）で回し、現実を手で変える"
$S sync --intent intent --provider fake --state "$STATE" --samples 1 --interval 1s > "$W/sync-daemon.log" 2>&1 &
PID=$!
sleep 1.5
# legacy の db-1 の size を手で 4 → 8 に
sed -i 's/"db-1": {"fields": {"name": "db-1", "size": 4}}/"db-1": {"fields": {"name": "db-1", "size": 8}}/' "$STATE"
sleep 2.5
# legacy に cache-1 を足す
sed -i 's/"resources": {/"resources": {"cache-1": {"fields": {"name": "cache-1", "size": 1}}, /; s/"scopes": {/"scopes": {"cache-1": {"site": "legacy"}, /' "$STATE"
sleep 2.5
# legacy の外（tokyo）の web-1 を変える → 記録しない
sed -i 's/"web-1": {"fields": {"name": "web-1", "size": 2}}/"web-1": {"fields": {"name": "web-1", "size": 3}}/' "$STATE"
sleep 2.5
kill $PID; wait $PID 2>/dev/null || true
grep -v 'unchanged\|nothing to commit' "$W/sync-daemon.log"
echo "（unchanged / nothing to commit の行: $(grep -c 'nothing to commit' "$W/sync-daemon.log") 周）"
git log --format="$LOG"
git log -p -2 --format='--- %h %s' -- generated/ | grep '^[-+] \|^--- [0-9a-f]'

step "9. Sync は現実に書かない（状態ファイルは手で変えた分だけ）"
cp "$STATE" "$W/state.after"
$S sync --intent intent --provider fake --state "$STATE" --samples 1 >/dev/null
cmp -s "$STATE" "$W/state.after" && echo "→ sync の前後で状態ファイルは同じ"
[ "$sum0" != "$(sha256sum "$STATE" | cut -d' ' -f1)" ] && echo "（最初からの違いは手順 8 の手の変更だけ）"

step "10. 揺れるだけのフィールド（managed 候補）ではコミットしない"
# lb-1 を legacy に足してから 1 回記録
sed -i 's/"resources": {/"resources": {"lb-1": {"fields": {"name": "lb-1", "size": 1}}, /; s/"scopes": {/"scopes": {"lb-1": {"site": "legacy"}, /' "$STATE"
$S sync --intent intent --provider fake --state "$STATE" --samples 1 | tail -1
n0=$(git rev-list --count HEAD)
flap() { ( for i in 1 2 3 4 5 6 7 8; do sleep 0.3; sed -i "s/\"lb-1\": {\"fields\": {\"name\": \"lb-1\", \"size\": [0-9]*}}/\"lb-1\": {\"fields\": {\"name\": \"lb-1\", \"size\": $i}}/" "$STATE"; done ) & }
flap; $S sync --intent intent --provider fake --state "$STATE" --samples 3 --sample-interval 1s | tail -1; wait
grep -A1 "# managed 候補: 観測" generated/legacy/sync-fake.yaml; grep "^# flapping-fields" generated/legacy/sync-fake.yaml
flap; $S sync --intent intent --provider fake --state "$STATE" --samples 3 --sample-interval 1s | tail -1; wait
$S sync --intent intent --provider fake --state "$STATE" --samples 3 --sample-interval 0.2s | tail -1
echo "コミット数: $n0 → $(git rev-list --count HEAD)（印を残す 1 回だけ）"
# 揺れるフィールドのほかが変わればコミットし、そのとき落ち着いた値も記録される
sed -i 's/"db-1": {"fields": {"name": "db-1", "size": 8}}/"db-1": {"fields": {"name": "db-1", "size": 16}}/' "$STATE"
$S sync --intent intent --provider fake --state "$STATE" --samples 1 | tail -1

step "11. --committer-name / --committer-email"
sed -i 's/"db-1": {"fields": {"name": "db-1", "size": 16}}/"db-1": {"fields": {"name": "db-1", "size": 32}}/' "$STATE"
$S sync --intent intent --provider fake --state "$STATE" --samples 1 --committer-name sync-bot --committer-email sync-bot@dc.example
git log -1 --format="$LOG"

step "12. 人が生成物を手で編集すると上書きしない（ハッシュが合わない）"
sed -i 's/size: "32"/size: "64"/' generated/legacy/sync-fake.yaml
$S sync --intent intent --provider fake --state "$STATE" --samples 1 || echo "→ 終了コード $?"
git checkout -q -- generated/legacy/sync-fake.yaml
$S sync --intent intent --provider fake --state "$STATE" --samples 1 | tail -1

step "13. 人が生成物をコミットで変えると止まる（ハッシュを合わせ直していても）"
python3 - generated/legacy/sync-fake.yaml <<'PY'
import hashlib, sys
p = sys.argv[1]
lines = open(p, encoding="utf-8").read().split("\n")
i = next(i for i, l in enumerate(lines) if l.startswith("# body-sha256: "))
body = "\n".join(lines[i+1:]).replace('size: "32"', 'size: "64"')
lines = lines[:i] + ["# body-sha256: " + hashlib.sha256(body.encode()).hexdigest()]
open(p, "w", encoding="utf-8").write("\n".join(lines) + "\n" + body)
PY
git -c commit.gpgsign=false commit -q -am "手で db-1 を 64 に"
$S sync --intent intent --provider fake --state "$STATE" --samples 1 || echo "→ 終了コード $?"
git -c commit.gpgsign=false revert --no-edit HEAD >/dev/null
$S sync --intent intent --provider fake --state "$STATE" --samples 1 | tail -1
git log --format="$LOG" -4

step "14. 人がステージしているものは Sync のコミットに入らない"
echo "メモ" > NOTES.md
git add NOTES.md
sed -i 's/"db-1": {"fields": {"name": "db-1", "size": 32}}/"db-1": {"fields": {"name": "db-1", "size": 33}}/' "$STATE"
$S sync --intent intent --provider fake --state "$STATE" --samples 1 | tail -1
git show --stat --format='%an: %s' HEAD | grep -v '^$'
git status --short

step "15. List が不完全なら書かずに前のスナップショットを残す"
sed -i 's/"provider_version"/"list_partial": true, "provider_version"/' "$STATE"
sed -i 's/"db-1": {"fields": {"name": "db-1", "size": 33}}/"db-1": {"fields": {"name": "db-1", "size": 1}}/' "$STATE"
$S sync --intent intent --provider fake --state "$STATE" --samples 1 || echo "→ 終了コード $?"
grep -A7 'id: "db-1"' generated/legacy/sync-fake.yaml | grep size
echo '→ 現実の db-1 は 1 にしたが、記録は 33 のまま'

step "16. dnszone を mode: sync の scope で回す（root scope のレコードは scope に入らない）"
git init -q "$W/dnsrepo"
cd "$W/dnsrepo"
mkdir intent
cat > intent/scopes.yaml <<'YAML'
scopes:
  - scope:
      zone: example.com
    mode: sync
    sync:
      target_path: generated/dns/
YAML
cp "$HERE/initial/example.com.zone" zone
$S sync --intent intent --provider dnszone --zone zone --samples 1
tail -2 generated/dns/sync-dnszone.yaml

step "おわり（作業ディレクトリ: $W）"
