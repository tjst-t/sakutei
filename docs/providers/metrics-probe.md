# /metrics の値を観測する provider（`metrics-probe`）

HTTP(S) の /metrics（Prometheus の text 形式）を取り、宣言したメトリクスの値を**観測するだけ**の provider（Backlog #149）。
使い道は、全体を止める型の更新の前に上流の積もり（キューの長さ・待ちの件数）を確かめること：Workflow の `require`（#87）や
ready_condition（§8.3）から「値が 1000 未満なら進める」を書く。
決まりの正は設計書 §29.1「/metrics の値を観測する口」。ここは使い方の説明で、矛盾したら spec/ が優先する。

- 観測だけ（`Capabilities.ObserveOnly`）。Apply は常に断る。List・BulkObserve は持たない（prune とは関係しない）。
- 1 回の Read は 1 回の GET。観測を周をまたいで持たない。

## 設定

CLI には専用のフラグを置かず、`--provider-config` の `metrics-probe` の `metrics_probe_config` で渡す（§28.9）。

```yaml
# providers.yaml
providers:
  metrics-probe:
    metrics_probe_config: /etc/sakutei/probes.yaml
```

```yaml
# probes.yaml
secret_store: secrets.json          # 資格を使うときだけ（file SecretBackend、§20.5）
endpoints:
  mq:                               # endpoint の名前
    url: https://mq1.example.net:15692/metrics
    ca_file: mq-ca.pem              # 省略可（この CA だけを信頼する）
    # insecure_skip_verify: true    # 明示したときだけ（既定 false。ca_file と排他）
    bearer_token: secretref:file/metrics/mq/token   # 省略可
    # username: sakutei             # basic 認証（password と組。bearer_token と排他）
    # password: secretref:file/metrics/mq/password
    timeout: 10s                    # 省略可（1 回の取得の上限、既定 10s）
probes:
  orders-queue:                     # probe の名前 = リソースの ID
    endpoint: mq
    metric: rabbitmq_queue_messages
    labels:                         # 省略可（一致させるラベル）
      vhost: /
      queue: orders
    aggregate: sum                  # 省略可（sum | max）
    ready:                          # 省略可（条件の名前 → 比べ方）
      below_limit: "< 1000"
```

- 相対パス（`secret_store`・`ca_file`）はこの設定ファイルのあるディレクトリから。
- `url` はパスまで丸ごと書くので、API のパスの前置きを変える欄（proxmox-vm・s3-bucket の `api_base_path`、Backlog #160）は持たない。実機と模擬で前置きが違えば `url` を変える。
- 資格は SecretRef でしか書けない（生の値は拒否。URL に `user:pass@` を書いても拒否）。値は取得の直前に解決する。
- `ca_file`・`insecure_skip_verify` は https の URL のときだけ書ける。
- 未知のキーは拒否する（書き間違いを黙って無視しない）。

### ラベルの一致と集計

- `labels` に書いたラベルがすべて等しい系列が一致する。書かないラベルは問わない。値 `""` は「そのラベルが無い」
  （Prometheus と同じく、空の値のラベルは無いのと等しい）。
- `aggregate` を省くと、一致はちょうど 1 本でなければならない（2 本以上なら観測できない：ラベルを絞るか集計を書く）。
- `sum` は一致した系列の和、`max` は最大。

### 比べ方の書き方

`"<演算子> <数>"`（間の空白は省ける）。

| 書き方 | 意味 |
| --- | --- |
| `"< 1000"` | 1000 未満 |
| `"<= 1000"`・`"≤ 1000"` | 1000 以下 |
| `"> 0"` | 0 を超える |
| `">= 5"`・`"≥ 5"` | 5 以上 |
| `"= 0"`・`"== 0"`・`"0"` | 0 と等しい |

数は有限の 10 進（`1e3`・`0.5` 可）。NaN・±Inf・16 進・`_` 区切りは書けない。`!=` は無い（`< x` と `> x` の 2 つの条件に分ける）。

## ID とフィールド

ID は probe の名前（`orders-queue`）。

| フィールド | 中身 |
| --- | --- |
| `probe` | probe の名前（= ID、identity） |
| `metric` | メトリクスの名前 |
| `value` | 観測の値（集計したもの。最短の 10 進、例 `1200`・`999.5`） |
| `series` | 一致した系列の数 |
| `ready.<名前>` | 比べ方の結果（`"true"` / `"false"`） |

比べ方の結果は `Observation.Ready[<名前>]` にも出る（ready_condition が読む）。

## 使い方

### Workflow の require（#87）

```yaml
workflows:
  - workflow: storage-firmware
    target:
      provider: redfish-system
    stages:
      - name: all
        targets: [array1]
        gate:
          manual_approval: required
          timeout: 4h
    steps:
      - require:                     # 上流のキューが 1000 未満になるまで待つ
          resource: metrics-probe/orders-queue
          fields:
            ready.below_limit: "true"
          timeout: 30m               # 過ぎれば halt（止めない）
      - apply:
          power: "Off"
```

probe の名前は設定の `probes` に書いたものだけ（`{target}` を使うなら、probe の名前を対象の名前にそろえる）。

### ready_condition（§8.3）

probe を intent の resources に置き、下流の `depends_on` で条件の名前を待つ。

```yaml
resources:
  - provider: metrics-probe
    id: orders-queue                 # fields は書かない（書けるのはこの probe の ready.<名前> だけ）
  - provider: fake
    id: app
    fields:
      name: app
      size: "1"
    depends_on:
      - id: orders-queue
        ready_condition: below_limit
```

下流は、その周の新しい観測で条件が真のときだけ Apply される（周をまたいで持たない）。
`fields: { ready.below_limit: "true" }` を書けば、偽の間は status・plan にずれとして出る（Apply はしない）。

## 観測できないとき

次はどれも**観測できない（unknown）**で、absent にしない（§3）。Workflow は待ち（待ちの理由に訳を添える）、`timeout` で halt する。
ready_condition の下流は待つ。

- 届かない・時間切れ・TLS の確かめに失敗した
- 2xx 以外の応答（401・503 など）。転送（3xx）は追わない（資格を別のホストへ運ばない）
- 形が壊れている（1 行でも読めなければページ全体を読まない）・16 MiB を超える
- 宣言したメトリクス（とラベル）に一致する系列が無い。**無いのを 0 と読まない**（キューが空なら exporter は 0 を出す）
- 集計なしで 2 本以上が一致した
- 値が NaN

text 形式は Prometheus 0.0.4 の text 形式（OpenMetrics の text も読める：コメント・`# EOF` は飛ばす）。
TYPE の無い系列・histogram・summary・タイムスタンプつきの行も読む。protobuf 形式は読まない。
