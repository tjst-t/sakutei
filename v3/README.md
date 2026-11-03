# Sakutei の新しいコア（v3）

設計は `docs/core/README.md`、もとにした検討は `docs/review/core-usecase-review.md`。ルートの v2 のコードとは別の Go モジュール（`github.com/tjst-t/sakutei/v3`、標準ライブラリだけ）。互換は持たない。

作る順番は設計書 §8 の 9 段。いまは **段 8a** まで：段 1（型・intent とつなぎ方の設定の読み込みと検証・素通しの Expand・偽の provider・`plan`）に、段 2（1 周の処理 `cycle.Run`：Apply・Delete・List・prune の守り、Policy の mode と予算、Record のロック・前の周の目印・監査の鎖、`reconcile --once`、終了コード）と、段 3（常駐・起床・読み取り HTTP・Events の状態型と送り出しの列・stdout の送り先）と、段 4（Condition の三値の式と量化・Graph の順序と待ち・同じ周の観測し直し・settle と効かない Apply の回数）と、段 5（Procedure・承認と承認の CLI・一時的な Policy の freeze）と、段 6（2 段の Apply・最初の実 provider の dnszone・hostconf・nftables と決まった道具 nft-guard。provider はコアの外のモジュール `providers/`）と、段 7（Expand の連鎖と外のコマンドの Expand・golden のテストの道具・外部入力・採番・秘密の世代と保管先。Expand のモジュール template・netpolicy・compat と保管先 vault-kv2・command は `providers/`）と、段 8a（Events の送り先 alertmanager・syslog・metrics・otlp と承認ツール sakutei-approve。どれも `providers/`）を足した。段 8 の残り（provider）は段 8b〜8f（8b の redfish・metrics-probe と、8d の proxmox・guest-os は済んだ）。

| パッケージ | 中身 |
|---|---|
| `core` | Resource・Range・Policy（承認 `Approval`・一時的な Policy の期限と理由）・Schema・Observation（ある・無い・分からない・作業中）と Provider の口（観測と列挙だけの `Reader`、一回きりの操作の `Invoker` と `ErrNotSent`、状況で壊す・承認の要る変更を言う `Classifier`、項目の比べ方 `FieldSpec.Match`）・Event と送り先の口 `Sink`（任意で今の firing の一覧を受け取る `StateSink`・監査の鎖を受け取る `AuditSink`）・周の設定・Condition の口（三値 `Tri`・`on_unknown`・確かめの `CheckFunc`）・Procedure と段 |
| `approval` | 承認を数える（N-of-M・申請者≠承認者・拒否・期限切れ・使った依頼）。Policy の承認と手順の gate で同じ数え方。判断を書く前の確かめと書き込み（`Decide`。CLI と sakutei-approve で同じ） |
| `procedure` | 手順を起こし・1 段ずつ進め・止め・観測で閉じる（`Run`。plan では何も書かずに次を出す） |
| `condition` | 観測についての三値の式を読み（`Parse`）、評価し（`Eval`）、Schema と突き合わせる（`Lint`） |
| `graph` | `depends_on` と `when` から宣言の依存（上流・下流・量化の選び方・循環）を作る |
| `intent` | intent のディレクトリ（`*.yaml`・`*.yml`・`*.json`）を読み、`resources:`・`ranges:`・`policies:`・`procedures:`・`cycle:` を Resource・Range・Policy・Procedure・周の設定にする |
| `config` | つなぎ方の設定（`providers.<名前>.module`・`settings`、`sinks.<名前>.module`・`settings`・`streams`、`listen.read`）。範囲・除外・方針は書けない（不変条件 24） |
| `expand` | Expand の口（`Expander`・`Input`・`Output`）・intent をそのまま読む `Passthrough`・`expand:` の順に呼ぶ `Run`・外のコマンドの Expand `Exec` |
| `expand/expandtest` | Expand の golden のテスト（`Golden`：2 回呼んで同じ出力と golden.json を確かめる、N-P5） |
| `alloc` | 採番：宣言の `{from_pool: …}` を intent のファイル `allocations/<pool>.yaml` の値で埋め、無ければ空きを選ぶ（`Resolve`）。周がロックを持って当てる前にファイルと Record の写しに書き（`Persist`）、人が返す（`Release`） |
| `secret` | 秘密の世代を宣言の参照に付ける（`Stamp`。読めなければ Uncertain）と、組み込みの保管先 `file` |
| `load` | 設定と intent を読み、Expand の出力を検証する（不変条件 25） |
| `plan` | List と観測をして宣言と比べ、prune の守り・依存の待ち（Condition）・Policy の mode と予算・効かない回数で操作ごとに当てるかを決める（`Decide`）。受け取るのは `core.Reader` だけで、何も書かない |
| `cycle` | 1 周（`Run`）。Record のロックを取り、一時的な Policy・承認・手順の進みを読んで監査に残し、`plan.Decide` と同じ判定で波ごとに当て、settle を待って読み直し、上流が効いたら下流を同じ周に観測し直して当て、手順を進め、効かない回数・目印・監査を書き、通知の状態を進めて送り出し、健全性を書く。ロックが取れなければ進めない周。`Plan` は同じ判定を何も書かずに出す |
| `events` | 周の結果から状態型・出来事型の Event を作る（`Derive`）・送り先へ送り出す（`Deliver`。`StateSink` には周ごとに今の firing の一覧も）・監査の鎖を送り出す（`DeliverAudit`）・stdout の送り先 |
| `daemon` | 常駐：`cycle.Run` を繰り返し、間隔を待ち、`sakutei wake`・SIGHUP・intent の変更で起きる |
| `readapi` | 常駐の読み取り HTTP（`/status`・`/plan`・`/events`（SSE）・`/graph`・`/health`）と TLS・クライアント証明書・読むだけのトークン |
| `record` | Record のディレクトリ：`LOCK`（flock）・`format`・`marks.json`（範囲ごとの件数・予算の窓・効かない Apply の回数）・`audit/`（前の行のハッシュの鎖）・`events/`（`state.json`・`outbox/`・`cursors/`・監査の送り先の位置 `cursors/audit/`）・`health.json`・`wake`・`runs/`（手順の進み）・`approvals/`（依頼と判断）・`temp-policies/`（freeze）・`commands/`（start・cancel の頼み） |
| `sdk/hostexec` | provider SDK のホストでコマンドを走らせる層（手元・ssh・sudo。ホスト鍵を known_hosts で確かめる。届かないは `UnreachableError`）と、設定の `hosts:` の読み方 |
| `sdk/contract` | provider の契約テスト（`Run`。§6.2：届かないなら Unknown・無いなら Absent・作って読む・冪等・正規形・List の完全さと Observe との一致・秘密が漏れない・書き込みの資格を WriteContext の外で読まない・Schema の lint・Invoke は送れなかったときだけ `ErrNotSent`・Delete の断り。E-P2） |
| `sdk/fake` | テスト用の偽の provider（Go から観測の答え・List の欠け・Apply の失敗・効かない Apply・上流の奥で届かない対象・削除の断り・一回きりの操作とその失敗（送れていない・送れたか分からない・送る途中で止まる）・2 段の Apply（`Prepare` の失敗・変更が別のものを壊す注入・時間切れの `Expire`）を置く・CLI ではモジュール `fake` の state のファイルと `ops`） |
| `cli`・`cmd/sakutei` | CLI。`sakutei`（常駐）とサブコマンド `plan`・`reconcile`・`wake`・`audit verify`・`approve`・`reject`・`approvals`・`freeze`・`unfreeze`・`start`・`cancel`・`alloc release`。組み込みは `WithModule`・`WithSink`・`WithCheck`・`WithExpander`・`WithSecretBackend`・`WithAllocator` |
| `internal/yaml` | 行番号を残す YAML の部分集合のパーサ（ブロックと 1 行のフロー） |

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
$ cd v3 && go run ./cmd/sakutei start     <procedure> --record <dir> [--target <名前>] [--as <名前>]
$ cd v3 && go run ./cmd/sakutei cancel    <run-id> --record <dir> --reason … [--as <名前>]
$ cd v3 && go run ./cmd/sakutei alloc release <pool> <value> --intent <dir> --record <dir> --reason … [--as <名前>]
```

`plan` の終了コード：0 計算できた／1 実行の誤り／2 読み込みで断った。

`reconcile` の終了コード（E-J14）：0 収束した／1 実行の誤り（当てて失敗した・Record に書けない）／2 読み込みで断った／3 ずれか Unknown が残る（report・待ち・初めての周で消さない宣言外を含む）／4 止まった（予算・List の急減・効かない Apply の上限・`on_unknown: block` の条件が分からない）。

段 2 の決まり：

- 宣言外を消すのは、Range を宣言し、`prune: true` で、List が完全で、List に出て、宣言に無く、外の持ち主でなく、観測が Present で、消せる種類（`DestructiveDelete`・観測だけの種類でない）で、前の周の件数が Record にあり、急減（`max_drop`）していず、予算を通ったときだけ（不変条件 6・7・9）。前の周の件数は List の件数から自分が消した分を差し引いて覚え、急減で止めた周は前の値を保つ。
- Unknown・Converging・答えの無い対象には書かず、宣言した上流が宣言どおりでない下流（宣言に無い上流は Present でない下流）は待つ（不変条件 3）。上流を当てた周の下流は、段 4 で同じ周に観測し直して当てるようにした。
- 当たる Policy のうち 1 つでも `mode: report` なら知らせるだけ。予算（`per_cycle`・`destructive_per_cycle`・`destructive_per_window` と `window`）は Policy ごとに数え、1 つでも超えれば周全体を止める（不変条件 20）。窓の使用量は発射の前に Record に書き、書けなければ当てない。断られた削除は数えない。
- 窓・別枠・`policy_sets` は後の段（承認・一時的な Policy は段 5）。

段 3 の決まり：

- 常駐（`sakutei --intent … --config … --record …`、`reconcile --once=false` も同じ）は、周ごとに intent と設定を読み直して `cycle.Run` を回し、`cycle.interval`（intent、既定 60s）を待つ。`sakutei wake`（Record の `wake` に時刻を書く）・SIGHUP・intent のディレクトリの変更で待ちを切り上げる（E-N5）。周をまたいで覚えるものは全部 Record で、常駐は何も持たない。だから一回実行の列と常駐は同じ Record から同じ結果・同じ Event を出す（不変条件 30。`daemon` のテストで、同じ現実の並びに対して周ごとの結果・Record の全ファイル・送り先に届いた Event が一致することを確かめる）。初めの読み込みで断れば終了コード 2。その後の周の読み込みの誤りは健全性に残して回り続ける。ログは stderr（`--json` なら周ごとに 1 行の JSON）。
- 状態型の Event（`Drift`・`Unknown`・`Converging`・`Unmanaged`・`Waiting`・`Halt`・`DeleteRefused`）はキー（種類＋対象）ごとに firing になった周と resolved になった周にだけ送り出しの列に積み、続いた周は `count` を数え、`cycle.stuck_after` の周数で 1 回だけ `stuck` を積む（E-H1）。対象が Unknown・作業中の周、範囲の List が不完全な周には resolved にせず、`count` も進めない（不変条件 29）。出来事型は `Applied`・`Deleted`・`CycleDone`（件数と周の時間。数えたものだけ）。
- 送り出しの列（`events/outbox/`）は送り先ごとの位置（`events/cursors/`）から送る。送れない・時間内に戻らない・panic した送り先は位置を保って次の周に同じ位置から送り直し、周の誤りにしない（不変条件 28）。全部の送り先が送ったファイルは消す。送り先は `sinks:` に書き、いまあるモジュールは `stdout`（1 行 1 件の JSON。一回実行では結果の前に出る）。進めない周（ロックがほか）は積まず送らない。
- 読み取り HTTP（`listen.read`）は GET だけ。`/status` は最後の周の結果（`reconcile --json` と同じ構造）、`/plan` は何も消費しない plan（同時に 1 つ、2 つ目は 503）、`/events` は送り出しの列の SSE（`?since=`・`Last-Event-ID`）、`/graph` は依存、`/health` は健全性（続いた誤りがあれば 503、E-K11）。loopback でない宛先の平文は `plaintext: true` を書いたときだけ。TLS・クライアント証明書（`tls.client_ca`）・読むだけのトークン（`auth.tokens_file`、Bearer）を書ける（N-L4）。
- `status` は後の段。`freeze`・承認・Procedure は段 5。

段 4 の決まり：

- `depends_on` の 1 件は `id`（名指しの上流）と `when`（Condition）の少なくとも一方を書く。`on_unknown`（`wait` 既定・`block`・`ignore`）は `when` と一緒に書く。`when` が真になるまで下流は操作しない。偽は待つ（Event `Waiting`、終了コード 3）。不明は `wait` なら待ち、`block` なら止めて知らせる（Event `Halt`、終了コード 4。E-C4 の容量の門）、`ignore` なら真とみなす。どの場合も、名指しの上流が Unknown・作業中なら待つ（不変条件 3）。`when` が無ければ段 2 と同じ（宣言した上流は宣言どおり、宣言に無い上流は Present）。
- 式（§3.4）：`observe(<type>/<id>).項目`・`present(...)`・`absent(...)`・`desired.項目`（この宣言の項目）・`it.項目`・`check(名前)`・`all`・`any`・`count`（選び方は `<type>[ラベル=値, ...]` で、**宣言した** Resource から選ぶ）・比較（`== != < <= > >= in contains`）・`and or not`・定数（文字列・数・`true`・`false`・`[...]`）。Unknown・作業中・観測していない対象への参照は不明。Absent の対象と観測に無い項目は「無い」で、`==` は偽・`!=` は真・大小と `in`・`contains` は偽。大小は数どうしと長さ（`"720h"`）どうしだけで、比べられない組は不明。`count(...) op N` は真の件数 k〜k+不明の件数のどれでも同じ結果のときだけ真偽。式の形の誤り、知らない provider・種類・項目、量化の外の `it`、登録していない `check` は読み込みで断る。`check` の確かめは `cli.WithCheck`（Go）で組み込み、周ごとに 1 回だけ呼んで今の周の観測を渡す。
- Graph：上流は `id` と、`when` が名指しする対象（`observe`・`present`・`absent`）。量化が選んだものは観測するが辺にしない。宣言どうしの循環は読み込みで断る。plan・`/graph` は変わる宣言の下流と辺の種類（`depends_on`・`when`）を出す。消す宣言（`ensure: absent`）は、それに依る宣言が観測で Absent になるまで待つ（下流から消す）。
- 波：1 周の中で、当てたものが読み直しで宣言どおり（効いた）なら、それに依る・それが依る宣言のうち、待っていたか観測できなかったもの（Unknown）を同じ周にもう一度観測して判定し、当てる（E-J7）。上限は観測し直し 3 回・周の始まりから 2 分。同じ Resource は 1 周に 1 回だけ当てる。上流が失敗・効かないなら下流は次の周。予算は周の全部の波で数え、後の波で超えればその波を止める（前の波で当てたものは残る）。`plan` は最初の波だけを出す（観測し直さない）。
- settle：波ごとに、当てて成功した種類の `Settle`（Schema、fake では `settle:`）の最も長いものを 1 回待ってから読み直す。
- 効かない Apply（不変条件 14、E-J6・E-K10）：当てて成功したのに読み直すとずれたまま（作る・変える・消すが残る）なら、Record の `marks.json` の `ineffective` に宣言のハッシュと一緒に 1 つ足す。宣言どおりなら消し、読み直しが Unknown・作業中なら数えない（不変条件 4）。上限は当たる Policy の `budget.max_ineffective_applies`（1 以上）の最も小さいもので、どれも書かなければ 3。上限に達した周に止め（終了コード 4）、次の周からは当てない（Event `Halt`）。宣言を直すか、現実が宣言どおりになれば数え直す。

段 5 の決まり：

- 手順（`procedures:`、§3.7）は `target`（`type` と `id` か `select.labels`）・`trigger`（`on_drift` と `drift_fields`、または `manual`）・`steps`・`done_when`・`precondition`・`invariant`・`concurrency`（既定 1）・`timeout`・`on_failure: halt` を書く。段は `apply`・`invoke`・`wait`（`when` か `for`、`timeout`）・`manual`（`instruction`・`url`・`timeout`）・`gate`（`approval`・`timeout`）・`policy`（`select` と `mode: report`）のどれか 1 つと、`name`・`skip_if`・`critical`（apply・invoke だけ）。式・名前・項目の `{target}` は対象の名前、`{desired.項目}` は対象の宣言の値に置き換える。知らない操作・項目・式の誤り、done_when の無い manual、同じ対象の同じ項目を 2 つの手順が受け持つ宣言、申請者の分からない separation の gate は読み込みで断る。戻しの段（rollback）・`exec`・`disruption`・`trigger: {policy: …}`・`before_delete` は後の段。
- `drift_fields` は収束ループが比べも当てもせず（plan に「手順が受け持つ」と出す）、ずれていれば対象ごとに 1 回手順を起こす（E-F2・E-A5）。同じ対象に未完了の手順は 1 つ。`manual` の手順は `sakutei start`（`--as` が申請者）で起こす。
- 進み（`runs/<手順>/<対象>.json`）は周だけが書く。周ごとに、手順の timeout・done_when（真なら済んだ。不明は済んでいない）・invariant（真でなければ待つ）・skip_if を見て、段を進められるだけ進める。済んだ手順・取り消した手順の進みは消す（履歴は監査）。done_when が真の対象には手順を起こさない（Record を失っても二度走らない、不変条件 18）。
- 一回きりの操作（`invoke`、provider の `core.Invoker`）と critical の段は、送る前に送信中の記録を `runs/` に書く。前の周の記録が残っていれば（送る途中で止まった）送らずに手順を止め、続ける承認（`<run-id>-resume-<段>`。要件は手順の最初の gate と同じ、gate が無ければ申請者でない 1 人）を頼む。承認されればその段をもう一度送る。`core.ErrNotSent` なら記録を消して次の周に送り直し、ほかの誤りは送れたか分からないので止める（不変条件 16）。一回実行（`reconcile --once`）は critical の段の手前で止め、Event `ProcedureNeedsDaemon` を出す。進めるのは常駐だけ。
- 止まった手順の対象は、収束でも宣言どおりに戻さない（plan に「触らない」、E-A9）。続けるか `sakutei cancel <run-id> --reason …` で片づける。手順を書く段は当たる Policy の予算を周の変更と合わせて数え、`mode: report` の Policy（freeze を含む）に当たる対象の手順は起こさず、書く段を進めない。段 `policy` の report は手順の間だけ効く（次の周から）。
- 承認（Policy の `approval: {quorum: {n, of}, separation, timeout}` と `applicant`、手順の `gate`）：周が依頼（`approvals/<id>/request.json`。Policy の変更の ID は宣言と操作から決まる `chg-…`、gate は `<run-id>-<段>`）を書き、`sakutei approve`・`reject`（または承認ツール）が `approvals/<id>/by-<名前>.json` を書く。数えるのは周で、申請者・`of` に無い人・期限の後の判断は数えず、異なる人だけを数える。数える人が 1 人でも拒否すれば拒否（Policy の変更は止める、gate は手順を止める）。期限が切れれば頼み直す（gate は止める）。使う前に依頼に「使った」を書き、書けなければ当てない。使った依頼は使い回さない（不変条件 21）。CLI も、申請者・`of` に無い人・無い依頼・済んだ依頼の判断と、理由の無い拒否を断る。新しい判断・拒否・期限切れ・使った承認は監査に残す。
- 一時的な Policy（`sakutei freeze`、`temp-policies/<名前>.json`）は理由・期限・誰がを必須とし、当たる先（既定は全部）を `mode: report` にする。周が始まり（`freeze.start`）を監査に残し、期限で消し（`freeze.expire`）、`sakutei unfreeze` の印（`<名前>.json.end`）で終える（`freeze.end`）。効いている間は Event `Frozen`。読めない一時的な Policy・承認・進みがあれば全体を report に寄せる（不変条件 22）。
- 常駐は `approvals/`・`temp-policies/`・`commands/` の書き込みで次の周を始める。Event は `ApprovalPending`（承認待ち）・`Frozen`・`ProcedureNeedsDaemon`・`Waiting`（手順が待つ）・`Halt`（手順が止まった）・`ProcedureStep`（段を済ませた・済んだ・取り消した）を足した。一回実行の終了コードは、済んでいない手順・承認待ちを 3、止まった手順・拒否を 4 に数える。
- 性質のテスト（`cycle`）は race detector のとき（と `-short`）回数を減らす（make test-race で v3/cycle を 60 秒以内に）。

段 6 の決まり：

- 2 段の Apply（`apply.transaction`、§3.1・§3.3）：`group`（同じ名前の宣言を 1 つにまとめる）・`timeout`（既定 30s、2s 以上）・`confirm`（`phase: before|after` と `when` の Condition）。provider は `core.Transactor` を持つ（無ければ読み込みで断る）。
- `before` は判定（plan と同じ）で見る：通らなければ変えない（待つ）。group の 1 つが当てられない（待つ・承認待ち・report・止めた）なら group ごと待つ。group の中の依存は読み込みで断る。
- 周は単位（group か宣言）ごとに provider ごとに `Prepare` し（相手の側に timeout の後に戻す仕掛けを置いて当てる）、settle を待ってから timeout の半分まで 1 秒ごとに、全部の宣言を読み直して宣言どおりで全部の `after` が真かを確かめる（不明は通らない）。全部が通ったときだけ `Commit`。通らなければ `Commit` を呼ばずに `Rollback`（急ぐためだけ。呼ばなくても相手の時間切れで戻る）。1 つでも準備できなければ、準備したものを戻して確定しない（不変条件 17）。
- 確かめられず戻した宣言は、同じ宣言のまま当て直さない（効かない回数を上限にして止める、終了コード 4。毎周締め出して戻すのを繰り返さない、不変条件 13・14）。宣言を直すか現実が宣言どおりになれば数え直す。結果の `transaction`・`rolled_back` に出る。
- `FieldSpec.Digest`：観測は中身の sha256 だけで、比べるときは宣言も sha256 にする（平文を観測・plan に出さない、E-E2）。
- 実の provider はコアの外の Go モジュール `providers/`（`github.com/tjst-t/sakutei/v3/providers`）。それを組み込んだ CLI は `providers/cmd/sakutei`、決まった道具は `providers/cmd/sakutei-nft-guard`。詳しくは [providers/README.md](providers/README.md)。
- v2 の試用の手順（DNS と FW）は `docs/trial/v3/`（`run.sh` と、同じ手順を回す `providers/cmd/sakutei` の `TestTrial`）。本物のカーネルの 2 段の Apply は `make integration-v3-nftables`。

段 7 の決まり：

- Expand（§3.8）：周の段 2 で、intent の最上位の項目をそのまま読む `Passthrough` のあと、intent の `expand:` に書いた順に Expand を呼び、出力を足していく。`kind: go:<名前>` は `cli.WithExpander` で組み込んだもの（自分が読む intent の項目を `Keys` で言う）、`kind: exec:<名前>` はつなぎ方の設定の `expanders.<名前>.exec` のコマンド（読む項目は `keys:`。stdin に入力の JSON、stdout に intent と同じ形の出力と `diagnostics`。30 秒で時間切れ）。Expand は足す分だけを返し、前の宣言を変えるのは依存を足す `DependsOn` だけ。組み込まれていない Expand・同じ項目を 2 つが読む宣言・誰も読まない項目は読み込みで断る。外のコマンドが失敗すればその周は読み込めない（終了コード 1、常駐は健全性に残して回り続ける）。
- 診断（§3.8）：`Reject` は読み込みで断る（終了コード 2）、`Warn` は知らせる、`Uncertain` は Target の宣言をその周 Unknown にする（観測を試みず、Apply も消しもせず、下流は待つ。不変条件 3・5）。Target が種類（`<provider>/<種類>`）か provider の名前なら、宣言が欠けているかもしれないので、その provider の範囲の宣言外を消さない（「消さない」で止める、E-K3・不変条件 26）。
- 外部入力（E-K3・E-P4）：intent の `inputs: [{name, items, expected_min, max_drop}]` で使う入力と欠けの目安を、つなぎ方の設定の `inputs.<名前>.file` で在りか（最上位がマップの YAML か JSON）を書く。件数は `items` のリストの長さ（書かなければ最上位の項目の数）。読めない・形が違う・`expected_min` より少ない・前の周の件数（Record の目印 `input_counts`）から `max_drop` を超えて減ったものは「届かない」（`Reachable: false` と理由）で、Expand はそれに依る宣言を作らず Uncertain を出す。欠けた周は前の件数を保つ。
- golden のテスト（N-P5）：`expand/expandtest.Golden` は `testdata/<場合>/intent/` と任意の `inputs.json` で Expand を 2 回呼び、同じ出力（純粋さ）と `golden.json` を確かめる（`go test -update` で書き直す）。Go の Expand も外のコマンドの Expand も使える。
- 採番（§3.8、N-B13・N-B12・N-A14）：intent の `pools: [{name, range: "100-199", blocks: [{name, range}], reuse}]`。宣言の項目に `{from_pool: <pool>, block: <名前>, template: "192.0.2.{n}"}` と書くと、intent のファイル `allocations/<pool>.yaml`（`allocations: {pool, values: [{key, value}], last: [{block, value}]}`、鍵は `<type>/<id>#<項目>`）の値で埋め、無ければ空きの先頭（`core.Allocator`、`cli.WithAllocator` で差し替え）を選ぶ。`template` があれば `{n}` を値にした文字列、無ければ整数。plan は選ぶだけで書かない。周はロックを持って当てる前に、ファイル（先）と Record の写し `alloc/<pool>.json`（後）に書き足し、監査に `alloc.assign` を残す。書けない・周の間にファイルか写しが変わったときは、その値に依る宣言をその周 Unknown にする。
- 二度払わない（不変条件 11）：値は宣言から消えても返さない（返すのは `sakutei alloc release <pool> <value> --reason …`。Record のロックを取ってファイルと写しの両方から外し、監査に `alloc.release`）。`reuse: false` のプールは block ごとの最後の値（`last`、ファイルにも書く）以下を払わないので、返した値も Record を失っても二度と払わない。ファイルが写しより戻っている（写しにある鍵が無い・値が違う・`last` が小さい。git で巻き戻した・手で消した）なら、そのプールから新しい値を出さず、それを要る宣言を Unknown にする（終了コード 3）。ファイルが写しより進んでいる（人が書き足した）のは受け入れて写しを書き直す。尽きた block は理由つきで止める。同じ値を 2 つに払うファイル・範囲の外の値は読み込みで断る。
- 秘密（§3.1・§3.8、E-G2・E-G3・E-G4・N-G2）：Schema の型 `secret` の項目は `{secret: "<backend>:<在りか>"}` で書く（平文は読み込みで断り、値を診断に出さない）。保管先はつなぎ方の設定の `secret_backends.<名前>.module`（組み込みは `file`。`vault-kv2`・`command` は providers、ほかは `cli.WithSecretBackend`）。周の初めに参照ごとに世代だけを読み（値は読まない、10 秒で時間切れ）、宣言の値を `<backend>:<在りか>@gen=<世代>` にする。比べるのはこの参照だけで、provider は当てた世代の参照を観測に返す（世代が変われば当て直す）。世代を読めない秘密に依る宣言はその周 Unknown（観測も Apply もしない、下流は待つ、E-G4）。値は provider が Apply の直前に `WriteContext.Secret` でだけ取り出す（Apply・Delete・Prepare・Invoke の書き込みの文脈だけが保管先を持つ）。値は Record・出力・state・誤りの文に出ない（不変条件 27）。
- Expand のモジュール（`providers/expanders`）：`go:template`（雛形を一覧の 1 件ごとに）・`go:netpolicy`（網の間の許可を nftables の base chain に）・`go:compat`（互換の表）。詳しくは [providers/README.md](providers/README.md)。

段 8a の決まり：

- 送り先（つなぎ方の設定の `sinks.<名前>`）に `streams: [events, audit]` を書ける（書かなければ `events` だけ、E-H2・E-L1）。`audit` は監査の鎖（`audit/`）を Record に書いたままの行で送り、位置は `events/cursors/audit/<名前>.json`。送れなければ位置を保って次の周に同じ位置から送り直す（届かない間は Record に貯まる。監査は消さない）。監査を送れないモジュール（`core.AuditSink` を持たない）に `audit` を書けば読み込みで断る。送るのはロックを持つ周だけ。
- `core.StateSink` を持つ送り先には、送り出しの列のあとに周ごとに今の firing の一覧（`events/state.json`）も渡す。続いている状態を送り直す送り先（Alertmanager の endsAt・metrics のゲージ）が使い、常駐を起こし直しても一覧から送り直せる。
- どの送り方も時間を区切って呼び（`events.DeliverTimeout`、既定 10s）、送れない・戻らない・panic でも周を止めない（不変条件 28）。周の結果の `deliveries` に送り先ごと（監査は `stream: audit`）の結果が出る。
- 送り先のモジュール（`providers/sinks`）：`alertmanager`・`syslog`・`metrics`・`otlp`。承認ツール `sakutei-approve`（`providers/cmd/sakutei-approve`）。詳しくは [providers/README.md](providers/README.md)。
- `sakutei approve`・`reject` と承認ツールは同じ確かめ（`approval.Decide`）を通す：申請者・要件の `of` に無い人・無い依頼・待っていない依頼への判断と理由の無い拒否は書く前に断る（不変条件 21）。

段 8b の決まり（redfish・metrics-probe と契約テスト）：

- 契約テスト（§6.2、E-P2）：`sdk/contract.Run(t, newTarget)` は確かめごとに新しい `Target`（provider と偽の相手・模擬）を作り、書かなかった確かめは飛ばす。届かない故障（接続を切る・5xx・時間切れ・認証の失敗・壊れた出力）で Unknown 以外を返す・宣言どおりなのに 2 回目の Apply が相手に書く・観測の項目が Schema に無いか正規形でない・観測で書き込み用の資格を使う・誤りの文や Note や Fields に秘密の値が出る・送れたのに `ErrNotSent`（送れないのに返さない）・断る削除で相手に書く、を落とす。偽の provider でも回す（`sdk/contract/contract_test.go`）。
- 型 `float`（`core.Float`、E-H4・N-T4）：小数の値（/metrics の値・メモリの量）。Condition の大小で比べる。NaN・±Inf は型に合わない。
- provider `redfish`・`metrics-probe` は `providers/`（README に設定と intent の例・統合テスト）。

段 8c の決まり（netconf・cli-device と Transaction の契約）：

- 契約テストの Transaction の戻り（§6.2、E-B3・E-B8・不変条件 17）：`sdk/contract.RunTxn(t, newTarget)` は確かめごとに新しい `TxnTarget`（Transactor を持つ provider・準備する宣言・時間切れ・時間切れを過ぎるまで待つ `Expire`）を作り、準備した変更が効く・確定すれば時間切れを過ぎても戻らない・Commit も Rollback もしなければ時間切れのあとに元に戻る・Rollback で戻る、を確かめる。偽の provider でも回す（`sdk/contract/txn_test.go`）。
- provider `netconf`・`cli-device` は `providers/`（README に設定と intent の例・netopeer2 の統合テスト `make integration-v3-netconf`）。

段 8d の決まり（proxmox・guest-os とコアの 2 つの口）：

- `FieldSpec.Match`（§3.3、E-I7・E-D1・E-C2）：宣言と観測の比べ方を provider が決める項目。plan も、Apply のあとの読み直しも、契約テストも同じ比べ方を使う。観測に無い項目でも Match が等しいと言えば差分にしない（作るときだけ使う項目。作るときは Apply に渡す）。
- `core.Classifier`（§3.3、E-C2・E-B6・E-C1）：provider が作る・変える変更ごとに `Hint`（壊す・承認が要る・理由）を言う。壊す変更は予算の壊す変更に数え、承認が要る変更は承認が揃うまで当てない（当たる Policy に approval が無ければ、申請者でない 1 人の承認を頼む。`sakutei approve` で揃える）。理由は plan の `provider:` の行と JSON の `classified`・`needs_approval` に出る。
- provider `proxmox`（種類 `vm`・`fw-rules`・観測だけの `volume`・`fw-rule`、network_policy の実現先 `proxmox-fw`）と `guest-os`（種類 `os`、観測だけ）は `providers/`（README に設定と intent の例・統合テスト）。

テストはルートの `make check`・`make test-race` が一緒に回す（`providers/` も）。
