# CHANGELOG v3.52 → v3.53

**改訂日: 2026-10-28**

provider の設定に残っていた管理の範囲を intent に移す共通の口の改訂である（Backlog #181。親のストーリー「M7：本番に向けた固め」の
A3 の前半）。設計の向きは変えていない（正しい状態は宣言に置く、§1.7。#158 の「provider の設定はつなぎ方だけ」の続き）。

## #181：provider の範囲を intent に書く（§7.7・§1.7・§5.4・§29.1）

- **背景**: #158 で「provider の設定はつなぎ方（宛先・鍵・時間）だけ、管理の範囲と除外は intent」と決め、除外は
  `external_owners:` に書けるようにした。範囲——proxmox-vm のクラスタの `scope`、dnszone の `zone_scope`、nftables の
  `ruleset_scope` と live の `ruleset_tables`（管理する表）——は provider の設定にしか書けず、git でレビューされる宣言の外にあった。
- **決定**:
  - intent のトップレベルの `providers:` に、provider のインスタンス（名前）ごとに範囲を書く：`dnszone.scope`・
    `nftables.scope`・`nftables.tables`（live だけ）・`proxmox-vm.clusters.<名前>.scope`。scope はラベルの map（resources の
    scope と同じ形）。
  - 除外は `external_owners:`（§7.3（c）、#158）に寄せ、`providers:` には書かない。proxmox-vm の `protected_vmids`・
    `protected_names` は #158 のとおり設定の網として残り、intent と突き合わせる。
  - 共通の口：provider は範囲の読み方を `provider.IntentRanges`（`FromIntent`・`FromConfig`・`Apply`）として registry に
    factory と並べて登録する（`RegisterIntentRanges`）。`provider.MergeRanges` が正規形で突き合わせる：設定にだけあれば警告して
    今どおり読み、intent にだけあれば intent の値で組み立て、両方にあって違えば読み込みで断る（弱い側に寄せない）。
  - 知らない provider・範囲を受け取らない provider・知らないキー・設定に無いクラスタ・同じ provider の二度書き・inventory の
    `providers:`・file の対象の nftables の `tables` は読み込みで断る。
  - 経路：`plan`・`reconcile`・`status`・`graph`・`sync`（起動のときに読んだ intent）と常駐。常駐は起動で断り、周の途中で intent が食い違うように変われば周を飛ばし、
    範囲の宣言が変わった provider は組み立て直す（`reconcile.Config.IntentProviders`）。
- **見送り**:
  - 設定から範囲を外す（設定に書けなくする）こと。次のメジャー版で行う。
  - ほかの provider（s3-bucket・postgresql の `scope` など）の範囲の移し（A3 の後半）。

## 影響

- §7.7 を新設し、§1.7・§5.4 に書き足し、§29.1 に 1 行を足した。spec/README.md の §7 の要約に足した。
- 公開の口 `provider.RangeSetting`・`provider.IntentRanges`・`provider.MergeRanges`・`provider.IntentKeys`・`provider.IntentScope`、
  `registry.RegisterIntentRanges`・`IntentRangesOf`・`CheckIntentProviders`・`GetWithIntent` を足した（足しただけ）。
- `cli.WithProviders` で足した registry の `IntentRanges` も取り込む。
- docs/providers/dnszone.md・nftables.md・proxmox-vm.md に「範囲を intent に書く」を足した。
