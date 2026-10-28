# nftables provider（nftables）

firewall の chain（順序付きのルール列）を収束させる provider。Schema の `ordered`（並び順が意味を持つ list フィールド）を end-to-end で使う最初の provider でもある。決まりの正は設計書 §29.1「nftables の実の ruleset 版（live）」（Backlog #121）・「nftables の base chain の policy と締め出しの守り」（Backlog #173）・「nftables の base chain の変更：確かめる先の宣言・変更の前の確かめ・承認と予算」（Backlog #179）・「網の間の許可を抽象のリソースにする」（Backlog #174、§7.6）と §7.4・§27（FW〈順序付き〉）。ここは使い方の説明で、矛盾したら spec/ が優先する。

| provider 名 | ID | 扱うもの |
| --- | --- | --- |
| `nftables` | `<family>/<table>/<chain>`（例 `inet/filter/web`） | regular chain 1 つ分のルール列、または intent が宣言した base chain の type・hook・priority・policy とルール列 |

- フィールドは `family`・`table`・`chain`（identity）と `rules`（**Ordered**）。`rules` は 1 本の文字列で、ルールを ` ; `（空白・セミコロン・空白）でつなぐ。並べ替えは diff（Update）になる。
- base chain の宣言は `type`・`hook`・`priority`・`policy` も書く（下の「base chain（既定の拒否）を宣言する」、Backlog #173）。到達を確かめる先は `confirm` に書ける（Backlog #179）。
- 完全階級（List・BulkObserve を持つ）。prune の対象にしない（`pruneable: false`。chain を消すとその chain のルールが一度に消え、通信断や穴あきに直結するため）。
- 宣言していない base chain（`type ... hook ...` や `policy` を持つ chain）・set・map は管理の外で、読みもせず書きもしない。List・BulkObserve は base chain を出さない（宣言していない base chain は status の宣言外にも出ない）。

## 対象：file と live

| | `file`（既定） | `live` |
| --- | --- | --- |
| 宣言する現実 | 管理された ruleset ファイル（`--ruleset <file>`） | カーネルの実際の ruleset |
| Read / List | ファイルを parse する | `nft -j list table` と `nft -a -s list table` を table ごとに 1 回ずつ |
| Apply | ファイルの対象の chain の行だけを書き換えて atomic に置き換える | `nft -c -f -` で検査してから `nft -f -` で 1 つのトランザクションとして流す |
| `nft` を呼ぶか | 呼ばない（ファイルを書いても FW には反映されない） | 呼ぶ |
| FW を直に変えたとき | 見えない | drift として見え、reconcile で戻る |
| 権限 | ファイルの読み書き | CAP_NET_ADMIN（ふつうは root） |
| 管理の範囲 | ファイルの中のすべての table の regular chain | 名指しした table（`--ruleset-tables`）の regular chain だけ |

Schema・identity・local id は同じなので、intent は対象を変えてもそのまま使える。file は CI や手元で ruleset を作る・見る用、live は実際の FW を収束させる用。

## 設定

フラグは常駐の `sakutei`・`plan`・`reconcile`・`status`・`import`・`sync` で効く。

| フラグ | `--provider-config` のキー | 意味 |
| --- | --- | --- |
| `--ruleset-target file\|live` | `ruleset_target` | 対象。既定 `file` |
| `--ruleset <file>` | `ruleset` | file の ruleset ファイル（無ければ `--state`、それも無ければ `sakutei-nftables.rules`） |
| `--ruleset-tables <family>/<name>,...` | `ruleset_tables` | live で管理する table。**live では必須**（推測しない）。family は `ip`・`ip6`・`inet`・`arp`・`bridge`・`netdev` |
| `--ruleset-nft <path>` | `ruleset_nft` | live で呼ぶ nft（既定 PATH の `nft`） |
| `--ruleset-scope <selector>` | `ruleset_scope` | List・BulkObserve が ID に付ける固定の scope（Backlog #106） |
| `--ruleset-rollback-timeout <d>` | `ruleset_rollback_timeout` | live で base chain を変えた後、確かめられなければホストが写しに戻すまでの時間（既定 `30s`、1 秒以上。確かめはその半分まで、Backlog #173） |
| `--ruleset-confirm-address <host:port>` | `ruleset_confirm_address` | 手元の live で base chain を変える前と後に、新しい TCP の接続で届くことを確かめる宛先（確かめる先の `self`、Backlog #173）。設定せず、宣言の `confirm` にも `tcp://` の先しか書いていなければ使わない。`self` を確かめるのに要るのに設定していなければ手元の base chain は変えない。離れたホストは ssh で確かめるので使わない |

`--provider-config` の例（フラグより優先する、§28.9）:

```yaml
# providers.yaml
providers:
  nftables:
    ruleset_target: live
    ruleset_tables: inet/filter,ip/nat
    ruleset_nft: /usr/sbin/nft
```

```console
$ sudo sakutei reconcile --intent intent/ --provider-config providers.yaml
$ sudo sakutei status --intent intent/ --ruleset-target live --ruleset-tables inet/filter
```

## intent の例

```json
{
  "resources": [
    {
      "provider": "nftables",
      "id": "inet/filter/web",
      "fields": {
        "family": "inet",
        "table": "filter",
        "chain": "web",
        "rules": "tcp dport 22 accept ; tcp dport { 80, 443 } ct state established,new counter accept comment \"web\" ; ip saddr 10.0.0.0/8 drop"
      }
    }
  ]
}
```

base chain から管理する chain へ飛ばす（`jump web`）のは、base chain を宣言しないなら人が書く（宣言していない base chain の hook・priority・policy・中身を Sakutei は変えない）。base chain ごと宣言で持つときは下の節。

## base chain（既定の拒否）を宣言する

既定で拒否する FW（`policy drop`）は、base chain を宣言して持つ（Backlog #173）。`type`・`hook`・`priority`・`policy` を書いた宣言が base chain の宣言で、4 つとも要る（読み込みで断る。policy を書き忘れて既定の accept になるのを防ぐ）。

```json
{
  "external_owners": [
    {"owner": "stage0", "provider": "nftables", "id": "fw1/inet/filter/mgmt"}
  ],
  "resources": [
    {
      "provider": "nftables",
      "id": "fw1/inet/filter/input",
      "fields": {
        "family": "inet", "table": "filter", "chain": "input",
        "type": "filter", "hook": "input", "priority": "filter", "policy": "drop",
        "rules": "ct state established,related accept ; jump mgmt ; tcp dport { 80, 443 } accept"
      }
    }
  ]
}
```

| フィールド | 値 |
| --- | --- |
| `type` | `filter`・`nat`・`route` |
| `hook` | `prerouting`・`input`・`forward`・`output`・`postrouting`（`ingress`・`egress` は device が要るので扱わない） |
| `priority` | 数（`0`・`-150`）か nft の名前（`filter`・`filter + 10`・`mangle - 5`）。名前は family ごとの nft の表で数に直して比べる（観測は数。`filter` と `0` は drift にならない） |
| `policy` | `accept`・`drop` |
| `confirm`（任意） | 変える前と後に到達を確かめる先（下の「確かめる先を宣言する」、Backlog #179）。書かなければ `self` |

- 宣言していない base chain は今どおり触らない。宣言したものだけを Read が観測し、Apply が変える。
- **宣言が regular chain（4 つを書かない）なのに現実が base chain なら、計画の時点で断る**（plan・reconcile は終了コード 1、status は観測できないとして報告）。policy を言わずに既定の拒否の chain のルールを変えさせない。
- type・hook・priority は今ある chain の上では変えられない（nft の決まり）ので、変わるときは 1 つのトランザクションで chain を消して作り直す（`flush chain`・`delete chain`・`add chain ... { type ... ; policy ... ; }`）。policy だけなら `add chain ... { ... }` で入れ替え、ルールは差分のあるときだけ入れ替える。
- file 版も同じ宣言を読み書きする（type 行は type・hook・priority・policy だけを読む。`device` などを含む行の chain は opaque で管理しない）。file は FW を変えないので下の守りは通さない。

### 到達性の最小の核は Sakutei の外で保守する

既定で拒否する chain を宣言どおりに収束させると、宣言が Sakutei 自身の管理の経路（ssh）を通していなければ、その瞬間に締め出される。**管理の ssh を通す規則などの最小の核は Sakutei の外で保守する**（§7.4）:

- 核は、宣言した base chain から `jump` する regular chain（上の例の `mgmt`、例えば `tcp dport 22 accept`）に置き、段0 の手順（Ansible など）で入れる。Sakutei には intent の `external_owners:`（§7.3（c））で「触らない」と宣言する（または scope で外す）。
- **ほかの base chain の `accept` は、この chain の `policy drop` を越えない**（nftables では `accept` は同じ hook のほかの base chain に進み、`drop` だけが最終の判定）。核を別の table の base chain に置いても救われない。宣言した base chain から辿れる場所（`jump`）に置く。
- 宣言の先頭近くに `ct state established,related accept` を置くと、いまの接続（ssh の master）も切れない。

### 締め出しの守り（live）

base chain を作る・変える・消す Apply は、NETCONF の commit confirmed と同じ考えの守りを通す（regular chain だけを変える Apply は今までどおり）:

0. **変える前に**、確かめる先（下の「確かめる先を宣言する」。書かなければ `self`）のすべてに新しい接続で 1 回ずつ届くかを確かめる。1 つでも届かなければ**変えない**（一時的な失敗。届かない先を名指しする。前に届かないのに変えると、後の確かめが落ちて戻すだけになるため。Backlog #179）。
1. 変える table の写しを取る（`nft list table`。戻しのスクリプトは `add table`・`delete table`・写し。無かった table は消す）。
2. `nft -c -f -` で検査する。
3. 対象のホストの側に、`ruleset_rollback_timeout`（既定 30 秒）の後に写しを `nft -f` で流す仕掛けを置く（POSIX の sh で一時のディレクトリ `${TMPDIR:-/tmp}/sakutei-nft-rollback.*` に写しを置き、`setsid`・`nohup` で切り離して眠る）。**ホストの上で時間で動くので、Sakutei との接続が切れても、Sakutei が落ちても戻る。** 戻したときは `logger -t sakutei-nft-rollback` で syslog に残す。
4. `nft -f -` で 1 つのトランザクションとして流す。
5. **新しい接続**で、確かめる先の**すべて**に到達を確かめる。`self` は、離れたホストなら ssh の master に相乗りしない新しい ssh の接続（`ControlPath=none`）で `true` を走らせ、手元の live なら `ruleset_confirm_address` への新しい TCP の接続。上限の半分まで、まだ届いていない先を張り直す。すべてに届いたときだけ確かめられたとする。
6. 確かめられたら仕掛けを取り消す（眠っている仕掛けを止め、写しを消す）。

- 確かめられなければ仕掛けを取り消さず、上限（＋少しの余り）まで待って写しに戻ったことを待ち、Apply の失敗として知らせる:

  ```
  sakutei: apply nftables//fw/inet/sk/input: transient: rolled back: nftables: Apply: host fw could not be reached over a new connection within 5s after changing base chain(s) fw/inet/sk/input (remote fw: unreachable: Connection timed out during banner exchange): the rollback armed on the host restored tables inet/sk as they were before, after 10s. ...
  ```

- 収束のループはそれを効かない Apply（#166）と同じに扱い、**同じ差分の Apply をその場で止める**（`ApplyIneffective` の STOPPED、reason `rolled_back`、status の oscillating）。次の周で同じ変更を流し続けない。宣言を直す（差分が変わる）・現実が宣言どおりになる・`sakutei apply-check release` で解ける。一回性の `reconcile` は終了コード 4 で、`--apply-check-state` を渡せば次の回も流さない。
- 仕掛けを置けない（sh が無い・権限が無い）ときは流さない（一時的な失敗）。手元の live で `self` を確かめるのに `ruleset_confirm_address` を設定していなければ base chain は変えない（PermanentError。宣言の `confirm` に `tcp://` の先だけを書けば設定は要らない）。
- 変える前に届かない先があったときの失敗の例（一回性の `reconcile` は Apply の失敗として報告し、終了コードは 0）:

  ```
  sakutei: apply nftables/site=lab/fw/inet/sk/input: transient: nftables: Apply: not changing base chain(s) fw/inet/sk/input: host fw cannot be reached over a new connection before the change (ssh://127.0.0.1:2200: remote fw: unreachable: ...); the change would only be rolled back. Confirm targets: self, ssh://127.0.0.1:2200
  ```
- 離れたホストで root が要るときは、remote の設定の `sudo: true` で仕掛けの sh も root で動く。

```yaml
providers:
  nftables:
    ruleset_target: live
    ruleset_tables: inet/filter
    ruleset_hosts: fw1
    remote_config: hosts.yaml
    ruleset_rollback_timeout: 20s
```

### 確かめる先を宣言する

到達を確かめる先は base chain の宣言の `confirm` に、`, ` で区切って書く（Backlog #179。正しい状態は宣言に置く）。**全部に届いたときだけ**確かめられたとし、変える前にも同じ先を確かめる。

| 書き方 | 確かめ方 |
| --- | --- |
| `self` | Sakutei 自身の管理の接続（書かなければこれだけ）。離れたホストはそのホストへの新しい ssh、手元の live は `ruleset_confirm_address` への TCP |
| `ssh://[user@]host[:port]` | 別の経路（別のアドレス・ポート）への新しい ssh の接続（`true` を走らせる）。鍵・known_hosts・タイムアウトはそのホストの remote の設定。user・port を書かなければそのホストの設定のまま。ホスト鍵はその経路の名前（`[host]:port`）で known_hosts と照らす。**手元の live には書けない**（ssh の設定が無い） |
| `tcp://host:port` | Sakutei からその宛先への新しい TCP の接続 |

```json
{
  "provider": "nftables", "id": "fw1/inet/filter/input", "scope": {"site": "lab"},
  "fields": {
    "family": "inet", "table": "filter", "chain": "input",
    "type": "filter", "hook": "input", "priority": "filter", "policy": "drop",
    "rules": "ct state established,related accept ; jump mgmt ; tcp dport 2222 accept ; tcp dport 443 accept",
    "confirm": "self, ssh://admin@192.0.2.10:2222, tcp://192.0.2.10:443"
  }
}
```

- regular chain の宣言には書けない（守りを通さないので）。形の読めないもの・`tcp://` のポート無し・同じ先の二度書き・`ssh://` の password は読み込みで断る。file 版も同じ形を見る（file は守りを通さないので使わない）。
- `confirm` は現実に無い（観測に出ない）ので、`confirm` だけを変えた宣言は収束のまま（Apply しない）。ほかの差分があるときは Apply に渡り、plan にも `+ confirm (new) → ...` と出る。
- 同じホストの 1 つのトランザクションで複数の base chain を変えるときは、それぞれの先を合わせたすべてを確かめる。

### 承認と予算（壊す変更）

base chain を**作る**・**type・hook・priority・policy を変える**（regular chain から base chain にする作り直し、type・hook・priority の変更の作り直しを含む）・**消す** Apply は、管理の経路を塞ぎうる**壊す変更**として扱う（Backlog #179。live だけ。file は FW を変えないので今までどおり）:

- **予算に数える**（Destructive。`--max-destructive-per-cycle` などの §13 の予算で止まる）。
- **承認ゲートを通す**：その変更を覆う planned_change（§13.3）の、その種類のレーン（作るのは `kind: create`、変えるのは `kind: update`、消すのは `ensure: absent` の `kind: delete`）が開いているときだけ流す。レーンの承認・cooling-off・窓（運用プロファイル、§14.4・§14.5）がそのまま承認ゲートで、レーンの `max_count` を使い、通常の予算にも数える。一回性の `reconcile` がレーンを開くのは `--audit-log` を付けたときだけ。
- レーンの外では流さず、level の Event `ChangeAwaitingApproval` を出す。`plan` には `[DESTRUCTIVE]`・`approval: required — changes base chain ... (policy accept → drop) (planned_change kind update)`・`blocked: approval — ...` と出る（`--json` では `destructive: true`・`approval_required: true`・`approval_reason`・`blocked_by: approval`）。
- ルールだけを変える Apply（base chain のルールを含む）と regular chain の Apply は今までどおり（承認は要らない。base chain のルールの変更は守りを通る）。

```json
{
  "planned_change": [
    {"scope": {"site": "lab"}, "kind": "update", "max_count": 2,
     "window": "2026-11-01 01:00 .. 2026-11-01 03:00",
     "approval": {"pr": 179, "approvers": ["alice"]}}
  ]
}
```

## 網の間の許可から作る

base chain の宣言は、intent の `network_policy:`（網と許可と行ごとの理由、Backlog #174）から展開して作れる。実現先（境界の FW・各ホスト）ごとに、そこを通る許可だけを 1 つの base chain（境界は hook `forward`、ホストは `input`、既定 `policy drop`）のルールにし、許可の `reason` をルールの comment に出す。展開した宣言はここに書いた base chain の宣言と同じに扱われる（承認と予算・締め出しの守り・確かめる先の `confirm`）。書き方・検査・comment の決まりは [../network-policy.md](../network-policy.md)。

## 離れたホスト（live）

live は、手元の nft の代わりに離れたホストの nft を ssh 越しに呼べる（Backlog #130）。1 つのデーモンが複数のホストの FW を扱う。設定・鍵とホスト鍵の扱い・権限は [../remote.md](../remote.md)。

| `--provider-config` のキー | 意味 |
| --- | --- |
| `ruleset_hosts` | 扱うホストの名前（`fw1,fw2`）。live のときだけ |
| `remote_config` | 対象の一覧のファイル（address・user・秘密鍵の SecretRef・known_hosts・sudo・タイムアウト） |

```yaml
providers:
  nftables:
    ruleset_target: live
    ruleset_tables: inet/filter
    ruleset_hosts: fw1,fw2
    remote_config: hosts.yaml
```

- ID の先頭にホストの名前が付く（`fw1/inet/filter/web`）。2 台に同じ table・chain があってもぶつからない。フィールドは変わらない。
- nft の呼び出し（`nft -j list`・`nft -a -s list`・`nft -c -f -`・`nft -f -`、base chain を変えるときは写しの `nft list table` と仕掛けの `sh`）をそのホストで走らせる。base chain を変える前と後の到達の確かめは、既定ではそのホストへの新しい ssh の接続（上の「締め出しの守り」）で、宣言の `confirm` で別の経路の ssh・TCP の宛先を足せる。スクリプトは標準入力で渡す。Apply はホストごとに 1 つのトランザクション。`ruleset_nft` はホストの上の nft のパスになる。
- ホストに届かない・認証の失敗は観測できない（unknown）、ホスト鍵の不一致は PermanentError。BulkObserve は読めないホストの分だけを返さず（そのホストの chain は Read が unknown と答える）、ほかのホストは収束を続ける。List は全部のホストを読めたときだけ complete。
- `ruleset_hosts` を書かなければ今までどおり手元の nft（ID も同じ）。

## live の動き方

- **観測**: 管理する table ごとに、`nft -j list table <family> <name>` の JSON で chain の並び・base chain かどうか（と base chain の type・hook・prio・policy）・ルールの handle の並びを、`nft -a -s list table <family> <name>` のテキストで handle ごとのルール文を読み、handle で突き合わせる。2 回の読みの間に ruleset が変わって合わなければ、その周は観測できない（次の周に読み直す）。
- **table が無い**のは「空」（chain は absent、List は complete で空）。Create は table も作る（`add table`）。
- **観測できない**（unknown。absent にしない、§3・§5.4）: nft が無い・走らない、権限が無い（`Operation not permitted`）、出力が読めない。List と BulkObserve は管理する table を全部読めたときだけ complete で、1 つでも読めなければエラー（prune の入力にしない）。
- **Apply**: 対象の chain の中身を入れ替えるスクリプトを作る。

  ```
  add table inet filter          # Create で table が無いとき
  add chain inet filter web      # Create
  flush chain inet filter web    # Create / Update
  add rule inet filter web tcp dport 22 accept
  ...
  flush chain inet filter web    # Delete
  delete chain inet filter web
  ```

  これを `nft -c -f -`（検査だけ）に通し、通ったら `nft -f -` で流す。nft の `-f` はスクリプト全体を 1 つのトランザクションにするので、途中で失敗すれば何も変わらない。検査で断られた（文法・値の誤り、存在しない chain への jump 等）のは PermanentError、権限が無い・`Device or resource busy`（ほかの chain から jump されている chain の削除など）・流す段の失敗は TransientError。同じプロセスの Apply は直列化する。
- **読み戻し**: 流した後に読み戻し、各 chain のルール列が宣言（の正規形）と同じかを確かめる。違えば下の「正規形」を参照。
- Apply はルールを入れ替えるので、その chain のカウンタは 0 に戻る。宣言どおりの chain には Apply が走らない（handle もカウンタもそのまま）。
- base chain を変えるスクリプトは上の「base chain（既定の拒否）を宣言する」のとおりで、締め出しの守りを通して流す。

## 正規形

観測するルール文は、nft が書き戻すテキストを次のようにそろえたもの。宣言のルール文も同じようにそろえてから比べる。

- 引用符の外の空白（連続・タブ・前後）を 1 つの空白にする。
- `counter packets <n> bytes <n>` を `counter` にする（観測は `-s` で読むので値は出ない。宣言に値を書いても無視する）。
- quota の `used <n> <unit>` を落とす。
- 末尾の `# handle <n>` は付けない。

### 無名の集合はまとめた形で比べる

nft は interval の集合（アドレス・CIDR・範囲・ポートの範囲を含む無名の集合 `{ ... }`）の**隣り合う・重なる範囲をまとめ、並べ替えて書き戻す**。要素が 1 つの集合は `{ }` を外して書き戻す。例（nftables v1.0.9）:

| 宣言 | nft が書き戻す形 |
| --- | --- |
| `ip saddr { 10.0.0.0/24, 10.0.1.0/24 } accept` | `ip saddr { 10.0.0.0/23 } accept` |
| `ip6 saddr { 2001:db8:8000::/33, 2001:db8::/33 } accept` | `ip6 saddr { 2001:db8::/32 } accept` |
| `tcp dport { 23, 22, 80-90, 91 } accept` | `tcp dport { 22-23, 80-91 } accept` |
| `tcp dport { 22 } accept` | `tcp dport 22 accept` |

これは drift にしない（Backlog #178）。比べるときは宣言の側も観測の側も同じ関数（`nftables.NormalizeSets`）で集合を正規化する: 要素を範囲に展開し、重なり・隣り合いを合わせて並べ替えた形どうしで比べる（1 つの範囲にまとまれば `{ }` を外した形）。PlanRefiner（plan・status の差分）と Apply の後の読み戻しの両方がこの比べ方で、file 版も同じ（ファイルに nft がまとめた形を書いても、宣言と同じなら差分にしない）。

- 正規化するのは、要素が全部 IPv4 のアドレス・範囲・CIDR か、全部 IPv6 のそれか、全部 10 進の数（ポート等）・数の範囲の集合だけ。名前（`{ established, related }`・`{ ssh, http }`）・連結（`ip saddr . tcp dport { ... }`）・map/vmap（`{ 22 : accept }`）・種類の混ざった集合は書いたまま比べる。
- 正規化しても違うもの（`{ 10.0.0.0/24, 10.0.2.0/24 }` と `{ 10.0.0.0/23 }`、`{ 22, 24 }` と `{ 22-24 }`）は今までどおり差分になる。
- 観測（state の `rules`）は nft が書き戻した形のまま持つ。正規形は比べるときだけ使う。
- 名前の付いた set・map は今までどおり管理の外（§29.1、Backlog #121）。
- ルールを作る側（網の間の許可の変換、Backlog #174、[../network-policy.md](../network-policy.md)）もこの正規形（`internal/nftrule.NormalizeSets`）で作るので、nft がまとめても同じ比べ方で適合になる。

これ以外の違いは drift になる。nft は宣言を**自分の形に書き換える**ことがある（例: `ct state new,established` → `ct state established,new`、`meta l4proto tcp tcp dport 22` → `tcp dport 22`）。そのような宣言は Apply の後の読み戻しで次のエラーになる（永久に drift が出続けないように、黙って受けない）:

```
permanent: nftables: Apply: nft lists rule 0 of inet/filter/ssh as "ct state established,new accept", not as declared "ct state new,established accept": declare it in the form nft lists it (it would show as drift every cycle otherwise)
```

ルールは **`nft list` が並べる形で宣言する**。迷ったら、使い捨ての table に一度流して `nft -a -s list chain` で読み戻した形を書く。

ルール文は nft のスクリプトにそのまま 1 行で埋め込むので、引用符の外の `;`・`#` と改行は受けない（読み込みの時点でエラー）。` ; ` を含むルール（引用符の中でも）は 1 本の `rules` に符号化できないので、そのルールを持つ chain は管理の外（Read は PermanentError）。

## 権限

live の Read も Apply も netlink で ruleset を読み書きするので、CAP_NET_ADMIN が要る（ふつうは root で動かす）。権限が無いと Read は観測できない（unknown）になり、drift とも適合とも数えない。network namespace の中の ruleset を扱うときは、その namespace に入った nft を `--ruleset-nft` に渡す（下の統合テストのラッパーと同じ形）。

## 統合テスト

本物の nft を相手にした統合テスト（Backlog #121）。build tag `integration_nftables` の付いたファイルにあり、`make check`・`make test`・CI には入らない（root が要るため）。

```console
$ make integration-nftables
```

要るもの: `nft`・`unshare`・`nsenter`・`docker`・`ssh`・`ssh-keygen`、パスワードなしの `sudo`（docker も `sudo -n docker` で呼ぶ）。締め出しの守りの段は、`make integration-remote` と同じイメージ（`tests/integration/remote/Dockerfile`、sshd・nft・sudo。無ければ作るので初回は apt のミラーに届くこと）を使う。

- `sudo -n unshare -n sh -c 'echo $$; exec cat >/dev/null'` で使い捨ての network namespace を作り、nft をその中で `sudo -n nsenter -t <pid> -n nft ...` として走らせるラッパーを `ruleset_nft` に渡す。sakutei は本物の ExecRunner で本物の nft を呼ぶ。ホストの FW には触れない（作った namespace がホストのものと違うことを最初に確かめる）。namespace は cat の標準入力が閉じれば（テストが終われば、落ちても）消える。
- sakutei のバイナリを作り、`--provider-config`（`ruleset_target: live`・`ruleset_tables: inet/sk`）で `status --json`・`plan`・`reconcile` を回す。

確かめること:

| 段 | 内容 |
| --- | --- |
| create from empty | table が無い状態で 2 chain が drift（現実に無い）→ reconcile → 適合、ルールの並びが宣言どおり |
| base chain and set are not touched | 管理する table に人が base chain（`jump web`）と set を足しても plan・status は変わらず、reconcile の後も両方そのまま |
| a rule deleted by hand | `nft delete rule ... handle` で 1 ルールを消す → status が drift 1 → reconcile で戻る |
| a reordering | chain のルールを手で並べ替える → drift 1 → reconcile で宣言の並びに戻る |
| a rule nft -c refuses | 範囲外のポート（`tcp dport 70000`）を宣言 → reconcile が `nft -c (check)` の PermanentError を報告し、ruleset 全体（handle 込み）が変わらない |
| no drift however many cycles | 宣言どおりのまま reconcile と status を 5 周 → drift 0、ruleset（handle 込み）が変わらない（Apply が走っていない） |
| normal form | 空白だけ違う宣言は drift にならない。nft が書き換える形（`ct state new,established`）の宣言は nft の形を添えたエラーになり、nft の形で書き直すと適合 |
| a set nft merges | 隣り合う /24 を 2 つ・隣り合う IPv6 の /33 を 2 つ・ポートとポートの範囲を並び違いで宣言 → reconcile が収束し、nft は `{ 10.0.0.0/23 }`・`{ 2001:db8::/32 }`・`{ 22-23, 80-91 }` とまとめて書き戻す → 3 周 status が適合のまま、ruleset（handle 込み）も変わらない（Apply が走らない）。要素が本当に違う集合の宣言は drift（Backlog #178） |
| the unmanaged table | 管理しない table（`ip other`）が最初から最後まで変わらない |

締め出しの守り（Backlog #173・#179、`lockout_integration_test.go`）。対象のホストは sshd・nft の入った使い捨てのコンテナ（自分の network namespace を持つ、NET_ADMIN）で、sshd は 22 と 2222（別の経路）で待ち、sakutei は手元から 127.0.0.1 の空いたポート越しに ssh で入る（`ruleset_hosts: fw`、`ruleset_rollback_timeout: 10s`、`ruleset_scope: site=lab`）。最小の核として、Sakutei の外で `inet sk mgmt`（`tcp dport 22 accept`）を入れ、intent の `external_owners:` で外す。base chain の作成・policy の変更は承認の要る変更なので、intent に承認した planned_change（`kind: create`・`kind: update`）を置き、reconcile に `--audit-log` を渡す。確かめと手の変更は docker exec（網を通らない）:

| 段 | 内容 |
| --- | --- |
| policy drop is applied and reaching the host is confirmed | `input`（hook input・policy drop・`ct state established,related accept ; jump mgmt ; tcp dport 80 accept`）を宣言 → reconcile が 0 で、上限を待たずに返る。policy は drop、新しい接続で sshd に届く。上限を過ぎても drop のまま（仕掛けは取り消された）、ホストに仕掛けの残りが無い。status は drift 0 |
| a declaration that shuts ssh out is rolled back on the host | `jump mgmt` を外した宣言 → reconcile が終了コード 4 で「rolled back」「could not be reached over a new connection」「STOPPED」を報告し、上限の後に返る。table は前の段のまま（handle・カウンタを除いて同じ）、新しい接続で届く |
| the same change is not applied again | もう一度 reconcile（`--apply-check-state`）→ 4 で STOPPED、流さない（すぐ返る）。table は変わらない |
| the host rolls back even when sakutei is gone | 別の、ssh を塞ぐ宣言で reconcile を始め、流れたのを確かめてから sakutei と ssh を SIGKILL → 新しい接続は届かない → 上限の後にホストの仕掛けが写しに戻し、届くようになる |
| a target not reached before the change: not changed | `confirm: self, ssh://127.0.0.1:<閉じたポート>` を書いた宣言 → reconcile は Apply の失敗（「before the change」とその先を名指し）を報告してすぐ返り、table は変わらず、ホストに仕掛けを置いていない（#179） |
| the other path opened (confirmed over self) | policy drop で塞がっている 2222 を、`self` だけで確かめる宣言で開ける（前で届かない先は書けないため） |
| another path declared and kept open: confirmed over both | `confirm: self, ssh://127.0.0.1:<2222 の手元のポート>` を書き、2222 を通したまま変える → reconcile は 0 で、上限を待たずに返る（両方の新しい ssh で届いた） |
| another path declared and shut: rolled back although self is reached | 同じ `confirm` で 2222 を通さない宣言 → 前の確かめは通り、後の確かめで別の経路が届かない → 4 で「rolled back」とその先を名指し、table は前の段のまま、両方の経路が届く |
| the minimal core was not touched | `mgmt` が最初から最後まで変わらない |

網の間の許可（Backlog #174、`netpolicy_integration_test.go` の `TestNFTablesNetworkPolicy`）：上の network namespace の nft に、`network_policy:` の境界（`inet np forward`）とホスト（`inet np input`）を展開して流す（確かめる先は宣言の `tcp://` のテストの待ち受け、承認した planned_change のレーンを置く）:

| 段 | 内容 |
| --- | --- |
| the allows are applied with their reasons as comments | 網 3 つ・許可 5 つ（`"`・`;`・`#`・`\`・日本語を含む reason、128 バイトを超える reason、icmpv6、reason の無い許可、同じ網の中の許可）→ reconcile → 2 つの base chain が policy drop で、ルールの comment に置き換え・切った reason が出る。status は適合 2・drift 0 |
| no drift however many cycles | reconcile と status を 3 周 → drift 0、table（handle 込み）が変わらない（Apply が走らない）。plan は「Converged」 |
| changing the declaration changes the rules | 許可を変える・外す・足す → reconcile → ルールが宣言どおりに変わり、drift 0 |
| an exception without reason is refused | `exception` の印で reason の無い許可 → reconcile が終了コード 1（`exception_without_reason`）、table は変わらない |

手元の live（`TestNFTablesLocalLockout`、上の network namespace の nft、`ruleset_rollback_timeout: 3s`）: 確かめの宛先（`ruleset_confirm_address`）がテストの待ち受けなら policy drop が残る。閉じたポートなら前の確かめで止まり、変えない。設定の宛先を持たず、宣言の `confirm` に `tcp://` の待ち受けを 2 つ書けば確かめられて変わる。2 つのうち 1 つが前の確かめの 1 度だけ受けて閉じる待ち受けなら、後の確かめで届かず 4 で「rolled back」とその先を名指し、table が元に戻る（仕掛けは手元の sh がラッパー越しの nft で流す。手元の TCP は namespace の FW を通らないので、「変えた後に届かない」は待ち受けの側で作る）。

### 結果

nftables v1.0.9 で回した結果（全段 PASS、Backlog #174 の段を足して）:

```
--- PASS: TestNFTablesLockoutOverSSH (57.60s)
    --- PASS: TestNFTablesLockoutOverSSH/policy_drop_is_applied_and_reaching_the_host_is_confirmed (14.14s)
    --- PASS: TestNFTablesLockoutOverSSH/a_declaration_that_shuts_ssh_out_is_rolled_back_on_the_host (12.47s)
    --- PASS: TestNFTablesLockoutOverSSH/the_same_change_is_not_applied_again (0.14s)
    --- PASS: TestNFTablesLockoutOverSSH/the_host_rolls_back_even_when_sakutei_is_gone (10.61s)
    --- PASS: TestNFTablesLockoutOverSSH/a_target_not_reached_before_the_change:_not_changed (0.65s)
    --- PASS: TestNFTablesLockoutOverSSH/the_other_path_opened_(confirmed_over_self) (2.69s)
    --- PASS: TestNFTablesLockoutOverSSH/another_path_declared_and_kept_open:_confirmed_over_both (1.20s)
    --- PASS: TestNFTablesLockoutOverSSH/another_path_declared_and_shut:_rolled_back_although_self_is_reached (12.68s)
    --- PASS: TestNFTablesLockoutOverSSH/the_minimal_core_was_not_touched (0.07s)
--- PASS: TestNFTablesLocalLockout (8.23s)
    --- PASS: TestNFTablesLocalLockout/confirmed (4.12s)
    --- PASS: TestNFTablesLocalLockout/not_reached_before_the_change:_not_changed (0.08s)
    --- PASS: TestNFTablesLocalLockout/declared_tcp_targets_without_ruleset_confirm_address (0.17s)
    --- PASS: TestNFTablesLocalLockout/one_declared_target_not_reached_after_the_change:_rolled_back (3.86s)
--- PASS: TestNFTablesNetworkPolicy (0.94s)
    --- PASS: TestNFTablesNetworkPolicy/the_allows_are_applied_with_their_reasons_as_comments (0.24s)
    --- PASS: TestNFTablesNetworkPolicy/no_drift_however_many_cycles_(comments_read_back_as_written) (0.39s)
    --- PASS: TestNFTablesNetworkPolicy/changing_the_declaration_changes_the_rules (0.29s)
    --- PASS: TestNFTablesNetworkPolicy/an_exception_without_reason_is_refused_and_nothing_changes (0.02s)
--- PASS: TestNFTablesLiveIntegration (2.41s)
    --- PASS: TestNFTablesLiveIntegration/create_from_empty (0.25s)
    --- PASS: TestNFTablesLiveIntegration/base_chain_and_set_are_not_touched (0.17s)
    --- PASS: TestNFTablesLiveIntegration/a_rule_deleted_by_hand_is_drift_and_comes_back (0.28s)
    --- PASS: TestNFTablesLiveIntegration/a_reordering_is_drift_and_is_put_back (0.21s)
    --- PASS: TestNFTablesLiveIntegration/a_rule_nft_-c_refuses_changes_nothing (0.07s)
    --- PASS: TestNFTablesLiveIntegration/declared_as_listed,_no_drift_however_many_cycles (0.43s)
    --- PASS: TestNFTablesLiveIntegration/normal_form (0.35s)
    --- PASS: TestNFTablesLiveIntegration/a_set_nft_merges_is_not_drift (0.62s)
    --- PASS: TestNFTablesLiveIntegration/the_unmanaged_table_did_not_change (0.01s)
PASS
```

- reconcile は Apply に失敗した resource を「not converged: 1 resource(s) failed to apply」と報告し、終了コードは 0（ほかの provider と同じ）。機械で見るときは `status --json` の `summary.drift` を見る。

## 通常のテスト

nft を呼ぶ所は `nftables.Runner`（seam）に、締め出しの守りは `nftables.Guard`（seam）に切ってあり、`make check` は偽の nft と偽の守り（`internal/nftables/nftfake`。Arm で写しを取り、上限で戻す。届かないかどうかはテストが決める）で回る。仕掛けの sh（`ExecGuard`）は手元の sh と、nft の代わりに写しを書き出すだけのスクリプトで、取り消し・時間切れの戻し・残りが無いことを確かめる。base chain と守りの試験は `internal/nftables/basechain_test.go`・`guard_test.go`、ループが巻き戻された Apply を次の周に繰り返さないことは `tests/acceptance/nftables_lockout_test.go`、確かめる先の宣言・前の確かめ・承認と予算は `tests/acceptance/nftables_approval_test.go`（Backlog #179。偽の守りは先ごとに届くかをテストが決める）。本物の nft の出力を記録したもの（`internal/nftables/testdata/`）を読む試験、契約テスト（`sdk.ContractTester`、観測できないのを absent にしないことを含む）、BulkObserve 等価性テスト（`sdk.BulkEquivalenceTester`）を live でも通す（`tests/acceptance/nftables_live_test.go`）。
