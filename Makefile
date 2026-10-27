.PHONY: build test test-race vet check demo clean integration-redfish integration-nftables integration-s3 integration-postgresql integration-ansible integration-remote integration-guestos

BIN     := sakutei
DEMODIR := .demo

build:
	go build -o $(BIN) ./cmd/sakutei

test:
	go test ./...

# race detector 付きのテスト。cgo が要るので gcc（C コンパイラ）が必要。
# -count=1 で結果のキャッシュを使わず、毎回実際に回す。
test-race:
	CGO_ENABLED=1 go test -race -count=1 ./...

vet:
	go vet ./...

check: build vet test

# Redfish の統合テスト（#84、設計 §26.1 の 3 段目）。qemu-bmc 0.10.0 を containerlab
# で 2 台立てて回し、終わったら（失敗しても）消す。KVM・docker・containerlab と
# sudo（パスワードなし）が要るので、check・test・CI には入れない。
# 手順と結果は docs/providers/redfish.md の「統合テスト」。
REDFISH_IT := ./tests/integration/redfish/
integration-redfish:
	go vet -tags integration_redfish $(REDFISH_IT)
	@status=0; \
	go test -tags integration_redfish -count=1 -v -timeout 20m $(REDFISH_IT) || status=$$?; \
	if sudo -n docker ps -a --format '{{.Names}}' 2>/dev/null | grep -q '^clab-sakutei-rf84-'; then \
	  echo 'integration-redfish: the lab was left behind; destroying it'; \
	  sudo -n docker unpause clab-sakutei-rf84-node1 clab-sakutei-rf84-node2 >/dev/null 2>&1; \
	  sudo -n containerlab destroy --name sakutei-rf84 --cleanup || status=1; \
	fi; \
	exit $$status

# nftables の live 版の統合テスト（#121）。本物の nft を `sudo -n unshare -n` の
# 使い捨ての network namespace の中で回す（ホストの FW には触れない。namespace は
# テストが終われば消える）。nft・unshare・nsenter と sudo（パスワードなし）が要る
# ので、check・test・CI には入れない。手順と結果は docs/providers/nftables.md の
# 「統合テスト」。
NFTABLES_IT := ./tests/integration/nftables/
integration-nftables:
	go vet -tags integration_nftables $(NFTABLES_IT)
	go test -tags integration_nftables -count=1 -v -timeout 10m $(NFTABLES_IT)

# s3-bucket provider の統合テスト（#122）。docker で S3 互換のサーバ（MinIO・
# SeaweedFS と、PUT の API を持たない古い SeaweedFS）を 127.0.0.1 の空いた
# ポートに立てて回し、終わったら（失敗しても）コンテナを消す。docker と sudo
# （パスワードなし）が要るので、check・test・CI には入れない。イメージは
# SAKUTEI_IT_MINIO_IMAGE などで差し替えられる。手順と結果は
# docs/providers/s3-bucket.md の「統合テスト」。
S3_IT := ./tests/integration/s3/
integration-s3:
	go vet -tags integration_s3 $(S3_IT)
	@status=0; \
	go test -tags integration_s3 -count=1 -v -timeout 15m $(S3_IT) || status=$$?; \
	left=$$(sudo -n docker ps -a --format '{{.Names}}' 2>/dev/null | grep '^sakutei-s3it-'); \
	if [ -n "$$left" ]; then \
	  echo "integration-s3: containers were left behind; removing: $$left"; \
	  sudo -n docker rm -f -v $$left >/dev/null || status=1; \
	fi; \
	exit $$status

# postgresql provider の統合テスト（#125）と sqlmigrate の engine: postgresql の
# 統合テスト（#126、常駐の sakutei で Task を回す）。docker の公式の postgres イメージ
# （tag と digest で固定）でサーバと psql を動かすクライアントを立て（ホストに
# psql は要らない）、終わったら（失敗しても）コンテナとネットワークを消す。
# docker と sudo（パスワードなし）が要るので、check・test・CI には入れない。
# イメージは SAKUTEI_IT_POSTGRES_IMAGE で差し替えられる。手順と結果は
# docs/providers/postgresql.md と docs/providers/sqlmigrate.md の「統合テスト」。
POSTGRESQL_IT := ./tests/integration/postgresql/
integration-postgresql:
	go vet -tags integration_postgresql $(POSTGRESQL_IT)
	@status=0; \
	go test -tags integration_postgresql -count=1 -v -timeout 15m $(POSTGRESQL_IT) || status=$$?; \
	left=$$(sudo -n docker ps -a --format '{{.Names}}' 2>/dev/null | grep '^sakutei-pgit-'); \
	if [ -n "$$left" ]; then \
	  echo "integration-postgresql: containers were left behind; removing: $$left"; \
	  sudo -n docker rm -f -v $$left >/dev/null || status=1; \
	fi; \
	if sudo -n docker network ls --format '{{.Name}}' 2>/dev/null | grep -q '^sakutei-pgit-net$$'; then \
	  sudo -n docker network rm sakutei-pgit-net >/dev/null || status=1; \
	fi; \
	exit $$status

# ansible provider の統合テスト（#127）。本物の ansible-core を、版を固定した
# 使い捨ての venv（ANSIBLE_IT_VENV、初回だけ作る。pip も wheel の sha256 で固定）
# に入れ、local 接続で一時ディレクトリを相手に回す（テストの一時ディレクトリは
# 終われば消える）。python3（venv モジュール）と、初回は PyPI に届くことが要る
# ので、check・test・CI には入れない。SAKUTEI_IT_ANSIBLE で手持ちの ansible を
# 渡せる（版が違えば SAKUTEI_IT_ANSIBLE_ANY=1 も）。手順と結果は
# docs/providers/ansible.md の「統合テスト」。
ANSIBLE_IT       := ./tests/integration/ansible/
ANSIBLE_IT_VENV  ?= $(HOME)/.cache/sakutei/ansible-it-core-2.21.5
PIP_WHEEL        := pip-26.2.2-py3-none-any.whl
PIP_WHEEL_URL    := https://files.pythonhosted.org/packages/3f/06/3e4420302363fbb71be3c7f35ce042f54bfca330e3e66b3a2e3c4cca579f/$(PIP_WHEEL)
PIP_WHEEL_SHA256 := fd7f4dfa8190e014013b9f08f3e973f4e4c26ffd179a204895533a7254813a70
integration-ansible:
	go vet -tags integration_ansible $(ANSIBLE_IT)
	@if [ -z "$$SAKUTEI_IT_ANSIBLE" ] && [ ! -x $(ANSIBLE_IT_VENV)/bin/ansible ]; then \
	  echo "integration-ansible: creating $(ANSIBLE_IT_VENV)"; \
	  rm -rf $(ANSIBLE_IT_VENV) && python3 -m venv --without-pip $(ANSIBLE_IT_VENV) && \
	  curl -sSfL -o $(ANSIBLE_IT_VENV)/$(PIP_WHEEL) $(PIP_WHEEL_URL) && \
	  echo "$(PIP_WHEEL_SHA256)  $(ANSIBLE_IT_VENV)/$(PIP_WHEEL)" | sha256sum -c - && \
	  $(ANSIBLE_IT_VENV)/bin/python3 $(ANSIBLE_IT_VENV)/$(PIP_WHEEL)/pip install --quiet --only-binary=:all: \
	    -r $(ANSIBLE_IT)requirements.txt || { rm -rf $(ANSIBLE_IT_VENV); exit 1; }; \
	fi
	SAKUTEI_IT_ANSIBLE=$${SAKUTEI_IT_ANSIBLE:-$(ANSIBLE_IT_VENV)/bin/ansible} \
	  go test -tags integration_ansible -count=1 -v -timeout 15m $(ANSIBLE_IT)

# ssh 越しの実行の層の統合テスト（#130）。docker で sshd・nft・sudo の入った
# 使い捨てのコンテナを 2 台立て（ベースのイメージは tag と digest で固定。
# nft のために NET_ADMIN）、手元の sakutei から ssh 越しに 2 台の nftables（live）と
# dnszone を収束させ、終わったら（失敗しても）コンテナを消す。docker・ssh・
# ssh-keygen と sudo（パスワードなし）が要るので、check・test・CI には入れない。
# 手順と結果は docs/remote.md の「統合テスト」。
REMOTE_IT := ./tests/integration/remote/
integration-remote:
	go vet -tags integration_remote $(REMOTE_IT)
	@status=0; \
	go test -tags integration_remote -count=1 -v -timeout 20m $(REMOTE_IT) || status=$$?; \
	left=$$(sudo -n docker ps -a --format '{{.Names}}' 2>/dev/null | grep '^sakutei-rmit-'); \
	if [ -n "$$left" ]; then \
	  echo "integration-remote: containers were left behind; removing: $$left"; \
	  sudo -n docker rm -f -v $$left >/dev/null || status=1; \
	fi; \
	exit $$status

# ゲスト OS の観測で OS 導入の完了を確かめる統合テスト（#86）。qemu-bmc 0.10.0 を
# containerlab で 3 台立て、Alpine の自動インストールの ISO（docs/trial/
# redfish-os-install/make-iso.sh で作る）で本当に OS を入れ、終わったら（失敗しても）
# lab を消す。KVM・docker・containerlab・sudo（パスワードなし）・ssh・xorriso・curl と、
# 初回は Alpine の ISO とパッケージを取るネットワークが要る（取ったものは
# SAKUTEI_IT_CACHE、既定 ~/.cache/sakutei-it/alpine に残す）。check・test・CI には
# 入れない。手順と結果は docs/providers/guest-os.md の「統合テスト」。
GUESTOS_IT := ./tests/integration/guestos/
integration-guestos:
	go vet -tags integration_guestos $(GUESTOS_IT)
	@status=0; \
	go test -tags integration_guestos -count=1 -v -timeout 30m $(GUESTOS_IT) || status=$$?; \
	if sudo -n docker ps -a --format '{{.Names}}' 2>/dev/null | grep -q '^clab-sakutei-os86-'; then \
	  echo 'integration-guestos: the lab was left behind; destroying it'; \
	  sudo -n containerlab destroy --name sakutei-os86 --cleanup || status=1; \
	fi; \
	exit $$status

clean:
	rm -rf $(BIN) $(DEMODIR)

# ─────────────────────────────────────────────────────────────────────────────
# make demo — GUI を持たない収束エンジンの「レビュー面」を実演する。
# dnszone（実 provider）を一時ディレクトリ .demo/ で動かし、次の物語を再生する:
#   1) plan       宣言した2レコードが「これから作られる」ことを dry-run で見る
#   2) reconcile  実体ファイル zone.db に収束させる
#   3) status     適合レポート（drifted=0）
#   4) ドリフト注入  誰かが zone.db を手で改ざん＋宣言外レコードを追加 → status が検知
#   5) reconcile  触っていないのに元へ戻る（自己修復）
#   6) status     再び適合（改ざんは戻り、宣言外は prune されず残る）
# 見どころ: 改ざんは cause B（ドリフト）で plan/status が一致、宣言外は unmanaged で可視化。
# すべて読み取り面（plan/status）に出るのがこのソフトのレビュー体験そのもの。
# ─────────────────────────────────────────────────────────────────────────────
demo: build
	@rm -rf $(DEMODIR) && mkdir -p $(DEMODIR)/intent
	@printf '%s\n' \
	  '{' \
	  '  "resources": [' \
	  '    {"provider": "dnszone", "id": "www.example.com./A", "fields": {"name": "www.example.com.", "type": "A", "value": "192.0.2.10", "ttl": "300"}},' \
	  '    {"provider": "dnszone", "id": "api.example.com./A", "fields": {"name": "api.example.com.", "type": "A", "value": "192.0.2.20", "ttl": "300"}}' \
	  '  ]' \
	  '}' > $(DEMODIR)/intent/zone.json
	@echo ''
	@echo '▶ intent（人が書く「あるべき姿」）: www=192.0.2.10, api=192.0.2.20'
	@echo ''
	@echo '════════ 1) sakutei plan — 何が変わるか（dry-run、まだ触らない）════════'
	@./$(BIN) plan --intent $(DEMODIR)/intent --zone $(DEMODIR)/zone.db || true
	@echo ''
	@echo '════════ 2) sakutei reconcile — 1回収束（zone.db を実際に書く）════════'
	@./$(BIN) reconcile --intent $(DEMODIR)/intent --zone $(DEMODIR)/zone.db || true
	@echo ''
	@echo '── 実体 zone.db ──'
	@cat $(DEMODIR)/zone.db
	@echo ''
	@echo '════════ 3) sakutei status — 適合レポート（今そろっている？）════════'
	@./$(BIN) status --intent $(DEMODIR)/intent --zone $(DEMODIR)/zone.db || true
	@echo ''
	@echo '▶ 誰かが zone.db を手で改ざん（www を 10.6.6.6 に + 宣言外 rogue を追加）'
	@printf '%s\n' \
	  'www.example.com. 300 IN A 10.6.6.6' \
	  'api.example.com. 300 IN A 192.0.2.20' \
	  'rogue.example.com. 300 IN A 192.0.2.99' > $(DEMODIR)/zone.db
	@echo ''
	@echo '════════ 4) sakutei status — ドリフト検知（触っていないのに気づく）════════'
	@./$(BIN) status --intent $(DEMODIR)/intent --zone $(DEMODIR)/zone.db || true
	@echo ''
	@echo '════════ 5) sakutei reconcile — 自己修復（触っていないのに戻る）════════'
	@./$(BIN) reconcile --intent $(DEMODIR)/intent --zone $(DEMODIR)/zone.db || true
	@echo ''
	@echo '── 実体 zone.db（www が戻り、rogue は prune されず残る＝確信なき減算はしない）──'
	@cat $(DEMODIR)/zone.db
	@echo ''
	@echo '════════ 6) sakutei status — 再確認 ════════'
	@./$(BIN) status --intent $(DEMODIR)/intent --zone $(DEMODIR)/zone.db || true
	@echo ''
	@echo '▶ 見どころ: 4)の www は cause B（ドリフト）で plan/status 一致。'
	@echo '            rogue は unmanaged（宣言外・実在・prune 対象外）として可視化。'
	@echo '            6)で www は自己修復済み、rogue は減算されず残る。'
