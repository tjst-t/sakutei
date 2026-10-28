# host-file provider

離れたホスト（または手元）のふつうのファイルを宣言どおりに保つ provider（`host-file`、Backlog #155）。中身・許可（mode）・持ち主（owner・group）を合わせる。ssh 越しの実行の層（[remote.md](../remote.md)、#130）の上で動き、ホストに agent は置かない。systemd のユニットと podman の quadlet は [systemd-unit](systemd-unit.md) で扱い、設定ファイルを書き換えたらサービスを入れ替える口もそちらにある。決まりの正は設計書 §29.1「離れたホストのファイルと systemd のユニット」。

- 標準階級（観測は本当の姿）。List・BulkObserve を持たず、prune しない：管理するのは intent に書いたファイルだけ。
- 中身は観測にも Event にも平文で載せない（sha256 の hash だけ）。

## 設定

`--provider-config` で、扱うホストと、つなぎ方（[対象の一覧](../remote.md#対象の一覧)）を渡す。

```yaml
# providers.yaml
providers:
  host-file:
    file_hosts: web1,web2          # 扱うホスト（remote_config の hosts の名前）。local は手元
    remote_config: hosts.yaml      # 対象の一覧（#130）
    # stamp_dir: /var/lib/sakutei  # 省略可。ホストの上の印の置き場（content_secret のときだけ使う）
    # secret_store: secrets.json   # 省略可。content_secret を解決する file SecretBackend（既定は remote_config の secret_store）
```

- 名前 `local` は手元（デーモンの動く機械）。`remote_config` に無くてよい（`file_hosts: local` だけなら `remote_config` も要らない）。
- ファイルの読み書きは `sh`・`stat`・`sha256sum`・`mktemp`・`mv`・`chmod`・`chown`・`chgrp`・`mkdir` で行う（GNU coreutils か busybox）。`sudo: true` のホストでは root で書く。

## 範囲を intent に書く（`providers:`、Backlog #182）

扱うホスト（`file_hosts`）は ID の頭で、どのファイルを管理できるかを決める範囲なので、intent のトップレベルの `providers:` に
書ける（§1.7・§7.7）。`remote_config`・`stamp_dir`・`secret_store` はつなぎ方として provider の設定に残る。

```yaml
# intent/providers.yaml
providers:
  host-file:
    hosts: [web1, web2, local]     # file_hosts と同じ（順は問わない）
```

- intent にだけ書けば、設定に書いたのと同じに動く。設定にだけあれば今までどおり読み、`plan`・`reconcile`・`status`・常駐が
  「is only in the provider config: write it in the intent too (providers.host-file.…)」と警告する（常駐では一度だけ）。
- 両方にあれば設定は安全の網として残り、**食い違えば読み込みで断る**（弱い側に寄せない。常駐は起動で断り、動いているあいだに
  食い違えばその周を飛ばす）。知らないキーも断る。設定から外すのは次のメジャー版。
- どれがつなぎ方でどれが範囲かの一覧は [ranges.md](ranges.md)。

## ID とフィールド

ID は `<ホスト>/<絶対パス>`（例 `web1/etc/app/app.env` はホスト web1 の `/etc/app/app.env`）。パスは正規形（`..`・`//`・末尾の `/` を含まない）で、ホストは `file_hosts` にあるもの。違えば intent を読む時点で断る。

| フィールド | 値 | 観測 |
| --- | --- | --- |
| `path` | 省略可。書くなら ID のパスと同じ | パス |
| `content` | 中身（平文、256 KiB まで） | `sha256:<hex>`（hash で比べる） |
| `content_secret` | 中身の SecretRef（`secretref:file/<path>#genN`） | 書いたときの参照（中身が同じ間だけ） |
| `mode` | 8 進（`"0640"`、`"644"` も可） | 4 桁の 8 進 |
| `owner`・`group` | 名前か数 | 名前（無ければ数）。数で書いても合う |
| `size`・`uid`・`gid` | 書けない（観測だけ） | |

```yaml
resources:
  - provider: host-file
    id: web1/etc/app/app.env
    fields:
      content: |
        APP_PORT=8080
        APP_LOG_LEVEL=info
      mode: "0640"
      owner: root
      group: root
  - provider: host-file
    id: web1/etc/app/db.env
    fields:
      content_secret: secretref:file/app/db-env#gen3   # 値は secret store にだけある
      mode: "0600"
```

- `content` と `content_secret` は同時に書けない。どちらも書かなければ中身は管理しない（無ければ空のファイルを作り、あとは属性だけを合わせる）。
- `mode`・`owner`・`group` を書かなければ、中身を書き換えるときも今のファイルの値を保つ。新しく作るときは 0644（`content_secret` なら 0600）で、持ち主は書き込む利用者（`sudo: true` なら root）。
- 書き込みは同じディレクトリの一時ファイル（`.sakutei-XXXXXX`）に書いてから rename する（読み手が半端な中身を見ない）。親のディレクトリは無ければ作る（0755）。
- シンボリックリンク・ディレクトリなど、ふつうのファイルでないものは扱わない（観測できない、PermanentError）。

## 差分の見え方

- 中身は hash で比べる。plan の差分は `content sha256 1a2b3c4d5e6f → 9f8e7d6c5b4a` の形で、To には宣言の平文が出る（宣言にもともと書いてある値）。ホストの上の中身（手で書き換えた値など）は plan・status・Event のどこにも出ない。
- 宣言できる中身は 256 KiB まで（plan に平文で出るため）。大きなファイルは配る仕組み（イメージ・パッケージ）に任せる。
- `content_secret`：書いたときに「SecretRef の参照（値ではない）と中身の hash」をホストの印 `<stamp_dir>/file-stamps/<パスの hash>` に残す。観測はいまの中身の hash が印と同じときだけその参照を返すので、手で書き換えれば drift、世代（`#genN`）を上げれば drift になって書き直す。値は Sakutei にもホストの印にも残らない。同じ世代のまま secret store の値だけを変えても drift にならない（§20：変化の印は世代）。

## 観測用と書き込み用の資格（Backlog #185）

対象の一覧のホストに観測用（`observe:`）と書き込み用（`write:`）の資格を分けて書ける（[../remote.md の「観測用と書き込み用の資格」](../remote.md#観測用と書き込み用の資格)）。観測は `sh -c` の小さなスクリプト（`stat`・`sha256sum`・印の `cat`）なので、観測用のユーザーは sudo 無しで、管理するファイルを読め、そのディレクトリを辿れる権限で動かす（`sh` を sudo で許すのは root を与えるのと同じ）。

- **読むだけでは観測できないもの**：観測用のユーザーが読めないファイル（0600 の root のファイル、`content_secret` の秘密のファイルなど）。グループか ACL で読みを渡す。覗けないディレクトリの中のファイルは「無い」と読まず観測できない（unknown）とし、何もしない（作り直す計画を出さない）。
- 書き込み用のユーザーは Apply の中だけで使う（ファイルの書き込み・持ち主とモードの変更・印の書き込み・消すこと）。

## 届かないとき

| 起きたこと | 答え |
| --- | --- |
| ホストに届かない・認証の失敗 | 観測できない（unknown、TransientError）。absent にしない |
| ホスト鍵の不一致 | 観測できない（PermanentError） |
| sudo が通らない・ファイルが読めない（`sha256sum` が失敗） | 観測できない（unknown） |
| シンボリックリンク・ディレクトリ | 観測できない（PermanentError） |

観測できないファイルは何もしない（作りも書き換えもしない）。

## 消すこと

prune しない（`Pruneable` にしない）。要らなくなったファイルは intent から外し、人が消す。Delete の Op（prune では来ない）は破壊的で、ファイルと印を消す。

## テスト

`make check` の中：`internal/hostconf`（偽のホスト `remotefake` で契約テスト・中身と mode の drift と復元・SecretRef が plan と観測に出ないこと・届かないと unknown・手元）と `tests/acceptance/hostconf_test.go`（デーモンの経路、`[AC-155-1]`・`[AC-155-5]`）。本物の sshd・systemd を相手にした統合テストは [systemd-unit.md の「統合テスト」](systemd-unit.md#統合テスト)（`make integration-hostconf`）。
