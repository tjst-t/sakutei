# 新しいコアの provider（v3/providers）

設計 `docs/core/README.md` の §7.1・§8 の段 6・段 7・段 8a。provider・Expand のモジュール・秘密の保管先・Events の送り先・承認ツールはコアの外のモジュールで、コア（`github.com/tjst-t/sakutei/v3`）のインターフェースだけを使う。この Go モジュールは `github.com/tjst-t/sakutei/v3/providers`（コアを `replace ../` で使う。標準ライブラリだけ）。

| パッケージ | 中身 |
|---|---|
| `dnszone` | DNS のゾーンファイルのレコード（種類 `record`、ID `<FQDN>/<型>`、項目 `value`・`values`・`ttl`）。ゾーンは Range の `args.zone`。書き換えた Apply・Delete のときだけ SOA のシリアルを上げる（E-B1・N-B11・E-I7） |
| `hostconf` | ホストのファイル（種類 `file`、ID `<host>:<絶対パス>`、項目 `content`・`mode`・`owner`・`group`）と systemd のユニット（種類 `unit`、ID `<host>:<ユニット名>`、項目 `content`・`enabled`・`active`・`restart_on`）。中身は sha256 だけを観測する（E-D5・E-E2・N-D7・N-E1・N-E6） |
| `nftables` | nftables の chain（種類 `chain`、ID `<host>/<family>/<table>/<chain>`、項目 `rules`・`type`・`hook`・`priority`・`policy`）。ホストごとに file（ruleset のファイル）か live（カーネル。root で走るのは決まった道具だけ）。扱う table は Range の `args.host`・`args.tables`。live の 2 段の Apply は `core.Transactor`（E-B2・E-B3・E-B4・N-B9） |
| `nftguard`・`cmd/sakutei-nft-guard` | 決まった道具 sakutei-nft-guard（v2 から移し、すぐ戻す `rollback` を足した）。`list`・`check`・`apply`・`arm`・`disarm`・`rollback`・`status` だけ。扱う表は root だけが書ける道具の設定ファイルに書く（E-E1・E-B3） |
| `expanders/template` | Expand `go:template`：intent の `templates:` の雛形を、台の一覧（外部入力 `for_each` か intent の `items`）の 1 件ごとに宣言にする。全台に共通の値は `vars` に 1 か所。外部入力が届かなければ宣言を作らず Uncertain（E-A11・N-A14・N-E7・N-B15・E-K3） |
| `expanders/netpolicy` | Expand `go:netpolicy`：intent の `network_policy:`（networks・allows・targets、domain・interfaces、reason・tags）を検査し、実現先ごとに nftables の base chain に展開する。重なり・理由の無い例外・重複・宣言に無い網・domain をまたぐ許可は Reject、隠れた許可・通らない許可は Warn。ほかの実現先は `netpolicy.WithRealizer`（E-B5） |
| `expanders/compat` | Expand `go:compat`：intent の `compatibility:`（部品の版の互換の表）。望む組み合わせが表に無ければ Reject、1 つずつ更新する途中の組み合わせは depends_on の `when` で止める（E-A7） |
| `secretbackend` | 秘密の保管先 `vault-kv2`（Vault KV v2・OpenBao。トークンのファイルか AppRole、世代は KV の版）と `command`（外のコマンド。世代は正の整数）。届かなければ誤りで、コアがその秘密に依る宣言をその周 Unknown にする（E-G3・E-G4・E-O2・N-O2） |
| `sinks/alertmanager` | 送り先 `alertmanager`：Alertmanager の API v2 に、今 firing の一覧を周ごとに `endsAt = 今 + ttl` で送り直し、resolved と設定した出来事型を 1 回送る。複数の宛先（`urls`）の全部に送り、1 台でも受け取れば送れたとする（E-H2・E-T3） |
| `sinks/syslog` | 送り先 `syslog`：RFC 5424（TCP の上は octet-counting、TLS は RFC 5425）で 1 件ずつ。`streams: [audit]` なら監査の鎖の行をそのまま MSG に（facility 13、受け手で鎖をたどれる）。番号は `[meta sequenceId]`（E-L1・N-L1） |
| `sinks/metrics` | 送り先 `metrics`：CycleDone と今の firing の一覧から Prometheus の値を作り `/metrics` で見せる。数えていない値は出さない。待ち受けは最初に送られたときに始める。TLS・クライアント証明書・読むだけのトークン・外向きの平文の明示は `listen.read` と同じ（E-H3・N-L4） |
| `sinks/otlp` | 送り先 `otlp`：CycleDone を周の span、その前の出来事型を子の span にして OTLP/HTTP の JSON で送る。ID は Event の番号から決まる（送り直しても同じ）。欄の値は載せない（E-H3・N-H5） |
| `approve`・`cmd/sakutei-approve` | 承認ツール：ネットワーク越しの承認・拒否を、CLI と同じ判断のファイルとして Record に書くだけ。承認者は前段の認証（クライアント証明書の CN か、認証をする proxy のヘッダ）から受け取る。申請者・要件に無い人・待っていない依頼は書く前に断る（不変条件 21、E-M4・N-M4・E-L3） |
| `cmd/sakutei` | 上の provider・Expand・保管先・送り先を `cli.Main(cli.WithModule(...), cli.WithExpander(...), cli.WithSecretBackend(...), cli.WithSink(...))` で組み込んだ CLI（E-O4・E-S4） |

ホストへのつなぎ方（hostconf・nftables の live）はコアの SDK `sdk/hostexec`（手元・ssh・sudo。ホスト鍵は known_hosts で確かめる）。

## つなぎ方の設定と intent の例

```yaml
# config.yaml（つなぎ方だけ）
providers:
  dns:
    module: dnszone
    settings:
      zone_dir: /var/lib/named          # ゾーン example.com. のファイルは /var/lib/named/example.com.zone
      reload_cmd: [rndc, reload, "{zone}"]
  host:
    module: hostconf
    settings:
      hosts:
        web1:
          ssh: {address: 192.0.2.21, user: sakutei, key: /etc/sakutei/id_ed25519, known_hosts: /etc/sakutei/known_hosts}
          sudo: true
  fw:
    module: nftables
    settings:
      hosts:
        lab: {ruleset: /srv/fw/lab.rules}            # file
        fw1:                                         # live
          ssh: {address: 192.0.2.1, user: sakutei, key: /etc/sakutei/id_ed25519, known_hosts: /etc/sakutei/known_hosts}
          sudo: true
          guard: [/usr/local/libexec/sakutei-nft-guard]
```

```yaml
# intent（範囲・宣言）
ranges:
  - {provider: dns, args: {zone: example.com.}}
  - {provider: host, args: {host: web1}}
  - {provider: fw, args: {host: fw1, tables: "inet/filter"}}
resources:
  - type: dns/record
    id: www.example.com./A
    fields: {value: 192.0.2.10, ttl: 300}
  - type: fw/chain
    id: fw1/inet/filter/input
    fields:
      type: filter
      hook: input
      priority: 0
      policy: drop
      rules: ["ct state established,related accept", "jump mgmt"]
    apply:
      transaction:
        timeout: 30s
        confirm:
          - {phase: before, when: 'check(mgmt-ssh)'}
          - {phase: after, when: 'check(mgmt-ssh)'}
```

- nftables の `rules` は nft が表示する形で書く（例 `iif "lo" tcp dport 22 accept`。nft が書き直す形で書くと、読み直しで宣言どおりにならず、2 段の Apply なら確定しない）。引用符の外の空白は 1 つ（違えば読み込みで正規形を示して断る）。
- live の base chain（4 つの項目を書いた宣言）は `apply.transaction` を書く（書かなければ読み込みで断る）。管理の ssh を通す最小の核は Sakutei の外で保守し、宣言した base chain から `jump` で参照する（E-B4。その chain は Range の `external_owners` で外す）。
- 決まった道具の設定ファイル（既定 `/etc/sakutei/nft-guard.json`、root の持ち物で group・other が書けない）：`{"tables": ["inet/filter"], "state_dir": "/run/sakutei-nft-guard", "max_rollback_seconds": 600}`。sudoers で許すのは道具だけにする。

## Expand と秘密の保管先の例

```yaml
# config.yaml
secret_backends:
  vault: {module: vault-kv2, settings: {address: "https://vault.example.com:8200", approle: {role_id_file: /etc/sakutei/role_id, secret_id_file: /etc/sakutei/secret_id}}}
inputs:
  inventory: {file: /srv/inventory/hosts.yaml}     # 最上位はマップ（hosts: [...]）
```

```yaml
# intent
expand:
  - kind: go:template
  - kind: go:netpolicy
inputs:
  - {name: inventory, items: hosts, expected_min: 40, max_drop: 50%}
templates:
  - name: bmc
    for_each: inventory.hosts
    vars: {firmware: "1.3"}
    resources:
      - {type: fw/version, id: "{item.name}-bmc", fields: {version: "{vars.firmware}"}}
network_policy:
  networks: [{name: office, cidrs: ["10.1.0.0/24"]}, {name: dmz, cidrs: ["192.0.2.0/24"]}]
  allows: [{from: office, to: dmz, proto: tcp, ports: [443], reason: "社内から公開の web"}]
  targets:
    - {name: fw1, kind: boundary, networks: [office, dmz], provider: fw, id: fw1/inet/filter/forward, apply: {transaction: {timeout: 30s}}}
```

各 Expand の golden のテストは `expanders/*/testdata/<場合>/`（`intent/`・任意の `inputs.json`・`golden.json`）にあり、コアの `expand/expandtest.Golden` で 2 回呼んで同じ出力（純粋さ）と golden を確かめる（N-P5）。意図した変更なら `go test ./expanders/... -update` で書き直す。

## 送り先と承認ツールの例

```yaml
# config.yaml
sinks:
  am:
    module: alertmanager
    settings:
      urls: ["https://am1.example.com:9093", "https://am2.example.com:9093"]
      tls: {ca: /etc/sakutei/am-ca.pem}
      token_file: /etc/sakutei/am.token     # 省略可（Authorization: Bearer）
      ttl: 10m                              # firing の endsAt（周の間隔より長く）
      incidents: [Applied, Deleted]         # alert にする出来事型（既定は無し）。長さは incident_ttl（既定 1h）
      labels: {instance: sakutei-tokyo}
  audit:
    module: syslog
    settings: {address: "tls://log.example.com:6514", tls: {ca: /etc/sakutei/log-ca.pem}}
    streams: [audit]                        # events も足せる
  prom:
    module: metrics
    settings: {listen: "127.0.0.1:9464"}    # tls・auth.tokens_file・plaintext は listen.read と同じ
  otel:
    module: otlp
    settings: {endpoint: "https://otel.example.com:4318", service_name: sakutei}
```

- Alertmanager の labels は `alertname`（Event の種類）・`resource` か `scope`・`stuck`（stuck に上がったときだけ）・宣言の labels（Prometheus の名前に直す）・設定の `labels`。annotations は `message`・`since`・`count`。Sakutei が止まれば送り直しも止まり、alert は `ttl` で消える。
- 平文の http は loopback の宛先か `plaintext: true` のときだけ（alertmanager・otlp）。URL に資格は書けない。
- `/metrics` の値：`sakutei_cycles_total`・`sakutei_last_cycle_timestamp_seconds`・`sakutei_cycle_duration_seconds`・`sakutei_resources{state}`・`sakutei_declared_resources`・`sakutei_cycle_applied`・`sakutei_cycle_failed`・`sakutei_halts`・`sakutei_budget_remaining{policy}`・`sakutei_budget_window_remaining{policy}`・`sakutei_firing{kind}`・`sakutei_firing_stuck{kind}`・`sakutei_events_total{kind}`。
- どの送り先も、届かない・戻らないときは誤りを返すだけで、コアが位置を保って次の周に送り直す（周は止めない、不変条件 28）。

```console
$ sakutei-approve --record /var/lib/sakutei --listen 127.0.0.1:8444 --user-header X-Remote-User   # 認証をする proxy の後ろ
$ sakutei-approve --record /var/lib/sakutei --listen 0.0.0.0:8444 \
    --tls-cert tls.crt --tls-key tls.key --tls-client-ca approvers-ca.pem                       # 証明書の CN が承認者
$ curl -X POST https://approve.example.com:8444/approvals/chg-3f…/approve -d '{"comment": "確認済み"}'
```

`GET /approvals`（待っている依頼。`?all=1` で全部）・`GET /approvals/{id}`・`POST /approvals/{id}/approve`・`POST /approvals/{id}/reject`（`comment` 必須）。断るときは 401（承認者が分からない）・403（申請者・要件に無い人）・404（依頼が無い）・409（待っていない）・400（書き方）。`--user-header` は loopback の待ち受けか `--tls-client-ca`（proxy の証明書）と一緒にしか使えない（誰でもヘッダを名乗れる口にしない）。数えて効かせ、監査に残すのは周。

## 統合テスト（本物のカーネル）

```console
$ make integration-v3-nftables
```

使い捨ての network namespace（`ip netns`）の中で本物の nft を使い、root の持ち物の置き場に入れた決まった道具で、`check(reach)`（namespace の中の TCP の待ち受けに新しい接続で届くか。管理の経路の代わり）を確かめにして 2 段の Apply を回す（`nftables/kernel_test.go`）。sudo（パスワードなし）・ip・nft・python3 が要るので、check・test・CI には入れない。ホストの FW には触れず、namespace と置き場はテストが消す。

- `TestKernelTransaction`：経路を通す既定の拒否（policy drop）は確かめが通って確定し、時間切れを過ぎても戻らない。経路を塞ぐ変更は確かめが通らず（timeout 6s の半分まで確かめる）確定せず、元の規則に戻り、終了コード 4 で止まる。次の周も同じ宣言を当て直さない。
- `TestKernelPrepareExpires`：`Prepare` のあと `Commit` も `Rollback` もしなければ、ホストの側の時間切れ（道具が切り離して眠る自分）で元の規則に戻る（不変条件 17 の契約）。

2026-10-31 に Linux 7.0・nftables 1.0.9 で通した（2 つで約 18 秒）。
