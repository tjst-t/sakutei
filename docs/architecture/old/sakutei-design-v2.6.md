# Sakutei(作庭)— 宣言的インフラ収束エンジン 設計書 v2.6

改訂日:2026-07-22。本書は現時点の**設計の正**である。決定に至った経緯や過去案は載せない。
本書は v3.0 全面再構成の前の**最終パッチラウンド**であり、v2.5 までの4ラウンドの検証で残った既知課題を消し込むことに焦点を置く。

**v2.5 からの主な変更**(詳細は `CHANGELOG-v2.6.md`):
- **計画変更宣言(`planned_change`)を第一級化**(§10.9)——大量正当作業(HPC 一斉再イメージ・棚卸し削除)と安全予算の衝突を、intent 内の宣言で解く専用解除弁。宣言は PR 承認を経て、窓内・scope 内・種別一致の変更に**宣言上限までの専用予算レーン**を与える(バイパスではなく別枠計上)。既存 `approved-large-change` タグを統合(11/13/09 指摘)。
- **承認ゲートの N-of-M 拡張**(§10.5)——手動承認ゲートに承認者要件(N-of-M・役割制約〈申請者≠承認者〉・全承認者の S4 監査記録)を追加。ゲート API とゲート定義の拡張のみで実装(17 指摘の 4-eyes)。
- **災害時運用経路(DR モード)**(§18.1)——DR intent のプリステージ(平時にレビュー済み・発動はパラメータ/claim 切替のみ)+ 緊急ローカル書込経路(git 到達不能時の署名付き break-glass コミット・事後合流手順とセット)。DR 中の PR 承認・署名経路の東京依存を解消(06 指摘)。
- **運用プロファイル(solo / team / enterprise)**(§10.10)——規模別の安全既定値セット。solo は事前承認を cooling-off(遅延実行)で代替可・自己マージ可、team(既定)は現行どおり、enterprise は N-of-M+役割制約を既定化。段0-minimal(§19.8)と solo の対応を明記(01/02 指摘)。
- **§26 カテゴリ A の最終消し込み**——`State`/`Plan`/`Scope`/`Event` の正準形を型として確定(§4.6)、S4 永続化先を正式決定(§17.5)、remediation schedule × ownership の優先規則(most-restrictive-wins・§10.8)、List 急減閾値の scope 単位宣言化(§8.6)。**§26 カテゴリ A は本改訂で空になった**(20 指摘含む)。

**目次**

| # | 章 | # | 章 |
|---|---|---|---|
| 1 | 概要(名称・位置づけ) | 14 | シークレット |
| 2 | 設計を貫く原則 | 15 | credential-broker |
| 3 | アーキテクチャ全体 | 16 | allocator |
| 4 | コア抽象 | 17 | 観測・イベント・監査 |
| 5 | **プロバイダ実装ガイド** | 18 | バックアップ / DR |
| 6 | 収束ループ | 19 | 段0(ブートストラップと自己運用) |
| 7 | render パイプライン | 20 | 適用ドメイン |
| 8 | 削除セマンティクス | 21 | プロバイダ戦略 |
| 9 | 3プリミティブ | 22 | 開発・検証環境(devenv / twin) |
| 10 | 安全機構 | 23 | Brownfield 導入 |
| 11 | オンプレ横断項目 | 24 | スコープ |
| 12 | マルチテナントとスケール | 25 | 到達点 |
| 13 | 理想状態の供給源と設定形式 | 26 | **未決事項(決定待ち)** |

---

## 1. 概要

### 1.1 何を作るか

オンプレミス・インフラ全体(物理電源からゲスト OS まで)を**宣言的に管理し、継続的に収束させる**
コントロールプレーン。k8s の**リコンシレーションループ(level-triggered な収束)という動作原理**だけを
借り、k8s の実装機構(apiserver / etcd 分散合意 / scheduler / admission / CRD 足場)は持たない。

### 1.2 立ち位置

| | 理想状態 | 観測 state | 駆動 |
|---|---|---|---|
| Ansible | 外部宣言 | 持たない | 一回 push(収束しない) |
| Terraform | 外部宣言 | **持つ**(state drift 地獄) | 一回 push |
| k8s | 内部 etcd | **持つ**(status subresource / informer cache) | ループ |
| **本システム** | 外部宣言 | **持たない**(観測 state レス) | **ループ** |

**Ansible の冪等性 + k8s のループ**。宣言的・観測 state レス・かつ回り続ける。
観測 state を持たないため drift 検知漏れがゼロになり、Terraform の三項差分問題(宣言 / state /
現実)が構造的に消える。k8s は status を etcd に永続化するが、本システムはそれすら持たず毎周観測する。

### 1.3 名称

プロジェクト名は **Sakutei(作庭)**。平安期の『作庭記』は世界最古の「庭の宣言的仕様書」であり、
**理想の姿を宣言し、絶えず観察し、剪定(prune)し、手を入れ続ける**という日本庭園の営みが、本システムの
動作原理そのものと一致する。庭は放置すれば drift する。

**命名規約:プロジェクト名のみ固有名詞、コンポーネントは機能名(英語)。**
名前は「覚えるもの」だが、**出現頻度が非対称**である——プロジェクト名は文書や会話に一度しか現れないため
固有名詞の記憶コストが問題にならないのに対し、コンポーネント名はコード・ログ・日常会話で高頻度に使われ、
「意味の紐付いていない音を記憶するコスト」が継続的に発生する。したがって:

| 対象 | 名前 | 役割 |
|---|---|---|
| プロジェクト全体 | **Sakutei** | 庭を作り維持する営み全体(『作庭記』由来) |
| 収束エンジン core | **reconciler** | Observe → Diff → Apply / prune の収束ループ |
| credential broker | **credential-broker** | JIT 短命発行 / bastion / 個人単位の到達資格(§15) |
| allocation authority | **allocator** | 採番払い出しの権威・記憶装置(§16) |
| intent UI | **catalog** | 意図を描く薄いフロントエンド。正は git 側(§13.4) |
| 開発・検証環境 | **devenv** | 隔離された検証環境(§22) |
| デジタルツイン | **twin** | 本番と同一宣言・render 入力差し替え(§22.5) |
| 汎用インストーラ | **provisioner** | 起動方式と OS 種別を分離したベアメタル導入基盤 |

**CLI の動詞も業界標準**を用いる(既存知識が転用でき、学習コストが生じないことに価値がある)。
バイナリは `sakutei` 単一とし、サブコマンドで機能を分岐する。

```
sakutei                 # 常駐して回り続ける(本来の姿)
sakutei plan            # 何が変わるか(= Diff で止める。dry-run)
sakutei reconcile       # 一回だけ収束させる(手動キック)
sakutei status          # drifted 一覧(conformance レポート)
sakutei import          # 既存の現実を intent として書き出す(brownfield 導入・§23.3)
sakutei --claim <scope-selector>   # 自分が収束を担当する scope を宣言して常駐(静的シャーディング・§12.4)
```

`--claim` は全 intent を pull した上で **自分の claim 分だけを収束**するための起動フラグ(スケール
アウトの段2)。シャード割当そのものを git の intent として宣言し(§12.4)、二重 claim は fencing token
(§12.3)で検知する。型は v1a で切り、実装は必要時。

`apply` を主動詞に置かないのは、**このシステムが「一回叩くツール」ではなく「回り続けるもの」**である
ことを名前で表明するため(§6)。

### 1.4 構成

**収束エンジン core(reconciler)は単一バイナリ・単一デーモン。周辺は小さな衛星群**
(credential-broker / allocator / catalog)。衛星を分けた理由は**ライフサイクル差・
可用性の逆転・攻撃面の分離**であり、機能膨張ではなく責務境界を正しく引いた結果。core は小さく保つ。

### 1.5 スコープと本書の位置づけ

対象は**オンプレ**。ネットワーク機器・LB/FW・SDN・ストレージ・ハイパーバイザー・VM・ゲスト OS・
コンテナ・物理電源。手作業が多いオンプレでこそ drift 自動修復と観測 state レスの価値が出る。

**本書は「汎用エンジンの仕様」であり、特定案件の構築手順ではない。** 両者は性質が異なる:

| | 内容 | 例 |
|---|---|---|
| **コア仕様(本書)** | どの現場にも適用できるエンジンそのものの仕様 | interface 定義・収束アルゴリズム・削除セマンティクス・安全機構・原因タグの法則 |
| **参照実装(別文書)** | 特定案件固有の意思決定と具体スキーマ | 「この機種の BMC API の癖」「この DC のトポロジ座標の付け方」「Phase 1-10 の構築手順」 |

本文中に現れる具体プロダクト名(Proxmox / Ceph / Vault 等)は**参照実装での採用例**であり、
仕様そのものは interface と seam の言葉で記述する。**具体スキーマ・ベンダー評価・構築手順は
参照実装文書へ分離する**(混在させると、コア仕様の変更提案と案件固有の設計変更がレビューで混ざる)。

---

## 2. 設計を貫く原則

全コンポーネントに共通する。個々の設計判断はこの6原則の適用として導かれる。

1. **level-triggered + 観測 state レス**
   毎周 desired と actual を突き合わせる。観測 state は持たない。持つ state は S1–S4 に分類し、
   **喪失が事故になる S2 のみ永続化**する(§6.5)。

2. **いつ = 収束 / どう = Task・Workflow**
   経路依存・非冪等・中断が致命的な手続き(migration / firmware / ローリング / DR)は収束ループに
   載せない。収束は「変えるべきか」を判定しトリガとゲートを担い、手続きは Task/Workflow に閉じる(§9)。

3. **確信が持てないとき、加算はしても減算はしない**(非対称原則)
   最も広く貫く原則。unknown ≠ absent / `pruneable:false` 既定 / 分断時の破壊保留 / 爆発半径 halt /
   freeze / **allocation の Release** まで、すべてこの一つの規律の適用。

4. **値・配置・予測・認可は持たない。委譲する**
   シークレット値 → Vault、配置最適化 → DRS、時系列予測 → Prometheus、認可・課金 → 上位
   プラットフォーム。**持たないことが強み**(漏洩面が縮み、再発明を避け、core が小さく保たれる)。

5. **seam は今切る、split は後**
   将来の分離点(分散実行・外部 CMDB・DB 供給源・認証・採番)を **interface として先に切り**、
   実際の分割は必要になってから。これによりコアを無変更のまま拡張を吸収できる。

6. **`ownership`(platform / tenant)が横断的に貫く**
   ただ一つの区別が、**所有・書き込みポリシー(PR 必須 / 即時)・修復モード・rate limit・爆発半径・
   供給源の選択・シャード分割境界**を決める。個別に導いた結論が同じ軸に乗ったことは、分解が正しい証拠。

---

## 3. アーキテクチャ全体

### 3.1 全体図

```
                        ┌────────────── 人間 ───────────────┐
                        │ 金庫:unseal key / Break Glass      │ ← ブートストラップの底
                        │ Ansible(段0の構築・unseal)         │
                        └──────────────┬────────────────────┘
                                       │ 構築(稀・意図的・pull 型)
                                       ▼
┌──────────────── 段0(人手管理・"作って忘れる" 不変基盤)──────────────────┐
│  管理NW到達性の最小核(§19.3)+ OOB / コンソールサーバ                     │
│  Proxmox 3ノード + 統合 Ceph + VM-HA                                       │
│    ├ Vault(シークレットの根)                                             │
│    ├ credential-broker← reconciler の依存先。落ちると全ターゲット全盲│
│    └ reconciler(収束エンジン core)                                          │
└──────────────────────────────┬────────────────────────────────────────────┘
                               │ 自律収束(常時・level-triggered)
                               ▼
┌───────────────────── 段1以降:本番インフラ全体 ────────────────────────────┐
│ 物理(電源/BMC)→ NW機器/FW → ハイパーバイザー/クラスタ → VM → guest OS      │
│ ストレージ / SDN / コンテナ / 基盤サービス(DNS/DHCP/PXE/NTP)              │
└───────────────────────────────────────────────────────────────────────────┘
```

### 3.2 コンポーネント

**A. reconciler — 収束エンジン core**(単一バイナリ・単一デーモン)

| 内部要素 | 役割 | 節 |
|---|---|---|
| 収束ループ | level-triggered。毎周 Observe → Diff → Apply | §6 |
| DAG / wave | 依存解決。毎周全 DAG、波単位並列、失敗は下流 skip + 次周回収 | §6.3 |
| render パイプライン | フェーズ1(外部入力解決・副作用あり)+ フェーズ2(決定的展開) | §7 |
| 3プリミティブ | Resource / Task / Workflow | §9 |
| 安全機構 | 爆発半径・原因タグ・振動検知・backoff・段階適用 | §10 |
| Event bus | 全ての意味ある出来事を単一 Event 型に集約 → sink | §15 |

**B. 衛星群**(core から分離した独立ソフト)

| 衛星 | 役割 | 分離理由 | core との関係 |
|---|---|---|---|
| **credential-broker**(credential broker) | 機器認証の統一(JIT 短命発行 / IdP / bastion / Break Glass) | ライフサイクル差(要求応答 vs 常時収束)/ **可用性の逆転** / 攻撃面の分離 | **core の依存先**(下位)かつ core の管理対象 |
| **allocator**(allocation authority) | プール採番(VLAN/IP/ASN)の権威 | 状態(冪等性の記憶)を持つ / 並行・シャード環境の採番調停を一元化 | core は「1つくれ」と要求するだけ |
| **catalog**(intent UI) | カタログ / フォーム(薄いフロントエンド) | UI にデータモデルを持たせない | 書き込み経路は `ownership` に従う |

**C. 外部委譲先**(作らない)

| 委譲先 | 委ねたもの | core の関与 |
|---|---|---|
| Vault | シークレットの値・動的発行・rotation | `SecretRef` で参照。値を持たない |
| IdP(SSO) | ヒューマン ID・認証 | broker 経由。core は認可を持たない |
| Prometheus / Alertmanager | 時系列監視・通知ルーティング | **監視"設定"を Resource として収束**。判定と通知は委譲 |
| DRS / ハイパーバイザー | 配置最適化・ビンパッキング・migrate | cluster への intent と粗い容量充足のみ。**scheduler を再発明しない** |
| 上位テナントプラットフォーム | 認可・quota・課金・ポータル | `DesiredSource` の一実装として理想状態を投入 |
| git(GitHub/GitLab) | intent の履歴・レビュー・改竄耐性アンカー | `FileSource` の実体。監査の半分を担う |
| k8s 等 | アプリ層のデプロイ・スケール | core は土台(インフラ)を供給 |
| Ansible(モジュール) | ロングテール provider の実行実体 | `Executor` の一実装。**モジュールのみ借り、実行モデルは借りない** |

### 3.3 依存の向き(鶏卵の構造)

```
人間の金庫 ──(最後の鍵)──▶ 段0
段0 ──(素の起動を保証)──▶ Vault / credential-broker / reconciler
reconciler ──(実行時に依存)──▶ credential-broker / Vault / allocator / git
reconciler ──(管理する)──────▶ 段1以降の全インフラ + 衛星自身(credential-broker も allocator も Resource)
```

core は broker/Vault を**管理する**が、実行時には**依存する**。この循環は **段0が素の起動を保証する**
ことで断ち切る。段0で確立した「最後は人間の物理保管に着地する」原則が底。

### 3.4 seam(拡張と分離の全接点)

**この表を seam の正とする。**

| seam | 差し替えるもの | v1 実装 | 将来 |
|---|---|---|---|
| `Provider` / `Lister` / `BulkObserver` | 個別操作 / 集合列挙(prune の前提)/ **バルク Observe(任意・§5.7)** | 自前(DNS 等) | Ansible provider / 各ベンダー / バルク API 系 |
| `Executor` | 実行境界 | in-process | プロセス隔離 / リモート(gRPC) |
| `Loader` | 設定形式 | YAML / JSON | CUE |
| `DesiredSource` / `Watcher` | 理想状態の供給源 / **早期収束のヒント供給(任意・§13.5)** | FileSource(git)+ webhook/merge `Watch` | DBSource / RenderedSource / DBSource 連動 `Watch` |
| `DesiredStore` | 理想状態の保存 | ファイル + インメモリ | bbolt / SQLite |
| `SecretBackend` | シークレット取得 | Vault(段0-minimal は file・§19.8) | k8s Secret / file |
| `AllocationAuthority` | 採番払い出し | v1a 不要 / **v1b で最小実装** | NetBox / Infoblox |
| `CredentialProvider` | ターゲット到達資格 | v1 は SecretRef 直結(§14.5) | credential broker(v2) |

---

## 4. コア抽象

### 4.1 Provider と Lister(二層)

reconcile のコアは「個別リソースの操作」と「集合の列挙」に分ける。

> **【正準定義の所在】Provider が実装する interface の正は §5.1 とする。** 本節のコードブロックは
> **コア内部の概念モデル**(誰が何を担うかの分解)であり、**実装契約ではない**。両者が食い違った場合は
> **§5.1 が優先**する。特に次の3点を本改訂で確定した(v2.2 では §4.1 と §5.1 が食い違っていた):
> - **呼称は `Read` に統一**(`Observe` は概念語としてのみ残す。実装メソッド名は `Read`)。
> - **Diff は Provider が実装しない**。差分計算はコアが `Schema` 駆動で汎用実装する(§5.1・§5.3)。
> - **Read の戻りは `Observation`**(`Availability` / `State` / `Ready` の3要素。§4.2)。`ObserveStatus`
>   という別名は使わず、`Observation.Availability`(ok/unknown/converging)がその役割を担う。

```go
// 個別リソースの操作(概念モデル。実装契約は §5.1)
type Provider interface {
    Read(ctx, id string) (Observation, error)   // 現実を見る(desired を知らない)。戻りは Observation
    Apply(ctx, Plan) error                        // 冪等に寄せる(create/update/delete 統合)
    // Diff は Provider に無い。コアが Schema 駆動で実装する(§5.1・§5.3)
}

// 集合の列挙(capability interface = 実装は任意)
type Lister interface {
    List(ctx, scope Scope) ([]ID, error)         // scope 内の "現実の全 ID 集合"(集合 diff の入力)
}
```

**prune には Lister が要る**。削除は `現実にある集合 − 理想にある集合` という**集合演算**であり、
個別の `Observe(id)` では「理想に書かれていないリソース」を発見できない。
`DeletionPolicy: PruneWithinScope` は **Lister を実装する provider にのみ許可**し、Lister 無しの
provider に prune を宣言したら理想状態ロード時に拒否する(循環検出と同じ入口・同じ思想)。

**diff は二段**:
1. **集合 diff**(desired 集合 vs List 結果 → create/delete 判定)… **コントローラ側の汎用ロジック**
2. **フィールド diff**(両方に在るものの update 判定)… **Schema 駆動でコアが汎用実装する**
   (provider は Diff ロジックを書かず、`Schema()` を宣言するだけ。§5)。

> 上記の `Provider` interface はコア内部の概念モデルである。**プロバイダ作者が実際に実装する
> interface と、その契約・SDK・実装の階級は §5「プロバイダ実装ガイド」を正とする**(上の【正準定義の
> 所在】ボックス参照)。

### 4.2 Observation と ConvergenceStatus(型として別)

`drifted / converged` は desired との比較結果=**Diff の出力**であって、Observe(現実を見るだけ)では
判定できない。混ぜると Observe が desired を知る必要が生じ、**dry-run の正しさの根拠**(Observe/Diff
分離)が崩れる。

```go
type Observation struct {
    Availability Availability  // ok | unknown(到達不能) | converging(相手が収束作業中)
    State        State         // Availability == ok のときのみ有効
    Ready        map[string]bool // 名前付き ready condition の充足状況(§6.7)
    ObservedAt   time.Time     // この観測を取った時刻。bulk/個別いずれも刻む(§5.8)。周期越え再利用の検知に使う
}
```

収束ステータス(報告用)はコントローラが**導出**する:
- `unknown` / `converging` … Observation から
- `converged` / `drifted` … Diff 結果(Plan が空か否か)から
- `oscillating` … 履歴から(§10.4)

**Observe は desired を知らず、Diff は現実を触らない**が型で保証される。

### 4.3 Capabilities(機械的縮退)

provider の能力差を散文の注意書きにせず、機械可読なメタデータにする。

```go
type Capabilities struct {
    TrueObserve   bool  // current を "状態" として返せる(Ansible check mode = false)
    List          bool  // → prune 可否(§4.1 と連動)
    Converging    bool  // 相手の作業中を検知できる
    CommitConfirm bool  // candidate/commit で自動ロールバック可(NW 機器)
    BulkObserve   bool  // scope 内を1コールでまとめて Read できる(バルク API・§5.7/§12.2)
}
```

**縮退規則(コントローラに固定)**:
- `TrueObserve:false` → actual 比較ができないので原因タグ A/B を区別不能。単一タグ `changed` に統合し、
  rate limit の重み付けは**破壊的側に倒す**(不明は危険扱い=非対称原則)。
- `List:false` → その型への prune 宣言はロード時に拒否。
- Ansible でも `*_info` 系モジュールを Observe に使える対象は `TrueObserve:true` に**部分昇格**でき、
  縮退は全面でなく部分回復可能。

### 4.4 Executor(実行境界)

`Provider` / `Lister` を束ねる実行境界。**コントローラはこの型にしか触らない**(DI で渡す)。

```go
type Executor interface {
    Observe(ctx, specs []Spec) ([]Observation, error)  // 内部で各 provider の Read を束ねて呼ぶ
    List(ctx, scope Scope) ([]ID, error)               // prune 対象 provider のみ有効
    Apply(ctx, plan Plan) (Result, error)
}

type LocalExecutor struct { /* providers */ }       // v1 唯一の実装:同一プロセス直接呼び
```

**守る規律は1つ**:コントローラのコードが `LocalExecutor` の存在を知らないこと。これさえ守れば、
将来 `RemoteExecutor{conn *grpc.ClientConn}` を書いて差し替え口に挿すだけで**分散が生える**。
プロセス境界は「隔離(障害・権限・言語)」で正当化されるのであって、スケールのためではない
(Observe は I/O バウンドで goroutine で足りる)。

### 4.5 dry-run と plan/reconcile の2回 Observe

Observe / Diff / Apply が分離されているので、`plan`(dry-run)は「**Diff で止める**」だけで原理的に
正しい(Ansible の check mode がモジュール依存で不安定なのと対照的)。

**`plan` と `reconcile` で Observe は2回走る。これは正しい挙動であり、削ってはならない。**
plan の観測結果を reconcile に持ち越すと「**古い現実に基づいて Apply する**」ことになり、排除したはずの
観測 state を再導入することになる(Terraform が plan ファイルを固定して適用するのと逆の思想)。
Observe は並列化されるためコストは問題にならない(§6.2)。

残る懸念は TOCTOU(plan で人が見た内容と、reconcile 時の現実がズレる)だが、これは Observe を1回に
しても解決しない(むしろ古い観測で実行する分、悪化する)。本システムでは次の3つで担保する:

- **level-triggered なのでズレても次周で回収される**(一回性ツールより本質的に強い)
- **爆発半径制御**(§10.2)が想定外の大量変更を halt する
- **承認ゲート**(§10.5)は「plan の内容を1バイト単位で固定する」のではなく、「**意図を承認し、実行時の
  安全機構を信頼する**」形で担保する(宣言的・収束型の思想と一貫)

厳密な照合が要る場合は `reconcile --expect-revision <rev>` で **理想側の版(rendered_revision)だけ**を
照合する。現実側は照合しない(観測 state を持たない原則を守る)。

### 4.6 コア型の定義状況

本章の interface は、いくつかの補助型を受け渡しする。**このうち一部は未定義であり、実装着手前に
決める必要がある**(後から変えると全プロバイダに波及するため)。現状を明示しておく。

**定義済み(本書で確定)**

| 型 | 定義 | 節 |
|---|---|---|
| `Provider` / `Lister` | 個別操作 / 集合列挙の二層。**正準は §5.1**(`Read`/`Apply`+任意 `List`) | §4.1, §5.1 |
| `Observation`(`Availability` / `State` / `Ready`) | `Read` の戻り。三値 + ready condition。`ObserveStatus` 別名は廃止 | §4.2 |
| `Capabilities` | TrueObserve / List / Converging / CommitConfirm | §4.3 |
| `Executor` | Provider/Lister を束ねる実行境界 | §4.4 |
| `Schema`(属性の意味) | type / required / secret / managed / ordered / identity / computed | §5.1, §5.5 |
| `DeletionPolicy` | ExplicitAbsentOnly / PruneWithinScope | §8.2 |
| `ScopeMode` | ConvergeMode / SyncMode | §23.8 |
| `DesiredSource` / `AllocationAuthority` / `CredentialProvider` | seam の各 interface | §3.4, §16.5, §15.2 |

**正準形を本改訂(v2.6)で確定**(§26.1 A-1 の最終消し込み。以下を実装着手時の型の正とする)

`State` / `Plan` / `Scope` / `Event` は v2.5 まで「決定待ち」だったが、**v2.6 で以下を正準形として確定する**
(§26.1 A は本改訂で空になる)。細部の命名・タグ体系は実装中に調整可能だが、**構造(何を持つか)はここで固定**する。

```go
// State — Schema 付き map(汎用マップだが、各フィールドは Schema 宣言に紐づく型付き値)
//   型情報は Schema(§5.1)が持つので State 自体は map で足りるが、値は Schema の宣言型に従う。
type State map[string]TypedValue   // key = Schema のフィールド名、value = Schema 宣言型に沿った値
type TypedValue struct { Kind FieldKind; Value any }  // FieldKind は Schema の type と対応

// Plan — 操作列 + 破壊性 + 原因タグ + 対象 ID + 人間可読の差分診断
//   Apply の契約(§5.3)・rate limit の重み付け(§10.2)・破壊予算(§10.2)・per-resource 原因タグ(§10.3)が全部これを読む。
type Plan struct {
    Ops         []Op       // Create / Update / Delete の操作列
}
type Op struct {
    Kind        OpKind     // Create | Update | Delete
    Target      ID         // 対象(§26.1 A-1 の (provider, scope, id) 複合)
    Destructive bool       // ★破壊性フラグ(rate limit・破壊予算・halt-all 凍結がこれで駆動)
    Cause       CauseTag   // ★原因タグ A/B/C/D/E(per-resource 帰属・§10.3)
    Diagnostic  string     // 差分の人間可読診断(sakutei status / plan 表示用)
    Fields      []FieldDiff // 変わるフィールド(Update 時)。表示粒度はここで展開(§8.5)
}

// Scope — provider 非依存のラベル集合 + パス形式。claim の scope-selector・prune 境界・
//   intent/<scope>/ パス規約(§12.5)と同一の語彙。zone/cluster/rack を1型で表す。
type Scope struct {
    Labels map[string]string  // 例: {site: tokyo, cluster: prod}。provider ごとに意味が違うのを1型で吸収
}
func (s Scope) Path() string  // labels → intent/<scope>/... のパス(§12.5)・rendered_revision 算出単位(§26.1 A-3)

// Event — 単一型 + kind enum + scope/resource 参照 + revision 参照
//   §17.4 の分類(severity / level か edge か / Since / 原因タグ)がそのまま入る。
type Event struct {
    Kind       EventKind   // enum(DriftDetected / ProviderUpgradeDiff / StageAwaitingApproval / BreakGlassUsed …)
    Severity   Severity    // info | warning | critical
    LevelOrEdge LevelKind  // level(継続状態・firing/resolved)か edge(事件・一回)か(§17.4)
    Scope      Scope       // どの scope で起きたか
    Resource   *ID         // 個別リソース参照(あれば)
    Revision   string      // 関連する rendered_revision / intent_revision
    Since      time.Time   // level イベントの継続開始(transient↔stuck 自動昇格・§17.4)
    Cause      *CauseTag   // 原因タグ(監査帰属・§17.5)
}
```

| 型 | 正準形(v2.6 確定) | 根拠 |
|---|---|---|
| `State` | **Schema 付き map**(フィールドは Schema 宣言に紐づく型付き値) | 型情報は `Schema` が持つので State 自体は map で足りる(§5.1) |
| `Plan` | **操作列(Create/Update/Delete)+ destructive flag + cause tag + 対象 ID + 差分診断(人間可読)** の構造体 | Apply の契約(§5.3)・rate limit(§10.2)・per-resource 原因タグ(§10.3)が全部読む |
| `ID` | **`(provider, scope, id)` 複合で確定(v2.4・§26.1 A-1)** | scope=ルーティングキー(§12.4)。global-unique は再シャードで壊れるため棄却済み |
| `Scope` | **provider 非依存のラベル集合 + パス形式**(claim の scope-selector・prune 境界・`intent/<scope>/` パス規約と同一語彙) | 型付きにすると provider 追加のたびにコア変更になるのを回避 |
| `Event` | **単一型 + kind enum + scope/resource 参照 + revision 参照** | §17.4 の分類(severity / level か edge か / Since / 原因タグ)がそのまま入る |

---

## 5. プロバイダ実装ガイド

**プロバイダの書きやすさが Sakutei の普及を直接決める**(Terraform が勝ったのはプロバイダ
エコシステムだった・§21)。したがって設計の肝は、**プロバイダ作者が書く最小を極限まで削り、面倒は
コア側の共通ライブラリ(SDK)が引き受ける**ことにある。

### 5.1 プロバイダが実装する最小 interface

削りに削ると、作者が書くべきは **4つの関数**。prune を許すなら `List` を1つ足す。

```go
type Provider interface {
    // メタデータ
    Schema() Schema              // フィールド定義(型・必須・secret・managed・ordered・identity・computed)
    Capabilities() Capabilities  // TrueObserve / List / Converging / CommitConfirm

    // 現実を読む(desired を知らない)。戻りは Observation(Availability / State / Ready)。§4.2
    Read(ctx, id string) (Observation, error)

    // 現実を変える(create / update / delete を1関数に統合)
    Apply(ctx, plan Plan) error
}

// prune を許すなら追加(任意)
type Lister interface {
    List(ctx, scope Scope) ([]ID, error)
}
```

**`Read` の戻りは §4.2 の `Observation` に統一する**(v2.2 の `(State, ObserveStatus, error)` は廃止)。
`Observation` は `Availability`(ok/unknown/converging)・`State`(ok のときのみ有効)・`Ready`(名前付き
ready condition の充足状況・§6.7)を1つの型に束ねる。これにより provider が `Ready` を返す経路が署名
レベルで通り(§6.7 の依存解決が成立する)、`ObserveStatus` という未定義の別名も不要になる。`error` は
到達失敗そのもの(接続不能等)を表し、到達したが「相手が状態を返せない」場合は `Availability` で表現する。

設計上の3つの負担軽減:

- **(1) Diff を書かせない**(最大の軽減)。作者は「読む(Read)」と「あるべき姿にする(Apply)」だけ
  書けばよく、**差分計算は Schema を使ってコアが汎用実装する**。フィールド比較はプロバイダごとに書くと
  車輪の再発明かつバグの温床。順序付きコレクション(§8.5)のような特殊 diff も **Schema で `ordered` と
  宣言すればコアが適用する**——作者は宣言するだけ、ロジックはコア。
- **(2) create/update/delete を Apply 1本に統合**。`Plan` に「何をすべきか(create/update/delete と
  変わるフィールド)」が入っているので、Apply 1本で受ける(中の分岐は自由)。
- **(3) Schema がすべての鍵**。フィールドの型・必須・secret・managed・ordered・identity・computed を
  宣言すれば、コアが diff・検証・マスキング・managed 除外を**全自動化**できる。

### 5.2 コアが提供する SDK / ヘルパー

作者を楽にするためにコア側が用意する:

| SDK 機能 | 何を肩代わりするか | 関連 |
|---|---|---|
| 差分エンジン | Schema ベースの汎用 diff(順序付き含む) | §4.1 |
| secret マスキング | Schema で `secret` 宣言したフィールドを自動 `SecretRef` 化・ログマスク | §14 |
| managed fields 除外 | Schema で `managed` 宣言したフィールドを diff 対象外に自動除外 | §11.2-4 |
| version 埋め込み | provider ビルド version を diff 入力に自動合成(原因タグ C) | §10.3 |
| エラー分類ヘルパー | Apply のエラーを transient / permanent に型付け | §6.6 |
| テストハーネス | フェイクプロバイダ基盤・Read/Apply の契約テスト自動生成 | §22 |
| Schema lint | 「secret 宣言し忘れ」「identity 未指定」等を静的検出 | §5.5 |

### 5.3 各関数の契約(Sakutei 特有の難しさの本体)

interface は4つでも、各関数が満たすべき契約に設計書の原則が集約される。

**Read の契約**
- **desired を絶対に見ない**(§4.2)。「現実に何があるか」だけを返し、「あるべき姿」と比較しない。
  破ると dry-run が壊れる。
- **三値を正しく返す**:存在 = `ok` + State、到達不能 = `unknown`(§2-3 の非対称原則の起点。
  **存在しないことと unknown を絶対に混同しない**)、相手が収束作業中 = `converging`(§10.4)。
- `TrueObserve: false` なプロバイダ(Ansible ラップ等)は State を返せず「変わったか」しか分からない。
  Capabilities で正直に宣言し、コアが縮退する(§4.3)。

**Apply の契約**
- **冪等**(§2-1)。同じ plan を2回実行しても同じ結果(「作成」も「既にあれば何もしない」を含む)。
  毎周回る Sakutei では絶対条件。
- **エラーを transient / permanent に分類して返す**(§6.6 の backoff のため)。権限エラー = permanent、
  API タイムアウト = transient。
- 順序付きコレクションは**全置換**(§8.5)。commit-confirm 対応機器は candidate/commit を使う(§11.2-5)。
- 部分適用は**中途半端な状態を残さない**か、次の Read で正しく観測できる状態にする。

**List の契約(prune するなら)**
- **scope 内の現実の全 ID を漏れなく返す**(§4.1)。不完全だと prune が「List に無い = 消す」で
  **取りこぼしを削除**する。List の網羅性が prune の安全性を担保する。
- **ページング完全性を保証する**(§8.6-a):next token が nil で終わったことを確認して返す。**途中打ち切りを
  検知したら成功として返さず error にする**。SDK の完走検証ヘルパを使い、手書きの取りこぼしを防ぐ。
- 網羅を保証できない今回の List は `ListStatus`(complete/partial/unknown)で申告できる(§8.6-c)。
  コアは `complete` 以外の周は prune を保守側に倒す(`Read` の三値と対称)。
- 実装できないなら `List: false` を宣言 → コアがその型への prune を拒否(§4.3)。
  **中途半端な List を返すくらいなら、実装しない方が安全。**

### 5.4 プロバイダの「階級」(全機能を要求しない)

全プロバイダに全機能を要求すると書けるものが激減する。**Capabilities で段階を認める**のが核心。

| 階級 | 実装 | できること | 例 |
|---|---|---|---|
| **完全** | Read(TrueObserve)+ Apply + List + CommitConfirm | 全機能(prune・原因タグ A/B・converging・自動ロールバック) | 自前 DNS / VM |
| **標準** | Read(TrueObserve)+ Apply | prune 以外(List 無し = open-world のみ) | 多くの REST API |
| **最小** | Apply + changed 真偽(TrueObserve なし) | 加算的収束のみ。diff 粗い・原因タグ縮退 | Ansible ラップ(§21.3) |

**作者は自分の階級を選べる**。最小階級なら Apply だけ書けばよく参入障壁が劇的に下がる。Capabilities で
正直に宣言すれば、コアが自動的に縮退して**安全に**扱う(prune させない・破壊的側に倒す)。
**「全機能を書けないと参加できない」ではなく「書ける範囲で参加でき、コアが安全に縮退する」**——これが
エコシステムを広げる鍵。

### 5.5 最難関は Schema 設計

実装の重さの本体は Read/Apply のロジックではなく、**Schema を正しく書くこと**。以下を間違えると、
設計書が積み上げた安全性が**無効化される**:

- どのフィールドが **secret**(§14)→ 間違えると資格情報が平文でログに出る
- どのフィールドが **managed / runtime**(§11.2-4)→ 間違えると DRS と永久に喧嘩
- どのフィールドが **ordered**(§8.5)→ 間違えると FW ルールの順序が壊れる
- どのフィールドが **identity**(削除・参照のキー)→ 間違えると別リソースを消す

**identity は「運用上の変更頻度」で選ぶ**:どのフィールドを identity にするかは、技術的な一意性だけで
決めてはならない。**変更されやすい属性(シリアル番号・MAC アドレス)を identity にすると、その属性が
変わるたびに Resource の再生成と理想状態の commit が必要**になり、運用コストが跳ね上がる。

> 例:物理サーバの identity をシリアル番号にすると、故障交換のたびに intent の書き換えが要る。
> **物理トポロジ座標**(「leaf03 スイッチの 12 番ポート」= `leaf03-p12`)を identity にすれば、
> 筐体交換時に理想状態の変更が一切不要になる。シリアルや型番は `managed: true` + `computed` として
> 観測のみ行い、identity にはしない。

したがって Schema はプロバイダ作者が最も注意深く書くべき部分であり、SDK の **Schema lint**(§5.2)が
宣言漏れを静的に検出する。プロバイダ実装は「まず Schema、次に Read/Apply」の順で書く。

### 5.6 【将来検討】抽象リソースと実装の分離(v1 では扱わない)

依存を**具体型でなく能力(capability)に対して張りたい**場面がある。例:「DB は VM の上で動くが、
その VM が VMware か Proxmox かは問わない」。これは抽象リソース(役割)とその実装(プロバイダ)の
分離であり、k8s の StorageClass / Crossplane Composition と同型。**v1 では具体依存のみを扱い、以下は
将来の検討事項として記録する**(seam として `Provides()` を予約しておく余地はあるが、解禁は慎重に)。

検討時に整理すべき論点:

- **2つの要求を分ける**:(A) **実装の選択**(VMware か Proxmox か)/ (B) **依存の抽象化**(具体
  インスタンスでなく能力に依存)。(B) は合成リソース/render で具体化でき素直——**抽象は収束の対象で
  なく render の入力**(人が書く語彙であって、収束が扱う実体ではない)。
- **実装の選択は誰が決めるか**:①運用者が明示 / ②**platform 層のポリシーが決める(ownership の自然な
  拡張・主候補)** / ③システムが最適化して選ぶ(= §11.3 で DRS に委譲した配置最適化の領域。Sakutei は
  持たない)。**②主 + ①上書き**が既存設計と整合。③には踏み込まない。
- **Schema への影響**:操作能力の `Capabilities`(TrueObserve/List 等)とは別に、役割能力
  `Provides() []Capability`(compute / block_storage 等)の宣言が要る。抽象と具体の適合は interface 型
  チェックとして検証する。
- **最大の危険は leaky abstraction**:「VM」は VMware と Proxmox で実際には異なる(スナップショットの
  意味・NW モデル・live migration・managed fields)。**抽象は最大公約数の能力に限定**し、実装固有機能が
  要るなら具体プロバイダを名指す。「隠してよい差異か、隠すと事故る差異か」の見極めがドメインごとの
  設計判断(VM は抽象化しやすい、ストレージは実装差が大きく慎重に)。
- **段階導入**:全面導入しない。`compute` のような抽象化しやすい少数の capability から始め、実装差の
  大きい領域は具体型のまま残す。

### 5.7 BulkObserve(バルク Observe・スケールアウト段1)

**決定(スケールアウト経路の第一級化・段1)**:§12.2 のスケール算術は「1 リソース = 1 コール」を暗黙の
前提にしており、これが単一デーモンの天井(≈Observe 時間)を規定していた。しかし現実の多くの権威は
**scope 内を1コールでまとめて返すバルク API** を持つ:vCenter PropertyCollector / Proxmox
`/cluster/resources` / AWS Describe* / NetBox の一覧 API 等。これを Provider の任意 capability として第一級化し、
「1 リソース1コール」前提の算術を書き換える。

```go
// prune 用の List(§5.1)と対をなす、Observe のバルク版(任意 capability)
type BulkObserver interface {
    BulkObserve(ctx, scope Scope) ([]Observation, error)  // scope 内をまとめて Read
}
```

- **`Capabilities.BulkObserve` で宣言**する(§4.3)。宣言した provider には Executor(§4.4)がバルク経路を
  使い、宣言していない provider には **SDK が per-resource `Read` へ透過的に fallback** する(コントローラは
  差を意識しない。`Lister` が任意 capability であるのと同じ宣言制)。
- **`Read` の契約はそのまま適用される**:戻りは `Observation`(Availability/State/Ready)であり、desired を
  見ない(§4.2)。バルクで取っても「取り方が変わるだけ」で、**毎周・全量を再観測する原則は不変**
  (§6.2)——観測 state レスに一切影響しない。
- **`List`(prune 用)との違い**:`List` は ID 集合(prune の前提・§4.1)、`BulkObserve` は状態(Observe の
  高速化)。バルク API が両方を1コールで返すなら1回で両者を満たせるが、型は別に保つ(prune の安全性は
  §8.6 の List 完全性検証に依存し、Observe の高速化とは要件が異なるため)。
- **非対称の明示**:バルク API を持つ層(REST/クラウド/CMDB 系)は単一デーモンで1万規模へ届きうる一方、
  バルクを持たない生 NW 機器(NETCONF/CLI)は**台数が物理的に有界**(1ラック 48 ポート等)であり、
  天井問題が自然に緩む。詳細な天井表は §12.2(BulkObserve 列)を正とする。

BulkObserve は **v1a で型だけ切る**(`Capabilities` フィールドと Executor の fallback 経路)。実際の
バルク実装は必要になった provider から書く(§24.1a)。**BulkObserve が満たすべき一貫性契約は §5.8 を正とする。**

### 5.8 BulkObserve の一貫性契約(SDK 契約として明文化)

**決定(第3ラウンド共通課題2 への対処。04/19 指摘)**:バルク API は「走査窓内のスナップショット」を
返すが、その窓は**全体で一貫していない**——DRS 稼働中の vCenter PropertyCollector は走査中に VM が移動し、
AWS Describe* は結果整合 + ページングで途中のリソースが古い版のまま返りうる。この非一貫を「正しさの問題」と
誤解すると、バルク経路を prune の根拠に流用したり周期をまたいで再利用したりして事故る。したがって
BulkObserve の契約を SDK レベルで明文化し、型で誤用を塞ぐ。

- **非アトミックスナップショット宣言**:BulkObserve の結果は「**走査窓内のある時点の各リソース**」であって、
  全体の一貫スナップショットではない。level-triggered が**次周期で自己修正する**ため、これは正しさの問題
  ではなく**揺らぎの問題**と位置づける(§6.2 の「毎周・全量再観測」がこの揺らぎを吸収する)。
- **complete-or-error**:ページングは**完走するかエラーを返す**(`List` の §8.6-a と同じ規律)。部分結果を
  「成功した BulkObserve」として黙示的に返すことを禁止する。途中打ち切りを検知したら成功として返さず error に
  する。
- **型分離の明文化(prune の根拠には使えない)**:BulkObserve は **Observe 専用**であり、**prune の根拠には
  一切使えない**。prune は従来どおり `Lister.List`(§4.1)+ §8.6 の完全性検証(件数急減 halt・ページング
  完走)を根拠にする。SDK は BulkObserve の戻りが集合差 prune の入力に渡る経路を**型レベルで塞ぐ**
  (§5.7 で `BulkObserve` と `List` の型を別に保ったのは、この誤用防止のためでもある)。
- **走査窓内遷移の吸収**:走査中の DRS 移動等が生む一時的不整合は、既存機構が吸収する——runtime placement は
  **managed fields 除外**(§11.2-4)が diff から落とし、それ以外の一時的な不一致は **converging / settle
  (Tier2・§10.4)** が自傷猶予として吸収する。BulkObserve は新しい吸収機構を要求せず、既存の縁切りに接続する。
- **SDK 等価性テストを標準装備**:fake provider 上で **`BulkObserve(scope) ≡ {Read(id) ∀ id ∈ scope}`** の
  等価性テストを SDK のテストハーネス(§5.2)に標準装備する。これを通ることが「**完全**階級 + bulk」を名乗る
  条件であり、バルク経路と個別経路が別の現実を返す実装バグを CI で捕まえる。
- **`observed_at` を各 Observation に刻む**:各 `Observation` に観測時刻 `observed_at` を持たせる(バルクでも
  個別でも同じ)。**bulk 結果は当該周期でのみ使用し、周期をまたいだ再利用は禁止**する——古いバルク結果を
  「現在状態」として次周期に持ち越すことは、排除したはずの観測 state を再導入する(§4.5・§6.2)。`observed_at`
  はこの周期越え再利用を検知・拒否する目印であり、揺らぎのデバッグ(いつ観測した値か)にも使う。

---

## 6. 収束ループ

### 6.1 モデル:毎周・全 DAG

```
毎 reconcile サイクル:
  1. render(§7)で desired を生成(rendered_revision 確定)
  2. desired のリソース集合を Observe + prune 対象 scope を List
       (並列・semaphore で並列度制限)
  3. Diff(集合 diff → フィールド diff)→ 変更が要るノード集合 D
  4. D の部分 DAG を構築
  5. Kahn で波(wave)に分解:
       while 残ノードあり:
         wave = 入次数0 かつ 上流が成功済み
         wave 内を並列 Apply(非同期・§6.4)
         失敗ノードの下流を skip 集合へ
  6. Diff の推移を記録(振動検知・ベースライン)
  7. jitter 付き sleep → 次サイクル
```

**render がサイクルの先頭に来る理由**:「何を Observe するか(リソース集合)」も「どの scope を List
するか(prune 対象)」も **desired から導かれる**。desired が確定していない時点では「全リソース」を
定義できない。render 入力は TTL キャッシュ(§7.4)なので、先頭に置いても毎周の外部呼び出しは増えない。
**なぜリソース単位ループ(k8s 流の独立コントローラ + 依存 gating)ではないか**:本システムの規模
(数十〜数千)では、**plan が常に全体で正しい**ことの価値が勝る。リソース単位ループは k8s 級の規模で
初めて元が取れる。

- 依存宣言は明示 `depends_on`(将来は参照からの暗黙抽出)。**設定ファイル上のキーは snake_case
  (`depends_on`)に統一**し、コード内フィールド名(`DependsOn`)との対応は Loader が変換する(§13.3)。
  v2.2 で `dependsOn` / `depends_on` が混在していたのを解消(整合性レビュー M-2)。
- **循環検出は理想状態ロード時に一度**。循環していればそのリビジョンを入口で拒否し、ループに持ち込まない。
- 基盤サービス(DNS/DHCP/NTP)は全リソースが暗黙依存する「依存の根」。全部に `depends_on` を書くのは
  非現実的なので、**`tier: foundation`** の粗い優先度帯(基盤層は常に通常層より先)で順序を与える。

**この毎周・全 DAG が「level-triggered な真実」の本体**である。§6.8 の早期収束パス(scoped reconcile)は
これを**置き換えず**、上に重ねる:トリガで早起きして影響サブ DAG だけを先に収束させても、正しさの保証は
あくまでこの定期周期が担う(トリガを取りこぼしても次の定期周期が拾う)。scoped run は「いつ収束するかの
最適化」であり、「何が真実か」はこの全 DAG 周期に一元化されている。

### 6.2 Observe:毎周・全部・間引きなし

並列化できる(goroutine)ので、コストは逐次和ではなく**最遅 Observe に漸近**する。毎周全部見ることが
観測 state レスの価値の源泉(drift 検知漏れゼロ)であり、間引きは本末転倒。

```go
sem := make(chan struct{}, 16)  // 同時 Observe 上限(相手 API への礼儀)
```

**割り切りが破綻する早期警戒線**(立つまで間引き・TTL・頻度別スケジュールは不要):
- Observe 全体が reconcile 間隔に対し無視できない比率(例:間隔 30s に対し 10s 超)
- 特定プロバイダの API 課金 / rate limit が Observe 頻度で効く
- リソース数が桁で増えた(数百→数千)

破綻したときの延命策は §12.2。

### 6.3 失敗伝播

依存が失敗・未達なら**下流を skip し、次サイクルで回収**する。level-triggered なので「今回諦めて次で
回収」が自然に効く。Ansible の `any_errors_fatal` 的な全停止より、部分適用 + 自己回復が優る。

### 6.4 長時間 Apply の非同期化 + in-flight 排他

VM 作成・image DL・firmware など分単位の Apply が wave に入ると、素朴なモデルは「ループが止まって
他の drift 検知が遅延」か「サイクルを重ねて同一グラフへ並行 Apply で競合」のどちらかに陥る。
実運用では **Observe コストより先に Apply 時間が刺さる**(DNS provider では見えず、VM/image で露呈)。

→ **Apply だけをサイクルから切り離す**:

```
・Observe/Diff ループは毎周・定刻で回り続ける(他リソースの drift 検知が長 Apply に巻き込まれない)
・Apply は goroutine で非同期実行し、対象リソースを in-flight 集合に入れる
・in-flight 中のリソースは converging と同じ扱い(再 plan・再 Apply しない、下流は skip → 次周回収)
・サイクルは重ねない(前周の Observe/Diff 完了後に次周)。長いのは Apply だけが継続
・in-flight 集合はインメモリのみ(S3)。再起動で消えても Apply 冪等 + 次周 Observe で安全に再導出
```

wave/skip セマンティクスに「Apply 完了はサイクル内でなくてよい」を足すだけで、モデルの構造は不変。

### 6.5 state の分類学

「state レス」は正確には**観測 state レス**である。他の state は限定的に持つ。判定基準は
**「喪失が事故になるのは S2 だけ → そこだけ永続化」**。

| クラス | 中身 | 置き場 | 喪失時 |
|---|---|---|---|
| **S1 観測 state** | リソースの current キャッシュ | **持たない** | — |
| **S2 進行 state** | Task/Workflow フェーズ・承認待ち・**シャードルーターの割当表** | **外部永続化必須** | 事故(割当表喪失=誤配送) |
| **S3 履歴 state** | 振動検知・ベースライン・backoff・Since・in-flight 集合・**allocation Lookup キャッシュ(§16.7)** | インメモリのみ | 検知が数周鈍るだけ(安全側)。**Lookup キャッシュ喪失時は allocator に問い直すだけ** |
| **S4 監査 state** | append-only ログ | 別ストア(収束経路から不可視) | 収束に影響なし |

これにより全クラスの再起動セマンティクスが定義済みになり、「再起動安全・DB 不要」という運用上の推論が
正しくなる。

### 6.6 per-resource backoff(Apply のみ)

Observe は毎周続ける(安価・drift 検知を維持)が、恒常失敗リソース(権限欠如等)を毎周 Apply で叩くのは
相手 API への攻撃になる。**(リソース, エラー種別)単位の指数 backoff + jitter** で Apply をゲートする。

- **transient**(API 一時エラー・ロック競合)→ backoff(上限 15 分程度)
- **permanent**(権限・バリデーション)→ 即 **stuck** 化し、**desired か provider version が変わるまで
  再試行しない**(原因タグの再利用)

backoff テーブルは S3(インメモリ)。再起動で消えても一回余分に叩くだけ。

### 6.7 収束は複数 Cycle に跨る(wave は Cycle 内の並列単位)

**wave(§6.1)は「1 Cycle 内の並列処理単位」であって「依存解決の最終形」ではない。**
物理サーバの起動・OS インストール・image DL のような長時間操作は 1 Cycle で終わらないため、
**依存の解放は複数 Cycle に跨って段階的に進む**のが正式なモデルである(§6.4 の in-flight と
§10.4 の converging の組み合わせで扱われる)。

そのため依存宣言は「存在するか」ではなく「**特定の観測可能な状態に達したか**」を条件にできる:

```yaml
depends_on:
  - type: proxmox_vm
    id: http-boot-1
    ready_condition: cloud_init_done    # 単なる存在ではなく、この condition の充足を待つ
```

- provider は `Observation.Ready`(§4.2)に名前付き condition の充足状況を返す。
- コントローラの `canApply` 判定は、上流の `Ready[condition]` が真になるまで**下流を skip し続ける**
  ——これは §6.3 の失敗伝播・下流 skip と**同じ機構の再利用**であり、新しいプリミティブは要らない。
- 毎 Cycle 再評価されるため、条件が満たされた Cycle で自然に下流が解放される。

例:`bare_server`(電源投入)→ `installed` → `data_network_verified` → `proxmox_node`(クラスタ join)
のように、1台の物理サーバの立ち上げが数十 Cycle に跨って進行する。

### 6.8 早期収束パス(トリガ収束)——「edge-triggered な起床、level-triggered な真実」

**決定(構成 07 の核心=周期律速への応答)**:本システムは level-triggered ゆえ、収束は**周期律速**である。
定期周期(§6.1)は「drift 検知漏れゼロ」を保証する一方、**人の意図変更(特に tenant 層のセルフサービス
API)の反映が次の周期まで待たされる**という構造的レイテンシを持つ。これに応えるため、**トリガによる
早起き(edge ヒント)+ 定期周期(level 保証)** のハイブリッドを第一級化する。k8s の informer watch
(edge ヒント)+ resync(level 保証)と同型である。

**貫く原則:トリガは「いつ収束するか」のヒントであり、「何が真実か」を一切運ばない。**

- **トリガ喪失は正しさを損なわない**(遅延のみ)。ヒントを取りこぼしても**次の定期周期が必ず拾う**。
- 早く起きた収束の中身は**通常と同じ新鮮な Observe → Diff → Apply**である。トリガはペイロード(変更後の
  値)を運ばず、「この scope が触られたらしい、今見に行け」としか言わない。**観測 state レスは無傷**
  ——トリガに含まれる情報を「現在状態」として信じることは一切しない。

**入口:`DesiredSource.Watch`(§13.5)**。`DesiredSource` に任意 capability として `Watch` を足す
(`Provider` に対する `Lister` と同じ宣言制)。`Watch` は `ChangeHint = {intent_revision, 触られた scope 群}`
のみを流す。FileSource なら git webhook / merge フック、DBSource なら書込通知が発火源。

**部分収束(scoped reconcile)**:トリガを受けたら全 DAG を回さず、影響サブ DAG だけを収束する:

```
ChangeHint 受信:
  1. render(§7)で新しい rendered desired を生成
  2. 新旧の rendered desired 同士を diff(★決定的・リアル非接触。ここでは Observe しない)
  3. desired が変わったリソースから影響サブ DAG を算出
  4. そのサブ DAG だけを Observe → Diff → Apply(通常の wave/skip セマンティクスをそのまま適用)
```

新旧 desired 同士の diff はリアルに触れないため安価で、**制御プレーンの追加レイテンシは秒オーダー**に
収まる。

**安全柵(速い経路を汎用修復チャネルにしない)**:

- **速い経路は「desired が変わったリソースのみ」を扱う**。新旧 desired diff から入るため、構造上
  **原因タグ A(意図)/ E(外部入力)のみ**が対象になる。道中で偶然見つけた **drift B は速い経路で修復
  せず、定期ループに残す**(B は周期律速のままでよい——drift の即時性より、速い経路の役割純化を優先)。
- **集合差 prune は速い経路で禁止**。prune の正しさは List 完全性(§8.6)に依存し、部分観測とは相容れない。
  テナントの即時削除は **DBSource 側の tombstone = 明示 absent**(`ExplicitAbsentOnly` の型・§8.2)で表現し、
  個別リソースの削除として扱う(集合差ではなく)。
- **rate limit・累積破壊予算のカウンタは定期経路と共有**する(§10.2)。速い経路がバイパスになってはならない
  ——両経路は同一の予算・同一の per-resource in-flight 排他(§6.4)を共有する。
- **同一 scope へのヒントは合流(coalesce)する**。実行時に**最新の desired を読む**ことで level 意味論を
  保つ(古いヒントで古い desired を適用しない)。scoped run の同時実行数には上限を設け、**溢れたヒントは
  次の定期周期へ縮退**する(速い経路が詰まっても正しさは定期周期が保証する)。

**ownership との整合**:

- **tenant 層(即時書込)が主客**。セルフサービス API の変更が秒で反映され、UX が周期待ちから解放される。
- **platform 層は「PR マージ = ヒント発火」**。承認直後に収束が起きる(ガバナンスは不変・レイテンシのみ
  短縮)。承認ゲート(§10.5)・PR 必須(§12.1)はそのまま効く。

**注意点(明記)**:render フェーズ1 の副作用(採番・§7.1)が速い経路にも乗るため、**allocator の可用性が
UX に直結する**。速い経路で新規払い出しが要る変更は allocator 応答が律速になる(§7.3 の不達時は前回保持で
安全側だが、UX としては遅延として現れる)。これはスケールアウトの次の天井(§12.4 の「allocator/Vault の
QPS」)と合流する論点である。**この天井は §16.7 で原則として塞ぐ**——定常収束は衛星コールゼロで回り、速い経路で
allocator が要るのは「新規採番を伴う変更」だけ。allocator 停止中は当該変更が `waiting-allocation` へ縮退し
(§16.8)、ヒントはキュー保持されて allocator 復帰後の周期で払い出される(正しさは損なわれず、UX 遅延のみ)。

**スライス**:**Watch seam + scoped run + 合流キューは v1a**。reconcile / plan とほぼ同一のコードパス
(Observe → Diff → Apply)を部分 DAG に適用するだけで、増分が小さいため(§24.1a)。**DBSource 連動の
`Watch`(書込通知)は従来どおり v2**(DBSource 自体が v2・§13.1)。v1a では FileSource の webhook/merge
フックによる `Watch` を対象とする。

---

## 7. render パイプライン

desired の生成は、**複数の外部権威に依存し、副作用を持ち、毎周走る多段パイプライン**であり、収束
ループと同格の複雑さを持つ。第一級コンポーネントとして扱う。

### 7.1 統一形:2フェーズ

```
render = フェーズ1(外部入力解決:★副作用あり・外部権威に依存)
           ├ inventory 合成(外部 CMDB)
           ├ シークレット世代解決(Vault metadata:値は読まない)
           └ allocation 解決(AllocationAuthority への払い出し)     ← v1b〜(v1a は呼ばない)
       + フェーズ2(決定的展開:副作用なし・同じ入力 → 同じ出力)
           └ 合成リソースの Expand(virtual_network → vxlan + vds + dhcp)
```

**Expand の居場所は render フェーズ2**(収束ループ内の入れ子コントローラではない)。決定性の要求
(同じ入力 → 同じ下位群)はフェーズ2の規律として一括で課す。

**バージョン帰属(v2.2 の矛盾解消)**:v2.2 では allocation(採番)を v2 としながら render フェーズ1 が
それに依存し、v1 スコープが実装不能だった。本改訂の決定は次のとおり:
- **allocation 解決(フェーズ1 の3つ目)は v1b〜**。**v1a では allocation 解決を呼ばない**
  (`AllocationAuthority` seam は切っておくが実装は無い)。
- **v1a の合成リソース展開(フェーズ2)は非採番=固定値展開のみ**。`from_pool` を使う展開は v1b から。
- したがって §7.3 の障害時挙動表の「allocation authority 不達」も **v1b〜の設計**とする。
- §16.3 の reserve→confirm(render 連動)も **v1b〜**。

### 7.2 intent revision と rendered revision

```
intent_revision   … 人が書いた理想(git commit hash / DB トランザクション ID)
rendered_revision … render 出力の世代(intent + 外部入力の解決結果すべてのハッシュ)
```

**desired は人の commit なしでも変わる**(シークレット rotate・割当変更・inventory 変更)。この2つを
型で分けることが、原因タグ(§10.3)の判定基盤になる。`DesiredSource.Fetch()` が返すのは intent_revision、
収束が比較するのは rendered_revision。

### 7.3 外部入力の障害時挙動(非対称原則の render 版)

**どの外部権威が不達でも「前回解決値を保持」する。空の desired を生成しない**(全削除事故の防止)。

| 外部入力 | 不達時 |
|---|---|
| inventory(CMDB) | 前回生成物を保持 |
| シークレット世代(Vault metadata) | 前回世代を保持 |
| allocation authority | 前回割当を保持(**新規払い出しは保留** → 未割当リソースは blocked で次周回収) |

Vault が sealed-restart 中は**世代解決も死ぬ**ため、SecretRef のキャッシュは**値だけでなく世代
metadata も含む**。

### 7.4 平常時の頻度規律

render が毎周実行になる結果、Vault metadata 読み(シークレット数ぶん)と authority Lookup(割当
リソース数ぶん)が**毎サイクルの外部呼び出し**になる。→ **render 入力は TTL 付きキャッシュ**
(staleness 上限 = TTL)。障害時挙動(§7.3)と合わせ、平常時・異常時の両方の規律が揃う。

### 7.5 合成リソース(複数の下位を1つに見せる)

「VXLAN 設定 + VDS 設定 → ユーザには仮想ネットワーク1つ」のように、下位リソース群を上位1つに束ねる。
IaaS カタログの各商品はこの合成リソースとして実現される。

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

**下位を別 Resource として依存グラフに乗せる**(1つの provider に押し込まない)。VXLAN と VDS は異種
システム(NSX / vCenter)を叩くため、単一 Apply に混ぜると部分失敗の扱いが破綻する。別 Resource なら:

- 展開 = **扇状 DAG**(wave / 依存 gate がそのまま効く)
- **下位の drift を個別に検知・修復**できる(粒度が保たれ、爆発半径と原因タグも粗くならない)
- 上位削除 → 下位を巻き取り(所有権・`pruneable`)
- 原子性が要る下位群は「1 wave で原子的に」指定できる

**上位ステータスの集約規則**:**最も悲観的な下位が上位を支配**する。

```
unknown > stuck > oscillating > drifted > blocked > converging > converged
```

fail-safe(非対称原則の再適用)。どの下位が原因かは追跡できる。**`stuck`(permanent 失敗・§6.6)と
`blocked`(依存未達・§17.4)を全順序に加える**(v2.2 は5値のみで、permanent 失敗の子が上位に昇る先が
無かった)。`stuck` は最悪級(人手を要する確定失敗)なので `unknown` の次に置き、`blocked` は「上流待ち
= 一時的」なので `drifted` と `converging` の間に置く。これで報告用の Event 分類(§17.4)と収束ステータス
集約の軸が一致する。

**採らない代替案:合成リソースを単一 Provider として実装する**(子を内部に隠蔽し、Apply 時に展開する)。
一見すると「controller は基本か合成かを意識せず扱える」利点があるように見えるが、**安全機構が効かなく
なる方向に働く**:合成が1つの Provider になると、コントローラから見える変更は **1件**になり、
子 50 個の削除が「1 リソースの変更」として rate limit(§10.2)を通過してしまう。
**子が独立した Resource だからこそ「50 件の削除」として halt できる。**
同じ理由で、下位ごとの drift 個別検知・原因タグの粒度・部分失敗の扱いも失われる。
→ **render フェーズ2 で展開し、子を理想状態に実在させる方式を正とする**(本節の方式)。

**展開ロジックの実装**:テンプレート(inventory データ + テンプレート)による展開を v1 の方式とする。
単純な変数展開(**v1a=非採番の固定値展開**)を超える処理(**プール採番・CIDR 計算・tier 別の自動割り当て
等は v1b〜**)は、テンプレートエンジンのカスタム関数、または展開後に走る処理層で埋める。カスタム関数は
**決定性を保つ**こと(同じ入力 → 同じ出力。§7.1)が要件で、その担保は `rendered_revision` 算出定義
(§26.1 A・着手前決定)に含める——決定性は原因タグ E の判定基盤なので「動かしながら決める」項目ではない。
将来的には `Loader`(§13.3)を CUE に差し替えることで、基本リソースと展開ロジックを同一言語で書ける
ようになる——それまでは実用性を優先する。

### 7.6 render 入力の incomplete≠empty(部分成功の防御)

**決定(安全性レビュー S-03 への対処)**:§7.3 の障害モデルは「不達 → 前回保持」の**二値**であり、
「200 OK だが中身が縮んで返る」(CMDB のレプリカ遅延・部分障害)を扱えない。歯抜けの inventory で
render すると、欠落分が「理想に存在しない」と展開され closed-world prune で削除対象になる——空 desired
ではないので「全削除事故防止」もすり抜ける。これは Observe 側の `unknown≠absent` と対をなす、
**desired 側の `incomplete≠empty`** の欠落である。

- **render 入力に "期待基数 / 前回比の急減検知" を入れる**。前回 N 件だった inventory が今回 X%(既定
  50%)未満に急減したら、それを「不達」と同じく**前回保持側へ倒す**(または halt + Event)。
- render 出力に対しても、§10.2 の異常検知の**逆方向=「削除方向 Diff の急増」**を専用に監視する。
- これは v1a に入れる(prune の安全性に直結し、後から足すと既存 provider の挙動が変わる)。

---

## 8. 削除セマンティクス

### 8.1 削除に要るのは state ではなく「境界の宣言」

```
消すべき = 現実にある集合 − 理想にある集合
```

鍵は「理想にある集合を網羅列挙できるか(closed-world assumption)」。

- **Ansible が `absent` を要求するのは state が無いからではなく open-world だから**
  (playbook は言及したものしか語らない)。
- **k8s が黙って消せるのは etcd に state があるからではなく**、ownerRef + label selector で
  「**管理する集合の境界**」を宣言できるから。
- → **closed-world は state ではなく境界の宣言で作られる**。観測 state レスと削除は両立する。

### 8.2 2モード

```go
type DeletionPolicy int
const (
    ExplicitAbsentOnly DeletionPolicy = iota // open-world:absent 明示が要る
    PruneWithinScope                          // closed-world:scope 内を網羅管理(Lister 必須)
)
```

**モード1 closed-world(prune)** — 現実の全集合を網羅 List できる種別(DNS zone 内 record / volume 群 /
VM 群 / FW ルール群)。

```yaml
scope:
  provider: dns
  zone: internal.example.com
  managed: true     # この zone 内は網羅管理。zone 外には触れない
records:
  - { name: web, ip: 10.0.0.1 }
  - { name: db,  ip: 10.0.0.2 }
# → zone 内で上記2つ以外の record は「消すべき」と確信できる(履歴 state 不要)
```

**モード2 open-world(explicit absent)** — 網羅 List が非現実的な種別(パッケージ / ファイル / 個別
設定エントリ)。個別宣言 + 消すには明示 `state: absent`。**この層は Ansible 方式が正しい**。

### 8.3 pruneable(二段構え)

closed-world 種別でも、破壊的な種別(volume / VM)は `managed:true` だけで消さず、**さらに削除
opt-in** を要求する。**`pruneable: false` を既定**とし、毎周回るループに `DROP` 相当を自動実行させない。

### 8.4 削除判定は3種類ある

| 共有リソース種別 | 削除可否の判定 | 例 |
|---|---|---|
| **離散・被参照** | 参照カウントがゼロ | image / テンプレート |
| **容量プールのメンバー** | **抜いても残余が充足 + migrate 可** | クラスタ内のホスト |
| **単純専有** | closed-world scope 内で理想に無い | テナントの VM |

2番目が重要:**VM が依存するのは個別ホストではなくクラスタ(容量プール)**なので、「VM が乗っている
ホスト」も**十分な残余容量があれば削除できる**(migrate すればよい)。参照カウントではなく容量充足性で
判定する(§11.3)。

### 8.5 順序付きコレクション

FW ルール・ACL・ルーティングポリシーは**位置依存の順序リスト**であり、unordered set-diff で扱うと
順序が入れ替わって穴が開く。規則:

- **リスト全体が原子単位**。scope = リスト、prune は置換に吸収、**Apply = 全置換**
  (candidate/commit と好相性)。
- ただし **plan の表示粒度はルール単位の差分に展開**する(**適用粒度 ≠ 表示粒度**)。

### 8.6 List 完全性の検証(prune の前提を実行時に守る)

**決定(安全性レビュー S-04・横断発見5 への対処)**:`unknown≠absent` は **`Read`(個別)の三値**で守られる
が、prune の根拠である **`List`(集合)のサイレント欠損**には無力である。ページングの途中打ち切り・
フィルタバグ・相手 DB のレプリカ遅延はいずれも「成功した List」として返り、欠けた ID が「理想に無い=
削除」に直結する(volume=データ消失 / VLAN=通信断)。§5.3 は「List の網羅性が prune の安全性を担保する」と
自認するが、**網羅性の実行時検証機構が v2.2 に無かった**。これを Provider 契約とコアの両面で塞ぐ。

- **(a) Provider 契約にページング完全性を明記**(§5.3):List は「next token が nil で終わったこと」を
  確認して返す。**途中打ち切りを検知したら成功として返さず、List を失敗(error)にする**。SDK に完走
  検証ヘルパを入れ、ページング処理を作者に手書きさせない。
- **(b) 前回観測比の件数急減 halt**(コア):`List` 結果の基数が前回 List 比で急減(既定 −50% 超)したら、
  **その scope の prune をその周は保留し Event を出す**(§7.6 の desired 側急減検知と対称)。前回 List
  基数は S3(インメモリ・非永続。喪失しても次周は保守側=prune 保留に倒れるので安全)。
  - **閾値は scope 単位で宣言化する(決定・v2.6・20 指摘の誤発火対策)**:急減閾値は既定 50% だが、
    `scope` に `list_drop_threshold: <fraction>` を宣言して scope ごとに上書きできる。**流動性の高い scope
    (テナントの短命 VM 群)は緩め、破壊が致命的な scope(ストレージ volume・VLAN)は厳しめ**に設定できる。
  - **`planned_change`(§10.9)との連動**:窓内・scope 内・種別一致の宣言済み削除/再作成(棚卸し削除・
    一斉再イメージ)は「急減」とみなさない——宣言済みの件数減は誤発火の原因(09 指摘)であり、planned_change
    が「これは意図された減少である」を宣言することで急減 halt を抑止する(宣言外の減少は引き続き halt)。
- **(c) List に三値を許す**:網羅を保証できない今回の List を実装が申告できる `ListStatus`(complete /
  partial / unknown)を返せるようにし、`complete` 以外の周は prune を保守側に倒す。`Read` の三値と対称。
- **(b) は v1a に入れる**(prune の安全性の中核)。(c) は SDK ヘルパとともに v1a、(a) の完走検証も v1a。

収束(Resource)だけでは**手続き的作業**(migration / firmware 更新 / ローリングアップグレード /
フェイルオーバー / ベアメタル導入)を表現できない。経路依存・非冪等・中断が致命的で、毎周回る収束
ループに載せると破壊する。手続きの重さに応じて3層に分ける。

| プリミティブ | 扱うもの | 例 | state | バージョン |
|---|---|---|---|---|
| **Resource** | 宣言的状態・収束 | DNS record / VM / volume / 設定 | 観測 state なし | v1a |
| **Task(単純型)** | 単一リソースの一回性手続き | migration | 進行フェーズのみ永続化(S2) | **v1.5** |
| **Task(多相型)** | 状態機械・リブート跨ぎ | firmware 更新 | 同上 | v2 |
| **Workflow(最小)** | 直列 + 不変条件 + 手動承認ゲート | ローリング/DR の骨格 | 進行位置 + invariant(S2) | **v1.5** |
| **Workflow(フル)** | 動的 select / 自動 canary / 自動段階ロールアウト | canary・自動ロールアウト | 同上 | v2 |

**バージョン帰属(v2.3・§24.1.5)**:20 構成の 12 が Task/Workflow に依存した事実を踏まえ、**単純 Task と
最小 Workflow(直列+不変条件+手動承認ゲート)を v1.5 に前倒す**。多相 Task 状態機械・自動段階ロール
アウトは v2 のまま。以下 §9.1(多相型)/§9.2 の動的 select・階層呼び出しは v2 の姿を先に記述している。

**共通原則:「いつ・どの順で変えるか」= 収束が判定。「どう変えるか」= Task/Workflow が実行。**
収束ループはトリガ・メンテ窓ゲート・依存順序・drift 検知・完了確認を担い、危険な手順そのものは
Task/Workflow に閉じ込める。実行中の対象は `converging` 扱いにして収束の介入を止める。

### 9.1 Task:単純型と多相型

**単純型**(migration):一度実行して completed 印を残す。完了印は**現実側**(DB 内 `schema_migrations`)に
置けば Observe で問える=観測 state レスと両立。

**多相型**(firmware 更新):中断不可・長時間・リブート跨ぎ。実質ステートマシン。

```
Idle → Staging(転送) → Verifying(hash/署名) → Applying(書込)
     → Rebooting → Confirming(バージョン再確認) → Done
                                            ↘ Failed → Rollback
```

- **進行中フェーズの外部永続化が必須**(S2)。ただし永続化するのは進行フェーズだけで、Done 判定は
  稼働バージョンを現実 Observe で問える。
- **リブート跨ぎ**:Applying 後に機器が消える → **unknown ≠ absent** で「リブート中」と解釈し、
  タイムアウト付きで再出現を待つ。Confirming で新バージョンを確認して初めて Done。
- **べき等な再開**:各相の入口で「もう済んでいるか」を現実 Observe で確認し、二重実行を防ぐ。

収束との接続(収束は判定とゲートのみ、手順は Task へ委譲):

```yaml
resource: switch-fw
target_version: "24.2R1"      # 収束が監視する「あるべき状態」(機器ファームウェア版)
update_policy:
  strategy: task              # 差分検知しても直接 Apply せず Task を起動するだけ
  window: "Sat 02:00-04:00"   # メンテ窓外では起動しない
  rollback_on_fail: true
```

### 9.2 Workflow:複数リソースまたぎ

ローリングアップグレードは Task では足りない。集合をまたぎ、**動的な状態依存**(隣が今 healthy か)・
**全体不変条件**(常に N 台 active / quorum 維持)・**部分失敗からの回復方針**を持つ。DAG は静的順序
しか表せない。

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

**too much ではない**:HA 上げは本来的に「順序・不変条件・失敗回復」を持つ問題で、どのツールでも要る
本質的複雑さ。Ansible で書けば `serial:` + `max_fail_percentage` + block/rescue で同じ概念を手書き
することになる。それを第一級プリミティブとして構造化するだけ。

---

## 10. 安全機構(level-triggered の急所)

### 10.1 なぜ構造的に危険か

edge-triggered なら暴発はイベント分で止まる。level-triggered は毎周・全リソースの desired vs actual を
突き合わせ差分を全部埋めにいくので、理想状態を1行間違えると次の1サイクルで数百リソースが同時に
「差分あり」判定 → 並列 Apply で一斉に潰す。**バグではなく設計通りの動作**(収束は理想が誤っていても
疑わない)。

> **「速く確実に収束する」能力が、そのまま「速く確実に破壊する」能力の裏返し。**
> しかも level 型は毎周繰り返す(手で戻しても次周また壊す)= 底なし。

危険は3軸から独立に漏れる:**量**(一度に多すぎ)/ **種類**(破壊的が混じる)/ **速度**(気づく前に完了)。

### 10.2 手綱は3階層

| 手綱 | 問い | 粒度 |
|---|---|---|
| **remediation mode** | そもそも自動で直すか | リソース / scope |
| **rate limit** | 直すとして、一度にどれだけか | グローバル / scope |
| **staged rollout** | その量を、どう刻んで適用するか | 変更ごと |

**remediation mode**:`auto` / `alert-only`。「常に自己修復」はオンプレでは強すぎる(緊急対応で人が
手を入れた変更を潰しかねない)。**`alert-only` は爆発半径ゼロ**で、最も保守的な本番リソース向け。

**rate limit**:
```yaml
safety:
  max_changes_per_cycle: { absolute: 10, percentage: 5% }
  on_exceed: halt-destructive-only   # halt-all | halt-destructive-only | warn-and-proceed
  # ↓ v2.3 追加:周期をまたいだ漸進的削除を塞ぐ累積予算(§10.2 の「時間窓予算」)
  destructive_budget:
    window: 24h
    max_deletes: 50            # 24h あたりの破壊的変更の総数上限
    max_scope_fraction: 20%    # 単一 scope の実在リソースの X% を超える削除進行で escalate
  on_budget_exceed: halt-all   # 予算超過 → rendered_revision 全体を凍結し人へ
```
閾値は**破壊的変更に重み付け**する(create 100 件は概ね安全、delete 10 件は止める)。`on_exceed` は
scope ごとに選択(本番 = destructive-only、dev = warn、最重要 = halt-all)。
**rate limit は in-flight(§6.4)を含む同時変更数で評価する**——さもないと「毎周 10 件ずつ発射し続けて
in-flight が積み上がる」抜け道になる。

**破壊的変更の累積時間窓予算(決定・安全性レビュー S-01 への対処)**:cycle 瞬間量だけを見る rate limit は、
**周期をまたいだ漸進的削除**(理想から 500 件が消えたまま、毎周 10 件ずつ 50 周で全消し)に構造的に無力
だった(`halt-destructive-only` は「その周の超過分を保留」であって「削除意図そのものの凍結」ではない)。
これを塞ぐため、cycle 瞬間量に加え **時間窓での累積破壊量**を見る:

- **24h 等の窓での破壊的変更総数**が予算(`max_deletes`)を超えたら **`halt-all`(理想変更そのものを凍結し
  人へ)** へエスカレーションする。
- **単一 scope の実在リソースの X%(`max_scope_fraction`)が削除進行中**、または **`halt-destructive-only`
  発動が M 周連続**でも同様に escalate する(「じわじわ全消し」の進捗率を見る)。
- **異常検知(巨大 Diff)の "止める" セマンティクスを明文化**する:巨大 Diff / 削除方向 Diff 急増を検知
  したら「超過分保留(漸進継続)」ではなく、**その rendered_revision 全体を凍結し、`planned_change` 宣言
  (§10.9)が対応するまで一切の破壊を保留**する(非対称原則に整合)。`Plan` が破壊性フラグを
  持つ(§4.6/§26.1)ことでこれを実装できる。
  - **明示的意図の表明は `planned_change` 宣言に統合した(v2.6)**:v2.5 までのコミット注記
    `approved-large-change: 50 creates` は、intent 内の第一級宣言 `planned_change`(§10.9)に**置換・統合**する。
    大量の正当作業(棚卸し削除・一斉再イメージ)は「コミットにタグを付ける」ではなく「対象 scope・変更種別・
    上限件数・時間窓・承認情報を宣言する」形で表明し、**宣言上限までの専用予算レーン**で実行される(§10.9)。
- 累積予算のカウンタは **S2(進行 state)相当**として永続化する(再起動で窓がリセットされると穴が復活する)。

**悪意/大量破壊への追加ゲート(決定・安全性レビュー S-11 への対処)**:安全機構は本来「誤操作」防御であり、
原因タグ A(意図)は「通す」側に倒れる。しかし侵害された reconciler は Apply を直接叩き、A タグとして
安全機構を素通りできる。したがって **`ownership: platform` の破壊的大量変更は、原因タグ A でも承認必須**
とする(「間違いは止め意図は通す」に「ただし大量破壊は意図でも人の承認を要する」を重ねる)。

**capacity gate(rate limit の一種)**:クラスタ使用率が閾値を超えたら新規の変更を止める。

```yaml
safety:
  capacity_gate:
    max_utilization: 80%
    on_exceed: halt-destructive-only
```

**是正は reconciler の外**——reconciler の役割は「危険域に入ったら止める」までで、「ではどうするか
(ホスト追加 / migrate / quota 締め)」の判断と実行は含まない。超過時は `CapacityThresholdExceeded`
Event(§17.4)を発火し、**是正は Workflow として別途明示的にトリガする**(自動連鎖させない)。

| 何をするか | 誰の仕事か |
|---|---|
| 使用率の観測 | ✅ reconciler(Observe) |
| 閾値超過時に新規変更を halt | ✅ reconciler(capacity gate) |
| 原因の判別(意図的増加 vs drift) | 人(原因タグ §10.3 の枠組みを使う) |
| クラスタ容量追加の意思決定 | ❌ 上位プラットフォーム / 人(§12.1 platform 層は PR 必須) |
| VM の他クラスタへの migrate | ❌ Workflow(手動トリガ推奨・§9.2) |
| 新規申請の quota 制限 | ❌ 上位プラットフォーム(§12.1) |

**異常検知**:固定閾値だけでは正当な大規模変更で誤発動する。**平常時の変更量ベースラインから外れたら
止める**。振動検知が「Diff が縮まない」を見るのに対し、これは「**Diff が突然巨大化した**」を見る
(同じ Diff 時系列監視の別用途)。

**貫く原則:デフォルト halt、明示で進む**。**明示的な意図表明は `planned_change` 宣言(§10.9)で行う**
(v2.5 までのコミット注記 `approved-large-change` を統合)。**間違いは止め、意図は通す**——ただし
大量破壊は意図(タグ A)でも人の承認を要する(上記「悪意/大量破壊への追加ゲート」)。

### 10.3 変更の「原因」区別(5分類)

収束ループが見るのは「desired ≠ actual」という**結果**だけで原因を区別しない。だが同じ差分でも
**なぜ生じたかで危険度と対応が正反対**。diff 結果に**原因タグ**を持たせる。

| 原因 | 判定 | 対応 |
|---|---|---|
| **A. 意図的**(人が理想変更) | **intent_revision が変化** | 適用してよい(規模制御は効かせる) |
| **B. drift**(現実が乖離) | **消去法**(diff が存在し、intent_revision / provider_version / rendered_revision がいずれも不変) | 自己修復 or アラート(remediation mode) |
| **C. ツール更新**(解釈が変化) | provider_version が変化 | **最も危険・見逃されやすい** |
| **D. カスケード**(A/E の依存波及) | 上流の変化に連鎖(DAG 下流) | plan 段階で波及範囲を可視化 |
| **E. 外部入力変化** | **rendered_revision は変化したが intent_revision は不変** | 下記 |

- **B は消去法で判定する**:観測 state を持たないため「actual が変化した」こと自体は観測できない
  (前回の actual を保持していない)。diff が存在し、他の3つの revision がいずれも動いていなければ、
  現実側が動いたと結論する。
- **C(幽霊 diff)**:provider の正規化ロジックを直しただけで、理想も現実も変わっていないのに既存全
  リソースが「今の解釈と違う」と判定され再適用される。**正しい修正でもバグでも同じ見た目**。
  → **provider version を diff 計算の入力に含める**。C を検知したら自動適用せず
  `ProviderUpgradeDiff` Event を発火し、dry-run で人が確認してから解禁。
- **E のサブタグ**:**E-secret**(世代 rotate)/ **E-alloc**(**intent 不変のまま割当だけ変わった場合**
  ——リソース追加に伴う新規払い出しは intent_revision が変わるので **A**)/ **E-inventory**(CMDB 変更)。
- **E の扱い**:帰属は「人の commit 由来ではない」と記録(A と混ぜない)。rate limit は不明度が高いので
  **破壊的側に重み付け**。
- **D の価値**:「image 1つ更新 → 依存 50VM が再作成」のようにカスケード規模が直感を超える。

**原因タグは per-resource で確定する(決定・安全性レビュー S-02 への対処)**:v2.2 は判定を Cycle
グローバルの revision(「intent_revision が変わった Cycle は全部 A」)で語れる余地があり、**1 Cycle 内に
A と E が混在するとき、E 由来(CMDB バグ)の破壊が無関係な人の小 commit に "相乗り" して A の緩い扱いで
通過する**穴があった。これを塞ぐため、**各リソースの diff 入力ハッシュを「intent 由来部分 / 外部入力
由来部分 / provider version 由来部分」に分解し、どの部分が動いたかでタグをリソース単位に決める**。
グローバル revision は「候補の絞り込み」にのみ使い、最終帰属は per-resource-diff で確定する。これは
`Plan` が原因タグを持つ(§26.1)ことで実装でき、v1a のスコープに入れる(安全機構の中核だから)。

**`rendered_revision` は decision-relevant な入力のみの決定的ハッシュとする(S-02b)**:TTL キャッシュの
ヒット/ミスや列挙順で値が揺れると、intent も現実も変わっていないのに E と誤判定(無用な halt)する。
逆に外部入力の変化がハッシュに反映されない実装バグは E を B と誤判定し、rotate したシークレットを古い
世代へ強制的に戻す。したがって算出定義(§26.1 A・着手前決定)で「何を含め何を含めないか」と
「TTL ヒット/ミスで揺れないこと」を厳密に規定する。

**version 埋め込みは登録必須契約(fail-closed。決定・安全性レビュー S-09 への対処)**:C の検知は
「provider version を diff 入力に含める」に全依存するが、v2.2 では埋め込みの付け忘れが「C 検知漏れ=
幽霊 diff が B に化けて自動適用」という**危険側に倒れる**緩い契約だった。またモノリシックバイナリでは
provider version = reconciler バイナリ version になり、本体の無関係な更新で全リソース誤検知しうる。→
- version の粒度は「**provider の diff/正規化ロジックのソースハッシュ**」を SDK が自動算出する(手動
  version でなく)。reconciler 本体 version とは分離する。これで誤検知(本体更新)・検知漏れ(付け忘れ)を
  両方防ぐ。
- **version 未埋め込みの provider は起動時に登録を拒否**(fail-closed)。Schema lint(§5.2)に
  「version 未宣言」を加える。
- C 検知時のデフォルト運用に **twin(§22.5)への自動流し込み**(幽霊 diff を本番前に観測)を組み込む。

**軽量 last-seen リング(決定・実装可能性レビュー H-4 への対処)**:運用者の最頻の問い「なぜ変わろうと
しているのか / いつからズレたのか」に B は「消去法(否定形)」でしか答えられず、観測 state レスの代償で
「actual がいつ変わったか」を原理的に言えなかった。→ **直近 N 周の actual スナップショットを S3
(インメモリ・非永続)にリングで保持し、drift 検知時に "前周は X だった" を Event に添える**。喪失しても
安全側(S3 の定義通り数周鈍るだけ)で、**永続化しない限り観測 state レスの看板は守れる**。これを
`sakutei status` の「なぜ変わるか」表示(原因タグ + 直近スナップショット差分 + 下流波及 D + provider
階級)と一体で v1a に入れる。

### 10.4 converging の3層検知

converging は「収束器の上の収束器」(SDN / VCF / アレイ / k8s は内部に収束ループを持つ)を扱う上で
最重要だが、多くの API に「作業中」の概念がない。3層 + deadline:

- **Tier1 ネイティブ**:vCenter task / NSX realization 等を写像(`Capabilities.Converging = true`)。
- **Tier2 自傷猶予(普遍的な床)**:自分の Apply 直後は型ごとの `settle: 30s / N サイクル` の猶予窓を
  置き、その間の不一致を drifted 扱いしない。**検知手段ゼロの provider でも成立**。
- **Tier3 振動からの推定**:M 回再 Apply しても Diff が縮まない → `oscillating` 判定し、そのリソースを
  **自動で alert-only に隔離**(綱引きの自動検疫)。
- **deadline 必須**:converging には timeout を付け、超過で `drifted` + Event に昇格(相手ループの
  stuck を取りこぼさない)。全て S3(インメモリ)で足りる。

### 10.5 段階適用 + 承認ゲート

既存 Workflow(wave + invariant + Event/sink + フェーズ永続化)の組み合わせで表現でき、新プリミティブは
不要。

```yaml
rollout:
  stages:
    - name: canary
      target: { cluster: least-critical }   # ①影響度順(明示指定)
      parallelism: 1                          # ③並列度をステージごとに
      gate:
        health_check: required                # ②自動ゲート(conformance + health)
        manual_approval: required             # ④人の承認(API)
    - name: broad
      target: { cluster: rest }
      parallelism: 5
      gate: { manual_approval: required }
  on_stage_fail: halt
```

**承認ゲートのフロー**:ステージ完了 → `StageAwaitingApproval` Event 発火 → sink 経由で人へ通知 →
Workflow は「待ち」フェーズで停止(S2 の永続化により留まる)→ 承認 API
`POST /workflows/{id}/gates/{gate}/approve`(却下 = halt)→ 次ステージ。タイムアウト(例 24h)で halt。
承認 API 自体は認可を持たない(誰が叩けるかは上位 IdP)が、**承認された事実は監査に記録**する。

**承認者要件の N-of-M 拡張(決定・v2.6・17 指摘の 4-eyes)**:v1.5 最小 Workflow の手動承認ゲートは v2.5 まで
「1人が叩けば通る」単一承認だった。規制環境(金融・§17 構成)が要求する 4-eyes(申請者≠承認者・複数承認)を、
**ゲート定義への `requirements` 追加**と**承認 API の拡張のみ**で実装する(新プリミティブ不要)。

```yaml
gate:
  manual_approval:
    requirements:
      quorum: { n: 2, of: 3 }        # N-of-M:3人の承認者候補のうち2人の承認で通過
      roles: [platform-admin]        # 役割制約:この役割を持つ者のみ承認可(役割判定は上位 IdP)
      separation: applicant_ne_approver  # 申請者≠承認者を強制(4-eyes の中核)
```

- **API 拡張**:`POST /workflows/{id}/gates/{gate}/approve` のボディに **承認者 ID + 署名**を載せる。
  ゲートは requirements を満たす承認が揃うまで「待ち」に留まり(S2)、揃った瞬間に次ステージへ進む。
- **役割制約**:承認者の役割判定は上位 IdP に委ねる(コアは認可を持たない・§12.1)。ゲートは「役割 X の署名が
  N 個必要」を requirements として保持するだけ。
- **申請者≠承認者(separation)**:`planned_change`(§10.9)や PR の申請者 ID と承認者 ID の不一致を強制でき、
  自己承認を型で塞ぐ。
- **全承認者を S4 監査に記録**:誰が・いつ・どの署名で承認したかの全員分を S4(§17.5)に append する。承認は
  「事実」として恒久記録され、原因タグの帰属(§17.5)と一体で「どの承認ゲートを誰が通したか」が追える。
- requirements を宣言しないゲートは従来どおり単一承認(team プロファイル既定・§10.10)。

### 10.6 変更凍結(freeze)

グローバルな変更凍結は、**全体を一時的に `alert-only` に倒すマクロ**(rate limit / gate 機構の一種)。
freeze 中も **drift 検知は続け、修復だけ止める**。

### 10.7 じわじわ型(部分的・二値でない)障害への言及

**設計上の通底原則(安全性レビュー横断発見)**:v2.2 の障害モデルは随所で「**瞬間・二値**」だった
——rate limit は cycle 瞬間量、render 入力は不達/到達、Observe は存在/unknown。しかし現実の障害は
**"じわじわ・部分的"** に起きる(周をまたぐ漸進削除・200 OK だが歯抜け・List のサイレント欠損・
レプリカ遅延)。この形の穴を **desired 側(§7.6)・List 側(§8.6)・削除累積側(§10.2)に対称に
「急減/部分欠損を疑う」検知を入れる**ことで横断的に塞ぐ、というのが本改訂の一貫方針である。

- 判定は「二値の閾値」だけでなく「**前回比の急変**」を併用する(前回 N → 今回 X% 未満で疑う)。
- 疑ったときの既定は**保守側**(前回値保持 / prune 保留 / halt)へ倒す(非対称原則の再適用)。
- converging も「二値の up/down」でなく「収束途中(部分的健全)」を第一級で扱う(§10.4 の3層)。

### 10.8 remediation schedule(時間帯連動の mode 自動切替)

**決定(20 構成横断発見4 への対処。構成 03/15/17 で必須)**:v2.2 は remediation mode を手動 freeze /
unfreeze でしか切り替えられず、保守窓文化の SLA 環境ではヒューマンエラー面が残った。**時間帯連動で mode を
自動切替する `remediation.schedule`** を安全機構に加える(v1.5)。

```yaml
remediation:
  default: alert-only              # 既定は最も保守的な側
  schedule:
    - window: "Sat 02:00-04:00"    # 保守窓のみ auto
      mode: auto
    - window: "*"                  # それ以外は alert-only(検知は続け、修復だけ止める)
      mode: alert-only
  tz: Asia/Tokyo
```

- **保守窓のみ auto、他は alert-only** を宣言的に表現する。窓の外で検知した drift は次の窓まで
  alert-only として持ち越される(level-triggered なので窓が開いた周に自然に修復される)。
- freeze(§10.6)は「今すぐ全体を alert-only に倒すマクロ」、schedule は「時間帯で mode を張る宣言」で
  あり、両者は重ねられる(freeze が最優先)。
- **段0 の Vault sealed-on-restart(§19.4)や保守作業と衝突しない**よう、窓は人手作業の時間帯と整合させる
  運用が推奨(§26.3 の運用パラメータ)。

**優先規則:most-restrictive-wins(決定・v2.6・20 指摘)**:複数の mode 決定源(schedule / ownership 既定 /
scope 明示 / freeze)が同一リソースに対して異なる mode を指すとき、**より制限的な mode が常に勝つ**
(`alert-only` > `auto`)。優先順位でなく「最も制限的」で解決するのは、**安全側への非対称原則(§2-3)を
mode 解決にも一貫適用する**ため——どこか1つでも「止めろ」と言えば止まる。

- 具体例:schedule が保守窓で `auto` を張っていても、`ownership: platform`(既定 alert-only 寄り・§12.1)や
  freeze(全体 alert-only)や scope 明示 `alert-only` のいずれかが効いていれば、そのリソースは `alert-only`
  に落ちる。**auto は「全ての決定源が auto を許した」ときだけ成立**する。
- これにより「schedule で窓を開けたつもりが、実は freeze 中/platform だった」という取り違えが**安全側に倒れる**
  (窓を開けたのに動かない、はあっても、止めたつもりが動く、は起きない)。

### 10.9 計画変更宣言(planned_change)——大量正当作業と安全予算の衝突への専用解除弁

**決定(v2.6・11/13/09 指摘への応答)**:安全機構(累積時間窓予算・§10.2 / cycle rate limit / List 急減 halt・
§8.6)は「誤操作・暴発」を止めるために破壊量を絞るが、これが**大量の正当作業**(棚卸しによる 500 リソース削除・
HPC 500 台の一斉再イメージ・テナント一括退去)と正面衝突する——正当な作業なのに毎周予算に阻まれて進まない、
あるいは予算超過で全体が halt する。かといって予算を恒常的に緩めると暴発が止まらない。この衝突を、**intent 内の
第一級宣言 `planned_change` による専用解除弁**で解く。

**`planned_change` は intent 内の第一級宣言**であり、**それ自体が PR 承認を経る**(= 人の意図の明示)。

```yaml
planned_change:
  scope: { site: hpc-a, cluster: gpu-pool }   # 対象 scope
  kind: reimage                                # 変更種別:delete | update | reimage | recreate 等
  max_count: 500                               # 上限件数(この宣言で許す破壊/変更の総数)
  window: "2026-08-01 00:00 .. 2026-08-01 06:00"  # 時間窓(この間だけ有効)
  approval: { pr: 1423, approvers: [alice, bob] }  # 承認情報(§10.5 の requirements を満たす)
```

**効果(バイパスではなく専用予算レーン)**:

- **窓内・scope 内・種別一致の変更は、宣言上限(`max_count`)までの専用予算レーンで実行される**。これは
  通常の累積時間窓予算(§10.2)・cycle rate limit とは**別枠で計上**され、通常予算を消費しない。
  **バイパスではない**——上限件数は宣言され、超えれば止まる。予算を「無効化」するのでなく「宣言分だけ別レーンを
  開ける」。
- **List 急減 halt(§8.6)の抑止**:宣言済みの削除/再作成による件数減は「急減」とみなさない(§8.6-b の
  planned_change 連動)。棚卸し削除が誤って急減 halt を誘発する問題(09 指摘)がこれで解ける。
- **HPC 一斉再イメージの delete 誤計上を解消(13 指摘)**:再イメージは内部的に delete+create に見えて破壊予算を
  食い潰すが、**変更種別 `reimage` を宣言**すれば「これは再イメージであって純粋な削除ではない」と分類され、
  500 台の一斉再イメージが破壊予算に delete としてカウントされない。
- **窓を過ぎたら自動失効**し、通常予算に戻る(宣言の効力は window に閉じる。恒久緩和にならない)。

**宣言外の変更は引き続き通常予算で評価される**——`planned_change` が site-a の reimage を許可していても、
同じ周期で起きた**宣言外の delete(別 scope・別種別・上限超過分)は通常の rate limit / 破壊予算 / halt が効く**。
すなわち **「宣言中でも、宣言外の暴発は止まる」**。これが「バイパスではなく専用レーン」の意味である。

- **既存 `approved-large-change` タグを統合(置換)**:v2.5 までのコミット注記 `approved-large-change: 50 creates`
  (§10.2)は、この `planned_change` 宣言に**統合・置換**する。コミットへのタグ付けより intent 内の構造化宣言の方が、
  対象・種別・上限・窓・承認を機械可読に持て、§10.2 の halt-all 凍結の解除条件としても直接使える。
- **スライス**:`planned_change` の型と専用予算レーンの評価は **v1a**(安全機構の中核であり、後付けすると予算
  計上経路が変わるため)。承認情報の requirements 連携(§10.5)は v1.5(手動承認ゲートと同じ段)。

### 10.10 運用プロファイル(solo / team / enterprise)——規模別の安全既定値セット

**決定(v2.6・01/02 指摘「1名運用に承認儀式が空回り」への応答)**:v2.5 までの安全既定(platform 破壊的変更に
PR 承認必須・§10.2 / §12.1)は、チーム運用を前提にしていた。だが **1名運用(ホームラボ・スモールオフィス・
段0-minimal)では「自分で書いて自分でレビューする」承認儀式が空回り**し、かえって運用を阻害する。逆に規制環境は
より厳しい既定(N-of-M)を最初から要る。**規模別の既定値セット = 運用プロファイル**を定義し、`ownership` の
書込ポリシー・承認ゲート・remediation の既定を規模で切り替える(個別上書きは常に可能)。

| プロファイル | platform 破壊的大量変更 | PR | 承認ゲート既定(§10.5) | 想定規模 |
|---|---|---|---|---|
| **solo** | **cooling-off(遅延実行)で代替可** | 自己マージ可 | 承認者要件なし(単一 or cooling-off) | 1名運用・ホームラボ・段0-minimal |
| **team**(既定) | 単一承認必須(現行) | PR 必須(他者承認) | 単一承認 | 少人数チーム |
| **enterprise** | N-of-M + 役割制約を既定化 | PR 必須 | N-of-M(§10.5)+ separation | 規制・大規模 |

- **solo:cooling-off(遅延実行)による承認代替**:platform の破壊的大量変更の事前承認を、**cooling-off = 遅延
  実行**で代替できる。**`planned_change`(§10.9)の宣言から N 時間後に自動実行**され、**その間いつでも halt 可能**
  (気づいたら止められる)。「他者のレビュー」を「時間差 + 自己による中断機会」で置き換える——1名運用では後者の方が
  実効的な安全になる。**監査記録は team と同等**(誰が・いつ宣言し・いつ実行されたかを S4 に残す)。PR は
  **自己マージ可**(他者承認の強制を外す)。

```yaml
profile: solo
solo:
  cooling_off: 2h        # planned_change 宣言から 2h 後に自動実行(その間 halt 可能)
```

- **team(既定)**:現行どおり。platform は PR 必須・単一承認・破壊的大量変更は A タグでも承認必須(§10.2)。
  プロファイルを指定しなければ team。
- **enterprise**:N-of-M + 役割制約(§10.5 の `requirements`)を既定化する。個別ゲートに書かなくても、
  platform の破壊的変更は自動的に 4-eyes(separation)を要求する。
- **段0-minimal(§19.8)と solo の対応**:段0-minimal(単一ノード・Vault/broker なし)は 1名運用と物理的に
  一致するため、**段0-minimal では solo プロファイルを既定とする**。承認儀式が空回りせず、cooling-off が
  「1名でも安全に大量変更を回す」実効弁になる。規模が育って段0(3ノード)+ チーム運用へ移ったら team/enterprise
  へ切り替える(プロファイルは宣言で切替可能)。
- **プロファイルは既定値セットであって上書きを禁じない**:solo でも特定 scope に N-of-M を明示できるし、
  enterprise でも dev scope を緩められる。プロファイルは「よく使う既定の束」を1語で選ぶ糖衣であり、個別の
  `ownership` / ゲート / remediation 宣言が常に優先する。

---

## 11. オンプレ横断項目

ほぼ全レイヤが収束モデルに向くが、レイヤ横断で効く落とし穴があり、Resource 抽象そのものに関わる。

### 11.1 レイヤ別適合

| レイヤ | Observe 手段 | 適合 | 主な地雷 |
|---|---|---|---|
| NW 機器 | NETCONF/gNMI/RESTCONF、旧機種は CLI scrape | ○ | 管理断ロックアウト / 順序付きリスト |
| LB/FW | F5 iControl / PANOS / Forti API | ○ | FW ルールは順序付きリスト |
| SDN | NSX-T policy / ACI / OVSDB | ◎ | **二重ループの綱引き** |
| ストレージ | Pure / ONTAP / PowerStore REST | ◎ | volume 削除 = データ消失 |
| ハイパーバイザー | vCenter(govmomi)/ libvirt | ◎ | **DRS/HA が動かすフィールドを触るな** |
| VM 内 OS/ソフト | SSH/WinRM or 常駐 agent | ○ | Observe 手段の確保 |
| VM 内 DB の中身 | — | ✕ | **収束外**(Task 行き) |
| コンテナ | podman/docker API | ◎ | k8s 上なら二重ループ無意味 |

**「薄いオーケストレータ」で済むのは上位が宣言的な部分(SDN / VCF / アレイ)だけ。生の NW 機器・FW・
guest OS が "泥臭い" 層で、実装の8割はここ。**

### 11.2 横断5項目

1. **収束器の上の収束器**:相手も収束ループを持つ。**converging を drift と誤認するな**(誤認して
   再 Apply すると綱引きで永久に収束しない)。→ §10.4。
2. **順序付きコレクション**:→ §8.5。
3. **破壊的差分**:→ `pruneable:false` 既定(§8.3)。
4. **managed fields**:DRS の vMotion・動的 IP・HA 電源状態を「理想と違う」と収束すると**プラット
   フォームと永久に喧嘩**する(非ゼロ diff が恒常化)。→ **宣言した intent のフィールドだけ管理し、
   runtime placement 等は無視**(Terraform `ignore_changes` / k8s server-side apply 相当)。
5. **管理プレーンのロックアウト**(NW 機器):config push で自分の管理到達性を切ると二度と直せない。
   → Junos `commit confirmed` / IOS-XR commit+rollback を使い、「確認が来なければ自動ロールバック」を
   Apply 契約に組み込む(`Capabilities.CommitConfirm`)。

### 11.3 ハイパーバイザーと容量プール

**VM が依存するのはクラスタ(容量プール)であって個別ホストではない。**

```yaml
resource: vm
ownership: tenant
tenant: acme
id: web-1
placement:
  cluster: prod-cluster                 # ★依存先はクラスタ(プール)
  demand: { cpu: 4, memory: 8 }         # 要求する容量
  # 現在どのホストに居るかは runtime = managed field。理想状態に書かない
```

- **配置は DRS 等に委譲**(ビンパッキング / フラグメンテーション / migrate 順序を解かない)。
  捨てた scheduler を再発明しない。DRS が「退避不能」と言えば削除 Workflow が失敗する、でよい。
- **ホスト削除** = `remaining = cluster.capacity − host.capacity` と `Σ vm.demand ≤ remaining` の粗い
  充足チェック → 「メンテモード投入 → VM 退避(DRS が実行)→ invariant 確認 → ホスト除去」の Workflow。
  **十分な残余があれば、VM が乗っていてもホストは削除可能**。

**「どのクラスタに置くか」も reconciler の外**——上記「クラスタ内のどのホストか」を DRS に委譲したのと
同じ原理を、一段上のレイヤにも適用する。経路は2つあり、**reconciler から見れば区別がつかない**
(どちらも「理想状態に cluster が既に書かれている」という同じ入力になる):

- **経路A(v1)**:ユーザが理想状態に `placement.cluster` を直接指定する。
- **経路B(v2 以降)**:上位プラットフォームがクラスタ選択ロジックを実行し、確定済みの理想状態を
  `DesiredSource` 経由で渡す(§12.1)。

**reconciler にクラスタ横断の最適化ロジックを持たせない**——これは §12.2「全体を統べるメタ収束器は
作らない(k8s federation の轍)」の適用例でもある。

| 動作 | 責務 |
|---|---|
| 指定クラスタの残余容量充足チェック(二値判定) | ✅ reconciler(容量プール判定の延長) |
| 削除時「退避先に十分な残余があるか」のゲート判定 | ✅ reconciler |
| クラスタ横断の配置最適化計算 | ❌ 上位プラットフォーム / 専用ツール |
| 「どこが最適か」のビンパッキング計算 | ❌ DRS / 専用ツール |

### 11.4 外部オーケストレータとの境界(「収束機構の上の収束」の第一級化)

**決定(20 構成横断発見3 への対処)**:UniFi・DRS/ACI・Heat/Neutron・k8s operator・MHA・VNFM・
AWS マネージド等、**内部に自前の収束ループを持つ外部オーケストレータ**が管理対象の一部を所有する構成が
20 構成中の多数で現れた。§10.4(converging 3層)と §11.2-4(managed fields)で「縁切り」はできるが、
v2.2 には **(a) 外部が所有する scope を管理境界から除外する明示概念**と **(b) 協調(状態連携)の seam** が
第一級として無く、二重管理事故の主因になりうる。これを2つの最小概念で埋める。

**(a) 外部所有 scope の除外宣言(`external_owner`)**:ある scope / フィールド群を「外部オーケストレータが
所有する」と宣言し、**Sakutei は Observe(可視化)はするが Apply しない**(収束対象から外す)。managed
fields(§11.2-4)がフィールド単位の除外なのに対し、これは **scope / リソース種別単位の除外**であり、
「この収束器は触らない」を宣言的に固定する。除外宣言は理想状態ロード時に検証する(prune・循環検出と
同じ入口)。**v1a では最も基本の "alert-only 相当の Apply 抑止" として入れ**、協調の seam は下記(b)へ。

```yaml
scope: { cluster: prod, subsystem: vm_placement }
external_owner: drs            # DRS が所有。Sakutei は Observe するが Apply しない
```

**(b) 協調の seam(状態連携の最小定義)**:縁切りだけでなく、外部の収束結果を **`ready_condition`(§6.7)
として Observe で待つ**ことで協調する(強い制御でなく結果整合)。例:MHA がフェイルオーバーを完了した
ことを Observe し、その ready を下流の gate にする。**Sakutei から外部へ指示は出さない**(外部の内部
ループには介入しない)——協調は「観測して待つ」の一方向に閉じる。これは §12.2 のシャード間依存
(「観測 ready 待ち」)と同じ機構の再利用であり、新プリミティブは要らない。seam は v1a で切り(型として
`ready_condition` を外部リソースにも張れるようにし)、外部固有の写像実装は v1b〜。

---

## 12. マルチテナントとスケール

### 12.1 2層所有権

マルチテナントは4つの関心事の束。**(1) 論理分離だけが収束エンジンの責務**で、残りは委譲する。

| 関心事 | どこ |
|---|---|
| **1. 論理的分離(境界)** | **収束エンジン**(prune・DAG がテナント境界を越えないことの保証) |
| 2. 認可 | 上位プラットフォーム / IdP |
| 3. quota・課金 | 上位プラットフォーム |
| 4. セルフサービス窓口 | 上位プラットフォーム(intent UI) |

上位プラットフォームは `DesiredSource` の一実装(認可・quota 適用済みの理想状態を tenant ラベル付きで
投入)。**コアは無変更**。

**所有権は2層**:

- `ownership: platform` … 共有基盤(クラスタ / NW コア / 共有ストレージ)。誰か一人の所有ではない。
  **platform 層の理想状態に一度だけ宣言**する(三重定義を防ぐ)。
- `ownership: tenant` + `tenant: <name>` … 専有(VM 等)。

テナントの closed-world scope は**自テナント所有分に限定**され、**prune が共有基盤を消さない**。
テナントは共有基盤を **placement 参照**するが**所有しない**。
DAG は**扇状**(共有基盤 = 全テナント共通上流、テナント間はエッジなし = 独立・並列)。

**`ownership` が決めるもの(原則6の実体)**:

| 層 | 書き込み経路 | remediation | rate limit | 爆発半径 | 供給源 |
|---|---|---|---|---|---|
| tenant | **即時(レビュー無)** | auto でよい | 緩め(warn) | 小(砂場内) | DBSource |
| platform | **PR 必須(人が承認)** | alert-only 寄り | 厳(halt) | 大(全体) | FileSource(git) |

- **テナントがレビュー不要な理由**:scope で砂場境界を切ってあり、他テナントも基盤も壊せない。
  **テナント内でレビューが要るかはテナントの自己責任**(必要ならゲスト側が自前で用意する)。
  プラットフォームは砂場の境界だけ保証し、砂場内の統治は委ねる。
- **テナントが基盤に触れる操作**(採番消費・容量消費)は、**authority(採番)と quota(容量)という
  事前に区切られた枠**を通すことで即時のまま安全。枠を超える要求だけ platform 層の承認に上がる。

### 12.2 スケール

**単一デーモンの天井 ≒ Observe 時間**。ただし **1回の Observe レイテンシは provider の階級で1桁以上違う**
ため、天井は単一の数字では表せない(実装可能性レビュー H-2 の指摘。v2.2 の「200ms 前提で数千〜1万」は
高速 REST 限定だった)。設計書が主対象と宣言する泥臭い層(生 NW 機器・CLI・Ansible)ではレイテンシが
1.5〜6s に達し、実用上限は数百台に落ちる。**実機レイテンシで書き直した天井**(10,000 リソース・並列度16・
早期警戒線 §6.2「間隔30sに対し Observe 予算 10s」= 上限台数 ≈ 10s × 16 / レイテンシ):

| Observe 手段 | 1回のレイテンシ | 10,000 台の1周 | 30s 間隔に収まる上限台数 | **BulkObserve あり(§5.7)** |
|---|---|---|---|---|
| 高速 REST(クラウド的) | 0.15s | 1.6 分 | ~1,067 | **Describe* 系で1万規模へ**(1コール/scope) |
| vCenter / govmomi(重い SOAP) | 0.8s | 8.3 分 | ~200 | **PropertyCollector で数千〜1万へ**(1コールでクラスタ全 VM) |
| Proxmox(REST) | 0.3s | 3.3 分 | ~533 | **`/cluster/resources` で1万規模へ**(1コールでクラスタ全量) |
| NETCONF / gNMI(NW 機器) | 1.5s | 15.6 分 | ~107 | バルク無し。ただし**台数が物理的に有界**(下記) |
| CLI scrape(旧機種) | 4.0s | 41.7 分 | ~40 | バルク無し。同上 |
| Ansible module(fork+SSH) | 6.0s | 62.5 分 | ~27 | バルク無し。同上 |

- **天井は provider 階級別に読む**:完全/標準階級(REST・自前)= 数千〜1万、**Ansible/CLI ラップ =
  数百〜千**。v2.2 の単一「数千〜1万」は撤回する。
- **BulkObserve(§5.7)が算術を書き換える(スケールアウト段1)**:バルク API を持つ層(vCenter
  PropertyCollector / Proxmox `/cluster/resources` / AWS Describe / NetBox 等)は「1 リソース1コール」前提の
  上限を突き抜け、**API 系で単一デーモン1万規模**へ届く。**毎周・全量再観測の原則は不変**で、取り方が
  「N コール → 1コール」に変わるだけ(§5.7)。
- **非対称の明示**:バルクを持たない生 NW 機器(NETCONF/CLI)は上限が数十〜百台に見えるが、これらは
  **台数が物理的に有界**(1 デーモンが受け持つラック/サイトのポート数は自然に上限される)であり、REST
  系のような「テナントが無限に増やせる」スケール圧を受けにくい。**バルクを持つ層は数が伸びるが1コールで
  畳め、バルクを持たない層は伸びない**——この非対称が天井問題を実際上緩めている。
- NW 機器は同時セッション数が2〜8に制限される機種が多く、機器単位で並列度を上げられない。Ansible は
  fork コストがあり goroutine 並列の恩恵が I/O 待ちほど効かない(CPU/メモリが先に律速しうる)。
- **`ready_condition` の複数 Cycle 依存(§6.7)は、1 Cycle が数分〜数十分になる規模では依存解放に数時間
  かかる**。遅いこと自体は設計上正しいが、運用ドキュメントで期待値を明示する。

**tier 別 Observe 頻度の最小形を v1 に前倒す(決定)**:泥臭い層を主対象と宣言する以上、間引きを
「破綻してから」(v2)では遅い。**tier 別の Observe 頻度(安定リソースは N 周に1回)の最小形を v1b の
Observe スケジューラ seam として入れる**(型に頻度の余地を残す)。完全な間引き/watch 併用の最適化は
v2 のまま。§6.2 の「毎周全部」は既定として維持し、頻度は tier で上書き可能にする。

**分散の前に単一を延ばす**(数万まで届きうる):
- **Observe 間引き**(安定リソースは N 周に1回。drift 検知が数周遅れるが許容。最小形 v1b・上記)
- **watch 併用ハイブリッド**(provider が watch を持つなら drift 即知 + 保険の低頻度ポーリング。v2)
- **tier 別頻度**(v1b で seam、最適化 v2)

**スケールアウト = シャーディング**(分割軸が重要):

| 軸 | スケールするか |
|---|---|
| **シャーディング**(tenant / site / cluster で水平分割) | **する(推奨)** |
| リーダー選出 active/standby | **しない**(可用性用。active は1台) |
| 機能分割(Observe 係 / Apply 係) | しない(過剰) |

- 分割キーは既存の `scope` / `tenant` / `ownership` がそのまま使える。
- **各シャードは shared-nothing で独立収束**(デーモン間協調不要 = **分散合意を中枢に戻さない**)。
- **共有基盤は専用の platform シャードが1つ所有・変更**し、テナントシャードは **read-only で参照**
  (書き手が1つなら綺麗)。**所有権の論理境界がそのままシャードの物理境界**になる。
- **シャード割当の方式は §12.4(claim-as-intent)を正とする**:v1b では「割当そのものを git の intent
  として宣言し、各デーモンが `--claim` で自分の担当分を pull して収束する」静的シャーディングを採る。
  下記「上位=ルーター(割当表=S2)」は **動的ルーター/自動リバランスを行う v2 の姿**であり、§12.4 が
  v1b 段階としてこれを上書きする。
- **上位は司令塔ではなくルーター(v2)**:「どのリソースがどのシャードか」を動的に引く薄い層(状態 =
  割当表 = S2)。自動リバランスを伴う。全体を統べるメタ収束器は作らない(k8s federation の轍を踏まない)。
- **シャード間の依存は弱化する**:シャード内は plan 順序による gating だが、**シャードをまたぐ依存は
  「観測 ready 待ち」**(結果整合)。platform リソースの readiness をテナントシャードが Observe で
  gate する。強い順序保証はシャード内に閉じたときのみ成立する。

**原則の格上げ:「シャード間協調は現実を介して行う」**。シャード同士は**互いに問い合わせない**。
相手の作った**現実を Observe して gate する**(上記「観測 ready 待ち」)。これは §12.2 の妥協ではなく、
**観測 state レス思想の延長**として明文化する——シャード間 RPC / 状態交換は正しさに一切関与しない
(交換した状態を信じることは、観測 state を持つのと同じ罠だから)。**共有物はすべて衛星に外出し済み**
(採番 = allocator・秘密 = Vault・intent = git)であり、**この分解がそのままスケールアウトの分解になる**
——シャードは「共有 state を持たない独立収束器」であり、共有が要るものは既に衛星へ切り出されている。

**単一 writer / 単一権威に fencing を必須化する(決定・安全性レビュー S-08 への対処)**:shared-nothing +
「分散合意を中枢に戻さない」は平常時は綺麗だが、**分断時に単一性を保証する機構が v2.2 に無かった**。
platform シャードの active/standby フェイルオーバーで旧 active が生きていると(GC pause / 片側分断)、
2つの writer が共有基盤(NW コア・共有ストレージ)へ書き、順序付きコレクション全置換(§8.5)と組み合わ
さって FW ルール/ルーティングが発振し全機器全盲を誘発しうる。→
- **platform シャード(単一 writer)に fencing token を必須化**する。共有基盤 provider の Apply は
  token 検証を通し、**古い世代の writer の書き込みを拒否**する(storage/lease fencing)。「**合意は持たない
  が fencing は持つ**」は両立する(fencing は合意より軽い)。
- 割当表(シャードルーター・S2)にも **fencing token / 世代番号**を持たせ、復元・再割当時に古い世代を
  fence する(§12.3 の `revision`/`leaseUntil` と同型を割当表にも適用)。**静的シャーディング(§12.4)では
  割当表そのものを持たず、二重 claim 検知に fencing 世代番号だけを S2 として使う**(新しい可用性部品を
  増やさない)。動的ルーター(v2)を採る場合の割当表 fencing はこれと同型。**fencing フィールドは型定義時
  から最初から持たせる**(後付けは危険)。

| 規模 | 構成 |
|---|---|
| 〜数千 | 単一デーモン・毎周全 Observe |
| 数千〜数万 | 単一 + Observe 間引き / watch 併用 / **BulkObserve(§5.7)** |
| 数万〜 | **静的シャーディング(§12.4・`--claim`)**(tenant / site / cluster) |
| 地理分散 | サイトごとシャード + **claim の git 配布 / S2-free シャード(§12.5)** + 動的ルーター(v2) |

> **注(衛星は Observe 天井の算術に乗らない)**:上表の天井は **Observe 時間**(リアル再観測)で規定される。
> 共有衛星(allocator/Vault)への QPS は**このスケールとは別軸**であり、**フリート規模ではなく変更レートに
> 比例する**(§16.7)——定常収束は割当 Lookup キャッシュ(S3)と secret TTL キャッシュで**衛星コールゼロ**で
> 回る。したがって「シャード数を増やすと衛星が詰まる」は churn とポータル申請ピークで sizing すれば回避でき、
> 衛星は Observe 天井の算術に直接は乗らない(§16.7/§12.5)。

### 12.3 分散 Executor 時の段階縮退(将来設計)

実行体を別ホストへ出す(`RemoteExecutor`)と、**ネットワーク分断が常態化**する。このとき agent が
理想状態をキャッシュして自律動作すること自体は正しい(制御面が落ちてもデータ面は動き続ける)が、
**キャッシュした理想は分断中に陳腐化しうる**——「消せ」という古い指示を持ったまま自律収束すると、
既に人が削除したはずのものを復活させ続ける。逆に、既に消された指示を知らずに削除を実行する危険もある。

→ **非対称原則(§2-3)の分散版**として、段階縮退を定める:

| 状態 | agent の振る舞い |
|---|---|
| 通常(制御面と疎通) | フル収束 |
| **制御面と分断** | **加算のみ**(作る・直す・起動する)。**破壊的操作(削除・停止)は保留** |
| **lease 失効** | Observe のみに縮退(自律収束を停止) |

キャッシュに必要なのは2フィールドだけ:**`revision`**(制御面が単調増加させる版。復帰時に「古い理想で
動いていたか」を検出)と **`leaseUntil`**(この時刻まで自律動作を許可。無期限の暴走と、意図的に切り離した
agent のゾンビ収束を防ぐ)。DB は不要。

**規律**:「確信が持てないときは、加算はしても減算はしない」——観測が unknown なら消えたと断じない、
理想が stale なら消せを実行しない。**破壊的操作だけが制御面への到達を要求する。**

### 12.4 静的シャーディング = claim-as-intent(スケールアウト段2・v1b)

**決定(スケールアウト経路の第一級化・段2)**:単一デーモンの天井(§12.2)を超える最初の分割手段を、
**新しい可用性部品(ルーター)も新しい S2 も増やさない**静的シャーディングとして定義する。段3(動的
ルーター/自動リバランス)は v2 のまま残す。

**方式:シャード割当そのものを git の intent として宣言する**。

- 各デーモンは **`sakutei --claim <scope-selector>`**(§1.3)で起動し、**全 intent を pull した上で、
  自分の claim にマッチする scope 分だけを収束**する(claim に含まれない scope は Observe すらしない)。
- **どのデーモンがどの scope を担当するか= git 上の宣言的 intent**とする(例:`shards/tokyo.yaml` に
  `claim: {site: tokyo}`)。これにより **シャード割当が PR レビュー・履歴・監査の対象**になる
  ——「割当は運用の隠れた設定」ではなく「収束すべき第一級の intent」になる。
- **二重 claim(2 デーモンが同一 scope を掴む)の検知は既存の fencing token(§12.3)**で行う。分断や
  設定ミスで割当が重なっても、共有基盤 provider の Apply が古い世代の writer を fence する(§12.2)。
- **ルーターという新しい可用性部品を増やさない**:動的ルーター(§12.2 の「上位=ルーター」)は割当表
  (S2)とその可用性が新たな中心依存になるが、claim-as-intent は **割当を git(既存の intent 経路)に
  載せる**ため、新 S2 は **fencing 世代番号のみ**。git が落ちてもローカルミラー(§19.4)で claim は読める。

**再シャード(scope の移動)は Workflow**:あるデーモンから別デーモンへ scope を移すのは、
**drain → claim 変更 PR → fencing 世代更新** の順序付き手続きであり、最小 Workflow(§9.2・v1.5)で表現
できる。

```
1. 旧シャードで対象 scope を drain(新規 Apply を止め、in-flight を完了させる)
2. claim 変更 PR をマージ(旧の claim から外し、新の claim に足す)= 割当 intent の更新
3. fencing 世代を更新(旧シャードの当該 scope への Apply を以後 fence)
4. 新シャードが次周(または §6.8 の Watch)で当該 scope を収束開始
```

**この方式が観測 state レスと整合する理由**:シャードは shared-nothing の独立収束器であり、割当という
「どの現実を誰が見るか」の情報を **git(intent)に外出し**した。§12.2 の「共有物は衛星へ外出し済み」の
一貫適用であり、シャード間で状態を交換しない(§12.2 の格上げ原則)。

**スライス**:`--claim` の**型は v1a で切る**(起動フラグと scope-selector のパース。実装は必要時)。
静的シャーディングの**実動作(claim フィルタ + fencing + 再シャード Workflow)は v1b**。動的ルーター・
自動リバランス・`RemoteExecutor`・lease 縮退は **v2**(§24.2)。

**正直な限界(スケールアウトが放棄するもの)**:

- **シャード間の強い順序保証は放棄する**(結果整合)。シャードをまたぐ依存は「観測 ready 待ち」
  (§12.2)であり、シャード内のような plan 順序 gating は効かない。
- **全域不変条件(総容量・グローバル quota 等)はシャード内 gate + 外部集約でしか扱えない**。1シャードの
  invariant(§9.2)は自シャード内でしか強制できず、全シャード合算の制約は Sakutei 単体では強制できない
  ——上位プラットフォーム(§12.1)や外部集約に委ねる。rate limit・累積破壊予算も同様にシャード単位で閉じ、
  全域予算は外部集約で見る(§26.1 A-1)。
- **次の天井は allocator・Vault の QPS**:シャードは shared-nothing だが、採番(allocator)と秘密(Vault)
  は共有衛星であり、シャード数を増やすと**衛星への QPS が次の律速**になる。これは早期収束パス(§6.8)の
  「allocator の可用性が UX に直結する」論点と合流する——共有衛星のスループットが、分散収束と早期収束の
  両方に効く共通の次の壁である。**この天井は §16.7(衛星の停止は変化を止めるが維持を止めない)で原則として
  塞ぐ**——定常収束は衛星コールゼロで回り、QPS は変更レートに比例する(フリート規模ではない)。

### 12.5 claim の git 配布と S2-free シャード(多拠点スケール詰め・v1b)

**決定(第3ラウンド共通課題3 への対処。14 指摘)**:§12.4 の claim-as-intent は「全 intent を pull した上で
自分の claim 分だけ収束する」と定義したが、**300 拠点規模では (1) 全 intent の pull トラフィックと
(2) 店舗ローカル S2 の耐久性**(段0-minimal は quorum なし・§19.8)が新たな詰め所になる。両者を配布規約と
新しいシャード階級で塞ぐ。

**(1) リポジトリレイアウト規約 + sparse checkout**:claim でシャーディングする場合、intent リポジトリは
**`intent/<scope>/...` のパス接頭辞規約**に従う(**claim ↔ パスの対応を機械可能にする**)。

```
intent/
  platform/…              # 全シャードが read-only 参照する共有 platform(§12.1)
  site-a/…                # claim: {site: site-a} のシャードが所有
  site-b/…
  …
```

- 各シャードは**全 intent を pull しない**。**sparse checkout / partial clone** で「**自 claim 分 +
  共有 platform 参照分**」のみを取得する。claim ↔ パスが機械対応するため、`--claim {site: site-a}` から
  fetch すべきパス(`intent/site-a/` + `intent/platform/`)が直接引ける。
- これは §12.4 の「全 intent を pull」を**否定せず精緻化する**——正しさは「自 claim にマッチする scope だけを
  収束する」ことで担保され、pull 範囲を絞るのは**トラフィックの最適化**である(sparse で取り逃した他 scope は
  そもそも自分の収束対象ではない)。

**(2) ミラー中継層**:多拠点は中央 git を直接叩かず、**読み取り専用 git ミラー**(状態を持たない中継。
中央 2 拠点程度)から **jitter 付きポーリング**で pull する。

- **webhook(早期収束ヒント・§6.8/§13.5)は NAT 越え不可の拠点ではポーリングが代替**する。level ループが
  正しさを保証する(§6.8)ため、**ヒント欠落は遅延のみ**で正しさに影響しない——WAN 断・NAT 環境の拠点でも
  定期ポーリングで収束は回り続ける。
- **算術**:300 拠点 × sparse fetch/60s = 5 fetch/s(自 claim 分のみの小さな fetch)。**ミラー 1 対で余裕**で
  捌ける(共有 platform 分はキャッシュヒットが効く)。中央 git 本体には拠点が直接触れないため、中央の負荷は
  ミラー数(2)に固定される。

**(3) S2-free シャード階級の新設**:エッジ / 店舗の claim は **Resource 収束のみ**を担い、**ローカル S2 を
持たない**階級を第一級で定義する。

- **デーモンホストが全損しても失うものがゼロ**——S2(進行 state・§6.5)を持たないので、**再イメージ → pull →
  収束で完全復旧**する。店舗ローカルディスクの耐久性(段0-minimal は quorum なし)に賭けなくてよい。
- **Task/Workflow が必要な操作**(S2 を要する手続き・§9)は、S2-free シャードでは次の 2 パターンで表現する:
  - **(a) 中央 claim 側で実行する**:手続きを中央のプロビジョニング claim(§下記ハンドオフ)側に置き、
    店舗シャードは収束済みの結果を受け取るだけにする。
  - **(b) 完了マーカーを現実側に置く**:一回性の完了印を**現実側**に置き Observe で問う(§20 の
    `schema_migrations` と同型・§9.1 単純型)。進行 state を外部永続化せず、現実が state を持つ。
- S2-free シャードは **Resource(宣言的・収束)しか持たない**という制約と引き換えに、**耐久性要件をゼロに
  する**——「何も覚えないから、何も失わない」。§19.8 の段0-minimal(単一ノード・quorum なし)と最も相性が
  よい階級である。

**(4) 開店/閉店 = claim 変更 PR による所有権ハンドオフ**:§12.4 の再シャード Workflow の具体適用として、
店舗のライフサイクルを claim 変更で表現する。

```
開店:
  1. 新店舗の scope を中央の「プロビジョニング claim」下で構築(中央が S2 付きで Task/Workflow を実行)
  2. 安定後、claim 変更 PR で当該 scope を店舗デーモン(S2-free)へ引き渡す
     = 中央 claim から外し、店舗 claim に足す(§12.4 の drain→claim 変更 PR→fencing 世代更新)
閉店:
  上記を逆順(店舗 claim から外し中央へ戻す → デコミッション)
```

- **プロビジョニング(手続き的・S2 が要る)と定常収束(Resource のみ・S2-free)を claim 境界で分ける**のが
  要点。開店直後の重い立ち上げ手続きは中央が担い、安定後の維持だけを店舗が S2-free で担う。
- 所有権ハンドオフが **PR + fencing 世代更新**(§12.4)で表現されるため、開店/閉店が**監査・レビューの対象**に
  なり、二重所有(旧所有者が生きている)は fencing で検知される。

**スライス**:リポジトリレイアウト規約・sparse checkout・ミラー中継・S2-free シャード階級・claim ハンドオフは
すべて **v1b**(静的シャーディングの実動作と同じ段・§24.1b)。`--claim` の型と scope-selector パースは v1a で
切り済み(§12.4)。

---

## 13. 理想状態の供給源と設定形式

### 13.1 DesiredSource(選択制)

```go
type DesiredSource interface {
    Fetch(ctx) ([]Resource, intentRevision, error)
}

// 任意 capability(実装は任意。Provider に対する Lister と同じ宣言制)。§6.8・§13.5
type Watcher interface {
    Watch(ctx) (<-chan ChangeHint, error)   // 早期収束のヒントを流す。真実は運ばない
}

type ChangeHint struct {
    IntentRevision string   // どの intent 版か(合流時に最新を読むための目印)
    Scopes         []Scope  // 触られた scope 群(これ "だけ" を運ぶ。値・状態は運ばない)
}
```

| 実装 | 保存 | 履歴 / 監査 | レビュー | 書込頻度 | 用途(ownership) |
|---|---|---|---|---|---|
| **FileSource(git)** | ファイル | git commit 連鎖 | PR | 低(人) | **platform 層** |
| **DBSource** | DB | append-only ログ | 無(即時) | 高(API) | **tenant 層**・払い出し |
| **RenderedSource** | 外部 CMDB + intent | 元ソース依存 | render → PR | 中 | inventory 合成 |

「ファイルか DB か」は**保存形式の選択ではなく、必要な "正の性質"(履歴・レビュー・書込頻度)の選択**
であり、それは `ownership` 層と一致する。

**ファイルには2役あった**:(1) 保存、(2) **git がくれる正の性質**(履歴・PR レビュー・`git diff`・
単一書き込み経路)。DB にすると (1) は自明に代替できるが、(2) は**別途組む**必要がある
(原因タグ A・レビューゲート・監査・爆発半径の土台が (2) に依存しているため)。
→ DB 版は **append-only 変更ログ**(監査)と **revision**(commit hash の代わりにトランザクション ID)を
自前で持つ。

**混在の境界**:同一リソースを2つの供給源が持たない(**ownership が供給源の所属も決める**)。engine から
見れば全部マージされた1つの理想状態で、収束ループは供給源を区別しない。

**RenderedSource の規律**:render の生成物は**手編集禁止**(`generated/` 配下に置き、冒頭に自動生成
ヘッダを付す。生成物への直接コミットは弾く)。人が触るのは intent ファイルと外部 CMDB だけであり、
これにより「生成物と intent の二重管理」が構造的に起こらない。

### 13.2 理想状態の保持(platform 層 / ファイル正)

正はファイル。起動時に読んでインメモリ保持し、通常運転は全部メモリ上で回す。API が書く場合は
インメモリ更新 + ファイルへ write-through。**DB プロセスゼロ、単一バイナリを維持**でき、`git diff` で
理想状態の変遷が読める。並行書き込みやクエリ性が実問題になった時点で embedded KV(bbolt / SQLite)へ
`DesiredStore` interface 経由で移行する。

**etcd(分散合意)は要らない**——etcd は「多書き手の合意」のためであって「永続化」のためではない。

### 13.3 設定形式(Loader)

**v1 は YAML / JSON 両対応**。同じ木を吐く別シリアライザとして `Loader` interface の下に置く
(JSON = API の機械経路、YAML = 人の手編集)。round-trip 保持は YAML のみ配慮する。

- **スキーマ検証は JSON Schema を外付け**し、ロード時に検証(`pruneable` / `scope` / enum の必須化、
  循環検出)。YAML の型曖昧性(Norway 問題等)はスキーマで封じる(「version は文字列必須」等)。
- 参照(暗黙依存)が主役になったら **CUE** へ Loader を差し替える余地を残す(CUE は検証 + 参照 + 導出を
  言語内に持つ)。**Starlark は round-trip 不可**で API write と衝突するため採らない。
- **キー命名規約**:設定ファイル上のキーは **snake_case** を正とし、Loader が Go フィールド名
  (`DependsOn` 等)へ変換する。`depends_on` / `dependsOn` の混在は Loader が吸収する(§6.1・M-2)。

### 13.4 intent UI(catalog)

**catalog(intent UI)は v2**(§24.2)。intent は既に YAML(git)で表現済みなので、**UI に独自のデータ
モデル / DB を持たせない**。UI は薄いフロントエンド。

- **書き込み経路は `ownership` に従う**:テナントのカタログ操作(「仮想ネットワーク追加」)→ 即時反映。
  管理者の基盤設定変更 → **UI → PR 起票 → 承認**。同じ UI でも触るリソースの層で裏の経路が変わる。
- 難所は UI ではなく **round-trip**(UI とテキスト手編集の2経路調停・コメント/整形保持)。
  platform 層を **UI → PR 生成**にすることで、**git を単一の書き込み経路**に保ち往復問題を回避する。
- **カタログ UI の各商品 = 合成リソースのテンプレート**(§7.5)。UI → 合成リソース → allocation →
  git/収束 が一本に繋がり、**UI が薄くできるのは下の仕組みが厚いから**。

### 13.5 Watch(早期収束のヒント供給・任意 capability)

`DesiredSource` の任意 capability として `Watch`(§13.1)を置き、**早期収束パス(§6.8)の発火源**にする。
`Provider` に対する `Lister`(prune の前提)と同じく**宣言制**であり、実装しない供給源では定期周期だけが
動く(正しさは不変・遅延のみ)。

| 供給源 | `Watch` の実体 | バージョン |
|---|---|---|
| **FileSource(git)** | git webhook / merge フック(PR マージ = ヒント発火) | **v1a** |
| **DBSource** | DB の書込通知(tenant セルフサービス API の即時反映) | v2(DBSource 自体が v2・§13.1) |
| RenderedSource | 元ソースの変更通知(実装可能なら) | 元ソース依存 |

- **`Watch` が流すのは `ChangeHint`(intent_revision + 触られた scope 群)だけ**。変更後の値・状態は
  運ばない(§6.8 の「真実を運ばない」原則)。コアはヒントを受けて **その scope を新鮮に render → Observe →
  Diff → Apply** する(観測 state レス無傷)。
- **合流(coalesce)と最新読み**:同一 scope への複数ヒントは1回の scoped run に合流し、実行時に最新の
  desired を読む(§6.8)。これにより「古いヒントで古い状態を適用する」経路を型レベルで塞ぐ。
- **`Watch` seam は v1a で切る**(FileSource の webhook/merge フック)。DBSource 連動は v2。

---

## 14. シークレット

### 14.1 大前提:値を「収束」しない

理想状態(git)に値は書けない → 収束エンジンは「あるべき値」を知らない/知るべきでない → **値の diff は
取れない**。よって**収束が扱うのは値ではなく参照・存在・鮮度だけ**。値は Vault の責務。
(DB のデータを収束外にしたのと同じ切り分け。)

### 14.2 参照解決は遅延展開(just-in-time)

**早すぎる展開は禁**。読み込み時に全展開して理想状態に実値を埋めると、ログ・Event・ダンプに漏れる。
**展開が早いほど漏洩面が広がる。**

- 理想状態は最後まで**参照のまま**保持し、実値化は **Apply 直前・provider に渡す瞬間だけ**。使用後に
  メモリから消す。diff は**参照同士で取る**(値を知らなくても diff できる)。
- **`SecretRef` 専用型**(文字列にしない):`String()` が `***` を返し**自動マスク**、実値取得は明示
  `.Resolve(ctx)` 経由のみ。「うっかり平文化」を型で防ぐ。
- **世代フィールド**:ユーザが書く参照には現れない。**render フェーズ1が Vault metadata から解決して
  `SecretRef` に注入する**(ピン留めではない)。値を隠したまま rotate を検知でき、git への書き戻しループが
  生じない。

```
理想状態(ピン無し): password: ${secret:vault:/db/prod#password}
   ↓ render フェーズ1 が metadata から現世代を読む(値は読まない)
世代が下流リソースの入力ハッシュに合成される → rotate すれば次周 render で diff が出る
```

これが無いと「Vault で rotate したのに使う側が古いまま = 接続断」になる。

### 14.3 秘密の3分類(出自による)

「値を収束しない」原則は共通だが、**秘密がどこから来るか**で扱いが分かれる。

| 分類 | 生成方法 | 対象例 | 人手 |
|---|---|---|---|
| **既存・手動登録** | 段0 admin が現地作業後、Vault に1回だけ登録 | 機器の初期パスワード、既存 API キー | 初回のみ要 |
| **新規・自動生成** | render フェーズ1 で Vault(または dynamic secret)が生成 | サービスの root password、cluster join token | 不要 |
| **rotation** | Workflow が定期・自動実行(§14.4) | 上記いずれも、鮮度ポリシーに従う | 不要 |

**設計上の含意**:「既存・手動登録」は初回の1点だけ人手が要るが、**その後は自動生成・rotation の系に
合流させる**のが正しい形。初期資格情報のまま運用し続けない(bootstrap Task が初回接触時に自動で
新しい値を生成・適用し、以後は rotation Workflow に載せる)。これは §14.5 / §15.2 の「層B(静的資格 + 定期
rotate)」の具体的な運用パターンである。

### 14.4 供給:収束できるもの / Task なもの

- **存在・鮮度は収束**:「このパスに存在すべき」を Observe(**存在だけ・値は見ない**)/ Apply(無ければ
  Vault に生成させる。値は見ずに委譲)。鮮度(`rotation: every 90d`)も収束対象。
- **rotation の実行は Task/Workflow**:「新値生成 → DB に設定 → アプリ設定更新 → 旧を無効化」の順序付き・
  協調が要る手続き(順序ミスで接続断)。「**いつ(90 日超)= 収束が検知、どう(回す手順)= Workflow**」。

### 14.5 SecretRef と CredentialProvider の境界

- **`SecretRef`** = リソースに**注入する設定値**(アプリに渡す DB パスワード、機器に設定する SNMP
  community)。**desired の一部**。
- **`CredentialProvider`** = **engine 自身がターゲットに到達するための資格**(スイッチにログインする
  短命アカウント、vCenter API トークン)。**desired の一部ではなく実行時の資格**。credential broker が発行。

→ **provider が機器にログインする資格は CredentialProvider 経路**。機器に設定する値は SecretRef 経路。

**v1 における暫定**:credential broker は v2 以降のため、v1(v1a/v1b)では provider のターゲット認証
(DNS API トークン、vCenter 資格等)を **`SecretRef`(Vault 保存の静的資格)** で供給する。これは
層B(静的資格 + 定期 rotate)相当の暫定であり、broker 導入時に `CredentialProvider` 経路へ移行する。
- **注入点を明示する(整合性レビュー M-4)**:到達資格の SecretRef は **desired 本体(理想状態)には
  載せない**。`CredentialProvider` = 実行時の資格という分離(本節冒頭)を v1 でも崩さないため、**Executor が
  Apply/Read 実行時に `.Resolve()` する**(§14.2 の「Apply 直前のみ実値化」と同じ規律)。desired に載るのは
  「どの Vault パスの資格を使うか」という参照だけで、実値は実行時にのみメモリに出て使用後に消える。
- 設計変更は不要で、既存の層A/層B の枠に収まる。broker(v2)への移行時も注入点は Executor 実行時のまま。

---

## 15. credential-broker

BMC / NW 装置 / ハイパーバイザー等、多数のターゲットへの認証を統一的に扱う**独立ソフト**。

### 15.1 なぜ core から分離するか

1. **ライフサイクルが違う**:core = 常時収束ループ、broker = 要求応答型。
2. **可用性が逆立ちしている(決定的)**:broker は core の**依存先(下位)**。**core が死んでも認証が
   生きていないと、緊急時に人が機器に入れない**。core 内に認証を置くと致命的な循環依存。
   さらに **broker 断は Vault 断より重い**——Vault 断は「シークレット依存の Apply が止まる」だけだが、
   **broker 断は認証が要る全ターゲットの Observe が止まる = システム全盲**。
3. **セキュリティ境界**:認証は最大の攻撃対象。広い攻撃面(provider 群 / Ansible 連携)を持つ core と
   同居させず、小さく監査可能な別プロセスに隔離する。

### 15.2 核心原則:静的資格情報を配るな、動的に発行しろ

共通アカウント使い回しの根本解決は、**長命な資格情報を保存・配布しないこと**。使う瞬間に短命資格を
発行し、使用後に失効させる(Vault 動的シークレット / JIT)。**保存しないものは漏れない。rotation が
不要になる。発行ログがそのまま監査になる。**

現実には2層:
- **層A(動的発行対応)**:SSH 署名証明書 / PKI / 一部 API。短命資格を JIT 発行。**保存ゼロ**。
- **層B(レガシー機器=静的のみ)**:古い BMC / NW 機器。Vault 保存(SecretRef)+ 定期 rotate で緩和。
- 統一 interface の裏で自動振り分け。

```go
type CredentialProvider interface {
    Acquire(ctx, target Target, identity Identity) (Credential, error)
    Release(ctx, Credential) error
}
```

### 15.3 identity は2入口、機器側アカウントと分離

- **マシン**:収束デーモンが Vault に自分を証明(段0のトークン注入が起点。AppRole / TPM 等で強化)。
- **ヒューマン**:IdP(SSO)で個人認証 → **その身元で短命資格を発行**。**bastion 経由**でアクセス経路を
  1本に絞り、セッションを記録。

**機器には個人アカウントを永続作成しない**(全機器に全員のアカウントを作るのは管理地獄)。機器側は
使い捨て、身元は IdP/Vault が保持し、**発行ログが「本当は誰が」を記録する**。
→ **機器側は共通/使い捨てでも、監査は個人単位**になる。

### 15.4 段階性(人 → デーモン)

「最終的にデーモン中心・人は緊急のみ、当面は段階的」は、**remediation mode の移行に内包**される:

- **段階1**:人が主。デーモンは Observe + drift 検知(`alert-only`)。人が短命資格で入って直す。
- **段階3**:デーモンが全 apply(`auto`)。人は緊急のみ。
- **仕組みは不変で、`CredentialProvider` の発行先分布が人 → デーモンへ移るだけ。**
  まず作るべきは**人向け短命発行(bastion + JIT)**——段階1で実利があり、個人監査の要求が即満たされる。

### 15.5 broker 断の緩和(Vault と対称)

- **(a) 発行済み短命資格を core 側に TTL 内キャッシュ**(broker 断中も TTL 満了まで Observe 継続)。
- **(b) broker 不達中は該当リソースを `unknown` 扱い**(absent と断じない)。
- **(c) broker は段0の VM-HA 保護対象**(Vault・収束デーモンと同列)。

### 15.6 Break Glass(真の緊急時)

通常認証(Vault / IdP / 短命発行)が**全滅した状況**でも機器に入れる最終手段。ここには矛盾がある——
Break Glass は認証基盤に依存できないため、**我々が消した「静的な共通資格情報」を意図的に1つ残す
必要悪**であり、ゼロにはできない。

**封じ込め原則(「使えるが、こっそりは使えない」)**:
- **オフライン物理保管**(Vault にもデーモンにも置かない)。**段0 の unseal key と同じ金庫・同じ性質 =
  最後の人間の鍵**。
- **使用が必ず検知される**(封筒の改竄シール / 取り出しアラート)。
- **使い捨て・使用後 rotate**。
- 最重要のものは**分割管理**(Shamir。Vault unseal と同型。単独犯行/単独ミスを防ぐ)。
- **事前チェック最小・事後監査最大**(緊急時は速度優先、抑止は確実な発覚で担保)。

**階層**:

| レベル | 何が死んだ | 手段 |
|---|---|---|
| L1 | reconciler のみ | 通常の bastion + 短命発行(Vault/IdP 生存 = Break Glass ではない) |
| L2 | Vault | オフライン保管の機器資格(rotate 済み最新を金庫に退避) |
| **L2.5** | **管理NW の到達性**(§19.3) | **OOB / コンソールサーバ(シリアル)経由**。ネットワークが死んでも機器に入れる = **ネットワーク版 Break Glass** |
| L3 | 段0 全滅 | 完全オフライン Break Glass + **段0 再構築**(Ansible playbook + unseal key) |

**L2.5 の注意**:管理NW が死ぬと **credential-broker も機器に到達できない**(`reconciler → credential-broker → 管理NW →
機器` の経路が切れる)= §15.5 の「broker 断 = 全盲」と同じ状態になる。したがって管理NW障害時は
§15.5 の緩和(TTL 内キャッシュ / `unknown` 扱い / alert-only 縮退)がそのまま効く。OOB は
**この状態から人が復旧するための唯一の経路**であり、Sakutei の管理対象外に置く。

**L3 は段0のブートストラップと同一**(段0を人手で再建する能力が、そのまま最終 Break Glass)。

**システム化**:
- **検出** = 取り出し/開封イベントを既存の **Event → sink** に `BreakGlassUsed`(最高 severity)で流す。
- **rotate** = **自動 rotate は危険**(緊急対応中に資格が変わると対応者を締め出す)。**事態収束後に人が
  明示キック**する。中身は既存の rotation Workflow で、トリガが「手動」なだけ。
- **分担**:保管はオフライン(システム外)、更新・rotate はシステム。

### 15.7 エッジの資格局所性(WAN 断中の自律収束と爆発半径の両立)

**決定(第3ラウンド 14 の残存ギャップ G3「資格常駐の破壊力 vs 自己修復」への応答)**:エッジ / 店舗の claim
(§12.5)が **WAN 断中も自律収束を回す**には、その店舗の機器へ到達する **write 資格が店舗ローカルに常駐**する
必要がある。だが資格を常駐させると漏洩時の破壊力が問題になる——このトレードオフを**局所化で決着させる**。

- **店舗ローカル file SecretBackend に、その店舗の機器分の資格だけを常駐させる**(§3.4 の `file` 実装・
  §19.8 段0-minimal と同じ SecretBackend seam)。**中央の全店舗資格を配らない**——ある店舗のデーモンが持つのは
  当該店舗の機器資格のみで、**爆発半径は当該店舗に閉じる**(1 店舗の資格漏洩は 1 店舗にしか及ばない)。
- **WAN 断中も自律収束が回る**:店舗デーモンは局所資格で自店舗の機器へ Apply/Observe でき、中央 Vault・
  中央 broker への到達を要しない(§12.5 の S2-free シャードと組み合わせ、店舗は「pull した intent + 局所資格」
  だけで完結する)。
- **再接続時に中央からローテーション収束**:WAN 復帰時、中央が rotation Workflow(§14.4)で店舗ローカル資格を
  収束(rotate)する。常駐資格の鮮度は「常駐しているから古いまま」ではなく、**中央からの定期 rotate 収束で
  維持**される。
- **トレードオフの決着**:「資格常駐の破壊力(漏洩面)」vs「自己修復(WAN 断耐性)」は、**資格を店舗単位に
  局所化する**ことで両立する——常駐はするが**当該店舗にしか及ばない**局所常駐であり、鮮度は中央 rotate が
  維持する。これは §16.7 の「維持は衛星ゼロで回る」を資格面に適用した形である。broker(v2)導入後は、この局所
  file SecretBackend が層B(静的資格 + 定期 rotate・§15.2)相当の暫定であり、broker の JIT 発行へ移行できる。

---

## 16. allocator

**名称の三者関係(整合性レビュー M-6)**:**コンポーネント名 = `allocator`**、その **interface =
`AllocationAuthority`**、担う **役割 = 採番権威(allocation authority)**。本章で「authority」と呼ぶのは
役割を指し、実装コンポーネントは `allocator`、コード上の型は `AllocationAuthority` である。**v1a では実装
しない**(§7・§24.1b。allocator と render の採番連携は v1b から)。

### 16.1 払い出しは第3のカテゴリ

VLAN ID / IP / ASN の**プールからの払い出し**は、inventory(事実の記録)でも intent(構成の意図)でも
ない。**副作用を伴う書き込み**であり、しかも**素朴だと冪等でない**(毎周同じ intent を見て毎回新しい ID を
払い出し、プールを食い潰す)。

**解決:要求(冪等)と割り当ての事実(state)に割る**

- **要求**(intent・冪等):ユーザは「VLAN が1つ必要」と書く。具体 ID は指定しない。
  ```yaml
  resource: virtual_network
  id: app-net
  vlan: { from_pool: tenant-acme-vlans }   # プールから1つ要求(ID 指定なし)
  ```
- **割り当ての事実**(state):`app-net → VLAN 573` を**外部 authority が永続化**する。次サイクルで
  「app-net は既に 573」と分かり、**二重払い出しを防ぐ = 冪等性が回復する**。
  → **authority が「前回何を払い出したか」の記憶装置**になり、収束エンジン自身は state を持たずに済む。

### 16.2 authority の3役

| 役割 | 読み書き |
|---|---|
| **inventory**(既にある事実) | 主に read |
| **allocation authority**(プールと払い出し記録) | **read-write** |
| **reality**(収束結果の逆流) | 収束エンジンが唯一の書き手 |

**authority を採番の単一権威にする**:払い出しは必ず authority の API 経由とし、収束エンジンは自前
プール管理を持たず「1つくれ」と頼む。→ **並行/シャード環境でも競合しない**。
**分散合意を core に戻さず、採番調停だけ外部に外出しできる。**

### 16.3 仮予約(reserve → confirm)

platform 層(PR 必須)で PR 生成時に本払い出しすると、**却下時にリークする**。

```
PR 生成時   : TTL 付き reserve(仮予約)→ PR diff に具体 ID が出る(何が使われるか人が見える)
マージ時    : confirm(本払い出しに昇格)。★confirm はマージ後 最初の render フェーズ1が実行
却下/TTL切れ: 自動的に解放(リークしない)
```

テナント層(即時)は仮予約不要で直接払い出す。

### 16.4 Release は非対称原則に従う

素朴な「所有者が desired から消えたら返却」は**危険**:render 入力が stale(CMDB 断・理想の一時的な
読み損ね)→ owner 消滅と誤認 → Release → **使用中の VLAN/IP が別リソースに再払い出し = 衝突**
(ネットワークでは最悪級の事故)。

- **(a) desired からの消滅では Release を発火させない。下流リソースの削除完了を Observe で確認して
  から**実行する(「理想から消えた」ではなく「**現実から消えた**」を根拠にする)。
- **(b) 返却後に cooldown / 検疫 TTL**(即時再払い出しをしない。DHCP のリース再利用猶予と同型)。
- **(c) `pruneable:false` 等で下位が残存する間は Release 保留**。

**「現実から消えた」を何で確認するか(決定・整合性レビュー H-6 への対処)**:v2.2 は「削除完了を Observe
で確認」と書いたが、`Observation` の三値(ok/unknown/converging)には **absent(消えた)を積極的に表す値が
無く**、`Read` が `unknown` を返す状態と「本当に消えた」を区別できなかった。個別 `Read` に第4値を足すと
全 provider に波及するため、**Release の削除完了確認は `Read` ではなく `List`(集合演算)を根拠にする**。
- **削除完了 = 対象 ID が scope の List 結果に現れなくなったこと**(§8.1 の「消すべき=現実にある集合 −
  理想にある集合」と同じ集合演算)。これは §8.6 の List 完全性検証(件数急減 halt・ページング完走)を
  そのまま前提にでき、`unknown` との混同を避けられる。
- したがって **Release は Lister を持つ provider の下流にのみ許可**する(§8.4 の削除判定3種と接続:
  離散・被参照 = 参照カウント0、容量プール = 残余充足、単純専有 = List 集合差)。List できない下流の
  Release は保守側(保留)に倒す。この規律により §16.4 が §4.2 / §8.1 / §8.6 と型レベルで接続する。

### 16.5 interface(実装非依存)

```go
type AllocationAuthority interface {
    Allocate(ctx, pool, owner string) (value string, err error)                  // 冪等:同 owner → 同 value
    Reserve(ctx, pool, owner string, ttl time.Duration) (value string, err error) // PR 用の仮予約
    Confirm(ctx, pool, owner string) error                                        // マージ後 render が昇格
    Release(ctx, pool, owner string) error   // ★下流削除を Observe 確認後のみ。cooldown あり
    Lookup(ctx, pool, owner string) (value string, ok bool, err error)
}
```

NetBox / Infoblox / phpIPAM / 自作を実装として差し替え可能。**独立 CMDB 全体は不要**——inventory は
当面ファイル、reality 逆流は将来、残るのは**採番だけ**。**採番は state を持つので自作の価値があり**
(intent は git が正だから UI も供給源も薄くてよい=**state を持つ部分と持たない部分で自作の意味が違う**)、
最小実装(SQLite + アトミック払い出し + フラットプール)で足りる。

**守る2規律**:(1) interface の裏に隠して差し替え可能に保つ。(2) スコープを**採番に限定**し、CMDB 全体に
育てない。

**allocator HA 最小形(決定・第3ラウンド共通課題1 への対処)**:v1b の自作 SQLite 実装は **段0 の VM-HA 上に
置き、active/standby + fencing token(§12.3 を再利用)** で保護する。allocator は payoff-out された共有 state
の権威(§16.1)であり、その喪失は最も痛い(§16.6・§26.1 A-4)ため、**可用性目標は reconciler 以上**とする。

- **fencing の再利用**:active/standby フェイルオーバーで旧 active が生きていると 2 権威が同一プールから独立
  払い出しし復帰時に採番衝突(§16.4 が最悪級と呼ぶ事故)になる。→ 割当記録に **fencing token / 世代番号**を
  持たせ、standby 昇格時に世代を上げ、古い active の Confirm/Release を fence する(§12.2 の単一 writer fencing
  と同型)。
- **quorum ストアへの相乗り可**:割当記録(最も喪失が痛い S2・§26.1 A-4)は、**S2 の永続化先(quorum ストア
  or 同期レプリカ + fencing・§26.1 A-4)と同じ置き場に相乗りしてよい**。allocator 専用の別ストアを立てる必要は
  なく、S2 の置き場が決まればそこに載る。
- ただし **§16.7 の原則により、allocator HA は「変化(新規採番)の可用性」のためであって「維持(定常収束)の
  可用性」のためではない**——定常収束は allocator が落ちていても Lookup キャッシュで回る(§16.7)。HA は
  新規払い出しの UX(§6.8 の早期収束)を守るための投資であり、システム全体の生存条件ではない。

**プール分割 by claim(決定・第3ラウンド共通課題1 への対処)**:サイト / シャード固有のプールは、**intent で
レンジを disjoint に分割宣言する**(§16.6 の「1 プール 1 権威」をシャード軸に一般化)。

```yaml
# 各サイトのプールレンジを intent で disjoint に宣言(claim と対応)
pools:
  - { name: site-a-vlans, range: "100-199", claim: {site: site-a} }
  - { name: site-b-vlans, range: "200-299", claim: {site: site-b} }
```

- サイト固有プールを disjoint に切ると、**シャードは自分のレンジだけを自 allocator(またはローカルインスタンス)で
  管理でき**、中央 allocator への依存は「**全シャード共有プール**」(サイトをまたぐ ASN 等)だけに縮む。
- **実行時のレンジ委譲プロトコルは作らない**——分割は **intent の宣言で足りる**(claim-as-intent・§12.4 と同じ
  思想:動的な調停機構でなく、静的な宣言で分割境界を固定する)。分断中も各シャードは自分のレンジ内で独立に
  払い出せる(§16.6 のサイト静的分割の一般形)。

### 16.6 allocator の state 喪失と分断(決定・安全性レビュー S-07/S-08 への対処)

authority は state を持つ(§16.1)ゆえに **喪失と分断の設計が要る**。v2.2 は cooldown(§16.4b)しか
持たず、割当記録の喪失・地理分散時の二重権威に無防備だった。

- **state 喪失時の再構築(逆引き)**:割当記録(SQLite・§16.5)が失われたとき、**現実から逆引きして
  再構築する手順**を持つ。`owner → value` は現実側(機器に設定された VLAN/IP)を Observe / List すれば
  多くが復元でき、復元できない分は cooldown 中の未使用として保守側に倒す。喪失=即再払い出し衝突を防ぐ。
- **1 プールにつき 1 権威**:地理分散で 2 権威が同一プールから独立払い出しすると復帰時に採番衝突(§16.4
  が最悪級と呼ぶ事故)になる。→ **プールをサイトごとに静的分割**(サイト A = VLAN 1-500、B = 501-1000)し、
  **そもそも 2 権威が同一プールに触れない**ようにする(shared-nothing を採番プールにも適用)。分断中も
  各サイトは自分のプール範囲内で独立に払い出せる。

### 16.7 衛星の停止は、変化を止めるが、維持を止めない(設計保証)

**決定(第3ラウンド共通課題1 への対処。04/06/07/11/20 が共通指摘した「allocator/Vault の QPS・可用性が
次の天井」への原則的応答)**:早期収束(採番が UX 直結・§6.8)と claim 分割(全シャードが共有衛星に依存・
§12.4/§12.5)の両方が allocator/Vault の QPS・可用性に合流する。これを部品追加(HA・キャッシュ)だけでなく
**原則**で塞ぐ。

> **衛星の停止は、変化を止めるが、維持を止めない。**
> 定常状態の収束ループは、衛星コールゼロで回る——これを設計保証にする。

**なぜ成立するか(観測 state レスとの整合)**:

- **割当事実は Release まで不変**:allocator が `app-net → VLAN 573` を確定させたら、その事実は Release
  (§16.4)まで変わらない。したがって **`Lookup`(§16.5)の結果はシャードローカルに無期限キャッシュしてよい**
  ——無効化は**自分の Release のときだけ**(他者が自分の割当を変えることはない・§16.6 の 1 プール 1 権威)。
- **観測 state レス原則との整合**:これは観測 state レス(§2-1)に反しない。割当事実は「**観測**」ではなく
  「**自分が確定させた事実**」だからである。現実を再観測して得る値(=毎周見に行くべき S1)ではなく、自分が
  払い出しを依頼して得た確定値である。喪失しても allocator に `Lookup` し直すだけで再取得できるので、これは
  **S2(喪失=事故)ではなく再取得可能なキャッシュ = S3 扱い**である(§6.5)。喪失時の縮退は「一度 allocator に
  問い直す」だけで、事故にならない。
- **secret も同型**:secret は既存の last-resolved 保持 + TTL キャッシュ(§14.2/§7.4)を維持する。Vault 断中も
  キャッシュ済み世代で Apply でき(§7.3)、Observe は資格キャッシュ(§15.5)で継続する。

**QPS 算術の第一級化(「allocator がスケール天井」という誤解を数値で潰す)**:allocator への QPS は
**フリート規模ではなく変更レートに比例する**。

- 例:**10,000 リソース・日次 churn 1% = 新規採番 ~100 件/日 ≒ 無負荷**(定常の 9,900 リソースは Lookup
  キャッシュヒットで allocator に触れない)。フリートが 10 万に増えても、churn が同じなら QPS は 10 倍で済み、
  「リソース数に比例して allocator が詰まる」は起きない。
- **早期収束のバースト = ポータル申請レート**(§6.8):早期収束で新規採番が要るのは「人がポータルで申請した
  瞬間」だけであり、その頻度は人の操作レート(申請/秒)で上限される。定常収束はバーストを生まない。
- したがって allocator の sizing は「フリート規模」ではなく「**変更レート(churn)+ ポータル申請ピーク**」で
  行う。これが §12.5 の「共有衛星が次の天井」を実務上大きく緩める根拠である。

### 16.8 衛星停止時の縮退(waiting-allocation)

**決定**:allocator が停止しても**定常収束は止まらない**(§16.7)。止まるのは**新規採番が必要なリソースだけ**で
ある。これを専用の縮退状態として定義する。

- **allocator 停止 → 新規採番が必要なリソースだけが `waiting-allocation` 状態**になる。これは `converging`
  (§10.4)に類似した「一時的に進めない」状態であり、**エラーでも halt でもなく、下流 skip のみ**(§6.3 の失敗
  伝播と同じ機構)。既存リソースの収束は Lookup キャッシュ(§16.7)で**継続**する。
- **Release は元々非対称・遅延可**(§16.4):allocator 停止中に Release を発火できなくても、Release は
  「下流削除を Observe 確認してから・cooldown 付き」で元々遅延前提なので、停止中は単に保留され安全側に倒れる
  (加算はしても減算はしない・§2-3)。
- **早期収束パスの縮退**:allocator 停止中、新規払い出しが要る早期収束ヒント(§6.8)は**キューに保持し、
  定期周期へ縮退**する(§6.8 の「溢れたヒントは次の定期周期へ」と同じ規律)。allocator 復帰後の周期で自然に
  払い出され、`waiting-allocation` が解ける。UX としては「申請が即時反映されず、allocator 復帰まで遅延する」
  として現れる(§6.8 の注意点の具体化)——正しさは損なわれない。

---

## 17. 観測・イベント・監査

### 17.1 3つの「チェック」を混同しない

| 種類 | 問い | どこで | 収束アクション |
|---|---|---|---|
| **conformance** | 宣言どおり在るか | 収束ループ(既存) | あり(Diff を埋める) |
| **health** | 在るものが動いているか | **監視システムに委譲** | **なし**(報告のみ) |
| **合成検証(canary)** | その操作が今も動くか | 時間トリガ Workflow | 能動的に実行して確かめる |

**在る ≠ 動いている。** VM が「存在し電源 ON」でも中のアプリは応答しないかもしれない。health は
「差分ゼロ」なので収束では捕まらない。

### 17.2 health = 「監視設定」を Resource として収束する

prober loop を自作しない。**observability as Resource**:

```yaml
vm:
  id: web-1
  observability:
    - blackbox: { probe: http, path: /healthz, port: 8080 }
    - alert: { name: web-down, for: 2m, severity: page }
```

これを通常の Resource として収束させ、**Prometheus の scrape target / blackbox / Alertmanager ルールを
生成・維持**する。判定と通知は Prometheus に委譲。**リソース削除時は監視設定も prune される**ので、
孤児 monitor が残らない。

### 17.3 合成検証(canary)

「VM を作成・削除できるか」は監視ではなく**能動的検証**。受動監視では絶対に得られない情報(特に
**削除経路の健全性**——「作成は動くが削除が権限不足で失敗」は現場で頻発する)。

```yaml
synthetic_check:
  name: vm-lifecycle-canary
  every: 1h
  scope: { datastore: test-only, network: canary-vlan }   # ★隔離必須
  steps:
    - create: { from: canary-template, ttl: 10m }          # TTL = 消し忘れ防止
    - assert: { powered_on: true, reachable: true }
    - delete: {}
    - assert: { gone: true }
  on_fail: { alert: page }
```

**時間トリガで起動する Workflow**。隔離 scope + TTL 自動 GC + cleanup 反復失敗で自動停止が必須。

### 17.4 Event(発生源と sink の分離)

収束失敗 / Workflow 失敗 / drift / canary 失敗 / 振動を**個別に通知実装しない**。単一の `Event` 型に
一本化し、通知は Event の consumer(sink)として分離する。**本システムはイベントの発生源であり、
ルーティング・エスカレーション・沈黙は Alertmanager 等に委譲する。**

- **収束失敗は level(継続状態)として報告**する。level-triggered だからこそ通知も firing/resolved に
  でき、失敗継続 = 更新、回復 = resolved。**毎周同じアラートを撒く破綻を回避**できる。
- **Since / 連続失敗回数から transient ↔ stuck を自動昇格**(level-triggered が過去を毎周持っている
  ことの無料の副産物。edge 型監視には出せない情報)。
- 分類:**transient**(自己回復見込み)/ **stuck**(権限・欠損・容量 = 即 critical)/ **oscillating**
  (綱引き)/ **blocked**(依存未達 = 上流 Event に集約)。
- **Workflow / canary 失敗は edge(事件)**:停止位置・到達済み step・invariant 状態を1回通知。
  canary の cleanup 反復失敗のみ level に昇格。

### 17.5 監査

Event を永続化(S4)し、**append-only + 連鎖ハッシュ**で改竄耐性を持たせる。

- **git が理想変更履歴の半分を担う**(コミット連鎖がそのまま改竄検知)。**Apply 実行記録**は追記専用ストアへ。
- **外部アンカーが必須**:連鎖ハッシュ単体では先頭(head)を外部固定しないと全体書き換えで改竄可能。
  日次で head hash を Event として sink に流し、git repo に記録する。**git push 先が事実上の改竄耐性の
  実体**であり、理想状態と同じ外部依存に相乗りできる(追加インフラゼロ)。
  - **アンカー間隔を短縮し即時 push する(決定・安全性レビュー S-11)**:日次アンカーは「最後のアンカーから
    今日まで」の最大 24h 分を検知されずに改竄できる窓を残す。→ **監査を外部 SIEM / WORM / append-only
    専用サービスへ即時(毎周 or 毎時)push** し、head アンカー間隔を短縮する。append-only の enforcement を
    「アプリの規律」でなく**ストレージ層(WORM / 外部即時 push)**で担保する(侵害されたホストが自分の
    犯行ログを書き換えられないように)。監査書き込みは reconciler と別権限・別プロセス(broker と同じ思想)。
  - **S4 の永続化先を正式決定する(決定・v2.6・§26.2 から格上げ)**:v2.5 まで S4 の置き場は §26.2(機構の
    置き場)に留まっていたが、**S4 は WORM ストレージ or 外部即時 push を正式な永続化先とする**(上記 S-11 の
    決定を §26 から本文の正式決定へ引き上げる)。**選択肢は排他でなく、どちらか一方(または併用)で
    「侵害ホストが自分の犯行ログを改竄できない」を満たせば足りる**——WORM(write-once ストレージ)なら
    書込み後の改竄が物理的に不可能、外部即時 push なら侵害ホストの外に記録が出た後の改竄が不可能。
    これで S2(§26.1 A-4)に続き S4 の永続化先も本文で確定し、状態クラス S1〜S4 の置き場がすべて決まった。
- **帰属**:収束は自律動作なので「誰が」が問題になるが、**変更の原因タグ(A/B/C/D/E)がそのまま監査の
  帰属になる**(自己修復か / 誰の commit 由来か / どの承認ゲートを誰が通したか)。原因区別の設計が
  監査で再利用される。
- **秘密の監査は SecretBackend 側の機能を使う**(自前実装しない):Vault KV v2 は更新のたびに
  **version history** を自動保持し、**audit log** は全 read/write を誰(AppRole/user)が行ったかを記録する。
  → **git(理想が誰にどう変更されたか)+ SecretBackend(秘密が何回更新され誰に読まれたか)の併用**で
  監査が揃う。Event(S4)はこの2つを繋ぐ収束側の記録であり、三者で完結する。

---

## 18. バックアップ / DR

「バックアップ構成が在る ≠ バックアップが取れている」。4つに割れ、**全て既存プリミティブに割り当たる**。

| 側面 | プリミティブ | 同型の既存設計 |
|---|---|---|
| バックアップ**構成**(ジョブ・保持ポリシー) | **Resource**(収束) | 通常リソース |
| バックアップ**結果**(取れたか・鮮度・復元可能性) | **health / canary**(収束外) | §17 |
| **リストア** | **Task**(一回性) | migration |
| **DR フェイルオーバー** | **Workflow**(invariant 付き) | rolling upgrade |

**最大の落とし穴**:構成が `converged` でも、毎晩失敗して1ヶ月バックアップが無い、が起こりうる。
**構成の収束は結果の健全性を保証しない。**

- **鮮度 = 時間ベースの health 述語**(証明書 expiry と同型):`backup_freshness: { max_age: 26h }`。
- **復元可能性 = canary**:存在し新しくても壊れて復元不能かもしれない(運用最大の恐怖)。
  **「取れているか」ではなく「戻せるか」を、隔離環境への定期リストアで証明する**。
- **3-2-1 等のポリシー = invariant**。
- **DR の特殊な難所**:被災時には**収束エンジン自身が死んでいる**(プライマリに居たなら)。フェイル
  オーバーを指示する主体が不在 → DR 自動化は**制御面のマルチサイト**が前提。**自動化は最後発**で、
  手動 runbook(人がセカンダリ側で Workflow 起動)で長く足りる。

### 18.1 災害時運用経路(DR モード)——被災サイトの承認・署名経路への依存を断つ

**決定(v2.6・06 指摘「DR 中の PR 承認・署名経路の東京依存」への応答)**:マルチサイト DR では、被災サイト
(例:東京)が持っていた**通常の書込経路(git の正・PR 承認・署名鍵)そのものが被災で到達不能**になりうる。
すると生存サイト(例:大阪)が引き継ごうとしても、「PR を承認する経路」「commit に署名する鍵」が東京依存の
ままだと、**平時のガバナンスがそのまま DR の障壁になる**——収束の仕組みは生きているのに、正への書込みができない。
これを2つの経路で断つ。

**(a) DR intent のプリステージ(平時にレビュー済みの構成を保持)**:大阪が東京 scope を引き継ぐための構成
(claim 変更を含む)を、**平時にレビュー・承認済みの状態でリポジトリに保持**する。発動時は新規レビューを要さず、
**パラメータ / claim の切替のみ**で有効化する。

```yaml
# 平時に PR 承認済みで保持。発動は enabled: true への切替(または claim 切替)のみ
dr_plan:
  name: tokyo-to-osaka
  primary: { site: tokyo }
  standby: { site: osaka }
  on_activation:
    claim_change: { from: {site: tokyo}, to: {site: osaka} }  # 大阪が東京 scope を claim(§12.4)
    enabled: false        # ★平時は false。発動 = true(この1点の切替が DR 発動)
```

- **DR 発動時に新規レビューが不要**なのが要点。承認は「平時に、東京が健在なうちに」済ませておく——被災後に
  「東京の承認者に PR レビューを頼む」必要がなくなる。claim 変更(§12.4/§12.5 の所有権ハンドオフ)がプリステージ
  済みなので、大阪デーモンは切替後の周期で東京 scope を収束開始する。
- プリステージ構成は generated ではなく人が書く intent であり、通常の PR フローで平時にレビューされる
  (§12.1 の platform 書込ポリシーに従う)。発動フラグの切替だけを緊急時に行う。

**(b) 緊急ローカル書込経路(break-glass 型・git の正へ到達不能時)**:git の正(中央リポジトリ)へ到達不能な
状況では、**被災を免れたサイトのローカルミラー(§19.4)への署名付きコミット**を許可する break-glass 型の書込経路を
第一級化する。これは §15.6 の Break Glass 原則(「使えるが、こっそりは使えない」)の**書込版**である。

- **位置づけ**:§15.6 は「機器へ入る(認証)」の Break Glass、これは「正へ書く(書込)」の Break Glass。
  既存の Break Glass 原則(§18 相当の封じ込め)をそのまま適用する:**オフライン相当の専用鍵 / 使用が必ず検出される
  / 使用後 rotate**。
- **使用は必ず検出される**:ローカルミラーへの break-glass コミットは `BreakGlassWriteUsed`(最高 severity)を
  Event → sink(§17.4)に流し、S4 監査(§17.5)に append する。「こっそり書く」ができない。
- **正への事後合流(reconciliation)手順とセット**:break-glass コミットは**必ず**「中央の正が復旧したら事後合流
  する手順」と一体で第一級化する。ローカルミラーの署名付きコミットを、正の復旧後に **PR として起票し事後レビュー**
  する(緊急時は書けるが、監査は後から必ず通る)。これにより「DR 中に緊急で書いた変更」が正の履歴に正規に取り込まれ、
  分岐したまま放置されない。
- **ローテーション**:break-glass 書込鍵は使用後 rotate する(§15.6 の使い捨て原則)。

**貫く原則**:DR 経路も「事前チェック最小・事後監査最大」(§15.6)——被災時は速度を優先して生存サイトが書けるように
し、抑止は**確実な発覚(Event + S4)+ 事後合流の強制**で担保する。平時のガバナンス(PR 承認・署名)を DR で
捨てるのではなく、**(a) で平時に前倒し、(b) で緊急書込を検出可能な break-glass に閉じ込める**の二段構えで、
「被災サイトへの承認・署名依存」を構造的に断つ。

**スライス**:DR intent のプリステージ(a)は claim ハンドオフ(§12.4/§12.5・v1b)と DR Workflow(§9.2・v1.5)の
組み合わせで表現でき、新プリミティブ不要。緊急ローカル書込経路(b)は Break Glass(§15.6)と Event/S4(§17.5)の
再利用であり、**書込 break-glass の型と事後合流 PR 起票は v1.5**(最小 Workflow と同じ段)。

---

## 19. 段0(ブートストラップと自己運用)

### 19.1 ブートストラップは人手でよい(定石)

このシステムはインフラを作るが、**自分自身(動作ホスト・自分の設定・資格情報)は自分では収束できない**。
一般解は存在せず、**自己を立ち上げる何かは自分の外側から来るしかない**(k8s の kubeadm / Cluster API の
management cluster / Terraform の state バックエンドと同じ構造)。妥協ではなく定石。

**目標は段0の最小化**——段0を「作って忘れる」不変の最小セットに保つこと。

### 19.2 段0の構成(確定)

```
人間(パソコン + Ansible + 金庫)
  ① 管理NW到達性の最小核  ② Proxmox 3ノード構築  ③ 統合 Ceph
  ④ VM 定義(Vault / [credential-broker=v2] / reconciler)
  ⑤ Vault unseal(★コールドスタートは Vault 先行・§19.7-1)
  ▼
段0(Proxmox 3ノード + 統合 Ceph + VM-HA)
  ├─ 管理NW到達性の最小核        … 段0 収容ポート + 最小 L2/L3(§19.3)。Sakutei は触らない
  ├─ Vault(VM-HA 保護)         … シークレットの根
  ├─ [credential-broker(VM-HA 保護)]   … 機器認証。reconciler の依存先。★v2 で追加(下記)
  └─ reconciler(VM-HA 保護)       … git から pull、ローカルミラーで全断耐性
  OOB:コンソールサーバ / OOB専用NW … 管理NW 全断時の唯一の経路(Break Glass L2.5)
  外部依存:GitHub/GitLab(理想状態リポジトリ)= 更新取得元。稼働必須依存にしない
  ▼ 自律収束
段1以降:本番インフラ全体
```

**broker のバージョン帰属(整合性レビュー H-4 の解消)**:credential-broker は **v2 で追加**される
(§14.5・§24.2)。**v1(v1a/v1b)の段0 に broker は建てない**——v1 は `reconciler → SecretRef(Vault)→
機器` で到達し、broker への実行時依存は存在しない。したがって §3.2 / §3.3 / §15 が broker を「core の
依存先・落ちると全盲」と描くのは **v2 以降の姿**であり、**上図の broker 行と ④ は v2 で有効化される**
(v1 では角括弧の要素を除いて読む)。§15.4 の「段階1(人向け短命発行 bastion+JIT)」も v2 で最初に作る
broker 機能であり、v1 には属さない。段0-minimal(§19.8)では Vault も broker も無い。

**役割分担が2ツールの性質差と一致する**:

- **段0 = Ansible**(open-world・pull 型・稀・意図的)。段0は不変基盤であり、**drift 自動修復はむしろ
  危険**(最上位ゆえ直す上位がいない)。人が意図したときだけ動く Ansible が正しい。
- **段1以降 = 本システム**(自律収束・常時ループ)。

### 19.3 管理ネットワーク(段0 / 段1 の境界)

BMC・スイッチ管理ポートへ到達するための**管理NW**は、段0の定義に直結する。判定基準は段0の一般則の
特殊化である:

> **それを壊したとき、Sakutei が自分でそれを直せなくなるか。**
> (= single point of blindness を持つか)

これは §11.2-5「管理プレーンのロックアウト」の**データセンター規模版**である。個別機器の config push で
自分を締め出す問題を、DC 全体のスケールでやってしまう。

**切り分け**:

| 要素 | 段 | 理由 |
|---|---|---|
| reconciler / credential-broker ホストの管理NW接続(NIC / IP / VLAN) | **段0** | これが無いと何にも到達できない |
| 管理NWコアの**到達性の最小核**(段0 収容ポート + 最小 L2/L3) | **段0** | 落とすと全管理対象が全盲 |
| 管理NWコアの**残りの config**(ACL / 追加 VLAN / 監視設定) | 段1 | 到達性を壊さない範囲 |
| 管理NWのアクセススイッチ(末端) | 段1 | 一部が落ちても全体は生きる |
| 各サーバの BMC 設定(IP / 認証) | 段1 | 1台死んでも他は生きる |
| 各機器の管理ポート設定 | 段1 | 自分自身への到達経路でなければ |

**要点は「同じ機器の中で段が分かれる」こと**。管理NWコアスイッチは、**到達性の最小核だけを段0
(Ansible が構築し、Sakutei は触らない)**とし、それ以外の config は段1(Sakutei が収束)とする。
これは §11.2-4 の **managed fields** と同じ発想——**同じ対象の中で、触ってよいフィールドと触っては
いけないフィールドを分ける**。段0を最小に保つ原則(§19.1)にも従う。

**自傷を3層で防ぐ**:

1. **`tier: foundation`**(§6.1):管理NW機器はその先の全機器の上流であり、依存の根の典型例。
2. **commit-confirm**(§11.2-5):到達性を切っても自動ロールバックする。
   **管理NW機器では `Capabilities.CommitConfirm` を必須とする**。
3. **platform 層 + PR 必須 + rate limit 厳格**(§12.1):管理NWコアの変更は全機器を全盲にしうる。

**v1 では管理NWコアを収束(Apply)対象にしない(決定・実装可能性レビュー M-4)**:commit-confirm は
Apply 契約の transactional/candidate-commit であり **v2 の NW 機器 provider で入る**(v1a/v1b には無い)。
commit-confirm 未実装のまま管理NWコアを Apply すると config push で自分を締め出す経路が残る。→ v1 では
**管理NWコアを `alert-only` 限定(観測のみ)**とし、NW 機器 provider は **`Capabilities.CommitConfirm` が
false の間は「Apply 不可・観測のみ」を Capabilities で強制**して、ロックアウト経路を型で塞ぐ。到達性の
最小核はそもそも段0(Ansible)で Sakutei は触らない(上表)。

**OOB(帯域外)= ネットワーク版 Break Glass**:管理NW そのものが死んだときの経路として、
**コンソールサーバ(シリアル集約)/ OOB 専用NW** を持つ。これは Sakutei の管理対象外に置き、
Break Glass の **L2.5**(§15.6)として扱う。

**管理系とデータ系は別系統である**:上記は**管理系**(BMC アクセス・reconciler から全機器への到達性)
の話であり、実運用では**データ系**(VM トラフィック・分散ストレージのレプリケーション・live migration)
がもう一系統存在する。両者は帯域も経路も異なる。

| 系統 | 目的 | 帯域 | 段の判定 |
|---|---|---|---|
| 管理系 | BMC / reconciler から全機器への到達性 | 低速で足りる | **到達性の最小核は段0**(上表) |
| データ系 | VM トラフィック / ストレージ IO / migration | 高速 | **全て段1**(下記) |

**データ系が段0でない理由**は、同じ判定基準の適用結果である——**データ系が全断しても reconciler は
管理系経由で機器を制御でき、自分で直せる**。したがって single point of blindness にならない。

**制御経路と実行対象の分離**:データ系スイッチ**自体の設定変更**も、**管理系経由で行う**。
データプレーンのスイッチが、自身の制御プレーンとして高速リンクを使う必要はない。これは §15.6 の
Break Glass 思想(制御経路を実行対象から独立させる)と同型であり、「データ系を壊しても、データ系を
直す経路は生きている」を保証する。

**依存順序への反映**:データ系の疎通確認は、その上に載るサービス(クラスタ join・ストレージ OSD 等)の
`ready_condition`(§6.7)として前置する。データ系が未疎通のままクラスタを組むと、
§11.2-1「収束器の上の収束器」問題を悪化させる。

### 19.4 設計判断

- **git は外部**(GitHub/GitLab)。段0から git 本体が消える。各ノードにローカルミラーを持ち、外部 git
  不達時は最後に pull した状態で収束を継続する(**稼働必須依存にしない**)。
- **Vault と broker は段0内**(置かざるを得ない)。有状態サービスが複数になるため段0が
  "小プラットフォーム化" し、**Ceph + VM-HA が正当化される**(Proxmox の統合 Ceph により運用重量が
  下がる)。収束デーモンは観測 state レス + level-triggered なので **VM 再起動の空白を許容**でき、
  アプリレベル HA は任意の上積みでよい。
- **unseal は Ansible(オペレータ起動)**。unseal key は playbook の外(Ansible Vault 暗号化 + 人の金庫)。
  **「シークレットの根を守るものの根」は人間の金庫に着地する**——これがブートストラップの無限後退の
  正しい終わり方。
- **可用性の正直な但し書き**:VM-HA でも Vault は再起動で sealed に戻り、unseal はオペレータ起動。
  → **Vault 載せノード障害の MTTR は人間の unseal 時間を含む**(Raft HA でも sealed-on-restart は
  解消しない)。緩和は §14・§15.5 のキャッシュと alert-only 縮退。

### 19.5 爆発半径(自己運用の最重要項目)

**「1コミットで全 VM 削除」が原理的に可能**なため、§10 の安全機構は自己運用上の生命線である。特に
**rate limit + halt** と **remediation mode** は v1 に必須(誤記1つで本番を壊さないための装置は、事故る
前に付ける)。

### 19.6 人手運用として残るもの

Proxmox 3ノードのハード保守、**管理NW到達性の最小核と OOB 経路の維持**(§19.3)、Ceph 健全性監視、
unseal key の金庫管理、たまのバージョンアップ(Ansible)。
段0 として現実的な負担だが、**「年数回〜月1回」は楽観的で、頻度はハード信頼性と Vault の運用形態に
依存する**(実装可能性レビュー H-6)——Vault の sealed-on-restart は「ノード障害・保守再起動のたび」に
unseal を要求し(§19.4・§19.7)、3ノード Ceph は最小構成で冗長性が薄い。導入時に「段0 は片手間では
回らない」ことを正直に提示する(H-4/brownfield で得た信頼を段0 の運用破綻で失わないため)。

### 19.7 段0 の SPOF・コールドスタート・自己収束自傷(決定・安全性レビュー S-10 への対処)

v2.2 の段0 は VM-HA でノード障害には備えるが、以下3点が未設計だった。いずれも段0 の生命線なので確定する。

**(1) コールドスタート順序(Vault unseal 先行)**:reconciler は broker に、broker は Vault に、Vault は
unseal(人手)に依存する。VM-HA が3つを同時起動すると **reconciler が Vault 未 unseal のまま起動 → 全
ターゲット unknown で空回り**する。→ **起動シーケンスを `ready_condition`(§6.7)相当で明文化**する:
`Vault unseal 完了 → broker healthy → reconciler start`。reconciler は依存が unready の間は **Observe すら
試みず待機**する(空回りログの氾濫を防ぐ)。

**(2) 統合 Ceph の SPOF と緩和**:統合 Ceph は段0 の全 VM(Vault/broker/reconciler)のストレージであり、
**全損すると3つが同時に落ちる = 段0 内の単一障害点**。3ノード Ceph の 2ノード同時障害(電源・空調・
ラック単位)は quorum 喪失。全損時の RTO は Break Glass L3(§15.6・Ansible + unseal key)による人手再構築
(数時間〜日)であることを **リスクとして明記**する。緩和:**Ceph の障害ドメインを分散**する(3ノードを
別ラック / 別電源 / 別空調に配置)ことを §19.6 の人手運用要件に含める。段0 の SPOF が「共有 Ceph 1つ」に
集約されている事実を隠さない。

**(3) 段0 サービスの自己収束自傷**:reconciler は broker/Vault を管理対象(Resource)にでき、**誤った理想
変更で自分の依存先(broker 到達設定・Vault アドレス)を壊すと自分が全盲になり直せない**。管理NW で
「到達性の最小核は段0(Sakutei は触らない)」と切り分けた(§19.3)のと**同じ切り分けを段0 サービスにも
適用**する:**reconciler は自分の broker 到達資格・Vault アドレスを収束対象から除外**する(managed field 的
除外 + §11.4 の `external_owner` 相当の「触らない最小核」宣言)。commit-confirm はネットワーク専用なので
これらには効かないため、除外で塞ぐ。

**Vault 断の全盲時間の緩和(S-07)**:broker の短命資格 TTL キャッシュ(§15.5a)は「短命」ゆえ、人手
unseal の MTTR より先に切れ、**最も必要な "長時間 Vault 断" のときに全盲を防げない**。→
- **段0 に限り Vault auto-unseal(HSM / Transit / cloud KMS)を選択肢として再検討**する。「人間の金庫に
  着地」の哲学は unseal key の**最終**保管には残しつつ(auto-unseal 用鍵を Shamir 分割 + 物理保管)、
  日常の再起動 unseal を自動化して MTTR から人間時間を外す(哲学と両立可能)。
- または **Observe 専用の read-only 低権限資格を長め TTL で別枠キャッシュ**(破壊はできないが全盲は防ぐ)。

### 19.8 段0-minimal(1〜2 台構成向けの縮退形)

**決定(20 構成横断発見2 への対処。構成 01/02 が段0 と物理的に衝突して不成立だった)**:段0(3ノード +
Ceph + Vault-HA)は 1〜2 台構成には過大で、ホームラボ・スモールオフィス帯を閉ざしていた。**段0-minimal**
を第一級の縮退形として定義する。

| 項目 | 段0(標準) | 段0-minimal |
|---|---|---|
| ノード数 | Proxmox 3ノード + VM-HA | **単一ノード**(HA なし) |
| ストレージ | 統合 Ceph | ローカルディスク(Ceph なし) |
| SecretBackend | Vault(VM-HA 保護) | **file ベース SecretBackend**(Vault なし。§3.4 seam の `file` 実装) |
| credential | broker(v2)/ SecretRef | SecretRef 直結(broker なし) |
| 可用性 | VM-HA + Ceph 冗長 | **単一障害点を許容**(トレードオフを明示) |

- **可用性トレードオフの明示**:段0-minimal はノード障害で段0 全体が止まる。だが **収束デーモンは観測
  state レス + level-triggered なので、復帰後に最後の理想へ自然に再収束**でき、制御面が落ちてもデータ面
  (既存 VM/機器)は動き続ける(§19.1)。「段0 が止まる = 新規障害の自己修復が止まる」だけであり、
  1〜2 台構成では許容できるトレードオフ。
- **file ベース SecretBackend** は sealed-on-restart が無いのでコールドスタートが単純(§19.7-1 の順序
  制約が消える)。代わりにシークレットの保護強度は下がる——**ディスク暗号化 + アクセス制御で緩和**し、
  規模が育ったら Vault へ `SecretBackend` seam 経由で移行する(seam があるので後付け可能)。
- 段0-minimal は **v1a で成立する**(broker/allocator/Vault いずれも不要な構成に一致するため)。
  可視化・drift 検出器としての価値(§23.2)は 1〜2 台でも出る。

### 19.9 衛星停止影響表(何を止め、何を止めないか)

**決定(第3ラウンド共通課題1 への対処)**:§16.7 の原則「衛星の停止は変化を止めるが維持を止めない」を、
各衛星について「**何を止め、何を止めないか**」の表として明示する。衛星の可用性要件の非対称(どれが最上位か)が
この表から直接読める。

| 衛星 | 停止が **止めるもの** | 停止が **止めないもの** | 縮退の接続先 |
|---|---|---|---|
| **allocator** | **新規採番が要るリソースの作成のみ**(`waiting-allocation`・§16.8) | 既存リソースの収束(Lookup キャッシュ・§16.7)/ Release(元々遅延可) | §16.8 の waiting-allocation・早期収束ヒントのキュー保持 |
| **Vault** | **secret を持つ Apply のみ**(alert-only 縮退) | Observe は継続(資格キャッシュ・§15.5)/ secret を持たない Apply | §19.7 の Vault 断緩和・§14.2 の last-resolved + TTL キャッシュに接続 |
| **credential-broker(v2)** | **認証が要る全ターゲットの Observe = 全盲**(§15.5) | (broker は v2。v1 は SecretRef 直結で broker 依存なし・§14.5) | だから**可用性要件が最上位**(§15.1・既存記述と接続)。TTL 内キャッシュで一時緩和 |

- **非対称の要点**:allocator/Vault の停止は**部分的**(新規採番だけ / secret Apply だけ)に止まり、定常収束は
  回り続ける。一方 **broker(v2)の停止だけは全盲**(Observe が全滅)であり、これが「broker 断 > Vault 断」
  (§15.1)という可用性要件の順位の根拠である。
- **v1(v1a/v1b)では broker が無い**(§19.2)ため、v1 の衛星停止影響は allocator と Vault の 2 行だけであり、
  どちらも**定常収束を止めない**。すなわち **v1 では「衛星がすべて落ちても、既存インフラの維持収束は回り続ける」**
  ——これが §16.7 の設計保証の v1 における具体的帰結である。

---

## 20. 適用ドメイン

### 20.1 収束できるもの / できないもの(DB を例に)

| 対象 | 扱い |
|---|---|
| postgres の導入・起動・`postgresql.conf` / `pg_hba.conf` | ✅ **Resource**(冪等・観測可能) |
| database / role / 権限 / extension の存在 | ✅ **Resource**(SQL で観測 + 冪等適用) |
| **スキーマ(テーブル定義)** | ❌ 収束対象にしない |
| **マイグレーション実行** | ❌ **Task**(操作であって状態でない) |
| **行データ** | ❌ 対象外(宣言不能・観測不能) |

**なぜスキーマが収束と相性が悪いか**:
- マイグレーションは**経路依存の操作**。理想状態に `column: new_name` とだけ書くと、システムは
  「old_name が余分 → 削除、new_name が無い → 追加」と解釈しかねない——**rename が drop+add に化けて
  データ消失**する。
- 破壊的差分の判定が付かない(「理想に無いテーブル → 削除」= 本番データ)。

**"インフラとしての DB"(導入・設定・role・権限)は本命ユースケース**であり、ループ型なので drift
自動修復(手で消された role を戻す)が効く点でむしろ強い。

### 20.2 デプロイ

| 「デプロイ」の中身 | プリミティブ |
|---|---|
| VM 作成・複製・電源・スペック | **Resource**(managed fields + pruneable 必須) |
| VM の再起動を伴う変更(vCPU 増・ディスク拡張) | **Task** |
| ベアメタルに ESXi/Proxmox 導入 | **多相 Task + Workflow** |
| クラスタへのホスト join | **Workflow**(invariant = quorum 維持) |
| golden image ビルド | **Task**(完了印はレジストリを Observe) |
| 上記すべての前提整備・Day-2 維持 | **Resource** |

一貫して「**産む/焼く/join する瞬間 = 手続き、前提と産まれた後 = 収束**」。

### 20.3 イメージ

`image → VM` の依存は既存 DAG でそのまま表現でき、**依存 ready まで下流の払い出しが gate される**。

- **DL 登録** = **Resource**(収束)。**ready 判定は存在ではなくチェックサム/署名一致**(破損・改竄・
  途中失敗を ready と誤認して VM を産まないため)。Apply が長時間になるため §6.4 の非同期化が効く。
- **build** = **Task**(一回性)。完了印は成果物レジストリを Observe。
- **コンテンツアドレス参照**(`image@sha256:...`):**名前ではなくハッシュで参照する**。名前参照だと
  「同じ名前だから変わっていない」と誤認し、古い image の VM が残る。**コンテンツアドレスが drift 検知を
  正しくする**(型に食い込むため v1 で導入)。
- **GC**:参照カウント + `keep_last: N`。v1 は「消さない・手動」で逃げてよい。

### 20.4 その他のドメイン

| 項目 | 帰結 |
|---|---|
| PKI / 証明書 | SecretRef + expiry health + rotation Workflow。**証明書は DAG 上流**(切れると依存サービスが倒れる) |
| 電源 / BMC(IPMI/Redfish) | Resource + Workflow。**managed field 境界に注意**(意図的に off にしたホストを勝手に on にしない) |
| 物理 NW(VLAN/L3/LAG) | Resource。順序付きコレクション + commit-confirm |
| 基盤サービス(DNS/DHCP/PXE/NTP) | Resource + `tier: foundation`(依存の根) |
| ストレージ(snapshot / レプリケーション) | バックアップと同じ4分割 |
| キャパシティ管理 | **現在充足性 = 収束**(容量プール判定)。**予測 = 収束の外**(時系列外挿は level-triggered の構造に無い)→ 監視系へ export |

---

## 21. プロバイダ戦略

### 21.1 二層構成(網羅性の現実解)

**エンタープライズにおける価値の大半は provider の数と品質**にある。一方で全ベンダーを自前で書き切るのは
不可能。→ **自前コアと Ansible の二層**で埋める。

- **コア層(自前 provider)**:DNS / FW / VM 等、重要で **Diff 品質・横断項目対応**が要るもの。
- **ロングテール層(Ansible provider)**:雑多な機器・OS 設定。**品質を落として広くカバー**する。

### 21.2 Ansible は「モジュール」だけ借りる

**Ansible のモジュール(冪等な実行実体)だけを借り、実行モデル(playbook / 一回 push)は借りない。**
収束ループ(いつ・何を = 本システム)と実行実体(どう = Ansible モジュール)が噛み合う。
`Executor` interface の一実装として乗る。

```go
type AnsibleExecutor struct { ... }   // Executor の一実装
//   Observe → ansible module を check mode で叩く
//   Apply   → ansible module を通常実行(冪等適用)
```

### 21.3 Ansible provider では横断保証の多くが落ちる(期待値管理)

**check mode は「与えた desired に対する changed 真偽」しか返さず、current state を返さない**。つまり
Diff は Ansible 内部で完結しており、**コントローラ側の Diff・原因タグ A/B の区別・converging 検知・
破壊的変更の重み付けが軒並み機能しない**(`Capabilities.TrueObserve = false` による機械的縮退)。

実装の8割が泥臭い層(=ロングテール=Ansible 担当)であることを踏まえると、**本システムの保証群は当面
少数派のコア provider にしか完全には効かない**。これは設計の嘘ではないが、正直に認識すべき制約である。

**緩和**:`*_info` 系モジュールで Observe できる対象は `TrueObserve: true` に**部分昇格**でき、縮退は
全面ではない。また順序付きコレクション / managed fields / commit-confirm は**本システム側でラップ**して
扱う(Ansible は1操作の実行だけを担い、賢さは自前側に置く)。

### 21.4 時間発展

将来はコア実装を広げ、Ansible の担当範囲を狭めていく。ただし**ブロックストレージ製品のような、専用 API が
複雑でベンダー固有性が高い領域は自前化が割に合わず、Ansible provider が長く残る**可能性が高い。

---

## 22. 開発・検証環境(devenv / twin)

### 22.1 なぜ特殊か

本システムは**インフラを操作対象にする**ため、「操作される側」を用意しないと検証にならない。しかも
検証したいのは正常系より**危険な収束挙動**(prune / 爆発半径 / drift 自己修復 / converging 誤認)であり、
**わざと壊れる状況を安全に再現する**必要がある。実機は論外。

### 22.2 忠実度を3層で選ぶ

- **層A:フェイク Provider(開発の主戦場)**
  収束ロジック・DAG・prune・爆発半径・原因タグ・in-flight 排他の検証に**本物は要らない**。むしろ
  フェイクの方が良い:**障害注入が自由**(unknown を返す / N 回に1回失敗 / converging 継続)、高速(秒)、
  決定的(CI 可)。**`Provider` / `Executor` interface に挿すだけ**で、収束エンジン本体を実インフラ
  なしで完全にテストできる——**seam 設計の副産物**。**開発の8割はここで済む。**
- **層B:軽量な本物**:DNS(CoreDNS)/ コンテナ(Podman)/ FW(nftables)/ Linux 系。VM かコンテナ1台。
- **層C:重い本物のミニチュア**:**Proxmox ネスト**(ハイパーバイザー)、**containerlab**(cEOS /
  SR Linux / FRR)、MinIO(S3)/ targetcli(iSCSI)、Vault。

### 22.3 devenv(坪庭)— 検証データセンター

**devenv** は開発・検証環境の総称(層A〜層C を含む)。メモリの大きい物理1台に、ネストで段0〜段2を
丸ごと再現する(実機ゼロ)。町家の内側に囲い込まれた一坪の庭のように、**敷地(本番)の内側に隔離された、
本物と同じ材料でできた小さな庭**。**devenv 自体を Sakutei で構築する**(ドッグフーディング)。

### 22.4 ワークフロー(速い順)

1. **ユニット + フェイク Provider**(秒):毎コミット CI。主戦場。
2. **単一 Provider 統合**(分):provider 実装の正しさ。
3. **devenv フルスタック E2E**(時間):VM 払い出し → DNS 登録 → FW 設定。
4. **カオステスト(意図的破壊)**:provider をわざと落とす / drift 注入 / 大量削除トリガ →
   **爆発半径制御・alert-only 縮退・振動検知・unknown≠absent が効くか**を検証。
   **このシステムの肝は「壊れそうで止まる」ことなので、壊す検証が最重要。**

### 22.5 twin(縮景)— デジタルツイン(最終像)

**twin**(実在の景観を縮小再現する作庭技法)は devenv の最終像。**本番の intent を同じ reconciler で
ツインに流し込み、本番とツインを同一の宣言から構築する。** 検証環境が本番と構造的に一致する。
坪庭で育てた検証手法が、縮景(本番の縮小再現)に到達する。

**定義:ツインとは「render の外部入力を差し替えた環境」**である(render を第一級化したことで、
「入力の差し替え点」として自然に表現できる)。

- **ツイン専用の AllocationAuthority**(本番スナップショットから seed すれば具体 ID の忠実度も確保)
- **専用/フェイクの SecretBackend**(本番 Vault を叩かない)
- inventory もツイン用スナップショット
- → **本番プールを消費せず、本番シークレットも展開しない**

**用途**:変更の事前検証(爆発半径・カスケード波及 D の可視化)/ **provider 更新の検証**(幽霊 diff C を
本番前に観測)/ DR・ローリングのリハーサル / カオスの常設先。

---

## 23. Brownfield 導入(既存の大規模基盤に入れる)

Sakutei はここまで「宣言から作る」前提で設計されているが、現実の導入は逆——**現実が先にあり、宣言が
後から追いつく**。既存基盤への導入は greenfield と質的に異なる問題であり、専用の戦略が要る。

### 23.1 最大の地雷:不完全な理想 × prune

> **理想状態が不完全なまま closed-world prune を有効にすると、「宣言に書かれていない = 余分」と
> 判定され、本番の大半が削除対象になる。**

これは §8 の削除セマンティクスが**正しく機能した結果**であってバグではない。Sakutei は「理想が完全で
ある」ことを前提にするが、**導入初期の理想は必ず不完全**である。したがって:

**導入初期は prune を全面禁止する**(全 scope で `DeletionPolicy: ExplicitAbsentOnly`、
`pruneable: false`)。closed-world への昇格は §23.4 の基準を満たしてから。

### 23.2 段階0:読むだけから始める(alert-only 強制)

**最初は一切 Apply しない。** 全リソースを `remediation: alert-only`(§10.2)に固定する。

- 全 provider が Observe だけ動く。Apply 経路は無効。
- Sakutei は「現実がどうなっているか」を報告するだけ。

この段階でも Sakutei は**構成の可視化ツール**として機能し、それ自体に価値がある(既存基盤の実態は、
たいてい誰も正確に把握していない)。**実態を暴くことが最初の価値提供であり、信頼獲得の入口**になる。

### 23.3 import(reverse-render):現実から理想を生成する

数千リソースの理想を手で書き起こすのは非現実的。**Observe した現実をそのまま理想状態として書き出す**。

```
sakutei import --scope <範囲> --out intent/
  → Observe した現実を intent(YAML)として出力
  → 「今の現実 = 理想」の状態から始める(diff がゼロになる)
```

**Sakutei の import は Terraform の import より構造的に楽である**。観測 state を持たないため、import は
「state ファイルを作る」のではなく**単に現実を YAML に書き出すだけ**。state と現実がズレるという
Terraform import 特有の苦しみが**原理的に発生しない**。実装も `Lister`(§4.1)があれば足りる。

ただし2つの注意:

- **import した YAML は「理想」ではなく「現状のスナップショット」**である。既存基盤には設定ミス・
  レガシー・意図しない差異が含まれており、そのまま理想にすると**間違った現状を永久に維持する**ことに
  なる。→ **import 後に人が読み、意図を反映して整理する工程は避けられない**(ここは人力)。
- **managed fields(§11.2-4)の切り分けが import 時に必要**。DRS が動かす配置・動的 IP・runtime な値を
  理想に取り込むと永久に喧嘩する。import は「**何を intent とし、何を runtime とするか**」を分ける
  必要がある。→ **managed 候補の自動サジェスト**(決定・実装可能性レビュー M-3):import 出力に
  「N 周観測して毎周値が変わるフィールド」を **managed 候補**としてマークし、人の切り分けを「ゼロから」
  から「サジェストの承認/却下」に下げる。これにより §23.4 の昇格基準も「diff ゼロ」でなく
  「**意味のある diff がゼロ(managed 除外後)**」に緩められ、ノイズと本物の drift を分離できる
  (アラート疲労の回避)。この機構は Sync Mode の commit 間引き(§23.8)と共通化する。

### 23.4 scope 単位の段階的昇格

一気に全体を載せない。**scope(§8.2)を最小単位で切り、1つずつ昇格させる**。

```
段階0: 全体 alert-only + prune 禁止(観測と可視化のみ)
段階1: 1 scope を import → 人が理想を整理 → alert-only で観察
段階2: 昇格基準を満たしたら auto + closed-world(prune)を有効化 ※その scope だけ
段階3: 次の scope へ
```

**昇格基準**:

> **その scope で N 日間、diff がゼロ(または全て説明可能な diff)であること。**

diff がゼロにならないなら、それは「**理想が不完全**」か「**誰かが手で触っている**」かのどちらかであり、
**どちらも昇格してはいけないサイン**である。

**管理外の領域には一切触れない**(open-world のまま放置)。§8.1 の「closed-world は境界の宣言で作られる」が
そのまま導入戦略になる——**境界を宣言できた範囲だけが Sakutei の領土**。

### 23.5 人手運用との共存(最大の実務課題)

既存基盤には**人が手で触る運用が根付いている**。Sakutei を入れた瞬間に両者が衝突する:

- 誰かが緊急対応で設定を手で変える → Sakutei が drift として**元に戻す** → **人の対応を潰す**
- **これが一度でも起きれば、運用チームは Sakutei を信用しなくなる**(そしてツールは殺される)

**対策は alert-only で drift 検知器として先に信頼を得ること**:

- 「勝手に戻す」を導入初期に**絶対にしない**。人が手で変えたら、Sakutei は「理想と違う」と**報告するだけ**。
- 報告を見て人が判断する:「正しい変更 → 理想(git)を更新する」/「誤り → 戻す」。
- 信頼が積もってから `auto` に昇格する。**`alert-only` → `auto` の移行が、そのまま組織の信頼獲得の
  プロセスになる**(§15.4 の「人が主 → デーモンが主」と同じ構造)。

**これは技術ではなく組織の問題**である。Sakutei が正しく動いても、運用チームが受け入れなければ導入は
失敗する。

### 23.6 脱出口を最初から

導入初期こそ **Sakutei を止める手段**が要る。

- **グローバル freeze**(§10.6)を1コマンドで叩けるようにする。
- 「おかしい → まず止める → 調べる」が即座にできること。
- **Sakutei を止めても本番は動き続ける**(制御面が落ちてもデータ面は動く・§19)ことを運用チームが
  理解していること。

**「いつでも止められる」と分かっていることが、新しいツールの受容を助ける。**

### 23.7 導入順序(リスクとリターン)

1. **安全で価値が出る周辺から**:DNS レコード / 監視設定 / 証明書。壊れても影響が局所的で、drift 検知の
   価値が即座に出る。
2. **read-heavy な可視化**:全 VM / 全 FW ルールを Observe して実態レポートを出す(Apply しない)。
   **既存基盤の実態を暴くこと自体が信頼獲得になる**。
3. **新規構築部分から `auto`**:既存を触るより、**これから作るクラスタ / テナントを最初から Sakutei で
   作る**。greenfield 部分で実績を作り、brownfield は後回しにする。
4. **既存の中核は最後**(ハイパーバイザー / コア NW)。**永久に alert-only のままでも構わない**。

> **「既存全部を Sakutei 管理下に置く」を目標にしない。価値が出る範囲だけ、安全に。**

### 23.8 Sync Mode(scope 単位の継続的 import)

§23.3 の `import` は離散・手動の操作である。だが実際の移行では、Sakutei 管理へ切り替えるまでの
数ヶ月間、対象 scope は**引き続き正常な人手運用の対象であり続ける**。この期間の設定変更は緊急対応でも
逸脱でもなく、その基盤にとっての**正常な運用そのもの**であり、`import` を都度手動実行する運用は
現実的でない。**現実の変化を継続的に intent(git)へ記録し続ける仕組み**が要る。

**scope は排他な2つの mode を持つ**:

```go
type ScopeMode int
const (
    ConvergeMode ScopeMode = iota // 既定・従来通り。intent(git)→現実へ収束
    SyncMode                       // 現実→intent(git)へ片方向反映。Apply には一切接続しない
)
```

1つの scope は同時に両方の mode になれない。ロード時に検証し、矛盾する宣言は理想状態ロード時に拒否する
(§4.1 の循環検出・prune 検証と同じ入口・同じ思想)。**`DeletionPolicy`(§8.2)と同種の、scope に紐づく
ポリシー enum**であり、`Provider`/`Executor` のような実装差し替え可能な interface(§3.4 の seam)とは
性質が異なる。

```yaml
scope: { site: legacy-dc, cluster: legacy-cluster-a }
mode: sync
sync:
  target_path: generated/legacy-dc/cluster-a/   # ★ generated/ 配下(下記参照)
```

**動作**:

```
毎周(通常の収束サイクルとは別ループ):
  1. List(§4.1)で scope 内の現実を取得
  2. 前回 sync 時のスナップショットと比較
  3. 差分があれば intent ファイルへ書き出し + commit(コミット者はシステム)
  4. render → Diff → Apply には一切渡さない(意図的に非接続)
```

`Lister` があれば実装できる点、内部ロジックが `import` と共通する点は §23.3 と同じ——本機能は import を
「都度手動」から「scope に紐づく継続ループ」に変えるものであり、新しい Observe/List の実装は要らない。

**なぜ双方向にしないか**:「intent → 現実への収束」と「現実 → intent への反映」を同一 scope で同時に
有効化すると、人が書いた intent が次周の反映で即座に上書きされる、原因タグ A(§10.3)が現実のわずかな
揺れと区別できなくなる、という矛盾が生じる。**片方向のみとし Apply 経路には一切接続しない**ことで
構造的に回避する。これは §14.2 で「シークレット世代を git に書き戻さない」と決めたのと同じ思想
——**書き戻しループを作らない**。

**generated/ 規律を適用する(§13.1 の再利用)**:片方向にしても、**人が sync 対象パスを「普通の
intent」だと誤解して直接編集する**と、次周の反映でサイレントに上書きされる、という別の穴が残る。
これは §13.1 で RenderedSource の生成物に定めた規律——**`generated/` 配下に置き、冒頭に自動生成
ヘッダを付し、直接コミットを拒否する**——がそのまま対象とする問題であり、Sync Mode の出力にも
同じ規律を適用する。

**正当性判断・承認ゲートは不要**:§23.5 で扱った「人の変更を drift として検知し正当性を判断する」
運用とは前提が異なる。Sync Mode 対象 scope では、そもそも git が intent として参照・編集されておらず、
すべての現実の変化が定義上正当な運用である。原因タグ A/B(§10.3)もこの scope には適用されない(Apply
判断に使われないため無意味)。ただし **commit の attribution はシステムとして明確にする**(§17.5)
——`generated/` ヘッダと合わせ、人の intent commit と混同されないようにする。

**commit 粒度は衛生の問題**(正しさには影響しない):意味のない差分(順序の入れ替わり・カウンタ値等)
まで毎周 commit すると監査ログの可読性が下がる。差分検出は毎周でよいが commit は間引く、あるいは
意味のあるフィールドのみ normalize してから比較する。割り切って全周 commit してもよい(監査上の
正確性は損なわれず、可読性のみの問題)。

**List capability が要る**:Sync Mode は `import` / `prune` / `closed-world` と同じく **`Capabilities.List`
に束ねられる**(§5.4)。List できないプロバイダの scope は Sync Mode の対象にできない——これは
import が List を要求するのと同一の理由(scope 内の現実を網羅列挙できることが前提)。

**切替(cutover):Sync Mode → Converge Mode**

数ヶ月の同期期間を経て準備が整った時点で、離散的な1ステップとして切り替える。このタイミングのみ
人のレビューが要る——理由は正当性判断ではなく、**§23.3 で指摘した managed fields の切り分け**
(DRS が動かす配置・動的 IP 等の runtime 値を intent に取り込まない)が生スナップショットのままでは
できないため。

```
1. Sync Mode を停止(最新 commit を凍結スナップショットとする)
2. 人がレビュー:managed fields の切り分け・不要な履歴ノイズの整理(§23.3 と同一の作業)
3. scope の mode を converge へ切替、remediation: alert-only で開始
4. §23.4 の通常の段階的昇格プロセス(N日間 diff ゼロ → auto 化)へ合流
```

### 23.9 観測不能資産の「階級」(Observe すら不可の資産を第一級で扱う)

**決定(20 構成横断発見7 への対処。構成 14/18 で unknown 常態の資産が積み上がる)**:ブラウンフィールドには
**Observe すら不可能な資産**(API を持たない旧機器、ドキュメントにしか存在しない配線、ベンダーロックで
読めない箱)が必ずある。v2.2 はこれらを扱う型が無く、`unknown` の運用負荷が積み上がるだけだった。→
台帳上 **`unknown` のまま第一級で扱う資産階級**を型として定義する。

```yaml
resource: legacy_appliance
id: old-fw-01
observability: none          # Observe 手段が無い(Read も List も張れない)
ledger_only: true            # 台帳(intent)には存在するが、収束の対象外
last_verified: 2026-03-01     # 人が最後に目視確認した日(時間ベース health 述語と同型)
```

- **`observability: none` の資産は Read/Diff/Apply の対象にしない**(Observe できないものを収束しようと
  すると必ず事故る)。だが **台帳(intent)には第一級で存在させる**——「存在は知っているが観測できない」を
  `unknown` として明示的に持つのが、`unknown≠absent`(§2-3)の台帳版である。
- **prune の巻き添えを防ぐ**:closed-world scope の List に現れない `observability: none` 資産を「理想に無い=
  削除」と判定してはならない。ledger_only の資産は **List 集合演算から除外**する(そもそも現実を List
  できないのだから、List 差分で消す根拠が無い)。
- **時間ベースの人手検証**:`last_verified` が古くなったら「人が目視確認せよ」を health 述語(§17.2 の
  証明書 expiry と同型)として上げる。observe できない資産の唯一の「鮮度」は人の確認である。
- これは v1a で型として持つ(後から足すと台帳の意味論が変わる)。実際の運用フローは v1b(brownfield)。

---

## 24. スコープ

### 24.1 v1 の再スライス(v1a / v1b / v1.5)

**設計判断(v2.2 からの変更)**:v2.2 §24.1 の v1 は「後付け不可のコア型」と「実装量の大きい MVP 機能」を
同列に積み、実装可能性レビューで約 22.5 人月・MVP として過大と判定された。さらに 20 構成検討で 12 構成が
「事業の核」を Task/Workflow 系に依存させていた。したがって v1 を **v1a(コア実証)/ v1b(brownfield 導入)
/ v1.5(手続きの最小形の前倒し)** に割る。基準は「**後付けすると全プロバイダ・全 scope に波及するか**」
(→ v1a)と「**その機能が無いと使い始められない導入形態があるか**」(→ v1b / v1.5)。

#### 24.1a v1a — コア実証(≈10 人月。「触っていないのに戻る」+「壊れそうで止まる」を実証)

**コア interface / 型(後付け不可・最初に型へ)**
1. `Provider` interface(`Schema` / `Capabilities` / `Read` / `Apply`、正準は §5.1)と `Lister`(prune の前提)
2. `Observation`(Availability/State/Ready)と `ConvergenceStatus` の分離(dry-run の根拠)
3. `Capabilities`(機械的縮退)
4. `SecretRef` 型(自動マスク + 明示 Resolve + 世代フィールド)
5. `DeletionPolicy` / `pruneable`(既定 false)/ `scope + managed`
6. **原因タグ(A/B/C/D/E)** と `intent_revision` / `rendered_revision` の分離(**per-resource 帰属**・§10.3)
7. provider version を diff 計算に含める(埋め込みは登録必須契約・§10.3)
8. **コンテンツアドレス参照**(`image@sha256`)
9. `ownership`(platform / tenant)と `remediation mode`(+ `remediation schedule`・§10.8)
10. `Executor`(実行境界。v1 は `LocalExecutor` のみだが、**コントローラは Executor 型にしか触らない**)
11. **コア型(正準形は §4.6 で確定済み・v2.6)**:`State`(Schema 付き map)/ `Plan`(操作列 + 破壊性 +
    原因タグ + ID + 診断)/ `ID`〈`(provider, scope, id)` 複合〉/ `Scope`(ラベル集合 + パス)/ `Event`
    (単一型 + kind enum + scope/resource/revision 参照)、および `rendered_revision` 算出定義〈**scope 単位で
    算出可能**・§26.1〉。**構造は §4.6 で確定済みで §26.1 A は空**。第0スプリントは算出定義の厳密な文書化と
    型の実装に着手する(新規の着手前決定は残っていない)
12. **早期収束の seam**:`DesiredSource.Watch`(§13.5)+ **scoped run(部分収束)+ 合流キュー**(§6.8)。
    reconcile/plan とほぼ同一コードパスを部分 DAG に適用するだけなので増分が小さい。v1a では FileSource の
    webhook/merge フックによる `Watch` を対象とする(DBSource 連動は v2)
13. **スケールアウトの型切り**(実装は必要時):`Capabilities.BulkObserve`(§5.7)+ Executor の per-resource
    `Read` fallback 経路、`sakutei --claim`(§12.4)の起動フラグと scope-selector パース

**機能**
- 収束ループ(level-triggered / DAG / wave / 非同期 Apply / in-flight 排他 / backoff)
- **`tier: foundation`**(基盤サービスを依存の根として DAG 順序に反映)
- render パイプライン(2フェーズ / TTL キャッシュ / 障害時は前回値保持 + **incomplete≠empty 急減検知**・§7.6)
  ただし **v1a のフェーズ2 展開は非採番の固定値展開のみ**(採番連携は v1b・下記および §7.5)
- **rate limit + halt**(in-flight を含めて評価)+ **破壊的変更の累積時間窓予算**(§10.2)+
  **List 完全性検証**(件数急減 halt・scope 単位閾値宣言・§8.6)+ **異常検知の "halt-all で凍結" セマンティクス**(§10.2)
- **`planned_change` 宣言 + 専用予算レーン**(§10.9)——大量正当作業の解除弁。型と予算レーン評価は v1a
  (承認情報 requirements 連携は v1.5)。既存 `approved-large-change` タグを統合
- **運用プロファイル(solo / team / enterprise)**(§10.10)——規模別の安全既定値セット。既定は team
- `ownership: platform` の破壊的大量変更は **A タグでも承認必須**(悪意/誤操作の両方を止める・§10.2。solo は cooling-off で代替可・§10.10)
- Event 型 + 構造化ログ(JSON)出力
- conformance レポート + **`sakutei status` のデバッグ体験**(原因タグ + provider 階級表示 +
  **軽量 last-seen リング**(S3・非永続で "前周は X だった" を添える・§10.3)+ 下流波及 D)
- `FileSource`(git)+ YAML/JSON `Loader` + JSON Schema 検証
- Vault `SecretBackend` 1つ + 段0 からのトークン注入(**provider 資格は §14.5 の SecretRef 直結。broker は無い**)
- **最小 SDK**:Schema 駆動 diff エンジン(順序付き含む)/ secret マスキング / managed 除外 /
  エラー分類 / version 埋め込み。**Schema lint はロード拒否レベルで強制**(§5.2・§5.5)
- devenv 層A(フェイク provider)+ 層B(軽量本物)+ カオステスト基盤(§22)
- provider 2〜3個(DNS + VM + FW/順序付き。**管理NWコアは収束対象にしない**・§19.3 / M-4)

#### 24.1b v1b — brownfield 導入と採番連携

- **`sakutei import`(reverse-render)**(§23.3)+ **managed 候補の自動サジェスト**(§23.3)
- **`ScopeMode`(converge / sync)+ Sync Mode の反映ループ**(§23.8)
- **`AllocationAuthority` interface + 最小実装**(SQLite・§16.5)と **render フェーズ2 の採番連携展開**
  (§7.5)。**採番を使う合成リソースは v1b から**(v1a では非採番の固定値展開のみ)
- **allocator HA 最小形 + Lookup キャッシュ + waiting-allocation 縮退**(§16.5/§16.7/§16.8):段0 VM-HA 上の
  active/standby + fencing、割当事実のシャードローカル無期限キャッシュ(S3)、allocator 停止時の
  `waiting-allocation` 縮退。**プール分割 by claim**(intent での disjoint レンジ宣言・§16.5)
- テストハーネス(契約テスト自動生成)+ golden path テンプレート(§5.2)+ **BulkObserve 等価性テスト**
  (§5.8。fake provider 上で `BulkObserve(scope) ≡ {Read(id)}` を検証)
- **`ready_condition` と複数 Cycle に跨る依存解決**(§6.7)+ **capacity gate**(§10.2)
  ——最初の実インフラ provider(VM/物理)を書く段で必要になる
- **BulkObserve の実バルク実装 + 一貫性契約**(§5.7/§5.8)——バルク API を持つ provider(vCenter/Proxmox 等)
  から書く。complete-or-error / prune 非流用 / `observed_at` 刻印を SDK で強制。型は v1a で切ってあるので追加は
  provider 単位で局所
- **静的シャーディングの実動作**(§12.4):`--claim` フィルタ + fencing による二重 claim 検知 +
  再シャード Workflow(drain → claim 変更 PR → fencing 世代更新)。型(起動フラグ)は v1a で切り済み
- **claim の git 配布 + S2-free シャード階級 + エッジ資格局所性**(§12.5/§15.7):`intent/<scope>/...` レイアウト
  規約 + sparse checkout / partial clone、ミラー中継層、S2-free シャード(Resource 収束のみ)、店舗ローカル
  file SecretBackend への資格局所常駐、開店/閉店 = claim 変更 PR ハンドオフ

#### 24.1.5 v1.5 — 手続きの最小形(v2 から前倒し)

**設計判断**:20 構成の 12 が Task/Workflow に依存した事実を踏まえ、**最も需要の高い最小形を前倒す**。
多相 Task 状態機械・自動段階ロールアウトは v2 のまま(重く、需要が特定領域に偏る)。

- **単純 Task**(§9.1 単純型):一度実行して現実側に完了印。migration / 一回性操作。S2 は進行フェーズのみ
- **最小 Workflow**:**直列(one-at-a-time)+ 不変条件(invariant)+ 手動承認ゲート**のみ(§9.2・§10.5)。
  ローリング/DR の骨格をこの3要素で表現する。**動的 select・自動 canary 判定・自動段階ロールアウトは v2**
- **承認ゲートの N-of-M 拡張**(§10.5)——承認者要件(N-of-M・役割制約・separation〈申請者≠承認者〉・
  全承認者の S4 記録)。ゲート定義の `requirements` と承認 API の署名対応のみで実装(17 の 4-eyes)
- **災害時運用経路(DR モード)**(§18.1)——DR intent のプリステージ(claim ハンドオフ+DR Workflow の
  組み合わせ)+ 緊急ローカル書込経路(書込 break-glass の型 + Event/S4 検出 + 事後合流 PR 起票)
- **`remediation schedule`**(§10.8)——保守窓文化の構成(03/15/17)で必須なので v1.5 で入れる

### 24.2 v2 以降

**多相 Task 状態機械**(§9.1 多相型)/ **自動段階ロールアウト + 自動 canary ゲート**(§10.5)/
health(observability as Resource)/ 常設 canary(§17.3)/ Prometheus exporter / `DBSource`
(および `DBSource` 連動の `Watch`・§13.5)/ credential broker(§15)/ **分散 Executor + lease 縮退
(§12.3)+ 動的ルーター/自動リバランス(§12.4 段3)**/ Observe 間引き・tier 別頻度・watch 併用
(§12.2)/ intent UI(catalog)/ twin フル機能(§22.5)/ 抽象リソース(§5.6)。

**注**:早期収束の Watch seam + scoped run(§6.8)と静的シャーディング(§12.4)は v2 ではなく **v1a/v1b**
に前倒し済み(§24.1a/§24.1b)。v2 に残るのは **動的**ルーター/自動リバランスと DBSource 連動 Watch のみ。

### 24.3 作らないもの(委譲)

シークレットストア本体(Vault)/ 時系列監視・通知ルーティング(Prometheus・Alertmanager)/
配置最適化(DRS)/ 認可・課金・ポータル(上位プラットフォーム)/ アプリ層のデプロイ・スケール(k8s)/
CMDB 全体(採番のみ自作)。

### 24.4 最初の一歩(v1a のゴール)

**DNS レコード provider** は Read / Apply が最もクリーンで、検証環境も軽い。そこから
podman コンテナ / Caddy ルート / TLS 証明書 と広げ、「**触っていないのに勝手に理想状態へ戻る**」体験を
数百行のオーケストレータ + provider 数個で作れる。これは §24.1a(v1a)の範囲であり、**SDK フルセット・
import・Sync Mode を含む §24.1 全体ではない**——両者の規模は1桁違う(前者 ≈10 人月・後者 ≈22.5 人月)。
greenfield / devenv で価値実証できるため、import(v1b)は v1a には要らない(§23.7-3 の「新規構築部分から」)。

**既存基盤へ入れる場合**は v1b の `import` を待ち、§23 に従う——`import` → `alert-only` で観察 →
scope 単位で昇格。**最初に価値が出るのは Apply ではなく可視化**(実態を暴くこと)である。

---

## 25. 到達点

新プリミティブの追加は **Task と Workflow の2つのみ**。オンプレ運用の広域(物理〜運用ライフサイクル)を、
**3プリミティブ + 少数の横断項目 + seam 群**で吸収できた。衛星が生えたのは機能の膨張ではなく、
**責務境界を正しく引いた結果**であり、core は小さいまま保たれている。

**k8s から借りたのは収束原理であって、スコープではない。** その原理をデータセンター運用という別ドメインに
移植し、k8s が扱っていない複雑さ(手続き・物理・テナント・DR・自己運用)を引き受けた。
**k8s の偶有的複雑さ(実装機構)は削り、本質的複雑さ(運用の泥臭さ)は引き受けた**——方向が逆の2つの動きで
あり、「芯は小さく、適用面は広い」が両立している。

---

## 26. 未決事項(決定待ち)

設計は概ね閉じている。**A(実装着手前に決めるべきもの)は本改訂(v2.6)で空になった**——後から変えると全
プロバイダに波及するコア型・着手前決定はすべて確定した。残るは B〜E(着手後・運用前でよいもの)である。

### 26.1 A:コア型と着手前決定 —— **本改訂(v2.6)で空(全項目決定済み)**

v2.3〜v2.5 で A を段階的に消し込み、**v2.6 で残っていた `State`/`Plan`/`Scope`/`Event` の正準形を型として確定
(§4.6)し、S4 永続化先も確定(§17.5)した**ことで、**A は空になった**。実装は第0スプリントで下表の「決定の所在」を
参照して型を書き始めればよく、A に属する未決は残っていない。**下表は「A が空である」ことの根拠一覧(既決の索引)**
であり、これ以降 A に新規項目を積まない(積むべき着手前決定が現れたら A を再オープンする)。

| 旧 A 項目 | 決定 | 決定の所在 | 確定版 |
|---|---|---|---|
| **A-1. コア型 `State`/`Plan`/`Scope`/`Event`** | 正準形を型として確定(State=Schema 付き map / Plan=操作列+破壊性+原因タグ+ID+診断 / Scope=ラベル集合+パス / Event=単一型+kind enum+scope/resource/revision 参照) | **§4.6** | **v2.6** |
| **A-1. `ID` の内部表現** | `(provider, scope, id)` 複合。scope=ルーティングキー。global-unique は棄却 | §4.6, §12.4 | v2.4 |
| **A-2. Provider interface 正準形** | §5.1 に一本化(`Read`→`Observation` / Provider は `Diff` を持たない / `ObserveStatus` 廃止) | §4.1【正準定義の所在】, §5.1 | v2.3 |
| **A-3. `rendered_revision` 算出定義** | decision-relevant な入力のみの決定的ハッシュ・TTL で揺れない・**scope 単位で算出可能** | §7.5, §10.3, §26.1 注記(下記) | v2.4 |
| **A-4. S2 永続化先** | quorum ストア or 同期レプリカ + fencing。allocator HA 最小形(段0 VM-HA 上 active/standby + fencing)に割当記録が相乗り | §16.5, §16.7 | v2.5 |
| **A-4b. S4 永続化先** | **WORM ストレージ or 外部即時 push**(侵害ホストが自分の犯行ログを改竄できないこと) | **§17.5** | **v2.6** |
| **A-5. `ready_condition` 名前空間** | Schema で宣言・未宣言 condition への依存はロード時拒否 | §6.7 | v2.3 |

**`rendered_revision` 算出定義の要点(A-3 の確定内容を再掲)**:decision-relevant な入力のみの決定的ハッシュとし、
「何を含め何を含めないか」「TTL ヒット/ミスや列挙順で揺れないこと」を厳密に規定する(原因タグ E と per-resource
帰属・§10.3 の判定基盤)。**scope 単位で算出可能**とし、あるシャードの render が他シャードの観測に依存しないことを
保証する(§12.4 の shared-nothing 独立収束の前提)。全域一括ハッシュは 1 scope の変更が全シャード再収束を誘発し
部分収束・シャード独立性を壊すため採らない。この算出定義の**厳密な文書化そのものは第0スプリントの成果物**だが、
「何を決めるか」は上記で確定しており、A の未決ではない(実装タスクとして残るだけ)。

### 26.2 B:機構の置き場

| 項目 | 未決の内容 | 関連 |
|---|---|---|
| **firmware 多相 Task の "相の途中 death"** | 各相の入口 Observe(§9.1)は相境界の death は守るが、Applying 途中(書き込み中)の death を「リブート中」と bricked で区別できない。Applying を "危険区間" としてマークし、区間内 death は Observe で Done 確認できない限り **auto 再開を禁止し人手ゲート**(S-06) | §9.1 |
| **Ansible executor の Observe 頻度** | fork+SSH の高レイテンシ層で tier 別間引き(§12.2)をどう既定するか | §12.2 |

### 26.3 C:運用パラメータ(動かしながら決めてよい)

- Sync Mode の commit 主体(bot アカウント・署名の有無。監査の帰属に効く・§23.8)
- scope 昇格基準の「N 日間 diff ゼロ」の N(§23.4)
- reconcile 間隔の既定値、メンテナンス窓 / freeze の粒度(§10.6)

### 26.4 D:意図的な先送り(**再検討トリガ**を決める)

先送り自体は正しいが、**いつ見直すか**を決めておかないと、放置されるか早すぎる時期に蒸し返される。

| 項目 | 推奨トリガ | 節 |
|---|---|---|
| 抽象リソース(capability への依存) | **2つ目の同種プロバイダ実装が実際に必要になったとき**。1つしかない間は抽象化しても検証できない | §5.6 |
| CUE への `Loader` 移行 | テンプレート側にロジックが溜まり始めたとき(カスタム関数の増加) | §13.3 |
| シャーディング / 分散 Executor | §6.2 の早期警戒線(定義済み) | §12.2, §12.3 |

### 26.5 E:プロジェクト運営(技術外)

- **本書と参照実装文書の分割**(§1.5 で境界は定義済み)。参照実装の内容を足す時点で境界が崩れるため、
  **早いほど安い**。
- プロジェクト名の衝突確認(コンポーネント名を機能名に改めたため、確認対象は `Sakutei` のみ)。
- 実務知見が設計に入っている以上、**職務発明・OSS 公開に関する所属組織の規程確認**は、コードを
  書き始める前に済ませておく方が安全(後から覆ると成果が無になる種類のリスク)。
