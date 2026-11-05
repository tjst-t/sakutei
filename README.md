# Sakutei（作庭）

**オンプレミス基盤全体を対象とした、宣言的インフラ収束エンジン（コントロールプレーン）。**

電源/BMC → ネットワーク → FW/SDN → ストレージ → ハイパーバイザ → VM → ゲスト OS → コンテナ までを、ひとつの「あるべき姿（intent）」から level-triggered に収束させます。位置づけは **「Ansible の冪等性 + Kubernetes の収束ループ」** です。

Kubernetes からは *収束原理*（level-triggered な収束ループ）だけを借り、実装機構（apiserver・etcd・scheduler・admission・CRD）は意図的に捨てています。最大の特徴は **観測 state レス** です。キャッシュした「現在状態」を持たず、cycle ごとに現実を観測し直します。これにより Terraform の three-way diff / state ドリフト問題を**構造的に**排除します。

名称は平安期の作庭書『作庭記』に由来します。

- **ライセンス:** Apache-2.0
- **言語:** Go（1.22 以上・標準ライブラリのみ、外部依存なし）
- **状態:** 2 つの系統があります。
  - **v2 系**（ルート、モジュール `github.com/tjst-t/sakutei/v2`）：リリース済み。最新は `v2.12.0`（2026-10-30）。設計書は [docs/architecture/spec/](docs/architecture/spec/)（v3.77）、変更点は [CHANGELOG.md](CHANGELOG.md)。
  - **v3**（[v3/](v3/)、モジュール `github.com/tjst-t/sakutei/v3`）：コアをゼロから作り直した新しいコア。設計書 [docs/core/README.md](docs/core/README.md) の Stage 1〜9 がすべて main に入っています。まだリリースしていません。
  - v3 への移行が終わったら、v2 のコードは削除する予定です。v2 の intent は `sakutei-migrate` で v3 の形に書き直せます。

> 設計書の「v3.77」は v2 系の設計書のバージョン番号で、新しいコア（v3）とは別のものです。

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

v2 は多くのユースケースを満たしましたが、同じ考えが別々の機能として何度も入り、設定の置き場所が分かれ、provider がコアの中にいました。v3 は同じユースケースを少ない部品の組み合わせで満たすように作り直したものです。v2 との互換は持ちません。

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

## v2 系

以下はルートの v2 系（`v2.12.0`）の説明です。

### クイックスタート

```console
$ make build      # ./sakutei をビルド（v3 のモジュールも一緒にビルドする）
$ make check      # build + vet + test（CI 相当）
$ make demo       # v2 の sakutei で plan → reconcile → drift → 自己修復を再生する
```

### `make demo` — GUI を持たない収束エンジンの「レビュー面」

GUI が無くても、`plan` / `status` の端末出力が**そのままレビュー面**です。`make demo` は実 provider（dnszone）を一時ディレクトリで動かし、次の流れを再生します。

1. `plan` — 宣言した 2 レコードが「これから作られる」ことを dry-run で見る
2. `reconcile` — 実体ファイル `zone.db` に収束
3. `status` — 適合レポート（`drifted=0`）
4. **誰かが `zone.db` を手で書き換える**（値を書き換え＋宣言に無いレコードを追加）
5. `status` — 書き換えを **cause B（ドリフト）** で検知し、宣言に無いレコードを **unmanaged** として出す
6. `reconcile` — **触っていないのに元へ戻る**（自己修復）

抜粋（ステップ 5）:

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

宣言に無いレコードは **prune されずに残ります**（原則「確信が持てないとき、加算はしても減算はしない」）。

### CLI

```
sakutei              # daemon として回り続ける（本来の姿）
sakutei plan         # dry-run: 何が変わるか（Diff で止まる）
sakutei reconcile    # 1 回収束する（手動キック）
sakutei status       # 適合レポート（drifted / unmanaged の一覧、read-only）
sakutei graph        # depends_on の DAG と ready_condition
sakutei import       # 既存の現実を intent（YAML）として書き出す
sakutei sync         # mode: sync の scope の現実を generated/ に記録する（Apply しない）
sakutei fence        # claim した scope の fencing 世代を表示する
sakutei task         # strategy: task の Task の一覧・取り消し・承認・却下
sakutei workflow     # Workflow の run の一覧・取り消し
sakutei approve      # Workflow の承認ゲートを承認する（reject で却下）
sakutei apply-check  # Apply を止めたリソース（oscillating）の一覧・解除
sakutei freeze       # 動いている daemon を止めずに freeze する（start / end / status）
sakutei audit        # audit のハッシュチェーンを確かめる・syslog へ送る
sakutei allocator    # 採番の authority（release / list / import を含む）
sakutei mirror       # intent リポジトリの read-only git ミラーを追従させる
sakutei secrets      # 拠点用の secret ファイルを作る（secrets extract）
sakutei breakglass   # DR モードの書き込み break-glass
```

`apply` は意図的に主 verb にしていません（「いつ変えるか」は収束が決める）。`plan` と `status` を前面に置くこと自体が、このソフトのレビュー体験です。各コマンドの終了コードは [docs/exit-codes.md](docs/exit-codes.md)、`plan --json`・`status --json` の形は [docs/plan-json.md](docs/plan-json.md)・[docs/status-json.md](docs/status-json.md) にあります。

主な機能と、詳しい説明の在りか:

- **Apply の効きの確認**：Apply のあとに読み直し、ineffective Apply が上限に達したら Apply を止める（oscillating）。設計 §11.1、CHANGELOG の #166。
- **依存先の Apply のあとの再観測**：観測できなかった依存元を同じ cycle で re-observe する。CHANGELOG の #177。
- **import と Sync Mode・採番・tier 別の Observe 頻度・静的シャーディング（claim の git 配布・S2-free シャード）**：設計 §17・§19・§24。
- **planned_change・承認ゲート・運用プロファイル・freeze・DR モード**：設計 §13・§14・§25。
- **本番の構成**（credential の分け方・secret backend・audit・/metrics と read-only HTTP API の TLS）：[docs/production.md](docs/production.md)。

### provider

Provider は `Schema` / `Capabilities` / `Read` / `Apply` ＋ 集合列挙 `List`（prune の前提）を実装します。SDK が diff・secret のマスク・managed 除外・バージョン埋め込み・エラー分類・契約テスト・Schema lint を受け持つので、作者は数個の関数を書くだけです。外の provider を足す方法は [examples/external-provider/README.md](examples/external-provider/README.md) にあります。

| 名前 | 対象 | 説明 |
| --- | --- | --- |
| `dnszone` | DNS のゾーンファイル | [docs/providers/dnszone.md](docs/providers/dnszone.md) |
| `nftables` | nftables の ruleset（ファイルとカーネル） | [docs/providers/nftables.md](docs/providers/nftables.md) |
| `redfish-system`・`redfish-virtual-media`・`redfish-bios`・`redfish-firmware` | BMC（Redfish）の電源・起動先・仮想メディア・BIOS・FW | [docs/providers/redfish.md](docs/providers/redfish.md) |
| `proxmox-vm`・`proxmox-fw` | Proxmox VE の VM と VM の FW の規則 | [proxmox-vm.md](docs/providers/proxmox-vm.md)・[proxmox-fw.md](docs/providers/proxmox-fw.md) |
| `netconf` | NETCONF の機器の設定の部分木 | [docs/providers/netconf.md](docs/providers/netconf.md) |
| `cli-device` | CLI でしか設定できない装置 | [docs/providers/cli-device.md](docs/providers/cli-device.md) |
| `host-file`・`systemd-unit` | 離れたホストのファイルと systemd のユニット | [host-file.md](docs/providers/host-file.md)・[systemd-unit.md](docs/providers/systemd-unit.md) |
| `guest-os` | ゲスト OS（観測だけ） | [docs/providers/guest-os.md](docs/providers/guest-os.md) |
| `metrics-probe` | /metrics の値（観測だけ） | [docs/providers/metrics-probe.md](docs/providers/metrics-probe.md) |
| `postgresql` | PostgreSQL のロール・データベース・権限・拡張 | [docs/providers/postgresql.md](docs/providers/postgresql.md) |
| `sqlmigrate` | SQLite・PostgreSQL のマイグレーション（Task 専用） | [docs/providers/sqlmigrate.md](docs/providers/sqlmigrate.md) |
| `ldap` | LDAP のエントリ | [docs/providers/ldap.md](docs/providers/ldap.md) |
| `s3-bucket` | S3 互換のバケット | [docs/providers/s3-bucket.md](docs/providers/s3-bucket.md) |
| `ansible` | Ansible のモジュール 1 つ（最小の階級） | [docs/providers/ansible.md](docs/providers/ansible.md) |
| `template-file` | 雛形から書き出すファイル（OS の自動導入の応答ファイルなど） | [docs/providers/template-file.md](docs/providers/template-file.md) |
| `fake` | テスト用 | fault injection ができる検証用 provider（chaos テストで使う） |

### 設計を貫く八つの原則（v2 系の設計書 §3）

1. **level-triggered + 観測 state レス** — cycle ごとに desired と actual を導出し直し、観測状態を保持しない
2. **いつ = 収束 / どう = 手続き** — 順序は収束が決め、変更手順は Task/Workflow が実行する
3. **確信が持てないとき、加算はしても減算はしない** — `unknown ≠ absent`、`pruneable:false` が既定、分断時は halt する
4. **持たない・委譲する** — 値/配置/予測/認可は自前で持たず外部に委ねる
5. **seam は今切る、split は後** — 拡張点の interface を早く定義し、コアは変えずに後で分割する
6. **ownership がすべてを貫く** — `platform`（厳格）か `tenant`（緩い）かの 1 軸が書き込み経路・remediation・blast-radius を決める
7. **シャード間協調は現実を介して行う** — シャードは問い合わせず、相手の作った現実を Observe して gate する
8. **衛星の停止は、変化を止めるが、維持を止めない** — 定常収束は衛星への呼び出しゼロで回る

v3 ではこれを 5 つの原則に整理し直しています（[docs/core/README.md](docs/core/README.md) §1.2）。

---

## リポジトリ構成

```
.
├── cmd/sakutei/            # v2: CLI の入口（cli.Main() を呼ぶだけ）
├── pkg/                    # v2: 公開 API（cli / core / provider / sdk / registry / netpolicy / allocation）
├── internal/               # v2: 収束ループ・diff・budget・prune・render・loader・各 provider など
├── tests/                  # v2: 受け入れテスト・chaos テスト（[AC-...] タグ付き）・統合テスト
├── examples/               # v2: 外の provider を足した自前のバイナリの例
├── v3/                     # v3: 新しいコア（別モジュール github.com/tjst-t/sakutei/v3）
│   └── providers/          #   provider・Expand・secret backend・sink・道具（別モジュール）
├── docs/                   # 文書（索引は docs/README.md）
├── Makefile                # build / test / vet / check / test-race / demo / integration-*
├── CHANGELOG.md            # v2 系のリリースごとの変更点
└── LICENSE                 # Apache-2.0
```

---

## ドキュメント

設計に関する記述は `docs/` を正とします。

- **[docs/README.md](docs/README.md)** — ドキュメントの索引（まずここ）
- **[docs/core/README.md](docs/core/README.md)** — v3（新しいコア）の設計書
- **[docs/architecture/spec/](docs/architecture/spec/)** — v2 系の設計書 v3.77（8 部 29 節・分冊）。§番号は全体で一意
- **[docs/writing-style.md](docs/writing-style.md)** — 文書の書き方
- **[docs/web/sakutei.html](docs/web/sakutei.html)** — 設計思想を庭園メタファで表したランディングページ
- **[docs/web/buildout.html](docs/web/buildout.html)** — 段0 だけがある状態から、20 台規模のポッド（リーフスイッチ → BMC → ハイパーバイザー導入 → クラスタ構築）を 1 つの intent PR から積み上げる構築ウォークスルー
- **[docs/web/scenarios.html](docs/web/scenarios.html)** — 稼働中の仮想基盤を題材に、増設・物理交換・FW 更新・ハイパーバイザーのローリング更新がどう実行されるかを追う運用シナリオ集

---

## 開発

```console
$ make build      # ./sakutei（v2）をビルドし、v3 と v3/providers もビルドする
$ make test       # 全テスト（v2 の acceptance・chaos、v3、v3/providers）
$ make vet        # go vet（3 つのモジュール）
$ make check      # build + vet + test
$ make test-race  # race detector 付きのテスト（cgo のため gcc が要る）
$ make demo       # v2 のレビュー用デモ
$ make clean      # 生成物を消す
```

統合テスト（`make integration-*`・`make integration-v3-*`）は KVM・docker・sudo などが要るので、`check` と CI には入りません。一覧と要るものは [CLAUDE.md](CLAUDE.md) と Makefile のコメントにあります。

外部依存はありません（標準ライブラリのみ）。文書とコメントは日本語で、[docs/writing-style.md](docs/writing-style.md) に従います。

---

## ライセンス

[Apache-2.0](LICENSE)。OSS の個人プロジェクトとして作者個人が開発しています。
