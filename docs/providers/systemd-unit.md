# systemd-unit provider

離れたホスト（または手元）の systemd のユニットを宣言どおりに保つ provider（`systemd-unit`、Backlog #155）。通常のユニット（`.service`・`.timer` など）と podman の **quadlet**（`.container`・`.volume`・`.network`・`.kube`・`.pod`・`.image`・`.build`）の元のファイル、動いているか（active / inactive）、通常のユニットの enabled を合わせる。**依って立つファイルを書き換えたらユニットを 1 回だけ入れ替える口**（`restart_on`）を持つ。ssh 越しの実行の層（[remote.md](../remote.md)、#130）の上で動き、設定ファイルは [host-file](host-file.md) で書く。決まりの正は設計書 §29.1「離れたホストのファイルと systemd のユニット」。

- 標準階級（観測は本当の姿）。List・BulkObserve を持たず、prune しない：管理するのは intent に書いたユニットだけ。ユニットを消すこともしない。
- 入れ替えの判定はホストの側の印（`/var/lib/sakutei/unit-stamps/`）で行う。Sakutei の中に状態を持たないので、デーモンを起こし直しても、別の道具から見ても同じ。

## 設定

```yaml
# providers.yaml
providers:
  host-file:
    file_hosts: web1
    remote_config: hosts.yaml
  systemd-unit:
    unit_hosts: web1                   # 扱うホスト（remote_config の hosts の名前）。local は手元
    remote_config: hosts.yaml          # 対象の一覧（#130）
    # stamp_dir: /var/lib/sakutei      # 省略可。ホストの上の印の置き場
    # unit_dir: /etc/systemd/system    # 省略可。通常のユニットの元のファイルの置き場
    # quadlet_dir: /etc/containers/systemd   # 省略可。quadlet の元のファイルの置き場
```

ホストに要るのは sshd・systemd（`systemctl`）と、ファイルの読み書きの `sh`・`sha256sum`・`mktemp`・`mv` など（[host-file](host-file.md#設定) と同じ）。quadlet には podman 4.4 以上（generator）。ふつうは `sudo: true`（root）で入る。

## 範囲を intent に書く（`providers:`、Backlog #182）

扱うホスト（`unit_hosts`）は ID の頭で、どのユニットを管理できるかを決める範囲なので、intent のトップレベルの `providers:` に
書ける（§1.7・§7.7）。`remote_config` とホストの上の置き場（`stamp_dir`・`unit_dir`・`quadlet_dir`）はつなぎ方として provider の
設定に残る。

```yaml
# intent/providers.yaml
providers:
  systemd-unit:
    hosts: [web1]                  # unit_hosts と同じ（順は問わない）
```

- intent にだけ書けば、設定に書いたのと同じに動く。設定にだけあれば今までどおり読み、`plan`・`reconcile`・`status`・常駐が
  「is only in the provider config: write it in the intent too (providers.systemd-unit.…)」と警告する（常駐では一度だけ）。
- 両方にあれば設定は安全の網として残り、**食い違えば読み込みで断る**（弱い側に寄せない。常駐は起動で断り、動いているあいだに
  食い違えばその周を飛ばす）。知らないキーも断る。設定から外すのは次のメジャー版。
- どれがつなぎ方でどれが範囲かの一覧は [ranges.md](ranges.md)。

## ID とフィールド

ID は `<ホスト>/<ユニットのファイル名>`。quadlet はファイルの名前で書き、動くユニットは podman の generator と同じ規則で決まる。

| ID の名前 | 元のファイル | 動くユニット |
| --- | --- | --- |
| `app.service`・`backup.timer` など | `unit_dir/<名前>` | 同じ名前 |
| `app.container`・`app.kube` | `quadlet_dir/<名前>` | `app.service` |
| `data.volume`・`net.network`・`web.pod`・`base.image`・`x.build` | `quadlet_dir/<名前>` | `data-volume.service`・`net-network.service`・`web-pod.service`・`base-image.service`・`x-build.service` |

| フィールド | 値 | 観測 |
| --- | --- | --- |
| `name` | 省略可。書くなら ID の名前と同じ | 名前 |
| `content` | 元のファイルの中身（256 KiB まで）。書かなければ元のファイルに触れない | `sha256:<hex>`（hash で比べる） |
| `state` | `active` か `inactive`。書かなければ動かす・止めるはしない | `ActiveState` が active・reloading・activating なら `active`、ほかは `inactive` |
| `enabled` | `true` か `false`（通常のユニットだけ） | `UnitFileState`（enabled → true、disabled → false、ほかはそのまま） |
| `restart_on` | 依って立つファイル（同じホストの絶対パス、`,` 区切り）。`state: active` のときだけ | 印といまの hash が同じファイル |
| `on_change` | `restart`（既定）か `reload`（`reload-or-restart`） | 宣言だけ |
| `disruptive` | `true` で入れ替えと停止を破壊的として予算に数える | 宣言だけ |
| `unit`・`source`・`sub_state` | 書けない（観測だけ） | 動くユニットの名前・元のファイル・`SubState` |

- 元のファイルを書き換えたら `systemctl daemon-reload` を 1 回だけ流す（systemd が読み直していない〈`NeedDaemonReload=yes`〉ときも）。
- **quadlet が作るユニットは enable できない**（生成されたユニットのため）。`enabled` を書くと intent を読む時点で断る。起動時に立ち上げるのは元のファイルの `[Install]`（`WantedBy=multi-user.target` など）に書く。
- `content` を書かないユニット（配布物の `nginx.service` など）は、状態と enabled だけを合わせる。systemd が知らず元のファイルも無いユニットは absent で、`content` が無ければ作れない（Apply が PermanentError）。

## 例：quadlet のコンテナと設定ファイル

```yaml
resources:
  - provider: host-file
    id: web1/etc/app/app.env
    fields:
      content: |
        APP_PORT=8080
      mode: "0640"
  - provider: systemd-unit
    id: web1/app.container
    depends_on: [web1/etc/app/app.env]      # ファイルを書いてからユニットを扱う
    fields:
      content: |
        [Unit]
        Description=app
        [Container]
        Image=docker.io/library/nginx@sha256:<digest>
        EnvironmentFile=/etc/app/app.env
        PublishPort=8080:80
        [Install]
        WantedBy=multi-user.target
      state: active
      restart_on: /etc/app/app.env          # これが変わったら app.service を入れ替える
```

- 1 周目：ファイルを書き、ユニットの元のファイルを書いて daemon-reload し、`app.service` を start して印を書く。
- `app.env` の宣言を変える：その周にファイルが書き換わり、次の周にユニットの `restart_on` が drift になって 1 回だけ restart する。何周回しても、デーモンを起こし直しても、もう入れ替えない。
- ユニットの `content`（イメージの digest など）を変える：daemon-reload 1 回と restart 1 回。
- イメージは tag でなく digest で固定する。版を上げるのは宣言の digest を変えること。

## 入れ替えの決まり（restart_on）

- ユニットを最後に（再）起動したときの元のファイルと `restart_on` のファイルの hash を、ホストの印 `<stamp_dir>/unit-stamps/<名前>` に残す（1 行 1 ファイル、`source <hash>`・`dep <hash> <パス>`）。
- 観測の `restart_on` は「印の hash といまの hash が同じファイル」の一覧。どれかを書き換えると宣言と違って drift になり（level で判定）、Apply がいまの姿を読み直して、まだ違えば入れ替え（`restart`、`on_change: reload` なら `reload-or-restart`）を 1 回流して印を更新する。同じ Op を 2 回適用しても、印が新しいので二度は入れ替えない。
- **印が無いとき（初めて扱うユニット）は入れ替えない**。印を書くだけ。走っていなかったユニットは start するので、どのみちいまのファイルで立ち上がる。印にまだ無いファイルを `restart_on` に足したときも同じ（入れ替えない）。すでに動いているサービスを Sakutei で扱い始めたときに、無用に止めないため。
- 手で書き換えたファイルが同じ周に宣言へ戻ったとき（host-file が書き戻した）は、Apply が読み直すと印と同じなので入れ替えない（サービスは元の中身で動き続けている）。
- 走っているユニットの元のファイルが印と違う（書いたあと入れ替える前に Sakutei が止まった）ときは、`content` の観測に `(changed since the unit was last (re)started)` が付いて drift になり、入れ替える。
- `restart_on` を外すときは `restart_on: ""` と書く（印から外れる）。行ごと消すと、印に残ったファイルでは入れ替え続ける。
- 入れ替えは既定で通常の操作。`disruptive: true` なら、走っているユニットの入れ替えと停止（`state: inactive`）の Op を破壊的（Destructive）にして予算（§13、`--max-destructive-per-cycle` など）に数える。予算を超えれば止まり、入れ替えない（ファイルの書き込みは止まらない）。

## 届かないとき

| 起きたこと | 答え |
| --- | --- |
| ホストに届かない・認証の失敗 | 観測できない（unknown、TransientError）。absent にしない |
| ホスト鍵の不一致 | 観測できない（PermanentError） |
| sudo が通らない | 観測できない（unknown） |
| systemctl が答えない（systemd が動いていない等） | 観測できない（unknown） |
| 元のファイルがシンボリックリンクの先でないディレクトリなど | 観測できない（PermanentError） |

観測できないユニットは何もしない（書かない・入れ替えない）。戻れば次の周に収束する。

## 扱わないもの

- ユーザ単位のユニット（`systemctl --user`）。
- drop-in（`<unit>.d/*.conf`）は host-file で書けるが、入れ替えの印には元のファイルしか入らない。入れ替えたいときは `restart_on` に drop-in のパスを書く。
- quadlet の `ServiceName=` で名前を変えたユニット（動くユニットはファイルの名前から決める）。
- ユニットを消すこと（Delete は断る）。要らなくなったら intent から外し、人が止めて消す。

## 統合テスト

本物の systemd・sshd・podman を相手にした統合テスト（Backlog #155）。build tag `integration_hostconf` の付いたファイルにあり、`make check`・`make test`・CI には入らない（docker の `--privileged` と sudo が要るため）。

```console
$ make integration-hostconf
```

要るもの: docker（パスワードなしの `sudo -n docker`）、`ssh`・`ssh-keygen`。初回は Dockerfile（`tests/integration/hostconf/Dockerfile`）からイメージを作るので apt のミラーに届くこと。

- ベースのイメージは `debian:trixie-slim`（tag と digest で固定）。systemd・sshd・sudo・podman 5（quadlet）を入れ、systemd を PID 1 で動かす（`--privileged --cgroupns=private`、`/run` は tmpfs）。podman の置き場（`/var/lib/containers`）は docker の volume にする（overlay の上に overlay は作れない）。
- コンテナの中では setgid の `unix_chkpwd` が動かず PAM のアカウントの検査が落ちたので、テストのイメージでは sshd を `UsePAM no`、sudo を `!pam_acct_mgmt` にしている。
- quadlet が動かすコンテナのイメージは手元の docker の `alpine:3.24`（tag と digest で固定）を `docker save` → `podman load` で入れる（コンテナの中からイメージを取りに行かない。`Pull=never`・`Network=none`）。
- sakutei は `sakutei` ユーザで ssh で入り、`sudo: true` で root になる。ホスト鍵は docker exec で読んだ公開鍵から known_hosts に書く。
- intent は 4 つ：host-file の `/etc/sakutei-it/echo.conf`、通常のユニット `sakutei-it-echo.service`（`enabled: true`。起動のたびに echo.conf の中身をログに 1 行足す）、host-file の `/etc/sakutei-it/app.env`、quadlet の `sakutei-it-app.container`（起動のたびに app.env をボリュームのファイルに 1 行足す）。ユニットはそれぞれのファイルを `restart_on` と `depends_on` に書く。起動の回数は、この行の数で数える。
- 1 回の `reconcile` は 1 回のプロセスなので、入れ替えの回数が増えないことは「Sakutei を起こし直しても入れ替えない」ことでもある。
- 終われば（落ちても）コンテナ（と volume）と一時ディレクトリを消し、ssh の master を止める。Makefile も最後に残ったコンテナを消す。

| 段 | 内容 |
| --- | --- |
| converge from empty | 4 つが drift → 収束 → 4 つ適合。ファイルは宣言どおり（app.env は root の 0640）、echo は active・enabled、quadlet の app.service は active・generated、podman でコンテナが動く、印がある。どちらも 1 回だけ起動した |
| no change however many reconciles | reconcile を 3 回 → どれも `Converged (no changes)`。起動の行は増えない |
| changing the files restarts each unit once | 2 つのファイルの宣言を変える → 収束までと、さらに 3 回 reconcile → それぞれのユニットがちょうど 1 回だけ入れ替わり、新しい中身で起動した |
| a hand edit comes back without a restart | コンテナの中で echo.conf を書き換え・app.env の mode を変える → status が drift → 収束で戻る。echo は入れ替わらない（同じ周に戻ったので） |
| changing the unit file reloads and restarts once | quadlet の中身（Description）を変える → systemd が読み直した（Description が新しい）、コンテナが 1 回入れ替わった |
| a stopped unit is started | echo を手で止める → drift → 収束で active |
| a quadlet unit cannot be enabled | quadlet に `enabled: true` を書くと reconcile が読み込みで断る（`[Install]` を案内する） |
| sudo refused | sudoers を外す → 4 つとも unobservable（sudo が断る）。reconcile は何も書かない → 戻すと収束し、新しい中身で 1 回入れ替わる |
| sshd stopped | sshd を止める → 4 つとも unobservable（`remote h1: unreachable`）。reconcile は何もしない → 戻すと適合、コンテナは入れ替わっていない |

### 結果

OpenSSH 9.6p1（手元の ssh）、docker 29.1.3、コンテナは Debian trixie の systemd 257・podman 5.4.2 で回した結果（全段 PASS）:

```
--- PASS: TestHostConfIntegration (13.57s)
    --- PASS: TestHostConfIntegration/converge_from_empty (1.76s)
    --- PASS: TestHostConfIntegration/no_change_however_many_reconciles (0.43s)
    --- PASS: TestHostConfIntegration/changing_the_files_restarts_each_unit_once (3.66s)
    --- PASS: TestHostConfIntegration/a_hand_edit_comes_back_without_a_restart (0.67s)
    --- PASS: TestHostConfIntegration/changing_the_unit_file_reloads_and_restarts_once (2.13s)
    --- PASS: TestHostConfIntegration/a_stopped_unit_is_started (0.55s)
    --- PASS: TestHostConfIntegration/a_quadlet_unit_cannot_be_enabled (0.00s)
    --- PASS: TestHostConfIntegration/sudo_refused:_unknown,_nothing_done (0.84s)
    --- PASS: TestHostConfIntegration/sshd_stopped:_unknown,_nothing_done (3.52s)
PASS
```

- 最初は quadlet の `RunInit=true` が `catatonit` の無いイメージで起動に失敗した（provider は Apply の失敗として正しく知らせた）。止めを速くするのは `StopTimeout=1` にした。

## 通常のテスト

`make check` の中：

- `internal/hostconf`：偽のホスト（`remotefake`）と偽の systemd（`internal/hostconf/systemdfake`。元のファイルは本物のファイルで、daemon-reload のときに読み、quadlet のファイルから .service を作る）で、契約テスト（届かないと unknown）、quadlet の 1 回だけの入れ替え・デーモンの起こし直し・手の書き換え・印が無いとき・書いたあと入れ替える前に止まった姿・reload と disruptive・enabled・届かないと unknown・書き方の誤り。
- `tests/acceptance/hostconf_test.go`：デーモンの経路（RunCycle）で `[AC-155-1]`〜`[AC-155-5]`。
