# CHANGELOG v3.65 → v3.66

**改訂日: 2026-10-29**

postgresql で、管理の境界の外のロール（観測用のロールなど）にデータベースの CONNECT を宣言で与える口を足した改訂である
（Backlog #203。親のストーリー「使って分かった足りない点」の 1 つ目）。#186 で観測用の資格を分けた流れの穴を埋めるもので、
§1.7（正しい状態は宣言に置く）に寄せた。設計の向きは変えていない。

## #203：postgresql の境界の外のロールの CONNECT（§29.1）

- **背景**: PUBLIC の CONNECT を宣言で外したデータベース（`grant/PUBLIC` の `privileges: ""`）には観測用のロールが繋げず、
  拡張が観測できない（unknown）・List が失敗して prune がその周止まる（PruneHalted）。観測用のロールは管理の境界の外なので
  `grant/<role>` に書けず、docs は手で `GRANT CONNECT` するよう案内していた。
- **決定**（§29.1「PostgreSQL provider の境界の外のロールの CONNECT」）:
  - 形は**データベースの欄** `connect_roles`。`grant/<role>` を境界の外のロールに開く案は採らなかった。欄なら
    `CREATE DATABASE` と同じ Apply の中で CONNECT を付けられ、宣言の順や depends_on に依らず PUBLIC の CONNECT を外す前に
    必ず付く（PUBLIC の権限はデータベースが在ってから変える）。境界の外のロールの権限を一覧ちょうどに揃えない（足すだけ）ことも
    別のリソース無しで言える。
  - 書けるのは Sakutei がつなぐロール（`user`・`observe.user`・`write.user`）と `pg_*` だけ。境界の内のロール・`PUBLIC` は
    読み込みで断る。
  - 観測は境界の外のロールのうち CONNECT を直に持つものの一覧で、宣言の一覧が含まれていれば同じ。Apply は足りない分だけ
    `GRANT CONNECT`、REVOKE はしない（欄から外しても残り、prune でも外れない）。ロール自体は触らない。
  - CONNECT の無いデータベースへの接続の `permission denied for database` は、届かないではなく接続が許されないとして知らせる。
  - `verify_passwords` を読むだけのロールで観測するときの `pg_authid` の列の権限（`GRANT SELECT (rolname, rolpassword) ON
    pg_authid`）は、列の権限がデータベースごとなので**管理用の DB で与える**。表・列の権限は持たないので宣言にはせず文書で案内する。
- **検証**: 単体（偽の psql の読むだけのロールを CONNECT の無いデータベースに繋げなくした。`[AC-203-1-1]`・`[AC-203-1-2]`）と
  `make integration-postgresql` の「connect_roles keeps the observe role connected ...」段（PostgreSQL 17.11。分けた資格で
  PUBLIC の CONNECT を外すデータベースを PruneWithinScope で空から宣言し、手の GRANT 無しでどの周も prune が止まらず拡張が
  観測できる。手の REVOKE はずれで 1 周で戻る）と、verify_passwords の段に足した列の権限の確かめ（管理用の DB でない所で
  与えても効かず、管理用の DB で与えれば効く）。
- **見送り**: 境界の外のロールへの CREATE・TEMPORARY（要るときに足す）。`connect_roles` から外したときの REVOKE（足すだけに
  留め、外すのは手か明示の手順で）。`pg_authid` の列の権限の宣言（表・列の権限を持つ provider か Task で）。
