# CHANGELOG v3.48 → v3.49

**改訂日: 2026-10-28**

nftables の base chain の変更の守りを詰める改訂である（Backlog #179。親のストーリー「M6：網の間の許可と NETCONF の Apply」）。
#173 の締め出しの守りの上に、確かめる先の宣言・変更の前の確かめ・承認と予算を足した。設計の向きは変えていない。

## #179：確かめる先の宣言・変更の前の確かめ・承認と予算（§7.4・§12.2・§13.3・§29.1）

- **背景**: #173 の確かめは、離れたホストなら同じホストへの新しい ssh、手元なら provider の設定の `ruleset_confirm_address` だけで、
  確かめる先が宣言に無かった（§1.7）。前に届かないのに変えると後の確かめが落ちて戻すだけになる。base chain の作成・policy の
  変更は管理の経路を塞ぎうる壊す変更なのに、予算にも承認にも数えていなかった。
- **決定**:
  - **確かめる先を宣言に書く**：base chain の宣言の `confirm` に `self`・`ssh://[user@]host[:port]`・`tcp://host:port` を `, ` で
    区切って書く。全部に届いたときだけ確かめられたとする。書かなければ `self`（今までの既定）。`ssh://` の鍵・known_hosts は
    そのホストの remote の設定（手元の live には書けない）。`confirm` は観測に出ないので、それだけの差分は収束とする。
  - **変更の前にも確かめる**：写し・仕掛けの前に同じ先のすべてに 1 回ずつ確かめ、届かなければ変えない（一時的な失敗。届かない
    先を名指しする）。
  - **壊す変更として予算と承認ゲートを通す**：provider の口に任意の `provider.ApprovalRequirer` を足した。理由を返した op は
    Destructive で `RequiresApproval`（`ApprovalReason`）になり、明示の absent の削除と同じ門（その種類の planned_change の
    レーン・通常予算にも数える）を通る。レーンの外では `ChangeAwaitingApproval`、`plan` は `blocked_by: approval` と
    `approval_required`。planned_change の kind に `create` を足した。nftables の live は base chain を作る・type/hook/priority/
    policy を変える・消す op をそうする。ルールだけを変える op と regular chain の op は今までどおり。
- **見送り**:
  - 確かめる先ごとの時間の上限。確かめで `true` 以外のコマンドを走らせること。
  - file 版の base chain の変更を承認の要る変更にすること（file の Apply は FW を変えない）。

## 影響

- §7.4 に 1 項、§13.3 に明示の absent の削除の項の次の 1 項と kind の `create`、§29.1 に 1 行を足し、
  #173 の行に注を付けた。
- provider の口に `provider.ApprovalRequirer`、`core.Op.ApprovalReason`、Event `ChangeAwaitingApproval`、plan の JSON の
  `approval_required`・`approval_reason` を足した（足しただけなので plan の JSON の版は上げない）。外の provider も使える。
- 一回性の `reconcile` は、承認待ちの変更があれば「awaiting approval」の要約を出す（終了コードは今までどおり）。
- 受け入れテスト [AC-179-1]〜[AC-179-6]（`tests/acceptance/nftables_approval_test.go`）、単体テスト（`internal/diff`・
  `internal/budget`・`internal/nftables`・`internal/remote`）、統合テスト（`make integration-nftables`）。#173 の受け入れテストと
  統合テストは承認したレーンを宣言に置くように改めた。
