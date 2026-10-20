# Sakutei（作庭）設計書 v3.11

**改訂日: 2026-10-20**（8 部 29 節・分冊構成）

## 本書の位置づけ

- 本書（この `spec/` ディレクトリ一式）は Sakutei の**設計の正（source of truth）**である。設計に関する記述は本書を正とし、旧版・サンプルとの矛盾は本書に寄せる。
- 本書の骨格は **v2.6 と内容（設計判断）が等価**の全面再構成（v3.0）である。v3.1 では §29.4（位置づけを OSS 個人プロジェクト・Apache-2.0 に確定）のみを更新した。v3.2 では障害駆動の人手対処（human-in-the-loop remediation）を §14.7 として新設し、§6.2（1-outstanding 制約）・§6.4（ゲート timeout 宣言）を補強した（ADR-0023）。v3.3 では段0-zero（非常駐・クライアント PC からの一回性収束）を §23.7 として新設し、§14.5（cooling-off のゲート化）・§26.6（非常駐運用の正当性）を補強した（ADR-0024。旧 §23.7 dogfooding は §23.8 へ）。v3.4 では v1b の実装に合わせ、設計判断を変えずに CLI の verb・フラグ、intent の書式（`pools:` / `scopes:` / `observe_tiers:` / `depends_on` の `ready_condition`）、採番・Sync Mode・import・prune・シャーディングの実装上の決定と既定値を書き足し、設計とずれた名前（`observe_tier`・`intent/<scope>/` のディレクトリ名）を直した。v3.5 では planned_change と専用予算レーン（§13.3）の実装に合わせ、§29.1 の reimage ペアリング判定と kind 整合検証を決めて書き足した。v3.6 では DR モード（§25.1）の実装に合わせ、DR plan の書式と効き方・書込 break-glass の署名と事後合流を書き足し、§29.1 の到達不能判定基準と合流の競合手順を決めた。v3.7 では planned_change の承認情報連携と運用プロファイル（§13.3 / §14.5）の実装に合わせ、approval の requirements による検証・プロファイルの既定・cooling-off の時計を S4 に置くことを書き足した。v3.8 では、v1 の採番をファイル版で確定し、SQLite 版を v2 以降へ移した（§19.9・§27.3・§27.5）。v3.9 では Sync Mode の git コミットの実装に合わせ、コミッター（既定 `sakutei-sync`・署名なし）・コミットの間引き・直接コミットの拒否を決めて書き足した（§24.5・§27.3・§29.1）。v3.10 では BulkObserve（§5.7）の実装に合わせ、v1 の実証対象（dnszone・nftables）と、収束の経路でのバルク経路の使い方（complete-or-error の受け方・バルクに無いリソースは `Read` で観測すること）を書き足した。v3.11 では、inventory の急減判定（§9.2）の境界を「50% 未満で halt」（ちょうど半分は受け入れる）に直し、§12.4（b）の List 急減とそろえた。v2.2〜v2.6 の改訂履歴は [`../old/`](../old/) の各 `CHANGELOG-v2.x.md` と旧版ファイルを、v2.6 → v3.0 の再構成は [`../CHANGELOG-v3.0.md`](../CHANGELOG-v3.0.md)、v3.0 → v3.1 は [`../CHANGELOG-v3.1.md`](../CHANGELOG-v3.1.md)、v3.1 → v3.2 は [`../CHANGELOG-v3.2.md`](../CHANGELOG-v3.2.md)、v3.2 → v3.3 は [`../CHANGELOG-v3.3.md`](../CHANGELOG-v3.3.md)、v3.3 → v3.4 は [`../CHANGELOG-v3.4.md`](../CHANGELOG-v3.4.md)、v3.4 → v3.5 は [`../CHANGELOG-v3.5.md`](../CHANGELOG-v3.5.md)、v3.5 → v3.6 は [`../CHANGELOG-v3.6.md`](../CHANGELOG-v3.6.md)、v3.6 → v3.7 は [`../CHANGELOG-v3.7.md`](../CHANGELOG-v3.7.md)、v3.7 → v3.8 は [`../CHANGELOG-v3.8.md`](../CHANGELOG-v3.8.md)、v3.8 → v3.9 は [`../CHANGELOG-v3.9.md`](../CHANGELOG-v3.9.md)、v3.9 → v3.10 は [`../CHANGELOG-v3.10.md`](../CHANGELOG-v3.10.md)、v3.10 → v3.11 は [`../CHANGELOG-v3.11.md`](../CHANGELOG-v3.11.md) を参照。
- §番号（§1〜§29）は全 8 ファイルを通して一意。ファイルをまたぐ参照も §番号のままで成立する。

## 改訂の規約

- 本書は**分冊のまま直接編集する**（版ごとのファイル複製はしない）。設計判断を変える改訂では `CHANGELOG-vX.Y.md` を `../` に追加し、本 README の版番号・改訂日を更新する。履歴は git が保持する。
- 大規模な再構成（メジャー改版）を行うときは、現行の `spec/` 一式を `../old/vX.Y/` にスナップショットしてから改版する。

## 目次

### [第I部 思想と境界](part1-philosophy.md)
- **§1 課題認識と位置づけ** — 何が問題か / 立ち位置 / level-triggered + 観測 state レスの定義 / 名の由来 / CLI が体現する位置づけ / 対象領域
- **§2 作るもの・委譲するもの・捨てるもの** — システム全体像 / 外部委譲 / k8s から捨てたもの / seam
- **§3 八つの原則と適用境界** — 8 原則 / 適用境界（スイートスポットと到達上限）

### [第II部 中核モデル](part2-core-model.md)
- **§4 正準型と状態分類** — ID / Scope / State / Plan / Observation / ChangeHint / Event / SecretRef / S1〜S4
- **§5 Provider 抽象** — interface 正準形 / Schema 属性 / Capabilities / Lister / エラー分類 / 階級 / BulkObserve / 観測不能階級 / Executor
- **§6 3プリミティブ** — Resource / Task / Workflow / 承認ゲート / RequiresTaskEvent
- **§7 所有と境界** — ownership 2層 / managed fields / external_owner / 境界宣言 / 合成リソースの ownership 伝播

### [第III部 収束機構](part3-convergence.md)
- **§8 収束ループ** — 周期ループ / モデルA / wave / 非同期 Apply・in-flight / backoff / 早期収束パス
- **§9 render パイプライン** — 二フェーズ / 前回解決値保持 / TTL / intent・rendered revision / 合成展開 / generated/ / ステータス集約
- **§10 差分と原因** — 型分離 / 原因タグ A〜E / per-resource 帰属 / provider_version fail-closed / last-seen / content-address
- **§11 多周期収束** — converging 三層検知 / ready_condition / oscillation / capacity gate

### [第IV部 安全機構](part4-safety.md)
- **§12 削除の規律** — 集合演算と境界宣言 / DeletionPolicy / pruneable / List 完全性 / Release 非対称 / final_backup
- **§13 変更予算** — rate limit / 累積時間窓予算 / planned_change / capacity gate
- **§14 修復の統制** — remediation mode / schedule / 優先規則と freeze / 承認ゲート / 運用プロファイル / alert-only 常用 / 人手対処（human-in-the-loop）
- **§15 隔離と防御** — blast radius 3軸 / fencing token / oscillation 隔離 / シャード=爆発半径 / デフォルト halt

### [第V部 スケールと分散](part5-scale.md)
- **§16 単一デーモンの天井** — Observe 時間律速 / 実機レイテンシ算術 / BulkObserve / tier 別頻度 / watch 併用 / 段階表
- **§17 claim シャーディング** — claim-as-intent / shared-nothing / git 配布規約 / S2-free シャード / 再シャード / 所有権ハンドオフ
- **§18 分散の原則と将来** — シャード間協調は現実を介して / 放棄するもの / v2 に残るもの / メタ収束器を作らない

### [第VI部 衛星と外部世界](part6-satellites.md)
- **§19 allocator（台帳）** — 払い出し / authority 3役 / 仮予約 / Release 非対称 / QPS 算術 / HA・プール分割 / waiting-allocation
- **§20 secret と資格** — 値は収束しない / SecretRef / Vault 断縮退 / credential-broker / 資格局所性 / Break Glass
- **§21 DesiredSource と catalog（図面）** — seam / Watch / Loader（intent の書式・YAML サブセット） / catalog
- **§22 委譲と停止影響** — 委譲一覧 / observability-as-Resource / Event と sink / 衛星停止影響表

### [第VII部 運用ライフサイクル](part7-operations.md)
- **§23 ブートストラップ** — 段0 / 設計判断 / 管理 NW 要件 / コールドスタートと SPOF / 段0-minimal / 段0-zero / devenv dogfooding
- **§24 ブラウンフィールド** — 不完全な理想×prune / 信頼獲得の順序 / import / Sync Mode / 観測不能資産 / escape hatch
- **§25 災害と復旧** — DR モード / git 外部化 / S2 喪失 / 分断時挙動と DR 判定
- **§26 検証と監査** — 検証4段 / devenv（坪庭）・twin（縮景） / 監査 S4 / 適合レポート / canary / CLI 全 verb

### [第VIII部 実装](part8-implementation.md)
- **§27 実装スライス** — 基準 / v1a / v1b / v1.5 / v2 以降 / 作らないもの / 相対規模
- **§28 Provider SDK** — SDK が採用を決める / 作者が書くもの / SDK が担うもの / Schema 設計 / golden path / 2層戦略 / 【将来】抽象リソース
- **§29 残る課題と既知の限界** — 実装中に決める事項 / 先送りと再検討トリガ / 既知の限界 / 非技術の着手条件 / 着手前決定を再オープンさせない運用 / v1b で見つけた実装上の問題

## §番号 → ファイル対応（早見）

| § | ファイル | | § | ファイル |
| --- | --- | --- | --- | --- |
| §1〜§3 | part1-philosophy.md | | §16〜§18 | part5-scale.md |
| §4〜§7 | part2-core-model.md | | §19〜§22 | part6-satellites.md |
| §8〜§11 | part3-convergence.md | | §23〜§26 | part7-operations.md |
| §12〜§15 | part4-safety.md | | §27〜§29 | part8-implementation.md |
