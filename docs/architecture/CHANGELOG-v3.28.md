# CHANGELOG v3.27 → v3.28

**改訂日: 2026-10-28**

離れたホストのファイルと systemd のユニット（podman の quadlet を含む）を宣言どおりに保ち、依って立つファイルを書き換えたら
ユニットを入れ替える口を足した改訂である（Backlog #155。親のストーリー「離れたホストのファイルと systemd のユニット（quadlet）を
宣言で保つ・通知の口の冗長」）。設計の向き（観測 state レス・正しい状態は宣言とホストの現実に置く）は変えていない。

## #155：host-file と systemd-unit（§29.1）

- **背景**: VM の中のサービスを podman の quadlet（`/etc/containers/systemd/*.container`）と systemd で配りたいが、口は ansible
  provider（最小の階級、§28.7）だけだった。フィールドごとの差分が無く、ファイルを書き換えてもサービスが入れ替わらない（Ansible の
  handler にあたるものが無い）。
- **決定**: 離れたホストの実行の層（#130）の上に、標準階級の provider を 2 つ置く。
  - `host-file`：ID `<ホスト>/<絶対パス>`。`content`（または SecretRef の `content_secret`）・`mode`・`owner`・`group`。
    観測は中身の sha256 と stat だけで、平文を State に載せない。宣言の `content` とは RefinePlan が hash で比べる。
    `content_secret` はホストの側の印に「書いた参照と中身の hash」を残し、いまの中身と同じときだけ参照を観測として返す。
    宣言できる中身は 256 KiB まで。書き込みは一時ファイル → rename。
  - `systemd-unit`：ID `<ホスト>/<ユニットのファイル名>`（quadlet は `app.container`、動くユニットは `app.service`）。
    `content`・`state`（active / inactive）・`enabled`（通常のユニットだけ）・`restart_on`・`on_change`（restart / reload）・
    `disruptive`。元のファイルを書き換えたら `daemon-reload` を 1 回。
  - **入れ替えの口は level で判定する**：最後に（再）起動したときの元のファイルと `restart_on` のファイルの hash をホストの側の
    印（`<stamp_dir>/unit-stamps/<名前>`）に残し、観測の `restart_on` は「印といまの hash が同じファイル」の一覧にする。
    書き換えれば宣言と違って drift になり、Apply がいまの姿を読み直して 1 回だけ入れ替え、印を更新する。Sakutei の中に
    状態を持たないので、デーモンを起こし直しても二重に入れ替えない。印が無いとき（初めて扱う）は入れ替えない。
  - 入れ替えは通常の操作。`disruptive: true` で破壊的として予算（§13）に数える。
  - List・prune は持たない。provider の設定はつなぎ方（`file_hosts`・`unit_hosts`・`remote_config`、`local` は手元）と
    ホストの上の置き場だけ。
  - 届かない・sudo が通らない・systemctl が答えないは unknown（absent にしない）。
- **Sakutei の中に「最後に入れ替えた hash」を持たなかった理由**: 観測 state レス（§3 の原則 1）に反し、デーモンを
  起こし直す・別のデーモンに移る（シャードのハンドオフ）と二重に入れ替える・入れ替え損ねる。ホストの印なら別の道具からも見える。
- **中身を観測に平文で載せなかった理由**: 観測は last-seen の環と drift の Event に入る（§20.2）。SecretRef で書いたファイルや、
  宣言の前から置いてあった秘密のファイルの平文を、宣言を見ない Read（ADR-0002）は区別できない。
- **見送り**: ユーザ単位のユニット・drop-in を入れ替えの印に入れること・quadlet の `ServiceName=`・ディレクトリの宣言。

## 受け入れテスト・統合テスト

- `internal/hostconf`：偽のホスト（`remotefake`）と偽の systemd（`internal/hostconf/systemdfake`）で、両方の provider の
  ContractTester（unreachable は unknown）、中身・mode の drift と復元、SecretRef の中身が plan・観測に出ないこと、
  1 回だけの入れ替え・印が無いときの扱い・書いたあと入れ替える前に止まった姿・reload と disruptive・書き方の誤り。
- `tests/acceptance/hostconf_test.go`：デーモンの経路（RunCycle）で `[AC-155-1]`〜`[AC-155-5]`（ファイルの収束と drift、
  依って立つファイルの書き換えで 1 回だけ入れ替わる・何周しても・デーモンを起こし直しても二重に入れ替えない、unknown は何もしない、
  disruptive は予算に数える、registry と手元のファイルを本物の CLI で収束）。
- `make integration-hostconf`（check・CI の外）：systemd を PID 1 で動かすコンテナに ssh で入り、通常のユニットと podman の
  quadlet で動くコンテナを収束させた。結果は `docs/providers/systemd-unit.md`。
