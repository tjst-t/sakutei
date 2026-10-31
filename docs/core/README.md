# Sakutei の新しいコア 設計書

2026-10-31。v2.12.0（main d28dc4a）までの Sakutei を、コアからゼロで作り直すための設計書。もとにした検討は `docs/review/core-reduction-draft.md`（たたき台）と `docs/review/core-usecase-review.md`（303 件のユースケースの検討。本書の `E-…`・`N-…` の ID はこの文書の ID）。互換は持たない。v2 のユースケースは全部満たす。

## 要約

- **何を作るか。** オンプレの基盤（BMC・ネットワーク・FW・ストレージ・VM・ゲスト OS・DB）を宣言どおりに保つ収束エンジン。土台の考え方は v2 と同じ 5 つの原則（毎周見て直す・確かでなければ消さない・壊す変更は数えて止め承認させる・いつは収束でどうは手続き・正しい状態は宣言に置く）。
- **コアの中身は「1 周だけ回す処理」1 つ。** 周をまたいで覚えるもの（手順の進み・承認・前の周の目印・通知の状態）は全部ディスクの Record に置き、メモリには持たない。だから cron で一回実行を回しても、常駐で回しても同じ結果になる。常駐はその処理を繰り返す薄い外側で、足すのは「承認の待ち受け」「読み取り HTTP」「すぐ次の周を始める起床」の 3 つだけ。標準の使い方は常駐。
- **部品は 9 つ。** Resource（宣言 1 件）・Range（管理する範囲）・Provider（装置とのやりとり）・Condition（観測についての三値の式）・Graph（順序と待ち）・Policy（いつ・どこまで変えてよいか）・Procedure（一回きりの手順）・Expand（intent から Resource を作る純粋な変換）・Record と Events（周をまたぐ記録と外への知らせ）。
- **コアの外に出すもの。** provider 全部、Expand の実装（網の許可の変換・互換の表など）、通知の送り先（Alertmanager・syslog・OTLP・metrics）、import と同期の道具、承認の HTTP 道具。コアはこれらのインターフェースだけを持つ。provider 固有の起動の引数は持たない。
- **設定の置き場所は 2 つ。** intent（何を・範囲・方針）と、つなぎ方の設定ファイル 1 つ（接続先・資格・待ち受け）。起動の引数は 6 つ。
- **やめるもの。** シャードと claim、fencing、git のミラー、DR モード、影響範囲だけの早い収束、BulkObserve の専用インターフェース、原因タグ A〜E、運用プロファイルの型、採番の外部権威と予約、runtime の役と締め緩めの規則、Procedure の定期起動。どれも「戻す目安」と「戻すときの最小のインターフェース」を §1.4 に残す。
- **規則。** 本書の §3 に載せる項目には、必ずそれを要求するユースケースの ID を付けた。ID を付けられない項目は入れない。機能が再び増えすぎるのを止めるための規則で、今後の改訂でも守る。

---

## 1. 目的と原則

### 1.1 目的

宣言（intent）に書いたとおりの状態を、毎周、現実を観測し直して保つ。手で変えられたら戻し（または知らせ）、確かでないときは何もしない。一回きりの危険な手順は、承認と観測で区切った手順として安全に進める。

v2 は 303 件のユースケースのほとんどを満たしたが、同じ考えが別々の機能として何度も入り（止める・緩める系が 10、観測で待つ系が 9、2 段で当てる系が 6）、設定の置き場所が 4 つに分かれ、provider がコアの中にいた。新しいコアは、同じユースケースを少ない部品の組み合わせで満たす。

### 1.2 5 つの原則

| # | 原則 | コアでの意味 |
|---|---|---|
| P1 | **毎周見て直す・観測を記録しない** | 毎周 desired と現実を出し直す。観測した値は周をまたいで持ち越さない（Record にも置かない）。前の周の値を正としない |
| P2 | **確かでなければ消さない** | 観測の答えは「ある・無い・分からない・作業中」の 4 値。「分からない」は「無い」ではない。消すのは範囲を宣言し、List が完全で、宣言に無く、外の持ち主でなく、予算と承認を通ったときだけ |
| P3 | **壊す変更は数えて止め、承認させる** | 壊す変更（削除・壊す項目の変更・provider が壊すと言った変更）は予算に数え、超えたら止める。承認が要るものは承認があるまで流さない |
| P4 | **「いつ」は収束、「どう」は手続き** | 収束ループは「いつ・どの順で」を決める。非冪等な操作や多段の作業は Procedure に閉じ込め、ループには載せない |
| P5 | **正しい状態は宣言に置く** | 範囲・除外・方針・採番の結果は intent に置く。ツールの中（Record・設定）にしか無い正しい状態を作らない。Record は進みと記録だけ |

### 1.3 持たないもの

| やめるもの | 満たしていたユースケース | 新しいコアでの受け皿 |
|---|---|---|
| claim とシャード・git のミラー・S2-free | E-N3・E-N4・E-K6・E-K9・N-N3・N-K6 | 範囲を分けた複数の常駐（それぞれ別の intent と Record）＋相手の現実を Condition で待つ。ミラーは git の外の道具。Record を揮発の場所に置けば S2-free と同じ |
| fencing（二重の書き手の守り） | E-K8・N-K7 | 1 つの intent の中で担当範囲が重なる宣言は読み込みで断る。同じ Record を共有する常駐はロックを取った 1 つだけが書く |
| DR モード | E-K7・N-K4・N-Q6 | 別の `--policy-set` で起動、DR 用の intent は Expand の入力で選ぶ。署名と合流は git の道具。コアは監査に intent の版を残すだけ |
| 影響範囲だけの早い収束（scoped reconcile） | E-N5・E-M6・N-M6 | 起床（次の周を今すぐ全部回す） |
| BulkObserve の専用インターフェース | E-D8・N-D11 | `Observe` を ID の一覧で呼ぶ形にする。prune の根拠は List の ID だけ |
| 観測の tier | E-N2 | Policy の任意の項目 `observe_every`（既定は毎周全部） |
| 原因タグ A〜E・last-seen | E-J13・E-J12 | 原因は 3 つ（宣言・外部入力・現実のずれ）。provider の版が変わった周は止める |
| 容量の門・振動の検知・互換の表の照合 | E-C4・E-J6・E-K10・E-A7 | Condition（`on_unknown: block`）・Policy の `max_ineffective_applies`・Expand |
| 運用プロファイル（solo/team/enterprise）の型 | E-J10 | Policy の承認の既定値の組を例として文書に置く（コアの型にしない） |
| 採番の外部権威・Reserve/Confirm・キャッシュ・TTL | E-K4・E-T4 | intent のファイルが正。コアは空きを選んで書き足すだけ。選び方は差し替えられる |
| runtime の役・daemon/oneshot の別の値・締め緩めの規則 | E-J11・E-M5・E-S5 | Policy と一時的な Policy と `--policy-set`。一回実行と常駐で値を分けない |
| Procedure の定期起動（every:） | N-J10・N-C4・N-C5 | 「新しい結果がある」を Resource の古さとして観測する（§3.7 の I15 の書き方） |
| provider の設定の除外（安全の網） | E-D2 | 除外は intent の Range にだけ書く |
| 承認の HTTP をコアに持つこと | E-M4・E-L3 | CLI と外の小さな承認ツールが Record に書く |
| 起動のフラグ 128 個・provider 固有のフラグ 13 個 | E-S1 | 6 つ（§5.5） |

### 1.4 戻す目安

大規模向けの機能は「いまの使い方では効かない」から外した。次の目安に当たったら戻すことを検討する。戻すときは下の最小のインターフェースだけを足し、コアの 1 周の形は変えない。

| 機能 | 戻す目安（どれか 1 つ） | 戻すときの最小のインターフェース |
|---|---|---|
| 観測の一括取得 | 1 つの provider の観測が周の間隔の 3 割を超える／API の呼び出しに課金がある | すでに `Observe(ids)` が一覧を受けるので、provider の中で一括 API を使うだけ。コアの変更なし |
| 観測の間引き | リソースが 1 万件を超える／遅い BMC が 100 台を超える | Policy の `observe_every`（初回・未収束・desired が変わった周は必ず観測） |
| 影響範囲だけの収束 | 秒単位の反映が契約上の要求になった（テナントのセルフサービス） | Graph に「出力の差分から下流を辿る」関数。予算は全体と共有、その経路では prune しない |
| シャード | 拠点が 3 つ以上で、1 つの常駐から全部に届かない | Range ごとに常駐を分ける起動の引数（どの Range を担当するか）。intent は 1 つのまま |
| fencing | 同じ範囲を 2 つの常駐が持つ引き継ぎが日常になった | Record に範囲ごとの世代を置き、Apply の前に確かめる任意のインターフェース |
| 原因の細かい分類 | 「波及」「ツール更新」を分けないと運用の判断ができない事例が 3 件以上 | Expand の出力を Resource ごとにハッシュして前の周と比べる（Record の目印に足す） |
| 採番の外部権威 | NetBox などを正にする現場が出た | `Allocator.Pick` の差し替え（すでにある） |
| DR モード | 主拠点の全損を機械で切り替える要求が出た | 署名つきの一時的な Policy を Record に置く道具 |

---

## 2. 1 周の流れ

### 2.1 段階

| 段 | すること | 失敗したとき |
|---|---|---|
| 0 ロック | Record のロック（flock）を取る | 取れなければ「進めない周」：観測して自分の status と読み取り HTTP に出すだけ。Apply・Delete・手順の前進・Record への書き込み・sink への送り出しはしない（それはロックを持つ側がする）。status に `AdvancedElsewhere` |
| 1 読み込み | intent・つなぎ方の設定・Record（一時的な Policy・目印・手順の進み・承認・通知の状態）を読む。外部入力と秘密の世代を読む | intent か設定が読めない・形が誤り → その周は何もしない（終了コード 2）。Record が読めない → 全体を report に寄せる。外部入力が届かない → Expand に「届かない」と伝える |
| 2 Expand | Expand を順に呼び、Resource・Range・Policy・Procedure・採番の要求・診断を得る。出力を検証する（同じ ID・未知の項目・循環・範囲の重なり・Schema に無い項目）。採番の要求があれば空きを選んで intent のファイルに書き足し、Expand をもう 1 回だけ呼ぶ | 検証で断ったら何もしない（終了コード 2）。診断で unknown にされた Resource は観測も Apply もしない |
| 3 観測 | provider ごとに `Observe(ids)` と、Range ごとに `List` を呼ぶ。provider の版を Record の目印と比べる | 呼び出しの失敗はその ID を unknown。List の失敗はその範囲を「不完全」 |
| 4 比べる | 宣言に書いた項目だけを比べ、操作（create・update・delete・prune・なし）を決める。原因（宣言・外部入力・ずれ）を目印のハッシュと比べて付ける | unknown・converging の Resource は操作なし |
| 5 方針の判定 | 操作ごとに当たる Policy を合わせ（最も厳しいものが勝つ）、mode・窓・予算・承認・別枠を判定する。Graph で順番（wave）を決める。Procedure の起動条件を判定する | 予算を超えたら超過分の保留でなく全体を止める（halt）。承認待ちは Event に出して止める |
| 6 当てる | wave ごとに Apply・Delete を並べて呼ぶ。Transaction を持つ provider は 2 段で当てる。Procedure を 1 段ずつ進める | 失敗はその Resource だけ。下流は同じ周に進めない |
| 7 確かめる | settle を待って読み直し、効いたかを見る。効かなければ回数を数える。上流が効いたら、観測できなかった下流を同じ周にもう一度観測して当てる（上限つき） | 効かない回数が上限に達したら止める |
| 8 記録と通知 | 監査を鎖で追記、目印・手順の進み・通知の状態・健全性を Record に書く。状態の変わった Event を送り出しの列に積む。ロックを放す | Record に書けなければ、その周に開いた別枠・承認を使わない（発射前に書く） |

`plan` は段 1〜5 を実行し、何も書かず何も消費しない（予算・別枠・承認を減らさない）。`status` は段 1〜4 の結果と Record の状態を出す。

### 2.2 一回実行と常駐

- **コアは `RunCycle(ctx, in) → CycleResult` 1 つ。** 一回実行（`sakutei reconcile --once`）はこれを 1 回呼んで終わる。常駐（`sakutei`）はこれを繰り返す。
- **周をまたいで覚えるものはすべて Record に置く。** 手順の進み・送信中の記録・承認・一時的な Policy・前の周の目印（provider の版・範囲ごとの件数・Expand の入力と出力のハッシュ・効かない回数・予算の窓の使用量）・通知の状態（firing の一覧と count・since）。メモリには置かない。だから cron で 5 分ごとに一回実行を回すのと、間隔 5 分の常駐は同じ結果になる（E-O3・N-O4）。
- **常駐が足すのは 3 つだけ。**
  1. 承認の待ち受け：Record の承認の置き場に書き込みがあったら起きる（承認は CLI か外の承認ツールが Record に書く。常駐は承認を受け取る HTTP を持たない）。
  2. 読み取り HTTP：`/status`・`/plan`・`/events`・`/graph`・`/health`。
  3. 起床：`sakutei wake`・SIGHUP・intent のディレクトリの変更で、待ちを切り上げて次の周を始める。
- **一回実行は `critical` の段を進めない。** 手順の中の危険な段（書き込み中に止まると壊れる段）は、止まったときに見張る者がいる常駐だけが進める。一回実行はその段の手前で止め、Event `ProcedureNeedsDaemon` を出す（検討文書 §6-14 の決定）。
- 標準の使い方は常駐。一回実行は、作業者の PC から収束させたいとき（段0-zero）と CI での確かめに使う。

---

## 3. 9 つの部品

表の「ユースケース」の列は `docs/review/core-usecase-review.md` の ID。

### 3.1 Resource（宣言 1 件）

**役割。** 1 つの対象について「こうあってほしい」を書く。比べるのは宣言に書いた項目だけ。

```yaml
resources:
  - type: dns/record                 # <provider のインスタンス名>/<種類>
    id: www.example.com./A
    labels: {site: a, owner: platform}
    fields: {value: 192.0.2.10, ttl: 300}
    reason: "CHG-1234"               # 監査と Event に載る
  - type: pve/vm
    id: old-vm
    ensure: absent                   # あれば Delete。壊す変更として予算と承認に通す
  - type: fw/chain
    id: fw1/inet/filter/input
    fields: {type: filter, hook: input, priority: 0, policy: drop, rules: ["jump mgmt", "tcp dport 443 accept"]}
    depends_on:
      - id: bmc/system/fw1
        when: observe(bmc/system/fw1).power == "On"
    apply:
      transaction:
        group: edge-wave-1
        timeout: 30s
        confirm:
          - {phase: before, when: 'observe(probe/tcp/fw1:22).reachable == true'}
          - {phase: after,  when: 'observe(probe/tcp/fw1:22).reachable == true'}
          - {phase: after,  when: 'check(bgp-up)'}
```

```go
type ID struct{ Type, Name string } // Type は "<provider>/<kind>"
type Fields map[string]any          // 宣言した項目だけ。秘密は SecretRef（値を持たない）

type Resource struct {
    ID        ID
    Ensure    Ensure            // Present | Absent
    Fields    Fields
    Labels    map[string]string
    DependsOn []Dependency      // {On ID; When Condition; OnUnknown Disposition}
    Apply     *ApplyOptions     // {Transaction *TxnSpec}
    Reason    string
    Origin    Origin            // 宣言のファイルと行
}
```

| 項目 | 内容 | ユースケース |
|---|---|---|
| `type`・`id` | 種類と ID。intent 全体で一意（重複は読み込みで断る） | E-M3・N-M3 |
| `fields` | 宣言した項目だけを比べる。宣言に無い項目・相手が動かす項目（配置・動的 IP・機器が付ける時刻）は比べない | E-D7・E-B7・N-I2・E-B14・E-A10 |
| `fields` の秘密 | `{secret: "vault:/db/app#password"}`。値は持たず、世代だけを比べる | E-G2・N-G2・N-E4 |
| `labels` | Policy の当たり先と Event の振り分け先 | E-M1・E-M2・N-M5 |
| `ensure: absent` | 名指しで消す。壊す変更として予算と承認に通す | E-D3・N-A9 |
| `depends_on` | 順序と待ち（§3.5） | E-J7・E-B13・E-I5 |
| `depends_on[].when`・`on_unknown` | 観測で満たすまで待つ。分からないときの扱い | E-C4・N-C8・N-J5・N-H6 |
| `apply.transaction` | 2 段で当てて確かめる（§3.3 の Transaction） | E-B3・E-B8・N-B2・N-D5 |
| `apply.transaction.group` | 同じ group は全部で確かめが通ったときだけ確定 | E-B9・N-B3・N-B14 |
| `confirm[].phase` | `before`（変える前に確かめ、だめなら変えない）・`after`（変えたあとに確かめ、だめなら確定しない） | E-B3・N-B9 |
| `reason` | 変更の理由を監査と Event に出す | N-J7・N-L8 |

### 3.2 Range（管理する範囲）

**役割。** 「この provider のこの集合はこちらが管理する」という境界を 1 か所に書く。List の引数、宣言外を消してよいか、触らないもの（外の持ち主）をまとめる。v2 の `providers:`・`external_owners:`・`deletion_policy`・`--claim` を置き換える。

```yaml
ranges:
  - provider: dns
    args: {zone: example.com.}          # provider が List に使う引数（zone・tables・hosts・base_dn・scope…）
    labels: {site: a}                   # この範囲の現実に付けるラベル（Policy の当たり先）
    prune: true                         # 宣言に無いものを消してよい（既定 false）
    max_drop: 50%                       # 前の周より List がこれ以上減ったら、この範囲は消さない
    external_owners:
      - {owner: dhcp, id_prefix: "dyn-", reason: "DHCP が登録する"}
  - provider: pve
    args: {cluster: pve-a}
    external_owners:
      - {owner: bootstrap, id: "vm/100", reason: "段0 の VM"}
```

```go
type Range struct {
    Provider       string
    Args           map[string]string
    Labels         map[string]string
    Prune          bool
    MaxDrop        Percent           // 既定 50%
    ExternalOwners []Exclusion       // {Owner; ID; IDPrefix; Fields; Reason}
}
```

| 項目 | 内容 | ユースケース |
|---|---|---|
| `provider`・`args` | List の引数。範囲は intent にだけ書く（設定には書かない） | E-B2・E-I6・E-G1・E-B4 |
| `labels` | 宣言外のものにも Policy を当てるためのラベル | E-D4・N-I3 |
| `prune` | 宣言外を消してよいか。既定は消さない。壊す型は prune でも承認が要る | E-C3・N-C2・N-F3・N-T1 |
| 宣言外の報告 | prune しない範囲も List して、宣言外（unmanaged）を status に出す。List できない provider は「検出できない」と出す | E-D4・E-B6・N-I3・N-A7 |
| `max_drop` | List の急減で消さない。自分が消した分と別枠で消した分は差し引く | E-K2・N-K2 |
| `external_owners` | 触らない。観測して見せ、待ちには使うが、Apply・Delete・prune はしない。除外は intent にだけ書く | E-D2・E-I5・E-T2・N-T2・E-O1・N-O3 |
| 範囲の重なりの拒否 | 同じ provider の Range が重なる宣言は読み込みで断る | E-K8・N-K7 |
| 範囲の外の宣言の拒否 | Range がある provider で、どの Range にも入らない Resource は断る（provider の `Validate` が判定） | E-I6・E-M3 |

### 3.3 Provider（装置とのやりとり）

**役割。** 1 つの種類の装置について、観測・変更・削除・列挙を行う。コアの外のモジュールで、コアは下のインターフェースだけを知る。

```go
type Availability int // Present, Absent, Unknown, Converging

type Observation struct {
    Availability Availability
    Fields       Fields      // Present のときだけ。正規形にして返す
    Note         string      // Unknown・Converging の理由
}

type Provider interface {
    Schema() Schema
    Observe(ctx context.Context, ids []ID) (map[ID]Observation, error) // 返さなかった ID は Unknown として扱う
    Apply(ctx context.Context, w WriteContext, id ID, desired Fields, current Observation) error
    Delete(ctx context.Context, w WriteContext, id ID, current Observation) error // 断るときは *RefuseError{Reason}
    List(ctx context.Context, r Range) (Listing, error)                       // {IDs []ID; Complete bool}
}

type Schema struct {
    Version string                  // 比べ方・正規形が変われば上げる
    Kinds   map[string]KindSchema
}
type KindSchema struct {
    Fields      map[string]FieldSpec // {Type; Identity bool; Destructive DestructiveOn}
    Settle      time.Duration        // Apply のあと読み直すまでの待ち
    ObserveOnly bool                 // 観測だけ。Apply・Delete を呼ばない
    DestructiveDelete bool           // 中身を失う種類（volume・DB・バケット）
}
type FieldType int // String, Int, Bool, Duration, List(順序あり・全置換), Set(順不同), Map, Secret

// 任意のインターフェース
type Transactor interface {
    Prepare(ctx context.Context, w WriteContext, changes []Change, opt TxnOptions) (Txn, error)
}
type Txn interface {
    Commit(ctx context.Context) error
    Rollback(ctx context.Context) error // 任意。呼ばなくても相手の時間切れで戻ること
}
type Invoker interface {
    Ops(kind string) map[string]OpSpec                                    // {Destructive bool}
    Invoke(ctx context.Context, w WriteContext, id ID, op string, args Fields) error // 送れていなければ ErrNotSent
}
type Classifier interface {
    Classify(id ID, current Observation, desired Fields) []Hint            // {Field; Destructive; NeedsApproval; Reason}
}
type Validator interface {
    Validate(res []Resource, ranges []Range) []Diagnostic
}
type DesiredObserver interface {
    ObserveWith(ctx context.Context, id ID, desired Fields) (Observation, error)
}
```

| 項目 | 内容 | ユースケース |
|---|---|---|
| `Observe` の 4 値 | Present・Absent・Unknown・Converging。届かない・読めない・覗けない・定義に合わない出力は Unknown | E-K1・N-K1・E-A12・E-E6・E-B11 |
| `Converging` | 相手が作業中（保留中の BIOS・再起動待ち・非同期の仕事）。重ねない・消さない・効かない回数に数えない | E-A4・E-D1・E-B14・E-A1 |
| `Observe(ids)` を一覧で | provider が一括 API を使える。結果は prune の根拠にしない | E-D8・N-D11・N-N1 |
| `Apply` | 冪等。List 型の項目は全置換 | E-B12・E-D1 |
| `Delete` を分ける | 断る（中身がある・子がある・keep の印がある）ときは理由を返す。コアは消さずに報告を続け、予算に数えない | E-F1・E-G1・E-C2・N-C2・N-F3 |
| `List` と `Complete` | 不完全なら（ページの打ち切り・権限不足）その範囲を消さない | E-K2・E-F3 |
| `WriteContext` | Apply・Delete・Invoke・Prepare のときだけ渡す。provider は書き込み用の資格をこの中でだけ取り出す | E-G5・N-G3 |
| `Schema.Version` | 版が変わった周を検出する | E-J12・N-S1・N-P3 |
| `FieldSpec.Identity` | 変われば作り直しになる項目。物理の座標を選べるようにする | E-A10・N-A8 |
| `FieldSpec.Type` の List・Set | 順序のある規則は全置換、集合は順不同で比べる | E-B12 |
| `FieldSpec.Type` の Secret | 値を比べず世代で比べる。表示は伏せる | E-G2・N-L5 |
| `FieldSpec.Destructive` | create・update・delete のどれで壊すか。壊す変更は予算と承認 | E-C1・E-A1・N-D8・N-G10・N-C4 |
| `Settle` | Apply のあと読み直すまで待つ | E-J6・E-R3 |
| `ObserveOnly` | 観測だけの種類（在庫・operational の状態・golden config） | E-B7・E-E4・N-B5・N-B6・N-A7 |
| `DestructiveDelete` | 宣言から消えても既定で消さない種類 | E-C3・N-C2 |
| `Transactor` | 準備 → 確かめ → 確定。確定を呼ばなければ相手の時間切れで戻る | E-B3・E-B8・E-B9・N-B2・N-D5・N-K9 |
| `Invoker` | 観測に残らない一回きりの操作（boot_once・reset・migrate・drain・upgrade）。Procedure からだけ呼ぶ | E-A2・E-B11・N-D4・N-E2・N-A11 |
| `Classifier` | 状況で壊す変更（volume の付け替え・base chain の作成・保持の設定の変更）を計画のときに言う | E-C2・E-B3・E-C1 |
| `Validator` | 読み込みのときに provider 固有の誤りを断る（属性の登録簿・範囲の外・確かめる先の不足） | E-I7・E-A4・E-B3 |
| `DesiredObserver` | 宣言を見ないと観測できない縮退（Ansible の check mode・雛形ファイル）。この provider は List と prune を持てない | E-E3・E-E5・N-E5 |

### 3.4 Condition（観測についての三値の式）

**役割。** 依存の待ち・手順の前提・done_when・確かめ・不変条件を 1 つの式で書く。値は真・偽・不明の三値。

**語彙（これ以上は持たない）。**

```
式      := 論理和
論理和  := 論理積 ("or" 論理積)*
論理積  := 否定 ("and" 否定)*
否定    := "not" 否定 | 比較
比較    := 項 (演算子 項)?            演算子: == != < <= > >= in contains
項      := 定数 | 参照 | 量化 | "(" 式 ")"
参照    := observe(<type>/<id>).<項目>(.<項目>)*
         | present(<type>/<id>) | absent(<type>/<id>)
         | desired.<項目>               （手順の対象の宣言）
         | it.<項目>                    （量化の中の 1 件）
         | check(<名前>)                （Go で登録した確かめ）
量化    := all(<選び方>, 式) | any(<選び方>, 式) | count(<選び方>, 式)
選び方  := <type>[<ラベル>=<値>, ...]
```

算術・文字列の加工・現在時刻は持たない。時間に依る判断は、provider が「残り時間」「最後の成功からの経過」を観測の値として返して表す（例：`observe(pki/cert/www).remaining < "720h"`）。

**三値の決まり。**
- 参照が Unknown・Converging の対象を指すと、その参照は不明。不明を含む比較は不明。
- and・or・not は三値の論理（偽 and 不明 = 偽、真 or 不明 = 真）。
- `all` は 1 件でも偽なら偽、偽が無く不明があれば不明。`any` は 1 件でも真なら真。
- `count(...) op N` は、真の件数 k と不明の件数 u から、k だけで決まれば真偽、k〜k+u のどこかで結果が変われば不明。

**不明のときの扱い**は使う側が書く：`on_unknown: wait`（待つ。既定）・`block`（止める。容量の門）・`ignore`（真とみなす。報告だけの条件）。Transaction の確かめと手順の done_when では、不明は常に「満たしていない」。

```go
type Tri int // True, False, Unknown
type Condition interface {
    Eval(env Env) Tri          // env は今の周の観測だけを引く。外へ問い合わせない
    Refs() []ID                // 観測すべき対象（コアが先に観測する）
}
type Disposition int // Wait, Block, Ignore
type CheckFunc func(ctx context.Context, obs ObservationView) (Tri, string)
```

| 項目 | 内容 | ユースケース |
|---|---|---|
| `observe(...).項目` と比較 | 観測した値で待つ・確かめる | E-A3・E-B13・E-H4・N-B17・N-T4 |
| `present`・`absent` | 在る・無いで待つ | E-A8・E-I5 |
| `desired.項目` | 手順の対象の宣言と観測を比べる（done_when） | E-A5・E-A6・E-F2 |
| `check(名前)` | 式で書けない確かめ（相手の台から見た経路）を Go で登録 | E-B9 |
| `all`・`any`・`count` | 集合の上の条件（MTU がそろう・primary が 1 つ・95 台以上が健全） | N-B16・N-F4・N-B18・N-J3 |
| 三値と `on_unknown` | 不明を偽とも真とも決めず、使う側が選ぶ | E-C4・N-C8・E-K1・N-H6 |
| 時刻を読まない | 期限は provider が残り時間を返す | N-G4・N-R2・E-I4・N-J10 |
| 観測だけを読む | Eval は今の周の観測だけを見る。周をまたいだ値を読まない | E-J9（plan と reconcile が同じ判定） |

### 3.5 Graph（順序と待ち）

**役割。** `depends_on` と Condition から、周の中の順番（wave）と、周をまたぐ待ちを決める。手順の段の並びも同じ Graph で扱う。

| 決まり | 内容 | ユースケース |
|---|---|---|
| wave | 依存先を先に当てる。循環は読み込みで断る | E-J7・E-M3 |
| 待ち | `when` が真になるまで下流は操作なし（Event `Waiting`）。偽でも不明でも待つ（`block` のときは止めて知らせる） | E-I5・E-B13・E-C4 |
| 同じ周の観測し直し | 上流の Apply が効いたら、観測できなかった下流を同じ周にもう一度観測して当てる。回数 3・時間 2 分を上限に | E-J7 |
| 下流を同じ周に進めない | 上流の Apply が失敗・未確認なら下流は次の周 | E-D6 |
| 同じ Resource に Apply は 1 つ | 同じ対象に 2 つの Apply を同時に走らせない。Apply 中のものは予算に数える | E-D6 |
| 外の持ち主を待つ | 外の持ち主のリソースも観測して待ちに使える | E-I5・N-T2・E-N3・N-N2 |
| 下流の表示 | plan と `/graph` に、変わる Resource の下流を出す | N-J11・E-H5 |

### 3.6 Policy（いつ・どこまで変えてよいか）

**役割。** 「この範囲で、いつ、どこまで変えてよいか」を 1 つの型で書く。freeze・保守の窓・予算・承認・大量作業の別枠・報告だけの範囲は、全部 Policy か一時的な Policy。

```yaml
cycle:
  interval: 60s                       # 常駐の周の間隔
  stuck_after: 5                      # 同じ状態の Event がこの周数続けば stuck に上げる
policies:
  - name: default
    select: {all: true}
    mode: apply
    budget:
      per_cycle: 20                   # 1 周の変更の数（Apply 中を含む）
      destructive_per_cycle: 3        # 壊す変更は別に数える
      destructive_per_window: 10      # 周をまたいだ累積
      window: 24h
      max_ineffective_applies: 3      # 効かない Apply の上限
  - name: platform
    select: {labels: {owner: platform}}
    windows: ["Sat 02:00-04:00 Asia/Tokyo"]   # 窓の外は report
    approval: {quorum: {n: 2, of: [alice, bob, carol]}, separation: true}
  - name: legacy
    select: {labels: {site: legacy}}
    mode: report                      # 差分は知らせるだけ
  - name: decommission-rack-3         # 大量の正当な作業（一時的な別枠）
    select: {labels: {rack: "3"}, type: bmc/system}
    allow: {kind: reimage, max_count: 48}
    windows: ["2026-11-05T22:00+09:00 .. 2026-11-06T06:00+09:00"]
    approval: {quorum: {n: 2, of: [alice, bob]}, separation: true, cooling_off: 1h}
    applicant: carol
    reason: "ラック 3 の入れ直し"
policy_sets:                          # 常駐ごとに使う Policy の組（--policy-set）
  observer: [default, legacy, {name: all-report, select: {all: true}, mode: report}]
```

一時的な Policy（freeze・その場の報告だけ）は CLI が Record に書く。理由と期限が必須。

```console
$ sakutei freeze --reason "障害対応中" --until 2026-11-01T09:00+09:00
$ sakutei freeze --select labels.site=a --mode report --reason "手作業中" --until 4h
$ sakutei unfreeze <name> --reason "対応終わり"
```

```go
type Policy struct {
    Name     string
    Select   Selector       // {All; Types; Labels}
    Mode     Mode           // Apply | Report
    Windows  []Window
    Budget   *Budget        // {PerCycle; DestructivePerCycle; DestructivePerWindow; Window; MaxIneffectiveApplies}
    Approval *Approval      // {Quorum{N, Of}; Separation; CoolingOff; Timeout}
    Allow    *Allow         // {Kind: delete|reimage|destructive|create; MaxCount}
    ObserveEvery int        // 既定 1（毎周）
    Until time.Time; Reason, By, Applicant string // 一時的な Policy と別枠
}
// most-restrictive-wins を 1 か所で
func Effective(ps []Policy, op Operation, clock Clock, usage Usage) Decision
```

| 項目 | 内容 | ユースケース |
|---|---|---|
| `select` | 種類・ラベルで当たり先を選ぶ。宣言外（Range のラベル）にも当たる | E-M2・N-M2・E-M1 |
| `mode: report` | 差分は知らせるだけ。観測と知らせは続ける | E-I3・E-Q1・N-Q2・N-I1 |
| `windows` | 窓の外は report。手順の起動にも効く | E-J3・E-R1・N-J1・N-R3・N-A3 |
| `budget.per_cycle`・`destructive_per_cycle` | 一度に変えすぎない。超えたら超過分の保留でなく止める | E-J1 |
| `budget.destructive_per_window`・`window` | 周をまたいだ「じわじわ全消し」を止める。使用量は Record | E-J1 |
| `budget.max_ineffective_applies` | 効かない Apply を繰り返さず止める（人とツールの取り合い） | E-J6・E-K10・E-Q4・N-J12・N-Q5 |
| `approval` | N-of-M・申請者≠承認者・cooling-off・期限。手順の gate と同じ型 | E-L2・N-L2・E-J10・N-G10 |
| `allow` | 大量の正当な作業の別枠。別枠であって予算の免除ではない。宣言の外は通常の予算。枠は発射の直前に使い、失敗しても戻さない。`reimage` は削除と作成の対を 1 と数える | E-J2・E-A9・N-J2・N-A12・N-E9 |
| `until`・`reason`・`by` | 一時的な Policy。期限で消える。読めなければ止める側に寄せる | E-J4・N-Q1・E-R2・N-R1 |
| 合わせ方 | 複数当たれば最も厳しいものが勝つ（report は apply より、承認ありは無しより、小さい予算は大きい予算より） | E-J5・E-S1 |
| `policy_sets` と `--policy-set` | 同じ intent で、観測だけの常駐と進める常駐を並べる。宣言に無い名前は断る | E-M5・N-M7・E-S5・E-J11 |
| `observe_every` | 遅い装置の観測を間引く（任意。初回・未収束・desired が変わった周は必ず観測） | E-N2 |
| `cycle.interval`・`stuck_after` | 周の間隔と、続いた状態を stuck に上げる周数 | E-J11・E-H1 |
| provider の版の停止 | 版が変わった provider の操作は、その周を report にする。その周に差分が出なければ新しい版を受け入れ、出たら `sakutei accept-version <provider>` まで report | E-J12・N-S1・N-P3 |

### 3.7 Procedure（一回きりの手順）

**役割。** 非冪等な操作・多段の作業・人の作業を、周をまたいで 1 段ずつ進める。v2 の単純 Task・多相 Task・Workflow・manual はすべてこれ。進みは Record に置き、「済んだ」は観測で決める。

```yaml
procedures:
  - name: bmc-fw
    target: {type: bmc/system, select: {labels: {rack: "3"}}}
    trigger: on_drift                     # 宣言と観測がずれたら対象ごとに 1 回
    drift_fields: [firmware]
    concurrency: 1
    invariant: 'count(bmc/system[role=gpu], it.power == "On") >= 30'
    steps:
      - name: verify
        skip_if: 'observe(bmc/firmware/{target}).version == desired.firmware'
        invoke: {op: verify_image, args: {sha256: "{desired.image_sha256}"}}
      - name: approve
        gate: {approval: {quorum: {n: 1, of: [bob]}}, timeout: 24h}
      - name: apply
        critical: true                    # ここで止まったら自動で再開しない
        invoke: {op: apply_update}
      - name: reboot
        invoke: {op: reset, args: {type: GracefulRestart}}
      - name: confirm
        wait: {when: 'observe(bmc/firmware/{target}).version == desired.firmware', timeout: 30m}
    done_when: 'observe(bmc/firmware/{target}).version == desired.firmware'
    on_failure: halt
    timeout: 6h

  - name: os-install
    trigger: {policy: decommission-rack-3}   # 別枠が開いたときだけ、対象ごとに枠を 1 使う
    target: {type: bmc/system, select: {labels: {rack: "3"}}}
    steps:
      - apply: {type: bmc/virtual-media, id: "{target}/cd", fields: {image: "http://192.0.2.5/os.iso"}}
      - wait: {when: 'observe(bmc/virtual-media/{target}/cd).inserted == true', timeout: 5m}
      - invoke: {op: boot_once, args: {target: Cd}}
      - invoke: {op: reset}
      - wait: {when: 'observe(guest/os/{target}).marker == desired.install_marker', timeout: 2h}
      - apply: {type: bmc/virtual-media, id: "{target}/cd", fields: {image: ""}}
    done_when: 'observe(guest/os/{target}).marker == desired.install_marker'

  - name: switch-os-manual
    target: {type: cli/device, select: {labels: {model: legacy-sw}}}
    trigger: on_drift
    drift_fields: [os_version]
    steps:
      - manual: {instruction: "管理 GUI で OS を {desired.os_version} に上げる", url: "https://wiki.example.com/sw-upgrade"}
    done_when: 'observe(cli/device/{target}).os_version == desired.os_version'
    timeout: 168h

  - name: gateway-upgrade
    target: {type: cli/device, id: gw1}
    trigger: manual                       # sakutei start gateway-upgrade で起こす
    disruption: {estimate: 15m, affects: "拠点 a の外向き通信", notify_before: 30m}
    steps:
      - wait: {when: 'observe(metrics/upstream-queue).value < 100', timeout: 2h}
      - gate: {approval: {quorum: {n: 1, of: [alice]}}}
      - policy: {select: {labels: {site: a}}, mode: report}   # この手順の間だけ効く
      - invoke: {op: upgrade}
      - wait: {for: 10m}
```

```go
type Procedure struct {
    Name         string
    Target       TargetSel        // {Type; ID; Select}
    Trigger      Trigger          // OnDrift{Fields} | Policy(name) | BeforeDelete | Manual
    Precondition, DoneWhen, Invariant Condition
    Steps        []Step
    Concurrency  int
    OnFailure    Failure          // Halt | Steps(rollback)
    Disruption   *Disruption      // {Estimate; Affects; NotifyBefore}
    Timeout      time.Duration
}
type Step struct {
    Name     string
    Kind     StepKind             // Apply | Invoke | Exec | Manual | Wait | Gate | Policy
    Apply    *Resource
    Op       string; Args Fields
    Exec     *ExecSpec            // 外のコマンド（ansible-playbook など）
    Instruction, URL string
    When     Condition; For time.Duration; Timeout time.Duration
    SkipIf   Condition; Critical bool
    Gate     *Approval; Policy *Policy
}
```

| 項目 | 内容 | ユースケース |
|---|---|---|
| `target` | 対象の種類・ID・選び方。`{target}` と `desired.項目` で段に差し込む | E-A5・N-J3 |
| `trigger: on_drift` | 指定した項目がずれたら、対象ごとに 1 回起こす。同じ対象に未完了は 1 つだけ | E-F2・E-A6・N-F2・E-G8 |
| `drift_fields` は収束で当てない | 手順が受け持つ項目は、収束ループが Apply せず手順を起こすだけ | E-F2・E-A5・N-A4 |
| `trigger: {policy: 名前}` | その Policy の別枠が開いたときだけ起こし、枠を使う | E-A9・N-A12 |
| `trigger: before_delete` | 削除の前に手順を挟む（最後のバックアップ） | E-C3・N-C3 |
| `trigger: manual` | 人が `sakutei start` で起こす | N-F4・N-K8・N-D4 |
| 定期の起動は持たない | 定期の仕事は Resource の古さとして書く（例：`backup/job` の `max_age: 24h`） | N-J10・N-C4・N-C5・N-H7 |
| `precondition` | 始める前の条件 | E-A8 |
| 段 `apply` | Resource を当てて進む | E-A3・E-A8・N-B10 |
| 段 `invoke` | 一回きりの操作。送る前に送信中の記録を Record に書き、再開時に記録があれば送らず止める | E-A2・N-A2・N-A11・N-D4・N-E2・N-D9 |
| 段 `exec` | 外のコマンド（playbook など）を 1 回動かす | N-T3・E-T1 |
| 段 `manual` | 人の作業。観測で閉じ、人の「やった」は閉じる根拠にしない | E-A6・E-Q3・N-A5・N-Q4・N-B19 |
| 段 `wait` | `when`（条件）か `for`（時間）を待つ。不明は待つ | E-A8・N-B11・N-F6・N-G5・N-R4 |
| 段 `gate` | 承認を待つ。型は Policy の approval と同じ | E-A5・E-L2・E-J8 |
| 段 `policy` | 手順の間だけ効く一時的な Policy（範囲を report に）。手順が終われば消える | E-Q3・N-D4 |
| `skip_if` | 済んでいる段を飛ばす（再開のとき） | E-A5 |
| `critical` | 書き込み中に止まると壊れる段。止まったら自動で再開せず gate に回す。一回実行では進めない | E-A5・E-K5・N-K5 |
| `done_when` | 済んだかを観測で決める。Record を失っても二度走らない | E-A9・E-A6・E-F2 |
| `invariant` | 段を進める前に毎回確かめる集合の条件 | N-B16・N-F4・N-B18・N-J3 |
| `concurrency` | 同時に進める対象の数（ローリング） | N-J3・N-E2・N-A4 |
| `on_failure` | 止める（既定）か、戻しの段を流す | E-F2 |
| `disruption` | 全体を止める作業の見積もり・影響を gate に見せ、止める前に Event `Disruption` を出す | E-J8・E-H6・N-J8 |
| `timeout` | 手順全体の時間の上限。過ぎたら stuck | E-A6・E-R2 |
| halt の対象を触らない | 止まった手順の対象は、収束でも宣言どおりに戻さない | E-A9 |

### 3.8 Expand（intent から Resource を作る）

**役割。** intent と外部入力から、Resource・Range・Policy・Procedure の一覧を作る純粋な変換。同じ入力に同じ出力。合成リソース・テンプレート・網の許可の変換・互換の表・採番の値の差し込みはここ。コアは変換の中身を知らない。

```yaml
inputs:
  - name: inventory                    # 中身の在りかはつなぎ方の設定に書く
    expected_min: 40                   # これより少なければ「届いたが欠けている」
    max_drop: 50%                      # 前の周より件数がこれ以上減れば止める
expand:
  - kind: go:template                  # Go で登録した変換
  - kind: go:netpolicy                 # network_policy: を fw/chain・pve-fw/rules に展開
  - kind: exec:./expand-compat         # 外のコマンド。stdin に入力の JSON、stdout に出力の JSON
pools:
  - name: vmid
    range: "100-199"
    blocks: [{name: staff, range: "100-149"}]
    reuse: false
resources:
  - type: pve/vm
    id: app01
    fields:
      vmid: {from_pool: vmid, block: staff}
      ip: {from_pool: app-net, template: "192.0.2.{n}"}
```

```go
type Input struct {
    Intent      []Document
    Inputs      map[string]ExternalInput // {Data any; Reachable bool}
    Allocations map[string]Allocation    // intent のファイルから読んだ採番の結果
    SecretGens  map[string]Generation    // ref → 世代（読めなければ欠ける）
    Now         *time.Time               // 任意。日か時に丸める。要る Expand だけが受け取る
}
type Output struct {
    Resources     []Resource
    Ranges        []Range
    Policies      []Policy
    Procedures    []Procedure
    AllocRequests []AllocRequest
    Diagnostics   []Diagnostic           // {Severity: Reject|Warn|Unknown; Target; Message}
}
type Expander interface {
    Expand(ctx context.Context, in Input) (Output, error)
}
type Allocator interface {
    Pick(pool Pool, used []Value, req AllocRequest) (Value, error) // 既定はファイルの空きの先頭
}
```

| 項目 | 内容 | ユースケース |
|---|---|---|
| Go の Expand と外のコマンドの Expand | 合成リソース・全台共通の設定・台ごとの差の展開 | E-A11・N-A14・N-E7・N-B15・N-A11・N-E8 |
| 診断 Reject | 入力の意味の誤り（重なる網・理由の無い例外・表に無い版の組）を読み込みで断る | E-B5・E-A7・N-B7 |
| 診断 Warn | 隠れて効かない許可などを知らせる | E-B5 |
| 診断 Unknown | その Resource を「分からない」扱いにする（観測も Apply もしない、下流は待つ） | E-G4・E-K3 |
| 外部入力が届かない | 依る Resource を Unknown にする。空の desired を作らない | E-K3・E-K4・N-K3・N-S4 |
| `expected_min`・`max_drop` | 届いたが欠けている入力で消さない。前の周の件数は Record の目印 | E-K3 |
| 秘密の世代 | 値は渡さず世代だけ。世代が変われば Apply（値は provider が Apply の直前に取り出す） | E-G2・E-G3・N-G2 |
| 採番の要求 | Expand は値を知らず要求を出す。コアが空きを選び intent のファイル `allocations/<pool>.yaml` に書き足して、Expand をもう 1 回呼ぶ | N-B13・N-B12・N-A14 |
| `blocks`・`reuse: false`・`template` | 区切った範囲・再利用しない・文字列への埋め込み | N-B13 |
| 解放は人 | 宣言から消えても値を返さない。`sakutei alloc release` で人が返す | N-B13・N-J4 |
| `Allocator.Pick` の差し替え | 外の採番権威を使う | E-T4・N-T5 |
| `Now`（任意・丸めた時刻） | 期限つきの宣言（期限つきの許可）を Expand が落とす | N-B8・N-R1 |
| 入力を差し替える | 本番の intent に、偽の外部入力・偽の秘密・偽の provider を当てる（ツイン） | E-P4・N-P2・N-K8 |
| 純粋さ | 同じ入力に同じ出力。golden のテストで確かめられる | N-P5 |
| 出力のハッシュ | `--expect-revision` と原因の判定に使う | E-J9・E-L5・N-J9・E-J13 |

### 3.9 Record と Events（周をまたぐ記録と、外への知らせ）

**役割。** Record は周をまたいで覚えるものの唯一の置き場（ディレクトリ 1 つと flock。形は §5）。観測の値は置かない。Events は今の状態（状態型：firing/resolved）と出来事（出来事型）を外へ流す。

| 項目 | 内容 | ユースケース |
|---|---|---|
| 手順の進み | 段・送信中の記録・始まりの時刻・承認 | E-K5・N-K5・E-A2・E-F2 |
| 承認 | 誰が・いつ・何を。承認・拒否・期限切れ・取り消しも残す | E-L2・E-L6・N-L3・E-M4 |
| 一時的な Policy | freeze・その場の report。理由と期限 | E-J4・N-Q1・N-Q2 |
| 前の周の目印 | provider の版・範囲ごとの List の件数・外部入力の件数・Expand の入力と出力のハッシュ・Resource ごとの効かない回数・予算の窓の使用量・別枠の使用量と cooling-off の始まり | E-J12・E-K2・E-K3・E-J13・E-J6・E-J1・E-J2 |
| 通知の状態 | firing の Event の一覧（count・since） | E-H1・E-O3 |
| 送り出しの列 | sink ごとの送り先の位置。送れなくても収束を止めない | E-H2・N-H2 |
| 監査の鎖 | Apply・Delete・prune・承認・freeze・手順の段・採番・版の受け入れを、前の行のハッシュで鎖にする。値は書かない | E-L1・N-L1・N-L7・E-K7 |
| 採番の写し | intent のファイルの写し。食い違えばそのプールから新しい値を出さない | N-B13・E-K4 |
| 健全性 | 最後の周の時刻・続いた誤りの数・最後の誤り | E-K11・N-H4 |
| 置き場は 1 つ | v2 の状態ファイル 9 種を 1 つのディレクトリにまとめる | E-S2・N-S5 |
| ロック | 進める常駐は 1 つ。取れない常駐は進めない周を回す | E-M5・N-M7・N-S3 |
| Events の sink | Alertmanager・webhook・syslog・OTLP・metrics は外のモジュール | E-H2・E-H3・N-H3・N-H5・E-T3・N-J6 |
| 読み取り HTTP | 状態を機械で読む。遅い読み手で周を止めない | E-H5・E-L4・N-L4・N-L6 |
| 起床 | 次の周を今すぐ | E-N5・E-M6 |

```go
type Record interface {
    Lock(ctx context.Context) (release func(), held bool, err error)
    Runs() RunStore
    Approvals() ApprovalStore
    TempPolicies() PolicyStore
    Marks() MarkStore
    Events() EventStore       // 通知の状態・送り出しの列・sink の位置
    Audit() AuditLog          // Append・Verify。鎖
    Allocations() AllocStore
    Health() HealthStore
}
type Event struct {
    Seq   uint64; Kind string  // Drift・Unknown・Halt・ApprovalPending…
    Type  EventType            // State | Incident
    State string               // firing | resolved（State のとき）
    Resource *ID; Policy, Procedure string; Labels map[string]string
    Since time.Time; Count int; Message string
}
type Sink interface {
    Name() string
    Deliver(ctx context.Context, batch []Event) error // 失敗しても周を止めない。次の周に同じ位置から
}
```

---

## 4. 不変条件

検討文書 §5.4 の 35 項を、意味の重なりをまとめて 30 項にした。テストの種類：**性質**＝偽の provider でランダムな観測の並び・故障を与えて確かめる性質のテスト、**契約**＝provider SDK の契約テスト、**読込**＝読み込みの検証のテスト、**単体**＝その部品の単体テスト、**往復**＝一回実行と常駐で同じ結果になるかのテスト。

| # | 不変条件 | 守るテスト |
|---|---|---|
| **観測** | | |
| 1 | 観測を周をまたいで持ち越さない。Record に観測の値を書かない。plan の観測を reconcile に使わない | 性質：Record の全ファイルに観測の値が現れない／単体：plan のあとの reconcile が Observe を呼び直す |
| 2 | Observe は desired を受け取らない（`ObserveWith` を持つ provider を除く）。比べる処理は現実を触らない | 単体：Diff は provider を持たない型で書く（コンパイルで守る） |
| 3 | Unknown ≠ Absent。Unknown の Resource には Apply・Delete・prune をせず、下流も進めない | 性質：どの観測の並びでも Unknown の対象に書き込みが出ない／契約：届かないとき Absent を返さない |
| 4 | Converging の相手には重ねない。効かない回数に数えない | 性質 |
| 5 | 依存（金庫・Record）が使えない間は、それに依る Resource の観測を試みない | 単体 |
| **減算** | | |
| 6 | 消すのは「Range を宣言し、List が完全で、宣言に無く、外の持ち主でなく、prune を許し、予算と承認を通った」ときだけ。宣言から外しただけでは消さない | 性質：条件のどれか 1 つを外すと消えない |
| 7 | List が不完全・前の周より急減した範囲は消さない。自分と別枠が消した分だけ差し引く。前の周の件数が無ければ消さない | 性質：List の欠損を注入 |
| 8 | Observe の結果を prune の根拠にしない（List の ID だけ） | 単体：prune の関数の引数は Listing だけ（型で守る） |
| 9 | 中身を失う種類は既定で消さない。壊す変更は予算に数え、承認が要るものは承認まで流さない。別枠は予算の免除でない | 性質：承認なしで壊す変更が出ない |
| 10 | provider が断った Delete は消さず報告し続け、予算に数えない | 性質 |
| 11 | 採番の値は宣言から消えても返さない。`reuse: false` は二度と払わない。ファイルと写しが食い違えばそのプールから新しい値を出さない | 単体：ファイルを巻き戻して確かめる |
| **Apply と手順** | | |
| 12 | 同じ Resource に同時に 2 つの Apply を走らせない。上流が済まない下流を同じ周に進めない。Apply 中のものは予算に数える | 性質（遅い Apply を注入） |
| 13 | 恒久的な失敗は、desired か provider の版が変わるまで再試行しない | 単体 |
| 14 | Apply のあと読み直し、効かなければ数えて上限で止める。Unknown は数えない | 性質（成功を返すが変わらない provider） |
| 15 | 非冪等な操作は Procedure からだけ。同じ対象に未完了の手順は 1 つ | 読込：Invoke を Resource に書けない／性質 |
| 16 | 一回きりの操作は送信中の記録を先に書き、記録があれば二度送らず止める。`critical` の段は自動で再開しない。一回実行は `critical` の段を進めない | 性質（送信の途中でプロセスを止める） |
| 17 | Transaction は確かめられなければ確定しない。group は 1 つでも落ちれば全部確定しない。`before` の確かめが通らなければ変えない。Commit を呼ばない限り相手側で戻る | 性質／契約：Prepare のあと放置すると戻る |
| 18 | 手順の「済んだ」は観測で決める。不明は済んでいない。止まった手順の対象を自動で戻さない | 性質 |
| **方針** | | |
| 19 | 当たる Policy の最も厳しいものが勝つ。起動の引数・一時的な Policy は締める向きにしか効かない。report でも観測と知らせは続ける | 単体：`Effective` の全組み合わせ |
| 20 | 予算を超えたら超過分の保留でなく止める。周をまたぐ窓を数える | 性質 |
| 21 | 承認は申請者≠承認者を強制し、要件を満たす異なる承認者だけ数える。承認・拒否・期限切れを監査に残す。Record に書けなければ別枠も承認も使わない | 単体／性質（Record の書き込み失敗を注入） |
| 22 | 一時的な Policy は理由と期限を持ち、期限で消える。Record が読めない周は止める側に寄せる | 単体（時計の差し替え） |
| 23 | provider の版が変わった周は、その provider の変更を report にする | 性質 |
| **宣言と設定** | | |
| 24 | 範囲・除外・方針・採番の結果は intent にだけ置く。設定はつなぎ方だけ（設定に範囲を書く項目が無い） | 読込：設定の schema に範囲の項目が無い |
| 25 | 同じ ID・未知の項目・循環・範囲の重なり・範囲の外の宣言・全部を外す除外は読み込みで断る | 読込 |
| 26 | Expand は同じ入力に同じ出力。外部入力が届かないとき空の desired を作らず、依る Resource を Unknown にする | 単体（golden）／性質 |
| **秘密と出力** | | |
| 27 | 秘密の値をコアが持たない・比べない・Record・Event・誤りの文・HTTP に出さない。書かない周に書き込み用の資格を取り出さない | 性質：全出力に印の値が現れない／契約：WriteContext の外で書き込み用の資格を読まない |
| 28 | 通知・監査の送り出しが失敗しても収束を止めない。監査の鎖を書き換えない | 性質（sink の失敗を注入）／単体：`audit verify` |
| 29 | 確かめられなかった周に状態型の Event を resolved にしない。観測しなかった値を 0 や「ずれ無し」として出さない | 性質 |
| 30 | plan は何も消費せず、reconcile と同じ判定をする。一回実行と常駐は同じ Record から同じ結果を出す。文と JSON は同じ構造から作る | 往復：同じ観測の並びで plan・一回実行の列・常駐を比べる |

---

## 5. Record・Events・読み取り HTTP・起動の引数・つなぎ方の設定

### 5.1 Record のディレクトリ

```
record/
├── LOCK                         # flock。進める常駐（または一回実行）が持つ
├── format                       # Record の形の版。知らない版なら読まず止める
├── runs/<procedure>/<target>.json
├── approvals/<request-id>.json  # CLI と承認ツールが書く。1 件 1 ファイル（書き込みは一時ファイル＋rename）
├── temp-policies/<name>.json
├── marks.json                   # 前の周の目印
├── events/
│   ├── state.json               # firing の一覧
│   ├── outbox/000123.jsonl      # 送り出しの列（seq 順。古いものは全 sink が送ったら消す）
│   └── cursors/<sink>.json
├── audit/000007.jsonl           # 監査の鎖（大きくなれば新しいファイルへ回す）
├── alloc/<pool>.json            # 採番の写し
└── health.json
```

すべて JSON。書き込みは一時ファイルに書いて rename する。Record を失ったときは安全側に寄る：目印が無ければ消さない（不変条件 7）・provider の版の受け入れを待つ（23）・進みが無い手順は done_when で済んでいれば起こさない（18）・送信中の記録が無い操作は観測で済んでいるかを見てから起こす。

`runs/…/<target>.json` の例：

```json
{
  "procedure": "bmc-fw", "target": "bmc/system/r3-n07", "run_id": "r-20261105-0012",
  "started_at": "2026-11-05T22:03:10+09:00", "intent_revision": "sha256:9f…",
  "step": 3, "step_name": "apply",
  "sending": {"op": "apply_update", "at": "2026-11-05T22:41:02+09:00"},
  "gates": {"approve": {"approved_by": ["bob"], "at": "2026-11-05T22:30:00+09:00"}},
  "state": "running"
}
```

`marks.json` の例：

```json
{
  "cycle": 18234, "at": "2026-11-05T22:41:00+09:00",
  "provider_versions": {"dns": "dnszone/3", "pve": "proxmox-vm/7"},
  "range_counts": {"dns:zone=example.com.": 412},
  "input_counts": {"inventory": 48},
  "hashes": {"intent": "sha256:…", "inputs": "sha256:…", "expanded": "sha256:…"},
  "ineffective": {"pve/vm/app01": 1},
  "budget_window": {"start": "2026-11-05T00:00:00+09:00", "destructive": 4},
  "allow_usage": {"decommission-rack-3": {"used": 12, "first_seen": "2026-11-04T10:00:00+09:00"}}
}
```

監査の 1 行：

```json
{"seq": 88121, "prev": "sha256:…", "at": "…", "kind": "resource.apply", "resource": "pve/vm/app01",
 "fields": ["memory"], "cause": "intent", "result": "ok", "policy": "platform", "approved_by": ["alice", "bob"],
 "intent_revision": "sha256:…", "run_id": null, "reason": "CHG-1234"}
```

### 5.2 Events

| 種類 | 型 | いつ |
|---|---|---|
| `Drift` | 状態 | 宣言とずれている（report・窓の外・承認待ちを含む） |
| `Unknown` | 状態 | 観測できない・Expand が Unknown にした |
| `Converging` | 状態 | 相手が作業中（stuck_after で stuck に上がる） |
| `Unmanaged` | 状態 | 範囲の中の宣言外 |
| `Waiting` | 状態 | 依存の条件を待っている |
| `ApprovalPending` | 状態 | 承認待ち（Policy・gate） |
| `Halt` | 状態 | 予算超過・効かない上限・急減・provider の版の変化・手順の停止 |
| `Frozen` | 状態 | 一時的な Policy が効いている |
| `DeleteRefused` | 状態 | provider が削除を断った |
| `AdvancedElsewhere`・`ProcedureNeedsDaemon` | 状態 | ロックが取れない・一回実行が critical の段の手前 |
| `Applied`・`Deleted`・`ProcedureStep`・`Disruption`・`CycleDone` | 出来事 | 起きたとき 1 回 |

状態型はキー（種類＋対象）ごとに firing になった周と resolved になった周にだけ送り出しの列へ積む。確かめられなかった周はそのまま firing。`CycleDone` には周の時間・件数（drift・unknown・予算の残り）を載せ、metrics と OTLP の sink はこれから値を作る。

```json
{"seq": 5521, "kind": "Drift", "type": "state", "state": "firing", "resource": "dns/record/www.example.com./A",
 "labels": {"site": "a", "owner": "platform"}, "since": "2026-11-05T22:40:00+09:00", "count": 3,
 "cause": "drift", "message": "value: 192.0.2.10 → 192.0.2.11（report: 窓の外）"}
```

### 5.3 読み取り HTTP

| パス | 返すもの |
|---|---|
| `GET /status` | 最後の周の結果（Resource ごとの状態・原因・当たった Policy・宣言外・承認待ち・手順の進み）。`status --json` と同じ構造 |
| `GET /plan` | 今の intent と観測で計算した plan。何も消費しない。`plan --json` と同じ構造 |
| `GET /events` | 送り出しの列を SSE で。`?since=<seq>` |
| `GET /graph` | Resource と手順の依存（下流の表示） |
| `GET /health` | 健全性。続いた誤りがあれば 503 |

TLS とトークン・クライアント証明書の認可はつなぎ方の設定に書く。外向きの平文は設定で明示したときだけ。書き込みのエンドポイントは持たない（承認は CLI と承認ツール）。

### 5.4 承認と CLI

```console
$ sakutei approve <request-id> --as alice --comment "確認済み"
$ sakutei reject  <request-id> --as bob   --comment "窓が違う"
$ sakutei start gateway-upgrade [--target gw1]
$ sakutei cancel <run-id> --reason "…"
$ sakutei accept-version <provider>
$ sakutei alloc release <pool> <value> --reason "…"
$ sakutei audit verify
```

承認ツール（別のバイナリ `sakutei-approve`）は、前段の認証の結果から承認者を受け取り、同じ形の 1 ファイルを `approvals/` に書くだけ。コアは認証をしない。承認者が条件（quorum・separation・役割）を満たすかは周の中で数える。

### 5.5 起動の引数

| 引数 | 内容 |
|---|---|
| `--intent <dir>` | intent のディレクトリ |
| `--config <file>` | つなぎ方の設定ファイル |
| `--record <dir>` | Record のディレクトリ |
| `--policy-set <name>` | 使う Policy の組（宣言に無ければ断る） |
| `--once` | 1 周だけ回して終わる（`reconcile` の既定） |
| `--json` | 出力を JSON に |

サブコマンド：`sakutei`（常駐）・`plan`・`reconcile`・`status`・`wake`・`freeze`/`unfreeze`・`approve`/`reject`・`start`/`cancel`・`accept-version`・`alloc release`・`audit verify`。

一回実行の終了コード（E-J14）：0 収束した／1 実行の誤り／2 読み込みで断った／3 ずれか Unknown が残る（report・承認待ちを含む）／4 止まった（予算・効かない上限・急減・版の変化）。

### 5.6 つなぎ方の設定ファイル

範囲・除外・方針は書けない（書けば読み込みで断る）。書くのは接続先・資格・待ち受け・外部入力の在りか・sink の宛先。

```yaml
providers:
  dns:
    module: dnszone
    settings: {zone_dir: /var/lib/named, reload_cmd: ["rndc", "reload"]}
  pve:
    module: proxmox-vm
    settings: {endpoint: "https://pve.example.com:8006", api_base_path: /api2/json}
    observe: {token: {secret: "file:/etc/sakutei/pve-ro.token"}}
    write:   {token: {secret: "vault:/pve/writer#token"}}
    max_concurrent: 8
  fw:
    module: nftables
    settings: {guard_tool: /usr/local/sbin/sakutei-nft-guard}
    ssh: {known_hosts: /etc/sakutei/known_hosts, key: {secret: "file:/etc/sakutei/id_ed25519"}}
secret_backends:
  vault: {module: vault-kv2, settings: {address: "https://vault.example.com:8200", approle: {role_id_file: /etc/sakutei/role_id, secret_id_file: /etc/sakutei/secret_id}}}
  file:  {module: file}
inputs:
  inventory: {file: /srv/inventory/hosts.yaml}
expanders:
  compat: {exec: /usr/local/libexec/sakutei/expand-compat}
sinks:
  am:     {module: alertmanager, settings: {urls: ["https://am1.example.com", "https://am2.example.com"]}}
  syslog: {module: syslog, settings: {address: "tls://log.example.com:6514"}, streams: [audit]}
  prom:   {module: metrics, settings: {listen: "127.0.0.1:9464"}}
listen:
  read: {address: "0.0.0.0:8443", tls: {cert: /etc/sakutei/tls.crt, key: /etc/sakutei/tls.key}, auth: {tokens_file: /etc/sakutei/read-tokens}}
```

| 項目 | ユースケース |
|---|---|
| `providers.<名前>.module`・`settings` | E-O4・E-S1・N-S2 |
| `observe`・`write` の資格 | E-G5・N-G3 |
| `ssh` の known_hosts と鍵 | E-E1 |
| `max_concurrent` | E-N1・N-N4 |
| `secret_backends` | E-G3・E-O2・N-O2 |
| `inputs` の在りか | E-K3・E-P4 |
| `expanders` の在りか（どれを使うかは intent の `expand:`） | E-B5・E-A7 |
| `sinks`（`streams: [events, audit]`） | E-H2・E-H3・E-L1 |
| `listen.read` | E-H5・E-L3・N-L4 |

---

## 6. provider SDK

### 6.1 作者が書くもの

1. `Schema()`：種類ごとの項目（型・identity・壊す変更の印）・settle・観測だけか・中身を失う種類か・版。
2. `Observe(ids)`：4 値と正規形の項目。宣言と観測で同じ正規化の関数を使う。
3. `Apply`・`Delete`：冪等。Delete は断る理由を返せる。
4. `List(range)`：ID の集合と「完全か」。
5. 設定の読み方（`settings` の型）。範囲の引数（`Range.Args`）は設定でなく Range から受け取る。
6. 任意：`Transactor`・`Invoker`・`Classifier`・`Validator`・`DesiredObserver`。

SDK が用意するもの：`SecretRef.Resolve(w WriteContext)`（書き込みの文脈でだけ書き込み用の資格を返す）・ssh の実行の層（known_hosts・接続の使い回し・決まったコマンドだけを sudo で）・HTTP の下回り（TLS・API のパスの前置き・リトライ）・`cli.Main(WithProvider(...), WithExpander(...), WithSink(...), WithSecretBackend(...), WithCheck(...))`。

| SDK の項目 | ユースケース |
|---|---|
| `cli.Main(With…)` で自前のバイナリに組み込む | E-O4・E-S4 |
| ssh の実行の層 | E-E1・N-E1 |
| `SecretRef` と `SecretBackend{Generation, Resolve}` | E-G2・E-G3・N-G2 |
| HTTP の下回りと API のパスの前置き | E-C1・E-D1 |
| `WithCheck` | E-B9 |

### 6.2 契約テスト（`sdk/contract`）

provider 作者は自分の provider と偽の相手（または模擬）を渡して回す。

| テスト | 確かめること | ユースケース |
|---|---|---|
| 届かないなら Unknown | 接続を切る・5xx・時間切れ・認証失敗・壊れた出力で、Absent を返さない | E-K1・E-E6・E-P2 |
| 無いなら Absent | 確かに無い対象に Absent を返す | E-P2 |
| 作って読む | Absent → Apply → Observe が Present で宣言どおり | E-P2 |
| 冪等 | 同じ Apply を 2 回して 2 回目に現実が変わらない | E-P2 |
| 正規形 | 宣言と観測を同じ関数で正規化し、書き直しの無い Apply が差分を作らない | E-I7・E-B7 |
| List の完全さ | ページの途中で切ると `Complete: false` | E-K2・E-P2 |
| Observe と List の一致 | List に出る ID は Observe で Present | E-D8 |
| 秘密が漏れない | 誤りの文・Note・Fields に秘密の値が出ない | E-G2・N-L5 |
| 書き込みの資格 | `WriteContext` の外で書き込み用の資格を読まない | E-G5 |
| Schema の lint | 版がある・identity の項目が宣言できる・壊す印の矛盾が無い | E-P2 |
| Transaction の戻り | Prepare して Commit しなければ、時間切れのあとに元に戻る（模擬で） | E-B3・E-B8 |
| Invoke の送信 | 送れなかったときだけ `ErrNotSent` | E-A2 |
| Delete の断り | 断る条件で `RefuseError` を返し、何も消さない | E-F1・E-C2 |

コアの性質のテスト（§4）は偽の provider（`sdk/fake`：観測の並び・失敗・遅さ・効かない Apply を注入できる）で回す（E-P1・N-P1）。統合テスト（qemu-bmc・netopeer2・Proxmox の入れ子・postgres など）は各 provider のモジュールの側で回す（E-P3・N-P4）。時計はコアの `Clock` で差し替えられる（E-R4）。

---

## 7. v2 から移すもの・捨てるもの

### 7.1 provider（全部コアの外のモジュールへ移植）

| v2 | 新しいモジュール | 移植の要点 | ユースケース |
|---|---|---|---|
| dnszone | `dnszone` | ゾーンの指定は Range の `args.zone` へ。SOA のシリアルを上げるのは Apply の中のまま。`soa_serial: keep` は Resource の項目 | E-B1・N-B11 |
| nftables（file・live）＋ sakutei-nft-guard | `nftables`・`nft-guard` | base chain の守りは `Transactor`（Prepare＝写しと guard の arm、Commit＝disarm）。確かめる先は Resource の `confirm`（`phase`）。作成・policy 変更は Schema の壊す印と `Classifier`。`tables` は Range へ | E-B2・E-B3・E-B4・N-B9 |
| redfish-system | `redfish` の `system` | power の強制停止は壊す印。電源の切り替え中は Converging | E-A1・N-A1 |
| redfish の boot_once・reset | `redfish` の `Invoker` | `ErrNotSent` の判定をそのまま移す | E-A2・N-A2 |
| redfish-virtual-media | `redfish` の `virtual-media` | 変更なし | E-A3 |
| redfish-bios | `redfish` の `bios` | 保留中は Converging。属性の登録簿の検査は `Validator` | E-A4・N-A3 |
| redfish-firmware（多相 Task） | `redfish` の `firmware`（観測だけ）＋ `Invoker`（verify・stage・apply） | 相は intent の Procedure に書き直す（§3.7 の例）。危険区間は `critical`。`TargetDetermined` の判定は provider の中 | E-A5・N-A4 |
| proxmox-vm | `proxmox` の `vm` | UPID の待ちは Apply の中か Converging。`keep`・`volume=` の付け替えは `Classifier`。keep の残る VM の削除は `RefuseError`。cluster の範囲は Range | E-D1・E-C2・N-D1・N-D10・N-C12・N-D3（置けるノードの集合は provider の項目で、今のノードが集合に入っていればずれにしない） |
| proxmox-fw | `proxmox` の `fw-rules` | 印の無い規則は List で宣言外として出す。規則の変更は壊す印 | E-B6 |
| netconf | `netconf` | 2 段の Apply は `Transactor`（commit confirmed）。commit_group は Resource の `transaction.group`。部分木と機器は Range。NACM の制限は settings | E-B7・E-B8・E-B9・E-B10・N-B1・N-B4 |
| cli-device | `cli-device` | 機種の定義ファイルは settings。定義の `actions` は `Invoker`。commit/rollback は `Transactor` | E-B11・N-B1 |
| postgresql | `postgresql` | 利用者の表がある DB の削除は `RefuseError`。scope は Range | E-F1・E-F3・N-F1・N-F7 |
| sqlmigrate | `sqlmigrate`（観測と `Invoker`） | `strategy: task` は Procedure（`trigger: on_drift`・`invoke: migrate`・`on_failure` で rollback） | E-F2・N-F2 |
| s3-bucket | `s3` | Object Lock の変更は `Classifier`（承認）。中身のあるバケットの削除は `RefuseError`。接頭辞は Range | E-C1・N-C7 |
| ldap | `ldap` | base_dn・filter・scope は Range。子のある項目の削除は `RefuseError` | E-G1・N-G1 |
| ansible | `ansible` | `DesiredObserver`（check mode）。List と prune は持たない | E-E3・N-E5・N-T3 |
| template-file | `template-file` | `DesiredObserver` | E-E5・N-E8 |
| host-file・systemd-unit（hostconf） | `hostconf` | `restart_on` は Resource の項目のまま。ホストは Range。中身は hash と stat だけ観測 | E-D5・E-E2・N-D7・N-E1・N-E6・E-H7・N-H1（監視の設定のファイルもこれで配る） |
| guest-os | `guest-os` | 観測だけ。目印は Resource の項目 | E-E4・E-A8 |
| metrics-probe | `metrics-probe` | 観測だけ。比べ方は provider の設定でなく Condition に移す（provider は値を返すだけ） | E-H4・N-T4 |
| secret backends（vault-kv2・command・file） | `secret/vault-kv2` ほか | 世代を返す `Generation` と `Resolve` のまま | E-G3・E-G4 |
| （新規）台帳 | `ledger` | 観測できない資産（紙の台帳・テープ・旧機器）を宣言だけで持つ。観測は台帳のファイルの「最後に確かめた日からの経過」だけを返し、List と Apply を持たない（消さない・触らない） | E-I4・N-I6・N-C6・N-B19 |
| （新規）秘密の生成 | `secret/generate` | Apply が乱数のパスワードや鍵対を作って backend に置く。宣言には参照だけ | N-G9・N-G8・N-C10 |
| （新規）到達の観測 | `probe` | tcp・ssh・http の到達を観測だけで返す。v2 で nftables・netconf の中にあった到達の確かめをここに集め、Condition から読む | E-B13・N-B17 |
| fakeprovider | `sdk/fake` | 故障の注入を足す | E-P1 |

### 7.2 コアの機能

| v2 の機能 | 扱い | 新しい受け皿 |
|---|---|---|
| reconcile ループ（loop.go） | 書き直す | `RunCycle` 1 つ（§2） |
| render のフェーズ 1・2 | 書き直す | 入力の読み込み＋Expand |
| managed・computed・PlanRefiner の落とし | 捨てる | 宣言した項目だけ比べる |
| ready_condition・require・done_when・apply_check・confirm の observe/check・capacity gate | まとめる | Condition |
| remediation mode・schedule・freeze・planned_change・max-* のフラグ・approve-large-change・profile・runtime | まとめる | Policy・一時的な Policy・`--policy-set` |
| 単純 Task・多相 Task・Workflow・manual・disruption | まとめる | Procedure |
| netconf の 2 段・nftables の守り・commit_group・confirmafter | まとめる | Transactor と Resource の `transaction` |
| `providers:`・`external_owners:`・deletion_policy・IntentRanges・ConfigExcluder | まとめる | Range |
| task-store・workflow-store・alloc-store・apply-check-state・freeze-state・daemon-status・cause-state・audit-log | まとめる | Record |
| fencing-store・claim・gitdist・S2-free・DR モード・observe tier・BulkObserver | 捨てる | §1.3・§1.4 |
| 原因タグ A〜E・last-seen | 縮める | 3 分類と版の停止 |
| allocator（authority・Lookup キャッシュ・cooldown・write_back の 3 方式） | 縮める | intent のファイル＋`Allocator.Pick` |
| network_policy（pkg/netpolicy） | 外へ | Expand モジュール `netpolicy` |
| compatibility | 外へ | Expand モジュール `compat` |
| eventnotify・audit の syslog・otlpexport・metrics | 外へ | sink モジュール |
| readapi | 縮める | 読み取り HTTP（書き込み無し） |
| 承認の HTTP（`--approval-listen`） | 外へ | `sakutei-approve`（E-M4・N-M4） |
| secrets extract・break glass の記録 | 外へ | 道具と運用。break glass の使用は道具が監査に 1 行足す（E-G6・E-G7） |
| importer・syncmode | 外へ | SDK の上の道具（現実→intent の片方向。Apply の経路につながない）（E-I1・E-I2・N-I5・E-Q2・N-Q3・N-E3・N-I1。他の道具からの変換 N-I7 も同じ形） |
| 起動のフラグ 128 | 捨てる | 6 つ＋サブコマンド |
| v2 の intent | 変換する | 変換の道具 `sakutei-migrate`（v2 の intent と provider 設定を読んで新しい intent とつなぎ方の設定を書く。範囲と除外は設定から intent へ動かす） |

---

## 8. 作る順番

各段で「動くもの」と「確かめられること」をはっきりさせる。v2 は直しだけにして、新しいコアが v2 の intent の変換結果を全部扱えるまで並べて保つ。

| 段 | 作るもの | 確かめられること |
|---|---|---|
| 1 | 型（Resource・Range・Schema・Observation）・intent とつなぎ方の設定の読み込みと検証・Expand（素通し）・偽の provider・`plan` | 読み込みで断る規則（不変条件 24・25）。plan が観測と比べるだけで何も書かない（30 の半分） |
| 2 | `RunCycle` の Apply・Delete・List・prune の守り・Policy の mode と予算・Record（ロック・目印・監査の鎖）・`reconcile --once`・終了コード | Unknown で消さない・List の欠損と急減で消さない・予算で止まる（3・6・7・9・20）。性質のテストの土台ができる |
| 3 | 常駐（繰り返し・起床・読み取り HTTP）・Events（状態型と送り出しの列）・stdout の sink | 一回実行の列と常駐が同じ結果（30）。確かめられない周に resolved にしない（29）。sink の失敗で止まらない（28） |
| 4 | Condition（三値・量化）・Graph（wave・待ち・同じ周の観測し直し）・settle と効かない回数 | 空から組む構成が 1 回で収束する（E-J7）。効かない Apply で止まる（14）。容量の門（E-C4） |
| 5 | Procedure（apply・invoke・wait・manual・gate・policy・critical）・承認の CLI・一時的な Policy・`approve` と `freeze` | 送信の途中で止めても二度送らない（16）。done_when で閉じる（18）。申請者≠承認者（21）。freeze の期限（22） |
| 6 | 最初の実 provider：`dnszone`・`hostconf`・`nftables`（Transactor を含む）と `nft-guard` | 実のファイルとカーネルで、2 段の Apply が戻る（17）。v2 の試用の手順（DNS と FW）が通る |
| 7 | Expand のモジュール（template・netpolicy・compat）・採番・秘密の世代と secret backend | 採番を巻き戻しても二度払わない（11）。金庫が落ちても Unknown に寄る（E-G4）。golden のテスト（N-P5） |
| 8 | 残りの provider（proxmox・redfish・netconf・cli-device・postgresql・sqlmigrate・s3・ldap・ansible・template-file・guest-os・metrics-probe）・sink（alertmanager・syslog・metrics・otlp）・`sakutei-approve` | 各 provider の契約テストと、v2 と同じ統合テスト（qemu-bmc・netopeer2・Proxmox の入れ子） |
| 9 | `sakutei-migrate`・import と同期の道具 | v2 の試用の intent を全部変換して、新しいコアの plan が v2 の plan と同じ操作を出す |

最初の版（段 2 まで）で、v2 の安全の芯（消さない・数えて止める・一回実行で収束）を偽の provider の上で確かめられる。段 6 で初めて実物に触る。
