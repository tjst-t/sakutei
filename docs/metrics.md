# デーモンの metrics（`/metrics`）

Backlog #54（設計書 §27.5 の Prometheus exporter）。常駐の `sakutei` が、収束の状態を Prometheus の text exposition format（0.0.4）で出す。client ライブラリは使わず stdlib だけで書く（`internal/metrics`）。

時系列の保存・監視・通知は作らない（§27.6）。Prometheus に集めさせ、通知は Alertmanager に任せる。

## 待ち受け

```console
$ sakutei --intent intent/ --metrics-listen 127.0.0.1:9470
sakutei: metrics listening on http://127.0.0.1:9470/metrics (Prometheus text format; no authorization)
```

- **明示したときだけ**待ち受ける。`--metrics-listen` を書かなければポートを開かない。
- 出すのは `/metrics` だけ（GET・HEAD）。認可は無い（承認 API と同じ）。信頼できるアドレスに縛るか、前に認証つきの proxy を置く。
- 一回性の `plan`・`reconcile`・`status` には無い（状態を出すのは回り続けるデーモン）。

## 出す値

値は**最後に終わった周**のもの。最初の周が終わるまでは、周の値（`sakutei_resources` など）は出さない（値が無いのは 0 ではない。unknown ≠ absent）。

| 名前 | 型 | 意味 |
|---|---|---|
| `sakutei_cycles_total` | counter | 回した周の数 |
| `sakutei_cycle_errors_total` | counter | エラーで終わった周の数（intent が読めない・fencing の store に届かない など） |
| `sakutei_last_cycle` | gauge | 最後の周の番号（ログの `cycle N`） |
| `sakutei_last_cycle_end_timestamp_seconds` | gauge | 最後の周が終わった時刻（Unix 秒）。止まったデーモンの検出に使う |
| `sakutei_cycle_duration_seconds` | gauge | 最後の周の所要時間 |
| `sakutei_observe_duration_seconds` | gauge | 最後の周の Observe の所要時間（期日の来たリソースの Observe と Diff）。単一デーモンの天井の早期警戒線（§16） |
| `sakutei_resources{state=…}` | gauge | 最後の周のリソースの数。`desired`（claim と fencing の後）・`observed`・`drifted`（desired との差分がある。作るべきものも含む）・`unknown`（観測できない：読めない・availability unknown・provider が解決できない）・`converging`（Apply の途中・対象が収束中と答えた） |
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

## 形式のテスト

`internal/metrics.Parse` が text format を厳しく読み直す（TYPE が先・族の行が続いている・名前とラベルの文字・エスケープ・同じ系列が 2 度出ない・counter は `_total`）。単体テスト（`internal/metrics`）と受け入れテスト `[AC-54-1]`・`[AC-54-2]`（`tests/acceptance/metrics_test.go`）がこれで `/metrics` を確かめる。
