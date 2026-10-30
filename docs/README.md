# Sakutei ドキュメント

Sakutei（作庭）の設計ドキュメント一覧。現時点ではコードは未実装で、本リポジトリは**設計仕様のみ**を保持する。

## 設計仕様（architecture/）

| ドキュメント | 位置づけ | 状態 |
| --- | --- | --- |
| [architecture/spec/README.md](architecture/spec/README.md) | **設計の正（source of truth）の索引。** v3.1（骨格は v3.0）。8 部 29 節の分冊構成（part1〜part8）。§番号は全部を通して一意。 | v3.1（最新・権威） |
| architecture/spec/part1〜8-\*.md | 各部の本文（思想と境界 / 中核モデル / 収束機構 / 安全機構 / スケールと分散 / 衛星と外部世界 / 運用ライフサイクル / 実装）。 | v3.0 |
| [architecture/CHANGELOG-v3.1.md](architecture/CHANGELOG-v3.1.md) / [v3.0](architecture/CHANGELOG-v3.0.md) | v3.0→v3.1（位置づけを OSS 個人・Apache-2.0 に確定）／ v2.6→v3.0 の構成対応マップ。 | — |
| architecture/old/ | 旧版アーカイブ（歴史として保持、編集しない）: `sakutei-design-v2.2〜v2.6.md`、各 `CHANGELOG-v2.x.md`、`reconciler-design-v1.md`（コアの初期設計メモ・背景資料）。矛盾する場合は **v3.0 が優先**。 | 旧版 |

## 実装例（examples/）

| ドキュメント | 内容 |
| --- | --- |
| [examples/proxmox-mysql-dbaas-schema.md](examples/proxmox-mysql-dbaas-schema.md) | v2.2 の §5（Provider 実装ガイド）と §7.5（合成リソース）の worked example。`proxmox_vm` を基底 Provider として定義し、その上に MySQL DBaaS を合成リソースとして構築する具体例。規範仕様ではなく解説用。 |

## provider の使い方（providers/）

| ドキュメント | 内容 |
| --- | --- |
| [providers/ranges.md](providers/ranges.md) | provider の設定のキーの一覧：つなぎ方（宛先・鍵・時間。provider の設定に残す）と管理の範囲（scope・扱うホスト・検索の根と filter・部分木・目印・バケットの接頭辞。intent の `providers:` に書く、§7.7、Backlog #181・#182）の分け方と、設定にだけあれば警告・食い違えば断る決まり。 |
| [providers/redfish.md](providers/redfish.md) | Redfish の BMC provider（`redfish-system`・`redfish-virtual-media`・`redfish-bios`・`redfish-firmware`）。BMC の一覧（`--redfish-config`、パスワードは SecretRef）と intent の例。BIOS の設定（宣言した属性だけを `Bios/Settings` で合わせる・効くのは再起動のあと）。FW の更新（UpdateService・承認ゲートつきの多相 Task、統合テストは偽のサーバまで）。qemu-bmc + containerlab の統合テスト（`make integration-redfish`）の回し方と結果。 |
| [providers/sqlmigrate.md](providers/sqlmigrate.md) | SQLite・PostgreSQL のマイグレーション provider（`sqlmigrate`、単純 Task 専用）。データベースの一覧（`--sqlmigrate-config`）、マイグレーションのファイル名の決まり、intent の例（`strategy: task`・`window`・`rollback_on_fail`）、plan・reconcile・常駐での動き方、PostgreSQL（`engine: postgresql`・advisory lock・トランザクションに入れられない文・統合テスト）。 |
| [providers/nftables.md](providers/nftables.md) | nftables provider（`nftables`）。対象 `file`（管理された ruleset ファイル）と `live`（カーネルの実際の ruleset を `nft` で読み・流す）の違い、設定（`--ruleset-target`・`--ruleset-tables`・`--ruleset-nft`・`--provider-config`）、権限（CAP_NET_ADMIN）、ルールの正規形。base chain（既定の拒否、`policy drop`）の宣言と、到達性の最小の核を Sakutei の外で保守すること、base chain を変える Apply の締め出しの守り（写し・ホストの側の時間切れの巻き戻し・新しい接続での確かめ、`--ruleset-rollback-timeout`・`--ruleset-confirm-address`、Backlog #173）。確かめる先の宣言（`confirm`）と変更の前の確かめ、base chain の作成・policy の変更が承認（planned_change のレーン）と予算を通ること（Backlog #179）。変える chain を通る経路の先は空から組めない注意と、変えたあとだけ確かめる印 `after:`（Backlog #192）。使い捨ての network namespace と、sshd の入った使い捨てのコンテナに ssh で入る統合テスト（`make integration-nftables`）の回し方と結果。 |
| [network-policy.md](network-policy.md) | 網の間の許可（intent の `network_policy:`、Backlog #174・#195・#197）。実現先は nftables と proxmox-fw（VM の FW の規則、関所の VM の例）。網（名前と CIDR、domain＝アドレスの空間）と許可（from・to・proto・ports）と行ごとの理由 `reason`・印 `tags` の書き方、既定の拒否、検査（重なり・宣言に無い網・理由の無い例外・重複は断る、隠れた許可・どこも通らない許可は知らせる）、実現先（境界・ホスト）ごとの nftables の base chain への展開と comment の決まり、plan の展開の節、別の実現先への変換を足す口（`cli.WithRealizer`）。 |
| [providers/dnszone.md](providers/dnszone.md) | dnszone provider（`dnszone`）。ID とフィールド、管理の境界（SOA・NS）と SOA シリアル、設定（`--zone`・`--zone-soa-serial`・`--zone-scope`・`zone_reload`）、離れたホストのゾーンファイル（`zone_hosts`）。 |
| [providers/ansible.md](providers/ansible.md) | Ansible provider（`ansible`、最小の階級）。設定（`--ansible-config`：inventory・ansible のパス・env・timeout・max_parallel）、リソース（host・module・args・destructive）と SecretRef、check mode での観測と落ちる保証（差分・原因タグ・converging・破壊的の重み・prune）、observe_tiers での間引き。本物の ansible-core 2.21.5 での統合テスト（`make integration-ansible`）の回し方と結果。 |
| [providers/s3-bucket.md](providers/s3-bucket.md) | S3 互換のバケット provider（`s3-bucket`）。endpoint の一覧（`--s3-config`、資格は SecretRef、`bucket_prefix`）、フィールド（versioning・policy・lifecycle・tags）と正規形、中身のあるバケットを消さない決まり、API を持たない endpoint の扱い。docker の SeaweedFS（必須）・MinIO（任意）での統合テスト（`make integration-s3`）の回し方と結果。 |
| [providers/postgresql.md](providers/postgresql.md) | PostgreSQL provider（`postgresql`）。サーバの一覧（`--postgresql-config`、管理用のパスワードは SecretRef）、ロール・データベース・権限・拡張の ID とフィールド、パスワードの扱い（SCRAM-SHA-256・`verify_passwords`）、管理の境界、利用者の表があるデータベースを消さない決まり。docker の公式の postgres イメージでの統合テスト（`make integration-postgresql`）の回し方と結果。 |
| [../examples/external-provider/README.md](../examples/external-provider/README.md) | 外の provider を足す（§28.9）。Sakutei を Go のライブラリとして使い、`pkg/cli` の入口に自前の provider を登録した自前のバイナリを作る例と、provider ごとの設定ファイル（`--provider-config`）。 |
| [providers/guest-os.md](providers/guest-os.md) | ゲスト OS を観測するだけの provider（`guest-os`、Backlog #86）。ssh 越しに届くか・`/etc/os-release`・導入の目印（`guest_markers`）を読み、OS 導入の Workflow が「電源が切れた」でなく「ゲスト OS が目印つきで起動した」で完了を判断する書き方。qemu-bmc 3 台で本当に Alpine を入れる統合テスト（`make integration-guestos`）の回し方と結果。 |
| [providers/metrics-probe.md](providers/metrics-probe.md) | /metrics（Prometheus の text 形式）の値を観測するだけの provider（`metrics-probe`、Backlog #149）。メトリクスの名前・ラベルの一致・`sum`/`max`・比べ方（`"< 1000"`）を宣言し、結果 `ready.<名前>` を Workflow の `require` や ready_condition から待つ書き方。届かない・無い・壊れているは unknown。 |
| [providers/template-file.md](providers/template-file.md) | 利用者の雛形（`text/template`）に台ごとの値（ホスト名・アドレス・ssh の公開鍵・導入の目印）を入れたファイルを書き出す provider（`template-file`、Backlog #139）。OS の自動導入の応答ファイルを宣言から作る。最小階級（描いた中身とファイルを比べる）・秘密は書き出さない。 |
| [providers/host-file.md](providers/host-file.md) | 離れたホスト（または手元）のファイルを保つ provider（`host-file`、Backlog #155）。ID は `<ホスト>/<絶対パス>`、中身（平文か SecretRef）・mode・owner・group。観測は中身の hash と stat だけで、平文を観測・Event に出さない。 |
| [providers/systemd-unit.md](providers/systemd-unit.md) | 離れたホスト（または手元）の systemd のユニットと podman の quadlet を保つ provider（`systemd-unit`、Backlog #155）。元のファイル・active / inactive・enabled、依って立つファイルを書き換えたら 1 回だけ入れ替える `restart_on`（ホストの側の印で level に判定）。quadlet の例と、systemd を PID 1 で動かすコンテナでの統合テスト（`make integration-hostconf`）の回し方と結果。 |
| [providers/cli-device.md](providers/cli-device.md) | CLI でしか設定できない装置を扱う汎用の provider（`cli-device`、Backlog #159）。機種の定義のファイル（観測のコマンドと読み方〈正規表現・キーと値・表〉・変えるコマンドの雛形・値の形・エラーの印・確定と取り消し・一度きりの操作）の書き方と、架空の装置の例。exec と対話（プロンプト・ページ送り）、定義に合わない出力は unknown、部分的な失敗の扱い。 |
| [providers/netconf.md](providers/netconf.md) | NETCONF の機器の設定の部分木を観測してずれを知らせ、設定で許したときは commit confirmed の 2 段で当てる provider（`netconf`、Backlog #141・#175・#176・#199・#208）。既定は観測だけ（`Capabilities.ObserveOnly`）。ssh の netconf サブシステム・1.0 と 1.1 の区切り・名前空間と空白と順をそろえた正規形・`ordered`。`netconf_apply` で許す 2 段の Apply（`:confirmed-commit:1.0` を主、新しい接続での確かめ、時間切れの巻き戻し、1.1 の persist）。`commit_group` で複数の台の変更をまとめて確定する（全部の台で確かめが通ったときだけ確定、残る窓）。機器の側（NACM）で部分木だけに絞ったユーザーで動かす（絞り方の例）。netopeer2 を相手の統合テスト（`make integration-netconf`）の回し方と結果。 |
| [providers/ldap.md](providers/ldap.md) | LDAP の項目を宣言した属性だけ合わせる provider（`ldap`、Backlog #144）。OpenLDAP のクライアント（`ldapsearch`・`ldapmodify`）を exec、bind のパスワードは SecretRef を `-y` の一時ファイルで、LDAPS・StartTLS。DN の正規形の ID、値の集合の書き方、objectClass の上位のクラス・DN の値の比べ方、書き込みだけの `userPassword`、管理の境界（base_dn・filter）と子のある項目を消さない決まり。389 Directory Server を相手の統合テスト（`make integration-ldap`）の回し方と結果。 |
| [providers/proxmox-vm.md](providers/proxmox-vm.md) | Proxmox VE の VM provider（`proxmox-vm`、Backlog #143・#204）。クラスタの一覧（`proxmox_config`、API トークンの secret は SecretRef、触らない VM。触らない VM を intent の `external_owners:` に書く、#158）、フィールド（vmid・template・cores・memory・disks・nics・cloud-init（初回のパッケージ更新 `ciupgrade` を含む、#170）・NIC の `firewall`・`mtu` と VM の FW の options（`firewall`、#196）・power・apply_pending）と正規形、作り直すときにデータのディスクを引き継ぐ `keep`・`volume=`（#204）、再起動を待つ変更（pending）・非同期の仕事（UPID）の扱い。本物の Proxmox VE 9.2 を入れ子の KVM で自動導入して回す統合テスト（`make integration-proxmox`）の回し方と結果。 |
| [providers/proxmox-fw.md](providers/proxmox-fw.md) | Proxmox VE の VM の FW の規則の provider（`proxmox-fw`、Backlog #197）。network_policy の実現先 `proxmox-fw` が作る。ID `<クラスタ>/<VM 名>/fw`、規則の文の書き方、Sakutei の印（comment の `sakutei: `）のある規則だけを書き換えて宣言の順に先頭に保つこと、印の無い規則は消さずに status の unmanaged（`.../fw/<pos>`）に出すこと、規則の変更を破壊的として予算に数えること、Apply の手順、観測用と書き込み用の権限（`VM.Audit`・`VM.Config.Network`）、Proxmox VE 9.2 で確かめた API の決まり |
| [providers/secret-backends.md](providers/secret-backends.md) | 秘密の取り出し先（`secret_backends:`）：Vault 形の KV v2（OpenBao で試験。token のファイルと AppRole、世代は KV の版）と外のコマンド。render の段で世代を読んで ref に入れ、読めなければそのリソースは unknown（#188） |
| [remote.md](remote.md) | 離れたホストを扱う口（ssh 越しの実行の層、Backlog #130）。対象の一覧（`remote_config`：address・user・秘密鍵の SecretRef・known_hosts・sudo・タイムアウト）、ID にホストの名前が付くこと、鍵とホスト鍵の扱い、接続の使い回し、権限、届かないときの答え、dnszone の再読み込み（`zone_reload`）。docker の sshd 2 台での統合テスト（`make integration-remote`）の回し方と結果。。観測用と書き込み用の資格（ホストの `observe:`・`write:`。書かない回は書き込み用の鍵が無くても起動する。読むだけのユーザーで観測するときに要るものと観測できない項目、#185） |
| [tracing.md](tracing.md) | トレースの出口（Backlog #189）：周・観測・plan・Apply・Workflow の段を span にして OTLP/HTTP（JSON の形）で Collector へ出す。既定は出さない（`--otlp-endpoint` を明示したときだけ、`OTEL_*` は読まない）。span と属性の一覧、送れないときの扱い、宛先の TLS と認可（`--otlp-config`、読み取りの口と同じ考え）、統合テスト。 |
| [exit-codes.md](exit-codes.md) | 各コマンドの終了コードの表（Backlog #166）。`reconcile` の 4（Apply は成功を返したが現実が宣言どおりにならなかった・Apply を止めたリソースがある）、`status` の 3、`allocator` の 2 など、今ある値を 1 か所に。 |
| [status-json.md](status-json.md) | `sakutei status --json` の出力の形（schema_version 1）。要約・リソースごとの状態・宣言外の検出ができなかった provider。 |
| [plan-json.md](plan-json.md) | `sakutei plan --json` の出力の形（schema_version 1、Backlog #135）。操作ごとの種類・フィールドの前と後・原因タグ・使う予算のレーン、予算・容量・alert-only の scope で止まる見込みとその理由、Workflow に任せている対象、採番待ち、直してよい時間帯の外か、計画のエラー。 |
| [allocation-file.md](allocation-file.md) | 採番の書き戻しのファイルの書式（schema_version 1、Backlog #157）。プールの `write_back: true`・owner の書き方・置き場とコミットの仕方（`commit` / `review` / `file`、`--alloc-write-back`）・ファイルと store が食い違うとき／取り込み待ちに Apply を止める決まり・`sakutei allocator import` で store を作り直す手順。 |
| [metrics.md](metrics.md) | デーモンの `/metrics`（Prometheus の text format、Backlog #54・§27.5）。`--metrics-listen` で明示したときだけ待ち受ける。出す値（drifted・unknown の件数、予算の残り、halt の有無、cycle と Observe の所要時間）と意味。 |
| [read-api.md](read-api.md) | デーモンの読み取り専用の口（Backlog #136）。`--read-listen` で明示したときだけ待ち受ける。`GET /status`・`/plan`（一回性の `--json` と同じ形に周の番号と時刻を足したもの）・`/events`（Server-Sent Events）・`/graph`（依存の DAG。`sakutei graph --json` と同じ形）。認証を持たないので外に出すときの守り方。 |
| [trial/redfish-os-install/README.md](trial/redfish-os-install/README.md) | Redfish と Workflow で OS を入れる手順（自動インストールの ISO は利用者が用意する）。intent と Workflow の例、試用の結果、限界。 |

## レビュー・適用可能性検討（review/）

| ドキュメント | 内容 |
| --- | --- |
| [review/README.md](review/README.md) | **第 1 ラウンド総括（対象: v2.2）。** 設計レビュー（3 観点）と 20 構成での適用可能性検討の判定一覧・横断的発見・設計へのフィードバック。この指摘が v2.3 に反映された。 |
| review/01〜03-\*-review.md | 設計レビュー各論（整合性・安全性・実装可能性。対象: v2.2）。 |
| review/scenarios/01〜20-\*.md | 20 構成それぞれの詳細検討（対象: v2.2）。 |
| [review/v2.3/README.md](review/v2.3/README.md) | **第 2 ラウンド総括(対象: v2.3)。** 判定の変化一覧（改善 10・据え置き 10・悪化 0）と残存課題。 |
| review/v2.3/scenarios/01〜20-\*.md | v2.3 での再検討（前回ギャップの解消/部分/未解消を差分判定）。 |
| [review/v2.4/README.md](review/v2.4/README.md) | **第 3 ラウンド総括(対象: v2.4)。** 早期収束パス+スケールアウト経路の影響 7 構成の差分再判定と全 20 構成の現在地。 |
| review/v2.4/scenarios/\*.md | v2.4 での再検討（04/06/07/11/14/19/20 の 7 構成）。 |
| [review/v2.5/README.md](review/v2.5/README.md) | **第 4 ラウンド総括(対象: v2.5)。** 衛星の天井・BulkObserve 契約・S2-free シャードの影響 7 構成の差分再判定と v2.2 からの推移。 |
| review/v2.5/scenarios/\*.md | v2.5 での再検討（04/06/07/11/14/19/20 の 7 構成）。 |
| [review/v2.6/README.md](review/v2.6/README.md) | **第 5 ラウンド総括(対象: v2.6)＝収束判定。** 7 構成の検証で 2 ラウンド連続新規ギャップゼロを確認し設計収束を宣言。最終判定分布（◎6/○10/△2/✗0）。 |
| review/v2.6/scenarios/\*.md | v2.6 での検証（01/02/06/09/11/13/17 の 7 構成）。 |
| [review/v3.0-audit-coverage.md](review/v3.0-audit-coverage.md) / [v3.0-audit-consistency.md](review/v3.0-audit-consistency.md) | v3.0 全面再構成の監査（決定事項の網羅性 73 件照合・整合性）。指摘はすべて v3.0 に反映済み。 |

## プレゼンテーション（web/）

| ファイル | 内容 |
| --- | --- |
| [web/sakutei.html](web/sakutei.html) | 設計思想を庭園メタファで表現したビジュアルなランディングページ（単一 HTML）。 |

## 読む順序の推奨

1. `architecture/spec/README.md` — 設計の正の索引。ここから 8 部（思想→モデル→収束→安全→スケール→衛星→運用→実装）を辿る
2. `architecture/old/reconciler-design-v1.md` — コアの制御ループと型を深掘り（背景資料）
3. `examples/proxmox-mysql-dbaas-schema.md` — Provider / 合成リソースの具体像
4. `review/v2.6/README.md` — 適用可能性の到達点（最終判定分布）と収束宣言
5. 設計の変遷を追う場合: `architecture/old/CHANGELOG-v2.3〜v2.6.md` → `architecture/CHANGELOG-v3.0.md` → `architecture/CHANGELOG-v3.1.md` を順に
