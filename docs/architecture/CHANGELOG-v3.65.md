# CHANGELOG v3.64 → v3.65

**改訂日: 2026-10-29**

proxmox-vm に NIC の口の FW・MTU と、VM の FW の options を宣言する口を足した改訂である（Backlog #196。親のストーリー
「アドレスの重なる網を宣言から守る（網の domain・VM の FW・Proxmox の FW の実現先）」の 1 つ目）。既存の NIC の `mac`・`model` と
cloud-init の `ciupgrade` と同じ「書いたときだけ比べる」扱いで、設計の向きは変えていない。

## #196：proxmox-vm の NIC の `firewall`・`mtu` と VM の FW の options（§29.1）

- **背景**: 利用者ごとに VM を 1 台置いて関所にし、守りを VM の口ごとの Proxmox の FW で行う形を宣言で書きたい。proxmox-vm は
  NIC の `firewall=1` を「宣言しないキー」として残すだけで比べず、VM の FW（`/nodes/<n>/qemu/<id>/firewall/options`）は扱わなかった。
- **決定**（§29.1「proxmox-vm の NIC の firewall・mtu と VM の FW の options」）:
  - `nics` の項目に `firewall=<0|1>` と `mtu=<1-65520>`（`1` は bridge の MTU）。書いたときだけ比べ、書き換えるときも書かない
    ものは今の値を残す。`mtu` は virtio の口だけに効くので、`model` に virtio 以外と `mtu` を両方書けば読み込みで断る。
  - フィールド `firewall`：VM の FW の options のうち `enable`・`policy_in`・`policy_out`・`ipfilter`・`macfilter` を
    `key=value` で書く。書いたキーだけを比べ、違うキーだけを `PUT /firewall/options` で書く。書かなければ比べない。
  - 観測：Read は options をいつも読む（desired を見ない、ADR-0002）。`GET` は書かれたキーだけを返すので、書かれていない
    キーは Proxmox の既定（pve-firewall が VM の口に使う値：`enable=0`・`policy_in=DROP`・`policy_out=ACCEPT`・`ipfilter=0`・
    `macfilter=1`）で埋めて 5 つのキーの正規形にする。NIC の観測の正規形は `firewall` を 1 のときだけ、`mtu` を書かれていれば出す
    （正規形が変わったので ProviderVersion を `proxmox-vm-v2` に上げた）。
  - 資格（§20.7）：options を読むのは `VM.Audit`（`PVEAuditor` に入っている。観測用のトークンはそのまま）、書くのは
    `VM.Config.Network`（`PVEVMAdmin` に入っている）。
  - FW の規則・datacenter とノードの FW を有効にするのは扱わない（規則は network_policy の実現先 proxmox-fw、親のストーリー）。
- **検証**: 単体と受け入れ（偽の Proxmox。`[AC-196-1]`・`[AC-196-2]`）、`make integration-proxmox` の「NIC の firewall・mtu と
  VM の FW の options」段（Proxmox VE 9.2。複製した VM に書かれ、手で変えると戻り、書かないキー・VM は比べない。観測用の
  `PVEAuditor` のトークンで drift が見える）。
- **見送り**: 走っている VM の `mtu` を変えること（Proxmox が口を抜き差しし、ゲストが応じなければ断る。止めてから変える）の
  特別な扱い。`dhcp`・`ndp`・`radv`・`log_level_*` の options（要るときに足す）。
