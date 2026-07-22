# 対象構成12: GPU/AI学習基盤 — Sakutei適用可能性レビュー

**総合判定: ○ 条件付き成立**(ノードの版数統一・ファーム更新・故障ドレインという「この現場の主戦場」は多相Task/Workflow/content-address参照に素直に写像でき、Sakuteiの level-triggered 収束と安全機構(rate limit + 段階ロールアウト)がこの構成のためにあるかのようにハマる。ただし **Slurmという「消してはいけない状態」との共存**、**InfiniBand/UFMファブリックのProvider化**、**四半期ファーム更新のような超長時間・全ノード規模Workflowの実運用性**の3点で拡張・割り切りを要する)

---

## 1. 構成の前提整理

- 規模: GPUノード128台 + CPUノード16台 = 物理144台。VMは無い(ベアメタル・仮想化なし)。**§12.2 の「〜数千 = 単一デーモン・毎周全Observe」帯**に完全に収まり、シャーディングは不要。リソース総数はノード×(OS/ドライバ/CUDA/複数ファーム面)で数千に届きうるが、天井(§12.2)には遠い。
- ネットワーク2系統(§19.3 の管理系/データ系分離がそのまま当てはまる): **管理系** = 400G Ethernet(管理・reconciler到達性)、**データ系** = InfiniBand NDR(コンピュート) + 400G Eth(ストレージ)。IBはUFMという**それ自体が収束ループを持つ上位コントローラ**である点が本シナリオの争点(後述7)。
- ストレージ: 分散NVMe FS(Weka相当) + S3互換オブジェクト。これらは「収束器の上の収束器(§11.2-1)」であり内部収束を持つ。
- ジョブスケジューラ: **Slurm。ジョブ配置はSlurmの領分でSakuteiは触らない**(§11.1 の「VM内DBの中身 = 収束外」と同じ切り分け。ジョブは宣言不能・観測state=Slurm側が権威)。SakuteiがSlurmに対して持つ関与は**drain/resumeという操作の呼び出しのみ**。
- 運用の主戦場: OS/ドライバ/CUDA/ファーム(GPU VBIOS・BMC・NIC・IBスイッチ)の**版数統一**。四半期ごとのファーム更新は再起動必須・ジョブdrain必要の大仕事。GPU故障が週1-2件。
- 組織: 運用4名。少人数ゆえ「128台を人手で版数追跡」は破綻しており、**版数統一の宣言化そのものがSakuteiの主価値**になる。

この構成はgreenfield寄り(HPCは版管理が最初から厳格)だが、稼働中クラスタへ後付けするなら §23 の段階戦略(alert-only → import → scope昇格)が入口になる。

## 2. リソースモデリング

### Resource/Task/Workflow 写像

- **Resource(収束・版数統一の本体)**: 各ノードの `target os_image` / `driver_version` / `cuda_version` / `firmware{gpu_vbios, bmc, nic, ib_switch}` を**あるべき版**として宣言。UFMのファブリック設定、Weka/S3の構成、監視設定(observability as Resource §17.2)、BMC電源/ブート設定(§20.4)。
- **Task(一回性・多相)**: **ファームウェア更新(多相Task・§9.1)** が中核。golden OSイメージビルド(完了印=レジストリObserve §20.2)。
- **Workflow(順序+invariant)**: **四半期ファーム更新の段階ロールアウト**(全ノードまたぎ・§10.5)、**故障ノードのdrain→修理→復帰サイクル**、DR。

### 版数統一を Resource としてどう宣言するか(content-address 参照)

§20.3 の**コンテンツアドレス参照が本シナリオの決め手**。版を名前(`cuda-12.4`)で参照すると「同じ名前だから変わっていない」と誤認し古いイメージ/ドライバが残る。ハッシュ参照にすれば drift 検知が正しくなる:

```yaml
resource: compute_node
id: gpu-leaf03-p12          # ★物理トポロジ座標を identity に(§5.5)
ownership: platform
tier: foundation            # 全ジョブの土台
target:
  os_image:      "img@sha256:ab12…"     # content-address(§20.3)
  driver:        "nvidia@sha256:cd34…"
  cuda:          "cuda@sha256:ef56…"
  firmware:
    gpu_vbios:   "96.00.xx"              # BMC/VBIOS はベンダ版数文字列(ハッシュ化不能)
    bmc:         "…"
    nic:         "…"
update_policy: { strategy: task, window: "quarterly-maint", rollback_on_fail: true }
```

**identity は物理トポロジ座標(`gpu-leaf03-p12`)**にする(§5.5)。故障交換で筐体が変わってもシリアルが変わるだけで intent 書き換え不要。シリアル・GPU UUID は `managed: true` + `computed` で観測のみ。**版数統一 = 全ノードのこのResource群が同一の target を指す**という宣言に落ちる。128台に同一 target を配るのは §7.5 のテンプレート展開(inventory + テンプレート)で一元管理し、`img@sha256` を1箇所直せば全ノードに drift として現れる。

### ファームウェア更新 = 多相Task(状態機械)の具体設計

§9.1 のステートマシンに Slurm連携と再起動跨ぎを織り込む:

```
Idle
 → Draining     : Slurm drain 発行 → ノードが idle+drain 状態になるまで Observe 待ち(実行中ジョブの完走待ち。timeout長め)
 → Staging      : イメージ/ファーム転送
 → Verifying    : hash/署名照合(content-address 一致・§20.3)
 → Applying     : ファーム書込(GPU VBIOS/BMC/NIC/IB)
 → Rebooting    : ノード消滅 → unknown≠absent で「再起動中」と解釈・timeout付き再出現待ち(§9.1)
 → Confirming   : 稼働版を現実Observe(Redfish/nvidia-smi/ib firmware)で target と一致確認
 → Resuming     : Slurm resume(undrain)発行 → node が再びジョブ受入可能に
 → Done                    ↘ Failed → Rollback(rollback_on_fail)/ keep_drained(復帰させず人へ)
```

- **進行フェーズのみ外部永続化(S2 §6.5)**。Done判定は稼働版を現実Observeで問える=観測stateレスと両立。
- **べき等な再開**: 各相入口で「もう済んでいるか」を現実Observeで確認(BMC死・reconciler再起動を跨ぐ)。
- **Slurmとの連携は drain/resume 2操作のみ**。ジョブがdrainで完走するまでの待ちは `ready_condition`(§6.7)= `node_idle` として下流(Applying)を gate する。

### 故障ノード対応の Workflow

GPU故障(週1-2件)は「drain → 修理(人手・RMA)→ 復帰」のサイクル。全自動化せず **Sakuteiは drain と復帰時の版verifyを担う**:

```yaml
workflow: node-repair-cycle
target: "{failed_node}"
steps:
  - slurm_drain: "{node}"                  # ジョブを新規受入停止・実行中は完走待ち
  - wait: { condition: node_idle }
  - mark: { state: maintenance }           # BMC/inventory 上で保守中に(managed field)
  # ── ここで人が物理修理・RMA(Sakutei管理外)──
  - on_return: verify_versions: "{node}"   # 復帰ノードの版が target に一致するか(交換部品は旧版のことがある)
  - firmware-update: "{node}"              # 不一致なら多相Taskで版合わせ(交換GPUのVBIOS等)
  - health-check: "{node}"                 # GPU/IB/NCCL 疎通(canary的・§17.3)
  - slurm_resume: "{node}"
on_failure: { policy: halt, keep_drained: true }
```

交換した部品は旧ファームで届くことが多く、**復帰時の版verify → 自動版合わせ**が版数統一の実効を担保する(人が忘れても drift として検知)。

### 必要 Provider 一覧

| Provider名 | 対象 | Capabilities | クラス | identity設計 | 実装難度 |
|---|---|---|---|---|---|
| `redfish_bmc` | 144台BMC(電源/ブート/BMCファーム) | TrueObserve / List / CommitConfirm:false | 標準〜完全 | 物理トポロジ座標(§5.5)。シリアルはcomputed | 中(Redfishは標準化されているが機種差あり) |
| `node_firmware` | GPU VBIOS/NIC/IB HCA ファーム | TrueObserve(版読取)/ CommitConfirm:false | 標準 | node座標 + component種別 | **高**(ベンダツール依存・更新=多相Task) |
| `node_image` | OS/ドライバ/CUDA(content-address) | TrueObserve / List | 完全 | node座標。版はimg@sha256 | 中〜高 |
| `ufm_fabric` | IBスイッチファーム・partition/routing設定 | TrueObserve / Converging(UFM内部収束)/ List | 完全 | switch/port座標 | **高**(二重ループ §11.2-1・API癖) |
| `slurm_node`(drain/resumeのみ) | Slurmノード状態 | TrueObserve(node state)/ **Apply=drain/resumeに限定** | 標準(部分) | nodename | 中(**ジョブは触らない** = Schemaで管理面を限定) |
| `weka_config` / `s3_config` | 分散FS/オブジェクト構成 | TrueObserve / Converging / List | 完全 | pool/cluster名 | 中〜高(二重ループ) |
| `node_monitor` | GPU/IB監視設定 | TrueObserve / List | 完全 | node+metric | 低(observability as Resource) |

**`slurm_node` Provider の設計上の要点**: Slurmは**ジョブという消してはいけないstate**の権威。SakuteiのSchemaはノードの `state`(idle/drain/down)だけを managed とし、**ジョブ・パーティション定義・配置は一切 Schema に含めない**(含めると prune がジョブを消す危険)。Apply は実質 `scontrol update nodename=X state=drain/resume` の2操作に限定。これは §11.1 の「VM内DBの中身 = 収束外」と同型の切り分け。

## 3. ownership・DesiredSource・sharding設計

- **ほぼ全てが `ownership: platform`**。本構成にはテナント砂場が無く(Slurmアカウント=ジョブ層はSakutei外)、ノード・ファブリック・ストレージは全て共有基盤。したがって書き込み経路は **PR必須・FileSource(git)**、remediation は **alert-only寄り**、rate limit は **厳格(halt)**(§12.1 の platform 行がそのまま適用)。128台の版を一括で直す性質上、これは必然。
- **DesiredSource = FileSource(git)**。DBSource/tenant層は不要。inventory(144台の座標・NIC・IB HCA割当)は RenderedSource(§13.1)で外部inventoryから合成しても、当面ファイルでも足りる。
- **sharding不要**(§12.2)。144台・数千リソースは単一デーモンの天井の遥か下。将来マルチクラスタ化したら site シャードを足せる seam は既にある。
- **allocator**: IB partition key / 管理IP の払い出しに使う余地はあるが、HPCは静的割当が多く v1 では自作最小 or ファイル固定でよい。

## 4. render(inventory/allocation/secret)の使い方

- **inventory合成(render フェーズ1)**が本シナリオで効く: 144台の物理座標・IB HCA・NICを外部inventoryから引き、テンプレート(§7.5)で `compute_node` Resource 群を展開。**版target(`img@sha256`)は1変数で全ノードに配る** → 版数統一が「1箇所直せば全体に drift」として現れる構造になる。
- **allocation**: IB partition key・管理IPを allocator から。§16.3 の reserve→confirm はPR運用に乗る。
- **secret**: BMC/UFM/Slurm/Weka API資格。v1 は §14.5 の暫定で **SecretRef(Vault保存の静的資格)を provider に注入**。BMCは層B(静的+定期rotate・§15.2)相当。broker導入時に CredentialProvider へ移行(設計変更不要)。
- **render障害時**: inventory/Vault不達でも前回値保持(§7.3)。空desired生成による全ノード再イメージ事故を構造的に防ぐ。

## 5. 安全機構の適用(「128台同時リブート」事故の防止)

これが本シナリオでSakuteiが最も光る点。**level-triggered は「1行間違えると1サイクルで128台を同時ドレイン→リブート」を原理的に起こしうる(§10.1)**が、そのための手綱が全て揃っている:

- **rate limit + 破壊的重み付け(§10.2)**: ファーム更新・リブートは破壊的側。`max_changes_per_cycle` と in-flight を含む同時変更数評価(§10.2)により、128台への一斉Task起動を halt できる。**in-flight込み評価が効くのが本質**——さもないと「毎周数台ずつ発射し続けて結局全台がリブート中」になる。
- **段階ロールアウト(§10.5)がファーム更新の正式形**:
  ```yaml
  rollout:
    stages:
      - { name: canary, target: {nodes: 2}, parallelism: 1,
          gate: { health_check: required, manual_approval: required } }   # NCCLベンチ合格まで進めない
      - { name: rack,   target: {rack: leaf03}, parallelism: 4, gate: { manual_approval: required } }
      - { name: broad,  target: {nodes: rest}, parallelism: 8, gate: { manual_approval: required } }
    on_stage_fail: halt
  ```
  canaryノード(2台)でファーム更新+NCCL/GPUベンチが通ってから広域へ。**四半期更新の「大仕事」が、承認ゲート付き段階ロールアウトとして構造化**される。運用4名でも「canary承認 → 一晩ラック単位 → 翌メンテ窓で残り」を安全に刻める。
- **Slurmという「消してはいけない状態」との共存**: 二段で守る。(1) 各ノードのファーム更新は多相Task内で `slurm_drain → node_idle待ち → …→ slurm_resume` を必ず通し、**実行中ジョブを完走させてからしかリブートしない**。(2) `slurm_node` Provider の Schema がジョブを一切扱わないため、prune・rate limit・drift修復のどの経路もジョブに触れない。§9 の「実行中対象は converging 扱いで収束の介入を止める」も併用。
- **freeze(§10.6)**: 四半期更新以外の期間は版target を凍結(全体 alert-only マクロ)にでき、意図しないファーム drift 修復の暴発を止められる。

## 6. 段0ブートストラップの当てはめ

- 段0(§19.2)= reconciler/Vault/broker を載せる小プラットフォーム。これは**GPUクラスタとは別**の管理系ノード(CPUサーバ16台のうち管理用、または専用Proxmox 3ノード)に置く。GPUノード144台は全て**段1以降(自律収束対象)**。
- **管理NW到達性の最小核 = 段0(§19.3)**: 400G Ethernet管理系のコア到達性は段0。UFM/BMCへの到達性が切れると全ノード全盲になるため、UFMホスト・管理コアスイッチの到達性最小核はAnsible構築・Sakutei不触。
- **データ系(IB NDR)は全て段1(§19.3)**: IBファブリックが全断してもreconcilerは管理系Eth経由でBMC/UFMを制御でき自分で直せる=single point of blindnessでない。IBスイッチ設定変更も管理系経由で行う(§19.3 の制御経路分離)。
- **OOB/コンソールサーバ = Break Glass L2.5**(§15.6)。144台規模ではシリアル集約が必須。
- **爆発半径(§19.5)**: 「1コミットで全128台再イメージ」が原理的に可能なため rate limit + halt は生命線。§5 の段階ロールアウトと合わせ二重に効く。

## 7. 成り立たない点・苦しい点・設計ギャップ・必要な拡張(忖度なし)

1. **UFM/InfiniBand の Provider 化が最大の未知数(高難度)**。UFMは「収束器の上の収束器」(§11.2-1)で内部にファブリック収束を持ち、Sakuteiは converging を drift と誤認してはならない。UFM APIに「realization完了」概念が乏しければ §10.4 Tier2(自傷猶予)/Tier3(振動→oscillating隔離)頼みになる。さらに**IBファブリックのファーム更新はスイッチ再起動でファブリック全体が瞬断**し、ノード単位のdrainでは守れない(ジョブの集団通信が落ちる)。**IBスイッチファーム更新は「全ジョブdrain前提の別クラスの大仕事」**であり、ノードローリングのWorkflowでは表現しきれない。ファブリックレベルのinvariant(「IB経路の冗長性を保つ」)は現状のWorkflow invariant語彙(min_active/quorum)では書けず、**拡張が要る**。

2. **超長時間・全144台規模Workflowの実運用性(設計ギャップ)**。四半期更新は「ドレイン待ちで実行中ジョブが数時間〜数日走る」ことがある。§9.1 の drain 待ちは `ready_condition` で表現できるが、**「数日ドレインを待つノード」を抱えたまま段階ロールアウトが数週間継続する**運用が現実的か。承認ゲートのタイムアウト(§10.5 は例24h)、S2永続化の長期保持、途中でのメンテ窓跨ぎ再開など、**Workflowの「週〜月単位の実行寿命」はv1想定を超える**可能性が高い。Task/Workflow自体がv2機能(§24.2)である点も、本シナリオでは中核が最初から要ることを意味する。

3. **Slurm drain のセマンティクスの深さ(苦しい点)**。`drain` は「新規受入停止」だが実行中ジョブは完走を待つ——マルチノードジョブは1ノードのdrainで全体が進めなくなることがあり、drain完了までの待ちが read一発では判定しづらい(ジョブ状態はSlurmの権威で、Sakuteiは深追いしない方針と衝突しかける)。「どこまでSlurmを観測するか」の線引きが実装judgment。preemption/requeue との協調は明確にSakutei外。

4. **ファーム更新の非冪等・ベンダツール依存(実装難度)**。GPU VBIOS/NIC/IBファーム更新はベンダCLI(nvidia系・mlxup等)ラップになり、多くが `TrueObserve` はできても Apply は非冪等・エラー分類が粗い最小階級(§5.4)に落ちる。§21.3 の通り横断保証(原因タグA/B・converging)が落ちる領域。rollback が効かないファーム面(VBIOS焼き失敗=文鎮化)は **Workflow の on_failure: halt + keep_drained + 人手RMA** に逃がすしかなく、自動化の限界が明確。

5. **health/canary(NCCL疎通・GPU健全性)がv2機能(§24.2)**。ファーム更新後の「本当にGPU/IBが正常か」の合成検証(§17.3)は本シナリオで必須級だが v2。v1では health-check step を外部スクリプト呼びで代替する割り切りが要る。

6. **物理修理は完全にSakutei外**(週1-2件のRMA・部品交換)。これは設計通りだが、Workflowが「人手ステップで無期限に停止して待つ」形になり、keep_drained したノードの管理が人の運用に残る。

## 8. 総合判定

**○ 条件付き成立。**

理由: 本シナリオの主戦場である**版数統一・ファーム更新・故障ドレイン**は、Sakuteiの設計思想の中心(content-address参照・多相Task状態機械・段階ロールアウト・rate limit + halt・managed field分離)にほぼ一対一で写像でき、**「128台同時リブート」事故の防止はまさにこのエンジンが解こうとした問題そのもの**である。運用4名という少人数で版数を宣言的に一元管理できる価値は極めて大きい。ownershipは全platform・sharding不要で構造もシンプル。

条件: (1) **UFM/IBファブリックのProvider化とファブリックレベルinvariantの拡張**——IBスイッチファーム更新は「全ジョブdrain前提の別クラス手続き」として設計語彙を足す必要がある。(2) **Task/Workflow(v2)の前倒し実装**と、その**実行寿命を週〜月単位に耐えるS2永続化・承認ゲートtimeout・メンテ窓跨ぎ再開**の強化。(3) **Slurm連携をdrain/resumeの2操作に厳格に限定**し、ジョブstateには触れないSchema規律の徹底。(4) health/canary(NCCL疎通検証)の代替手当。これらが揃えば、GPU/AI基盤はSakuteiの適用ドメインとして**むしろ好適**(泥臭い版管理が主・テナント砂場が無く境界が明快)である。
