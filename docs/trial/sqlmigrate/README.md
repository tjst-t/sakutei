# sqlmigrate で単純 Task を実物で試す

v1.5 の単純 Task（設計 §6.2、`update_policy.strategy: task`）を、実 provider の sqlmigrate（SQLite のマイグレーション、§29.1、
[docs/providers/sqlmigrate.md](../../providers/sqlmigrate.md)）を相手に、このコンテナの本物の `sqlite3`（3.45.1）と常駐の sakutei
（`--task-store`）で回して確かめる手順と、2026-10-23 にこのコンテナで回した結果（Backlog #95）。外のサービスには繋がない。

## まず知っておくこと

- sqlmigrate は **`strategy: task` 専用**。収束ループは差分を見つけても Apply せず、常駐の sakutei が Task を起動する
  （`--task-store` が要る。1 回収束の `reconcile` は Task を起動しない）。Task は今の版の次から宣言の版まで、1 本ずつ
  トランザクションで流し、同じトランザクションで `schema_migrations` に版を書く（完了印）。
- **完了は現実に問う**。Task が成功を返しても記録は消さず、次の周の Read（`schema_migrations` の最大の版）で差分が無くなったときに消す。
- **非冪等な操作を自分で流し直さない**。失敗（halt）・途中で落ちた（中断）記録が残っている間は、差分が残っていても起動しない。
  人が現実を確かめて `sakutei task cancel` で記録を消すと、差分が残っていれば窓の中で新しい Task を起動する。
- 0002 は**わざと遅い**（数を数える計算を入れて 5 秒ほどかかる）。デーモンは `--interval 1s` で回すので、0002 を流している間に
  周を何周か跨ぐ。手順 1（二重に起動しない）と手順 5（実行中に殺す）はこれを使う。
- 窓は本物の時計で決まる（時計を差し替える口は CLI に無い）。手順 2 は、今の時刻を含まない窓と含む窓を `run.sh` が今の時刻から作る。

## 確かめる観点

| # | 観点 | 設計 | 手順 |
|---|---|---|---|
| 1 | 版 0 → 3 を宣言すると Task が 1 回だけ起動し（1-outstanding。実行中に周を跨いでも二重に起動しない）、完了印を観測して記録が片づき、収束する | §6.2 | 1 |
| 2 | メンテ窓（`window`）の外では起動しない。窓に入ると起動する | §6.2 | 2 |
| 3 | 壊れたマイグレーションで失敗 → halt（再起動しない、知らせが出る）。`rollback_on_fail` なら起動したときの版に戻る | §6.2・§29.1 | 3a・3b |
| 4 | `sakutei task cancel` のあと、差分が残っていれば改めて起動する（マイグレーションを直してから） | §6.2 | 4 |
| 5 | 実行中にデーモンを SIGKILL すると、起こし直したデーモンは自動で再実行しない（中断扱い、人が cancel するまで）。DB の中身 | §6.2 | 5a・5b |
| 6 | 版を下げる宣言（3 → 1）で down が逆順に流れる | §29.1 | 6 |

## 置いてあるもの（docs/trial/sqlmigrate/）

| ファイル | 中身 |
|---|---|
| `databases.yaml` | `--sqlmigrate-config` に渡す設定。DB `app`（`data/app.db`）とマイグレーションの置き場所（`migrations/app`）、`busy_timeout: 5s` |
| `migrations/app/` | 0001 `users` の表・0002 `email` の列と `user_stats`（**遅い**）・0003 `orders` の表。それぞれ up と down |
| `broken/0003_create_orders.up.sql` | 壊れた 0003。1 文目の `CREATE TABLE orders` は通り、2 文目で表の名前を書き間違えていて失敗する |
| `run.sh` | 手順 1〜6 を通して流すスクリプト。intent は手順ごとに書く |

## 通しで流す

```sh
docs/trial/sqlmigrate/run.sh            # 作業ディレクトリは /tmp/sakutei-sqlmigrate（毎回作り直す）
```

`sqlite3` と Go が要る。リポジトリのファイルは書き換えない。通しで 1 分半ほど。手順 6 は手順 4 のデーモンをそのまま使うので、
手順 4 の次に流す（5a・5b はその後）。各手順の作業ディレクトリに `daemon.log`・`tasks.json`・`data/app.db` が残る。

## 1 つずつ流す

```sh
W=/tmp/sakutei-sqlmigrate; rm -rf $W; mkdir -p $W/intent $W/data
cp -r docs/trial/sqlmigrate/{databases.yaml,migrations} $W/
go build -o $W/sakutei ./cmd/sakutei; cd $W
cat > intent/main.yaml <<'EOF'
version: "1"
resources:
  - provider: sqlmigrate
    id: app
    update_policy:
      strategy: task
      # window: "02:00-04:00 UTC"   # 手順 2
      # rollback_on_fail: true      # 手順 3b
    fields:
      version: "3"
EOF
./sakutei plan --intent intent --sqlmigrate-config databases.yaml
./sakutei      --intent intent --sqlmigrate-config databases.yaml --task-store tasks.json --interval 1s
# 別の端末から
./sakutei task list --store tasks.json
sqlite3 data/app.db 'select * from schema_migrations'
```

| 手順 | すること | 確かめること |
|---|---|---|
| 1 | 上の intent（版 3）でデーモンを起動 | Task の起動は 1 回。0002 を流している間の周は `awaiting their Task`。版 3 になった次の周に `completed (completion mark observed…)` と出て、`task list` が `no outstanding task` |
| 2 | `window` に今を含まない窓を書いて起動 → 今を含む窓に書き換える（デーモンはそのまま） | 窓の外では `no Task launched (outside-window)` で DB を作りもしない。書き換えた周に起動する |
| 3a | `cp docs/trial/sqlmigrate/broken/0003_create_orders.up.sql migrations/app/` してから版 3 で起動 | 0003 で失敗して `failed`。0001・0002 は残る（版 2）。以後の周は起動しない |
| 3b | 版 1 に収束させてから、`rollback_on_fail: true` で版 3 を宣言（0003 は壊れたまま） | 失敗のあと、起動したときの版 1 まで down で戻る（`failed (rolled back)`） |
| 4 | 3a のあと、0003 を直す → `./sakutei task cancel --store tasks.json sqlmigrate//app` | 直しただけでは起動しない。cancel の次の周に新しい Task を起動して版 3 に収束 |
| 5 | 版 3 で起動し、0002 を流している最中にデーモンを `kill -9` → 起こし直す | 起こし直したデーモンは起動しない（`still recorded as running … interrupted if that daemon is gone`）。cancel すると起動する |
| 6 | 版 3 から版 1 に書き換える | 0003・0002 の down が流れて版 1 |

## 回した結果（2026-10-23、このコンテナ）

`run.sh` の出力の抜粋。Task の ID（`<ホスト名>-<pid>-<時刻>-<連番>`）は `…-<時刻>-<連番>` に縮めてある。

### 1. 版 0 → 3：Task は 1 回だけ起動し、完了印を観測して片づく

`plan` は Task を起動する差分だと示す（DB には触らない）。

```
~ UPDATE sqlmigrate//app
  reason: resource sqlmigrate//app has 1 field(s) out of sync
  cause:  B
  ~ version               "0" → "3"
  task:   launches a Task (update_policy.strategy: task; window: any time) — run by the daemon, not Applied
```

0002 を流している最中（DB は版 1）の `task list`：

```
RESOURCE                                 PHASE      TASK                           STARTED              ERROR
sqlmigrate//app                          running    banto-…-3166884-1792773812977545000-1 2026-10-23T16:43:32Z
```

デーモンの出力：

```
sakutei: DriftEvent: sqlmigrate//app [cause: B] no previous snapshot (first cycle or post-restart)
sakutei: RequiresTaskEvent: sqlmigrate//app: task …-1792773812977545000-1 launched (strategy: task; not applying)
sakutei: cycle 1: 1 task(s) launched (strategy: task): [app]
sakutei: DriftEvent: sqlmigrate//app [cause: B] drift B: pending was "3" prev cycle now "2"; version was "0" prev cycle now "1" (…) [firing transient count=2 since=2026-10-23T16:43:32Z]
sakutei: cycle 2: 1 strategy: task resource(s) awaiting their Task (not applied): [app]
sakutei: cycle 3: 1 strategy: task resource(s) awaiting their Task (not applied): [app]
sakutei: cycle 4: 1 strategy: task resource(s) awaiting their Task (not applied): [app]
sakutei: DriftEvent: sqlmigrate//app [cause: B] prev snapshot at 2026-10-23T16:43:36Z matches current (no field change in ring) [firing stuck count=5 since=2026-10-23T16:43:32Z]
sakutei: cycle 5: 1 strategy: task resource(s) awaiting their Task (not applied): [app]
sakutei: task …-1792773812977545000-1 for sqlmigrate//app completed (completion mark observed; was succeeded)
sakutei: cycle 6: observed=1 converged (no changes)
sakutei: cycle 6: RESOLVED Drift sqlmigrate//app: resolved after 5 cycle(s) (firing since 2026-10-23T16:43:32Z)
sakutei: cycle 7: observed=1 converged (no changes)
```

- **Task の起動は 1 回**。Task の実行中に 4 周回ったが（`awaiting their Task`）、二重に起動しなかった。
- 版 3 を観測した周に記録が消え（`task list` は `no outstanding task`）、以後 `converged (no changes)`。

```
$ sqlite3 data/app.db 'select * from schema_migrations'
1|2026-10-23T16:43:32Z
2|2026-10-23T16:43:37Z
3|2026-10-23T16:43:37Z
（表: orders schema_migrations user_stats users、users の列: id name email）
```

### 2. メンテ窓

`window: "18:43-19:43 UTC"`（今 16:43 UTC を含まない）。`plan` の差分にも窓が出る（`task: launches a Task (…; window: 18:43-19:43 UTC)`）。

```
sakutei: RequiresTaskEvent: sqlmigrate//app: no Task launched (outside-window): outside maintenance window 18:43-19:43 UTC (now Fri 16:43 UTC)
sakutei: cycle 1: 1 strategy: task resource(s) awaiting their Task (not applied): [app]
…（cycle 5 まで毎周同じ）
```

- 5 周、起動しない。DB のファイルも作らない（版 none）。`task list` は `no outstanding task`（窓の外は記録を作らない）。

デーモンを止めずに `window: "16:13-17:13 UTC"`（今を含む）に書き換えると、次の周に起動して版 1 に収束した。

```
sakutei: RequiresTaskEvent: sqlmigrate//app: task …-1792773826803942446-1 launched (strategy: task; not applying)
sakutei: cycle 6: 1 task(s) launched (strategy: task): [app]
sakutei: task …-1792773826803942446-1 for sqlmigrate//app completed (completion mark observed; was succeeded)
sakutei: cycle 7: observed=1 converged (no changes)
```

### 3a. 壊れた 0003 で失敗 → halt（rollback_on_fail なし）

```
sakutei: RequiresTaskEvent: sqlmigrate//app: task …-1792773827920271571-1 launched (strategy: task; not applying)
…（0001・0002 を流す間 cycle 2〜5 は awaiting）
sakutei: RequiresTaskEvent: sqlmigrate//app: no Task launched (outstanding): task …-1792773827920271571-1 failed: sqlmigrate "app": 0003_create_orders.up.sql failed (rolled back; the database stays at version 2): sqlite3: exit status 1: Parse error near line 4: no such table: user_list; halted until cancelled
sakutei: cycle 6: 1 strategy: task resource(s) awaiting their Task (not applied): [app]
…（cycle 10 まで毎周同じ）
```

```
$ sakutei task list --store tasks.json
RESOURCE                                 PHASE      TASK                           STARTED              ERROR
sqlmigrate//app                          failed     banto-…-3167804-1792773827920271571-1 2026-10-23T16:43:47Z sqlmigrate "app": 0003_create_orders.up.sql failed (rolled back; the database stays at version 2): sqlite3: exit status 1: Parse error near line 4: no such table: user_list
$ sqlite3 data/app.db 'select * from schema_migrations'
1|2026-10-23T16:43:47Z
2|2026-10-23T16:43:52Z
（表: schema_migrations user_stats users）
```

- Task の起動は 1 回。失敗のあと 5 周、起動し直さない。0003 の 1 文目（`CREATE TABLE orders`）はトランザクションごと巻き戻り、`orders` は残らない。
- 知らせ：`RequiresTaskEvent … failed … halted until cancelled`（warning）が毎周出る。level の知らせ（§22.3）としては同じリソースの
  `DriftEvent` が `firing`（5 周で `stuck`・critical）で出続け、収束すると `RESOLVED` が 1 回出る（手順 4）。
- `near line 4` は壊れた 0003 のファイルの 4 行目（直す前は `near line 7` と出ていた。下の「見つけたこと」2）。

### 3b. rollback_on_fail: true

版 1 に収束させてから、`rollback_on_fail: true` で版 3 を宣言した（0003 は壊れたまま）。

```
sakutei: RequiresTaskEvent: sqlmigrate//app: task …-1792773839518616982-2 launched (strategy: task; not applying)
…（0002・0003 と rollback の間 cycle 4〜7 は awaiting）
sakutei: RequiresTaskEvent: sqlmigrate//app: no Task launched (outstanding): task …-1792773839518616982-2 failed (rolled back): sqlmigrate "app": 0003_create_orders.up.sql failed (rolled back; the database stays at version 2): … no such table: user_list; halted until cancelled
```

```
$ sqlite3 data/app.db 'select * from schema_migrations'
1|2026-10-23T16:43:57Z
（表: schema_migrations users、users の列: id name）
```

- 0002 は通ったあと down で戻り（`email` の列と `user_stats` が消えた）、**起動したときの版 1** に戻った。以後は起動しない。

### 4. 直して cancel → 改めて起動

3a のデーモンのまま、0003 を直した。

- 直しただけでは起動しない（2 周見て、版 2・起動 1 回のまま）。

```
$ sakutei task cancel --store tasks.json sqlmigrate//app
cancelled task …-1792773827920271571-1 for sqlmigrate//app (was failed)
```

```
sakutei: RequiresTaskEvent: sqlmigrate//app: task …-1792773849130461546-2 launched (strategy: task; not applying)
sakutei: cycle 21: 1 task(s) launched (strategy: task): [app]
sakutei: task …-1792773849130461546-2 for sqlmigrate//app completed (completion mark observed; was succeeded)
sakutei: cycle 22: observed=1 converged (no changes)
sakutei: cycle 22: RESOLVED Drift sqlmigrate//app: resolved after 21 cycle(s) (firing since 2026-10-23T16:43:47Z)
```

```
$ sqlite3 data/app.db 'select * from schema_migrations'
1|2026-10-23T16:43:47Z
2|2026-10-23T16:43:52Z
3|2026-10-23T16:44:09Z
```

- cancel の次の周に、0003 だけを流す新しい Task を起動した（0001・0002 は流し直さない）。

### 5. 実行中にデーモンを SIGKILL

版 3 を宣言し、0002 を流している最中（DB は版 1）にデーモンを `kill -9` した。殺し方を 2 通り試した。

| | 5a. デーモンだけを殺す | 5b. プロセスグループごと殺す（子の sqlite3 も。電源断に近い） |
|---|---|---|
| 殺した直後の sqlite3 のプロセス | **残っている**（親が死んでも 0002 を流し続ける） | 無い |
| 殺した直後の版 | 1 | 1 |
| 6 秒後の版 | **2**（残った sqlite3 が 0002 を COMMIT した） | 1（0002 のトランザクションは巻き戻った） |
| `pragma integrity_check` | ok | ok |
| `task list` | `running`（殺したデーモンの記録のまま） | `running` |
| 起こし直したデーモン | 6 周、起動しない | 6 周、起動しない |
| cancel のあと | 0003 だけを流して版 3 | 0002・0003 を流して版 3 |

5a の DB と、起こし直したデーモンの出力：

```
$ sqlite3 data/app.db 'select * from schema_migrations'
1|2026-10-23T16:44:14Z
2|2026-10-23T16:44:19Z
（表: schema_migrations user_stats users、users の列: id name email）
```

```
sakutei: DriftEvent: sqlmigrate//app [cause: B] no previous snapshot (first cycle or post-restart)
sakutei: RequiresTaskEvent: sqlmigrate//app: no Task launched (outstanding): task …-1792773854490935626-1 was launched by "banto-…-3175290" at 2026-10-23T16:44:14Z and is still recorded as running — running there, or interrupted if that daemon is gone; not re-running — check reality and cancel it if needed
sakutei: cycle 1: 1 strategy: task resource(s) awaiting their Task (not applied): [app]
…（cycle 6 まで毎周同じ）
```

- どちらも、起こし直したデーモンは自動で再実行しない（中断扱い）。人が `task cancel` すると、そのときの版から宣言の版まで流す。
- 1 本のマイグレーションは 1 つのトランザクションなので、どこで殺しても DB は「ある版まで通った」状態で、途中の 1 本の半端は残らない。
  ただし 5a のように、**殺した後にも版が進むことがある**（sqlite3 は別のプロセスで、SQL は標準入力で渡し済み）。cancel する前に
  `schema_migrations` を見て、残った sqlite3 が無いこと（`pgrep sqlite3`）を確かめる。

### 6. 版を下げる（3 → 1）

手順 4 のデーモンのまま、intent の版を 1 に書き換えた。

```
~ UPDATE sqlmigrate//app
  ~ version               "3" → "1"
  task:   launches a Task (update_policy.strategy: task; window: any time) — run by the daemon, not Applied
```

```
sakutei: RequiresTaskEvent: sqlmigrate//app: task …-1792773852284115092-3 launched (strategy: task; not applying)
sakutei: cycle 24: 1 task(s) launched (strategy: task): [app]
sakutei: task …-1792773852284115092-3 for sqlmigrate//app completed (completion mark observed; was succeeded)
sakutei: cycle 25: observed=1 converged (no changes)
```

```
$ sqlite3 data/app.db 'select * from schema_migrations'
1|2026-10-23T16:43:47Z
（表: schema_migrations users、users の列: id name）
```

- 0003 の down（`orders` を消す）、0002 の down（`user_stats` と `email` の列を消す）の順に流れて版 1。

## 見つけたこと

| # | 見つけたこと | 扱い |
|---|---|---|
| 1 | デーモンを起こし直すと、前のデーモンが running のまま残した記録について `task … is running on "<ホスト名>-<旧 pid>"` とだけ出た。既定の holder は `<ホスト名>-<pid>` で、起こし直すと別の holder になるので、「別のデーモンで実行中」と読めて、中断（人が cancel する）だと分からない | 直した。holder が違う running の記録は、起動した holder と時刻を示し、「そこで実行中か、そのデーモンがもう無ければ中断」と「確かめて cancel」の案内を出す（手順 5 の出力）。単体テスト `TestTrigger_RunningRecordOfAnotherHolder`。同じ holder（`--shard-id` を固定したとき）なら前からの `was interrupted (daemon restarted while running)` |
| 2 | 失敗したマイグレーションのエラーの `near line N` が、ファイルの行より 3 行（busy_timeout が無ければ 2 行）大きかった（壊れた 0003 の 4 行目が `near line 7`）。実行体が包む `BEGIN`・`schema_migrations` を作る文・`.timeout` の行のぶん | 直した。`.timeout` は `-cmd` で渡し、`BEGIN …;` とファイルの 1 行目を同じ行に置く。`near line N` がファイルの行になる。単体テスト `TestRunTask_ErrorLineIsTheFileLine` |
| 3 | `RequiresTaskEvent … no Task launched (outside-window／outstanding)` が毎周出る（窓の外で待つ間・halt の間ずっと） | 直していない（設計どおり）。§22.3 は `RequiresTask` を edge の Event に数えていて、level の報告（firing／resolved）にするには設計の変更が要る。その間の level の知らせは同じリソースの `DriftEvent`（firing → 5 周で stuck → 収束で RESOLVED）が担っている。人に聞く |
| 4 | Task が実行中なだけでも、同じリソースの `DriftEvent` が 5 周（`--stuck-after`）で `firing stuck`（critical）に上がる（手順 1：0002 が 5 秒かかるだけで critical）。周期 30s なら 2 分半、長いマイグレーションでは毎回 critical になる | 直していない（設計の判断が要る）。§22.3 の stuck の判定に「自分が起動した Task の実行中（`awaiting their Task`）は数えない／別の class にする」を入れるかは人が決める |
| 5 | 5a：デーモンを SIGKILL しても、子の `sqlite3` は流し続けて 0002 を COMMIT し、版が殺した後に 1 → 2 に進んだ | 直していない（安全側：1 本はトランザクションなので半端は残らず、記録は running のままで再実行しない）。cancel の前に `pgrep sqlite3` で確かめることを手順に書いた。Linux の `Pdeathsig` で子も殺す案はあるが、どちらでも動きは「ある版まで通った」で同じ |
| 6 | 3b の記録のエラーは `… failed (rolled back; the database stays at version 2)` で、rollback の後の版（1）が書かれない。`(rolled back; …)` は失敗した 1 本のトランザクションのこと。`task list` の PHASE も `failed` だけで、rollback したかはデーモンの出力（`failed (rolled back)`）でしか分からない | 直していない（表示だけ）。`task list` に rollback の有無を出すかは人が決める |
| 7 | 周の要約 `cycle N: observed=1 applied=0 launched=0` の `launched` は非同期の Apply の数で、Task を起動した周も 0（次の行の `1 task(s) launched` が Task）。起動行は sqlmigrate だけでも `state=sakutei-fake-state.json` と出る（remediation-profile の見つけたこと 2 と同じ） | 直していない（表示の言葉の判断が要る） |
