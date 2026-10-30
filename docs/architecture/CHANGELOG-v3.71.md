# CHANGELOG v3.70 → v3.71

**改訂日: 2026-10-29**

proxmox-vm で、VM を作り直すときにデータのディスクを消さずに新しい VM へ引き継げるようにした改訂である（Backlog #204）。
「宣言で消すと決めたもののほかは消さない」「確信が持てないときは減算しない」（§3 原則 3）を、VM のディスクの単位に当てた。

## #204：proxmox-vm のディスクの keep と volume=（§29.1）

- **背景**: VM を新しいイメージから作り直して OS を更新する運用で、`ensure: absent` の Delete がディスクごと消すので、
  データのディスクを引き継げなかった。既にある volume を VM に付ける欄も無かった。
- **決定**（§29.1「proxmox-vm：作り直すときにデータのディスクを引き継ぐ」）:
  - `disks` の項目に `keep`（消さない印）と `volume=<storage>:<volume>`（slot が空のときに既にある volume を付ける）。
  - keep の印は Proxmox の VM の tags（`sakutei-keep-<slot>`）に置いて観測して比べる。`ensure: absent` は欄を持たないので、
    消す VM のどのディスクを残すかを現実の側で分かるようにする（§1.7）。
  - 付け替えは Proxmox の reassign（`move_disk` の `target-vmid`）。volume は消えず付け先の名前に変わる。同じノードの VM の
    間だけ、元の VM の slot のディスクは元の VM が止まっているときだけ（走っていれば何も変えずに待つ）。付いたあとは volume の
    名前を比べない。
  - volume を付ける Op は承認の要る壊す変更（`provider.ApprovalRequirer`）：予算に数え、planned_change の `create`・`update` の
    レーンで走る。
  - keep の印のある VM は、keep の slot のディスクと unused が残る間は消さない（Delete は止めて、残る volume を挙げて待つ）。
  - 付け先の無い volume（VM の `unusedN`）は List が `<クラスタ>/<VM 名>/unusedN` で返し、status の unmanaged に出す。
  - 途中で落ちたら、どこまで進んだかを知らせ、ディスクを消さない。次の周は現実から計画し直す。
  - 観測の正規形は keep の印のあるときだけ ` keep` が足される（今の宣言の比べ方は変わらないので ProviderVersion は据え置き）。
- **検証**: 偽の Proxmox VE（`pvetest` に volume の台帳・unused・`move_disk`・消すときの volume の扱い）の単体テストと受け入れ
  テスト `[AC-204-1]`〜`[AC-204-3]`（承認が無ければ付け替えない・create のレーンで付け替わり前の VM を消しても残る・keep の
  ディスクを持つ VM の absent は止めて消さない・外したままの volume が unmanaged に出る）。`make integration-proxmox` の段
  （Proxmox VE 9.2：データのディスクに書いた中身が新しい VM で読め、keep でないディスクは前の VM と一緒に消え、手で外した
  volume が unmanaged に出て、承認が無ければ付け直さない）。
- **見送り**: 同じ名前・vmid のままの作り直し（Proxmox は持ち主の VM の設定から外した volume を unused にし、VM を消すと unused
  も消すので、同じ vmid で消して作る間に volume を預ける先が無い）。走っている VM からの hot-unplug での取り外し。
