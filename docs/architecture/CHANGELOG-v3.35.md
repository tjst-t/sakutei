# CHANGELOG v3.34 → v3.35

**改訂日: 2026-10-28**

Apply が成功を返しても現実が変わらないときに、同じ Apply を繰り返し続けて成功として終わっていたのを直した改訂である
（Backlog #166）。設計書に書いてあったが未配線だった §11.1 Tier2（settle の猶予）・Tier3（効かない Apply の自動隔離、
`oscillating`）を配線し、その決まりを書いた。

## 背景

- BMC が電源 On の命令に 204 を返しても電源が Off のままの故障（qemu-bmc の障害注入 `power-action-ignored` で再現）で、
  `sakutei reconcile` は毎回「1 change(s) applied」・終了コード 0 で終わり、180 秒の間に同じ On の Apply を 30 回繰り返した。
  Apply の応答だけを見て、現実が変わったかを確かめていなかった。
- §11.1・§11.3・§15.3 に Tier2・Tier3 は書いてあったが、§29.2 のとおり未配線（`core.StatusOscillating` は型だけ）だった。

## 決定（§11.1 の「v1 の実装」、§11.3、§15.3、§29.1「Apply の効きの確かめ・効かない Apply の自動隔離」）

- **確かめ（Tier2）**：Apply が成功を返したリソースは、その場で読み直して Diff が消えたかを確かめる。型ごとの settle
  （`provider.Capabilities.Settle` を足した。redfish-system は 15 秒、既定は 0＝1 回だけ読み直す）の間は待つ。相手が
  Converging と言えばそこで終え（Tier1）、観測できないまま settle が過ぎたものとともに数えない（unknown ≠ ineffective）。
  常駐では Apply の goroutine の中で確かめ、その間リソースは in-flight（converging）。
- **数え方と隔離（Tier3）**：settle を過ぎても Diff の指紋（desired の値と、op の種類・破壊性・欄ごとの現実と宣言の値。
  cause tag と理由は含めない）が Apply したときと同じなら「効かなかった」。Diff が変われば数え直す。同じ Diff で
  `max_ineffective_applies`（既定 3）回、または最初から `max_ineffective_duration`（既定なし）で、そのリソースへの Apply を
  止める（`oscillating`、§9.7 の全順序に載る。そのリソースだけ alert-only、下流は待つ）。
- **解く**：Diff の中身（desired か現実の値）が変わったとき・現実が宣言どおりになったとき・人が
  `sakutei apply-check release --state <file> --audit-log <file> --approver <id> <resource>`（S4 に記録してから状態の
  ファイルに求めを書く。`sakutei task approve` と同じ形）で解いたとき。
- **記録**：S3（メモリ）。`--apply-check-state <file>` を渡すと作業の記録としてファイルに書く（常駐は起動時に読み、周の初めに
  人の解く求めを取り込んで書く。一回性の reconcile は回をまたいで数える）。正しい状態ではなく、失っても数え直すだけ。
  一覧は `sakutei apply-check list --state <file> [--json]`。
- **終了コード**：一回性の `reconcile` は、Apply が成功を返したのに現実が宣言どおりにならなかった（Diff が残った）か、Apply を
  止めたリソースがあれば **4**。0・1 の意味は変えない（1 の条件と同時なら 1）。2 は Go の panic・包む道具の失敗と紛れるので
  避けた。コマンドごとの表を `docs/exit-codes.md` に 1 か所にまとめた。
- **設定と食い違い**：旗 `--settle`・`--max-ineffective-applies`・`--max-ineffective-duration`（常駐と reconcile）と、intent の
  `apply_check:`（top-level は provider の型ごとの列、リソースは自分の 1 つ。リソースの宣言が型の宣言に勝つ）。旗と intent が
  食い違えば **most-restrictive-wins**（§14.3）：settle は長い方、回数は少ない方、時間は短い方（上限なしより上限のある方）。
  旗で intent を緩めることはできない（長く効く値はレビューを通る intent が正）。
- **知らせ**：Event `ApplyIneffective`（level、§22.3）。常駐ではリソースに記録がある間 firing（回数が進むたびに更新）、止めた
  ものは failure=permanent で stuck・critical、記録が消えたら resolved。通知の口（Alertmanager・webhook）にもそのまま届く。
  止めたリソースは、読み取り専用の口の `GET /status`（`state: oscillating`・`apply_stopped`・`summary.oscillating`）・
  `--daemon-status` と `GET /status` の `daemon.apply_stopped`・`GET /plan`（`blocked_by: apply_stopped`）・`/metrics`
  （`sakutei_resources{state="oscillating"}`・`sakutei_apply_ineffective_total`）に出る。周の要約は `oscillating=N`・
  `ineffective=N` と言い、「converged」と言わない。一回性の要約は「not converged: N change(s) reported applied, but M
  resource(s) still differ …」と言い、「applied」と言い切らない。
- **既存の決まりとの関係**：strategy: task は Apply しないので数えない（#112）。cause C の ghost diff は Apply しないので
  数えない（§10.4）。予算・容量・alert-only で止めた op は Apply していないので数えない。止めたリソースの Drift は level の
  まま続き、`--stuck-after` で stuck に上がる（§22.3、変えていない）。早期収束の scoped 経路（§8.6）は確かめない（次の定期の
  周が確かめる）。
- **§29.2**：多周期の振動検知・隔離の行を「v3.35 で配線した」に改め、残る先送り（自分の Apply が一度効いたあとで相手が
  settle より遅く戻す綱引きは数えない・converging の汎用の deadline）とその再検討トリガを書いた。

## 見送り

- 自分の Apply が一度は効いた（確かめで Diff が消えた）あとで、相手が settle より遅く戻す綱引きの検出。今は Drift の level が
  続いて stuck に上がることで知らせる。
- converging の汎用の deadline（Tier1 の provider が自分で持つ待ちの外）。
- 早期収束の scoped 経路での確かめ。
- `sakutei status`（一回性）でデーモンの止めたリソースを `resources[]` に `oscillating` として出すこと（`--daemon-status` の
  `apply_stopped` には出る）。

## 受け入れテスト

`tests/acceptance/apply_ineffective_test.go`:

- `[AC-166-1]` 一回性：Apply は成功を返すが現実が変わらない → 終了コード 4、要約は not converged、`ApplyIneffective`。
- `[AC-166-2]` `--apply-check-state` で回をまたいで数え、上限で Apply せずに 4。`apply-check list`（文・`--json`）と
  `apply-check release`（S4 監査に記録）で解くと次の回は Apply する。
- `[AC-166-3]` ふつうに効く Apply は今どおり 0、観測できないものと同時なら 1。
- `[AC-166-4]` 常駐：上限で Apply が止まり、`GET /status`・`--daemon-status`・`GET /plan`・`/metrics` に出て、
  `ApplyIneffective` が stuck・critical で firing（1 回）。現実が直ると resolved、次のずれはまた Apply する。
- `[AC-166-5]` 常駐：desired を変えると解け、Apply が戻る。
- `[AC-166-6]` 常駐：人が `apply-check release` で解くと次の周に Apply が戻る。
- `[AC-166-7]` 偽の Redfish（電源の Reset に 204 を返すが電源を変えない、`SetIgnorePowerActions`）で、常駐が上限で Reset を
  止め、電源が直ると resolved。
- `[AC-166-8]` 偽の Redfish で一回性の reconcile が 4。intent の `apply_check:` の settle（300ms）が旗（100ms）より長いので
  勝つ。

単体：`internal/reconcile/applycheck_test.go`（設定の決め方・数え方・止め方・解き方・状態のファイル・Diff の指紋）、
`internal/loader/apply_check_test.go`（`apply_check:` の読み込みと拒否）。

統合：`make integration-redfish` の 9 段目（`[AC-166-9]`、qemu-bmc 0.10.0 の障害注入 `power-action-ignored`）。2026-10-28 に
合格（1〜8 段目もそのまま合格）。
