# Sakutei ドキュメント

Sakutei（作庭）の設計ドキュメント一覧。現時点ではコードは未実装で、本リポジトリは**設計仕様のみ**を保持する。

## 設計仕様（design/）

| ドキュメント | 位置づけ | 状態 |
| --- | --- | --- |
| [design/sakutei-design-v2.6.md](design/sakutei-design-v2.6.md) | **設計の正（source of truth）。** 汎用収束エンジン全体の仕様。決定済みの設計のみを記録する。v3.0 全面再構成前の最終パッチラウンド。 | v2.6（最新・権威） |
| [design/CHANGELOG-v2.6.md](design/CHANGELOG-v2.6.md) | v2.5 → v2.6 の変更一覧（計画変更宣言・承認ゲート N-of-M・DR モード・運用プロファイル・§26 カテゴリ A の最終消し込み）。 | — |
| [design/sakutei-design-v2.5.md](design/sakutei-design-v2.5.md) | 旧版（歴史として保持、編集しない）。 | v2.5（旧版） |
| [design/CHANGELOG-v2.5.md](design/CHANGELOG-v2.5.md) | v2.4 → v2.5 の変更一覧（衛星の天井・BulkObserve 一貫性契約・claim の git 配布と S2-free シャード）。 | — |
| [design/sakutei-design-v2.4.md](design/sakutei-design-v2.4.md) | 旧版（歴史として保持、編集しない）。 | v2.4（旧版） |
| [design/CHANGELOG-v2.4.md](design/CHANGELOG-v2.4.md) | v2.3 → v2.4 の変更一覧（早期収束パス・BulkObserve・claim-as-intent）。 | — |
| [design/sakutei-design-v2.3.md](design/sakutei-design-v2.3.md) | 旧版（歴史として保持、編集しない）。 | v2.3（旧版） |
| [design/CHANGELOG-v2.3.md](design/CHANGELOG-v2.3.md) | v2.2 → v2.3 の変更一覧（20 件、対応レビュー指摘・見送り理由つき）。 | — |
| [design/sakutei-design-v2.2.md](design/sakutei-design-v2.2.md) | 旧版（歴史として保持、編集しない）。 | v2.2（旧版） |
| [design/reconciler-design-v1.md](design/reconciler-design-v1.md) | 収束エンジンコア（reconciler）の初期設計メモ。コアの制御ループ・型・実装判断がより詳細。矛盾する場合は **v2.6 が優先**。 | v1（背景資料） |

## 実装例（examples/）

| ドキュメント | 内容 |
| --- | --- |
| [examples/proxmox-mysql-dbaas-schema.md](examples/proxmox-mysql-dbaas-schema.md) | v2.2 の §5（Provider 実装ガイド）と §7.5（合成リソース）の worked example。`proxmox_vm` を基底 Provider として定義し、その上に MySQL DBaaS を合成リソースとして構築する具体例。規範仕様ではなく解説用。 |

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

## プレゼンテーション（web/）

| ファイル | 内容 |
| --- | --- |
| [web/sakutei.html](web/sakutei.html) | 設計思想を庭園メタファで表現したビジュアルなランディングページ（単一 HTML）。 |

## 読む順序の推奨

1. `design/sakutei-design-v2.6.md` — 全体像・原則・v1a/v1b/v1.5 スコープを把握
2. `design/CHANGELOG-v2.6.md` — 直近の設計変更とその理由
3. `design/reconciler-design-v1.md` — コアの制御ループと型を深掘り
4. `examples/proxmox-mysql-dbaas-schema.md` — Provider / 合成リソースの具体像
5. `review/v2.5/README.md` — 現設計の適用可能性の到達点と残存課題（対象は v2.5。v2.6 は残存既知課題の消し込みラウンド）
