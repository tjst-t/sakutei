# 新しいコアの provider（v3/providers）

設計 `docs/core/README.md` の §7.1・§8 の段 6・段 7・段 8a・段 8b・段 8c・段 8d。provider・Expand のモジュール・秘密の保管先・Events の送り先・承認ツールはコアの外のモジュールで、コア（`github.com/tjst-t/sakutei/v3`）のインターフェースだけを使う。この Go モジュールは `github.com/tjst-t/sakutei/v3/providers`（コアを `replace ../` で使う。標準ライブラリだけ）。

| パッケージ | 中身 |
|---|---|
| `dnszone` | DNS のゾーンファイルのレコード（種類 `record`、ID `<FQDN>/<型>`、項目 `value`・`values`・`ttl`）。ゾーンは Range の `args.zone`。書き換えた Apply・Delete のときだけ SOA のシリアルを上げる（E-B1・N-B11・E-I7） |
| `hostconf` | ホストのファイル（種類 `file`、ID `<host>:<絶対パス>`、項目 `content`・`mode`・`owner`・`group`）と systemd のユニット（種類 `unit`、ID `<host>:<ユニット名>`、項目 `content`・`enabled`・`active`・`restart_on`）。中身は sha256 だけを観測する（E-D5・E-E2・N-D7・N-E1・N-E6） |
| `nftables` | nftables の chain（種類 `chain`、ID `<host>/<family>/<table>/<chain>`、項目 `rules`・`type`・`hook`・`priority`・`policy`）。ホストごとに file（ruleset のファイル）か live（カーネル。root で走るのは決まった道具だけ）。扱う table は Range の `args.host`・`args.tables`。live の 2 段の Apply は `core.Transactor`（E-B2・E-B3・E-B4・N-B9） |
| `nftguard`・`cmd/sakutei-nft-guard` | 決まった道具 sakutei-nft-guard（v2 から移し、すぐ戻す `rollback` を足した）。`list`・`check`・`apply`・`arm`・`disarm`・`rollback`・`status` だけ。扱う表は root だけが書ける道具の設定ファイルに書く（E-E1・E-B3） |
| `redfish` | 物理サーバの BMC（Redfish）。種類 `system`（ID `<node>`、項目 `power`・`boot_enabled`・`boot_target`。Off へは GracefulShutdown だけ。切り替えの途中は Converging）・`virtual-media`（ID `<node>/cd`、項目 `image`）・`bios`（ID `<node>/<属性>`、項目 `value`。Settings に PATCH し、保留中は Converging。登録簿で確かめてから送る）・`firmware`（ID `<node>/<部品>`、観測だけ。`version` と最新の更新の Task の `update_state`・`update_image`・`update_percent`・`update_message`）。一回きりの操作（`core.Invoker`）は system の `boot_once`・`reset`・`force_restart`（壊す）・`force_off`（壊す）と firmware の `verify`・`stage`（壊す）・`reset`・`force_restart`。観測用と書き込み用の資格を分け、書き込み用は Apply・Invoke の中でだけ読む。List を持たず Delete は断る（E-A1〜E-A5・N-A1〜N-A4・E-G5） |
| `metricsprobe` | /metrics（Prometheus の text 形式）を観測するだけ（module 名 `metrics-probe`）。種類 `series`（ID `<endpoint>/<メトリクス>[{ラベル="値",…}]`、項目 `series`・`value`（1 本のときだけ）・`sum`・`max`・`min`）。比べ方は Condition に書く。届かない・形が壊れている・一致する系列が無い・NaN は Unknown（E-H4・N-T4） |
| `netconf` | NETCONF の機器の設定の部分木（種類 `subtree`、ID `<host>/<部分木>`、項目 `config`（宣言する XML）・`capabilities`（観測だけ））。機器と部分木（subtree filter・順を比べる道）は Range の `args.host`・`args.filter.<部分木>`・`args.ordered.<部分木>`。比べ方は名前空間・空白・兄弟の順・宣言に無い属性を問わない正規形。既定は観測だけで、`settings.apply: true` のとき `:confirmed-commit:1.0` の 2 段で当てる（`core.Transactor`：Prepare＝lock・edit-config・validate・commit confirmed、Commit＝確定の commit、Rollback＝discard・cancel-commit かセッションの終わり。確定しなければ機器の confirm-timeout で戻る）。NACM で部分木だけに絞ったユーザーで動く。commit_group は `apply.transaction.group`、確かめる先は `confirm` の `observe(…)`・`present(…)`・`check(…)`（E-B7・E-B8・E-B9・E-B10・N-B1・N-B4） |
| `clidevice` | CLI でしか設定できない装置（種類＝機種の定義の項目、ID＝装置。項目のフィールドは文字列か秘密の参照）。機種の定義（観測のコマンドと読み方・変えるコマンドの雛形・確定と取り消し・`commit_confirmed`）は settings の `definition`（ファイルかマップ）。扱う装置と項目は Range の `args.host`・`args.items`。出力が定義に合わなければ Unknown。定義の `actions` は `core.Invoker`、`config.commit_confirmed`・`confirm`・`cancel` があれば `core.Transactor`（E-B11・N-B1） |
| `guestos` | ゲスト OS を ssh 越しに観測するだけ（module 名 `guest-os`）。種類 `os`（ID はホストの名前、項目 `os_id`・`os_version_id`・`os_pretty_name`・`hostname`・`boot_id`・`markers`・`marker`）。目印のパスは Resource の項目 `markers`（名前 → 絶対パス）に書き、中身は観測の `marker`（名前 → 値、4 KiB まで）で `observe(guest/os/node1).marker.installed` と読む。宣言できるのは `markers` と期待の値 `os_id`・`os_version_id`・`hostname` だけ。届かない・認証・ホスト鍵・壊れた出力・読めない目印は Unknown（E-E4・E-A8・E-K1） |
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

ホストへのつなぎ方（hostconf・nftables の live・guest-os）はコアの SDK `sdk/hostexec`（手元・ssh・sudo。ホスト鍵は known_hosts で確かめる）。netconf の ssh サブシステムと cli-device の対話（pty）・1 行の exec は、同じ `hosts:` の設定の上の `internal/sshstream`（接続を使い回さない・sudo を付けない・ssh の終了コード 255 は届かない）。

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

## BMC と /metrics の例

```yaml
# config.yaml（つなぎ方だけ）
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
  - name: bmc-fw                                   # v2 の多相 Task（redfish-firmware）を手順に書き直したもの
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

- 再起動・強制停止・1 回だけの起動先は宣言できない（観測に残らない一回きりの操作。読み込みで断り、手順の `invoke` を示す）。v2 の `power_off: graceful_then_force` は 2 つに分けた：宣言の `power: "Off"` は GracefulShutdown だけを送り（止まらなければ効かない Apply の回数で止まる、E-J6）、強制停止は手順の `invoke: {op: force_off}`（壊す操作。`wait: {for: 5m}` のあとに `skip_if` で止まっていなければ送る、など）。
- BIOS の属性の登録簿（読み取り専用・選べる値・範囲）は読み込みでなく Apply の前に BMC から読んで確かめる（読み込みは BMC に触れない）。合わなければ送らずに Apply が失敗する（設計 §7.1 の redfish-bios、E-A4・N-A3）。
- `stage` の `transfer: multipart` は Sakutei がイメージを取りに行って MultipartHttpPushUri に送る（`sha256` を書けば送る中身を確かめる）。`apply_time: OnReset` は再起動で効く。イメージを取りに行く資格は `image_username` と秘密の参照の `image_password`（`"<backend>:<在りか>"`）。
- 送れていない（BMC に届かない・電源の切り替えの途中・イメージを取りに行けない）ときだけ `ErrNotSent`（次の周に送り直す）。直さないと通らない理由（BMC が持たない起動先・更新できない部品・sha256 の不一致）と送ったあとの誤りは手順を止める。

## netconf と cli-device の例

```yaml
# config.yaml（つなぎ方だけ）
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
          write:                  # 省略可。Prepare・Commit・Rollback だけが使う書き込み用の資格（E-G5）
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
        group: core                           # commit_group：同じ名前の台は全部の確かめが通ったときだけ確定
        timeout: 60s                          # confirm-timeout（確かめはその半分まで）
        confirm:
          - {phase: after, when: 'check(mgmt-path)'}             # cli.WithCheck で組み込んだ確かめ
          - {phase: after, when: 'present(nc/subtree/sw2/acl)'}  # 別のリソースの観測
  - type: sw/syslog
    id: sw1
    fields: {host: 192.0.2.53, port: "514"}
  - type: sw/community
    id: sw1
    fields: {community: {secret: "vault:/net/snmp#community"}}
```

netconf の決まり：

- `filter.<部分木>` の最上位の要素は名前空間を書く（名前空間の無い subtree filter はすべての名前空間に当たる）。宣言の最上位の要素は filter が選ぶものだけで、filter の外を書く宣言・`nc:operation` などの当て方の属性・中身の無い要素・`capabilities` の宣言・知らない項目は読み込みで断る。
- 観測は get-config（running、部分木の filter）と close-session だけ。毎回新しいセッションを開いて閉じる（準備のあとの確かめも新しい接続で届くかを見る。コアは provider を閉じないので開いたままのセッションを残さない）。中身の無い要素だけの `<data>` は Absent、届かない・rpc-error・access-denied は Unknown（理由つき）。
- 観測の `config` は、宣言と意味が同じなら宣言の書き方のまま返し、違えば機器の正規形（宣言に書いた属性だけ）を返す。比べるのは宣言に書いた属性だけで、機器が付ける commit の時刻などで drift にしない。
- 当てるのはいつも commit confirmed の 2 段（`apply.transaction` を書かない宣言も、Apply の中で新しい接続で読み直して宣言どおりなら確定し、`timeout` の半分までに確かめられなければ戻す）。機器が `:candidate` と `:confirmed-commit` を持たなければ何も送らない。部分木を消さない（`prune: true` の範囲・`ensure: absent` は読み込みで断り、Delete は `RefuseError`）。List は範囲の部分木のうち機器に在るもの（宣言外の報告）。
- NACM で絞ったユーザー：送る rpc は get-config・lock・unlock・edit-config・validate・commit・cancel-commit・discard-changes・close-session だけで、部分木の外を読み書きしない。access-denied で断られたら何も確定せず、許すべきもの（部分木の読み書きと上の rpc の実行）を理由に書く。読むのを許されていない部分木は NACM が黙って省くので Absent に見える。

cli-device の決まり（機種の定義は `clidevice/testdata/example-device.yaml`、架空の装置は `clidevice/clifake`）：

- 種類は定義の項目、ID は装置（`sw/syslog` の `sw1`）。項目は全体で宣言する。値は定義の `pattern`（値の全体）に合わなければ読み込みで断る（コマンドへの差し込みを防ぐ。値は文言に出さない）。秘密の項目は `{secret: …}` でしか書けず、Apply の直前に `WriteContext.Secret` で取り出し、誤りの文・観測では `***` に伏せる。観測は「このプロセスが書いた値と同じなら当てた参照（世代つき）」だけを返す。
- 出力が定義に合わない（読み方が当たらない・2 つ当たる・エラーの印・プロンプトが来ない・exec の 0 以外の終了）・届かないは Unknown で、Apply は何も流さない。1 回の Observe で同じコマンドの出力は使い回す。
- Apply は流す前に観測し、宣言どおりなら流さない。エラーの印で止め、`config.rollback` があれば取り消して確定しない。流した後は観測し直し、宣言どおりでなければ誤り（コアが効かない回数で止める）。
- 2 段の Apply：定義の `config.commit_confirmed`（`{seconds}`・`{minutes}` に transaction.timeout が入る）・`confirm`（確定）・`cancel`（任意。すぐ戻す）を書いた機種だけ。Prepare は設定に入ってコマンド列と commit_confirmed を流し、Commit は新しいやりとりで confirm を流す。どれも無ければ装置の時間切れで戻る（不変条件 17）。書かない機種の宣言に `apply.transaction` を書くと Prepare で断る。
- 定義の `actions` は `core.Invoker`（`Ops` の `Destructive` は定義の `destructive`）。届かない・1 つも打てていないときだけ `ErrNotSent`。
- v2 の項目の `disruptive`（Apply を壊す変更に数える）は、コアの壊す変更の印（`Classifier`）が入るまで書けない（書けば読み込みで断る）。

## ゲスト OS の例

```yaml
# config.yaml（つなぎ方だけ。観測用のユーザーでよい）
providers:
  guest:
    module: guest-os
    settings:
      hosts:
        node1:
          ssh: {address: 192.0.2.21, user: monitor, key: /etc/sakutei/id_ed25519, known_hosts: /etc/sakutei/known_hosts}
```

```yaml
# intent（目印は Resource の項目）
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

- 目印のパスは観測のたびに読み直す（Validate が宣言の `markers` を ID ごとに覚え、Observe がそれを読む）。宣言の無い ID（Condition だけが参照する）は目印なしで観測する。
- 目印のファイルが無いのは観測の事実で、`marker` にその鍵が無いだけ（Condition では null）。読めない（権限・ディレクトリ・4 KiB より大きい）目印は Unknown。
- 無いパスを「無い」とするのは上の一番近いディレクトリを覗けるときだけ（覗けない観測用のユーザーは見分けられないので Unknown）。ホストには sh・dd・od だけが要る（busybox でもよい）。

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

## 統合テスト（qemu-bmc）

```console
$ make integration-v3-redfish
```

v2 と同じ qemu-bmc 0.11.0（tag と digest で固定）を containerlab で 2 台立て（`redfish/integration/topology.clab.yml`。lab `sakutei-v3rf`・管理ネット 172.31.93.0/24 で、v2 の `make integration-redfish` の lab `sakutei-rf84` と重ならない）、v3 の sakutei（`cmd/sakutei`）の CLI と常駐を本物の Redfish に向けて回し、終わったら（失敗しても）lab を消す。KVM・docker・containerlab と sudo（パスワードなし）が要るので、check・test・CI には入れない。段（`redfish/integration/redfish_test.go`）：

1. plan は BMC に書かず、reconcile で電源・起動先（Continuous の Pxe・Disabled）・仮想メディアに収束し、次の plan は変えない。
2. 手で変えた電源・起動先・仮想メディアを次の reconcile で戻す。
3. ACPI に応えない VM への既定の停止は GracefulShutdown だけで Off にならず、効かない Apply の回数で止まる（終了コード 4）。手順の `force_off` で止まる。
4. BMC を止める（docker pause）とそのノードは Unknown で何もせず（終了コード 3）、もう 1 台は収束する。
5. `boot_enabled: Once`・`power: ForceRestart` は読み込みで断る（終了コード 2）。
6. 手順の `boot_once`（Cd）と `force_restart`：1 回の起動で BMC が上書きを Disabled に戻す。
7. BIOS の属性：Settings に 1 度だけ PATCH し（再起動しない）、保留中の reconcile は Converging で重ねない。読み取り専用の属性は送らない。手順の `force_restart` で効き、宣言していない属性は変わらない。
8. FW の更新（BMC）：常駐は承認の前に何も送らず、イメージも取りに来ない。承認の後に verify・stage（critical）を 1 回ずつ送り、BMC の Task（Completed）を待って版で閉じる。
9. FW の更新（BIOS）：BMC の Task が終わっても版は再起動まで変わらず、手順の `force_restart` で効く。
10. FW の故障（qemu-bmc の障害注入）：イメージを断られる（400 VerificationFailed）と手順は止まって送り直さない。BMC の Task が Exception で終わると版は変わらず、待ちの時間切れで止まる。
11. 書き込み用のパスワードのファイルが無くても plan は観測できて差分を出し、reconcile は書かずに失敗する。

2026-10-31 に Linux 7.0・containerlab・qemu-bmc 0.11.0 で通した（lab の用意を除いて約 110 秒）。

## 統合テスト（netopeer2、v3 段 8c）

```console
$ make integration-v3-netconf
```

v2 の `make integration-netconf` と同じ netopeer2 のイメージ（`sysrepo/sysrepo-netopeer2`、tag と digest で固定。`SAKUTEI_IT_NETOPEER2_IMAGE` で差し替えられる）を docker で 4 台（`sakutei-v3nc-sw1`・`g1`〜`g3`。v2 の `sakutei-ncit-*` と重ならない）127.0.0.1 の空いたポートに立て、v3 の CLI（`cli.Run` の plan・reconcile、`netconf.Module` と `cli.WithCheck`）を本物の NETCONF over SSH に向けて回す（`netconf/integration/`、build tag `integration_v3_netconf`）。docker・ssh・ssh-keygen と sudo（パスワードなし）が要るので、check・test・CI には入れない。コンテナは終われば（落ちても）消す。

1. 宣言どおりなら差分なし（空白・group の利用者の順が機器と違っても）。
2. 観測だけ：別のセッションで手で変えるとずれ（drift、終了コード 3）、当てない。
3. commit confirmed の 2 段で当てる：1.0（`apply.transaction`）・1.1 の persist・`apply.transaction` を書かない宣言。確定したものは confirm-timeout を過ぎても戻らない。
4. NACM で 2 つの部分木と要る rpc だけを許した利用者：観測と 2 段の Apply が通り、その間に netopeer2 が断ったものが無い。部分木の外を書く宣言は access-denied（理由つき）で、機器は変わらない。
5. 当てると NACM を読めなくなる宣言（読みの締め出し）：確かめが通らず確定しない（終了コード 4・rolled_back）、機器が元に戻り、次の周も同じ宣言を当て直さない。
6. Prepare のあと Commit も Rollback もしなければ、機器の confirm-timeout で元に戻る（不変条件 17）。
7. confirm の `present(nc/subtree/g2/marker)`：別の台に印が無ければ確定せず、印を置けば確定する。
8. commit_group（g1〜g3）と `check(path)`：全部の台で通れば 3 台とも確定し、通らなければどの台も確定せず戻る。
9. 機器を止めると Unknown（0 以外で終わる）。

2026-10-31 に docker 29.1.3 で通した（9 段・約 85 秒）。この netopeer2 のイメージは、コアの周で commit_group を戻したあと（8 の後半）同じ機器に次の commit confirmed を送ると netopeer2-server が SIGSEGV で落ちた（同じ rpc の並びを provider に直に送ると落ちず、手元の側では原因を絞れなかった）。v2 の統合テストも group を最後に回しているので、8 を g1〜g3 で最後に回している。
