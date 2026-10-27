# 変更点（CHANGELOG）

Sakutei のリリースごとの変更点。設計書の改訂は `docs/architecture/CHANGELOG-v3.*.md` を見る。`#nn` は Backlog の項目番号。

## 未リリース

設計書は v3.17 → v3.20（各 `docs/architecture/CHANGELOG-v3.*.md`）。

### 追加

- **外の provider を足す口**（#119、設計書 v3.20 §28.9）：CLI の入口を `pkg/cli` に出した。外のモジュールが `cli.Main(cli.WithProviders(...))` で自前の provider を登録した自前のバイナリを作れる。provider ごとの設定は `--provider-config <file>`。例は `examples/external-provider/`。
- **`sakutei status --json`**（#118）：結果を JSON で出す。形は `docs/status-json.md`。
- **`status`・`plan`・`reconcile` の `--inventory-file`**（#115）：デーモンと同じ inventory の合成をする。
- **`sakutei allocator release`・`list`**（#116）：採番を人が CLI で解放する・一覧を見る。

### 変更

- `sakutei status` は観測エラーで止まらず、そのリソースを観測できないものとして数えて最後まで報告する。観測できないものがあれば終了コード 3（#117）。

## v1.1.0（2026-10-22）

v1.0.0 を手で試して見つかった不具合の修正と、知らせ（Event）の作り直し、Redfish の BMC provider の追加。設計書は v3.11 → v3.16。

### 追加

- **Redfish の BMC provider**（#48、#80〜#84。設計書 v3.15・v3.16 §29.1）
  - `redfish-system`：電源（`power: On / Off`）と起動先の上書き（`boot_enabled: Continuous / Disabled`、`boot_target`）を管理する。UUID・MAC・CPU 数・メモリ・起動モードは観測だけの値。
  - 停止は既定では正常停止（GracefulShutdown）だけ。止まらなければ強制停止せず、stuck として知らせる。`power_off: graceful_then_force` と `force_after` を書いたときだけ強制停止（ForceOff）に進み、強制停止は予算（`--max-destructive-per-cycle`）に数える。
  - 起動先の `Once` は計画の時点でエラーにする（1 回起動すると BMC が戻すため、あるべき状態にならない。将来 Task 側で扱う）。
  - 電源の切り替え中（PoweringOff / PoweringOn）は操作を重ねない。BMC に届かないときは Unknown として何も操作しない。
  - `redfish-virtual-media`：仮想メディア（CD）の `image` を宣言し、挿入・取り出し・差し替えで合わせる。ID は `<ノード名>/cd`。
  - BMC の一覧は `--redfish-config <file>`。パスワードは SecretRef で渡し、値を設定や intent に書かない。自己署名の証明書は明示したときだけ許す。Go の標準ライブラリだけで書いている。
  - `make integration-redfish`：qemu-bmc 0.9.0 を containerlab で 2 台立て、収束・手で変えた電源を戻す・既定の停止で stuck・`graceful_then_force` で強制停止・BMC 停止で無操作・`Once` の拒否を通しで確かめる（KVM と sudo が要る。`make check` には入らない）。使い方は `docs/providers/redfish.md`。
- **知らせ（Event）の作り直し**（#74、#77〜#79。設計書 v3.14 §22.3）
  - 知らせを 1 つの Event の型から出す。`--log-format json` で JSON lines にできる。
  - `--event-log <path>` で、知らせを JSON lines のファイルにも書く。
  - ずれ・Apply の失敗・halt を、毎周同じ文で出さず、始まったとき（firing）と消えたとき（resolved）に 1 回ずつ出す。続いた周数と始まった時刻を付ける。既定で 5 周続くと stuck に上げる（`--stuck-after` で変える）。
- **dnszone の SOA シリアル**（#70、設計書 v3.12）：管理するレコードを書き換えたときだけ、SOA のシリアルを `YYYYMMDDnn`（UTC）で上げる。今の値より必ず大きくする。`--zone-soa-serial keep` で上げないようにできる。
- 外部入力に届かずに飛ばした周の数を、ログと `status` に出す（#15）。
- 試用の手順 `docs/trial/README.md`（例の intent と `run.sh`、デーモンの確かめ方と結果）。

### 修正

- nftables：Apply で base chain や set・map の行を消していた。管理する chain の行だけを書き直す（#68）。
- dnszone：`$ORIGIN` と `@` を解いて名前を FQDN で比べる。ID を `<FQDN>/<TYPE>` にそろえた。前は絶対名で書くとレコードが二重にできた（#69）。ID は小文字にそろえ、大文字を含む ID はロードで拒否する（#71）。
- デーモン：資源ごとに provider を振り分ける。plan・reconcile に `--ruleset` を足した（#67）。
- dnszone・nftables：デーモンが並行して Apply すると変更を取りこぼすことがあった。ファイルごとの排他（mutex と flock）を入れた（#72）。
- 予算や prune の安全装置で止めた周、prune で消した周、観測できず飛ばした資源がある周に、要約が「converged」と出ていた。止めた件数・消した件数・飛ばした件数を出す（#73・#76・#85）。観測状態（Availability）は数字ではなく名前で出す（#85）。
- 急減の判定：Sakutei 自身が prune で消した分（予算と承認を通ったもの）を、次の周の判定で差し引く。前は自分の削除を急減と見なして止まり続けた（#75、設計書 v3.13 §12.4（b））。
- 非同期の wave で、in-flight の上流の下流を同じ周に起動していた（#12）。
- 破壊的でない in-flight の Apply を、破壊的な操作の上限に数えていた（#13）。
- cause D（親の変更による差分）を、変えた親の子にだけ付ける（#14）。
- Sync Mode：シンボリックリンクを解いてから相対パスを取る（#50）。

### 既知の限界（v1.0.0 からの変わりなし、と追加）

- 振動の検知と隔離は未配線。Vault の SecretBackend はスタブ。採番はファイル版（SQLite 版は v2）。BulkObserve は dnszone・nftables での実証まで。
- Event の出し先は標準エラーと `--event-log` のファイルだけ。firing / resolved の状態はメモリに持つので、デーモンを起こし直すと続いているものを改めて firing として出す。
- Redfish provider は qemu-bmc を相手にした統合テストまでで、実機のサーバの BMC では確かめていない。IPMI は持たない。

## v1.0.0（2026-10-21）

v1（v1a・v1b・v1.5）の完成。設計書 v3.11。
