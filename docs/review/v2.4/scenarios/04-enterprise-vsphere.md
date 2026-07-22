# 対象構成04: 単一DC大規模仮想化基盤(エンタープライズ)Sakutei 適用可能性 再々評価(v2.4)

**新判定: ○(条件付き成立)を維持しつつ「スケール天井が実質的にクリアされ、v2 シャーディング前提が外れた」ことで確度が一段上がる。既存8,000VM全域の即時 auto 化を目標にしても、残るのは FC/意味的CMDB腐り/組織文化であり、スケールと周期律速はもはや主因でない。**

**判定推移: v2.2 △(大幅拡張が必要) → v2.3 ○(条件付き。ただし天井は「正直化」して悪化、シャーディングは v2 前提) → v2.4 ○(強い条件付き。BulkObserve で単一デーモン圏内に復帰、静的シャーディングが v1b に前倒し、周期律速が早期収束で解消)**

v2.3 は「external_owner・remediation schedule・incomplete≠empty・v1 再スライス」で方向性を確定させたが、
**スケールの芯(単一デーモン約200台/vCenter)とレイテンシの芯(周期律速)は未解決**のまま ○ の但し書きに残っていた。
v2.4 の2提案(早期収束パス=CHG-E群 / スケールアウト経路=CHG-F群)は、まさにこの2つの芯を狙い撃ちしており、
本構成に対する効果が大きい。

---

## ギャップ解消表(CHG-ID 対応)

| # | v2.3 残存ギャップ | v2.4 該当変更(CHG-ID / 章) | 判定 | 根拠 |
|---|---|---|---|---|
| 1 | 単一デーモン天井 約200台/vCenter(8,000VM を単一で回せない) | **CHG-F1 §5.7 BulkObserve** / §12.2 天井表 | **解消(型 v1a・実装 v1b)** | vCenter **PropertyCollector で1コール/クラスタ全VM**。天井表 vCenter 行が「~200台」→「**数千〜1万へ**」。8,000VM(2 vCenter)が**単一デーモン圏内に復帰**。毎周全量再観測は不変(取り方が N コール→1コール) |
| 2 | tier別間引きは v1b seam どまり、watch 併用は v2 | **CHG-F1 §5.7**(バルク観測)/ §12.2(間引きは補助に降格) | **解消(前提が変わった)** | 間引きの動機だった「Observe が律速」が BulkObserve で緩む。間引きは"更に伸ばす"補助手段に降格し、drift 即知とのトレードオフ(v2.3 懸念1)を回避しやすくなる |
| 3 | シャーディング本体が v2 送り | **CHG-F2 §12.4 claim-as-intent** / CHG-F3 §24.2 | **解消(静的は v1b・動的のみ v2)** | クラスタ/サイト単位の**静的シャーディングが v1b に前倒し**。`--claim` 型は v1a、実動作は v1b。新 S2(割当表)を増やさず fencing 世代番号のみ。縦割り10名の組織境界を `shards/*.yaml` に写せる。動的ルーター/自動リバランスのみ v2 残 |
| 4 | 周期律速で申請VM反映が周期待ち(月500件/日20件超) | **CHG-E1 §6.8 早期収束** + **CHG-E2 §13.5 Watch(FileSource)** | **解消(v1a)** | **PR マージ=ヒント発火→秒オーダーで scoped reconcile**。ITIL の RFC 承認直後に収束。ガバナンス不変・レイテンシのみ短縮(CHG-E4)。Watch/scoped run は v1a |
| 5 | ID/rendered_revision がシャードで壊れないか未確定 | **CHG-Z1 §26.1 A-1**(ID=(provider,scope,id)確定)/ **CHG-Z2**(rendered_revision scope 単位) | **解消(着手前決定に格上げ)** | 再シャード(scope 移動)で `id` 不変・`depends_on`/監査帰属が壊れない。部分収束もシャード独立収束も scope 単位ハッシュで成立。着手前決定型に確定 |
| 6 | シャード予算/爆発半径の粒度が曖昧 | **CHG-Z3 §26.1 A-1** | **解消** | rate limit・累積破壊予算は**シャード=ownership=爆発半径境界**単位。速い経路(§6.8)と定期経路が同一シャード内で予算共有(CHG-E3)。「1 commit で全VM削除」防御がシャード内で閉じる |
| 7 | FCゾーニング/PowerMax provider 高難度 | 変更なし(v2.4 は触れず) | **未解消** | v2.4 は BulkObserve/早期収束/シャーディングに集中。FC/ストレージ層は依然 Ansible ラップ+alert-only 天井 |
| 8 | 意味的CMDB腐り(件数正常・値誤り) | 変更なし | **未解消** | §7.6 基数検知どまり。Observe 突き合わせ異常検知は v2.4 でも設計外。CMDB を desired 権威にしない運用回避が前提のまま |
| 9 | ITIL/CAB との思想差(組織問題) | CHG-E4 §6.8(PR マージ=承認直後収束)で技術面は更に整うが | **未解消(組織課題)** | 早期収束で「承認→即反映」が技術的に綺麗になる分、CAB との橋渡しは強化。だが「宣言を承認する」文化転換(§23.5)は技術で解けない残課題 |

---

## 判定変更の理由

### BulkObserve が「単一デーモン天井」評価を反転させた(最大の効き)
v2.3 の ○ は「単一では約200台/デーモン、超過分はシャード(v2)」という段階の上に立っており、
**8,000VM の即時 auto 化を目標にすると △ に戻る**という但し書きが本体だった。CHG-F1(§5.7 BulkObserve)は
この算術の暗黙前提「1リソース=1コール」を崩す。vCenter は **PropertyCollector** を持ち、クラスタ内全VMの
指定プロパティを1コールで返せる。天井表(§12.2)の vCenter 行は「0.8s × 8,000 = 8.3分/周・上限~200台」から
「**1コール/scope で数千〜1万へ**」に書き換わった。VM 8,000(vCenter 2台)なら **vCenter あたり4,000**
であり、下位 Resource(監視・バックアップ)を含む2〜4万規模も、これらの権威(Prometheus 系・バックアップ
API)がバルクを持つ限り単一デーモン圏内に収まりうる。**結果、v2.3 で ○ の但し書きの主因だった
「スケール天井」がクリアされ、シャーディング(v2)を必須前提にしなくてよくなった。** これは判定の確度を
一段上げる。ただし「毎周全量再観測」は不変(§5.7・§6.2)であり、観測 state レス思想は無傷。

### 静的シャーディングの v1b 前倒しで「縦割り10名」が第一級の分割軸になる
仮にバルクでも足りない/障害隔離を強めたい場合、CHG-F2(§12.4 claim-as-intent)が **クラスタ/サイト単位の
静的シャーディングを v1b で提供**する。本構成の「仮想化/NW/ストレージの縦割り10名」を、そのまま
`shards/*.yaml`(`claim: {cluster: ...}`)の PR レビュー対象=第一級 intent に写せる。v2.3 では
「割当は運用の隠れた設定」で新 S2(割当表)依存だったのが、**fencing 世代番号のみ S2**で済む。
共有基盤(ACI/FC/PowerMax)は専用 platform シャードが単一 writer(fencing 必須・CHG-B6 は v2.3 で導入済)、
テナントシャードは read-only 参照。**動的リバランスだけが v2 に残る**(CHG-F3)ので、
静的で足りる本構成では v2 待ちが消える。

### 早期収束パスが「申請ベース・周期律速」と ITIL を同時に解く
月500件(日20件超)の申請VMは、v2.3 では「PR マージ後、次の定期周期(数分〜)まで反映が待たされる」
周期律速に晒されていた。CHG-E1(§6.8)+ CHG-E2(§13.5 Watch=FileSource git merge フック・**v1a**)で
**PR マージ=ヒント発火→影響サブ DAG だけを秒オーダーで scoped reconcile** できる。決定的な新旧 desired
diff(リアル非接触)で影響範囲を算出し、そこだけ新鮮に Observe→Diff→Apply するため、観測 state レスは無傷。
ITIL 的にはむしろ好適で、**RFC 承認(PR マージ)直後に収束が起きる**——ガバナンスは不変・レイテンシのみ
短縮(CHG-E4)。承認ゲート(§10.5)・PR 必須(§12.1)はそのまま効く。CAB の変更窓は v2.3 の
remediation schedule と併用でき、「窓内は auto+早期収束、窓外は alert-only」が宣言できる。

---

## v2.4 が生む新たな懸念

1. **PropertyCollector の一貫性スナップショット問題**:BulkObserve は「1コールで scope 全量」を返すが、
   PropertyCollector が返すのは厳密なポイントインタイム・スナップショットではない(内部でオブジェクトを
   走査する間に vMotion/電源変化が進む)。**DRS/HA が常時配置を動かす本構成**では、バルク結果内に
   「走査開始時のホスト」と「走査終了時のホスト」が混在しうる。managed field(current_host 等)を
   Schema で除外していれば diff には影響しないが、**除外設計を誤ると"バルクの中で自己矛盾した観測"が
   drift として立つ**。§5.7 は「取り方が変わるだけ・毎周全量」と言うが、per-resource Read の各コールが
   個別時点だったのに対しバルクは"走査窓"を持つという微妙な差は、DRS 前提の環境で顕在化する。

2. **PropertyCollector の managed fields 混入とバルクの粒度**:バルクで全プロパティを一括取得すると、
   runtime.host / runtime.powerState / DRS 予約など **managed:true にすべきフィールドが素朴に全部乗って
   くる**。per-resource Read なら Provider の Read ロジックで取捨できたが、バルクは「まとめて返る」ぶん
   SDK 側の managed 除外(§4.3/§5.7)が確実に効くことへの依存が強まる。v2.3 の「Schema が本体」という
   難所が、BulkObserve 経路でも(あるいはより)効く——バルク結果に対する managed 除外の適用漏れは
   DRS との永久喧嘩に直結する。

3. **early-convergence と DRS/HA の相互作用**:早期収束は「desired が変わったリソースのみ」を扱い drift B は
   定期ループに残す(CHG-E3)。これは正しい純化だが、**PR マージで触った VM のサブ DAG を Observe した
   瞬間、その VM が HA 電源再投入/vMotion 中**という一時状態を拾う可能性がある。ready_condition/converging
   写像(§10.4)で吸収する設計だが、scoped run は範囲が狭いぶん「周辺の落ち着き」を待たずに Apply へ進む
   経路になりうる。scoped run の wave/skip セマンティクスが managed 一時状態を正しく converging 扱いに
   落とせるかは、本構成固有の検証点。

4. **BulkObserve と List(prune 用)の型分離の運用負荷**:§5.7 は BulkObserve(状態)と List(ID集合・
   prune 前提)を意図的に別型に保つ。vCenter は1コールで両方返せるが、**prune の安全性は §8.6 List
   完全性検証に依存**するため、バルク結果を prune 判断に流用してはならない。8,000VM 規模で「Observe は
   速いが prune は別経路で慎重」という二重構造を運用者が誤解し、バルク結果で削除判断してしまう事故余地。

5. **allocator/Vault QPS が新しい共通天井**(CHG-E4 / CHG-F5 §12.4):早期収束(render フェーズ1 の採番が
   速い経路に乗る)と静的シャーディング(シャード数だけ衛星 QPS が増える)の**両方が allocator/Vault の
   スループットに合流**する。月500件・日20件超の申請VMが早期収束で即時払い出しを叩くと、allocator が
   UX の律速になる。本構成では NetBox/Infoblox 等の採番権威の QPS 設計が、従来の"背景タスク"から
   "UX 直結の前景依存"に格上げされる。

---

## この構成で成立させるための条件(v2.4 更新版)

1. **brownfield 導入戦略(§23)は不変で厳守**:段階0 全域 alert-only+prune 禁止 → import → Sync Mode で
   CMDB 機械是正 → scope 単位で「managed 除外後 diff ゼロ N 日」で昇格。ハイパーバイザ中核・FC/ストレージは
   永久 alert-only 可(§23.7-4)。**この順序は BulkObserve/早期収束があっても変えない**。
2. **v1 スコープを正しく取る(v2.4 で拡大)**:v1a に **BulkObserve 型切り・`--claim` 型切り・Watch seam+
   scoped run+合流キュー**が加わった(CHG-Z4 §24.1a)。v1b に **BulkObserve 実装(vCenter から)・静的
   シャーディング実動作(claim フィルタ+fencing+再シャード Workflow)**。v1.5 に最小 Workflow+承認ゲート+
   remediation schedule。**動的ルーター/自動リバランス・DBSource 連動 Watch のみ v2**。
3. **vCenter Provider は BulkObserve を第一級で書く**:PropertyCollector を使い、managed 除外(current_host/
   powerState/DRS 予約)を Schema で厳密宣言。バルク結果を **prune 判断に流用しない**(List は別経路)。
   スナップショット非一貫性を許容する設計(managed 一時状態を converging に落とす)を検証で確定。
4. **外部収束器を宣言で縁切り(v2.3 踏襲)**:VM 配置=`external_owner: drs`、ACI=`external_owner: aci`。
   APIC には粗い intent のみ、realization は §10.4 Tier1+deadline に委譲。
5. **申請VM経路を早期収束+承認ゲートで組む**:tenant 月500件は authority+quota の standard change 枠、
   PR マージ=Watch ヒント=秒オーダー反映。CAB 変更窓は `remediation.schedule` で auto、繁忙期は freeze。
   **allocator(NetBox/Infoblox)の QPS を UX 前提として容量設計**する(新規に前景化した律速)。
6. **シャード境界=縦割り組織境界**:cluster/site+tenant をシャード軸兼 ownership 境界に。共有基盤
   (ACI/FC/PowerMax)は専用 platform シャードが単一 writer(fencing 必須)で所有、他は read-only。
   静的シャーディング(v1b)で足りる規模なので動的化(v2)は原則不要。
7. **CMDB は desired 権威にしない(v2.3 踏襲)**:allocator を採番権威に分離、CMDB は §7.6 急減検知付き
   read 限定+Sync Mode 是正先。意味的腐りは Observe 真実源で上書きし続ける運用で吸収。

**総括**:v2.4 は本構成の ○ を支えていた4つの但し書きのうち **2つ(スケール天井・周期律速)を構造的に
解消**し、**1つ(シャーディング前提)を v2→v1b に前倒し**した。残る △ 要因は **FC/ストレージ provider の
高難度・意味的 CMDB 腐り・ITIL 文化転換**であり、いずれもスケールでもレイテンシでもない領域固有/組織課題。
したがって「可視化・CMDB是正・新規クラスタ/テナントの自律化・保守窓 auto」に限定した ○ は v2.3 より確度が
上がり、**「既存8,000VM全域の即時 auto 化」を目標にしても主因がスケールから外れた**ぶん、△ への逆戻り
リスクが FC/意味腐り/文化の3点に絞られる。
