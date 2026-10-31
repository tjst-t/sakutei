# Sakutei の新しいコア（v3）

設計は `docs/core/README.md`、もとにした検討は `docs/review/core-usecase-review.md`。ルートの v2 のコードとは別の Go モジュール（`github.com/tjst-t/sakutei/v3`、標準ライブラリだけ）。互換は持たない。

作る順番は設計書 §8 の 9 段。いまは **段 2** まで：段 1（型・intent とつなぎ方の設定の読み込みと検証・素通しの Expand・偽の provider・`plan`）に、段 2（1 周の処理 `cycle.Run`：Apply・Delete・List・prune の守り、Policy の mode と予算、Record のロック・前の周の目印・監査の鎖、`reconcile --once`、終了コード）を足した。

| パッケージ | 中身 |
|---|---|
| `core` | Resource・Range・Policy・Schema・Observation（ある・無い・分からない・作業中）と Provider の口（観測と列挙だけの `Reader`） |
| `intent` | intent のディレクトリ（`*.yaml`・`*.yml`・`*.json`）を読み、`resources:`・`ranges:`・`policies:` を Resource・Range・Policy にする |
| `config` | つなぎ方の設定（`providers.<名前>.module`・`settings`）。範囲・除外・方針は書けない（不変条件 24） |
| `expand` | Expand の口と素通しの `Passthrough` |
| `load` | 設定と intent を読み、Expand の出力を検証する（不変条件 25） |
| `plan` | List と観測をして宣言と比べ、prune の守り・依存の待ち・Policy の mode と予算で操作ごとに当てるかを決める（`Decide`）。受け取るのは `core.Reader` だけで、何も書かない |
| `cycle` | 1 周（`Run`）。Record のロックを取り、`plan.Decide` と同じ判定で当て、読み直し、目印と監査を書く。ロックが取れなければ進めない周 |
| `record` | Record のディレクトリ：`LOCK`（flock）・`format`・`marks.json`（範囲ごとの件数・予算の窓）・`audit/`（前の行のハッシュの鎖） |
| `sdk/fake` | テスト用の偽の provider（Go から観測の答え・List の欠け・Apply の失敗・削除の断りを置く・CLI ではモジュール `fake` の state のファイル） |
| `cli`・`cmd/sakutei` | CLI。サブコマンドは `plan`・`reconcile`・`audit verify` |
| `internal/yaml` | 行番号を残す YAML の部分集合のパーサ（ブロックと 1 行のフロー） |

```console
$ cd v3 && go run ./cmd/sakutei plan      --intent <dir> --config <file> [--record <dir>] [--json]
$ cd v3 && go run ./cmd/sakutei reconcile --intent <dir> --config <file> --record <dir> [--once] [--json]
$ cd v3 && go run ./cmd/sakutei audit verify --record <dir>
```

`plan` の終了コード：0 計算できた／1 実行の誤り／2 読み込みで断った。

`reconcile` の終了コード（E-J14）：0 収束した／1 実行の誤り（当てて失敗した・Record に書けない）／2 読み込みで断った／3 ずれか Unknown が残る（report・待ち・初めての周で消さない宣言外を含む）／4 止まった（予算・List の急減）。

段 2 の決まり：

- 宣言外を消すのは、Range を宣言し、`prune: true` で、List が完全で、List に出て、宣言に無く、外の持ち主でなく、観測が Present で、消せる種類（`DestructiveDelete`・観測だけの種類でない）で、前の周の件数が Record にあり、急減（`max_drop`）していず、予算を通ったときだけ（不変条件 6・7・9）。前の周の件数は List の件数から自分が消した分を差し引いて覚え、急減で止めた周は前の値を保つ。
- Unknown・Converging・答えの無い対象には書かず、宣言した上流が宣言どおりでない下流（宣言に無い上流は Present でない下流）は待つ（不変条件 3）。上流を当てた周も下流は次の周。
- 当たる Policy のうち 1 つでも `mode: report` なら知らせるだけ。予算（`per_cycle`・`destructive_per_cycle`・`destructive_per_window` と `window`）は Policy ごとに数え、1 つでも超えれば周全体を止める（不変条件 20）。窓の使用量は発射の前に Record に書き、書けなければ当てない。断られた削除は数えない。
- 窓・承認・別枠・一時的な Policy・`policy_sets`・常駐は後の段。

テストはルートの `make check`・`make test-race` が一緒に回す。
