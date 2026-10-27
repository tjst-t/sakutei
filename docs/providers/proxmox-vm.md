# Proxmox VE の VM provider（proxmox-vm）

Proxmox VE のクラスタの VM（qemu）を宣言で作り、CPU・メモリ・ディスク・NIC・cloud-init・電源を合わせる provider（Backlog #143、設計書 §29.1「Proxmox VE の VM provider」）。
REST API（`/api2/json`）を Go の標準ライブラリの net/http で叩く。認証は API トークン（`Authorization: PVEAPIToken=<user>@<realm>!<tokenname>=<secret>`）。

- 完全の階級：Read・Apply・List・BulkObserve（`/cluster/resources` を 1 回と VM ごとの設定）。
- VM は破壊的な型なので **prune しない**（`Pruneable: false`、§12.3）。消すのは明示の削除だけで、Delete は破壊的として予算（§13）に数える。
- **触らない VM**（Sakutei 自身が動く VM など段0 のもの、§23）を設定に書け、それらは List に出さず、Read・Apply もしない。宣言すれば計画のエラー。
- コンテナ（LXC）・ストレージ・ネットワーク（ブリッジ）・HA・バックアップの設定は扱わない。ゲスト OS の中は離れたホストの層（[docs/remote.md](../remote.md)）と [Ansible provider](ansible.md) で合わせる。

## クラスタの一覧（proxmox_config）

CLI には専用のフラグを置かず、`--provider-config` の `proxmox-vm` の `proxmox_config` で渡す（§28.9）。

```yaml
# provider-config.yaml
providers:
  proxmox-vm:
    proxmox_config: /etc/sakutei/pve.yaml
```

```yaml
# pve.yaml
secret_store: secrets.json            # file SecretBackend（§20.5）。相対パスはこのファイルから
clusters:
  lab:                                # クラスタの名前（ID は <名前>/<VM 名>）
    endpoint: https://pve1.example.net:8006
    token_id: sakutei@pve!sakutei     # <user>@<realm>!<tokenname>
    token_secret: secretref:file/proxmox/lab/token
    node: pve1                        # 省略可：node を宣言しない VM を作るノード
    scope: site=tokyo                 # 省略可：このクラスタの ID の scope（既定 root）
    protected_vmids:                  # 触らない VM（vmid で）
      - 100
    protected_names:                  # 触らない VM（名前で）
      - sakutei-ctl
    ca_file: pve-root-ca.pem          # 省略可：これだけを信頼する
    insecure_skip_verify: false       # 明示したときだけ true（ca_file とは同時に書けない）
    timeout: 15s                      # 省略可：1 つの要求の時間切れ
    task_wait: 30s                    # 省略可：Apply が非同期の仕事を待つ長さ（0 なら待たない）
```

- `token_secret` は SecretRef でしか書けない（生の文字列はエラー。値は文言にも出さない）。値は要求の直前に解決し（JIT、§20.2）、provider にも持たない。読み込みの時点では store にその path があることだけを確かめる。
- 未知のキーはエラー。YAML はリポジトリのサブセットで、`[a, b]` の流れの書き方は使えない（`- a` の形で書く）。
- トークンに要る権限：VM の作成・複製・設定・電源・削除（`VM.Allocate`・`VM.Clone`・`VM.Config.*`・`VM.PowerMgmt`）、ストレージの領域の割り当て（`Datastore.AllocateSpace`）、`/cluster/resources` と仕事の状態を読むこと（`Sys.Audit`）。テンプレートから複製するなら、そのテンプレートに `VM.Clone`。privsep のあるトークンには、利用者と同じかそれ以下の権限を付ける。

## intent の例

```yaml
version: "1"
pools:
  - name: vmids
    range: "500-599"
resources:
  - provider: proxmox-vm
    id: lab/mon1                      # <クラスタ>/<VM 名>
    fields:
      vmid:
        from_pool: vmids              # 採番から配る（vmid: "501" と書いてもよい）
      template: debian-13-cloud       # 複製する元（名前か vmid）。作るときだけ
      cores: "2"
      memory: 4G                      # MiB の整数（4096）でもよい
      disks: "scsi0 storage=local-lvm size=32G"
      nics: "net0 bridge=vmbr0 vlan=20"
      ci_user: ops
      ci_ssh_keys: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA... sakutei@ctl"
      ci_ip: 192.0.2.21/24
      ci_gateway: 192.0.2.1
      ci_dns: "192.0.2.53 192.0.2.54"
      power: running
```

### フィールド

宣言したものだけを管理し、書かないものは触らない。

| フィールド | 値 | 決まり |
| --- | --- | --- |
| `cluster`・`name` | 識別子 | ID と同じ値（省略できる）。VM は**名前で引く**（vmid を ID にしないのは採番から配れるようにするため） |
| `vmid` | 100〜999999999 | 作るときに使う。宣言するか `from_pool` で配る（どちらも無ければ作るときにエラー）。今ある VM と違えば計画のエラー（番号は変えない）。使われている番号・触らない番号なら作らない |
| `node` | ノード名 | 省略すればクラスタの `node`。今ある VM と違えば計画のエラー（移行は扱わない） |
| `template` | 名前か vmid | 作るときだけ使う full clone の元。観測しない（差分から落とす）。宣言したディスクが全部同じストレージなら、複製の先のストレージにする |
| `cores` | 正の整数 | |
| `memory` | `2048`（MiB）・`2048M`・`2G` | |
| `disks` | `<slot> storage=<storage> size=<大きさ>` を `,` でつなぐ | slot は `scsiN`・`virtioN`・`sataN`・`ideN`。大きさは GiB で割り切れること。**宣言した slot だけ**を比べる。大きくするのはよい（resize）、**小さくするのと、ストレージを変えるのは計画のエラー**。無い slot は足す。宣言から外したディスクは消さない |
| `nics` | `<slot> bridge=<bridge> [vlan=<1-4094>] [mac=<MAC>] [model=<型>]` を `,` でつなぐ | **宣言した slot だけ**を比べる。`bridge` と `vlan`（書かなければタグ無し）はいつも、`mac` と `model`（virtio・e1000・e1000e・rtl8139・vmxnet3）は書いたときだけ比べる。書き換えるときも、宣言しない MAC・型とほかのキー（`firewall=1` など）は残す |
| `ci_user` | ユーザ名 | cloud-init（`ciuser`） |
| `ci_ssh_keys` | 公開鍵（改行で区切る） | `sshkeys`。Proxmox は URL エンコードして持つ |
| `ci_ip` | `dhcp` か CIDR（IPv4） | `ipconfig0` の `ip`。`ip6` などほかのキーは残す |
| `ci_gateway` | IPv4 | `ipconfig0` の `gw` |
| `ci_dns` | アドレスを空白かカンマで | `nameserver`（順は意味がある） |
| `power` | `running`・`stopped` | 停止は正常停止（shutdown）。再起動は宣言できない（下の「再起動」） |
| `power_off` | `graceful`（既定）・`graceful_then_force` | `graceful_then_force` のときだけ、最初の shutdown から `force_after` を過ぎても止まらなければ強制停止（stop）。その Op は**破壊的**（予算に数える） |
| `force_after` | 時間（`5m` など） | `graceful_then_force` のときだけ書け、そのときは必須 |
| `apply_pending` | `never`（既定）・`reboot` | `reboot` のときだけ、宣言したフィールドに再起動を待つ変更があれば VM を再起動する |

cloud-init の値は、空から作る VM（`template` を宣言しない）では cloud-init のドライブ（`ide2`、最初のディスクと同じストレージ）を足して入れる。テンプレートから作る VM ではテンプレートのドライブを使う（無ければ値は設定に入るだけで、ゲストには届かない）。
cloud-init の値はゲストが次に起動したときに読む（cloud-init の決まり。一度起動した VM の値を変えても、ゲストの中が変わるかはイメージの cloud-init の設定による）。

観測だけの値（Computed）：`lock`（Proxmox の lock）、`pending`（再起動を待つフィールドの名前）、`current_values`（それらの今効いている値、JSON）。

### 正規形（永久 drift を出さない）

Proxmox が書き戻す表記と、宣言の書き方の違いは比べる前にそろえる。

- 大きさ：`32G`・`32768M`・`33554432K` は同じ（割り切れる一番大きい単位で持つ）。
- メモリ：`2048`・`2048M`・`2G` は同じ。観測の `current=2048,...`（新しい版の書き方）も同じ。
- ディスク：設定の `local-lvm:vm-100-disk-0,iothread=1,size=32G` から storage と size だけを取る。CD-ROM・cloud-init のドライブ・unused は入れない。
- NIC：設定の `virtio=BC:24:11:AA:BB:CC,bridge=vmbr0,tag=10,firewall=1` から。MAC は大文字小文字を区別しない。
- ssh の公開鍵：URL エンコードを戻し、行ごとに前後の空白を落とし、空行を除く。
- `ci_ip`・`ci_gateway`・`ci_dns`：アドレスの書き方をそろえる。

## 動き方

### 観測

`Read` は `/cluster/resources` で名前の VM を探し、`/nodes/<node>/qemu/<vmid>/status/current` で電源と lock を、`/nodes/<node>/qemu/<vmid>/pending` で設定を読む（今の値と、再起動を待つ値が 1 回で取れる）。

- `/cluster/resources` の vmid・ノードはクラスタの設定（pmxcfs）から来るが、**名前・status・template は pvestatd が約 10 秒ごとに写したもの**で、作った直後の VM は名前が空、status が古いことがある（本物の Proxmox VE の統合テストで見つけた）。
  名前が空の項目は VM の設定（`/config`）で名前とテンプレートかを確かめる（「無い」と見てもう一度作らないように）。電源と lock はいつも `/status/current` で読む（古い `stopped` を見て、走っている VM に start を重ねないように）。

- 無ければ absent。同じ名前が 2 つ以上・テンプレート・触らない VM は観測できない（Unknown とエラー）。
- 届かない・5xx（落ちたノードの 595 を含む）・ノードが落ちて status が `unknown`・認証の失敗（401・403）は **Unknown**（absent にしない）。認証の失敗は設定を直すまで治らないエラー（PermanentError）。エラーの文言にトークンの secret は出ない。
- VM に lock がある間（複製・移行・バックアップ・スナップショットなど）は **Converging**。
- `List` は全クラスタの `/cluster/resources` が取れ、どの VM も名前で一意に引けるときだけ complete。テンプレートと触らない VM は出さない。名前の無い・重なる VM があればエラー（名前を付けるか、`protected_vmids` に入れる）。
- `BulkObserve` はクラスタごとに `/cluster/resources` を 1 回だけ呼び、VM ごとに設定を読む（Read と等価、`sdk.BulkEquivalenceTester` で確かめている）。

### 再起動が要る変更（pending）

走っている VM の設定の一部（hotplug できない `cores`・`memory` など）は、Proxmox が pending に置いて次の起動で効かせる。

- 観測の値は**設定が言う値**（pending があればその値）。宣言どおりの値が pending にあれば差分にならず、**重ねて書かない**。そのフィールドの名前が `pending` に出る。
- 走っている VM を手で変えると、その変更も pending に置かれる。今効いている値は宣言どおりでも、手の変更が pending にあれば差分にして宣言の値を書き戻す（Proxmox は今の値と同じ pending を消す）。
- **再起動は明示したときだけ**：`apply_pending: reboot` を宣言する（宣言したフィールドに pending があり、VM が走っていて、`power: stopped` を宣言していないとき、1 回の正常な再起動を Op にする）か、Workflow の手順の一度きりの操作で再起動する。

### 非同期の仕事（UPID）

作る・複製する・resize・電源・消すは Proxmox の仕事として走る。Apply は `task_wait` まで終わりを待ち、終われば同じ Apply で続き（複製のあとの設定・起動）をする。終わらなければ UPID をメモリ（S3）に持って戻り、仕事が終わるまでの周は **Converging**（操作を重ねない）。
仕事が失敗で終われば、次の Read がその失敗を 1 回だけ Unknown として知らせる。UPID を失っても（デーモンの再起動）、VM に lock が残る間は Converging。

### 作る・消す

- 作る：`template` があれば full clone（`/nodes/<テンプレートのノード>/qemu/<vmid>/clone`、`target` はこの VM のノード）、無ければ空から（`POST /nodes/<node>/qemu`。scsi のディスクがあれば `scsihw: virtio-scsi-single`）。終わったら同じ Apply で残りを合わせて、`power: running` なら起動する。
- 消す（明示の削除、Delete は破壊的）：走っていれば止めて（stop）から、ディスクごと消す（`purge=1`・`destroy-unreferenced-disks=1`）。触らない VM・テンプレートは消さない。

### 一度きりの操作（Workflow の action）

`provider.Actioner` を持つ。Workflow の手順に `action: { op: reboot }`（正常な再起動。pending の変更が効く）・`action: { op: reset }`（強制の再起動、**破壊的**で予算に数える）を書ける。止まっている VM にはどちらも起動を送る。lock がある・届かないときは送らずに待つ。

## 統合テスト（`make integration-proxmox`）

本物の Proxmox VE を入れ子の KVM で動かす統合テスト（build tag `integration_proxmox`、[tests/integration/proxmox](../../tests/integration/proxmox)）。`make check`・`make test`・CI には入らない。

```console
$ make integration-proxmox
```

要るもの：KVM（`/dev/kvm`。入れ子の VM のため、ホストの kvm の nested が有効だとよい）、qemu（`qemu-system-x86_64`・`qemu-img`）、docker（パスワードなしの `sudo -n`）、curl・ssh・ssh-keygen。初回は ISO を取りに行く。

1. **ISO**：`proxmox-ve_9.2-1.iso` を版と sha256 で固定して取る（`SAKUTEI_IT_CACHE`、既定 `~/.cache/sakutei-it/proxmox` に残す）。
2. **自動導入の ISO**：docker の `debian:trixie-slim`（digest で固定）に Proxmox の apt の置き場から `proxmox-auto-install-assistant` を入れ、テストの中の応答（`answer.toml`：ext4・`10.0.2.10/24`・root のパスワード・テストの ssh の公開鍵）を焼いた ISO を作る（`--fetch-from iso`）。ssh の鍵はキャッシュに 1 つ作って使い回す（失敗したときに中の様子を集めるため）。
3. **導入**：qemu（4 CPU・4 GiB・32 GiB の virtio ディスク）でその ISO から入れ、`-no-reboot` で止まるのを待つ。導入済みのディスクはキャッシュに残し、次からは使い回す。
4. **起動**：導入済みのディスクの上の使い捨ての overlay で起動し、qemu の user ネット（slirp）で 8006 番と 22 番を `127.0.0.1` の空いたポートに出す。
   PVE は `10.0.2.10` に置き、slirp の DHCP は `10.0.2.100` から配らせる（slirp の DHCP は既定で `10.0.2.15` から配るので、PVE をそこに置くと、PXE で DHCP を引く入れ子の VM が同じアドレスをもらって hostfwd がぶつかり、API が答えたり答えなかったりする。最初の試行でこれに当たった）。API が答えたら root のパスワードで ticket を取り、root の API トークン（`root@pam!sakutei`、privsep なし）を作る。テストの間の証明書は自己署名なので、テストのクラスタの一覧は `insecure_skip_verify: true` を明示する。
5. ssh で触らない VM（`100 sakutei-ctl`。OS は無く、ディスクからしか起動しない＝PXE で DHCP を引かない。走らせておく）と、テンプレート（`9000 it-tmpl`）を作る。
   テンプレートには Debian 13 のクラウドイメージ（`debian-13-genericcloud-amd64-20261021-2638.qcow2`、版と sha512 で固定、キャッシュに残す）を `import-from` で入れ、cloud-init のドライブと vmbr0 の NIC を付ける（絶対パスからの取り込みは root@pam だけに許されるので、ここは API トークンでなく ssh）。
   作った VM の 22 番も `127.0.0.1` の空いたポートに出し（宛先は cloud-init で入れる `10.0.2.50`）、テストの鍵で入れることを確かめる。
6. `--provider-config` を渡した sakutei の CLI を回す。終われば（落ちても）qemu を止める。落ちたときは ssh で中の様子（負荷・`qm list`・仕事・pveproxy などの journal）をログに出す。

手で調べるときは `SAKUTEI_IT_PROXMOX_HOLD=30m go test -tags integration_proxmox -run TestHold -v ./tests/integration/proxmox/` で lab を立てたままにできる（API と ssh のポート・鍵の場所を出す）。

確かめること：

| | 内容 |
| --- | --- |
| テンプレートから作る → 適合 | vmid を採番（`500-509`）から配り、`plan` が CREATE を出し、`reconcile` で複製・resize（3G→4G）・NIC の足し（net1 に VLAN 42）・cloud-init・起動まで済み、`Converged (no changes)` になる。API で設定を見て宣言どおり。VM に cloud-init の利用者 `ops` とテストの鍵で ssh で入れ、ホスト名が `it-vm1`、アドレスが `10.0.2.50/24`、CPU が 2、ディスクが 4 GiB |
| 何周でも drift が出ない | `reconcile` を 5 回続けて `Converged (no changes)`、`status` は drifted=0 で、触らない VM とテンプレートは出ない |
| 手でメモリを変える → 戻る | API で走っている VM のメモリを変える（pending に置かれる）と `status` が memory の drift を出し、`reconcile` が書き戻して pending が消える |
| pending を重ねない・apply_pending | `cores` を変えると pending で止まり、次の `reconcile` は `Converged (no changes)`。`apply_pending: reboot` を足すと 1 回再起動して効き、VM の中の `nproc` が 3 になる |
| 触らない VM に触れない | 名前でも vmid でも、宣言すれば `plan`・`reconcile` が protected と言って断り、VM 100 の設定の digest は変わらず、走ったまま |
| 届かないと unknown | qemu を SIGSTOP で止めると `status --json` が unobservable=1・drifted=0。SIGCONT で戻せば適合に戻る |
| 消す | Delete で止めて消える。ほかの VM（100・9000）は残る |

### 結果（2026-10-27）

KVM の使える開発機（入れ子の KVM が有効、QEMU 8.2.2、docker 29.1.3）で、Proxmox VE 9.2-1 の ISO（入ったのは pve-manager 9.2.2・カーネル 7.0.2-6-pve）と Debian 13 のクラウドイメージ 20261021-2638 で回した結果（PASS）:

```
    proxmox_integration_test.go:352: Proxmox VE API is up on 127.0.0.1:33369 after 19s
=== RUN   TestProxmoxVM/create_from_the_template,_then_converged
    proxmox_integration_test.go:597: reconcile 1: Reconcile: 1 change(s) applied
    proxmox_integration_test.go:598: converged after 2 reconcile(s)
    proxmox_integration_test.go:613: inside the created VM:
        it-vm1
        2: eth0    inet 10.0.2.50/24 brd 10.0.2.255 scope global eth0\       valid_lft forever preferred_lft forever
        2
        4294967296
=== RUN   TestProxmoxVM/a_hand_change_of_the_memory_is_put_back
    proxmox_integration_test.go:636: after the hand change (running VM): pending map[memory:2048]
=== RUN   TestProxmoxVM/a_change_needing_a_restart_waits_as_pending,_and_apply_pending_reboots
    proxmox_integration_test.go:656: cores: current 2, pending "3"
    proxmox_integration_test.go:668: reconcile 1: Reconcile: 1 change(s) applied
=== RUN   TestProxmoxVM/unreachable_is_unknown
    proxmox_integration_test.go:718: the Proxmox VE VM was stopped for 4s
--- PASS: TestProxmoxVM (156.56s)
    --- PASS: TestProxmoxVM/create_from_the_template,_then_converged (26.98s)
    --- PASS: TestProxmoxVM/no_drift,_cycle_after_cycle (0.20s)
    --- PASS: TestProxmoxVM/a_hand_change_of_the_memory_is_put_back (0.20s)
    --- PASS: TestProxmoxVM/a_change_needing_a_restart_waits_as_pending,_and_apply_pending_reboots (82.60s)
    --- PASS: TestProxmoxVM/the_protected_VM_is_never_touched (0.03s)
    --- PASS: TestProxmoxVM/unreachable_is_unknown (5.08s)
    --- PASS: TestProxmoxVM/delete (5.14s)
ok  	github.com/tjst-t/sakutei/tests/integration/proxmox	156.570s
```

初回は ISO（1.7 GB）とクラウドイメージ（0.3 GB）の取得、自動導入の ISO の作成（約 30 秒）、導入（約 2 分）が前に付く。2 回目からはキャッシュを使い、上の約 2 分半で終わる。

本物で回して分かり、直したこと:

- **`/cluster/resources` は pvestatd の写しで遅れる**。作った直後の VM は名前が空・status が古く、名前で引くと「無い」と見えてもう一度作ろうとし（vmid がぶつかって止まった）、古い `stopped` を見て走っている VM に start を送っていた（`VM 500 already running`）。名前が空の項目は `/config` で確かめ、電源と lock は `/status/current` で読むようにした（偽の Proxmox にも `ResourceLag` で同じ遅れを入れ、`TestStaleResources` で確かめている）。直す前は作成に 7 回の reconcile（うち 4 回は Apply の失敗）、直した後は 1 回の Apply と確かめの 1 回。
- **Proxmox は形の悪い ssh の公開鍵を設定の書き込みで断る**（HTTP 500 `SSH public key validation error`）。計画の時点（ValidateIntent）で鍵の形（型と base64 の中の型の一致）を確かめるようにした。
- 入れ子の VM が PXE で DHCP を引くと、slirp の DHCP の最初のアドレス（`10.0.2.15`）を PVE とぶつけて API が不安定になる（テストの環境の話。上の手順の 4）。
- qemu を長く（60 秒を越えて）SIGSTOP で止めると、PVE の watchdog-mux（softdog）が戻ったあとに VM を再起動する（テストの環境の話。届かない段は時間切れ 2 秒の設定で回し、止める間を数秒にした）。

## 通常のテスト

`make check` に入るもの：

- [internal/proxmox](../../internal/proxmox)：偽の Proxmox VE（[pvetest](../../internal/proxmox/pvetest)、httptest。API トークンを確かめ、ディスクの切り出し・MAC の付与・pending・非同期の仕事・lock・ノードの停止・接続の切断と、`ResourceLag` で pvestatd の写しの遅れを真似る）を相手に、`sdk.ContractTester`・`sdk.BulkEquivalenceTester`、正規形、pending、触らない VM、ディスクの縮小とストレージの変更の計画のエラー、強制停止が破壊的になること、非同期の仕事の Converging と失敗、一度きりの操作。
- [tests/acceptance/proxmox_vm_test.go](../../tests/acceptance/proxmox_vm_test.go)：DefaultRegistry と CLI（`--provider-config`）を通して、採番の vmid でテンプレートから作り何周でも適合のまま・手の変更が戻る・pending を重ねない（AC-143-1〜4）。
