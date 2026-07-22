# 適用性レビュー 01: ホームラボ(最小構成)

**総合判定: △ 大幅な拡張が必要(条件次第で ○ に届く)**
— コア収束モデル(観測 state レス・level-triggered・prune・爆発半径)はこの規模でこそ価値が出るが、**段0(3ノード + Ceph + Vault-HA)がそのまま物理的に不可能**であり、段0を「単一ノード縮退版」として再設計する拡張が前提になる。設計 v2.2 は段0の縮退モードを規定していない(§19 は3ノード固定)。ここが最大のギャップ。

---

## 1. 構成の前提整理

- **物理**: Proxmox 1台(N100 / 32GB)、Synology NAS 1台(NFS/SMB)、OpenWrt ルータ、非管理型 L2 スイッチ1台。
- **ワークロード**: VM/CT 10〜15(Pi-hole/CoreDNS、Home Assistant、メディアサーバ、監視、Caddy、k3s 1ノード)。
- **人的資源**: 管理者1名(趣味)。24/365 の運用も専用 HA も組めない。
- **可用性の現実**: 停電・再起動が日常。単一障害点(Proxmox 1台)が全体障害点。
- **git**: GitHub 個人リポジトリ(外部)。これは v2.2 §19.4 の「git は外部・稼働必須依存にしない」と整合。

この規模は設計書 §12.2 の「〜数千リソース = 単一デーモン・毎周全 Observe」帯の**さらに下限**(数十リソース)。スケール面は完全に余裕。問題は**可用性の底(段0)**に集約される。

## 2. リソースモデリング

主要素の写像は素直に決まる。「産む/焼く/join する瞬間 = Task/Workflow、それ以外 = Resource」(§20.2)。

- **Resource(収束)**: 各 VM/CT の定義(cpu/mem/disk/image/power_state)、Pi-hole/CoreDNS の DNS レコード、Caddy のリバースプロキシルート、NAS 上の NFS 共有・データストア宣言、Prometheus scrape/alert 設定(observability as Resource・§17.2)、TLS 証明書(SecretRef + expiry health)、k3s ノード VM。
- **Task**: VM の再起動を伴う変更(vCPU 増・ディスク拡張)、golden image ビルド、証明書 rotation の実行手順。
- **Workflow**: Proxmox/Synology の DR リストア、Home Assistant の設定リカバリ。ただし**1名運用では Workflow の invariant(min_active 等)は意味が薄い**(冗長台数が無い)。実質 Task で足りる。

### 必要 Provider 一覧

| Provider名 | 対象 | Capabilities (TrueObs/List/Converging/CommitConfirm) | クラス | identity 設計 | 実装難度 |
|---|---|---|---|---|---|
| `proxmox_vm` | VM/CT 定義・電源・スペック | ✓ / ✓ / ✓ / ✗ | 完全 | `vmid`(allocator 払い出し)。node は配置ヒント、current_node は managed | 中(例あり) |
| `dns` (Pi-hole/CoreDNS) | DNS レコード | ✓ / ✓ / ✗ / ✗ | 完全 | zone + name。§24.4 の最初の一歩 | 低 |
| `caddy` | リバプロ route/TLS | ✓ / ✓ / ✗ / ✗ | 完全 | route matcher key。API/Caddyfile 経由 | 低〜中 |
| `synology_share` | NFS/SMB 共有・LUN | △ / △ / ✗ / ✗ | 標準〜最小 | 共有名。DSM API は癖が強い | 中〜高 |
| `podman`/`docker` | コンテナ(VM 内) | ✓ / ✓ / ✗ / ✗ | 完全 | コンテナ名 | 低 |
| `prometheus_config` | scrape/alert ルール | ✓ / ✓ / ✗ / ✗ | 完全 | ルール名 | 低 |
| `tls_cert` | 証明書存在・鮮度 | ✓ / ✗ / ✗ / ✗ | 標準 | ドメイン名 | 低(Vault/ACME 委譲) |
| `openwrt` | ルータ設定(VLAN/FW/DHCP) | △ / △ / ✗ / ✗ | 最小(Ansible ラップ) | 物理ポート/インタフェース名 | 中 |
| `k3s_node` | k3s ノード参加 | Ansible ラップ想定 | 最小 | ノード名 | 中 |

**注**: OpenWrt / Synology / k3s は自前 provider が割に合わず、§21 の Ansible ラップ(最小階級・`TrueObserve:false`)が現実解。結果として**横断保証(原因タグ A/B 区別・converging・破壊的重み付け)がこれらでは縮退する**(§21.3)。完全階級で効くのは proxmox_vm / dns / caddy / podman に限られる。

## 3. ownership・DesiredSource・sharding 設計

- **ownership**: 実質全リソースが `platform`。テナントは存在しない(§12.1 の2層のうち tenant 層は不要)。→ 全リソースが「PR 必須・alert-only 寄り・rate limit 厳・FileSource(git)」の platform プロファイル。**ただし1名運用では「PR 必須」は自己 PR の儀式**になり、実利は薄い(§7 で後述)。
- **DesiredSource**: `FileSource(git)` 一択。DBSource / RenderedSource は不要。GitHub 個人リポジトリ + 各ノード(=単一ノード)ローカルミラー。§19.4 の全断耐性がそのまま効く。
- **sharding**: 不要。単一デーモン・単一シャード。allocator も `vmid` プールと(使うなら)DHCP レンジの最小実装(SQLite + アトミック払い出し・§16.5)で足りる。IP は OpenWrt DHCP に委譲し、allocator は vmid のみ管理する割り切りが妥当。

## 4. render(inventory / allocation / secret)の使い方

- **inventory**: 外部 CMDB は不要。ノード1台・VM 十数個は intent ファイルに直書き or 小さな YAML inventory で十分。RenderedSource は使わない。
- **allocation**: `vmid` の払い出しのみ(§16.1 のパターン)。VLAN は非管理 L2 スイッチのため VLAN 分割自体が困難 → allocator の VLAN プールは実質空。IP は DHCP 委譲。
- **secret**: Vault を SecretBackend として1つ。Home Assistant のトークン、DNS API キー、cloud-init パスワードを SecretRef 化(§14.5 の暫定経路 = 静的資格 + rotate)。**ここで段0問題が直撃する**(§6)—Vault が段0内に必要だが単一ノードでは HA が組めない。

## 5. 安全機構の適用

この規模でこそ安全機構の価値が出る(§19.5「1コミットで全 VM 削除が原理的に可能」は1台構成でも同じ)。

- **remediation mode**: 導入初期は全 `alert-only`(§23.2)。停電復帰後に手で直した状態を勝手に潰さないため、**恒常的に alert-only 寄りに置く**運用が現実的(1名では auto 暴走の監視ができない)。
- **rate limit**: `max_changes_per_cycle` を絶対値小さめ(例 absolute:3, destructive-only halt)。リソース総数が数十なので percentage より絶対値が効く。
- **pruneable**: 既定 false を厳守。VM/volume/NAS 共有の prune は明示 opt-in。データ消失が家庭内で致命的(写真・録画)。
- **freeze**: 停電・メンテ前に1コマンド freeze(§10.6)。停電再起動が日常なので、**起動直後の一斉収束を抑制する freeze/settle 窓**が実務上重要。

## 6. 段0ブートストラップの当てはめ(最重要の縮退)

設計 v2.2 §19.2 の段0は **Proxmox 3ノード + 統合 Ceph + VM-HA + Vault** を「確定」としている。本構成では**3ノードもCephもVM-HAも物理的に不可能**。縮退の当てはめ:

- **判定基準は生きる**(§19.3「壊したとき自分で直せなくなるか」)。段0の**役割**は縮退しても消えない。
- **単一ノード段0**: Proxmox 1台に Vault + reconciler を VM で載せる。VM-HA は無い → **ノード障害 = 段0全滅 = 手動再建(Ansible + unseal key)**。§15.6 の L3(段0全滅)が日常運用の一部になる。
- **Ceph → NAS**: 統合 Ceph の代わりに Synology NFS を VM ストレージに。**NAS が段0の一部に格上げ**される(落ちると VM が起動不能)。設計が段0外に置いた NAS が SPOF 段0要素になる。
- **unseal**: v2.2 §19.4 の「Vault は再起動で sealed に戻り、unseal はオペレータ起動」が**停電のたびに発生**する。停電が日常なので、MTTR に毎回「人間の unseal 時間」が乗る(§19.4 の但し書きが常態化)。→ **auto-unseal(NAS 上のファイル/TPM)への縮退**が事実上必須だが、これは「シークレットの根を人間の金庫に着地させる」原則を緩める。趣味ラボでは許容だが**設計原則との明示的トレードオフ**。
- **OOB / コンソールサーバ**: 存在しない。L2.5 Break Glass 経路が無い。→ Proxmox のローカルコンソール(物理キーボード/画面)が唯一の OOB。管理者が物理アクセスできる前提なら成立。

**結論**: 段0は「単一ノード + NAS + auto-unseal + 物理コンソール OOB」に縮退できるが、これは v2.2 が明文化していない**新しい段0クラス(段0-minimal)**であり、設計拡張として定義が要る。

## 7. 成り立たない点・苦しい点・設計ギャップ(忖度なし)

1. **段0の縮退モードが設計に存在しない(最大)**: §19 は3ノード固定。単一ノード段0の可用性セマンティクス(unseal 頻発・NAS の段0格上げ・OOB 不在)を v2.2 は一切規定しない。ここを埋めないと「成り立つ」と言えない。
2. **Vault-HA 不可 × 停電日常 = unseal 地獄**: 停電のたびに Vault sealed → 手動 unseal まで SecretRef 依存の Apply が全停止(§14 の縮退で Observe は続くが Apply は止まる)。auto-unseal は原則を曲げる。**1名・趣味で最も痛いのはここ**。
3. **PR 必須ワークフローが1名運用で空回り**: platform 層の「PR 必須・人が承認」(§12.1)は複数人ガバナンス前提。1名では自己 PR で儀式コストだけ残る。**「単独運用者向けに直接 commit + 事後監査」モードが欲しい**(設計にはない)。tenant 層プロファイル(即時)を単独運用に流用する手はあるが ownership の意味が歪む。
4. **ロングテール provider だらけで完全階級が少数**: OpenWrt/Synology/k3s が Ansible 最小階級 → 原因タグ・converging・破壊的重み付けが効くのは半分以下。**Sakutei の売りの多くがこの構成では縮退側で動く**(§21.3 の正直な制約が濃く出る)。
5. **非管理型 L2 スイッチ**: VLAN/管理ポート収束の対象が無い。ネットワーク層の収束価値がほぼ出ない(OpenWrt のみが対象)。設計の NW 機器収束(§11.1)の大半が空振り。
6. **canary/synthetic check の隔離 scope が作りにくい**: §17.3 は隔離 datastore/network を要求するが、単一ノード・非管理 L2 では隔離環境の確保が苦しい。VM ライフサイクル canary は縮小せざるを得ない。
7. **twin/devenv のネスト検証は 32GB で厳しい**: §22 のネスト段0再現は N100/32GB では容量的にほぼ不可能。検証は層A(フェイク provider)中心に寄せるしかない。

**必要な拡張**: (a) 段0-minimal クラスの定義(単一ノード・NAS 段0・auto-unseal 許容・物理コンソール OOB)、(b) 単独運用者プロファイル(直接 commit + 事後監査、PR 儀式の省略)、(c) 停電復帰時の起動一斉収束を抑える boot-settle/freeze の自動化。

## 8. 総合判定

**△ 大幅な拡張が必要(拡張が入れば ○ 条件付き成立)**

- **成立する部分**: コア収束モデル(観測 state レス・level-triggered・prune・爆発半径・SecretRef・FileSource/git)はこの規模に対して過不足なく、むしろ「触ってないのに理想へ戻る」体験(§24.4)が最も安く得られる帯。スケール・sharding・マルチテナントの複雑性は全て不要で、単一デーモンで完結する。
- **成立を阻む核心**: 設計の段0が3ノード + Ceph + Vault-HA 固定であり、**本構成の物理制約とそのまま衝突**する。段0-minimal クラスと単独運用者プロファイルという2つの拡張を設計に加えて初めて「○ 条件付き成立」に到達する。拡張なしの v2.2 素のままでは**段0が組めず不成立寄り**。
- **条件**: ①段0-minimal(単一ノード + NAS 段0 + auto-unseal + 物理 OOB)を許容・明文化する ②停電日常前提で Apply を alert-only 中心に据える ③PR 儀式を省いた単独運用モードを認める。この3条件が揃えば、趣味ラボの「drift 自己修復付き宣言的管理」として実用価値がある。
