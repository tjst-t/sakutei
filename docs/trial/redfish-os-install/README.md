# Redfish と Workflow で OS を入れる（自動インストールの ISO は利用者が用意する）

Redfish の BMC provider（[docs/providers/redfish.md](../../providers/redfish.md)）と最小 Workflow（§6.3、`internal/workflow`）を組み合わせて、
「CD に ISO を差す → 差さったことを確かめる → **次の 1 回だけ** CD から起動 → 再起動 → インストーラが電源を切るのを待つ → 電源を入れる（ディスクから起動）→ 入れた OS が目印つきで起動したのを確かめる → CD を抜く」を 1 台に回す手順。2026-10-22 に qemu-bmc 0.9.0 で試した。
最後の確かめ（ゲスト OS の観測、[guest-os provider](../../providers/guest-os.md)）は Backlog #86 で、一度きりの操作（1 回だけ CD から起動・再起動、Workflow の `action` 手順）と
**自動導入の応答ファイルを宣言から作る**（[template-file provider](../../providers/template-file.md)）は Backlog #139 で足した。同じ形を `make integration-guestos`（qemu-bmc 0.11.0 を 3 台、成功・失敗する導入）で確かめている。

## 線引き：何を Sakutei がして、何を利用者が用意するか

インストーラそのもの（自動インストールの ISO・PXE・kickstart / autoinstall / preseed の仕組み）は **Sakutei の責務の外**（設計 §29.3 (c)：provisioner 実体は持たない）。
Sakutei は BMC の「あるべき姿」と、その順番と、台ごとの応答ファイルの中身だけを扱う。

| Sakutei がする | 利用者が用意する |
|---|---|
| CD に ISO を差しておく（`redfish-virtual-media`） | **人の手を介さずに入れて、終わったら電源を切る ISO**（BMC から見えるパスか URL に置く）。応答ファイルを取りに行く口を持つもの |
| 次の 1 回だけ CD から起動し、再起動し、電源が切れるのを待って電源を入れる（Workflow の `action`・`apply`・`wait` + `redfish-system`） | BMC の一覧（`--redfish-config`）とパスワードの file SecretBackend |
| 台ごとの**応答ファイル**（ホスト名・アドレス・Sakutei の ssh の公開鍵・導入の目印）を、利用者の雛形から宣言どおりに書き出し、保つ（`template-file`） | 応答ファイルの**雛形**（OS の種類ごと。text/template）と、書き出した置き場をインストーラに届ける手段（HTTP で配る・ISO に入れる） |
| 入れた OS が目印つきで起動したかを ssh で観測し、確かめるまで先へ進まない（`guest-os`） | 入れた OS に sshd・Sakutei の鍵・**導入の目印**（ファイル）を置くこと（インストーラの仕事。値は応答ファイルから） |
| 終わったあとの姿（電源オン・上書きなし・CD は空）を毎周保つ | |

ISO が満たすべき約束は 3 つ：**応答ファイルを取って、その値で入れる**、**入れ終わったら電源を切る**（下の「なぜ電源を切るのか」）、**入れた OS に目印を置き、ssh で読めるようにする**。
Workflow は電源オフを「インストーラが終わった」の合図にし、目印の観測を「導入が済んだ」の合図にする（インストーラが失敗しても電源を切る ISO を、電源オフだけで成功と見なさない）。

**なぜ電源を切るのか**：起動先の上書き Once は BMC が「次の 1 回の起動」で使い切る。実機ではインストーラが再起動すればそのままディスクから起動するので、手順の 5・6（電源オフを待つ・電源を入れる）は要らない。
ただし qemu-bmc は Once を QEMU を立ち上げるとき（電源 On）に使い切り、ゲストの中からの再起動では QEMU の起動順が変わらない（もう一度 CD から起動する）。試用と統合テストでは、インストーラが電源を切り、手順 6 で電源を入れ直す形にしている。

## 置いてあるもの（docs/trial/redfish-os-install/）

| ファイル | 中身 |
|---|---|
| `intent/os.yaml` | 最終形の宣言・応答ファイルの宣言と Workflow `os-install`（下の「intent」）。loader で読めることを `internal/loader` のテスト `TestRedfishOSInstallTrialIntentLoads` が確かめる |
| `templates/alpine-answers.sh.tmpl` | Alpine の自動導入の**応答ファイルの雛形**（text/template。`autoinstall.start` が sh で読む形）。`template-file` が値を入れて `answers/<UUID>` に書き出す。単体・受け入れ・統合のテストもこの雛形を使う |
| `topology.clab.yml` | qemu-bmc 0.11.0 を 1 台（`VM_DISK_SIZE` 2G・`VM_MEMORY` 512、`SYSTEM_UUID` は応答ファイルの ID）。BMC は `172.31.85.11`、入れた OS の ssh は QEMU の user ネットの hostfwd で `172.31.85.11:2222` |
| `bmcs.yaml` | BMC の一覧（`insecure_skip_verify: true`、パスワードは SecretRef `file/bmc/node1`） |
| `hosts.yaml` / `providers.yaml` | 入れた OS を観測する `guest-os` の対象の一覧（鍵は SecretRef `file/ssh/node1`、known_hosts）と `--provider-config`（`guest-os` の目印・`template-file` の雛形と書き出す先） |
| `make-iso.sh` / `autoinstall.start` | Alpine 3.24 の virt ISO を自動インストールの ISO に作り直すスクリプト（試用で使ったもの。下の「ISO を作る」） |

**`make-iso.sh` と `autoinstall.start` は試用のための補助で、Sakutei の一部ではない。** リポジトリの「Go の標準ライブラリだけ（外部依存なし）」の決まりは Go のコードの話で、
この docs の下のシェルはその外にある（curl・xorriso・docker を使い、ネットワークから ISO とパッケージを取る）。`make check`・`make test`・CI からは呼ばない。

## 応答ファイル（template-file、#139）

台ごとに違う値（ホスト名・アドレス・Sakutei の ssh の公開鍵・導入の目印）は ISO に焼かず、宣言から**応答ファイル**を作ってインストーラに取りに来させる。ISO は何台にも使える 1 つでよい。

- 宣言は `template-file` のリソース（下の intent の 3 つ目）。ID は書き出す先（`output_dir`）からの相対パスで、ここでは台の UUID（インストーラが DMI の UUID で引けるように）。
- 雛形は `templates/alpine-answers.sh.tmpl`。`.Hostname`・`.Address`・`.SSHAuthorizedKeys`・`.InstallMark`・`.Vars.<名前>`（宣言の `var.<名前>`）を使い、`shquote` で sh の値にする。
  ほかの OS なら、その OS の応答ファイル（kickstart・cloud-init の user-data・preseed など）の雛形を書けばよい。
- **秘密は書き出さない**：値に SecretRef や秘密鍵を書くと読み込みの時点で断る。書くのは公開鍵だけ。入れる OS のホスト鍵（秘密鍵）は応答ファイルでなく ISO に入れている（`SSH_HOST_KEY`）。
- 書き出したファイルは収束ループが毎周確かめ、手で直されたり雛形が変わったりすれば書き直す。インストーラに届けるのは利用者の側で、試用では `answers/` を HTTP で配る（下の「回し方」）。
- `autoinstall.start` は `ANSWER_URL`（`{uuid}` は DMI の UUID に置き換わる）から応答ファイルを取り、`HOSTNAME`（入れた OS の `/etc/hostname`）・`ADDRESS` と `GATEWAY`（eth0 の静的なアドレス）・
  `SSH_AUTHORIZED_KEYS`（root の鍵、sshd を動かす）・`MARK`（目印）で入れる。取れなければ（2 分）電源を切らずに止まる（Workflow は手順 5 の `timeout` で halt）。

## ISO を作る（試用の例：Alpine 3.24）

実機や別のディストリビューションでは、それぞれの自動インストールの仕組み（kickstart・autoinstall・preseed など）で「応答ファイルを取って入れ、電源を切る」ISO を用意すればよい。
試用では Alpine 3.24.3 の virt ISO（`alpine-virt-3.24.3-x86_64.iso`）を次のように作り直した。

1. **apkovl**（`localhost.apkovl.tar.gz` を ISO のルートに置くと、live 環境の `/etc` に重なる）に `/etc/local.d/autoinstall.start` を入れ、
   `default` の runlevel で `local` を動かす。スクリプトは `ANSWER_URL` があれば eth0 を DHCP で上げて応答ファイルを取り、`setup-alpine -e -f <回答>` のあと `ERASE_DISKS=/dev/vda setup-disk -m sys -s 0 /dev/vda` で入れ、
   入れた OS に目印（`/etc/sakutei-installed`、中身は応答ファイルの `MARK`）と起動時にシリアルへ出す `hello.start` を置いて `poweroff` する。失敗したときは電源を切らずに止まる。
   公開鍵（応答ファイルの `SSH_AUTHORIZED_KEYS`、または `make-iso.sh` の `SSH_AUTHORIZED_KEYS`）があれば、入れた OS に openssh を入れて sshd を動かし、root にその鍵で入れるようにする。`SSH_HOST_KEY` を渡すとそれをホスト鍵にする（known_hosts を前もって書ける）。
2. **apkovl を使うときは runlevel を自分で全部書く**。既定の runlevel が入らないので、`sysinit` に `modloop` を入れる（devfs・dmesg・mdev・hwdrivers・modloop）。
   入れ忘れるとカーネルのモジュールが使えず、インストールが通らない。
3. **Alpine の virt ISO にはカーネルのパッケージが無い**。`setup-disk -m sys` はディスクに `linux-virt` を入れるので、`linux-virt`・`syslinux`・`mkinitfs`・`e2fsprogs`・`sfdisk` などと
   その依存を `apk fetch -R` で取り、ISO の中のリポジトリ `/extra/x86_64/` に置く（索引は公式の署名つきの `APKINDEX.tar.gz` をそのまま置く）。スクリプトは
   `/etc/apk/repositories` に `/media/cdrom/extra` を足してから `setup-disk` を呼ぶ。
4. syslinux の起動メニューに `SERIAL 0 115200` と `console=ttyS0,115200` を足す（qemu-bmc のシリアルで進み具合を見るため。要らなければ省ける）。
5. 起動の設定（El Torito・isohybrid）は元の ISO のまま、`xorriso -boot_image any replay` でファイルだけ足して書き出す。

```sh
# curl・xorriso・GNU tar・docker（alpine:3.24 で apk fetch）が要る。sudo が要るなら DOCKER="sudo docker"
W=/tmp/sakutei-os; mkdir -p $W/iso
ssh-keygen -q -t ed25519 -N '' -f $W/node1-key        # Sakutei が入る鍵（公開鍵は応答ファイルへ）
ssh-keygen -q -t ed25519 -N '' -f $W/node1-hostkey    # 入れる OS のホスト鍵（秘密なので ISO に入れる）
ANSWER_URL='http://172.31.85.1:8080/{uuid}' SSH_HOST_KEY=$W/node1-hostkey \
  docs/trial/redfish-os-install/make-iso.sh $W/iso/sakutei-autoinstall.iso
# 手元の ISO を使うなら ALPINE_ISO=/path/to/alpine-virt-3.24.3-x86_64.iso、取ったものを残すなら CACHE_DIR=<dir> を足す
```

`ANSWER_URL` の `172.31.85.1` は lab のネットワーク（`topology.clab.yml` の `172.31.85.0/24`）のゲートウェイ＝ホスト。インストーラは QEMU の user ネットから外へ出て、ホストの HTTP に届く。
応答ファイルの `install_mark`（目印）は intent の手順 7 の `marker.installed` と同じ値にする。**導入ごとに変える**（入れ直すときは intent の両方の値を変える。ISO は作り直さなくてよい）と、前の導入で入った OS を取り違えない。
応答ファイルの `var.fail: disk`（何も入れずに電源を切る）・`var.fail: marker`（入れるが目印を置かない）は失敗の試し用（`make integration-guestos` が使う）。`ANSWER_URL` を渡さなければ、
前のとおり `INSTALL_MARK`・`SSH_AUTHORIZED_KEYS`・`AUTOINSTALL_FAIL` で値を ISO に焼ける（台ごとに ISO を作る）。

BMC を通さずに ISO だけを確かめるなら、qemu で直に回せる（2026-10-23、インストールして電源が切れるまで約 40 秒、そのあとディスクから起動して目印が出た。`ANSWER_URL` なしで作った ISO で）。

```sh
qemu-img create -f qcow2 d.qcow2 2G
qemu-system-x86_64 -accel kvm -m 512 -drive file=d.qcow2,if=virtio -cdrom sakutei-autoinstall.iso -boot d \
  -no-reboot -display none -monitor none -serial file:inst.log     # SAKUTEI-AUTOINSTALL: done, powering off で終わる
qemu-system-x86_64 -accel kvm -m 512 -drive file=d.qcow2,if=virtio -boot c \
  -display none -monitor none -serial file:boot.log                # SAKUTEI-INSTALLED-OS: booted from disk が出る（Ctrl-C で止める）
```

## intent

`intent/os.yaml`（YAML はブロックスタイルで書く。`{ … }` / `[ … ]` のフロースタイルは loader が読まない）。`ssh_authorized_keys` は `$W/node1-key.pub` の中身に置き換える。

```yaml
resources:
  # インストールが済んだあとの「あるべき姿」: 電源オン・起動先の上書きなし（ディスクから起動）
  - provider: redfish-system
    id: node1
    fields:
      power: "On"
      boot_enabled: Disabled
  # 終わったら CD は抜く（Workflow が done になるまでは Workflow に任せ、収束ループは触らない）
  - provider: redfish-virtual-media
    id: node1/cd
    fields:
      image: ""
  # 自動導入の応答ファイル（#139）。雛形 templates/alpine-answers.sh.tmpl に値を入れて
  # answers/<UUID> に書き出す（UUID は topology.clab.yml の SYSTEM_UUID。インストーラが
  # DMI の UUID で引く）。公開の値だけを書く（秘密鍵は書けない）
  - provider: template-file
    id: 6f1b7c52-0e1a-4d2b-9c11-000000008501
    fields:
      template: alpine-answers.sh.tmpl
      hostname: node1
      address: 10.0.2.15/24
      var.gateway: 10.0.2.2
      ssh_authorized_keys: ssh-ed25519 REPLACE-WITH-node1-key.pub sakutei
      install_mark: sakutei-trial-1

workflows:
  - workflow: os-install
    applicant: trial
    target:
      provider: redfish-system
    # 終わりの姿（#209）。workflow store が無い（一回性の reconcile に --workflow-store を
    # 渡さない）・store を失ったときは、これが観測で満たされれば導入は済んだと見て、
    # node1 と CD をふつうに宣言どおりに保つ（常駐は入れ直さずに done と書く）。
    # 満たされない・ssh で届かない間は Workflow に任せて触らない
    done_when:
      resource: guest-os/{target}
      fields:
        os_id: alpine
        marker.installed: sakutei-trial-1
    strategy:
      order: one-at-a-time
    stages:
      - name: install
        targets:
          - node1
    steps:
      # 1. CD（別の provider）に自動インストールの ISO を差す
      - apply:
          resource: redfish-virtual-media/{target}/cd
          fields:
            image: /iso/sakutei-autoinstall.iso
      # 2. 差さったことを確かめる（前提条件。満たさないまま 5 分過ぎたら halt）
      - require:
          resource: redfish-virtual-media/{target}/cd
          fields:
            image: /iso/sakutei-autoinstall.iso
          timeout: 5m
      # 3. 次の 1 回だけ CD から起動する（#139。起動先の上書き Once。BMC が次の起動で戻す）
      - action:
          op: boot_once
          args:
            target: Cd
      # 4. 再起動する（既定 GracefulRestart。電源が切れていれば電源 On）。インストーラが走る
      - action:
          op: reset
      # 5. インストーラが終わって電源を切るのを待つ
      - wait:
          fields:
            power: "Off"
          timeout: 15m
      # 6. 電源を入れる（上書きは使い切ったので、ディスクから起動する）
      - apply:
          power: "On"
      # 7. 入れた OS が目印つきで起動したのを確かめる（#86。電源が切れただけでは成功と
      #    見なさない）。guest-os provider が ssh で /etc/os-release と目印を読む。目印の値は
      #    応答ファイルの install_mark（導入ごとに変える）、hostname は応答ファイルのホスト名。
      #    届かない・目印が違うまま 10 分過ぎたら halt（インストーラの失敗）
      - require:
          resource: guest-os/{target}
          fields:
            os_id: alpine
            marker.installed: sakutei-trial-1
            marker.hostname: node1
          timeout: 10m
      # 8. CD を抜く
      - apply:
          resource: redfish-virtual-media/{target}/cd
          fields:
            image: ""
```

読み方：

- `resources` の 1・2 つ目は**終わったあとの姿**。Workflow が走っている間（done になるまで）、対象の `node1` と手順が触る CD（`node1/cd`）は収束ループが Apply しない（Workflow に任せる、§6.3）。
  done になると収束に戻り、`node1` は最終形と同じなので何もしない（起動先の上書きは Once を BMC が使い切っているので、`boot_enabled: Disabled` の宣言はもう満たされている）。以後、手で電源を切られても次の周に On に戻す。
- 3 つ目の `template-file` は応答ファイル。Workflow の対象ではないので、収束ループが最初の周に書き出す（インストーラが取りに来るのは CD から起動したあと）。
- 手順 1・2 は別の provider（`redfish-virtual-media`）の CD を触る（`resource: <provider>/<id>`、`{target}` は今の対象のノード名、Backlog #87）。
  手順 2 の `require` は前提条件で、CD が差さったと観測できるまで手順 3 に進まない。BMC が ISO を取りに行けないなどで `timeout`（5m）を過ぎると halt。
- 手順 3・4 は**一度きりの操作**（`action`、#139）。`boot_once` は起動先の上書きを Once・Cd にする（再起動はしない。BMC が次の 1 回の起動で Disabled に戻すので、上書きは残らない）。
  `reset` は再起動する（既定 GracefulRestart。電源が切れていれば電源 On を送る）。どちらも収束の宣言にはできない（Once は 1 回で消え、再起動は「状態」でない、§29.1）ので、手順としてその位置で 1 回だけ施す。
  送りかけたまま止まった（デーモンが落ちた）ら二度送らずに halt する。`args: { type: ForceRestart }` は破壊的として予算（§13）に数える。
- 手順 5 の `wait` の `timeout`（15m）を過ぎると on_failure の halt。インストーラが失敗して電源を切らないとき（`autoinstall.start` は失敗すると切らずに止まる）もこれで止まる。
- 手順 6 は電源を入れるだけ（上書きはもう無いので、ディスクから起動する）。
- 手順 7 は入れた OS を ssh で観測する（`guest-os/{target}`、`providers.yaml` の `guest_hosts` の名前がノード名と同じ）。電源が切れただけでは先へ進まず、`os_id: alpine` と今回の目印と、
  応答ファイルのホスト名（目印 `hostname` は `/etc/hostname`）が見えるまで待つ。起動しない（ディスクに何も入っていない）・sshd が上がらない間は観測できない（unknown）まま待ち、
  目印が違う・無い間も待ち、どちらも `timeout`（10m）で halt（手順 8 の CD を抜くは施さない）。「まだ起動していない」と「壊れた」は観測からは見分けられないので、timeout は入れた OS が起動して sshd が答えるまでの時間より十分長くする。
- `boot_once` の `target: Cd` は BMC の `@Redfish.AllowableValues` にある値でなければならない（送る前に確かめ、無ければ halt）。

## BMC の注意（Workflow で OS を入れるときの一般的な事実）

BMC と機種によって振る舞いが違う。Workflow を書くときは次の 2 つを前提にしておく。

- **ゲストの中からの再起動で、また CD から起動して導入が繰り返されることがある。** 起動先の上書き（Once）が再起動のどこで使い切られるかは
  BMC によって違い、インストーラが終わりに OS の中から再起動すると、上書きが残っていたり CD が差さったままだったりで、もう一度 CD から
  起動して導入をやり直す BMC がある。導入の終わりは**再起動でなく電源 Off にし**（応答ファイルでインストーラに電源を切らせる。上の例の
  手順 5）、**Off を `wait` で待ってから `apply` で On にする**形を勧める（手順 6）。CD は On の前か、入れた OS を観測したあとに抜く（手順 8）。
- **導入中の GracefulRestart は無視されることがある。** GracefulRestart は OS に頼んで再起動させる操作なので、インストーラや起動途中の OS
  （ACPI の電源ボタンを扱わない）では BMC が受け付けても何も起きないことがある。そのため `reset`（既定 GracefulRestart）のあとの `wait` には
  必ず `timeout` を書き、**時間切れの扱いを決めておく**：halt して人が見る（既定、`on_failure: halt`）か、ForceRestart（`args: { type: ForceRestart }`、
  破壊的として予算に数える）や電源の操作（`apply: { power: "Off" }` → Off を待つ → `apply: { power: "On" }`）に切り替える手順を書く。電源の状態が
  分からないまま再起動を重ねない（一度きりの操作は二度送らない、#139）。

## 回し方

Workflow を進めるのは**常駐の sakutei だけ**（`plan`・`reconcile` は Workflow を進めない）。進み具合は `--workflow-store`（S2）に残るので、必ず指定する。同じ store を 2 つの常駐に渡しても、進めるのは store の排他のロック（`wf.json.advance.lock`）を取った 1 つだけ（#209）。

導入が済んだあとは、一回性の `reconcile` でも node1 と CD を宣言どおりに保てる（#209）。`--workflow-store wf.json` を渡せば読むだけに使い、done の Workflow の対象をふつうのリソースとして戻す。渡さなくても、intent の `done_when`（入れた OS の目印）が ssh で観測できれば済んだと見て戻す。目印が見えない・届かない間は「Workflow に任せる」として触らず、理由（`progress unknown (no --workflow-store, and done_when … is not observed on guest-os/node1)` など）を出す。halt した run の対象は、`sakutei workflow cancel` で片づけるまで触らない（理由は `status`・`plan` と level の Event `WorkflowTargetHeld`）。

Workflow を進めない常駐（監視だけの alert-only など）を並べるときは、同じ store を `--workflow-store wf.json --workflow-observe-only` で読むだけに渡す（#210）。
store を渡さない常駐・読むだけの常駐・alert-only の常駐は、Workflow を誤りにせず「別の処理が進める」として扱い、level の Event `WorkflowAdvancedElsewhere` を最初の周に
1 回だけ出し、`sakutei status --daemon-status` に 1 行（`注意: Workflow は別の処理が進める（§6.3）: …`）出す。進めるロックは取らないので、進める常駐の邪魔をしない。

`--audit-log` を渡すと、導入の run が監査の鎖に残る（#210、§26.3）：`workflow.run.start`（Workflow の名前・run の id・起こした理由）、手ごとの `workflow.step`
（`boot_once`・`reset` は op と args、`wait` は満たしたか時間切れかと、かかった時間）、`workflow.run.end`（done・halted・cancelled と理由）。手から出た
`resource.apply`（CD を差す・電源を入れる）には run の id と手の番号が付く。`sakutei audit verify --audit-log audit.jsonl` で鎖が切れていないことを確かめられる。

```sh
W=/tmp/sakutei-os
# ISO と鍵は上の「ISO を作る」で $W に作っておく
cp -r docs/trial/redfish-os-install/intent docs/trial/redfish-os-install/templates docs/trial/redfish-os-install/bmcs.yaml \
  docs/trial/redfish-os-install/hosts.yaml docs/trial/redfish-os-install/providers.yaml docs/trial/redfish-os-install/topology.clab.yml $W/
# 応答ファイルの宣言に Sakutei の公開鍵を書く（公開鍵だけ。秘密鍵は書けない）
sed -i "s|ssh-ed25519 REPLACE-WITH-node1-key.pub sakutei|$(cat $W/node1-key.pub)|" $W/intent/os.yaml
# 秘密は file SecretBackend にだけ置く（鍵のファイルは消してよい）
python3 -c 'import json,sys; print(json.dumps({"secrets": {"bmc/node1": {"gen": 1, "value": "password"},
  "ssh/node1": {"gen": 1, "value": open(sys.argv[1]).read()}}}))' $W/node1-key > $W/secrets.json
chmod 600 $W/secrets.json; rm $W/node1-key
echo "[172.31.85.11]:2222 $(cat $W/node1-hostkey.pub)" > $W/known_hosts
go build -o $W/sakutei ./cmd/sakutei
cd $W
sudo containerlab deploy -t topology.clab.yml
# 応答ファイルの置き場（answers/、template-file が書く）を lab のゲートウェイで配る（ANSWER_URL と同じ）
mkdir -p answers && python3 -m http.server --bind 172.31.85.1 --directory answers 8080 &
./sakutei --intent intent --redfish-config bmcs.yaml --provider-config providers.yaml --workflow-store wf.json --interval 5s
# 別の端末で: Workflow の状態
./sakutei workflow list --store wf.json
# 終わったら（必ず消す）
kill %1; sudo containerlab destroy -t topology.clab.yml --cleanup
```

インストーラの進み具合（シリアル）は、qemu-bmc のコンテナの中の TCP `localhost:9002`（qemu-bmc の `SERIAL_ADDR` の既定）に出る。画面なら Redfish と同じポートの noVNC
（`https://172.31.85.11/novnc/vnc.html`）で見られる。どちらも qemu-bmc の README を見る。

## 結果（2026-10-22）

この節と次の節は、CD を普段の宣言にして手順を 3 つ（CD から起動 → 電源オフを待つ → ディスクから起動）にしていた当時の intent で回した記録（手順の番号も当時のもの）。
手順 6（ゲスト OS の観測）を足した形は、この手順書でなく `make integration-guestos` で回した（結果は [docs/providers/guest-os.md](../../providers/guest-os.md) の「統合テスト」）。

KVM が使える開発用のコンテナ（`/dev/kvm` あり、docker 29.1.3・containerlab 0.81.1）で、qemu-bmc 0.9.0 を 1 台（`VM_DISK_SIZE` 2G・`VM_MEMORY` 512）、`--interval 5s` で回した。

- **インストールは約 20 秒**（シリアルの `SAKUTEI-AUTOINSTALL: start` 12:36:00Z → `done, powering off`）、**Workflow 全体は約 35 秒**（cycle 1 で開始 → cycle 7 で完了）。
- 完了のあと、シリアルに `SAKUTEI-INSTALLED-OS: booted from disk: installed by sakutei trial 2026-10-22T12:36:20Z` が出た——**ディスクから起動した**。
  以後は `converged (no changes)` が続いた（最終形のまま、CD は差したまま）。

デーモンの出力（抜粋）：

```
sakutei: cycle 1: WorkflowStarted: workflow os-install: run os-install-1792672556181948330 started (1 stage(s), 1 target(s))
sakutei: cycle 1: WorkflowStepDone: workflow os-install: stage install, target node1, step 1 (apply): applied map[boot_enabled:Continuous boot_target:Cd power:On]
sakutei: cycle 1: WorkflowWaiting: workflow os-install: stage install, target node1, step 2 (wait): waiting for map[power:Off]
sakutei: cycle 1: 1 Workflow target(s) driven by their Workflow (not converged by the loop): [node1]
  …（cycle 2〜6 は waiting for map[power:Off]）
sakutei: cycle 7: WorkflowStepDone: workflow os-install: stage install, target node1, step 2 (wait): observed map[power:Off]
sakutei: cycle 7: WorkflowStepDone: workflow os-install: stage install, target node1, step 3 (apply): applied map[boot_enabled:Disabled power:On]
sakutei: cycle 7: WorkflowCompleted: workflow os-install: run os-install-1792672556181948330 completed; targets return to convergence
sakutei: cycle 8: observed=2 converged (no changes)
```

CD（`node1/cd`）は cycle 1 に普段の収束で差され（初回の観測なので cause B の Drift として出る）、同じ周に Workflow が電源を入れた。試用では間に合ったが、順番は保証されなかった。#87 で CD を Workflow の手順に入れ、`require` で差さったことを確かめてから電源を入れるようにした（上の intent。順番は偽 BMC の受け入れテスト `TestWorkflowMultiProvider_CDBeforePower` と `make integration-redfish` の [AC-87-7] で確かめる）。

### 手順書どおりにやり直した結果（2026-10-23）

`make-iso.sh` で ISO を作り直し、上の「回し方」どおりに回した。インストールは約 20 秒（15:00:47Z → 15:01:07Z）、Workflow は cycle 1 で開始・cycle 7 で完了し、ディスクから起動した（`SAKUTEI-INSTALLED-OS: booted from disk`）。

続けて、電源が On（入れた OS が動いている）のまま `sakutei workflow cancel` すると、次の周に新しい run が始まり手順 1 を施した（起動先が Continuous・Cd になった）が、電源は入れ直されず、インストーラは走らなかった（シリアルの `AUTOINSTALL: start` は 1 回のまま）。run は手順 2 で `waiting for map[power:Off]` のまま待ち続けた。下の「限界」の「入れ直し」。

## 入れ直し（#88）

上の `os-install` は宣言されると 1 回走り、done のあとはもう走らない。入れ直しは、Workflow に `trigger: reimage` と `target.scope` を書き、**入れ直したいときに `planned_change` の `kind: reimage` を宣言する**（PR で承認を通す）。

```yaml
workflows:
  - workflow: reimage-node1
    trigger: reimage                 # planned_change kind: reimage が開いているときだけ、宣言ごとに 1 回
    applicant: trial
    target:
      provider: redfish-system
      scope:
        rack: r1
    stages:
      - name: install
        targets:
          - node1
    steps:                           # 上の os-install と同じ。ただし手順 4 と目印が違う
      - apply:
          resource: redfish-virtual-media/{target}/cd
          fields:
            image: /iso/sakutei-autoinstall.iso
      - require:
          resource: redfish-virtual-media/{target}/cd
          fields:
            image: /iso/sakutei-autoinstall.iso
          timeout: 5m
      - action:
          op: boot_once
          args:
            target: Cd
      # 4. 正常停止を待たずに再起動する（ディスクを消して入れ直すので。入れ直しの
      #    1 回分に含まれ、通常予算に数えない）
      - action:
          op: reset
          args:
            type: ForceRestart
      - wait:
          fields:
            power: "Off"
          timeout: 15m
      - apply:
          power: "On"
      # 7. 目印は入れ直すたびに変える（応答ファイルの install_mark も同じ値に）
      - require:
          resource: guest-os/{target}
          fields:
            os_id: alpine
            marker.installed: sakutei-trial-2
            marker.hostname: node1
          timeout: 10m
      - apply:
          resource: redfish-virtual-media/{target}/cd
          fields:
            image: ""
planned_change:
  scope:
    rack: r1
  kind: reimage
  max_count: 1                       # 入れ直す台の数
  window: "2026-10-28 00:00 .. 2026-10-28 06:00"
  applicant: carol
  approval:
    pr: 1500
    approvers:
      - alice                        # 運用プロファイル team（既定）は 1 人
```

- 宣言が無い・承認が足りない・窓の外の間は走らない。対象（`node1`）は収束ループが普段どおり最終形に保つ。
- 承認された宣言の窓の中で、対象の scope を覆う宣言があれば run が 1 回走る。対象ごとに手を付ける前に `max_count` を 1 つ使い、使い切った宣言では同じ scope のほかの Workflow は走らない。
- done のあと、同じ宣言ではもう走らない。宣言を外しても走らない。もう一度入れ直すには、別の窓の宣言を書く（目印の値も変える）。
- 予算の使用量は S4（`--audit-log`）に残る。`--audit-log` を付けて回す。
- qemu-bmc での確かめは `make integration-guestos` の TestReimage（[docs/providers/guest-os.md](../../providers/guest-os.md) の「統合テスト」）。

## 限界（v1 の今）

| 限界 | 何が起きるか | 先の話 |
|---|---|---|
| ~~完了は電源オフでしか判断できない~~（#86 で解消） | 2026-10-22 の試用では Workflow が見るのは BMC から見た `power: Off` だけで、インストーラが失敗しても電源を切れば成功と見なして先へ進んだ。今は手順 7 で入れた OS を ssh で観測し（`guest-os`）、目印が見えるまで進まない | 入れた OS に sshd と鍵を置けない（ssh で届かない）OS では使えない。agent を置く観測は持たない |
| ~~起動先の上書きを戻す手順を書く~~（#139 で解消） | 当時は Continuous の上書きしか書けず、導入のあとに Disabled に戻す手順が要った。今は `action: boot_once` で 1 回だけ CD から起動し、BMC が使い切る | Once を使い切る時を BMC がどう決めるかは BMC 次第（qemu-bmc は電源 On のとき。上の「なぜ電源を切るのか」） |
| ~~**入れ直しは手で cancel**~~（#88 で解消） | 当時は done の Workflow は二度と走らず、入れ直すには `sakutei workflow cancel --store wf.json os-install` で記録を消すしかなかった。今は Workflow に `trigger: reimage` を書き、`planned_change` の `kind: reimage` を宣言すると、承認されたその宣言ごとに 1 回走る（下の「入れ直し」） | 入れ直しは**ディスクを消す**。目印の値を変えておかないと、前の導入の OS を見て進んでしまう |
| ~~**workflow store を失うと、もう一度走る**~~（#209 で解消） | 当時は進み具合（done）が `--workflow-store` のファイルにしか無く、消える・別のファイルを指すと次の周に手順 1 から走って**ディスクを消して入れ直した**。今は store にその run が無くても、`done_when`（入れた OS の目印）が観測で満たされていれば、常駐は run を起こさず done と書く。`trigger: reimage` の Workflow は宣言が無ければ走らず、宣言の `max_count` の使用量は S4（`--audit-log`）に残るので、store を失っても同じ宣言で `max_count` を越えては入れ直さない | `done_when` を書かない Workflow、目印が見えない（ssh で届かない）ときは今までどおり手順 1 から走る。store は消えない場所に置く |
| **応答ファイルを届けるのは利用者** | `template-file` はファイルを書くだけ。HTTP で配る・ISO に入れるのは利用者の側（試用は `python3 -m http.server`）。Workflow は応答ファイルが書かれたことを確かめずに進む（収束ループが最初の周に書く。インストーラは取れるまで 2 分待つ） | |
