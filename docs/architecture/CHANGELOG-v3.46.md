# CHANGELOG v3.45 → v3.46

**改訂日: 2026-10-28**

nftables の base chain の policy（既定の拒否）を宣言で管理し、自分（Sakutei）を締め出さない守りを入れる改訂である（Backlog #173。
親のストーリー「M6：網の間の許可と NETCONF の Apply」の 1 つめ）。nftables の live 版（#121）・ssh 越しの実行の層（#130）・
効かない Apply を止める仕組み（#166）・触らない宣言（#158）の上に乗せたもので、設計の向きは変えていない。

## #173：base chain の policy と締め出しの守り（§7.4・§29.1）

- **背景**: nftables provider は base chain（type/hook/policy を持つ chain）を管理の境界の外に置いていた（#121）。既定で拒否する
  FW（`policy drop`）を宣言で持てず、正しい状態が宣言の外にあった（§1.7）。一方で、既定で拒否する chain を宣言どおりに収束させる
  と、宣言が Sakutei 自身の管理の経路（ssh）を通していなければ、その瞬間に締め出される。
- **決定**:
  - **base chain は intent が宣言したときだけ扱う**。`type`・`hook`・`priority`・`policy` の 4 つを書いた宣言が base chain の宣言で、
    4 つとも要る。priority は数か nft の名前（`filter`・`filter + 10`）で、数に直して比べる。設定で base chain を名指しすることは
    しない（範囲は intent に置く、§1.7・#158）。
  - **宣言していない base chain は今どおり触らない**。List・BulkObserve に出さない（status の宣言外にも出ない）。宣言したものは
    Read が観測する。宣言が regular chain なのに現実が base chain なら計画の時点で断る。
  - **締め出しの守り**（commit confirmed と同じ考え）：base chain を作る・変える・消す Apply は、変える table の写しを取り、検査し、
    対象のホストの側に時間切れ（`ruleset_rollback_timeout`、既定 30 秒）で写しへ戻す仕掛けを置いてから 1 つのトランザクションで
    流し、**新しい接続**（離れたホストは ssh の master に相乗りしない新しい ssh の接続、手元の live は `ruleset_confirm_address`
    への TCP の接続）で到達を確かめてから仕掛けを取り消す。仕掛けはホストの上で切り離して動くので、Sakutei との接続が切れても戻る。
  - **確かめられなければ**仕掛けが戻すのを待ち、Apply は巻き戻されたとして失敗する（`provider.RolledBackError`、一時的な失敗）。
    コアはそれを効かない Apply と同じに扱い、同じ差分の Apply をその場で止める（oscillating、reason `rolled_back`、§11.1 Tier3）。
    一回性の `reconcile` は終了コード 4。
  - **到達性の最小の核は Sakutei の外で保守する**。管理の ssh を通す規則などは、宣言した base chain から `jump` する chain などに
    置き、`external_owners:`（§7.3（c））か scope で外す。ほかの base chain の `accept` はこの chain の `policy drop` を越えないので、
    核は宣言した base chain から辿れる場所に置く（§7.4）。
- **見送り**:
  - regular chain だけを変える Apply を守りに通すこと（base chain から `jump` される chain の変更でも締め出しうるが、宣言の側で
    核への `jump` を置いて防ぐ）。
  - 写しを ruleset 全体で取ること。変える table だけを写す。
  - set・map を宣言すること。ingress・egress の hook（device が要る）。

## 影響

- §7.4 に境界の決まりを、§29.1 に 1 行を足し、#121 の行の管理境界に注を付けた。
- provider の口に `RolledBackError`（`provider.NewRolledBackError`・`provider.IsRolledBack`）を足した。外の provider も、Apply を
  対象の側で巻き戻したときにこれを返せば、同じ差分の Apply が止まる。
- 受け入れテスト [AC-173-1-*]〜[AC-173-5-*]（`internal/nftables/basechain_test.go`・`guard_test.go`・`internal/remote`・
  `tests/acceptance/nftables_lockout_test.go`）と統合テスト（`make integration-nftables`）。
