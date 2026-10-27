# Backlog #130 の統合テストのホスト（make integration-remote）。sshd・nft・sudo。
# ベースは tag と digest で固定する。
FROM debian:bookworm-slim@sha256:7ff6ba31527b9e336162fa061746c366c4b2032f4d2cff2078bea43f78efe3f8
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends openssh-server nftables sudo procps \
 && rm -rf /var/lib/apt/lists/* \
 && useradd -m -s /bin/bash sakutei && usermod -p '*' sakutei \
 && install -d -m 700 -o sakutei -g sakutei /home/sakutei/.ssh \
 && install -d -m 755 /run/sshd /var/lib/sakutei-it \
 && echo 'sakutei ALL=(ALL) NOPASSWD: ALL' > /etc/sudoers.d/sakutei && chmod 0440 /etc/sudoers.d/sakutei \
 && printf '#!/bin/sh\necho "$@" >> /var/log/sakutei-it-reload.log\n' > /usr/local/bin/sakutei-it-reload \
 && chmod 755 /usr/local/bin/sakutei-it-reload \
 && rm -f /etc/ssh/ssh_host_*
CMD ["sleep", "infinity"]
