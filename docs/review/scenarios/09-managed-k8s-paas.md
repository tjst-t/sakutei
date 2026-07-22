# 対象構成09: マネージドKubernetes/PaaSプロバイダ — Sakutei適用可能性レビュー

**総合判定: △ 大幅な拡張が必要(条件付き成立の芽はある)**

Sakuteiは「k8sクラスタを産む/バージョンを上げる/削除する」というライフサイクルの外殻(Cluster API相当)は素直に載る。しかし本商品の価値中枢である**クラスタ内operator(PostgreSQL/Redis/Cilium/MetalLB)の収束**とSakuteiの収束が二重ループとして真正面から衝突し、境界設計を誤ると「収束器の上の収束器」(§11.2-1)が全クラスタ規模で発生する。v1スコープ(Task/Workflow/canary/シャーディングがv2以降)では中核機能が揃わず、実質v2待ちである。

---

## 1. 構成の前提整理

- 顧客専有k8sクラスタ50個(各3〜20ノード)、年率2倍成長。3年で200〜400クラスタ、ノード総数は数百〜数千。
- 基盤はProxmox 60ノード上のVM。CNI=Cilium、LB=MetalLB+上位BGP。これは設計書§11.3(容量プール)・§20.2(VM=Resource+多相Task+Workflow)がそのまま当てはまる領域。
- 商品はクラスタの**ライフサイクル**:作成/ノード増減/k8sローリングアップグレード/etcdバックアップ/証明書ローテーション/削除。これらは §9 の3プリミティブ(Resource/Task/Workflow)へ綺麗に写像できる形をしている。
- 「k8sアプリ層はk8sに委譲」(§3.2-C・§24.3)という設計原則に対し、本構成では**k8sクラスタそのものが管理対象**である点が要注意 —— 委譲先だったk8sを、今度はSakuteiが産み育てる立場になる。
- SRE 6名。バージョンアップは全クラスタ順次(canary的)。→ §10.5 段階ロールアウトの主要ユースケース。

---

## 2. リソースモデリング

### `k8s_cluster` 合成リソース(render フェーズ2 の展開先)

`k8s_cluster` を §7.5 の合成リソースとし、render フェーズ2 で下位を実在させる(子を単一Providerに隠蔽しない=§7.5の「採らない代替案」を守る。子50個の削除を1件に潰さないため)。

| 展開される下位リソース | プリミティブ | 根拠 |
|---|---|---|
| `proxmox_vm` ×(control-plane 3 + worker N) | Resource | §11.3 容量プール、example の proxmox_vm 完全階級 |
| ノードごとの CIDR / Pod CIDR | allocation(§16) | プール採番。冪等要求+割当事実の分離 |
| クラスタ ASN(BGP peering) | allocation(§16) | MetalLB↔上位BGP。ASN重複は事故 |
| `kubeadm_init` / `node_join` | 多相Task(§9.1)/Workflow(§20.2「join=Workflow・invariant=quorum」) | 経路依存・非冪等 |
| etcd バックアップジョブ | Resource(構成)+ canary(結果・§18) | 「取れているか≠戻せるか」 |
| 証明書(kube-apiserver/etcd/kubelet) | SecretRef + expiry health + rotation Workflow(§20.4) | 証明書はDAG上流 |
| CNI(Cilium)/ MetalLB address-pool | Resource(初期適用)/ **境界注意** | §7章の課題に直結 |
| PostgreSQL / Redis operator | **境界注意(§7参照)** | k8s内部収束との衝突点 |

### ローリングアップグレード = Workflow

§9.2 のWorkflowにそのまま乗る。全クラスタ規模では §10.5 段階ロールアウトと入れ子にする。

```yaml
workflow: k8s-node-rolling-upgrade
target: cluster-acme
invariant:
  min_active: "N-1"              # 常にN-1ノード稼働(不変条件)
  etcd_quorum: maintained        # control-planeは quorum 維持
  pdb_respected: true            # PodDisruptionBudget を破らない
strategy: { order: one-at-a-time, select: worker-first-then-cp }
steps:
  - cordon: "{node}"
  - drain: { node: "{node}", respect_pdb: true, timeout: 15m }  # PDBで止まったらstep失敗
  - wait: { condition: pods_evicted }
  - upgrade-kubelet: "{node}"     # 多相Task(§9.1)を内部で呼ぶ
  - health-check: { node: "{node}", condition: node_ready }
  - uncordon: "{node}"
  - verify: { invariant: min_active }
on_failure: { policy: halt, keep_drained: true }   # ローリングは全ロールバック困難
```

**invariant「PDB尊重」の実現に落とし穴がある**:drainがPDBで無限に待つのは正しいが、SakuteiはPDBの中身(どのPodが何個必要か)を知らない。ここは「drain APIが返すまで待つ+timeout」でk8sに委譲するのが正しく、Sakuteiが独自にPDB充足を判定してはならない(§4原則4「予測・配置は持たない」)。

### 必要 Provider 一覧

| Provider | 役割 | 階級(§5.4) | Capabilities |
|---|---|---|---|
| `proxmox_vm` | ノードVM(既存・example済) | 完全 | TrueObserve/List/Converging |
| `k8s_cluster`(合成) | 展開のみ(実体はrender F2) | — | — |
| `bgp_peer`(上位ルータ) | MetalLB↔BGP採番反映 | 標準〜完全 | 順序付きの可能性 |
| `k8s_object`(汎用CR/manifest) | operator/CRD適用 | 標準(List:condで真) | **§7の核心** |
| `etcd_backup` | バックアップ構成 | 標準 | — |
| `cert`(PKI) | 証明書存在・鮮度 | 標準 | expiry health |

---

## 3. ownership・DesiredSource・sharding

- **ownership**:クラスタの「殻」(VM群・CIDR・ASN・k8sバージョン)は `platform` 相当(SREが握る、PR必須、rate limit厳、alert-only寄り・§12.1)。顧客がクラスタ内に作るワークロードは委譲先k8sの領域でSakutei外。→ 本構成の管理対象はほぼ全てplatform寄りで、tenant即時払い出しの砂場モデルは薄い。
- **DesiredSource**:クラスタ定義はFileSource(git/PR)。ただしクラスタ生成の申込みは商品ポータル経由なので、v2の`DBSource`+上位プラットフォームで「承認・quota済みの`k8s_cluster` intentを投入」する形(§12.1・§13.1)が本命。v1のgit手書きでは50→200成長時に運用が破綻気味。
- **sharding(50→200)**:§12.2 の分割軸「cluster」がそのまま効く。1 Sakuteiシャードが N クラスタを担当し shared-nothing(§12.2)。**分割キー=顧客クラスタ**が自然な物理境界。Proxmox基盤(共有)は専用 platform シャードが1つ所有、クラスタシャードは read-only 参照。200クラスタ×各20ノード+operator群で数千〜1万リソースに達し、§12.2の実用上限(単一デーモン数千〜1万)を超えるため**シャーディングは成長の必然**。ただしシャーディングはv2以降(§24.2)。

---

## 4. render(inventory/allocation/secret)

- **CIDR/ASN採番**:§16 allocator の典型。クラスタごとにPod CIDR・Service CIDR・ノードIP・BGP ASNをプールから払い出す。冪等要求(「CIDRを1つ」)と割当事実(`cluster-acme → 10.244.0.0/16`)を分離(§16.1)。PR時は `reserve→confirm`(§16.3)でPR diffに具体CIDRが出る。CIDR重複・ASN重複はネットワーク最悪級事故なので§16.4 のRelease非対称原則(下流削除をObserve確認後のみ返却+cooldown)が生命線。
- **kubeconfig/証明書 と credential-broker**:ここが本構成特有の論点。二種類ある。
  - (a) **顧客/SREがクラスタを操作するkubeconfig** = クラスタの成果物であり、SecretRef(§14.5)としてVaultに格納しrotation Workflow対象。証明書はDAG上流(§20.4)。
  - (b) **Sakutei自身がクラスタAPIを叩く資格** = CredentialProvider経路(§14.5・§15)。作ったばかりのクラスタのadmin certで自クラスタを収束するのは**鶏卵**:クラスタ生成Task完了→kubeconfig生成→Vault格納→以後 credential-broker 経由でJIT取得。broker はv2以降のためv1は §14.5 暫定(SecretRef静的資格)で凌ぐ。broker断=全クラスタ全盲(§15.5)は本構成で特に痛い(50→200クラスタの目)。

---

## 5. 安全機構の適用

- **「k8sクラスタ削除」の危険性**:合成リソース削除は子(VM 20台+etcdデータ+顧客データ)を巻き込む。§7.5により子が独立Resourceなので「20件の削除」としてrate limit(§10.2)がhalt判定できる —— これは正しく効く。加えて `pruneable:false` 既定(§8.3)+ etcd/PV は破壊的種別として最終バックアップWorkflow(example §2.5と同型)を強制すべき。**顧客データを持つクラスタ削除は canary(§17.3 削除経路検証)と最も相性が良い一方、最も事故が怖い**。платформ層のPR必須+`halt-all`(§10.2)で守る。
- **canary的バージョンアップ**:「least-critical クラスタ→承認ゲート→broad」は §10.5 段階ロールアウトそのもの。ステージ間 `manual_approval`(§10.5)でSRE 6名が canary クラスタの health を確認して次段へ。**これはv2機能(§24.2 に承認ゲート/段階適用)**。v1では表現できない —— v1にあるのはrate limit+haltまでで、「1クラスタで検証→残りへ」の段階制御・自動ゲート・承認APIは無い。**v1では足りない。段階ロールアウトはv2の中核依存**。

---

## 6. 段0ブートストラップの当てはめ

§19の段0(Proxmox 3ノード+Ceph+VM-HAにVault/broker/reconciler)は**既存構成と親和的**(基盤が既にProxmox 60ノード)。段0の3ノードは本番60ノードとは別の最小核とし、そこからreconcilerが本番Proxmox・k8sクラスタ群を収束する。管理NW到達性の最小核(§19.3)はここでも段0。注意点:**「Sakuteiを動かすk8s」を段0に置きたい誘惑**が生じるが、設計は段0をProxmox+VMで固定しており、reconcilerは単一バイナリ・単一デーモン(§1.4)。管理対象のk8sと制御面のSakuteiを混ぜない(自分が管理するk8s上で自分を動かすと鶏卵が悪化)。この規律は守れる。

---

## 7. 成り立たない点・苦しい点・設計ギャップ(最重要)

**核心の問い:k8s内部operatorの収束と、Sakuteiの収束の境界線をどこに引くか。**

1. **二重ループの正面衝突(最大のギャップ)**。PostgreSQL operator / Redis operator / Cilium / MetalLB は**それ自身がk8s内で収束ループを回す**。Sakuteiがこれらのマニフェスト(CR)を `k8s_object` Resourceとして収束しようとすると、§11.2-1「収束器の上の収束器」が発生する。operatorがCRのstatusやspec.replicasを書き換える→Sakuteiが「drift」と誤認→綱引き。**境界の正解**:Sakuteiは「**operatorのデプロイ(Deployment/CRDの存在)と、顧客向けCRの"意図"(desired spec)まで**」を収束し、**operatorが管理するruntimeフィールド(status/実配置/自動フェイルオーバ結果)はmanaged fields扱い(§11.2-4)でdiff除外**する。これはexampleのcurrent_node/ip_addressと同型だが、**k8s CRのどのフィールドがdesiredでどれがoperator-managedかを、Provider Schemaで正しく宣言する作業が極めて重い**(§5.5「最難関はSchema設計」がCRD数だけ発生)。この線引きは設計書に明示ガイドが無く、**構成09固有の設計ギャップ**。

2. **`k8s_object` ProviderのList/prune網羅性**。k8s namespace内の全オブジェクトをListしてprune、は原理的に可能(§4.1)だが、operatorが動的生成するオブジェクト(Pod/EndpointSlice/operator派生リソース)を「理想に無い=削除」で消すと壊れる。→ **prune scopeを「Sakuteiが明示宣言したCRのみ」に厳格に限定**し、operator派生物には触れない境界宣言(§8.1)が必須。closed-worldの境界を「k8s namespace全体」にしてはならない。

3. **k8sの内部整合はk8s自身が持つ = Sakuteiの level-triggered と粒度が合わない**。k8sは自前でetcd合意・reconcileを持つ(§1.2 の表でk8s=内部etcd)。Sakuteiが毎周 kube-apiserver を叩いてObserveするのは、k8sが既にやっている収束の二度手間になりやすい。**苦しい点**:Sakuteiの価値(観測stateレス・毎周全Observe)が、クラスタ内部については薄まる。境界は「**クラスタの外形(存在・バージョン・ノード数・ネットワーク・証明書鮮度)まではSakutei、クラスタの内部整合はk8sに委譲**」が現実解。§17.3 canaryで「クラスタが機能しているか(API応答・Pod schedule可能か)」を能動検証する方が、内部を毎周収束するより筋が良い。

4. **v1スコープ不足(致命的ではないが商品化を阻む)**。ローリングアップグレード=Workflow、段階ロールアウト・承認ゲート、canary、シャーディング、DBSource、credential-broker が**すべてv2以降**(§24.2)。本構成の商品価値(バージョンアップ・スケール・マルチクラスタ)はこれらに乗るため、**v1のみでは実質「クラスタを作れるが商品として回せない」**。

5. **PDB/drainのタイムアウト設計**。invariantでPDBを尊重すると、顧客が壊れたPDB(minAvailable=全replica)を置いた場合、drainが永久にブロックしWorkflowがhalt(§9.2既定)する。**顧客起因でSREのアップグレードが止まる**運用リスク。timeout+強制evict判断は人(§10.5承認)に上げる設計が要る。

---

## 8. 総合判定

**判定: △ 大幅な拡張が必要**

**理由**:
- クラスタの「殻」のライフサイクル(VM群・採番・ローリング・証明書・削除安全)は §7.5/§9/§16/§10 に素直に写像でき、設計の骨格は本構成を吸収できる。
- しかし (a) **商品中核機能がv2以降に集中**(Workflow・段階ロールアウト・canary・シャーディング・DBSource・broker)、(b) **k8s内部operatorとの二重ループ境界に設計書の明示ガイドが無く、Schema設計負担が構成09固有に膨らむ**、(c) クラスタ内部については観測stateレスの旨味が薄まる —— の3点で「そのまま成立」とは言えない。

**○条件付き成立に格上げできる条件**:
1. 管理境界を「**クラスタ外形のみSakutei収束・内部整合はk8s/operatorに完全委譲・runtimeはmanaged fields除外**」と明文化し、`k8s_object` のprune scopeを明示CRのみに限定する境界宣言を設計に追加する。
2. v2(Workflow/段階ロールアウト/承認ゲート/canary/シャーディング/DBSource/credential-broker)まで実装が進む。v1のみでの本番商品化は不可。
3. クラスタの健全性検証をpoll収束でなく §17.3 canary(API疎通・Pod schedulability・削除経路)に寄せる。

上記が満たされれば「Cluster API + マルチクラスタ運用基盤」としてSakuteiは十分に成立しうる。現時点(v1近傍・境界ガイド未整備)では **△**。
