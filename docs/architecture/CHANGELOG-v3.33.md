# CHANGELOG v3.32 → v3.33

**改訂日: 2026-10-28**

proxmox-vm で作る VM を Proxmox の pool に入れる口を足し、常駐のデーモンが `--cause-state` を受けない決まりを書いた改訂である
（Backlog #165。親のストーリー「proxmox-vm の使い勝手：宣言で VM を消す・宣言の外の VM を unmanaged に出す・pool に入れる」の、
pool に入れる部分と、常駐の `--cause-state` の確かめ）。

## #165：proxmox-vm の pool（§29.1）

- **背景**:
  - proxmox-vm（#143）で作った VM は Proxmox の pool に入らず、API トークンを pool の ACL だけで絞れなかった（root に近い
    トークンを渡すしかなかった）。
- **決定**（具体は §29.1「proxmox-vm の pool・常駐の `--cause-state`」）:
  - フィールド `pool`（intent に書く）。作るときに渡し（複製・作成の `pool`）、観測（`/cluster/resources` の `pool`）と違えば
    drift として移す（`PUT /pools`、`allow-move=1`。`""` は pool から外す）。宣言しなければ触らない。
  - pool は作らない。無い pool・権限が無いのは Apply の PermanentError（Proxmox の 500 も「無い」と言っていれば止める）で、
    要る権限を文言に出す。
  - pool に絞ったトークンの権限：`/pool/<pool>` に `PVEVMAdmin` と `PVEPoolUser`（`Pool.Audit`）、ストレージに
    `PVEDatastoreUser`、bridge に `PVESDNUser`、テンプレートに `PVETemplateUser`。`Pool.Audit` が無いと pool が観測に
    載らない（本物の Proxmox VE 9.2 で確かめた）。pool の出し入れには `Pool.Allocate`。
  - pool から手で外された VM は、pool に絞ったトークンには見えない。作り直そうとして vmid がぶつかるのを、「vmid が見えない
    VM に使われている」と PermanentError で知らせ、作り直さない。
- **見送り**:
  - pool を作る・ACL を張る口（Proxmox の設定を宣言で持つ provider は別の仕事）。
  - Proxmox VE 8.1 より前の `PUT /pools/{poolid}`（allow-move が無い）での出し入れ。
  - トークンに見えない VM を pool に戻すこと（見えないものは観測できない：unknown ≠ absent の原則のとおり、作り直しも
    しない）。

## #165：常駐の `--cause-state`（§29.1）

- 常駐のデーモンが `--cause-state` を受けないのは**意図どおり**：常駐は前の周の revision をメモリに持ち（`Daemon.prevRev`）、
  原因タグはそれと比べる。ファイルは一回性の plan・reconcile・status が回をまたぐためのもの。
- これまでは `flag provided but not defined` で落ちていた。黙って無視せず、理由を言って断る（終了コード 1）。README の CLI と
  試用の手順にも書いた。

## 受け入れテスト

`tests/acceptance/proxmox_vm_test.go`（偽の Proxmox VE。pool と、pool に絞ったトークンを足した）:

- `[AC-165-1]` pool を宣言した VM は複製のときに pool に入り（あとから `PUT /pools` しない）、手で pool から外すと `status` が
  pool の drift を出し、`reconcile` が戻す。
- `[AC-165-2]` pool に絞ったトークンで作れ、`reconcile` は適合、`status --json` はその VM だけを適合に出す。pool から手で
  外すと、作り直さずに「見えない VM が vmid を使っている」と知らせる。
- `[AC-165-3]` 常駐は `--cause-state` を、前の周をメモリに持つと言って断る。

単体：`internal/proxmox/pool_test.go`（複製・作成の pool、手で外す・移すと戻る、`""` で外す、宣言しなければ触らない、無い
pool は PermanentError、pool に絞ったトークン、`Pool.Audit` の無いトークン、pool の名前の検査）。

統合：`make integration-proxmox` の「pool に入れて作る・pool に絞ったトークン」段（pool と privsep のトークンを作り、そのトークンで
テンプレートから作って適合、トークンには pool の外の VM が見えない、手で外すと知らせる、root のトークンでは drift を戻す、
無い pool はエラー）。
