# CHANGELOG v3.20 → v3.21

**改訂日: 2026-10-27**

provider を 2 つ足した改訂である（Backlog ストーリー #120、子 #121〜#123）。設計の向きは変えず、§29.1 に provider の
範囲と決まりを書き足した。

## #121：nftables provider の実の ruleset 版（live）

**問題**：nftables provider は管理された ruleset ファイルだけを読み書きし、`nft` を呼ばなかった。ファイルを書いてもカーネルの
FW に反映されず、FW を直に変えても drift として見えなかった。

**決定**：対象を設定で選ぶ（`file`＝今のまま・既定／`live`＝カーネルの実際の ruleset）。live では、管理する table を必ず指定し
（推測しない）、`nft -j list table` で観測する。Apply は chain の中身を入れ替えるスクリプトを `nft -c -f`（検査）のあと
`nft -f` で 1 つのトランザクションとして流す。管理外の table・base chain・set・map には触らない。nft が動かない・権限が
無いときは観測できない（unknown）。Schema・identity はファイル版と同じ。統合テストは使い捨ての network namespace の中で
実際の nft を使う（`make integration-nftables`。check・CI には入れない）。

## #122・#123：S3 互換 API のバケット provider `s3-bucket`

**決定**：S3 互換 API だけを使う汎用の provider（アカウント・利用者の管理は製品ごとに違うので対象外。§28.9 の外部 provider で
足す）。名前付きの endpoint の一覧を設定に持ち、ID は `<endpoint の名前>/<bucket>`。versioning・policy・lifecycle・tags の
うち宣言したものだけを管理し、正規形で比べて永久 drift を出さない。endpoint がその API を持たないとき、宣言したフィールドは
観測できない（unknown）。バケットの削除は破壊的として予算に数え、中身のあるバケットは消さない。署名（SigV4）は標準ライブラリ
だけで実装した。統合テスト（`make integration-s3`）は SeaweedFS を必須の相手にし、ほかの実装はイメージが取れなければ飛ばす。
イメージは tag と digest で固定する。

## 変更箇所

`docs/architecture/spec/part8-implementation.md`（§29.1 に 2 行）、`README.md`（版）、`docs/providers/nftables.md`・
`docs/providers/s3-bucket.md`（新規）。
