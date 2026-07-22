# 対象構成11:DBaaSプロバイダ(worked exampleのスケール版)

**総合判定:○ 条件付き成立** — Resource収束+合成リソースのモデルはそのまま800→3,000超に拡大でき、収束コアの適合は高い。ただし「フェイルオーバー(Workflow)」「PITR/binlog」「800個分の資格情報世代管理」「日20件のPRフローとgit sourceの相性」が、いずれも**設計上はv2以降に置かれた機能(Workflow/DBSource/allocator/credential-broker)に依存**する。それら拡張を実装し、かつ後述のギャップを埋めることが成立の条件。**v1スコープのままでは不成立**。

---

## 1. 構成の前提整理

- 提供物:マネージドMySQL、800インスタンス(単体500、主従ペア300)。展開後の実Resource数は概算 **3,000〜3,700**(後述)。
- 基盤:Proxmox 25ノード×4クラスタ。worked exampleの`mysql_database`(§7.5合成リソース)を素直に拡大した想定。
- 商品:small/medium/large、バックアップ(日次+PITR binlog)、8.0系マイナーVerUp、フェイルオーバー、スケールアップ(要再起動)。
- 供給経路:社内ポータル→git PR自動生成→承認→マージ、日20件。
- 運用3名。
- worked exampleとの前提差:①ペア構成(replica_set)②状態遷移を伴うフェイルオーバー③PITRという連続的営み④桁が1つ上がった資格情報/採番⑤日20件の変更速度。①③④が新規モデリングを要求する。

## 2. リソースモデリング(worked exampleからの差分)

worked exampleの`mysql_database → proxmox_vm + mysql_install(Task) + observability + backup_policy`は**そのまま単体500に流用可能**。差分は以下。

### 主従ペア:`mysql_replica_set`合成リソース

`mysql_database`をもう一段束ねる合成リソースとして設計する。render フェーズ2(§7.1/§7.5)が下位を扇状DAGに展開:

```
mysql_replica_set(ユーザが書く面)
  └ render フェーズ2 で展開
      ├ mysql_database (role: primary)   ← 既存合成リソースを再利用
      ├ mysql_database (role: replica)   ← 同上
      ├ replication_link(Resource)       ← CHANGE MASTER 相当。存在・接続性を収束
      └ observability(レプリ遅延 lag を health 述語で監視)
```

- **レプリケーション"構成"はResource**(§20.1「role/権限/接続の存在」は収束向き)。`replication_link`はプライマリ座標・レプリケーションユーザの**存在と疎通**を収束対象にする。
- **レプリ"状態"(遅延・SQLスレッド稼働)はhealth**(§18/§17.1「在る≠動いている」)。`Seconds_Behind_Master`は差分ゼロなので収束では捕まらず、監視委譲。
- **どちらが今primaryかは`managed: true`+`computed`**(§11.2-4)。フェイルオーバーで入れ替わる値を desired に書くと永久に喧嘩する。これがモデリング上の最重要判断。desired側には「role: primary を張るべき論理ノード」ではなく「ペアである」ことだけを書く設計にしないと、フェイルオーバー後に収束が旧primaryへ role を戻す誤動作を起こす。

### フェイルオーバー:Workflow(§9.2)。MHA的自動判断は外部委譲

**「いつ切るか(検知・判断)」と「どう切るか(手順)」を分離する**のが設計の芯(§2-2, §9)。

- **判断(いつ)は外部に委ねる**:プライマリ死活の検知と自動フェイルオーバー発火はSakuteiの管轄外。理由は§18のDRと同型 — 検知主体が収束エンジンだと、被災時に自分も巻き込まれうるうえ、level-triggeredの収束は「状態遷移の判断」に向かない(健全性はhealthへ委譲済み)。MHA/Orchestrator/ProxySQL等の専用フェイルオーバー機構、またはAlertmanager発火の人間判断が担う。
- **手順(どう)はWorkflow**:切替そのものは`invariant`付きWorkflowとして構造化する。§9.2のrolling-upgradeと同型:

```yaml
workflow: mysql-failover
target: replica_set/orders-db
invariant: { writable_primary: 1 }        # 常にwritableが1つ(split-brain防止)
steps:
  - stop_writes_on: "{old_primary}"        # 到達可能なら
  - wait: { condition: replica_caught_up, timeout: 30s }
  - promote: "{replica}"
  - repoint_apps: "{new_primary}"          # VIP/ProxySQL切替
  - reconfigure_replication: { from: "{new_primary}" }
on_failure: { policy: halt }               # 自動ロールバック困難→人へ
```

- 実行中の`replica_set`は`converging`扱いにして収束の介入を止める(§9)。フェイルオーバー完了後、`managed`なcurrent-primaryが観測で更新され、収束は新primaryを事実として受け入れる。
- **自動切替をSakuteiでやるか**:判断は**外部委譲が正**。Workflowの手動/イベント起動から始め、自動発火はAlertmanager→Workflowトリガのフックとして最後発で足す(§18のDR自動化と同じ「最後発」原則)。

### PITR/binlog:連続的な営みは health/canary へ寄せる

PITRは「状態」ではなく「連続する結果の健全性」なので、**収束Resourceにはできない**。§18の4分割を適用:

| PITRの側面 | プリミティブ |
|---|---|
| binlog有効化・保持設定・バックアップジョブ定義 | Resource(収束) |
| binlogが実際に流れ続けているか・欠損なし・鮮度 | health述語(`binlog_freshness: max_age`) |
| 任意時点への復元が本当に効くか | canary(隔離環境へPITR復元を定期実行・§18「戻せるかを証明」) |
| 実際の復元操作 | Task(一回性) |

binlogそのものの連続転送はSakuteiの収束ループの管轄外(行データと同じく観測・宣言に馴染まない)。「binlogが途切れていないこと」を時間ベースhealth述語で監視するのが設計上の正しい寄せ方。

### 必要Provider一覧

| Provider/機構 | 種別 | 階級(§5.4) | 備考 |
|---|---|---|---|
| `proxmox_vm` | 基盤Resource | 完全(TrueObserve+List) | worked exampleそのまま |
| `mysql_install` | 多相Task | — | worked exampleそのまま |
| `replication_link` | Resource | 標準(自前実装)〜完全 | primary座標・repl userの存在/疎通を収束。ペア構成の新規部品 |
| `mysql_grant`/`mysql_user` | Resource | 標準 | role/権限の収束(§20.1) |
| `backup_policy` + binlog設定 | Resource | 標準 | 日次+binlog有効化 |
| `observability` | Resource | — | lag/binlog鮮度/health(§17.2) |
| `secret`(root/repl資格) | Resource | — | 存在・鮮度の収束(§14.4)。値はVault |
| `mysql-failover` | Workflow | — | v2機能 |
| `mysql-restore`/`pitr-restore` | Task | — | v2機能 |
| allocator(vmid/vlan) | 衛星 | — | 800×2規模の採番。v2機能 |

## 3. ownership・DesiredSource・sharding

- **ownership**:各DBは`ownership: tenant`(§12.1)。数百チーム=数百tenant。共有基盤(Proxmoxクラスタ・NWコア・共有ストレージ)は`ownership: platform`でPR必須。DBはtenant所有なので即時反映・auto可、共有基盤はhalt寄り、という原則6の分岐がそのまま効く。
- **DesiredSource**:テナントDBはDBSource(§13.1、即時・API・append-onlyログ)が本来の相性。**ただし本構成は「ポータル→git PR→承認→マージ」を採る**ため、実質的に全リソースがFileSource(git)経路になる。これは設計と衝突しない — §13.4で「テナントのカタログ操作は即時、管理者の基盤変更はPR」と非対称にしているが、**本構成は"tenant層にも人間承認を挟む"組織方針**であり、これはtenant層をFileSource+PRで運用する選択として許容される(catalogのUI→PR生成経路・§13.4がそのまま使える)。**allocatorの仮予約 reserve→confirm(§16.3)がこのフローの前提** — PR生成時にvmid/vlanをreserveし、PR diffに具体IDを出し、マージ後renderでconfirmする。却下時のリークをここで防ぐ。
- **sharding/収束周期試算**(§12.2):単一デーモン天井≒Observe時間。3,500リソース×200ms/並列度16 ≈ **44秒/周**。§6.2の早期警戒線(数千リソース・Observe比率)にちょうど掛かる規模。単一デーモンでも数十秒周期なら回るが、**「4クラスタ」という自然なシャード境界がある**ため、tenant/cluster軸で4シャードに分ければ各シャード〜900リソース×200ms/16 ≈ 11秒/周に収まる。§12.2の推奨(数万でシャーディング、それ以下は間引き/watch併用)に照らすと、**3,500は「単一+Observe間引き」で十分足りる領域**。安定したDB(converged継続)をN周に1回のObserveへ落とせば、単一デーモンで余裕を持って回る。共有基盤は専用platformシャード1つが所有し、tenantシャードはread-only参照(§12.2)。
- PR自動生成フローとgit sourceの相性は良好。日20件=git commit 20件/日は、監査(§17.5 git=改竄耐性アンカー)・原因タグA(§10.3 intent_revision変化)・レビューゲートがすべてこの経路に自然に乗る。

## 4. render(inventory/allocation/secret)

- **inventory**:Proxmoxクラスタ容量・ノード一覧を外部入力。不達時は前回値保持(§7.3)。
- **allocation**:vmid×最大1,100個+tenant VLAN。allocator(§16、v2)の対象。800×2規模でも「1つくれ」方式なので収束コア自体はstateを持たない。**確認済みの重要点**:Release非対称原則(§16.4)がDB削除で効く — 「理想から消えた」ではなく「proxmox_vmの削除完了をObserveで確認してから」vmid/vlanをReleaseし、cooldownを置く。数百規模の削除で誤Releaseは致命的なので、この規律が800規模で初めて本気で効く。
- **secret世代管理(本構成の山場)**:800DB×(rootパスワード+レプリケーション資格情報)≈**1,600本の資格情報**。§14.2の設計はこれをそのまま扱える:
  - 値はVault、理想状態には`${secret:vault:/...}`参照のみ(ピン留めしない)。
  - **世代はrenderフェーズ1がVault metadataから解決**して下流入力ハッシュに合成(§14.2)。rotate every 90d を1,600本に掛けても、git書き戻しループは発生しない。
  - **懸念**:renderフェーズ1が毎周1,600本のVault metadata読みを行う(§7.4)。TTLキャッシュ前提だが、1,600本×毎周は無視できない負荷。**世代解決の負荷がObserve時間と並ぶ第二の天井になりうる**(§7.4のTTLキャッシュとシャード分割で緩和)。
  - rotation実行自体はWorkflow(§14.4「新値生成→DBに設定→レプリカ側repl user更新→旧無効化」)。**レプリ構成でのrotationは順序が増える**(primary/replica両方+repl user)ため、単体より手順が重い。

## 5. 安全機構の適用

- **「DBを消す」多層防御**:①`pruneable: false`既定(§8.3、明示opt-inなしでは消えない)②`final_backup: required`(worked example 2.5、削除前最終バックアップ強制)③削除はWorkflow化(final backup→停止確認→VM削除→Release)④rate limitのdestructive重み付け(§10.2、delete 10件でhalt)。**4層が揃っており、DBaaSの最大の恐怖("1コミットで全DB削除")に構造的に対処できる**。§19.5「1コミットで全VM削除が原理的に可能」がまさにこの構成の生命線。
- **日20件とrate limitの整合**:日20件=1周(数十秒)あたりの変更はほぼ常に1〜2件。`max_changes_per_cycle`のabsolute:10程度は日常運用を一切妨げない。むしろ「静かなインフラで突然50件削除」の異常検知(§10.2ベースライン逸脱)が本領を発揮する規模。ただし**マイナーVerUp(8.0系)を800台に一斉適用する類の"正当な大規模変更"**は、明示`approved-large-change`(§10.2)+段階適用(canary→broad、§10.5)で通す設計にしないとhaltに引っかかる。

## 6. 段0ブートストラップの当てはめ

- 段0(Proxmox 3ノード+統合Ceph+VM-HA、§19.2)はDBaaS基盤(25ノード×4クラスタ)とは別建て。段0にVault/credential-broker/reconciler。
- **DBaaSのVaultは1,600本の資格情報の根**になるため、Vault sealed-on-restart のMTTR(§19.4、人間のunseal時間を含む)が重い。Vault断中は§14/§15.5のキャッシュ+alert-only縮退で「稼働中DBの収束は継続、新規/rotate直後のApplyだけ止まる」。1,600本のSecretRefキャッシュ(値+世代metadata)を段0で持つ前提。
- git外部(GitHub/GitLab)+ローカルミラー(§19.4)。日20件のPRマージがこのgit経路に乗る。

## 7. 成り立たない点・苦しい点・設計ギャップ・必要な拡張(忖度なし)

1. **v1では動かない(最大の事実)**:本構成が要求するフェイルオーバー(Workflow)、PITR復元(Task)、allocator、DBSource/UI、health(observability as Resource)、canary、シャーディング、credential-broker は**すべて§24.2でv2以降に分類されている**。v1スコープ(Resource収束+FileSource+import)だけでは、単体500の"作って放置"は回るが、**商品メニューの過半(フェイルオーバー・スケールアップ・PITR検証)が実装できない**。「Sakuteiで管理が成り立つか」は「v2機能を実装するか」と等価。

2. **フェイルオーバーの自動判断は明確にスコープ外**:設計は「いつ=外部委譲/どう=Workflow」で正しく分離しているが、DBaaSの商品価値の中核である**自動フェイルオーバーの判断ロジックそのものはSakuteiが提供しない**。MHA/Orchestrator/ProxySQL等を別途運用し、それらとWorkflowを繋ぐ接着剤(発火フック・VIP切替の実行実体)が必要。ここは設計書が「作らない」と言っている領域で、運用3名がこの外部機構も面倒を見ることになる。split-brain防止の`invariant: writable_primary:1`をWorkflowが保証しても、**判断の誤り(誤検知フェイルオーバー)はSakuteiの外**で起きる。

3. **role入れ替えとmanaged fieldの緊張**:フェイルオーバー後、旧primaryが復帰するとreplica化する。current-primaryを`managed`にすれば収束は喧嘩しないが、**「ペアのうち少なくとも片方はwritable」という論理不変条件をdesiredにどう書くか**が非自明。desiredが「role固定」だと戻し合いが起き、「role非固定」だと収束が何を保証するのか曖昧になる。§5.6の抽象化にも似た、合成リソースのアイデンティティ設計の難所で、worked exampleにはない新規論点。**設計拡張が要る**(replica_setのschemaで「論理primaryを固定しない」ことを一級で表現する仕組み)。

4. **PITR/binlogは収束の枠外に大きくはみ出す**:binlogの連続転送・欠損検知・任意時点復元は、設計の「連続的な営み」の想定(rotation/backup鮮度)を超える運用の塊。health述語とcanaryで「健全性の証明」はできるが、**PITRエンジン本体(binlog管理・point-in-time適用)はMySQL/外部ツール側**であり、Sakuteiは存在・鮮度・復元可能性の"監視設定を収束"するだけ。DBaaSの売り物としてのPITRの実装主体はSakuteiではない、という期待値管理が要る。

5. **secret世代解決の毎周負荷**:1,600本のVault metadata読み×毎周(§7.4)は、Observe時間と並ぶ第二のスケール天井になりうる。TTLキャッシュとシャード分割で緩和可能だが、**rendered_revisionの算出定義(§26.2 未決事項B)が「TTLキャッシュのヒット/ミスで値が変わらない」を担保できているか**が実装前に確定必須。1,600本規模で原因タグE-secretが誤爆すると、日常的に幽霊diffが立つ。

6. **tenant層にPR承認を挟む選択の代償**:数百チーム×日20件のセルフサービスをgit PRで捌くと、**マージがボトルネック**になりうる。設計はtenant層を即時(DBSource)にできると言っているが、本構成は承認を挟む。運用3名で日20件のPRレビューは回るが、規模が伸びると承認遅延がセルフサービスの体験を損なう。DBSource(即時)へ倒すか、承認を自動化(quota内なら自動マージ)する上位プラットフォーム側の作り込みが要る — これはSakuteiの外(§12.1)。

7. **Ansible provider階級の落とし穴**:MySQL層をAnsibleモジュールでラップすると`TrueObserve:false`に縮退し(§21.3)、原因タグA/B区別・converging検知・破壊的重み付けが落ちる。DBaaSの中核(mysql/replication)は**自前provider(完全階級)で書く必要**があり、Ansible頼みだと安全機構が効かない。実装コストが「800台を安全に回す」ための必須投資になる。

## 8. 総合判定

**○ 条件付き成立**

- **成立する根拠**:収束コア・合成リソース・ownership 2層・削除多層防御・secret参照モデル・シャード境界(4クラスタ)は、800→3,000超スケールに設計通り拡大でき、DBaaSの"インフラとしての面"(プロビジョニング・設定・role/権限・バックアップ構成・drift自己修復)には極めてよく適合する。3,500リソースは単一デーモン+Observe間引き、または4シャードで余裕を持って回る規模で、スケール上の破綻はない。日20件はrate limitと完全に整合し、"DBを消す"多層防御が最大の恐怖に構造的に応える。

- **成立の条件**:(a) v2機能群(Workflow/Task/allocator/health/canary/DBSource)の実装、(b) `mysql_replica_set`合成リソースと「論理primary非固定」schemaの新規設計、(c) フェイルオーバー判断とPITRエンジンを外部機構に委ね、Sakuteiは手順(Workflow)・監視設定(health/canary)・存在収束に限定するという期待値の確立、(d) MySQL中核を自前provider(完全階級)で実装、(e) 1,600本secret世代解決の負荷とrendered_revision定義の事前確定。

- **不成立となる場合**:v1スコープのまま、あるいはフェイルオーバー/PITR/自動フェイルオーバー判断までをSakuteiに実装させようとすると **✗ 不成立**(設計が明確に「収束の外/外部委譲」としている領域を侵すため)。判断と行データ・binlogの連続性はSakuteiの管轄外、という線引きを守る限りにおいて成立する。
