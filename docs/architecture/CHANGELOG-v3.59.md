# CHANGELOG v3.58 → v3.59

**改訂日: 2026-10-28**

nftables の離れたホストで root で走るものを決まった道具 1 つにし、sudo をその道具だけに絞れるようにする改訂である
（Backlog #191。親のストーリー「M7：本番に向けた固め」の鍵の分けの続き）。#185 で観測用と書き込み用の資格を分けたが、
書き込み用の利用者には `sh` と `nft` の sudo（実質 root）を許すしかなく、書き込み用の資格が漏れたときの被害はホスト全体だった。
設計の向きは変えていない。

## #191：root で走るものを決まった道具 1 つに（§20.7・§29.1）

- **背景**: base chain を変えるときの巻き戻しの仕掛け（arm・disarm）は対象のホストで `sh -c` に台本を渡して動かし、nft も
  sudo で直接流していた。sudoers で `sh` と `nft` を許すのは root を与えるのと同じ。
- **決定**:
  - **決まった道具** `sakutei-nft-guard`（`cmd/sakutei-nft-guard`、中身は `internal/nftguard`。Go の小さな別の実行ファイル、
    外の依存なし）をホストの決まった場所 `/usr/local/libexec/sakutei-nft-guard` に置く。provider の設定 `ruleset_guard_tool`
    （つなぎ方。離れたホストだけ）に書いたホストでは、nft と sh の代わりに道具だけを走らせる。
  - 道具が受けるのは決まった形の引数だけ：`list <family> <name> json|handles`・`check`・`apply`・`arm <秒> [<family>/<name>...]`・
    `disarm <token>`・`status`。ほかは何もせず終了コード 2。
  - **扱う表は道具の設定ファイル** `/etc/sakutei/nft-guard.json`（root だけが書ける。`tables` 必須）に書き、呼び手からは渡さない。
  - **規則は構造として読む**：5 つの行の形（`add table`・base chain の `add chain`・`flush chain`・`delete chain`・`add rule`）
    だけを、許した表のものだけ受ける。ルール文の引用符の外の `;`・`#`・`$`・`\`・制御文字・閉じない括弧と引用符は受けない。
    `flush ruleset`・`include`・`define`・別の表などが 1 行でもあれば何もせず断る。
  - **写しを取るのも戻すのも許した表だけ**。予約は道具が自分を切り離して起動し直して眠り、秒の後に写しを流す（Sakutei が
    居なくなっても戻る）。起動し直した道具は予約のディレクトリにだけ置いた nonce を受けたときだけ動く。
  - **道具が無い・版（`protocol`）が合わない**ときは base chain を変えない（一時的な失敗）。表・秒が道具の設定の外なら変えない
    （PermanentError）。上限は秒に切り上げて仕掛け、Sakutei も切り上げた時刻まで待つ。
  - **観測用の資格（#185）でも読める**：観測は道具の `list` だけ。sudoers の最小の行は、書き込み用に道具だけ、観測用に道具の
    `list *` だけ。
  - sudo を使わず root で入る構成では、今までの動かし方（nft と sh）を残してよい。
- **互換**: `ruleset_guard_tool` を書かなければ今までどおり。手元の live は変わらない。sh の仕掛けも、上限が秒の端数のときは
  切り上げた時刻まで待つようになった（仕掛け自体はもともと切り上げていた）。
- **見送り**:
  - nft の JSON の形で規則を受けること（ルール文を JSON に直すには nft そのものが要る。行の形を構造として読む）。
  - 手元の live での道具（Sakutei 自身が root か nft のラッパーで動く）。
  - 道具の本体を host-file で配ること（中身の上限 256 KiB を超える。設定ファイルと sudoers の行は配れる）。
  - systemd のユニット越しの予約（`systemd-run` の無いホストがあるため、道具が自分で切り離す）。
  - ほかの ssh 越しの provider（dnszone・host-file・systemd-unit）の道具。
