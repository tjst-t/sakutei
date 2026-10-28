# `sakutei status --json` の形（schema_version 1）

`sakutei status --json` は、適合レポート（§26.4）を標準出力に **JSON を 1 つ**出す（#118）。status の結果を外部の道具で機械的に集めるための形である。中身は文の出力と同じ `internal/statusreport.Report` から作るので、件数・各リソースの状態・理由は文の出力と一致する。

- 標準エラーへの出力（リソースごとの観測エラーなど）と**終了コードは文の出力と同じ**（0 = すべて観測できた、3 = 観測できないものがある、1 = 報告そのものができない）。終了コード 1 のときは JSON を出さない。
- `schema_version` はフィールドを足すだけなら上げない。意味を変える・消すときに上げる。読む側は知らないフィールドを無視する。
- 値が無いことの多いフィールド（`missing`・`cause`・`fields`・`observe_error` など）は、そのリソースの状態に関係しないときは出さない。

## 全体

```json
{
  "schema_version": 1,
  "generated_at": "2026-10-27T08:18:54Z",
  "intent_revision": "520e…",
  "rendered_revision": "520e…",
  "complete": true,
  "summary": { … },
  "resources": [ … ],
  "unmanaged_undetectable": [ … ],
  "daemon": { … },
  "daemon_note": "…"
}
```

| フィールド | 型 | 意味 |
| --- | --- | --- |
| `schema_version` | 数 | この形の版。いまは `1` |
| `generated_at` | 文字列 | 出した時刻（UTC、RFC 3339） |
| `intent_revision` / `rendered_revision` | 文字列 | 文の出力のヘッダと同じ版情報（§10.3） |
| `complete` | 真偽 | 観測できないもの（リソース、または List がエラーの provider）が無いこと。`false` なら終了コード 3 |
| `summary` | オブジェクト | 件数（下記） |
| `resources` | 配列 | リソースごとの状態（下記）。`id` の昇順 |
| `unmanaged_undetectable` | 配列 | 宣言外の検出ができなかった provider とその理由（下記）。無ければ空の配列 |
| `daemon` | オブジェクト | `--daemon-status` を読めたときだけ。常駐デーモンの健全性（#15） |
| `daemon_note` | 文字列 | `--daemon-status` を読めなかったときの注記 |

## `summary`

文の出力の「要約:」の行と同じ数。

| フィールド | 意味 | 文の出力 |
| --- | --- | --- |
| `conformant` | 適合（drift なし） | `converged=` |
| `drift` | drift（現実に無いものを含む） | `drifted=` |
| `missing` | drift のうち、宣言されているのに現実に無いもの（`drift` に含む） | — |
| `unobservable` | 観測できない（Read のエラー、または Availability が `ok` でない）。drift とも適合とも数えない（§3） | `unobservable=` |
| `unmanaged` | 宣言外・実在（見せるだけで消さない）。provider の型が prune する型かどうかに関わらず、宣言が 1 つも無くても設定されている provider（`--provider-config` に節がある、またはその provider だけのフラグ）の現実を含む（#164） | `unmanaged=` |
| `workflow` | Workflow に任せている（収束ループは触らない、§6.3） | `workflow=` |
| `oscillating` | Apply を止めたリソース（同じ Diff の Apply が成功を返しても現実が変わらないまま上限に達した、§11.1 Tier3、#166）。デーモンの `GET /status` だけが数える（一回性の `status` は常に 0） | `oscillating=`（あるときだけ） |
| `list_errors` | List がエラーだった、または List の ID の scope が宣言と食い違った（#169）provider の数（観測できない側に数える） | — |
| `total` | `conformant + drift + unobservable + unmanaged + workflow + oscillating`。`resources` の要素数と同じ | — |

## `resources[]`

| フィールド | 出るとき | 意味 |
| --- | --- | --- |
| `id` | 常に | 合成 ID（`provider/scope/local`、文の出力と同じ書き方） |
| `provider` | 常に | provider 名 |
| `scope` | 常に | scope のラベル（無ければ `{}`） |
| `local_id` | 常に | provider 内の ID |
| `state` | 常に | `conformant` / `drift` / `unobservable` / `unmanaged` / `workflow` / `oscillating`。文の出力の converged / `DRIFTED` / `UNOBSERVABLE` / `UNMANAGED` / `WORKFLOW` / `OSCILLATING` に当たる。`oscillating`（#166）はデーモンの `GET /status` だけに出る |
| `missing` | `drift` で現実に無いとき | `true`（文の出力では `DRIFTED` のまま） |
| `cause` | `drift` | 原因タグ `A`〜`E`（§10.2） |
| `fields` | `drift` | ずれたフィールドの配列。`{"field", "from", "to"}`、`from` が現実・`to` が宣言（無いときは空文字）。secret は参照の形（§20.2） |
| `diagnostic` | `drift` | diff エンジンの理由の文 |
| `last_seen` | `drift` | last-seen リング由来の「前周は X だった」（S3・非永続） |
| `tier` | `drift` | provider 階級 `{"true_observe", "list", "bulk_observe", "pruneable", "provider_version"}` |
| `observe_error` | `unobservable` | Read のエラー文（`observe error: …`）、または `availability=…` |
| `workflow` / `workflow_stage` | `workflow` | 任せている Workflow の名前と run の段階 |
| `apply_stopped` | `oscillating` | Apply を止めたわけ（#166）`{"diff", "ineffective_applies", "max_applies", "settle", "reason", "note", "since"}`。`diff` は止めたときの Diff（`power: "Off" → "On"` など）、`reason` は `max_ineffective_applies` か `max_ineffective_duration`、`since` は止めた時刻。解くのは Diff の中身が変わる・現実が宣言どおりになる・`sakutei apply-check release` |

## `unmanaged_undetectable[]`

宣言外の検出ができなかった provider（provider 名の昇順）。検出できないことを「宣言外ゼロ」と読まないためにある。設定されているが宣言の無い provider が組み立てられなかったとき（設定のファイルが読めないなど）も `list_error` で出る（#164）。

| フィールド | 意味 |
| --- | --- |
| `provider` | provider 名 |
| `kind` | `list_unsupported`（List 非対応）/ `list_error`（List がエラー。`complete` が `false` になる）/ `list_incomplete`（List が完全でない）/ `scope_mismatch`（List の ID の scope が宣言の scope と食い違う。`complete` が `false` になり `list_errors` に数える、#169）/ `scope_undecided`（`--claim` のとき、宣言外の ID の scope が決まらず claim の内外を決められなかった。`complete` は変えない、#169） |
| `reason` | エラーの文、`status=…`、食い違いの中身（`scope_mismatch`）、または `count=N`（`scope_undecided`）。`list_unsupported` では出ない |
| `note` | 文の出力に出る注記の行 |

## `daemon`

`--daemon-status` のファイル（`internal/statusreport.DaemonHealth`）と同じフィールド（`cycle`・`consecutive_unreachable_skips`・`unreachable_skip_limit`・`consecutive_error_cycles`・`last_error`・`interval`・`updated_at`）に、次の 2 つを足したもの。

| フィールド | 意味 |
| --- | --- |
| `prolonged` | 連続スキップが上限に達している（ExternalInputUnreachableProlonged、§9.2） |
| `stale` | ファイルが周期の数倍更新されていない（デーモンが止まっているかもしれない） |

デーモンの担当と配布の状態（#136）。あるときだけ出る。文の出力ではデーモンの行の下に `claim:`・`shard:`・`fencing:`・`mirror:` の行で出る。デーモンの `GET /status`（[read-api.md](read-api.md)）の `daemon` も同じ形（`stale` は常に `false`）。

| フィールド | 出るとき | 意味 |
| --- | --- | --- |
| `claim` | 担当を絞っているとき | このデーモンの担当（scope-selector、§17.1）。`--claim`、または git の配布の `shards/<名前>.yaml`（DR plan で移ったときはその claim、§25.1） |
| `idle` | git の配布で担当が無いとき | `true`：このシャードは今何も担当していない（何も収束しない、§17.7） |
| `shard` | `--fencing-store` のとき | fencing の holder（`--shard-id`） |
| `fencing` | `--fencing-store` のとき | scope ごとの世代 `{"scope", "generation", "holder", "fenced"}`（scope の順）。`fenced: true` は別のシャードが新しい世代を持っていて、このデーモンはその scope に書かない（§15.2）。そのときの `generation`・`holder` はストアの現在の世代 |
| `mirror` | git の配布（`--intent-repo`）のとき | intent の取得の状態 `{"remote", "revision", "fetched_at", "reachable", "error"}`。`revision` は最後に取れた（手元の写しにある）コミット、`fetched_at` は最後に取得が成功した時刻（まだ無ければ出ない）、`reachable` は直近の取得が届いたか（中央・ミラーに届いているか）。`false` のあいだは手元の写しで収束を続ける（§17.4）。`remote` の URL に埋め込まれた資格は伏せる |
| `apply_stopped` | Apply を止めたリソースがあるとき（#166） | `[{"resource", "diff", "ineffective_applies", "max_applies", "settle", "reason", "note", "since"}]`（ID の順）。同じ Diff の Apply が成功を返しても現実が変わらないまま上限に達し、デーモンが Apply を止めているリソース（oscillating、§11.1 Tier3）。文の出力ではデーモンの行の下に `警告: Apply を止めている` と `OSCILLATING` の行で出る。status の終了コードは変えない |
| `runtime_tightened` | 起動の旗が intent の `runtime:` を締めているとき（#183） | `[{"key", "flag", "flag_value", "intent_value", "file"}]`。旗が宣言より締める向き（予算が小さい・mode が弱い）で、旗の値が効いている一時的な締め（§26.7）。文の出力では `注意: 起動の旗が intent の runtime: を一時的に締めている` と `TIGHTENED` の行で出る |
| `runtime_changed` | 起動したあとに intent の `runtime:` が変わったとき（#183） | 違いの文（`max_changes_per_cycle: started with 20, now 5` を `; ` でつなぐ）。デーモンは起動したときの値で回り、起こし直すと取り込む |
| `notify` | 通知の口（`--event-webhook`・`--event-alertmanager`）があるとき | 宛先ごとの送りの状態（#154、開いた順）`{"sink", "url", "delivered", "failed", "rejected", "dropped", "pending", "active", "consecutive_failures", "last_success", "last_failure", "last_error"}`。数は要求（POST）の単位でデーモンが起きてからのもの（`delivered` 届いた・`failed` 届かず送り直した・`rejected` 4xx で捨てた・`dropped` 待ち行列が溢れて捨てた）。`active` は Alertmanager に送り直している firing の alert の数（webhook では出ない）。`consecutive_failures` が 0 でなければ、その宛先に今届いていない（他の宛先へは送り続けている）。文の出力では `notify:` の行と、届いていない宛先の警告の行で出る。`url` の資格と query は伏せる |
