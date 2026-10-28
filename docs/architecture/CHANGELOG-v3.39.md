# CHANGELOG v3.38 → v3.39

**改訂日: 2026-10-28**

/metrics と読み取りの口の TLS と認可を決めた改訂である（Backlog #161）。

## #161：/metrics と読み取りの口の TLS と認可（§22.1・§22.3・§26.4・§29.1）

- **背景**: デーモンを固い区画に置き、監視のコレクターが別の区画から FW を越えて `/metrics`（`--metrics-listen`）と読み取り
  専用の口（`--read-listen`：`GET /status`・`/plan`・`/events`・`/graph`）を読みに来る構成では、その口が区画へ入る口になる。
  どちらも平文で認可が無く、守りは前段のプロキシに任せていた。
- **決定**:
  1. 設定は `--listen-auth-config <file>` に口ごと（`metrics:`・`read:`）に書く。サーバの証明書と鍵（PEM）・読むだけの
     トークンは `SecretRef`（file SecretBackend）でしか書けない。クライアント証明書を確かめる CA はファイル。
  2. **TLS** は 1.2 以上（1.3 も選べる）。証明書と鍵・トークンは 30 秒ごとに金庫を読み直し、再起動なしで入れ替える（読め
     なければ前のまま）。CA の束の入れ替えは再起動。
  3. **認可**は読むだけのトークン（`Authorization: Bearer`、定数時間で比べる）とクライアント証明書（CA で確かめ、任意で
     subject・SAN の一覧で絞る）。どちらか、または両方（両方が要る）。
  4. **既定は手元だけ**：loopback 以外を TLS なし・認可なしで待ち受けるのは `--metrics-insecure-listen`・
     `--read-insecure-listen` を付けたときだけ。付けずに外向きの平文（`0.0.0.0`・空の host・名前を含む）を指定すると起動で断る。
  5. 断ったら 401／403 を返し、理由を外に出さない（ログには出す）。数は `/metrics` の
     `sakutei_listen_rejected_total{listener,reason}`。
  6. 承認の書き込みの口（`--approval-listen`、§6.4）は同じポート・同じ資格を共有しない（同じアドレスは起動で断る）。
- **§22.1 との関係**: 人の認証・認可は IdP と上位に委ねたまま。口が持つのは機械の読み手が「読んでよい相手か」の 1 ビット
  だけで、利用者・役割は持たない。
- **互換**: `--listen-auth-config` を渡さず loopback で待ち受ける今までの使い方は、そのまま動く。loopback 以外に平文で
  待ち受けていた構成は、旗（`--metrics-insecure-listen`・`--read-insecure-listen`）を足すか TLS と認可を設定する。
- 実装：`internal/listenguard`、`pkg/cli/metrics.go`。受け入れテスト `tests/acceptance/listen_auth_test.go`（`[AC-161-1]`〜`[AC-161-5]`）。
