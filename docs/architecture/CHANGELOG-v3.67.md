# CHANGELOG v3.66 → v3.67

**改訂日: 2026-10-29**

network_policy の本体の実現先に proxmox-fw（Proxmox VE の VM の FW の規則、入りと出）を足した改訂である（Backlog #197。親のストーリー
「アドレスの重なる網を宣言から守る（網の domain・VM の FW・Proxmox の FW の実現先）」の 3 つ目。網の domain と実現先の口は v3.64・#195、
NIC の firewall と VM の FW の options は v3.65・#196）。実現先の口（`WithRealizer`）と、展開した resource がふつうの resource と同じに
収束する決まり（§7.6）は変えていない。

## #197：network_policy の実現先 proxmox-fw（§7.6・§29.1）

- **背景**: 利用者ごとに VM を 1 台置いて関所にし、片側の口を利用者の網、もう片側をサービスの網につなぎ、守りを VM の口ごとの Proxmox の
  FW で行う形を宣言から当てたい。v3.64 で口と向きは realizer に渡るようになったが、本体の実現先は nftables だけで（口を受けない）、VM の
  FW の規則（`/nodes/<n>/qemu/<id>/firewall/rules`）を扱う provider も無かった。
- **決定**:
  - **provider `proxmox-fw`**（§29.1）：1 つの resource ＝ VM 1 台の Sakutei が置いた規則の並び（`rules`）。ID は `<クラスタ>/<VM 名>/fw`
    （Local ID は provider をまたいで一意なので、proxmox-vm の `<クラスタ>/<VM 名>` とぶつからないように `/fw` を足す）。クラスタの一覧・触らない
    VM・観測用と書き込み用のトークン（§20.7）は proxmox-vm と同じ。範囲（クラスタの scope）は intent の `providers.proxmox-fw`（§7.7）。
  - **印と宣言の外の規則**：Sakutei の置いた規則は comment の頭の `sakutei: ` で見分け、印のある規則だけを書き換える。印の無い規則は消さず
    書き換えず、List が `<クラスタ>/<VM 名>/fw/<pos>` で返し status の unmanaged に出す（prune しない型）。
  - **順**：Sakutei の規則を並びの先頭に宣言の順で保ち、手の規則は後ろに元の順で残す。観測の `rules` に間の手の規則を `(unmanaged) <文>`
    として含めるので、順のずれと上に入った手の規則は drift になる。
  - **Apply** は 1 手ずつ読み直して `digest` を付けて書く（全部を消して作り直さない）。Proxmox VE 9.2 で確かめた決まり（作ると先頭・
    `enable` の既定 0・`moveto` は元の位置の前・違う `digest` は断る）に合わせ、偽の Proxmox（`internal/proxmox/pvetest`）も同じに振る舞う。
  - **予算**：規則を置く・変える・外す Op は FW の変更として破壊的（§13）。
  - **realizer `proxmox-fw`**（§7.6）：許可が通る口と向きごとに `in`・`out` の規則と `iface=netN`（ホストは to が自分の網の口で in・from が
    自分の網の口で out、境界は from の口の in と to の口の out）、アドレスの族ごとに 1 規則、reason を comment に。口を書かないホストは口の無い
    `in`、口を書かない境界は断る。`settings` は受けない（既定の拒否は proxmox-vm の `firewall`、#196）。
- **検証**: 単体（規則の文 `internal/pvefwrule`・realizer・provider）、受け入れ（偽の Proxmox。`[AC-197-1-1]`〜`[AC-197-1-2]`・
  `[AC-197-2-1]`〜`[AC-197-2-5]`）、`make integration-proxmox` の「network_policy の実現先 proxmox-fw」段（Proxmox VE 9.2。関所の VM の
  利用者側の口は出る向きの KDC・DNS・LDAP・NTP、サービス側の口は入る向きの 443 だけ。規則が口と向きと印と reason つきで宣言の順に入り、
  手の規則は後ろに残って unmanaged に出て、手の変更は観測用のトークンでも drift に見え、戻しても手の規則を消さない）。
- **見送り**: datacenter とノードの FW・security group・IPSet と alias の中身・`type: group` の規則、Proxmox の規則の数の上限の扱い、
  パケットが実際に止まることの確かめ（試験の VM は止めたまま）。
