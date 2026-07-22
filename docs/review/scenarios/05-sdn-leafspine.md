# 対象構成05: Leaf-Spine SDN データセンターネットワークへの Sakutei 適用検討

**総合判定: ○ 条件付き成立** — コア抽象(Resource / 順序付き ACL / commit-confirm / 物理トポロジ座標 identity / NetBox=inventory+allocator)がこの構成に驚くほど素直に写像する。ただし成立は **(1) EVPN/VXLAN の分散状態=ファブリック全体の到達性を「収束器の上の収束器(converging)」として正しく扱えること**、**(2) commit-confirm を全 leaf で協調させる Workflow(段階適用)を v2 相当まで実装すること**、**(3) SONiC/EOS 混在を1論理リソース2実装として吸収する provider 設計** の3点が満たされる場合に限る。v1 スコープ(§24.1)のままでは「可視化(alert-only)+ import/Sync Mode」までしか成立せず、自動収束(auto)には v2 の Task/Workflow/段階適用が必須。

---

## 1. 構成の前提整理

- **規模**: Spine 4 + Leaf 96(SONiC 60 / EOS 36)、200 ラック。テナント VRF 約80、VLAN/VNI 約1,500。純 NW リソースだけで概算 数千〜1万オブジェクト(peer × config × VRF × VNI)——§12.2 の単一デーモン実用上限(数千〜1万)の**上端に張り付く**。
- **コントローラレス**: NSX/ACI 無し = 上位の宣言的コントローラが存在しない。EOS/SONiC の各機器が config の直接対象。ただし **BGP EVPN 制御プレーン自体が分散収束系**であり、ここが本構成最大の「収束器の上の収束器」ポイント。
- **アンダーレイ eBGP**: ASN 採番が必要 = **allocator の一級ユースケース**。VNI・ループバック IP・P2P リンク IP も同様。
- **ToR 配下サーバは管理対象外**: NW/config のみが Sakutei の領土。境界は明確(§8.1「境界の宣言」が引きやすい = brownfield 昇格に有利)。
- **NetBox 既設・正として整備**: ケーブル/ラック/IPAM が正 = `RenderedSource`(inventory)+ `AllocationAuthority`(採番)の理想的な供給源が既にある。
- **運用の現状**: Ansible + 手動レビュー、NETCONF/gNMI 経験あり 5名。→ Sakutei の provider 実行実体(§21.2 Ansible モジュール借用)と NETCONF/gNMI ネイティブ provider の両方に踏み込める人員。組織的受容の素地もある(§23.5)。

---

## 2. リソースモデリング

**基本方針**: config は「順序無関係の宣言集合(BGP peer / VLAN / VNI / VRF / SVI)」と「順序依存リスト(ACL / route-map / prefix-list)」に割れる。前者は通常 Resource、後者は §8.5 順序付きコレクション(リスト全体を原子単位・Apply=全置換・表示はルール単位)。firmware/ローリングは Task/Workflow(§9)。ファブリック全体の一斉変更(全 leaf の BGP テンプレート更新)は §10.5 段階適用 Workflow。

**Resource/Task/Workflow への写像**

| 対象 | プリミティブ |
|---|---|
| BGP peer(アンダーレイ eBGP / EVPN オーバーレイ)、VRF、VNI、VLAN、SVI、VXLAN VTEP | Resource(closed-world / peer は座標 identity) |
| ACL(東西 FW)、route-map、prefix-list、community-list | Resource(順序付きコレクション §8.5) |
| Palo Alto セキュリティポリシー(南北)、F5 VS/pool/member | Resource(順序付き:PANOS ルール、F5 は set) |
| NOS/EOS firmware 更新 | 多相 Task(§9.1) |
| ファブリック全体の BGP/EVPN テンプレート一斉変更 | Workflow + 段階適用(canary→broad §10.5) |
| leaf 増設(ケーブル済み→config 流し込み→EVPN 参加) | Workflow(ready_condition 連鎖 §6.7) |

**必要 Provider 一覧**

| Provider 名 | 対象 | Capabilities | クラス | identity 設計 | 実装難度 |
|---|---|---|---|---|---|
| `arista_eos` | Spine 4 + Leaf 36(EOS)。BGP/VRF/VNI/VLAN/SVI | TrueObserve:○(eAPI/gNMI で state 取得) List:○ Converging:△(config 反映は即だが EVPN 収束は別) CommitConfirm:○(`configure session` + `commit timer`) | 完全 | **物理トポロジ座標**: `leaf03`, peer は `leaf03↔spine01`(自ポート×対向)。ASN/loopback は managed+computed | 中〜高 |
| `sonic` | Leaf 60(SONiC)。同上 | TrueObserve:○(gNMI/CONFIG_DB Redis) List:○ Converging:△ **CommitConfirm:✗(ネイティブ candidate/commit 無し)** | 標準〜完全 | 同上(座標で EOS と同一 identity 空間を共有) | **高**(commit-confirm 不在の代替が要る・下記§5) |
| `nos_acl`(論理) | 順序付き ACL。EOS/SONiC 両実装にディスパッチ | 上記に準拠 + `ordered` Schema | 完全 | ACL 名 + 適用点(interface/VRF)。ルール単位でなくリスト全体が identity | 中(§8.5 に乗る) |
| `panos` | Palo Alto ペア(南北 FW) | TrueObserve:○ List:○ Converging:○(candidate/commit + job API) CommitConfirm:○ | 完全 | ルールベース名 + vsys。順序付き | 中 |
| `f5_ltm` | F5 ペア(LB) | TrueObserve:○(iControl REST) List:○ | 標準 | partition + VS 名 | 中 |
| `netbox_source` | inventory 合成(RenderedSource) | — | 供給源 | — | 中 |
| `netbox_alloc` | ASN/VNI/loopback IP/P2P IP 払い出し(AllocationAuthority) | — | allocator 実装 | pool × owner | 中(NetBox の予約 API を Reserve/Confirm/Release に写像) |

**混在の要点(SONiC/EOS = 同一論理リソースに複数実装)**: BGP peer / VNI / VLAN は「論理 Schema は1つ、Read/Apply 実装が NOS ごとに2つ」。§5.6 の「抽象リソースと実装の分離」に該当するが、v1 では**具体型2つ + render での実装選択**(NetBox の platform フィールドで leaf ごとに `arista_eos`/`sonic` を振り分け)で回避するのが正。§5.6 の解禁トリガ(「2つ目の同種実装が必要になったとき」)が**まさにこの構成で発火する**——本構成は capability 抽象の最初の実証ケースになりうる。

**interface/BGP peer の identity(§5.5)**: シリアル・MAC・実 IP は変更頻度が高く NG。**物理トポロジ座標**(`leaf03-Ethernet1↔spine01-Ethernet3` / `leaf03` の ASN は座標から導出)を identity にすれば、機器故障交換で intent 不変。ASN・loopback・P2P IP は `managed:true`+`computed`(allocator が払い出し、観測のみ)。これは §5.5 の設計指針が**本構成のためにあるかのように一致**する。

---

## 3. ownership・DesiredSource・sharding 設計

- **ownership**: ファブリック本体(アンダーレイ eBGP / spine / EVPN コア / 共有 VRF)は `platform`(PR 必須・alert-only 寄り・rate limit 厳格・halt)。テナント VRF/VNI/VLAN 約80 は `tenant`(即時・auto 可・warn)。**ただし NW は VM と違い「テナントの砂場」が物理を共有する** ため、テナント VLAN 追加すら全 leaf の config を触りうる。→ テナント層でも「枠(採番済み VNI レンジ・許可 leaf 集合)内は即時、枠外は platform 承認」(§12.1)を厳格に効かせる必要がある。
- **DesiredSource**: platform=`FileSource`(git・PR)、inventory 合成=`RenderedSource`(NetBox→render→PR)。テナント層を `DBSource`/catalog にするかは NW チーム5名の運用モデル次第(当面は git 一本で足りる)。
- **sharding**: 現状は単一デーモンで足りる(§12.2 上限内)が上端。将来は **site/pod 単位**(Leaf-Spine の pod)で shard 分割が自然。共有基盤(spine/アンダーレイ)は platform shard 1つが所有、tenant shard は read-only 参照(§12.2)。分割キーは pod/rack 座標がそのまま使える。

---

## 4. render(inventory/allocation/secret)の使い方

- **NetBox = inventory + AllocationAuthority**(§16.5・§7)。ケーブル/ラック/IPAM が正なので、render フェーズ1 が NetBox から (a) トポロジ(誰が誰と繋がる=BGP peer 生成の素材)、(b) ラック/機器一覧、を合成し、フェーズ2 で `virtual_network`(=VRF+VNI+VLAN+SVI+EVPN route-target)を扇状 DAG に展開する。
- **allocator の使い分け**: NetBox は IPAM(IP/prefix)+ ASN/VLAN の予約 API を持つので、**IP・ASN・VLAN は NetBox を `AllocationAuthority` 実装として使う**(§16.5 の「NetBox を実装として差し替え可能」)。ただし **VNI(1,500)や EVPN route-target のプールは NetBox が素直に持てない場合があり**、そこは**自作最小 allocator**(SQLite+アトミック払い出し)が正当化される(§16.5「state を持つ部分は自作の価値」)。→ **IP/ASN/VLAN=NetBox、VNI/RT=自作**の二本立てが現実解。
- **仮予約 reserve→confirm(§16.3)**: platform 層は PR 生成時に ASN/VNI を TTL reserve し PR diff に具体 ID を出す→マージで confirm。NW では二重払い出し=衝突が最悪級事故(§16.4)なので、Release は下流 config の実消去を Observe 確認後 + cooldown を厳守。
- **secret**: BGP MD5/TCP-AO 鍵、SNMP community、NETCONF/gNMI ログイン資格は `SecretRef`(機器に設定する値)/ `CredentialProvider`(engine が機器にログインする資格)を §14.5 の境界どおり分離。v1 は Vault 静的資格注入で暫定。

---

## 5. 安全機構の適用(全断事故の防止)

想定事故:全 leaf の BGP テンプレートを1行間違え→次サイクルで全 leaf に一斉 Apply→**ファブリック全断**。level-triggered の「速く確実に破壊する」裏返し(§10.1)の教科書ケース。防御は多層:

1. **rate limit + halt(§10.2)**: `max_changes_per_cycle` を破壊的側重み付けで厳格化。96 leaf 同時変更は絶対に閾値超過→`halt-destructive-only`。**in-flight を含めて評価**(§6.4)しないと「毎周少しずつ全断」の抜け道が残る。
2. **段階適用(§10.5)**: ファブリック変更は canary(1 leaf・least-critical)→ health gate + manual_approval → broad。これが**本構成で auto を成立させる中核**であり v1 スコープ外(v2)。
3. **commit-confirm(§11.2-5・§19.3)**: 管理到達性を切る変更でも「confirm が来なければ自動ロールバック」。**EOS/PANOS は candidate/commit ネイティブで成立。SONiC は candidate/commit が無い**——ここが最大の穴。SONiC には (a) Apply 前に管理到達性チェックを行う「自己 watchdog」型(別経路から到達確認→失敗なら config replace で巻き戻し)、(b) config snapshot + タイマー付き rollback スクリプトの自作、で `Capabilities.CommitConfirm` 相当を**provider 側で合成**する必要がある。§21.3 の「賢さは自前側に置く」の適用。**管理NW/spine 系機器では CommitConfirm 必須(§19.3)** なので、SONiC がここに含まれると設計上の要請と実装能力が衝突する。
4. **converging 誤認防止(§10.4・§11.2-1)**: EVPN の route 収束(VTEP 学習・MAC/IP advertise)は機器 config 適用後も数秒〜十数秒かかる。config は converged でも**ファブリックの到達性はまだ収束中**。これを drift と誤認して再 Apply すると綱引き。Tier2 自傷猶予(settle 窓)+ Tier3 振動検疫が効くが、EVPN の realization を Tier1 で観測する手段(gNMI で BGP EVPN table の収束を見る)を provider に実装できるかが品質を分ける。
5. **freeze(§10.6)/ alert-only 既定**: 導入初期・変更時の脱出口。

---

## 6. 段0ブートストラップの当てはめ(循環)

**reconciler の通信路 = この管理 NW そのもの**という循環がフルに発生する。§19.3 が正確にこの問題を扱う:

- **管理系とデータ系の分離**: 本構成は EVPN/VXLAN=**データ系**。reconciler が leaf/spine に到達する**管理系(OOB/管理 VRF)は別系統**であり、データ系(VXLAN)を全断しても管理系経由で機器を制御できれば single point of blindness にならない(§19.3)→ **データ系は全て段1、管理系の到達性最小核だけ段0**。
- **同じ機器の中で段が分かれる(§19.3)**: spine/管理 leaf の「reconciler 到達性を担う最小 config(管理ポート・管理 VRF・OOB 経路)」は段0(Ansible 構築・Sakutei は触らない・managed field 扱い)、それ以外の EVPN/テナント config は段1。
- **OOB = ネットワーク版 Break Glass(L2.5)**: コンソールサーバ/OOB 専用 NW が管理系全断時の唯一経路。Sakutei 管理対象外。**本構成では OOB の設計が段0成立の絶対前提**——アンダーレイ eBGP が管理系も兼ねる設計だと循環が閉じず段0が成立しない。管理系を EVPN から独立させられるかが判定を左右する。
- **tier: foundation**: spine と管理 leaf は依存の根。

---

## 7. 成り立たない点・苦しい点・設計ギャップ(忖度なし)

1. **【最大】SONiC の commit-confirm 不在 × 全断リスク**: 設計は「管理NW機器では CommitConfirm 必須」(§19.3)と明言するが、SONiC はネイティブに持たない。provider 側で watchdog/rollback を合成せねばならず、その正しさ(Apply 中に自分が死んだ場合の rollback 発火保証)は非自明。**60 台が SONiC** という比率は無視できない。commit-confirm 相当を作れなければ SONiC leaf は永久に alert-only に留めるのが安全側の結論になり、「自動収束」の価値が半減する。

2. **EVPN の分散状態が Resource モデルの外にある**: Sakutei が収束するのは**各機器の config**であって、**ファブリック全体の到達性(どの MAC/IP がどこから到達可能か)ではない**。config が全機器 converged でも EVPN が正しく収束している保証はない(route-target 設定ミス・非対称 config は config diff ゼロでも通信不通)。これは §17.1 の「在る≠動いている(health)」であり、**canary(合成検証:テナント間/テナント内の疎通を能動テスト)が事実上必須**。しかし canary は v2。→ v1 では「config は正しいが通信できない」を検知できない。

3. **順序付き ACL の全置換 × ライブトラフィック**: §8.5 は Apply=全置換。だが稼働中 ACL の全置換は瞬間的なポリシー空白(全 deny/全 permit)を生みうる。機器がアトミックに差し替えられるか(EOS の `configure session` は原子的、SONiC の ACL は ACL テーブル単位の置換で挙動が NOS 依存)は provider 実装の生死を分ける。設計書は「candidate/commit と好相性」と言うが**SONiC でその前提が崩れる**。

4. **スケール上端(§12.2)**: 数千〜1万オブジェクトは単一デーモン実用上限の上端。毎周全 Observe が 96+4 機器 × gNMI subscribe で成立するか、Observe 時間が reconcile 間隔を食わないか(§6.2 早期警戒線)は実測が要る。1,500 VNI × 80 VRF の展開で render フェーズ2 のオブジェクト数が膨らむと、DAG 計算より先に Observe が刺さる。pod shard 分割(§12.2)を早期に見込む必要。

5. **capability 抽象(§5.6)が v1 に無い**: EOS/SONiC の1論理リソース2実装を v1 では「具体型2つ+render 振り分け」で回避できるが、Schema の二重管理(BGP peer を EOS 用・SONiC 用で2回書く)は保守コスト。§5.6 の解禁が必要になる公算が高く、**本構成が capability 抽象の要件定義を駆動する**。

6. **converging の Tier1 観測が難しい**: EVPN realization を「収束完了」として観測する汎用手段が無い(vCenter task API のような明示的 job がない)。Tier2 settle 窓 + Tier3 振動検疫で妥協はできるが、settle 窓を長く取ると drift 検知が鈍り、短いと綱引き。EVPN 特有の realization 検知 provider ロジックが品質の肝で、これは新規実装。

7. **組織・運用(§23.5)**: 現状 Ansible+手動レビュー。「勝手に戻す」が一度でも全断を起こせば信頼を失う。NW は爆発半径が構造的に全体に及ぶため、**alert-only 期間を他ドメインより長く取る**べき。import→Sync Mode で NetBox/git に現実を溜め、diff ゼロ N 日を厳しめに設定。

**必要な拡張(まとめ)**: (a) SONiC commit-confirm 合成 provider、(b) EVPN realization を見る converging Tier1 実装、(c) 疎通 canary(v2 前倒し)、(d) 段階適用 Workflow(v2)、(e) capability 抽象(§5.6 解禁)、(f) pod shard(§12.2)、(g) VNI/RT 自作 allocator。

---

## 8. 総合判定

**○ 条件付き成立。**

**成立する理由**: リソースモデリングが極めて素直——物理トポロジ座標 identity(§5.5)、順序付き ACL(§8.5)、commit-confirm(§11.2-5)、NetBox=inventory+allocator(§7・§16.5)、管理系/データ系分離の段0(§19.3)は、いずれも本構成を**具体例として想定していたかのように**設計書の各節に直接対応する。NetBox が正として整備済み・NW チームに NETCONF/gNMI 経験がある点も追い風。可視化(alert-only)+import/Sync Mode(§23)なら v1 スコープで**即座に価値が出る**(NW の実態可視化・drift 検知は現場価値が大きい)。

**条件(これが揃わなければ auto 収束は成立しない)**:
1. **SONiC の commit-confirm 相当**を provider 側で合成できること(全断の自動ロールバック保証)。できなければ SONiC leaf は alert-only 固定。
2. **段階適用 Workflow(§10.5)と疎通 canary(§17.3)を v2 相当まで実装**すること。config 収束だけでは EVPN の到達性を保証できないため。
3. **管理系(OOB/管理 VRF)がデータ系 EVPN から独立**していること(§19.3・§6 の循環を断つ絶対条件)。アンダーレイが管理系を兼ねると段0が閉じない。

これらを満たせば ◎ に近い。満たさない場合は「**可視化 + テナント層の枠内即時収束は成立、ファブリック本体は永久 alert-only**」という部分成立に落ち着き、それでも Ansible+手動レビューの現状より前進する。**推奨導入順**: DNS/監視のような周辺ではなく、まず全機器 import→alert-only で NetBox/git と現実を突き合わせる可視化から入り、新規増設 pod を greenfield で auto 化、既存中核は最後(§23.7)。
