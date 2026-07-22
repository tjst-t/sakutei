# 対象構成07:パブリックIaaSプロバイダ — Sakutei適用可能性レビュー

**総合判定:△ 大幅な拡張が必要**(v1では不成立。DBSource・シャーディング・採番の高スループット化という v2+ の未実装機能群がクリティカルパス上にあり、かつ quota/課金/認可を委譲した先の「上位テナント基盤」が実質もう一つの大規模開発になる)

---

## 1. 構成の前提整理

約2,000テナント・VM合計約10,000。VM作成は**即時・API駆動**、毎分数件〜ピーク毎秒数件。基盤は KVMホスト400台(20クラスタ)、Ceph 3クラスタ、EVPN/VXLANファブリック。テナントごとに VPC相当(VRF+VXLAN+SG+FWaaS+LBaaS+FIP)。課金・quota・テナント間分離が事業要件、SLA 99.95%、24/365有人運用。

Sakuteiは「観測stateレス・level-triggered・宣言→収束」のオンプレ収束エンジンであり、本構成は設計書が §12.1(2層所有権)・§13.1(DBSource=tenant層)・§16(allocator)・§7.5(合成リソース)で**まさに想定していた形**である。ただし tenant層の即時書込に必要な **DBSource は v2+ 送り**、シャーディング・分散Executorも v2+(§24.2)。この時間軸のギャップが本レビューの中心論点になる。

## 2. リソースモデリング

**写像の骨格は素直に決まる**。VM=Resource(managed fields + pruneable、§20.2)、VM作成・resize=Task(再起動を伴う変更、§20.2/DBaaS例2.4)、ホスト退役・クラスタjoin=Workflow(invariant=quorum/容量、§9.2/§11.3)。テナントVPCは §7.5 の合成リソースそのもの。

**必要Provider一覧:**

| Provider | 対象 | 階級(§5.4) | 主な地雷 |
|---|---|---|---|
| kvm_vm / libvirt | VM本体・電源・スペック | 完全(List/TrueObserve/Converging) | managed fields(live migration・動的IP) |
| ceph_volume | ブロックボリューム | 完全 | volume削除=データ消失(pruneable:false) |
| evpn_vrf / vxlan_segment | テナントVRF・VNI | 完全(NETCONF/gNMI) | 順序無・realizationラグ=converging |
| security_group / fw_rule | SG・FWaaS | 完全 | **順序付きコレクション**(§8.5・全置換) |
| lb_vip / lb_pool | LBaaS | 標準〜完全 | 相手が収束器(§11.2-1) |
| floating_ip | FIP割当 | 完全 | allocator払い出し・Release非対称原則 |
| dns / dhcp_scope | 基盤 | 完全 | tier:foundation |

**テナントVPC=合成リソース**:`tenant_vpc` をユーザ面とし、render フェーズ2で `evpn_vrf + vxlan_segment(VNI採番) + security_group + fw_rule + lb_vip + floating_ip + dhcp_scope` へ扇状展開する(§7.5)。**どこまで展開するかの設計判断**:下位を個別Resourceとして理想状態に実在させ、単一Providerに押し込まない(§7.5 の「採らない代替案」)——さもないと子50個の削除が rate limit を1件で通過する。ただし異種システム(EVPN機器/OVN/LB)を跨ぐため、原子性が要る下位群(VRF+VNI)は「1 waveで原子的に」指定する。

## 3. ownership・DesiredSource・sharding

platform層(20クラスタ・Ceph・EVPNコア・共有ストレージ)= FileSource(git)+ PR必須 + alert-only寄り + rate limit厳(§12.1)。tenant層(VM・VPC・FIP)= **DBSource + 即時書込 + auto + warn**。テナントの closed-world scope は自テナント所有分に限定され、prune が platform を消さない。DAGは扇状(platform=共通上流、テナント間エッジなし)。

**シャーディング戦略**:10,000VM は単一デーモンの実用上限(数千〜1万、§12.2)の天井そのもの。1万×200ms÷並列16 ≈ 125秒/周で、SLA 99.95% とピーク応答には遅すぎる。→ **tenantキーでシャード**。2,000テナントを N シャード(例:20シャード=各100テナント・500VM)にハッシュ分割し、各シャードは shared-nothing で独立収束(§12.2)。platform層は専用 platform シャードが1つ所有・変更し、tenantシャードは read-only参照(書き手が1つで綺麗)。所有権の論理境界=シャードの物理境界。

**shard router**:上位は司令塔でなくルーター。「どのテナント/リソースがどのシャードか」を引く薄い層で、割当表は **S2(外部永続化必須)**——喪失=誤配送(§6.5)。テナントは基本またがないのでルーティングは静的でよく、リバランス時のみ割当表更新+ドレインが要る。

**レイテンシがユーザ体験に与える影響(核心)**:API→DBSource→収束はポーリング型で、`render(TTLキャッシュ)→Observe→Diff→Apply起動`が挟まる。VM作成待ち時間 = reconcile間隔(数〜数十秒)+ 非同期Apply完了時間(VM clone は分単位、§6.4)。**level-triggeredは「即時性」の逆で、収束周期に律速される**。毎秒数件のピークは DBSource への書込は捌けても、**収束周期を跨いで初めて反映**されるため、ポータルは「作成受付→provisioning→ready」の非同期UXを前提に作る必要がある(OpenStack Nova と同様)。edge-triggered(watch併用・§12.2)を足さないと、周期短縮で Observe コストが跳ねる。ここは設計思想と即時要件の構造的緊張点。

## 4. render(inventory/allocation/secret)の使い方

**採番規模が桁違い**。VNI(2,000テナント各1〜数個)・VMID(10,000)・テナントIP(VPCごとCIDR + VM単位)・FIP(グローバルプールから数千)。allocatorは §16 の interface(Allocate冪等/Reserve/Confirm/Release)で吸収できるが、**負荷が問題**:tenant層は即時払い出し(仮予約不要、§16.3)なので、ピーク毎秒数件×(VMID+IP+場合によりFIP)= 毎秒十数回の read-write が単一 allocator に集中する。§16.5 は「最小実装(SQLite+アトミック払い出し)」を想定しており、**この規模ではボトルネック/SPOF になりうる**。NetBox/Infoblox 等の実運用IPAM、またはシャード対応 allocator が要る。

render は毎周走り、Vault metadata読み(シークレット数=テナント×リソース)+ authority Lookup(割当リソース数=1万超)が毎サイクルの外部呼び出しになる(§7.4)。TTLキャッシュで緩和するが、**1万リソースぶんの Lookup を毎周キャッシュ更新する負荷はシャード分割前提でないと成立しない**。Release の非対称原則(§16.4:下流の現実削除をObserve確認後・cooldown付き)は、FIP/VNI の即時再払い出し衝突(ネットワーク最悪級事故)を防ぐため必須で、これは設計に織り込み済み。

## 5. 安全機構の適用

**テナントAがテナントBに波及しない保証**は設計の芯が効く:(1) closed-world scope が自テナント所有分に限定(§12.1)、(2) DAGにテナント間エッジなし=独立並列(§12.1)、(3) shared-nothingシャードで物理的にも分離、(4) tenant層 rate limit は warn だが scope がテナント内に閉じるため爆発半径が小さい(§12.1)。テナントAの理想状態の誤記は自テナントscope内で halt される。

**platform層の変更の爆発半径は大**。EVPNコア/共有クラスタの変更は全テナントを全盲/全断しうる(§19.3 の DC規模ロックアウト)。→ platform=PR必須 + rate limit halt + `tier:foundation` + 管理NW機器は CommitConfirm必須(§19.3)。capacity gate(§10.2)で使用率超過時に新規VM作成を halt。これらは設計済みで、本構成にそのまま効く。**残る懸念**:1シャードのplatform変更が全tenantシャードのread-only参照先を動かすため、シャード間依存(§12.2「シャードをまたぐ依存=観測ready待ち=結果整合」)により、platform変更の反映が全テナントに結果整合で波及する。強い順序保証はシャード内に閉じたときのみ成立するので、テナント跨ぎのアトミックな整合は原理的に提供できない。

## 6. 段0ブートストラップの当てはめ

段0(Proxmox 3ノード+統合Ceph+VM-HA、§19.2)に Vault/credential-broker/reconciler を載せる形はそのまま当てはまる。ただし本番規模が大きいため段0で持つべきものが増える:**N台のシャードデーモン**(各シャードのreconciler)、**allocator(高可用構成)**、**DBSource のDB本体**(append-onlyログ+revision、§13.1)、**shard router の割当表ストア(S2)**。段0が "小プラットフォーム化" するのは設計が認めるところ(§19.4)だが、本構成では段0自体が中規模クラスタになり、Proxmox 3ノードでは不足しうる。管理NW到達性の最小核・OOB(§19.3)は400台規模で必須。git は外部・稼働必須依存にしない原則(§19.4)は維持できる。

## 7. 成り立たない点・苦しい点・設計ギャップ・必要な拡張(最重要・忖度なし)

- **G1. クリティカルパスが丸ごと v2+**:DBSource(tenant即時書込)・シャーディング・分散Executor・intent UI が全て v2以降(§24.2)。本構成の**必須要素が v1 に一つも無い**。設計書自身が「DBSourceはv2+送り」と明記しており、v1では即時テナント書込が実現できない=**本構成は v1 では不成立**。これは適用不能ではなく「未実装機能の集合」だが、量が大きく、実質 Sakutei の後半ロードマップ全体を作り切る話になる。

- **G2. 即時性 vs level-triggered の構造的緊張**:毎秒数件・即時ポータルという要求に対し、収束は周期律速で本質的に「結果整合・非同期」。watch併用ハイブリッド(§12.2)を足しても、Sakutei は "provisioning is eventually consistent" なシステムであり、**同期的な「作成APIが返った=VMがある」を提供しない**。OpenStack/クラウドAPIの非同期タスクモデルをポータル側に必ず被せる必要があり、そこは Sakutei の外。

- **G3. quota/課金/認可を「上位テナント基盤に委譲」した先が、もう一つの大規模開発になる**(最重要):設計は §2-4/§12.1 で quota・課金・認可・ポータルを上位プラットフォームに委譲し「core が小さく保たれる」と主張する。これは core にとっては正しい。**しかしパブリックIaaS事業では、その委譲先こそが事業の本体**である——2,000社のメータリング(使用量課金・レーティング・請求連携)、テナント別quota強制(VM数/vCPU/メモリ/FIP/帯域)、RBAC/マルチテナント認可、セルフサービスAPI+ポータル、SLA計測。これらは OpenStack でいう Keystone+Nova-quota+Ceilometer/CloudKitty+Horizon 相当で、**Sakuteiと同等かそれ以上の開発規模**。Sakuteiは「宣言的なインフラ収束」という下半分しか担わず、**上半分(事業ロジック)は丸ごと自作**になる。「委譲した」は「作らなくてよい」ではなく「別途フルスクラッチで作る」であり、**総開発量はむしろ増える**。この非対称を認識しないと工数見積が破綻する。

- **G4. quota強制の穴**:§12.1 は「authority(採番)と quota(容量)という事前に区切られた枠を通すことで即時のまま安全」とするが、**quota の実装主体は上位プラットフォーム**であり、DBSource に理想状態が書き込まれる**前**に quota チェックが完了している前提。Sakutei は quota超過を止める機構を持たない(capacity gate はクラスタ物理容量のみ)。テナントが quota を超える理想を DBSource に注入したら、Sakutei は忠実に収束してしまう。**quota は Sakutei の外の admission に完全依存**し、そこが甘いと事業が破綻する。

- **G5. スケールの実測未検証**:1万リソース×シャードは設計上の推定(§12.2)であり、EVPNファブリックの realization ラグ・Ceph API・LB API の rate limit が Observe/Apply 頻度で効く(§6.2 早期警戒線)。ピーク毎秒数件の Apply が in-flight で積み上がる(§6.4/§10.2)挙動は twin(§22.5)での負荷検証が必須。

- **G6. ロングテールProvider問題は軽い**:本構成は自前Provider中心(VM/Ceph/EVPN/LB)で Ansible ラップ依存が少なく、§21.3 の「保証が落ちる」問題は相対的に軽い。これは数少ない有利点。

**必要な拡張の総量**:DBSource + シャーディング + 分散Executor + 高スループット allocator(またはIPAM統合)+ watch併用 + intent UI + 上位プラットフォーム(quota/課金/認可/ポータル)全体。前段はSakuteiコアの v2ロードマップ、後段は Sakukei外の別プロダクト。

## 8. 総合判定

**△ 大幅な拡張が必要**(v1不成立)。

**理由**:リソースモデリング(§2)・テナント分離の安全機構(§5)・合成リソース設計(§7.5)・採番の非対称原則(§16.4)は本構成に「設計として」よく適合し、Sakuteiが想定した最有力ユースケースであることは確か。しかし**必須要素がすべて v2以降の未実装機能**(DBSource/シャーディング/分散Executor/intent UI)であり、v1では動かない。さらに、収束モデルは即時同期APIと構造的に相性が悪く(G2)、事業要件の中核である quota/課金/認可は Sakukei外の大規模開発として別途フルスクラッチが必要(G3/G4)で、「委譲」は工数を減らさない。

**成立条件**:(1) Sakukei v2ロードマップ(DBSource/シャーディング/allocator高可用化/watch)を完遂する、(2) 上位テナントプラットフォーム(quota admission・メータリング課金・認可・非同期ポータルAPI)を Sakukei と同規模で別途構築する、(3) twin で 1万リソース×ピーク負荷の収束周期・realizationラグ・API rate limit を実測検証する、(4) ポータルUXを非同期provisioning前提で設計する。この4条件を満たせば ○(条件付き成立)に到達しうるが、(2)の存在により Sakukei単体で本構成を「管理できる」と表現するのは誤り——Sakuteiは事業システムの下半分を担う部品である、が正確な位置づけ。
