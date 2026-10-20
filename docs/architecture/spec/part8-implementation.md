> Sakutei 設計書 v3.0 の一部（設計の正）。全体目次と §→ファイル対応は [README.md](README.md) を参照。§番号は全 8 部を通して一意。

# 第VIII部 実装

本部は Sakutei を実際に作る順序（§27）、採用を左右するプロバイダ SDK の設計（§28）、そして実装中に決める事項と設計では解けない既知の限界（§29）を定める。第II部〜第VII部で確定した機構を、どの順で・どこまで実装するかへ落とし込む。

---

## §27 実装スライス

### §27.1 スライスの基準

v1 を一括の MVP として積むと、後付け不可能なコア型と実装量の大きい機能が同列に並び、規模が過大になる。したがって v1 を **v1a（コア実証）／ v1b（brownfield 導入と採番連携）／ v1.5（手続きの最小形）** に割り、v2 以降を明確に切り離す。分割基準は二つである。

- **後付けすると全プロバイダ・全 scope に波及するか** → **v1a**（コア型・seam・安全機構の骨格）。
- **その機能が無いと使い始められない導入形態があるか** → **v1b / v1.5**（既存基盤への導入・手続き）。

言語は **Go**（全 interface 定義が Go）。バイナリは `sakutei` **単一**とし、サブコマンドで機能を分岐する。状態保存は**メモリ + file（git）が主**で、将来 `DesiredStore` として bbolt / SQLite を差し替える（§4 の状態分類に従い、喪失が事故になる S2 のみ外部永続化する）。**etcd や必須 DB プロセスは持たない**。

### §27.2 v1a — コア実証（≈10 人月）

v1a が実証するのは二つの体験である——**「触っていないのに勝手に理想状態へ戻る」**（level-triggered な収束）と、**「壊れそうで止まる」**（安全機構）。この二つが成立すれば、Sakutei の中核命題は証明される。

**コア interface / 型（後付け不可・最初に型へ）**

1. `Provider` interface（`Schema` / `Capabilities` / `Read` / `Apply`、正準は §5）と `Lister`（prune の前提）。
2. `Observation`（Availability / State / Ready）と `ConvergenceStatus` の分離（dry-run の根拠・§4）。
3. `Capabilities`（機械的縮退・§5）。
4. `SecretRef` 型（自動マスク + 明示 Resolve + 世代フィールド・§20）。
5. `DeletionPolicy` / `pruneable`（既定 false）/ `scope + managed`（§12）。
6. **原因タグ（A/B/C/D/E）** と `intent_revision` / `rendered_revision` の分離（**per-resource 帰属**・§10）。
7. `provider_version` を diff 計算に含める（埋め込みは登録必須契約・§10）。
8. **コンテンツアドレス参照**（`image@sha256`）。
9. `ownership`（platform / tenant）と `remediation mode`（+ `remediation schedule`・§14）。
10. `Executor`（実行境界。v1 は `LocalExecutor` のみだが、**コントローラは Executor 型にしか触らない**・§5）。
11. **コア型（正準形は §4 で確定済み）**：`State`（Schema 付き map）/ `Plan`（操作列 + 破壊性 + 原因タグ + ID + 診断）/ `ID`〈`(provider, scope, id)` 複合〉/ `Scope`（ラベル集合 + パス）/ `Event`（単一型 + kind enum + scope/resource/revision 参照）、および `rendered_revision` 算出定義〈**scope 単位で算出可能**〉。**構造は確定済み**であり、第0スプリントは算出定義の厳密な文書化と型の実装に着手する（新規の着手前決定は残っていない・§29）。
12. **早期収束の seam**：`DesiredSource.Watch`（§21）+ **scoped run（部分収束）+ 合流キュー**（§8）。reconcile/plan とほぼ同一コードパスを部分 DAG に適用するだけなので増分が小さい。v1a では FileSource の webhook/merge フックによる `Watch` を対象とする（DBSource 連動は v2）。
13. **スケールアウトの型切り**（実装は必要時）：`Capabilities.BulkObserve`（§5）+ Executor の per-resource `Read` fallback 経路、`sakutei --claim`（§17）の起動フラグと scope-selector パース。

**機能**

- 収束ループ（level-triggered / DAG / wave / 非同期 Apply / in-flight 排他 / backoff・§8）。
- **`tier: foundation`**（基盤サービスを依存の根として DAG 順序に反映・§8）。
- render パイプライン（2フェーズ / TTL キャッシュ / 障害時は前回値保持 + **incomplete≠empty 急減検知**・§9）。ただし **v1a のフェーズ2 展開は非採番の固定値展開のみ**（採番連携は v1b）。
- **rate limit + halt**（in-flight を含めて評価）+ **破壊的変更の累積時間窓予算**（§13）+ **List 完全性検証**（件数急減 halt・scope 単位閾値宣言・§12）+ **異常検知の "halt-all で凍結" セマンティクス**（§13）。
- **`planned_change` 宣言 + 専用予算レーン**（§13）——大量正当作業の解除弁。型と予算レーン評価は v1a（承認情報 requirements 連携は v1.5）。**実装は v1.5 にずれ込んだ**（Backlog #39・§13.3「v1.5 の実装」）。
- **運用プロファイル（solo / team / enterprise）**（§14）——規模別の安全既定値セット。既定は team。
- `ownership: platform` の破壊的大量変更は **A タグでも承認必須**（悪意・誤操作の両方を止める・§13。solo は cooling-off で代替可・§14）。
- Event 型 + 構造化ログ（JSON）出力。
- conformance レポート + **`sakutei status` のデバッグ体験**（原因タグ + provider 階級表示 + **軽量 last-seen リング**〈S3・非永続で「前周は X だった」を添える〉+ 下流波及 D・§10）。
- `FileSource`（git）+ YAML/JSON `Loader` + JSON Schema 検証（§21）。
- Vault `SecretBackend` 1つ + 段0 からのトークン注入（**provider 資格は SecretRef 直結。broker は無い**・§20）。
- **最小 SDK**：Schema 駆動 diff エンジン（順序付き含む）/ secret マスキング / managed 除外 / エラー分類 / version 埋め込み。**Schema lint はロード拒否レベルで強制**（§28）。
- devenv 層A（フェイク provider）+ 層B（軽量本物）+ カオステスト基盤（§26）。
- **provider 2〜3個**：DNS + VM + FW（順序付き）。**DNS を第一号**とする——Read / Apply が最もクリーンで検証環境も軽く、「触っていないのに勝手に戻る」体験を最小のコードで作れるからである。**管理 NW コアは収束対象にしない**（§23）。
- **非採番 allocation のみ**（採番連携は v1b）。

v1a のゴールは、DNS レコード provider から podman コンテナ / Caddy ルート / TLS 証明書と広げ、数百行のオーケストレータ + provider 数個で level-triggered な自己修復を greenfield / devenv 上に成立させることである。これは §27.3 の SDK フルセット・import・Sync Mode を含む「v1 全体」ではなく、規模は1桁小さい。

### §27.3 v1b — brownfield 導入と採番連携

v1b が実証するのは、**既存の大規模基盤に安全に入れる**ことと、**採番権威と連携して合成リソースを実体化する**ことである。

- **`sakutei import`（reverse-render）**（§24）+ **managed 候補の自動サジェスト**（§24）。
- **`ScopeMode`（converge / sync）+ Sync Mode の反映ループ**（§24）。
- **`AllocationAuthority` interface + 最小実装**（v1 の採番はファイル版で確定。SQLite 版は v2 以降・§19.9）と **render の採番連携**（割当の解決はフェーズ1、子への伝播はフェーズ2 の展開・§9）。**採番を使う合成リソースは v1b から**（v1a では非採番の固定値展開のみ）。
- **allocator HA 最小形 + Lookup キャッシュ + waiting-allocation 縮退**（§19）：段0 VM-HA 上の active/standby + fencing、割当事実のシャードローカル無期限キャッシュ（S3）、allocator 停止時の `waiting-allocation` 縮退。**プール分割 by claim**（intent での disjoint レンジ宣言・§19）。
- **テストハーネス**（契約テスト自動生成）+ golden path テンプレート（§28）+ **BulkObserve 等価性テスト**（fake provider 上で `BulkObserve(scope) ≡ {Read(id)}` を検証・§5/§28）。
- **`ready_condition` と複数 Cycle に跨る依存解決**（§8）+ **capacity gate**（§13）——最初の実インフラ provider（VM / 物理）を書く段で必要になる。
- **BulkObserve の実バルク実装 + 一貫性契約**（§5）——v1 ではファイル系の dnszone・nftables で実証し、バルク API を持つ provider（vCenter / Proxmox 等）は v1 の後に書く（Backlog #49）。complete-or-error / prune 非流用 / `observed_at` 刻印を SDK で強制。型は v1a で切ってあるので追加は provider 単位で局所。
- **静的シャーディングの実動作**（§17）：`--claim` フィルタ + fencing による二重 claim 検知 + 再シャード Workflow（drain → claim 変更 PR → fencing 世代更新）。型（起動フラグ）は v1a で切り済み。
- **claim の git 配布 + S2-free シャード階級 + エッジ資格局所性**（§17/§20）：`intent/<scope>/...` レイアウト規約 + sparse checkout / partial clone、ミラー中継層、S2-free シャード（Resource 収束のみ）、店舗ローカル file SecretBackend への資格局所常駐、開店/閉店 = claim 変更 PR ハンドオフ。
- **tier 別 Observe 頻度（間引き）** の導入余地（§16 の早期警戒線が立った層から）。
- **inventory 検証**（フェーズ1 の外部入力〈inventory 合成〉が急減・欠損したときの incomplete≠empty 判定・§9）。

**実装状況（2026-10-20 時点）**：

| 項目 | 状況 | 正 |
|---|---|---|
| `sakutei import` + managed 候補 | 実装済み | §24.4 |
| `ScopeMode` + Sync Mode | 実装済み（`generated/` への書き出し + git コミット。コミッターは既定 `sakutei-sync`・署名なし、v3.9） | §24.5 |
| `AllocationAuthority` + 採番連携 | ファイル版（JSON + flock）で実装済み。**v1 の採番はこれで確定**。SQLite 版は v1 の範囲外（v2 以降、Backlog #36） | §19.3・§19.9 |
| allocator HA 最小形・Lookup キャッシュ・waiting-allocation・プール分割 by claim | 実装済み（`sakutei allocator`） | §19.6・§19.8・§19.9 |
| 契約テストの Schema からの生成・golden path テンプレート・BulkObserve 等価性テスト | 実装済み | §28.3・§28.6 |
| `ready_condition` + capacity gate | 実装済み（capacity gate は CLI フラグで宣言） | §11.2・§13.4 |
| BulkObserve の実装 + 一貫性契約 | 実装済み（dnszone・nftables で実証。収束の経路は SDK の `CycleObserver` でバルク経路を使い、1 周のファイル読み込みは 1 回）。実際の一括 API を持つ対象（Proxmox・vCenter 等）の provider は Backlog #49 | §5.7 |
| 静的シャーディング（claim フィルタ + fencing） | 実装済み。再シャードは手動手順（Workflow 化は v1.5） | §17.1・§17.6・§15.2 |
| claim の git 配布 + S2-free + 資格局所性 | 実装済み（`sakutei mirror`・`sakutei secrets extract`）。資格の中央からのローテーションは v1.5 | §17.4・§17.5・§20.5 |
| tier 別 Observe 頻度 | 実装済み（キーは `observe_tier` / `observe_tiers`） | §16.5 |
| inventory 検証 | 実装済み（ファイルの inventory source） | §9.2 |

v1b の途中で、v1a の実装の簡略化が設計とずれていた 2 点を直した——prune の閉世界が provider 全体だった（§12.2）・
intent_revision が YAML の intent を数えていなかった（§9.4）。

### §27.4 v1.5 — 手続きの最小形（v2 から前倒し）

20 構成の適用検討で 12 構成が「事業の核」を Task/Workflow 系に依存させていた。したがって**最も需要の高い最小形を前倒す**。多相 Task 状態機械・自動段階ロールアウトは重く需要が特定領域に偏るため v2 に残す。

- **単純 Task**（§6 単純型）：一度実行して現実側に完了印。migration / 一回性操作。S2 は進行フェーズのみ。
- **最小 Workflow**：**直列（one-at-a-time）+ 不変条件（invariant）+ 手動承認ゲート**のみ（§6/§14）。ローリング / DR の骨格をこの3要素で表現する。**動的 select・自動 canary 判定・自動段階ロールアウトは v2**。
- **承認ゲートの N-of-M 拡張**（§14）——承認者要件（N-of-M・役割制約・separation〈申請者≠承認者〉・全承認者の S4 記録）。ゲート定義の `requirements` と承認 API の署名対応のみで実装。
- **災害時運用経路（DR モード）**（§25）——DR intent のプリステージ（claim ハンドオフ + DR Workflow の組み合わせ）+ 緊急ローカル書込経路（書込 break-glass の型 + Event/S4 検出 + 事後合流 PR 起票）。
- **`planned_change` 宣言 + 専用予算レーン**（§13）——v1a から持ち越し（Backlog #39 で実装）。
- **`planned_change` の承認情報連携**（§13）——予算レーンに、承認 requirements を接続する。運用プロファイル（solo / team / enterprise・§14.5）の既定と solo の cooling-off もここで入れる（Backlog #44 で実装・§13.3「承認と運用プロファイル」・§14.5「v1.5 の実装」）。
- **`remediation schedule`**（§14）——保守窓文化の構成で必須なので v1.5 で入れる。

### §27.5 v2 以降

- **採番の SQLite 版（または NetBox 等の外部権威）**（§19.9）——v1 の採番はファイル版で確定（v3.8）。
- **多相 Task 状態機械**（§6 多相型）。
- **自動段階ロールアウト + 自動 canary ゲート**（§14）/ health（observability as Resource）/ 常設 canary / Prometheus exporter。
- **`DBSource`**（および `DBSource` 連動の `Watch`・§21）。
- **credential-broker**（§20）——JIT 短命発行 / bastion / 個人単位の到達資格。
- **分散 Executor + lease 縮退**（§18）+ **動的ルーター / 自動リバランス**（§17 段3）。
- **Observe 間引き・tier 別頻度・watch 併用**の本格化（§16）。
- **intent UI（catalog）**（§21）/ twin フル機能（§26）/ 抽象リソース（§28.8）。

**注**：早期収束の Watch seam + scoped run（§8）と静的シャーディング（§17）は v2 ではなく v1a / v1b に前倒し済みである。v2 に残るのは **動的**ルーター / 自動リバランスと DBSource 連動 Watch のみである。

### §27.6 作らないもの（委譲）

シークレットストア本体（Vault）/ 時系列監視・通知ルーティング（Prometheus・Alertmanager）/ 配置最適化（DRS）/ 認可・課金・ポータル（上位プラットフォーム）/ アプリ層のデプロイ・スケール（k8s）/ CMDB 全体（採番のみ自作）。委譲の根拠と停止影響は §22 を正とする。

### §27.7 スライスの相対規模

| スライス | 実証するもの | 目安 |
|---|---|---|
| **v1a** | 触っていないのに戻る + 壊れそうで止まる（コア型・収束ループ・安全機構・最小 SDK・provider 2〜3個・devenv） | **≈10 人月** |
| **v1b** | 既存基盤に安全に入る + 採番連携（import / Sync / tier 別頻度 / 静的シャーディング / inventory 検証） | v1a と合わせ「使い始められる」水準 |
| **v1.5** | 手続きの最小形（単純 Task / 最小 Workflow / N-of-M 承認 / DR / remediation schedule） | 12 構成が依存する核 |
| v1 全体 | 上記 v1a + v1b + v1.5 の合計 | ≈22.5 人月（v1a とは1桁違う） |

---

## §28 Provider SDK

### §28.1 なぜ SDK が採用を決めるか

**プロバイダの書きやすさが Sakutei の普及を直接決める**。エコシステムでプロバイダの数と品質を得た側が広く使われる——これは先行ツールの教訓である。したがって設計の肝は、**プロバイダ作者が書く最小を極限まで削り、面倒はコア側の共通ライブラリ（SDK）が引き受ける**ことにある。作者が書くべきは **約4関数 + `List`**（prune を許す場合）に留め、残りはすべて SDK が担う。

### §28.2 作者が書くもの（約4関数）

削りに削ると、作者が書くべきは次の4つ。prune を許すなら `Lister.List` を1つ足す（正準 interface は §5）。

- `Schema()` — フィールド定義（型 / 必須 / secret / managed / ordered / identity / computed）。
- `Capabilities()` — TrueObserve / List / Converging / CommitConfirm / BulkObserve。
- `Read(ctx, id)` — 現実を読む（desired を知らない）。戻りは `Observation`。
- `Apply(ctx, plan)` — 現実を変える（create / update / delete を1関数に統合）。
- （任意）`List(ctx, scope)` — scope 内の現実の全 ID。prune の前提。
- （任意）`BulkObserve(ctx, scope)` — scope 内をまとめて Read（バルク API を持つ場合）。

設計上の負担軽減は三つである。**（1） Diff を書かせない**——差分計算は Schema を使ってコアが汎用実装する（順序付きコレクションのような特殊 diff も Schema で `ordered` と宣言すればコアが適用する）。**（2） create/update/delete を Apply 1本に統合**——`Plan` に「何をすべきか」が入っているので分岐は作者の自由。**（3） Schema がすべての鍵**——型・必須・secret・managed・ordered・identity・computed を宣言すれば、コアが diff・検証・マスキング・managed 除外を全自動化できる。

### §28.3 SDK が担うもの

| SDK 機能 | 何を肩代わりするか | 関連 |
|---|---|---|
| 差分エンジン | Schema ベースの汎用 diff（順序付き含む） | §5 |
| secret マスキング | Schema で `secret` 宣言したフィールドを自動 `SecretRef` 化・ログマスク | §20 |
| managed fields 除外 | Schema で `managed` 宣言したフィールドを diff 対象外に自動除外 | §7 |
| version 埋め込み | provider ビルド version を diff 入力に自動合成（原因タグ C） | §10 |
| エラー分類ヘルパー | Apply のエラーを transient / permanent に型付け | §5.5 |
| テストハーネス | フェイクプロバイダ基盤・Read/Apply の契約テスト自動生成 | §26 |
| **BulkObserve 等価性テスト** | fake provider 上で `BulkObserve(scope) ≡ {Read(id) ∀ id ∈ scope}` を検証。これを通ることが「完全階級 + bulk」を名乗る条件（§5）。v1b で `BulkEquivalenceTester` として実装済み（下記） | §5 |
| **Schema lint** | 「secret 宣言し忘れ」「identity 未指定」等を静的検出。**ロード拒否レベルで強制** | §28.4 |

**v1b のテストハーネス（`pkg/sdk`）**：

- **`ContractTester`**（契約テスト）：冪等性・absent → Create → Read・List 完全性・三値を検査する。sample の desired
  （`SampleDesired`）を渡さなくても、provider の Schema から生成して回せる。生成の規則は、`Examples`（フィールドごとの
  例の値。対象が値を検証するフィールドだけ渡せばよい）にあればその値、無ければ Kind からの既定値。Computed / Managed は
  入れず、Secret は `SecretRef` の形にする。sample の ID は `MakeID` で作り、既定は Identity フィールドの値を名前順に
  `/` でつないだもの。生成結果は `GenerateSample` で作者が確かめられる。`MakeUnreachable`（対象を到達不能にする関数）を
  渡すと、到達不能のとき `Read` が error か `unknown` を返すこと（`ok` + 空の State で「無い」と答えないこと・§4.5）を
  検査する。最小階級（`TrueObserve: false`）の provider には、identity 以外のフィールドの欠落を許す。`RunAll` は呼び出し側の
  `ContractTester` を書き換えない。
- **Schema（`FieldSpec`）は契約テストのために変えない**と決めた（例の値は Schema に載せず、テスト側の `Examples` に置く）。
  Schema は provider の公開 API であり、テストの都合で属性を増やすと全 provider に波及するためである（2026-10-20 の決定）。
- **`BulkEquivalenceTester`**（§5.7 の等価性テスト）：次を検査する。`Capabilities.BulkObserve` の宣言と `BulkObserver` の
  実装の一致（両方向）・対象 ID（明示、または `List(scope)` が `ListComplete` で返したもの）がすべて bulk の結果にある
  こと・各 ID の Availability が `Read` と一致し、`ok` なら State も一致すること・bulk の各 Observation に `ObservedAt` が
  あること・List から対象を取ったとき、bulk が List に無い ID を返さないこと。違反はまとめて報告する。

### §28.4 最難関は Schema 設計

実装の重さの本体は Read / Apply のロジックではなく、**Schema を正しく書くこと**である。以下の宣言を誤ると、設計が積み上げた安全保証が**無音で無効化**される（エラーにならず、静かに保証だけが外れる）。

- どのフィールドが **secret**（§20）→ 誤ると資格情報が平文でログに出る。
- どのフィールドが **managed / runtime**（§7）→ 誤ると DRS と永久に喧嘩する。
- どのフィールドが **ordered**（§12）→ 誤ると FW ルールの順序が壊れる。
- どのフィールドが **identity**（削除・参照のキー）→ 誤ると別リソースを消す。

**identity は「運用上の変更頻度」で選ぶ**。技術的な一意性だけで決めてはならない。変更されやすい属性（シリアル番号・MAC アドレス）を identity にすると、その属性が変わるたびに Resource の再生成と理想状態の commit が必要になり、運用コストが跳ね上がる。

> 例：物理サーバの identity をシリアル番号にすると、故障交換のたびに intent の書き換えが要る。**物理トポロジ座標**（「leaf03 スイッチの 12 番ポート」= `leaf03-p12`）を identity にすれば、筐体交換時に理想状態の変更が一切不要になる。シリアルや型番は `managed: true` + `computed` として観測のみ行い、identity にはしない。

したがってプロバイダ実装は「**まず Schema、次に Read/Apply**」の順で書く。SDK の Schema lint が宣言漏れ（secret 未宣言・identity 未指定）を静的に検出し、通らない Schema は理想状態ロード時に拒否する（循環検出・prune 宣言拒否と同じ入口・同じ思想）。

### §28.5 プロバイダの階級（全機能を要求しない）

**階級（完全・標準・最小）の定義は §5.6 を正とする。** ここでは SDK 文脈での含意だけを述べる。

**作者は自分の階級を選べる**。最小階級なら Apply だけ書けばよく参入障壁が劇的に下がる。Capabilities で正直に宣言すれば、コアが自動的に縮退して**安全に**扱う（prune させない・破壊的側に倒す・§5）。「全機能を書けないと参加できない」ではなく「**書ける範囲で参加でき、コアが安全に縮退する**」——これがエコシステムを広げる鍵である。

### §28.6 golden path と事例集

作者を最短経路に乗せるため、SDK は次を用意する。

- **golden path テンプレート**：Schema → Read → Apply の骨格を持つ雛形。最小階級はさらに短い。
- **契約テスト自動生成**：Read / Apply の冪等性・三値の正しさ・List 完全性を fake provider 上で自動検証。
- **v1b の雛形（`templates/provider/`）**：`full/`（完全 + bulk。Schema → Read → Apply → List → BulkObserve の骨格）と
  `minimal/`（最小階級。Schema → Read〈存在のみ〉→ Apply）の 2 つ。どちらも対象との境界を `Client` seam に切り、
  インメモリの fake の上で `ValidateProvider`・契約テスト・（full は）BulkObserve 等価性テストが最初から合格する。
  コピーして名前を 1 語置き換えるだけでビルド・テストが通ることを受け入れテストで毎回確かめている。標準階級は `full/`
  から `List` と `BulkObserve` を外して作る。
- **worked example**：Provider と合成リソースの完全な実装例を参照実装として提供する（Proxmox 上の MySQL DBaaS の Provider / Schema を `docs/examples/proxmox-mysql-dbaas-schema.md` に置く）。

### §28.7 2層プロバイダ戦略

エンタープライズでの価値の大半は provider の数と品質にあるが、全ベンダーを自前で書き切るのは不可能である。したがって **自前コアと Ansible の二層**で埋める。

- **コア層（自作 provider）**：DNS / FW / VM 等、重要で **Diff 品質・横断項目対応**が要るもの。**完全階級**を目指し、prune・原因タグ・converging・commit-confirm を効かせる。
- **ロングテール層（Ansible provider）**：雑多な機器・OS 設定。**品質を落として広くカバー**する。**changed: true/false のみ**を返し、State を返さない（最小階級）。

**Ansible は「モジュール」だけ借りる**。冪等な実行実体であるモジュールだけを借り、実行モデル（playbook / 一回 push）は借りない。収束ループ（いつ・何を = Sakutei）と実行実体（どう = Ansible モジュール）が噛み合う。

```go
type AnsibleExecutor struct { ... }   // Executor の一実装（§5）
//   Observe → ansible module を check mode で叩く（= Read）
//   Apply   → ansible module を通常実行（冪等適用）
```

Ansible provider では横断保証の多くが落ちる。**check mode は「与えた desired に対する changed 真偽」しか返さず、current state を返さない**——Diff は Ansible 内部で完結し、コントローラ側の Diff・原因タグ A/B の区別・converging 検知・破壊的変更の重み付けが軒並み機能しない（`Capabilities.TrueObserve = false` による機械的縮退）。実装の8割が泥臭いロングテール層である以上、Sakutei の保証群は当面**少数派のコア provider にしか完全には効かない**。これは設計の嘘ではなく、正直に認識すべき制約である（§29）。

**緩和は二つ**。`*_info` 系モジュールで Observe できる対象は `TrueObserve: true` に**部分昇格**でき、縮退は全面ではない。また順序付きコレクション / managed fields / commit-confirm のような**横断機能は Sakutei 側でラップして扱う**（Ansible は1操作の実行だけを担い、賢さは自前側に置く）。時間発展としてはコア実装を広げ Ansible の担当範囲を狭めていくが、専用 API が複雑でベンダー固有性が高い領域（ブロックストレージ製品等）は自前化が割に合わず、Ansible provider が長く残る。

### §28.8 【将来検討】抽象リソースと実装の分離（v1 では扱わない）

依存を**具体型でなく能力（capability）に対して張りたい**場面がある（例：「DB は VM の上で動くが、その VM が VMware か Proxmox かは問わない」）。抽象リソース（役割）とその実装（プロバイダ）の分離であり、k8s の StorageClass / Crossplane Composition と同型である。**v1 では具体依存のみを扱い、以下を将来の検討事項として記録する**（seam として `Provides() []Capability` を予約する余地はあるが、解禁は慎重に行う）。決定として固定しておくのは次の四点である。

- **実装選択は誰が決めるか＝②platform ポリシー主 ＋ ①運用者上書き**。①運用者が明示 / ②platform 層のポリシーが決める（ownership の自然な拡張・主候補）/ ③システムが最適化して選ぶ、の三択のうち、③（配置最適化）は DRS へ委譲済みの領域（§13）であり**踏み込まない**。**②主 ＋ ①上書き**が既存の ownership 設計と整合する。なお「依存の抽象化」（能力に依存する語彙）は render の入力であって収束の対象ではない——抽象は人が書く語彙であり、収束が扱う実体は具体化後のリソースである（§9）。
- **最大の危険は leaky abstraction**。「VM」は VMware と Proxmox で実際には異なる（スナップショットの意味・NW モデル・live migration・managed fields）。→ **抽象は最大公約数の能力に限定**し、実装固有機能が要るなら具体プロバイダを名指す。「隠してよい差異か、隠すと事故る差異か」の見極めがドメインごとの設計判断である（VM は抽象化しやすい、ストレージは実装差が大きく慎重に）。
- **段階導入**。全面導入せず、`compute` のような抽象化しやすい少数の capability から始め、実装差の大きい領域は具体型のまま残す。役割能力は操作能力（`Capabilities`・§5）とは別の `Provides()` として宣言し、抽象と具体の適合は interface 型チェックで検証する。
- **解禁トリガ＝2つ目の同種プロバイダ実装が実際に必要になったとき**。1つしかない間は抽象化しても検証できず、早すぎる抽象化は最大公約数を誤る（§29.2 の再検討トリガ表に登録する）。

---

## §29 残る課題と既知の限界

設計は概ね閉じている。**実装着手前に決めるべきコア型・着手前決定はすべて確定済み**（§4 でコア型正準形、§20 で S4 永続化先まで確定）であり、この索引を実装中に**再オープンさせない**運用を敷く（着手前決定が新たに現れたときのみ再オープンする）。残るのは （1） 実装中に決める事項、（2） 意図的に先送りした機能とその再検討トリガ、（3） 設計では解けない既知の限界、（4） 非技術の着手条件である。

### §29.1 実装中に決める事項

設計の正しさには影響せず、実装・運用しながら詰める事項である。いずれもコア型に波及しない。

| 項目 | 決める内容 | 関連 |
|---|---|---|
| **多相 Task の「相の途中 death」の人手ゲート** | 各相の入口 Observe（§6）は相境界の death は守るが、Applying 途中（書き込み中）の death を「リブート中」と bricked で区別できない。Applying を「危険区間」としてマークし、区間内 death は Observe で Done 確認できない限り **auto 再開を禁止し人手ゲート**にする、その具体判定 | §6 |
| **reimage の delete/create ペアリング判定** | `planned_change` の `kind: reimage` 宣言下で、再イメージが「delete 誤計上」でなく正当な delete→create の対であることを機械的に確認する判定基準。**v1.5 で決定**：同じ周期の Plan で、同じ provider・scope のラベル集合が完全一致・1 対 1（§13.3「v1.5 の実装」） | §13 |
| **break-glass の到達不能判定基準・合流競合手順** | 緊急ローカル書込経路（§25）を発動してよい「git 到達不能」の判定基準と、復旧後に本流と事後合流するときの競合解消手順。**v1.5 で決定**：到達不能 = 中央 git（ミラーではなくその upstream）への `git ls-remote` が 1 回 10s の timeout 付きで、10s 間隔に **3 回続けて**すべて失敗したとき（回数の下限 3。1 回でも届けば書けない。認証の失敗も不達に数える）。合流 = break-glass コミットを rebase・squash せず `breakglass/<session>` として中央へ push し PR の下書きを作る。中央が base から進んでいれば両側で変わったパスを下書きに並べ、**自動解消はせず人が解く**（中央を正とし、合流ブランチに中央を merge して載せ直す）。取り込みは merge commit / fast-forward に限り、中央が break-glass の先端を含むまでミラーは中央を追わない（§25.1「v1.5 の実装」） | §25 |
| **`planned_change` の kind 整合検証** | 宣言 kind（reimage / decommission 等）と実際に生じた Plan の操作種別が一致することの検証ルール。**v1.5 で決定**：kind ごとに通す操作種別を固定し（delete→Delete、update→Update、reimage / recreate→対の Delete+Create）、scope 内で kind の合わない破壊的 op はレーンに入れず通常予算で評価して `PlannedChangeKindMismatch` を出す（§13.3） | §13 |
| **List 急減閾値のチューニング** | scope 単位で宣言する件数急減 halt の閾値の実運用値（誤発火と見逃しの均衡） | §12 |
| **Ansible executor の Observe 頻度** | fork + SSH の高レイテンシ層で tier 別間引き（§16。`observe_tier`）をどう既定するか。executor を実装するまでの既定は「毎周」 | §16 |
| **運用パラメータ** | Sync Mode の commit 主体（bot / 署名有無）、scope 昇格の「N 日間 diff ゼロ」の N、reconcile 間隔既定値、freeze の粒度。**Sync Mode の commit 主体は v1b で決定**：既定のコミッター（author も同じ）は `sakutei-sync <sakutei-sync@localhost>`、署名なし。名前・メールは `--committer-name` / `--committer-email` で変えられる。署名は既定でオフとし、足すときはコミットを作る 1 か所に鍵を渡す形にする（§24.5「v1b の実装」） | §14/§24 |

### §29.2 意図的な先送りと再検討トリガ

一部の機能は**意図的に先送り**している。先送り自体は正しい判断だが、**いつ見直すか**を決めておかないと、放置されるか、逆に早すぎる時期に蒸し返される。したがって「先送りを決めたら、同時に**再検討トリガ**を決めておく」ことをメタ決定として固定し、主要な先送りにトリガを紐づける。

| 先送り項目 | 再検討トリガ | 関連 |
|---|---|---|
| 抽象リソース（capability への依存） | **2つ目の同種プロバイダ実装が実際に必要になったとき**。1つしかない間は抽象化しても検証できず、早すぎる抽象化は最大公約数を誤る | §28.8 |
| CUE への `Loader` 移行 | テンプレート側にロジックが溜まり始めたとき（カスタム関数の増加） | §21.3 |
| シャーディング / 分散 Executor | 単一デーモンの天井（§16 のレイテンシ算術）に近づく**早期警戒線**に触れたとき | §17, §18 |
| 多周期の**振動検知・隔離**（`StatusOscillating` の点灯と隔離）| v1a では未配線——`core.StatusOscillating` は型として定義済みだが、これを点灯させ振動リソースを隔離する production コードは無い（§27 が v1a の安全機構を予算 + List 完全性に絞っているため、許容される v1a の限界）。危険な系（破壊的な flip-flop ループ）は ghost-diff hold 経路が構造的に抑える——cause C は毎周期 hold され破壊ループに落ちないことを chaos テストで実証済みであり、**明示的な振動隔離**はそれとは別に §11/§15 の検知規定に沿って後続で配線する | §11, §15 |

### §29.3 既知の限界（設計では解けないもの）

検証を経て確定した、**設計で埋められない**限界である。これらは設計の穴ではなく、対象側の制約・別事業・責務外に純化している（第I部の境界宣言と一致する）。

- **（a） 管理対象の API が貧しい環境は観測止まりが上限**。Observe すら不可能な旧機器・ベンダーロックの箱（SOHO 級ネットワーク / ストレージ機器等）は、台帳上 `unknown` のまま第一級で扱い（`ledger_only` / `observability:none`・§24）、`external_owner` で縁は切れるが、**収束（Apply）の品質は上がらない**。可視化・drift 検知までが到達上限として確定する。
- **（b） 上位テナント基盤（quota / 課金 / 認可 / ポータル）は Sakutei と同規模の別事業**。委譲は工数を消さない——Sakutei は `DesiredSource` の一実装として認可・quota 適用済みの理想状態を受け取るだけであり、パブリック IaaS 級の構成で残る差分は、この委譲先の実装工数であって Sakutei 側の技術ギャップではない（§22）。
- **（c） ゼロタッチ物理・provisioner 実体は責務外**。電源も入っていない物理の「段0 の底」を人手・OOB なしに立ち上げる能力は持たない。ブートストラップは人手（Ansible + 金庫）から始まる（§23）——これは妥協ではなく定石であり、自己を立ち上げるものは自分の外側から来るしかない。provisioner（起動方式と OS 種別を分離したベアメタル導入基盤）の実体構築も責務外である。
- **（d） AWS 等パブリッククラウドは技術的に可能だが対 Terraform の経済判断**。バルク API（Describe*）を持つため BulkObserve 経路に素直に乗り、技術的には収束できる。しかしその領域は Terraform のエコシステムが厚く、**採否は技術ではなく経済（既存 provider 資産との費用対効果）で決まる**。Sakutei の主戦場はオンプレであり、パブリッククラウドは適用可能だが主対象ではない。

これらの限界は、20 構成の適用可能性検討で到達不能として残った領域がいずれも上記に純化したこと——**設計で解ける課題はすべて解けた（残るは実装順序と対象側の制約のみ）**という収束状態——を根拠とする。

### §29.4 非技術の着手条件

- **位置づけ：OSS 個人プロジェクトを第一歩とする**。本プロジェクトは作者個人が OSS として開発する。将来的に業務でも利用しうるが、第一歩は OSS である。したがって職務発明・雇用主の IP 規程による着手前の制約は**存在しない**（この関門はクリア済み）。この位置づけの転換により、次の 2 点が設計判断として第一級に昇格する——(a) **プロバイダ SDK の公開 API 設計**（§28。採用を左右する中核目標であり、ボーナスではない）、(b) **段0-minimal（§23.6）が「縮退形」ではなく OSS 初期利用者にとっての標準の入口**になりうること（Proxmox 3ノード + Ceph + Vault-HA の段0 は個人利用には重い）。
- **ライセンス：Apache-2.0**。最初のコード公開時点から適用する。インフラ／Go／プロバイダ SDK エコシステムの定番（Terraform 系・k8s 系と同じ）であり、特許条項を含むため将来の企業利用でも安心して採用できる。ライセンス選定は OSS 前提では第一級の着手前判断であり、ここで確定させる。
- **本書と参照実装文書の分割**（境界は第I部で定義済み）。参照実装の内容を足す時点で境界が崩れるため、分割は早いほど安い。
- **プロジェクト名の衝突確認**。コンポーネント名を機能名に改めたため、確認対象は固有名詞 `Sakutei` のみである。OSS 公開に伴い、パッケージ名・リポジトリ名・既存 OSS との名称衝突を公開前に確認する。

### §29.5 着手前決定を再オープンさせない運用

コア型・着手前決定の索引（§4 のコア型正準形・§5 の Provider interface 正準形・`rendered_revision` 算出定義・S2/S4 永続化先・`ready_condition` 名前空間）は**すべて確定済み**である。実装の第0スプリントはこの索引を参照して型を書き始めればよく、着手前決定として残る未決は無い。**新たに「後から変えると全プロバイダに波及する」種類の決定が現れたときにのみ、この索引を再オープンする**——それ以外の実装中の判断（§29.1）は索引を再オープンさせない。この規律が、パッチ的改訂で見通しを崩さないための運用境界である。

### §29.6 v1b で見つけた実装上の問題

v1b の実装中に見つけ、Backlog に積んだ（または未登録の）実装上の問題である。いずれも設計（原則・機構）の変更を要しない。

| 問題 | 内容 | 関連 |
|---|---|---|
| dnszone が読めない行を黙って読み飛ばす（Backlog #37） | ゾーンファイルの解釈できない行を捨てるため、打ち間違えたレコードが `Read` で「無い」（`ok` + 空の State）と見なされ、Create されて重複しうる。unknown ≠ absent（§4.5）に照らせば読めない行は `unknown` / error にするのが安全側だが、未対応の RFC1035 構文を含む既存のゾーンファイルがすべて `unknown` になるため、どこまで厳しく読むかは未決 | §4.5・§12.4 |
| 予算カウンタが S3（メモリ）のまま（v1.5 で確認・未登録） | §13.2 は累積時間窓予算のカウンタを S2 相当として永続化すると定めるが、実装はメモリに持ち、再起動で窓がリセットされる。planned_change のレーン使用量（§13.3）は v1.5（#44）で S4 の `planned_change.executed` から読み直すようになった（`--audit-log` を付けたとき。付けない常駐は再起動をまたぐと `max_count` を超えうる。one-shot `reconcile` は `--audit-log` が無ければレーンを開かない）。常駐は使用量を周の終わりに S4 へ書くので、周の途中で落ちるとその周の分は数えられない | §13.2・§13.3 |
