# Sakutei の新しいコア（v3）

設計は `docs/core/README.md`、もとにした検討は `docs/review/core-usecase-review.md`。ルートの v2 のコードとは別の Go モジュール（`github.com/tjst-t/sakutei/v3`、標準ライブラリだけ）。互換は持たない。

作る順番は設計書 §8 の 9 段。いまは **段 1**（型・intent とつなぎ方の設定の読み込みと検証・素通しの Expand・偽の provider・`plan`）。

| パッケージ | 中身 |
|---|---|
| `core` | Resource・Range・Schema・Observation（ある・無い・分からない・作業中）と Provider の口 |
| `intent` | intent のディレクトリ（`*.yaml`・`*.yml`・`*.json`）を読み、`resources:`・`ranges:` を Resource・Range にする |
| `config` | つなぎ方の設定（`providers.<名前>.module`・`settings`）。範囲・除外・方針は書けない（不変条件 24） |
| `expand` | Expand の口と素通しの `Passthrough` |
| `load` | 設定と intent を読み、Expand の出力を検証する（不変条件 25） |
| `plan` | 観測して宣言と比べる。受け取るのは `core.Observer` だけで、何も書かない |
| `sdk/fake` | テスト用の偽の provider（Go から観測の答えを置く・CLI ではモジュール `fake` の state のファイル） |
| `cli`・`cmd/sakutei` | CLI。サブコマンドは `plan` だけ |
| `internal/yaml` | 行番号を残す YAML の部分集合のパーサ（ブロックと 1 行のフロー） |

```console
$ cd v3 && go run ./cmd/sakutei plan --intent <dir> --config <file> [--record <dir>] [--json]
```

終了コード：0 計算できた／1 実行の誤り／2 読み込みで断った。

テストはルートの `make check`・`make test-race` が一緒に回す。
