> Sakutei 設計書 v3.0 の一部（設計の正）。全体目次と §→ファイル対応は [README.md](README.md) を参照。§番号は全 8 部を通して一意。

# 第II部 中核モデル

本部は Sakutei の中核データモデルを定義する。§4 で全プロバイダに波及するコア型と状態分類を確定し、§5 でプロバイダが実装する抽象を、§6 で3つのプリミティブを、§7 で所有と境界の宣言を定義する。ここで固定した型と契約が、第III部以降（収束機構・安全機構・スケール）の全機構が読む共通の語彙になる。

---

## §4 正準型と状態分類

コア型は「後から変えると全プロバイダに波及する」ため、構造（何を持つか）をここで正準形として固定する。細部の命名・タグ体系は実装中に調整可能だが、構造は変えない。

### §4.1 ID —— （provider, scope, id） 複合

リソースの同一性は3要素の複合で表す。

```go
// ID — リソースの正準識別子。global-unique な単一文字列ではなく複合キー
type ID struct {
    Provider string  // どの provider が扱うか
    Scope    Scope   // ルーティングキー(§4.2)。claim シャーディングの境界(§17)
    Local    string  // scope 内でのローカル ID(provider が発行・観測する id 文字列)
}
```

**確定理由:`scope` がルーティングキーである。** claim シャーディング（§17）は「どの scope を自分が収束するか」で担当を分ける。ID に scope を含めないと、再シャード時にリソースの帰属が壊れる。global-unique な単一文字列 ID は「再シャードで壊れる」ため棄却する。`Local` は provider が観測・発行する id 文字列であって、その解釈は provider に閉じる（コアは不透明に扱う）。

### §4.2 Scope —— ラベル集合 + パス形式

`Scope` は provider 非依存の管理境界を表す。zone / cluster / rack を1型で表し、provider 追加のたびにコア型を変えずに済ませる。

```go
// Scope — provider 非依存のラベル集合 + パス形式
type Scope struct {
    Labels map[string]string  // 例: {site: tokyo, cluster: prod}。provider ごとの意味を1型で吸収
}

func (s Scope) Path() string  // labels → intent/<scope>/... のパス。rendered_revision 算出単位でもある(§9)
```

`Scope` は4つの用途で同一語彙を共有する。この一致が設計の要である。

- **claim の scope-selector**（§17）:`--claim {site: site-a}` が担当境界を切る。
- **prune 境界**（§12）:closed-world の集合演算は scope 内に閉じる。
- **`intent/<scope>/` パス規約**（§17）:claim ↔ パスが機械的に対応し、sparse checkout が「自 claim 分 + 共有 platform 分」だけを引ける。
- **rendered_revision 算出単位**（§9）:`Path()` が revision 計算の粒度になる。

型付き（zone 型・cluster 型と分ける）にすると provider 追加のたびにコア変更になるため、**ラベル集合という1型**に統一する。

### §4.3 State —— Schema 付き map

観測された現実の状態を表す。型情報は `Schema`（§5）が持つので `State` 自体は map で足りるが、各値は Schema の宣言型に従う。

```go
// State — Schema 付き map(key=Schema のフィールド名、value=Schema 宣言型に沿った値)
type State map[string]TypedValue
type TypedValue struct {
    Kind  FieldKind  // Schema の type と対応
    Value any
}
```

`State` は現実側の状態のみを表し、desired（あるべき姿）は持たない。この分離が dry-run の正しさの根拠になる（§4.6, §8）。

### §4.4 Plan —— 操作列 + 破壊性 + 原因タグ + 対象 + 診断

`Plan` は「何をすべきか」を表す。Apply の契約（§5.4）・rate limit の重み付け（§13）・破壊予算（§13）・per-resource 原因タグ（§10）が全部これを読むため、必要な情報をすべてここに載せる。

```go
// Plan — 操作列 + 破壊性 + 原因タグ + 対象 ID + 人間可読の差分診断
type Plan struct {
    Ops []Op  // Create / Update / Delete の操作列
}

type Op struct {
    Kind        OpKind      // Create | Update | Delete
    Target      ID          // 対象(§4.1 の (provider, scope, id) 複合)
    Destructive bool        // ★破壊性フラグ。rate limit・破壊予算・halt-all 凍結がこれで駆動(§13)
    Cause       CauseTag    // ★原因タグ A/B/C/D/E(per-resource 帰属・§10)
    Diagnostic  string      // 差分の人間可読診断(sakutei status / plan 表示用)
    Fields      []FieldDiff // 変わるフィールド(Update 時)。表示粒度はここで展開(§12)
}
```

- **`Destructive`** が安全機構の駆動軸である。rate limit・破壊予算・全体凍結がこのフラグで動く。不明時（`TrueObserve:false`）は破壊的側に倒す（§5。Ansible provider は利用者の `destructive` の宣言に委ねる、§29.1）。
- **`Cause`** は差分1件ごとに原因タグを帰属させる（per-resource）。原因の分類定義は §10 を正とする。

### §4.5 Observation —— Availability + State + Ready

`Read`（§5）の戻り値。`drifted` / `converged`（desired との比較結果）は Observe では判定できず、これらは **Diff の出力**である。混ぜると Observe が desired を知る必要が生じ、dry-run の根拠（Observe/Diff 分離）が崩れる。

```go
// Observation — 現実を見た結果。desired を知らない
type Observation struct {
    Availability Availability     // ok | unknown(到達不能) | converging(相手が収束作業中)
    State        State            // Availability == ok のときのみ有効
    Ready        map[string]bool  // 名前付き ready condition の充足状況(§8.3, §11.2)
    ObservedAt   time.Time        // この観測を取った時刻。bulk/個別いずれも刻む(§5)。周期越え再利用の検知に使う
}

type Availability int  // Ok | Unknown | Converging
```

**三値の意味を絶対に混同しない**（非対称原則の起点・§3）。

- **`ok`** … 存在する。`State` が有効。
- **`unknown`** … 到達不能。**存在しないことと unknown を絶対に混同しない**（`unknown ≠ absent`）。
- **`converging`** … 相手が収束作業中（相手も収束ループを持つ）。drift と誤認して再 Apply すると綱引きになる（§11）。

`error` は到達失敗そのもの（接続不能等）を表し、到達したが「相手が状態を返せない」場合は `Availability` で表現する。収束ステータス（報告用）はコントローラが**導出**する:`unknown`/`converging` は Observation から、`converged`/`drifted` は Diff 結果（Plan が空か否か）から、`oscillating` は履歴から（§11.3）。

**`ObservedAt` を各 Observation に刻む**（バルクでも個別でも）。バルク結果は当該周期でのみ使用し、**周期をまたいだ再利用は禁止**する——古い観測を「現在状態」として次周期に持ち越すことは、排除したはずの観測 state の再導入である（§8）。`ObservedAt` はこの周期越え再利用を検知・拒否する目印である。

### §4.6 ChangeHint —— revision + scopes のみ

早期収束（トリガ収束・§8）の入口が流すヒント。**トリガは「いつ収束するか」のヒントであり、「何が真実か」を一切運ばない。**

```go
// ChangeHint — 早期収束のトリガ。ペイロード(変更後の値)は運ばない
type ChangeHint struct {
    IntentRevision string  // intent_revision(触られたことの目印)
    Scopes         []Scope // 触られた scope 群(この scope を今見に行け、としか言わない)
}
```

ヒントに含まれる情報を「現在状態」として信じることは一切しない。トリガを取りこぼしても次の定期周期が必ず拾う（遅延のみ・正しさは無傷）。詳細は §8 を正とする。

### §4.7 Event —— 単一型 + kind

イベントは単一型に kind enum を持たせる。発生源と sink を分離するための共通型で、severity・level か edge か・Since・原因タグをそのまま入れる。

```go
// Event — 単一型 + kind enum + scope/resource 参照 + revision 参照
type Event struct {
    Kind        EventKind  // enum(DriftDetected / ProviderUpgradeDiff / StageAwaitingApproval / BreakGlassUsed …)
    Severity    Severity   // info | warning | critical
    LevelOrEdge LevelKind  // level(継続状態・firing/resolved)か edge(事件・一回)か
    Scope       Scope      // どの scope で起きたか
    Resource    *ID        // 個別リソース参照(あれば)
    Revision    string     // 関連する rendered_revision / intent_revision
    Since       time.Time  // level イベントの継続開始(transient↔stuck 自動昇格)
    Cause       *CauseTag  // 原因タグ(監査帰属)
}
```

### §4.8 SecretRef —— 自動マスクされる専用型

秘密は文字列にしない。`SecretRef` 専用型を用い、`String()` が `***` を返すことで**自動マスク**し、実値取得は明示 `.Resolve(ctx)` 経由のみとする。「うっかり平文化」を型で防ぐ。

```go
// SecretRef — 秘密の参照。値は保持せず、Apply 直前に JIT 解決する
type SecretRef struct {
    Path string  // 例: vault:/db/prod#password
    Gen  string  // 世代(render フェーズ1 が Vault metadata から注入。ピン留めではない)
}

func (r SecretRef) String() string          // 常に "***" を返す(ログ・Event・ダンプで自動マスク)
func (r SecretRef) Resolve(ctx) (string, error)  // Apply 直前・provider に渡す瞬間だけ実値化
```

diff は**参照同士で取る**（値を知らなくても diff できる）。git への実値書き戻しループは生じない。JIT 解決の詳細・`CredentialProvider` との境界（注入する設定値か、engine 自身の到達資格か）は §20 を正とする。

### §4.9 状態分類 S1〜S4

「state レス」は正確には**観測 state レス**である。他の state は限定的に持つ。判定基準は**「喪失が事故になるのは S2 だけ → そこだけ永続化」**。

| クラス | 中身 | 置き場 | 喪失時 |
|---|---|---|---|
| **S1 観測 state** | リソースの current キャッシュ | **持たない** | —（そもそも保持しない） |
| **S2 進行 state** | Task/Workflow フェーズ・承認待ち・シャードルーターの割当表 | **外部永続化必須** | 事故（割当表喪失=誤配送） |
| **S3 履歴 state** | 振動検知・ベースライン・backoff・Since・in-flight 集合・allocation Lookup キャッシュ | インメモリのみ | 検知が数周鈍るだけ（安全側）。Lookup キャッシュ喪失時は allocator に問い直すだけ |
| **S4 監査 state** | append-only ログ | 別ストア（収束経路から不可視） | 収束に影響なし |

これにより全クラスの再起動セマンティクスが定義済みになり、「再起動安全・DB 不要」という運用上の推論が正しくなる。永続化が必要なのは S2 だけであり、その永続化先（quorum ストア + fencing）は §17 を正とする。

**S2-free シャード階級。** エッジ / 店舗の claim は **Resource 収束のみ**を担い、**ローカル S2 を持たない**階級を第一級で定義する。「何も覚えないから、何も失わない」——デーモンホストが全損しても失うものがゼロで、再イメージ → pull → 収束で完全復旧する。Task/Workflow が要する S2 は中央 claim 側に置くか、完了マーカーを現実側に置いて Observe で問う（§6.2 単純型と同型）。詳細な運用（git 配布規約・claim ハンドオフ）は §17 を正とする。

---

## §5 Provider 抽象

**プロバイダの書きやすさが Sakutei の普及を直接決める**。設計の肝は、プロバイダ作者が書く最小を極限まで削り、面倒はコア側の共通ライブラリ（SDK）が引き受けることにある。作者が書くのは約4関数（+ prune 用 `List`）、SDK が diff・秘匿マスク・managed 除外・version 埋込・エラー分類・テストハーネスを担う。

### §5.1 Provider interface 正準形

削りに削ると、作者が書くべきは **4つの関数**。prune を許すなら `List` を1つ足す。

```go
type Provider interface {
    // メタデータ
    Schema() Schema              // フィールド定義(型・必須・secret・managed・ordered・identity・computed)
    Capabilities() Capabilities  // TrueObserve / List / Converging / CommitConfirm / BulkObserve

    // 現実を読む(desired を知らない)。戻りは Observation(Availability / State / Ready)(§4.5)
    Read(ctx, id string) (Observation, error)

    // 現実を変える(create / update / delete を1関数に統合)
    Apply(ctx, plan Plan) error
}

// prune を許すなら追加(任意 capability)
type Lister interface {
    List(ctx, scope Scope) ([]ID, error)
}
```

**Diff は Provider が実装しない。** 差分計算はコアが `Schema` 駆動で汎用実装する。作者は「読む（Read）」と「あるべき姿にする（Apply）」だけ書けばよい。設計上の3つの負担軽減:

- **（1） Diff を書かせない**（最大の軽減）。フィールド比較は車輪の再発明かつバグの温床。順序付きコレクションのような特殊 diff も **Schema で `ordered` と宣言すればコアが適用する**——作者は宣言するだけ、ロジックはコア。
- **（2） create/update/delete を Apply 1本に統合**。`Plan` に「何をすべきか」が入っているので Apply 1本で受ける（中の分岐は自由）。
- **（3） Schema がすべての鍵**。フィールドの型・必須・secret・managed・ordered・identity・computed を宣言すれば、コアが diff・検証・マスキング・managed 除外を全自動化できる。

diff は二段である。**集合 diff**（desired 集合 vs List 結果 → create/delete 判定・コントローラの汎用ロジック）と、**フィールド diff**（両方に在るものの update 判定・Schema 駆動でコアが汎用実装）。

### §5.2 Schema 属性

`Schema` はフィールドごとに意味を宣言する。**Schema を正しく書くことが実装の重さの本体**であり、間違えると設計書が積み上げた安全性が無効化される。

| 属性 | 意味 | 誤ると |
|---|---|---|
| **type** | フィールドの型（FieldKind） | diff / 検証が壊れる |
| **required** | 必須か | 不完全な desired を検知できない |
| **secret** | 秘密値 | 資格情報が平文でログに出る（§4.8, §20） |
| **managed** | 外部システムが動かす欄 | 外部と永久に喧嘩（§7） |
| **ordered** | 順序が意味を持つコレクション | FW ルールの順序が壊れる（§12） |
| **identity** | 削除・参照のキー | 別リソースを消す |
| **computed** | 観測のみ（intent に書かない導出値） | 書けない値を書こうとして事故る |

**identity は「運用上の変更頻度」で選ぶ。** 技術的な一意性だけで決めてはならない。変更されやすい属性（シリアル番号・MAC アドレス）を identity にすると、その属性が変わるたびに Resource の再生成と理想状態の commit が必要になり、運用コストが跳ね上がる。物理サーバの identity をシリアル番号にすると故障交換のたびに intent の書き換えが要るが、**物理トポロジ座標**（「leaf03 スイッチの 12 番ポート」= `leaf03-p12`）を identity にすれば筐体交換時に理想状態の変更が一切不要になる。シリアルや型番は `managed: true` + `computed` として観測のみ行い、identity にはしない。SDK の **Schema lint** が「secret 宣言し忘れ」「identity 未指定」等を静的に検出する。実装は「まず Schema、次に Read/Apply」の順で書く。

### §5.3 Capabilities —— 機械的縮退

provider の能力差を散文の注意書きにせず、機械可読なメタデータにする。

```go
type Capabilities struct {
    TrueObserve   bool  // current を "状態" として返せる(Ansible check mode = false)
    List          bool  // → prune 可否(§5.4 の Lister と連動)
    Converging    bool  // 相手の作業中を検知できる
    CommitConfirm bool  // candidate/commit で自動ロールバック可(NW 機器)
    BulkObserve   bool  // scope 内を1コールでまとめて Read できる(§5.7)
}
```

**縮退規則（コントローラに固定）**:

- `TrueObserve:false` → actual 比較ができないので原因タグ A/B を区別不能。単一タグ `changed` に統合し、rate limit の重み付けは**破壊的側に倒す**（不明は危険扱い=非対称原則）。ただし Ansible provider では、どの適用が破壊的かを利用者が `destructive` で宣言し、宣言の無いものは破壊的に数えない（すべてを破壊的に数えると行の追加まで予算で止まって使えないため。原因タグは新しいタグを足さず、intent の版の変化だけで A、ほかは B とする。§29.1、Backlog #127）。
- `List:false` → その型への prune 宣言はロード時に拒否する。
- Ansible でも `*_info` 系モジュールを Observe に使える対象は `TrueObserve:true` に**部分昇格**でき、縮退は全面でなく部分回復可能。

### §5.4 Lister と各関数の契約

**prune には Lister が要る。** 削除は `現実にある集合 − 理想にある集合` という**集合演算**であり、個別の `Read(id)` では「理想に書かれていないリソース」を発見できない。`DeletionPolicy: PruneWithinScope` は Lister を実装する provider にのみ許可し、Lister 無しの provider に prune を宣言したら理想状態ロード時に拒否する。

各関数が満たすべき契約に設計書の原則が集約される。

**Read の契約**:desired を絶対に見ない（§4.5）。三値（存在 = `ok` + State / 到達不能 = `unknown` / 相手が収束作業中 = `converging`）を正しく返す。`TrueObserve:false` なら State を返せず「変わったか」しか分からないので Capabilities で正直に宣言する。

**Apply の契約**:**冪等**（同じ plan を2回実行しても同じ結果）。毎周回る Sakutei では絶対条件。エラーを **transient / permanent に分類**して返す（backoff のため・§8.5）——権限エラー = permanent、API タイムアウト = transient。順序付きコレクションは**全置換**。commit-confirm 対応機器は candidate/commit を使う。部分適用は中途半端な状態を残さないか、次の Read で正しく観測できる状態にする。

**List の契約（prune するなら）**:scope 内の現実の全 ID を漏れなく返す。不完全だと prune が「List に無い = 消す」で取りこぼしを削除する——**List の網羅性が prune の安全性を担保する**。ページング完全性を保証し（next token が nil で終わったことを確認）、途中打ち切りを検知したら成功として返さず error にする（**complete-or-error**）。網羅を保証できない今回の List は `ListStatus`（complete / partial / unknown）で申告でき、コアは `complete` 以外の周は prune を保守側に倒す（`Read` の三値と対称）。中途半端な List を返すくらいなら実装しない方が安全。

### §5.5 エラー分類

Apply のエラーは2種類に分類して返す（§8.5 の backoff が読む）。

- **transient**（API 一時エラー・ロック競合）→ 指数 backoff（上限 15 分程度）で再試行。
- **permanent**（権限・バリデーション）→ 即 **stuck** 化し、**desired か `provider_version` が変わるまで再試行しない**。

SDK が Apply のエラーを transient / permanent に型付けするヘルパを提供する。

### §5.6 プロバイダの階級

全プロバイダに全機能を要求すると書けるものが激減する。**Capabilities で段階を認める**のが核心であり、作者は自分の階級を選べる。

| 階級 | 実装 | できること | 例 |
|---|---|---|---|
| **完全** | Read（TrueObserve）+ Apply + List + CommitConfirm | 全機能（prune・原因タグ A/B・converging・自動ロールバック） | 自前 DNS / VM |
| **標準** | Read（TrueObserve）+ Apply | prune 以外（List 無し = open-world のみ） | 多くの REST API |
| **最小** | Apply + changed 真偽（TrueObserve なし） | 加算的収束のみ。diff 粗い・原因タグ縮退 | Ansible ラップ |

Capabilities で正直に宣言すれば、コアが自動的に縮退して**安全に**扱う（prune させない・破壊的側に倒す）。「全機能を書けないと参加できない」ではなく「書ける範囲で参加でき、コアが安全に縮退する」——これがエコシステムを広げる鍵。

### §5.7 BulkObserve —— バルク観測と一貫性契約

多くの権威は **scope 内を1コールでまとめて返すバルク API** を持つ（vCenter PropertyCollector / Proxmox `/cluster/resources` / AWS Describe* / NetBox 一覧 API）。これを任意 capability として第一級化し、単一デーモンの天井（§16）を押し上げる。

```go
// prune 用の List(§5.4)と対をなす、Observe のバルク版(任意 capability)
type BulkObserver interface {
    BulkObserve(ctx, scope Scope) ([]Observation, error)  // scope 内をまとめて Read
}
```

`Capabilities.BulkObserve` で宣言する。宣言した provider には Executor がバルク経路を使い、宣言していない provider には **SDK が per-resource `Read` へ透過的に fallback** する（コントローラは差を意識しない）。`Read` の契約はそのまま適用される——バルクで取っても desired を見ず、**毎周・全量を再観測する原則は不変**である（観測 state レスに一切影響しない）。

**BulkObserve の一貫性契約（型で誤用を塞ぐ）:**

- **非アトミックスナップショット宣言**:結果は「走査窓内のある時点の各リソース」であって、全体の一貫スナップショットではない（DRS 稼働中は走査中に VM が移動する）。level-triggered が次周期で自己修正するため、これは正しさの問題ではなく**揺らぎの問題**である。
- **complete-or-error**:ページングは完走するかエラーを返す（`List` と同じ規律）。部分結果を「成功した BulkObserve」として黙示的に返すことを禁止する。
- **型分離（prune の根拠には使えない）**:BulkObserve は **Observe 専用**であり、**prune の根拠には一切使えない**。prune は `Lister.List` + List 完全性検証（§12）を根拠にする。SDK は BulkObserve の戻りが集合差 prune の入力に渡る経路を型レベルで塞ぐ（§5.4 で `BulkObserve` と `List` の型を別に保ったのはこの誤用防止のためでもある——`List` は ID 集合、`BulkObserve` は状態）。
- **走査窓内遷移の吸収**:走査中の DRS 移動が生む一時的不整合は既存機構が吸収する——runtime placement は managed fields 除外（§7）が diff から落とし、それ以外は converging / settle（§11.1）が自傷猶予として吸収する。
- **`observed_at` を各 Observation に刻む**:bulk 結果は当該周期でのみ使用し、周期をまたいだ再利用は禁止する（§4.5）。
- **SDK 等価性テストを標準装備**:fake provider 上で **`BulkObserve(scope) ≡ {Read(id) ∀ id ∈ scope}`** の等価性テストを SDK のテストハーネスに標準装備する。これを通ることが「完全階級 + bulk」を名乗る条件であり、バルク経路と個別経路が別の現実を返す実装バグを CI で捕まえる。v1b で `BulkEquivalenceTester` として実装済みである（検査項目は §28.3）。

**v1b の実装（Backlog #27）:** v1 の実証は dnszone と nftables で行う（どちらもファイルを 1 回読めば全件が分かる素直なバルク。実際の一括 API を持つ対象〈Proxmox・vCenter 等〉は Backlog #49）。収束の経路（常駐ループの定期 Observe と `sakutei reconcile`）は SDK の `CycleObserver` を周期ごとに作って観測する。宣言した provider は、その周期の最初の Observe で `BulkObserve(claim)` を 1 回だけ呼び、残りはその結果から答える。SDK が強制する規律は次のとおり。

- **宣言と実装の両方があるときだけ**バルク経路を使う。宣言しない実装・実装のない宣言は per-resource `Read` に回す（後者は `ValidateProvider` が登録時に拒否する）。
- **complete-or-error の受け方**:バルクがエラーなら、その周期の全リソースを「`Read` が `unknown` を返したとき」と同じに扱う（`unknown` + エラー → unobservable、Apply しない）。このとき `Read` へは回さない（provider が「いま現実を読めない」と言っている）。`ObservedAt` の無いエントリが 1 つでもあれば、結果全体を契約違反として同じ扱いにする。
- **バルクに無いリソースは `Read` で観測する**:バルクの結果は「何が無いか」を言わない（`unknown ≠ absent`）。結果に無い ID は個別に `Read` し、「無い」と言えるのは `Read` だけにする。ファイルが存在しないときのバルクは空の結果（エラーではない）で、各リソースは `Read` で absent になる。
- **キーの照合**:desired の ID で引き、無ければ同じ provider・local id の root scope の ID で引く（scope の概念を持たない現実〈ファイル〉の provider は、`List` と同じく root scope でキーを作る）。
- 結果はその周期だけで使い、周期をまたいで持たない（`CycleObserver` は周期ごとに作り直す）。早期収束パス（§8）は影響サブ DAG の数件だけを見るので per-resource `Read` のままとする。

### §5.8 観測不能階級 —— observability:none / ledger_only

ブラウンフィールドには **Observe すら不可能な資産**（API を持たない旧機器、ドキュメントにしか存在しない配線、ベンダーロックで読めない箱）が必ずある。台帳上 **`unknown` のまま第一級で扱う資産階級**を型として定義する。

```yaml
resource: legacy_appliance
id: old-fw-01
observability: none          # Observe 手段が無い(Read も List も張れない)
ledger_only: true            # 台帳(intent)には存在するが、収束の対象外
last_verified: 2026-03-01     # 人が最後に目視確認した日(時間ベース health 述語と同型)
```

- **`observability: none` の資産は Read/Diff/Apply の対象にしない**（Observe できないものを収束しようとすると必ず事故る）。だが**台帳には第一級で存在させる**——「存在は知っているが観測できない」を `unknown` として明示的に持つのが `unknown ≠ absent` の台帳版である。
- **prune の巻き添えを防ぐ**:closed-world scope の List に現れない `observability: none` 資産を「理想に無い = 削除」と判定してはならない。ledger_only の資産は **List 集合演算から除外**する（現実を List できないのだから List 差分で消す根拠が無い）。
- **時間ベースの人手検証**:`last_verified` が古くなったら「人が目視確認せよ」を health 述語として上げる。観測できない資産の唯一の「鮮度」は人の確認である。

### §5.9 Executor seam —— 実行境界

`Provider` / `Lister` を束ねる実行境界。**コントローラはこの型にしか触らない**（DI で渡す）。

```go
type Executor interface {
    Observe(ctx, specs []Spec) ([]Observation, error)  // 内部で各 provider の Read を束ねて呼ぶ
    List(ctx, scope Scope) ([]ID, error)               // prune 対象 provider のみ有効
    Apply(ctx, plan Plan) (Result, error)
}

type LocalExecutor struct { /* providers */ }  // v1 唯一の実装:同一プロセス直接呼び
```

**守る規律は1つ**:コントローラのコードが `LocalExecutor` の存在を知らないこと。これさえ守れば、将来 `RemoteExecutor{conn *grpc.ClientConn}` を書いて差し替え口に挿すだけで分散が生える。プロセス境界は「隔離（障害・権限・言語）」で正当化されるのであってスケールのためではない（Observe は I/O バウンドで goroutine で足りる）。Ansible / Remote Executor は §18 / §28 を正とする。

---

## §6 3プリミティブ

収束（Resource）だけでは**手続き的作業**（migration / firmware 更新 / ローリングアップグレード / フェイルオーバー / ベアメタル導入）を表現できない。経路依存・非冪等・中断が致命的で、毎周回る収束ループに載せると破壊する。手続きの重さに応じて3層に分ける。

| プリミティブ | 扱うもの | 例 | state |
|---|---|---|---|
| **Resource** | 宣言的状態・収束 | DNS record / VM / volume / 設定 | 観測 state なし |
| **Task（単純型）** | 単一リソースの一回性手続き | migration | 進行フェーズのみ永続化（S2） |
| **Task（多相型）** | 状態機械・リブート跨ぎ | firmware 更新 | 同上 |
| **Workflow** | 順序 + 不変条件 + 手動承認ゲート | ローリング / DR | 進行位置 + invariant（S2） |

**共通原則:「いつ・どの順で変えるか」= 収束が判定。「どう変えるか」= Task/Workflow が実行。** 収束ループはトリガ・メンテ窓ゲート・依存順序・drift 検知・完了確認を担い、危険な手順そのものは Task/Workflow に閉じ込める。実行中の対象は `converging` 扱いにして収束の介入を止める。

### §6.1 Resource —— 宣言的・収束

宣言的状態を毎周期 Observe → Diff → Apply で収束する。観測 state を持たない（§4.9 S1）。全プリミティブの土台であり、Task / Workflow も内部では Resource の観測結果をゲートに使う。

### §6.2 Task —— 単純型 / 多相型 / 進捗

**単純型**（migration）:一度実行して completed 印を残す。完了印は**現実側**（DB 内 `schema_migrations`）に置けば Observe で問える=観測 state レスと両立する。

**多相型**（firmware 更新）:中断不可・長時間・リブート跨ぎ。実質ステートマシンである。

```
Idle → Staging(転送) → Verifying(hash/署名) → Applying(書込)
     → Rebooting → Confirming(バージョン再確認) → Done
                                            ↘ Failed → Rollback
```

- **進行中フェーズの外部永続化が必須**（S2）。ただし永続化するのは進行フェーズだけで、Done 判定は稼働バージョンを現実 Observe で問える。
- **リブート跨ぎ**:Applying 後に機器が消える → `unknown ≠ absent` で「リブート中」と解釈し、タイムアウト付きで再出現を待つ。Confirming で新バージョンを確認して初めて Done。
- **べき等な再開**:各相の入口で「もう済んでいるか」を現実 Observe で確認し、二重実行を防ぐ。
- **危険区間の人手ゲート**:Applying（書込）の途中で止まった Task は「リブート中」と bricked を区別できない。Applying を危険区間として S2 に印を付け、区間内で止まった Task は Observe で Done を確かめられない限り自動で再開せず、人手ゲート（承認で再開・却下とタイムアウトで halt）にする（判定の具体は §29.1）。

収束との接続（収束は判定とゲートのみ、手順は Task へ委譲）。差分検知しても直接 Apply せず Task を起動するだけ、というポリシーを Resource に付ける。

```yaml
resource: switch-fw
target_version: "24.2R1"      # 収束が監視する「あるべき状態」(機器ファームウェア版)
update_policy:
  strategy: task              # 差分検知しても直接 Apply せず Task を起動するだけ
  window: "Sat 02:00-04:00"   # メンテ窓外では起動しない
  rollback_on_fail: true
```

- **1-outstanding 制約**:`strategy: task` の起動は、同一リソースにつき未完了の Task/Workflow インスタンスを高々 1 つに制限する。level-triggered な収束は同じ差分を毎周期検知し続けるため、抑止がないと手続きが周期ごとに乱造される（人手対処 Workflow ならチケット乱造・§14.7）。未完了（承認待ち・halt を含む）の間は再起動せず、完了または cancel 後の周期に差分がなお残っていれば改めて起動する。

### §6.3 Workflow —— 順序 + 不変条件 + 承認ゲート

ローリングアップグレードは Task では足りない。集合をまたぎ、**動的な状態依存**（隣が今 healthy か）・**全体不変条件**（常に N 台 active / quorum 維持）・**部分失敗からの回復方針**を持つ。DAG は静的順序しか表せない。

```yaml
workflow: rolling-upgrade
target: cluster-db
invariant:                  # ★各ステップ前後で検証。破れるなら進まない
  min_active: 2
  quorum: maintained
strategy:
  order: one-at-a-time
  select: least-loaded-first
steps:
  - drain: "{node}"
  - failover_from: "{node}"
  - wait: { condition: role_moved, timeout: 5m }
  - firmware-update: "{node}"          # ← 多相 Task を内部で呼ぶ(階層構造)
  - health-check: "{node}"
  - undrain: "{node}"
  - verify: { invariant: min_active }
on_failure:
  policy: halt              # 既定は halt(止めて人間へ)。ローリングは全ロールバックが困難
  keep_drained: true
```

これは過剰ではない——HA 上げは本来的に「順序・不変条件・失敗回復」を持つ問題で、どのツールでも要る本質的複雑さである。それを第一級プリミティブとして構造化する。

**手順が触るリソースと前提条件（Backlog #87）。** 手順は Workflow の target と別の provider のリソースを触れる。OS の導入では「CD（`redfish-virtual-media`）にインストーラを差す」→「CD 起動にして電源オン（`redfish-system`）」の順が要り、CD を普段の宣言にすると順番が保証されない（CD より先に電源が入ると空のドライブから起動しようとして止まる）。そこで二つを入れる。

- **手順ごとの対象**：`apply`・`wait`・`require` に `resource: <provider>/<id>` を書くと、その手順はそのリソースを Observe・Diff・Apply する（その provider の Schema で比べる）。id の `{target}` は今の対象の Local ID に置き換わり（例 `redfish-virtual-media/{target}/cd`）、scope は Workflow の `target.scope`。`apply` は `{ resource, fields }` の形で書き、`resource` を書かない手順は今までどおり対象そのもの（`apply: { active: "false" }` のように fields を直に書く形もそのまま使える）。`task` は対象だけ（別のリソースの Task は多相 Task、v2）。
- **前提条件 `require`**：`{ resource, fields, timeout }`。観測で fields が満たされるまで次の手順に進まず、`timeout`（必須）を過ぎると on_failure（halt）。観測できない間も待つ（unknown ≠ absent）。`wait` と判定は同じで、「手順の結果を待つ」`wait` は timeout を省けるが、「前に要るもの」を確かめる `require` は満たされないまま待ち続けないよう省けない。

```yaml
steps:
  - apply:                                   # 1. CD を差す（別の provider）
      resource: redfish-virtual-media/{target}/cd
      fields:
        image: /iso/installer.iso
  - require:                                 # 2. 差さったことを確かめる（前提条件）
      resource: redfish-virtual-media/{target}/cd
      fields:
        image: /iso/installer.iso
      timeout: 5m
  - apply:                                   # 3. CD 起動で電源オン（target そのもの）
      boot_enabled: Continuous
      boot_target: Cd
      power: "On"
```

手順が触るリソースは target と同じに扱う：done でない Workflow の target と手順の resource（target ごとに展開したもの）は収束ループが Apply せず、plan・status は「Workflow に任せる」と示す。2 つの Workflow が同じリソースを触る宣言と、`strategy: task` のリソースを手順で触る宣言はロード時に拒否する。承認ゲート・1 台ずつ（target ごとに全手順）・不変条件（`min_active` は target だけで数える。target でないリソースへの手順は active の見込みを変えない）・S2 の進行位置・起こし直しからの再開は変わらない。claim は Workflow が触るリソースがすべて claim の内にあるシャードだけが進める。

### §6.4 承認ゲート

段階適用の承認ゲートは既存 Workflow（wave + invariant + Event/sink + フェーズ永続化）の組み合わせで表現でき、新プリミティブは不要である。ゲートは health_check（自動）と manual_approval（人）を持てる。

```yaml
rollout:
  stages:
    - name: canary
      target: { cluster: least-critical }   # 影響度順(明示指定)
      parallelism: 1
      gate:
        health_check: required               # 自動ゲート(conformance + health)
        manual_approval:
          requirements:
            quorum: { n: 2, of: 3 }          # N-of-M:3人の候補のうち2人の承認で通過
            roles: [platform-admin]          # 役割制約(役割判定は上位 IdP)
            separation: applicant_ne_approver # 申請者≠承認者を強制(4-eyes の中核)
    - name: broad
      target: { cluster: rest }
      parallelism: 5
      gate: { manual_approval: required }
  on_stage_fail: halt
```

**承認ゲートのフロー**:ステージ完了 → `StageAwaitingApproval` Event 発火 → sink 経由で人へ通知 → Workflow は「待ち」フェーズで停止（S2 の永続化により留まる）→ 承認 API → 次ステージ。タイムアウトで halt する（`timeout` はゲート単位で宣言可・既定 24h。部品調達を伴う物理作業の観測ゲートでは週単位が正当・§14.7）。

- **API 形**:`POST /workflows/{id}/gates/{gate}/approve`（却下 = halt）。ボディに承認者 ID + 署名を載せる。ゲートは requirements を満たす承認が揃うまで「待ち」に留まり、揃った瞬間に次ステージへ進む。
- **承認 API 自体は認可を持たない**（誰が叩けるか・役割 X の判定は上位 IdP）。ゲートは「役割 X の署名が N 個必要」を requirements として保持するだけである（コアは認可を持たない・§7）。
- **申請者≠承認者（separation）**:PR や planned_change の申請者 ID と承認者 ID の不一致を強制し、自己承認を型で塞ぐ。
- **全承認者を S4 監査に記録**:誰が・いつ・どの署名で承認したかの全員分を S4 に append する。承認は「事実」として恒久記録され、「どの承認ゲートを誰が通したか」が追える。
- requirements を宣言しないゲートは従来どおり単一承認である。

### §6.5 RequiresTaskEvent —— 非冪等操作の収束ループからの排除

**非冪等な操作を毎周回る収束ループに載せてはならない。** 収束ループは冪等性（同じ Apply を何度実行しても同じ結果）を前提に毎周期 Apply を叩くため、非冪等操作（migration・firmware 書込・フェイルオーバー等）を Resource として直接 Apply させると、毎周期の再実行が破壊を招く。

そこで、非冪等操作を要する Resource は **`strategy: task` を宣言**し、収束は「差分を検知したら Task を起動する」というイベント発火だけを担う（§6.2）。収束ループが直接手を下すのは冪等な操作に限り、非冪等な手順は Task/Workflow というイベント駆動の手続きに排除する。これが「いつ = 収束 / どう = 手続き」の分離の型レベルの実装であり、収束の冪等性契約（§5.4 Apply）を非冪等操作から守る仕組みである。

---

## §7 所有と境界

### §7.1 ownership 2層が貫く7つの関心事

所有権は2層である。この1軸が、書込経路から blast-radius まで貫く。

- **`ownership: platform`** … 共有基盤（クラスタ / NW コア / 共有ストレージ）。誰か一人の所有ではない。platform 層の理想状態に**一度だけ宣言**する（三重定義を防ぐ）。
- **`ownership: tenant`** + **`tenant: <name>`** … 専有（VM 等）。

`ownership` が決めるものを表にする。これが原則6（ownership がすべてを貫く）の実体である。

| 関心事 | tenant | platform |
|---|---|---|
| 1. 書き込み経路 | **即時（レビュー無）** | **PR 必須（人が承認）** |
| 2. remediation | auto でよい | alert-only 寄り |
| 3. rate limit | 緩め（warn） | 厳（halt） |
| 4. 爆発半径 | 小（砂場内） | 大（全体） |
| 5. 供給源（source） | DBSource | FileSource（git） |
| 6. シャード境界 | scope で砂場を切る | 共有上流 |
| 7. prune 境界 | 自テナント所有分に限定 | platform 層に閉じる |

テナントの closed-world scope は**自テナント所有分に限定**され、**prune が共有基盤を消さない**。テナントは共有基盤を **placement 参照**するが**所有しない**。DAG は**扇状**（共有基盤 = 全テナント共通上流、テナント間はエッジなし = 独立・並列）。

**テナントがレビュー不要な理由**:scope で砂場境界を切ってあり、他テナントも基盤も壊せない。テナント内でレビューが要るかはテナントの自己責任である。プラットフォームは砂場の境界だけ保証し、砂場内の統治は委ねる。テナントが基盤に触れる操作（採番消費・容量消費）は、**authority（採番）と quota（容量）という事前に区切られた枠**を通すことで即時のまま安全であり、枠を超える要求だけ platform 層の承認に上がる。認可・quota・課金・セルフサービス窓口はコアの責務ではなく、上位プラットフォーム / IdP に委ねる（コアは論理分離の境界保証だけを担う）。

### §7.2 managed fields —— 外部システムが動かす欄の diff 除外

外部システムが動かすフィールド（DRS の vMotion・動的 IP・HA 電源状態）を「理想と違う」と収束すると、プラットフォームと**永久に喧嘩**する（非ゼロ diff が恒常化する）。**Schema で `managed` 宣言したフィールドを diff 対象から自動除外**する（Terraform `ignore_changes` / k8s server-side apply 相当）。

```yaml
resource: vm
ownership: tenant
tenant: acme
id: web-1
placement:
  cluster: prod-cluster                 # 依存先はクラスタ(プール)。intent が管理する
  demand: { cpu: 4, memory: 8 }         # 要求する容量
  # 現在どのホストに居るかは runtime = managed field。理想状態に書かない
```

宣言した intent のフィールドだけを管理し、runtime placement 等は無視する。これはフィールド単位の除外であり、次の `external_owner`（scope 単位の除外）と対をなす。

### §7.3 external_owner —— 外部オーケストレータ所有 scope の除外と一方向協調

UniFi・DRS/ACI・Heat/Neutron・k8s operator・MHA・VNFM・AWS マネージド等、**内部に自前の収束ループを持つ外部オーケストレータ**が管理対象の一部を所有する構成がある。managed fields（フィールド単位）では足りず、**scope / リソース種別単位の除外**を第一級で持つ。

**（a） 外部所有 scope の除外宣言。** ある scope / フィールド群を「外部オーケストレータが所有する」と宣言し、**Sakutei は Observe（可視化）はするが Apply しない**（収束対象から外す）。

```yaml
scope: { cluster: prod, subsystem: vm_placement }
external_owner: drs            # DRS が所有。Sakutei は Observe するが Apply しない
```

除外宣言は理想状態ロード時に検証する（prune・循環検出と同じ入口）。v1a では最も基本の "alert-only 相当の Apply 抑止" として入れる。「この収束器は触らない」を宣言的に固定するのが要点である。

**（b） 「観測して待つ」一方向協調 seam。** 縁切りだけでなく、外部の収束結果を **`ready_condition`（§6, §11）として Observe で待つ**ことで協調する（強い制御でなく結果整合）。例:MHA がフェイルオーバーを完了したことを Observe し、その ready を下流の gate にする。**Sakutei から外部へ指示は出さない**（外部の内部ループには介入しない）——協調は「観測して待つ」の一方向に閉じる。これはシャード間依存（観測 ready 待ち）と同じ機構の再利用であり、新プリミティブは要らない。

`ready_condition` の連携は、provider が `Observation.Ready` に名前付き condition の充足状況を返し、コントローラの依存解決（`canApply`）が上流の `Ready[condition]` が真になるまで下流を skip し続けることで成立する。これは失敗伝播・下流 skip と同じ機構であり、外部リソースにも張れる。

### §7.4 scope + managed による境界宣言

境界の宣言は2つの粒度を持つ。**scope**（§4.2）がリソース種別・所有単位の境界を切り、**managed**（§7.2）がフィールド単位で「触らない欄」を切る。両者を組み合わせることで、「この scope は DRS が所有（`external_owner`）、その中でこのフィールドは runtime（`managed`）」というように、外部世界との境界を宣言的に固定できる。prune・循環検出・境界検証はすべて理想状態ロード時の同一入口で行う。

### §7.5 合成リソースの子への ownership 伝播（managed_by）

合成リソース（§9 render フェーズ2 で下位リソース群を上位1つに束ねる）は、**下位を別 Resource として依存グラフに乗せる**（1つの provider に押し込まない）。子が独立した Resource だからこそ、子 50 個の削除を「1 リソースの変更」ではなく「50 件の削除」として rate limit（§13）で halt できる——合成を単一 Provider にすると安全機構が効かなくなる。

このとき**子は親の ownership を継承する**。子リソースは親合成リソースに `managed_by`（どの親が展開した子か）を持ち、これにより:

- **上位削除 → 下位を巻き取り**:親が削除されると、`managed_by` で結びついた子が所有権に従って巻き取られる（`pruneable` の判定も親から伝播する）。
- **所有境界の一貫性**:親が `ownership: tenant` なら展開された子も同じ tenant scope に属し、prune 境界（§7.1）・書込経路が親から一貫して決まる。人が子を個別に intent へ書いたわけではないので、子の所有は親から機械的に導出される。
- **粒度の保持**:`managed_by` は所有の伝播だけを担い、drift 個別検知・原因タグの粒度・部分失敗の扱いは子ごとに保たれる（子が独立 Resource である利点）。

これにより、合成リソースは「見せる面は1つ」でありながら、内部では所有・安全・粒度が親から子へ一貫して伝播する。


