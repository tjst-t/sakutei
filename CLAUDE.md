# CLAUDE.md

このファイルは、本リポジトリで作業する Claude Code（および他の AI エージェント）向けのガイドです。

## プロジェクト概要

**Sakutei（作庭）** は、オンプレミス基盤全体（電源/BMC → ネットワーク → FW/SDN → ストレージ → ハイパーバイザ → VM → ゲスト OS → コンテナ）を対象とした**宣言的インフラ収束エンジン（コントロールプレーン）**の設計プロジェクト。

Kubernetes からは *収束原理*（level-triggered な収束ループ）だけを借り、実装機構（apiserver・etcd・scheduler・admission・CRD）は意図的に捨てている。位置づけは「**Ansible の冪等性 + k8s のループ**」。最大の特徴は **観測 state レス**（キャッシュした「現在状態」を持たず毎周期リアルを再観測する）で、これにより Terraform の three-way-diff / state ドリフト問題を構造的に排除する。名称は平安期の作庭書『作庭記』に由来する。

> **重要: 現時点でコードは未実装。** このリポジトリは設計仕様（ドキュメント）のみを保持する。ソースツリー・ビルド/テスト/デプロイのコマンドはまだ存在しない。

## リポジトリ構成

```
.
├── CLAUDE.md                                  # このファイル
├── README.md
└── docs/
    ├── README.md                              # ドキュメント索引（まずここを見る）
    ├── design/
    │   ├── sakutei-design-v2.4.md             # ★設計の正（source of truth）
    │   ├── CHANGELOG-v2.4.md                  # v2.3→v2.4（早期収束パス・BulkObserve・claim）
    │   ├── sakutei-design-v2.3.md             # 旧版（歴史として保持・編集しない）
    │   ├── CHANGELOG-v2.3.md                  # v2.2→v2.3 の変更一覧（20件・見送り理由つき）
    │   ├── sakutei-design-v2.2.md             # 旧版（歴史として保持・編集しない）
    │   └── reconciler-design-v1.md            # コアの初期設計メモ（背景資料）
    ├── examples/
    │   └── proxmox-mysql-dbaas-schema.md      # Provider/合成リソースの worked example
    ├── review/
    │   ├── README.md                          # 第1ラウンド総括（対象: v2.2）
    │   ├── 01〜03-*-review.md                 # 設計レビュー（整合性/安全性/実装可能性）
    │   ├── scenarios/01〜20-*.md              # 20構成の適用可能性検討（対象: v2.2）
    │   ├── v2.3/                              # 第2ラウンド（対象: v2.3）: README.md + scenarios/
    │   └── v2.4/                              # 第3ラウンド（対象: v2.4）: README.md + scenarios/（7構成）
    └── web/
        └── sakutei.html                       # ビジュアルなランディングページ
```

**権威関係:** `sakutei-design-v2.4.md` が唯一の「設計の正」。v2.2/v2.3 は旧版（編集しない）、`reconciler-design-v1.md` は詳細だが古く、矛盾時は **v2.4 が優先**。すべて日本語で書かれている。

## アーキテクチャ（設計）

- **reconciler（収束コア）** — 単一バイナリ・単一デーモン。収束ループ（Observe→Diff→Apply/prune）、DAG/wave 依存解決（Kahn 法・wave 単位で並列）、render パイプライン（2 フェーズ）、3 プリミティブ、安全機構、Event bus。
- **衛星群（別プロセス・別ソフトウェア）** — ライフサイクル/可用性/攻撃面の都合で分離:
  - **credential-broker**（関守）— ターゲット認証の一元化（JIT 短命クレデンシャル発行）。reconciler が実行時に依存。
  - **allocator**（台帳）— VLAN/IP/ASN プールの採番権威。state（冪等性の記憶）を持つ。
  - **catalog**（図面）— intent 用の薄い UI。独自の DB を持たず git が真実の源。
- **外部委譲（作らない）** — Vault（シークレット値）、IdP/SSO（人の認証）、Prometheus/Alertmanager（監視/通知）、DRS/ハイパーバイザ（配置最適化）、上位テナント基盤（認可/quota/課金）、git（intent 履歴/レビュー/監査）、k8s（アプリ層）、Ansible modules（ロングテール実行）。
- **ブートストラップ:** 段0（人が Ansible + 金庫で構築する不変の土台: Proxmox 3ノード + Ceph + VM-HA 上に Vault/credential-broker/reconciler）→ 段1以降は自律収束。

## 設計を貫く原則

1. **level-triggered + 観測 state レス** — 毎周期 desired と actual を再導出し、観測状態を保持しない。
2. **いつ = 収束 / どう = 手続き** — 「いつ・どの順で変えるか」は収束が決め、「どう変えるか」は Task/Workflow が実行する。
3. **確信が持てないとき、加算はしても減算はしない**（unknown ≠ absent、`pruneable:false` がデフォルト、分断/blast-radius 時は halt）。
4. **持たない・委譲する** — 値/配置/予測/認可は自前で持たず外部に委ねる。
5. **seam は今切る、split は後** — 拡張/分離の接点となる interface を早期に定義し、コアは変えずに後で分割する。
6. **ownership がすべてを貫く** — `platform`（PR 必須・厳格）か `tenant`（即時書込・緩い）かの 1 軸が、書込経路/remediation/レート制限/blast-radius/source/シャード境界を決める。

## 中核の用語

- **Provider / Lister** — 2 層のコア抽象。`Provider`（`Schema`/`Capabilities`/`Observe`/`Diff`/`Apply`）はリソース単位、`Lister`（`List`）は集合列挙で **prune の前提**。
- **Observation vs ConvergenceStatus** — Observe は Availability（`ok`/`unknown`/`converging`）+ State を返し desired を知らない。`drifted`/`converged` は Diff から**導出**される報告用状態。
- **3 プリミティブ** — **Resource**（宣言的・収束）/ **Task**（単一リソースの手続き）/ **Workflow**（複数リソースの順序付き手続き + 不変条件）。
- **render パイプライン** — フェーズ1（外部入力解決・副作用あり: inventory 合成・Vault メタデータ解決・allocation）+ フェーズ2（合成リソースの決定的展開）。
- **cause tag（A〜E）** — 差分の原因分類。A 意図的 / B ドリフト / C ツール更新（ghost diff）/ D 波及 / E 外部入力変化。`intent_revision`（人のコミット）と `rendered_revision`（intent + 解決済み外部入力のハッシュ）の差で検出。
- **DeletionPolicy** — `ExplicitAbsentOnly`（open-world）vs `PruneWithinScope`（closed-world、Lister 必須）。破壊的型は `pruneable:false` がデフォルト。
- **状態クラス S1〜S4** — S1 観測（保持しない）/ **S2 進捗（外部永続必須・喪失=事故）** / S3 履歴（メモリのみ）/ S4 監査（append-only）。
- **SecretRef** — 専用型。`String()` は `***` を返し自動マスク、値は Apply 直前に JIT 解決。git への書き戻しループなし。
- **devenv（坪庭）/ twin（縮景）** — 隔離検証 DC / デジタルツイン（render の外部入力を差し替えて本番プールに触れない）。
- **早期収束パス（§6.8）** — 「edge-triggered な起床、level-triggered な真実」。`DesiredSource.Watch()` のヒント（revision+scope のみ、状態は運ばない）で影響サブ DAG だけ即時に新鮮な Observe→Diff→Apply。cause A/E のみ・集合差 prune 禁止・予算カウンタは定期経路と共有。ヒント喪失=次周期が拾う。
- **BulkObserve（§5.7）/ claim-as-intent（§12.4）** — スケールの 3 段: ①バルク観測でスケール算術を桁で改善（API 系は単一デーモン 1 万規模）→ ②`--claim <scope-selector>` の静的シャーディング（割当自体を git の intent として宣言、二重 claim は fencing で検知、ルーター不要）→ ③動的ルーター/リバランスのみ v2。**シャード間協調は現実を介して行う**（相手の作った現実を Observe して gate。シャード間 RPC は正しさに関与しない）。

## CLI（設計上の verb）

```
sakutei              # 常駐して回り続ける（本来の姿）
sakutei plan         # dry-run: 何が変わるか（Diff で止まる）
sakutei reconcile    # 1 回収束（手動キック）  --expect-revision <rev>
sakutei status       # drifted の一覧（適合レポート）
sakutei import       # 既存リアルを intent として書き出す（ブラウンフィールド）
                     #   --scope <範囲> --out intent/
```
`apply` は意図的に主 verb にしない（「叩く道具」ではなく「回り続けるもの」）。承認ゲート API: `POST /workflows/{id}/gates/{gate}/approve`。

## 想定技術スタック

- **言語:** Go（全 interface 定義が Go）。単一 `sakutei` バイナリ + サブコマンド。
- **設定形式:** YAML + JSON（`Loader` 経由）、JSON Schema で検証。将来 CUE を検討（Starlark は round-trip 不可のため却下）。
- **状態保存:** メモリ + file(git) が主。将来 `DesiredStore` として bbolt / SQLite。**etcd や必須 DB プロセスなし。**
- **シークレット:** HashiCorp Vault（KV v2）。
- **ブートストラップ:** Proxmox 3ノード + Ceph + VM-HA、Ansible で段0 構築/unseal。
- **検証:** fake Provider（主戦場）、containerlab、Podman、nftables、Proxmox nesting など。

## 実装に関する規約・注意

- **`generated/` は自動生成専用** — RenderedSource / Sync Mode の出力先。自動生成ヘッダ必須、直接コミットは拒否（二重管理防止）。人が編集しない。
- **`intent/` は人が書く intent の出力先。**
- **seam interface が実質的なモジュール境界:** `Provider`/`Lister`・`Executor`・`Loader`・`DesiredSource`・`DesiredStore`・`SecretBackend`・`AllocationAuthority`・`CredentialProvider`。
- **Provider 作成のしやすさが採用を決める**（Terraform はエコシステムで勝った）。作者は約 4 関数（+ prune 用 `List`）を書き、SDK が diff/秘匿マスク/managed 除外/バージョン埋込/エラー分類/テストハーネスを担う。
- **Schema 設計が最難関** — secret/managed/ordered/identity の宣言を誤ると安全保証が黙って無効化される。identity は運用上の変更頻度で選ぶ（シリアルより `leaf03-p12` のような物理トポロジ座標）。
- **安全機構は level-triggered の要**（速く収束する力は速く壊す力、しかも毎周期）。remediation mode（`auto`/`alert-only`）・レート制限（破壊的変更に加重）・段階ロールアウト。デフォルトは halt、明示的意図で進む。global `freeze` で全体を一括 alert-only に。**「1 commit で全 VM 削除」を止める rate-limit+halt と remediation-mode は v1 必須。**
- **plan と reconcile はそれぞれ独立に Observe する**（plan の観測を reconcile に持ち越すと観測 state を再導入してしまう）。
- **ブラウンフィールドは read-only/alert-only から**始めて信頼を得る（import → alert-only → スコープごとに昇格）。人の変更を早々に自動で巻き戻さない。

## 実装スコープ（v2.3 で再スライス）と未決事項

- **v1a（コア実証 ≈10人月）:** コア型+収束ループ+安全機構（累積時間窓予算・List 完全性検証含む）+最小 SDK+Provider 2 個+devenv。allocation は非採番のみ。v2.4 で追加: `DesiredSource.Watch` seam+scoped run+合流キュー（早期収束）、`BulkObserve`/`--claim` の型切り（実装は v1b 以降）。
- **v1b（brownfield 導入・採番連携）:** `sakutei import`/Sync Mode、tier 別 Observe 頻度（間引き）、採番連携、inventory 検証、静的シャーディング（claim-as-intent）実装。
- **v1.5（v2 から前倒し）:** 単純 Task+最小 Workflow（直列+不変条件+手動承認ゲート）+remediation schedule（時間帯連動 mode 切替）。
- **v2+ のまま:** 多相 Task 状態機械、自動段階ロールアウト/canary、DBSource、credential-broker、分散 Executor、動的シャードルーター/リバランス、intent UI。
- **v2.3 の主要新概念:** `external_owner`（外部オーケストレータ所有 scope の除外宣言+協調 seam）、段0-minimal（1 ノード縮退形）、`ledger_only`/`observability:none`（観測不能資産の階級）、fencing token。詳細は `docs/design/CHANGELOG-v2.3.md`。
- **v2.4 の主要新概念:** 早期収束パス（§6.8）、BulkObserve（§5.7）、claim-as-intent 静的シャーディング（§12.4）、`DesiredSource.Watch`（§13.5）。詳細は `docs/design/CHANGELOG-v2.4.md`。
- **実装前に必ず決める（§26 カテゴリ A）:** コア型 `State`/`Plan`/`Scope`/`Event`、Provider interface 正準形（§5.1 に一本化済み）、S2 永続化先（quorum ストア+fencing）。v2.4 で確定済み: `ID`=`(provider, scope, id)` 複合、rendered_revision は scope 単位で算出、予算カウンタはシャード単位。
- **非技術的未決:** 実装前に雇用主の IP / OSS 公開規程の確認が必要（§26.5）。自組織 社内利用が主目的、OSS 化はボーナス。

## 作業時の指針

- ドキュメントはすべて日本語。追記・修正時も**日本語**で、既存の用語（英語機能名の component 名、庭園メタファの別名）を踏襲する。
- 設計に関する記述を変更するときは `docs/design/sakutei-design-v2.3.md` を正とする。旧版やサンプルとの矛盾は v2.3 に寄せる。設計を改訂するときは新バージョンのファイルを作り、旧版は編集せず残し、CHANGELOG を添える。
- コンポーネント名は機能的な英語名（reconciler / credential-broker / allocator / catalog）を使う。固有名詞は「Sakutei」のみ。
