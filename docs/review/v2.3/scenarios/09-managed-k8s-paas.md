# 対象構成09: マネージドKubernetes/PaaSプロバイダ — Sakutei適用可能性レビュー(v2.3 再評価)

**新判定: △→○ 条件付き成立(境界宣言・段階ロールアウトの縮退運用が条件。full商品化はv2依存が残る)**

**前回判定(v2.2時): △ 大幅な拡張が必要(条件付き成立の芽はある)**

v2.3 は前回の3大論点(外部operatorとの二重ループ境界、v1スコープ不足、prune巻き添え)にほぼ正面から手を入れた。とくに **CHG-D2(外部オーケストレータ境界の第一級化)** が構成09固有ギャップの中核を、**CHG-C1(v1.5 最小Workflow前倒し)** が「クラスタを作れるが回せない」問題の骨格を埋めた。一方、canary的な**自動**段階ロールアウト・自動ゲート・credential-broker・シャーディングはv2据え置きで、「full商品化」には依然v2待ちが残る。よって「殻のライフサイクル運用」までは○に格上げ可、「マルチクラスタ自動運用商品」としてはv2条件付き、という二段判定になる。

---

## 1. ギャップ解消表

| 前回ギャップ(§参照は前回レポート) | v2.3該当変更 | 判定 | 根拠 |
|---|---|---|---|
| §7-1 二重ループの正面衝突(operatorのCRを収束すると綱引き)。境界の明示ガイドが設計書に無い | **CHG-D2**(§11.4 `external_owner`)、CHG-B4 補強(§10.4 converging3層) | **解消** | scope/種別単位で「外部所有=Observeするが Applyしない」を宣言的に固定する第一級概念が新設。PG/Redis operator・Cilium・MetalLBの管理scopeを `external_owner` で除外宣言でき、綱引きが構造的に起きない。ロード時検証(prune・循環検出と同入口)も付く |
| §7-1 runtimeフィールドの managed fields 除外を Provider Schema で宣言する作業が重い(CRD数だけ発生) | §11.2-4(managed維持)、CHG-C3(§5.5 Schema lint強制・golden path) | **部分解消** | フィールド単位除外(managed)は健在で、CHG-D2 の scope単位除外で「CRD1本ずつ全フィールドを分類する」必要は激減(operator所有scopeは丸ごと除外できる)。ただし「顧客向けCRの desired specだけ収束・operator書き戻しは除外」という混在フィールドの線引きは残り、Schema設計負担そのものは lint/雛形で軽くなるが消えない |
| §7-2 `k8s_object` の prune が operator派生物(Pod/EndpointSlice)を巻き込む | §8.1 境界宣言(健在)+ **CHG-B2**(§8.6 List完全性・件数急減halt)+ CHG-D4(§23.9 観測不能資産をprune集合から除外) | **解消** | prune scopeを「明示宣言したCRのみ」に限定する境界宣言(closed-world=境界の宣言)は元々あり、v2.3で List完全性検証(complete以外はprune保守側)と件数急減 −50% halt が加わり、operator が動的生成物を List に出し入れしても誤prune前に止まる二重の防御になった |
| §7-3 クラスタ内部を毎周Observeするのは k8s の収束と二度手間・観測stateレスの旨味が薄れる | CHG-D2(b)(§11.4 協調seam=ready_condition一方向)、canary(§17.3) | **部分解消** | 「外部の収束結果を ready_condition として Observe で待ち、指示は出さない」一方向連携が第一級化。内部整合はk8sに委ね外形だけ待つ設計を型で表現できる。ただし canary(能動検証)は依然 **v2**(§24.2)なので、v1系では「疎通・schedulability の能動検証」に寄せきれず、Observeベースに留まる |
| §5 canary的バージョンアップ(least-critical→承認→broad)の段階制御・自動ゲートがv1に無い | **CHG-C1**(§24.1.5 v1.5:直列+不変条件+**手動承認ゲート**)、§10.5、CHG-D3(§10.8 remediation schedule) | **部分解消** | v1.5で「手動承認ゲート付き直列Workflow」が前倒しされ、SRE 6名が canaryクラスタを確認して次段へ進む運用の骨格は成立。ただし **動的select・自動canary判定・自動段階ロールアウトは明示的に v2 据え置き**(§24.1.5末尾)。「1クラスタで自動検証→自動で残りへ」は不可、人手ゲートで刻む形に縮退 |
| §7-4 ローリングUG・シャーディング・DBSource・broker が全てv2 → v1では商品化不能 | CHG-C1(v1.5 Task/最小Workflow)、CHG-A2(broker=v2・SecretRef直結明示)、CHG-C2(tier別Observe=v1b seam) | **部分解消** | Workflow骨格は v1.5に降りた。しかしシャーディング+fencing・DBSource・credential-broker は **v2据え置き(§24.2)** が確定。50→200成長で必然のシャーディングと、tenant即時払い出しのDBSourceが無い間は「マルチクラスタ商品」の規模運用は組めない |
| §5 ローリングUGのPDB/drain無限ブロック→顧客起因でSREのUGがhalt | §9.2 on_failure:halt(既定・健在)、§10.5 承認ゲート | **部分解消** | drainのtimeout→haltで「止まる」防御は元より効く。v1.5の手動承認ゲートで「強制evict判断を人に上げる」経路は表現可能になった。ただしdrain timeout超過を自動で承認フローへ昇格させる専用機構は明文化されておらず、運用手順側の設計に依存 |
| §5-1/§5安全 クラスタ削除(子VM+etcd+顧客データ)の巻き添え | §7.5合成子独立(健在)+ §10.2 **CHG-B1**(累積削除予算・halt-all)+ **CHG-B6**(platform破壊的大量変更はAタグでも承認必須) | **解消(強化)** | 前回の「20件削除でrate limit halt」に加え、24h窓の `destructive_budget`・単一scope削除進捗率(`max_scope_fraction`)・「毎周10件×50周の漸進全消し」封じが入り、多クラスタ規模の削除暴走に有効。さらに platform破壊的大量変更の承認必須化で、侵害reconcilerがAタグで素通りする経路も塞いだ。顧客データ持つクラスタ削除に最も効く強化 |
| §4 credential-broker断=全クラスタ全盲(50→200の目) | CHG-A2(broker=v2明示・v1はSecretRef直結)、CHG-B3(§19.7 段0自己収束の除外・Vault断緩和) | **部分解消** | v1はSecretRef静的資格で凌ぐ方針が明確化し、broker前提の矛盾は解消。ただし broker本体はv2のままで、JIT発行による全盲回避は将来依存。段0側の自傷(broker到達資格を収束対象から除外)は §19.7 で手当て済み |

---

## 2. 判定変更の理由(△→○ 条件付き)

前回△の主因は3つ ——(a) 二重ループ境界の設計ガイド不在、(b) 商品中核のv2集中、(c) 内部観測の旨味減 —— だった。v2.3 で:

- **(a) は CHG-D2 で構造的に解消**。`external_owner` により「operatorが所有するscopeは Observeするが Applyしない」を宣言でき、Schema設計で全CRDのフィールドを1本ずつ分類する重労働を、scope丸ごと除外に置き換えられる。前回レポートが「構成09固有の設計ギャップ」と名指した線引きが第一級概念になった意味は大きい。**これが△→○格上げの最大要因**。
- **(c) も CHG-D2(b) の協調seam(ready_condition一方向)で骨格が入った**。内部整合はk8sに委ね外形とreadyだけ待つ、が型で表現できる。
- **(b) は v1.5前倒しで部分緩和にとどまる**。最小Workflow(直列+不変条件+**手動**承認)は「殻のローリングUG」を成立させるが、自動段階ロールアウト・自動canary・シャーディング・DBSource・brokerは明示的にv2据え置き。したがって「殻のライフサイクル運用基盤」としては○、「マルチクラスタ**自動**運用商品」としてはv2条件付き、という二段判定にする。純粋な○(そのまま成立)には至らない。

## 3. v1.5最小WorkflowがローリングUGにどこまで届くか(精査)

§24.1.5 の v1.5 Workflowは「直列(one-at-a-time)+ 不変条件 + 手動承認ゲート」の3要素。構成09のノードローリングUGへの到達度:

- **届く**:cordon→drain(respect_pdb+timeout)→wait→upgrade-kubelet→health-check→uncordon→verify の直列鎖と、`min_active: N-1`・`etcd_quorum`・`pdb_respected` の不変条件は v1.5 の表現力に収まる。PDB尊重は「drain APIが返るまで待つ+timeout→halt」でk8sに委譲する前回の正解が、そのまま §9.2 on_failure:halt に乗る。
- **届かない**:v1.5は「**動的 select・自動 canary 判定・自動段階ロールアウトは v2**」と明記(§24.1.5)。前回ワークフロー例の `select: worker-first-then-cp`(隣が今healthyかで動的選択)や、全クラスタ規模の §10.5 段階ロールアウトの**自動**ゲートは v1.5 では表現不可。**手動承認ゲートで段間を刻む運用**に縮退する。SRE 6名でクラスタ数十規模なら手動ゲートで回せるが、200→400クラスタ規模で人手ゲートは律速になる → §10.5 自動ロールアウト(v2)依存が残る。

## 4. 外部オーケストレータ境界の第一級化が二重ループ問題をどう変えたか(精査)

前回は「managed fields で縁切りはできるが、除外の**明示概念**と協調seamが第一級で無い」ことが二重管理事故の主因だった。CHG-D2 は:

- **除外の粒度が2段になった**:managed(§11.2-4)=フィールド単位、`external_owner`(§11.4)=scope/種別単位。PG/Redis operator・Cilium・MetalLBのように「operatorが1つのnamespace/CRD群を丸ごと自律管理する」ケースは scope単位除外が正しく効き、CRDのフィールドを個別分類する負担が消える。
- **協調が一方向に閉じた**:§11.4(b)で「外部へ指示は出さない・ready_condition で Observe して待つだけ」と規律化。これはシャード間依存(§12.2)と同一機構の再利用で新プリミティブ不要。operator の自動フェイルオーバ結果を Sakuteiが drift誤認する経路が構造的に閉じる。
- **converging検知(§10.4)が保険**:仮に除外漏れがあっても Tier3(M回Applyしても縮まらない→oscillating→自動 alert-only 隔離)が綱引きを自動検疫する。二重ループの「永久に収束しない」最悪ケースが自動で止まる。

結論:二重ループ問題は「Schema設計で頑張って線引きする」から「scopeを宣言的に除外し、協調は観測待ちに閉じ、漏れは自動検疫する」へと質的に変わり、**構成09の最大ギャップは解消**と判定できる。

## 5. 商品中核機能のv2依存がスライス変更でどう動いたか(精査)

CHG-C1 の再スライス(v1a/v1b/v1.5)で、前回「全部v2」だった中核が三分された:

- **v1.5に降りた**:単純Task・最小Workflow(直列+不変条件+手動承認)・remediation schedule。→ ローリングUG骨格・保守窓auto/alert-only切替(証明書ローテ等の定期作業に有効)。
- **v1bに降りた**:allocator最小(CIDR/ASN採番)・import/Sync Mode・ready_condition/複数Cycle依存・capacity gate・tier別Observe seam。→ 採番とbrownfield導入と規模のObserve間引き基盤。
- **v2据え置き(動かず)**:多相Task状態機械・**自動**段階ロールアウト/自動canayゲート・常設canary・DBSource・credential-broker・シャーディング+fencing・intent UI(catalog)。

つまり「クラスタを作り、殻を直列Workflow+手動ゲートでUGし、削除を安全に止める」までは v1a〜v1.5 で成立する一方、**「自動でマルチクラスタを段階ロールアウト・tenant即時払い出し・数千リソースのシャード分割・JIT資格」という商品スケール要件は依然v2依存**。前回の「v1では実質作れるが回せない」は「v1.5で殻運用は回るが、自動マルチクラスタ商品はv2」へと前進したが、依存の芯は残った。

---

## 6. v2.3で新たに気づいた問題・残存課題

1. **`external_owner` の粒度と「殻の外形は収束したい」の両立**:operator scopeを丸ごと除外すると、operatorのDeployment/CRDの**存在・バージョン**(=Sakuteiが収束したい外形)まで除外に巻き込む恐れ。§11.4は「scope/種別単位除外」だが、「Deploymentは収束・CR runtimeは除外」の混在境界を1つのnamespace内でどう切るかの具体ガイドは無く、構成09では設計判断が残る(部分解消の根)。
2. **canary(能動検証)がv2据え置き**:前回レポートの推奨「内部を毎周収束せず §17.3 canary(API疎通・schedulability・削除経路)で能動検証」の canary自体が §24.2 で v2。v1系では Observeベースの外形確認に留まり、「作成は動くが削除が権限不足で失敗」型の検出は弱いまま。**残存課題**。
3. **手動承認ゲートの規模律速**:v1.5は手動ゲートのみ。200→400クラスタ・SRE 6名では、段間ゲートを人が捌く運用が現実的な律速になる。自動段階ロールアウト(v2)まで、canary→broad の刻みは粗くせざるを得ない。
4. **List件数急減halt(CHG-B2)とk8sの正常な変動の衝突**:operatorがオブジェクトを正常に増減させる際、−50%件数急減 halt が**正常なスケールダウンで誤発火**しうる。prune scopeを明示CRのみに絞れば影響は限定されるが、`external_owner` scopeと List完全性検証scopeの整合を明示設計する必要がある。
5. **drain timeout→承認昇格の自動化欠落**:PDB無限ブロック問題は「halt」までは効くが、「timeout超過を自動で承認フロー(強制evict是非)に昇格」する専用機構は無い。運用手順側の作り込みに依存(部分解消の根)。
6. **broker/シャーディング/DBSourceのv2依存は不変**:50→200成長で必然のシャーディングとtenant払い出しDBSourceが無い間、規模運用の器が組めない点は前回同様に残存。

---

## 7. この構成で成立させるための条件(更新版)

1. **`external_owner` による境界宣言を運用の第一級手順にする**:PG/Redis operator・Cilium・MetalLB の管理scopeを `external_owner` で除外宣言し、Sakuteiは (i) operator の Deployment/CRD の存在・バージョン と (ii) 顧客向けCRの desired spec のみを収束、operator書き戻し runtime は除外 —— という混在境界を構成09の運用規約として明文化する(設計書は概念を与えたが、namespace内の混在切り分けは利用側判断)。
2. **prune scopeと List完全性検証scopeの整合設計**:prune対象を「明示宣言したCRのみ」に厳格限定し、operator派生物・observability:none 資産(§23.9)を List集合演算から除外。件数急減haltがk8s正常変動で誤発火しないよう scopeを合わせる。
3. **v1.5で殻運用を開始し、自動マルチクラスタ運用はv2を待つ二段導入**:v1a(作成・削除安全・累積削除予算)→ v1b(CIDR/ASN採番・import・capacity gate)→ v1.5(直列Workflow+手動承認ゲートでローリングUG・証明書ローテ)。この範囲で「殻のライフサイクル運用基盤」として本番投入可。自動段階ロールアウト・canary・シャーディング・DBSource・credential-broker が揃うv2で「マルチクラスタ自動運用商品」に到達。
4. **段0はProxmox+VM固定を堅持**(§19.7/§19.8で自己収束自傷・コールドスタート・Ceph SPOFは手当て済み)。管理対象k8s上でSakuteiを動かさない規律を守る。
5. **クラスタ健全性検証はv2 canary(§17.3)に寄せる前提で設計**:v1系ではObserveベースの外形確認で凌ぎつつ、削除経路・schedulability の能動検証はv2 canary投入時に移す設計余地を残す。

上記のうち 1〜2 は v2.3 が概念を提供したことで**実現可能**になった(前回は設計書に概念自体が無かった)。3 の二段導入を許容できれば、構成09は **殻のライフサイクル運用基盤として○(条件付き成立)**、full マルチクラスタ自動運用商品としては **v2条件付き** と評価できる。
