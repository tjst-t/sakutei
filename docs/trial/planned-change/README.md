# planned_change を手で試す（専用の予算レーン・kind の整合・reimage の対・承認・窓・急減判定）

設計 §13.3（planned_change 宣言・専用予算レーン）、§29.1（kind 整合検証・reimage のペアリング判定）、§14.4・§14.5（承認の
requirements と運用プロファイル）、§12.4（b）（List 急減 halt との連動）が、実際の CLI と常駐デーモンで設計どおりに動くかを確かめる。
受け入れテスト（`tests/acceptance/chaos_planned_change_test.go`・`planned_change_profile_test.go`）は fake provider で同じことを見ているが、
ここでは dnszone・nftables（ファイル系の実 provider）と、scope の付いた現実を作れる fake を手で回す。

2026-10-23 にこのコンテナで `run.sh` を回した。結果は下の各節（出力の抜粋。窓の時刻はその日のもの）。

## 確かめる観点（設計から挙げたもの）

| # | 観点 | 設計 | 手順 |
|---|---|---|---|
| 1 | 宣言があっても、通常予算で止まる量の変更が**専用レーン**で通る（予算の無効化でなく別枠） | §13.3 | A3・B2 |
| 2 | 同じ周の**宣言外**の変更（別 scope）は通常予算で止まる | §13.3「宣言中でも、宣言外の暴発は止まる」 | A3・B2 |
| 3 | `max_count` を超えた分は `PlannedChangeExhausted` を出して通常予算へ。使用量は S4 が実行をまたいで持つ | §13.3「レーンの計上」 | A4 |
| 4 | one-shot `reconcile` は `--audit-log` が無ければレーンを開かず、その旨を表示する | §13.3「one-shot」 | A2 |
| 5 | 承認が requirements（宣言のもの、無ければプロファイルの既定）を満たさないと開かず `PlannedChangeUnapproved` | §13.3・§14.4・§14.5 | A5 |
| 6 | 窓の外（まだ来ていない・過ぎた）では効かない。過ぎたら `PlannedChangeExpired` を一度出す | §13.3 | A6・B6 |
| 7 | scope の外の変更には効かない | §13.3 | A7 |
| 8 | 宣言の kind と合わない破壊的な op はレーンに入らず通常予算で評価し `PlannedChangeKindMismatch` | §29.1 kind 整合 | B3・B4 |
| 9 | `kind: reimage` は同じ scope（ラベル完全一致）の DELETE+CREATE の対を 1 件として通し、破壊予算に delete と数えない。対の無い DELETE は kind 不一致 | §13.3・§29.1 ペアリング | B4 |
| 10 | 常駐ループでレーンが消した分は次の周の List 急減判定から差し引き、宣言外の減少は引き続き halt する | §12.4（b）・§13.3 | B5 |
| 11 | S4 に `planned_change.declared`・`opened`・`executed` が残る | §13.3「S4 の記録」 | A3 |

## 置いてあるもの（docs/trial/planned-change/）

| ファイル | 中身 |
|---|---|
| `initial/example.com.zone` | A の最初の現実。`a1`〜`a3` の A レコード（192.0.2.1〜3） |
| `initial/firewall.rules` | A の最初の現実。base chain `input` と、管理する regular chain の `sakutei_a`・`sakutei_b`（22 番だけ許可） |
| `intent/resources.yaml` | A の資源。`a1`〜`a3` を 198.51.100.x に（scope `zone: example`）、2 本の chain に 443 番を足す（scope `fw: edge`）。`run.sh` はこの後ろに `planned_change:` を足して `intent/main.yaml` にする |
| `run.sh` | A1〜B6 を通して流すスクリプト。宣言の窓は「いま」に合わせてその場で書く。B の fake の現実（state ファイル）も `run.sh` が作る |

## 通しで流す

```sh
docs/trial/planned-change/run.sh            # 作業ディレクトリは /tmp/sakutei-trial-pc（毎回作り直す）
```

Go と python3（fake の state ファイルを書くのに使う）が要る。リポジトリのファイルは書き換えない。B5・B6 は常駐デーモンを数秒ずつ回すので、全部で 30 秒ほどかかる。

宣言の書き方（v1b の YAML サブセットはフロースタイル `{}`・`[]` を読まないので、ブロックで書く）：

```yaml
planned_change:
  scope:
    zone: example
  kind: update                 # delete | update | reimage | recreate
  max_count: 3
  window: "2026-10-23 14:40 .. 2026-10-23 16:40"   # UTC。RFC 3339 も可
  applicant: carol
  approval:
    pr: 7
    approvers:
      - alice
```

## A. dnszone・nftables（kind: update）

`--max-changes-per-cycle 1`（1 周に変更 1 件まで）で、5 件の UPDATE（dnszone 3・nftables 2）を出す。

### A1・A2. 宣言なし／`--audit-log` なし：全部止まる

```
sakutei reconcile: note: 1 planned_change declaration(s) need --audit-log to open their lanes in a one-shot run (max_count and the cooling-off are held in S4 across runs); this run evaluates every change by the normal budget
sakutei reconcile: BUDGET HALTED: rate-limit (per-cycle): 5 total ops (+ 0 in-flight = 5) exceeds max_changes_per_cycle_absolute=1
sakutei reconcile: budget: blocked UPDATE op for dnszone/zone=example/a1.example.com./A
…
Reconcile: BUDGET HALTED (no changes applied): …
```

### A3. `--audit-log` を付ける：宣言の 3 件はレーンで通り、宣言外の 2 件は止まる（観点 1・2・11）

```
PlannedChangeOpened: planned_change(scope=zone=example kind=update window=…): lane opened (profile team; approval 1 of 1 approver(s) [alice] required by the team profile)
PlannedChange: planned_change(scope=zone=example kind=update window=…): 3 change(s) admitted to the planned_change lane (used 0 of max_count 3 before this cycle); not counted in the normal budget
sakutei reconcile: BUDGET HALTED: rate-limit (per-cycle): 2 total ops (+ 0 in-flight = 2) exceeds max_changes_per_cycle_absolute=1
sakutei reconcile: budget: blocked UPDATE op for nftables/fw=edge/inet/filter/sakutei_a
sakutei reconcile: budget: blocked UPDATE op for nftables/fw=edge/inet/filter/sakutei_b
Reconcile: 3 change(s) applied (budget halted some ops)
```

ゾーンは `a1`〜`a3` が 198.51.100.x になり、ruleset は変わらない。S4 には `planned_change.declared`・`planned_change.opened`・
`planned_change.executed`（count 3）が残る。通常予算は宣言外の 2 件だけで数えている（`2 total ops`）。

### A4. 使い切った後：通常予算に戻る（観点 3）

ゾーンを手で元に戻して同じ宣言・同じ `--audit-log` でもう一度回す。別のプロセスでも S4 から使用量 3 を読み直す。

```
PlannedChangeExhausted: planned_change(…): max_count 3 reached; 3 more change(s) are evaluated by the normal budget
sakutei reconcile: BUDGET HALTED: rate-limit (per-cycle): 5 total ops …
```

### A5. 承認が足りない（観点 5）

3 通りとも開かず、5 件とも通常予算で止まる。

```
# team・approvers なし
PlannedChangeUnapproved: …: approval has 0 of the 1 approver(s) required by the team profile; the lane stays closed and changes in its scope are evaluated by the normal budget (§14.4)
# profile: enterprise（2-of-2・申請者≠承認者）に carol（申請者）と alice
PlannedChangeUnapproved: …: approval has 1 of the 2 approver(s) required by the enterprise profile (not counted: separation applicant_ne_approver: carol is the applicant and cannot approve); …
# 宣言の requirements.roles: [dns-owner]。alice は役割を名乗っていない
PlannedChangeUnapproved: …: approval has 0 of the 1 approver(s) required by the declaration's requirements (not counted: approver alice holds none of the required roles [dns-owner] (asserted roles [])); …
```

### A6・A7. 窓の外・scope の外（観点 6・7）

1 時間後に始まる宣言と 1 時間前に終わった宣言を並べると、過ぎた方に `PlannedChangeExpired` が 1 回出て、5 件とも通常予算で止まる。
まだ来ていない方は何も出ない（S4 に `declared` だけ残る）。scope `zone: other` の宣言はレーンを開く（`PlannedChangeOpened`）が、
`zone=example` の変更は入らず 5 件とも止まる。

```
PlannedChangeExpired: planned_change(scope=zone=example kind=update window=…13:40:00Z..…14:40:00Z): window closed (used 0 of max_count 3); changes in its scope are evaluated by the normal budget again
```

### A8. dnszone の削除は planned_change で宣言できない（見つけたこと 1）

```
--- scope zone=example の PruneWithinScope：www の CREATE だけで、ゾーンにある intent 外の a1〜a3 を消す計画が出ない
+ CREATE dnszone/zone=example/www.example.com./A
--- scope を外す（root scope）と a1〜a3 が DELETE になる
+ CREATE dnszone//www.example.com./A
- DELETE dnszone//a1.example.com./A [DESTRUCTIVE]
…
--- それを覆う planned_change は scope を空にできない
  - $.planned_change: 必須プロパティ "scope" がありません
```

そのため削除・reimage・急減判定は、scope 付きの現実を返せる fake provider で試した（B）。

## B. fake provider（scope の付いた現実）

fake の state ファイルの `scopes` に ID ごとのラベルを書くと、List がその scope を付けて返す。desired は全部 `PruneWithinScope`。

### B1・B2. kind: delete：宣言の 5 件はレーンで消え、宣言外の 2 件は止まる（観点 1・2）

現実は `pool=decom` に keep-decom と decom-1〜5、`pool=other` に keep-other と stray-1〜2。`--max-destructive-per-cycle 1`。
宣言なし（B1）は 7 件の DELETE が全部止まる。`kind: delete`・scope `pool: decom`・`max_count: 5` を宣言すると（B2）：

```
PlannedChange: planned_change(scope=pool=decom kind=delete …): 5 change(s) admitted to the planned_change lane (used 0 of max_count 5 before this cycle); not counted in the normal budget
sakutei reconcile: BUDGET HALTED: rate-limit (per-cycle): 2 destructive ops (+ 0 in-flight = 2) exceeds max_destructive_per_cycle=1 (approve-large-change=0)
sakutei reconcile: budget: blocked destructive op DELETE fake/pool=other/stray-1 (budget limit)
sakutei reconcile: budget: blocked destructive op DELETE fake/pool=other/stray-2 (budget limit)
Reconcile: 5 change(s) applied (budget halted some ops)
現実: keep-decom keep-other stray-1 stray-2
```

### B3. kind の不一致（観点 8）

同じ現実に `kind: update` を宣言すると、DELETE はレーンに入らず 7 件とも止まる。

```
PlannedChangeKindMismatch: planned_change(scope=pool=decom kind=update …): 5 destructive op(s) in its scope do not match kind update (e.g. fake/pool=decom/decom-1, fake/pool=decom/decom-2, fake/pool=decom/decom-3); evaluated by the normal budget (§29.1)
sakutei reconcile: BUDGET HALTED: rate-limit (per-cycle): 7 destructive ops …
```

### B4. kind: reimage（観点 8・9）

現実は `pool=hpc` に hpc-head と node-1-v1〜node-5-v1（それぞれ `slot: 1`〜`5`）。desired は hpc-head と node-1-v2〜node-3-v2
（slot 1〜3）。Plan は DELETE 5（node-1〜5-v1）と CREATE 3（node-1〜3-v2）。破壊予算は 1 周 2 件・窓 2 件。

- 宣言なし：`5 destructive ops … exceeds max_destructive_per_cycle=2` で全部止まる（CREATE も `halt-all` で止まる）。
- `kind: reimage`・scope `pool: hpc`・`max_count: 3`：

  ```
  PlannedChange: planned_change(scope=pool=hpc kind=reimage …): 3 change(s) admitted to the planned_change lane (used 0 of max_count 3 before this cycle); not counted in the normal budget
  PlannedChangeKindMismatch: planned_change(scope=pool=hpc kind=reimage …): 2 destructive op(s) in its scope do not match kind reimage (e.g. fake/pool=hpc,slot=4/node-4-v1, fake/pool=hpc,slot=5/node-5-v1); evaluated by the normal budget (§29.1)
  Reconcile: 8 change(s) applied
  現実: hpc-head node-1-v2 node-2-v2 node-3-v2
  ```

  slot 1〜3 の対（6 op）は枠を 3 消費し、通常予算は対の無い 2 件だけを数えて 2 件に収まった。対を delete と数えていれば 5 件で超える。
- 同じ宣言で、対の無い node-4・node-5 が通常予算（1 周 1 件）を超えると止まる（`2 destructive ops … exceeds max_destructive_per_cycle=1`）。

### B5. 急減判定との連動（常駐デーモン、観点 10）

現実 13 件（keep-decom・decom-1〜8・ext-1〜4。ext は desired に無く PruneWithinScope の外）。`kind: delete`・`max_count: 8` で常駐させる。

```
sakutei: pruned fake/pool=decom/decom-1 (reality − desired; planned_change lane)
…（8 件）
sakutei: cycle 1: observed=1 applied=0 launched=0 pruned=8
（cycle 2〜5 は converged (no changes)。13 → 5 件・61% の減少は急減とみなさない）
```

続けて ext-1〜3 を手で消す（5 → 2 件・60%。宣言の外の減少）と halt する：

```
sakutei: prune HALT: List count dropped from 5 to 2 (60% > threshold 50%); halting prune to prevent accidental bulk delete
sakutei: cycle 6: observed=1 halted (0 op(s) held) applied=0 launched=0
sakutei: cycle 6: prune HALTED (safety guard); no orphan deleted this cycle
```

halt は止まった周に基準の件数を更新しない（§12.4（b））ので、以降の周も止まり続ける（設計どおり。人が見てデーモンを起こし直す）。

### B6. 常駐デーモンと窓（観点 6）

4 秒後に始まり 8 秒後に終わる窓（RFC 3339 で秒まで書ける）で、decom-1〜3 を消す宣言を置いて常駐させる。

```
sakutei: cycle 1: BUDGET HALTED: rate-limit (per-cycle): 3 destructive ops (+ 0 in-flight = 3) exceeds max_destructive_per_cycle=1 (approve-large-change=0)
sakutei: cycle 4: PlannedChangeOpened: planned_change(scope=pool=decom kind=delete window=…:32Z..…:36Z): lane opened (…)
sakutei: cycle 4: pruned=3 orphan(s) (reality − desired)
sakutei: cycle 8: PlannedChangeExpired: planned_change(…): window closed (used 3 of max_count 3); changes in its scope are evaluated by the normal budget again
```

窓に入るまでは通常予算で止まり、入った周にレーンで消え、終わった周に一度だけ失効を知らせる。

## 見つけたこと（2026-10-23）

| # | 見つけたこと | 扱い |
|---|---|---|
| 1 | **dnszone・nftables の削除（と reimage・急減判定からの差し引き）は planned_change で扱えない**。両 provider の List・BulkObserve は現実を root scope（ラベル無し）で返す（`internal/dnszone/dnszone.go` の List、「provisional — v1a uses root scope」）。一方 planned_change の scope は空を許さない（空は「全世界にレーンを開く」ので拒否、§13.3）。prune の DELETE は List の ID を対象にするので、root scope の DELETE はどの宣言の scope にも入らない。逆に desired に scope を付けて `PruneWithinScope` にすると、root scope の現実はその prune scope の外になり、**消す計画が黙って出ない**（A8。安全側だが、宣言した prune が効いていないことに気づけない）。kind: update は desired の ID（scope 付き）が対象なので使える（A3） | **設計の判断が要る**ので直していない。案：ファイル系 provider にファイルごとの scope を持たせる（例 `--zone-scope zone=example`。List・BulkObserve がそのラベルを付ける）、あるいは root scope の現実を持つ provider には desired の scope を写す、など。どちらも §17 の scope の決まりに触れる |
| 2 | one-shot `reconcile` が通常予算で止めた CREATE・UPDATE を `budget: blocked 0 op for …`・`blocked 1 op for …` と出していた（`core.OpKind` に String が無く、数字のまま出ていた） | 直した：`core.OpKind` に `String()`（CREATE / UPDATE / DELETE）を足し、`blocked UPDATE op for …` と出る。テスト `pkg/core/plan_test.go` |
| 3 | まだ来ていない窓の宣言は、待っている間何も知らせない（S4 に `planned_change.declared` が残るだけ）。scope の外の宣言（A7）も `PlannedChangeOpened` は出るが、入る変更が無いことは知らせない | 設計どおり（Event は開いた・使い切った・失効した・不一致の時に出る）。気になるなら `plan` に宣言の状態を出す案（使いにくさ・未登録） |
| 4 | one-shot `reconcile` の planned_change の Event（`PlannedChangeOpened` など）は標準出力に接頭辞なしで出て、`BUDGET HALTED` などの行（標準エラー・`sakutei reconcile:` 付き）と出方が違う | 小さな見た目の違い。直していない（使いにくさ・未登録） |
