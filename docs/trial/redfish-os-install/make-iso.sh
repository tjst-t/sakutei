#!/bin/sh
# Alpine の virt ISO を、/dev/vda に自動で入れて電源を切る ISO に作り直す
# （docs/trial/redfish-os-install/README.md の「ISO を作る」）。
# 使い方: docs/trial/redfish-os-install/make-iso.sh [出力する ISO]   （既定 ./sakutei-autoinstall.iso）
#
# これは試用のための手順で、Sakutei の一部ではない（インストーラは Sakutei の責務の外、§29.3 (c)）。
# リポジトリの「Go の標準ライブラリだけ」の決まりの外にあり、make check・CI からは呼ばない。
# 要るもの: curl・xorriso・GNU tar・docker（alpine:3.24 のイメージで apk fetch する。
# sudo が要るなら DOCKER="sudo docker"）。ネットワークから ISO とパッケージを取る。
set -eu

VER=${ALPINE_VERSION:-3.24.3}
BRANCH=v${VER%.*}
ARCH=x86_64
MIRROR=${ALPINE_MIRROR:-https://dl-cdn.alpinelinux.org/alpine}
DOCKER=${DOCKER:-docker}
HERE=$(cd "$(dirname "$0")" && pwd)
OUT=$(realpath -m "${1:-sakutei-autoinstall.iso}")
W=$(mktemp -d)
trap 'rm -rf "$W"' EXIT

# 1. 元の ISO（ALPINE_ISO で手元のファイルを渡せる）
ISO=${ALPINE_ISO:-}
if [ -z "$ISO" ]; then
  ISO=$W/alpine-virt-$VER-$ARCH.iso
  curl -fsSL -o "$ISO" "$MIRROR/$BRANCH/releases/$ARCH/alpine-virt-$VER-$ARCH.iso"
fi

# 2. ISO の中のリポジトリ /extra。virt の ISO にはカーネルのパッケージが無く、setup-disk -m sys が
#    入れる linux-virt・syslinux・mkinitfs などを足す。索引は公式のもの（署名つき）をそのまま置く。
mkdir -p "$W/extra/$ARCH"
$DOCKER run --rm -v "$W/extra/$ARCH:/out" "alpine:${BRANCH#v}" sh -c \
  "apk update -q && apk fetch -q -R -o /out alpine-base linux-virt syslinux mkinitfs e2fsprogs sfdisk lsblk blkid util-linux-misc mtools && chmod -R a+rX /out"
curl -fsSL -o "$W/extra/$ARCH/APKINDEX.tar.gz" "$MIRROR/$BRANCH/main/$ARCH/APKINDEX.tar.gz"

# 3. apkovl（live 環境の /etc に重ねる）。apkovl を使うと既定の runlevel が入らないので、
#    sysinit に modloop を入れる（入れないとカーネルのモジュールが使えず、インストールが通らない）。
O=$W/ovl/etc
mkdir -p "$O/local.d" "$O/apk" "$O/runlevels/sysinit" "$O/runlevels/boot" "$O/runlevels/default" "$O/runlevels/shutdown"
cp "$HERE/autoinstall.start" "$O/local.d/autoinstall.start"
chmod 755 "$O/local.d/autoinstall.start"
echo sakutei-installer > "$O/hostname"
echo alpine-base > "$O/apk/world"
echo /media/cdrom/apks > "$O/apk/repositories"
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
  -map "$W/extra" /extra \
  -map "$W/localhost.apkovl.tar.gz" /localhost.apkovl.tar.gz \
  -map "$W/syslinux.cfg" /boot/syslinux/syslinux.cfg 2>/dev/null
echo "wrote $OUT"
