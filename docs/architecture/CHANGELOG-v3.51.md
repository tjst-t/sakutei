# CHANGELOG v3.50 → v3.51

**改訂日: 2026-10-28**

網の間の通信の許可を、機器に依らない抽象のリソースとして宣言する改訂である（Backlog #174。親のストーリー「M6：網の間の許可と NETCONF の Apply」の 2）。
#173・#179 の nftables の base chain の宣言の上に、許可の宣言・検査・展開を足した。設計の向きは変えていない（正しい状態は宣言に置く、§1.7）。

## #174：網の間の許可を抽象のリソースにする（§7.6・§29.1）

- **背景**: 網の間の許可は機器（境界の FW・ホストの nftables）ごとの規則としてしか書けず、同じ許可が機器の数だけ散らばり、
  なぜ許したか（特に弱い方式を例外として許した理由）が宣言の外に残っていた。網の重なり・前の規則に隠れて効かない規則・
  宣言に無い網を指す規則も、規則を流すまで分からなかった。
- **決定**:
  - intent のトップレベルの `network_policy:` に `networks`（名前と CIDR）・`allows`（from・to・proto・ports・reason・tags）・
    `targets`（実現先：`kind: boundary` か `host`、実現先の resource の id・scope・`before`・`after`・realizer に固有の `settings`）を書く。
    既定は拒否で、書いた許可だけが通る。
  - 許可の行ごとに `reason` と印 `tags` を書ける。`exception` の印の付いた許可に reason が無ければ読み込みで断る。
  - 検査：網の CIDR の重なり・宣言に無い網・理由の無い例外・同じ許可の重複（と名前の二度書き・網が 2 つ未満の境界）は読み込みで
    断り（全部を並べる）、前の許可に覆われて効かない許可・どの実現先も通らない許可は warning として plan と reconcile に出す。
    結果には行の場所と reason・tags を添える。
  - 展開は読み込みで行い（§9.1 フェーズ 2 と同じ決定的な展開）、作った resource は intent の resources と同じに収束する
    （予算・承認・締め出しの守り・external_owners・重複の id の検査がそのまま効く）。実現先ごとにそこを通る許可だけを realizer に渡す。
  - 本体の realizer は nftables だけ：実現先 1 つを base chain 1 つ（`type filter`・boundary は hook `forward`・host は `input`・
    `priority filter`・`policy drop` が既定）にし、許可 1 つをアドレスの族ごとに 1 ルールにし、reason を comment に出す
    （nft の comment の 128 バイトと、`"` を逃がせないことに合わせて切る・置き換える）。
  - 展開の口 `cli.WithRealizer(name, r)`（`pkg/netpolicy.Realizer`）。使う側が別の実現先（スイッチの ACL など）への変換を足す。
    reason・tags も渡す。本体はスイッチの ACL への変換を持たない。
  - `sakutei plan` は実現先ごとの展開（通る許可・reason・tags・規則）と warning を出し、`--json` は `network_policy` を持つ。
- **見送り**:
  - スイッチの ACL への変換（使う側の拡張）。
  - 網の重なりを許す「外の網」（`0.0.0.0/0` など）。いまは重なりとして断る。
  - 送り元のポート・接続の状態を許可に書くこと、拒否の宣言（既定の拒否だけ）。

## 影響

- §7.6 を新設し、§29.1 に 1 行を足した。spec/README.md の §7 の要約に足した。
- 公開の package `pkg/netpolicy`（宣言の型・`Parse`・`Check`・`Compile`・`Realizer`・`Register`・本体の `NFTables`）と
  `cli.WithRealizer` を足した。nftables のルールの正規形（`NormalizeSets`）を葉の package `internal/nftrule` に移し、
  nftables provider と `pkg/netpolicy` が同じ関数を使う（振る舞いは変えていない）。
- plan の JSON に `network_policy` を足した（足しただけなので版は上げない）。
- 単体テスト（`pkg/netpolicy`・`internal/loader`・`pkg/cli`）、受け入れテスト [AC-174-3-1]〜[AC-174-3-3]
  （`tests/acceptance/netpolicy_test.go`）、統合テスト（`make integration-nftables` の `TestNFTablesNetworkPolicy`）。
- 書き方は `docs/network-policy.md`。
