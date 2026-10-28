# CHANGELOG v3.56 → v3.57

**改訂日: 2026-10-28**

S4 監査の記録を固める改訂である（Backlog #187。親のストーリー「M7：本番に向けた固め」の監査）。§26.3 が求めていた
「append-only + 連鎖ハッシュ」「Apply 実行記録」「外部アンカー・即時 push」を v1 の形で決めた。設計の向きは変えていない。

## #187：監査の記録を固める（ふつうの Apply も・改ざんしにくい形・外へすぐ送る、§26.3）

- **背景**: S4 監査には承認ゲート・planned_change・freeze・break-glass は残っていたが、ふつうの Apply と prune は Event と
  ログにしか無く、「何を変えたか」が監査に残らなかった。記録は JSON Lines を追記するだけで、行を書き換えても抜いても分からず、
  外へ出す口も無かった（侵害されたホストが自分の記録を書き換えられる）。
- **決定**:
  - **Apply と prune を残す**：Op ごとに 1 件、種別 `resource.apply`・`resource.prune`。resource・op（create / update / delete）・
    変えた欄の名前（値は書かない）・cause（帰属）・結果（`ok` / `failed` と理由）・planned_change のレーンで通った Op はその宣言と
    宣言を承認した人。Executor の層で記録するので、常駐の周・早期収束・一回性の `reconcile`・Workflow の apply の段のどれも同じ
    形で残る（fencing で断られた Apply も失敗として残る）。記録は Apply のあと。書けなかったときは知らせ、Apply の結果は変えない。
  - **鎖**：各行に通し番号 `seq` と前の行の SHA-256 `prev`。`sakutei audit verify`（`--json`）が書き換え・抜き取り・差し込み・
    回したファイルの抜けを見つけ、終了コード 3 で知らせる。鎖より前の先頭の行は legacy として受ける。追記は `<file>.lock` の
    flock で並べる（常駐と CLI が同じファイルへ書くため）。
  - **書き換え・切り詰めをしない**：回すとき（`--audit-rotate-bytes`）は新しいファイル `<file>.000001` … へ続け、鎖はつながる。
    書いている途中で切れた行も切り詰めず、改行で閉じて verify が知らせる。WORM の置き場でも成り立つ。
  - **外へすぐ送る**：`--audit-syslog tcp://…` / `tls://…`（`--audit-syslog-ca`・`-cert`・`-key`・`-server-name`）で 1 件ずつ
    syslog へ（RFC 5424、octet-counting、TLS は RFC 5425。`[meta sequenceId]` に seq、MSG は書いた行そのもの）。貯め場はログ
    そのもので、送り終えた所を `--audit-syslog-state` に持ち、届かない間の記録は手元に残って、常駐は起こされるたび・
    `--audit-syslog-retry` ごとに、一回性のコマンドは次の記録のときに続きから送り直す（`sakutei audit forward` でその場で）。
    送れなくてもコマンドは止めない。届けるのは少なくとも 1 回。
  - **末尾の切り詰め**は鎖だけでは分からないので、verify は送り終えた所（seq と行のハッシュ）を外の控えとして突き合わせる。
  - `--audit-syslog` 系の旗は常駐・`reconcile`・`freeze`・`approve` / `reject`・`workflow cancel`・`task approve` / `reject`・
    `apply-check release`・`allocator release` / `import` に付く。
- **互換**: 監査のログの行に `seq`・`prev` と Apply・prune の行が増える。既存のログには続けて書け、先頭の古い行は legacy として
  verify を通る。監査のログを種別で絞らずに数えていた道具は Apply・prune の行を除く必要がある。
- **見送り**:
  - 監査を書くプロセスを reconciler と分けること（別権限・別プロセス、§26.3）。
  - syslog 以外の送り先（SIEM の HTTP の口・オブジェクトストレージの WORM へ直接）と、受け手の受け取りの確認（RELP など）。
  - break-glass（`sakutei breakglass`）からの syslog の直送（記録は鎖に入り、次に送る者が送る）。
