# CHANGELOG v3.54 → v3.55

**改訂日: 2026-10-28**

freeze を監査に残す改訂である（Backlog #184。親のストーリー「M7：本番に向けた固め」の C）。正しい状態は宣言に置き、ツールの
中だけに持たない（§1.7）の仕上げの 1 つで、設計の向きは変えていない。

## #184：freeze を監査に残す（誰が・なぜ・いつまで、§14.3）

- **背景**: global freeze（`--freeze`）は「誰かが自動修復を止めた」という運用の判断なのに、その事実は起動の旗とプロセスの中に
  しか無かった。誰が・なぜ止めたのか、いつまで止めるつもりなのかが残らず、解き忘れも見えない。また、動いている常駐を freeze
  するには常駐を止めて旗を付けて起こし直すしかなかった。
- **決定**:
  - 入口は 2 つ。起動の旗 `--freeze`（その実行・その常駐の間だけ効く）と、動いている常駐を止めずに freeze する書き込みの口
    `sakutei freeze start` / `end` / `status`（freeze の状態のファイル `--freeze-state` を書く。常駐は毎周、一回性の
    `reconcile` は毎回読む）。どちらも most-restrictive-wins の決定源の 1 つ。
  - 理由は必須。`--freeze` は `--freeze-reason` と `--audit-log` を要り、`sakutei freeze start` は `--reason` を要る。無ければ
    何も変えず、監査にも書かずに断る。誰が（`--freeze-by` / `--by`、既定は OS のユーザー名）・期限（`--freeze-until` / `--until`。
    RFC 3339 の時刻か今からの長さ。任意）を添える。効いている freeze があるところへの `start` は断る。
  - S4 監査の種別 `freeze.started`・`freeze.ended`・`freeze.expired`（Actor に誰が、Detail に入口・理由・期限）。状態のファイルを
    書く前に追記する。`--freeze` の終わりは実行・常駐が終わるときに書く。
  - 期限を過ぎた freeze は、次に読んだ者（常駐の周・一回性の `reconcile`・`sakutei freeze`）が解いて `freeze.expired` を書く。
  - freeze の状態のファイルが読めない周は freeze として扱う（§3）。
  - 効いている間は level の Event `Frozen`（attrs：`source`・`by`・`reason`・`since`・`until`）を出し、解けた周に resolved。
    運用者が選んだ止めなので stuck に上げない。status は常駐の健全性（`--daemon-status`）の `freeze` と、`--freeze-state` の
    ファイルの `freeze`（`--json` でも）で出す。
- **互換**: `--freeze` を `--freeze-reason`・`--audit-log` なしで使っていた起動は断られる（理由と記録の無い freeze を作らない）。
- **見送り**:
  - HTTP の書き込みの口（承認の口と同じく認可を持たないので、いまはファイルと CLI。誰が書けるかはファイルの権限で絞る）。
  - scope ごとの freeze（`freeze_scope`、§14.7 の Workflow の段）。
  - 監査の記録の改ざんしにくい形・外へすぐ送ること（M7 の次の項目）。
