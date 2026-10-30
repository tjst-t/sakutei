# CHANGELOG v3.75 → v3.76

**改訂日: 2026-10-30**

採番のプールに、区切った範囲（block）と、上がるだけで再利用しない設定を足した改訂である（Backlog #206）。

## #206：`blocks:`・`block:` と `reuse: false`（§19.8、§19.3・§19.5・§19.7・§19.9）

- **背景**: 外部の利用者が v2.8.0 を使い、利用者の番号（uid など）を用途ごとの範囲に分けたい、一度払った番号は解放しても
  二度と払いたくない、という足りない点が出た。今のプールは範囲が 1 つで、解放した値は cooldown（既定 24h）のあとに
  再び払う。cooldown を長くしても、store を失えば cooldown も失い、書き戻しから作り直すと解放した値を払い直しうる。
- **決定**（§19.8「区切った範囲（block）と、上がるだけで再利用しない設定」）:
  - プールに `blocks:`（`name`・`range`）を書ける。block はプールの範囲の中に収まり、互いに重ならない。block を持つ
    プールへの要求は `block:` で 1 つを名指しする。名指しの無い要求・宣言に無い block・block の無いプールへの
    `block:`・重なる block・範囲の外の block・`/` を含むプール名と block 名はロード時に断る。
  - 値は名指しした block の中からだけ払う。block が尽きたら他の block へはみ出さず、その要求を理由（block の名前と範囲）
    つきの waiting-allocation にする。plan・status は block を `<pool>/<block>` と出し、空きは block ごとに数える。
  - authority への要求は pool の引数を `<pool>/<block>` にして block を伝える（`allocation.BlockRef`）。
    `AllocationAuthority` の interface は変えない。プールの宣言は範囲・block・reuse を `allocation.PoolSpec` で渡す
    （`DeclarePool`、サービスの `POST /v1/ensure-pool` の `spec`）。
  - `reuse: false`（既定 `true`）は block ごとに最後に払った値を記録し、その上からだけ払う。解放した値は cooldown に入れず
    退役（`retired`）させ、二度と払わない。**再利用しないので cooldown は意味を持たず、使わない。** 範囲の上まで払えば、
    下に空きがあっても枯渇として理由（最後の値）つきで止める。`reuse: true` から `false` に変えると、最後の値は使用中・
    cooldown 中の値の最大から始まる。
  - 最後に払った値は正しい状態として宣言の側に置く：`write_back: true` のプールは、採番の書き戻しのファイルに `last:`
    （block ごとの値）を載せ、解放してもこれを下げない。`sakutei allocator import` は store の最後の値をファイルの最後の値
    と取り込んだ値の大きい方へ上げ、退役した値・どの block にも入らない値・宣言に無い block の最後の値は断る。store の
    最後の値がファイルより小さいあいだは、そのプールから新しい値を配らず import を案内する。
  - `sakutei allocator list` は最後の値（状態 `last`）と退役した値（状態 `retired`）を出し、`release` は退役したことを
    1 行で出す。
- **見送り**: block をまたぐ払い出し（ある block が尽きたら次へ、という順の宣言。用途ごとに分けた意味が薄れ、需要が
  まだ無い）。払ったあとに要求の `block:` を書き換えたときの払い直し（払い出しは冪等で値は動かない。動かしたければ
  解放して払い直す）。`write_back` の無い `reuse: false` のプールの最後の値の保全（store が正のプールの喪失は事故のまま、
  §25.3。再利用が事故になる番号は `write_back: true` と組み合わせる）。
