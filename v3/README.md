# Sakutei の新しいコア（v3）

- 設計：`docs/core/README.md`。もとにした検討：`docs/review/core-usecase-review.md`。
- Go モジュール `github.com/tjst-t/sakutei/v3`（標準ライブラリだけ）。以前の実装（v2、モジュール `github.com/tjst-t/sakutei/v2`）との互換は持たない。v2 の実装はリポジトリから消した（最後の版は `v2.12.0`）。v2 の intent は `sakutei-migrate` で書き直す（Stage 9）。
- 実の provider・sink・Expand のモジュール・secret backend・道具は、コアの外の Go モジュール `providers/`（`github.com/tjst-t/sakutei/v3/providers`）に置く。詳しくは [providers/README.md](providers/README.md)。

作る順番は設計書 §8 の Stage 1〜9。**Stage 1〜9 はすべて実装済み**。

| Stage | 中身 |
|---|---|
| 1 | 型、intent と接続設定（config ファイル）の読み込みと検証、passthrough の Expand、fake provider、`plan` |
| 2 | 1 cycle の処理 `cycle.Run`（Apply・Delete・List・prune の守り）、Policy の mode と budget、Record（ロック・前の cycle の marks・audit のハッシュチェーン）、`reconcile --once`、終了コード |
| 3 | daemon、wake、read-only HTTP API、level Event・edge Event と outbox、sink `stdout` |
| 4 | Condition（三値の式と quantifier）、Graph の順序と待ち、同じ cycle での re-observe、settle、ineffective Apply の回数 |
| 5 | Procedure、承認と承認の CLI、temporary Policy（freeze） |
| 6 | two-phase Apply（Transaction）、最初の実 provider（`dnszone`・`hostconf`・`nftables`）と専用ツール `sakutei-nft-guard` |
| 7 | Expand の連鎖と外部コマンドの Expand、golden テストの道具、外部入力、採番、secret の generation と secret backend。Expand のモジュール（`template`・`netpolicy`・`compat`）と secret backend（`vault-kv2`・`command`） |
| 8a | sink（`alertmanager`・`syslog`・`metrics`・`otlp`）と承認ツール `sakutei-approve` |
| 8b | provider `redfish`・`metrics-probe`、provider の契約テスト |
| 8c | provider `netconf`・`cli-device`、Transaction の契約テスト |
| 8d | provider `proxmox`・`guest-os`、`FieldSpec.Match` と `core.Classifier` |
| 8e | provider `postgresql`・`sqlmigrate`・`ldap` |
| 8f | provider `s3`（Object Lock を含む）・`ansible`・`template-file` |
| 9 | Policy の `windows` と `allow`、v2 の intent を書き直す `sakutei-migrate`、import・同期の道具 `sakutei-import` |

Stage 6 以降のモジュールと道具は、どれも `providers/` にある。

## パッケージ

| パッケージ | 中身 |
|---|---|
| `core` | 正準型と provider の interface。<br>型：Resource・Range・Policy（承認 `Approval`、temporary Policy の期限と理由）・Schema（型 `float` を含む）・Observation（Present・Absent・Unknown・Converging）・cycle の設定・Procedure と step。<br>provider の interface：観測と List だけの `Reader`、one-shot な操作の `Invoker` と `ErrNotSent`、two-phase Apply の `Transactor`、変更ごとに「破壊的か・承認が要るか」を返す `Classifier`（`Hint`）、field の比べ方 `FieldSpec.Match`、採番の `Allocator`。<br>Event と sink の interface `Sink`（任意で、今 firing の一覧を受け取る `StateSink`、audit のハッシュチェーンを受け取る `AuditSink`）。<br>Condition の interface（三値 `Tri`・`on_unknown`・check の `CheckFunc`） |
| `approval` | 承認を数える（N-of-M・申請者≠承認者・拒否・期限切れ・使用済みのリクエスト）。Policy の承認と Procedure の gate で同じ数え方をする。承認・拒否を書く前の検証と書き込み（`Decide`。CLI と `sakutei-approve` で共通） |
| `procedure` | Procedure を開始し、1 step ずつ進め、halt し、観測で完了させる（`Run`）。plan では何も書かずに次の step を出す |
| `condition` | 観測についての三値の式を読み（`Parse`）、評価し（`Eval`）、Schema と突き合わせる（`Lint`） |
| `graph` | `depends_on` と `when` から宣言の依存（上流・下流・quantifier の選択・循環）を作る |
| `intent` | intent のディレクトリ（`*.yaml`・`*.yml`・`*.json`）を読む。最上位の `resources:`・`ranges:`・`policies:`・`procedures:`・`cycle:`・`pools:`・`allocations:`・`expand:`・`inputs:` を Resource・Range・Policy・Procedure・cycle の設定などにする |
| `config` | 接続設定（config ファイル）：`providers.<名前>.module`・`settings`、`sinks.<名前>.module`・`settings`・`streams`、`secret_backends.<名前>.module`、`expanders.<名前>.exec`、`inputs.<名前>.file`、`listen.read`。Range・exclusion・Policy は書けない（不変条件 24） |
| `expand` | Expand の interface（`Expander`・`Input`・`Output`）、intent をそのまま読む `Passthrough`、`expand:` の順に呼ぶ `Run`、外部コマンドの Expand `Exec` |
| `expand/expandtest` | Expand の golden テスト（`Golden`：2 回呼んで出力が同じことと `golden.json` との一致を確かめる、N-P5） |
| `alloc` | 採番。宣言の `{from_pool: …}` を intent のファイル `allocations/<pool>.yaml` の値で埋め、無ければ空きを選ぶ（`Resolve`）。cycle がロックを持ったまま、Apply する前にファイルと Record の写しに書く（`Persist`）。値を返すのは人（`Release`） |
| `secret` | secret の generation を宣言の参照に付ける（`Stamp`。読めなければ Uncertain）。組み込みの secret backend `file` |
| `load` | 接続設定と intent を読み、Expand の出力を検証する（不変条件 25） |
| `plan` | List と観測をして宣言と比べ、操作ごとに Apply するかを決める（`Decide`）。判定に使うのは prune の守り・依存の待ち（Condition）・Policy の mode と budget・window と allow・ineffective Apply の回数。受け取るのは `core.Reader` だけで、何も書かない |
| `cycle` | 1 cycle の処理（`Run`）。下の「cycle の流れ」を参照。`Plan` は同じ判定を何も書かずに出す |
| `events` | cycle の結果から level Event・edge Event を作る（`Derive`）。sink へ送る（`Deliver`。`StateSink` には cycle ごとに今 firing の一覧も渡す）。audit のハッシュチェーンを送る（`DeliverAudit`）。sink `stdout` |
| `daemon` | daemon。`cycle.Run` を繰り返し、interval を待つ。`sakutei wake`・SIGHUP・intent の変更で wake する |
| `readapi` | daemon の read-only HTTP API（`/status`・`/plan`・`/events`（SSE）・`/graph`・`/health`）。TLS・クライアント証明書・read-only トークン |
| `record` | Record のディレクトリ（下の「Record のファイル」を参照） |
| `sdk/hostexec` | provider SDK の、ホストでコマンドを実行する層（ローカル・ssh・sudo）。ホスト鍵は known_hosts で確かめる。到達できなければ `UnreachableError`。接続設定の `hosts:` の読み方 |
| `sdk/contract` | provider の契約テスト（`Run`、Transaction の `RunTxn`。§6.2、E-P2） |
| `sdk/fake` | テスト用の fake provider。Go から観測の結果・List の欠け・Apply の失敗・ineffective Apply・上流の奥の到達できない対象・Delete の refuse・one-shot な操作とその失敗（送信できない・送信できたか分からない・送信の途中で止まる）・two-phase Apply（`Prepare` の失敗・変更が別のものを壊す注入・時間切れの `Expire`）を仕込める。CLI からはモジュール `fake` の state のファイルと `ops` で使う |
| `cli`・`cmd/sakutei` | CLI。`sakutei`（daemon）とサブコマンド `plan`・`reconcile`・`wake`・`audit verify`・`approve`・`reject`・`approvals`・`freeze`・`unfreeze`・`start`・`cancel`・`alloc release`。組み込み用の Option は `WithModule`・`WithSink`・`WithCheck`・`WithExpander`・`WithSecretBackend`・`WithAllocator`。`cmd/sakutei` が組み込む provider は `fake` だけ |
| `internal/yaml` | 行番号を残す YAML のサブセットのパーサ（ブロック形式と 1 行のフロー形式） |

### cycle の流れ

`cycle.Run` は 1 cycle で次を行う。

1. Record のロックを取る。取れなければ何も進めない cycle になる。
2. temporary Policy・承認・Procedure の進捗を読み、audit に残す。
3. `plan.Decide` と同じ判定で、wave ごとに Apply する。
4. settle を待ってから再観測する。上流が反映されたら、下流を同じ cycle で re-observe して Apply する。
5. Procedure を進める。
6. ineffective Apply の回数・marks・audit を書く。
7. 通知の状態を進めて sink へ送り、health を書く。

### Record のファイル

| パス | 中身 |
|---|---|
| `LOCK` | flock |
| `format` | Record の形式 |
| `marks.json` | marks：Range ごとの件数・budget の window・ineffective Apply の回数・外部入力の件数（`input_counts`）・allow の使用数（`lanes`） |
| `audit/` | audit（各行が前の行のハッシュを持つハッシュチェーン） |
| `events/` | `state.json`（今 firing の一覧）・`outbox/`・`cursors/`（sink ごとの位置。audit の位置は `cursors/audit/`） |
| `health.json` | health |
| `wake` | wake の要求 |
| `runs/` | Procedure の進捗 |
| `approvals/` | 承認リクエストと承認・拒否 |
| `temp-policies/` | temporary Policy（freeze） |
| `commands/` | start・cancel の要求 |
| `alloc/` | 採番のファイルの写し |

## コマンド

`cmd/sakutei` は fake provider だけを組み込んだ CLI。実の provider を全部組み込んだ CLI は `providers/cmd/sakutei`。

```console
$ cd v3 && go run ./cmd/sakutei           --intent <dir> --config <file> --record <dir> [--json]
$ cd v3 && go run ./cmd/sakutei wake      --record <dir>
$ cd v3 && go run ./cmd/sakutei plan      --intent <dir> --config <file> [--record <dir>] [--json]
$ cd v3 && go run ./cmd/sakutei reconcile --intent <dir> --config <file> --record <dir> [--once] [--json]
$ cd v3 && go run ./cmd/sakutei audit verify --record <dir>
$ cd v3 && go run ./cmd/sakutei approve   <request-id> --record <dir> --as <名前> [--comment …]
$ cd v3 && go run ./cmd/sakutei reject    <request-id> --record <dir> --as <名前> --comment …
$ cd v3 && go run ./cmd/sakutei approvals --record <dir> [--all] [--json]
$ cd v3 && go run ./cmd/sakutei freeze    --record <dir> --reason … --until <時刻|長さ> [--select labels.<k>=<v>|type=<t>]… [--name <名前>] [--as <名前>]
$ cd v3 && go run ./cmd/sakutei unfreeze  <名前> --record <dir> --reason … [--as <名前>]
$ cd v3 && go run ./cmd/sakutei start     <procedure> --record <dir> [--target <名前>] [--reason …] [--as <名前>]
$ cd v3 && go run ./cmd/sakutei cancel    <run-id> --record <dir> --reason … [--as <名前>]
$ cd v3 && go run ./cmd/sakutei alloc release <pool> <value> --intent <dir> --record <dir> --reason … [--as <名前>]
```

`plan` の終了コード：

| コード | 意味 |
|---|---|
| 0 | 計算できた |
| 1 | 実行時エラー |
| 2 | 読み込み時にエラーにした（reject） |

`reconcile` の終了コード（E-J14）：

| コード | 意味 |
|---|---|
| 0 | 収束した |
| 1 | 実行時エラー（Apply の失敗・Record に書けない） |
| 2 | 読み込み時にエラーにした（reject） |
| 3 | drift か Unknown が残る（report・待ち・初めての cycle で消さない unmanaged を含む） |
| 4 | halt した（budget・List の急減・ineffective Apply の上限・`on_unknown: block` の条件が不明） |

## Stage 2 の決まり

- unmanaged（宣言に無いもの）を消すのは、次の条件を全部満たすときだけ（不変条件 6・7・9）：
  - Range を宣言し、`prune: true` である。
  - List が完全で、対象が List に出ている。
  - 宣言に無く、external owner のものでない。
  - 観測が Present である。
  - 消せる type である（`DestructiveDelete`。観測だけの type でない）。
  - 前の cycle の件数が Record にあり、急減（`max_drop`）していない。
  - budget を通る。
- 前の cycle の件数は、List の件数から自分が消した分を差し引いて覚える。急減で halt した cycle は前の値を保つ。
- Unknown・Converging・結果の無い対象には書かない。上流が整うまで下流は待つ（不変条件 3）。
  - 宣言した上流は、宣言と一致するまで待つ。
  - 宣言に無い上流は、Present になるまで待つ。
  - 上流を Apply した cycle の下流は、Stage 4 から同じ cycle で re-observe して Apply する。
- 当てはまる Policy のうち 1 つでも `mode: report` なら、通知するだけ。
- budget（`per_cycle`・`destructive_per_cycle`・`destructive_per_window` と `window`）は Policy ごとに数える。1 つでも超えれば cycle 全体を halt する（不変条件 20）。
- window の使用量は発射の前に Record に書く。書けなければ Apply しない。refuse された削除は数えない。
- Policy の `windows`・`allow` は Stage 9 で入った（下の「Stage 9 の決まり」）。承認と temporary Policy は Stage 5。`policy_sets` はまだ無い。

## Stage 3 の決まり

daemon：

- daemon（`sakutei --intent … --config … --record …`。`reconcile --once=false` も同じ）は、cycle ごとに intent と接続設定を読み直して `cycle.Run` を回し、`cycle.interval`（intent に書く。既定 60s）を待つ。
- 待ちは `sakutei wake`（Record の `wake` に時刻を書く）・SIGHUP・intent のディレクトリの変更で切り上げる（E-N5）。
- cycle をまたいで覚えるものは全部 Record にあり、daemon 自身は何も持たない。だから one-shot 実行の繰り返しと daemon は、同じ Record から同じ結果・同じ Event を出す（不変条件 30）。`daemon` のテストは、同じ現実の並びに対して cycle ごとの結果・Record の全ファイル・sink に届いた Event が一致することを確かめる。
- 最初の読み込みで reject すれば終了コード 2。その後の cycle の読み込みエラーは health に残して回り続ける。
- ログは stderr に出す（`--json` なら cycle ごとに 1 行の JSON）。

level Event と edge Event：

- level Event は `Drift`・`Unknown`・`Converging`・`Unmanaged`・`Waiting`・`Halt`・`DeleteRefused`。
  - キー（Event の kind と対象）ごとに、firing になった cycle と resolved になった cycle にだけ outbox に積む。
  - 続いた cycle では `count` を数え、`cycle.stuck_after` の cycle 数で 1 回だけ `stuck` を積む（E-H1）。
  - 対象が Unknown・Converging の cycle と、Range の List が不完全な cycle では resolved にせず、`count` も進めない（不変条件 29）。
- edge Event は `Applied`・`Deleted`・`CycleDone`（件数と cycle の時間。数えたものだけ）。

outbox と sink：

- outbox（`events/outbox/`）は sink ごとの位置（`events/cursors/`）から送る。
- 送信できない・時間内に戻らない・panic した sink は、位置を保ったまま次の cycle に同じ位置から送り直す。cycle のエラーにはしない（不変条件 28）。
- 全部の sink が送ったファイルは消す。
- sink は `sinks:` に書く。コアに組み込みの sink は `stdout`（1 行 1 件の JSON。one-shot 実行では結果の前に出る）。ほかの sink は Stage 8a。
- ロックがほかにあって進めない cycle では、積まず送らない。

read-only HTTP API（`listen.read`）：

- GET だけを受ける。
  - `/status`：最後の cycle の結果（`reconcile --json` と同じ構造）。
  - `/plan`：何も消費しない plan。同時に 1 つだけで、2 つ目は 503。
  - `/events`：outbox の SSE（`?since=`・`Last-Event-ID`）。
  - `/graph`：依存。
  - `/health`：health。エラーが続いていれば 503（E-K11）。
- loopback でない宛先への平文は、`plaintext: true` を書いたときだけ。
- TLS・クライアント証明書（`tls.client_ca`）・read-only トークン（`auth.tokens_file`、Bearer）を書ける（N-L4）。

サブコマンド `status` はまだ無い（最後の cycle の結果は `/status` で読む）。`freeze`・承認・Procedure は Stage 5 で入った。

## Stage 4 の決まり

`depends_on`：

- 1 件ごとに `id`（名指しの上流）と `when`（Condition）の少なくとも一方を書く。
- `on_unknown`（`wait` が既定・`block`・`ignore`）は `when` と一緒に書く。
- `when` が真になるまで、下流は操作しない。
  - 偽：待つ（Event `Waiting`、終了コード 3）。
  - 不明：`wait` なら待つ。`block` なら halt して通知する（Event `Halt`、終了コード 4。E-C4 の容量のゲート）。`ignore` なら真とみなす。
- どの場合も、名指しの上流が Unknown・Converging なら待つ（不変条件 3）。
- `when` が無ければ Stage 2 と同じ（宣言した上流は宣言と一致するまで、宣言に無い上流は Present になるまで待つ）。

式（§3.4）：

- 書けるもの：
  - `observe(<type>/<id>).field`・`present(...)`・`absent(...)`
  - `desired.field`（この宣言の field）・`it.field`・`check(名前)`
  - quantifier `all`・`any`・`count`。対象は `<type>[ラベル=値, ...]` で、**宣言した** Resource から選ぶ。
  - 比較（`== != < <= > >= in contains`）・`and or not`
  - 定数（文字列・数・`true`・`false`・`[...]`）
- Unknown・Converging・観測していない対象への参照は不明。
- Absent の対象と、観測に無い field は「無い」。`==` は偽、`!=` は真、大小比較と `in`・`contains` は偽。
- 大小比較は数どうしと長さ（`"720h"`）どうしだけ。比べられない組は不明。
- `count(...) op N` は、真の件数が k〜k+不明の件数のどれでも同じ結果になるときだけ真か偽。それ以外は不明。
- 次は読み込み時にエラーにする：式の構文エラー、知らない provider・type・field、quantifier の外の `it`、登録していない `check`。
- `check` の実装は `cli.WithCheck`（Go）で組み込む。cycle ごとに 1 回だけ呼び、今の cycle の観測を渡す。

Graph：

- 上流は `id` と、`when` が名指しする対象（`observe`・`present`・`absent`）。
- quantifier が選んだ対象は観測するが、辺にはしない。
- 宣言どうしの循環は読み込み時にエラーにする。
- plan と `/graph` は、変わる宣言の下流と辺の種類（`depends_on`・`when`）を出す。
- 消す宣言（`ensure: absent`）は、それに依存する宣言が観測で Absent になるまで待つ（下流から消す）。

wave（E-J7）：

- 1 cycle の中で、Apply したものが再観測で宣言と一致した（反映された）とき、それに依存する宣言・それが依存する宣言のうち、待っていたものか Unknown だったものを同じ cycle で re-observe して判定し、Apply する。
- 上限は re-observe 3 回、または cycle の開始から 2 分。
- 同じ Resource を Apply するのは 1 cycle に 1 回だけ。
- 上流が失敗したか ineffective なら、下流は次の cycle。
- budget は cycle の全部の wave で数える。後の wave で超えたらその wave を halt する（前の wave で Apply したものは残る）。
- `plan` は最初の wave だけを出す（re-observe しない）。

settle：wave ごとに、Apply に成功した type の `Settle`（Schema。fake では `settle:`）のうち最も長いものを 1 回待ってから再観測する。

ineffective Apply（不変条件 14、E-J6・E-K10）：

- Apply は成功したのに、再観測でも drift が残る（作成・変更・削除が残る）とき、Record の `marks.json` の `ineffective` に宣言のハッシュと一緒に 1 を足す。
- 宣言と一致すれば消す。再観測が Unknown・Converging なら数えない（不変条件 4）。
- 上限は、当てはまる Policy の `budget.max_ineffective_applies`（1 以上）の最小値。どれにも書かなければ 3。
- 上限に達した cycle で halt し（終了コード 4）、次の cycle からは Apply しない（Event `Halt`）。
- 宣言を直すか、現実が宣言と一致すれば数え直す。

## Stage 5 の決まり

Procedure の書き方（`procedures:`、§3.7）：

- 書く field：
  - `target`：`type` と、`id` か `select.labels`
  - `trigger`：`on_drift` と `drift_fields`、または `manual`
  - `steps`・`done_when`・`precondition`・`invariant`・`concurrency`（既定 1）・`timeout`・`on_failure: halt`
- step は次のどれか 1 つと、`name`・`skip_if`・`critical`（apply・invoke だけ）を書く：
  - `apply`
  - `invoke`
  - `wait`（`when` か `for`、`timeout`）
  - `manual`（`instruction`・`url`・`timeout`）
  - `gate`（`approval`・`timeout`）
  - `policy`（`select` と `mode: report`）
- 式・名前・field の中の `{target}` は対象の名前、`{desired.field}` は対象の宣言の値に置き換える。
- 次は読み込み時にエラーにする：知らない操作・field、式のエラー、`done_when` の無い manual、同じ対象の同じ field を 2 つの Procedure が受け持つ宣言、申請者の分からない separation の gate。
- まだ無いもの（書けば読み込み時にエラー）：rollback の step、`exec`、`disruption`、`trigger: {policy: …}`、`before_delete`。

開始と進み方：

- `drift_fields` は収束ループが比べも Apply もしない（plan に「手順が受け持つ」と出る）。drift があれば対象ごとに 1 回 Procedure を開始する（E-F2・E-A5）。
- 同じ対象で未完了の Procedure は 1 つだけ。
- `manual` の Procedure は `sakutei start` で開始する（`--as` が申請者）。
- 進捗（`runs/<procedure>/<対象>.json`）を書くのは cycle だけ。
- cycle ごとに、Procedure の `timeout`・`done_when`・`invariant`・`skip_if` を見て、進められるだけ step を進める。
  - `done_when` が真なら完了。不明なら未完了。
  - `invariant` が真でなければ待つ。
- 完了・取り消した Procedure の進捗は消す（履歴は audit に残る）。
- `done_when` が真の対象には Procedure を開始しない。だから Record を失っても二度実行しない（不変条件 18）。

one-shot な操作と critical な step（不変条件 16）：

- `invoke`（provider の `core.Invoker`）と critical な step は、送信の前に「送信中」の記録を `runs/` に書く。
- 前の cycle の記録が残っていれば（送信の途中で止まった）、送信せずに Procedure を halt し、続行の承認（`<run-id>-resume-<step>`）をリクエストする。承認の要件は Procedure の最初の gate と同じ。gate が無ければ申請者以外の 1 人。
- 承認されれば、その step をもう一度送信する。
- `core.ErrNotSent` なら記録を消し、次の cycle に送り直す。ほかのエラーは送信できたか分からないので halt する。
- one-shot 実行（`reconcile --once`）は critical な step の手前で止まり、Event `ProcedureNeedsDaemon` を出す。critical な step を進めるのは daemon だけ。

halt した Procedure と Policy：

- halt した Procedure の対象は、収束でも宣言に戻さない（plan に「触らない」と出る、E-A9）。続行するか、`sakutei cancel <run-id> --reason …` で片づける。
- Procedure の書き込む step は、当てはまる Policy の budget を cycle の変更と合わせて数える。
- `mode: report` の Policy（freeze を含む）に当てはまる対象では、Procedure を開始せず、書き込む step も進めない。
- step `policy` の report は Procedure の間だけ効く（次の cycle から）。

承認（Policy の `approval: {quorum: {n, of}, separation, timeout}` と `applicant`、Procedure の `gate`）：

- cycle が承認リクエスト（`approvals/<id>/request.json`）を書く。
  - Policy の変更の ID は、宣言と操作から決まる `chg-…`。
  - gate の ID は `<run-id>-<step>`。
- `sakutei approve`・`reject`（または承認ツール）が `approvals/<id>/by-<名前>.json` を書く。
- 数えるのは cycle。申請者・`of` に無い人・期限の後の承認や拒否は数えず、異なる人だけを数える。
- 数える人が 1 人でも拒否すれば拒否。Policy の変更は halt し、gate は Procedure を halt する。
- 期限が切れればリクエストし直す（gate は halt する）。
- 使う前にリクエストに「使用済み」と書く。書けなければ Apply しない。使用済みのリクエストは使い回さない（不変条件 21）。
- CLI も、申請者・`of` に無い人・存在しないリクエスト・完了済みのリクエストへの承認や拒否と、理由の無い拒否をエラーにする。
- 新しい承認・拒否・期限切れ・使用済みの承認は audit に残す。

temporary Policy（`sakutei freeze`、`temp-policies/<名前>.json`）：

- 理由・期限・実行者が必須。対象（既定は全部）を `mode: report` にする。
- cycle が開始（`freeze.start`）を audit に残し、期限で消し（`freeze.expire`）、`sakutei unfreeze` の印（`<名前>.json.end`）で終える（`freeze.end`）。
- 効いている間は Event `Frozen`。
- 読めない temporary Policy・承認・進捗があれば、全体を report に寄せる（不変条件 22）。

daemon・Event・終了コード：

- daemon は `approvals/`・`temp-policies/`・`commands/` への書き込みで次の cycle を始める。
- 足した Event：`ApprovalPending`（承認待ち）・`Frozen`・`ProcedureNeedsDaemon`・`Waiting`（Procedure が待つ）・`Halt`（Procedure が halt した）・`ProcedureStep`（step の完了・Procedure の完了・取り消し）。
- one-shot 実行の終了コードでは、未完了の Procedure と承認待ちを 3、halt した Procedure と拒否を 4 に数える。

テスト：`cycle` の性質テストは、race detector のとき（と `-short`）回数を減らす（`make test-race` で v3/cycle を 60 秒以内に）。

## Stage 6 の決まり

two-phase Apply（Transaction。`apply.transaction`、§3.1・§3.3）：

- 書く field：
  - `group`：同じ名前の宣言を 1 つにまとめる。
  - `timeout`：既定 30s、2s 以上。
  - `confirm`：`phase: before|after` と `when` の Condition。
- provider は `core.Transactor` を実装する。実装していなければ読み込み時にエラーにする。
- `before` は判定（plan と同じ）で見る。通らなければ変えずに待つ。
- group の 1 つでも Apply できない（待つ・承認待ち・report・halt）なら、group ごと待つ。group の中の依存は読み込み時にエラーにする。
- cycle は単位（group か宣言）ごと・provider ごとに `Prepare` する。相手の側に「timeout の後に元に戻す仕掛け」を置いてから Apply する。
- settle を待ってから、timeout の半分まで 1 秒ごとに、全部の宣言を再観測する。全部が宣言と一致し、全部の `after` が真かを確かめる（不明は通らない）。
- 全部が通ったときだけ `Commit` する。通らなければ `Commit` を呼ばずに `Rollback` する。`Rollback` は急ぐためだけで、呼ばなくても相手の時間切れで戻る。
- 1 つでも `Prepare` できなければ、準備したものを戻して確定しない（不変条件 17）。
- 確かめられずに戻した宣言は、同じ宣言のまま Apply し直さない。ineffective Apply の回数を上限にして halt する（終了コード 4）。毎 cycle 締め出しと巻き戻しを繰り返さないため（不変条件 13・14）。
- 宣言を直すか、現実が宣言と一致すれば数え直す。結果の `transaction`・`rolled_back` に出る。

`FieldSpec.Digest`：観測は中身の sha256 だけを返し、比べるときは宣言の値も sha256 にする。平文を観測・plan に出さない（E-E2）。

実の provider：

- コアの外の Go モジュール `providers/`（`github.com/tjst-t/sakutei/v3/providers`）に置く。
- それを組み込んだ CLI は `providers/cmd/sakutei`、専用ツールは `providers/cmd/sakutei-nft-guard`。詳しくは [providers/README.md](providers/README.md)。
- v2 の試用と同じ手順（DNS と FW）を v3 で流すスクリプトは `docs/trial/v3/run.sh`。同じ手順を `providers/cmd/sakutei` の `TestTrial` が回す。
- 本物のカーネルでの two-phase Apply は `make integration-v3-nftables`。

## Stage 7 の決まり

Expand（§3.8）：

- cycle の 2 番目の処理で、intent の最上位の field をそのまま読む `Passthrough` のあと、intent の `expand:` に書いた順に Expand を呼び、出力を足していく。
  - `kind: go:<名前>`：`cli.WithExpander` で組み込んだもの。自分が読む intent の field を `Keys` で示す。
  - `kind: exec:<名前>`：接続設定の `expanders.<名前>.exec` のコマンド。読む field は `keys:`。stdin に入力の JSON を渡し、stdout に intent と同じ形の出力と `diagnostics` を受け取る。30 秒で時間切れ。
- Expand は足す分だけを返す。前の宣言を変えられるのは、依存を足す `DependsOn` だけ。
- 次は読み込み時にエラーにする：組み込まれていない Expand、同じ field を 2 つの Expand が読む宣言、誰も読まない field。
- 外部コマンドが失敗すれば、その cycle は読み込めない（終了コード 1。daemon は health に残して回り続ける）。

診断（§3.8）：

- `Reject`：読み込み時にエラーにする（終了コード 2）。
- `Warn`：通知する。
- `Uncertain`：Target の宣言をその cycle で Unknown にする。観測を試みず、Apply も削除もせず、下流は待つ（不変条件 3・5）。
- Target が type（`<provider>/<type>`）か provider の名前なら、宣言が欠けているかもしれないので、その provider の Range の unmanaged を消さない（hold にする、E-K3・不変条件 26）。

外部入力（E-K3・E-P4）：

- intent の `inputs: [{name, items, expected_min, max_drop}]` に、使う入力と欠けの目安を書く。
- 接続設定の `inputs.<名前>.file` に場所を書く（最上位がマップの YAML か JSON）。
- 件数は `items` のリストの長さ。書かなければ最上位の field の数。
- 次の入力は「到達できない」（`Reachable: false` と理由）とする：読めない・形が違う・`expected_min` より少ない・前の cycle の件数（Record の marks `input_counts`）から `max_drop` を超えて減った。
- 到達できない入力に依存する宣言は Expand が作らず、Uncertain を出す。欠けた cycle は前の件数を保つ。

golden テスト（N-P5）：

- `expand/expandtest.Golden` は、`testdata/<ケース>/intent/` と任意の `inputs.json` で Expand を 2 回呼ぶ。出力が同じこと（純粋さ）と `golden.json` との一致を確かめる。
- `go test -update` で golden を書き直す。
- Go の Expand にも外部コマンドの Expand にも使える。

採番（§3.8、N-B13・N-B12・N-A14）：

- intent に `pools: [{name, range: "100-199", blocks: [{name, range}], reuse}]` を書く。
- 宣言の field に `{from_pool: <pool>, block: <名前>, template: "192.0.2.{n}"}` と書くと、intent のファイル `allocations/<pool>.yaml` の値で埋める。
  - ファイルの形は `allocations: {pool, values: [{key, value}], last: [{block, value}]}`。key は `<type>/<id>#<field>`。
  - 値が無ければ、空きの先頭を選ぶ（`core.Allocator`。`cli.WithAllocator` で差し替えられる）。
  - `template` があれば `{n}` を値にした文字列、無ければ整数。
- plan は選ぶだけで書かない。
- cycle はロックを持ったまま、Apply する前に、ファイル（先）と Record の写し `alloc/<pool>.json`（後）に書き足し、audit に `alloc.assign` を残す。
- 書けない、または cycle の間にファイルか写しが変わったときは、その値に依存する宣言をその cycle で Unknown にする。

二重の払い出しをしない（不変条件 11）：

- 値は宣言から消えても返さない。返すのは `sakutei alloc release <pool> <value> --reason …`。Record のロックを取ってファイルと写しの両方から外し、audit に `alloc.release` を残す。
- `reuse: false` のプールは、block ごとの最後の値（`last`。ファイルにも書く）以下を払い出さない。だから返した値も、Record を失っても二度と払い出さない。
- ファイルが写しより戻っている（写しにある key が無い・値が違う・`last` が小さい。git で巻き戻した・手で消した）ときは、そのプールから新しい値を出さず、それを必要とする宣言を Unknown にする（終了コード 3）。
- ファイルが写しより進んでいる（人が書き足した）ときは受け入れ、写しを書き直す。
- 尽きた block は理由つきで halt する。
- 同じ値を 2 つに払い出すファイル・範囲の外の値は、読み込み時にエラーにする。

secret（§3.1・§3.8、E-G2・E-G3・E-G4・N-G2）：

- Schema の型が `secret` の field は `{secret: "<backend>:<パス>"}` と書く。平文は読み込み時にエラーにし、値を診断に出さない。
- secret backend は接続設定の `secret_backends.<名前>.module` に書く。組み込みは `file`。`vault-kv2`・`command` は `providers/`。ほかは `cli.WithSecretBackend` で組み込む。
- cycle の初めに、参照ごとに generation だけを読み（値は読まない。10 秒で時間切れ）、宣言の値を `<backend>:<パス>@gen=<generation>` にする。
- 比べるのはこの参照だけ。provider は Apply した generation の参照を観測に返す。generation が変われば Apply し直す。
- generation を読めない secret に依存する宣言は、その cycle で Unknown（観測も Apply もしない。下流は待つ、E-G4）。
- 値を取り出すのは provider が Apply の直前に `WriteContext.Secret` で行うときだけ。secret backend を持つのは Apply・Delete・Prepare・Invoke の書き込みの文脈だけ。
- 値は Record・出力・state・エラーメッセージに出ない（不変条件 27）。

Expand のモジュール（`providers/expanders`）：

- `go:template`：テンプレートを一覧の 1 件ごとに展開する。
- `go:netpolicy`：network policy を nftables の base chain に展開する。
- `go:compat`：compatibility table。
- 詳しくは [providers/README.md](providers/README.md)。

## Stage 8a の決まり

- sink（接続設定の `sinks.<名前>`）に `streams: [events, audit]` を書ける。書かなければ `events` だけ（E-H2・E-L1）。
- `audit` の stream：
  - audit のハッシュチェーン（`audit/`）を、Record に書いたままの行で送る。位置は `events/cursors/audit/<名前>.json`。
  - 送信できなければ位置を保ち、次の cycle に同じ位置から送り直す。届かない間は Record に貯まる（audit は消さない）。
  - audit を送れないモジュール（`core.AuditSink` を実装しない）に `audit` を書けば、読み込み時にエラーにする。
  - 送るのはロックを持つ cycle だけ。
- `core.StateSink` を実装する sink には、outbox のあとに cycle ごとの今 firing の一覧（`events/state.json`）も渡す。続いている状態を送り直す sink（Alertmanager の endsAt・metrics のゲージ）が使う。daemon を再起動しても一覧から送り直せる。
- どの送信も時間を区切って呼ぶ（`events.DeliverTimeout`、既定 10s）。送信できない・戻らない・panic でも cycle を止めない（不変条件 28）。cycle の結果の `deliveries` に sink ごとの結果が出る（audit は `stream: audit`）。
- sink のモジュール（`providers/sinks`）：`alertmanager`・`syslog`・`metrics`・`otlp`。
- 承認ツール `sakutei-approve`（`providers/cmd/sakutei-approve`）。詳しくは [providers/README.md](providers/README.md)。
- `sakutei approve`・`reject` と承認ツールは、同じ検証（`approval.Decide`）を通す。申請者・要件の `of` に無い人・存在しないリクエスト・待っていないリクエストへの承認や拒否と、理由の無い拒否は、書く前にエラーにする（不変条件 21）。

## Stage 8b の決まり（redfish・metrics-probe と契約テスト）

契約テスト（§6.2、E-P2）：

- `sdk/contract.Run(t, newTarget)` は、確認ごとに新しい `Target`（provider と、fake の相手や模擬）を作る。Target に書かなかった確認は飛ばす。
- 確かめる内容：
  - 到達できないなら Unknown、無いなら Absent。
  - 作って観測できる。冪等（宣言と一致していれば 2 回目の Apply は相手に書かない）。
  - 観測の field が Schema にあり、正規形である。
  - List が完全で、Observe と一致する。
  - secret が漏れない（エラーメッセージ・Note・Fields に secret の値が出ない）。
  - 書き込み用の credential を WriteContext の外（観測）で読まない。
  - Schema の lint を通る。
  - Invoke は送信できなかったときだけ `ErrNotSent` を返す。
  - refuse する削除で相手に書かない。
- 到達できない故障（接続の切断・5xx・時間切れ・認証の失敗・壊れた出力）で Unknown 以外を返す provider は落ちる。
- fake provider でも回す（`sdk/contract/contract_test.go`）。

型 `float`（`core.Float`、E-H4・N-T4）：小数の値（/metrics の値・メモリの量）。Condition の大小比較で比べる。NaN・±Inf は型に合わない。

provider `redfish`・`metrics-probe` は `providers/` にある（README に接続設定と intent の例・統合テスト `make integration-v3-redfish`）。

## Stage 8c の決まり（netconf・cli-device と Transaction の契約）

- Transaction の契約テスト（§6.2、E-B3・E-B8・不変条件 17）：`sdk/contract.RunTxn(t, newTarget)` は、確認ごとに新しい `TxnTarget` を作る。`TxnTarget` は、`Transactor` を実装する provider・準備する宣言・時間切れ・時間切れを過ぎるまで待つ `Expire` を持つ。
- 確かめる内容：
  - 準備した変更が効く。
  - 確定すれば、時間切れを過ぎても戻らない。
  - Commit も Rollback もしなければ、時間切れのあとに元に戻る。
  - Rollback で戻る。
- fake provider でも回す（`sdk/contract/txn_test.go`）。
- provider `netconf`・`cli-device` は `providers/` にある（README に接続設定と intent の例・netopeer2 の統合テスト `make integration-v3-netconf`）。

## Stage 8d の決まり（proxmox・guest-os とコアの 2 つの interface）

`FieldSpec.Match`（§3.3、E-I7・E-D1・E-C2）：

- 宣言と観測の比べ方を provider が決める field。
- plan・Apply 後の再観測・契約テストが同じ比べ方を使う。
- 観測に無い field でも、Match が等しいと言えば差分にしない（作るときだけ使う field。作るときは Apply に渡す）。

`core.Classifier`（§3.3、E-C2・E-B6・E-C1）：

- provider が、作成・変更ごとに `Hint`（破壊的か・承認が要るか・理由）を返す。
- 破壊的な変更は budget の破壊的な変更に数える。
- 承認が要る変更は、承認が揃うまで Apply しない。当てはまる Policy に approval が無ければ、申請者以外の 1 人の承認をリクエストする（`sakutei approve` で揃える）。
- 理由は plan の `provider:` の行と、JSON の `classified`・`needs_approval` に出る。

provider：

- `proxmox`：type `vm`・`fw-rules`、観測だけの `volume`・`fw-rule`。network policy の実現先 `proxmox-fw`。
- `guest-os`：type `os`（観測だけ）。
- どちらも `providers/` にある（README に接続設定と intent の例・入れ子の Proxmox VE の統合テスト `make integration-v3-proxmox`）。

## Stage 8e の決まり（postgresql・sqlmigrate・ldap）

- provider `postgresql`・`sqlmigrate`・`ldap` は `providers/` にある（README に接続設定と intent の例・postgres の統合テスト `make integration-v3-postgresql`・389 Directory Server の統合テスト `make integration-v3-ldap`）。
- sqlmigrate の v2 の `strategy: task` は Procedure（`trigger: on_drift`・`invoke: migrate`・`done_when`）に書き直す。Procedure の `on_failure` はいまも `halt` だけ。失敗したときの戻しは `migrate` の引数 `rollback_on_fail` で行う（E-F2・N-F2）。

## Stage 8f の決まり（s3・ansible・template-file）

- provider `s3`（Object Lock を含む）・`ansible`・`template-file` は `providers/` にある（README に接続設定と intent の例・S3 互換のサーバの統合テスト `make integration-v3-s3`・ansible-core の統合テスト `make integration-v3-ansible`）。
- s3：Object Lock の保持の変更は、`core.Classifier` で承認の要る破壊的な変更にする。中身のあるバケットの削除は refuse する。Range は `args.endpoint`・`args.prefix`。
- ansible と template-file は、読み込み時に `Validate` が宣言を覚え、Observe がそれを使う。

## Stage 9 の決まり（v2 の intent の書き直しと import・同期のツール。どちらもコアの外の `providers/`）

### Policy の window と allow

コアに Policy の `windows` と `allow` を足した（§3.6）。v2 の planned_change・remediation の時間帯・運用プロファイルの受け皿（E-J2・E-J3・E-A9・N-J2・E-R1）。

`windows`：

- 書ける形：
  - 毎週：`"Sat,Sun 02:00-04:00 Asia/Tokyo"`。曜日は `Mon-Fri`・`Sat,Sun` の形で、省けば毎日。終わりが始めより前なら日をまたぎ、曜日は window が開く日を指す。
  - 期間：`"2026-11-05 22:00 .. 2026-11-06 06:00 Asia/Tokyo"`。RFC 3339 も書ける。tz を省けば UTC。
  - `"*"`
- allow でない Policy は、window の外では当てはまる変更を report にする（plan の `action: report`、`policy … の窓の外`）。

`allow: {kind, max_count}`（kind は `delete`・`update`・`create`・`reimage`・`destructive`）：

- allow の Policy は、当てはまる変更を縛らない。
- window の中で承認が揃い、cooling-off が過ぎたときだけ開く。開いている間は、select と kind に合う変更を、通常の budget の外で max_count まで通す。
- allow で通した変更は budget に数えず、通常の budget の超過でも halt しない（halt するのは宣言の外の暴発だけ、E-J2）。
- `reimage` は、同じ type・同じラベルの削除と作成の対を 1 と数える（作成は 0）。
- 使った数は Record の marks `lanes` に、発射の直前に書く。失敗しても戻さない。fingerprint は select・kind・window から作る（max_count を変えただけなら保つ）。
- `select: {all: true}` と、`mode`・`budget` は読み込み時にエラーにする。

allow の承認：

- Policy ごとに 1 つのリクエスト（`lane-…`、kind `lane`）。変更ごとではない。使っても消えず、宣言か max_count を変えればリクエストし直す。
- `approval.cooling_off` は allow の Policy にだけ書ける。宣言を初めて見た時刻（marks の `seen`）から数える。
- `quorum` を書かずに `cooling_off` だけを書けば、承認者を置かない allow になる（v2 の solo）。
- plan とテキスト出力に、`別枠 <policy>` の行と、変更ごとの `別枠: policy …` が出る。

fake：Range に `scope`（`"k=v,…"`。現実のラベルがちょうどそれと一致するもの）と、state の `labels` を足した（v2 の fake の scope の書き直し用）。

### sakutei-migrate

`sakutei-migrate`（`providers/migrate`・`providers/cmd/sakutei-migrate`、§7.2、N-I7）：

- 読むもの：
  - v2 の intent のディレクトリ（直下の `.yaml`・`.yml`・`.json`）。
  - provider の設定：`--provider-config` と、v2 の起動引数 `--zone`・`--ruleset`・`--state`・`--redfish-config`・`--sqlmigrate-config`・`--alloc-store`・`--audit-log`（同じ名前で受ける）。
- `--out` に書くもの：新しい intent（`intent/`）・接続設定（`config.yaml`）・v2 の ID と新しい ID の対応（`migrate-ids.json`）。
- 書き直せないものは黙って落とさない。全部を挙げてエラーにし（終了コード 2）、何も書かない。
- 捨てても意味の変わらないもの（observe_tier・Sync Mode の scope・verify: invariant）は `note:` で通知する。

Range は設定から intent へ移す（不変条件 24）：

- dnszone のゾーン（`--zone` のファイル名 `<zone>zone` から）と、nftables の扱う table（宣言の ID から）は `ranges` にする。
- `deletion_policy: PruneWithinScope` は Range の `prune: true` にする。
- v2 の scope は宣言の `labels` にする。
- fake の PruneWithinScope は、宣言の scope と、それを含む今の現実の scope ごとの Range（`args.scope`・`labels`）にする。v2 は宣言の scope を含む scope の unmanaged も消していた。後から現れた scope は Range に入らない。
- 起動引数の budget（`--max-changes-per-cycle`・`--max-destructive-per-cycle`・`--max-destructive-per-window`・`--window-duration`）と `--mode alert-only` は、全部に当てはまる Policy `default` にする。

Policy：

- `planned_change` は allow の Policy にする：
  - scope → `select.labels`
  - kind → `allow.kind`（recreate は reimage）
  - max_count → `allow.max_count`。`--audit-log` の v2 の監査ログで使った数を差し引く。使い切っていれば書かない。
  - window → `windows`
  - applicant → `applicant`
- 承認はプロファイル（`profile:` を書かなければ team）と宣言の `requirements` で決める：
  - team：1 人。
  - enterprise：2 人と申請者≠承認者（`separation`）。
  - solo：`cooling_off` だけ（`solo.cooling_off`、既定 2h）。
  - 宣言の `cooling_off` も `approval.cooling_off` にする。
- v2 は承認者（`approval.approvers`）を intent に書いた。新しいコアは承認を Record に残すので、書いた承認者と `--approver` を承認できる人（`quorum.of`）にする。書き直したあと、承認者が `sakutei approve` する。
- `requirements.roles` は、その role を名乗った承認者か、`--role` の持ち主だけにする。
- `remediation:`：
  - default が alert-only（省いたときも）で、schedule の window が全部 auto なら、window の Policy にする（tz は window の末尾に付ける）。
  - window の無い default alert-only は `mode: report` の Policy にする。
  - default auto は何も書かない。
- `scopes:` の `remediation: alert-only` は、その scope のラベルに当てはまる report の Policy にする。
- enterprise では、requirements の無い Workflow の gate も 2 人と申請者≠承認者にする。

field：

- v2 の field は文字列なので、数の字句は Schema の型に合わせて整数にする（dnszone の `ttl`・sqlmigrate の `version`・nftables の `priority`）。
- nftables の `rules` は ` ; ` で区切ってリストにする。
- template-file の `var.<名前>` は `vars` にする。
- 採番の `{from_pool: …}` には `template: "{n}"` を足す（v2 は払い出した値を文字列で入れていた）。
- v2 の採番の記録（`--alloc-store`）は、払い出した値を `allocations/<pool>.yaml` に写す（払い出し直さない、不変条件 11）。予約のままの値は写さず、通知する。

Procedure：

- v2 の Workflow は、stage ごとに 1 つの Procedure（`trigger: on_drift`）にする。
  - `drift_fields` は、step が対象に Apply する field。
  - 前の stage の対象が宣言と一致するまで開始しないよう `precondition` を付ける。
  - `invariant.min_active` は `count(…) >= N` にする。
  - `done_when`・`require`・`wait` は `observe(…).field == "値"` の and にする。
- gate の承認者は名前の並び。v2 の roles は `--role <role>=<名前>,…` で渡し、roles の無い gate は `--approver` で渡す（無ければエラー）。`applicant_ne_approver` は申請者を並びから外す。
- step が Apply する別の宣言（導入中の CD など）は宣言から外す。新しいコアは、Procedure の対象でない宣言を収束で戻して取り合うため。最終の値は Procedure の最後の step が Apply する。
- v2 の単純 Task（sqlmigrate の `strategy: task`）は `invoke: migrate` の Procedure にする。
- secret の参照（`secretref:…`）は `--secret-file <参照>=<ファイル>` でファイルにする。

意味の違い：v2 は Workflow を宣言している間、完了するまで実行した。新しいコアは `drift_fields` に drift があるときに開始する（今すぐ実行するなら `sakutei start`）。

完了条件の確認（`providers/migrate/testdata/trial-cases.json`、32 件）：

- v2 の試用の intent を全部、それぞれの最初の現実に対して v2 の plan が出す操作を表に書く。新しいコアの側（`providers/migrate/trial_test.go`）は書き直した intent の plan と突き合わせる（v2 の実装を消すまでは、v2 の側の試験が v2 の plan と突き合わせていた）。v2 の試用の intent と最初の現実は `providers/migrate/testdata/trial/` にある（v2 の `docs/trial/` から移した）。
- 対象の intent：
  - v1・allocation
  - observe（最初の現実と、癖のあるファイル）
  - planned-change（planned_change の無い資源、通常の budget で halt する形、A3 の専用の allow で通る、A6 の window の外、A7 の範囲の外、B2 の削除の allow、B4 の reimage の対）
  - sync-import・workflow-gates
  - sharding（a・b・c）・dr-mode（tokyo・osaka）
  - sqlmigrate（v2 の試用の run.sh が書く intent の写し `providers/migrate/testdata/trial/sqlmigrate/intent/`）
  - redfish-os-install
  - remediation-profile（window の中・window の外・今日の曜日・明日の曜日・default の alert-only と、プロファイル team の承認なし・承認者 1 人・enterprise の承認者 1 人・申請者と 1 人・2 人・solo の cooling-off・cooling-off 0s）
- v2 の試用の run.sh が今の時刻から書く window は、表の `{{t <ずれ> <形> [tz]}}` で書く。
- 比べるのは、収束が Apply する操作（作成・変更・削除・halt・window の外で通知だけ）と、観測できないもの。Procedure に渡したものは、v2 の Workflow・Task が持っていたものに収まること。
- 承認の要る allow は、新しいコアの側で先に reconcile を 1 回流して Record に承認リクエストを作り、現実を戻し、v2 の intent に書いた承認者が `sakutei approve` してから plan で比べる（`v3_record`・`v3_approve`）。
- 新しいコアの provider が現実を v2 と違って読む（直した）ところだけ、`v3_ops`・`v3_unknown` と理由 `v3_reason` を書く（observe の癖のあるファイル、到達できない BMC）。

表に入れていない v2 の試用の run.sh の手順：

- daemon で window が開いて閉じる、cooling-off の時計が再起動で延びない（新しいコアの単体テスト `cycle/lane_test.go` が確かめる）。
- `--mode`・`--freeze` の引数（新しいコアは起動引数に Policy を持たず、`sakutei freeze` を使う）。
- 宣言のエラーを読み込み時に reject する手順（v2 の読み込みの規則）。
- A4 の使い切り（v2 の監査ログの使用量は、書き直しで max_count から差し引く。単体テスト `TestPlannedChangeBecomesLane`）。
- B5 の急減の判定と B6 の daemon の window（daemon の cycle の性質で、plan の比較ではない）。

まだ書き直せずエラーにするもの（どれも試用の intent には無い）：

- Workflow の `disruption` と `trigger: reimage`
- `remediation` の、window の中だけ通知する window（default auto で alert-only の window。新しいコアでは freeze を使う）
- 最上位の `network_policy`・`compatibility`・`external_owners`・`providers:`・`runtime:`
- `ready_conditions`・`depends_on` の `ready_condition`
- `update_policy` の `window`（v2 の単純 Task のメンテナンス window）
- nftables の live、sqlmigrate の PostgreSQL
- 知らない provider と field
- scope だけで分かれていた ID が、同じ新しい ID になるもの

### sakutei-import

`sakutei-import`（`providers/importer`・`providers/cmd/sakutei-import`、§7.2、E-I1・E-I2・E-Q2・N-I1・N-I5・N-Q3・N-E3）：

- intent の `ranges` ごとに List し、列挙した ID を Observe して、Schema の field（中身の sha256 だけの field を除く）を宣言の形で `--out` に書く。
- 観測と List の interface だけを使い、Apply の経路にはつながない。
- external owner のものは書かない。
- List のエラー・欠け、観測の Unknown・Converging があれば、その回は何も書かない（前のファイルを残す、終了コード 3）。
- 観測の形と宣言の形が違う provider は、任意の interface `importer.Declarer` で埋める（dnszone：値が 1 つなら `value`、2 つ以上なら `values`）。書く宣言が読み込みを通らなければ書かない。
- `--undeclared` は宣言に無いものだけを書く。

同期（v2 の Sync Mode）：

- 同じ書き出しを `--every <長さ>` で繰り返し、変わったときだけ書き換える。
- ファイルの先頭に本文の sha256 を書く。手で書き換えられた（または道具が書いたものでない）ファイルは上書きせずに止める（終了コード 4）。
- `--commit` は、書き換えるたびにそのファイルだけを 1 つの git commit にする（trailer `Sakutei-Import`）。
- 収束させない範囲は、Policy の `mode: report` で書く。

組み込むモジュールの一覧は `providers/builtin`（`sakutei` と `sakutei-import` が同じ一覧を使う）。

## テスト

ルートの `make check`・`make test-race` が、v3 と `providers/` のテストを回す。
