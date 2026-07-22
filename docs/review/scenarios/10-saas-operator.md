# 適用性レビュー 10: SaaS事業者の自社サービス基盤

**総合判定: ○ 条件付き成立**
— k8s より下(ベアメタル/Proxmox/NW/FW)と k8s 外のデータ層(MySQL/Redis/ES/Kafka)という守備範囲は、v2.2 のコアと Brownfield 戦略(§23)にほぼ素直に載る。ArgoCD との境界は「k8s 内=ArgoCD / k8s の土台と外=Sakutei」で明快に引ける。**条件は2つ**:(1) **MHA というもう一つの自動機構との共存**を managed fields + alert-only + `Capabilities.Converging` で正しく縁切りできること、(2) **クラスタメンバーシップ(quorum を持つ ES/Kafka)を Workflow(v2)まで待てること**。データ層の事故=事業停止という性質上、v1 の Task/Workflow 不在が最大の制約になる。

---

## 1. 構成の前提整理

- **k8s 3クラスタ 60ノード**: アプリは ArgoCD で GitOps 済み → **Sakutei のスコープ外**(§3.2「k8s はアプリ層のデプロイ・スケールに委譲」)。Sakutei が持つのは k8s ノードの**土台**(ベアメタル/Proxmox VM・OS・NW・FW・LB)と k8s の**外**(データ層 VM)。
- **データ層(k8s 外・VM 直)**: MySQL 20台(主従+MHA)、Redis 6台、ES 5台、Kafka 5台。ここが Sakutei の主戦場かつ最大の難所。
- **基盤**: ベアメタル30 + Proxmox 40、Cisco SW 12、FW 2ペア。§20/§11 の適用ドメインにそのまま対応。
- **運用実態**: アプリ1日20回(ArgoCD)、インフラ変更 週数回。SRE 8名が Terraform+Ansible 熟練。
- **規模感**: リソース総数は数千オーダー(VM 数百 + NW/FW/データ層設定)。§12.2 の「〜数千=単一デーモン・毎周全 Observe」帯に収まり、シャーディングは当面不要。既に GitOps 文化があるため §23.5 の「組織の受容」ハードルは他構成より低い。

## 2. リソースモデリング

「産む/焼く/join する瞬間=Task/Workflow、それ以外=Resource」(§20.2)を軸に写像する。

- **Resource(収束)**: Proxmox VM 定義、ベアメタル電源/BMC、Cisco の VLAN/L3/LAG(順序付き+commit-confirm・§8.5/§11.2-5)、FW ルール(順序付きコレクション)、LB プール、DNS、各データ層ノードの OS 層(パッケージ/サービス/`my.cnf`・`redis.conf`・`elasticsearch.yml`=§20.1 層1)、observability 設定(§17.2)、バックアップ構成(§18)。
- **Task**: golden image ビルド、VM の再起動を伴う変更、ベアメタルへの Proxmox 導入(多相 Task)、MySQL/ES の minor バージョン更新。
- **Workflow**: k8s ノード群のローリング更新の土台側、**ES/Kafka のクラスタ join(invariant=quorum 維持)**、**MySQL の計画的 failover**、DR。← いずれも v2 プリミティブ。

### 必要 Provider 一覧

| Provider名 | 対象 | Capabilities (TrueObs/List/Converging/CommitConfirm) | クラス | identity 設計 | 実装難度 |
|---|---|---|---|---|---|
| `proxmox_vm` | VM 定義・電源・スペック | ✓ / ✓ / ✓ / ✗ | 完全 | `vmid`(allocator)。current_node は managed | 中(§examples に実装例) |
| `baremetal` | 電源/BMC(Redfish/IPMI) | ✓ / ✓ / ✗ / ✗ | 完全 | 物理トポロジ座標(`leaf03-p12`・§5.5)。serial は managed | 中 |
| `cisco_switch` | VLAN/L3/LAG | △ / ✓ / ✗ / **✓** | 標準+CC | ポート/インタフェース名 | 中〜高 |
| `firewall` | FW ルール(順序付き) | ✓ / ✓ / ✗ / ✓ | 完全 | ルールリスト全体が原子単位(§8.5) | 高 |
| `mysql_node` | OS層+my.cnf+role状態 | ✓ / ✓ / **△** / ✗ | 標準 | ホスト名。**replication role は managed**(下記) | 高 |
| `redis_node` | OS層+redis.conf | ✓ / ✓ / ✗ / ✗ | 標準 | ホスト名。master/replica は managed | 中 |
| `es_node` | OS層+elasticsearch.yml | ✓ / △ / ✗ / ✗ | 標準 | node.name。**cluster membership は Task/Workflow** | 高 |
| `kafka_broker` | OS層+server.properties | ✓ / △ / ✗ / ✗ | 標準 | broker.id。**ISR/membership は Workflow** | 高 |
| `lb_pool` | LB プール/メンバー | ✓ / ✓ / ✗ / ✗ | 完全 | プール名 | 中 |
| `dns` | DNS レコード | ✓ / ✓ / ✗ / ✗ | 完全 | zone+name(§24.4 の一歩目) | 低 |

**注**: Cisco/FW は自前 provider を書く価値がある(横断項目=順序付き/commit-confirm/管理断が集中する層)。データ層 provider は Diff 対象を**OS層(層1)に限定**し、レプリケーション状態やクラスタ membership は下記の通り managed / Task 側へ逃がすのが肝。

### MySQL レプリカセット(MHA が主従を勝手に入れ替える = managed field)

これは §11.2-4(managed fields)と §11.2-1(収束器の上の収束器)の合わせ技で解ける。**MHA は「もう一つの収束/自動機構」**であり、DRS が vMotion で `current_node` を動かすのと構造的に同型:

- `mysql_node` の **replication role(master/replica)・現在の master ホスト・read_only フラグ**は **`managed: true` + `computed`** とし、**Diff 対象から除外**する。理想状態に「node-A が master」と書かない(書くと MHA が failover するたびに Sakutei が「元に戻す」誤動作=綱引き=事業停止事故)。
- Sakutei が収束するのは**MHA に依存しない土台のみ**: MySQL パッケージ版・`my.cnf` の静的パラメータ・ユーザ/権限(§20.1 層2)・レプリケーション**が張られていること自体**(トポロジの向きではなく「replication healthy」という述語)。
- **failover 中は `Converging`**: MHA が failover 実行中の状態を `mysql_node` が `Observation.Converging` として返せれば理想。だが MHA に「作業中」API が無いなら **Tier2 自傷猶予**(§10.4・role 変化を検知したら settle 窓)+ **Tier3 振動隔離**(綱引きを自動 alert-only 化)で床を張る。
- **VIP/接続先の切替**は MHA スクリプトの責務であり Sakutei は触らない(managed 扱い)。

### Kafka/ES クラスタのメンバーシップ

MySQL の「主従」と違い、ES/Kafka は **quorum を持つメンバーシップ**であり性質が異なる:

- **membership を Resource で収束させてはいけない**。ノード追加/削除は経路依存(shard rebalance・ISR 再構成)で、毎周ループに載せると quorum を割る破壊が起きる。→ **join/leave は Workflow**(invariant=`min_active`/`quorum: maintained`・§9.2)、v2。
- v1 では: 各ノードの **OS層・設定ファイル・プロセス起動を Resource** で収束し、**membership(現在の cluster 構成メンバー)は managed field として観測のみ**。「N台の broker が存在し設定が正しい」までが v1、「クラスタに安全に join する手続き」は v2 の Workflow を待つ。
- ES の shard 配置・Kafka の ISR は**それ自身が内部収束ループ**(§11.2-1)なので、Sakutei は intent を流す薄い層に徹し、途中状態を drift と誤認しない。

## 3. ownership・DesiredSource・sharding 設計(Sakutei と ArgoCD の境界線)

**境界線の明文化(最重要)**:

| 層 | 所有 | ツール | 供給源 |
|---|---|---|---|
| k8s 内(Deployment/Service/HPA/アプリ) | — | **ArgoCD** | Git(ArgoCD 管理) |
| k8s ノードの土台(VM/OS/kubelet 前提/NW/LB) | platform | **Sakutei** | FileSource(git・PR 必須) |
| k8s 外データ層(MySQL/Redis/ES/Kafka の VM・OS・設定) | platform | **Sakutei** | FileSource(git) |
| 物理/Proxmox/Cisco/FW | platform | **Sakutei** | FileSource(git) |

- **境界は「k8s API の中か外か」で引く**。ArgoCD は k8s オブジェクトを収束、Sakutei は「k8s クラスタが立つための土台」と「k8s の外の VM 直サービス」を収束。**同一リソースを二重管理しない**(§13.1 の混在境界原則)。両者の接点は**ノードの Ready 条件**: Sakutei が VM/OS を用意 →(kubeadm/Cluster API 相当は運用者領域)→ ArgoCD がアプリを載せる。Sakutei の `ready_condition`(§6.7)で「ノードが k8s join 済み」を下流の gate に使える。
- **ownership はほぼ全て `platform`**: このシナリオは単一事業者の自社基盤でテナント分割が薄い。したがって **書き込みは全て PR 必須・remediation は alert-only 寄り・rate limit 厳格**(§12.1 の platform 行)。マルチテナントの砂場分離(§12.1 tenant 行)は使わない。prod/staging/dev のクラスタ別 scope が自然な分割軸。
- **sharding は不要**(数千リソース=単一デーモン帯・§12.2)。将来増えても site/cluster 軸で水平分割でき、prod/staging/dev がそのままシャード境界になる。DBSource(tenant 層・即時書込)は使う場面がない。

## 4. render(inventory/allocation/secret)の使い方

- **inventory**: 既存の Ansible インベントリ/CMDB を **RenderedSource**(§13.1)の素材にできる。ただし v1 は FileSource のみなので、当面は inventory をファイル化(または import で生成)。ベアメタルの identity は**物理トポロジ座標**にして筐体交換で intent を書き換えない(§5.5)。
- **allocation**: VMID・VLAN・IP を allocator に払い出させる(§16)。既存の IP 管理(NetBox/phpIPAM/Excel)を `AllocationAuthority` の実装として差し込む。データ層 VM の増設で新規払い出しが走るが、**Release は非対称原則**(下流削除を Observe 確認後・cooldown・§16.4)で、使用中 IP の誤再割当=最悪級事故を防ぐ。
- **secret**: MySQL root / replication / ES/Kafka の認証を Vault に置き **SecretRef**(§14)。**値は収束しない**(§14.1)。rotation は `rotation: every 90d` を収束で検知 → 実行は Workflow(v2)。v1 の間は provider のターゲット認証も SecretRef 静的資格(§14.5 暫定経路)。**世代を render フェーズ1で解決**(§14.2)しないと「Vault で rotate したのに MySQL クライアントが古いまま=接続断」が起きる点に注意。

## 5. 安全機構の適用(データ層は事故=事業停止)

このシナリオの核心。データ層は §11.2 の地雷(volume 削除=データ消失)が集中する。

- **pruneable: false を厳守**(§8.3 既定)。MySQL/ES/Kafka の VM・volume は closed-world scope でも自動削除しない。削除は明示 opt-in + `final_backup: required`(§examples 2.5)+ Workflow 化。
- **remediation mode の使い分け**:
  - **alert-only(最保守)**: **MHA が触る全フィールド**(role/master/read_only)、ES shard 配置、Kafka ISR、稼働中データ層の破壊的になりうる設定。「収束器の上の収束器」に対しては alert-only が既定であるべき(§11.2-1 の綱引き回避)。
  - **auto**: DNS、observability 設定、LB プールメンバー、staging/dev のデータ層 OS 層、golden image。壊れても局所的で drift 自己修復の価値が出る層。
- **rate limit**: `on_exceed: halt-destructive-only`(本番)。データ層 scope は `halt-all` も検討。in-flight 込みで評価(§10.2)。「1コミットで MySQL 20台に破壊的変更」を物理的に止める装置が生命線。
- **capacity gate**: Proxmox クラスタ使用率で新規 VM 作成を止める(§10.2)。
- **freeze**: デプロイ1日20回のアプリ層は ArgoCD 管轄なので影響しないが、インフラ変更凍結窓を freeze(§10.6)で全体 alert-only に倒せる。

## 6. 段0ブートストラップの当てはめ

- **段0(§19.2)はほぼそのまま適用可能**: Proxmox 3ノード + 統合 Ceph + VM-HA に Vault / credential-broker / reconciler を載せる。既に Proxmox 40ノードの運用ノウハウがあるため段0の3ノードは容易。
- **段0/段1 の境界(§19.3)**: Cisco 12台のうち**管理NW到達性の最小核だけを段0**(Ansible 管理・Sakutei は触らない)、残り config は段1。管理NW機器は **commit-confirm 必須**。OOB/コンソールサーバを Break Glass L2.5 として整備。
- **役割分担が既存資産と一致**: 段0=Ansible(SRE が熟練)、段1以降=Sakutei。**既存 Ansible 資産の一部が段0にそのまま残る**のは移行上むしろ好都合。
- **git は外部**(既に GitHub/GitLab 運用ありと推定)。稼働必須依存にしない(§19.4)。

## 7. 成り立たない点・苦しい点・設計ギャップ・必要な拡張(忖度なし)

1. **【最大】v1 に Task/Workflow が無く、データ層の肝がまるごと v2 待ち**。MySQL 計画 failover・ES/Kafka の安全な join/leave・minor バージョンのローリング更新・DR フェイルオーバー・rotation 実行——**このシナリオでデータ層を「運用」するために本当に欲しい機能は全て v2 プリミティブ**(§24.2)。v1 の Sakutei はデータ層に対して「OS層 Resource 収束 + drift の alert」までしかできず、**MHA/クラスタ操作は依然 Ansible/手運用のまま**残る。v1 だけでは「データ層を Sakutei で管理」とは言い難く、**価値の大半は Task/Workflow 実装後**。

2. **MHA との共存は managed fields で「縁切り」できるが「協調」はできない**。Sakutei は MHA が動かすフィールドを無視するだけで、**MHA の failover を Sakutei がトリガ・観測・記録する統合は設計に無い**。failover 後に「新 master を前提とした下流(VIP・アプリ接続先・監視のプライマリ指定)」を Sakutei 側で追随する仕組みが要るが、MHA→Sakutei のイベント連携 seam が未定義。**MHA に「作業中」概念が無いため `Converging` の Tier1 が使えず、Tier2/3 の床頼み**で、failover 中の一瞬の drift 誤検知リスクが残る(alert-only なら破壊はしないが誤アラートは出る)。→ **拡張要求**: MHA マネージャの状態を Observe する薄い provider か、MHA の hook から Sakutei へ Event を送る接続。

3. **ES/Kafka の quorum を持つ membership は「Resource でも Task でもなく Workflow」で、v1 では観測しかできない**。join/leave の invariant 検証(§9.2)が無いと安全に扱えないため、v1 では membership 変更は手運用/Ansible のまま。**データ層 provider の「どこまでを Resource 収束し、どこからを managed/Workflow にするか」の切り分け基準が設計書に無い**(§20.1 は単体 DB の層1/2/3 は示すが、クラスタ membership の扱いは §examples の MySQL 単体例までしかカバーしていない)。→ **拡張要求**: 「クラスタメンバーシップ = 容量プールの member_of + Workflow join」の参照実装。

4. **Terraform+Ansible 資産からの移行パス**: これは §23 Brownfield が比較的よく効く。
   - **Terraform 資産**: TF が管理する VM/NW は **`sakutei import`(§23.3)で現実→intent 生成** → alert-only 観察 → scope 単位昇格(§23.4)。TF state との二重管理を避けるため、**scope ごとに「TF から Sakutei へ主権を移す」cutover** が要る。**TF の module 抽象を Sakutei の合成リソース(§7.5)に読み替える設計工数**が発生。TF import と違い state ズレの苦しみが無い(§23.3)のは利点。
   - **Ansible 資産**: playbook のモジュールは `AnsibleExecutor`(§21.2)で最小階級 provider として**再利用できる**が、**横断保証(原因タグ A/B・converging・破壊的重み付け)が縮退**(§21.3)。データ層の泥臭い設定は当面 Ansible ラップで広くカバーし、Cisco/FW/proxmox は自前 provider で品質を上げる二層戦略。
   - **Sync Mode(§23.8)が移行期間の主力**: 数ヶ月の並行運用中、人手/Ansible/TF の変更を継続的に git へ反映。ES/Kafka/MHA のように「当面手運用が正常」な scope は Sync Mode に置き続けられる。**このシナリオは Sync Mode の想定ユースケースにほぼ完全一致**。

5. **k8s ノードの「土台」と ArgoCD の境界が実運用では滲む**。kubelet の設定・CNI・ノードの kernel パラメータは「土台(Sakutei)」だが、一部は k8s Operator/DaemonSet(ArgoCD 経由)が触る。**同一ノード内で「Sakutei が触るフィールド/ArgoCD が触るフィールド」を managed fields で分ける**必要があり、その境界定義は運用者の設計判断として残る(§19.3 の「同じ機器の中で段が分かれる」の k8s 版)。

6. **provider 実装工数が重い**。Cisco(12台・順序付き+commit-confirm)、FW(順序付き)、データ層4種の provider を書く必要があり、§5 の SDK があっても Schema 設計(managed/ordered/identity の正しさ=§5.5)がデータ層で特にシビア(managed の宣言ミス=MHA と永久に喧嘩)。

## 8. 総合判定

**○ 条件付き成立。**

**成立する理由**: (1) スコープ境界が明快——「k8s 内=ArgoCD、k8s の土台と外=Sakutei」で二重管理なく分割でき、v2.2 の委譲思想(§3.2)と完全整合。(2) 規模が単一デーモン帯で、既に GitOps 文化があり §23.5 の組織受容ハードルが低い。(3) MHA/DRS のような「もう一つの自動機構」との共存は managed fields + alert-only + converging という**既存の型で原理的に扱える**(新プリミティブ不要)。(4) Brownfield 戦略(import/Sync Mode/scope 昇格)がこのシナリオの移行に素直に効く。

**条件**:
- **A. Task/Workflow(v2)まで待てること**。v1 だけではデータ層は「OS層収束 + drift alert」に留まり、failover/join/rotation/DR という運用の肝が手運用のまま残る。データ層を真に Sakutei 管理下に置くのは v2 以降。
- **B. MHA/ES/Kafka の自動機構と Sakutei の縁切りを Schema で厳密に(managed 宣言ミス=事業停止事故)**。加えて MHA→Sakutei の状態連携 seam(現状未定義)を参照実装で埋めること。
- **C. データ層 scope は alert-only + pruneable:false + halt を厳守**し、auto 昇格は「N日 diff ゼロ」を満たした土台/周辺(DNS/observability/LB/staging)から段階適用すること。

これらを満たせば、まず**土台層(物理/Proxmox/NW/FW)と k8s 外周辺で ◎ に近い成立**、データ層は **v2 待ちの ○** という二段構えで導入できる。「既存全部を管理下に」を目標にせず、価値が出る範囲から(§23.7)が正着。
