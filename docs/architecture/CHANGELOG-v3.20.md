# CHANGELOG v3.19 → v3.20

**改訂日: 2026-10-27**

外部の利用者が Sakutei を自分の環境で評価・運用しやすくするための、
汎用の機能を足した改訂である（Backlog ストーリー #114、子 #115〜#119）。設計の向きは変えず、v1 の CLI と provider の
組み込み方の決まりを書き足した。

## #119：外部 provider の組み込み方（§28.9 を新設）

**問題**：provider は本体のバイナリに組み込む作りで、`pkg/registry.DefaultRegistry` が `internal/` の provider を直に登録し、
`cmd/sakutei` は package main だった。本体のリポジトリの外で provider を書くには fork するしか無かった。

**決定**：プロセス境界や動的ロードは持たず（§3 原則 4・5）、**Sakutei を Go のライブラリとして使い、自前の provider を登録した
自前のバイナリを作る**。

- CLI の中身を `pkg/cli` に移し、`cmd/sakutei` は `cli.Main()` を呼ぶだけにした。外のモジュールは
  `cli.WithProviders` / `cli.WithRegistry` で provider を足す。verb・フラグ・終了コードは本体と同じ。
- 組み込みと同じ名前の登録は起動時のエラー（上書きしない）。
- `--provider-config <file>`（JSON / YAML、`{"providers": {"<name>": {...}}}`）で provider ごとの設定を渡す。共通のフラグから
  作る設定に重ね、同じキーはファイルが勝つ。登録されていない名前はエラー。
- 外の provider が `internal/` に頼らず書けるよう、SecretRef の読み取り（`core.ParseSecretRef`）と file SecretBackend の
  解決（`sdk.NewFileSecretResolver`）を `pkg/` に出した。
- 動く例は `examples/external-provider/`。受け入れテスト `[AC-114-5-*]`。

## #115：一回性の `status`・`plan`・`reconcile` にも `--inventory-file`（§9.2・§26.6、Backlog #109 の 3）

デーモンだけが inventory で desired を合成していたため、inventory 由来のリソースが `status` で管理外に見えていた。
3 つの verb に `--inventory-file`・`--inventory-min-ratio`・`--inventory-expected-min` を足し、デーモンと同じ合成をする。
一回性のコマンドは前の周の件数を持たないので、急減の判定は `--inventory-expected-min` だけが効く。受け入れテスト `[AC-114-1-*]`。

## #117：`status` は観測エラーで止めない（§26.4、Backlog #109 の 5）

1 つのリソースの観測エラーで終了コード 1 になり、ほかの適合を出さなかった。観測エラーはそのリソースの「観測できない」
（unknown ≠ absent、§3・§5.4）として数え、全部を最後まで報告する。終了コードは 0＝すべて観測できた（drift があっても 0）／
3＝報告はできたが観測できないものがある／1＝報告そのものができない。受け入れテスト `[AC-114-3-*]`。

## #118：`status --json`（§26.4）

外部の道具が status の結果を機械で集められるよう、`status --json` で JSON を 1 つ出す（schema_version 1。形は `docs/status-json.md`）。
文の出力と同じ情報源から作り、終了コードも同じ。受け入れテスト `[AC-114-4-*]`。

## #116：採番を人が CLI で解放する（§19.3・§26.6、Backlog #108 の 2）

v1 の解放の手段は `POST /v1/release` か JSON の手直しだけだった。`sakutei allocator release`（`--store` のファイル版、
または `--url` のサービス）と `sakutei allocator list` を足した。cooldown（§19.5）はそのまま効く。`--audit-log` で S4 に残せる。
§19.5 の「下流の削除を List で確かめてから」自動で解放する経路は引き続き作っていない。受け入れテスト `[AC-114-2-*]`。

## 変更箇所

`docs/architecture/spec/part3-convergence.md`（§9.2）、`part6-satellites.md`（§19.3）、`part7-operations.md`（§26.4・§26.6）、
`part8-implementation.md`（§28.9 新設）、`README.md`（版・目次）、`docs/status-json.md`（新規）、`examples/external-provider/`（新規）。
