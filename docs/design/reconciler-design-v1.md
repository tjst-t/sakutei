# 宣言的インフラ収束エンジン — v1 設計メモ

> **改訂履歴(本文が常に「現在の正」。以下は経緯のみ)**
>
> **v1.1**(設計レビュー反映・実装前のコア厳密化):(1) Provider/Lister 二層分離で prune の集合列挙を
> 可能に、(2) Observation と ConvergenceStatus を型分離し Observe/Diff 分離(dry-run の根拠)を回復、
> (3) 長時間 Apply をサイクルから非同期化 + in-flight排他、(4) provider Capabilities で機械的縮退、
> (5)「stateレス」を4クラス分類学に精密化、(6) Vault sealed-restart の MTTR明記+SecretRefキャッシュ、
> (7) シークレット世代を render時解決(git書き戻し廃止)、(8) converging の3層検知+timeout、
> (9) per-resource backoff、(10-13) 重複統合・比較表修正・監査の外部アンカー・順序付き型の規則明文化。
>
> **v1.2**(上流の規律):検討が下流(Provider・実行)から**上流(desired がどう作られるか)**に移った
> のに上流に規律が無かった。→ **render パイプラインを第一級化**(2フェーズ統一・外部入力の障害時挙動と
> 頻度規律を一元化・intent_revision / rendered_revision の分離)。その派生として、**原因タグに E
> (外部入力変化)を追加**、**allocation の Release に非対称原則を適用**(仮予約 reserve→confirm、
> 下流削除の Observe 確認後のみ返却)、**デジタルツインを「render 入力の差し替え」と定義**、
> credential broker の可用性を Vault と対称化、SecretRef / CredentialProvider の境界を定義。
> 骨格(3プリミティブ・seam群・ownership 貫通)は一貫して不変。

k8s のリコンシレーションループのアイデアを抜き取り、汎用インフラ管理に使う。
k8s の too much(apiserver / etcd 分散合意 / scheduler / admission / CRD 足場)は削り、
**level-triggered な収束ループ**という本質だけを残す。

**構成(v1.2 で正直に更新)**:**収束エンジン core は単一バイナリ・単一デーモン。周辺は小さな衛星群**
(credential broker / allocation authority / intent UI)。衛星を分けた理由は本文自身の主張——
**ライフサイクル差・可用性の逆転(認証は engine の依存先)・攻撃面の分離**。要点は **core が小さく
保たれている**ことで、衛星が生えたのは責務境界を正しく引いた結果(偶有的複雑さを避ける原則は維持)。

立ち位置:**Ansible の冪等性 + k8s のループ**。宣言的・観測stateレス・かつ回り続ける。

| | 理想状態 | 観測state | 駆動 |
|---|---|---|---|
| Ansible | 外部宣言 | 持たない | 一回push |
| Terraform | 外部宣言 | 持つ(drift地獄) | 一回push |
| k8s | 内部etcd | **持つ**(status subresource / informer cache) | ループ |
| **本システム** | 外部宣言(ファイル) | **持たない**(観測stateレス) | **ループ** |

(k8s は status subresource を etcd に永続化し observed state を持つ。本システムはそれすら持たず
毎周 Observe する——この差が「DB不要・drift検知漏れゼロ」を生む。ただし正確には観測stateのみ
レスで、進行/履歴/監査stateは限定的に持つ。上記「state の分類学」参照。)

---

## コア抽象

> **v1.1 改訂(レビュー反映)**:当初の3メソッド Resource interface には、型を書き始めた
> 瞬間に露呈する2つの穴があった——(1) 個別Observeしか無く prune のための集合列挙が
> できない、(2) `drifted/converged` を Observe が返す形は Observe/Diff 分離を壊す。以下は修正版。

reconcile のコアは「個別リソースの操作」と「集合の列挙」の二層に分ける。

```go
// 個別リソースの操作(全 provider が実装)
type Provider interface {
    Observe(ctx, id string) (Observation, error)   // 現実を見る(desired を知らない)
    Diff(desired, current State) Plan               // 差分を計算(現実を触らない)
    Apply(ctx, Plan) error                          // 冪等に寄せる
}

// 集合の列挙(capability interface = 実装は任意)
type Lister interface {
    List(ctx, scope Scope) ([]Observed, error)      // scope内の"現実の全集合"
}
```

**(1) prune には Lister が要る**(レビュー項目1)。削除は `現実にある集合 − 理想にある集合`
という**集合演算**で、個別の `Observe(id)` では「理想に書かれていないリソース」を発見できない。
`DeletionPolicy: PruneWithinScope` は **Lister を実装する provider にのみ許可**し、Lister 無し
provider に prune を宣言したら理想状態ロード時に拒否(循環検出と同じ入口・同じ思想)。
→ **Diff は二段**になる:集合diff(desired集合 vs List結果 → create/delete 判定)→ フィールド
diff(両方に在るものの update 判定)。reconcile の実態はもともとこの二段で、型が実態に一致する。

**(2) Observation と ConvergenceStatus を分離**(レビュー項目2)。`drifted/converged` は desired
との比較結果=Diff の出力であって、Observe(現実を見るだけ)では判定できない。混ぜると Observe が
desired を知る必要が生じ、dry-run の正しさの根拠(Observe/Diff 分離)が崩れる。

```go
type Observation struct {
    Availability Availability  // ok | unknown(到達不能) | converging(相手が収束作業中)
    State        State         // Availability==ok のときのみ有効
}
// 収束判定(報告用)はコントローラが導出する:
//   unknown / converging → Observation から
//   converged / drifted  → Diff 結果(Plan が空か否か)から
```

四値 `unknown/converging/converged/drifted` は「Observe の戻り」ではなく**導出された報告用
ステータス**に格下げ。Observe は desired を知らず、Diff は現実を触らない、が型で保証される。

**(4) provider capability を宣言**(レビュー項目4)。散文の注意書きを機械可読なメタデータに昇格し、
コントローラが縮退を機械的に行う。

```go
type Capabilities struct {
    TrueObserve   bool  // current を"状態"として返せる(Ansible check mode = false)
    List          bool  // → prune 可否(上記1と連動)
    Converging    bool  // 相手の作業中を検知できる
    CommitConfirm bool  // candidate/commit で自動ロールバック可(NW機器)
}
```

縮退規則をコントローラに固定:`TrueObserve:false` → 原因タグ A/B(意図/drift)は actual 比較
不能なので単一タグ `changed` に統合し、rate limit 重み付けは破壊的側に倒す(**不明は危険扱い**=
非対称原則の適用)。`List:false` → その型への prune 宣言はロード時拒否。Ansible でも `*_info`
系モジュールを Observe に使える対象は `TrueObserve:true` に部分昇格でき、縮退は全面でなく部分回復可能。

削除の可否は Resource 種別ごとの `DeletionPolicy` + `pruneable`(「削除セマンティクス」参照)。
Observe / Diff / Apply の分離により `--dry-run` は「Diffで止める」だけで原理的に正しい。

---

## v1 決定事項

| 項目 | 決定 |
|---|---|
| 収束モデル | level-triggered ループ、常駐デーモン |
| state | **観測stateレス**(Resource収束経路のみ)/ 進行・履歴・監査stateは限定的に持つ(下記分類学) |
| 理想状態 | **ファイル正** + インメモリ保持 + write-through |
| 依存解決 | DAG / 波(wave)単位並列 / **モデルA(毎周全DAGを流す)** |
| 失敗伝播 | 依存失敗なら下流を skip + 次周で回収 |
| Observe | 毎周・全リソース・並列(semaphoreで並列度制限)・**間引きなし** |
| 実行境界 | **Executor interface** で分離、v1は in-process のみ |
| 分断対応 | 型に **unknown ≠ absent** の余地だけ残す。lease等は将来 |

### state の分類学(v1.1 改訂・レビュー項目5)
「stateレス」の看板は不正確になっていた(Task進行・Workflow位置・承認待ち・監査ログ・振動検知・
ベースライン・backoff・Since 等が蓄積)。正確には**観測stateレス**であり、他の state は限定的に持つ。
4クラスに分け、クラスごとに置き場と再起動時挙動を定義する。判定基準は「**喪失が事故になるのは S2 だけ
→ そこだけ永続化**」。

| クラス | 中身 | 置き場 | 喪失時 |
|---|---|---|---|
| **S1 観測state** | リソースの current キャッシュ | **持たない**(本来の主張) | — |
| **S2 進行state** | Task/Workflowフェーズ・承認待ち・**シャードルーターの割当表** | **外部永続化必須** | 事故(だから永続化。割当表の喪失=誤配送) |
| **S3 履歴state** | 振動検知・ベースライン・backoff・Since・in-flight集合 | インメモリのみ | 検知が数周鈍るだけ(安全側) |
| **S4 監査state** | append-onlyログ | 別ストア・収束経路から不可視 | 収束に影響なし |

これで全クラスの再起動セマンティクスが定義済みになり、「再起動安全・DB不要」という運用上の推論が
正しくなる(誤解の元だった S3 の振動検知・ベースラインは、消えても数周鈍るだけで安全側に倒れる)。

### 理想状態:ファイル正 + write-through

正(source of truth)はファイル。起動時に読んでインメモリ保持、通常運転は全部
メモリ上=速い。APIを足す場合はインメモリ更新+ファイルへ write-through。
→ **DBプロセスゼロ、単一バイナリ維持。`git diff` で理想状態の変遷が読める。**

並行書き込み or クエリ性が実問題になった時点で embedded KV(bbolt / SQLite)へ。
`DesiredStore` interface で切っておけば移行は局所的。分散合意(etcd)は単一
デーモンには不要 — etcd は「多書き手の合意」のためで「永続化」のためではない。

### 依存解決:DAG(モデルA)

```
毎 reconcile サイクル:
  1. 全リソースを Observe(並列・semaphore で並列度制限)
  2. 各リソースの Diff → 変更が要るノード集合 D
  3. D の部分DAGを構築
  4. Kahn で波に分解:
       while 残ノードあり:
         wave = 入次数0 かつ 上流が成功済み
         wave 内を並列 Apply
         失敗ノードの下流を skip 集合へ
  5. 「Diff が前回より縮んだか」を記録(振動検知)
  6. jitter付き sleep → 次サイクル
```

- 依存宣言:v1は明示 `dependsOn`。将来は参照からの暗黙抽出に置換。
- **循環検出は理想状態ロード時に一度**。循環していたらそのリビジョンを入口で拒否
  し、ループには持ち込まない。
- モデルB(リソース単位ループ+依存gating)は k8s 級規模で初めて元が取れる。
  本システムの規模(数十〜数百)では **plan が常に全体で正しい**モデルAの価値が勝つ。

#### 長時間 Apply とサイクルの関係(v1.1 改訂・レビュー項目3)
VM作成・image DL・firmware など分単位の Apply が wave に入ると、素朴なモデルAは
「ループが止まって他の drift 検知が遅延」か「サイクルを重ねて同一グラフへ並行Applyで競合」の
どちらかに陥る。実運用では Observe コストより**先に Apply 時間が刺さる**(DNSプロバイダの最初の
一歩では見えず、VM/imageで初めて露呈)。→ **Apply だけをサイクルから切り離す**:

```
・Observe/Diff ループは毎周・定刻で回り続ける(他リソースの drift 検知が長Applyに巻き込まれない)
・Apply は goroutine で非同期実行し、対象リソースを in-flight 集合に入れる
・in-flight 中のリソースは converging と同じ扱い(再plan・再Applyしない、下流は skip → 次周回収)
・サイクルは重ねない(前周の Observe/Diff が終わってから次周)。長いのは Apply だけが継続
・in-flight 集合はインメモリのみ(S3)。再起動で消えても Apply冪等 + 次周Observe で安全に再導出
```

既存の wave/skip セマンティクスに「Apply完了はサイクル内でなくてよい」を足すだけで、モデルAの
構造は不変。per-resource の in-flight 排他が「同一リソースへの並行Apply」を防ぐ。

### Observe:毎周全部・間引きなし

並列化できる(goroutine)ので、コストは逐次和ではなく最遅Observeに漸近。
毎周全部見ることが stateレスの価値の源泉(drift検知漏れゼロ)。間引きは本末転倒。

```go
sem := make(chan struct{}, 16)  // 同時Observe上限(rate limit避けの礼儀)
// 全 spec を goroutine で Observe、sem で並列度を絞る
```

**割り切りが破綻する早期警戒線**(立つまで間引き・TTL・頻度別スケジュールは不要):
- Observe全体が reconcile間隔に対し無視できない比率(例:間隔30sに対し10s超)
- 特定プロバイダの API課金 / rate limit が Observe頻度で効く
- リソース数が桁で増えた(数百→数千)
- **Apply時間**が長い(VM/image/firmware)→ 上記「長時間 Apply とサイクルの関係」で対処済み
  (Observe間引きより先にこちらが刺さる)

**per-resource backoff(Apply のみ・レビュー項目9)**:Observe は毎周続ける(安価・drift検知維持)が、
恒常失敗リソース(権限欠如等)を毎周 Apply で叩くのは相手APIへの攻撃になる。(リソース, エラー種別)
単位の指数backoff + jitter で Apply をゲート。分類は Event の transient/stuck を流用:transient →
backoff(上限15分程度)、permanent(権限・バリデーション)→ 即 stuck 化し **desired か provider
version が変わるまで再試行しない**(原因タグの再利用)。backoff テーブルは S3(インメモリ)=再起動で
消えても一回余分に叩くだけ。

### 実行境界:Executor interface(seam を切る、split はしない)

```go
// これが seam。将来この後ろが RPC になり得るが、v1では気にしない。
// Provider/Lister(コア抽象)を束ねる実行境界。戻りは Observation(v1.1 で確定した型)。
type Executor interface {
    Observe(ctx, specs []Spec) ([]Observation, error)
    List(ctx, scope Scope) ([]Observed, error)      // prune 対象 provider のみ有効
    Apply(ctx, plan Plan) (Result, error)
}

type LocalExecutor struct { /* providers */ } // v1唯一の実装:同一プロセス直接呼び
```

**守る規律1つ**:コントローラは `Executor` 型にしか触らない(DI で渡す)。
これさえ守れば将来 `RemoteExecutor{conn *grpc.ClientConn}` を挿すだけで分散が生える。

- ホスト内スケール目的の subprocess spawn は **却下**。Observe は I/Oバウンドで
  goroutine で足りる。プロセス境界は「隔離(障害/権限/言語)」でのみ正当化される。
- 分散(別ホスト+RPC)は正当だが「税金の塊」(登録・heartbeat・mTLS・分断)。
  v1の外に予約。**seam は今、split は後。**

### 分断対応(将来用の予約 — 型にだけ痕跡を残す)

`Observe` の戻り値に **「観測失敗/不明」を表現できる余地**(exists とは別の三値、
または error と空stateの区別)を持たせる。これは Executor インターフェースの
戻り値の形の問題なので、後付けするとseamを切り直すことになる。ここだけ最初から。

将来分散する際の原則(v1では実装不要、メモのみ):

> **確信が持てないときは、加算はしても減算はしない。**
> - 観測が unknown → 「消えた」と断じない(再作成/削除を走らせない)
> - 理想が stale(master分断・キャッシュ陳腐化)→ 「消せ」を実行しない

agent キャッシュは段階縮退で:通常=フル / master分断=加算のみ(破壊保留) /
lease失効=Observeのみ。`revision`(単調増加、ファイル世代に紐付け)+ `leaseUntil`
の2フィールドで安全性を担保。DB不要。

---

## 適用ドメインの向き/不向き

### 向く
- **Cloud / コンテナ管理**(API があり Observe/Apply が冪等・観測可能)
- **VM内 層1**:パッケージ / サービス / 設定ファイル
  (`dpkg -l`・`systemctl is-active`・ファイルhash で Observe、install/start/
  設定書換+reload で Apply)。**むしろ本命ユースケースの一つ。**
- **VM内 層2**:DBインスタンス内のオブジェクト(database / role / 権限 / extension)
  = SQLで現在状態を問い合わせ、`CREATE ... IF NOT EXISTS` で冪等適用。
  Terraform postgresql provider と同じ土俵。ループ型なので drift自動修復が効く
  (手で消された role を勝手に戻す)ぶんむしろ強い。

### 向かない(収束モデルの管轄外)
- **VM内 層3**:スキーマ変更 / マイグレーション / 行データ
  - マイグレーションは**経路依存の操作**であって宣言的状態でない
    (`rename` が `drop+add` に化けてデータ消失する古典的事故)
  - 破壊的差分の判定不能(理想に無い→現実に在る→`DROP TABLE` 本番データ、の悪夢)
  - 行データは宣言不能・観測不能

### DB管理の切り方
層1・層2までを収束対象にし、層3は収束の外へ。層3をやりたくなったら、収束
(Resource)ではなく **一回性の Task プリミティブ**(k8s Job 相当)で。完了印は
現実側(DB内 `schema_migrations`)に置けば stateレス哲学と両立(Observeで問える)。
→ 手続き的作業の扱いは後述「3プリミティブ階層」で Resource / Task / Workflow として整理。

---

## オンプレ全レイヤの適合と横断5項目

主対象はオンプレ。ほぼ全レイヤに向く(drift自動修復とstateレスの価値が最も出る領域)
が、レイヤ横断で効く落とし穴が5つあり、Resource抽象そのものに関わるため設計段階で
織り込む必要がある。**「薄いオーケストレータ」で済むのは上位が宣言的な部分(SDN/VCF/
アレイ)だけで、生のNW機器・FW・guest OS が"泥臭い"層=実装の8割はここ。**

→ レイヤ別適合表・横断5項目・Observe状態の拡張・削除セマンティクスの詳細は、後述の
**「オンプレ全レイヤ管理:横断5項目」**および**「削除セマンティクス」**節に一本化した(重複を統合)。

## オンプレ全レイヤ管理:横断5項目

オンプレの NW機器 / LB・FW / SDN / ストレージ / ハイパーバイザ / VM内OS・ソフト /
コンテナ は、ほぼ全レイヤが収束モデルに向く(drift自動修復と stateレスの価値が
クラウドより出る領域)。ただし**レイヤ横断で効く落とし穴が5つ**あり、うち3・4は
Resource 抽象そのものに関わるので**設計段階で型に織り込む**。

### レイヤ別の適合

| レイヤ | Observe手段 | 適合 | 主な地雷 |
|---|---|---|---|
| NW機器(ルータ/SW) | NETCONF/gNMI/RESTCONF(YANG)、旧機種はCLI scrape | ○ | 全config一括 vs 部分所有 / 管理断ロックアウト |
| LB/FW | F5 iControl, PANOS/Forti API, HAProxy runtime | ○ | FWルールは**順序付きリスト** |
| SDN | NSX-T policy, ACI APIC, OVSDB | ◎ | 相手も収束ループ=**二重ループの綱引き** |
| ストレージ | Pure/ONTAP/PowerStore REST | ◎(provision) | **volume削除=データ消失** |
| ハイパーバイザ | vCenter(govmomi), libvirt | ◎ | **DRS/HA が動かすフィールドを触るな** |
| VM内OS/ソフト(層1,2) | SSH/WinRM or 常駐agent | ○ | Observe手段で**agent問題が再来** |
| VM内 DB中身(層3) | — | ✕ | 収束外(Task行き) |
| コンテナ | podman/docker API | ◎ | k8s上なら二重ループ無意味(k8sが既に収束) |

### 横断項目

1. **「収束器の上の収束器」問題(最重要)**
   SDN・VCF SDDC Manager・ストレージアレイ・k8s は**それ自体が内部に収束ループを
   持つ**。本システムは「宣言的コントローラに intent を流す薄い層」になる。致命的な区別:
   > **converging(相手が収束作業中)を drift と誤認してはいけない。**
   途中状態を Observe して再Applyすると綱引き=永久に収束せず振動検知が鳴りっぱなし。
   → Observe戻り値に三値以上が必要(下記「Observe状態の拡張」)。

2. **順序付きコレクションが集合モデルを壊す**
   FWルール・ACL・ルーティングポリシーは**位置依存の順序リスト**(migrationと同じ
   経路依存)。unordered set-diff で扱うとルール順が入れ替わり穴が開く。
   → 順序を第一級で持つ Resource 型が要る。**規則(レビュー項目13)**:順序付き型は
   **リスト全体が原子単位**。scope=リスト、prune は置換に吸収、Apply=全置換(candidate/commit と
   好相性)。ただし **plan の表示粒度はルール単位の差分に展開**する(適用粒度≠表示粒度)。
   これで横断3(prune)と横断5(commit-confirm)がこの型に自然に収まる。

3. **破壊的差分が全レイヤの共通敵** → `pruneable` フラグ(下記「削除セマンティクス」)。

4. **プラットフォームが正当に動かすフィールドを収束対象から外す(managed fields)**
   DRS の vMotion、動的IP、HA による電源状態を「理想と違う」と収束すると
   プラットフォームと永久に喧嘩(非ゼロdiff恒常化)。
   → **宣言した intent のフィールドだけ管理、runtime placement 等は無視**。
   Terraform `ignore_changes` / k8s server-side apply の managed fields 相当。

5. **管理プレーンのロックアウト(NW機器固有)**
   config push で自分の管理到達性を切ると二度と直せない。
   → Junos `commit confirmed` / IOS-XR commit+rollback を活用。「確認が来なければ
   自動ロールバック」を Apply契約に組み込む。REST系は原子性が無いので、
   **ネイティブ candidate/commit がある機器はそれを使う**。

### Observe状態と収束ステータス(1・2・8 を統合・v1.1)

> コア抽象で確定した通り、`Observation.Availability`(Observeの戻り=`ok/unknown/converging`)と、
> 導出される収束ステータス(`converged/drifted` は Diff 結果から)は**型として別**。以下は
> converging の検知手段(レビュー項目8)。

converging は「最重要」横断項目だが、多くのAPIに「作業中」概念が無い。3層で検知し、必ず
deadline を付ける:

- **Tier1 ネイティブ**:vCenter task / NSX realization 等を持つ provider はそれを写像
  (`Capabilities.Converging = true`)。
- **Tier2 自傷猶予(普遍的な床)**:自分の Apply 直後は型ごとの `settle: 30s / N サイクル` の猶予窓を
  置き、その間の不一致を drifted 扱いしない。**検知手段ゼロの provider でも成立**する。
- **Tier3 振動からの推定**:M回再Applyしても Diff が縮まない → `oscillating` 判定し、そのリソースを
  **自動で alert-only に隔離**(綱引きの自動検疫)。
- **deadline 必須**:converging には必ず timeout を付け、超過で `drifted` + Event に昇格
  (相手ループの stuck を取りこぼさない)。全部 S3(インメモリ)で足りる。

---

## 削除セマンティクス:stateレスでも安全に消す

**削除に必要なのは履歴state(前回何を作ったか)ではなく、「所有権の境界」の宣言。**
`消すべき = 現実にある集合 − 理想にある集合`。「理想にある集合」を網羅列挙できれば
(closed-world)、毎周Observeで「現実にある集合」を取り、状態ゼロで差分削除できる。

Ansible が `state: absent` を要求するのは**状態が無いからではなく open-world だから**
(playbookは言及したものしか語らない)。本システムは **scope宣言で特定ドメインだけ
closed-world に昇格**できる点が Ansible より強い。ただし列挙不能ドメイン
(パッケージ・ファイル)は open-world のままにせざるを得ず、そこは absent 明示に戻る。

### 2モードを型で持つ

```go
type DeletionPolicy int
const (
    ExplicitAbsentOnly DeletionPolicy = iota // open-world: absent明示が要る
    PruneWithinScope                          // closed-world: scope内を網羅管理
)
```

**モード1 closed-world(scoped)** — 集合を網羅listできる種別
(DNS zone内record / volume群 / VM群 / FWルール群)。`scope + managed:true` を宣言
→ 理想に無いものを自動削除。**状態不要**(毎周現実をObserveするから履歴が要らない)。

```yaml
scope:
  provider: dns
  zone: internal.example.com
  managed: true      # このzone内は網羅管理(closed-world)。zone外には一切触れない
records:
  - { name: web, ip: 10.0.0.1 }
  - { name: db,  ip: 10.0.0.2 }
# → zone内で上記2つ以外のrecordは「消すべき」と確信できる(状態ゼロで導出可)
```

**モード2 open-world(explicit)** — 網羅listが非現実的な種別
(パッケージ / ファイル / 個別設定エントリ)。各リソースを個別宣言、消すには
明示 `state: absent`。**この層は Ansible方式が正しい**。

### pruneable による二段構え

closed-world 種別でも、破壊的な種別(volume/VM)は `managed:true` だけでは消さず、
**さらに削除opt-in を要求**する。破壊的種別は `pruneable: false` をデフォルトに。

```
pruneable:false(既定) → 境界が分かっていても自動削除しない。明示opt-inで解禁
```

「境界は分かっているが、それでも毎周ループに DROP 相当を自動実行させない」。
これは分散対応で立てた原則 **「確信が持てないときは加算はしても減算はしない」** の、
リソース種別レベルでの普遍化。

---

## 3プリミティブ階層(手続き的作業の扱い)

収束(Resource)だけでは、**手続き的作業**——migration、firmware更新、HAローリング
アップグレード、フェイルオーバー——は表現できない。これらは経路依存・非冪等・中断が
致命的で、毎周回る収束ループに載せると破壊する。手続きの重さに応じて3層に分ける。

| プリミティブ | 扱うもの | 例 | state |
|---|---|---|---|
| **Resource** | 宣言的状態・収束 | DNS record, VM, volume, 設定 | 観測stateなし(毎周Observe) |
| **Task** | 単一リソースの手続き | migration(単純型), firmware更新(多相型) | 進行中フェーズのみ外部永続化 |
| **Workflow** | 複数リソースまたぎの順序付き手続き+不変条件 | ローリングアップグレード, フェイルオーバー | 進行位置+invariant状態を永続化 |

**共通原則:「いつ・どの順で変えるか」= 収束が判定。「どう変えるか」= Task/Workflowが実行。**
収束ループはトリガ・メンテ窓ゲート・依存順序・ドリフト検知・完了確認を担い、危険な
手順そのものは Task/Workflow に閉じ込める。実行中の対象は `converging` 扱いにして
収束の介入を止める(二重ループ防止=横断項目1)。

### Task:単純型と多相型

- **単純型**(migration):一度実行して completed印を残す。二度目は実行しない。
  完了印は現実側(DB内 `schema_migrations`)に置けば Observe で問える=stateレスと両立。
- **多相型**(firmware更新):中断不可・長時間・リブート跨ぎ。実質ステートマシン。

```
firmware-update(多相Task)の相:
  Idle → Staging(転送) → Verifying(hash/署名) → Applying(書込)
       → Rebooting → Confirming(バージョン再確認) → Done
                                              ↘ Failed → Rollback
```

- **進行中フェーズの外部永続化が必須**(stateレス原則の正当な例外)。ただし永続化
  するのは"進行中フェーズ"だけ。Done判定は稼働バージョンを現実Observeで問える。
- **リブート跨ぎ**:Applying後に機器が消える → `unknown ≠ absent` で「リブート中」と
  解釈しタイムアウト付きで再出現を待つ。Confirmingで新バージョン確認して初めてDone。
- **べき等な再開**:各相の入口で「もう済んでいるか」を現実Observeで確認し二重実行を防ぐ。

収束との接続例(収束は判定とゲートのみ、手順はTaskへ委譲):

```yaml
resource: switch-fw
target_version: "v2.1"        # 収束が監視する「あるべき状態」(宣言的)
update_policy:
  strategy: task              # 差分検知しても直接ApplyせずTaskを起動するだけ
  window: "Sat 02:00-04:00"   # メンテ窓外では起動しない
  rollback_on_fail: true
```

### Workflow:複数リソースまたぎ

ローリングアップグレードは Task では足りない。集合をまたぎ、**動的な状態依存**
(隣が今healthyか)・**全体不変条件**(常にN台active/quorum維持)・**部分失敗からの
回復方針**を持つ。DAGは静的順序しか表せず、これらは表現できない。

```yaml
workflow: rolling-upgrade
target: cluster-db
invariant:                  # ★全体を通して守る制約(各ステップ前後で検証)
  min_active: 2
  quorum: maintained
strategy:
  order: one-at-a-time      # 同時に触ってよい数
  select: least-loaded-first
steps:                      # 各ノードに対して繰り返す
  - drain: "{node}"
  - failover_from: "{node}"            # activeなら role を他へ移す
  - wait: { condition: role_moved, timeout: 5m }
  - firmware-update: "{node}"          # ← 多相Taskを内部で呼ぶ(階層構造)
  - health-check: "{node}"
  - undrain: "{node}"
  - verify: { invariant: min_active }  # ★次へ進む前に不変条件を再確認
on_failure:
  policy: halt              # or rollback_node / rollback_all。既定は halt(人間へ)
  keep_drained: true
```

- **invariant を各ステップ前後で検証**し、破れるなら進まない。「次を落とすと min_active
  を割る(前ノードがまだ復帰していない)」→ 待つ。DAGにできない動的状態依存が、
  invariant + wait条件として表現される。
- **Workflow は内部で Task を呼ぶ**(階層:Workflow=集合の制御構造、Task=1リソースの手順)。
- **失敗方針を明示**。ローリングは全ロールバックが困難なので **halt(止めて人間へ)が既定**。

**これは too much ではない**:HA上げは本来的に「順序・不変条件・失敗回復」を持つ問題で、
どのツールでも要る本質的複雑さ。Ansibleで書けば `serial:` + `max_fail_percentage` +
block/rescue で同じ概念を手書きすることになる。それを第一級プリミティブとして構造化
するだけ。ただし **v1スコープ外**——Resource(収束コア)と Task が動いてから足す最上位レイヤ。

---

## バックログ(v1スコープ外・必要になったら着手)

- [ ] **API / 手編集のファイル調停**(fsnotify での理想状態 reload 競合)
      — APIを足す段階で。v1(手編集のみ)なら不要。
- [ ] **暗黙依存**(参照からの自動抽出 / 補間 `${dns.mysite.ip}`)
      — v1は明示 `dependsOn` で妥協。
- [ ] **プロセス隔離 Executor**(subprocess / unix socket)
      — プラグインの障害隔離・権限分離が必要になったら。
- [ ] **分散 Executor**(RemoteExecutor / gRPC / mTLS / heartbeat / lease / revision)
      — 2台目のホストが現実になったら。unknown≠absent の分岐だけは v1 で型に予約済み。
- [ ] **Task プリミティブ(単純型 / 多相型)** — 下記「3プリミティブ階層」参照
      — 単純型:migration 等の一回実行+completed印。多相型:firmware更新等の
        中断不可ステートマシン。Resource とは別建て。
- [ ] **Workflow プリミティブ**(複数リソースまたぎ・順序+不変条件+失敗方針)
      — HAローリングアップグレード / フェイルオーバー を扱いたくなったら。
        3プリミティブ階層の最上位。コアが固まってから。
- [ ] **embedded KV への移行**(bbolt / SQLite)
      — 並行書き込み or クエリ性が実問題になったら。`DesiredStore` interface で局所化済み。
- [ ] **プロバイダごとの Observe 並列度・間引き**
      — 早期警戒線(上記)が立ったら。それまで一律16・間引きなし。
- [ ] **順序付きコレクション型**(FWルール/ACL/ルーティングポリシー)
      — 位置依存を第一級で持つ Resource。最初にFW/ACLプロバイダを書くとき。
- [ ] **managed fields**(プラットフォーム所有フィールドの除外)
      — vCenter/DRS・SDN等、runtime placement を持つプロバイダを書くとき。
- [ ] **Apply契約の commit-confirm / transactional**
      — NW機器(Junos/IOS-XR)プロバイダを書くとき。管理断ロックアウト対策。
- [ ] **converging 状態の実装**(上位コントローラ系プロバイダ)
      — SDN/VCF/アレイ等「収束器の上の収束器」を書くとき。v1は型の余地のみ。
- [ ] **DesiredSource 抽象 / NetBox 連携**(理想状態の供給源を interface 化)
      — v1は `FileSource`(ファイル正)のみ。NetBox等の外部CMDBを使いたくなったら
        `RenderedSource` を追加。設計方針だけ確定済み(下記メモ)、コアは無変更で吸収可能。

> **注:設計段階で Resource 抽象に入れておくべき(後付けだと全プロバイダ書き直し):**
> - Observation / ConvergenceStatus の分離(Observe の戻りは `ok/unknown/converging`。四値
>   `unknown/converging/converged/drifted` は**導出された報告用ステータス**であって Observe の戻りではない)
> - `pruneable` フラグ(既定 false で破壊的削除を禁止)
> - `DeletionPolicy`(closed-world prune / open-world absent の2モード)
> - `scope + managed` による所有権境界の宣言
> - **コンテンツアドレス参照**(`image@sha256:...`)。imageは名前でなくハッシュで参照
>   させ、中身変更を drift 検知に載せる。名前参照は後から全設定書き直しになるため v1 で型へ。

---

## 実装着手の最初の一歩(参考)

DNSレコード・プロバイダ単体は Observe/Diff/Apply が極めてクリーンで、最初の1個に最適。
そこから podman コンテナ / Caddy ルート / TLS証明書 と広げ、「触ってないのに勝手に
理想状態へ戻る」体験を数百行のオーケストレータ + プロバイダ数個で作れる。

---

## メモ:設定形式と理想状態の供給源(将来)

### 設定形式
- **v1は YAML/JSON 両対応**。同じ木を吐く別シリアライザとして `Loader` interface の
  下に置く。JSON=API入出力の機械経路、YAML=人の手編集。round-trip 保持は YAML のみ配慮。
- スキーマ検証は JSON Schema を外付けし、ロード時に検証(pruneable/scope/enum の必須化、
  循環検出)。Norway問題は「version は文字列必須」等スキーマで封じる。
- 参照(暗黙依存 `${dns.web.ip}`)が主役になったら CUE へ Loader を差し替える余地を残す
  (CUEは検証+参照+導出を言語内で持つ)。Starlark は round-trip 不可で API write と
  衝突するため採らない。

### 理想状態の供給源:DesiredSource 抽象
Executor / Loader と同じ seam 戦略で、理想状態の供給源を interface 化する。

```go
type DesiredSource interface {
    Fetch(ctx) ([]Resource, revision, error)
}
// FileSource     — ファイル正(v1デフォルト)
// RenderedSource — 外部CMDB(素材) + 意図ファイル を render して供給(将来)
```

### NetBox 等の外部CMDB連携(将来・RenderedSource)
- **層を分ける**:NetBox = inventory(事実・在庫・IP割当)、ファイル = intent(ポリシー・
  pruneable・min_active・FW構造)。render 段階で合成して desired を生成。
  「NetBox を直接収束対象にする」のではなく「NetBox から理想を生成し、生成物に収束」。
- **二重管理の解消**:IP等は NetBox が正、DNS/FW はそこから導出(CUEの `for` 導出を
  組織スケールでやるイメージ)。
- **レビューゲート(推奨=案A)**:NetBox は「編集自由=提案の場」に格下げ。
  変更 → render → **生成 desired を Git に PR 起票** → 影響 diff を人間がレビュー →
  マージ → 収束。適用の権限は Git マージに移り、GUI 直変即本番を構造的に防ぐ。
  `RenderedSource.Fetch()` は**マージ済み生成物だけ**を返す(未承認の render は収束に出さない)。
- **安全弁3つ**:(1) render は決定的(同じ入力→同じ出力、diff がノイズらない)
  (2) 生成物は手編集禁止(`generated/` + ヘッダ)(3) **NetBox障害時は前回生成物を保持**、
  空 desired にしない(unknown ≠ absent の設定版。全削除PR事故を防ぐ)。
- コアは無変更で吸収可能(`DesiredSource` の内側に閉じる)。

### メモ:イメージ供給(DL=Resource / build=Task)
- `image → VM` の依存は既存DAGでそのまま表現(依存 ready まで下流払い出しを gate)。
- **DL登録**=Resource(収束)。ready判定は存在でなく**チェックサム/署名一致**。Applyは
  長時間になりうる。v1は同期Applyで割り切り、待ちが問題化したら非同期(Task型)へ。
- **build**=Task(一回性)。完了印は成果物レジストリを Observe。
- **コンテンツアドレス参照**(`image@sha256`)は上記のとおり v1 で型に入れる(drift検知の正しさに直結)。
- **image GC / retention**(参照ゼロ + `keep_last: N`)は将来。v1は「消さない・手動GC」で逃げる。

### メモ:正常性確認・合成検証・通知(2軸+1に分ける)
すべて「あなたのシステムは意味を発する、判断と通知は外部に委ねる」で一貫する。

- **(a) conformance(宣言どおり在るか)** = 収束ループが既に計算している `drifted` の可視化。
  v1で「今ズレているリソース一覧」を出すだけ(副産物・実装ほぼゼロ)。
- **(b) health(在るものが動いているか)** = prober loop を作らず、**「監視設定」を Resource
  として収束**する(observability as Resource)。webサーバ定義に `observability:` を書けば
  Prometheus scrape target / blackbox / Alertmanager ルールを生成し収束。判定と通知は
  Prometheus 等に委譲。リソース削除時は監視設定も prune(孤児monitor が残らない)。
- **(c) 合成検証(VM作成削除が実際に動くか)** = 監視ではない。**時間トリガで起動する
  Workflow**(canary)。`create → assert → delete → assert(gone)` の多相手続き。
  **隔離scope(専用datastore/VLAN/命名prefix)+ TTL自動GC + cleanup反復失敗で自動停止**必須。
  削除経路の能動検証(受動監視では得られない)に価値。Workflow が固まった v2 以降。

### メモ:イベント通知(発生源と sink を分離)
- 収束失敗 / Workflow失敗 / drift / canary失敗 / 振動 を**個別に通知実装しない**。単一の
  `Event` 型に一本化し、通知は Event の consumer(sink)として分離。あなたのシステムは
  「イベント発生源」、ルーティング/エスカレーション/沈黙は Alertmanager 等に委譲。
- **収束失敗は level(継続状態)として報告**:level-triggered だから通知も firing/resolved に
  でき、失敗継続=更新・回復=resolved。毎周同じアラートを撒く破綻を回避。振動検知・依存gate
  もそのまま Event(Since 付き)。
- **Since / 連続失敗回数で transient↔stuck を自動昇格**(level-triggered が過去を毎周持つ
  無料の副産物)。分類:transient(自己回復見込み)/ stuck(権限・欠損・容量=即critical)/
  oscillating(綱引き)/ blocked(依存未達=上流Eventに集約)。
- **Workflow/canary失敗は edge(事件)**:停止位置 + 到達済みstep + invariant状態を1回通知。
  canary の cleanup 反復失敗のみ level に昇格。
- **v1**:`Event` 型 + 構造化ログ(JSON)を stdout に吐くまで。**v2**:Prometheus/Alertmanager
  exporter。通知基盤そのものは作らない。

### メモ:マルチテナント(制御面/データ面の分離 + 2層所有権)
- **渡すのは認可・課金・ポータル、渡しきれないのは論理的分離**。マルチテナントは4つの
  関心事の束:(1)論理分離(境界)(2)認可(3)quota/課金(4)セルフサービス窓口。
  2・3・4 は別システム(IdP / 課金 / ポータル)に委譲。1 だけは収束エンジン自身が持つ。
- **モデルは k8s と同じ**:あなたのシステム=テナント境界の*語彙*(namespace相当)を持つ
  データ面。上位のテナント管理プラットフォーム=認可・課金・窓口を持つ制御面。上下でなく
  制御面/データ面の分離。上位システムは `DesiredSource` の一実装(認可/quota適用済みの
  理想状態を tenant ラベル付きで投入)。**コア無変更**。
- **2層所有権(共有リソース対応)**:「tenant を scope の一次元」だけでは共有基盤で破綻する。
  所有を2層に分ける:
  - `ownership: platform`(共有基盤=クラスタ/ハイパーバイザー。誰か一人の所有でない)
  - `ownership: tenant` + `tenant: <name>`(専有=VM等)
  - テナントの closed-world scope は自テナント所有分に限定=**prune が共有基盤を消さない**。
  - 共有基盤の宣言は**platform層の理想状態に一度だけ**(三重定義を防ぐ)。
- **依存の粒度=容量プール(重要)**:VMが依存するのは**個別ホストではなくクラスタ(容量
  プール)**。`vm.placement.cluster` + `vm.placement.demand{cpu,memory}` を宣言。
  **現在どのホストに居るかは runtime = managed field**。理想状態に書かない(DRS が動かす
  ので、収束が「元ホストに戻す」誤動作をしない)。ホストは cluster の構成メンバー(`member_of`)。
- **配置はハイパーバイザーlayer(DRS等)に委譲**:「どのVMをどのホストへ」「ホストを抜く際に
  何をどこへ migrate するか」は解かない(= 捨てた scheduler を再発明しない)。あなたのシステムは
  クラスタに intent を流す薄い層。ビンパッキング/フラグメンテーションは DRS の責務
  (「収束器の上の収束器」の再適用)。DRSが「退避不能」と言えば削除Workflowが失敗する、で良い。
- **ホスト削除 = 容量充足チェック + 退避委譲 Workflow**(参照カウントではない):
  `remaining = cluster.capacity − host.capacity`、`Σ vm.demand ≤ remaining` を粗くチェック →
  「メンテモード投入 → VM退避(DRSが実行)→ 退避完了を invariant 確認 → ホスト除去」の多相手続き。
  invariant = 残余容量充足 + 全テナントVM継続。**十分な残余があれば、VMが乗っていてもホストは
  削除可能**(migrateすればよい)。容量は platform層が持ち、個別テナントは自分の demand のみ宣言。
- **削除セマンティクスは3種に精密化**:
  - 離散・被参照 → 参照カウントゼロ(image/テンプレート)
  - 容量プールのメンバー → 抜いても残余が充足+migrate可(クラスタ内ホスト)
  - 単純専有 → closed-world scope 内で理想に無い(テナントVM)
- **DAGは扇状(fan-out)**:共有基盤(クラスタ)=全テナント共通上流、テナント間はエッジ無し
  (独立・並列)。モデルA・波並列・依存gate がそのまま効く。新収束ロジック不要。
- **唯一の新しい緊張**:共有基盤の変更は全テナントに波及。メンテは「全テナントの invariant
  積集合」を守る Workflow(権限=プラットフォーム管理者)。v1は手動、将来 Workflow。
- `ownership` 2層区別・placement(cluster参照 + demand)・配置をruntime扱いにする managed
  field は、tenant次元同様に後付けだと全設定書き直しになるため型に入れる側。ソフト分離
  (ラベル)で始め、ハード分離が要れば隔離Executor seam に後付け。

---

## 未検討領域(オンプレ運用の抜け・これから一つずつ潰す)

棚卸しの結果。3層に分かれる。★ = 型に食い込むので早期に決める(後付けが高い)。

### 層1:このシステム自身の運用(メタな盲点・最重要)
「管理する側」ばかり設計し「管理する当のデーモン」を忘れていた。本番中枢なのでここが最も重い。
- [x] **可用性 / 単一障害点 & ブートストラップ / 鶏卵問題 → 段0で解決済み(下記「段0」参照)**。
      2段ブートストラップ:段0(人手・Ansible・Proxmox 3ノード + 統合Ceph + VM-HA)が
      Vault と 収束デーモンを VM-HA で保護。段0が段1(本番基盤)を、段1が段2(全インフラ)を
      収束。可用性は段0の3ノード冗長 + VM-HA で担保。stateレス+level-triggered なので収束
      デーモンは VM再起動の空白を許容(アプリHAは任意の上積み)。
- [ ] **自己アップグレード**:自分の更新中は収束が止まる。firmware Task を**自分に向ける**
      (段0は Ansible で更新するので、当面は段0側の運用として吸収)。
- [x] ★**爆発半径の制御 → 設計済み(下記「爆発半径制御」参照)**。level-triggered は
      「速く確実に収束する」能力が「速く確実に破壊する」能力の裏返しで、しかも毎周繰り返す
      ため構造的に危険。rate limit + 変更の原因区別 + 段階適用/承認ゲートで対処。

#### 段0(確定):Proxmox 3ノード + 統合Ceph + VM-HA、Ansible で人手管理
役割分担が2ツールの性質差と一致:**段0 = Ansible(open-world・pull型・稀・意図的)**、
**段1以降 = このシステム(自律収束・push型・常時ループ)**。段0は"作って忘れる"不変基盤で
drift自動修復がむしろ危険(最上位ゆえ直す上位が無い)→ 人が意図したときだけ動く Ansible が正しい。

```
人間(パソコン + Ansible + 金庫)
  ① Proxmox 3ノード構築  ② 統合Ceph  ③ VM定義(Vault, credential broker, 収束デーモン)
  ④ Vault unseal
  ▼
段0(Proxmox 3ノード + 統合Ceph + VM-HA)
  ├─ Vault(VM-HA保護)              … シークレットの根
  ├─ credential broker(VM-HA保護)   … 機器認証。engine の依存先(落ちると全ターゲットの Observe が
  │                                    止まり**システム全盲**。Vault より可用性要求が重い)
  └─ 収束デーモン(VM-HA保護)        … GitHub/GitLab から pull、ローカルミラーで全断耐性
  外部依存:GitHub/GitLab(理想状態リポジトリ)= 更新取得元。稼働必須依存にしない
  ▼ 自律収束
段1以降:本番オンプレインフラ全体
```

- **git は外部(GitHub/GitLab)**:段0から git 本体が消える。各ノードにローカルミラーを持ち、
  外部git到達不能時は最後にpullした状態で収束継続(制御面ダウン時もデータ面が動く原則)。
- **Vault は段0内**(置かざるを得ない)。有状態サービス2個(Vault+デーモン)により段0が
  "小プラットフォーム化" → Ceph+VM-HA が正当化される(Proxmox統合CephでCeph運用重量が下がり
  「イージーに」が成立)。Vault自身のRaft HAより、単一Vault VM を VM-HA で守るほうが運用単純。
- **unseal は Ansible(オペレータ起動)**:auto-unseal(KMS委譲)の思想的ねじれを避けつつ手作業
  ミスも減らす中間。unseal key は playbook の外(Ansible Vault 暗号化 + 人の金庫)。段0再起動
  という稀イベントの runbook として固定化。「シークレットの根を守るものの根」は人間の金庫に着地
  =ブートストラップ無限後退の正しい終わり方。
- **可用性の正直な但し書き(レビュー項目6)**:VM-HA は可用性の担保だが、Vault は再起動で sealed に
  戻り unseal はオペレータ起動。つまり **Vault載せノード障害の MTTR は人間の unseal 時間を含む**
  (Raft HA でも sealed-on-restart は解消しない。auto-unseal を取らない限りこれが正直な線)。
  影響を縮める緩和2つ:(a) **解決済み SecretRef をデーモンにインメモリキャッシュ(TTL付き)** →
  Vault断中も稼働中の収束は継続し、止まるのは「新規/rotate直後のシークレットが要る Apply」だけ。
  (b) Vault不達中はシークレット依存リソースを **自動で alert-only に縮退**(破壊経路に落とさない=
  非対称原則)。
- 人手運用が残るのは:Proxmox 3ノードのハード保守、Ceph健全性監視(Proxmox UI)、unseal key の
  金庫管理、たまのバージョンアップ(Ansible)。年数回〜月1回レベルで段0として現実的。


### 層2:オンプレ物理・低レイヤの固有リソース
- [x] ★**シークレット管理 → 設計済み(下記「シークレット管理」参照)**。核心は「値を収束しない」
      ——収束は参照・存在・鮮度だけ扱い、値はVaultの責務。SecretRef型(遅延展開+自動マスク)と
      世代を参照に含める(rotation検知)が v1 で型に入れる側。
- [x] **PKI / 証明書 → 済(下記「残り項目まとめ」)**。SecretRef + expiry health + rotation
      Workflow。証明書はDAG上流(切れると依存サービスが倒れる)。シークレットの直系、新規なし。
- [x] **電源 / ブート / BMC → 済**。Resource + Workflow。managed field 境界に注意(意図的にoffにした
      ホストを収束が勝手にonにしない)。
- [x] **物理ネットワーク土台 → 済**。Resource。順序付きコレクション + commit-confirm(既存横断項目の
      適用先)。VLAN定義等はNetBox inventory から render。
- [x] **基盤サービス(DNS/DHCP/PXE/NTP)→ 済**。Resource + `tier: foundation`(依存の根)。粗い優先度帯
      (基盤層→通常層)で表現。細かい暗黙依存は将来。
- [x] **ストレージ掘り下げ → 済**。バックアップと同じ4分割(スナップショット存在=Resource / 一時snap=Task /
      レプリ構成=Resource / レプリ状態=health)。

### 層3:運用ライフサイクルの横断機能
- [x] **バックアップ / リストア / DR → 設計済み(下記「バックアップ / DR」参照)**。当初の一行は
      「構成=Resource / リストア=Task / DR=Workflow」で、**結果の健全性(実際に取れたか・戻せるか)**
      が抜けていた。4分割に精緻化。核心は「取れている」でなく「戻せる」を復元検証canaryで定期証明。
- [x] **監査ログ / コンプライアンス → 済(下記)**。Event永続化 + **append-only完全性(連鎖ハッシュ)**。
      git が理想変更履歴の半分を担い、Apply実行記録は追記専用ストア。**変更の原因タグ(A/B/C/D)が
      そのまま監査の帰属になる**(自己修復か・誰のcommit由来か・どの承認ゲート通過か)。
- [x] **メンテナンスウィンドウ / 変更凍結 → 済**。freeze = 全体を一時的に alert-only に倒すマクロ
      (爆発半径の gate/rate-limit 機構の一種)。freeze中も drift検知は続け、修復だけ止める。新規なし。
- [x] **キャパシティ管理 → 済(下記)**。現在充足性(今置けるか)は収束が持つ(容量プール判定・既存)。
      **予測(いつ枯渇するか)は収束の外**——時系列トレンドは監視系にexportし Prometheus 等で見る。
- [x] ★**auto-remediate vs alert-only → 済(「爆発半径制御」で回収)**。remediation mode を
      Resource/scope 属性に(auto / alert-only)。alert-only は爆発半径ゼロ=最も保守的な本番向け。
      「直すか(mode)/ どれだけ(rate limit)/ どう刻むか(staged)」の3階層の手綱の一段目。

### 意図的スコープ外(抜けではなく正しく委譲済み)
- テナントの認可・課金・ポータル → 上位プラットフォーム
- 時系列メトリクス監視・通知ルーティング → Prometheus/Alertmanager
- 配置最適化・ビンパッキング → DRS
- DBスキーマ・アプリのデプロイ中身 → 収束外(層3 resource / Task)

### 早期に型へ入れる(後付けが高い)
コア interface(v1.1 改訂で確定):
0a. **Provider / Lister の二層分離**(Lister 実装が prune の前提。無しへの prune 宣言はロード時拒否)
0b. **Observation と ConvergenceStatus の分離**(Observe は desired を知らない=dry-run の根拠)
0c. **provider Capabilities**(TrueObserve / List / Converging / CommitConfirm で機械的に縮退)

周辺(既出):
1. **シークレット参照**(Loader の `${secret:...}` 解決)
2. **爆発半径制御**(rate limit + halt、挙動は選択式)
3. **修復モード選択**(auto-remediate / alert-only を Resource属性に)
4. **変更の原因タグ**(diff結果に A意図 / B drift / C ツール更新 / D カスケード / **E 外部入力変化**を付与)
5. **provider version を diff計算に含める**(C=ツール更新由来の幽霊diff検知に必須)
6. **intent_revision と rendered_revision の分離**(E の判定基盤。v1.2 で追加)

---

## 爆発半径制御(level-triggered の急所)

**なぜ構造的に危険か**:edge-triggered なら暴発はイベント分で止まる。level-triggered は
毎周・全リソースの desired vs actual を突き合わせ差分を全部埋めにいくので、理想状態を1行
間違えると次の1サイクルで数百リソースが同時に「差分あり」判定 → 並列Applyで一斉に潰す。
バグではなく設計通りの動作(収束は理想が誤っていても疑わない)。**「速く確実に収束する」能力が
そのまま「速く確実に破壊する」能力**。しかも level 型は毎周繰り返す(手で戻しても次周また壊す)=底なし。

危険は3軸から独立に漏れる:**量**(一度に多すぎ)/ **種類**(破壊的が混じる)/ **速度**(気づく前に完了)。

### 制御1:量 — rate limit(挙動は選択式)
```yaml
safety:
  max_changes_per_cycle: { absolute: 10, percentage: 5% }
  applies_to: [create, update, delete]
  on_exceed: halt-destructive-only   # halt-all | halt-destructive-only | warn-and-proceed
```
- 閾値は**破壊的変更に重み付け**(create 100件は概ね安全=加算、delete 10件は止める)。
  「加算は許し減算は止める」の非対称(unknown≠absent / pruneable:false と同じ思想)。
- **on_exceed は scope ごとに選択式**:本番=destructive-only、dev=warn、最重要=halt-all。

### 制御2:種類 — 異常検知(振動検知の兄弟)
固定閾値だけだと正当な大規模変更で誤発動。**平常時の変更量ベースラインから外れたら止める**。
振動検知が「Diffが縮まない」を見たのに対し、これは「**Diffが突然巨大化した**」を見る=同じ
Diff時系列監視の別用途。「静かなインフラで突然大量削除」を確実に捕まえる。

### 制御3:速度 — 段階適用(ローリングWorkflowの一般化)
大規模変更を一度に全適用せず、少数→検証→次バッチ。ローリングアップグレードWorkflowの仕組みを
**通常の収束にも適用オプション**として持たせる。バッチ間の検証ゲート(conformance + health)で
悪化を検知したらそこで halt、残りを保護。「全部壊す」が「5%壊して気づいて止まる」になる。

### 貫く原則:デフォルト halt、明示で進む(fail-safe)
異常な規模の変更はデフォルトで止め、人間が「意図的だ」と承認して初めて進む。コミットに
`approved-large-change: 50 creates` のような**明示的な意図表明**を付ける。**間違いは止め、意図は通す**。

---

## 変更の「原因」区別(5分類)

収束ループが見るのは「desired ≠ actual」という**結果**だけで原因を区別しない。だが同じ差分でも
**なぜ生じたかで危険度と対応が正反対**。diff結果に**原因タグ**を持たせる(型に食い込む=早期)。

判定の基盤は **intent_revision と rendered_revision の分離**(render パイプライン節参照):
```
intent_revision   … 人が書いた理想(git commit hash / DB トランザクションID)
rendered_revision … render 出力の世代(intent + 外部入力の解決結果すべてのハッシュ)
```

| 原因 | 何が動いたか | 判定 | 対応 |
|---|---|---|---|
| **A. 意図的**(人が理想変更) | intent | **intent_revision が変化** | 適用してよい(規模制御は効かせる) |
| **B. drift**(リソース側が変化) | actual | actual が変化 | 自己修復 or アラート(remediation mode) |
| **C. ツール更新**(解釈が変化) | provider | provider_version が変化 | **最も危険・見逃されやすい**(下記) |
| **D. カスケード**(A/E の依存波及) | 上流 | 上流の変化に連鎖(DAG下流) | plan段階で波及範囲を可視化 |
| **E. 外部入力変化** | render 入力 | **rendered_revision は変化したが intent_revision は不変** | 下記 |

- **E が必要な理由**:v1.2 以降、desired は**人の commit 無しでも変わる**(シークレット rotate、
  authority 側の割当変更、inventory 変更)。これを A と誤認すると**監査の帰属(誰の commit 由来か)・
  レビューゲートの前提・rate limit の重み付けが全て誤る**。
- **E のサブタグ**:**E-secret**(世代 rotate)/ **E-alloc**(**intent 不変のまま割当だけ変わった場合**
  ——リソース追加に伴う新規払い出しは intent_revision が変わるので **A** に分類される)/
  **E-inventory**(CMDB 変更)。
- **E の扱い**:帰属は「人の commit 由来ではない」と記録(A と混ぜない)。rate limit は不明度が高いので
  **破壊的側に重み付け**(不明は危険扱い=非対称原則)。
- **C が最悪の落とし穴**:プロバイダの正規化ロジックを直しただけで、理想も現実も変わって
  いないのに既存全リソースが「今の解釈と違う」と判定され再適用される。**正しい修正でもバグでも
  同じ見た目**で区別がつかない。→ **provider version を diff計算の入力に含める**(image の
  コンテンツアドレスと同じ発想)。C を検知したら自動適用せず `ProviderUpgradeDiff` Event を
  発火、dry-run で人が中身確認してから解禁。
- **D の価値**:「image 1つ更新→依存50VMが再作成」のようにカスケード規模が直感を超える。
  plan で波及範囲を見せる。

---

## 段階適用 + 承認ゲート(既存Workflowの拡張・新プリミティブ不要)

「影響の少ないクラスタから → 問題なければ進む → 並列度を上げられる → ゲートごとに人承認(API)」は
Workflow の wave + invariant + Event/sink + フェーズ永続化の組み合わせで全部表現できる。

```yaml
rollout:
  stages:
    - name: canary
      target: { cluster: least-critical }   # ①影響度順(まず明示指定。自動推定は将来)
      parallelism: 1                          # ③並列度をステージごとに指定
      gate:
        health_check: required                # ②問題なければ進む(自動ゲート=conformance+health)
        manual_approval: required             # ④人の承認(API)
    - name: broad
      target: { cluster: rest }
      parallelism: 5                          # ③後半は並列度を上げる
      gate: { manual_approval: required }
  on_stage_fail: halt
```

- **承認ゲートのフロー**:ステージ完了 → `StageAwaitingApproval` Event発火 → sink経由で人へ通知
  → Workflow は「待ち」フェーズで停止(フェーズ永続化の性質でそのまま留まる)→ 承認API
  `POST /workflows/{id}/gates/{gate}/approve`(却下=halt)→ 次ステージ。タイムアウト(例24h)で halt。
- **承認の証跡**は監査ログ項目と合流(誰が・いつ・どのゲートを承認)。承認API自体は認可を持たない
  (誰が叩けるかは上位IAM)が、承認された事実は記録。オンプレ変更管理で必須。
- 新プリミティブ不要:足すのは「ステージ定義構文(target/parallelism/gate)」と「承認を受ける口(API)」だけ。

---

## シークレット管理

### 大前提:値を"収束"しない
他リソースは「理想の値に現実を寄せる」が、シークレットにこれを適用すると破綻する。理想状態(git)に
値を書けない → 収束エンジンは「あるべき値」を知らない/知るべきでない → **値のdiffが取れない**。
よって**収束が扱うのは値でなく参照・存在・鮮度だけ**。値はVaultの責務(DBデータを収束外にしたのと同じ切り分け)。

### 3つを分ける(混同しない)
| 関心事 | 何を | どこで |
|---|---|---|
| 1. 参照の解決(consumption) | `${secret:...}` を実値に展開しプロバイダへ渡す | Loader/Executor |
| 2. 供給(provisioning) | シークレットを作る・置く・rotate | 収束 + Task/Workflow |
| 3. 根の保護(root of trust) | Vault unseal・最初のトークン | 段0(人手・解決済み) |

### 関心事1:参照解決 = 遅延展開(just-in-time)★型に食い込む
- **早すぎる展開は禁**:読み込み時に全展開して理想状態に実値を埋めると、ログ・Event・ダンプに漏れる。
  展開が早いほど漏洩面が広がる。
- **正**:理想状態は最後まで**参照のまま**保持。実値化は**Apply直前・プロバイダに渡す瞬間だけ**、
  使用後メモリから消す。diffは参照同士で取る(値を知らなくてもdiffできる)。
- **`SecretRef` 専用型**(ただの文字列にしない):String() が `***` を返し**自動マスク**、実値取得は
  明示 `.Resolve(ctx)` 経由のみ。「うっかり平文化」を型で防ぐ。v1で型に入れる本体。
- **バックエンド複数を型で想定**(`vault:` / `k8s:` / `file:` プレフィックス)。Loaderは
  バックエンドプロバイダとして抽象化(Executor/Loaderと同じseam)。v1はVault一本。

### 世代の扱い:render時に解決(git書き戻しをしない)★型に食い込む(v1.1 改訂・レビュー項目7)
参照モデルの副作用:Vault側で rotate しても参照が不変なら「差分なし」→ 使う側(接続設定等)が
古いまま=**接続断**。当初案「参照に `@v5` をピン留め」は、rotation後に誰が desired を v6 に上げるか
という問題を生む——git に書き戻すなら**システムが理想状態リポジトリへの write 権限を持つ**ことになり、
「理想はファイル正・人がレビュー」と緊張する。

**正しい形:参照はピン無しのまま、世代は desired 生成(render)時に解決する。**
```
理想状態(ピン無し): password: ${secret:vault:/db/prod#password}
   ↓ render 時、Vault の metadata から現世代を読む(KV v2 の version metadata は"値を読まずに"取れる)
下流リソースの入力ハッシュに世代を合成 → rotate すれば次周 render で世代が変わり diff が出る
```
- **毎周 stateレスに再導出**され、git 書き戻しループが消滅。image のコンテンツアドレス、provider
  version を diff に含めるのと**完全に同型**。値は隠したまま変化だけ検知する。
- 外部でrotateされても render が現世代を読むので、ピン留めによる「古いまま diff が出ない」も解消。
- **監査上ピン留めが要る規制系パスのみ**、rotation Workflow の最終ステップを「PR起票(bot は PR
  作成権のみ・マージは人)」とするオプションを残す。NetBox連携のレビューゲート(案A)の再利用で、
  この場合だけシステムに限定的な write 権限(PR作成)を与え、適用権限は人のマージに留める。

### 関心事2:供給 = 収束できるもの / Taskなもの
- **存在・鮮度は収束**:「このパスに存在すべき」を Observe(存在だけ・値は見ない)/ Apply(無ければ
  Vaultに生成させる、値は見ずに委譲)。鮮度(`rotation: every 90d`)も収束対象。
```yaml
resource: secret
path: vault:/db/prod
ensure: exists
generate: { type: password, length: 32 }   # 無ければVaultにGenerateさせる(値は見ない)
rotation: { every: 90d }
```
- **rotation実行はTask/Workflow**:「新値生成→DBに設定→アプリ設定更新→旧を無効化」の順序付き・
  協調が要る手続き(順序ミスで接続断)。migration/firmware と同じ。「いつ(90日超)=収束が検知、
  どう(回す手順)=Workflow」の再適用。

### 段0との接続(鶏卵の底)
- root of trust(Vault本体+unseal)は段0=収束の外(解決済み)。
- 段1以降は収束エンジンが `SecretRef` でVault参照。ただし**収束エンジン自身がVaultに繋ぐ最初の
  トークンだけは SecretRef で解決できない**(Vaultに繋ぐ前だから)→ 段0が環境変数/ファイルで注入する
  最小1個の平文。ここがブートストラップの底。

### スコープ
- **v1**:`SecretRef` 型(自動マスク+明示Resolve、**世代フィールドを型に持たせる**)、遅延展開、
  Vaultバックエンド1つ、段0からのトークン注入経路。
  ※**世代フィールドの意味(v1.2 で変更)**:世代は**ユーザが書く参照には現れない**(ピン留めではない)。
  **render フェーズ1が Vault metadata から解決して SecretRef に注入するフィールド**であり、
  型に持たせるのはそのため(後付け不可)。世代変化を検知して下流収束する*ロジック*はv2でよい。
- **v2**:存在収束(generate)、世代変化のrotation検知ロジック、rotation Workflow、複数バックエンド。
- **作らない**:シークレットストア本体(Vault)、値生成アルゴリズム(Vault/プロバイダに委譲)。

---

## バックアップ / DR

「リソースとして」は半分だけ正しい。バックアップ**構成**はResourceだが、**結果の健全性**(実際に
取れたか・戻せるか)は収束外(前の health「在る≠動いている」がここでも効く)。4つに割れ、
**全部が既存プリミティブに割り当たる(新規なし)**。

| 側面 | 何を | プリミティブ | 同型の既存設計 |
|---|---|---|---|
| バックアップ構成 | ジョブ・保持ポリシーが在るか | **Resource(収束)** | 通常リソース |
| バックアップ結果 | 実際に取れたか・鮮度・復元可能性 | **health/canary**(収束外・報告) | (b) health監視 |
| リストア | 実際に戻す | **Task**(一回性) | migration |
| DRフェイルオーバー | サイト切替 | **Workflow**(invariant付き) | rolling upgrade |

**最大の落とし穴**:バックアップジョブが存在する ≠ バックアップが取れている。構成が `converged`
でも毎晩失敗して1ヶ月バックアップが無い、が起こりうる。構成の収束は結果の健全性を保証しない。

### 結果の健全性(ここが本質・当初の一行から欠けていた)
- **鮮度 = 時間ベースの health述語**(証明書 expiry と同型):
```yaml
vm:
  id: db-1
  backup: { schedule: daily, retention: 90d }
  health:
    - backup_freshness: { max_age: 26h, severity: page }  # 最終成功から。SLA 24h+猶予
```
- **復元可能性 = 合成検証(canary)**:存在し新しくても壊れて復元不能かもしれない(運用最大の恐怖)。
  「取れているか」でなく「**戻せるか**」を確かめるには実際に復元するしかない = VM作成削除canaryと同型。
```
restore-verification(定期Workflow・canary):
  隔離環境に最新バックアップからリストア → 起動確認 → 整合性チェック → 破棄
```
  前に作った「隔離scope + TTL + 検証」がそのまま効く。**"ある"でなく"復元できることを定期証明"**が核心。
- **3-2-1 等のポリシー = invariant**(「常にNコピーがM拠点に存在」を監視述語に)。

### DRフェイルオーバーの特殊な難所
通常Workflowは「健全な収束エンジンがインフラを操作」。DRは「**プライマリが死んだ**のでセカンダリへ
切替」だが、収束エンジンがプライマリに居たら**それも死んでいる**=フェイルオーバーを指示する主体が
不在。→ DR自動化は**制御面のマルチサイト**が前提(段0のDR先パブクラ折衷がここで効く)。
**自動化は最後発**、手動runbook(人がセカンダリ側でWorkflow起動)で長く足りる。

### スコープ
- **v1**:バックアップ**構成**を Resource として収束(ジョブ・保持ポリシーの存在)。これだけ。
- **v2**:バックアップ**鮮度**の health述語(`backup_freshness`、証明書 expiry と同型)。
- **v2〜v3**:復元検証 canary(隔離リストア。既存 canary 機構の適用)。
- **後発**:リストア Task、DRフェイルオーバー Workflow(手動起動から。自動化は制御面マルチサイトが要る)。

---

## 残り項目まとめ(全て既存プリミティブへ割り当て・新プリミティブゼロ)

未検討リストの残り8項目を検討した結果、**新プリミティブは不要**。大半は既存の
Resource/Task/Workflow/health/SecretRef と既存横断項目(managed fields / 順序付きコレクション /
commit-confirm / gate)への割り当てで閉じる。これは設計骨格がオンプレ運用の広域をカバーできる
ことの検証にもなっている(抽象が正しく選ばれていた)。

| 項目 | 帰結 | 新規判断 |
|---|---|---|
| PKI/証明書 | SecretRef + expiry health + rotation Workflow。証明書はDAG上流 | なし(シークレット直系) |
| 電源/BMC | Resource + Workflow。managed field 境界注意 | なし |
| 物理NW | Resource。順序付きコレクション + commit-confirm | なし(横断項目の適用) |
| 基盤サービス | Resource + `tier: foundation`(依存の根) | 暗黙依存/tier(小) |
| ストレージ掘り下げ | バックアップと同じ4分割 | なし |
| **監査ログ** | Event永続化 + **append-only完全性 + 原因タグ帰属** | **完全性が新要件** |
| **キャパシティ管理** | 現在充足=収束、**予測=収束の外(監視へ)** | **予測は収束外の線引き** |
| 変更凍結 | freeze = 全体を一時 alert-only にするマクロ | なし(既存gate拡張) |

### 新しい設計判断(実質2つ)
1. **監査ログの完全性**:通常ログと違い改竄耐性が要る。append-only + 連鎖ハッシュ(git がコミット
   連鎖で理想変更履歴の半分を担う)。Apply実行記録は追記専用ストア。収束は自律動作ゆえ「誰が」の
   帰属が問題だが、**変更の原因タグ(A意図/B drift/C ツール更新/D カスケード)がそのまま帰属になる**
   ——原因区別の設計が監査で再利用される。
   **外部アンカー必須(レビュー項目12)**:連鎖ハッシュ単体では先頭(最新head)を外部に固定しないと
   全体書き換えで改竄可能。最安は「日次で head hash を Event として通知 sink に流す + git repo に
   note で記録」。**git push 先(GitHub)が事実上の改竄耐性の実体**——理想状態と同じ外部依存に
   相乗りでき、追加インフラゼロ。
2. **キャパシティの予測は収束の外**:現在充足性(今VMを置けるか)は収束が持つ(容量プール判定・既存)。
   だが「このペースでN週後に枯渇」の時系列外挿は level-triggered の構造に無い → 監視系にexportし
   Prometheus 等で傾向を見る(監視委譲の再適用)。「あと何台作れるか」は収束、「いつ枯渇か」は監視。

### 暗黙依存 / tier(基盤サービスから浮上した唯一の横断新項目)
全リソースが DNS/DHCP/NTP に暗黙依存するが、全部 `dependsOn: dns` と書くのは非現実的。
v1は `tier: foundation` の粗い優先度帯(基盤層は常に通常層より先)でDAGに順序を与える。
細かい「参照からの暗黙依存」は将来(DNS名を使うリソースに自動でDNS依存を張る)。

---

## 到達点(設計骨格の検証)

一連の検討で、v1コア(Resource収束)から始まり、オンプレ運用の広域(クラウド/コンテナ/VM/
ハイパーバイザー/NW/ストレージ/デプロイ/イメージ/監視/通知/マルチテナント/共有基盤/自己運用/
シークレット/バックアップ・DR/監査/キャパシティ)を、**3プリミティブ(Resource/Task/Workflow)+
少数の横断項目 + seam群(全列挙は「システム全体構成」の表を正とする)**で吸収できる
ことを確認した。新プリミティブの追加は Task と Workflow の2つのみで、それ以外は全て既存部品の
組み合わせ・適用先だった。

一貫して効いた設計原則:
- **level-triggered + stateレス**(毎周 desired vs actual、観測stateを持たない)
- **いつ=収束 / どう=Task・Workflow**(手続き的作業の分離)
- **確信が持てないとき加算はするが減算はしない**(unknown≠absent / pruneable:false / 段0分断 /
  爆発半径 / freeze まで貫通)
- **値/配置/予測は持たない、委譲する**(シークレット値=Vault / 配置=DRS / 予測=Prometheus /
  認可・課金=上位プラットフォーム)
- **seam は今切る、split は後**(分散・NetBox・複数バックエンド等を型で予約しコアは無変更)

---

## プロバイダ戦略:自前コア + Ansible provider の二層(網羅性の現実解)

### スコープ確定:自組織内利用が第一、OSS化はなお良し
事業化はしない。これにより事業化の重荷(プロバイダ網羅性競争・外部顧客への信用構築・
サポート体制・既存接着剤との競合)が**全て降りる**。「全ベンダー対応」ではなく**自組織が実際に
使う機器だけ**でよく、それはあなたが実務で知る有限集合。設計の質・一次情報・OSS文化という資産だけ残る。
- 順序「個人OSS → 社内利用」は妥当。ただし**職務発明/知財/OSS許諾は早めに会社のOSSポリシーを確認**
  (VCF/VCSP運用の具体が設計に入ると個人成果と業務成果の線が曖昧になるため)。技術外だが後で痛まないよう。

### Ansible を Provider に使う(Executor seam の一実装)
Ansible の**モジュール(冪等な実行実体)だけを借り、実行モデル(playbook / 一回push)は借りない**。
収束ループ(いつ・何を=あなた)と実行実体(どう=Ansibleモジュール)が噛み合う。前に切った
`Executor` interface にそのまま乗る。
```go
type AnsibleExecutor struct { ... }   // Executor の一実装
//   Observe → ansible module を check mode で叩く
//   Apply   → ansible module を通常実行(冪等適用)
```
利点:何千もある既存モジュール(NW機器/クラウド/OS/ミドルウェア)を一から書かずに収束ループへ
載せられる=網羅性問題の現実的緩和。段0がAnsible前提なので社内の学習コストも低く、既存playbook資産を
モジュール単位で流用できる。

### ただし Ansible の弱点も部分的に来る(3制約)
1. **check mode の信頼性**:モジュール依存で dry-run が不正確なことがある → Observe が信頼できる
   モジュールを選別して使う。
2. **Diff の粗さ**:多くのモジュールは `changed: true/false` しか返さない → Ansible経由リソースは
   「Diff詳細が粗い」と割り切る。
3. **横断項目の非対応**:順序付きコレクション / managed fields / commit-confirm をモジュールは
   知らない → **あなたのシステム側でラップ**して扱う(Ansibleは1操作の実行だけ、賢さは自前側)。

### 二層構成 + 時間発展
- **コア層(自前 provider)**:DNS/FW/VM 等、重要で Diff 品質・横断項目対応が要るもの。
- **ロングテール層(Ansible provider)**:雑多な機器・OS設定。**品質を落として広くカバー**。
- **時間発展**:将来はコア実装を広げ Ansible の担当範囲を狭めていく。ただし**ブロックストレージ製品の
  ような、専用APIが複雑・ベンダー固有性が高い領域は自前化が割に合わず、Ansible provider が長く残る
  可能性が高い**。網羅性を「自前で品質」と「Ansibleで広さ」のトレードオフで現実的に埋め続ける。

---

## 開発・検証環境(仮想中心・実機ゼロ志向)

### なぜ検証環境が特殊か
このシステムは**インフラを操作対象にする**ので「操作される側(vCenter/スイッチ/ストレージ/Vault)」を
丸ごと用意しないと検証にならない。しかも検証したいのは正常系より**危険な収束挙動**(prune・爆発半径・
drift自己修復・converging誤認)=**わざと壊れる状況を安全に再現**する必要がある。実機は論外(本番を壊す)。
仮想志向は好みでなく必須。

### 鍵:Provider ごとに「忠実度」を3層で選ぶ
全インフラの完全再現は不要。検証したいものによって必要な忠実度が違う。

- **層A:フェイク Provider(収束ロジック検証)=開発の主戦場**
  収束ループ・DAG・prune・爆発半径・原因タグ・in-flight排他の検証に**本物は要らない**。インメモリの
  ニセ Provider で十分、むしろ良い:障害注入が自由(unknown を返す/N回に1回失敗/converging継続)、
  高速(秒)、決定的(CI可)。**`Executor`/`Provider` interface に挿すだけ**で収束エンジン本体を
  実インフラなしで完全テストできる——seam設計の副産物。**開発の8割はここ**。
- **層B:軽量な本物(Provider実装検証)**
  Provider が本物のAPIを正しく叩くかの検証。DNS(CoreDNS)/コンテナ(Podman)/FW(nftables)/
  Linux系(systemd/パッケージ/設定)は VM or コンテナ1台で足りる。最初のProvider=DNSが軽いのは
  検証上も好都合。
- **層C:重い本物のミニチュア(統合検証)**
  ネストで縮小再現:ハイパーバイザー=**Proxmoxネスト**(段0と一貫)、NW機器=**containerlab**
  (cEOS/SR Linux/FRR)や VyOS、ストレージ=MinIO(S3)/targetcli(iSCSI)/NFS、Vault=そのまま。
  本物のPure/NetAppは無理だがプロトコルレベル(S3/iSCSI/NFS)は再現できる。

### 箱庭データセンター(ネストで段0〜段2を1台に)
```
物理ホスト1台(メモリ多め 64〜128GB)
  └─ Proxmox / KVM
       ├─ VM: 収束デーモン(開発中の本体)
       ├─ VM: Vault(段0再現)
       ├─ nested: Proxmox(ハイパーバイザーProvider検証)
       ├─ containerlab: cEOS/SR Linux/FRR(NW機器Provider検証)
       ├─ VM: Linux群(DNS/FW/パッケージ/設定)
       ├─ container: MinIO/targetcli(ストレージ)
       └─ git は外部(GitHub)or ローカル Gitea
```
実機ゼロ、メモリだけ食う。**箱庭自体をこのシステムで構築**(ドッグフーディング)し、壊れたら
収束で作り直す使い捨てにする。

### ワークフロー(速い順)
1. **ユニット+フェイクProvider**(秒):収束ロジック/DAG/prune/爆発半径。毎コミットCI。主戦場。
2. **単一Provider統合**(分):軽い本物1つ。Provider実装の正しさ。
3. **箱庭フルスタックE2E**(十分〜時間):VM払い出し→DNS登録→FW設定を通す。リリース前。
4. **カオステスト(意図的破壊)**:Providerをわざと落とす/drift注入/大量削除トリガ →
   **爆発半径制御・alert-only縮退・振動検知・unknown≠absent が効くか**を検証。**このシステムの肝は
   「壊れそうで止まる」ことなので、壊す検証が最重要**。正常系はフェイクで足り、危険な収束は箱庭で本物相当に。

### 始め方(段階的)
- ノートPCの Goプロセス + フェイクProvider だけで**数ヶ月戦える**(追加インフラゼロ、コアは全部書ける)。
- 次に Podman + CoreDNS を1台(最初の本物Provider)。
- メモリ多めの1台に Proxmox + containerlab は、Provider を実インフラ種別へ広げてから。

### 最終像:containerlab によるデータセンターのデジタルツイン
最終的には、**本番の理想状態(git)を同じ収束エンジンで containerlab ベースの箱庭に流し込み、
本番とツインを同一宣言から構築する**。検証環境が本番と構造的に一致する(同じ desired → 同じ収束経路)。
価値:
- **変更の事前検証**:本番に流す前にツインへ同じ理想状態変更を適用し、爆発半径・カスケード・
  converging・順序付き型の置換を安全に観察(前に作った原因タグ D=カスケードの波及可視化がここで効く)。
- **プロバイダ更新の検証**:C(ツール更新由来の幽霊diff)をツインで先に観測してから本番解禁。
- **DR/ローリングのリハーサル**:Workflow(rolling/DRフェイルオーバー)をツインで通し、invariant と
  承認ゲートの挙動を確認してから本番。
- **カオスの常設先**:意図的破壊はツインで恒常的に回し、安全機構の退行を検知。

containerlab は NW中心だが、compute(コンテナ/nested VM)・ストレージ(MinIO等)を同じトポロジ定義に
同居させれば、NW+compute+storage を含む DC 全体のツインになる。**「同じ収束エンジンで本番とツインを
建てる」**のが核心で、ツインの忠実度はProviderの忠実度(層B/C)に比例して上げていける。

---

## 合成リソース(複数の下位を1つに見せる)

IaaS/PaaS のセルフサービスでは「VXLAN設定 + VDS設定 → ユーザには仮想ネットワーク1つ」のように
**複数の下位リソースを束ねて1つの上位リソースに見せる**必要がある。これは新概念でなく、image→VM /
cluster→host / 共有基盤2層と**同じ抽象の再来**。カタログの各商品がこの合成リソースとして実現される。

### 束ね方は3方式(粒度と原子性のトレードオフ)
| 方式 | 正はどっち | 下位drift個別検知 | 原子性 | 使いどころ |
|---|---|---|---|---|
| **1 合成(Expand)** | 上位 | できる(下位もResource) | 弱い(下位別々Apply) | 大半のIaaS抽象。**推奨** |
| 2 集約ビュー | 下位 | できる | — | 既存下位を後から束ねる表示 |
| 3 Provider隠蔽 | 上位=下位一体 | **できない** | 強い(まとめてApply) | 下位が常に一心同体な時 |

### 方式1(合成/Expand)が本命
ユーザは上位1つの理想状態を書き、システムの `Expand()` が下位リソース群を生成する。
```yaml
resource: virtual_network        # ユーザが書く(見せる面)
tenant: acme
id: app-net
cidr: 10.50.0.0/24
# ↓ システムが展開(内部)
#   → vxlan_segment{ vni: 5010, ... }
#   → vds_portgroup{ vlan: ..., uplink: ... }
#   → dhcp_scope{ ... }
```
k8s の Composite Resource(Crossplane XR)/ Operator に相当。**VXLAN+VDS→仮想ネットワークは方式1が
本命**:両者は異種システム(NSX/物理NW と vCenter)を叩くので、1 Provider に押し込む(方式3)と
部分失敗が地獄(VXLANは通ったがVDS失敗をどう戻す)。別Resourceとして依存グラフに乗せるのが正しい。

### 既存設計が全部効く
- **展開=扇状DAG**:`virtual_network → {vxlan, vds, dhcp}`。モデルA・波・依存gate そのまま。
- **上位削除→下位巻き取り**:下位は上位に所有される。参照カウント/所有権/pruneable で上位削除時に下位も消す。
- **部分失敗=失敗伝播**:VXLAN成功/VDS失敗 → 上位「部分converged」。既存の失敗伝播。
- **原子性が要れば**:必ず一緒に変えたい下位群は「1 wave で原子的に」指定(順序付きコレクションの
  "リスト全体が原子単位"の応用)。

### 足す設計は1つ:上位ステータスの集約規則
上位の収束ステータスを下位から畳む。**最も悲観的な下位が上位を支配**
(`unknown > oscillating > drifted > converging > converged` の優先度)= fail-safe(unknown≠absent の再適用)。
どの下位が原因かは追える。これだけ足せば合成は閉じる。

---

## スケール(単一デーモンの天井とスケールアウト)

### 単一デーモンの天井 ≒ Observe 時間
「毎周・全リソースObserve」がモデルAの前提。ボトルネックは効く順に:
1. **Observe総時間(最初に効く)**:1万リソース × 200ms / 並列度16 ≈ 125秒/周、10万で ≈ 20分/周。
   間隔30sを狙うなら**数千リソースで破綻し始める**。並列度を上げると相手API(vCenter/スイッチ)が
   rate limit で音を上げる=無限には上げられない。**実用上限は数千〜1万リソース**(小〜中DC1つなら十分)。
2. メモリ:全理想+全Observationインメモリでも10万で数百MB〜GB=**まだ余裕。天井でない**。
3. DAG計算・Applyスループット:DAGは10万でもミリ秒〜秒、Applyは非同期化済み=**天井でない**。

→ 天井はほぼ **Observe 時間一択**。「毎周全部見る」というstateレスの美点がそのまま首を絞める。

### 分散の前に単一を延ばす(先に使う)
- **Observe間引き(ついに解禁)**:安定リソースはN周に1回(drift検知が数周遅れるが許容)。当初
  「破綻したら考える」とした破綻がここ。
- **watch併用ハイブリッド**:provider が watch/通知を持つならそれで drift 即知(k8s informer 的)。
  全ポーリングを watch + 保険の低頻度ポーリングに。level の安全性を保ちつつ Observe 負荷を落とす。
- **tier別頻度**:`tier`(基盤/通常)や「最近drift/安定」で Observe 頻度帯を分ける。
- これらで**中規模(数万)まで単一で届きうる**。分散は最後の手段。

### スケールアウト = シャーディング(分割軸が重要)
| 軸 | 内容 | スケールするか |
|---|---|---|
| **1 シャーディング** | リソースを tenant/site/cluster で水平分割、各デーモンが担当分だけ収束 | **する(推奨)** |
| 2 リーダー選出 active/standby | 段0 HAの形 | **しない**(可用性用。activeは1台) |
| 3 機能分割(Observe係/Apply係) | 役割で割る | しない(過剰) |

- **分割キーは既存の `scope` / `tenant` / `ownership` がそのまま使える**。「テナントA→デーモン1」。
- **各シャードは shared-nothing で独立収束**(デーモン間協調不要=**分散合意を中枢に戻さない**。
  最初にetcdを捨てた原則を死守)。発想は k8s でなく Nomad / Prometheus functional sharding。

### シャーディングの難所:境界をまたぐ共有基盤(既存2層所有権の投影)
専有リソースはシャードに綺麗に割れるが、**共有基盤(共有クラスタ/NWコア/共有ストレージ)は全シャードが
参照**する=どのシャードに属すか問題。解決は前の2層所有権モデルの物理投影:
- **共有基盤は専用の platform シャードが1つ所有・変更**。テナントシャードは **read-onlyで参照**(容量
  充足を観測するが変えない)。書き手が1つなら綺麗。
- 前の「共有基盤の宣言は platform層に一度だけ」の分散版。**所有権の論理境界がそのままシャードの物理境界**。

### 上位の「操作するもの」= 司令塔でなくルーター
- **要る(軽い)**:シャードルーター/レジストリ。「どのリソース/テナントがどのデーモンか」を引く薄い層
  (状態=割当表のみ)。理想状態投入時(DesiredSource前段)に正しいシャードへ振り分ける。
- **要らない(重い)**:全シャードを統べるメタ収束器。各シャード独立収束なので全体協調は不要
  (**k8s federation が重くて廃れた轍を踏まない**)。上位は「振り分け」だけ、「協調」はしない。

### 天井の全体像
| 規模 | 構成 |
|---|---|
| 〜数千 | 単一デーモン・毎周全Observe(今の設計そのまま。ホームラボ〜小DC) |
| 数千〜数万 | 単一 + Observe間引き/watch併用/tier別頻度(中規模。分散前にここで延ばす) |
| 数万〜 | シャーディング(tenant/site/cluster が分割キー) |
| 地理分散 | サイトごとシャード + 薄いルーター(各サイト独立収束、DR時も生存) |

**核心**:スケールアウトの設計が、既に作ったマルチテナント2層所有権モデルの物理投影で済む。論理で
platform共有/tenant専有を分けたおかげで、それをそのままデーモンの分割境界にできる=分散のための
新概念がほぼ不要。seam戦略(境界は先に切る)がスケール局面でも効いている。

---

## 外部 CMDB / IPAM 連携と allocation(払い出し)

NetBox 等を前提にせず、**「外部の記録・払い出し権威(CMDB/IPAM authority)」**として抽象化する。
NetBox はその一実装(他に Infoblox、phpIPAM、自作 DB 等)。`DesiredSource` / `SecretBackend` と
同じく、`AllocationAuthority` interface で seam を切り、実装を差し替え可能にする。

### 払い出しは inventory でも intent でもない第3カテゴリ = allocation
前の「CMDB = inventory(事実) / ファイル = intent(構成)」の分割は**読み取り前提**だった。
「VLAN ID / IP / ASN をプールから払い出す」はこれを壊す:
- 事実の記録でない(まだ割り当てられていないものを新規割り当て)
- intent でない(ユーザは「VLAN 573 が欲しい」と言わず「仮想ネットワークが欲しい」と言う。573 は
  システムが決める)
- **副作用を伴う書き込み**(プールから1つ消費=在庫が減る)

→ allocation は第3カテゴリ。しかも**素朴だと冪等でない**(毎周同じ intent を見て毎回新しいIDを払い出し、
プールを食い潰す)。「プールから1つ取る」は経路依存の操作で宣言的状態でない(migration/採番の再来)。

### 解決:allocation を「要求(冪等)」と「割り当ての事実(state)」に割る
- **要求**(intent・冪等):ユーザ/上位は「VLAN が1つ必要」と書く。具体IDは指定しない。
  ```yaml
  resource: virtual_network
  id: app-net
  vlan: { from_pool: tenant-acme-vlans }   # プールから1つ要求(ID指定なし)
  ```
- **割り当ての事実**(state):`app-net → VLAN 573` を**外部 authority が永続化**。次サイクルで
  「app-net は既に 573」と分かり二重払い出しを防ぐ。**authority が"前回何を払い出したか"の記憶装置**
  =冪等性が回復する。allocation state は持たざるを得ない状態で、その置き場が外部 authority
  (収束エンジン自身は持たない=stateレス原則を保つ)。

収束は「要求」を見て authority に「割り当て済みか」を問い、未割り当てなら払い出して記録、割り当て済みなら
その値を使う。

### 外部 authority の3役
| 役割 | 何を | 読み書き | 収束との関係 |
|---|---|---|---|
| **1 inventory** | 既にある事実(ラック/機器) | 主に read | intent と合成(render) |
| **2 allocation authority** | プール(VLAN/IP/ASN)と払い出し記録 | **read-write** | 冪等性の記憶・払い出しの権威 |
| **3 reality(source of record)** | 収束結果の逆流(実在するもの) | 収束が write | 監査・可視化 |

### authority を採番の単一権威にする(競合とシャードの調停)
allocation が read-write になると並行払い出しの競合が出る(2つが同時に同じIDを取る)。
- **払い出しは必ず authority の API 経由**(IPAM は「次の空き」をアトミックに払い出す機能を持つ)。
  収束エンジンは自前プール管理をせず「1つくれ」と頼む=**authority が採番の権威**。
- これで**シャーディングと整合**:複数デーモンが並行しても採番は authority に一元化=競合しない。
  **分散合意を中枢に戻さず、採番調停だけ外部 authority に外出し**(綺麗な役割分担)。

### render が2フェーズ化(「render は決定的」の修正)
```
フェーズ1:allocation 解決 … intent「VLAN 1つ要る」→ authority に問う → あれば既存値、なければ払い出し
                            (★副作用あり。authority が「同じ要求に同じ答え」を保証して全体を冪等化)
フェーズ2:desired 生成    … 573 で下位を展開(vxlan{573}, vds_portgroup{573})→ 決定的 → 収束
```
前の「render は決定的であるべき」を、**allocation フェーズ(副作用・authority が冪等担保)+ 生成
フェーズ(決定的)**に分割して修正。

### レビューゲートとの分担 + 仮予約方式(reserve → confirm)
前の案A(intent変更→PR→人が承認)と、allocation の書き込みは緊張する。書き込みの種類で分ける:
- **intent の変更**(仮想ネットワークを追加)= レビュー対象。人が承認。
- **具体的な番号割り当て**はシステムが自動(人は「サブネットが要る」と決め、具体IPは自動採番)。

ただし **platform層(PR必須)で PR 生成時に本払い出しすると、却下時にリークする**。→ **仮予約方式**:
```
PR 生成時   : authority に TTL 付き reserve(仮予約)→ PR diff に具体 ID が出る(何が使われるか人が見える)
マージ時    : confirm(本払い出しに昇格)。★confirm はマージ後**最初の render フェーズ1**が実行する
却下/TTL切れ: 自動的に解放(リークしない)
```
代償は authority に予約状態が増えることだが、**PR diff の具体性**が勝る。
**テナント層(即時)は仮予約不要**で直接払い出し。

### Release は非対称原則に従う(「確信が持てないとき減算しない」)
素朴な「所有者が desired から消えたら返却」は**危険**:render 入力が stale(CMDB断で古い生成物、
理想の一時的な読み損ね)→ owner 消滅と誤認 → Release → **使用中の VLAN/IP が別リソースに
再払い出し=衝突**(ネットワークでは最悪級の事故)。→ 3点セット:
- **(a) desired からの消滅では Release を発火させない**。**下流リソースの削除完了を Observe で確認
  してから**実行(「理想から消えた」でなく「**現実から消えた**」を根拠にする)。
- **(b) 返却後に cooldown / 検疫 TTL**(即時再払い出しをしない。DHCP のリース再利用猶予と同型)。
- **(c) `pruneable:false` 等で下位が残存する間は Release 保留**。

### reality 逆流(役割3)
収束が実際に作った実在の事実を authority に書き戻すか(authority を "実際にあるもの" の source of
record にする)。するなら **reality 欄は収束エンジンが唯一の書き手**(人は触らない)、allocation 欄
(人/システムが払い出す)と分離。二重管理を避ける。

### 抽象化(NetBox非依存)
```go
type AllocationAuthority interface {
    Allocate(ctx, pool string, owner string) (value string, err error)  // 冪等:同owner→同value
    Reserve(ctx, pool string, owner string, ttl time.Duration) (value string, err error) // PR用仮予約
    Confirm(ctx, pool string, owner string) error                        // マージ後 render が昇格
    Release(ctx, pool string, owner string) error  // ★下流削除を Observe 確認後のみ。cooldown あり
    Lookup(ctx, pool string, owner string) (value string, ok bool, err error)
}
```
NetBox / Infoblox / phpIPAM / 自作DB を実装として差し替え。v1はプールが要るまで不要、必要になったら
1実装(NetBox でも SQLite でも)を挿す。**核心は「払い出しを収束の中でやらず、外部 authority に
権威を置き、収束は"要求して結果を使う"に徹する」**——冪等性・競合・分散が同時に解け、既存 IPAM の
枠組みに乗れる(自前採番=車輪の再発明を避ける)。

---

## 書き込みポリシー・intent UI・allocation authority(結論)

### 書き込みポリシーは ownership 層で分岐する(レビュー要否 = 所有権)
「レビューが要るか」は UI でも書き込み経路でもなく、**そのリソースが誰の所有物か(既存の
`ownership` 2層)**で決まる。同じ層区別が5つの関心事を貫通する:

| 層 | ownership | 書き込み経路 | remediation | rate limit | 爆発半径 |
|---|---|---|---|---|---|
| テナント | tenant | **即時(レビュー無)** | auto でよい | 緩め(warn) | 小(砂場内) |
| プラットフォーム | platform | **PR必須(人が承認)** | alert-only 寄り | 厳(halt) | 大(全体) |

`ownership` 一本で所有・書き込み・修復モード・rate limit・爆発半径が揃う=設計が正しく分解できている証拠。

- **テナント層がレビュー不要な理由**:scope で砂場境界を切ってあり爆発半径がテナント内に閉じる。
  他テナントも基盤も壊せない。**テナント内でレビューが要るかはテナントの自己責任**(必要ならゲスト側が
  自前でCI等を用意すればよい)。プラットフォームは砂場の境界だけ保証し、砂場内の統治は委ねる
  (パブリッククラウドの思想)。
- **プラットフォーム層がPR必須な理由**:基盤変更は全テナントに波及=爆発半径最大。前の「共有基盤の
  変更権限はプラットフォーム管理者」と一致。

**属性化**:`write_policy: direct | review_required` を ownership からデフォルト導出(tenant→direct、
platform→review_required)。大半は書かなくてよく、例外だけ明示上書き。

**テナントが基盤に触れる操作(採番消費・容量消費)**は、authority(採番)と quota(容量)という
**事前に区切られた枠**を通すことで即時のまま安全:枠内はテナントが即時実行、枠を超える要求だけ
プラットフォーム層の承認に上がる。allocation authority と quota が「テナント即時」と「基盤保護」の緩衝材。

### intent UI は独立CMDBにしない(git が正、UI は薄いフロントエンド)
intent は既に YAML(ファイル正・git)で表現済み。UI に独自データモデル/DBを持たせない。
- **UI は書き込み経路を ownership に従わせる**:テナントのカタログ操作(「仮想ネットワーク追加」)→
  即時反映(PRなし)。管理者の基盤設定変更 → PR起票して承認。**同じUIでも触るリソースの層で裏の経路が
  自動で変わる**。
- 難所は UI でなく **round-trip(UIとテキスト手編集の2経路調停・コメント/整形保持)**。
  → プラットフォーム層は **UI→PR生成**(gitを単一の書き込み経路にして往復問題を回避、前のレビュー
  ゲート再利用)。テナント層は即時なので砂場内で完結(手編集との競合が起きにくい)。
- **カタログUIの各商品 = 合成リソースのテンプレート**(vxlan+vds を束ねた「仮想ネットワーク」等)。
  UI → 合成リソース → allocation → git/収束 が一本に繋がり、UI は薄いフロントエンドに留まる
  (下の仕組みが厚いから薄くできる)。

### allocation authority は小さく自作(NetBox は不要に)
上記により、NetBox のような独立CMDB全体は不要。inventory は当面ファイル、reality逆流は将来、
残るのは **allocation(採番払い出し)だけ**。ここは状態を持つので自作の価値があり(intentは
git が正だからUIは薄くてよい=**状態を持つ部分と持たない部分で自作の意味が違う**)、勉強がてら
最小実装を自作する。
- **`AllocationAuthority` interface の最小実装**(3メソッド:Allocate/Release/Lookup)。
  SQLite 1ファイル + **アトミック払い出し**(`BEGIN IMMEDIATE` 内で空きを探して即マーク=並行制御の
  学習価値の核)+ フラットプールから開始(階層は必要になったら)。NetBox の巨大さ(DCIM・正規化
  データモデル・UI等)は要らない。
- **守る2規律**:(1) interface の裏に隠して差し替え可能に保つ(力尽きたら NetBox/Infoblox に戻せる=
  seam死守)。(2) スコープを IPAM 払い出しに限定し、CMDB全体に育てない(inventory/reality を1つの
  自作物に背負わせない=役割を混ぜない)。
- **順序**:必要になった時点で最小版を自作。当面 v1 は不要(プールが要るまで)。

---

## 認証・機器アクセス(別ソフト = credential broker として括り出す)

BMC/NW装置/ハイパーバイザー等、多数のターゲットへの認証を統一的に扱う。ただし**これは
reconciliation engine とは関心が別**で、独立ソフト(credential broker)として分離する。

### 問題は3層(secret storage は解決済み)
1. **secret storage(値の保管)** … Vault で解決済み(SecretRef・遅延展開・世代)。
2. **identity(誰が)** … マシン(収束デーモン)/ 人(個人ごと、監査のため)。
3. **target access(何にどう認証)** … 各機器へのログイン資格。共通アカウントを使い回さない。

### 核心原則:静的資格情報を配るな、動的に発行しろ
共通アカウント使い回しの根本解決は**長命資格情報を保存・配布しないこと**。使う瞬間に短命資格を
発行し使用後失効(Vault 動的シークレット/JIT)。**保存しないものは漏れない・rotation不要・発行ログが
監査**。現実には2層:
- **層A(動的発行対応)**:短命資格をJIT発行(SSH署名証明書/PKI/一部API)。保存ゼロ。
- **層B(レガシー機器=静的のみ)**:古いBMC/NW機器。Vault保存(SecretRef)+ 定期rotate で緩和。
- 統一 interface の裏で自動振り分け(`SecretRef` は層Bの実装)。

### identity は2入口、機器側アカウントと分離
- **マシン**:収束デーモンが Vault に自分を証明(段0のトークン注入が起点、AppRole/TPM等で強化)。
  デーモンの操作は全てマシンアイデンティティ経由で記録。
- **ヒューマン**:IdP(SSO)で個人認証 → その身元で短命資格を発行。**bastion 経由**でアクセス経路を
  1本に絞りセッション記録。
- **機器側アカウントとシステム側アイデンティティを分離**:機器に個人アカウントを永続作成せず使い捨て、
  身元は IdP/Vault が保持、**発行ログが「本当は誰が」を記録**。→ 機器側は共通/使い捨てでも監査は個人単位
  (要求の両立)。全機器に全員のアカウントを作る地獄を回避。

### 段階性は remediation mode に内包(仕組みを変えず移行)
「最終的にデーモン中心・人は緊急のみ、当面は段階的」は、既存の alert-only→auto の移行そのもの:
- 段階1:人が主。デーモンは Observe + drift検知(alert-only)。人が短命資格で入って直す。
- 段階3:デーモンが全 apply(auto)。人は緊急のみ。
- **仕組みは不変、`CredentialProvider` の発行先分布が人→デーモンへ移るだけ**。まず作るべきは
  **人向け短命発行(bastion + JIT)**——段階1で実利があり個人監査の要求が即満たされる。

### なぜ別ソフト(credential broker)にするか — 3根拠
1. **ライフサイクルが違う**:engine=常時収束ループ、認証=要求応答型。混ぜると片方の障害が波及。
2. **可用性が逆立ち(決定的)**:認証は engine の**依存先(下位)**。engine が死んでも認証が生きて
   いないと**緊急時に人が機器に入れない**。engine 内に認証を置くと致命的な循環依存。Vault を独立
   させたのと同じ理由。
3. **セキュリティ境界**:認証は最大の攻撃対象。広い攻撃面(Provider群/Ansible連携)の engine と
   同居させず、小さく監査可能な別プロセスに隔離。

### 分割線 = 既存の CredentialProvider interface
```
Reconciliation Engine ──[CredentialProvider interface = seam]──▶ Credential Broker(別ソフト)
  「target に identity で                                          Vault連携(層A/B) / IdP連携 /
   アクセスする資格が欲しい」と要求                                bastion / 短命発行 / TTL管理 /
                                                                  発行の監査 / Break Glass 検出・rotate
```
engine は要求するだけ、broker が発行して返す。**先に切った `CredentialProvider` seam がそのまま
プロセス境界**になる(seam戦略の配当)。broker は内部で Vault/IdP に委譲するので薄い。**過剰分割
しない**:Vault連携/IdP/bastion/Break Glass を別々のソフトにせず、broker 1本に括る。

### broker は engine の依存先かつ管理対象(段0の鶏卵と同型)
broker は engine から独立して動く(依存先)が、同時に engine の収束管理対象(1サービス Resource)
にもなる。実行時依存 × 管理関係の鶏卵は段0で解決:**broker の素の起動は段0に置き**(Vault と同じ
レイヤ)、engine は段1以降で broker を管理する。broker は engine 以外(CI・監視・他自動化)からも
使える汎用性があり、独立プロダクトとして立つ。

### Break Glass(真の緊急時)= 別レイヤの必要悪
通常認証(Vault/IdP/短命発行)が**全滅した状況**でも機器に入れる最終手段。ここで矛盾:Break Glass は
認証基盤に依存できない=**我々が消した静的共通資格を意図的に1つ残す**必要悪。ゼロにできない。
封じ込め原則(「使えるが、使ったら必ずバレる」):
- **オフライン物理保管**(Vault/デーモンに置かない=それらが死んだとき用)。**段0 unseal key と同じ
  金庫・同じ性質=最後の人間の鍵**。
- **使用が必ず検知**(封筒の改竄シール/取り出しアラート)。
- **使い捨て・使用後 rotate**。
- 最重要のものは**分割管理**(Shamir、Vault unseal と同型。単独犯行/ミスを防ぐ)。
- **事前チェック最小・事後監査最大**(緊急時は速度優先、抑止は確実な発覚で担保)。

階層(何が死んだか):
| L1 | 収束デーモンのみ死 | 通常の bastion+短命発行で対応(Vault/IdP生存=Break Glassではない) |
| L2 | Vault 死 | オフライン保管の機器資格(rotate済み最新を金庫に退避) |
| L3 | 段0全滅 | 完全オフライン Break Glass + 段0再構築(Ansible playbook + unseal key) |
**L3 は段0ブートストラップと統合**(段0を人手再建する能力がそのまま最終 Break Glass)。

### Break Glass のシステム化(検出・rotate)
- **検出**:取り出し/開封イベントを既存の **Event → sink** に `BreakGlassUsed`(最高severity)で流す。
  新規の仕組み不要。
- **rotate**:**自動rotateは危険**(緊急対応中に資格が変わると対応者を締め出す)→ **事態収束後に人が
  明示キック**。中身は既存の rotation Workflow(新資格生成→機器設定→旧無効化→金庫へ)、トリガを
  「手動」にするだけ。キック後「新しい Break Glass を金庫へ」を人に促す。
- **分担**:保管はオフライン(システム外)、更新・rotate はシステム。engine が死んでも使える性質を
  保ちつつ、通常時はシステムが鮮度を保つ。

### あなたのシステムの役割
認証基盤(Vault/IdP)は作らず、それを**全ターゲットに統一的に使う credential broker**を engine とは
別ソフトとして持つ。engine 側に残るのは `CredentialProvider` 経由の要求と、操作を「どの身元で」実行
したかの帰属記録のみ。

---

## 理想状態の供給源は選択制(DesiredSource の実装違い)

「YAML ファイルである必要はなく DB でもよい、選択できるとよい」は、既に切ってある
`DesiredSource` interface の実装違い。`FileSource` の隣に `DBSource` を置くだけ=**選択制は seam
として予約済み・コア無変更**。

### ただし混同注意:ファイルには2役あった
理想状態を「ファイル正」にしたとき、ファイルは2つを担っていた:(1) 保存、(2) **git がくれる"正の
性質"**=履歴・PRレビュー・`git diff`・単一書き込み経路。DB にすると (1) は自明に代替できるが、(2)
を**どう確保するか込みで設計しないと穴が開く**(原因タグA=commit / レビューゲート / 監査の半分 /
爆発半径の「1コミットで全削除」をPRで止める、の土台が (2) に依存)。

### 選択制が正しい理由 = ownership 層の書き込みポリシーと一致
用途により必要な"正の性質"が違い、それは既に決めた ownership 分岐と重なる:
| 実装 | 保存 | 履歴/監査 | レビュー | 書込頻度 | 用途(ownership) |
|---|---|---|---|---|---|
| **FileSource(git)** | ファイル | git commit連鎖 | PR | 低(人) | プラットフォーム層 |
| **DBSource** | DB | append-onlyログ | 無(即時) | 高(API) | テナント層・払い出し |
| **RenderedSource** | 外部CMDB+intent | 元ソース依存 | 案A(render→PR) | 中 | inventory合成 |

全部**同じ `DesiredSource` interface の裏**。engine コアは供給源を知らない。「選べるとよい」は正確には
**層ごとに供給源を選ぶ**——プラットフォーム層=git(履歴/PRが要る)、テナント層=DB(即時/高頻度/
クエリ性)。またしても ownership が貫く。

### DB を選んだとき git の性質をどう補うか
- **捨ててよい**:PRレビュー(テナント層は即時)、人間可読diff(APIが書く)。
- **別途要る**:履歴・監査 → **append-only変更ログ**(前に監査ログで設計した「append-only+連鎖
  ハッシュ」の再利用)。
- **revision**:`DesiredSource.Fetch()` が返す revision は、git なら commit hash、DB なら
  トランザクションID/世代番号。revision の抽象は供給源に依らず成立するよう既に切ってある。

### 混在の境界
1システム内で File正 と DB正 が混在しうる(プラットフォーム=git、テナント=DB)。境界:
- **同一リソースを2供給源が持たない**(ownership が供給源の所属も決める)。
- engine から見れば全部マージされた1つの理想状態。供給源が違っても収束ループは区別しない
  (revision で各々の世代を追うだけ)。

**洞察**:「YAMLファイルか DB か」は保存形式でなく「その理想状態にどんな正の性質(履歴・レビュー・
書込頻度)が要るか」の選択で、それは ownership 層の区別と一致する。seam(DesiredSource)を先に
切ってあるのでコア無変更で吸収。

---

# render パイプライン(第一級コンポーネント)

> desired 生成は ①inventory合成 ②シークレット世代解決 ③allocation解決(副作用あり)
> ④合成リソースExpand の4処理を経る「複数の外部権威に依存し、副作用を持ち、毎周走る多段パイプライン」
> であり、**収束ループと同格の複雑さを持つ**。専用の節・統一形・失敗セマンティクスをここに集約する。

## 統一形:2フェーズ
```
render = フェーズ1(外部入力解決:副作用あり・外部権威に依存)
           ├ inventory 合成(外部CMDB)
           ├ シークレット世代解決(Vault metadata:値は読まない)
           └ allocation 解決(AllocationAuthority への払い出し=副作用)
       + フェーズ2(決定的展開:副作用なし・同じ入力→同じ出力)
           └ 合成リソースの Expand(virtual_network → vxlan + vds + dhcp)
```
allocation 節の「2フェーズ化」はこの一般形の**特殊例**として吸収される。
**Expand の居場所を確定**:収束ループ内の入れ子コントローラではなく **render フェーズ2**。
決定性の要求(同じ入力→同じ下位群)は render フェーズ2の規律として一括で課す。

### 外部入力の障害時挙動を一箇所で統一(unknown ≠ absent の render 版)
**どの外部権威が不達でも「前回解決値を保持」**。空の desired を生成しない(全削除事故の防止)。
この安全弁は従来 CMDB にしか書いていなかったが、**Vault metadata・allocation authority にも同様に適用**:

| 外部入力 | 不達時 | 備考 |
|---|---|---|
| inventory(CMDB) | 前回生成物を保持 | 既定義 |
| シークレット世代(Vault metadata) | **前回世代を保持** | ★新規。Vault sealed-restart 中は世代解決も死ぬ |
| allocation authority | **前回割当を保持**(新規払い出しは保留) | ★新規。未割当リソースは blocked で次周回収 |

→ 初回レビュー項目6の緩和(SecretRef キャッシュ)は、**値だけでなく世代 metadata のキャッシュも含む**。

### 平常時の頻度規律(TTL キャッシュ)
render が毎周実行になった結果、Vault metadata 読み(シークレット数ぶん)と authority Lookup
(割当リソース数ぶん)が**毎サイクルの外部呼び出し**になる。Observe には semaphore / backoff の規律が
あるが render 入力には無かった。→ **render 入力は TTL 付きキャッシュ**(staleness 上限 = TTL)。
不達時は上表の通り前回値保持。障害時挙動は定義済みで、**平常時の頻度規律がこれで揃う**。

### intent revision と rendered revision を分ける(型)
```
intent_revision   … 人が書いた理想(git commit hash / DB トランザクションID)
rendered_revision … render 出力の世代(intent + 外部入力の解決結果すべてのハッシュ)
```
これが次の「原因タグ E」の判定基盤になる。`DesiredSource.Fetch()` が返す revision は
intent_revision、収束が比較するのは rendered_revision。

---

## 原因タグ・allocation の Release(本文に統合済み)

原因タグ5分類(A意図 / B drift / C ツール更新 / D カスケード / **E 外部入力変化**)と
intent_revision / rendered_revision による判定は、**「変更の『原因』区別(5分類)」節を正**とする。
allocation の**仮予約方式(reserve→confirm)**と**非対称 Release**(下流削除の Observe 確認後・
cooldown・pruneable残存中は保留)は、**「外部 CMDB / IPAM 連携と allocation」節を正**とする。

---

## デジタルツイン = 「render の外部入力を差し替えた環境」

**問題**:同じ desired をツインに流すと、render が**本番の authority と Vault を叩く**——
ツイン検証が**本番プールを消費**し、**本番シークレットがツインに展開**される事故。

**修正**:ツインを「**render の外部入力を差し替えた環境**」と定義する(render を第一級化したので
「入力の差し替え点」として自然に表現できる)。
- **ツイン専用の AllocationAuthority インスタンス**(本番スナップショットから seed すれば、具体 ID の
  忠実度も確保できる)。
- **専用/フェイクの SecretBackend**(本番 Vault を叩かない)。
- inventory も同様にツイン用スナップショットを入力に。
- → 同じ intent・同じ engine・**差し替えた外部入力**でツインが建つ。本番プールもシークレットも汚さない。

---

## シャード間の依存セマンティクスの弱化

単一デーモンでは DAG が **plan レベル**で依存順序を保証するが、**shared-nothing シャード間には依存
gate が存在しない**。テナントシャードは platform リソースの状態を **Observe(現実)経由でしか知れない**。

→ **意味論の変化として明記**:シャード内は「plan 順序による gating」、**シャードをまたぐ依存は
「観測 ready 待ち」に弱化**する(結果整合)。規則:**platform リソースの readiness を、テナント
シャードが Observe で gate する**(「共有クラスタが ready と観測できるまで、その上の VM を払い出さない」)。
実用上は結果整合で足りるが、強い順序保証は**シャード内に閉じたときのみ**成立する、と理解する。

---

## 境界定義・補足規則

### SecretRef と CredentialProvider の境界(未定義だった)
- **`SecretRef`** = リソースに**注入する設定値**(例:アプリに渡す DB パスワード、機器に設定する
  SNMP community)。**desired の一部**であり、render/Apply 時に解決される。
- **`CredentialProvider`** = **engine 自身がターゲットに到達するための資格**(例:スイッチにログイン
  するための短命アカウント、vCenter API トークン)。**desired の一部ではなく実行時の資格**。
  credential broker(別ソフト)が発行する。
- → **provider が機器にログインする資格は CredentialProvider 経路**。機器に設定する値は SecretRef 経路。

```go
type CredentialProvider interface {   // ← 本文で参照していたが未定義だったので定義
    Acquire(ctx, target Target, identity Identity) (Credential, error)
    Release(ctx, Credential) error
}
```

### credential broker の可用性(Vault と対称に・レビュー重大4)
broker 断は Vault 断より重い:Vault 断は「シークレット依存の Apply が止まる」だけだが、**broker 断は
認証が要る全ターゲットの Observe が止まる=システム全盲**。→ Vault 緩和と対称の3点:
- **(a) 発行済み短命資格を engine 側に TTL 内キャッシュ**(broker 断中も TTL 満了まで Observe 継続)。
- **(b) broker 不達中は該当リソースを `unknown` 扱い**(absent と断じない=既存原則)。
- **(c) broker を段0 の VM-HA 保護対象に**(Vault・収束デーモンと同列。段0構成図に追加)。

### seam の全列挙(更新)
`Executor` / `Loader` / `DesiredStore` / `DesiredSource` / `SecretBackend` /
**`AllocationAuthority`** / **`CredentialProvider`**。

### その他の修正
- **合成リソースのステータス集約に `oscillating` を追加**:
  `unknown > oscillating > drifted > converging > converged`(最も悲観的な下位が上位を支配)。
- **集合 diff の実装位置**:集合 diff(List結果 vs desired集合 → create/delete 判定)は
  **コントローラ側の汎用ロジック**。**provider が実装するのはフィールド diff のみ**
  (`Provider.Diff` の署名からは読み取れないため明記)。
- **シャードルーターの割当表は S2**(進行state・外部永続化必須)。喪失=誤配送で事故になるため、
  S3(インメモリ可)ではない。state 分類学に追加。
- **Observe の戻りと収束ステータスは別**:Observe の戻りは `ok / unknown / converging`。四値
  (`unknown / converging / converged / drifted`)は**導出された報告用ステータス**であって Observe の
  戻りではない(コア抽象 0b を正とする)。
- **シークレットの世代フィールド**:世代は**ユーザが書く参照には現れず**、**render フェーズ1が解決して
  SecretRef に注入するフィールド**(ピン留めではない)。

### 看板
broker・allocation authority・UI という衛星が生えたため、「単一バイナリ・単一デーモン」だけでは
不正確。正確には:
> **収束エンジン core は単一バイナリ・単一デーモン。周辺は小さな衛星群**
> (credential broker / allocation authority / intent UI)。

衛星を分けた理由(**ライフサイクル差・可用性の逆転・攻撃面の分離**)は本文自身の主張であり、
看板をそれに合わせる。**core が小さく保たれている**ことが要点で、衛星が生えたのは責務境界を
正しく引いた結果(k8s の偶有的複雑さを避けた原則は維持されている)。

---

# システム全体構成(全コンポーネントとその関係)

出発点は「k8s の収束ループをシンプルに」だったが、オンプレ運用の広域を引き受けた結果、
**収束エンジン core(小さく保つ)+ 小さな衛星群 + 外部委譲先**からなる総合インフラ管理ツールに
到達した。ここに全コンポーネントと関係を集約する。

## 全体図

```
                          ┌──────────── 人間 ─────────────┐
                          │ 金庫:unseal key / Break Glass  │  ← ブートストラップの底
                          │ Ansible(段0の構築・unseal)     │
                          └────────────┬──────────────────┘
                                       │ 構築(稀・意図的・pull型)
                                       ▼
┌─────────────────────── 段0(人手管理・"作って忘れる")────────────────────────┐
│  Proxmox 3ノード + 統合Ceph + VM-HA                                          │
│    ├ Vault(シークレットの根)                                                │
│    ├ credential broker(衛星)         ← engine の依存先。落ちると全盲       │
│    └ 収束エンジン core(本体)                                                │
└──────────────────────────────────┬──────────────────────────────────────────┘
                                   │ 自律収束(常時・push型・level-triggered)
                                   ▼
┌──────────────────────── 段1以降:本番インフラ全体 ───────────────────────────┐
│  物理(電源/BMC)→ NW機器/FW → ハイパーバイザー/クラスタ → VM → guest OS/ソフト │
│  ストレージ / SDN / コンテナ / 基盤サービス(DNS/DHCP/PXE/NTP)                │
└─────────────────────────────────────────────────────────────────────────────┘
```

## コンポーネント一覧

### A. 収束エンジン core(単一バイナリ・単一デーモン。本設計の中心)

| 内部要素 | 役割 |
|---|---|
| **収束ループ** | level-triggered。毎周 Observe → Diff → Apply。観測stateレス |
| **DAG / wave** | 依存解決。モデルA(毎周全DAG)、波単位並列、失敗は下流skip+次周回収 |
| **render パイプライン** | フェーズ1(外部入力解決:副作用あり)+ フェーズ2(決定的展開:Expand) |
| **3プリミティブ** | Resource(収束) / Task(1リソースの手続き) / Workflow(複数リソース+invariant) |
| **安全機構** | 爆発半径(rate limit / 段階適用 / 承認ゲート)、原因タグ A-E、振動検知、backoff |
| **Event bus** | 全ての意味ある出来事を単一 Event 型に集約 → sink へ |

**core が持つ seam(interface)= 拡張と分離の全接点**:

| seam | 何を差し替えるか | v1実装 | 将来 |
|---|---|---|---|
| `Provider` / `Lister` | 個別操作 / 集合列挙(prune の前提) | 自前(DNS等) | Ansible provider・各ベンダー |
| `Executor` | 実行境界 | in-process | プロセス隔離 / リモート(gRPC) |
| `Loader` | 設定形式 | YAML/JSON | CUE |
| `DesiredSource` | 理想状態の供給源 | FileSource(git) | DBSource / RenderedSource |
| `DesiredStore` | 理想状態の保存 | ファイル+インメモリ | bbolt / SQLite |
| `SecretBackend` | シークレット取得 | Vault | k8s Secret / file |
| `AllocationAuthority` | 採番払い出し | (不要) | 自作最小実装 / NetBox / Infoblox |
| `CredentialProvider` | ターゲット到達資格 | — | credential broker |

### B. 衛星群(core から分離した独立ソフト)

| 衛星 | 役割 | 分離理由 | core との関係 |
|---|---|---|---|
| **credential broker** | 機器認証の統一(層A JIT短命発行 / 層B保存+rotate、IdP連携、bastion、Break Glass 検出・rotate) | ライフサイクル差(要求応答 vs 常時収束)/ **可用性の逆転**(engine が死んでも認証は生きる必要)/ 攻撃面の分離 | **core の依存先**(下位)。同時に core の管理対象(段0に素の起動を置く鶏卵構造) |
| **allocation authority** | プール採番(VLAN/IP/ASN)の権威。冪等な払い出し記録 | 状態を持つ(冪等性の記憶)/ 並行・シャード環境の採番調停を一元化 | core は「1つくれ」と要求するだけ。**分散合意を core に戻さないための外出し** |
| **intent UI** | カタログ/フォーム。薄いフロントエンド | UI にデータモデルを持たせない | 書き込み経路は `ownership` に従う(platform→PR / tenant→即時) |

### C. 外部委譲先(作らない。既存に任せる)

| 委譲先 | 何を委ねたか | core の関与 |
|---|---|---|
| **Vault** | シークレットの値・動的発行・rotation | SecretRef で参照、値は持たない |
| **IdP(SSO)** | ヒューマン ID・認証 | broker 経由。core は認可を持たない |
| **Prometheus / Alertmanager** | 時系列監視・通知ルーティング | **監視"設定"を Resource として収束**(observability as Resource)。判定と通知は委譲 |
| **DRS / ハイパーバイザー** | 配置最適化・ビンパッキング・migrate | core は cluster への intent と粗い容量充足のみ。**scheduler を再発明しない** |
| **上位テナントプラットフォーム** | 認可・quota・課金・ポータル | `DesiredSource` の一実装として理想状態を投入 |
| **git(GitHub/GitLab)** | intent の履歴・レビュー・改竄耐性アンカー | FileSource の実体。監査の半分を担う |
| **k8s 等** | アプリ層のデプロイ・スケール(PaaS の中核) | core は土台(インフラ)を供給 |
| **Ansible(モジュール)** | ロングテール provider の実行実体 | `Executor` の一実装。**モジュールのみ借り実行モデルは借りない** |

### D. 検証環境

| 要素 | 役割 |
|---|---|
| **フェイク Provider** | 収束ロジック検証の主戦場(障害注入自由・決定的・高速) |
| **箱庭 DC** | Proxmoxネスト + containerlab + MinIO 等。段0〜段2を1台に |
| **デジタルツイン** | **render の外部入力を差し替えた環境**(専用 authority / SecretBackend)。本番プールもシークレットも汚さず、変更・provider更新・DR/ローリングを事前検証 |

## 依存の向き(重要)

```
人間の金庫 ─(最後の鍵)→ 段0
段0 ─(素の起動を保証)→ Vault, credential broker, 収束エンジン core
収束エンジン core ─(実行時に依存)→ credential broker, Vault, AllocationAuthority, git
収束エンジン core ─(管理する)→ 段1以降の全インフラ + 衛星自身(broker/authority もResource)
```

**鶏卵の構造**:core は broker/Vault を「管理」するが、実行時には「依存」する。この循環は
**段0が素の起動を保証する**ことで断ち切られる(段0で確立した「最後は人間の物理保管に着地する」原則)。
Break Glass L3(段0全滅時)は、この段0再構築能力そのもの。

## 設計を貫く原則(全コンポーネントに共通)

1. **level-triggered + 観測stateレス**(毎周 desired vs actual。持つ state は S1-S4 に分類し S2 のみ永続化)
2. **いつ=収束 / どう=Task・Workflow**(手続き的作業を収束から分離)
3. **確信が持てないとき、加算はしても減算はしない**(unknown≠absent / pruneable:false / 分断 /
   爆発半径 / freeze / **allocation の Release** まで貫通)
4. **値・配置・予測・認可は持たない、委譲する**(Vault / DRS / Prometheus / 上位プラットフォーム)
5. **seam は今切る、split は後**(分散・NetBox・broker・DB供給源を型で予約し、コアは無変更で吸収)
6. **`ownership`(platform / tenant)が横断的に貫く**——所有・書き込みポリシー(PR必須/即時)・
   修復モード・rate limit・爆発半径・供給源選択・シャード分割境界を、**すべて同じ1つの区別が決める**

**到達点**:新プリミティブの追加は Task と Workflow の2つのみ。オンプレ運用の広域(物理〜運用
ライフサイクル)を、3プリミティブ + 少数の横断項目 + seam群で吸収できた。衛星が生えたのは
機能の膨張ではなく、**責務境界を正しく引いた結果**であり、core は小さいまま保たれている。
