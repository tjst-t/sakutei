# CHANGELOG v3.33 → v3.34

**改訂日: 2026-10-28**

採番の結果を宣言の側へ書き戻し、それを正として読むようにした改訂である（Backlog #157。親のストーリー「正しい状態は
宣言に置く：採番の結果を宣言の側へ書き戻す・触らない宣言を intent に入れる」の 1）。あわせて、その元になる原則を
一般の形で書いた。

## 原則：正しい状態は宣言に置き、ツールの中だけに持たない（§1.7 を新設）

- ツールを失っても、ツールに依らない宣言と、宣言どおりに組まれた現実が残るようにする。あるべき状態の正は宣言の側に
  素の形で置き、ツールの中の記録は写しか作業の途中の記録（Task・Workflow の進み、承認、cooldown、キャッシュ）にとどめる。
- provider の設定はつなぎ方（宛先・鍵・時間）だけにし、管理の範囲と除外は intent に書く。
- 観測 state レス（§1.3：現在状態を持たない）と対になる原則として §1 に置いた（§3 の八つの原則は番号ごと保つ）。

## #157：採番の結果の書き戻し（§19.7・§19.9・§24.5・§22.3・§29.1）

- **背景**: 採番で配った値（vmid・IP・VLAN）が採番の store にしか無く、intent には `from_pool` しか無い。ツールと store を
  失うと、宣言からはどの owner に何を配ったかが分からない。
- **決定**:
  - 書き戻す範囲は intent：`pools:` の各プールの `write_back: true`（既定 false）。
  - ファイルはプールごとに 1 つの素の YAML（`schema_version: 1`・`pool`・`allocations`（owner・value・allocated_at））。
    store の内部の形は出さない。並びは value の順で決定的。owner は `<provider>/<local id>#<field>`（scope は入れない）。
    書式は `docs/allocation-file.md`。
  - 置き場とコミットの仕方は設定（`--alloc-write-back`）：パス（既定 `generated/allocations`、リポジトリの根からの相対）・
    仕方 `commit`（既定。intent のブランチへ直接、Sync Mode のコミットに相乗り：コミッター・署名なし・間引き。trailer
    `Sakutei-Allocation: <pool>`）・`review`（別のブランチにコミットし、人が取り込む）・`file`（書くだけ）・つなぎ方
    （`repo`・`remote`・`ssh_key`）。
  - loader（render のフェーズ1）はファイルを正として読む：合えばそのまま／食い違えば Apply を止めて知らせる（どちらにも
    寄せない）／ファイルにあり store に無ければ止めて import を案内／store で配ったがファイルに無ければ `commit`・`file` は
    書き戻してから使い、`review` は取り込まれるまで止める（plan・status に「書き戻しの取り込み待ち」）。その間も値は
    store で配ったままで、他に配らない。ファイルの割当のうち 1 つでも store に無い・食い違うあいだは、そのプールから
    新しい値を配らない（再払い出しの衝突を防ぐ）。
  - 止めた要求は waiting-allocation と同じくそのリソースと下流だけを skip し、level の Event `AllocationHeld` を出す
    （一回性の reconcile の終了コードは waiting-allocation と同じく 0）。
  - `sakutei allocator import --intent <dir> (--store | --url)`：ファイルから store を作り直す。既にある記録と食い違う
    （owner が別の値・値を別の owner が持つ・値が cooldown 中・範囲の外）ならそのプールには何も書かずに断る。S4 に
    `allocation.imported` を残す。サービス版は `POST /v1/import`。
  - 解放：`sakutei allocator release` に `--intent` を付けると、同じ仕方でファイルから消す。authority の Release を
    通る解放はみな同じ道を通る。store は cooldown を持ち続ける。
  - store は「宣言の写しと作業の記録」に下がる（§19.9）。`write_back` の無いプールは今までどおり store が正。
- **#108 の 3 を置き換えた**: 旧 §19.7 の「現実から逆引きして再構築する手順」は、書き戻しで置き換えた。逆引きは機器ごとに
  読み方が要り、復元できない分が残る。書き戻しなら宣言を読むだけで同じ値に戻り、付け替わらない。
- **実装の形**: `AllocationAuthority` の前に置く包み（`internal/allocwriteback`）。常駐では Lookup キャッシュのさらに前に
  置き、ファイルは毎周読む（手元のファイルなので allocator のコールは churn 比例のまま、§19.6）。判断は毎回ファイルと
  store から取り、包み自身は何も覚えないので、一回性の実行と常駐で同じに動く。`review` のブランチは作業ツリーに触らずに
  （一時の index と `commit-tree`）作り、取り込まれていないブランチには積み重ね、取り込まれたら HEAD の上に作り直す。
- **見送り**: 解放した owner がファイルに残っている（`--intent` 無しの release・`review` で解放の取り込み前）のを「解放の
  取り込み待ち」と見分けること（今は「store に無い」としてそのプールの新しい払い出しも止める。安全側）。複数のシャードが
  同じリポジトリへ同時にコミットするときの git のロックの待ち合わせ。

## 受け入れテスト

- `tests/acceptance/allocation_write_back_test.go`：
  - `[AC-157-1]` commit：配るとファイル（素の YAML・`schema_version`）に書かれ、システムとして git にコミットされる。
    変わらなければコミットしない。
  - `[AC-157-2]` store を消すと Apply が止まり import を案内する（その間に同じ値を他に配らない）。import で同じ値に戻り、
    付け替わらない（S4 に残る）。食い違う import は何も書かずに断る。
  - `[AC-157-3]` ファイルと store が食い違うと、その owner の Apply が止まり `AllocationHeld` が出る。plan にも出る。
    どちらも書き換えない。
  - `[AC-157-4]` review：別のブランチにコミットし、取り込まれるまで Apply が止まる（plan・status に「書き戻しの取り込み
    待ち」）。取り込むと進む。
  - `[AC-157-5]` 解放するとファイルから消え（commit ではコミットされる）、store は cooldown を持つ。
  - `[AC-157-6]` file：git なしで書く。
- `internal/allocwriteback`：常駐の Lookup キャッシュの前で定常状態が store に問い合わせないこと・store に無い割当が
  あるプールは新しく配らないこと。`internal/allocator`：import の受け入れと断り（ファイル版・サービス版）。
