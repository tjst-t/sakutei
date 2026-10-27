# CHANGELOG v3.21 → v3.22

**改訂日: 2026-10-27**

provider を 3 つ足し、Workflow の手順が複数の provider を扱えるようにした改訂である（Backlog ストーリー #124、子 #125〜#127、と #87）。設計の向きは変えず、§29.1 に provider の範囲と決まりを、§6.3 に Workflow の書式を書き足した。

## #125：PostgreSQL provider `postgresql`

ロール・データベース・データベースの権限（CONNECT・CREATE・TEMPORARY）・拡張を宣言で収束させる。`psql` を exec で呼ぶ（Go の依存を増やさない）。識別子と文字列は自前で quote する。パスワードは SecretRef で、送る前に SCRAM-SHA-256 の検証値にする。手で変えられたパスワードは、`verify_passwords` を明示したときだけ drift として見る。システムのロールと template0・template1・postgres は List に返さない（管理境界）。利用者の表があるデータベース・他のオブジェクトを持つロールは消さない。スキーマ・表の権限と default privileges は範囲の外。

## #126：sqlmigrate の PostgreSQL 版

データベースごとに `engine`（`sqlite`＝既定／`postgresql`）を選ぶ。決まりは SQLite 版と同じで、流す間は `pg_advisory_lock` で排他する。トランザクションに入れられない文は失敗として知らせる。

## #127：Ansible provider `ansible`（§28.7 のロングテール層）

1 つのホストへの 1 つのモジュールの呼び出しを 1 リソースとし、`--check --diff` で観測（changed=false なら適合）、通常実行で適用する。最小の階級（`TrueObserve: false`）で List を持たない（prune しない）。破壊的かどうかは利用者の `destructive` の宣言に委ねる。観測の頻度の既定は毎周（`observe_tier` で間引ける）。

## #87：Workflow の手順ごとの対象と前提条件（§6.3）

`apply`・`wait`・`require` に `resource: <provider>/<id>`（`{target}` で今の対象に展開）を書くと、その手順は target と別の provider のリソースを触れる。`require` は観測で満たすまで待ち、timeout で halt する。手順が触るリソースは target と同じく収束ループが触らず、plan・status は「Workflow に任せる」と示す。

## 変更箇所

`docs/architecture/spec/part2-core-model.md`（§5.6・§6.3）、`part5-scale.md`、`part8-implementation.md`（§28.7・§29.1）、`README.md`（版）、`docs/providers/postgresql.md`・`ansible.md`（新規）・`sqlmigrate.md`・`redfish.md`。
