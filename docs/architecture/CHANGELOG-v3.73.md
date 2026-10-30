# CHANGELOG v3.72 → v3.73

**改訂日: 2026-10-30**

NETCONF の 2 段の Apply（#175）の確かめる先に、別のリソースの観測と、使う側が渡す確かめの関数を足した改訂である（Backlog #200、親の
ストーリー netconf-apply-2 の 2 つめ。1 つめは v3.70 の commit_group）。ある台への変更が効いたかは、相手の台から見ないと分からない
ことがある（例：相手の台で経路が見えること）。

## #200：confirm の先の observe: と check:（§23.4・§29.1）

- **背景**: 2 段の Apply の確かめは、変えた台そのものに新しい接続で届くことと、部分木が宣言どおりなことだけだった。
- **決定**（§23.4「NETCONF の confirm の先」、§29.1）:
  - netconf のリソースの欄 `confirm`（intent にも、外の realizer が返す Fields にも書ける）。確かめる先を「, 」で区切る:
    `self`（機器そのもの。書かなければこれだけ）・`observe:<provider>/<scope>/<id> [expect:{…}]`・`check:<name>`。どう確かめるかの
    宣言で、機器には無い（ほかの差分があるときだけ op に残す）。
  - **observe:**：コアがそのリソースの provider の Read を**観測用の資格**で（`provider.WithObserve`。Apply の中でも書き込みの文脈を
    外す、§20.7）、**ふだんの範囲のまま**読む。読めるのは intent が宣言するリソースだけ（宣言に無い ID・`ensure: absent` は読み込みで
    断る。範囲は provider がふだん観測するときのまま。netconf なら部分木の filter と機器の NACM）。`expect:` は JSON の object
    （欄 → 値）で、**欄の部分集合**だけを、そのリソースの宣言に重ねて diff と provider の PlanRefiner に通して比べる（ふだんの収束と
    同じ比べ方）。computed・managed・secret の欄は書けない。`expect:` が無ければ観測できて在ることだけを確かめる。
  - **check:**：名前で登録した確かめの関数（`pkg/cli.WithConfirmCheck(name, func(ctx, provider.ConfirmTarget) error)` と、
    network_policy の realizer の `netpolicy.Output.Checks`）を呼ぶ。何をどう読んで判定するかは関数を渡す側が持つ。ctx は観測の文脈。
    同じ名前の 2 度の登録・登録の無い名前は断る。
  - **いつも変えたあとだけ**：observe: と check: は commit confirmed と self の確かめのあと、確定の前に、通るまで時間の上限（self と
    同じ confirm-timeout の半分）の中で繰り返す。`after:` は書いても書かなくても同じ。**到達の先（self）が 1 つも無い confirm は読み
    込みで断る**（v3.61 の「全部の先に印の付いた宣言は断る」を、到達の先についてだけ数える形にしたもの）。`after:self` も断る。
  - **通らなければ確定しない**：self が落ちたときと同じ扱い（確定の commit を送らず、機器の巻き戻しに任せ、PermanentError）。
  - **commit_group**：group の全部の台が commit confirmed を終え、全部の台の self が通ってから、全部の台の observe:・check: を待つ。
    1 つでも通らなければ、どの台も確定しない。
  - **コアの口**: `provider.Confirmer`（`CheckObserve`・`HasCheck`・`Observe`・`Check`）、`provider.ConfirmUser`（コアが読み込みの
    たびに、ValidateIntent の前に Confirmer を渡す）、`provider.ConfirmCheck`・`provider.ConfirmTarget`・`provider.WithObserve`。
    常駐は `reconcile.Config.ConfirmChecks`。
- **検証**: 受け入れテスト `[AC-200-1]`〜`[AC-200-6]`（偽の NETCONF サーバ 2 台とデーモンの周：observe: が期待どおりなら確定・違えば
  確定しない・観測用の資格と範囲だけで読む・外の realizer の Output.Checks の check:・commit_group で全部の commit confirmed のあと・
  読み込みで断るもの）、`internal/netconf`（書式）・`internal/reconcile`（Confirmer）・`pkg/cli`（WithConfirmCheck）の単体テスト。
  `make integration-netconf` に netopeer2 2 台（commit_group の試験の g1・g2）の段を足した（g1 の変更を g2 の観測を条件に確定し、
  g2 が期待と違えば確定せず機器が戻す）。
- **見送り**: nftables の base chain の `confirm`（#179・#192）には observe:・check: を足していない（netconf だけ。要る例が出たら同じ
  口で足す）。observe: で機器の operational の状態は読めない（netconf の観測は get-config。#201）。
