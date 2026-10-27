# CHANGELOG v3.23 → v3.24

**改訂日: 2026-10-27**

空の物理サーバに宣言だけで OS を入れる手順を部品にした改訂である（Backlog ストーリー os-bios、子 #139）。#87（Workflow の手順ごとの対象・前提条件）と #86（ゲスト OS の観測）の上に、一度きりの操作と応答ファイルを足した。設計の向きは変えていない。

## #139：一度きりの操作（§6.3）

- **背景**: 起動先の上書きは Continuous しか宣言できない（Once は BMC が 1 回の起動で使い切るので収束しない、§29.1 Redfish の（5））。OS の導入のあとに上書きを戻す手順を利用者が書く必要があった。再起動も「状態」でないので宣言できなかった。
- **変更内容**: Workflow の手順に `action: { resource, op, args }` を足した。provider の任意の capability `provider.Actioner`（`PlanAction`・`Action`、送っていない失敗は `ErrActionNotSent`）で施す。その位置で 1 回だけ施し、効いたかは後の `wait` / `require` で確かめる。送る直前に「送りかけ」の印を S2（Run の `action_sending`）に書き、印が残ったまま読み直したら二度送らずに halt。破壊的な操作は変更予算（§13）を通す。`redfish-system` は `boot_once`（`args.target`）と `reset`（`args.type`：既定 GracefulRestart、ForceRestart は破壊的。電源が切れていれば On）を持つ。
- **変えないこと**: 収束の宣言で `boot_enabled: Once` を断る決まり（エラーの文言は `action` を案内するようにした）。

## #139：自動導入の応答ファイル（§29.1）

- **背景**: 台ごとの値（ホスト名・アドレス・Sakutei の ssh の公開鍵・導入の目印）を宣言から作る口が無く、台ごとに ISO を作り直していた。
- **変更内容**: 雛形（`text/template`）に宣言の値を入れたファイルを書き出す provider `template-file` を足した。最小階級（`CheckReader` で「描いた中身とファイルが同じか」を答える）。秘密は書き出さない（SecretRef と秘密鍵に見える値は読み込みで断る）。置き場（ISO・HTTP）に入れるのは利用者の側。Alpine の雛形を例として docs に置いた。
- **見送り**: Workflow の手順から応答ファイルが書けたことを確かめる口（template-file は普通のリソースとして収束ループが書く。インストーラは取れるまで待つ）。

## OS の導入の例と統合テスト

`docs/trial/redfish-os-install` の手順を「CD を差す → require → boot_once → reset → 電源オフを待つ → 電源 On → guest-os を require → CD を抜く」に直した。`make integration-guestos` は 1 つの ISO の 3 台が宣言から書かれた応答ファイルを DMI の UUID で取りに来て入り、成功する台は done、失敗する 2 台は先へ進まず、3 台とも起動先の上書きが残らないことを確かめる。
