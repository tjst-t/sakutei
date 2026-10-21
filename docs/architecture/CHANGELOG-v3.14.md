# Sakutei 設計書 v3.14 変更一覧（CHANGELOG）

対象: v3.13 → v3.14。改訂日 2026-10-21。骨格（§1〜§29 の機構）と設計判断（原則・機構）は不変。本改訂は **level 報告（§22.3）の v1 の決まりと既定値**を、実装（Backlog #79）に合わせて書き足すだけの小改訂である。

形式: **変更ID / 対応 / 章 / 変更内容 / 設計判断の理由**。対応の `#nn` は Backlog の項目番号。

---

## CHG-01 — level 報告の対象・キー・resolved の条件

- **対応**: #79
- **章**: §22.3
- **変更内容**: drift・Apply の失敗・halt（予算・容量・prune・inventory・外部入力不達）・ghost diff の hold を level の Event とし、kind + resource（または scope・provider）をキーに、初めての周に firing、続く間は内容が変わった周だけ更新、消えた周に resolved を 1 回出す。Event に `state`・`since`・`count`・`class` を持たせる。resolved は「その周に対象を確かめられて消えていた」ときだけ出し、確かめられなかった周（観測していない・周を飛ばした等）は firing のまま。状態は S3 で、起こし直すと改めて firing になる。
- **理由**: v1 の試用（docs/trial/README.md の D2・D3）で、alert-only で手でずらしたままにすると同じ知らせが毎周出続け、予算・prune の halt も毎周出た。§22.3 が避けると決めていた「毎周同じアラートを撒く破綻」そのもの。観測していない周に resolved を出すと、in-flight や tier の間引きのたびに resolved → firing を繰り返すので、消えたと言えるのは確かめた周だけにした（原則 3 の「確信が持てないとき、減算しない」を報告にも当てる）。

## CHG-02 — transient → stuck に上げる既定は連続 5 周

- **対応**: #79
- **章**: §22.3
- **変更内容**: 連続 5 周続いた level の Event を stuck（`severity: critical`）に上げる。`--stuck-after <n>` で変えられる。provider が `PermanentError` を返した Apply（権限・欠損など）と容量の halt は初めから stuck。
- **理由**: 既定の周期 10 秒で約 50 秒。in-flight の Apply・backoff・一時的な外部断で自然に消える程度の長さを越えたら、人が見るべきものとして上げる。周数で数えるのは、level-triggered のループが毎周持っている情報（§22.3）でそのまま決まり、周期を変えても意味がそろうため。

---

## 見送り

- **oscillating の点灯**: 振動の検知は v1 の既知の限界（§29.2）のまま。level の Event を数えても綱引きは見分けられない（同じキーが firing のまま続くだけ）。
- **blocked の明示**: 下流は自分の Event を出さず、上流の Event がそれを代表する。下流の一覧を上流の Event に載せるのは、運用の要求が出てから。
- **一回性の `reconcile`・`plan` の level 化**: 周をまたがないので対象外。
