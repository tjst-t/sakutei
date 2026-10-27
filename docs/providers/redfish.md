# Redfish の BMC provider（redfish-system / redfish-virtual-media）

物理サーバの BMC を Redfish で扱う provider。決まりの正は設計書 §29.1「Redfish の BMC provider」（v3.15・v3.16）。ここは使い方の説明で、矛盾したら spec/ が優先する。

| provider 名 | ID | 扱うもの |
| --- | --- | --- |
| `redfish-system` | ノード名（例 `node1`） | 電源（On / Off）・起動先の上書き（Continuous / Disabled） |
| `redfish-virtual-media` | `<ノード名>/cd`（例 `node1/cd`） | 仮想メディアの CD（Insert / Eject） |

どちらも標準階級（List・BulkObserve を持たない）で、prune の対象にしない（サーバは Sakutei が作ったり消したりしない）。

## BMC の一覧（--redfish-config）

BMC のアドレスと資格は intent に書かず、`--redfish-config <file>` で渡す（`plan`・`reconcile`・`status`・常駐の `sakutei`）。書式は intent と同じ JSON / YAML サブセット。

```yaml
# bmcs.yaml
secret_store: secrets.json            # パスワードを解決する file SecretBackend（必須）
bmcs:
  node1:
    address: https://172.20.20.2      # https だけ（scheme を省けば https）
    system: /redfish/v1/Systems/1     # 省略可（/redfish/v1/Systems の最初のメンバー）
    manager: /redfish/v1/Managers/1   # 省略可（/redfish/v1/Managers の最初のメンバー）
    username: admin
    password: secretref:file/bmc/node1  # SecretRef だけ（生の文字列はエラー）
    ca_file: bmc-ca.pem               # 省略可。BMC の証明書を検証する CA（PEM）
    timeout: 10s                      # 省略可（既定 10s、1 要求あたり）
  node2:
    address: https://172.20.20.3
    username: admin
    password:
      $secret: bmc/node2
      backend: file
    insecure_skip_verify: true        # 自己署名をそのまま許す。明示したときだけ（ca_file とは併用不可）
```

- `password` は SecretRef でしか書けない（§20）。`"secretref:file/<path>"` の文字列か `{"$secret": "<path>", "backend": "file"}`。生のパスワードを書くと読み込みの時点でエラーになる（エラーの文言にも値は出さない）。
- 値は `secret_store` の file SecretBackend（`internal/render/secretbackend_file.go`、§23.6 段0-minimal）から、BMC に要求を送るたびに解決する（JIT、§20.2）。sakutei は値を持ち続けないので、store の値を書き換えれば次の要求から新しい値を使う。読み込みの時点では store にその path があることだけを確かめる。
- store が読めない周は BMC に届かないのと同じ扱い（Unknown、何も操作しない）。
- 相対パス（`secret_store`・`ca_file`）は設定ファイルのあるディレクトリから。
- 未知のキーはエラー。
- intent に、設定に無いノードを書くと計画の時点でエラー（`no BMC is configured for node "…"`）。redfish のリソースがあるのに `--redfish-config` が無いのもエラー。

secret store（file SecretBackend）の例:

```json
{"secrets": {
  "bmc/node1": {"gen": 1, "value": "…"},
  "bmc/node2": {"gen": 1, "value": "…"}
}}
```

## intent の例

dnszone・nftables など、ほかの provider のリソースと同じ intent に混ぜられる。

```yaml
resources:
  # システム: 電源を落とし、PXE 起動を常に上書きしておく
  - provider: redfish-system
    id: node1
    fields:
      power: "Off"
      power_off: graceful_then_force   # 既定は graceful（ForceOff に進まない）
      force_after: 5m                  # graceful_then_force のときだけ・必須
      boot_enabled: Continuous         # Continuous / Disabled（Once はエラー）
      boot_target: Pxe                 # BMC の AllowableValues にある値

  # システム: 電源を入れ、起動先の上書きを外す
  - provider: redfish-system
    id: node2
    fields:
      power: "On"
      boot_enabled: Disabled

  # 仮想メディア: CD にインストーラを入れる（空の image は取り出した状態）
  - provider: redfish-virtual-media
    id: node1/cd
    fields:
      image: https://repo.example/install.iso   # URL か BMC から見える絶対パス

  - provider: redfish-virtual-media
    id: node2/cd
    fields:
      image: ""
```

YAML の `On` / `Off` は文字列のまま読まれる（bool になるのは `true` / `false` だけ）が、誤読を避けるため引用符で囲むのを勧める。

### システムのフィールド

| フィールド | 値 | 意味 |
| --- | --- | --- |
| `power` | `On` / `Off` | Off→On は Reset On、On→Off は GracefulShutdown。PoweringOn / PoweringOff の間は操作を重ねない。再起動は宣言できない（Task の領分）。 |
| `power_off` | `graceful`（既定）/ `graceful_then_force` | graceful は ForceOff に進まず、止まらなければずれが続いて stuck で知らせる。graceful_then_force は最初の GracefulShutdown から `force_after` を過ぎたら ForceOff（予算で破壊的に数える）。 |
| `force_after` | 時間（例 `5m`） | graceful_then_force のときだけ書け、そのときは必須。 |
| `boot_enabled` | `Continuous` / `Disabled` | `Once` は計画の時点でエラー（1 回起動で BMC が Disabled に戻すので収束しない）。 |
| `boot_target` | 例 `Pxe` / `Hdd` / `Cd` | `boot_enabled: Continuous` のときだけ比べる。PATCH だけで再起動はしない。 |

観測だけの値（宣言できない）: `uuid`・`macs`・`cpu_count`・`memory_gib`・`boot_mode`・`power_state`。

### 仮想メディアのフィールド

| フィールド | 値 | 意味 |
| --- | --- | --- |
| `image` | URL / 絶対パス / 空 | 違う image が入っていれば同じ Apply の中で Eject → Insert、空なら Eject。取得失敗で外れたら次の周に入れ直す。破壊的に数えない。 |

観測だけの値: `inserted`・`media_image`。

## OS を入れる（Workflow と組み合わせる）

「CD から起動 → インストーラが電源を切るのを待つ → ディスクから起動」は、最小 Workflow（§6.3）の手順として書ける。
自動インストールの ISO は利用者が用意する（Sakutei の責務の外、§29.3 (c)）。intent と Workflow の例・試用の結果（qemu-bmc 0.9.0 + Alpine 3.24）・限界は
[docs/trial/redfish-os-install/README.md](../trial/redfish-os-install/README.md)。

## 実行例

```console
$ sakutei plan      --intent intent/ --redfish-config bmcs.yaml
$ sakutei reconcile --intent intent/ --redfish-config bmcs.yaml
$ sakutei           --intent intent/ --redfish-config bmcs.yaml --interval 30s
```

偽 BMC（`internal/redfish/redfishtest`）を相手に plan → reconcile を通す受け入れテストは `tests/acceptance/redfish_cli_test.go`（`[AC-83-*]`）。

## 統合テスト（qemu-bmc + containerlab、`make integration-redfish`）

設計 §26.1 の 3 段目（§29.1 の（7））。本物の Redfish（[qemu-bmc](https://github.com/tjst-t/qemu-bmc) **0.10.0**、イメージ `ghcr.io/tjst-t/qemu-bmc:0.10.0@sha256:875c9b6490eb444821a5dbcde348716bedfce767563d4c683728b7185022526a` に tag と digest で固定）を containerlab で 2 台立て、sakutei の CLI と常駐ループを向ける。KVM・docker・containerlab と sudo が要るので、`make check`・`make test`・CI には入れない（テストは build tag `integration_redfish` の付いたファイルにあり、`go test ./...` では空のパッケージになる）。

### 要るもの

- `/dev/kvm`（accel=kvm で動かす）と `/dev/net/tun`
- docker と containerlab（確かめた版: docker 29.1.3、containerlab 0.81.1）
- パスワードなしの `sudo`（containerlab と docker を sudo で動かす。環境変数 `SAKUTEI_SUDO=` を空にすると sudo を付けない）
- Go 1.22 以上（テストが sakutei をビルドする）

### 回し方

```console
$ make integration-redfish
```

`go vet -tags integration_redfish` のあと `go test -tags integration_redfish -v ./tests/integration/redfish/` を回す。テスト（`TestMain`）は次を自分でやる。

1. 一時ディレクトリにトポロジ（`tests/integration/redfish/topology.clab.yml`）を写し、小さな ISO 9660 を 2 つ（`iso/sakutei-a.iso`・`iso/sakutei-b.iso`、テストがその場で書く）と sakutei のバイナリ、BMC の一覧（`insecure_skip_verify: true`、パスワードは file SecretBackend の SecretRef）を置く。
2. 同じ名前の lab の残りを消してから `sudo containerlab deploy` する。lab 名と管理ネットは `sakutei-rf84`（`172.31.84.0/24`、BMC は node1 = `172.31.84.11`・node2 = `172.31.84.12`、Redfish は HTTPS 443・自己署名・`admin`/`password`）。
3. (1)〜(6) を順に回す（前の段の現実を次の段が使うので、1 つ落ちたら残りは回さない）。
4. **終わったら必ず `containerlab destroy --cleanup`**（失敗しても・Ctrl-C で止めても）。テストのプロセスごと落ちた（`-timeout` など）ときは、Makefile が lab の残りを見つけて消す。

トポロジの要点: 2 台とも `/dev/kvm`・`/dev/net/tun` を渡し、`SYSTEM_UUID`・`VM_MACS` を固定、VM の NIC は eth1（2 台を直結）、`GRACEFUL_TIMEOUT` は 10 秒。**`VM_DISK_SIZE` は付けない**——ディスクの無い VM は BIOS のまま ACPI の電源ボタンに応えないので、GracefulShutdown は PoweringOff のあと `GRACEFUL_TIMEOUT` 秒で On に戻る。これを既定停止の stuck と graceful_then_force の確かめに使う。`POWER_ON_AT_START` の既定（false）で VM は Off から始まる。

手で lab を立てて触るとき（後で必ず消す）:

```console
$ cd tests/integration/redfish && mkdir -p iso && cp /path/to/some.iso iso/
$ sudo containerlab deploy -t topology.clab.yml
$ curl -sk -u admin:password https://172.31.84.11/redfish/v1/Systems/1
$ curl -sk -u admin:password -X POST -H 'Content-Type: application/json' \
    -d '{"ResetType": "ForceOff"}' https://172.31.84.12/redfish/v1/Systems/1/Actions/ComputerSystem.Reset
$ sudo containerlab destroy -t topology.clab.yml --cleanup && rm -rf iso
```

### 確かめること

| | 内容 | 確かめ方 |
| --- | --- | --- |
| (0) | lab が想定どおり | Managers/1 の FirmwareVersion が `v0.10.0`。0.9 以降、サービスルート（`/redfish/v1`）は認証なしで 200、`Systems/1` は認証なしで 401。UUID は `SYSTEM_UUID` のとおり、最初は Off。 |
| (1) `[AC-84-1]` | plan → reconcile で宣言に収束 | node1 = On・Continuous の Pxe・CD に `sakutei-a.iso`、node2 = On・Disabled・CD は空。plan は差分を出して BMC に書かない。reconcile のあと Redfish の GET が宣言どおりで、QEMU が `accel=kvm` で動いている（`docker top`）。続く plan は `Converged (no changes)`。image を `sakutei-b.iso` に変えると同じ Apply の中で Eject → Insert して収束。 |
| (2) `[AC-84-2]` | 手で変えたものを次の周で戻す | 常駐の sakutei（`--interval 2s`）を回したまま、curl 相当の要求で node2 を ForceOff、node1 の起動先の上書きを Disabled、node1 の CD を Eject。次の周で 3 つとも cause B の Drift として出て、On・Continuous/Pxe・`sakutei-b.iso` に戻る。 |
| (3) `[AC-84-3]` | 既定の停止（graceful）は ForceOff しない | node1 を `power: "Off"`（power_off は既定）にして常駐（`--stuck-after 3`）。電源は On → PoweringOff → On を繰り返し、一度も Off にならない。BMC のログに「guest did not power off」が 2 回以上（GracefulShutdown を送り直している）。Drift が `class: stuck`・`severity: critical` に上がる（`--event-log`）。plan はまだ Off を求める。 |
| (4) `[AC-84-4]` | graceful_then_force は force_after 後に ForceOff | `power_off: graceful_then_force`・`force_after: 10s`。電源の移り変わりは On → PoweringOff →（BMC があきらめて）On → **Off**（PoweringOff を経ずに Off = ForceOff）。Off になったのは起動から force_after 以上あと。予算で止まらない（BudgetHalted なし）。続く plan は Converged。 |
| (5) `[AC-84-5]` | BMC を止めると Unknown で何も操作しない | node2 を手で ForceOff・node1 の起動先を手で外してから、node2 のコンテナを `docker pause`。plan は node2 の観測が unreachable でエラー。常駐は node2 を毎周「unobservable」として飛ばし（3 周以上）、node1 は収束を続ける（起動先が戻る）。node2 には Apply を出さない。unpause して 5 秒待っても node2 は Off のまま（止まっている間に送った要求が受信待ちに溜まっていれば、ここで実行されてしまう）。reconcile で On に戻る。 |
| (6) `[AC-84-6]` | Once は計画の時点でエラー | node2 を `boot_enabled: Once` にすると、plan も reconcile も `boot_enabled "Once" cannot be declared` で失敗し、node2 は変わらない。 |

### 結果（2026-10-27、qemu-bmc 0.10.0）

同じ開発用のコンテナで、qemu-bmc を 0.10.0（上の digest）に上げて `make integration-redfish` が合格した。sakutei の側で変えたのは、lab の版の確かめ（FirmwareVersion）とイメージの固定だけ。

```
--- PASS: TestRedfishIntegration (75.35s)
    --- PASS: TestRedfishIntegration/0_lab (0.00s)
    --- PASS: TestRedfishIntegration/1_plan_reconcile (0.35s)
    --- PASS: TestRedfishIntegration/2_daemon_restores_drift (2.10s)
    --- PASS: TestRedfishIntegration/3_graceful_stuck (31.08s)
    --- PASS: TestRedfishIntegration/4_graceful_then_force (11.03s)
    --- PASS: TestRedfishIntegration/5_bmc_paused_unknown (30.78s)
    --- PASS: TestRedfishIntegration/6_once_rejected (0.01s)
```

### 結果（2026-10-22、qemu-bmc 0.9.0、開発用のコンテナ）

KVM が使える開発用のコンテナ（`/dev/kvm` あり、docker 29.1.3・containerlab 0.81.1）で `make integration-redfish` が合格した。lab の deploy から destroy まで約 80 秒。

```
--- PASS: TestRedfishIntegration (75.80s)
    --- PASS: TestRedfishIntegration/0_lab (0.00s)
    --- PASS: TestRedfishIntegration/1_plan_reconcile (0.34s)
    --- PASS: TestRedfishIntegration/2_daemon_restores_drift (2.54s)
    --- PASS: TestRedfishIntegration/3_graceful_stuck (31.10s)
    --- PASS: TestRedfishIntegration/4_graceful_then_force (11.03s)
    --- PASS: TestRedfishIntegration/5_bmc_paused_unknown (30.78s)
    --- PASS: TestRedfishIntegration/6_once_rejected (0.01s)
ok  	github.com/tjst-t/sakutei/tests/integration/redfish	80.072s
```

見えたこと:

- (3) の電源の移り変わりは `[On PoweringOff On PoweringOff On PoweringOff]`。GRACEFUL_TIMEOUT（10 秒）ごとに BMC が On に戻し、sakutei が次の周に GracefulShutdown を送り直す。Drift は送り直すたびに数えられ、3 回目（約 30 秒）で stuck に上がった。PoweringOff の間は Converging で、周は数えない。
- (4) は `[On PoweringOff On Off]`、起動から 11 秒で Off。
- (5) の BMC が止まっている間は、要求が 3 秒（BMC の一覧の `timeout`）で切れて Unknown になる（TCP は張れるが HTTPS の応答が来ない）。
- qemu-bmc 0.9.0 は path の InsertMedia でファイルの中身を確かめないが、テストは正しい ISO 9660 を差している。
