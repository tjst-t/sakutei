# Redfish の BMC provider（redfish-system / redfish-virtual-media / redfish-bios）

物理サーバの BMC を Redfish で扱う provider。決まりの正は設計書 §29.1「Redfish の BMC provider」（v3.15・v3.16）と「Redfish の BIOS の設定」（#140）。ここは使い方の説明で、矛盾したら spec/ が優先する。

| provider 名 | ID | 扱うもの |
| --- | --- | --- |
| `redfish-system` | ノード名（例 `node1`） | 電源（On / Off）・起動先の上書き（Continuous / Disabled）。Workflow の手順から一度きりの操作（1 回だけの起動先・再起動、#139） |
| `redfish-virtual-media` | `<ノード名>/cd`（例 `node1/cd`） | 仮想メディアの CD（Insert / Eject） |
| `redfish-bios` | `<ノード名>/bios`（例 `node1/bios`） | BIOS の設定（宣言した属性だけを `Bios/Settings` で合わせる。効くのは次の再起動、#140） |

どれも標準階級（List・BulkObserve を持たない）で、prune の対象にしない（サーバは Sakutei が作ったり消したりしない）。

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
| `power` | `On` / `Off` | Off→On は Reset On、On→Off は GracefulShutdown。PoweringOn / PoweringOff の間は操作を重ねない。再起動は宣言できない（Workflow の手順の `action: { op: reset }`、下の「一度きりの操作」）。 |
| `power_off` | `graceful`（既定）/ `graceful_then_force` | graceful は ForceOff に進まず、止まらなければずれが続いて stuck で知らせる。graceful_then_force は最初の GracefulShutdown から `force_after` を過ぎたら ForceOff（予算で破壊的に数える）。 |
| `force_after` | 時間（例 `5m`） | graceful_then_force のときだけ書け、そのときは必須。 |
| `boot_enabled` | `Continuous` / `Disabled` | `Once` は計画の時点でエラー（1 回起動で BMC が Disabled に戻すので収束しない。Workflow の手順の `action: { op: boot_once }` を使う）。 |
| `boot_target` | 例 `Pxe` / `Hdd` / `Cd` | `boot_enabled: Continuous` のときだけ比べる。PATCH だけで再起動はしない。 |

観測だけの値（宣言できない）: `uuid`・`macs`・`cpu_count`・`memory_gib`・`boot_mode`・`power_state`。

### 仮想メディアのフィールド

| フィールド | 値 | 意味 |
| --- | --- | --- |
| `image` | URL / 絶対パス / 空 | 違う image が入っていれば同じ Apply の中で Eject → Insert、空なら Eject。取得失敗で外れたら次の周に入れ直す。破壊的に数えない。 |

観測だけの値: `inserted`・`media_image`。

## BIOS の設定（redfish-bios、#140）

`attributes` に書いた BIOS の属性だけを合わせる。書かない属性は見ない・触らない（BMC の画面で人が変えてもずれにしない）。

```yaml
resources:
  - provider: redfish-bios
    id: node1/bios                     # <ノード名>/bios（システムの node1 と並べて書ける）
    fields:
      attributes: '{"ProcVirtualization": "Enabled", "PowerProfile": "MaxPerformance", "BootRetryCount": 3}'
```

| フィールド | 値 | 意味 |
| --- | --- | --- |
| `attributes` | JSON のオブジェクト（値は文字列・数・真偽） | 管理する属性。名前は BMC の属性の名前（`Bios` の `Attributes` のキー）。`""` か `{}` は何も管理しない。値の型は BMC のとおりに書く（`"3"` と `3` は違う）。 |

観測だけの値: `pending_attributes`（保留中の変更のうち今の値と違うもの、JSON）・`attribute_registry`（属性の登録簿の Id）・`bios_present`（BMC が Bios を持つか）。観測の `attributes` は今の値の全部（キー順の JSON）で、比べるのは宣言した属性だけ。

### 再起動が要る

BIOS の設定は書いてもすぐには効かない。sakutei は `Bios/Settings` に PATCH するだけで（保留中の変更になる）、**再起動はしない**。効かせるには再起動が要る:

- 人が再起動する、か
- Workflow の手順に `action: { op: reset }`（[一度きりの操作](#一度きりの操作workflow-の-action-手順139)）を書く。

保留中の変更があるあいだ、`redfish-bios` の観測は **converging** になる（BMC が次の再起動を待っている途中なので、同じ変更を重ねて出さない）。`plan` は `unobservable (availability=converging)`、一回性の `reconcile` は「収束しきらない」終了コード（1）、常駐は converging として数える。再起動で効けば、次の周に今の値で比べ直して converged になる。

- 保留中の変更が宣言と**違っても**重ねない（ほかの道具が置いた変更と取り合わない）。再起動で効いたあとに今の値で比べ、違えばそこで PATCH する。急ぐなら BMC の側で保留中を消す。
- 保留中に「既定値に戻す」（`ResetBiosToDefaultsPending`）があるときも converging。
- PATCH は宣言した属性のうち「保留中があればその値、無ければ今の値」と違うものだけ。PATCH のあと読み直し、保留中にも今の値にも宣言の値が見えなければ PermanentError（`Bios/Settings` を報告しない BMC に毎周 PATCH を重ねない）。
- BIOS の設定の変更は破壊的に数えない（予算 §13 を使わない）。再起動の `ForceRestart` は Workflow の側で破壊的に数える。

### 計画の時点で断るもの

- BMC が属性の登録簿（`Bios` の `AttributeRegistry` の Id、`/redfish/v1/Registries` から辿る）を出していれば、それで確かめる: 登録簿に無い名前・読み取り専用（`ReadOnly`・`Immutable`）・型違い（`Enumeration`・`String` は文字列、`Integer` は整数、`Boolean` は真偽）・選べない値・長すぎる / 短すぎる文字列・範囲の外の数・`Password` 型の属性（秘密は intent に書かない、§20）。登録簿は Id ごとに 1 度だけ読み、Id が変われば（BMC の版が上がれば）読み直す。
- 登録簿を出さない BMC では、今の値に無い名前と、今の値と JSON の型が違う値だけを断る。ほかは PATCH で BMC が断れば（400）PermanentError で、BMC の言葉をそのまま出す。
- BMC が Bios を持たない（System に `Bios` のリンクが無い・404）ときは、`attributes` を宣言したときだけ `the BMC of node "…" has no Bios resource` で止まる。システム・仮想メディアは影響を受けない。
- 届かない・5xx・時間切れは unknown（何も操作しない）。

### qemu-bmc で分かったこと（0.10.0）

- `Systems/1/Bios` の値は**文字列だけ**（`Integer`・`Boolean` の属性は無い）。属性と既定値は qemu-bmc の README の表のとおり（`ProcVirtualization`・`PowerProfile`・`AssetTag` など。`BootMode`・`SecureBoot` は読み取り専用）。値は表示用で、`BootMode`・`SecureBoot` 以外は VM の動きを変えない。
- `Bios/Settings` は保留中の属性だけを返す（今の値の写しは返さない）。PATCH は 200 で本文に Settings を返す。保留中は電源オン・`ForceRestart`・`GracefulRestart`・`PowerCycle` のたびに効いて空になる（ゲストの中からの再起動では効かない）。
- 登録簿は `/redfish/v1/Registries/BiosAttributeRegistry`（`Registry` は `BiosAttributeRegistry.1.0`）の `Location` の `Uri` にあり、`Bios` の `AttributeRegistry` は `BiosAttributeRegistry.1.0.0`。sakutei は `Registry` が Id の版を除いた頭に当たるものを使う。
- 読み取り専用・選べない値などの PATCH は 400（`@Message.ExtendedInfo` に属性ごとの Base のメッセージ）で、何も変えない。

偽 BMC（`internal/redfish/redfishtest`）は qemu-bmc の Bios を真似たうえで、数（`BootRetryCount`）と真偽（`SriovGlobalEnable`）の属性も持つ。契約テストは `internal/redfish/system/bios_test.go`（`[AC-140-1]`〜`[AC-140-5]`。契約テストは Apply の直後の観測が宣言どおりであることを求めるので、Apply と偽 BMC の再起動を組にして回す）、CLI の受け入れテストは `tests/acceptance/redfish_bios_test.go`（`[AC-140-6]`・`[AC-140-7]`）。

## 一度きりの操作（Workflow の action 手順、#139）

「次の 1 回だけ CD から起動」と「再起動」は、あるべき状態ではない（Once は BMC が 1 回の起動で使い切り、再起動は終われば何も残らない）ので宣言できない。
Workflow の手順（§6.3）の `action` として、その位置で 1 回だけ施す（`resource` は省けば Workflow の対象、書くなら `redfish-system/{target}`）。

```yaml
steps:
  - action:
      op: boot_once          # 起動先の上書きを Once・target にする（PATCH だけ。再起動しない）
      args:
        target: Cd           # BMC の AllowableValues にある値
  - action:
      op: reset              # 再起動。電源が切れていれば On を送る
      args:
        type: ForceRestart   # 省けば GracefulRestart
```

| op | args | 意味 |
| --- | --- | --- |
| `boot_once` | `target`（必須） | 起動先の上書きを `Once`・`target` にする。送ったあと読み直して Once・target になっていなければ halt。BMC が次の起動で Disabled に戻すので、上書きは残らない。 |
| `reset` | `type`: `GracefulRestart`（既定）/ `ForceRestart` | 電源が On なら `type` で再起動、Off なら Reset On。`ForceRestart` は**破壊的**として変更予算（§13）に数え、上限なら送らずに待つ。 |

- 送る前に BMC を読む。届かない・読めない・電源の切り替えの途中なら送らずに待ち、次の周に送り直す。
- 送る直前に「送りかけ」を Workflow の S2 に書く。書いたままデーモンが止まって起き直したら、送ったかどうか分からないので**二度送らずに halt** する（台を確かめて `sakutei workflow cancel`）。
- 効いたかは後の手順（`wait` / `require`）で確かめる。
- 知らない `op`・誤った `args`・設定に無いノードは、その手順で halt（何も送らない）。

## OS を入れる（Workflow と組み合わせる）

「CD に ISO を差す → 差さったことを確かめる → 次の 1 回だけ CD から起動 → 再起動 → インストーラが電源を切るのを待つ → 電源 On（ディスクから）→ 入れた OS を確かめる → CD を抜く」は、1 つの Workflow（§6.3）の手順として書ける。
CD は別の provider（`redfish-virtual-media`）なので、その手順には `resource: redfish-virtual-media/{target}/cd` を書き、電源を入れる前に `require` で差さったことを確かめる（Backlog #87）。
起動先は `action: boot_once` で 1 回だけ CD にする（#139）ので、終わったあとの起動先の宣言は `Disabled` のままでよく、戻す手順も要らない。
自動インストールの ISO は利用者が用意する（Sakutei の責務の外、§29.3 (c)）。台ごとの応答ファイル（ホスト名・アドレス・公開鍵・目印）は宣言から作れる（[template-file](template-file.md)）。
intent と Workflow の例・試用の結果（qemu-bmc + Alpine 3.24）・限界は [docs/trial/redfish-os-install/README.md](../trial/redfish-os-install/README.md)。
偽 BMC を相手にした受け入れテストは `tests/acceptance/workflow_action_test.go`（`[AC-139-*]`）、本物の OS を入れる統合テストは `make integration-guestos`（[guest-os](guest-os.md) の「統合テスト」）。

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
3. (1)〜(8) を順に回す（前の段の現実を次の段が使うので、1 つ落ちたら残りは回さない）。
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
| (7) `[AC-87-7]` | OS 導入の Workflow | CD を差して差さったことを確かめてから電源を入れる（[OS を入れる](#os-を入れるworkflow-と組み合わせる)）。 |
| (8) `[AC-140-8]` | BIOS の設定（redfish-bios） | node1 のシステムと `node1/bios`（`ProcVirtualization: Disabled`・`PowerProfile: Balanced`・`AssetTag`）を並べて宣言。plan は差分を出して `Bios/Settings` に書かない。reconcile は Settings に保留中として書き、今の値は変わらず電源も On のまま。2 回目の reconcile は converging で、qemu-bmc のログの「pending until the next boot」は 1 回だけ（PATCH を重ねない）。node2 に読み取り専用（`BootMode`）・選べない値・登録簿に無い名前を宣言すると plan がエラーで、保留中は空のまま。`ForceRestart` で効いたあと plan は Converged で、宣言していない属性は元のまま。 |

### 結果（2026-10-27、qemu-bmc 0.10.0、BIOS の設定を足して、#140）

(8) BIOS の設定を足して、同じ機械（docker 29.1.3・containerlab 0.81.1）で `make integration-redfish` が合格した。qemu-bmc の振る舞いは上の「qemu-bmc で分かったこと」のとおりで、sakutei の側で合わせたところは無い。

```
--- PASS: TestRedfishIntegration (84.93s)
    --- PASS: TestRedfishIntegration/0_lab (0.00s)
    --- PASS: TestRedfishIntegration/1_plan_reconcile (0.34s)
    --- PASS: TestRedfishIntegration/2_daemon_restores_drift (2.54s)
    --- PASS: TestRedfishIntegration/3_graceful_stuck (31.09s)
    --- PASS: TestRedfishIntegration/4_graceful_then_force (11.03s)
    --- PASS: TestRedfishIntegration/5_bmc_paused_unknown (30.76s)
    --- PASS: TestRedfishIntegration/6_once_rejected (0.01s)
    --- PASS: TestRedfishIntegration/7_workflow_cd_before_power (9.05s)
    --- PASS: TestRedfishIntegration/8_bios_settings (0.11s)
ok  	github.com/tjst-t/sakutei/tests/integration/redfish	89.105s
```

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
