# CHANGELOG v3.76 → v3.77

**改訂日: 2026-10-30**

nftables の base chain の確かめる先（`confirm`）に、netconf と同じ別のリソースの観測（`observe:`）と確かめの関数（`check:`）を足した改訂である（Backlog #214）。

## #214：nftables の confirm の `observe:`・`check:`（§7.4・§23.4・§29.1）

- **背景**: #200 で netconf の confirm に `observe:`・`check:` を書けるようにしたが、nftables の base chain の confirm（#173・#179・#192）で
  書けるのは `self`・`ssh://`・`tcp://` と `after:` の印だけだった。境界の FW の規則を変えたあと、そのホスト自身への到達ではなく
  「転送の経路で向こう側に届くか」を、別の資源の観測や外から渡した確かめの関数で確かめられなかった。
- **決定**:
  - nftables の `confirm` に netconf と同じ書き方で `observe:<provider>/<scope>/<id> [expect:{…}]` と `check:<name>` を書ける
    （intent にも、外の realizer が返すリソースの Fields にも）。`observe:` は intent が宣言するリソースを、コア（`provider.Confirmer`）が
    その provider の Read で観測用の資格のまま、ふだんの範囲で読み、`expect:` の欄の部分集合を比べる。`check:` は
    `pkg/cli.WithConfirmCheck` と network_policy の realizer の `Output.Checks` で名前で登録した関数を観測の文脈で呼ぶ。
  - 読み方・読み込みの時点の確かめ・繰り返し・時間の上限の扱い・知らせ方は netconf と共通の口（`internal/confirmafter`）にした。
  - どちらもいつも**変えたあとだけ**確かめる（`after:` の印は書いても書かなくても同じ）。base chain を変えて流し、到達の先に新しい
    接続で全部届いてから、同じ時間の上限（巻き戻しの上限の半分）の中で通るまで繰り返す。
  - 通らなければ仕掛けを取り消さず、ホストの側の時間切れで元の規則に戻す（今の締め出しの守りと同じ。`RolledBackError` として同じ
    差分の Apply を止める）。知らせには通らなかった先（`observe:<ID>`・`check:<name>`）と最後に見た理由を出す。
  - 「全部の先に `after:` の印が付いた宣言は断る」決まりは**到達の先（self・ssh://・tcp://）についてだけ数え**、**到達の先が 1 つも
    無い宣言は読み込みで断る**（observe:・check: は管理の経路が開いていることを確かめない。締め出しの守りを弱めない）。
  - 読み込みの時点で、`observe:` の先が intent の宣言で比べられる欄であること・`check:` の名前が登録されていることを確かめる
    （file 版も同じ）。同じホストの 1 つのトランザクションで複数の base chain を変えるときは、それぞれの宣言の `observe:`・`check:` を
    それぞれの変更について確かめる。
- **確かめ**: 受け入れテスト [AC-214-1]〜[AC-214-4]（偽の nft・偽の守り・デーモンの周）。統合試験 `TestNFTablesConfirmObserve`
  （本物の nft・決まった道具・観測用の利用者。apply から disarm までの読みが観測用の利用者の道具の list だけなことを sudo の記録で
  確かめ、expect と違えば道具の巻き戻しで元の table に戻ることを確かめる）。
- **変えた節**: §7.4（変えたあとの確かめに別のリソースの観測と確かめの関数）、§23.4（nftables も同じ口）、§29.1（決定の行）。
