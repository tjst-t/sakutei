# Sakutei 設計レビュー・適用可能性検討 総括

実施日: 2026-07-22。対象: `docs/design/sakutei-design-v2.2.md`（設計の正）。

2 部構成:
1. **設計レビュー**（3 観点: 整合性・安全性・実装可能性）
2. **20 構成での適用可能性検討**（ホームラボ〜数百台 DC〜マルチサイト〜IaaS/SDN/PaaS/SaaS 等。各構成は独立のアーキテクトエージェントが v2.2 全文を読んだ上で検討）

---

## 第 1 部: 設計レビュー

| レポート | 指摘件数 | 要点 |
| --- | --- | --- |
| [01-consistency-review.md](01-consistency-review.md) 整合性・完全性 | 高6 / 中8 / 低5 | `Provider` interface が §4.1 と §5.1 で食い違う。allocation・credential-broker が v2 なのに v1 の render/段0 が依存。§26 の「着手前に決める」リストにこれらの矛盾が漏れている |
| [02-safety-review.md](02-safety-review.md) 安全性・障害モード | 防御不十分 10 シナリオ | 最危険 3 つ: ①周期をまたいだ漸進的削除（rate limit が瞬間量しか見ない）②List サイレント欠損による誤 prune（unknown≠absent は Read の防御で List には効かない）③段0 の Ceph SPOF とコールドスタート順序未定義 |
| [03-feasibility-review.md](03-feasibility-review.md) 実装可能性・運用性 | 高6 / 中7 / 低4 | v1 全部で約 22.5 人月 → コア実証 (v1a) 約 10〜11 人月への分割を推奨。スケール算術（10k リソース/125 秒）は高速 REST 限定で、実機レイテンシでは天井が 1 桁下がる |

## 第 2 部: 20 構成の適用可能性 — 判定一覧

判定: ◎成立 / ○条件付き成立 / △大幅な拡張が必要 / ✗不成立

| # | 構成 | 規模感 | 判定 | 一言 |
| --- | --- | --- | --- | --- |
| 01 | [ホームラボ](scenarios/01-homelab.md) | 1 ノード | △ | 段0 が 3 ノード+Vault 前提で物理的に衝突。単一ノード段0 クラスが未定義 |
| 02 | [スモールオフィス](scenarios/02-small-office.md) | 2 台+NAS | ✗（可視化なら△） | 段0 が構成より重い。主要機器が最小クラス Provider に落ち中核価値が縮退 |
| 03 | [中堅企業仮想化](scenarios/03-midsize-virtualization.md) | Proxmox 3 ノード, VM100 | ○ | 段0 と同型で正面から収まる。Task/Workflow の v2 送りで手続きが半分手動 |
| 04 | [大規模 vSphere DC](scenarios/04-enterprise-vsphere.md) | 300 ホスト, 8000VM | △ | v1 単一デーモン天井を超過（sharding は v2）。ITIL/CAB との思想差 |
| 05 | [Leaf-Spine SDN DC](scenarios/05-sdn-leafspine.md) | スイッチ 100 台 | ○ | 設計と一対一で写像が最も素直。SONiC の commit-confirm 欠如と管理系独立が条件 |
| 06 | [マルチサイト+DR](scenarios/06-multisite-dr.md) | 3DC, VM600 | △ | DR 中核（sharding/分散 Executor/S2 複製）が全て v2+。git 単一サイト配置が §19.4 違反 |
| 07 | [パブリック IaaS](scenarios/07-public-iaas.md) | 2000 テナント, 10kVM | △ | 設計が最も想定したユースケースだが必須要素（DBSource/sharding/UI）が全て v2+。「上位基盤へ委譲」した先が事業の本体 |
| 08 | [OpenStack 管理](scenarios/08-openstack.md) | 100 ノード | (a)○ / (b)△ | アンダークラウドのドリフト検知は本命。オーバークラウドは Heat と正面衝突 |
| 09 | [マネージド k8s/PaaS](scenarios/09-managed-k8s-paas.md) | 50 クラスタ | △ | 殻のライフサイクルは素直だが商品の核（Workflow/canary）が全て v2+ |
| 10 | [SaaS 事業者基盤](scenarios/10-saas-operator.md) | k8s3+データ層 | ○ | ArgoCD との境界が明快。MHA と「縁切り」はできるが「協調」の seam が未定義 |
| 11 | [DBaaS](scenarios/11-dbaas.md) | MySQL 800 | ○ | worked example のスケール版として高適合。フェイルオーバー/PITR は v2 の Task/Workflow 待ち |
| 12 | [GPU/AI 基盤](scenarios/12-gpu-ai.md) | 128 台 H100 | ○ | ファーム更新=多相 Task が設計の中心と好適。ファブリック単位 invariant の語彙不足 |
| 13 | [HPC](scenarios/13-hpc.md) | 500 ノード | ○ | image@sha256×ステートレス文化が level-triggered と等価。主戦場を管理系に絞る条件 |
| 14 | [エッジ小売 300 拠点](scenarios/14-edge-retail.md) | 300 サイト | △ | WAN 断定常には分散 Executor+lease 縮退（v2）が必須。店舗への資格供給が設計ギャップ |
| 15 | [通信 NFV](scenarios/15-telco-nfv.md) | 2 サイト 120 ノード | △ | 保守窓のみ auto という時間帯連動 remediation schedule が設計に存在しない |
| 16 | [CDN/PoP](scenarios/16-cdn-pop.md) | 40 PoP | ○ | 版数統一/証明書配布は中核ドメイン。段階ロールアウトと anycast 撤退判定 gate が条件 |
| 17 | [金融高規制](scenarios/17-regulated-finance.md) | VM400, エアギャップ | ○ | auto を封じても「ドリフト検出器+適合レポート+twin による 4 面一致証明」で成立 |
| 18 | [レガシー DC](scenarios/18-legacy-brownfield.md) | 15 年物混在 | 可視化○ / 収束△ | §23 がこの構成のための処方箋。観測不能機器の「階級」が型として未定義 |
| 19 | [ハイブリッド (AWS)](scenarios/19-hybrid-cloud.md) | オンプレ+AWS | オンプレ○ / AWS△ | 常時 Observe が AWS レート制限/課金と衝突。オンプレ=Sakutei / AWS=Terraform 継続が最合理 |
| 20 | [ベアメタルホスティング](scenarios/20-baremetal-hosting.md) | 1000 台 | ○ | 誤ワイプへの多層防御が構造的に効く。provisioner が設計書で名前のみ |

集計: ○=9、△=8、✗=1、分割判定=2（08, 18, 19 は部分◯）。◎は 0。

## 横断的な発見（20 構成 + 3 レビューに共通するテーマ）

### 1. 最大の問題は設計ではなく v1/v2 の切り方
20 構成中 12 で「その構成の事業の核」が v2+ 機能（**Task/Workflow・承認ゲート・段階ロールアウト・sharding・分散 Executor・DBSource・credential-broker・allocator**）に依存する。設計自体は写像可能でも「v1 では商品にならない」が最頻の△理由。特に **Task/Workflow はほぼ全構成で必要**で、v2 送りの再考価値が最も高い。逆に §24 の v1 スコープは実装可能性レビューで「MVP として過大（22.5 人月）」とも指摘されており、**「v1 を薄くしつつ Task/Workflow の最小形を前倒す」再スライスが最重要の示唆**。

### 2. 段0 に縮退形がない
1〜2 台構成（01, 02）は 3 ノード+Ceph+Vault-HA 前提の段0 と物理的に衝突して不成立。「段0-minimal」（単一ノード、file ベース secret 等）の定義があれば小規模帯が開ける。大規模側でも段0 の Ceph SPOF・コールドスタート順序未定義が安全性レビューで最危険級。

### 3. 「収束機構の上の収束」の境界宣言が第一級概念として不足
UniFi(02)、DRS/ACI(04)、Heat/Neutron(08)、k8s operator(09)、MHA(10)、VNFM(15)、AWS マネージド(19)——converging 3-tier と managed fields で「縁切り」はできるが、**「外部オーケストレータが所有する scope を管理境界から除外する」明示概念と「協調」の seam が設計にない**。二重管理事故の主因になりうる。

### 4. remediation mode に時間軸がない
保守窓文化（03, 15, 17）で共通して「時間帯連動の mode 切替（`remediation.schedule`）」が要求される。現設計は手動 freeze/unfreeze しかなく、SLA 環境ではヒューマンエラー面が残る。

### 5. 安全機構の残穴（安全性レビューと構成検討の双方で確認）
- rate limit が周期瞬間量のみで**累積削除量の時間窓監視がない**（漸進的全削除が通る）
- **List の部分欠損**（ページング打ち切り等）を検証する機構がなく誤 prune に直結
- 観測不能・unknown 常態の資産を持つ環境（14, 18）で unknown≠absent の運用負荷が積み上がる

### 6. スケール算術は実機レイテンシで再計算が必要
「単一デーモン 数千〜1 万」は 200ms/Observe の高速 REST 前提。vCenter 0.8s / NETCONF 1.5s / CLI 4s / Ansible 6s では実用上限が数十〜数百台に低下（03 実測圏、04 は明確に超過）。Observe 間引き（v2）が実は v1 の天井計算に必要という緊張がある。

### 7. 設計のスイートスポット（○が集中した帯）
**数十〜数千リソース・platform ownership 中心・テンプレート化された均質構成・git/PR 文化が受容される環境**: 中堅仮想化(03)、SDN ファブリック(05)、SaaS 基盤(10)、DBaaS(11)、GPU(12)、HPC(13)、CDN(16)、ベアメタル(20)。また **alert-only のドリフト検出器・適合レポート生成器としての価値はほぼ全構成で成立**し（17, 18 が典型）、ブラウンフィールド戦略 §23 は最も完成度の高い章として一致して評価された。

## 設計へのフィードバック上位（優先度順）

1. **v1 スコープの再スライス**: コア実証(≈10 人月)と Task/Workflow 最小形の前倒し、DBSource/sharding は v2 のまま
2. **§26 カテゴリ A への追加**: Provider interface の統一（§4.1 vs §5.1）、allocation/credential-broker の v1 依存解消、rendered_revision 算出定義、S2 永続化先
3. **段0-minimal（1〜2 台縮退形）の定義**
4. **rate limit への累積時間窓 + List 完全性検証（件数急減 halt 等）の追加**
5. **外部オーケストレータ所有 scope の除外宣言と remediation schedule の導入**
6. **スケール節の実機レイテンシによる書き直し**
