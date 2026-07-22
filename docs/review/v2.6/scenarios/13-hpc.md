# 対象構成13: 学術HPCクラスタ Sakutei 適用可能性レポート(v2.6 再評価)

**新判定: ◎相当まで一歩前進した ○(条件付き成立)** — 前回 v2.3 で「成立の最大の必須条件」に格上げされた「再イメージを delete にカウントさせない Schema 設計」が、v2.6 の `planned_change`(§10.9)で**機構として直接吸収された**。祈りでも Schema 職人芸でもなく、intent の第一級宣言で解ける。ただし計算ノード層で level-triggered 収束が遊ぶ哲学的本質は不変のため ◎ には届かない。

**判定推移: v2.2 ○(祈りで成立)→ v2.3 ○(仕様に根拠のある成立・ただし新論点=累積予算暴発を要注意で残置)→ v2.6 ○(要注意論点が機構で解消・○の内実が最も濃い)**

v2.4/v2.5 は本構成が再判定対象外だったが、その間に入った `BulkObserve`(§5.7/§5.8)は本構成に副次的な追い風となっており、あわせて評価する。

---

## ギャップ解消表

| 前回(v2.3)残した論点 | v2.6 該当変更 | 判定 | 根拠 |
|---|---|---|---|
| **【要注意・最重要】§10.2 累積時間窓予算が500台一斉再プロビジョンで暴発。再イメージを delete にカウントさせない Schema 設計が新たな必須成立条件。解除弁が approved-large-change タグで形骸化再燃** | **CHG-J1**(§10.9 `planned_change`・変更種別 `reimage`・専用予算レーン・時間窓・自動失効。§10.2 の approved-large-change タグを統合置換) | **解消** | 下記詳述。「reimage は破壊予算に delete としてカウントしない」を宣言で機械可読に指定でき、形骸化タグ運用が構造化宣言に置換された |
| §7-2 メンテ時 rate limit 緩和が形骸化 | CHG-J1(§10.9)+ §10.8 remediation schedule(v2.3 CHG-D3)+ **CHG-K3**(most-restrictive-wins) | **解消** | schedule(時間軸)と planned_change(量・種別軸)が別レーンで併存。窓を開けても budget は宣言分だけ別枠。most-restrictive-wins で「窓を開けたつもりが freeze 中で動く」誤りが安全側に倒れる |
| §7 一斉更新の「意図表明」自体が approved-large-change に依存し二重ゲート・紐づけ経路が仕様に無い | CHG-J1 + **CHG-J2**(N-of-M ゲート・§10.5)+ CHG-K6(v1.5 承認連携) | **解消** | `planned_change` の `approval` フィールドが §10.5 の requirements と直結。メンテ Workflow(v1.5)の承認ゲート通過=宣言承認=専用レーン開放に一本化でき、「毎回手でタグを付ける」形骸化を構造で排除 |
| §7-6 スケール算術(Redfish 重く 546 筐体で毎周破綻) | v2.4 **CHG-F1**(BulkObserve §5.7)+ v2.5 CHG-H1(一貫性契約 §5.8)+ CHG-K4(list_drop_threshold scope 単位化) | **部分解消** | BulkObserve は Redfish/BMC には効かない(バルク API 非対応)が、xCAT/イメージサーバ側 List には効く(下記)。tier 別間引き(v1b)は引き続き必須 |
| §7-4 lustre/IB・棚卸し的削除での List 急減誤発火 | **CHG-K4**(§8.6-b scope 単位閾値 + planned_change 連動) | **解消** | `list_drop_threshold` で volume/IB を厳しめ・流動的 scope を緩めに個別設定。宣言済み reimage/削除は「急減」とみなさない |
| §26 カテゴリ A(コア型未確定=着手前ブロッカー) | **CHG-K1/K2/K5**(§4.6 で State/Plan/Scope/Event 正準形確定・S4 永続化先確定・A を空に) | **解消** | 本構成固有ではないが、`Plan` が破壊性フラグを持つ確定により §10.2/§10.9 の破壊量計上が型で実装可能に。着手前ブロッカー消滅 |
| firmware/BIOS 版数の「是正」機構 | 変更なし(多相 Task は v2 のまま) | **未解消(先送り)** | 検出=Sakutei / 是正=xCAT・手製の分業は継続。前回同 |

---

## 判定理由:要注意論点への §10.9 の効果

**半年ごと全系メンテを planned_change で通すシナリオ**を具体に確認した。intent に次を宣言する(§10.9 の worked example に準拠):

```yaml
planned_change:
  scope: { site: hpc-a, cluster: gpu-pool }
  kind: reimage
  max_count: 500
  window: "2026-08-01 00:00 .. 2026-08-01 06:00"
  approval: { pr: 1423, approvers: [alice, bob] }
```

この宣言が **PR 承認を経る**ことで「人の意図の明示」が構造化される。効果は4点で前回の穴を的確に塞ぐ:

1. **delete 誤計上の解消**:再イメージは内部的に delete+create に見えるが、`kind: reimage` により「純粋な削除ではない」と分類され、§10.2 の破壊予算(`max_scope_fraction: 20%`)に delete としてカウントされない(§10.9・13 指摘への直接応答)。前回「Schema 設計で明示せよ」とした必須条件が、Schema 職人芸ではなく **intent 側の宣言**で解けるようになった。これは大きい——Schema は provider 作者が誤ると黙って安全が無効化される最難関だが(CLAUDE.md)、planned_change は運用者が窓ごとに明示する。
2. **専用予算レーン(バイパスではない)**:窓内・scope 内・種別一致の変更は `max_count: 500` までの別枠で計上され、通常予算を消費しない。**501 台目や別 scope・別種別は通常予算で止まる**。「宣言中でも宣言外の暴発は止まる」(§10.9)——前回懸念した「approved-large-change タグが付けば何でも通る」形骸化を、上限件数・scope・種別・窓で四重に絞ることで排除した。
3. **自動失効**:window(6h)を過ぎたら通常予算に戻る。恒久緩和にならない。半年後の次メンテは新しい PR 承認を要する。「一度タグ運用を許すと外し忘れる」慢性緩和を構造的に防ぐ。
4. **List 急減 halt の抑止**:§8.6-b の planned_change 連動で、宣言済み reimage による件数変動は「急減」とみなされない。lustre/IB の volume・IB scope は `list_drop_threshold` を厳しめに個別宣言でき、宣言外の急減だけ halt が効く(CHG-K4)。

**二重ゲートの解消**:前回「schedule(時間帯)と budget(累積量)が別軸で紐づかない」と指摘した点は、planned_change の `approval` が §10.5 の requirements と直結し、メンテ Workflow(v1.5・N-of-M ゲート)の承認通過をそのまま宣言承認に写像できることで解けた。「窓=auto かつ budget 自動緩和」を単一の承認イベントに一本化できる。

### v2.4/v2.5 の副次効果(BulkObserve は xCAT/イメージサーバ側 List に効くか)

**効く。ただし Redfish/BMC には効かない**——非対称を正確に読む必要がある。§12.2 の天井表は BulkObserve 列で「vCenter/Proxmox/AWS はバルクで1万規模へ、**バルクを持たない生 NW/CLI は台数が物理的に有界**」と明記する。Redfish/IPMI(電源/BIOS)は BMC 単位のバルク API を持たず、546 筐体を毎周全 Observe すると 0.8s×546÷16 ≒ 27秒/周で 30s 警戒線に張り付く前回の試算は v2.6 でも不変。ここは **tier 別間引き(v1b)必須**のまま。

一方、xCAT のノード台帳・イメージサーバのイメージ一覧・DNS/DHCP レコードといった**上位の管理系権威**は多くがバルク列挙 API か DB を持つ。これらを provider 化するなら `BulkObserve`(§5.7)+ `List`(§8.6 完全性検証つき)が効き、下位 Resource(総 2,000〜4,000)の Observe を1コール/scope に畳める。§5.8 の一貫性契約(complete-or-error・型分離=BulkObserve は prune 根拠に使えない・observed_at 刻印・周期越え再利用禁止)により、xCAT DB のレプリカ遅延やページング途中打ち切りが「サイレント欠損→誤 prune」に化ける事故も型・CI で塞がれる。**結論:BMC 直叩き層は間引きで、管理系権威層は BulkObserve で、という二層戦略が v2.6 で型として揃った。**

---

## v2.6 の新ギャップ有無

**本構成に関して、v2.6 が新たに生んだ設計ギャップは見当たらない。** 具体的に確認した観点:

- **planned_change が新たなバイパス穴にならないか** → ならない。上限・scope・種別・窓・承認の五重制約 + 「宣言外は通常予算」で、むしろ approved-large-change タグより穴が小さい。§10.9 の見送り欄でも「バイパス(無制限)にする案は棄却」と明記され設計者が自覚的。
- **kind: reimage を悪用して純粋 delete を隠せないか** → reimage 宣言でも create が伴わず delete のみが観測されれば、それは種別不一致=通常予算で評価される(専用レーンは「種別一致」が条件)。純粋削除を reimage 偽装で通す経路は理屈上ない。ただし **「reimage 宣言と実際の Plan の種別一致をコアがどう判定するか」の実装細部**(delete+create のペアリング判定)は §10.9 に規定がなく、実装時の要注意点として残る(ギャップというより実装課題)。
- **solo プロファイル(§10.10)の混入リスク** → HPC は team/enterprise 運用が前提で solo は無関係。cooling-off が HPC メンテに誤適用される経路はない。
- **most-restrictive-wins(CHG-K3)との干渉** → planned_change で窓を開けても freeze 中なら止まる、が正しく効く。安全側で一貫。

強いて挙げれば、planned_change は**単一 window・単一 kind・単一 max_count** の宣言であり、「1ラック reimage → health 目視 → 残り545台 reimage」のような**段階実行を1宣言で表現できない**。段階ごとに別宣言を切るか、v1.5 の直列 Workflow で回すことになる(自動 canary/段階ロールアウトは依然 v2)。これは前回からの既知の限界であり v2.6 の新規ギャップではない。

---

## 残存課題

1. **firmware/BIOS 版数の「是正」は v1 に機構が無い**(多相 Task=§9.1 は v2 のまま)。検出=Sakutei / 是正=xCAT・手製の分業は継続。前回同・変化なし。
2. **Redfish/BMC の毎周全 Observe は依然スケール律速**。BulkObserve が効かない層のため tier 別間引き(v1b)が必須成立条件のまま。546 筐体≒27秒/周の試算は不変。
3. **自動段階ロールアウト/自動 canary/自動 health gate は v2**。v1.5 では「1ラック直列→人が目視→手動承認→残り」まで。planned_change も段階実行は複数宣言に分割が必要。
4. **計算ノード層で level-triggered 収束が遊ぶ哲学的本質は不変**——◎ に届かない理由。external_owner + reimage 宣言により Sakutei は計算ノードに対して「可観測性ツール + 大量作業の安全弁」に縮退することを、v2.6 の仕様がさらに追認した形。
5. **reimage の delete+create ペアリング判定の実装細部**が §10.9 に未規定(上記・新ギャップではなく実装課題)。
