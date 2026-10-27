#!/bin/sh
# Alpine の virt ISO を、/dev/vda に自動で入れて電源を切る ISO に作り直す
# （docs/trial/redfish-os-install/README.md の「ISO を作る」）。
# 使い方: docs/trial/redfish-os-install/make-iso.sh [出力する ISO]   （既定 ./sakutei-autoinstall.iso）
#
# これは試用のための手順で、Sakutei の一部ではない（インストーラは Sakutei の責務の外、§29.3 (c)）。
# リポジトリの「Go の標準ライブラリだけ」の決まりの外にあり、make check・CI からは呼ばない。
# 要るもの: curl・xorriso・GNU tar・docker（alpine:3.24 のイメージで apk fetch する。
# sudo が要るなら DOCKER="sudo docker"）。ネットワークから ISO とパッケージを取る。
#
# 環境変数（どれも省略できる）:
#   ALPINE_ISO=<file>           手元の元の ISO を使う（取りに行かない）
#   CACHE_DIR=<dir>             取った ISO と /extra のパッケージをここに残し、次から使い回す
#   INSTALL_MARK=<値>           入れた OS の目印（/etc/sakutei-installed の中身。既定 sakutei-trial）。
#                               英数字と . _ : - だけ。導入ごとに変えると、前の導入の OS と見分けられる
#   SSH_AUTHORIZED_KEYS=<file>  入れた OS で sshd を動かし（eth0 は DHCP）、root にこの鍵で入れる
#                               （guest-os provider で観測するため）
#   SSH_HOST_KEY=<file>         入れた OS のホスト鍵（ed25519 の秘密鍵。<file>.pub も要る）。
#                               known_hosts を前もって書ける（無ければ sshd が初回に作る）
#   AUTOINSTALL_FAIL=disk|marker  試験用にわざと失敗する ISO にする（autoinstall.start を見る）
#   ANSWER_URL=<URL>            インストーラが応答ファイル（template-file が宣言から書き出したもの、
#                               #139）を取る URL。{uuid} は機械の DMI の UUID に置き換わる。渡すと
#                               ホスト名・アドレス・公開鍵・目印は応答ファイルの値になり、1 つの ISO
#                               を何台にも使える（例 http://172.31.85.1:8080/{uuid}）
set -eu

VER=${ALPINE_VERSION:-3.24.3}
BRANCH=v${VER%.*}
ARCH=x86_64
MIRROR=${ALPINE_MIRROR:-https://dl-cdn.alpinelinux.org/alpine}
DOCKER=${DOCKER:-docker}
HERE=$(cd "$(dirname "$0")" && pwd)
OUT=$(realpath -m "${1:-sakutei-autoinstall.iso}")
MARK=${INSTALL_MARK:-sakutei-trial}
FAIL=${AUTOINSTALL_FAIL:-}
case "$MARK" in *[!A-Za-z0-9._:-]*|'') echo "INSTALL_MARK: only letters, digits and . _ : - ($MARK)" >&2; exit 1;; esac
case "$FAIL" in ''|disk|marker) ;; *) echo "AUTOINSTALL_FAIL: want disk or marker ($FAIL)" >&2; exit 1;; esac
ANSWER=${ANSWER_URL:-}
case "$ANSWER" in *[!A-Za-z0-9._:/{}-]*) echo "ANSWER_URL: only letters, digits and . _ : / { } - ($ANSWER)" >&2; exit 1;; esac
W=$(mktemp -d)
trap 'rm -rf "$W"' EXIT
CACHE=${CACHE_DIR:-$W/cache}
mkdir -p "$CACHE"

# 1. 元の ISO（ALPINE_ISO で手元のファイルを渡せる）
ISO=${ALPINE_ISO:-}
if [ -z "$ISO" ]; then
  ISO=$CACHE/alpine-virt-$VER-$ARCH.iso
  if [ ! -s "$ISO" ]; then
    curl -fsSL -o "$ISO.part" "$MIRROR/$BRANCH/releases/$ARCH/alpine-virt-$VER-$ARCH.iso"
    mv "$ISO.part" "$ISO"
  fi
fi

# 2. ISO の中のリポジトリ /extra。virt の ISO にはカーネルのパッケージが無く、setup-disk -m sys が
#    入れる linux-virt・syslinux・mkinitfs などを足す（openssh は SSH_AUTHORIZED_KEYS のときに入れる）。
#    索引は公式のもの（署名つき）をそのまま置く。
PKGS="alpine-base linux-virt syslinux mkinitfs e2fsprogs sfdisk lsblk blkid util-linux-misc mtools openssh"
EXTRA=$CACHE/extra-$VER-$(printf '%s' "$PKGS" | cksum | cut -d' ' -f1)
if [ ! -s "$EXTRA/$ARCH/APKINDEX.tar.gz" ]; then
  rm -rf "$EXTRA.part"
  mkdir -p "$EXTRA.part/$ARCH"
  $DOCKER run --rm -v "$EXTRA.part/$ARCH:/out" "alpine:${BRANCH#v}" sh -c \
    "apk update -q && apk fetch -q -R -o /out $PKGS && chmod -R a+rwX /out"
  curl -fsSL -o "$EXTRA.part/$ARCH/APKINDEX.tar.gz" "$MIRROR/$BRANCH/main/$ARCH/APKINDEX.tar.gz"
  rm -rf "$EXTRA"
  mv "$EXTRA.part" "$EXTRA"
fi

# 3. apkovl（live 環境の /etc に重ねる）。apkovl を使うと既定の runlevel が入らないので、
#    sysinit に modloop を入れる（入れないとカーネルのモジュールが使えず、インストールが通らない）。
O=$W/ovl/etc
mkdir -p "$O/local.d" "$O/apk" "$O/runlevels/sysinit" "$O/runlevels/boot" "$O/runlevels/default" "$O/runlevels/shutdown"
cp "$HERE/autoinstall.start" "$O/local.d/autoinstall.start"
chmod 755 "$O/local.d/autoinstall.start"
echo sakutei-installer > "$O/hostname"
echo alpine-base > "$O/apk/world"
echo /media/cdrom/apks > "$O/apk/repositories"
printf 'MARK=%s\nFAIL=%s\nANSWER_URL=%s\n' "$MARK" "$FAIL" "$ANSWER" > "$O/sakutei-autoinstall.conf"
# 公開鍵は ISO に焼くか（SSH_AUTHORIZED_KEYS）、応答ファイルから取る（ANSWER_URL）。ホスト鍵は秘密
# なので応答ファイルには書かず、ここで ISO に入れる
if [ -n "${SSH_AUTHORIZED_KEYS:-}" ] || [ -n "${SSH_HOST_KEY:-}" ]; then
  mkdir -p "$O/sakutei-ssh"
  [ -n "${SSH_AUTHORIZED_KEYS:-}" ] && cp "$SSH_AUTHORIZED_KEYS" "$O/sakutei-ssh/authorized_keys"
  if [ -n "${SSH_HOST_KEY:-}" ]; then
    cp "$SSH_HOST_KEY" "$O/sakutei-ssh/ssh_host_ed25519_key"
    cp "$SSH_HOST_KEY.pub" "$O/sakutei-ssh/ssh_host_ed25519_key.pub"
  fi
  chmod 700 "$O/sakutei-ssh"
  chmod 600 "$O/sakutei-ssh/"*
fi
for s in devfs dmesg mdev hwdrivers modloop; do ln -s "/etc/init.d/$s" "$O/runlevels/sysinit/$s"; done
for s in modules hwclock hostname sysctl bootmisc syslog; do ln -s "/etc/init.d/$s" "$O/runlevels/boot/$s"; done
ln -s /etc/init.d/local "$O/runlevels/default/local"
for s in mount-ro killprocs savecache; do ln -s "/etc/init.d/$s" "$O/runlevels/shutdown/$s"; done
tar -C "$W/ovl" --owner=0 --group=0 -czf "$W/localhost.apkovl.tar.gz" etc

# 4. BIOS の起動メニュー（syslinux）をシリアルコンソールに出す（qemu-bmc のシリアルで進み具合を見る）
xorriso -osirrox on -indev "$ISO" -extract /boot/syslinux/syslinux.cfg "$W/syslinux.cfg" 2>/dev/null
chmod u+w "$W/syslinux.cfg"
grep -q '^SERIAL' "$W/syslinux.cfg" || sed -i '1i SERIAL 0 115200' "$W/syslinux.cfg"
grep -q 'console=ttyS0' "$W/syslinux.cfg" || sed -i 's|^\(APPEND .*\)$|\1 console=ttyS0,115200|' "$W/syslinux.cfg"

# 5. 起動の設定（El Torito・isohybrid）は元の ISO のまま、ファイルだけ足して書き出す
rm -f "$OUT"
xorriso -indev "$ISO" -outdev "$OUT" -boot_image any replay \
  -map "$EXTRA" /extra \
  -map "$W/localhost.apkovl.tar.gz" /localhost.apkovl.tar.gz \
  -map "$W/syslinux.cfg" /boot/syslinux/syslinux.cfg 2>/dev/null
echo "wrote $OUT"
