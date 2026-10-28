# CHANGELOG v3.37 → v3.38

**改訂日: 2026-10-28**

List が返す ID の scope と宣言の scope の食い違いで、status の宣言外（unmanaged）と prune の集合差の突き合わせが外れて
いたのを直した改訂である（Backlog #169）。

## #169：List の ID の scope を宣言に合わせてから突き合わせる（§5.4・§12.2・§12.4（d）・§26.4）

- **背景**: List を持つ provider（proxmox-vm・postgresql・s3-bucket・ldap・dnszone・nftables など）は、List・BulkObserve の
  ID に provider の設定の scope（proxmox-vm のクラスタの `scope`、`--zone-scope`、`--ruleset-scope`、s3-bucket の Scope。無ければ
  root）を付け、宣言の scope とは独立に決まる。宣言に `scope: {node: mgmt}` と書き provider に scope を書かないと：
  - `status --claim node=mgmt`：List の ID は root なので claim に入らず、宣言の外のもの（手で作った VM など）が出ない。
  - `status`（claim なし）：宣言済みのものまで unmanaged に出る。
  - prune：claim ありでは root の ID が claim の外に落ちて孤児を取りこぼし、ID で突き合わせる経路では宣言済みのものが孤児に
    見えうる。
  BulkObserve の経路には「宣言の scope で無ければ root の ID で引く」逃げ道があったが、List の側には無かった。
- **決定**（provider に依らず本体の 1 か所、`internal/listscope`）：**宣言にあるものの scope は宣言が正**（§1.7）。
  1. 宣言にあるものは宣言の ID に合わせる：List の ID が root で、同じ provider・local の宣言がちょうど 1 つあれば、その宣言の
     ID（scope ごと）に置き換える。
  2. 食い違いは止める：List の ID が root でなく同じ provider・local の宣言の scope と違う、または root の ID に scope 違いの
     宣言が 2 つ以上あるとき。status はその provider の宣言外を「検出不可（scope の食い違い：provider は X、宣言は Y）」と
     注記し（List のエラーと同じく終了コード 3、JSON の `kind: scope_mismatch`）、prune はその provider の prune を止める
     （`PruneWithinScope` の宣言があるときだけ。弱い側に寄せない）。
  3. 宣言に無いもの（status だけ）：root の ID は、その provider の宣言（claim で絞る前の intent 全体、合成の親は除く）の
     scope がちょうど 1 種類で root でなければその scope に入れる。宣言が 0 件か 2 種類以上なら root のまま。そのうえで
     `--claim` があれば、claim の内外を決められない件数を「検出不可（scope が決まらない N 件。provider の scope の設定か
     宣言で決める）」と注記する（終了コードは変えない、JSON の `kind: scope_undecided`）。claim の外のものは今までどおり
     出さない。
  4. prune では 3 の推定をしない（確信なき減算はしない）。claim で絞った desired と突き合わせ、claim の外の宣言に合うものは
     今までどおり claim の外に落ちる。
  5. status の「宣言済み」（宣言外にしない集合）は claim で絞る前の宣言全体で決める（ほかの shard の宣言は宣言外ではない）。
     候補は今までどおり claim で絞る。
- **実装の形**: `listscope.Align`（合わせると食い違いの検出）・`listscope.InferScope`（宣言からの推定）。status は
  `detectUnmanaged`、prune は `prune.Engine.ComputePrune` の中（claim の境界と集合差の前）で使うので、`plan`・`reconcile`・
  常駐のすべてに効く。
- **見送り**: provider の設定の scope を intent へ移すこと（§1.7 の「provider の設定はつなぎ方だけ」に沿って provider ごとに順に）。

## 受け入れテスト

- `tests/acceptance/list_scope_align_test.go`（fake provider。List が root を返す、どの provider にも当たる形）：
  - `[AC-169-1]` scope 付きの宣言と `--claim`：宣言に無いものだけが unmanaged に出て、claim の外は出ない。宣言の scope が
    2 種類なら決まらない件数を注記する。
  - `[AC-169-2]` claim なし：宣言済みは unmanaged に出ない。
  - `[AC-169-3]` provider の scope と宣言の scope が食い違う：検出不可の注記（終了コード 3）と、prune が止まる。
  - `[AC-169-4]` claim なし（とあり）の `PruneWithinScope`：scope 付きの宣言済みのものが削除の候補にならない。
- `internal/listscope`：合わせる・食い違い・推定・決まらない。`internal/prune`：claim のもとで合わせる・食い違いで止まる・
  宣言済みが候補にならない。
- `make integration-proxmox` の「scope 付きの宣言と --claim（provider の側に scope を書かない）」段：API で直接作った VM だけが
  unmanaged に出て、宣言した VM は出ない（claim あり・なし）。
