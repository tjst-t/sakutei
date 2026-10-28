# 変更点（CHANGELOG）

Sakutei のリリースごとの変更点。設計書の改訂は `docs/architecture/CHANGELOG-v3.*.md` を見る。`#nn` は Backlog の項目番号。

## 未リリース

### 追加

- **トレースの出口**（#189）：`--otlp-endpoint <url>` を渡すと（常駐・`plan`・`reconcile`）、周・観測の段と Read・BulkObserve・plan の段・Apply（prune を含む）・Workflow とその手順を span にして OTLP/HTTP（JSON の形、`/v1/traces`）で出す。既定は出さない（`OTEL_*` の環境変数は読まない）。span には欄の値を載せない。送れなくても収束は止めず、1 度だけ知らせる。宛先の TLS と認可は `--otlp-config`（TLS 1.2+・CA・クライアント証明書と bearer トークンは SecretRef）で、loopback 以外へ TLS と認可を欠いて送るのは `--otlp-insecure` のときだけ。外の依存は増やしていない。文書は `docs/tracing.md`、統合テストは `make integration-otlp`（OpenTelemetry Collector contrib 0.163.2）。

## v2.8.0（2026-10-28）

本番に向けて権限と記録を固める版。観測用と書き込み用の資格を分け、nftables で root で走るものを決まった道具 1 つに絞り、監査の記録を鎖にして外へすぐ送れるようにし、秘密の取り出し先に Vault 形（KV v2）と外のコマンドを足す。設計書は v3.55 → v3.61（`docs/architecture/CHANGELOG-v3.56.md`〜`v3.61.md`）。

### 追加

- **観測用と書き込み用の資格を分ける**（#185）：ssh の対象の一覧（`remote_config`）のホストに `observe:`・`write:`（`user`・`private_key`・`sudo`）を書け、観測（`plan`・`status`・読み取りの口・毎周の観測）は観測用だけ、Apply・action は書き込み用だけを使う（nftables の live・dnszone・host-file・systemd-unit・cli-device・guest-os・netconf）。書き込み用の鍵は Apply の直前に取り出し、接続を使い回さない。書かない回（`plan`・`status`・`import`・`sync`・`graph`、`--mode alert-only`・`--freeze` の `reconcile`・常駐）は書き込み用の鍵を確かめず、無くても起動する。公開の口 `provider.WithWrite`・`provider.Writing`・設定のキー `access`（`provider.ConfigAccess`、`--provider-config` には書けない）。読むだけのユーザーで観測するときの要るものと観測できない項目は docs/remote.md。

- **監査の記録を固める**（#187）：監査のログ（`--audit-log`）にふつうの Apply と prune も Op ごとに残す（種別 `resource.apply`・`resource.prune`。resource・変えた欄の名前・cause・結果・レーンの承認者）。各行に通し番号 `seq` と前の行のハッシュ `prev` を持たせて鎖にし、`sakutei audit verify`（exit 3）が書き換え・抜き取り・差し込み・回したファイルの抜け・送った所を超える切り詰めを見つける。`--audit-rotate-bytes` で書き換え・切り詰めをせず新しいファイルへ回す。`--audit-syslog tcp://…`／`tls://…` で RFC 5424 の syslog へ 1 件ずつ送り、届かない間は手元に貯めて送り直す（`sakutei audit forward`）。

- **管理 API の provider の資格も分ける**（#186）：proxmox-vm・redfish・ldap・postgresql・s3-bucket の対象（クラスタ・BMC・サーバ・endpoint）に `observe:`・`write:` を書け（proxmox-vm は `token_id`・`token_secret`、redfish は `username`・`password`、ldap は `bind_dn`・`bind_password`、postgresql は `user`・`password`、s3-bucket は `access_key`・`secret_key`。書かないキーは対象の値）、観測は観測用だけ、Apply・action・Task の実行は書き込み用だけを使う。書かない回は書き込み用の secret が無くても起動する。観測用の資格に要る最小の権限（Proxmox の `PVEAuditor`、Redfish の `ReadOnly`、LDAP の read・search、PostgreSQL の `LOGIN`、S3 の読みの policy）は各 provider の文書。metrics-probe は観測だけなので資格は観測用のまま。
- **nftables：root で走るものを決まった道具 1 つに**（#191）：離れたホストの nftables（live）で、provider の設定 `ruleset_guard_tool` に決まった道具 `sakutei-nft-guard`（新しい実行ファイル `cmd/sakutei-nft-guard`）のパスを書くと、nft と巻き戻しの `sh -c` の代わりに道具だけを走らせる。道具は `list`・`check`・`apply`・`arm`・`disarm`・`status` の決まった形の引数だけを受け、扱う表はホストの root だけが書ける `/etc/sakutei/nft-guard.json` に書く（呼び手からは渡さない）。規則は行ごとに構造として読み、許した表の外に触れるもの・`flush ruleset`・`include` などが 1 行でもあれば何もせず断る。巻き戻しの写しも許した表だけを道具が取る。sudoers は書き込み用に道具だけ、観測用に道具の `list *` だけを許せばよい（`sh`・`nft` は許さない）。道具が無い・版が合わないときは base chain を変えない。使い方は `docs/providers/nftables.md` の「決まった道具」。
- **nftables：確かめる先の「変えたあとだけ確かめる」印**（#192）：base chain の宣言の `confirm` の先の前に `after:` を書くと（`self, after:ssh://admin@192.0.2.20:2200`）、その先は変える前には確かめず、変えたあとはほかの先と同じに必ず届くことを確かめ、届かなければ巻き戻す。変える chain を通る経路の先（境界の FW の forward・DNAT を通る別の経路の ssh）は空からの初回に変える前に届かず、一度も変えられなかったため。全部に印の付いた宣言・`after:self` は読み込みで断る。plan の `confirm` の行と前・後の確かめの知らせに印が出る。注意と避け方は `docs/providers/nftables.md` の「変える chain を通る経路の先」と `docs/network-policy.md`。

- **秘密の取り出し先**（#188）：`--provider-config` に `secret_backends:` を書き、SecretRef の取り出し先を足せる。組み込みは Vault 形の KV v2（`type: vault-kv2`。OpenBao で試験。token のファイルと AppRole、世代は KV の版）と外のコマンド（`type: command`）。`#gen` を書かない ref は render の段で世代を読んで入れ、版が上がれば差分（cause E）として Apply する。世代が読めなければそのリソースは unknown（ほかは収束する）。型は `pkg/cli.WithSecretBackend` で足せる（口は `pkg/sdk.SecretBackend`）。使い方は `docs/providers/secret-backends.md`、統合試験は `make integration-vault`。

> **振る舞いの変化**（#185）：host-file・systemd-unit・guest-os を root でない利用者で動かすとき、覗けないディレクトリの中のパスを「無い」でなく「観測できない」とするようになった（そのリソースは何もされない。これまでは在るファイルを作り直す計画を出していた）。

> **振る舞いの変化**（#187）：監査のログに Apply・prune の行と、各行の `seq`・`prev` が増える。既存のログにはそのまま続けて書ける。種別で絞らずに行を数えている道具は直す必要がある。

> **振る舞いの変化**（#186）：redfish-firmware・sqlmigrate の Task の実行は、資格を分けた設定では書き込み用の資格を使う。sqlmigrate の `engine: postgresql` の版の観測は観測用のロールで読む（版の表の SELECT が要る）。postgresql の `verify_passwords` は、`pg_authid` を読めない観測用のロールでは観測できない（unknown）。

## v2.7.0（2026-10-28）

正しい状態を宣言に置く原則を仕上げる版。provider の設定に残っていた管理の範囲と、長く効く起動の引数を intent に書けるようにし、freeze を誰が・なぜ・いつまでつきで監査に残す。設計書は v3.51 → v3.55（`docs/architecture/CHANGELOG-v3.52.md`〜`v3.55.md`）。

> **互換の注意**：`--freeze` は理由（`--freeze-reason`）と監査の記録先（`--audit-log`）が要るようになった（#184）。理由の無い `--freeze`・記録できない `--freeze` は、何も変えずに断られる。

### 追加

- **provider の範囲を intent に書く `providers:`**（#181）：provider の設定に残っていた管理の範囲——proxmox-vm のクラスタの `scope`、dnszone の `zone_scope`（`--zone-scope`）、nftables の `ruleset_scope`（`--ruleset-scope`）と live の `ruleset_tables`（`--ruleset-tables`）——を intent のトップレベルの `providers:` に provider ごとに書ける（`dnszone.scope`・`nftables.scope`・`nftables.tables`・`proxmox-vm.clusters.<名前>.scope`）。設定にだけあれば今どおり読んで警告し、両方にあって違えば読み込みで断る（常駐は起動で断り、周の途中で食い違えば周を飛ばす）。除外は `external_owners:` に寄せる。外の provider も `registry.RegisterIntentRanges` で同じ口を使える。設定から範囲を外すのは次のメジャー版。設計書 v3.53（`docs/architecture/CHANGELOG-v3.53.md`）。

- **残りの provider の範囲も intent の `providers:` に**（#182）：netconf の `hosts`・`subtrees`（旧 `netconf_hosts`・`netconf_subtrees`）、ldap の `scope`・`servers.<名前>.{base_dn, filter, scope}`、cli-device・host-file・systemd-unit の `hosts`（旧 `cli_hosts`・`file_hosts`・`unit_hosts`）、guest-os の `hosts`・`markers`（旧 `guest_hosts`・`guest_markers`）、postgresql の `scope`・`servers.<名前>.scope`、s3-bucket の `scope`・`endpoints.<名前>.bucket_prefix`。どれがつなぎ方（設定に残す）でどれが範囲（intent）かの一覧は [docs/providers/ranges.md](docs/providers/ranges.md)。移し方と食い違いの扱いは #181 と同じ。ldap の `base_dn` は intent に書けばサーバの一覧から外せる。設計書 v3.54。

- **長く効く起動の引数を intent の `runtime:` に書く**（#183）：予算・既定の mode・settle・効かない Apply と観測し直しの上限・stuck の周数・間隔を、intent のトップレベルの `runtime:` に書ける。旗だけ・宣言だけならそれを使う。大小のある引数と mode・`on_exceed` は、旗が締める向きなら旗を使い、level の Event `RuntimeTightened` と status の `runtime_tightened` に旗の値と宣言の値を両方出す。緩める向きなら起動を断る。向きの無い引数（`interval`・`settle`・`window_duration`・`reobserve_*`・`stuck_after`）は、食い違えば起動を断る。断るときは 1 行に、引数の名前・両方の値・直し方（旗を外すか、宣言を変えるか）を出す。常駐は起動したときの値で回り、途中で `runtime:` が変われば `RuntimeDeclarationChanged` と status の `runtime_changed` で知らせる。設計書 v3.52。

- **freeze を監査に残す**（#184）：global freeze の始まり・終わり・期限切れを、誰が・理由（必須）・期限（任意）つきで監査の記録に残す（種別 `freeze.started`・`freeze.ended`・`freeze.expired`）。入口は起動の旗 `--freeze`（`--freeze-reason`・`--freeze-by`・`--freeze-until`）と、動いている常駐を止めずに freeze する `sakutei freeze start / end / status`（状態のファイルを常駐と `reconcile` が `--freeze-state` で毎周読む）。効いている freeze への start は断る。期限を過ぎた freeze は次に読んだ者が解いて記録する。状態のファイルが読めない周は freeze として扱う。level の Event `Frozen` と status の `freeze` に出す。起動に失敗した常駐は freeze を記録しない。設計書 v3.55。

> **振る舞いの変化**：`--zone-scope`・`--ruleset-scope`・`--ruleset-tables`・proxmox-vm のクラスタの `scope`、#182 の範囲（`netconf_hosts`・`netconf_subtrees`・ldap の `base_dn`・`filter`・`scope`・`*_hosts`・`guest_markers`・postgresql と s3-bucket の `scope`・`bucket_prefix`）を設定にだけ書いていると、`plan`・`reconcile`・`status`・常駐が警告を出す（動きは変わらない）。

## v2.6.0（2026-10-28）

網の設定を宣言で扱う版。網の間の許可を機器に依らない抽象のリソースとして書き、nftables の既定の拒否（base chain の policy）を締め出しの守りつきで保ち、NETCONF の機器に commit confirmed の 2 段で設定を当てる。あわせて、空から 1 回で組めるよう依存先の Apply のあとに観測し直す。設計書は v3.44 → v3.51（`docs/architecture/CHANGELOG-v3.45.md`〜`v3.51.md`）。

> **振る舞いの変化**：一回性の `reconcile` は、依存先の Apply が効いたあとに、周の頭で観測できなかった依存元を同じ回でもう一度観測するようになった（#177）。これまで 2 回目で収束していた構成が 1 回で収束する（終了コード 0）。観測し直さないなら `--reobserve-max 0`。終了コードの意味は変わらない。

### 追加

- **網の間の許可 `network_policy:`**（#174）：intent に網（名前と CIDR）・許可（from の網・to の網・proto・ports）・実現先（`kind: boundary` か `host`）を書く。既定は拒否で、書いた許可だけが通る。許可の行ごとに理由 `reason` と印 `tags` を書け、`exception` の印の付いた許可に reason が無ければ読み込みで断る。網の重なり・宣言に無い網・同じ許可の重複も読み込みで断り、前の許可に隠れて効かない許可・どの実現先も通らない許可は warning として plan と reconcile に出す。実現先ごとに、そこを通る許可だけを nftables の base chain の宣言へ展開し、reason をルールの comment に出す（予算・承認・締め出しの守りがそのまま効く）。`sakutei plan` は展開と warning を出す。別の実現先（スイッチの ACL など）への変換は `pkg/cli` の `WithRealizer` で足せる（本体は持たない）。使い方は `docs/network-policy.md`。
- **nftables の base chain（既定の拒否）を宣言で保つ**（#173・#179）：intent が `type`・`hook`・`priority`・`policy` を書いた chain を base chain として扱い、作るときもずれを直すときも 4 つとルールを宣言どおりにする（宣言していない base chain は今どおり触らない）。live で base chain を変える Apply は締め出しの守りを通す：確かめる先（宣言の `confirm`：`self`・`ssh://…`・`tcp://…`）に変更の前に届くかを確かめ、届かなければ変えない → table の写しを取り、ホストの側に時間切れ（`ruleset_rollback_timeout`、既定 30 秒）で写しへ原子的に戻す仕掛けを置く → 1 つのトランザクションで流す → 新しい接続で全部の先に届くことを確かめてから仕掛けを取り消す。届かなければホストが戻し（Sakutei との接続が切れても戻る）、同じ差分の Apply を止める（終了コード 4）。base chain を作る・type/hook/priority/policy を変える・消す変更は壊す変更として予算に数え、承認ゲート（planned_change のレーン。kind に `create` を足した）を通す。到達性の最小の核は `external_owners:` か scope で外し、Sakutei の外で保守する。使い方は `docs/providers/nftables.md`。
- **NETCONF の 2 段の Apply**（#175）：`netconf` provider は `netconf_apply: "true"` を明示したときだけ Apply する（明示しなければ観測だけのまま）。`:confirmed-commit:1.0` だけの機器を主に、candidate へ部分木を置き換え → validate → 同じセッションで commit confirmed → 新しい ssh 接続で届くこと・宣言どおりなことを confirm-timeout の半分までに確かめる → 同じセッションで確定。確かめられなければ確定せず、機器の時間切れの巻き戻しに任せる。確定の成否が分からなければ unknown。1.1 を持つ機器には persist の流れ（`netconf_commit_flow: "1.1"`）もある。`:candidate` か `:confirmed-commit` を持たない機器には何も送らない。統合試験は netopeer2。
- **NETCONF を部分木だけに絞ったユーザーで動かす**（#176）：送る rpc は get-config・lock・unlock・edit-config・validate・commit・discard-changes・close-session だけで、宣言した部分木の外を読み書きしない。機器の側（NACM）で権限を絞ったユーザーで観測と Apply が動く。access-denied は理由つきで知らせ、何も確定しない。NACM の設定の例は `docs/providers/netconf.md`。
- **依存先の Apply のあとに、観測できなかった依存元を同じ回で観測し直す**（#177）：依存先の Apply が効いたことを確かめたあと、それに直接・間接に依存していて周の頭で観測できなかったものだけを観測し直し、届けばその回のうちに予算・承認・順序を通して Apply まで進める。上限は 1 つあたりの回数（`--reobserve-max`、既定 3）と回の全体の時間（`--reobserve-time`、既定 30 秒）。届かない・上限に当たればいまどおり skip。常駐は届けば次の周を待たずに始める。
- **HTTP の管理 API のパスの前置きを設定で変える `api_base_path`**（#160）：proxmox-vm（既定 `/api2/json`）と s3-bucket（既定は前置き無し）の provider の設定。

### 修正

- **nftables の無名の集合で、差分が消えなかった**（#178）：nft は集合の隣り合う・重なる範囲をまとめて書き戻す（例 `{ 10.0.0.0/24, 10.0.1.0/24 }` → `10.0.0.0/23`）ので、live の Apply の読み戻しが宣言と合わず止まっていた。アドレス・範囲・CIDR・ポートの集合を、範囲に展開してまとめた形どうしで比べる。
- **採番の書き戻しを review の仕方で使うとき、解放のあとの案内が誤っていた**（#167）：解放の書き戻しが取り込まれるまでの間、照合の知らせが `sakutei allocator import` を勧めていた（import すると cooldown と食い違って断られる）。「解放の書き戻しの取り込み待ち」と知らせ、import を勧めない。

## v2.5.0（2026-10-28）

FW とソフトの更新を宣言で扱う部品を揃えた版。管理 API に更新の口が無い装置の版を人の手順で合わせる、部品の版の互換の表、CLI でしか設定できない装置の汎用の provider、全体を止める型の更新。あわせて、/metrics と読み取りの口の TLS と認可。設計書は v3.38 → v3.44（`docs/architecture/CHANGELOG-v3.39.md`〜`v3.44.md`）。

> **互換の注意**：loopback 以外のアドレスで `--metrics-listen`・`--read-listen` を TLS なし・認可なしで待ち受けていた構成は、起動で断られる（#161）。`--listen-auth-config` で TLS と認可を設定するか、平文のままでよい場合は `--metrics-insecure-listen`・`--read-insecure-listen` を明示する。loopback で待ち受ける使い方はそのまま動く。

### 追加

- **/metrics と読み取りの口の TLS と認可**（#161）：`--listen-auth-config <file>` に口ごと（`metrics:`・`read:`）に、サーバの証明書と鍵（SecretRef）・TLS の版（1.2 以上）・認可を書く。認可は読むだけのトークン（`Authorization: Bearer`、SecretRef、定数時間で比べる）とクライアント証明書（CA で確かめ、任意で subject・SAN で絞る）のどちらか、または両方。証明書・鍵・トークンは再起動なしで読み直す。断ったら 401／403 で理由を外に出さず、`sakutei_listen_rejected_total` に数える。承認の書き込みの口とは同じアドレスを共有できない。使い方は `docs/metrics.md`・`docs/read-api.md`。
- **管理 API に更新の口が無い装置の版を、人の手順で合わせる**（#150）：どのリソースにも `update_policy.strategy: manual`（`procedure` か `procedure_url`、`confirm_timeout`〈既定 1h〉、`window`）を書ける。版が違えば承認ゲートつきの Task が手順の文・いまの版・望む版を知らせ、人が作業して承認すると、観測で宣言どおりになるのを待って閉じる。ならなければ stuck で知らせ、勝手に閉じない。承認しなくても観測で合えば閉じる。Sakutei は装置に何も書かない。
- **部品の版の互換の表**（#151）：intent の `compatibility:` に、部品（ID と版のフィールド）と許す版の組み合わせを書く。望む組み合わせが合わなければ plan・一回性の reconcile は観測の前に終了コード 1、常駐はその周を飛ばす。1 つずつ更新する途中の組み合わせも depends_on の順で照らし、合わなければ止めて（Event `VersionIncompatible`）、合う順序があれば depends_on の書き方を案内する。
- **CLI でしか設定できない装置を扱う汎用の provider `cli-device`**（#159）：ssh で装置の CLI を打って観測し、コマンド列で変える。機種ごとの中身（観測のコマンドと読み方・変えるコマンドの雛形・エラーの印・確定と取り消し・一度きりの操作）は使う側が持つ定義のファイルで与え、本体は機種を知らない。exec と対話（pty・プロンプト・ページ送り）の両方。値の形は必須で、差し込みになる値は読み込みで断る。出力が定義に合わなければ unknown で何もしない。Apply はエラーの印で止まり、どこまで流れたかを知らせ、取り消しを持つ定義では取り消す。定義の操作は Workflow の `action` としても流せる。使い方は `docs/providers/cli-device.md`。
- **全体を止める型の更新 `disruption:`**（#152）：Workflow に、止める時間の見積もり・影響を受ける下流・止めてよい時間帯・上流の積もりの条件・待つ上限・事前の知らせ・承認ゲートを宣言できる。時間帯の外では起こさず、上流の積もりが多い間は待ち、承認ゲートに見積もりと影響を出し、止める前に Event `Disruption` で知らせ、止める直前にもう一度確かめる（外れていれば知らせを取り下げて待つ）。見積もりを過ぎれば stuck。
- **proxmox-vm の cloud-init の初回のパッケージ更新 `ciupgrade`**（#170）：真偽値のフィールド。作るとき（作成・複製）に渡し、観測して違えば合わせる（cloud-init のほかのフィールドと同じ扱い）。書かなければ Proxmox に渡さず比べない（既定の振る舞いは変わらない）。外への出口が無い網では `ciupgrade: false` を明示すると、初回の起動がパッケージ更新の時間切れを待たない。`make integration-proxmox` に「ciupgrade: false」段。設計書 v3.43（`docs/architecture/CHANGELOG-v3.43.md`）。

### 修正

- **proxmox-vm の `apply_pending: reboot` の Apply が、効かなかったと数えられることがあった**（v2.4.0 の #166 との組み合わせ）：reboot の仕事が終わった直後は VM の status が少しのあいだ `stopped` と読めるため、Apply のあとの読み直しで Diff が残ったと見て、一回性の reconcile が終了コード 4 で終わることがあった。proxmox-vm に settle の既定 30 秒を入れ、その間は読み直して待つ（intent の `apply_check:` か `--settle` で変えられる）。`make integration-proxmox` の「再起動が要る変更」段で見つけた。

## v2.4.0（2026-10-28）

正しい状態を宣言の側に置く仕組みと、効かない Apply を止める仕組みを入れた版。採番の結果を宣言の側のファイルへ書き戻してそれを正として読む、触らない宣言を intent に書く、Apply のあとに読み直して効いたかを確かめる。あわせて、List の ID の scope と宣言の scope の突き合わせの修正と、Redfish の FW の更新を BMC の模擬で確かめたこと。設計書は v3.33 → v3.38（`docs/architecture/CHANGELOG-v3.34.md`〜`v3.38.md`）。

> **終了コードの追加**：一回性の `sakutei reconcile` に終了コード **4** を足した（下の #166）。**0 と 1 の意味は変わらない**（1 の条件と同時なら 1）。終了コードを見て分岐する包みは、4 を「収束していない」として扱うこと。コマンドごとの表は `docs/exit-codes.md`。

### 追加

- **Apply のあとに読み直して効いたかを確かめ、効かない Apply を止める**（#166）：Apply が成功を返したリソースはその場で読み直し（型ごとの settle の間は待つ。redfish-system は 15 秒、ほかは 1 回読み直す）、Diff が残れば「効かなかった」と数える。同じ Diff で `max_ineffective_applies`（既定 3）回効かなければ、そのリソースへの Apply を止める（`oscillating`。そのリソースだけ alert-only、下流は待つ）。Diff が変わる・現実が宣言どおりになる・人が `sakutei apply-check release` で解くと戻る。Event `ApplyIneffective`、status・`GET /status`・`GET /plan`・/metrics に出る。一回性の `reconcile` は、Apply が成功を返したのに Diff が残った・Apply を止めたリソースがあれば**終了コード 4**。旗 `--settle`・`--max-ineffective-applies`・`--max-ineffective-duration` と intent の `apply_check:`（食い違えば厳しい方）。BMC が電源の命令を受けて何もしない故障を `make integration-redfish` の段で確かめた（PASS）。
- **採番の結果を宣言の側へ書き戻し、それを正として読む**（#157）：`pools:` のプールに `write_back: true` と書くと、配った値をプールごとの素の YAML（`schema_version: 1`、書式は `docs/allocation-file.md`）に書き、Git にコミットする（`--alloc-write-back` で、直接コミット `commit`・別のブランチで人のレビューを通す `review`・書くだけ `file`）。読み込みはそのファイルを正とし、store と食い違えばその Apply を止めて知らせる（どちらにも寄せない）。store を失ったら `sakutei allocator import` でファイルから作り直せる（同じ値に戻り、付け替わらない）。store は写しと作業の記録に下がる。
- **触らない宣言 `external_owners:`**（#158）：intent に「この範囲は外の持ち主のもの」を provider に依らず書ける（provider・scope・ID の完全一致か前方一致・観測した値。proxmox-vm は `vmid`・`node`・`pool`）。選ばれたものは観測しても Apply・Delete・prune をしない。intent が同じものを管理・absent にすると読み込みで断る。provider の設定の除外（proxmox-vm の `protected_vmids` など）は最後の安全の網として残し、intent と食い違えば断る。原則「provider の設定はつなぎ方だけ、管理の範囲と除外は intent」を設計書 §1.7 に書いた。

### 修正

- **List の ID の scope が宣言の scope と合わず、status の unmanaged と prune の突き合わせが外れた**（#169）：List を持つ provider は ID に provider の設定の scope（無ければ root）を付けるので、宣言に `scope:` を書き provider に scope を書かないと、`status` で宣言済みのものが unmanaged に出て、`status --claim` では宣言の外のものが出ず、prune は claim の下で孤児を取りこぼしていた。突き合わせる前に scope を宣言に合わせる（provider に依らず本体の 1 か所）：宣言にあるものは宣言の ID に合わせ、宣言に無いものは status だけがその provider の宣言の scope（1 種類のとき）に入れる。provider の scope と宣言の scope が食い違えば、status は `unmanaged_undetectable` の `scope_mismatch`（終了コード 3）、prune は止まる。`--claim` で scope が決まらないものは `scope_undecided` の注記で件数を出す。設計書 v3.38（`docs/architecture/CHANGELOG-v3.38.md`）。`make integration-proxmox` に「scope 付きの宣言と --claim」段。
- **Redfish の FW の更新が、Task に `Payload` を返さない BMC で止まるはずだった**（#168）：BMC の更新の Task を、`Payload` が無ければ Update の登録簿のメッセージ `TargetDetermined` で見分ける。統合試験の BMC の模擬を qemu-bmc 0.11.0（digest で固定）に上げ、`make integration-redfish` に FW の段（版の観測・BMC の更新・BIOS の再起動までの保留・イメージの拒否と更新の失敗）を足した（PASS）。

## v2.3.0（2026-10-28）

宣言から外したもの・宣言で消すものを扱いやすくした版。宣言で消す口（`ensure: absent`）、宣言の外にあるものを status に出す、Proxmox の VM を pool に入れる。あわせて、/metrics の値を観測する provider。設計書は v3.29 → v3.33（`docs/architecture/CHANGELOG-v3.30.md`〜`v3.33.md`、と §29.1）。

### 追加

- **宣言で消す `ensure: absent`**（#163）：intent のリソースに `ensure: absent` と書くと、在れば消し、無ければ適合とする。provider に依らない汎用の口で、prune しない型（proxmox-vm の VM など）にも効く。消すのは必ず承認（そのリソースを覆う `planned_change` の `kind: delete`）を通り、予算にも数える。承認が無い間は消さず、Event `AbsentAwaitingApproval` を出し、plan では `blocked_by: approval`（`--json` の `absent: true`）。absent に中身のフィールドや依存を書く・absent に依るリソースがある・触らない宣言（proxmox-vm の protected）を absent にする、は読み込みで断る。proxmox-vm は走っている VM を止めてからディスクごと消す。`make integration-proxmox` に「absent で消す」段（PASS）。
- **Proxmox の VM を pool に入れる**（#165）：proxmox-vm のフィールド `pool`。作るとき（複製・作成）に渡し、違えば移す。pool は作らない。無い pool・権限の不足はそう言って止まる。pool に絞った API トークンに要る最小の権限を `docs/providers/proxmox-vm.md` に書いた。`make integration-proxmox` に「pool に入れて作る・pool に絞ったトークン」段（PASS）。
- **/metrics の値を観測する provider `metrics-probe`**（#149）：HTTP(S) の /metrics（Prometheus の text 形式）から、宣言したメトリクス（名前・ラベルの一致・sum / max）の値を観測し、比べ方（`"< 1000"` など）の結果を Workflow の `require` や `ready_condition` から待てる。観測だけ。届かない・形が壊れている・メトリクスが無いは unknown。使い方は `docs/providers/metrics-probe.md`。

### 修正

- **宣言の外にあるものが status に出なかった**（#164）：ある provider の宣言をすべて外すと、その provider は List されず、宣言の外のもの（unmanaged）が 1 つも出なかった。status は、宣言の無い provider も設定されていれば List し、宣言に無いものを unmanaged に出す（見せるだけで消さない）。設定されているのに組み立てられない provider は `unmanaged_undetectable`（終了コード 3）。`make integration-proxmox` に「宣言の外の VM が unmanaged に出て、消されない」段（PASS）。
- 常駐のデーモンに `--cause-state` を渡すと、旗の解析のエラーではなく、理由（常駐は前の周をメモリに持つので要らない）を言って終了コード 1 で断る。

## v2.2.0（2026-10-28）

離れたホストのファイルと systemd のユニットを宣言で保てるようにし、通知を複数の宛先へ送れるようにした版。あわせて、Redfish の FW の版の観測と承認ゲートつきの更新。設計書は v3.26 → v3.29（`docs/architecture/CHANGELOG-v3.27.md`・`v3.28.md`・`v3.29.md`、と §29.1）。

### 追加

- **離れたホストのファイル `host-file` と systemd のユニット `systemd-unit`**（#155）：ssh 越しの実行の層（#130）の上で、ファイルの中身（宣言の文か SecretRef）・mode・owner・group と、ユニット（podman の quadlet を含む。`app.container` → `app.service`）の中身・state・enabled を宣言どおりに保つ。観測は中身の sha256 と stat だけで、平文を状態やイベントに載せない。**書き換えたら入れ替える口**：`restart_on` に依って立つファイルを書くと、その中身が前の起動のときと変わったときだけユニットを 1 回入れ替える（ホストの側の印で level に判定）。ユニットのファイルを書き換えたら daemon-reload を 1 回。`disruptive: true` は予算に数える。`make integration-hostconf`：systemd を PID 1 で動かすコンテナで、ふつうのユニットと podman 5 の quadlet を確かめた（PASS）。使い方は `docs/providers/host-file.md`・`systemd-unit.md`。
- **通知の宛先を複数に**（#154）：`--event-alertmanager`・`--event-webhook` は、フラグを繰り返すか `,` でつないで複数の宛先を書ける（1 つの書き方はそのまま）。`--event-notify-config` の `alertmanager.urls`・`webhook.urls` にも書ける。宛先ごとに送り直し・backoff・送り待ちを持ち、1 台が落ちても他へは送り続け、戻った宛先には待っていた分と firing の今を送る。宛先ごとの送りの状態を `/metrics`（`sakutei_notify_*{sink,url}`）と status の `notify` に出す。
- **Redfish の FW の版 `redfish-firmware`**（#148）：UpdateService の FirmwareInventory の版を観測し、宣言した版と違えば、承認ゲートつきの多相 Task（確かめる → 送る → 当てる → 宣言したときだけ再起動 → 確かめる）で更新する。イメージの sha256 を承認の後で確かめ、各相の入口で版と BMC の Task を観測して二重に送らない。版の違いは破壊的な変更として予算に数える。試験は偽の Redfish サーバまで（統合試験に使う BMC の模擬が UpdateService を持たないため、模擬・実機での確かめはまだ）。使い方は `docs/providers/redfish.md`。
- **Ansible の例**：podman と systemd の quadlet でサービスを配る例を `docs/providers/ansible.md` に足した（例そのものは試していない）。

### 修正

- 通知の口：閉じる直前に積まれた知らせを送らずに終わることがあった（一回きりの reconcile で Alertmanager・webhook に最後の知らせが届かないことがある）。閉じるときは、積まれた分を送り切ってから止まる。
- Proxmox の統合試験：一時ディレクトリとキャッシュが別のファイルシステムでも、自動導入の ISO を移せるようにした。
- LDAP の統合試験：一時のパスワードファイルの消し残しを、その試験だけの一時ディレクトリで確かめるようにした。

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
