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
| [providers/redfish.md](providers/redfish.md) | Redfish の BMC provider（`redfish-system`・`redfish-virtual-media`）。BMC の一覧（`--redfish-config`、パスワードは SecretRef）と intent の例。qemu-bmc + containerlab の統合テスト（`make integration-redfish`）の回し方と結果。 |
| [providers/sqlmigrate.md](providers/sqlmigrate.md) | SQLite・PostgreSQL のマイグレーション provider（`sqlmigrate`、単純 Task 専用）。データベースの一覧（`--sqlmigrate-config`）、マイグレーションのファイル名の決まり、intent の例（`strategy: task`・`window`・`rollback_on_fail`）、plan・reconcile・常駐での動き方、PostgreSQL（`engine: postgresql`・advisory lock・トランザクションに入れられない文・統合テスト）。 |
| [providers/nftables.md](providers/nftables.md) | nftables provider（`nftables`）。対象 `file`（管理された ruleset ファイル）と `live`（カーネルの実際の ruleset を `nft` で読み・流す）の違い、設定（`--ruleset-target`・`--ruleset-tables`・`--ruleset-nft`・`--provider-config`）、権限（CAP_NET_ADMIN）、ルールの正規形。使い捨ての network namespace での統合テスト（`make integration-nftables`）の回し方と結果。 |
| [providers/dnszone.md](providers/dnszone.md) | dnszone provider（`dnszone`）。ID とフィールド、管理の境界（SOA・NS）と SOA シリアル、設定（`--zone`・`--zone-soa-serial`・`--zone-scope`・`zone_reload`）、離れたホストのゾーンファイル（`zone_hosts`）。 |
| [providers/ansible.md](providers/ansible.md) | Ansible provider（`ansible`、最小の階級）。設定（`--ansible-config`：inventory・ansible のパス・env・timeout・max_parallel）、リソース（host・module・args・destructive）と SecretRef、check mode での観測と落ちる保証（差分・原因タグ・converging・破壊的の重み・prune）、observe_tiers での間引き。本物の ansible-core 2.21.5 での統合テスト（`make integration-ansible`）の回し方と結果。 |
| [providers/s3-bucket.md](providers/s3-bucket.md) | S3 互換のバケット provider（`s3-bucket`）。endpoint の一覧（`--s3-config`、資格は SecretRef、`bucket_prefix`）、フィールド（versioning・policy・lifecycle・tags）と正規形、中身のあるバケットを消さない決まり、API を持たない endpoint の扱い。docker の SeaweedFS（必須）・MinIO（任意）での統合テスト（`make integration-s3`）の回し方と結果。 |
| [providers/postgresql.md](providers/postgresql.md) | PostgreSQL provider（`postgresql`）。サーバの一覧（`--postgresql-config`、管理用のパスワードは SecretRef）、ロール・データベース・権限・拡張の ID とフィールド、パスワードの扱い（SCRAM-SHA-256・`verify_passwords`）、管理の境界、利用者の表があるデータベースを消さない決まり。docker の公式の postgres イメージでの統合テスト（`make integration-postgresql`）の回し方と結果。 |
| [../examples/external-provider/README.md](../examples/external-provider/README.md) | 外の provider を足す（§28.9）。Sakutei を Go のライブラリとして使い、`pkg/cli` の入口に自前の provider を登録した自前のバイナリを作る例と、provider ごとの設定ファイル（`--provider-config`）。 |
| [providers/guest-os.md](providers/guest-os.md) | ゲスト OS を観測するだけの provider（`guest-os`、Backlog #86）。ssh 越しに届くか・`/etc/os-release`・導入の目印（`guest_markers`）を読み、OS 導入の Workflow が「電源が切れた」でなく「ゲスト OS が目印つきで起動した」で完了を判断する書き方。qemu-bmc 3 台で本当に Alpine を入れる統合テスト（`make integration-guestos`）の回し方と結果。 |
| [remote.md](remote.md) | 離れたホストを扱う口（ssh 越しの実行の層、Backlog #130）。対象の一覧（`remote_config`：address・user・秘密鍵の SecretRef・known_hosts・sudo・タイムアウト）、ID にホストの名前が付くこと、鍵とホスト鍵の扱い、接続の使い回し、権限、届かないときの答え、dnszone の再読み込み（`zone_reload`）。docker の sshd 2 台での統合テスト（`make integration-remote`）の回し方と結果。 |
| [status-json.md](status-json.md) | `sakutei status --json` の出力の形（schema_version 1）。要約・リソースごとの状態・宣言外の検出ができなかった provider。 |
| [metrics.md](metrics.md) | デーモンの `/metrics`（Prometheus の text format、Backlog #54・§27.5）。`--metrics-listen` で明示したときだけ待ち受ける。出す値（drifted・unknown の件数、予算の残り、halt の有無、cycle と Observe の所要時間）と意味。 |
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
