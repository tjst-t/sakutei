# Sakutei（作庭）

**オンプレミス基盤全体を対象とした、宣言的インフラ収束エンジン（コントロールプレーン）。**

電源/BMC → ネットワーク → FW/SDN → ストレージ → ハイパーバイザ → VM → ゲスト OS → コンテナ までを、ひとつの「あるべき姿（intent）」から level-triggered に収束させます。位置づけは **「Ansible の冪等性 + Kubernetes の収束ループ」**。

Kubernetes からは *収束原理*（level-triggered な収束ループ）だけを借り、実装機構（apiserver・etcd・scheduler・admission・CRD）は意図的に捨てています。最大の特徴は **観測 state レス** — キャッシュした「現在状態」を持たず、毎周期リアルを再観測します。これにより Terraform の three-way-diff / state ドリフト問題を**構造的に**排除します。

名称は平安期の作庭書『作庭記』に由来します。

- **ライセンス:** Apache-2.0
- **言語:** Go（1.22・標準ライブラリのみ、外部依存なし）
- **状態:** 設計 v3.1（収束済み）／ 実装 **v1a 完成**（コア実証）

---

## なぜ観測 state レスなのか

宣言的ツールの多くは「前回自分が書いた状態」をキャッシュし、それと現実・意図の3者を突き合わせます（three-way diff）。このキャッシュが現実とズレると、ドリフト検知が壊れ、`terraform refresh` のような救済が要ります。

Sakutei は**キャッシュを持ちません**。毎周期、

```
desired（intent を render）  ⇄  actual（provider が現実を Read）  →  diff  →  必要なら Apply
```

を回すだけです。「前回の状態」が無いのでズレようがなく、**触っていないのに戻る**（自己修復）が最小の機構で成立します。

---

## クイックスタート

```console
$ make build      # 単一バイナリ ./sakutei をビルド
$ make check      # build + vet + test（CI 相当）
$ make demo       # レビュー用の再現デモ（下記）
```

### `make demo` — GUI を持たない収束エンジンの「レビュー面」

GUI が無くても、`plan` / `status` の端末出力が**そのままレビュー面**です。`make demo` は実 provider（dnszone）を一時ディレクトリで動かし、次の物語を再生します:

1. `plan` — 宣言した2レコードが「これから作られる」ことを dry-run で見る
2. `reconcile` — 実体ファイル `zone.db` に収束
3. `status` — 適合レポート（`drifted=0`）
4. **誰かが `zone.db` を手で改ざん**（値を書き換え＋宣言外レコードを追加）
5. `status` — 改ざんを **cause B（ドリフト）** で検知、宣言外を **unmanaged** で提示
6. `reconcile` — **触っていないのに元へ戻る**（自己修復）

抜粋（ステップ5）:

```
════════ sakutei status — ドリフト検知（触っていないのに気づく）════════

drifted: 1 リソース

  DRIFTED dnszone//www.example.com./A
    原因タグ: B（ドリフト）
    provider 階級: TrueObserve=true List=true BulkObserve=false Pruneable=true ...
    理由: resource dnszone//www.example.com./A has 1 field(s) out of sync

unmanaged（宣言外・実在・prune 対象外）: 1 リソース
  注記: これらは現実に在るが intent に宣言が無い。既定（ExplicitAbsentOnly）では
  自動削除されない（prune 対象外・read-only）。削除するなら intent 側で明示する。
  UNMANAGED dnszone//rogue.example.com./A

要約: drifted=1 converged=1 unobservable=0 unmanaged=1
```

改ざんは `plan`/`reconcile`/`status` すべてで cause B に一致し、宣言外レコードは **prune されず残ります**（原則「確信が持てないとき、加算はしても減算はしない」）。

---

## CLI

```
sakutei              # 常駐して回り続ける（本来の姿）      --claim <scope-selector>
sakutei plan         # dry-run: 何が変わるか（Diff で止まる）
sakutei reconcile    # 1 回収束（手動キック）              --expect-revision <rev>
sakutei status       # 適合レポート（drifted / unmanaged 一覧、read-only）
sakutei import       # 既存リアルを intent として書き出す（設計 verb・v1b で実装）
```

`apply` は意図的に主 verb にしていません（「いつ変えるか」は収束が決める）。`plan` と `status` を前面に置くこと自体が、このソフトのレビュー体験です。

---

## 実装済みの Provider

Provider は `Schema` / `Capabilities` / `Read` / `Apply` ＋ 集合列挙 `List`（prune の前提）を実装します。SDK が diff・secret マスキング・managed 除外・version 埋込・エラー分類・契約テスト・Schema lint を肩代わりするので、作者は数関数を書くだけです。

| 名前 | 種別 | 説明 |
| --- | --- | --- |
| `dnszone` | 実 | ゾーンファイルを宣言的現実として管理（RFC1035 サブセット、原子的書込、SOA/NS は管理外） |
| `nftables` | 実 | nftables ルールセットファイルを管理。**ordered ルール**（並べ替え＝変更）を実証 |
| `fake` | テスト | fault-injection 可能な検証用 provider（chaos テストの主戦場） |

---

## リポジトリ構成

```
.
├── cmd/sakutei/            # CLI エントリ（plan / reconcile / status / daemon）
├── pkg/                    # 公開 API
│   ├── core/               #   正準型（ID / Scope / State / Plan / Observation / SecretRef ...）
│   ├── provider/           #   Provider / Lister / BulkObserver / Capabilities / Schema
│   ├── sdk/                #   Provider 作者向け SDK（lint / 契約テスト / エラー）
│   └── registry/           #   provider 名 → ファクトリ
├── internal/               # 収束ループ・diff・cause・prune・budget・render・loader・secret ...
├── tests/acceptance/       # 受入テスト（[AC-...] タグ付き）・chaos スイート
├── docs/                   # 設計の正（下記）
├── Makefile                # build / test / vet / check / demo / clean
└── LICENSE                 # Apache-2.0
```

---

## 設計ドキュメント（source of truth）

コードは設計仕様の**射影**です。設計に関する記述は `docs/` を正とします。

- **[docs/README.md](docs/README.md)** — ドキュメント索引（まずここ）
- **[docs/architecture/spec/](docs/architecture/spec/)** — 設計の正 v3.1（8 部 29 節・分冊）。§番号は全体で一意
- **[docs/web/sakutei.html](docs/web/sakutei.html)** — 設計思想を庭園メタファで表したランディングページ
- **[docs/web/scenarios.html](docs/web/scenarios.html)** — 仮想基盤を題材に、構築/増設/物理交換/FW更新/HVローリング更新が実際どう実行されるかを追う運用シナリオ集

### 設計を貫く八つの原則（§3）

1. **level-triggered + 観測 state レス** — 毎周期 desired と actual を再導出し、観測状態を保持しない
2. **いつ = 収束 / どう = 手続き** — 順序は収束が決め、変更手順は Task/Workflow が実行する
3. **確信が持てないとき、加算はしても減算はしない** — `unknown ≠ absent`、`pruneable:false` が既定、分断時は halt
4. **持たない・委譲する** — 値/配置/予測/認可は自前で持たず外部に委ねる
5. **seam は今切る、split は後** — 拡張点の interface を早期に定義し、コアは変えずに後で分割する
6. **ownership がすべてを貫く** — `platform`（厳格）か `tenant`（緩い）かの 1 軸が書込経路・remediation・blast-radius を決める
7. **シャード間協調は現実を介して行う** — シャードは問い合わせず、相手の作った現実を Observe して gate する
8. **衛星の停止は、変化を止めるが、維持を止めない** — 定常収束は衛星コールゼロで回る

---

## 実装スコープ

| スライス | 内容 | 状態 |
| --- | --- | --- |
| **v1a** | コア型・収束ループ・安全機構（累積予算・List 完全性）・早期収束/BulkObserve/--claim の型切り・最小 SDK・実 provider 2 個・devenv・secret（file backend）・loader・status・chaos | ✅ 完成 |
| **v1b** | import / Sync Mode、tier 別 Observe 頻度、採番連携、静的シャーディング実装、inventory 検証 | 予定 |
| **v1.5** | 単純 Task ＋ 最小 Workflow（直列＋不変条件＋N-of-M 手動承認）＋ remediation schedule ＋ planned_change | 予定 |
| **v2+** | 多相 Task、自動段階ロールアウト/canary、DBSource、credential-broker、分散 Executor、intent UI | 予定 |

既知の v1a 限界（設計 §29 に記録）: 振動検知/隔離は未配線（`StatusOscillating` は型のみ・破壊的 flip-flop は ghost-diff hold で抑制）／ Vault SecretBackend はオフラインスタブ（file backend は実装済み）／ 早期収束・BulkObserve・--claim シャーディングは seam まで（実配線は v1b）。

---

## 開発

```console
$ make build      # ./sakutei をビルド
$ make test       # 全テスト（acceptance・chaos 含む）
$ make vet        # go vet
$ make check      # build + vet + test
$ make demo       # レビュー用デモ
$ make clean      # 生成物削除
```

外部依存はありません（標準ライブラリのみ）。ドキュメント・コメントは日本語です。

---

## ライセンス

[Apache-2.0](LICENSE)。OSS 個人プロジェクトとして作者個人が開発しています。
