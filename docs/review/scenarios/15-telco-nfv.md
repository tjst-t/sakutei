# 対象構成15: 通信事業者 NFV / テレコムクラウド Sakutei 適用可能性レポート

**総合判定: △ 大幅な拡張が必要(NFVI 収束対象としての相性は良いが、「保守窓のみ auto」の時間帯スケジュールと通信事業法級の証跡が v1 に無く、設計拡張なしには成立しない)**

Sakutei が守備範囲とする NFVI 層(ホスト OS・ハイパーバイザ設定・SR-IOV/NUMA・アンダーレイ NW・PTP)は、宣言的で観測可能・冪等適用しやすく、収束モデルと本質的に相性が良い。VNFM/MANO との境界も `ownership` + managed fields で綺麗に引ける。だが本構成の核心である「常時収束 vs 保守窓文化」の正面衝突に対し、v1 が用意する道具は `remediation mode`(auto/alert-only)と `freeze`(全体を alert-only に倒す)だけで、**時間帯に応じた mode 自動昇格(保守窓のみ auto)という概念が設計に存在しない**。ここが最大のギャップである。

---

## 1. 構成の前提整理

- **守備範囲は NFVI(基盤層)のみ**: VNF のライフサイクルは VNFM/MANO(ベンダ製オーケストレータ)が持つ。Sakutei が触るのは、その VNF が乗る土台——ホスト OS・KVM/QEMU 設定・SR-IOV VF 割当・NUMA/CPU ピンニング・HugePages・アンダーレイ NW・PTP。§2-4「値・配置・予測・認可は持たない。委譲する」が VNFM 境界にそのまま効く。
- **規模**: 2 サイト冗長 × 各 60 ノード = 120 ノード。1 ノードあたり NFVI 系 Resource(ホスト設定・NIC/VF・NUMA・HugePages・PTP・アンダーレイポート)を 10〜30 個とすると **数千 Resource 級**。§12.2 の単一デーモン実用上限(数千〜1万)の内側で、v1 でリソース数は捌ける。
- **SLA 99.999%(年 5 分)**: ダウンタイム予算が極端に薄い。level-triggered の「勝手に直す」が VNF のデータプレーンに触れると即 SLA 違反になりうる。
- **保守窓のみ作業**: 深夜 2 時間・事前申請制・切り戻し手順必須。**保守窓以外は一切触らない**という文化が、Sakutei の「常時回り続ける」動作原理と正面衝突する。
- **監査**: 通信事業法対応の作業記録・証跡が必須。誰が・いつ・何を・なぜ変えたかの完全な追跡が要る。
- **職掌分離**: オペレータ / 保守ベンダ / 設計部門の 3 層。書き込み経路・承認・到達資格の分離が必要。

---

## 2. リソースモデリング(NFVI 層の Resource 写像)

NFVI 設定は「宣言的状態」そのものであり、大半が Resource に落ちる。SR-IOV/NUMA/HugePages/PTP は Schema のフィールドとして宣言する。

### Schema での宣言方針(要点)

- **SR-IOV**: `sriov_vf`(PF ごとの VF 数・MTU・trust/spoofcheck・VLAN)を Resource 化。VF の PCI アドレスは `identity` にしない(交換で変わる)——**物理トポロジ座標**(`node03-nic1-pf0`)を identity にし、PCI アドレス・MAC は `managed:true`+`computed`(§5.5)。VNFM が VF を掴んだ後の「どの VNF に紐づくか」は VNFM 領域なので **managed**。
- **NUMA/CPU ピンニング**: `numa_topology`(isolcpus / nohz_full / CPU affinity マスク / hugepage の NUMA 別配置)を宣言。これらはブート引数・カーネル設定に落ちるため、変更は再起動を伴う手続き=**Task**(§9.1 多相 Task、リブート跨ぎ)に委譲し、収束は「あるべき値」の監視とゲートのみ担う(§2-2)。
- **HugePages**: `hugepages`(サイズ 1G/2M・ページ数・NUMA 別割当)。実際の予約はブート時 or 動的で、動的分は VNF が消費した実数が `managed`(VNFM/カーネルが動かす)。desired は「予約すべき総数」だけ。
- **PTP 時刻同期**: `ptp_config`(ptp4l/phc2sys 設定・PTP プロファイル G.8275.1・grandmaster 優先度・境界クロック役割)を Resource 化。**PTP の同期状態(offset/lock 状況)は health(§17.2)であって収束対象ではない**——設定は収束、ロック品質は監視系へ export。ここを混同すると「同期が揺れた」を drift と誤認する。
- **アンダーレイ NW**: `underlay_port`(VLAN/MTU/LAG/QoS)。順序付きコレクション(ACL/QoS ポリシー)は `ordered:true`+全置換(§8.5)、機器は `CommitConfirm` 必須(§11.2-5・管理断ロックアウト防止)。

### VNFM 管理領域との managed fields 境界

「NFVI が敷く土台」と「VNFM がその上で動かす実体」の境界を **managed** で引く。desired に書くのは土台の宣言(VF 数・HugePages 予約総数・PTP プロファイル)まで。VNFM が確定する「どの VF/CPU/ページを、どの VNF インスタンスに、いつ割り当てたか」は runtime=managed で収束対象外にする。これを誤ると **VNFM と永久に喧嘩**(§11.2-4)し、VNF がスケール/移動するたびに drift 判定→再 Apply で綱引きになる。SR-IOV/NUMA は「上の VNFM がリアルタイムに動かす層」なので、境界設計の難度は vSphere/DRS 案件より高い。

### 必要 Provider 一覧

| Provider名 | 対象 | Capabilities(TrueObserve/List/Converging/CommitConfirm) | クラス | identity 設計 | 実装難度 |
|---|---|---|---|---|---|
| `host_os` | ホスト OS パッケージ/サービス/sysctl/カーネルブート引数 | ✓/△(open-world寄り)/✗/✗ | 標準 | 物理トポロジ座標 | 中(層1・Ansible 併用可) |
| `sriov_vf` | PF ごとの VF 数・trust・spoofcheck・MTU | ✓/✓/✗/✗ | 標準〜完全 | `nodeXX-nicY-pfZ`。PCI/MAC は computed+managed | 高(VNFM 境界=Schema が本体) |
| `numa_topology` | isolcpus/CPU affinity/HugePages NUMA 配置 | ✓/✗/✗/✗ | 標準 | node 座標+socket | 高(変更=リブート Task 委譲) |
| `hugepages` | サイズ別ページ予約数 | ✓/✗/✗/✗ | 標準 | node 座標 | 中(消費実数は managed) |
| `ptp_config` | ptp4l/phc2sys/G.8275.1 プロファイル | ✓/△/✗/✗ | 標準 | node 座標 | 中(同期状態は health へ) |
| `underlay_port` | アンダーレイ VLAN/MTU/LAG/QoS/ACL | ✓/✓/△/**✓(要求)** | 完全 | 機器+ポート論理名 | 高(順序付き・commit-confirm) |
| `hypervisor_cfg` | KVM/QEMU・libvirt・IOMMU/vfio | ✓/✓/△/✗ | 標準〜完全 | node 座標 | 中 |
| `bmc_redfish` | 物理電源/ブート | ✓/✓/△/✗ | 標準 | 物理トポロジ座標(§5.5) | 中(意図的 off を on にしない=managed 境界) |

VNF そのもの・VNFM は Provider 化しない(委譲)。せいぜい「VNFM が要求する NFVI 容量が満たされているか」の粗い health を持つ程度。

---

## 3. ownership・DesiredSource・sharding 設計

- **ownership はほぼ全て `platform`**: NFVI は共有基盤であり、テナント専有物(§12.1 の tenant 層)はほぼ無い。すなわち **書き込み経路は全て PR 必須・remediation は alert-only 寄り・rate limit 厳格・爆発半径大**(§12.1 の platform 行)がそのまま適用される。これは本構成の保守窓文化・職掌分離と方向が一致する強み。
- **職掌分離の写像**: 設計部門=git への PR 承認者(intent の source of truth)、保守ベンダ=承認済み変更の実行と Observe 監視、オペレータ=freeze/承認 API の操作。**書き込みは git PR に一本化**され、GUI 直変即本番が構造的に防がれる(§13.4)。「誰が承認したか」は承認ゲート(§10.5)と監査(§17.5)に記録。
- **DesiredSource**: `FileSource`(git)。全域 platform 層なので DBSource(即時・tenant 向け)は不要。git commit 連鎖が監査の半分を担う(§17.5)ため、通信事業法の証跡要件と親和的。
- **sharding**: 2 サイトを**サイト単位でシャード**(§12.2)。各サイトのデーモンが shared-nothing で独立収束し、片サイトの制御面障害がもう一方に波及しない=冗長構成と shard 境界が一致する理想形。ただし **v1 はシャーディング未実装**(§24.2 で v2)なので、2 サイトを 2 デーモンで回すには v2 相当の拡張が要る(規模的には各サイト数千 Resource で単一デーモンに収まるため、当面は 2 台の独立単一デーモン運用で代替可能)。

---

## 4. render(inventory/allocation/secret)の使い方

- **inventory**: 120 ノードのハード実装(NIC の PF 配置・NUMA トポロジ・PTP 結線)を inventory とし、render フェーズ1 で合成。NIC/NUMA は機種ごとに固定的なので、外部 CMDB より git 管理の inventory ファイルで足りる公算が高い。
- **allocation**: アンダーレイ VLAN プールを allocator(§16)から払い出す。platform 層なので **reserve→confirm**(§16.3)を使い、PR diff に具体 VLAN が出る=何が使われるかレビューで見える。Release は非対称原則(§16.4・下流削除を Observe 確認後)。
- **secret**: PTP 認証鍵・機器管理資格・ホスト OS の初期資格を `SecretRef`(§14)で参照。値は収束しない。v1 は broker 未実装のため機器到達資格も SecretRef(Vault 静的資格)注入で暫定(§14.5)。
- **合成リソース**: 「NFVI ノード 1 台の完成形」を合成リソース化(`host_os` + `sriov_vf` + `numa_topology` + `hugepages` + `ptp_config` の扇状 DAG)し、ノード追加を 1 つの intent で書ける(§7.5)。子が独立 Resource なので個別 drift 検知・爆発半径が粒度良く効く。

---

## 5. 安全機構の適用(最重要)

### 保守窓文化との整合——ここが設計上の急所

**alert-only 常用 + 保守窓のみ auto 昇格という運用は、設計思想としてはサポートされるが、機構として v1 に存在しない。**

- **常用 alert-only は成立する**: `remediation: alert-only` は爆発半径ゼロで最も保守的(§10.2)。保守窓外は全 NFVI を alert-only に固定すれば「回り続けるが触らない」——**Observe と drift 検知だけは常時回り、修復は止まる**。これは §10.6 の freeze(全体を alert-only に倒すマクロ)と同義であり、保守窓外を恒常 freeze にする運用は**思想的には完全に整合**する。level-triggered の存在意義はここで肯定される: **保守窓外も観測は止めず「今ズレているものリスト」を蓄積し、保守窓に入った瞬間に「何を直すか」が全て揃っている**——これは edge-triggered なら得られない価値であり、「常時収束が回る」ことと「触らない」ことは Observe/Apply 分離により両立する。
- **だが「保守窓のみ auto 昇格」の自動化は無い**: §9.1 の Task には `window: "Sat 02:00-04:00"`(メンテ窓外では Task を起動しない)が存在するが、これは**個別 Task のゲート**であって、**Resource 収束の remediation mode を時間帯で auto⇔alert-only に自動切替する仕組みではない**。設計を全文精査した限り、**remediation mode の時間帯スケジュールという概念は設計に存在しない**(§10.2 は mode を「リソース/scope 属性」とするが、時間軸を持たない)。現状の代替は「保守窓の開始/終了時にオペレータが手動で freeze/unfreeze する」運用のみで、これは事前申請制と噛むが、自動化されない分ヒューマンエラー面が残る。→ **§7 で拡張として提起**。
- **切り戻し手順必須との整合**: NW 機器は `CommitConfirm`(§11.2-5・確認が来なければ自動ロールバック)、複数ノードにまたぐ変更は Workflow の承認ゲート+段階適用(§10.5・canary→broad、stage 失敗で halt)、`on_failure: halt` で切り戻し不能時は人へ。ただし切り戻し「手順書」そのものの生成・添付は Sakutei の管轄外。

### 監査証跡要件と S4 監査 state の適合

- **適合度は高い**: §17.5 の監査は append-only + 連鎖ハッシュ + 外部アンカー(git head hash の日次記録)で改竄耐性を持つ。**変更の原因タグ A/B/C/D/E がそのまま監査の帰属になる**(自己修復か・誰の commit 由来か・どの承認ゲートを誰が通したか)——通信事業法が求める「誰が・なぜ」に構造的に答えられる。git(intent 変更履歴)+ Vault audit log(秘密アクセス)+ Event(S4)の三者で監査が揃う。
- **不足**: (a) S4 の永続化先が §26.2 で**未定義**(「別ストア」とだけ)——通信事業法級の保存年限・可用性要件を満たす証跡ストアの具体化が要る。(b) 承認ゲートの承認者記録はあるが、**保守窓申請・作業計画との突合**(この Apply がどの承認申請番号に紐づくか)は設計に無く、外部の変更管理システム連携が要る。(c) 「作業前に人が dry-run を確認した」証跡(plan の承認)は §10.5 のゲートで表現できるが、Resource 収束(auto)には plan 承認の段がない。

---

## 6. 段0ブートストラップの当てはめ

- 段0(Proxmox 3ノード + 統合 Ceph + VM-HA で Vault/broker/reconciler を保護・§19)は各サイトに 1 セット必要。2 サイトなら段0 も 2 セット(冗長と一致)。
- **管理系/データ系の分離(§19.3)がテレコムに好適**: NFVI はデータプレーン(VNF トラフィック・SR-IOV パススルー)と管理系が明確に分かれる。reconciler は管理系経由で全ノードに到達し、データ系(SR-IOV/DPDK が使う高速 NW)が全断しても自分で直せる=single point of blindness にならない。OOB/コンソールサーバは L2.5 Break Glass(§15.6)。
- **PTP の grandmaster は tier: foundation 級**の依存の根(時刻がずれると VNF・同期網が倒れる)。DNS/DHCP と同様に §6.1 の foundation 帯に置く。
- **注意**: 段0 の Proxmox 自体は NFVI の SR-IOV/DPDK 要件を負わない(段0 は制御面)。段0 と NFVI 本番でハイパーバイザ要件が異なる点は運用上明確に分離すべき。

---

## 7. 成り立たない点・苦しい点・設計ギャップ・必要な拡張(忖度なし)

1. **【最大】remediation mode の時間帯スケジュールが存在しない**: 「保守窓のみ auto、他は alert-only」は本構成の生命線だが、設計にこの機構が無い。手動 freeze/unfreeze でしのげるが、SLA 99.999% の現場で「オペレータが unfreeze し忘れ/し過ぎ」のヒューマンエラーを機構で防げない。→ **`remediation.schedule`(時間帯 × 曜日で mode を切替、保守窓カレンダー連携)を新規に設計する必要がある**。freeze 機構の拡張として実装可能だが、設計追加は必須。

2. **【重い】保守窓外の drift をどう扱うかの運用矛盾**: alert-only で「保守窓外にズレを検知したが直せない」状態が常態化する。level-triggered は「毎周ズレを再報告」するため、**保守窓まで何時間も同じ drift Event が firing し続ける**。§17.4 の level 報告(firing/resolved・同じアラートを撒かない)で技術的には破綻しないが、「直せないのに鳴り続ける」ことの運用的な受容(=保守窓待ちの drift を snooze する概念)が設計に無い。→ **保守窓待ち Event の suppression/deferral 状態**が要る。

3. **【重い】SR-IOV/NUMA の managed fields 境界の脆さ**: VNFM がリアルタイムに VF/CPU/ページを掴んで動かす層に対し、「土台の宣言」と「VNFM の運用」の境界を Schema の managed で正確に引き切れるかが成否を分ける。境界を 1 つ間違えると VNF のスケール毎に drift 判定→(保守窓で)再 Apply で **VNF を巻き込む事故**になりうる。§5.5 通り Schema 設計が実装の本体で、ここは vSphere 案件より難度が高い。leaky abstraction のリスクが大きく、v1 の managed 機構だけで十分かは実装検証が要る。

4. **【中】監査証跡が通信事業法級には未達**: §5 記載の通り、S4 永続化先の具体化(§26.2 未定義)・変更管理システム連携(申請番号突合)・auto 収束時の plan 承認段が不足。証跡の骨格はあるが、法対応レベルへの肉付けは拡張。

5. **【中】シャーディング未実装(v1)**: 2 サイト冗長を shard 境界に一致させる理想形は v2。当面は 2 独立デーモンで代替できるが、サイト間の「共有基盤の readiness を跨いで gate」(§12.2 のシャード間依存=結果整合)は設計方針のみで未実装。

6. **【思想】そもそも「常時収束が回る」ことへの現場の心理的抵抗**: これは技術ではなく §23.5 と同じ組織問題。SLA 99.999% の保守ベンダは「勝手に回るもの」を本質的に嫌う。alert-only 常用+可視化ツールとしての価値(§23.2)から入り、信頼を積んでから保守窓 auto へ、という §23 の brownfield 戦略がそのまま処方箋になるが、auto 化が永遠に来ない(alert-only + 手動保守窓作業のまま)可能性も現実的にある。その場合 Sakutei は「収束エンジン」ではなく「drift 検知器 + 承認付き実行基盤」として使われる——それでも価値はあるが、level-triggered auto 収束という中核価値は発揮されない。

---

## 8. 総合判定

**△ 大幅な拡張が必要**(alert-only 常用 + 可視化 + 承認付き保守窓実行に限れば ○ 条件付き成立に届く)

**理由**: NFVI 層(SR-IOV/NUMA/HugePages/PTP/アンダーレイ)の Resource 写像・managed fields による VNFM 境界・全域 platform 層の PR 必須運用・段0 の管理/データ系分離・原因タグベースの監査帰属は、いずれも本構成に良く適合し、思想レベルでは「保守窓外は Observe だけ回し、保守窓に入ったら揃った drift を承認して直す」という level-triggered の存在意義がむしろ肯定される。しかし本構成の生命線である **(1) 時間帯連動の auto/alert-only 自動切替、(2) 保守窓待ち drift の deferral、(3) 通信事業法級の証跡ストア** が v1 に存在せず、いずれも新規設計を要する。SR-IOV/NUMA の managed 境界の脆さと相まって、無改造での成立は困難。

**成立条件**:
- 短期(改造なし): 全域 alert-only 固定 + 保守窓に手動 unfreeze + 承認ゲート付き Workflow で実行、という **「drift 検知器 + 承認付き実行基盤」としての限定運用**なら ○ に届く。§23 の brownfield 導入戦略(可視化から入り信頼を積む)を厳格に適用する。
- 本来運用(要拡張): `remediation.schedule`(保守窓カレンダー連動 mode 切替)・保守窓待ち Event の deferral 状態・法対応証跡ストア(S4 永続化先の具体化 + 変更管理連携)・サイト単位シャーディング(v2)を実装し、SR-IOV/NUMA の managed 境界を devenv/twin(§22)でカオステスト検証してから本番へ。
