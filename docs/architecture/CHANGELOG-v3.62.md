# CHANGELOG v3.61 → v3.62

**改訂日: 2026-10-28**

トレースの出口（OTLP/HTTP）を決めた改訂である（Backlog #189。親のストーリー「M7：本番に向けた固め」の「トレース」）。

## #189：OTLP のトレースの出口（§26.8・§29.1）

- **背景**: `/metrics`（§27.5）は「いまどうなっているか」の数を出すが、1 つの周の中で何にどれだけ時間がかかったか（どの provider の
  観測が遅いか・どの Apply が失敗したか・Workflow のどの手順で待っているか）は、ログを読み合わせるしかなかった。
- **決定**:
  1. **span**：常駐の周（`sakutei.cycle`。周ごとに 1 つのトレースの根）、周の中の観測の段（`sakutei.observe`）と 1 つずつの Read
     （`sakutei.read`）・BulkObserve（`sakutei.bulk_observe`）、Diff・prune の List・ゲート・予算の段（`sakutei.plan`）、provider の
     Apply の 1 回（`sakutei.apply`。prune の Delete を含む）、Workflow のその周の進み（`sakutei.workflow`）と手順（`sakutei.workflow.step`）。
     一回性の `plan`・`reconcile` は 1 回が 1 つのトレース（根は `sakutei.plan`・`sakutei.reconcile`、終了コードを載せる）。
  2. **形**：OTLP/HTTP の JSON の写像（`POST <endpoint>/v1/traces`）。protobuf も OpenTelemetry の SDK も使わない（外の依存なし）。
  3. **既定は出さない**：`--otlp-endpoint` を明示したときだけ。`OTEL_*` の環境変数は読まず、環境の proxy を通さず、転送に従わない。
  4. **値は載せない**：名前・ID・数・結果（availability・Op の種類・cause・手順の結果）だけ。欄の値・秘密は載せない。
  5. **収束を止めない**：上限つきの列（4096）と束（512・1 秒）で送り、届かない・429・5xx は 2 度まで送り直し、だめなら捨てて
     数える。送れない間は 1 度だけ知らせ、戻れば 1 度だけ知らせる。
  6. **宛先の TLS と認可は読み取りの口（#161）と同じ考え**（`--otlp-config`）：TLS 1.2 以上（1.3 も選べる）・`ca_file` と
     `server_name` で確かめる（確かめを外す設定は無い）・bearer トークンとクライアント証明書は SecretRef（file SecretBackend）で
     30 秒ごとに読み直す・URL に資格を書けば断る。loopback 以外の宛先へ TLS と認可のどちらかを欠いて送るのは `--otlp-insecure` の
     ときだけで、無ければ起動で断る。
- **見送り**: 標本化（サンプリング）・属性の加工・gRPC・protobuf の形は持たない（Collector と後段に委ねる。§2）。ログと
  メトリクスの OTLP は今回の範囲外（ログは `--log-format json`・event sink、メトリクスは `/metrics`）。
- **互換**: `--otlp-endpoint` を渡さない今までの使い方は何も変わらない（何も送らない）。
- 実装：`internal/tracing`（span の口。標準のライブラリだけに依り、どこからでも呼べる）・`internal/otlpexport`（送り手と設定）・
  `pkg/cli/trace.go`。受け入れ `tests/acceptance/otlp_trace_test.go`（`[AC-189-1]`〜`[AC-189-4]`）、統合 `make integration-otlp`
  （OpenTelemetry Collector contrib 0.163.2 を tag と digest で固定、`IT-189-1`〜`IT-189-5`）。利用者の文書は `docs/tracing.md`。
