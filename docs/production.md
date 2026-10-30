# 本番の構成の手引き（鍵・監査・秘密・TLS・freeze・runtime）

常駐の `sakutei` を本番で長く回すときの構成をまとめた手引きである（Backlog #190）。観測用と書き込み用の資格、監査の記録と
外への送り、秘密の取り出し先、読み取りの口の TLS と認可、freeze の使い方、intent の `runtime:` と起動の旗の扱いを、1 つの
置き場の例でつなげて書く。決まりの正は設計書（§1.7・§14.3・§20・§26.3・§26.7・§26.8）で、各口の細かい決まりはリンク先の
文書にある。矛盾したら spec/ が優先する。

この文書の設定の例（`<!-- check: … -->` の印の付いた塊）は、そのまま読み込めることをテストで確かめている
（`pkg/cli/production_doc_test.go`）。テストは `/etc/sakutei`・`/var/lib/sakutei`・`/srv/sakutei` を一時ディレクトリに置き換え、
金庫（secret store）と CA の束はテストが作る。コマンドの例は、旗の名前がそのコマンドにあることを確かめ、freeze と監査の確かめは
実際に走らせる。

## 置き場と分け方

正しい状態は宣言に置き、ツールの中だけに持たない（§1.7）。本番では次の 3 つに分けて置く。

| 何を | どこに | 例 |
| --- | --- | --- |
| **管理の範囲と回り方**（どれを管理するか・予算・mode・周期） | intent（git。PR で変える） | `providers:`（範囲、§7.7）・`external_owners:`（触らないもの）・`runtime:`（長く効く起動の引数、§26.7） |
| **つなぎ方**（宛先・鍵・時間） | デーモンのホストの設定のファイル | `--provider-config`・`remote_config`・`proxmox_config`・`--listen-auth-config`・`--otlp-config` |
| **その場の操作**（一時的な止め・締め） | 起動の旗・`sakutei freeze` | `--freeze`・`--freeze-state`・締める向きの旗 |

この手引きの例の置き場:

```
/etc/sakutei/                      # つなぎ方（root:sakutei 0640。秘密は金庫にだけ）
  provider-config.yaml             # --provider-config（provider のつなぎ方と secret_backends:）
  remote.yaml                      # ssh 越しの対象の一覧（観測用と書き込み用の鍵）
  pve.yaml                         # Proxmox VE のクラスタ（観測用と書き込み用のトークン）
  listen-auth.yaml                 # /metrics と読み取りの口の TLS と認可
  otlp.yaml                        # トレースの出口の TLS と認可
  secrets.json                     # file SecretBackend（接続の鍵・トークン・口の証明書。0600）
  known_hosts  *-ca.pem  vault-role-id  vault-secret-id  siem-client*.pem
/var/lib/sakutei/                  # S2・S4（デーモンが書く）
  audit/audit.log                  # S4 監査（WORM の置き場にしてよい）
  audit.syslog-state               # 送り終えた所（書き換えるので WORM の外）
  freeze.json                      # freeze の状態（sakutei freeze が書く）
  workflow.json  task.json         # S2（Workflow・Task の進み）
/srv/sakutei/intent/               # intent（git の作業木。--intent-repo なら取得先）
```

## 観測用と書き込み用の資格（§20.7、#185・#186）

観測（`plan`・`status`・読み取りの口・常駐の毎周の観測）と書き込み（Apply・Workflow の action）で資格を分ける。観測用は読むだけの
ユーザー・トークンにし、書き込み用の鍵は**デーモンのホストの金庫にだけ**置く。観測用の鍵だけを置いた運用者の PC からも `status`・
`plan` が回る（書かない回は書き込み用の資格を取り出さず、金庫に無くても起動する）。

ssh 越しの provider（nftables・host-file など）は対象の一覧のホストに `observe:`・`write:` を書く（[remote.md](remote.md#観測用と書き込み用の資格)）:

<!-- check: file /etc/sakutei/remote.yaml -->
```yaml
secret_store: /etc/sakutei/secrets.json
known_hosts: /etc/sakutei/known_hosts
hosts:
  fw1:
    address: 192.0.2.11
    observe:                        # 観測：nft の list だけを sudoers で許したユーザー
      user: sakutei-ro
      private_key: secretref:file/ssh/fw1-ro
      sudo: true
    write:                          # 書き込み：Apply の中だけ。使い終われば鍵の一時ファイルを消す
      user: sakutei
      private_key: secretref:file/ssh/fw1-rw
      sudo: true
  web1:
    address: 192.0.2.21
    observe:
      user: sakutei-ro
      private_key: secretref:file/ssh/web1-ro
    write:
      user: sakutei
      private_key: secretref:file/ssh/web1-rw
      sudo: true
```

管理 API の provider（proxmox-vm・redfish・ldap・postgresql・s3-bucket）は対象の設定に同じ `observe:`・`write:` を書く
（[proxmox-vm](providers/proxmox-vm.md#観測用と書き込み用の資格backlog-186) ほか各 provider の文書）。観測用のトークンは
Proxmox VE なら `/` に `PVEAuditor`:

<!-- check: file /etc/sakutei/pve.yaml -->
```yaml
secret_store: /etc/sakutei/secrets.json
clusters:
  tokyo:
    endpoint: https://pve1.example.net:8006
    ca_file: /etc/sakutei/pve-root-ca.pem
    observe:
      token_id: sakutei-ro@pve!audit
      token_secret: secretref:file/proxmox/tokyo/audit
    write:
      token_id: sakutei@pve!sakutei
      token_secret: secretref:file/proxmox/tokyo/token
    timeout: 15s
```

`--provider-config` はつなぎ方だけ。扱うホスト・クラスタの scope・管理する表（範囲）は intent の `providers:` に書く
（[providers/ranges.md](providers/ranges.md)。設定にだけあれば警告し、両方にあって違えば読み込みで断る）:

<!-- check: file /etc/sakutei/provider-config.yaml -->
```yaml
providers:
  nftables:
    ruleset_target: live
    ruleset_hosts: fw1
    remote_config: /etc/sakutei/remote.yaml
  host-file:
    remote_config: /etc/sakutei/remote.yaml
  proxmox-vm:
    proxmox_config: /etc/sakutei/pve.yaml
secret_backends:
  vault:
    type: vault-kv2
    address: https://vault.example.net:8200
    ca_file: /etc/sakutei/vault-ca.pem
    approle_role_id_file: /etc/sakutei/vault-role-id
    approle_secret_id_file: /etc/sakutei/vault-secret-id
```

<!-- check: file /srv/sakutei/intent/providers.yaml -->
```yaml
providers:
  nftables:
    scope:
      site: tokyo
    tables:
      - inet/filter
  host-file:
    hosts:
      - web1
  proxmox-vm:
    clusters:
      tokyo:
        scope:
          site: tokyo
```

- この回が書くか（`access`）は本体が命令と mode から決める。`--provider-config` にも intent にも書けない。
- **書かない回**：`plan`・`status`・`import`・`sync`・`graph`、`--mode alert-only` か `--freeze` の `reconcile`、期限の無い `--freeze`
  の常駐。書き込み用の鍵が金庫に無くても起動し、書こうとすれば断る。
- **書く回**（`reconcile`・常駐の既定）は書き込み用の鍵が金庫に無ければ起動しない。`--freeze-state` で freeze する常駐、期限つきの
  `--freeze` の常駐、remediation schedule で時間帯により alert-only になる常駐は、途中で書く回に戻るので書く回として扱う。
- 運用者の PC には観測用の鍵（`ssh/*-ro`・`proxmox/*/audit`）だけを入れた金庫を置き、同じ `remote.yaml`・`pve.yaml`
  （`secret_store` だけ違う）で `status`・`plan` を回す。

## 秘密の取り出し先（§20.2・§20.3、#188）

- **接続の資格**（ssh の鍵・API のトークン・口の証明書と鍵・読むだけのトークン）は file SecretBackend（`secrets.json`、
  `secretref:file/…`）から取り出す。値は使う直前に読み（JIT）、provider にもプロセスにも持たない。設定に生の値を書けば断る。
- **リソースの中の秘密**（アプリの資格のファイル・DB のロールのパスワードなど）は、`--provider-config` の `secret_backends:`
  （上の例の `vault`）から取り出せる。intent は `#gen` を書かない。render の段で KV の版を読んで ref に入れ、版が上がれば差分
  （cause E）として Apply する。読めなければそのリソースだけを unknown にし、空や古い値で書かない。

<!-- check: file /srv/sakutei/intent/app.yaml -->
```yaml
resources:
  - provider: host-file
    id: web1/etc/app/db.env
    fields:
      content_secret: secretref:vault/app/db#env   # KV v2 の secret/app/db のキー env
      mode: "0600"
      owner: root
```

- Vault の側の権限は読むだけで足りる（`secret/data/app/*`・`secret/metadata/app/*` に `read`）。AppRole の secret_id のファイルは
  デーモンのユーザーだけが読めるようにする。書き方・外のコマンドの取り出し先は [providers/secret-backends.md](providers/secret-backends.md)。
- 秘密の読み書きの監査は取り出し先の側（Vault の audit device）で取る（§26.3）。Sakutei の監査には値も版の中身も書かない。

## 監査の記録と外への送り（§26.3、#187・#210）

`--audit-log` に S4 監査（JSON Lines）を残す。ふつうの Apply と prune（Op ごとに resource・変えた欄の名前・cause・結果）、承認
ゲート、planned_change、freeze、break-glass、Workflow の run と手が入る。各行は前の行のハッシュで鎖になる。

本番では次の 3 つをそろえる:

1. **書き換え・切り詰めをしない置き場**：`--audit-rotate-bytes` で大きくなれば新しいファイル（`audit.log.000001` …）へ続ける。
   元のファイルは書き換えないので WORM（書いたものを変えられない置き場）に置ける。送り終えた所のファイル
   （`--audit-syslog-state`）は書き換えるので WORM の外に置く。
2. **外へすぐ送る**：`--audit-syslog tls://…` で 1 件ずつ syslog の受け手（SIEM）へ送る。受け手が止まっている間は手元のログに
   貯まり、`--audit-syslog-retry` ごとに続きから送り直す。送れなくても収束は止めない。受け手の証明書を確かめる CA
   （`--audit-syslog-ca`）と、受け手が求めればクライアント証明書（`--audit-syslog-cert`・`--audit-syslog-key`。これは SecretRef
   でなくファイルなので、デーモンのユーザーだけが読める 0600 にする）を渡す。
3. **定期に確かめる**：`sakutei audit verify` で鎖をたどる（書き換え・抜き取り・差し込み、送り終えた所より後ろの切り詰めを見つけ
   れば終了コード 3）。systemd の timer などで回し、3 を知らせにつなぐ。

監査のログを書くコマンド（常駐・`reconcile`・`approve`・`freeze`・`workflow`・`task` など）には、同じ `--audit-log` と同じ
`--audit-syslog*` を渡す（1 つの鎖に順に並ぶ。追記は `<file>.lock` の flock で並べる）。

<!-- check: run -->
```console
$ sakutei audit verify --audit-log /var/lib/sakutei/audit/audit.log --audit-syslog-state /var/lib/sakutei/audit.syslog-state
```

受け手に送れていない分をその場で送るときは `sakutei audit forward`（常駐と同じ `--audit-syslog*` を渡す）。

## 読み取りの口の TLS と認可（#161）

`/metrics`（`--metrics-listen`）と読み取りの口（`--read-listen`）は、明示したときだけ待ち受ける。中身には構成（ID・欄の値・ずれ）が
入るので、**loopback 以外で開くときは TLS と認可の両方**を `--listen-auth-config` に書く（片方だけ・どちらも無いときは、
`--metrics-insecure-listen`・`--read-insecure-listen` が無い限り起動で断る）。

<!-- check: file /etc/sakutei/listen-auth.yaml -->
```yaml
secret_store: /etc/sakutei/secrets.json
metrics:                                   # Prometheus：読むだけのトークン
  tls:
    cert: secretref:file/sakutei/listen_cert
    key: secretref:file/sakutei/listen_key
    min_version: "1.3"
  bearer_token: secretref:file/sakutei/metrics_token
read:                                      # 可視化の道具：クライアント証明書
  tls:
    cert: secretref:file/sakutei/listen_cert
    key: secretref:file/sakutei/listen_key
  client_ca_file: /etc/sakutei/clients-ca.pem
  allowed_subjects:
    - grafana
```

- 証明書・鍵・トークンは SecretRef だけ。30 秒ごとに金庫を読み直し、再起動なしで入れ替わる（CA の束の入れ替えは再起動）。
  書き方の全体は [metrics.md の「TLS と認可」](metrics.md#tls-と認可--listen-auth-config)。
- **承認の口（`--approval-listen`）はこの設定を通らず、認可を持たない。** loopback で待ち受け、承認する人は ssh のポート転送か、
  認証する前段のプロキシを通す。読み取りの口と同じアドレスには置けない。
- トレースの出口（`--otlp-endpoint`）も同じ考えで、loopback 以外の宛先は https と認可（トークンかクライアント証明書）が要る
  （[tracing.md](tracing.md)）:

<!-- check: file /etc/sakutei/otlp.yaml -->
```yaml
secret_store: /etc/sakutei/secrets.json
tls:
  ca_file: /etc/sakutei/collector-ca.pem
  min_version: "1.3"
bearer_token: secretref:file/sakutei/otlp_token
service_name: sakutei-tokyo
timeout: 10s
```

## freeze の使い方（§14.3、#184）

freeze は全体を alert-only に倒す（ずれは見続け、直すのを止める）。止めた事実は誰が・なぜ・いつまでつきで監査に残り、効いている
間は level の Event `Frozen` と status に出る。理由の無い freeze は断る。

本番の常駐には **`--freeze-state` を付けておく**。動いている常駐を止めずに、`sakutei freeze start` / `end` で freeze できる
（常駐は同じファイルを毎周読む。読めない周は freeze として扱う）。同じ `--freeze-state` を一回性の `reconcile` にも渡せば、
手で回す reconcile も同じ freeze に従う。

<!-- check: run -->
```console
$ sakutei freeze start --state /var/lib/sakutei/freeze.json --audit-log /var/lib/sakutei/audit/audit.log --by alice --reason "年末の変更凍結 CHG-1234" --until 72h
$ sakutei freeze status --state /var/lib/sakutei/freeze.json --audit-log /var/lib/sakutei/audit/audit.log
$ sakutei freeze end --state /var/lib/sakutei/freeze.json --audit-log /var/lib/sakutei/audit/audit.log --by alice --reason "凍結の期間が明けた"
```

- **期限を付ける**（`--until`。RFC 3339 の時刻か今からの長さ）。過ぎれば次に読んだ者が解き、期限切れを記録する（解き忘れで自動
  修復が止まったままにならない）。効いている freeze があるところへの `start` は断る（先に `end`）。
- `status --freeze-state` と、常駐の `--daemon-status`・読み取りの口の `/status` に効いている freeze が出る。
- 起動の旗 `--freeze`（`--freeze-reason` と `--audit-log` が要る）は、その実行・その常駐の間だけ効く。調べもののために一時的に
  alert-only の常駐を立てるときなどに使う。期限の無い `--freeze` の常駐は書かない回なので、書き込み用の鍵が要らない。
- 時間帯で決まる止め（保守の窓の外は直さない）は freeze でなく intent の remediation schedule に書く。freeze は予定の外の判断に使う。

## intent の `runtime:` と起動の旗（§26.7、#183・#193）

予算・既定の mode・settle・上限・周期のように起動のたびに同じ値を渡す引数は、intent の `runtime:` に書く。systemd のユニットの
旗にだけ置くと、回り方の正が宣言の外に残る。

<!-- check: file /srv/sakutei/intent/runtime.yaml -->
```yaml
runtime:
  interval: 30s
  mode: auto
  max_changes_per_cycle: 20
  max_destructive_per_cycle: 2
  max_destructive_per_window: 10
  window_duration: 24h
  on_exceed: halt-all
  stuck_after: 5
  oneshot:                         # 一回性の reconcile と plan だけに効く値
    max_changes_per_cycle: 50
```

- 書くのは intent のファイルのどれか 1 つに 1 回だけ（inventory のファイルには書けない）。知らないキー・範囲の外の値は読み込みで断る。
- ユニットの旗には `runtime:` に書いた引数を**重ねて書かない**（下のユニットの例）。旗だけ・宣言だけならそれを使う。
- 旗が宣言より**締める向き**（予算・上限が小さい、`mode` が `alert-only`、`on_exceed` が `halt-all`）なら旗を使い、level の Event
  `RuntimeTightened` と status に旗の値と宣言の値を両方出す。その場の締めに使い、締めを続けたいなら宣言を変える:

<!-- check: runtime-tightens daemon -->
```console
$ sakutei --intent /srv/sakutei/intent --provider-config /etc/sakutei/provider-config.yaml --max-destructive-per-cycle 1 --mode alert-only
```

- **緩める向き**（上限の `0` は上限なしで、いちばん緩い）と、向きの無い引数（`interval`・`settle`・`window_duration`・`reobserve_*`・
  `stuck_after`）の食い違いは、引数ごとに 1 行の理由（旗の値・宣言の値・直し方）を出して起動を断る（終了コード 1）:

<!-- check: runtime-refused daemon -->
```console
$ sakutei --intent /srv/sakutei/intent --provider-config /etc/sakutei/provider-config.yaml --max-destructive-per-cycle 0 --interval 10s
```

- 常駐は起動したときの値で回り続ける。動いている間に `runtime:` が変われば level の Event `RuntimeDeclarationChanged` で「起こし直すと
  取り込む」と知らせる。
- `--freeze`・`--approve-large-change` はその場の操作なので `runtime:` に置かない。

## 常駐のユニットの例

上の置き場をつないだ systemd のユニット（抜粋）。回り方の引数（周期・予算・mode）は intent の `runtime:` にあるので旗に書かない。

<!-- check: flags -->
```ini
[Service]
User=sakutei
ExecStart=/usr/local/bin/sakutei \
  --intent /srv/sakutei/intent \
  --provider-config /etc/sakutei/provider-config.yaml \
  --workflow-store /var/lib/sakutei/workflow.json \
  --task-store /var/lib/sakutei/task.json \
  --audit-log /var/lib/sakutei/audit/audit.log \
  --audit-rotate-bytes 67108864 \
  --audit-syslog tls://siem.example.net:6514 \
  --audit-syslog-ca /etc/sakutei/siem-ca.pem \
  --audit-syslog-cert /etc/sakutei/siem-client.pem \
  --audit-syslog-key /etc/sakutei/siem-client-key.pem \
  --audit-syslog-state /var/lib/sakutei/audit.syslog-state \
  --freeze-state /var/lib/sakutei/freeze.json \
  --daemon-status /run/sakutei/daemon-status.json \
  --listen-auth-config /etc/sakutei/listen-auth.yaml \
  --metrics-listen 192.0.2.10:9470 \
  --read-listen 192.0.2.10:9471 \
  --approval-listen 127.0.0.1:8470 \
  --otlp-endpoint https://collector.example.net:4318 \
  --otlp-config /etc/sakutei/otlp.yaml
```

運用者が手で回すときも、同じ置き場を指す:

<!-- check: flags -->
```console
$ sakutei status --intent /srv/sakutei/intent --provider-config /etc/sakutei/provider-config.yaml --freeze-state /var/lib/sakutei/freeze.json --daemon-status /run/sakutei/daemon-status.json
$ sakutei plan --intent /srv/sakutei/intent --provider-config /etc/sakutei/provider-config.yaml
$ sakutei reconcile --intent /srv/sakutei/intent --provider-config /etc/sakutei/provider-config.yaml --audit-log /var/lib/sakutei/audit/audit.log --audit-syslog tls://siem.example.net:6514 --audit-syslog-ca /etc/sakutei/siem-ca.pem --freeze-state /var/lib/sakutei/freeze.json --workflow-store /var/lib/sakutei/workflow.json
```

## 確かめる一覧

| 項目 | 確かめ方 |
| --- | --- |
| 書き込み用の鍵がデーモンのホストにだけある | 運用者の PC の金庫で `sakutei status` が通り、`sakutei reconcile` は `write.private_key`／`write.token_secret` が無いと断る |
| 観測用のユーザーが読むだけ | 各 provider の文書の「観測用と書き込み用の資格」の権限の表のとおりか（sudoers・NACM・PVEAuditor など） |
| 範囲が intent にある | `plan`・`status` に「is only in the provider config」の警告が出ない |
| 監査が外に届く | SIEM に `sequenceId` の続いた記録が届く。`sakutei audit verify` が 0 で終わる |
| 口が守られている | 資格なしの `curl https://…:9471/status` が 401／403。平文の `http://` は届かない |
| freeze が効く | `sakutei freeze start` の次の周から `Frozen` が firing、読み取りの口の `/plan` の `remediation` が alert-only。`end` で resolved |
| 回り方が宣言にある | ユニットの旗に `runtime:` の引数が無い。status に `runtime_tightened` が出ていない |
