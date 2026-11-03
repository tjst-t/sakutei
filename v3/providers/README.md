# 新しいコアの provider（v3/providers）

設計 `docs/core/README.md` の §7.1・§8 の段 6・段 7・段 8a・段 8b・段 8c・段 8d・段 8e・段 8f。provider・Expand のモジュール・秘密の保管先・Events の送り先・承認ツールはコアの外のモジュールで、コア（`github.com/tjst-t/sakutei/v3`）のインターフェースだけを使う。この Go モジュールは `github.com/tjst-t/sakutei/v3/providers`（コアを `replace ../` で使う。標準ライブラリだけ）。

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
| `proxmox` | Proxmox VE の VM と VM の FW の規則（REST API、API トークン）。種類 `vm`（ID `<クラスタ>/<VM 名>`、項目 `vmid`・`node`・`template`（作るときだけ）・`pool`・`cores`・`memory`・`disks`（宣言した slot だけ。大きくするだけ。`keep`・`volume=`）・`nics`（宣言した slot・キーだけ。`firewall`・`mtu`）・`firewall`（VM の FW の options、書いたキーだけ）・`ci_user`・`ci_ssh_keys`・`ci_ip`・`ci_gateway`・`ci_dns`・`ciupgrade`・`power`・`apply_pending`。観測だけ `lock`・`pending`・`current_values`）・`fw-rules`（ID `<クラスタ>/<VM 名>`、項目 `rules`：Sakutei の印のある規則の並び）・観測だけの `volume`（付け先の無い volume、`<クラスタ>/<VM 名>/unusedN`）と `fw-rule`（印の無い規則、`<クラスタ>/<VM 名>/<位置>`）。比べ方は `FieldSpec.Match`、volume の付け替え（承認の要る壊す変更）と規則の変更（壊す変更）は `core.Classifier`。keep のディスクが残る VM の削除は断る。一回きりの操作は `reboot`・`reset`（壊す）・`stop`（壊す）。範囲は `args.cluster`。network_policy の実現先 `proxmox-fw`（`proxmox.Realizer`）。偽の Proxmox は `proxmox/pvetest`、規則の文は `proxmox/fwrule`（E-D1・E-C2・E-B6・N-D1・N-D10・N-C12・E-G5） |
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
| `postgresql` | PostgreSQL のロール（種類 `role`、ID `<server>/<ロール>`、項目 `login`・`superuser`・`createdb`・`createrole`・`inherit`・`bypassrls`・`replication`・`connection_limit`・`member_of`・`password`（秘密の参照））・データベース（種類 `database`、ID `<server>/<DB>`、項目 `owner`・`connection_limit`・`encoding`・`lc_collate`・`lc_ctype`・`template`・`connect_roles`。中身を失う種類なので宣言の外でも prune しない）・権限（種類 `grant`、ID `<server>/<DB>/<ロール か PUBLIC>`、項目 `privileges`）・拡張（種類 `extension`、ID `<server>/<DB>/<拡張>`、項目 `version`）。psql を exec し、パスワードは SCRAM の検証値で送る。管理の範囲は Range の `args.server`。観測用と書き込み用の資格を分け、書き込み用は Apply・Delete の中でだけ読む。利用者の表がある DB の削除は `RefuseError`（E-F1・E-F3・N-F1・N-F7・E-G5） |
| `sqlmigrate` | 版つきのマイグレーション（種類 `database`、ID は settings の `databases` の名前。観測だけ：`version`（schema_migrations の最大の版）・`latest`・`pending`）。SQLite（sqlite3 の CLI）と PostgreSQL（psql、advisory lock）。版は手順の `invoke` の `migrate`（上げ下げ・`rollback_on_fail`）と `rollback`（下げるだけ）でだけ変える（どちらも壊す操作）。v2 の `strategy: task` は `trigger: on_drift`・`invoke: migrate`・`done_when` に書き直す。観測用と書き込み用の資格を分け、書き込み用（`password_file` か秘密の参照 `password_secret`）は Invoke の中でだけ読む。List を持たず Delete は断る（E-F2・N-F2・E-G5） |
| `ldap` | LDAP のディレクトリの項目（種類 `entry`、ID `<サーバ>/<DN の正規形>`、項目 `attributes`（属性の名前 → 値。値の集合で比べる）・`user_password`（秘密の参照。書き込みだけ）・`user_password_set`（観測だけ））。OpenLDAP の ldapsearch・ldapmodify を exec で呼ぶ。管理の範囲は Range の `args.server`・`args.base_dn`・`args.filter`・`args.scope`（sub・one）。LDAPS と StartTLS。観測用と書き込み用の bind を分ける。子のある項目・宣言した項目の上の項目の削除は `RefuseError`（E-G1・N-G1・E-G5） |
| `s3` | S3 互換のバケット（SigV4 は標準ライブラリ）。種類 `bucket`（ID `<endpoint>/<バケット>`、項目 `versioning`・`policy`（マップ。IAM の書き方の違いをそろえた正規形で比べる）・`lifecycle`（ルールの一覧。値を文字列に・順を問わない正規形）・`tags`（マップ）・`object_lock`（`enable_on_create`・`mode`・`days` か `years`・`enabled`。期間は Days にそろえて比べる）、観測だけ `creation_date`・`unsupported`）。endpoint の `fields` に無い設定は読まずに宣言を断り（`object_lock` は書いたときだけ）、`storage_class_aliases`・`api_base_path`。既にあるバケットの Object Lock の保持の変更は向きによらず承認の要る壊す変更（`core.Classifier`）。バケットは宣言外で消さず（DestructiveDelete）、中身のあるバケットの `ensure: absent` は断る。観測用と書き込み用の資格を分け、書き込み用は Apply・Delete の中でだけ読む。範囲は `args.endpoint`・`args.prefix`（接頭辞の重なりは断る）。偽の S3 は `s3/s3test`（E-C1・N-C7・E-G5・E-K1・E-K2・E-C3） |
| `ansible` | 専用の provider の無い雑多な設定を Ansible のモジュール 1 つで覆う最小階級（module 名 `ansible`）。種類 `call`（ID は利用者が付ける名前、項目 `host`（inventory のホスト 1 台）・`module`（FQCN）・`args`（モジュールの引数、Map。値はテンプレートとして評価しない）・`destructive`（既定 false））。観測はモジュールを `--check --diff` で流す：宣言を Validate が ID ごとに覚えて Observe が使い（コアは宣言を Observe に渡さないので、設計 §3.3 の DesiredObserver をこの形にした）、changed なら観測の `module` を「<FQCN>（check mode で changed）」にして Update として出し、差分は `core.Classifier` の理由に出す。適用は同じ呼び出しを通常の実行で。`destructive: true` の変更を壊す変更として予算に数える（`core.Classifier`）。届かない・失敗・check mode を持たないモジュール・グループ名の host は Unknown。List・prune・Delete を持たない（消すのは引数の `state: absent`）。args の秘密の参照は断る（check mode の観測では値を取り出せないので、秘密は inventory の変数・ansible-vault に置く）。つなぎ方は settings の `inventory`・`ansible`（絶対パス）・`env`・`timeout`・`max_parallel`（E-E3・N-E5・E-K1） |
| `templatefile` | 雛形（text/template）に台ごとの値を入れたファイルを書き出す（module 名 `template-file`）。種類 `file`（ID は `output_dir` からの相対パス、項目 `template`（`template_dir` からの相対パス、必須）・`hostname`・`address`・`ssh_authorized_keys`・`install_mark`・`vars`（名前 → 文字列））。雛形は `.ID`・`.Hostname`・`.Address`・`.SSHAuthorizedKeys`・`.InstallMark`・`.Vars.<名前>` と関数 `shquote`・`json`・`lines`（missingkey=error）。Validate が宣言を ID ごとに覚え、観測は宣言で描いた中身とファイルを比べる（無ければ Absent、違えば `template` の項目に両方の sha256 を出して Update。中身は出さない）。秘密の参照・秘密鍵は読み込みで断る。雛形が無いのは知らせるだけで、雛形が無い・読めない・描けない・書き出す先が読めないは Unknown。List を持たず Delete は断る（E-E5・N-E8・E-K1） |
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

## Proxmox VE の例

```yaml
# config.yaml（つなぎ方だけ。観測用と書き込み用のトークンを分ける）
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
    args: {cluster: lab}                    # List と宣言外の報告の範囲
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

- 宣言した項目だけを比べる。`disks`・`nics` は宣言した slot だけを、`nics` の `mac`・`model`・`firewall`・`mtu` と `firewall` の options は書いたキーだけを比べる。`memory` の `4G` と `4096`、DNS の区切りなどの書き方の違いはずれにしない（provider の正規形、`FieldSpec.Match`）。`template` は作るときだけ使う。
- ディスクは大きくするだけ（小さくする・ストレージを変えるのは Apply の誤り）。`vmid`・`node` を今の VM と違えるのも Apply の誤り（番号を変えない・移行は扱わない）。
- 走っている VM の hotplug できない変更（`cores`・`memory` など）は Proxmox が pending に置く。宣言の値が pending にあればずれにせず重ねて書かない（観測の `pending` に名前が出る）。`apply_pending: reboot` なら 1 回再起動して効かせる。
- 非同期の仕事（作成・複製・resize・電源・削除）は Apply が `task_wait` まで待つ。待ち切れなければ誤りにせず戻り、その VM の仕事が走る間（`/nodes/<node>/tasks?source=active`）と lock がある間の観測は Converging（provider は UPID を覚えない）。
- 止めるのは正常停止（`power: stopped`）。強制の停止・再起動は手順の `invoke`（`stop`・`reset` は壊す操作、`reboot`）。
- `volume=` の付け替え（持ち主の VM から Proxmox の reassign で外して付ける）は承認の要る壊す変更：当たる Policy に approval が無ければ申請者でない 1 人の承認を頼み、`sakutei approve` で揃うまで当てない。持ち主の VM が走っていれば何も変えずに待つ（`power: stopped` を宣言する）。付いたあとは volume の名前を比べない。
- keep の印（VM の tags の `sakutei-keep-<slot>`）のあるディスクか unused が残る VM の `ensure: absent` は、何も書かずに断る（`DeleteRefused`）。付け先の無い volume（`unusedN`）と印の無い FW の規則は、観測だけの種類 `volume`・`fw-rule` として宣言外に出る（消さない）。
- 触らない VM は Range の `external_owners`（`id: vm/<クラスタ>/<VM 名>`）。宣言すれば読み込みで断る。
- 観測用のトークンに要るのは `/` の `PVEAuditor`（`/cluster/resources`・`/status/current`・`/pending`・`/firewall/*`・他人の仕事の一覧）。書き込み用のトークンは Apply・Delete・Invoke の中でだけ読む。

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

## postgresql・sqlmigrate・ldap の例

```yaml
# config.yaml（つなぎ方だけ）
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
  - {provider: pg, args: {server: main}, prune: true}    # ロール・権限・拡張の宣言外を prune（DB は消さない）
  - provider: dir
    args: {server: dir1, base_dn: "dc=example,dc=com", filter: "(!(ou=outside))", scope: sub}
    prune: true                                          # 宣言外を消す（子のある項目は消さない）
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
  - {type: pg/database, id: main/olddb, ensure: absent} # 利用者の表があれば断る（RefuseError）
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
  - name: migrate-orders                           # v2 の strategy: task を手順に書き直したもの
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

postgresql の決まり：

- psql はシェルを通さず、SQL は標準入力で渡し、接続の設定は子の環境変数（親の `PG*` は引き継がない）。名前はいつも quote する。パスワードのファイルは呼ぶたびに読み、0600 の一時的な passfile で渡して消す。
- ロールのパスワードは Apply の直前に `WriteContext.Secret` で取り出し、SCRAM-SHA-256 の検証値にして送る（平文はサーバのログに出ない）。ロールのコメントに参照（世代つき）と検証値の sha256 を書き、観測はその参照を返すので、秘密の世代が変わったときだけ当て直す。`verify_passwords` なら pg_authid の検証値の sha256 が合わなければずれ、pg_authid を読めなければそのロールは Unknown。v2 が書いたコメントの形とは違うので、v2 でパスワードを設定したロールは 1 回設定し直す。
- 観測は、宣言と意味が同じなら宣言の書き方を返す（encoding の `UTF-8`・ロケールの codeset・権限の `TEMP`・小文字。`connect_roles` は宣言が観測に含まれていれば同じ）。`template` は観測できず、作った後は比べない。`encoding`・`lc_collate`・`lc_ctype` を作った後に変える宣言は Apply が断る（作り直さない）。
- PUBLIC の権限は DB が在れば在り、宣言どおりにする（空の集合で全部外す）。List に出さず（宣言しなければ触らない）、`ensure: absent` は書けない。ロールの権限は集合ちょうどで、空にはできない。
- `connect_roles` は管理の境界の外のロール（つなぐロール・`pg_*`）にだけ書け、DB を作る Apply の中で CONNECT を与える。足すだけで外さない。
- 管理の境界：`pg_*`・つなぐロール・`template0`・`template1`・`postgres`・管理用の DB・所有者の権限・`plpgsql` は List に出さず、宣言すれば読み込みで断る（`plpgsql` の宣言はできる）。List は snapshot と全部の DB の拡張が読めたときだけ完全。
- データベースは中身を失う種類なので、宣言の外でも prune しない（不変条件 9。v2 は空の宣言外の DB を消したが、v3 では `ensure: absent` で名指しする）。
- 届かない・認証の失敗・psql が無い・パスワードのファイルを読めない・壊れた出力は Unknown。Delete は利用者の表がある DB・接続を許さない DB、DB を持つか権限を持つロール、PostgreSQL が 2BP01 で断るロール・拡張を `RefuseError` で断る（CASCADE・DROP OWNED は使わない）。

sqlmigrate の決まり：

- 種類 `database` は観測だけ。宣言の `version` は収束ループが当てず、手順が `migrate` で合わせる。ディレクトリに無い版・観測だけの項目・`ensure: absent`・範囲は読み込みで断る。
- 1 本は 1 つのトランザクション（BEGIN … SQL … schema_migrations の完了印 … COMMIT）。失敗した 1 本だけが巻き戻り、前に通した版は残る。`rollback_on_fail: true` なら呼んだときの版まで down で戻してから止める（手順の `on_failure` の戻しの段は後の段。いまは halt だけ）。トランザクションに入れられない文（CREATE INDEX CONCURRENTLY）は 25001 の失敗として止める。
- 流す前に要る SQL を全部読み、sqlite3 のドットコマンド・psql のメタコマンド・足りない down があれば何も流さない。ファイルの中で COMMIT・ROLLBACK を書くと「確定したか分からない」誤りで止める。
- 2 つの処理が同じ DB を動かしても同じ版を二度流さない：1 本ごとに鍵（SQLite は BEGIN IMMEDIATE、PostgreSQL は v2 と同じ鍵の `pg_advisory_xact_lock`）を `lock_timeout` まで待って取り、版が流す前のままかを確かめる。違えば何も流さず読み直して続ける。
- 送れていない（`ErrNotSent`、次の周に送り直す）のは、まだ 1 本も確定していないときの「つながらない・認証の失敗・sqlite3／psql が無い・パスワードを読めない・鍵を待ちきれない」だけ。SQL の誤り・確定したあとの誤り・接続が途中で切れたときは手順を止める。
- 読めない（届かない・壊れている・データベースが無い・鍵を待ちきれない）は Unknown。SQLite のファイルが無いのは版 0（置き場所があり sqlite3 が起動できるときだけ）。

ldap の決まり：

- 比べるのは宣言した属性だけで、宣言に無い属性は触らない。属性の名前は大文字小文字を問わない。値の集合をその一覧ちょうどにし、`""` と `[]` は「その属性が無い」。DN を値に持つ属性（member・uniqueMember・owner・seeAlso・manager など）は DN の正規形で比べる。観測の `attributes` は宣言した属性だけで、意味が同じなら宣言の書き方のまま、違えばサーバの値（並べた一覧か 1 つの文字列）を返す。
- Range の `scope` は LDAP の検索の scope（`sub`・`one`）。v2 の scope（ID の `site=tokyo`）は v3 に無い。
- 読み込みで断るもの：正規形でない ID・どの範囲にも入らない宣言（base_dn そのもの・scope が one の孫）・bind の DN・同じサーバの重なる範囲・objectClass の無い宣言・大文字小文字だけ違う同じ属性・操作属性（modifyTimestamp など）・`attributes` の中の userPassword・文字列でない値・空の値・オプションつきの属性の名前（`cn;lang-ja`）・RDN の値を含まない属性・`user_password_set` の宣言・settings の base_dn・filter・scope。値は文に出さない。
- `user_password` は書き込みだけ：値は Apply の中で `WriteContext.Secret` で取り出し、ldapmodify の標準入力（base64 の LDIF）でだけ渡す。サーバに userPassword が在れば適合で、世代を上げても書き直さない（替えるときはサーバの userPassword を消せば次の周に今の値を書く）。観測用の DN にも userPassword の read を許す（読めないと無いと見えて毎周書く）。
- bind のパスワードは settings の `password_file` からその都度読み、0600 の一時ファイルで `-y` に渡して消す。引数・環境変数・誤りの文には出さない。観測と List は `observe` の資格だけ、Apply・Delete は `write` の資格だけ（書き込み用のファイルが無くても plan は観測できる）。親の LDAP で始まる環境変数は引き継がず、`LDAPTLS_REQCERT=demand`。
- 届かない・時間切れ・bind と TLS の失敗・コマンドが無い・パスワードのファイルを読めない・読めない出力・スキーマを読めないは Unknown（理由つき）。noSuchObject（32）だけが Absent。List は範囲のページに分けた検索で、最後まで読めなければ誤り（件数の上限で切れたときも）。base_dn そのもの・bind の DN・サブエントリ・tombstone は List に出さない。
- Apply は宣言と違う属性だけを 1 つの modify の replace で書き、宣言どおりなら何も書かない。無ければ add で作る（葉の RDN は宣言の綴り）。親が無ければ誤りなので親を `depends_on` に書く。Delete は子の項目がある項目と、宣言した項目の上の項目を消さない（`RefuseError`）。コアは同じ周に宣言外の親の prune を子の作成より先に流すことがあるので、子がまだ作られていなくても断る（統合テストで見つけた）。

## S3・Ansible・雛形のファイルの例

```yaml
# config.yaml（つなぎ方だけ）
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
  - {type: s3/bucket, id: minio/app-old, ensure: absent}   # 中身があれば断る
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

- s3：空のポリシー・ルール・タグは `{}`・`[]`・`{}` と書く（宣言すれば外す）。観測用の利用者に要る許可は `s3:ListAllMyBuckets`・`s3:GetBucketVersioning`・`s3:GetBucketPolicy`・`s3:GetLifecycleConfiguration`・`s3:GetBucketTagging`（`object_lock` を扱うなら `s3:GetBucketObjectLockConfiguration`、`ensure: absent` の前の中身の確かめに `s3:ListBucket`）。
- ansible には「無い」が無いので、`ensure: absent`・範囲は読み込みで断る。消すのはモジュールの引数（`state: absent`）で宣言する。playbook を手順の一段として流す（N-T3）のはこの provider でなく、手順の `exec` の段。
- template-file の雛形の値は文字列だけ（`port: "22"` のように引用する）。書き出したファイルを ISO や HTTP の置き場に入れるのは利用者の側。

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

## 統合テスト（入れ子の Proxmox VE）

```console
$ make integration-v3-proxmox
```

v2 の `make integration-proxmox` と同じ入れ子の Proxmox VE 9.2（ISO は版と sha256 で固定、自動導入の ISO を docker で作り qemu の入れ子の KVM で入れる。ISO・自動導入の ISO・導入済みのディスクは同じキャッシュ `SAKUTEI_IT_CACHE`、既定 `~/.cache/sakutei-it/proxmox` を使い回す）を、導入済みのディスクの上の使い捨ての overlay で立て、v3 の sakutei（`cmd/sakutei`）の CLI を向けて回し、終われば（落ちても）qemu を止める。KVM・qemu・docker・curl・ssh と sudo（パスワードなし）、初回は ISO とクラウドイメージを取るネットワークが要るので、check・test・CI には入れない。段（`proxmox/integration/proxmox_test.go`）：

1. テンプレートから作る：採番の vmid（500〜509）で Debian 13 のクラウドイメージのテンプレートから複製し、resize・VLAN の NIC・cloud-init・起動まで 1 回の reconcile で収束する。VM に cloud-init の利用者と鍵で ssh で入れ、ホスト名・アドレス・CPU の数・ディスクの大きさが宣言どおり。
2. 何周しても適合のまま：reconcile を続けても終了コード 0、plan に変える操作が無く、触らない VM（external_owners）とテンプレートは出ない。
3. 手でメモリを変える（走っている VM なので pending に置かれる）と plan が memory だけのずれを出し、reconcile が戻して pending が消える。
4. `cores` を変えると pending に置かれ、次の reconcile は重ねて書かない（終了コード 0）。`apply_pending: reboot` を足すと 1 回再起動して効き、VM の中の `nproc` が 3。
5. 触らない VM（`external_owners` の `vm/it/sakutei-ctl`）を宣言すると読み込みで断り（終了コード 2）、その VM の設定の digest は変わらない。
6. qemu を SIGSTOP で止めると plan はその VM を unknown にし、戻せば収束する。
7. 宣言を外すと VM は宣言外（unmanaged）に出て、reconcile しても消えない（触らない VM・テンプレートは出ない）。
8. `ciupgrade: false`・NIC の `firewall=1`・`mtu=1450`・VM の FW の options で作り（止めたまま）、手で NIC と options を変えると、観測用のトークン（`/` に `PVEAuditor`）の plan が nics と firewall のずれを出す。そのトークンだけの reconcile は書けずに失敗し、書き込み用のトークンを分けた設定の reconcile が戻す（書かないキー `ipfilter` は残す）。
9. network_policy の実現先 proxmox-fw：手の規則を置いた VM に、口と向き（net0 は出る向き・net1 は入る向き、IPv4 と IPv6）の許可を当てると、`/etc/pve/firewall/550.fw` に印と reason の comment つきの規則が宣言の順に先頭に入り、手の規則は後ろに残って種類 `fw-rule` の宣言外に出る。置いた規則の port を手で変え先頭に手の規則を置くと、観測用のトークンの plan でもずれになり、reconcile が戻して手の規則を消さない。
10. 作り直しでデータのディスクを引き継ぐ：keep の印（tags）のある前の VM の `ensure: absent` は断られて消えない。新しい VM の `volume=` は承認が揃うまで作らず（`sakutei approve`）、承認のあとに reassign で付け替えて中身（LV に書いた印）が残る。前の VM はそのあと消せ、keep でないディスクは一緒に消え、付け替えたディスクは残る。
11. pool に入れて作り、手で pool から外すと plan が pool のずれを出して reconcile が戻す。無い pool は `does not exist` の誤り。
12. 承認つきで消す：`approval` の Policy に当たる `ensure: absent` は承認の前に消さず、`sakutei approve` のあとに走っている VM を止めて消す。ほかの VM には触れない。

2026-10-31 に Linux 7.0・qemu の入れ子の KVM・Proxmox VE 9.2 で 12 段とも通した（キャッシュの導入済みのディスクを使い、lab の起動を含めて約 280 秒。作る VM は 1 回の reconcile で収束した）。

実機で見つけたこと（Proxmox VE 9.2）：作った直後の VM は `/cluster/resources` に仮の名前 `VM <vmid>` で載る（名前が空のときと同じく VM の設定で確かめる。偽の Proxmox の `ResourceLag` も真似る）。複製した VM のディスクの volume の番号は Proxmox が決める（テンプレートの cloud-init のドライブなどで飛ぶ）。

## 統合テスト（postgres、v3 段 8e）

```console
$ make integration-v3-postgresql
```

v2 の `make integration-postgresql` と同じ公式の postgres イメージ（`postgres:17.11-bookworm`、tag と digest で固定。`SAKUTEI_IT_POSTGRES_IMAGE` で差し替えられる）で、サーバと psql を動かすだけのクライアントを docker のネットワークに立て、v3 の CLI（`cli.Run` の plan・reconcile と常駐）を向ける。postgresql provider（`postgresql/integration/`）は `sakutei-v3pg-server`・`sakutei-v3pg-client`・`sakutei-v3pg-net`、sqlmigrate（`sqlmigrate/integration/`）は `sakutei-v3sm-*` を使う（v2 の `sakutei-pgit-*` と重ならない）。build tag は両方 `integration_v3_postgresql`。ホストに psql は要らない（ラッパーのスクリプトでクライアントのコンテナの中で動かす）。docker と sudo（パスワードなし）が要るので、check・test・CI には入れない。コンテナとネットワークは終われば（落ちても）消す。

postgresql：

1. 空から作る：ロール（引用符を含む名前も）・DB（template0・C ロケール）・権限・拡張・PUBLIC から全部外す。宣言のパスワードでつなげ、pg_authid は SCRAM の検証値。宣言していない DB の PUBLIC は既定のまま。
2. 手で変えるとずれ（パスワードも verify_passwords で）、1 周で戻る。
3. PUBLIC と BYPASSRLS を手で変えると戻り、宣言しない属性（REPLICATION）は触らない。
4. 秘密の新しい世代でパスワードを設定し直し（古いものではつなげない）、拡張の版を上げる。作った後の encoding の変更は Apply が断る（終了コード 1、DB は変わらない）。
5. 何周してもずれず、パスワードを設定し直さない。
6. 利用者の表がある DB の `ensure: absent` は断り（refused、行は残る）、空の DB は消す。宣言外の DB は unmanaged で prune しない。宣言外のロールは prune し、`pg_*`・管理用のロールは消さない。
7. サーバを止めると全部 Unknown・List が不完全で何も当てない（終了コード 3）。
8. 観測用（LOGIN だけ）と書き込み用のロール：書き込み用のパスワードのファイルが無くても plan は動き、reconcile は書けない。観測用のロールが流すのは `/* sakutei:… */` の読む問い合わせだけ（サーバのログで確かめる）で、CREATE DATABASE は書き込み用のロール。つなぐ 2 つのロールは List に出ず prune しない。
9. verify_passwords：観測用のロールが pg_authid を読めなければ Unknown（理由に pg_authid）、管理用の DB で列の SELECT を与えるか pg_read_all_data なら比べられる（別の DB で与えた列の権限は効かない）。
10. connect_roles：PUBLIC の CONNECT を外す DB に観測用のロールが繋がり、List は完全。手で REVOKE すると DB はずれ・拡張は Unknown・List は不完全で、1 周で戻る（その周は List が不完全なので終了コード 3）。宣言から外しても REVOKE しない。

sqlmigrate（観測は読むだけのロール、invoke はデータベースの持ち主のロールで、そのパスワードは secret_backends から `password_secret` で取り出す）：

1. 一回実行の reconcile で手順（on_drift → migrate → done_when）が 0 から 3 へ流し（E'' と $$ の引用・最後の ";" の無いファイル）、次の plan は何もしない。
2. 常駐が宣言の 1 へ down を逆順に流す。
3. 失敗する 0004：手順は止まり（42P01・版 3）、0004 は巻き戻り、次の周も送り直さない。
4. `rollback_on_fail`：失敗したら呼んだときの版（1）に戻して止まる。
5. CREATE INDEX CONCURRENTLY は 25001 で止まり、何も残さない。
6. 2 つの常駐（別々の Record）：テストが advisory lock を持つ間はどちらも送れていない（55P03）。外すと同じ版を二度流さずに 3 になり、両方の手順が済む。
7. 書き込み用の秘密が無い：plan は観測用の資格で読めて差分を出し、reconcile は何も流さず送れていない。秘密を戻すと収束する。
8. サーバを止める（docker pause）と Unknown（終了コード 3）で何もしない。

2026-10-31 に docker 29.1.3 で通した（postgresql は 2 テスト・15 段で約 150 秒、sqlmigrate は 8 段で約 43 秒）。

## 統合テスト（389 Directory Server、v3 段 8e）

```console
$ make integration-v3-ldap
```

v2 の `make integration-ldap` と同じ 389 Directory Server のイメージ（`389ds/dirsrv:3.1`、tag と digest で固定。`SAKUTEI_IT_389DS_IMAGE` で差し替えられる）を docker で立て（`sakutei-v3ldap-server`・クライアント `sakutei-v3ldap-client`・ネットワーク `sakutei-v3ldap-net`。v2 の `sakutei-ldapit-*` と重ならない）、v3 の CLI（`cli.Run` の plan・reconcile、`ldap.Module`）を向けて回す（`ldap/integration/`、build tag `integration_v3_ldap`）。ldapsearch・ldapmodify はクライアントのコンテナの中で動かす（ラッパー。-y のファイルと CA は docker cp）。docker と sudo（パスワードなし）が要るので、check・test・CI には入れない。コンテナとネットワークは終われば（落ちても）消す。

1. 空から作る（StartTLS）：plan は全部 create、reconcile で作り、objectClass にサーバが上位のクラスを足しても適合。userPassword はハッシュで保存され、宣言のパスワードで bind できる。`,`・`"`・Ü・改行を含む値、空白入りの member の DN。
2. 手で変える（mail・description・member・objectClass の追加・宣言に無い telephoneNumber）とずれ、宣言した属性だけを戻す。
3. userPassword は書き込みだけ：新しい世代でも書き直さず、サーバの userPassword を消すと次の周に今の値を書く。
4. 何周しても書かない（5 周、modifyTimestamp が変わらない）。
5. LDAPS でも適合。誤った bind のパスワードは全部 Unknown（理由つき）で List は不完全。
6. prune の範囲：宣言外の葉は消し、宣言した子のある宣言外の項目は消さない（RefuseError）。filter の外と base_dn は消さない。
7. 観測は読むだけの DN（ACI で read・search・compare だけ）、書き込みは Directory Manager：手で変えたものを戻し（modifiersName は Directory Manager）、bind の DN は宣言外に出ない。
8. サーバを止めると全部 Unknown（終了コード 3）、List は不完全で prune しない。起こすと適合。

2026-10-31 に Linux 7.0・docker 29.1.3 で通した（8 段・約 99 秒）。

## 統合テスト（S3 互換のサーバ、v3 段 8f）

```console
$ make integration-v3-s3
```

v2 の `make integration-s3` と同じイメージ（tag と digest で固定）の SeaweedFS 4.51（必須）・MinIO RELEASE.2026-08-04（任意）・PUT の API を持たない SeaweedFS 3.80（任意）を docker で 127.0.0.1 の空いたポートに立て（コンテナは `sakutei-v3s3-*`。v2 の `sakutei-s3it-*` と重ならない。`SAKUTEI_IT_SEAWEEDFS_IMAGE`・`SAKUTEI_IT_MINIO_IMAGE`・`SAKUTEI_IT_SEAWEEDFS_OLD_IMAGE` で差し替えられ、任意のものは取れなければその段を飛ばす）、v3 の CLI（`cli.Run` の plan・reconcile・approve、`s3.Module`）を向けて回し（`s3/integration/`、build tag `integration_v3_s3`）、終われば（落ちても）コンテナを消す。docker と sudo（パスワードなし）が要るので、check・test・CI には入れない。

1. 空から作る：versioning・policy・lifecycle・tags を 1 回の reconcile で作り、次の plan は変えない（MinIO がポリシーを `{"AWS":["*"]}` の形で持ってもずれにならない）。
2. 手で変えた policy と versioning は 2 項目の update のずれになり、reconcile が戻す。
3. 何周しても宣言どおりで、ポリシーを書き換えない。
4. `prune: true` の範囲でも宣言の外のバケット（中身のあるもの・空のもの）は消さずに unmanaged に出す。接頭辞の外のバケットは出ない。
5. `ensure: absent`：空のバケットは消え、中身のあるバケットは refused（オブジェクトがある）で残る。手で空にすると次の周で消える。
6. サーバを止める（docker pause）とその宣言は unknown・範囲は不完全・終了コード 3 で何もしない。動いているサーバは宣言どおり。戻せば収束する。
7. API を持たない endpoint（SeaweedFS 3.80）：バケットは作り、versioning の PUT は NotImplemented と分かる誤りで止まる。宣言しない設定は比べない。
8. endpoint の `fields`（policy・tags だけ）：ほかの設定は読まず、`versioning` の宣言は読み込みで断る（終了コード 2）。
9. 観測用と書き込み用の利用者（MinIO の `mc admin`）：書き込み用の secret key のファイルが無くても plan は回り、reconcile は書かずに誤り。読むだけの利用者の書き込みは 403。書き込み用の利用者で収束する。観測の許可が 1 つでも欠ければ Unknown（無いとは読まない）。
10. Object Lock：GOVERNANCE 1 日・COMPLIANCE 1 年で作って観測する。保持を 1 → 2 日に延ばす変更と既定の保持を外す変更は承認が揃うまで当てず、`sakutei approve` のあとに当てる。Object Lock の無い既にあるバケットへの `enable_on_create` は承認を求めず作り直さない誤りで止まる。MinIO の観測用の利用者は `s3:GetBucketObjectLockConfiguration` で読め、Put は 403。

2026-11-03 に docker 29.1.3・SeaweedFS 4.51・3.80・MinIO RELEASE.2026-08-04 で 3 テスト 17 段とも通した（約 42 秒）。

## 統合テスト（ansible-core、v3 段 8f）

```console
$ make integration-v3-ansible
```

v2 の `make integration-ansible` と同じ版を固定した venv（ansible-core 2.21.5、`~/.cache/sakutei/ansible-it-core-2.21.5`。無ければ同じ手順で作る）の ansible を、local 接続で一時ディレクトリを相手に v3 の CLI（`cli.Run` の plan・reconcile、`ansible.Module`）から回す（`ansible/integration/`、build tag `integration_v3_ansible`）。届かないホストは 127.0.0.1 の閉じたポートへの ssh。ansible は `SAKUTEI_IT_ANSIBLE`（絶対パス）で渡せ、ほかの版なら `SAKUTEI_IT_ANSIBLE_ANY=1` も。python3 と、初回は PyPI に届くことが要るので、check・test・CI には入れない。

1. 空から：plan は module の Update と check mode の差分（provider の理由）を出して何も変えず、reconcile が当てて読み直しで宣言どおり（`{{ … }}` はテンプレートとして評価しない）。
2. 手でファイルを変えると、変えた呼び出しだけがずれ（差分つき）、reconcile が戻す。
3. reconcile を 5 回続けても変える操作が無く、ファイルを書き換えない。
4. 届かないホストと check mode を持たないモジュール（ansible.builtin.command）は Unknown（理由つき、終了コード 3）で、コマンドは流れない。
5. `destructive: true` の 2 つの呼び出しは予算の壊す変更に数え、`destructive_per_cycle: 1` を超える周は止まって（終了コード 4）何も当てない。予算を 2 に上げると全部当たる。
6. グループ名の host は check mode の観測で 2 台に当たって Unknown になり当てない。args の秘密の参照・`ensure: absent`・範囲は読み込みで断る（終了コード 2）。
7. 消すのはモジュールの引数：`state: absent` の lineinfile が行を消し、次の plan は宣言どおり。

2026-11-03 に Linux 7.0・ansible-core 2.21.5 で 7 段とも通した（約 37 秒）。
