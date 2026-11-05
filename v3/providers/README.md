# v3 の provider・Expand・sink・ツール（v3/providers）

v3 のコアの外に置くモジュールをまとめた Go モジュール `github.com/tjst-t/sakutei/v3/providers`。

- 中身は provider・Expand・secret backend・sink・承認ツール・移行と import のツール。
- どれもコア（`github.com/tjst-t/sakutei/v3`）の interface だけを使う。
- コアは `replace ../` で参照する。依存は標準ライブラリだけ。
- 設計は `docs/core/README.md` の §7.1（provider の移植）と §8（作る順番）。

作る順番（§8）のうち、このモジュールが受け持つのは Stage 6〜9。Stage 1〜9 はすべて main に入っている。

| Stage | このモジュールで足したもの |
|---|---|
| 6 | 最初の実 provider：`dnszone`・`hostconf`・`nftables` と専用ツール `sakutei-nft-guard` |
| 7 | Expand（`template`・`netpolicy`・`compat`）と secret backend（`vault-kv2`・`command`） |
| 8a | sink（`alertmanager`・`syslog`・`metrics`・`otlp`）と承認ツール `sakutei-approve` |
| 8b | `redfish`・`metrics-probe` |
| 8c | `netconf`・`cli-device` |
| 8d | `proxmox`・`guest-os` |
| 8e | `postgresql`・`sqlmigrate`・`ldap` |
| 8f | `s3`・`ansible`・`template-file` |
| 9 | v2 の intent の書き直し `sakutei-migrate` と、import・同期のツール `sakutei-import` |

## 目次

- [パッケージの一覧](#パッケージの一覧)
- [ホストへの接続](#ホストへの接続)
- [dnszone・hostconf・nftables](#dnszonehostconfnftables)
- [redfish と metrics-probe](#redfish-と-metrics-probe)
- [netconf と cli-device](#netconf-と-cli-device)
- [proxmox](#proxmox)
- [guest-os](#guest-os)
- [postgresql・sqlmigrate・ldap](#postgresqlsqlmigrateldap)
- [s3・ansible・template-file](#s3ansibletemplate-file)
- [Expand と secret backend](#expand-と-secret-backend)
- [sink と承認ツール](#sink-と承認ツール)
- [移行と import のツール](#移行と-import-のツール)

統合テストは、それぞれの provider の節の最後に置いた。どれも sudo（パスワードなし）などが要るので、`make check`・`make test`・CI には入れていない。

## パッケージの一覧

表の `E-…`・`N-…` は、設計書がもとにしたユースケースの ID。

### provider

| パッケージ | 中身 |
|---|---|
| `dnszone` | DNS のゾーンファイルのレコード（type `record`、ID `<FQDN>/<型>`、field `value`・`values`・`ttl`）。ゾーンは Range の `args.zone`。SOA のシリアルは、ファイルを書き換えた Apply・Delete のときだけ上げる（E-B1・N-B11・E-I7） |
| `hostconf` | ホストのファイル（type `file`、ID `<host>:<絶対パス>`、field `content`・`mode`・`owner`・`group`）と systemd のユニット（type `unit`、ID `<host>:<ユニット名>`、field `content`・`enabled`・`active`・`restart_on`）。中身は sha256 だけを観測する（E-D5・E-E2・N-D7・N-E1・N-E6） |
| `nftables` | nftables の chain（type `chain`、ID `<host>/<family>/<table>/<chain>`、field `rules`・`type`・`hook`・`priority`・`policy`）。ホストごとに file（ruleset のファイル）か live（カーネル）を選ぶ。live で root で走るのは専用ツールだけ。扱う table は Range の `args.host`・`args.tables`。live の two-phase Apply は `core.Transactor`（E-B2・E-B3・E-B4・N-B9） |
| `nftguard`・`cmd/sakutei-nft-guard` | 専用ツール `sakutei-nft-guard`。v2 から移し、すぐに戻す `rollback` を足した。サブコマンドは `list`・`check`・`apply`・`arm`・`disarm`・`rollback`・`status` だけ。扱う table は、root だけが書けるツールの config ファイルに書く（E-E1・E-B3） |
| `redfish` | 物理サーバの BMC（Redfish）。type は 4 つ（下の表）。観測用と書き込み用の credential を分け、書き込み用は Apply・Invoke の中でだけ読む。List を持たず、Delete は拒否する（E-A1〜E-A5・N-A1〜N-A4・E-G5） |
| `metricsprobe` | /metrics（Prometheus の text 形式）を観測するだけ（module 名 `metrics-probe`）。type `series`（ID `<endpoint>/<メトリクス>[{ラベル="値",…}]`、field `series`・`value`（系列が 1 本のときだけ）・`sum`・`max`・`min`）。比べ方は Condition に書く。到達できない・形式が壊れている・一致する系列が無い・NaN は Unknown（E-H4・N-T4） |
| `netconf` | NETCONF の機器の設定の部分木（type `subtree`、ID `<host>/<部分木>`、field `config`（宣言する XML）・`capabilities`（観測だけ））。既定は観測だけ。`settings.apply: true` のときだけ `:confirmed-commit:1.0` の two-phase Apply をする（`core.Transactor`）。NACM で部分木だけに絞ったユーザーで動く。詳しくは [netconf と cli-device](#netconf-と-cli-device)（E-B7・E-B8・E-B9・E-B10・N-B1・N-B4） |
| `clidevice` | CLI でしか設定できない装置（module 名 `cli-device`）。type は機種の定義の item（`items`）、ID は装置。field の値は文字列か secret の参照。機種の定義は settings の `definition`（ファイルかマップ）。扱う装置と item は Range の `args.host`・`args.items`。出力が定義に合わなければ Unknown。定義の `actions` は `core.Invoker`、`config.commit_confirmed`・`confirm`・`cancel` があれば `core.Transactor`（E-B11・N-B1） |
| `proxmox` | Proxmox VE の VM と、VM の FW の規則（REST API・API トークン）。type `vm`・`fw-rules` と、観測だけの `volume`・`fw-rule`。Range は `args.cluster`。network_policy の realizer `proxmox-fw`（`proxmox.Realizer`）も持つ。偽の Proxmox は `proxmox/pvetest`、規則の文は `proxmox/fwrule`。詳しくは [proxmox](#proxmox)（E-D1・E-C2・E-B6・N-D1・N-D10・N-C12・E-G5） |
| `guestos` | ゲスト OS を ssh 越しに観測するだけ（module 名 `guest-os`）。type `os`（ID はホスト名、field `os_id`・`os_version_id`・`os_pretty_name`・`hostname`・`boot_id`・`markers`・`marker`）。詳しくは [guest-os](#guest-os)（E-E4・E-A8・E-K1） |
| `postgresql` | PostgreSQL のロール・データベース・権限・拡張（type `role`・`database`・`grant`・`extension`）。psql を exec し、パスワードは SCRAM の検証値で送る。Range は `args.server`。観測用と書き込み用の credential を分け、書き込み用は Apply・Delete の中でだけ読む。ユーザーのテーブルがある DB の削除は `RefuseError`（E-F1・E-F3・N-F1・N-F7・E-G5） |
| `sqlmigrate` | バージョンつきのマイグレーション（type `database`、観測だけ）。SQLite（sqlite3 の CLI）と PostgreSQL（psql・advisory lock）。バージョンは Procedure の `invoke` の `migrate`・`rollback` でだけ変える。観測用と書き込み用の credential を分ける。List を持たず、Delete は拒否する（E-F2・N-F2・E-G5） |
| `ldap` | LDAP のエントリ（type `entry`、ID `<サーバ>/<DN の正規形>`、field `attributes`・`user_password`・`user_password_set`）。OpenLDAP の ldapsearch・ldapmodify を exec で呼ぶ。Range は `args.server`・`args.base_dn`・`args.filter`・`args.scope`（`sub`・`one`）。LDAPS と StartTLS。観測用と書き込み用の bind を分ける。子のあるエントリ・宣言したエントリの上位のエントリの削除は `RefuseError`（E-G1・N-G1・E-G5） |
| `s3` | S3 互換のバケット（SigV4 は標準ライブラリで実装）。type `bucket`。Range は `args.endpoint`・`args.prefix`。偽の S3 は `s3/s3test`。詳しくは [s3・ansible・template-file](#s3ansibletemplate-file)（E-C1・N-C7・E-G5・E-K1・E-K2・E-C3） |
| `ansible` | 専用の provider が無い雑多な設定を、Ansible のモジュール 1 つで扱う最小階級の provider（module 名 `ansible`）。type `call`。観測は `--check --diff` で流す。List・prune・Delete を持たない（E-E3・N-E5・E-K1） |
| `templatefile` | テンプレート（text/template）に台ごとの値を入れたファイルを書き出す（module 名 `template-file`）。type `file`。List を持たず、Delete は拒否する（E-E5・N-E8・E-K1） |

redfish の type：

| type | ID | field など |
|---|---|---|
| `system` | `<node>` | `power`・`boot_enabled`・`boot_target`。Off へは GracefulShutdown だけを送る。切り替えの途中は Converging |
| `virtual-media` | `<node>/cd` | `image` |
| `bios` | `<node>/<属性>` | `value`。Settings に PATCH し、保留中は Converging。Attribute Registry で確かめてから送る |
| `firmware` | `<node>/<部品>` | 観測だけ。`version` と、最新の更新 Task の `update_state`・`update_image`・`update_percent`・`update_message` |

redfish の一回きりの操作（`core.Invoker`）：

- system：`boot_once`・`reset`・`force_restart`（破壊的）・`force_off`（破壊的）
- firmware：`verify`・`stage`（破壊的）・`reset`・`force_restart`

### Expand・secret backend・sink

| パッケージ | 中身 |
|---|---|
| `expanders/template` | Expand `go:template`。intent の `templates:` のテンプレートを、台の一覧（外部入力の `for_each` か intent の `items`）の 1 件ごとに宣言にする。全台に共通の値は `vars` に 1 か所で書く。外部入力が届かなければ宣言を作らず Uncertain（E-A11・N-A14・N-E7・N-B15・E-K3） |
| `expanders/netpolicy` | Expand `go:netpolicy`。intent の `network_policy:`（networks・allows・targets、domain・interfaces、reason・tags）を検査し、realizer ごとに nftables の base chain に展開する。重なり・reason の無い例外・重複・宣言に無いネットワーク・domain をまたぐ許可は Reject、隠れた許可・通らない許可は Warn。ほかの realizer は `netpolicy.WithRealizer` で足す（E-B5） |
| `expanders/compat` | Expand `go:compat`。intent の `compatibility:`（部品のバージョンの compatibility table）。望む組み合わせが表に無ければ Reject。1 つずつ更新する途中の組み合わせは depends_on の `when` で待たせる（E-A7） |
| `secretbackend` | secret backend `vault-kv2`（Vault KV v2・OpenBao。token のファイルか AppRole、世代は KV のバージョン）と `command`（外部コマンド。世代は正の整数）。到達できなければエラーを返し、コアがその secret に依る宣言をその cycle は Unknown にする（E-G3・E-G4・E-O2・N-O2） |
| `sinks/alertmanager` | sink `alertmanager`。Alertmanager の API v2 に、今 firing の一覧を cycle ごとに `endsAt = 今 + ttl` で送り直す。resolved と、設定した edge Event は 1 回だけ送る。複数の宛先（`urls`）の全部に送り、1 台でも受け取れば送信できたとみなす（E-H2・E-T3） |
| `sinks/syslog` | sink `syslog`。RFC 5424 で 1 件ずつ送る（TCP では octet-counting、TLS は RFC 5425）。`streams: [audit]` なら audit のハッシュチェーンの行をそのまま MSG に入れる（facility 13。受け手でチェーンをたどれる）。通し番号は `[meta sequenceId]`（E-L1・N-L1） |
| `sinks/metrics` | sink `metrics`。CycleDone と今の firing の一覧から Prometheus の値を作り、`/metrics` で公開する。数えていない値は出さない。listen は最初に送られたときに始める。TLS・クライアント証明書・読み取り専用の token・外向きの平文の明示は `listen.read` と同じ（E-H3・N-L4） |
| `sinks/otlp` | sink `otlp`。CycleDone を cycle の span、それより前の edge Event を子 span にして、OTLP/HTTP の JSON で送る。ID は Event の番号から決める（送り直しても同じ）。field の値は載せない（E-H3・N-H5） |

### ツールと組み込み

| パッケージ | 中身 |
|---|---|
| `approve`・`cmd/sakutei-approve` | 承認ツール。ネットワーク越しの承認・拒否を、CLI と同じ判断のファイルとして Record に書くだけ。承認者は前段の認証（クライアント証明書の CN か、認証をする proxy のヘッダ）から受け取る。申請者・要件に無い人・待っていない依頼は、書く前に拒否する（不変条件 21、E-M4・N-M4・E-L3） |
| `builtin` | 組み込むモジュールの一覧（provider・Expand・secret backend・sink）。`cmd/sakutei` と `cmd/sakutei-import` が同じ一覧を使う。試験用の `fake` provider（`sdk/fake`）も含む（E-O4・E-S4） |
| `cmd/sakutei` | `builtin` の一覧を `cli.Main(cli.WithModule(...), cli.WithExpander(...), cli.WithSecretBackend(...), cli.WithSink(...))` で組み込んだ CLI（E-O4・E-S4） |
| `migrate`・`cmd/sakutei-migrate` | v2 の intent と provider の設定を、v3 の intent・接続設定・ID の対応表に書き直す（Stage 9）。詳しくは [移行と import のツール](#移行と-import-のツール)（N-I7） |
| `importer`・`cmd/sakutei-import` | 今の現実を宣言の形に書き出す。`--every` で繰り返す同期もできる。詳しくは [移行と import のツール](#移行と-import-のツール)（E-I1・E-I2・E-Q2・N-I1・N-I5・N-Q3・N-E3） |

## ホストへの接続

- hostconf・nftables の live・guest-os は、コアの SDK `sdk/hostexec` を使う（手元・ssh・sudo。ホスト鍵は known_hosts で確かめる）。
- netconf の ssh サブシステムと、cli-device の対話（pty）・1 行の exec は `internal/sshstream` を使う。設定は同じ `hosts:` の書き方。
  - 接続は使い回さない。
  - sudo は付けない。
  - ssh の終了コード 255 は「到達できない」とみなす。

## dnszone・hostconf・nftables

### 接続設定と intent の例

```yaml
# config.yaml（接続設定だけ）
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
# intent（Range・宣言）
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

### nftables のルール

- `rules` は nft が表示する形で書く（例 `iif "lo" tcp dport 22 accept`）。
  - nft が書き直す形で書くと、読み直したときに宣言どおりにならない。two-phase Apply なら確定しない。
  - 引用符の外の空白は 1 つにする。違えば、正規形を示して読み込み時にエラーにする（reject）。
- live の base chain（`type`・`hook`・`priority`・`policy` の 4 つを書いた宣言）には `apply.transaction` を書く。書かなければ読み込み時にエラーにする。
- 管理用の ssh を通す最小限のルールは Sakutei の外で保守し、宣言した base chain から `jump` で参照する（E-B4）。その chain は Range の `external_owners` で外す。
- 専用ツールの config ファイル（既定 `/etc/sakutei/nft-guard.json`）は root が所有し、group・other が書けないようにする。sudoers で許可するのは専用ツールだけにする。

  ```json
  {"tables": ["inet/filter"], "state_dir": "/run/sakutei-nft-guard", "max_rollback_seconds": 600}
  ```

  `tables` は必須。`nft`（nft の絶対パス）・`state_dir`・`max_rollback_seconds`（1〜3600）は省略できる。

### 統合テスト（本物のカーネル）

```console
$ make integration-v3-nftables
```

- 使い捨ての network namespace（`ip netns`）の中で本物の nft を使う（`nftables/kernel_test.go`）。
- 専用ツールは root 所有のディレクトリに置く。
- check には `check(reach)` を使う。namespace の中の TCP の listen に新しい接続で届くかを見るもので、管理用の経路の代わり。
- 要るもの：sudo（パスワードなし）・ip・nft・python3。テストは `SAKUTEI_IT_NFT=1` のときだけ動く（make のターゲットが付ける）。
- ホストの FW には触れない。namespace とディレクトリはテストが消す。

テスト：

- `TestKernelTransaction`
  - 経路を通す既定の拒否（policy drop）は、check が通って確定する。timeout を過ぎても戻らない。
  - 経路を塞ぐ変更は check が通らず（timeout 6s の半分まで確かめる）、確定しない。元のルールに戻り、終了コード 4 で止まる。
  - 次の cycle も、同じ宣言を Apply し直さない。
- `TestKernelPrepareExpires`：`Prepare` のあと `Commit` も `Rollback` もしなければ、ホストの側の timeout で元のルールに戻る。専用ツールが切り離して sleep する自分自身のプロセスが戻す（不変条件 17 の契約）。

2026-10-31 に Linux 7.0・nftables 1.0.9 で通した（2 つで約 18 秒）。

## redfish と metrics-probe

### 接続設定と intent の例

```yaml
# config.yaml（接続設定だけ）
providers:
  bmc:
    module: redfish
    settings:
      settle: 15s                                  # 電源・起動先の Apply のあと読み直すまで（既定 15s）
      nodes:
        node1:
          endpoint: https://192.0.2.11
          ca_file: /etc/sakutei/bmc-ca.pem         # か insecure_skip_verify: true（明示したときだけ）
          observe: {username: monitor, password_file: /etc/sakutei/bmc-monitor}
          write: {username: admin, password_file: /etc/sakutei/bmc-admin}   # 省けば observe を使う
  metrics:
    module: metrics-probe
    settings:
      endpoints:
        mq: {url: "https://mq1.example.net:15692/metrics", bearer_token_file: /etc/sakutei/mq-token}
```

```yaml
# intent
resources:
  - type: bmc/system
    id: node1
    fields: {power: "On", boot_enabled: Continuous, boot_target: Pxe}
    depends_on:
      - when: 'observe(metrics/series/mq/rabbitmq_queue_messages{queue="orders"}).sum < 100'
  - {type: bmc/virtual-media, id: node1/cd, fields: {image: "http://192.0.2.5/os.iso"}}
  - {type: bmc/bios, id: node1/ProcVirtualization, fields: {value: Disabled}}
  - {type: bmc/firmware, id: node1/BMC, fields: {version: "2.0.0"}}
procedures:
  - name: bmc-fw                                   # v2 の多相 Task（redfish-firmware）を Procedure に書き直したもの
    target: {type: bmc/firmware, id: node1/BMC}
    trigger: on_drift
    drift_fields: [version]
    steps:
      - name: approve
        gate: {approval: {quorum: {n: 1, of: [bob]}}}
      - name: verify
        invoke: {op: verify, args: {image_uri: "http://192.0.2.5/bmc-2.0.0.bin", sha256: "…"}}
      - name: stage
        critical: true
        skip_if: 'observe(bmc/firmware/{target}).version == desired.version'
        invoke: {op: stage, args: {image_uri: "http://192.0.2.5/bmc-2.0.0.bin"}}
      - name: applied
        wait: {when: 'observe(bmc/firmware/{target}).update_state == "Completed"', timeout: 1h}
    done_when: 'observe(bmc/firmware/{target}).version == desired.version'
```

### redfish のルール

- 再起動・強制停止・1 回だけの起動先は宣言できない。観測に残らない一回きりの操作なので、読み込み時にエラーにし、Procedure の `invoke` を案内する。
- v2 の `power_off: graceful_then_force` は 2 つに分けた。
  - 宣言の `power: "Off"` は GracefulShutdown だけを送る。止まらなければ ineffective Apply の回数で止まる（E-J6）。
  - 強制停止は Procedure の `invoke: {op: force_off}`（破壊的な操作）。例：`wait: {for: 5m}` のあと、`skip_if` で止まっていなければ送る。
- BIOS の Attribute Registry（読み取り専用・選べる値・範囲）は、読み込み時でなく Apply の前に BMC から読んで確かめる（読み込みは BMC に触れない）。合わなければ送らずに Apply を失敗させる（設計 §7.1 の redfish-bios、E-A4・N-A3）。
- `stage` の引数：
  - `transfer: multipart` なら、Sakutei がイメージを取得して MultipartHttpPushUri に送る。`sha256` を書けば、送る中身を確かめる。
  - `apply_time: OnReset` は再起動で反映される。
  - イメージを取得するときの credential は `image_username` と、secret の参照 `image_password`（`"<backend>:<パス>"`）。
- `ErrNotSent`（次の cycle に送り直す）を返すのは、送信できていないときだけ。BMC に到達できない・電源の切り替えの途中・イメージを取得できない、の場合。
- 直さないと通らない理由（BMC が持たない起動先・更新できない部品・sha256 の不一致）と、送ったあとのエラーでは Procedure を halt する。

### 統合テスト（qemu-bmc）

```console
$ make integration-v3-redfish
```

- qemu-bmc 0.11.0（tag と digest で固定）を containerlab で 2 台立てる（`redfish/integration/topology.clab.yml`）。
  - lab は `sakutei-v3rf`、管理ネットワークは 172.31.93.0/24。ほかの `make integration-v3-*` と重ならないので、同時に回せる。
- v3 の sakutei（`cmd/sakutei`）の CLI と daemon を、本物の Redfish に向けて回す。
- 終わったら（失敗しても）lab を消す。
- 要るもの：KVM・docker・containerlab・sudo（パスワードなし）。

step（`redfish/integration/redfish_test.go`）：

1. plan は BMC に書かない。reconcile で電源・起動先（Continuous の Pxe・Disabled）・仮想メディアに収束し、次の plan は何も変えない。
2. 手で変えた電源・起動先・仮想メディアを、次の reconcile で戻す。
3. ACPI に応えない VM への既定の停止は GracefulShutdown だけなので Off にならず、ineffective Apply の回数で止まる（終了コード 4）。Procedure の `force_off` で止まる。
4. BMC を止める（docker pause）と、そのノードは Unknown で何もしない（終了コード 3）。もう 1 台は収束する。
5. `boot_enabled: Once`・`power: ForceRestart` は読み込み時にエラーにする（終了コード 2）。
6. Procedure の `boot_once`（Cd）と `force_restart`：1 回起動すると、BMC が上書きを Disabled に戻す。
7. BIOS の属性：Settings に 1 度だけ PATCH する（再起動しない）。保留中の reconcile は Converging で、重ねて送らない。読み取り専用の属性は送らない。Procedure の `force_restart` で反映され、宣言していない属性は変わらない。
8. FW の更新（BMC）：daemon は承認の前に何も送らず、イメージも取りに来ない。承認のあとに verify・stage（critical）を 1 回ずつ送り、BMC の Task（Completed）を待ってバージョンで閉じる。
9. FW の更新（BIOS）：BMC の Task が終わってもバージョンは再起動まで変わらない。Procedure の `force_restart` で反映される。
10. FW の故障（qemu-bmc の障害注入）：
    - イメージを拒否される（400 VerificationFailed）と、Procedure は止まって送り直さない。
    - BMC の Task が Exception で終わると、バージョンは変わらず、wait の timeout で止まる。
11. 書き込み用のパスワードのファイルが無くても、plan は観測できて差分を出す。reconcile は書かずに失敗する。

2026-10-31 に Linux 7.0・containerlab・qemu-bmc 0.11.0 で通した（lab の用意を除いて約 110 秒）。

## netconf と cli-device

### 接続設定と intent の例

```yaml
# config.yaml（接続設定だけ）
providers:
  nc:
    module: netconf
    settings:
      apply: true                 # 既定は観測だけ（2 段の Apply を許すときだけ書く）
      commit_flow: "1.0"          # "1.1" なら :confirmed-commit:1.1 の persist（セッションが切れても新しいセッションから確定）
      timeout: 60s                # apply.transaction を書かない宣言の confirm-timeout（既定 30s）
      hosts:
        sw1:
          ssh: {address: 192.0.2.1, port: 830, user: sakutei-ro, key: /etc/sakutei/id_ro, known_hosts: /etc/sakutei/known_hosts}
          write:                  # 省略可。Prepare・Commit・Rollback だけが使う書き込み用の credential（E-G5）
            ssh: {address: 192.0.2.1, port: 830, user: sakutei, key: /etc/sakutei/id_rw, known_hosts: /etc/sakutei/known_hosts}
  sw:
    module: cli-device
    settings:
      definition: /etc/sakutei/example-switch.yaml    # 機種の定義（YAML・JSON のファイルか、ここに直に書くマップ）
      hosts:
        sw1:
          ssh: {address: 192.0.2.11, user: ro, key: /etc/sakutei/id_ro, known_hosts: /etc/sakutei/known_hosts}
          write: {ssh: {address: 192.0.2.11, user: admin, key: /etc/sakutei/id_rw, known_hosts: /etc/sakutei/known_hosts}}
          # definition: /etc/sakutei/other.yaml       # 装置ごとの上書き
```

```yaml
# intent
ranges:
  - provider: nc
    args:
      host: sw1
      filter.acl: '<nacm xmlns="urn:ietf:params:xml:ns:yang:ietf-netconf-acm"><rule-list><name>edge</name></rule-list></nacm>'
      ordered.acl: nacm/rule-list/rule        # 順に意味のある列（ordered-by user）だけ順を比べる
  - {provider: sw, args: {host: sw1, items: "syslog,community"}}   # items を書かなければ定義の全部
resources:
  - type: nc/subtree
    id: sw1/acl
    fields: {config: '<nacm xmlns="urn:ietf:params:xml:ns:yang:ietf-netconf-acm"><rule-list><name>edge</name>…</rule-list></nacm>'}
    apply:
      transaction:
        group: core                           # commit_group：同じ名前の台は全部の check が通ったときだけ確定
        timeout: 60s                          # confirm-timeout（check はその半分まで）
        confirm:
          - {phase: after, when: 'check(mgmt-path)'}             # cli.WithCheck で組み込んだ check
          - {phase: after, when: 'present(nc/subtree/sw2/acl)'}  # 別のリソースの観測
  - type: sw/syslog
    id: sw1
    fields: {host: 192.0.2.53, port: "514"}
  - type: sw/community
    id: sw1
    fields: {community: {secret: "vault:/net/snmp#community"}}
```

### netconf のルール

Range と field：

- 機器と部分木は Range に書く：`args.host`・`args.filter.<部分木>`（subtree filter）・`args.ordered.<部分木>`（順序を比べるパス）。
- `filter.<部分木>` の最上位の要素には名前空間を書く。名前空間の無い subtree filter は、すべての名前空間に一致してしまう。
- 宣言の最上位の要素は、filter が選ぶものだけにする。次のものは読み込み時にエラーにする：
  - filter の外を書く宣言
  - `nc:operation` などの操作の属性
  - 中身の無い要素
  - `capabilities` の宣言
  - 知らない field
- 比べるときは、名前空間・空白・兄弟の順序・宣言に無い属性を問わない正規形にそろえる。比べるのは宣言に書いた属性だけで、機器が付ける commit の時刻などでは drift にしない。

観測：

- 使う rpc は get-config（running、部分木の filter）と close-session だけ。
- 毎回新しいセッションを開いて閉じる。Prepare のあとの check も、新しい接続で届くかを見る。コアは provider を閉じないので、開いたままのセッションを残さない。
- 中身の無い要素だけの `<data>` は Absent。名前空間の有無は問わない。
- 到達できない・rpc-error・access-denied は Unknown（理由つき）。
- 観測の `config` は、宣言と意味が同じなら宣言の書き方のまま返す。違えば機器の正規形（宣言に書いた属性だけ）を返す。

Apply：

- Apply はいつも commit confirmed の two-phase Apply（`core.Transactor`）。
  - Prepare：lock・edit-config・validate・commit confirmed
  - Commit：確定の commit
  - Rollback：discard・cancel-commit か、セッションの終了
  - 確定しなければ、機器の confirm-timeout で戻る。
- `apply.transaction` を書かない宣言も同じ。Apply の中で新しい接続で読み直し、宣言どおりなら確定する。`timeout` の半分までに確かめられなければ戻す。
- 機器が `:candidate` と `:confirmed-commit` を持たなければ、何も送らない。
- commit_group は `apply.transaction.group`。check（`confirm`）には `observe(…)`・`present(…)`・`check(…)` を書ける。
- 部分木は消さない。`prune: true` の Range・`ensure: absent` は読み込み時にエラーにし、Delete は `RefuseError`。
- List は、Range の部分木のうち機器にあるものを返す（宣言に無いもの（unmanaged）の報告に使う）。

NACM で絞ったユーザー：

- 送る rpc は get-config・lock・unlock・edit-config・validate・commit・cancel-commit・discard-changes・close-session だけ。部分木の外は読み書きしない。
- access-denied で拒否されたら何も確定しない。許可すべきもの（部分木の読み書きと、上の rpc の実行）を理由に書く。
- 読むことを許されていない部分木は NACM が黙って省くので、Absent に見える。

### cli-device のルール

機種の定義の例は `clidevice/testdata/example-device.yaml`、架空の装置は `clidevice/clifake`。

機種の定義（settings の `definition`）に書くもの：観測のコマンドと読み方・変更のコマンドのテンプレート・確定と取り消し・`commit_confirmed`・`actions`。

宣言と観測：

- type は定義の item、ID は装置（例：`sw/syslog` の `sw1`）。field は item の全体で宣言する。
- 値が定義の `pattern`（値の全体）に合わなければ、読み込み時にエラーにする。コマンドへの差し込みを防ぐため。エラーの文に値は出さない。
- secret の field は `{secret: …}` でしか書けない。Apply の直前に `WriteContext.Secret` で取り出し、エラーの文・観測では `***` に伏せる。観測は「このプロセスが書いた値と同じなら、Apply した参照（世代つき）」だけを返す。
- 次の場合は Unknown で、Apply は何も流さない：
  - 出力が定義に合わない（読み方が一致しない・2 つ一致する・エラーのマーカー・プロンプトが来ない・exec が 0 以外で終わる）
  - 到達できない
- 1 回の Observe の中では、同じコマンドの出力を使い回す。

Apply：

- 流す前に観測し、宣言どおりなら流さない。
- エラーのマーカーで止める。`config.rollback` があれば取り消して、確定しない。
- 流したあとは再観測（re-observe）する。宣言どおりでなければエラー（コアが ineffective Apply の回数で halt する）。

two-phase Apply：

- 定義に `config.commit_confirmed`・`confirm`（確定）・`cancel`（任意。すぐ戻す）を書いた機種だけが使える。`commit_confirmed` の `{seconds}`・`{minutes}` には transaction.timeout が入る。
- Prepare は設定モードに入り、コマンド列と commit_confirmed を流す。Commit は新しいセッションで confirm を流す。
- どれも来なければ、装置の timeout で戻る（不変条件 17）。
- これらを書かない機種の宣言に `apply.transaction` を書くと、Prepare で拒否する。
- `config.rollback`（取り消し）は確定しない変更を捨てるだけで、timeout で戻す仕組みにはならない。

そのほか：

- 定義の `actions` は `core.Invoker`。`Ops` の `Destructive` は定義の `destructive`。到達できない・1 つもコマンドを打てていないときだけ `ErrNotSent`。
- v2 の item の `disruptive`（Apply を破壊的な変更として数える）は書けない。cli-device は `core.Classifier` を実装していないため。定義に書くと unknown key として読み込み時にエラーにする。

### 統合テスト（netopeer2、Stage 8c）

```console
$ make integration-v3-netconf
```

- netopeer2 のイメージ（`sysrepo/sysrepo-netopeer2`、tag と digest で固定）を使う。`SAKUTEI_IT_NETOPEER2_IMAGE` で差し替えられる。
- docker で 4 台（`sakutei-v3nc-sw1`・`g1`〜`g3`）を 127.0.0.1 の空いたポートに立てる。
- v3 の CLI（`cli.Run` の plan・reconcile、`netconf.Module` と `cli.WithCheck`）を、本物の NETCONF over SSH に向けて回す（`netconf/integration/`、build tag `integration_v3_netconf`）。
- 要るもの：docker・ssh・ssh-keygen・sudo（パスワードなし）。
- コンテナは終われば（落ちても）消す。

step：

1. 宣言どおりなら差分なし（空白・group のユーザーの順序が機器と違っても）。
2. 観測だけ：別のセッションで手で変えると drift（終了コード 3）になり、Apply しない。
3. commit confirmed の two-phase Apply：1.0（`apply.transaction`）・1.1 の persist・`apply.transaction` を書かない宣言。確定したものは confirm-timeout を過ぎても戻らない。
4. NACM で 2 つの部分木と必要な rpc だけを許可したユーザー：観測と two-phase Apply が通り、その間に netopeer2 が拒否したものが無い。部分木の外を書く宣言は access-denied（理由つき）になり、機器は変わらない。
5. Apply すると NACM を読めなくなる宣言（読み取りの締め出し）：check が通らず確定しない（終了コード 4・rolled_back）。機器は元に戻り、次の cycle も同じ宣言を Apply し直さない。
6. Prepare のあと Commit も Rollback もしなければ、機器の confirm-timeout で元に戻る（不変条件 17）。
7. confirm の `present(nc/subtree/g2/marker)`：別の台にマーカーが無ければ確定せず、マーカーを置けば確定する。
8. commit_group（g1〜g3）と `check(path)`：全部の台で通れば 3 台とも確定する。通らなければどの台も確定せずに戻る。
9. 機器を止めると Unknown（0 以外で終わる）。

2026-10-31 に docker 29.1.3 で通した（9 step・約 85 秒）。

既知の問題：このイメージの netopeer2-server は、コアの cycle で commit_group を戻したあと（step 8 の後半）、同じ機器に次の commit confirmed を送ると SIGSEGV で落ちた。同じ rpc の並びを provider に直接送ると落ちず、手元では原因を絞れなかった。v2 の統合テストも group を最後に回していたので、step 8 を g1〜g3 で最後に回している。

## proxmox

type：

| type | ID | field など |
|---|---|---|
| `vm` | `<クラスタ>/<VM 名>` | `vmid`・`node`・`template`（作るときだけ）・`pool`・`cores`・`memory`・`disks`（宣言した slot だけ。拡大だけ。`keep`・`volume=`）・`nics`（宣言した slot・キーだけ。`firewall`・`mtu`）・`firewall`（VM の FW の options。書いたキーだけ）・`ci_user`・`ci_ssh_keys`・`ci_ip`・`ci_gateway`・`ci_dns`・`ciupgrade`・`power`・`apply_pending`。観測だけ：`lock`・`pending`・`current_values` |
| `fw-rules` | `<クラスタ>/<VM 名>` | `rules`：Sakutei のマーカーのあるルールの並び |
| `volume` | `<クラスタ>/<VM 名>/unusedN` | 観測だけ。付け先の無い volume |
| `fw-rule` | `<クラスタ>/<VM 名>/<位置>` | 観測だけ。マーカーの無いルール |

- 比べ方は `FieldSpec.Match`。
- volume の付け替え（承認が必要な破壊的な変更）とルールの変更（破壊的な変更）は `core.Classifier`。
- keep のディスクが残る VM の削除は拒否する。
- 一回きりの操作：`reboot`・`reset`（破壊的）・`stop`（破壊的）。

### 接続設定と intent の例

```yaml
# config.yaml（接続設定だけ。観測用と書き込み用のトークンを分ける）
providers:
  pve:
    module: proxmox
    settings:
      settle: 30s                           # Apply のあと読み直すまで（既定 30s）
      clusters:
        lab:
          endpoint: https://pve1.example.net:8006
          api_base_path: /api2/json         # 省けば /api2/json
          node: pve1                        # node を宣言しない VM を作るノード
          ca_file: /etc/sakutei/pve-ca.pem  # か insecure_skip_verify: true
          task_wait: 2m                     # Apply が仕事（UPID）を待つ長さ
          observe: {token_id: "sakutei-ro@pve!audit", token_file: /etc/sakutei/pve-ro.token}   # / に PVEAuditor
          write:   {token_id: "sakutei@pve!sakutei", token_file: /etc/sakutei/pve-rw.token}
```

```yaml
# intent
pools:
  - {name: vmids, range: "500-599"}
ranges:
  - provider: pve
    args: {cluster: lab}                    # List と、宣言に無いもの（unmanaged）を報告する Range
    external_owners:
      - {owner: stage0, id: vm/lab/sakutei-ctl, reason: "Sakutei 自身が動く VM"}
resources:
  - type: pve/vm
    id: lab/mon1
    fields:
      vmid: {from_pool: vmids}
      template: debian-13-cloud             # 作るときだけ使う full clone の元
      pool: apps
      cores: 2
      memory: 4G
      disks: "scsi0 storage=local-lvm size=32G, scsi1 storage=local-lvm size=100G keep"
      nics: "net0 bridge=vmbr0 vlan=20 firewall=1"
      firewall: "enable=1 policy_in=DROP policy_out=ACCEPT"
      ci_user: ops
      ci_ip: 192.0.2.21/24
      ciupgrade: false
      power: running
  - type: pve/vm                            # 作り直し：前の VM の keep のディスクを引き取る
    id: lab/mon2
    fields:
      vmid: 601
      template: debian-14-cloud
      disks: "scsi0 storage=local-lvm size=32G, scsi1 volume=local-lvm:vm-500-disk-1 keep"
  - {type: pve/vm, id: lab/old, ensure: absent}
network_policy: …                           # targets の realizer: proxmox-fw・provider: pve・id: lab/mon1
```

### proxmox のルール

比べ方：

- 宣言した field だけを比べる。
- `disks`・`nics` は宣言した slot だけを比べる。`nics` の `mac`・`model`・`firewall`・`mtu` と、`firewall` の options は書いたキーだけを比べる。
- 書き方の違いは drift にしない（provider の正規形、`FieldSpec.Match`）。例：`memory` の `4G` と `4096`、DNS の区切り。
- `template` は作るときだけ使う。

変更の制限：

- ディスクは拡大だけ。縮小・ストレージの変更は Apply のエラー。
- `vmid`・`node` を今の VM と違う値にするのも Apply のエラー（番号は変えない・移行は扱わない）。

pending と非同期のタスク：

- 走っている VM の hotplug できない変更（`cores`・`memory` など）は、Proxmox が pending に置く。
  - 宣言の値が pending にあれば drift にせず、重ねて書かない（観測の `pending` に名前が出る）。
  - `apply_pending: reboot` なら 1 回再起動して反映する。
- 非同期のタスク（作成・複製・resize・電源・削除）は、Apply が `task_wait` まで待つ。
  - 待ちきれなくてもエラーにせず戻る。
  - その VM のタスクが走っている間（`/nodes/<node>/tasks?source=active`）と lock がある間、観測は Converging。provider は UPID を覚えない。

電源：

- 停止は正常停止（`power: stopped`）だけ。
- 強制停止・再起動は Procedure の `invoke`（`stop`・`reset` は破壊的な操作、`reboot`）。

ディスクの引き継ぎ（`volume=` と `keep`）：

- `volume=` の付け替えは、承認が必要な破壊的な変更。持ち主の VM から Proxmox の reassign で外して付ける。
  - 該当する Policy に approval が無ければ、申請者でない 1 人の承認を求める。`sakutei approve` で揃うまで Apply しない。
  - 持ち主の VM が走っていれば、何も変えずに待つ（持ち主には `power: stopped` を宣言する）。
  - 付けたあとは volume の名前を比べない。
- keep のマーカー（VM の tags の `sakutei-keep-<slot>`）のあるディスクか unused が残る VM の `ensure: absent` は、何も書かずに拒否する（`DeleteRefused`）。
- 付け先の無い volume（`unusedN`）とマーカーの無い FW のルールは、観測だけの type `volume`・`fw-rule` として、宣言に無いもの（unmanaged）に出す。消さない。

そのほか：

- 触らない VM は Range の `external_owners`（`id: vm/<クラスタ>/<VM 名>`）に書く。その VM を宣言すると読み込み時にエラーにする。
- 観測用のトークンに要るのは `/` の `PVEAuditor`（`/cluster/resources`・`/status/current`・`/pending`・`/firewall/*`・他人のタスクの一覧を読むため）。
- 書き込み用のトークンは Apply・Delete・Invoke の中でだけ読む。

### 統合テスト（入れ子の Proxmox VE）

```console
$ make integration-v3-proxmox
```

- 入れ子の Proxmox VE 9.2 を使う。
  - ISO はバージョンと sha256 で固定する。自動導入の ISO を docker で作り、qemu の入れ子の KVM で導入する。
  - ISO・自動導入の ISO・導入済みのディスクは、同じキャッシュ `SAKUTEI_IT_CACHE`（既定 `~/.cache/sakutei-it/proxmox`）を使い回す。
- 導入済みのディスクの上の使い捨ての overlay で立て、v3 の sakutei（`cmd/sakutei`）の CLI を向けて回す。
- 終われば（落ちても）qemu を止める。
- 要るもの：KVM・qemu・docker・curl・ssh・sudo（パスワードなし）。初回は ISO とクラウドイメージを取得するネットワーク。

step（`proxmox/integration/proxmox_test.go`）：

1. テンプレートから作る：採番した vmid（500〜509）で、Debian 13 のクラウドイメージのテンプレートから複製する。resize・VLAN の NIC・cloud-init・起動まで、1 回の reconcile で収束する。cloud-init のユーザーと鍵で VM に ssh で入れ、ホスト名・アドレス・CPU の数・ディスクの大きさが宣言どおり。
2. 何 cycle 回しても宣言どおりのまま：reconcile を続けても終了コード 0。plan に変更の操作が無く、触らない VM（external_owners）とテンプレートは出ない。
3. 手でメモリを変える（走っている VM なので pending に置かれる）と、plan が memory だけの drift を出す。reconcile が戻し、pending が消える。
4. `cores` を変えると pending に置かれ、次の reconcile は重ねて書かない（終了コード 0）。`apply_pending: reboot` を足すと 1 回再起動して反映され、VM の中の `nproc` が 3 になる。
5. 触らない VM（`external_owners` の `vm/it/sakutei-ctl`）を宣言すると、読み込み時にエラーにする（終了コード 2）。その VM の設定の digest は変わらない。
6. qemu を SIGSTOP で止めると、plan はその VM を unknown にする。戻せば収束する。
7. 宣言から外すと、VM は宣言に無いもの（unmanaged）に出る。reconcile しても消えない（触らない VM・テンプレートは出ない）。
8. `ciupgrade: false`・NIC の `firewall=1`・`mtu=1450`・VM の FW の options で作る（止めたまま）。
   - 手で NIC と options を変えると、観測用のトークン（`/` に `PVEAuditor`）の plan が nics と firewall の drift を出す。
   - そのトークンだけの reconcile は書けずに失敗する。
   - 書き込み用のトークンを分けた設定の reconcile が戻す（書いていないキー `ipfilter` は残す）。
9. network_policy の realizer proxmox-fw：
   - 手のルールを置いた VM に、interface と向き（net0 は出る向き・net1 は入る向き、IPv4 と IPv6）の許可を Apply する。
   - `/etc/pve/firewall/550.fw` の先頭に、マーカーと reason の comment つきのルールが宣言の順で入る。手のルールは後ろに残り、type `fw-rule` の unmanaged に出る。
   - 置いたルールの port を手で変え、先頭に手のルールを置くと、観測用のトークンの plan でも drift になる。reconcile が戻し、手のルールは消さない。
10. 作り直しでデータのディスクを引き継ぐ：
    - keep のマーカー（tags）のある前の VM の `ensure: absent` は拒否され、消えない。
    - 新しい VM の `volume=` は承認が揃うまで作らない（`sakutei approve`）。承認のあと reassign で付け替え、中身（LV に書いたマーカー）が残る。
    - 前の VM はそのあと消せる。keep でないディスクは一緒に消え、付け替えたディスクは残る。
11. pool に入れて作り、手で pool から外すと、plan が pool の drift を出して reconcile が戻す。存在しない pool は `does not exist` のエラー。
12. 承認つきの削除：`approval` の Policy に該当する `ensure: absent` は、承認の前には消さない。`sakutei approve` のあとに、走っている VM を止めて消す。ほかの VM には触れない。

2026-10-31 に Linux 7.0・qemu の入れ子の KVM・Proxmox VE 9.2 で 12 step とも通した（キャッシュの導入済みのディスクを使い、lab の起動を含めて約 280 秒。作る VM は 1 回の reconcile で収束した）。

実機で見つけたこと（Proxmox VE 9.2）：

- 作った直後の VM は、`/cluster/resources` に仮の名前 `VM <vmid>` で載る。名前が空のときと同じく、VM の設定で確かめる。偽の Proxmox の `ResourceLag` もこれを真似る。
- 複製した VM のディスクの volume の番号は Proxmox が決める（テンプレートの cloud-init のドライブなどで番号が飛ぶ）。

## guest-os

- 宣言できるのは `markers` と、期待する値 `os_id`・`os_version_id`・`hostname` だけ。
- marker のパスは Resource の field `markers`（名前 → 絶対パス）に書く。中身は観測の `marker`（名前 → 値、4 KiB まで）に入り、Condition から `observe(guest/os/node1).marker.installed` のように読む。
- 到達できない・認証の失敗・ホスト鍵の不一致・壊れた出力・読めない marker は Unknown。

### 接続設定と intent の例

```yaml
# config.yaml（接続設定だけ。観測用のユーザーでよい）
providers:
  guest:
    module: guest-os
    settings:
      hosts:
        node1:
          ssh: {address: 192.0.2.21, user: monitor, key: /etc/sakutei/id_ed25519, known_hosts: /etc/sakutei/known_hosts}
```

```yaml
# intent（marks は Resource の field）
resources:
  - type: guest/os
    id: node1
    fields:
      markers: {installed: /etc/sakutei-installed, cloud-init: /var/lib/cloud/instance/boot-finished}
procedures:
  - name: os-install
    target: {type: bmc/system, id: node1}
    trigger: manual                                # sakutei start os-install で起こす
    steps:
      # ISO を差す・1 回だけ CD 起動・電源 Off を待つ・On は redfish の段（E-A8）
      - name: booted
        wait: {when: 'observe(guest/os/node1).marker.installed == "sakutei-2026-10-27"', timeout: 30m}
```

### guest-os のルール

- marker のパスは観測のたびに読み直す。コアは宣言を Observe に渡さないので、Validate が宣言の `markers` を ID ごとに覚え、Observe がそれを読む。
- 宣言の無い ID（Condition からだけ参照される）は、marker なしで観測する。
- marker のファイルが無いのは観測の事実で、`marker` にそのキーが無いだけ（Condition では null）。
- 読めない marker（権限・ディレクトリ・4 KiB より大きい）は Unknown。
- 存在しないパスを「無い」とするのは、一番近い上位のディレクトリを読めるときだけ。読めない観測用のユーザーでは区別できないので Unknown。
- ホストに要るのは sh・dd・od だけ（busybox でもよい）。

## postgresql・sqlmigrate・ldap

postgresql の type：

| type | ID | field |
|---|---|---|
| `role` | `<server>/<ロール>` | `login`・`superuser`・`createdb`・`createrole`・`inherit`・`bypassrls`・`replication`・`connection_limit`・`member_of`・`password`（secret の参照） |
| `database` | `<server>/<DB>` | `owner`・`connection_limit`・`encoding`・`lc_collate`・`lc_ctype`・`template`・`connect_roles`。中身を失う type なので、宣言に無くても prune しない |
| `grant` | `<server>/<DB>/<ロール か PUBLIC>` | `privileges` |
| `extension` | `<server>/<DB>/<拡張>` | `version` |

sqlmigrate の type `database`：ID は settings の `databases` の名前。field は観測だけで、`version`（schema_migrations の最大のバージョン）・`latest`・`pending`。

ldap の type `entry` の field：

- `attributes`：属性名 → 値。値の集合で比べる。
- `user_password`：secret の参照。書き込みだけ。
- `user_password_set`：観測だけ。

### 接続設定と intent の例

```yaml
# config.yaml（接続設定だけ）
secret_backends:
  vault: {module: vault-kv2, settings: {address: "https://vault.example.com:8200", token_file: /etc/sakutei/vault-token}}
providers:
  pg:
    module: postgresql
    settings:
      psql: /usr/bin/psql                 # 省略可（既定は PATH の psql）
      servers:
        main:                             # サーバの名前（ID の先頭）
          host: db.example.net            # / で始まれば Unix ソケットのディレクトリ
          port: 5432
          dbname: postgres                # 管理用につなぐ DB（既定 postgres）
          sslmode: verify-full
          sslrootcert: /etc/sakutei/pg-ca.pem
          connect_timeout: 10s            # 既定 10s
          verify_passwords: false         # true で手で変えたパスワードをずれにする（観測用のロールに pg_authid の列が要る）
          observe: {user: sakutei_ro, password_file: /etc/sakutei/pg-ro}     # 観測用（LOGIN だけでよい）
          write: {user: sakutei, password_file: /etc/sakutei/pg-admin}       # 省略可。Apply・Delete だけが使う（E-G5）
  db:
    module: sqlmigrate
    settings:
      lock_timeout: 5s                             # DB の鍵（SQLite の書き込みの鍵・PostgreSQL の advisory lock）を待つ長さ
      databases:
        app:                                       # engine: sqlite（既定）
          path: /var/lib/app/app.db
          migrations: /etc/sakutei/migrations/app  # NNNN_<名前>.up.sql / .down.sql
        orders:
          engine: postgresql
          host: db1.example.net
          database: orders                         # 省けば ID
          sslmode: verify-full
          connect_timeout: 5s
          migrations: /etc/sakutei/migrations/orders
          observe: {user: sakutei_ro, password_file: /etc/sakutei/pg-ro}
          write: {user: sakutei_migrate, password_secret: "vault:/db/orders#migrate"}   # か password_file
  dir:
    module: ldap
    settings:
      ldapsearch: /usr/bin/ldapsearch       # 省略可（既定は PATH の ldapsearch）
      ldapmodify: /usr/bin/ldapmodify       # 省略可
      servers:
        dir1:                               # サーバの名前（ID の先頭）
          uri: ldap://ldap.example.net:389  # ldap:// か ldaps://（1 つだけ）
          starttls: true                    # ldap:// で StartTLS を必須にする（-ZZ）
          ca_cert: /etc/sakutei/ldap-ca.pem # 省略可（LDAPTLS_CACERT。証明書はいつも確かめる）
          timeout: 10s                      # 省略可（既定 10s）
          observe: {bind_dn: "uid=sakutei-read,ou=system,dc=example,dc=com", password_file: /etc/sakutei/ldap-read}
          write: {bind_dn: "cn=Directory Manager", password_file: /etc/sakutei/ldap-dm}   # 省けば observe を使う
```

```yaml
# intent
ranges:
  - {provider: pg, args: {server: main}, prune: true}    # ロール・権限・拡張の unmanaged を prune（DB は消さない）
  - provider: dir
    args: {server: dir1, base_dn: "dc=example,dc=com", filter: "(!(ou=outside))", scope: sub}
    prune: true                                          # unmanaged を消す（子のある entry は消さない）
resources:
  - type: pg/role
    id: main/app
    fields: {login: true, connection_limit: 10, member_of: [pg_read_all_data], password: {secret: "vault:/db/app#password"}}
  - type: pg/database
    id: main/appdb
    fields: {owner: app, encoding: UTF8, lc_collate: C, lc_ctype: C, template: template0, connect_roles: [sakutei_ro]}
    depends_on: [{id: pg/role/main/app}]
  - type: pg/grant
    id: main/appdb/PUBLIC
    fields: {privileges: []}                             # PUBLIC から全部外す
    depends_on: [{id: pg/database/main/appdb}]
  - type: pg/extension
    id: main/appdb/hstore
    fields: {version: "1.8"}
    depends_on: [{id: pg/database/main/appdb}]
  - {type: pg/database, id: main/olddb, ensure: absent} # ユーザーのテーブルがあれば拒否する（RefuseError）
  - {type: db/database, id: orders, fields: {version: 3}}
  - type: dir/entry
    id: dir1/ou=people,dc=example,dc=com
    fields: {attributes: {objectClass: [top, organizationalUnit], ou: People}}
  - type: dir/entry
    id: dir1/uid=alice,ou=people,dc=example,dc=com      # DN は正規形（小文字・空白なし）
    fields:
      attributes:
        objectClass: [top, inetOrgPerson]               # サーバが足す上位のクラスは在ってよい
        uid: Alice                                      # 作るときの RDN の綴り
        cn: Alice Liddell
        sn: Liddell
        mail: [alice@example.com, al@example.com]       # 値の集合（順は問わない）
        uidNumber: "1000"                               # 数も文字列で書く
      user_password: {secret: "vault:/ldap/alice#password"}
    depends_on: [{id: dir/entry/dir1/ou=people,dc=example,dc=com}]
procedures:
  - name: migrate-orders                           # v2 の strategy: task を Procedure に書き直したもの
    target: {type: db/database, id: orders}
    trigger: on_drift
    drift_fields: [version]
    steps:
      - name: approve
        gate: {approval: {quorum: {n: 1, of: [bob]}}}
      - name: migrate
        invoke: {op: migrate, args: {version: "{desired.version}", rollback_on_fail: true}}
    done_when: 'observe(db/database/{target}).version == desired.version'
```

### postgresql のルール

psql の呼び出し：

- psql はシェルを通さずに呼ぶ。SQL は標準入力で渡す。
- 接続の設定は子プロセスの環境変数で渡す。親の `PG*` は引き継がない。
- 名前はいつも quote する。
- パスワードのファイルは呼ぶたびに読み、0600 の一時的な passfile で渡して消す。

ロールのパスワード：

- Apply の直前に `WriteContext.Secret` で取り出し、SCRAM-SHA-256 の検証値にして送る。平文はサーバのログに出ない。
- ロールのコメントに、参照（世代つき）と検証値の sha256 を書く。観測はその参照を返すので、secret の世代が変わったときだけ設定し直す。
- `verify_passwords` なら、pg_authid の検証値の sha256 が合わなければ drift。pg_authid を読めなければ、そのロールは Unknown。
- v2 が書いたコメントの形式とは違う。v2 でパスワードを設定したロールは、1 回設定し直す。

観測と比べ方：

- 宣言と意味が同じなら、宣言の書き方を返す（encoding の `UTF-8`・ロケールの codeset・権限の `TEMP`・小文字）。`connect_roles` は、宣言が観測に含まれていれば同じとみなす。
- `template` は観測できないので、作ったあとは比べない。
- 作ったあとに `encoding`・`lc_collate`・`lc_ctype` を変える宣言は、Apply が拒否する（作り直さない）。

権限：

- PUBLIC の権限は DB があれば存在し、宣言どおりにする（空の集合で全部外す）。List に出さない（宣言しなければ触らない）。`ensure: absent` は書けない。
- ロールの権限は集合ちょうどにする。空にはできない。
- `connect_roles` は管理の境界の外のロール（接続に使うロール・`pg_*`）にだけ書ける。DB を作る Apply の中で CONNECT を与える。足すだけで外さない。

管理の境界：

- `pg_*`・接続に使うロール・`template0`・`template1`・`postgres`・管理用の DB・所有者の権限・`plpgsql` は List に出さない。宣言すると読み込み時にエラーにする（`plpgsql` だけは宣言できる）。
- List が完全なのは、snapshot と全部の DB の拡張を読めたときだけ。
- データベースは中身を失う type なので、宣言に無くても prune しない（不変条件 9）。v2 は空の unmanaged な DB を消したが、v3 では `ensure: absent` で名指しする。

エラーと削除：

- 到達できない・認証の失敗・psql が無い・パスワードのファイルを読めない・壊れた出力は Unknown。
- Delete は次のものを `RefuseError` で拒否する。CASCADE・DROP OWNED は使わない。
  - ユーザーのテーブルがある DB・接続を許可しない DB
  - DB か権限を持つロール
  - PostgreSQL が 2BP01 で拒否するロール・拡張

### sqlmigrate のルール

- type `database` は観測だけ。宣言の `version` は収束ループが Apply せず、Procedure が `migrate` で合わせる。
  - v2 の `strategy: task` は、`trigger: on_drift`・`invoke: migrate`・`done_when` に書き直す（上の例の `migrate-orders`）。
  - バージョンを変える操作は `invoke` の `migrate`（上げ下げ・`rollback_on_fail`）と `rollback`（下げるだけ）だけ。どちらも破壊的な操作。
- 読み込み時にエラーにするもの：ディレクトリに無いバージョン・観測だけの field・`ensure: absent`・Range。
- 書き込み用の credential（`password_file` か secret の参照 `password_secret`）は Invoke の中でだけ読む。

トランザクション：

- 1 ファイルは 1 つのトランザクション（BEGIN … SQL … schema_migrations の完了の記録 … COMMIT）。失敗した 1 ファイルだけが巻き戻り、前に流したバージョンは残る。
- `rollback_on_fail: true` なら、呼んだときのバージョンまで down で戻してから halt する。Procedure の `on_failure` で戻す step はまだ無く、いまは halt するだけ。
- トランザクションに入れられない文（CREATE INDEX CONCURRENTLY）は、25001 の失敗として halt する。
- 流す前に必要な SQL を全部読む。sqlite3 のドットコマンド・psql のメタコマンド・足りない down があれば、何も流さない。
- ファイルの中に COMMIT・ROLLBACK を書くと、「確定したか分からない」エラーで halt する。

並行実行：

- 2 つのプロセスが同じ DB を動かしても、同じバージョンを二度流さない。
- 1 ファイルごとにロックを `lock_timeout` まで待って取り、バージョンが流す前のままかを確かめる。違えば何も流さず、読み直して続ける。
  - SQLite：BEGIN IMMEDIATE
  - PostgreSQL：v2 と同じキーの `pg_advisory_xact_lock`

エラー：

- `ErrNotSent`（次の cycle に送り直す）を返すのは、まだ 1 ファイルも確定していないときの、次の場合だけ：接続できない・認証の失敗・sqlite3／psql が無い・パスワードを読めない・ロックを待ちきれない。
- SQL のエラー・確定したあとのエラー・途中で接続が切れたときは、Procedure を halt する。
- 読めない（到達できない・壊れている・データベースが無い・ロックを待ちきれない）は Unknown。
- SQLite のファイルが無いのはバージョン 0。ただし、置き場所のディレクトリがあり sqlite3 が起動できるときだけ。

### ldap のルール

比べ方：

- 比べるのは宣言した属性だけ。宣言に無い属性は触らない。
- 属性名の大文字小文字は区別しない。
- 値の集合を、その一覧ちょうどにする。`""` と `[]` は「その属性が無い」。
- DN を値に持つ属性（member・uniqueMember・owner・seeAlso・manager など）は、DN の正規形で比べる。
- 観測の `attributes` は宣言した属性だけ。意味が同じなら宣言の書き方のまま、違えばサーバの値（並べた一覧か 1 つの文字列）を返す。

Range：

- Range の `scope` は LDAP の検索の scope（`sub`・`one`）。v2 の scope（ID の `site=tokyo`）は v3 には無い。

読み込み時にエラーにするもの（値はエラーの文に出さない）：

- 正規形でない ID
- どの Range にも入らない宣言（base_dn そのもの・scope が one のときの孫）
- bind の DN
- 同じサーバで重なる Range
- objectClass の無い宣言
- 大文字小文字だけが違う同じ属性
- 操作属性（modifyTimestamp など）
- `attributes` の中の userPassword
- 文字列でない値・空の値
- オプションつきの属性名（`cn;lang-ja`）
- RDN の値を含まない属性
- `user_password_set` の宣言
- settings の base_dn・filter・scope

`user_password`（書き込みだけ）：

- 値は Apply の中で `WriteContext.Secret` で取り出し、ldapmodify の標準入力（base64 の LDIF）でだけ渡す。
- サーバに userPassword があれば宣言どおりとみなし、世代を上げても書き直さない。替えるときは、サーバの userPassword を消せば次の cycle に今の値を書く。
- 観測用の DN にも userPassword の read を許可する。読めないと「無い」に見えて、毎 cycle 書いてしまう。

bind の credential：

- bind のパスワードは settings の `password_file` から毎回読み、0600 の一時ファイルで `-y` に渡して消す。引数・環境変数・エラーの文には出さない。
- 観測と List は `observe` の credential だけ、Apply・Delete は `write` の credential だけを使う。書き込み用のファイルが無くても、plan は観測できる。
- 親の LDAP で始まる環境変数は引き継がない。`LDAPTLS_REQCERT=demand`。

観測と List：

- 到達できない・timeout・bind と TLS の失敗・コマンドが無い・パスワードのファイルを読めない・読めない出力・スキーマを読めない、は Unknown（理由つき）。
- noSuchObject（32）だけが Absent。
- List は Range をページに分けて検索する。最後まで読めなければエラー（件数の上限で切れたときも）。
- base_dn そのもの・bind の DN・サブエントリ・tombstone は List に出さない。

Apply と Delete：

- Apply は宣言と違う属性だけを、1 つの modify の replace で書く。宣言どおりなら何も書かない。
- 無ければ add で作る（葉の RDN は宣言の綴り）。親が無ければエラーなので、親を `depends_on` に書く。
- Delete は、子のエントリがあるエントリと、宣言したエントリの上位のエントリを消さない（`RefuseError`）。
- コアは同じ cycle の中で、宣言に無い親の prune を子の作成より先に流すことがある。そのため、子がまだ作られていなくても拒否する（統合テストで見つけた）。

### 統合テスト（postgres、Stage 8e）

```console
$ make integration-v3-postgresql
```

- 公式の postgres イメージ（`postgres:17.11-bookworm`、tag と digest で固定）を使う。`SAKUTEI_IT_POSTGRES_IMAGE` で差し替えられる。
- サーバと、psql を動かすだけのクライアントを docker のネットワークに立て、v3 の CLI（`cli.Run` の plan・reconcile と daemon）を向ける。
- コンテナとネットワークの名前：
  - postgresql provider（`postgresql/integration/`）：`sakutei-v3pg-server`・`sakutei-v3pg-client`・`sakutei-v3pg-net`
  - sqlmigrate（`sqlmigrate/integration/`）：`sakutei-v3sm-*`
- build tag は両方 `integration_v3_postgresql`。
- ホストに psql は要らない。ラッパーのスクリプトで、クライアントのコンテナの中で動かす。
- 要るもの：docker・sudo（パスワードなし）。
- コンテナとネットワークは終われば（落ちても）消す。

postgresql の step：

1. 空から作る：ロール（引用符を含む名前も）・DB（template0・C ロケール）・権限・拡張を作り、PUBLIC から全部外す。宣言のパスワードで接続でき、pg_authid は SCRAM の検証値。宣言していない DB の PUBLIC は既定のまま。
2. 手で変えると drift になり（パスワードも verify_passwords で）、1 cycle で戻る。
3. PUBLIC と BYPASSRLS を手で変えると戻る。宣言していない属性（REPLICATION）は触らない。
4. secret の新しい世代でパスワードを設定し直す（古いものでは接続できない）。拡張のバージョンを上げる。作ったあとの encoding の変更は Apply が拒否する（終了コード 1、DB は変わらない）。
5. 何 cycle 回しても drift にならず、パスワードを設定し直さない。
6. ユーザーのテーブルがある DB の `ensure: absent` は拒否し（refused、行は残る）、空の DB は消す。宣言に無い DB は unmanaged に出し、prune しない。宣言に無いロールは prune し、`pg_*`・管理用のロールは消さない。
7. サーバを止めると、全部 Unknown・List が不完全で、何も Apply しない（終了コード 3）。
8. 観測用（LOGIN だけ）と書き込み用のロール：
   - 書き込み用のパスワードのファイルが無くても plan は動き、reconcile は書けない。
   - 観測用のロールが流すのは `/* sakutei:… */` の読み取りクエリだけ（サーバのログで確かめる）。CREATE DATABASE は書き込み用のロールが流す。
   - 接続に使う 2 つのロールは List に出ず、prune しない。
9. verify_passwords：観測用のロールが pg_authid を読めなければ Unknown（理由に pg_authid）。管理用の DB で列の SELECT を与えるか、pg_read_all_data があれば比べられる（別の DB で与えた列の権限は効かない）。
10. connect_roles：PUBLIC の CONNECT を外した DB に観測用のロールが接続でき、List は完全。手で REVOKE すると、DB は drift・拡張は Unknown・List は不完全になり、1 cycle で戻る（その cycle は List が不完全なので終了コード 3）。宣言から外しても REVOKE しない。

sqlmigrate の step（観測は読み取り専用のロール、invoke はデータベースの所有者のロール。そのパスワードは secret_backends から `password_secret` で取り出す）：

1. one-shot 実行の reconcile で、Procedure（on_drift → migrate → done_when）が 0 から 3 まで流す（E'' と $$ の引用・最後に ";" の無いファイルを含む）。次の plan は何もしない。
2. daemon が、宣言の 1 まで down を逆順に流す。
3. 失敗する 0004：Procedure は止まり（42P01・バージョン 3）、0004 は巻き戻る。次の cycle も送り直さない。
4. `rollback_on_fail`：失敗したら、呼んだときのバージョン（1）に戻して止まる。
5. CREATE INDEX CONCURRENTLY は 25001 で止まり、何も残さない。
6. 2 つの daemon（別々の Record）：テストが advisory lock を持つ間は、どちらも未送信（55P03）。外すと同じバージョンを二度流さずに 3 になり、両方の Procedure が終わる。
7. 書き込み用の secret が無い：plan は観測用の credential で読めて差分を出す。reconcile は何も流さず、未送信になる。secret を戻すと収束する。
8. サーバを止める（docker pause）と Unknown（終了コード 3）で、何もしない。

2026-10-31 に docker 29.1.3 で通した（postgresql は 2 テスト・15 step で約 150 秒、sqlmigrate は 8 step で約 43 秒）。

### 統合テスト（389 Directory Server、Stage 8e）

```console
$ make integration-v3-ldap
```

- 389 Directory Server のイメージ（`389ds/dirsrv:3.1`、tag と digest で固定）を使う。`SAKUTEI_IT_389DS_IMAGE` で差し替えられる。
- docker で立てるもの：サーバ `sakutei-v3ldap-server`・クライアント `sakutei-v3ldap-client`・ネットワーク `sakutei-v3ldap-net`。
- v3 の CLI（`cli.Run` の plan・reconcile、`ldap.Module`）を向けて回す（`ldap/integration/`、build tag `integration_v3_ldap`）。
- ldapsearch・ldapmodify はクライアントのコンテナの中で動かす（ラッパー。-y のファイルと CA は docker cp で渡す）。
- 要るもの：docker・sudo（パスワードなし）。
- コンテナとネットワークは終われば（落ちても）消す。

step：

1. 空から作る（StartTLS）：plan は全部 create。reconcile で作り、objectClass にサーバが上位のクラスを足しても宣言どおり。userPassword はハッシュで保存され、宣言のパスワードで bind できる。`,`・`"`・Ü・改行を含む値と、空白入りの member の DN も扱う。
2. 手で変える（mail・description・member・objectClass の追加・宣言に無い telephoneNumber）と drift になり、宣言した属性だけを戻す。
3. userPassword は書き込みだけ：新しい世代でも書き直さない。サーバの userPassword を消すと、次の cycle に今の値を書く。
4. 何 cycle 回しても書かない（5 cycle、modifyTimestamp が変わらない）。
5. LDAPS でも宣言どおり。誤った bind のパスワードでは全部 Unknown（理由つき）で、List は不完全。
6. prune の範囲：宣言に無い葉は消す。宣言した子を持つ、宣言に無いエントリは消さない（RefuseError）。filter の外と base_dn は消さない。
7. 観測は読み取り専用の DN（ACI で read・search・compare だけ）、書き込みは Directory Manager：手で変えたものを戻し（modifiersName は Directory Manager）、bind の DN は unmanaged に出ない。
8. サーバを止めると全部 Unknown（終了コード 3）で、List は不完全なので prune しない。起動し直すと宣言どおりに戻る。

2026-10-31 に Linux 7.0・docker 29.1.3 で通した（8 step・約 99 秒）。

## s3・ansible・template-file

s3 の type `bucket`：

- ID は `<endpoint>/<バケット>`。
- field：
  - `versioning`
  - `policy`（マップ。IAM の書き方の違いをそろえた正規形で比べる）
  - `lifecycle`（ルールの一覧。値を文字列にし、順序を問わない正規形で比べる）
  - `tags`（マップ）
  - `object_lock`（`enable_on_create`・`mode`・`days` か `years`・`enabled`。期間は Days にそろえて比べる）
  - 観測だけ：`creation_date`・`unsupported`

ansible の type `call`：

- ID は利用者が付ける名前。
- field：`host`（inventory のホスト 1 台）・`module`（FQCN）・`args`（モジュールの引数、Map。値はテンプレートとして評価しない）・`destructive`（既定 false）。

template-file の type `file`：

- ID は `output_dir` からの相対パス。
- field：`template`（`template_dir` からの相対パス、必須）・`hostname`・`address`・`ssh_authorized_keys`・`install_mark`・`vars`（名前 → 文字列）。
- テンプレートが受け取る値は `.ID`・`.Hostname`・`.Address`・`.SSHAuthorizedKeys`・`.InstallMark`・`.Vars.<名前>`。関数は `shquote`・`json`・`lines`（missingkey=error）。

### 接続設定と intent の例

```yaml
# config.yaml（接続設定だけ）
providers:
  s3:
    module: s3
    settings:
      endpoints:
        minio:
          endpoint: https://s3.example.net:9000
          api_base_path: /s3                            # 省けば前置き無し
          region: us-east-1                             # 省けば us-east-1
          addressing: path                              # path（既定）か virtual
          ca_file: /etc/sakutei/s3-ca.pem               # か insecure_skip_verify: true（明示したときだけ）
          fields: [versioning, policy, lifecycle, tags, object_lock]   # 省けば object_lock の外の 4 つ
          storage_class_aliases: {VENDOR_COLD: GLACIER}
          observe: {access_key: sakutei-ro, secret_key_file: /etc/sakutei/s3-ro.secret}
          write:   {access_key_file: /etc/sakutei/s3-rw.id, secret_key_file: /etc/sakutei/s3-rw.secret}   # 省けば observe
  ans:
    module: ansible
    settings:
      inventory: /etc/sakutei/ansible/inventory.ini   # 絶対パス
      ansible: /opt/ansible/bin/ansible               # 省くと PATH の ansible
      env: {ANSIBLE_CONFIG: /etc/sakutei/ansible/ansible.cfg}
      timeout: 5m
      max_parallel: 4
  answers:
    module: template-file
    settings:
      template_dir: /etc/sakutei/templates
      output_dir: /srv/http/answers
```

```yaml
# intent
ranges:
  - {provider: s3, args: {endpoint: minio, prefix: app-}}
resources:
  - type: s3/bucket
    id: minio/app-logs
    fields:
      versioning: Enabled
      lifecycle: [{ID: expire-logs, Status: Enabled, Filter: {Prefix: logs/}, Expiration: {Days: 30}}]
      tags: {env: prod}
      object_lock: {enable_on_create: true, mode: COMPLIANCE, years: 7}
  - {type: s3/bucket, id: minio/app-old, ensure: absent}   # 中身があれば拒否する
  - type: ans/call
    id: motd-web1
    fields:
      host: web1
      module: ansible.builtin.lineinfile
      args: {path: /etc/motd, line: managed by sakutei, create: true}
  - type: answers/file
    id: node1/answers.sh                                   # output_dir からの相対パス
    fields:
      template: alpine-answers.sh.tmpl
      hostname: node1
      address: 10.0.2.15/24
      ssh_authorized_keys: ssh-ed25519 AAAA... sakutei
      install_mark: install-2026-10-27
      vars: {gateway: 10.0.2.2}
```

### s3 のルール

- endpoint の `fields` に無い設定は読まない。その field を宣言すると読み込み時にエラーにする。`object_lock` は `fields` に書いたときだけ扱う。
- endpoint の設定にはほかに `storage_class_aliases`・`api_base_path` がある。
- 空のポリシー・ルール・タグは `{}`・`[]`・`{}` と書く（宣言すれば外す）。
- 既にあるバケットの Object Lock の保持の設定を変えるのは、向きによらず承認が必要な破壊的な変更（`core.Classifier`）。
- バケットは宣言に無くても消さない（DestructiveDelete）。中身のあるバケットの `ensure: absent` は拒否する。
- Range は `args.endpoint`・`args.prefix`。接頭辞が重なる Range は読み込み時にエラーにする。
- 観測用と書き込み用の credential を分ける。書き込み用は Apply・Delete の中でだけ読む。
- 観測用のユーザーに要る許可：
  - `s3:ListAllMyBuckets`・`s3:GetBucketVersioning`・`s3:GetBucketPolicy`・`s3:GetLifecycleConfiguration`・`s3:GetBucketTagging`
  - `object_lock` を扱うなら `s3:GetBucketObjectLockConfiguration`
  - `ensure: absent` の前に中身を確かめるため `s3:ListBucket`

### ansible のルール

観測（check mode）：

- 観測は、モジュールを `--check --diff` で流して行う。
- コアは宣言を Observe に渡さない。そこで Validate が宣言を ID ごとに覚え、Observe がそれを使う（設計 §3.3 の DesiredObserver をこの形にした）。
- changed なら、観測の `module` を「<FQCN>（check mode で changed）」にして Update を出す。差分は `core.Classifier` の理由に出す。
- 到達できない・失敗・check mode を持たないモジュール・グループ名の host は Unknown。

Apply：

- 同じ呼び出しを通常のモードで流す。
- `destructive: true` の変更は、破壊的な変更として budget に数える（`core.Classifier`）。

書けないもの：

- ansible には「無い」が無いので、`ensure: absent`・Range は読み込み時にエラーにする。List・prune・Delete も持たない。消すときはモジュールの引数（`state: absent`）で宣言する。
- args の secret の参照は読み込み時にエラーにする。check mode の観測では値を取り出せないため。secret は inventory の変数か ansible-vault に置く。

そのほか：

- 接続設定は settings の `inventory`・`ansible`（絶対パス）・`env`・`timeout`・`max_parallel`。
- playbook を Procedure の 1 step として流す（N-T3）のはこの provider ではなく、Procedure の `exec` の step。

### template-file のルール

- 観測は「宣言で描いた中身とファイルが同じか」だけを答える。Validate が宣言を ID ごとに覚え、Observe がそれで描く。
  - ファイルが無ければ Absent。
  - 違えば、`template` の field に両方の sha256 を出して Update にする。中身は出さない。
- テンプレートの値は文字列だけ（`port: "22"` のように引用する）。
- secret の参照・秘密鍵は読み込み時にエラーにする。
- テンプレートが無いのは、読み込み時には警告するだけ。
- テンプレートが無い・読めない・描けない、書き出し先が読めない、は観測で Unknown。
- 書き出したファイルを ISO や HTTP の置き場所に入れるのは、利用者の側の仕事。

### 統合テスト（S3 互換のサーバ、Stage 8f）

```console
$ make integration-v3-s3
```

- S3 互換のサーバのイメージ（tag と digest で固定）を、docker で 127.0.0.1 の空いたポートに立てる。
  - SeaweedFS 4.51（必須）
  - MinIO RELEASE.2026-08-04（任意）
  - PUT の API を持たない SeaweedFS 3.80（任意）
- コンテナは `sakutei-v3s3-*`。
- `SAKUTEI_IT_SEAWEEDFS_IMAGE`・`SAKUTEI_IT_MINIO_IMAGE`・`SAKUTEI_IT_SEAWEEDFS_OLD_IMAGE` で差し替えられる。任意のイメージが取得できなければ、その step を飛ばす。
- v3 の CLI（`cli.Run` の plan・reconcile・approve、`s3.Module`）を向けて回す（`s3/integration/`、build tag `integration_v3_s3`）。
- 要るもの：docker・sudo（パスワードなし）。
- 終われば（落ちても）コンテナを消す。

step：

1. 空から作る：versioning・policy・lifecycle・tags を 1 回の reconcile で作り、次の plan は何も変えない（MinIO がポリシーを `{"AWS":["*"]}` の形で持っても drift にならない）。
2. 手で変えた policy と versioning は、2 field の update の drift になり、reconcile が戻す。
3. 何 cycle 回しても宣言どおりで、ポリシーを書き換えない。
4. `prune: true` の Range でも、宣言に無いバケット（中身のあるもの・空のもの）は消さずに unmanaged に出す。接頭辞の外のバケットは出ない。
5. `ensure: absent`：空のバケットは消え、中身のあるバケットは refused（オブジェクトがある）で残る。手で空にすると、次の cycle で消える。
6. サーバを止める（docker pause）と、その宣言は unknown・Range は不完全・終了コード 3 で、何もしない。動いているサーバは宣言どおり。戻せば収束する。
7. API を持たない endpoint（SeaweedFS 3.80）：バケットは作る。versioning の PUT は NotImplemented と分かるエラーで止まる。宣言していない設定は比べない。
8. endpoint の `fields`（policy・tags だけ）：ほかの設定は読まず、`versioning` の宣言は読み込み時にエラーにする（終了コード 2）。
9. 観測用と書き込み用のユーザー（MinIO の `mc admin`）：
   - 書き込み用の secret key のファイルが無くても plan は回り、reconcile は書かずにエラーになる。
   - 読み取り専用のユーザーの書き込みは 403。書き込み用のユーザーで収束する。
   - 観測の許可が 1 つでも欠ければ Unknown（「無い」とは読まない）。
10. Object Lock：
    - GOVERNANCE 1 日・COMPLIANCE 1 年で作って観測する。
    - 保持を 1 日から 2 日に延ばす変更と、既定の保持を外す変更は、承認が揃うまで Apply しない。`sakutei approve` のあとに Apply する。
    - Object Lock の無い既存のバケットへの `enable_on_create` は、承認を求めず、作り直さないエラーで止まる。
    - MinIO の観測用のユーザーは `s3:GetBucketObjectLockConfiguration` で読め、Put は 403。

2026-11-03 に docker 29.1.3・SeaweedFS 4.51・3.80・MinIO RELEASE.2026-08-04 で 3 テスト 17 step とも通した（約 42 秒）。

### 統合テスト（ansible-core、Stage 8f）

```console
$ make integration-v3-ansible
```

- バージョンを固定した venv（ansible-core 2.21.5、`~/.cache/sakutei/ansible-it-core-2.21.5`）の ansible を使う。無ければ作る（入れるものは `ansible/integration/requirements.txt`）。
- local 接続で一時ディレクトリを相手に、v3 の CLI（`cli.Run` の plan・reconcile、`ansible.Module`）から回す（`ansible/integration/`、build tag `integration_v3_ansible`）。
- 到達できないホストは、127.0.0.1 の閉じたポートへの ssh で作る。
- ansible は `SAKUTEI_IT_ANSIBLE`（絶対パス）で渡せる。別のバージョンなら `SAKUTEI_IT_ANSIBLE_ANY=1` も付ける。
- 要るもの：python3。初回は PyPI に届くこと。

step：

1. 空から：plan は module の Update と check mode の差分（provider の理由）を出して、何も変えない。reconcile が Apply し、読み直すと宣言どおり（`{{ … }}` はテンプレートとして評価しない）。
2. 手でファイルを変えると、変えた呼び出しだけが drift（差分つき）になり、reconcile が戻す。
3. reconcile を 5 回続けても変更の操作が無く、ファイルを書き換えない。
4. 到達できないホストと、check mode を持たないモジュール（ansible.builtin.command）は Unknown（理由つき、終了コード 3）。コマンドは流れない。
5. `destructive: true` の 2 つの呼び出しは budget の破壊的な変更に数える。`destructive_per_cycle: 1` を超える cycle は止まり（終了コード 4）、何も Apply しない。budget を 2 に上げると全部 Apply される。
6. グループ名の host は、check mode の観測で 2 台に一致して Unknown になり、Apply しない。args の secret の参照・`ensure: absent`・Range は読み込み時にエラーにする（終了コード 2）。
7. 消すのはモジュールの引数：`state: absent` の lineinfile が行を消し、次の plan は宣言どおり。

2026-11-03 に Linux 7.0・ansible-core 2.21.5 で 7 step とも通した（約 37 秒）。

## Expand と secret backend

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

各 Expand の golden テスト：

- 置き場所は `expanders/*/testdata/<ケース>/`（`intent/`・任意の `inputs.json`・`golden.json`）。
- コアの `expand/expandtest.Golden` で 2 回呼び、同じ出力になること（純粋さ）と golden との一致を確かめる（N-P5）。
- 意図した変更なら、`go test ./expanders/... -update` で golden を書き直す。

## sink と承認ツール

```yaml
# config.yaml
sinks:
  am:
    module: alertmanager
    settings:
      urls: ["https://am1.example.com:9093", "https://am2.example.com:9093"]
      tls: {ca: /etc/sakutei/am-ca.pem}
      token_file: /etc/sakutei/am.token     # 省略可（Authorization: Bearer）
      ttl: 10m                              # firing の endsAt（cycle の間隔より長く）
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

### sink のルール

- どの sink も、到達できない・応答が無いときはエラーを返すだけ。コアが位置を保ち、次の cycle に送り直す（cycle は止めない、不変条件 28）。
- 平文の http は、loopback の宛先か `plaintext: true` のときだけ（alertmanager・otlp）。URL に credential は書けない。
- Alertmanager：
  - labels は `alertname`（Event の kind）・`resource` か `scope`・`stuck`（stuck に上がったときだけ）・宣言の labels（Prometheus の名前に直す）・設定の `labels`。
  - annotations は `message`・`since`・`count`。
  - Sakutei が止まれば送り直しも止まり、alert は `ttl` で消える。
- `/metrics` の値：`sakutei_cycles_total`・`sakutei_last_cycle_timestamp_seconds`・`sakutei_cycle_duration_seconds`・`sakutei_resources{state}`・`sakutei_declared_resources`・`sakutei_cycle_applied`・`sakutei_cycle_failed`・`sakutei_halts`・`sakutei_budget_remaining{policy}`・`sakutei_budget_window_remaining{policy}`・`sakutei_firing{kind}`・`sakutei_firing_stuck{kind}`・`sakutei_events_total{kind}`。

### 承認ツール sakutei-approve

```console
$ sakutei-approve --record /var/lib/sakutei --listen 127.0.0.1:8444 --user-header X-Remote-User   # 認証をする proxy の後ろ
$ sakutei-approve --record /var/lib/sakutei --listen 0.0.0.0:8444 \
    --tls-cert tls.crt --tls-key tls.key --tls-client-ca approvers-ca.pem                       # 証明書の CN が承認者
$ curl -X POST https://approve.example.com:8444/approvals/chg-3f…/approve -d '{"comment": "確認済み"}'
```

HTTP API：

| メソッドとパス | 中身 |
|---|---|
| `GET /approvals` | 待っている依頼の一覧。`?all=1` で全部 |
| `GET /approvals/{id}` | 1 件の依頼 |
| `POST /approvals/{id}/approve` | 承認する |
| `POST /approvals/{id}/reject` | 拒否する（`comment` 必須） |

拒否するときのステータス：401（承認者が分からない）・403（申請者・要件に無い人）・404（依頼が無い）・409（待っていない）・400（書き方の誤り）。

- `--user-header` は、loopback の listen か `--tls-client-ca`（proxy の証明書を確かめる）と一緒にしか使えない。誰でもヘッダで承認者を名乗れる状態にしないため。
- loopback でない宛先を TLS なしで listen するときは、`--plaintext` を明示する。
- 承認を数えて反映し、audit に残すのは、ツールでなくコアの cycle。

## 移行と import のツール

### sakutei-migrate（Stage 9）

v2 の intent と provider の設定を、v3 の intent・接続設定・ID の対応表（`migrate-ids.json`）に書き直す。

| v2 | v3 |
|---|---|
| provider の設定にある範囲 | intent の `ranges` |
| 起動の引数の budget | Policy |
| planned_change | allow の Policy（allow・windows・approval・applicant） |
| remediation の時間帯 | window の Policy |
| Workflow と単純 Task | Procedure |
| 採番の記録 | `allocations/` |

- 書き直せないものがあれば、全部を挙げてエラーにする。
- v2 の試用の intent 全部（planned_change と remediation-profile を含む 32 件。`migrate/testdata/trial/` に置いてある）で、v3 のコアの plan が v2 の plan と同じ操作を出すことを確かめる（`migrate/testdata/trial-cases.json`）。
- 書き方の決まりは [../README.md](../README.md#stage-9-の決まりv2-の-intent-の書き直しと-import同期のツールどちらもコアの外の-providers) の「Stage 9 の決まり」。

### sakutei-import

今の現実を、宣言の形に書き出す。

- intent の `ranges` ごとに List と Observe をする。Apply はしない。
- 欠けた現実（観測できなかったもの）は書かない。
- `--every` で繰り返す同期は、変わったときだけ書き換える。手で書き換えられたファイルは上書きせずに止める。
- `--commit` で、書き換えるたびに 1 commit する。
- ほかの引数：`--provider`（その provider の Range だけを読む。繰り返せる）・`--undeclared`（宣言に無いものだけを書く）。
- 観測と宣言の形が違う provider は、任意の interface `importer.Declarer` を実装する。
