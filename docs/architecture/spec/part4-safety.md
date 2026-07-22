> Sakutei 設計書 v3.0 の一部（設計の正）。全体目次と §→ファイル対応は [README.md](README.md) を参照。§番号は全 8 部を通して一意。

# 第IV部 安全機構

level-triggered な収束は、毎周・全リソースの desired と actual を突き合わせ、差分を全部埋めにいく。理想を1行間違えれば、次の1サイクルで数百リソースが同時に「差分あり」と判定され、並列 Apply で一斉に潰す。これはバグではなく設計通りの動作であり、収束は理想が誤っていても疑わない。

> **「速く確実に収束する」能力は、そのまま「速く確実に破壊する」能力の裏返しである。**
> しかも level 型は毎周繰り返す——手で戻しても次周また壊す。底なしである。

危険は3軸から独立に漏れる:**量**（一度に多すぎる）/ **種**（破壊的が混じる）/ **速**（気づく前に完了する）。本部はこの3軸への応答を、削除の規律（§12）・変更予算（§13）・修復の統制（§14）・隔離と防御（§15）として定める。安全機構を貫く原則は一つ、**デフォルトは halt、明示的意図で進む**である。

---

## §12 削除の規律

### §12.1 削除は集合演算であり、境界の宣言を要する

削除に要るのは観測 state ではなく、**理想にある集合を網羅列挙できるか**（closed-world assumption）という境界の宣言である。

```
消すべき = 現実にある集合 − 理想にある集合
```

- Ansible が `absent` の明示を要求するのは state が無いからではなく、open-world だから（playbook は言及したものしか語らない）。
- k8s が黙って消せるのは etcd に state があるからではなく、ownerRef + label selector で「管理する集合の境界」を宣言できるから。
- したがって **closed-world は state ではなく境界の宣言で作られる**。観測 state レス（§4）と削除は両立する。

集合演算の左辺（現実にある集合）を得るには集合列挙が要る。これが `Lister`（§5）であり、prune の前提である。**Lister を持たない provider に prune は許されない**。

### §12.2 DeletionPolicy（2モード）

```go
type DeletionPolicy int
const (
    ExplicitAbsentOnly DeletionPolicy = iota // open-world:absent 明示が要る
    PruneWithinScope                          // closed-world:scope 内を網羅管理(Lister 必須)
)
```

**モード1 closed-world（prune）** — 現実の全集合を網羅 List できる種別（DNS zone 内 record / volume 群 / VM 群 / FW ルール群）。境界宣言 `managed: true` により、scope 内で理想に無いものを「消すべき」と確信できる。

```yaml
scope:
  provider: dns
  zone: internal.example.com
  managed: true     # この zone 内は網羅管理。zone 外には触れない
records:
  - { name: web, ip: 10.0.0.1 }
  - { name: db,  ip: 10.0.0.2 }
# → zone 内で上記2つ以外の record は「消すべき」と確信できる(履歴 state 不要)
```

**モード2 open-world（explicit absent）** — 網羅 List が非現実的な種別（パッケージ / ファイル / 個別設定エントリ）。個別宣言し、消すには明示 `state: absent` を要求する。この層は Ansible 方式が正しい。

削除判定は共有リソースの種別で3種に分かれる。

| リソース種別 | 削除可否の判定 | 例 |
|---|---|---|
| **離散・被参照** | 参照カウントがゼロ | image / テンプレート |
| **容量プールのメンバー** | 抜いても残余が充足 + migrate 可 | クラスタ内のホスト |
| **単純専有** | closed-world scope 内で理想に無い | テナントの VM |

VM が依存するのは個別ホストではなくクラスタ（容量プール）なので、VM が乗っているホストも十分な残余容量があれば migrate して削除できる。参照カウントではなく容量充足性で判定する。

### §12.3 pruneable（二段構え・破壊的型は既定 false）

closed-world 種別でも、破壊的な種別（volume / VM）は `managed: true` だけでは消さず、**さらに削除 opt-in** を要求する。**`pruneable: false` を破壊的型の既定**とし、毎周回るループに `DROP` 相当を自動実行させない。closed-world の網羅性は「消せる」を意味するが、「消してよい」は別の明示的意図を要する、という二段構えである。

順序付きコレクション（FW ルール・ACL・ルーティングポリシー）は位置依存の順序リストであり、unordered set-diff で扱うと順序が入れ替わって穴が開く。

- **リスト全体が原子単位**。scope = リスト、prune は置換に吸収され、**Apply = 全置換**（candidate/commit と好相性）。
- ただし plan の表示粒度はルール単位の差分に展開する（**適用粒度 ≠ 表示粒度**）。

### §12.4 List 完全性の検証（prune の前提を実行時に守る）

`unknown ≠ absent`（§4）は `Read`（個別）の三値で守られるが、prune の根拠である `List`（集合）の**サイレント欠損**には無力である。ページングの途中打ち切り・フィルタバグ・相手 DB のレプリカ遅延はいずれも「成功した List」として返り、欠けた ID が「理想に無い = 削除」に直結する（volume = データ消失 / VLAN = 通信断）。「List の網羅性が prune の安全性を担保する」以上、その網羅性を実行時に検証する。

- **（a） Provider 契約にページング完全性を明記**（§28）:List は「next token が nil で終わったこと」を確認して返す。**途中打ち切りを検知したら成功として返さず、List を失敗（error）にする**。SDK に完走検証ヘルパを入れ、ページング処理を作者に手書きさせない。
- **（b） 前回観測比の件数急減 halt**:`List` 結果の基数が前回 List 比で急減（既定 −50% 超）したら、**その scope の prune をその周は保留し Event を出す**（render 入力側の desired 急減検知・§9 と対称）。前回 List 基数は S3（インメモリ・非永続。喪失しても次周は保守側 = prune 保留に倒れるので安全）。
- **（c） List に三値を許す**:網羅を保証できない今回の List を実装が申告できる `ListStatus`（`complete` / `partial` / `unknown`）を返せるようにし、`complete` 以外の周は prune を保守側に倒す。`Read` の三値と対称。

```yaml
scope:
  provider: storage
  cluster: ceph-a
  managed: true
  list_drop_threshold: 0.10   # この scope は急減閾値を厳しめに(既定 0.50)
```

急減閾値は既定 50% だが、`scope` に `list_drop_threshold` を宣言して scope ごとに上書きできる。**流動性の高い scope（テナントの短命 VM 群）は緩め、破壊が致命的な scope（ストレージ volume・VLAN）は厳しめ**に設定する。

**planned_change（§13.3）との連動**:窓内・scope 内・種別一致の宣言済み削除/再作成（棚卸し削除・一斉再イメージ）は「急減」とみなさない。宣言済みの件数減は誤発火の原因であり、planned_change が「これは意図された減少である」を宣言することで急減 halt を抑止する（宣言外の減少は引き続き halt）。

（a）（b）（c） はいずれも v1a（prune の安全性の中核）。

### §12.5 Release の非対称（下流削除を List で確認後 + cooldown）

払い出し資源（VLAN/IP/ASN）の返却 = Release は、削除の一種でありながら誤発火が最悪級の事故（使用中の値の再払い出し衝突）を招くため、非対称原則を特に厳格に適用する。素朴な「所有者が desired から消えたら返却」は、render 入力が stale（CMDB 断・理想の読み損ね）で owner 消滅と誤認すれば衝突を起こす。

- **（a） desired からの消滅では Release を発火させない。下流リソースの削除完了を確認してから**実行する（「理想から消えた」ではなく「現実から消えた」を根拠にする）。
- **（b） 削除完了は `List`（集合演算）を根拠にする**。`Observation` の三値（§4）には absent を表す値が無く、`Read` の `unknown` と「本当に消えた」を区別できない。したがって **削除完了 = 対象 ID が scope の List 結果に現れなくなったこと**（§12.1 の集合演算）とし、§12.4 の List 完全性検証（件数急減 halt・ページング完走）をそのまま前提にする。
- **（c） 返却後に cooldown / 検疫 TTL** を置く（即時再払い出しをしない。DHCP のリース再利用猶予と同型）。
- **（d） `pruneable: false` 等で下位が残存する間は Release を保留**する。

この規律により、**Release は Lister を持つ provider の下流にのみ許可**される。List できない下流の Release は保守側（保留）に倒す。allocator 側の返却プロトコルと cooldown は §19 に接続する。

### §12.6 final_backup と削除 Workflow 推奨形

破壊的削除は取り返しがつかないため、削除の直前に退避を取る規律を推奨形として定める。

- **final_backup**:破壊的型（volume / VM）の prune 前に、直近スナップショット / エクスポートを取得してから削除する。バックアップの取得先・保持は §25 の DR 経路に従う。
- **削除 Workflow 推奨形**:大量削除・容量プールメンバー削除は、収束ループ上の自動 prune ではなく Workflow（§6）として明示的にトリガすることを推奨する。順序は「List 完全性確認 → final_backup → migrate（容量プールなら）→ 削除 → List で消滅確認 → Release（下流採番があれば）」。危険な手順を毎周回るループから隔離する原則（§6）の削除への適用である。

---

## §13 変更予算

### §13.1 rate limit（cycle 単位・破壊的加重・in-flight 込み）

同時に走る変更の量に上限を張る。閾値は**破壊的変更に重み付け**する（create 100 件は概ね安全、delete 10 件は止める）。

```yaml
safety:
  max_changes_per_cycle: { absolute: 10, percentage: 5% }
  on_exceed: halt-destructive-only   # halt-all | halt-destructive-only | warn-and-proceed
```

- `on_exceed` は scope ごとに選択する（本番 = `halt-destructive-only`、dev = `warn-and-proceed`、最重要 = `halt-all`）。
- **rate limit は in-flight（非同期 Apply・§8）を含む同時変更数で評価する**。さもないと「毎周 10 件ずつ発射し続けて in-flight が積み上がる」抜け道になる。

### §13.2 累積時間窓予算（漸進的全削除を塞ぐ）

cycle 瞬間量だけを見る rate limit は、**周期をまたいだ漸進的削除**（理想から 500 件が消えたまま、毎周 10 件ずつ 50 周で全消し）に構造的に無力である（`halt-destructive-only` は「その周の超過分を保留」であって「削除意図そのものの凍結」ではない）。これを塞ぐため、cycle 瞬間量に加え**時間窓での累積破壊量**を見る。

```yaml
safety:
  destructive_budget:
    window: 24h
    max_deletes: 50            # 24h あたりの破壊的変更の総数上限
    max_scope_fraction: 20%    # 単一 scope の実在リソースの X% を超える削除進行で escalate
  on_budget_exceed: halt-all   # 予算超過 → rendered_revision 全体を凍結し人へ
```

- **24h 等の窓での破壊的変更総数**が `max_deletes` を超えたら **`halt-all`（理想変更そのものを凍結し人へ）** へエスカレーションする。
- **単一 scope の実在リソースの `max_scope_fraction` が削除進行中**、または **`halt-destructive-only` 発動が M 周連続**でも escalate する（「じわじわ全消し」の進捗率を見る）。
- **異常検知（巨大 Diff / 削除方向 Diff 急増）の "止める" セマンティクス**:検知したら「超過分保留（漸進継続）」ではなく、**その rendered_revision 全体を凍結し、planned_change 宣言（§13.3）が対応するまで一切の破壊を保留**する（非対称原則に整合）。`Plan` が破壊性フラグを持つ（§4）ことでこれを実装できる。
- 累積予算のカウンタは **S2（進行 state）相当として永続化**する（再起動で窓がリセットされると穴が復活する）。**予算カウンタはシャード単位で持つ**——シャード = ownership = 爆発半径境界（§15）であり、全域予算は外部集約で見る（§29）。

**悪意/大量破壊への追加ゲート**:安全機構は本来「誤操作」防御であり、原因タグ A（意図・§10）は「通す」側に倒れる。しかし侵害された reconciler は Apply を直接叩き、A タグとして安全機構を素通りできる。したがって **`ownership: platform` の破壊的大量変更は、原因タグ A でも承認必須**とする（「間違いは止め意図は通す」に「ただし大量破壊は意図でも人の承認を要する」を重ねる）。

### §13.3 planned_change 宣言（専用予算レーン）

累積時間窓予算・cycle rate limit・List 急減 halt（§12.4）は「誤操作・暴発」を止めるために破壊量を絞るが、これが**大量の正当作業**（棚卸しによる 500 リソース削除・HPC 500 台の一斉再イメージ・テナント一括退去）と正面衝突する。正当な作業なのに毎周予算に阻まれて進まない、あるいは予算超過で全体が halt する。かといって予算を恒常的に緩めると暴発が止まらない。この衝突を、**intent 内の第一級宣言 `planned_change` による専用解除弁**で解く。`planned_change` はそれ自体が PR 承認を経る（= 人の意図の明示）。

```yaml
planned_change:
  scope: { site: hpc-a, cluster: gpu-pool }   # 対象 scope
  kind: reimage                                # 変更種別:delete | update | reimage | recreate 等
  max_count: 500                               # 上限件数(この宣言で許す破壊/変更の総数)
  window: "2026-08-01 00:00 .. 2026-08-01 06:00"  # 時間窓(この間だけ有効)
  approval: { pr: 1423, approvers: [alice, bob] }  # 承認情報(§14.4 の要件を満たす)
```

効果は**バイパスではなく専用予算レーン**である。

- **窓内・scope 内・種別一致の変更は、宣言上限（`max_count`）までの専用予算レーンで実行**される。これは通常の累積時間窓予算・cycle rate limit とは**別枠で計上**され、通常予算を消費しない。上限件数は宣言され、超えれば止まる。予算を「無効化」するのでなく「宣言分だけ別レーンを開ける」。
- **List 急減 halt（§12.4）の抑止**:宣言済みの削除/再作成による件数減は「急減」とみなさない。棚卸し削除が誤って急減 halt を誘発する問題がこれで解ける。
- **reimage の delete 誤計上を解消**:再イメージは内部的に delete+create に見えて破壊予算を食い潰すが、**変更種別 `kind: reimage` を宣言**すれば「これは再イメージであって純粋な削除ではない」と分類され、一斉再イメージが破壊予算に delete としてカウントされない。
- **窓を過ぎたら自動失効**し、通常予算に戻る（宣言の効力は window に閉じる。恒久緩和にならない）。

**宣言外の変更は引き続き通常予算で評価される**。`planned_change` が site-a の reimage を許可していても、同じ周期で起きた宣言外の delete（別 scope・別種別・上限超過分）は通常の rate limit / 破壊予算 / halt が効く。すなわち **「宣言中でも、宣言外の暴発は止まる」**——これが「バイパスではなく専用レーン」の意味である。`planned_change` の型と専用予算レーンの評価は v1a、承認情報の requirements 連携（§14.4）は v1.5 に属する。

### §13.4 capacity gate

rate limit の一種として、クラスタ使用率が閾値を超えたら新規の変更を止める。

```yaml
safety:
  capacity_gate:
    max_utilization: 80%
    on_exceed: halt-destructive-only
```

**是正は reconciler の外**である。reconciler の役割は「危険域に入ったら止める」までで、「ではどうするか（ホスト追加 / migrate / quota 締め）」の判断と実行は含まない。超過時は `CapacityThresholdExceeded` Event（§26）を発火し、是正は Workflow として別途明示的にトリガする（自動連鎖させない）。

| 何をするか | 誰の仕事か |
|---|---|
| 使用率の観測 | reconciler（Observe） |
| 閾値超過時に新規変更を halt | reconciler（capacity gate） |
| 原因の判別（意図的増加 vs drift） | 人（原因タグ・§10） |
| クラスタ容量追加の意思決定 | 上位プラットフォーム / 人（§7 platform 層は PR 必須） |
| VM の他クラスタへの migrate | Workflow（手動トリガ推奨・§6） |
| 新規申請の quota 制限 | 上位プラットフォーム（§7） |

**キャパシティ管理のドメイン境界——現在充足性 = 収束 / 予測 = 収束の外**。capacity gate が収束として担うのは**現在の容量充足判定**（指定クラスタの残余が閾値内か・容量プール判定の延長）だけである。**時系列外挿による予測は持たない**——将来の枯渇予測は過去の観測を保持して外挿する営みであり、毎周リアルを再観測して現在だけを見る level-triggered の構造に元々存在しない（観測 state レスと相容れない）。予測は監視系へ export し（時系列予測 → Prometheus・§3 第4原則）、収束が引き受けるのは「今、閾値内か」の二値判定に限る。

---

## §14 修復の統制

### §14.1 remediation mode（auto / alert-only）

そもそも自動で直すかを決める最も手前の手綱。粒度はリソース / scope。

- **`auto`** — 検知した drift を自動修復する。
- **`alert-only`** — 検知は続け、修復だけ止める。**爆発半径ゼロ**で、最も保守的な本番リソース向け。「常に自己修復」はオンプレでは強すぎる（緊急対応で人が手を入れた変更を潰しかねない）。

### §14.2 remediation schedule（時間帯連動の自動切替）

remediation mode を手動 freeze / unfreeze でしか切り替えられないと、保守窓文化の SLA 環境ではヒューマンエラー面が残る。**時間帯連動で mode を自動切替する `remediation.schedule`** を加える（v1.5）。

```yaml
remediation:
  default: alert-only              # 既定は最も保守的な側
  schedule:
    - window: "Sat 02:00-04:00"    # 保守窓のみ auto
      mode: auto
    - window: "*"                  # それ以外は alert-only(検知は続け、修復だけ止める)
      mode: alert-only
  tz: Asia/Tokyo
```

**保守窓のみ auto、他は alert-only** を宣言的に表現する。窓の外で検知した drift は次の窓まで alert-only として持ち越され、level-triggered なので窓が開いた周に自然に修復される。freeze（§14.3）は「今すぐ全体を alert-only に倒すマクロ」、schedule は「時間帯で mode を張る宣言」であり、両者は重ねられる（freeze が最優先）。

### §14.3 優先規則（most-restrictive-wins）と freeze

複数の mode 決定源（schedule / ownership 既定 / scope 明示 / freeze）が同一リソースに対して異なる mode を指すとき、**より制限的な mode が常に勝つ**（`alert-only` > `auto`）。優先順位でなく「最も制限的」で解決するのは、安全側への非対称原則（§3）を mode 解決にも一貫適用するためである——どこか1つでも「止めろ」と言えば止まる。

- schedule が保守窓で `auto` を張っていても、`ownership: platform`（§7）や freeze や scope 明示 `alert-only` のいずれかが効いていれば、そのリソースは `alert-only` に落ちる。**auto は「全ての決定源が auto を許した」ときだけ成立**する。
- これにより「窓を開けたつもりが、実は freeze 中/platform だった」という取り違えが安全側に倒れる（窓を開けたのに動かない、はあっても、止めたつもりが動く、は起きない）。

**global freeze**:グローバルな変更凍結は、全体を一時的に `alert-only` に倒すマクロ（rate limit / gate 機構の一種）である。freeze 中も **drift 検知は続け、修復だけ止める**。

### §14.4 承認ゲート（N-of-M・申請者≠承認者）

段階適用における承認ゲートは、既存 Workflow（wave + invariant + Event/sink + フェーズ永続化）の組み合わせで表現でき、新プリミティブは不要。ゲート型そのものは §6 の Workflow を参照する。

**承認ゲートのフロー**:ステージ完了 → `StageAwaitingApproval` Event 発火 → sink 経由で人へ通知 → Workflow は「待ち」フェーズで停止（S2 の永続化により留まる）→ 承認 API `POST /workflows/{id}/gates/{gate}/approve`（却下 = halt）→ 次ステージ。タイムアウト（例 24h）で halt。承認 API 自体は認可を持たない（誰が叩けるかは上位 IdP）が、承認された事実は監査（S4・§26）に記録する。

規制環境（金融）が要求する 4-eyes（申請者≠承認者・複数承認）は、**ゲート定義への `requirements` 追加**と**承認 API の拡張のみ**で実装する（新プリミティブ不要）。

```yaml
gate:
  manual_approval:
    requirements:
      quorum: { n: 2, of: 3 }        # N-of-M:3人の承認者候補のうち2人の承認で通過
      roles: [platform-admin]        # 役割制約:この役割を持つ者のみ承認可(役割判定は上位 IdP)
      separation: applicant_ne_approver  # 申請者≠承認者を強制(4-eyes の中核)
```

- **API 拡張**:`approve` のボディに承認者 ID + 署名を載せる。ゲートは requirements を満たす承認が揃うまで「待ち」に留まり（S2）、揃った瞬間に次ステージへ進む。
- **役割制約**:承認者の役割判定は上位 IdP に委ねる（コアは認可を持たない・§7）。ゲートは「役割 X の署名が N 個必要」を requirements として保持するだけ。
- **申請者≠承認者（separation）**:`planned_change`（§13.3）や PR の申請者 ID と承認者 ID の不一致を強制でき、自己承認を型で塞ぐ。
- **全承認者を S4 監査に記録**:誰が・いつ・どの署名で承認したかを全員分 append する。承認は「事実」として恒久記録され、「どの承認ゲートを誰が通したか」が追える（§26）。
- requirements を宣言しないゲートは単一承認（team プロファイル既定・§14.5）。

### §14.5 運用プロファイル（solo / team / enterprise）

platform 破壊的変更に PR 承認必須という既定は、チーム運用を前提にしている。だが 1名運用（ホームラボ・スモールオフィス・段0-minimal・§23）では「自分で書いて自分でレビューする」承認儀式が空回りし、かえって運用を阻害する。逆に規制環境はより厳しい既定（N-of-M）を最初から要る。**規模別の既定値セット = 運用プロファイル**を定義し、`ownership` の書込ポリシー・承認ゲート・remediation の既定を規模で切り替える（個別上書きは常に可能）。

| プロファイル | platform 破壊的大量変更 | PR | 承認ゲート既定（§14.4） | 想定規模 |
|---|---|---|---|---|
| **solo** | cooling-off（遅延実行）で代替可 | 自己マージ可 | 承認者要件なし（単一 or cooling-off） | 1名運用・ホームラボ・段0-minimal |
| **team**（既定） | 単一承認必須 | PR 必須（他者承認） | 単一承認 | 少人数チーム |
| **enterprise** | N-of-M + 役割制約を既定化 | PR 必須 | N-of-M + separation | 規制・大規模 |

- **solo:cooling-off（遅延実行）による承認代替**。platform の破壊的大量変更の事前承認を、cooling-off = 遅延実行で代替できる。**`planned_change`（§13.3）の宣言から N 時間後に自動実行**され、**その間いつでも halt 可能**である。「他者のレビュー」を「時間差 + 自己による中断機会」で置き換える——1名運用では後者の方が実効的な安全になる。監査記録は team と同等（誰が・いつ宣言し・いつ実行されたかを S4 に残す）。PR は自己マージ可。

```yaml
profile: solo
solo:
  cooling_off: 2h        # planned_change 宣言から 2h 後に自動実行(その間 halt 可能)
```

- **team（既定）**:プロファイルを指定しなければ team。platform は PR 必須・単一承認・破壊的大量変更は A タグでも承認必須（§13.2）。
- **enterprise**:N-of-M + 役割制約（§14.4 の `requirements`）を既定化する。個別ゲートに書かなくても、platform の破壊的変更は自動的に 4-eyes（separation）を要求する。
- **段0-minimal（§23）と solo の対応**:段0-minimal（単一ノード・Vault/broker なし）は 1名運用と物理的に一致するため、段0-minimal では solo プロファイルを既定とする。規模が育って段0（3ノード）+ チーム運用へ移ったら team/enterprise へ切り替える。
- **プロファイルは既定値セットであって上書きを禁じない**:solo でも特定 scope に N-of-M を明示でき、enterprise でも dev scope を緩められる。プロファイルは「よく使う既定の束」を1語で選ぶ糖衣であり、個別の `ownership` / ゲート / remediation 宣言が常に優先する。

### §14.6 ブラウンフィールドの alert-only 常用

ブラウンフィールド導入では、人の変更を早々に自動で巻き戻さないため、read-only / alert-only から始めて信頼を得る。import → alert-only → scope ごとに auto へ昇格、という順序を採る。詳細は §24 に定める。

---

## §15 隔離と防御

### §15.1 blast radius（爆発半径）3軸

安全機構は「速く収束する力は速く壊す力」への応答であり、危険は独立した3軸から漏れる。本部の各節はこの3軸への応答である。

| 軸 | 危険 | 応答 | 節 |
|---|---|---|---|
| **量** | 一度に多すぎる | 変更予算（rate limit / 累積時間窓予算） | §13 |
| **種** | 破壊的が混じる | 異常検知（巨大 Diff / 削除方向急増の凍結） | §13.2 |
| **速** | 気づく前に完了する | 段階ロールアウト（canary → broad） | v2（§14.4 のゲートで骨格） |

段階ロールアウト本体（自動 canary / 自動段階昇格）は v2 に属し、v1.5 の最小 Workflow（直列 + 不変条件 + 手動承認ゲート）がその骨格を先に持つ。

### §15.2 fencing token（単一 writer の保証）

shared-nothing で「分散合意を中枢に戻さない」という方針は平常時は綺麗だが、**分断時に単一性を保証する機構**を別に要する。platform シャードの active/standby フェイルオーバーで旧 active が生きていると（GC pause / 片側分断）、2つの writer が共有基盤（NW コア・共有ストレージ）へ書き、順序付きコレクション全置換（§12.3）と組み合わさって FW ルール/ルーティングが発振し全機器全盲を誘発しうる。

- **platform シャード（単一 writer）に fencing token を必須化**する。共有基盤 provider の Apply は token 検証を通し、**古い世代の writer の書き込みを拒否**する（storage/lease fencing）。「合意は持たないが fencing は持つ」は両立する（fencing は合意より軽い）。
- **割当表（allocator 世代番号）にも fencing token を持たせ**、復元・再割当時に古い世代を fence する。static シャーディング（§17）では割当表そのものを持たず、**二重 claim（2 デーモンが同一 scope を掴む）の検知に fencing 世代番号だけを S2 として使う**（新しい可用性部品を増やさない）。
- **fencing フィールドは型定義時から最初から持たせる**（後付けは危険）。

fencing token / 世代番号は、単一 writer 前提の3経路——platform 共有基盤の書き込み・claim シャーディングの二重検知・allocator の払い出し権威（§19）——を同型の機構で守る。

### §15.3 oscillation の自動隔離

「収束器の上の収束器」（SDN / アレイ / k8s は内部に収束ループを持つ）と綱引きになると、M 回再 Apply しても Diff が縮まない。これを `oscillating` と判定し、**そのリソースを自動で alert-only に隔離**する（綱引きの自動検疫）。振動そのものの検知と多周期収束の扱いは §11 に接続する。

### §15.4 シャード = 爆発半径境界

シャードは shared-nothing の独立収束器であり、その論理境界（ownership）がそのまま物理的な爆発半径境界になる。

- **予算カウンタはシャード単位**（= ownership = 爆発半径境界・§13.2）。1シャードの予算超過は他シャードに波及しない。
- **全域不変条件（総容量・グローバル quota）はシャード内 gate + 外部集約でしか扱えない**。1シャードの invariant は自シャード内でしか強制できず、全シャード合算の制約は上位プラットフォーム（§7）や外部集約に委ねる。rate limit・累積破壊予算も同様にシャード単位で閉じる。
- **シャード間協調は現実を介して行う**——シャード同士は互いに問い合わせず、相手の作った現実を Observe して gate する（観測 state レス思想の延長）。詳細は §17 に定める。

### §15.5 デフォルト halt、明示的意図で進む

本部を貫く一つの原則を再確認する。**確信が持てないとき、加算はしても減算はしない**（非対称原則・§3）——unknown を absent と断じない、List が欠けたら prune を保留する、予算を超えたら halt する、mode 解決は most-restrictive-wins に倒す、Release は現実の消滅を待つ。

危険域に入ったときの既定は常に halt であり、そこから先へ進むには**明示的な意図**が要る。その意図の表明経路は一本化されている——コミット注記のような場当たり的タグではなく、**intent 内の第一級宣言 `planned_change`（§13.3）** と **承認ゲート（§14.4）** である。「間違いは止め、意図は通す」——ただし大量破壊は意図（タグ A）でも人の承認を要する（§13.2）。速く収束する力を持つ以上、止まる力を既定に据えることが、level-triggered な収束を運用に耐えさせる要である。


