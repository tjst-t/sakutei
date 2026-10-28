# CHANGELOG v3.47 → v3.48

**改訂日: 2026-10-28**

NETCONF の観測と 2 段の Apply を、機器の側（NACM、RFC 8341）で宣言した部分木だけに絞ったユーザーで動かす改訂である（Backlog #176。
親のストーリー「M6：網の間の許可と NETCONF の Apply」の 4 番目）。#141 の観測・#175 の 2 段の Apply の上に乗せたもので、設計の向きは
変えていない。

## #176：部分木だけに絞ったユーザー（§29.1）

- **背景**: 網の機器の NETCONF のユーザーは、宣言する部分木だけを読み書きできるよう NACM で絞って運用したい（爆発半径を機器の
  側でも小さくする）。Sakutei が部分木の外を読む・書く・ほかの rpc を要るなら、絞ったユーザーでは動かない。
- **決定**:
  - **送る rpc**：観測は `<get-config>`（running、部分木の filter）と `<close-session>` だけ。2 段の Apply はそれに `<lock>`・
    `<unlock>`（candidate）・`<edit-config>`（部分木の置き換え、外には触れない）・`<validate>`・`<commit>`・`<discard-changes>` を
    足すだけ。機器について知ることは hello の capability だけから読む。lock は candidate の全体を締めるが、締めるだけで外を読み
    書きしない。lock の間の candidate は自分の変更しか持たないので、discard・validate・commit も外を変えない。
  - **access-denied**：観測なら unknown の PermanentError（許すべきもの〈get-config の実行と部分木の読み〉を理由に書く）。Apply の
    どの段でも何も確定せず（edit-config の後なら candidate を discard して）PermanentError（error-path と、外を変える宣言か許されて
    いない rpc かを理由に書く）。discard-changes が失敗したときは「捨てた」と言わずにそう書く。
  - **既知の限界**：NACM は読めないデータを黙って省くので、読むのを許されていない部分木は absent と見分けられない。その宣言は
    plan に CREATE と出て、Apply は access-denied で断られる（何も書かない）。
- **確かめ**: 偽の NETCONF サーバに絞ったユーザーの近似（`Scope`・`ScopeOps`、外を触ったら記録）を足した単体テスト
  （[AC-176-1-*]）と、netopeer2 に NACM で 2 つの部分木と要る rpc だけを許した利用者を作る統合テスト（`make integration-netconf`）。

## 変えた所

| 節 | 変更 |
| --- | --- |
| §29.1 | 「NETCONF を部分木だけに絞ったユーザーで動かす」の行を足した |
| README（spec） | 版を v3.48 に上げ、v3.48 の要約と本 CHANGELOG への参照を足した |
