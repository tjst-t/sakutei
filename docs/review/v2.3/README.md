# v2.3 再検討 総括 — 20 構成の適用可能性(第 2 ラウンド)

実施日: 2026-07-22。対象: `docs/design/sakutei-design-v2.3.md`(v2.2 のレビュー指摘を反映した改訂版。変更内容は [CHANGELOG-v2.3](../../design/CHANGELOG-v2.3.md))。

各構成の再検討は、前回(v2.2)レポートの指摘ギャップごとに「解消/部分解消/未解消」を判定する差分方式で実施した。

## 判定の変化一覧

判定: ◎成立 / ○条件付き成立 / △大幅な拡張が必要 / ✗不成立

| # | 構成 | v2.2 | v2.3 | 変化の主因 |
| --- | --- | --- | --- | --- |
| 01 | [ホームラボ](scenarios/01-homelab.md) | △ | **○** ↑ | 段0-minimal 新設(CHG-D1)で 3 ノード前提との物理衝突が消滅。Vault 不在で unseal 地獄も構造消滅 |
| 02 | [スモールオフィス](scenarios/02-small-office.md) | ✗ | **△** ↑ | 段0-minimal+external_owner で「可視化+局所 Apply」まで成立。全体自律収束は依然 ✗ |
| 03 | [中堅企業仮想化](scenarios/03-midsize-virtualization.md) | ○ | **◎** ↑ | v1.5(Task+最小 Workflow+schedule)で手続き骨格が自動化。段0 自己言及も除外の第一級化で決着 |
| 04 | [大規模 vSphere](scenarios/04-enterprise-vsphere.md) | △ | **○** ↑ | 実機算術の正直化+tier 間引き v1b+承認ゲート v1.5。全域即時 auto 化を狙うと△に戻る |
| 05 | [SDN Leaf-Spine](scenarios/05-sdn-leafspine.md) | ○ | ○ → | 全断防御群(累積予算/List 検証/fencing)がコアに降格し内実前進。SONiC 60 台は v2 まで観測専用 |
| 06 | [マルチサイト+DR](scenarios/06-multisite-dr.md) | △ | △ → | S2 永続化先確定・fencing・git 外部化は前進したが、サイトシャーディング/分散 Executor 本体が v2 のまま |
| 07 | [パブリック IaaS](scenarios/07-public-iaas.md) | △ | △ → | 安全性は多層化したが、DBSource/シャーディング/UI が v2 のまま+「委譲先=事業本体」問題に応答なし |
| 08 | [OpenStack](scenarios/08-openstack.md) | (a)○/(b)△ | **(a)○強 / (b)○** ↑ | (b)の最大ギャップ「外部所有 scope 除外の概念欠如」が external_owner(CHG-D2)で正面解消 |
| 09 | [マネージド k8s/PaaS](scenarios/09-managed-k8s-paas.md) | △ | **○** ↑ | external_owner で operator 二重ループ境界が構造解消。自動 canary/段階ロールアウトは v2 のまま |
| 10 | [SaaS 事業者](scenarios/10-saas-operator.md) | ○ | ○ → | MHA 協調 seam(一方向)新設・v1.5 骨格で実効改善。MHA「作業中」不可視は原理的に残存 |
| 11 | [DBaaS](scenarios/11-dbaas.md) | ○ | **◎** ↑ | v1.5 で PITR 復元 Task・failover 手順 Workflow が書ける+rendered_revision 定義が着手前決定に格上げ |
| 12 | [GPU/AI](scenarios/12-gpu-ai.md) | ○ | ○ → | 累積予算・external_owner(UFM/Slurm)・S2 確定で前進。中核の多相 Task が v2 のままが律速 |
| 13 | [HPC](scenarios/13-hpc.md) | ○ | ○ → | 「祈りの○」から「仕様に根拠のある○」へ。新論点: 累積予算が 500 台一斉再イメージと衝突しうる |
| 14 | [エッジ小売 300 拠点](scenarios/14-edge-retail.md) | △ | △ → | 分散 Executor/lease 縮退が v2 のままが最大要因。段0-minimal は案 C(店舗ローカル)の代替にならず |
| 15 | [通信 NFV](scenarios/15-telco-nfv.md) | △ | **○** ↑ | 最大ギャップ「時間帯連動 mode」が remediation schedule(CHG-D3、v1.5 配置)で解消 |
| 16 | [CDN/PoP](scenarios/16-cdn-pop.md) | ○ | ○ → | 前提が「v2 全面依存」から「v1.5+v1b で骨格成立」へ軽量化。自動段階 BGP 展開のみ v2 依存 |
| 17 | [金融高規制](scenarios/17-regulated-finance.md) | ○ | **○+** ↑ | 承認ゲート付き Workflow の v1.5 前倒し+WORM 監査強化。4-eyes/N-of-M は未解消(上位実装必須) |
| 18 | [レガシー DC](scenarios/18-legacy-brownfield.md) | 可視化○/収束△ | **可視化◎**/収束△ ↑ | ledger_only/observability:none 階級(CHG-D4)で観測不能資産が型で救済。組織統治リスクは型で解けず |
| 19 | [ハイブリッド AWS](scenarios/19-hybrid-cloud.md) | オンプレ○/AWS△ | **オンプレ◎**/AWS△ ↑ | オンプレは v1a/v1b で条件解消。AWS は watch 併用が v2 のままで「AWS=Terraform 継続」推奨不変 |
| 20 | [ベアメタル](scenarios/20-baremetal-hosting.md) | ○ | ○ → | ワイプ安全骨格が v1.5 で成立し確度前進。provisioner が名前のみ(Sakutei 外)が上限を規定 |

**集計(主判定):** ◎=2(+部分◎ 2) / ○=10 / △=5 / ✗=0。**改善 10 構成、据え置き 10 構成、悪化 0。** v2.2 時の ✗ は消滅した。

## v2.3 で構造的に解決されたもの

1. **external_owner(外部オーケストレータ境界)の第一級化** — 最も効いた変更。OpenStack(b)・k8s operator・UniFi・MHA・UFM/Slurm・RDS と、6 構成以上で「二重管理事故の主因」を型で消した。
2. **v1a/v1b/v1.5 再スライス** — 「事業の核が v2+」問題の過半が v1.5(単純 Task+最小 Workflow+手動承認ゲート+remediation schedule)で解け、7 構成の判定/内実を引き上げた。
3. **段0-minimal** — 小規模帯(01/02)の物理的不成立が消滅。
4. **累積時間窓予算+List 完全性検証** — 「漸進的全削除」「List 欠損誤 prune」の 2 大事故経路をほぼ全構成で封鎖。
5. **remediation schedule** — 保守窓文化(03/15/17)との正面衝突が解消。
6. **実機レイテンシのスケール算術** — 天井が正直になり(悪化方向の修正)、かえって 04/05/11 で「シャード分割・間引きをいつ入れるか」の計画が立つようになった。

## 残存する構造的課題(v2.4 以降への課題)

1. **分散型ユースケースは v2 まで開かない** — 分散 Executor・シャーディング本体・lease 縮退が v2 のままで、06(マルチサイト DR)/07(IaaS)/14(エッジ 300 拠点)の△はこれが芯。この 3 構成を取るなら分散実行系の前倒しが必要。
2. **多相 Task(状態機械)が v2** — GPU ファーム更新(12)・ベアメタルプロビジョニング(20)の中核が「最初から要る」構成では律速のまま。
3. **v2.3 が生んだ新しい摩擦(次回改訂での要調整)**:
   - CHG-B6「platform 破壊的大量変更は承認必須」が 1 名運用(01/02)の摩擦を増やす — 運用規模での既定値切替が要る
   - 累積削除予算・List 急減 halt が「正当な一斉作業」(HPC 500 台再イメージ、DBaaS 800 台 VerUp、k8s 正常変動)と衝突/誤発火しうる — approved-large-change との接続を専用の解除弁として設計すべき
   - tier 別間引きと drift 即知性のトレードオフ、remediation schedule と ownership 別 mode の優先規則が未定義
   - fencing token 発行元のクロスサイト単一性(06)が新たな中心依存になりうる
4. **型で解けないもの(設計の限界として明示済み)** — 組織統治(git 単一書込経路・drift 報告の責任主体、18)、4-eyes/N-of-M(17、上位実装)、provisioner 実体(20)、AWS との対 Terraform 優位(19)。
