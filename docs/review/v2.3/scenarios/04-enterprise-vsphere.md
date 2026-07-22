# 対象構成04: 単一DC大規模仮想化基盤(エンタープライズ)Sakutei 適用可能性 再評価(v2.3)

**新判定: △→○(条件付き成立)に前進。「まず可視化・CMDB是正・新規部分の自律化」に限定すれば ○、既存8,000VM全域の即時 auto 化を目標にすると依然 △**

**前回判定(v2.2 時): △ 大幅な拡張が必要(alert-only / brownfield 導入なら ○ に届く)**

前回は「8,000VM の単一デーモン天井」「ACI/APIC 二重収束」「ITIL/CAB 整合」「腐った CMDB」を主要摩擦点として △ に落とした。
v2.3 は本構成に効く変更(スケール算術の実機書き直し・外部オーケストレータ境界の第一級化・remediation schedule・
incomplete≠empty 防御・v1 再スライス)を集中的に投入しており、**方向性の正しさに加えて「何が v1 で使えるか」が明確化**した。
ただしシャーディング自体は v2 のままであり、天井の読み替えは判定を変えるが「単一デーモンで 8,000VM を賄える」を意味しない。

---

## ギャップ解消表

| # | 前回ギャップ(§7 番号) | v2.3 該当変更(CHG-ID / 章) | 判定 | 根拠 |
|---|---|---|---|---|
| 1 | 単一デーモン天井を明確に超える(8,000VM+下位で v1 単一が破綻) | CHG-C2 §12.2(実機レイテンシ表・階級別天井・tier別頻度=v1b)/ シャーディング=v2 | **部分解消** | 天井が「vCenter 0.8s→30s間隔で〜200台/デーモン」と定量化され、tier別間引き最小形が v1b に前倒し。だがシャーディング本体は v2 のまま。**算術は下記「判定変更の理由」参照** |
| 2 | vSphere に watch 前提が無ければポーリング一択 | CHG-C2 §12.2(watch併用=v2、tier別頻度 seam=v1b) | **部分解消** | tier別 Observe 頻度が seam として v1b に入り、安定VMを間引ける余地。watch(property collector)併用は v2 で実装検証課題として残存 |
| 3 | ACI/APIC 二重収束の deadline 設計が未成熟 | CHG-D2 §11.4(external_owner・協調 seam)+ §10.4(converging 3層は既存) | **部分解消** | `external_owner: aci` で APIC 所有 scope を Apply 除外し二重管理を明示的に断てる第一級概念が入った。realization の deadline 値・部分 realization 扱いは §26.3 C(運用パラメータ)に留まり参照実装の詰めは残る |
| 4 | CMDB「到達可・中身誤り(腐り)」を検知できず誤 desired を静かに生む | CHG-B4 §7.6(incomplete≠empty)+ CHG-A5 §7.5 集約 + §23.8 Sync Mode | **部分解消** | 「200 OK だが歯抜け/前回比急減」は §7.6 の急減検知(v1a)で塞がれ、prune 巻き添えを回避。ただし**「件数は正常だが値が誤り」型の意味的腐りは依然未検知**。Sync Mode で CMDB を Observe 真実源から是正する用途は正の方向に強化(§23.8) |
| 5 | ITIL/CAB と level-triggered 自己修復の思想差(組織問題) | CHG-D3 §10.8(remediation schedule=v1.5)+ §10.5 承認ゲート + §23.5 | **部分解消** | 保守窓のみ auto/他は alert-only を宣言化でき CAB の変更窓に直結。承認ゲート・PR=RFC の橋渡しも v1.5/v1a で揃う。だが「宣言を承認する」文化転換は §23.5 が明言する通り技術で解けない残課題 |
| 6 | FCゾーニング/PowerMax provider が高難度・自前化コスト大 | 変更なし(§21.3/§21.4 は v2.2 踏襲) | **未解消** | v2.3 は本ギャップに触れず。Ansible ラップで横断保証が落ちる構図(§21.3)は不変。FC/ストレージ層は当面 alert-only+可視化が現実的 |
| 7a | 承認ゲート/段階適用が v2 送り | CHG-C1 §24.1.5(最小 Workflow+手動承認ゲート=v1.5) | **解消** | 直列+不変条件+手動承認ゲートが v1.5 に前倒し。ローリング ESXi/退役 Workflow の骨格が v1 で組める |
| 7b | シャーディング/分散 Executor が v2 送り | §12.2/§12.3(v2 のまま。ただし fencing フィールドは型に前倒し) | **未解消(緩和)** | 分散本体は v2 継続。CHG-B6 で割当表/platform シャードの fencing token を型定義時に持たせる決定が入り、後付け危険は先回り |
| 追 | inventory の妥当性検証(前回 (c) 設計ギャップ) | CHG-D4 §23.9(observability:none / ledger_only)+ §7.6 | **部分解消** | 観測不能資産を台帳上 unknown のまま第一級化し prune 巻き添えを型で防止(v1a 型)。腐り CMDB の意味検証そのものは §7.6 の基数検知どまり |

---

## 判定変更の理由

### スケール算術の書き直しが単一デーモン天井評価をどう変えるか
v2.2 は「1万×200ms/並列16≈125秒/周」の単一値で語り、前回はこれを根拠に「VM だけで天井付近」と △ の主因にした。
v2.3(CHG-C2 §12.2)は **vCenter/govmomi=0.8s** の実機値を採用し、「30s 間隔・並列16・Observe 予算 10s」で
**単一デーモンあたり約200台**と読み替えた。これは前回想定より天井が**低く**、8,000VM を単一で回すのは v2.2 想定以上に非現実的
であることを裏づける(むしろ「単一では無理」がより明確に定量化された)。**したがってシャーディングが v2 のまま前提であることは変わらない。**
一方で、(a) tier別 Observe 頻度の最小形が v1b に前倒しされ、安定した本番 VM を「N周に1回」に間引けば実効天井を数倍押し上げられる、
(b) 天井が「階級別(完全/標準=数千〜1万、Ansible/CLI=数百〜千)」と明示され、cluster/tenant シャード内を高速 provider で束ねれば
数千規模のシャードが成立見込み — の2点で**設計の見通しが立った**。前回は「破綻する/v2 が必須」で止まっていたのに対し、
v2.3 は「単一では約200台/デーモン、間引きで数倍、超過分はシャード」という段階が読める。天井評価は「悪化(=正直化)」したが、
**回避経路(間引きの v1b 前倒し+シャードの型準備)が明示された**ため、総合判定は前進する。

### 外部オーケストレータ境界の第一級化(DRS/ACI)
前回は DRS の managed fields も APIC の converging も「§11.2/§10.4 で対処可能だが除外の明示概念が無い」と評した。
CHG-D2 §11.4 が **`external_owner`(scope/種別単位の Apply 除外)** と **協調 seam(外部の結果を ready_condition で Observe して待つ一方向連携)**
を第一級化した。本構成では `external_owner: drs`(VM 配置)・`external_owner: aci`(ファブリック realization)を宣言的に固定でき、
「二重管理事故の主因」を型で断てる。これが判定を押し上げる最大要因の一つ。加えて DRS は §11.4 の除外(scope)と §11.2-4 の
managed fields(フィールド)の**二段**で綱引きを回避でき、前回「Schema が本体」とした難所の設計余地が広がった。

### remediation schedule と CAB/ITIL の整合
CHG-D3 §10.8 で保守窓連動の mode 自動切替が v1.5 に入り、「Sat 02:00-04:00 のみ auto、他は alert-only」を宣言できる。
これは CAB の変更窓・繁忙期凍結と直結し、前回「手動 freeze しかなくヒューマンエラー面が残る」を実質解消。承認ゲート(v1.5)と
PR=RFC(platform 層)、standard change 枠(authority+quota)の3段橋渡しが v1 スコープ内で揃い、ITIL との**技術的**整合は成立する。
残るのは思想差の受容という組織課題(§23.5)のみ。

### CMDB「到達可・中身誤り」問題
前回の核心指摘「到達できるが中身が誤っている腐りは検知できない」に対し、v2.3 は **§7.6 incomplete≠empty**(前回比 −50% 等の急減を
不達扱いで前回保持へ倒す、v1a)で「歯抜け/急減」型を塞いだ。さらに §23.8 Sync Mode が Observe を真実源に CMDB を機械的に是正する
用途を第一級化し、前回レポートが「最も価値が高い」とした CMDB 是正ツールとしての入り方が設計に定着した。ただし
**「件数は正常だが値が誤り(古い所有者・誤コスト区分がそのまま存在)」型の意味的腐りは基数検知では捕まらず**、依然
「CMDB を desired 権威にしない」運用回避が前提。ここは部分解消。

---

## v2.3 で新たに気づいた問題・残存課題

1. **tier別間引きと drift 即知のトレードオフが本構成で顕在化**:DRS/HA が配置・電源を常時動かす環境で安定VMを「N周に1回」に間引くと、
   本物の drift(誤削除・意図せぬ電源断)の検知が数周(=数分〜十数分)遅れる。§12.2 は「許容」とするが、8,000VM 規模では
   間引き対象の選定規律(何を安定とみなすか)が運用設計の新たな急所になる。v1b の Observe スケジューラ seam の設計品質に依存。
2. **external_owner の除外と capacity gate の相互作用が未整理**:VM 配置を `external_owner: drs` で除外しつつ、§10.2 の capacity gate は
   クラスタ使用率を Observe して halt する。DRS がリバランス中の一時的な偏りを gate が誤発火する可能性があり、除外 scope と
   容量観測の境界(何を見て何を見ないか)は本構成固有の詰めが要る。
3. **シャード fencing の運用負荷**:CHG-B6 で platform シャード/割当表に fencing token が必須化された(良い前進)が、
   縦割り10名・ITIL 統制下で世代番号・fence の運用手順(誰がいつ token を発行/失効するか)は新たな段0 相当の統制対象になる。
4. **§7.6 の急減閾値(既定 −50%)が 8,000VM 規模で粗い**:大規模退役や tenant 一括削除の正当な変更で誤発火しうる一方、
   数百VM 単位の腐りは 50% 未満で素通りする。scope 別の閾値調整(§26.3 C)が運用前提になる。
5. **FC/ストレージ層は v2.3 で一切前進せず**(ギャップ6 未解消)。本構成のストレージ横断保証は依然 Ansible ラップ+alert-only 天井。
6. **意味的 CMDB 腐りの残存**(ギャップ4 の未解消部分)。Observe との突き合わせ異常検知は設計に入らず、運用回避のまま。

---

## この構成で成立させるための条件(更新版)

1. **brownfield 導入戦略(§23)を厳守**:段階0 全域 alert-only+prune 禁止 → §23.3 import(managed 候補自動サジェスト活用)→
   Sync Mode(§23.8)で数ヶ月同期しつつ CMDB を機械是正 → scope 単位で「managed 除外後 diff ゼロ N 日」で昇格。
   ハイパーバイザー中核・FC/ストレージは**永久 alert-only 可**(§23.7-4)。
2. **v1 スコープの実装対象を正しく取る**:v1a(コア型・per-resource 原因タグ・rate limit 累積予算・incomplete≠empty・
   external_owner の Apply 抑止・observability:none 型)+ v1b(import・Sync Mode・tier別頻度 seam・capacity gate・ready_condition)+
   v1.5(最小 Workflow+手動承認ゲート・remediation schedule)。**シャーディング/分散 Executor(v2)は「数千超のシャードが要る段」
   まで先送り**でよいが、割当表の fencing フィールドは型定義時に持たせる(CHG-B6)。
3. **外部収束器を宣言で縁切り**:VM 配置=`external_owner: drs`、ACI ファブリック=`external_owner: aci`。APIC には粗い intent のみ渡し
   realization は §10.4 Tier1+deadline に委譲。deadline 値・部分 realization 扱いは参照実装で早期にチューニング(NW チーム不信の予防)。
4. **CAB/ITIL 整合を宣言に落とす**:platform=PR(RFC)、tenant 月500件=authority+quota の standard change 枠(枠超えのみ CAB)、
   保守窓は `remediation.schedule` で auto、繁忙期は freeze。drift 自己修復は承認後に段階昇格(§23.5 の信頼獲得と同型)。
5. **CMDB は desired 権威にしない**:allocator(NetBox/Infoblox/最小自作)を採番権威に分離。CMDB は §7.6 急減検知付きの read 限定+
   Sync Mode 是正先。意味的腐りは Observe 真実源で上書きし続ける運用で吸収する。
6. **間引き規律とシャード境界を組織の縦割りに写す**:cluster/site+tenant をシャード軸兼 ownership 境界とし、共有基盤(ACI/FC/PowerMax)は
   専用 platform シャードが単一 writer(fencing 必須)で所有、他は read-only 参照。

この条件下では、**「可視化・CMDB是正・新規クラスタ/テナントの自律化・保守窓 auto」に限定した ○(条件付き成立)**として v2.2 時より
確度高く価値を出せる。「既存 8,000VM 全部を単一/即時 auto 収束下に置く」を目標にすると、シャーディング未実装(v2)と
FC/意味的腐りの未解消により **△** に戻る — これは §23.7 が戒める目標設定である。
