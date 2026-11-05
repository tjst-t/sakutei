# CLAUDE.md

このファイルは、本リポジトリで作業する Claude Code（および他の AI エージェント）向けのガイドです。

## プロジェクト概要

**Sakutei（作庭）** は、オンプレミス基盤全体（電源/BMC → ネットワーク → FW/SDN → ストレージ → ハイパーバイザ → VM → ゲスト OS → コンテナ）を対象とした**宣言的インフラ収束エンジン（コントロールプレーン）**。

Kubernetes からは *収束原理*（level-triggered な収束ループ）だけを借り、実装機構（apiserver・etcd・scheduler・admission・CRD）は意図的に捨てている。位置づけは「**Ansible の冪等性 + k8s のループ**」。最大の特徴は **観測 state レス**（キャッシュした「現在状態」を持たず cycle ごとに現実を観測し直す）で、これにより Terraform の three-way diff / state ドリフト問題を構造的に排除する。名称は平安期の作庭書『作庭記』に由来する。

- **ライセンス:** Apache-2.0。OSS の個人プロジェクト（作者個人が開発。将来の業務利用はありうる）。
- **言語:** Go 1.22 以上・標準ライブラリのみ（外部依存なし）。

## 現状：2 つの系統

| | v2 系 | v3（新しいコア） |
| --- | --- | --- |
| 置き場所 | リポジトリのルート（`cmd/`・`pkg/`・`internal/`・`tests/`・`examples/`） | `v3/`（コア）と `v3/providers/`（provider・Expand・secret backend・sink・道具） |
| Go モジュール | `github.com/tjst-t/sakutei/v2` | `github.com/tjst-t/sakutei/v3` と `github.com/tjst-t/sakutei/v3/providers`（別モジュール） |
| 設計書 | `docs/architecture/spec/`（v3.77、8 部 29 節） | `docs/core/README.md` |
| 状態 | リリース済み。最新はタグ `v2.12.0`（2026-10-30）。変更点は `CHANGELOG.md` | 設計書 §8 の Stage 1〜9 がすべて main に入っている。未リリース |
| 入口の文書 | `README.md` の「v2 系」 | `v3/README.md`・`v3/providers/README.md` |

- v3 はコアをゼロから作り直したもので、v2 との互換は持たない。v2 のユースケースは全部満たす（設計書 §7 に v2 から移すもの・捨てるものの表）。
- v2 の intent は `sakutei-migrate`（`v3/providers/cmd/sakutei-migrate`）で v3 の intent と接続設定に書き直せる。手順は `docs/trial/README.md` の「v2 の intent を新しいコアへ書き直す」。
- **v3 への移行が終わったら v2 のコードは削除する予定。** それまで v2 は直しだけにし、新しい機能は v3 に足す（設計書 §8 の方針）。
- 設計書の「v3.77」は v2 系の設計書のバージョン番号で、v3（新しいコア）とは別のもの。取り違えない。
- v2 系の設計書の改訂の経緯は `docs/architecture/CHANGELOG-v3.*.md`、リリースごとの変更点は `CHANGELOG.md`（`#nn` は Backlog の番号）にある。ここには繰り返さない。

## 仕事の管理（Backlog）

- 仕事の一覧・優先順・状態は **Backlog**（git の `backlog` ブランチの `tasks.json`）が唯一の正。旧 `docs/ROADMAP.json` は廃止した（v1a のスプリント 1〜9 は Backlog に done のストーリー `v1a-1`〜`v1a-9` として移行済み。受け入れ条件の全文は git 履歴のコミット 9d7cf32 の `docs/ROADMAP.json` にある）。
- v2 の受け入れテストには `[AC-<スプリントID>-<ストーリー>-<番号>]`（例 `[AC-Sc7a647-2-1]`）のタグが付いている。旧スプリント ID と Backlog の対応は各ストーリーの本文に書いてある。
- 新しい作業は Story → Task に分けて Backlog に置き、完了条件は計測できる形で書く。

## 開発コマンド

ルートの Makefile が 3 つのモジュール（ルートの v2・`v3`・`v3/providers`）をまとめて回す。

```console
$ make build      # ./sakutei（v2）をビルドし、v3 と v3/providers もビルドする
$ make test       # 全テスト（v2 の tests/acceptance の受け入れ・chaos、v3、v3/providers）
$ make vet        # go vet（3 つのモジュール）
$ make check      # build + vet + test
$ make test-race  # race detector 付きのテスト（3 つのモジュール。cgo のため gcc が要る）
$ make demo       # v2 の sakutei で plan → converge → drift → self-heal を再生する
$ make check-module VERSION=<tag|commit>  # 外のモジュールから v2 を go get で取り込めるか（ネットワークが要る）
```

- 同時に回すパッケージの数は `TEST_P`（既定 8）。CPU の多いホストで fork/exec が EAGAIN で落ちるのを避けるための値なので、むやみに上げない。
- v3 だけを回すなら `cd v3 && go test ./...`・`cd v3/providers && go test ./...`。
- CI は `.github/workflows/ci.yml`（GitHub Actions）。push（main とタグ）と PR のたびに、Go 1.22 と最新の stableで `make check` を、最新の stableで `make test-race` を回す。push では `make check-module` も回す。

### 統合テスト

本物の相手（カーネル・BMC の模擬・コンテナ・入れ子の VM）を使う。KVM・docker・sudo（パスワードなし）などが要るので、`check`・`test`・CI には入れない。要るものと手順は Makefile の各ターゲットのコメントにある。結果は v2 は `docs/providers/*.md`、v3 は `v3/providers/README.md` の「統合テスト」に書く。

| v3 | v2 | 相手 |
| --- | --- | --- |
| `make integration-v3-nftables` | `make integration-nftables` | 使い捨ての network namespace の nft（v2 は ssh で入るコンテナも） |
| `make integration-v3-redfish` | `make integration-redfish` | qemu-bmc 0.11.0 × 2（containerlab） |
| `make integration-v3-netconf` | `make integration-netconf` | netopeer2（docker） |
| `make integration-v3-proxmox` | `make integration-proxmox` | 入れ子の Proxmox VE 9.2（qemu） |
| `make integration-v3-postgresql` | `make integration-postgresql` | PostgreSQL（docker。v3 は sqlmigrate も） |
| `make integration-v3-ldap` | `make integration-ldap` | 389 Directory Server（docker） |
| `make integration-v3-s3` | `make integration-s3` | S3 互換のサーバ（docker） |
| `make integration-v3-ansible` | `make integration-ansible` | ansible-core（venv。初回は PyPI に届くことが要る） |
| — | `make integration-remote`・`integration-guestos`・`integration-hostconf`・`integration-vault`・`integration-otlp` | sshd のコンテナ・qemu-bmc での OS 導入・systemd を PID 1 で動かすコンテナ・OpenBao（Vault 形の KV v2）・OpenTelemetry Collector |

同じ名前の lab・コンテナを使うターゲットは同時に 1 つだけ回す。同時に回せるかどうかは各ターゲットのコメントに書いてある。

## リポジトリ構成

```
.
├── CLAUDE.md                     # このファイル
├── README.md                     # 入口（v2 系と v3 の紹介）
├── CHANGELOG.md                  # v2 系のリリースごとの変更点
├── Makefile
├── cmd/sakutei/                  # v2: main（cli.Main() を呼ぶだけ）
├── pkg/                          # v2: 公開 API。cli（CLI の入口、§28.9）/ core（正準型）/ provider /
│                                 #   registry / sdk / netpolicy（network policy、§7.6）/ allocation
├── internal/                     # v2: reconcile・diff・budget・prune・cause・render・loader・各 provider など
├── tests/acceptance/             # v2: 受け入れテスト・chaos テスト（AC タグ付き）
├── tests/integration/            # v2: 統合テスト（build tag 付き）
├── examples/external-provider/   # v2: 外の provider を足した自前のバイナリの例（別の go.mod）
├── templates/provider/           # v2: provider を書き始めるための雛形（§28.6）
├── scripts/                      # check-module の確認スクリプト
├── v3/                           # v3: 新しいコア（別モジュール）。パッケージの一覧は v3/README.md
│   ├── cmd/sakutei/              #   コアだけの CLI（偽の provider）
│   ├── sdk/                      #   provider SDK（contract・fake・hostexec）
│   └── providers/                #   コアの外のモジュール。provider・Expand・secret backend・sink
│       └── cmd/                  #   sakutei（全部を組み込んだ CLI）・sakutei-migrate・sakutei-import・
│                                 #   sakutei-approve・sakutei-nft-guard
└── docs/
    ├── README.md                 # ドキュメントの索引（まずここを見る）
    ├── writing-style.md          # 文書の書き方（用語の表）
    ├── core/README.md            # v3 の設計書
    ├── architecture/spec/        # v2 系の設計書 v3.77（README.md + part1〜8）
    ├── architecture/CHANGELOG-v3.*.md  # v2 系の設計書の改訂ごとの変更
    ├── architecture/old/         # 旧バージョンのアーカイブ（編集しない）
    ├── providers/                # v2 の provider ごとの説明
    ├── trial/                    # 試用の手順（v1・v2 の各機能、v3 は trial/v3/）
    ├── review/                   # 設計レビュー・適用可能性検討・v3 のもとにした検討
    └── web/                      # ランディングページなど
```

## v3 の作業の決まり

- **別モジュール。** `v3/` と `v3/providers/` はそれぞれ `go.mod` を持つ。ルートの v2 のパッケージを import しない（互換を持たない）。`v3/providers` は `replace` でコアを手元の `v3/` に向ける。
- **標準ライブラリだけ。** v3 にも外部依存を入れない（YAML も `v3/internal/yaml` で読む）。
- **コアと外の分担。** provider・Expand の実装・secret backend・Events の sink・承認ツール・import と同期の道具はコアの外の `v3/providers/` に置く。コアはインターフェースだけを持つ。provider 固有の起動の引数をコアに足さない。組み込む一覧は `v3/providers/builtin`。
- **設計書が正。** 振る舞いを変えるときは `docs/core/README.md` を先に（または同時に）直す。各部品の決まりは §3、不変条件は §4（30 項。それぞれ守るテストが表にある）、作る順番は §8。Stage ごとの決まりは `v3/README.md` に書く。
- **テスト。** 変更のあとは `make check` と `make test-race` を通す。provider は `sdk/contract` の契約テスト（`contract.Run`、Transactor を持つものは `contract.RunTxn`）を通す。Expand は `expand/expandtest.Golden` の golden テストを使う。
- **用語。** v3 の用語は v2 と違うものがある（Task/Workflow → Procedure、planned_change → Policy の window と allow、原因タグ A〜E・シャード・claim・fencing・DR モードはやめた）。v3 の文書・コードでは v3 の用語を使う。

## v2 系の設計（docs/architecture/spec/）

**権威関係:** `docs/architecture/spec/`（README.md + part1〜8）が v2 系の「設計の正」（現行 v3.77。骨格は v2.6 と内容が同じ全面再構成 v3.0 で、v3.1〜v3.77 は分冊を直接改訂）。`old/` は旧バージョン（編集しない）、矛盾したら **spec/ が優先**。

| 部 | 節 | ファイル | 内容 |
| --- | --- | --- | --- |
| I 思想と境界 | §1-3 | part1-philosophy.md | 課題認識 / 作る・委譲する・捨てる / **八つの原則**と適用境界 |
| II 中核モデル | §4-7 | part2-core-model.md | 正準型と状態分類 S1-S4 / Provider 抽象 / 3 プリミティブ / 所有と境界（provider の範囲 §7.7 を含む） |
| III 収束機構 | §8-11 | part3-convergence.md | 収束ループ+早期収束パス / render / 差分と原因（cause tags）/ 多周期収束 |
| IV 安全機構 | §12-15 | part4-safety.md | 削除の規律 / 変更予算+planned_change / 修復の統制+プロファイル / 隔離と防御（fencing）|
| V スケールと分散 | §16-18 | part5-scale.md | 天井算術（BulkObserve）/ claim シャーディング+S2-free / 分散の原則 |
| VI 衛星と外部世界 | §19-22 | part6-satellites.md | allocator / secret と credential / DesiredSource と catalog / 委譲と停止影響表 |
| VII 運用ライフサイクル | §23-26 | part7-operations.md | 段0・段0-minimal・段0-zero / ブラウンフィールド / 災害と復旧（DR モード）/ 検証と監査 |
| VIII 実装 | §27-29 | part8-implementation.md | スライス v1a/v1b/v1.5/v2 / Provider SDK / 残る課題と既知の限界 |

### 設計を貫く八つの原則（§3）

1. **level-triggered + 観測 state レス** — cycle ごとに desired と actual を導出し直し、観測状態を保持しない。
2. **いつ = 収束 / どう = 手続き** — 「いつ・どの順で変えるか」は収束が決め、「どう変えるか」は Task/Workflow が実行する。
3. **確信が持てないとき、加算はしても減算はしない**（unknown ≠ absent、`pruneable:false` が既定、分断/blast-radius 時は halt する）。
4. **持たない・委譲する** — 値/配置/予測/認可は自前で持たず外部に委ねる。
5. **seam は今切る、split は後** — 拡張/分離の接点となる interface を早めに定義し、コアは変えずに後で分割する。
6. **ownership がすべてを貫く** — `platform`（PR 必須・厳格）か `tenant`（即時書き込み・緩い）かの 1 軸が、書き込み経路/remediation/レート制限/blast-radius/source/シャード境界を決める。
7. **シャード間協調は現実を介して行う** — シャード同士は問い合わせない。相手の作った現実を Observe して gate する。
8. **衛星の停止は、変化を止めるが、維持を止めない** — 定常収束は衛星への呼び出しゼロで回る。

v3 ではこれを 5 つの原則（`docs/core/README.md` §1.2）に整理し直している。

### 中核の用語（v2 系、§参照つき）

- **Provider / Lister**（§5）— `Schema`/`Capabilities`/`Read`/`Apply` + 集合列挙 `List`（prune の前提、complete-or-error）。
- **BulkObserve**（§5）— バルク観測の capability。prune には使わない（型を分ける）。
- **Observation vs ConvergenceStatus**（§4, §10）— Observe は Availability（`ok`/`unknown`/`converging`）+ State を返し desired を知らない。`drifted`/`converged` は Diff から導出する。
- **3 プリミティブ**（§6）— Resource / Task（単純・多相）/ Workflow（順序+不変条件+承認ゲート）。
- **render パイプライン**（§9）— フェーズ1（外部入力の解決）+ フェーズ2（決定的な展開）。外部の権威に届かないときは前回の解決値を保つ（空の desired を出さない）。
- **cause tag A〜E**（§10）— A 意図 / B ドリフト / C ツール更新（ghost diff）/ D 波及 / E 外部入力。
- **planned_change**（§13）— 大量の正当な作業の宣言（scope/種別/上限/window/承認）。専用の budget レーン。
- **運用プロファイル**（§14）— solo / team（既定）/ enterprise。
- **claim-as-intent / S2-free シャード**（§17）、**external_owner**（§7）、**SecretRef**（§20）、**DR モード**（§25）、**状態クラス S1〜S4**（§4）。

### v2 の CLI（§26）

`sakutei`（daemon）・`plan`・`reconcile`・`status`・`import` が設計上の主 verb。`apply` は意図的に主 verb にしない。ほかのサブコマンドは `pkg/cli/main.go` の `printUsage` と `README.md` にある。

## 作業時の指針

- **文書は `docs/writing-style.md` に従って書く。** 説明文は日本語、技術用語・コマンド名・見出し語は英語のまま。IT の現場でふつうのカタカナ語はそのまま使い、無理な和訳・漢語化・独自の和語の言い換え（「周」「口」「欄」など）をしない。用語の表に従う。
- 既存の用語（英語の component 名、庭園メタファの別名: 関守/台帳/図面/坪庭/縮景）を踏襲する。コンポーネント名は機能的な英語名（reconciler / credential-broker / allocator / catalog）を使い、固有名詞は「Sakutei」だけ。
- **v3 の設計を変えるとき**は `docs/core/README.md` を直す。
- **v2 系の設計を変えるとき**は `docs/architecture/spec/` を正とする。旧バージョンやサンプルとの矛盾は spec/ に寄せる。改訂は分冊（part1〜8）を直接編集し、`docs/architecture/CHANGELOG-vX.Y.md` を足して `spec/README.md` のバージョン番号を上げる（バージョンごとのファイル複製はしない。履歴は git）。大規模な再構成のときだけ `spec/` 一式を `old/vX.Y/` にスナップショットする。
- v2 のリリースの変更点は `CHANGELOG.md` の「未リリース」に書く。
- `generated/` は自動生成専用（ヘッダ必須・直接コミット拒否）、`intent/` は人が書く intent の置き場。
- **Schema 設計が最難関**（secret/managed/ordered/identity の誤宣言は安全保証を黙って無効化する。identity は運用上の変更頻度で選ぶ）。
- 公開リポジトリなので、社内のホスト名・特定ベンダーの機種名・内部の道具やプロジェクトの名前を文書・コード・テストに書かない。例のアドレスは文書用の範囲（`192.0.2.0/24` など）、ドメインは `example.com` を使う。
