# Redfish と Workflow で OS を入れる（自動インストールの ISO は利用者が用意する）

Redfish の BMC provider（[docs/providers/redfish.md](../../providers/redfish.md)）と最小 Workflow（§6.3、`internal/workflow`）を組み合わせて、
「CD に ISO を差す → 差さったことを確かめる → CD から起動 → インストーラが電源を切るのを待つ → ディスクから起動 → 入れた OS が目印つきで起動したのを確かめる」を 1 台に回す手順。2026-10-22 に qemu-bmc 0.9.0 で試した。
最後の確かめ（ゲスト OS の観測、[guest-os provider](../../providers/guest-os.md)）は Backlog #86 で足した。同じ形を `make integration-guestos`（qemu-bmc 0.10.0 を 3 台、成功・失敗する ISO）で確かめている。

## 線引き：何を Sakutei がして、何を利用者が用意するか

インストーラそのもの（自動インストールの ISO・PXE・kickstart / autoinstall / preseed の設定）は **Sakutei の責務の外**（設計 §29.3 (c)：provisioner 実体は持たない）。
Sakutei は BMC の「あるべき姿」と、その順番だけを扱う。

| Sakutei がする | 利用者が用意する |
|---|---|
| CD に ISO を差しておく（`redfish-virtual-media`） | **人の手を介さずに入れて、終わったら電源を切る ISO**（BMC から見えるパスか URL に置く） |
| CD 起動にして電源を入れ、電源が切れるのを待ち、起動先の上書きを外して電源を入れる（Workflow + `redfish-system`） | BMC の一覧（`--redfish-config`）とパスワードの file SecretBackend |
| 入れた OS が目印つきで起動したかを ssh で観測し、確かめるまで先へ進まない（`guest-os`） | 入れた OS に sshd・Sakutei の鍵・**導入の目印**（ファイル）を置くこと（ISO の仕事） |
| 終わったあとの姿（電源オン・上書きなし）を毎周保つ | |

ISO が満たすべき約束は 2 つ：**入れ終わったら電源を切る**（再起動しない）と、**入れた OS に目印を置き、ssh で読めるようにする**。Workflow は電源オフを「インストーラが終わった」の合図にし、目印の観測を「導入が済んだ」の合図にする（インストーラが失敗しても電源を切る ISO を、電源オフだけで成功と見なさない）。

## 置いてあるもの（docs/trial/redfish-os-install/）

| ファイル | 中身 |
|---|---|
| `intent/os.yaml` | 最終形の宣言と Workflow `os-install`（下の「intent」）。loader で読めることを `internal/loader` のテスト `TestRedfishOSInstallTrialIntentLoads` が確かめる |
| `topology.clab.yml` | qemu-bmc 0.9.0 を 1 台（`VM_DISK_SIZE` 2G・`VM_MEMORY` 512）。BMC は `172.31.85.11`、入れた OS の ssh は QEMU の user ネットの hostfwd で `172.31.85.11:2222` |
| `bmcs.yaml` | BMC の一覧（`insecure_skip_verify: true`、パスワードは SecretRef `file/bmc/node1`） |
| `hosts.yaml` / `providers.yaml` | 入れた OS を観測する `guest-os` の対象の一覧（鍵は SecretRef `file/ssh/node1`、known_hosts）と `--provider-config` |
| `make-iso.sh` / `autoinstall.start` | Alpine 3.24 の virt ISO を自動インストールの ISO に作り直すスクリプト（試用で使ったもの。下の「ISO を作る」） |

**`make-iso.sh` と `autoinstall.start` は試用のための補助で、Sakutei の一部ではない。** リポジトリの「Go の標準ライブラリだけ（外部依存なし）」の決まりは Go のコードの話で、
この docs の下のシェルはその外にある（curl・xorriso・docker を使い、ネットワークから ISO とパッケージを取る）。`make check`・`make test`・CI からは呼ばない。

## ISO を作る（試用の例：Alpine 3.24）

実機や別のディストリビューションでは、それぞれの自動インストールの仕組み（kickstart・autoinstall・preseed など）で「入れて電源を切る」ISO を用意すればよい。
試用では Alpine 3.24.3 の virt ISO（`alpine-virt-3.24.3-x86_64.iso`）を次のように作り直した。

1. **apkovl**（`localhost.apkovl.tar.gz` を ISO のルートに置くと、live 環境の `/etc` に重なる）に `/etc/local.d/autoinstall.start` を入れ、
   `default` の runlevel で `local` を動かす。スクリプトは `setup-alpine -e -f <回答>` のあと `ERASE_DISKS=/dev/vda setup-disk -m sys -s 0 /dev/vda` で入れ、
   入れた OS に目印（`/etc/sakutei-installed`、中身は `INSTALL_MARK`）と起動時にシリアルへ出す `hello.start` を置いて `poweroff` する。失敗したときは電源を切らずに止まる。
   `SSH_AUTHORIZED_KEYS` を渡すと、入れた OS に openssh を入れて sshd を動かし（eth0 は DHCP）、root にその鍵で入れるようにする。`SSH_HOST_KEY` を渡すとそれをホスト鍵にする（known_hosts を前もって書ける）。
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
ssh-keygen -q -t ed25519 -N '' -f $W/node1-key        # Sakutei が入る鍵
ssh-keygen -q -t ed25519 -N '' -f $W/node1-hostkey    # 入れる OS のホスト鍵
INSTALL_MARK=sakutei-trial-1 SSH_AUTHORIZED_KEYS=$W/node1-key.pub SSH_HOST_KEY=$W/node1-hostkey \
  docs/trial/redfish-os-install/make-iso.sh $W/iso/sakutei-autoinstall.iso
# 手元の ISO を使うなら ALPINE_ISO=/path/to/alpine-virt-3.24.3-x86_64.iso、取ったものを残すなら CACHE_DIR=<dir> を足す
```

`INSTALL_MARK` は intent の手順 6 の `marker.installed` と同じ値にする。**導入ごとに変える**（入れ直すときは ISO を作り直して intent の値も変える）と、前の導入で入った OS を取り違えない。
`AUTOINSTALL_FAIL=disk`（何も入れずに電源を切る）・`AUTOINSTALL_FAIL=marker`（入れるが目印を置かない）は失敗の試し用（`make integration-guestos` が使う）。

BMC を通さずに ISO だけを確かめるなら、qemu で直に回せる（2026-10-23、インストールして電源が切れるまで約 40 秒、そのあとディスクから起動して目印が出た）。

```sh
qemu-img create -f qcow2 d.qcow2 2G
qemu-system-x86_64 -accel kvm -m 512 -drive file=d.qcow2,if=virtio -cdrom sakutei-autoinstall.iso -boot d \
  -no-reboot -display none -monitor none -serial file:inst.log     # SAKUTEI-AUTOINSTALL: done, powering off で終わる
qemu-system-x86_64 -accel kvm -m 512 -drive file=d.qcow2,if=virtio -boot c \
  -display none -monitor none -serial file:boot.log                # SAKUTEI-INSTALLED-OS: booted from disk が出る（Ctrl-C で止める）
```

## intent

`intent/os.yaml`（YAML はブロックスタイルで書く。`{ … }` / `[ … ]` のフロースタイルは loader が読まない）。

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

workflows:
  - workflow: os-install
    applicant: trial
    target:
      provider: redfish-system
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
      # 3. CD から起動する（インストーラが走る）
      - apply:
          boot_enabled: Continuous
          boot_target: Cd
          power: "On"
      # 4. インストーラが終わって電源を切るのを待つ
      - wait:
          fields:
            power: "Off"
          timeout: 15m
      # 5. ディスクから起動する
      - apply:
          boot_enabled: Disabled
          power: "On"
      # 6. 入れた OS が目印つきで起動したのを待つ（#86。電源が切れただけでは成功と見なさない）。
      #    guest-os provider が ssh で /etc/os-release と目印（/etc/sakutei-installed）を読む。
      #    目印の値は make-iso.sh の INSTALL_MARK（導入ごとに変える）。届かない・目印が違うまま
      #    10 分過ぎたら halt（インストーラの失敗）
      - wait:
          resource: guest-os/{target}
          fields:
            os_id: alpine
            marker.installed: sakutei-trial-1
          timeout: 10m
```

読み方：

- `resources` は**終わったあとの姿**。Workflow が走っている間（done になるまで）、対象の `node1` と手順が触る CD（`node1/cd`）は収束ループが Apply しない（Workflow に任せる、§6.3）。
  done になると収束に戻り、`node1` は最終形と同じなので何もせず、CD は宣言どおり抜く。以後、手で電源を切られても次の周に On に戻す。
- 手順 1・2 は別の provider（`redfish-virtual-media`）の CD を触る（`resource: <provider>/<id>`、`{target}` は今の対象のノード名、Backlog #87）。
  手順 2 の `require` は前提条件で、CD が差さったと観測できるまで手順 3（電源オン）に進まない。BMC が ISO を取りに行けないなどで `timeout`（5m）を過ぎると halt。
- 手順 3 は電源が **Off から**始まる前提（qemu-bmc は `POWER_ON_AT_START` の既定で Off）。電源が入ったままの台に回すと、起動先を変えるだけで再起動はしない
  （再起動は宣言できない、§29.1）ので、手順 4 の待ちが `timeout` で切れて halt する。
- `wait` の `timeout`（15m）を過ぎると on_failure の halt。インストーラが失敗して電源を切らないとき（`autoinstall.start` は失敗すると切らずに止まる）もこれで止まる。
- 手順 6 は入れた OS を ssh で観測する（`guest-os/{target}`、`providers.yaml` の `guest_hosts` の名前がノード名と同じ）。電源が切れただけでは先へ進まず、`os_id: alpine` と今回の目印が見えるまで待つ。
  起動しない（ディスクに何も入っていない）・sshd が上がらない間は観測できない（unknown）まま待ち、目印が違う・無い間も待ち、どちらも `timeout`（10m）で halt。
  「まだ起動していない」と「壊れた」は観測からは見分けられないので、timeout は入れた OS が起動して sshd が答えるまでの時間より十分長くする。
- `boot_target: Cd` は BMC の `@Redfish.AllowableValues` にある値でなければならない（計画の時点で確かめる）。

## 回し方

Workflow を進めるのは**常駐の sakutei だけ**（`plan`・`reconcile` は Workflow の対象を Apply せず「Workflow に任せている」と出す）。進み具合は `--workflow-store`（S2）に残るので、必ず指定する。

```sh
W=/tmp/sakutei-os; mkdir -p $W/iso
docs/trial/redfish-os-install/make-iso.sh $W/iso/sakutei-autoinstall.iso
# ISO と鍵は上の「ISO を作る」で $W に作っておく
cp -r docs/trial/redfish-os-install/intent docs/trial/redfish-os-install/bmcs.yaml docs/trial/redfish-os-install/hosts.yaml \
  docs/trial/redfish-os-install/providers.yaml docs/trial/redfish-os-install/topology.clab.yml $W/
# 秘密は file SecretBackend にだけ置く（鍵のファイルは消してよい）
python3 -c 'import json,sys; print(json.dumps({"secrets": {"bmc/node1": {"gen": 1, "value": "password"},
  "ssh/node1": {"gen": 1, "value": open(sys.argv[1]).read()}}}))' $W/node1-key > $W/secrets.json
chmod 600 $W/secrets.json; rm $W/node1-key
echo "[172.31.85.11]:2222 $(cat $W/node1-hostkey.pub)" > $W/known_hosts
go build -o $W/sakutei ./cmd/sakutei
cd $W
sudo containerlab deploy -t topology.clab.yml
./sakutei --intent intent --redfish-config bmcs.yaml --provider-config providers.yaml --workflow-store wf.json --interval 5s
# 別の端末で: Workflow の状態
./sakutei workflow list --store wf.json
# 終わったら（必ず消す）
sudo containerlab destroy -t topology.clab.yml --cleanup
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

## 限界（v1 の今）

| 限界 | 何が起きるか | 先の話 |
|---|---|---|
| ~~完了は電源オフでしか判断できない~~（#86 で解消） | 2026-10-22 の試用では Workflow が見るのは BMC から見た `power: Off` だけで、インストーラが失敗しても電源を切れば成功と見なして先へ進んだ。今は手順 6 で入れた OS を ssh で観測し（`guest-os`）、目印が見えるまで進まない | 入れた OS に sshd と鍵を置けない（ssh で届かない）OS では使えない。agent を置く観測は持たない |
| **入れ直しは手で cancel** | done の Workflow は二度と走らない（定義を変えれば新しい run になる）。入れ直すには `sakutei workflow cancel --store wf.json os-install` で記録を消す。次の周に新しい run が始まる（2026-10-23 に確かめた）。**ただし電源が On のまま cancel すると入れ直しにならない**：手順 3 の apply は起動先を CD に書き換えるだけで電源を入れ直さず（再起動は宣言できない）、インストーラは走らずに手順 4 で待ち続ける（`timeout` で halt）。先に台を止めて（電源 Off を確かめて）から cancel する | planned_change の `kind: reimage` で入れ直しを宣言する：Backlog #88（v2） |
| **workflow store を失うと、もう一度走る** | 進み具合（done）は `--workflow-store` のファイルにしか無い。消える・別のファイルを指すと、次の周に手順 1 から走る（電源が入っていれば手順 4 で halt、切れていれば**ディスクを消して入れ直す**）。消えない場所に置く | #88 で入れ直しを宣言に寄せるときに合わせて考える |
