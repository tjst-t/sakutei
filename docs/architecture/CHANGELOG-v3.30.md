# CHANGELOG v3.29 → v3.30

**改訂日: 2026-10-28**

/metrics（Prometheus の text 形式）の値を観測するだけの provider を足した改訂である（Backlog #149。親のストーリー
「FW の更新と入れ直しを宣言で扱う」の 4：全体を止める型の更新の前に上流の積もりを観測し、上限を超えそうなら止めない／
待つ、を require で書く）。設計の向きは変えていない。

## #149：/metrics の値を観測する口（§29.1）

- **背景**: 止めている間に下流が使えなくなる更新の前に、上流のキューの長さ・待ちの件数を確かめる手段が無かった。
  Workflow の `require`（#87）と ready_condition（§8.3）は「観測の値が宣言と一致するか」で待つが、キューの長さのような
  数を観測する provider が無かった。
- **決定**: 観測だけの provider `metrics-probe`（`Capabilities.ObserveOnly`）。
  - 宣言は provider の設定（`--provider-config` の `metrics_probe_config`）の `endpoints`（URL・TLS・資格）と `probes`
    （メトリクスの名前・ラベルの一致・集計 `sum` / `max`・比べ方）。probe の名前がリソースの ID。
  - 比べ方は `"< 1000"` の形（`<` `<=` `≤` `>` `>=` `≥` `=` `==`、演算子を省けば `=`）。provider が比べ、結果を
    フィールド `ready.<名前>`（`"true"` / `"false"`）と `Observation.Ready` に出す。`require` は
    `fields: { ready.<名前>: "true" }` で待ち、ready_condition は条件の名前で待つ。
  - TLS は `ca_file` で確かめ、`insecure_skip_verify` は明示したときだけ。資格（bearer トークン・basic のパスワード）は
    SecretRef でしか書けず、取得の直前に解決する。転送（3xx）は追わない。
  - 届かない・2xx 以外・形が壊れている・メトリクスが無い・集計なしで 2 本以上が一致・NaN は unknown（absent と
    取り違えない。無いのを 0 と読まない）。Workflow は待ち、`timeout` で halt する。
  - text 形式は internal/metrics の `ParseScrape`（他の exporter のページを読む寛容な版。TYPE の無い系列・histogram・
    ラベルの周りの空白・タイムスタンプを受け、壊れた行と系列の重複は拒否）で読む。デーモンの /metrics の形の検査に
    使う厳しい `Parse` はそのまま。
- **比べ方を Diff の側に持ち込まなかった理由**: Workflow の待ち・ready_condition・plan の比べ方はすべて「一致」で、
  そこに大小を足すと Schema・Diff・cause の全部に効く。閾値は観測の元を宣言する場所（provider の設定）に置き、
  真偽にして渡せば既存の経路がそのまま使える。
- **見送り**: PromQL のような式（率・時間窓）・複数のメトリクスをまたぐ比べ方・OpenMetrics の protobuf 形式・
  Prometheus のサーバに問い合わせる口（/api/v1/query）。必要になったら別の probe の種類として足す。

## 受け入れテスト

`tests/acceptance/metrics_probe_test.go`（httptest の /metrics、デーモンの経路）：

- `[AC-149-2-1]` キューが 1500・ちょうど 1000 の間は `require`（`< 1000`）が満たされず、対象を止める手順へ進まない。
  999 に下がれば進み、done になる。
- `[AC-149-2-2]` 503・メトリクスが無い間は unknown で進まず、待ちの理由に訳を添える。届かないまま `timeout` を過ぎると
  halt し、対象を止めない。
- `[AC-149-2-3]` ready_condition：越えている・届かない・メトリクスが無い周は下流を Apply しない。下がった周に Apply する。
- `[AC-149-2-4]` registry：`metrics_probe_config` で作れる。無ければエラー。

`internal/metricsprobe/provider_test.go`：値と集計・unknown の各場合・観測だけ・TLS と資格・設定の誤り・比べ方の
書き方（`[AC-149-1-1]`〜`[AC-149-1-5]`）。
