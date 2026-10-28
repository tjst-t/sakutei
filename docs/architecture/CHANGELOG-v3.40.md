# CHANGELOG v3.39 → v3.40

**改訂日: 2026-10-28**

管理 API に更新の口が無い装置（GUI だけで FW を上げるスイッチ・PDU など）の版を宣言で持てるようにした改訂である
（Backlog #150。親のストーリー「FW の更新と入れ直しを宣言で扱う」の 2：観測と人の手順の組み合わせ）。多相 Task（§6.2、#51）の
承認の要る相・待つ相（#148）と承認ゲート（§6.4）の上に乗せたもので、設計の向きは変えていない。

## #150：人の手順で合わせる Task（§6.2・§6.5）

- **背景**: Redfish の UpdateService のように管理 API で更新できる部品は多相 Task で流せる（#148）。しかし更新の口が管理 API に
  無い装置は、版を観測できても Sakutei からは書けない。試験用の模擬にだけある更新の口に頼る作りは本物の装置で使えない。
- **決定**（§6.2「人の手順で合わせる Task」）:
  - `update_policy.strategy: manual` を provider に依らずどのリソースにも書ける。`procedure`（手順の説明）か `procedure_url`
    （手順書）の少なくとも一方が要る。`confirm_timeout`（既定 1h）・`window` を書ける。`rollback_on_fail` は書けない。
    `procedure`・`procedure_url`・`confirm_timeout` を manual 以外に書けば読み込みで断る。
  - 収束ループは Apply しない（§6.5。`strategy: task` と同じく Task を起こすだけ）。1-outstanding・窓・予算はそのまま効く。
    provider は観測できればよく、Task の実行体を持たなくてよい。Task は provider の実行体を一度も呼ばず、装置に何も書かない。
  - Task は相が 1 つ（`manual`）の多相 Task で、承認の要る相と待つ相を兼ねる。入口で観測して違えば承認ゲートを開き、手順の
    文・URL・いまの版と望む版を S2 の記録（`notice`、`sakutei task list`）と RequiresTask の Event（Attrs の `procedure`・
    `procedure_url`・`current`・`desired`・`state`）に出す。
  - 承認（`sakutei task approve`）は「人が作業した、観測で確かめ始めてよい」という合図。手順は何もせず、観測で宣言どおりに
    なるのを承認の時刻から `confirm_timeout` まで待ち、なれば閉じる。ならなければ **stuck**（halt、Event は critical）で、
    勝手に閉じない。承認待ちの間も stuck の後も、観測で宣言どおりになれば閉じる（人の答えは要らない）。
  - 観測はそのリソースの provider の `Read` と収束ループと同じ Diff。見えないものは「済んでいない」と取らず待つ。
  - Event の `state` は `launched` → `awaiting_approval` → `confirming` → `stuck` / `halted`（`outside_window`・`outstanding`）。
    level（§22.3）で、状態が変われば更新として出し、閉じれば resolved。現実が宣言から外れた（cause B）ときは DriftEvent も出る。
- **見送り**:
  - 人の作業を待つ間の上限（承認ゲートの上限）は `--task-gate-timeout`（既定 24h）を共有し、リソースごとには書けない。
  - 手順を通知の口（§22.3）で特定の宛先（チケットなど）へ送り分けること。今は Event の Attrs に載せるだけ。
  - provider の計画の時点の確かめ（PlanRefiner）は update_policy を知らない。`redfish-firmware` に `strategy: manual` を
    書くと、計画の時点の確かめ（イメージの在りかの宣言・`Updateable` が偽の部品を断る）は `strategy: task` のときのまま効く
    （Task は Redfish に何も送らない）。更新の口の無い部品を manual で扱うには、確かめを policy に合わせる口が要る。
  - 部品の版の互換の表・全体を止める型の更新（親のストーリーの 3・4）。

## 受け入れテスト

`tests/acceptance/manual_update_test.go`（常駐ループ、fake provider の `size` を装置の版とみなす）:

- `[AC-150-1]` 版が違う → 知らせ（手順の文・URL・いまの版と望む版を承認ゲートと Event に出す）と承認待ち → 承認 → 観測の版を
  変える → 閉じる。Apply も provider の実行体も呼ばない。閉じたあとで版が戻れば DriftEvent と新しい承認待ち。
- `[AC-150-2]` 承認しても `confirm_timeout` までに版が変わらなければ閉じずに stuck（critical）。後で観測の版が合えば閉じる。
- `[AC-150-3]` 承認しなくても観測で版が合えば閉じる（監査に承認は残らない）。
- `[AC-150-4]` 宣言の誤り（手順の無い manual・manual 以外の procedure / confirm_timeout・manual の rollback_on_fail・読めない
  confirm_timeout）は読み込みで断る。

`internal/task/manual_test.go`:

- `[AC-150-5]` manual の Task は相を 1 つ（承認・待つ・危険区間でない）宣言し、provider の実行体を呼ばない。manual でない Task は
  provider の実行体に回す。
