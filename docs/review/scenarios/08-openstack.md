# 構成08:既存 OpenStack プライベートクラウドの管理 — Sakutei 適用可能性レビュー

**総合判定:(a) アンダークラウド管理 = ○ 条件付き成立 / (b) オーバークラウドリソース管理 = △ 大幅な拡張が必要**

> 一言:(a) は kolla-ansible 置換ではなく「設定ドリフト検知器」として Brownfield(§23)導入すれば強い。(b) は Nova/Neutron/Heat という「収束機構の上の収束」に真正面から衝突し、二重管理・Heat との役割衝突が構造的リスクになる。

---

## 1. 構成の前提整理

- OpenStack Yoga・100ノード(controller 3 / network 3 / compute 90 / Ceph 統合)。運用4名、年次アップグレードが最大イベント。
- 現状は kolla-ansible デプロイ(コンテナ化された OpenStack)+ 日々の設定ドリフト(誰かが controller の conf を直接触る)。
- Sakutei から見ると管理対象は2層に割れる:
  - **(a) アンダークラウド**=ノードの OS/パッケージ/kolla コンテナ/`nova.conf` 等の設定ファイル/systemd(または container)/MariaDB(Galera)/RabbitMQ。**Sakutei の本命ドメイン(§20.1「インフラとしての DB」・§11.1 VM内OS/ソフト)にほぼ一致**。
  - **(b) オーバークラウドリソース**=project/network/subnet/flavor/image/quota を OpenStack API 経由で管理。**「収束機構の上の収束」の極端ケース**(§11.2-1)。
- 重要な非対称:OpenStack 自体が Nova/Neutron/OVN/Heat という強い内部収束・オーケストレーションを持つ。Sakutei は §11.2-1「converging を drift と誤認するな」の最難ケースに当たる。

## 2. リソースモデリング

### (a) アンダークラウド

| 対象 | プリミティブ | 根拠 |
|---|---|---|
| ノード OS/パッケージ/カーネルパラメータ | Resource | §11.1(VM内OS/ソフト・○) |
| kolla コンテナ(image tag/起動状態) | Resource(コンテンツアドレス §20.3) | §20.2 |
| `nova.conf`/`neutron.conf` 等の設定ファイル | Resource(closed-world or open-world) | §20.1(conf は本命) |
| MariaDB/Galera・RabbitMQ の導入・cluster 設定 | Resource | §20.1 |
| DB スキーマ・`nova-manage db sync` | Task(単純型) | §9.1・§20.1(スキーマは収束外) |
| Galera 再同期・RabbitMQ クラスタ復旧 | Workflow(invariant=quorum) | §9.2 |
| **Yoga→次版アップグレード** | **Workflow(rolling + invariant)** | §9.2・§10.5 |

- **kolla-ansible との共存/置換**:全面置換は非推奨。kolla-ansible の Ansible モジュール(冪等な実行実体)を `AnsibleExecutor`(§21.2)として借り、実行モデル(playbook 一回 push)は捨て、Sakutei の収束ループに載せるのが設計思想と整合。ただし §21.3 の通り Ansible ラップは `TrueObserve:false` に縮退し、Diff 品質・原因タグ A/B・converging 検知が落ちる。**設定ファイルの drift 検知**という本ユースケースの核心は、自前 file/conf provider(TrueObserve:true)を書く価値が高い(ドリフト元は「conf 直接編集」なので、まさにここが強い)。
- **アップグレード=Workflow の設計**:controller→network→compute の順、`invariant: {api_available, rabbitmq_quorum, galera_synced, min_compute_active: N}`、`select: least-loaded-first`、各段に承認ゲート(§10.5)。DB マイグレーション相は多相 Task として内部呼び出し。**現状 kolla-ansible の `deploy`/`upgrade` playbook が担う手続きを、第一級の Workflow に構造化する**ことになる(概念は同型・§9.2 の主張どおり too much ではない)。

### (b) オーバークラウドリソース

| 対象 | プリミティブ | 根拠/懸念 |
|---|---|---|
| project/domain | Resource | Keystone API・冪等 |
| flavor | Resource(pruneable:false) | 既存 VM が参照。削除は参照カウント §8.4 |
| image(Glance) | Resource(コンテンツアドレス §20.3) | チェックサム ready 判定 §20.3 |
| network/subnet/router | Resource | **Neutron/OVN が内部収束 → converging §10.4** |
| quota | Resource | 上位プラットフォームの責務と衝突(§12.1) |
| セキュリティグループ(順序) | Resource(ordered §8.5) | FW ルール型 |

### 必要 Provider 一覧

| Provider | 層 | Capabilities(想定) | 実装方針 |
|---|---|---|---|
| `os_package`/`os_service`/`file` | (a) | TrueObserve:t / List:一部 | 自前(conf drift の核心) |
| `kolla_container` | (a) | TrueObserve:t / List:t | podman/docker API §11.1 |
| `galera`/`rabbitmq` | (a) | TrueObserve:t / Converging:一部 | 自前 or Ansible |
| `keystone_project`/`nova_flavor`/`glance_image` | (b) | TrueObserve:t / List:t | 自前(OpenStack API)or Ansible `os_*` |
| `neutron_network`/`neutron_router` | (b) | TrueObserve:t / **Converging:t 必須** | 自前(OVN realization を写像) |
| `nova_quota` | (b) | TrueObserve:t | 自前(ただし §7 参照) |

## 3. ownership・DesiredSource・sharding

- **ownership**:(a) アンダークラウド全体を `ownership: platform`(共有基盤・PR 必須・alert-only 寄り・rate limit 厳格 §12.1)。100ノードの中核なので当然 platform。(b) はテナントが consume する project/network/quota → `ownership: tenant`(即時・DBSource・砂場境界)。**この2層区別が本構成にきれいに乗る**のは設計の強み。
- **DesiredSource**:(a) = `FileSource(git)`+PR(kolla の `globals.yml`/`passwords.yml` 相当を intent 化)。(b) = 本来は上位テナントプラットフォームが `DBSource`/`RenderedSource` として投入(§12.1)。**ただし本現場は運用4名・上位ポータル未整備**の可能性が高く、(b) を誰が正として投入するかが未確定(§7 のギャップ)。
- **sharding**:100ノード×アンダークラウド全 conf/service/DB を Observe すると数千〜1万リソースになり、単一デーモンの実用上限(§12.2)に近い。**site/role でシャード分割**が現実的:controller シャード/compute シャード。ただし §12.2「共有基盤は platform シャード1つが所有」の原則上、controller 群は1シャードに集約する方が綺麗。compute 90ノードは role シャードで水平分割可。

## 4. render(inventory/allocation/secret)の使い方

- **inventory**:kolla の inventory(どのノードが何役か)を CMDB として `RenderedSource`(§7.1 フェーズ1)で合成。ノード追加/交換時に intent 側は不変(identity は物理トポロジ座標 §5.5、シリアルは managed)。
- **allocation**:(b) の tenant network の VLAN/VXLAN VNI・IP は allocator(§16)払い出しが素直に効く。ただし **OpenStack(Neutron)自身が VNI/IP を払い出す権威を既に持つ**ため、二重権威になる(§7 のギャップ)。allocator と Neutron のどちらが採番の正か決めないと §16.4 の Release 衝突がネットワーク最悪級事故になる。
- **secret**:kolla の `passwords.yml`(サービス間パスワード・DB root・RabbitMQ・Keystone admin)は §14 の SecretRef で Vault 化。**既存パスワードは §14.3「既存・手動登録」→ rotation 系に合流**が正しい移行。値は収束せず存在・鮮度・世代のみ(§14.1)。RabbitMQ/Galera のパスワード rotate は接続断を伴うため rotation Workflow(§14.4)。

## 5. 安全機構の適用(内部収束との衝突)

- **衝突条件**:Sakutei が Neutron/OVN の realization 途中(port binding 中・OVN southbound 反映中)を Observe して「desired と違う」と判定 → 再 Apply すると綱引き(§11.2-1)。Nova でも HA/live-migration が動かすインスタンス配置を drift 扱いすると永久に喧嘩(§11.2-4 managed fields)。
- **防ぎ方(converging 3-tier §10.4 の具体適用)**:
  - **Tier1 ネイティブ**:Neutron の provisioning status(`BUILD`/`ACTIVE`/`DOWN`)、OVN の `up`/realization state、Nova task_state(`spawning`/`migrating`)を `Capabilities.Converging=true` に写像。**これが (b) 成立の必要条件**。写像できない項目は…
  - **Tier2 自傷猶予**:Apply 直後 `settle: 30-60s` で不一致を drifted 扱いしない。
  - **Tier3 振動検知**:M 回再 Apply で Diff が縮まらなければ `oscillating` 判定 → 自動 alert-only 隔離。
  - **deadline**:realization が stuck した OVN を取りこぼさないよう timeout 必須。
- **managed fields**:instance の `host`/`hypervisor`(Nova scheduler/live-migration が動かす)・port の binding host・floating IP の動的割当は `managed:true` で diff 除外(§11.2-4)。これを誤ると 90 compute で恒常 drift。
- **rate limit/爆発半径**:(a) の platform 層 conf を1行間違えると 100ノードの nova.conf が一斉書換 → §10.2 の `halt-destructive-only`+ベースライン逸脱検知が生命線。アップグレード Workflow は §10.5 canary(1 compute)→ broad。

## 6. 段0ブートストラップの当てはめ

- **重要な区別**:このシナリオでは OpenStack 自体が「管理対象(段1)」であり、Sakutei の段0(Proxmox 3ノード+Vault+broker+reconciler §19.2)は **OpenStack の外に別途立てる**必要がある。既存 OpenStack を段0基盤に流用してはならない(自分が管理する対象の上に自分が乗ると鶏卵・§19.1)。
- **管理NW到達性の最小核**(§19.3):controller の API エンドポイント・SSH・BMC への到達性は段0。OpenStack のコントロールプレーン(Keystone/管理 API)を Sakutei が壊すと自分で直せなくなるので、**Keystone・RabbitMQ・Galera の「到達性の最小核」は §19.3 の managed fields 分割**を適用し、Sakutei の破壊的変更から守る(commit-confirm 相当が無いので platform+PR+rate limit で代替 §19.3-3)。
- **Break Glass**:OpenStack admin 資格(Keystone admin・DB root)は §15.6 のオフライン金庫に1つ残す。

## 7. 成り立たない点・苦しい点・設計ギャップ・必要な拡張(忖度なし)

1. **【最重要】(b) は Heat と役割が正面衝突する**。Heat(と Terraform/OpenStack provider)は既に「宣言的テンプレート→OpenStack リソース収束」を担う層。Sakutei が (b) をやると **Heat の上にもう一段の収束ループ**が乗り、同一リソース(network/router/stack)を2つの権威が管理する二重管理になる。§13.1「同一リソースを2つの供給源が持たない」原則に真っ向から反する。**Heat 管理下のリソースには触らない境界宣言**が必須で、これは現状の設計に無い拡張(「外部オーケストレータが所有する scope」の概念)。

2. **採番権威の二重化(allocator vs Neutron)**。Neutron が IPAM/VNI 払い出しの権威を内蔵している。Sakutei allocator を (b) に持ち込むと二重権威で §16.4 の Release 事故(使用中 IP の再払い出し=NW 最悪級)を招く。**(b) では allocator を使わず「Neutron が正、Sakutei は Neutron の払い出し結果を Observe するだけ」**とすべきで、§16 の「収束エンジンが1つくれと頼む」モデルは (b) には適用できない。設計ギャップというより「適用してはいけない領域」の明示が要る。

3. **quota の責務衝突**。quota は §12.1 で明確に「上位プラットフォーム(認可・課金)の責務・委譲先」とされている。Sakutei が (b) で quota を Resource 収束すると、認可・課金と結びついた quota を収束エンジンが上書きし、上位プラットフォームと綱引きする。**quota は (b) の管理対象から外す**のが設計と整合。

4. **(a) の Ansible ラップ縮退**。conf drift 検知が核心なのに、kolla-ansible モジュールを `AnsibleExecutor` で借りると TrueObserve:false → 原因タグ A/B(意図的変更か手動 drift か)が区別できず「誰が触ったか」が曖昧化(§21.3)。ドリフト元特定が本ユースケースの目的なので、**自前 conf/file provider(TrueObserve:true)を書かないと価値が半減**する。ここは実装コスト。

5. **OpenStack 内部収束の realization を全部写像できない**。§10.4 Tier1 は「ネイティブの作業中概念を写像」だが、OVN の分散 realization・Nova の cell/placement は状態が深く、全 API に「作業中」概念が揃っていない。Tier2/3 で床は張れるが、settle 窓の間の drift は見逃す/振動隔離が頻発する懸念。

6. **アップグレード Workflow の invariant 表現力**。OpenStack アップグレードは「N-1 互換(controller だけ先に上げて compute は旧版のまま動く)」という半端状態を長時間許容する。§9.2 の invariant(min_active/quorum)では「バージョン跨ぎ互換ウィンドウ」を素直に書けない。**バージョン制約付き invariant**の拡張が要る。

## 8. 総合判定

- **(a) アンダークラウド管理 = ○ 条件付き成立**
  Sakutei の本命ドメイン(conf/service/DB as Resource・§20.1)に一致し、「conf を直接触られる drift」を alert-only の drift 検知器として即座に価値化できる(§23.2/§23.7)。kolla-ansible は置換せず Brownfield(§23)で共存 → Sync Mode(§23.8)→ scope 昇格が定石。アップグレードは Workflow 化できる。**条件**:自前 conf provider を書く(Ansible 縮退回避)、段0を OpenStack 外に別建て、管理 API/DB の到達性最小核を managed fields で保護、invariant にバージョン制約拡張。

- **(b) オーバークラウドリソース管理 = △ 大幅な拡張が必要**
  network/project/flavor/image を Resource 化する枠組み自体は存在するが、**Heat との役割衝突・Neutron 採番権威の二重化・quota 責務衝突**という3つの構造問題が同時に立つ。これらは「converging 検知を実装すれば済む」レベルではなく、「**外部オーケストレータ(Heat)が所有する scope を Sakutei の管理境界から除外する第一級概念**」「(b) では allocator/quota を適用しない明示的制約」という設計拡張を要する。converging 3-tier の Tier1 写像(Neutron/OVN/Nova task_state)も (b) 成立の必要条件。**現状の設計をそのまま当てると二重管理事故を招くため △**。Heat 不使用・上位プラットフォーム不在の限定環境で、かつ採番/quota を Neutron/上位に委ねきる境界を引ければ ○ に近づく。
