# remediation schedule（直してよい時間帯）と運用プロファイル（cooling-off を含む）を手で試す

remediation schedule（設計 §14.2）・mode の決め方（most-restrictive-wins、§14.3）・運用プロファイル（solo / team / enterprise、§14.5）と
solo の cooling-off（§13.3「承認と運用プロファイル」）を、実際の CLI（`plan`・`reconcile`）と常駐デーモンで回して確かめる手順と、
2026-10-23 にこのコンテナで回した結果。相手の実 provider は dnszone（ゾーンファイル）。外のサービスには繋がない。

## まず知っておくこと

- **窓は本物の時計で決まる**。テスト用に時計を差し替える口は CLI に無いので、この手順は窓を「今の時刻」から作る（だから intent は
  `run.sh` が書く）。手順 2 は次の分の頭から 1 分だけの窓を作り、窓が開いて閉じるのを実際に待つ。
- 窓は `"*"`・`"HH:MM-HH:MM"`（毎日）・`"<曜日> HH:MM-HH:MM"`（`Sat`・`Mon-Fri`・`Sat,Sun`）。始めは含み、終わりは含まない。
  終わりが始め以前なら日付をまたぎ、曜日は**窓が開く日**（`"Fri 22:00-02:00"` は金曜 22:00〜土曜 02:00）。先に書いた窓が勝ち、
  どれにも入らなければ `default`（書かなければ alert-only）。`tz` を書かなければ UTC。
- 曜日・日付またぎを確かめる窓は、今がその土地の昼ごろになる `Etc/GMT±N` で書く（UTC の 0 時前後に流しても、±1 時間の窓が日付を
  またがないように）。
- 運用プロファイルの効き目は planned_change のレーン（§13.3）で見る。dnszone の List は scope を持たない（`dnszone//…`）ので、
  scope を要る planned_change で prune の削除を通すことはできない。そこで **scope `pool: web` を付けた 3 件の A レコードの
  値を変える（kind: update）** を、`--max-changes-per-cycle 1` の下で通す。通常の予算では 3 件とも止まり（1 件だけ通すこともしない）、
  レーンが開けば 3 件とも通る。
- 1 回収束の `reconcile` がレーンを開くのは `--audit-log`（S4）を付けたときだけ（S4 が回をまたいで cooling-off の時計と `max_count` の
  消費を持つ）。

## 確かめる観点（設計書と受け入れテストから挙げたもの）

| # | 観点 | 設計 | 手順 |
|---|---|---|---|
| 1 | 今を含む窓は auto で直す。含まない窓は alert-only で「would be」と知らせるだけ・終了コード 0。default・先勝ち・曜日・日付またぎ・tz | §14.2 | 1 |
| 2 | 常駐：窓の外のずれは持ち越され、窓が開いた周に直る（level-triggered）。窓が閉じたら、新しいずれは直さない。mode の切り替わりを理由つきでログに出す | §14.2 | 2 |
| 3 | `--mode auto / alert-only`、`--freeze`。schedule が auto でも、`--mode alert-only`・freeze・scope 明示の alert-only のどれか 1 つで止まる。値の誤りは拒否 | §14.1・§14.3 | 3 |
| 4 | `remediation:` の誤り（時刻・曜日・空の窓・mode・tz・default・2 ファイルでの宣言）はロードで拒否 | §14.2 | 4 |
| 5 | 運用プロファイルの既定：team（書かなければ）= 承認者 1 人、enterprise = 2-of-2 + 申請者≠承認者、solo = 要件なし + cooling-off 2h。宣言の `requirements`・`cooling_off` が既定に勝つ。enterprise は requirements の無いゲートに applicant を求める。書式の誤りは拒否 | §14.4・§14.5 | 5 |
| 6 | cooling-off の時計は S4：宣言を初めて見た時刻から数え、デーモンを起こし直しても延びも縮みもしない。S4 が無ければ閉じたまま。開く前に消すと取り下げで、書き戻すとやり直し。窓の外（alert-only）でも時計は動く | §13.3・§14.5 | 6 |

## 置いてあるもの（docs/trial/remediation-profile/）

| ファイル | 中身 |
|---|---|
| `initial/example.com.zone` | 最初の現実。`www` は 192.0.2.99（intent は 192.0.2.10 なので、ずれている）、`a1`〜`a3` は 192.0.2.1〜3 |
| `run.sh` | 手順 1〜6 を通して流すスクリプト。intent は今の時刻から書く |

## 通しで流す

```sh
docs/trial/remediation-profile/run.sh            # 作業ディレクトリは /tmp/sakutei-remediation（毎回作り直す）
SKIP2=1 docs/trial/remediation-profile/run.sh    # 手順 2（本物の時計で 2〜3 分待つ）を飛ばす
```

Go が要る。リポジトリのファイルは書き換えない。通しで 4 分ほど（手順 2 が 2〜3 分、手順 6 が 1 分半）。

## 1 つずつ流す

作業ディレクトリで、`run.sh` が書く intent と同じものを手で書く。例（今が UTC 15:46 なら）：

```sh
W=/tmp/sakutei-remediation; mkdir -p $W/intent; cp docs/trial/remediation-profile/initial/example.com.zone $W/
go build -o $W/sakutei ./cmd/sakutei; cd $W
cat > intent/main.yaml <<'EOF'
version: "1"
remediation:
  default: alert-only
  schedule:
    - window: "15:47-15:48"     # 次の分の頭から 1 分だけ auto
      mode: auto
resources:
  - provider: dnszone
    id: www.example.com./A
    fields:
      name: www.example.com.
      type: A
      value: 192.0.2.10
      ttl: "300"
EOF
./sakutei reconcile --intent intent --zone example.com.zone           # 窓の外：Remediation mode: alert-only
./sakutei --intent intent --zone example.com.zone --interval 2s       # 常駐：15:47 の周に直る
```

cooling-off（手順 6）は：

```yaml
profile: solo
solo:
  cooling_off: 40s
planned_change:
  scope:
    pool: web
  kind: update
  max_count: 3
  window: "2026-10-23 14:48 .. 2026-10-23 18:48"
```

を置いて `./sakutei --intent intent --zone example.com.zone --interval 2s --max-changes-per-cycle 1 --audit-log audit.jsonl`
を起動し、途中で Ctrl-C で止めて起こし直す。

## 回した結果（2026-10-23、このコンテナ）

`run.sh` の出力の抜粋。全文は `run.sh` を流せば同じものが出る（時刻以外）。流したときの時計は
`Fri 15:53 UTC / Fri 12:53 Etc/GMT+3 / Sat 00:53 Asia/Tokyo`。

### 1. 時間帯（1 回収束）

```
[今を含む窓]                Remediation mode: auto (--mode auto, schedule window "12:23-13:23" (auto))
                             www=192.0.2.10（終了コード 0）
[今を含まない窓]            Remediation mode: alert-only (schedule default (alert-only))
                             www=192.0.2.99（終了コード 0）
    Reconcile (alert-only): 1 drifted resource(s) detected — no changes applied
      DRIFT EVENT: would be UPDATE dnszone//www.example.com./A
[default: auto・窓の外]     Remediation mode: auto (--mode auto, schedule default (auto))
[先に書いた窓が勝つ]        Remediation mode: alert-only (schedule window "12:23-13:23" (alert-only))
[今日の曜日]                Remediation mode: auto (--mode auto, schedule window "Fri 12:23-13:23" (auto))
[明日の曜日]                Remediation mode: alert-only (schedule default (alert-only))
[昨日開いて今日閉じる窓]    Remediation mode: auto (--mode auto, schedule window "Thu 13:24-13:23" (auto))
[今日開いて明日閉じる窓]    Remediation mode: alert-only (schedule default (alert-only))
[tz: Asia/Tokyo の窓]       Remediation mode: auto (--mode auto, schedule window "00:23-01:23" (auto))
[同じ窓を tz 無し（UTC）で] Remediation mode: alert-only (schedule default (alert-only))
```

- `default` を書かない schedule は窓の外で alert-only（保守側）。`default: auto` を書けば窓の外は auto。
- `"Thu 13:24-13:23"` は木曜 13:24 に開いて金曜 13:23 に閉じる窓なので、金曜 12:53 を含む。`"Fri 13:24-13:23"` は金曜 13:24 に開くので含まない（設計どおり）。
- 同じ `"00:23-01:23"` が `tz: Asia/Tokyo` では今を含み、tz 無し（UTC）では含まない。ホストの tz（このコンテナは UTC）には寄らない。

### 2. 常駐：窓が開くと直し、閉じるとまた直さない

```
remediation:
  default: alert-only
  schedule:
    - window: "15:55-15:56"
      mode: auto
--- 窓の前（15:54:04）
sakutei: convergence loop started (interval=2s, intent=intent, state=sakutei-fake-state.json, mode=auto)
sakutei: cycle 1: observed=1 drifted (alert-only, no changes)
sakutei: cycle 1: remediation mode=alert-only (schedule default (alert-only))
sakutei: cycle 1: alert-only: 1 drifted resource(s) detected — no changes applied
sakutei: cycle 1: DRIFT EVENT: would be UPDATE dnszone//www.example.com./A
→ www=192.0.2.99（直していない）
--- 窓が開いた（15:55:01）
sakutei: cycle 29: remediation mode=auto (--mode auto, schedule window "15:55-15:56" (auto))
→ www=192.0.2.10（窓が開いた周に直した）
--- 窓が閉じた（15:56:00）
sakutei: cycle 57: remediation mode=alert-only (schedule default (alert-only))
→ ゾーンの www を手で 192.0.2.77 に書き換えた
sakutei: cycle 58: DRIFT EVENT: would be UPDATE dnszone//www.example.com./A
sakutei: cycle 60: alert-only: 1 drifted resource(s) detected — no changes applied
→ www=192.0.2.77（窓の外なので直さない）
```

窓の外で見つけたずれは何も持たずに持ち越され、15:55:00 を過ぎた最初の周（cycle 29）で直った。mode の行は切り替わった周にだけ出る
（cycle 1・29・57 の 3 回）。起動行の `mode=auto` は `--mode` の値で、schedule を含めた mode ではない（「見つけたこと」の 2）。

### 3. --mode・--freeze・scope 明示の alert-only（schedule は今を含む窓で auto）

```
[--mode auto]                Remediation mode: auto (--mode auto, schedule window "12:26-13:26" (auto))（終了コード 0、www=192.0.2.10）
[--mode alert-only]          Remediation mode: alert-only (--mode alert-only)（終了コード 0、www=192.0.2.99）
[--freeze]                   Remediation mode: alert-only (freeze)（終了コード 0、www=192.0.2.99）
[--mode alert-only --freeze] Remediation mode: alert-only (freeze, --mode alert-only)（終了コード 0、www=192.0.2.99）
sakutei reconcile: invalid --mode "Auto": unknown mode "Auto"; use auto or alert-only            （終了コード 1）
sakutei reconcile: invalid --mode "freeze": unknown mode "freeze"; use auto or alert-only        （終了コード 1）
sakutei: invalid --mode "alertonly": unknown mode "alertonly"; use auto or alert-only            （終了コード 1、常駐）
sakutei reconcile: invalid --mode "": empty mode; use auto or alert-only                          （終了コード 1）
[常駐 --mode alert-only]
  sakutei: cycle 1: remediation mode=alert-only (--mode alert-only)
  sakutei: cycle 1: alert-only: 1 drifted resource(s) detected — no changes applied
[常駐 --freeze]
  sakutei: convergence loop started (… mode=alert-only [FROZEN])
  sakutei: cycle 1: remediation mode=alert-only (freeze)
  sakutei: cycle 1: FROZEN (alert-only): 1 drifted resource(s) detected — no changes applied
--- scope 明示の remediation: alert-only（a1〜a3 は scope tier=db、www は scope なし）
Remediation mode: auto (--mode auto, schedule window "12:26-13:26" (auto))
  DRIFT EVENT: would be UPDATE dnszone/tier=db/a1.example.com./A
  DRIFT EVENT: would be UPDATE dnszone/tier=db/a2.example.com./A
  DRIFT EVENT: would be UPDATE dnszone/tier=db/a3.example.com./A
~ UPDATE dnszone//www.example.com./A
Reconcile: 1 change(s) applied
→ www=192.0.2.10 a1=192.0.2.1（www は直し、tier=db の a1〜a3 は直さない）
```

- 止めた決定源は全部理由に並ぶ（`freeze, --mode alert-only`）。auto の理由は「許した決定源の全部」。
- `--mode=`（空）は、直す前は黙って auto になっていた（「見つけたこと」の 3、直した）。
- `plan` は `--mode` を受け取らない（読むだけなので mode は要らない）。そのかわり、今が窓の外かどうかを `plan` では見られない（「見つけたこと」の 4）。

### 4. remediation: の宣言の誤り

```
[時刻の範囲外] … remediation.schedule[0]: window "Sat 25:00-26:00": time "25:00" is out of range （終了コード 1）
[空の窓]       … window "Sat 02:00-02:00": empty range (start equals end); use "*" for always （終了コード 1）
[曜日の誤り]   … window "Satu 02:00-04:00": unknown day "Satu" (use Sun Mon Tue Wed Thu Fri Sat) （終了コード 1）
[mode が無い]  … intent スキーマ検証エラー:   - $.remediation.schedule[0]: 必須プロパティ "mode" がありません （終了コード 1）
[mode の誤り]  … $.remediation.schedule[0].mode: 許容されない値です: freeze（enum: [auto alert-only]） （終了コード 1）
[tz の誤り]    … remediation.tz: unknown time zone Asia/Tokio （終了コード 1）
[default の誤り] … $.remediation.default: 許容されない値です: sometimes（enum: [auto alert-only]） （終了コード 1）
[2 つのファイルに remediation:] … remediation: declared in both main.yaml and second.yaml; declare the schedule once (§14.2) （終了コード 1）
```

### 5. 運用プロファイルの既定と上書き（1 回収束・--audit-log・上限 1 周 1 件）

```
[team（profile を書かない）・承認なし]
  PlannedChangeUnapproved: planned_change(pool=web update): approval has 0 of the 1 approver(s) required by the team profile; the lane stays closed …
  Reconcile: BUDGET HALTED (no changes applied): rate-limit (per-cycle): 3 total ops (+ 0 in-flight = 3) exceeds max_changes_per_cycle_absolute=1
[team・承認者 1 人]
  PlannedChangeOpened: planned_change(pool=web update): lane opened (profile team; approval 1 of 1 approver(s) [alice] required by the team profile)
  Reconcile: 3 change(s) applied
[team・承認者 1 人・--audit-log なし]
  sakutei reconcile: note: 1 planned_change declaration(s) need --audit-log to open their lanes in a one-shot run …
  Reconcile: BUDGET HALTED (no changes applied): …
[enterprise・承認者 1 人]
  PlannedChangeUnapproved: … approval has 1 of the 2 approver(s) required by the enterprise profile …
[enterprise・申請者本人と 1 人]
  PlannedChangeUnapproved: … approval has 1 of the 2 approver(s) required by the enterprise profile (not counted: separation applicant_ne_approver: carol is the applicant and cannot approve) …
[enterprise・申請者以外の 2 人]
  PlannedChangeOpened: … lane opened (profile enterprise; approval 2 of 2 approver(s) [alice bob] required by the enterprise profile, separation applicant_ne_approver (applicant carol))
[enterprise・宣言の requirements（1-of-1）が既定に勝つ]
  PlannedChangeOpened: … lane opened (profile enterprise; approval 1 of 1 approver(s) [alice] required by the declaration's requirements)
[solo（cooling_off は既定の 2h）]
  PlannedChangeCoolingOff: … declared 2026-10-23T15:48:37Z, cooling-off 2h0m0s until 2026-10-23T17:48:37Z; the lane stays closed until then (remove the declaration to halt it, §14.5)
[solo・solo.cooling_off: 30m]
  PlannedChangeCoolingOff: … cooling-off 30m0s until 2026-10-23T16:18:37Z …
[solo・宣言の cooling_off: 0s が既定に勝つ]
  PlannedChangeOpened: … lane opened (profile solo; no approval required)
[solo・宣言の requirements（2-of-3）は solo でも効く・承認者 1 人]
  PlannedChangeUnapproved: … approval has 1 of the 2 approver(s) required by the declaration's requirements …
--- enterprise の承認ゲート（requirements の無いゲート）と applicant
[enterprise・applicant なし] sakutei plan: load intent: loader: profile enterprise puts quorum 2 + separation on gates without requirements: workflow renumber: stage all: separation applicant_ne_approver needs the workflow's applicant (declare the workflow's applicant, or write the gate's requirements explicitly) （終了コード 1）
[enterprise・applicant あり] workflow: renumber … （終了コード 0）
[team・applicant なし]       workflow: renumber … （終了コード 0）
--- 書式の誤り
[profile: Solo]                      $.profile: 許容されない値です: Solo（enum: [solo team enterprise]） （終了コード 1）
[profile: team + solo.cooling_off]   solo: (in main.yaml) applies only under profile: solo (the profile is team) （終了コード 1）
[solo.cooling_off: -1h]              solo.cooling_off: must not be negative (got -1h0m0s) （終了コード 1）
[solo.delay: 1h]                     $.solo.delay: 未知のプロパティ "delay" は許可されていません （終了コード 1）
[2 つのファイルに profile:]          profile: declared in both main.yaml and second.yaml; declare it once (§14.5) （終了コード 1）
```

レーンが閉じている間、3 件は通常の予算で評価され、1 件も通らない（上限 1 件に 3 件なので halt）。閉じた理由は 1 回だけ出る。

### 6. cooling-off の時計は S4

```
--- 常駐を起動（cooling_off 40s）
sakutei: cycle 1: PlannedChangeCoolingOff: planned_change(pool=web update): declared 2026-10-23T15:56:10Z, cooling-off 40s until 2026-10-23T15:56:50Z; …
sakutei: cycle 1: BUDGET HALTED: rate-limit (per-cycle): 3 total ops (+ 0 in-flight = 3) exceeds max_changes_per_cycle_absolute=1
→ 13 秒で止めた。a1=192.0.2.1（レーンは開いていない）
--- 3 秒おいて起こし直す
sakutei: cycle 1: PlannedChangeCoolingOff: planned_change(pool=web update): declared 2026-10-23T15:56:10Z, cooling-off 40s until 2026-10-23T15:56:50Z; …
→ 起こし直す前と同じ「until 2026-10-23T15:56:50Z」（S4 の planned_change.declared から読み直した）
sakutei: cycle 13: PlannedChangeOpened: planned_change(pool=web update): lane opened (profile solo; cooling-off 40s elapsed since the declaration at 2026-10-23T15:56:10Z)
sakutei: cycle 13: PlannedChange: … 3 change(s) admitted to the planned_change lane (used 0 of max_count 3 before this cycle); not counted in the normal budget
→ 宣言を初めて見てから 41 秒、起こし直してから 25 秒で開いた（cooling-off 40s）。a1=198.51.100.1 a2=198.51.100.2 a3=198.51.100.3
--- S4（audit.jsonl）
{"time":"2026-10-23T15:56:10Z","kind":"planned_change.declared","count":3,"detail":"planned_change(pool=web update) declared (max_count 3, profile solo, pr 0, approvers [])"}
{"time":"2026-10-23T15:56:50Z","kind":"planned_change.opened","count":3,"detail":"… lane opened (profile solo; cooling-off 40s elapsed since the declaration at 2026-10-23T15:56:10Z)"}
{"time":"2026-10-23T15:56:51Z","kind":"planned_change.executed","count":3,"detail":"… 3 change(s) dispatched in the lane (used 3 in total)"}
--- cooling-off は S4 が無ければ評価できない（--audit-log なしの常駐）
sakutei: cycle 1: PlannedChangeCoolingOff: planned_change(pool=web update): cooling-off 1s needs the S4 audit log (--audit-log) to record when it was declared; the lane stays closed
→ a1=192.0.2.1（1 秒経ってもレーンは開かない）
--- デーモンを止めている間に宣言を消す（取り下げ）→ 書き戻すと cooling-off はやり直し
  PlannedChangeWithdrawn: pool=web|update|2026-10-23T14:56:00Z|2026-10-23T18:56:00Z (max_count 3) was removed from intent before its lane opened; writing it back starts its cooling-off over (§14.5)
→ 20 秒以上経ってから同じ宣言を書き戻した
sakutei: cycle 1: PlannedChangeCoolingOff: planned_change(pool=web update): declared 2026-10-23T15:57:19Z, cooling-off 20s until 2026-10-23T15:57:39Z; …
→ 書き戻してから 21 秒で開いた（やり直した 20s の後）
--- S4 の記録の並び
planned_change.declared  planned_change.withdrawn  planned_change.declared  planned_change.opened  planned_change.executed
--- 窓の外（alert-only）の回でも宣言を見たことは S4 に残り、cooling-off の時計は動く（cooling_off 5s）
  Remediation mode: alert-only (schedule default (alert-only))
→ （終了コード 0、a1=192.0.2.1）。S4: planned_change.declared
→ 6 秒後に、窓を今を含む帯に書き換えた（窓が開いたのと同じ）
  Remediation mode: auto (--mode auto, schedule window "12:29-13:29" (auto))
  PlannedChangeOpened: planned_change(pool=web update): lane opened (profile solo; cooling-off 5s elapsed since the declaration at 2026-10-23T15:59:41Z)
  Reconcile: 3 change(s) applied
→ 窓の外の回から数えた cooling-off が過ぎているので、窓が開いた回にそのまま開く
```

起こし直しても `until` は同じで、開いたのは宣言を初めて見てから 41 秒（起こし直してから 25 秒）。起こし直した時刻から数え直して
いない（延びない）し、止めていた間を飛ばしてもいない（縮まない）。最後の小節は直したあとの動き（「見つけたこと」の 1）。

## 見つけたこと

| # | 見つけたこと | 扱い |
|---|---|---|
| 1 | **alert-only の周・回（schedule の窓の外・`--mode alert-only`・`--freeze`）では、planned_change の宣言を「見た」ことにならず、cooling-off の時計が動かなかった**。宣言の記録（S4 の `planned_change.declared`）は予算ゲートの中で書いていたが、alert-only は予算ゲートの前で周を終える。そのため solo の cooling-off は窓が開いて auto になった周から数え始め、「保守窓 Sat 02:00-04:00 + cooling_off 2h」では窓が閉じる 04:00 まで開かない（窓の中で一度も通らない）。§14.5 の「宣言を**初めて見た時刻**から数え」と食い違う | 直した。intent を読んで宣言を渡したとき（`SetPlannedChanges`、毎周・1 回収束の最初）に宣言を S4 に記録して時計を動かす。alert-only の周でも、それで出た知らせ（取り下げ・S4 に書けない）を出す。単体テスト `TestPlannedGate_DeclaredWithoutCheckCycle`、受け入れテスト `[AC-102-1]`（1 回収束の `--mode alert-only`・常駐の `--freeze` で宣言を見てから cooling-off が過ぎれば、最初の auto の回でそのまま開く） |
| 2 | 常駐の起動行の `mode=auto` は `--mode` の値で、intent の schedule を含めた mode ではない（手順 2 では起動行が `mode=auto`、cycle 1 が `remediation mode=alert-only`）。また dnszone で回しても `state=sakutei-fake-state.json`（fake provider の既定）と出る | 直していない（表示の言葉の判断が要る）。各周の mode は `remediation mode=` の行が正しく出るので、動きには関わらない |
| 3 | `--mode=`（空の値）が黙って auto になっていた。`--mode=$MODE` で MODE が空のとき、修復を止めたつもりで開いてしまう | 直した。空の値は拒否する（`invalid --mode "": empty mode; use auto or alert-only`、終了コード 1）。単体テスト `TestParseMode` |
| 4 | `plan` では、今が窓の外か（次の `reconcile`・周が直すか）が分からない。`reconcile` を回すと `Remediation mode:` の行で分かる | 直していない（使いにくさ。`plan` に mode の行を足すかは人が決める） |
| 5 | 取り下げの知らせ `PlannedChangeWithdrawn` だけ、宣言を内部の鍵（`pool=web\|update\|<開始>\|<終了>`）で出す。ほかの知らせは `planned_change(scope=… kind=… window=…)` | 直していない（表示だけ） |
| 6 | §14.3 の「`ownership: platform` は schedule が auto でも alert-only」と、remediation の既定をプロファイルで切り替えることは、v1.5 の外（§14.5「v1.5 の実装」）なので試していない | 設計どおり（v1.5 の外） |
