# 採番の書き戻しのファイル（allocation file）

採番のプールに `write_back: true` を書くと、そのプールが払い出した結果（どの owner にどの値を配ったか）を
**宣言の側（intent のリポジトリ）のファイル**に書き戻し、以後はそのファイルを割当の正として読む（Backlog #157、
設計書 §19.7・§24.5）。採番の store（`--alloc-store` のファイル・`sakutei allocator` のサービス）は、ファイルの
**写し**と作業の記録（cooldown・仮予約）に下がる。ツールと store を失っても、宣言から同じ値に戻せる。

```yaml
pools:
  - name: vm-ids
    range: "100-199"
    write_back: true      # 既定は false（今までどおり store だけ）
```

## ファイルの形

プールごとに 1 つ。素の YAML（JSON の `{ }` は使わない）で、store の内部の形（世代・仮予約の期限など）は出さない。

```yaml
# sakutei:generated allocation-write-back
# 採番の結果の書き戻し（§19.7）。この pool の割当の正はこのファイルで、採番の store はその写しです。
# 書式は docs/allocation-file.md。store を失ったら `sakutei allocator import` でここから作り直します。
schema_version: 1
pool: "vm-ids"
allocations:
  - owner: "proxmox-vm/web-1#vmid"
    value: 100
    allocated_at: "2026-10-28T09:00:00Z"
  - owner: "fake/net-a#vlan"
    value: 101
    allocated_at: "2026-10-28T09:00:05Z"
```

| キー | 意味 |
| --- | --- |
| `schema_version` | 書式の版。今は `1`。知らない版のファイルは読まずに、そのプールを止める |
| `pool` | プールの名前。ファイル名（`<pool>.yaml`）のプールと違えば読まない |
| `allocations` | 割当の一覧。無ければ空（`allocations:` だけの行） |
| `owner` | 誰に配ったか。`<provider>/<local id>#<field>`（下を見る） |
| `value` | 配った整数（テンプレート `template: "192.0.2.{n}"` を通す前の値） |
| `allocated_at` | 書き戻した時刻（RFC 3339、UTC）。読むときは省いてよい |

- **並び**は `value` の小さい順（同じ割当なら同じバイト列になり、差分は来た値・去った値として読める）。
- 文字列は二重引用符で書く（owner の `#` を YAML がコメントと読まないように）。
- 先頭の `# sakutei:generated ...` の行は YAML のコメントで、読むときには関わらない。本文のハッシュは持たない
  （Sync Mode のスナップショットと違い、このファイルは正なので、人が直してよい。直した内容は store と照らされる）。
- 読めないファイル（壊れた YAML・owner や value の重複・整数でない値）は推し量らず、そのプールの要求をすべて止める。

### owner の書き方

`<provider>/<local id>#<field>`。例：intent の

```yaml
resources:
  - provider: proxmox-vm
    id: web-1
    fields:
      vmid: { from_pool: vm-ids }
```

の owner は `proxmox-vm/web-1#vmid`。**scope は入れない**（シャードの組み替えで scope が動いても、local id が同じなら
同じ値を持ち続けるため、§4.2）。合成リソースの値は、合成の親の owner で 1 つ持つ（展開した子は親の値を受け継ぐ）。

## 置き場とコミットの仕方（設定）

置き場とコミットの仕方は宣言のリポジトリの事情で決まるので、intent ではなく設定（`--alloc-write-back <file>`、
JSON / YAML）に書く。`plan`・`reconcile`・`status`・`graph`・常駐・`sakutei allocator release / import` に同じ
ファイルを渡す。渡さなければ既定（下の表の既定）。

```yaml
path: generated/allocations     # 既定。intent ディレクトリの親（リポジトリの根）からの相対、または絶対パス
mode: commit                    # commit（既定）| review | file
repo: /srv/intent               # 既定: path を含む git の作業ツリー
branch: sakutei/allocation-write-back   # review のブランチ（既定）
remote: origin                  # 任意: コミットのたびに push する
ssh_key: /etc/sakutei/deploy-key        # 任意: push に使う鍵（GIT_SSH_COMMAND）
committer_name: sakutei-sync            # 既定は Sync Mode と同じ
committer_email: sakutei-sync@localhost
```

| mode | 動き |
| --- | --- |
| `commit` | ファイルに書いてから値を使う。その周の終わりに、書いたファイルだけを intent のブランチ（作業ツリーの今のブランチ）へ 1 つのコミットにする。Sync Mode のコミット（§24.5）に相乗りする：コミッターはシステム（既定 `sakutei-sync`）・署名なし・中身が変わらなければコミットしない。trailer は `Sakutei-Allocation: <pool>`。作業ツリーでなければ書くだけ |
| `review` | intent のファイルには書かず、別のブランチ（`branch`）に更新したファイルをコミットする（作業ツリーには触らない）。取り込みは人（マージ・PR）。取り込まれて intent の側のファイルに値が現れるまで、その値を使う Apply を止める（`plan`・`status` に「書き戻しの取り込み待ち」と出る）。その間も値は store で配ったままなので、同じ値を他に配らない。まだ取り込まれていないブランチには積み重ね、取り込まれた後は HEAD の上に作り直す |
| `file` | ファイルに書くだけ。git は使わない |

## 読み方（ファイルが正）

| ファイル | store | どうなるか |
| --- | --- | --- |
| ある | 同じ値 | そのまま使う |
| ある | 違う値 | その owner を使う Apply を止めて知らせる（`AllocationHeld`）。どちらにも勝手に寄せない。人がどちらかを直す |
| ある | 無い | Apply を止め、`sakutei allocator import` を案内する |
| ある | 無い（その値が cooldown 中＝解放済み） | Apply を止め、「解放の取り込み待ち」と知らせる。import は案内しない（下の「解放」） |
| 無い | 配った（または今配る） | `commit`・`file` は書き戻してから使う。`review` は取り込まれるまで止める |

- ファイルの割当のうち 1 つでも store に無い・store と食い違うあいだは、そのプールから**新しい値を配らない**
  （store がファイルの値を空きと思って別の owner に配るのを防ぐ）。
- 止めた要求は waiting-allocation と同じく、そのリソースと下流だけを skip する。ほかのリソースは収束を続ける（§19.9）。
  一回性の `reconcile` は waiting-allocation と同じく終了コード 0 のまま、止めた理由を `AllocationHeld`（level の Event）で出す。

## store を作り直す：`sakutei allocator import`

```console
$ sakutei allocator import --intent intent/ --store alloc.json [--pool vm-ids] [--alloc-write-back wb.yaml] [--audit-log audit.jsonl]
pool vm-ids: imported 2 allocation(s) from generated/allocations/vm-ids.yaml (0 already in the store)
```

- intent の `write_back: true` のプール（`--pool` でその 1 つ）のファイルから store を作り直す。サービスなら `--url`。
- 既に store にある記録と同じなら何もしない。食い違う（owner が別の値を持つ・値を別の owner が持つ・値が cooldown 中＝
  解放済み・範囲の外）なら、**そのプールには何も書かずに断る**（終了コード 1）。
- 作り直したことを S4 の監査（`--audit-log`、kind `allocation.imported`）に残す。

## 解放

人の `sakutei allocator release` に `--intent`（と `--alloc-write-back`）を付けると、store の解放のあと、同じ仕方
（commit / review / file）でファイルから owner を消す。store は cooldown（作業の記録）を持ち続ける。宣言から消えた
ことによる解放も、authority の Release を通る限り同じ道でファイルから消える（v1 には自動の解放の経路は無い、§19.5）。
`--intent` を付けずに解放すると、ファイルにはその owner が残り、そのプールは「store に無い」として止まる（import は
cooldown 中の値を断るので、誤って戻ることはない）。

`review` の仕方では、解放（ファイルから owner を消すこと）もレビュー用のブランチにコミットされ、人が取り込むまで作業
ツリーのファイルにはその owner が残る。そのあいだ store ではその値が cooldown 中（解放済み）なので、照合はこれを
「store を失った」ではなく **解放の取り込み待ち**（`ErrReleaseAwaitingReview`）として止め、そのプールから新しい値を
配らない。知らせは取り込みを促し、`sakutei allocator import` は勧めない（import は cooldown 中の値を断る）。取り込めば
解ける（#167）。`commit`・`file` で同じ形が出たら（ファイルの書き換えに失敗した・手で戻した）、その行を手で消す。
