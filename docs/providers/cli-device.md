# cli-device provider

管理 API に設定の口が無く、CLI でしか設定できない装置（syslog の宛先・NTP・ソフトの更新など）を、ssh で CLI を打って観測し、
コマンド列で変える汎用の provider（`cli-device`、Backlog #159）。ssh 越しの実行の層（[remote.md](../remote.md)、#130）の上で動く。
**機種ごとの中身**（どのコマンドで観測し、どう読み、どのコマンドで変えるか）は**機種の定義のファイル**で与え、本体は機種を知らない。
決まりの正は設計書 §29.1「CLI でしか設定できない装置を扱う口」。ここは使い方の説明で、矛盾したら spec/ が優先する。

- 標準階級（観測は本当の姿）。List・BulkObserve を持たず、prune しない：管理するのは intent に書いた項目だけ。
- 出力が定義に合わないときは**観測できない（unknown）**で、absent と取り違えない。何も変えない。
- 変更は 1 つの項目のコマンド列。エラーの印が出たらそこで止め、どこまで流れたかを知らせる。流した後は必ず観測し直して確かめる。

## 設定

`--provider-config` で、扱う装置と、つなぎ方（[対象の一覧](../remote.md#対象の一覧)）と、機種の定義を渡す（A3：範囲は intent、
つなぎ方と機種の定義は設定）。

```yaml
# providers.yaml
providers:
  cli-device:
    cli_hosts: sw1,sw2                 # 扱う装置（remote_config の hosts の名前）
    remote_config: hosts.yaml          # 対象の一覧（#130）。装置の ssh のアドレス・利用者・鍵・known_hosts
    cli_definition: example-switch.yaml          # 機種の定義（全部の装置の既定）
    # cli_definitions: sw2=other-model.yaml      # 省略可。装置ごとの上書き（<装置>=<ファイル>,...）
    # secret_store: secrets.json       # 省略可。秘密のフィールドを解決する file SecretBackend（既定は remote_config の secret_store）
```

- 装置の ssh は 1 本の接続に 1 つのセッションしか受けないことが多いので、cli-device は接続を使い回さない（`control_persist` は効かない）。
  `sudo` も付けない（装置の CLI に sudo は無い）。
- 1 つの装置へは同時に 1 つのやりとりだけを開く（観測と設定が交ざらない）。

## 範囲を intent に書く（`providers:`、Backlog #182）

扱う装置（`cli_hosts`）は管理の範囲なので、intent のトップレベルの `providers:` に書ける（§1.7・§7.7）。管理する項目は前から
intent の `resources:`（`<装置>/<項目>`）にしかなく、機種の定義（どう観測しどう変えるか）・`cli_definitions`・`remote_config`・
`secret_store` はつなぎ方として provider の設定に残る。

```yaml
# intent/providers.yaml
providers:
  cli-device:
    hosts: [sw1, sw2]                  # cli_hosts と同じ（順は問わない）
```

- intent にだけ書けば、設定に書いたのと同じに動く。設定にだけあれば今までどおり読み、`plan`・`reconcile`・`status`・常駐が
  「is only in the provider config: write it in the intent too (providers.cli-device.…)」と警告する（常駐では一度だけ）。
- 両方にあれば設定は安全の網として残り、**食い違えば読み込みで断る**（弱い側に寄せない。常駐は起動で断り、動いているあいだに
  食い違えばその周を飛ばす）。知らないキーも断る。設定から外すのは次のメジャー版。
- どれがつなぎ方でどれが範囲かの一覧は [ranges.md](ranges.md)。

## 機種の定義

使う側が持つファイル（JSON か、intent と同じ YAML のサブセット。`{ }`・`[ ]` のフロースタイルは書けない）。未知のキーは断る。

```yaml
# example-switch.yaml — 架空の装置（どの製品の CLI でもない）
name: example-switch
session:
  mode: interactive                 # interactive（既定。pty を取ってプロンプトを待つ）| exec（コマンドごとに ssh の exec）
  prompt: '^[a-z][a-z0-9-]*(\(config\))?# $'   # 最後の行に当てる。interactive で必須
  pager: '^--More--$'               # 省略可。最後の行に当たればページ送りのキーを打ち、印は出力から消す
  pager_key: " "                    # 省略可（既定 " "）
  newline: lf                       # 省略可（lf | cr | crlf）
  setup:                            # 省略可。つないだ直後に打つ（ページ送りを止めるなど）
    - terminal length 0
  logout:                           # 省略可。切る前に打つ
    - exit
  timeout: 30s                      # 省略可。コマンド 1 つの上限
errors:                             # 必須。出力のこの行が出たら失敗
  - '^% '
config:                             # 省略可（interactive だけ）
  enter:                            # 設定の入り方
    - configure
  leave:                            # 出方
    - end
  # commit:                         # 確定（装置が持つなら）
  #   - commit
  # rollback:                       # 取り消し（commit と組で）
  #   - abort
items:
  syslog:                           # 項目の名前。ID は <装置>/syslog
    fields:                         # 値の形（必須）。合わない値は読み込みで断る
      host:
        pattern: '[0-9]{1,3}(\.[0-9]{1,3}){3}'
      port:
        pattern: '[0-9]{1,5}'
    observe:
      command: show running-config
      regex: '^logging host (?P<host>\S+) port (?P<port>[0-9]+)$'   # 名前付きの組 = フィールド
      absent: '^no logging host$'   # 省略可。当たれば「設定が無い」
    set:                            # 変えるコマンドの雛形。{名前} に宣言の値が入る
      - logging host {host} port {port}
    unset:                          # 省略可。ensure: absent で消すとき（observe.absent が要る）
      - no logging host
  ntp:
    fields:
      server:
        pattern: '[0-9]{1,3}(\.[0-9]{1,3}){3}'
    observe:
      command: show ntp
      table:                        # 表で読む
        header: '^Index +Server +Status$'
        row: '^1 '                  # 行を選ぶ（ちょうど 1 行）
        columns:
          server: 1                 # 0 から数えた列
    set:
      - ntp server {server}
    disruptive: true                # Apply を破壊的として予算（§13）に数える
  hostname:
    fields:
      name:
        pattern: '[a-z][a-z0-9-]{0,30}'
    observe:
      command: show system
      key_value:                    # "キー: 値" の行で読む
        separator: ":"
        keys:
          name: Hostname
    set:
      - hostname {name}
  community:
    fields:
      community:
        pattern: '[A-Za-z0-9]{8,32}'
        secret: true                # 宣言は SecretRef。値はログ・エラー・plan・観測に出さない
    observe:
      command: show running-config
      regex: '^snmp community (?P<community>\S+)$'
      absent: '^no snmp community$'
    set:
      - snmp community {community}
  version:                          # set の無い項目は観測だけ（変えるのは action か人の手順）
    fields:
      version:
        pattern: '[0-9]+\.[0-9]+\.[0-9]+'
    observe:
      command: show system
      key_value:
        separator: ":"
        keys:
          version: Version
actions:                            # 省略可。Workflow の action（一度きりの操作）
  upgrade:
    args:
      image:
        pattern: '[0-9]+\.[0-9]+\.[0-9]+\.bin'
    commands:
      - upgrade image {image}
    destructive: true               # 予算に数える
    # config: true                  # true なら config の enter / commit / leave で囲む
```

### 読み方

正規表現はどれも複数行（`(?m)`）で当てる。読み方は項目ごとにちょうど 1 つで、読み方が出すフィールドと `fields` は一致しなければならない。

| 読み方 | 書き方 | 当たり |
| --- | --- | --- |
| `regex` | 名前付きの組（`(?P<名前>…)`）がフィールド | 当たりはちょうど 1 つ（2 つ以上は観測できない） |
| `key_value` | `separator` と、フィールド → キーの `keys` | 書いたキーはどれもちょうど 1 回。一部だけ欠けるのは観測できない |
| `table` | `header`（見出しの行）・`row`（行を選ぶ）・`columns`（フィールド → 列）・`split`（列の区切り、既定 空白） | 見出しの後ろ、空の行までの中で `row` に当たるのはちょうど 1 行 |

出力に `absent` が当たれば「設定が無い」（absent）。読み方も `absent` も当たらない、両方が当たる、エラーの印の行がある、
exec で 0 以外で終わる、プロンプトが `timeout` までに来ない——どれも**観測できない（unknown）**で、何も変えない。

対話（interactive）の出力は端末として読む：`\r\n` は改行、行の途中の `\r` は行の頭に戻る、`\b` は 1 文字消す、ESC の制御列は捨てる。
打ったコマンドのこだま（pty の echo）は出力の先頭から外す。

### 値の形（差し込みを防ぐ）

- `fields`・`args` の `pattern` は必須で、値の**全体**に当てる（`^(?:…)$` で囲む）。値は改行・制御文字を含められない。
- intent の値は**読み込みの時点**で形を確かめ、合わなければ周を回さない（値そのものはエラーに出さない）。秘密の値（SecretRef）は
  Apply の直前に解決したときに確かめ、合わなければ流さない。
- Diff は変わるフィールドだけを渡すので、雛形の変わらないフィールドは Apply の前に装置から読んだ値で埋める。この値も同じ形で確かめ、
  合わなければ観測できない（unknown）として何も流さない（読み方が `(?P<x>.+)` のように広くても、装置の上の値がコマンドに入り込まない）。
- コマンドの雛形の `{名前}` は、その項目のフィールド（action なら引数）だけ。知らない名前は定義の読み込みで断る。
- `set` を書くなら、どのフィールドもどれかの `set` に入っていなければならない（変えられないフィールドを宣言させない）。

## ID とフィールド

ID は `<装置>/<項目>`（例 `sw1/syslog`）。装置は `cli_hosts` にあるもの、項目は定義の `items` にあるもの。

```yaml
resources:
  - provider: cli-device
    id: sw1/syslog
    fields:
      host: 192.0.2.53
      port: "514"
  - provider: cli-device
    id: sw1/community
    fields:
      community: secretref:file/snmp/community
```

- 項目は全体で宣言する（その項目のどのフィールドも書く）。`item` は観測にいつも載る名前（identity。書くなら ID の項目と同じ）。
- 秘密のフィールドは SecretRef でしか書けない。観測は「このプロセスが書いた値と同じなら、宣言に書かれたとおりの参照」だけを返す
  （値は覚えず、プロセスごとの鍵の HMAC だけを覚える）。デーモンを起こし直した後の最初の周は差分に見えるが、Apply は装置の値が
  宣言の値と同じなら書かずに覚えるだけ。

## 変え方と失敗の扱い

1 回の Apply は 1 つの項目：

1. 装置に 1 つのやりとりを開き（`setup` を打ち）、まず観測する。出力が定義に合わなければ**何も流さない**。すでに宣言どおりなら流さない。
2. （`config` があれば）`enter` → `set`（または `unset`）のコマンドを順に → `commit` → `leave`。
3. どれかでエラーの印が出たら（exec なら 0 以外の終了も）**そこで止める**。
   - 定義に `rollback` があれば取り消しを流し、**確定しない**。
   - 無ければ流れたところは装置に残る。観測し直した現実を正として次の周で直す（level）。
   - エラー（Event・通知の口）に、何番目のコマンドが失敗したか・それより前に流れたコマンド・印の行・取り消したかを書く。
4. 流した後は必ず同じやりとりで観測し直し、宣言どおりでなければ失敗にする。
5. 失敗は TransientError（収束ループが backoff で回し直す）。同じ失敗が 3 回続けば PermanentError（stuck）。

秘密の値は、コマンド・エラー・装置の出力の行のどこに現れても `***` に伏せる。

## 一度きりの操作（Workflow の action）

定義の `actions` は、Workflow の `action` 手順（#139）から同じ口で流せる。ソフトの更新のように「収束の宣言にできない操作」を、
承認ゲートつきの Workflow で 1 台ずつ流すのに使う。

```yaml
workflows:
  - workflow: upgrade
    target:
      provider: cli-device
    stages:
      - name: canary
        targets:
          - sw1/version
        gate:
          manual_approval: required
          timeout: 24h
      - name: rest
        targets:
          - sw2/version
    steps:
      - action:
          op: upgrade
          args:
            image: 1.2.0.bin
      - wait:
          fields:
            version: 1.2.0
          timeout: 1h
```

- 引数は定義の `args` の形で確かめる（合わなければ halt）。秘密の引数は SecretRef。
- 装置につなげない（届かない・認証の失敗）ときは「送っていない」：Workflow は待って送り直す。コマンドを 1 つでも打った後の失敗は送り直さない（halt）。
- `destructive: true` の操作は予算に数える。
- 更新の口が CLI にも無い装置は、人の手順で合わせる Task（`update_policy.strategy: manual`、#150）で、観測だけの項目（`set` の無い項目）を宣言する。

## 試す

`internal/clidevice/clifake` は架空の装置の CLI（プロンプト・ページ送り・設定の入り方・確定と取り消し）で、
`internal/clidevice/testdata/example-device.yaml` がその定義（上の例と同じ）。受け入れテストは
`tests/acceptance/clidevice_test.go`（[AC-159-1]〜[AC-159-8]）。

## 限界

- 項目は定義に名前で書いたものだけ（インタフェースごとの項目のような、名前に値を含む項目は書けない）。
- 値は 1 つの文字列（リストは書けない）。
- 観測の出力は項目ごとに取り直す（同じコマンドで読む項目が多いと、そのぶん打つ）。
