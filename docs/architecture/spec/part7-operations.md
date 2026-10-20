> Sakutei 設計書 v3.0 の一部（設計の正）。全体目次と §→ファイル対応は [README.md](README.md) を参照。§番号は全 8 部を通して一意。

# 第VII部 運用ライフサイクル

本部は Sakutei を「現実の基盤に載せ、事故から復旧し、動作を検証・監査する」までの運用ライフサイクルを定める。第II部〜第VI部が機構そのものを定めるのに対し、本部はその機構を**時間軸に沿ってどう立ち上げ・移行し・守るか**を定める。貫く姿勢は「制御面は落ちてもよい・データ面は動き続ける」「壊れる前に止まる」「事前チェック最小・事後監査最大」の三つである。

---

## §23 ブートストラップ

### §23.1 自己を立ち上げる何かは自分の外側から来る

Sakutei はインフラを作るが、**自分自身（動作ホスト・自分の設定・資格情報）は自分では収束できない**。これは一般解の存在しない構造であり、自己を立ち上げる何かは自分の外側から来るしかない（k8s の kubeadm、Cluster API の management cluster、Terraform の state バックエンドと同型の構造）。妥協ではなく定石である。

したがってブートストラップは**2段構造**とする。

- **段0** — 人が Ansible と金庫で作る、**不変の最小の土台**。Proxmox 3ノード + 統合 Ceph + VM-HA の上に Vault / credential-broker / reconciler を載せる。
- **段1以降** — 段0 の上で reconciler が自律収束する、本番インフラ全体。

**設計目標は段0の最小化**である。段0 は「作って忘れる」不変の最小セットに保ち、変化するものはすべて段1以降へ押し出す。

### §23.2 段0の構成

```
人間（パソコン + Ansible + 金庫）
  ① 管理NW到達性の最小核  ② Proxmox 3ノード構築  ③ 統合 Ceph
  ④ VM 定義（Vault / [credential-broker=v2] / reconciler）
  ⑤ Vault unseal（★コールドスタートは Vault 先行・§23.5）
  ▼
段0（Proxmox 3ノード + 統合 Ceph + VM-HA）
  ├─ 管理NW到達性の最小核        … 段0 収容ポート + 最小 L2/L3（§23.4）。Sakutei は触らない
  ├─ Vault（VM-HA 保護）         … シークレットの根
  ├─ [credential-broker（VM-HA 保護）]  … 機器認証。reconciler の依存先。v2 で有効化
  └─ reconciler（VM-HA 保護）      … git から pull、ローカルミラーで全断耐性
  OOB：コンソールサーバ / OOB専用NW … 管理NW 全断時の唯一の経路（Break Glass）
  外部依存：GitHub/GitLab（理想状態リポジトリ）= 更新取得元。稼働必須依存にしない
  ▼ 自律収束
段1以降：本番インフラ全体
```

**credential-broker は v2 で有効化される衛星である**（§20）。v1（v1a/v1b）の段0 に broker は建てない——v1 は `reconciler → SecretRef（Vault）→ 機器` で到達し、broker への実行時依存を持たない。上図の broker 行と ④ の broker 部分は v2 で有効化される（v1 では角括弧の要素を除いて読む）。段0-minimal（§23.6）では Vault も broker も無い。

### §23.3 段0の設計判断（open-world・pull・occasional）

段0 は本システムではなく **Ansible** で構築・維持する。この役割分担は2ツールの性質差と一致する。

- **段0 = Ansible**（open-world・pull 型・稀・意図的）。段0 は不変基盤であり、その上位に「直す上位」がいない最上位ゆえ、**drift の自動修復はむしろ危険**である。人が意図したときだけ動く Ansible が正しい。
- **段1以降 = 本システム**（自律収束・常時ループ）。

段0 に固有の判断を確定する。

- **git は外部**（GitHub/GitLab）に置き、段0 内から git 本体を消す。各ノードにローカルミラーを持ち、外部 git 不達時は最後に pull した状態で収束を継続する（**稼働必須依存にしない**・§25.2）。
- **Vault と broker は段0 内**に置かざるを得ない。有状態サービスが複数になるため段0 が小プラットフォーム化し、**Ceph + VM-HA が正当化される**（Proxmox の統合 Ceph により運用重量が下がる）。収束デーモンは観測 state レス + level-triggered であり **VM 再起動の空白を許容**するため、アプリレベル HA は任意の上積みでよい。
- **unseal は Ansible（オペレータ起動）**で行う。unseal key は playbook の外に置く（Ansible Vault で暗号化 + 人の金庫）。「シークレットの根を守るものの根」は人間の金庫に着地する——これがブートストラップの無限後退の正しい終わり方である。
- **可用性の正直な但し書き**：VM-HA でも Vault は再起動で sealed に戻り、unseal はオペレータ起動である。Vault 載せノード障害の MTTR は**人間の unseal 時間を含む**（Raft HA でも sealed-on-restart は解消しない）。緩和は §20 の資格キャッシュと alert-only 縮退による。

**爆発半径は自己運用の最重要項目である。** 段0 では「1コミットで全 VM 削除」が原理的に可能なため、第IV部の安全機構は段0 の生命線となる。特に **変更予算による halt（§13）** と **remediation mode（§14）** は v1 に必須である（誤記1つで本番を壊さない装置は、事故る前に付ける）。

### §23.4 管理ネットワークの要件（段0 / 段1 の境界）

BMC・スイッチ管理ポートへ到達するための**管理NW**は段0 の定義に直結する。判定基準は段0 の一般則の特殊化である。

> **それを壊したとき、Sakutei が自分でそれを直せなくなるか。**（= single point of blindness を持つか）

これは「個別機器の config push で自分を締め出す」問題（§14 の管理プレーンロックアウト）の**データセンター規模版**である。要点は「**同じ機器の中で段が分かれる**」ことにある。

| 要素 | 段 | 理由 |
|---|---|---|
| reconciler / broker ホストの管理NW接続（NIC / IP / VLAN） | **段0** | これが無いと何にも到達できない |
| 管理NWコアの**到達性の最小核**（段0 収容ポート + 最小 L2/L3） | **段0** | 落とすと全管理対象が全盲 |
| 管理NWコアの**残りの config**（ACL / 追加 VLAN / 監視設定） | 段1 | 到達性を壊さない範囲 |
| 管理NWのアクセススイッチ（末端） | 段1 | 一部が落ちても全体は生きる |
| 各サーバの BMC 設定（IP / 認証） | 段1 | 1台死んでも他は生きる |

管理NWコアスイッチは、**到達性の最小核だけを段0**（Ansible が構築し Sakutei は触らない）とし、それ以外の config は段1（Sakutei が収束）とする。これは §5 の managed fields と同じ発想——同じ対象の中で、触ってよいフィールドと触ってはいけないフィールドを分ける。

**管理NW機器の自傷は3層で防ぐ。**

1. `tier: foundation`（§8）——管理NW機器はその先の全機器の上流であり、依存の根の典型例。
2. commit-confirm（§14）——到達性を切っても自動ロールバックする。**管理NW機器では `Capabilities.CommitConfirm` を必須とする**。
3. platform 層 + PR 必須 + 変更予算厳格（§7・§13）——管理NWコアの変更は全機器を全盲にしうる。

**管理 NW の要件（CommitConfirm 不在なら型で Apply 禁止）**：commit-confirm は Apply 契約の transactional / candidate-commit であり、v2 の NW 機器 provider で入る（v1a/v1b には無い）。commit-confirm 未実装のまま管理NWコアを Apply すると、config push で自分を締め出す経路が残る。→ **v1 では管理NWコアを Apply 対象にしない**。NW 機器 provider は `Capabilities.CommitConfirm` が false の間は「**Apply 不可・観測のみ**」を Capabilities で機械的に強制し、ロックアウト経路を型で塞ぐ。到達性の最小核はそもそも段0（Ansible）で Sakutei は触らない。

**OOB（帯域外）= ネットワーク版 Break Glass**：管理NW そのものが死んだときの経路として、コンソールサーバ（シリアル集約）/ OOB 専用NW を持つ。これは Sakutei の管理対象外に置き、Break Glass の経路（§20）として扱う。

**管理系とデータ系は別系統である。** 上記は管理系（BMC アクセス・reconciler から全機器への到達性）の話であり、実運用ではデータ系（VM トラフィック・分散ストレージのレプリケーション・live migration）がもう一系統存在する。両者は帯域も経路も異なる。データ系が全断しても reconciler は管理系経由で機器を制御でき自分で直せる（single point of blindness にならない）ため、**データ系は全て段1**とする。データ系スイッチ自体の設定変更も管理系経由で行い、「データ系を壊しても、データ系を直す経路は生きている」を保証する。

### §23.5 コールドスタート順序と段0 の SPOF

段0 は VM-HA でノード障害には備えるが、以下3点は個別に確定する。いずれも段0 の生命線である。

**（1）コールドスタート順序（Vault unseal 先行）**：reconciler は broker に、broker は Vault に、Vault は unseal（人手）に依存する。VM-HA が3つを同時起動すると、reconciler が Vault 未 unseal のまま起動し**全ターゲット unknown で空回り**する。→ 起動シーケンスを `ready_condition`（§11）相当で明文化する：`Vault unseal 完了 → broker healthy → reconciler start`。reconciler は依存が unready の間は **Observe すら試みず待機**する（空回りログの氾濫を防ぐ）。

**（2）統合 Ceph の SPOF と障害ドメイン分散**：統合 Ceph は段0 の全 VM（Vault/broker/reconciler）のストレージであり、**全損すると3つが同時に落ちる = 段0 内の単一障害点**である。3ノード Ceph の 2ノード同時障害（電源・空調・ラック単位）は quorum 喪失となる。全損時の RTO は Break Glass（§20・Ansible + unseal key）による人手再構築であることをリスクとして明記する。緩和として **Ceph の障害ドメインを分散**する（3ノードを別ラック / 別電源 / 別空調に配置）ことを段0 の人手運用要件に含める。段0 の SPOF が「共有 Ceph 1つ」に集約されている事実は隠さない。

**（3）段0 サービスの自己収束自傷を除外する**：reconciler は broker/Vault を管理対象（Resource）にでき、**誤った理想変更で自分の依存先（broker 到達設定・Vault アドレス）を壊すと、自分が全盲になり直せない**。管理NW で「到達性の最小核は段0（Sakutei は触らない）」と切り分けたのと**同じ切り分けを段0 サービスにも適用**する：reconciler は自分の broker 到達資格・Vault アドレスを**収束対象から除外**する（managed field 的除外 + `external_owner` 相当の「触らない最小核」宣言・§7）。commit-confirm はネットワーク専用でこれらには効かないため、除外で塞ぐ。

**Vault 断の全盲時間の緩和**：broker の短命資格 TTL キャッシュ（§20）は「短命」ゆえ、人手 unseal の MTTR より先に切れ、最も必要な「長時間 Vault 断」のときに全盲を防げない。→ 段0 に限り **Vault auto-unseal（HSM / Transit / cloud KMS）を選択肢として採る**。「人間の金庫に着地」の哲学は unseal key の最終保管に残しつつ（auto-unseal 用鍵を Shamir 分割 + 物理保管）、日常の再起動 unseal を自動化して MTTR から人間時間を外す。あるいは **Observe 専用の read-only 低権限資格を長め TTL で別枠キャッシュ**する（破壊はできないが全盲は防ぐ）。

段0 の人手運用として残るのは、Proxmox 3ノードのハード保守、管理NW到達性の最小核と OOB 経路の維持、Ceph 健全性監視、unseal key の金庫管理、たまのバージョンアップ（Ansible）である。頻度はハード信頼性と Vault の運用形態に依存し、Vault の sealed-on-restart はノード障害・保守再起動のたびに unseal を要求する。導入時に「段0 は片手間では回らない」ことを正直に提示する。

### §23.6 段0-minimal（1ノード縮退形）

段0（3ノード + Ceph + Vault-HA）は 1〜2 台構成には過大であり、ホームラボ・スモールオフィス帯を閉ざす。**段0-minimal を第一級の縮退形**として定義し、solo プロファイルの既定とする。

| 項目 | 段0（標準） | 段0-minimal |
|---|---|---|
| ノード数 | Proxmox 3ノード + VM-HA | **単一ノード**（HA なし） |
| ストレージ | 統合 Ceph | ローカルディスク（Ceph なし） |
| SecretBackend | Vault（VM-HA 保護） | **file ベース SecretBackend**（Vault なし。§20 seam の `file` 実装） |
| credential | broker（v2）/ SecretRef | SecretRef 直結（broker なし） |
| 可用性 | VM-HA + Ceph 冗長 | **単一障害点を許容**（トレードオフを明示） |

- **可用性トレードオフの明示**：段0-minimal はノード障害で段0 全体が止まる。だが収束デーモンは観測 state レス + level-triggered なので、**復帰後に最後の理想へ自然に再収束**でき、制御面が落ちてもデータ面（既存 VM/機器）は動き続ける。「段0 が止まる = 新規障害の自己修復が止まる」だけであり、1〜2 台構成では許容できるトレードオフである。
- **file ベース SecretBackend** は sealed-on-restart が無いのでコールドスタートが単純になる（§23.5-1 の順序制約が消える）。代わりにシークレットの保護強度は下がるため、ディスク暗号化 + アクセス制御で緩和し、規模が育ったら `SecretBackend` seam 経由で Vault へ移行する（seam があるので後付け可能）。
- 段0-minimal は **v1a で成立する**（broker/allocator/Vault いずれも不要な構成に一致する）。可視化・drift 検出器としての価値（§24.2）は 1〜2 台でも出る。

### §23.7 段0-zero（非常駐・クライアント PC からの一回性収束）

段0-minimal（1ノード常駐）でもなお、ホームラボには「常時稼働する制御面ノードを 1 台確保する」こと自体が重い。**段0-zero を最下段の運用形態**として定義する——制御面専用ノードを持たず、クライアント PC（ノート PC 可）から `sakutei reconcile`（§26.6）で**任意のタイミングに一回性収束**させる。

| 項目 | 段0-minimal | 段0-zero |
|---|---|---|
| 制御面 | 単一ノードに常駐 | **専用ノードなし**（クライアント PC から都度実行） |
| 収束 | 周期ループ | **人間キックの一回性収束**（`reconcile`） |
| ドリフト検知 | 毎周期 | **実行時のみ**（実行間は不可視） |
| S2 | ローカル file | ローカル file（PC 上） |
| SecretBackend | file | file（PC 上。ディスク暗号化を強く推奨） |

- **設計上の位置づけ**:新モードではなく既存要素の組み合わせである。`reconcile` は既存 verb（§26.6）であり、観測 state レスは実行間の状態保持をそもそも要求しない——毎回リアルを再観測するため、Terraform 的な「開いて叩く」UX でも state 鮮度問題を持ち込まない。早期収束パスの標語（§8）を借りれば「**人間 triggered な起床、level-triggered な真実**」であり、§23.6 の「制御面が落ちてもデータ面は動き続け、復帰後に最後の理想へ自然に再収束」の極限形（制御面がほぼ常に停止している）である。
- **失うもの**:周期ドリフト検知・自動修復・早期収束パス・remediation schedule。とりわけ**ドリフトは検知すらされない**（alert-only の「検知は続け」も止まる）。運用作法は「**開いたらまず `status` / `plan`**」である。久しぶりの実行で巨大 diff が出ても、変更予算・List 急減 halt・削除方向急増の凍結（§13）は周期単位で効くのでそのまま守る。
- **cooling-off のゲート化**（§14.5）:非常駐では「宣言から N 時間後に自動実行」が成立しない。段0-zero では cooling-off を「**次回 reconcile 時に N 時間経過していれば通す**」ゲート条件として評価する（タイマーからゲートへの level-triggered な再解釈）。宣言を初めて見た時刻とレーンの使用量は S4 に残し、次の reconcile が読み直す（`sakutei reconcile --audit-log <file>`。無ければレーンを開かない・§13.3「承認と運用プロファイル」）。
- **二重 writer**:複数のクライアント PC から叩く事故は、S2 file + fencing 世代（§15.2 の流用）で古い世代の書込を拒否する。
- **制約**:多相 Task / Workflow（§6.2）の実行中はプロセスを終了できない（フタを閉じない）。中断しても S2 file 永続とべき等再開で耐えるが、長時間手続きは段0-minimal 以上を推奨する。S3（前回 List 基数・last-seen）は実行間で失われるが、S3 喪失は保守側（prune 保留）に倒れる設計なので安全である（cause 帰属・§10 の質は下がる）。
- **採用ファネル**:段0-zero → 段0-minimal → 段0 の昇格経路が、OSS 初期利用者の入口（§29.4）を最小まで軽くする。clone して PC から plan → reconcile が最初の体験になる。

### §23.8 devenv は Sakutei 自身で構築する（dogfooding）

隔離検証環境である devenv（坪庭・§26.2）は、**Sakutei 自身で構築する**。本システムがインフラ収束エンジンである以上、その検証環境もまた収束の対象であり、自らを自らで立ち上げることが最も強い動作証明になる。段0 の立ち上げ手順（Ansible による不変土台 + reconciler による自律収束）を、devenv でそのまま踏襲する。

---

## §24 ブラウンフィールド

Sakutei はここまで「宣言から作る」前提で設計されているが、現実の導入は逆である——**現実が先にあり、宣言が後から追いつく**。既存基盤への導入は greenfield と質的に異なる問題であり、専用の戦略が要る。貫く原則は「**全既存インフラを配下に置くことを目指さない。価値が出る範囲だけ、安全に**」である。

### §24.1 最大の地雷：不完全な理想 × prune

> 理想状態が不完全なまま closed-world prune を有効にすると、「宣言に書かれていない = 余分」と判定され、本番の大半が削除対象になる。

これは §12 の削除セマンティクスが**正しく機能した結果**であってバグではない。Sakutei は「理想が完全である」ことを前提にするが、導入初期の理想は必ず不完全である。したがって**導入初期は prune を全面禁止する**（全 scope で `DeletionPolicy: ExplicitAbsentOnly`、`pruneable: false`）。closed-world への昇格は §24.4 の基準を満たしてからとする。

### §24.2 信頼獲得の順序：import → alert-only → スコープごと昇格

ブラウンフィールド導入は、技術以上に**組織の信頼獲得プロセス**である。Sakutei が正しく動いても、運用チームが受け入れなければ導入は失敗する。信頼は次の順序で獲得する。

**段階0：読むだけから始める（alert-only 強制）**。最初は一切 Apply しない。全リソースを `remediation: alert-only`（§14）に固定し、全 provider は Observe だけを動かす。この段階でも Sakutei は**構成の可視化ツール**として機能する——既存基盤の実態はたいてい誰も正確に把握しておらず、**実態を暴くことが最初の価値提供であり、信頼獲得の入口**になる。

**scope 単位の段階的昇格**。一気に全体を載せない。scope（§12）を最小単位で切り、1つずつ昇格させる。

```
段階0：全体 alert-only + prune 禁止（観測と可視化のみ）
段階1：1 scope を import → 人が理想を整理 → alert-only で観察
段階2：昇格基準を満たしたら auto + closed-world（prune）を有効化 ※その scope だけ
段階3：次の scope へ
```

**昇格基準**：

> その scope で **N 日間、diff がゼロ（または全て説明可能な diff）**であること。

diff がゼロにならないなら、それは「理想が不完全」か「誰かが手で触っている」かのどちらかであり、**どちらも昇格してはいけないサイン**である。管理外の領域には一切触れない（open-world のまま放置）。closed-world は境界の宣言で作られる（§12）——**境界を宣言できた範囲だけが Sakutei の領土**である。

### §24.3 人の変更を早々に巻き戻さない

既存基盤には人が手で触る運用が根付いており、Sakutei を入れた瞬間に両者が衝突する。誰かが緊急対応で設定を手で変えたとき、Sakutei が drift として即座に元に戻せば人の対応を潰す。**これが一度でも起きれば、運用チームは Sakutei を信用しなくなる**（そしてツールは殺される）。

対策は alert-only で drift 検知器として先に信頼を得ることである。導入初期に「勝手に戻す」を**絶対にしない**。人が手で変えたら Sakutei は「理想と違う」と報告するだけとし、報告を見て人が判断する（正しい変更なら理想 git を更新する / 誤りなら戻す）。信頼が積もってから `auto` に昇格する。**`alert-only` → `auto` の移行が、そのまま組織の信頼獲得のプロセスになる**。

### §24.4 import（reverse-render）

数千リソースの理想を手で書き起こすのは非現実的である。**Observe した現実をそのまま理想状態として書き出す**。

```
sakutei import --provider <名前> --out intent/ [--scope <範囲>]
  → Observe した現実を intent（YAML）として出力
  → 「今の現実 = 理想」の状態から始める（diff がゼロになる）
```

Sakutei の import は Terraform の import より構造的に楽である。観測 state を持たないため、import は「state ファイルを作る」のではなく**単に現実を YAML に書き出すだけ**であり、state と現実がズレる Terraform import 特有の苦しみが原理的に発生しない。実装も `Lister`（§5）があれば足りる。ただし2つの注意がある。

- **import した YAML は「理想」ではなく「現状のスナップショット」**である。既存基盤には設定ミス・レガシー・意図しない差異が含まれ、そのまま理想にすると間違った現状を永久に維持することになる。**import 後に人が読み、意図を反映して整理する工程は避けられない**（ここは人力）。
- **managed fields の切り分けが import 時に必要**（§5）。DRS が動かす配置・動的 IP・runtime な値を理想に取り込むと永久に喧嘩する。→ **managed 候補の自動サジェスト**：import 出力に「N 周観測して毎周値が変わるフィールド」を managed 候補としてマークし、人の切り分けを「ゼロから」から「サジェストの承認/却下」に下げる。これにより §24.2 の昇格基準も「diff ゼロ」でなく「**意味のある diff がゼロ（managed 除外後）**」に緩められ、ノイズと本物の drift を分離できる（アラート疲労の回避）。この機構は Sync Mode の commit 間引き（§24.5）と共通化する。

**v1b の実装（安全側の既定）**：

- **出力**：`<out>/imported-<provider>.yaml` に provider ごとに 1 ファイル書く（`--provider` はカンマ区切りで複数可）。
  loader の YAML サブセットで書き、値はすべて二重引用符、出力は決定的である。既存の出力は `--force` なしでは上書き
  しない。`--scope k=v,...` で scope を絞れる。直後の `plan` は差分ゼロになる。
- **prune させない**：全リソースを `deletion_policy: ExplicitAbsentOnly` で書く（§24.1）。
- **List が `ListComplete` でなければ中止**し、何も書かずにエラーにする（不完全な列挙を「現状」として書かない）。
- **観測できないリソースは書かない**（`unknown` を「この値で在る」と書かない。書かなかったものは報告する）。
- **secret の平文は書かない**。secret フィールドは `SecretRef` の形のときだけ書く。Computed / Managed のフィールドは
  書かない（観測値を Diff が比べるのと同じ intent の値に直して書く）。
- **managed 候補**：現実を `--samples` 回（既定 3）、`--sample-interval`（既定 2s）おきに観測し、値が変わったフィールドの
  **直前にコメント**で候補の印を付ける（`# managed 候補: 観測のたびに値が変化（a → b）`）。人はこれを見て Schema で
  managed にするか、その行を消す。
- 現実には書き込まない。

### §24.5 Sync Mode（reality → intent 一方向反映）

§24.4 の `import` は離散・手動の操作である。だが実際の移行では、Sakutei 管理へ切り替えるまでの数ヶ月間、対象 scope は引き続き正常な人手運用の対象であり続ける。この期間の設定変更は緊急対応でも逸脱でもなく、その基盤にとっての**正常な運用そのもの**であり、`import` を都度手動実行する運用は現実的でない。**現実の変化を継続的に intent（git）へ記録し続ける仕組み**が Sync Mode である。

scope は排他な2つの mode を持つ。

```go
type ScopeMode int
const (
    ConvergeMode ScopeMode = iota // 既定・従来通り。intent（git）→現実へ収束
    SyncMode                       // 現実→intent（git）へ片方向反映。Apply には一切接続しない
)
```

1つの scope は同時に両方の mode になれない。ロード時に検証し、矛盾する宣言は理想状態ロード時に拒否する（§5 の循環検出・prune 検証と同じ入口・同じ思想）。これは `DeletionPolicy`（§12）と同種の scope に紐づくポリシー enum であり、`Provider` / `Executor` のような実装差し替え可能な interface（seam）とは性質が異なる。

```yaml
scopes:                   # intent のトップレベル（v1b の書式）
  - scope:
      site: legacy-dc
      cluster: legacy-cluster-a
    mode: sync            # 省略時は converge
    sync:
      target_path: generated/legacy-dc/cluster-a/   # ★ generated/ 配下（下記参照）
```

**ロード時の検証（v1b）**：（1） 空の scope は拒否する（全世界を一つの mode にする事故で、収束がすべて止まる）。
（2） 同じ scope の二重宣言は拒否する（1 つの scope は 1 つの mode）。（3） `mode: sync` は `sync.target_path` を必須とし、
`generated/` の下の相対パスに限る（`converge` に `target_path` は書けない）。（4） **sync の scope に入る intent の
リソースは拒否する**（sync の scope は収束しないので、そこに置いたリソースは書き戻しループの種になる）。

**動作**（通常の収束サイクルとは別ループ）：

```
毎周：
  1. List（§5）で scope 内の現実を取得
  2. 前回 sync 時のスナップショットと比較
  3. 差分があれば intent ファイルへ書き出し + commit（コミット者はシステム）
  4. render → Diff → Apply には一切渡さない（意図的に非接続）
```

`Lister` があれば実装でき、内部ロジックが `import` と共通する——本機能は import を「都度手動」から「scope に紐づく継続ループ」に変えるものであり、新しい Observe/List の実装は要らない。

**なぜ双方向にしないか**：「intent → 現実への収束」と「現実 → intent への反映」を同一 scope で同時に有効化すると、人が書いた intent が次周の反映で即座に上書きされ、原因タグ A（§10）が現実のわずかな揺れと区別できなくなる。**片方向のみとし Apply 経路には一切接続しない**ことで構造的に回避する。これは「シークレット世代を git に書き戻さない」（§20）と同じ思想——**書き戻しループを作らない**。

**generated/ 規律を適用する**（§21）：片方向にしても、人が sync 対象パスを「普通の intent」だと誤解して直接編集すると次周の反映でサイレントに上書きされる、という穴が残る。RenderedSource の生成物に定めた規律——`generated/` 配下に置き、冒頭に自動生成ヘッダを付し、直接コミットを拒否する——を Sync Mode の出力にも同じく適用する。commit の attribution はシステムとして明確にし（§26.3）、人の intent commit と混同されないようにする。commit 粒度は衛生の問題であり正しさには影響しない——意味のない差分まで毎周 commit すると監査ログの可読性が下がるため、commit は間引くか意味のあるフィールドのみ normalize して比較する。

**v1b の実装（`generated/` への書き出しまで）**：

- `sakutei sync --intent <dir> --provider <名前,...>` が、`scopes:` の sync の scope ごとに現実を List + Read し（観測は
  `import` と共通）、`<target_path>/sync-<provider>.yaml`（target_path は intent ディレクトリの親＝リポジトリの根から
  の相対）に書き出す。`--interval` を付けると繰り返し、付けなければ 1 回で終わる。現実には書かない。
- 出力の先頭に自動生成ヘッダ（`# sakutei:generated sync`）と**本文の sha256** を書く。本文がヘッダのハッシュと
  合わない（人が編集した）ファイル、ヘッダの無いファイルは**上書きを拒否する**（黙って人の編集を捨てない）。
- **変化があったときだけ書く**（上の動作の 2〜3）。**List が不完全なら書かずに前のスナップショットを残す**（縮んだ・
  欠けた現実を記録しない）。ある scope と provider の組の失敗は、ほかの組を止めない。
- **git へのコミット（上の動作の 3 の commit・コミット者・間引き・直接コミットの拒否）は後回し**とした（Backlog #35）。
- **prune との関係**：sync の scope の現実は、`--claim` の外と同じく**集合差の前に落とす**（`plan`・`reconcile`・
  常駐ループ）。`mode: sync` の宣言を外すと、その scope の現実は通常の prune の規律（§12）に戻る。

Sync Mode は `import` / `prune` / `closed-world` と同じく `Capabilities.List` に束ねられる（§5）。List できないプロバイダの scope は Sync Mode の対象にできない。

**切替（cutover）：Sync Mode → Converge Mode**。数ヶ月の同期期間を経て準備が整った時点で、離散的な1ステップとして切り替える。このタイミングのみ人のレビューが要る——理由は正当性判断ではなく、**§24.4 で指摘した managed fields の切り分け**（DRS が動かす配置・動的 IP 等の runtime 値を intent に取り込まない）が生スナップショットのままではできないためである。

```
1. Sync Mode を停止（最新 commit を凍結スナップショットとする）
2. 人がレビュー：managed fields の切り分け・不要な履歴ノイズの整理（§24.4 と同一の作業）
3. scope の mode を converge へ切替、remediation: alert-only で開始
4. §24.2 の通常の段階的昇格プロセス（N日間 diff ゼロ → auto 化）へ合流
```

### §24.6 観測不能資産の階級（observability:none / ledger_only）

ブラウンフィールドには **Observe すら不可能な資産**（API を持たない旧機器、ドキュメントにしか存在しない配線、ベンダーロックで読めない箱）が必ずある。これらを扱う型が無いと `unknown` の運用負荷が積み上がるだけになる。この資産階級（`observability: none` / `ledger_only` / `last_verified`）の型定義・prune 巻き添えの遮断・時間ベースの人手検証は **§5.8 を正とする**——ブラウンフィールドはこの型が最も必要になる現場であり、本節はその適用文脈である。

- **`last_verified` による鮮度述語**：`last_verified` が古くなったら「人が目視確認せよ」を health 述語（§25.2 の時間ベース health 述語と同型）として上げる。observe できない資産の唯一の「鮮度」は人の確認である。
- これは v1a で型として持つ（後から足すと台帳の意味論が変わる）。実際の運用フローは v1b（brownfield）。

### §24.7 導入順序と escape hatch

導入はリスクとリターンで順序づける。

1. **安全で価値が出る周辺から**：DNS レコード / 監視設定 / 証明書。壊れても影響が局所的で、drift 検知の価値が即座に出る。
2. **read-heavy な可視化**：全 VM / 全 FW ルールを Observe して実態レポートを出す（Apply しない）。既存基盤の実態を暴くこと自体が信頼獲得になる。
3. **新規構築部分から `auto`**：既存を触るより、これから作るクラスタ / テナントを最初から Sakutei で作る。greenfield 部分で実績を作り、brownfield は後回しにする。
4. **既存の中核は最後**（ハイパーバイザー / コア NW）。**コア既存インフラは永久に alert-only のままでよい**。

**escape hatch（freeze）は初日から用意する。** 導入初期こそ Sakutei を止める手段が要る。グローバル freeze（§14）を1コマンドで叩けるようにし、「おかしい → まず止める → 調べる」を即座にできるようにする。Sakutei を止めても本番は動き続ける（制御面が落ちてもデータ面は動く・§23）ことを運用チームが理解していることが重要である。**「いつでも止められる」と分かっていることが、新しいツールの受容を助ける。**

---

## §25 災害と復旧

被災時には**収束エンジン自身が死んでいる**（プライマリに居たなら）ことがあり、フェイルオーバーを指示する主体が不在になりうる。DR 自動化は制御面のマルチサイトが前提であり、自動化は最後発でよい。本節は、DR を人間トリガで安全に発動するための経路を定める。

### §25.1 DR モード

マルチサイト DR では、被災サイト（例：東京）が持っていた**通常の書込経路（git の正・PR 承認・署名鍵）そのものが被災で到達不能**になりうる。すると生存サイト（例：大阪）が引き継ごうとしても、PR を承認する経路・commit に署名する鍵が東京依存のままだと、**平時のガバナンスがそのまま DR の障壁になる**——収束の仕組みは生きているのに正への書込みができない。これを2つの経路で断つ。

**（a）DR intent のプリステージ**（平時にレビュー済みの構成を保持）。大阪が東京 scope を引き継ぐための構成（claim 変更を含む）を、**平時にレビュー・承認済みの状態でリポジトリに保持**する。発動時は新規レビューを要さず、**パラメータ / claim の切替のみ**で有効化する。

```yaml
# shards/dr/tokyo-to-osaka.yaml
# 平時に PR 承認済みで保持。発動は enabled: true への切替（または claim 切替）のみ
dr_plan:
  name: tokyo-to-osaka
  primary:                # 引き継がれる scope（平時の東京の claim）
    site: tokyo
  standby:
    site: osaka
  on_activation:
    claim_change:         # 大阪の DR シャードが東京 scope を claim（§17）
      from:
        shard: tokyo
      to:
        shard: osaka-dr
    enabled: false        # ★平時は false。発動 = true（この1点の切替が DR 発動）
```

DR 発動時に新規レビューが不要なのが要点である。承認は「平時に、東京が健在なうちに」済ませておく——被災後に東京の承認者に PR レビューを頼む必要がなくなる。claim 変更（所有権ハンドオフ・§17）がプリステージ済みなので、大阪デーモンは切替後の周期で東京 scope を収束開始する。プリステージ構成は generated ではなく人が書く intent であり、通常の PR フローで平時にレビューされる（§7 の platform 書込ポリシーに従う）。発動フラグの切替だけを緊急時に行う。

**（b）書込 break-glass**（git の正へ到達不能時）。git の正（中央リポジトリ）へ到達不能な状況では、被災を免れたサイトのローカルミラー（§25.2）への**署名付きコミット**を許可する break-glass 型の書込経路を第一級化する。これは Break Glass 原則（「使えるが、こっそりは使えない」・§20）の**書込版**である。

- **位置づけ**：機器へ入る（認証）の Break Glass に対し、これは正へ書く（書込）の Break Glass である。既存の Break Glass 原則をそのまま適用する——オフライン相当の専用鍵 / 使用が必ず検出される / 使用後 rotate。
- **必ず検出される**：ローカルミラーへの break-glass コミットは `BreakGlassWriteUsed`（最高 severity）を Event → sink（§26）に流し、S4 監査（§26）に append する。「こっそり書く」ができない。
- **事後合流 PR とセット**：break-glass コミットは**必ず**「中央の正が復旧したら事後合流する手順」と一体で第一級化する。ローカルミラーの署名付きコミットを、正の復旧後に **PR として起票し事後レビュー**する（緊急時は書けるが、監査は後から必ず通る）。これにより DR 中に緊急で書いた変更が正の履歴に正規に取り込まれ、分岐したまま放置されない。
- **ローテーション**：break-glass 書込鍵は使用後 rotate する（使い捨て原則）。

**貫く原則**：DR 経路も「事前チェック最小・事後監査最大」——被災時は速度を優先して生存サイトが書けるようにし、抑止は確実な発覚（Event + S4）+ 事後合流の強制で担保する。平時のガバナンス（PR 承認・署名）を DR で捨てるのではなく、**（a）で平時に前倒し、（b）で緊急書込を検出可能な break-glass に閉じ込める**の二段構えで、「被災サイトへの承認・署名依存」を構造的に断つ。

DR intent のプリステージ（a）は claim ハンドオフ（§17・v1b）と DR Workflow（§6・v1.5）の組み合わせで表現でき、新プリミティブ不要である。書込 break-glass（b）は Break Glass（§20）と Event/S4（§26）の再利用であり、書込 break-glass の型と事後合流 PR 起票は v1.5（最小 Workflow と同じ段）とする。

**v1.5 の実装**（Backlog #45）：

- **DR intent の置き場所と効き方**：DR plan は `shards/dr/<name>.yaml`（`name` はファイル名と一致）に置く。`shards/` の下なので全シャードの sparse checkout（§17.4）に必ず入る。claim は 1 シャード 1 scope なので、引き継ぐ側は standby 拠点で**平時 idle の DR シャード**（`shards/osaka-dr.yaml` が `claim:` 空）として待機させ、`claim_change` はシャード名（`{shard: <name>}`）で書く。`enabled: true` の間、`from` のシャードは idle になり（平時の claim が `primary` と一致していることを要する）、`to` のシャードが `primary` を claim する。以後は claim 変更 PR のハンドオフ（§17.7）と同じ経路で、`to` が fencing の世代を上げて引き継ぎ、旧プライマリが戻ってきても plan が有効な間は idle に留まる（split-brain にならない）。発動中の plan があるシャードは `DRPlanActive` Event を出す。
- **宣言の矛盾は収束させない**：`to` が平時に idle でない、`from` が `primary` を claim していない、1 つのシャードを 2 つ以上の発動中 plan が動かす、`enabled` が明記されていない、未知のキー——のいずれかがあれば、その周を飛ばす（曖昧な claim で収束しない。空の desired で収束しないのと同じ扱い・§17.4）。`enabled` をシステムが書き換えることはない（§25.4 人間トリガ）。
- **DR Workflow との組み合わせ**：引き継いだ scope の intent（`intent/site=tokyo/`）に置いた Workflow は、claim が移った DR シャードが S2（§25.3）とともに進める。DR plan 自体は新プリミティブを持たない。
- **書込 break-glass**（`sakutei breakglass commit`）：運用者はローカルミラーの clone に緊急の変更を書き、break-glass 鍵（ed25519。`sakutei breakglass keygen` で平時に作りオフライン保管）で署名して 1 コミットにする。署名はコミットの tree・parent・メッセージ（署名行を除く）を覆い、trailer（`Sakutei-Break-Glass` = session・`-Actor`・`-Key` = 鍵の指紋・`-Signature`）に置く（git の gpg/ssh 署名に依存しない。確認は `sakutei breakglass verify`）。手順は **到達不能の判定（§29.1）→ 失効鍵の拒否 → 署名 → S4 に `breakglass.write` を append（書けなければ書かない）→ ミラーに session の印 → ミラーへ push** の順で、使用は `BreakGlassWriteUsed`（severity=critical）Event になる。actor・理由・S4 の指定が無ければ書けない。拠点の常駐ループも、新しく届いた break-glass コミットごとに `BreakGlassWriteUsed` を出す。
- **ミラーの保持と事後合流**：session を開いたミラー（`sakutei mirror`）は中央を追わず、break-glass の先端を配り続ける（中央の古い先端で緊急書込を巻き戻さない）。中央に届くようになると、break-glass コミットをそのまま（rebase・squash せず署名を保ったまま）合流用ブランチ `breakglass/<session>` として中央へ push し、PR の下書き（Markdown。コミット・実行者・鍵・両側で変わったパス・取り込み方）をミラーの横に書き、S4 に `breakglass.merge_back.opened` を残す。中央の先端が break-glass の先端を祖先に含んだら（merge commit / fast-forward で取り込まれたら）session を閉じて中央の追従に戻り、S4 に `breakglass.merged` と、使った鍵ごとの `breakglass.key.retired` を残す。失効した鍵での break-glass 書込は拒否される（使い捨て原則：鍵は rotate する）。
- **既知の限界**：PR の起票はブランチの push と下書きの生成までで、GitHub/GitLab の API で PR を開くのは運用者（またはその上の自動化）が行う。session の間は中央で進んだ変更もそのミラーの拠点には届かない（合流で取り込まれてから届く）。squash merge は祖先関係が残らないので session が閉じない（squash しない規約で担保する）。

### §25.2 git 外部化とローカルミラー（読み取り全断耐性）

git は外部（GitHub/GitLab）に置き、各段0 ノードに**ローカルミラー**を持つ（§23.3）。外部 git 不達時は最後に pull した状態で収束を継続する（**稼働必須依存にしない**）。これにより中央リポジトリへの読み取りが全断しても、reconciler は最後の理想へ向けて収束を続けられる。§25.1（b）の書込 break-glass も、このローカルミラーを書込先とすることで成立する。

バックアップ / DR の各側面は既存プリミティブに割り当たる——バックアップ構成は Resource（収束）、バックアップ結果（取れたか・鮮度・復元可能性）は health / canary（収束外・§26）、リストアは Task（一回性）、DR フェイルオーバーは invariant 付き Workflow（§6）である。最大の落とし穴は「構成が converged でも毎晩失敗して1ヶ月バックアップが無い」であり、**構成の収束は結果の健全性を保証しない**。鮮度は時間ベースの health 述語、復元可能性は隔離環境への定期リストア canary、3-2-1 等のポリシーは invariant で守る。

### §25.3 S2 喪失は事故（quorum ストア + fencing）

観測 state は毎周再導出するため喪失しても再構築できるが、Task/Workflow の進行（S2・§4）は**喪失が事故になる** state である。承認ゲート待ち・rolling 進行位置・invariant 状態などが失われると、二重実行や中途半端な適用が起こりうる。したがって **S2 は quorum ストアに永続化し、fencing token（§17）で二重 claim を検知**する。DR で claim をハンドオフする際も、この fencing により旧プライマリの遅延した書込みを弾き、split-brain を防ぐ。S2 の永続化先を持つことは、状態分類（§4）における S2 の定義そのものの帰結である。

### §25.4 サイト分断時の挙動と DR 判定

サイト分断（生存サイトから被災サイトの現実が観測できない状態）では、**観測できない資産を `unknown` として扱う**。ここで §12 の非対称原則をそのまま適用する——**確信が持てないとき、加算はしても減算はしない**。すなわち：

- unknown な資産を「存在しない」とみなして prune してはならない（`unknown ≠ absent`）。
- 分断中は**加算のみ**（生存サイトで必要な新規リソースの作成）を許し、分断先の資産への破壊的操作は halt する。

**DR 判定は人間トリガとする。** 観測 state レス設計では「現在状態のキャッシュ」を持たないため、「被災か・一時的な観測断か」をシステムが自動判定する材料を原理的に持たない。分断を「災害」と解釈して自動フェイルオーバーすると、一時的なネットワーク断で split-brain を誘発する。したがって DR の発動（§25.1 の `enabled: true` 切替 / claim ハンドオフ）は**人間が明示的にトリガ**する。システムは分断を Event として上げるところまでを担い、災害と断じて経路を切り替える判断は人に残す。

---

## §26 検証と監査

本システムは**インフラを操作対象にする**ため、「操作される側」を用意しないと検証にならない。しかも検証したいのは正常系より**危険な収束挙動**（prune / 爆発半径 / drift 自己修復 / converging 誤認）であり、わざと壊れる状況を安全に再現する必要がある。実機は論外である。

### §26.1 検証の4段

検証は忠実度と速度の異なる4段を積む。速い順に、下段ほど頻繁に、上段ほど最終確認として回す。

1. **ユニット + フェイク Provider（主戦場・秒）**。収束ロジック・DAG・prune・爆発半径・原因タグ・in-flight 排他の検証に本物は要らない。むしろフェイクの方が良い——**障害注入が自由**（unknown を返す / N 回に1回失敗 / converging 継続）、高速（秒）、決定的（CI 可）。`Provider` / `Executor` interface に挿すだけで収束エンジン本体を実インフラなしで完全にテストでき、これは seam 設計（§5）の副産物である。**開発の8割はここで済む。** 毎コミット CI で回す。
2. **単一 Provider 統合（分）**。provider 実装そのものの正しさを、軽量な本物（DNS=CoreDNS / コンテナ=Podman / FW=nftables / Linux 系）に対して検証する。
3. **devenv フルスタック E2E（時間）**。VM 払い出し → DNS 登録 → FW 設定 のように、段をまたぐ収束を通しで確認する。重い本物のミニチュア（Proxmox ネスト / containerlab / MinIO / targetcli / Vault）を含む。
4. **chaos（意図的破壊・最重要）**。provider をわざと落とす / drift を注入する / 大量削除をトリガする → **爆発半径制御・alert-only 縮退・oscillation（振動）検知・`unknown ≠ absent` が効くか**を検証する。本システムの肝は「壊れそうで止まる」ことなので、**壊す検証が最重要**である。カオスの常設先は twin（§26.2）とする。

### §26.2 devenv（坪庭）と twin（縮景）

**devenv（坪庭）** は開発・検証環境の総称（上記4段の層1〜3 を含む）。メモリの大きい物理1台に、ネストで段0〜段2 を丸ごと再現する（実機ゼロ）。町家の内側に囲い込まれた一坪の庭のように、敷地（本番）の内側に隔離された、本物と同じ材料でできた小さな庭である。**devenv 自体を Sakutei で構築する**（dogfooding・§23.8）。

**twin（縮景）** は devenv の最終像であり、実在の景観を縮小再現する作庭技法にちなむ。**本番の intent を同じ reconciler でツインに流し込み、本番とツインを同一の宣言から構築する**——検証環境が本番と構造的に一致する。定義は「**render の外部入力を差し替えた環境**」である（render を第一級化したことで「入力の差し替え点」として自然に表現できる・§9）。

- **ツイン専用の AllocationAuthority**（本番スナップショットから seed すれば具体 ID の忠実度も確保）。
- **専用/フェイクの SecretBackend**（本番 Vault を叩かない）。
- inventory もツイン用スナップショット。
- → **本番プールを消費せず、本番シークレットも展開しない**。

用途は、変更の事前検証（爆発半径・カスケード波及 D の可視化）/ provider 更新の検証（幽霊 diff C を本番前に観測）/ DR・ローリングのリハーサル / カオスの常設先である。

### §26.3 監査（S4）

Event を永続化（S4・§4）し、**append-only + 連鎖ハッシュ**で改竄耐性を持たせる。

- **git が理想変更履歴の半分を担う**（コミット連鎖がそのまま改竄検知）。Apply 実行記録は追記専用ストアへ書く。
- **外部アンカーが必須**：連鎖ハッシュ単体では先頭（head）を外部固定しないと全体書き換えで改竄可能である。head hash を Event として sink に流し、記録する。
- **アンカー間隔を短縮し即時 push する**：日次アンカーは「最後のアンカーから今日まで」の最大 24h 分を検知されずに改竄できる窓を残す。→ **監査を外部 SIEM / WORM / append-only 専用サービスへ即時（毎周 or 毎時）push** し、head アンカー間隔を短縮する。append-only の enforcement を「アプリの規律」でなく**ストレージ層（WORM / 外部即時 push）**で担保する（侵害されたホストが自分の犯行ログを書き換えられないように）。監査書き込みは reconciler と別権限・別プロセスとする。
- **S4 の永続化先**：S4 は **WORM ストレージ or 外部即時 push を正式な永続化先とする**。選択肢は排他でなく、どちらか一方（または併用）で「侵害ホストが自分の犯行ログを改竄できない」を満たせば足りる——WORM（write-once ストレージ）なら書込み後の改竄が物理的に不可能、外部即時 push なら侵害ホストの外に記録が出た後の改竄が不可能である。
- **帰属**：収束は自律動作なので「誰が」が問題になるが、**変更の原因タグ（A/B/C/D/E・§10）がそのまま監査の帰属になる**（自己修復か / 誰の commit 由来か / どの承認ゲートを誰が通したか）。原因区別の設計が監査で再利用される。
- **秘密の監査は SecretBackend 側の機能を使う**（自前実装しない）：Vault KV v2 は更新のたびに version history を自動保持し、audit log は全 read/write を誰が行ったかを記録する。→ git（理想が誰にどう変更されたか）+ SecretBackend（秘密が何回更新され誰に読まれたか）+ Event（S4・この2つを繋ぐ収束側の記録）の三者で監査が完結する。

### §26.4 適合レポート（status）

**適合（conformance）**——宣言どおり在るか——は収束ループの中で常時判定されている。その結果を人向け・監査向けに切り出すのが適合レポートである。

```
sakutei status       # drifted 一覧（適合レポート）
```

`status` は現時点で理想と現実がずれている（drifted）リソースの一覧を返す。これは health（在るものが動いているか・監視システムに委譲）や合成検証（canary・その操作が今も動くか・§26.5）とは別の「チェック」であり、混同しない——status が扱うのは「宣言どおり在るか」だけである。drifted 一覧はブラウンフィールドの昇格判定（§24.2 の「N 日間 diff ゼロ」）の根拠にもなり、監査向けの適合エビデンスとしても使える。

### §26.5 合成検証（canary）

3つの「チェック」を混同しない——**conformance**（宣言どおり在るか・収束ループが判定・§26.4）、**health**（在るものが動いているか・監視システムに委譲・§22.2）、そして**合成検証（canary）**（その操作が**今も動くか**）の三つである。canary は前二者とは別の**能動的検証**であり、受動監視では絶対に得られない情報を取る。特に狙うのは**削除経路の健全性**——「作成は動くが削除が権限不足で失敗」は現場で頻発し、conformance も health も「差分ゼロ」なので捕まえられない。

「VM を作成・削除できるか」を実際にやってみて確かめる。設計形は**時間トリガで起動する Workflow**であり、隔離 scope + TTL 自動 GC + cleanup 反復失敗で自動停止を必須とする。

```yaml
synthetic_check:
  name: vm-lifecycle-canary
  every: 1h
  scope: { datastore: test-only, network: canary-vlan }   # ★隔離必須
  steps:
    - create: { from: canary-template, ttl: 10m }          # TTL = 消し忘れ防止
    - assert: { powered_on: true, reachable: true }
    - delete: {}
    - assert: { gone: true }
  on_fail: { alert: page }
```

- **隔離必須**：canary は本番プールに触れず、専用の datastore / VLAN 等の隔離 scope で回す（爆発半径を canary 自身に閉じる）。
- **TTL による自動 GC**：作成物には TTL を付し、消し忘れ（canary 自身が孤児を残す事故）を防ぐ。
- **cleanup 反復失敗で自動停止**：cleanup（delete）が反復して失敗する canary は level に昇格して通知し、自動停止する（自らが drift 源にならないため）。canary 失敗そのものは edge（事件）として1回通知する（§22.3 の単一 Event）。
- **§26.1 の chaos との対比**：chaos は「わざと壊して安全機構が効くか」を twin（§26.2）で検証する破壊試験、canary は「正規の操作経路が今も生きているか」を隔離 scope で常時確かめる健全性検証であり、目的が異なる。実装は Workflow（§6）に載るため v1.5 以降。

### §26.6 CLI の全 verb

バイナリは `sakutei` 単一とし、サブコマンドで機能を分岐する。動詞は業界標準を用いる（既存知識が転用でき、学習コストが生じない）。

```
sakutei                          # 常駐して回り続ける（本来の姿）
sakutei plan                     # 何が変わるか（= Diff で止める。dry-run）
sakutei reconcile                # 一回だけ収束させる（手動キック）
sakutei reconcile --expect-revision <rev>   # 期待する rendered_revision を指定し、ズレていれば中断
sakutei status                   # drifted 一覧（適合レポート・§26.4）
sakutei import --provider <名前> --out intent/  # 既存の現実を intent として書き出す（§24.4）
sakutei --claim <scope-selector> # 自分が収束を担当する scope を宣言して常駐（静的シャーディング・§17）
sakutei sync --intent <dir> --provider <名前>   # mode: sync の scope の現実を generated/ に記録（§24.5）
sakutei allocator --store <file> # 採番権威を HTTP サービスとして提供（active/standby・§19.8）
sakutei fence --store <file>     # scope ごとの fencing 世代と holder を表示（read-only・§15.2/§17.6）
sakutei mirror --upstream <url> --dir <dir>     # 拠点が pull する読み取り専用 git ミラー（§17.4）
sakutei secrets extract --from <file> --claim <selector> --out <file>  # 1 拠点分の資格を抜き出す（§20.5）
```

- **`plan` と `reconcile` はそれぞれ独立に Observe する**（§5）。plan の観測を reconcile に持ち越すと観測 state を再導入してしまうため、両者は別々に現実を見る。
- **`--expect-revision`** は、plan で確認した内容と reconcile が適用する内容の同一性を担保する（間に理想が更新されていれば中断する）。承認ゲートが「plan を1バイト単位で固定する」のではなく「意図を承認し実行時の rendered_revision で同一性を確かめる」思想（§14）と対応する。
- **`--claim`** は全 intent を pull した上で自分の claim 分だけを収束するための起動フラグ（スケールアウトの段2・§17）。シャード割当そのものを git の intent として宣言し、二重 claim は fencing token（§17）で検知する。型は v1a で切り、実動作（claim フィルタ・fencing・git 配布）は v1b で実装した（§17）。
- **`apply` を主動詞に置かない**のは、このシステムが「一回叩くツール」ではなく「回り続けるもの」であることを名前で表明するためである（§8）。
- **常駐せず `reconcile` だけで運用する形態も正当**である（段0-zero・§23.7）。「本来の姿」は常駐のまま、起床トリガが人間になるだけで、毎回リアルを再観測する level-triggered な真実は変わらない（`apply` でなく `reconcile` という動詞がこの一回性運用でも意味を保つ）。

**v1b 時点のフラグ（実装に合わせた具体）**。scope-selector は CLI では `site=tokyo,cluster=prod`（`ラベル=値` を `,` で並べる）と書く。`version` / `help` は自明なので省く。

| verb | 主なフラグ（v1b で足したもの） | 挙動の要点 |
|---|---|---|
| 常駐（`sakutei`） | `--alloc-store <file>` / `--alloc-url <url,url>`（排他）・`--claim <selector>`・`--fencing-store <file>`・`--shard-id <名前>`（既定 `<hostname>-<pid>`）・`--capacity-gate <宣言>`（繰り返し可）・`--inventory-file <file>`・`--inventory-min-ratio`（既定 0.5）・`--inventory-expected-min`（既定 0 = 無効）・`--intent-repo <url>`・`--workdir <dir>`・`--shard <名前>`・`--git-branch`（既定 `main`）・`--git-poll`（既定 60s + 最大 50% の jitter）・`--s2-free`・`--secret-store <file>` | 採番は Allocate（Lookup キャッシュ付き・§19.6）。`--claim` 外の scope は Observe しない（§17.1）。`--intent-repo` は `--intent` と排他で、`--workdir` と、`--shard` か `--claim` のどちらか一方を要する（§17.4）。`--s2-free` は `--intent-repo` を必須にし `--alloc-store` を拒否する（§17.5） |
| `plan` | `--alloc-store` / `--alloc-url` | 採番は Lookup のみ（払い出さない）。未割当は「割り当て待ち」と表示 |
| `reconcile` | `--alloc-store` / `--alloc-url`・`--capacity-gate`・`--audit-log <file>`（v1.5） | 採番は Allocate。`--claim` は v1a から持つ。`--audit-log` を付けると planned_change のレーンを開く（S4 が実行をまたいで `max_count` と cooling-off を持つ・§13.3） |
| `status` | `--alloc-store` / `--alloc-url` | 採番は Lookup のみ。read-only |
| `import` | `--provider <名前,...>`（必須）・`--out <dir>`（必須）・`--scope k=v,...`・`--samples N`（既定 3）・`--sample-interval`（既定 2s）・`--force` | §24.4 |
| `sync` | `--intent <dir>`（必須）・`--provider <名前,...>`（必須）・`--interval <d>`（既定は 1 回で終了） | §24.5 |
| `allocator` | `--store <file>`（必須）・`--listen`（既定 `127.0.0.1:7070`）・`--standby` | `--standby` なしは起動時に昇格（fencing 世代を上げる）。standby は `POST /v1/promote` まで 503（§19.8） |
| `fence` | `--store <file>`（常駐の `--fencing-store` と同じファイル） | read-only |
| `mirror` | `--upstream <url>`・`--dir <dir>`（共に必須）・`--interval`（既定 30s）・`--once` | 状態を持たない中継（§17.4） |
| `secrets extract` | `--from <file>`・`--claim <selector>`・`--out <file>`（すべて必須） | 出力はモード 0600（§20.5） |

- `plan` / `status` は**採番を払い出さない**（Lookup のみ）。dry-run と適合レポートが副作用を持たないという §26.4 の規律を採番にも適用したものである。
- **`--expect-revision` は v1b 時点で未実装**である（`reconcile` にフラグが無い）。設計上の位置づけ（上記）は変えない。

**承認ゲート API**。段階適用（rolling）の手動承認ゲートは HTTP API で通過させる。

```
POST /workflows/{id}/gates/{gate}/approve   # ボディに承認者 ID + 署名
```

ステージ完了 → `StageAwaitingApproval` Event 発火 → sink 経由で人へ通知 → Workflow は「待ち」フェーズで停止（S2 の永続化により留まる）→ 承認 API で通過（却下 = halt）→ 次ステージ、という流れである（タイムアウトで halt）。承認 API 自体は認可を持たない（誰が叩けるかは上位 IdP・§7）が、**承認された事実は監査（S4）に記録**する。N-of-M・役割制約・申請者≠承認者（separation）などの承認者要件はゲート定義の `requirements` に載せ（§14）、全承認者を S4 に append することで「どの承認ゲートを誰が通したか」が原因タグの帰属（§26.3）と一体で追える。

---


