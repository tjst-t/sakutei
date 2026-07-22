# 対象構成04: 単一DC大規模仮想化基盤(エンタープライズ)Sakutei 適用可能性 第4ラウンド評価(v2.5)

**新判定: ○(条件付き成立)を維持。確度はさらに一段上がる。v2.4 で残った「新懸念」のうち、BulkObserve の一貫性(4点)と allocator/Vault の QPS 天井(1点)が §5.8 の一貫性契約と §16.7 の churn 算術で構造的に消え、残るのは v2.4 と同じ FC/意味的CMDB腐り/ITIL文化の3点のみ。スケール・レイテンシ・衛星の可用性はもはや本構成の判定を動かさない。**

**判定推移: v2.2 △(大幅拡張が必要) → v2.3 ○(条件付き。天井は正直化して悪化・シャーディングは v2 前提) → v2.4 ○(強い条件付き。BulkObserve で単一デーモン圏内に復帰・静的シャーディング v1b 前倒し・周期律速解消。ただしバルク一貫性と衛星 QPS が新懸念として浮上) → v2.5 ○(v2.4 の新懸念5点を SDK 契約・原則で解消。残 △ 要因が FC/CMDB意味腐り/文化の3点に純化)**

v2.4 は本構成の ○ を支えていた4つの但し書き(スケール天井・周期律速・シャーディング前提・ID/予算粒度)を解消した一方、その代償として**BulkObserve 経路そのものの一貫性**と**早期収束+シャード分割が合流する衛星 QPS**という2系統の新懸念を生んだ。v2.5 の3テーマ(衛星の天井=G群 / BulkObserve 一貫性契約=H群 / claim の git 配布・S2-free シャード=I群)は、まさにこの新懸念を狙い撃ちする。本構成では G群・H群が直接効く。

---

## 懸念解消表(CHG-ID 対応)

| # | v2.4 の新懸念 | v2.5 該当変更(CHG-ID / 章) | 判定 | 根拠 |
|---|---|---|---|---|
| 1 | PropertyCollector の一貫性スナップショット非保証(DRS 稼働中に走査窓内で配置が動く) | **CHG-H1 §5.8**(非アトミックスナップショット宣言 + 走査窓内遷移の吸収) | **解消(型 v1a・実装/テスト v1b)** | 走査窓内の非一貫を「正しさ」ではなく「揺らぎ」と定義。level-triggered の毎周全量再観測(§6.2)が自己修正。DRS の runtime placement は managed fields 除外(§11.2-4)が diff から落とし、残りの一時不整合は converging/settle(Tier2・§10.4)が吸収。**新しい吸収機構を作らず既存の縁切りに接続**した点が要 |
| 2 | バルクへの managed fields 混入(runtime.host/powerState が素朴に全部乗る) | **CHG-H1 §5.8**(走査窓内遷移の吸収は managed 除外に委譲)+ §4.3/§11.2-4 | **解消(設計上は。実装依存は残る)** | 「Schema が本体」という難所は不変だが、v2.5 は managed 除外を BulkObserve 経路の明示的接続先として §5.8 に固定。除外設計を誤れば DRS と永久喧嘩なのは変わらないが、それは Schema バグであって BulkObserve 固有の新リスクではないと切り分けられた。**SDK 等価性テスト(下記4)が除外漏れの一部を CI で捕まえる** |
| 3 | BulkObserve と List(prune 用)の型分離の運用誤用(バルク結果で削除判断してしまう) | **CHG-H1 §5.8**(型分離の明文化 + SDK が型レベルで誤用封じ)+ 見送り節(prune 流用を棄却) | **解消(v1a 型 / SDK)** | 「prune の根拠には一切使えない」を SDK 契約に格上げし、**BulkObserve の戻りが集合差 prune の入力に渡る経路を型レベルで塞ぐ**。運用者の誤解に頼らず型で封じる。prune は従来どおり Lister.List + §8.6 完全性検証(件数急減 halt) |
| 4 | (3の裏)バルク結果の周期越え再利用・個別経路との乖離 | **CHG-H1 §5.8**(observed_at 刻印 + 周期越え再利用禁止 + SDK 等価性テスト)+ §4.2 ObservedAt 追加 | **解消(v1a 型 / v1b テスト)** | `BulkObserve(scope) ≡ {Read(id) ∀id}` を SDK テストハーネスに標準装備(「完全+bulk」を名乗る条件)。バルクと個別が別現実を返す実装バグを CI で捕捉。`observed_at` で周期越え再利用を検知・拒否し、観測 state 再導入を防ぐ |
| 5 | allocator/Vault QPS が新しい共通天井(早期収束+静的シャードが合流) | **CHG-G1 §16.7**(衛星停止は変化を止めるが維持を止めない)+ **CHG-G2 §16.7**(QPS=churn 比例)+ **CHG-G3 §16.8**(waiting-allocation)+ **CHG-G4 §19.9**(停止影響表)+ **CHG-G5 §16.5**(HA 最小形+プール分割 by claim) | **解消(原則 v1a・実装 v1b)** | QPS は**フリート規模でなく churn 比例**。8,000VM でも日次 churn 1% なら新規採番 ~80件/日 ≒ 無負荷(定常はシャードローカル Lookup キャッシュ=S3 でヒットし allocator に触れない)。早期収束バーストは**ポータル申請レートで上限**。allocator 停止でも定常収束は継続し、止まるのは新規採番だけ(waiting-allocation・下流 skip)。プール分割 by claim で中央 allocator 依存を「全シャード共有プール」だけに縮小 |

---

## 判定変更の理由

### §5.8 の一貫性契約が「バルクが正しさを壊す」懸念を「揺らぎ」に降格させた(最大の効き)
v2.4 の新懸念1〜4は突き詰めると「BulkObserve を導入したら、per-resource Read では起きなかった別種の壊れ方(自己矛盾した観測・削除誤判断・古い状態の持ち越し)が生じるのでは」という一点だった。§5.8 はこれを**5つの明文化**で構造的に潰す:(1) 非アトミック宣言で「走査窓の非一貫は正しさでなく揺らぎ」と位置づけ、毎周全量再観測(§6.2)に吸収を委ねる。(2) complete-or-error で部分結果の黙示返却を禁止(§8.6-a と同規律)。(3) 型分離を SDK で型レベル強制し、prune 流用を封じる。(4) 走査窓内遷移を managed 除外(§11.2-4)+ converging/settle(§10.4)という**既存機構**に接続——新しい吸収装置を要求しない。(5) SDK 等価性テストで「バルク≡個別」を CI 保証。DRS/HA が常時配置を動かす本構成はまさに §5.8 の想定ケース(04 指摘由来)であり、除外設計さえ正しければ「バルクの中で自己矛盾した drift」は立たない。観測 state レス思想も `observed_at` の周期越え禁止で保全されている。

### §16.7 の churn 算術が「衛星がスケール天井」を数値で否定した
v2.4 の新懸念5は「早期収束(採番が UX 直結)+ 静的シャード(シャード数だけ衛星 QPS 増)の両方が allocator/Vault に合流し、月500件・日20件超の申請がここで律速する」だった。v2.5 は**部品(HA・キャッシュ)でなく原則**で塞ぐ。核心は「割当事実は Release まで不変であり、`Lookup` はシャードローカルに無期限キャッシュ可(S3・喪失=allocator 問い直しだけ)」。これにより定常収束は**衛星コールゼロ**で回り、allocator QPS は**フリート規模でなく churn に比例**する。8,000VM でも日次 churn 数%で新規採番は日 200〜300件オーダーに収まり、早期収束バーストは人の申請レートで上限される。さらに §16.8(waiting-allocation)で allocator 停止時も定常収束は止まらず、止まるのは新規採番だけ(converging 類似・下流 skip・エラーでも halt でもない)。§19.9 の停止影響表が「allocator 停止=新規作成のみ停止/Vault 停止=secret Apply のみ alert-only 縮退/v1 は broker 無しで全衛星落ちても維持は回る」を1枚で示す。**結果、v2.4 で前景化した「allocator が UX 律速」は churn 算術で緩み、HA(§16.5・段0 VM-HA 上 active/standby + fencing 再利用)は「変化の可用性」のための投資と位置づけが確定した。** allocator 専用ストアも不要(S2 quorum ストアへ相乗り可)で新しい中心依存を増やさない。

### 本構成では I群(git 配布・S2-free シャード)は間接効果にとどまる
CHG-I群(sparse checkout・ミラー中継・S2-free 階級)は 300 拠点級の地理分散(構成14)への応答であり、単一DC の本構成には直接効かない。ただしプール分割 by claim(§16.5・§12.5 と同思想)は縦割り組織境界のシャード分割を補強し、共有基盤プール依存を縮める点で間接的に効く。

---

## 残存(v2.4 から動いていない・想定どおり)

1. **FC ゾーニング / PowerMax provider 高難度** — v2.5 は BulkObserve 一貫性・衛星の天井・claim 配布に集中し、FC/ストレージ層には触れていない。依然 Ansible ラップ + 永久 alert-only 天井(§23.7-4)。**未解消(領域固有)。**
2. **意味的 CMDB 腐り(件数正常・値誤り)** — §7.6 基数検知どまりで、Observe 突き合わせによる意味的異常検知は v2.5 でも設計外。CMDB を desired 権威にしない運用回避が前提のまま。**未解消(設計外)。**
3. **ITIL / CAB との思想差(組織問題)** — 早期収束(§6.8)+ remediation schedule で「承認→即反映」の技術面はさらに整うが、「宣言を承認する」文化転換(§23.5)は技術で解けない。**未解消(組織課題)。**

いずれもスケール・レイテンシ・衛星可用性のいずれでもなく、領域固有難度 or 組織課題。v2.4 の残存3点と完全に一致し、動いていないことを確認した。

---

## この構成で成立させるための条件(v2.5 更新版)

1. **brownfield 導入戦略(§23)は不変で厳守** — 段階0 全域 alert-only + prune 禁止 → import → Sync Mode で CMDB 機械是正 → scope 単位で「managed 除外後 diff ゼロ N 日」で昇格。ハイパーバイザ中核・FC/ストレージは永久 alert-only 可(§23.7-4)。**BulkObserve/§5.8/衛星原則があってもこの順序は変えない。**
2. **v1 スコープ(v2.5 で追記)** — v1a は BulkObserve 型切り+等価性テスト seam・`--claim` 型切り・Watch+scoped run・**衛星原則(§16.7)の型的帰結(Lookup キャッシュ=S3・waiting-allocation・observed_at)**。v1b は **BulkObserve 実装 + §5.8 一貫性契約 + 等価性テスト**、静的シャーディング実動作、**allocator HA 最小形 + Lookup キャッシュ + waiting-allocation 縮退 + プール分割 by claim**(CHG-Z2 §24.1b)。v1.5 は最小 Workflow + 承認ゲート + remediation schedule。動的ルーター/自動リバランス・broker のみ v2。
3. **vCenter Provider は BulkObserve を第一級かつ §5.8 準拠で書く** — PropertyCollector 使用。managed 除外(current_host/powerState/DRS 予約)を Schema で厳密宣言し、**SDK 等価性テスト(`BulkObserve≡{Read}`)を通す**ことを「完全+bulk」の名乗り条件に。**バルク結果を prune 判断に流用しない**(型で封じられるが運用者にも徹底)。`observed_at` の周期越え再利用禁止を CI で担保。走査窓内 DRS 移動は managed 除外 + converging/settle に落とす設計を fake provider 検証で確定。
4. **外部収束器を宣言で縁切り(v2.3 踏襲)** — VM 配置=`external_owner: drs`、ACI=`external_owner: aci`。APIC には粗い intent のみ、realization は §10.4 Tier1 + deadline に委譲。DRS 縁切りは §5.8 の走査窓内遷移吸収の前提でもある。
5. **申請VM経路を早期収束 + churn ベース sizing で組む** — tenant 月500件は authority + quota の standard change 枠、PR マージ=Watch ヒント=秒オーダー反映。CAB 変更窓は `remediation.schedule` で auto、繁忙期は freeze。**allocator sizing は「フリート規模」でなく「churn + ポータル申請ピーク」で行う(§16.7)。** 定常はシャードローカル Lookup キャッシュで allocator に触れないことを容量前提に。allocator は段0 VM-HA 上 active/standby + fencing(§16.5)で「変化の可用性」を守る(維持は allocator 停止でも回る)。
6. **シャード境界=縦割り組織境界** — cluster/site + tenant をシャード軸兼 ownership 境界に。共有基盤(ACI/FC/PowerMax)は専用 platform シャードが単一 writer(fencing 必須)で所有、他は read-only。**プール分割 by claim(§16.5)でサイト固有 VLAN レンジを disjoint 宣言**し、中央 allocator 依存を全シャード共有プールだけに縮める。静的シャーディング(v1b)で足りる規模なので動的化(v2)は原則不要。
7. **CMDB は desired 権威にしない(v2.3 踏襲)** — allocator を採番権威に分離、CMDB は §7.6 急減検知付き read 限定 + Sync Mode 是正先。意味的腐りは Observe 真実源で上書きし続ける運用で吸収。

**総括**:v2.5 は v2.4 が解消と引き換えに生んだ**新懸念5点(バルク一貫性4 + 衛星 QPS 1)を、SDK 契約(§5.8)と設計原則(§16.7「衛星の停止は変化を止めるが維持を止めない」)で構造的に消した**。とりわけ §5.8 は新しい吸収機構を作らず既存の managed 除外 / converging・settle / 毎周再観測に接続し、§16.7 は churn 算術で「衛星がスケール天井」という誤直観を数値で否定した。結果、本構成の ○ を左右する要因は**もはやスケールでもレイテンシでも衛星可用性でもなく、FC/ストレージ provider 難度・意味的 CMDB 腐り・ITIL 文化転換の3点(すべて領域固有 or 組織課題)に純化**した。「可視化・CMDB是正・新規クラスタ/テナントの自律化・保守窓 auto」に限定した ○ は v2.4 よりさらに確度が高く、「既存8,000VM全域の即時 auto 化」を目標にしても △ への逆戻りリスクはこの3点に完全に絞られる。
