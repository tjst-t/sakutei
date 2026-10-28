# 離れたホストを扱う（ssh 越しの実行の層）

1 台の Sakutei のデーモンから、agent を置かずに離れたホストの FW（nftables の live）と DNS のゾーンファイル（dnszone）を収束させるための層（Backlog #130）。ゲスト OS を観測するだけの [guest-os](providers/guest-os.md)（#86）と、NETCONF の機器の設定を観測するだけの [netconf](providers/netconf.md)（#141。コマンドでなく ssh の netconf サブシステムを開く）も同じ層と対象の一覧を使う。ファイルと systemd のユニット（quadlet を含む）を保つ [host-file](providers/host-file.md)・[systemd-unit](providers/systemd-unit.md)（#155）もこの層の上で動く。CLI でしか設定できない装置を扱う [cli-device](providers/cli-device.md)（#159）は、装置の CLI に 1 行をそのまま流す口（`RunLine`）と pty つきの殻（`OpenShell`）を使う。provider が流す外部コマンドとファイル操作を「手元」か「ssh 越し」で走らせる。決まりの正は設計書 §29.1「離れたホストを扱う口：ssh 越しの実行の層」。ここは使い方の説明で、矛盾したら spec/ が優先する。

- ssh は OpenSSH の `ssh` CLI を呼ぶ（Go の依存は増やさない）。ホストに要るのは sshd と、扱うもののコマンド（nft、ゾーンファイルなら `sh`・`cat`・`flock`・`mktemp`・`mv`・`chmod`・`dirname`）だけ。
- 完全の階級（List・BulkObserve・順序つき diff・原因タグ）はそのまま。prune も手元と同じ決まりで効く。
- 対象を設定しない今の使い方（手元のファイル・手元の nft）は、ID も動きも変わらない。

## 対象の一覧

名前付きのホストの一覧をファイルに書き、`--provider-config` の `remote_config` で渡す。書式は intent と同じ JSON / YAML。

```yaml
# hosts.yaml
secret_store: secrets.json          # file SecretBackend。private_key を使うなら必須
known_hosts: known_hosts            # 既定の known_hosts（ホストごとに上書きできる）
control_dir: /run/sakutei/ssh       # 省略可（接続の使い回しのソケットと一時の鍵。0700）
ssh: /usr/bin/ssh                   # 省略可（既定は PATH の ssh）
hosts:
  fw1:                              # 名前（ID の先頭になる。英小文字・数字・_・-）
    address: 192.0.2.11             # 必須
    port: 22                        # 省略可
    user: sakutei                   # 省略可
    private_key: secretref:file/ssh/sakutei   # 省略可。無ければ ssh-agent
    sudo: true                      # 省略可。コマンドの前に sudo -n -- を付ける
    connect_timeout: 10s            # 省略可（既定 10s）
    timeout: 60s                    # 省略可（コマンド 1 回の上限。既定 60s）
    control_persist: 60s            # 省略可（接続の使い回し。0s で使い回さない）
  fw2:
    address: 192.0.2.12
    user: sakutei
    private_key: secretref:file/ssh/sakutei
    sudo: true
  ns1:
    address: 192.0.2.53
    known_hosts: /etc/sakutei/known_hosts.dns
    trust_on_first_use: false       # 省略可。true で知らないホストの鍵を初回だけ信じる
```

- 相対パス（`secret_store`・`known_hosts`・`control_dir`、`/` を含む `ssh`）は、このファイルのあるディレクトリから。
- 知らないキーはエラー（書き間違いを黙って無視しない）。

provider ごとに、どのホストを扱うかを `--provider-config` で名指しする（guest-os は `guest_hosts`、[providers/guest-os.md](providers/guest-os.md)。netconf は `netconf_hosts`、[providers/netconf.md](providers/netconf.md)。host-file は `file_hosts`、systemd-unit は `unit_hosts`、cli-device は `cli_hosts`、[providers/cli-device.md](providers/cli-device.md)）。

```yaml
# providers.yaml
providers:
  nftables:
    ruleset_target: live
    ruleset_tables: inet/filter
    ruleset_hosts: fw1,fw2          # 扱うホスト
    remote_config: hosts.yaml
  dnszone:
    zone: /var/lib/knot/example.com.zone   # 各ホストの上のパス（絶対パス）
    zone_hosts: ns1
    remote_config: hosts.yaml
    zone_reload: knotc zone-reload example.com.
```

```console
$ sakutei --intent intent/ --provider-config providers.yaml
$ sakutei status --intent intent/ --provider-config providers.yaml
```

## ID

ホストを設定すると、local id の先頭にホストの名前が付く。2 台に同じ table・chain、同じレコードがあってもぶつからない。

| provider | 手元（今まで） | ホストあり |
| --- | --- | --- |
| nftables（live） | `inet/filter/web` | `fw1/inet/filter/web` |
| dnszone | `www.example.com./A` | `ns1/www.example.com./A` |

フィールドは変わらない（ホストは ID にだけ書く）。ホストのない ID・一覧に無いホストの ID は intent を読む時点で断る。

```json
{
  "resources": [
    {"provider": "nftables", "id": "fw1/inet/filter/web",
     "fields": {"family": "inet", "table": "filter", "chain": "web", "rules": "tcp dport 443 accept ; drop"}},
    {"provider": "nftables", "id": "fw2/inet/filter/web",
     "fields": {"family": "inet", "table": "filter", "chain": "web", "rules": "tcp dport 443 accept ; drop"}},
    {"provider": "dnszone", "id": "ns1/www.example.com./A",
     "fields": {"name": "www.example.com.", "type": "A", "value": "192.0.2.10", "ttl": "300"}}
  ]
}
```

## 鍵とホスト鍵

- **秘密鍵**は SecretRef（`secretref:file/<path>`）でしか書けない。生の鍵を書くとエラーになる（エラーの文言にも出さない）。値は ssh を起動する直前に store から読み、0600 の一時ファイル（`control_dir` の中）に出して `-i` で渡し、ssh が終われば消す。鍵を引数・ログ・エラーに出さない（§20）。`private_key` を書かなければ ssh-agent（`SSH_AUTH_SOCK`）を使う。
- **ホスト鍵**は `known_hosts` で確かめる（`StrictHostKeyChecking=yes`）。知らない鍵・違う鍵は拒む。`known_hosts` は必須。初回だけ信じる（`accept-new`）のは `trust_on_first_use: true` を明示したときだけで、そのときも違う鍵は拒む。known_hosts は別の経路（構築のときに記録した公開鍵など）で作っておくのがよい。非標準のポートは `[address]:port` の形で書く。
- 利用者の ssh の設定（`~/.ssh/config`・`/etc/ssh/ssh_known_hosts`）は読まない（`-F /dev/null`）。いつも `BatchMode=yes`（パスワードや確認を尋ねない）、`ConnectTimeout`、`ServerAliveInterval` を付ける。

## 接続の使い回し

ホストごとに 1 本の ssh の master（`ControlMaster`）を立て、コマンドはそこに相乗りする（周ごとに TCP と鍵交換をし直さない）。master は `control_persist` の間使われなければ自分で終わり、死んでいれば次のコマンドで立て直す。ソケットは `control_dir`（既定は `$XDG_RUNTIME_DIR/sakutei-ssh`、無ければ `<一時ディレクトリ>/sakutei-ssh-<uid>`）に置く。このディレクトリは自分の uid の 0700 でなければ使わない。unix ソケットのパスには長さの上限（約 104 バイト）があるので、長すぎるとエラーになる（`control_dir` を短いパスにする）。

例外は、nftables の base chain を変えた後の到達の確かめ（Backlog #173、[providers/nftables.md](providers/nftables.md) の「締め出しの守り」）。いまの master は既存の接続なので FW の新しい規則を通らないことがある。確かめは master に相乗りせず（`ControlPath=none`）、新しい ssh の接続で `true` を走らせる（sudo は付けない。前の失敗で新しい接続を控えることもしない）。

## 権限

- `sudo: true` なら、すべてのコマンド（nft、ゾーンファイルを読み書きする `sh`、再読み込みのコマンド）の前に `sudo -n --` を付ける。`-n` なのでパスワードは尋ねない。sudoers で NOPASSWD を与えておく。
- nft は CAP_NET_ADMIN（ふつうは root）が要る。ゾーンファイルはディレクトリへの書き込み（一時ファイルと rename）と `<zone>.lock` の作成ができること。
- sudo を絞るなら、許すコマンドは `nft`、`sh`（ゾーンファイルの読み書きの小さなスクリプト）と再読み込みのコマンド。`sh` を許すのは root を与えるのとほぼ同じなので、ゾーンファイルの持ち主の利用者で直に入る（`sudo: false`）方が狭くできる。

## 届かないとき

| 起きたこと | provider の答え |
| --- | --- |
| ホストに届かない（接続の拒否・期限切れ・名前を引けない）・sshd が止まっている | 観測できない（unknown、TransientError）。absent にしない |
| 認証の失敗（鍵が受け入れられない） | 観測できない（unknown、TransientError） |
| ホスト鍵の不一致（known_hosts に無い・違う） | 観測できない（unknown）。設定を直すまで治らないので **PermanentError** で知らせる |
| コマンドが走って失敗した（nft の権限が無い等） | 観測できない（unknown） |

- 観測できないものは drift とも適合とも数えず、何もしない（§3）。List は全部のホストを読めたときだけ complete で、1 台でも読めなければ prune を止める。
- BulkObserve は読めないホストの分だけを返さない（そのホストの資源は Read が unknown と答える）。ほかのホストはそのまま収束を続ける。
- 失敗したホストには `connect_timeout` の間は新しくつながず、同じ失敗をすぐ返す（届かないホストの資源ごとに待たない）。

## 再読み込み（dnszone）

ゾーンファイルを書き換えたあと、DNS サーバに読み直させるコマンドを `zone_reload` で設定できる（既定は無し）。手元のゾーンファイル（`zone_hosts` なし）でも使える。

```yaml
zone_reload: knotc zone-reload example.com.
# zone_reload: rndc reload example.com
```

- 空白で区切った語をそのまま引数にする（シェルを通さない）。ホストがあればそのホストで（`sudo: true` なら sudo を付けて）、無ければ手元で走らせる。
- 走るのは Apply が実際にファイルを書き換えたときだけ（変わらない Apply では走らない）。
- 失敗は Apply の失敗として知らせる（ファイルはもう書き換わっている）。次にそのゾーンへ Apply が来たとき（ファイルが変わらなくても）もう一度走らせる。ゾーンが宣言どおりで Apply が来ない間は走らないので、失敗の Event を見たら原因を直して手で読み直させる（§29.1）。
- SOA のシリアルは今までどおり Apply が上げる（`soa_serial: keep` で止められる）。

## 統合テスト

本物の sshd を相手にした統合テスト（Backlog #130）。build tag `integration_remote` の付いたファイルにあり、`make check`・`make test`・CI には入らない（docker と sudo が要るため）。

```console
$ make integration-remote
```

要るもの: docker（パスワードなしの `sudo -n docker`）、`ssh`・`ssh-keygen`。初回は Dockerfile（`tests/integration/remote/Dockerfile`）からイメージを作るので apt のミラーに届くこと。

- ベースのイメージ（`debian:bookworm-slim`）は tag と digest で固定し、sshd・nft・sudo を入れたイメージを作る（Dockerfile のハッシュでイメージに名前を付けるので、作るのは 1 回だけ）。
- コンテナを 2 台（`sakutei-rmit-h1`・`h2`）立てる。nft のために `--cap-add NET_ADMIN`（コンテナ自身の network namespace の中の ruleset だけを触る）。sshd は PID 1 でなく `docker exec -d` で動かす（sshd だけを止め・立て直せるように）。ホスト鍵は起動のたびに作り直し、known_hosts は docker exec で読んだ公開鍵から書く。
- 手元で鍵を作り、秘密鍵は file SecretBackend にだけ置く（鍵のファイルは消す）。sakutei は `sakutei` ユーザで入り、`sudo: true` で root になる。
- sakutei のバイナリを作り、`--provider-config`（nftables：`ruleset_hosts: h1,h2`、dnszone：`zone_hosts: h1,h2` と `zone_reload`）で `status --json`・`reconcile` を回す。intent は 2 台の同じ chain（`h1/inet/sk/web`・`h2/inet/sk/web`）、2 台の同じレコード、h1 だけのレコードの 5 つ。
- 終われば（落ちても）コンテナと一時ディレクトリを消し、ssh の master を止める。Makefile も最後に残ったコンテナを消す。

確かめること:

| 段 | 内容 |
| --- | --- |
| converge both hosts from empty | 空（table もレコードも無い）で 5 つが drift → reconcile → 5 つ適合。2 台の chain が宣言どおり、h1 だけのレコードは h2 に無い。再読み込みのコマンドが両方のホストで走る（引数どおり）。ゾーンファイルは root の 0644 のまま |
| the ssh connection is reused | ホストごとに master のソケットが 1 つあり、生きている。一時の鍵のファイルが残っていない |
| drift by hand on both hosts comes back | コンテナの中で手で変える（h1 のルールを消す・h2 の chain を空にする・h2 の値を変える・h1 のレコードを消す）→ status が 4 つ drift → reconcile → 5 つ適合。再読み込みが走る |
| no drift however many cycles | reconcile と status を 5 周 → drift 0。2 台の ruleset（handle 込み）・ゾーンファイル・再読み込みの回数が変わらない（Apply が走っていない） |
| sshd stopped | h1 と h2 を手で変えてから h2 の sshd を止める → h2 の 2 つは unobservable（`remote h2: unreachable`）、h1 は drift → reconcile で h1 だけ戻り、h2 は何も変わらない → sshd を戻すと 5 つ適合 |
| key removed | h2 の authorized_keys を外して既存の接続を切る → h2 は unobservable（`authentication failed`）、reconcile で h2 は何も変わらない → 鍵を戻すと適合 |
| host key changed | h2 のホスト鍵を作り直す → h2 は unobservable（`permanent: … host key verification failed`）、h1 は適合のまま、reconcile で h2 は何も変わらない → known_hosts を新しい鍵に直すと適合 |

### 結果

OpenSSH 9.6p1（手元の ssh）、コンテナは Debian bookworm の openssh-server・nftables で回した結果（全段 PASS）:

```
--- PASS: TestRemoteIntegration (10.78s)
    --- PASS: TestRemoteIntegration/converge_both_hosts_from_empty (1.66s)
    --- PASS: TestRemoteIntegration/the_ssh_connection_is_reused (0.01s)
    --- PASS: TestRemoteIntegration/drift_by_hand_on_both_hosts_comes_back (1.44s)
    --- PASS: TestRemoteIntegration/no_drift_however_many_cycles (2.04s)
    --- PASS: TestRemoteIntegration/sshd_stopped:_unknown,_nothing_done_there,_the_other_host_converges (1.56s)
    --- PASS: TestRemoteIntegration/key_removed:_unknown,_nothing_done (2.15s)
    --- PASS: TestRemoteIntegration/host_key_changed:_unknown_(permanent),_nothing_done (1.93s)
PASS
```

- 1 周（2 台 × nftables と dnszone の観測、List）は接続の使い回しのおかげで 0.2 秒前後。
- 最初は、長い一時ディレクトリの下に置いた master のソケットが unix ソケットの長さの上限を超えて、すべて unobservable になった。上限を超えるパスは今は分かりやすいエラーにしている（`control_dir` を短くする）。

## 通常のテスト

`make check` は本物の sshd を使わない。

- `internal/remote`：引用（どんな引数も sh・bash・dash・zsh でそのまま 1 語に戻る＝注入させない）、ssh の引数（BatchMode・known_hosts・ConnectTimeout・使い回し・sudo）、秘密鍵の一時ファイル（0600、終われば消える、引数とエラーに出ない）、失敗の分け方を、偽の ssh（引数を記録して最後の引数を sh で走らせるスクリプト）で確かめる。
- `tests/acceptance/remote_test.go`：偽のホスト（`internal/remote/remotefake`。ゾーンファイルのスクリプトは本当に sh・flock で走り、nft は偽の nft）を 2 台立て、nftables（live）と dnszone の契約テスト（観測できないのを absent にしないことを含む）・BulkObserve 等価性テスト、2 台で同じ chain・レコード、再読み込み、1 台が落ちたとき・ホスト鍵の不一致を確かめる。
