# Sakutei 設計書 v3.17 変更一覧（CHANGELOG）

対象: v3.16 → v3.17。改訂日 2026-10-23。骨格と設計判断は不変。本改訂は **単純 Task（§6.2）を動かす最初の本物の provider（sqlmigrate）** の決まりを §29.1 に書き足すだけの小改訂である。

## CHG-01 — 単純 Task の実 provider：sqlmigrate（SQLite のマイグレーション）

- **対応**: #92（2026-10-23、人が「本物の provider を作って Task を試験まで」と依頼し、AI が対象を選んだ）
- **章**: §29.1（§6.2・§28）
- **変更内容**: SQLite のデータベースに版つきのマイグレーションを流す provider。`version` を宣言し、`schema_migrations` の最大の版を観測する。`strategy: task` 専用で、差分があれば Task が up（または down）の SQL を 1 本ずつトランザクションで流し、同じトランザクションで完了印を書く。失敗は halt、`rollback_on_fail` で起動時の版まで戻す。`sqlite3` の CLI を exec で呼ぶ（Go の標準ライブラリだけの決まりのため）。
- **理由**: v1.5 の単純 Task は fake provider でしか動かせず、実物で確かめられなかった。§6.2 が単純型の典型として挙げる migration（完了印を現実側に置き、Observe で問う）は、Task の 1-outstanding・メンテ窓・失敗時の halt と rollback・中断時に再実行しないこと、を素直に試せる。SQLite はファイル 1 つで立てられ、試験の環境を選ばない。

## 見送り

- PostgreSQL などサーバ型の DB（同じ形で足せる。要る人が出てから）。
- 多相 Task（ファームウェア更新など、リブート跨ぎ）は v2（#51・#52）。
