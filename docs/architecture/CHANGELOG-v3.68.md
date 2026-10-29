# CHANGELOG v3.67 → v3.68

**改訂日: 2026-10-29**

netconf の比べ方を、属性つきの configuration と空の configuration を返す NETCONF サーバでも収束するように直した改訂である
（Backlog #208）。「書き方の違いはずれにしない」「部分木が無いのは観測の事実（absent）」という #141 の向きは変えず、その当て方を
機器の返り方に合わせた。

## #208：netconf の属性の比べ方と空の configuration（§29.1）

- **背景**: get-config の返りの最上位の `<configuration>` に commit の時刻・利用者などの属性を付けるサーバでは、中身が同じでも
  drift のままで、2 段の Apply も新しい接続の確かめで属性の値が変わっていて合わず、毎回 not confirmed で機器の巻き戻しに戻って
  いた。部分木が無いときに名前空間の無い空の `<configuration/>` を返すサーバでは、plan が CREATE でなく UPDATE になった。
  属性だけが違うときの理由は「the order of an ordered list differs」で、違いを指していなかった。
- **決定**（§29.1「NETCONF：属性つきの configuration と空の configuration を返す機器で収束させる」）:
  - 比べるのは宣言に書いた属性だけ。機器の側の属性は、宣言の同じ道の要素に同じ名前（名前空間と名前）の属性があるときだけ
    比べる。観測の値は機器の返したままで、絞るのは比べる側（PlanRefiner）と 2 段の Apply の確かめ。
  - 子も値も無い最上位の要素は、名前空間の有無も属性も問わず absent。中身の無い要素の宣言は読み込みで断る。
  - 属性だけが違えば、理由に「道 @属性 = 値」の行で機器の側と宣言の側を出す。
  - ProviderVersion を `netconf-v2` に上げた。
  - docs/providers/netconf.md に機器の側の事実（非 ASCII が文字参照の形で崩れて返るサーバ・NACM を持たないサーバでは書き込み用の
    利用者を絞り切れず、部分木だけの置き換えが守りの一つになること・filter の最上位にサーバ固有の名前空間が要るサーバ）を書いた。
- **検証**: 偽の NETCONF サーバ（`netconffake` の `TopAttrs`・`EmptyData`）の受け入れテスト `[AC-208-1-1]`〜`[AC-208-1-4]`
  （属性だけの違いは drift にならない・2 段の Apply が確定まで通る・部分木が無いときの plan が CREATE・宣言の属性の違いを名指し）。
  既存の `make integration-netconf`（netopeer2）。
- **見送り**: 比べない属性を provider の設定で名指しする口（宣言に書かなければ比べないので、要る例が出るまで入れない）。
