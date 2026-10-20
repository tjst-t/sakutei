# Sakutei 設計書 v3.7 変更一覧（CHANGELOG）

対象: v3.6 → v3.7。改訂日 2026-10-20。骨格（§1〜§29 の機構）と設計判断（原則・機構）は不変。本改訂は **planned_change の承認情報連携と運用プロファイル（§13.3 / §14.4 / §14.5、Backlog #44）の実装**に合わせ、承認の検証の仕方・プロファイルの既定・cooling-off の時計の置き場所を書き足す小改訂である。

形式: **変更ID / 対応 / 章 / 変更内容 / 設計判断の理由**。対応の `#nn` は Backlog の項目番号。

---

## CHG-01 — planned_change の approval を requirements で検証する

- **対応**: #44
- **章**: §13.3「承認と運用プロファイル」
- **変更内容**: `approval.approvers` を、宣言の `requirements`（ゲートと同じ `quorum` / `roles` / `separation`）か、書かなければ運用プロファイルの既定で検証し、満たさなければレーンを開かない（`PlannedChangeUnapproved`）。申請者は `applicant` に書く。承認者は ID か `{ id, roles }`。満たさない宣言はロードを失敗させず、レーンを閉じるだけにする。
- **理由**: 承認を「記録するだけ」にしておくと、§13.2 の「大量破壊は意図でも人の承認を要する」が宣言 1 つで素通りになる。数え方をゲート（§14.4）と同じ関数にして、ゲートと planned_change で「何人の承認か」がずれないようにした。ロード失敗にしないのは、宣言 1 つの不備で全体の収束（自己修復）を止めないため（レーンが閉じるのは保守側・原則3）。

## CHG-02 — 運用プロファイルの書式と v1.5 で切り替えるもの

- **対応**: #44
- **章**: §14.5「v1.5 の実装」
- **変更内容**: トップレベルの `profile: solo | team | enterprise`（既定 team）と `solo: { cooling_off }`（既定 2h、solo のときだけ）。既定は planned_change の承認要件（solo = なし / team = 1 人 / enterprise = 2-of-2 + separation）と、enterprise のゲート既定（requirements の無いゲートに 2-of-2 + separation、applicant の無い Workflow はロード拒否）。宣言・ゲートの `requirements` / `cooling_off` が常に優先する。ownership の書込ポリシー・remediation の既定の切り替えは v1.5 の外。
- **理由**: enterprise の「書かなくても 4-eyes」を守るには既定の値が具体的でなければならない。役割名は現場ごとなので既定化せず、人数と separation だけを既定にした。enterprise で applicant の無い Workflow を拒否するのは、separation を黙って外すと 4-eyes が効いているように見えて効かないから。

## CHG-03 — cooling-off の時計を S4 に置く

- **対応**: #44
- **章**: §13.3・§14.5・§23.7・§26.6・§29.6
- **変更内容**: cooling-off は宣言を初めて見た時刻から数え、その時刻を S4 に `planned_change.declared` として残す。レーンが開いたら `planned_change.opened`、レーンで発射した件数を `planned_change.executed` として残す。常駐の再起動と次の `reconcile` は S4 を読み直す。one-shot `reconcile` は `--audit-log` を付けたときだけレーンを開き、枠を発射前にまとめて取って `executed` を S4 に書けてから発射する。cooling-off は S4 が無ければ評価できず、レーンは閉じたまま。`max_count` を広げた宣言は cooling-off をやり直す。
- **理由**: 非常駐（段0-zero）では「N 時間後に動くタイマー」が成立しない（§23.7）。宣言の時刻を S4 に置けば、常駐でも reconcile でも「評価した時点で N 経っていれば通す」という同じ式になる（タイマーからゲートへの level-triggered な再解釈）。宣言の時刻に git のコミット時刻を使わないのは、intent の取り出し方（ローカルディレクトリ・ミラー）によって取れないことがあり、また Sakutei が実際に見た時刻から数える方が保守側だから。`executed` の合計で reconcile が実行をまたいで `max_count` を守れるようになり、#39 の「one-shot はレーンを開かない」制約を S4 がある場合に限り外した。
