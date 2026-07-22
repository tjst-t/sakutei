# 対象構成03: 中堅企業の仮想化基盤 — Sakutei適用可能性 再レビュー(v2.3)

**新判定: ◎ 成立(v1.5 導入形として実運用可能)。** ただし NW 中核(FortiGate/Catalyst)は
CommitConfirm を持つ v2 provider が入るまで恒久 alert-only に留まる、という縮退が「設計上の割り切り」
として明示された前提で成立する。

**前回判定(v2.2併記): ○ 条件付き成立**(中核レイヤは素直だが、FortiGate HA・Catalyst の ordered ACL・
段0同型性の3点で設計の空白 or 運用側の割り切りを要した)。

---

## ギャップ解消表

| # | 前回(v2.2)ギャップ | v2.3 該当変更 | 判定 | 根拠 |
|---|---|---|---|---|
| G1 | **v1スコープと必須機能の乖離**(Task/Workflow が v2 送りで「危険な手続きは全部手作業」) | **CHG-C1**(v1.5=単純Task+最小Workflow〈直列+invariant+手動承認ゲート〉)、**CHG-D3**(remediation schedule=v1.5) | **解消(条件付き)** | ローリング/DR/フェイルオーバーの骨格が v1.5 で第一級に載る(§24.1.5・§9.2)。「手続きが半分手動」は「骨格は自動化・多相/自動canaryのみ v2」へ縮小。時期の問題が v1.5 導入まで前倒しされた |
| G2 | **CommitConfirm が取れない機器の段0矛盾**(§19.3 が管理NWコアに必須とするが Catalyst 旧IOS は不可) | **CHG-C5**(v1 は管理NWコアを alert-only 限定・CommitConfirm=false の間は Capabilities で Apply 不可を型強制) | **解消(縮退として)** | 矛盾を「型でロックアウト経路を塞ぐ」形で決着(§19.3 末尾)。段0にできない矛盾ではなく「v1 は観測のみ・Apply は v2 provider を待つ」と明文化。ただし NW 中核の自動収束は先送りという実質は残る |
| G3 | **FortiGate HA の収束粒度と config 同期の綱引きリスク**(revision/rollback の写像が未実証) | **CHG-D2**(§11.4 `external_owner` で外部所有 scope を Apply 除外+協調 seam) | **部分解消** | HA同期を「外部オーケストレータ(FortiOS 自身の同期)所有」として `external_owner` で宣言的に縁切りでき、二重管理事故の主因を型で塞げる。だが FortiGate policy の全置換(§8.5)と revision rollback の写像自体は依然 provider 品質依存で未実証 |
| G4 | **brownfield 後付けコスト**(100VM の managed 切り分けが人力・数ヶ月) | **CHG-A5/F M-3**(§23.3 managed 候補の自動サジェスト)、**CHG-D4**(§23.9 観測不能資産の階級) | **部分解消** | managed 候補の自動サジェスト(§23.3)と Sync Mode(§23.8・v1b)で移行期間を吊る仕組みは前進。`observability:none`+`ledger_only`(§23.9)で観測不能資産の prune 巻き添えも塞がれた。だが「意図の整理は人力」の工数本体は残り、v1b 待ち |
| G5 | **3名・保守的文化の受容リスク**(組織問題・「勝手に戻す」で殺される) | **CHG-D3**(remediation schedule)、**CHG-B3**(§19.6 運用負荷の正直化) | **部分解消** | 手動 freeze しか無かった mode 切替が `remediation.schedule` で宣言的・自動化(§10.8)され、ヒューマンエラー面が縮小。ただし alert-only 期間を長く取る等の受容設計は依然「技術でなく組織の問題」(§23.5) |
| G6 | **段0 同居の自己言及**(段境界維持が運用規律依存) | **CHG-B3**(§19.7-3 自己収束自傷の除外を第一級化) | **解消** | reconciler が自分の broker 到達資格・Vault アドレスを収束対象から**除外**(managed 除外+`external_owner` 相当の「触らない最小核」宣言)と確定(§19.7-3)。§19.3 の管理NW切り分けと同型を段0サービスへ拡張。「自分の土台を prune する」経路が型で塞がれた |

---

## 判定変更の理由(○ → ◎)

前回 ○ の主因は G1(道具が半分=時期の問題)と、G2/G6 の「設計に解はあるが運用規律に依存」だった。
v2.3 はこの2つの中核を潰した:

- **G1**: 本構成の「手続きが半分手動」問題は、v1.5 の**単純Task+最小Workflow(直列+invariant+手動承認
  ゲート)**の前倒し(CHG-C1)で構造が変わる。Proxmox ローリングアップデート・DR・HA フェイルオーバーの
  **骨格**(§9.2 の drain→failover→wait→health→undrain→verify)がこの3要素で表現でき、v2 を待たずに
  「運用が回る」水準へ到達する。残るのは多相 Task 状態機械(firmware の中断跨ぎ)と自動 canary 判定のみで、
  これらは月次窓での手動オペと相性が良く、v1.5 の手動承認ゲートで代替が効く。よって「導入時期を選ぶ」
  という前回の留保が実質解消した。

- **remediation schedule × 月次メンテ窓文化**(本構成の思想的核心): 前回は「alert-only 既定+手動 freeze」
  で窓運用を写像していたが、窓の開閉が手動で SLA 環境のヒューマンエラー面が残った。CHG-D3(§10.8)は
  `remediation.schedule` で「保守窓のみ auto、他は alert-only」を**宣言的に固定**する。level-triggered
  なので窓が開いた周に検知済み drift が自然修復される——「窓外は drift 検知器、窓内で承認済み変更を auto
  適用」という本組織の文化が、手動操作なしで型に写像された。これが月次メンテ窓文化に直接効き、判定を
  押し上げた最大要因。

- **G6(段0自己言及/自己収束自傷)**: 前回は「設計に解(§19.3)はあるが運用規律依存」だった。CHG-B3 の
  §19.7-3 が **reconciler の broker 到達資格・Vault アドレスを収束対象から除外**することを第一級決定にし、
  「Sakutei が自分の土台を prune する」経路を**型で**塞いだ。本構成は段0=本番 Proxmox 同型が最大の特徴で
  あり、この確定は本シナリオに直撃で効く。運用規律依存が構造的防御に格上げされた。

## v2.3 で新たに気づいた問題・残存課題

1. **【残存・最重要】NW 中核の恒久 alert-only 縮退**(G2 の裏面): CHG-C5 で「v1 は管理NWコアを Apply
   しない」が型強制された結果、FortiGate/Catalyst の**自動収束は v2 の CommitConfirm 対応 provider を待つ**。
   本構成は「泥臭い層」に実装難度が集中する構成(§11.1)であり、NW 中核が長期に観測のみに留まる。矛盾は
   解消したが、「NW/ハイパーバイザー中核が永続 alert-only に縮退し、実質 drift 検知ツール」という前回の
   最悪着地は v1/v1.5 の間は依然として現実的シナリオ。IOS-XE の `reload in <n>` 疑似ロールバック実装は
   §5.3 の Apply 契約範囲内で自前 provider 側に載せられるが、これは v2 provider の仕事。

2. **【新規】段0 Ceph SPOF と Vault sealed-on-restart の運用負荷が正直化された**(CHG-B3・§19.6/§19.7):
   本構成は段0=本番 Proxmox 同居のため、§19.7-2 の「統合 Ceph 全損=段0の単一障害点」と §19.4 の
   「Vault は再起動ごとに sealed→人手 unseal」が**そのまま本番の運用負荷**になる。§19.6 が「年数回〜月1回は
   楽観的」と明記した通り、3名運用で段0が片手間で回らない点は新たに直視すべき残存課題。緩和は段0限定の
   Vault auto-unseal(§19.7・HSM/KMS)か Ceph 障害ドメイン分散(別ラック/電源/空調)で、後者は 3ノード
   同居構成だと物理的に確保しにくい。**remediation schedule の窓は段0 の unseal 作業時間帯と整合させる**
   必要がある(§10.8 の注記)。

3. **【新規・緩和】FortiGate HA を external_owner で扱う際の観測整合**: §11.4 の `external_owner` で HA 同期を
   縁切りする方針は綺麗だが、Sakutei は「Observe はする」ため、FortiOS 内部同期が途中の一時的不整合を
   drift として誤検知しうる。§10.4 の converging Tier2(settle 窓)で吸収する設計だが、FortiGate provider が
   Converging capability を写像できるかは未実証(G3 の残存部分)。

4. **【残存】brownfield 工数本体は v1b 待ち**: import・Sync Mode・allocator は全て v1b(§24.1b)。v1a では
   greenfield/新規構築部分からしか始められず(§24.4)、既存100VM への後付けは v1b を待つ。managed 自動
   サジェスト(§23.3)で工数は減るが、100VM 分の意図整理の人力コストは残る。

## この構成で成立させるための条件(更新版)

1. **v1.5 の実装完了を導入前提に置く**(v1a/v1b では手続きが手作業に留まる)。単純Task+最小Workflow+
   remediation schedule が揃って初めて「月次窓で自動収束、窓外は drift 検知器」の運用が回る(§24.1.5)。
2. **NW 中核(FortiGate/Catalyst)は当面 alert-only 固定を受容する**。CommitConfirm 対応の v2 provider が
   入るまで Apply しない(§19.3/CHG-C5)。IOS-XE 系は疑似 commit-confirm(`reload in`)を自前 provider で
   前倒し実装すれば早期に Apply 昇格の余地。**OOB(コンソールサーバ)を Break Glass L2.5 として常設**は必須。
3. **段0=本番 Proxmox 同居の段境界を型で固定する**。§19.7-3 に従い reconciler の broker/Vault 到達設定を
   収束対象から除外し、段0 VM群・段0 pool は `pruneable:false` + scope 外に固定(G6 は型で解決済だが
   Schema/scope 宣言の徹底は運用側の責務)。
4. **段0 の運用負荷を正直に見積もる**(§19.6)。Ceph 障害ドメイン分散と Vault auto-unseal(§19.7)を導入時に
   検討し、3名運用で段0 が回るかを最初に評価する。remediation schedule の窓は unseal 作業時間と整合させる。
5. **brownfield は §23 の alert-only → scope 単位昇格**で進め(v1b の import/Sync Mode 前提)、
   「既存全部管理下」を目標にしない(§23.7)。観測不能資産は §23.9 の `observability:none` で台帳管理。
6. **FortiGate HA は §11.4 `external_owner` で HA 同期を縁切り**し、Observe のみで協調(ready_condition)する。

これらが満たせれば本構成は v1.5 導入形で実運用に耐える(◎)。特に条件1(v1.5)と条件3(段境界の型固定)は
v2.3 で構造的に手当てされたため、前回 ○ の主要留保は解けた。満たせない場合の最悪着地(NW/ハイパーバイザー
中核が永続 alert-only の「可視化・drift 検知ツール」)は依然として残るが、中堅企業には十分な価値がある、
という前回の着地は維持される。
