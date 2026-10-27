# PostgreSQL provider（postgresql）

PostgreSQL のロール・データベース・データベースの権限・拡張を宣言で収束させる provider。Go の依存を増やさないため、ドライバを入れずに `psql` の CLI を exec で呼ぶ（sqlmigrate が `sqlite3` を呼ぶのと同じ形）。決まりの正は設計書 §29.1「PostgreSQL provider」（Backlog #125）。ここは使い方の説明で、矛盾したら spec/ が優先する。

| provider 名 | ID | 扱うもの |
| --- | --- | --- |
| `postgresql` | `<server>/role/<name>` | ロール（属性・所属・パスワード） |
| | `<server>/database/<name>` | データベース（所有者・符号化・照合順序・接続数の上限） |
| | `<server>/database/<db>/grant/<role>` | あるロールがあるデータベースに直に持つ権限（CONNECT・CREATE・TEMPORARY） |
| | `<server>/database/<db>/extension/<name>` | あるデータベースの拡張と版 |

- 完全階級（Read・Apply・List・BulkObserve）。prune できる（`pruneable: true`）が、**利用者の表があるデータベースは消さない**（後述）。
- `<server>` は設定ファイルのサーバの名前。名前（ロール・データベース・拡張）は 1〜63 バイトで、`/`・NUL・制御文字を含まない（データベースの名前は `=` も含まない。libpq が接続文字列と読むため）。それ以外は SQL では**常に quote する**ので、大文字・空白・引用符を含んでもよい。
- 持たないもの：スキーマ・表・列の権限、default privileges（`ALTER DEFAULT PRIVILEGES`）、ロールの設定（`ALTER ROLE ... SET`）、データベースの設定、表領域。要れば別の provider か Task（sqlmigrate の PostgreSQL 版）で足す。

## サーバの一覧（--postgresql-config）

サーバの一覧は設定ファイルで渡す。`--postgresql-config <file>`、または `--provider-config` の `postgresql` の `postgresql_config` キー（§28.9）。フラグは常駐の `sakutei`・`plan`・`reconcile`・`status`・`import`・`sync` で効く。書式は intent と同じ JSON / YAML。

```yaml
# pg.yaml
psql: /usr/bin/psql                   # 省略可（既定は PATH の psql）。"/" を含めば設定ファイルからの相対パス
secret_store: secrets.json            # file SecretBackend（§20.5）。SecretRef を使うなら必須
scope: site=tokyo                     # 省略可。List・BulkObserve が ID に付ける固定の scope（既定 root）
servers:
  main:                               # サーバの名前。ID の先頭になる（a-z 0-9 _ -）
    host: db.example.net              # 省略可（libpq の既定）。/ で始まれば Unix ソケットのディレクトリ
    port: 5432                        # 省略可
    dbname: postgres                  # 管理用につなぐ DB（既定 postgres）
    user: sakutei                     # 管理用のロール（必須）
    password: secretref:file/pg/main/admin   # 省略可（SecretRef だけ）
    sslmode: verify-full              # 省略可（disable / allow / prefer / require / verify-ca / verify-full）
    sslrootcert: ca.pem               # 省略可
    connect_timeout: 10s              # 省略可（既定 10s。秒に切り上げ、最小 2 秒）
    scope: site=osaka                 # 省略可。このサーバの ID の scope（既定は上の scope）
    verify_passwords: false           # 省略可。true でロールのパスワードを確かめる（後述。超ユーザが要る）
```

```console
$ sakutei reconcile --intent intent/ --postgresql-config pg.yaml
$ sakutei status --intent intent/ --postgresql-config pg.yaml --json
```

- 管理用のロールには、作る・変えるものに見合う権限が要る（ロールなら CREATEROLE、superuser 属性を宣言するなら超ユーザ、データベースなら CREATEDB、拡張は拡張による）。`verify_passwords` には超ユーザが要る（`pg_authid` を読むため）。
- `password` は **SecretRef でしか書けない**（`"secretref:file/<path>"` か `{"$secret": "<path>", "backend": "file"}`）。生の値を書くとエラーになり、その値は文言にも出ない。読み込みの時点では store にその path があることだけを確かめる。
- 未知のキーはエラー（書き間違いを黙って無視しない）。

### psql の呼び方

- シェルは通さない。引数は固定のオプションだけ（`-X -w -q -A -t -v ON_ERROR_STOP=1 -v VERBOSITY=verbose -f -`）で、SQL は**標準入力**で渡す。読む問い合わせは名前を SQL に埋め込まない決まった文で、結果を 1 つの JSON にして返させる。
- 接続の設定は子プロセスの環境変数（`PGHOST`・`PGPORT`・`PGDATABASE`・`PGUSER`・`PGSSLMODE`・`PGSSLROOTCERT`・`PGCONNECT_TIMEOUT`）で渡す。親の `PG*` の環境変数は引き継がない（`PGSERVICE`・`PGOPTIONS`・`PGPASSWORD` などで黙って別の所へつながないように）。
- 管理用のパスワードは psql を起動する直前に secret store から解決し（JIT、§20.2）、0600 の一時的な passfile に書いて `PGPASSFILE` で渡し、終われば消す。**引数・環境変数の値・ログ・エラーには出ない**。`password` を書かなければ passfile を渡さない（`~/.pgpass`・証明書・peer の認証などに任せる）。
- 名前は識別子として常に `"..."` で囲み（中の `"` は `""`）、値は `E'...'` で囲む（中の `'` は `''`、`\` は `\\`）。NUL を含む名前・値はエラー。
- エラーは SQLSTATE で分ける。届かない（終了コード 2）・時間切れ・一時的な SQLSTATE（class 08・40・53・55・57、まだ無いデータベース 3D000・ロールなど 42704）は Transient、psql が無い・認証の失敗・そのほかの SQL のエラーは Permanent。エラーの文から SCRAM・md5 の検証値は消す。

## intent の例

```json
{
  "resources": [
    {"provider": "postgresql", "id": "main/role/app",
     "fields": {"login": "true", "createdb": "false", "connection_limit": "20",
                "member_of": "pg_read_all_data", "password": "secretref:file/pg/app#gen1"}},
    {"provider": "postgresql", "id": "main/role/reader",
     "fields": {"login": "true", "member_of": "app"}},
    {"provider": "postgresql", "id": "main/database/appdb",
     "fields": {"owner": "app", "encoding": "UTF8", "lc_collate": "C.UTF-8", "lc_ctype": "C.UTF-8",
                "template": "template0", "connection_limit": "100"},
     "deletion_policy": "PruneWithinScope"},
    {"provider": "postgresql", "id": "main/database/appdb/grant/reader",
     "fields": {"privileges": "CONNECT,TEMPORARY"}},
    {"provider": "postgresql", "id": "main/database/appdb/extension/hstore",
     "fields": {"version": "1.8"}}
  ]
}
```

### フィールド

宣言したものだけを管理し、**書かないものは触らない**（例えば `createdb` を書かなければ、誰が変えても drift にならず Apply もしない）。種類に合わないフィールドは読み込みの時点で断る。

| 種類 | フィールド | 値 | 比べ方・決まり |
| --- | --- | --- | --- |
| ロール | `login`・`superuser`・`createdb`・`createrole`・`inherit` | `true` / `false` | そのまま |
| ロール・データベース | `connection_limit` | 整数（`-1` は上限なし） | そのまま |
| ロール | `member_of` | 所属するロールの一覧（`,` 区切り） | 集合として（順は問わない）。宣言すれば**一覧ちょうど**にする（無いものは GRANT、余分なものは REVOKE） |
| ロール | `password` | SecretRef | 下の「パスワード」 |
| データベース | `owner` | ロールの名前 | そのまま |
| データベース | `encoding` | `UTF8` など | 英数字だけを小文字にして比べる（`UTF-8` と `utf8` は同じ）。**作るときだけ**：後で違う値を宣言すると計画のエラー |
| データベース | `lc_collate`・`lc_ctype` | `C.UTF-8` など | codeset の部分を同じく正規化（`en_US.UTF-8` と `en_US.utf8` は同じ）。**作るときだけ** |
| データベース | `template` | データベースの名前 | **作るときだけ**。観測できないので作った後は比べない。`encoding`・照合順序を template1 と違う値にするなら `template0` を書く |
| 権限 | `privileges` | `CONNECT`・`CREATE`・`TEMPORARY`（`TEMP`）の一覧 | 大文字小文字・`TEMP`・順を問わない。**必須・空にできない**（全部外すなら宣言から外し、prune か明示の absent で消す＝`REVOKE ALL`） |
| 拡張 | `version` | 版 | 宣言したときだけ比べ、違えば `ALTER EXTENSION ... UPDATE TO` |
| すべて | `server`・`role`・`database`・`extension` | ID と同じ値（省略可） | identity |

- 権限は「そのロールがそのデータベースに**直に**持つ権限」。PUBLIC の権限・ロールの所属から継ぐ権限は数えない。Apply は 1 つのトランザクションで `REVOKE ALL` と `GRANT` を流して宣言ちょうどにする。
- データベースの所有者は暗に全部の権限を持つので、所有者の権限は List に出さない（宣言はできる）。

### パスワード

PostgreSQL からパスワードは読めない（`pg_authid` は超ユーザだけが読め、読めても検証値）。永久 drift を出さず、平文を外に出さないために:

- Apply は値を secret store から解決し、**SCRAM-SHA-256 の検証値**（`pg_authid` の形、salt は毎回新しい）にしてから `CREATE ROLE` / `ALTER ROLE ... PASSWORD` に渡す。サーバのログ（`log_statement`）や psql のエラーにも平文は出ない（`psql` の `\password` と同じやり方）。パスワードは **ASCII の文字だけ**（PostgreSQL は ASCII 以外に SASLprep をかけるが、この provider は持たないので、ASCII 以外を含むものは PermanentError）。
- 同じトランザクションでロールのコメントに `sakutei:password=<SecretRef の参照>` を書く（値ではなく参照）。Read はこの参照を `password` として返すので、**宣言の参照（`#gen<N>` の世代を含む）が変わったときだけ差が出る**。世代を上げれば（cause E）設定し直す。コメントは `password` を宣言したロールでだけ書き換える（そのロールのほかのコメントは失われる）。
- `verify_passwords: true` のサーバでは、Read はさらに `pg_authid` の検証値（SCRAM-SHA-256 か md5）がその参照の値と合うかを確かめ、合わなければ `password` を空として返す——**手で変えられたパスワードが drift として見え、Apply が戻す**。このときだけ Read でもパスワードを secret store から解決する（値はその場の比較にだけ使う）。管理用のロールが超ユーザでなければ観測できない（Unknown）。既定はオフ（コメントの参照だけを比べ、手での変更は見えない）。

## 動き方

- **Read**：管理用の DB に 1 回だけ問い合わせ（`pg_roles`・`pg_auth_members`・`pg_database` と `aclexplode` を 1 つの JSON に）、拡張はそのデータベースにつないで `pg_extension` を読む。無ければ absent。
- **Apply**：op の宣言の値と、その場で読み直した現実から文を組み立てる（同じ plan を 2 回流しても同じ。Create のときに相手が既に在っても宣言の値にそろえる）。ロールの属性・所属・パスワードは 1 つのトランザクションで流す。`CREATE DATABASE` はトランザクションの中で流せないので 1 文だけで流す。適用した後は**読み戻し**、宣言と違う値で残ったら（サーバが別の形で持つ・別のロールが与えた権限が残るなど）その値を添えた PermanentError にする（Apply を繰り返さない）。
- **Delete**：prune（PruneWithinScope）か明示の absent からだけ来て、**破壊的**として予算に数える（§12・§13）。`CASCADE`・`REASSIGN OWNED`・`DROP OWNED` は使わない。
  - データベース：**利用者の表**（普通の表・パーティションの表・マテリアライズドビュー・外部表。`pg_catalog`・`information_schema`・`pg_toast`・一時スキーマのものと、拡張が持つ表は数えない）が 1 つでもあれば消さず、「the database has N user table(s), so it is not dropped」の PermanentError。表を数えてから `DROP DATABASE` までの間に作られた表は防げない。接続を許さないデータベース（`datallowconn = false`）は表を数えられないので消さない。ほかに接続がある間は Transient（55006）。
  - ロール：オブジェクトを持つ・権限を持つロールは PostgreSQL が断る（2BP01）ので、それを PermanentError で知らせる。同じ周に所有するデータベースが prune で消えれば、次の周に消える。
  - 権限：`REVOKE ALL`。別のロールが与えた権限が残れば PermanentError。
  - 拡張：`DROP EXTENSION`。ほかのオブジェクトが頼っていれば PostgreSQL が断る（2BP01、PermanentError）。
- **観測できないもの**：届かない・時間切れ・認証の失敗・接続を許されない・psql が無い・管理用のパスワードを解決できない・出力が読めないは Unknown（absent にしない。作りも消しもしない）。
- **List**：全サーバの snapshot と、全データベース（境界の内）の拡張が読めたときだけ complete。1 つでも失敗すれば error で、prune はその周止まる。接続を許さないデータベースがあると拡張が読めないので List は失敗する。
- **BulkObserve**：サーバごとに snapshot 1 回、データベースごとに拡張 1 回（Read と等価）。止まったサーバのリソース・拡張を読めないデータベースの拡張は結果に入れず、SDK がそれだけを Read に回して Unknown にする。**1 つのサーバが止まっても、ほかのサーバは観測と収束を続ける**。

### 管理の境界（§7）

List は次を返さない（prune で消えない）。これらに向けた宣言は読み込みの時点で断る。

- `pg_` で始まるロール、管理用のロール（`user`）、接続しているロール。`member_of` に `pg_*` のロールを書くのはよい。
- `template0`・`template1`・`postgres`、管理用の DB（`dbname`）。これらの中の権限・拡張も。
- データベースの所有者の権限、`plpgsql`（どのデータベースにも template1 から入る。宣言はできる）。

それ以外は PruneWithinScope の「閉じた世界」に入る。例えば公式の docker イメージが `POSTGRES_USER` の名前で作るデータベースも、宣言しなければ（空なら）prune で消える。残したいものは宣言するか、PruneWithinScope を使わない。

## 統合テスト（docker、`make integration-postgresql`）

本物の PostgreSQL を相手にした統合テスト（Backlog #125）。build tag `integration_postgresql` の付いたファイル（`tests/integration/postgresql/`）にあり、`make check`・`make test`・CI には入らない。

```console
$ make integration-postgresql
```

要るもの: docker と、パスワードなしの `sudo`（`sudo -n docker`）。**ホストに psql は要らない**。

- 公式の `postgres` イメージ（既定 `postgres:17.11-bookworm@sha256:858bb5e98eed02eb8cafbcdc007f9a65d1b613972028944a8ec2133ccaf80d27`、**tag と digest で固定**。digest は 2026-10-27 に docker pull で確かめた値。`SAKUTEI_IT_POSTGRES_IMAGE` で差し替えられる）で、サーバ（`sakutei-pgit-server`）と、psql を動かすだけのクライアント（`sakutei-pgit-client`、同じイメージで `sleep infinity`）を docker のネットワーク `sakutei-pgit-net` に立てる。サーバはホストに公開しない。
- `--postgresql-config` の `psql` はラッパーのスクリプトで、渡された環境変数（`PGHOST` など）を付けてクライアントのコンテナの中で `psql` を動かす。passfile は `docker cp` でコンテナに渡して終われば消す（引数にパスワードを出さない）。
- 管理用のロールのパスワードは secret store に置き、`verify_passwords: true` で回す。sakutei のバイナリを作り、`reconcile`・`status --json` を回す。手での変更・確かめは sakutei の外から psql で行う。名前に空白・引用符・`\`・`;`・`--` を含むロール（`sk it "o'q\`）とデータベース（`sk-db 'x"; --`）も宣言する。
- 終われば（落ちても）コンテナとネットワークを消す。`make integration-postgresql` は残った `sakutei-pgit-*` を最後にもう一度消す。

確かめること:

| 段 | 内容 |
| --- | --- |
| create from empty | ロール 3・データベース 2・権限 1・拡張 2 を宣言。status が missing 8 → reconcile（1 周）→ 適合 8。属性・所有者・符号化・照合順序・拡張の版・引用符を含む名前が宣言どおり。`app` が宣言のパスワードで実際につながる（SCRAM-SHA-256 の検証値が本物と合う） |
| changed by hand | 手で `ALTER ROLE app CREATEDB CONNECTION LIMIT 3`・パスワードを変える・`REVOKE app FROM reader`・`REVOKE CONNECT ... FROM reader`・`ALTER DATABASE appdb CONNECTION LIMIT 5` → status が drift 4 → reconcile で戻る（パスワードでまたつながる） |
| a new generation of the password and an extension update | secret store の値と intent の `#gen` を上げる → 新しいパスワードでつながり、古いのではつながらない。hstore を 1.7 → 1.8。`encoding` を変える宣言は計画のエラーで、status は観測できない 1（終了コード 3） |
| no drift however many cycles | reconcile と status を 5 周 → drift 0、計画が出ない、`pg_authid` の検証値が書き直されない |
| a database with user tables is not pruned | 表のある orphan DB・空の orphan DB（拡張あり）・orphan ロールを手で作り、PruneWithinScope で reconcile → 空の DB と orphan ロールは消える（`[DESTRUCTIVE]`）、表のある DB は「user table(s), so it is not dropped」の PermanentError で残り、表の行も残る。`postgres`・`template0`・`template1`・`pg_monitor`・管理用のロールは残る |
| a stopped server | `docker stop` → status は 8 つとも観測できない（終了コード 3）、reconcile は何もせず（`prune halted`・`8 resource(s) skipped`）。`docker start` の後は適合 8 |
| clean up through prune | 宣言を `app` だけにして PruneWithinScope → データベース・権限・拡張は 1 周目、それらを持っていたロールは 2 周目に消える。残るのは `postgres`・`template0`・`template1` と `app`・管理用のロール |

### 結果

既定のイメージ（PostgreSQL 17.11）で回した結果（全段 PASS、2026-10-27）:

```
--- PASS: TestPostgreSQLIntegration (90.11s)
    --- PASS: TestPostgreSQLIntegration/create_from_empty (13.47s)
    --- PASS: TestPostgreSQLIntegration/changed_by_hand_is_drift_and_comes_back (9.94s)
    --- PASS: TestPostgreSQLIntegration/a_new_generation_of_the_password_and_an_extension_update (9.14s)
    --- PASS: TestPostgreSQLIntegration/no_drift_however_many_cycles (21.40s)
    --- PASS: TestPostgreSQLIntegration/a_database_with_user_tables_is_not_pruned (23.42s)
    --- PASS: TestPostgreSQLIntegration/a_stopped_server_is_unobservable_and_nothing_is_done (5.07s)
    --- PASS: TestPostgreSQLIntegration/clean_up_through_prune (7.67s)
PASS
```

分かったこと:

- 空から 1 周で 8 つすべてができる（データベースの後に権限・拡張が流れる順になった。順が逆でも、まだ無いデータベース・ロールは Transient で次の周に回る）。
- 手で変えたパスワードは `verify_passwords` で drift として見える（`~ password "***" → "secretref:file/pg/app#gen1"`。観測の側は空なので `***` と出る）。
- 公式のイメージは `POSTGRES_USER`（ここでは管理用のロールと同じ `sakutei`）の名前のデータベースを作る。管理の境界の外ではないので status は宣言外（unmanaged）と出し、PruneWithinScope では空なので消える（`- DELETE postgresql//main/database/sakutei [DESTRUCTIVE]`）。
- 表のある orphan：`prune apply postgresql//main/database/sk_orphan_full: ... permanent: the database has 1 user table(s), so it is not dropped (Sakutei never drops data; ...)`。prune は毎周その Delete を試みて同じ文で断られる（表を消すか、宣言し直すまで）。拡張（hstore）だけがある空の DB は消える。
- サーバを止めると、名前が引けない（`could not translate host name`）のでつながらず、`prune halted: List returned error: ... unreachable` で何もしない。名前が引けて届かない場合は `connect_timeout` まで待つ。
- 片付けの prune で、データベースを所有する・権限を持つロールの `DROP ROLE` は同じ周では 2BP01 の PermanentError（`the role owns objects or holds privileges, so it is not dropped`）になり、データベースが消えた次の周に消える。

## 通常のテスト

`make check` は偽の psql（`internal/postgresql/pgtest`）で回る。偽の psql は provider が流す決まった形の文と読む問い合わせだけを SQL の字句の決まりで解釈し（`""`・`''`・`E'\\'`）、環境変数と passfile で接続を確かめ、エラーは SQLSTATE 付きで終了コード 3、`CREATE DATABASE` をトランザクションの中で流すとエラーにする。

- quote：識別子・文字列の quote と SQL 注入（`x"; DROP ROLE sakutei; --` などの名前が、そのままの 1 つの名前で作られ、ほかに何も起きない）。
- SCRAM-SHA-256：RFC 7677 の例と同じ検証値になる。md5 の形も確かめられる。
- 契約テスト（`sdk.ContractTester`、ロール・データベース・権限・拡張の 4 種類で、届かないのを absent にしないことを含む）・BulkObserve 等価性テスト・管理の境界・パスワード（平文が引数・環境変数・SQL・plan に出ない、passfile が残らない、世代を上げれば設定し直す、`verify_passwords` で手の変更が見える）・作るときだけのフィールド・権限と所属の集合・拡張の版・Delete（表のある DB・オブジェクトを持つロール・頼られている拡張は消さない）・観測できないもの（届かない・パスワード違い・secret が無い・psql が無い）・読み戻しの不一致・本物の exec の経路（テストのバイナリを偽の psql として起動）・設定ファイルの検査（`internal/postgresql/*_test.go`）。
- 受け入れテスト（`tests/acceptance/postgresql_test.go`、`[AC-125-*]`）：偽の psql を実行ファイルにして、DefaultRegistry から作って契約テスト、PruneWithinScope で表のある DB が残ること、届かないと prune が止まり何もしないこと、`sakutei reconcile`・`status --json` で空から適合になりパスワードが出力に出ないこと。
