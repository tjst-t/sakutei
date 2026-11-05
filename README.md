# Sakutei（作庭）

**オンプレミス基盤全体を対象とした、宣言的インフラ収束エンジン（コントロールプレーン）。**

電源/BMC → ネットワーク → FW/SDN → ストレージ → ハイパーバイザ → VM → ゲスト OS → コンテナ までを、ひとつの「あるべき姿（intent）」から level-triggered に収束させます。位置づけは **「Ansible の冪等性 + Kubernetes の収束ループ」** です。

Kubernetes からは *収束原理*（level-triggered な収束ループ）だけを借り、実装機構（apiserver・etcd・scheduler・admission・CRD）は意図的に捨てています。最大の特徴は **観測 state レス** です。キャッシュした「現在状態」を持たず、cycle ごとに現実を観測し直します。これにより Terraform の three-way diff / state ドリフト問題を**構造的に**排除します。

名称は平安期の作庭書『作庭記』に由来します。

- **ライセンス:** Apache-2.0
- **言語:** Go（1.22 以上・標準ライブラリのみ、外部依存なし）
- **状態:** コア（[v3/](v3/)、モジュール `github.com/tjst-t/sakutei/v3`）と、provider などのモジュール（[v3/providers/](v3/providers/)、モジュール `github.com/tjst-t/sakutei/v3/providers`）からなります。設計書 [docs/core/README.md](docs/core/README.md) の Stage 1〜9 がすべて main に入っています。まだリリースしていません。
- **v2 から：** 以前の実装（v2 系、モジュール `github.com/tjst-t/sakutei/v2`）はリポジトリから消しました。最後の版はタグ `v2.12.0`（2026-10-30）です。v2 の intent は `sakutei-migrate` で今の形に書き直せます（下の「v2 からの移行」）。

---

## なぜ観測 state レスなのか

宣言的ツールの多くは「前回自分が書いた状態」をキャッシュし、それと現実・意図の 3 者を突き合わせます（three-way diff）。このキャッシュが現実とズレると、ドリフト検知が壊れ、`terraform refresh` のような救済が要ります。

Sakutei は**キャッシュを持ちません**。cycle ごとに、

```
desired（intent を render）  ⇄  actual（provider が現実を Read）  →  diff  →  必要なら Apply
```

を回すだけです。「前回の状態」が無いのでズレようがなく、**触っていないのに戻る**（自己修復）が最小の機構で成立します。

---

## v3（新しいコア）

以前の実装（v2）は多くのユースケースを満たしましたが、同じ考えが別々の機能として何度も入り、設定の置き場所が分かれ、provider がコアの中にいました。v3 は同じユースケースを少ない部品の組み合わせで満たすように作り直したものです。v2 との互換は持ちません。

- **コアは「1 cycle だけ回す処理」1 つ。** cycle をまたいで覚えるもの（Procedure の進み・承認・前の cycle の marks・通知の状態）は全部ディスクの Record に置きます。one-shot 実行（`reconcile --once`）でも daemon でも同じ結果になります。
- **部品は 9 つ：** Resource・Range・Provider・Condition・Graph・Policy・Procedure・Expand・Record と Events。
- **5 つの原則：** cycle ごとに観測し直して直す・確かでなければ消さない・壊す変更は数えて止め承認させる・「いつ」は収束で「どう」は Procedure・正しい状態は宣言に置く。
- **設定の置き場所は 2 つ：** intent（何を・Range・Policy）と、接続設定（config ファイル 1 つ。接続先・credential・待ち受け）。
- **provider はコアの外：** provider・Expand のモジュール・secret backend・Events の sink・承認ツールは別モジュール [v3/providers/](v3/providers/)（`github.com/tjst-t/sakutei/v3/providers`）にあります。

入口:

- **[v3/README.md](v3/README.md)** — パッケージの一覧・CLI・Stage ごとの決まり
- **[docs/core/README.md](docs/core/README.md)** — 新しいコアの設計書（原則・部品・不変条件・作る順番）
- **[v3/providers/README.md](v3/providers/README.md)** — provider ごとの接続設定と intent の例、統合テスト
- **[docs/trial/README.md](docs/trial/README.md)** の「v3 で試す」「v2 の intent を新しいコアへ書き直す」 — `docs/trial/v3/run.sh` で DNS と FW の試用の手順を流す

provider を組み込んだ CLI と道具は `v3/providers/cmd/` にあります。

```console
$ cd v3/providers && go build -o /tmp/sakutei-v3 ./cmd/sakutei
$ /tmp/sakutei-v3 plan      --intent <dir> --config <file> [--record <dir>]
$ /tmp/sakutei-v3 reconcile --intent <dir> --config <file> --record <dir> --once
$ /tmp/sakutei-v3           --intent <dir> --config <file> --record <dir>   # daemon
```

| コマンド | 役割 |
| --- | --- |
| `sakutei` | 収束エンジン本体（`plan`・`reconcile`・daemon・`approve`・`freeze`・`start` など。一覧は v3/README.md） |
| `sakutei-migrate` | v2 の intent と provider の設定を、v3 の intent と接続設定に書き直す |
| `sakutei-import` | 今の現実を宣言の形に書き出す（`--every` で繰り返すと v2 の Sync Mode の代わり） |
| `sakutei-approve` | ネットワーク越しの承認・却下を Record に書く承認ツール |
| `sakutei-nft-guard` | nftables のホストで root で動く専用ツール |

---

## v2 からの移行

v2 の実装と文書はこのリポジトリから消しました。v2 の最後の版は `v2.12.0` で、コード・設計書（v2 系の設計書 v3.77）・provider ごとの説明はそのタグで読めます。変更点の履歴は [CHANGELOG.md](CHANGELOG.md) にあります。

- v2 の intent と provider の設定は、`sakutei-migrate`（`v3/providers/cmd/sakutei-migrate`）で今の intent と接続設定に書き直せます。手順は [docs/trial/README.md](docs/trial/README.md) の「v2 の intent を新しいコアへ書き直す」にあります。
- v2 の 8 つの原則は 5 つの原則に整理し直しました（[docs/core/README.md](docs/core/README.md) §1.2）。v2 から移したもの・捨てたものの表は設計書の §7 にあります。

---

## リポジトリ構成

```
.
├── v3/                     # コア（モジュール github.com/tjst-t/sakutei/v3）
│   └── providers/          #   provider・Expand・secret backend・sink・道具（別モジュール）
├── docs/                   # 文書（索引は docs/README.md）
├── scripts/                # 外のモジュールから取り込めるかの確認スクリプト
├── Makefile                # build / test / vet / check / test-race / check-module / integration-v3-*
├── CHANGELOG.md            # リリースごとの変更点（v2 系の履歴を含む）
└── LICENSE                 # Apache-2.0
```

---

## ドキュメント

設計に関する記述は `docs/` を正とします。

- **[docs/README.md](docs/README.md)** — ドキュメントの索引（まずここ）
- **[docs/core/README.md](docs/core/README.md)** — 設計書（v3、新しいコア）
- **[v3/README.md](v3/README.md)**・**[v3/providers/README.md](v3/providers/README.md)** — パッケージ・CLI・provider の説明
- **[docs/writing-style.md](docs/writing-style.md)** — 文書の書き方

---

## 開発

```console
$ make build      # v3 と v3/providers をビルドする
$ make test       # v3 と v3/providers のテスト
$ make vet        # go vet（2 つのモジュール）
$ make check      # build + vet + test
$ make test-race  # race detector 付きのテスト（cgo のため gcc が要る）
```

統合テスト（`make integration-v3-*`）は KVM・docker・sudo などが要るので、`check` と CI には入りません。一覧と要るものは [CLAUDE.md](CLAUDE.md) と Makefile のコメントにあります。

外部依存はありません（標準ライブラリのみ）。文書とコメントは日本語で、[docs/writing-style.md](docs/writing-style.md) に従います。

---

## ライセンス

[Apache-2.0](LICENSE)。OSS の個人プロジェクトとして作者個人が開発しています。
