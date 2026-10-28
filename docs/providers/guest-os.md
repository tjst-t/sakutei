# ゲスト OS を観測する provider（`guest-os`）

ゲスト OS の中を ssh 越しに**観測するだけ**の provider（Backlog #86）。ホストに届くか・OS の識別（`/etc/os-release`）・導入の目印（利用者が決めるファイルの中身）を読む。
使い道は Workflow の `wait`・`require`（`resource: guest-os/{target}`、#87）で、OS 導入の完了を「BMC から見て電源が切れた」ではなく「**ゲスト OS が目印つきで起動した**」で判断する。
決まりの正は設計書 §29.1「ゲスト OS を観測する口と、OS 導入の完了の確かめ方」。ここは使い方の説明で、矛盾したら spec/ が優先する。

- ssh 越しの実行の層（[docs/remote.md](../remote.md)、#130）の上で動く。ホストに agent は置かない。要るのは sshd と `sh`・`dd`・`od`（busybox でもよい）だけ。
- 観測だけ。Apply は常に断り、intent の `resources` に書くと読み込みの時点で断る（その周は何も Observe・Apply しない）。ゲスト OS の中を変えるのは provisioner（責務の外、§29.3 (c)）か別の provider（nftables・dnszone など）。
- List・BulkObserve は持たない（prune とは関係しない）。

## 設定

宛先と鍵は #130 の対象の一覧（`remote_config`）と同じ形で書き、`--provider-config` で観測するホストを名指しする。

```yaml
# hosts.yaml（対象の一覧。書き方は docs/remote.md）
secret_store: secrets.json
known_hosts: known_hosts
hosts:
  node1:
    address: 192.0.2.21
    user: root
    private_key: secretref:file/ssh/sakutei
    connect_timeout: 5s
```

```yaml
# providers.yaml
providers:
  guest-os:
    remote_config: hosts.yaml
    guest_hosts: node1,node2                      # 観測するホスト（intent の providers.guest-os.hosts に書かなければ必須）
    guest_markers: installed=/etc/sakutei-installed,cloud-init=/var/lib/cloud/instance/boot-finished
```

- `remote_config` の相対パスは sakutei を動かすディレクトリから（`--provider-config` の値はそのまま provider に渡る）。
- `guest_markers` は `<名前>=<絶対パス>` をカンマで並べる（名前は英小文字・数字・`_`・`-`）。無ければ目印なし。
- ホストの名前を Workflow の対象（例 `redfish-system` のノード名）と同じにしておくと、手順に `resource: guest-os/{target}` と書ける。
- ホスト鍵は known_hosts で確かめる（#130 と同じ）。入れ直すたびにホスト鍵が変わる OS では、導入のときに決めたホスト鍵を入れておけば known_hosts を前もって書ける（下の統合テストはそうしている）。

## 範囲を intent に書く（`providers:`、Backlog #182）

観測するホスト（`guest_hosts`）と導入の目印（`guest_markers`）は「何を観測するか」なので、intent のトップレベルの `providers:` に
書ける（§1.7・§7.7）。`remote_config` はつなぎ方として provider の設定に残る。

```yaml
# intent/providers.yaml
providers:
  guest-os:
    hosts: [node1, node2]                         # guest_hosts と同じ（順は問わない）
    markers:                                      # guest_markers と同じ（名前 → 絶対パス）
      installed: /etc/sakutei-installed
      cloud-init: /var/lib/cloud/instance/boot-finished
```

- 目印は名前ごとに突き合わせる。intent にだけある目印は設定の目印に足される。
- intent にだけ書けば、設定に書いたのと同じに動く。設定にだけあれば今までどおり読み、`plan`・`reconcile`・`status`・常駐が
  「is only in the provider config: write it in the intent too (providers.guest-os.…)」と警告する（常駐では一度だけ）。
- 両方にあれば設定は安全の網として残り、**食い違えば読み込みで断る**（弱い側に寄せない。常駐は起動で断り、動いているあいだに
  食い違えばその周を飛ばす）。知らないキーも断る。設定から外すのは次のメジャー版。
- どれがつなぎ方でどれが範囲かの一覧は [ranges.md](ranges.md)。

## ID とフィールド

ID はホストの名前（`node1`）。

| フィールド | 中身 |
| --- | --- |
| `host` | ホストの名前（= ID、identity） |
| `os_id` | os-release の `ID`（例 `alpine`・`debian`・`rhel`） |
| `os_version_id` | os-release の `VERSION_ID`（例 `3.24.3`） |
| `os_pretty_name` | os-release の `PRETTY_NAME` |
| `hostname` | `/proc/sys/kernel/hostname` |
| `boot_id` | `/proc/sys/kernel/random/boot_id`（起動ごとに変わる） |
| `marker.<名前>` | 目印のファイルの中身（末尾の空白・改行を落とす。4 KiB まで） |

- os-release は `/etc/os-release`、無ければ `/usr/lib/os-release`。どちらも無ければ `os_*` は出ない。
- 目印のファイルが無いのは観測の事実で、そのフィールドが出ないだけ（待ちの値とは一致しない）。
- 値を出すコマンドは目印にできない（観測で任意のコマンドを走らせない）。cloud-init のように「終わったらファイルを置く」仕組みはそのまま目印にできる。

## 観測できないとき

| 起きたこと | 答え |
| --- | --- |
| ホストに届かない（電源が切れている・まだ起動していない・sshd が上がっていない） | 観測できない（unknown、TransientError）。absent にしない |
| 認証の失敗 | 観測できない（unknown、TransientError） |
| ホスト鍵の不一致 | 観測できない（unknown、PermanentError） |
| 目印がディレクトリ・読む権限が無い・4 KiB より大きい | 観測できない（unknown） |
| 出力が途中で切れた・コマンドが失敗した | 観測できない（unknown） |

「まだ起動していない」と「壊れた（ディスクに何も入っていない・起動しない）」は観測からは見分けられない。**見分けるのは Workflow の待ちの `timeout`**で、過ぎれば halt する（§3：確信が持てないとき、進めない）。

## OS 導入の Workflow

[docs/trial/redfish-os-install](../trial/redfish-os-install/README.md) の Workflow の、電源オフを待ってディスクから起動したあとに、ゲスト OS の確かめを足す。

```yaml
steps:
  # …CD を差す → 確かめる → 次の 1 回だけ CD から起動（action boot_once）→ 再起動（action reset）
  - wait:                              # インストーラが電源を切るのを待つ
      fields:
        power: "Off"
      timeout: 15m
  - apply:                             # 電源を入れる（上書きは使い切ったのでディスクから起動する）
      power: "On"
  - require:                           # ゲスト OS が目印つきで起動したのを確かめる
      resource: guest-os/{target}
      fields:
        os_id: alpine
        marker.installed: install-2026-10-27
        marker.hostname: node1         # guest_markers の hostname=/etc/hostname（応答ファイルのホスト名）
      timeout: 10m
  - apply:                             # ここから先は導入が済んだときだけ
      resource: redfish-virtual-media/{target}/cd
      fields:
        image: ""
```

`wait` でも `require` でも判定は同じ（`require` は timeout を省けない）。
- インストーラが失敗しても電源を切る ISO では、`power: "Off"` の待ちは通ってしまう。ゲスト OS の待ちがあれば、ゲスト OS に届かない（unknown）・目印が無いまま `timeout` で halt し、その先の手順に進まない。
- 目印の値は**導入ごとに変える**（日付・導入の番号など）。前の導入で入った OS が同じファイルを持っていても取り違えない。
- 待っている間（届かない間）は `waiting for map[…] (unobservable: <なぜ観測できないか>; unknown ≠ absent)`（例 `remote node1: unreachable: …`）、届いたが目印が違う・無い間は `waiting for map[…]` と出る。
- 多相 Task（#51）の相ごとの入口の Observe にも、同じ provider をそのまま使える（普通の Read を持つ）。

## 統合テスト

qemu-bmc 0.11.0（`ghcr.io/tjst-t/qemu-bmc:0.11.0@sha256:3ab6ea901b1ae8a510a2a140113fd1d80ad0f3ad2c26327ab04ef94f95bbdfc9`、#168 で 0.10.0 から上げた）を containerlab で 3 台立て、本当に Alpine を入れる統合テスト（Backlog #86、#139 で広げ、#88 で入れ直しの TestReimage を足した）。build tag `integration_guestos` の付いたファイルにあり、`make check`・`make test`・CI には入らない。

```console
$ make integration-guestos
```

要るもの: KVM（`/dev/kvm`）、docker と containerlab（パスワードなしの `sudo -n`）、`ssh`・`ssh-keygen`・`xorriso`・`curl`。初回は Alpine 3.24 の virt ISO とパッケージを取りに行く（`SAKUTEI_IT_CACHE`、既定 `~/.cache/sakutei-it/alpine` に残し、次からは使い回す）。

- ISO は [make-iso.sh](../trial/redfish-os-install/make-iso.sh) で **3 台に共通の 1 つ**（`auto.iso`）を作る。台ごとの値（ホスト名・静的なアドレス・テストの公開鍵・目印・失敗のさせ方）は ISO に焼かず、
  sakutei が宣言から書き出す**応答ファイル**（[template-file](template-file.md)、雛形は [alpine-answers.sh.tmpl](../trial/redfish-os-install/templates/alpine-answers.sh.tmpl)、ID は台の UUID）から取る。
  テストは書き出す先を HTTP で配り（`0.0.0.0` の空いたポート）、インストーラは lab のゲートウェイ（`172.31.86.1`）越しに DMI の UUID で取りに来る。ホスト鍵（秘密）はテストが作って ISO に入れる（known_hosts を前もって書く）。目印の値は回ごとに変える（`it86-<時刻>`）。
  - node1: 入れて、sshd と目印を置いて電源を切る。
  - node2（`var.fail: disk`）: 何も入れずに電源を切る（インストーラが失敗しても電源を切る ISO の真似）。
  - node3（`var.fail: marker`）: OS は入れるが目印を置かない（導入の仕上げが済んでいない）。
- VM の NIC は QEMU の user ネット（slirp）で、ゲストの 22 番をコンテナの 2222 番に出す（`QEMU_EXTRA_ARGS` の `hostfwd`）。ホストの sakutei は BMC と同じアドレスの 2222 番に ssh でつなぐ。
- 常駐の sakutei（`--interval 5s`）がノードごとの Workflow `os-install-<ノード>` を回す: CD を差す → `require` で確かめる → `action: boot_once Cd` → `action: reset`（切れているので電源 On）→ 電源オフを待つ →
  電源 On（上書きは使い切ったのでディスクから）→ `guest-os/{target}` の `os_id: alpine`・`marker.installed: <今回の値>`・`marker.hostname: <ノード名>` を `require`（timeout 3m）→ CD を抜く。
  終わったあとの姿の宣言は電源オンと CD は空だけで、**起動先の上書きは宣言しない**（上書きが残らないのは Workflow と BMC のおかげだと見るため）。
- 終われば（落ちても）lab を消し、ssh の master を止める。Makefile も残った lab を消す。

確かめること:

| | 内容 |
| --- | --- |
| [AC-86-5] node1 | 電源オフのあとディスクから起動し、ゲスト OS の目印・ホスト名と `os_id` を観測して先へ進み（CD を抜く）、done。手で ssh して目印が今回の値であることも見る |
| [AC-86-6] node2 | インストーラが電源を切ったので `power: Off` の待ちは通るが、ゲスト OS に届かない（unobservable）まま待ち、timeout で halt。CD は差さったまま（先へ進まない） |
| [AC-86-7] node3 | ゲスト OS に届く（unobservable でなく目印を待つ）が目印が無いので待ち、timeout で halt。CD は差さったまま。手で ssh して os-release があり目印が無いことも見る |
| [AC-139-7] 3 台 | 応答ファイルが宣言から書き出され（秘密鍵を含まない）、インストーラが取りに来た。boot_once・reset を送ったと出る。Continuous の上書きは一度も無く、終わったあと 3 台とも上書きは Disabled。node1 を手で ssh すると、ホスト名とアドレスが応答ファイルの値 |

### 結果（2026-10-28、qemu-bmc 0.11.0、#168）

qemu-bmc を 0.11.0 に上げ、テストを変えずに回した結果（PASS）。TestReimage も合格:

```
    guestos_integration_test.go:603: all workflows finished in 3m40s
--- PASS: TestGuestOSInstall (232.64s)
    reimage_integration_test.go:256: reimage-node1 finished in 40s
    reimage_integration_test.go:317: second reimage-node1 finished in 45s
--- PASS: TestReimage (140.55s)
ok  	github.com/tjst-t/sakutei/v2/tests/integration/guestos	377.349s
```

### 結果（2026-10-27、#139）

KVM の使える開発機（docker 29.1.3・containerlab 0.81.1・OpenSSH 9.6p1）で、qemu-bmc 0.10.0 × 3・Alpine 3.24.3 で回した結果（PASS）:

```
    guestos_integration_test.go:603: all workflows finished in 3m40s
--- PASS: TestGuestOSInstall (232.63s)
PASS
```

デーモンの出力（node1、抜粋）:

```
cycle 1: WorkflowStepDone: … target node1, step 1 (apply redfish-virtual-media/node1/cd): applied map[image:/iso/auto.iso]
cycle 1: WorkflowStepDone: … target node1, step 2 (require redfish-virtual-media/node1/cd): observed map[image:/iso/auto.iso]
cycle 1: WorkflowStepDone: … target node1, step 3 (action): sent boot_once Cd (boot override Once)
cycle 1: WorkflowStepDone: … target node1, step 4 (action): sent reset GracefulRestart (On if powered off)
cycle 5: WorkflowStepDone: … target node1, step 5 (wait): observed map[power:Off]
cycle 5: WorkflowStepDone: … target node1, step 6 (apply): applied map[power:On]
cycle 6: WorkflowStepDone: … target node1, step 7 (require guest-os/node1): observed map[marker.hostname:node1 marker.installed:it86-1793115913 os_id:alpine]
cycle 6: WorkflowStepDone: … target node1, step 8 (apply redfish-virtual-media/node1/cd): applied map[image:]
cycle 6: WorkflowCompleted: workflow os-install-node1: run … completed; targets return to convergence
cycle 35: WorkflowHalted: workflow os-install-node2: … step 7 (require guest-os/node2): require map[…] timed out after 3m0s (on_failure: halt; …)
cycle 39: WorkflowHalted: workflow os-install-node3: … step 7 (require guest-os/node3): require map[…] timed out after 3m0s (on_failure: halt; …)
```

node1 のシリアル（インストーラが応答ファイルを取り、その値で入れた）:

```
SAKUTEI-AUTOINSTALL: answer file http://172.31.86.1:41595/6f1b7c52-0e1a-4d2b-9c11-000000008601 (hostname node1, mark it86-1793115913)
SAKUTEI-AUTOINSTALL: sshd enabled on /dev/vda2
SAKUTEI-AUTOINSTALL: hostname node1
SAKUTEI-AUTOINSTALL: marked /dev/vda2
SAKUTEI-AUTOINSTALL: done, powering off
SAKUTEI-INSTALLED-OS: booted from disk: it86-1793115913
```

- 応答ファイルは収束ループが cycle 1 に書き、インストーラは CD から起動して約 10 秒で取りに来た（取れるまで 2 分待つが、待たずに取れた）。
- **起動先の上書き**：BMC は boot_once の Once を電源 On（reset）で使い切り、インストーラが電源を切ったあとの電源 On はディスクから起動した。終わったあと 3 台とも Disabled。
- node2 は前と同じく、ディスクに何も無いので差さったままの CD に落ちてインストーラがもう一度走り（応答ファイルも 2 回取った）、また何も入れずに電源を切った。ゲスト OS に届かないまま halt。
- 最初に回したとき、`make-iso.sh` が `SSH_AUTHORIZED_KEYS` を渡したときしかホスト鍵を ISO に入れず（公開鍵を応答ファイルから取る形では渡さない）、入れた OS がホスト鍵を自分で作ったので known_hosts と合わず、ゲスト OS が観測できなかった（unknown のまま timeout で halt）。ホスト鍵を別に入れるよう直した。

### 結果（2026-10-27、#86 のとき）

当時は台ごとに ISO を作り（値は ISO に焼く）、起動先は Continuous で CD にしてディスクから起動するときに Disabled に戻す手順だった（手順の番号も当時のもの）。

2026-10-27、KVM の使える開発機（docker 29.1.3・containerlab 0.81.1・OpenSSH 9.6p1）で、qemu-bmc 0.10.0 × 3・Alpine 3.24.3 で回した結果（PASS）:

```
    guestos_integration_test.go:525: all workflows finished in 3m45s
--- PASS: TestGuestOSInstall (237.46s)
PASS
```

デーモンの出力（抜粋）:

```
cycle 3: WorkflowStepDone: workflow os-install-node2: stage install, target node2, step 4 (wait): observed map[power:Off]
cycle 6: WorkflowStepDone: workflow os-install-node1: stage install, target node1, step 4 (wait): observed map[power:Off]
cycle 6: WorkflowStepDone: workflow os-install-node3: stage install, target node3, step 4 (wait): observed map[power:Off]
cycle 7: WorkflowStepDone: workflow os-install-node1: stage install, target node1, step 6 (wait guest-os/node1): observed map[marker.installed:it86-1793108639 os_id:alpine]
cycle 7: WorkflowStepDone: workflow os-install-node1: stage install, target node1, step 7 (apply redfish-virtual-media/node1/cd): applied map[image:]
cycle 7: WorkflowCompleted: workflow os-install-node1: run os-install-node1-… completed; targets return to convergence
cycle 35: WorkflowHalted: workflow os-install-node2: stage install, target node2, step 6 (wait guest-os/node2): wait map[…] timed out after 3m0s (on_failure: halt; …)
cycle 40: WorkflowHalted: workflow os-install-node3: stage install, target node3, step 6 (wait guest-os/node3): wait map[…] timed out after 3m0s (on_failure: halt; …)
```

- **node1**：インストールは約 20 秒。ディスクから起動した次の周（約 5 秒後）に目印を観測して CD を抜き、done。
- **node2**：3 つとも電源オフの待ち（BMC から見た「終わった」）は通った。node2 はディスクに何も無いので、ディスクから起動させても BIOS が差さったままの CD に落ちてインストーラがもう一度走り、また何も入れずに電源を切った（シリアルに `failing on purpose` が 2 回）。
  その間の待ちの理由は `unobservable: … remote node2: unreachable: Connection timed out during banner exchange`（VM は動いているがゲストの sshd がいない）→ `… ssh: connect to host 172.31.86.12 port 2222: Connection refused`（電源が切れて QEMU がいない）で、3 分の timeout で halt。CD は差さったまま。
- **node3**：起動したあとは観測できる（unobservable が付かない）`waiting for map[…]` が続き、3 分の timeout で halt。CD は差さったまま。手で ssh すると os-release があり目印が無い。
- 最初に回したとき、ssh の master のソケットを長い一時ディレクトリの下に置いて unix ソケットのパスの上限を超え、ゲスト OS がすべて観測できなかった（`control_dir` は短いパスにする。docs/remote.md）。2 回目は `--provider-config` の `remote_config` を相対パスで書き、デーモンを別のディレクトリで動かしたので provider が作れなかった（`provider guest-os is unavailable`）。どちらも待ちの理由が「unobservable」だけでは分からなかったので、Workflow の待ちの理由に観測できない理由を添えるようにした。

### 入れ直しを宣言で起こす（TestReimage、#88）

同じ lab の node1・node2 に、`trigger: reimage` の OS 導入の Workflow（`reimage-<ノード>`、`target.scope` は `rack: r1`）を 1 台 1 つずつ置き、常駐の sakutei（`--audit-log` 付き）を 1 つ回したまま、
`planned_change` の `kind: reimage`（scope `rack: r1`・`max_count: 1`）を書いたり外したりする（[docs/trial/redfish-os-install](../trial/redfish-os-install/README.md) の「入れ直し」）。応答ファイルは 2 台とも成功する中身で、
手順 4 の再起動は `type: ForceRestart`（ディスクを消して入れ直すので正常停止を待たない。入れ直しの 1 回分に含まれ、通常予算に数えない）。`go test -tags integration_guestos -run TestReimage` で単独でも回せる。

| | 内容 |
| --- | --- |
| [AC-88-9] | 宣言が無い間は走らない（run を作らず、`WorkflowStarted` も無い）。2 台は収束ループが電源 On に保つ |
| [AC-88-10] | 承認の無い宣言では走らない（`PlannedChangeUnapproved`。運用プロファイル team は 1 人） |
| [AC-88-11] | 承認された宣言で node1 に 1 回走り、今回の目印の OS が起動して done（手で ssh して目印を見る）。node2 は枠が無いので走らない（インストーラも起動しない）。S4 に `planned_change.executed` が 1 |
| [AC-88-12] | 宣言を外すと走らない（同じ done の run のまま、OS も前の目印のまま） |
| [AC-88-13] | 新しい宣言（別の窓・新しい目印）で、動いている node1 がもう一度入れ直されて新しい目印の OS になる。node2 は走らない。起動先の上書きは Disabled・CD は抜けている |

#### 結果（2026-10-27）

同じ開発機で、qemu-bmc 0.10.0・Alpine 3.24.3（PASS）。1 回目の入れ直しは承認から 40 秒、動いている OS の入れ直しは 45 秒:

```
    reimage_integration_test.go:256: reimage-node1 finished in 40s
    reimage_integration_test.go:317: second reimage-node1 finished in 45s
--- PASS: TestReimage (140.55s)
```

デーモンの出力（抜粋。宣言は `planned_change(scope=rack=r1 kind=reimage window=…)` を `<宣言>` と略す）:

```
cycle 5: PlannedChangeUnapproved: <宣言>: approval has 0 of the 1 approver(s) required by the team profile; the lane stays closed …
cycle 8: WorkflowStarted: workflow reimage-node1: run … started (1 stage(s), 1 target(s)) for <宣言>
cycle 8: WorkflowStepDone: workflow reimage-node1: stage install, target node1: reimage admitted by <宣言>
cycle 8: WorkflowStepDone: … target node1, step 3 (action): sent boot_once Cd (boot override Once) (in the planned_change lane)
cycle 8: WorkflowStepDone: … target node1, step 4 (action): sent reset ForceRestart (On if powered off; destructive) (in the planned_change lane)
cycle 8: WorkflowWaiting: workflow reimage-node2: not starting: <宣言> has no max_count left for scope rack=r1
cycle 8: PlannedChangeOpened: <宣言>: lane opened (profile team; approval 1 of 1 approver(s) [alice] required by the team profile)
cycle 8: PlannedChange: <宣言>: reimage of redfish-system/rack=r1/node1 by workflow reimage-node1 admitted to the lane (used 1 of max_count 1); not counted in the normal budget
cycle 14: WorkflowStepDone: … target node1, step 7 (require guest-os/node1): observed map[marker.hostname:node1 marker.installed:it86-1793117468-r1 os_id:alpine]
cycle 14: WorkflowCompleted: workflow reimage-node1: run … completed; targets return to convergence
  …（宣言を外した cycle 15〜18 は何も起きない）
cycle 19: WorkflowStarted: workflow reimage-node1: run … started (1 stage(s), 1 target(s)) for <新しい宣言>
cycle 25: WorkflowStepDone: … target node1, step 7 (require guest-os/node1): observed map[marker.hostname:node1 marker.installed:it86-1793117468-r2 os_id:alpine]
cycle 25: WorkflowCompleted: workflow reimage-node1: run … completed; targets return to convergence
```

node1 のシリアル（インストーラは宣言ごとに 1 回だけ走り、2 回目は動いている OS を ForceRestart で止めて CD から入れ直した）:

```
SAKUTEI-AUTOINSTALL: start 2026-10-27T16:11:51Z
SAKUTEI-AUTOINSTALL: answer file http://172.31.86.1:42181/6f1b7c52-0e1a-4d2b-9c11-000000008601 (hostname node1, mark it86-1793117468-r1)
SAKUTEI-AUTOINSTALL: done, powering off
SAKUTEI-INSTALLED-OS: booted from disk: it86-1793117468-r1
SAKUTEI-AUTOINSTALL: start 2026-10-27T16:12:54Z
SAKUTEI-AUTOINSTALL: answer file http://172.31.86.1:42181/6f1b7c52-0e1a-4d2b-9c11-000000008601 (hostname node1, mark it86-1793117468-r2)
SAKUTEI-AUTOINSTALL: done, powering off
SAKUTEI-INSTALLED-OS: booted from disk: it86-1793117468-r2
```

- node2 のシリアルに `SAKUTEI-AUTOINSTALL` は一度も出なかった（宣言の `max_count: 1` を node1 が使い、node2 の Workflow は起動しなかった）。
- `not starting` の待ちは、枠の無い宣言が開いている間、毎周出る。
