# CLAUDE.md

このファイルは、本リポジトリで作業する Claude Code（および他の AI エージェント）向けのガイドです。

## プロジェクト概要

**Sakutei（作庭）** は、オンプレミス基盤全体（電源/BMC → ネットワーク → FW/SDN → ストレージ → ハイパーバイザ → VM → ゲスト OS → コンテナ）を対象とした**宣言的インフラ収束エンジン（コントロールプレーン）**の設計プロジェクト。

Kubernetes からは *収束原理*（level-triggered な収束ループ）だけを借り、実装機構（apiserver・etcd・scheduler・admission・CRD）は意図的に捨てている。位置づけは「**Ansible の冪等性 + k8s のループ**」。最大の特徴は **観測 state レス**（キャッシュした「現在状態」を持たず毎周期リアルを再観測する）で、これにより Terraform の three-way-diff / state ドリフト問題を構造的に排除する。名称は平安期の作庭書『作庭記』に由来する。

> **現状:** 設計は v3.36（v3.4〜v3.11 は v1b・v1.5 の実装に設計書を追いつかせた改訂、v3.12〜v3.14 は v1 の試用で出た決定（SOA シリアル・自分の prune を急減判定から差し引く・level 報告の既定値）、v3.15・v3.16 は Redfish BMC provider（#48）の決まり、v3.17 は単純 Task の実 provider（sqlmigrate、#92）、v3.18 は sqlmigrate の試用で見つけた strategy: task の level 報告の穴を直した決まり（#112）、v3.19 は allocator Lookup キャッシュの cooldown 衝突バグ（#105）と dnszone / nftables の scope 固定（#106）の決まり、v3.20 は外部 provider の組み込み方（`pkg/cli`・`--provider-config`、§28.9）と CLI の直し（一回性コマンドの `--inventory-file`・`status` の観測できないものの報告と `--json`・`allocator release`／`list`、#114〜#119）、v3.21 は nftables の実の ruleset 版（live）と S3 互換のバケット provider（#121〜#123）、v3.22 は PostgreSQL・sqlmigrate の PostgreSQL 版・Ansible の provider と Workflow の複数 provider（#87、#125〜#127）、v3.23 は ssh 越しに離れたホストを扱う層・通知の口・ゲスト OS の観測・/metrics（#130・#131・#86・#54）と s3-bucket・postgresql の直し（#132・#133）。v3.24 は Workflow の一度きりの操作（action：Redfish の boot_once・reset）と応答ファイルを書く template-file（#139）。v3.25 は planned_change の reimage で入れ直しの Workflow を起こす `trigger: reimage`（#88）。v3.26 は採番の値を文字列に埋め込むテンプレート（`template: "192.0.2.{n}"`、#145）。v3.27 は Redfish の FW の版の観測と多相 Task での更新（`redfish-firmware`・承認の要る相、#148）。v3.28 は離れたホストのファイルと systemd のユニット（quadlet を含む）を保つ provider と、依って立つファイルを書き換えたら入れ替える口（#155）。v3.29 は通知の口の宛先を複数に（Alertmanager を複数台で動かす構成、#154）。v3.30 は /metrics の値を観測して require・ready_condition から参照する `metrics-probe`（#149）。v3.31 は status の宣言外（unmanaged）の検出を、prune しない型にも、宣言が 0 件でも設定されている provider にも行う決まり（#164）。v3.32 は宣言で消す明示の absent（`ensure: absent`、planned_change のレーンを承認にして予算にも数える、#163）。v3.33 は proxmox-vm の pool（作るときに渡し、違えば移す。v3.34 は原則「正しい状態は宣言に置き、ツールの中だけに持たない」（§1.7）と、採番の結果を宣言の側のファイルへ書き戻しそれを正として読む `write_back`（store は写しに下げる・`sakutei allocator import`、#157）。pool に絞ったトークン）と常駐が `--cause-state` を受けない決まり（#165）。v3.35 は Apply のあとに読み直して効いたかを確かめ、効かない Apply を上限で止める（oscillating、§11.1 Tier3 の配線）と一回性の `reconcile` の終了コード 4（#166）。v3.36 は触らない宣言を intent に入れる `external_owners:`（§7.3。provider の設定の除外は安全の網として残し、食い違えば読み込みで断る、#158）。経緯は `docs/architecture/CHANGELOG-v3.*.md`）。実装は **v1（v1a・v1b・v1.5）が完成**（タグ `v1.0.0`。試用での修正と Redfish BMC provider を足した `v1.1.0`、外の provider の口・nftables の live 版・s3-bucket・sqlmigrate を足した `v1.2.0`、PostgreSQL・Ansible の provider と Workflow の複数 provider を足した `v1.3.0`、離れたホスト・通知・ゲスト OS の観測・metrics を足した `v1.4.0`、多相 Task・OS の導入と入れ直し・BIOS・NETCONF の観測・読み取り専用の口を足した `v2.0.0`。変更点は `CHANGELOG.md`）。次は v1 を実際に使って確かめること（手順は `docs/trial/README.md`）。v2（Backlog #20）は v1 を試してから人が着手を決める。Redfish BMC provider（#48）は v1.1.0 で完了。

## 仕事の管理（Backlog）

- 仕事の一覧・優先順・状態は **Backlog**（git の `backlog` ブランチの `tasks.json`）が唯一の正。旧 `docs/ROADMAP.json` は廃止した（v1a のスプリント 1〜9 は Backlog に done のストーリー `v1a-1`〜`v1a-9` として移行済み。受け入れ条件の全文は git 履歴のコミット 9d7cf32 の `docs/ROADMAP.json` にある）。
- 受け入れテストには `[AC-<スプリントID>-<ストーリー>-<番号>]`（例 `[AC-Sc7a647-2-1]`）のタグが付いている。旧スプリント ID と Backlog の対応は各ストーリーの本文に書いてある。
- 新しい作業はストーリー → タスクに分けて Backlog に置き、完了条件は計測できる形で書く。

## 開発コマンド

```console
$ make build      # ./sakutei をビルド
$ make test       # 全テスト（tests/acceptance の受け入れ・chaos を含む）
$ make vet        # go vet
$ make check      # build + vet + test
$ make demo       # plan → converge → drift → self-heal のウォークスルー
$ make test-race  # race 検出付きのテスト（cgo のため gcc が要る）
$ make integration-redfish  # Redfish の統合テスト（qemu-bmc 0.10.0 × 2 を containerlab で。KVM・docker・sudo が要る。check・CI には入れない）
$ make integration-proxmox  # proxmox-vm の統合テスト（Proxmox VE 9.2 を qemu で自動導入して動かす。KVM・qemu・docker・sudo が要る。check・CI には入れない）
```

Go 1.22 以上。外部依存なし（stdlib のみ）。CI は `.github/workflows/ci.yml`（GitHub Actions）。push（main）と PR のたびに、Go 1.22 と最新の安定版で `make check` を、最新の安定版で `make test-race` を回す。

## リポジトリ構成

```
.
├── CLAUDE.md                                  # このファイル
├── README.md
├── Makefile
├── cmd/sakutei/                               # main（cli.Main() を呼ぶだけ）
├── pkg/                                       # 公開 API: cli（CLI の入口。常駐ループ・plan・reconcile・status、§28.9）/
│                                              #   core（正準型）/ provider / registry / sdk
├── examples/external-provider/                # 外の provider を足した自前のバイナリの例（別の go.mod）
├── internal/                                  # reconcile・diff・budget・prune・cause・render・loader・
│                                              #   remediation・statusreport・fakeprovider・dnszone・nftables 等
├── tests/acceptance/                          # 受け入れテスト・chaos テスト（AC タグ付き）
├── tests/integration/redfish/                 # Redfish の統合テスト（build tag integration_redfish）
└── docs/
    ├── README.md                              # ドキュメント索引（まずここを見る）
    ├── architecture/
    │   ├── spec/                              # ★設計の正（source of truth）v3.36 分冊
    │   │   ├── README.md                      #   索引（版・改訂規約・目次・§→ファイル対応）
    │   │   └── part1〜part8-*.md              #   8部29節の本文（§番号は全部で一意）
    │   ├── CHANGELOG-v3.0.md                  # v2.6→v3.0 構成対応マップ+分冊化の記録
    │   └── old/                               # 旧版アーカイブ（編集しない）
    │       ├── sakutei-design-v2.2〜v2.6.md   #   旧版設計書
    │       ├── CHANGELOG-v2.3〜v2.6.md        #   各改訂の変更一覧（見送り理由つき）
    │       └── reconciler-design-v1.md        #   コアの初期設計メモ（背景資料）
    ├── examples/
    │   └── proxmox-mysql-dbaas-schema.md      # Provider/合成リソースの worked example
    ├── review/
    │   ├── README.md, 01〜03-*-review.md      # 第1ラウンド: 設計レビュー3観点（対象: v2.2）
    │   ├── scenarios/                         # 第1ラウンド: 20構成の適用可能性検討
    │   ├── v2.3/ v2.4/ v2.5/ v2.6/            # 第2〜5ラウンド（差分再判定。v2.6 で収束宣言）
    │   └── v3.0-audit-*.md                    # v3.0 の網羅性・整合性監査（反映済み）
    └── web/
        └── sakutei.html                       # ビジュアルなランディングページ
```

**権威関係:** `docs/architecture/spec/`（README.md + part1〜8）が唯一の「設計の正」（現行 v3.36。骨格は v2.6 と内容等価の全面再構成 v3.0 で、v3.1〜v3.36 は分冊を直接改訂）。`old/` は旧版（編集しない）、矛盾時は **spec/ が優先**。すべて日本語で書かれている。

## v3.0 の構成（8部29節・分冊）

| 部 | 節 | ファイル | 内容 |
| --- | --- | --- | --- |
| I 思想と境界 | §1-3 | part1-philosophy.md | 課題認識 / 作る・委譲する・捨てる / **八つの原則**と適用境界 |
| II 中核モデル | §4-7 | part2-core-model.md | 正準型と状態分類 S1-S4 / Provider 抽象 / 3プリミティブ / 所有と境界 |
| III 収束機構 | §8-11 | part3-convergence.md | 収束ループ+早期収束パス / render / 差分と原因（cause tags）/ 多周期収束 |
| IV 安全機構 | §12-15 | part4-safety.md | 削除の規律 / 変更予算+planned_change / 修復の統制+プロファイル / 隔離と防御（fencing）|
| V スケールと分散 | §16-18 | part5-scale.md | 天井算術（BulkObserve）/ claim シャーディング+S2-free / 分散の原則 |
| VI 衛星と外部世界 | §19-22 | part6-satellites.md | allocator / secret と資格 / DesiredSource と catalog / 委譲と停止影響表 |
| VII 運用ライフサイクル | §23-26 | part7-operations.md | 段0・段0-minimal・段0-zero / ブラウンフィールド / 災害と復旧（DRモード）/ 検証と監査 |
| VIII 実装 | §27-29 | part8-implementation.md | スライス v1a/v1b/v1.5/v2 / Provider SDK / 残る課題と既知の限界 |

## 設計を貫く八つの原則（§3）

1. **level-triggered + 観測 state レス** — 毎周期 desired と actual を再導出し、観測状態を保持しない。
2. **いつ = 収束 / どう = 手続き** — 「いつ・どの順で変えるか」は収束が決め、「どう変えるか」は Task/Workflow が実行する。
3. **確信が持てないとき、加算はしても減算はしない**（unknown ≠ absent、`pruneable:false` がデフォルト、分断/blast-radius 時は halt）。
4. **持たない・委譲する** — 値/配置/予測/認可は自前で持たず外部に委ねる。
5. **seam は今切る、split は後** — 拡張/分離の接点となる interface を早期に定義し、コアは変えずに後で分割する。
6. **ownership がすべてを貫く** — `platform`（PR 必須・厳格）か `tenant`（即時書込・緩い）かの 1 軸が、書込経路/remediation/レート制限/blast-radius/source/シャード境界を決める。
7. **シャード間協調は現実を介して行う** — シャード同士は問い合わせない。相手の作った現実を Observe して gate する。シャード間 RPC は正しさに関与しない。
8. **衛星の停止は、変化を止めるが、維持を止めない** — 定常収束は衛星コールゼロで回る（割当事実は Release まで不変＝キャッシュ可）。allocator QPS は churn 比例。

## 中核の用語（v3.0 の§参照つき）

- **Provider / Lister**（§5）— `Schema`/`Capabilities`/`Read`/`Apply` + 集合列挙 `List`（prune の前提、complete-or-error）。階級: 完全/標準/最小 + 観測不能階級（`ledger_only`/`observability:none`）。
- **BulkObserve**（§5）— バルク観測 capability。一貫性契約: 非アトミック宣言・complete-or-error・**prune 非流用の型分離**・SDK 等価性テスト。API 系ターゲットは単一デーモン1万規模に。
- **Observation vs ConvergenceStatus**（§4, §10）— Observe は Availability（`ok`/`unknown`/`converging`）+ State を返し desired を知らない。`drifted`/`converged` は Diff から導出。
- **3 プリミティブ**（§6）— Resource（宣言的・収束）/ Task（単純=v1.5、多相=v2）/ Workflow（順序+不変条件+承認ゲート。N-of-M・申請者≠承認者対応）。
- **render パイプライン**（§9）— フェーズ1（外部入力解決・副作用あり）+ フェーズ2（決定的展開）。外部権威到達不能時は前回解決値を保持（空 desired を出さない）。
- **cause tag A〜E**（§10）— A 意図 / B ドリフト / C ツール更新（ghost diff）/ D 波及 / E 外部入力。`intent_revision` と `rendered_revision`（scope 単位算出）の差で検出。
- **早期収束パス**（§8）— 「edge-triggered な起床、level-triggered な真実」。`DesiredSource.Watch()` の ChangeHint（revision+scope のみ）で影響サブ DAG だけ即時に新鮮な Observe→Diff→Apply。cause A/E 限定・集合差 prune 禁止・予算共有。
- **planned_change**（§13）— 大量正当作業の宣言（scope/種別/上限/窓/承認）。専用予算レーン（バイパスでなく別枠）。List 急減 halt と連動。`kind: reimage` で再イメージの delete 誤計上を回避。
- **運用プロファイル**（§14）— solo（cooling-off で承認代替）/ team（既定）/ enterprise（N-of-M 既定）。remediation schedule（時間帯連動）との優先は most-restrictive-wins。
- **claim-as-intent / S2-free シャード**（§17）— シャード割当を git の intent として宣言（`intent/<scope>/` 規約+sparse checkout+ミラー中継）。エッジは Resource 収束のみで S2 レス（全損→再イメージ→pull→復旧）。開店/閉店=claim 変更 PR のハンドオフ。
- **external_owner**（§7）— 外部オーケストレータ（Heat/operator/VNFM/MHA 等）所有 scope の除外宣言+「観測して待つ」一方向協調 seam。
- **SecretRef / 資格局所性**（§20）— 値は収束しない。自動マスク・JIT 解決。エッジの write 資格は拠点ローカル file SecretBackend（爆発半径=当該拠点）。
- **DR モード**（§25）— DR intent プリステージ+書込 break-glass（署名付きローカルコミット・必ず検出・事後合流）。
- **状態クラス S1〜S4**（§4）— S1 観測（保持しない）/ S2 進捗（quorum ストア+fencing で外部永続）/ S3 履歴（メモリ）/ S4 監査（append-only+WORM/外部即時 push）。
- **devenv（坪庭）/ twin（縮景）**（§26）— 隔離検証 DC / デジタルツイン（render の外部入力差し替えで本番プールに触れない）。

## CLI（設計上の verb、§26）

```
sakutei              # 常駐して回り続ける（本来の姿）  --claim <scope-selector>
sakutei plan         # dry-run: 何が変わるか（Diff で止まる）
sakutei reconcile    # 1 回収束（手動キック）  --expect-revision <rev>
sakutei status       # drifted の一覧（適合レポート）
sakutei import       # 既存リアルを intent として書き出す  --scope <範囲> --out intent/
```
`apply` は意図的に主 verb にしない。承認ゲート API: `POST /workflows/{id}/gates/{gate}/approve`。

## 想定技術スタック

- **言語:** Go（全 interface 定義が Go）。単一 `sakutei` バイナリ + サブコマンド。
- **設定形式:** YAML + JSON（`Loader` 経由）、JSON Schema で検証。将来 CUE を検討（Starlark は round-trip 不可のため却下）。
- **状態保存:** メモリ + file(git) が主。S2 のみ quorum ストア+fencing。**etcd や必須 DB プロセスなし。**
- **シークレット:** HashiCorp Vault（KV v2）。段0-minimal は file SecretBackend。
- **ブートストラップ:** 段0 = Proxmox 3ノード + Ceph + VM-HA を Ansible + 金庫で構築（1〜2台は段0-minimal）。
- **検証:** fake Provider（主戦場）、containerlab、Podman、nftables、Proxmox nesting、chaos テスト（最重要）。

## 実装スコープ（§27）

- **v1a（コア実証 ≈10人月）:** コア型+収束ループ+安全機構（累積予算・List 完全性）+早期収束 seam+BulkObserve/--claim 型切り+最小 SDK+Provider 2 個（DNS が第一号）+devenv。
- **v1b（brownfield・採番・分割）:** import/Sync Mode、tier 別 Observe 頻度、採番連携、静的シャーディング実装、inventory 検証。
- **v1.5:** 単純 Task+最小 Workflow（直列+不変条件+N-of-M 手動承認）+remediation schedule+planned_change。
- **v2+:** 多相 Task、自動段階ロールアウト/canary、DBSource、credential-broker、分散 Executor、動的ルーター、watch 併用、intent UI。
- **実装着手前に決めるべき事項（旧 §26 カテゴリ A）は全て決定済み**（正準型・rendered_revision 算出・S2 永続化先ほか）。実装中に決める残課題と既知の限界は §29。
- **位置づけ:** OSS 個人プロジェクトが第一歩（作者個人が開発。将来の業務利用はありうる）。ライセンス **Apache-2.0**。雇用主 IP 規程による着手制約はなし（§29.4 でクリア）。この前提により Provider SDK 公開 API 設計と段0-minimal が第一級に昇格。

## 検証の履歴（設計の信頼根拠）

20 の多様な構成（ホームラボ〜数百台 DC〜マルチサイト〜IaaS/SDN/PaaS/エッジ/金融/レガシー）に対する適用可能性検討を 5 ラウンド実施し、**v2.6 で収束を宣言**（2 ラウンド連続で新規設計ギャップゼロ）。最終判定: **◎6 / ○10 / △2 / ✗0**（△の 2 つは機器 API の制約と Sakutei 責務外の要素で、設計の到達上限として確定）。詳細は `docs/review/v2.6/README.md`。

## 作業時の指針

- ドキュメントはすべて日本語。追記・修正時も**日本語**で、既存の用語（英語機能名の component 名、庭園メタファの別名: 関守/台帳/図面/坪庭/縮景）を踏襲する。
- 設計に関する記述を変更するときは `docs/architecture/spec/` を正とする。旧版やサンプルとの矛盾は spec/ に寄せる。**改訂は分冊（part1〜8）を直接編集**し、`CHANGELOG-vX.Y.md` を `docs/architecture/` に追加して `spec/README.md` の版番号を上げる（版ごとのファイル複製はしない。履歴は git）。大規模再構成時のみ `spec/` 一式を `old/vX.Y/` にスナップショットする。
- `generated/` は自動生成専用（ヘッダ必須・直接コミット拒否）、`intent/` は人が書く intent の出力先（claim シャーディング時は `intent/<scope>/` 規約）。
- **Schema 設計が最難関**（secret/managed/ordered/identity の誤宣言は安全保証を黙って無効化する。identity は運用上の変更頻度で選ぶ）。
- コンポーネント名は機能的な英語名（reconciler / credential-broker / allocator / catalog）を使う。固有名詞は「Sakutei」のみ。
