# 秘密の取り出し先（secret_backends：Vault KV v2・外のコマンド）

SecretRef（`secretref:<backend>/<path>`）の値と世代を取り出す先。決まりの正は設計書 §20.2・§20.3（v3.60、Backlog #188）で、ここは
使い方の説明。矛盾したら spec/ が優先する。

- `file`：provider の設定の `secret_store`（file SecretBackend、段0-minimal）。世代は store の `gen`、intent に `#gen<N>` で書く。
- `--provider-config` の `secret_backends:` に書いた取り出し先：intent は `#gen` を書かず、**render フェーズ1が世代を読んで ref に入れる**。
  版が上がれば差分（cause E）が出て Apply される。

## 設定

`--provider-config`（`plan`・`reconcile`・`status`・常駐の `sakutei` が受け取る）に `secret_backends:` を書く。書くのはつなぎ方だけ。

```yaml
providers:
  kvfile: {path: records.json}
secret_backends:
  vault:                       # ref の <backend>（小文字・数字・- と _。file は予約）
    type: vault-kv2
    address: https://vault.example:8200
    ca_file: /etc/sakutei/vault-ca.pem
    approle_role_id_file: /etc/sakutei/vault-role-id
    approle_secret_id_file: /etc/sakutei/vault-secret-id
  pass:
    type: command
    command: /usr/local/bin/sakutei-secret
    timeout: 5s
```

intent では：

```yaml
resources:
  - provider: kvfile
    id: app
    fields: {name: app, value: v, token: "secretref:vault/app/db#password"}
```

render のあと、Diff と Apply が見るのは `secretref:vault/app/db#password#gen3`（KV の版 3）。`#gen<N>` を自分で書いた ref は
ピン留めで、世代を読まない。

### vault-kv2（Vault・OpenBao の KV v2）

| キー | 意味 |
| --- | --- |
| `address` | 必須。`https://host:port`。平文の `http://` は loopback の宛先か `allow_plain_http: true` のときだけ |
| `mount` | KV v2 の mount（既定 `secret`） |
| `namespace` | `X-Vault-Namespace`（任意） |
| `ca_file` | サーバの証明書の CA（PEM、任意） |
| `token_file` | token のファイル（要求ごとに読み直す。Vault Agent などが回せる） |
| `approle_role_id_file`・`approle_secret_id_file` | AppRole のログイン（`token_file` とどちらか一方） |
| `approle_mount` | AppRole の mount（既定 `approle`） |
| `timeout` | 要求ごと（既定 10s） |

- ref の path は `<秘密のパス>#<キー>`。`#<キー>` が無ければキーは `value`。`gen` で始まるキーは `#gen<N>` と紛れるので書けない。
- 世代は KV の版（`/v1/<mount>/metadata/<path>` の `current_version`。値は読まない）。現在の版が消されて・壊されていれば読めないとする。
  値は Apply の直前に `/v1/<mount>/data/<path>?version=<世代>` で読む。
- 要る権限（読むだけで足りる）：

  ```hcl
  path "secret/data/app/*"     { capabilities = ["read"] }
  path "secret/metadata/app/*" { capabilities = ["read"] }
  ```

- AppRole の client token は lease の 2/3 まで持ち、403 が返れば一度だけログインし直す。

### command（外のコマンド）

| キー | 意味 |
| --- | --- |
| `command` | 必須。実行ファイル（PATH から探す） |
| `timeout` | 1 回の呼び出し（既定 10s） |

```
<command> generation <path>          → 世代（正の整数）を標準出力に
<command> value <path> <世代>         → 値を標準出力に（末尾の改行 1 つは落とす）
```

0 でない終了は誤り。標準出力は誤りの文に出さない。標準エラーは `generation` のときだけ 1 行に縮めて出す。

### 取り出し先の型を足す

自前のバイナリ（[examples/external-provider](../../examples/external-provider/)）で `pkg/cli.WithSecretBackend("<type>", factory)` を渡す。
factory は `secret_backends.<名前>` の `type` 以外の設定（文字列）を受け、`pkg/sdk.SecretBackend`（`Generation`・`Resolve`）を返す。
値を誤りの文に入れてはならない。

## provider から値を取り出す

provider は Apply の直前に `core.ParseSecretRef` → `SecretRef.Resolve(ctx, resolver)` で値を取り出し、使ったら `Zero()` する。
resolver は `pkg/sdk.NewFileSecretResolver(secret_store)`（組み込みの provider は file SecretBackend）で、ref が `secret_backends:` の名前なら
その取り出し先へ渡る（`secret_store` は無くてよい）。組み込みの provider の多くは、資格・宣言の SecretRef を今は `file` に限っている
（provider ごとの文書を見る）。

## 届かないとき（unknown）

世代が読めない（宛先に届かない・sealed・秘密が無い・コマンドが失敗する）ref を持つリソースは、その周 **unknown**：Observe も
Apply もせず、下流は待ち、prune しない。ほかのリソースは収束を続ける。

```
sakutei reconcile: kvfile//app: secret generation of secretref:vault/app/db#password in field "token" could not be read: vault-kv2: metadata of app/db: HTTP 503 (sealed or in standby): Vault is sealed (unknown: not Observed or Applied until it can, §20.3)
```

`plan` は計画しない（JSON の `unobservable` に挙げる）、`reconcile` は終了コード 1、`status` は観測できないものに数える。

## 統合テスト

`make integration-vault`（docker と sudo（パスワードなし）が要る。check・CI には入れない）。OpenBao `2.4.1`（tag と digest で固定、
`SAKUTEI_IT_OPENBAO_IMAGE` で差し替え）を dev モードで立て、読むだけの policy の token のファイルと AppRole で、examples の kvfile を
`plan`・`reconcile`・`status`・常駐で回す。

| 試験 | 確かめること |
| --- | --- |
| IT-188-1 | token のファイル：KV の版が世代として ref に入り、その版の値で Apply される |
| IT-188-2 | 版が上がると plan に cause E の差分、reconcile が新しい版で Apply |
| IT-188-3 | AppRole で同じこと |
| IT-188-4 | 常駐が版の上がりを次の周で Apply |
| IT-188-5 | 閉じたポート・sealed：そのリソースは unknown（plan は計画しない・reconcile は 1・status は観測できない）、ほかは収束 |
| IT-188-6 | 値はどの出力にも記録のファイルにも出ない |

2026-10-28 に OpenBao 2.4.1 で 4 つの試験（上の 6 項目）がすべて PASS。
