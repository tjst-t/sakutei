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

**管理 NW の要件（CommitConfirm 不在なら型で Apply 禁止）**：commit-confirm は Apply 契約の transactional / candidate-commit であり、v2 の NW 機器 provider で入る（v1a/v1b には無い）。commit-confirm 未実装のまま管理NWコアを Apply すると、config push で自分を締め出す経路が残る。→ **v1 では管理NWコアを Apply 対象にしない**。NW 機器 provider は `Capabilities.CommitConfirm` が false の間は「**Apply 不可・観測のみ**」を Capabilities で機械的に強制し、ロックアウト経路を型で塞ぐ。強制の口は `Capabilities.ObserveOnly`（§5.3）：コアはその provider の差分を知らせるだけで、どの経路でも Apply に渡さない（最初の NW 機器 provider `netconf` がこれ。§29.1、Backlog #141）。到達性の最小核はそもそも段0（Ansible）で Sakutei は触らない。

**NETCONF の 2 段の Apply（commit confirmed、Backlog #175）**：`netconf` は provider の設定で明示したとき（`netconf_apply: "true"`）だけ `CommitConfirm` を宣言して `ObserveOnly` を外し、commit confirmed の 2 段で当てる。明示しなければ観測だけのまま。予算・承認ゲート・remediation mode は他の Apply と同じに通る。

1. Apply 用の新しいセッションを開き、機器の hello に `:candidate` と `:confirmed-commit`（1.0 か 1.1）があることを確かめる。**どちらかが無い機器には何も送らない**（その機器は観測だけのまま。PermanentError）。
2. candidate を lock し、running の部分木を読み、宣言を**部分木の置き換え**として candidate に `<edit-config>` する（filter が選ぶ部分木の外には触れない）。`:validate` があれば `<validate>`。
3. **同じセッションで** `<commit><confirmed/><confirm-timeout>…</confirm-timeout></commit>`。
4. **新しい ssh 接続で**、届くこと・running の部分木が宣言どおりなことを確かめる。確かめは **confirm-timeout の半分で打ち切る**。
5. 確かめられたら**同じセッションで**確定の `<commit>`。

主は **`:confirmed-commit:1.0` だけの機器**の流れ（確定は commit confirmed を送った同じセッションからしかできず、そのセッションが切れると取り消される。だから同じセッションを開いたまま新しい接続で確かめ、同じセッションで確定する）。1.1 の機器でもこの流れで動く。`:confirmed-commit:1.1` を持つ機器のために、設定で選べば persist の流れ（commit confirmed に `<persist>`、確定は `<persist-id>`。Apply のセッションが切れても新しいセッションから確定する）も持つ。

- **確かめられなければ確定しない**：candidate を discard し、Apply のセッションを開いたまま confirm-timeout が過ぎるのを待って、**機器の自動の巻き戻しに任せる**（閉じる要求が届く保証は無いので、巻き戻しを機器の時計に頼る形にそろえる）。過ぎたら新しい接続で元に戻ったかを見て知らせる。PermanentError（同じ宣言は変わるまで当て直さない。届かなくなる設定を繰り返さない）。
- **確定の成否が分からない**（commit confirmed・確定の commit の応答の前に切れた等）ときは unknown として次の周の観測に任せる（TransientError）。**途中で落ちたら機器の巻き戻しに任せる**（1.0 の機器はセッションの終わりで、そうでなくても confirm-timeout で戻す）。
- lock の取り合いは TransientError、機器が受けない宣言（edit-config・validate の rpc-error）は candidate を discard して PermanentError。

**NETCONF の commit_group（複数の台の変更をまとめて確定、Backlog #199）**：同じ変更を複数の台に入れる構成（例：同じセグメントを全部の leaf に足す）で、確かめが一部の台で落ちて変更が片方の台にだけ残ることを避ける。netconf のリソースの欄 `commit_group: <名前>`（intent にも、外の realizer が返すリソースの Fields にも書ける）に同じ名前を書いたリソースは、全部の台で確かめが通ったときだけ確定する。**書かなければ今どおり台ごとに確定する**（暗黙にはまとめない）。

- **コアの受け持ち**（provider の口 `provider.CommitGrouper`）：読み込みの時点で group を割り当て、**group の中の依存の辺（直接でも間接でも）・scope をまたぐ group・`strategy: task` の member を断る**。netconf は 1 台に 1 つの member まで（1 台の candidate は 1 つのセッションしか持てない）。member は**同じ波に並ぶ**（各 member が全 member の依存先を待つ）。差分のある member の op は**まとめて 1 回の Apply**（op の `CommitGroup`）。**1 つでも行けない member**（観測できない・止めている・上流が落ちた・backoff・in-flight・予算で止まった・承認待ち）があれば、**どの member も行かない**（承認ゲートは group 単位で、一部だけの承認はさせない）。予算は今どおり台ごとの変更として数える。早期収束パスと #177 の観測し直しの回は group の member を当てず、group の全部を見る周に任せる。
- **流れ**：(1) group の全部の台で、それぞれの Apply のセッションを開き candidate に部分木を当てる（1 台でも送れなければ、どの台にも commit confirmed を送らない）。(2) 全部の台に、それぞれのセッションで commit confirmed。(3) 全部の台で、新しい ssh 接続での到達と宣言どおりかを確かめる（confirm の先の確かめ、#200 も入る）。(4) 全部が通ったときだけ、各台に確定の commit。
- **時間の上限は group の中で一番短い confirm-timeout を基準にする**（台ごとの confirm-timeout は `netconf_confirm_timeouts`）。最初の commit confirmed から、その半分までに (4) に入れなければ確定しない。
- **1 台でも送れない・確かめに落ちる・時間の上限の半分に届く、のどれかなら、どの台も確定しない**：candidate を discard し、`:confirmed-commit:1.1` を持つ台には `<cancel-commit>` も送ってすぐ戻し、ほかは機器の confirm-timeout の巻き戻しに任せる（その時間までセッションを開いたまま待ち、新しい接続で戻ったかを見る）。
- **残る窓（全部か何もしないかにならない場合）**：(4) の確定は台ごとの rpc で、NETCONF に複数の機器をまたぐ確定は無い。**確定の rpc を送っている途中で切れた台は成否が分からない**ので unknown にする（確定していれば残り、届いていなければ confirm-timeout で戻る。次の周の観測に任せる）。機器が確定を断った台は confirm-timeout で戻る。どちらの場合も、ほかの台は確定している。provider は台ごとの結果（committed・not_committed・unknown）を返し（`provider.CommitGroupError`）、コアは**台ごとに Event `CommitGroupApplied`** を出し、確定しなかった台・分からない台をその台の Apply の失敗として扱う。plan は操作ごとに `commit_group` を出し、止まった member のある group の残りを `blocked_by: commit_group` にする。

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

**v1b の実装**：

- `sakutei sync --intent <dir> --provider <名前,...>` が、`scopes:` の sync の scope ごとに現実を List + Read し（観測は
  `import` と共通。`--samples`（既定 3）回、`--sample-interval`（既定 2s）おき）、`<target_path>/sync-<provider>.yaml`（target_path は intent ディレクトリの親＝リポジトリの根から
  の相対）に書き出す。`--interval` を付けると繰り返し、付けなければ 1 回で終わる。現実には書かない。
- 出力の先頭に自動生成ヘッダ（`# sakutei:generated sync`）と**本文の sha256** を書く。本文がヘッダのハッシュと
  合わない（人が編集した）ファイル、ヘッダの無いファイルは**上書きを拒否する**（黙って人の編集を捨てない）。
- **意味のある変化があったときだけ書く**（上の動作の 2〜3）。比べるのは managed 候補（§24.4 と同じ判定：1 周の観測の
  あいだに値が変わったフィールド）の値を除いた内容のハッシュである。一度 managed 候補になったフィールドはスナップショット
  に印（`# managed 候補` のコメントと本文先頭の機械用の一覧）を残し、以後の周もその値の変化だけでは書き直さない
  （揺れるだけのフィールドで毎周 commit しない。落ち着いた先の値も、ほかの変化で書き直すときに記録される）。**List が不完全なら書かずに前のスナップショットを残す**（縮んだ・
  欠けた現実を記録しない）。ある scope と provider の組の失敗は、ほかの組を止めない。
- **git へのコミット**：リポジトリの根が git の作業ツリーなら、1 周で書いたスナップショットをまとめて **1 つのコミット**
  にする（作業ツリーでなければ書くだけ）。コミットに入れるのはスナップショットのパスだけで、人がステージしているものは
  入れない。author・committer は**システム**（既定 `sakutei-sync <sakutei-sync@localhost>`、`--committer-name` /
  `--committer-email` で変更可、§26.3）、**署名なし**（§29.1）。メッセージの末尾に `Sakutei-Sync: <scope> <provider>`
  の trailer を付け、Sync は名前ではなくこの trailer で自分のコミットを見分ける。コミットに失敗したら書いた
  スナップショットは作業ツリーに残り、次の周がコミットする。
- **直接コミットの拒否**（§9.6・§21）：HEAD のスナップショットが、trailer の付いた Sync の最後のコミットが書いたものと
  違う（人がコミットで変えた。本文のハッシュを合わせ直していても）か、Sync が書いたことのないファイルがコミットされて
  いるなら、その組は**上書きもコミットもせずに止める**。Sync の最後のコミットの内容に戻すか、コミットで消せば、Sync が
  書き直す。
- **prune との関係**：sync の scope の現実は、`--claim` の外と同じく**集合差の前に落とす**（`plan`・`reconcile`・
  常駐ループ）。`mode: sync` の宣言を外すと、その scope の現実は通常の prune の規律（§12）に戻る。

**採番の書き戻し（v3.34、Backlog #157）**：Sync Mode のコミットの仕組みは、採番の結果を宣言の側へ書き戻すこと（§19.7）
にも使う。現実ではなく**ツールが決めた値**を宣言へ記録する点が違うが、システムとして書き、人の intent のコミットと
混ぜない点は同じである。書き戻し先のパス（既定 `generated/allocations/<pool>.yaml`。リポジトリの根からの相対）・
仕方・つなぎ方（リポジトリ・push の宛先と鍵）は宣言のリポジトリの事情で決まるので設定（`--alloc-write-back`）に、
書き戻すプールは intent（`pools:` の `write_back: true`）に書く。仕方は 3 つ：

- `commit`（既定）：ファイルに書いてから値を使い、その周の終わりに intent のブランチ（作業ツリーの今のブランチ）へ直接
  コミットする。**Sync Mode のコミットに相乗りする**——コミッター（既定 `sakutei-sync`・署名なし、§29.1）、書いた
  パスだけを入れる、中身が変わらなければコミットしない（間引き）。trailer は `Sakutei-Allocation: <pool>`。
  作業ツリーでなければ書くだけ。
- `review`：別のブランチ（既定 `sakutei/allocation-write-back`）に更新したファイルをコミットし、人のレビューを通す
  （作業ツリーには触らない。取り込みは人）。取り込まれて intent の側のファイルに値が現れるまで、その値を使う Apply を
  止める（`plan`・`status` に「書き戻しの取り込み待ち」と出る）。
- `file`：書くだけで git は使わない。

Sync Mode のスナップショットと違い、書き戻しのファイルは**正**なので本文のハッシュで人の編集を拒まない。人が直した
内容は store と照らされ、食い違えば Apply を止める（§19.7）。書き戻しは Apply に接続するが、書き戻しループにはならない：
書くのは store が払い出した値だけで、ファイルの値を store へ戻すのは人の `sakutei allocator import` だけである。

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
- **v1 の記録の形（v3.57、#187）**：S4 は `--audit-log` のファイル（JSON Lines）である。
  - **何を残すか**：承認ゲートの承認・却下・timeout（§6.4）、planned_change の宣言・開き・使用量（§14.5）、freeze（§14.3）、
    break-glass（§25.1）に加え、**ふつうの Apply と prune も Op ごとに 1 件**残す（`resource.apply`・`resource.prune`）。
    常駐の周・早期収束の経路・一回性の `reconcile`・Workflow の apply の段のどれでも、Apply は同じ Executor を通るので、そこで
    記録する（fencing で断られた Apply も失敗として残る）。1 件は resource・op（create / update / delete）・変えた欄の**名前**
    （値は書かない。秘密が混ざらないように）・cause（§10、帰属）・結果（`ok` / `failed` と理由）・planned_change のレーンで通った
    Op はその宣言と、宣言を承認した人。記録は Apply のあとに書く（結果を書くため）。書けなかったときは知らせるが、Apply の結果は
    変えない（起きた変更を失敗と偽ると同じ変更を繰り返すだけになる）。
  - **Workflow の記録（v3.72、#210）**：Workflow の run の始まり（`workflow.run.start`）・手ごと（`workflow.step`。action の
    op と args、待ちの手の満たした／時間切れとかかった時間）・終わり（`workflow.run.end`。done・halted・cancelled と理由）を、
    Workflow の名前と run の id つきで残す。Workflow の手から出た `resource.apply` には run の id と手の番号（stage・target）が
    付き、「どの Workflow のどの手で何をしたか」を監査だけから追える（形は §6.3）。同じ鎖に書くので `sakutei audit verify` で
    たどれる。
  - **鎖**：各行は通し番号 `seq`（ファイルをまたいで 1 から）と、前の行（書かれたとおりのバイト列）の SHA-256 である `prev` を
    持つ。行の書き換え・抜き取り・差し込みは次の行の `prev` か `seq` で分かる。`sakutei audit verify` がたどり、見つければ終了コード
    3（`--json` でも出す）。鎖より前の版で書いた先頭の行は legacy として受け、鎖が始まったあとの `seq` の無い行は差し込みとして
    扱う。複数のプロセス（常駐と CLI）が同じファイルへ書くので、追記は `<file>.lock` の flock で順に並べる。書いている途中で
    切れた行は切り詰めずに改行で閉じ、verify が読めない行として知らせる。
  - **書き換え・切り詰めをしない**：回すとき（`--audit-rotate-bytes`）は今のファイルをそのままにして新しいファイル
    （`<file>.000001`, `.000002` …）へ続け、鎖はファイルをまたいでつながる。回したファイルが抜ければ verify が見つける。
    だから WORM の置き場（書いたものを変えられない）に置いても成り立つ。
  - **外へすぐ送る**（外部アンカー）：`--audit-syslog tcp://host[:514]` / `tls://host[:6514]` で、記録を 1 件ずつ syslog の
    受け手へ送る（RFC 5424。TCP の上は RFC 6587 の octet-counting、TLS は RFC 5425。facility は log audit、失敗した Apply・prune
    は warning、ほかは notice。MSGID は種別、`[meta sequenceId]` に seq、MSG は書いた行そのもの＝受け手でも鎖をたどれる）。
    送り終えた所は `--audit-syslog-state`（既定 `<file>.syslog-state`。書き換える小さなファイルなので WORM の外に置く）に
    残す。**貯め場はログそのもの**で、受け手が止まっている間に書いた記録は手元のログに残り、常駐は Append で起こされるたび・
    `--audit-syslog-retry`（既定 30s）ごとに、一回性のコマンドは次の Append のときに、送り終えた所の続きから順に送り直す
    （`sakutei audit forward` でその場で送れる）。送れなくてもコマンドは止めない。届けるのは少なくとも 1 回（落ちれば同じ行を
    もう一度送る。受け手は sequenceId で見分ける）。TCP には受け取りの確認が無いので、書けたことを届いたことと見なす。
  - **末尾の切り詰め**は鎖だけでは分からない（最後の数行を消しても残りは鎖のまま）。verify は送り終えた所（seq とその行の
    ハッシュ）を外の控えとして突き合わせ、ログがそこまで無い・その行が違えば切り詰め・書き換えとする。送ったあとの記録は
    受け手の側にあるので、手元が書き換えられても分かる。
  - 監査を書くプロセスを reconciler と分けること（別権限・別プロセス）は v1 では行わない（同じプロセスが書き、外へ送ることで
    侵害後の改ざんを分かるようにする）。
- **秘密の監査は SecretBackend 側の機能を使う**（自前実装しない）：Vault KV v2 は更新のたびに version history を自動保持し、audit log は全 read/write を誰が行ったかを記録する。→ git（理想が誰にどう変更されたか）+ SecretBackend（秘密が何回更新され誰に読まれたか）+ Event（S4・この2つを繋ぐ収束側の記録）の三者で監査が完結する。

### §26.4 適合レポート（status）

**適合（conformance）**——宣言どおり在るか——は収束ループの中で常時判定されている。その結果を人向け・監査向けに切り出すのが適合レポートである。

```
sakutei status       # drifted 一覧（適合レポート）
```

`status` は現時点で理想と現実がずれている（drifted）リソースの一覧を返す。これは health（在るものが動いているか・監視システムに委譲）や合成検証（canary・その操作が今も動くか・§26.5）とは別の「チェック」であり、混同しない——status が扱うのは「宣言どおり在るか」だけである。drifted 一覧はブラウンフィールドの昇格判定（§24.2 の「N 日間 diff ゼロ」）の根拠にもなり、監査向けの適合エビデンスとしても使える。

**観測できないリソースがあっても報告は止めない**（#117）。あるリソースの Read がエラーになった、または Availability が `ok` でなかったときは、そのリソースを**観測できない（unobservable）**として数え、drifted にも適合にも数えない（unknown ≠ absent・§3、§5.4）。provider の List がエラーのときも同じく「観測できない」側に数え、宣言外の検出ができなかったことを注記する（エラーを「宣言外ゼロ」と読まない）。どちらも残りのリソースの評価は最後まで続け、エラーの文はリソースごと（List なら provider ごと）にレポートと標準エラーへ出す。BMC が1台落ちただけで status 全体が使えなくなると、障害のときの見え方そのものが測れないからである。

**宣言外（unmanaged）の検出**（#164）。status は List を持つ provider ごとに現実を列挙し、宣言に無いものを**宣言外（unmanaged）**として出す。見せるだけで消さない（削除は §12 の規律にだけ従う）ので、provider の型が prune する型（`Pruneable`、§12.3）かどうかに関わらない——VM のような破壊的型こそ、宣言から外したものが残っていることを見えなくしてはならない。List する provider は、intent に宣言のある provider に加えて、**宣言が 1 つも無くてもその回に設定されている provider**（`--provider-config` に節がある、または `--zone`・`--ruleset`・`--s3-config`・`--postgresql-config` のようにその provider だけの設定が与えられた）である。宣言を全部外すとその provider が組み立てられず、外したものがどこにも出なかったためである。`--state` は fake provider の既定で他の provider の代わりにもなるので、それだけでは設定されたと見ない。provider が触らないと決めているもの（proxmox-vm の `protected_vmids`・`protected_names`・テンプレート、nftables の base chain など）は List に出ないので宣言外にも出ない。設定されているのに組み立てられない provider は、List のエラーと同じく検出できなかったものとして出す（終了コード 3、「宣言外ゼロ」と読まない）。`--claim` があれば claim の中の現実だけを候補にする。

**宣言外の scope（v3.38、#169）**。List の ID の scope は provider の設定から付く（§5.4）ので、宣言と突き合わせる前に宣言に合わせる。
宣言済みかは `--claim` で絞る前の宣言全体で決める（ほかの shard の宣言は宣言外ではない）。宣言にあるものは宣言の ID に合わせ、
provider の scope と宣言の scope が食い違えばその provider の宣言外は「検出不可（scope の食い違い：provider は X、宣言は Y）」と
注記する（List のエラーと同じく終了コード 3、JSON の `kind: scope_mismatch`）。宣言に無いもので root の ID は、その provider の
宣言（claim で絞る前の intent 全体、合成の親は除く）の scope がちょうど 1 種類で root でなければその scope に入れる（宣言の側から
provider の範囲を決める）。宣言が 0 件か scope が 2 種類以上なら root のままで、`--claim` があれば claim の内外を決められない件数を
「検出不可（scope が決まらない N 件。provider の scope の設定か宣言で決める）」と注記する（黙って隠さない。終了コードは変えない、
JSON の `kind: scope_undecided`）。claim の外のものは今までどおり出さない。prune はこの推定をしない（§12.4（d））。常駐の `/status`・`/metrics` は今までどおり宣言外を出さない（口のための List を増やさない。下の「読み取り専用の口」）。

終了コードは次のとおり。

| 終了コード | 意味 |
| --- | --- |
| 0 | すべて観測でき、報告できた（drift があっても 0） |
| 3 | 報告はできたが、観測できないリソース（または List がエラーの provider）がある |
| 1 | 報告そのものができない（フラグの誤り、intent の読み込み・provider の解決の失敗など） |

観測できないときに 1 でなく 3 を使うのは、既存の呼び出し側が 1 を「status 自体の失敗」と見ているのを壊さないためである。

**機械で読む出力**（#118）。`sakutei status --json` は同じレポートを標準出力に JSON を 1 つとして出す。中身は版（`schema_version: 1`）・時刻・要約（適合・drift・観測できない・管理外・Workflow・合計の件数）・リソースごとの配列（provider・scope・local id・状態・drift のフィールドの一覧・原因タグ・観測エラーの文）・管理外の検出ができなかった provider とその理由で、文の出力が持つ情報をすべて含む。状態の語（`conformant` / `drift` / `unobservable` / `unmanaged` / `workflow`）は文の出力の分類と同じで、文の出力と同じレポートから作るので両者は食い違わない。終了コードは文の出力と同じ。形の詳細は [`docs/status-json.md`](../../status-json.md)。

**デーモンの担当と配布の状態**（#136）。status（文と JSON）のデーモンの項（`--daemon-status`）に、あるときだけ次を足す：claim（このデーモンの担当、§17.1。git の配布では `shards/` から決まったもの）、fencing の世代（scope ごとの世代と holder、fence されているか、§15.2）、git の取得の状態（最後に取れた revision と時刻、中央・ミラーに届いているか、§17.4）。シャード化・拠点配布の運用で「このデーモンは何を担当し、どの intent で収束しているか」が status だけで分かるようにする。表示だけで、正しさには関与しない。

**読み取り専用の口**（#136）。外の道具（可視化・ダッシュボード）がデーモンの状態を読むために、デーモンは `--read-listen <addr>` を明示したときだけ読み取り専用の HTTP を待ち受ける。Sakutei 本体は画面を持たず、汎用の口だけを出す（§2 の catalog の原則：UI は独自のデータを持たない薄いフロントエンド。履歴は外の道具が貯める、§22.1・§27.6）。

| 口 | 中身 |
| --- | --- |
| `GET /status` | 直前に観測した周の適合レポート（`status --json` と同じ形 + 周の番号と時刻） |
| `GET /plan` | 直前に観測した周の計画（`plan --json` と同じ形 + 周の番号と時刻）。その周に収束ループが実際に下した判定で、止めた操作とその仕組み・理由、保留（Workflow・採番待ち・観測できないもの）を含む |
| `GET /events` | Event の流し出し（Server-Sent Events、§22.3） |
| `GET /graph` | 依存の DAG（リソースと depends_on・ready_condition、今の ready かどうか、Apply の wave）。一回性の `sakutei graph --json` も同じ形 |

- **読み取り専用**：`GET`・`HEAD` 以外は 405。承認の書き込みの口（`--approval-listen`、§6.4）とは別の待ち受けにし、書き込みと読み取りの口を混ぜない。`/metrics`（§27.5）もそのまま別。
- **観測 state レスを保つ**：中身は収束ループが周の終わりに作った見え方を返すだけで、口のために観測し直さず、収束の判断にも使わない。観測まで進まなかった周（飛ばした・エラー）は中身を「ずれなし」に置き換えず、最後に観測した周のものを出す（周の番号で分かる、unknown ≠ absent）。tier で間引いた（§16.5）リソースは最後に観測した周の項目を表示だけ持ち越す。宣言外（unmanaged）の検出は口のための List を増やさないため一回性の status に任せる。
- **TLS と認可**（#161）：口（と `/metrics`、§27.5）は `--listen-auth-config` で TLS（サーバの証明書と鍵は `SecretRef`、最小の版は 1.2）と認可（読むだけのトークン `Authorization: Bearer`、値は `SecretRef`・定数時間で比べる／クライアント証明書、CA で確かめ任意で subject・SAN を絞る。両方を書けば両方が要る）を持てる。証明書とトークンは再起動なしで金庫から読み直す（30 秒ごと。読めなければ前のまま）。**既定は手元だけ**：loopback 以外を TLS なし・認可なしで待ち受けるのは明示の旗（`--metrics-insecure-listen`・`--read-insecure-listen`）のときだけで、旗なしに外向きの平文を指定すると起動で断る。断った要求は 401／403 で理由を外に出さず（ログには出す）、数を `/metrics` の `sakutei_listen_rejected_total{listener,reason}` に出す。承認の書き込みの口（`--approval-listen`、§6.4）は同じポート・同じ資格を共有しない（同じアドレスなら起動で断る。読み取りの資格では書き込めない）。中身は status・plan と同じく秘密の値を含まない（§20.2）。
- 形の詳細は [`docs/read-api.md`](../../read-api.md)。

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
sakutei graph --json             # 依存の DAG（ready_condition・今の ready・Apply の wave。§26.4）
sakutei import --provider <名前> --out intent/  # 既存の現実を intent として書き出す（§24.4）
sakutei --claim <scope-selector> # 自分が収束を担当する scope を宣言して常駐（静的シャーディング・§17）
sakutei sync --intent <dir> --provider <名前>   # mode: sync の scope の現実を generated/ に記録（§24.5）
sakutei allocator --store <file> # 採番権威を HTTP サービスとして提供（active/standby・§19.8）
sakutei allocator release --pool <p> --owner <o> (--store <file> | --url <a,b>)  # 人が採番を解放する（cooldown は効く・§19.3）
sakutei allocator list (--store <file> | --url <a,b>) [--pool <p>]  # 採番の一覧（使用中・予約・cooldown・read-only）
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
| `plan` | `--alloc-store` / `--alloc-url`・`--inventory-file` / `--inventory-min-ratio` / `--inventory-expected-min`（#115） | 採番は Lookup のみ（払い出さない）。未割当は「割り当て待ち」と表示。inventory は常駐と同じ合成（§9.2） |
| `reconcile` | `--alloc-store` / `--alloc-url`・`--capacity-gate`・`--audit-log <file>`（v1.5。`--audit-syslog`・`--audit-rotate-bytes` は #187、§26.3）・`--expect-revision <rev>`（#46）・`--inventory-file` / `--inventory-min-ratio` / `--inventory-expected-min`（#115） | 採番は Allocate。`--claim` は v1a から持つ。`--audit-log` を付けると planned_change のレーンを開く（S4 が実行をまたいで `max_count` と cooling-off を持つ・§13.3）。`--expect-revision` は rendered_revision がずれていれば何も Apply せず 0 以外で終わる |
| `status` | `--alloc-store` / `--alloc-url`・`--inventory-file` / `--inventory-min-ratio` / `--inventory-expected-min`（#115） | 採番は Lookup のみ。read-only |
| `import` | `--provider <名前,...>`（必須）・`--out <dir>`（必須）・`--scope k=v,...`・`--samples N`（既定 3）・`--sample-interval`（既定 2s）・`--force` | §24.4 |
| `sync` | `--intent <dir>`（必須）・`--provider <名前,...>`（必須）・`--interval <d>`（既定は 1 回で終了） | §24.5 |
| `allocator` | `--store <file>`（必須）・`--listen`（既定 `127.0.0.1:7070`）・`--standby` | `--standby` なしは起動時に昇格（fencing 世代を上げる）。standby は `POST /v1/promote` まで 503（§19.8） |
| `allocator release` | `--pool`・`--owner`（共に必須）・`--store <file>` / `--url <url,url>`（どちらか一方）・`--audit-log <file>` | `Release` を呼ぶ（#116）。値は cooldown に入る（§19.5（b））。記録の無い owner・未知の pool は終了コード 1。`--audit-log` で S4 に `allocation.released` を記録（§19.3） |
| `allocator list` | `--store <file>` / `--url <url,url>`（どちらか一方）・`--pool` | pool・owner・値・状態・残り時間の表。read-only |
| `fence` | `--store <file>`（常駐の `--fencing-store` と同じファイル） | read-only |
| `audit verify` | `--audit-log <file>`（必須）・`--audit-syslog-state <file>`・`--json` | 監査の鎖をたどる（#187、§26.3）。無傷は exit 0、書き換え・抜き取り・差し込み・回したファイルの抜け・送った所を超える切り詰めは exit 3。read-only |
| `audit forward` | `--audit-log <file>`・`--audit-syslog <url>`（共に必須）・`--audit-syslog-ca` / `-cert` / `-key` / `-server-name`・`--audit-syslog-state` | 送り残しをその場で syslog へ送る（#187） |
| `mirror` | `--upstream <url>`・`--dir <dir>`（共に必須）・`--interval`（既定 30s）・`--once` | 状態を持たない中継（§17.4） |
| `secrets extract` | `--from <file>`・`--claim <selector>`・`--out <file>`（すべて必須） | 出力はモード 0600（§20.5） |

- `plan` / `status` は**採番を払い出さない**（Lookup のみ）。dry-run と適合レポートが副作用を持たないという §26.4 の規律を採番にも適用したものである。
- **`plan` / `reconcile` / `status` の inventory**（#115）：常駐と同じ `--inventory-file` の合成（§9.2）を、常駐と共通の関数で行う。三つが常駐と同じ desired を見るためである（無いと inventory 由来のリソースが status で unmanaged に見える）。一回性なので前回の件数が無く、急減判定は `--inventory-expected-min` だけが効く（`--inventory-min-ratio` は受け付けるが効かない）。inventory が読めない・incomplete なら、何も Observe・Apply せずに終了コード 1 で止まる（incomplete≠empty）。
- **`--expect-revision` の実装**（#46）：`reconcile` は render フェーズ1（外部入力・採番）を終えて rendered_revision を算出した直後、generated/ の書き出しと Observe・Apply より前に、渡された値と照合する。ずれていれば `RevisionMismatch` を出し、何も Apply せずに 0 以外で終わる（plan からやり直す）。plan は採番を Lookup のみで行うため、plan 時点で割り当て待ちだった採番を reconcile が払い出すと rendered_revision がずれて中断する（払い出しは冪等なので、plan をやり直せば同じ値が見え、その rendered_revision で収束する）。

**承認ゲート API**。段階適用（rolling）の手動承認ゲートは HTTP API で通過させる。

```
POST /workflows/{id}/gates/{gate}/approve   # ボディに承認者 ID + 署名
```

ステージ完了 → `StageAwaitingApproval` Event 発火 → sink 経由で人へ通知 → Workflow は「待ち」フェーズで停止（S2 の永続化により留まる）→ 承認 API で通過（却下 = halt）→ 次ステージ、という流れである（タイムアウトで halt）。承認 API 自体は認可を持たない（誰が叩けるかは上位 IdP・§7）が、**承認された事実は監査（S4）に記録**する。N-of-M・役割制約・申請者≠承認者（separation）などの承認者要件はゲート定義の `requirements` に載せ（§14）、全承認者を S4 に append することで「どの承認ゲートを誰が通したか」が原因タグの帰属（§26.3）と一体で追える。

### §26.7 長く効く起動の引数を intent の `runtime:` に書く（v3.52）

予算・既定の mode・settle・効かない Apply の上限・観測し直しの上限・stuck の周数・周期のように、**起動のたびに同じ値を
渡し続ける引数は「どう回るべきか」の正**であり、起動の旗（systemd のユニットや手で打つコマンド）の中だけに置くと、
宣言の外に正が残る（§1.7）。そこで intent のトップレベルの `runtime:` に書けるようにし、旗は「その場の締め」にとどめる
（Backlog #183）。

```yaml
runtime:
  interval: 30s                    # --interval（常駐だけ）
  mode: auto                       # --mode（auto | alert-only）
  max_changes_per_cycle: 20        # --max-changes-per-cycle（0 = 上限なし）
  max_destructive_per_cycle: 3     # --max-destructive-per-cycle
  max_destructive_per_window: 10   # --max-destructive-per-window
  window_duration: 24h             # --window-duration
  on_exceed: halt-all              # --on-exceed（halt-destructive-only | halt-all）
  settle: 10s                      # --settle
  max_ineffective_applies: 3       # --max-ineffective-applies
  max_ineffective_duration: 30m    # --max-ineffective-duration（0 = 上限なし）
  reobserve_max: 2                 # --reobserve-max
  reobserve_time: 30s              # --reobserve-time
  stuck_after: 5                   # --stuck-after（常駐だけ）
  daemon:                          # 常駐だけに効く値（v3.63）
    mode: alert-only
  oneshot:                         # 一回性の reconcile と plan だけに効く値（v3.63）
    mode: auto
```

- **書く場所**：intent のファイルのどれか 1 つに 1 回だけ（2 つのファイルに書けば読み込みで断る）。inventory のファイルには書けない。
  知らないキー・形や範囲の外の値は読み込みで断る。`--freeze`・`--approve-large-change` はその場の操作なので `runtime:` に置かない。
- **実行の種類ごとの値**（v3.63、Backlog #193）：`runtime:` の下に `daemon:`（常駐）と `oneshot:`（一回性の `reconcile` と `plan`）を
  書ける。中身は `runtime:` と同じ欄で、**その種類に効く値**は、種類の欄にあればそれ、無ければ共通の `runtime:` の値とする
  （キーごとに決める）。常駐は alert-only・一回性は auto のように実行の種類で使い分ける運用を、旗に頼らず宣言に置くためである
  （共通を auto にして常駐だけを旗で締めると、常駐の status と Event に `RuntimeTightened` がいつも出てしまい、mode を宣言に書かない
  ことになる。§1.7）。種類に意味の無い欄（`oneshot:` の `interval`・`stuck_after`）・知らないキー・入れ子の種類は読み込みで断る。
  常駐の途中の変化（下の `RuntimeDeclarationChanged`）も、常駐に効く値どうしで比べる。
- **旗と宣言の突き合わせ**（起動のとき。比べる相手はその実行の種類に効く宣言の値。`plan` は予算の引数、一回性の `reconcile` は `interval` と `stuck_after` を除く引数を見る）：
  - 旗だけ・宣言だけなら、それを使う（宣言だけの値は旗で渡したのと同じに効く）。
  - **大小のある引数**（予算の上限の 3 つと `max_ineffective_*`）と **`mode`・`on_exceed`**：旗が宣言より**締める向き**（予算・上限が
    小さい、`mode` が `alert-only`、`on_exceed` が `halt-all`）なら旗を使い、**一時的な締め**として報告する。上限の `0` は上限なしで、
    いちばん緩い。**緩める向きなら起動を断る**。
  - **向きの無い引数**（`interval`・`settle`・`window_duration`・`reobserve_*`・`stuck_after`）：食い違えば起動を断る。settle は長くても
    短くても安全の向きが決まらず、観測し直しの上限と stuck の周数は手間と知らせの早さの釣り合いで、どれも締めの向きを持たない。
  - 断るときは、運用者がその場で直せるよう、引数ごとに **1 行**で、引数の名前・旗の値と宣言の値・直し方（旗を外すか、`runtime:` を
    書いたファイルの宣言を変えるか）を出し、終了コード 1 で終わる。種類の欄の値と比べたときは、その欄（`runtime.daemon.mode`）を指す。
  - 一回性だからといって、明示した旗が宣言と食い違っても受ける、という扱いはしない（宣言より弱い側に寄せない）。一回性で別の値を
    使いたいなら `oneshot:` に書く。
- **締めている間の報告**：level の Event `RuntimeTightened`（warning。attrs の `keys` に締めている引数、`<key>.flag` と `<key>.intent` に
  旗の値と宣言の値）と、常駐の status（`--daemon-status` の `runtime_tightened`・`sakutei status` の表示・読み取りの口の /status）に
  旗の値と宣言の値を両方載せる（種類の欄の値なら、その欄の名前 `intent_key` も）。締めは運用者が選んだものなので、続いても stuck に上げない。一回性の `reconcile` も同じ Event を出す。
- **常駐の途中で `runtime:` が変わったとき**：起動の引数は起動のときに決まり、常駐は起動したときの値で回り続ける。宣言が起動した
  ときと違う間は level の Event `RuntimeDeclarationChanged`（attrs の `<key>.started` と `<key>.intent`）と status の `runtime_changed`
  で「起こし直すと取り込む」と知らせ、宣言が戻れば resolved にする。`--intent-repo` の常駐は起動のときに 1 回取得して `runtime:` を
  読む（その取得は最初の周がそのまま使う）。取得できない・intent が読めないまま起動したときは旗だけで回り、あとで読めた `runtime:` を
  変わったものとして知らせる。

### §26.8 トレースの出口（OTLP/HTTP、v3.62）

`/metrics`（§27.5）は「いまどうなっているか」の数を出すが、**1 つの周の中で何にどれだけ時間がかかったか**（どの provider の
観測が遅いか・どの Apply が失敗したか・Workflow のどの手順で待っているか）は分からない。そこで周・観測・plan・Apply・
Workflow の段を span にし、OTLP/HTTP で OpenTelemetry の Collector へ出す（Backlog #189）。

- **既定は出さない**：`--otlp-endpoint <url>` を明示したときだけ出す（常駐・`plan`・`reconcile`）。`OTEL_EXPORTER_OTLP_*` などの
  環境変数は読まない（黙って外へ出さない。宛先は宣言した 1 つだけで、環境の proxy も通さず、転送にも従わない）。
- **形**：OTLP/HTTP の JSON の写像（`POST <url>/v1/traces`、`Content-Type: application/json`）。protobuf も OpenTelemetry の SDK も
  使わない（外の依存を入れない、§28）。
- **span**（名前は種類だけ、どれかは属性で言う）：

  | span | どこ | 主な属性 |
  |---|---|---|
  | `sakutei.cycle` | 常駐の 1 周（**周ごとに 1 つのトレースの根**） | 周の番号・observed・drifted・unknown・applied・pruned・held・止まり（予算・prune・inventory・届かない外部入力）・alert-only |
  | `sakutei.plan` | 一回性の `plan` の根、と周の中の Diff・prune の List・ゲート・予算の段 | 計画の数・Op の数・prune の Op の数・held・予算と容量の止まり・mode |
  | `sakutei.reconcile` | 一回性の `reconcile` の根 | 終了コード |
  | `sakutei.observe` | 周の中の観測の段 | 対象の数・observed・drifted・unknown・converging |
  | `sakutei.read` | 1 つのリソースの観測（Read・最小の階級の ReadFor・Workflow の観測） | provider・resource・結果（ok・absent・unknown・converging） |
  | `sakutei.bulk_observe` | provider の BulkObserve の 1 回（§5.7） | scope・返った数 |
  | `sakutei.apply` | provider の Apply の 1 回（prune の Delete を含む。Workflow の手順の Apply はその手順の下） | provider・resource（の一覧）・Op の数と種類・cause・prune か |
  | `sakutei.workflow` | 1 つの Workflow のその周の進み | Workflow・run・周の終わりの phase |
  | `sakutei.workflow.step` | Workflow の手順の 1 つ | stage・対象・手順の番号と種類・結果（done・pending・failed） |

  一回性の `plan`・`reconcile` は 1 回が 1 つのトレースで、根に終了コードを載せ、終わる前に送り終える（時間切れ 5 秒）。
  失敗は span の status を ERROR にする。provider の誤り（Read・Apply）はその誤りの文を載せる。Workflow の手順の失敗と halt は、
  Sakutei 自身が手順の欄の値を書き足した文（`apply map[...]: ...`・`wait map[...] timed out ...`）になるので、その文は載せず、
  決まった短い理由（`<種類> step failed: timed out`・`plan error`・`apply error`・`task failed`・`action error` など、
  Workflow の span は `workflow run halted`・`workflow error`）だけを載せる。理由の全文は S2 の run とログに残る。
- **値は載せない**：span には名前・ID・数・結果だけを載せ、欄の値（Diff の中身・SecretRef の値）は載せない（§20 の決まりと同じ）。
- **送れなくても収束を止めない・待たない**：span は上限つきの列（4096）に積み、1 つの goroutine が束（最大 512・1 秒ごと）に
  して送る。届かない・429・5xx は 2 度まで送り直し、それでも駄目なら捨てて数える。送れない間は 1 度だけ知らせ、戻れば 1 度だけ
  知らせる。トレースは観測の補助であって正しさに関与しない（§3 原則 8 と同じ向き：外への出口が止まっても収束は止めない）。
- **宛先の TLS と認可は読み取りの口（§22.4、#161）と同じ考え**（`--otlp-config <file>`）：
  - TLS は 1.2 以上（1.3 も選べる）。宛先の証明書は `tls.ca_file`（無ければシステムのルート）と `tls.server_name` で確かめ、
    確かめを外す設定は持たない。
  - 認可は bearer トークン（`bearer_token`）とクライアント証明書（`tls.client_cert`・`tls.client_key`）。どちらも SecretRef
    （file SecretBackend）でしか書けず、値は誤りの文にも出さない。30 秒ごとに金庫を読み直し、再起動なしで入れ替える（読めなければ
    前のまま）。URL に資格を書くと断る。
  - **loopback 以外の宛先へ、TLS（https）と認可のどちらかを欠いて送るのは `--otlp-insecure` を付けたときだけ**。付けずに指定すると
    起動で断る。
- **Collector との分担**：標本化・属性の加工・ほかの形への変換・保管は Collector と後段に委ねる（§2、持たない・委譲する）。
  Sakutei は全部の span を出し、どれを残すかを持たない。

---
