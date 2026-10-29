# Proxmox VE の VM provider（proxmox-vm）

Proxmox VE のクラスタの VM（qemu）を宣言で作り、pool・CPU・メモリ・ディスク・NIC・VM の FW の options・cloud-init・電源を合わせる provider（Backlog #143、設計書 §29.1「Proxmox VE の VM provider」）。
REST API（既定の前置きは `/api2/json`。設定の `api_base_path` で変えられる）を Go の標準ライブラリの net/http で叩く。認証は API トークン（`Authorization: PVEAPIToken=<user>@<realm>!<tokenname>=<secret>`）。

- 完全の階級：Read・Apply・List・BulkObserve（`/cluster/resources` を 1 回と VM ごとの設定）。
- VM は破壊的な型なので **prune しない**（`Pruneable: false`、§12.3）。宣言から外しても消えない。消すのは intent の **`ensure: absent`**（宣言で消す、§12.2、Backlog #163。下の「宣言で消す」）だけで、Delete は破壊的として予算（§13）に数え、planned_change（`kind: delete`）のレーンが開いているときだけ走る（承認）。
- **触らない VM**（Sakutei 自身が動く VM など段0 のもの、§23）は intent の `external_owners:`（§7.3、Backlog #158。下の「触らない VM を intent に書く」）に書く。設定の `protected_vmids`・`protected_names` は最後の安全の網として残り、それらは List に出さず、Read・Apply もしない。宣言すれば計画のエラー。
- コンテナ（LXC）・ストレージ・ネットワーク（ブリッジ）・HA・バックアップの設定と、FW の規則・datacenter とノードの FW の設定は扱わない（VM の FW は options だけ）。ゲスト OS の中は離れたホストの層（[docs/remote.md](../remote.md)）と [Ansible provider](ansible.md) で合わせる。

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
    api_base_path: /api2/json         # 省略可：API のパスの前置き（既定 /api2/json、"/" は前置き無し）
    token_id: sakutei@pve!sakutei     # <user>@<realm>!<tokenname>
    token_secret: secretref:file/proxmox/lab/token
    observe:                          # 省略可：観測用のトークン（下の「観測用と書き込み用の資格」）
      token_id: sakutei-ro@pve!audit
      token_secret: secretref:file/proxmox/lab/audit
    write:                            # 省略可：書き込み用のトークン（書かないキーは上の値）
      token_id: sakutei@pve!sakutei
      token_secret: secretref:file/proxmox/lab/token
    node: pve1                        # 省略可：node を宣言しない VM を作るノード
    scope: site=tokyo                 # 省略可：このクラスタの ID の scope（既定 root）。intent の providers: に移す（下）
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
- `api_base_path`（Backlog #160）は、実機と試験用の模擬で API のパスの前置きが違うとき（模擬が `/mock/pve/api2/json` の下にいる、逆プロキシの下のパスに置いた、など）に変える。つなぎ方なので provider の設定に置き（intent には書かない）、同じ intent を実機にも模擬にも当てられる。`/` で始まるパスだけを受け付け（末尾の `/` は落とす）、クエリ・`%`・空白・空の段・`.`・`..` の段は読み込みの時点でエラー。省けば今までどおり `/api2/json`。
- 未知のキーはエラー。YAML はリポジトリのサブセットで、`[a, b]` の流れの書き方は使えない（`- a` の形で書く）。
- トークンに要る権限は下の「トークンの権限」。

### クラスタの scope を intent に書く（`providers:`、Backlog #181）

クラスタの ID の scope は管理の範囲なので、intent のトップレベルの `providers:` に書く（§1.7・§7.7）。宛先・トークン・時間は
設定（`proxmox_config`）に残す:

```yaml
# intent/providers.yaml
providers:
  proxmox-vm:
    clusters:
      lab:                            # proxmox_config の clusters のクラスタの名前
        scope:                        # clusters.lab.scope: site=tokyo と同じ
          site: tokyo
```

- intent にだけ書けば、設定の `scope` に書いたのと同じに動く（List・BulkObserve の ID の scope、設定の除外の scope）。
- 設定にだけあれば、今までどおり読み、`plan`・`reconcile`・`status`・常駐が「is only in the provider config: write it in the
  intent too (providers.proxmox-vm.clusters.lab.scope)」と警告する（常駐では一度だけ）。
- 両方にあれば、設定は安全の網として残る。**食い違えば読み込みで断る**（弱い側に寄せない）。常駐は起動で断り、動いているあいだに
  intent が食い違うように変われば、その周を飛ばす。
- `proxmox_config` に無いクラスタの名前は断る（どこへどうつなぐかは設定が言う）。書けるのは `clusters.<名前>.scope` だけ。
- 触らない VM（`protected_vmids`・`protected_names`）は `providers:` ではなく `external_owners:` に書く（下）。設定から `scope` を
  外すのは次のメジャー版。

### 触らない VM を intent に書く（external_owners、Backlog #158）

設定はつなぎ方（宛先・鍵・時間）だけにし、管理の範囲と除外は intent に書く（§1.7・§29.1）。触らない VM は intent のトップレベルの `external_owners:`（§7.3（c））に書く。proxmox-vm は観測の値で選べるフィールドとして `vmid`・`node`・`pool` を申告している。

```yaml
# intent/stage0.yaml
external_owners:
  - owner: stage0                     # 持ち主（知らせの文言に出る）
    provider: proxmox-vm
    id: lab/sakutei-ctl               # protected_names の sakutei-ctl と同じもの
  - owner: stage0
    provider: proxmox-vm
    id_prefix: lab/                   # クラスタ lab の
    fields: { vmid: 100 }             # vmid 100（protected_vmids の 100 と同じもの）
  - owner: legacy-team
    provider: proxmox-vm
    id_prefix: lab/legacy-            # 名前で前方一致
```

- 選ばれた VM は観測するが、Apply・Delete・prune をしない。`resources:` に宣言する（`ensure: absent` を含む）と読み込みで断る。`vmid` で選ぶ宣言は、`resources:` が同じ `vmid` を宣言したときに断り、`vmid` を宣言しない（採番で配る）VM は観測した `vmid` が合えば Apply せずに知らせる（下流は待つ）。
- 設定の `protected_vmids`・`protected_names` は今までどおり効く（List に出さず、Read・Apply もしない）。そのうえでコアが intent と突き合わせる：
  - intent が設定で除外した VM を管理すると宣言すれば、読み込みで断る（intent の `external_owners:` に書いてあっても。弱い側に寄せない）。
  - 設定にだけある除外は、`plan`・`reconcile`・`status`・常駐が「write it in the intent too」と警告する（常駐では一度だけ）。上の書き方で intent にも書けば消える。
- 移し方：まず intent に `external_owners:` を書き（警告が消える）、設定の除外は網として残す。

## トークンの権限

トークンに要るのは、VM の作成・複製・設定・電源・削除（`VM.Allocate`・`VM.Clone`・`VM.Config.*`・`VM.PowerMgmt`・`VM.Audit`。VM の FW の options を書くのは `VM.Config.Network`）、ストレージの領域の割り当て（`Datastore.AllocateSpace`）、bridge を使うこと（`SDN.Use`）、テンプレートから複製するならそのテンプレートに `VM.Clone`。
仕事（UPID）の状態は、そのトークンが始めた仕事なら権限なしで読める。privsep のあるトークン（`--privsep 1`）の権限は、利用者とトークンの両方に付けた ACL の重なりなので、同じ ACL を両方に付ける。

### pool に絞ったトークン（Backlog #165）

VM に `pool` を宣言すると、作るときにその pool に入るので、トークンの権限を pool の ACL だけで絞れる。Proxmox VE 9.2 で確かめた最小の組（`make integration-proxmox` の pool の段と同じ）:

```console
# pvesh create /pools --poolid apps
# pveum user add sakutei@pve
# pveum user token add sakutei@pve sakutei --privsep 1
# for who in "--users sakutei@pve" "--tokens sakutei@pve!sakutei"; do
    pveum acl modify /pool/apps              --roles PVEVMAdmin,PVEPoolUser $who   # pool の VM を作る・設定・電源・消す、pool を見る
    pveum acl modify /storage/local-lvm      --roles PVEDatastoreUser $who         # ディスク・cloud-init のドライブの領域
    pveum acl modify /sdn/zones/localnetwork --roles PVESDNUser $who               # vmbr0 などの bridge（VLAN のタグを含む）
    pveum acl modify /vms/9000               --roles PVETemplateUser $who          # 複製の元のテンプレート（見る・複製する）
  done
```

| ACL | ロール | 何のため |
| --- | --- | --- |
| `/pool/<pool>` | `PVEVMAdmin` | pool の中の VM の作成（`pool` を渡した作成・複製は `/vms/<vmid>` でなく `/pool/<pool>` の `VM.Allocate` で通る）・設定・resize・電源・削除・読み・VM の FW の options（読みは `VM.Audit`、書きは `VM.Config.Network`） |
| `/pool/<pool>` | `PVEPoolUser`（`Pool.Audit`） | **pool を観測するため**。これが無いと `/cluster/resources` に `pool` が載らず、VM が「pool に無い」に見える（pool の drift になり、移そうとして権限で断られる） |
| `/storage/<storage>` | `PVEDatastoreUser` | 宣言したディスク・複製の先のストレージ |
| `/sdn/zones/localnetwork`（か bridge ごと） | `PVESDNUser` | NIC の bridge |
| テンプレート（`/vms/<vmid>`） | `PVETemplateUser` | 複製の元を見つけて（`VM.Audit`）複製する（`VM.Clone`）。テンプレートを同じ pool に入れてもよい |
| `/pool/<pool>` | `PVEPoolAdmin`（`Pool.Allocate`） | pool を移す（宣言の pool と違うとき）ときだけ。作るときに渡すだけなら要らない |

このトークンに見えるのは pool の VM と、`VM.Audit` のあるテンプレートだけである（`List`・`status` の unmanaged も pool の中だけ）。触らない VM（Sakutei 自身の VM）を pool の外に置けば、トークンからは見えも触れもしない。

## 観測用と書き込み用の資格（Backlog #186）

クラスタに観測用（`observe:`）と書き込み用（`write:`）のトークンを分けて書ける（§20.7。ssh 越しの provider の
[`observe:`・`write:`](../remote.md#観測用と書き込み用の資格) と同じ決まり）。どちらも `token_id`・`token_secret` だけを持ち、
書かないキーはクラスタの上の値を受け継ぐ。どちらも書かなければ今までどおり 1 本のトークン。

- 観測（`Read`・`List`・`BulkObserve`。`plan`・`status`・読み取りの口・常駐の毎周の観測、Apply のあとの効いたかの読み直し）は
  観測用のトークンだけを、書き込み（`Apply`・Workflow の `action`）は書き込み用のトークンだけを使う。Apply の中の読み直し
  （作った VM の設定を読むなど）は書き込み用で行う。
- 書かない回（`plan`・`status`・`import`・`--mode alert-only`・`--freeze` の `reconcile` など）は、書き込み用の `token_secret` が
  secret store に無くても起動する（確かめず、取り出さない）。書く回は無ければ読み込みで断る（`clusters.<名前>.write.token_secret: ... is not in the secret store`）。

観測用のトークンに要る最小の権限は **`/` に `PVEAuditor`**（`Datastore.Audit`・`Pool.Audit`・`SDN.Audit`・`Sys.Audit`・`VM.Audit` などの
読むだけの権限。Proxmox VE 9.2 で確かめた。`make integration-proxmox` の段と同じ）:

```console
# pveum user add sakutei-ro@pve
# pveum user token add sakutei-ro@pve audit --privsep 1
# pveum acl modify / --roles PVEAuditor --users sakutei-ro@pve
# pveum acl modify / --roles PVEAuditor --tokens 'sakutei-ro@pve!audit'
```

- `VM.Audit` で `/cluster/resources`・`/status/current`・`/pending`・`/firewall/options`（VM の FW の options、Backlog #196）が読める。`Pool.Audit` が無いと `/cluster/resources` に `pool` が
  載らない（pool を宣言した VM が「pool に無い」に見える。上の pool に絞ったトークンの表と同じ）。観測を pool に絞るなら、
  `/` でなく `/pool/<pool>` と複製の元のテンプレートに `PVEAuditor` を付ける（見える VM は pool の中だけになる）。
- Apply が待ち切れなかった仕事（UPID）の状態は、次の周の観測が観測用のトークンで読む。他人の始めた仕事の状態を読むには
  `/nodes/<node>` の `Sys.Audit` が要り、`PVEAuditor` に入っている。
- 書き込み用のトークンの権限は上の「トークンの権限」（または pool に絞ったトークン）のまま。

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
      pool: apps                      # Proxmox の pool。作るときに渡し、違えば移す
      cores: "2"
      memory: 4G                      # MiB の整数（4096）でもよい
      disks: "scsi0 storage=local-lvm size=32G"
      nics: "net0 bridge=vmbr0 vlan=20 firewall=1"
      firewall: "enable=1 policy_in=DROP policy_out=ACCEPT"   # VM の FW の options（書いたキーだけ）
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
| `pool` | pool の名前（入れ子は `親/子`）か `""` | 作るときに渡す（複製・作成の `pool`）。観測と違えば移す（`""` は pool から外す）。宣言しなければ触らない。pool は作らない（下の「pool」） |
| `cores` | 正の整数 | |
| `memory` | `2048`（MiB）・`2048M`・`2G` | |
| `disks` | `<slot> storage=<storage> size=<大きさ>` を `,` でつなぐ | slot は `scsiN`・`virtioN`・`sataN`・`ideN`。大きさは GiB で割り切れること。**宣言した slot だけ**を比べる。大きくするのはよい（resize）、**小さくするのと、ストレージを変えるのは計画のエラー**。無い slot は足す。宣言から外したディスクは消さない |
| `nics` | `<slot> bridge=<bridge> [vlan=<1-4094>] [mac=<MAC>] [model=<型>] [firewall=<0\|1>] [mtu=<1-65520>]` を `,` でつなぐ | **宣言した slot だけ**を比べる。`bridge` と `vlan`（書かなければタグ無し）はいつも、`mac`・`model`（virtio・e1000・e1000e・rtl8139・vmxnet3）・`firewall`（口の FW、Backlog #196）・`mtu`（`1` は bridge の MTU を使う、#196）は書いたときだけ比べる。書き換えるときも、宣言しない MAC・型・firewall・mtu とほかのキー（`rate` など）は残す。`mtu` は virtio の口だけに効くので、`model` に virtio 以外を書いて `mtu` も書くと読み込みで断る |
| `firewall` | `enable=<0\|1> policy_in=<ACCEPT\|DROP\|REJECT> policy_out=<…> ipfilter=<0\|1> macfilter=<0\|1>` のうち書くものを空白（かカンマ）でつなぐ | VM の FW の options（`/nodes/<node>/qemu/<vmid>/firewall/options`、Backlog #196）。**書いたキーだけ**を比べ、違うキーだけを書く（書かないキー・`dhcp`・`ndp`・`log_level_in` などは触らない）。書かなければ読むだけで比べない。観測はいつも 5 つのキーで、書かれていないキーは Proxmox の既定（`enable=0`・`policy_in=DROP`・`policy_out=ACCEPT`・`ipfilter=0`・`macfilter=1`）と見る。設定ではないので pending にならず、走っている VM にもすぐ効く |
| `ci_user` | ユーザ名 | cloud-init（`ciuser`） |
| `ci_ssh_keys` | 公開鍵（改行で区切る） | `sshkeys`。Proxmox は URL エンコードして持つ |
| `ci_ip` | `dhcp` か CIDR（IPv4） | `ipconfig0` の `ip`。`ip6` などほかのキーは残す |
| `ci_gateway` | IPv4 | `ipconfig0` の `gw` |
| `ci_dns` | アドレスを空白かカンマで | `nameserver`（順は意味がある） |
| `ciupgrade` | `true`・`false` | cloud-init が初回の起動でパッケージを更新するか（`ciupgrade`、Backlog #170）。**書かなければ Proxmox に渡さず、比べない**（Proxmox の既定は更新する）。観測は設定の `0`・`1` で、書かれていなければ既定の `true` と見る |
| `power` | `running`・`stopped` | 停止は正常停止（shutdown）。再起動は宣言できない（下の「再起動」） |
| `power_off` | `graceful`（既定）・`graceful_then_force` | `graceful_then_force` のときだけ、最初の shutdown から `force_after` を過ぎても止まらなければ強制停止（stop）。その Op は**破壊的**（予算に数える） |
| `force_after` | 時間（`5m` など） | `graceful_then_force` のときだけ書け、そのときは必須 |
| `apply_pending` | `never`（既定）・`reboot` | `reboot` のときだけ、宣言したフィールドに再起動を待つ変更があれば VM を再起動する |

cloud-init の値は、空から作る VM（`template` を宣言しない）では cloud-init のドライブ（`ide2`、最初のディスクと同じストレージ）を足して入れる。テンプレートから作る VM ではテンプレートのドライブを使う（無ければ値は設定に入るだけで、ゲストには届かない）。
外への出口が無い網では `ciupgrade: false` を明示すると、初回の起動がパッケージ更新の時間切れを待たない（手で確かめた例：起動の完了までが約 185 秒から約 54 秒に縮んだ）。
cloud-init の値はゲストが次に起動したときに読む（cloud-init の決まり。一度起動した VM の値を変えても、ゲストの中が変わるかはイメージの cloud-init の設定による）。

観測だけの値（Computed）：`lock`（Proxmox の lock）、`pending`（再起動を待つフィールドの名前）、`current_values`（それらの今効いている値、JSON）。

### 正規形（永久 drift を出さない）

Proxmox が書き戻す表記と、宣言の書き方の違いは比べる前にそろえる。

- 大きさ：`32G`・`32768M`・`33554432K` は同じ（割り切れる一番大きい単位で持つ）。
- メモリ：`2048`・`2048M`・`2G` は同じ。観測の `current=2048,...`（新しい版の書き方）も同じ。
- ディスク：設定の `local-lvm:vm-100-disk-0,iothread=1,size=32G` から storage と size だけを取る。CD-ROM・cloud-init のドライブ・unused は入れない。
- NIC：設定の `virtio=BC:24:11:AA:BB:CC,bridge=vmbr0,tag=10,firewall=1,mtu=1450` から。MAC は大文字小文字を区別しない。観測の正規形の `firewall` は 1 のときだけ出す（`firewall=0` と書かれていないのは同じ。`firewall=0` の宣言はどちらとも適合する）。`mtu` は書かれていれば出す。
- VM の FW の options：`GET /firewall/options` は書かれたキーだけを返す（Proxmox VE 9.2 で確かめた）ので、書かれていないキーは既定（pve-firewall が VM の口に使う値：入りは DROP、出は ACCEPT、MAC の filter は有効）で埋め、`enable=… policy_in=… policy_out=… ipfilter=… macfilter=…` の順に並べる。
- ssh の公開鍵：URL エンコードを戻し、行ごとに前後の空白を落とし、空行を除く。
- `ci_ip`・`ci_gateway`・`ci_dns`：アドレスの書き方をそろえる。

## 動き方

### 観測

`Read` は `/cluster/resources` で名前の VM を探し、`/nodes/<node>/qemu/<vmid>/status/current` で電源と lock を、`/nodes/<node>/qemu/<vmid>/pending` で設定を読む（今の値と、再起動を待つ値が 1 回で取れる）。VM の FW の options は `/nodes/<node>/qemu/<vmid>/firewall/options` で読む（宣言に依らずいつも読む。読めなければ設定と同じく Unknown）。

- `/cluster/resources` の vmid・ノードはクラスタの設定（pmxcfs）から来るが、**名前・status・template は pvestatd が約 10 秒ごとに写したもの**で、作った直後の VM は名前が空、status が古いことがある（本物の Proxmox VE の統合テストで見つけた）。
  名前が空の項目は VM の設定（`/config`）で名前とテンプレートかを確かめる（「無い」と見てもう一度作らないように）。電源と lock はいつも `/status/current` で読む（古い `stopped` を見て、走っている VM に start を重ねないように）。

- 無ければ absent。同じ名前が 2 つ以上・テンプレート・触らない VM は観測できない（Unknown とエラー）。
- 届かない・5xx（落ちたノードの 595 を含む）・ノードが落ちて status が `unknown`・認証の失敗（401・403）は **Unknown**（absent にしない）。認証の失敗は設定を直すまで治らないエラー（PermanentError）。エラーの文言にトークンの secret は出ない。
- VM に lock がある間（複製・移行・バックアップ・スナップショットなど）は **Converging**。
- `List` は全クラスタの `/cluster/resources` が取れ、どの VM も名前で一意に引けるときだけ complete。テンプレートと触らない VM は出さない。名前の無い・重なる VM があればエラー（名前を付けるか、`protected_vmids` に入れる）。
- `List`・`BulkObserve` の ID の scope は設定の `scope`（無ければ root）で、宣言の scope とは別に決まる。コアは status の
  宣言外（unmanaged）と prune で突き合わせる前に、これを宣言に合わせる（Backlog #169、§5.4）：
  - 宣言した VM は宣言の scope に合わせる（設定に `scope` を書かず、宣言に `scope: {node: mgmt}` と書いてよい。`--claim node=mgmt`
    でも claim なしでも、宣言した VM は宣言外に出ない）。
  - 宣言に無い VM は、この provider の宣言の scope が 1 種類ならその scope に入れる（status だけ。`--claim` の内外もそれで決まる）。
    2 種類以上か宣言が無ければ root のままで、`--claim` があれば「scope が決まらない N 件」と注記する。prune はこの推定をしない。
  - 設定の `scope` と宣言の scope が違えば食い違いで、status は宣言外を検出不可と注記し（終了コード 3）、prune は止まる。
    設定の `scope` は書かないか、宣言と同じにする。
- `BulkObserve` はクラスタごとに `/cluster/resources` を 1 回だけ呼び、VM ごとに設定を読む（Read と等価、`sdk.BulkEquivalenceTester` で確かめている）。

### Apply のあとの確かめ（settle）

Apply が成功を返したら、読み直して宣言どおりになったかを確かめる（Backlog #166、§11.1）。proxmox-vm の settle の既定は **30 秒**：電源の操作（start・shutdown・`apply_pending: reboot`）は、Proxmox の仕事が終わってからも VM の status が少しのあいだ前の値を返すことがある（reboot の仕事が終わった直後に `stopped` と読める）ので、その間は読み直して待つ。intent の `apply_check:` か `--settle` で変えられる（食い違えば長い方）。

### 再起動が要る変更（pending）

走っている VM の設定の一部（hotplug できない `cores`・`memory` など）は、Proxmox が pending に置いて次の起動で効かせる。
NIC の `firewall` は走っている VM にすぐ効く（Proxmox VE 9.2 で確かめた）。NIC の `mtu` を走っている VM で変えると、Proxmox は口を抜き差し（hot-unplug）し、ゲストが応じなければ `hotplug problem` で断る（Apply のエラー。止めてから変える）。

- 観測の値は**設定が言う値**（pending があればその値）。宣言どおりの値が pending にあれば差分にならず、**重ねて書かない**。そのフィールドの名前が `pending` に出る。
- 走っている VM を手で変えると、その変更も pending に置かれる。今効いている値は宣言どおりでも、手の変更が pending にあれば差分にして宣言の値を書き戻す（Proxmox は今の値と同じ pending を消す）。
- **再起動は明示したときだけ**：`apply_pending: reboot` を宣言する（宣言したフィールドに pending があり、VM が走っていて、`power: stopped` を宣言していないとき、1 回の正常な再起動を Op にする）か、Workflow の手順の一度きりの操作で再起動する。

### 非同期の仕事（UPID）

作る・複製する・resize・電源・消すは Proxmox の仕事として走る。Apply は `task_wait` まで終わりを待ち、終われば同じ Apply で続き（複製のあとの設定・起動）をする。終わらなければ UPID をメモリ（S3）に持って戻り、仕事が終わるまでの周は **Converging**（操作を重ねない）。
仕事が失敗で終われば、次の Read がその失敗を 1 回だけ Unknown として知らせる。UPID を失っても（デーモンの再起動）、VM に lock が残る間は Converging。

### 作る・消す

- 作る：`template` があれば full clone（`/nodes/<テンプレートのノード>/qemu/<vmid>/clone`、`target` はこの VM のノード）、無ければ空から（`POST /nodes/<node>/qemu`。scsi のディスクがあれば `scsihw: virtio-scsi-single`）。`pool` を宣言していればどちらにも渡す。終わったら同じ Apply で残りを合わせて、`power: running` なら起動する。
- 消す（`ensure: absent` の Delete、破壊的）：走っていれば止めて（stop）から、ディスクごと消す（`purge=1`・`destroy-unreferenced-disks=1`）。触らない VM・テンプレートは消さない。

### pool（Backlog #165）

- 作るときに渡す（あとから入れ直さない）。観測は `/cluster/resources` の `pool`（pool の設定の今の値で、名前・status のようには遅れない）。
- 観測と違えば drift（`status` に `pool` が出る）で、`reconcile` が移す：`PUT /pools` に `poolid`・`vms`・`allow-move=1`（ほかの pool にあれば移す）。`pool: ""` は今の pool から外す（`delete=1`）。`PUT /pools` は Proxmox VE 8.1 からの口で、それより前の版では移せない（Apply のエラー）。
- **pool は作らない**（pool は Proxmox の設定で、VM の宣言の外）。無い pool は Apply のエラーで、設定を直すまで繰り返さない（PermanentError）：作成・複製は Proxmox が 403 `Permission check failed (pool 'x' does not exist)` で、移すのは 500 `pool 'x' does not exist` で断る。文言に「pool を Proxmox で作る」と出る。
- 権限が無い（403）のも PermanentError で、要る権限（作るなら `/pool/<pool>` の `VM.Allocate`、移すなら `Pool.Allocate`、見るなら `Pool.Audit`）を文言に出す。
- **pool に絞ったトークンでは、pool から手で外された VM は見えない**（absent に見える）。作り直そうとして vmid がぶつかるので、Proxmox の `unable to create VM <vmid>: config file already exists` を「vmid がこのトークンに見えない VM に使われている（pool の外など）」と PermanentError にして知らせ、作り直さない。pool に戻すのは pool の外も見える資格（人か、別のトークン）の仕事。root に近いトークンでは、手で外したのは drift に見えて `reconcile` が戻す。

### 宣言で消す（`ensure: absent`、Backlog #163）

VM を消すことも宣言に書く（§12.2 の明示の absent。provider に依らない汎用の口）。

```yaml
planned_change:                        # 消すことの承認（§13.3・§14.4）
  scope: { site: lab }
  kind: delete
  max_count: 1
  window: "2026-10-28 00:00 .. 2026-10-29 00:00"
  approval: { pr: 163, approvers: [alice] }
resources:
  - provider: proxmox-vm
    scope: { site: lab }
    id: lab/old-app
    ensure: absent                      # 既定は present
```

- 在れば Delete、無ければ適合（何もしない）。`Pruneable` に関わらず効く（利用者が名指しで消すので）。
- `ensure: absent` のリソースに書けるのは `provider`・`scope`・`id`・`observe_tier` だけ。`fields`（`power_off` も）・`depends_on`・`update_policy`・`deletion_policy` は読み込みで断る。これに `depends_on` で依るリソースがあっても断る。
- **承認**：Delete はそのリソースを覆う planned_change（`kind: delete`）のレーンが開いているとき（窓の中・承認・cooling-off が揃った、§13.3・§14.5）だけ走り、レーンの `max_count` を 1 使う。レーンが無い・閉じている間は消さずに `AbsentAwaitingApproval`（level の Event）を出し、`plan` は `blocked: approval` を出す。一回性の `reconcile` がレーンを開くのは `--audit-log` を付けたときだけ。
- **予算**：レーンは承認であって除外ではない。Delete は通常の予算（rate limit・累積時間窓予算、§13.1・§13.2）にも数え、超えれば止まる。
- **電源**：走っている VM は stop（強制の停止）で止めてから消す。ディスクごと消すので正常停止は待たない（`power_off` の決まりはここには効かない）。中のサービスを正常に止めてから消したいときは、先に `power: stopped`（要れば `power_off`）を宣言して止め、そのあとで `ensure: absent` にする。
- 触らない VM（`protected_names`）を `ensure: absent` にすると読み込みで断る。`protected_vmids` の VM は名前では分からないので、Read・Apply が触らないことで守る（Unknown のまま消さない）。
- 採番（`vmid: { from_pool: … }`）の owner だった VM を消しても、値は自動では返さない。返すのは今の決まりどおり `sakutei allocator release`（cooldown のあと再び配る、§19.5）。

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
| 宣言の外の VM は unmanaged | 宣言を 0 件にすると `status --json` は it-vm1 だけを unmanaged に出し（total 1）、文の出力も `unmanaged=1`。触らない VM（100）とテンプレート（9000）は出ない。`reconcile` しても VM は消えず走ったまま。宣言に戻すと unmanaged は 0（#164） |
| pool に入れて作る・pool に絞ったトークン | root で pool `it-pool` と、privsep のトークン `sakutei@pve!pool`（ACL は上の「pool に絞ったトークン」の組）を作る。そのトークンの設定で、`pool: it-pool` の VM（vmid 520）をテンプレートから作り（複製・resize・VLAN の NIC・cloud-init・起動）、`Converged (no changes)` まで。root の API で VM が `it-pool` にあり走っている。トークンには pool の外の VM（100）が見えず、`status --json` は it-vm2 だけを適合に出す。root で pool から外すと、トークンの `reconcile` は「見えない VM が vmid を使っている」と知らせ、root のトークンでは `status` が pool の drift を出して `reconcile` が戻す。無い pool を宣言すると `does not exist` のエラーで、VM は動かない |
| absent で消す | 宣言から外しただけでは消えない（走ったまま）。`ensure: absent` にすると `plan` が DELETE を `blocked: approval` で出し、レーンが無い `reconcile` は消さない（`AbsentAwaitingApproval`）。承認のある planned_change（`kind: delete`）を足した `reconcile --audit-log` で走っている VM が止まって消え、次の `reconcile` は `Converged (no changes)`。ほかの VM（100・9000）は残る。触らない VM を `ensure: absent` にすると protected と言って断る |
| scope 付きの宣言と `--claim` | 設定に `scope` を書かず、it-vm2 を `scope: {node: mgmt}` で宣言し、API で直接 VM（vmid 530、`it-hand`）を作る。`status --json --claim node=mgmt` と claim なしの `status --json` は it-hand だけを `node=mgmt` の unmanaged に出し、it-vm2 は適合で注記は無い。`--claim node=edge` は何も出さない。`reconcile --claim node=mgmt` は `Converged (no changes)` で it-hand を消さない（#169）。クラスタの scope を intent の `providers.proxmox-vm.clusters.it.scope`（`node: mgmt`）に書いても `status --json` は警告なしに it-hand を出し、`reconcile` は `Converged (no changes)`。設定に無いクラスタの名前を書くと `plan` が断る（#181） |
| ciupgrade: false | `ciupgrade: false` の VM（vmid 540、止めたまま）をテンプレートから作り、`Converged (no changes)` まで。API で見た設定の `ciupgrade` が `0` で、`qm cloudinit dump 540 user` に `package_upgrade` が無い（Proxmox は 1 のときだけ `package_upgrade: true` を書く）。`ciupgrade` を書かない VM（520）の設定には `ciupgrade` が無い。API で 1 にすると `status` が ciupgrade の drift を出し、`reconcile` が 0 に戻す（#170）。出口の無い網で起動が待たされないことは確かめない（テストの網は slirp で外に出られる） |
| 観測用（PVEAuditor）と書き込み用のトークン | root で `sakutei-ro@pve!audit`（privsep、`/` に `PVEAuditor`）を作る。そのトークンは `/cluster/resources` を読めて、VM の設定を変えられない（403）。観測用にそれ、書き込み用に root のトークンを書いた設定で、API で手で `cores` を変えた VM（540）に対し、書き込み用の secret の無い store で `status`・`plan`・`reconcile --mode alert-only` が drift を出し、書く `reconcile` は `write.token_secret` が store に無いと言って起動せず VM は変わらない。両方の secret の store の `reconcile` が `1 change(s) applied` で戻し、次は `Converged (no changes)`（#186） |
| NIC の firewall・mtu と VM の FW の options | `nics: "net0 bridge=vmbr0 firewall=1 mtu=1450, net1 bridge=vmbr0 vlan=43 firewall=1"` と `firewall: "enable=1 policy_in=DROP policy_out=REJECT macfilter=0"` の VM（vmid 550、止めたまま）をテンプレートから作り、`Converged (no changes)` まで。API で見た net0・net1 に `firewall=1`（net0 に `mtu=1450`）、options は `enable`・`policy_out`・`macfilter` だけが書かれ（`policy_in=DROP` は既定と同じなので書かない）、`/etc/pve/firewall/550.fw` に `enable: 1`。API で net0 を `firewall=0,mtu=1500`、options を `enable=0`・`ipfilter=1`（書かないキー）にすると、観測用のトークン（`PVEAuditor`）でも `status` が nics と firewall の drift を出し、`reconcile` が `1 change(s) applied` で戻して `ipfilter=1` は残す。書かない VM（540）は options が空のままで、手で net0 に `firewall=1,mtu=1400`・options に `enable=1` を書いても `reconcile` は `Converged (no changes)`（#196） |

### 結果（2026-10-29、Backlog #196 の「NIC の firewall・mtu と VM の FW の options」）

同じ開発機・同じキャッシュで、`make integration-proxmox` が PASS（全段で約 2 分 20 秒）。最後の段に、NIC の `firewall`・`mtu` と
VM の FW の options を書いた VM を複製して合わせ、手の変更を戻し、書かないキーと書かない VM は比べない段を足した。
あわせて手で確かめたこと（Proxmox VE 9.2）：`GET /firewall/options` は書かれたキーだけを返す（作った直後は `digest` だけ）、
pve-firewall は書かれていない `policy_in` を DROP・`policy_out` を ACCEPT・`macfilter` を有効として使う、
走っている VM の NIC の `firewall` の変更は pending にならずすぐ効く、`mtu` の変更は口の抜き差しになり、ゲストの無い VM では
`hotplug problem` で断られる。options の読みは `VM.Audit`、書きは `VM.Config.Network`（`PVE/API2/Firewall/VM.pm` の権限の検査）:

```
--- PASS: TestProxmoxVM (136.10s)
    ...
    --- PASS: TestProxmoxVM/観測用（PVEAuditor）と書き込み用のトークン (2.18s)
    --- PASS: TestProxmoxVM/NIC_の_firewall・mtu_と_VM_の_FW_の_options (9.94s)
ok  	github.com/tjst-t/sakutei/v2/tests/integration/proxmox	136.108s
```

### 結果（2026-10-28、Backlog #186 の「観測用（PVEAuditor）と書き込み用のトークン」）

同じ開発機・同じキャッシュで、`make integration-proxmox` が PASS（全段で約 3 分）。最後の段に、`/` に `PVEAuditor` だけの privsep の
トークンで観測し（書き込み用の secret の無い store で `status`・`plan`・`reconcile --mode alert-only` が drift を出し、書く `reconcile`
は起動しない）、root のトークンで書いて戻す段を足した。監査役のトークンで VM の設定を変える要求は Proxmox が断った:

```
--- PASS: TestProxmoxVM (186.21s)
    ...
    --- PASS: TestProxmoxVM/ciupgrade:_false（初回の起動でパッケージを更新しない） (10.61s)
    --- PASS: TestProxmoxVM/観測用（PVEAuditor）と書き込み用のトークン (2.22s)
ok  	github.com/tjst-t/sakutei/v2/tests/integration/proxmox	186.211s
```

### 結果（2026-10-28、Backlog #181 の「クラスタの scope を intent に書く」）

同じ開発機・同じキャッシュで、`make integration-proxmox` が PASS（全段で約 2 分）。「scope 付きの宣言と --claim」の段の終わりに、
クラスタの scope を intent の `providers.proxmox-vm.clusters.it.scope` に書いた `status --json`・`reconcile` と、設定に無いクラスタを
書いた `plan` の拒否を足した。最初の試行は段の確かめ方の誤りで落ちた（#158 の除外の警告も「is only in the provider config」と
言うので、範囲の警告を `providers.proxmox-vm.clusters` で見るように直した）:

```
--- PASS: TestProxmoxVM (123.74s)
    ...
    --- PASS: TestProxmoxVM/scope_付きの宣言と_--claim（provider_の側に_scope_を書かない） (1.30s)
    --- PASS: TestProxmoxVM/ciupgrade:_false（初回の起動でパッケージを更新しない） (10.75s)
ok  	github.com/tjst-t/sakutei/v2/tests/integration/proxmox	123.740s
```

### 結果（2026-10-28、Backlog #170 の「ciupgrade: false」）

同じ開発機・同じキャッシュで、`make integration-proxmox` が PASS（全段で約 3 分）。複製の後の設定の書き換えに `-ciupgrade 0` が
cloud-init のほかの値と一緒に入った（pvedaemon の journal）。最初の試行では user-data に `package_upgrade: false` が出ると見込んで
落ちた：Proxmox VE 9.2 は `ciupgrade` が 1 のときだけ `package_upgrade: true` を書き、0 のときは何も書かない（cloud-init の既定は
更新しない）。確かめ方を「`package_upgrade` が無い」に直した:

```
--- PASS: TestProxmoxVM (183.59s)
    ...
    --- PASS: TestProxmoxVM/scope_付きの宣言と_--claim（provider_の側に_scope_を書かない） (2.20s)
    --- PASS: TestProxmoxVM/ciupgrade:_false（初回の起動でパッケージを更新しない） (10.58s)
ok  	github.com/tjst-t/sakutei/v2/tests/integration/proxmox	183.598s
```

### 結果（2026-10-28、Backlog #169 の「scope 付きの宣言と --claim」）

同じ開発機・同じキャッシュで、`make integration-proxmox` が PASS（全段で約 3 分）。あわせて「何周でも drift が出ない」段の
確かめ方を、#158 の警告の行（触らない VM の名前を言う）を除いた報告の行だけを見るように直した:

```
--- PASS: TestProxmoxVM (172.02s)
    ...
    --- PASS: TestProxmoxVM/pool_に入れて作る・pool_に絞ったトークン (16.19s)
    --- PASS: TestProxmoxVM/scope_付きの宣言と_--claim（provider_の側に_scope_を書かない） (1.19s)
ok  	github.com/tjst-t/sakutei/v2/tests/integration/proxmox	172.023s
```

### 結果（2026-10-28、Backlog #165 の「pool に入れて作る・pool に絞ったトークン」）

同じ開発機・同じキャッシュで、`make integration-proxmox` が PASS（全段で約 3 分）:

```
=== RUN   TestProxmoxVM/pool_に入れて作る・pool_に絞ったトークン
    proxmox_integration_test.go:926: reconcile 1: Reconcile: 1 change(s) applied
    proxmox_integration_test.go:927: converged with the pool token after 2 reconcile(s)
    proxmox_integration_test.go:945: the pool token sees [520:it-vm2:it-pool 9000:it-tmpl:]
--- PASS: TestProxmoxVM (171.87s)
    ...
    --- PASS: TestProxmoxVM/absent_で消す (5.31s)
    --- PASS: TestProxmoxVM/pool_に入れて作る・pool_に絞ったトークン (16.20s)
ok  	github.com/tjst-t/sakutei/v2/tests/integration/proxmox	171.876s
```

pool に絞ったトークン（privsep）で、テンプレートからの複製・resize・VLAN の NIC・cloud-init・起動が 1 回の Apply で済み、次の `reconcile` は適合。トークンに見えるのは pool の VM とテンプレートだけ（テンプレートの `pool` は空）。

段を作る前に本物で調べて分かったこと：

- `/cluster/resources` の `pool` は、トークンにその pool の **`Pool.Audit` が無いと載らない**（`PVEVMAdmin` だけでは VM は見えるが pool が空に見える）。`PVEPoolUser` を足すと載る。
- 無い pool は、作成・複製では 403 `Permission check failed (pool 'nope' does not exist)`、`PUT /pools` では 500 `update pools failed: pool 'nope' does not exist`。ほかの pool にある VM は `allow-move` が無ければ 500 で断られ、pool に無い VM を外すと 500 `VM <vmid> is not a pool member`。
- pool の外の VM と同じ vmid で複製すると 500 `unable to create VM <vmid>: config file already exists`（トークンにその VM は見えない）。

### 結果（2026-10-28、Backlog #163 の「absent で消す」）

同じ開発機・同じキャッシュ（Proxmox VE 9.2-1、Debian 13 のクラウドイメージ 20261021-2638）で、`make integration-proxmox` が PASS（直接の Delete の段を「absent で消す」に置き換えた）:

```
=== RUN   TestProxmoxVM/absent_で消す
    proxmox_integration_test.go:795: reconcile 0 with the approved lane: exit 0
        sakutei reconcile: PlannedChangeOpened: planned_change(scope=site=it kind=delete ...): lane opened (profile team; approval 1 of 1 approver(s) [alice] required by the team profile)
        - DELETE proxmox-vm/site=it/it/it-vm1 [DESTRUCTIVE]
          reason: resource proxmox-vm/site=it/it/it-vm1 is declared ensure: absent but exists in reality; will delete
          cause:  A
        Reconcile: 1 change(s) applied
    --- PASS: TestProxmoxVM/absent_で消す (5.27s)
ok  	github.com/tjst-t/sakutei/v2/tests/integration/proxmox	157.051s
```

走っている VM は 1 回の `reconcile` で止まって消え（stop と destroy が `task_wait` のうちに終わった）、次の `reconcile` は `Converged (no changes)`。この回のあとで、レーンの知らせ（`PlannedChange: … admitted`）の文言だけを直した（absent の削除は通常の予算にも数えると書く）。動きは変えていない。

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
    --- PASS: TestProxmoxVM/delete (5.14s)   ← 2026-10-28（#163）に「absent で消す」へ置き換えた
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
- [tests/acceptance/proxmox_vm_test.go](../../tests/acceptance/proxmox_vm_test.go)：DefaultRegistry と CLI（`--provider-config`）を通して、採番の vmid でテンプレートから作り何周でも適合のまま・手の変更が戻る・pending を重ねない（AC-143-1〜4）。pool に入って作られ、手で外すと戻る・pool に絞ったトークンで作れて読め、pool から外された VM は作り直さずに知らせる（AC-165-1・2）。
- pool の単体（[pool_test.go](../../internal/proxmox/pool_test.go)）：偽の Proxmox に pool（`Options.Pools`・`SetPool`・`PUT /pools`）と pool に絞ったトークン（`Options.PoolTokens`。pool の VM とテンプレートしか見えず触れない、`PoolAudit` が無ければ pool が載らない）を足して確かめる。
