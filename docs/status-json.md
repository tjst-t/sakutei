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
| `unmanaged` | 宣言外・実在（prune 対象外） | `unmanaged=` |
| `workflow` | Workflow に任せている（収束ループは触らない、§6.3） | `workflow=` |
| `list_errors` | List がエラーだった provider の数（観測できない側に数える） | — |
| `total` | `conformant + drift + unobservable + unmanaged + workflow`。`resources` の要素数と同じ | — |

## `resources[]`

| フィールド | 出るとき | 意味 |
| --- | --- | --- |
| `id` | 常に | 合成 ID（`provider/scope/local`、文の出力と同じ書き方） |
| `provider` | 常に | provider 名 |
| `scope` | 常に | scope のラベル（無ければ `{}`） |
| `local_id` | 常に | provider 内の ID |
| `state` | 常に | `conformant` / `drift` / `unobservable` / `unmanaged` / `workflow`。文の出力の converged / `DRIFTED` / `UNOBSERVABLE` / `UNMANAGED` / `WORKFLOW` に当たる |
| `missing` | `drift` で現実に無いとき | `true`（文の出力では `DRIFTED` のまま） |
| `cause` | `drift` | 原因タグ `A`〜`E`（§10.2） |
| `fields` | `drift` | ずれたフィールドの配列。`{"field", "from", "to"}`、`from` が現実・`to` が宣言（無いときは空文字）。secret は参照の形（§20.2） |
| `diagnostic` | `drift` | diff エンジンの理由の文 |
| `last_seen` | `drift` | last-seen リング由来の「前周は X だった」（S3・非永続） |
| `tier` | `drift` | provider 階級 `{"true_observe", "list", "bulk_observe", "pruneable", "provider_version"}` |
| `observe_error` | `unobservable` | Read のエラー文（`observe error: …`）、または `availability=…` |
| `workflow` / `workflow_stage` | `workflow` | 任せている Workflow の名前と run の段階 |

## `unmanaged_undetectable[]`

宣言外の検出ができなかった provider（provider 名の昇順）。検出できないことを「宣言外ゼロ」と読まないためにある。

| フィールド | 意味 |
| --- | --- |
| `provider` | provider 名 |
| `kind` | `list_unsupported`（List 非対応）/ `list_error`（List がエラー。`complete` が `false` になる）/ `list_incomplete`（List が完全でない） |
| `reason` | エラーの文、または `status=…`（`list_unsupported` では出ない） |
| `note` | 文の出力に出る注記の行 |

## `daemon`

`--daemon-status` のファイル（`internal/statusreport.DaemonHealth`）と同じフィールド（`cycle`・`consecutive_unreachable_skips`・`unreachable_skip_limit`・`consecutive_error_cycles`・`last_error`・`interval`・`updated_at`）に、次の 2 つを足したもの。

| フィールド | 意味 |
| --- | --- |
| `prolonged` | 連続スキップが上限に達している（ExternalInputUnreachableProlonged、§9.2） |
| `stale` | ファイルが周期の数倍更新されていない（デーモンが止まっているかもしれない） |
