# CHANGELOG v3.35 → v3.36

**改訂日: 2026-10-28**

触らない宣言（§7.3 external_owner）を intent に汎用で入れた改訂である（Backlog #158。親のストーリー「正しい状態は
宣言に置く：採番の結果を宣言の側へ書き戻す・触らない宣言を intent に入れる」の 2）。あわせて、原則「provider の設定は
つなぎ方だけ、管理の範囲と除外は intent」を §1.7・§29.1 に書いた。

## #158：触らない宣言を intent に（§7.3（c）・§7.4・§1.7・§29.1）

- **背景**: §7.3 は「この範囲は外の持ち主のもの、Sakutei は観測しても Apply しない」を宣言する `external_owner` を持つが、
  loader に実装が無かった。除外は provider ごとの設定（proxmox-vm の `protected_vmids`・`protected_names`）にしか書けず、
  「触らない」という正しい状態が宣言の外にあった（§1.7）。
- **決定**:
  - 書き方：intent のトップレベルの `external_owners:`。1 件は `owner`（必須）と選び方——`provider`・`scope`（その scope と
    その下）・`id`（完全一致）か `id_prefix`（前方一致）・`fields`（観測した値の完全一致）。項目は AND、空は「何でもよい」。
    provider も空でない scope も無い 1 件は断る（世界の全部を渡してしまう）。`id`・`id_prefix`・`fields` は provider と書く。
  - `fields` で選べるのは provider が申告したフィールドだけ（新しい任意の口 `provider.OwnerFieldSelector`。proxmox-vm は
    `vmid`・`node`・`pool`）。それ以外は読み込みで断る。
  - 効き方：選ばれたものは観測するが Apply・Delete・prune をしない。prune は集合差の候補から外す（ID で選ぶ宣言は Read の
    前に、`fields` で選ぶ宣言は Read して値を見てから）。intent が宣言したものの観測した値が `fields` に合えば Apply せず
    知らせ、下流を待たせる（観測と ready の記録はするので「観測して待つ」はそのまま）。status の宣言外（unmanaged）には
    ID で選ぶ宣言のものを出さない。
  - 読み込みで断る：intent が同じものを `resources:` に宣言する・`ensure: absent` で消すと断る（`fields` で選ぶ宣言は、
    intent がそのフィールドを同じ値で宣言したとき）。loader と、各経路（plan・reconcile・status・常駐）の provider を
    組み立てた入口（render のあとの desired）の両方で検査する。inventory のファイルには書けない。
  - provider の設定の除外は最後の安全の網として残す：新しい任意の口 `provider.ConfigExcluder`（`ConfigExclusions()`）で
    同じ選び方の形に直して申告し、コアが突き合わせる。intent が設定で除外したものを管理すれば読み込みで断る（弱い側に
    寄せない。intent の `external_owners:` に書いてあっても断る）。設定にだけある除外は intent にも書くように警告する
    （常駐では同じ警告は一度だけ）。proxmox-vm は `protected_vmids` を「`<クラスタ>/` の前方一致 + `vmid`」、
    `protected_names` を「`<クラスタ>/<名前>` の完全一致」として申告する。
  - 原則（§1.7・§29.1）：provider の設定はつなぎ方（宛先・鍵・時間）だけにし、管理の範囲と除外は intent に書く。既存の
    provider の設定にある範囲の設定は後で順に移す。
- **実装の形**: 選び方と照合は `internal/extowner`（`Selector`・`Set`・`Check`）。prune エンジンは `SetExternalOwners` で
  受け取る。読み込みの検査は loader（`extowner.ValidateDesired`）と各経路の入口（`extowner.Check`）。
- **見送り**: status の宣言外の検出で `fields` で選ぶ宣言のものを外すこと（List の結果を 1 件ずつ Read することになる）。
  provider の設定の範囲の設定（nftables の live 版の `ruleset_tables` など）を intent へ移すこと（provider ごとに順に）。

## 受け入れテスト

- `tests/acceptance/external_owner_test.go`：
  - `[AC-158-1]` 読み込みで断る：intent が external_owners の選ぶもの（ID・scope・前方一致・宣言した値）を宣言する・
    absent で消す。何も選ばない 1 件。provider が申告していないフィールド。
  - `[AC-158-2]` prune：ID で選ばれた孤児と、観測した値で選ばれた孤児は、plan・reconcile・常駐で消えない（宣言が無ければ
    消える）。status は宣言外に出さない。
  - `[AC-158-3]` Apply：宣言していない値が観測で選ばれたリソースは、reconcile・常駐で Apply されない（宣言が無ければ
    Apply される）。
  - `[AC-158-4]` proxmox-vm：設定にだけある除外は警告になり、intent に書けば消える。intent が vmid で選んだ VM は Apply
    されず、それを同じ vmid で宣言すると断る。設定で除外した VM を intent が管理すると（intent の external_owners の有無に
    よらず）断る。
- `internal/extowner`：設定の除外の覆い方（広い前方一致は覆う・別の ID は覆わない）・食い違いの断り・観測した値での選び方。
