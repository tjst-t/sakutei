# S3 互換のバケット provider（s3-bucket）

オブジェクトストレージのバケットを宣言で収束させる provider。S3 互換の API だけを使い、特定の製品に依らない。決まりの正は設計書 §29.1「S3 互換のバケット provider」（Backlog #122）。ここは使い方の説明で、矛盾したら spec/ が優先する。

| provider 名 | ID | 扱うもの |
| --- | --- | --- |
| `s3-bucket` | `<endpoint の名前>/<bucket>`（例 `minio/app-logs`） | バケット 1 つ（バージョニング・ポリシー・ライフサイクル・タグ・Object Lock） |

- 完全階級（Read・Apply・List・BulkObserve）。prune できる（`pruneable: true`）が、**中身のあるバケットは消さない**（後述）。
- 持たないもの：アカウント・利用者・アクセスキーの管理（製品ごとに違う。要れば外の provider で足す、§28.9）、オブジェクトの読み書き・削除。
- HTTP は Go の標準ライブラリだけで書き、AWS Signature Version 4 を自前で実装している（AWS の公開のテストベクタで単体テスト）。

## endpoint の一覧（--s3-config）

endpoint の一覧は設定ファイルで渡す。`--s3-config <file>`、または `--provider-config` の `s3-bucket` の `s3_config` キー（§28.9）。フラグは常駐の `sakutei`・`plan`・`reconcile`・`status`・`import`・`sync` で効く。書式は intent と同じ JSON / YAML。

```yaml
# s3.yaml
secret_store: secrets.json            # file SecretBackend（§20.5）。相対パスは設定ファイルから
scope: site=tokyo                     # 省略可。List・BulkObserve が ID に付ける固定の scope（既定 root）
endpoints:
  minio:                              # endpoint の名前。ID の前半になる（a-z 0-9 _ -）
    endpoint: https://s3.example.net:9000   # scheme は必須（http / https）。パスは書かない
    api_base_path: /s3                # 省略可。API のパスの前置き（既定は無し。path なら <前置き>/<bucket>）
    region: us-east-1                 # 省略可（既定 us-east-1）。署名と CreateBucket の LocationConstraint に使う
    addressing: path                  # path（既定。<endpoint>/<bucket>）| virtual（<bucket>.<host>）
    access_key: secretref:file/s3/minio/access_key
    secret_key: secretref:file/s3/minio/secret_key
    observe:                          # 省略可：観測用のキー（下の「観測用と書き込み用の資格」）
      access_key: secretref:file/s3/minio/ro_access_key
      secret_key: secretref:file/s3/minio/ro_secret_key
    write:                            # 省略可：書き込み用のキー（Apply だけが使う）
      access_key: secretref:file/s3/minio/rw_access_key
      secret_key: secretref:file/s3/minio/rw_secret_key
    bucket_prefix: app-               # 省略可。この接頭辞のバケットだけを扱う
    ca_file: ca.pem                   # 省略可。この CA だけで証明書を検証する
    insecure_skip_verify: false       # 明示したときだけ true（ca_file とは排他）
    timeout: 10s                      # 省略可。1 要求あたり（既定 10s）
    fields:                           # 省略可。この endpoint で扱う設定（既定は object_lock の外の 4 つ。下の「S3 互換実装との差異への対応」）
      - policy
      - lifecycle
      - tags
    storage_class_aliases:            # 省略可。endpoint が返す StorageClass の名前 → 標準の名前
      <実装の名前>: GLACIER
```

```console
$ sakutei reconcile --intent intent/ --s3-config s3.yaml
$ sakutei status --intent intent/ --s3-config s3.yaml --json
```

- `access_key`・`secret_key` は **SecretRef でしか書けない**（`"secretref:file/<path>"` か `{"$secret": "<path>", "backend": "file"}`）。生の値を書くとエラーになり、その値は文言にも出ない。値は Read・Apply・List の直前に file SecretBackend から解決し、provider には持たない。読み込みの時点では store にその path があることだけを確かめる。
- **`bucket_prefix` を勧める**。List は資格から見える全バケットを返すので、PruneWithinScope を宣言すると、prefix が無ければアカウントの全バケットが「閉じた世界」になる。prefix を書けば、その接頭辞の外のバケットは List にも Read にも出ない（intent に書くと読み込みの時点で断る）。
- `api_base_path`（Backlog #160）は、S3 の API がホストの根ではなくパスの下にいるとき（逆プロキシの下のパス、試験用の模擬など）に書く。要求のパスは path 形式なら `<前置き>/<bucket>`、virtual-host 形式なら `<前置き>/`（バケット名はホストに付く）、ListBuckets は `<前置き>/`。署名（SigV4）は前置きを含んだパスで作る（前置きを書き換えて転送する逆プロキシの下では署名が合わない）。`/` で始まるパスだけを受け付け（末尾の `/` は落とす。`/` は前置き無し）、クエリ・`%`・空白・空の段・`.`・`..` の段は読み込みの時点でエラー。省けば今までどおり前置き無し。`endpoint` には今までどおりパスを書かない。
- virtual-host 形式で https を使うとき、名前に `.` を含むバケットは証明書のワイルドカードに合わないことが多い。
- 未知のキーはエラー（書き間違いを黙って無視しない）。

## 観測用と書き込み用の資格（Backlog #186）

endpoint に観測用（`observe:`）と書き込み用（`write:`）のキーを分けて書ける（§20.7。ssh 越しの provider の
[`observe:`・`write:`](../remote.md#観測用と書き込み用の資格) と同じ決まり）。どちらも `access_key`・`secret_key` だけを持ち
（SecretRef でしか書けないのは上と同じ）、書かないキーは endpoint の上の値を受け継ぐ。どちらも書かなければ今までどおり 1 組のキー。

- 観測（`Read`・`List`・`BulkObserve`。`plan`・`status`・読み取りの口・常駐の毎周の観測、Apply のあとの効いたかの読み直し）は
  観測用のキーだけで、書き込み（`Apply`）は書き込み用のキーだけで署名する。Apply の中の読み戻し（設定したポリシー・
  ライフサイクル・タグの Get）は書き込み用で行う。
- 書かない回（`plan`・`status`・`import`・`--mode alert-only`・`--freeze` の `reconcile` など）は、書き込み用のキーが
  secret store に無くても起動する（確かめず、取り出さない。書き込みを頼まれれば何も送らずに断る）。書く回は無ければ
  読み込みで断る（`endpoints.<名前>.write.access_key: ... is not in the secret store`）。

観測用のキーに要る最小のポリシー（MinIO RELEASE.2026-08-04T00-00-00Z で確かめた。`make integration-s3` の段と同じ。
どれか 1 つを外すと、そのバケットは観測できない）:

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": [
      "s3:ListAllMyBuckets",
      "s3:GetBucketVersioning",
      "s3:GetBucketPolicy",
      "s3:GetLifecycleConfiguration",
      "s3:GetBucketTagging"
    ],
    "Resource": ["arn:aws:s3:::*"]
  }]
}
```

- `fields` で扱う設定を絞った endpoint では、外した設定の Get の許可は要らない（Get を呼ばない）。
- `fields` に `object_lock` を書いた endpoint（下の「Object Lock」）では、観測用のキーに `s3:GetBucketObjectLockConfiguration` も要る
  （書かなければ要らない。既定の `fields` には入らないので、上の 5 つのままで今までどおり観測できる）。
- **許可の足りない Get は「設定が無い」と読まない**。Get が 403（`AccessDenied`）なら、そのバケットはその周は観測できない
  （Unknown。absent にしない、§3 第3原則）。設定が無いこと（`NoSuchBucketPolicy`・`NoSuchLifecycleConfiguration`・
  `NoSuchTagSet`。404）だけを空の値として読む。`s3:ListAllMyBuckets` が無ければ ListBuckets が 403 で、List も error（prune は止まる）。
  MinIO は ListAllMyBuckets の無い利用者に見えるバケットだけの一覧を返すのでなく、ListBuckets を 403 で断る（確かめた）。
- `s3:GetBucketLocation` は要らない（provider は呼ばない。region は設定で渡す）。

書き込み用のキーのポリシー（同じ版で確かめた。作る・消す・4 つの設定の Put と外す、Apply の中の読み戻し）:

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": [
      "s3:ListAllMyBuckets",
      "s3:CreateBucket", "s3:DeleteBucket",
      "s3:PutBucketVersioning", "s3:GetBucketVersioning",
      "s3:PutBucketPolicy", "s3:DeleteBucketPolicy", "s3:GetBucketPolicy",
      "s3:PutLifecycleConfiguration", "s3:GetLifecycleConfiguration",
      "s3:PutBucketTagging", "s3:GetBucketTagging"
    ],
    "Resource": ["arn:aws:s3:::*"]
  }]
}
```

- `object_lock` を扱う endpoint では、書き込み用のキーに `s3:PutBucketObjectLockConfiguration` と `s3:GetBucketObjectLockConfiguration`
  （Apply の中の今の設定の読みと読み戻し）も要る。作るときに有効にする（`enable_on_create`）なら、AWS の決まりでは
  `s3:PutBucketVersioning` も要る（作るときの有効化はバージョニングを Enabled にする）。
- **Object Lock の設定の PUT は管理用（書き込み用）の権限、Get は観測用の権限でもできる**前提で分ける（MinIO
  RELEASE.2026-08-04T00-00-00Z で、`s3:GetBucketObjectLockConfiguration` だけを足した観測用の利用者の Get が通り、PUT が 403 で
  断られることを確かめた）。ただし、実装によっては Object Lock の設定について観測用と書き込み用の権限をまだ分けられないものがある
  （利用者の権限を粗い組でしか持てないなど）。その実装では観測用のキーにも書き込みのできる権限を渡すことになるので、その endpoint の
  観測用のキーの扱いに気をつける。
- ライフサイクル・タグを外す（DeleteBucketLifecycle・DeleteBucketTagging）は、それぞれ `s3:PutLifecycleConfiguration`・
  `s3:PutBucketTagging` の許可で通る（AWS と同じ）。
- 書き込み用に読むだけのキーを書き違えると、Apply は endpoint の 403（`AccessDenied`）の PermanentError で止まり、何も変わらない。

## 範囲を intent に書く（`providers:`、Backlog #182）

List・BulkObserve が ID に付ける `scope` と、endpoint ごとの扱うバケットの接頭辞（`bucket_prefix`）は管理の範囲なので、intent の
トップレベルの `providers:` に endpoint の一覧と同じ形で書ける（§1.7・§7.7）。endpoint・鍵・時間と、実装が持つ API を言う
`fields`・`storage_class_aliases` はつなぎ方として endpoint の一覧に残る。

```yaml
# intent/providers.yaml
providers:
  s3-bucket:
    scope:                     # endpoint の一覧の scope（ラベルの map で書く）
      site: tokyo
    endpoints:
      minio:                   # endpoint の一覧にある endpoint（無い名前は断る）
        bucket_prefix: app-
```

- intent にだけ書けば、設定に書いたのと同じに動く。設定にだけあれば今までどおり読み、`plan`・`reconcile`・`status`・常駐が
  「is only in the provider config: write it in the intent too (providers.s3-bucket.…)」と警告する（常駐では一度だけ）。
- 両方にあれば設定は安全の網として残り、**食い違えば読み込みで断る**（弱い側に寄せない。常駐は起動で断り、動いているあいだに
  食い違えばその周を飛ばす）。知らないキーも断る。設定から外すのは次のメジャー版。
- どれがつなぎ方でどれが範囲かの一覧は [ranges.md](ranges.md)。

## intent の例

```json
{
  "resources": [
    {
      "provider": "s3-bucket",
      "id": "minio/app-logs",
      "fields": {
        "versioning": "Enabled",
        "policy": "{\"Version\": \"2012-10-17\", \"Statement\": [{\"Sid\": \"public-read\", \"Effect\": \"Allow\", \"Principal\": \"*\", \"Action\": \"s3:GetObject\", \"Resource\": \"arn:aws:s3:::app-logs/*\"}]}",
        "lifecycle": "[{\"ID\": \"expire-logs\", \"Status\": \"Enabled\", \"Filter\": {\"Prefix\": \"logs/\"}, \"Expiration\": {\"Days\": 30}}]",
        "tags": "{\"env\": \"prod\", \"owner\": \"platform\"}"
      },
      "deletion_policy": "PruneWithinScope"
    }
  ]
}
```

### フィールド

宣言したものだけを管理し、**書かないものは触らない**（例えば `versioning` を書かなければ、誰が変えても drift にならず Apply もしない）。

| フィールド | 値 | 比べ方 |
| --- | --- | --- |
| `versioning` | `Enabled` / `Suspended` | そのまま。一度も設定していないバケットの観測は `""` |
| `policy` | バケットのポリシー（JSON のオブジェクト） | 正規形（下）。`""` は「ポリシーが無い」で、宣言すれば外す |
| `lifecycle` | ルールの JSON の配列。各ルールは S3 の XML の `Rule` をそのまま JSON にした形（`Filter`・`Expiration`・`Transition`・`NoncurrentVersionExpiration` など。繰り返す要素は配列）。**`ID` と `Status` は必須** | 正規形（下）。`""` か `[]` は「ルールが無い」 |
| `tags` | JSON のオブジェクト（値は文字列。50 個まで） | キー順。`""` か `{}` は「タグが無い」 |
| `object_lock` | Object Lock（JSON のオブジェクト。下の「Object Lock」）。**endpoint の `fields` に書いたときだけ** | 期間を Days にそろえた正規形。`""` は「Object Lock が無い」 |
| `endpoint`・`bucket` | ID と同じ値（省略可） | identity |
| `creation_date`（観測だけ） | ListBuckets の CreationDate | 比べない |
| `unsupported`（観測だけ） | endpoint がその API を持たない（NotImplemented）か、endpoint の `fields` に無いフィールドの一覧 | 比べない |

### 正規形（永久 drift を出さない）

endpoint はポリシーやルールを書かれたとおりには持たないことがある（MinIO は `"Principal": "*"` を `{"AWS": ["*"]}` に、`"Action": "s3:GetObject"` を `["s3:GetObject"]` に書き換えて返す。SeaweedFS は書かれたまま返す）。宣言と観測を同じ正規形にしてから比べる。

- **policy**：キー順・空白を捨てる。`Statement` が 1 つのオブジェクトなら配列に。`Action`・`NotAction`・`Resource`・`NotResource` と、`Principal`・`NotPrincipal`・`Condition` の値の文字列は、並べて重複を落とした配列に。`Principal` の `"*"` は `{"AWS": ["*"]}` と同じ。`Statement` の順は変えない。
- **lifecycle**：値をすべて文字列に（`30` と `"30"` は同じ）、日時は RFC 3339 の UTC に（`2027-01-01T00:00:00.000Z` と `2027-01-01T00:00:00Z` は同じ）、空の値（`"Filter": {}`・`<Prefix></Prefix>`）を落とし、1 要素の配列は中身だけに、ルールと配列の並びは正規形の順に。
- 正規形にしてもそろわない形で endpoint が持つとき（endpoint が文を足すなど）、Apply は読み戻して気づき、**endpoint の持っている形を添えた PermanentError** にする（Apply を繰り返さない）。その形で宣言し直せば適合する。

## Object Lock（Backlog #205）

書き換えを禁じたい版（監査の記録・バックアップ）を置くバケットに、Object Lock とバケットの既定の保持を宣言で持たせる。
扱うのは、作るときの有効化と、既定の保持（mode と期間）だけ。オブジェクトごとの保持・Legal Hold は扱わない（オブジェクトを扱わない）。

```yaml
# s3.yaml（endpoint の一覧）：object_lock は fields に書いたときだけ扱う
endpoints:
  minio:
    endpoint: https://s3.example.net:9000
    access_key: secretref:file/s3/minio/access_key
    secret_key: secretref:file/s3/minio/secret_key
    fields:
      - versioning
      - policy
      - lifecycle
      - tags
      - object_lock
```

```json
{"provider": "s3-bucket", "id": "minio/audit-archive",
 "fields": {"object_lock": "{\"enable_on_create\": true, \"mode\": \"COMPLIANCE\", \"years\": 7}"}}
```

| キー | 値 | 意味 |
| --- | --- | --- |
| `enable_on_create` | `true` / `false`（省略は false） | 作るときに `x-amz-bucket-object-lock-enabled: true` を送る。**書いたときだけ送る**（持たない実装でも宣言どおりにできるように、既定は送らず PutObjectLockConfiguration で有効にする）。作る手順の引数で、比べない |
| `mode` | `GOVERNANCE` / `COMPLIANCE` | バケットの既定の保持の mode。期間と組で書く |
| `days` / `years` | 正の整数（どちらか一方。days は 36500、years は 100 まで） | 既定の保持の期間 |

- `object_lock` を宣言すれば Object Lock を有効にする。`mode` と期間を書かなければ（`{}`・`{"enable_on_create": true}`）既定の保持は無い（有れば外す）。
  `""` は「Object Lock が無い」。宣言しなければ比べない（書いたときだけ比べて合わせる）。
- **期間は Days にそろえて比べる**。`years` は 365 倍した `days` と同じ（7 年は 2555 日）。Years で PUT しても Get で Days を返す実装があるため。
  観測の値は、Object Lock が無ければ `""`、有れば `{"days":2555,"mode":"COMPLIANCE"}`、既定の保持が無ければ `{}`。
- **既定の保持は PutObjectLockConfiguration だけで掛ける**。versioning を読んで有効かを確かめる手順は踏まない（versioning の Get を持た
  ない実装でも動く。versioning を宣言していなければ、その Get が 501 でもエラーにならない）。Apply は今の設定を Get で読み、同じなら
  PUT を送らず（作るときの有効化で済んだときなど）、違えば PUT して読み戻して確かめる。
- **Get の読み方**：404 `ObjectLockConfigurationNotFoundError` は「設定が無い」、Rule の無い応答は「既定の保持が無い」。403 は今までどおり
  観測できない（Unknown。「設定が無い」と読まない）。501・NotImplemented は、宣言していれば観測できないとして計画の時点で止める。
- **実装が受けない設定は理由つきで止める**。PUT が 400（`InvalidArgument`・`MalformedXML` など。受けない mode・期間）なら
  「the endpoint refused the Object Lock configuration …」の PermanentError（黙って流さない）。既にあるバケットに有効にできない実装
  （SeaweedFS・MinIO は 409 `InvalidBucketState`）は「the endpoint refused to turn Object Lock on for this existing bucket …」。
- **既にあるバケットの保持の設定を変える Apply は、向きによらず全部、予算に数え、承認ゲートを通す**（設計の決定）。有効にする・mode を
  変える・期間を延ばす・期間を短くする・既定の保持を外す、のどれも。延ばす向きと COMPLIANCE へ変える向きは戻せず、短くする向きと
  外す向きは実装に断られずに上書きされ、これから書く版の守りが弱まるため。宣言を既定で断る形にはせず、正しい変更も宣言で書け、人の
  承認で止める。承認は planned_change のレーン（kind `update`、§13.3）で、レーンが無ければ `ChangeAwaitingApproval` で持ち越し、
  `plan` は `blocked: approval` と向き（例「shortens the default retention 2555 → 30 days」）を出す。**新しく作るバケットに初めて
  掛けるのはふつうの作成**（承認は要らない）。
- **既にあるバケットと食い違う宣言は理由つきで止め、作り直さない**：Object Lock が有効なバケットに `""`（外せない）と、Object Lock の
  無い既にあるバケットに `enable_on_create: true`（作るときだけの引数）は計画の時点のエラー。後者は `enable_on_create` を外せば
  PutObjectLockConfiguration で有効にする変更（承認が要る）になる。
- Object Lock の有効なバケットも、中身が無ければ prune で消える（中身のあるバケットは今までどおり消えない）。

## 動き方

- **Read**：ListBuckets で在るかを見て（無ければ absent）、GetBucketVersioning・GetBucketPolicy・GetBucketLifecycleConfiguration・GetBucketTagging を読む（endpoint の `fields` に無い設定の Get は呼ばない）。設定が無いこと（NoSuchBucketPolicy など）は空の値。
- **Apply**：Create は CreateBucket（既に自分のものなら成功）の後に宣言したフィールドを設定する。Update は差分のフィールドだけを設定する。同じ plan を 2 回流しても同じ。
- **Delete**：prune（PruneWithinScope）か明示の absent からだけ来て、**破壊的**として予算に数える（§12・§13）。DeleteBucket は空のバケットしか消せない。中身があれば endpoint が `BucketNotEmpty` で断り、provider はそれを「中身があるので消さない（オブジェクトは消さない。手で空にするか、宣言し直す）」という PermanentError として知らせる。**オブジェクトを消す機能は持たない**。
- **観測できないもの**：届かない・時間切れ・5xx（501 を除く）・認証の失敗（401 / 403）・資格を解決できない・証明書を検証できないは Unknown（absent にしない。作りも消しもしない）。リダイレクト（region や addressing の誤り）は辿らず、要求の誤りとして知らせる。
- **List**：全 endpoint の ListBuckets が ContinuationToken を最後まで辿れたときだけ complete。1 つでも失敗すれば error で、prune はその周止まる。
- **BulkObserve**：endpoint ごとに ListBuckets 1 回と各バケットの設定の読み（Read と等価）。止まった endpoint のバケットは結果に入れず、SDK がそれだけを Read に回して Unknown にする。**1 つの endpoint が止まっても、ほかの endpoint のバケットは観測と収束を続ける**。

### endpoint がその API を持たないとき

- GET が 501 / `NotImplemented` を返す API のフィールドは State に入れず `unsupported` に挙げる。**宣言していなければ無視**する。**宣言していれば観測できない**として計画の時点で止める（「cannot observe lifecycle: the endpoint does not implement GetBucketLifecycleConfiguration (NotImplemented)」）。常駐と `status` では観測できないものとして数える。一回性の `plan`・`reconcile` は、ほかの provider の計画のエラーと同じくその文を出して終了コード 1 で止まる。
- GET は既定の値を返し、PUT だけが NotImplemented の endpoint もある（下の古い SeaweedFS）。この場合 Read は欠けを知れないので、宣言した値は drift として見え、Apply が「the endpoint does not implement this API (NotImplemented)」の PermanentError で止まる。その endpoint の `fields` からそのフィールドを外せば、宣言は読み込みの時点で断られる（下）。

## S3 互換実装との差異への対応

S3 互換の実装には AWS と違う振る舞いのものがある（Backlog #132、設計書 §29.1）。provider が自分で吸収するものと、endpoint の設定で知らせるものがある。

**provider が吸収するもの（設定は要らない）**

- **API を持たない（501 / `NotImplemented`）**：上の「endpoint がその API を持たないとき」。そのフィールドは `unsupported` に挙げ、宣言していなければ無視、宣言していれば計画の時点で止める。
- **policy・lifecycle の書き換え**：endpoint が意味の同じ別の書き方で返すもの（`"Principal": "*"` と `{"AWS": ["*"]}`、文字列と 1 要素の配列、数と文字列、日時の書き方、空の要素）は正規形でそろえて比べる（上の「正規形」）。
- **読み戻しの確かめ**：policy・lifecycle・tags は設定した後に読み戻し、正規形でもそろわない形で endpoint が持つなら、その形を添えた PermanentError にする（毎周 Apply を繰り返さない）。

**endpoint の設定で知らせるもの**

- **`fields`（扱う設定の一覧）**：`versioning`・`policy`・`lifecycle`・`tags` のうち、この endpoint で扱うもの。省略すれば 4 つ全部。API を持たないことを 501 ではなく別のコード（400・403 など）で返す実装では、Read が宣言を知らないので、既定のままだと宣言していない設定の Get でも観測エラーになり、**バケット全体が観測できなくなる**。`fields` から外した設定は：
  - Get を呼ばない（Read・BulkObserve のどちらも）。観測では `unsupported` に挙がり、State に入らない。
  - intent で宣言すると、**読み込みの時点でエラー**（「cannot manage versioning: endpoint … does not handle it (not in the endpoint's fields in the s3 config)」）。ほかの invalid な intent と同じく、その周は Observe・Apply・prune をしない。intent から外すか、`fields` に足す。
  - 宣言していなければ無視する（ほかのフィールドは観測・収束を続ける）。
- **`storage_class_aliases`（StorageClass の名前の読み替え）**：lifecycle の `StorageClass`（`Transition`・`NoncurrentVersionTransition` の中など）を、受け取ったあと実装独自の名前に書き換えて返す実装がある。その実装が PUT では標準の名前しか受け付けないと、どちらの名前で宣言しても毎周 drift になる（標準の名前で書けば観測が独自の名前、独自の名前で書けば PUT が断られる）。`storage_class_aliases` に「実装が返す名前 → 標準の名前」を書くと、観測した lifecycle の StorageClass をこの表で読み替えてから比べる。Apply の読み戻しの確かめも同じ表で読み替える。intent には**標準の名前**（`GLACIER`・`STANDARD_IA` など）で書く。
  - 表が無いとき、読み戻しは「keeps it in another form」の PermanentError になり、文言で `storage_class_aliases` を案内する。

```yaml
endpoints:
  archive:
    endpoint: https://s3.example.net
    access_key: secretref:file/s3/archive/access_key
    secret_key: secretref:file/s3/archive/secret_key
    fields:                 # versioning の Get に 400 を返す実装：versioning を扱わない
      - policy
      - lifecycle
      - tags
    storage_class_aliases:  # lifecycle の StorageClass を独自の名前で返す実装
      <実装の名前>: GLACIER
```

## 統合テスト（docker、`make integration-s3`）

本物の S3 互換のサーバを相手にした統合テスト（Backlog #122）。build tag `integration_s3` の付いたファイル（`tests/integration/s3/`）にあり、`make check`・`make test`・CI には入らない。

```console
$ make integration-s3
```

要るもの: docker と、パスワードなしの `sudo`（`sudo -n docker`）。

- サーバを `docker run` で立て、127.0.0.1 の空いたポートにだけ公開する。既定のイメージは **tag と digest（`name:tag@sha256:…`）で固定**している（digest は 2026-10-27 に docker pull で確かめた値）。環境変数で差し替えられる（Backlog #123）。

  | endpoint の名前 | 必須か | 環境変数 | 既定のイメージ |
  | --- | --- | --- | --- |
  | `seaweedfs` | 必須 | `SAKUTEI_IT_SEAWEEDFS_IMAGE` | `chrislusf/seaweedfs:4.51@sha256:f7a49e3f3972ebed5be2ed4befa4a0c68eb7bdddba6e692d42729ceb545134b8` |
  | `minio` | 任意 | `SAKUTEI_IT_MINIO_IMAGE` | `pgsty/minio:RELEASE.2026-08-04T00-00-00Z@sha256:b6bfe7239bfc83fb90d31612d9704d86039dd714f7904b3f1ad68f211e602372`（MinIO のソースから作ったイメージ。MinIO は 2026-04 にアーカイブされ、公式のイメージは匿名で取れないことがある） |
  | `seaweedfs-old` | 任意 | `SAKUTEI_IT_SEAWEEDFS_OLD_IMAGE` | `chrislusf/seaweedfs:3.80@sha256:1055999e08eed1789b0ae45d235126e4495e23d3fb9d6396293fd42539b1ae6a`（バージョニング・ポリシー・タグの PUT が NotImplemented の版） |

- 始める前に各イメージを `docker pull` する。**必須のサーバ（seaweedfs）が取れなければ失敗**にする。**任意のサーバは取れなければ（または環境変数が空なら）使わず**、「`minio (optional) is not used: docker pull … failed: …`」を出して、そのサーバに関わる段だけを飛ばす（`an endpoint without the API` は seaweedfs-old が無ければ SKIP。minio が無ければ main は seaweedfs だけになり、`a stopped server` は seaweedfs を止め、動いたままのサーバとして seaweedfs-old を使う。それも無ければ止めたサーバの側だけを確かめる）。

- 資格は secret store（file SecretBackend）に置き、`--s3-config` の endpoint の一覧（`bucket_prefix: sk-it-`）から SecretRef で引く。sakutei のバイナリを作り、`status --json`・`reconcile` を回す。手での変更（ポリシー・バージョニング・オブジェクトの PUT）は sakutei の外から S3 の API で行う。
- 終われば（落ちても）コンテナを `docker rm -f -v` で消す。`make integration-s3` は残った `sakutei-s3it-*` を最後にもう一度消す。

確かめること:

| 段 | 内容 |
| --- | --- |
| create from empty | main（4 つの API を持つサーバ。seaweedfs と、使えれば minio）に全フィールドを宣言したバケット。status が drift 2（現実に無い）→ reconcile → 適合 2、各サーバの値が宣言どおり |
| changed by hand | main の各サーバのバージョニングを Suspended に、ポリシーを Deny に手で変える → status が drift 2 → reconcile で戻る |
| no drift however many cycles | 宣言どおりのまま reconcile と status を 5 周 → drift 0、計画が出ない、ポリシーが書き直されない（MinIO が書き換えた形のまま） |
| a bucket with objects is not pruned | 中身のある orphan・空の orphan・`bucket_prefix` の外のバケットを手で作り、PruneWithinScope で reconcile → 空の orphan だけが消える（`[DESTRUCTIVE]`）。中身のある orphan は `BucketNotEmpty` の PermanentError で残る。prefix の外は List に出ず残る |
| a stopped server | minio を `docker pause`（minio が無ければ seaweedfs）→ status で止めたサーバのバケットだけが観測できない、reconcile はそれを skip（作らない）し、動いたままのサーバのバケットは観測を続ける。List が完全でないので prune は止まり、動いたままのサーバの空の orphan も消えない |
| after the server is back | unpause → reconcile → main がすべて適合 |
| an endpoint without the API | 今の minio・seaweedfs は 4 つの GET をどれも持つことを確かめる。seaweedfs-old に versioning・tags を宣言 → バケットは作られ、Apply が NotImplemented の PermanentError で止まり、status は drift。フィールドを宣言しなければ適合 |
| fields leave out settings | 同じ seaweedfs を別の名前 `seaweedfs-compat`（`bucket_prefix: sk-compat-`、`fields` は policy・tags だけ）でも登録。policy・tags を宣言したバケットが適合し、versioning は触らない。versioning を宣言すると `plan` が `fields` を名指したエラーで止まる。`storage_class_aliases` は SeaweedFS では再現できない（この版は Transition を含む lifecycle の PUT が 501、古い版は受け付けるが読み戻すと Transition が無い）ので、偽サーバの単体・受け入れテストで確かめる |
| Object Lock（#205、`TestS3ObjectLock`） | seaweedfs（と使えれば minio）に `fields` に `object_lock` を書いた endpoint（`bucket_prefix: sk-lock-`、scope `site=it`）を足す。`enable_on_create` つきで GOVERNANCE 1 日・COMPLIANCE 1 年のバケットを作り、Get が宣言どおり・status が適合・3 周 converged。GOVERNANCE を 2 日に延ばす宣言はレーンが無ければ `ChangeAwaitingApproval` で変わらず、承認つきの update のレーンで変わる。既定の保持を外すのも同じ。Object Lock の無い既にあるバケットに `enable_on_create` は `plan` が「does not re-create」で止まり、外すと（承認つき）PUT が 409 `InvalidBucketState` で理由つきで止まる。minio では Get の許可だけの利用者の Get が通り PUT が 403 |
| clean up through prune | 宣言を 1 つだけにして PruneWithinScope で reconcile → 空のバケットが prune で消える（中身のあるものは残る）。コンテナは teardown で消す |
| 観測用と書き込み用の資格（#186、`TestS3CredentialSplit`、minio が要る） | minio の中の `mc admin` で、上の読むだけのポリシーの利用者 `sakutei-ro` と書き込み用のポリシーの利用者 `sakutei-rw` を作り、`observe:`・`write:` に書いた endpoint の一覧（`bucket_prefix: sk-split-`）を使う。観測用のキーだけの store で `status --json`（complete・drift 1）と `plan` が通り、`reconcile`（書く回）は `endpoints.minio.write.access_key` が無いと断ってバケットを作らない。`sakutei-ro` の CreateBucket・PutBucketTagging・PutBucketVersioning・DeleteBucket は 403 で断られる。両方のキーの store の `reconcile` で収束し（空の orphan の prune を含む）、4 つの設定を外す・付け直すのも通る。読むだけのポリシーから 1 つずつ外した利用者では、そのバケットの Read が Unknown（absent にならない） |

### 結果

Object Lock の段（#205、2026-10-30。SeaweedFS 4.51・MinIO RELEASE.2026-08-04T00-00-00Z。TestS3Integration・TestS3CredentialSplit も同じ回で全段 PASS）:

```
--- PASS: TestS3ObjectLock (1.21s)
    --- PASS: TestS3ObjectLock/create_with_Object_Lock_and_observe_it (0.20s)
    --- PASS: TestS3ObjectLock/changing_the_retention_needs_approval (0.16s)
    --- PASS: TestS3ObjectLock/an_existing_bucket_without_Object_Lock (0.06s)
    --- PASS: TestS3ObjectLock/the_observe_user_reads_with_GetBucketObjectLockConfiguration (0.78s)
    --- PASS: TestS3ObjectLock/clean_up (0.01s)
```

- SeaweedFS 4.51・MinIO のどちらも、作るときの有効化を持ち（バージョニングも Enabled になる）、Years で掛けた保持を Years のまま返す
  （Days で返す実装の振る舞いは偽サーバで確かめる）。どちらも Object Lock の無い既にあるバケットへの有効化を 409 `InvalidBucketState`
  で断る（MinIO は「Object Lock configuration cannot be enabled on existing buckets」）。受けない mode は SeaweedFS が 400
  `MalformedXML`、MinIO が 400 `InvalidArgument`、0 日は SeaweedFS が 400 `InvalidRetentionPeriod`、MinIO が 400 `InvalidArgument`。
  保持を短くする PUT・外す PUT はどちらも断らない（承認ゲートで止める理由）。

それより前の段:

既定のイメージ（SeaweedFS 4.51・MinIO RELEASE.2026-08-04T00-00-00Z・SeaweedFS 3.80）で回した結果（全段 PASS、2026-10-27）:

```
--- PASS: TestS3Integration (25.97s)
    --- PASS: TestS3Integration/create_from_empty (0.21s)
    --- PASS: TestS3Integration/a_policy_and_versioning_changed_by_hand_are_drift_and_come_back (0.16s)
    --- PASS: TestS3Integration/declared_as_is,_no_drift_however_many_cycles (0.10s)
    --- PASS: TestS3Integration/a_bucket_with_objects_is_not_pruned (0.29s)
    --- PASS: TestS3Integration/a_stopped_server_is_unobservable_and_nothing_is_done (25.11s)
    --- PASS: TestS3Integration/after_the_server_is_back_it_converges_again (0.03s)
    --- PASS: TestS3Integration/an_endpoint_without_the_API (0.05s)
    --- PASS: TestS3Integration/clean_up_through_prune (0.04s)
PASS
```

観測用と書き込み用の資格の段（#186、2026-10-28。TestS3Integration も同じ回で全段 PASS）:

```
--- PASS: TestS3CredentialSplit (3.06s)
    --- PASS: TestS3CredentialSplit/observe_with_the_read-only_user,_without_the_write_keys (0.02s)
    --- PASS: TestS3CredentialSplit/a_write_with_the_read-only_user_is_refused (0.01s)
    --- PASS: TestS3CredentialSplit/converge_with_the_write_user (0.36s)
    --- PASS: TestS3CredentialSplit/each_read_action_is_needed_and_a_missing_one_is_unknown,_not_absent (1.83s)
```

読むだけのポリシーから 1 つ外した利用者の Read は、どれも `authentication failed (HTTP 403): AccessDenied: Access Denied.`
（`ListBuckets` か、外した許可の Get）で Unknown になる。

任意のサーバのイメージをわざと取れないものにした場合（同日）:

- `SAKUTEI_IT_MINIO_IMAGE=registry.invalid/minio:none`：`minio (optional) is not used: docker pull registry.invalid/minio:none failed: … no such host` を出し、seaweedfs と seaweedfs-old で全段 PASS（`a stopped server` は seaweedfs を止め、seaweedfs-old の orphan が消えないことを確かめる）。
- さらに `SAKUTEI_IT_SEAWEEDFS_OLD_IMAGE=chrislusf/seaweedfs:0.0.0-nonexistent`：seaweedfs だけで回り、`an endpoint without the API` だけが SKIP（`seaweedfs-old is not used: docker pull … not found`）、ほかは PASS。
- 必須の `SAKUTEI_IT_SEAWEEDFS_IMAGE=registry.invalid/weed:none`：`seaweedfs: docker pull registry.invalid/weed:none: …` で FAIL（テストを始めない）。

分かったこと:

- **MinIO はポリシーを書き換えて持つ**。`"Principal": "*"`・`"Action": "s3:GetObject"` と宣言すると `{"AWS": ["*"]}`・`["s3:GetObject"]` で返す。SeaweedFS は書かれたまま返す。正規形で比べるので、どちらでも 5 周 drift が出ず、ポリシーは書き直されない（Apply が走らない）。
- **SeaweedFS（`weed server`）のボリュームの数**：既定の `-volume.max`（8）では、オブジェクトを書いたバケットごとにボリュームを約 7 個使うので、オブジェクトを入れるバケットが 3 つ目になるとその PUT が HTTP 500 になる。テストは `-volume.max=1000 -master.volumeSizeLimitMB=64` で立てる（小さいボリュームを多く持てるように）。バケットの作成やポリシー・タグなどの設定（この provider が使う API）はボリュームを使わないが、同じ SeaweedFS にオブジェクトを多くのバケットに入れる使い方では、この上限に気をつける。
- **SeaweedFS は起動してから最初のオブジェクトの PUT に 15 秒ほどかかる**ことがある（ボリュームを割り当てる間）。テストは ListBuckets が通るのを待ったあと、`bucket_prefix` の外のバケットにオブジェクトを 1 つ PUT できるまで（最大 2 分）待ってから始める（そのバケットは消す）。
- 4 つの API（バージョニング・ポリシー・ライフサイクル・タグ）の **GET は、今の MinIO・SeaweedFS のどちらも持つ**（NotImplemented にならない）。片方にしか無い GET は今の版には無いので、「宣言したフィールドの GET が NotImplemented なら観測できない」は偽サーバでだけ確かめている（`internal/s3bucket` の単体テスト）。
- **SeaweedFS 3.80 は GET に既定の値を返し（バージョニングは常に Suspended、ポリシー・タグは無い）、PUT だけが 501 NotImplemented**。Read は欠けを知れないので、宣言した値は drift として見え、reconcile は「tags: permanent: the endpoint does not implement this API (NotImplemented) …」で止まる（バケット自体は作られる）。宣言しなければ適合。
- MinIO は `AbortIncompleteMultipartUpload` を持つ lifecycle のルールを `InvalidArgument`（スキーマに合わない）で断る（SeaweedFS は受け付ける）。endpoint が拒む値は、その endpoint の文言つきの PermanentError になる。テストの宣言には使っていない。
- prune：空の orphan は `- DELETE s3-bucket//minio/sk-it-orphan-empty [DESTRUCTIVE]` で消え、中身のある orphan は `prune apply …: permanent: … the bucket is not empty, so it is not deleted (Sakutei never deletes objects; …): … BucketNotEmpty` で残る。prune は毎周その Delete を試みて同じ文で断られる（中身を空にするか、宣言し直すまで）。
- minio を止めると：`prune halted: List returned error: … List minio: … unreachable`、`skip minio/sk-it-app (unobservable …)`、`Reconcile: no changes applied; 1 resource(s) skipped`。seaweedfs のバケットは観測を続けて適合のまま。止まった endpoint への要求は `timeout` まで待つので、その周は遅くなる（この段の 25 秒の大半は時間切れの待ち）。

## 通常のテスト

`make check` は偽の S3 サーバ（`internal/s3bucket/s3test`、httptest。要求ごとに SigV4 の署名・`x-amz-content-sha256`・`Content-MD5` を検証する）で回る。

- SigV4：AWS の公開のテストベクタ（Signature Version 4 の test suite の get-vanilla、S3 の文書の GET Object・PUT Object・GET Bucket Lifecycle・ListObjects の例、署名鍵の導出の例）と同じ値になる（`internal/s3bucket/sigv4_test.go`）。
- 契約テスト（`sdk.ContractTester`、届かないのを absent にしないことを含む）・BulkObserve 等価性テスト（`sdk.BulkEquivalenceTester`）・正規形・NotImplemented・中身のあるバケットの Delete・観測できないもの（届かない・5xx・鍵の誤り・資格を解決できない）・ListBuckets のページ送りと `bucket_prefix`・path-style と virtual-host のアドレス・設定ファイルの検査（`internal/s3bucket/*_test.go`）。
- 観測用と書き込み用のキー（`internal/s3bucket/credential_split_test.go`、`[AC-186-6-*]`）：偽サーバに読むだけのキー（GET・HEAD だけ。ほかは 403 `AccessDenied`）を足し、キーごとの読み・書きの回数を数える。観測は読むだけのキーだけ・書き込みは書き込み用のキーだけで収束すること、書かない回は書き込み用のキーが store に無くても起動して書き込みを `provider.ErrNoWriteAccess` で断ること（書き込み用のキーの要求は 0）、書く回は無ければ読み込みで断ること、設定の誤り、4 つの Get のどれが 403 でも Unknown（absent にしない）こと。
- Object Lock（`internal/s3bucket/objectlock_test.go`、`tests/acceptance/s3bucket_object_lock_test.go` の `[AC-205-*]`）：偽サーバに AWS の形の
  Object Lock の口（PUT の成功は 200 で本文は空、Get は ObjectLockEnabled と Rule/DefaultRetention、設定していなければ 404
  `ObjectLockConfigurationNotFoundError`、Days と Years の両方・どちらも無い・ObjectLockEnabled が Enabled 以外は 400 `MalformedXML`、
  0 以下・受けない mode は 400 `InvalidArgument`、持ち主でないは 403）と、作るときの有効化が無い・versioning の Get が 501・COMPLIANCE
  だけ・Years を Days で返す振る舞い（`Options.NoObjectLockOnCreate`・`ObjectLockModes`・`ObjectLockYearsAsDays`）を持たせ、CLI で
  PutObjectLockConfiguration だけで宣言どおりになる・years の宣言が Days の観測と合う・versioning を宣言しなければエラーにならない・
  GOVERNANCE は理由つきで止まる・短くする・外すは承認が無ければ流れないことを確かめる。
- 受け入れテスト（`tests/acceptance/s3bucket_test.go`、`[AC-122-*]`）：DefaultRegistry から作って契約テスト、PruneWithinScope の prune で空の orphan だけが消えること、届かないと prune が止まり何もしないこと。
