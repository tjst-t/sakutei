# CHANGELOG v3.74 → v3.75

**改訂日: 2026-10-30**

同じ intent の中で、常駐の役ごとに `runtime:` の値を宣言できるようにした改訂である（Backlog #213）。

## #213：`runtime.roles:` と `--runtime-role`（§26.7）

- **背景**: 同じ宣言に対して、ふだんの常駐（`mode: alert-only`）と、一部の台だけを claim して一時的に `mode: auto` で動く常駐
  （例：OS の導入の間だけ）を並べたいことがある。旗で `--mode auto` を渡すのは緩める向きなので起動を断られ（v3.52 の決まりどおり）、
  回避には宣言を写した別の intent が要り、宣言が 2 つに分かれていた（§1.7）。
- **決定**（§26.7「常駐の役ごとの値」）:
  - `runtime:` の下に `roles:`（役の名前 → `runtime:` と同じ欄）を書ける。常駐は起動の旗 `--runtime-role <name>` で役を 1 つ選ぶ。
  - 効く値はキーごとに、役の欄 → `daemon:` → 共通の `runtime:` の順。役は常駐の値なので `interval`・`stuck_after` も書ける。
    一回性の `reconcile`・`plan` には効かない（一回性は `oneshot:`）。
  - 役の値は宣言なので、共通の値より緩くても断らない（役を足す・変えるのは intent の変更）。旗との突き合わせは役に効く値に対して
    v3.52 の決まりをそのまま当てる（締める旗は `RuntimeTightened`、緩める旗・向きの無い食い違いは断る。どちらも欄
    `runtime.roles.<name>.<key>` を指す）。
  - 宣言に無い役（`runtime:`・`roles:` が無いときを含む）で起動すれば、宣言にある役と直し方を 1 行で出して終了コード 1 で断る。
    役を選んだのに起動のとき intent が読めないときも断る。役の名前の形の外・知らないキー・役の中の `daemon:`・`oneshot:`・`roles:`
    は読み込みで断る。
  - 常駐の status に `runtime_role`（`sakutei status` では「runtime の役」の行）を、S4 監査の各記録に `runtime_role` を出す。
  - 起動したあとに役の値が変わる・役が宣言から消えたときは `RuntimeDeclarationChanged`（役が消えたときは attrs の `role`）で知らせ、
    起動したときの値で回り続ける。
- **見送り**: 一回性の `reconcile`・`plan` の役（一回性の値は `oneshot:` で足り、一回性を役ごとに分ける需要がまだ無い）。
  役ごとの既定の claim（claim は `--claim`・git の claim の宣言のまま。役は回り方の値だけを持つ）。
