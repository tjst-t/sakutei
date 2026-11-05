# Sakutei ドキュメント

Sakutei（作庭）の文書の索引。リポジトリには 2 つの系統がある。

- **v3（新しいコア）**：`v3/`（モジュール `github.com/tjst-t/sakutei/v3`）。設計書は [core/README.md](core/README.md)。Stage 1〜9 がすべて main に入っている。未リリース。
- **v2 系**：リポジトリのルート（モジュール `github.com/tjst-t/sakutei/v2`）。最新のリリースは `v2.12.0`。設計書は [architecture/spec/](architecture/spec/README.md)（v3.77）。v3 への移行が終わったら v2 のコードは削除する予定。

設計書の「v3.77」は v2 系の設計書のバージョン番号で、新しいコア（v3）とは別のもの。

## まず読むもの

| ドキュメント | 内容 |
| --- | --- |
| [../README.md](../README.md) | リポジトリの入口。v2 系と v3 の紹介、CLI、provider の一覧、開発コマンド。 |
| [writing-style.md](writing-style.md) | 文書の書き方。説明文は日本語、技術用語は英語のまま。使わない書き方と使う書き方の用語の表。文書・コメント・CLI のメッセージはこれに従う。 |
| [core/README.md](core/README.md) | v3 の設計書。 |
| [architecture/spec/README.md](architecture/spec/README.md) | v2 系の設計書の索引。 |

## v3（新しいコア）

| ドキュメント | 内容 |
| --- | --- |
| [core/README.md](core/README.md) | **v3 の設計の正。** 5 つの原則、やめるものと戻す目安、1 cycle の流れ、9 つの部品（Resource・Range・Provider・Condition・Graph・Policy・Procedure・Expand・Record と Events）、30 項の不変条件と守るテスト、Record・Events・read-only HTTP API・接続設定、provider SDK、v2 から移すもの・捨てるもの、作る順番（Stage 1〜9）。 |
| [../v3/README.md](../v3/README.md) | コアのパッケージの一覧、CLI と終了コード、Stage ごとの決まり。 |
| [../v3/providers/README.md](../v3/providers/README.md) | コアの外のモジュール（provider・Expand・secret backend・sink・承認ツール・`sakutei-migrate`・`sakutei-import`）。provider ごとの接続設定と intent の例、統合テスト（`make integration-v3-*`）の回し方と結果。 |
| [trial/README.md](trial/README.md) の「v3 で試す」 | `trial/v3/run.sh` で DNS と FW の試用の手順を v3 で流す。v2 の intent を `sakutei-migrate` で v3 に書き直す手順。 |
| [review/core-reduction-draft.md](review/core-reduction-draft.md) | v3 のもとにした下書き。v2.12.0 を見直した結果とコアの案。 |
| [review/core-usecase-review.md](review/core-usecase-review.md) | v3 のもとにした検討。303 件のユースケースで部品の案を確かめたもの。設計書の `E-…`・`N-…` の ID はこの文書の ID。 |

## v2 系の設計（architecture/）

| ドキュメント | 内容 | 状態 |
| --- | --- | --- |
| [architecture/spec/README.md](architecture/spec/README.md) | **v2 系の設計の正（source of truth）の索引。** 8 部 29 節の分冊（part1〜part8）。§番号は全部を通して一意。 | v3.77（現行） |
| architecture/spec/part1〜8-\*.md | 各部の本文（思想と境界 / 中核モデル / 収束機構 / 安全機構 / スケールと分散 / 衛星と外部世界 / 運用ライフサイクル / 実装）。 | v3.77 |
| architecture/CHANGELOG-v3.\*.md | 設計書の改訂ごとの変更一覧（[v3.0](architecture/CHANGELOG-v3.0.md) は v2.6 → v3.0 の構成対応マップ、v3.1〜v3.77 は各改訂）。 | — |
| architecture/old/ | 旧バージョンのアーカイブ（編集しない）: `sakutei-design-v2.2〜v2.6.md`、各 `CHANGELOG-v2.x.md`、`reconciler-design-v1.md`（コアの初期設計メモ・背景資料）。矛盾したら **spec/ が優先**。 | 旧バージョン |
| DESIGN/・VISION.json・DESIGN_PRINCIPLES.json | v2 系の設計の要約（JSON）と ADR。 | v2 系 |
| [../CHANGELOG.md](../CHANGELOG.md) | v2 系のリリースごとの変更点（`#nn` は Backlog の番号）。 | — |

## v2 系の provider（providers/）

| ドキュメント | 内容 |
| --- | --- |
| [providers/ranges.md](providers/ranges.md) | provider の設定のキーの一覧。接続設定（宛先・鍵・タイムアウト。provider の設定に残す）と Range（scope・扱うホスト・検索の根と filter・部分木・marks・バケットの接頭辞。intent の `providers:` に書く、§7.7）の分け方。設定にだけあれば警告し、食い違えば読み込み時にエラーにする。 |
| [providers/dnszone.md](providers/dnszone.md) | `dnszone`。ID と field、管理の境界（SOA・NS）と SOA のシリアル、設定、離れたホストのゾーンファイル（`zone_hosts`）。 |
| [providers/nftables.md](providers/nftables.md) | `nftables`。対象 `file`（ruleset ファイル）と `live`（カーネルの ruleset）、設定、権限、ルールの正規形。base chain（既定の拒否）と、締め出しを防ぐ守り（写し・ホスト側のタイムアウトでの巻き戻し・新しい接続での確認）。`confirm` の宣言と `after:`、承認と budget。統合テスト（`make integration-nftables`）。 |
| [providers/redfish.md](providers/redfish.md) | Redfish の BMC provider（`redfish-system`・`redfish-virtual-media`・`redfish-bios`・`redfish-firmware`）。BMC の一覧（パスワードは SecretRef）と intent の例、BIOS の設定、FW の更新（承認ゲートつきの多相 Task）。統合テスト（`make integration-redfish`）。 |
| [providers/proxmox-vm.md](providers/proxmox-vm.md) | `proxmox-vm`。クラスタの一覧（API トークンは SecretRef）、field と正規形、データのディスクを引き継ぐ `keep`・`volume=`、再起動待ちの変更（pending）と非同期の仕事（UPID）。統合テスト（`make integration-proxmox`）。 |
| [providers/proxmox-fw.md](providers/proxmox-fw.md) | `proxmox-fw`（VM の FW の規則。network policy の実現先）。Sakutei の印のある規則だけを書き換え、印の無い規則は unmanaged として出す。規則の変更は破壊的として budget に数える。観測用と書き込み用の権限。 |
| [providers/netconf.md](providers/netconf.md) | `netconf`。既定は観測だけ。正規形と `ordered`、commit confirmed による two-phase Apply、複数の台をまとめて確定する `commit_group`、NACM で部分木に絞ったユーザー。統合テスト（`make integration-netconf`）。 |
| [providers/cli-device.md](providers/cli-device.md) | `cli-device`（CLI でしか設定できない装置）。機種の定義ファイル（観測のコマンドと読み方・変更のコマンドの雛形・エラーの判定・確定と取り消し）の書き方と、架空の装置の例。 |
| [providers/host-file.md](providers/host-file.md) | `host-file`（離れたホストのファイル）。中身（平文か SecretRef）・mode・owner・group。観測は hash と stat だけ。 |
| [providers/systemd-unit.md](providers/systemd-unit.md) | `systemd-unit`（systemd のユニットと podman の quadlet）。依存するファイルを書き換えたら 1 回だけ再起動する `restart_on`。統合テスト（`make integration-hostconf`）。 |
| [providers/guest-os.md](providers/guest-os.md) | `guest-os`（ゲスト OS を観測するだけ）。OS 導入の Workflow が「ゲスト OS が marks つきで起動した」で完了を判断する書き方。統合テスト（`make integration-guestos`）。 |
| [providers/metrics-probe.md](providers/metrics-probe.md) | `metrics-probe`（/metrics の値を観測するだけ）。Workflow の `require` や ready_condition から値を待つ書き方。 |
| [providers/template-file.md](providers/template-file.md) | `template-file`（雛形から台ごとのファイルを書き出す）。OS の自動導入の応答ファイルを宣言から作る。 |
| [providers/postgresql.md](providers/postgresql.md) | `postgresql`。ロール・データベース・権限・拡張、パスワードの扱い（SCRAM-SHA-256）、管理の境界。統合テスト（`make integration-postgresql`）。 |
| [providers/sqlmigrate.md](providers/sqlmigrate.md) | `sqlmigrate`（SQLite・PostgreSQL のマイグレーション、単純 Task 専用）。ファイル名の決まり、intent の例、plan・reconcile・daemon での動き方。 |
| [providers/ldap.md](providers/ldap.md) | `ldap`。DN の正規形の ID、値の集合での比較、書き込みだけの `userPassword`、管理の境界。統合テスト（`make integration-ldap`）。 |
| [providers/s3-bucket.md](providers/s3-bucket.md) | `s3-bucket`（S3 互換のバケット）。versioning・policy・lifecycle・tags・Object Lock、中身のあるバケットを消さない決まり。統合テスト（`make integration-s3`）。 |
| [providers/ansible.md](providers/ansible.md) | `ansible`（最小の階級）。check mode での観測と、その分落ちる保証。統合テスト（`make integration-ansible`）。 |
| [providers/secret-backends.md](providers/secret-backends.md) | secret backend（`secret_backends:`）。Vault 形の KV v2（token のファイルと AppRole、世代は KV のバージョン）と外のコマンド。読めなければそのリソースは Unknown。 |
| [../examples/external-provider/README.md](../examples/external-provider/README.md) | 外の provider を足す（§28.9）。`pkg/cli` に自前の provider を登録したバイナリを作る例と、provider ごとの設定ファイル（`--provider-config`）。 |

## v2 系の運用と機能

| ドキュメント | 内容 |
| --- | --- |
| [production.md](production.md) | 本番の構成の手引き。置き場の分け方、観測用と書き込み用の credential、secret backend、audit と外への転送、/metrics と read-only HTTP API の TLS と認可、freeze、intent の `runtime:` とフラグ、systemd のユニットの例。 |
| [network-policy.md](network-policy.md) | network policy（intent の `network_policy:`）。ネットワーク・許可・行ごとの `reason` と `tags`、既定の拒否、検査、実現先（nftables・proxmox-fw）への展開、別の実現先を足す `cli.WithRealizer`。 |
| [remote.md](remote.md) | ssh 越しに離れたホストを扱う層。対象の一覧（`remote_config`）、鍵とホスト鍵、観測用と書き込み用の credential（`observe:`・`write:`）。統合テスト（`make integration-remote`）。 |
| [allocation-file.md](allocation-file.md) | 採番の書き戻しファイルの書式（`write_back: true`）。コミットの仕方、ファイルと store が食い違うときの決まり、`sakutei allocator import`。 |
| [provider-upgrade.md](provider-upgrade.md) | provider のバージョンを上げるときの注意。最初の `reconcile` が `ProviderUpgradeDiff`（cause C）で 1 回止まる意味と、確かめてから進める手順。 |
| [tracing.md](tracing.md) | トレースの出力（OTLP/HTTP の JSON）。`--otlp-endpoint` を明示したときだけ出す。span と属性の一覧、TLS と認可。統合テスト（`make integration-otlp`）。 |
| [metrics.md](metrics.md) | daemon の `/metrics`（Prometheus の text format）。`--metrics-listen` を明示したときだけ待ち受ける。 |
| [read-api.md](read-api.md) | daemon の read-only HTTP API（`/status`・`/plan`・`/events`・`/graph`）。`--read-listen` を明示したときだけ待ち受ける。 |
| [exit-codes.md](exit-codes.md) | 各コマンドの終了コードの表。 |
| [plan-json.md](plan-json.md) | `sakutei plan --json` の出力の形。 |
| [status-json.md](status-json.md) | `sakutei status --json` の出力の形。 |

## 試用の手順（trial/）

| ドキュメント | 内容 |
| --- | --- |
| [trial/README.md](trial/README.md) | v1 を dnszone と nftables で動かして確かめる手順。「v3 で試す」と、v2 の intent を v3 に書き直す手順もここにある。 |
| trial/allocation・dr-mode・observe・planned-change・remediation-profile・sharding・sqlmigrate・sync-import・workflow-gates | v2 系の各機能を手で試す手順（各ディレクトリの README.md と run.sh）。 |
| [trial/redfish-os-install/README.md](trial/redfish-os-install/README.md) | Redfish と Workflow で OS を入れる手順（自動インストールの ISO は利用者が用意する）。 |

## 実装例（examples/、v2 系）

| ドキュメント | 内容 |
| --- | --- |
| [examples/proxmox-mysql-dbaas-schema.md](examples/proxmox-mysql-dbaas-schema.md) | v2.2 の §5（Provider 実装ガイド）と §7.5（合成リソース）の worked example。規範仕様ではなく解説用。 |

## レビューと適用可能性の検討（review/、v2 系）

| ドキュメント | 内容 |
| --- | --- |
| [review/README.md](review/README.md) | **第 1 ラウンドの総括（対象: v2.2）。** 設計レビュー（3 観点）と 20 構成での適用可能性の判定一覧。この指摘が v2.3 に反映された。 |
| review/01〜03-\*-review.md | 設計レビューの各論（整合性・安全性・実装可能性。対象: v2.2）。 |
| review/scenarios/01〜20-\*.md | 20 構成それぞれの詳しい検討（対象: v2.2）。 |
| [review/v2.3/README.md](review/v2.3/README.md) | **第 2 ラウンドの総括（対象: v2.3）。** 判定の変化（改善 10・据え置き 10・悪化 0）と残った課題。 |
| [review/v2.4/README.md](review/v2.4/README.md) | **第 3 ラウンドの総括（対象: v2.4）。** 早期収束パスとスケールアウトの影響を受ける 7 構成の再判定。 |
| [review/v2.5/README.md](review/v2.5/README.md) | **第 4 ラウンドの総括（対象: v2.5）。** 衛星の天井・BulkObserve の契約・S2-free シャードの影響を受ける 7 構成の再判定。 |
| [review/v2.6/README.md](review/v2.6/README.md) | **第 5 ラウンドの総括（対象: v2.6）＝収束の判定。** 2 ラウンド続けて新しいギャップがゼロ。最終の判定分布（◎6/○10/△2/✗0）。 |
| review/v2.3〜v2.6/scenarios/\*.md | 各ラウンドの構成ごとの再検討。 |
| [review/v3.0-audit-coverage.md](review/v3.0-audit-coverage.md) / [v3.0-audit-consistency.md](review/v3.0-audit-consistency.md) | 設計書 v3.0 の全面再構成の監査（決定事項 73 件の網羅性・整合性）。指摘はすべて v3.0 に反映済み。 |

## プレゼンテーション（web/）

| ファイル | 内容 |
| --- | --- |
| [web/sakutei.html](web/sakutei.html) | 設計思想を庭園メタファで表したランディングページ（単一 HTML）。 |
| [web/buildout.html](web/buildout.html) | 段0 だけがある状態から、20 台規模のポッドを 1 つの intent PR から積み上げる構築ウォークスルー。 |
| [web/scenarios.html](web/scenarios.html) | 稼働中の仮想基盤での増設・物理交換・FW 更新・ハイパーバイザーのローリング更新を追う運用シナリオ集。 |

## 読む順序のおすすめ

v3 から入る場合:

1. `core/README.md` — 要約と §1（原則）・§2（1 cycle の流れ）・§3（部品）
2. `../v3/README.md` — パッケージと CLI
3. `../v3/providers/README.md` — provider の接続設定と intent の例
4. `trial/README.md` の「v3 で試す」 — 実際に動かす

v2 系の設計を追う場合:

1. `architecture/spec/README.md` — 設計の正の索引。ここから 8 部（思想 → モデル → 収束 → 安全 → スケール → 衛星 → 運用 → 実装）をたどる
2. `architecture/old/reconciler-design-v1.md` — コアの制御ループと型を深掘りする（背景資料）
3. `examples/proxmox-mysql-dbaas-schema.md` — Provider / 合成リソースの具体像
4. `review/v2.6/README.md` — 適用可能性の到達点（最終の判定分布）と収束の宣言
5. 設計の変遷: `architecture/old/CHANGELOG-v2.3〜v2.6.md` → `architecture/CHANGELOG-v3.0.md` → `architecture/CHANGELOG-v3.*.md`
