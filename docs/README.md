# Sakutei ドキュメント

Sakutei（作庭）の文書の索引。実装は `v3/`（モジュール `github.com/tjst-t/sakutei/v3`）と `v3/providers/`（モジュール `github.com/tjst-t/sakutei/v3/providers`）にあり、設計書は [core/README.md](core/README.md)。Stage 1〜9 がすべて main に入っている。未リリース。

以前の実装（v2 系、モジュール `github.com/tjst-t/sakutei/v2`）とその文書（v2 系の設計書 v3.77 の `architecture/`・provider ごとの説明の `providers/`・運用の文書・`web/`・`examples/`・v2 の試用の手順）は消した。最後の版はタグ `v2.12.0` で、そのタグで読める。v2 の intent は `sakutei-migrate` で書き直せる（[trial/README.md](trial/README.md) の「v2 の intent を新しいコアへ書き直す」）。

設計書の「v3.77」は v2 系の設計書のバージョン番号で、新しいコア（v3）とは別のもの。

## まず読むもの

| ドキュメント | 内容 |
| --- | --- |
| [../README.md](../README.md) | リポジトリの入口。紹介、CLI、v2 からの移行、開発コマンド。 |
| [writing-style.md](writing-style.md) | 文書の書き方。説明文は日本語、技術用語は英語のまま。使わない書き方と使う書き方の用語の表。文書・コメント・CLI のメッセージはこれに従う。 |
| [core/README.md](core/README.md) | v3 の設計書。 |

## v3（新しいコア）

| ドキュメント | 内容 |
| --- | --- |
| [core/README.md](core/README.md) | **v3 の設計の正。** 5 つの原則、やめるものと戻す目安、1 cycle の流れ、9 つの部品（Resource・Range・Provider・Condition・Graph・Policy・Procedure・Expand・Record と Events）、30 項の不変条件と守るテスト、Record・Events・read-only HTTP API・接続設定、provider SDK、v2 から移すもの・捨てるもの、作る順番（Stage 1〜9）。 |
| [../v3/README.md](../v3/README.md) | コアのパッケージの一覧、CLI と終了コード、Stage ごとの決まり。 |
| [../v3/providers/README.md](../v3/providers/README.md) | コアの外のモジュール（provider・Expand・secret backend・sink・承認ツール・`sakutei-migrate`・`sakutei-import`）。provider ごとの接続設定と intent の例、統合テスト（`make integration-v3-*`）の回し方と結果。 |
| [trial/README.md](trial/README.md) の「v3 で試す」 | `trial/v3/run.sh` で DNS と FW の試用の手順を v3 で流す。v2 の intent を `sakutei-migrate` で v3 に書き直す手順。 |
| [review/core-reduction-draft.md](review/core-reduction-draft.md) | v3 のもとにした下書き。v2.12.0 を見直した結果とコアの案。 |
| [review/core-usecase-review.md](review/core-usecase-review.md) | v3 のもとにした検討。303 件のユースケースで部品の案を確かめたもの。設計書の `E-…`・`N-…` の ID はこの文書の ID。 |

## 試用の手順（trial/）

| ドキュメント | 内容 |
| --- | --- |
| [trial/README.md](trial/README.md) | 「v3 で試す」（`trial/v3/run.sh` で DNS と FW の試用の手順を流す）と、v2 の intent を `sakutei-migrate` で書き直す手順。 |
| trial/v3/ | v3 の試用の intent・最初の現実・接続設定と `run.sh`。`providers/cmd/sakutei` の `TestTrial` も同じものを使う。 |

v2 の各機能の試用の手順（allocation・dr-mode・observe・planned-change・redfish-os-install など）は消した。その intent と最初の現実は、`sakutei-migrate` のテストの入力として `v3/providers/migrate/testdata/trial/` に移してある。

## レビューと適用可能性の検討（review/、v2 系）

v2 系の設計書を対象にした検討。本文が引く v2 の文書・コード（`architecture/`・`providers/`・`examples/`・`pkg/` など）は消したので、タグ `v2.12.0` で読む。v3 のもとにした検討（`core-reduction-draft.md`・`core-usecase-review.md`）は上の「v3（新しいコア）」の表にある。

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

## 読む順序のおすすめ

1. `core/README.md` — 要約と §1（原則）・§2（1 cycle の流れ）・§3（部品）
2. `../v3/README.md` — パッケージと CLI
3. `../v3/providers/README.md` — provider の接続設定と intent の例
4. `trial/README.md` の「v3 で試す」 — 実際に動かす
