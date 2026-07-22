# 対象構成19:ハイブリッドクラウド(オンプレ+AWS) — Sakutei適用可能性レビュー

**総合判定:オンプレ側=○ 条件付き成立 / AWS側=△ 大幅な拡張が必要**
（オンプレ側は本設計の主戦場でありbrownfield戦略§23がそのまま効く。AWS側は「AWS APIが最良ケースのProvider基盤を与える」一方で、常時Observeループと従量課金/レート制限/結果整合が構造的に衝突し、Terraformとの直接対決では「stateレスの優位」が実運用コストで相殺されやすい。両者をまたぐ依存は結果整合のready待ちで扱えるが、シャード横断の強い順序保証は原理的に出せない）

---

## 1. 構成の前提整理

オンプレ Proxmox 16ノード・VM 200(基幹DB/レイテンシ敏感/データ主権)、AWS は VPC 8・EC2 約150・RDS 20・Route53・S3、Direct Connect 2本 + S2S VPN。現状はAWS=Terraform(state破損2回・drift常態化)、オンプレ=Ansible。またぐ依存は「オンプレDB←EC2アプリ」「Route53←オンプレサービスのレコード」。SRE 5名。発端は「TerraformのstateレスがSakuteiの売りなら、AWS側もSakuteiで管理できないか」。

Sakuteiは §1.5 で**対象を明確にオンプレと宣言**しており、AWSは設計が想定していない「他人の庭」である。本レビューの核心は、AWSをProvider化(§5)することが設計原則(特に §6.2 毎周全Observe・§10 安全機構・§16 採番)と噛み合うかの検証である。オンプレ側は §23(brownfield)がそのまま効く既知の道であり、緊張はほぼ全てAWS側に集まる。

## 2. リソースモデリング

**AWS APIは実際にProvider階級「完全」(§5.4)を満たす最良ケースである**——検証結果は肯定寄り。DescribeXxx が current state を型で返す(`TrueObserve:true`)、`describe-instances`/`list-hosted-zones`等がscope網羅列挙を返す(`List:true`→prune/import/Sync Mode可)。多くのAPIに非同期状態(EC2 `pending`/`shutting-down`、RDS `modifying`/`backing-up`、Route53 `ChangeInfo.Status=PENDING`)があり **`Converging:true` を素直に写像できる**——ここはむしろオンプレの生NW機器より恵まれている。`CommitConfirm` はAWSに無い(false)。

**必要Provider一覧:**

| Provider | 対象 | 階級(§5.4) | 主な地雷 |
|---|---|---|---|
| aws_vpc / subnet / rtb / igw | VPC土台 | 完全 | tier:foundation・削除順序依存 |
| aws_ec2_instance | EC2本体 | 完全 | 従量Describe課金・停止/終了=破壊的・IMDS/AMI参照 |
| aws_rds_instance | RDS | 完全(Converging強い) | 削除=データ消失(pruneable:false)・`modifying`が長い |
| aws_security_group / sg_rule | SG | 完全 | **順序無だが参照グラフ**(SG間相互参照・§8.5とは別種) |
| aws_route53_record | DNSレコード | 完全 | **結果整合(伝播)=converging**・zone単位closed-world |
| aws_s3_bucket / policy | S3 | 完全 | バケットポリシー=順序付きJSON・削除ガード |
| eip / eni | IP/ENI | 完全 | allocator不要(AWS採番)・Release非対称原則 |

**設計との適合点2つ**:(a) Route53の**伝播ラグを converging として扱える**——`ChangeInfo` が INSYNC になるまで §10.4 Tier1、無い経路でも Tier2 自傷猶予(`settle`)で「Apply直後の不一致をdriftと誤認しない」が普遍的な床になる。(b) VPC=合成リソース(§7.5)としてsubnet/rtb/igw/natgwへ扇状展開でき、子を個別Resourceに実在させることで「VPC1削除=子20個削除」を rate limit が1件で通過する事故を防げる。

**設計とズレる点**:AWSの採番はEC2 instance-id/eni-id/eip等を**AWS側が払い出す**ため §16 allocator の出番がほぼ無い(identityはAWS採番IDをcomputedで受ける)。これは楽になる方向のズレ。逆にSGの相互参照は §8.5 の順序付きコレクションとは別種の「参照グラフの循環」であり、循環検出(§6.1・ロード時拒否)がAWSの実体(SG-A→SG-B→SG-A の正当な相互許可)を誤って弾かないよう、参照種別の区別が要る。

## 3. ownership・DesiredSource・sharding設計

**シャードはオンプレ/AWSで分ける**のが素直——理由は §12.2「所有権の論理境界=シャードの物理境界」「各シャードshared-nothingで独立収束」であり、到達経路(オンプレ=管理NW / AWS=IAM+HTTPS API)・Observe周期・課金モデル・爆発半径が全く異なるため、同一デーモンに混ぜる利点が無い。AWS側はさらに **VPC(dev/stg/prod×事業部)でサブシャード**が自然(§12.2 のsite/cluster軸をaccount/VPC軸に読み替え)。

**DesiredSource**:AWSもオンプレも FileSource(git)+ PR必須が妥当(§13.1)。ここは §07/§10 のようなテナント即時払い出しが無く、全て platform相当(共有基盤・人が変更・低頻度)なので **DBSource(v2+)を待つ必要がない**——これがオンプレ側○判定を支える大きな要素。drift常態化しているAWSは §23.5 の「alert-onlyで先に信頼を得る」がそのまま処方箋になる。

**またがる依存の扱い**:「EC2アプリ←オンプレDB」「Route53←オンプレサービス」はシャードをまたぐ。§12.2 の規律により**シャード内は plan順序gating、シャードをまたぐ依存は『観測ready待ち』(結果整合)**になる。具体的にはオンプレDBの `ready_condition`(§6.7)をAWSシャードがObserveでgate、逆にRoute53レコードはオンプレサービスの実在をObserve後にApply。**強い順序保証はシャード内に閉じたときのみ成立**し、オンプレ↔AWSのアトミックな整合は原理的に提供できない(これは制約であって欠陥ではない・結果整合で足りる依存だから許容)。

## 4. render(inventory/allocation/secret)の使い方

**allocation**:AWS側は §16 allocator の主対象(VLAN/IP/ASN採番)がほぼ無い。CIDRは事前設計・EIP/ENIはAWS採番なので、allocatorは**オンプレ側にのみ実質必要**。楽になるが、逆に「AWSが払い出したIDをどうgitの理想に安定して結びつけるか」が問題化する——EC2はimmutableでinstance-idが再作成で変わるため、§5.5「identityは変更頻度で選ぶ」に従い **Nameタグ等の安定キーをidentityにし、instance-idはmanaged+computed**とする(EC2 identityをinstance-idにすると再作成のたびにintent書き換え=Terraformのstate地獄の再来)。

**secret/credential(核心)**:2階建てになる。(A) **engine→AWSへの到達資格** = §14.5/§15 の `CredentialProvider` 経路。AWSは §15.2「静的資格を配るな、動的発行しろ」の**層Aに理想的に合致**——IAM Role + **AssumeRole(STS)で短命クレデンシャルをJIT発行**でき、reconcilerホストにアクセスキーを保存しない(EC2上ならInstance Profile、外ならOIDC federation)。credential-broker(v2+)の `Acquire/Release` がSTS AssumeRoleを包む形は設計変更なしに収まる。**v1暫定(§14.5)ではSecretRefでIAMキーを注入**するが、これはAWSでは避けたい(長命キー=最大の攻撃面)ので、AWS導入はできれば broker/STS 経路(v2)を待つ方が安全。(B) **AWS Secrets Manager と オンプレVault の関係** = 両者を統一せず **SecretRefのbackendを2つ持つ**(§3.4 `SecretBackend` seam。Vault=オンプレ資格、Secrets Manager=RDS認証等AWS内で完結する値)。§14.1「値を収束しない・参照と鮮度だけ」は両backendに等しく効く。

## 5. 安全機構の適用（最重要の緊張点）

**常時Observeループ × AWSの現実の衝突を数字で見る。** §6.2 は「毎周・全部・間引きなし」を価値の源泉(drift検知漏れゼロ)とするが、AWSでは3つが同時に効く:

- **API課金**:DescribeはAPIコール課金対象ではない(大半のDescribe/Listは無料)ため**直接のコール課金は軽微**——ここはよくある誤解で、実は課金面は思ったより痛くない。ただし**CloudTrail data/management event記録・Config・監視連携がノイズと保管コストを生む**:約340リソース(EC2 150+RDS 20+VPC/SG/Route53/S3多数)を10秒周期でDescribeし続けると 340÷10 = **毎秒34コール、月間約8,800万リクエスト**。CloudTrailのmanagement event自体は課金外だがS3保管とダウンストリーム(SIEM/Athenaスキャン)が量に比例し、**Describeがイベントログを埋め尽くして本当の変更が埋もれる**運用ノイズが実害。
- **レート制限**:EC2 DescribeInstancesはトークンバケット制。毎秒34コールは単一アカウントなら耐えるが、8VPC×多リソースを短周期で叩くと**スロットリング(RequestLimitExceeded)**が出て、これは §5.3 の transient エラーとして backoff(§6.6)に落ちる=**Observe自体がrate limitで律速**され、§6.2 の早期警戒線(「API課金/rate limitがObserve頻度で効く」)に**AWSは即座に抵触**する。
- **帰結**:AWS側は §6.2 の「間引きなし」を**放棄せざるを得ず、§12.2 の延命策(Observe間引き・tier別頻度・watch併用)がv1初日から必須**。理想は **EventBridge/Config を watch源にしたハイブリッド**(§12.2「provider が watch を持つなら drift即知+低頻度ポーリング保険」)だが、これはv2+機能。

**「全EC2削除」事故の防止**:設計の芯がそのまま効く最重要ポイント。(1) `pruneable:false` 既定(§8.3)でRDS/EC2は明示opt-inなしに消えない、(2) rate limit の破壊的重み付け + `on_exceed: halt-destructive-only`(§10.2)で「delete 10件でhalt」、(3) render障害時の前回値保持(§7.3)で空desired→全削除を構造的に防止。**これはまさにTerraformのstate破損2回が引き起こした事故クラスを設計で殺せる領域**であり、Sakuteiの対Terraform最大の売り。

## 6. 段0ブートストラップの当てはめ

段0(Proxmox 3ノード+統合Ceph+VM-HA、§19.2)はオンプレに置き、Vault/credential-broker/reconcilerを載せる形はそのまま当てはまる。**AWS向けの追加**は:(a) reconcilerがAWSへ到達する経路 = Direct Connect経由でSTSエンドポイントに到達 or OIDC federation、(b) AWSシャードのreconcilerも段0オンプレに置くか、AWS内(管理用VPC)に置くかの判断。**後者を推奨**——DX 2本+VPN冗長でもオンプレ→AWS経路断時に「AWSのdrift検知が全盲」になるのは §15「single point of blindness」そのもの。AWSシャードを管理VPC内のEC2に置けば、DX断でもAWS側は自律収束継続でき、§12.3 の分散Executor段階縮退(分断中=加算のみ・破壊保留)が効く。ただし分散Executor自体がv2+。段0の「git外部・稼働必須依存にしない」(§19.4)は維持できる。

## 7. 成り立たない点・苦しい点・設計ギャップ・必要な拡張(最重要・忖度なし)

- **G1. Observe毎周主義がAWSで即破綻**:§6.2 の「間引きなし=価値の源泉」がAWSのrate limit/CloudTrailノイズと真っ向衝突。**AWS側はwatch併用ハイブリッド(EventBridge/Config駆動、v2+)が事実上必須**で、これ無しではSakuteiの中核体験(drift即知)がAWSで劣化する。設計が誇る「毎周全Observe」はオンプレ前提の贅沢であり、他人の庭では成立しない。
- **G2. Terrantとの直接対決で優位が薄い**:AWSは既にDescribe=真のObserve/Listが揃うため、**Terraformのstate地獄の主因(state≠現実の三項差分)は、Terraform側も `terraform plan -refresh` で毎回refreshすれば大きく緩和できる**。Sakuteiのstateレス優位は「state破損事故の根絶」と「drift自動修復ループ」に絞られ、**前者はTerraform Cloud/state locking+暗号化で相当程度対処可能、後者はAtlantis/driftctl+CIで擬似再現可能**。純増価値は「常時収束ループ」だが、それがG1でAWSでは削られる。**Terraformの巨大なAWS providerエコシステム(§21が認める勝因)を捨てて自前AWS providerを書く工数**は、SRE 5名には過大。
- **G3. AWSマネージド抽象と収束モデルの摩擦**:RDS/ASG/マネージドサービスは**内部に強い収束器を持つ**(§11.2-1「収束器の上の収束器」)。RDSの`modifying`が数十分続く・メンテナンスウィンドウでAWS側が勝手に動かすフィールド(マイナーバージョン自動アップグレード・バックアップ)は全て §11.2-4 managed fields として除外必須で、**managed宣言を1つ間違えるとAWSと永久に喧嘩**する。この切り分けはAWSサービスごとに深く、自前provider実装の8割がここに溶ける。
- **G4. 部分導入(オンプレのみSakutei)の妥当性=高い**:これが忖度なしの結論。**オンプレ側はSakuteiの主戦場で○成立、AWS側はG1-G3で△**。したがって**「オンプレ=Sakutei / AWS=Terraform継続」の併存が現時点で最も合理的**。またがる依存(Route53←オンプレ、EC2←オンプレDB)は、Sakuteiが**Route53だけをprovider化して書く**(DNSは §24.4 が最初の一歩に挙げるクリーンな対象・伝播はconverging)ことで、オンプレ発のRoute53レコードのみSakutei管理・残りのAWSはTerraform、という**seam分割**が可能。全AWSをSakuteiに寄せる必然性は無い。
- **G5. コスト比較(概算)**:AWS全面移行 = 自前AWS provider群(VPC/EC2/RDS/SG/Route53/S3、各managed fields切り分け込み)+ watch併用(v2)+ credential-broker STS(v2)= **v2ロードマップ相当の開発**。対してTerraform継続コスト = state locking整備 + drift検知CI(driftctl等)+ 事故再発防止のstate暗号化/バックアップ = **既存資産の補強**。SRE 5名では**後者が圧倒的に安い**。Sakuteiを入れる価値が最大化するのはオンプレ(Ansibleのpush型・drift放置・手作業多)側であり、そこに集中投資すべき。

## 8. 総合判定

| 側 | 判定 | 理由 |
|---|---|---|
| **オンプレ(Proxmox/VM 200)** | **○ 条件付き成立** | 設計の主戦場。§23 brownfield(import→alert-only→scope昇格)がそのまま処方箋。DBSource不要(全platform相当)でv1機能に収まる。条件=alert-onlyで信頼獲得、基幹DBは永久alert-onlyでも可(§23.7) |
| **AWS(VPC 8/EC2 150/RDS 20)** | **△ 大幅な拡張が必要** | Provider基盤としてのAWS APIは最良ケースだが、常時Observeループがrate limit/課金ノイズと衝突(G1)、対Terraformの純増価値が薄い(G2)、managed fields切り分けが深い(G3)。watch併用・credential-broker(STS)・自前AWS provider群という**v2+機能群がクリティカルパス**。部分導入(Route53のみ・§24.4のseam分割)なら即座に価値が出る |

**結論**:「AWS側もSakuteiで」への回答は**現時点ではNo(全面)/ Yes(Route53等の境界リソースのみ)**。Sakuteiのstateレスは魅力だが、AWSではTerraform自身がrefreshで擬似stateレス化でき、かつ毎周Observe主義がAWSの現実で削られるため、**対Terraformの決定的優位が立たない**。投資はオンプレに集中し、またがる依存はseamで分割するのが最合理。
