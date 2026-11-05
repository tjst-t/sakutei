# 文書の書き方

このリポジトリの文書（README・設計書・docs）と、コードのコメント・CLI のメッセージの書き方。

## 基本

- 説明文は日本語で書く。
- 技術用語・コマンド名・UI のラベル・見出し語（Step・Phase・Issue・PR・commit など）は英語のまま使う。
- 日本の IT 現場でふつうに使うカタカナ語（デプロイ・リポジトリ・ロック・キューなど）はそのまま使う。
- 無理な和訳や漢語化はしない。英語の用語を和語に言い換えて独自の言葉を作らない（例：「口」「欄」「周」「当てる」）。
- 結論を先に書く。1 文を短くする。

## 用語

コアの部品名・状態・設定のキーは英語のまま書く。

| 使わない書き方 | 使う書き方 |
|---|---|
| 周・1 周 | cycle・1 cycle |
| 常駐 | daemon |
| 一回実行 | one-shot 実行（`reconcile --once`） |
| 当てる | Apply する |
| 送り先 | sink |
| 送り出しの列 | outbox |
| 読み取り HTTP・読み取りの口 | read-only HTTP API |
| つなぎ方の設定 | 接続設定（config ファイル） |
| 宣言外 | 宣言に無いもの（unmanaged） |
| 状態型の Event・出来事型の Event | level Event・edge Event |
| 量化 | quantifier（`all`・`any`・`count`） |
| 手順（コアの部品として） | Procedure |
| 手順の段 | step |
| 段 1〜9（作る順番） | Stage 1〜9 |
| 2 段の Apply | two-phase Apply（Transaction） |
| 管理する範囲 | Range |
| 予算・窓・別枠 | budget・window・allow |
| 決まった道具 | 専用ツール |
| 外の持ち主 | external owner |
| 目印 | marks（`marks.json`） |
| 健全性 | health |
| 起床 | wake |
| 監査の鎖 | audit のハッシュチェーン |
| 秘密・保管先 | secret・secret backend |
| 資格 | credential |
| 項目 | field |
| 種類（Resource の） | type |
| ある・無い・分からない・作業中 | Present・Absent・Unknown・Converging |
| 観測し直す | 再観測する（re-observe） |
| 効かない Apply | ineffective Apply |
| 読み込みで断る | 読み込み時にエラーにする（reject） |
| 知らせる | 通知する |
| 止める（halt） | halt する |
| 網・網の許可 | ネットワーク・network policy |
| 互換の表 | compatibility table |
| 確かめ（`check`） | check |
| 版 | バージョン |

「観測」「宣言」「収束」「承認」「採番」「冪等」のように、ふつうの日本語や IT の現場で通じる言葉はそのまま使う。
