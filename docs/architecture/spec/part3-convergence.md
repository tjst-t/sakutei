> Sakutei 設計書 v3.0 の一部（設計の正）。全体目次と §→ファイル対応は [README.md](README.md) を参照。§番号は全 8 部を通して一意。

# 第III部 収束機構

収束機構は、理想状態（desired）を現実（actual）へ寄せる周期処理の本体である。第II部で定めた正準型と
Provider 抽象を土台に、本部は「いつ・どの順で変えるか」を決める四つの機構を規定する——収束ループ（§8）、
desired を生成する render パイプライン（§9）、差分とその原因の帰属（§10）、そして一周期で終わらない収束の
扱い（§11）である。安全機構（第IV部）はこの上に載る手綱であり、本部は「速く収束する力」そのものを定義する。

貫く二つの規律を先に置く。第一に **level-triggered な真実**——毎周期 desired と actual を再導出し、観測状態を
保持しない。第二に **観測 state レス**——キャッシュした「現在状態」を持たず、毎周期リアルを再観測する。この
二つが崩れると、本部の後続すべて（dry-run の正しさ、原因タグの帰属、drift 検知漏れゼロ）が成立しない。

---

## §8 収束ループ

### §8.1 周期ループの全景

収束ループは単一デーモン内で回り続ける単一のループである。一周期（Cycle）の骨格は次のとおり。

```
毎 Cycle:
  1. render(§9)で desired を生成(rendered_revision 確定)
  2. desired のリソース集合を Observe + prune 対象 scope を List
       (全並列・semaphore で同時度を制限)
  3. Diff(集合 diff → フィールド diff)→ 変更が要るノード集合 D
  4. D の部分 DAG を構築
  5. Kahn 法で wave に分解:
       while 残ノードあり:
         wave = 入次数0 かつ 上流が成功済み
         wave 内を並列 Apply(非同期・§8.4)
         失敗ノードの下流を skip 集合へ
  6. Diff の推移を記録(振動検知・ベースライン・§11.3)
  7. jitter 付き sleep → 次 Cycle
```

**render がループの先頭に来る理由**:「何を Observe するか（リソース集合）」も「どの scope を List するか
（prune 対象）」も desired から導かれる。desired が確定していない時点では「全リソース」を定義できない。render
入力は TTL キャッシュ（§9.4）なので、先頭に置いても毎周の外部呼び出しは増えない。

**Observe は全並列**。goroutine で並列化するため、Observe のコストは逐次和ではなく **最遅 Observe に漸近**する。
同時度は相手 API への礼儀として semaphore で上限を課す（既定 16）。

```go
sem := make(chan struct{}, 16)  // 同時 Observe 上限
```

**依存の与え方**は明示 `depends_on`（将来は参照からの暗黙抽出）。設定ファイル上のキーは snake_case
（`depends_on`）に統一し、コード内フィールド名（`DependsOn`）との対応は Loader（§21）が変換する。
**循環検出は理想状態ロード時に一度**行い、循環していればそのリビジョンを入口で拒否してループに持ち込まない。
基盤サービス（DNS/DHCP/NTP）は全リソースが暗黙依存する「依存の根」であり、全部に `depends_on` を書くのは
非現実的なので、粗い優先度帯 **`tier: foundation`**（基盤層は常に通常層より先）で順序を与える。

### §8.2 モデルA:毎周・全 DAG の採用

本システムは **毎 Cycle で全 DAG を回す**（モデルA）。リソース単位の独立コントローラ + 依存 gating（k8s 流）
ではない。理由は規模にある。本システムの想定規模（数十〜数千リソース）では、**plan が常に全体で正しい**ことの
価値が勝る。リソース単位ループは k8s 級の規模で初めて元が取れる。

**Observe は毎周・全部・間引きなし**。毎周全部見ることが観測 state レスの価値の源泉（drift 検知漏れゼロ）で
あり、間引きは本末転倒である。この毎周・全 DAG が **level-triggered な真実の本体**である。§8.6 の早期収束
パスはこれを置き換えず、上に重ねる——トリガで早起きして影響サブ DAG だけを先に収束させても、正しさの保証は
あくまでこの定期周期が担う（トリガを取りこぼしても次の定期周期が拾う）。

**失敗は下流へ伝播する**。依存が失敗・未達なら下流を skip し、次 Cycle で回収する。level-triggered なので
「今回諦めて次で回収」が自然に効く。全停止（Ansible の `any_errors_fatal` 的）より、部分適用 + 自己回復が優る。

**割り切りが破綻する早期警戒線**（立つまで間引き・TTL・頻度別スケジュールは不要）:

- Observe 全体が reconcile 間隔に対し無視できない比率（例:間隔 30s に対し 10s 超）
- 特定プロバイダの API 課金 / rate limit が Observe 頻度で効く
- リソース数が桁で増えた（数百→数千）

破綻したときの延命策は §16。

### §8.3 wave は Cycle 内の並列単位

**wave は「1 Cycle 内の並列処理単位」であって「依存解決の最終形」ではない。** 物理サーバの起動・OS
インストール・image DL のような長時間操作は 1 Cycle で終わらないため、**依存の解放は複数 Cycle に跨って
段階的に進む**のが正式なモデルである（§8.4 の in-flight と §11.1 の converging の組み合わせで扱う）。

そのため依存宣言は「存在するか」ではなく「**特定の観測可能な状態に達したか**」を条件にできる。

```yaml
depends_on:
  - id: http-boot-1
    ready_condition: cloud_init_done    # 単なる存在ではなく、この condition の充足を待つ
```

**intent の書式（v1b で実装済み）**：`depends_on` の要素は、上流の Local ID の文字列か、`{ id, ready_condition }`
のオブジェクトのどちらかである（同じリストに混在してよい）。Local ID は intent 全体で一意であることをロード時に
検証しているので、上流は `id` だけで特定でき、`type` のキーは持たない（未知のキーはロード時に拒否する）。

provider は `Observation.Ready`（§4）に名前付き condition の充足状況を返す。コントローラの `canApply` 判定は、
上流の `Ready[condition]` が真になるまで下流を skip し続ける——これは §8.2 の失敗伝播・下流 skip と **同じ機構の
再利用**であり、新しいプリミティブは要らない。毎 Cycle 再評価されるため、条件が満たされた Cycle で自然に下流が
解放される。ready_condition の多周期収束としての位置づけは §11.2 に述べる。

### §8.4 非同期 Apply と in-flight 集合

VM 作成・image DL・firmware など分単位の Apply が wave に入ると、素朴なモデルは「ループが止まって他の drift
検知が遅延」か「Cycle を重ねて同一グラフへ並行 Apply で競合」のどちらかに陥る。実運用では **Observe コストより
先に Apply 時間が刺さる**。→ **Apply だけを Cycle から切り離す**。

```
・Observe/Diff ループは毎周・定刻で回り続ける(他リソースの drift 検知が長 Apply に巻き込まれない)
・Apply は goroutine で非同期実行し、対象リソースを in-flight 集合に入れる
・in-flight 中のリソースは converging と同じ扱い(再 plan・再 Apply しない、下流は skip → 次周回収)
・Cycle は重ねない(前周の Observe/Diff 完了後に次周)。長いのは Apply だけが継続
・in-flight 集合はインメモリのみ(S3)。再起動で消えても Apply 冪等 + 次周 Observe で安全に再導出
```

wave/skip セマンティクスに「Apply 完了は Cycle 内でなくてよい」を足すだけで、モデルの構造は不変である。
**Cycle は積み重ならない**——前周の Observe/Diff が終わってから次周が始まる。長時間化するのは Apply だけであり、
これは Cycle の外で継続する。

**per-resource in-flight 排他**:同一リソースに対する Apply は同時に一つだけ。定期経路と早期収束パス（§8.6）は
この排他を共有し、両経路が同一リソースへ同時に Apply することはない。rate limit（§13）は in-flight を含む同時
変更数で評価する——さもないと「毎周少しずつ発射し続けて in-flight が積み上がる」抜け道になる。

### §8.5 per-resource backoff

Observe は毎周続ける（安価・drift 検知を維持）が、恒常失敗リソース（権限欠如等）を毎周 Apply で叩くのは相手
API への攻撃になる。**（リソース, エラー種別）単位の指数 backoff + jitter** で Apply をゲートする。

- **transient**（API 一時エラー・ロック競合）→ backoff（上限 15 分程度）
- **permanent**（権限・バリデーション）→ 即 **stuck** 化し、**desired か provider_version が変わるまで
  再試行しない**（原因タグの再利用）

backoff テーブルは S3（インメモリ）。再起動で消えても一回余分に叩くだけである。

### §8.6 早期収束パス（トリガ収束）

本システムは level-triggered ゆえ、収束は **周期律速**である。定期周期（§8.1）は「drift 検知漏れゼロ」を
保証する一方、**人の意図変更（特に tenant 層のセルフサービス API）の反映が次の周期まで待たされる**という構造的
レイテンシを持つ。これに応えるため、**トリガによる早起き（edge ヒント）+ 定期周期（level 保証）** のハイブリッドを
第一級化する——「**edge-triggered な起床、level-triggered な真実**」。k8s の informer watch（edge ヒント）+
resync（level 保証）と同型である。

**貫く原則:トリガは「いつ収束するか」のヒントであり、「何が真実か」を一切運ばない。**

- **トリガ喪失は正しさを損なわない**（遅延のみ）。ヒントを取りこぼしても次の定期周期が必ず拾う。
- 早く起きた収束の中身は通常と同じ新鮮な Observe → Diff → Apply である。トリガはペイロード（変更後の値）を
  運ばず、「この scope が触られたらしい、今見に行け」としか言わない。**観測 state レスは無傷**——トリガに含まれる
  情報を「現在状態」として信じることは一切しない。

**入口は `DesiredSource.Watch`（§21）**。`DesiredSource` に任意 capability として `Watch` を足す（`Provider` に
対する `Lister` と同じ宣言制）。`Watch` は `ChangeHint = {intent_revision, 触られた scope 群}` のみを流す。
FileSource なら git webhook / merge フック、DBSource なら書込通知が発火源である。

**部分収束（scoped reconcile）**:トリガを受けたら全 DAG を回さず、影響サブ DAG だけを収束する。

```
ChangeHint 受信:
  1. render(§9)で新しい rendered desired を生成
  2. 新旧の rendered desired 同士を diff(★決定的・リアル非接触。ここでは Observe しない)
  3. desired が変わったリソースから影響サブ DAG を算出
  4. そのサブ DAG だけを Observe → Diff → Apply(通常の wave/skip セマンティクスをそのまま適用)
```

新旧 desired 同士の diff はリアルに触れないため安価で、**制御プレーンの追加レイテンシは秒オーダー**に収まる。
影響サブ DAG の算出は「新旧 rendered の desired 同士の決定的 diff」から出発する点が要である——ここで Observe を
挟まないから、速い経路が観測 state を再導入しない。

**安全柵（速い経路を汎用修復チャネルにしない）**:

- **速い経路は「desired が変わったリソースのみ」を扱う**。新旧 desired diff から入るため、構造上 **原因タグ
  A（意図）/ E（外部入力）のみ**が対象になる。道中で偶然見つけた **drift B は速い経路で修復せず、定期ループに
  残す**（B は周期律速のままでよい——速い経路の役割純化を優先）。
- **集合差 prune は速い経路で禁止**。prune の正しさは List 完全性（§12）に依存し、部分観測とは相容れない。
  テナントの即時削除は **DBSource 側の tombstone = 明示 absent**（`ExplicitAbsentOnly` の型・§12）で表現し、
  個別リソースの削除として扱う（集合差ではなく）。tombstone は個別リソース削除として速い経路で即時に処理できる。
- **rate limit・累積破壊予算のカウンタは定期経路と共有**する（§13）。速い経路がバイパスになってはならない——
  両経路は同一の予算・同一の per-resource in-flight 排他（§8.4）を共有する。
- **同一 scope へのヒントは合流（coalesce）する**。実行時に **最新の desired を読む**ことで level 意味論を保つ
  （古いヒントで古い desired を適用しない）。scoped run の同時実行数には上限を設け、**溢れたヒントは次の定期周期へ
  縮退**する（速い経路が詰まっても正しさは定期周期が保証する）。合流キューは最新 desired を読む一点に集約する。

**ownership との整合**:

- **tenant 層（即時書込）が主客**。セルフサービス API の変更が秒で反映され、UX が周期待ちから解放される。
- **platform 層は「PR マージ = ヒント発火」**。承認直後に収束が起きる（ガバナンスは不変・レイテンシのみ短縮）。
  承認ゲート・PR 必須（§7）はそのまま効く。

**plan と reconcile はそれぞれ独立に Observe する規律**は速い経路でも守る。トリガで起きた scoped run も、plan の
観測を reconcile に持ち越さない——持ち越すと排除したはずの観測 state を再導入することになる。

**注意点**:render フェーズ1 の副作用（採番・§9.1）が速い経路にも乗るため、**allocator の可用性が UX に
直結する**。速い経路で新規払い出しが要る変更は allocator 応答が律速になる（§9.3 の不達時は前回保持で安全側だが、
UX としては遅延として現れる）。allocator 停止中は当該変更が `waiting-allocation` へ縮退し（§19）、ヒントはキュー
保持されて allocator 復帰後の周期で払い出される（正しさは損なわれず、UX 遅延のみ）。定常収束は衛星コールゼロで
回るため、速い経路で allocator が要るのは「新規採番を伴う変更」だけである（§19）。

---

## §9 render パイプライン

desired の生成は、**複数の外部権威に依存し、副作用を持ち、毎周走る多段パイプライン**であり、収束ループと同格の
複雑さを持つ。第一級コンポーネントとして扱う。

### §9.1 二フェーズ

```
render = フェーズ1(外部入力解決:★副作用あり・外部権威に依存)
           ├ inventory 合成(外部 CMDB)
           ├ シークレット世代解決(Vault metadata:値は読まない)
           └ allocation 解決(AllocationAuthority への払い出し)
       + フェーズ2(決定的展開:副作用なし・同じ入力 → 同じ出力)
           └ 合成リソースの Expand(virtual_network → vxlan + vds + dhcp)
```

**フェーズ1 は外部入力解決**であり、副作用を持ち外部権威に依存する。inventory を外部 CMDB から合成し、
シークレットの世代を Vault metadata から解決し（値は読まない）、allocation を AllocationAuthority から払い出す。
**フェーズ2 は決定的展開**であり、副作用を持たず、同じ入力に対し常に同じ出力を返す。合成リソースの Expand は
このフェーズに属する。**Expand の居場所は render フェーズ2**（収束ループ内の入れ子コントローラではない）。
決定性の要求はフェーズ2の規律として一括で課す。

### §9.2 外部権威到達不能時は前回解決値を保持

**どの外部権威が不達でも「前回解決値を保持」する。空の desired を生成しない**（全削除事故の防止）。これは
Observe 側の `unknown≠absent` と対をなす、desired 側の非対称原則である。

| 外部入力 | 不達時 |
|---|---|
| inventory（CMDB） | 前回生成物を保持 |
| シークレット世代（Vault metadata） | 前回世代を保持 |
| allocation authority | 前回割当を保持（**新規払い出しは保留** → 未割当リソースは blocked で次周回収） |

Vault が sealed-restart 中は世代解決も死ぬため、SecretRef のキャッシュは **値だけでなく世代 metadata も含む**
（§20）。

**incomplete≠empty（部分成功の防御）**:「不達 → 前回保持」は二値であり、「200 OK だが中身が縮んで返る」
（CMDB のレプリカ遅延・部分障害）を扱えない。歯抜けの inventory で render すると、欠落分が「理想に存在しない」と
展開され closed-world prune で削除対象になる——空 desired ではないので「全削除事故防止」もすり抜ける。

- **render 入力に「期待基数 / 前回比の急減検知」を入れる**。前回 N 件だった inventory が今回 X%（既定 50%）未満に
  急減したら、それを「不達」と同じく前回保持側へ倒す（または halt + Event）。
- render 出力に対しても、削除方向 Diff の急増（§13）を専用に監視する。

**v1b の実装（inventory 合成と incomplete≠empty ガード）**：

- inventory はフェーズ1 の `InventorySource` seam から合成する。v1b の実装は intent と同じ形式の resource を書いた
  1 ファイル（CMDB export の代役）を読む `FileInventorySource` で、常駐ループの `--inventory-file` で指定する。
  ファイルが無い・読めない（書き込み途中を含む）ときは「空」ではなく「不達」と答える。
- 判定（毎周、前回受け入れた inventory と比べる）：
  - 不達で前回がある → 前回の inventory を保持して続ける。不達で前回が無い → その周を halt。
  - 件数が前回の `--inventory-min-ratio`（既定 0.5。**ちょうど半分も incomplete**）以下、または `--inventory-expected-min`（期待基数。既定 0 = 無効）未満 → **incomplete として周期を halt**し、
    Apply も prune も走らせず `InventoryIncomplete` Event を出す。非空 → 空もこれに含まれる。
  - halt 中は基準（前回受け入れた件数）を動かさないので、inventory が戻るまで止まり続ける。それ以外は受け入れて
    新しい基準にする。漸減（例 10 → 8）は受け入れ、通常どおり prune に進む。
- 境界の注意：§12.4（b）の List 急減の実装は「50% を**超える**減少」で halt する（ちょうど半分は halt しない）ので、
  inventory（ちょうど半分で halt）とは境界が 1 点ずれている。どちらも既定値 50% の規律であり、安全側は inventory のほうである。
- 基準は S3（メモリ）に置く。再起動後の最初の取得はそのまま受け入れる（期待基数を宣言していれば、それで下限を守る）。
- inventory の内容ハッシュ（並び順に依存しない）を rendered_revision の外部入力に含める（cause E。§9.4）。

### §9.3 TTL キャッシュと平常時の頻度規律

render が毎周実行になる結果、Vault metadata 読み（シークレット数ぶん）と authority Lookup（割当リソース数ぶん）が
**毎 Cycle の外部呼び出し**になる。→ **render 入力は TTL 付きキャッシュ**（staleness 上限 = TTL）。障害時挙動
（§9.2）と合わせ、平常時・異常時の両方の規律が揃う。TTL キャッシュのヒット/ミスや列挙順で rendered_revision の
値が揺れてはならない（§10.1）——揺れると intent も現実も変わっていないのに E と誤判定し、無用な halt を招く。

### §9.4 intent_revision と rendered_revision

```
intent_revision   … 人が書いた理想(git commit hash / DB トランザクション ID)
rendered_revision … render 出力の世代(intent + 外部入力の解決結果すべてのハッシュ)
```

**desired は人の commit なしでも変わる**（シークレット rotate・割当変更・inventory 変更）。この二つを型で分ける
ことが、原因タグ（§10）の判定基盤になる。`DesiredSource.Fetch()` が返すのは intent_revision、収束が比較するのは
rendered_revision である。**rendered_revision は scope 単位で算出する**——scope をまたいで一括ハッシュ化すると、
無関係な scope の外部入力変化が全体の E 判定を汚す。

`rendered_revision` は **decision-relevant な入力のみの決定的ハッシュ**とする。TTL キャッシュのヒット/ミスや
列挙順で値が揺れると E と誤判定し、逆に外部入力の変化がハッシュに反映されない実装バグは E を B と誤判定して、
rotate したシークレットを古い世代へ強制的に戻す。したがって「何を含め何を含めないか」と「TTL ヒット/ミスで
揺れないこと」を厳密に規定する。

**v1b での具体**：

- FileSource の intent_revision は、intent ディレクトリの `.json` / `.yaml` / `.yml` ファイル（loader が読むのと同じ
  集合）の内容をファイル名順にハッシュしたものである。v1a では `.json` だけを数えていたため、YAML の intent の
  編集が cause B に誤分類されていた（v1b で修正）。git 配布（§17.4）では `intent/<scope>/` 以下を再帰的に数える。
- rendered_revision の外部入力として、v1b では **採番の割当**（§19）と **inventory の内容ハッシュ**（§9.2）が入る。
  どちらかが変われば cause E になる。

### §9.5 合成リソースは実子リソースに展開する

「VXLAN 設定 + VDS 設定 → ユーザには仮想ネットワーク一つ」のように、下位リソース群を上位一つに束ねる。IaaS
カタログの各商品はこの合成リソースとして実現される。

```yaml
resource: virtual_network        # ユーザが書く(見せる面)
tenant: acme
id: app-net
cidr: 10.50.0.0/24
# ↓ render フェーズ2 が展開(内部)
#   → vxlan_segment{ vni: 5010, ... }
#   → vds_portgroup{ vlan: ..., uplink: ... }
#   → dhcp_scope{ ... }
```

**下位を別 Resource として依存グラフに乗せる**（一つの provider に押し込まない）。VXLAN と VDS は異種システム
（NSX / vCenter）を叩くため、単一 Apply に混ぜると部分失敗の扱いが破綻する。別 Resource なら、展開は扇状 DAG
（wave / 依存 gate がそのまま効く）になり、下位の drift を個別に検知・修復でき、上位削除で下位を巻き取り（所有権・
`pruneable`・§12）、原子性が要る下位群は「1 wave で原子的に」指定できる。

**採らない代替案:合成リソースを単一 Provider として実装する**（子を内部に隠蔽し Apply 時に展開する）。一見
「controller は基本か合成かを意識せず扱える」利点があるように見えるが、**安全機構が効かなくなる方向に働く**。
合成が一つの Provider になると、コントローラから見える変更は一件になり、子 50 個の削除が「1 リソースの変更」として
rate limit（§13）を通過してしまう。**子が独立した Resource だからこそ「50 件の削除」として halt できる。** 同じ
理由で、下位ごとの drift 個別検知・原因タグの粒度・部分失敗の扱いも失われる。→ **render フェーズ2 で展開し、子を
理想状態に実在させる方式を正とする**。

**展開ロジック**はテンプレート（inventory データ + テンプレート）による展開を方式とする。単純な変数展開を超える
処理（プール採番・CIDR 計算・tier 別の自動割り当て等）は、テンプレートエンジンのカスタム関数、または展開後に走る
処理層で埋める。カスタム関数は **決定性を保つ**こと（同じ入力 → 同じ出力・§9.1）が要件で、その担保は
rendered_revision 算出定義に含める——決定性は原因タグ E の判定基盤である。将来的には Loader（§21）を CUE に
差し替えることで、基本リソースと展開ロジックを同一言語で書けるようになる。

### §9.6 generated/ 規約

合成リソースの展開結果を明示的に生成物として書き出す経路では、出力先を `generated/` に限定する。`generated/` は
**自動生成専用**であり、自動生成ヘッダを必須とし、直接コミットを拒否する（二重管理防止）。人が書く intent の出力先は
`intent/` であり、両者を型レベルで分離する。

### §9.7 ステータス集約（最悪優先）

合成リソースの上位ステータスは、**最も悲観的な下位が上位を支配**する（fail-safe・非対称原則の再適用）。全順序を
最悪優先で定める。

```
unknown > stuck > oscillating > drifted > blocked > converging > converged
```

`stuck`（permanent 失敗・§8.5）は最悪級（人手を要する確定失敗）なので `unknown` の次に置き、`blocked`（依存未達）は
「上流待ち=一時的」なので `drifted` と `converging` の間に置く。これで報告用の Event 分類（§26）と収束ステータス
集約の軸が一致する。どの下位が原因かは追跡できる。この全順序は catalog（§21）の UI 表示、`sakutei status`
出力（§10）、多周期収束の判定（§11）で共通に用いる。

---

## §10 差分と原因

### §10.1 Observation と ConvergenceStatus の型分離

`drifted / converged` は desired との比較結果=**Diff の出力**であって、Observe（現実を見るだけ）では判定できない。
混ぜると Observe が desired を知る必要が生じ、**dry-run の正しさの根拠**（Observe/Diff 分離）が崩れる。

```go
type Observation struct {
    Availability Availability     // ok | unknown(到達不能) | converging(相手が収束作業中)
    State        State            // Availability == ok のときのみ有効
    Ready        map[string]bool  // 名前付き ready condition の充足状況(§8.3, §11.2)
    ObservedAt   time.Time        // この観測を取った時刻(周期越え再利用の検知に使う)
}
```

収束ステータス（報告用）はコントローラが **導出**する。

- `unknown` / `converging` … Observation から
- `converged` / `drifted` … Diff 結果（Plan が空か否か）から
- `oscillating` … 履歴から（§11.3）

**Observe は desired を知らず、Diff は現実を触らない**が型で保証される。

### §10.2 変更の原因区別（A〜E）

収束ループが見るのは「desired ≠ actual」という **結果**だけで原因を区別しない。だが同じ差分でも **なぜ生じたかで
危険度と対応が正反対**である。diff 結果に **原因タグ**を持たせる。

| 原因 | 判定 | 対応 |
|---|---|---|
| **A. 意図的**（人が理想変更） | intent_revision が変化 | 適用してよい（規模制御は効かせる） |
| **B. drift**（現実が乖離） | 消去法（diff が存在し、intent_revision / provider_version / rendered_revision がいずれも不変） | 自己修復 or アラート（remediation mode） |
| **C. ツール更新**（解釈が変化） | provider_version が変化 | 最も危険・見逃されやすい（ghost diff） |
| **D. カスケード**（A/E の依存波及） | 上流の変化に連鎖（DAG 下流） | plan 段階で波及範囲を可視化 |
| **E. 外部入力変化** | rendered_revision は変化したが intent_revision は不変 | 下記 |

- **B は消去法で判定する**:観測 state を持たないため「actual が変化した」こと自体は観測できない（前回の
  actual を保持していない）。diff が存在し、他の三つの revision がいずれも動いていなければ、現実側が動いたと
  結論する。
- **C（ghost diff）**:provider の正規化ロジックを直しただけで、理想も現実も変わっていないのに既存全リソースが
  「今の解釈と違う」と判定され再適用される。**正しい修正でもバグでも同じ見た目**。→ provider_version を diff
  計算の入力に含める。C を検知したら自動適用せず `ProviderUpgradeDiff` Event を発火し、dry-run で人が確認してから
  解禁する（twin（§26.2）への自動流し込みを既定運用に組み込み、ghost diff を本番前に観測する）。
- **E のサブタグ**:**E-secret**（世代 rotate）/ **E-alloc**（intent 不変のまま割当だけ変わった場合——リソース
  追加に伴う新規払い出しは intent_revision が変わるので A）/ **E-inventory**（CMDB 変更）。
- **E の扱い**:帰属は「人の commit 由来ではない」と記録（A と混ぜない）。rate limit は不明度が高いので破壊的側に
  重み付けする。
- **D の価値**:「image 一つ更新 → 依存 50VM が再作成」のようにカスケード規模が直感を超える。

### §10.3 原因タグは per-resource で帰属する

原因タグは **per-resource で確定する**。Cycle グローバルの revision（「intent_revision が変わった Cycle は全部 A」）で
語ると、**1 Cycle 内に A と E が混在するとき、E 由来（CMDB バグ）の破壊が無関係な人の小 commit に「相乗り」して
A の緩い扱いで通過する**穴がある。これを塞ぐため、**各リソースの diff 入力ハッシュを「intent 由来部分 / 外部入力
由来部分 / provider_version 由来部分」に分解し、どの部分が動いたかでタグをリソース単位に決める**。グローバル
revision は「候補の絞り込み」にのみ使い、最終帰属は per-resource-diff で確定する。これは `Plan.Op.Cause`（§4）が
原因タグを持つことで実装する。

### §10.4 provider_version の diff 埋め込みと fail-closed

C の検知は「provider_version を diff 入力に含める」に全依存する。埋め込みの付け忘れが「C 検知漏れ=ghost diff が
B に化けて自動適用」という **危険側に倒れる**緩い契約であってはならない。またモノリシックバイナリでは
provider_version = reconciler バイナリ version になり、本体の無関係な更新で全リソースを誤検知しうる。

- version の粒度は「**provider の diff/正規化ロジックのソースハッシュ**」を SDK（§28）が自動算出する（手動 version
  でなく）。reconciler 本体 version とは分離する。これで誤検知（本体更新）・検知漏れ（付け忘れ）を両方防ぐ。
- **version 未埋め込みの provider は起動時に登録を拒否**（fail-closed）。Schema lint（§28）に「version 未宣言」を
  加える。

### §10.5 last-seen リング（B の「なぜ」追跡）

運用者の最頻の問い「なぜ変わろうとしているのか / いつからズレたのか」に、B は「消去法（否定形）」でしか答えられ
ない——観測 state レスの代償で「actual がいつ変わったか」を原理的に言えなかった。→ **直近 N 周の actual
スナップショットを S3（インメモリ・非永続）にリングで保持し、drift 検知時に「前周は X だった」を Event に添える**。
喪失しても安全側（S3 の定義通り数周鈍るだけ）であり、**永続化しない限り観測 state レスの看板は守れる**。これを
`sakutei status` の「なぜ変わるか」表示（原因タグ + 直近スナップショット差分 + 下流波及 D + provider 階級）と
一体で提供する。

### §10.6 content-address 参照

image 等の参照は **名前ではなくハッシュで参照する**（`image@sha256:...`）。名前参照だと「同じ名前だから変わって
いない」と誤認し、古い image の VM が残る。**content-address が drift 検知を正しくする**——参照そのものが内容の
ハッシュなので、内容が変われば参照が変わり、Diff が正しく差分を検出する。これは型に食い込むため中核で導入する。
ready 判定も存在ではなくチェックサム/署名一致で行う（破損・改竄・途中失敗を ready と誤認して VM を産まないため）。

---

## §11 多周期収束

収束は一周期で終わらない（§8.3）。相手が内部に収束ループを持つ場合、依存が観測可能条件の充足を待つ場合、
そして自分と相手が綱引きして発振する場合——これらを扱う機構を本節に集める。

### §11.1 converging の三層検知

converging は「収束器の上の収束器」（SDN / VCF / アレイ / k8s は内部に収束ループを持つ）を扱う上で最重要だが、
多くの API に「作業中」の概念がない。**三層 + deadline** で扱う。

- **Tier1 ネイティブ**:vCenter task / NSX realization 等を写像する（`Capabilities.Converging = true`）。相手が
  「作業中」を返せる場合、そのまま converging として尊重する。
- **Tier2 settle 猶予（普遍的な床）**:自分の Apply 直後は型ごとの `settle: 30s / N サイクル` の猶予窓を置き、その間の
  不一致を drifted 扱いしない。**検知手段ゼロの provider でも成立**する。相手が「作業中」を返せなくても、自傷猶予で
  一時的な不一致を drift と誤認しない床が張れる。
- **Tier3 発振からの推定**:M 回再 Apply しても Diff が縮まない → `oscillating` 判定し、そのリソースを **自動で
  alert-only に隔離**する（綱引きの自動検疫）。発振は「自分の Apply と相手（他システム・人）の変更が互いを打ち消し
  合う」状態であり、放置すると毎周無駄な Apply を撃ち続けるため、自動隔離で連鎖を断つ。
- **deadline 必須**:converging には timeout を付け、超過で `drifted` + Event に昇格する（相手ループの stuck を
  取りこぼさない）。全て S3（インメモリ）で足りる。

三層いずれも報告用ステータスは §9.7 の全順序に載る（`oscillating > drifted > converging`）。

### §11.2 ready_condition（観測可能条件への依存）

依存は「存在するか」ではなく「**特定の観測可能な状態に達したか**」を条件にできる（§8.3）。provider は
`Observation.Ready`（§10.1）に名前付き condition の充足状況を返し、コントローラは上流の `Ready[condition]` が真に
なるまで下流を skip し続ける。毎 Cycle 再評価されるため、条件が満たされた Cycle で下流が自然に解放される。

```yaml
depends_on:
  - id: leaf03-node1
    ready_condition: data_network_verified   # 観測可能条件の充足を周期をまたいで待つ
```

これは **周期をまたぐ収束**の中核である。物理サーバの立ち上げ（`bare_server` 電源投入 → `installed` →
`data_network_verified` → `proxmox_node` クラスタ join）のように、1 台の立ち上げが数十 Cycle に跨って進行する。
各段の間の待ちは、§8.2 の失敗伝播・下流 skip と同じ機構で表現され、新しいプリミティブを要さない。

**v1b の実装の挙動**：上流の今回の観測で `Observation.Ready[condition]` が真でない間、下流は skip する（失敗伝播と
同じ扱いで、待ちの理由をログに出す）。真になった周期で下流を解放する。この判定は常駐ループ・one-shot の
`reconcile`・早期収束の scoped 経路のすべてで効く。上流が desired に無い（外部所有の）場合も、上流を観測して
gate に使う。

**シャード間依存の「観測 ready 待ち」への接続**は §18 を参照。あるシャードのリソースが別シャードのリソースの
ready を待つ場合も、この ready_condition の意味論をシャード境界越しに適用する。

### §11.3 oscillation 検出（Diff 縮小の記録）

発振の検出は **Diff の推移の記録**に基づく。収束ループは毎 Cycle で Diff の推移を記録し（§8.1 の手順6）、同一
リソースに対し M 回再 Apply しても **Diff が縮まない**ことを発振の兆候とする。振動検知が「Diff が縮まない」を見るのに
対し、異常検知（§13）は「Diff が突然巨大化した」を見る——同じ Diff 時系列監視の別用途である。

- Diff 縮小の履歴は S3（インメモリ）に保持する。喪失しても検知が数周鈍るだけ（安全側）。
- 発振と判定したリソースは §11.1 Tier3 に従い自動で alert-only に隔離する。
- ベースライン（平常時の変更量）も同じ時系列から導出し、§13 の異常検知が参照する。

### §11.4 capacity gate

多周期収束の途上でも、クラスタ使用率が閾値を超えたら新規の変更を止める。これは rate limit の一種であり、収束の
速度そのものを容量で律速する。capacity gate の宣言形・halt セマンティクス・「是正は reconciler の外」の分担は
変更予算に属するため **§13.4 を正とする**（本節はその多周期収束における適用点であることを述べるに留める）。


