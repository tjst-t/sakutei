# Sakutei ドキュメント

Sakutei（作庭）の設計ドキュメント一覧。現時点ではコードは未実装で、本リポジトリは**設計仕様のみ**を保持する。

## 設計仕様（design/）

| ドキュメント | 位置づけ | 状態 |
| --- | --- | --- |
| [design/sakutei-design-v2.2.md](design/sakutei-design-v2.2.md) | **設計の正（source of truth）。** 汎用収束エンジン全体の仕様。決定済みの設計のみを記録する。 | v2.2（最新・権威） |
| [design/reconciler-design-v1.md](design/reconciler-design-v1.md) | 収束エンジンコア（reconciler）の初期設計メモ。コアの制御ループ・型・実装判断がより詳細。v2.2 と矛盾する場合は **v2.2 が優先**。 | v1（背景資料） |

## 実装例（examples/）

| ドキュメント | 内容 |
| --- | --- |
| [examples/proxmox-mysql-dbaas-schema.md](examples/proxmox-mysql-dbaas-schema.md) | v2.2 の §5（Provider 実装ガイド）と §7.5（合成リソース）の worked example。`proxmox_vm` を基底 Provider として定義し、その上に MySQL DBaaS を合成リソースとして構築する具体例。規範仕様ではなく解説用。 |

## レビュー・適用可能性検討（review/）

| ドキュメント | 内容 |
| --- | --- |
| [review/README.md](review/README.md) | **総括レポート。** 設計レビュー（3 観点）と 20 構成での適用可能性検討の判定一覧・横断的発見・設計へのフィードバック。 |
| review/01〜03-\*-review.md | 設計レビュー各論（整合性・安全性・実装可能性）。 |
| review/scenarios/01〜20-\*.md | 20 構成（ホームラボ〜大規模 DC〜マルチサイト〜IaaS/SDN/PaaS/SaaS 等）それぞれの詳細検討。 |

## プレゼンテーション（web/）

| ファイル | 内容 |
| --- | --- |
| [web/sakutei.html](web/sakutei.html) | 設計思想を庭園メタファで表現したビジュアルなランディングページ（単一 HTML）。 |

## 読む順序の推奨

1. `design/sakutei-design-v2.2.md` — 全体像・原則・v1 スコープを把握
2. `design/reconciler-design-v1.md` — コアの制御ループと型を深掘り
3. `examples/proxmox-mysql-dbaas-schema.md` — Provider / 合成リソースの具体像
