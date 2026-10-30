# CHANGELOG v3.73 → v3.74

**改訂日: 2026-10-30**

s3-bucket に Object Lock（作るときの有効化と、バケットの既定の保持の mode・期間）を足した改訂である（Backlog #205、親のストーリー
「使って分かった足りない点」の 3 つめ）。書き換えを禁じたい版（監査の記録・バックアップ）を置くバケットで、Object Lock を宣言で
持ちたいという外の利用者の声から。

## #205：s3-bucket の Object Lock（§29.1）

- **背景**: S3 互換の実装には、作るときの有効化（`x-amz-bucket-object-lock-enabled`）を持たないもの、versioning の Get を持たない
  もの（501）、mode の片方（例：COMPLIANCE）しか受けないもの、Years で掛けた保持を Days で返すものがある。保持の設定は、延ばす
  向きは戻せず、短くする・外す向きは実装に断られずに通って、これから書く版の守りが弱まる。
- **決定**（§29.1「s3-bucket の Object Lock」）:
  - 欄 `object_lock`（JSON のオブジェクト）。`enable_on_create`（作るときに有効化のヘッダを送る。書いたときだけ）・`mode`
    （GOVERNANCE・COMPLIANCE）・`days` か `years`。宣言すれば Object Lock を有効にし、`""` は「Object Lock が無い」。mode と期間を
    書かなければ既定の保持は無い。書いたときだけ比べて合わせる。
  - endpoint の設定の `fields` に `object_lock` を書いたときだけ扱う（既定の fields には入れない。GetObjectLockConfiguration を持た
    ないことを 501 以外で返す実装や、その許可の無い観測用の資格で、宣言していないバケットまで観測できなくならないように）。
  - 既定の保持は PutObjectLockConfiguration だけで掛ける。versioning を読んで有効かを確かめる手順は踏まない。Apply は Get で今の
    設定を読み、同じなら PUT を送らず、違えば PUT して読み戻す。
  - 期間の正規形は Days（years は 365 倍）。Get の 404 `ObjectLockConfigurationNotFoundError` は「設定が無い」、Rule の無い応答は
    「既定の保持が無い」。
  - 実装が受けない設定（400 `InvalidArgument`・`MalformedXML`、既にあるバケットに有効にできない 409 `InvalidBucketState`）と、API を
    持たない実装（501・NotImplemented）は理由つきで止める（PermanentError・計画のエラー）。
  - **既にあるバケットの保持の設定を変える Update は、向きによらず全部、予算に数え、承認ゲートを通す**（`provider.ApprovalRequirer`。
    planned_change のレーン kind `update`）。有効にする・mode を変える・延ばす・短くする・外すのどれも。宣言を既定で断る形にはせず、
    人の承認で止める。新しく作るバケットに初めて掛けるのはふつうの作成。
  - 既にあるバケットと食い違う宣言（有効なバケットに `""`、Object Lock の無いバケットに `enable_on_create: true`）は計画の時点で
    理由つきで止め、作り直さない。
  - 観測用の資格に `s3:GetBucketObjectLockConfiguration`、書き込み用に `s3:PutBucketObjectLockConfiguration`。
- **検証**: 偽の S3 サーバ（`internal/s3bucket/s3test`）に AWS の形の Object Lock の口（PUT の成功は 200 で本文は空、Get は
  ObjectLockEnabled と Rule/DefaultRetention、設定していなければ 404、断るのは 400 MalformedXML・400 InvalidArgument・403）と、
  作るときの有効化が無い・versioning の Get が 501・COMPLIANCE だけ・Years を Days で返す振る舞いを足した。受け入れテスト
  `[AC-205-1]`〜`[AC-205-3]`（PutObjectLockConfiguration だけで宣言どおりになる、years の宣言が Days の観測と合う、versioning を
  宣言しなければエラーにならない、GOVERNANCE は理由つきで止まる、短くする・外すは承認が無ければ流れない）と単体テスト。
  `make integration-s3` に `TestS3ObjectLock`（SeaweedFS 4.51 と MinIO で作る・観測する・承認つきで変える、既にあるバケットに有効に
  すると 409 で理由つきで止まる、MinIO の観測用の利用者の Get が通り Put が断られる）を足した。
- **見送り**: オブジェクトごとの保持（PutObjectRetention）と Legal Hold（オブジェクトは扱わない）。Object Lock の有効なバケットの
  prune の特別扱い（中身のあるバケットは今までどおり消えない）。
