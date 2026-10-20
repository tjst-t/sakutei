# Sakutei 設計書 v3.6 変更一覧（CHANGELOG）

対象: v3.5 → v3.6。改訂日 2026-10-20。骨格（§1〜§29 の機構）と設計判断（原則・機構）は不変。本改訂は **DR モード（§25.1、Backlog #45）の実装**に合わせ、DR plan の書式と効き方、書込 break-glass の署名・検出・事後合流を書き足し、§29.1 で「実装中に決める」としていた break-glass の到達不能判定基準と合流の競合手順を決める小改訂である。

形式: **変更ID / 対応 / 章 / 変更内容 / 設計判断の理由**。対応の `#nn` は Backlog の項目番号。

---

## CHG-01 — DR plan の置き場所と claim の書き方を決める

- **対応**: #45
- **章**: §25.1 (a)
- **変更内容**: DR plan を `shards/dr/<name>.yaml` に置くとし、§25.1 の例の `claim_change` を `{from: {shard: tokyo}, to: {shard: osaka-dr}}`（シャード名）に直した。引き継ぐ側は standby 拠点で平時 idle の DR シャードとして待機する。`enabled: true` の間 `from` は idle、`to` は `primary` を claim する。宣言の矛盾（`to` が平時 idle でない・`from` が `primary` を claim していない・1 シャードを 2 つの発動中 plan が動かす・`enabled` の書き忘れ）はその周を飛ばす。例の YAML はローダーの書式（ブロック形式）に合わせた。
- **理由**: claim は 1 シャード 1 scope（§17.1）なので、「大阪が東京 scope も持つ」はシャードの複数 claim ではなく「大阪にいる別シャードが東京の claim を受け取る」で表すのが既存の claim ハンドオフ（§17.7）にそのまま乗る。`shards/` の下に置けば全シャードの sparse checkout に入り、発動は 1 行の PR（または緊急時は break-glass）で済む。矛盾を収束させないのは、空の desired で収束しないのと同じ安全側（原則3）。

## CHG-02 — 書込 break-glass の署名と検出の形を決める

- **対応**: #45
- **章**: §25.1 (b)
- **変更内容**: break-glass 鍵は ed25519（`sakutei breakglass keygen`）。署名はコミットの tree・parent・メッセージ（署名行を除く）を覆い、trailer に置く（`sakutei breakglass verify` で確認）。手順は判定 → 失効鍵の拒否 → 署名 → S4 に `breakglass.write` → ミラーに session の印 → push の順で、S4 に書けなければ書かない。Event は `BreakGlassWriteUsed`（severity=critical）で、書いた側と、新しく届いた break-glass コミットを見た拠点の常駐ループの両方が出す。
- **理由**: git の gpg / ssh 署名は外部ツール（gpg・ssh-keygen）と鍵の配り方に依存する。stdlib だけで署名・検証でき、事後レビューで誰でも確かめられる形にした。S4 を先に書くのは「こっそり書けない」（§20 Break Glass 原則）を順序で保証するため。

## CHG-03 — §29.1 の「break-glass の到達不能判定基準・合流競合手順」を決める

- **対応**: #45
- **章**: §29.1・§25.1
- **変更内容**:
  - **到達不能の判定**: 中央 git（ミラーではなくその upstream）への `git ls-remote` が、1 回 10s の timeout 付きで 10s 間隔に 3 回続けてすべて失敗したとき到達不能とする。回数の下限は 3。1 回でも届けば break-glass は使えない。認証の失敗も不達に数える。
  - **合流**: ミラーは session の間は中央を追わず break-glass の先端を配り続ける。中央に届くと break-glass コミットを rebase・squash せず `breakglass/<session>` として中央へ push し、PR の下書きを作る。中央が base から進んでいれば両側で変わったパスを下書きに並べ、自動解消はせず人が解く（中央を正とし、合流ブランチに中央を merge して載せ直す）。取り込みは merge commit / fast-forward に限り、中央の先端が break-glass の先端を含んだら session を閉じて中央の追従に戻り、使った鍵を S4 で失効させる。
- **理由**: 1 回の失敗で break-glass を許すと、一時的な揺らぎで正の外への書込が起きる（§25.4 と同じく「分断」を「災害」と取り違えない）。判定する相手をミラーではなく中央にするのは、ミラーに届いても正には届かないから。合流で中央を自動で追うと、中央の古い先端で緊急書込が巻き戻る。squash・rebase を禁じるのは、署名付きコミットを正の履歴に残し事後監査を通すため（§25.1「事後合流 PR とセット」）。自動解消をしないのは、DR 中の変更と平時の変更のどちらを正とするかが人の判断だから。

## CHG-04 — 既知の限界を書く

- **対応**: #45
- **章**: §25.1
- **変更内容**: PR は合流ブランチと下書きまで（ホスティングの API で開くのは運用者）、session の間は中央の変更がそのミラーの拠点に届かない、squash merge では session が閉じない、を §25.1「v1.5 の実装」に書いた。
- **理由**: Sakutei は git ホスティングの API に依存しない（§22 委譲）。保持の副作用は緊急書込を巻き戻さないことの代価として受け入れる。
