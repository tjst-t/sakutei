# Proxmox VE の VM の FW の規則（proxmox-fw）

Proxmox VE の VM の FW の規則（`/nodes/<node>/qemu/<vmid>/firewall/rules`）を宣言で置く provider（Backlog #197、設計書 §29.1「network_policy の実現先 proxmox-fw」）。
網の間の許可（[network_policy](../network-policy.md)）の実現先 `proxmox-fw` がこの provider の resource を作る。手で宣言してもよい。
REST API とクラスタの一覧（`proxmox_config`）は [proxmox-vm](proxmox-vm.md) と同じものを使う。

- 1 つの resource ＝ 1 台の VM の、**Sakutei が置いた規則の並び**。ID は `<クラスタ>/<VM 名>/fw`（Local ID は provider をまたいで一意なので、proxmox-vm の `<クラスタ>/<VM 名>` に `/fw` を足す）。
- Sakutei が置いた規則は comment の頭の**印**（`sakutei: <reason>`、reason が無ければ `sakutei`）で見分け、**印のある規則だけを書き換える**。印の無い規則（手で置いたもの・ほかの道具のもの）は消さず、書き換えない。
- 規則の**順は宣言どおり**：Sakutei の規則は並びの先頭に宣言の順で置き、印の無い規則はその後ろに元の順で残す。順の違い・Sakutei の規則の間や上に入った手の規則は drift になり、`reconcile` が戻す。
- 規則を置く・変える・外すのは **FW の変更として破壊的**（`[DESTRUCTIVE]`）に数える（§13 の予算。`--max-destructive-per-cycle` などを超えれば止まる）。
- **prune しない**（`Pruneable: false`）。宣言から外した VM の印の規則は残り、status の unmanaged に出る。外すのは `ensure: absent`（§12.2。印の規則だけを外す。planned_change の `kind: delete` のレーンが要る）か、`rules: ""` の宣言。
- VM の FW を有効にすること（NIC の `firewall=1`、FW の options の `enable=1`・既定の拒否 `policy_in`・`policy_out`）は proxmox-vm の欄（[proxmox-vm.md](proxmox-vm.md) の `nics`・`firewall`、#196）。VM がまだ無いとき、観測は「無い」、Apply は一時の失敗（proxmox-vm が作るのを待つ）。
- 扱わないもの：datacenter とノードの FW・security group・IPSet・alias の中身（規則から名前で指すのはよい）・`type: group`。

## 設定（proxmox_config）

proxmox-vm と同じクラスタの一覧を `--provider-config` の `proxmox-fw` に渡す（同じファイルでよい）:

```yaml
# provider-config.yaml
providers:
  proxmox-vm:
    proxmox_config: /etc/sakutei/pve.yaml
  proxmox-fw:
    proxmox_config: /etc/sakutei/pve.yaml
```

- 触らない VM（`protected_vmids`・`protected_names`）は proxmox-fw にも効く（List に出さず、Read・Apply もしない。宣言すれば計画のエラー）。
- **範囲は intent に**（§7.7）：クラスタの scope は intent の `providers.proxmox-fw.clusters.<名前>.scope` に書く（proxmox-vm と同じ形、[ranges.md](ranges.md)）。設定の `scope` は proxmox-vm と同じく安全の網として残る（食い違えば読み込みで断る）。

## 規則の書き方（rules）

`rules` は規則の文を ` ; ` でつないだもの（1 つも無ければ空の文字列）:

```
<in|out> <ACCEPT|DROP|REJECT> [iface=netN] [source=..] [dest=..] [proto=..] [dport=..] [sport=..] [icmp-type=..] [macro=..] [log=..] [enable=0] [comment=<文>]
```

| キー | 値 |
| --- | --- |
| 向き | `in`（VM に入る）・`out`（VM から出る）。大文字・小文字は問わない |
| action | `ACCEPT`・`DROP`・`REJECT` |
| `iface` | VM の口（`net0`・`net1`…）。書かなければすべての口 |
| `source`・`dest` | アドレス・CIDR の `,` 区切り（Proxmox の書き方。IPSet `+name`・alias の名前も書ける）。1 つの規則に IPv4 と IPv6 は混ぜられない |
| `proto`・`dport`・`sport` | `tcp`・`udp`・`icmp`・`icmpv6` など。ポートは `80,443,8000:8100`（範囲は `:`）。ポートは `proto`（か `macro`）が要る |
| `icmp-type`・`macro`・`log` | Proxmox の値（`macro=SSH`、`log=info` など） |
| `enable=0` | 置くが効かせない（既定は効く。Sakutei は作るときにいつも `enable` を渡す。Proxmox の作るときの既定は 0） |
| `comment=` | 最後に置き、行の終わりまで（空白を含めてよい。`;` は書けない）。**印は書かない**（provider が付け外す）。宣言の comment も読み戻した comment も同じに正規化して比べる（タブ・改行・続く空白は 1 つの空白、前後の空白は落とす、200 バイトを超えれば文字の境目で切って `…`）ので、そう書いても毎周の drift にならない |

```yaml
resources:
  - provider: proxmox-fw
    id: lab/gw-a/fw
    fields:
      rules: >-
        out ACCEPT iface=net0 source=10.0.0.1 dest=10.0.0.10,10.0.0.11 proto=udp dport=53,88,123 comment=関所から DNS・KDC・NTP ;
        in ACCEPT iface=net1 source=172.16.1.0/24 dest=172.16.0.11 proto=tcp dport=443 comment=サービス側は 443 だけ
```

書き方の違い（キーの順・大文字・小文字・空白）は正規形で比べるので drift にならない。network_policy から作るときは [network-policy.md](../network-policy.md) の「展開（proxmox-fw）」。

## 観測

| フィールド | 値 |
| --- | --- |
| `rules` | 並びの先頭から最後の印の規則までの正規形の文。間にある印の無い規則は `(unmanaged) <文>` として入る（順のずれが drift として見える） |
| `unmanaged` | 印の無い規則の数 |
| `cluster`・`name`・`vmid`・`node` | VM（ID から） |

**List** は VM ごとに、印の規則があれば `<クラスタ>/<VM 名>/fw` を、印の無い規則を 1 つずつ `<クラスタ>/<VM 名>/fw/<pos>`（観測したときの並びの位置）で返す。宣言の外の規則は `sakutei status` の unmanaged に出る（[§12.2](../architecture/spec/part4-safety.md)、#164）:

```
unmanaged（宣言外・実在・prune 対象外）: 1 リソース
  UNMANAGED proxmox-fw//lab/gw-a/fw/3
```

`<pos>` の付いた ID は宣言できない（読み込みで断る。その規則を Sakutei に持たせるなら、`<クラスタ>/<VM 名>/fw` の `rules` に書いて手の規則を消す）。どれかの VM の規則が読めなければ List はエラー（complete-or-error）で、status はその provider の unmanaged を「検出不可」と注記する。

## Apply

1 手ずつ、毎手読み直して（`digest` を付けて書くので、間に誰かが変えれば Proxmox が断り、その回は失敗する）:

1. 先頭から見て、宣言と違う位置に、後ろにある同じ規則を動かす（`moveto`）。
2. 無ければ、その位置の印の規則が後ろで要らなければ書き換える（`PUT`、消す欄は `delete`）。
3. 無ければ作る（Proxmox はいつも先頭に作るので、作った規則をその位置へ動かす）。
4. 最後に、余った印の規則を後ろから外す。

規則を全部消して作り直さないので、変える間に許可が抜ける時間を短くする。手の規則には触れない（位置は Sakutei の規則が上に入るぶん下がる）。

## 観測用と書き込み用の資格（§20.7）

proxmox-vm と同じクラスタの `observe:`・`write:` のトークンを使う（[proxmox-vm.md](proxmox-vm.md) の「観測用と書き込み用の資格」）。

| 操作 | 権限 |
| --- | --- |
| 観測（`GET /firewall/rules`、`/cluster/resources`） | `VM.Audit`（`/` か `/pool/<pool>` に `PVEAuditor`。Proxmox VE 9.2 で確かめた） |
| 規則を置く・変える・外す（`POST`・`PUT`・`DELETE /firewall/rules`） | `VM.Config.Network`（`PVEVMAdmin` に入っている） |

書かない回（`plan`・`status`）は書き込み用の `token_secret` が secret store に無くても動く。

## 統合テスト

`make integration-proxmox`（[proxmox-vm.md](proxmox-vm.md) の「統合テスト」と同じ入れ子の Proxmox VE 9.2）の「network_policy の実現先 proxmox-fw」段が、関所の VM（利用者側の `net0` は出る向きの KDC・DNS・LDAP・NTP、サービス側の `net1` は入る向きの 443 だけ）の network_policy を `reconcile` で当て、`/etc/pve/firewall/<vmid>.fw` に口と向きの規則が印と reason の comment つきで宣言の順に入ること、前から在った手の規則が後ろに残り status の unmanaged に出ること、置いた規則の port を手で変え先頭に手の規則を置くと（`PVEAuditor` のトークンの status でも）drift になり、`reconcile` が宣言の順に戻して手の規則を消さないことを確かめる。

確かめた Proxmox VE 9.2 の決まり（偽の Proxmox `internal/proxmox/pvetest` も同じに振る舞う）:

- `POST` で作った規則はいつも並びの先頭に入り、`enable` を渡さなければ 0（効かない）。
- `PUT …/rules/<pos>` の `moveto` は「元の位置 `moveto` の規則の前」に入れる（上へ動かすとその位置に着き、下へ動かすと `moveto-1` に着く。並びの外なら最後）。
- `GET` の各項目は並び全体の `digest` を持ち、`PUT`・`DELETE` に違う `digest` を渡すと「detected modified configuration」で断る。
- comment の日本語・`"`・`#` はそのまま読み戻せる。
