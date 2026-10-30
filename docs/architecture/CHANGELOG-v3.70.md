# CHANGELOG v3.69 → v3.70

**改訂日: 2026-10-29**

NETCONF の 2 段の Apply（#175）を、複数の台にまたがる変更で全部か何もしないかに近づけた改訂である（Backlog #199、親のストーリー
netconf-apply-2 の 1 つめ）。台ごとに確定する今の決まりは変えず、明示した `commit_group` だけをまとめて確定する。

## #199：commit_group で複数の台の変更をまとめて確定する（§23.4・§29.1）

- **背景**: 同じ変更を複数の台に入れる構成（例：同じセグメントを全部の leaf に足す）で、台ごとの 2 段の Apply は、確かめが一部の台で
  落ちると変更を片方の台にだけ残した。
- **決定**（§23.4「NETCONF の commit_group」、§29.1）:
  - netconf のリソースの欄 `commit_group: <名前>`。intent にも、外の realizer が返すリソースの Fields にも書ける。書かなければ今どおり
    台ごとに確定する（暗黙にはまとめない）。どう確定するかの宣言で、機器には無い（差分にならない）。
  - **コアの口 `provider.CommitGrouper`**（`CommitGroup`・`ValidateCommitGroup`）と `core.Op.CommitGroup`。読み込みの時点で group を
    割り当て、group の中の依存の辺（直接でも間接でも）・scope をまたぐ group・`strategy: task` の member を断り、provider にも確かめ
    させる（netconf は 1 台に 1 つの member まで）。
  - member は同じ波に並ぶ（各 member が全 member の依存先を待つ）。差分のある member の op をまとめて 1 回の Apply で渡す。
    1 つでも行けない member（観測できない・止めている・上流が落ちた・backoff・in-flight・予算で止まった・承認待ち）があれば、どの
    member も行かない。承認ゲートは group 単位で、一部だけの承認はさせない。予算は今どおり台ごとの変更として数える。早期収束パスと
    一回性の reconcile の観測し直しの回は group の member を当てない。
  - 流れ: 全部の台に commit confirmed → 全部の台で新しい ssh 接続での到達と宣言どおりかを確かめる（#200 の確かめもここ）→ 全部が
    通ったときだけ各台に確定。時間の上限は group の中で一番短い confirm-timeout の半分（台ごとの値は provider の設定
    `netconf_confirm_timeouts`）。
  - 1 台でも送れない・確かめに落ちる・時間の上限の半分に届く、のどれかなら、どの台も確定しない。`:confirmed-commit:1.1` を持つ台には
    `<cancel-commit>` を送ってすぐ戻し、ほかは機器の confirm-timeout の巻き戻しに任せる。
  - 台ごとの結果（committed・not_committed・unknown）を `provider.CommitGroupError` で返し、コアは台ごとに Event `CommitGroupApplied`
    （edge）を出す。確定しなかった台・分からない台はその台の Apply の失敗（S4 監査も台ごと）。plan は操作ごとに `commit_group` を出し、
    止まった member のある group の残りを `blocked_by: commit_group` にする。
  - **残る窓**: NETCONF に機器をまたぐ確定は無い。確定の rpc を送っている途中で切れた台は成否が分からない（unknown、次の周の観測に
    任せる）。機器が確定を断った台は confirm-timeout で戻る。どちらも、ほかの台は確定している（docs/providers/netconf.md「残る窓」）。
- **検証**: 偽の NETCONF サーバ 3 台の provider のテスト `[AC-199-1-1]`〜`[AC-199-1-5]`、コアのテスト `[AC-199-2-1]`〜`[AC-199-2-4]`、
  デーモンの周の受け入れテスト `[AC-199-3-1]`〜`[AC-199-3-6]`（全部が通る・1 台の確かめが落ちる・確定の途中で切れる（unknown）・
  時間切れ・依存の辺を断る・外の realizer の Fields）。`make integration-netconf` に netopeer2 3 台（tag と digest で固定）の段を足した
  （全部が通る・1 台の確かめが落ちてどの台も確定しない）。
- **見送り**: 1 台に 2 つの部分木を同じ group に入れる（同じセッションで 2 つの部分木を当てる。要る例が出るまで断る）。
