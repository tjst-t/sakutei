# CHANGELOG v3.31 → v3.32

**改訂日: 2026-10-28**

リソースを宣言で消す口（明示の absent、`ensure: absent`）を足した改訂である（Backlog #163。親のストーリー「proxmox-vm の
使い勝手：宣言で VM を消す・宣言の外の VM を unmanaged に出す・pool に入れる」の、宣言で消す部分）。正しい状態は宣言に置く
（原則1）ので、消すことも宣言で表す。prune（§12.1）の閉世界・`pruneable`（§12.3）の二段構えは変えていない。

## #163：宣言で消す（§7.4・§8.6・§12.2・§13.3・§29.1）

- **背景**:
  - prune しない型（`Pruneable:false`。proxmox-vm の VM など）は宣言から外しても消えず、intent に「消す」を書く口も
    無かった。§12.2 のモード2は「消すには明示 `state: absent` を要求する」と書いていたが、汎用の口は実装されていなかった。
  - 既にあった明示の absent の仕組みは早期収束パス（§8.6）の tombstone だけで、v1a の実装は「desired から消えた＝個別削除の
    意図」とみなしていた。これでは prune しない型も、宣言から外すだけで（Watch を持つ DesiredSource の下では）予算だけを
    通って消えてしまう。
- **決定**（具体は §29.1「宣言で消す」）:
  - リソースに `ensure: present | absent`（既定 present）。`state` は §4.3 の State・§4.9 の状態分類と紛れるので使わない。
    provider に依らない汎用の口（`DesiredResource.Absent`）。
  - 在れば Delete（破壊的。承認の要る op `core.Op.RequiresApproval`）、無ければ適合、観測できなければ何もしない。
    `pruneable` に関わらず効く。
  - 承認：そのリソースを覆う planned_change（`kind: delete`）のレーンが開いているときだけ走る（承認・cooling-off・窓・
    `max_count`、§13.3・§14.4・§14.5）。レーンは承認であって除外ではなく、通常の予算（§13.1・§13.2）にも数えて超えれば
    止める。レーンの外では消さず、level の Event `AbsentAwaitingApproval`、`plan` の `blocked_by: approval`。
  - 読み込みで断る：absent に中身（`fields`・`depends_on`・`update_policy`・`deletion_policy`）を書く、absent に依る
    リソース、Workflow の対象の absent、観測だけ・State を観測できない provider での absent、provider が触らないと決めて
    いるもの（`provider.AbsentValidator`。proxmox-vm の `protected_names`）。
  - 早期収束パスの tombstone は「新 desired で `ensure: absent` になったもの」だけにした（消えただけでは消さない）。
  - 自分の absent の Delete は List 急減（§12.4（b））で差し引く（非同期の Delete が次の List より後に終わっても）。
  - 採番の owner の値は自動では返さない（§19.5 のまま、`sakutei allocator release` と cooldown）。
  - proxmox-vm：走っている VM は stop で止めてからディスクごと消す（`power_off` は効かない）。
- **見送り**:
  - planned_change を書かずにリソースごとに承認を書く口（`approval:` を absent のリソースに置く）。承認の要件・プロファイル・
    cooling-off の時計（S4）をレーンと二重に持つことになるので、レーンを使う。1 台だけ消すなら `max_count: 1` の宣言で足りる。
  - absent の Delete の前の正常停止（`power_off` の決まりを absent に持ち込む口）。先に `power: stopped` を宣言すればよい。
  - 消えたあとの採番の自動の返却（§19.5 の「下流の削除を List で確かめてから」を満たす経路は別に作る）。

## 受け入れテスト

`tests/acceptance/explicit_absent_test.go`（fake と偽の Proxmox VE）:

- `[AC-163-1]` 読み込みで断る：absent に fields・depends_on・update_policy、absent に依るリソース、知らない ensure の値。
- `[AC-163-2]` `plan` が DELETE を出し（`--json` の `absent: true`）、レーンが無ければ `blocked: approval`、承認のある
  レーンがあれば止めない。現実に無ければ `Converged (no changes)`。
- `[AC-163-3]` 一回性の `reconcile`：レーンが無い・承認の無いレーンでは消さない（`AbsentAwaitingApproval`・
  `PlannedChangeUnapproved`）。承認のあるレーンでも予算を超えれば消さない（`BUDGET HALTED`）。承認と予算があれば消え、
  次は適合。
- `[AC-163-4]` 常駐：レーンが無いあいだは消さず（`awaiting_approval=1`）、承認のある planned_change を書けば消えて適合に
  戻る。宣言から外しただけのリソースは消さない。
- `[AC-163-5]` proxmox-vm：走っている VM を absent にすると、承認が無ければ残り、承認があれば止めてから消える。触らない
  VM は absent にできない。

既存の早期収束の受け入れテスト（`scoped_reconcile_test.go`・`inflight_budget_test.go`）は、tombstone を「消えたもの」から
「`ensure: absent` になったもの」に、予算の確かめを承認のあるレーンの下に書き換えた。

単体：`internal/budget/absent_test.go`（レーンの外は止めて通常予算に数えない・レーンの中でも通常予算に数えて止める）、
`internal/diff/absent_test.go`（在れば承認の要る破壊的な Delete・無ければ・観測できなければ何もしない、読み込みの検査）。

統合：`make integration-proxmox` の「absent で消す」段（宣言から外しただけでは消えない・レーンが無ければ消さない・承認の
あるレーンで走っている VM を止めて消す・次は適合・触らない VM は断る）。
