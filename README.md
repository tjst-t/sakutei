# Sakutei（作庭）

**オンプレミス基盤全体を対象とした、宣言的インフラ収束エンジン（コントロールプレーン）。**

電源/BMC → ネットワーク → FW/SDN → ストレージ → ハイパーバイザ → VM → ゲスト OS → コンテナ までを、ひとつの「あるべき姿（intent）」から level-triggered に収束させます。位置づけは **「Ansible の冪等性 + Kubernetes の収束ループ」**。

Kubernetes からは *収束原理*（level-triggered な収束ループ）だけを借り、実装機構（apiserver・etcd・scheduler・admission・CRD）は意図的に捨てています。最大の特徴は **観測 state レス** — キャッシュした「現在状態」を持たず、毎周期リアルを再観測します。これにより Terraform の three-way-diff / state ドリフト問題を**構造的に**排除します。

名称は平安期の作庭書『作庭記』に由来します。

- **ライセンス:** Apache-2.0
- **言語:** Go（1.22・標準ライブラリのみ、外部依存なし）
- **状態:** 設計 v3.57／ 実装 **v1 完成**（v1a・v1b・v1.5。最新のタグ `v2.0.1`、変更点は [CHANGELOG.md](CHANGELOG.md)）

---

## なぜ観測 state レスなのか

宣言的ツールの多くは「前回自分が書いた状態」をキャッシュし、それと現実・意図の3者を突き合わせます（three-way diff）。このキャッシュが現実とズレると、ドリフト検知が壊れ、`terraform refresh` のような救済が要ります。

Sakutei は**キャッシュを持ちません**。毎周期、

```
desired（intent を render）  ⇄  actual（provider が現実を Read）  →  diff  →  必要なら Apply
```

を回すだけです。「前回の状態」が無いのでズレようがなく、**触っていないのに戻る**（自己修復）が最小の機構で成立します。

---

## クイックスタート

```console
$ make build      # 単一バイナリ ./sakutei をビルド
$ make check      # build + vet + test（CI 相当）
$ make demo       # レビュー用の再現デモ（下記）
```

### `make demo` — GUI を持たない収束エンジンの「レビュー面」

GUI が無くても、`plan` / `status` の端末出力が**そのままレビュー面**です。`make demo` は実 provider（dnszone）を一時ディレクトリで動かし、次の物語を再生します:

1. `plan` — 宣言した2レコードが「これから作られる」ことを dry-run で見る
2. `reconcile` — 実体ファイル `zone.db` に収束
3. `status` — 適合レポート（`drifted=0`）
4. **誰かが `zone.db` を手で改ざん**（値を書き換え＋宣言外レコードを追加）
5. `status` — 改ざんを **cause B（ドリフト）** で検知、宣言外を **unmanaged** で提示
6. `reconcile` — **触っていないのに元へ戻る**（自己修復）

抜粋（ステップ5）:

```
════════ sakutei status — ドリフト検知（触っていないのに気づく）════════

drifted: 1 リソース

  DRIFTED dnszone//www.example.com./A
    原因タグ: B（ドリフト）
    provider 階級: TrueObserve=true List=true BulkObserve=false Pruneable=true ...
    理由: resource dnszone//www.example.com./A has 1 field(s) out of sync

unmanaged（宣言外・実在・prune 対象外）: 1 リソース
  注記: これらは現実に在るが intent に宣言が無い。既定（ExplicitAbsentOnly）では
  自動削除されない（prune 対象外・read-only）。削除するなら intent 側で明示する。
  UNMANAGED dnszone//rogue.example.com./A

要約: drifted=1 converged=1 unobservable=0 unmanaged=1
```

改ざんは `plan`/`reconcile`/`status` すべてで cause B に一致し、宣言外レコードは **prune されず残ります**（原則「確信が持てないとき、加算はしても減算はしない」）。

---

## CLI

```
sakutei              # 常駐して回り続ける（本来の姿）      --claim <scope-selector> [--fencing-store <ファイル> --shard-id <名前>]
sakutei plan         # dry-run: 何が変わるか（Diff で止まる）
sakutei reconcile    # 1 回収束（手動キック）              --expect-revision <rev>
sakutei status       # 適合レポート（drifted / unmanaged 一覧、read-only）
sakutei import       # 既存リアルを intent（YAML）として書き出す   --provider <名前> --out <dir> [--scope k=v]
sakutei sync         # mode: sync の scope の現実を generated/ に記録する（Apply しない）  --provider <名前> [--interval 30s] [--committer-name <名前>]
sakutei fence        # 各 scope の fencing 世代と holder を表示する（read-only）  --store <ファイル>
sakutei mirror       # 拠点が pull する読み取り専用の git ミラーを中央に追従させる  --upstream <url> --dir <dir> [--once]
sakutei secrets extract  # 中央の資格ファイルから 1 拠点分だけを抜き出す  --from <ファイル> --claim <selector> --out <ファイル>
sakutei apply-check list|release  # Apply を止めたリソース（oscillating）の一覧・人が解く  --state <ファイル>
```

**Apply の効きの確かめ（#166）**：Apply が成功を返したリソースは、その場で読み直して Diff が消えたかを確かめます（型ごとの settle の間は待つ。redfish-system は 15 秒）。消えなければ `ApplyIneffective` を出し、一回性の `reconcile` は終了コード **4** で終わります（0・1 の意味は変わらない。表は [docs/exit-codes.md](docs/exit-codes.md)）。同じ Diff で効かない Apply が上限（既定 3 回）に達したリソースには Apply を止め（`oscillating`）、status・/metrics・通知に出します。settle と上限は `--settle`・`--max-ineffective-applies`・`--max-ineffective-duration` と intent の `apply_check:` で変えられます（食い違えば厳しい方）。一回性で回をまたいで数えるには `--apply-check-state <ファイル>` を渡します。

**依存先の Apply のあとの観測し直し（#177）**：観測は Apply の前にまとめて行うので、FW の許可（依存先）とその奥のホストの上のリソース（依存元）を空から組むと、依存元は最初は届きません。依存先の Apply が効いたことを確かめたら、観測できなかった依存元だけを同じ回でもう一度観測し、届けばその回のうちに予算・承認・順序を通して Apply します（一回性の `reconcile` は 1 回で 0 で終わる）。常駐では、届いたら Interval を待たずに次の周を始めます。上限は `--reobserve-max`（1 つのリソースを観測し直す回数、既定 3、`0` で止める）と `--reobserve-time`（回の全体で使う時間、既定 30 秒）。届かなければ・上限に当たればいまどおり skip して次の回に任せます。

原因タグ（A〜E、§10.2）は前の回と比べて付けます。一回性の `plan`・`reconcile`・`status` は前の回の revision を `--cause-state <ファイル>` で持ち越します（無ければ初回として扱う）。常駐の `sakutei` は前の周をメモリに持つので要らず、`--cause-state` を渡すと理由を言って断ります（#165）。

`import` は現実を List と Read で観測し、`<dir>/imported-<provider>.yaml` に書き出します。直後の `plan` は差分ゼロになります。何度か観測して（`--samples`、既定 3 回）値が変わったフィールドには **managed 候補** の印を付けます。書き出すのは「現状のスナップショット」なので、人が読んで整理してから intent にしてください。全リソースは `deletion_policy: ExplicitAbsentOnly`（prune しない）で書き出され、secret の平文は書き出しません。

**Sync Mode（v1b）**：Sakutei の管理に切り替えるまでの間、人手運用を続ける scope は、intent のトップレベルの `scopes:` で `mode: sync` と宣言します（`scope`・`mode`・`sync.target_path`）。`sakutei sync` がその scope の現実を `target_path`（`generated/` の下）に自動生成ヘッダ付きで書き出し、変化があったときだけ書き換えます。sync の scope は収束しません。intent にリソースを書けず、prune の対象にもなりません。手で編集されたスナップショットは上書きしません。リポジトリが git の作業ツリーなら、書いたスナップショットをシステム（既定 `sakutei-sync <sakutei-sync@localhost>`、署名なし。`--committer-name` / `--committer-email` で変更可）としてコミットします。観測のたびに値が揺れるだけのフィールド（managed 候補）の変化ではコミットしません。人がコミットで変えたスナップショットは上書きしません。

**採番（v1b）**：VLAN などの番号はプールから払い出せます。intent のトップレベルの `pools:` にプール（`name`・`range: "100-199"`）を宣言し、フィールドに `from_pool: <プール名>` と書きます。`reconcile` と常駐ループは `--alloc-store <ファイル>` の採番記録から払い出し（同じリソースには常に同じ値）、`plan` と `status` は既存の割当を引くだけです（未割当は「割り当て待ち」と表示）。

**tier 別の Observe 頻度（v1b）**：常駐ループは既定で毎周すべてのリソースを Observe します。Observe が重い層（NETCONF・CLI・Ansible など）は、intent のトップレベルの `observe_tiers:` に tier（`name`・`observe_every: N`）を宣言し、リソースに `observe_tier: <名前>` と書くと、収束しているあいだは N 周に1回だけ Observe されます。初回・未収束（差分・エラー）・desired が変わった周は間引かずに Observe するので、遅れるのは安定したリソースの drift 検知だけです（最大 N−1 周）。`observe_tier` を書かないリソースは毎周のままです（Ansible executor の既定値も、executor を実装するまで「毎周」です）。キーを `tier:` にしていないのは、設計（§8）が `tier: foundation`（DAG 順序の優先度帯）にその名前を使っているためで、二つは別の設定です。

**静的シャーディング（v1b）**：常駐ループを `--claim site=tokyo` で起動すると、claim にマッチする scope のリソースだけを収束します。マッチしない scope は Observe もしません（prune の対象からも外れます）。シャードを複数動かすときは、全シャードで同じ `--fencing-store <ファイル>` を指定し、`--shard-id` でシャードごとに名前を付けます。シャードは claim 内の scope を初めて収束するとき、その scope の fencing 世代を1つ上げて自分を holder にします。Apply と Delete の直前には毎回、手元の世代がストアの現在世代と一致するかを確かめます。2つのデーモンが同じ scope を claim すると、後から世代を上げた方が勝ち、古い世代の方はその scope への書き込みを拒否されて `FENCED` を出します（その scope は Observe・Apply・prune のどれもしません）。fence されたシャードは、再起動するまでその scope を取り返しません。fencing ストアに届かない周は、何もせずに飛ばします。

再シャード（scope を別のシャードへ移す、§17.6）は、v1b では次の手動手順です（Workflow による自動化は v1.5）。

1. **drain**：旧シャードで対象 scope への新規 Apply を止め、in-flight の Apply が終わるのを待つ（`--mode alert-only` で再起動するなど）。
2. **claim 変更 PR**：シャード割当（各シャードの `--claim`）の変更をレビューしてマージする。
3. **fencing 世代の更新**：新シャードを新しい claim で起動する。新シャードが対象 scope の世代を上げ、旧シャードはその scope だけ fence される（ほかの scope の収束は続く）。`sakutei fence --store <ファイル>` で holder が新シャードに替わったことを確かめる。
4. **旧シャードの再起動**：旧シャードを新しい claim で再起動する。再起動しないと、旧シャードは対象 scope について `FENCED` を出し続ける。

**claim の git 配布と S2-free シャード（v1b）**：拠点のデーモンは `--intent` の代わりに `--intent-repo <url> --workdir <dir>` で起動すると、intent を git から取ります。リポジトリは次のレイアウト規約に従います（`<scope>` は `site=tokyo` のように `ラベル=値` を `,` で並べたもの）。

```
intent/platform/…        # 全シャードが read-only で参照する共有 platform
intent/site=tokyo/…      # claim {site: tokyo} のシャードが所有
shards/<シャード名>.yaml  # シャード割当（claim: の下に scope ラベル。空なら idle）
```

デーモンは partial clone（`--filter=blob:none`）と cone モードの sparse checkout で、`shards/`・`intent/platform/`・自 claim の `intent/<scope>/` だけを取得します（他拠点の intent は blob も手元に来ません）。中央 git は直接叩かず、`sakutei mirror` で作った読み取り専用ミラーを指し、`--git-poll`（既定 60s、最大 50% の jitter）ごとに pull します。ミラーに届かない間は前回 pull した intent で収束を続け、一度も pull できていない・自 claim の `intent/<scope>/` がコミットに無いときは周を飛ばします（空の desired で収束しない）。claim は `--claim` で固定するか、`--shard <名前>` で `shards/<名前>.yaml` から読みます。後者では **claim 変更 PR がそのまま開店/閉店のハンドオフ**です。中央の `shards/central.yaml` から scope を外して店舗の `shards/store-x.yaml` に足す PR をマージすると、旧担当は in-flight の Apply を待って手を引き（`claim released`）、新担当がその scope の fencing 世代を上げて収束を始めます。claim が変わったシャードは再起動と同じように収束し直すので、上の手順 4 の再起動は要りません。

`--s2-free` は S2-free シャード（Resource 収束のみ）の宣言です。拠点のディスクに置くのは `--workdir`（手元写しと render 出力）だけで、どちらも pull と収束で作り直せます。ホストが全損しても、再イメージ → 資格の再配布 → 同じフラグで起動、で元に戻ります。拠点ローカルの採番ストア（`--alloc-store`）は失うと困る状態なので、`--s2-free` では拒否します（中央の `--alloc-url` を使います）。

拠点の write 資格は `--secret-store <ファイル>`（file SecretBackend）に置き、そこには**その拠点の機器の資格だけ**を入れます（パスは `<scope>/…`、例 `site=tokyo/router/admin`）。他拠点の資格が 1 つでも入っていると、デーモンは収束しません。拠点用のファイルは `sakutei secrets extract --from <中央のファイル> --claim site=tokyo --out <拠点のファイル>` で作ります（モード 0600）。資格のローテーション（中央から拠点の資格を収束させる Workflow）は v1.5 です。

`apply` は意図的に主 verb にしていません（「いつ変えるか」は収束が決める）。`plan` と `status` を前面に置くこと自体が、このソフトのレビュー体験です。

---

## 実装済みの Provider

Provider は `Schema` / `Capabilities` / `Read` / `Apply` ＋ 集合列挙 `List`（prune の前提）を実装します。SDK が diff・secret マスキング・managed 除外・version 埋込・エラー分類・契約テスト・Schema lint を肩代わりするので、作者は数関数を書くだけです。

| 名前 | 種別 | 説明 |
| --- | --- | --- |
| `dnszone` | 実 | ゾーンファイルを宣言的現実として管理（RFC1035 サブセット、原子的書込、SOA/NS は管理外。書き換えたときは SOA のシリアルだけ上げる） |
| `nftables` | 実 | nftables ルールセットファイルを管理。**ordered ルール**（並べ替え＝変更）を実証 |
| `redfish-system` | 実 | BMC（Redfish）越しにサーバの電源（On / Off）と起動先の上書き（Continuous / Disabled）を管理。UUID・MAC・CPU・メモリは観測だけ。停止は既定で正常停止のみ、`power_off: graceful_then_force` のときだけ強制停止（予算に数える）。v1.1.0 で追加、[docs/providers/redfish.md](docs/providers/redfish.md) |
| `redfish-virtual-media` | 実 | BMC の仮想メディア（CD）の挿入・取り出しを `image` の宣言で管理。v1.1.0 で追加 |
| `redfish-bios` | 実 | BMC 越しに BIOS の設定を、宣言した属性だけ `Bios/Settings` で合わせる（効くのは次の再起動。保留中は converging で重ねない）。属性の登録簿で名前・型・値を計画の時点で確かめる。#140 |
| `redfish-firmware` | 実 | BMC の UpdateService の FirmwareInventory で部品ごとの FW の版を観測し、違えば承認ゲートつきの多相 Task（SimpleUpdate・BMC の Task の完了を待つ・宣言したときだけ再起動・版を観測して閉じる）で更新する。破壊的として予算に数える。統合テストは偽のサーバまで。#148 |
| `proxmox-vm` | 実 | Proxmox VE の VM を REST API（API トークン、secret は SecretRef）で宣言どおりに作り、CPU・メモリ・ディスク（大きくするだけ）・NIC・cloud-init・電源を合わせる。vmid は採番から配れる。再起動が要る変更は pending として重ねず、再起動は `apply_pending: reboot` か Workflow の action のときだけ。触らない VM（段0）を設定でき、宣言すれば計画のエラー。prune しない。#143、[docs/providers/proxmox-vm.md](docs/providers/proxmox-vm.md) |
| `sqlmigrate` | 実 | SQLite（または `engine: postgresql` で PostgreSQL）のデータベースに版つきのマイグレーション（`NNNN_<名前>.up.sql` / `.down.sql`）を流す。宣言は `version`、観測は `schema_migrations` の最大の版。`strategy: task` 専用で、常駐の sakutei（`--task-store`）が起動する Task だけが変える（一回性の reconcile は Apply しない）。`sqlite3`（PostgreSQL は `psql`。接続の設定は postgresql provider のサーバの一覧を使い、流す間は `pg_advisory_xact_lock` を取る）の CLI を呼ぶ。設定は `--sqlmigrate-config`、[docs/providers/sqlmigrate.md](docs/providers/sqlmigrate.md) |
| `ansible` | 実 | Ansible のモジュールを 1 つのホストに流す最小の階級の provider（§28.7）。観測は `--check --diff` の changed だけ（State を返さない）、適用は通常の実行。原因タグ・差分は粗く、prune しない。破壊的かは `destructive` で宣言する。設定は `--ansible-config`、[docs/providers/ansible.md](docs/providers/ansible.md) |
| `host-file` | 実 | 離れたホスト（ssh 越し、または手元）のファイルの中身（平文か SecretRef）・mode・owner・group を保つ。観測は中身の hash と stat だけ。prune しない。[docs/providers/host-file.md](docs/providers/host-file.md) |
| `systemd-unit` | 実 | 離れたホスト（または手元）の systemd のユニットと podman の quadlet の元のファイル・active / inactive・enabled を保つ。`restart_on` に書いたファイルを書き換えるとユニットを 1 回だけ入れ替える（ホストの側の印で判定）。[docs/providers/systemd-unit.md](docs/providers/systemd-unit.md) |
| `cli-device` | 実 | CLI でしか設定できない装置を ssh で CLI を打って観測し、コマンド列で変える汎用の provider。機種ごとの観測のコマンド・読み方・変えるコマンドの雛形・エラーの印・確定と取り消しは機種の定義のファイルで与える。定義に合わない出力は unknown、部分的な失敗はどこまで流れたかを知らせる。定義の操作を Workflow の action として流せる。[docs/providers/cli-device.md](docs/providers/cli-device.md) |
| `fake` | テスト | fault-injection 可能な検証用 provider（chaos テストの主戦場） |

---

## リポジトリ構成

```
.
├── cmd/sakutei/            # CLI エントリ（plan / reconcile / status / daemon）
├── pkg/                    # 公開 API
│   ├── core/               #   正準型（ID / Scope / State / Plan / Observation / SecretRef ...）
│   ├── provider/           #   Provider / Lister / BulkObserver / Capabilities / Schema
│   ├── sdk/                #   Provider 作者向け SDK（lint / 契約テスト / エラー）
│   └── registry/           #   provider 名 → ファクトリ
├── internal/               # 収束ループ・diff・cause・prune・budget・render・loader・secret ...
├── tests/acceptance/       # 受入テスト（[AC-...] タグ付き）・chaos スイート
├── docs/                   # 設計の正（下記）
├── Makefile                # build / test / vet / check / demo / clean
└── LICENSE                 # Apache-2.0
```

---

## 設計ドキュメント（source of truth）

コードは設計仕様の**射影**です。設計に関する記述は `docs/` を正とします。

- **[docs/README.md](docs/README.md)** — ドキュメント索引（まずここ）
- **[docs/architecture/spec/](docs/architecture/spec/)** — 設計の正 v3.57（8 部 29 節・分冊）。§番号は全体で一意
- **[docs/web/sakutei.html](docs/web/sakutei.html)** — 設計思想を庭園メタファで表したランディングページ
- **[docs/web/buildout.html](docs/web/buildout.html)** — 段0だけがある状態から、20台規模のポッド(リーフスイッチ→BMC→ハイパーバイザー導入→クラスタ構築)を1つの intent PR から積み上げる様子を追う構築ウォークスルー
- **[docs/web/scenarios.html](docs/web/scenarios.html)** — 稼働中の仮想基盤を題材に、増設/物理交換/FW更新/HVローリング更新が実際どう実行されるかを追う運用シナリオ集

### 設計を貫く八つの原則（§3）

1. **level-triggered + 観測 state レス** — 毎周期 desired と actual を再導出し、観測状態を保持しない
2. **いつ = 収束 / どう = 手続き** — 順序は収束が決め、変更手順は Task/Workflow が実行する
3. **確信が持てないとき、加算はしても減算はしない** — `unknown ≠ absent`、`pruneable:false` が既定、分断時は halt
4. **持たない・委譲する** — 値/配置/予測/認可は自前で持たず外部に委ねる
5. **seam は今切る、split は後** — 拡張点の interface を早期に定義し、コアは変えずに後で分割する
6. **ownership がすべてを貫く** — `platform`（厳格）か `tenant`（緩い）かの 1 軸が書込経路・remediation・blast-radius を決める
7. **シャード間協調は現実を介して行う** — シャードは問い合わせず、相手の作った現実を Observe して gate する
8. **衛星の停止は、変化を止めるが、維持を止めない** — 定常収束は衛星コールゼロで回る

---

## 実装スコープ

| スライス | 内容 | 状態 |
| --- | --- | --- |
| **v1a** | コア型・収束ループ・安全機構（累積予算・List 完全性）・早期収束/BulkObserve/--claim の型切り・最小 SDK・実 provider 2 個・devenv・secret（file backend）・loader・status・chaos | ✅ 完成 |
| **v1b** | import / Sync Mode（git コミットまで）、tier 別 Observe 頻度、採番連携（ファイル版・HA 最小形）、静的シャーディング（claim の git 配布・S2-free シャードを含む）、inventory 検証、BulkObserve（dnszone・nftables で実証） | ✅ 完成 |
| **v1.5** | 単純 Task ＋ 最小 Workflow（直列＋不変条件＋N-of-M 手動承認）＋ remediation schedule ＋ planned_change ＋ 運用プロファイル ＋ DR モード（break-glass） | ✅ 完成 |
| **v2+** | 多相 Task、自動段階ロールアウト/canary、DBSource、credential-broker、分散 Executor、intent UI、採番の SQLite 版、一括 API を持つ対象（Proxmox・vCenter 等）の provider | 予定 |

既知の v1 の限界（設計 §29 に記録）: 振動の検知と隔離は未配線（`StatusOscillating` は型のみ。破壊的な flip-flop は ghost-diff hold で抑制）／ Vault SecretBackend はスタブ（file backend は実装済み）／ 採番はファイル版（同一ホストでの同時払い出しは flock で直列化。SQLite 版は v2）／ BulkObserve は dnszone・nftables での実証まで（一括 API を持つ対象は Backlog #49）／ Event の出し先（sink）は標準エラー・JSON lines のファイル（`--event-log`）・webhook（`--event-webhook`）・Alertmanager（`--event-alertmanager`。webhook とも宛先を複数書ける、#154）。Alertmanager は本物を相手にした統合テストをまだ持たない（httptest まで）。firing / resolved の状態はメモリに持つので、デーモンを起こし直すと続いているものを改めて firing として出す。／ Redfish provider は qemu-bmc（0.9.0）を相手にした統合テストまでで、実機のサーバの BMC では確かめていない。IPMI は持たない。起動先の `Once` は宣言できない（Workflow の手順の `action: boot_once` で 1 回だけ施す）。

---

## 開発

```console
$ make build      # ./sakutei をビルド
$ make test       # 全テスト（acceptance・chaos 含む）
$ make vet        # go vet
$ make check      # build + vet + test
$ make demo       # レビュー用デモ
$ make integration-redfish  # Redfish の統合テスト（qemu-bmc + containerlab、KVM と sudo が要る。check には入らない）
$ make clean      # 生成物削除
```

外部依存はありません（標準ライブラリのみ）。ドキュメント・コメントは日本語です。

---

## ライセンス

[Apache-2.0](LICENSE)。OSS 個人プロジェクトとして作者個人が開発しています。
