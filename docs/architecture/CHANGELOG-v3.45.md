# CHANGELOG v3.44 → v3.45

**改訂日: 2026-10-28**

NETCONF の機器の設定を宣言で当てる改訂である（Backlog #175。親のストーリー「網の間の許可と NETCONF の Apply」の 3 つめ）。
#141 で決めた (6) の方針（commit confirmed の 2 段の Apply）を入れたもので、§23.4 の「CommitConfirm が無ければ Apply 禁止」は変えていない。

## #175：NETCONF の 2 段の Apply（§5.3・§23.4・§29.1）

- **背景**: `netconf` は観測だけ（`ObserveOnly`、#141）で、ずれを知らせても直せなかった。NW 機器の設定を当てると、届かなくなる設定で
  自分を締め出す経路が残るので、§23.4 は commit confirmed（自動の巻き戻し）を Apply の条件にしていた。
- **決定**:
  - **設定で明示したときだけ Apply する**：`--provider-config` の `netconf_apply: "true"` で `CommitConfirm` を宣言し `ObserveOnly` を外す。
    明示しなければ観測だけのまま。`netconf_confirm_timeout`（既定 2m、下限 2s）・`netconf_commit_flow`（`1.0` が既定、`1.1`）。
    Apply を許さずにこれらだけを書くのは読み込みで断る。予算・承認ゲート・remediation mode は他の Apply と同じに通る。
  - **流れ（`:confirmed-commit:1.0` を主）**：Apply 用の新しいセッション → `:candidate` と `:confirmed-commit`（1.0 か 1.1）が無い機器には
    何も送らない → candidate を lock → running の部分木を読む → 部分木の置き換えを edit-config → `:validate` があれば validate →
    **同じセッションで** commit confirmed → **新しい ssh 接続で**届くこと・宣言どおりなことを確かめる（**confirm-timeout の半分で打ち切る**）→
    **同じセッションで**確定の commit。1.1 の機器でも既定はこの流れ。`1.1` を選べば、1.1 を持つ機器で persist・persist-id の流れ。
  - **確かめられなければ確定しない**：candidate を discard し、Apply のセッションを開いたまま confirm-timeout まで待って機器の自動の巻き戻しに
    任せ、過ぎたら新しい接続で戻ったかを見て PermanentError で知らせる（同じ宣言は変わるまで当て直さない）。
  - **確定の成否が分からない**ときは TransientError（unknown）で次の周の観測に任せる。途中で落ちたら機器の巻き戻しに任せる。
  - **部分木の置き換え**は filter の形で決める（content match だけ・子の無い要素を境目に replace と remove、containment は中に入る、
    `ordered` の境目は `yang:insert="last"`）。filter の選ばない宣言の要素と、宣言の中の `nc:operation`・`yang:insert` は断る。
- **見送り**:
  - 1 台の複数の部分木を 1 つの commit にまとめること。部分木ごとに 2 段で当てる（1 台に 1 度に 1 つ）。
  - 確かめに失敗したときに Apply のセッションを閉じて（1.0 の機器はその場で戻す）待ち時間を縮めること。巻き戻しは機器の時計に頼る形にそろえた。
  - 部分木を消すこと（prune・`ensure: absent`）。Delete は当てない。
  - 宣言した部分木の外を読まない・書かないユーザー（NACM で絞る）での統合試験は、親のストーリーの 4 つめ。

## 影響

- §23.4 に流れと時間の決まりを、§5.3 に `netconf` が CommitConfirm を宣言する条件を、§29.1 に 1 行を足し、#141 の行の (6) を「入れた」に直した。
- 単体テスト [AC-175-1-*]（edit-config の中身）・[AC-175-2-*]（偽の NETCONF サーバで 2 段の Apply）、受け入れテスト [AC-175-3-*]
  （`tests/acceptance/netconf_apply_test.go`）、統合テスト（`make integration-netconf` に 2 段の Apply の 3 段）。
