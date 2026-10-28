# CHANGELOG v3.59 → v3.60

**改訂日: 2026-10-28**

SecretRef の取り出し先を足す口と、Vault 形（KV v2）・外のコマンドの取り出し先の改訂である（Backlog #188。親のストーリー
「M7：本番に向けた固め」の「秘密の取り出し先」）。

## #188：秘密の取り出し先を足す口と、Vault 形（KV v2）の取り出し先（§20.2・§20.3・§9.4）

- **背景**: SecretRef の取り出し先は file SecretBackend（段0-minimal）だけで、Vault は「オフラインでは未実装」を返す stub だった。
  §20.2 は「世代は render フェーズ1が Vault（KV v2）の metadata から解決して SecretRef に注入する（ピン留めではない）」と書くが、
  実装では世代を intent に `#gen<N>` で手書きし、秘密を回しても使う側に差分が出なかった。
- **決定**:
  - **取り出し先の設定**：`--provider-config` に `secret_backends:` を足す。名前ごとに `type` とつなぎ方（宛先・資格のファイル・
    時間）を書き、SecretRef の `<backend>` がその名前を指す。`file` は provider の `secret_store` の名前として予約（書けば断る）。
    知らない型・知らない設定・形の誤りは起動で断る。`providers:` だけ・`secret_backends:` だけのファイルも書ける。
  - **足す口**：`pkg/sdk.SecretBackend`（`Generation(ctx, path)`：世代＝正の整数、値は読まない／`Resolve(ctx, path, gen)`：
    その世代の値）と `pkg/sdk.SecretBackendFactory`、型を足す `pkg/cli.WithSecretBackend(type, factory)`（組み込みの型と同じ名前・
    二度の登録は起動の誤り）。
  - **`vault-kv2`**：ref の path は `<秘密のパス>#<キー>`（無ければキーは `value`。`gen` で始まるキーは `#gen<N>` と紛れるので
    書けない）。世代は KV の版（metadata の `current_version`。現在の版が消されて・壊されていれば読めない）。Apply の直前に
    その版の値を読む。認証は `token_file`（要求ごとに読み直す）か AppRole（`approle_role_id_file`・`approle_secret_id_file`。
    client token は lease の 2/3 まで持ち、403 で一度だけログインし直す）。`ca_file`・`namespace`・`mount`（既定 `secret`）・
    `timeout`。平文の http は loopback の宛先か `allow_plain_http: true` のときだけ（token と値が網を平文で渡るため）。
    誤りの文には HTTP の状態と Vault の `errors` だけを出し、応答の本文（値）は出さない。
  - **`command`**：`<command> generation <path>` と `<command> value <path> <世代>`。標準出力（値）は誤りの文に出さない。
    標準エラーは `generation` のときだけ 1 行に縮めて出す。`timeout`（既定 10s）。
  - **render フェーズ1の世代の解決**：設定した取り出し先の ref で `#gen` を書かないものの世代を読み（1 回の render で秘密ごとに
    1 度）、ref に入れる。`secret:<backend>/<path>=<世代>` を rendered_revision の外部入力に足す（版が上がれば cause E で Apply）。
    `#gen<N>` を書いた ref はピン留めとして読まない。`file` の ref は今までどおり（世代を入れない）。常駐・`plan`・`reconcile`・
    `status` が同じ関数を使う。
  - **読めなければ unknown**（§20.3）：そのリソースと合成の子はその周 Observe も Apply もせず、下流は待ち、prune しない。
    理由を 1 行で出す。`plan` は計画せず unobservable に、`reconcile` は終了コード 1、`status` は観測できないもの。ほかの
    リソースは収束する。
  - **値の取り出し**：provider は今までどおり file SecretBackend の resolver（外の provider は `pkg/sdk.NewFileSecretResolver`）で
    取り出す。ref が設定した取り出し先の名前ならその取り出し先へ渡す。stub（`VaultSecretBackend`）は消した。
- **確かめ方**: 単体（取り出し先の模擬・render の段）、受け入れ（`tests/acceptance/secret_backend_test.go`、`command` の取り出し先で
  examples の kvfile を回す）、統合（`make integration-vault`：OpenBao 2.4.1 を tag と digest で固定。token のファイル・AppRole・
  常駐・閉じたポート・sealed、すべて PASS）。
- **見送り**:
  - §20.3 の「直近に解決した世代を TTL 内で保持し、断の間もその世代で Apply する」。世代が読めない間は値も取り出せないことが多く、
    保持した世代で差分を消しても Apply できない。unknown を安全側とした。
  - 組み込みの provider（postgresql・ldap・proxmox-vm・s3-bucket など）の設定・宣言の検証が `file` 以外の取り出し先を断っている所の
    緩め。provider ごとに、資格の SecretRef を Vault から取り出すのを試してから広げる。
  - 秘密の存在・鮮度の収束（「このパスに在るべき」を Vault に生成させる、`rotation:`）。§20.1 のとおり別の口で扱う。
