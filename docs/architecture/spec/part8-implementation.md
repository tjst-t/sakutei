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

Ansible provider では横断保証の多くが落ちる。**check mode は「与えた desired に対する changed 真偽」しか返さず、current state を返さない**——Diff は Ansible 内部で完結し、コントローラ側の Diff・原因タグ A/B の区別・converging 検知・破壊的変更の重み付けが軒並み機能しない（`Capabilities.TrueObserve = false` による機械的縮退。実装は `ansible` provider で、観測はコアに足した最小の階級の口 `provider.CheckReader` を通す、§29.1）。実装の8割が泥臭いロングテール層である以上、Sakutei の保証群は当面**少数派のコア provider にしか完全には効かない**。これは設計の嘘ではなく、正直に認識すべき制約である（§29）。

**緩和は二つ**。`*_info` 系モジュールで Observe できる対象は `TrueObserve: true` に**部分昇格**でき、縮退は全面ではない。また順序付きコレクション / managed fields / commit-confirm のような**横断機能は Sakutei 側でラップして扱う**（Ansible は1操作の実行だけを担い、賢さは自前側に置く）。時間発展としてはコア実装を広げ Ansible の担当範囲を狭めていくが、専用 API が複雑でベンダー固有性が高い領域（ブロックストレージ製品等）は自前化が割に合わず、Ansible provider が長く残る。

### §28.8 【将来検討】抽象リソースと実装の分離（v1 では扱わない）

依存を**具体型でなく能力（capability）に対して張りたい**場面がある（例：「DB は VM の上で動くが、その VM が VMware か Proxmox かは問わない」）。抽象リソース（役割）とその実装（プロバイダ）の分離であり、k8s の StorageClass / Crossplane Composition と同型である。**v1 では具体依存のみを扱い、以下を将来の検討事項として記録する**（seam として `Provides() []Capability` を予約する余地はあるが、解禁は慎重に行う）。決定として固定しておくのは次の四点である。

- **実装選択は誰が決めるか＝②platform ポリシー主 ＋ ①運用者上書き**。①運用者が明示 / ②platform 層のポリシーが決める（ownership の自然な拡張・主候補）/ ③システムが最適化して選ぶ、の三択のうち、③（配置最適化）は DRS へ委譲済みの領域（§13）であり**踏み込まない**。**②主 ＋ ①上書き**が既存の ownership 設計と整合する。なお「依存の抽象化」（能力に依存する語彙）は render の入力であって収束の対象ではない——抽象は人が書く語彙であり、収束が扱う実体は具体化後のリソースである（§9）。
- **最大の危険は leaky abstraction**。「VM」は VMware と Proxmox で実際には異なる（スナップショットの意味・NW モデル・live migration・managed fields）。→ **抽象は最大公約数の能力に限定**し、実装固有機能が要るなら具体プロバイダを名指す。「隠してよい差異か、隠すと事故る差異か」の見極めがドメインごとの設計判断である（VM は抽象化しやすい、ストレージは実装差が大きく慎重に）。
- **段階導入**。全面導入せず、`compute` のような抽象化しやすい少数の capability から始め、実装差の大きい領域は具体型のまま残す。役割能力は操作能力（`Capabilities`・§5）とは別の `Provides()` として宣言し、抽象と具体の適合は interface 型チェックで検証する。
- **解禁トリガ＝2つ目の同種プロバイダ実装が実際に必要になったとき**。1つしかない間は抽象化しても検証できず、早すぎる抽象化は最大公約数を誤る（§29.2 の再検討トリガ表に登録する）。

### §28.9 外部 provider の組み込み方

provider は本体のバイナリに組み込む（プラグインのプロセス境界や動的ロードは持たない。§3 原則 4・5：境界は Go の interface で今切り、プロセス分割は後）。本体のリポジトリの外で provider を書く人は、fork せずに **Sakutei を Go のライブラリとして使い、自前の provider を登録した自前のバイナリを作る**。

- **モジュールのパスは `github.com/tjst-t/sakutei/v2`**（v2.0.1 から。Go の決まりでメジャー版 2 以上はパスに `/v2` を付ける）。外のモジュールは `go get github.com/tjst-t/sakutei/v2@<版>` で取り、`github.com/tjst-t/sakutei/v2/pkg/...` を import する。
- **入口は `pkg/cli`**。`cmd/sakutei` は `cli.Main()` を呼ぶだけで、外のモジュールも同じ入口を使う。`cli.WithProviders(func(*registry.ProviderRegistry))`（または組み立て済みの registry を渡す `cli.WithRegistry`）で足した provider は、組み込みの provider（`registry.DefaultRegistry`）に加わる。verb・フラグ・終了コードは本体と同じで、足した provider は常駐・`plan`・`reconcile`・`status`・`import`・`sync` のすべてで名前（intent の `provider`）から解決される。
- **名前の衝突は起動時のエラー**。組み込みと同じ名前、または二つの登録で同じ名前を使うと、どの verb でも起動時に終了コード 1 で止まる。上書きはしない（黙って別の実装に差し替わると、Schema と ProviderVersion の前提が崩れる。§10.4）。
- **provider の設定はファイルで渡す**（`--provider-config <file>`、JSON または YAML）：

  ```json
  {"providers": {"<name>": {"<key>": "<value>", "...": "..."}}}
  ```

  provider ごとの値は、共通のフラグ（`--state` 等）から作る設定に**重ねて**その provider の factory に渡る。同じキーはファイルの値が勝つ。値はスカラーに限り、文字列として渡る（パスの解釈は factory の責任）。登録されていない provider の名前はエラー（打ち間違いを黙って無視しない）。既存のフラグはそのまま使える。
- **外の provider は `pkg/` だけで書ける**。`pkg/provider`（interface・Schema・Capabilities・エラー分類）、`pkg/core`（正準型）、`pkg/sdk`（`ValidateProvider`・契約テスト・BulkObserve 等価性テスト）、`pkg/registry`（factory の登録）。secret のフィールドは Apply に SecretRef の参照形で届くので、`core.ParseSecretRef` で読み、`sdk.NewFileSecretResolver`（file SecretBackend、§20.5）を渡して `SecretRef.Resolve` で Apply の直前にだけ値にし、使い終えたら `Zero()` する（§20.2）。`internal/` には頼れない（Go の規則で外から import できない）。
- **起動時の検査は組み込みと同じ**。足した provider も `sdk.ValidateProvider`（ProviderVersion・Schema lint、§10.4）を通らなければ使われない（fail-closed）。雛形は `templates/provider/`（§28.6）を使い、契約テストを自分のモジュールで回す。
- **動く例**は `examples/external-provider/`（別の `go.mod` を持ち、`replace` で本体を指す小さなモジュール。ファイルに書くだけの provider を一つ登録した `main`）。受け入れテストがこの例をネットワークに出ずにビルドし、plan・reconcile・status・import・常駐で動くことを毎回確かめている。

---

## §29 残る課題と既知の限界

設計は概ね閉じている。**実装着手前に決めるべきコア型・着手前決定はすべて確定済み**（§4 でコア型正準形、§20 で S4 永続化先まで確定）であり、この索引を実装中に**再オープンさせない**運用を敷く（着手前決定が新たに現れたときのみ再オープンする）。残るのは （1） 実装中に決める事項、（2） 意図的に先送りした機能とその再検討トリガ、（3） 設計では解けない既知の限界、（4） 非技術の着手条件である。

### §29.1 実装中に決める事項

設計の正しさには影響せず、実装・運用しながら詰める事項である。いずれもコア型に波及しない。

| 項目 | 決める内容 | 関連 |
|---|---|---|
| **多相 Task の「相の途中 death」の人手ゲート** | 各相の入口 Observe（§6）は相境界の death は守るが、Applying 途中（書き込み中）の death を「リブート中」と bricked で区別できない。Applying を「危険区間」としてマークし、区間内 death は Observe で Done 確認できない限り **auto 再開を禁止し人手ゲート**にする、その具体判定。**決定（2026-10-27、Backlog #52）**：（1）**危険区間**は相の宣言 `Critical`（firmware では Applying だけ）の **RunStep の間**。RunStep の前に S2 の記録へ印（`critical`）を立て、返った後に下ろす。デーモンの停止は RunStep を切らないので、印が立ったまま残るのは持ち主が落ちた（lease 切れ）か、印を下ろす書込に失敗したときだけ（安全側）。（2）**Done と言える判定**は二つ：その相の入口の Observe（`CheckStep`）が **Done**（firmware では書き込んだ版が機器に見える）か、Task 全体の **完了印**（稼働版）が収束ループの Observe で見えること（いつもの Settle で記録を消す）。印の立った記録を引き継ぐデーモンはその相の `CheckStep` を 1 回問い、Done なら印を下ろして普通に続ける（入口の Observe で飛ばす）。**pending（書けたと見えない）も unknown（見えない・Observe のエラー）も Done ではない**——リブート中と bricked を区別できないので、自動で再開せず rollback もせず、記録を `awaiting_approval` にして人手ゲートを開き、RequiresTask（warning）で知らせる。ゲートの間も周ごとに同じ判定を問い、Done が見えれば承認なしでゲートを閉じて続ける。（3）**承認**は `sakutei task approve --store --audit-log --approver [--role] [--signature] <resource>`（却下は `reject`）。Workflow の承認（§6.4）と同じく S4 監査に append してから S2 に書き、コアは認可を持たない（§7）。要件は単一承認（team の既定、§14.4・§14.5。N-of-M・separation は Task の申請者を持たないので v2 の外）。承認した記録は危険区間の相の入口で手放され（相のタイムアウトは承認の時刻から数え直す）、次の周にどのデーモンでも引き継ぐ。入口の Observe が済んでいないと言えば **RunStep をもう一度呼ぶ**（書き直してよいと人が決めた）。却下は `failed`（halt、人が cancel するまで再起動しない。rollback もしない）。（4）**ゲートのタイムアウト**は既定 24h（§6.4 のゲートの既定と同じ。`--task-gate-timeout`）。開いたときに上限を記録に置くので、デーモンを起こし直しても延びない。過ぎたら `failed`（halt）、過ぎた承認は効かない。（5）desired が起動時から変わっていればゲートを閉じない（人が確かめて cancel する、#51 の引き継ぎと同じ）。`Critical` でない相は、RunStep の途中で落ちても入口の Observe が済んでいないと言えばもう一度 RunStep を呼ぶので、べき等な相に限る | §6 |
| **reimage の delete/create ペアリング判定** | `planned_change` の `kind: reimage` 宣言下で、再イメージが「delete 誤計上」でなく正当な delete→create の対であることを機械的に確認する判定基準。**v1.5 で決定**：同じ周期の Plan で、同じ provider・scope のラベル集合が完全一致・1 対 1（§13.3「v1.5 の実装」） | §13 |
| **break-glass の到達不能判定基準・合流競合手順** | 緊急ローカル書込経路（§25）を発動してよい「git 到達不能」の判定基準と、復旧後に本流と事後合流するときの競合解消手順。**v1.5 で決定**：到達不能 = 中央 git（ミラーではなくその upstream）への `git ls-remote` が 1 回 10s の timeout 付きで、10s 間隔に **3 回続けて**すべて失敗したとき（回数の下限 3。1 回でも届けば書けない。認証の失敗も不達に数える）。合流 = break-glass コミットを rebase・squash せず `breakglass/<session>` として中央へ push し PR の下書きを作る。中央が base から進んでいれば両側で変わったパスを下書きに並べ、**自動解消はせず人が解く**（中央を正とし、合流ブランチに中央を merge して載せ直す）。取り込みは merge commit / fast-forward に限り、中央が break-glass の先端を含むまでミラーは中央を追わない（§25.1「v1.5 の実装」） | §25 |
| **`planned_change` の kind 整合検証** | 宣言 kind（reimage / decommission 等）と実際に生じた Plan の操作種別が一致することの検証ルール。**v1.5 で決定**：kind ごとに通す操作種別を固定し（delete→Delete、update→Update、reimage / recreate→対の Delete+Create）、scope 内で kind の合わない破壊的 op はレーンに入れず通常予算で評価して `PlannedChangeKindMismatch` を出す（§13.3） | §13 |
| **List 急減閾値のチューニング** | scope 単位で宣言する件数急減 halt の閾値の実運用値（誤発火と見逃しの均衡） | §12 |
| **Ansible executor の Observe 頻度** | fork + SSH の高レイテンシ層で tier 別間引き（§16。`observe_tier`）をどう既定するか。**決定（2026-10-27、Backlog #127）**：Ansible provider の既定も「毎周」のまま（§8 の既定を変えない）。1 回の観測は ansible の起動で local 接続でも 1 秒前後かかるので、間引くかは利用者が `observe_tiers` / `observe_tier` で宣言する（provider からは既定を決めない。安定した設定ほど大きな `observe_every` にする） | §16 |
| **運用パラメータ** | Sync Mode の commit 主体（bot / 署名有無）、scope 昇格の「N 日間 diff ゼロ」の N、reconcile 間隔既定値、freeze の粒度。**Sync Mode の commit 主体は v1b で決定**：既定のコミッター（author も同じ）は `sakutei-sync <sakutei-sync@localhost>`、署名なし。名前・メールは `--committer-name` / `--committer-email` で変えられる。署名は既定でオフとし、足すときはコミットを作る 1 か所に鍵を渡す形にする（§24.5「v1b の実装」） | §14/§24 |
| **dnszone の SOA シリアル**（Backlog #70） | ゾーンファイルの SOA は管理の外（provider はレコードとして観測も Apply もしない）だが、シリアルが上がらないとセカンダリへの転送やサーバの再読み込みが起きず、収束した値が DNS の答えに出ない。**v1 で決定（v3.12）**：管理するレコードを実際に書き換えた Apply（Create / Update / Delete でファイルの中身が変わったとき）だけ、同じ書き込みで SOA のシリアルを上げる。新しい値は `YYYYMMDDnn`（日付は UTC）で、「今日の `YYYYMMDD00`」と「今の値 + 1」の大きいほう——今の値より必ず大きくなる（今の値が日付の形でない・未来の日付・その日の 99 を使い切ったときも + 1 で進む）。32 ビットの上限（4294967295）を超えるときは上げずに Apply を PermanentError で止める（巻き戻しは RFC 1982 の比較でセカンダリを壊すため）。SOA のそれ以外の欄（MNAME・RNAME・refresh・retry・expire・minimum）は引き続き管理の外で、書き換えない。シリアルは Diff の対象にしない（自分で上げた値をずれと見なさない）。ゾーンに SOA が無ければ何もしない。既定は「上げる」、provider の設定 `soa_serial: keep`（CLI は `--zone-soa-serial keep`、既定 `bump`）で上げないようにできる——ゾーンを別のツールや人も書き、シリアルの採番をそちらに任せる環境のため。Apply は 1 操作ずつなので、1 周で複数のレコードを変えるとシリアルも複数回上がる（単調増加は保たれる） | §27.2・§28.4 |
| **Redfish の BMC provider**（Backlog #48） | 物理（電源/BMC）層の最初の provider の範囲と、電源・起動先・仮想メディアの収束の決まり。**v3.15 で決定（2026-10-22 人が決定）、v3.16 で仮想メディアの ID を決定**：（1）**Redfish だけ**——IPMI は持たない（途中の状態〈PoweringOff / PoweringOn〉が見えず、Converging を返せないため。IPMI だけの BMC が要れば別の provider にする）。HTTP は Go の標準ライブラリで書き（gofish は使わない）、Basic 認証のパスワードは `SecretRef`（§20）で渡す。自己署名の証明書は明示したとき（CA ファイルか `insecure_skip_verify`）だけ許す。Reset・InsertMedia・EjectMedia は 204 を期待し、Redfish の Task（202）は使わない。（2）**リソース**：ノードごとに「システム」（電源・起動先の上書き、provider `redfish-system`）と「仮想メディア」（CD、provider `redfish-virtual-media`）の 2 つ。**システムの ID はノード名**（物理トポロジの座標、§28.4）、**仮想メディアの ID は `<ノード名>/cd`**（例 `node1/cd`。v3.16 で決定）。Local ID は provider をまたいで intent 全体で一意（重複はロードで拒む。depends_on の名前でもある）なので、両方をノード名にすると同じノードに並べて宣言できないため。BMC はノード名（仮想メディアは `/cd` の前の部分）で引き、BMC のアドレスは provider の設定に置く。仮想メディアの `name` フィールドはノード名を書く。サーバは Sakutei が作ったり消したりするものではないので List を持たず、prune の対象にしない。UUID・MAC・CPU 数・メモリ・起動モードは観測だけの値（Computed、Diff しない・識別子にしない）。（3）**電源**：`power: On / Off`。Off の観測で On を宣言していれば Reset `On`。On を Off にするのは `GracefulShutdown`。PowerState が PoweringOff / PoweringOn のあいだは Converging（操作を重ねない）、BMC に届かない・5xx・タイムアウトは Unknown（何も操作しない、unknown ≠ absent）。（4）**正常停止が効かないとき**：宣言の `power_off` で選ぶ。既定の `graceful` では ForceOff に進まない——止まらない間は GracefulShutdown を送り直し、ずれの level（§22.3）が続いて既定 5 周で stuck に上がることで人に知らせる。`power_off: graceful_then_force` と `force_after`（時間、必須）を宣言したときだけ、最初に GracefulShutdown を送ってから `force_after` を過ぎても Off にならなければ ForceOff を送る。ForceOff は**破壊的な操作として予算（§13）に数える**（予算で止まれば他の破壊的な操作と同じく保留）。GracefulShutdown を送った時刻はインメモリ（S3）に持ち、失えば数え直す（強制停止が遅れる側＝安全側）。GracefulShutdown・On は破壊的に数えない。再起動（ForceRestart・GracefulRestart）は「あるべき状態」ではなく一度きりの操作なので宣言できない（Task の領分）。（5）**起動先の上書き**：宣言できる `boot_enabled` は `Continuous` と `Disabled` だけ。**`Once` は計画の時点でエラー**にする（1 回起動すると BMC が Disabled に戻すため収束の対象にならない。Once は将来 Task 側で扱う）。`Disabled` のときは `boot_target` を比べない（BMC は設定した値を返し続ける）。`boot_target` と Reset の種類は BMC が出す `@Redfish.AllowableValues` で計画の時点に確かめ、使えないものはエラーにする。起動先の変更は PATCH だけで、再起動はしない。（6）**仮想メディア**（ID は `<ノード名>/cd`、（2））：`image`（URL か BMC から見えるパス。空なら取り出した状態）を宣言し、`Inserted`・`Image` を観測して比べる（`Inserted` が false なら image は空として比べる）。違う image が入っていれば同じ Apply の中で Eject してから Insert する。BMC の取得失敗で `Inserted` が false に戻れば、次の周にずれとして入れ直す（続けば level で stuck）。仮想メディアの操作は破壊的に数えない。（7）**統合テスト**（§26.1 の 3 段目）：qemu-bmc（v0.8.0 以降）を containerlab で 2 台立て、KVM のある機械で `make` の別ターゲットとして回す（通常の CI には入れない）。 | §5・§13・§22.3・§28.4 |
| **単純 Task の実 provider：sqlmigrate**（Backlog #92） | v1.5 の単純 Task（§6.2、`strategy: task`）を実際に動かす最初の本物の provider。**v3.17 で決定（2026-10-23、人の依頼で AI が選んだ）**：§6.2 の単純型の典型（migration、完了印を現実側の `schema_migrations` に置く）をそのまま実装する。（1）**対象は SQLite のデータベース**（ファイル）で、Go の標準ライブラリだけの決まりを守るため、ドライバを入れずに `sqlite3` の CLI を exec で呼ぶ（CLI が無い・起動できないときは Unknown）。PostgreSQL など他の DB は同じ形で後から足せるが、今回は持たない（PostgreSQL は #126 で足した。下の「sqlmigrate の PostgreSQL 版」）。（2）**リソース**：ID はデータベースの名前（provider の設定に、ファイルのパスとマイグレーションのディレクトリを置く）。宣言するのは `version`（整数、マイグレーションの版）だけ。観測は `schema_migrations` の最大の版（表が無ければ 0）。List は持たず、prune の対象にしない。（3）**`strategy: task` 専用**：収束ループの Apply は拒否する（`strategy: task` でない宣言は計画の時点でエラー）。差分を見つけると Task を起動し、実行体（task.Runner）が `NNNN_<名前>.up.sql` を今の版の次から宣言の版まで順に、1 本ずつトランザクションで流し、同じトランザクションで `schema_migrations` に版を書く（完了印）。宣言の版が今より小さいときは `NNNN_<名前>.down.sql` を逆順に流す（無ければ失敗）。（4）**失敗**：途中の 1 本が失敗すると、その 1 本は巻き戻り（トランザクション）、それより前に通した版は残って Task は失敗＝halt（§6.2）。`rollback_on_fail: true` のときは、起動したときの版まで down で戻す。（5）完了の判断は Read（`schema_migrations`）で行う——Task の記録は「起動した」ことしか持たない（§6.2 の「完了は現実を Observe して問う」）。 | §6.2・§28 |
| **allocator Lookup キャッシュの TTL 化**（Backlog #105） | §19.6 は「割当事実は Release まで不変だから `Lookup` は無期限キャッシュしてよい」としていたが、v1 には `POST /v1/release`（手の解放、§19.9 の縮退から外の運用経路）という「自分の Release」ではない形で割当事実を変える経路があり、無期限キャッシュのままだとこの経路で解放された値をシャードが知らずに使い続け、cooldown 明けに別 owner へ再払い出しされて衝突しうる（§19.5 が最悪級と呼ぶ事故）。**v3.19 で決定（2026-10-24 人が決定。案B＝キャッシュに TTL を入れて定期的に裏で確かめ直す、の折衷）**：Lookup キャッシュ（`internal/allocator.Cache`）のエントリに確かめた時刻を持たせ、**TTL（既定 `DefaultLookupTTL` = 15 分、cooldown の既定 24h より十分短い）を超えたエントリは、使う前に `Lookup` で 1 回だけ確かめる**。authority にまだ同じ owner の記録があればその値を使い続け（値が動いていれば更新）、無くなっていれば（他所で解放された）キャッシュを捨てて新たに `Allocate` する。authority に届かないときは確定した無効化の証拠が無いので、度に古い値を使い続ける（§19.9 の waiting-allocation は新規の採番だけに掛かる）。仮予約（`Reserve`）は元から未キャッシュのまま。定常状態のコール数は依然「周回数」ではなく「churn + 1/TTL」に比例する——secret の last-resolved + TTL キャッシュ（§20.3）と同型になった | §19.6 |
| **dnszone / nftables の scope 固定**（Backlog #106） | dnszone・nftables provider の `List` / `BulkObserve` は常に root scope（ラベル無し）で ID を返していたため、`PruneWithinScope`（§12.2）で非 root の scope を宣言すると `inAnyScope` が一致せず orphan が境界の外（open-world）として扱われ**消えないまま黙って残る**（prune が効かない）。同様に Sync Mode（§24.5）が非 root の sync scope を宣言すると importer の `Selector.Matches` が常に false になり、**generated/ のスナップショットが常に 0 件になる**。**v3.19 で決定（2026-10-24 人が決定。案A＝provider 設定で固定 scope）**：provider インスタンスごとに固定の scope を 1 つ設定できるようにし（`dnszone.WithScope` / `nftables.WithScope`、CLI は `--zone-scope` / `--ruleset-scope`、`--claim` と同じ `key=value,...` 書式）、設定されていれば `List` / `BulkObserve` が返す全 ID をその scope で返す（呼び出し側が渡す scope 引数は無視する——1 インスタンス 1 scope に固定するため、動的な切替は持たない）。未設定の既定は従来どおり root。これにより非 root scope の `PruneWithinScope` と Sync Mode が、provider 側の scope 設定を正しく合わせれば動くようになる（合わせ忘れれば従来どおり orphan が残る／0 件になるので、設定の運用規律は利用者の責任） | §4.2・§12.2・§24.5 |
| **nftables の実の ruleset 版（live）**（Backlog #121） | nftables provider は管理された ruleset ファイルだけを読み書きし、カーネルの FW を変えず、FW を直に変えても drift として見えなかった。**決定（2026-10-27）**：（1）provider に**対象**の設定を足す——`file`（今のまま、既定）と `live`（カーネルの実際の ruleset）。CLI は `--ruleset-target file\|live`、live では管理する table の一覧（`--ruleset-tables <family>/<name>,...`、**必須**。推測しない）と nft のパス（`--ruleset-nft`、既定 `nft`）を受け、`--provider-config` では同じものを `ruleset_target`・`ruleset_tables`・`ruleset_nft` のキーで渡せる。Schema・identity {family, table, chain}・local id・Ordered の rules は file 版と同じで、intent は対象を変えてもそのまま使える。（2）**観測**は table ごとに nft を 2 回——`nft -j list table` の JSON で構造（chain の並び・base かどうか・ルールの handle の並び）を、`nft -a -s list table` のテキストで handle ごとのルール文を読み、handle で突き合わせる（合わなければ読む間に変わったのでその周は観測できない）。table が無いのは「空」、nft が走らない・権限が無い・出力が読めないのは**観測できない**（unknown、absent にしない、§3・§5.4）。List・BulkObserve は管理する table を全部読めたときだけ complete。（3）**正規形**：nft が書き戻すテキスト（`-s` でカウンタの値と quota の used を落とした形）の、引用符の外の空白を 1 つにそろえたもの。宣言も同じ関数で正規形にして比べる（PlanRefiner が空白・カウンタの値だけの差分を落とす）。nft は宣言を自分の形に書き換えることがある（例 `ct state new,established` → `established,new`）ので、Apply は流した後に読み戻し、正規形が宣言と違えば「nft が並べる形で宣言せよ」と nft の形を添えた PermanentError を返す——**永久に drift が出続ける宣言を黙って受けない**。（4）**Apply** は対象の chain の中身を入れ替えるスクリプト（`flush chain` と `add rule` の列。Create は `add table`・`add chain`、Delete は `flush chain`・`delete chain`）を作り、`nft -c -f -`（検査だけ）を通してから `nft -f -` で **1 つのトランザクション**として流す（途中で失敗すれば何も変わらない）。検査で断られたのは PermanentError、権限・EBUSY と流す段の失敗は TransientError。同じホストの Apply はプロセス内で直列化する。（5）**管理境界**（§7、Backlog #68 と同じ）：base chain（type/hook/policy を持つ）・set・map・名指ししていない table は読まず書かない。ルール文はスクリプトにそのまま埋め込むので、引用符の外の `;`・`#`・改行は読み込みの時点（IntentValidator）と Apply で断る（ほかの table を変えるコマンドを注入させない）。（6）nft を呼ぶ所は seam（`Runner`）に切り、通常のテストは偽の nft で契約テスト・BulkObserve 等価性テストを回す。本物の nft は統合テスト（build tag `integration_nftables`、`make integration-nftables`。`sudo -n unshare -n` の使い捨ての network namespace の中で回し、ホストの FW に触れない。root が要るので check・test・CI には入れない）。使い方は `docs/providers/nftables.md` | §5・§7・§26 |
| **S3 互換のバケット provider**（Backlog #122） | オブジェクトストレージのバケットを宣言で収束させる provider の範囲と決まり。**決定（2026-10-27）**：（1）**S3 互換の API だけ**を使い、特定の製品に依らない。アカウント・利用者・アクセスキーの管理は製品ごとに違うので持たない（要れば外の provider で足す、§28.9）。オブジェクトを読み書き・削除する操作も持たない。HTTP は Go の標準ライブラリで書き、AWS Signature Version 4 を自前で実装する（AWS の公開のテストベクタで単体テスト）。アドレスは path-style が既定で、virtual-host は設定で選ぶ。region は設定（既定 `us-east-1`）。（2）**接続は名前付きの endpoint の一覧**（`--s3-config <file>`、または `--provider-config` の `s3-bucket` の `s3_config`）。1 つの provider で複数の endpoint を扱えるようにし、**ID は `<endpoint の名前>/<bucket>`**（Redfish のノード名と同じく、どこにあるかを ID の前半に持つ）。endpoint ごとに `endpoint`（scheme は必須で `http` / `https`。推測しない）・`region`・`addressing`（`path` \| `virtual`）・`access_key` と `secret_key`（**どちらも SecretRef**。file SecretBackend から Read・Apply・List の直前にだけ解決し、値を設定ファイル・intent・ログ・エラーに出さない、§20）・`ca_file`・`insecure_skip_verify`（明示したときだけ）・`timeout`、加えて `bucket_prefix`（その接頭辞のバケットだけを扱う。List もそれだけを返すので、PruneWithinScope の閉じた世界をアカウントの全バケットではなくこの接頭辞に限れる）。設定ファイルの `scope` は List・BulkObserve が ID に付ける固定の scope（#106 と同じ）。（3）**リソースは 1 バケット**、完全の階級（Read・Apply・List・BulkObserve）。BulkObserve は endpoint ごとに ListBuckets を 1 回と各バケットの設定の読みで、Read と等価（SDK の等価性テストで確かめる）。ListBuckets が失敗した endpoint のバケットは結果に入れず（SDK が Read に回し、Read が Unknown にする）、1 つの endpoint が止まってもほかの endpoint の観測は続ける。全 endpoint が失敗したときだけ error。フィールドは `versioning`（`Enabled` / `Suspended`）・`policy`（JSON）・`lifecycle`（ルールの JSON の配列。各ルールは S3 の XML の Rule をそのまま JSON にした形で、`ID` と `Status` を必須にする——ID を省くと endpoint が作った ID で永久に一致しないため）・`tags`（JSON のオブジェクト）で、**宣言したものだけを管理し、書かないものは触らない**。空（`""`・`[]`・`{}`）の宣言は「無い」ことで、ポリシー・ルール・タグを外す。`endpoint`・`bucket` は identity（ID と同じ値で省略可）。観測だけの値は `creation_date` と `unsupported`。（4）**正規形で比べ、永久 drift を出さない**。policy は JSON を読み直してキー順・空白を捨て、IAM で意味が同じ書き方をそろえる（Statement の 1 オブジェクト→配列、Action・Resource 等と Principal・Condition の値の文字列→並べた配列、Principal の `"*"` ＝ `{"AWS": ["*"]}`。Statement の順は変えない）。lifecycle は値をすべて文字列に（`30` と `"30"` は同じ）、日時を RFC 3339 の UTC に、空の値を落とし、ルールと配列の順を正規形の順に並べる。tags はキー順。比べ方は PlanRefiner で、宣言の正規形が観測と同じなら差分から落とす。Apply の後に読み戻し、endpoint が宣言と違う形で持つ（正規形でもそろわない）なら、その形を添えた PermanentError にする（Apply を繰り返さない）。（5）**endpoint がその API を持たないとき**（501 か Code が `NotImplemented`）：Read はそのフィールドを State に入れず `unsupported` に挙げる。宣言していれば計画の時点で「観測できない（NotImplemented の API の名前つき）」として止め（RefinePlan のエラー。常駐では観測できないとして数え、`status` も観測できないとして報告する。一回性の `plan`・`reconcile` はほかの provider の計画のエラーと同じくその文を出して終了コード 1 で止まる）、宣言していなければ無視する。GET は既定の値を返すが PUT だけ NotImplemented の endpoint（例：古い SeaweedFS）では Read は欠けを知れないので、drift として見え、Apply が「その API を持たない」と分かる文の PermanentError で止まる。（6）**届かない・時間切れ・5xx（501 を除く）・認証の失敗・資格を解決できない・証明書の検証失敗は観測できない**（Unknown。absent にしない）。リダイレクトは辿らない（region・addressing の誤りとして知らせる）。List は全 endpoint の ListBuckets が ContinuationToken を最後まで辿れたときだけ ListComplete で、1 つでも失敗すれば error（prune は止まる）。（7）**削除**：Pruneable は true——中身のあるバケットは endpoint が消させない（`BucketNotEmpty`）ので、prune で失うのは空のバケットだけ。prune は PruneWithinScope を宣言したときだけ起き、Delete は破壊的として予算に数える（§12・§13）。**オブジェクトを消す機能は持たず**、`BucketNotEmpty` は「中身があるので消さない」と分かる文の PermanentError として知らせる。（8）統合テストは build tag `integration_s3`（`make integration-s3`、check・CI には入れない）で、docker で立てた MinIO と SeaweedFS（と PUT の API を持たない古い SeaweedFS）に向けて CLI を回す。（9）**S3 互換実装との差異への対応**（Backlog #132、2026-10-27）：501・NotImplemented・policy の書き換え・読み戻しの不一致は（4）（5）のとおり provider が吸収する。それ以外は endpoint の設定で知らせる——`fields`（その endpoint で扱う設定の一覧、既定は 4 つ全部）に無い設定は Get を呼ばず（API を持たないことを 400・403 などで返す実装で、宣言していない設定の観測エラーがバケット全体を観測できなくしないため。Read は宣言を知らないので設定で知らせる）、観測では `unsupported` に挙げ、intent で宣言すれば読み込みの時点で「この endpoint は扱わない」と分かるエラー（IntentValidator。まだ無いバケットにも効く）。`storage_class_aliases`（実装が返す名前 → 標準の名前）は観測した lifecycle の StorageClass を読み替えてから比べ、Apply の読み戻しも同じ表で読み替える（受け取った StorageClass を独自の名前で返し、PUT では標準の名前しか受け付けない実装で、どちらで書いても毎周 drift になるのを防ぐ）。intent には標準の名前で書く | §5・§12・§20・§28.9 |
| **PostgreSQL provider**（Backlog #125） | PostgreSQL のロール・データベース・データベースの権限・拡張を宣言で収束させる provider の範囲と決まり。**決定（2026-10-27）**：（1）provider 名は `postgresql`。Go の標準ライブラリだけの決まりを守るため、ドライバを入れずに **`psql` の CLI を exec で呼ぶ**（sqlmigrate と同じ形）。シェルは通さず、引数は固定のオプションだけ、SQL は標準入力で渡す。接続の設定は子プロセスの環境変数で渡し、親の `PG*` は引き継がない。名前は識別子として常に `"..."` で、値は `E'...'` で quote する（自前の quote_ident・quote_literal）。読む問い合わせは名前を埋め込まない決まった文で、結果を 1 つの JSON にして返させる。エラーは SQLSTATE で分ける。（2）設定は名前付きのサーバの一覧（`--postgresql-config`、または `--provider-config` の `postgresql_config`）：host・port・dbname（管理用につなぐ DB、既定 postgres）・user（管理用のロール、必須）・password（SecretRef。psql を起動する直前に解決し、0600 の一時的な passfile で渡して終われば消す。引数・ログ・エラーに出さない、§20）・sslmode・sslrootcert・connect_timeout・scope・verify_passwords。（3）**ID** は `<server>/role/<name>`・`<server>/database/<name>`・`<server>/database/<db>/grant/<role>`・`<server>/database/<db>/extension/<name>`。名前は 1〜63 バイトで `/`・NUL・制御文字を含まない（データベースは `=` も含まない。libpq が接続文字列と読むため）。1 つの Schema に全種類のフィールドを持ち、種類に合わないフィールドは読み込みの時点で断る。（4）**フィールド**（宣言したものだけを管理）：ロールは login・superuser・createdb・createrole・inherit・connection_limit・member_of（集合。宣言すれば一覧ちょうどにする）・password、データベースは owner・connection_limit と、作るときだけの encoding・lc_collate・lc_ctype・template（encoding・照合順序を後で変える宣言は計画のエラー。template は観測できないので作った後は比べない。符号化名・ロケールの codeset は正規化して比べる）、権限は privileges（CONNECT・CREATE・TEMPORARY の、そのロールが**直に**持つもの。空にはできず、全部外すなら宣言から外して消す＝REVOKE ALL。1 つのトランザクションで REVOKE ALL と GRANT）、拡張は version（宣言したときだけ ALTER EXTENSION ... UPDATE TO）。**スキーマ・表・列の権限と default privileges、ロール・データベースの設定は持たない**（要れば別の provider か Task で足す）。（5）**パスワード**：観測できないので、Apply は値を **SCRAM-SHA-256 の検証値**にしてから渡し（平文をサーバに送らない。ASCII だけ。SASLprep は持たない）、同じトランザクションでロールのコメントに `sakutei:password=<SecretRef の参照>`（値ではなく参照）を書く。Read はこの参照を password として返すので、宣言の参照（世代を含む）が変わったときだけ差が出る（永久 drift を出さない。世代を上げれば cause E で設定し直す）。サーバに `verify_passwords: true` を明示したときだけ、Read はさらに `pg_authid` の検証値がその参照の値と合うかを確かめ（超ユーザが要る。このときだけ Read でも値を解決する）、合わなければ password を空として返す（手で変えたパスワードが drift として見え、戻る）。（6）**管理の境界**（§7）：List は `pg_` で始まるロール・管理用のロール・template0・template1・postgres・管理用の DB とその中の権限・拡張、データベースの所有者の権限、plpgsql を返さない（prune で消さない）。これらに向けた宣言は読み込みの時点で断る（plpgsql の宣言はできる）。（7）Apply は宣言の値とその場で読み直した現実から文を組み立て（冪等）、適用した後に読み戻して、宣言と違う値で残れば PermanentError。`CREATE DATABASE` は 1 文だけで流す。（8）**削除**：Pruneable は true、Delete は破壊的として予算に数える（§12・§13）。`CASCADE`・`REASSIGN OWNED`・`DROP OWNED` は使わない。**利用者の表（拡張が持つ表を除く）があるデータベースは消さない**（PermanentError。接続を許さないデータベースも数えられないので消さない）。オブジェクト・権限を持つロール、頼られている拡張は PostgreSQL が断り、それを PermanentError で知らせる。（9）**届かない・時間切れ・認証の失敗・psql が無い・管理用のパスワードを解決できないは観測できない**（Unknown。absent にしない）。List は全サーバ・全データベースの拡張が読めたときだけ ListComplete。BulkObserve はサーバごとに 1 回の snapshot とデータベースごとの拡張の読み（Read と等価）で、止まったサーバのものは Read に回す。（10）偽の psql（seam）で契約テスト、統合テストは build tag `integration_postgresql`（`make integration-postgresql`、check・CI には入れない）で、docker の公式の postgres イメージ（tag と digest で固定）に向けて CLI を回す | §5・§7・§12・§20・§28.9 |
| **sqlmigrate の PostgreSQL 版**（Backlog #126） | 単純 Task の実 provider（sqlmigrate）を PostgreSQL のデータベースにも向ける。**決定（2026-10-27）**：（1）データベースごとに `engine`（`sqlite`＝既定・今のまま／`postgresql`）を持つ。`postgresql` のデータベースは `server`（サーバの名前）・`database`（つなぐデータベース）・`migrations` を書き、サーバは sqlmigrate の設定の `postgresql_config` が指す **postgresql provider と同じサーバの一覧**から引く。psql の呼び方（固定の引数・`ON_ERROR_STOP=1`・環境変数で渡す接続の設定・親の `PG*` を引き継がない）と管理用のパスワードの渡し方（JIT に解決して 0600 の一時的な passfile）は postgresql provider と共通の口を使い、二重に書かない。（2）**決まりは SQLite 版と同じ**：観測は `schema_migrations`（`version bigint PRIMARY KEY, applied_at timestamptz`、search_path で引く）の最大の版（表が無ければ 0）。up は 1 本を 1 回の psql の呼び出しで `BEGIN … その SQL … INSERT INTO schema_migrations … COMMIT` として流し（失敗した 1 本は残らない）、down は同じ形で `DELETE`。RollbackTask、Apply の拒否、`strategy: task` でない宣言のエラーも同じ。届かない・認証の失敗・psql が無い・パスワードを解決できない・**データベースが無い**は観測できない（Unknown。版 0 にしない。データベースは sqlmigrate が作らない——作るなら postgresql provider で宣言して `depends_on` で待たせる）。（3）**複数のデーモン**：同じデータベースを複数のデーモンが同時に動かさないよう、1 本ごとのトランザクションの頭で `pg_advisory_xact_lock`（固定の鍵。advisory lock はデータベースごとの名前空間）を取り、取れたら `schema_migrations` が流す前の版のままかを同じトランザクションで確かめる（違えば SQLSTATE 55000 で、何も流さずに失敗＝halt）。待たされた側は先の側が進めた版を見て止まり、同じ版を二重に流さない。セッションの鍵（`pg_advisory_lock`）にしないのは、1 本ごとに psql を起動し直すため（鍵の範囲を 1 本のトランザクションにそろえる）。（4）**トランザクションに入れられない文**（`CREATE INDEX CONCURRENTLY`・`VACUUM`・`CREATE DATABASE` など）は特別扱いせず、PostgreSQL のエラー（25001）をその 1 本の失敗として知らせる（その 1 本は巻き戻る）。要るなら Task の外で人が流すか、別の Task にする。（5）**psql のメタコマンド**（文字列・引用符の識別子・dollar quote の外の `\`。`\!` はシェルを起動する）を含むファイルは流さない（流す前にエラー。コメントの中の `\` も psql との読み違いを避けるため断る）。SQLite 版のドットコマンドの禁止と同じ趣旨。（6）単体テストは偽の psql（Runner の seam）で、統合テストは `make integration-postgresql` の同じコンテナで、常駐の sakutei（`--task-store`）が 0→3・3→1 の down・失敗する 1 本・`rollback_on_fail`・25001・2 つのデーモン（テストが鍵を持つ間は 2 つとも待ち、外すと片方だけが流し、もう片方は 55000 で止まる）を通す | §6.2・§20・§28 |
| **Ansible provider**（Backlog #127） | §28.7 の 2 層 provider 戦略のロングテール層の範囲と、最小の階級（§5.6）の観測の口。**決定（2026-10-27）**：（1）**provider 名 `ansible`**、`ansible`（ad-hoc）を os/exec で呼ぶ（シェルを通さない。パスは設定）。借りるのはモジュールだけで、playbook と一回 push の実行モデルは借りない。（2）**リソースは「1 つのホストに対する 1 つのモジュールの呼び出し」**。ID は利用者が付ける名前。フィールドは `host`（inventory のホスト名。パターン・`all`・`ungrouped` は断り、適用の前に `--list-hosts` でちょうどそのホスト 1 台を指すことを確かめる）・`module`（FQCN）・`args`（JSON のオブジェクトの文字列。値の中に SecretRef を書け、実行の直前にだけ file SecretBackend から解決して渡す。引数はコマンドラインに載せず 0600 の一時ファイルの extra vars で渡し、文字列は `!unsafe` でテンプレートとして評価させない。解決した平文は知らせの中で伏せ、SecretRef を使う呼び出しでは `--diff` を添えない）・`destructive`（既定 false）。`host`・`module` が identity。（3）**観測の口を core に足す**：最小の階級は State を返せないが、desired を与えれば「変わるか」を答えられる。`provider.CheckReader`（`ReadCheck(ctx, id, desired)`）を任意の capability として足し、観測は `core.Observation.Check`（`Changed` と、相手の差分 `Detail`）で返す（State は nil）。desired が観測に入るのはこの口だけで、問いとして相手に渡すだけ（比べるのは相手、何も変えない）。`TrueObserve: true` の provider が CheckReader を持てば ValidateProvider が断る（ADR-0002 を崩さない）。コアは desired のある観測の経路（常駐・scoped・plan・reconcile・status・workflow の確認）で `provider.ReadFor` を使い、desired の無い Read（ready_condition の上流など）では CheckReader は観測できない（Unknown）。（4）**縮退した diff**：Check が changed でなければ適合、changed なら desired の全フィールドを From 空で持つ Update を 1 つ（Create にしない。相手は答えたので absent とは言えない）。理由に相手の差分を添え、DriftEvent にも 1 行で添える。原因タグは新しいタグを足さず、intent の版の変化だけで A、ほかは B（現実からは区別できない）。Destructive はコアでは false で、provider の PlanRefiner が `destructive: true` の呼び出しだけを上げる——§5.3 の「分からなければ破壊的側に倒す」をそのまま当てると、すべての適用が破壊予算を食って行の追加まで止まり使えないため、重み付けは利用者の宣言に委ねる。（5）**観測できない**（Unknown。作りも変えもしない）：届かない（unreachable）・check mode を持たないモジュール（skipped）・失敗・ansible が走らない・時間切れ・ホストが inventory に無い。（6）**List を持たない**（prune しない）。`PruneWithinScope` は List を持たない provider には読み込みの時点で断る——この拒否は §5.3・§12.1 にあったが一回性・常駐とも Lister のときにしか検査していなかったので、読み込みの検査（`resolveProviders`・常駐の周の頭）に足した。Delete も来れば permanent error（消すのはモジュールの引数、例えば `state: absent`）。（7）**結果の読み方**：ansible-core（2.21.5 で確かめた）は JSON の stdout callback を持たない（`json` は ansible.posix）ので、ホストの結果ごとに 1 行の JSON を出す小さな callback を自前で持ち（Go のバイナリに埋め込み、走るたびに一時ディレクトリに書く）、`ANSIBLE_STDOUT_CALLBACK`・`ANSIBLE_LOAD_CALLBACK_PLUGINS=1`・`ANSIBLE_CALLBACK_PLUGINS` で読ませる。判断は終了コードでなく結果の行で行い、結果がちょうど 1 つで host 自身のものでなければエラー。（8）**設定**（`--ansible-config`、または `--provider-config` の `ansible` の `ansible_config`）：`inventory`（必須）・`ansible`・`secret_store`・`env`（結果を読むための変数は変えられない）・`timeout`（既定 5m、超えたらプロセスグループごと止める）・`max_parallel`（同時に走らせる ansible の数、既定 4）。（9）適用の失敗は届かない・ふつうの失敗を transient、ansible が宣言そのものを断ったもの（`Unsupported parameters`・`missing required arguments`・`Cannot resolve` など）を permanent。（10）統合テストは build tag `integration_ansible`（`make integration-ansible`、check・CI には入れない）で、版を固定した venv の ansible-core を local 接続で一時ディレクトリに向けて CLI を回す | §5・§13・§16.5・§20・§28.7 |
| **Workflow の手順が触る provider と前提条件**（Backlog #87） | 最小 Workflow（§6.3）の target は provider 1 種類で、OS の導入の「CD（`redfish-virtual-media`）を差す → CD 起動で電源オン（`redfish-system`）」の順を 1 つの Workflow に書けなかった（2026-10-22 の試用）。**決定（2026-10-27）**：両方入れる。（1）`apply`・`wait`・`require` の手順に `resource: <provider>/<id>`（`{target}` は今の対象の Local ID、scope は target.scope）を書け、その手順はそのリソースをその provider で Observe・Diff・Apply する。`apply` は `{ resource, fields }`、書かなければ今までどおり対象（今の書き方はそのまま）。`task` は対象だけ（多相 Task は v2）。（2）前提条件の手順 `require: { resource, fields, timeout }`：観測で満たすまで進まず、`timeout`（必須）で halt。（3）手順が触るリソースは target と同じく done まで収束ループが触らず（plan・status は「Workflow に任せる」）、2 つの Workflow から触る宣言・`strategy: task` のリソースを触る宣言はロード時に拒否。承認ゲート・1 台ずつ・`min_active`（target だけで数える）・S2 の進行位置・再開の規則は変えない。偽 BMC の受け入れテストと `make integration-redfish`（qemu-bmc）で「CD が差さってから電源が入る」を確かめる | §6.3 |
| **離れたホストを扱う口：ssh 越しの実行の層**（Backlog #130） | nftables（live）と dnszone はデーモンの動く機械の nft とファイルだけを扱い、複数のホストの FW・DNS を扱うにはホストごとにデーモン（と秘密）を置くしかなかった。**決定（2026-10-27）**：（1）provider の外部コマンドとファイル操作を「手元」か「ssh 越し」で流す**実行の層**（`internal/remote` の `Executor`）を置く。ssh は **OpenSSH の `ssh` CLI** を os/exec で呼ぶ（Go の依存を増やさない）。引数の列は各要素を shell-quote した 1 本の文字列にし（リモートのシェルが解釈するため。sh・bash・dash・zsh で 1 語に戻ることを試験する）、中身（ゾーンファイル・nft のスクリプト）は標準入力で渡す。（2）ssh は利用者の設定を読まず（`-F /dev/null`）、`BatchMode=yes`・`ConnectTimeout`・`ServerAliveInterval` を付ける。**ホスト鍵は設定の known_hosts で確かめ（必須）、知らない鍵は拒む**。初回だけ信じる（`accept-new`）のは `trust_on_first_use` を明示したときだけで、違う鍵は常に拒む。接続は対象ごとの master（`ControlMaster`、`ControlPersist`）に相乗りして周ごとの接続を減らす。ソケットは自分の uid の 0700 のディレクトリに置く。（3）**秘密鍵は SecretRef（file SecretBackend）だけ**で書け、ssh を起動する直前に解決して 0600 の一時ファイルに出し、終われば消す（書かなければ ssh-agent）。鍵を引数・ログ・エラーに出さない（§20）。root が要るコマンドには設定で `sudo -n --` を付ける。（4）**届かない・認証の失敗・ホスト鍵の不一致は上の provider で観測できない（unknown）**にし、absent にしない（§3・§5.4）。ホスト鍵の不一致は設定を直すまで治らないので PermanentError で知らせる。失敗したホストには ConnectTimeout の間は新しくつながない（届かないホストの資源ごとに待たない）。（5）対象の一覧（名前・address・port・user・秘密鍵・known_hosts・sudo・タイムアウト）はファイルに書き、`--provider-config` の `remote_config` で渡し、provider ごとに `ruleset_hosts`（nftables の live）・`zone_hosts`（dnszone）で名指しする。（6）**複数のホストを 1 つのデーモンで扱う形は「ID に対象の名前を含める」**（`fw1/inet/filter/web`、`ns1/www.example.com./A`）に決めた。「provider のインスタンスを対象ごとに分けて scope（#106 の固定 scope）で分ける」形は、registry が provider の名前ごとにインスタンスを 1 つしか作らず、`Read` は local id しか受けない（scope で振り分けられない）ので、コアを変えずには成り立たない。フィールドは変えず、ホストは ID にだけ書く。対象を設定しない今の使い方は ID も動きも変えない。（7）**1 つのホストが観測できないとき**：List は全部のホストを読めたときだけ complete（1 台でも読めなければ prune を止める）。BulkObserve はホストごとに complete-or-error とし、読めないホストの分だけを返さない（そのホストの資源は Read が unknown と答え、ほかのホストは収束を続ける。全部のホストが読めなければ error）。コアは BulkObserve の error で provider の全資源を観測できないとするので、1 台の停止で全ホストが止まらないようにこう決めた。（8）**dnszone のファイル操作**はホストの上で行う：読み出しは `<zone>.lock` の共有の flock の下で `cat`、Apply は `<zone>.lock` の排他の flock を Apply の間ずっと持つ（ホストの上のシェルが標準入力が閉じるまで持つ。こちらが落ちれば接続ごと外れる。同じホストで動く sakutei とも排他）、書き込みは同じディレクトリの一時ファイル → rename で、置き換えるファイルのモードと持ち主を保つ。ゾーンファイルが無いのは、ディレクトリを辿れるときだけ「空」とする（辿れないのは unknown）。（9）dnszone に**再読み込みのコマンド**（`zone_reload`、例 `knotc zone-reload <zone>`・`rndc reload <zone>`。既定は無し、シェルを通さない）を足す。手元でも使える。Apply が実際にファイルを書き換えたときだけ走り、失敗は Apply の失敗として知らせ、次にそのゾーンへ Apply が来たときにもう一度走らせる。**既知の限界**：ゾーンが宣言どおりで Apply が来ない間は失敗した再読み込みを走らせ直さない（再読み込みの済み・未済は観測できる現実でなく、保持もしない）。失敗の Event を見たら人が直して読み直させる。（10）統合テスト（`make integration-remote`、check・CI の外）は docker の sshd・nft の入ったコンテナ 2 台で、空から収束・手の変更の drift と戻り・何周でも drift が出ないこと・再読み込み・sshd の停止／鍵を外す／ホスト鍵の変更で観測できず何もしないことを確かめる（docs/remote.md） | §3, §5.4, §7, §20, §28.9 |
| **PostgreSQL provider の PUBLIC の権限と bypassrls・replication**（Backlog #133） | データベースの権限はロールが直に持つものだけを見ていたので、新しいデータベースに既定で付く PUBLIC の CONNECT・TEMPORARY を宣言で外せず、手での `GRANT ... TO PUBLIC` も Converged のまま見えなかった。ロールの BYPASSRLS・REPLICATION も見えず、行単位のセキュリティ（RLS）で守る使い方の穴だった。**決定（2026-10-27）**：（1）**PUBLIC の ID** は `<server>/database/<db>/grant/PUBLIC`（grant の後ろの大文字ちょうどの `PUBLIC` を予約）。PostgreSQL は `public` という名前のロールを作らせないので、`public` と大文字小文字だけが違う名前は（`role/` の ID・grant の ID のどちらでも）ロールとしては断る（引用符で作った `"PUBLIC"` のようなロールは管理の境界の外）。SQL では引用符の無いキーワード `PUBLIC` で書く（quote すればロールの名前になる）。（2）**観測**：データベースの ACL の PUBLIC の分（`aclexplode(coalesce(datacl, acldefault('d', datdba)))` の grantee が 0 の行。datacl が NULL なら既定の CONNECT・TEMPORARY）。データベースが在れば PUBLIC の権限は**権限が 1 つも無くても在る**（privileges が空）。データベースが無ければ absent。（3）**宣言**：privileges は必須で、**PUBLIC に限り空の一覧（全部外す）を宣言できる**（通常のロールの「空にできない」は変えない）。宣言すれば一覧ちょうどにする（足りない分を `GRANT ... TO PUBLIC`、余分を `REVOKE ... FROM PUBLIC`、1 つのトランザクション）。明示の absent は `REVOKE ALL ON DATABASE ... FROM PUBLIC`（別のロールが与えた分が残れば PermanentError）。（4）**管理の境界**：PUBLIC の権限は List に出さない（宣言していない PUBLIC の権限を prune で外さない）。扱うのはデータベースの権限だけ（スキーマ・表の PUBLIC の権限は今までどおり持たない）。（5）**ロールの属性** `bypassrls`・`replication`（true / false）を足す。ほかの属性と同じく、宣言しなければ触らず、宣言すれば観測して直す（`ALTER ROLE ... [NO]BYPASSRLS / [NO]REPLICATION`。付けるには超ユーザが要る）。（6）偽の psql の単体・契約テストと、統合テスト（PUBLIC から全部外す宣言が新しいデータベースの既定の分ごと収束する・手での `GRANT ... TO PUBLIC` と `ALTER ROLE ... BYPASSRLS` が drift として見えて戻る・何周回しても drift が出ない）で確かめる | §5・§7・§12 |
| **ゲスト OS を観測する口と、OS 導入の完了の確かめ方**（Backlog #86） | 2026-10-22 の OS インストールの試用で、Workflow の完了の判断が「BMC から見て電源が切れた（`power: Off`）」だけで、インストーラが失敗しても電源を切れば成功と見なして次へ進むことが分かった。Sakutei はゲスト OS の中を見る手段を持たなかった。**決定（2026-10-27）**：（1）**観測だけの provider `guest-os`** を ssh 越しの実行の層（#130）の上に置く。ホストに agent は置かない。ID はホストの名前（`remote_config` の hosts の名前。Workflow の対象と同じ名前にすれば `resource: guest-os/{target}`）、対象は `guest_hosts` で名指しし、宛先と鍵は #130 の対象の一覧と同じ形で渡す。（2）フィールドは `os_id`・`os_version_id`・`os_pretty_name`（`/etc/os-release`、無ければ `/usr/lib/os-release`）、`hostname`・`boot_id`（`/proc`）と、**導入の目印** `marker.<名前>`（利用者が `guest_markers: <名前>=<絶対パス>,...` で決めるファイルの中身、末尾の空白を落とした値、4 KiB まで）。どれも Computed にしない（Workflow の wait・require が Diff で比べるため）。値を出すコマンドは目印にしない（任意のコマンドを観測で走らせない）。（3）1 回の Read は 1 本の ssh のコマンド（sh の小さなスクリプト。読むパスは引数、中身は `od` で 16 進にして返し、最後の行が無ければ途中で切れたとして unknown）。（4）**届かない・認証の失敗・ホスト鍵の不一致・読めない目印（ディレクトリ・権限・大きすぎる）・途中で切れた出力は観測できない（unknown）**で、absent にしない。目印のファイルが無いのは観測の事実（そのフィールドが無い）で、待ちの値とは一致しない。「まだ起動していない」と「壊れた」は観測からは見分けられないので、**見分けるのは Workflow の待ちの `timeout`（過ぎれば halt）**。（5）**観測だけ**：Apply は常に PermanentError、intent の resources に書けば ValidateIntent が断る（周を飛ばす）。ゲスト OS の中を変えるのは provisioner（§29.3 (c)、責務外）か別の provider。（6）**使い方**：OS 導入の Workflow は電源オフを待ってディスクから起動したあと、`wait: { resource: guest-os/{target}, fields: { os_id, marker.<名前>: <今回の導入の値> }, timeout }` で「ゲスト OS が目印つきで起動した」を待ってから先へ進む（目印の値を導入ごとに変えると、前の導入の OS を取り違えない）。provider は普通の Read を持つので、多相 Task（#51）の相ごとの入口の Observe にもそのまま使える。（7）Workflow のロードの検査（2 つの Workflow から同じリソースを触る宣言の拒否）が Local ID だけで比べていたため、1 つの Workflow で `redfish-system` の `node1` と `guest-os` の `node1` を触ると拒否されていた。provider・scope・Local ID の組で比べるよう直した。また Workflow の待ちの理由に、観測できない理由（provider の Read の error の頭、例 `remote node1: unreachable`）を添える（`waiting for map[…] (unobservable: <理由>; unknown ≠ absent)`。理由が変わったときだけ S2 に書く今の決まりのまま）。（8）統合テスト（`make integration-guestos`、check・CI の外）は qemu-bmc 0.10.0 を 3 台立て、Alpine の自動インストールの ISO（`docs/trial/redfish-os-install/make-iso.sh`。ゲストの sshd は QEMU の user ネットの hostfwd で BMC と同じアドレスに出す）で本当に OS を入れ、成功する ISO は目印を観測して先へ進み done、何も入れずに電源を切る ISO と目印を置かない ISO は電源オフの待ちを通ったあとゲスト OS の待ちの timeout で halt し、先の手順（CD を抜く）を施さないことを確かめる（docs/providers/guest-os.md） | §3, §5.4, §6.2, §6.3, §29.3 (c) |
| **OS の導入の手順と応答ファイル**（Backlog #139） | #87・#86 で OS の導入を 1 つの Workflow に書けるようになったが、（1）起動先の上書きは Continuous しか宣言できず（Once は収束しないので計画で断る、上の Redfish の（5））、導入のあとに上書きを戻す手順を利用者が書く必要があった。（2）自動導入の応答ファイル（ホスト名・アドレス・Sakutei の ssh の公開鍵・導入の目印）を宣言から作る口が無く、台ごとに ISO を作り直していた。**決定（2026-10-27）**：（1）**一度きりの操作は Workflow の手順 `action: { resource, op, args }` として書く**（§6.3）。収束の宣言で Once を断る決まりは変えない（エラーの文言は `action` を案内する）。provider は任意の capability `provider.Actioner`（`PlanAction`：現実を見ない検査と、破壊的かどうか。`Action`：1 回だけ施す。送っていないと分かる失敗は `ErrActionNotSent` で包む）を持つ。（2）**`redfish-system` の操作**：`boot_once`（`args.target` 必須。起動先の上書きを Once・target に PATCH するだけで再起動しない。target は BMC の AllowableValues で確かめ、PATCH のあと読み直して Once・target でなければ失敗）と `reset`（`args.type`：既定 GracefulRestart、ForceRestart。電源が On なら type で再起動、Off なら Reset On。電源の切り替えの途中・BMC に届かないときは送らない＝`ErrActionNotSent`）。**ForceRestart は破壊的**として変更予算（§13）に数える。GracefulRestart・boot_once は数えない。（3）**二度送らない**：送る直前に「送りかけ」の印を Workflow の S2（Run）に書き、送れて次へ進むときに消す。印が残ったまま読み直したら halt（台を確かめて cancel）。送っていない失敗は印を消して待ち、それ以外の失敗は halt。（4）**応答ファイルを宣言から作る provider `template-file`**：利用者の雛形（`text/template`、`template_dir`）に宣言の値（`hostname`・`address`・`ssh_authorized_keys`・`install_mark`・`var.<名前>`）を入れ、`output_dir` の下の ID（相対パス）に書き出す。置き場（ISO や HTTP）に入れるのは利用者の側。**最小階級**（§5.6、§28.7）：書いたファイルから宣言の値は読み戻せないので、`CheckReader` で「今の宣言で描いた中身とファイルが同じか」だけを答え、違えば（手で直された・雛形が変わった）書き直す。**秘密は書き出さない**（SecretRef と秘密鍵に見える値は読み込みで断る。公開鍵だけ）。List を持たず消さない。書き込みは一時ファイル → rename。OS の種類ごとの雛形は例として docs に 1 つ（Alpine、`docs/trial/redfish-os-install/templates/`）。（5）**OS の導入の例**：CD を差す → `require` → `action boot_once Cd` → `action reset` → インストーラが電源を切るのを待つ → 電源 On（上書きは使い切ったのでディスクから）→ `require guest-os`（目印と応答ファイルのホスト名、timeout で halt）→ CD を抜く。起動先の宣言は Disabled のままでよい。インストーラが電源を切るのは、qemu-bmc が Once を電源 On のときに使い切り、ゲストの中からの再起動では起動順を変えないため（実機でインストーラが再起動するなら、電源オフを待つ手順は要らない）。（6）統合テスト（`make integration-guestos` を広げる）：1 つの ISO の 3 台が、宣言から書かれた応答ファイルを DMI の UUID で取りに来て入り、成功する台は guest-os で目印とホスト名を確かめて CD を抜き done、失敗する 2 台は先へ進まず halt、3 台とも起動先の上書きは残らない（docs/providers/guest-os.md・docs/providers/template-file.md） | §5.6, §6.3, §13, §28.7, §29.3 (c) |
| **入れ直しを宣言で起こす**（Backlog #88） | 一度 done になった Workflow はもう走らず、入れ直すには `sakutei workflow cancel` か定義の書き換えしかなかった。planned_change の `kind: reimage`（§13.3）とつながっていなかった。**決定（2026-10-27）**：（1）Workflow に `trigger: reimage` を足す（§6.3）。対象の scope を覆う `kind: reimage` の宣言のレーンが開いている（窓・承認・cooling-off）ときだけ、宣言ごとに 1 回 run を起こす。run は起こした宣言の Key を S2 に持ち、同じ宣言では二度起こさない。宣言が無い間は対象を Workflow に持たせず、収束ループに任せる。（2）**予算**：対象ごとに手を付ける前に `max_count` を 1 つ使い、S4 の `planned_change.executed` に残す（S2 を失っても S4 の使用量で守る）。通常予算の破壊的変更に数えず、入れ直しの中の破壊的な一度きりの操作（ForceRestart）も通常予算に通さない。使い切った宣言は run を起こさず、走っている run の次の対象・宣言の取り下げ・窓の過ぎた後の次の対象は halt。（3）**承認**は planned_change の承認（§13.3・§14.4・§14.5）がそのまま効く。Workflow のステージのゲートも重ねられる。（4）動いている台を止めて CD から起動し直すのは #139 の `action reset`（入れ直しでは ForceRestart を勧める。ディスクを消すので正常停止を待たない）。（5）**見送り**：入れ直しの多相 Task 化（相の S2 永続化・入口 Observe は #51 が持つが、OS の導入は手順の並びが利用者ごとに違うので Workflow のまま）。走っている run を宣言の取り下げで止めること（対象の途中で止めると台が壊れたまま残るので、対象の切れ目で止める）。統合テストは `make integration-guestos` の TestReimage（qemu-bmc 0.10.0） | §6.3, §13.3, §14.4, §14.5 |
| **Redfish の BIOS の設定**（Backlog #140） | 空の物理サーバを使える状態にするのに BIOS の設定（仮想化支援・電力の型など）を宣言で合わせる口が無かった。BIOS の設定は書いてもすぐには効かず、次の再起動で効く（Redfish の `Bios/Settings`）ので、電源・起動先と同じ「書けば観測が変わる」収束にはならない。**決定（2026-10-27）**：（1）**リソース**：provider `redfish-bios`、**ID は `<ノード名>/bios`**（例 `node1/bios`。仮想メディアの `<ノード名>/cd` と同じく、Local ID が intent 全体で一意なので同じノードのシステム〈ID はノード名〉と並べて書くため）。標準階級で List を持たず prune の対象にしない。（2）**宣言した属性だけを管理する**：`attributes`（JSON のオブジェクト、値は文字列・数・真偽）に書いた属性だけを比べて合わせ、書かない属性は見ない・触らない。観測（Read）は desired を知らないので今の値の全部を返し、宣言した属性だけの比べ方は PlanRefiner（§28.4）で当てる（書かない属性の違いは差分から落とす）。（3）**保留中**：Read は `Bios` の Attributes（今の値）と、`@Redfish.Settings` の SettingsObject（`Bios/Settings`）の Attributes（保留中）を読む。保留中のうち今の値と違うもの（`pending_attributes`）か `ResetBiosToDefaultsPending` があるあいだは **Converging**（BMC は次の再起動を待っている途中なので、同じ変更を重ねて出さない。§11）。保留中が宣言と違っても重ねない（ほかの道具が置いた変更と取り合わない）。再起動で効けば次の周に今の値で比べ直す。Read は desired を見ない（ADR-0002）ので「保留中が宣言どおりか」で分けることはしない。（4）**Apply**：`Bios/Settings` に、宣言した属性のうち「保留中があればその値、無ければ今の値」と違うものだけを PATCH する。**再起動はしない**（反映は Workflow の手順の `action: reset` か人の再起動）。PATCH のあと読み直し、保留中にも今の値にも宣言の値が無ければ PermanentError（Settings を報告しない BMC に毎周 PATCH を重ねない）。BIOS の設定の変更は破壊的に数えない。（5）**計画の時点の検査**：Bios の `AttributeRegistry` の Id の登録簿を `/redfish/v1/Registries` から読み（Id ごとにインメモリ〈S3〉に持ち、Id が変われば読み直す。登録簿は BMC の版で決まる中身で、観測した現実の値ではない）、属性名・読み取り専用・型（Enumeration / String / Integer / Boolean）・選べる値・長さ・範囲が合わなければ計画の時点でエラー。Password 型の属性は宣言できない（§20）。登録簿を出さない BMC では、今の値に無い属性名と、今の値と JSON の型が違う値だけを断る。（6）**Bios を持たない BMC**：Read は Ok（`bios_present: false`）で、属性を宣言したときだけ計画の時点のエラー。届かない・5xx・時間切れは Unknown（何も操作しない）。（7）**統合テスト**：qemu-bmc 0.10.0 の Bios（値は文字列だけ、保留中は起動のたびに効いて空になる、登録簿あり）で `make integration-redfish` の 8 段目。 | §5・§11・§20・§28.4 |
| **Redfish の FW の更新**（Backlog #148） | 物理サーバの BMC・BIOS などの FW を宣言した版に合わせる口が無かった。FW の更新は中断できず、リブートをまたぎ、冪等でないので、収束ループの Apply では扱えない。**決定（v3.27、2026-10-28）**：（1）**リソース**：provider `redfish-firmware`、**ID は `<ノード名>/firmware/<部品の Id>`**（部品の Id は UpdateService の FirmwareInventory のメンバーの Id。例 `node1/firmware/BMC`。`<ノード名>/<Id>` にしないのは、Local ID が intent 全体で一意なので BIOS の `<ノード名>/bios` などと重なりうるため）。標準階級で List を持たず prune の対象にしない。（2）**宣言**：`version`（望む版）・`image_uri`（http / https。`transfer: multipart` なら file も）・`sha256`（任意）・`transfer`（`simple_update` 既定 / `multipart`。multipart は UpdateService が `MultipartHttpPushUri` を持つときだけ）・`apply_time`（`Immediate` 既定 / `OnReset`。BMC が `@Redfish.OperationApplyTimeSupport` を報告すればその中だけ）・`reboot`（`none` 既定 / `graceful` / `force`。#48 の電源の決まりと同じく GracefulRestart / ForceRestart、切れていれば On）・`update_timeout`（既定 1h）・`reboot_timeout`（既定 30m）・`image_username`・`image_password`（**SecretRef だけ**、§20。使う直前に解決し、ログ・エラー・S2・S4 に出さない。URI に資格を書くのはロードで拒否）。版以外は「どう合わせるか」で BMC に無いので、PlanRefiner で差分から落とす。（3）**版の比べ方**：前後の空白を除いた文字列の**完全一致**（大文字と小文字も区別する）。それ以上の正規化（`v` の有無・数としての比較）はしない——版の書き方は部品ごとに違い、上げ下げの順序は Sakutei が持たない。違えば上げるか下げるかを問わず更新する。（4）**strategy: task 専用**：`update_policy.strategy: task` でなければロードで拒否し、収束ループの Apply は拒む（§6.5）。`rollback_on_fail` は書けない（FW は自動では戻さない。戻すなら前の版を宣言する）。版の違いの Op は**破壊的**（§13 の予算に数え、超えれば Task を起こさない。planned_change の `kind: update` のレーンにも乗る）。（5）**相**（§6.2 多相型）：`verifying`（**承認の要る相**：入口で済んでいなければ人の承認〈`sakutei task approve`、§6.4・#52 と同じゲート〉を待ち、承認の後で `sha256` を書いていれば Sakutei がイメージを取りに行って確かめる）→ `staging`（SimpleUpdate を送る。multipart なら取りに行った中身を確かめてそのまま送る）→ `applying`（BMC の Task の完了を待つ。**危険区間**、#52）→ `rebooting`（`reboot` を宣言していればホストを再起動し、版が宣言どおりになるまで待つ。`none` なら待つだけ。**再起動を宣言したときは危険区間**：Reset を送る RunStep の中で落ちた Task は人手ゲート。RunStep が返ったことは S2 の記録に `awaiting` の印として置き、待ちの間に落ちた Task を引き継いだデーモンは入口の観測も RunStep もせずに待つだけ——OnReset で部品を書いている最中のホストに二度目の Reset を送らない）→ `confirming`（版を確かめる）。（6）**相の入口の観測**：版が宣言どおりならどの相も済み（Task は Settle で閉じる）。`verifying` は**版が宣言どおりのときだけ**済み（BMC の Task では済みにしない。承認の要る相を現実の残りかすで飛ばさない）。`staging` は**この更新の BMC の Task**（TaskService の Task のうち Payload の TargetUri が SimpleUpdate の target〈multipart なら MultipartHttpPushUri〉で、JsonBody の Targets にこの部品があり、SimpleUpdate なら ImageURI が宣言と同じもの。**StartTime を時刻として読み、この Task の起動の時刻〈S2 の記録の `started_at`〉から時計のずれの許し〈既定 5 分〉を引いた時刻より前に始まったものは数えない**〈前の更新の残りかすを「もう送った」と取らない〉。StartTime を読めない Task は数える〈自分の送った Task を見落として二重に送る側に倒さない〉。いくつもあれば StartTime の新しいもの）が失敗で終わらずにあれば済み（もう送った。二重に送らない）。`applying` はその Task が Completed（`OnReset` なら Pending も）なら済み。BMC に届かないのは unknown（BMC 自身の更新で再起動している。absent ではない）。Task の URI は持たない（観測 state レス）。送ったあとこの Task が観測できなければ `staging` で失敗（halt。Payload を返さない BMC には送り直さない側に倒す）。BMC の Task が Exception・Killed・Cancelled で終われば `applying` で失敗（halt）。（7）**TaskService を持たない BMC**は計画の時点のエラー（更新を終わりまで追えない）。UpdateService や部品が無いのも計画の時点のエラー（Read は Ok で `present: false`）。（8）**統合テスト**：qemu-bmc 0.10.0 は UpdateService・TaskService を持たないので、**統合テストは偽のサーバ（`internal/redfish/redfishtest`）まで**。偽のサーバは DMTF の仕様に沿って UpdateService・FirmwareInventory・SimpleUpdate・multipart の送信・TaskService を持つ。**既知の限界**：BMC の時計がこのホストより許し（5 分）を超えて遅れていると、自分の送った Task を前のものと見て数えない。送った直後の確かめ（`staging` の RunStep の後）で見えなければ halt するので送り直しは重ねないが、送ってから次の相を S2 に書く前に落ちたときは、引き継いだデーモンがもう一度送る。StartTime を読めない BMC では前の更新の残りかすを数えてしまう（`staging`・`applying` を飛ばし `rebooting` のタイムアウトで halt。承認は飛ばさない）。`sha256` の確認は Sakutei が取りに行った中身についてで、SimpleUpdate で BMC が取りに行く中身と同じとは保証しない（同じにしたいなら multipart）。承認は Task ごと・単一承認で、まとめて承認する口は無い。 | §5・§6.2・§6.4・§13・§20・§28.4 |
| **NETCONF の機器の設定の部分木の観測（観測だけ）**（Backlog #141） | NW 機器の設定を宣言と比べてずれを知らせる口が無かった。§23.4 は「CommitConfirm が無ければ Apply 禁止・観測のみを型で強制」と決めていたが、それを強制する型（Capabilities の宣言）がコアに無かった。**決定（2026-10-27）**：（1）**`Capabilities.ObserveOnly` を足す**（§5.3）。コアはその provider の差分を、定期周期・早期収束パス・一回性の reconcile のどれでも Apply に渡さず、ずれの知らせ（`DriftWouldApply`、デーモンの `observe-only provider(s): N drifted resource(s) reported — never applied`、plan の `blocked_by: observe_only`）にする。remediation mode が auto でも同じ。止めたリソースは収束していない上流として下流を待たせる。executor と router も ObserveOnly の provider への Apply を断る（最後の柵）。`List`・`Pruneable` との同時の宣言は起動時に拒否。（2）**provider `netconf`**（`internal/netconf`）：NETCONF over SSH（RFC 6241・6242）を、ssh 越しの実行の層（#130）の ssh で **netconf サブシステム**（`ssh -s`）として流す。サブシステムは往復するので Run でなく双方向の流れ（`remote.SubsystemOpener`）で開く。接続は ssh の多重化を使わず直につなぎ（多重化を受けない機器の ssh がある）、セッションをホストごとに 1 本使い回す（1 度に 1 つの rpc。使い回したセッションが切れていたら 1 度だけ開き直す。rpc-error ではセッションを捨てない）。区切り（1.0 の `]]>]]>`・1.1 の chunked。両方が base:1.1 を出したときだけ chunked）と XML は stdlib。hello の NETCONF の capability（base:1.0・1.1、candidate、confirmed-commit:1.0・1.1、validate など）を観測の `capabilities`（Computed）に記録する。（3）**リソースは「設定の部分木」**：ID は `<ホスト名>/<部分木の名前>`。**どこ（subtree filter の XML、最上位の要素は名前空間つき）は provider の側の部分木の一覧（`netconf_subtrees`）に、中身は intent の `config` に書く**（Read は desired を見ない、ADR-0002）。観測は `<get-config>`（running、subtree filter）。`<data>` が空なら absent（観測の事実）。（4）**比べ方は正規形**：名前空間は接頭辞でなく URI、値の中の宣言された接頭辞（identityref）は URI に解決、要素の間の空白と値の前後の空白は落とし、属性は並べ替える。兄弟の順は既定で問わず、intent の `ordered`（`nacm/rule-list/rule` のような要素の名前の道。比べ方の宣言で機器には無い）で名指しした列だけ順を比べる。比べるのは `PlanRefiner`（観測の値は機器の順のままの正規形）で、違えば「道 = 値」の行で機器にだけある行・宣言にだけある行を Diagnostic に出す。機種固有の設定の型（YANG）は持たない（利用者が機器の YANG に沿った XML を書く）。（5）**届かない・認証・ホスト鍵・hello が合わない（NETCONF の名前空間でない・session-id が無い・base が無い）・区切りが壊れた・rpc-error は観測できない（unknown）**。（6）**後で入れる 2 段の Apply の方針**（この版では入れない）：`:candidate` に edit-config → `:validate` があれば validate → commit confirmed（timeout つき）→ **新しい接続で**届くこと・宣言どおりなことを確かめる → 確定の commit。`:confirmed-commit:1.1` の機器では persist-id で別のセッションから確定できるが、**`:confirmed-commit:1.0` だけの機器では確定は commit confirmed を送った同じセッションからしかできず、そのセッションが切れると取り消される**ので、同じセッションを開いたまま新しい接続で確かめ、同じセッションで確定する。確かめられなければ確定せずに timeout で戻させる。入れたら `CommitConfirm` を宣言し `ObserveOnly` を外す。（7）単体・契約テストは Go の中で動く偽の NETCONF サーバ（`netconffake`、base:1.0 だけのものと 1.1 のもの、subtree filter を当てる）、受け入れテストはデーモンの周と CLI（偽の ssh の先で偽のサーバ）。統合テスト（`make integration-netconf`、check・CI の外）は netopeer2 のコンテナ（tag と digest で固定）を相手に、宣言どおりなら適合、別の接続の edit-config で変えると drift が知らされ Apply は出ない（機器は変えたまま）、何周してもずれが出ない、止めると unknown を確かめる（docs/providers/netconf.md） | §3, §5.3, §5.6, §10, §23.4, ADR-0002 |
| **LDAP provider**（Backlog #144） | 管理系の VM とアプリを宣言で組み上げる（ストーリー vm-proxmox-vm-ldap-ip）うえで、LDAP のユーザ・グループを宣言で保つ口が無かった。**決定（2026-10-27）**：（1）provider `ldap`（`internal/ldap`）。Go の依存を増やさないため OpenLDAP のクライアント（`ldapsearch`・`ldapmodify`）を exec で呼び、変更は LDIF（DN と値はいつも base64）を標準入力で渡す（postgresql が psql を呼ぶのと同じ形。シェルは通さない）。bind は simple bind で、パスワードは SecretRef を呼ぶたびに解決して 0600 の一時ファイルに書き `-y` で渡す（引数・環境変数に出さない）。LDAPS と StartTLS（`-ZZ`）、証明書は必ず確かめる（`LDAPTLS_REQCERT=demand`、親の `LDAP*` の環境変数は引き継がない）。サーバの一覧は `--provider-config` の `ldap_config`。（2）**リソースは 1 つの項目**：ID は `<サーバ>/<DN>`。DN は正規形（型と値を小文字、区切りの前後の空白なし、RFC 4514 の最小のエスケープ、複数値の RDN は並べる）で書き、正規形でない ID は読み込みで断る（DN に使う標準の属性は大文字小文字を区別しないので、List と intent の ID が綴りだけで食い違って二重に作る・prune で消すのを型で防ぐ）。作るときの葉の RDN の値は宣言の属性の綴りを使う。（3）**宣言した属性だけ管理**：フィールドは属性の名前（大文字小文字を問わない）→ 値の集合（1 つなら値、複数なら JSON の文字列の配列。`""`・`[]` は「無い」）。属性ごとに `replace` で一覧ちょうどにし、1 つの modify で送る。比べるのは PlanRefiner：objectClass は宣言のクラスがすべて在り、宣言に無いクラスはどれも在るクラスの上位（サーバの subschema の `objectClasses` の SUP をたどる。観測の `implied_objectclass`、Computed）か `top` なら適合（389 Directory Server は上位のクラスを足すため）。DN を値に持つ標準の属性は DN の正規形で比べる。操作属性・属性のオプションは宣言できない。適用の後に読み戻し、宣言と違えば PermanentError。（4）**`userPassword` は SecretRef で書き込みだけ**：作るとき・項目に無いときに書き、観測には値（ハッシュ）を入れず在ることだけを返す。在れば適合（永久 drift を出さない）。postgresql と違ってロールのコメントのような参照の置き場が LDAP の項目に無いので、**世代を上げても書き直さない**（替えるときはサーバの値を消せば次の周に書く。docs/providers/ldap.md）。（5）**管理の境界**：List は設定の `base_dn` の下（base そのものは含まない）の、設定の `filter` に合う項目だけ（ページに分けて受け取る）。bind_dn の項目・サブエントリ・tombstone は出さない。スキーマ・設定の項目は base の外。base の外・bind_dn を指す ID は読み込みで断る。（6）**削除は破壊的**（Pruneable、Delete は変更予算に数える）。**子の項目がある項目は消さない**（PermanentError）。（7）**届かない・bind の失敗・TLS の失敗・コマンドが無い・bind のパスワードを解決できないは unknown**（absent にしない。List は error で prune は止まる）。（8）偽の ldap コマンド（`internal/ldap/ldaptest`。Runner の seam に差すものと、受け入れテストで exec する実行ファイル。389 Directory Server のように上位のクラスを足し、DN の値の空白を詰める）で単体・契約テスト。統合テスト（`make integration-ldap`、check・CI の外）は 389 Directory Server の公式のイメージ（tag と digest で固定）を相手に、空から作る→適合、手で属性を変える→戻る、何周しても drift が出ない（`modifyTimestamp` も変わらない）、子のある項目は prune されない、止めると unknown を StartTLS・LDAPS で確かめる。**389 Directory Server で確かめた。他の実装でも標準の LDAP なら動くはずだが未確認**（docs/providers/ldap.md） | §5.3, §5.4, §7, §12, §20, ADR-0002 |
| **Proxmox VE の VM provider**（Backlog #143） | ハイパーバイザの上の VM を宣言で作る口が無く、管理系・監視系の VM を手で作っていた。**決定（2026-10-27）**：（1）**provider 名 `proxmox-vm`**、REST API（`/api2/json`）を Go の標準ライブラリの net/http で叩く（ライブラリは入れない）。認証は **API トークン**（`PVEAPIToken=<user>@<realm>!<id>=<secret>`）で、secret は SecretRef（要求の直前に解決し、ログ・エラーに出さない）。TLS は `ca_file`、`insecure_skip_verify` は明示したときだけ。設定（`--provider-config` の `proxmox_config`）は名前付きのクラスタの一覧（endpoint・トークン・既定のノード・scope・時間切れ・Apply が非同期の仕事を待つ長さ `task_wait`）。（2）**ID は `<クラスタ>/<VM 名>`**で、VM は名前で引く（vmid は ID にしない：採番の `from_pool` から配れるように。plan・status の採番を待つ仮の値は検査に掛けない）。同じ名前が 2 つあれば観測できない（Unknown）、名前の無い・重なる VM があれば List はエラー（complete と言わない）。（3）**触らない VM**（`protected_vmids`・`protected_names`。Sakutei 自身が動く VM など段0 のもの、§23）は List に出さず Read・Apply もせず、宣言すれば計画のエラー（vmid の採番が触らない番号を配っても作らない）。テンプレートも List に出さない。（4）**フィールドは宣言したものだけを管理**する：vmid・node（今ある VM と違えば計画のエラー。番号を変えない・移行しない）、template（作るときだけ、full clone、観測しない）、cores・memory、disks（宣言した slot だけ比べる。大きくするのはよい、**小さくする・ストレージを変えるのは計画のエラー**、宣言から外しても消さない）、nics（宣言した slot だけ。bridge・VLAN はいつも、MAC・型は書いたときだけ比べ、ほかのキーは残す）、cloud-init（ユーザ・ssh の公開鍵・ipconfig0 の IP とゲートウェイ・DNS）、power（running / stopped。停止は正常停止で、`power_off: graceful_then_force` と `force_after` を宣言したときだけ強制停止に進み、その Op は破壊的＝予算に数える。Redfish と同じ）。Proxmox が書き戻す表記（大きさの単位・`current=` のメモリ・大文字の MAC・URL エンコードの公開鍵）は正規形で比べ、永久 drift を出さない。（5）**再起動が要る変更（pending）**：観測の値は設定が言う値（pending があればその値）とし、今効いている値は Computed の `current_values` に、再起動を待つフィールドは `pending` に出す。宣言どおりの値が pending にあれば差分にせず**重ねて書かない**。走っている VM の手の変更が pending に置かれたときは差分にして書き戻す。再起動は `apply_pending: reboot` の宣言（宣言したフィールドに pending があるときだけ 1 回の正常な再起動）か、Workflow の action（`reboot`・破壊的な `reset`、provider.Actioner）で明示したときだけ。（6）**非同期の仕事（UPID）**：Apply は `task_wait` まで終わりを待ち、終われば同じ Apply で続ける。終わらなければ UPID を S3 に持ち、終わるまでと VM に lock がある間は Converging（操作を重ねない）。失敗は次の Read が 1 回だけ Unknown で知らせる。（7）**削除**は明示だけ（VM は破壊的な型で `Pruneable: false`、§12.3）で、Delete は破壊的として予算に数え、走っていれば止めてからディスクごと消す。（8）`/cluster/resources` は VM を探すのにだけ使う：その名前・status・template は pvestatd が周期で写したもので、作った直後の VM は名前が空・status が古い（本物の統合テストで見つけた。名前が空を「無い」と見るともう一度作ろうとし、古い stopped を見ると走っている VM に start を重ねる）。名前が空の項目は VM の設定（pmxcfs が正）で確かめ、電源と lock は `/status/current` で読む。届かない・5xx（ノードの 595）・ノードの停止・認証の失敗は Unknown。BulkObserve はクラスタごとに `/cluster/resources` 1 回と VM ごとの `/status/current`・`/pending`（Read と等価）。（9）**検証**：偽の Proxmox（httptest）で契約テスト・BulkObserve の等価性、本物の Proxmox VE 9.2（ISO を版と sha256 で固定し、proxmox-auto-install-assistant で自動導入）を入れ子の KVM で動かす統合テスト `make integration-proxmox`（Debian 13 のクラウドイメージのテンプレートから作った VM に cloud-init の鍵とアドレスで ssh で入れる・手の変更が戻る・pending と apply_pending・触らない VM・届かないと unknown・削除。2026-10-27 PASS）。**見送り**：LXC・ストレージ・ブリッジ・HA・バックアップ・スナップショット・移行の管理、linked clone、複数の ipconfig、cloud-init のパスワード（秘密を設定に置かない）。 | §5/§12/§13/§23 |
| **採番の値のテンプレート**（Backlog #145、#108 の 1） | v1 のプールは整数だけで、払い出したアドレスを A レコードや VM の cloud-init に入れられなかった（2026-10-23 の採番の試用）。IP のプール（CIDR の範囲）か、整数を値に埋め込むテンプレートか。**v3.26 で決定（2026-10-27）**：**テンプレート**にする——プール・採番記録・authority（HTTP の API・HA・キャッシュ）は整数のまま変えず、要求の側に `template: "192.0.2.{n}"` を書いて render のフェーズ1 で整数を埋め込む（§19.8）。写し方は `{n}` の置き換えだけ（足し算・オクテットの割り算は持たず、ほかの `{ }` はロードで拒否）。任意の `kind: ipv4 / ipv6` で、範囲のすべての値がそのアドレスになることをロード時に確かめる。CIDR のプールを採らなかった理由：採番記録の値の型・範囲の縮小の検査・claim の重なりの判定・HTTP の API がみな整数を前提にしており、変える範囲が大きい割に、v1 の使い方（1 つのサブネットの中のホスト部を配る）はテンプレートで足りる。**見送り**：サブネットをまたぐ範囲（/23 以上のホスト部）・IPv6 の 16 進の写し方（`{n}` は 10 進のまま。`2001:db8::{n}` は 16 進として読まれるので、範囲は 9999 まで）・同じプールの別のテンプレートの値の重なりの検査（テンプレートは 1 つの値の書き方で、2 つのテンプレートが同じ文字列を作ることはプールが知らない）。 | §19.8・§9.1 |

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
| dnszone が読めない行を黙って読み飛ばす（Backlog #37・解決済み） | ゾーンファイルの解釈できない行を捨てていたため、打ち間違えたレコードが `Read` で「無い」（`ok` + 空の State）と見なされ、Create されて重複しえた。unknown ≠ absent（§4.5）に従い、レコード行として解釈できない行（未知のレコード型・`$ORIGIN` / `$TTL` 以外のディレクティブ・複数行の括弧・所有者名を省いた字下げ行など）が 1 行でもあればファイル全体を parse error とし、`Read` は `unknown`、`List` はエラー（prune は halt）にした。エラーには読めない行の行番号を出す。未対応の RFC1035 構文を含む既存のゾーンファイルは `unknown` になる（既知の限界） | §4.5・§12.4 |
| 予算カウンタが S3（メモリ）のまま（v1.5 で確認・未登録） | §13.2 は累積時間窓予算のカウンタを S2 相当として永続化すると定めるが、実装はメモリに持ち、再起動で窓がリセットされる。planned_change のレーン使用量（§13.3）は v1.5（#44）で S4 の `planned_change.executed` から読み直すようになった（`--audit-log` を付けたとき。付けない常駐は再起動をまたぐと `max_count` を超えうる。one-shot `reconcile` は `--audit-log` が無ければレーンを開かない）。常駐は使用量を周の終わりに S4 へ書くので、周の途中で落ちるとその周の分は数えられない | §13.2・§13.3 |
