# 新しいコアの provider（v3/providers）

設計 `docs/core/README.md` の §7.1・§8 の段 6・段 7。provider・Expand のモジュール・秘密の保管先はコアの外のモジュールで、コア（`github.com/tjst-t/sakutei/v3`）のインターフェースだけを使う。この Go モジュールは `github.com/tjst-t/sakutei/v3/providers`（コアを `replace ../` で使う。標準ライブラリだけ）。

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
| `cmd/sakutei` | 上の provider・Expand・保管先を `cli.Main(cli.WithModule(...), cli.WithExpander(...), cli.WithSecretBackend(...))` で組み込んだ CLI（E-O4・E-S4） |

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

## 統合テスト（本物のカーネル）

```console
$ make integration-v3-nftables
```

使い捨ての network namespace（`ip netns`）の中で本物の nft を使い、root の持ち物の置き場に入れた決まった道具で、`check(reach)`（namespace の中の TCP の待ち受けに新しい接続で届くか。管理の経路の代わり）を確かめにして 2 段の Apply を回す（`nftables/kernel_test.go`）。sudo（パスワードなし）・ip・nft・python3 が要るので、check・test・CI には入れない。ホストの FW には触れず、namespace と置き場はテストが消す。

- `TestKernelTransaction`：経路を通す既定の拒否（policy drop）は確かめが通って確定し、時間切れを過ぎても戻らない。経路を塞ぐ変更は確かめが通らず（timeout 6s の半分まで確かめる）確定せず、元の規則に戻り、終了コード 4 で止まる。次の周も同じ宣言を当て直さない。
- `TestKernelPrepareExpires`：`Prepare` のあと `Commit` も `Rollback` もしなければ、ホストの側の時間切れ（道具が切り離して眠る自分）で元の規則に戻る（不変条件 17 の契約）。

2026-10-31 に Linux 7.0・nftables 1.0.9 で通した（2 つで約 18 秒）。
