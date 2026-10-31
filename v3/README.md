# Sakutei の新しいコア（v3）

設計は `docs/core/README.md`、もとにした検討は `docs/review/core-usecase-review.md`。ルートの v2 のコードとは別の Go モジュール（`github.com/tjst-t/sakutei/v3`、標準ライブラリだけ）。互換は持たない。

作る順番は設計書 §8 の 9 段。いまは **段 4** まで：段 1（型・intent とつなぎ方の設定の読み込みと検証・素通しの Expand・偽の provider・`plan`）に、段 2（1 周の処理 `cycle.Run`：Apply・Delete・List・prune の守り、Policy の mode と予算、Record のロック・前の周の目印・監査の鎖、`reconcile --once`、終了コード）と、段 3（常駐・起床・読み取り HTTP・Events の状態型と送り出しの列・stdout の送り先）と、段 4（Condition の三値の式と量化・Graph の順序と待ち・同じ周の観測し直し・settle と効かない Apply の回数）を足した。

| パッケージ | 中身 |
|---|---|
| `core` | Resource・Range・Policy・Schema・Observation（ある・無い・分からない・作業中）と Provider の口（観測と列挙だけの `Reader`）・Event と送り先の口 `Sink`・周の設定・Condition の口（三値 `Tri`・`on_unknown`・確かめの `CheckFunc`） |
| `condition` | 観測についての三値の式を読み（`Parse`）、評価し（`Eval`）、Schema と突き合わせる（`Lint`） |
| `graph` | `depends_on` と `when` から宣言の依存（上流・下流・量化の選び方・循環）を作る |
| `intent` | intent のディレクトリ（`*.yaml`・`*.yml`・`*.json`）を読み、`resources:`・`ranges:`・`policies:`・`cycle:` を Resource・Range・Policy・周の設定にする |
| `config` | つなぎ方の設定（`providers.<名前>.module`・`settings`、`sinks.<名前>.module`・`settings`、`listen.read`）。範囲・除外・方針は書けない（不変条件 24） |
| `expand` | Expand の口と素通しの `Passthrough` |
| `load` | 設定と intent を読み、Expand の出力を検証する（不変条件 25） |
| `plan` | List と観測をして宣言と比べ、prune の守り・依存の待ち（Condition）・Policy の mode と予算・効かない回数で操作ごとに当てるかを決める（`Decide`）。受け取るのは `core.Reader` だけで、何も書かない |
| `cycle` | 1 周（`Run`）。Record のロックを取り、`plan.Decide` と同じ判定で波ごとに当て、settle を待って読み直し、上流が効いたら下流を同じ周に観測し直して当て、効かない回数・目印・監査を書き、通知の状態を進めて送り出し、健全性を書く。ロックが取れなければ進めない周 |
| `events` | 周の結果から状態型・出来事型の Event を作る（`Derive`）・送り先へ送り出す（`Deliver`）・stdout の送り先 |
| `daemon` | 常駐：`cycle.Run` を繰り返し、間隔を待ち、`sakutei wake`・SIGHUP・intent の変更で起きる |
| `readapi` | 常駐の読み取り HTTP（`/status`・`/plan`・`/events`（SSE）・`/graph`・`/health`）と TLS・クライアント証明書・読むだけのトークン |
| `record` | Record のディレクトリ：`LOCK`（flock）・`format`・`marks.json`（範囲ごとの件数・予算の窓・効かない Apply の回数）・`audit/`（前の行のハッシュの鎖）・`events/`（`state.json`・`outbox/`・`cursors/`）・`health.json`・`wake` |
| `sdk/fake` | テスト用の偽の provider（Go から観測の答え・List の欠け・Apply の失敗・効かない Apply・上流の奥で届かない対象・削除の断りを置く・CLI ではモジュール `fake` の state のファイル） |
| `cli`・`cmd/sakutei` | CLI。`sakutei`（常駐）とサブコマンド `plan`・`reconcile`・`wake`・`audit verify` |
| `internal/yaml` | 行番号を残す YAML の部分集合のパーサ（ブロックと 1 行のフロー） |

```console
$ cd v3 && go run ./cmd/sakutei           --intent <dir> --config <file> --record <dir> [--json]
$ cd v3 && go run ./cmd/sakutei wake      --record <dir>
$ cd v3 && go run ./cmd/sakutei plan      --intent <dir> --config <file> [--record <dir>] [--json]
$ cd v3 && go run ./cmd/sakutei reconcile --intent <dir> --config <file> --record <dir> [--once] [--json]
$ cd v3 && go run ./cmd/sakutei audit verify --record <dir>
```

`plan` の終了コード：0 計算できた／1 実行の誤り／2 読み込みで断った。

`reconcile` の終了コード（E-J14）：0 収束した／1 実行の誤り（当てて失敗した・Record に書けない）／2 読み込みで断った／3 ずれか Unknown が残る（report・待ち・初めての周で消さない宣言外を含む）／4 止まった（予算・List の急減・効かない Apply の上限・`on_unknown: block` の条件が分からない）。

段 2 の決まり：

- 宣言外を消すのは、Range を宣言し、`prune: true` で、List が完全で、List に出て、宣言に無く、外の持ち主でなく、観測が Present で、消せる種類（`DestructiveDelete`・観測だけの種類でない）で、前の周の件数が Record にあり、急減（`max_drop`）していず、予算を通ったときだけ（不変条件 6・7・9）。前の周の件数は List の件数から自分が消した分を差し引いて覚え、急減で止めた周は前の値を保つ。
- Unknown・Converging・答えの無い対象には書かず、宣言した上流が宣言どおりでない下流（宣言に無い上流は Present でない下流）は待つ（不変条件 3）。上流を当てた周の下流は、段 4 で同じ周に観測し直して当てるようにした。
- 当たる Policy のうち 1 つでも `mode: report` なら知らせるだけ。予算（`per_cycle`・`destructive_per_cycle`・`destructive_per_window` と `window`）は Policy ごとに数え、1 つでも超えれば周全体を止める（不変条件 20）。窓の使用量は発射の前に Record に書き、書けなければ当てない。断られた削除は数えない。
- 窓・承認・別枠・一時的な Policy・`policy_sets` は後の段。

段 3 の決まり：

- 常駐（`sakutei --intent … --config … --record …`、`reconcile --once=false` も同じ）は、周ごとに intent と設定を読み直して `cycle.Run` を回し、`cycle.interval`（intent、既定 60s）を待つ。`sakutei wake`（Record の `wake` に時刻を書く）・SIGHUP・intent のディレクトリの変更で待ちを切り上げる（E-N5）。周をまたいで覚えるものは全部 Record で、常駐は何も持たない。だから一回実行の列と常駐は同じ Record から同じ結果・同じ Event を出す（不変条件 30。`daemon` のテストで、同じ現実の並びに対して周ごとの結果・Record の全ファイル・送り先に届いた Event が一致することを確かめる）。初めの読み込みで断れば終了コード 2。その後の周の読み込みの誤りは健全性に残して回り続ける。ログは stderr（`--json` なら周ごとに 1 行の JSON）。
- 状態型の Event（`Drift`・`Unknown`・`Converging`・`Unmanaged`・`Waiting`・`Halt`・`DeleteRefused`）はキー（種類＋対象）ごとに firing になった周と resolved になった周にだけ送り出しの列に積み、続いた周は `count` を数え、`cycle.stuck_after` の周数で 1 回だけ `stuck` を積む（E-H1）。対象が Unknown・作業中の周、範囲の List が不完全な周には resolved にせず、`count` も進めない（不変条件 29）。出来事型は `Applied`・`Deleted`・`CycleDone`（件数と周の時間。数えたものだけ）。
- 送り出しの列（`events/outbox/`）は送り先ごとの位置（`events/cursors/`）から送る。送れない・時間内に戻らない・panic した送り先は位置を保って次の周に同じ位置から送り直し、周の誤りにしない（不変条件 28）。全部の送り先が送ったファイルは消す。送り先は `sinks:` に書き、いまあるモジュールは `stdout`（1 行 1 件の JSON。一回実行では結果の前に出る）。進めない周（ロックがほか）は積まず送らない。
- 読み取り HTTP（`listen.read`）は GET だけ。`/status` は最後の周の結果（`reconcile --json` と同じ構造）、`/plan` は何も消費しない plan（同時に 1 つ、2 つ目は 503）、`/events` は送り出しの列の SSE（`?since=`・`Last-Event-ID`）、`/graph` は依存、`/health` は健全性（続いた誤りがあれば 503、E-K11）。loopback でない宛先の平文は `plaintext: true` を書いたときだけ。TLS・クライアント証明書（`tls.client_ca`）・読むだけのトークン（`auth.tokens_file`、Bearer）を書ける（N-L4）。
- `status`・`freeze`・承認・Procedure は後の段。

段 4 の決まり：

- `depends_on` の 1 件は `id`（名指しの上流）と `when`（Condition）の少なくとも一方を書く。`on_unknown`（`wait` 既定・`block`・`ignore`）は `when` と一緒に書く。`when` が真になるまで下流は操作しない。偽は待つ（Event `Waiting`、終了コード 3）。不明は `wait` なら待ち、`block` なら止めて知らせる（Event `Halt`、終了コード 4。E-C4 の容量の門）、`ignore` なら真とみなす。どの場合も、名指しの上流が Unknown・作業中なら待つ（不変条件 3）。`when` が無ければ段 2 と同じ（宣言した上流は宣言どおり、宣言に無い上流は Present）。
- 式（§3.4）：`observe(<type>/<id>).項目`・`present(...)`・`absent(...)`・`desired.項目`（この宣言の項目）・`it.項目`・`check(名前)`・`all`・`any`・`count`（選び方は `<type>[ラベル=値, ...]` で、**宣言した** Resource から選ぶ）・比較（`== != < <= > >= in contains`）・`and or not`・定数（文字列・数・`true`・`false`・`[...]`）。Unknown・作業中・観測していない対象への参照は不明。Absent の対象と観測に無い項目は「無い」で、`==` は偽・`!=` は真・大小と `in`・`contains` は偽。大小は数どうしと長さ（`"720h"`）どうしだけで、比べられない組は不明。`count(...) op N` は真の件数 k〜k+不明の件数のどれでも同じ結果のときだけ真偽。式の形の誤り、知らない provider・種類・項目、量化の外の `it`、登録していない `check` は読み込みで断る。`check` の確かめは `cli.WithCheck`（Go）で組み込み、周ごとに 1 回だけ呼んで今の周の観測を渡す。
- Graph：上流は `id` と、`when` が名指しする対象（`observe`・`present`・`absent`）。量化が選んだものは観測するが辺にしない。宣言どうしの循環は読み込みで断る。plan・`/graph` は変わる宣言の下流と辺の種類（`depends_on`・`when`）を出す。消す宣言（`ensure: absent`）は、それに依る宣言が観測で Absent になるまで待つ（下流から消す）。
- 波：1 周の中で、当てたものが読み直しで宣言どおり（効いた）なら、それに依る・それが依る宣言のうち、待っていたか観測できなかったもの（Unknown）を同じ周にもう一度観測して判定し、当てる（E-J7）。上限は観測し直し 3 回・周の始まりから 2 分。同じ Resource は 1 周に 1 回だけ当てる。上流が失敗・効かないなら下流は次の周。予算は周の全部の波で数え、後の波で超えればその波を止める（前の波で当てたものは残る）。`plan` は最初の波だけを出す（観測し直さない）。
- settle：波ごとに、当てて成功した種類の `Settle`（Schema、fake では `settle:`）の最も長いものを 1 回待ってから読み直す。
- 効かない Apply（不変条件 14、E-J6・E-K10）：当てて成功したのに読み直すとずれたまま（作る・変える・消すが残る）なら、Record の `marks.json` の `ineffective` に宣言のハッシュと一緒に 1 つ足す。宣言どおりなら消し、読み直しが Unknown・作業中なら数えない（不変条件 4）。上限は当たる Policy の `budget.max_ineffective_applies`（1 以上）の最も小さいもので、どれも書かなければ 3。上限に達した周に止め（終了コード 4）、次の周からは当てない（Event `Halt`）。宣言を直すか、現実が宣言どおりになれば数え直す。

テストはルートの `make check`・`make test-race` が一緒に回す。
