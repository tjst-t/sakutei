# `sakutei plan --json` の形（schema_version 1）

`sakutei plan --json` は、計画（Diff で止めた dry-run、§26）を標準出力に **JSON を 1 つ**出す（#135）。外の道具（可視化・ダッシュボード）が「いま reconcile したら何が起きるか」を機械で読むための形である。中身は文の出力と同じ `internal/planreport.Report` から作るので、操作の件数・並び・各操作の中身は文の出力と一致する。

- 標準エラーへの出力（採番待ち・planned_change の知らせ・prune の停止・観測できないリソースなど）は文の出力と同じ。`--json` のときは標準出力の文の行を出さず、JSON だけを出す。
- **終了コードは文の出力と同じ**（0 = 計画ができた。変更の有無や、予算で止まる見込みの有無によらない。1 = 計画ができなかった）。終了コード 1 のときに JSON が出るかどうかは次のとおり（外の道具は「標準出力に JSON が出るか」をこれで判断する）。
  - **JSON を出す**（`error` に理由が入る）：予算のフラグの誤り（`--window-duration`・`--on-exceed`）、`--claim` の誤り、`--intent` の指定漏れ、intent を読めない、render（外部入力・採番・合成の展開・`generated/` の書き出し）の失敗、provider を解決できない・intent の id や `update_policy` を provider が受け付けない、観測（Read）のエラー、secret のフィールドの平文、差分の計算のエラー。
  - **JSON を出さない**（標準エラーに 1 行だけ）：フラグの読み取りのエラー（知らないフラグ・値の型の誤り。`flag` パッケージが使い方を出す）と、起動の確かめで直接終わるもの——`--log-format`・`--zone-soa-serial`・`--zone-scope`・`--ruleset-scope`・`--ruleset-target`・`--ruleset-tables` の誤り、`--provider-config` を読めない、`--inventory-file` の誤り（`--inventory-min-ratio` の範囲外・読めない・不完全）、provider の検証（`provider_version` など、§10.4）に通らない。
- **秘密の値は出さない。** secret のフィールドは diff の段階で SecretRef の参照の形（`secretref:<backend>/<path>#genN`）にそろえられており、`fields` には参照だけが入る（§20.2）。plan は秘密の値を解決しない。
- plan は読むだけである。state・S4（`--audit-log`）・採番の記録・Workflow の記録を書かない（`--cause-state` と `generated/` は文の出力と同じく書く）。
- `schema_version` はフィールドを足すだけなら上げない。意味を変える・消すときに上げる。読む側は知らないフィールドを無視する。
- 配列は、空でも `[]` で出す（`null` にしない）。値が無いことの多いフィールド（`cause`・`fields`・`planned_change`・`blocked_by` など）は関係しないときは出さない。

## 止める見込み（予算・容量・alert-only の scope）

plan は reconcile と同じ判定を、何も消費せずに評価する。reconcile と同じフラグを受ける：`--max-destructive-per-cycle`・`--max-changes-per-cycle`・`--max-destructive-per-window`・`--window-duration`・`--on-exceed`・`--approve-large-change`（変更予算、§13.1・§13.2）、`--capacity-gate`（容量ゲート、§13.4）、`--audit-log`（planned_change のレーン、§13.3・§14.5。**読むだけ**）。同じフラグで reconcile したときに止まる操作を `blocked: true` で示す。

- 予算は一回性の reconcile と同じく、その回の操作だけで数える（累積の窓は空から始まる）。常駐デーモンの予算の残りは `/metrics` を見る。
- 予算に数えるのは reconcile が Apply する操作だけである：原因タグ C（ghost diff）の操作と、`strategy: task` のリソースの操作（`task` が付く）は数えない。
- `--audit-log` を渡すと、S4 に記録された宣言の時刻・レーンの使用量から planned_change のゲートとレーンを評価する。S4 にまだ無い宣言は「いま宣言された」として扱う（solo の cooling-off はこの回から数える見込みになる）。`--audit-log` が無ければ、reconcile と同じくすべてを通常予算で評価する。
- remediation schedule の窓の外（`remediation.outside_window: true`）では、reconcile は何も直さない（報告だけ）。plan はそのときも操作と予算の判定を示す。

## 全体

```json
{
  "schema_version": 1,
  "generated_at": "2026-10-27T14:40:31Z",
  "intent_revision": "a41d…",
  "rendered_revision": "a41d…",
  "error": "…",
  "claim": { … },
  "remediation": { … },
  "summary": { … },
  "operations": [ … ],
  "budget": { … },
  "capacity": { … },
  "planned_change_events": [ … ],
  "prune_halts": [ … ],
  "prune_deferred": [ … ],
  "workflow": [ … ],
  "pending_allocations": [ … ],
  "unobservable": [ … ],
  "composites": [ … ]
}
```

| フィールド | 型 | 意味 |
| --- | --- | --- |
| `schema_version` | 数 | この形の版。いまは `1` |
| `generated_at` | 文字列 | 出した時刻（UTC、RFC 3339） |
| `intent_revision` / `rendered_revision` | 文字列 | 文の出力の 1 行目と同じ版情報（§10.3）。`reconcile --expect-revision` に渡す値 |
| `error` | 文字列 | 計画ができなかった理由（終了コード 1 のときだけ）。そのとき他の中身は分かったところまで |
| `claim` | オブジェクト | `--claim` を渡したときだけ。`{"selector", "in_claim", "excluded"}`（文の出力では標準エラーの `--claim …: N resource(s) in claim, M excluded` の行） |
| `remediation` | オブジェクト | intent に remediation schedule があるときだけ（下記） |
| `summary` | オブジェクト | 件数（下記） |
| `operations` | 配列 | 操作（下記）。文の出力と同じ順（宣言の順の差分、続いて provider 名・ID の順の prune） |
| `budget` / `capacity` | オブジェクト | 変更予算・容量ゲートの判定 `{"halted", "halt", "reason"}`。`halt` は `all`（すべての変更を止める）か `destructive`（破壊的な変更だけ止める）。文の出力では `Budget: a reconcile now would halt …` / `Capacity: …` の行 |
| `planned_change_events` | 文字列の配列 | planned_change の知らせ（レーンに入った・使い切った・承認が足りない・冷却期間中など）。文の出力では標準エラーの `sakutei plan: PlannedChange…` の行 |
| `prune_halts` | 配列 | prune の安全装置が止めた provider（下記） |
| `prune_deferred` | 配列 | 宣言外にあるが、消せる型でない（`pruneable:false`）・観測できないなどで削除を見送ったもの（`{"id", "provider", "scope", "local_id"}`）。理由は標準エラーの `sakutei: prune defer …` の行 |
| `workflow` | 配列 | Workflow に任せている対象（下記） |
| `pending_allocations` | 配列 | 採番待ち（下記） |
| `unobservable` | 配列 | 観測できず計画に入らなかったリソース（下記） |
| `composites` | 配列 | 合成リソースの集約状態 `{"id", "status"}`（§9.7）。文の出力の `composite <id>: <status>` の行 |

## `remediation`

| フィールド | 意味 |
| --- | --- |
| `mode` | いまの時刻での remediation のモード：`auto` / `alert-only`（§14.2） |
| `reason` | どの窓（または schedule の既定）でそう決まったか |
| `outside_window` | `true` なら直してよい時間帯の外。reconcile しても報告だけで直さない |
| `line` | 文の出力の `Remediation mode now: …` の行 |

## `summary`

| フィールド | 意味 | 文の出力 |
| --- | --- | --- |
| `changes` | 操作の数（`operations` の要素数） | `Plan: N change(s)` |
| `create` / `update` / `delete` | 種類ごとの数 | — |
| `destructive` | 破壊的な操作の数 | `[DESTRUCTIVE]` |
| `blocked` | reconcile なら止まる操作の数 | `Blocked: K of N change(s) …` |
| `planned_change_lane` | planned_change のレーンに入る操作の数 | `lane:` の行 |
| `task` | `strategy: task` のリソースの操作の数（Apply されず、デーモンが Task を起こす） | `task:` の行 |
| `workflow` | Workflow に任せている対象の数 | `Workflow に任せる …: N target(s)` |
| `pending_allocations` | 採番待ちの数 | 標準エラーの `pending allocation:` の行 |
| `unobservable` | 観測できなかったリソースの数 | 標準エラーの `unobservable` の行 |
| `prune_halts` | prune を止めた provider の数 | 標準エラーの `prune halted` の行 |

## `operations[]`

| フィールド | 出るとき | 意味 |
| --- | --- | --- |
| `id` | 常に | 合成 ID（`provider/scope/local`、文の出力と同じ書き方） |
| `provider` / `scope` / `local_id` | 常に | ID の分解。`scope` は無ければ `{}` |
| `kind` | 常に | `CREATE` / `UPDATE` / `DELETE` |
| `destructive` | 常に | 破壊的か（予算の破壊的な変更に数える。文の出力の `[DESTRUCTIVE]`） |
| `prune` | 常に | 宣言外の削除（集合差 prune、§12）なら `true` |
| `absent` | `ensure: absent` の削除のとき | 宣言で消す（明示の absent、§12.2、Backlog #163）の削除なら `true`。planned_change（`kind: delete`）のレーンが開いているときだけ走り、外なら `blocked_by: approval` |
| `approval_required`・`approval_reason` | provider が承認の要る変更としたとき | provider が承認の要る変更とした操作（nftables の live の base chain の作成・type/hook/priority/policy の変更、Backlog #179）なら `true` と理由。その種類（`create`・`update`・`delete`）の planned_change のレーンが開いているときだけ走り、外なら `blocked_by: approval` |
| `cause` | 原因が分かるとき | 原因タグ `A`〜`E`（§10.2） |
| `diagnostic` | あるとき | diff エンジンの理由の文（文の出力の `reason:`） |
| `fields` | 差分があるとき | `{"field", "from", "to"}` の配列。`from` が現実（無いときは空文字）、`to` が宣言（消すときは空文字）。secret は参照の形。採番待ちのフィールドは `<pending allocation from pool …>` |
| `lane` | 常に | 使う予算のレーン：`normal`（通常予算）/ `planned_change`（§13.3） |
| `planned_change` | `lane` が `planned_change` | どの宣言のレーンか（`planned_change(scope=… kind=… window=…)`） |
| `blocked` | 常に | reconcile なら止まるか |
| `blocked_by` | `blocked` | 止める仕組み：`budget`（変更予算）/ `capacity`（容量ゲート）/ `alert_only_scope`（`remediation: alert-only` を宣言した scope、§14.3）/ `approval`（`ensure: absent` の削除で、それを覆う planned_change（`kind: delete`）のレーンが開いていない。provider が承認の要る変更とした操作で、その種類のレーンが開いていない（Backlog #179）。承認・窓・cooling-off、§13.3・§14.4・§14.5）。デーモンの `GET /plan`（[read-api.md](read-api.md)）では、ほかに `alert_only`（全体の remediation mode が alert-only：`--mode alert-only`・`--freeze`・schedule の窓の外、§14.1〜§14.3）と `prune_halt`（prune の安全装置が止めた宣言外の削除、§12.4）、`apply_stopped`（Apply を止めたリソースの操作：同じ Diff の Apply が成功を返しても現実が変わらないまま上限に達した、§11.1 Tier3、#166）も出る |
| `block_reason` | `blocked` | 止める理由の文（文の出力の `blocked:` の行） |
| `task` | `strategy: task`・`strategy: manual` のリソースの CREATE・UPDATE | `{"window", "rollback_on_fail"}`（§6.2）。Apply されず、デーモンが窓の中で Task を起こす。`strategy: manual`（人の手順で合わせる Task、#150）なら `"manual": true` と `"procedure"`・`"procedure_url"` も付く |

## `prune_halts[]`

| フィールド | 意味 |
| --- | --- |
| `provider` | provider 名 |
| `reason` | 止めた理由（List のエラー・List が完全でない・件数の急減など、§12.4） |
| `count_drop` | 件数の急減（§12.4（b））で止めたなら `true` |
| `held_orphans` | 止めたことで消さずに残した宣言外の数（List が使えたときだけ分かる） |

止めた provider の宣言外の削除は `operations` に一つも出ない。

## `workflow[]`

`id`・`provider`・`scope`・`local_id` に加えて：

| フィールド | 意味 |
| --- | --- |
| `workflow` | 任せている Workflow の名前 |
| `stage` | run の段階（`--workflow-store` が無ければ `stage unknown (no --workflow-store)`） |

## `pending_allocations[]`

`id`・`provider`・`scope`・`local_id` に加えて：

| フィールド | 意味 |
| --- | --- |
| `field` / `pool` | 採番を待つフィールドと pool |
| `free_known` | pool の空きを数えられたか |
| `free` / `pending_in_pool` | 空きの数と、同じ pool の採番待ちの数（`free_known` のときだけ意味がある。`free` が 0 なら reconcile でも採番できない） |
| `error` | allocator に届かなかったときの理由 |
| `line` | 標準エラーの `pending allocation:` の行 |

## `unobservable[]`

`id`・`provider`・`scope`・`local_id` に加えて `availability`（`unknown` / `converging`）。観測できないものは「無い」とみなさず（§3）、計画にも prune の候補にも入れない。
