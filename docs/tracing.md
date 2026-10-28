# トレースの出口（OTLP/HTTP）

Sakutei は周・観測・plan・Apply・Workflow の段を span にして、OTLP/HTTP で OpenTelemetry の Collector へ出せる（Backlog #189、
設計 §26.8）。**既定は出さない**。`--otlp-endpoint` を明示したときだけ出す。

## 使い方

```console
# 手元の Collector（loopback は平文でよい）
$ sakutei --intent intent/ --otlp-endpoint http://127.0.0.1:4318
$ sakutei plan --intent intent/ --otlp-endpoint http://127.0.0.1:4318
$ sakutei reconcile --intent intent/ --otlp-endpoint http://127.0.0.1:4318

# 別の区画の Collector（TLS と認可が要る）
$ sakutei --intent intent/ --otlp-endpoint https://collector.example:4318 --otlp-config otlp.yaml
```

| 旗 | 意味 |
|---|---|
| `--otlp-endpoint <url>` | OTLP/HTTP の受け口（`http://host[:port]` か `https://host[:port]`、前置きのパスを付けてもよい）。`/v1/traces` を足して送る。空なら出さない（既定） |
| `--otlp-config <file>` | 宛先の TLS と認可（下） |
| `--otlp-insecure` | loopback 以外の宛先へ、TLS（https）と認可のどちらかを欠いて送ることを許す。無ければ起動で断る |

- 起動すると標準エラーに `sending traces to <url> (<守り>) over OTLP/HTTP` と 1 行出す（値は出さない）。
- `OTEL_EXPORTER_OTLP_ENDPOINT` などの `OTEL_*` の環境変数は**読まない**。環境の proxy（`HTTPS_PROXY` など）も通さず、受け口の
  転送（3xx）にも従わない。宛先は旗で書いた 1 つだけ。
- 送る形は OTLP/HTTP の JSON（`Content-Type: application/json`）。Collector の `otlp` receiver の `http` がそのまま受ける。gRPC と
  protobuf の形は持たない。

## span

| span | どこ | 主な属性 |
|---|---|---|
| `sakutei.cycle` | 常駐の 1 周。**周ごとに 1 つのトレース** | `sakutei.cycle.number`・`sakutei.observed`・`sakutei.drifted`・`sakutei.unknown`・`sakutei.applied`・`sakutei.pruned`・`sakutei.held`・`sakutei.budget_halted`・`sakutei.prune_halted`・`sakutei.alert_only`・`sakutei.skipped_unreachable`・`sakutei.inventory_halted` |
| `sakutei.observe` | 周の中の観測の段 | `sakutei.resources`（対象の数）・`sakutei.observed`・`sakutei.drifted`・`sakutei.unknown`・`sakutei.converging` |
| `sakutei.read` | 1 つのリソースの観測 | `sakutei.provider`・`sakutei.resource`・`sakutei.outcome`（`ok`・`absent`・`unknown`・`converging`） |
| `sakutei.bulk_observe` | provider の BulkObserve の 1 回 | `sakutei.scope`・`sakutei.resources`（返った数） |
| `sakutei.plan` | 周の中の Diff・prune の List・ゲート・予算の段。一回性の `plan` では根 | `sakutei.plans`・`sakutei.ops`・`sakutei.prune_ops`・`sakutei.held`・`sakutei.budget_halted`・`sakutei.capacity_halted`・`sakutei.mode` |
| `sakutei.reconcile` | 一回性の `reconcile` の根 | `sakutei.exit_code` |
| `sakutei.apply` | provider の Apply の 1 回（prune の Delete を含む） | `sakutei.provider`・`sakutei.resource`（1 つのとき）・`sakutei.resources`・`sakutei.ops`・`sakutei.op_kinds`・`sakutei.causes`・`sakutei.prune` |
| `sakutei.workflow` | 1 つの Workflow のその周の進み | `sakutei.workflow`・`sakutei.workflow.run`・`sakutei.outcome`（周の終わりの phase） |
| `sakutei.workflow.step` | Workflow の手順の 1 つ。その手順の Apply・観測はこの下 | `sakutei.workflow.stage`・`sakutei.workflow.target`・`sakutei.workflow.step`・`sakutei.workflow.step_kind`・`sakutei.resource`・`sakutei.outcome`（`done`・`pending`・`failed`） |

- resource の属性は `service.name`（既定 `sakutei`、`service_name` で変える）・`service.version`・`host.name`・`sakutei.command`
  （`daemon`・`plan`・`reconcile`）。
- 一回性の `plan`・`reconcile` は 1 回が 1 つのトレースで、根に `sakutei.exit_code` を載せる。終わる前に送り終える（最大 5 秒待つ）。
- 失敗（provider の誤り・Workflow の halt・終了コード 1）は span の status を ERROR にし、誤りの文を載せる。
- **欄の値は載せない**：Diff の中身・SecretRef の値は span に入らない。載るのは名前・ID・数・結果だけ。
- 非同期の Apply は周の span が閉じたあとに終わることがある（子が親より遅く閉じる。OTLP では正しい形）。

## 送れないとき

トレースは観測の補助で、正しさに関与しない。**送れなくても収束を止めない・待たない**：

- span は上限つきの列（4096）に積み、1 つの goroutine が束（最大 512、1 秒ごと）にして送る。列が溢れた span は捨てる。
- 届かない・429・502・503・504 は少し待って 2 度まで送り直し、それでも駄目ならその束を捨てる。ほかの応答（401・403 など）は
  送り直さない。
- 送れない間は `sakutei: otlp: cannot send N span(s) to <url>: <理由>; dropped ...` を 1 度だけ出し、送れるようになったら
  `sending traces to <url> again` を 1 度だけ出す。

## TLS と認可（`--otlp-config`）

読み取りの口（`--listen-auth-config`、[metrics.md](metrics.md)）と同じ考え。書式は intent と同じ JSON / YAML のサブセット。

```yaml
secret_store: secrets.json             # file SecretBackend（client_cert・bearer_token に要る）
tls:                                   # https の宛先だけ（省略可）
  ca_file: collector-ca.pem            # 宛先の証明書を確かめる CA（既定はシステムのルート）
  server_name: collector.example       # 確かめる名前（既定は宛先の host）
  min_version: "1.2"                   # "1.2"（既定）か "1.3"
  client_cert: secretref:file/sakutei/otlp_cert   # クライアント証明書（PEM、中間を含めてよい）
  client_key: secretref:file/sakutei/otlp_key     # その鍵（PEM）
bearer_token: secretref:file/sakutei/otlp_token   # Authorization: Bearer <token>
service_name: sakutei-tokyo            # resource の service.name（既定 sakutei）
timeout: 10s                           # 1 回の送りの時間切れ（既定 10s）
```

- 証明書・鍵・トークンは **SecretRef でしか書けない**（値を書くと断る。値は誤りの文にも出さない）。30 秒ごとに金庫を読み直し、
  再起動なしで入れ替える（読めなければ前のまま）。CA の束の入れ替えは再起動。
- 宛先の証明書の確かめを外す設定は無い。
- URL に資格（`https://user:pass@...`）を書くと断る。
- **loopback 以外**（`127.0.0.0/8`・`::1`・`localhost` 以外）の宛先は、`https` と認可（`bearer_token` か `tls.client_cert`）の両方が
  無ければ起動で断る。どうしても平文・認可なしで送るときは `--otlp-insecure` を付ける。
- 相対パスは設定ファイルのあるディレクトリから。知らないキーは断る。

Collector の側の例（contrib の配布。bearer トークンは `bearertokenauth` 拡張、クライアント証明書は receiver の `client_ca_file`）:

```yaml
extensions:
  bearertokenauth:
    filename: /etc/otelcol/sakutei-token
receivers:
  otlp:
    protocols:
      http:
        endpoint: 0.0.0.0:4318
        tls:
          cert_file: /etc/otelcol/server.pem
          key_file: /etc/otelcol/server-key.pem
          client_ca_file: /etc/otelcol/sakutei-clients-ca.pem
        auth:
          authenticator: bearertokenauth
service:
  extensions: [bearertokenauth]
  pipelines:
    traces:
      receivers: [otlp]
      exporters: [otlp/backend]   # Tempo・Jaeger など
```

標本化（サンプリング）・属性の加工・保管は Collector と後段に任せる。Sakutei は全部の span を出す。

## 統合テスト

`make integration-otlp`（docker と sudo（パスワードなし）が要る。check・test・CI には入れない）。OpenTelemetry Collector
（`otel/opentelemetry-collector-contrib:0.163.2`、digest で固定。`SAKUTEI_IT_OTELCOL_IMAGE` で差し替えられる）を 127.0.0.1 の
空いたポートに立て、受け口を 2 つ（平文と、TLS 1.2+・クライアント証明書・bearer トークンを求めるもの）開き、受けた span を
file exporter でファイルに書かせて読む。終われば（落ちても）コンテナを消す。

| 番号 | 確かめること |
|---|---|
| IT-189-1 | 常駐：周ごとのトレースが届き、その下に観測の段・Read・plan の段・Apply・Workflow と手順がある。Workflow の手順の Apply は手順の下。`service.name=sakutei`。欄の値は届かない |
| IT-189-2 | `plan`・`reconcile`：1 回が 1 つのトレース（根に終了コード 0）で、終わる前に届く |
| IT-189-3 | TLS 1.3・クライアント証明書・bearer トークンの受け口に届く。トークンは span に入らない |
| IT-189-4 | トークンが違う・クライアント証明書が無いと Collector が断り、何も受け付けられない。sakutei は 1 度だけ知らせ、収束は終える（終了コード 0） |
| IT-189-5 | `--otlp-endpoint` が無ければ（`OTEL_*` の環境変数があっても）常駐・`plan`・`reconcile` から何も届かない |

結果（2026-10-28、Collector contrib 0.163.2）：5 件とも通った。
