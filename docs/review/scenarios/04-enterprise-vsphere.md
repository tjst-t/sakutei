# 対象構成04: 単一DC大規模仮想化基盤(エンタープライズ)Sakutei 適用可能性レポート

**総合判定: △ 大幅な拡張が必要(ただし alert-only / brownfield 導入なら ○ に届く)**

Sakutei の思想(観測 state レス・level-triggered・委譲主義)はこの構成と本質的に相性が良い一方で、
「8,000 VM の単一デーモン天井」「ACI/APIC という第二の宣言的収束器」「ITIL/CAB との承認モデルの噛み合わせ」で
v1 スコープを超える拡張(v2 のシャーディング・承認ゲート・converging 三層)を前提にしないと成立しない。

---

## 1. 構成の前提整理

- 規模: vSphere 300ホスト / vCenter 2台 / クラスタ12個 / VM 約8,000。VM 単体を Resource にすると
  リソース数は VM だけで 8,000、監視・バックアップ設定等の下位 Resource を含めると 2〜4万規模になる。
- 常時外力の存在: **DRS/HA/vMotion が配置と電源を常時動かす**。これは §11.2-4 の managed fields 問題が
  基盤全域で常態化することを意味する。
- 二重の収束器: **Cisco ACI/APIC は独自の宣言的収束機構(SDN)**を持つ。§11.2-1「収束器の上の収束器」の
  典型であり、綱引き回避が設計の急所になる。
- 手続き性の強い層: **FC-SAN(PowerMax + Brocade 2面ファブリック)のゾーニング**は順序付きコレクション性が強く、
  一部 vSAN は vSphere 側に寄る。
- 組織: 運用10名が仮想化/NW/ストレージで**縦割り**。ownership/シャードの分割境界と自然に対応させたい。
- 正の状態: **ServiceNow CMDB が手動更新で腐り気味**。inventory ソースの信頼性が低い。
- 変更管理: **ITIL 準拠・CAB 承認必須**、VM プロビジョニングは申請ベースで月500件(≒日20件強)。

---

## 2. リソースモデリング(Resource/Task/Workflow への写像)

- **Resource**: VM(スペック・電源・ネットワーク結線)、DVS/ポートグループ、FCゾーン・マスキング、
  監視設定(§17.2)、バックアップ構成(§18)、ACI のテナント/EPG/コントラクト(APIC への intent)。
- **Task**: VM の再起動を伴う変更(vCPU 増・ディスク拡張)、ESXi/ファーム更新、ゾーニング反映後の
  path 検証。
- **Workflow**: クラスタへのホスト join(invariant=HA/DRS 許容度)、ホスト退役(メンテモード→DRS 退避→除去)、
  ローリング ESXi アップグレード、DR フェイルオーバー。

### 必要 Provider 一覧

| Provider名 | 対象 | Capabilities(TrueObserve/List/Converging/CommitConfirm) | クラス | identity 設計 | 実装難度 |
|---|---|---|---|---|---|
| `vsphere_vm` | VM 本体・電源・スペック | ✓/✓/✓(vCenter task)/✗ | 完全 | 運用不変な論理キー(vm名+tenant)。UUID/MoRef は computed。**current_host/vMotion 先/HA 電源は managed** | 中(govmomi・managed 境界が肝) |
| `vsphere_cluster` | クラスタ容量プール・DRS/HA 設定 | ✓/✓/✓/✗ | 完全 | cluster 論理名 | 中 |
| `vsphere_dvs_pg` | 分散ポートグループ/VLAN | ✓/✓/△/✗ | 標準〜完全 | dvs名+pg名 | 中 |
| `aci_tenant_epg` | APIC テナント/BD/EPG/コントラクト | ✓/✓/**✓(realization)**/✗ | 完全 | ACI 論理 DN | **高**(二重収束・realization 待ち) |
| `brocade_zoning` | FCゾーン(2面) | ✓/✓/△/**✓(要求)** | 完全 | zone名(ファブリック単位) | **高**(順序=全置換・commit性・2面独立) |
| `powermax_masking` | PowerMax マスキングビュー/SG | ✓/✓/△/✗ | 完全 | masking view 名 | 高(volume 削除=データ消失) |
| `vsan_config` | vSAN ストレージポリシー | ✓/✓/△/✗ | 標準 | policy名 | 中 |
| `servicenow_inventory` | CMDB 読取(inventory) | ✓(read)/✓/-/- | inventory ソース(§16.2/render) | CI sys_id | 中(腐り対策が本体) |
| `esxi_host`(BMC/Redfish 併用) | 物理ホスト・電源 | ✓/✓/△/✗ | 標準 | **物理トポロジ座標**(rack/slot)。serial は computed(§5.5) | 中 |

**注意点の写像**:
- **DRS/HA の managed fields**: `current_host` / vMotion 先 / HA による電源再投入 / DRS が動かす予約は
  すべて `managed:true`+`computed`。desired には `placement.cluster`(容量プール)と `demand` のみ書く(§11.3)。
  ここを誤ると DRS と**永久に喧嘩**(非ゼロ diff 恒常化)する。実装難度の本体は Read ロジックではなく Schema。
- **ACI/APIC の converging 扱い**: APIC は宣言を受けて内部で realization する第二の収束器。§10.4 Tier1
  (realization 状態を `Converging` に写像)+ deadline を必須にし、realization 未完を drift と誤認して
  再 Apply する綱引きを避ける。Sakutei は APIC に**粗い intent を渡すだけ**(EPG/コントラクトの存在)で、
  ファブリック内部の収束は APIC に委譲(§2-4)。「APIC の下位の物理配線までを Sakutei で収束」しようとすると破綻。
- **FCゾーニングの ordered 性**: ゾーンセットは位置依存。Schema で `ordered:true` を宣言し、Apply=全置換
  (§8.5)。**2面ファブリックは独立 Resource** とし片面ずつ収束(片面全断を避ける)。plan 表示は
  ゾーン単位差分に展開。Brocade は commit 性を Apply 契約に組み込む。

---

## 3. ownership・DesiredSource・sharding 設計

- **ownership 2層(§12.1)**: 共有基盤(クラスタ・DVS・ACI ファブリック・PowerMax/Brocade)= `platform`
  (PR 必須・alert-only 寄り・rate limit 厳)。テナント VM = `tenant`(即時・auto 可・rate limit 緩)。
- **8,000 VM の単一デーモン天井**: §12.2 の試算「1万×200ms/並列16 ≈ 125秒/周」に照らすと、VM だけで
  すでに天井付近、下位 Resource を含めると単一デーモンは破綻する(早期警戒線を明確に超える)。よって
  **§12.2 のシャーディング(v2)が必須前提**。分割軸は **cluster/site + tenant**。各シャード shared-nothing で
  独立収束、共有基盤は専用 platform シャード1つが所有し他は read-only 参照。
- **縦割りチームとの対応**: 仮想化/NW/ストレージの縦割りは、そのまま provider 群 + シャード + ownership 境界に
  写せる。ただし ACI・FC ゾーニング・PowerMax は**チーム横断の依存**(VM 収束が NW/ストレージ realization を
  ready 待ちする)を持ち、§12.2 の「シャード跨ぎ依存は観測 ready 待ち(結果整合)」で弱結合化する。
  組織の縦割りが強い分、シャード間の強い順序保証を期待しない設計が組織構造と一致し、むしろ好都合。
- **DesiredSource**: platform=FileSource(git/PR)、tenant=DBSource(月500件の申請ベース払い出しを API 経路に)。
  上位プラットフォーム/申請ポータルを DesiredSource の一実装として認可・quota 適用済み intent を投入。

---

## 4. render(inventory/allocation/secret)の使い方

- **ServiceNow CMDB を inventory ソースにできるか**: 技術的には可能(RenderedSource / §7.1 フェーズ1の
  inventory 合成)。だが**「手動更新で腐り気味」が致命的**。腐った CMDB を inventory にすると誤った現実像で
  render し、誤 desired を生む。Sakutei の思想はここで有利: **CMDB を desired の権威にせず、Observe を
  真実源とする**。むしろ逆に **Sync Mode(§23.8)/ import(§23.3)で Sakutei の Observe 結果を
  ServiceNow へ書き戻し、腐った CMDB を機械的に正す**用途が最も価値が高い(CMDB inventory 化より先に、
  CMDB 是正ツールとして入れる)。inventory として使うのは正規化された一部フィールド(所有者・コスト区分)に限定。
- **allocation(§16)**: VMID・VLAN・IP を allocator が払い出し。ServiceNow ではなく allocator(NetBox/Infoblox/
  自作最小)を採番権威にする。CMDB は事実の read、allocator は read-write を分離(§16.2)。
- **secret(§14)**: vCenter/APIC/Brocade/PowerMax の API 資格は v1 暫定で SecretRef(Vault 静的+rotate、§14.5)、
  将来 credential-broker の JIT 発行へ。レガシー FC/ストレージ機器は層B(静的+定期 rotate)に落ちる想定。

---

## 5. 安全機構の適用(CAB/ITIL 整合・rate limit)

- **CAB/ITIL との整合**: ここが構造的な摩擦点。ITIL は「変更を事前に固定して承認」、Sakutei は
  「**意図を承認し実行時の安全機構を信頼**」(§4.5)で思想が異なる。橋渡しは:
  (1) platform 層 = PR 必須 → PR を CAB の RFC 単位に対応させる。(2) tenant 層の月500件申請は
  §12.1 の「authority(採番)+ quota という事前に区切られた枠」を standard change(事前承認済み)扱いにし、
  枠内は即時、枠超えのみ CAB へ上げる。(3) §10.5 の承認ゲート(v2)+ 段階適用を normal change に対応。
  (4) §10.6 freeze を「変更凍結期間(繁忙期)」に直結。
- **drift 自己修復と CAB の衝突**: CAB を通さずデーモンが勝手に直すと ITIL 統制を破る。→ 本番中核は
  `alert-only`(§10.2)を既定にし、drift は Event として起票、CAB 承認後に auto 化(§23.5 の信頼獲得プロセスと同型)。
- **8,000 VM での rate limit 設計**: §10.2 の破壊的重み付け+ in-flight 込み評価が生命線。
  `max_changes_per_cycle` を scope(シャード)ごとに設定し、platform=halt-all、本番テナント=halt-destructive-only。
  **capacity gate**(クラスタ使用率、DRS 前提で 80% 等)を必須。「1コミットで全 VM 削除」(§19.5)が
  8,000 台で現実の恐怖なので、平常ベースラインからの逸脱検知(§10.2)を厳しめに。VM 大量作成(月500件)は
  create 側なので概ね安全側だが、テンプレ差し替えによるカスケード再作成(原因タグ D)は爆発半径を超えうる。

---

## 6. 段0ブートストラップの当てはめ

- 既存エンタープライズ DC なので**純粋な greenfield 段0(Proxmox 3ノード+Ceph)を新設する必要はない**が、
  段0の原理(§19)は当てはめる: reconciler/credential-broker/Vault を、既存基盤とは**独立した最小の
  管理クラスタ**(vSphere 管理クラスタや別途小規模ノード)に載せ、管理NW 到達性の最小核と OOB を段0扱い。
- **管理NW/OOB(§19.3)**: ACI 管理・vCenter・BMC への到達性最小核は段0(Sakutei が触らない)。
  コンソールサーバ/OOB は Break Glass L2.5。ACI の APIC 到達性も段0の single point of blindness 候補。
- git は外部・ローカルミラーで全断耐性(§19.4)。Vault sealed-on-restart の MTTR に人手 unseal が入る点は
  この規模でも変わらない(§19.4 の正直な但し書き)。

---

## 7. 成り立たない点・苦しい点・設計ギャップ・必要な拡張(忖度なし)

1. **単一デーモン天井を明確に超える**: 8,000 VM + 下位 Resource で v1 の「単一・毎周全 Observe」は破綻。
   シャーディング(§12.2, v2)+ Observe 間引き/watch 併用が**必須**であり、これは「あれば良い」ではなく
   「無いと動かない」。v1 スコープ(§24.1)には無く、v2 送り(§24.2)。→ **判定を △ に落とす主因**。
2. **vSphere に watch がない前提だとポーリング一択**: §12.2 の watch 併用ハイブリッドは provider が watch を
   持つ前提。vCenter は Events/property collector があるが、8,000 VM の drift 即知を安定供給できるかは
   実装検証課題。持てなければシャーディングだけで天井を押し上げるしかない。
3. **ACI/APIC の二重収束の deadline 設計が未成熟**: §10.4 は枠組みを与えるが、ACI realization の
   タイムアウト値・部分 realization(一部 leaf のみ反映)の扱いは参照実装レベルの詰めが必要。
   ここを誤ると NW チームが最初に不信を持つ。
4. **CMDB が腐っている前提での inventory 信頼性**: §7.3 は「外部入力不達時は前回値保持」を規定するが、
   「**到達できるが中身が誤っている**(腐り)」は検知できない。誤った inventory は誤 desired を静かに生む。
   → **inventory の妥当性検証(Observe との突き合わせ・異常検知)が設計ギャップ**。当面は CMDB を desired 権威に
   しない運用で回避。
5. **ITIL/CAB と level-triggered 自己修復の思想差**: 技術的には alert-only + PR=RFC で橋渡し可能だが、
   「宣言を承認するのであって individual change を承認するのではない」文化転換が要る。§23.5 が明言するとおり
   **これは技術でなく組織の問題**で、縦割り10名・ITIL 統制下では受容の障壁が高い。
6. **FCゾーニング/PowerMax の provider が高難度で自前化コスト大**: §21.4 が予言する「ブロックストレージは
   専用 API 複雑・ベンダー固有性高く自前化が割に合わず Ansible が長く残る」に正面から該当。Ansible ラップは
   TrueObserve 縮退で横断保証(原因タグ A/B・converging・破壊的重み付け)が落ちる(§21.3)。
   FC/ストレージ層は当面 alert-only+可視化に留めるのが現実的。
7. **必要な拡張の総括**: (a) シャーディング/分散 Executor(§12.3)、(b) 承認ゲート/段階適用(§10.5)、
   (c) inventory 妥当性検証、(d) ACI realization の実運用チューニング、(e) 高品質な vSphere/ACI/FC provider 群。
   (a)(b) は設計済み(v2)、(c)(d) は設計ギャップ、(e) は実装工数。

---

## 8. 総合判定

**△ 大幅な拡張が必要**(条件を満たせば ○ 条件付き成立)

- **理由**: Sakutei の中核思想(観測 state レス・DRS/APIC への委譲・managed fields・二重収束の converging・
  ordered コレクション・容量プール判定・ownership 2層)は本構成の困難(常時外力・二重収束器・順序性)に
  的確に対応しており、**方向性は正しく相性も良い**。しかし 8,000 VM 規模は v1 の単一デーモン天井を明確に
  超え、シャーディング・承認ゲートという v2 機能を前提にしないと動かない。加えて inventory 妥当性検証と
  ACI realization チューニングは設計ギャップとして残る。
- **○ に到達する条件**:
  1. まず **brownfield 導入戦略(§23)を厳守**——import → 全域 alert-only(可視化・CMDB 是正から価値提供)→
     scope 単位で N 日 diff ゼロ→昇格。ハイパーバイザー中核と FC/ストレージは**永久 alert-only でも可**(§23.7)。
  2. v2 の**シャーディング**(cluster/tenant 軸)と**承認ゲート**を実装対象に含める。
  3. 新規構築のクラスタ/テナントを最初から auto(greenfield 部分)にし、既存中核は後回し。
  4. CAB を PR(RFC)と standard change 枠(採番+quota)にマッピングし、drift 自己修復は承認後に段階昇格。
- この条件下では、**「まず可視化・CMDB 是正・新規部分の自律化」に限定した ○(条件付き成立)**として十分な
  価値を出せる。「既存 8,000 VM 全部を auto 収束下に置く」を目標にすると ✗ に近づく——それは §23.7 が
  明確に戒める目標設定である。
