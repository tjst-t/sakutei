# Redfish の BMC provider（redfish-system / redfish-virtual-media / redfish-bios / redfish-firmware）

物理サーバの BMC を Redfish で扱う provider。決まりの正は設計書 §29.1「Redfish の BMC provider」（v3.15・v3.16）と「Redfish の BIOS の設定」（#140）・「Redfish の FW の更新」（#148）。ここは使い方の説明で、矛盾したら spec/ が優先する。

| provider 名 | ID | 扱うもの |
| --- | --- | --- |
| `redfish-system` | ノード名（例 `node1`） | 電源（On / Off）・起動先の上書き（Continuous / Disabled）。Workflow の手順から一度きりの操作（1 回だけの起動先・再起動、#139） |
| `redfish-virtual-media` | `<ノード名>/cd`（例 `node1/cd`） | 仮想メディアの CD（Insert / Eject） |
| `redfish-bios` | `<ノード名>/bios`（例 `node1/bios`） | BIOS の設定（宣言した属性だけを `Bios/Settings` で合わせる。効くのは次の再起動、#140） |
| `redfish-firmware` | `<ノード名>/firmware/<部品の Id>`（例 `node1/firmware/BMC`） | FW の版（UpdateService の FirmwareInventory を観測し、違えば承認ゲートつきの多相 Task で更新する、#148） |

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
- API のパスの前置きを変える欄（proxmox-vm・s3-bucket の `api_base_path`、Backlog #160）は持たない。Redfish はサービスルートを規格（DSP0266）で `/redfish/v1` に決めており、provider は BMC が返す `@odata.id`（サーバの根からの絶対パス）を辿るので、前置きを足しても辿った先には効かない。サービスルートの下で置き場所が BMC ごとに違うものは、今までどおり `system`・`manager` で指す。
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

### Apply が効いたかの確かめ（settle、#166）

BMC は Reset に 204 を返してから PowerState を変える。Sakutei は Apply が成功を返したリソースを読み直して Diff が消えたかを
確かめ、`redfish-system` では既定で **15 秒**（settle）まで待って読み直す（PoweringOn・PoweringOff の間は Converging なので、
そこで確かめを終えて数えない）。settle を過ぎても電源が変わらない（BMC が命令を受けて何もしない故障。qemu-bmc の障害注入
`power-action-ignored`）と、`ApplyIneffective` を出し、一回性の `reconcile` は終了コード 4（[exit-codes](../exit-codes.md)）。
同じ Diff で 3 回（既定）効かなければ、常駐はそのノードへの Reset を止めて `oscillating` として status・/metrics・通知に出す
（設計 §11.1 の「v1 の実装」）。settle と上限は intent の `apply_check:` か旗で変える：

```yaml
apply_check:
  - provider: redfish-system
    settle: 60s                    # 遅い BMC
    max_ineffective_applies: 2
```

止めたノードは、電源が宣言どおりになるか・宣言を変えるか・`sakutei apply-check release --state <file> --audit-log <file>
--approver <id> redfish-system//<node>`（常駐は `--apply-check-state <file>` を渡しておく）で、また Reset を送る。

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

## FW の更新（redfish-firmware、#148）

UpdateService の `FirmwareInventory` の部品ごとの `Version` を観測し、宣言した版と違えば**多相 Task**（§6.2）で更新する。FW の更新は中断できずリブートをまたぐので、収束ループは Apply せず（`update_policy.strategy: task` が必須）、Task を起こすだけ。更新は**破壊的**として予算（§13、`--max-destructive-per-cycle` など）に数え、Task は最初に**承認ゲート**で止まる。

```json
{
  "provider": "redfish-firmware",
  "id": "node1/firmware/BMC",
  "update_policy": {"strategy": "task", "window": "Sat 02:00-06:00"},
  "fields": {
    "name": "node1",
    "version": "2.0.0",
    "image_uri": "https://images.example/bmc-2.0.0.bin",
    "sha256": "<イメージの SHA-256、16 進 64 文字>",
    "apply_time": "OnReset",
    "reboot": "graceful",
    "image_username": "fw",
    "image_password": "secretref:file/fw/images"
  }
}
```

部品の Id は BMC の `/redfish/v1/UpdateService/FirmwareInventory` のメンバーの `Id`（`BMC`・`BIOS` など、BMC ごとに違う）。ID を `<ノード名>/<Id>` にしないのは、Local ID が intent 全体で一意なので `node1/bios` などと重なりうるため。

### フィールド

| フィールド | 値 | 意味 |
| --- | --- | --- |
| `name` | ノード名 | ID のノード名と同じ（必須） |
| `component` | 部品の Id | 書くなら ID の部品と同じ |
| `version` | 文字列 | 望む版（必須）。**前後の空白を除いた文字列の完全一致**で比べる（大文字と小文字も区別する。`v` の有無や数としての大小は見ない）。違えば上げるか下げるかを問わず更新する |
| `image_uri` | http / https の URI | イメージの在りか（必須）。`transfer: multipart` なら `file:///…` も。資格を URI に書くとエラー |
| `sha256` | 16 進 64 文字 | 書けば、流す前に Sakutei がイメージを取りに行って確かめる |
| `transfer` | `simple_update`（既定）/ `multipart` | `simple_update` は BMC が `image_uri` を取りに行く（`#UpdateService.SimpleUpdate`）。`multipart` は Sakutei が取りに行って確かめた中身を `MultipartHttpPushUri` に送る（BMC がその口を持つときだけ） |
| `apply_time` | `Immediate`（既定）/ `OnReset` | `@Redfish.OperationApplyTime`。BMC が `@Redfish.OperationApplyTimeSupport` を報告していればその中だけ |
| `reboot` | `none`（既定）/ `graceful` / `force` | 書き終えたあとホストを再起動するか（GracefulRestart / ForceRestart、切れていれば On）。`none` でも、誰かが再起動して版が合えば閉じる |
| `update_timeout` | 時間（既定 `1h`） | BMC の Task の完了を待つ上限 |
| `reboot_timeout` | 時間（既定 `30m`） | 再起動して版が合うのを待つ上限 |
| `image_username`・`image_password` | 文字列・**SecretRef** | `image_uri` を取りに行く資格（BMC の SimpleUpdate の `Username`・`Password` と、Sakutei が取りに行くときの Basic 認証）。`image_password` は BMC の一覧の `secret_store` から使う直前に解決し、ログ・エラー・Task の記録・監査に出さない |

版以外のフィールドは「どう合わせるか」で BMC には無いので、差分には出ない（`plan` が出すのは版の違いだけ）。`update_policy.rollback_on_fail` は書けない（FW は自動では戻さない。戻すなら前の版を宣言する）。

### 相と承認

| 相 | すること | 入口で「もう済んでいる」と見るとき |
| --- | --- | --- |
| `verifying` | **承認を待つ**（下）。承認の後、`sha256` を書いていればイメージを取りに行って確かめる | 版が宣言どおりのときだけ（BMC の Task では済みにしない＝承認を飛ばさない） |
| `staging` | SimpleUpdate を送る（`multipart` なら中身を確かめて送る） | この更新の BMC の Task があって失敗で終わっていない（もう送った。二重に送らない） |
| `applying` | BMC の Task の完了を待つ（**危険区間**：この中で落ちた Task は人手ゲート、#52） | その Task が `Completed`（`OnReset` なら `Pending` も） |
| `rebooting` | `reboot` を宣言していれば再起動し、版が合うまで待つ（再起動を宣言したときは**危険区間**：Reset を送る最中に落ちた Task は人手ゲート。送った後の待ちの間に落ちた Task は、引き継いだデーモンが待つだけで二度目の Reset を送らない） | 版が宣言どおり |
| `confirming` | 版を確かめる | 版が宣言どおり |

どの相も、版が宣言どおりなら済んでいる（Task は次の周の観測で閉じる）。BMC に届かない間（BMC 自身の更新で再起動している）は unknown で、相のタイムアウトまで待つ。「この更新の BMC の Task」は TaskService の Task のうち、Payload の `TargetUri` が SimpleUpdate の target（`multipart` なら `MultipartHttpPushUri`）で、`JsonBody` の `Targets` にこの部品があり、SimpleUpdate なら `ImageURI` が宣言と同じもの（Payload を返さない BMC では Update の `TargetDetermined` のメッセージで見分ける。下の「BMC の Task の見分け方」）。Task の URI は持たず、毎回見つけ直す。前の更新の残りかすを数えないよう、BMC の Task の `StartTime` を時刻として読み、この Task の起動の時刻から 5 分（BMC とこのホストの時計のずれの許し）を引いた時刻より前に始まったものは数えない（`StartTime` を読めない Task は数える）。

承認ゲートで止まった Task は `sakutei task list` に `awaiting_approval:verifying` と出て、デーモンは周ごとに RequiresTask（warning）で知らせる。

```console
$ sakutei task approve --store tasks.json --audit-log audit.log --approver alice redfish-firmware//node1/firmware/BMC
$ sakutei task reject  --store tasks.json --audit-log audit.log --approver alice redfish-firmware//node1/firmware/BMC   # halt
```

承認・却下は S4 監査に残る。承認が無いまま既定 24h（`--task-gate-timeout`）を過ぎれば halt。待つ間に別の道具で版が合えば、承認なしで閉じる。BMC の Task が `Exception` などで終われば halt で、送り直さない（BMC を確かめてから `sakutei task cancel`）。

### 計画の時点で断るもの

更新できない部品（`Updateable: false`）・FirmwareInventory に無い部品・UpdateService の無い BMC・TaskService の無い BMC（更新を終わりまで追えない）・`MultipartHttpPushUri` の無い BMC への `multipart`・BMC が選べない `apply_time`。

### 限界

- BMC の時計がこのホストより 5 分を超えて遅れていると、自分の送った Task を前のものと見て数えない。送った直後に見えなければ halt するので送り直しは重ねないが、送ってから次の相を記録する前にデーモンが落ちると、引き継いだデーモンがもう一度送る。BMC の時計を合わせておく。
- `StartTime` を読めない BMC では前の更新の残りかすを数えてしまい、`staging`・`applying` を飛ばして `reboot_timeout` で halt する（承認は飛ばさない）。
- Task の Payload も Update の `TargetDetermined` のメッセージも返さない BMC では、送ったことを観測できないので `staging` の後で halt する（下の「BMC の Task の見分け方」）。
- `sha256` の確認は Sakutei が取りに行った中身についてで、SimpleUpdate で BMC が取りに行く中身と同じとは保証しない（同じにしたいなら `multipart`）。
- 承認は Task（1 台の 1 部品）ごとの単一承認。

### qemu-bmc で分かったこと（0.11.0、FW の更新）

- サービスルートに `UpdateService` と `Tasks` のリンクがある。`FirmwareInventory` は `BMC` と `BIOS`（`Updateable: true`）。初めの版は `BMC_FIRMWARE_VERSION`（既定は qemu-bmc の版、`v0.11.0`）・`BIOS_FIRMWARE_VERSION`（既定 `QBMC-BIOS-1.0.0`）。`Managers/1` の `FirmwareVersion`・`Systems/1` の `BiosVersion` は部品の版と同じ値を出す（0.11 から `FirmwareVersion` は qemu-bmc の版ではなく BMC の部品の版）。
- イメージは 1 行目が `QBMC-FW <BMC|BIOS> <version>` のファイル（その後は何でもよい）。形が違う・`Targets` と合わないイメージは 400（`Update.1.0.VerificationFailed`）で、Task を作らない。SimpleUpdate は `ImageURI` と `Targets`（`TransferProtocol` は省ける）、`UpdateService/upload` は multipart の `UpdateParameters`・`UpdateFile`。どちらも 202 と `Location`（TaskMonitor）。`@Redfish.OperationApplyTimeSupport` は報告しない（`apply_time` は既定の `Immediate` だけ使える）。
- Task は `New` → `Running` → `Completed` / `Exception`（`PercentComplete` は `FIRMWARE_UPDATE_SECONDS`、既定 10 秒で 0 → 100）。**`Payload` を返さない**。Messages は MessageId・MessageArgs つき（`Update.1.0.TargetDetermined`（部品、イメージ）・`InstallingOnComponent`・`UpdateSuccessful`・`AwaitToActivate`・`ApplyFailed`）。multipart の Task のイメージはファイルの名前。
- BMC の更新はすぐ効く（BMC は再起動しない）。BIOS の更新は次の起動（電源 On・`ForceRestart`・`PowerCycle`・`GracefulRestart`）まで保留で、その間 BIOS の部品は `StandbyOffline`、Task は `Completed` で最後のメッセージが `Update.1.0.AwaitToActivate`。
- 障害注入（`FAULT_API_ADDR` に `POST /faults`）: `{"kind":"firmware-image-rejected","target":"BIOS"}` は届いたイメージを壊れたものとして 400 で断る。`{"kind":"firmware-update-fail","target":"BMC","atPercent":30}` は Task を 30% で `Exception`（`Update.1.0.ApplyFailed`）にし、版を変えない。どちらも `DELETE /faults` まで続く。

### 再起動を宣言しない BIOS の更新

BIOS のように**起動したときに効く**部品は、BMC の Task が `Completed` で終わっても版が変わらない（qemu-bmc 0.11.0 では、部品の `Status.State` が `StandbyOffline`、Task の最後のメッセージが `Update.1.0.AwaitToActivate`）。

- `reboot: none`（既定）では Sakutei は再起動を送らない。`rebooting` の相で版が合うのを `reboot_timeout` まで待ち、合わなければ `step rebooting: the step did not complete (pending) within …` で **halt** する。デーモンは RequiresTask（warning、続いた周数が `--stuck-after` に達すれば critical）で知らせ、通知の口（`--event-webhook`・`--event-alertmanager`）にも出る。待つ間に誰かが再起動すれば、版が合って閉じる。
- 再起動を任せるなら、人が BMC を確かめてから `sakutei task cancel` し、`reboot: graceful` か `force` を宣言し直す。新しい Task も承認ゲートで止まる。承認すると、BMC の Task はもう済んでいる（起動の時刻から 5 分の許しの中）ので**更新を送り直さず**、再起動だけを送って、起動したあと版が合えば閉じる。

### BMC の Task の見分け方（Payload を返さない BMC）

「この更新の BMC の Task」は、Task の `Payload` があればそれで（上の「相と承認」）、**無ければ Task の `Messages` の DMTF の Update の登録簿のメッセージ `TargetDetermined`**（`MessageArgs` が「部品の URI、イメージ」、登録簿の版は問わない）で見分ける。部品が同じで、イメージが SimpleUpdate なら `image_uri`、`multipart` なら送ったファイルの名前（`image_uri` の最後の要素）のもの。qemu-bmc 0.11.0 は Payload を返さず、こちらで見分ける（#168）。どちらも無い BMC では送ったことを観測できないので `staging` の後で halt する。BMC の Task が失敗で終われば、halt の理由にメッセージの文と MessageId（`Update.1.0.ApplyFailed` など）を出す。

### 確かめ方

- 偽 BMC（`internal/redfish/redfishtest`）は DMTF の Redfish の仕様に沿って UpdateService・FirmwareInventory・SimpleUpdate・multipart の送信・TaskService を持ち、qemu-bmc に寄せて Task の Messages に MessageId・MessageArgs を持つ。`UpdateOptions.NoTaskPayload` で Payload を返さない形、`SetImageRejected` でイメージを断る（400 `Update.1.0.VerificationFailed`、Task を作らない）形になる。契約テストと単体は `internal/redfish/system/firmware_test.go`（`[AC-148-1]`〜`[AC-148-6]`・`[AC-148-12]`、Payload の無い形と故障は `TestFirmware_TaskWithoutPayload`・`TestFirmware_ImageRejectedAndApplyFailed`）、常駐ループの受け入れテストは `tests/acceptance/redfish_firmware_test.go`（`[AC-148-7]`〜`[AC-148-11]`・`[AC-148-13]`・`[AC-148-14]`）。
- 本物（qemu-bmc 0.11.0 の UpdateService の模擬）は `make integration-redfish` の 10〜13 段目（`[AC-168-10]`〜`[AC-168-13]`、下の「統合テスト」）。

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

設計 §26.1 の 3 段目（§29.1 の（7））。本物の Redfish（[qemu-bmc](https://github.com/tjst-t/qemu-bmc) **0.11.0**、イメージ `ghcr.io/tjst-t/qemu-bmc:0.11.0@sha256:3ab6ea901b1ae8a510a2a140113fd1d80ad0f3ad2c26327ab04ef94f95bbdfc9` に tag と digest で固定。#168 で 0.10.0 から上げた）を containerlab で 2 台立て、sakutei の CLI と常駐ループを向ける。KVM・docker・containerlab と sudo が要るので、`make check`・`make test`・CI には入れない（テストは build tag `integration_redfish` の付いたファイルにあり、`go test ./...` では空のパッケージになる）。

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
3. (0)〜(13) を順に回す（前の段の現実を次の段が使うので、1 つ落ちたら残りは回さない）。10〜13 段目は、FW のイメージを配る HTTP（管理ネットのゲートウェイ、既定 `172.31.84.1` の空いたポート。BMC が取りに来る）と、通知を受ける webhook（`127.0.0.1`、常駐の `--event-webhook`）をテストの中に立てる。
4. **終わったら必ず `containerlab destroy --cleanup`**（失敗しても・Ctrl-C で止めても）。テストのプロセスごと落ちた（`-timeout` など）ときは、Makefile が lab の残りを見つけて消す。

トポロジの要点: 2 台とも `/dev/kvm`・`/dev/net/tun` を渡し、`SYSTEM_UUID`・`VM_MACS` を固定、VM の NIC は eth1（2 台を直結）、`GRACEFUL_TIMEOUT` は 10 秒、FW の更新の Task は `FIRMWARE_UPDATE_SECONDS` 3 秒。node1 だけ初めの FW の版を `BMC_FIRMWARE_VERSION: "1.0.0"`・`BIOS_FIRMWARE_VERSION: "BIOS-1.0.0"` にし、node2 は既定のまま（BMC は `v0.11.0`）。障害注入の口（`FAULT_API_ADDR: ":8623"`）は node2 だけ。**`VM_DISK_SIZE` は付けない**——ディスクの無い VM は BIOS のまま ACPI の電源ボタンに応えないので、GracefulShutdown は PoweringOff のあと `GRACEFUL_TIMEOUT` 秒で On に戻る。これを既定停止の stuck と graceful_then_force の確かめに使う。`POWER_ON_AT_START` の既定（false）で VM は Off から始まる。

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
| (0) | lab が想定どおり | Managers/1 の FirmwareVersion が node2（既定のまま）で `v0.11.0`、node1 で `BMC_FIRMWARE_VERSION` の `1.0.0`。0.9 以降、サービスルート（`/redfish/v1`）は認証なしで 200、`Systems/1` は認証なしで 401。UUID は `SYSTEM_UUID` のとおり、最初は Off。 |
| (1) `[AC-84-1]` | plan → reconcile で宣言に収束 | node1 = On・Continuous の Pxe・CD に `sakutei-a.iso`、node2 = On・Disabled・CD は空。plan は差分を出して BMC に書かない。reconcile のあと Redfish の GET が宣言どおりで、QEMU が `accel=kvm` で動いている（`docker top`）。続く plan は `Converged (no changes)`。image を `sakutei-b.iso` に変えると同じ Apply の中で Eject → Insert して収束。 |
| (2) `[AC-84-2]` | 手で変えたものを次の周で戻す | 常駐の sakutei（`--interval 2s`）を回したまま、curl 相当の要求で node2 を ForceOff、node1 の起動先の上書きを Disabled、node1 の CD を Eject。次の周で 3 つとも cause B の Drift として出て、On・Continuous/Pxe・`sakutei-b.iso` に戻る。 |
| (3) `[AC-84-3]` | 既定の停止（graceful）は ForceOff しない | node1 を `power: "Off"`（power_off は既定）にして常駐（`--stuck-after 3`）。電源は On → PoweringOff → On を繰り返し、一度も Off にならない。BMC のログに「guest did not power off」が 2 回以上（GracefulShutdown を送り直している）。Drift が `class: stuck`・`severity: critical` に上がる（`--event-log`）。plan はまだ Off を求める。 |
| (4) `[AC-84-4]` | graceful_then_force は force_after 後に ForceOff | `power_off: graceful_then_force`・`force_after: 10s`。電源の移り変わりは On → PoweringOff →（BMC があきらめて）On → **Off**（PoweringOff を経ずに Off = ForceOff）。Off になったのは起動から force_after 以上あと。予算で止まらない（BudgetHalted なし）。続く plan は Converged。 |
| (5) `[AC-84-5]` | BMC を止めると Unknown で何も操作しない | node2 を手で ForceOff・node1 の起動先を手で外してから、node2 のコンテナを `docker pause`。plan は node2 の観測が unreachable でエラー。常駐は node2 を毎周「unobservable」として飛ばし（3 周以上）、node1 は収束を続ける（起動先が戻る）。node2 には Apply を出さない。unpause して 5 秒待っても node2 は Off のまま（止まっている間に送った要求が受信待ちに溜まっていれば、ここで実行されてしまう）。reconcile で On に戻る。 |
| (6) `[AC-84-6]` | Once は計画の時点でエラー | node2 を `boot_enabled: Once` にすると、plan も reconcile も `boot_enabled "Once" cannot be declared` で失敗し、node2 は変わらない。 |
| (7) `[AC-87-7]` | OS 導入の Workflow | CD を差して差さったことを確かめてから電源を入れる（[OS を入れる](#os-を入れるworkflow-と組み合わせる)）。 |
| (9) `[AC-166-9]` | Apply が効かない BMC（#166） | node2 だけ qemu-bmc の障害注入の口（`FAULT_API_ADDR: ":8623"`、管理ネットに直に届く）を開け、node2 を手で ForceOff してから `power-action-ignored` を足す。`power: "On"` の reconcile（`--settle 5s --apply-check-state … --max-ineffective-applies 2`）は 5 秒読み直して終了コード 4・`ApplyIneffective`・「Reconcile: not converged」。2 回目で上限に達し、3 回目は Reset を送らずに 4（`Apply stopped (oscillating`）、電源は Off のまま。障害を外して `sakutei apply-check release` で解くと、reconcile が電源を入れて 0。 |
| (8) `[AC-140-8]` | BIOS の設定（redfish-bios） | node1 のシステムと `node1/bios`（`ProcVirtualization: Disabled`・`PowerProfile: Balanced`・`AssetTag`）を並べて宣言。plan は差分を出して `Bios/Settings` に書かない。reconcile は Settings に保留中として書き、今の値は変わらず電源も On のまま。2 回目の reconcile は converging で、qemu-bmc のログの「pending until the next boot」は 1 回だけ（PATCH を重ねない）。node2 に読み取り専用（`BootMode`）・選べない値・登録簿に無い名前を宣言すると plan がエラーで、保留中は空のまま。`ForceRestart` で効いたあと plan は Converged で、宣言していない属性は元のまま。 |
| (10) `[AC-168-10]` | FW の版の観測（redfish-firmware、#168） | node1・node2 の BMC・BIOS の部品を観測した版のまま宣言すると plan は Converged。node1 の BMC を `2.0.0`、node2 の BIOS を `QBMC-BIOS-2.0.0` にすると、plan はその 2 つだけ `"1.0.0" → "2.0.0"` の形で版の差分を出し、BMC の Task も増えず、イメージも取りに来ない。 |
| (11) `[AC-168-11]` | BMC の即時の更新 | 常駐（`--task-store`・`--event-webhook`）で node1 の BMC を `2.0.0`（SimpleUpdate）にすると、Task は承認ゲート（`awaiting_approval:verifying`）で止まり、3 周回しても BMC の Task は増えず、イメージも取りに来ない。webhook に RequiresTask（awaiting approval）。`sakutei task approve` のあと BMC の Task が 1 つ増えて Running → Completed（`TargetDetermined` の MessageArgs が部品とイメージの URI、`UpdateSuccessful`）、イメージを取りに来たのは 1 回、部品と Managers/1 の版が `2.0.0` になって Task が閉じる。続けて `2.1.0` を `transfer: multipart` で宣言すると、同じく承認の後に 1 回だけ送り、BMC の Task のイメージは送ったファイルの名前（`bmc-2.1.0.bin`）で、版が `2.1.0` になる。plan は Converged。 |
| (12) `[AC-168-12]` | BIOS の保留と再起動 | node1 の BIOS を `BIOS-2.0.0`（`reboot` を書かず、`reboot_timeout: 20s`）にして承認すると、BMC の Task は Completed で最後のメッセージが `Update.1.0.AwaitToActivate`、BIOS の部品は `StandbyOffline` で版は `BIOS-1.0.0` のまま。Sakutei は再起動を送らず（電源はずっと On）、Task は `rebooting` の相で halt し、webhook に RequiresTask（rebooting の理由つき）。`sakutei task cancel` して `reboot: force` を宣言し直して承認すると、更新は送り直さず（BMC の Task もイメージの取得も 1 回のまま）、再起動のあと BIOS が `BIOS-2.0.0`・`Enabled`、Systems/1 の `BiosVersion` も同じ。plan は Converged。 |
| (13) `[AC-168-13]` | 故障 | node2 に `firmware-image-rejected`（BIOS）と `firmware-update-fail`（BMC、`atPercent: 30`）を足し、BIOS と BMC の版を宣言して両方承認する。BIOS は `staging` で halt（理由は BMC の 400 の文「Verification of image … failed」）、BMC の Task は作られない。BMC は BMC の Task が 30% で `Exception`（`Update.1.0.ApplyFailed`）になり、`applying` で halt（理由に `Update.1.0.ApplyFailed`）。どちらも版は変わらず、webhook に理由つきの RequiresTask。さらに 3 周回しても二度目は送らない（BMC の Task は 1 つ、イメージの取得は 1 回のまま）、Task は halt のまま（人が確かめて cancel する）。 |

### 結果（2026-10-28、qemu-bmc 0.11.0、FW の更新を足して、#168）

qemu-bmc を 0.11.0（上の digest）に上げ、(10)〜(13) を足して `make integration-redfish` が合格した（docker 29.1.3・containerlab 0.81.1）。(0) の版の確かめを 0.11 の FirmwareVersion に合わせた（node1 は `BMC_FIRMWARE_VERSION`、node2 は既定の `v0.11.0`）ほかは、(1)〜(9) を変えていない。
本物で分かった違いは、BMC の Task が `Payload` を返さないこと。provider に Update の `TargetDetermined` で見分ける口を足し、偽 BMC にも同じ形（`NoTaskPayload`）を足した（上の「BMC の Task の見分け方」）。

```
--- PASS: TestRedfishIntegration (191.99s)
    --- PASS: TestRedfishIntegration/0_lab (0.00s)
    --- PASS: TestRedfishIntegration/1_plan_reconcile (0.34s)
    --- PASS: TestRedfishIntegration/2_daemon_restores_drift (2.54s)
    --- PASS: TestRedfishIntegration/3_graceful_stuck (31.59s)
    --- PASS: TestRedfishIntegration/4_graceful_then_force (11.03s)
    --- PASS: TestRedfishIntegration/5_bmc_paused_unknown (30.78s)
    --- PASS: TestRedfishIntegration/6_once_rejected (0.01s)
    --- PASS: TestRedfishIntegration/7_workflow_cd_before_power (9.06s)
    --- PASS: TestRedfishIntegration/8_bios_settings (0.11s)
    --- PASS: TestRedfishIntegration/9_power_action_ignored (10.18s)
    --- PASS: TestRedfishIntegration/10_firmware_observe (0.05s)
    --- PASS: TestRedfishIntegration/11_firmware_bmc_update (35.61s)
    --- PASS: TestRedfishIntegration/12_firmware_bios_pending_reboot (41.13s)
    --- PASS: TestRedfishIntegration/13_firmware_faults (19.56s)
ok  	github.com/tjst-t/sakutei/v2/tests/integration/redfish	196.361s
```

止まったときの理由（ログから）:

```
BIOS（reboot なし）: step rebooting: the step did not complete (pending) within 20s (unknown ≠ absent: waited, never treated as gone)
BIOS（イメージを断る）: step staging: redfish-firmware "node2/firmware/BIOS": staging: SimpleUpdate: redfish: POST /redfish/v1/UpdateService/Actions/UpdateService.SimpleUpdate: request rejected (HTTP 400): Verification of image 'http://172.31.84.1:…/n2-bios-9.0.0.bin' at '/redfish/v1/UpdateService/FirmwareInventory/BIOS' failed. invalid firmware image: rejected (injected fault); …
BMC（更新が落ちる）: step applying: redfish-firmware "node2/firmware/BMC": applying: the BMC task /redfish/v1/TaskService/Tasks/1 ended Exception (… ; Installation of image '…/n2-bmc-9.0.0.bin' to '/redfish/v1/UpdateService/FirmwareInventory/BMC' failed. (Update.1.0.ApplyFailed))
```

同じ機械の別の作業場所で同じ名前の lab（`sakutei-rf84`）を同時に立てると、互いに deploy・destroy し合って落ちる（1 回目の 3 段目がそれで落ちた。BMC が 2 台とも同時に届かなくなる）。統合テストは同時に 1 つだけ回す。

### 結果（2026-10-28、qemu-bmc 0.10.0、Apply の確かめを足して、#166）

(9) を足し、Apply のあとの確かめ（redfish-system の settle 15 秒）を入れた状態で `make integration-redfish` が合格した。
(1)〜(8) は変えていない（電源の Reset のあと PoweringOn・PoweringOff の間は Converging で、確かめをそこで終える）。

```
--- PASS: TestRedfishIntegration (95.24s)
    --- PASS: TestRedfishIntegration/0_lab (0.00s)
    --- PASS: TestRedfishIntegration/1_plan_reconcile (0.36s)
    --- PASS: TestRedfishIntegration/2_daemon_restores_drift (2.56s)
    --- PASS: TestRedfishIntegration/3_graceful_stuck (31.10s)
    --- PASS: TestRedfishIntegration/4_graceful_then_force (11.03s)
    --- PASS: TestRedfishIntegration/5_bmc_paused_unknown (30.82s)
    --- PASS: TestRedfishIntegration/6_once_rejected (0.01s)
    --- PASS: TestRedfishIntegration/7_workflow_cd_before_power (9.07s)
    --- PASS: TestRedfishIntegration/8_bios_settings (0.12s)
    --- PASS: TestRedfishIntegration/9_power_action_ignored (10.18s)
ok  	github.com/tjst-t/sakutei/v2/tests/integration/redfish	99.534s
```

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
