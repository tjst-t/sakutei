# Sakutei のコアをユースケースから見直す（8 部品の案の検証）

2026-10-31。対象は v2.12.0（main d28dc4a）と設計書 v3.77、たたき台は `docs/review/core-reduction-draft.md`。情報源は CHANGELOG・設計書 §1〜§29・設計改訂履歴 v3.0〜v3.77・docs/*.md・docs/providers・docs/trial・受け入れテスト・internal/ と pkg/・20 構成の適用可能性検討と設計レビュー・Backlog 214 件。本書は検討の記録であって決定ではない。

## 要約（1 ページ）

**結論。** たたき台の 8 部品は、これまでのユースケース 136 件と、インフラの構築・運用で考えられる 167 件の合わせて 303 件のうち、208 件をそのまま書け（◎）、62 件は欄や口を広げれば書け（○）、書けないのは 2 件（✕）、31 件はコアの外（provider・Expand・sink・道具・運用）の仕事だった。✕ の 2 件はどちらも Condition の語彙（集合の量化・時計）の問題で、新しい部品が要ると判断したのは「範囲（集合の境界）」1 つだけ。**8 部品の骨格は保てる。ただし、たたき台の定義のままでは Provider の口が 2 つ足りず（一回きりの操作・converging）、Schema が薄すぎ、Condition が unknown の倒し方を書けず、Record が「前回の周の目印」を持たないために、現状の安全の決まりの 3 分の 1 ほどが落ちる。** それぞれ小さく広げれば足りる（§5.1）。

**足りないもの（§5.1、17 件）。** 主なもの：
1. Provider の一回きりの操作の口 `Invoke`（boot_once・reset・migrate・drain）。これが無いと Procedure の動作が `apply` と `manual` だけになる。
2. Observe の答えに `converging`（相手が作業中。重ねない・消さない）。
3. Schema に `identity`・`secret`・set/list・欄ごとの「壊す」印・型ごとの `settle`。managed/computed は要らない（宣言した欄だけ比べる）。
4. Condition を三値にし、使う側が `on_unknown: wait|block|ignore` を書く。Transaction の confirm には `phase: before|after`。集合の量化（all/any/count）と時計をどう扱うかは論点（§6-3・§6-4）。
5. Procedure の段の属性（`skip_if`・`critical`・`gate`・`wait for`・段の間だけの一時的な Policy）と trigger の種類（on_drift・policy:<name>・before_delete・manual）。
6. Policy の欄（allow{kind,max_count}・until/reason/by・observe_every・max_ineffective_applies・max_concurrent_observe）。freeze と planned_change は Record に置く一時的な Policy。
7. **範囲（`ranges:`）**：provider の List の引数・prune の可否・外の持ち主を 1 か所に。9 つめの部品（§6-1）。
8. Record に「前回の周の目印」（provider の版・範囲ごとの件数・Expand の出力のハッシュ・送りかけの印・レーンの使用量と時計）。観測の値は置かない。
9. 起床の口（次の周を今すぐ）。影響範囲だけの収束は持たない。

**過剰なもの（§5.2）。** たたき台の「やめる」一覧に加えて：Provider の任意の口 15 → 5（Transaction・Invoke・Classify・Validate・ObserveWith）、Capabilities の旗 7 → 3、原因タグ 5 → 3、止める・緩める系の 10 機構 → Policy 1 つ＋一時的な Policy、観測で待つ系の 9 か所 → Condition 1 つ、2 段で当てる系の 6 機構 → Transaction 1 つ、状態ファイル 9 種 → Record 1 つ、設定の置き場 4 → 2、旗 128 → 6 ほど、provider 固有の旗 13 → 0。一方、たたき台で「やめる」に入っているが要求が残るもの：効かない Apply の上限・provider の版が変わった周の停止・容量の門・観測の間引き・外部入力の急減。いずれも Policy か Condition か Record の 1 欄で残せる。

**境界（§5.3）。** Transaction は 1 周・1 Apply の中で終わり戻すのは相手側、Procedure は周をまたぎ Record に進みを持つ。時間の窓は Policy だけ、Condition は観測だけ。Expand は採番の要求を出すだけで空きを選ぶのは Record。Invoke は観測に残らない操作だけで Procedure からしか呼べない。承認は Policy（Apply の門）と Procedure の gate（手順の門）で同じ型。

**不変条件（§5.4、30 項）。** unknown ≠ absent・観測を持ち越さない・消すのは範囲と List 完全と承認と予算が揃ったときだけ・壊す操作は数えて止める・同じ対象に Apply は 1 つ・一回きりの操作は印を先に書き二度送らない・Transaction は確かめられなければ確定しない・「済んだ」は観測で決める・most-restrictive-wins・レーンは別枠でバイパスでない・Record に書けなければ開かない・秘密の値を持たない・送れなくても収束を止めない・plan は何も消費しない。

**修正版の部品（§5.5）。** Resource／Range／Provider（＋Transaction・Invoke・Classify・Validate・ObserveWith）／Condition／Graph／Policy／Procedure／Expand／Record＋Events の 9。Go の interface の素案を §5.5 に置いた。起動の引数は intent・つなぎ方の設定・Record・`--policy-set`・`--once`・`--json` の 6 つに収まる見込み。

**迷う所（§6、15 件）。** 範囲の置き場、Observe の 3 値か 4 値か、Condition の量化と時計、承認の書き込みの口、早期収束を捨てるか、原因の分類の数、fencing、Transaction の戻しの主体、Expand の純粋さの代償、provider 設定の除外、Delete の分離、Procedure の trigger、一回性と常駐の差、規模の前提を戻す条件。
## 1. 前提と読み方

### 1.1 情報源と番号の付け方

| 記号 | 出典 | 件数の目安 |
|---|---|---|
| `S` | 設計書 `docs/architecture/spec/` part1〜8（§1〜§29、v3.77） | §ごとに拾った決まり約 310 件 |
| `C3` | 設計改訂履歴 `docs/architecture/CHANGELOG-v3.0〜v3.77.md`（78 本） | 改訂ごとの要求約 110 件 |
| `CL` | 実装の変更履歴 `CHANGELOG.md`（v1.1.0〜v2.12.0）と `README.md` | 115 件 |
| `TR` | 試用の手順と結果 `docs/trial/`（11 種） | 15 件 |
| `D` | `docs/*.md`（production・remote・read-api・status-json・plan-json・exit-codes・metrics・tracing・allocation-file・network-policy・provider-upgrade）と `docs/examples/` | — |
| `P` | provider の文書 `docs/providers/`（19 本）と `examples/external-provider/` | 90 件 |
| `K` | コードの棚卸し（`pkg/`・`internal/` の口と型、旗 128、intent の欄、受け入れテスト 804 関数、`internal/reconcile` の構造） | — |
| `SC`/`R` | 20 構成の適用可能性検討（`docs/review/scenarios/`）と設計レビュー 3 観点・差分再判定（v2.3〜v2.6）・v3.0 監査 | 構成 20・指摘 70 件 |
| `#n` | Backlog（`backlog` ブランチ `tasks.json`、214 件）の番号 | — |

本文の各ユースケースには `E-<分類>-<番号>`（これまでのもの）か `N-<分類>-<番号>`（他に考えられるもの）の ID を付け、出典を `[S §12.4]`・`[CL #166]`・`[SC-14]` のように添えた。分類は §3.0 の 20 分類（A〜T）で、§2 と §3 と §4 で同じ記号を使う。

### 1.2 判定の凡例（§4 の表）

| 記号 | 意味 |
|---|---|
| ◎ | 8 部品の組み合わせでそのまま書ける。新しい欄も口も要らない |
| ○ | 書けるが不便。欄が増える・2 つの部品にまたがる・書く人が規律を覚える必要がある・性能や運用で痛い |
| ✕ | 8 部品では書けない。部品を広げるか新しい部品が要る |
| 外 | コアの仕事ではない（provider・Expand・sink・外の道具・運用手順）。コアは口だけ持てばよい |

「コアの外」の列は、◎○✕ とは別に「その要求を満たす本体はどこか」を書く。コアの外で満たすものでも、コアに口（interface）が要ればそれを書いた。

### 1.3 たたき台の 8 部品（再掲）と本書での読み

| 部品 | たたき台の定義 | 本書で補った読み（§5 で確定する） |
|---|---|---|
| 1 Resource | `(type, id)` と欄の map。比べるのは宣言に書いた欄だけ | 加えて `ensure: present/absent`・`scope`（ラベル）・`labels`（Policy の選び先）・依存。**集合の境界（範囲）** の宣言をどこに置くかが論点（§5.1） |
| 2 Provider | Observe/Apply/Delete/List ＋任意の Transaction | Schema（欄の型・壊す変更か）を含む。任意の口は Transaction のほかに「一回きりの操作（Invoke）」と「宣言を見ないと観測できない縮退（最小階級）」が要るかが論点 |
| 3 Condition | 観測した値についての条件の式 1 つ | 三値（真・偽・不明）で評価し、**不明のときどうするか**を使う側が決める。時計を観測の 1 つに含めるかが論点 |
| 4 Graph | `depends_on` と Condition | 周の中の順序（wave）と、周をまたぐ待ち（Condition が真になるまで skip）。Procedure の段の並びも同じ Graph |
| 5 Policy | 範囲 × mode × 時間の窓 × 予算 × 承認 | 一時的な Policy（freeze・planned_change）を Record が持つ。most-restrictive-wins と「別枠であってバイパスでない」が不変条件 |
| 6 Procedure | 一回きりの手順。前の条件・動作・done_when・承認・時間の上限 | 動作は `apply`（Resource を当てる）・`invoke`（provider の一回きりの操作）・`exec`（外のコマンド）・`manual`（人）・`wait/require`（Condition）・`gate`（承認）。多相 Task・Workflow・manual は全部これ |
| 7 Expand | intent → Resource の一覧を作る純粋な関数 | 入力は intent と外部入力（inventory・採番の結果・秘密の世代）。出力は Resource・Procedure・Policy と**診断（断る・知らせる）** と**採番の要求** |
| 8 Record と Events | 進行中の手順・承認と監査・採番の結果。sink 1 つと読み取り HTTP | Record は 1 つの置き場（dir + flock）。Events は level（firing/resolved）と edge の単一型 |

### 1.4 書き方の型（§4 の表で参照する）

§4 の「8 部品でどう書くか」の列は、下の型（I1〜I20）を名指しして短く書く。型そのものは、たたき台の 8 部品を使った**提案の書式（擬似 YAML）** で示す。書式は本書の提案であって決定ではない（§5 の interface 素案と対応する）。

#### I1 宣言どおりに保つ（Resource ＋ Provider）
```yaml
resources:
  - type: dnszone/record            # <provider>/<type>
    id: www.example.com./A
    scope: {site: a}
    fields: {value: 192.0.2.10, ttl: 300}   # 書いた欄だけ比べる（managed・computed・宣言に無い欄は比べない）
```
毎周 `Observe` → 宣言と比べる → 違えば `Apply`。`unknown` なら何もしない。

#### I2 報告だけ・観測だけ（Policy mode: report ／ Provider の観測だけの能力）
```yaml
policies:
  - name: legacy-observe
    select: {scope: {site: legacy}}
    mode: report                      # 差分は知らせるだけで Apply しない（alert-only・ブラウンフィールドの最初の段）
```
provider が「観測だけ」なら mode に依らず Apply しない（Capabilities）。

#### I3 名指しで消す（Resource `ensure: absent` ＋ Policy の承認）
```yaml
resources:
  - type: proxmox/vm
    id: old-vm
    ensure: absent                    # 在れば Delete。壊す操作として予算に数え、Policy の承認（レーン）が開いているときだけ
```

#### I4 宣言外を整理する（範囲の宣言 ＋ List 完全 ＋ Policy）
```yaml
ranges:                               # 集合の境界（§5.1 で「Resource の側に置く」を提案）
  - provider: dnszone
    range: {zone: example.com.}       # provider の List の引数（何を数えるか）
    labels: {site: a}
    prune: true                       # 宣言に無いものを消してよい（既定 false）
    external_owners:
      - {owner: dhcp, id_prefix: "dyn-"}   # 触らない（観測して見せるだけ）
```
`List` が complete でない・急減した・範囲の宣言が無い・外の持ち主のもの → 消さない。消す数は Policy の予算。

#### I5 順序と待ち（Graph ＋ Condition）
```yaml
  - type: host-file/file
    id: app01//etc/app.conf
    depends_on:
      - id: fw/allow-ssh                                       # 先に当てる
      - id: redfish/system/app01
        when: observe(redfish/system/app01).power == "On"      # 観測で満たすまで skip
```
Condition が不明（観測できない）なら「待つ」。

#### I6 一回きりの手順（Procedure）
```yaml
procedures:
  - name: migrate-db
    target: {type: sqlmigrate/db, id: app}
    trigger: on_drift                     # 宣言と観測がずれたら 1 回だけ起こす（1-outstanding）
    steps:
      - invoke: {op: migrate, args: {to: "{desired.version}"}}
    done_when: observe(sqlmigrate/db/app).version == "{desired.version}"
    on_failure: {policy: halt, rollback: {op: rollback}}
    timeout: 2h
```
収束ループは Apply せず Procedure を起こすだけ。

#### I7 人の手順（Procedure の `manual`）
```yaml
    steps:
      - manual: {instruction: "GUI で FW を 2.4 に上げる", url: "https://..."}
        done_when: observe(cli-device/sw01).firmware == "2.4"
        timeout: 168h                     # 観測で閉じる。人の「やった」は閉じる根拠にしない
```

#### I8 二段で当てて確かめる（Provider の Transaction ＋ Condition）
```yaml
  - type: netconf/subtree
    id: leaf01/interfaces
    fields: {...}
    apply:                                # Provider が Transaction を持つときだけ書ける
      confirm:
        - observe(netconf/subtree/leaf01/interfaces).committed == true   # 変えたあとだけ確かめる
        - reach(ssh://leaf01:22)                                        # 管理の経路が開いていること
        - check:bgp-neighbors-up                                        # 名前で登録した確かめ
      timeout: 60s                        # 確かめられなければ相手側が戻す
```
`Prepare`（commit confirmed／写しと時間切れの戻しを仕掛ける）→ Condition で確かめる → `Commit`（確定）／`Rollback`。

#### I9 まとめて確定（Transaction のグループ）
```yaml
    apply: {group: leaf-wave-1, confirm: [...]}   # 同じ group の全部で確かめが通ったときだけ全部を Commit
```

#### I10 時間の窓・凍結（Policy）
```yaml
policies:
  - name: prod
    select: {labels: {owner: platform}}
    mode: apply
    windows: ["Sat 02:00-04:00 Asia/Tokyo"]       # 窓の外は report に落ちる
  - name: freeze-incident-123                     # 一時的な Policy（Record に置く。CLI `sakutei freeze` が書く）
    select: {all: true}
    mode: report
    until: "2026-11-01T09:00+09:00"
    reason: "障害対応中"
    by: alice
```
複数の Policy が当たれば最も厳しいものが勝つ（most-restrictive-wins）。

#### I11 大量の正当な作業（一時的な Policy ＝ planned_change）
```yaml
policies:
  - name: decommission-rack-3
    select: {scope: {rack: "3"}}
    allow: {kind: delete, max_count: 48}          # この種類をこの数だけ別枠で通す。別枠であってバイパスでない
    windows: ["2026-11-05 22:00 .. 2026-11-06 06:00"]
    approval: {quorum: {n: 2, of: [alice, bob]}, separation: true}
    applicant: carol
```
宣言の外（別の範囲・別の種類・上限超過）は通常の予算で止まる。入れ直しは `kind: reimage`（Delete と Create の対を 1 と数える）。

#### I12 採番（Expand の要求 → Record が選ぶ → intent に書き戻す）
```yaml
pools:
  - name: vmid
    range: "100-199"
    blocks: [{name: staff, range: "100-149"}]
    reuse: false                                  # 解放した値を二度と払わない
resources:
  - type: proxmox/vm
    id: app01
    fields:
      vmid: {from_pool: vmid, block: staff}
      ip: {from_pool: app-net, template: "192.0.2.{n}", kind: ipv4}
```
Expand は値を知らず「要求」を出す。コアは `generated/allocations/<pool>.yaml`（正）を読み、空きを選んで書き足し、次の周に Expand が値を見る。解放は人が CLI で。

#### I13 合成・変換（Expand）
```yaml
expand:
  - kind: go:k8s-cluster            # Go で登録した展開（WithExpander）
  - kind: exec:./expand-netpolicy   # 外のコマンド（stdin に intent と外部入力、stdout に Resource の一覧と診断）
```
`network_policy:` → nftables / proxmox-fw の規則、テンプレートの一括展開、合成リソースの子（`managed_by` を付ける）はすべて Expand。

#### I14 条件つきの作成（Condition を Policy か depends_on に）
```yaml
  - type: proxmox/vm
    id: new-vm
    depends_on:
      - id: metrics-probe/ceph-usage
        when: observe(metrics-probe/ceph-usage).value < 80
        on_unknown: block                 # 観測できないときは「止める」（容量の門は unknown を超過と見る）
```

#### I15 定期の仕事を level で書く（Resource の「古さ」を観測する）
```yaml
  - type: backup/job
    id: db-nightly
    fields: {max_age: 24h}               # provider は「最後の成功からの経過」を観測し、古ければ Apply（＝取り直す）
```
cron を持たず、「新しい結果が在る」を宣言どおりに保つ。

#### I16 段階ロールアウト（Procedure の Graph ＋ Policy の同時数 ＋ Condition）
```yaml
procedures:
  - name: fw-upgrade
    target: {type: redfish/system, select: {labels: {role: gpu}}}
    order: {by: labels.rack, concurrency: 1}
    invariant: count(observe(redfish/system/*[role=gpu]).power == "On") >= 126
    stages:
      - {name: canary, select: {labels: {canary: "true"}}, gate: {approval: {quorum: {n: 1}}}}
      - {name: rest}
    steps: [...]
```

#### I17 秘密（Resource の欄の型 `secret` ＋ SDK の取り出し口）
```yaml
    fields:
      password: {secret: "vault:/db/prod#password"}   # 値は宣言に無い。世代だけを Expand が読み、値は Apply の直前に provider が取り出す
```

#### I18 外の持ち主（範囲の `external_owners` ／ Resource `managed_by: external`）
観測して見せ、依存の待ちには使うが、Apply・Delete・prune はしない（I4）。

#### I19 知らせと監査（Events ＋ Record）
level の Event（drift・halt・unknown・承認待ち…）は firing/resolved で 1 回ずつ。Apply・prune・承認・freeze・手順の始まりと終わりは Record の監査（鎖）に残る。出口は sink 1 つ（stderr/JSON lines/webhook/Alertmanager/syslog/OTLP は sink の実装）。

#### I20 試験とツイン（Expand の入力の差し替え ＋ 偽の Provider）
本番の intent をそのまま、外部入力（inventory・採番・秘密）だけ差し替えて偽の Provider に流す。コアは入力を明示の引数で受けるので差し替え点が型で決まる。
## 2. これまでのユースケース

情報源（§1.1）に書かれた機能・決まり・口を、現場の要求に戻して並べた。機能名は「今の仕組み」の列に寄せ、「要求」の列は誰が何に困って何を望むかで書く。同じ要求から複数の機能が出ているものは 1 行にまとめ、要求が違うものは機能が似ていても分けた。分類は §3.0 の A〜T。出典の記号は §1.1。

### 2.A 物理（電源・BMC・FW/BIOS・OS の導入と入れ直し）

| ID | 要求（誰が・何に困って・何を望む） | 今の仕組み | 出典 |
|---|---|---|---|
| E-A1 | 物理サーバの電源と起動先を BMC 越しに宣言で保ちたい。止めるのは正常停止だけにし、止まらなければ強制せず知らせてほしい。パスワードは宣言に書きたくない | `redfish-system`（power・boot_enabled・boot_target・power_off: graceful_then_force は宣言したときだけ。ForceOff は予算に数える。電源の切り替え中は重ねない。届かなければ unknown） | [CL #48][S §29.1][C3 v3.15] |
| E-A2 | 「次の 1 回だけ CD から起動」「再起動」のように BMC が使い切る操作を、手順の一段として 1 回だけ送りたい。止まって起こし直したとき二度送りたくない | Workflow の `action`（`boot_once`・`reset`）、`provider.Actioner`、送りかけの印を S2 に先に書き、読み直したら送らず halt。`ErrActionNotSent` だけ送り直す | [CL #139][S §6.3] |
| E-A3 | 仮想メディアに ISO を差し、差さったことを確かめてから起動したい | `redfish-virtual-media`（ID `<ノード>/cd`）、Workflow の `require` | [CL #82 #87] |
| E-A4 | BIOS の属性を機種の基準に合わせたい。効くのは次の再起動のあとで、保留中は重ねて書きたくない | `redfish-bios`（宣言した属性だけ `Bios/Settings`、保留中は converging、属性の登録簿で計画の時点に検査） | [CL #140] |
| E-A5 | 部品ごとの FW の版を観測し、違えば承認のあとで更新したい。リブートをまたぎ、途中で止まっても二重に送らず、書き込み中に止まったら人に回したい | `redfish-firmware` の多相 Task（確かめる→送る→当てる→再起動→確かめる。承認の要る相、危険区間の人手ゲート、待つ相の引き継ぎ、`sakutei task approve/cancel`） | [CL #148 #51 #52][S §6.2] |
| E-A6 | 管理 API に更新の口が無い装置（GUI でしか上げられない）の版も宣言で持ち、人に作業を頼み、観測で閉じたい。Sakutei は装置に何も送らない | `update_policy.strategy: manual`（procedure/procedure_url・confirm_timeout・window。承認しても観測で合わなければ stuck） | [CL #150][S §6.2] |
| E-A7 | BMC・BIOS のように合う版の組み合わせがあり、1 つずつ更新する途中も含めて合わない組み合わせへの更新を計画の時点で止めたい。表は intent に置きたい | `compatibility:`（望む組と途中の組を depends_on の順で照らし、合わなければ止めて順序を案内。観測できない部品がある組は変わる部品を止める） | [CL #151][S §6.2] |
| E-A8 | 空の物理サーバに OS を入れる手順（ISO を差す→1 回だけ CD 起動→再起動→電源 Off を待つ→On→入った OS を目印で確かめる→CD を抜く）を宣言から回したい。インストーラは利用者が用意する | OS 導入の Workflow（`apply`・`require`・`action`・`wait`）、`guest-os`（ssh 越しの到達・os-release・導入の目印）、`template-file`（応答ファイルの雛形） | [TR redfish-os-install][CL #86 #139] |
| E-A9 | 入れ直しを宣言で起こしたい。入れ直しは delete+create に見えても削除と数えず、承認と窓と台数の上限つきで行いたい。store を失っても二度と走ってディスクを消さないでほしい | Workflow の `trigger: reimage` と `planned_change` の `kind: reimage`（対を 1 と数える・max_count を消費）、`done_when`（観測で済んだと見る） | [CL #88 #209][S §13.3 §6.3] |
| E-A10 | 筐体交換のたびに intent を書き換えたくない。identity は物理の座標にし、シリアル・MAC は観測だけにしたい | Schema の `identity`／`managed`／`computed`（identity は運用上の変更頻度で選ぶ） | [S §5.2][SC-05 12 13 20] |
| E-A11 | 版数統一をテンプレートの 1 変数で全台に展開し、内容ハッシュで参照したい（同じ名前の image の中身だけ変わるのを見逃さない） | 合成リソースの展開（render フェーズ 2）、content-address 参照（`image@sha256`。seam `ContentAddressed` は挙動なし） | [S §9.5 §10.6][K] |
| E-A12 | BMC が届かない・5xx・遅いときは何もせず、戻ったら直してほしい。操作の途中で故障しても重ねたくない | unknown≠absent・電源の切り替え中は重ねない（統合テストの故障注入は未着手） | [#128][S §5.4] |

### 2.B ネットワーク

| ID | 要求 | 今の仕組み | 出典 |
|---|---|---|---|
| E-B1 | ゾーンのレコードを宣言で保ち、手で変えたら戻したい。書き換えたら SOA のシリアルを上げてほしい（別のツールが採番する現場では上げない） | `dnszone`（ゾーンファイル、$ORIGIN と FQDN の正規化、SOA シリアルを管理するレコードを書き換えた Apply のときだけ `YYYYMMDDnn`、`soa_serial: keep`、再読み込みのコマンド） | [CL #70 #69 #71][C3 v3.12] |
| E-B2 | FW の規則をファイルでなくカーネルの実際の ruleset で観測・適用したい。管理外の table・chain・set には触りたくない | `nftables` の live（`nft -j list`／`nft -c -f`／`nft -f`、`tables` の範囲、集合の正規形） | [CL #121 #178] |
| E-B3 | 既定の拒否（base chain の policy）を宣言で管理したいが、変えると締め出されうる。ホスト側で戻る守りと到達の確かめが要る。Sakutei が落ちてもホストが戻ってほしい | base chain の宣言（type/hook/priority/policy の 4 欄）、変える前の確かめ、写しとホスト側の時間切れの戻し、新しい接続での確かめ、`confirm`（self/ssh/tcp/after:/observe:/check:）、`sakutei-nft-guard`（root で走る道具 1 つ、arm/disarm）、作成・変更・削除は承認（`ApprovalRequirer`）と予算 | [CL #173 #179 #191 #192 #214][S §7.4] |
| E-B4 | 到達性の最小の核（管理の ssh を通す規則）は Sakutei の外で保守し、宣言した chain から jump で参照したい | `external_owners:` か scope で外す。宣言していない base chain は読まず書かず List にも出さない | [S §7.4] |
| E-B5 | 網の間の許可を機器に依らない抽象として理由つきで書き、nftables・VM の FW に展開したい。理由の無い例外・重なり・隠れて効かない許可を知りたい。利用者ごとに重なるアドレス空間を扱いたい | `network_policy:`（networks/allows/targets、domain、interfaces、reason/tags、検査、`WithRealizer`、nftables と proxmox-fw の realizer） | [CL #174 #195 #197][S §7.6] |
| E-B6 | VM の FW の規則として許可を実現し、手で入れた規則は消さず見せ、Sakutei の規則は宣言の順に先頭に保ちたい。規則の変更は壊す操作として数えたい | `proxmox-fw`（comment の印 `sakutei: `、印の無い規則は unmanaged、族ごとに 1 規則） | [CL #197] |
| E-B7 | NETCONF の機器の設定の部分木のずれを知らせたい。機器が付ける印（commit の時刻）で drift にしたくない | `netconf`（観測だけが既定、正規形、`ordered`、宣言した属性だけ比べる、中身の無い configuration は absent） | [CL #141 #208 #211] |
| E-B8 | 機器に設定を当てるとき、届かなくなっても機器の時間切れで戻ってほしい。確定の成否が分からなければ unknown にしたい | 2 段の Apply（candidate→validate→commit confirmed→新しい ssh で確かめ→確定。`netconf_apply` を明示したときだけ。1.1 の persist） | [CL #175][S §23.4] |
| E-B9 | 複数台の同じ変更をまとめ、全部の台で確かめが通ったときだけ確定したい（1 台でも落ちれば全部確定しない）。相手の台から見ないと効いたか分からないことがある | `commit_group`（`provider.CommitGrouper`、group の中の依存は断る、確定の途中で切れた台は unknown）、`confirm` の `observe:<別の resource> expect:`・`check:<name>` | [CL #199 #200] |
| E-B10 | 機器の側（NACM）で部分木だけに絞ったユーザーで観測と Apply を動かしたい。外を読み書きする rpc を送らないでほしい | 送る rpc を限定、access-denied は理由つきで知らせる | [CL #176] |
| E-B11 | CLI でしか設定できない装置を、機種ごとの定義ファイルで観測・変更したい。本体は機種を知らない。一度きりの操作（upgrade）も定義で | `cli-device`（観測のコマンドと読み方、変えるコマンドの雛形、値の形、エラーの印、commit/rollback、`actions`。定義に合わない出力は unknown） | [CL #159] |
| E-B12 | 順序のある規則の集合は全置換で扱い、plan ではルール単位の差分を見せたい | Schema の `ordered`、Apply は全置換、表示粒度はルール単位 | [S §12.3] |
| E-B13 | 到達性（ssh/TCP/HTTP）や相手から見た経路を、依存の待ちや確かめに使いたい | `confirm` の到達の先、`guest-os`、`metrics-probe`、`require` | [CL #130 #149 #200] |
| E-B14 | 商用 FW の HA ペアの内部同期・SDN の realization・SOHO ルータの自前の収束と喧嘩したくない | `external_owners:`・managed fields・converging（Tier1〜3） | [SC-03 04 02][S §7.3 §11.1] |

### 2.C ストレージ

| ID | 要求 | 今の仕組み | 出典 |
|---|---|---|---|
| E-C1 | バケットの versioning・policy・lifecycle・tags・Object Lock を宣言で合わせたい。中身のあるバケットは消したくない。実装ごとに持たない設定を読みに行かないでほしい | `s3-bucket`（SigV4 は stdlib、endpoint の `fields`・`storage_class_aliases`、`api_base_path`、Object Lock の変更は承認） | [CL #122 #132 #160 #205] |
| E-C2 | データのディスクは VM を作り直しても引き継ぎたい。既にある volume を付け替えたい。keep のディスクが残る VM は消さないでほしい | `proxmox-vm` の `keep`（VM の tags に印）と `volume=`（reassign。承認の要る壊す変更）、付け先の無い volume は unmanaged | [CL #204] |
| E-C3 | volume・DB のような壊す型は宣言から消えても自動で消さず、二段の opt-in と承認を要したい。消す前に退避を取りたい | `pruneable: false` 既定、`ensure: absent` のレーン、`final_backup`（推奨形）、削除 Workflow の推奨形 | [S §12.3 §12.6] |
| E-C4 | 容量（プールの使用率）が閾値を超えたら新規の作成を止めたい。観測できないプールは容量内と断じない。是正や予測は収束の外 | `--capacity-gate`（旗）、設計は `safety.capacity_gate`。観測できないプールは超過扱い | [S §13.4][C3 v3.4] |

### 2.D 仮想化とコンテナ

| ID | 要求 | 今の仕組み | 出典 |
|---|---|---|---|
| E-D1 | VM（CPU・メモリ・ディスクは大きくするだけ・NIC・cloud-init・pool・電源・FW）を宣言で合わせたい。再起動を待つ変更を重ねて書きたくない。非同期の仕事（UPID）を待ちたい | `proxmox-vm`（テンプレートから clone、`apply_pending: reboot`、`task_wait`、Converging、settle 30s、`ciupgrade`、NIC の `firewall`/`mtu`、`firewall:` options、`pool`） | [CL #143 #165 #170 #196] |
| E-D2 | 段0 の VM など触ってはいけない VM を intent で除外し、provider の設定の除外は安全の網にしたい。触らないものを absent にする宣言は断ってほしい | `external_owners:`（owner/scope/provider/id/id_prefix/fields）、`ConfigExcluder`・`OwnerFieldSelector`、absent の境界の検査 | [CL #158 #163][S §7.3 §7.4] |
| E-D3 | VM を宣言で消したい（prune しない型でも）。消すのは必ず承認を通し、止めてからディスクごと消してほしい | `ensure: absent`（`kind: delete` のレーンが門、予算にも数える、`AbsentAwaitingApproval`） | [CL #163][S §12.2] |
| E-D4 | 宣言の外にある VM・規則を消さずに棚卸し（unmanaged）として見たい。宣言が 0 件の provider でも | status の unmanaged（prune しない型でも、宣言 0 件でも List）、`unmanaged_undetectable` | [CL #164 #169] |
| E-D5 | 離れたホストの systemd のユニット（quadlet のコンテナを含む）を宣言で保ち、依って立つファイルを書き換えたら 1 回だけ入れ替えたい。平文を状態に載せたくない | `systemd-unit`（state・enabled・中身、`restart_on`、`disruptive` は予算、ホストの側の印で level 判定）、`host-file`（sha256 と stat だけ観測） | [CL #155] |
| E-D6 | VM の作成・テンプレート差し替えのような分単位の Apply に drift 検知を巻き込みたくない。同じリソースへ同時に 2 つの Apply を走らせたくない | 非同期 Apply と in-flight（Cycle を重ねない、per-resource 排他、周の壁、in-flight を予算に数える） | [S §8.4][CL #12 #72] |
| E-D7 | DRS/HA が動かす配置・電源・動的 IP を「違う」と直して喧嘩したくない | Schema の `managed`（diff から除外）。宣言した欄だけ比べる | [S §7.2] |
| E-D8 | 一括 API（`/cluster/resources`）で 1 万規模を 1 コールで観測したい。バルクの結果を prune の根拠にしたくない | `BulkObserve`（complete-or-error、型分離、等価性テスト、CycleObserver、バルクに無いものは Read）。実機の一括 API は #49 未着手 | [S §5.7 §16.3][CL #27] |

### 2.E ゲスト OS とソフト

| ID | 要求 | 今の仕組み | 出典 |
|---|---|---|---|
| E-E1 | 1 台の常駐から agent を置かずに複数の離れたホストを扱いたい。ホスト鍵を確かめ、知らない鍵は拒みたい。sudo は決まった道具だけに絞りたい | ssh 越しの実行の層（`remote_config`：known_hosts・接続の使い回し・秘密鍵は SecretRef・観測用/書き込み用のユーザ）、`sakutei-nft-guard` | [CL #130 #185 #191][D remote] |
| E-E2 | 離れたホストのファイル（中身・mode・owner）を宣言で保ちたい。秘密を含むファイルの平文を観測・ログに出したくない | `host-file`（ID `<ホスト>/<パス>`、SecretRef の中身、観測は hash と stat） | [CL #155] |
| E-E3 | 専用 provider の無い雑多な設定を Ansible のモジュール 1 つで覆いたい（playbook の一回 push でなく毎周の収束で） | `ansible`（最小階級：check mode で `changed` を観測、通常実行で適用、`destructive` は利用者が宣言、原因タグと prune の保証は落ちる） | [CL #127][S §5.6 §28.7] |
| E-E4 | 入れた OS が目印つきで起動したことで導入の完了を判断したい（電源オフでは失敗を成功と見てしまう） | `guest-os`（観測だけ：到達・os-release・目印） | [CL #86] |
| E-E5 | 台ごとの差（ホスト名・アドレス・鍵・目印）だけを書き、応答ファイルを雛形から作りたい。秘密は書き出さない | `template-file`（text/template、最小階級、CheckReader） | [CL #139] |
| E-E6 | 読むだけのユーザで覗けないパスを「無い」と読んで作り直したくない | 読むだけでは観測できない項目は unknown（在るものを無いと読まない） | [CL #185][S §20.7] |

### 2.F DB とスキーマ

| ID | 要求 | 今の仕組み | 出典 |
|---|---|---|---|
| E-F1 | ロール・DB・権限・拡張を宣言で合わせたい。パスワードの平文をサーバのログに出したくない。利用者の表がある DB は消したくない。PUBLIC の権限も扱いたい | `postgresql`（psql を exec、SCRAM の検証値、`verify_passwords`、PUBLIC は宣言どおり・未宣言は prune しない、bypassrls/replication） | [CL #125 #133 #137] |
| E-F2 | 版つきのマイグレーション（非冪等）を窓の中で 1 本ずつ流し、失敗したら戻したい。完了は現実（schema_migrations）に問いたい。常駐が二重に起動しないこと | `sqlmigrate`（`strategy: task`・`window`・`rollback_on_fail`、SQLite と PostgreSQL（advisory lock）、`task list/cancel`、1-outstanding） | [CL #92 #126][TR sqlmigrate] |
| E-F3 | 観測用の弱いロールが作ったばかりの DB に繋げず List が落ちて prune が止まる。境界の外のロールに CONNECT を宣言で付けたい（足すだけ） | `connect_roles`（作る Apply の中で付け、外さない） | [CL #203] |

### 2.G ID と秘密と証明書

| ID | 要求 | 今の仕組み | 出典 |
|---|---|---|---|
| E-G1 | LDAP の項目の属性を値の集合ちょうどに合わせたい。パスワードは書き込みだけ。子のある項目は消さない。検索の根と filter で範囲を絞りたい | `ldap`（ldapsearch/ldapmodify を exec、DN の正規形、LDAPS/StartTLS、`base_dn`/`filter`/`scope` は intent） | [CL #144 #182] |
| E-G2 | 秘密の値を宣言に書かず参照で書き、ログ・Event・diff に平文を出したくない。rotate したら世代で気づきたい（値は見ない）。git に世代を書き戻すループは作らない | `SecretRef`（`String()` は `***`、`Resolve` は Apply 直前、`Zero()`）、世代をフェーズ 1 で読む、`#gen` はピン留め | [S §4.8 §20.2] |
| E-G3 | Vault 以外の金庫や自前のコマンドから秘密を引きたい。つなぎ方は設定に、どの秘密を使うかは intent に | `secret_backends:`（`vault-kv2`：token/AppRole・`command`）、`pkg/sdk.SecretBackend`、`WithSecretBackend` | [CL #188] |
| E-G4 | 金庫に届かない（sealed）ときに秘密を absent と断じて壊したり全体を止めたりしたくない | 世代が読めない ref を持つリソースは unknown（Observe も Apply もしない・下流は待つ）。last-resolved TTL は見送り | [CL #188][S §20.3] |
| E-G5 | 観測（plan・status・毎周）は読むだけの資格で、書き込みは Apply の直前だけ書き込み用の資格で。書かない回は書き込み用の資格が無くても起動したい | `provider.WithWrite`/`Writing`、設定の `access`、対象ごとの `observe:`/`write:`、Task の実行は書き込み、最小権限の文書 | [CL #185 #186][S §20.7] |
| E-G6 | 拠点の資格は拠点分だけ置き、1 拠点の漏洩が全拠点に及ばないようにしたい | `--secret-store` と claim の接頭辞、`sakutei secrets extract` | [S §20.5] |
| E-G7 | 認証基盤が全滅しても機器に入れる最後の鍵が要る。こっそり使われては困る | Break Glass（オフライン保管、`BreakGlassUsed`、使用後 rotate は人が明示キック）。credential-broker・JIT は v2 | [S §20.4 §20.6] |
| E-G8 | 秘密の存在・鮮度（90 日で回す）は保ちたいが値は収束しない。回す手順は Workflow | `rotation: every`（設計のみ、未実装） | [S §20.1][C3 v3.60] |

### 2.H 監視と通知

| ID | 要求 | 今の仕組み | 出典 |
|---|---|---|---|
| E-H1 | ずれ・halt・Apply の失敗を毎周撒かず、始まりと終わりで 1 回ずつ知らせたい。続けば stuck に上げたい。確かめられなかった周に消えたとみなしたくない | level の Event（kind + resource をキーに firing/resolved・count・since・class、`--stuck-after` 5）、確かめられなかった周は firing のまま | [CL #79][S §22.3] |
| E-H2 | Event を webhook・Alertmanager（複数台）に送りたい。送れなくても収束を止めない。認証ヘッダの値を漏らさない | `--event-webhook`・`--event-alertmanager`・`--event-notify-config`（送り直し・backoff・1000 件・SecretRef のヘッダ・宛先ごとの数） | [CL #131 #154] |
| E-H3 | 収束の状態（drifted・unknown の件数・予算の残り・halt・周の時間）を Prometheus で見たい。周・観測・Apply を span で見たい | `/metrics`、OTLP（`--otlp-endpoint`、既定は出さない、欄の値は載せない） | [CL #54 #189] |
| E-H4 | /metrics の値（容量・健全性）を依存の待ちや手順の前提に使いたい。自前の prober は持たない | `metrics-probe`（名前・ラベル・sum/max・比べ方を provider の設定に書き `ready.<名前>` の真偽で待つ。届かない・無いは unknown） | [CL #149] |
| E-H5 | 外の道具が状態を読みたい（status/plan/graph/events を機械で）。遅い読み手で収束を止めない。外に出すときは TLS と読むだけの認可 | `--read-listen`（GET /status・/plan・/graph・/events SSE）、`status --json`・`plan --json`、`--listen-auth-config`、平文は明示の旗 | [CL #118 #135 #136 #161] |
| E-H6 | 全体を止める更新は止める前に知らせ、見積もりを過ぎたら重く知らせたい | `Disruption`（level）、`notify_before`、overrun で stuck | [CL #152] |
| E-H7 | 監視の設定（scrape・alert・blackbox）を Resource として収束し、リソースを消したら監視設定も消えてほしい | observability-as-Resource（設計。実装は host-file/systemd-unit で配る方針、#53 は後回し） | [S §22.2][#53] |

### 2.I ブラウンフィールド

| ID | 要求 | 今の仕組み | 出典 |
|---|---|---|---|
| E-I1 | 既にある現実を intent として書き出したい。state ファイルは作らない。揺れる欄は managed 候補として示してほしい。List が不完全なら書かない。秘密の平文は書かない | `sakutei import`（`--samples`・`--force`、ExplicitAbsentOnly で出す、Computed/Managed は書かない） | [CL #22][S §24.4] |
| E-I2 | 現実を generated/ に記録して git にコミットし、人がレビューして intent に取り込みたい。手運用・既存ツールと数か月並行したい | Sync Mode（`scopes[].mode: sync`、自動生成ヘッダと sha256、1 周 1 コミット、trailer で見分ける、手編集・直接コミットがあれば止める、List 不完全なら書かない） | [CL #23 #35][S §24.5] |
| E-I3 | まず観測だけで信頼を得てから scope ごとに直す・消すへ上げたい。「勝手に戻す」を一度でもやると信頼を失う | alert-only から始める、`scopes[].remediation`、`pruneable: false`、昇格基準（managed 候補のサジェスト） | [S §14.6 §24.2][SC-03 18] |
| E-I4 | API を持たない旧機器や紙の台帳にしか無い資産を、観測できないまま台帳に持ち、prune の巻き添えにしたくない | `observability: none` + `ledger_only` + `last_verified`（設計。List の集合演算から除外） | [S §5.8] |
| E-I5 | 外のオーケストレータ（Heat・operator・VNFM・MHA）が持つ範囲は触らず、観測して待つだけにしたい | `external_owner`/`external_owners:`、ready_condition の一方向協調 | [S §7.3] |
| E-I6 | 管理の範囲（scope・表・ホスト・部分木・検索の根）は宣言に置き、provider の設定と食い違えば断ってほしい。ツールを失っても範囲が分かるように | `providers:`（`IntentRanges`、設定にだけあれば警告・両方で違えば断る）、ranges.md | [CL #181 #182][S §7.7] |
| E-I7 | 既存のゾーン・ruleset の読み方の癖（$ORIGIN・相対名・大文字・集合のまとめ・$TTL 未対応）で誤って二重に作らない | 正規化とロードでの拒否（相対名・大文字の ID） | [CL #69 #71 #178] |

### 2.J 日々の運用

| ID | 要求 | 今の仕組み | 出典 |
|---|---|---|---|
| E-J1 | 一度に変えすぎない。壊す操作は重く数え、周をまたいだ「じわじわ全消し」も止めたい。超えたら超過分の保留でなく凍結してほしい | rate limit（周ごと・in-flight 込み）、累積時間窓予算（`--max-destructive-per-window`・`--window-duration`、halt-all）、`--approve-large-change` | [S §13.1 §13.2][TR D3] |
| E-J2 | 大量の正当な作業（退役 500・一斉入れ直し・閉店の 100% 削除）を予算を緩めずに通したい。宣言の外の暴発は止まってほしい。窓を過ぎたら自動で失効 | `planned_change`（scope/kind/max_count/window/applicant/approval/requirements/cooling_off。別枠でバイパスでない、枠は発射直前に消費、急減 halt の抑止、S4 で時計と使用量を持つ） | [CL #39 #44][S §13.3] |
| E-J3 | 保守の窓でだけ直し、窓の外は検知だけにしたい。手動の freeze/unfreeze の人為ミスを無くしたい | `remediation:`（default・tz・schedule[window, mode]）、窓の外のずれは持ち越す | [CL #40][S §14.2] |
| E-J4 | 障害対応中は全体を止めたい（検知は続ける）。誰が・なぜ・いつまでを残し、期限で解けてほしい。動いている常駐を止めずに | `--freeze`（理由と `--audit-log` が必須）、`sakutei freeze start/end/status`（`--freeze-state`）、`Frozen`、読めない周は freeze 扱い | [CL #184][S §14.3] |
| E-J5 | 複数の決定源（schedule・scope・freeze・旗・disruption の窓）が違う mode を指したら、必ず止まる側に倒したい | most-restrictive-wins | [S §14.3] |
| E-J6 | 直したのに効かない（応答は成功だが現実が変わらない）とき同じ Apply を繰り返さず止めてほしい。一回性でも「効かなかった」を終了コードで知りたい | Apply のあとの読み直し（settle）、`max_ineffective_applies`、oscillating、`apply-check release`、終了コード 4 | [CL #166][S §11.1] |
| E-J7 | 空から組む構成（FW の許可→その奥のホスト）を 1 回の reconcile で収束させたい | 依存先の Apply が効いたあとの依存元の観測し直し（`--reobserve-max/-time`、常駐は次の周を繰り上げ） | [CL #177] |
| E-J8 | 全体を止める型の更新（ゲートウェイ）は、止めてよい時間帯・上流の積もり・見積もりと影響を人に見せて承認を取り、止める前に知らせたい | Workflow の `disruption:`（estimate/affects/window/upstream/max_wait/notify_before/gate の 7 段） | [CL #152][S §6.3] |
| E-J9 | 変更の前に「何が変わるか」を正確に見たい（dry-run が嘘をつかない）。plan で見たものと同じものを reconcile したい | `sakutei plan`（Observe/Diff の分離、plan の観測を持ち越さない）、`--expect-revision` | [S §1.5 §8.6][CL #46] |
| E-J10 | 1 名運用では自分で書いて自分で承認する儀式が空回りする。規制環境は最初から N-of-M。規模ごとの既定を 1 語で選びたい | `profile: solo/team/enterprise`（solo は cooling-off で承認を代替） | [CL #44][S §14.5] |
| E-J11 | 長く効く起動の引数（予算・mode・settle・間隔）を旗でなく宣言に置き、旗で緩められないようにしたい。常駐と一回性、役ごとに違う値にしたい | `runtime:`（締める旗は使う・緩める旗は断る、`daemon:`/`oneshot:`/`roles:`、`--runtime-role`） | [CL #183 #193 #213][S §26.7] |
| E-J12 | provider の版を上げた直後に全リソースが再適用されるのを止め、差分が版の違いだけかを人が確かめてから進めたい | `ProviderUpgradeDiff`（cause C は自動適用しない、`ProviderVersion` の fail-closed、`--cause-state`） | [S §10.2 §10.4][D provider-upgrade] |
| E-J13 | 「なぜ変わろうとしているのか」「いつからずれたのか」に答えたい（意図・drift・ツール更新・波及・外部入力） | 原因タグ A〜E（per-resource 帰属）、last-seen リング（S3）、status の表示 | [S §10.2 §10.5] |
| E-J14 | 終了コードで結果を機械に伝えたい（収束した・失敗・観測できないものがある・効かなかった） | exit-codes（reconcile 0/1/4、status 3、allocator 2） | [D exit-codes][CL #117 #166] |

### 2.K 障害と復旧・DR

| ID | 要求 | 今の仕組み | 出典 |
|---|---|---|---|
| E-K1 | 観測できない（届かない・認証失敗・鍵の不一致・壊れた出力）ときは「無い」と断じず、何もしないで戻ったら直してほしい | 三値の観測（ok/unknown/converging）、unknown≠absent の適用先 10 種（届かない・読めない・覗けない・定義に合わない・バルクに無い・NaN・見えない・組の部品が unknown・done_when が unknown・settle 内に観測できない） | [S §4.5 §3.3][C3 §3-A] |
| E-K2 | List が急に減ったら（分断・ページング打ち切り・レプリカ遅延）prune を止めてほしい。自分が消した分や宣言した削除は急減と見ないでほしい | List の守り（complete-or-error・急減 50%・`ListStatus`・`list_drop_threshold`）、planned_change と自分の prune の差し引き | [S §12.4][CL #75] |
| E-K3 | 外部入力（CMDB・inventory）が落ちた周に desired が空になって全削除にならないでほしい。届いたが歯抜けのときも | 前回解決値の保持、incomplete≠empty（`--inventory-min-ratio`・`--inventory-expected-min`、halt 中は基準を動かさない） | [S §9.2][CL #31] |
| E-K4 | 衛星（採番・金庫）が落ちても既存の維持は続き、止まるのは新規採番と秘密を持つ Apply だけにしたい | 停止影響表、waiting-allocation（下流 skip）、Lookup キャッシュ、unknown への縮退 | [S §22.4 §19.9] |
| E-K5 | ツール自身が死んで再起動しても、途中の手順（Task・Workflow）を続きから再開したい。危険区間で止まったものは自動で再開せず人に回したい | S2（task-store・workflow-store）、相の記録、危険区間の人手ゲート、`advance.lock` | [S §4.9 §6.2][CL #52] |
| E-K6 | 拠点が中央（git・金庫）から切れても前回の intent で維持を続け、戻ったら追いついてほしい | 読み取り専用ミラー、届かない間は前回の intent、一度も pull できなければ周を飛ばす | [S §17.4] |
| E-K7 | 主拠点が全損したとき DR 拠点が引き継ぎ、DR 中の intent の変更を署名して後で合流したい。分断と全損は機械では区別できないので人が決める | DR モード（DR plan のプリステージ・break-glass の署名と到達不能判定 3 回・事後合流 PR・`BreakGlassWriteUsed`） | [CL #45][S §25.1] |
| E-K8 | 分断で 2 つの常駐が同じ範囲を直さないようにしたい | fencing token（世代・holder・FENCED は再起動まで取り返さない） | [S §15.2] |
| E-K9 | 店舗のホストが全損しても「失うものゼロ」で再イメージ→pull→収束で戻したい | S2-free シャード（`--s2-free`、ローカル S2 を持たない。手順は中央で） | [S §17.5] |
| E-K10 | 綱引き（自分の Apply と相手の変更が打ち消し合う）を自動で検疫したい | oscillating（効かない Apply の上限で alert-only に隔離） | [S §11.3][CL #166] |
| E-K11 | 常駐が周のエラーで止まり続けている・止まったことを status で知りたい | `--daemon-status`（consecutive_error_cycles・last_error） | [TR observe] |

### 2.L 監査と規制

| ID | 要求 | 今の仕組み | 出典 |
|---|---|---|---|
| E-L1 | 誰が・何を・なぜ・いつ（Apply・prune・承認・freeze・planned_change・Workflow の run と手・採番の解放）を改ざんしにくい形で残し、外へ即時に送りたい。値は書かない | S4 監査（JSON Lines・seq/prev の鎖・`audit verify`・回し・syslog RFC 5424 TCP/TLS・送り直し）、Workflow の run.start/step/run.end | [CL #187 #210][S §26.3] |
| E-L2 | 大量の壊す変更は意図（A）でも人の承認を要したい。N-of-M・役割・申請者≠承認者。承認の事実を全員分残したい | 承認ゲート（`requirements`、ゲート単位の timeout）、planned_change の `approval`、platform の大量破壊は承認必須 | [CL #43][S §6.4 §13.2 §14.4] |
| E-L3 | 承認 API に認可を持たせたくない（役割の真偽は IdP）。読む口は TLS と 1 ビットの認可だけ | 承認 API は署名と ID を記録するだけ、`--listen-auth-config` | [S §6.4 §22.1] |
| E-L4 | 適合レポート（ずれ・観測できない・宣言外）を機械で読み、証跡にしたい | `status`（--json、終了コード 3）、plan --json | [CL #118 #135] |
| E-L5 | 作業の前に人が dry-run を確かめた証跡、plan と reconcile の同一性 | `--expect-revision` | [CL #46] |
| E-L6 | 拒否した承認・期限後の承認・cancel も監査に残したい | 未決（#109） | [#109] |

### 2.M 複数人・複数チーム

| ID | 要求 | 今の仕組み | 出典 |
|---|---|---|---|
| E-M1 | 共有基盤（platform）は PR 必須・厳格、テナントは即時・緩く。この 1 軸で書込経路・remediation・予算・prune 境界を決めたい | `ownership: platform/tenant`（設計の 1 軸。v1 では一部） | [S §3.6 §7.1] |
| E-M2 | 範囲ごとに mode・承認者・予算を変えたい | `scopes[].remediation`、planned_change の scope、profile | [S §14] |
| E-M3 | 同じ ID を 2 つの intent が宣言したら読み込みで断ってほしい。未知のキー・循環も | loader（Local ID は intent 全体で一意、`additionalProperties: false`、循環検出） | [S §21.3] |
| E-M4 | 承認を CLI でも API でも出したい。待っていないゲートは拒否 | `sakutei approve/reject`、`POST /workflows/{id}/gates/{gate}/approve` | [TR workflow-gates] |
| E-M5 | 監視だけの常駐と進める常駐を同じ intent で並べたい。進めない常駐を誤りにしない | `runtime.roles`、`--workflow-observe-only`、`WorkflowAdvancedElsewhere` | [CL #210 #213] |
| E-M6 | テナントのセルフサービス（即時書込・カタログ） | DBSource・catalog（v2、未実装） | [S §21][#57 #64] |

### 2.N 規模

| ID | 要求 | 今の仕組み | 出典 |
|---|---|---|---|
| E-N1 | 1 台でどこまで載るかを provider の階級別に見積もりたい。相手の API を同時に叩きすぎない | 天井算術（同時度 16・10s 予算の表） | [S §16.1 §16.2] |
| E-N2 | 安定したリソースの観測を間引きたい（BMC は遅くバルクが無い）。既定は毎周全部 | `observe_tiers`/`observe_tier`（初回・未収束・desired 変化の周は必ず観測） | [S §16.5][CL #30] |
| E-N3 | 拠点ごと・チームごとに常駐を分け、互いに問い合わせず現実を見て協調したい。担当の分割を intent で宣言し PR でレビューしたい | claim-as-intent（`--claim`・`shards/<name>.yaml`・sparse checkout・git 配布・開店/閉店 PR） | [S §17] |
| E-N4 | 300 拠点が中央 git を直接叩かないようにしたい | 読み取り専用ミラー 2 拠点（5 fetch/s） | [S §17.4] |
| E-N5 | 早く起きて（PR マージ・API 書込）影響する範囲だけ秒で収束したい。真実はヒントで運ばない | 早期収束パス（`Watcher`・`ChangeHint`・scoped run。CLI には未配線） | [S §8.6 §21.2][K] |

### 2.O ブートストラップ

| ID | 要求 | 今の仕組み | 出典 |
|---|---|---|---|
| E-O1 | 段0（3 ノード＋Ceph＋VM-HA＋金庫）は Ansible で人が組み、Sakutei はその上で動く。自分の土台は prune しない・依存先の到達設定は収束しない | 段0 の設計、自己収束の除外、`external_owners:` | [S §23 §2.1][SC-03] |
| E-O2 | 1〜2 台の最小構成（file の秘密、金庫なし）から始めたい | 段0-minimal（`--secret-store`、solo 既定） | [S §23.6] |
| E-O3 | 常駐を置かず、作業者の PC から一回性に収束したい（「開いたらまず status/plan」） | 段0-zero（`sakutei reconcile`。state ファイルを持たない。cron 疑似常駐は却下） | [S §23.7][C3 v3.3] |
| E-O4 | 外の provider を自前のバイナリに組み込みたい。設定は 1 つのファイルで | `pkg/cli.Main(WithProviders…)`、`--provider-config`、`examples/external-provider` | [CL #119][S §28.9] |

### 2.P テストとツイン

| ID | 要求 | 今の仕組み | 出典 |
|---|---|---|---|
| E-P1 | 偽の provider でコアの性質（unknown で消さない・予算を超えない・承認なしで壊さない・List 欠損で消さない）を現実の bytes と件数で確かめたい | fakeprovider（fail_inject・apply_count）、chaos テスト 23 本、受け入れテスト 804 関数 | [K §6] |
| E-P2 | provider の契約（冪等・absent→Create→Read・List 完全・unreachable≠absent・Schema の lint）を自動で確かめたい | `sdk.ContractTester`・`LintSchema`・`ValidateProvider`・`BulkEquivalenceTester` | [S §28.3][K] |
| E-P3 | 本物の装置の模擬（qemu-bmc・netopeer2・Proxmox の入れ子・SeaweedFS・389ds・postgres・OpenBao・sshd）で統合テストを回したい | `make integration-*`（check には入れない） | [P][CL] |
| E-P4 | 本番の intent を外部入力だけ差し替えてツインに流したい（本番のプール・秘密に触れない）。坪庭で全部偽物にしたい | `--devenv`（フェーズ 1 の差し替え）、twin は v2 | [S §26.2][#65] |

### 2.Q 緊急時の手作業との共存

| ID | 要求 | 今の仕組み | 出典 |
|---|---|---|---|
| E-Q1 | 手で入れた変更をツールが戻さないよう、範囲や全体を報告だけにしたい。脱出口を最初に握らせたい | alert-only・freeze・`scopes[].remediation` | [S §14.1 §14.3][SC-03] |
| E-Q2 | 手の変更を正にしたいときは取り込む道が欲しい | import・Sync Mode | [S §24] |
| E-Q3 | 人の作業（部品交換・unseal）を手順の一段として待ち、観測で閉じたい。人に作業指示は出さず、人が変えた現実を観測して進む | human-in-the-loop（承認ゲート＋観測ゲート、作業中 scope の freeze、1-outstanding、timeout 168h） | [S §14.7][C3 v3.2] |
| E-Q4 | 人とツールが取り合わないように、効かない Apply の上限と外の持ち主を持ちたい | oscillating、external_owners | [S §11.3 §7.3] |

### 2.R 時間で動くもの

| ID | 要求 | 今の仕組み | 出典 |
|---|---|---|---|
| E-R1 | 直してよい時間帯・止めてよい時間帯・Task の窓・planned_change の窓 | `remediation.schedule`、`disruption.window`、`update_policy.window`、`planned_change.window` | [S §14.2 §6.2 §13.3] |
| E-R2 | freeze と planned_change の期限、承認ゲートの timeout、manual の confirm_timeout、cooling-off の経過（非常駐では「次の回に N 時間経っていれば」） | `--freeze-until`、`window`、`gate.timeout`、`confirm_timeout`、S4 の時計 | [S §14.3 §13.3 §14.5] |
| E-R3 | settle・backoff・rollback の時間切れ・Transaction の確かめの上限 | `settle`、backoff 15 分、`ruleset_rollback_timeout` 30s、`netconf_confirm_timeout` | [S §11.1 §8.5][P] |
| E-R4 | 本物の時計しか無く、試用で窓を待つしかない | 差し替えの口は無い（`Config.Now` は常駐にある） | [TR remediation-profile][K] |

### 2.S ツール自身

| ID | 要求 | 今の仕組み | 出典 |
|---|---|---|---|
| E-S1 | 設定の置き場（旗・intent・provider 設定・ホスト上の道具の設定）の食い違いの規則を減らしたい | 現状は 4 つの置き場と 3 種の規則（most-restrictive／警告・断る／締める・緩める・向きなし） | [C3 §4] |
| E-S2 | 状態ファイルを 1 か所にまとめたい（9 種に散っている） | task-store・workflow-store・alloc-store・apply-check-state・freeze-state・daemon-status・cause-state・fencing-store・audit-log | [K §4.1] |
| E-S3 | systemd で動かし、ロック・ログ・資格・TLS・監査の置き場を決めたい | production.md、`<store>.advance.lock`、file lock | [D production] |
| E-S4 | 外の Go モジュールとして取り込めること | module path `/v2`、`make check-module` | [CL #146] |
| E-S5 | 常駐の役を選んで起動したい。宣言に無い役は断る | `--runtime-role` | [CL #213] |

### 2.T 外の道具との共存

| ID | 要求 | 今の仕組み | 出典 |
|---|---|---|---|
| E-T1 | Terraform・Ansible・xCAT・kolla の資産と並行し、モジュールだけ借りて実行モデルは借りない | ansible provider、external_owners、Sync Mode | [S §22.1][SC-08 13 19] |
| E-T2 | k8s の中は GitOps、外（殻）は Sakutei。operator の派生物を消さない | external_owners・prune は明示の CR だけ | [SC-09 10] |
| E-T3 | 監視の判定・通知の経路・沈黙は Prometheus/Alertmanager に委ね、自前の prober は持たない | metrics-probe、Event の sink、observability-as-Resource | [S §22.1 §22.2] |
| E-T4 | 外の採番権威（NetBox）を seam の裏で差し替えたい | `AllocationAuthority`（SQLite・NetBox は v2） | [S §19.9][#36] |
## 3. インフラの構築・運用で考えられる他のユースケース

これまでの情報源に無い（または部分的にしか無い）要求を、分類を先に立ててから埋めた。ID は `N-<分類>-<番号>`。既存の機能で既に満たしているものも、分類の抜けを防ぐため載せ、備考に「既存：U-…」と対応を書く。各行の「要求」は現場の言い方で書き、§4 の表で 8 部品への当てはめを判定する。

### 3.0 分類の立て方

| 分類 | 内容 |
|---|---|
| A 物理 | 電源・BMC・FW/BIOS・OS の導入と入れ直し・部品交換・退役時の消去・在庫 |
| B ネットワーク | スイッチ・ルータ・FW・LB・DNS/DHCP・IPAM・VPN・到達性 |
| C ストレージ | ブロック・ファイル・オブジェクト・テープ・バックアップ・スナップショット・容量 |
| D 仮想化とコンテナ | VM・テンプレート・移動・HA・コンテナ・k8s のクラスタ |
| E ゲスト OS とソフト | パッケージ・ユーザ・鍵・ファイル・サービス・再起動・パッチ |
| F DB とスキーマ | ロール・DB・権限・マイグレーション・レプリカ・切り替え・バックアップ |
| G ID と秘密と証明書 | ディレクトリ・SSO・鍵・トークン・証明書の発行/更新/失効・回転 |
| H 監視と通知 | 監視の設定・通知の経路・保守の窓の沈黙・ツール自身の健全性 |
| I ブラウンフィールド | 取り込み・部分所有・信頼の段階・宣言外の棚卸し |
| J 日々の運用 | 保守の窓・ローリング更新・退役・容量・コスト・変更の記録 |
| K 障害と復旧・DR | 部品の故障・拠点の断・復元・リハーサル・分断 |
| L 監査と規制 | 誰が何を・承認・職務分掌・WORM・証跡・秘密の非露出 |
| M 複数人・複数チーム | 所有・境界・承認者・衝突・通知先 |
| N 規模 | 件数・台数・拠点・観測の頻度・分割 |
| O ブートストラップ | 段0・自分自身の管理・鶏と卵 |
| P テストとツイン | 偽 provider・乾式実行・縮景・故障注入・性質のテスト |
| Q 緊急時の手作業 | 凍結・手の変更の検出と取り込み・break-glass・人の手順 |
| R 時間で動くもの | 期限・定期・古さ・有効期間（他の分類にまたがる横串） |
| S ツール自身 | 版上げ・HA・設定の置き場・資格・外部依存の停止 |
| T 外の道具との共存 | Ansible/Terraform/k8s operator/VNFM/監視系との役割分担 |

### 3.1 A 物理

| ID | 要求 | 備考 |
|---|---|---|
| N-A1 | 電源の入/切・強制停止を宣言で保ち、止まっているべき台が勝手に起きていたら知らせたい | 既存：Redfish の power（U-P）。 |
| N-A2 | 一度だけ CD/PXE から起動して OS を入れ、入ったことをゲストの印で確かめたい | 既存：boot_once・guest-os・template-file |
| N-A3 | BIOS/UEFI の設定（SR-IOV、起動順、セキュアブート）を機種ごとの基準に合わせ、再起動の要る反映は保守の窓で行いたい | 既存：redfish-bios。窓は Policy |
| N-A4 | BMC・BIOS・NIC・RAID の FW を基準の版にそろえたい。更新の順は互換の表に従い、一度に 1 台ずつ、承認を挟みたい | 既存：redfish-firmware・compatibility |
| N-A5 | 管理 API に更新の口が無い装置は、人に手順を頼み、版の観測で閉じたい | 既存：update_policy manual |
| N-A6 | BMC 自身の設定（管理ユーザ、パスワード回転、NTP、SNMP/syslog の宛先、管理 NW のアドレス）を宣言で保ちたい | 新。BMC の設定 Resource（provider） |
| N-A7 | ハードの在庫（型番・シリアル・DIMM・ディスクの健全性・温度）を観測し、宣言には無いが棚卸しと故障の知らせに使いたい | 観測だけの Resource（報告） |
| N-A8 | 部品交換（ディスク・NIC・筐体）の前に退避し、交換後に同じ identity で戻したい。シリアルが変わっても同じ台として扱いたい | identity の選び方・Procedure |
| N-A9 | 退役する台のディスクを消去し、BMC の資格を無効化し、在庫から外したい（壊す操作、承認つき） | Procedure＋Policy 承認 |
| N-A10 | PDU のコンセント、電源キャッピング、ラック位置・ケーブルの対応（宣言と観測の突き合わせ）を扱いたい | provider。LLDP の観測との突き合わせは Condition |
| N-A11 | PXE/iPXE で OS を入れたい（仮想メディアを持たない台）。DHCP の予約・TFTP の設定・応答ファイルを一式そろえたい | Expand で一式を展開・Procedure |
| N-A12 | 入れ直し（reimage）を宣言で起こし、delete と create を別々に壊す変更として数えないでほしい | 既存：planned_change kind reimage |
| N-A13 | GPU/アクセラレータの FW・ドライバ・MIG の分割を宣言で保ちたい | provider |
| N-A14 | 同じ機種の台を 100 台入れるとき、台ごとの差（ホスト名・アドレス・鍵）だけを書き、残りは雛形から展開したい | Expand＋採番 |

### 3.2 B ネットワーク

| ID | 要求 | 備考 |
|---|---|---|
| N-B1 | スイッチのポート・VLAN・LAG・MTU・記述を宣言で保ちたい。手で変えたポートはずれとして知らせたい | 既存：netconf・cli-device |
| N-B2 | 設定を変えたら到達が保たれることを確かめてから確定し、確かめられなければ機器側の時間切れで戻したい | 既存：2 段の Apply |
| N-B3 | 同じ変更を複数台（leaf 群）にまとめて入れ、全部で確かめが通ったときだけ確定したい | 既存：commit_group |
| N-B4 | 経路（BGP/OSPF の peer・route-map）・ACL・QoS を宣言で保ちたい | provider（netconf/cli-device） |
| N-B5 | 機器の設定を定期に取って保存し（バックアップ）、基準（golden config）とのずれを報告だけしたい | 観測だけ＋report |
| N-B6 | 機器の operational の状態（リンク、peer の up、経路の有無）を期待と比べてずれを知らせたい | Backlog #201。Condition で待つのにも使う |
| N-B7 | FW の規則を網の間の許可として抽象に書き、nftables・VM の FW・機器の ACL に展開したい。理由の無い例外は断りたい | 既存：network_policy |
| N-B8 | 一時的な許可（期限つきの例外）を書き、期限が来たら自動で消えてほしい | 新。時間を Condition か Expand の入力に |
| N-B9 | 既定の拒否（base chain の policy）を宣言で管理し、締め出しの守りを通したい | 既存 |
| N-B10 | LB の VIP・プール・ヘルスチェックを宣言で保ち、保守の前にメンバーを drain してから更新したい | provider＋Procedure |
| N-B11 | DNS のゾーンとレコード、SOA のシリアル、DNSSEC の鍵の回転（KSK/ZSK、事前公開→切替→撤去） | 既存：dnszone。回転は Procedure |
| N-B12 | DHCP の予約・範囲・オプションを宣言で保ち、IPAM と整合させたい | provider＋採番 |
| N-B13 | IP アドレス・VLAN ID・ASN・VMID などの採番。範囲を区切る、再利用しない、解放は人が決める | 既存：allocator blocks/reuse/release |
| N-B14 | VPN（WireGuard/IPsec）の peer と鍵を宣言で保ち、鍵の回転は両端を同じ窓で切り替えたい | provider＋Procedure |
| N-B15 | NTP・syslog・SNMP の宛先、AAA（TACACS/RADIUS）の設定を全機器で同じにしたい | Expand で全台に同じ Resource を展開 |
| N-B16 | 経路の MTU が端から端まで同じであること（集合の上の不変条件）を確かめたい | 集合の上の Condition（要検討） |
| N-B17 | 到達性（ping/TCP/HTTP）を観測し、依存の待ちや確かめに使いたい | 既存：confirm の observe/check・metrics-probe |
| N-B18 | 機器の OS の版上げ（イメージの転送→再起動→確認）を 1 台ずつ、隣の台が健全なときだけ進めたい | Procedure＋Condition |
| N-B19 | 管理できない機器（API も CLI の雛形も無い）は、宣言だけ持って人の手順で合わせたい | manual の Procedure・観測不能階級 |

### 3.3 C ストレージ

| ID | 要求 | 備考 |
|---|---|---|
| N-C1 | Ceph のプール・RBD・CRUSH の規則、ZFS のデータセット、LVM、iSCSI/NVMe-oF のターゲットを宣言で保ちたい | provider |
| N-C2 | データの入ったボリューム・バケット・DB は宣言から消えても自動で消さず、承認を要する壊す操作にしたい | 既存：中身のあるバケット/利用者の表のある DB を消さない。不変条件 |
| N-C3 | 消す前に最後のバックアップを取りたい（final_backup） | §12。Procedure を削除の前に |
| N-C4 | スナップショットを定期に取り、古いものを保持の規則で消したい | 定期＝古さの観測（level）。削除は数える |
| N-C5 | バックアップの定期実行と、復元の試験（定期のリハーサル） | 定期の Procedure／level 化 |
| N-C6 | テープ・オフライン媒体は観測できないので、宣言と人の記録だけで持ち、消さない | ledger_only・manual |
| N-C7 | オブジェクトストレージのバケット・versioning・lifecycle・Object Lock・レプリケーション・ユーザと鍵の発行 | 既存：s3-bucket。鍵の発行は秘密の生成 |
| N-C8 | 容量のしきい値（プールの使用率）を見て、作成を止めたり知らせたりしたい | 既存：capacity gate・metrics-probe |
| N-C9 | 共有ストレージの quota・NFS/SMB の export・ACL | provider |
| N-C10 | ディスクの暗号化鍵（LUKS/SED）の登録と回転 | 秘密＋Procedure |
| N-C11 | データの移行（旧→新のプール）を段階で進め、切り替えは承認で | Procedure |
| N-C12 | 作り直す VM のデータディスクを引き継ぎたい（keep・volume の付け替え） | 既存：proxmox-vm keep/volume= |

### 3.4 D 仮想化とコンテナ

| ID | 要求 | 備考 |
|---|---|---|
| N-D1 | VM（CPU・メモリ・ディスク・NIC・cloud-init・pool・FW）を宣言で保ち、再起動の要る変更は窓で | 既存：proxmox-vm |
| N-D2 | テンプレート/イメージの基準（版）をカタログから引き、古い VM を順に入れ直したい | DesiredSource/catalog・Expand・planned_change |
| N-D3 | 置けるノードの集合を宣言し、いまどこにいるかはずれにしない。HA の資源・グループ・親和性 | Backlog #207。比べない欄 |
| N-D4 | ホストの保守の前に VM を退避（live migration）し、終わったら戻したい | Procedure |
| N-D5 | 変更の前にスナップショットを取り、確かめが通らなければ戻したい | Transaction（Prepare=snapshot） |
| N-D6 | クラスタ（Proxmox/vSphere）の設定・ストレージの登録・ネットワークの橋 | provider |
| N-D7 | コンテナを systemd の quadlet で保ち、依って立つファイルを変えたら 1 回だけ入れ替えたい | 既存：systemd-unit restart_on |
| N-D8 | レジストリとイメージの保持（古い tag の削除は壊す操作） | provider＋Policy |
| N-D9 | k8s クラスタの構築（bootstrap）・ノードの参加/離脱・drain・版上げのローリング | Procedure＋Condition。k8s の中は委譲 |
| N-D10 | VM の作り直しのときディスクの keep と volume の付け替え、付け先の無い volume を unmanaged に | 既存 |
| N-D11 | 大量 API（/cluster/resources）で一括に観測したい（1 万規模） | 既存：BulkObserve。Backlog #49 |

### 3.5 E ゲスト OS とソフト

| ID | 要求 | 備考 |
|---|---|---|
| N-E1 | ファイル・ユーザ・鍵・sysctl・パッケージ・サービスを宣言で保ちたい（ssh 越し、sudo は最小） | 既存：host-file・systemd-unit・ansible |
| N-E2 | パッケージの更新と再起動を、グループの中で一度に 1 台ずつ、健全性を見ながら行いたい | ローリング＝Procedure＋Policy 同時数＋Condition |
| N-E3 | 宣言に無い手の変更（設定ファイルを直接編集）を検出して知らせ、必要なら取り込みたい | drift 報告＋import |
| N-E4 | 秘密（鍵・パスワード）を含むファイルを置くが、中身を観測・ログに出したくない | 既存：host-file の hash 観測・SecretRef |
| N-E5 | Ansible の既存の playbook/role を観測（check mode）と適用に使いたい | 既存：ansible provider |
| N-E6 | 設定の反映に再起動やサービスの入れ替えが要るものを、反映の「印」で level に判定したい | 既存：restart_on の印。apply_pending |
| N-E7 | 時刻・ログ転送・監視エージェントなど全台共通の設定を 1 か所に書いて全台に展開したい | Expand |
| N-E8 | 台ごとのホスト名・アドレス・鍵を雛形に差し込んだファイル（応答ファイル・設定）を作りたい | 既存：template-file |
| N-E9 | カーネル/OS の大版上げは入れ直し（reimage）で行いたい | 既存：trigger reimage |

### 3.6 F DB とスキーマ

| ID | 要求 | 備考 |
|---|---|---|
| N-F1 | ロール・DB・権限・拡張・パラメータを宣言で保ちたい。パスワードは比べ方を選びたい | 既存：postgresql |
| N-F2 | マイグレーションを順に当て、失敗したら戻したい。トランザクションに入らない文の扱い | 既存：sqlmigrate strategy task |
| N-F3 | 利用者の表がある DB は宣言から消えても消さない | 既存：不変条件 |
| N-F4 | レプリカの構成と、計画的な切り替え（フェイルオーバーのリハーサル）を承認つきの手順で | Procedure |
| N-F5 | バックアップ/PITR の設定と、復元の試験 | Resource＋定期の Procedure |
| N-F6 | DB の資格の回転：新しい資格を発行→利用側へ配る→古いのを失効、の重なりの窓 | Procedure＋秘密 |
| N-F7 | 観測用のロールに最小の権限を宣言で与えたい | 既存：connect_roles |
| N-F8 | MySQL/Redis など他の DB も同じ形で扱いたい | provider |

### 3.7 G ID と秘密と証明書

| ID | 要求 | 備考 |
|---|---|---|
| N-G1 | LDAP/AD の項目・グループ・属性を宣言で保ち、子のある項目は消さない | 既存：ldap |
| N-G2 | 秘密の値は宣言に書かず、取り出し先（Vault/コマンド/ファイル）を参照で書き、世代で比べたい | 既存：SecretRef・secret_backends |
| N-G3 | 観測用と書き込み用の資格を分け、書かない回は書き込み用の資格を取り出さない | 既存：WithWrite |
| N-G4 | X.509 の証明書を発行し、期限の前に更新し、古いものを失効したい（ACME/内部 CA） | 新。期限＝時間の Condition。発行＝秘密の生成 |
| N-G5 | 鍵の回転を、重なりの期間（両方有効）を置いて切り替えたい | Procedure |
| N-G6 | SSH の CA と署名、ホスト鍵の配布、known_hosts | provider＋秘密 |
| N-G7 | 人向けの短命の資格（JIT）と bastion、機器に個人アカウントを作らない | Backlog #60。コアの外（関守） |
| N-G8 | API トークン・サービスアカウントの発行と権限（RBAC）の宣言 | provider。発行は秘密の生成 |
| N-G9 | 秘密をコアが生成して保存先に置く（乱数のパスワード、鍵対）必要がある。宣言には参照だけ | 新：秘密の生成の口 |
| N-G10 | 失効（revoke）は壊す操作として数え、承認を要したい | Policy |

### 3.8 H 監視と通知

| ID | 要求 | 備考 |
|---|---|---|
| N-H1 | 監視の設定（scrape の対象・alert ルール・blackbox）を Resource として宣言から作りたい | Backlog #53。host-file で配れる |
| N-H2 | ずれ・halt・unknown を Alertmanager/webhook に level（firing/resolved）で知らせたい | 既存：event sink・level 報告 |
| N-H3 | 保守の窓の間は監視の通知を沈黙（silence）させたい | 新：Policy の窓→sink/Resource |
| N-H4 | ツール自身の健全性（周の時間・観測できない件数・予算の残り）を metrics で見たい | 既存：/metrics |
| N-H5 | 周・観測・Apply の span をトレースに出したい | 既存：OTLP |
| N-H6 | 「在る≠動いている」の health を判定材料に、ロールアウトの次の段へ進む/止める | Backlog #55/#56。Condition on health |
| N-H7 | 常設の canary（作って・確かめて・消す）で削除の経路の健全性を定期に確かめたい | Backlog #55。定期の Procedure＋TTL |

### 3.9 I ブラウンフィールド

| ID | 要求 | 備考 |
|---|---|---|
| N-I1 | 既にある現実を intent として書き出し（import）、まず観測だけで信頼を得てから直す→消すへ段階的に進みたい | 既存：import・Sync Mode・alert-only |
| N-I2 | ある型は欄の一部だけを管理し、残りは他の道具や人に任せたい | 既存：宣言した欄だけ比べる |
| N-I3 | 宣言外のものを消さずに棚卸し（unmanaged）として出したい。prune しない型でも | 既存：#164 |
| N-I4 | 外のオーケストレータが持つ範囲は触らず、観測して待つだけにしたい | 既存：external_owners |
| N-I5 | 現実を観測して generated/ に書き git にコミットし、人がレビューして intent に取り込む（Sync Mode） | 既存 |
| N-I6 | 観測できない資産（紙の台帳、古い機器）を宣言だけ持ち、消さない | ledger_only |
| N-I7 | 他の道具（Terraform の state、Ansible の inventory）から intent を変換して取り込みたい | 変換の道具（Expand の外） |

### 3.10 J 日々の運用

| ID | 要求 | 備考 |
|---|---|---|
| N-J1 | 直してよい時間帯を範囲ごとに決め、外では報告だけにしたい。凍結は全部止める | 既存：remediation schedule・freeze |
| N-J2 | 大量の正当な作業（退役 50 台、全 VM 入れ直し）を宣言し、急減の halt と予算を一時的に広げたい | 既存：planned_change |
| N-J3 | ローリング更新：一度に N 台、各台の後に健全性を確かめ、悪ければ止める。canary → broad の段階 | Procedure＋Policy＋Condition（#56） |
| N-J4 | 退役：drain→削除→採番の解放→台帳から除く、の順を承認つきで | Procedure＋I3/I12 |
| N-J5 | 容量：使用率の観測・しきい値で作成を止める・報告 | 既存：capacity gate |
| N-J6 | コスト：資源の数と大きさをタグで集計し報告だけしたい（ハイブリッドのクラウド側） | 観測だけ・sink |
| N-J7 | 変更には理由（チケット番号）を付け、監査と通知に出したい | reason/tags（network_policy は既存） |
| N-J8 | 全体を止める更新は、止める前の知らせ・影響の見積もり・承認・時間帯を要したい | 既存：disruption |
| N-J9 | 変更の前に「何が変わるか」を機械で読める形で出し、CI で確かめたい | 既存：plan --json・expect-revision |
| N-J10 | 定期の仕事（証明書の更新、バックアップ、レポート）を level で書き、cron を持ちたくない | 新：古さの観測 |
| N-J11 | 依存先の変更の影響（波及）を事前に見たい | cause D・graph |
| N-J12 | 直したのに効かない（Apply の応答は成功だが現実が変わらない）とき繰り返さず止めてほしい | 既存：oscillating・終了コード 4 |

### 3.11 K 障害と復旧・DR

| ID | 要求 | 備考 |
|---|---|---|
| N-K1 | 観測できない（届かない・5xx・遅い）ときは何もしないで、戻ったら直してほしい。消さない | 既存：unknown≠absent（不変条件） |
| N-K2 | List が急に減ったら prune を止めてほしい（分断・誤設定の疑い） | 既存：急減 halt |
| N-K3 | 拠点が中央（git・Vault・fencing）から切れても、維持は続けたい。変化（新規・削除）は止めてよい | 原則 8。既存：衛星停止影響表 |
| N-K4 | 被災拠点で DR の intent を事前に置き、緊急の書き込み（break-glass）を署名して後で合流したい | 既存：DR モード |
| N-K5 | ツール自身が死んで再起動しても、途中の手順（Procedure）を続きから再開したい。観測の状態は要らない | Record（進行中の手順） |
| N-K6 | 全損した拠点を intent から作り直したい（再イメージ→pull→復旧） | S2-free の考え。Procedure |
| N-K7 | 分断時に二重のデーモンが同じ範囲を直さないようにしたい | fencing（削除候補）。静的分割で代替 |
| N-K8 | 復元のリハーサルをツインで定期に行いたい | ツイン＋Procedure |
| N-K9 | 片側だけ確定した（commit_group の途中で切れた）台を unknown として知らせ、人が決めたい | 既存：#199 の残る窓 |

### 3.12 L 監査と規制

| ID | 要求 | 備考 |
|---|---|---|
| N-L1 | 誰が・何を・なぜ・いつ変えたか（Apply・prune・承認・freeze・Workflow の手）を改ざんしにくい鎖で残し、外へ即時に送りたい | 既存：S4 監査・syslog |
| N-L2 | N-of-M の承認、申請者≠承認者、役割の制約、cooling-off | 既存 |
| N-L3 | 拒否した承認・期限切れの承認・cancel も記録したい | Backlog #109 |
| N-L4 | 読むだけの監査者の口（TLS・トークン・クライアント証明書） | 既存：read-api |
| N-L5 | 秘密を監査・Event・status に出さない | 既存：自動マスク |
| N-L6 | 変更の窓の外で変えていないことの証跡、ずれを検出した証跡（適合レポート） | 既存：status・level |
| N-L7 | 版の保持（WORM/外部の長期保管）とローテーション | 既存：新しいファイルへ回す |
| N-L8 | 「この規則は誰が承認した例外か」を規則に刻みたい | 既存：network_policy の reason と comment |

### 3.13 M 複数人・複数チーム

| ID | 要求 | 備考 |
|---|---|---|
| N-M1 | 基盤チーム（PR 必須・厳格）とテナント（即時・緩い）で書き込みの経路と承認を変えたい | 既存：ownership 2 層 |
| N-M2 | 範囲ごとに承認者・mode・予算を変えたい | Policy の範囲 |
| N-M3 | 同じ ID を 2 つの intent が宣言したら読み込みで断りたい | loader |
| N-M4 | 承認を人がネットワーク越しに出したい（CLI・API・git の PR） | 承認の口（迷う所） |
| N-M5 | 通知の宛先を範囲の持ち主ごとに変えたい | sink のルーティング（Event に range/owner の属性） |
| N-M6 | テナントのセルフサービス（カタログ→合成→採番→収束）を即時に受けたい | Backlog #57/#64 |
| N-M7 | 複数の常駐の役（観測だけの役、進める役）を同じ intent で動かしたい | 既存：roles・WorkflowAdvancedElsewhere |

### 3.14 N 規模

| ID | 要求 | 備考 |
|---|---|---|
| N-N1 | 1 万件のリソースを単一デーモンで回すため、一括の観測と間引きを使いたい | BulkObserve・tier（削減候補） |
| N-N2 | 拠点ごと・層ごとにデーモンを分け、互いに問い合わせず現実を見て協調したい | 静的分割（intent のディレクトリ）＋Condition |
| N-N3 | 分割の境界（claim）を intent で宣言し、開店/閉店を PR で | claim-as-intent（削減候補） |
| N-N4 | 観測の並列度・API の rate limit を守りたい | Policy（provider ごとの同時数） |
| N-N5 | 周の時間が延びたときに知りたい | /metrics |

### 3.15 O ブートストラップ

| ID | 要求 | 備考 |
|---|---|---|
| N-O1 | 段0（Proxmox 3 台＋Ceph＋VM-HA）は Ansible で組み、Sakutei はその上で動く | 外 |
| N-O2 | 1〜2 台の最小構成（file の秘密、常駐なし）から始めたい | 段0-minimal/zero |
| N-O3 | 自分自身（Sakutei の動く VM・ユニット・設定）を自分で管理したい。自分を消さない | 鶏と卵。external_owner/除外 |
| N-O4 | 作業者の PC から一回性に収束し、常駐を置かない | 既存：reconcile 一回性 |

### 3.16 P テストとツイン

| ID | 要求 | 備考 |
|---|---|---|
| N-P1 | 偽 provider でコアの性質（unknown で消さない・予算を超えない・承認なしで壊さない）を確かめたい | 既存：fakeprovider・chaos |
| N-P2 | 本番の intent を外部入力だけ差し替えてツインに流したい（本番のプール・秘密に触れない） | twin（Expand 入力の差し替え） |
| N-P3 | provider の版上げの幽霊 diff を本番の前に見たい | 既存：ProviderUpgradeDiff |
| N-P4 | 統合テストを本物の装置の模擬（qemu-bmc・netopeer2・Proxmox の入れ子）で回したい | provider の側 |
| N-P5 | Expand の結果を golden で確かめたい | Expand が純粋なら容易 |

### 3.17 Q 緊急時の手作業との共存

| ID | 要求 | 備考 |
|---|---|---|
| N-Q1 | 障害対応中は全部止め（freeze）、誰が・なぜ・いつまでを残し、期限で解けてほしい | 既存 |
| N-Q2 | 手で変えたものをツールが戻してしまわないよう、範囲を一時的に報告だけにしたい | Policy mode report（一時） |
| N-Q3 | 手の変更を正にしたいときは intent に取り込む道（import/sync） | 既存 |
| N-Q4 | 人の作業を手順の一段として記録し、観測で閉じたい | 既存：manual |
| N-Q5 | 人とツールが取り合わない（往復しない）ように、効かない Apply の上限と外の持ち主 | 既存 |
| N-Q6 | 緊急の書き込みを署名して記録し、後で正規の経路に合流 | 既存：break-glass |

### 3.18 R 時間で動くもの（横串）

| ID | 要求 | 備考 |
|---|---|---|
| N-R1 | 期限（証明書・例外の許可・freeze・planned_change の窓）が来たら自動で振る舞いが変わる | 時間を誰が見るか（迷う所） |
| N-R2 | 古さ（最後のバックアップ・最後の観測）がしきい値を超えたら動く／知らせる | 観測した値＋現在時刻 |
| N-R3 | 窓（直してよい時間帯・止めてよい時間帯） | Policy |
| N-R4 | 待ち（再起動後 10 分は触らない、cooling-off、Transaction の時間切れ） | Procedure/Transaction の timeout |

### 3.19 S ツール自身

| ID | 要求 | 備考 |
|---|---|---|
| N-S1 | 版を上げた直後は報告だけで 1 周止まり、差分が版の違いだけかを確かめたい | 既存：ProviderUpgradeDiff |
| N-S2 | 設定の置き場を 2 つ（intent・つなぎ方）にし、旗はファイルの場所と一回性だけにしたい | たたき台 |
| N-S3 | デーモンの HA（止まったら別の台で続きから） | Record の排他（迷う所） |
| N-S4 | 外部依存（Vault・git・Alertmanager）が止まっても維持は続く | 停止影響表 |
| N-S5 | systemd で動かし、ログ・ロック・store の置き場を決めたい | production.md |

### 3.20 T 外の道具との共存

| ID | 要求 | 備考 |
|---|---|---|
| N-T1 | Terraform が持つ範囲と Sakutei が持つ範囲を分け、互いの管理外を消さない | external_owners・prune の範囲 |
| N-T2 | k8s operator/VNFM/Heat が持つ scope は観測して待つだけ | 既存 |
| N-T3 | Ansible の playbook を手順の一段として使う | 既存：ansible provider |
| N-T4 | 監視系（Prometheus）の判定を借り、自前の prober を持たない | 既存：metrics-probe |
| N-T5 | 外の採番権威（NetBox）を使いたい | AllocationAuthority の別実装（Backlog #36） |
## 4. 8 部品で書けるかの検証

「書き方」の列は §1.4 の型（I1〜I20）を名指しし、足りない欄や口は太字で書く。「外」の列はコアの外で満たす本体（provider・Expand・sink・道具・運用）。判定の集計は §4.4。

### 4.1 これまでのユースケース（E-）

#### A 物理

| ID | 8 部品での書き方 | 判定 | コアの外 |
|---|---|---|---|
| E-A1 | I1。`power`・`boot_enabled` は Resource の欄。強制停止は Schema で「壊す」印の欄（`power_off: graceful_then_force` を書いたときだけ Apply が強制する）。電源の切り替え中は Observe が `converging` を返し、コアは重ねない | ◎ | provider（Redfish） |
| E-A2 | I6 の `invoke`（`op: boot_once`）。Procedure の run（Record）に「送りかけ」の印を書いてから送り、再開時に印があれば送らず halt。**Provider に一回きりの操作の口 `Invoke` が要る**（たたき台の 4＋Transaction には無い） | ○ | provider |
| E-A3 | I6 の `apply` と `require`（`observe(virtual-media/{t}/cd).inserted == true`、timeout） | ◎ | provider |
| E-A4 | I1。保留中は Observe が `converging`（または欄 `pending: true` を Condition で読む）。**Observe の答えに `converging` を残す**（たたき台は present/absent/unknown の 3 つ） | ○ | provider |
| E-A5 | I6。相＝段、相の入口の「済んでいるか」＝段の `skip_if: Condition`、危険区間＝段の `critical: true`（再開時は gate に回す）、承認の要る相＝段の `gate`、再起動の待ち＝`wait`（unknown は待つ）。**段の属性に `skip_if`・`critical` が要る** | ○ | provider（Redfish の UpdateService の癖） |
| E-A6 | I7（`manual` ＋ `done_when` ＋ timeout）。trigger は `on_drift`（版がずれたら 1 回） | ◎ | — |
| E-A7 | Expand が互換の表を読み、`depends_on` と各辺の `when: observe(X).version in [...]` に変換する。「望む組み合わせが表に無い」は Expand の診断（読み込みで断る）。「途中の組み合わせ」は辺の Condition で止まる。**表の検査を Expand が担い、コアは表を知らない** | ○ | Expand |
| E-A8 | I6（apply→require→invoke→invoke→wait→apply→require）。応答ファイルは `template-file` の Resource を Procedure の `precondition` に | ◎ | provider ×3 |
| E-A9 | I11 の一時的な Policy（`allow: {kind: reimage, max_count}`）＋ Procedure の `trigger: {policy: <name>}`（レーンが開いたときだけ 1 回、対象ごとに枠を消費）＋ `done_when`。**Procedure の trigger が Policy を名指しする結び付きが要る** | ○ | — |
| E-A10 | Resource は宣言した欄だけ比べるので managed/computed の宣言は要らない。**identity（この欄が変われば作り直し）は Schema に要る**（たたき台の Schema には無い） | ○ | provider |
| E-A11 | I13（Expand のテンプレート）。内容ハッシュの参照は欄の値の決まり（provider の正規形） | ◎ | Expand・provider |
| E-A12 | I1（unknown は何もしない。Apply の途中の故障は provider が `converging` を返す） | ◎ | provider |

#### B ネットワーク

| ID | 8 部品での書き方 | 判定 | コアの外 |
|---|---|---|---|
| E-B1 | I1。SOA の bump は provider の Apply の副作用（書き換えたときだけ） | ◎ | provider |
| E-B2 | I1 ＋ I4（`ranges` の `tables`） | ◎ | provider |
| E-B3 | I8。`Prepare`＝写しと時間切れの仕掛け、`confirm`＝Condition（`reach(ssh://…)`・`observe:`・`check:`）、`Commit`＝仕掛けの取り消し、`Rollback`＝時間切れに任せる。作成・policy 変更の承認＝Schema の欄の印（`policy`・`hook` 等は「作る・変えると壊す」）→ Policy の承認レーン。**「変える前にも確かめる」と「変えたあとだけ確かめる（after:）」の区別を Condition の列に `phase: before|after` として持つ** | ○ | provider・guard tool |
| E-B4 | I4 の `external_owners`（核の chain）＋ `before: [jump mgmt]` は Expand か宣言 | ◎ | 運用 |
| E-B5 | I13（Expand が `network_policy:` を Resource に展開し、重なり・理由の無い例外は診断で断る。影・未到達は知らせの診断） | ◎ | Expand |
| E-B6 | I1 ＋ List が印の無い規則を 1 つずつ ID で返して report（prune: false の範囲）。規則の変更は Schema の「壊す」印 | ◎ | provider |
| E-B7 | I2（観測だけの provider）。宣言した属性だけ比べる | ◎ | provider |
| E-B8 | I8（`Prepare`＝commit confirmed、`Commit`＝確定、確かめは `reach(ssh://leaf)`） | ◎ | provider |
| E-B9 | I9（`group`）＋ Condition の `observe(leaf02/…).routes contains …`・`check:name`。**Transaction を複数 Resource でまとめて確定する口（group）をコアが持つ** | ◎ | provider |
| E-B10 | provider のつなぎ方の設定（NACM に合う rpc だけ） | 外 | provider |
| E-B11 | I1 ＋ `invoke`（定義の actions）。Apply の中の commit/rollback は provider の Transaction（任意） | ◎ | provider |
| E-B12 | Schema の `ordered`（list は全置換、set は順不同）。**Schema に型の意味（set/list）が要る** | ○ | provider |
| E-B13 | Condition（`reach(…)`・`observe(…)`・`check:`） | ◎ | — |
| E-B14 | I18 ＋ 宣言した欄だけ ＋ `converging` | ◎ | provider |

#### C ストレージ

| ID | 8 部品での書き方 | 判定 | コアの外 |
|---|---|---|---|
| E-C1 | I1。Object Lock の変更は Schema の「壊す」印 → Policy の承認。持たない設定は宣言に書かなければ比べない | ◎ | provider |
| E-C2 | I1 ＋ `volume=` の付け替えは「状況で壊す変更」。**Schema の静的な印では言えず、provider が Plan のときに「この op は壊す・承認が要る」と言う口（`Plan(observed, desired) → ops`）が要る** | ○ | provider |
| E-C3 | Policy（`prune: false` 既定・absent のレーン）＋ Procedure `trigger: {on: before_delete, select}` で final_backup。**削除の前に手順を挟む trigger が要る** | ○ | provider |
| E-C4 | I14（`depends_on` に Condition、`on_unknown: block`） | ◎ | provider（metrics-probe） |

#### D 仮想化とコンテナ

| ID | 8 部品での書き方 | 判定 | コアの外 |
|---|---|---|---|
| E-D1 | I1。再起動を待つ変更は `invoke: reboot` を Procedure で、または provider の Apply の中。非同期の仕事は `converging` | ◎ | provider |
| E-D2 | I4 の `external_owners`。provider 設定の除外は持たない（宣言が唯一の正なので食い違いの規則が消える） | ◎ | — |
| E-D3 | I3 | ◎ | provider |
| E-D4 | I4（`prune: false` でも List して report）。List できない provider は「検出できない」と report | ◎ | provider |
| E-D5 | I1（印はホストの側に置く） | ◎ | provider |
| E-D6 | コアのループの決まり（Apply は非同期、同じ Resource に 1 つ、Apply を出した下流は同じ周に出さない、in-flight を予算に数える） | ◎ | — |
| E-D7 | Resource は宣言した欄だけ比べる | ◎ | — |
| E-D8 | **Observe をまとめて呼ぶ形（`Observe(ids...)`）** にすれば一括 API を provider が使える。prune の根拠は List の ID 集合だけ（型で分ける） | ○ | provider |

#### E ゲスト OS とソフト

| ID | 8 部品での書き方 | 判定 | コアの外 |
|---|---|---|---|
| E-E1 | provider SDK の下回り（ssh の実行の層・鍵・sudo・guard tool） | 外 | SDK |
| E-E2 | I1（hash と stat だけ観測） | ◎ | provider |
| E-E3 | **宣言を見ないと観測できない縮退（Ansible の check mode）**：Observe が `desired` を受け取る任意の口（`ObserveWith(id, desired)`）。原因・prune の保証は落ちると宣言する | ○ | provider |
| E-E4 | I2（観測だけ） | ◎ | provider |
| E-E5 | I1（雛形は provider の欄か Expand） | ◎ | provider |
| E-E6 | unknown | ◎ | provider |

#### F DB とスキーマ

| ID | 8 部品での書き方 | 判定 | コアの外 |
|---|---|---|---|
| E-F1 | I1。消さない決まりは provider の `Delete` が断る | ◎ | provider |
| E-F2 | I6（`trigger: on_drift`、`invoke: migrate`、失敗時の `rollback` 段、Policy の窓で Procedure の起動を止める） | ◎ | provider |
| E-F3 | I1（欄 `connect_roles`、足すだけは provider の Apply の決まり） | ◎ | provider |

#### G ID と秘密と証明書

| ID | 8 部品での書き方 | 判定 | コアの外 |
|---|---|---|---|
| E-G1 | I1 ＋ I4（`base_dn`・`filter` は range） | ◎ | provider |
| E-G2 | I17。**Resource の欄の型に `secret` が要り、コアは値を比べず世代（backend が返す）だけを Expand の入力にする**。provider は Apply の直前に SDK の口で取り出す | ○ | SDK |
| E-G3 | SDK の口 `SecretBackend{Generation, Resolve}` と登録 | 外 | SDK |
| E-G4 | 世代が読めない → Expand の診断でそのリソースを `unknown` 扱い（触らない・下流は待つ） | ◎ | Expand |
| E-G5 | コアは Apply を呼ぶ回にだけ provider を「書く文脈」で組み立てる（mode: report・plan・status では呼ばない）。provider は書き込み用の資格をその中でだけ取り出す | ◎ | provider |
| E-G6 | 道具（store の抜き出し） | 外 | 道具 |
| E-G7 | 運用（オフライン保管）＋ Events（`BreakGlassUsed` は外の道具が Event を入れる口）。**外から Event を入れる口（sink の逆）は任意** | 外 | 運用 |
| E-G8 | I15（`vault/secret` の Resource に `max_age`、provider が鮮度を観測し古ければ回す Procedure を `on_drift` で起こす） | ◎ | provider |

#### H 監視と通知

| ID | 8 部品での書き方 | 判定 | コアの外 |
|---|---|---|---|
| E-H1 | Events（level：キー＝kind＋resource、firing/resolved/count/since、確かめられなかった周は firing のまま） | ◎ | — |
| E-H2 | sink の実装（送り直し・複数宛先・SecretRef ヘッダ） | 外 | sink |
| E-H3 | sink の実装（metrics・OTLP）。コアは周の統計と span の切れ目を Events に出す | 外 | sink |
| E-H4 | Condition（`observe(metrics-probe/x).value < 1000`。比べ方はコアの式に。provider は値を返すだけ） | ◎ | provider |
| E-H5 | 読み取り HTTP（status/plan/graph/events）。TLS と認可は口の包み | ◎ | listener |
| E-H6 | Procedure の `notify_before` と Events | ◎ | — |
| E-H7 | I1（監視設定のファイルを host-file で）か Expand | ◎ | provider・Expand |

#### I ブラウンフィールド

| ID | 8 部品での書き方 | 判定 | コアの外 |
|---|---|---|---|
| E-I1 | 道具（Provider の List/Observe と Schema から intent を書く逆 Expand） | 外 | 道具 |
| E-I2 | 道具（同じ逆 Expand を周期で回して git にコミット）＋ Policy `mode: report` | 外 | 道具 |
| E-I3 | I2（範囲ごとに `mode: report`、`prune: false` 既定） | ◎ | — |
| E-I4 | **観測できない資産**：provider `ledger`（Observe は常に unknown、List 無し）に載せれば prune も Apply もされない。`last_verified` の古さは Condition（`now() - observe(ledger/x).verified_at > 90d`）で知らせ。**時計を Condition で読めること** | ○ | provider |
| E-I5 | I18 ＋ Condition で待つ | ◎ | — |
| E-I6 | I4（範囲は intent にだけ書く。provider の設定はつなぎ方だけ。置き場が 1 つなので食い違いの規則が要らない） | ◎ | — |
| E-I7 | provider の正規形と読み込みの検証（Validate の口） | 外 | provider |

#### J 日々の運用

| ID | 8 部品での書き方 | 判定 | コアの外 |
|---|---|---|---|
| E-J1 | Policy の budget（周ごと・壊す操作・窓ごと、in-flight を数える、超えたら halt-all） | ◎ | — |
| E-J2 | I11（一時的な Policy の `allow`。別枠でバイパスでない・枠は発射直前に消費・失敗で戻さない・使用量と時計は Record） | ◎ | — |
| E-J3 | I10（windows） | ◎ | — |
| E-J4 | I10 の一時的な Policy（CLI が Record に書く。理由必須・期限で消える・読めなければ freeze 扱い） | ◎ | — |
| E-J5 | Policy の合成の決まり（most-restrictive-wins） | ◎ | — |
| E-J6 | ループの決まり（Apply の後に読み直す。settle は Schema の hint）＋ Policy の `max_ineffective_applies`（止める）＋ Record（一回性は回をまたいで数える） | ◎ | — |
| E-J7 | ループの決まり（依存先が効いたら依存元を同じ回で再観測） | ◎ | — |
| E-J8 | Procedure の `disruption:`（window・upstream の Condition と max_wait・gate・notify_before・estimate）。Policy の窓と most-restrictive | ○ | — |
| E-J9 | plan は Policy の判定を何も消費せずに評価する（コアの性質）。`--expect-revision` は Expand の出力のハッシュ | ◎ | — |
| E-J10 | プロファイルは Policy の承認の既定の束（Expand の preset か文書）。cooling-off は `approval.cooling_off`（時計は Record） | ◎ | Expand |
| E-J11 | `runtime:` は Policy に畳む。旗は「ファイルの場所と一回性」だけで、締める旗（`--mode report`・`--freeze`）は一時的な Policy として Record に書く。役は Policy の集合に名前（`--policy-set`）。緩める向きの旗は存在しない | ◎ | — |
| E-J12 | **Record に「前回見た provider の版」を置き**、版が変わった周は Policy が report に落とす（`ProviderUpgradeDiff`）。原因タグ無しで満たせる | ○ | — |
| E-J13 | 原因 3 種に縮める：intent が変わった（Expand の出力のハッシュが前周と違う）／外の入力が変わった（Expand の入力のハッシュ）／それ以外は drift。前周のハッシュはメモリ（一回性は Record）。「前周は X だった」はメモリの環（永続化しない） | ○ | — |
| E-J14 | CLI | ◎ | — |

#### K 障害と復旧・DR

| ID | 8 部品での書き方 | 判定 | コアの外 |
|---|---|---|---|
| E-K1 | Observe の三値と「unknown では何もしない」（コアの不変条件） | ◎ | — |
| E-K2 | I4（List が complete でない・前周比で急減した範囲は prune しない。自分が消した ID と一時的な Policy の下で消した ID は差し引く。前周の件数はメモリ、一回性は Record） | ◎ | — |
| E-K3 | Expand の入力（inventory）が届かない → その入力に依る Resource を `unknown` 扱い（Expand の診断）。「届いたが縮んだ」は**前周の件数**が要る（メモリ／Record）。空の desired を作らない決まりはコアに残す | ○ | Expand |
| E-K4 | 採番の正は intent のファイル（Record は写し）なので衛星なし。秘密は unknown に縮退 | ◎ | — |
| E-K5 | Record の run（段・印・承認）＋段の `critical` | ◎ | — |
| E-K6 | コアはローカルのファイルを読むだけ。ミラーは外の道具 | 外 | 道具 |
| E-K7 | DR 拠点の常駐は別の Policy の集合で起動（`--policy-set dr`）。DR の intent のプリステージは Expand の入力（`site_role`）。break-glass の署名と合流は git の道具。コアに要るのは監査に「intent の版と署名者」を残すことだけ | ○ | 道具・運用 |
| E-K8 | 担当の分割は intent（範囲）で重ならないように宣言し、重なれば読み込みで断る。引き継ぎの窓の二重書きは **Record に範囲ごとの世代を置く小さな口**（任意）で防げるが、たたき台では落とす | ○ | 運用 |
| E-K9 | Record の置き場を揮発にし、Procedure は中央の常駐だけが持つ（範囲で分ける） | ◎ | — |
| E-K10 | Policy の `max_ineffective_applies`（I6 の収束版） | ◎ | — |
| E-K11 | Record の健全性（周・誤り・時刻）→ 読み取り HTTP と status | ◎ | — |

#### L 監査と規制

| ID | 8 部品での書き方 | 判定 | コアの外 |
|---|---|---|---|
| E-L1 | Record の監査（鎖・回し・値を書かない）。syslog は sink | ◎ | sink |
| E-L2 | Policy の approval（quorum・roles・separation・cooling_off）＋ Procedure の gate（同じ型） | ◎ | — |
| E-L3 | 承認は CLI が Record に書く（認可は OS のファイル権限）。HTTP の承認の口は薄い外の道具 | ◎ | 道具 |
| E-L4 | 読み取り HTTP／CLI の status と plan（文と JSON は同じ構造から） | ◎ | — |
| E-L5 | `--expect-revision`（Expand の出力のハッシュ） | ◎ | — |
| E-L6 | Record の監査に拒否・期限切れ・cancel も書く（決めるだけ） | ◎ | — |

#### M 複数人・複数チーム

| ID | 8 部品での書き方 | 判定 | コアの外 |
|---|---|---|---|
| E-M1 | Resource の `labels: {owner: platform}` と Policy の select。PR 必須は git の運用 | ◎ | 運用 |
| E-M2 | Policy の select（範囲・type・labels）ごとに mode・承認・予算 | ◎ | — |
| E-M3 | Expand の出力の検証（同じ ID は断る・未知の欄・循環）。コアの読み込みで | ◎ | — |
| E-M4 | CLI（Record に書く）。**ネットワーク越しの承認は読み取り HTTP の外に書き込みの口が要る**（薄い道具で Record に書くか、Record を git に置いて PR で承認する） | ○ | 道具 |
| E-M5 | Policy の集合に名前（`--policy-set`）、Procedure を進めるのは Record のロックを取った常駐だけ、進めない常駐は `ProcedureAdvancedElsewhere`（level） | ◎ | — |
| E-M6 | Expand の入力の 1 つとして DB の supply を足す口（DesiredSource）。即時性は wake の口（E-N5） | ○ | 道具 |

#### N 規模

| ID | 8 部品での書き方 | 判定 | コアの外 |
|---|---|---|---|
| E-N1 | 文書（天井の算術）。コアは Observe の同時数を Policy に持つ（`max_concurrent_observe`） | 外 | 文書 |
| E-N2 | Policy に `observe_every`（select ごと。初回・未収束・desired 変化の周は必ず観測）。任意の欄 | ○ | — |
| E-N3 | 常駐を範囲（intent のディレクトリ・ranges）で分けて複数起動。相手の作った現実は Condition で待つ。claim の宣言は「どの intent を読むか」の起動の引数 | ◎ | 運用 |
| E-N4 | git の道具 | 外 | 道具 |
| E-N5 | **起床の口**（`sakutei wake`／SIGUSR1／inotify／HTTP）で次の周を今すぐ始める。影響サブ DAG だけの収束は持たない（全部回す。規模が要るなら Policy の observe_every で削る） | ○ | — |

#### O ブートストラップ

| ID | 8 部品での書き方 | 判定 | コアの外 |
|---|---|---|---|
| E-O1 | 運用（Ansible）。自分の土台は `external_owners` | 外 | 運用 |
| E-O2 | file の secret backend ＋ Policy の既定（solo の束） | ◎ | SDK |
| E-O3 | 一回性の `reconcile`（同じ Record の置き場を使う。cooling-off は「次の回に経っていれば」） | ◎ | — |
| E-O4 | SDK（`cli.Main(WithProvider…)`、つなぎ方の設定ファイル 1 つ） | ◎ | SDK |

#### P テストとツイン

| ID | 8 部品での書き方 | 判定 | コアの外 |
|---|---|---|---|
| E-P1 | 偽の provider と性質のテスト（どんな観測の並びでも unknown で消さない・予算を超えない・承認なしで壊さない・List 欠損で消さない）＋故障の注入（provider の包み・時計の差し替え） | ◎ | — |
| E-P2 | SDK の契約テスト（冪等・absent→Create→Observe・List 完全・unreachable≠absent・Schema の lint） | ◎ | SDK |
| E-P3 | provider の側の統合テスト | 外 | provider |
| E-P4 | I20（Expand の入力を引数で差し替える。本番のプール・秘密に触れない） | ◎ | — |

#### Q 緊急時の手作業との共存

| ID | 8 部品での書き方 | 判定 | コアの外 |
|---|---|---|---|
| E-Q1 | I2・I10（一時的な Policy） | ◎ | — |
| E-Q2 | 道具（import／sync） | 外 | 道具 |
| E-Q3 | I7 ＋ gate ＋ Procedure の段で範囲を一時的に `report` にする（`policy: {select, mode: report}` を段として置く。手順が終われば戻す） | ○ | — |
| E-Q4 | Policy の `max_ineffective_applies` ＋ `external_owners` | ◎ | — |

#### R 時間で動くもの

| ID | 8 部品での書き方 | 判定 | コアの外 |
|---|---|---|---|
| E-R1 | Policy の windows（Resource と Procedure の両方に効く） | ◎ | — |
| E-R2 | Policy の `until`・Procedure の `timeout`・gate の `timeout`・approval の `cooling_off`（時計は Record） | ◎ | — |
| E-R3 | Transaction の `timeout`、Schema の `settle` hint、backoff はループの決まり | ◎ | provider |
| E-R4 | コアの Clock を差し替えの口にする（テストと試用） | ◎ | — |

#### S ツール自身

| ID | 8 部品での書き方 | 判定 | コアの外 |
|---|---|---|---|
| E-S1 | 置き場を 2 つ（intent・つなぎ方の設定 1 つ）＋ その場の操作は Record（一時的な Policy）。食い違いの規則は most-restrictive 1 つ | ◎ | — |
| E-S2 | Record を 1 つのディレクトリ（flock）に | ◎ | — |
| E-S3 | 文書 | 外 | 文書 |
| E-S4 | モジュール構成 | 外 | — |
| E-S5 | `--policy-set <name>`（宣言に無ければ断る） | ◎ | — |

#### T 外の道具との共存

| ID | 8 部品での書き方 | 判定 | コアの外 |
|---|---|---|---|
| E-T1 | I18 ＋ ansible provider ＋ 道具 | ◎ | provider・道具 |
| E-T2 | I18 ＋ ranges（prune の範囲を明示の ID だけに） | ◎ | — |
| E-T3 | sink ＋ metrics-probe ＋ I1（監視設定） | ◎ | sink・provider |
| E-T4 | Record の採番に「空きを選ぶ」口（`Allocator` の差し替え）。既定はファイル | ○ | 道具 |

### 4.2 他に考えられるユースケース（N-）

| ID | 8 部品での書き方 | 判定 | コアの外 |
|---|---|---|---|
| N-A1 | I1（E-A1） | ◎ | provider |
| N-A2 | I6（E-A8） | ◎ | provider |
| N-A3 | I1 ＋ Policy の windows（再起動を伴う反映は `invoke: reset` を Procedure で窓の中に） | ◎ | provider |
| N-A4 | I6 ＋ I16（順序は Expand が互換の表から `depends_on` に、同時数は Procedure の `concurrency`、承認は gate） | ○ | provider・Expand |
| N-A5 | I7 | ◎ | — |
| N-A6 | I1（BMC の設定の type：ユーザ・NTP・syslog・管理 IP）。パスワードの回転は I17 ＋ `on_drift` の Procedure | ◎ | provider |
| N-A7 | I2（在庫の type は観測だけ。宣言には無いが `ranges` で List して report に出す） | ◎ | provider |
| N-A8 | Schema の identity を物理の座標にし、シリアルは宣言に書かない（比べない）。退避・復帰は Procedure | ◎ | provider |
| N-A9 | I6（wipe→証明の取得→BMC の資格の失効→`ensure: absent`）＋ Policy の承認（gate と absent のレーン） | ◎ | provider |
| N-A10 | I1（PDU・電源キャッピングの provider）。配線の宣言と LLDP の観測の突き合わせは Condition（`observe(sw/port).neighbor == "..."`）で drift を知らせ | ◎ | provider |
| N-A11 | Expand が台ごとに DHCP の予約・TFTP の設定・応答ファイルの Resource を展開し、Procedure が `invoke: pxe_boot`→`wait`→`require` | ◎ | provider・Expand |
| N-A12 | I11 ＋ E-A9 | ○ | — |
| N-A13 | I1（GPU の FW・MIG の provider） | ◎ | provider |
| N-A14 | I13 ＋ I12 | ◎ | Expand |
| N-B1 | I1（netconf／cli-device） | ◎ | provider |
| N-B2 | I8 | ◎ | provider |
| N-B3 | I9 | ◎ | provider |
| N-B4 | I1（部分木の type ごと） | ◎ | provider |
| N-B5 | I2（観測だけの Resource に基準の設定を書く＝golden config）。保存は provider が Apply の副作用で（または host-file に書き出す Procedure） | ◎ | provider |
| N-B6 | I2（operational の type。期待の値を欄に書き、drift として知らせ。Apply は無い）＋ Condition で待つ | ◎ | provider |
| N-B7 | I13 | ◎ | Expand |
| N-B8 | 期限つきの許可：Expand の入力に時計（時刻を粗くして決定的に）を渡し、期限を過ぎた許可を出力しない。**Expand の入力に `now` を含めるか**が論点（含めれば ◎、含めなければ人が消す） | ○ | Expand |
| N-B9 | I8 ＋ I4（E-B3） | ○ | provider |
| N-B10 | I1（LB の provider）＋ Procedure（drain＝`apply: {weight: 0}`→`wait: active_conns == 0`→更新→戻す） | ◎ | provider |
| N-B11 | I1 ＋ 鍵の回転は Procedure（事前公開→`wait: {for: TTL}`→切替→撤去）。**時間の待ち（`wait: {for: 2h}`）が Procedure に要る** | ○ | provider |
| N-B12 | I1 ＋ I12（予約のアドレスは from_pool） | ◎ | provider |
| N-B13 | I12 | ◎ | — |
| N-B14 | I1 ＋ Procedure（両端を同じ Transaction の group で確定すれば片側だけの切替を防げる） | ◎ | provider |
| N-B15 | I13（全台に同じ Resource を展開） | ◎ | Expand |
| N-B16 | 集合の上の不変条件（経路の全部の MTU が同じ）。**Condition に集合の量化（`all(select).mtu == 9000`・`count(...)`）が要る**。たたき台の「式 1 つ」が単一の観測の参照だけなら書けない | ✕ | — |
| N-B17 | Condition（`reach`・`observe`・`check`） | ◎ | — |
| N-B18 | I16（Procedure の `concurrency: 1` と隣の台の Condition） | ◎ | provider |
| N-B19 | I7 ＋ `ledger` provider（E-I4） | ○ | — |
| N-C1 | I1 | ◎ | provider |
| N-C2 | Policy `prune: false` 既定 ＋ provider の `Delete` が中身のあるものを断る ＋ I3 の承認 | ◎ | provider |
| N-C3 | Procedure `trigger: {on: before_delete}`（E-C3） | ○ | — |
| N-C4 | I15（`snapshots` の Resource に `keep: 7`。provider が古いものを消す Apply を出し、Delete 相当は Policy の予算に数える＝Schema で壊す印） | ◎ | provider |
| N-C5 | I15（`backup/last_success max_age`）＋ 復元の試験は `restore-test/last_run max_age: 30d` の Resource（provider が Apply で試験を走らせる） | ◎ | provider |
| N-C6 | `ledger` provider（E-I4） | ○ | — |
| N-C7 | I1 ＋ 鍵の発行は provider の Apply（値は backend に置き、宣言には参照だけ） | ◎ | provider |
| N-C8 | I14 | ◎ | provider |
| N-C9 | I1 | ◎ | provider |
| N-C10 | I17 ＋ Procedure | ◎ | provider |
| N-C11 | I6（段階と gate） | ◎ | provider |
| N-C12 | E-C2 | ○ | provider |
| N-D1 | I1 | ◎ | provider |
| N-D2 | Expand の入力にカタログ（版）を渡し、`image@sha256` を欄に。入れ直しは I11 | ◎ | Expand |
| N-D3 | 「置けるノードの集合」は欄の比べ方が等値でない（`node in [a,b,c]`）。**欄の値に式（集合に含まれる・範囲）を許すか、provider の正規形で吸収するか**。HA の資源は I1 | ○ | provider |
| N-D4 | I6（`invoke: migrate` → `wait` → 戻す）。Policy で VM の type を手順の間 report に | ◎ | provider |
| N-D5 | I8（Transaction の Prepare＝スナップショット、Rollback＝戻す。確かめは Condition） | ◎ | provider |
| N-D6 | I1 | ◎ | provider |
| N-D7 | I1（E-D5） | ◎ | provider |
| N-D8 | I1 ＋ 古い tag の削除は Schema の壊す印で予算に | ◎ | provider |
| N-D9 | I6 ＋ I16（drain は `invoke`、PDB は k8s に委ね `wait: observe(node).drained == true`）。クラスタの中は委譲 | ◎ | provider |
| N-D10 | E-C2 | ○ | provider |
| N-D11 | E-D8 | ○ | provider |
| N-E1 | I1 | ◎ | provider |
| N-E2 | I16 ＋ `invoke: reboot` ＋ `wait: observe(host).uptime < 5m` | ◎ | provider |
| N-E3 | I1 の drift の知らせ ＋ import の道具 | ◎ | 道具 |
| N-E4 | I17 ＋ hash 観測 | ◎ | provider |
| N-E5 | E-E3 | ○ | provider |
| N-E6 | I1（印をホストに置く。E-D5） | ◎ | provider |
| N-E7 | I13 | ◎ | Expand |
| N-E8 | I13 または provider の雛形 | ◎ | — |
| N-E9 | I11 ＋ E-A9 | ○ | — |
| N-F1 | I1 | ◎ | provider |
| N-F2 | I6 | ◎ | provider |
| N-F3 | provider の `Delete` が断る ＋ Policy | ◎ | provider |
| N-F4 | I6（gate・invariant `count(primary)==1`）。**集合の Condition（N-B16）** | ○ | provider |
| N-F5 | I1 ＋ I15 | ◎ | provider |
| N-F6 | I6（発行→配布→失効の 3 段。重なりは `wait: {for: 1h}`） | ○ | provider |
| N-F7 | I1 | ◎ | provider |
| N-F8 | I1（provider を足すだけ） | ◎ | provider |
| N-G1 | I1 | ◎ | provider |
| N-G2 | I17 | ○ | SDK |
| N-G3 | E-G5 | ◎ | provider |
| N-G4 | 期限の前の更新：Resource `cert` の欄 `renew_before: 30d`、provider が `not_after` を観測し、`now() + 30d > not_after` なら drift → Apply（再発行）。**時計を Observe か Condition のどちらかで読めること**（provider が「残り」を欄で返せば ◎） | ○ | provider |
| N-G5 | I6（重なりの期間は `wait: {for}`） | ○ | provider |
| N-G6 | I1 ＋ I17 | ◎ | provider |
| N-G7 | 外（関守） | 外 | 道具 |
| N-G8 | I1 | ◎ | provider |
| N-G9 | 秘密の生成：Resource `vault/secret` に `generate: {length: 32}`。provider の Apply が生成して backend に置く（値はコアを通らない） | ◎ | provider |
| N-G10 | Schema の壊す印 ＋ Policy の承認 | ◎ | provider |
| N-H1 | I1（監視設定のファイル）／Expand | ◎ | provider・Expand |
| N-H2 | Events ＋ sink | ◎ | sink |
| N-H3 | Policy の窓に入った・出た Event を sink が受けて silence を作る（sink の実装） | 外 | sink |
| N-H4 | sink（metrics）。コアは周の統計を Events に | 外 | sink |
| N-H5 | sink（OTLP） | 外 | sink |
| N-H6 | I16（段の gate に Condition `observe(metrics-probe/health).ok == true`、`on_unknown: block`） | ◎ | provider |
| N-H7 | I15 ＋ Procedure（`canary/last_run max_age: 1d`、provider の Apply が作って確かめて消す。cleanup の失敗は level） | ○ | provider |
| N-I1 | 道具 ＋ I2 | 外 | 道具 |
| N-I2 | 宣言した欄だけ | ◎ | — |
| N-I3 | I4（report） | ◎ | — |
| N-I4 | I18 | ◎ | — |
| N-I5 | 道具 | 外 | 道具 |
| N-I6 | `ledger` provider | ○ | provider |
| N-I7 | 道具（変換） | 外 | 道具 |
| N-J1 | I10 | ◎ | — |
| N-J2 | I11 | ◎ | — |
| N-J3 | I16（段・concurrency・health の Condition・gate。bake は `wait: {for}`） | ○ | — |
| N-J4 | I6 ＋ I3 ＋ I12（解放は人が CLI） | ◎ | — |
| N-J5 | I14 | ◎ | provider |
| N-J6 | I2 ＋ sink（集計は外） | 外 | sink |
| N-J7 | Resource と Policy と Procedure の `reason`（監査と Event に載せる）。Expand が network_policy の reason を comment に | ◎ | — |
| N-J8 | E-J8 | ○ | — |
| N-J9 | plan --json ＋ expect-revision | ◎ | — |
| N-J10 | I15（定期＝古さの観測。cron を持たない） | ◎ | provider |
| N-J11 | plan が Graph の下流を出す（Expand の出力の差分から影響を辿る） | ◎ | — |
| N-J12 | E-J6 | ◎ | — |
| N-K1 | ◎（不変条件） | ◎ | — |
| N-K2 | I4 | ◎ | — |
| N-K3 | ローカルのファイル・unknown への縮退・採番は intent（E-K4） | ◎ | — |
| N-K4 | E-K7 | ○ | 道具 |
| N-K5 | Record の run ＋ `critical` | ◎ | — |
| N-K6 | 揮発の Record ＋ 中央の Procedure（E-K9） | ◎ | — |
| N-K7 | E-K8 | ○ | 運用 |
| N-K8 | I20 ＋ Procedure | ◎ | — |
| N-K9 | Transaction の group の「確定の途中で切れた台は unknown」（E-B9） | ◎ | provider |
| N-L1 | Record の監査 ＋ sink | ◎ | sink |
| N-L2 | Policy の approval | ◎ | — |
| N-L3 | Record の監査に書く | ◎ | — |
| N-L4 | 読み取り HTTP ＋ listener | ◎ | listener |
| N-L5 | 欄の型 `secret`（マスク） | ○ | SDK |
| N-L6 | status/plan と Events（level の since） | ◎ | — |
| N-L7 | Record の回し ＋ sink | ◎ | sink |
| N-L8 | Expand（reason を comment に） | ◎ | Expand |
| N-M1 | labels ＋ Policy の select | ◎ | 運用 |
| N-M2 | Policy の select | ◎ | — |
| N-M3 | Expand の出力の検証 | ◎ | — |
| N-M4 | E-M4 | ○ | 道具 |
| N-M5 | Event に `labels`（owner）を載せ、sink が振り分ける | ◎ | sink |
| N-M6 | E-M6 | ○ | 道具 |
| N-M7 | `--policy-set` ＋ Record のロック | ◎ | — |
| N-N1 | E-D8 ＋ Policy の `observe_every` | ○ | provider |
| N-N2 | 範囲で分けて複数起動 ＋ Condition | ◎ | 運用 |
| N-N3 | 起動の引数（どの intent を読むか）。PR は git | ◎ | 運用 |
| N-N4 | Policy の `max_concurrent_observe`（provider ごと） | ◎ | — |
| N-N5 | sink（metrics） | 外 | sink |
| N-O1 | 運用 | 外 | 運用 |
| N-O2 | E-O2 | ◎ | SDK |
| N-O3 | I18（自分の VM・ユニットは external_owners か別の常駐で） | ◎ | 運用 |
| N-O4 | E-O3 | ◎ | — |
| N-P1 | E-P1 | ◎ | — |
| N-P2 | I20 | ◎ | — |
| N-P3 | E-J12 | ○ | — |
| N-P4 | provider の側 | 外 | provider |
| N-P5 | Expand が純粋なので golden で確かめられる | ◎ | Expand |
| N-Q1 | I10（一時的な Policy） | ◎ | — |
| N-Q2 | I2（一時的な Policy の select） | ◎ | — |
| N-Q3 | 道具 | 外 | 道具 |
| N-Q4 | I7 | ◎ | — |
| N-Q5 | E-Q4 | ◎ | — |
| N-Q6 | E-K7 | ○ | 道具 |
| N-R1 | 期限が来たら振る舞いが変わる：Policy の `until` と gate/Procedure の `timeout` はコアが時計で評価する（◎）。**Resource や許可そのものの期限**（N-B8・N-G4）はたたき台の Condition（観測した値だけ）では書けない | ✕ | — |
| N-R2 | 古さで動く：provider が「最後の成功からの経過」を欄で返せば I15（◎）。**Condition で `now()` を読めれば一般化できる** | ○ | provider |
| N-R3 | Policy の windows | ◎ | — |
| N-R4 | `wait: {for}`・timeout・cooling_off・Transaction の timeout | ○ | — |
| N-S1 | E-J12 | ○ | — |
| N-S2 | 置き場 2 つ ＋ Record | ◎ | — |
| N-S3 | 常駐の HA：Record のロック（flock）は 1 台の中だけ。**別の台で続きから動かすには Record を共有ストレージに置き、ロックが越えられること**。コアには「ロックが取れなければ進めない常駐になる」決まりだけ | ○ | 運用 |
| N-S4 | 停止影響：採番は intent、秘密は unknown、sink は積んで送り直す、git は道具 | ◎ | — |
| N-S5 | 文書 | 外 | 文書 |
| N-T1 | I4 の `external_owners` ＋ prune の範囲 | ◎ | — |
| N-T2 | I18 ＋ Condition | ◎ | — |
| N-T3 | I1（ansible）＋ `exec` の段 | ◎ | provider |
| N-T4 | Condition（metrics-probe） | ◎ | provider |
| N-T5 | Record の採番の「空きを選ぶ」口を差し替え | ○ | 道具 |

### 4.3 書きにくい所の擬似 YAML（○・✕ の根拠）

判定が ○・✕ になった行のうち、部品の広げ方が決まれば ◎ になるものを、提案の書式で書いて示す。

#### (1) base chain の変更（E-B3）：Transaction ＋ 前後の Condition ＋ 承認

```yaml
resources:
  - type: nftables/chain
    id: fw1/inet/filter/input
    fields:
      type: filter
      hook: input
      priority: 0
      policy: drop                 # Schema: policy/hook/type/priority は「作る・変えると壊す」→ Policy の承認レーン
      rules: ["jump mgmt", "tcp dport 443 accept"]
    apply:
      transaction:
        timeout: 30s               # Prepare（写しと時間切れの仕掛け）→ 確かめ → Commit（仕掛けの取り消し）
        confirm:
          - {phase: before, when: reach(ssh://fw1:22)}        # 変える前に届かなければ変えない
          - {phase: after,  when: reach(ssh://fw1:22)}        # 変えたあとに届かなければ時間切れで戻る
          - {phase: after,  when: reach(ssh://app01:22)}      # 経路の先（空からの初回は after だけ）
          - {phase: after,  when: observe(guest-os/app01).reachable == true}
          - {phase: after,  when: check:bgp-up}
policies:
  - name: fw-chain-approval
    select: {type: nftables/chain}
    approval: {quorum: {n: 1, of: [alice]}}      # 壊す印の欄の変更はこの承認が無いと止まる
```

広げる所：Condition の列に `phase`。Transaction を group にまとめる口（`group:`）。「到達の先が 1 つも before に無い宣言は断る」は読み込みの検証。

#### (2) FW の多相更新（E-A5）：Procedure の段の `skip_if`・`critical`・`gate`

```yaml
procedures:
  - name: bmc-fw
    target: {type: redfish/system, select: {labels: {rack: "3"}}}
    trigger: on_drift                            # desired.firmware ≠ observed.firmware のとき 1 回（1-outstanding）
    concurrency: 1
    steps:
      - name: verify
        skip_if: observe(redfish/firmware/{t}).version == "{desired.firmware}"
        invoke: {op: verify_image, args: {sha256: "{desired.image_sha256}"}}
      - name: approve
        gate: {approval: {quorum: {n: 1, of: [bob]}}, timeout: 24h}
      - name: stage
        invoke: {op: stage}
      - name: apply
        critical: true                           # ここで止まったら自動で再開せず gate に回す
        invoke: {op: apply}
      - name: reboot
        invoke: {op: reset, args: {type: GracefulRestart}}   # 送りかけの印を Record に先に書く
      - name: confirm
        wait: {when: observe(redfish/firmware/{t}).version == "{desired.firmware}", timeout: 30m}
    done_when: observe(redfish/firmware/{t}).version == "{desired.firmware}"
    on_failure: halt
```

#### (3) 大量の正当な作業（E-J2・E-A9）：一時的な Policy と Procedure の結び付き

```yaml
policies:
  - name: reimage-2026-11
    select: {scope: {rack: "3"}, type: redfish/system}
    allow: {kind: reimage, max_count: 48}        # Delete と Create の対を 1 と数える
    windows: ["2026-11-05 22:00 .. 2026-11-06 06:00 Asia/Tokyo"]
    approval: {quorum: {n: 2, of: [alice, bob]}, separation: true, cooling_off: 0s}
    applicant: carol
procedures:
  - name: os-install
    trigger: {policy: reimage-2026-11}          # レーンが開いた（窓・承認・cooling-off）ときだけ、対象ごとに枠を 1 つ使って 1 回
    target: {type: redfish/system, select: {scope: {rack: "3"}}}
    steps: [...]
```

Record：レーンの使用量（executed）と宣言を初めて見た時刻（cooling-off）。「宣言の外は通常の予算」「枠は発射直前に消費」「失敗しても戻さない」はコアの決まり。

#### (4) 秘密（E-G2）：欄の型と Expand の入力

```yaml
resources:
  - type: postgresql/role
    id: app
    fields:
      password: {secret: "vault:/db/app#password"}   # 型 secret。コアは値を持たず、世代（backend の Generation）を比べる
```

Expand の入力に `secrets: {ref → generation}` を渡す（コアが backend に問い、読めなければ `unknown` の診断）。provider の Apply は SDK の `Resolve(ref, generation)` で値を取り出して使い、終われば消す。

#### (5) 外部入力の守り（E-K3）：Expand の診断と前周の件数

```yaml
expand:
  - kind: exec:./expand-from-inventory
    inputs: {inventory: {file: inventory.yaml, min_ratio: 0.5, expected_min: 40}}
```

コアは入力を読めなければ「届かない」として Expand にその旨を渡し、Expand は依る Resource を `unknown` の診断で返す（空の desired を作らない）。件数の急減（前周比）はコアが前周の件数（メモリ、一回性は Record）と比べて周を止める。Expand は純粋なまま。

#### (6) 時間を読む Condition（N-R1・N-G4・N-B8）：たたき台では書けない

```yaml
  - type: pki/cert
    id: www
    fields: {renew_before: 30d}
    # provider が not_after を観測し、now()+30d > not_after を drift とする（provider の中で時計を読む）→ ◎
  - type: nftables/chain
    id: fw1/inet/filter/input
    fields:
      rules:
        - {text: "tcp dport 2222 accept", until: "2026-12-01T00:00Z"}   # Expand が now を入力に受け取れば消せる
```

時計を「観測の 1 つ」として Condition に `now()` を足すか、provider と Expand の入力に閉じ込めるかが §6 の論点。

#### (7) 集合の上の不変条件（N-B16・N-F4）：たたき台では書けない

```yaml
procedures:
  - name: leaf-upgrade
    invariant: count(observe(netconf/system/*[role=leaf]).bgp_established == true) >= 95
```

Condition に `all(...)`・`any(...)`・`count(...)` の量化を足せば書ける。足さなければ metrics-probe（外の集計）に逃がす。

#### (8) 承認をネットワーク越しに（E-M4）

Record は 1 台のファイル。承認は `sakutei approve`（ssh で入って CLI）か、Record を git に置いて承認を PR で（Policy の承認者＝レビュー承認者）か、読み取り HTTP とは別の薄い書き込みの道具（Record に追記するだけ・認可は前段）のいずれか。コアには「承認の記録の形」と「読み込みで数える関数」だけがあればよい。

### 4.4 判定の集計

| 対象 | 行数 | ◎ | ○ | ✕ | 外 |
|---|---|---|---|---|---|
| これまでのユースケース（E-、§4.1） | 136 | 92 | 26 | 0 | 18 |
| 他に考えられるユースケース（N-、§4.2） | 167 | 116 | 36 | 2 | 13 |
| 合計 | 303 | 208 | 62 | 2 | 31 |

- ✕ は 2 行（N-B16 集合の上の不変条件、N-R1 期限で振る舞いが変わるもの）。どちらも Condition の語彙（量化・時計）の問題で、§5.1 の #8・#9 を入れれば ○ 以上になる。
- ○ の原因は 17 種に集約でき（§5.1 の表）、うち新しい部品が要るのは「範囲」1 つ。残りは 8 部品のどれかの欄・口を広げれば済む。
- 外 の 31 行は provider・Expand・sink・道具（import・sync・mirror・break-glass・承認の HTTP）・運用・文書で、コアは口（Provider の任意の口・SecretBackend・Sink・Wake・Record の形）だけを持てばよい。
## 5. 結論

### 5.1 足りない部品・口（○・✕ の原因）と、最小限で埋める案

○・✕ の原因を部品ごとにまとめ、「広げる」か「新しい部品」かを書く。番号は §6 の論点と対応する。

| # | 足りないもの | 原因になった行 | 最小限で埋める案 | 広げる／新しい |
|---|---|---|---|---|
| 1 | **Provider の一回きりの操作の口（Invoke）** | E-A2・E-B11・N-D4・N-E2 ほか Procedure の `invoke` を使う行 | `Invoke(id, op, args) → (sent bool, err)` を任意の口に。送ったか分からない失敗は `ErrNotSent` と区別し、コアは送りかけの印を Record に先に書く。たたき台の「Procedure の動作」はこれが無いと `apply` と `manual` しか無い | Provider を広げる |
| 2 | **Observe の答えの `converging`** | E-A4・E-D1・E-B14 | `present / absent / unknown / converging` の 4 値。`converging` は「相手が作業中。比べない・重ねない・消さない」。欄 `pending: true` を Condition で読む形でも書けるが、コアが「重ねない」を保証するには答えの値として要る | Provider を広げる |
| 3 | **Schema の属性** | E-A10（identity）・E-B12（set/list）・E-G2（secret）・E-C1（壊す印）・E-R3（settle） | 欄ごとに `kind`（string/int/bool/set/list/map/secret）・`identity`・`destructive`（create/update/delete のどれで壊すか）。型ごとに `settle`（Apply のあと読み直すまでの猶予）。managed・computed は要らない（宣言した欄だけ比べる） | Provider を広げる（たたき台の「型と壊すか」を 5 属性に） |
| 4 | **状況で壊す変更を provider が計画時に言う口** | E-C2（volume の付け替え）・E-B3（base chain の作成）・E-C1（Object Lock の向き） | 任意の口 `Classify(observed, desired) []Hint`（この欄のこの変化は壊す・承認が要る・理由）。Schema の静的な印で足りる provider は実装しない。今の PlanRefiner＋ApprovalRequirer を 1 つにしたもの | Provider を広げる（任意） |
| 5 | **宣言を見ないと観測できない縮退** | E-E3（Ansible の check mode・template-file） | 任意の口 `ObserveWith(id, desired)`。これを持つ provider は List も prune も持てず、原因の区別が落ちることを文書に書く。持たなければ最小階級の provider を捨てることになる | Provider を広げる（任意） |
| 6 | **Condition の三値と unknown の倒し方** | E-C4（容量は止める）・E-J6（効かない Apply は数えない）・I5（待つ） | 式は真・偽・不明の三値で評価し、使う側が `on_unknown: wait | block | ignore` を書く（既定は wait）。式の語彙：`observe(ref).field`、`present(ref)`・`absent(ref)`、`reach(url)`、`check:name`、比較・and/or/not | Condition を広げる |
| 7 | **Condition の phase（変える前／変えたあと）** | E-B3・N-B9 | Transaction の confirm の各項に `phase: before | after`。「before の到達の先が 1 つも無い宣言は断る」 | Condition を広げる（Transaction の中でだけ） |
| 8 | **Condition の集合の量化** | N-B16・N-F4（`count(...) >= N`・`all(...)`） | `all(select).field op v`・`any(...)`・`count(select, cond) op n`。select は Resource の select と同じ語彙。無ければ metrics-probe に逃がす | Condition を広げる（論点 §6-3） |
| 9 | **時計** | N-R1・N-G4・N-B8・E-I4 | 3 案：(a) Condition に `now()`、(b) provider が「残り時間」を欄で返す、(c) Expand の入力に粗い `now`。(b) だけなら Condition は観測だけのまま保てる | 論点 §6-4 |
| 10 | **Procedure の段の属性** | E-A5（skip_if・critical）・E-J8（disruption）・N-B11（wait for duration）・E-Q3（段の間だけ report） | 段に `skip_if`・`critical`・`gate`・`wait: {when, for, timeout}`・`policy:`（段の間だけ効く一時的な Policy）。Procedure に `trigger: {on_drift | policy:<name> | before_delete | manual}`・`concurrency`・`invariant`・`disruption{estimate, affects, upstream, max_wait, notify_before}`・`done_when`・`on_failure` | Procedure を広げる |
| 11 | **Policy の欄** | E-J2（allow）・E-J4（until・reason・by）・E-N2（observe_every）・N-N4（同時数）・E-J6（max_ineffective） | `select`・`mode`・`windows`・`budget{per_cycle, destructive_per_cycle, destructive_per_window, window, max_in_flight, max_ineffective_applies}`・`approval{quorum, roles, separation, cooling_off}`・`prune`・`allow{kind, max_count}`・`until`・`reason`・`by`・`observe_every`・`max_concurrent_observe`。一時的な Policy は Record に置き、CLI が書く | Policy を広げる |
| 12 | **範囲（集合の境界）の宣言** | E-B2・E-D4・E-I6・E-K2・N-T1 | `ranges: [{provider, range, labels, prune, external_owners}]`。List の引数・prune の可否・触らないものを 1 か所に。今の `providers:`＋`external_owners:`＋`deletion_policy`＋`--claim` を畳む | **新しい部品**（または Resource の一部。論点 §6-1） |
| 13 | **Record に置く小さな状態** | E-J12（前回の provider の版）・E-K3（前周の件数）・E-J2（レーンの使用量と時計）・E-A2（送りかけの印）・E-J6（一回性の効かない回数） | Record の 3 つ（手順・承認と監査・採番）に「前回の周の目印（provider の版・件数・Expand の出力のハッシュ）」を足す。観測の値は置かない。失えば安全側（report・halt）に倒れる | Record を広げる |
| 14 | **起床の口** | E-N5・E-M6 | `sakutei wake`／シグナル／HTTP の 1 口で「次の周を今すぐ」。影響範囲だけの収束は持たない | Events の逆向きの 1 口 |
| 15 | **承認の書き込みの口** | E-M4 | CLI で Record に書く。ネットワーク越しは薄い道具か git | 道具（コアは記録の形だけ） |
| 16 | **Expand の入出力** | E-G2・E-K3・E-A7・E-B5 | 入力：intent・inventory・採番の結果・秘密の世代・（粗い時計）。出力：Resource・Procedure・Policy・ranges・採番の要求・診断（断る／知らせる／unknown にする） | Expand を広げる（純粋さは保つ） |
| 17 | **Delete の断り** | E-F1・N-C2・E-G1 | `Delete` が「中身がある・子がある・keep の印がある」で断れること（PermanentError とは別の `ErrRefuse` と理由）。コアは断られた削除を report に出し続け、予算に数えない | Provider を広げる（返り値） |

✕ の 2 行（N-B16 の集合の不変条件、N-R1 の期限）はどちらも Condition の語彙の問題で、#8・#9 を入れれば ○ 以上になる。新しい部品が要ると判断したのは #12 の「範囲」だけで、これはたたき台の Resource か Policy のどちらにも収まりが悪い（Resource は 1 件の宣言、Policy は方針。範囲は「集合の境界」という第 3 の種類）。

### 5.2 過剰な部品・欄（使うユースケースが無い、または組み合わせで書ける）

たたき台の「やめる」一覧に加えて、本書の検証で「組み合わせで書ける」と確かめたものを足す。

| 分類 | 今あるもの | 置き換え |
|---|---|---|
| Provider の任意の口 15 | PlanRefiner・IntentValidator・AbsentValidator・UpdatePolicyValidator | `Validate(resources)` 1 つ（読み込みで断る理由を返す）と `Classify`（#4） |
| | IntentRanges・ConfigExcluder・OwnerFieldSelector | 範囲の宣言（#12）を intent にだけ置けば、設定との突き合わせの口が要らない |
| | CheckReader | `ObserveWith`（#5）に名前を変えるだけ。要らないなら捨てる |
| | BulkObserver | `Observe(ids...)` をまとめて呼べる形にすれば別の口は要らない。prune の根拠は List の ID だけ（型分離は保つ） |
| | Confirmer/ConfirmUser/CommitGrouper/ApprovalRequirer | Transaction（confirm の Condition・group）と Schema の壊す印・Classify に畳む |
| Capabilities の旗 | TrueObserve・List・Pruneable・Converging・CommitConfirm・ObserveOnly・BulkObserve | 「任意の口を実装しているか」と「Schema」と「ranges の prune」で表せる。残すのは `ObserveOnly`（観測だけ）と `Version` と `Settle` の 3 つ |
| 原因タグ A〜E | per-resource の 5 分類・rendered_revision・last-seen | 3 種（intent が変わった／外の入力が変わった／drift）に縮める。D（波及）は Graph の上流を見れば分かる。C（版）は Record の前回の版で止める（E-J12）。last-seen はメモリの環として残してもよいが Record には置かない |
| 止める・緩める系 | remediation mode・schedule・freeze（2 入口）・planned_change・max-* の旗・approve-large-change・profile・runtime（daemon/oneshot/roles）・締める/緩める/向きなしの規則 | Policy 1 つ＋一時的な Policy（Record）＋ `--policy-set`。旗は締める向きの一時的な Policy を書く糖衣だけ。profile は Policy の preset。daemon/oneshot の分けは要らない（一回性は同じ Policy を 1 周だけ評価する） |
| 観測で条件を待つ系 | ready_condition・require・wait・done_when・apply_check・confirm の observe/check・capacity gate・compatibility の照合・manual の閉じ | Condition 1 つ（#6〜#9） |
| 比べない欄系 | managed・computed・provider ごとの `fields`・PlanRefiner の落とし・netconf の属性の名指し | 「宣言した欄だけ比べる」。正規形は provider の中（宣言と観測で同じ関数） |
| 2 段で当てる系 | netconf の 2 段・nftables の守り・guard の arm/disarm・commit_group・confirmafter・RolledBackError | Transaction 1 つ（Prepare/Commit/Rollback＋confirm の Condition＋group） |
| 状態ファイル 9 種 | task-store・workflow-store・alloc-store・apply-check-state・freeze-state・daemon-status・cause-state・fencing-store・audit-log | Record 1 つ（dir＋flock）。cause-state と apply-check-state は「前回の周の目印」に、freeze は一時的な Policy に、fencing は捨てる |
| 設定の置き場 4 つ | 旗 128・intent・provider 設定・ホスト上の道具の設定 | intent と つなぎ方の設定 1 つ。旗は場所と一回性だけ（20 以下）。provider 固有の旗 13 は全部削る |
| 規模・分散 | claim・shard・fencing・gitdist・S2-free・DR モード・tier・BulkObserve・天井算術・scoped reconcile | 範囲で分けた複数の常駐＋Condition で待つ（◎）。tier は Policy の `observe_every`（任意）。早期収束は起床の口（#14） |
| 採番 | authority の 5 操作・Reserve/Confirm・cooldown・Lookup キャッシュ・HA・space・claim 分割・write_back の 3 方式 | intent のファイルが正、Record は空きを選んで書き足すだけ。`blocks`・`reuse: false`・`template/kind` は残す。解放は人が CLI。差し替えの口は 1 つ（`Allocator.Pick`） |
| その他 | capacity gate・oscillation 検知・compatibility・network_policy・observe_tiers・inventory guard・DR plan・mirror・secrets extract・breakglass | Condition（容量）・Policy（効かない上限）・Expand（互換の表・網の許可・inventory の診断）・道具（mirror・secrets・breakglass） |

削らない方がよいと判断したもの（たたき台で「やめる」に入っているが要求が残る）：
- **効かない Apply の上限（oscillating）**：E-J6・E-K10・N-Q5。Policy の 1 欄で残す。
- **provider の版が変わった周に止める（cause C）**：E-J12・N-S1。Record の前回の版で残す。
- **容量の門**：E-C4。Condition（on_unknown: block）で残す。
- **観測の間引き**：E-N2。Policy の任意の欄で残す（既定は毎周全部）。
- **外部入力の急減（incomplete≠empty）**：E-K3。前周の件数で残す。
- **運用プロファイル**：E-J10。preset として残す（コアの型は要らない）。

### 5.3 部品どうしの境界があいまいな所

| 境界 | あいまいさ | 本書の線引き |
|---|---|---|
| Procedure と Transaction | どちらも「準備→確かめ→確定／戻す」に見える | Transaction は **1 周の中・1 つの Apply（または group）の中**で終わり、戻すのは相手側の仕掛け（時間切れ）。Procedure は **周をまたぎ**、Record に進みを持ち、戻すのは段として書く。Transaction の中に承認は入らない（承認は Policy で Apply の前） |
| Condition と Policy の時間の窓 | 「いつ動くか」が両方にある | 時間の窓は Policy だけ（mode を変える）。Condition は観測の値だけ（時計を含めるかは §6-4）。Procedure の `wait: {for}` は時間だが「待つ」であって「許す」ではない |
| Expand と採番 | 採番は副作用（空きを選ぶ） | Expand は要求を出すだけ（純粋）。空きを選んで intent のファイルに書くのはコア（Record）。次の周の Expand が値を見る。採番待ちの Resource は unknown 扱い |
| Policy の承認と Procedure の gate | 承認が 2 か所 | 同じ型（quorum・roles・separation・timeout・cooling_off）。Policy の承認は「Apply を通す門」、gate は「手順を進める門」。記録は同じ Record |
| Resource の範囲と Policy の select | 両方「どれに効くか」 | 範囲は **集合の境界（List の引数・prune・外の持ち主）** で Resource の側。select は **方針の当たり先（type・scope・labels）** で Policy の側。範囲が無い provider（List 無し）にも Policy は当たる |
| Events と Record の監査 | 両方「起きたこと」 | Events は今の状態（level）と事件（edge）を外へ流す（失ってよい）。監査は変えた事実と承認の鎖（失ってはならない）。Apply の成功は監査に、drift は Events に |
| Transaction の確かめと Apply の後の読み直し（settle） | 両方「効いたか」 | settle はコアのループの決まり（全 Apply に等しく、Schema の hint で待つ）。confirm は Transaction を持つ provider だけ（戻しを伴う） |
| Invoke と Apply | 一回きりの操作と欄の変更 | 欄として観測できて宣言に戻るものは Apply（`power: Off`）。相手が使い切る・観測に残らないものは Invoke（`boot_once`・`reset`）。Invoke は Procedure からしか呼べない |
| Expand の診断とコアの検証 | 「断る」が 2 か所 | Expand は自分の入力の意味（重なり・理由の無い例外・表に無い版）を断る。コアは出力の形（同じ ID・未知の欄・循環・Schema に無い欄・範囲の外）を断る |
| Policy の report と provider の観測だけ | 両方「Apply しない」 | provider の観測だけは能力（宣言しても Apply できない）。Policy の report は方針（できるがしない）。status は理由を分けて出す |
| 「書く回か」 | 命令・mode・旗で決まっていた | Policy の評価の結果「この周に Apply を呼ぶか」だけ。provider は Apply が呼ばれたときだけ書き込み用の資格を取り出す |

### 5.4 最小のコアでも外してはならない不変条件

受け入れ・chaos テストが現実の bytes と件数で確かめているもの（E-P1）を中心に、設計書・改訂履歴・レビューから集めた。番号は性質のテストの名前にできる。

**観測**
1. 観測を周をまたいで持ち越さない。前の周の値を正としない（plan の観測も reconcile に持ち越さない）。
2. Observe は desired を見ない（最小階級の `ObserveWith` を除く）。Diff は現実を触らない。
3. unknown ≠ absent。届かない・読めない・覗けない・定義に合わない・バルクに無い・組の部品が unknown・秘密の世代が読めない は全部 unknown。unknown の Resource には何もしない（Apply も Delete も prune も、下流も進めない）。
4. converging の相手には重ねない。

**減算**
5. 消すのは「範囲を宣言し、List が complete で、宣言に無く、外の持ち主でなく、prune を許し、予算と承認を通った」ときだけ。宣言から外しただけでは消さない。
6. List が complete でない・前周比で急減した周はその範囲を消さない。自分が消した分と一時的な Policy で消した分だけ差し引く。前周の件数を失えば消さない側に倒す。
7. Observe の答えをまとめて取った結果を prune の根拠にしない（List の ID 集合だけ）。
8. 壊す型は既定で prune しない。壊す変更（Delete・壊す印の欄・Classify が言ったもの）は予算に数え、承認のレーンが開いているときだけ流す。レーンは別枠であってバイパスでない。
9. provider が断った Delete（中身・子・keep）は消さず報告し続ける。
10. 採番の値は宣言から消えても自動で返さない。`reuse: false` の値は二度と払わない。ファイルと Record が食い違えばどちらにも寄せず、そのプールから新しい値を配らない。

**Apply と手順**
11. 同じ Resource に同時に 2 つの Apply を走らせない。Apply を出した下流を同じ周に出さない。in-flight を予算に数える。
12. Apply は冪等。順序付きの欄は全置換。permanent な失敗は desired か provider の版が変わるまで再試行しない。
13. Apply のあと読み直し、効かなければ数え、上限で止める（unknown は数えない）。巻き戻された Apply は即止める。
14. 非冪等な操作を収束ループに載せない。Procedure は同じ対象に未完了が高々 1 つ（1-outstanding）。
15. 一回きりの操作は送りかけの印を先に Record に書き、印があれば二度送らず halt。`critical` の段で止まった手順は自動で再開せず gate に回す。
16. Transaction は確かめられなければ確定しない。group は 1 つでも落ちれば全部確定しない。before の到達の先に届かなければ変えない。
17. 手順の「済んだ」は観測で決める（人の申告・Record の run は根拠にしない）。unknown ≠ 済んだ。halt の対象を自動で宣言どおりに戻さない。

**方針**
18. 複数の Policy が当たれば最も厳しいものが勝つ。旗は締める向きにしか効かない。report でも観測と知らせは続ける。
19. 予算を超えたら超過分の保留でなく止める（halt-all）。周をまたぐ累積（窓）を数える。
20. 承認は申請者≠承認者を型で強制し、要件を満たす異なる承認者だけ数える。承認・拒否・期限切れを監査に残す。Record に書けなければ開かない・発射しない。
21. 一時的な Policy（freeze・allow）は理由と期限を持ち、期限で消える。Record が読めない周は止める側に倒す。
22. provider の版が変わった最初の周は report で止める。

**宣言と設定**
23. 範囲・除外・方針・採番の結果は intent に置く。設定はつなぎ方だけ。食い違いは弱い側に寄せず読み込みで断る（置き場が 1 つなら起きない）。
24. 同じ ID の二重宣言・未知の欄・循環・範囲の外の宣言・世界の全部を渡す除外は読み込みで断る。宣言 1 つの不備で全体の収束を止めない（断るのは読み込み、レーンが閉じるのは保守側）。
25. Expand は同じ入力に同じ出力。外の入力が届かないとき空の desired を作らず、依る Resource を unknown にする。

**秘密と出力**
26. 秘密の値をコアが持たない・比べない・記録・Event・誤りの文・トレースに出さない。
27. 書かない周に書き込み用の資格を取り出さない。
28. 通知・監査の送り・トレースが送れなくても収束を止めない。監査の鎖は書き換えない。
29. 確かめられなかった周に level を resolved にしない。観測しなかった周の値を 0 や「ずれなし」として出さない。
30. plan は何も消費せず、reconcile と同じ判定を評価する。文と JSON は同じ構造から作る。

**設計書にあり、落としやすいもの（§23〜§29 から）**
31. 段0（自分の土台）の drift を自動修復しない。自分の依存先（金庫の宛先・管理の経路の最小核）を収束の対象にしない。
32. 依存（金庫・Record）が ready でない間は Observe すら試みない（空回りとログの氾濫を避ける）。
33. 観測で任意のコマンドを走らせない（値の形で縛る。ルールの文に区切り文字があれば断る）。
34. 現実→intent の道具（import・sync）の出力を Apply の経路につながない（片方向）。
35. Schema はテストの都合で変えない。Schema の検証を通らない provider は読み込みで断る（fail-closed）。

### 5.5 修正版のコアの部品と Go の interface の素案

#### 部品（8 → 9。範囲を足し、Events と Record は分けたまま 1 対として数える）

| 部品 | 役割 | 持つ状態 |
|---|---|---|
| 1 Resource | 宣言 1 件（type・id・scope・labels・ensure・fields・depends_on） | なし |
| 2 Range（範囲） | 集合の境界（provider・range・labels・prune・external_owners） | なし |
| 3 Provider | Schema・Observe・Apply・Delete・List（＋任意：Transaction・Invoke・Classify・Validate・ObserveWith） | なし（つなぎ方の設定は外） |
| 4 Condition | 三値の式（観測・到達・check・集合・（時計）） | なし |
| 5 Graph | depends_on と Condition から wave と待ちを決める | なし（周の中） |
| 6 Policy | select × mode × windows × budget × approval × prune × allow × until | 一時的な Policy と使用量・時計は Record |
| 7 Procedure | 段（apply/invoke/exec/manual/wait/gate）・trigger・done_when・invariant・concurrency・disruption | 進みは Record |
| 8 Expand | 純粋な展開（Go か exec）。入力と診断 | なし |
| 9 Record ＋ Events | Record：手順の進み・承認と監査の鎖・採番の結果（写し）・一時的な Policy・前回の周の目印。Events：level/edge の流れと sink 1 つ、読み取り HTTP、起床の口 | Record はファイル＋flock |

#### interface の素案（抜粋。名前は仮）

```go
// ---- Resource / Range ----
type ID struct{ Type, Scope, Name string }        // Scope は "site=a,rack=3" の正規形。宣言の ID は Type+Scope+Name で一意
type Fields map[string]any                        // 宣言した欄だけ。秘密は SecretRef（値を持たない）
type Resource struct {
    ID        ID
    Ensure    Ensure            // Present | Absent
    Fields    Fields
    DependsOn []Dependency      // {On ID; When Condition; OnUnknown Disposition}
    Labels    map[string]string // Policy の select・Event の振り分け
    Origin    Origin            // intent のファイルと行（監査・診断）
}
type Range struct {
    Provider       string
    Range          map[string]string // provider が List に使う引数（zone・tables・hosts・base_dn…）
    Labels         map[string]string // この範囲の現実に付く scope ラベル
    Prune          bool              // 既定 false
    ExternalOwners []Exclusion       // {Owner; ID; IDPrefix; Fields}
}

// ---- Provider ----
type Availability int // Present, Absent, Unknown, Converging
type Observation struct { Availability; Fields Fields; ObservedAt time.Time; Note string }
type Provider interface {
    Schema() Schema
    Observe(ctx context.Context, ids []ID) (map[ID]Observation, error)       // まとめて呼べる。無い ID は Absent を明示して返す
    Apply(ctx context.Context, id ID, desired Fields, current Observation) error
    Delete(ctx context.Context, id ID, current Observation) error            // 断るときは ErrRefuse{Reason}
    List(ctx context.Context, r Range) (Listing, error)                      // {IDs []ID; Complete bool}
}
type Schema struct {
    Version string                       // 比べ方が変われば上げる（無ければ登録を断る）
    Types   map[string]TypeSchema
}
type TypeSchema struct {
    Fields      map[string]FieldSpec     // {Kind; Identity; Destructive DestructiveOn; Secret bool}
    Settle      time.Duration            // Apply のあと読み直すまで
    ObserveOnly bool                     // 観測だけ（Apply/Delete を呼ばない）
}
// 任意の口
type Transactor interface {
    Prepare(ctx context.Context, changes []Change, opt TxnOptions) (Txn, error)   // 写し・仕掛け・commit confirmed
}
type Txn interface { Commit(ctx context.Context) error; Rollback(ctx context.Context) error }
type Invoker interface {
    Invoke(ctx context.Context, id ID, op string, args Fields) error            // 送れなかったら ErrNotSent
    Describe(op string) OpSpec                                                  // {Destructive bool}
}
type Classifier interface { Classify(id ID, current Observation, desired Fields) []Hint }  // {Field; Destructive; Approval; Reason}
type Validator interface { Validate(res []Resource, ranges []Range) []Diagnostic }
type DesiredObserver interface { ObserveWith(ctx context.Context, id ID, desired Fields) (Observation, error) } // 最小階級

// ---- Condition ----
type Tri int // True, False, Unknown
type Condition interface { Eval(ctx context.Context, env Env) (Tri, error) }   // env: observe(ref)・reach(url)・check(name)・now()（論点）
type Disposition int // Wait, Block, Ignore（unknown のとき）

// ---- Policy ----
type Policy struct {
    Name     string
    Select   Selector                 // {Types; Scopes; Labels; All}
    Mode     Mode                     // Apply | Report
    Windows  []Window
    Budget   Budget                   // {PerCycle; DestructivePerCycle; DestructivePerWindow; Window; MaxInFlight; MaxIneffectiveApplies}
    Approval *Approval                // {Quorum{N, Of}; Roles; Separation; CoolingOff}
    Prune    *bool
    Allow    *Allow                   // {Kind; MaxCount}（一時的な別枠）
    ObserveEvery int
    Until    time.Time; Reason, By, Applicant string   // 一時的な Policy
}
func Effective(ps []Policy, r Resource, now time.Time) Decision   // most-restrictive-wins を 1 か所で

// ---- Procedure ----
type Procedure struct {
    Name string; Target TargetSel; Trigger Trigger   // OnDrift | Policy(name) | BeforeDelete | Manual
    Precondition Condition; Steps []Step; DoneWhen Condition
    Invariant Condition; Concurrency int; Order OrderSpec; Timeout time.Duration
    OnFailure Failure; Disruption *Disruption
}
type Step struct {
    Name string; Kind StepKind       // Apply | Invoke | Exec | Manual | Wait | Gate | Policy
    Target *ID; Fields Fields; Op string; Args Fields; Instruction string
    When Condition; For time.Duration; Timeout time.Duration
    SkipIf Condition; Critical bool; Gate *Approval; TempPolicy *Policy
}

// ---- Expand ----
type Input struct { Intent []Document; Inventory map[string]any; Allocations map[string]Allocation; SecretGens map[string]int64; Now time.Time /* 粗い。論点 */ }
type Output struct { Resources []Resource; Ranges []Range; Policies []Policy; Procedures []Procedure; AllocRequests []AllocRequest; Diagnostics []Diagnostic }
type Expander interface { Expand(ctx context.Context, in Input) (Output, error) }   // 同じ入力に同じ出力

// ---- Record ----
type Record interface {
    Lock(ctx context.Context) (release func(), ok bool, err error)   // 進める常駐だけが取る
    Runs() RunStore                 // Procedure の進み・送りかけの印・承認
    Audit() AuditLog                // Append(entry) / Verify()。鎖
    Allocations() AllocStore        // 空きを選んで intent のファイルに書き足す。Pick は差し替え可
    TempPolicies() PolicyStore      // freeze・allow（until つき）
    Marks() MarkStore               // 前回の周の目印：provider の版・範囲ごとの件数・Expand の出力のハッシュ・効かない回数
}

// ---- Events ----
type Event struct { Kind string; Level bool; State string /* firing|resolved */; Resource *ID; Policy, Procedure string; Since time.Time; Count int; Attrs map[string]string }
type Sink interface { Emit(ctx context.Context, e Event) error }     // 積むだけで戻る。止めない
type Wake interface { Wake(reason string) }                           // 次の周を今すぐ
```

`sakutei` の起動の引数は「intent の場所・つなぎ方の設定・Record の場所・`--policy-set`・`--once`・`--json`」の 6 つに収まる見込み。provider 固有の旗は無い。
## 6. 判断に迷う所（迷いの中身と選択肢）

決めつけず、選択肢と、それぞれを選んだときに何が起きるかを書く。§5.1 の番号を併記する。

### 6-1 「範囲」を 9 つめの部品にするか、Resource か Policy に入れるか（#12）
- 迷い：集合の境界（List の引数・prune・外の持ち主）は、1 件の宣言（Resource）でも方針（Policy）でもない。今は `providers:`・`external_owners:`・`deletion_policy`・`--claim` の 4 か所に散っている。
- 選択肢：(a) 9 つめの部品 `ranges:` として独立させる（本書の案。型が 1 つ増えるが、prune の決まりと unmanaged の報告が 1 か所に集まる）。(b) Resource の一種（`type: <provider>/range` の Resource）として書く（部品は 8 のまま。ただし「Resource は比べる」という意味がぶれる）。(c) Policy の `select` に `prune` と `external_owners` を持たせる（Policy が「何を数えるか」まで知ることになり境界があいまいになる）。
- 本書の傾き：(a)。

### 6-2 Observe の答えは 3 値か 4 値か（#2）
- 迷い：たたき台は present/absent/unknown。今の設計は ok/unknown/converging で absent が無い（NotFound は Read では unknown、集合演算で消えたと知る）。worked example も NotFound → Unknown。
- 選択肢：(a) 4 値（present/absent/unknown/converging）。Observe が「確かに無い」を言えるので Create の判断が 1 回の Observe で済む。ただし provider 作者が「届かない」を absent と誤って返す事故が起きうる（契約テストで塞ぐ）。(b) 3 値（present/unknown/converging）＋ absent は List の集合差でだけ決める。安全だが、List を持たない provider は Create の判断ができない（今は Read の Unknown＋desired ありで Create している実装が多い）。
- 本書の傾き：(a)。契約テスト「unreachable ≠ absent」を SDK に残す。

### 6-3 Condition に集合の量化を入れるか（#8）
- 迷い：`count(...) >= N`・`all(...)` が無いと N-B16・N-F4・E-A5 の invariant が書けない。入れると式が小さな言語になる。
- 選択肢：(a) 入れる（select の語彙を Policy と共有）。(b) 入れず、集計は provider（metrics-probe）か Expand に逃がす（Condition は 1 つの観測の参照だけ）。
- 本書の傾き：(a)。ただし式は「参照・比較・and/or/not・all/any/count」までに止め、算術や文字列の加工は持たない。

### 6-4 時計を誰が読むか（#9）
- 迷い：期限（証明書・一時的な許可・ledger の last_verified）は時間の関数。Condition に `now()` を入れると「観測した値についての式」でなくなり、plan の決定性も日時に依存する。
- 選択肢：(a) Condition に `now()` を入れる（最も素直。plan は「いまの時刻で」評価する）。(b) provider が「残り時間」「経過」を欄で返し、Condition は観測だけ（期限の意味を provider が知る）。(c) Expand の入力に粗い時刻（時刻を日や時に丸めて決定的に）を渡し、期限つきの宣言を Expand が落とす。
- 本書の傾き：(b) を既定にし、(c) を Expand の任意の入力として許す。(a) は見送る（Policy の窓と Procedure の timeout だけがコアの時計を読む）。

### 6-5 承認をどこに記録し、どこから書くか（#15）
- 迷い：Record は 1 台のファイル。承認者は別の場所にいる。読み取り HTTP に書き込みを足すとコアが認証の問題を抱える。
- 選択肢：(a) CLI だけ（ssh で入る。認可は OS の権限）。(b) Record の承認の部分を git に置き、PR のレビュー承認を承認とみなす（Policy の承認者＝レビュー承認者。監査は git にもある）。(c) 薄い書き込みの道具を外に置く（前段で認証し Record に追記する。今の `--approval-listen` 相当）。
- 本書の傾き：(a)＋(c)。(b) は「宣言に置く」に最も忠実だが、承認の時刻と cooling-off の時計を git のコミット時刻に頼ることになり、ミラーやローカルでは取れない（v3.7 で退けた理由と同じ）。

### 6-6 早期収束（影響範囲だけの収束）を捨ててよいか（#14）
- 迷い：テナントの秒反映（SC-07・SC-11）は早期収束パスで答えていた。たたき台は触れていない。今の実装は CLI に未配線。
- 選択肢：(a) 起床の口だけ持ち、周は全部回す（小さな規模なら十分）。(b) 影響サブ DAG だけ回す経路を持つ（Graph に「新旧の出力の差分から影響を辿る」が要り、予算の共有・prune 禁止の規律が増える）。
- 本書の傾き：(a)。規模が要るときは Policy の `observe_every` で削る。

### 6-7 原因の分類をいくつ残すか（E-J13）
- 迷い：A〜E の 5 分類は per-resource の帰属と rendered_revision を要する。たたき台はやめる。しかし「版が変わった周に止める」（C）と「外の入力が変わった」（E）と「人が変えた」（A）の区別は status を読む人が頼りにする。
- 選択肢：(a) 3 分類（intent／外の入力／drift）＋「版が変わった周は止める」。(b) 分類を持たず、status に「前周と比べて desired が変わったか」だけ出す。(c) 5 分類を残す。
- 本書の傾き：(a)。Expand の入力と出力のハッシュを前周と比べるだけで出せ、観測の状態を持たない。

### 6-8 fencing（二重の書き手の守り）をどこまで残すか（E-K8）
- 迷い：範囲を intent で分ければ平時は重ならない。引き継ぎの窓と誤設定のときだけ二重になる。
- 選択肢：(a) 捨てる（重なる宣言は読み込みで断る。引き継ぎは人が片方を止めてから）。(b) Record に範囲ごとの世代を置き、Apply の前に確かめる小さな口を任意で持つ（共有の Record が要る）。(c) provider 側の lease（相手が受け付ける世代）に委ねる。
- 本書の傾き：(a)。多拠点の要求が実際に出たら (b)。

### 6-9 Transaction の「戻す」の主体
- 迷い：netconf は機器の時計、nftables はホストの道具、cli-device は provider の rollback、Proxmox のスナップショットは provider。コアが `Rollback` を呼ぶ形にすると「Sakutei が落ちても戻る」が保証できない。
- 選択肢：(a) `Rollback` は任意で、既定は「相手側の時間切れに任せる」（`Commit` を呼ばなければ戻る）。(b) コアが必ず `Rollback` を呼ぶ。
- 本書の傾き：(a)。不変条件は「Commit を呼ばない限り戻る」。

### 6-10 Expand を純粋に保つ代償（E-K3・#16）
- 迷い：外部入力の「前回値の保持」「急減の検知」は状態が要る。Expand に持たせると純粋でなくなる。
- 選択肢：(a) コアが入力を読み、届かないときは Expand に「届かない」と伝え、Expand は依る Resource を unknown の診断で返す。急減はコアが前周の件数（メモリ・一回性は Record）で止める。(b) 前回の入力を Record に保存し、届かないときはそれを Expand に渡す（今の設計に近い。Record が肥える）。
- 本書の傾き：(a)。unknown の縮退で足りる（v3.60 で秘密について同じ判断をしている）。

### 6-11 provider の設定の除外（安全の網）を残すか（E-D2）
- 迷い：今は intent の `external_owners:` が正で、provider 設定の除外は網（食い違えば断る）。置き場を 1 つにすれば規則は消えるが、「設定を触るだけで守れる最後の柵」も消える。
- 選択肢：(a) intent だけ。(b) provider のつなぎ方の設定に「触らない ID」を残し、intent と食い違えば断る（規則が 1 つ残る）。
- 本書の傾き：(a)。段0 の VM は別の常駐にも触らせない要求なら、範囲（ranges）の外に置けば触れない。

### 6-12 `Delete` を Apply と分けるか（ADR-0003 との違い）
- 迷い：今は Apply(plan) が create/update/delete を 1 つで受ける。たたき台は Delete を分ける。
- 選択肢：(a) 分ける（Delete が断る理由を返しやすく、予算の数え方が型で見える）。(b) 1 つに戻す。
- 本書の傾き：(a)。

### 6-13 Procedure の trigger の種類（#10）
- 迷い：`on_drift`（Task 相当）・`policy:<name>`（reimage 相当）・`before_delete`（final_backup）・`manual`（人が起こす）のほかに、「時間で起こす」（定期）を持つか。
- 選択肢：(a) 持たない（定期は I15 で Resource の古さとして書く）。(b) `every:` を持つ（cron を持つことになる）。
- 本書の傾き：(a)。

### 6-14 一回性と常駐の違いをどこまで無くすか
- 迷い：今は「Task/Workflow は常駐だけ」「レーンは S4 があるときだけ」「cause-state は一回性だけ」と差がある。
- 選択肢：(a) 同じ Record の置き場を使えば一回性も常駐も同じ（Procedure を進めるのはロックを取れた方。一回性は 1 周で終わる）。(b) 一回性は Procedure を進めない。
- 本書の傾き：(a)。ただし `critical` の段を一回性が進めてよいかは別に決める（進めない方が安全）。

### 6-15 規模の前提を捨てた結果、復活させる条件
- 迷い：たたき台は「VM 1 台で 1 つの基盤」を前提に分散・tier・BulkObserve を落とす。20 構成の半分はこれらを「成立を分ける芯」としていた。
- 選択肢：§29.2 の再検討トリガ（Observe が間隔の無視できない比率・API の課金・リソース数が桁で増えた・2 つ目の同種 provider）をそのまま残し、落とす機能ごとに「戻すときの最小の口」を書いておく（Observe の batch・Policy の observe_every・ranges での分割・Record の世代）。
- 本書の傾き：トリガの表を新しい設計書に残す。
## 7. 付録

### 7.1 たたき台の「やめる」一覧と、その要求の受け皿

| やめるもの | 残る要求（出典） | 受け皿 |
|---|---|---|
| claim とシャード・git のミラー・S2-free | 拠点ごとに常駐を分け、分断中も維持を続ける（SC-06・14、E-N3・E-K6・E-K9） | 範囲で分けた複数の常駐＋Condition で待つ。ミラーは git の道具。Record を揮発にすれば S2-free |
| fencing | 引き継ぎの窓の二重書き（E-K8） | 重なる範囲は読み込みで断る。要れば Record の世代（§6-8） |
| DR モード | 主拠点全損時の引き継ぎと緊急書込（E-K7） | 別の Policy の集合で起動・Expand の入力で DR の intent を選ぶ・署名と合流は git の道具 |
| 観測の tier | BMC が遅い（E-N2） | Policy の `observe_every`（任意） |
| BulkObserve | 一括 API（E-D8） | `Observe(ids...)` をまとめて呼ぶ形。prune の根拠は List だけ |
| 容量の門 | 満杯に載せない（E-C4） | Condition（`on_unknown: block`） |
| 振動の検知 | 効かない Apply を繰り返さない（E-J6・E-K10） | Policy の `max_ineffective_applies`＋Apply 後の読み直し |
| 原因タグ A〜E と last-seen | なぜ変わるか（E-J13）、版が変わった周に止める（E-J12） | 3 分類（intent／外の入力／drift）と Record の前回の版。last-seen はメモリの環 |
| 運用プロファイル | 1 名運用の cooling-off・規制の N-of-M（E-J10） | Policy の approval（cooling_off・quorum・separation）。preset は文書か Expand |
| 採番の authority・キャッシュ・TTL・Reserve/Confirm | 二重払い出しの防止・衛星停止でも維持（E-K4） | intent のファイルが正、Record が空きを選ぶ（flock）。blocks・reuse:false・template は残す |
| runtime の役と締め緩めの規則 | 同じ intent で違う回り方の常駐（E-M5）、その場で締める（E-J4） | `--policy-set`、一時的な Policy（締める旗はその糖衣） |

### 7.2 情報源の Backlog 番号と本書の ID の対応（抜粋）

| Backlog | 本書 | Backlog | 本書 | Backlog | 本書 |
|---|---|---|---|---|---|
| #48 #80〜#84 | E-A1 E-A3 | #51 #52 #148 | E-A5 | #86 | E-A8 E-E4 |
| #87 #139 | E-A2 E-A3 E-A8 | #88 #209 | E-A9 | #150 | E-A6 |
| #151 | E-A7 | #92 #126 | E-F2 | #125 #133 #137 #203 | E-F1 E-F3 |
| #121 #173 #179 #191 #192 #214 | E-B2 E-B3 | #174 #195 #197 | E-B5 E-B6 | #141 #175 #176 #199 #200 #208 #211 | E-B7〜E-B10 |
| #159 | E-B11 | #122 #132 #160 #205 | E-C1 | #204 | E-C2 |
| #143 #165 #170 #196 | E-D1 | #158 | E-D2 | #163 | E-D3 |
| #164 #169 | E-D4 | #155 | E-D5 E-E2 | #130 #185 #186 | E-E1 E-G5 |
| #127 | E-E3 | #144 #182 | E-G1 | #188 | E-G3 E-G4 |
| #79 #74 | E-H1 | #131 #154 | E-H2 | #54 #189 | E-H3 |
| #149 | E-H4 | #118 #135 #136 #161 | E-H5 | #152 | E-J8 |
| #22 | E-I1 | #23 #35 | E-I2 | #181 #182 | E-I6 |
| #39 #44 | E-J2 | #40 | E-J3 | #184 | E-J4 |
| #166 | E-J6 | #177 | E-J7 | #46 | E-J9 |
| #183 #193 #213 | E-J11 | #212 | E-J12 | #117 #166 | E-J14 |
| #31 #47 | E-K3 | #24 #25 #105 #157 #206 #116 | E-K4 E-T4 | #45 | E-K7 |
| #28 #29 | E-K8 E-K9 E-N3 | #187 #210 | E-L1 | #43 | E-L2 |
| #119 #146 | E-O4 E-S4 | #27 #49 | E-D8 | #30 | E-N2 |

未着手の Backlog（#20・#36・#49・#53・#55〜#66・#107・#109・#128・#198・#201・#202・#207）は §3 の N- の行（N-A13・N-B6・N-D3・N-H6・N-H7・N-M6・N-T5 ほか）として扱った。
