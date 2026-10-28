# Backlog #155 の統合テストのホスト（make integration-hostconf）。systemd を PID 1 で
# 動かし、sshd・sudo・podman（quadlet。Debian trixie の podman 5）を入れる。
# ベースは tag と digest で固定する。コンテナの中では setgid の unix_chkpwd が動かない
# ことがある（PAM のアカウントの検査が落ちる）ので、sshd は UsePAM no、sudo は
# pam_acct_mgmt を切る。
FROM debian:trixie-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends systemd systemd-sysv dbus openssh-server sudo podman \
 && rm -rf /var/lib/apt/lists/* \
 && useradd -m -s /bin/bash sakutei && usermod -p '*' sakutei \
 && install -d -m 700 -o sakutei -g sakutei /home/sakutei/.ssh \
 && printf 'Defaults !pam_acct_mgmt, !pam_session\nsakutei ALL=(ALL) NOPASSWD: ALL\n' > /etc/sudoers.d/sakutei && chmod 0440 /etc/sudoers.d/sakutei \
 && echo 'UsePAM no' > /etc/ssh/sshd_config.d/sakutei-it.conf \
 && systemctl enable ssh.service
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
