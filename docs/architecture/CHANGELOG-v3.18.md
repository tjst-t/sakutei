# Sakutei 設計書 v3.18 変更一覧（CHANGELOG）

対象: v3.17 → v3.18。改訂日 2026-10-24。骨格と設計判断は不変。本改訂は sqlmigrate の試用（Backlog #92）で見つかった **strategy: task の level 報告の穴**を直すだけの小改訂である。

## CHG-01 — Task 実行中の Drift・RequiresTask を level 化

- **対応**: #112（2026-10-23 の sqlmigrate 試用で見つけ、2026-10-24 人が「推奨で直して」と決定）
- **章**: §22.3（§6.5）
- **見つかった穴**（docs/trial/sqlmigrate/README.md の見つけたこと 3・4）：
  1. 自分が起動した Task が走っている間も、同じリソースの `DriftEvent` が毎周数えられ、`--stuck-after`（既定 5 周）で `firing stuck`（severity critical）に上がる。周期が短い・マイグレーションが長いと、Task が走っているだけで毎周 critical の知らせが飛ぶ。
  2. `RequiresTaskEvent`（§6.5、起動しない理由：窓の外・outstanding など）が edge のまま毎周出て、窓の外で待つ間・halt の間ずっと撒かれる。
- **変更内容**：
  - Task の実行中（`task.Running`、自分がこのデーモンで起動した Task が走っている周）は、その周の `DriftEvent` を出さない。続いた周数を増やさず firing のままにする（まだ差分は残っているので resolved にもしない）。Task が長くかかっても、走っている間は stuck に上がらない。
  - `EventRequiresTask` を level の kind に加える。起動しない理由が続く間は firing のまま、理由が変わった周だけ更新を出し、差分が消えた周に resolved を出す。起動した知らせ（`task launched`）も同じ level 系列に入る。message は毎周変わりうるので fingerprint の比較には使わない（`Drift` と同じ扱い）。
  - Task の実行時間そのものへの別の timeout の知らせは、v1 の範囲では持たない。
- **理由**：§22.3 の level 報告は「毎周同じアラートを撒く破綻を回避する」ためのもの。Task の実行という正常な待ち時間を drift の継続と同じに数えると、その目的に反する。RequiresTask の「起動しない理由」も継続状態であり、edge（起きた周に1回）で扱うと level の対象から漏れて毎周スパムになる。

## 見送り

- Task の実行時間そのものに対する別の timeout の知らせ（長すぎる Task を知らせる仕組み）。v1 では持たない。
- transient/stuck 以外の新しい class（`in-progress` など）を Task 実行中に導入すること。既存の「カウントを増やさない」だけで足りると判断した。
