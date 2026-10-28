# CHANGELOG v3.49 → v3.50

**改訂日: 2026-10-28**

HTTP の管理 API を使う provider で、API のパスの前置きを provider の設定で変えられるようにする改訂である（Backlog #160。
親のストーリー「FW の更新と入れ直しを宣言で扱う」）。設計の向きは変えていない。

## #160：API のパスの前置きを設定で変える（`api_base_path`、§29.1）

- **背景**: 実機と試験用の模擬で、管理 API のパスの前置き（Proxmox VE の `/api2/json` など）が違うことがある。前置きは
  provider の中に決め打ちで、同じ intent を実機と模擬の両方に当てられなかった。
- **決定**:
  - **proxmox-vm**：クラスタの設定に `api_base_path`（既定 `/api2/json`）。
  - **s3-bucket**：endpoint の設定に `api_base_path`（既定は前置き無し）。要求のパスは path 形式なら `<前置き>/<bucket>`、
    virtual-host 形式なら `<前置き>/`、ListBuckets は `<前置き>/`。SigV4 の署名は前置きを含むパスで作る。`endpoint` には
    今までどおりパスを書かない。
  - **置き場所**：つなぎ方なので provider の設定に置き、intent には書かない（§1.7、#158 の「provider の設定はつなぎ方だけ」）。
  - **値の検査**：`/` で始まるパスだけ。末尾の `/` は落とし、`/` は前置き無し。クエリ・フラグメント・`%`・空白・空の段・
    `.`・`..` の段は読み込みの時点でエラー（`internal/apibase`）。
  - **持たない provider**：metrics-probe は `url` をパスまで丸ごと書くので要らない。Redfish はサービスルートが規格（DSP0266）で
    `/redfish/v1` に決まり、provider は BMC の返す `@odata.id`（根からの絶対パス）を辿るので、前置きを足しても辿った先には
    効かない。置き場所が BMC ごとに違うものは今までどおり `system`・`manager` で指す。
- **見送り**:
  - 前置きを書き換えて転送する逆プロキシの下の s3-bucket（署名のパスが合わない）。
  - 通知の口・allocator の HTTP の口（どれも URL を丸ごと書く）。

## 影響

- §29.1 に 1 行を足した。
- 設定のキーを足しただけで、省けば今までどおり。intent・plan の JSON・provider の口は変えない。
- 単体テスト：`internal/apibase`、`internal/proxmox`（前置きを変えた偽の Proxmox で作る・観測する・一覧する、既定の前置きの
  ままでは観測できない、設定の読み込み）、`internal/s3bucket`（要求のパス、前置きを変えた偽の S3 で作る・観測する・一覧する、
  設定の読み込み）。偽サーバ `pvetest`・`s3test` に前置きの `BasePath` を足した。
- docs：`docs/providers/proxmox-vm.md`・`s3-bucket.md`・`metrics-probe.md`・`redfish.md`。
