# CHANGELOG v3.22 → v3.23

**改訂日: 2026-10-27**

1 台のデーモンから離れたホストを扱う実行の層と、それに続く観測・通知・metrics を足した改訂である（Backlog ストーリー #129、子 #130・#131・#86・#54）。あわせて s3-bucket（#132）と postgresql（#133）の直しの決まりを書き足した。設計の向きは変えていない。

## #130：離れたホストを扱う実行の層

provider の外部コマンドとファイル操作を「手元」か「ssh 越し」で流す interface を足した。ssh は OpenSSH の CLI を exec で呼ぶ。引数は要素ごとに quote し、中身は標準入力で渡す。`BatchMode=yes`、ホスト鍵は設定の known_hosts で確かめる（知らない鍵は拒む）、接続を使い回す。秘密鍵は SecretRef から使う間だけ一時ファイルに出す。届かない・認証の失敗・ホスト鍵の不一致は unknown（absent にしない。ホスト鍵の不一致は永続エラー）。nftables（live）と dnszone をこの上に乗せ、1 台のデーモンで複数のホストを扱える。dnszone には再読み込みのコマンドの設定を足した。

## #131：通知の口（§22.3）

`--event-webhook`（Event を JSON のまま POST）と `--event-alertmanager`（level の Event を Alertmanager の alerts に写し、firing の間は送り直す）。送れなくても収束は止めない。

## #86：ゲスト OS の観測

#130 の上に、ゲスト OS を観測するだけの provider を足した。Workflow の `require`・`wait` で使い、OS の導入の完了を電源の状態ではなくゲスト OS の観測で判断する。届かない間は unknown で、Workflow の timeout で halt する。

## #54：デーモンの /metrics

stdlib だけで Prometheus の text format を書く。待ち受けは明示したときだけ。時系列の保存・通知の判定は委譲したまま（§27.6）。

## #132・#133

s3-bucket に endpoint ごとの `fields` と `storage_class_aliases`。postgresql のデータベースの権限の対象に PUBLIC（空の一覧で全部外せる。宣言していない PUBLIC の権限は prune しない）と、ロールの `bypassrls`・`replication`。
