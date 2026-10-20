# Sakutei 設計書 v3.4 変更一覧（CHANGELOG）

対象: v3.3 → v3.4。改訂日 2026-10-20。骨格（§1〜§29 の機構）と設計判断（原則・機構）は不変。本改訂は **v1b の実装（git の 9d7cf32..be9fb52）に設計書を追いつかせる**小改訂である。実装で入った CLI の verb・フラグ、intent の書式、既定値、実装上の決定を該当節に「v1b の実装」として書き足し、設計とずれていた名前を直した。新設の節は §29.6 のみ。

形式: **変更ID / 対応 / 章 / 変更内容 / 設計判断の理由**。対応の `#nn` は Backlog の項目番号。

---

## CHG-01 — CLI の verb とフラグを実装に合わせる

- **対応**: #22・#23・#24・#25・#26・#28・#29・#31
- **章**: §1.5・§26.6
- **変更内容**: verb に `sakutei sync`・`sakutei allocator`・`sakutei fence`・`sakutei mirror`・`sakutei secrets extract` を足した。`import` の書式を `--provider <名前> --out <dir> [--scope]` に直した。§26.6 に verb ごとのフラグと既定値の表を足した（常駐ループの `--alloc-store` / `--alloc-url`・`--claim`・`--fencing-store` / `--shard-id`・`--capacity-gate`・`--inventory-*`・`--intent-repo` / `--workdir` / `--shard` / `--git-branch` / `--git-poll` / `--s2-free` / `--secret-store`、`plan` / `reconcile` / `status` の採番フラグ、`import` の `--samples` / `--sample-interval` / `--force` ほか）。CLI での scope-selector は `site=tokyo,cluster=prod` と書くことを明記した。`plan` / `status` は採番を払い出さない（Lookup のみ）ことを書いた。`--expect-revision` は v1b 時点で未実装であると注記した。`--claim` の注記を「実装は必要時」から「v1b で実装」に直した。
- **理由**: 実装された verb とフラグが設計書の正典箇所（§26.6）に無かった。「`apply` を主動詞に置かない」方針は変えていない。補助 verb はどれも収束の起床トリガではなく、衛星（allocator）と運用（fencing 世代の確認・git ミラー・資格の抜き出し）のためのものである。

## CHG-02 — intent の書式の一覧と YAML サブセットの明記

- **対応**: #23・#24・#26・#30
- **章**: §21.3
- **変更内容**: トップレベルのキー（`resources` / `pools` / `scopes` / `observe_tiers` / `version`）と resource のキー（`provider` / `id` / `scope` / `fields` / `depends_on` / `deletion_policy` / `observe_tier`）の表を足した。未知のキーはロード時に拒否すること、Local ID の重複と `depends_on` の循環をロード時に拒否することを書いた。stdlib のみの最小 YAML サブセットが受け付ける構文と、明確なエラーで拒否する構文（フロースタイル・アンカー・タグ・複数ドキュメントほか）を書き、本書のフロースタイルの YAML 例は略記であって v1b の intent ではブロックで書くことを注記した。
- **理由**: 書式が各節とコードに散っていた。本書の例（`{ name: ..., range: ... }` など）をそのまま書くとロードで落ちるため、そのずれを正典箇所で解消する。

## CHG-03 — `depends_on` の `{ id, ready_condition }`

- **対応**: #26
- **章**: §8.3・§11.2
- **変更内容**: YAML 例から `type:` のキーを外した（実装の書式は `id` と `ready_condition` だけで、上流は intent 全体で一意な Local ID で特定する）。要素は文字列か `{ id, ready_condition }` で、混在してよいことを書いた。v1b の挙動（今回の観測で `Ready[condition]` が真になるまで下流を skip・真になった周期で解放・常駐ループ / `reconcile` / scoped 経路のすべてで効く・desired 外の上流も観測して gate に使う）を書いた。
- **理由**: 設計の例の `type:` は実装のスキーマでは未知のキーとして拒否される。機構（§8.2 の下流 skip の再利用）は変えていない。

## CHG-04 — Observe 頻度のキーを `observe_tier` / `observe_tiers` とする

- **対応**: #30
- **章**: §16.5・§29.1
- **変更内容**: intent の書式（トップレベルの `observe_tiers:` に `name` / `observe_every`、resource の `observe_tier`）を書いた。**キーを `tier:` にしない命名の決定**（`tier:` は §8 の DAG 優先度帯 `tier: foundation` の名前であり、Observe 頻度とは独立）を明記した。ロード時の検証、間引かない周（初回・未収束・desired の変化）、間引いた周の扱い（Observe も Diff もしない・unobservable にしない・prune 候補にならない・記録は S3）、合成リソースの子が親の tier を継ぐこと、Ansible executor の既定が当面「毎周」であることを書いた。「v1b は seam まで」を「seam と tier 別の間引きまで」に直した。
- **理由**: 実装の初版は `tier:` を Observe 頻度に使っており、設計どおり `tier: foundation` と書くと「未宣言の tier」で落ちた（レビューで修正）。二つの設定が同じ名前を取り合わないことを設計書に残す。

## CHG-05 — 採番（AllocationAuthority）の v1b の実装上の決定

- **対応**: #24・#36
- **章**: §19.1・§19.3・§19.7・§19.9・§27.3
- **変更内容**: 要求の書式（`fields` の値に `{ from_pool: <プール> }`・宣言されていないプールはロード時に拒否）を書いた。5 操作は設計どおり。owner を `"<provider>/<local id>#<field>"` とし scope を含めないこと（§4.2 の理由）、`reconcile` と常駐ループが `Allocate`・`plan` と `status` は `Lookup` のみ、割当の解決は render フェーズ1 で行い rendered_revision の外部入力に入ること（cause E）、プールの宣言と範囲変更の拒否、cooldown の既定 24h、Release を呼ぶ経路はまだ無い（§19.5 の List 確認が要るため保守側）ことを書いた。**v1b はファイル（JSON）+ flock の `FileAuthority` で最小実装し、SQLite 版は seam の裏で後から差し替える**（#36）という決定と理由（リポジトリを stdlib のみに保つ）を §19.9 に書いた。§27.3 の「render フェーズ2 の採番連携」を「解決はフェーズ1、子への伝播はフェーズ2」に直した。
- **理由**: §19.9 は SQLite + `BEGIN IMMEDIATE` を最小実装として挙げていたが、実装は外部依存を避けてファイル版にした（2026-10-20 の決定）。同一プールへの並行 `Allocate` を直列化するという要件は同じロックの規律で満たしており、seam（§19.9 の規律（1））は守っている。

## CHG-06 — allocator の HA 最小形・Lookup キャッシュ・waiting-allocation・プール分割の具体

- **対応**: #25
- **章**: §19.6・§19.8・§19.9
- **変更内容**: `sakutei allocator`（active/standby の HTTP サービス。`--standby` なしは起動時に昇格して fencing 世代を上げる・standby は `POST /v1/promote` まで 503・fence された active は standby に降りる・API の一覧・`--alloc-url a,b` のフェイルオーバー・`--alloc-store` との排他）を書いた。常駐ループの Lookup キャッシュ（期限なし・自分の Release でだけ無効化・Reserve はキャッシュしない・S3）を書いた。`pools:` の書式（`name` / `range` / `claim` / `space`、ブロックで書く）とロード時の検証（同じ `space` で claim の異なるプールの重なりを拒否・claim のないプール同士は重なってよい・claim 付きのプールへの要求は claim の中からだけ）を書き、§19.8 の例をブロック書きに直した。waiting-allocation の v1b の挙動（そのリソースと下流だけ skip・prune 候補にしない・全 verb で縮退）と chaos テストで確かめた内容を書いた。
- **理由**: 設計の機構（fencing の再利用・1 プール 1 権威・維持は衛星ゼロ）をどう実装したかが書かれていなかった。`space` は実装で足した欄で、「別のものを数えるプールが同じ番号を持つ」ことを許しつつ claim 分割の disjoint を検証するために要る。

## CHG-07 — Sync Mode は `generated/` への書き出しまで

- **対応**: #23・#35
- **章**: §24.5
- **変更内容**: `scopes:` の書式の例を v1b の形に直し、ロード時の検証（空の scope 不可・二重宣言不可・sync は `target_path` 必須で `generated/` 配下・sync の scope に intent のリソース不可）を書いた。`sakutei sync` の動作（`<target_path>/sync-<provider>.yaml`・自動生成ヘッダと本文の sha256・手編集の上書き拒否・変化時のみ書く・List 不完全なら前を保持・現実に書かない）を書いた。**git コミットは後回し**（#35）と明記した。sync の scope の現実は prune の集合差の前に落とすことを書いた。
- **理由**: 設計の動作の手順 3（commit）は v1b では実装していない（2026-10-20 の決定）。sha256 による手編集の検知は、§9.6 の「直接コミットを拒否する」規律を、コミットを作る前の段階でファイルに対して効かせる形である。

## CHG-08 — import の安全側の既定

- **対応**: #22
- **章**: §24.4
- **変更内容**: 出力（`<out>/imported-<provider>.yaml`・決定的・`--force` なしで上書きしない）、全リソース `ExplicitAbsentOnly`、List が `ListComplete` でなければ中止、観測不能は書かない、secret 平文は書かない（`SecretRef` の形のみ）、Computed / Managed は書かない、managed 候補はフィールド直前のコメント（`--samples` 既定 3・`--sample-interval` 既定 2s）を書いた。コマンドの書式を直した。
- **理由**: §24.1 の「不完全な理想 × prune」の地雷を import の段で踏まないための既定値が、実装にはあって設計書に無かった。

## CHG-09 — prune の閉世界は宣言した scope の中だけ

- **対応**: #38・#23・#28
- **章**: §12.2
- **変更内容**: 集合差の候補を、`PruneWithinScope` を宣言したリソースの scope に合う現実だけに絞ることを明記した。v1a の実装は簡略化のため provider 全体を対象にしており、別の scope の現実まで消していたこと、v1b でそれを本節の定義どおりに直したことを書いた。`--claim` 外と `mode: sync` の scope は別途、集合差の前に落とすことを書いた。
- **理由**: 設計（§12.1「closed-world の集合演算は scope 内に閉じる」・§4.2）は元から scope 単位だった。実装のずれを直したことを記録し、§24.2 の「scope ごとの昇格」が prune の側でも成り立つことを確かめられるようにする。

## CHG-10 — inventory 検証（incomplete≠empty）の具体

- **対応**: #31
- **章**: §9.2
- **変更内容**: `InventorySource` seam と v1b の `FileInventorySource`（`--inventory-file`）、判定（不達で前回あり → 保持・前回なし → halt、`--inventory-min-ratio` 既定 0.5 で**ちょうど半分も incomplete**（§12.4（b）との境界の違いも注記）・`--inventory-expected-min`・halt 中は基準を動かさない・漸減は受け入れる）、`InventoryIncomplete` Event、基準は S3、inventory の内容ハッシュを rendered_revision に入れることを書いた。
- **理由**: §9.2 は「前回比 X% 未満で halt」とだけ書いており、境界・基準の扱い・初回の扱いが未定義だった。実装は「既定 50% 以下で halt」（ちょうど半分を含む）で、§12.4（b）の List 急減の実装（50% を超える減少で halt）とは境界が 1 点ずれていることもあわせて注記した（実装のコメントは「同じ規則」と書いているが、実際は異なる）。どちらにそろえるかは決めていない。

## CHG-11 — capacity gate の v1b の宣言形

- **対応**: #26
- **章**: §13.4
- **変更内容**: v1b では capacity gate を intent の `safety:` ではなく `--capacity-gate` フラグ（`resource` / `used` / `total` / `max_utilization` / `on_exceed` / `provider`）で宣言することを書いた。閾値は「超えたら」超過、観測できないプールは超過扱い、`on_exceed` の既定は `halt-all`、判定は変更予算と合成して同じ経路で Apply / prune を絞ることを書いた。
- **理由**: 宣言の置き場が設計（intent の `safety:`）と実装（CLI）で違うことを明示する。観測不能を超過とみなすのは §3 の非対称原則の適用であり、新しい判断ではない。intent での宣言は後で足せる。

## CHG-12 — 静的シャーディングと fencing の具体

- **対応**: #28
- **章**: §15.2・§17.1・§17.6・§17.8
- **変更内容**: 常駐ループの `--claim` で claim 外の scope を Observe しない・prune の境界も同じ claim で絞ることを書いた。fencing の v1b の実装（`--fencing-store` の JSON ファイル + ロックファイル・初めて収束するときに世代を上げる・Apply / Delete 直前の検証・`FENCED` の scope は Observe / Apply / prune しない・再起動まで取り返さない・ストアに届かない周は飛ばす・`--shard-id` の既定・`sakutei fence`）を書いた。再シャードの v1b の手動手順（drain → claim 変更 PR → fencing 世代の更新 → 旧シャードの再起動）を書き、Workflow 化は v1.5 とした。
- **理由**: §17.6 は再シャードを Workflow と定めるが、Workflow は v1.5 である（§27.4）。それまでの手順を設計書に置き、手順 3 の「世代の更新」が実装ではどの操作にあたるかを明確にする。

## CHG-13 — claim の git 配布・S2-free・資格局所性の具体と、`intent/<scope>/` のディレクトリ名

- **対応**: #29
- **章**: §17.4・§17.5・§17.7・§20.5
- **変更内容**: §17.4 のレイアウト例の `intent/site-a/` を、実装と §4.2 の `Scope.Path()` の規約どおり `intent/site=site-a/`（`ラベル=値` をラベル名順に `,` でつなぐ）に直し、`shards/<名前>.yaml` の書式（`claim:` のみ・空なら idle・未宣言はエラー）を足した。`--intent-repo` / `--workdir` / `--shard` / `--git-poll` の挙動（partial clone + cone モード sparse checkout・jitter・ミラー断中は前回の intent で収束・一度も pull できていないか自 claim のディレクトリが無ければ周を飛ばす）と `sakutei mirror` を書いた。`--s2-free`（`--intent-repo` 必須・`--alloc-store` 拒否・手元は `--workdir` だけ）を書いた。claim 変更 PR のマージがそのままハンドオフになる v1b の挙動（in-flight を待って手を引く・新担当が世代を上げる・手元の世代を捨てて収束し直す）を書いた。`--secret-store` の資格局所性の検査（claim の接頭辞の外の資格があれば収束しない）と `sakutei secrets extract` を書いた。
- **理由**: 設計の例のディレクトリ名（`site-a`）は claim ↔ パスの機械対応（§4.2）を満たさない略記だった。実装は §4.2 の規約に従っており、設計書をそちらに寄せた。

## CHG-14 — SDK のテストハーネスと golden path 雛形

- **対応**: #21・#32・#33
- **章**: §5.7・§28.3・§28.6
- **変更内容**: `ContractTester` の Schema からの sample 生成（`Examples` / `MakeID` / `DefaultValueForKind` / `GenerateSample`）、`MakeUnreachable` による三値の検査、最小階級の欠落フィールドの許容を書いた。**Schema（`FieldSpec`）は契約テストのために変えない**という決定（例の値はテスト側の `Examples` に置く）を書いた。`BulkEquivalenceTester` の検査項目を書いた。`templates/provider/` の `full/`・`minimal/` の雛形と、付属のテスト・受け入れテストでの検証を書いた。
- **理由**: §28.3 / §28.6 は「契約テスト自動生成」「golden path テンプレート」を機能名で挙げるだけだった。Schema を変えない決定は、Schema が provider の公開 API（§29.4 で第一級）であり、テストの都合で属性を増やすと全 provider に波及するためである（§29.5 の「再オープンさせない」規律とも整合する）。

## CHG-15 — intent_revision と rendered_revision の v1b の具体

- **対応**: #34・#24・#31
- **章**: §9.4
- **変更内容**: FileSource の intent_revision が `.json` / `.yaml` / `.yml`（loader と同じ集合）の内容ハッシュであること、v1a では `.json` だけを数えて YAML の編集を cause B に誤分類していたのを直したこと、git 配布では再帰的に数えることを書いた。rendered_revision の外部入力が v1b では採番の割当と inventory のハッシュであることを書いた。
- **理由**: cause A と B の区別（§10.2）は intent_revision の正しさに依存する。何を数えるかを設計書に置き、同じずれを繰り返さないようにする。

## CHG-16 — v1b の実装状況と、見つけた実装上の問題（§29.6 新設）

- **対応**: #18（v1b）・#27・#35・#36・#37
- **章**: §27.3・§29.1・§29.6（新設）
- **変更内容**: §27.3 に 2026-10-20 時点の実装状況の表を足した（残り: #27 BulkObserve の実バルク実装〈実バルク API を持つ対象の provider が要る〉・#35 Sync Mode の git コミット・#36 SQLite 版 allocator）。§29.6 を新設し、dnszone が読めない行を読み飛ばす問題（#37）と `reconcile --expect-revision` の未実装を載せた。§29.1 の Ansible executor の行に当面の既定（毎周）を足した。
- **理由**: 「設計の正」に、どこまで実装済みかの目印が無かった。§29.6 の 2 件はどちらも設計の変更を要しない実装上の問題であり、§29.1〜§29.3 の分類（決める事項・先送り・設計で解けない限界）のどれにも当たらないため別の節にした。

---

## 見送り

- **intent に `safety.capacity_gate` を実装に合わせて消す**: 設計の宣言形（intent の `safety:`）は、PR レビューと監査の対象になるという点で CLI フラグより望ましい。v1b の CLI フラグは実装の現状として書き足すに留め、設計の宣言形は残した。
- **§19.9 の SQLite を「ファイル版」に書き換える**: SQLite 版は Backlog #36 として残っており、設計の最小実装の候補から外す決定はしていない。ファイル版は v1b の実装上の決定として併記した。
- **`--expect-revision` を設計から外す**: plan と reconcile の同一性の担保（§26.6）は承認ゲートの思想（§14）と対の設計判断であり、未実装であることは設計を変える理由にならない。未実装として注記するに留めた。Backlog には未登録である。
- **本書のフロースタイルの YAML 例をすべてブロック書きに直す**: 説明のための略記として読みやすさを優先し、§21.3 にその旨を注記した。実装の書式を示す箇所（§16.5・§19.1・§19.8・§24.5）だけをブロック書きにした。
- **各分冊の冒頭行の版表記（「設計書 v3.0 の一部」）の更新**: v3.1〜v3.3 でも更新しておらず、版の正は `spec/README.md` である。
