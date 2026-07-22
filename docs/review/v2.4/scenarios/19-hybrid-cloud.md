# 対象構成19:ハイブリッドクラウド(オンプレ+AWS) — Sakutei適用可能性レビュー(v2.4再評価)

**新判定:オンプレ側=◎ 成立(v1a/v1bで到達可) / AWS側=△→○寄りの△(境界導入は○、常時Observe×AWSの構造衝突はBulkObserveで大幅緩和。ただし自前AWS provider群+broker STSの重心はv2に残る)**

**判定推移(v2.2 → v2.3 → v2.4):**
- オンプレ側:**○ 条件付き → ◎ 成立 → ◎ 成立(据置・早期収束で内実強化)**
- AWS側:**△ 大幅拡張要 → △(内実改善) → △(境界○・構造衝突の主因1本が構造的に解消)**

v2.4は「早期収束パス(§6.8)」と「スケールアウト経路3段(§5.7 BulkObserve / §12.4 静的シャーディング)」を第一級化した。本構成のAWS側△の主因は3本柱(①毎周全Observe×AWSレート/課金/CloudTrailノイズ、②自前AWS provider群+broker=v2、③Terraform対比の純増価値が薄い)だった。v2.4は**①を構造的に潰し**、③に**早期収束という新しい差別化軸**を与えた。②は不変で、これが依然AWS全面移行の支配要因である——ゆえに総合の△は据え置くが、記号の重みは「①が消えた△」へさらに軽くなった。

---

## 1. ギャップ解消表(CHG-ID対応)

| 前回(v2.3)ギャップ | v2.4該当変更(CHG-ID) | 判定 | 根拠 |
|---|---|---|---|
| **G1. 毎周全Observe×AWSレート/課金/CloudTrailノイズ**(v2.3ではtier別間引き=v1b前倒しで部分解消、本命watch=v2) | **CHG-F1(§5.7 BulkObserve)** | **解消(構造)** | AWS Describe*は「1コールでscope内全量を返す」バルクの代表格。per-resource前提の「340コール/周」が**BulkObserveで十数コール/周に畳める**(下§3試算)。tier別間引き(結果整合の取りこぼし窓を作る副作用付き・v2.3のN1)に頼らず、**全量を毎周見たまま**レート/ノイズ問題を解ける。「間引きなし=価値の源泉」(§6.2)を放棄せずに済む点が質的転換。**毎周全量再観測の原則は不変**(§5.7)で観測stateレスは無傷。 |
| **G2. Terraform対比の純増価値が薄い**(常時収束ループの価値がwatch=v2で削られる) | **CHG-E1(§6.8 早期収束パス)/ CHG-E2(§13.5 Watch)** | **部分解消(新差別化軸)** | 「PRマージ=ヒント発火→秒でAWS反映」(§6.8 platform層)が **terraform apply手動実行に対する新たな差別化**。ただしこの構成は全platform相当・SRE 5名運用で**tenant即時払い出しの主眼(§6.8の主客)が薄い**ため、恩恵は「マージから収束までのレイテンシ短縮」に限定。TerraformもCI連動applyで擬似的に追随できるため、差別化は「観測stateレス+早期収束+安全柵(§10.2/§8.6)を1系で持つ」統合価値に集約。単独の決め手にはならないが、G2は前進。 |
| **G3. AWSマネージド抽象との摩擦**(RDS内部収束器) | (v2.3で§11.4 external_ownerにより解消済・v2.4変更なし) | **解消(据置)** | v2.3のexternal_ownerがそのまま有効。v2.4で新規変更なし。 |
| **G4. 部分導入(境界のみ)の妥当性** | **CHG-Z1(§26.1 ID=(provider,scope,id)確定)+ CHG-F2(§12.4 claim-as-intent)** | **解消(強化)** | scopeがIDに織り込まれ再シャードで参照が壊れない(CHG-Z1)ため、「Route53だけSakutei/残AWSはTerraform」の**境界scopeを`--claim`でgit宣言的に固定**でき、境界の出し入れがPRレビュー対象に。前回「綺麗に切れる」がさらに「シャード割当ごと監査可能」へ。 |
| **G5. またぐ依存の順序保証** | **CHG-F4(§12.2 「シャード間協調は現実を介して行う」を原則格上げ)** | **解消(思想的裏づけ)** | オンプレDB←EC2、Route53←オンプレの「観測ready待ち」が**妥協でなく観測stateレスの必然的帰結**と明文化。シャード間RPC/状態交換は正しさに関与しないと確定。結論(強い順序はシャード内のみ)は不変だが、設計判断の拠り所が明確化。 |
| **N1(v2.3新規)tier間引き×結果整合の取りこぼし窓** | **CHG-F1(§5.7)が前提を無効化** | **解消** | BulkObserveで全量毎周が現実的になれば、そもそも間引きが不要になりconverging取りこぼし窓が生じない。v2.3で運用パラメータ送りだった懸念がv2.4で構造的に消える(間引きは天井に達した時の予備手段へ後退)。 |
| **N3(v2.3残存)broker STS=v2** | 変更なし(CHG-A2据置) | **未解消(構造的・不変)** | v1はSecretRef直結・長命IAMキー。AWS全面運用の攻撃面は残る。 |

## 2. 判定変更の理由

**オンプレ側 ◎維持(内実強化)**:v2.3で到達した◎は不変。v2.4のBulkObserve(§5.7)はvCenter PropertyCollector / Proxmox `/cluster/resources` を1コールで畳むため、VM 200が単一デーモン上限~200台(vCenter 0.8s律速・§12.2)だった天井が**数千〜1万規模へ拡張**され、「cluster軸シャーディング必須」だった予備条件すら不要になった。早期収束(§6.8)でオンプレの基幹DB周辺のPRマージ反映も秒化。◎の余裕が増した。

**AWS側 △維持(記号の重みは最軽量へ)**:△の据え置きは②「自前AWS provider群(aws_vpc/ec2/rds/sg/route53/s3…約7種)+broker STS」がv2に残る**実装コストの構造的不変**が支配的だから。しかし△の内部構成は決定的に変わった:v2.3で「即効薬(tier間引き)はv1b・本命(watch)はv2」という**中途半端さがG1の核**だったのに対し、v2.4は**BulkObserveでG1を根から断ち**(watchを待たずに全量毎周がレート内に収まる)、tier間引きの副作用も消した。前回推奨の分岐点だった「常時ObserveがAWSの現実と衝突」という**構造問題そのものが解消**された。残る△は純粋に「provider実装工数×5名SREの機会費用」の経済判断であり、技術的障壁ではなくなった。

## 3. BulkObserveによるAPIコール数/課金/レート再試算(v2.3 per-resource前提との比較)

**v2.3試算(per-resource)**:約340リソース(EC2 150+RDS 20+VPC/SG/Route53/S3多数)÷10秒周期 = **毎秒34コール・月間約8,800万リクエスト**。8VPC短周期でRequestLimitExceeded、CloudTrailがDescribeで埋まる。

**v2.4試算(BulkObserve・§5.7)**:各サービスはページ単位バルクで1〜数コール:
- EC2 `DescribeInstances`:150台を~100/ページ = **~2コール/周**(前回150)
- RDS `DescribeDBInstances`:20台 = **1コール/周**(前回20)
- VPC/Subnet/RTB/IGW `Describe*`:各1コール = **~4コール/周**(前回数十)
- SG `DescribeSecurityGroups`:**1コール/周**(前回SG数分)
- Route53 `ListResourceRecordSets`:zone単位 = **~2コール/周**(前回レコード数分)
- S3 `ListBuckets`+ポリシー:**~2コール/周**

合計 **約12〜15コール/周**。10秒周期なら**毎秒~1.3コール**(前回34の約1/25)、月間~350万リクエスト(前回8,800万)。

- **レート制限**:毎秒~1.3コールはEC2トークンバケットに対し桁違いに余裕。**RequestLimitExceeded起因の§5.3 transient→§6.6 backoff自己律速がほぼ消える**。前回「律速要因はレイテンシでなくAPIレート/トークンバケット」の核心そのものが、コール数1/25で無力化される。
- **課金/CloudTrailノイズ**:Describe自体は概ね無料(前回同様)だが、実害だったCloudTrailノイズ(Describeが本当の変更を埋める)が**コール数1/25=ノイズ1/25**へ直接縮小。SIEM/Athenaスキャンの従量コストも比例減。tier別間引き(v2.3の二次効果・副作用付き)に頼らずこれが得られる。
- **算術の帰結**:§12.2天井表の「高速REST 0.15s→30s間隔で~1,067台」に対し、AWS 340リソースは**BulkObserve列で「Describe*系1万規模へ」**に乗る。**「常時ObserveがAWSの現実と衝突」という構造問題は、レート・ノイズ・レイテンシの三面ともBulkObserveで解消**する。残る唯一の衝突源はページング途中の非一貫性(下§5)である。

## 4. 早期収束パスはTerraform対比の新差別化になるか(精査)

**なる(が本構成では限定的)**。§6.8は「PRマージ=ヒント発火→承認直後に収束(§6.8 platform層)」で、`terraform apply`の手動/CI実行に対し**制御プレーン追加レイテンシ秒オーダー**を約束する。新旧rendered desired同士のdiff(決定的・リアル非接触・§6.8)で影響サブDAGだけを収束するため、340リソース全Observeを待たずRoute53の1レコード変更を秒で反映できる。**FileSourceのwebhook/mergeフックによるWatchはv1a(§13.5・§24.1a項目12)**で、broker(v2)を待たずに使える点が実務的に大きい。

ただし本構成はSRE 5名・全platform相当で**tenantセルフサービスAPIの即時払い出し(§6.8の主客)が無い**ため、恩恵は「人がPRをマージしてから反映までの待ち時間短縮」に留まる。Terraform Cloud/Atlantis等もPR連動applyで秒〜分の追随ができるため、早期収束**単独では決め手にならない**。差別化の本体は「観測stateレス+早期収束+累積破壊予算(§10.2)+List完全性halt(§8.6)を**1つの収束系で統合**して持つ」点であり、Terraformが状態破損2回で起こした事故クラス(前回§5指摘)を殺す安全機構と早期反映が**同一系に同居**する統合価値にある。

## 5. BulkObserveのAWS固有の新懸念(結果整合・ページング途中の非一貫性 × §8.6)

**N-new1(新規・重要):Describeの結果整合とページング途中のスナップショット非一貫性**。AWS Describe*は多くが**結果整合**で、かつ**ページングの各ページが同一時点のスナップショットである保証がない**(ページ1取得中に新規EC2が起動しページ2に現れる/消える等)。BulkObserveは「1コール」と書くが実体は複数ページであり、v2.3のN1(tier間引き)とは別種の非一貫性が持ち込まれる。

これは**§8.6 List完全性検証との関係で二面性**を持つ:
- **Observe側(BulkObserve)**:非一貫でも§4.2の`Observation`は個別リソースの状態を返すだけで、多少の陳腐化は「converging/settle猶予(§10.4)」で吸収でき、次周で収束するため**致命的でない**(観測stateレスの利点)。
- **prune側(List)**:§5.7が明示する通り**BulkObserveとListは型を別に保つ**——ページング途中打ち切りやレプリカ遅延で欠けたIDが「理想に無い=削除」に直結する危険は、Observe高速化とは無関係にListの問題として残る。ここに§8.6が正しく効く:(a)ページング完走検証(next token=nilを確認、途中打ち切りはerror化)、(b)前回比−50%超の件数急減halt、(c)ListStatus三値(complete/partial/unknown)。**AWSのDescribe結果整合は§8.6(b)(c)が主防御**となり、BulkObserveがListを兼ねても安全性はList完全性検証に一元化される。設計はこの分離を§5.7で明示しており、**BulkObserveがprune安全を毀損しない構造**は保たれている。実装時は「BulkObserveの1コールを流用してList基数を得る場合、ListStatusをpartialに倒す規律」がv1b実装のポイント。

**N-new2(残存):external_ownerのObserveコストは1コールに縮小**。v2.3のN2(RDS除外してもDescribeはレート消費)は、BulkObserveで`DescribeDBInstances`が1コール/周になるため**ほぼ無害化**した。

**N-new3(据置・不変):broker STS=v2 / 分散Executor=v2**。AWSシャードを管理VPC内EC2に置きDX断時も自律収束(§12.3縮退)は依然v2。静的シャーディング(§12.4・v1b)で「オンプレシャード/AWSシャード」をclaim-as-intentで分ける型はv1aで切れるが、AWSシャードのRemoteExecutor実動作はv2。

## 6. この構成で成立させるための条件(更新版)

**オンプレ側(◎成立)**:
1. v1a(DNS/VM/FW providerで「触っていないのに戻る」実証)→ v1b(import→alert-only→scope昇格)。
2. 基幹DB/レイテンシ敏感VMは永久alert-only(§23.7)、観測不能資産は`ledger_only`(§23.9)。
3. **VM 200はBulkObserve(§5.7・vCenter PropertyCollector)で単一デーモン数千規模に収まる**——v2.3の~200台天井が拡張され、cluster軸シャーディングは不要に。
4. `destructive_budget`(§10.2)+List急減halt(§8.6)を初日から有効化。
5. 早期収束(§6.8・v1a FileSource Watch)でPRマージ反映を秒化(任意・UX強化)。

**AWS側(△・境界のみ○)**:
1. **全面移行はしない**が判断はより中立へ:Route53のオンプレ発レコードをDNS provider(v1a)でSakutei管理、残AWSはTerraform継続。境界scopeは`--claim`(§12.4)でgit宣言。
2. RDS/ASG等は`external_owner`(§11.4)で明示除外(Observe=可視化のみ、BulkObserveなら1コール)。
3. **AWSシャードを立てる場合、tier別間引きは不要**:BulkObserve(§5.7)で340リソースを~12〜15コール/周(毎秒~1.3コール)に畳み、全量毎周のままレート予算内に収める。間引きは天井到達時の予備手段へ後退。
4. **List完全性検証(§8.6)を必須**:AWS Describeの結果整合・ページング非一貫に対し、完走検証+件数急減halt+ListStatus三値でprune暴走を封鎖。BulkObserveをList流用する際はpartial規律を守る。
5. **本格全面運用はbroker STS(v2)+自前AWS provider群(約7種の実装工数)を待つ**。この実装コストが依然AWS全面移行の支配要因であり、v2.4はここを動かさない。それまでは「境界リソース+BulkObserveによる低コスト全量alert-only可視化」に留める——ただしv2.3より**可視化の運用コストが1/25**になった分、alert-only監視シャードとしての採用ハードルは実質下がった。
6. またぐ依存はseam分割:オンプレDBの`ready_condition`をAWSシャードがObserve gate、Route53はオンプレサービス実在Observe後Apply(§12.2 CHG-F4=観測stateレスの延長として確定)。
