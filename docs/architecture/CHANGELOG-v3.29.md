# CHANGELOG v3.28 → v3.29

**改訂日: 2026-10-28**

通知の口（§22.3、#131）の宛先を複数にした改訂である（Backlog #154。親のストーリー「離れたホストのファイルと systemd の
ユニット（quadlet）を宣言で保つ・通知の口の冗長」の、通知の口の冗長の部分）。届ける口だけを持ち、振り分け・抑制は
送り先に委ねる（§22.1）という向きは変えていない。

## #154：通知の口の宛先を複数にする（§22.3）

- **背景**: Alertmanager を複数台で動かす構成では、送る側がすべての Alertmanager に同じ alert を送り、重複は
  Alertmanager どうしが消す。`--event-alertmanager` は宛先が 1 つで、その 1 台が落ちると知らせが途切れた。
- **決定**（具体は §22.3「v3.29 の決まり」）:
  - `--event-alertmanager`・`--event-webhook` はフラグを繰り返すか `,` でつないで複数書ける（1 つの書き方はそのまま）。
    `--event-notify-config` の `alertmanager.urls`・`webhook.urls` にも一覧で書け、フラグの宛先と合わせる（同じ宛先は 1 つに）。
    設定（headers・TLS・時間・labels）は全部の宛先に共通。
  - 宛先ごとに 1 つの sink（送り直し・待ち・送り待ち・firing の覚え）を持つ。1 台が落ちても他の宛先の送りを止めず、
    戻った宛先には送り待ちと firing の今を送る。一回性の `reconcile` の送り切りは全部の宛先で同時に行う。
  - 宛先ごとの送りの状態（要求の数：届いた・送り直した・4xx で捨てた・溢れて捨てた、送り待ち、続けて届かなかった数、
    最後に届いた時刻）を `/metrics` の `sakutei_notify_*{sink,url}` と、デーモンの健全性（`--daemon-status`・
    `GET /status`）の `notify` に出す。
- **見送り**:
  - 宛先ごとに別の設定（headers・TLS・labels）を持つ口。今は全部の宛先に同じ設定。要れば `urls` の要素を
    map にして足せる（今の書き方は文字列の一覧のまま通る）。
  - 宛先の重み付け・どれか 1 台に届けばよいという送り方（Alertmanager の HA の作法は「全部に送る」なので持たない）。

## 受け入れテスト

`tests/acceptance/event_notify_multi_test.go`（httptest の Alertmanager を 2 台）:

- `[AC-154-1]` 2 台の Alertmanager の両方に同じ firing の alert（同じ labels・startsAt）が届き、送り直しも両方に続く。
- `[AC-154-2]` 1 台を止めても（503）もう 1 台には firing・resolved が届き、止めた方の失敗は宛先ごとの状態（`--daemon-status`
  の `notify`・status の文）に出る。戻れば止めた方にも待っていた resolved が届き、両方の状態が戻る。
- `[AC-154-3]` `--event-alertmanager a,b` とフラグの繰り返しのどちらでも、一回性の `reconcile` が両方に送り切る。

単体（`internal/eventnotify/set_test.go`・`internal/metrics`）: URL の分け方、設定の `urls`、同じ宛先をまとめる、
宛先ごとの状態、落ちた宛先が閉じる時間を使い切っても届く宛先は送り切る、`sakutei_notify_*` の値。
