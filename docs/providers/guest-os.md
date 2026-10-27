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
    guest_hosts: node1,node2                      # 観測するホスト（必須）
    guest_markers: installed=/etc/sakutei-installed,cloud-init=/var/lib/cloud/instance/boot-finished
```

- `remote_config` の相対パスは sakutei を動かすディレクトリから（`--provider-config` の値はそのまま provider に渡る）。
- `guest_markers` は `<名前>=<絶対パス>` をカンマで並べる（名前は英小文字・数字・`_`・`-`）。無ければ目印なし。
- ホストの名前を Workflow の対象（例 `redfish-system` のノード名）と同じにしておくと、手順に `resource: guest-os/{target}` と書ける。
- ホスト鍵は known_hosts で確かめる（#130 と同じ）。入れ直すたびにホスト鍵が変わる OS では、導入のときに決めたホスト鍵を入れておけば known_hosts を前もって書ける（下の統合テストはそうしている）。

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

[docs/trial/redfish-os-install](../trial/redfish-os-install/README.md) の Workflow の、電源オフを待ってディスクから起動したあとに、ゲスト OS の待ちを足す。

```yaml
steps:
  # …CD を差す → 確かめる → CD 起動で電源オン
  - wait:                              # インストーラが電源を切るのを待つ
      fields:
        power: "Off"
      timeout: 15m
  - apply:                             # ディスクから起動する
      boot_enabled: Disabled
      power: "On"
  - wait:                              # ゲスト OS が目印つきで起動したのを待つ
      resource: guest-os/{target}
      fields:
        os_id: alpine
        marker.installed: install-2026-10-27
      timeout: 10m
  - apply:                             # ここから先は導入が済んだときだけ
      resource: redfish-virtual-media/{target}/cd
      fields:
        image: ""
```

- インストーラが失敗しても電源を切る ISO では、`power: "Off"` の待ちは通ってしまう。ゲスト OS の待ちがあれば、ゲスト OS に届かない（unknown）・目印が無いまま `timeout` で halt し、その先の手順に進まない。
- 目印の値は**導入ごとに変える**（日付・導入の番号など）。前の導入で入った OS が同じファイルを持っていても取り違えない。
- 待っている間（届かない間）は `waiting for map[…] (unobservable: <なぜ観測できないか>; unknown ≠ absent)`（例 `remote node1: unreachable: …`）、届いたが目印が違う・無い間は `waiting for map[…]` と出る。
- 多相 Task（#51）の相ごとの入口の Observe にも、同じ provider をそのまま使える（普通の Read を持つ）。

## 統合テスト

qemu-bmc 0.10.0 を containerlab で 3 台立て、本当に Alpine を入れる統合テスト（Backlog #86）。build tag `integration_guestos` の付いたファイルにあり、`make check`・`make test`・CI には入らない。

```console
$ make integration-guestos
```

要るもの: KVM（`/dev/kvm`）、docker と containerlab（パスワードなしの `sudo -n`）、`ssh`・`ssh-keygen`・`xorriso`・`curl`。初回は Alpine 3.24 の virt ISO とパッケージを取りに行く（`SAKUTEI_IT_CACHE`、既定 `~/.cache/sakutei-it/alpine` に残し、次からは使い回す）。

- ISO は [make-iso.sh](../trial/redfish-os-install/make-iso.sh) で 3 つ作る。どれも入れた OS で sshd を動かし（eth0 は DHCP）、テストの鍵で root に入れるようにし、ホスト鍵はテストが作ったもの（known_hosts を前もって書く）。目印 `/etc/sakutei-installed` の値は回ごとに変える（`it86-<時刻>`）。
  - `ok.iso`（node1）: 入れて、sshd と目印を置いて電源を切る。
  - `fail-disk.iso`（node2、`AUTOINSTALL_FAIL=disk`）: 何も入れずに電源を切る（インストーラが失敗しても電源を切る ISO の真似）。
  - `fail-marker.iso`（node3、`AUTOINSTALL_FAIL=marker`）: OS は入れるが目印を置かない（導入の仕上げが済んでいない）。
- VM の NIC は QEMU の user ネット（slirp）で、ゲストの 22 番をコンテナの 2222 番に出す（`QEMU_EXTRA_ARGS` の `hostfwd`）。ホストの sakutei は BMC と同じアドレスの 2222 番に ssh でつなぐ。
- 常駐の sakutei（`--interval 5s`）がノードごとの Workflow `os-install-<ノード>` を回す: CD を差す → `require` で確かめる → CD 起動で電源オン → 電源オフを待つ → ディスクから起動 → `guest-os/{target}` の `os_id: alpine` と `marker.installed: <今回の値>` を待つ（timeout 3m）→ CD を抜く。
- 終われば（落ちても）lab を消し、ssh の master を止める。Makefile も残った lab を消す。

確かめること:

| | 内容 |
| --- | --- |
| [AC-86-5] node1 | 電源オフのあとディスクから起動し、ゲスト OS の目印と `os_id` を観測して先へ進み（CD を抜く）、done。手で ssh して目印が今回の値であることも見る |
| [AC-86-6] node2 | インストーラが電源を切ったので `power: Off` の待ちは通るが、ゲスト OS に届かない（unobservable）まま待ち、timeout で halt。CD は差さったまま（先へ進まない） |
| [AC-86-7] node3 | ゲスト OS に届く（unobservable でなく目印を待つ）が目印が無いので待ち、timeout で halt。CD は差さったまま。手で ssh して os-release があり目印が無いことも見る |

### 結果

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
