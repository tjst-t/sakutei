# 構成08:既存 OpenStack プライベートクラウドの管理 — Sakutei v2.3 再評価

**新判定:(a) アンダークラウド管理 = ○ 成立(条件付き・v2.2 から強化)/ (b) オーバークラウドリソース管理 = △→○条件付き に前進(構造ギャップの主因が第一級概念で埋まった)**

> 前回判定(v2.2):(a) = ○ 条件付き成立 / (b) = △ 大幅な拡張が必要

前回 (b) の最大ギャップ「外部オーケストレータ所有 scope を管理境界から除外する第一級概念が無い」は、v2.3 の `external_owner`+協調 seam(CHG-D2 / §11.4)で正面から埋まった。これにより (b) は「設計を当てると二重管理事故を招く」状態から「境界を宣言すれば成立する」状態へ移った。ただし converging Tier1 写像の実装コストと採番の適用禁止線引きは残る。

---

## 1. ギャップ解消表

| # | 前回ギャップ(v2.2 §7) | v2.3 該当変更(CHG-ID / 章) | 判定 | 根拠 |
|---|---|---|---|---|
| 1 | **(b) Heat と役割が正面衝突**。外部オーケストレータ所有 scope を除外する第一級概念が無い(最重要) | **CHG-D2 / §11.4** `external_owner` + 協調 seam | **解消** | scope/リソース種別単位で「外部が所有=Observe するが Apply しない」を宣言的に固定。理想状態ロード時に prune・循環検出と同じ入口で検証。§13.1「同一リソースを2供給源が持たない」原則と接続し、Heat 管理下 stack を境界外に置ける。協調は `ready_condition` の一方向 Observe に閉じ、Heat の内部ループに介入しない |
| 2 | **採番権威の二重化(allocator vs Neutron)**。二重権威で §16.4 Release 事故 | CHG-B6 / §16.6(1プール1権威の静的分割・state 逆引き)、§16.4(Release=List 根拠化 CHG-A4) | **部分解消** | 「1プール1権威」原則が明文化され、Neutron を唯一権威とし allocator を (b) に持ち込まない線引きが設計語彙で書けるようになった。ただし「Neutron が IPAM 内蔵の場合 allocator を適用しない」という**適用禁止の明示は §16 本文には無く**、運用側で `external_owner` により network/subnet scope ごと除外する形での回避が実体。§16.4 の Release=List 根拠化は二重化そのものは解かない |
| 3 | **quota の責務衝突** | §12.1(2層所有権・quota=上位プラットフォーム責務、v2.2 から不変) | **未解消(元々設計整合)** | quota は §12.1 で明確に上位プラットフォーム(認可・課金)の責務。v2.3 でも同じ。これは「(b) の管理対象から quota を外す」で対処済みであり拡張不要。前回同様、(b) から除外する運用規律が答え |
| 4 | **(a) Ansible ラップ縮退で conf drift 原因タグが曖昧** | CHG-C3 / §5.2·§5.5(Schema lint 強制・golden path)、CHG-B5 / §10.3(per-resource 帰属)、CHG-C4 / §10.3(last-seen リング) | **部分解消** | 自前 conf provider を書くべき、という前回の結論自体は不変。ただし §10.3 の原因タグ per-resource 帰属化・version 埋め込み fail-closed・last-seen リング(「前周は X だった」)で、TrueObserve:true provider を書いた場合の「いつ誰が触ったか」体験が大幅強化。Ansible ラップの縮退は §21.3 のまま残る(実装コストの性質は不変) |
| 5 | **OpenStack 内部収束の realization を全部写像できない**(converging Tier1) | §10.4(Tier1/2/3・deadline、v2.2 から実質不変)、CHG-D2 協調 seam | **部分解消** | Tier2 自傷猶予(settle)・Tier3 振動自動隔離・deadline は「検知手段ゼロの provider でも成立する床」として明記。全 API に「作業中」概念が揃わない問題は本質的に残るが、写像できない項目は `external_owner` で scope ごと Apply 抑止に倒せるようになった(縁切りの受け皿が増えた)。適応 settle 窓(S-05)は §26.3 C の運用パラメータに留め非必須 |
| 6 | **アップグレード Workflow の invariant 表現力**(N-1 互換ウィンドウを書けない) | CHG-C1 / §24.1.5(最小 Workflow を v1.5 に前倒し)、§9.2(invariant=min_active/quorum のまま) | **未解消** | Workflow 最小形(直列+不変条件+手動承認ゲート)は v2 から v1.5 へ前倒しされ着手時期は前進。しかし「バージョン跨ぎ互換ウィンドウ」を invariant に素直に書く拡張は v2.3 に無い。多相 Task 状態機械・自動段階ロールアウトは v2 のまま。アップグレードの半端状態表現は依然として承認ゲート+人手の invariant 表現で代替 |

---

## 2. 判定変更の理由

**(b) が △ から「○ に近づく条件付き」へ動いた最大要因は CHG-D2(§11.4)である。** 前回 (b) を △ に落としていた3構造問題(Heat 衝突・採番二重化・quota 衝突)のうち、最も重く「converging 実装では済まない設計拡張」と評したのが Heat 衝突だった。v2.3 は `external_owner` を **v1a の最も基本(alert-only 相当の Apply 抑止)**として導入し、除外を理想状態ロード時に検証する第一級概念に格上げした。これにより:

- **Heat/Terraform 管理下の network/router/stack** を `external_owner: heat` で境界外に置き、Sakutei は Observe(可視化)だけ行う。二重管理事故の主因が構造的に塞がる。
- **Neutron が IPAM/VNI 権威を内蔵する scope** も、allocator を持ち込まず `external_owner` で Apply 抑止し「Neutron の払い出し結果を Observe するだけ」を宣言で固定できる。前回「§16 モデルは (b) に適用できない/適用してはいけない領域の明示が要る」と指摘した穴の受け皿ができた。
- **協調 seam** により、Heat stack の収束完了を `ready_condition` として下流ゲートにする一方向連携が新プリミティブ無しで書ける(§12.2 のシャード間依存の再利用)。

結果、(b) は「Sakutei が管理する scope(project/flavor/image=Keystone/Nova/Glance の冪等 CRUD 系)」と「外部所有 scope(Neutron 収束系・Heat stack・quota)」を境界で分離すれば成立する構図が設計語彙で完結する。**「現状設計をそのまま当てると事故る」という前回の △ 根拠が消えた**ため前進と判定した。ただし後述の残存課題により無条件 ○ には至らない。

**(a) は ○ 据え置きだが強化。** conf drift 検知という核心に対し、CHG-B5(原因タグ per-resource 帰属・version fail-closed)+ CHG-C4(last-seen リングと `sakutei status` デバッグ体験)が「誰がいつ conf を触ったか」の追跡を直接強化する。CHG-C3(Schema lint 強制)は自前 conf provider の誤り爆発半径(別ノードの nova.conf 一斉書換)を型で抑える。CHG-C1 で Workflow 最小形が v1.5 に前倒され、アップグレードの着手時期も前進した。CHG-B3(§19.7 段0 コールドスタート順序・Ceph SPOF)は「段0 を OpenStack 外に別建て」する本構成の生命線を補強する。

---

## 3. v2.3 で新たに気づいた問題・残存課題

1. **`external_owner` の粒度と Neutron の入れ子。** Neutron network/subnet は「Sakutei が project を作る→その中に Neutron が収束する」という入れ子関係にある。scope 単位除外が「project は管理・その配下 network は除外」を綺麗に切れるか、除外境界がテナント境界と直交して二重定義にならないかは、§11.4 が scope/種別単位としか規定しておらず**運用設計に委ねられている**。切り方を誤ると prune が Neutron 管理リソースを巻き添えにする危険が残る。

2. **採番二重化の「適用禁止」が明文化されていない。** §16.6 は「1プール1権威・静的分割」を書くが、これは Sakutei allocator 同士の分割規律であって「**Neutron のように外部が IPAM 権威を内蔵する場合は allocator を使うな**」という禁止線は §16 本文に無い。実務上は `external_owner` で回避できるが、allocator を素朴に (b) に持ち込む実装者を型レベルで止める仕掛けは無い(誤用余地が残る)。

3. **converging Tier1 写像は依然として実装コストの塊。** §10.4 の床(Tier2/3/deadline)は v2.2 と実質同一。Neutron provisioning status / OVN realization / Nova task_state を `Converging=true` に写像する自前実装が (b) 成立の必要条件である点は不変。写像できない項目を `external_owner` で逃がせるようになった分だけ緩和されたが、「逃がす=Apply しない=収束価値が出ない」ので、収束したい scope については結局 Tier1 写像を書く必要がある。

4. **List 完全性検証(CHG-B2 §8.6)が OpenStack API のページングと相性問題を起こしうる。** (b) の prune 根拠は List 集合演算(§16.4 の Release も List 根拠化 CHG-A4)。OpenStack API はページング・マイクロバージョン・レプリカ遅延を持つため、§8.6 の「件数急減 −50% で prune 保留」が Neutron の大量 port などで誤発火/取りこぼす懸念。新しい防御が新しい調整点を生んでいる。

5. **段0 の Ceph SPOF 明記(§19.7)は本構成の統合 Ceph と衝突しない別建て要件。** 既存 OpenStack が Ceph 統合構成なので、段0 の Ceph SPOF 対策(障害ドメイン分散を人手運用要件化)を「OpenStack 外の段0 基盤」に別途適用する必要がある。CHG-D1(段0-minimal §19.8)は運用4名の本現場には魅力だが、100ノード中核の platform 層を単一ノード段0 で支えるのは可用性的に non-starter で、本構成では素直に §19.2 の3ノード段0 を採る。

---

## 4. この構成で成立させるための条件(更新版)

### (a) アンダークラウド = ○ 成立
- **自前 conf/file provider(TrueObserve:true)を書く**(Ansible 縮退回避)。v2.3 では CHG-B5/C4 の原因タグ帰属・last-seen が価値を増幅するので投資対効果が向上。
- **Schema lint 強制(§5.5/CHG-C3)に乗せ**、100ノード一斉書換の爆発半径を型で抑える。
- **段0 を OpenStack 外に3ノード別建て**(§19.2)。§19.7 のコールドスタート順序(Vault unseal→broker→reconciler)・Ceph 障害ドメイン分散を運用要件化。
- **管理 API/Keystone/Galera/RabbitMQ の到達性最小核を §19.3 の alert-only 限定(v1 は収束対象にしない)**で保護。commit-confirm 未実装の v1 は Apply 経路を Capabilities で塞ぐ。
- アップグレードは v1.5 の最小 Workflow で着手可。ただし N-1 互換ウィンドウは invariant に書けず承認ゲート+人手 invariant で代替(残存)。

### (b) オーバークラウド = ○ 条件付き(v2.3 で到達可能に)
- **`external_owner` で境界を明示的に引く**:Heat/Terraform 管理下 scope・Neutron が IPAM 権威を持つ network/subnet 系・quota を除外宣言し、Sakutei は Observe のみ。
- **Sakutei が収束するのは冪等 CRUD 系に限定**:project/domain(Keystone)・flavor(pruneable:false・参照カウント §8.4)・image(コンテンツアドレス §20.3)。
- **allocator を (b) に持ち込まない**。Neutron を唯一の採番権威とし、Sakutei は払い出し結果を Observe。§16.6 の「1プール1権威」は Neutron 側に権威を寄せる形で満たす。
- **収束したい Neutron scope については converging Tier1 写像を自前実装**(provisioning status/OVN realization/Nova task_state → `Converging=true`)。写像不能項目は `external_owner`/managed fields(§11.2-4)で Apply 抑止。
- **List 完全性検証(§8.6)の閾値を OpenStack のページング特性に合わせて調整**し、prune/Release の誤発火を防ぐ。
- quota は上位プラットフォーム(§12.1)へ委譲。上位ポータル不在の限定環境では quota・採番を Neutron/OpenStack ネイティブに委ねきる。
