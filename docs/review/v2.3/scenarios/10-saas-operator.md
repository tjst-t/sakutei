# 適用性レビュー 10(v2.3 再評価): SaaS事業者の自社サービス基盤

**新判定: ◎ に近い ○ 条件付き成立(前回から実効改善)**
**前回判定(v2.2時): ○ 条件付き成立**

構成前提は前回 §1 のまま(k8s 3クラスタ60ノードはArgoCD、Sakutei は k8s の土台と k8s 外データ層〈MySQL20/Redis6/ES5/Kafka5〉+物理/Proxmox/Cisco/FW)。境界線「k8s API の中=ArgoCD / 外と土台=Sakutei」は v2.3 でも不変で、むしろ §11.4 の新設で外部所有境界が第一級化され、この構成の肝である「もう一つの自動機構(MHA/DRS)との共存」の型が強化された。総合として **判定の記号は据え置きだが、v2.2 で条件A(v2待ち)に押し込まれていたデータ層運用の一部が v1.5 に前倒しされ、条件B(MHA協調seam)に設計上の受け皿ができた**ため、実効的な成立度は前回より一段上がった。

---

## ギャップ解消表

| 前回ギャップ(§7) | v2.3 該当変更 | 判定 | 根拠 |
|---|---|---|---|
| **G1**: v1にTask/Workflow無し、データ層の肝(failover/join/rotation/DR/ローリング更新)が全てv2待ち | **CHG-C1**(§24.1.5 v1.5) | **部分解消** | 単純Task + 最小Workflow(直列one-at-a-time + invariant + 手動承認ゲート)が v2 → **v1.5 前倒し**。MySQL計画failover・ES/Kafka join・minorローリングの「骨格」は v1.5 で表現可能に。ただし多相Task状態機械・動的select・自動canary・自動段階ロールアウトは **v2 据え置き**。rotation実行もbroker=v2依存(CHG-A2)で v1.5では張れない |
| **G2a**: MHAの縁切りはできるが協調不能。MHA→Sakuteiのイベント連携seam未定義 | **CHG-D2**(§11.4) | **部分解消** | (b)協調seamを新設:外部の収束結果を `ready_condition` で **Observe して待つ一方向連携**。「MHAのfailover完了をObserveし下流gateにする」が例示され、まさにこの構成の要求(新master前提の下流追随)に一致。seam型は v1a、外部固有の写像実装は v1b〜。ただし **一方向(Sakuteiから指示は出さない)** に閉じるため、MHA未完了中の中間状態のfailover trigger/記録統合は依然範囲外 |
| **G2b**: MHAに「作業中」概念が無く converging Tier1が使えず Tier2/3の床頼み | 変更なし(§10.4 既存) | **未解消(設計上は許容)** | §10.4のTier2(settle窓)/Tier3(oscillating自動alert-only隔離)は v2.2 から不変。MHA hookからの「作業中」通知を受ける口は新設されず、床頼みの構図は残る。ただし §11.4(a)`external_owner` で MHAが触るscope/種別を **scope単位で明示除外** できるようになり、フィールド単位managedより宣言の粒度が上がった分、誤検知面の管理は改善 |
| **G3**: ES/Kafkaのquorum membershipはResourceでもTaskでもなくWorkflow、v1では観測のみ。切り分け基準が設計書に無い | **CHG-C1**(§24.1.5) + §20.2/§11.3 | **部分解消** | 「クラスタへのホストjoin = Workflow(invariant=quorum維持)」が §20.2 表・§9.2 例(`quorum: maintained`)で明記され、v1.5 の最小Workflowで **join骨格が組める**。切り分け基準も「産む/join = Task/Workflow、Day-2維持 = Resource」(§20.2最終行)+ §11.3の容量プール型で補強。ただし **ES shard rebalance / Kafka ISR 固有の参照実装は依然不在**(前回要求「member_of + Workflow join の参照実装」は未提供)。動的selectが v2 のため v1.5 Workflowは直列固定 |
| **G4**: TF/Ansible移行(§23 Brownfield) | **CHG-A2/D4**(import・Sync Mode継続) | **解消(維持〜微強化)** | §23.3 import + §23.8 Sync Mode は v1b で維持。§23.9(CHG-D4)`observability: none`/`ledger_only` の観測不能資産の型が新設され、当面手運用のscopeを台帳上 `unknown` のまま第一級で持てる。このシナリオの「MHA/ES/Kafkaは当面手運用が正常」に更に素直 |
| **G5**: k8sノード土台とArgoCDの境界が実運用で滲む(managed fieldsで分ける負担) | **CHG-D2**(§11.4 a) | **部分解消** | k8s operator/DaemonSet が触るフィールドを `external_owner` で **scope単位除外** できるようになり、フィールド単位managedの手作業が減る。ただし同一ノード内の細かい境界設計判断は運用者側に残る |
| **G6**: provider実装工数(特にSchema設計のmanaged/ordered/identity正しさ) | **CHG-C3**(§5.2/§5.5) | **部分解消(むしろ正直化)** | Schema lint が「あると良い」→ **ロード拒否レベルで強制**(managed未宣言・content-addressにidentity不可・secret平文拒否等)。golden path雛形(REST CRUD/Ansibleラップ)同梱。「managed宣言ミス=MHAと永久に喧嘩」を型で早期に潰せる。ただし工数の絶対量は減らず「4関数+Schema、Schemaが肝」と訴求が正直化されただけ |

---

## 判定変更の理由

記号は **○ 据え置き**だが、前回「◎に近い成立(土台)/ v2待ちの○(データ層)」の二段構えのうち、**データ層側の底上げ**が起きた:

1. **条件A(v2待ち)の緩和**: 前回「データ層を真に管理下に置くのは v2 以降」としたが、CHG-C1 で failover/join/ローリングの **骨格が v1.5 に降りた**。「1年以上先の v2 を待つ」から「v1.5(v1a→v1b→v1.5 の順)を待つ」に短縮され、投資判断のリスクが下がる。ただし *骨格* であって、多相Task・動的最適化・rotation実行は依然 v2。

2. **条件B(MHA協調seam)に受け皿**: 前回「拡張要求」として挙げた MHA→Sakutei 連携が、§11.4(b) の `ready_condition` 一方向協調 + §11.4(a) `external_owner` 除外宣言という **2つの最小概念で第一級化**された。新プリミティブ無しで既存の「観測ready待ち」機構を再利用する解法で、設計の一貫性も保たれている。

3. 一方 G2b(MHAの作業中不可視)・G3のES/Kafka固有参照実装・rotation実行 は未解消のまま。したがって **◎への昇格には至らず、○のまま実効改善**という評価が最も正確。

---

## v2.3 で新たに気づいた問題・残存課題

1. **協調seamが一方向に閉じることの帰結**: §11.4(b) は「Sakuteiから外部へ指示は出さない」と明示的に片方向。よって **MySQL計画failover(Sakutei が MHA/切替を能動trigger したいケース)は §11.4 では表現できない**。これは v1.5 の Workflow ステップ(`failover_from` §9.2 例)側で MHA スクリプトを Task として呼ぶ形になるが、その Task と MHA の *自動* failover が二重に走らない排他(MHA が勝手に動く最中に Workflow が failover を叩く)は設計書に明示が無い。**運用者が in-flight 排他(§6.4)と external_owner の関係を自前設計する必要**が残る。

2. **`external_owner` 除外と Workflow の緊張**: MHAが触るrole/masterを `external_owner: mha` で除外(Apply抑止)する一方、計画failoverでは Sakutei主導でrole を動かしたい。**「平常時は除外・計画作業時のみ Sakutei が触る」の切替**が宣言的に書けるか(remediation schedule §10.8 で近似できるが scope除外とmodeは別軸)は v2.3 でも未整理。この構成では実装時の設計判断として残る。

3. **rotation の穴が残置**: CHG-A2 で credential-broker が **v2 確定**、v1は SecretRef 直結。前回指摘した「VaultでrotateしたのにMySQLクライアントが古い=接続断」は §14.2 の遅延展開で render 側は救えるが、**rotation の *実行*(実際に新資格へ切替える手続き)は broker=v2 待ちのまま**。データ層の資格 rotation はこの構成の運用上重い項目で、v1.5 に前倒しされていない。

4. **ES/Kafka の直列制約**: v1.5 Workflow は **直列(one-at-a-time)+ 動的selectは v2**。5台規模なら直列join/leaveで実用上足りるが、shard rebalance/ISR再構成の途中状態を「隣が今healthyか」で動的判断する部分(§9.2 の `select: least-loaded-first` は v2)は v1.5 では固定順序に退化する。quorum invariant 検証自体は v1.5 で効くため致命的ではない。

5. **累積削除予算がデータ層に効くのは好材料(新規の利点)**: CHG-B1(§10.2 destructive_budget + halt-all)+ CHG-B2(§8.6 List完全性/件数急減halt)で、前回条件C「1コミットでMySQL20台に破壊的変更を止める生命線」が **瞬間量だけでなく24h累積・List欠損まで塞がれた**。前回「毎周10件ずつ50周で全消し」型の穴が閉じ、データ層scopeの安全性は明確に向上した。

---

## この構成で成立させるための条件(更新版)

- **A'(緩和)**: データ層の failover/join/ローリング更新は **v1.5**(単純Task+最小Workflow)まで待てること。多相Task・動的最適化・rotation実行・DR自動化は依然 v2。導入は「土台〜k8s外周辺(v1a/v1b)→ データ層手続き(v1.5)」の順で、v1a/v1b の可視化・drift alert 価値を先に取る。
- **B'(受け皿あり)**: MHA/ES/Kafka の縁切りは §11.4(a) `external_owner`(scope単位除外)+ §11.2-4 managed(フィールド単位)を **二段で厳密に**宣言し、協調は §11.4(b) `ready_condition` の一方向Observe待ちに閉じる。**計画failover時の「除外の一時解除 × MHA自動failoverとの排他」を運用者が設計**すること(v2.3 では宣言的に未整理な残タスク)。
- **C(強化・堅持)**: データ層scopeは alert-only + `pruneable: false` + `halt-all` + **destructive_budget(24h累積)** + **List完全性検証**を堅持(CHG-B1/B2 で床が上がった)。auto昇格は「managed除外後の diff ゼロがN日」を満たした土台/周辺(DNS/observability/LB/staging)から。
- **D(新規)**: 当面手運用が正常な scope(MHA協調の細部・ES shard・Kafka ISR)は §23.8 Sync Mode + §23.9 `observability: none`/`ledger_only` で **台帳上 unknown のまま第一級**に持ち、prune巻き添えを防ぐ。
- **E(据え置き)**: Cisco/FW/データ層 provider の Schema(managed/ordered/identity)を CHG-C3 の **ロード拒否lint + golden path**を活用して厳密に。「managed宣言ミス=MHAと永久に喧嘩=事業停止」は依然この構成最大の実装リスク。

**結論**: 土台層(物理/Proxmox/NW/FW)+ k8s外周辺は v1a/v1b で **◎ に近い成立**、データ層は v2.2 の「v2待ちの○」から **v1.5待ちの○(骨格は組める)** へ前進。ES/Kafka固有の参照実装と rotation実行・双方向MHA協調が揃えば ◎。「既存全部を管理下に」ではなく価値の出る範囲から(§23.7)の正着は不変。
