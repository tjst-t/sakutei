# LDAP provider（ldap）

LDAP のディレクトリの**項目（entry）**を、宣言した属性だけ宣言どおりに保つ provider（Backlog #144）。ユーザ・グループ・OU を intent に書いておき、手で変えられても戻す。
決まりの正は設計書 §29.1「LDAP provider」。ここは使い方の説明で、矛盾したら spec/ が優先する。

**389 Directory Server で確かめた**（下の「統合テスト」、389 Directory Server 3.1）。項目の読み書きは標準の LDAP（RFC 4511 の検索・追加・変更・削除、RFC 4512 の subschema、RFC 4514 の DN）だけを使うので、OpenLDAP の slapd など他の実装でも動くはずだが、**確かめていない**。

- Go の依存を増やさないため、**OpenLDAP のクライアント**（`ldapsearch`・`ldapmodify`）を exec で呼ぶ。変更は LDIF で標準入力に渡す。sakutei を動かすホストに `ldapsearch`・`ldapmodify`（Debian・Ubuntu の `ldap-utils`、RHEL 系の `openldap-clients`、openSUSE の `openldap2-client`）が要る。`-o ldif-wrap=no` を使うので OpenLDAP 2.4.24 以降。
- 完全の階級：Read・Apply・List・BulkObserve（サーバごとに部分木の検索 1 回とスキーマの読み 1 回）。

## サーバの一覧（--provider-config の ldap_config）

```yaml
# providers.yaml（--provider-config）
providers:
  ldap:
    ldap_config: ldap.yaml
```

```yaml
# ldap.yaml
ldapsearch: /usr/bin/ldapsearch      # 省略可（既定は PATH の ldapsearch）
ldapmodify: /usr/bin/ldapmodify      # 省略可（既定は PATH の ldapmodify）
secret_store: secrets.json           # file SecretBackend（必須）
scope: site=tokyo                    # 省略可（List・BulkObserve の ID の scope）
servers:
  dir1:                              # サーバの名前（ID の先頭）
    uri: ldap://ldap.example.net:389 # 必須。ldap:// か ldaps://（1 つだけ）
    starttls: true                   # ldap:// で StartTLS を必須にする（-ZZ。できなければ失敗）
    ca_cert: ca.pem                  # 省略可（サーバの証明書を確かめる CA。既定は OpenLDAP の設定）
    bind_dn: cn=Directory Manager    # 必須（simple bind）
    bind_password: secretref:file/ldap/dir1   # 必須（SecretRef だけ）
    base_dn: dc=example,dc=com       # 必須。この下の項目だけを扱う
    filter: (objectClass=*)          # 省略可。List に出す項目の条件（管理の境界）
    timeout: 10s                     # 省略可（接続と検索の時間の上限）
```

- `bind_password` は **SecretRef だけ**（生の文字列は読み込みで断る。値は文言にも出さない）。値はコマンドを起動するたびに secret store から解決し、0600 の一時ファイルに書いて **`-y` で渡し、終われば消す**（引数・環境変数・ログには出さない）。
- 暗号化は `ldaps://` か `starttls: true` を使う。証明書は必ず確かめる（`LDAPTLS_REQCERT=demand`。利用者の `ldaprc` の `TLS_REQCERT never` などで緩めさせない）。平文の `ldap://` も書けるが、bind のパスワードが平文で流れる。
- 親の環境変数のうち `LDAP` で始まるもの（`LDAPTLS_*` など）は引き継がない。
- 相対パスは設定ファイルのあるディレクトリから。未知のキーはエラー。

## intent の例

```yaml
resources:
  - provider: ldap
    id: dir1/ou=people,dc=example,dc=com
    fields:
      objectClass: '["top","organizationalUnit"]'
      ou: People
      description: the people
  - provider: ldap
    id: dir1/uid=alice,ou=people,dc=example,dc=com
    depends_on:
      - dir1/ou=people,dc=example,dc=com
    fields:
      objectClass: '["top","inetOrgPerson"]'
      uid: Alice
      cn: Alice Liddell
      sn: Liddell
      mail: '["alice@example.com","al@example.com"]'
      userPassword: secretref:file/ldap/alice
  - provider: ldap
    id: dir1/cn=admins,ou=groups,dc=example,dc=com
    fields:
      objectClass: groupOfNames
      cn: admins
      member: uid=alice,ou=people,dc=example,dc=com
```

### ID

`<サーバ>/<DN>`。DN は**正規形**で書く：属性の型と値は小文字、`,` `=` `+` の前後に空白なし（RFC 4514 のエスケープ。例 `cn=smith\, john,ou=people,dc=example,dc=com`）。正規形でない ID は読み込みの時点で断り、正規形を示す。

- 値まで小文字にするのは、DN に使う標準の属性（`dc`・`ou`・`cn`・`uid` など）が大文字小文字を区別しないため。サーバに `ou=People` と書かれた項目も `ou=people` で同じ項目を指す。綴りが 1 つに決まるので、List の ID と intent の ID が大文字小文字だけで食い違い、二重に作ろうとしたり、宣言した項目を prune で消したりすることがない。
- 項目を作るときの DN は、**葉の RDN の値だけ**、宣言した属性の値の綴りを使う（上の例の alice は `uid=Alice,ou=people,...` として作られる）。
- `base_dn` の下でなければならない（`base_dn` そのもの・その外は断る）。`bind_dn` の項目も断る。

### フィールド

| フィールド | 意味 |
| --- | --- |
| 属性の名前 | その属性の値の集合。値が 1 つならその値、複数なら JSON の文字列の配列（`'["a","b"]'`）。順は問わない。`""` と `[]` は「その属性が無い」（在れば消す）。名前の大文字小文字は問わない（`objectClass` と `objectclass` は同じ。同じ属性を 2 回書くとエラー）。 |
| `objectClass` | 必須。比べ方は下の「objectClass」。 |
| `userPassword` | SecretRef だけ。書き込みだけ（下の「パスワード」）。 |
| `server`・`dn` | 識別子（ID と同じ値。省略できる）。 |
| `implied_objectclass` | 観測だけ（サーバが足してよいクラス）。宣言できない。 |

- **宣言した属性だけ**を管理する。宣言していない属性（サーバが付けるもの、ほかの道具が書くもの）は触らない。
- 値が `[` で始まるときは JSON の配列として読む（`[` で始まる 1 つの値は `'["[x"]'` と書く）。値は UTF-8 で、空の値・NUL は書けない。改行を含む値は JSON の `\n` で書ける。
- 属性のオプション（`cn;lang-ja`）と、サーバが保つ操作属性（`createTimestamp`・`modifiersName`・`entryUUID`・`nsUniqueId` など）は宣言できない。
- 葉の RDN の属性を宣言するなら、値の 1 つが RDN の値（大文字小文字は問わない）でなければならない。宣言しなければ、作るときに RDN の値を足す。
- 変えるときは属性ごとに `replace`（値の集合をその一覧ちょうどに）を 1 つの modify で送る。サーバは全部を一度に当てるか、何も変えない。

### objectClass

サーバ（389 Directory Server など）は項目に**上位のクラスを足す**（`inetOrgPerson` を書くと `organizationalPerson`・`person` が付く。`top` も）。これを drift にしないため、`objectClass` だけは次のように比べる：

- 宣言したクラスがすべて在り（大文字小文字は問わない）、
- 宣言に無いクラスは、どれも在るクラスの上位のクラスか `top`。

上位のクラスはサーバのスキーマ（項目の `subschemaSubentry` が指す subschema の `objectClasses`）から読み、観測の `implied_objectclass` に入れる。手で足された補助クラス（`posixAccount` など）は drift になり、Apply で宣言の一覧に戻す（サーバはまた上位のクラスを足す）。

### DN を値に持つ属性

`member`・`uniqueMember`・`owner`・`seeAlso`・`manager`・`secretary`・`roleOccupant`・`memberOf`・`nsRoleDN`・`aliasedObjectName` は、値を DN の正規形で比べる（サーバは `UID=Alice, OU=People,...` を `UID=Alice,OU=People,...` のように空白を詰めて保存する）。

### パスワード

`userPassword` は **SecretRef で書き込みだけ**。

- 項目を作るとき、と、**項目に `userPassword` が無いとき**に、secret store の値（平文、TLS の上で送る）を書く。サーバはハッシュにして保存する。
- 値は読まない・比べない：Read は観測に値（ハッシュ）を入れず、在ることだけを `(set)` として返す。在れば適合（postgresql の `password` と同じく、永久 drift を出さない）。
- そのため、**SecretRef の世代を上げても書き直さない**（観測から今の値が分からないので、変わったかどうかを言えない）。パスワードを替えるときは、新しい値を secret store に置いて intent の SecretRef を変え、サーバの `userPassword` を消す（`ldapmodify` で `delete: userPassword`）。次の周に今の値を書く。
- plan・status・ログに値は出ない（ハッシュも出さない）。

## 動き方

- **観測（Read）**：`ldapsearch -b <DN> -s base '(objectClass=*)' '*' subschemaSubentry` と、スキーマの読み 1 回。項目が無い（結果コード 32）は absent。
- **差分**：属性の名前の大文字小文字を問わずに観測と突き合わせ、値の集合として比べる（PlanRefiner）。残った差分には、サーバの今の値を理由（reason）に添える。
- **Apply**：Create は `add`（項目が既に在れば、宣言の属性を `replace` でそろえる。親がまだ無ければ一時的な失敗で次の周に作る。親は `depends_on` で先に作れる）、Update は属性ごとの `replace`。適用した後に読み戻し、宣言と違う値で残れば PermanentError（サーバが書き換える属性は宣言できない）。
- **届かない・bind の失敗・TLS の失敗・コマンドが無い・bind のパスワードを解決できない**は観測できない（unknown）で、absent にしない。そのサーバの List は error になり、prune は止まる。

### 管理の境界（§7）

List は `base_dn` の下（`base_dn` そのものは含まない）の、`filter` に合う項目だけを返す（ページに分けて受け取るので、サーバの件数の上限に当たらない）。`bind_dn` の項目、サブエントリ（`ldapSubEntry`）・tombstone は返さない。スキーマ（`cn=schema`）・設定（`cn=config`）は `base_dn` の外にある。`filter` に合わない項目は、宣言しなければ prune されない（例：`(!(ou=outside))`）。

### 削除

- prune（PruneWithinScope）の Delete は**破壊的**（変更予算に数える、§12・§13）。
- **子の項目がある項目は消さない**（PermanentError「the entry has child entries, so it is not deleted」）。子が先に消えれば（子も orphan なら同じ周か次の周に）消せる。
- 消えている項目の Delete は何もしない。

## 統合テスト（docker、`make integration-ldap`）

本物の 389 Directory Server を相手にした統合テスト（Backlog #144）。build tag `integration_ldap` の付いたファイル（`tests/integration/ldap/`）にあり、`make check`・`make test`・CI には入らない。

```console
$ make integration-ldap
```

要るもの: docker と、パスワードなしの `sudo`（`sudo -n docker`）。**ホストに OpenLDAP のクライアントは要らない**。

- 389 Directory Server の公式のイメージ（既定 `389ds/dirsrv:3.1@sha256:af99ccf42c1cd02799d674ffdee3612a1bbd426c1a8a636da67ea349659ee5e0`、**tag と digest で固定**。digest は 2026-10-27 に docker pull で確かめた値で、`latest` と同じ。`SAKUTEI_IT_389DS_IMAGE` で差し替えられる）で、サーバ（`sakutei-ldapit-server`、ホスト名も同じにして自己署名の証明書をその名前で作らせる）と、`ldapsearch`・`ldapmodify` を動かすだけのクライアント（`sakutei-ldapit-client`、同じイメージで `sleep infinity`）を docker のネットワーク `sakutei-ldapit-net` に立てる。サーバはホストに公開しない。`dsconf ... backend create --create-suffix` で `dc=example,dc=com` を作る。
- 設定の `ldapsearch`・`ldapmodify` はラッパーのスクリプトで、クライアントのコンテナの中でコマンドを動かす。`-y` のパスワードのファイルと CA（`LDAPTLS_CACERT`）は `docker cp` でコンテナに渡して終われば消す（引数にパスワードを出さない）。
- 接続は StartTLS（`ldap://...:3389` と `starttls: true`、サーバの CA で確かめる）。LDAPS（`ldaps://...:3636`）でも status を回す。`filter: (!(ou=outside))`。
- sakutei のバイナリを作り、`--provider-config` で `reconcile`・`status --json` を回す。手での変更・確かめは sakutei の外から Directory Manager の `ldapmodify`・`ldapsearch` で行う。sakutei の出力に bind・利用者のパスワードとハッシュ（`{PBKDF2`・`{SSHA`）が出ないことも毎回確かめる。
- 終われば（落ちても）コンテナとネットワークを消す。`make integration-ldap` は残った `sakutei-ldapit-*` を最後にもう一度消す。

確かめること:

| 段 | 内容 |
| --- | --- |
| create from empty | OU 2・利用者 2（`inetOrgPerson`。複数の値、`,` と `"` を含む値、非 ASCII の値、改行を含む値、`userPassword`）・グループ 1（`member` を空白入りの DN で）を宣言。status が missing 5 → reconcile → 適合 5。objectClass にサーバが上位のクラス（`organizationalPerson`・`person`）を足し、それが drift にならない。`userPassword` はハッシュで保存され、alice が宣言のパスワードで bind できる |
| changed by hand | 手で alice の `mail` を変えて宣言していない `telephoneNumber` を足す、OU の `description` を変える、グループから bob の `member` を外す、bob に `extensibleObject` を足す → status が drift 4 → reconcile で戻る（`telephoneNumber` は触らない） |
| userPassword is write-only | SecretRef の世代を上げても適合のまま（書き直さない）。サーバの `userPassword` を手で消すと drift 1 → reconcile で今の値を書き、新しいパスワードで bind でき、古いのではできない |
| no drift however many cycles | reconcile と status を 5 周 → drift 0、計画が出ない、5 つの項目の `modifyTimestamp` が変わらない（書き込みが無い） |
| LDAPS and a failed bind | LDAPS の設定で status → 適合 5。誤った bind のパスワードの設定で status → 5 つとも観測できない（終了コード 3、missing にしない） |
| an entry with children is not pruned | 手で `ou=handmade`・`ou=leaf`・`ou=outside` を作り、`ou=handmade` の下の `uid=kept` だけを宣言して PruneWithinScope で reconcile → `ou=leaf` は消える（`[DESTRUCTIVE]`）、`ou=handmade` は「child entries」の PermanentError で残る、filter に合わない `ou=outside` と base の項目は残る |
| a stopped server | `docker stop` → status は 5 つとも観測できない（終了コード 3）、reconcile は何もせず `prune halted`、手で作った orphan は残る。`docker start` の後は適合 5 |

### 結果

既定のイメージ（389 Directory Server 3.1）で回した結果（全段 PASS、2026-10-27）:

```
--- PASS: TestLDAPIntegration (83.58s)
    --- PASS: TestLDAPIntegration/create_from_empty (12.56s)
    --- PASS: TestLDAPIntegration/changed_by_hand_is_drift_and_comes_back (11.95s)
    --- PASS: TestLDAPIntegration/userPassword_is_write-only (12.81s)
    --- PASS: TestLDAPIntegration/no_drift_however_many_cycles (22.73s)
    --- PASS: TestLDAPIntegration/LDAPS_and_a_failed_bind (5.04s)
    --- PASS: TestLDAPIntegration/an_entry_with_children_is_not_pruned (8.99s)
    --- PASS: TestLDAPIntegration/a_stopped_server_is_unobservable_and_nothing_is_done (9.51s)
PASS
```

分かったこと:

- 389 Directory Server は `objectClass` に上位のクラスを足し、`member` の DN の空白を詰めて保存する（`UID=Alice, OU=People, dc=...` → `UID=Alice,OU=People,dc=...`、大文字小文字はそのまま）。どちらも比べ方で吸収し、何周しても書き込みが起きない。
- StartTLS の失敗（証明書を確かめられない）は `ldap_start_tls: Connect error (-11)` で終了コード 1、届かないのは 255。どちらも観測できない（unknown）として扱う。
- `-LLL` でもページに分けた検索（`-E pr=...`）は `# pagedresults` のコメントを出す（読むときに飛ばす）。

## 通常のテスト

- 単体・契約テスト：`internal/ldap`（偽の ldap コマンド `internal/ldap/ldaptest`。`sdk.ContractTester`、DN の正規形・LDIF・値の書き方・スキーマ、属性の大文字小文字、objectClass・DN の値・userPassword の比べ方、管理の境界と prune、届かない・bind・TLS の失敗、BulkObserve と Read の等価、本物の exec の経路）。
- 受け入れ：`tests/acceptance/ldap_test.go`（`[AC-144-1]`〜`[AC-144-3]`、偽の ldap コマンドを実行ファイルにして本物の CLI と `--provider-config` で）。
