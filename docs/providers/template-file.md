# 雛形からファイルを書く provider（template-file）

利用者の雛形（Go の標準ライブラリの `text/template`）に対象ごとの値を入れたファイルを書き出し、宣言どおりに保つ provider（Backlog #139）。
使い道は **OS の自動導入の応答ファイル**（kickstart・autoinstall / cloud-init の user-data・preseed・Alpine の回答など）：ホスト名・アドレス・Sakutei の ssh の公開鍵・導入の目印を宣言から作る。
決まりの正は設計書 §29.1「OS の導入の手順と応答ファイル」（v3.24）。ここは使い方の説明で、矛盾したら spec/ が優先する。

書き出したファイルを ISO や HTTP の置き場に入れる（インストーラに届ける）のは**利用者の側**。書き出す先のディレクトリを HTTP で配るのがいちばん簡単（[試用](../trial/redfish-os-install/README.md) は `python3 -m http.server`）。

## 設定（--provider-config）

```yaml
providers:
  template-file:
    template_dir: templates   # 雛形の置き場（必須）
    output_dir: answers       # 書き出す先（必須）
```

相対パスは sakutei を動かすディレクトリから。

## intent

```yaml
resources:
  - provider: template-file
    id: 6f1b7c52-0e1a-4d2b-9c11-000000008501   # output_dir からの相対パス（ここでは台の UUID）
    fields:
      template: alpine-answers.sh.tmpl        # template_dir からの相対パス（必須）
      hostname: node1
      address: 10.0.2.15/24
      ssh_authorized_keys: ssh-ed25519 AAAA… sakutei
      install_mark: install-2026-10-27
      var.gateway: 10.0.2.2                   # ほかの値は var.<名前>（英数字と _）
```

| フィールド | 雛形での名前 | 意味 |
| --- | --- | --- |
| `template` | — | 雛形のファイル（必須）。 |
| `hostname` | `.Hostname` | ホスト名。 |
| `address` | `.Address` | アドレス（書き方は雛形しだい。例 `10.0.2.15/24`）。 |
| `ssh_authorized_keys` | `.SSHAuthorizedKeys` | Sakutei が入る ssh の**公開鍵**（複数なら改行で区切る。`lines` で 1 行ずつ）。 |
| `install_mark` | `.InstallMark` | 導入の目印（[guest-os](guest-os.md) の目印で確かめる値。導入ごとに変える）。 |
| `var.<名前>` | `.Vars.<名前>` | そのほかの値。無いかもしれない値は `index .Vars "<名前>"`（無ければ空）。 |
| `path` | — | 書くなら ID と同じ値（identity）。省いてよい。 |
| — | `.ID` | リソースの ID。 |

雛形で使える関数: `shquote`（sh の単一引用符で囲む）・`json`（JSON の文字列にする。YAML の値にも使える）・`lines`（改行で分けて空行を落とす）。
宣言に無い値を `.Vars.<名前>` で使うと描けない（`missingkey=error`、読み込みの時点でエラー）。

雛形の例（試用・統合テストで使う Alpine の回答、[templates/alpine-answers.sh.tmpl](../trial/redfish-os-install/templates/alpine-answers.sh.tmpl)）:

```
HOSTNAME={{shquote .Hostname}}
ADDRESS={{shquote .Address}}
GATEWAY={{shquote (index .Vars "gateway")}}
MARK={{shquote .InstallMark}}
SSH_AUTHORIZED_KEYS={{shquote .SSHAuthorizedKeys}}
FAIL={{shquote (index .Vars "fail")}}
```

cloud-init の user-data なら、たとえば:

```
#cloud-config
hostname: {{.Hostname}}
ssh_authorized_keys:
{{- range lines .SSHAuthorizedKeys}}
  - {{json .}}
{{- end}}
write_files:
  - path: /etc/sakutei-installed
    content: {{json .InstallMark}}
```

## 決まり

- **最小階級**（§5.6）：書き出したファイルから宣言の値は読み戻せないので、観測は「今の宣言で描いた中身とファイルの中身が同じか」だけ（`provider.CheckReader`）。
  違う・無ければ書き直す。手で直されたファイルも、雛形を直したときも、次の周に宣言どおりに書き直す（`status` には `the file differs from the rendered template (sha256 …)` と出る）。
- **秘密は書き出さない**：値に SecretRef（`secretref:…`）や秘密鍵（`PRIVATE KEY`）を書くと読み込みの時点で断る（何も書かない）。応答ファイルは HTTP などで配られる前提なので、入れる値は公開のものだけにする。
  入れる OS のホスト鍵（秘密鍵）が要るなら、応答ファイルでなく ISO など別の経路で渡す。
- ID と `template` は置き場の中の相対パスだけ（絶対パス・`..` は断る）。
- 書き込みは同じディレクトリの一時ファイルに書いてから rename（読み手が半端な中身を見ない）。親のディレクトリは作る。ファイルは 0644。
- List を持たず、消さない（prune しない）。要らなくなったファイルは人が消す。

## 確かめ方

- 単体: `internal/templatefile`（試用の雛形で書き出し・適合・手で直したら書き直し・秘密や `..` の拒否）。
- 受け入れ: `tests/acceptance/template_file_test.go`（`[AC-139-8]`・`[AC-139-9]`、本物の CLI と `--provider-config`）。
- 統合: `make integration-guestos`（[guest-os](guest-os.md) の「統合テスト」）。3 台の応答ファイルを宣言から書き出し、1 つの ISO のインストーラが DMI の UUID で取りに来て、その値（ホスト名・静的なアドレス・公開鍵・目印）で入る。
