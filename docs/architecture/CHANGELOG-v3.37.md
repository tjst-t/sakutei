# CHANGELOG v3.36 → v3.37

**改訂日: 2026-10-28**

Redfish の FW の更新（provider `redfish-firmware`、#148）を、偽の Redfish サーバだけでなく qemu-bmc 0.11.0 の
UpdateService の模擬で確かめた改訂である（Backlog #168）。本物で見つけた違い（BMC の Task が `Payload` を返さない）に
合わせて、「この更新の BMC の Task」の見分け方を足した。

## 背景

- qemu-bmc 0.10.0 は UpdateService・TaskService を持たなかったので、redfish-firmware は偽のサーバ
  （`internal/redfish/redfishtest`）でしか確かめていなかった（§29.1 の「Redfish の FW の更新」の（8））。
- qemu-bmc 0.11.0 で UpdateService の模擬（FirmwareInventory の BMC・BIOS、SimpleUpdate、`UpdateService/upload` への
  multipart の送信、TaskService、BIOS の起動までの保留、障害注入の `firmware-image-rejected`・`firmware-update-fail`）が入った。
- 0.11.0 に向けると、BMC の Task には `Payload`（Redfish では任意の欄）が無く、Sakutei は SimpleUpdate を送った直後に
  「送ったのに TaskService にその Task が見えない」として halt するはずだった。Task の `Messages` には DMTF の Update の
  登録簿のメッセージ `Update.1.0.TargetDetermined`（MessageArgs が「部品の URI、イメージ」）がある。

## 決定（§29.1「Redfish の FW の更新」の（8））

- **Task の見分け方**：この更新の BMC の Task は、`Payload` があれば従来どおり（`TargetUri` と `JsonBody` の `Targets`・
  `ImageURI`）、無ければ Update の登録簿のメッセージ `TargetDetermined`（登録簿の版は問わない）で見分ける：部品が同じで、
  イメージが SimpleUpdate なら `image_uri`、multipart なら送ったファイルの名前（`image_uri` の最後の要素）のもの。
  `StartTime` による残りかすの除き方（起動の時刻 − 5 分）は変えない。どちらも無い BMC は従来どおり `staging` の後で halt
  （二重に送らない側）。
- **失敗の理由**：BMC の Task が失敗で終われば、メッセージの文に MessageId を添えて出す（例
  `Installation of image '…' to '…' failed. (Update.1.0.ApplyFailed)`）。イメージを断る 400 は従来どおり BMC の
  `@Message.ExtendedInfo` の文をそのまま出す。
- **再起動を宣言しない BIOS の更新**（決まりは変えず、振る舞いを書いた）：BMC の Task が `Completed`
  （`Update.1.0.AwaitToActivate`）で終わっても版は起動まで変わらない（部品の Status.State は `StandbyOffline`）。
  `reboot: none` では Sakutei は再起動を送らず、`rebooting` の相が `reboot_timeout` で halt し、RequiresTask
  （warning。続いた周数が stuck に達していれば critical）で通知の口に出る。人が `sakutei task cancel` して `reboot` を
  宣言し直すと、BMC の Task はもう済んでいるので更新を送り直さず、承認の後に再起動だけを送り、起動したあと版が合う。
- **偽のサーバ**：qemu-bmc に寄せて、Task の Messages に MessageId・MessageArgs（受けたとき `TargetDetermined`、終わるとき
  `UpdateSuccessful`・`AwaitToActivate`・`ApplyFailed`）を持たせ、`Payload` を返さない形（`UpdateOptions.NoTaskPayload`）と、
  受けた更新を 400 `Update.1.0.VerificationFailed` で断って Task を作らない口（`SetImageRejected`）を足した。

## 確かめたこと

- `make integration-redfish`（qemu-bmc 0.11.0、`ghcr.io/tjst-t/qemu-bmc:0.11.0@sha256:3ab6ea901b1ae8a510a2a140113fd1d80ad0f3ad2c26327ab04ef94f95bbdfc9`）
  が既存の 0〜9 段目と、FW の 10〜13 段目（`[AC-168-10]`〜`[AC-168-13]`：版の観測・BMC の SimpleUpdate と multipart・
  BIOS の保留と再起動・2 種の故障）を含めて合格した。手順と結果は `docs/providers/redfish.md` の「統合テスト」。
- 単体：`internal/redfish/update_test.go`（MessageId・MessageArgs・`Reason`）、
  `internal/redfish/system/firmware_test.go`（`TestFirmware_TaskWithoutPayload`・`TestFirmware_ImageRejectedAndApplyFailed`）。

## 見送ったこと

- **保留の理由を halt の文に出す**：`reboot: none` の halt の文は「the step did not complete (pending) within …」で、
  BMC が `StandbyOffline`・`AwaitToActivate` と言っていることまでは書かない。多相 Task の相の待ち（`CheckStep`）が理由を
  返す口を持たないため。部品の Status は Redfish でそのまま読める。
- **qemu-bmc の EventService（`TaskEvent`・`ResourceChanged`）を観測に使う**：観測 state レス（§3 の 1）のまま毎回
  TaskService を読み直す。
