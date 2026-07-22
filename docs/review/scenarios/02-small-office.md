# 対象構成02:スモールオフィス/SOHO への Sakutei 適用性レビュー

**総合判定:✗ 不成立(現行設計のまま入れると害。ただし §23 の「可視化ツール」用途に限れば △ で残る)**

Sakutei の設計はデータセンター規模の複雑さ(手続き・物理・テナント・DR・自己運用)を吸収するために組まれており、その代償として段0(Proxmox 3ノード + 統合 Ceph + VM-HA + Vault + broker)という重い不変基盤を前提にする。本構成はこの前提の逆——「予算極小・IT担当1名(コーディング浅)・物理2台・クラスタなし独立運用」——であり、**エンジンが要求する土台のほうが管理対象より重い**という倒立が起きる。以下、各観点で具体化する。

## 1. 構成の前提整理

| 項目 | 内容 | Sakutei 前提との差 |
|---|---|---|
| 物理 | Proxmox 2台・**クラスタなし独立運用** + QNAP NAS | 段0は「Proxmox 3ノード + 統合 Ceph + VM-HA」を要求(§19.2)。3ノード未満では VM-HA・Ceph quorum が成立しない |
| VM | 約30個(ファイル/Wiki/会計/WinServer AD/監視/VPN GW) | 数十リソースは単一デーモンの射程内(§12.2)。規模は問題にならない |
| NW | UniFi 一式(UDM Pro + SW2 + AP4)。**Controller が独自の収束機構を持つ** | §11.2-1「収束器の上の収束器」。UniFi は宣言的 API が弱く TrueObserve/List が困難 |
| 運用 | IT担当1名・兼任・コーディング浅・予算極小 | Schema 設計・原因タグ・prune 境界宣言は「注意深さ」を強く要求(§5.5)。単一障害点の人 |
| 経路 | VPN 経由リモートワーク。git は**社内VM上の Gitea** | intent が管理対象インフラの上に載る循環(§3.3 の鶏卵を悪化させた形) |

規模(30 VM)は全く問題ない。**問題は土台の重さ・provider 品質・運用者の器の3点**に集中する。

## 2. リソースモデリング

主要な写像自体は素直に効く:VM=Resource(managed fields + pruneable、§20.2)、AD/ファイル共有の設定=Resource(標準〜最小階級)、バックアップ構成=Resource+結果は canary(§18)、UniFi=Resource だが下記の難所。会計DBの中身・スキーマは**収束外=対象外/Task**(§20.1)で正しく落ちる。手続き(VM 再起動を伴う変更・OS導入)は Task/Workflow。

**必要 Provider 一覧(◎=良/○=可/△=苦/✗=困難):**

| Provider | 対象 | TrueObserve / List / Converging / CommitConfirm | クラス | identity 設計 | 実装難度 |
|---|---|---|---|---|---|
| `proxmox_vm` | 30 VM の作成・電源・スペック | ○ / ○ / △ / ✗ | 完全〜標準 | `vmid`(allocator 払い出し)。node は managed(§例) | 中(参照実装あり) |
| `proxmox_host` | 2ノードの設定 | ○ / △ / — / ✗ | 標準 | 物理トポロジ座標(§5.5)。**クラスタなしなので容量プール判定が無意味化** | 中 |
| `winserver_config` | AD/DNS/ファイル共有 | △(WinRM/`*_info`) / ✗ / ✗ / ✗ | 最小(Ansible ラップ) | ホスト名+役割 | 高(AD は非冪等要素多) |
| `qnap_share` | NAS 共有・ジョブ | △ / ✗ / ✗ / ✗ | 最小 | share 名 | 高(公式宣言 API 貧弱) |
| **`unifi_*`** | UDM/SW/AP 設定 | **✗〜△ / ✗ / ✗ / ✗** | **最小** | site+device MAC(MAC は変更されうる=identity 不適・§5.5) | **最高** |
| `dns` / `dhcp` | 基盤サービス(`tier: foundation`) | ○ / ○ / — / — | 完全 | zone+name | 低(§24.4 の最初の一歩) |
| `backup_job` | Proxmox/QNAP バックアップ | ○ / ○ / — / — | 標準 | job 名。結果は canary | 低〜中 |

**UniFi=「収束機構の上の収束」の扱いが最大の設計課題**。UniFi Controller は自前の provisioning ループを持ち adopt/provision 状態を内部で駆動する。Sakutei から見ると (a) Converging を検知する API 概念が乏しく、Apply 直後の provisioning 中を drift と誤認しやすい→ §10.4 Tier2「自傷猶予(settle 窓)」で床は張れるが Tier1 ネイティブ写像は不可、(b) List の網羅性が保証しにくく **prune を安全に有効化できない**(§4.3 で List:false → prune 拒否が正しく効く=安全側だが、UniFi の drift 自動修復という主目的が達成できない)、(c) commit-confirm が無く、UDM Pro への誤 config で**自分の管理到達経路(VPN 含む)を切ると復旧不能**(§11.2-5/§19.3 のロックアウトが SOHO 規模でも起きる)。結論:**UniFi は Sakutei で「触る」より alert-only の可視化に留めるのが妥当**で、収束の主戦場にならない。

## 3. ownership・DesiredSource・sharding

- テナント分離は不要(単一組織)。全リソースを実質 `ownership: platform` として扱うのが自然。だが platform 層は **PR 必須・alert-only 寄り・rate limit 厳格**(§12.1)——これは「1名運用で PR レビューの相手がいない」現実と衝突する。PR の意義(履歴・単一書込経路)は残るが**レビューは自己承認**になり、原因タグ A(意図的変更)の帰属監査は形骸化する。
- DesiredSource は FileSource(git)一択で妥当。DBSource/allocator/broker/UI は v2 以降(§24.2)なので v1 スコープに収まる=**むしろ小構成向き**。
- sharding は不要(§12.2 の「〜数千=単一デーモン」)。ここは完全に適合。

## 4. render(inventory / allocation / secret)の使い方

- inventory:CMDB は過剰。当面ファイル inventory で足りる(§16.5)。
- allocation:VLAN/IP を UniFi/ルータが握るなら allocator は不要。使うなら SQLite 最小実装(§16.5)。**予算極小には過剰装備**で、経路A(理想状態に直書き)で十分。
- secret:Vault が段0に**必須**(§19.2/§14)。これが本構成で最も重い要求。物理2台に Vault + reconciler を載せると、**Vault は再起動で sealed に戻り unseal は人手**(§19.4)。IT担当1名が不在の夜間に Vault ノードが落ちると、シークレット依存 Apply が MTTR=人間の unseal 時間ぶん止まる。SecretRef キャッシュ/alert-only 縮退(§14.3/§15.5)で延命はできるが、**運用者 1名の可用性がそのまま系の可用性の天井**になる。

## 5. 安全機構の適用

ここは Sakutei が本構成に与える数少ない明確な価値。rate limit + halt(§10.2)・remediation `alert-only`(爆発半径ゼロ)・グローバル freeze(§10.6/§23.6 の脱出口)は、**1名運用で誤記1つの被害が致命的**な環境でこそ効く。特に「1コミットで全 VM 削除が原理的に可能」(§19.5)への防御は必須。ただし安全機構は**正しく設定されて初めて効く**——`pruneable`/`scope managed`/`safety` の閾値を運用者が理解して書けるかが前提で、コーディング浅い1名には学習負荷が高い。

## 6. 段0ブートストラップの当てはめ(2台問題 + Gitea 循環)

**ここで構造的に破綻する。**

- 段0は **Proxmox 3ノード + 統合 Ceph + VM-HA** を明示要求(§19.2)。本構成は **2台・クラスタなし独立運用**。3ノード未満では Ceph の quorum も VM-HA も成立せず、段0の可用性設計(Vault/broker/reconciler を VM-HA 保護)が丸ごと組めない。→ **段0の縮退形(1〜2台・HA 無し・Ceph 無し)は設計書に定義が無い**(明確なギャップ)。2台に無理に載せると、Vault/reconciler が載ったノードの単一障害が即「シークレット断+収束停止」になり、level-triggered の drift 自己修復という主目的が可用性で裏切られる。
- **Gitea 循環**:git は「段0から本体が消え外部(GitHub/GitLab)に置き、稼働必須依存にしない」が設計の答え(§19.4)。本構成の Gitea は**管理対象 VM の上**にある。intent の供給源が自分の管理対象に載る=§3.3 の鶏卵を悪化させた形。緩和策はある——reconciler の**ローカルミラーで全断耐性**(§19.4)は Gitea が落ちても「最後に pull した intent」で収束継続を保証する。つまり**定常運用では循環は表面化しない**。破綻するのは**新規 intent を push したい復旧時**:Gitea VM が Sakutei の誤収束で壊れた場合、直すための新しい intent を push する先が無い。→ **外部 git(GitHub private 等)へのミラー push か、Gitea を段0外の物理/別筐体に逃がす**のが最小の回避策で、これは追加コスト。

## 7. 成り立たない点・苦しい点・設計ギャップ(最重要・忖度なし)

1. **段0が構成より重い(致命)**。3ノード+Ceph+VM-HA を要求する不変基盤を、2台・予算極小に載せられない。**Sakutei は段0縮退形を定義していない**——これは本構成にとって設計の空白であり、埋めない限り正攻法では入らない。
2. **provider 品質が UniFi/QNAP/WinServer で最小階級に落ちる**。§21.3 の通り最小階級(Ansible ラップ)では TrueObserve なし→原因タグ A/B 区別不能・converging 検知不能・破壊的重み付けが破壊側固定。本構成の主要機器(UniFi/QNAP/Win AD)がほぼここに落ち、**Sakutei の保証群(drift 検知漏れゼロ・原因区別)が「触っていないのに戻る」体験を提供できるコア provider(DNS 等)にしか効かない**。SOHO の実インフラの大半は保証の外。
3. **UniFi の二重ループ + ロックアウト**。UnFi Controller 自身が収束機構を持ち、Sakutei で収束させると綱引き(§10.4 oscillating で自動検疫される=安全だが、要は「管理できない」)。commit-confirm 無しで UDM Pro を触ると VPN 込みの到達経路を自爆しうる(§19.3)。**UniFi は Sakutei の管理対象にせず、alert-only 可視化に留めるべき**。
4. **運用者1名 × 学習曲線**。Schema 設計(§5.5:identity/managed/ordered/secret を間違えると安全性が無効化)・原因タグ・prune 境界・rate limit 閾値は、いずれも「コーディング浅い兼任1名」には重い。**Sakutei は運用の泥臭さを引き受ける代わりに、設定の正しさを運用者に強く要求する**。SPOF の人が倒れると系が凍る。
5. **可用性の天井が人**。Vault sealed-on-restart の unseal・Break Glass(オフライン金庫・Shamir 分割 §15.6)・段0再構築(Ansible)——これらは全て「複数人 or 訓練された運用」を暗黙前提にする。1名では金庫分割管理も回らない。
6. **必要な拡張**:(a) **段0縮退プロファイル**(1〜2台・HA/Ceph 無し・Vault を file backend or SOPS 等の軽量代替に落とす seam)。(b) **PR 不要モード**(単一運用者向けに platform 層の PR 必須を緩め、履歴だけ git に残す)。(c) UniFi/QNAP 等**閉じた収束機構を持つ機器を「観測専用アダプタ」として一級市民化**する扱い(現状は最小 provider に押し込むしかない)。

## 8. 総合判定:✗ 不成立(§23 可視化用途に限れば △)

**理由**:規模(30 VM)・sharding 不要・v1 スコープ収まりという点では小構成に向く要素がある。しかし **Sakutei の入場料である段0(3ノード+Ceph+VM-HA+Vault+broker 相当)が本構成の予算・台数・人員を構造的に超過**し、設計書はその縮退形を定義していない。加えて主要機器(UniFi/QNAP/WinAD)が provider 最小階級に落ち、Sakutei の中核価値(原因区別・converging・prune 安全性)が効かない。**「作庭」に例えれば、一坪の庭に DC 規模の灌漑設備を敷こうとしている状態**。

**条件付きで成立させるなら**:収束エンジンとしてではなく、§23 の**導入段階0=alert-only の可視化・drift 検知ツール**として、外部 git(GitHub private)+ 軽量シークレット代替 + 1〜2台縮退段0(HA 無しを許容)で入れ、Apply は DNS/バックアップ構成など**壊れても局所的なコア provider だけ**に限定する。この範囲なら △(条件付き)まで上げられるが、UniFi/QNAP/AD を含む「インフラ全体の自律収束」という本来の姿は本構成では ✗ である。予算極小の SOHO には、Sakutei よりも Ansible の定期実行 + 手運用のほうが総コストで勝る蓋然性が高い。
