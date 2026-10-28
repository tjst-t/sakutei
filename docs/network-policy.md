# 網の間の許可（network_policy）

網と網の間の通信の許可を、機器に依らない宣言として intent に書く（Backlog #174）。Sakutei は宣言を検査し、実現先（境界の FW・各ホスト）ごとに、そこを通る許可だけを nftables の base chain の宣言へ展開して収束させる。決まりの正は設計書 §7.6 と §29.1「網の間の許可を抽象のリソースにする」。ここは書き方の説明で、矛盾したら spec/ が優先する。

- **既定は拒否**。書いた許可だけが通る（展開した base chain の `policy` は既定 `drop`）。
- 許可の行ごとに **なぜ許すか（`reason`）** と **印（`tags`）** を書く。reason は検査の結果・plan・nftables のルールの comment に出る。
- 正しい状態は宣言に置く（§1.7）。どの許可をどの実現先に出すかは intent に書き、provider の設定（`ruleset_target`・`ruleset_hosts`・鍵・時間）はつなぎ方だけを持つ。

## 書き方

```json
{
  "network_policy": {
    "networks": [
      {"name": "office", "cidrs": ["10.1.0.0/24", "2001:db8:1::/64"]},
      {"name": "dmz",    "cidrs": ["192.0.2.0/24", "2001:db8:2::/64"]},
      {"name": "db",     "cidrs": ["10.2.0.10", "10.2.0.11"]}
    ],
    "allows": [
      {"from": "office", "to": "dmz", "proto": "tcp", "ports": [80, 443], "reason": "社内から公開の web を見る"},
      {"from": "dmz",    "to": "db",  "proto": "tcp", "ports": [5432],    "reason": "web から DB"},
      {"from": "office", "to": "db",  "proto": "tcp", "ports": [23], "tags": ["exception"],
       "reason": "古い装置は telnet しか話さない（2027 年に入れ替える）"}
    ],
    "targets": [
      {"name": "fw1", "kind": "boundary", "networks": ["office", "dmz", "db"],
       "id": "fw1/inet/filter/forward", "scope": {"site": "lab"},
       "before": ["ct state established,related accept"]},
      {"name": "db1", "kind": "host", "networks": ["db"],
       "id": "db1/inet/filter/input", "scope": {"site": "lab"},
       "before": ["ct state established,related accept", "iif \"lo\" accept", "jump mgmt"],
       "settings": {"confirm": "self"}}
    ]
  },
  "planned_change": [
    {"scope": {"site": "lab"}, "kind": "create", "max_count": 2,
     "window": "2026-11-01 01:00 .. 2026-11-01 03:00", "approval": {"pr": 174, "approvers": ["alice"]}}
  ]
}
```

複数のファイルに分けて書ける（網を 1 つのファイルに、許可を別のファイルに、など）。許可の順はファイルの名前の順・ファイルの中の順。

### networks（網）

| キー | 値 |
| --- | --- |
| `name` | 網の名前（英数字・`_`・`.`・`-`）。二度書くと断る |
| `cidrs` | CIDR の一覧（1 つ以上）。IPv4 と IPv6 を混ぜてよい。アドレスだけなら /32・/128。ホストのビットの立った CIDR（`10.0.0.5/24`）は断る |

**網の CIDR は重ねられない**（網の中でも網の間でも）。アドレスがどちらの網か決まらず、片方への許可がもう片方にも漏れるため。

### allows（許可）

| キー | 値 |
| --- | --- |
| `from`・`to` | 網の名前（要る）。宣言に無い網は断る |
| `proto` | `tcp`・`udp`・`icmp`（IPv4 だけ）・`icmpv6`（IPv6 だけ）・`any`。既定 `any` |
| `ports` | 宛先のポート。数（`443`）か範囲（`"8000-8100"`）の一覧。`tcp`・`udp` のときだけ。書かなければすべてのポート |
| `reason` | なぜ許すかの文。印の無い許可では任意 |
| `tags` | 印（文字列の一覧）。`exception` は「弱い方式をその機器だけ例外として許す」行で、**reason が要る**（無ければ読み込みで断る） |

### targets（実現先）

| キー | 値 |
| --- | --- |
| `name` | 実現先の名前 |
| `kind` | `boundary`（網と網の間の FW。`networks` の違う 2 つの網を from・to とする許可が通る）か `host`（そのホストに入る通信。to が `networks` にある許可が通る。同じ網の中からの通信も通る） |
| `networks` | boundary はつなぐ網（2 つ以上）、host はホストのいる網 |
| `realizer` | 変換の名前。既定 `nftables`（本体にあるのはこれだけ。下の「別の実現先へ変換する」） |
| `id`・`scope` | 作る resource の id と scope。nftables は nftables provider の local id（`[<host>/]<family>/<table>/<chain>`、family は `ip`・`ip6`・`inet`） |
| `before`・`after` | 許可の前と後に置くルール（1 要素に 1 ルール）。nftables なら `ct state established,related accept`・`iif "lo" accept`・最小の核への `jump mgmt` |
| `settings` | realizer に固有の値。nftables は `hook`（既定 boundary は `forward`、host は `input`）・`priority`（既定 `filter`）・`policy`（既定 `drop`）・`confirm`（base chain の確かめる先、[providers/nftables.md](providers/nftables.md) の「確かめる先を宣言する」） |

host の実現先で `policy drop` の input を作ると、Sakutei の管理の経路（ssh）も宣言どおりにしか通らない。**到達性の最小の核は Sakutei の外で保守し**、`before` にその chain への `jump` を書く（[providers/nftables.md](providers/nftables.md) の「到達性の最小の核は Sakutei の外で保守する」）。

## 検査

読み込み（`plan`・`reconcile`・`status`・常駐のすべて）の時点で次を確かめる。

| 結果 | 重さ | 内容 |
| --- | --- | --- |
| `overlap` | 断る | 網の CIDR の重なり（網の中も網の間も） |
| `undeclared_network` | 断る | 宣言に無い網を指す許可・実現先 |
| `exception_without_reason` | 断る | `exception` の印の付いた許可に reason が無い |
| `duplicate` | 断る | 同じ許可（同じ from・to・proto・ポート）の二度書き |
| `duplicate_name`・`target` | 断る | 網・実現先の名前の二度書き、網が 2 つ未満の境界、網の無いホスト |
| `shadowed` | 知らせる | 前の許可に覆われて効かない許可（同じ from・to で、前の許可の proto とポートが全部を含む） |
| `unreached` | 知らせる | どの実現先も通らない許可 |

断るものは全部を並べて終了コード 1 で止まる（`plan --json` は `error` に入る）。結果には行の場所と reason・tags が付く:

```
sakutei plan: load intent: loader: network_policy: 1 problem(s):
  - error: exception_without_reason: net.json: network_policy.allows[2] (office → db tcp 23): an allow tagged exception must say why (reason): an exception is declared with its reason — reason: (none); tags: exception
```

知らせるものは `plan` の展開の節と `--json` の `network_policy.findings`、一回性の `reconcile` の標準エラーに出る:

```
warning: shadowed: net.json: network_policy.allows[1] (office → dmz tcp 443): has no effect: net.json: network_policy.allows[0] (office → dmz tcp 80,443) already allows all of it (reason: "社内から公開の web を見る") — reason: "web の https"
```

## 展開（nftables）

実現先 1 つが nftables の base chain 1 つになる。ルールは `before`・許可・`after` の順で、許可 1 つをアドレスの族ごとに 1 ルールにする（from と to の両方にその族の CIDR があるときだけ。family `ip` は IPv4 だけ、`ip6` は IPv6 だけ）。`sakutei plan` は実現先ごとに、通る許可・reason・tags・なったルールを出す:

```
Network policy (§7.6): 3 network(s), 3 allow(s), 2 target(s)
  target fw1 (boundary, nftables) → 3 allow(s) nftables/fw1/inet/filter/forward
    allow office → dmz tcp 80,443 (net.json: network_policy.allows[0])
      reason: "社内から公開の web を見る"
      → ip saddr 10.1.0.0/24 ip daddr 192.0.2.0/24 tcp dport { 80, 443 } accept comment "社内から公開の web を見る"
      → ip6 saddr 2001:db8:1::/64 ip6 daddr 2001:db8:2::/64 tcp dport { 80, 443 } accept comment "社内から公開の web を見る"
    ...
    allow office → db tcp 23 (net.json: network_policy.allows[2])
      reason: "古い装置は telnet しか話さない（2027 年に入れ替える）"
      tags:   [exception]
      → ip saddr 10.1.0.0/24 ip daddr 10.2.0.10/31 tcp dport 23 accept comment "古い装置は telnet しか話さない（2027 年に入れ替える）"
```

その後に、展開した base chain の差分（`+ CREATE nftables/site=lab/fw1/inet/filter/forward` など）がふつうの resource と同じに出る。

- ルールの形は nft が書き戻す形（/32 はアドレスだけ、ポートがあれば `tcp dport`、無ければ `meta l4proto tcp`、icmpv6 は `meta l4proto ipv6-icmp`）。集合は nftables provider の正規形（`NormalizeSets`、隣り合う範囲をまとめる）で作るので、nft がまとめて書き戻しても drift にならない。
- **comment**：nft の comment は 128 バイトまでで、`"` を逃がせない。reason の `"` は `'` に、`\` は `/` に、`;` は `,` に（ルールの区切り ` ; ` を作らない）、改行・タブ・続く空白は 1 つの空白にし、128 バイトを超えれば文字の境目で切って `…` を付ける。reason が空なら comment を付けない。読み戻しでも同じ文字列になる（統合テストで確かめた）。
- 展開した resource は手で書いた resource と同じに扱う：同じ id を resources にも書く・`external_owners:` が選ぶ・inventory のファイルに `network_policy:` を書くと断る。base chain を作る・policy を変えるのは承認の要る壊す変更（#179）なので、live では planned_change のレーン（`kind: create`・`kind: update`）が要り、締め出しの守り（#173）を通る。ルールだけの変更（許可を足す・reason を変える）は承認は要らず守りを通る。
- file 版（`--ruleset`）でも同じ宣言が使える（FW は変えない）。

## 別の実現先へ変換する（展開の口）

本体の変換は nftables だけ。スイッチの ACL などへの変換は、使う側が `pkg/cli` の `WithRealizer` で足した自前のバイナリ（[§28.9](architecture/spec/part8-implementation.md) の外の provider と同じ作り方、`examples/external-provider/`）で持つ:

```go
package main

import (
	"github.com/tjst-t/sakutei/v2/pkg/cli"
	"github.com/tjst-t/sakutei/v2/pkg/netpolicy"
)

func main() {
	cli.Main(
		cli.WithProviders(registerSwitchACLProvider),
		cli.WithRealizer("switch-acl", netpolicy.RealizerFunc(
			func(t netpolicy.Target, allows []netpolicy.Passing) (netpolicy.Output, error) {
				var out netpolicy.Output
				// allows はこの実現先を通る許可だけ（宣言の順）。p.Allow.Reason・p.Allow.Tags・
				// p.From.CIDRs・p.To.CIDRs・p.Allow.Proto・p.Allow.Ports から ACL を作る。
				for _, p := range allows {
					out.Rules = append(out.Rules, netpolicy.Rule{Allow: p.Allow.Index, Resource: t.ID, Text: "..."})
				}
				out.Resources = append(out.Resources, netpolicy.Resource{
					Provider: "switch-acl", ID: t.ID, Scope: t.Scope, Fields: map[string]string{ /* ... */ },
				})
				return out, nil
			})),
	)
}
```

target に `"realizer": "switch-acl"` を書くと、その実現先はこの変換を通る。`Output.Rules` は plan の展開に出る。同じ入力には同じ出力を返す（決定的）。すでにある名前（`nftables` を含む）は起動のエラー。

## 見送り

- スイッチの ACL への変換（使う側の拡張）。
- 網の重なりを許す「外の網」（`0.0.0.0/0` など）。いまは重なりとして断る。外は具体の CIDR で書く。
- 送り元のポート・接続の状態を許可に書くこと（状態は実現先の `before` に書く）、拒否の宣言（既定の拒否だけ）。
