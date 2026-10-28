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
| 管理の範囲 | ファイルの中のすべての table の regular chain | 名指しした table（intent の `providers.nftables.tables`、または `--ruleset-tables`）の regular chain だけ |

Schema・identity・local id は同じなので、intent は対象を変えてもそのまま使える。file は CI や手元で ruleset を作る・見る用、live は実際の FW を収束させる用。

## 設定

フラグは常駐の `sakutei`・`plan`・`reconcile`・`status`・`import`・`sync` で効く。

| フラグ | `--provider-config` のキー | 意味 |
| --- | --- | --- |
| `--ruleset-target file\|live` | `ruleset_target` | 対象。既定 `file` |
| `--ruleset <file>` | `ruleset` | file の ruleset ファイル（無ければ `--state`、それも無ければ `sakutei-nftables.rules`） |
| `--ruleset-tables <family>/<name>,...` | `ruleset_tables` | live で管理する table。**live では必須**（推測しない。intent の `providers.nftables.tables` に書けば設定には要らない）。family は `ip`・`ip6`・`inet`・`arp`・`bridge`・`netdev`。**intent に移す**（下の「範囲を intent に書く」） |
| `--ruleset-nft <path>` | `ruleset_nft` | live で呼ぶ nft（既定 PATH の `nft`） |
| `--ruleset-scope <selector>` | `ruleset_scope` | List・BulkObserve が ID に付ける固定の scope（Backlog #106）。**intent の `providers.nftables.scope` に移す** |
| `--ruleset-rollback-timeout <d>` | `ruleset_rollback_timeout` | live で base chain を変えた後、確かめられなければホストが写しに戻すまでの時間（既定 `30s`、1 秒以上。確かめはその半分まで、Backlog #173） |
| `--ruleset-confirm-address <host:port>` | `ruleset_confirm_address` | 手元の live で base chain を変える前と後に、新しい TCP の接続で届くことを確かめる宛先（確かめる先の `self`、Backlog #173）。設定せず、宣言の `confirm` にも `tcp://` の先しか書いていなければ使わない。`self` を確かめるのに要るのに設定していなければ手元の base chain は変えない。離れたホストは ssh で確かめるので使わない |
| （設定だけ） | `ruleset_guard_tool` | 離れたホスト（`ruleset_hosts`）で、nft と sh の代わりに決まった道具 `sakutei-nft-guard` を走らせる（そのホストの上の道具の絶対パス。ふつうは `/usr/local/libexec/sakutei-nft-guard`、Backlog #191）。下の「決まった道具（sakutei-nft-guard）」。`ruleset_nft` と併せては書けない（nft のパスは道具の設定ファイルに書く）。手元の live には使えない |

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

## 範囲を intent に書く（`providers:`、Backlog #181）

provider の設定はつなぎ方（対象・nft・ホスト・時間）だけにし、管理の範囲（管理する table と ID の scope）は intent に書く
（§1.7・§7.7）:

```yaml
# intent/providers.yaml
providers:
  nftables:
    scope:                            # --ruleset-scope site=tokyo と同じ
      site: tokyo
    tables:                           # --ruleset-tables inet/filter,ip/nat と同じ（live だけ）
      - inet/filter
      - ip/nat
```

- intent にだけ書けば、フラグを渡したのと同じに動く（live の `--provider-config` に `ruleset_tables` は要らない）。
- 設定にだけあれば、今までどおり読み、`plan`・`reconcile`・`status`・常駐が「is only in the provider config: write it in the
  intent too (providers.nftables.tables)」と警告する（常駐では一度だけ）。
- 両方にあれば、設定は安全の網として残る。**食い違えば読み込みで断る**（弱い側に寄せない。nft を走らせる前に断る）。table の
  並びの違いは食い違いではない（同じ組なら同じ）。常駐は起動で断り、動いているあいだに intent が食い違うように変われば、その周を
  飛ばす。intent を直せば provider を組み立て直して続ける。
- `tables` は live のためのもの。file の対象（ファイルの全部を収束させる）で `tables` を書けば断る（範囲が黙って広がらないように）。
- 書けるのは `scope` と `tables` だけ。設定から `ruleset_scope`・`ruleset_tables` を外すのは次のメジャー版。

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
3. 対象のホストの側に、`ruleset_rollback_timeout`（既定 30 秒）の後に写しを `nft -f` で流す仕掛けを置く（POSIX の sh で一時のディレクトリ `${TMPDIR:-/tmp}/sakutei-nft-rollback.*` に写しを置き、`setsid`・`nohup` で切り離して眠る。決まった道具を使うホストでは道具の `arm` が写しを自分で取り、root だけが読み書きできる状態のディレクトリに置いて、自分を切り離して起動し直して眠る。下の「決まった道具」）。**ホストの上で時間で動くので、Sakutei との接続が切れても、Sakutei が落ちても戻る。** 戻したときは `logger -t sakutei-nft-rollback` で syslog に残す。
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
- 離れたホストで root が要るときは、remote の設定の `sudo: true` で仕掛けの sh も root で動く。sh と nft を sudo で許すのは root を与えるのと同じなので、sudo を使うホストでは決まった道具（下）を使い、sudo で許すのを道具だけにする。
- 上限は秒に切り上げて仕掛ける（`1.5s` なら 2 秒）。Sakutei は切り上げた時刻まで待つ。

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
- `ruleset_guard_tool` を書いたホストでは、上の nft と sh の代わりに決まった道具だけを走らせる（下）。

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

## 決まった道具（sakutei-nft-guard、Backlog #191）

離れたホストを sudo つきの利用者で扱うと、今までの動かし方（nft を直接・巻き戻しの仕掛けを `sh -c` で）では sudoers に `nft` と `sh` を許すしかなく、それは root を与えるのと同じだった。資格が漏れたときの被害を小さくするため（役目ごとの鍵の分け、#185 と同じ考え）、ホストで root で走るのを**決まった道具 1 つ**にし、**sudo で許すのも道具だけ**にできる。

- 道具はこのリポジトリの `cmd/sakutei-nft-guard`（Go の小さな別の実行ファイル。外の依存なし）。ホストの決まった場所 `/usr/local/libexec/sakutei-nft-guard` に置き、provider の設定 `ruleset_guard_tool` にそのパスを書く。
- 道具がやるのは、決まった形の引数の次だけ（ほかは何もせず終了コード 2 で断る）:

  | 引数 | すること |
  | --- | --- |
  | `list <family> <name> json\|handles` | 規則を読む（`nft -j list table`・`nft -a -s list table`。観測用） |
  | `check` / `apply` | 標準入力の規則を確かめる（`nft -c -f -`）・当てる（`nft -f -`） |
  | `arm <秒> [<family>/<name>...]` | 名指しした表（無ければ許した表のすべて）の写しを道具が取り、秒の後に写しへ戻す予約を置いて token（16 進 32 桁）を返す |
  | `disarm <token>` | 予約を取り消す（`disarmed`。もう戻していれば `fired`） |
  | `status` | 道具の名前・取り決めの版（`protocol`）・扱う表・秒の上限・予約の様子を JSON で |

- **扱う表は道具の設定ファイルに書く**（呼び手からは渡さない）。決まった場所 `/etc/sakutei/nft-guard.json`:

  ```json
  {
    "tables": ["inet/filter"],
    "nft": "/usr/sbin/nft",
    "state_dir": "/run/sakutei-nft-guard",
    "max_rollback_seconds": 600
  }
  ```

  `tables` は必須（provider の管理する表と同じものを書く）。`nft`（絶対パス、既定 `/usr/sbin/nft`）・`state_dir`（写しと予約を置く。既定 `/run/sakutei-nft-guard`。道具が 0700 で作る）・`max_rollback_seconds`（`arm` の秒の上限、1〜3600、既定 600）は省略できる。知らないキーは断る。設定ファイル・そのディレクトリ・nft は root の持ち物で group・other が書けないこと、状態のディレクトリは root だけが読み書きできることを道具が毎回確かめ、違えば何もしない（終了コード 3）。
- **規則の中身は構造として読む**：`check`・`apply` の標準入力は、行ごとに `add table`・`add chain`（base chain の `{ type … hook … priority <数> ; policy accept|drop ; }` だけ）・`flush chain`・`delete chain`・`add rule` の 5 つの形だけを受け、どれも許した表のものでなければならない。ルール文は引用符の外の `;`・`#`・`$`・`\`・制御文字・閉じない括弧と引用符を受けない（1 行に 1 つの命令しか書けない）。`flush ruleset`・`include`・`define`・`delete table`・別の表・`table … { }` の塊が **1 行でもあれば、何もせず断る**。Sakutei の流すスクリプトはいつもこの形。
- **写しを取るのも戻すのも許した表だけ**。予約は道具が自分を `setsid` で切り離して起動し直して眠り、秒の後に写しを `nft -f` で流す（syslog の authpriv に残す）。Sakutei との接続が切れても、Sakutei が居なくなっても戻る。取り消しと戻すのは写しのファイルの rename で決着する。起動し直した道具は、予約のディレクトリにだけ置いた nonce を受け取ったときだけ動く（呼び手は `_fire` を流せない）。
- **引数の形・秒の範囲・token の形も道具が確かめる**（秒は 1〜`max_rollback_seconds` の整数、token は小文字の 16 進 32 桁、表は `family/name` で許したもの、同じ表の二度書きも断る）。
- 終了コード: 0 成功、2 断った（何もしていない）、3 道具の設定・状態のディレクトリが使えない、4 それ以外、10 nft が失敗した（標準エラーは nft のもの）。
- **道具が無い・版が合わない**ときは base chain を変えない（一時的な失敗）。Sakutei は base chain を変える前に道具の `status` を読み、名前と取り決めの版（`protocol`）が合うこと、変える表が道具の `tables` にあること、`ruleset_rollback_timeout` が `max_rollback_seconds` に収まることを確かめる。表・秒が道具の設定の外なら変えない（直すのは設定、PermanentError）。道具が断った検査（`check`）は PermanentError。regular chain だけを変える Apply・観測も道具を通るので、道具が無ければ観測できない（unknown）・Apply は一時的な失敗。
- **sudo を使わず root で入る構成**（`user: root`・`sudo: false`）では、今までの動かし方（nft と sh）を残してよい（`ruleset_guard_tool` を書かない）。道具を使っても構わない。

### 入れ方

```console
$ CGO_ENABLED=0 go build -trimpath -o sakutei-nft-guard ./cmd/sakutei-nft-guard
$ sudo install -o root -g root -m 0755 sakutei-nft-guard /usr/local/libexec/sakutei-nft-guard
$ sudo install -d -o root -g root -m 0755 /etc/sakutei
$ sudo install -o root -g root -m 0644 nft-guard.json /etc/sakutei/nft-guard.json
$ sudo /usr/local/libexec/sakutei-nft-guard status
{"tool":"sakutei-nft-guard","protocol":1,"tables":["inet/filter"],"max_rollback_seconds":600,"rollbacks":[]}
```

道具は Sakutei の版と取り決めの版（`protocol`）で合わせる。Sakutei を上げて `protocol` が変わったら道具も入れ直す（合わなければ base chain を変えずに知らせる）。

### sudoers の最小の行

書き込み用の利用者には道具だけを、観測用の利用者（#185）には道具の `list` だけを許す。`nft`・`sh` は許さない:

```
# /etc/sudoers.d/sakutei（visudo -c で確かめる。0440 root:root）
sakutei    ALL=(root) NOPASSWD: /usr/local/libexec/sakutei-nft-guard
sakutei-ro ALL=(root) NOPASSWD: /usr/local/libexec/sakutei-nft-guard list *
```

対象の一覧（[../remote.md](../remote.md)）は今までどおり `sudo: true`（観測用の資格を分けるなら `observe:` に `sakutei-ro`）:

```yaml
# hosts.yaml
hosts:
  fw1:
    address: 192.0.2.10
    user: sakutei
    private_key: secretref:file/ssh/fw1-rw
    sudo: true
    observe:
      user: sakutei-ro
      private_key: secretref:file/ssh/fw1-ro
```

```yaml
# providers.yaml
providers:
  nftables:
    ruleset_target: live
    ruleset_tables: inet/filter
    ruleset_hosts: fw1
    remote_config: hosts.yaml
    ruleset_guard_tool: /usr/local/libexec/sakutei-nft-guard
```

### 配り方（host-file で配る例）

道具の設定ファイルと sudoers の行は [host-file](host-file.md) で配れる。ただし道具に絞った利用者は `/etc` を書けないので、**配る側は別の資格**（段0 の管理者の利用者など、`sudo` を広く持つ別の対象。同じ address を別の名前で対象の一覧に書く）で動かす。道具の本体（静的リンクの数 MiB の実行ファイル）は host-file の中身の上限（256 KiB）を超えるので、パッケージ・イメージ・段0 の Ansible で置く。

```yaml
# 配る側の対象の一覧（hosts-admin.yaml）の fw1-admin は fw1 と同じ address、管理者の利用者
resources:
  - provider: host-file
    id: fw1-admin/etc/sakutei/nft-guard.json
    fields:
      content: |
        {"tables": ["inet/filter"], "max_rollback_seconds": 600}
      mode: "0644"
      owner: root
      group: root
  - provider: host-file
    id: fw1-admin/etc/sudoers.d/sakutei
    fields:
      content: |
        sakutei    ALL=(root) NOPASSWD: /usr/local/libexec/sakutei-nft-guard
        sakutei-ro ALL=(root) NOPASSWD: /usr/local/libexec/sakutei-nft-guard list *
      mode: "0440"
      owner: root
      group: root
```

- 道具の `tables` は provider の管理する表（intent の `providers.nftables.tables`・`ruleset_tables`）と同じにする。道具の側が狭ければ、その表の base chain は変えない（PermanentError で知らせる）。
- sudoers を書き換えるときは、書く前に `visudo -c -f` で確かめる手順を配る側に置く（壊れた sudoers は sudo 全体を止める）。

## 観測用と書き込み用の資格（Backlog #185）

離れたホスト（live）では、対象の一覧のホストに観測用（`observe:`）と書き込み用（`write:`）の資格を分けて書ける（[../remote.md の「観測用と書き込み用の資格」](../remote.md#観測用と書き込み用の資格)）。

- 観測（`Read`・`List`・`BulkObserve`）が流すのは、管理するテーブルごとの `nft -j list table <family> <name>` と `nft -a -s list table <family> <name>` だけ。観測用のユーザーは sudoers でこの 2 つだけを許せば、全部の項目を観測できる（sudo 無しでは CAP_NET_ADMIN が無く観測できない）。
- 書き込み用のユーザーは Apply の中だけで使う：`nft -f -`・`nft -c -f -`、base chain の締め出しの守り（ホストの側の時間切れの巻き戻しを仕掛ける・外す `sh -c` のスクリプト）、新しい接続での到達の確かめ。sudo には `nft` と `sh` が要る。
- 決まった道具（`ruleset_guard_tool`、Backlog #191）を使うホストでは、観測は道具の `list <family> <name> json|handles` だけ、書き込みは道具の `check`・`apply`・`arm`・`disarm`・`status` だけになる。sudoers は観測用に道具の `list *`、書き込み用に道具だけを許せばよい（上の「sudoers の最小の行」）。
- `plan`・`status`・`--mode alert-only` の `reconcile` は書き込み用の鍵が無くても動く。

## 権限

live の Read も Apply も netlink で ruleset を読み書きするので、CAP_NET_ADMIN が要る（ふつうは root で動かす）。権限が無いと Read は観測できない（unknown）になり、drift とも適合とも数えない。network namespace の中の ruleset を扱うときは、その namespace に入った nft を `--ruleset-nft` に渡す（下の統合テストのラッパーと同じ形）。離れたホストを sudo で扱うときは、sudo で許すのを決まった道具だけにする（上の「決まった道具」）。

## 統合テスト

本物の nft を相手にした統合テスト（Backlog #121）。build tag `integration_nftables` の付いたファイルにあり、`make check`・`make test`・CI には入らない（root が要るため）。

```console
$ make integration-nftables
```

要るもの: `nft`・`unshare`・`nsenter`・`docker`・`ssh`・`ssh-keygen`、パスワードなしの `sudo`（docker も `sudo -n docker` で呼ぶ）。締め出しの守りの段は、`make integration-remote` と同じイメージ（`tests/integration/remote/Dockerfile`、sshd・nft・sudo。無ければ作るので初回は apt のミラーに届くこと）を使う。

- `sudo -n unshare -n sh -c 'echo $$; exec cat >/dev/null'` で使い捨ての network namespace を作り、nft をその中で `sudo -n nsenter -t <pid> -n nft ...` として走らせるラッパーを `ruleset_nft` に渡す。sakutei は本物の ExecRunner で本物の nft を呼ぶ。ホストの FW には触れない（作った namespace がホストのものと違うことを最初に確かめる）。namespace は cat の標準入力が閉じれば（テストが終われば、落ちても）消える。
- sakutei のバイナリを作り、`--provider-config`（`ruleset_target: live`・`ruleset_tables: inet/sk`）で `status --json`・`plan`・`reconcile` を回す。
- 管理する table を intent に書く（Backlog #181、`intent_ranges_integration_test.go`）：`--provider-config` に `ruleset_tables` を書かず、intent の `providers.nftables.tables: [inet/ir]` だけで `reconcile` が `inet ir web` を作り、次は `Converged (no changes)`。設定の `ruleset_tables: inet/sk` と食い違えば `plan`・`reconcile` が断り、ruleset は変わらない。設定と intent が同じなら警告は無く、intent から外して設定にだけ残すと警告して `Converged (no changes)`。ほかの table は変わらない。

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

決まった道具（Backlog #191、`guardtool_integration_test.go` の `TestNFTablesGuardToolOverSSH`）：同じイメージの別のコンテナに、CGO なしでビルドした `cmd/sakutei-nft-guard` を `/usr/local/libexec/sakutei-nft-guard`（root・0755）に、`/etc/sakutei/nft-guard.json`（`inet/sk` だけを許す、`max_rollback_seconds: 60`）を置く。**sudoers は上の「sudoers の最小の行」だけ**（書き込み用の `sakutei` に道具だけ、観測用の `sakutei-ro` に道具の `list *` だけ。イメージの `NOPASSWD: ALL` は消す）。sakutei は `ruleset_guard_tool` で道具を使い、対象の一覧の `observe:` は `sakutei-ro`。最小の核（`inet sk mgmt`）と、道具の許さない表 `inet other` を Sakutei の外で入れる。テストの側の ssh は sakutei と別の、新しい接続:

| 段 | 内容 |
| --- | --- |
| sudo runs the tool only | `sakutei`・`sakutei-ro` のどちらでも、`sudo -n sh -c id`・`bash`・`nft list ruleset`・`nft flush ruleset`・`id`・`-s`・`-i`・`env`・`cat /etc/shadow`・`sudoedit`・道具に `sh -c id`・`nft …` を渡すのが通らない（終了コードが 0 でなく、`uid=0` を出さない）。`sakutei` の道具の `status`、`sakutei-ro` の道具の `list inet sk json` は通る。`sakutei-ro` の道具の `apply`・`check`・`arm`・`status`・`disarm` は sudo が断り、ruleset は変わらない |
| the tool refuses and changes nothing | 道具に `flush ruleset`・別の表を足す・許さない表のルール・`delete table`・`;` の後の命令・正しい行の後の `flush ruleset`・正しい行の後の別の表・`include`・`define` と `$x`・`table { }` の塊・`\` の行の続き・base chain の `{ }` の後の `; flush ruleset`・許さない表の `list`・`list ruleset`・知らない形式・`arm 0`・`arm 61`（上限の外）・`arm 100000`・`arm -5`・許さない表の `arm`・同じ表の二度書き・`disarm ../../etc/passwd`・形の外の token・`_fire`・知らない命令・空の引数を流す → どれも終了コード 2。ruleset 全体が変わらず、状態のディレクトリに何も残らない。許した形（引用符の中の `;` を含む comment）の `check` は通る |
| policy drop is applied and confirmed through the tool | lockout と同じ宣言 → reconcile が 0 で上限を待たずに返り、policy drop。上限を過ぎても drop のまま、状態のディレクトリに予約が残らず、sh の仕掛け（`/tmp/sakutei-nft-rollback.*`）は使っていない。`status --json`（観測用の `sakutei-ro` が道具の `list` で読む）は drift 0 |
| a declaration that shuts ssh out is rolled back by the tool | `jump mgmt` を外した宣言 → 4 で「rolled back」、上限の後に返り、table は前の段のまま、新しい接続で届く |
| the host rolls back even when sakutei is gone | 別の、ssh を塞ぐ宣言で reconcile を始め、流れたのを確かめてから sakutei と ssh を SIGKILL → 新しい接続は届かない → 上限の後に道具の予約が写しに戻し、届くようになる。道具の `status` はその予約を `fired` と出す |
| a tool of another protocol: not changed | `status` だけ `protocol: 999` を返す道具に入れ替える → reconcile は Apply の失敗（`protocol 999` を名指し）を報告し、table は変わらない |
| no tool: not changed | 道具を退ける → reconcile は流さず、table は変わらない |
| the tool back: the change goes through | 道具を戻す → 同じ宣言が流れる |
| outside the tool's tables: not touched | `inet sk mgmt` と `inet other` が最初から最後まで変わらない |

網の間の許可（Backlog #174、`netpolicy_integration_test.go` の `TestNFTablesNetworkPolicy`）：上の network namespace の nft に、`network_policy:` の境界（`inet np forward`）とホスト（`inet np input`）を展開して流す（確かめる先は宣言の `tcp://` のテストの待ち受け、承認した planned_change のレーンを置く）:

| 段 | 内容 |
| --- | --- |
| the allows are applied with their reasons as comments | 網 3 つ・許可 5 つ（`"`・`;`・`#`・`\`・日本語を含む reason、128 バイトを超える reason、icmpv6、reason の無い許可、同じ網の中の許可）→ reconcile → 2 つの base chain が policy drop で、ルールの comment に置き換え・切った reason が出る。status は適合 2・drift 0 |
| no drift however many cycles | reconcile と status を 3 周 → drift 0、table（handle 込み）が変わらない（Apply が走らない）。plan は「Converged」 |
| changing the declaration changes the rules | 許可を変える・外す・足す → reconcile → ルールが宣言どおりに変わり、drift 0 |
| an exception without reason is refused | `exception` の印で reason の無い許可 → reconcile が終了コード 1（`exception_without_reason`）、table は変わらない |

手元の live（`TestNFTablesLocalLockout`、上の network namespace の nft、`ruleset_rollback_timeout: 3s`）: 確かめの宛先（`ruleset_confirm_address`）がテストの待ち受けなら policy drop が残る。閉じたポートなら前の確かめで止まり、変えない。設定の宛先を持たず、宣言の `confirm` に `tcp://` の待ち受けを 2 つ書けば確かめられて変わる。2 つのうち 1 つが前の確かめの 1 度だけ受けて閉じる待ち受けなら、後の確かめで届かず 4 で「rolled back」とその先を名指し、table が元に戻る（仕掛けは手元の sh がラッパー越しの nft で流す。手元の TCP は namespace の FW を通らないので、「変えた後に届かない」は待ち受けの側で作る）。

### 結果

nftables v1.0.9 で回した結果（全段 PASS、Backlog #191 の段を足して）:

```
--- PASS: TestNFTablesGuardToolOverSSH (63.57s)
    --- PASS: TestNFTablesGuardToolOverSSH/sudo_runs_the_tool_only (7.72s)
    --- PASS: TestNFTablesGuardToolOverSSH/the_tool_refuses_and_changes_nothing (6.62s)
    --- PASS: TestNFTablesGuardToolOverSSH/policy_drop_is_applied_and_confirmed_through_the_tool (16.18s)
    --- PASS: TestNFTablesGuardToolOverSSH/a_declaration_that_shuts_ssh_out_is_rolled_back_by_the_tool (13.56s)
    --- PASS: TestNFTablesGuardToolOverSSH/the_host_rolls_back_even_when_sakutei_is_gone (11.34s)
    --- PASS: TestNFTablesGuardToolOverSSH/a_tool_of_another_protocol:_not_changed (1.28s)
    --- PASS: TestNFTablesGuardToolOverSSH/no_tool:_not_changed (0.20s)
    --- PASS: TestNFTablesGuardToolOverSSH/the_tool_back:_the_change_goes_through (2.74s)
    --- PASS: TestNFTablesGuardToolOverSSH/outside_the_tool's_tables:_not_touched (0.12s)
--- PASS: TestNFTablesLiveIntentRanges (0.24s)
    --- PASS: TestNFTablesLiveIntentRanges/tables_only_in_the_intent (0.13s)
    --- PASS: TestNFTablesLiveIntentRanges/a_disagreement_is_refused_before_nft_runs (0.02s)
    --- PASS: TestNFTablesLiveIntentRanges/the_same_in_both,_and_only_in_the_config (0.06s)
--- PASS: TestNFTablesLockoutOverSSH (58.56s)
    --- PASS: TestNFTablesLockoutOverSSH/policy_drop_is_applied_and_reaching_the_host_is_confirmed (14.16s)
    --- PASS: TestNFTablesLockoutOverSSH/a_declaration_that_shuts_ssh_out_is_rolled_back_on_the_host (12.46s)
    --- PASS: TestNFTablesLockoutOverSSH/the_same_change_is_not_applied_again (0.13s)
    --- PASS: TestNFTablesLockoutOverSSH/the_host_rolls_back_even_when_sakutei_is_gone (10.63s)
    --- PASS: TestNFTablesLockoutOverSSH/a_target_not_reached_before_the_change:_not_changed (0.74s)
    --- PASS: TestNFTablesLockoutOverSSH/the_other_path_opened_(confirmed_over_self) (2.86s)
    --- PASS: TestNFTablesLockoutOverSSH/another_path_declared_and_kept_open:_confirmed_over_both (1.40s)
    --- PASS: TestNFTablesLockoutOverSSH/another_path_declared_and_shut:_rolled_back_although_self_is_reached (12.76s)
    --- PASS: TestNFTablesLockoutOverSSH/the_minimal_core_was_not_touched (0.07s)
--- PASS: TestNFTablesLocalLockout (8.24s)
    --- PASS: TestNFTablesLocalLockout/confirmed (4.11s)
    --- PASS: TestNFTablesLocalLockout/not_reached_before_the_change:_not_changed (0.09s)
    --- PASS: TestNFTablesLocalLockout/declared_tcp_targets_without_ruleset_confirm_address (0.17s)
    --- PASS: TestNFTablesLocalLockout/one_declared_target_not_reached_after_the_change:_rolled_back (3.87s)
--- PASS: TestNFTablesNetworkPolicy (0.98s)
    --- PASS: TestNFTablesNetworkPolicy/the_allows_are_applied_with_their_reasons_as_comments (0.25s)
    --- PASS: TestNFTablesNetworkPolicy/no_drift_however_many_cycles_(comments_read_back_as_written) (0.40s)
    --- PASS: TestNFTablesNetworkPolicy/changing_the_declaration_changes_the_rules (0.31s)
    --- PASS: TestNFTablesNetworkPolicy/an_exception_without_reason_is_refused_and_nothing_changes (0.02s)
--- PASS: TestNFTablesLiveIntegration (2.24s)
    --- PASS: TestNFTablesLiveIntegration/create_from_empty (0.23s)
    --- PASS: TestNFTablesLiveIntegration/base_chain_and_set_are_not_touched (0.16s)
    --- PASS: TestNFTablesLiveIntegration/a_rule_deleted_by_hand_is_drift_and_comes_back (0.25s)
    --- PASS: TestNFTablesLiveIntegration/a_reordering_is_drift_and_is_put_back (0.18s)
    --- PASS: TestNFTablesLiveIntegration/a_rule_nft_-c_refuses_changes_nothing (0.07s)
    --- PASS: TestNFTablesLiveIntegration/declared_as_listed,_no_drift_however_many_cycles (0.41s)
    --- PASS: TestNFTablesLiveIntegration/normal_form (0.33s)
    --- PASS: TestNFTablesLiveIntegration/a_set_nft_merges_is_not_drift (0.59s)
    --- PASS: TestNFTablesLiveIntegration/the_unmanaged_table_did_not_change (0.01s)
PASS
```

- reconcile は Apply に失敗した resource を「not converged: 1 resource(s) failed to apply」と報告し、終了コードは 0（ほかの provider と同じ）。機械で見るときは `status --json` の `summary.drift` を見る。

## 通常のテスト

nft を呼ぶ所は `nftables.Runner`（seam）に、締め出しの守りは `nftables.Guard`（seam）に切ってあり、`make check` は偽の nft と偽の守り（`internal/nftables/nftfake`。Arm で写しを取り、上限で戻す。届かないかどうかはテストが決める）で回る。仕掛けの sh（`ExecGuard`）は手元の sh と、nft の代わりに写しを書き出すだけのスクリプトで、取り消し・時間切れの戻し・残りが無いことを確かめる。決まった道具（Backlog #191）は、道具の中身を `internal/nftguard/nftguard_test.go`（nft の代わりに呼ばれ方と標準入力を書き出すスクリプト。受ける形・断る形と nft を呼ばないこと・写しの範囲・予約と取り消しと時間切れの戻し・引数と設定の検査）で、provider の側を `internal/nftables/tool_test.go`（偽のホストの上の偽の道具。道具の list・check・apply・arm・disarm・status だけを呼ぶこと・道具が無い・版が合わない・表と秒が合わないときに変えないこと）と `tests/acceptance/remote_test.go`（`ruleset_guard_tool` の設定の検査）で確かめる。base chain と守りの試験は `internal/nftables/basechain_test.go`・`guard_test.go`、ループが巻き戻された Apply を次の周に繰り返さないことは `tests/acceptance/nftables_lockout_test.go`、確かめる先の宣言・前の確かめ・承認と予算は `tests/acceptance/nftables_approval_test.go`（Backlog #179。偽の守りは先ごとに届くかをテストが決める）。本物の nft の出力を記録したもの（`internal/nftables/testdata/`）を読む試験、契約テスト（`sdk.ContractTester`、観測できないのを absent にしないことを含む）、BulkObserve 等価性テスト（`sdk.BulkEquivalenceTester`）を live でも通す（`tests/acceptance/nftables_live_test.go`）。
