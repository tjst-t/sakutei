# SQLite のマイグレーション provider（sqlmigrate）

SQLite のデータベース（ファイル）に版つきのマイグレーションを流す provider。v1.5 の単純 Task（§6.2、`update_policy.strategy: task`）を実物で動かすためのもの。決まりの正は設計書 §29.1「単純 Task の実 provider：sqlmigrate」（v3.17）。ここは使い方の説明で、矛盾したら spec/ が優先する。

| provider 名 | ID | 扱うもの |
| --- | --- | --- |
| `sqlmigrate` | データベースの名前（例 `app`） | `schema_migrations` の版（マイグレーションをどこまで流したか） |

- 標準階級（List・BulkObserve を持たない）で、prune の対象にしない（データベースは Sakutei が作ったり消したりしない）。
- Go の標準ライブラリだけで書くため、ドライバは入れず `sqlite3` の CLI を呼ぶ。SQL は標準入力で渡し、シェルは通さない。
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
