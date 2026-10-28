# CHANGELOG v3.57 → v3.58

**改訂日: 2026-10-28**

エンジンが現実に到達する資格を観測用と書き込み用に分ける改訂の後半である（Backlog #186。親のストーリー「M7：本番に向けた固め」の
「鍵を役目ごとに分ける」）。前半（v3.56、#185）の共通の口（`provider.WithWrite`・`provider.Writing`・設定の `access`）を、管理 API の
provider に当てた。

## #186：管理 API の provider の資格を観測用と書き込み用に分ける（§20.7）

- **背景**: proxmox-vm・redfish・ldap・postgresql・s3-bucket は、対象（クラスタ・BMC・サーバ・endpoint）ごとに 1 本の資格で観測も
  書き込みも行っていた。`plan`・`status`・常駐の毎周の観測に、VM を消せるトークン・BMC の管理者・ディレクトリの管理者・DB の
  管理ロール・バケットを消せる鍵が常に乗っていた。書かない回でもその資格が store に要った。
- **決定**:
  - 対象の設定に `observe:`・`write:` を書ける。どちらもその provider の資格のキーだけを持ち、書かないキーは対象の上の値を受け継ぐ
    （どちらも書かなければ今までどおり 1 本）。資格のキーは proxmox-vm が `token_id`・`token_secret`、redfish が `username`・`password`、
    ldap が `bind_dn`・`bind_password`、postgresql が `user`・`password`、s3-bucket が `access_key`・`secret_key`。読み方と、要求ごとの
    資格の選び方は共通の部品（`internal/apicred`：`Read`・`Pair.For`）にまとめた。
  - 要求・コマンドごとに `provider.Writing(ctx)` で資格を選ぶ。観測（`Read`・`List`・`BulkObserve`、Apply のあとの効いたかの読み直し）は
    観測用だけ、書き込み（`Apply`・`Action`。Apply の中の読み直しを含む）は書き込み用だけ。書かない回は書き込み用の SecretRef を store
    に確かめず、取り出さず、書き込みを `provider.ErrNoWriteAccess` で断る（要求は送らない）。書く回は無ければ読み込みで断り、文言に
    場所（`clusters.<名前>.write.token_secret` など）を出す。
  - **Task の実行も書き込みの文脈で呼ぶ**：単純 Task の `RunTask`・`RollbackTask` と、多相 Task の相の `RunStep`・巻き戻し（redfish-firmware
    の更新、sqlmigrate の適用）。相が済んだかの `CheckStep` は観測で、観測用の資格で読む。
  - **自分の資格の利用者は管理の外**：ldap の bind DN、postgresql の管理ロールは、観測用・書き込み用の両方を List に出さず（prune
    しない）、ID として宣言すれば断る。
  - **読むだけの資格で読めない項目は unknown**（§3 第3原則）：s3-bucket の Get の 403、postgresql の `verify_passwords`（`pg_authid`
    を読めない観測用のロール）は、無い・違うと読まず観測できないとする。LDAP の `userPassword` は、読めないのと無いのを
    サーバの応答で見分けられないので、宣言するなら観測用の DN にその属性の読みを許す（文書に書いた）。
  - 観測用の資格に要る最小の権限を各 provider の文書に書き、統合試験で確かめた：Proxmox は `/` に `PVEAuditor`（Proxmox VE 9.2）、
    LDAP は管理する木の read・search（389 Directory Server の aci）、PostgreSQL は `LOGIN` だけ（観測の catalog は PUBLIC で読める。
    PostgreSQL 17）、S3 は `s3:ListAllMyBuckets` と 4 つの `s3:Get*`（MinIO）。Redfish は `ReadOnly` の役割（Login）。
  - metrics-probe は観測だけの provider で、資格はいつも観測用（`observe:`・`write:` は書けない）。
- **確かめ方**: 単体・受け入れ（`tests/acceptance/api_credential_split_test.go`、偽の Proxmox で CLI を回す）と、各 provider の統合試験
  （`make integration-proxmox`・`integration-ldap`・`integration-postgresql`・`integration-s3`・`integration-redfish`、すべて PASS）。
  qemu-bmc 0.11.0 は AccountService も役割も持たない（IPMI で足した利用者も書ける）ので、redfish の統合試験は BMC の前に置いた
  試験の中の HTTPS の中継で ReadOnly の役割を真似て確かめた（観測用の利用者の GET 以外を 403 にする）。
- **見送り**:
  - 観測用の資格の権限が足りるかを読み込みの時点で確かめること（対象に問い合わせないと分からない。足りなければ観測が unknown や
    認証のエラーとして出る）。
  - 他人の Task を見せない BMC での redfish-firmware の分けた資格（相の確かめが観測できず止まる。文書で、そういう BMC では分けない
    ように書いた）。

## 影響

- §20.7 の「見送り」にあった管理 API の provider の資格の分けを決まりに書き換えた。§29.1 に 1 行を足した。
- 各 provider の設定に `observe:`・`write:` を足した。今の書き方はそのまま動く。
- 振る舞いの変化：redfish-firmware・sqlmigrate の Task の実行は書き込み用の資格を使う（分けない設定では同じ資格）。sqlmigrate の
  `engine: postgresql` の版の観測は観測用のロールで読む（版の表の SELECT が要る）。
