# 変更点（CHANGELOG）

Sakutei のリリースごとの変更点。設計書の改訂は `docs/architecture/CHANGELOG-v3.*.md` を見る。`#nn` は Backlog の項目番号。

## v2.1.0（2026-10-28）

仮想マシンとディレクトリを宣言で扱えるようにした版。Proxmox VE の VM、LDAP の項目、採番した値を IP アドレスなどの文字列にして埋め込むテンプレート。設計書は v3.25 → v3.26（`docs/architecture/CHANGELOG-v3.26.md`）と §29.1。

### 追加

- **Proxmox VE の VM provider `proxmox-vm`**（#143）：REST API と API トークン（SecretRef）で、VM・CPU・メモリ・ディスク（大きくするだけ）・NIC（bridge・VLAN）・cloud-init（利用者・ssh の公開鍵・IP・GW・DNS）・電源を宣言で合わせる。テンプレートから full clone で作り、vmid は宣言か採番から配る。再起動を待つ変更（pending）を重ねて書かず、再起動は宣言したとき（`apply_pending: reboot`）か Workflow の操作のときだけ。触らない VM を設定で除外できる（List に出さず、宣言すれば計画のエラー）。VM は prune しない。`make integration-proxmox`：Proxmox VE 9.2-1 を入れ子の KVM に自動導入し、Debian 13 のクラウドイメージのテンプレートから作った VM に cloud-init の鍵とアドレスで入れることまで確かめた（PASS）。使い方は `docs/providers/proxmox-vm.md`。
- **LDAP provider `ldap`**（#144）：OpenLDAP のクライアント（`ldapsearch`・`ldapmodify`）を呼び、項目（DN）の宣言した属性だけを値の集合ちょうどに合わせる。DN は正規形で扱い、`userPassword` は書き込みだけ（SecretRef）。LDAPS・StartTLS、証明書は必ず確かめる。子のある項目は消さない。`make integration-ldap`：389 Directory Server で、作る・手の変更が戻る・何周しても drift が出ない・止めると unknown を確かめた（PASS）。使い方は `docs/providers/ldap.md`。
- **採番の値のテンプレート**（#145、#108 の 1）：採番の要求に `template: "192.0.2.{n}"` を書くと、払い出した整数を埋め込んだ文字列がフィールドに入る（採番の記録は整数のまま）。`kind: ipv4` / `ipv6` を書くと、プールの範囲のすべての値がアドレスになることを読み込みで確かめる。

## v2.0.1（2026-10-27）

### 修正

- **Go のモジュールとして取り込めるのは v2.0.1 から**。module のパスを `github.com/tjst-t/sakutei/v2` にした。Go の決まりでは、メジャー版を 2 以上にするとき module のパスに `/v2` を付ける。v2.0.0 は付け忘れていたため、`go get` で取れなかった（`go get github.com/tjst-t/sakutei@v2.0.0` も `.../v2@v2.0.0` も断られる）。v2.0.0 は使わず、v2.0.1 を使うこと。
  - 移り方：`go get github.com/tjst-t/sakutei/v2@v2.0.1` で取り、import のパスを `github.com/tjst-t/sakutei/pkg/...` から `github.com/tjst-t/sakutei/v2/pkg/...` に変える（例 `github.com/tjst-t/sakutei/v2/pkg/cli`）。go.mod の `require github.com/tjst-t/sakutei vX` は `require github.com/tjst-t/sakutei/v2 v2.0.1` に変える。
  - 中身（動き・フラグ・終了コード）は v2.0.0 と同じ。
  - 再発防止：CI に、外のモジュールから `go get github.com/tjst-t/sakutei/v2@<版>` をして examples/external-provider を build する確かめを足した（`make check-module VERSION=<版>`、タグではその版・main ではその commit）。リリースの前の点検にも入れる。

## v2.0.0（2026-10-27）

> **注意：この版は Go のモジュールとして取り込めない**（module のパスに `/v2` が無い）。Go のモジュールとして取り込めるのは v2.0.1 から。中身は v2.0.1 と同じ。

設計で v2 に置いていた核の 1 つ、**多相 Task**（相を持ち、リブートをまたぐ手続き）が入った版。あわせて、空の物理サーバに宣言だけで OS を入れて完了を確かめる部品、入れ直し、BIOS の設定、NETCONF 対応の機器の観測、外の道具が状態を読む読み取り専用の口。設計書は v3.23 → v3.25（`docs/architecture/CHANGELOG-v3.24.md`・`v3.25.md`、と §29.1）。

### 追加

- **多相 Task**（#51、設計書 §6.2）：相（Staging・Verifying・Applying・Rebooting・Confirming など）を持つ Task。進行中の相だけを S2 に残し、各相の入口で「もう済んでいるか」を観測で確かめて二重に実行しない。リブート中に見えなくなった対象を absent と取り違えず、時間を区切って待つ。FW の更新のような手続きの土台。
- **書き込み中に止まった多相 Task は人の承認に回す**（#52）：Applying の途中で止まった Task は、観測で完了を確かめられない限り自動で再開せず、承認ゲートで止まる。
- **Workflow の一度きりの操作と自動導入の応答ファイル**（#139）：手順に「次の 1 回だけ CD から起動」「再起動」を書ける（収束の宣言で Once を断る決まりは変えない）。自動導入の応答ファイルを、利用者の雛形と宣言から書き出す（公開鍵だけで、秘密は書き出さない）。
- **宣言で入れ直す**（#88）：planned_change の `kind: reimage` を宣言すると、対象の台で OS の導入の Workflow が 1 回走る（予算・承認を通る）。
- **Redfish の BIOS 設定 `redfish-bios`**（#140）：宣言した属性だけを `Bios/Settings` で合わせ、再起動待ちの変更を二重に出さない。
- **NETCONF provider `netconf`（観測だけ）**（#141）：NETCONF 対応の機器の設定の部分木を観測し、ずれを知らせる。変更は型で止める（後で commit confirmed を使う 2 段の変更を入れる）。`make integration-netconf`。
- **`plan --json`**（#135）と、**デーモンの読み取り専用の HTTP**（#136）：`--read-listen` で `GET /status`・`/plan`・`/events`（Server-Sent Events）・`/graph`。`sakutei graph --json`。status に担当（claim）・fencing の世代・ミラーの状態。

### 修正

- postgresql：管理用の接続の秘密が読めないときの Apply を、ほかの provider と同じく一時的な失敗（安全に止まる側）にした。権限の空と、宣言から外したときの扱いを文書に明記した（#137）。

## v1.4.0（2026-10-27）

1 台のデーモンから、agent を置かずに離れたホストを扱えるようにした。ゲスト OS を観測して OS の導入の完了を確かめる provider、デーモンの /metrics、s3-bucket の S3 互換実装への対応。設計書は v3.22 → v3.23（`docs/architecture/CHANGELOG-v3.23.md`）。

### 追加

- **離れたホストを扱う実行の層**（#130、設計書 v3.23 §29.1）：外部コマンドとファイル操作を、手元か ssh 越し（OpenSSH の `ssh` CLI を exec、Go の依存は増やさない）で流す。ホスト鍵は設定の known_hosts で確かめ、知らない鍵は拒む。接続を使い回す。秘密鍵は SecretRef。nftables（live）と dnszone がこの上で動き、1 台のデーモンで複数のホストを扱える。dnszone は書き換えのあとの再読み込みのコマンドを設定できる。届かない・鍵が合わない・ホスト鍵が変わったときは観測できない（unknown）として何もしない。`make integration-remote`。使い方は `docs/remote.md`。
- **ゲスト OS の観測 `guest-os`**（#86）：ssh 越しに到達・OS の識別・導入の目印を読む（観測だけ）。Workflow の `require`・`wait` に使い、OS の導入の完了を「電源が切れた」ではなく「ゲスト OS が目印つきで起動した」で判断できる。インストーラが失敗したときは先へ進まない。`make integration-guestos`。
- **デーモンの `/metrics`**（#54）：Prometheus の text format で、リソースの状態ごとの件数・周と観測の所要時間・予算の残り・halt・発火中の Event などを出す（待ち受けは明示したときだけ）。
- **s3-bucket の S3 互換実装への対応**（#132）：endpoint ごとに扱う設定の一覧 `fields`（一覧に無い設定は読みに行かない）と、StorageClass の名前の読み替え `storage_class_aliases`。

## v1.3.1（2026-10-27）

PostgreSQL provider の、ずれがいつまでも直らなかった穴の修正と、Event を通知に送る口。

### 修正

- **PostgreSQL provider：PUBLIC への権限**（#133）：データベースの権限の対象に `PUBLIC` を書けるようにした。宣言した一覧ちょうどに合わせ、PUBLIC に限り空の一覧で全部外せる（新しいデータベースに既定で付く CONNECT・TEMPORARY も外せる）。手で PUBLIC に付けた権限も drift として見えて戻る。宣言していない PUBLIC の権限は prune で外さない。
- **PostgreSQL provider：ロールの `bypassrls`・`replication`**（#133）：宣言すれば観測して直す。手で `BYPASSRLS` を付けられても見えなかった。

### 追加

- **通知の口**（#131、設計書 §22.3）：`--event-webhook` で Event を JSON のまま POST し、`--event-alertmanager` で level の Event（firing・resolved）を Alertmanager の alerts に写して送る。送れなくても収束は止めない。

## v1.3.0（2026-10-27）

PostgreSQL と Ansible の provider、sqlmigrate の PostgreSQL 版、Workflow で複数の provider を扱う口。設計書は v3.21 → v3.22（`docs/architecture/CHANGELOG-v3.22.md`）。

### 追加

- **PostgreSQL provider `postgresql`**（#125、設計書 v3.22 §29.1）：ロール・データベース・データベースの権限（CONNECT・CREATE・TEMPORARY）・拡張を宣言で合わせる。`psql` を exec で呼び、Go の依存は増やさない。パスワードは SecretRef で渡し、SCRAM-SHA-256 の検証値にしてから送る（平文はサーバのログにも出ない）。利用者の表があるデータベースは消さない。`make integration-postgresql`。使い方は `docs/providers/postgresql.md`。
- **sqlmigrate の PostgreSQL 版**（#126）：データベースごとに `engine: postgresql` を選べる。1 本ずつトランザクションで流し、流す間は `pg_advisory_lock` で他のデーモンと排他する。
- **Ansible provider `ansible`**（#127、§28.7 のロングテール層）：1 つのホストへの 1 つのモジュールの呼び出しを、`--check --diff` で観測し、通常実行で適用する。最小の階級（State を返さない）なので、原因タグ・prune などの保証は落ちる（`docs/providers/ansible.md` に正直に書いた）。`make integration-ansible`。
- **Workflow の手順ごとの対象と前提条件**（#87、設計書 §6.3）：手順に `resource: <provider>/<id>` を書くと、Workflow の target と別の provider のリソースを触れる。`require`（観測で満たすまで待ち、timeout で halt）で順番を保証できる。OS の導入の「CD を差す → 差さったことを確かめる → CD 起動で電源オン」を 1 つの Workflow に書ける（`make integration-redfish` に追加）。

### 変更

- Redfish の統合テストの qemu-bmc を 0.10.0（tag と digest）に上げた。

## v1.2.0（2026-10-27）

外の provider を足す口、実際の FW を扱う nftables、S3 互換のバケット provider、SQLite のマイグレーションを流す単純 Task の provider（sqlmigrate）と、v1 を手で試して見つかった不具合の修正。設計書は v3.16 → v3.21（各 `docs/architecture/CHANGELOG-v3.*.md`）。

### 追加

- **sqlmigrate provider**（#92〜#95、設計書 v3.17 §29.1）：SQLite のマイグレーションを単純 Task（`strategy: task`）として流す。`sqlite3` の CLI を呼び、`schema_migrations` の最大の版を観測し、up / down を 1 本ずつトランザクションで流す。`--sqlmigrate-config`。使い方は `docs/providers/sqlmigrate.md`、試用の結果は `docs/trial/sqlmigrate/`。
- **外の provider を足す口**（#119、設計書 v3.20 §28.9）：CLI の入口を `pkg/cli` に出した。外のモジュールが `cli.Main(cli.WithProviders(...))` で自前の provider を登録した自前のバイナリを作れる。provider ごとの設定は `--provider-config <file>`。例は `examples/external-provider/`。
- **`sakutei status --json`**（#118）：結果を JSON で出す。形は `docs/status-json.md`。
- **`status`・`plan`・`reconcile` の `--inventory-file`**（#115）：デーモンと同じ inventory の合成をする。
- **`sakutei allocator release`・`list`**（#116）：採番を人が CLI で解放する・一覧を見る。
- **nftables の実の ruleset 版**（#121、設計書 v3.21 §29.1）：`--ruleset-target live` で、カーネルの実際の ruleset を `nft -j list` で観測し、`nft -c -f` で検査してから `nft -f` で一括して適用する。管理する table は `--ruleset-tables` で必ず指定する。管理外の table・base chain・set には触らない。`make integration-nftables`（使い捨ての network namespace）。使い方は `docs/providers/nftables.md`。
- **S3 互換のバケット provider `s3-bucket`**（#122・#123、設計書 v3.21 §29.1）：versioning・policy・lifecycle・tags を宣言で合わせる。署名（SigV4）は標準ライブラリだけで実装。中身のあるバケットは消さない。`make integration-s3`（SeaweedFS が必須の相手、ほかは取れなければ飛ばす。イメージは tag と digest で固定）。使い方は `docs/providers/s3-bucket.md`。

- 機能ごとの試用の手順と結果（#97〜#104、`docs/trial/`）と、Redfish と Workflow で OS を入れる手順書（#91）。

### 変更

- `sakutei status` は観測エラーで止まらず、そのリソースを観測できないものとして数えて最後まで報告する。観測できないものがあれば**終了コード 3**（#117）。今までは 1 だった。
- CLI の中身を `pkg/cli` に移した（#119）。`sakutei` コマンドの verb・フラグ・終了コードは（上の status を除き）変わらない。
- `strategy: task` の知らせ：自分が起動した Task が走っている周はずれを数えず stuck に上げない。起動しない理由（窓の外など）は周をまたいで 1 回だけ出す（#112、設計書 v3.18）。

### 修正

- 採番の Lookup キャッシュに有効期限（既定 15 分）を入れ、期限を過ぎたものは使う前に確かめ直す。手で解放された値を知らずに使い続けて、cooldown 明けに別の持ち主と衝突しうる穴を塞いだ（#105、設計書 v3.19）。
- dnszone・nftables の List が常に root の scope で ID を返していたため、root 以外の scope で `PruneWithinScope` と Sync Mode が効かなかった。provider ごとに固定の scope を設定できるようにした（`--zone-scope`・`--ruleset-scope`、#106、設計書 v3.19）。
- plan・status が Workflow に任せている対象を普段の差分として出していた（#89）。Workflow の説明の例が intent の YAML で読めない書き方だった（#90）。
- Apply が失敗した reconcile の要約が「Converged」と言っていた（#110）。試用で見つけた表示の細かな不具合（#111）。task list に rollback の有無と戻した版を出し、周の要約に Task の起動数を出す（#113）。

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
