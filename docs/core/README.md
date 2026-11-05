# Sakutei の新しいコア 設計書

2026-10-31 作成。v2.12.0（main d28dc4a）までの Sakutei を、コアからゼロで作り直すための設計書。互換は持たないが、v2 のユースケースは全部満たす。

もとにした検討は 2 つある。`docs/review/core-reduction-draft.md`（たたき台）と、`docs/review/core-usecase-review.md`（303 件のユースケースの検討）。本書の `E-…`・`N-…` の ID は後者の ID。

実装は `v3/`（コア）と `v3/providers/`（provider などコアの外のモジュール）にある。§8 の Stage 1〜9 はすべて main に入っている。設計のうちまだ実装していないものは §8.1 にまとめ、本文の該当箇所に「未実装」と書いた。

## 要約

- **何を作るか。** オンプレの基盤（BMC・ネットワーク・FW・ストレージ・VM・ゲスト OS・DB）を宣言どおりに保つ収束エンジン。土台の考え方は v2 と同じ 5 つの原則：
  - 毎 cycle 観測して直す
  - 確かでなければ消さない
  - 破壊的な変更は数えて halt し、承認させる
  - 「いつ」は収束、「どう」は Procedure
  - 正しい状態は宣言に置く
- **コアの中身は「1 cycle だけ回す処理」1 つ。** cycle をまたいで覚えるもの（Procedure の進捗・承認・前の cycle の marks・通知の状態）は、全部ディスクの Record に置く。メモリには持たない。だから cron で one-shot 実行を回しても、daemon で回しても同じ結果になる。daemon はその処理を繰り返す薄い外側で、足すのは 3 つだけ：承認の待ち受け、read-only HTTP API、すぐ次の cycle を始める wake。標準の使い方は daemon。
- **部品は 9 つ。**
  - Resource（宣言 1 件）
  - Range（管理する範囲）
  - Provider（装置とのやりとり）
  - Condition（観測についての三値の式）
  - Graph（順序と待ち）
  - Policy（いつ・どこまで変えてよいか）
  - Procedure（一回きりの手順）
  - Expand（intent から Resource を作る純粋な変換）
  - Record と Events（cycle をまたぐ記録と、外への通知）
- **コアの外に出すもの。** provider 全部、Expand の実装（network policy の変換・compatibility table など）、通知の sink（Alertmanager・syslog・OTLP・metrics）、import と同期のツール、承認の HTTP ツール。コアはこれらの interface だけを持つ。provider 固有の起動フラグは持たない。
- **設定の置き場所は 2 つ。** intent（何を・Range・方針）と、接続設定（config ファイル）1 つ（接続先・credential・listen）。起動フラグは 6 つ（うち `--policy-set` は未実装、§5.5）。
- **やめるもの。** シャードと claim、fencing、git のミラー、DR モード、影響範囲だけの早い収束、BulkObserve の専用 interface、原因タグ A〜E、運用プロファイルの型、採番の外部権威と予約、runtime の役と締め緩めの規則、Procedure の定期起動。どれも「戻す目安」と「戻すときの最小の interface」を §1.4 に残す。
- **規則。** §3 に載せる項目には、必ずそれを要求するユースケースの ID を付ける。ID を付けられない項目は入れない。機能がまた増えすぎるのを止めるための規則で、今後の改訂でも守る。

---

## 1. 目的と原則

### 1.1 目的

intent に書いたとおりの状態を、毎 cycle 現実を再観測して保つ。手で変えられたら戻す（または通知する）。確かでないときは何もしない。一回きりの危険な手順は、承認と観測で区切った Procedure として安全に進める。

v2 は 303 件のユースケースのほとんどを満たした。ただし同じ考えが別々の機能として何度も入っていた（halt する・緩める系が 10、観測で待つ系が 9、two-phase Apply の系が 6）。設定の置き場所は 4 つに分かれ、provider はコアの中にいた。新しいコアは、同じユースケースを少ない部品の組み合わせで満たす。

### 1.2 5 つの原則

| # | 原則 | コアでの意味 |
|---|---|---|
| P1 | **毎 cycle 観測して直す・観測を記録しない** | 毎 cycle、desired と現実を出し直す。観測した値は cycle をまたいで持ち越さない（Record にも置かない）。前の cycle の値を正としない |
| P2 | **確かでなければ消さない** | 観測の答えは Present・Absent・Unknown・Converging の 4 値。Unknown は Absent ではない。消すのは次がすべてそろったときだけ：Range を宣言している・List が完全・宣言に無い・external owner のものでない・budget と承認を通った |
| P3 | **破壊的な変更は数えて halt し、承認させる** | 破壊的な変更（削除・破壊的な field の変更・provider が破壊的と言った変更）は budget に数え、超えたら halt する。承認が要るものは承認があるまで流さない |
| P4 | **「いつ」は収束、「どう」は Procedure** | 収束ループは「いつ・どの順で」を決める。非冪等な操作や多段の作業は Procedure に閉じ込め、ループには載せない |
| P5 | **正しい状態は宣言に置く** | Range・除外・方針・採番の結果は intent に置く。ツールの中（Record・設定）にしか無い正しい状態を作らない。Record は進捗と記録だけ |

### 1.3 持たないもの

| やめるもの | 満たしていたユースケース | 新しいコアでの受け皿 |
|---|---|---|
| claim とシャード・git のミラー・S2-free | E-N3・E-N4・E-K6・E-K9・N-N3・N-K6 | Range を分けた複数の daemon（それぞれ別の intent と Record）＋相手の現実を Condition で待つ。ミラーは git の外のツール。Record を揮発性の場所に置けば S2-free と同じ |
| fencing（二重の書き手の防止） | E-K8・N-K7 | 1 つの intent の中で担当範囲が重なる宣言は、読み込み時にエラーにする（reject）。同じ Record を共有する daemon は、ロックを取った 1 つだけが書く |
| DR モード | E-K7・N-K4・N-Q6 | 別の `--policy-set` で起動し、DR 用の intent は Expand の入力で選ぶ。署名と合流は git のツール。コアは audit に intent のバージョンを残すだけ |
| 影響範囲だけの早い収束（scoped reconcile） | E-N5・E-M6・N-M6 | wake（次の cycle を今すぐ全部回す） |
| BulkObserve の専用 interface | E-D8・N-D11 | `Observe` を ID の一覧で呼ぶ形にする。prune の根拠は List の ID だけ |
| 観測の tier | E-N2 | Policy の任意の field `observe_every`（既定は毎 cycle 全部） |
| 原因タグ A〜E・last-seen | E-J13・E-J12 | 原因は 3 つ（宣言・外部入力・現実のずれ）。provider のバージョンが変わった cycle は halt する |
| 容量の門・振動の検知・compatibility table の照合 | E-C4・E-J6・E-K10・E-A7 | Condition（`on_unknown: block`）・Policy の `max_ineffective_applies`・Expand |
| 運用プロファイル（solo/team/enterprise）の型 | E-J10 | Policy の承認の既定値の組を、例として文書に置く（コアの型にしない） |
| 採番の外部権威・Reserve/Confirm・キャッシュ・TTL | E-K4・E-T4 | intent のファイルが正。コアは空きを選んで書き足すだけ。選び方は差し替えられる |
| runtime の役・daemon/oneshot の別の値・締め緩めの規則 | E-J11・E-M5・E-S5 | Policy と一時的な Policy と `--policy-set`。one-shot 実行と daemon で値を分けない |
| Procedure の定期起動（every:） | N-J10・N-C4・N-C5 | 「新しい結果がある」を Resource の古さとして観測する（§3.7 の I15 の書き方） |
| provider の設定での除外（安全網） | E-D2 | 除外は intent の Range にだけ書く |
| 承認の HTTP をコアに持つこと | E-M4・E-L3 | CLI と外の小さな承認ツールが Record に書く |
| 起動フラグ 128 個・provider 固有のフラグ 13 個 | E-S1 | 6 つ（§5.5） |

### 1.4 戻す目安

大規模向けの機能は「いまの使い方では効かない」ので外した。次の目安に当たったら戻すことを検討する。戻すときは下の最小の interface だけを足し、コアの 1 cycle の形は変えない。

| 機能 | 戻す目安（どれか 1 つ） | 戻すときの最小の interface |
|---|---|---|
| 観測の一括取得 | 1 つの provider の観測が cycle の間隔の 3 割を超える／API の呼び出しに課金がある | `Observe(ids)` がすでに一覧を受けるので、provider の中で一括 API を使うだけ。コアの変更なし |
| 観測の間引き | リソースが 1 万件を超える／遅い BMC が 100 台を超える | Policy の `observe_every`（初回・未収束・desired が変わった cycle は必ず観測） |
| 影響範囲だけの収束 | 秒単位の反映が契約上の要求になった（テナントのセルフサービス） | Graph に「出力の差分から下流をたどる」関数。budget は全体と共有し、その経路では prune しない |
| シャード | 拠点が 3 つ以上で、1 つの daemon から全部に届かない | Range ごとに daemon を分ける起動フラグ（どの Range を担当するか）。intent は 1 つのまま |
| fencing | 同じ範囲を 2 つの daemon が持つ引き継ぎが日常になった | Record に範囲ごとの世代を置き、Apply の前に確認する任意の interface |
| 原因の細かい分類 | 「波及」「ツール更新」を分けないと運用の判断ができない事例が 3 件以上 | Expand の出力を Resource ごとにハッシュして前の cycle と比べる（Record の marks に足す） |
| 採番の外部権威 | NetBox などを正にする現場が出た | `Allocator.Pick` の差し替え（すでにある） |
| DR モード | 主拠点の全損を機械で切り替える要求が出た | 署名つきの一時的な Policy を Record に置くツール |

---

## 2. 1 cycle の流れ

### 2.1 Phase

1 cycle は Phase 0〜8 の順に進む。

| Phase | すること | 失敗したとき |
|---|---|---|
| 0 ロック | Record のロック（flock）を取る | 取れなければ「進めない cycle」になる。観測して、自分の結果と read-only HTTP API に出すだけ。Apply・Delete・Procedure の前進・Record への書き込み・sink への送信はしない（ロックを持つ側がする）。結果に `advanced_elsewhere` を立てる |
| 1 読み込み | intent・接続設定・Record（一時的な Policy・marks・Procedure の進捗・承認・通知の状態）を読む。外部入力と secret の世代を読む | intent か設定が読めない・形が誤り → その cycle は何もしない（終了コード 2）。Record が読めない → 全体を report に寄せる。外部入力が届かない → Expand に「届かない」と伝える |
| 2 Expand | Expand を順に呼び、Resource・Range・Policy・Procedure・採番の要求・診断を得る。出力を検証する（同じ ID・未知の field・循環・範囲の重なり・Schema に無い field）。採番の要求があれば空きを選んで intent のファイルに書き足し、Expand をもう 1 回だけ呼ぶ | 検証で reject したら何もしない（終了コード 2）。診断で Unknown にされた Resource は、観測も Apply もしない |
| 3 観測 | provider ごとに `Observe(ids)` を、Range ごとに `List` を呼ぶ。provider のバージョンを Record の marks と比べる（未実装） | 呼び出しが失敗した ID は Unknown。List が失敗した範囲は「不完全」 |
| 4 比べる | 宣言に書いた field だけを比べ、操作（create・update・delete・prune・なし）を決める。原因（宣言・外部入力・ずれ）を marks のハッシュと比べて付ける（原因は未実装） | Unknown・Converging の Resource は操作なし |
| 5 方針の判定 | 操作ごとに該当する Policy を合わせ（最も厳しいものが勝つ）、mode・window・budget・承認・allow を判定する。Graph で順番（wave）を決める。Procedure の起動条件を判定する | budget を超えたら、超過分だけ保留するのでなく全体を halt する。承認待ちは Event に出して止める |
| 6 Apply | wave ごとに Apply・Delete を並べて呼ぶ。Transaction を持つ provider には two-phase Apply をする。Procedure を 1 step ずつ進める | 失敗はその Resource だけ。下流は同じ cycle に進めない |
| 7 確認 | settle を待って読み直し、効いたかを見る。効かなければ回数を数える。上流が効いたら、観測できなかった下流を同じ cycle に再観測して Apply する（上限つき） | ineffective Apply の回数が上限に達したら halt する |
| 8 記録と通知 | audit のハッシュチェーンに追記する。marks・Procedure の進捗・通知の状態・health を Record に書く。状態の変わった Event を outbox に積む。ロックを放す | Record に書けなければ、その cycle に開いた allow・承認を使わない（実行の前に書く） |

`plan` は Phase 1〜5 を実行する。何も書かず、何も消費しない（budget・allow・承認を減らさない）。`status` サブコマンドは Phase 1〜4 の結果と Record の状態を出す設計だが、未実装（§8.1）。daemon の最後の cycle の結果は read-only HTTP API の `/status` で読める。

### 2.2 one-shot 実行と daemon

- **コアは 1 cycle を回す関数 1 つ。** 実装では `cycle.Run(ctx, in) (plan.Result, error)`。one-shot 実行（`sakutei reconcile --once`）はこれを 1 回呼んで終わる。daemon（`sakutei`）はこれを繰り返す。
- **cycle をまたいで覚えるものは、すべて Record に置く。** メモリには置かない。置くものは次のとおり：
  - Procedure の進捗・送信中の記録・承認・一時的な Policy
  - 前の cycle の marks（Range ごとの件数・外部入力の件数・ineffective Apply の回数・budget の window の使用量・allow の使用量）
  - 通知の状態（firing の一覧と count・since）

  だから、cron で 5 分ごとに one-shot 実行を回すのと、間隔 5 分の daemon は同じ結果になる（E-O3・N-O4）。
- **daemon が足すのは 3 つだけ。**
  1. 承認の待ち受け：Record の承認の置き場に書き込みがあったら起きる。承認は CLI か外の承認ツールが Record に書く。daemon は承認を受け取る HTTP を持たない。
  2. read-only HTTP API：`/status`・`/plan`・`/events`・`/graph`・`/health`。
  3. wake：`sakutei wake`・SIGHUP・intent のディレクトリの変更で、待ちを切り上げて次の cycle を始める。
- **one-shot 実行は `critical` の step を進めない。** Procedure の中の危険な step（書き込み中に止まると壊れる step）は、止まったときに見張る者がいる daemon だけが進める。one-shot 実行はその step の手前で止め、Event `ProcedureNeedsDaemon` を出す（検討文書 §6-14 の決定）。
- 標準の使い方は daemon。one-shot 実行は、作業者の PC から収束させたいとき（段0-zero）と、CI での確認に使う。

---

## 3. 9 つの部品

表の「ユースケース」の列は `docs/review/core-usecase-review.md` の ID。

### 3.1 Resource（宣言 1 件）

**役割。** 1 つの対象について「こうあってほしい」を書く。比べるのは宣言に書いた field だけ。

```yaml
resources:
  - type: dns/record                 # <provider のインスタンス名>/<kind>
    id: www.example.com./A
    labels: {site: a, owner: platform}
    fields: {value: 192.0.2.10, ttl: 300}
    reason: "CHG-1234"               # audit と Event に載る
  - type: pve/vm
    id: old-vm
    ensure: absent                   # あれば Delete。破壊的な変更として budget と承認に通す
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

この例の `probe` provider は未実装（§8.1）。いまは到達の確認を `check(<名前>)`（`cli.WithCheck` で組み込む）か、ほかの provider の Resource の `observe(…)`・`present(…)` で書く。

```go
type ID struct{ Type, Name string } // Type は "<provider>/<kind>"
type Fields map[string]any          // 宣言した field だけ。secret は SecretRef（値を持たない）

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

| field | 内容 | ユースケース |
|---|---|---|
| `type`・`id` | type と ID。intent 全体で一意（重複は読み込み時にエラーにする） | E-M3・N-M3 |
| `fields` | 宣言した field だけを比べる。宣言に無い field・相手が動かす field（配置・動的 IP・機器が付ける時刻）は比べない | E-D7・E-B7・N-I2・E-B14・E-A10 |
| `fields` の secret | `{secret: "vault:/db/app#password"}`。値は持たず、世代だけを比べる | E-G2・N-G2・N-E4 |
| `labels` | Policy の適用対象と、Event のルーティング先 | E-M1・E-M2・N-M5 |
| `ensure: absent` | 名指しで消す。破壊的な変更として budget と承認に通す | E-D3・N-A9 |
| `depends_on` | 順序と待ち（§3.5） | E-J7・E-B13・E-I5 |
| `depends_on[].when`・`on_unknown` | 観測で条件を満たすまで待つ。Unknown のときの扱い | E-C4・N-C8・N-J5・N-H6 |
| `apply.transaction` | two-phase Apply をして確認する（§3.3 の Transactor） | E-B3・E-B8・N-B2・N-D5 |
| `apply.transaction.group` | 同じ group は、全部で confirm が通ったときだけ Commit する | E-B9・N-B3・N-B14 |
| `confirm[].phase` | `before`：変える前に確認し、だめなら変えない。`after`：変えたあとに確認し、だめなら Commit しない | E-B3・N-B9 |
| confirm の決まり | `after` が通るのは、単位（group か宣言）の全部の宣言を読み直して宣言どおりで、かつ全部の `after` が真のときだけ。Unknown は通らない。`timeout`（既定 30s、2s 以上）の半分まで、1 秒ごとに確認し直す。通らなければ Commit を呼ばない（Rollback は戻りを急ぐためだけ）。同じ宣言のまま Apply し直さず、halt して通知する（宣言を直すか、現実が宣言どおりになれば数え直す）。group の 1 つが Apply できない（待ち・承認待ち・report）なら group ごと待つ。group の中の依存は読み込み時にエラーにする | E-B3・E-B9・N-B3・不変条件 13・17 |
| `reason` | 変更の理由を audit と Event に出す | N-J7・N-L8 |

### 3.2 Range（管理する範囲）

**役割。** 「この provider のこの集合はこちらが管理する」という境界を 1 か所に書く。List の引数、宣言に無いもの（unmanaged）を消してよいか、触らないもの（external owner）をまとめる。v2 の `providers:`・`external_owners:`・`deletion_policy`・`--claim` を置き換える。

```yaml
ranges:
  - provider: dns
    args: {zone: example.com.}          # provider が List に使う引数（zone・tables・hosts・base_dn・scope…）
    labels: {site: a}                   # この範囲の現実に付けるラベル（Policy の適用対象）
    prune: true                         # 宣言に無いものを消してよい（既定 false）
    max_drop: 50%                       # 前の cycle より List がこれ以上減ったら、この範囲は消さない
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
    MaxDrop        int               // 百分率。既定 50（DefaultMaxDrop）
    ExternalOwners []Exclusion       // {Owner; ID; IDPrefix; Reason}。ID か IDPrefix のどちらか 1 つ
}
```

| field | 内容 | ユースケース |
|---|---|---|
| `provider`・`args` | List の引数。範囲は intent にだけ書く（設定には書かない） | E-B2・E-I6・E-G1・E-B4 |
| `labels` | 宣言に無いものにも Policy を効かせるためのラベル | E-D4・N-I3 |
| `prune` | 宣言に無いものを消してよいか。既定は消さない。破壊的な type は prune でも承認が要る | E-C3・N-C2・N-F3・N-T1 |
| unmanaged の報告 | prune しない範囲も List して、宣言に無いもの（unmanaged）を status に出す。List できない provider は「検出できない」と出す | E-D4・E-B6・N-I3・N-A7 |
| `max_drop` | List が急減したら消さない。自分が消した分と、allow で消した分は差し引く | E-K2・N-K2 |
| `external_owners` | 触らない。観測して見せ、待ちには使うが、Apply・Delete・prune はしない。除外は intent にだけ書く | E-D2・E-I5・E-T2・N-T2・E-O1・N-O3 |
| 範囲の重なりの拒否 | 同じ provider の Range が重なる宣言は、読み込み時にエラーにする | E-K8・N-K7 |
| 範囲の外の宣言の拒否 | Range がある provider で、どの Range にも入らない Resource は reject する（provider の `Validate` が判定） | E-I6・E-M3 |

### 3.3 Provider（装置とのやりとり）

**役割。** 1 つの type の装置について、観測・変更・削除・列挙をする。コアの外のモジュールで、コアは下の interface だけを知る。

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
    Delete(ctx context.Context, w WriteContext, id ID, current Observation) error // 拒否するときは *RefuseError{Reason}
    List(ctx context.Context, r Range) (Listing, error)                       // {IDs []ID; Complete bool}
}

type Schema struct {
    Version string                  // 比べ方・正規形が変われば上げる
    Kinds   map[string]KindSchema
}
type KindSchema struct {
    Fields      map[string]FieldSpec // {Type; Identity bool; Digest bool; Match func(desired, observed) bool}
    Settle      time.Duration        // Apply のあと読み直すまでの待ち
    ObserveOnly bool                 // 観測だけ。Apply・Delete を呼ばない
    DestructiveDelete bool           // 中身を失う type（volume・DB・バケット）
}
type FieldType int // String, Int, Bool, Duration, List(順序あり・全置換), Set(順不同), Map, Secret, Float(小数。/metrics の値)

// 任意の interface
type Transactor interface {
    Prepare(ctx context.Context, w WriteContext, changes []Change, opt TxnOptions) (Txn, error) // Change{ID; Delete; Desired; Current}・TxnOptions{Timeout}
}
type Txn interface {
    Commit(ctx context.Context) error
    Rollback(ctx context.Context) error // 任意。呼ばなくても相手のタイムアウトで戻ること
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
```

実装では `Provider` は観測だけの `Observer`（`Schema`・`Observe`）と列挙の `Lister`（`List`）をまとめた `Reader` に、`Apply`・`Delete` を足した形。plan と 1 cycle の判定は `Reader` だけを受け取り、現実を変える関数を型として持たない（不変条件 2）。

| 要素 | 内容 | ユースケース |
|---|---|---|
| `Observe` の 4 値 | Present・Absent・Unknown・Converging。届かない・読めない・中を見られない・定義に合わない出力は Unknown | E-K1・N-K1・E-A12・E-E6・E-B11 |
| `Converging` | 相手が作業中（保留中の BIOS・再起動待ち・非同期のジョブ）。重ねない・消さない・ineffective Apply に数えない | E-A4・E-D1・E-B14・E-A1 |
| `Observe(ids)` を一覧で | provider が一括 API を使える。結果は prune の根拠にしない | E-D8・N-D11・N-N1 |
| `Apply` | 冪等。List 型の field は全置換 | E-B12・E-D1 |
| `Delete` を分ける | 拒否する（中身がある・子がある・keep の tag がある）ときは理由を返す。コアは消さずに報告を続け、budget に数えない | E-F1・E-G1・E-C2・N-C2・N-F3 |
| `List` と `Complete` | 不完全なら（ページの打ち切り・権限不足）、その範囲を消さない | E-K2・E-F3 |
| `WriteContext` | Apply・Delete・Invoke・Prepare のときだけ渡す。provider は書き込み用の credential を、この中（`WriteContext.Secret`）でだけ取り出す | E-G5・N-G3 |
| `Schema.Version` | バージョンが変わった cycle を検出する（未実装、§8.1） | E-J12・N-S1・N-P3 |
| `FieldSpec.Identity` | 変われば作り直しになる field。物理の座標を選べるようにする | E-A10・N-A8 |
| `FieldSpec.Type` の List・Set | 順序のある規則は全置換、集合は順不同で比べる | E-B12 |
| `FieldSpec.Type` の Secret | 値を比べず、世代で比べる。表示は伏せる | E-G2・N-L5 |
| `FieldSpec.Type` の Float | 小数の値（/metrics の値・メモリの量）を、Condition の大小で比べる。NaN・±Inf は型に合わない | E-H4・N-T4 |
| `FieldSpec.Digest` | 観測は中身の sha256（`sha256:<hex>`）だけ。比べるときは宣言の値も sha256 にする。平文を観測・plan・Record に出さない（host のファイル・ユニットの中身） | E-E2・E-D5 |
| `FieldSpec.Match` | 宣言と観測の比べ方を provider が決める field（provider の正規形で吸収する）。例：宣言した slot・キーだけを比べる（VM の disks・nics・FW の options）・作るときだけ使う（template）・付け替えで名前の変わる volume を比べない・書き方の違い（`2G` と `2048`）をずれにしない。無ければ型の比べ方 | E-I7・E-D1・E-C2 |
| `FieldSpec.Destructive` | create・update・delete のどれで破壊的になるかを Schema に静的に書く。破壊的な変更は budget と承認に通す。**未実装**（§8.1）。いまは削除を `DestructiveDelete`、状況で決まる変更を `Classifier`、一回きりの操作を `OpSpec.Destructive` で言う | E-C1・E-A1・N-D8・N-G10・N-C4 |
| `Settle` | Apply のあと読み直すまで待つ | E-J6・E-R3 |
| `ObserveOnly` | 観測だけの type（在庫・operational な状態・golden config） | E-B7・E-E4・N-B5・N-B6・N-A7 |
| `DestructiveDelete` | 宣言から消えても、既定で消さない type | E-C3・N-C2 |
| `Transactor` | Prepare → confirm → Commit。Commit を呼ばなければ、相手のタイムアウトで戻る | E-B3・E-B8・E-B9・N-B2・N-D5・N-K9 |
| `Invoker` | 観測に残らない一回きりの操作（boot_once・reset・migrate・drain・upgrade）。Procedure からだけ呼ぶ | E-A2・E-B11・N-D4・N-E2・N-A11 |
| `Classifier` | 状況で破壊的になる変更（volume の付け替え・base chain の作成・保持の設定の変更）を、plan の時点で言う | E-C2・E-B3・E-C1 |
| `Validator` | 読み込み時に、provider 固有の誤り（範囲の外・confirm の先の不足・宣言だけで決まる値の誤り）を reject する。読み込みは相手に触れない。だから相手から読まないと分からない決まり（BIOS の attribute registry など）は、Apply の前に確認する | E-I7・E-A4・E-B3 |
| `DesiredObserver`（interface は持たない） | 宣言を見ないと観測できない縮退（Ansible の check mode・テンプレートファイル）。この provider は List と prune を持てない。コアは宣言を Observe に渡さないので、interface としては持たない。provider の `Validate` が宣言を ID ごとに覚え、`Observe` がそれを使う（guest-os の marks と同じ形。Stage 8f で決めた）。ずれは、宣言した field の 1 つ（ansible は `module`、template-file は `template`）の観測の値で表す | E-E3・E-E5・N-E5 |

### 3.4 Condition（観測についての三値の式）

**役割。** 依存の待ち・Procedure の前提・done_when・confirm・不変条件を、1 つの式で書く。値は真・偽・不明の三値。

**語彙（これ以上は持たない）。**

```
式         := 論理和
論理和     := 論理積 ("or" 論理積)*
論理積     := 否定 ("and" 否定)*
否定       := "not" 否定 | 比較
比較       := 項 (演算子 項)?            演算子: == != < <= > >= in contains
項         := 定数 | 参照 | quantifier | "(" 式 ")"
参照       := observe(<type>/<id>).<field>(.<field>)*
            | present(<type>/<id>) | absent(<type>/<id>)
            | desired.<field>              （Procedure の対象の宣言）
            | it.<field>                   （quantifier の中の 1 件）
            | check(<名前>)                （Go で登録した check）
quantifier := all(<選び方>, 式) | any(<選び方>, 式) | count(<選び方>, 式)
選び方     := <type>[<ラベル>=<値>, ...]
```

算術・文字列の加工・現在時刻は持たない。時間で決まる判断は、provider が「残り時間」「最後の成功からの経過」を観測の値として返して表す（例：`observe(pki/cert/www).remaining < "720h"`）。参照の `<type>/<id>` は `<provider>/<kind>/<名前>` の 3 つの部分を必ず書く。

**三値の決まり。**
- 参照が Unknown・Converging の対象を指すと、その参照は不明。不明を含む比較は不明。
- and・or・not は三値の論理（偽 and 不明 = 偽、真 or 不明 = 真）。
- `all` は 1 件でも偽なら偽。偽が無く不明があれば不明。`any` は 1 件でも真なら真。
- `count(...) op N` は、真の件数 k と不明の件数 u から決める。k だけで決まれば真偽、k〜k+u のどこかで結果が変われば不明。

**不明のときの扱い**は、使う側が書く：`on_unknown: wait`（待つ。既定）・`block`（halt する。容量の門）・`ignore`（真とみなす。報告だけの条件）。Transaction の confirm と Procedure の done_when では、不明は常に「満たしていない」。

```go
type Tri int // TriUnknown, TriTrue, TriFalse
type Condition interface {
    Eval(env ConditionEnv) Tri               // env は今の cycle の観測だけを引く。外へ問い合わせない
    Refs(sel func(Selection) []ID) []ID      // 観測すべき対象（コアが先に観測する）
    Direct() []ID; Checks() []string; String() string
}
type Disposition int // OnUnknownWait, OnUnknownBlock, OnUnknownIgnore
type CheckFunc func(ctx context.Context, obs ObservationView) (Tri, string)
```

| 要素 | 内容 | ユースケース |
|---|---|---|
| `observe(...).field` と比較 | 観測した値で待つ・確認する | E-A3・E-B13・E-H4・N-B17・N-T4 |
| `present`・`absent` | 在る・無いで待つ | E-A8・E-I5 |
| `desired.field` | Procedure の対象の宣言と観測を比べる（done_when） | E-A5・E-A6・E-F2 |
| `check(名前)` | 式で書けない確認（相手の機器から見た経路）を Go で登録する | E-B9 |
| `all`・`any`・`count` | 集合の上の条件（MTU がそろう・primary が 1 つ・95 台以上が健全） | N-B16・N-F4・N-B18・N-J3 |
| 三値と `on_unknown` | 不明を偽とも真とも決めず、使う側が選ぶ | E-C4・N-C8・E-K1・N-H6 |
| 時刻を読まない | 期限は provider が残り時間を返す | N-G4・N-R2・E-I4・N-J10 |
| 観測だけを読む | Eval は今の cycle の観測だけを見る。cycle をまたいだ値を読まない | E-J9（plan と reconcile が同じ判定をする） |

### 3.5 Graph（順序と待ち）

**役割。** `depends_on` と Condition から、cycle の中の順番（wave）と、cycle をまたぐ待ちを決める。Procedure の step の並びも同じ Graph で扱う。

| 決まり | 内容 | ユースケース |
|---|---|---|
| wave | 依存先を先に Apply する。循環は読み込み時にエラーにする | E-J7・E-M3 |
| 待ち | `when` が真になるまで、下流は操作なし（Event `Waiting`）。偽でも不明でも待つ（`block` のときは halt して通知する） | E-I5・E-B13・E-C4 |
| 同じ cycle の再観測 | 上流の Apply が効いたら、観測できなかった下流を同じ cycle に再観測して Apply する。上限は 3 回・2 分 | E-J7 |
| 下流を同じ cycle に進めない | 上流の Apply が失敗・未確認なら、下流は次の cycle | E-D6 |
| 同じ Resource に Apply は 1 つ | 同じ対象に 2 つの Apply を同時に走らせない。Apply 中のものは budget に数える | E-D6 |
| external owner を待つ | external owner のリソースも観測して、待ちに使える | E-I5・N-T2・E-N3・N-N2 |
| 下流の表示 | plan と `/graph` に、変わる Resource の下流を出す | N-J11・E-H5 |

### 3.6 Policy（いつ・どこまで変えてよいか）

**役割。** 「この範囲で、いつ、どこまで変えてよいか」を 1 つの型で書く。freeze・保守の window・budget・承認・大量作業の allow・報告だけの範囲は、全部 Policy か一時的な Policy で書く。

```yaml
cycle:
  interval: 60s                       # daemon の cycle の間隔
  stuck_after: 5                      # 同じ状態の Event がこの cycle 数続けば stuck に上げる
policies:
  - name: default
    select: {all: true}
    mode: apply
    budget:
      per_cycle: 20                   # 1 cycle の変更の数（Apply 中を含む）
      destructive_per_cycle: 3        # 破壊的な変更は別に数える
      destructive_per_window: 10      # cycle をまたいだ累積
      window: 24h
      max_ineffective_applies: 3      # ineffective Apply の上限
  - name: platform
    select: {labels: {owner: platform}}
    windows: ["Sat 02:00-04:00 Asia/Tokyo"]   # window の外は report
    approval: {quorum: {n: 2, of: [alice, bob, carol]}}
  - name: legacy
    select: {labels: {site: legacy}}
    mode: report                      # 差分は通知するだけ
  - name: decommission-rack-3         # 大量の正当な作業（一時的な allow）
    select: {labels: {rack: "3"}, type: bmc/system}
    allow: {kind: reimage, max_count: 48}
    windows: ["2026-11-05T22:00+09:00 .. 2026-11-06T06:00+09:00"]
    approval: {quorum: {n: 2, of: [alice, bob]}, separation: true, cooling_off: 1h}
    applicant: carol
    reason: "ラック 3 の reimage"
policy_sets:                          # daemon ごとに使う Policy の組（--policy-set）。未実装
  observer: [default, legacy, {name: all-report, select: {all: true}, mode: report}]
```

一時的な Policy（freeze・その場の report）は CLI が Record に書く。理由と期限が必須。

```console
$ sakutei freeze --reason "障害対応中" --until 2026-11-01T09:00+09:00
$ sakutei freeze --select labels.site=a --mode report --reason "手作業中" --until 4h
$ sakutei unfreeze <name> --reason "対応終わり"
```

```go
type Policy struct {
    Name      string
    Select    Selector       // {All; Types; Labels}
    Mode      Mode           // Apply | Report
    Windows   []Window
    Budget    *Budget        // {PerCycle; DestructivePerCycle; DestructivePerWindow; Window; MaxIneffectiveApplies}
    Approval  *Approval      // {N; Of; Separation; Timeout; CoolingOff}（intent では quorum: {n, of}）
    Applicant string         // 申請者。separation: true のときは必須
    Allow     *Allow         // {Kind: delete|update|create|reimage|destructive; MaxCount}
    Until     time.Time; Reason, By string // 一時的な Policy と allow
    // ObserveEvery int       // 既定 1（毎 cycle）。未実装
}
// most-restrictive-wins の合わせ方は plan の判定（v3/plan）の 1 か所にある
```

| field | 内容 | ユースケース |
|---|---|---|
| `select` | type・ラベルで適用対象を選ぶ。宣言に無いもの（Range のラベル）にも効く | E-M2・N-M2・E-M1 |
| `mode: report` | 差分は通知するだけ。観測と通知は続ける | E-I3・E-Q1・N-Q2・N-I1 |
| `windows` | window の外は report。Procedure の起動にも効く。allow の Policy では、window は allow が開く時間（window の外は allow を閉じ、通常の budget で数える）。書き方は 3 つ：毎週 `"Sat,Sun 02:00-04:00 Asia/Tokyo"`（終わりが始めより前なら日をまたぎ、曜日は開く日）・決まった期間 `"2026-11-05 22:00 .. 2026-11-06 06:00 Asia/Tokyo"`・`"*"` | E-J3・E-R1・N-J1・N-R3・N-A3 |
| `budget.per_cycle`・`destructive_per_cycle` | 一度に変えすぎない。超えたら、超過分だけ保留するのでなく halt する | E-J1 |
| `budget.destructive_per_window`・`window` | cycle をまたいだ「じわじわ全消し」を止める。使用量は Record に置く | E-J1 |
| `budget.max_ineffective_applies` | ineffective Apply を繰り返さず halt する（人とツールの取り合い） | E-J6・E-K10・E-Q4・N-J12・N-Q5 |
| `approval` | N-of-M・申請者≠承認者・cooling-off・期限。Procedure の gate と同じ型。`separation: true` には `applicant` が要る（申請者が分からない依頼は申請者≠承認者を確かめられないので reject する） | E-L2・N-L2・E-J10・N-G10 |
| `allow` | 大量の正当な作業のための、通常の budget とは別の枠。budget の免除ではない（max_count で数えて止まる）。決まりは次のとおり：<br>・allow の Policy は、該当する変更を縛らない。window の中で承認がそろい、cooling-off が過ぎたときだけ開く。<br>・開いた allow に入れた変更は、通常の budget に数えない。通常の budget を超えても halt しない（halt するのは宣言の外の想定外の大量変更）。宣言の外は通常の budget で数える。<br>・枠は実行の直前に使い（Record の marks の `lanes`）、失敗しても戻さない。<br>・`reimage` は、同じ type・同じラベルの削除と作成の対を 1 と数える。<br>・承認は Policy ごとに 1 つの依頼（変更ごとではない）。`approval.cooling_off` は宣言を初めて見た時刻から数える。`quorum` を書かず `cooling_off` だけなら、承認者を置かない allow。<br>・`select: {all: true}` は reject する | E-J2・E-A9・N-J2・N-A12・N-E9 |
| `until`・`reason`・`by` | 一時的な Policy。期限で消える。読めなければ止める側に寄せる | E-J4・N-Q1・E-R2・N-R1 |
| 合わせ方 | 複数が該当すれば、最も厳しいものが勝つ（report は apply より、承認ありは無しより、小さい budget は大きい budget より） | E-J5・E-S1 |
| `policy_sets` と `--policy-set` | 同じ intent で、観測だけの daemon と進める daemon を並べる。宣言に無い名前は reject する。**未実装**（§8.1） | E-M5・N-M7・E-S5・E-J11 |
| `observe_every` | 遅い装置の観測を間引く（任意。初回・未収束・desired が変わった cycle は必ず観測）。**未実装**（§8.1） | E-N2 |
| `cycle.interval`・`stuck_after` | cycle の間隔と、続いた状態を stuck に上げる cycle 数 | E-J11・E-H1 |
| provider のバージョンでの停止 | バージョンが変わった provider の操作は、その cycle を report にする。その cycle に差分が出なければ新しいバージョンを受け入れ、出たら `sakutei accept-version <provider>` まで report。**未実装**（§8.1） | E-J12・N-S1・N-P3 |

### 3.7 Procedure（一回きりの手順）

**役割。** 非冪等な操作・多段の作業・人の作業を、cycle をまたいで 1 step ずつ進める。v2 の単純 Task・多相 Task・Workflow・manual は、すべてこれにまとめる。進捗は Record に置き、「完了」は観測で決める。

下の例のうち、`trigger: {policy: …}`・`disruption`・`on_failure` の rollback は未実装（§8.1）。いまの `trigger` は `on_drift` と `manual`、`on_failure` は `halt` だけ。

```yaml
procedures:
  - name: bmc-fw
    target: {type: bmc/firmware, select: {labels: {rack: "3"}}}   # ID は <node>/<部品>
    trigger: on_drift                     # 宣言と観測がずれたら対象ごとに 1 回
    drift_fields: [version]
    concurrency: 1
    invariant: 'count(bmc/system[role=gpu], it.power == "On") >= 30'
    steps:
      - name: approve
        gate: {approval: {quorum: {n: 1, of: [bob]}}, timeout: 24h}
      - name: verify
        invoke: {op: verify, args: {image_uri: "http://192.0.2.5/bmc-2.0.0.bin", sha256: "…"}}
      - name: stage
        critical: true                    # ここで止まったら自動で再開しない
        skip_if: 'observe(bmc/firmware/{target}).version == desired.version'
        invoke: {op: stage, args: {image_uri: "http://192.0.2.5/bmc-2.0.0.bin"}}
      - name: staged
        wait: {when: 'observe(bmc/firmware/{target}).update_state == "Completed"', timeout: 1h}
      - name: reboot                      # 再起動で効く部品（BIOS・apply_time: OnReset）だけ
        invoke: {op: reset}
      - name: confirm
        wait: {when: 'observe(bmc/firmware/{target}).version == desired.version', timeout: 30m}
    done_when: 'observe(bmc/firmware/{target}).version == desired.version'
    on_failure: halt
    timeout: 6h

  - name: os-install
    trigger: {policy: decommission-rack-3}   # allow が開いたときだけ、対象ごとに枠を 1 使う（未実装）
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
    trigger: manual                       # sakutei start gateway-upgrade で起動する
    disruption: {estimate: 15m, affects: "拠点 a の外向き通信", notify_before: 30m}   # 未実装
    steps:
      - wait: {when: 'observe(metrics/series/mq/upstream_queue).value < 100', timeout: 2h}
      - gate: {approval: {quorum: {n: 1, of: [alice]}}}
      - policy: {select: {labels: {site: a}}, mode: report}   # この Procedure の間だけ効く
      - invoke: {op: upgrade}
      - wait: {for: 10m}
```

```go
type Procedure struct {
    Name         string
    Target       ProcTarget       // {Type; ID; Labels}（intent では id か select: {labels: …}）
    Trigger      Trigger          // {Kind: OnDrift | Manual; Fields}。Policy(name)・BeforeDelete は未実装
    Precondition, DoneWhen, Invariant string // 書いたままの式。{target} を置き換えてから Condition として読む
    Steps        []Step
    Concurrency  int              // 既定 1
    Timeout      time.Duration
    // OnFailure（rollback の step）・Disruption は未実装。on_failure は halt だけ
}
type Step struct {
    Name     string
    Kind     StepKind             // Apply | Invoke | Manual | Wait | Gate | Policy（Exec は未実装）
    Apply    *Resource
    Op       string; Args Fields
    Instruction, URL string
    When     string; For time.Duration; Timeout time.Duration
    SkipIf   string; Critical bool
    Gate     *Approval; Policy *Policy
}
```

| field | 内容 | ユースケース |
|---|---|---|
| `target` | 対象の type・ID・選び方。`{target}` と `desired.field` で step に差し込む | E-A5・N-J3 |
| `trigger: on_drift` | 指定した field がずれたら、対象ごとに 1 回起動する。同じ対象に未完了の Procedure は 1 つだけ | E-F2・E-A6・N-F2・E-G8 |
| `drift_fields` は収束ループで Apply しない | Procedure が受け持つ field は、収束ループが Apply せず、Procedure を起動するだけ | E-F2・E-A5・N-A4 |
| `trigger: {policy: 名前}` | その Policy の allow が開いたときだけ起動し、枠を使う。**未実装**（§8.1） | E-A9・N-A12 |
| `trigger: before_delete` | 削除の前に Procedure を挟む（最後のバックアップ）。**未実装**（§8.1） | E-C3・N-C3 |
| `trigger: manual` | 人が `sakutei start` で起動する | N-F4・N-K8・N-D4 |
| 定期の起動は持たない | 定期の仕事は Resource の古さとして書く（例：`backup/job` の `max_age: 24h`） | N-J10・N-C4・N-C5・N-H7 |
| `precondition` | 始める前の条件 | E-A8 |
| step `apply` | Resource を Apply して進む | E-A3・E-A8・N-B10 |
| step `invoke` | 一回きりの操作。送る前に送信中の記録を Record に書く。再開時に記録があれば、送らずに止める | E-A2・N-A2・N-A11・N-D4・N-E2・N-D9 |
| step `exec` | 外のコマンド（playbook など）を 1 回動かす。**未実装**（§8.1） | N-T3・E-T1 |
| step `manual` | 人の作業。観測で完了にし、人の「やった」は完了の根拠にしない | E-A6・E-Q3・N-A5・N-Q4・N-B19 |
| step `wait` | `when`（条件）か `for`（時間）を待つ。不明なら待つ | E-A8・N-B11・N-F6・N-G5・N-R4 |
| step `gate` | 承認を待つ。型は Policy の approval と同じ | E-A5・E-L2・E-J8 |
| step `policy` | Procedure の間だけ効く一時的な Policy（範囲を report に）。Procedure が終われば消える | E-Q3・N-D4 |
| `skip_if` | 完了している step を飛ばす（再開のとき） | E-A5 |
| `critical` | 書き込み中に止まると壊れる step。止まったら自動で再開せず、gate に回す。one-shot 実行では進めない | E-A5・E-K5・N-K5 |
| `done_when` | 完了したかを観測で決める。Record を失っても二度走らない | E-A9・E-A6・E-F2 |
| `invariant` | step を進める前に毎回確認する、集合の条件 | N-B16・N-F4・N-B18・N-J3 |
| `concurrency` | 同時に進める対象の数（ローリング） | N-J3・N-E2・N-A4 |
| `on_failure` | halt する（既定）か、rollback の step を流す。**rollback は未実装**（§8.1） | E-F2 |
| `disruption` | 全体を止める作業の見積もりと影響を gate に見せ、止める前に Event `Disruption` を出す。**未実装**（§8.1） | E-J8・E-H6・N-J8 |
| `timeout` | Procedure 全体の時間の上限。過ぎたら stuck | E-A6・E-R2 |
| halt した対象に触らない | halt した Procedure の対象は、収束ループでも宣言どおりに戻さない | E-A9 |

### 3.8 Expand（intent から Resource を作る）

**役割。** intent と外部入力から、Resource・Range・Policy・Procedure の一覧を作る純粋な変換。同じ入力には同じ出力を返す。合成リソース・テンプレート・network policy の変換・compatibility table・採番の値の差し込みはここでする。コアは変換の中身を知らない。

```yaml
inputs:
  - name: inventory                    # 中身の在りかは接続設定に書く
    items: hosts                       # 件数を数えるリスト（書かなければ最上位の field の数）
    expected_min: 40                   # これより少なければ「届いたが欠けている」
    max_drop: 50%                      # 前の cycle より件数がこれ以上減れば「届いたが欠けている」
expand:
  - kind: go:template                  # Go で登録した変換（自分が読む field templates: を宣言する）
  - kind: go:netpolicy                 # network_policy: を fw/chain・pve-fw/rules に展開
  - {kind: "exec:compat", keys: [compatibility]}   # 外のコマンド（在りかは設定の expanders.compat）。stdin に入力の JSON、stdout に出力の JSON
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
    Intent []Document
    Inputs map[string]ExternalInput // {Data any; Reachable bool; Note string}
    Prior  Output                   // それより前の Expand の出力（読むだけ。compatibility table の照合が使う）
}
type Output struct {
    Resources   []Resource
    Ranges      []Range
    Policies    []Policy
    Procedures  []Procedure
    Cycles      []CycleSettings          // intent の cycle:（2 つ以上は reject）
    Pools       []Pool
    Allocations []Allocation             // intent のファイル allocations/<pool>.yaml
    DependsOn   map[ID][]Dependency      // 前の宣言に足す依存（変えてよいのはこれだけ）
    Diagnostics []Diagnostic             // {Severity: Reject|Warn|Uncertain; Target; Message}
}
type Expander interface {
    Keys() []string                      // intent の最上位で読む field
    Expand(ctx context.Context, in Input) (Output, error) // 足す分だけを返す
}
type Allocator interface {
    Pick(pool Pool, taken func(int64) bool, req AllocRequest) (int64, error) // 既定（FirstFree）は空きの先頭
}
```

| 要素 | 内容 | ユースケース |
|---|---|---|
| Go の Expand と外のコマンドの Expand | 合成リソース・全台共通の設定・台ごとの差の展開 | E-A11・N-A14・N-E7・N-B15・N-A11・N-E8 |
| 診断 Reject | 入力の意味の誤り（重なるネットワーク・理由の無い例外・compatibility table に無いバージョンの組）を、読み込み時にエラーにする | E-B5・E-A7・N-B7 |
| 診断 Warn | 隠れて効かない許可などを通知する | E-B5 |
| 診断 Uncertain | その Resource を Unknown 扱いにする（観測も Apply もしない、下流は待つ）。Target が type か provider なら、宣言が欠けているかもしれないので、その provider の範囲を消さない | E-G4・E-K3 |
| 外部入力が届かない | その入力に依る Resource を Unknown にする。空の desired を作らない | E-K3・E-K4・N-K3・N-S4 |
| `expected_min`・`max_drop` | 届いたが欠けている入力では消さない。前の cycle の件数は Record の marks に置く | E-K3 |
| secret の世代 | Expand のあとで、コアが参照ごとに世代だけを読み、参照に付ける（`<backend>:<在りか>@gen=<世代>`）。Expand には値も世代も渡さない。世代が変われば Apply する（値は provider が Apply の直前に `WriteContext.Secret` で取り出す） | E-G2・E-G3・N-G2 |
| 採番の要求 | Expand は値を知らず、宣言の field に `{from_pool: …}` を書いて出す。コアが intent のファイル `allocations/<pool>.yaml` の値を差し込み、無ければ空きを選ぶ（plan は選ぶだけ）。cycle はロックを持って、Apply の前にファイルとコピーに書き足す | N-B13・N-B12・N-A14 |
| `blocks`・`reuse: false`・`template` | 区切った範囲・再利用しない・文字列への埋め込み | N-B13 |
| 解放は人がする | 宣言から消えても値を返さない。`sakutei alloc release` で人が返す | N-B13・N-J4 |
| `Allocator.Pick` の差し替え | 外の採番権威を使う | E-T4・N-T5 |
| `Now`（任意・丸めた時刻） | 期限つきの宣言（期限つきの許可）を Expand が落とす。`Input` に日か時に丸めた時刻を足し、要る Expand だけが受け取る。**未実装**（§8.1）。期限つきの宣言を書く Expand と一緒に足す | N-B8・N-R1 |
| 入力の差し替え | 本番の intent に、fake の外部入力・fake の secret・fake provider を差し込む（ツイン） | E-P4・N-P2・N-K8 |
| 純粋さ | 同じ入力に同じ出力。golden のテストで確かめられる | N-P5 |
| 出力のハッシュ | `--expect-revision` と原因の判定に使う。**未実装**（§8.1） | E-J9・E-L5・N-J9・E-J13 |

### 3.9 Record と Events（cycle をまたぐ記録と、外への通知）

**役割。** Record は、cycle をまたいで覚えるものの唯一の置き場（ディレクトリ 1 つと flock。形は §5）。観測の値は置かない。Events は、今の状態（level Event：firing/resolved）と出来事（edge Event）を外へ流す。

| 要素 | 内容 | ユースケース |
|---|---|---|
| Procedure の進捗 | step・送信中の記録・始まりの時刻 | E-K5・N-K5・E-A2・E-F2 |
| 承認 | 誰が・いつ・何を。承認・拒否・期限切れ・取り消しも残す | E-L2・E-L6・N-L3・E-M4 |
| 一時的な Policy | freeze・その場の report。理由と期限 | E-J4・N-Q1・N-Q2 |
| 前の cycle の marks | Range ごとの List の件数・外部入力の件数・Resource ごとの ineffective Apply の回数・budget の window の使用量・allow の使用量と cooling-off の始まり。設計ではさらに provider のバージョンと Expand の入力・出力のハッシュを置く（未実装、§8.1） | E-J12・E-K2・E-K3・E-J13・E-J6・E-J1・E-J2 |
| 通知の状態 | firing の Event の一覧（count・since） | E-H1・E-O3 |
| outbox | sink ごとの送信位置。送れなくても収束を止めない | E-H2・N-H2 |
| audit のハッシュチェーン | Apply・Delete・prune・承認・freeze・Procedure の step・採番を、前の行のハッシュでチェーンにする。値は書かない。設計ではバージョンの受け入れも残す（未実装） | E-L1・N-L1・N-L7・E-K7 |
| 採番のコピー | intent のファイルのコピー。食い違えば、そのプールから新しい値を出さない | N-B13・E-K4 |
| health | 最後の cycle の時刻・続いた誤りの数・最後の誤り | E-K11・N-H4 |
| 置き場は 1 つ | v2 の状態ファイル 9 種を、1 つのディレクトリにまとめる | E-S2・N-S5 |
| ロック | 進める daemon は 1 つ。取れない daemon は進めない cycle を回す | E-M5・N-M7・N-S3 |
| Events の sink | Alertmanager・webhook・syslog・OTLP・metrics は外のモジュール | E-H2・E-H3・N-H3・N-H5・E-T3・N-J6 |
| read-only HTTP API | 状態を機械で読む。遅い読み手で cycle を止めない | E-H5・E-L4・N-L4・N-L6 |
| wake | 次の cycle を今すぐ始める | E-N5・E-M6 |

Record は interface でなく、ディレクトリを扱う構造体（`record.Record`）。主なメソッドは次のとおり。

```go
type Record struct{ /* dir */ }
func (r *Record) Lock() (release func(), held bool, err error)
func (r *Record) Runs() ([]Run, error)              // WriteRun・RemoveRun
func (r *Record) Marks() (Marks, error)             // WriteMarks
func (r *Record) WriteRequest(req ApprovalRequest, fresh bool) error
func (r *Record) Commands() ([]Command, error)      // sakutei start・cancel の依頼
func (r *Record) EventState() (EventState, error)   // 通知の状態。AppendOutbox・Cursor・AuditCursor
func (r *Record) Audit() (*Audit, error)            // Append・Verify。ハッシュチェーン
func (r *Record) WriteAlloc(a core.Allocation) error
func (r *Record) Health() (Health, error)           // RecordCycle

type Event struct {
    Seq      uint64; Kind string   // Drift・Unknown・Halt・ApprovalPending…
    Type     EventType             // state（level Event）| incident（edge Event）
    State    string                // firing | resolved（level Event のとき）
    Resource string; Scope string  // Scope は対象が Resource でないとき（範囲・Policy）
    Labels   map[string]string
    At       time.Time; Since *time.Time; Count int; Stuck bool
    Message  string
    Values   map[string]int        // CycleDone の件数など
}
type Sink interface {
    Deliver(ctx context.Context, batch []Event) error // 失敗しても cycle を止めない。次の cycle に同じ位置から
}
type StateSink interface { // 任意：cycle ごとに今の firing の一覧も受け取る（Alertmanager の再送・metrics のゲージ、E-H2・E-H3）
    DeliverState(ctx context.Context, firing []Event) error
}
type AuditSink interface { // 任意：streams: [audit] で audit のハッシュチェーンの行をそのまま受け取る（syslog、E-L1・N-L1）
    DeliverAudit(ctx context.Context, batch []AuditLine) error
}
```

---

## 4. 不変条件

検討文書 §5.4 の 35 項を、意味の重なりをまとめて 30 項にした。テストの種類は次のとおり。

- **性質**：fake provider でランダムな観測の並び・故障を与えて確かめる、性質のテスト
- **契約**：provider SDK の契約テスト
- **読込**：読み込みの検証のテスト
- **単体**：その部品の単体テスト
- **往復**：one-shot 実行と daemon で同じ結果になるかのテスト

| # | 不変条件 | 守るテスト |
|---|---|---|
| **観測** | | |
| 1 | 観測を cycle をまたいで持ち越さない。Record に観測の値を書かない。plan の観測を reconcile に使わない | 性質：Record の全ファイルに観測の値が現れない／単体：plan のあとの reconcile が Observe を呼び直す |
| 2 | Observe は desired を受け取らない。比べる処理は現実を触らない | 単体：Diff は provider を持たない型で書く（コンパイルで守る） |
| 3 | Unknown ≠ Absent。Unknown の Resource には Apply・Delete・prune をせず、下流も進めない | 性質：どの観測の並びでも Unknown の対象に書き込みが出ない／契約：届かないとき Absent を返さない |
| 4 | Converging の相手には重ねない。ineffective Apply に数えない | 性質 |
| 5 | 依存（secret backend・Record）が使えない間は、それに依る Resource の観測を試みない | 単体 |
| **減算** | | |
| 6 | 消すのは「Range を宣言し、List が完全で、宣言に無く、external owner のものでなく、prune を許し、budget と承認を通った」ときだけ。宣言から外しただけでは消さない | 性質：条件のどれか 1 つを外すと消えない |
| 7 | List が不完全な範囲・前の cycle より急減した範囲は消さない。自分と allow が消した分だけ差し引く。前の cycle の件数が無ければ消さない | 性質：List の欠損を注入 |
| 8 | Observe の結果を prune の根拠にしない（List の ID だけ） | 単体：prune の関数の引数は Listing だけ（型で守る） |
| 9 | 中身を失う type は既定で消さない。破壊的な変更は budget に数え、承認が要るものは承認まで流さない。allow は budget の免除ではない | 性質：承認なしで破壊的な変更が出ない |
| 10 | provider が拒否した Delete は消さずに報告し続け、budget に数えない | 性質 |
| 11 | 採番の値は宣言から消えても返さない。`reuse: false` は二度と払い出さない。ファイルとコピーが食い違えば、そのプールから新しい値を出さない | 単体：ファイルを巻き戻して確かめる |
| **Apply と Procedure** | | |
| 12 | 同じ Resource に同時に 2 つの Apply を走らせない。上流が完了しない下流を同じ cycle に進めない。Apply 中のものは budget に数える | 性質（遅い Apply を注入） |
| 13 | 恒久的な失敗は、desired か provider のバージョンが変わるまで再試行しない | 単体 |
| 14 | Apply のあと読み直し、効かなければ数えて、上限で halt する。Unknown は数えない | 性質（成功を返すが変わらない provider） |
| 15 | 非冪等な操作は Procedure からだけ。同じ対象に未完了の Procedure は 1 つ | 読込：Invoke を Resource に書けない／性質 |
| 16 | 一回きりの操作は送信中の記録を先に書き、記録があれば二度送らずに止める。`critical` の step は自動で再開しない。one-shot 実行は `critical` の step を進めない | 性質（送信の途中でプロセスを止める） |
| 17 | Transaction は confirm が通らなければ Commit しない。group は 1 つでも落ちれば、全部 Commit しない。`before` の confirm が通らなければ変えない。Commit を呼ばない限り、相手の側で戻る | 性質／契約：Prepare のあと放置すると戻る |
| 18 | Procedure の「完了」は観測で決める。不明は完了ではない。halt した Procedure の対象を自動で戻さない | 性質 |
| **方針** | | |
| 19 | 該当する Policy のうち、最も厳しいものが勝つ。起動フラグ・一時的な Policy は締める向きにしか効かない。report でも観測と通知は続ける | 単体：Policy の合わせ方の全組み合わせ |
| 20 | budget を超えたら、超過分だけ保留するのでなく halt する（開いた allow に入れた変更は別の枠で数える）。cycle をまたぐ window を数える | 性質 |
| 21 | 承認では申請者≠承認者を強制し、要件を満たす異なる承認者だけを数える。承認・拒否・期限切れを audit に残す。Record に書けなければ allow も承認も使わない | 単体／性質（Record の書き込み失敗を注入） |
| 22 | 一時的な Policy は理由と期限を持ち、期限で消える。Record が読めない cycle は止める側に寄せる | 単体（時計の差し替え） |
| 23 | provider のバージョンが変わった cycle は、その provider の変更を report にする（未実装、§8.1） | 性質 |
| **宣言と設定** | | |
| 24 | Range・除外・方針・採番の結果は intent にだけ置く。設定は接続の情報だけ（設定に範囲を書く field が無い） | 読込：設定の schema に範囲の field が無い |
| 25 | 同じ ID・未知の field・循環・範囲の重なり・範囲の外の宣言・全部を外す除外は、読み込み時にエラーにする | 読込 |
| 26 | Expand は同じ入力に同じ出力を返す。外部入力が届かないとき、空の desired を作らず、依る Resource を Unknown にする | 単体（golden）／性質 |
| **secret と出力** | | |
| 27 | secret の値をコアが持たない・比べない。Record・Event・エラーメッセージ・HTTP に出さない。書かない cycle に書き込み用の credential を取り出さない | 性質：テスト用の secret の値が全出力に現れない／契約：WriteContext の外で書き込み用の credential を読まない |
| 28 | 通知・audit の送信が失敗しても、収束を止めない。audit のハッシュチェーンを書き換えない | 性質（sink の失敗を注入）／単体：`audit verify` |
| 29 | 確認できなかった cycle に、level Event を resolved にしない。観測しなかった値を 0 や「ずれ無し」として出さない | 性質 |
| 30 | plan は何も消費せず、reconcile と同じ判定をする。one-shot 実行と daemon は、同じ Record から同じ結果を出す。テキストと JSON は同じ構造から作る | 往復：同じ観測の並びで、plan・one-shot 実行の列・daemon を比べる |

---

## 5. Record・Events・read-only HTTP API・起動フラグ・接続設定

### 5.1 Record のディレクトリ

```
record/
├── LOCK                         # flock。進める daemon（または one-shot 実行）が持つ
├── format                       # Record の形のバージョン。知らないバージョンなら読まずに止める
├── wake                         # sakutei wake が書く。daemon が見て次の cycle を始める
├── runs/<procedure>/<target>.json   # cycle だけが書く。完了・取り消しの Procedure は消す（履歴は audit）
├── approvals/<request-id>/
│   ├── request.json             # 依頼。cycle が書く（要件・申請者・期限・pending/used/rejected/expired）
│   └── by-<承認者>.json          # 判断。CLI と承認ツールが書く。1 人 1 ファイル（書き込みは一時ファイル＋rename）
├── temp-policies/<name>.json    # CLI が書く。終えた印は <name>.json.end（cycle と CLI が同じファイルを書き合わない）
├── commands/<時刻>-<種類>-*.json  # sakutei start・cancel の依頼。cycle が audit に残して消す
├── marks.json                   # 前の cycle の marks
├── events/
│   ├── state.json               # firing の一覧
│   ├── outbox/000000000123.jsonl  # outbox（seq 順。名前は最初の seq の 12 桁。古いものは全 sink が送ったら消す）
│   └── cursors/<sink>.json      # audit を送る sink は cursors/audit/<sink>.json も
├── audit/000007.jsonl           # audit のハッシュチェーン（大きくなれば新しいファイルへ回す）
├── alloc/<pool>.json            # 採番のコピー
└── health.json
```

すべて JSON。書き込みは一時ファイルに書いて rename する。Record を失ったときは安全側に寄る：
- marks が無ければ消さない（不変条件 7）
- provider のバージョンの受け入れを待つ（23。未実装）
- 進捗が無い Procedure は、done_when で完了していれば起動しない（18）
- 送信中の記録が無い操作は、観測で完了しているかを見てから起動する

`runs/…/<target>.json` の例：

```json
{
  "procedure": "bmc-fw", "target": "bmc/firmware/r3-n07/BMC", "run_id": "r-20261105-0012",
  "started_at": "2026-11-05T22:03:10+09:00",
  "step": 3, "step_name": "stage", "step_started_at": "2026-11-05T22:41:00+09:00",
  "sending": {"step": 3, "op": "stage", "at": "2026-11-05T22:41:02+09:00"},
  "state": "running"
}
```

`state` は `running` か `halted`。ほかに、`sakutei start` の申請者 `started_by`・止めた理由 `reason`・再開の承認 `resume`・step `policy` の一時的な Policy `policies` が入る。gate の承認は `approvals/` に置く。

`marks.json` の例：

```json
{
  "cycle": 18234, "at": "2026-11-05T22:41:00+09:00",
  "range_counts": {"dns:zone=example.com.": 412},
  "input_counts": {"inventory": 48},
  "ineffective": {"pve/vm/app01": {"count": 1, "desired": "sha256:…"}},
  "budget_windows": {"default": {"start": "2026-11-05T00:00:00+09:00", "destructive": 4}},
  "lanes": {"decommission-rack-3": {"fingerprint": "sha256:…", "used": 12, "seen": "2026-11-04T10:00:00+09:00"}}
}
```

設計では、さらに provider のバージョン（`provider_versions`）と、intent・外部入力・Expand の出力のハッシュ（`hashes`）を置く（未実装、§8.1）。

audit の 1 行：

```json
{"seq": 88121, "prev": "sha256:…", "at": "…", "kind": "resource.apply", "resource": "pve/vm/app01",
 "fields": ["memory"], "result": "ok", "policies": ["platform"], "approved_by": ["alice", "bob"],
 "reason": "CHG-1234"}
```

ほかに、種類に応じて `error`・`by`・`request`・`procedure`・`run_id`・`step`・`op`・`pool`・`value` が入る。設計では原因（`cause`）と `intent_revision` も残す（未実装、§8.1）。

### 5.2 Events

| 種類 | 型 | いつ |
|---|---|---|
| `Drift` | level | 宣言とずれている（report・window の外・承認待ちを含む） |
| `Unknown` | level | 観測できない・Expand が Unknown にした |
| `Converging` | level | 相手が作業中（stuck_after で stuck に上がる） |
| `Unmanaged` | level | 範囲の中の、宣言に無いもの |
| `Waiting` | level | 依存の条件を待っている |
| `ApprovalPending` | level | 承認待ち（Policy・gate） |
| `Halt` | level | budget 超過・ineffective Apply の上限・急減・Procedure の halt（provider のバージョンの変化は未実装） |
| `Frozen` | level | 一時的な Policy が効いている |
| `DeleteRefused` | level | provider が削除を拒否した |
| `ProcedureNeedsDaemon` | level | one-shot 実行が critical の step の手前で止まった |
| `Applied`・`Deleted`・`ProcedureStep`・`CycleDone` | edge | 起きたとき 1 回 |
| `Disruption` | edge | 全体を止める作業の前（未実装、§8.1） |

ロックが取れない cycle は Event でなく、結果の `advanced_elsewhere` で示す（§2.1 の Phase 0）。

level Event（`type: state`）は、キー（種類＋対象）ごとに、firing になった cycle と resolved になった cycle にだけ outbox へ積む。確認できなかった cycle はそのまま firing。stuck は種類でなく、Event の `stuck` で示す。edge Event は `type: incident`。`CycleDone` には cycle の時間と件数（drift・unknown・budget の残り）を `values` に載せる。metrics と OTLP の sink は、これから値を作る。

```json
{"seq": 5521, "kind": "Drift", "type": "state", "state": "firing", "resource": "dns/record/www.example.com./A",
 "labels": {"site": "a", "owner": "platform"}, "at": "2026-11-05T22:42:00+09:00", "since": "2026-11-05T22:40:00+09:00", "count": 3,
 "message": "value: 192.0.2.10 → 192.0.2.11（report: window の外）"}
```

### 5.3 read-only HTTP API

| パス | 返すもの |
|---|---|
| `GET /status` | 最後の cycle の結果（Resource ごとの状態・該当した Policy・unmanaged・承認待ち・Procedure の進捗）。まだ cycle を回していなければ 503 |
| `GET /plan` | 今の intent と観測で計算した plan。何も消費しない。`plan --json` と同じ構造 |
| `GET /events` | outbox を SSE で。`?since=<seq>` |
| `GET /graph` | Resource と Procedure の依存（下流の表示） |
| `GET /health` | health。続いた誤りがあれば 503 |

TLS と、トークン・クライアント証明書（`tls.client_ca`）での認可は、接続設定の `listen.read` に書く。外向きの平文は、設定で明示したとき（`plaintext`）だけ。書き込みのエンドポイントは持たない（承認は CLI と承認ツールでする）。

### 5.4 承認と CLI

```console
$ sakutei approve <request-id> --as alice --comment "確認済み"
$ sakutei reject  <request-id> --as bob   --comment "窓が違う"
$ sakutei approvals [--all] [--json]                 # 待っている依頼と数え方
$ sakutei start gateway-upgrade [--target gw1]
$ sakutei cancel <run-id> --reason "…"
$ sakutei accept-version <provider>
$ sakutei alloc release <pool> <value> --reason "…"
$ sakutei audit verify
```

どのサブコマンドも `--record` で Record のディレクトリを指す。`--as` を省くと `$USER`。`start` は `--reason`、`freeze` は `--name` も取る（`freeze --mode` に書けるのは `report` だけ）。`alloc release` は `--intent` も要る。`accept-version` は未実装（§8.1）。

承認ツール（別のバイナリ `sakutei-approve`）は、前段の認証の結果から承認者を受け取り、同じ形の 1 ファイルを `approvals/<request-id>/` に書くだけ。コアは認証をしない。

- 承認者が条件（quorum・separation・役割）を満たすかは、cycle の中で数える。申請者・要件に無い人・期限の後の判断は数えない。
- CLI も、申請者と要件に無い人の判断は書く前に reject する。
- 依頼の ID は次のとおり：
  - Policy の変更なら、宣言と操作から決まる `chg-…`
  - Procedure の gate なら `<run-id>-<step>`
  - 送る途中で止まった Procedure を続ける承認なら `<run-id>-resume-<step>`

人の判断を書くサブコマンド（approve・reject・freeze・unfreeze・start・cancel）は、ロックを取らずにファイルを書くだけ。それを効かせて audit に残すのは cycle。daemon は、その置き場への書き込みで次の cycle を始める。

### 5.5 起動フラグ

| フラグ | 内容 |
|---|---|
| `--intent <dir>` | intent のディレクトリ |
| `--config <file>` | 接続設定（config ファイル） |
| `--record <dir>` | Record のディレクトリ |
| `--policy-set <name>` | 使う Policy の組（宣言に無ければ reject）。**未実装**（§8.1） |
| `--once` | 1 cycle だけ回して終わる（`reconcile` のフラグで、既定 true） |
| `--json` | 出力を JSON に |

サブコマンド：`sakutei`（daemon。`sakutei --intent …` で起動）・`plan`・`reconcile`・`status`・`wake`・`freeze`/`unfreeze`・`approve`/`reject`/`approvals`・`start`/`cancel`・`accept-version`・`alloc release`・`audit verify`。このうち `status` と `accept-version` は未実装（§8.1）。

one-shot 実行の終了コード（E-J14）：

| コード | 意味 |
|---|---|
| 0 | 収束した |
| 1 | 実行の誤り |
| 2 | 読み込み時にエラーにした（reject） |
| 3 | ずれか Unknown が残る（report・承認待ちを含む） |
| 4 | halt した（budget・ineffective Apply の上限・急減。provider のバージョンの変化は未実装） |

### 5.6 接続設定（config ファイル）

範囲・除外・方針は書けない（書けば読み込み時にエラーにする）。書くのは、接続先・credential・listen・外部入力の在りか・sink の宛先。`providers.<名前>` に書けるのは `module` と `settings` だけで、観測用と書き込み用の credential（`observe`・`write`）は各 provider の `settings` の中に書く（形は provider ごと。`v3/providers/README.md`）。

```yaml
providers:
  dns:
    module: dnszone
    settings: {zone_dir: /var/lib/named, reload_cmd: ["rndc", "reload"]}
  pve:
    module: proxmox
    settings:
      endpoint: "https://pve.example.com:8006"
      api_base_path: /api2/json
      observe: {token_id: "sakutei-ro@pve!audit", token_file: /etc/sakutei/pve-ro.token}
      write:   {token_id: "sakutei@pve!sakutei", token_file: /etc/sakutei/pve-rw.token}
  fw:
    module: nftables
    settings:
      hosts:                         # ホストごとの接続設定（扱う table は intent の ranges）
        fw1:
          ssh: {address: 192.0.2.1, user: sakutei, known_hosts: /etc/sakutei/known_hosts, key: /etc/sakutei/id_ed25519}
          sudo: true
          guard: [/usr/local/libexec/sakutei-nft-guard]
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

| field | ユースケース |
|---|---|
| `providers.<名前>.module`・`settings` | E-O4・E-S1・N-S2 |
| `settings` の中の `observe`・`write` の credential | E-G5・N-G3 |
| `hosts.<名前>` の `ssh`（known_hosts と鍵）・`local`・`sudo`（SDK の `sdk/hostexec.ParseHosts`） | E-E1 |
| `max_concurrent`（provider ごとの同時実行の上限）。**未実装**（§8.1） | E-N1・N-N4 |
| `secret_backends` | E-G3・E-O2・N-O2 |
| `inputs` の在りか | E-K3・E-P4 |
| `expanders` の在りか（どれを使うかは intent の `expand:`） | E-B5・E-A7 |
| `sinks`（`streams: [events, audit]`。書かなければ events。audit を送れないモジュールに audit を書けば reject） | E-H2・E-H3・E-L1 |
| `listen.read`（`tls.client_ca`・`plaintext` を含む） | E-H5・E-L3・N-L4 |

---

## 6. provider SDK

### 6.1 作者が書くもの

1. `Schema()`：type ごとの field（型・identity・digest・比べ方）・settle・観測だけか・中身を失う type か・バージョン。
2. `Observe(ids)`：4 値と、正規形の field。宣言と観測で同じ正規化の関数を使う。
3. `Apply`・`Delete`：冪等。Delete は拒否の理由を返せる。
4. `List(range)`：ID の集合と「完全か」。
5. 設定の読み方（`settings` の型）。範囲の引数（`Range.Args`）は、設定でなく Range から受け取る。
6. 任意：`Transactor`・`Invoker`・`Classifier`・`Validator`。

SDK とコアが用意するもの：
- `WriteContext.Secret(ctx, value)`：書き込みの文脈でだけ、secret の参照を値にする（書き込み用の credential を返す）
- ssh の実行の層（`sdk/hostexec`）：known_hosts の確認・決まったコマンドだけを sudo で。届かないときは `UnreachableError` を返し、provider は Unknown にする
- `cli.Main(WithModule(...), WithExpander(...), WithSink(...), WithSecretBackend(...), WithCheck(...))`：自前のバイナリに組み込む。ほかに `WithAllocator`・`WithClock` がある
- HTTP のヘルパー（TLS・API のパスの prefix・リトライ）は設計にあるが**未実装**（§8.1）。いまは各 provider が自分で持つ

| SDK の要素 | ユースケース |
|---|---|
| `cli.Main(With…)` で自前のバイナリに組み込む | E-O4・E-S4 |
| ssh の実行の層 | E-E1・N-E1 |
| `SecretRef` と `SecretBackend{Generation, Resolve}` | E-G2・E-G3・N-G2 |
| HTTP のヘルパーと API のパスの prefix（未実装） | E-C1・E-D1 |
| `WithCheck` | E-B9 |

### 6.2 契約テスト（`sdk/contract`）

provider の作者は、自分の provider と fake の相手（またはモック）を渡して回す。`contract.Run` が下の表を、`contract.RunTxn` が Transaction の戻りを確かめる。

| テスト | 確かめること | ユースケース |
|---|---|---|
| 届かないなら Unknown | 接続を切る・5xx・タイムアウト・認証失敗・壊れた出力で、Absent を返さない | E-K1・E-E6・E-P2 |
| 無いなら Absent | 確かに無い対象に Absent を返す | E-P2 |
| 作って読む | Absent → Apply → Observe が Present で、宣言どおり | E-P2 |
| 冪等 | 同じ Apply を 2 回して、2 回目に現実が変わらない | E-P2 |
| 正規形 | 宣言と観測を同じ関数で正規化し、書き直しの無い Apply が差分を作らない | E-I7・E-B7 |
| List の完全さ | ページの途中で切ると `Complete: false` | E-K2・E-P2 |
| Observe と List の一致 | List に出る ID は Observe で Present（「List の完全さ」の中で確かめる） | E-D8 |
| secret が漏れない | エラーメッセージ・Note・Fields に secret の値が出ない（各テストのあとに毎回確かめる） | E-G2・N-L5 |
| 書き込みの credential | `WriteContext` の外で書き込み用の credential を読まない | E-G5 |
| Schema の lint | バージョンがある・identity の field が宣言できる・destructive の宣言に矛盾が無い | E-P2 |
| Transaction の戻り | Prepare して Commit しなければ、タイムアウトのあとに元に戻る（モックで）。Commit すれば戻らず、Rollback で戻る（`contract.RunTxn`） | E-B3・E-B8 |
| Invoke の送信 | 送れなかったときだけ `ErrNotSent` | E-A2 |
| Delete の拒否 | 拒否する条件で `RefuseError` を返し、何も消さない | E-F1・E-C2 |

コアの性質のテスト（§4）は、fake provider（`sdk/fake`）で回す（E-P1・N-P1）。`sdk/fake` は観測の並び・失敗・ineffective Apply を注入できる。設計にある「遅さ」の注入は未実装（§8.1）。統合テスト（qemu-bmc・netopeer2・Proxmox の入れ子・postgres など）は、各 provider のモジュールの側で回す（E-P3・N-P4）。時計はコアの `Clock` で差し替えられる（E-R4）。

---

## 7. v2 から移すもの・捨てるもの

### 7.1 provider（全部コアの外のモジュールへ移植）

新しいモジュールの名前は、接続設定の `providers.<名前>.module` に書く名前（`v3/providers/builtin`）。

| v2 | 新しいモジュール | 移植の要点 | ユースケース |
|---|---|---|---|
| dnszone | `dnszone` | ゾーンの指定は Range の `args.zone` へ。SOA のシリアルを上げるのは Apply の中のまま。`soa_serial: keep` を Resource の field にするのは**未実装**（§8.1。いまはシリアルを必ず上げる） | E-B1・N-B11 |
| nftables（file・live）＋ sakutei-nft-guard | `nftables`（専用ツールはパッケージ `nftguard` とバイナリ `sakutei-nft-guard`） | base chain の守りは `Transactor`（Prepare＝guard の check・arm（コピーとタイムアウトでの戻し）・apply、Commit＝disarm、Rollback＝guard の rollback）。live の base chain の宣言は、`apply.transaction` を書かなければ読み込み時にエラーにする。confirm の先は Resource の `confirm`（`phase`）。base chain の作成・policy の変更を破壊的な変更として数える `Classifier` は**未実装**（§8.1）。`host` と `tables` は Range へ | E-B2・E-B3・E-B4・N-B9 |
| redfish-system | `redfish` の `system` | 電源の切り替え中は Converging。`power: "Off"` は GracefulShutdown だけを送る。強制の停止・再起動は `Invoker` の `force_off`・`force_restart`（どちらも破壊的） | E-A1・N-A1 |
| redfish の boot_once・reset | `redfish` の `Invoker`（`system` の `boot_once`・`reset`） | `ErrNotSent` の判定をそのまま移す | E-A2・N-A2 |
| redfish-virtual-media | `redfish` の `virtual-media` | 変更なし | E-A3 |
| redfish-bios | `redfish` の `bios`（ID `<node>/<属性>`、field `value`） | 保留中は Converging。attribute registry（読み取り専用・選べる値・範囲・型）の検査は、`Validator` でなく Apply の直前にする。読み込みは BMC に触れないので、Apply の前に BMC から registry を読み、合わなければ送らずに Apply を失敗させる | E-A4・N-A3 |
| redfish-firmware（多相 Task） | `redfish` の `firmware`（観測だけ。`version` と、最新の更新の Task の `update_state`・`update_image`・`update_percent`・`update_message`）＋ `Invoker`（`verify`・`stage`（破壊的）・`reset`・`force_restart`（破壊的）） | 相は intent の Procedure に書き直す（§3.7 の例）。`stage` で送り、Task の完了は観測の `update_state` で待つ。すぐ効く部品は `stage` だけでバージョンが変わる。再起動で効く部品（BIOS・`apply_time: OnReset`）は `reset` か `force_restart` で効かせる（`apply` という操作は無い）。危険な区間は `critical`。`TargetDetermined` の判定は provider の中 | E-A5・N-A4 |
| proxmox-vm | `proxmox` の `vm`（ID `<クラスタ>/<VM 名>`） | UPID の待ちは Apply の中（`task_wait`）。待ち切れなければ、観測が VM の走っているジョブ（`/nodes/<node>/tasks?source=active`）か lock を見て Converging にする（provider は UPID を覚えない）。宣言した slot・キーだけを比べる・template は作るときだけ使う、は `FieldSpec.Match`。`volume=` の付け替えは `Classifier`（破壊的・承認が要る。該当する Policy に approval が無ければ、申請者でない 1 人の承認）。keep の残る VM の削除は `RefuseError`（何も書かずに拒否する。持ち主の VM は `power: stopped` で止める）。付け先の無い volume は、List が type `volume`（観測だけ）で出す。強制の停止は `Invoker` の `stop`（破壊的。v2 の `power_off: graceful_then_force` の代わり）と `reboot`・`reset`（破壊的）。cluster の範囲は Range（`args.cluster`）。触らない VM は Range の external_owners | E-D1・E-C2・N-D1・N-D10・N-C12（N-D3 の「置けるノードの集合」はまだ無い。v2 にも無い） |
| proxmox-fw | `proxmox` の `fw-rules`（ID `<クラスタ>/<VM 名>`）・`fw-rule` | comment に `sakutei: ` の印が無い規則は、List が type `fw-rule`（観測だけ、ID `<クラスタ>/<VM 名>/<位置>`）の unmanaged として出す。規則の変更は `Classifier` で破壊的な変更にする。network policy の実現先 `proxmox-fw` は `netpolicy.WithRealizer` で組み込み、作る宣言は同じ id の vm に依存させる | E-B6・E-B5 |
| netconf | `netconf` | two-phase Apply は `Transactor`（commit confirmed。Commit＝確定の commit、Rollback＝cancel-commit かセッションの終わり）。commit_group は Resource の `transaction.group`、confirm は `{phase, when}` で、`when` は Condition（`observe(…)`・`present(…)`・`check(…)`）。機器と部分木（`filter.<部分木>`・順を比べるパス `ordered.<部分木>`）は Range。観測は毎回新しいセッション。NACM で絞ったユーザーで動く（範囲の外を読み書きする rpc を送らない） | E-B7・E-B8・E-B9・E-B10・N-B1・N-B4 |
| cli-device | `cli-device` | 機種の定義ファイルは settings。type は定義の field、ID は装置。扱う装置と field は Range。定義の `actions` は `Invoker`。`Transactor` は、定義にタイムアウトで戻る Commit（`config.commit_confirmed`・`confirm`・`cancel`）がある機種だけ（取り消しの `rollback` は Commit しない変更を捨てるだけで、タイムアウトでの戻しにならない） | E-B11・N-B1 |
| postgresql | `postgresql`（type `role`・`database`・`grant`・`extension`） | 利用者のテーブルがある DB の削除は `RefuseError`。database は中身を失う type（宣言の外でも prune しない）。scope は Range の `args.server`。パスワードは secret の参照で、SCRAM の検証値で送る。観測用と書き込み用のロールを分ける | E-F1・E-F3・N-F1・N-F7・E-G5 |
| sqlmigrate | `sqlmigrate`（type `database` は観測だけ＋`Invoker` の `migrate`・`rollback`） | `strategy: task` は Procedure（`trigger: on_drift`・`invoke: migrate`・`done_when`）。Procedure の `on_failure` の rollback の step が入るまでは、失敗したときの戻しは `migrate` の引数 `rollback_on_fail` | E-F2・N-F2 |
| s3-bucket | `s3` | Object Lock の変更は `Classifier`（承認）。中身のあるバケットの削除は `RefuseError`。接頭辞は Range | E-C1・N-C7 |
| ldap | `ldap`（type `entry`、ID `<server>/<DN の正規形>`） | server・base_dn・filter・scope（LDAP の検索の scope）は Range。子のあるエントリと、宣言したエントリの上のエントリの削除は `RefuseError` | E-G1・N-G1 |
| ansible | `ansible` | `DesiredObserver` の縮退（check mode。Validate が覚えた宣言で流す）。List と prune は持たない。`destructive` は `Classifier`。args の secret の参照は reject する（観測では値を取り出せない。secret は ansible の側に置く） | E-E3・N-E5・N-T3 |
| template-file | `template-file` | `DesiredObserver` の縮退（Validate が覚えた宣言で描いて、ファイルと比べる。無ければ Absent）。`var.<名前>` は Map の field `vars` に | E-E5・N-E8 |
| host-file・systemd-unit（hostconf） | `hostconf`（type `file`・`unit`） | `restart_on` は Resource の field のまま（入れ替えた時の digest の記録はホストの側）。ホストは Range。中身は hash と stat だけ観測する（`FieldSpec.Digest`） | E-D5・E-E2・N-D7・N-E1・N-E6・E-H7・N-H1（監視の設定のファイルもこれで配る） |
| guest-os | `guest-os`（type `os`、ID はホストの名前） | 観測だけ。marks は Resource の field（`markers`：名前 → 絶対パス）。中身は観測の `marker` で、Condition は `observe(guest/os/<host>).marker.<名前>` で読む。コアは宣言を Observe に渡さないので、Validate が宣言の `markers` を覚えて Observe が読む。ホストは settings の `hosts`（接続設定だけ、`sdk/hostexec`） | E-E4・E-A8 |
| metrics-probe | `metrics-probe` | 観測だけ。比べ方は provider の設定でなく Condition に移す（provider は値を返すだけ） | E-H4・N-T4 |
| secret backends（vault-kv2・command・file） | `vault-kv2`・`command`（パッケージ `providers/secretbackend`）・`file`（コアに組み込み） | 世代を返す `Generation` と `Resolve` のまま | E-G3・E-G4 |
| （新規）台帳 | `ledger`。**未実装**（§8.1） | 観測できない資産（紙の台帳・テープ・旧機器）を宣言だけで持つ。観測は台帳のファイルの「最後に確認した日からの経過」だけを返し、List と Apply を持たない（消さない・触らない） | E-I4・N-I6・N-C6・N-B19 |
| （新規）secret の生成 | `secret/generate`。**未実装**（§8.1） | Apply が乱数のパスワードや鍵対を作って secret backend に置く。宣言には参照だけ | N-G9・N-G8・N-C10 |
| （新規）到達の観測 | `probe`。**未実装**（§8.1） | tcp・ssh・http の到達を観測だけで返す。v2 で nftables・netconf の中にあった到達の確認をここに集め、Condition から読む | E-B13・N-B17 |
| fakeprovider | `sdk/fake`（module 名 `fake`） | 故障の注入を足す | E-P1 |

### 7.2 コアの機能

| v2 の機能 | 扱い | 新しい受け皿 |
|---|---|---|
| reconcile ループ（loop.go） | 書き直す | 1 cycle の関数 `cycle.Run` 1 つ（§2） |
| render のフェーズ 1・2 | 書き直す | 入力の読み込み＋Expand |
| managed・computed・PlanRefiner の落とし | 捨てる | 宣言した field だけ比べる |
| ready_condition・require・done_when・apply_check・confirm の observe/check・capacity gate | まとめる | Condition |
| remediation mode・schedule・freeze・planned_change・max-* のフラグ・approve-large-change・profile・runtime | まとめる | Policy・一時的な Policy・`--policy-set` |
| 単純 Task・多相 Task・Workflow・manual・disruption | まとめる | Procedure |
| netconf の two-phase Apply・nftables の守り・commit_group・confirmafter | まとめる | Transactor と Resource の `transaction` |
| `providers:`・`external_owners:`・deletion_policy・IntentRanges・ConfigExcluder | まとめる | Range |
| task-store・workflow-store・alloc-store・apply-check-state・freeze-state・daemon-status・cause-state・audit-log | まとめる | Record |
| fencing-store・claim・gitdist・S2-free・DR モード・observe tier・BulkObserver | 捨てる | §1.3・§1.4 |
| 原因タグ A〜E・last-seen | 縮める | 3 分類とバージョンでの停止（どちらも未実装、§8.1） |
| allocator（authority・Lookup キャッシュ・cooldown・write_back の 3 方式） | 縮める | intent のファイル＋`Allocator.Pick` |
| network_policy（pkg/netpolicy） | 外へ | Expand モジュール `netpolicy` |
| compatibility | 外へ | Expand モジュール `compat` |
| eventnotify・audit の syslog・otlpexport・metrics | 外へ | sink モジュール |
| readapi | 縮める | read-only HTTP API（書き込み無し） |
| 承認の HTTP（`--approval-listen`） | 外へ | `sakutei-approve`（E-M4・N-M4） |
| secrets extract・break glass の記録 | 外へ | ツールと運用。break glass の使用は、ツールが audit に 1 行足す（E-G6・E-G7） |
| importer・syncmode | 外へ | SDK の上のツール `sakutei-import`（現実→intent の片方向。Apply の経路につながない）（E-I1・E-I2・N-I5・E-Q2・N-Q3・N-E3・N-I1。他のツールからの変換 N-I7 も同じ形） |
| 起動フラグ 128 | 捨てる | 6 つ＋サブコマンド |
| v2 の intent | 変換する | 変換ツール `sakutei-migrate`（v2 の intent と provider の設定を読み、新しい intent と接続設定を書く。範囲と除外は設定から intent へ動かす。受け皿の無いものは reject する）（N-I7） |

---

## 8. 作る順番

各 Stage で「動くもの」と「確かめられること」をはっきりさせる。v2 は修正だけにして、新しいコアが v2 の intent の変換結果を全部扱えるまで、並べて保つ。

Stage 1〜9 はすべて main に入っている。設計のうち、どの Stage にも入らず残っているものは §8.1 に挙げる。

| Stage | 作るもの | 確かめられること |
|---|---|---|
| 1 | 型（Resource・Range・Schema・Observation）・intent と接続設定の読み込みと検証・Expand（素通し）・fake provider・`plan` | 読み込み時にエラーにする規則（不変条件 24・25）。plan が観測と比べるだけで何も書かない（30 の半分） |
| 2 | 1 cycle の処理（Apply・Delete・List・prune の守り）・Policy の mode と budget・Record（ロック・marks・audit のハッシュチェーン）・`reconcile --once`・終了コード | Unknown で消さない・List の欠損と急減で消さない・budget で halt する（3・6・7・9・20）。性質のテストの土台ができる |
| 3 | daemon（繰り返し・wake・read-only HTTP API）・Events（level Event と outbox）・stdout の sink | one-shot 実行の列と daemon が同じ結果（30）。確認できない cycle に resolved にしない（29）。sink の失敗で止まらない（28） |
| 4 | Condition（三値・quantifier）・Graph（wave・待ち・同じ cycle の再観測）・settle と ineffective Apply の回数 | 空から組む構成が 1 回で収束する（E-J7）。ineffective Apply で halt する（14）。容量の門（E-C4） |
| 5 | Procedure（apply・invoke・wait・manual・gate・policy・critical）・承認の CLI・一時的な Policy・`approve` と `freeze` | 送信の途中で止めても二度送らない（16）。done_when で完了にする（18）。申請者≠承認者（21）。freeze の期限（22） |
| 6 | 最初の実 provider：`dnszone`・`hostconf`・`nftables`（Transactor を含む）と専用ツール `sakutei-nft-guard` | 実のファイルとカーネルで、two-phase Apply が戻る（17）。v2 の試用の手順（DNS と FW）が通る |
| 7 | Expand のモジュール（template・netpolicy・compat）・採番・secret の世代と secret backend | 採番を巻き戻しても二度払い出さない（11）。secret backend が落ちても Unknown に寄る（E-G4）。golden のテスト（N-P5） |
| 8 | 残りの provider（proxmox・redfish・netconf・cli-device・postgresql・sqlmigrate・s3・ldap・ansible・template-file・guest-os・metrics-probe）・sink（alertmanager・syslog・metrics・otlp）・`sakutei-approve`。大きいので 8a（sink と `sakutei-approve`）と 8b〜8f（provider）に分ける | 各 provider の契約テストと、v2 と同じ統合テスト（qemu-bmc・netopeer2・Proxmox の入れ子）。8a：各 sink が fake の受け手に送れ、受け手が落ちても cycle を止めない（28）。`sakutei-approve` が申請者≠承認者を強制する（21） |
| 9 | `sakutei-migrate`・import と同期のツール `sakutei-import` | v2 の試用の intent を全部変換して、新しいコアの plan が v2 の plan と同じ操作を出す |

最初のリリース（Stage 2 まで）で、v2 の安全の芯（消さない・数えて halt する・one-shot 実行で収束する）を fake provider の上で確かめられる。Stage 6 で初めて実物に触る。

### 8.1 まだ実装していないもの

本文に書いた設計のうち、2026-11 の main（Stage 1〜9 まで）に無いもの。どれもまだどの Stage にも割り当てていない。

| 設計 | 本文 | いまの実装 |
|---|---|---|
| provider のバージョンでの停止・`sakutei accept-version`・marks の `provider_versions` | §3.3・§3.6・§5.4・不変条件 23 | バージョンを記録も比較もしない |
| 原因の 3 分類（宣言・外部入力・ずれ）・Expand の入力と出力のハッシュ・`--expect-revision`・audit の `cause`・`intent_revision` | §2.1 Phase 4・§3.8・§5.1 | 原因を付けない |
| `status` サブコマンド | §2.1・§5.5 | daemon の `/status` だけ |
| `policy_sets` と `--policy-set` | §3.6・§5.5 | intent の最上位の field として reject する |
| Policy の `observe_every` | §3.6 | 毎 cycle 全部を観測する |
| `FieldSpec.Destructive`（field ごとの破壊的な変更の印） | §3.3 | 削除は `DestructiveDelete`、状況で決まる変更は `Classifier`、一回きりの操作は `OpSpec.Destructive` で言う |
| nftables の `Classifier`（base chain の作成・policy の変更を破壊的に数える） | §7.1 | nftables は `Classifier` を持たない |
| Procedure の `trigger: {policy: …}`・`trigger: before_delete`・step `exec`・`on_failure` の rollback・`disruption` と Event `Disruption` | §3.7・§5.2 | `trigger` は `on_drift`・`manual`、`on_failure` は `halt` だけ。ほかは reject する |
| Expand の `Now` | §3.8 | `Input` に時刻を渡さない |
| 接続設定の `max_concurrent` | §5.6 | 未知の field として reject する |
| SDK の HTTP のヘルパー | §6.1 | 各 provider が自分で持つ |
| `sdk/fake` の遅さの注入 | §6.2 | 観測の並び・失敗・ineffective Apply だけ注入できる |
| dnszone の `soa_serial: keep` | §7.1 | シリアルを必ず上げる |
| 新規のモジュール `ledger`・`secret/generate`・`probe` | §7.1 | 無い。到達の確認は `check(<名前>)`（`cli.WithCheck`）か、ほかの provider の観測で書く |
