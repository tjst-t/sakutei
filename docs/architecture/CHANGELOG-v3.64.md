# CHANGELOG v3.63 → v3.64

**改訂日: 2026-10-29**

network_policy の網に domain（アドレスの空間）を足し、実現先に口（interface）ごとに見る domain を書けるようにした改訂である（Backlog #195。親のストーリー「アドレスの重なる網を宣言から守る（網の domain・VM の FW・Proxmox の FW の実現先）」の最初のタスク）。

## #195：網の domain と実現先の口（§7.6・§29.1）

- **背景**: v3.51 の network_policy（§7.6）は網の CIDR の重なりを全部の網の間で断り、実現先は網を名前だけで持った。利用者ごとに網を分けてアドレスの重なりを許し、利用者ごとに置く関所の VM（片側の口は利用者の網、もう片側はサービスの網）の口ごとに守る形を、宣言で書けなかった。
- **決定**:
  1. **網の `domain:`**：書かなければ既定の domain `default`。domain を書かない宣言の展開（作る resource・plan の文）は v3.63 と同じ。網の名前は domain をまたいで一意（許可は網の名前で指し、許可の domain は網から決まる）。
  2. **重なりは同じ domain の中でだけ断る**。違う domain の網は同じ CIDR を持ってよい。
  3. **domain をまたぐ許可は読み込みで断る**（検査の種類 `cross_domain`）。2 つの domain をつなぐ所（関所の VM）は、その所の各 domain の側のアドレスを網として宣言し、片側ずつの許可として書く。
  4. **実現先の口**：target の `interfaces:`（`name`・`domain`〈要る〉・`networks`〈書かなければ実現先の `networks` のうちその domain のもの全部〉）。1 つの実現先が複数の domain の口を持てる。口の名前の二度書き・網の無い domain・実現先に無い網・口の domain に無い網・2 つの口に載る網・どの口にも載らない網は読み込みで断る。**口を書かない実現先は既定の domain の網だけを見る**（既定の外の網を名指せば断る）。
  5. **向き**：口を書いた実現先では、許可ごとに通る口と向きを `Passing.Via`（`Interface`・`Domain`・`Direction`）に、許可の domain を `Passing.Domain` に入れて realizer に渡す。境界は from の網の口で `in`・to の網の口で `out`、ホストは to が自分の網なら `in`・from が自分の網なら `out`（口を書いたホストは出る向きの許可も通る）。口ごとに入りと出に分けた組は `netpolicy.ByInterface`。`Realizer` の形は変えないので、外の realizer（`cli.WithRealizer`）にも同じに渡る。
  6. **本体の nftables の realizer は口を受けない**（断る）。口ごとの規則にする実現先（VM の口ごとの FW）は別の realizer で足す。
  7. **plan**：文の出力は口（`interface <名前> (domain <d>): [網]`）と、既定でない domain（`domain:`）・向き（`via:`）を出す。`--json` は許可の `domain`・`via` と実現先の `interfaces` を持つ（足しただけなので版は上げない）。
- **見送り**: VM の FW の実現先（proxmox-fw）と proxmox-vm の NIC の `firewall`・`mtu`・VM の FW の options は、同じストーリーの別のタスク。機種に固有の変換（スイッチの ACL）は使う側の `WithRealizer`。domain をまたぐ通信の宣言（NAT の写し）は持たない（片側ずつの許可で書く）。
- **互換**: domain も interfaces も書かない今までの宣言は、読み込みの結果・展開・plan の文が同じ（`--json` の許可に `"domain": "default"` が足される）。
- 実装：`pkg/netpolicy`（`Network.Domain`・`Target.Interfaces`・`Passing.Domain`・`Passing.Via`・`ByInterface`・検査）・`internal/loader` の JSON Schema・`pkg/cli/netpolicy.go`・`internal/planreport`。受け入れ `tests/acceptance/netpolicy_domain_test.go`（`[AC-195-1-1]`〜`[AC-195-1-4]`）。利用者の文書は `docs/network-policy.md`・`docs/plan-json.md`。
