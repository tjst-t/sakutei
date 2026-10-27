# マイグレーションの provider（sqlmigrate：SQLite・PostgreSQL）

SQLite のデータベース（ファイル）か PostgreSQL のデータベースに版つきのマイグレーションを流す provider。v1.5 の単純 Task（§6.2、`update_policy.strategy: task`）を実物で動かすためのもの。決まりの正は設計書 §29.1「単純 Task の実 provider：sqlmigrate」（v3.17）と「sqlmigrate の PostgreSQL 版」（Backlog #126）。ここは使い方の説明で、矛盾したら spec/ が優先する。PostgreSQL に固有のことは下の「[PostgreSQL（engine: postgresql）](#postgresqlengine-postgresql)」。

| provider 名 | ID | 扱うもの |
| --- | --- | --- |
| `sqlmigrate` | データベースの名前（例 `app`） | `schema_migrations` の版（マイグレーションをどこまで流したか） |

- 標準階級（List・BulkObserve を持たない）で、prune の対象にしない（データベースは Sakutei が作ったり消したりしない）。
- Go の標準ライブラリだけで書くため、ドライバは入れず `sqlite3` の CLI（PostgreSQL なら `psql`）を呼ぶ。SQL は標準入力で渡し、シェルは通さない。
- **`strategy: task` 専用**。収束ループの Apply は拒否し、`strategy: task` でない宣言は計画の時点でエラーになる。変更は常駐の sakutei が起動する Task だけが行う。

## データベースの一覧（--sqlmigrate-config）

DB のファイルとマイグレーションの置き場所は intent に書かず、`--sqlmigrate-config <file>` で渡す（`plan`・`reconcile`・`status`・常駐の `sakutei`）。書式は intent と同じ JSON / YAML サブセット。

```yaml
# databases.yaml
sqlite3: /usr/bin/sqlite3      # 省略可（既定は PATH の sqlite3）
busy_timeout: 5s               # 省略可。DB がロック中のときに待つ時間（既定 0 = 待たない）
databases:
  app:
    path: data/app.db          # DB のファイル
    migrations: migrations/app # マイグレーションのディレクトリ
  billing:
    path: /srv/billing/billing.db
    migrations: /srv/billing/migrations
```

- 相対パス（`path`・`migrations`、`/` を含む `sqlite3`）は設定ファイルのあるディレクトリから。`/` を含まない `sqlite3` は PATH から探す。
- 未知のキーはエラー（書き間違いを黙って無視しない）。
- intent に、設定に無い名前を書くと計画の時点でエラー（`database "…" is not configured`）。sqlmigrate のリソースがあるのに `--sqlmigrate-config` が無いのもエラー。
- DB のファイルが無いのは「まだ何も流していないデータベース」＝版 0 として扱う（最初の Task がファイルを作る）。ただし置き場所のディレクトリが無い・読めない、`sqlite3` が無い・起動できない、DB がロック中・壊れていて読めないときは Unknown（何もしない、unknown ≠ absent、§4.5）。

## マイグレーションのファイル名の決まり

マイグレーションのディレクトリに、版ごとに次の 2 つを置く。

```
migrations/app/
  0001_create_users.up.sql
  0001_create_users.down.sql
  0002_add_email.up.sql
  0002_add_email.down.sql
```

- 名前は `NNNN_<名前>.up.sql` と `NNNN_<名前>.down.sql`。`NNNN` は版（1 以上の整数。桁数は自由で、先頭の 0 は読み飛ばす）。
- 版は **1 から飛びなく** 並べる。同じ版を二重に置かない（名前が違っても同じ版ならエラー）。up の無い down はエラー。
- `.down.sql` は省略できる。ただし無い版より下へは戻せない（下げる Task も `rollback_on_fail` も失敗する）。
- `.sql` 以外のファイル（README など）とサブディレクトリは見ない。
- ファイルの中で `BEGIN` / `COMMIT` を書かない。1 本を 1 つのトランザクションで包み、同じトランザクションで `schema_migrations` に版を書く（完了印）のは実行体の仕事。
- `sqlite3` のドットコマンド（行頭の `.`、例 `.shell`）は書けない（流す前にエラー）。

完了印の表は `schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT)`。観測する版はこの表の最大の版（表が無ければ 0）。

## intent の例

dnszone・nftables・redfish など、ほかの provider のリソースと同じ intent に混ぜられる。

```yaml
resources:
  - provider: sqlmigrate
    id: app
    update_policy:
      strategy: task              # 必須（sqlmigrate は Task でしか変えない）
      window: "Sat 02:00-04:00"   # メンテ窓。窓の外では Task を起動しない（省略時は常時）
      rollback_on_fail: true      # 途中で失敗したら起動したときの版まで down で戻す
    fields:
      version: "2"                # 宣言する版（非負の整数。ディレクトリに無い版は計画の時点でエラー）

  # 版に依存するアプリは depends_on で待たせる（DB が宣言の版に収束するまで Apply しない）
  - provider: dnszone
    id: app.example.com./A
    depends_on: [app]
    fields:
      name: app.example.com.
      type: A
      value: 192.0.2.10
      ttl: "300"
```

- 宣言できるのは `version` だけ（`name` を書くなら ID と同じ値）。`pending`（今の版より上の up の本数）は観測だけの値。
- `version` は正規形の整数で書く（`"02"` はエラー。観測の `"2"` と文字列で食い違い、差分が消えないため）。
- 宣言の版が今の版より小さければ、down を逆順に流して下げる。
- `window` の書式は `"[<曜日>] HH:MM-HH:MM [<タイムゾーン>]"`（例 `"02:00-04:00"`・`"Mon-Fri 22:00-06:00"`・`"Sat,Sun 02:00-04:00 UTC"`）。

## 動き方

| コマンド | 何をするか |
| --- | --- |
| `sakutei plan` | 差分を出す。sqlmigrate の差分には `task: launches a Task (update_policy.strategy: task; …) — run by the daemon, not Applied` が付き、Task を起動する差分だと分かる。DB には触らない。 |
| `sakutei reconcile` | **sqlmigrate のリソースを Apply しない**（AC-v1.5-task-9）。`RequiresTaskEvent` を出し、そのリソースは収束していない扱い（depends_on で待つものも Apply しない）。ほかの provider のリソースは収束させる。 |
| `sakutei status` | 宣言の版と DB の版がずれていれば DRIFTED。 |
| `sakutei`（常駐、`--task-store <file>` 付き） | 差分を見つけ、窓の中なら Task を起動する（1 リソースに同時に 1 つ、§6.2）。Task は今の版の次から宣言の版まで 1 本ずつ流す。完了の判断は Task の記録ではなく、次の周の Read（`schema_migrations`）で行う。`--task-store` が無いと Task を起動せず、そのリソースは保留のまま。 |

失敗したとき（§6.2）:

- 途中の 1 本が失敗すると、その 1 本は巻き戻り（トランザクション）、それより前に通した版は残る。Task は失敗＝halt で、デーモンは同じ Task を自分で流し直さない。
- `rollback_on_fail: true` なら、起動したときの版まで down で戻してから halt する。
- 人が現実（DB の版とマイグレーションの中身）を確かめ、直してから `sakutei task cancel --store <file> sqlmigrate//<名前>` で記録を消す。差分が残っていれば、デーモンは窓の中で新しい Task を起動する。未完了の記録は `sakutei task list --store <file>` で見る。

## 実行例

```console
$ sakutei plan      --intent intent/ --sqlmigrate-config databases.yaml
$ sakutei status    --intent intent/ --sqlmigrate-config databases.yaml
$ sakutei           --intent intent/ --sqlmigrate-config databases.yaml --task-store tasks.json --interval 30s
$ sakutei task list --store tasks.json
```

本物の `sqlite3` を相手に plan → reconcile（Apply しない）→ 常駐の sakutei が Task を起動して宣言の版に収束するまでを通す受け入れテストは `tests/acceptance/sqlmigrate_cli_test.go`（`[AC-94-*]`。`sqlite3` が無ければ Skip）。provider と Task の実行体の単体テストは `internal/sqlmigrate/`。

常駐の sakutei で 1-outstanding・メンテ窓・失敗と rollback・cancel・実行中に SIGKILL・版を下げる、を実際に回す試用の手順と結果は [docs/trial/sqlmigrate/](../trial/sqlmigrate/README.md)。

## PostgreSQL（engine: postgresql）

データベースに `engine: postgresql` と書くと、そのデータベースには `psql` で流す（Backlog #126）。決まり（版の観測・1 本ずつのトランザクション・down・失敗と `rollback_on_fail`・Apply の拒否・`strategy: task` 専用）は SQLite と同じで、intent も同じ（`version` だけ）。同じ一覧に SQLite と PostgreSQL のデータベースを混ぜられる。

```yaml
# databases.yaml
postgresql_config: pg.yaml      # postgresql provider のサーバの一覧（--postgresql-config と同じ書式）
databases:
  app:                          # engine を書かなければ sqlite
    path: data/app.db
    migrations: migrations/app
  orders:
    engine: postgresql
    server: main                # pg.yaml の servers の名前
    database: orders            # つなぐデータベース（sqlmigrate は作らない）
    migrations: migrations/orders
```

- **接続の設定は postgresql provider のサーバの一覧をそのまま使う**（`host`・`port`・`user`・`password`（SecretRef）・`sslmode`・`sslrootcert`・`connect_timeout`、`psql` のパス、`secret_store`。書式は [postgresql.md](postgresql.md#サーバの一覧--postgresql-config)）。sqlmigrate の設定には接続の値を書かない。psql の呼び方（固定の引数・`ON_ERROR_STOP=1`・環境変数で渡す接続の設定・親の `PG*` を引き継がない）と、パスワードを呼ぶたびに解決して 0600 の一時的な passfile で渡し終われば消すのも postgresql provider と同じ口。マイグレーションを流すロールを管理用のロールと分けたいなら、そのロールの `user` を書いたサーバの一覧を別のファイルにして指す。
- `postgresql_config` の相対パスは sqlmigrate の設定ファイルのある場所から。`engine: postgresql` のデータベースがあるのに `postgresql_config` が無い、`server` が一覧に無い、`server`・`database` が無いのは読み込みのエラー。`engine: sqlite` に `server`・`database` を書く、`engine: postgresql` に `path` を書くのもエラー。
- 完了印の表は `schema_migrations (version bigint PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`（search_path で引く。最初の up が作る）。観測する版はこの表の最大の版（表が無ければ 0）。
- **データベースが無い**、届かない・認証の失敗・`psql` が無い・パスワードを解決できないときは Unknown（版 0 にしない。何もしない）。データベースを宣言で作るなら postgresql provider のリソースにし、sqlmigrate のリソースを `depends_on` で待たせる。
- 1 本は 1 回の psql の呼び出しで `BEGIN; … その SQL … INSERT INTO schema_migrations … COMMIT;` として流す。SQL のエラーで psql が止まり（`ON_ERROR_STOP=1`）、接続が閉じてその 1 本は巻き戻る。エラーには SQLSTATE が付く（例 `SQL error 42P01: relation "no_such_table" does not exist`）。
- **トランザクションに入れられない文**（`CREATE INDEX CONCURRENTLY`・`VACUUM`・`CREATE DATABASE`・`ALTER SYSTEM` など）は特別扱いしない。PostgreSQL が `25001`（`cannot run inside a transaction block`）で断り、その 1 本の失敗として知らせる（Task は失敗＝halt）。要るなら Task の外で人が流すか、トランザクションの中で流せる形（`CONCURRENTLY` を外すなど）にする。
- ファイルの中で `BEGIN` / `COMMIT` を書かない（SQLite と同じ）。
- **psql のメタコマンド**（文字列 `'…'`・`E'…'`、引用符の識別子 `"…"`、dollar quote `$$…$$` の外の `\`。`\!` はシェルを起動し、`\i` はほかのファイルを読む）は書けない（流す前にエラー）。コメントの中の `\` も断る（psql との読み違いを避けるため）。`E'
'` や `$$a\b$$` のような文字列の中の `\` は書ける。

### 複数のデーモン（pg_advisory_xact_lock）

同じデータベースを複数のデーモン（別々の `--task-store`。例：拠点ごとの sakutei が同じ DB を宣言している）が同時に動かさないよう、1 本ごとのトランザクションの頭で `pg_advisory_xact_lock(<固定の鍵>)` を取る（鍵は `"sakutei:sqlmigrate"` の FNV-1a。advisory lock はデータベースごとなので、ほかのデータベースとはぶつからない）。鍵が取れたら、`schema_migrations` が流す前の版のままかを同じトランザクションで確かめ、違えば `55000`（`schema_migrations is no longer at version N (another runner migrated this database)`）で何も流さずに失敗する。

- 待たされた側は、先の側がその 1 本を流し終える（COMMIT）まで待ち、進んだ版を見て止まる。同じ版を二重に流さない（列の二重の追加などにならない）。
- 止まった側の Task は失敗＝halt だが、先の側が宣言の版にしたあとの周で、現実（`schema_migrations`）が宣言の版になったのを見て記録が消える（`completed (completion mark observed; was failed)`）。
- 鍵は 1 本のトランザクションの間だけ持つ（1 本ごとに psql を起動し直すため、セッションの鍵 `pg_advisory_lock` ではなくトランザクションの鍵にしている）。

### 統合テスト

`make integration-postgresql`（docker と `sudo -n` が要る。check・CI には入らない）が、postgresql provider の統合テストと同じコンテナで `tests/integration/postgresql/sqlmigrate_integration_test.go` を回す。データベース `migdb` を作り、postgresql provider の統合テストと同じサーバの一覧を `postgresql_config` で指して、常駐の sakutei（`--interval 300ms --task-store`）を段ごとに起動する。

| 段 | 内容 |
| --- | --- |
| 0 to 3 | 版 3 を宣言 → 3 本が流れて版 3、`users`・`orders`・`schema_migrations` ができる。`E'mail\naddress'` と `$$a'b$$` が正しく渡る。記録が消える |
| 3 to 1 | 版 1 → 0003・0002 の down が逆順に流れ、`orders` と `users.email` が消える |
| a failing migration | 版 4（0004 の 2 文目が無い表に INSERT）→ 0002・0003 は残り、0004 は 42P01 で失敗、`half_done` は残らない。Task は失敗＝halt（`task cancel` で消す） |
| rollback_on_fail | 版 1 から版 4 を `rollback_on_fail: true` で → 0004 で失敗し、版 1 まで down で戻る（`rolled back: the database is at version 1`） |
| 25001 | 0002 を `CREATE INDEX CONCURRENTLY` にして版 2 → 25001 で失敗、版 1 のまま、`users.email` も残らない |
| two daemons | テストが同じ advisory lock を持つ間に、別々の task store の 2 つのデーモンを 1→3 で起動 → 2 つとも最初の 1 本（0002）で待つ（版 1 のまま、`pg_locks` に待ちが 2 つ）。鍵を外すと片方だけが 0002 を流し、もう片方は 0002 で 55000 になって何も流さずに止まる。`schema_migrations` は 1,2、`users.email` は 1 本（二重に流れない）。0003 はわざと失敗させて版を 2 で止め、どちらの記録も消えずに残るようにして確かめる |

#### 結果

既定のイメージ（PostgreSQL 17.11）で回した結果（全段 PASS、2026-10-27）:

```
--- PASS: TestSQLMigratePostgreSQLIntegration (19.18s)
    --- PASS: TestSQLMigratePostgreSQLIntegration/0_to_3 (2.25s)
    --- PASS: TestSQLMigratePostgreSQLIntegration/3_to_1_runs_down_in_reverse (1.60s)
    --- PASS: TestSQLMigratePostgreSQLIntegration/a_failing_migration_halts_and_leaves_nothing_behind (2.01s)
    --- PASS: TestSQLMigratePostgreSQLIntegration/rollback_on_fail_goes_back_to_the_version_at_launch (3.88s)
    --- PASS: TestSQLMigratePostgreSQLIntegration/a_statement_that_cannot_run_in_a_transaction_is_a_failure (1.13s)
    --- PASS: TestSQLMigratePostgreSQLIntegration/two_daemons_on_the_same_database (7.67s)
PASS
```

分かったこと:

- 失敗の文には SQLSTATE が付く（`0004_bad.up.sql failed (its transaction was rolled back; the database was left at version 3): server main database "migdb": SQL error 42P01: relation "no_such_table" does not exist`）。`CREATE INDEX CONCURRENTLY` は `SQL error 25001: CREATE INDEX CONCURRENTLY cannot run inside a transaction block` で、同じ 1 本の `ALTER TABLE ... ADD COLUMN` も残らない。
- 2 つのデーモンでは、負けた側が `0002_email.up.sql failed ...: SQL error 55000: sakutei: schema_migrations is no longer at version 1 (another runner migrated this database)` で止まる。勝つのはどちらとも決まらない（回すたびに入れ替わる）。
- 最初の段で `E'mail\naddress'`（改行）と `$$a'b$$` がそのまま渡る（メタコマンドの検査が文字列の中の `\` を断らない）。

### 通常のテスト

`make check` は偽の psql（`internal/sqlmigrate/postgres_test.go` の Runner）で回る：0→3→1、接続の設定が環境変数と passfile で渡り、パスワードが引数・環境変数・SQL に出ないこと、失敗（42P01・25001）で版がそのまま、`rollback_on_fail`、版の確かめ（55000）で何も流さないこと、観測できないもの（届かない・psql が無い・secret が無い）が Unknown、メタコマンドの拒否と字句（`E''`・`''`・`""`・dollar quote・`$1`・コメント）、設定の検査。
