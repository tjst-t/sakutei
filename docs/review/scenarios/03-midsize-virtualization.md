# 対象構成03: 中堅企業の仮想化基盤 — Sakutei適用可能性レビュー

**総合判定: ○ 条件付き成立**(Proxmox/VM/監視/DNS 等の中核レイヤは素直に成立。ただし FortiGate HAペア・Catalyst の ordered ACL・段0と本番の同型性の3点で、設計の空白か運用側の割り切りを要する)

---

## 1. 構成の前提整理

- ハイパーバイザー: Proxmox 3ノード + 統合Ceph。**設計書 §19.2 の段0構成とほぼ同型**という点が本シナリオ最大の特徴(後述7・6)。
- VM 約100個: 基幹系(ERP/DB)、社内システム、Windows AD 2台、Zabbix。規模は §12.2 の「〜数千 = 単一デーモン」帯に完全に収まり、シャーディング不要。
- ネットワーク: FortiGate FW HAペア、Catalyst L2/L3 6台、拠点間IPsec 2本。ここが「泥臭い層(§11.1)」で実装難度が集中する。
- 組織: 情シス3名・uptime重視の保守的文化・**月次メンテウィンドウ**。これは level-triggered 常時収束との相性が本シナリオの思想的争点(後述5)。
- 周辺: バックアップ=PBS、git=社内GitLab。GitLab は §19.4 の「git は外部・稼働必須依存にしない」にそのまま乗る。

この構成は greenfield ではなく **brownfield 導入(§23)**であり、既存100VM・稼働中FW/SWへの後付けになる。§23 の段階戦略(alert-only → import → scope単位昇格)が全体の適用フレームになる。

## 2. リソースモデリング

Resource/Task/Workflow への写像は概ね素直:
- **Resource(収束)**: VM定義(§20.2/例doc)、DNSレコード、FWルール集合・ACL、監視設定(observability as Resource §17.2)、PBSバックアップジョブ構成(§18)、VLAN/L3。
- **Task(一回性)**: vCPU増・ディスク拡張(reboot要 §20.2)、Proxmox/機器のファームウェア更新(多相 §9.1)、DBマイグレーション。
- **Workflow(順序+invariant)**: Proxmoxローリングアップデート(quorum維持 §9.2)、FortiGate HAフェイルオーバー、DR、ホスト退避(容量充足+DRS委譲 §11.3)。

**必要Provider一覧:**

| Provider名 | 対象 | Capabilities | クラス | identity設計 | 実装難度 |
|---|---|---|---|---|---|
| `proxmox_vm` | 100VM | TrueObserve / List / Converging(UPID) / CommitConfirm:false | 完全 | vmid(allocator払出・computed)。node は managed field | 中(例docで実証済) |
| `proxmox_node` | 3ノード | TrueObserve / List / Converging | 完全 | 物理トポロジ座標(§5.5)。シリアルは computed | 中 |
| `ceph_pool` | Ceph | TrueObserve / List / Converging(内部収束) | 完全 | pool名 | 中〜高(二重ループ §11.2-1) |
| `dns_record` | 内部DNS | TrueObserve / List / CommitConfirm:false | 完全 | zone+name+type | 低(最初の一歩 §24.4) |
| `fortigate_policy` | FW HAペア | TrueObserve / List / CommitConfirm(要検証) | 完全〜標準 | **policy list全体が原子単位(ordered §8.5)** | **高** |
| `catalyst_acl` | ACL(6台) | CLI scrape / CommitConfirm:機種依存 | 標準〜最小 | **ACL全体が原子単位(ordered)** | **高** |
| `catalyst_vlan_l3` | VLAN/L3/LAG | NETCONF or CLI scrape / CommitConfirm(IOS-XE:reload timeout) | 標準 | インターフェース名/VLAN ID | 中〜高 |
| `ipsec_tunnel` | 拠点間2本 | FortiGate API / TrueObserve | 標準 | ローカル/リモートID | 中 |
| `zabbix_monitor` | 監視設定 | TrueObserve / List | 完全 | ホスト+item key | 低(observability as Resource) |
| `pbs_backup_job` | バックアップ構成 | TrueObserve / List | 完全 | job名。**結果健全性は health/canary で別管理(§18)** | 中 |
| `windows_ad`(層1) | AD 2台 | WinRM/Ansible | 最小 | — | 中(Ansibleラップ §21.3) |

**FortiGate HAペアの扱い(注意点)**: HAペアは論理的に1台(config同期はFortiGate自身が担う)。Sakutei からは **1つの Resource(management IP宛)として収束**し、ペア内同期は「収束器の上の収束器(§11.2-1)」として委譲するのが正しい。ペア個別を2 Resource にすると綱引きになる。フェイルオーバー自体は Workflow(手動トリガ)。FWルールは §8.5 の順序付きコレクション(リスト全体=原子単位・Apply=全置換・plan表示はルール単位)。CommitConfirm 相当(FortiOS の revision/rollback)が使えるかは要検証で、無ければ管理断ロックアウト(§11.2-5)のリスクが残る。

**Catalyst の ordered ACL の扱い(注意点)**: ACL は位置依存リスト。§8.5 でそのまま扱えるが、Catalyst(IOS/IOS-XE)は真の candidate/commit を持たない機種が多く、**`Capabilities.CommitConfirm` が機種依存で満たせない**。§19.3 が「管理NW機器では CommitConfirm 必須」とする一方、Catalyst でそれが取れない場合に段0にできない矛盾が生じる(後述7)。TrueObserve も CLI scrape 依存で、Ansible ラップなら最小階級に落ち原因タグA/Bが縮退する。

## 3. ownership・DesiredSource・sharding設計

- **ownership**: 本シナリオは実質シングルテナント(情シス管理)。§12.1 の 2層のうち **platform 層がほぼ全て**になる。Proxmoxクラスタ/Ceph/コアNW/FortiGate は `ownership: platform`(PR必須・alert-only寄り・rate limit厳)。ERP/社内システムVM群を `ownership: tenant`(部門=論理テナント)として即時反映の砂場に切ることは可能だが、3名運用では過剰で、**全体を platform 一層 + PR運用**が現実的。
- **DesiredSource**: GitLab の `FileSource` 一本で足りる(§13.1)。DBSource/RenderedSource は不要。inventory は当面ファイル、VLAN/vmid 採番だけ allocator(§16、SQLite最小実装)に外出しすればよい。
- **sharding**: 不要(§12.2、100VM は単一デーモンの余裕圏)。可用性は段0の VM-HA(active 1台)で担保、リーダー選出不要。

## 4. render(inventory/allocation/secret)の使い方

- **inventory**: VM/ノード/機器の在庫はファイル正で開始。将来 NetBox 化しても RenderedSource で吸収可(コア無変更 §13.1)。
- **allocation**: vmid(例doc §1.1)と VLAN を allocator 払い出し。platform 層なので PR時 reserve → マージで confirm(§16.3)。Release は下流削除の Observe 確認後(§16.4)で、VLAN 誤再払い出し事故を防ぐ。
- **secret**: FortiGate/Catalyst/Proxmox/AD の管理資格を Vault 保存の `SecretRef`(§14.5 の層B暫定=broker未導入時)。Zabbix/DB のパスワードも SecretRef。世代を render フェーズ1で解決(§14.2)し rotation を検知。credential-broker は v2 なので当面は静的資格+定期rotate。

## 5. 安全機構の適用(月次メンテウィンドウ文化との相性)

これが本シナリオの思想的核心。**level-triggered 常時収束と「月次メンテウィンドウ」文化は一見矛盾するが、設計は既にこの緊張に答えを用意している**:

- **`alert-only` 既定 + freeze(§10.6)**: メンテ窓外は全 platform リソースを alert-only に倒す(=爆発半径ゼロ)。**drift 検知は毎周続き、修復(Apply)だけ止まる**。これはまさに保守的文化に合致する——「常に監視、変更は窓のみ」。
- **update_policy の window(§9.1)**: `window: "第N土曜 02:00-04:00"` を Task/reboot要変更に付け、窓外では起動しない。ファームウェア・vCPU変更はここに閉じる。
- **rate limit + halt-destructive-only(§10.2)**: 誤記1行での100VM一斉削除(§10.1/§19.5)を構造的に止める。3名運用こそこの装置が生命線。
- **相性の結論**: level-triggered は「常時 Apply する」ことではなく「常時 Diff を計算し、Apply の可否を mode/window/rate limit がゲートする」思想であり、**メンテ窓文化と両立する**。むしろ「窓外は alert-only の drift 検知器、窓内で承認済み変更を auto 適用」という運用は、この組織の文化にきれいに写像する。§23.5 の「alert-only で信頼を得てから auto」がそのまま導入プロセスになる。

## 6. 段0ブートストラップの当てはめ(段0と本番が同型の扱い)

段0は Proxmox 3ノード + 統合Ceph + VM-HA(§19.2)であり、**本シナリオの本番ハイパーバイザーと同型**。この同型性は諸刃:

- **利点**: 段0を別途調達する必要がなく、本番Proxmoxクラスタ上に段0 VM群(Vault/reconciler/(将来broker))を VM-HA で載せられる。devenv/twin も Proxmox ネスト(§22.2 層C)で同一材料で組める。
- **危険(重要)**: 段0が本番と物理的に同一クラスタに同居すると、**「Sakutei が本番Proxmoxを収束対象にしつつ、その上で自分が動く」自己言及**が生じる。§19.3 の段の切り分けを厳密に適用する必要がある——**同じクラスタでも、reconciler/Vault ホストの収容ポート・最小L2/L3・段0 VMのHA設定は段0(Ansible管理・Sakutei不可侵)**、それ以外のVM・pool・NW config は段1(Sakutei収束)。§11.2-4 の managed fields と同じ「同一対象の中で段が分かれる」を、クラスタ丸ごとに適用する。ここを曖昧にすると「Sakutei が自分の土台を prune する」事故に直結する。段0 VM群・段0関連poolは `pruneable:false` かつ Sakutei の scope 外に固定すべき。

## 7. 成り立たない点・苦しい点・設計ギャップ・必要な拡張(忖度なし)

1. **【最大】v1スコープと本シナリオ必須機能の乖離**: Task/Workflow は **v2以降(§24.2)**。しかし本構成では Proxmoxローリングアップデート・ファームウェア更新・HAフェイルオーバー・DR・reboot要のVM変更(vCPU/ディスク)が**日常運用の中核**であり、これらは全て Task/Workflow に載る。v1(Resource収束のみ)で導入すると、「収束できるのはVM定義/DNS/監視/FW/ACL の Day-2 維持だけで、危険な手続きは全て手作業のまま」になる。**Resource 層だけでも drift検知器として価値は出る(§23.2)が、"運用が回る"には v2 が事実上必須**。これは不成立ではないが、導入時期を選ぶ。

2. **【NW機器】CommitConfirm が取れない機器の段0矛盾**: §19.3 は管理NWコア機器に `Capabilities.CommitConfirm` を**必須**とするが、Catalyst(特に旧IOS)は真の candidate/commit を持たない。管理断ロックアウト(§11.2-5)を commit-confirm で防げない機器が管理NWコアに居ると、設計の前提が満たせない。緩和は (a) `reload in <n>` による疑似ロールバック実装(IOS-XE)、(b) 当該機器を段1に留め alert-only 固定、(c) OOB(コンソールサーバ)を Break Glass L2.5 として必ず持つ、のいずれか。**OOB経路(§19.3・L2.5)がこの構成では必須**(拠点間IPsec/コアSW障害時の唯一の復旧路)。設計上の穴ではないが、機器実装難度が「高」に張り付く。

3. **【FortiGate HA】収束粒度と config同期の綱引きリスク**: HAペアを1 Resource として扱えば設計に乗るが、FortiOS の revision管理・auto-rollback を Provider が正しく写像できるかは未実証。ここが甘いと FWルール全置換(§8.5)で一時的にトラフィックが切れる・管理断が起きるリスクがある。フェイルオーバー Workflow は v2 待ち。**FortiGate/Catalyst の自前 Provider 品質が本シナリオの成否を実質決める**(§21.1 の「価値の大半は provider の数と品質」がそのまま効く)。

4. **【brownfield】既存100VM・稼働FWへの後付けコスト**: import(§23.3)で現実→理想を生成できるのは楽だが、100VM分の managed fields 切り分け・意図の整理は人力(§23.3注意)。Sync Mode(§23.8)で移行期間を吊れるが、3名で全scopeを昇格基準(N日diffゼロ §23.4)まで持っていく工数は数ヶ月級。**「既存全部を管理下に」を目標にしない(§23.7)**割り切りが前提。

5. **【組織】3名・保守的文化の受容リスク**: §23.5 が明言する通りこれは技術でなく組織問題。「勝手に戻す」を一度でもやると殺される。alert-only 期間を長く取り、脱出口(freeze §23.6)を最初から握らせる運用設計が必須。技術的成立と導入成功は別。

6. **【自己言及】段0同居**: 6章の危険。設計に解(§19.3)はあるが、運用者が段の境界を正しく維持し続ける規律に依存する。ドキュメント化・scope固定の徹底が要る。

**必要な拡張(コア変更を要さないもの)**: Task/Workflow(v2予定・既定路線)、FortiGate/Catalyst の自前Provider(§21.4)、Catalyst向け疑似commit-confirmラッパー(§21.3の「賢さは自前側」)。**コア型の変更を要する新規拡張は見当たらない**——本シナリオは設計が想定した適用範囲(オンプレ全レイヤ・§19.2の段0構成そのもの)に収まっている。

## 8. 総合判定

**○ 条件付き成立。**

**理由**: 対象構成は設計書 §19.2 の段0構成・§20/§11 のオンプレ全レイヤ・§23 の brownfield 戦略が**全て正面から想定している範囲**に収まっており、規模(100VM)も単一デーモン圏。VM/DNS/監視/バックアップ構成/FW・ACLの Day-2 収束と drift検知は素直に成立し、月次メンテウィンドウ文化も alert-only + freeze + window で思想的にきれいに写像できる。設計ギャップというより「v1スコープでは道具が半分」という時期の問題である。

**成立の条件**:
1. **Task/Workflow(v2)の実装完了を待つ**か、v1では「Resource収束+drift検知器」に用途を限定する(ローリング/FW/DR等の手続きは当面手作業)。
2. **FortiGate・Catalyst の自前Provider を高品質に実装**し、Catalyst の CommitConfirm 欠如を疑似ロールバック or alert-only固定 + **OOB経路の常設**で補う。
3. **段0(Proxmox同居部分)の scope を厳密に分離・固定**し、Sakutei が自分の土台を prune しない不可侵境界を維持する。
4. **brownfield は §23 に従い alert-only → scope単位昇格**で進め、「既存全部管理下」を目標にせず、組織の信頼獲得を優先する。

これらが満たせれば実運用に耐える。満たせない(特に条件2のNW機器品質、条件3の段分離)場合は、NW/ハイパーバイザー中核が永続 alert-only に留まり、実質「可視化・drift検知ツール」に縮退する——それでも中堅企業には十分な価値がある、という着地になる。
