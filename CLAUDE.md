# CLAUDE.md

このファイルは、本リポジトリで作業する Claude Code（および他の AI エージェント）向けのガイドです。

## プロジェクト概要

**Sakutei（作庭）** は、オンプレミス基盤全体（電源/BMC → ネットワーク → FW/SDN → ストレージ → ハイパーバイザ → VM → ゲスト OS → コンテナ）を対象とした**宣言的インフラ収束エンジン（コントロールプレーン）**。

Kubernetes からは *収束原理*（level-triggered な収束ループ）だけを借り、実装機構（apiserver・etcd・scheduler・admission・CRD）は意図的に捨てている。位置づけは「**Ansible の冪等性 + k8s のループ**」。最大の特徴は **観測 state レス**（キャッシュした「現在状態」を持たず cycle ごとに現実を観測し直す）で、これにより Terraform の three-way diff / state ドリフト問題を構造的に排除する。名称は平安期の作庭書『作庭記』に由来する。

- **ライセンス:** Apache-2.0。OSS の個人プロジェクト（作者個人が開発。将来の業務利用はありうる）。
- **言語:** Go 1.22 以上・標準ライブラリのみ（外部依存なし）。

## 現状

| | |
| --- | --- |
| 置き場所 | `v3/`（コア）と `v3/providers/`（provider・Expand・secret backend・sink・道具） |
| Go モジュール | `github.com/tjst-t/sakutei/v3` と `github.com/tjst-t/sakutei/v3/providers`（別モジュール） |
| 設計書 | `docs/core/README.md` |
| 状態 | 設計書 §8 の Stage 1〜9 がすべて main に入っている。未リリース |
| 入口の文書 | `v3/README.md`・`v3/providers/README.md` |

- v3 はコアをゼロから作り直したもので、以前の実装（v2 系、モジュール `github.com/tjst-t/sakutei/v2`）との互換は持たない。v2 のユースケースは全部満たす（設計書 §7 に v2 から移したもの・捨てたものの表）。
- **v2 の実装と文書は消した。** 最後の版はタグ `v2.12.0`（2026-10-30）。v2 のコード・設計書（v2 系の設計書 v3.77、`docs/architecture/spec/`）・provider ごとの説明は、そのタグで読める。v2 の新しい版は出さない。
- v2 の intent は `sakutei-migrate`（`v3/providers/cmd/sakutei-migrate`）で v3 の intent と接続設定に書き直せる。手順は `docs/trial/README.md` の「v2 の intent を新しいコアへ書き直す」。テストの入力（v2 の試用の intent と最初の現実）は `v3/providers/migrate/testdata/trial/` にある。
- 「v3.77」は v2 系の設計書のバージョン番号で、v3（新しいコア）とは別のもの。`docs/review/` などで見かけても取り違えない。
- リリースごとの変更点は `CHANGELOG.md`（`#nn` は Backlog の番号。v2 系の履歴を含む）にある。

## 仕事の管理（Backlog）

- 仕事の一覧・優先順・状態は **Backlog**（git の `backlog` ブランチの `tasks.json`）が唯一の正。旧 `docs/ROADMAP.json` は廃止した（v1a のスプリント 1〜9 は Backlog に done のストーリー `v1a-1`〜`v1a-9` として移行済み。受け入れ条件の全文は git 履歴のコミット 9d7cf32 の `docs/ROADMAP.json` にある）。
- v2 の受け入れテスト（`[AC-<スプリントID>-<ストーリー>-<番号>]` のタグ付き）は v2 と一緒に消した。旧スプリント ID と Backlog の対応は各ストーリーの本文に書いてある。
- 新しい作業は Story → Task に分けて Backlog に置き、完了条件は計測できる形で書く。

## 開発コマンド

ルートの Makefile が 2 つのモジュール（`v3` と `v3/providers`）をまとめて回す。

```console
$ make build      # v3 と v3/providers をビルドする
$ make test       # v3 と v3/providers のテスト
$ make vet        # go vet（2 つのモジュール）
$ make check      # build + vet + test
$ make test-race  # race detector 付きのテスト（2 つのモジュール。cgo のため gcc が要る）
$ make check-module VERSION=<tag|commit>  # 外のモジュールから v3 と v3/providers を go get で取り込めるか（ネットワークが要る）
```

- 同時に回すパッケージの数は `TEST_P`（既定 8）。CPU の多いホストで fork/exec が EAGAIN で落ちるのを避けるための値なので、むやみに上げない。
- 片方だけを回すなら `cd v3 && go test ./...`・`cd v3/providers && go test ./...`。
- CI は `.github/workflows/ci.yml`（GitHub Actions）。push（main とタグ）と PR のたびに、Go 1.22 と最新の stableで `make check` を、最新の stableで `make test-race` を回す。push では `make check-module` も回す。

### 統合テスト

本物の相手（カーネル・BMC の模擬・コンテナ・入れ子の VM）を使う。KVM・docker・sudo（パスワードなし）などが要るので、`check`・`test`・CI には入れない。要るものと手順は Makefile の各ターゲットのコメントにある。結果は `v3/providers/README.md` の「統合テスト」に書く。

| ターゲット | 相手 |
| --- | --- |
| `make integration-v3-nftables` | 使い捨ての network namespace の nft |
| `make integration-v3-redfish` | qemu-bmc 0.11.0 × 2（containerlab） |
| `make integration-v3-netconf` | netopeer2（docker） |
| `make integration-v3-proxmox` | 入れ子の Proxmox VE 9.2（qemu） |
| `make integration-v3-postgresql` | PostgreSQL（docker。sqlmigrate も） |
| `make integration-v3-ldap` | 389 Directory Server（docker） |
| `make integration-v3-s3` | S3 互換のサーバ（docker） |
| `make integration-v3-ansible` | ansible-core（venv。初回は PyPI に届くことが要る） |

同じ名前の lab・コンテナを使うターゲットは同時に 1 つだけ回す。同時に回せるかどうかは各ターゲットのコメントに書いてある。

## リポジトリ構成

```
.
├── CLAUDE.md                     # このファイル
├── README.md                     # 入口
├── CHANGELOG.md                  # リリースごとの変更点（v2 系の履歴を含む）
├── Makefile
├── scripts/                      # check-module の確認スクリプト
├── v3/                           # コア（別モジュール）。パッケージの一覧は v3/README.md
│   ├── cmd/sakutei/              #   コアだけの CLI（偽の provider）
│   ├── sdk/                      #   provider SDK（contract・fake・hostexec）
│   └── providers/                #   コアの外のモジュール。provider・Expand・secret backend・sink
│       └── cmd/                  #   sakutei（全部を組み込んだ CLI）・sakutei-migrate・sakutei-import・
│                                 #   sakutei-approve・sakutei-nft-guard
└── docs/
    ├── README.md                 # ドキュメントの索引（まずここを見る）
    ├── writing-style.md          # 文書の書き方（用語の表）
    ├── core/README.md            # v3 の設計書
    ├── trial/                    # 試用の手順（v3 は trial/v3/）
    └── review/                   # v3 のもとにした検討（v2 の設計書・文書を引いている）
```

## v3 の作業の決まり

- **別モジュール。** `v3/` と `v3/providers/` はそれぞれ `go.mod` を持つ。`v3/providers` は `replace` でコアを手元の `v3/` に向ける。
- **標準ライブラリだけ。** 外部依存を入れない（YAML も `v3/internal/yaml` で読む）。
- **コアと外の分担。** provider・Expand の実装・secret backend・Events の sink・承認ツール・import と同期の道具はコアの外の `v3/providers/` に置く。コアはインターフェースだけを持つ。provider 固有の起動の引数をコアに足さない。組み込む一覧は `v3/providers/builtin`。
- **設計書が正。** 振る舞いを変えるときは `docs/core/README.md` を先に（または同時に）直す。各部品の決まりは §3、不変条件は §4（30 項。それぞれ守るテストが表にある）、作る順番は §8。Stage ごとの決まりは `v3/README.md` に書く。
- **テスト。** 変更のあとは `make check` と `make test-race` を通す。provider は `sdk/contract` の契約テスト（`contract.Run`、Transactor を持つものは `contract.RunTxn`）を通す。Expand は `expand/expandtest.Golden` の golden テストを使う。
- **用語。** v3 の用語は v2 と違うものがある（Task/Workflow → Procedure、planned_change → Policy の window と allow、原因タグ A〜E・シャード・claim・fencing・DR モードはやめた）。v3 の文書・コードでは v3 の用語を使う。v3 の原則は設計書 §1.2 の 5 つ（v2 の 8 つの原則を整理し直したもの）。

## 作業時の指針

- **文書は `docs/writing-style.md` に従って書く。** 説明文は日本語、技術用語・コマンド名・見出し語は英語のまま。IT の現場でふつうのカタカナ語はそのまま使い、無理な和訳・漢語化・独自の和語の言い換え（「周」「口」「欄」など）をしない。用語の表に従う。
- 既存の用語（英語の component 名、庭園メタファの別名: 関守/台帳/図面/坪庭/縮景）を踏襲する。コンポーネント名は機能的な英語名（reconciler / credential-broker / allocator / catalog）を使い、固有名詞は「Sakutei」だけ。
- **v3 の設計を変えるとき**は `docs/core/README.md` を直す。
- リリースの変更点は `CHANGELOG.md` の「未リリース」に書く。
- `generated/` は自動生成専用（ヘッダ必須・直接コミット拒否）、`intent/` は人が書く intent の置き場。
- **Schema 設計が最難関**（secret/managed/ordered/identity の誤宣言は安全保証を黙って無効化する。identity は運用上の変更頻度で選ぶ）。
- 公開リポジトリなので、社内のホスト名・特定ベンダーの機種名・内部の道具やプロジェクトの名前を文書・コード・テストに書かない。例のアドレスは文書用の範囲（`192.0.2.0/24` など）、ドメインは `example.com` を使う。
