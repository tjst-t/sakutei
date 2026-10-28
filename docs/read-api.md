# デーモンの読み取り専用の口（`--read-listen`）

常駐デーモン（`sakutei`）が「いま何をしているか」を、外の道具（可視化・ダッシュボード）が読むための HTTP の口である（Backlog #136）。Sakutei 本体は画面を持たない。ここは読み取り専用の口を汎用に出すだけで、独自のデータも履歴も持たない（§2 の catalog の原則：UI は独自のデータを持たない薄いフロントエンド。履歴は外の道具が貯める、§22.1・§27.6）。

```console
$ sakutei --intent intent/ --read-listen 127.0.0.1:9471 ...
sakutei: read-only API listening on http://127.0.0.1:9471 (GET /status /plan /events /graph; no authorization)
```

| パス | 中身 | 形 |
| --- | --- | --- |
| `GET /status` | 直前に観測した周の適合レポート | `sakutei status --json`（[status-json.md](status-json.md)）+ 周の番号と時刻 |
| `GET /plan` | 直前に観測した周の計画（止めた操作と理由、保留を含む） | `sakutei plan --json`（[plan-json.md](plan-json.md)）+ 周の番号と時刻 |
| `GET /events` | Event の流し出し（Server-Sent Events） | Event の JSON（`--event-log` の 1 行と同じ）を 1 件ずつ |
| `GET /graph` | 依存の DAG（リソースと depends_on・ready_condition、今の ready かどうか、Apply の順） | `sakutei graph --json`（下記）+ 周の番号と時刻 |

- **待ち受けは明示したときだけ。** `--read-listen` を渡さなければ口は開かない。承認の書き込みの口（`--approval-listen`、§6.4）・`/metrics`（`--metrics-listen`、[metrics.md](metrics.md)）とは別の待ち受けで、それらはそのまま。
- **書き込みは受けない。** `GET`・`HEAD` 以外（`POST`・`PUT`・`PATCH`・`DELETE` など）は `405 Method Not Allowed`（`Allow: GET, HEAD`）。知らないパスは `404`。エラーの本文は `{"error": "…"}`。
- **収束を止めない。** 中身は収束ループが周の終わりに渡したものを返すだけで、口のために現実を観測し直さない（観測 state レスのまま：次の周はまた観測し直す）。遅い読み手のために収束ループが待つことはない（`/events` を見る）。
- **秘密の値は出さない。** 中身は `status --json`・`plan --json` と同じく、secret のフィールドは参照の形（`secretref:…#genN`）だけ（§20.2）。git の取得先の URL に埋め込まれた資格は伏せる。

## 認証は持たない（外に出すときは前段で守る）

口は認証も TLS も持たない。**手元（`127.0.0.1`）で待ち受ける前提**である。読み取り専用でも、中身には構成（リソースの ID・フィールドの値・ずれ・止めた理由）が入るので、そのまま外に開けない。

外の道具が別のホストにあるときは、前段に置く道具で守る：

- **ssh のポート転送**：`ssh -L 9471:127.0.0.1:9471 <デーモンのホスト>` で、読む側から手元の口に入る。いちばん簡単。
- **リバースプロキシ**（nginx・Caddy・Envoy など）：プロキシが TLS と認証（mTLS・Basic 認証・OIDC の前段など）を受け持ち、デーモンの `127.0.0.1:9471` に流す。`/events` は長く開いたままの応答なので、プロキシの応答のバッファを切り（nginx なら `proxy_buffering off;`）、読み取りのタイムアウトを延ばす（15 秒ごとにコメントの行を送るので、それより長くする）。
- 書き込みのメソッドはデーモンが断るが、プロキシでも `GET`・`HEAD` だけを通すと二重に守れる。

## 共通：周の番号と時刻

`/status`・`/plan`・`/graph` の JSON は、一回性の `--json` の形のトップレベルに次の 3 つを足したもの。

| フィールド | 意味 |
| --- | --- |
| `cycle` | 中身を作った（現実を観測した）周の番号（デーモンのログの `cycle N` と同じ） |
| `cycle_time` | その周が終わった時刻（UTC、RFC 3339） |
| `last_cycle` | デーモンが最後に回した周の番号。`cycle` より大きければ、その後の周は Observe まで進んでいない（外部入力に届かず飛ばした・intent が読めない・fencing のストアに届かないなど）。そのとき中身は最後に観測した周のまま（unknown ≠ absent、§3：観測できなかった周を「ずれなし」にしない） |

`generated_at` は応答を作った時刻。デーモンが起動してからまだ一度も観測していなければ `503 Service Unavailable`（`Retry-After: 1`）。

## `GET /status`

`sakutei status --json` と同じ形（[status-json.md](status-json.md)）。一回性の status との違い：

- 情報源はデーモン自身の周の観測（Observe → Diff）。`daemon` は常に出て、最後の周の健全性（`--daemon-status` に書くものと同じ）に、claim・fencing の世代・git の取得の状態が入る（あるときだけ、[status-json.md](status-json.md) の `daemon`）。`stale` は常に `false`。
- **宣言外（unmanaged）は数えない。** デーモンは status のために List をしない。`unmanaged` は 0、`unmanaged_undetectable` は空。宣言外を見るのは一回性の `sakutei status`。prune で消す宣言外は `/plan` の `prune: true` の操作に出る。
- 周の事情で観測しなかったリソースは `unobservable` に理由つきで出る：デーモンの Apply が進行中（`availability=converging (an Apply of this daemon is in flight)`）、採番待ち（`waiting-allocation …`）など。
- tier の間引き（§16.5）でその周に観測しなかったリソースは、最後に観測した周の項目をそのまま出す（表示だけの持ち越し。収束の判断には使わない）。一度も観測していなければ `unobservable`（`not observed yet …`）。
- `last_seen`（前の周との違い）はデーモンの last-seen リングから作る。

## `GET /plan`

`sakutei plan --json` と同じ形（[plan-json.md](plan-json.md)）。一回性の plan が「いま reconcile したら」の見込みを評価するのに対し、`/plan` は**その周に収束ループが実際に下した判定**である。

- `operations`：その周の差分（原因タグ C の ghost diff を含む）と、prune で消す宣言外の削除。止めたものは `blocked: true` と `blocked_by`・`block_reason`：`budget`（変更予算。累積の窓はデーモンが数えているもの）/ `capacity`（容量ゲート）/ `alert_only_scope`（`remediation: alert-only` の scope）/ `alert_only`（全体の remediation mode が alert-only）/ `prune_halt`（prune の安全装置、§12.4）/ `apply_stopped`（Apply を止めたリソース、oscillating、#166）。planned_change のレーンに入る操作は `lane: "planned_change"`。
- `remediation` は常に出る：その周の mode（`--mode`・`--freeze`・intent の schedule を most-restrictive-wins で決めたもの、§14.3）と理由。
- 保留：`workflow`（Workflow に任せている対象と段階）、`pending_allocations`（採番待ち）、`unobservable`（観測できなかったもの。`availability` が `converging` ならデーモンの Apply が進行中）、`prune_halts`・`prune_deferred`。
- `budget`・`capacity`・`planned_change_events` はその周の判定。
- 依存の上流が直っていない・ready_condition を満たしていないために、その周に Apply しなかった操作は止めたものとして書かない（ゲートで止めたのではない）。ready は `/graph` で見る。

## `GET /events`（Server-Sent Events）

```console
$ curl -N http://127.0.0.1:9471/events
: sakutei events

data: {"time":"2026-10-27T15:06:37.73Z","kind":"Drift","severity":"warning","cycle":4,"resource":"fake//web",…,"state":"firing","since":"…","count":4,"class":"transient"}

data: {"time":"…","kind":"Drift","severity":"info","resource":"fake//web",…,"state":"resolved",…}
```

- 1 つの SSE の event が Event 1 件。`data:` の行は Event の JSON をそのまま（`--event-log` の 1 行と同じ形、§22.3）で、`event:` の名前は付けない。流れるのは知らせ（Event）だけで、周の要約の行は流れない。
- **接続した時点で firing 中の level の Event を先に送る**（ずれ・収束の失敗・halt など、`state: "firing"`）。そのあとに起きた Event を順に流す。level の Event は firing / resolved で出る（毎周同じものは撒かない、§22.3）。
- 15 秒ごとにコメントの行（`: keepalive`）を送る。
- **遅い読み手で収束を止めない。** 読み手ごとに 256 件までためる。それを超えた分は落として数え、次に送るときに `event: dropped` と `data: {"dropped": N}`（その接続で落とした数の累計）を先に送る。落とした分は再送しない：読み手は `/status`・`/plan` を読み直して今の状態に合わせる。
- 履歴は持たない。つなぐ前の Event は返らない（firing 中のものを除く）。貯めるのは外の道具（`--event-log` のファイル・通知の先）。

## `GET /graph` と `sakutei graph --json`

依存の DAG。デーモンの `/graph` は直前に観測した周の ready の条件から作る。一回性の `sakutei graph` は intent を render して（`generated/` は書かない・採番は Lookup だけ）、ready_condition の上流だけを読むだけで観測する。フラグは status と同じ（`--intent`・`--state`・`--zone` など provider のファイル・`--claim`・`--inventory-file`・`--alloc-store`）。`--json` が無ければ wave と辺を文で出す。終了コードは 0 = グラフができた、1 = できなかった（`--json` なら `error` 入りの JSON を出す。フラグの読み取りのエラーと、起動の確かめで直接終わるものは出さない：plan と同じ）。

```json
{
  "schema_version": 1,
  "generated_at": "2026-10-27T15:06:37Z",
  "intent_revision": "85ff…",
  "rendered_revision": "85ff…",
  "claim": "site=a",
  "nodes": [
    {"id": "fake//svc-1", "provider": "fake", "scope": {}, "local_id": "svc-1",
     "depends_on": ["vm-1"], "observed": true, "ready": false,
     "waiting_for": "vm-1:cloud_init_done", "wave": 1},
    {"id": "fake//vm-1", "provider": "fake", "scope": {}, "local_id": "vm-1",
     "depends_on": [], "observed": true, "conditions": {"cloud_init_done": false},
     "ready": true, "wave": 0}
  ],
  "edges": [
    {"from": "vm-1", "to": "svc-1", "ready_condition": "cloud_init_done", "ready": false}
  ],
  "waves": [["vm-1"], ["svc-1"]],
  "summary": {"nodes": 2, "external": 0, "edges": 1, "waves": 2, "not_ready": 1}
}
```

| フィールド | 意味 |
| --- | --- |
| `claim` | `--claim`（デーモンはその周の claim）。無ければ出ない |
| `nodes[]` | リソース（`local_id` の昇順）。`depends_on` は上流の `local_id`（宣言の順） |
| `nodes[].external` | 宣言の外の上流（ready_condition の相手として観測だけするもの、§7 external owner）。下流の provider で観測する。`wave` は `-1` |
| `nodes[].observed` | ready の条件を読めた（Availability `ok`）こと。一回性の graph は ready_condition の上流だけを観測する |
| `nodes[].conditions` | 観測した ready の条件（名前 → 満たしているか）。`observed` が `false` なら出ない |
| `nodes[].ready` | ready_condition 付きの上流がすべて条件を満たしていること。`false` なら収束ループはこのリソースとその下流をその周に進めない（§8.3）。観測できない上流の条件は満たしていない側に数える（unknown ≠ ready） |
| `nodes[].waiting_for` | 満たしていない最初の条件（`<上流>:<条件>`） |
| `nodes[].wave` | `waves` の中の位置（0 から） |
| `edges[]` | 依存 1 本（`from` が上流、`to` が下流。`to` の昇順、その中は depends_on の順）。`ready_condition` が無い辺は順序だけの依存で、`ready` も出ない |
| `waves` | Apply の順（Kahn の wave、各 wave の中は `local_id` の昇順。収束ループと同じ順） |
| `summary` | 件数。`not_ready` は `ready: false` のリソースの数 |

`schema_version` はフィールドを足すだけなら上げない。読む側は知らないフィールドを無視する。
