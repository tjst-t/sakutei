# 対象構成11:DBaaSプロバイダ(worked exampleのスケール版)— v2.3 再評価

**新判定:◎ 条件付き成立(前回より前進 — v1.5で商品メニュー過半が実装可能に)**
前回判定(v2.2時):**○ 条件付き成立**(ただし商品メニュー過半がv2依存で「v2を実装するか」と等価)

v2.3のスコープ再スライス(CHG-C1)で、この構成の心臓部だったフェイルオーバー・PITRリストアの骨格が **v1.5** に前倒しされ、allocatorが **v1b** に確定した。前回「v1スコープのままでは不成立」だった帯が、v1.5到達で「インフラの面 + 手続きの骨格」まで成立する帯に広がった。合成リソース・削除多層防御・シャード境界の適合は前回同様高いまま、安全性の穴(漸進的全削除・原因タグ相乗り・rendered_revision揺れ)が構造的に塞がれた点が判定を一段引き上げる。

---

## ギャップ解消表(前回ギャップ → v2.3該当変更 → 判定 → 根拠)

| # | 前回ギャップ(§7の指摘) | v2.3該当変更 | 判定 | 根拠 |
|---|---|---|---|---|
| 1 | **v1では動かない** — failover(Workflow)/PITR復元(Task)/allocator/health/canary/DBSourceが全てv2 | CHG-C1(v1a/v1b/v1.5再スライス)、CHG-C2 | **部分解消** | 単純Task+最小Workflow(直列+invariant+手動承認)が **v1.5** に前倒し(§24.1.5)。PITRリストア(Task=migration型)・failover手順(Workflow=rolling同型・§18)がv1.5で書ける。allocatorは **v1b**(§24.1b)。ただし health/常設canary/DBSource/多相Task/自動段階ロールアウトは **v2据え置き**(§24.2) → PITR復元可能性の証明(canary)とlag監視(health)は依然v2。 |
| 2 | フェイルオーバー自動判断はスコープ外 | 変更なし(§18 DR、§11.4 CHG-D2) | **未解消(設計通り)** | 「いつ=外部委譲/どう=Workflow」は不変。CHG-D2の `external_owner` + `ready_condition` 協調seamで、MHA/Orchestrator等を「観測して待つ」一方向連携として第一級化。二重管理事故は減るが、判断ロジック本体は依然Sakutei外。 |
| 3 | role入れ替えとmanaged fieldの緊張(replica_setアイデンティティ設計) | 直接変更なし。§10.4 converging隔離、§26.1 A(ID複合キー・Scope) | **未解消** | 「論理primary非固定」をschemaで一級表現する仕組みはv2.3でも未提供。合成リソースの集約順序に `stuck`/`blocked` が追加(CHG-A5・§7.5)されfailover中の状態表現は精緻化したが、desiredへの不変条件記述の難所は残る。 |
| 4 | PITR/binlogは収束枠外に大きくはみ出す | §18(不変)、health/canary=v2 | **未解消(期待値管理課題)** | 4分割(構成=Resource/鮮度=health/復元可能性=canary/復元=Task)は不変。復元Task自体はv1.5で可能になったが、鮮度health・復元canaryがv2のままなので「戻せることの継続証明」はv2まで手動運用。 |
| 5 | **secret世代解決の毎周負荷 + rendered_revision算出定義未決** | CHG-A3/CHG-B5(§10.3・§26.1 A-3)、CHG-C5 | **解消(定義確定)** | rendered_revision算出定義が **§26.2未決B → §26.1 A-3(着手前決定)に格上げ**。「decision-relevant入力のみの決定的ハッシュ」「TTLヒット/ミスや列挙順で揺れない」を厳密規定と明文化(§10.3 S-02b)。1,600本のE-secret誤爆(幽霊diff)の天井問題に対する設計上の答えが確定。負荷自体はTTLキャッシュ(§7.4)+間引き(§12.2 v1b)で緩和方針も明示。 |
| 6 | tenant層にPR承認を挟む代償(マージ律速) | 変更なし(§12.1/§13.4、catalog=v2確定) | **未解消(組織方針・Sakutei外)** | catalog(intent UI)がv2と明示(CHG-A5)されたため、承認自動化=上位プラットフォーム側の作り込みという線引きは前回のまま。 |
| 7 | Ansible provider階級の落とし穴(TrueObserve縮退) | CHG-C2(§12.2 実機レイテンシ)、CHG-C3(§5.5 lint強制) | **部分解消(可視化)** | 階級別天井が実機値で明示化され「Ansible頼みだと安全機構が落ちる」が数値・lintで見える化。MySQL中核を自前provider(完全階級)で書く必要という結論は不変だが、Schema lintがロード拒否レベルに強制(CHG-C3)されたため事故は減る。 |

---

## 判定変更の理由

前回の`○`は「収束コアの適合は高いが、商品価値の過半がv2機能に人質」という状態だった。v2.3で決定的だったのは **CHG-C1のv1.5**である。20構成中12が「事業の核」をTask/Workflowに依存していた事実を踏まえ、最も需要の高い最小形(単純Task+直列Workflow+invariant+手動承認ゲート)を前倒した。この構成のfailover(§18でrolling upgradeと同型)とPITRリストア(migration型Task)は、まさにこの3要素で骨格が書ける。前回「Sakuteiで管理が成り立つか=v2を実装するか」だった等式が、v1.5到達で「商品メニューの骨格は成立、健全性の継続証明(health/canary)だけがv2」に緩んだ。

第二に、前回§7-5で「実装前に確定必須」と名指しした **rendered_revision算出定義がA-3として着手前決定に格上げ**され(CHG-A3/B5)、1,600本規模の資格情報世代解決という本構成固有の天井問題に対し設計上の担保が付いた。per-resource原因タグ帰属(CHG-B5)も加わり、A(人の小commit)とE-secret(rotate)が1 Cycle内で混在しても相乗り破壊が起きない。これは1,600本を90日rotateで回すDBaaSで日常的に発生する状況であり、直撃の改善。

第三に、前回スケール試算は「200ms前提」だったが、CHG-C2で実機レイテンシに書き直された(下記精査)。以上より`○`→`◎`(条件付き成立の確度が上がった)に更新する。ただし health/canary/DBSourceがv2に残るため無条件`◎`ではない。

---

## 実機レイテンシ算術での3,500リソース再試算(精査)

v2.3 §12.2の実機表で本構成を引き直す。MySQLを **自前provider(完全/標準階級)** で書けば高速REST相当(0.15s)〜govmomiでVM層0.8sの範囲。前回「3,500×200ms/16≈44秒/周」は撤回対象の楽観値。

- **VM層(govmomiではなくProxmox API/自前・~0.15〜0.8s)**:3,500リソースの多くはmysql_database/replication_link等のアプリ層Observe。自前完全階級(0.15s)なら早期警戒線(30s間隔・Observe予算10s・並列16)の上限 **~1,067台/デーモン**。3,500は**単一デーモンでは超過**する。
- **したがって前回の「単一+間引きで十分」は楽観すぎた**。v2.3の正しい読み:(a)**4クラスタ=自然なシャード境界**で4分割 → 各~875台。完全階級0.15sなら1シャードは上限内(~1,067)に**辛うじて収まる**。(b)標準階級やProxmox SOAP的な0.8sが混じると1シャード上限~200に落ち、875台は超過 → **Observe間引き(安定リソースN周に1回、v1b・§12.2)併用が前提**になる。
- **結論**:3,500は「単一デーモンで余裕」ではなく **「4シャード + tier別Observe間引き(v1b)」が実質必須**の帯。シャーディング自体はv2(§24.2)だが、4クラスタ境界での静的分割は所有権境界=物理境界(§12.2)に乗るため設計上は素直。**ただしシャード分割・fencing(CHG-B6/§12.2)がv2である以上、v1.5/v1b段階では単一デーモン+間引きで~1,000台級/デーモンが上限**であり、3,500全体を1デーモンで捌く構成はv2のシャーディングを待つ。前回より天井が下がった(悲観方向に正された)ことは、この構成の成立条件を1つ増やす。

---

## v2.3で新たに気づいた問題・残存課題

1. **PITRリストアTaskは相の途中death問題を抱える(§26.2 B)**:リストア自体はv1.5の単純Taskだが、大規模DBのPITR適用は長時間・中断不可に寄り、実質多相Task的。§26.2 Bで「Applying途中death → auto再開禁止・人手ゲート」が未決事項として残る。firmware更新の議論だが、PITR/大容量リストアにも同型で効く。DBaaSの復元は「途中で落ちたら人が判断」が既定になり、運用3名の負荷に直結。

2. **health/canaryがv2のまま = lag監視・PITR復元可能性証明が骨格に無い**:v1.5でfailover手順とリストア操作は書けるが、`Seconds_Behind_Master`のhealth述語も、隔離環境への定期PITR復元canary(§18の「戻せるかを証明」)もv2。**商品としてのSLA担保(遅延監視・復元保証)はv2まで外部監視(Alertmanager等)に委譲**が必要で、これは前回§4指摘の継続。

3. **段0 Vaultのsealed-on-restart MTTRが1,600本の根を止める(CHG-B3で緩和策提示)**:CHG-B3(§19.7)で「Vault unseal→broker healthy→reconciler」の起動順序明文化、TTLキャッシュ<unseal MTTRに対し **段0限定auto-unseal or Observe専用長TTL別枠キャッシュ** が緩和策として提示された。ただしbrokerはv2(§14.5)なので、v1は`SecretRef`直結(Executor実行時Resolve)で、1,600本の世代metadataキャッシュを段0が持つ前提は不変。緩和策の提示は前進だが、auto-unsealの採否は運用設計に委ねられる。

4. **allocatorのRelease確認がList依存に精緻化(CHG-A4/B2で強化 = むしろ改善)**:前回「Release非対称原則がDB削除で効く」を、CHG-A4が「削除完了=対象IDがList結果から消えたこと(Read不可)」と型レベルで確定(§16.4)。CHG-B2のList完全性検証(件数急減halt・ページング完走)が前提として接続。**新たな注意点**:800×2規模の一括削除で、List結果の件数急減(−50%)がハルトを誘発しうる → 正当な大量削除は`approved-large-change`(CHG-B1)必須。DBaaSの計画的縮退で日常的に踏む。

5. **累積破壊予算 halt-all が800台一斉VerUpと衝突(CHG-B1の副作用)**:CHG-B1で24h窓`max_deletes`超過→halt-all凍結が入った。前回懸念した「800台一斉マイナーVerUp」がdestructive寄りに評価されると、`max_scope_fraction:20%`や`halt-destructive-only`のM周連続でescalateし全凍結しうる。段階適用(canary→broad)+`approved-large-change`で通す設計が**前回より強く必要**になった。

---

## この構成で成立させるための条件(更新版)

- **(a) v1.5到達を前提とする**:単純Task(PITRリストア)+ 最小Workflow(failover:直列+`invariant:writable_primary:1`+手動承認ゲート)。v1a単体では「作って放置」のみで商品メニュー骨格が立たない。
- **(b) allocatorはv1b**:vmid×~1,100 + tenant VLANの採番。reserve→confirm(PR連動)もv1b。v1aの合成リソースは非採番の固定値展開のみなので、800×2の採番はv1bを待つ。
- **(c) 3,500リソースは「4クラスタ=4シャード + tier別Observe間引き(v1b)」で設計する**。単一デーモンの上限は自前完全階級で~1,067台/デーモン。フルシャーディング+fencingはv2だが、4クラスタ静的分割は所有権境界に乗り素直。
- **(d) §26.1 A-3(rendered_revision算出定義)を第0スプリントで確定**:1,600本の世代解決でE-secret誤爆(幽霊diff)を防ぐ生命線。TTLヒット/ミスで揺れない決定的ハッシュを厳密規定。per-resource原因タグ帰属(A-6)も必須。
- **(e) MySQL中核を自前provider(完全階級)で実装**:AnsibleラップはTrueObserve縮退で安全機構が落ちる(§21.3)。Schema lintがロード拒否レベルに強制(CHG-C3)されるため実装規律は上がる。
- **(f) failover判断・health(lag)・PITR復元可能性(canary)は外部委譲/v2待ち**:v1.5では手順(Workflow)とリストア操作(Task)まで。SLA担保の継続証明はv2 or 外部監視。この期待値を組織で確立する。
- **(g) 計画的大量変更(800台VerUp・大量縮退)は`approved-large-change`+段階適用で通す**:CHG-B1のhalt-all凍結、CHG-B2のList件数急減haltを日常運用で踏むため、明示承認フローを最初から組み込む。
- **(h) PITR/大容量リストアの"途中death人手ゲート"(§26.2 B)を運用手順に織り込む**:auto再開禁止が既定。運用3名の判断介入点として明示。
- **(i) 段0 Vaultのauto-unseal採否を決める**:1,600本の根のMTTRを段0限定auto-unsealで縮めるか、長TTL別枠キャッシュで凌ぐか(CHG-B3)。
