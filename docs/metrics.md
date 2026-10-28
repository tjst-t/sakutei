# デーモンの metrics（`/metrics`）

Backlog #54（設計書 §27.5 の Prometheus exporter）。常駐の `sakutei` が、収束の状態を Prometheus の text exposition format（0.0.4）で出す。client ライブラリは使わず stdlib だけで書く（`internal/metrics`）。

時系列の保存・監視・通知は作らない（§27.6）。Prometheus に集めさせ、通知は Alertmanager に任せる。

## 待ち受け

```console
$ sakutei --intent intent/ --metrics-listen 127.0.0.1:9470
sakutei: metrics listening on http://127.0.0.1:9470/metrics (Prometheus text format; no authorization)
```

- **明示したときだけ**待ち受ける。`--metrics-listen` を書かなければポートを開かない。
- 出すのは `/metrics` だけ（GET・HEAD）。
- **既定は手元（loopback）だけ。** loopback 以外（`0.0.0.0`・空の host の `:9470`・名前）を TLS なし・認可なしで待ち受けるのは `--metrics-insecure-listen` を付けたときだけ。付けなければ起動で断る。外から読ませるときは下の「TLS と認可」を設定する。
- 一回性の `plan`・`reconcile`・`status` には無い（状態を出すのは回り続けるデーモン）。

## TLS と認可（`--listen-auth-config`）

Backlog #161。デーモンを固い区画に置き、別の区画のコレクターが FW を越えて読みに来る構成のために、`/metrics` と読み取り専用の口（`--read-listen`、[read-api.md](read-api.md)）は TLS と認可を持てる。設定は 1 つのファイルに口ごとに書く（書式は intent と同じ JSON / YAML のサブセット）。

```yaml
secret_store: secrets.json              # file SecretBackend（相対パスはこのファイルから）
metrics:                                # --metrics-listen の口
  tls:
    cert: secretref:file/sakutei/listen_cert   # サーバの証明書（PEM。中間を続けてよい）
    key: secretref:file/sakutei/listen_key     # 鍵（PEM）
    min_version: "1.2"                  # "1.2"（既定）か "1.3"。それより古い版は受けない
  bearer_token: secretref:file/sakutei/metrics_token   # 読むだけのトークン
read:                                   # --read-listen の口（中身の書き方は同じ）
  tls:
    cert: secretref:file/sakutei/listen_cert
    key: secretref:file/sakutei/listen_key
  client_ca_file: clients-ca.pem        # クライアント証明書を確かめる CA（PEM の束）
  allowed_subjects:                     # 任意。CN か subject の全体（CN=…,O=…）
    - grafana
  allowed_sans:                         # 任意。DNS 名・IP・URI・email の SAN
    - dash.example
```

```console
$ sakutei --intent intent/ --listen-auth-config listen-auth.yaml     --metrics-listen 192.0.2.10:9470 --read-listen 192.0.2.10:9471
sakutei: metrics listening on https://192.0.2.10:9470/metrics (Prometheus text format; TLS 1.2+; authorization: bearer token)
sakutei: read-only API listening on https://192.0.2.10:9471 (GET /status /plan /events /graph; TLS 1.2+; authorization: client certificate)
```

- **秘密は SecretRef だけ。** 証明書・鍵・トークンを設定に生で書くとエラー（値は文言にも出さない）。CA は秘密でないのでファイル。未知のキーはエラー。
- **認可はトークン・クライアント証明書のどちらか、または両方**（両方を書けば両方が要る）。
  - トークン：`Authorization: Bearer <token>`。比べるのは定数時間（両方の SHA-256 を比べ、値も長さも時間に出さない）。
  - クライアント証明書：`client_ca_file` の CA で署名され、用途に clientAuth があり、期限内であること。`allowed_subjects`・`allowed_sans` のどちらかを書けば、そのどれかに合うものだけ通す（両方空なら CA の署名だけ）。`client_ca_file` は `tls` と一緒でなければ書けない。
- **証明書とトークンは再起動なしで入れ替わる。** 30 秒ごとに金庫を読み直し、値が変わっていれば次の接続（トークンは次の要求）から新しいものを使う。読めない・証明書と鍵が組にならないときは前のものを使い続け、ログに 1 行出す。起動のときに読めなければ起動しない。CA の束は起動のときだけ読む（入れ替えは再起動）。
- **外向きの口は TLS と認可の両方が要る。** loopback 以外で TLS か認可のどちらかが欠ければ、`--metrics-insecure-listen`（読み取りの口は `--read-insecure-listen`）が無い限り起動で断る（トークンだけで TLS が無い、も断る）。設定に書いた口を `--*-listen` で開かない・旗だけ渡す、も起動で断る。
- **断ったとき**は理由を外に出さない：401 `{"error":"unauthorized"}`（資格が無い・トークンが違う。トークンを使う口は `WWW-Authenticate: Bearer`）、403 `{"error":"forbidden"}`（証明書の CA が違う・期限切れ・許していない相手）。TLS の口に平文で来たものは net/http が 400 を返す。理由と相手はデーモンのログ（標準エラー）に出す。
- **承認の書き込みの口（`--approval-listen`）とは分ける。** この設定は承認の口に効かず、承認の口を読み取りの口と同じアドレスに置くと起動で断る。読み取りの資格では書き込めない（`/metrics`・読み取りの口は書き込みのメソッドを 405 で断る）。

Prometheus の設定の例（トークンと CA）:

```yaml
scrape_configs:
  - job_name: sakutei
    scheme: https
    authorization: {credentials_file: /etc/prometheus/sakutei-token}
    tls_config: {ca_file: /etc/prometheus/sakutei-ca.pem}
    static_configs: [{targets: ["192.0.2.10:9470"]}]
```

## 出す値

値は**最後に終わった周**のもの。最初の周が終わるまでは、周の値（`sakutei_halted` など）は出さない（値が無いのは 0 ではない。unknown ≠ absent）。

### Observe まで届かなかった周

周は Observe に入る前に終わることがある。外部入力に届かず前回値も無いので飛ばした周、inventory の halt、intent が読めない、render の失敗、fencing の store に届かない、などである。こうした周は現実について何も確かめていない。その周のリソースの数を 0 として出すと「ずれは 0 件」と読める。すると `drifted > 0` で鳴らしている alert が、ずれが直っていないまま resolved になってしまう。

そこで、**観測の値（`sakutei_resources{state=…}` と `sakutei_observe_duration_seconds`）は、Observe まで届いた直前の周の値を残す**。系列を消す方式は採らない。消すと、Prometheus の上ではやはり alert が resolved になるからである。

- どの周の値かは `sakutei_last_observe_cycle`・`sakutei_last_observe_end_timestamp_seconds` に出る。`sakutei_last_cycle` より古ければ、その後の周は観測していない。観測が止まったことは、例えば `time() - sakutei_last_observe_end_timestamp_seconds` で見張れる。
- 一度も Observe まで届いていなければ、観測の値は出さない（0 とも出さない）。
- 周の止まり方（`sakutei_halted{gate=…}`・`sakutei_last_cycle_error` など）は、観測しなかった周でもその周の値を出す。

| 名前 | 型 | 意味 |
|---|---|---|
| `sakutei_cycles_total` | counter | 回した周の数 |
| `sakutei_cycle_errors_total` | counter | エラーで終わった周の数（intent が読めない・fencing の store に届かない など） |
| `sakutei_last_cycle` | gauge | 最後の周の番号（ログの `cycle N`） |
| `sakutei_last_cycle_end_timestamp_seconds` | gauge | 最後の周が終わった時刻（Unix 秒）。止まったデーモンの検出に使う |
| `sakutei_cycle_duration_seconds` | gauge | 最後の周の所要時間 |
| `sakutei_last_observe_cycle` | gauge | Observe まで届いた最後の周の番号。下の 2 つはこの周の値 |
| `sakutei_last_observe_end_timestamp_seconds` | gauge | Observe まで届いた最後の周が終わった時刻（Unix 秒） |
| `sakutei_observe_duration_seconds` | gauge | Observe まで届いた最後の周の Observe の所要時間（期日の来たリソースの Observe と Diff）。単一デーモンの天井の早期警戒線（§16） |
| `sakutei_resources{state=…}` | gauge | Observe まで届いた最後の周のリソースの数。`desired`（claim と fencing の後）・`observed`・`drifted`（desired との差分がある。作るべきものも含む）・`unknown`（観測できない：読めない・availability unknown・provider が解決できない）・`converging`（Apply の途中・対象が収束中と答えた。Apply のあとの確かめの settle の間を含む、#166）・`oscillating`（drifted のうち Apply を止めたもの：同じ Diff の Apply が成功を返しても現実が変わらないまま上限に達した、§11.1 Tier3、#166） |
| `sakutei_apply_ineffective_total` | counter | Apply が成功を返したのに、settle まで読み直しても Diff が Apply したときと同じまま残った回数（#166）。`increase(...)` が続けて上がる・`sakutei_resources{state="oscillating"}` が 0 でないときは、BMC などが命令を受けて何もしていない |
| `sakutei_halted{gate=…}` | gauge | 最後の周を止めた gate に 1。`budget`（§13.1）・`capacity`（§13.4）・`prune`（§12.4）・`inventory`（§9.2）・`external_input`（外部入力に届かず前回値も無いので周を飛ばした、§9.2） |
| `sakutei_held_ops` | gauge | 最後の周で gate が止めた op の数 |
| `sakutei_alert_only` | gauge | 最後の周が alert-only か freeze なら 1（§14.1） |
| `sakutei_last_cycle_error` | gauge | 最後の周がエラーで終わったら 1 |
| `sakutei_consecutive_error_cycles` | gauge | エラーで終わった周の連続回数（`--daemon-status` と同じ） |
| `sakutei_consecutive_unreachable_skips` | gauge | 外部入力に届かず飛ばした周の連続回数（同上） |
| `sakutei_budget_destructive_window_used` | gauge | 累積予算の窓の中で数えた破壊的な op の数（§13.2） |
| `sakutei_budget_destructive_window_limit` | gauge | 窓の中で許す破壊的な op の数（`--max-destructive-per-window`。0＝無制限なら出さない） |
| `sakutei_budget_destructive_window_remaining` | gauge | 窓の中の残り（limit − used。負にはしない。無制限なら出さない） |
| `sakutei_budget_window_seconds` | gauge | 窓の長さ（`--window-duration`、既定 24h） |
| `sakutei_budget_destructive_cycle_limit` | gauge | 1 周で許す破壊的な op の数（`--max-destructive-per-cycle`。無制限なら出さない） |
| `sakutei_budget_changes_cycle_limit` | gauge | 1 周で許す op の数（`--max-changes-per-cycle`。無制限なら出さない） |

### 通知の口（#154）

`--event-webhook`・`--event-alertmanager` があるとき、宛先ごとに（labels `sink`＝`webhook`／`alertmanager`、`url`＝宛先。資格と query は伏せる）送りの状態を出す。周の値ではなく、スクレイプした時点の値。数は要求（POST）の単位で、デーモンが起きてからのもの（webhook は 1 要求＝1 Event、Alertmanager は 1 要求＝その時点の alert の全部）。宛先ごとに送り直し・待ち・待ち行列を持つので、1 台が落ちていても他の宛先の数は進む。

| 名前 | 型 | 意味 |
|---|---|---|
| `sakutei_notify_requests_total{sink,url,result=…}` | counter | 送った要求の数。`delivered`（届いた）・`failed`（届かず、待ちを挙げて送り直した）・`rejected`（408・429 を除く 4xx。捨てた） |
| `sakutei_notify_dropped_total{sink,url}` | counter | 待ち行列が溢れて捨てた Event・alert の数 |
| `sakutei_notify_pending{sink,url}` | gauge | 送り待ち（webhook は Event、Alertmanager は 1 回だけ送る resolved・edge の alert）の数 |
| `sakutei_notify_up{sink,url}` | gauge | 最後の要求が届いていれば 1（まだ送っていなくても 1）、届かなかったら 0 |
| `sakutei_notify_consecutive_failures{sink,url}` | gauge | 続けて届かなかった要求の数 |
| `sakutei_notify_active_alerts{sink="alertmanager",url}` | gauge | その Alertmanager に送り直している firing の alert の数（Alertmanager だけ） |
| `sakutei_notify_last_success_timestamp_seconds{sink,url}` | gauge | 最後に届いた時刻（Unix 秒）。まだ一度も届いていなければ出さない |

Alertmanager を複数台で動かす構成では、`min by (url) (sakutei_notify_up{sink="alertmanager"}) == 0` で、どれか 1 台に届いていないことを見張れる（その間も他の Alertmanager には届いている）。

### 読み取りの口の断った数（#161）

`/metrics`・読み取りの口のうち開いた口ごとに（label `listener`＝`metrics`／`read`）、理由ごとの断った数を出す。開いた口は全部の理由を 0 から出す（`rate()` が使えるように）。

| 名前 | 型 | 意味 |
|---|---|---|
| `sakutei_listen_rejected_total{listener,reason=…}` | counter | 断った接続・要求の数。接続：`plaintext`（TLS の口に平文で来た）・`tls_handshake`（古い版・途中で切れたなど、握手が通らない）。401：`no_client_certificate`・`no_token`・`bad_token`。403：`bad_client_certificate`（CA が違う・期限切れ・用途が違う）・`client_not_allowed`（subject・SAN が一覧に無い） |

`rate(sakutei_listen_rejected_total{reason=~"bad_.*|client_not_allowed"}[5m]) > 0` で、違う資格で読みに来る相手を見張れる。

## 形式のテスト

`internal/metrics.Parse` が text format を厳しく読み直す（TYPE が先・族の行が続いている・名前とラベルの文字・エスケープ・同じ系列が 2 度出ない・counter は `_total`）。単体テスト（`internal/metrics`）と受け入れテスト `[AC-54-1]`〜`[AC-54-3]`（`tests/acceptance/metrics_test.go`）がこれで `/metrics` を確かめる。
