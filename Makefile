.PHONY: build test test-race vet check check-module integration-v3-nftables integration-v3-redfish integration-v3-netconf integration-v3-postgresql integration-v3-ldap integration-v3-proxmox integration-v3-s3 integration-v3-ansible

# Sakutei は 2 つの Go モジュールからなる：コア（v3/、設計は docs/core/README.md）と、
# コアの外の provider・Expand・secret backend・sink・道具（v3/providers/）。
# build・vet・test・test-race はどちらも回す。
V3 := cd v3 &&
V3P := cd v3/providers &&

# 同時に回すパッケージの数（go test -p）。既定（CPU の数）だと CPU の多いホストで
# テストのバイナリとそのスレッドが一度に千を超え、ほかのジョブと task の上限
# （cgroup の pids.max）を分け合う環境では fork/exec が EAGAIN で落ちて
# 「[build failed]」になる。
TEST_P ?= 8

build:
	$(V3) go build -p $(TEST_P) ./...
	$(V3P) go build -p $(TEST_P) ./...

test:
	$(V3) go test -p $(TEST_P) ./...
	$(V3P) go test -p $(TEST_P) ./...

# race detector 付きのテスト。cgo が要るので gcc（C コンパイラ）が必要。
# -count=1 で結果のキャッシュを使わず、毎回実際に回す。
test-race:
	$(V3) CGO_ENABLED=1 go test -race -p $(TEST_P) -count=1 ./...
	$(V3P) CGO_ENABLED=1 go test -race -p $(TEST_P) -count=1 ./...

vet:
	$(V3) go vet -p $(TEST_P) ./...
	$(V3P) go vet -p $(TEST_P) ./...

check: build vet test

# 外のモジュールから 2 つのモジュール（github.com/tjst-t/sakutei/v3 と
# github.com/tjst-t/sakutei/v3/providers）を go get で取り込めるかを確かめる（ネットワークが
# 要る。check・test には入れない。CI とリリースの前の点検で回す）。VERSION はタグか commit。
check-module:
	@test -n "$(VERSION)" || { echo 'usage: make check-module VERSION=<tag-or-commit>'; exit 2; }
	./scripts/check-module-consumable.sh $(VERSION)

# ansible-core を入れる venv（integration-v3-ansible）。版を固定し、pip も wheel の sha256 で固定する。
ANSIBLE_IT_VENV  ?= $(HOME)/.cache/sakutei/ansible-it-core-2.21.5
PIP_WHEEL        := pip-26.2.2-py3-none-any.whl
PIP_WHEEL_URL    := https://files.pythonhosted.org/packages/3f/06/3e4420302363fbb71be3c7f35ce042f54bfca330e3e66b3a2e3c4cca579f/$(PIP_WHEEL)
PIP_WHEEL_SHA256 := fd7f4dfa8190e014013b9f08f3e973f4e4c26ffd179a204895533a7254813a70

# 新しいコアの nftables provider の統合テスト（v3 段 6、不変条件 17）。本物の nft を
# 使い捨ての network namespace（ip netns）の中で回し、2 段の Apply が確かめられれば
# 確定し、確かめられなければ確定せず戻ること、Prepare のあと放置するとホストの側の
# 時間切れで戻ることを確かめる（ホストの FW には触れない。namespace と道具の置き場は
# テストが消す）。root で走るのは決まった道具 sakutei-nft-guard だけ。sudo（パスワード
# なし）・ip・nft・python3 が要るので、check・test・CI には入れない。手順と結果は
# v3/providers/README.md。
integration-v3-nftables:
	cd v3/providers && SAKUTEI_IT_NFT=1 go test -count=1 -v -run Kernel -timeout 5m ./nftables/

# 新しいコアの redfish provider の統合テスト（v3 段 8b）。qemu-bmc 0.11.0 を
# containerlab で 2 台立て（lab sakutei-v3rf・管理ネット 172.31.93.0/24。ほかの
# integration-v3-* と重ならないので同時に回せる）、v3 の sakutei の
# CLI と常駐で電源・起動先・仮想メディア・BIOS・FW の更新（手順の verify・stage・
# reset）を回し、終わったら（失敗しても）lab を消す。KVM・docker・containerlab と
# sudo（パスワードなし）が要るので、check・test・CI には入れない。手順と結果は
# v3/providers/README.md。
V3_REDFISH_IT := ./redfish/integration/
integration-v3-redfish:
	$(V3P) go vet -tags integration_v3_redfish $(V3_REDFISH_IT)
	@status=0; \
	(cd v3/providers && go test -tags integration_v3_redfish -count=1 -v -timeout 30m $(V3_REDFISH_IT)) || status=$$?; \
	if sudo -n docker ps -a --format '{{.Names}}' 2>/dev/null | grep -q '^clab-sakutei-v3rf-'; then \
	  echo 'integration-v3-redfish: the lab was left behind; destroying it'; \
	  sudo -n docker unpause clab-sakutei-v3rf-node1 clab-sakutei-v3rf-node2 >/dev/null 2>&1; \
	  sudo -n containerlab destroy --name sakutei-v3rf --cleanup || status=1; \
	fi; \
	exit $$status

# 新しいコアの netconf provider の統合テスト（v3 段 8c、不変条件 17）。公開の NETCONF
# サーバの netopeer2 のイメージ（tag と digest で固定）を docker で
# 4 台（sakutei-v3nc-sw1・g1〜g3。ほかの integration-v3-* と重ならないので同時に回せる）
# 127.0.0.1 の空いたポートに立て、v3 の CLI（cli.Run）で観測だけ・commit confirmed の
# 2 段の Apply（1.0・1.1 の persist）・NACM で絞ったユーザー・確かめが通らなければ確定
# せず戻る・Prepare のあと確定しなければ機器の時間切れで戻る・commit_group と confirm の
# check(…)・present(…)・止めると Unknown を確かめ、終わったら（失敗しても）コンテナを
# 消す。docker・ssh・ssh-keygen と sudo（パスワードなし）が要るので、check・test・CI
# には入れない。手順と結果は v3/providers/README.md。
.PHONY: integration-v3-netconf
V3_NETCONF_IT := ./netconf/integration/
integration-v3-netconf:
	$(V3P) go vet -tags integration_v3_netconf $(V3_NETCONF_IT)
	@status=0; \
	(cd v3/providers && go test -tags integration_v3_netconf -count=1 -v -timeout 15m $(V3_NETCONF_IT)) || status=$$?; \
	left=$$(sudo -n docker ps -a --format '{{.Names}}' 2>/dev/null | grep '^sakutei-v3nc-'); \
	if [ -n "$$left" ]; then \
	  echo "integration-v3-netconf: containers were left behind; removing: $$left"; \
	  sudo -n docker rm -f -v $$left >/dev/null || status=1; \
	fi; \
	exit $$status

# 新しいコアの postgresql と sqlmigrate の provider の統合テスト（v3 段 8e、E-F1・E-F2・
# E-F3・N-F1・N-F2・N-F7）。公式の postgres イメージ
# （tag と digest で固定）でサーバと psql を動かすだけのクライアントを docker のネット
# ワークに立て（postgresql は sakutei-v3pg-*、sqlmigrate は sakutei-v3sm-*。ほかの
# integration-v3-* と重ならないので同時に回せる）、v3 の CLI（cli.Run）で、postgresql は
# 空から作る・手の変更を戻す・PUBLIC と BYPASSRLS・秘密の世代と拡張の版・何周しても
# ずれない・利用者の表がある DB の削除を断る・止めると Unknown・観測用と書き込み用の
# ロール・verify_passwords と pg_authid・connect_roles を、sqlmigrate は手順（on_drift →
# migrate → done_when）・down・失敗で止まる・rollback_on_fail・advisory lock で二重に
# 流さない・書き込み用の秘密が無い・止めると Unknown を確かめ、終わったら（失敗しても）
# コンテナとネットワークを消す。docker と sudo（パスワードなし）が要るので、check・
# test・CI には入れない。手順と結果は v3/providers/README.md。
.PHONY: integration-v3-postgresql
V3_POSTGRESQL_IT := ./postgresql/integration/ ./sqlmigrate/integration/
integration-v3-postgresql:
	$(V3P) go vet -tags integration_v3_postgresql $(V3_POSTGRESQL_IT)
	@status=0; \
	(cd v3/providers && go test -tags integration_v3_postgresql -count=1 -v -timeout 20m $(V3_POSTGRESQL_IT)) || status=$$?; \
	left=$$(sudo -n docker ps -a --format '{{.Names}}' 2>/dev/null | grep -E '^sakutei-v3(pg|sm)-'); \
	if [ -n "$$left" ]; then \
	  echo "integration-v3-postgresql: containers were left behind; removing: $$left"; \
	  sudo -n docker rm -f -v $$left >/dev/null || status=1; \
	fi; \
	for net in sakutei-v3pg-net sakutei-v3sm-net; do \
	  if sudo -n docker network ls --format '{{.Name}}' 2>/dev/null | grep -qx "$$net"; then \
	    echo "integration-v3-postgresql: the network $$net was left behind; removing it"; \
	    sudo -n docker network rm $$net >/dev/null || status=1; \
	  fi; \
	done; \
	exit $$status

# 新しいコアの ldap provider の統合テスト（v3 段 8e、E-G1・N-G1・E-G5・E-K1）。
# 389 Directory Server の公式のイメージ（tag と digest で固定）を
# docker で立て（sakutei-v3ldap-server・sakutei-v3ldap-client・ネットワーク
# sakutei-v3ldap-net。ほかの integration-v3-* と重ならないので同時に回せる）、v3 の CLI
# （cli.Run）で空から作る・手で変えたものを戻す・userPassword は書き込みだけ・何周しても
# 書かない・StartTLS と LDAPS・bind の失敗は Unknown・子のある項目は prune しない・観測と
# 書き込みの資格を分ける・止めると Unknown で何も消さないを確かめ、終わったら（失敗しても）
# コンテナとネットワークを消す。docker と sudo（パスワードなし）が要るので、check・test・
# CI には入れない。手順と結果は v3/providers/README.md。
.PHONY: integration-v3-ldap
V3_LDAP_IT := ./ldap/integration/
integration-v3-ldap:
	$(V3P) go vet -tags integration_v3_ldap $(V3_LDAP_IT)
	@status=0; \
	(cd v3/providers && go test -tags integration_v3_ldap -count=1 -v -timeout 15m $(V3_LDAP_IT)) || status=$$?; \
	left=$$(sudo -n docker ps -a --format '{{.Names}}' 2>/dev/null | grep '^sakutei-v3ldap-'); \
	if [ -n "$$left" ]; then \
	  echo "integration-v3-ldap: containers were left behind; removing: $$left"; \
	  sudo -n docker rm -f -v $$left >/dev/null || status=1; \
	fi; \
	if sudo -n docker network ls --format '{{.Name}}' 2>/dev/null | grep -qx sakutei-v3ldap-net; then \
	  echo "integration-v3-ldap: the network sakutei-v3ldap-net was left behind; removing it"; \
	  sudo -n docker network rm sakutei-v3ldap-net >/dev/null || status=1; \
	fi; \
	exit $$status

# 新しいコアの proxmox provider の統合テスト（v3 段 8d）。入れ子の
# Proxmox VE 9.2（ISO・自動導入の ISO・導入済みのディスクはキャッシュ
# SAKUTEI_IT_CACHE、既定 ~/.cache/sakutei-it/proxmox を使い回す）を qemu で立て、v3 の
# sakutei の CLI で VM の作成・適合・手の変更の戻し・pending・触らない VM・届かない
# ときの Unknown・宣言外・NIC の firewall/mtu と FW の options・観測用のトークン・
# network_policy の実現先 proxmox-fw・keep のディスクの付け替え・承認つきの削除・pool を
# 回し、終われば（落ちても）qemu を止める。KVM・qemu・docker・curl・ssh と sudo
# （パスワードなし）、初回は ISO を取るネットワークが要るので、check・test・CI には
# 入れない。手順と結果は v3/providers/README.md。
V3_PROXMOX_IT := ./proxmox/integration/
integration-v3-proxmox:
	$(V3P) go vet -tags integration_v3_proxmox $(V3_PROXMOX_IT)
	$(V3P) go test -tags integration_v3_proxmox -count=1 -v -timeout 90m $(V3_PROXMOX_IT)

# 新しいコアの s3 provider の統合テスト（v3 段 8f）。S3 互換の
# サーバ（SeaweedFS と MinIO、tag と digest で固定）を docker で 127.0.0.1 の空いた
# ポートに立て（コンテナは sakutei-v3s3-*。ほかの integration-v3-* と重ならない）、v3 の
# sakutei の CLI でバケットの作成・適合・手の変更の戻し・中身のあるバケットの削除の
# 断り・観測用と書き込み用のキー・Object Lock（承認つきの保持の設定の変更）を回し、
# 終わったら（失敗しても）コンテナを消す。docker と sudo（パスワードなし）が要るので、
# check・test・CI には入れない。手順と結果は v3/providers/README.md。
.PHONY: integration-v3-s3 integration-v3-ansible
V3_S3_IT := ./s3/integration/
integration-v3-s3:
	$(V3P) go vet -tags integration_v3_s3 $(V3_S3_IT)
	@status=0; \
	(cd v3/providers && go test -tags integration_v3_s3 -count=1 -v -timeout 15m $(V3_S3_IT)) || status=$$?; \
	left=$$(sudo -n docker ps -a --format '{{.Names}}' 2>/dev/null | grep '^sakutei-v3s3-'); \
	if [ -n "$$left" ]; then \
	  echo "integration-v3-s3: containers were left behind; removing: $$left"; \
	  sudo -n docker rm -f -v $$left >/dev/null || status=1; \
	fi; \
	exit $$status

# 新しいコアの ansible provider の統合テスト（v3 段 8f）。版を固定した
# venv（ANSIBLE_IT_VENV。無ければ作る。入れるものは ansible/integration/requirements.txt）の ansible-core を、
# local 接続で一時ディレクトリを相手に v3 の sakutei の CLI から回す。python3（venv
# モジュール）と、初回は PyPI に届くことが要るので、check・test・CI には入れない。
# SAKUTEI_IT_ANSIBLE で手持ちの ansible を渡せる。手順と結果は v3/providers/README.md。
V3_ANSIBLE_IT := ./ansible/integration/
integration-v3-ansible:
	$(V3P) go vet -tags integration_v3_ansible $(V3_ANSIBLE_IT)
	@if [ -z "$$SAKUTEI_IT_ANSIBLE" ] && [ ! -x $(ANSIBLE_IT_VENV)/bin/ansible ]; then \
	  echo "integration-v3-ansible: creating $(ANSIBLE_IT_VENV)"; \
	  rm -rf $(ANSIBLE_IT_VENV) && python3 -m venv --without-pip $(ANSIBLE_IT_VENV) && \
	  curl -sSfL -o $(ANSIBLE_IT_VENV)/$(PIP_WHEEL) $(PIP_WHEEL_URL) && \
	  echo "$(PIP_WHEEL_SHA256)  $(ANSIBLE_IT_VENV)/$(PIP_WHEEL)" | sha256sum -c - && \
	  $(ANSIBLE_IT_VENV)/bin/python3 $(ANSIBLE_IT_VENV)/$(PIP_WHEEL)/pip install --quiet --only-binary=:all: \
	    -r v3/providers/ansible/integration/requirements.txt || { rm -rf $(ANSIBLE_IT_VENV); exit 1; }; \
	fi
	cd v3/providers && SAKUTEI_IT_ANSIBLE=$${SAKUTEI_IT_ANSIBLE:-$(ANSIBLE_IT_VENV)/bin/ansible} \
	  go test -tags integration_v3_ansible -count=1 -v -timeout 15m $(V3_ANSIBLE_IT)