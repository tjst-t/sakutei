# Ansible provider（ansible）

雑多な機器・OS の設定を、Ansible の**モジュール**だけを借りて広く覆う provider。設計 §28.7 の 2 層 provider 戦略のロングテール層で、最小の階級（§5.6）。決まりの正は設計書 §29.1「Ansible provider」（Backlog #127）。ここは使い方の説明で、矛盾したら spec/ が優先する。

| provider 名 | ID | 扱うもの |
| --- | --- | --- |
| `ansible` | 利用者が付ける名前（例 `motd-web1`。空白を含まない） | 1 つのホストに対する 1 つのモジュールの呼び出し |

- 借りるのはモジュール（冪等な実行体）だけ。playbook と一回 push の実行モデルは借りない。**いつ・どの順で流すかは収束ループ**が決め、どう変えるかはモジュールが行う。
- `ansible`（ad-hoc）を os/exec で呼ぶ。シェルは通さない。Go の依存は増えない。
- ansible-core 2.21.5 で確かめた（統合テスト、下）。ansible-core 2.19 より前は確かめていない。

## 保証が落ちること（正直に）

最小の階級なので、コア provider（dnszone・nftables・s3-bucket など）が持つ保証の多くが**効かない**。§28.7 が予告していた縮退で、設計の嘘ではなく制約である。

| 保証 | ansible provider では | なぜ |
| --- | --- | --- |
| フィールドごとの差分 | **無い**。plan は「モジュールの check mode が変わると言った」ことと、ansible 自身の `--diff` の差分だけを出す。フィールドの From は空 | check mode は「与えた引数に対して changed か」しか答えず、現在の状態（State）を返さない（`TrueObserve: false`） |
| 原因タグ A / B の区別 | **粗い**。A は intent の版が変わった周だけで、ほかはすべて B。実の差を見て「意図の変更」と「手の変更」を分けられない | 比べる State が無い |
| converging の検知 | **無い**。相手が収束作業中かどうかは分からない | モジュールの結果に「収束中」は無い |
| 破壊的変更の重み付け | **コアは知れない**。予算（§13）に数えるかは利用者が `destructive: true` で宣言する（既定 false） | どの適用がデータを失うかは差分からは分からない。§5.3 の「分からなければ破壊的側に倒す」をそのまま当てると、行の追加まで予算で止まって使えないため、宣言に委ねた（§29.1） |
| prune（宣言から消えたものを消す） | **効かない**。List を持たないので `deletion_policy: PruneWithinScope` は読み込みの時点で断る。Delete もしない | 「どの呼び出しが現実にあるか」を列挙できない。消すのはモジュールの引数（`state: absent` など）で宣言する |
| 観測の安さ | **高い**。1 回の観測が ansible の起動（Python・接続・モジュールの転送）で、local 接続でも 1 秒前後 | 下の「間引き」を使う |

保たれるもの：unknown ≠ absent（届かない・失敗・check mode を持たないモジュールは観測できないとして何もしない）、観測 state レス（毎周 check mode で聞き直す）、予算・承認（`destructive: true` の適用）、secret の扱い（SecretRef、§20）。

## 設定（--ansible-config）

設定は設定ファイルで渡す。`--ansible-config <file>`、または `--provider-config` の `ansible` の `ansible_config` キー（§28.9）。フラグは常駐の `sakutei`・`plan`・`reconcile`・`status` で効く（List を持たないので `import`・`sync` には無い）。書式は intent と同じ JSON / YAML。

```yaml
# ansible.yaml
inventory: inventory.ini              # 必須。ansible の -i（相対パスは設定ファイルから）
ansible: /opt/ansible/bin/ansible     # 省略可（既定 "ansible"、PATH から）。/ を含む相対パスは設定ファイルから
secret_store: secrets.json            # 省略可。args に SecretRef を書くなら必須（file SecretBackend、§20.5）
env:                                   # 省略可。ansible に足す・上書きする環境変数
  ANSIBLE_CONFIG: ansible.cfg
  TMPDIR: /tmp
timeout: 5m                           # 省略可。ansible 1 回の上限（既定 5m。超えたら ssh ごと止める）
max_parallel: 4                       # 省略可。同時に走らせる ansible の数の上限（既定 4）
```

```console
$ sakutei status  --intent intent/ --ansible-config ansible.yaml
$ sakutei reconcile --intent intent/ --ansible-config ansible.yaml
```

- 未知のキーはエラー。`env` で `ANSIBLE_STDOUT_CALLBACK`・`ANSIBLE_LOAD_CALLBACK_PLUGINS`・`ANSIBLE_CALLBACK_PLUGINS`・`ANSIBLE_NOCOLOR` は変えられない（provider が結果を読むのに使う。下）。
- ansible-core 2.19 以降は起動時に `TMPDIR` の下に unix socket を作る。`TMPDIR` が長い（100 バイト前後を超える）と「Local RPC server did not start」で起動しない。そのときは `env` で短い `TMPDIR` を渡す。ssh の ControlPath も同じ理由で `ANSIBLE_SSH_CONTROL_PATH_DIR` を短くする。

## intent の例

```json
{
  "resources": [
    {
      "provider": "ansible",
      "id": "motd-web1",
      "fields": {
        "host": "web1",
        "module": "ansible.builtin.lineinfile",
        "args": "{\"path\": \"/etc/motd\", \"line\": \"managed by sakutei\", \"create\": true}"
      },
      "observe_tier": "ansible"
    },
    {
      "provider": "ansible",
      "id": "app-token-web1",
      "fields": {
        "host": "web1",
        "module": "ansible.builtin.copy",
        "args": "{\"dest\": \"/etc/app/token\", \"mode\": \"0600\", \"content\": {\"$secret\": \"app/token\", \"backend\": \"file\"}}"
      }
    },
    {
      "provider": "ansible",
      "id": "wipe-cache-web1",
      "fields": {
        "host": "web1",
        "module": "ansible.builtin.copy",
        "args": "{\"dest\": \"/var/cache/app/state\", \"content\": \"reset\\n\"}",
        "destructive": "true"
      }
    }
  ],
  "observe_tiers": [{"name": "ansible", "observe_every": 5}]
}
```

### フィールド

| フィールド | 値 | 決まり |
| --- | --- | --- |
| `host` | inventory のホスト名（必須） | 1 台だけ。パターンの記号（`* ? , : ! & [ ] ~` など）・`all`・`ungrouped` は断る。適用の前に `ansible <host> --list-hosts` でちょうどそのホスト 1 台を指すことを確かめる（グループ名なら全員に流れてしまうので断る） |
| `module` | モジュールの FQCN（必須。例 `ansible.builtin.lineinfile`） | 短い名前（`lineinfile`）は断る（コレクションで別のものに当たりうる） |
| `args` | モジュールの引数。**JSON のオブジェクトを文字列で**書く（省略可） | 引数の名前は英数字と `_`。値は入れ子も可。**値はテンプレートとして評価しない**（`{{ … }}` はそのまま渡る） |
| `destructive` | `true` / `false`（既定 false） | true ならこの呼び出しの適用を破壊的として予算（§13）に数える |

ほかのフィールドを書くとエラー（書き間違いを黙って無視しない）。

### secret（SecretRef）

`args` の値のどこにでも SecretRef を書ける（値まるごと）：`"secretref:file/<path>"` か `{"$secret": "<path>", "backend": "file"}`。

- 値は**実行の直前にだけ** `secret_store`（file SecretBackend）から解決し、ansible に渡す。intent・plan・ログに出るのは参照の形だけ。
- 引数は**コマンドラインに載せない**（`ps` に見えない）。0600 の一時ファイルの extra vars（`-e @file`）で渡し、走り終われば消す。
- 解決した平文が ansible の知らせ（失敗の文・stderr）に混じれば `***` に伏せる。SecretRef を使う呼び出しでは `--diff` の差分を知らせに添えない（差分は中身そのものを含みうる）。
- 読み込みの時点では store にその path があることだけを確かめる（値は読まない）。

## 動き方

- **観測**：同じ呼び出しを `--check --diff` で流す。`changed: false` なら適合、`changed: true` なら drift（plan の理由と DriftEvent に ansible の差分を添える）。何も変えない。
- **観測できない**（Unknown。作りも変えもしない）：
  - 届かない（`unreachable`）
  - check mode を持たないモジュール（`skipped`。例 `ansible.builtin.command`。「does not run in check mode」と知らせる）
  - モジュールの失敗、ansible が走らない・時間切れ、ホストが inventory に無い
  - 常駐と `status` では観測できないとして数え、理由を出す。一回性の `reconcile` は skip して、観測できないものがあったとして終了コード 1。一回性の `plan` はほかの provider の観測エラーと同じく止まる。
- **適用**：同じ呼び出しを通常の実行で流す（冪等なのはモジュール）。失敗はモジュールの知らせを添えて返す。届かない・ふつうの失敗は transient（backoff で再試行）、ansible が引数やモジュールを断ったもの（`Unsupported parameters`・`missing required arguments`・`Cannot resolve` など）と check mode でない実行での skipped は permanent（宣言を直すまで再試行しない）。
- **Delete しない**：明示の absent や prune から Delete が来ることは無い（List が無い）。来ても permanent error。
- **同時に走らせる数**：`max_parallel` で上限を決める（観測と適用の合計）。

## 間引き（observe_tiers、§16.5）

1 回の観測が ansible の起動なので、毎周すべてを観測すると周期が長くなる。安定しているものは `observe_tiers`（§16.5）で間引ける（常駐の sakutei で効く。一回性の `reconcile`・`status` は 1 周なので毎回観測する）。既定は毎周で、provider からは決めない：

```yaml
observe_tiers:
  - name: ansible
    observe_every: 5      # 収束しているあいだ 5 周に 1 回だけ check mode で聞く
resources:
  - provider: ansible
    id: motd-web1
    observe_tier: ansible
    fields: {host: web1, module: ansible.builtin.lineinfile, args: '{"path": "/etc/motd", "line": "managed by sakutei"}'}
```

間引いているあいだの drift の検知は数周遅れる（§16.5 が許容する遅れ）。drift が見えた・観測できなかったリソースは次の周も観測する（§16.5 の規則）。

## 結果の読み方（ansible-core で確かめた方法）

ansible-core は JSON の stdout callback を持たない（`json` は ansible.posix のもの）。provider は**自前の小さな stdout callback**（`internal/ansible/callback/sakutei_json.py`、Go のバイナリに埋め込み）を走るたびに一時ディレクトリへ書き、`ANSIBLE_CALLBACK_PLUGINS`（その場所）・`ANSIBLE_STDOUT_CALLBACK=sakutei_json`・`ANSIBLE_LOAD_CALLBACK_PLUGINS=1`（ad-hoc に stdout callback を読ませる）で読ませる。callback はホストの結果ごとに `SAKUTEI-RESULT {"host", "status", "changed", "msg", "diff"}` の 1 行を出し、provider はその行だけを読む（ほかの callback や警告の出力と混ざらない）。

- 引数は `-e @<一時ファイル>` の extra vars（YAML。文字列はすべて `!unsafe`）で渡し、`-a` は `{"path": "{{ __sakutei_args['path'] }}", …}` のようにその変数を指すだけ。値の型（真偽・数・入れ子）はそのまま渡り、値の中の `{{` は評価されない。
- 結果がちょうど 1 つで、それが `host` 自身のものでなければエラー（ホストが inventory に無い・複数に当たった）。
- 終了コード（0・2・4 など）ではなく結果の行で判断する。

## 統合テスト（`make integration-ansible`）

本物の ansible-core を相手にした統合テスト（Backlog #127）。build tag `integration_ansible` の付いたファイル（`tests/integration/ansible/`）にあり、`make check`・`make test`・CI には入らない。

```console
$ make integration-ansible
```

要るもの: python3（venv モジュール。ensurepip は要らない）と、初回は PyPI に届くこと。

- ansible-core を**版で固定した使い捨ての venv**（`ANSIBLE_IT_VENV`、既定 `~/.cache/sakutei/ansible-it-core-2.21.5`）に入れる。初回だけ作る。pip は wheel を sha256 で確かめて使い（venv に pip を入れない）、`tests/integration/ansible/requirements.txt` の版（ansible-core 2.21.5 と依存をすべて `==` で固定、2026-10-27 に PyPI で確かめた版）を wheel だけで入れる。テストは `ansible --version` が 2.21.5 であることを確かめる（手持ちの ansible は `SAKUTEI_IT_ANSIBLE=<path>`、版が違えば `SAKUTEI_IT_ANSIBLE_ANY=1`）。
- 対象は **local 接続**の `target`（一時ディレクトリの中のファイルだけを変える）と、届かない `gone`（127.0.0.1 の閉じたポート 1 への ssh）。sakutei のバイナリを作り、`status --json`・`plan`・`reconcile` を回す。手での変更は sakutei の外からファイルを書き換える。
- ansible の `TMPDIR`・`ANSIBLE_SSH_CONTROL_PATH_DIR`・`ANSIBLE_LOCAL_TEMP` は `/tmp` の下の短い一時ディレクトリ（上の unix socket の長さの制限のため）。
- 終われば（落ちても）一時ディレクトリをすべて消す（venv はキャッシュとして残る）。

確かめること:

| 段 | 内容 |
| --- | --- |
| create from empty | lineinfile 2 つ（片方は値に `{{ not_a_var }}` を含む）・copy・SecretRef の中身の copy の 4 つ。status が drift 4 → plan に check mode の差分（`+managed by sakutei`）、status・plan は何も変えない → reconcile → 適合 4。`{{ … }}` はそのまま書かれ、secret の値は出力のどこにも出ない |
| changed by hand | copy の中身と lineinfile の行を手で変える → status で変えた 2 つだけが drift → reconcile で戻る |
| no drift however many cycles | reconcile と status を 5 周 → 計画が出ない（UPDATE・DriftEvent が無い）、ファイルの mtime が変わらない |
| unreachable / no check mode | 届かないホストへの lineinfile と、check mode を持たない `ansible.builtin.command` を足す → status で観測できない 2（理由に unreachable と check mode）、reconcile は終了コード 1 で、どちらも計画せず、command は走らない |
| destructive is budgeted | 足すだけの lineinfile と、`destructive: true` の copy 2 つ。`--max-destructive-per-cycle 1 --on-exceed halt-destructive-only` → 足すものだけ適用され、破壊的な 2 つは予算で止まる → `--approve-large-change 2` で両方適用 |
| clean up | `state: absent` の宣言で行を消す（provider は Delete をしない）→ 適合 |

### 結果

ansible-core 2.21.5（Python 3.12.3）で回した結果（全段 PASS、2026-10-27）:

```
integration-ansible: ansible-core 2.21.5 (~/.cache/sakutei/ansible-it-core-2.21.5/bin/ansible)
--- PASS: TestAnsibleIntegration (112.07s)
    --- PASS: TestAnsibleIntegration/create_from_empty (21.13s)
    --- PASS: TestAnsibleIntegration/changed_by_hand_is_drift_and_comes_back (14.94s)
    --- PASS: TestAnsibleIntegration/no_drift_however_many_cycles (38.24s)
    --- PASS: TestAnsibleIntegration/an_unreachable_host_and_a_module_without_check_mode_are_unobservable_and_nothing_is_done (10.14s)
    --- PASS: TestAnsibleIntegration/destructive_applies_are_stopped_by_the_budget (24.77s)
    --- PASS: TestAnsibleIntegration/clean_up (2.86s)
PASS
```

見つけたこと:

- 1 回の観測・適用は local 接続でも 1 秒前後（4 リソースの status 1 回でおよそ 4 秒）。観測の重さは数で効くので、台数が多ければ間引き（observe_tiers）を使う。
- `ansible.builtin.copy` は空の `content` を「src (or content) is required」で断る。provider はこれをモジュールの失敗（観測できない）として扱い、適用しない（最初の版のテストはこれで落ちた）。
- 届かないホスト（ssh の接続拒否）は結果の status が `unreachable`（終了コード 4）、check mode を持たない `command` は `skipped`（「Command would have run if not in check mode」）、inventory に無いホストは結果が無く（警告だけで終了コード 0）、どれも観測できないになる。
- ansible-core 2.21.5 には JSON の stdout callback が無い（`ANSIBLE_STDOUT_CALLBACK=json` は「Could not load 'json' callback plugin」）。自前の callback を持つことにしたのはこのため。

## テスト（通常）

- 偽の ansible：`internal/ansible/ansiblefake`（Runner の seam。lineinfile・copy をメモリのファイルで真似る、届かない・check mode を持たない・グループ）で、provider の単体テストと SDK の契約テスト（`sdk.ContractTester` の check mode の契約：desired 無しの Read は Ok にならない／適用前は changed／適用後と 2 回目の適用後は changed でない／届かないなら Ok にならない）。
- exec の経路：`internal/ansible/runner_test.go` はテストのバイナリ自身を偽の `ansible` として走らせ、引数（値がコマンドラインに無いこと・`-e @file` が 0600・`!unsafe`）・環境変数・結果の行の読み方・時間切れを確かめる。
- CLI：`tests/acceptance/ansible_test.go`（AC-127-1〜5）は偽の ad-hoc の ansible の実行ファイル（`tests/acceptance/testdata/fakeansible`）を `--ansible-config` で渡し、status・plan・reconcile・予算・PruneWithinScope の拒否を確かめる。
