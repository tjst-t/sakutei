# 対象構成13: 学術HPCクラスタ Sakutei 適用可能性レポート

**総合判定: ○ 条件付き成立(ただし収束の主戦場を「ノード内部の設定」ではなく「ブート供給の骨格 + 版数の可観測性」に絞る限りにおいて)**

Sakutei の「観測 state レス + level-triggered」は、計算ノードのステートレス運用(ネットワークブート・再プロビジョン文化)と**驚くほど親和する**。ただし本構成の核心である「壊れたら外し、半年に一度一斉更新する」という運用は、Sakutei が最も得意とする「常時じわじわ収束」とは時間軸が真逆であり、そこを無理に収束に載せると xCAT 文化と正面衝突する。**強みは可視化と骨格供給、苦しさは"収束させたくない"哲学との折衷**にある。

---

## 1. 構成の前提整理

- 規模: 計算ノード500(CPU)+ GPU 32 + ログイン4 + 管理系10 = 約546筐体。Lustre 専用アプライアンス4筐体、OPA/IB ファブリック。
- **ステートレス運用**: 計算ノードはネットワークブート(イメージベース)。ローカルディスクはスクラッチ専用=**永続 state を持たない**。障害時は「収束(直す)」ではなく「外して再プロビジョン(捨てて作り直す)」。
- 個体差最小: 500台がほぼ同一構成。ただし実際は BIOS 設定・ファーム版数がばらつく=**"同一のはず"と"実際の差異"のギャップ検出が主要ニーズ**。
- 変更の時間軸: 半年ごとの全系停止メンテで一斉更新。それ以外の期間は「壊れたら外す」のみ=**平常時はほぼ変更ゼロ、メンテ時は大量一斉変更**という双峰性。
- 運用: 5名(HPC専門・コード書ける)。既存ツール = xCAT 的プロビジョニング + 手製スクリプト群。**新ツールへの要求水準が高く、既存文化が強固**。

---

## 2. リソースモデリング:「イメージブート + 再プロビジョン」と level-triggered の関係

### 2.1 image@sha256 宣言と収束は相性が良い(検証: 真)

`image@sha256`(§20.3・コンテンツアドレス参照)は本構成の**中心概念にぴったり嵌まる**。500台のブート対象イメージを `boot_image: registry/compute@sha256:abc...` として宣言しておけば:

- **level-triggered が「500台が宣言されたイメージでブートすべき」を毎周検証**する。名前参照(`compute:latest`)では「同じ名前だから変わっていない」と誤認するが、ハッシュ参照なら drift(古いイメージのまま残ったノード)を確実に検知できる。
- ここで収束の正しい対象は**「ノードの内部状態」ではなく「ブート供給インフラ(DHCP/PXE/TFTP/HTTP boot + イメージ配布)の宣言」**である。ノード自身は再起動すれば宣言イメージに戻る=**再プロビジョン文化そのものが level-triggered の等価物**。「収束」を "設定をノード内部で直す" と読むと文化衝突するが、"正しいブート供給を維持し、ノードは焼き直す" と読むと完全に一致する。これが本構成成立の鍵。

### 2.2 「再プロビジョン ≠ 収束」の危険域

一方、**稼働中の計算ノードに対して Sakutei が Apply で内部を直しに行くのは禁忌**。ジョブ実行中のノード設定を drift 修復で書き換えると計算結果を汚染する。→ 計算ノードは原則 **`remediation: alert-only`**(§10.2)に固定し、「差異を報告するが直さない」。実際の是正は「ジョブ完了 → drain → 再ブート」であり、これは Sakutei の Resource 収束ではなく **Workflow / 既存 xCAT の再プロビジョン**が担う。Sakutei は "いつ・どのノードが宣言とズレたか" を握るだけ。

### 2.3 BIOS/ファーム版数ばらつきの検出を Resource でどう書くか

これは**収束(修復)ではなく可観測性(検出)のユースケース**として書くのが正解。

```yaml
resource: bare_node
id: rack07-node012          # ★物理トポロジ座標(§5.5)。シリアル/MAC は identity にしない
ownership: platform
scope: { partition: compute }
remediation: alert-only     # ★版数を勝手に直さない(firmware は Task 領域・§9.1)
desired:
  bios_settings: { hyperthreading: off, turbo: on, numa: enabled }
  firmware:
    bmc_version: "7.10"       # 収束が「あるべき版」として監視する対象
    bios_version: "2.14.1"
# serial / current_mac / gpu_ecc_count は managed:true + computed(観測のみ)
```

- BIOS 設定の diff(HT が意図せず on になった等)は **標準的な drift として検出**でき、alert-only なら爆発半径ゼロで「500台のうち何台がズレているか」の conformance レポート(§17)がそのまま出る。**"500台が同一のはず"を機械的に検証する監査台帳**として即価値が出る(§23.2 の可視化価値)。
- ファーム**版数の相違**は diff として検出する(`bios_version: 2.14.1` vs 観測 2.12.0)。ただし**是正=firmware 更新は多相 Task**(§9.1)に委譲する。収束が直接 Apply してはならない(中断=文鎮化)。`update_policy: { strategy: task, window: "Sat maintenance" }` でメンテ窓ゲートを掛ける。「**いつズレたか = 収束が検知、どう直すか = Task**」の分業がそのまま効く。

### 2.4 必要 Provider 一覧

| Provider名 | 対象 | Capabilities(TrueObserve/List/Converging/CommitConfirm) | クラス | identity 設計 | 実装難度 |
|---|---|---|---|---|---|
| `bare_node`(Redfish/IPMI) | 電源・BIOS 設定・ファーム版数観測 | ✓/✓/△/✗ | 標準〜完全 | **rack+slot トポロジ座標**。serial/MAC は computed | 中(Redfish の版差・BIOS 属性名の機種差) |
| `boot_provision`(xCAT/warewulf ラップ or 自前) | ノード→イメージ割当・ブート方式(PXE/HTTP boot) | ✓/✓/-/✗ | 標準 | node論理名(=トポロジ座標) | **高**(既存 xCAT との境界画定が本体) |
| `image_registry` | ブートイメージ(`@sha256`)存在・整合 | ✓/✓/-/✗ | 標準 | image digest | 中(ready=チェックサム一致・§20.3) |
| `dhcp_pxe`(基盤) | DHCP スコープ / PXE / TFTP / HTTP boot | ✓/✓/-/✗ | 完全 | scope名 | 中(`tier: foundation`・§6.1) |
| `dns_record`(基盤) | ノード名前解決 | ✓/✓/-/✓ | 完全 | zone内 record | 低(§24.4 の最初の一歩) |
| `slurm_config` | パーティション定義・ノード状態(drain/resume)・QoS | ✓(scontrol)/✓/-/✗ | 標準 | partition/node名 | 中(slurm.conf の順序性・reconfig 副作用) |
| `lustre_target` | Lustre アプライアンス設定(OST/MDT・クォータ) | ✓/△/-/✗ | 標準〜最小 | target名 | **高**(ベンダー固有 API・データ消失リスク) |
| `ib_opa_fabric` | IB/OPA スイッチ設定・サブネットマネージャ | ✓/△(順序)/-/△ | 標準 | switch+port | 高(順序付きコレクション・管理断ロックアウト) |
| `node_health_obs` | 監視設定(§17.2・GPU ECC/温度/IB リンク) | ✓/✓/-/✗ | 標準 | probe名 | 低(observability as Resource) |

**注意点の写像**:
- **identity は必ずトポロジ座標**(§5.5)。500台を「壊れたら外す」運用では筐体交換が日常であり、serial/MAC を identity にすると交換のたびに 500 件の intent 書き換えが発生して破綻する。`rack07-node012` を identity にすれば筐体交換で intent 変更ゼロ。
- **slurm/lustre/IB は "収束器の上の収束器" ではないが、副作用の強いラップ対象**。slurm の `scontrol reconfigure` は全ノードに波及するため、slurm.conf は順序付きコレクション扱い + Apply=全置換 + メンテ窓ゲート。
- **GPU 32台は別 partition の別 Resource** として切る(ECC 状態・MIG 構成が CPU ノードと異なる)。

---

## 3. ownership・DesiredSource・sharding設計

### 3.1 ownership 設計

本構成は**マルチテナントではなく単一運用主体**なので、tenant 層はほぼ不要。全て **`ownership: platform`**(PR 必須・FileSource/git・alert-only 寄り・rate limit 厳格・§12.1)に倒すのが自然。運用5名がコードを書けるため git/PR ワークフローは既存文化と親和する(xCAT の設定を git 管理している現場も多い)。ジョブ投入ユーザは Sakutei の管理対象外(slurm が担う)。

### 3.2 DesiredSource

- **FileSource(git)一択**。500ノードの定義は §4 のテンプレート展開で生成する(手書きしない)。
- **allocator は最小限**。HPC ではノード IP は固定割当(トポロジ座標に紐づく)が普通で、動的払い出しの必要が薄い。IPMI/BMC アドレス帯は静的に書いてよく、`AllocationAuthority` は v1 で不要(§16.5 の「採番だけ自作」すら省ける)。

### 3.3 500ノードの Observe 負荷試算

§12.2 の試算式で:**546筐体 × Redfish Observe ~500ms / 並列度16 ≈ 17秒/周**。ここに boot/image/slurm/dns 等の下位 Resource を足すと総リソース数は 2,000〜4,000 規模(ノード×平均5〜7 Resource)。**4,000 × 300ms / 16 ≈ 75秒/周**。

- これは§12.2 の「実用上限 数千〜1万」の範囲内=**単一デーモンで成立**する。シャーディングは不要。
- ただし Redfish の Observe は機種によって遅い(1〜3秒)ことがあり、並列度16では最遅ノードに漸近して天井が上振れする。**早期警戒線**(§6.2: Observe が reconcile 間隔の10s に対し無視できない比率)に触れる可能性があるため、**BIOS/ファーム版数の Observe は tier 別間引き**(§12.2・安定リソースは N 周に1回)を最初から見込む。平常時に版数がそう頻繁に変わらないので、版数 Observe を10分間隔・電源状態を1分間隔、のように分けるのが実用的。
- **もし将来スケールが問題化するなら分割軸は "site/room" や "partition(CPU/GPU)"**(§12.2)。共有基盤(Lustre/IB/DNS/DHCP)は platform シャードが1つ所有、計算ノード群は read-only 参照、が綺麗に効く。

---

## 4. render(inventory/allocation/secret)の使い方

500台を1台ずつ書くのは非現実的=**render フェーズ2のテンプレート展開(§7.5)が本構成の生命線**。

```yaml
# intent(人が書くのはこれだけ)
node_template:
  partition: compute
  count: 500
  racks: [rack01..rack20]      # inventory(トポロジ台帳)から展開
  boot_image: registry/compute@sha256:abc123...
  bios_profile: hpc-standard
# ↓ render フェーズ2 が inventory(rack/slot 台帳)× テンプレートで 500 個の bare_node を生成
#   → generated/compute/rack07-node012.yaml ... (自動生成ヘッダ付き・手編集禁止・§13.1)
```

- **inventory(トポロジ台帳)が唯一の可変入力**で、render フェーズ1 がこれを解決してフェーズ2 が展開する(§7.1)。xCAT のノード定義テーブルを inventory ソース(RenderedSource・§13.1)として読めれば、**既存資産をそのまま流用**できる=移行コストが劇的に下がる。
- CIDR 計算・BMC アドレス算出(トポロジ座標→IP の決定的マッピング)は**テンプレートのカスタム関数**(§7.5)で埋める。将来ロジックが溜まれば CUE へ(§13.3)。
- **secret**: ノード共通の BMC パスワード・slurm munge key 等は `SecretRef`(Vault)で注入(§14)。500台が同一 credential を使う HPC の慣行は、broker 導入前は層B(静的 + 定期 rotate・§15.2)で受ける。munge key の rotate は全系停止を要するので rotation Workflow をメンテ窓に固定。

---

## 5. 安全機構の適用(全系メンテ = "一斉変更がむしろ正")

本構成は**安全機構の想定と時間軸が逆**なのが最大の特徴。通常 Sakutei は「一度に多すぎる変更を halt する」が、**全系メンテでは500台一斉更新こそ正常な運用**。

- **平常時**: rate limit を厳格に(`max_changes_per_cycle: { absolute: 5 }`・destructive-only halt)。平常時に500台のうち5台以上が同時に「宣言と違う」と判定されたら、それは**理想状態のバグかブート供給の故障のサイン**であり、halt して人を呼ぶのが正しい。誤って500台の再ブースト命令が出る事故を止める生命線(§19.5)。
- **メンテ時**: §10.2 の「デフォルト halt、明示で進む」がそのまま解。コミットに **`approved-large-change: 500 reprovisions`** を付けて意図表明する。「間違いは止め、意図は通す」。rate limit を恒久解除するのではなく、**そのコミット・そのメンテ窓に限って**閾値を緩める。
- **staged rollout(§10.5)がメンテ運用に最適**: いきなり500台を焼かず、canary(1ラック)→ health gate(ジョブが正常起動するか)→ manual_approval → broad(残り)。HPC の全系更新で1ラックを先行検証する運用は既存文化にもあり、**Workflow として構造化するだけ**。
- **freeze(§10.6)= ジョブ繁忙期の変更凍結**に転用できる。締切前の大規模ジョブ実行中は全体 alert-only に倒す。

---

## 6. 段0ブートストラップの当てはめ

- 段0(Vault + reconciler + credential-broker + 管理NW到達性の最小核・§19.2)は**管理系10台のうち数台**に相当する。Proxmox 3ノード + Ceph は HPC 現場ではやや異物だが、管理系に小さな仮想化基盤(あるいは既存の管理VM基盤)を置く形で吸収できる。
- **管理NW到達性の最小核が段0**(§19.3)は HPC でも自然: BMC 管理NW と IB データ系は完全に別系統であり、§19.3 の「管理系=到達性の最小核は段0 / データ系=全て段1」の切り分けがそのまま適用できる。**IB/OPA ファブリックはデータ系=段1**(全断しても管理系経由でノードを制御でき自分で直せる=single point of blindness にならない)。
- **OOB(コンソールサーバ)= Break Glass L2.5**(§15.6)は HPC 現場に元々ある(シリアルコンソール集約は標準装備)。既存資産がそのまま Sakutei の Break Glass 経路になる。
- **段0 = Ansible(§19.2)は既存の手製スクリプト群と競合する**が、段0 は「作って忘れる」不変基盤なので影響は小さい。

---

## 7. 成り立たない点・苦しい点・設計ギャップ・必要な拡張(忖度なし)

1. **【最大の哲学衝突】"収束" vs "再プロビジョン"**。Sakutei の中核価値は「drift を検知して**直し続ける**」だが、HPC の計算ノードは「壊れたら**外して捨てる**」文化。ステートレスノードに対して Sakutei が Resource 収束で内部を直す動機がそもそも薄い(再ブートすれば宣言状態に戻るのだから)。→ **計算ノード層では Sakutei は "可観測性ツール兼ブート供給の骨格管理" に縮退する**。それでも版数ばらつき検出・conformance 台帳の価値は実在するが、「level-triggered 収束エンジン」というフル機能の8割は計算ノード層では遊ぶ。**強みが最も出るのは管理系・基盤サービス(DNS/DHCP/slurm/lustre/監視設定)であって、500台の計算ノードではない**。これは導入正当化の議論で正直に認めるべき。

2. **【時間軸の双峰性】平常時ほぼ静止 × メンテ時大量一斉**。Sakutei は「常時じわじわ回る」前提で設計されており、「半年に1回だけ大爆発する」運用とはリズムが合わない。平常時は毎周 Observe が「差分ゼロ」を確認するだけで、収束エンジンの動的機構(wave/DAG/backoff)がほぼ発火しない=**投資対効果が薄い期間が長い**。メンテ時は逆に安全機構(rate limit)を毎回緩める運用になり、「デフォルト halt」の緩め方が**形骸化するリスク**(毎回 approved-large-change を付けるなら、それはもはやゲートとして機能していない)。→ メンテ手順を Workflow として構造化し、緩和を「窓 + canary gate」に紐づけることで形骸化を防ぐ設計上の規律が要る。

3. **【xCAT との二重管理・境界画定】**。既存の xCAT + 手製スクリプトは**それ自体が既に宣言的プロビジョナ**であり、Sakutei と機能が重複する。`boot_provision` provider を xCAT ラップにするか、xCAT を置換するかで導入の重さが激変する。**現実解はラップ**(§21.2 の "実行実体だけ借りる" と同型)だが、xCAT の状態と Sakutei の宣言が二重管理になる穴が残る。slurm.conf・lustre 設定も既存スクリプトが触るため、§23.5 の「人手運用との共存」問題(手で変えた → Sakutei が戻す → 運用が信用を失う)が計算ノードで最も起きやすい。→ **§23.8 Sync Mode(現実→git 片方向反映)を移行期の主力にする**のが正着。xCAT が触り続ける scope を Sync Mode で git に記録し続け、準備が整った scope だけ Converge へ cutover する。**Sync Mode が無いと本構成の移行は現実的でない**(v1 スコープに入っているので救われる)。

4. **【lustre/IB のロングテール性】**。Lustre 専用アプライアンスと IB/OPA ファブリックは**ベンダー固有 API + データ消失/管理断リスク**の泥臭い層(§11.1 の "実装の8割")。自前 provider は割に合わず Ansible ラップ(§21.3)になるが、そうすると TrueObserve が落ちて原因タグ・converging 検知が縮退する。**この層では Sakutei の保証群がほぼ効かない**。lustre volume の prune は絶対に有効化してはならない(`pruneable: false` 死守)。

5. **【設計ギャップ・小】ready_condition の HPC 用途**。ノード立ち上げ「電源投入 → PXE → イメージ展開 → slurm resume」の連鎖は §6.7 の複数 Cycle 依存で表現できるが、**「ジョブ実行中か否か」を Observe して drain 完了を待つ ready_condition** が要る(稼働中ノードを焼かないため)。これは slurm 状態の Observe で実装可能だが、condition 名前空間(§26.2 B)の設計を詰める必要がある。

---

## 8. 総合判定

**○ 条件付き成立**

**成立する条件**:
1. **収束の主戦場を計算ノード内部ではなく「ブート供給インフラ + 版数の可観測性 + 管理系/基盤サービス」に定義する**。計算ノードは alert-only の可視化対象とし、是正は再プロビジョン(Workflow/xCAT)に委譲する。
2. **移行は Sync Mode(§23.8)を主力**とし、xCAT が触り続ける scope を片方向で git 記録、準備が整った scope のみ Converge へ cutover する。「既存全部を Sakutei 下に」を目標にしない(§23.7)。
3. **identity をトポロジ座標に固定**(筐体交換で intent 不変)、**lustre/IB は Ansible ラップ + prune 全面禁止**、**メンテ時の一斉変更は Workflow + canary gate + approved-large-change** で構造化する。

**◎ に届かない理由**: 計算ノードのステートレス・再プロビジョン文化は level-triggered と等価だが、それは裏を返せば **"収束エンジンの動的機構が計算ノード層ではほぼ遊ぶ"** ことを意味する。フル機能を正当化できるのは管理系・基盤サービス層であり、そこは規模が小さい(10〜数十筐体)。「500台の HPC を Sakutei で管理する」という看板ほどには、Sakutei の中核価値(常時 drift 修復)が500台に対して効かない——**可視化と骨格供給とメンテ自動化には十分価値があるが、それは xCAT 置換ではなく xCAT 補完である**。運用5名がコードを書ける組織であることは大きな追い風で、この折衷を運用できる素地はある。
