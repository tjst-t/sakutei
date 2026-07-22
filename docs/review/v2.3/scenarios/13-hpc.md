# 対象構成13: 学術HPCクラスタ Sakutei 適用可能性レポート(v2.3 再評価)

**新判定: ○ 条件付き成立(前回から据え置き。ただし"成立の足場"が仕様側で明確に補強され、○の内実が濃くなった)**

**前回判定(v2.2 時): ○ 条件付き成立** — 収束の主戦場を「ノード内部の設定」ではなく「ブート供給の骨格 + 版数の可観測性 + 管理系/基盤サービス」に絞る限りにおいて。

v2.3 は本構成の三大ギャップ(xCAT 二重管理・メンテ時 rate limit の形骸化・スケール算術の非現実性)に正面から手を当てており、前回「条件付き成立を支える足場が仕様に明文化されていない」ことが最大の不安だった点が大きく改善した。判定記号は変わらないが、**"祈りで成立させる ○" から "仕様に根拠のある ○" へ格上げ**された。

---

## ギャップ解消表

| 前回指摘ギャップ | v2.3 該当変更(CHG-ID) | 判定 | 根拠 |
|---|---|---|---|
| §7-3 xCAT 二重管理・移行手段が Sync Mode 名指しだが仕様が薄い | **CHG-D2**(§11.4 `external_owner`)+ Sync Mode 精緻化(§23.8) | **解消** | scope 単位の除外宣言 `external_owner: xcat` が第一級化。Observe はするが Apply しない縁切りが v1a で入る。Sync Mode も cutover 手順・generated/ 規律・List 要件が確定 |
| §7-2 メンテ時 rate limit 緩和が形骸化(毎回 approved-large-change で無意味化) | **CHG-D3**(§10.8 remediation schedule)+ **CHG-B1**(§10.2 累積時間窓) | **部分解消** | 保守窓のみ auto の宣言的表現で「毎回手動緩和」を排除。ただし一斉更新の"意図表明"自体は依然 approved-large-change に依存(下記・新論点) |
| §7-1 スケール算術が 500ms 前提で非現実的 | **CHG-C2**(§12.2 実機レイテンシ表・tier 別間引き前倒し) | **解消** | Redfish は "重い REST/SOAP" 階級で 0.8s 前後。546 筐体の再試算が可能に(下記)。tier 別 Observe 間引きが v1b seam へ前倒し |
| §5 メンテ一斉更新を Workflow で構造化したいが Workflow が v2 | **CHG-C1**(§24.1.5 v1.5 最小 Workflow) | **部分解消** | 直列 + invariant + 手動承認ゲートが v1.5 へ前倒し。ただし前回提案の canary→health gate→broad の**自動段階ロールアウト/自動 canary 判定は v2 のまま** |
| §7-5 稼働中ノードを焼かない ready_condition(drain 完了待ち)未整備 | **CHG-A1/A3**(§6.7・§26.1 A-5 ready_condition 名前空間)+ CHG-C1(v1b で複数 Cycle 依存) | **部分解消** | ready_condition 名前空間が着手前決定 A-5 へ格上げ、複数 Cycle 依存が v1b。slurm 状態 Observe→drain gate の写像自体は各自実装 |
| §7-4 lustre/IB のロングテール・二重管理・介入禁止 | CHG-D2(§11.4 external_owner)+ CHG-B2(§8.6 List 完全性) | **部分解消** | ベンダー機を external_owner で縁切り可能に。prune 巻き添え防止も List 件数急減 halt で補強。ただし TrueObserve 落ち・保証縮退の本質は不変 |
| §2.3 firmware 版数是正=多相 Task 委譲、メンテ窓ゲート | §9.1(update_policy: task + window)+ §26.2 B(相の途中 death=人手ゲート) | **未解消(前倒し対象外)** | 多相 Task 状態機械は v2 のまま。Applying 中 death の人手ゲートは決定されたが実装は v2。HPC の firmware 一斉更新は v1 では機構が無い |
| §3.3 Observe 負荷分割(tier 別間引き) | CHG-C2(tier 別頻度=v1b seam) | **解消** | 前回「最初から見込む」とした間引きが v1b の Observe スケジューラ seam として型に載った |

---

## 判定変更の理由(据え置きだが内実の変化)

前回 ○ は「Sync Mode があるから移行できる、canary gate があるから一斉更新できる、という仕様上の約束に依存」していたが、v2.2 時点ではそれらの多くが v2 送り・仕様曖昧で、**"できるはず"の担保が弱かった**。v2.3 で:

- **移行の主力(Sync Mode)が v1b に明確配置**され、さらに `external_owner`(§11.4)という「xCAT が所有する scope を Observe だけして Apply しない」除外宣言が v1a で入った。xCAT と Sakutei の**二重管理事故を構造的に塞ぐ縁切り概念**が第一級化したことは、本構成にとって最大の前進。前回「Sync Mode が無いと移行は現実的でない」と書いたが、v2.3 では Sync Mode に加えて external_owner という**恒久的な棲み分け経路**まで手に入った。
- **スケール算術が実機ベースに書き直された**ことで、前回の楽観的試算(17秒/周)が是正され、tier 別間引きが v1b の型に載った(後述の再試算参照)。
- **rate limit の形骸化が半分だけ**改善された。§10.8 の remediation schedule で「保守窓のみ auto」を宣言でき、平常時 alert-only を機械的に維持できる。これは前回「形骸化リスク」と名指しした穴の半分を塞ぐ。

◎ に届かない理由は前回と同じ:**計算ノード層では level-triggered 収束の動的機構が遊ぶ**という哲学上の本質は v2.3 でも変わらない。むしろ external_owner が「Sakutei は触らない」を第一級化したことは、この構成で Sakutei が計算ノードに対して**可観測性ツールに縮退する**ことを仕様が追認したとも読める。

---

## v2.3 で新たに気づいた問題・残存課題

1. **【新論点・要注意】累積時間窓予算(§10.2 destructive_budget)が一斉メンテで暴発しうる**。v2.3 は 24h 窓で `max_deletes: 50` / `max_scope_fraction: 20%` を超える破壊進行を **halt-all**(rendered_revision 全体凍結)へエスカレーションする。**500 台一斉再プロビジョンは "単一 scope の 20% 超" に確実に触れる**。ここで決定的なのは「**再プロビジョン(再イメージ)が"破壊的変更 delete"にカウントされるか**」——カウントされるなら本構成の正常運用が毎メンテ halt-all を叩く。**メンテ窓での一時的解除経路は存在するが、それは累積窓専用の解除弁ではなく、依然 `approved-large-change` コミットタグ**(超過分保留ではなく「タグが付くまで一切保留」)である。つまり累積窓は「毎回タグを付ければ通る」ゲートであり、前回指摘した**形骸化リスクがここで再燃**する。remediation schedule(時間帯)と destructive_budget(累積量)は**別軸で、schedule で窓を開けても budget は別途タグを要求する**——この二重ゲートを HPC メンテの実運用で「窓 = auto かつ budget 自動緩和」に紐づける経路が仕様に無い。→ **成立条件として、再プロビジョンを delete 扱いにしない Schema 設計(再イメージ=更新であって削除ではない)**を明示的に選ぶ必要がある。これを誤ると累積窓がむしろ邪魔になる。

2. **【前倒しの限界】v1.5 最小 Workflow は"直列+invariant+手動承認"のみ**。前回 §5 で推した「canary(1ラック)→ health gate(自動)→ manual_approval → broad」のうち、**自動 health gate と自動段階ロールアウトは v2**。v1.5 で書けるのは「1ラックを直列で焼く → 人が health を目視 → 手動承認 → 残り」まで。HPC の全系更新に1ラック先行検証の文化はあるので**手動ゲートでも実務は回る**が、「546 筐体を段階自動化」の看板は v1 では出せない。定期メンテの構造化には使える(=部分解消)が、自動化度は前回想定より低い。

3. **【残存】firmware/BIOS 版数の"是正"は v1 に機構が無い**。検出(alert-only の conformance 台帳)は v1a で即価値が出るが、多相 Task(§9.1)は v2 のまま。HPC の BIOS/BMC 版数一斉更新を Sakutei の機構で回すのは v2 待ち。v1〜v1.5 では**検出は Sakutei / 是正は xCAT・手製スクリプト**の分業が続く(前回の分業像そのまま、期限が明確化しただけ)。

4. **【新規・軽微だが有用】observability: none(§23.9)が OPA/IB の一部・観測不能なベンダー箱に嵌まる**。API を持たない旧管理機器や配線を `ledger_only: true` で台帳に持ち、prune 集合演算から除外できる。HPC の泥臭い周辺機器を「unknown のまま第一級」で持てるのは前回想定に無かった収穫。

5. **【残存】Redfish 版差・BIOS 属性名の機種差**という provider 実装難度(前回 §2.4「中」)は v2.3 で直接は救われていない。CHG-C3(Schema lint 強制・golden path)で Schema 設計支援は厚くなったが、Redfish の機種依存吸収は依然 provider 作者の負担。

---

## この構成で成立させるための条件(更新版)

1. **収束の主戦場を「ブート供給インフラ + 版数の可観測性 + 管理系/基盤サービス」に限定する**(前回同)。計算ノードは alert-only + `external_owner: xcat`(§11.4)で縁切りし、是正は xCAT/再プロビジョンに委譲。**v2.3 では external_owner がこれを恒久的な棲み分けとして固定でき、Sync Mode(§23.8)で xCAT 所有 scope を git に片方向記録し続けられる**。移行は「準備の整った scope だけ Converge へ cutover」。

2. **【v2.3 で追加の必須条件】再プロビジョン(再イメージ)を Schema 上"削除"にカウントさせない**。さもないと §10.2 の累積時間窓予算(max_scope_fraction: 20%)が毎メンテ halt-all を発動する。500 台の再焼きは「update」であって「delete」ではない、と Schema 設計(§5.5・A-2 正準)で明示する。

3. **メンテ緩和を二重に紐づける**:§10.8 remediation schedule で「保守窓のみ auto」を宣言(平常時 alert-only の機械的維持=形骸化防止の主軸)し、加えて一斉更新のコミットに approved-large-change を付ける運用を**メンテ手順 Workflow(v1.5 の直列+invariant+手動承認)に構造として埋める**。タグを「手で毎回付ける」ではなく「メンテ Workflow 開始 = 承認ゲート通過 = タグ付与」に一本化して形骸化を防ぐ。

4. **identity をトポロジ座標に固定**(筐体交換で intent 不変・前回同)。**lustre/IB は Ansible ラップ + external_owner + prune 全面禁止**(`pruneable: false` 死守)、List 完全性検証(§8.6)で件数急減 halt を効かせる。観測不能なベンダー箱は `observability: none`(§23.9)で台帳化。

5. **スケールは tier 別間引き前提で設計**:平常時は電源状態 1 分・版数 Observe を N 周に1回へ間引く(v1b の Observe スケジューラ seam)。546 筐体は単一デーモンで成立するが、Redfish は "重い REST" 階級(§12.2)であり間引き無しの毎周全 Observe は早期警戒線に触れる。

6. **firmware 是正は v2 待ち**と正直に位置づけ、v1〜v1.5 では「検出=Sakutei / 是正=xCAT・手製」の分業で導入正当化を組む。

### 546 筐体の再試算(§12.2 実機レイテンシ表による)

Redfish/IPMI は表の「高速 REST 0.15s」ではなく **vCenter/govmomi 級の重い SOAP/REST(0.8s 前後)** に相当する(BMC の応答は遅く、機種によっては 1〜3s)。並列度16・早期警戒線(30s 間隔に対し Observe 予算10s → 上限台数 ≈ 10s×16÷レイテンシ)で読むと:

- **0.8s なら上限 ≈ 200 台**。546 筐体の電源/BIOS を毎周全 Observe すると **546×0.8s÷16 ≈ 27秒/周**で、30s 間隔にほぼ張り付く。前回試算(17秒/周)は楽観的すぎた。
- ここに boot/image/slurm/dns 等の下位 Resource(総 2,000〜4,000)を足すと毎周全 Observe は破綻する。→ **版数・BIOS 設定は tier 別間引き(N 周に1回)必須**。電源状態のみ高頻度、版数は10分間隔、と分ける前回の設計は正しく、v2.3 でそれが v1b の型 seam として裏打ちされた。
- 将来のスケール分割軸は partition(CPU/GPU)/ site。共有基盤は platform シャードが1所有・計算ノード群は read-only 参照(§12.2・前回同)。**fencing token 必須化(CHG-B6)**は本構成の単一 writer 前提と整合し、追い風。
