# 対象構成16:CDN/エッジPoPネットワーク — Sakutei 適用可能性レビュー

**総合判定:○ 条件付き成立**
テンプレート化されたPoPの日常運用(版数統一・証明書配布・設定drift修復)はSakuteiの中核と極めて相性が良い。ただし「anycast BGPを含むPoP追加/撤収」と「40拠点・高レイテンシ・無人・分散運用」という2点が、v1では未実装のWorkflow/シャーディング/分散Executorに全面依存し、そこに設計ギャップが残る。

---

## 1. 構成の前提整理

- 40PoP × (キャッシュ4〜10台 + エッジルータ2台 + スイッチ)。PoP内は完全テンプレート化(S/M/L)。
- anycast BGP:全PoPが同一プレフィックスを広告。PoP撤退=BGP撤回で自動迂回。**BGP広告状態が最重要の破壊面**(誤って全PoP撤回=全サービス断)。
- 日常運用=キャッシュソフト(Varnish/nginx)の設定・版数統一、TLS証明書配布・更新。これは典型的なdrift収束ドメイン(§20.2, §1.2の「Ansibleの冪等性+k8sのループ」がそのまま効く)。
- コンテンツ/ルーティング制御は別システム(トラフィック制御プレーン)の領分=**Sakuteiの管轄外**。これは§4「値・配置・予測・認可は持たない」の適用で綺麗に切れる。
- 無人・回線品質バラバラ(IX/トランジット)=高レイテンシ・分断が常態。運用6名がグローバル分散。

Sakuteiの「土台(インフラ)は収束、トラフィック制御は上位委譲」という境界が、この構成にほぼ理想的に一致する。

## 2. リソースモデリング

**PoP=合成リソースのテンプレート**(§7.5, §13.4)。`pop{size: M, region: apac}` を1つ書き、render フェーズ2 が下位を扇状DAGに展開する:キャッシュサーバN台(`bare_server`→OS→`cache_node`)、`edge_router`×2、`switch`、Varnish/nginx設定、証明書、BGPセッション。S/M/Lはテンプレートのサイズパラメータ。下位を独立Resourceに実在させることで、下位drift個別検知と爆発半径が保たれる(§7.5「単一Provider化しない」)。

**PoP追加/撤収=Workflow**(§9.2)。BGP広告の順序制約が不変条件になる:
```
invariant: bgp_advertise は cache_warm 完了 かつ health OK の後のみ
撤収: drain(BGP med下げ)→ traffic退避確認 → BGP撤回 → 収束停止 → 物理撤去
```
「キャッシュwarm前に広告してはいけない」は`ready_condition`(§6.7:`cache_warmed`)をBGP広告Resourceの依存gateに前置することで表現。撤収時は§16.4の非対称原則で、BGPセッション/ASN/IPの解放は**現実からの消滅をObserve確認してから**(誤解放=別PoPへ再割当=経路事故)。

**TLS証明書=SecretRef + expiry health + rotation Workflow**(§20.4, §14.4)。短命証明書の自動ローテーションは「鮮度=時間述語で収束が検知、更新手順=Workflow」。ACME等の発行は多相Task、配布はRead/Applyで各PoPへ収束。証明書はDAG上流(切れると全依存が倒れる)。

**必要Provider一覧**

| Provider | 階級 | List/prune | 備考 |
|---|---|---|---|
| bare_server(電源/BMC Redfish) | 標準〜完全 | 可 | identityは物理座標(§5.5) |
| cache_node(Varnish/nginx設定・版数) | 完全 | 可 | 日常運用の主戦場・closed-world |
| tls_cert(SecretRef供給) | 標準 | — | rotation Workflow連携 |
| edge_router BGP(BIRD/FRR/ベンダー) | 完全(要CommitConfirm) | 可 | 順序付きコレクション・commit-confirm必須 |
| switch(config) | 標準 | 可 | 順序付きACL |
| observability(Prometheus設定) | 標準 | 可 | health委譲(§17.2) |
| allocator(ASN/IP/prefix) | — | — | §16、撤収時Release非対称 |

BGP/ルータは§11.2-5の管理断ロックアウトと直結するため`Capabilities.CommitConfirm: true`を要求、ロングテール機器はAnsible provider(最小階級・§21.3)で吸収。

## 3. ownership・DesiredSource・sharding設計

- **ownership**:PoPインフラは基本`platform`(共有基盤・PR必須・alert-only寄り・rate limit厳)。テナント概念は薄い(コンテンツ制御は別システム)ので2層のうちtenant層はほぼ使わない。BGP/証明書/コア設定はすべてplatform。
- **DesiredSource**:PoPテンプレートと版数固定は`FileSource(git)`が正(§13.1、PR=変更承認=BGP事故の防波堤)。PoP inventory(どのPoPが実在するか)は`RenderedSource`でCMDB合成もあり得る。
- **sharding**:**site(PoP)を分割軸にした地理分散シャーディングが本命**(§12.2「地理分散=サイトごとシャード+薄いルーター」)。ただし「40PoP=40シャード」は過剰。推奨は**中央2リージョン(例:AMER/EMEA-APAC)にリージョンシャードを置き、各シャードが担当PoP群をshared-nothingで収束**。共有基盤(グローバルBGPポリシー・ルート集約)はplatformシャード1つが所有・書き込み、リージョンシャードはread-only参照(§12.2)。
- **高レイテンシPoPのObserve**:100ms超はObserve並列度16に対し最遅Observeがサイクル長を支配する(§6.2の早期警戒線)。シャードをPoP近傍/リージョンに寄せることで管理系RTTを縮められる。ただし分散Executor(§12.3)はv2以降であり、v1の単一デーモンから40PoPをまたぐと全PoPのObserveが1プロセスに集中する。

## 4. render(inventory/allocation/secret)の使い方

- **inventory合成**(フェーズ1):PoP一覧・各PoPの機器台数・回線メタ(IX/トランジット)を外部CMDBから合成し、S/M/Lテンプレートに流し込む。CMDB断時は前回生成物保持(§7.3、空desired=全PoP削除事故を防ぐ)。
- **allocation**(フェーズ1):ASN/loopback IP/anycast prefix/VMID相当をallocatorから払い出す(§16)。PoP追加のPR起票時は`Reserve`(TTL付き仮予約)でdiffに具体ID表示→マージ時`Confirm`、却下でリーク無し(§16.3)。撤収は`Release`を現実消滅Observe後+cooldownで(§16.4)。
- **secret世代解決**(フェーズ1):TLS証明書・BGP MD5/TCP-AOキーの世代をVault metadataから解決し入力ハッシュに合成(§14.2)。rotateすれば次周renderでdiffが立ち、配布が自動追従。値は書かない。
- **フェーズ2展開**:`pop{size}`→下位群。CIDR計算・tier別割当はテンプレートのカスタム関数で(§7.5)。

## 5. 安全機構の適用

**「全PoPのBGP設定を1commitで壊す」がこの構成の最悪事故**であり、§10・§19.5の全機構がそのまま生命線になる:
- **remediation mode**:BGP広告は最保守=`alert-only`寄り、または少なくとも段階ロールアウト必須。
- **rate limit + halt**:破壊的側(BGP撤回=delete相当)に重み付け。「同時に複数PoPの広告撤回」を`halt-destructive-only`で止める。in-flight込みで評価(§10.2)。
- **capacity/health gate**:撤退時「残PoPが残余トラフィックを捌けるか」をgate化(§10.2の容量gateの応用)。
- **段階ロールアウト(PoP 1→5→全部)**:これは必須要件。§10.5の`rollout.stages`(canary=least-critical PoP → broad)で表現でき、各ステージにhealth自動ゲート+manual_approval。**v1機能では段階ロールアウト(Workflow/承認ゲート)は未実装(v2以降)** ——ここが充足性の穴。v1でできるのはrate limit haltとfreezeまでで、「意図的に1PoPずつ」の構造化された段階適用はできない。
- **freeze**(§10.6):障害調査時に全PoPを一括alert-onlyへ倒すマクロは無人・分散運用で有効。

## 6. 段0ブートストラップの当てはめ

段0(Proxmox 3ノード+Ceph+VM-HA+Vault+broker+reconciler)は**中央リージョンに置く**(§19.2)。PoPは段1(収束対象)。ここで構成特有の緊張が出る:
- **管理NW到達性の最小核**(§19.3)が、40の地理分散PoPそれぞれに対して存在する。各PoPへの到達性(トランジット/IX経由の管理経路)が落ちると、そのPoPは§15.5の「broker断=全盲」と同じ状態になる。**OOB(L2.5)を各PoPに敷設するのは無人拠点では高コスト**で、遠隔PoPのBreak Glass経路が現実的に細い。
- reconciler→broker→管理NW→PoP機器の経路が長く、回線品質バラバラ。管理系断時は§15.5の緩和(TTLキャッシュ/unknown扱い/alert-only縮退)がそのまま効くのは設計通りだが、**復旧のための人手到達(OOB)が拠点ごとに用意しづらい**点が段0の当てはめで最も苦しい。

## 7. 成り立たない点・苦しい点・設計ギャップ・必要な拡張(忖度なし)

1. **段階ロールアウト=Workflow/承認ゲートがv2以降**(§24.2)。この構成では「PoP 1→5→全部」の段階BGP展開が**必須**なのに、v1にはWorkflowも承認ゲートも無い。v1のrate limit+haltは「暴発を止める」だけで「意図的に刻んで進める」を構造化できない。**PoP追加/撤収そのものがWorkflow前提**であり、v1コア(Resource収束)だけでは日常drift収束はできてもPoPライフサイクルは回らない。→ この構成はv2機能(Task/Workflow/承認ゲート/段階適用)が実質必須。

2. **分散Executor(§12.3)/シャーディング(§12.2)がv2以降**。40PoP・100ms超・無人・6名分散という規模と地理は、単一デーモンの単一リージョン集中収束では(a)最遅PoP Observeがサイクルを律速、(b)管理系RTTが長い、(c)リージョン制御面断で全PoP盲、という3点に効く。設計上シャーディングと段階縮退(分断時=加算のみ・破壊保留)は用意されているが**v1に無い**。この構成は分散Executorの「税金」を払わないと本番規模で成立しない。

3. **anycast BGPの「全体不変条件」がSakuteiのモデル境界を跨ぐ**。「最低N PoPが常に広告している」はWorkflow invariant(§9.2)で書けるが、**anycastの実効性(実際にトラフィックが健全に迂回したか)はSakuteiの管轄外**(トラフィック制御プレーン/監視の領分)。収束は「BGPセッションが広告状態か」まで、「迂回が本当に成立したか」はhealth/canary(§17)止まりで、能動的なトラフィック検証はcanary Workflow(v2)に依存。BGP撤退の安全判定を外部システムのシグナルにgateする連携設計が別途要る(設計に明示のフックが無い=ギャップ)。

4. **無人・遠隔PoPのOOB(Break Glass L2.5)が現実的に細い**(§6で既述)。設計は「OOBを管理対象外に置く」とするが、40無人拠点それぞれにコンソールサーバ/OOB網を敷く前提が暗黙にあり、コスト・運用の実在性が構成側で担保されていないと段0原則が崩れる。

5. **回線品質バラバラ=Observe早期警戒線の常時発火**(§6.2)。TTLキャッシュ/tier別頻度/watch併用(v2)が実質必須。v1の「毎周全Observe・間引きなし」は高レイテンシ多数拠点で破綻しやすい。

6. **PoPテンプレートのS/M/L=render フェーズ2テンプレートで表現可(v1あり)**だが、CIDR計算・ASN割当・tier別自動割当のロジックがテンプレートのカスタム関数に溜まる。§26.4のCUE移行トリガ(ロジック蓄積)を早期に踏む可能性が高い。

## 8. 総合判定

**○ 条件付き成立。**

**成立する根拠**:日常運用(Varnish/nginx版数統一・設定drift自動修復・TLS証明書の鮮度収束と配布)はSakuteiの中核ドメインそのもので、drift検知漏れゼロ・観測stateレス・PoP=合成リソーステンプレートが理想的に噛み合う。トラフィック制御を上位委譲する境界(§4)も構成に一致し、ownership=platform・FileSource+PR・site分割シャーディングという設計の既定路線に素直に乗る。

**条件(これが揃わなければ不成立に落ちる)**:
- (a) **v2機能(Task/Workflow/承認ゲート/段階適用)が実装済みであること**。PoP追加/撤収とBGP段階展開はWorkflow前提で、v1コアだけでは成立しない。
- (b) **シャーディング+分散Executor(v2)**で地理分散・高レイテンシ・リージョン制御面の局所化を実現すること。単一リージョン集中収束は本番規模で律速・全盲リスクを負う。
- (c) **BGP撤退の安全判定を外部トラフィック制御/監視のシグナルにgateする連携**を別途設計すること(anycast実効性はSakutei管轄外)。
- (d) **無人PoPのOOB/Break Glass経路**を構成側で現実的に用意すること。

日常運用だけを対象にするなら◎に近いが、PoPライフサイクルと40拠点規模を含めた「管理が成り立つか」を問う本設問では、v1では大幅な機能不足、v2を前提にして初めて成立するため**○(条件付き成立)**とする。
